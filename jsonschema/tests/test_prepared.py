"""
Tests for `jsonschema.prepare` and `PreparedValidator`.
"""

from urllib.parse import urljoin
import re

from referencing import Registry
import referencing.jsonschema
import pytest

from jsonschema import (
    Draft202012Validator,
    PatternLocation,
    PreparationError,
    validators,
)
import jsonschema

SCHEMA = {
    "$id": "https://example.com/root.json",
    "type": "object",
    "properties": {
        "name": {"pattern": "^[a-z]+$"},
        "nickname": {"pattern": "^[a-z]+$"},
    },
    "patternProperties": {"^x-": {"type": "integer"}},
}


class TestPrepare:
    def test_compiles_patterns_once_for_repeated_content(self):
        prepared = jsonschema.prepare(SCHEMA)
        assert set(prepared.patterns) == {"^[a-z]+$", "^x-"}
        assert all(
            isinstance(each, re.Pattern)
            for each in prepared.patterns.values()
        )

    def test_records_each_location(self):
        prepared = jsonschema.prepare(SCHEMA)
        assert prepared.locations["^[a-z]+$"] == (
            PatternLocation(
                uri="https://example.com/root.json",
                pointer="/properties/name/pattern",
            ),
            PatternLocation(
                uri="https://example.com/root.json",
                pointer="/properties/nickname/pattern",
            ),
        )
        assert prepared.locations["^x-"] == (
            PatternLocation(
                uri="https://example.com/root.json",
                pointer="/patternProperties/^x-",
            ),
        )

    def test_root_without_id_uses_empty_uri(self):
        prepared = jsonschema.prepare({"pattern": "foo"})
        assert prepared.locations["foo"] == (
            PatternLocation(uri="", pointer="/pattern"),
        )

    def test_validates_like_the_unprepared_validator(self):
        prepared = jsonschema.prepare(SCHEMA)
        unprepared = Draft202012Validator(SCHEMA)
        instances = [
            {"name": "abc", "x-1": 3},
            {"name": "ABC"},
            {"x-1": "nope"},
            {"name": "abc", "nickname": "ABC", "x-1": "nope"},
            "not an object",
            12,
        ]
        for instance in instances:
            assert prepared.is_valid(instance) == unprepared.is_valid(instance)
            assert [
                each.message for each in prepared.iter_errors(instance)
            ] == [
                each.message for each in unprepared.iter_errors(instance)
            ]

    def test_validate_raises(self):
        prepared = jsonschema.prepare(SCHEMA)
        with pytest.raises(jsonschema.ValidationError):
            prepared.validate({"name": "ABC"})
        prepared.validate({"name": "abc"})

    def test_invalid_pattern_fails_at_preparation_time(self):
        schema = {
            "$id": "https://example.com/bad.json",
            "properties": {"foo": {"pattern": "["}},
        }
        with pytest.raises(PreparationError) as excinfo:
            jsonschema.prepare(schema)
        error = excinfo.value
        assert error.pattern == "["
        assert error.uri == "https://example.com/bad.json"
        assert error.pointer == "/properties/foo/pattern"
        assert "https://example.com/bad.json#/properties/foo/pattern" in str(
            error,
        )

    def test_invalid_pattern_property_key_fails_at_preparation_time(self):
        with pytest.raises(PreparationError) as excinfo:
            jsonschema.prepare({"patternProperties": {"(": {}}})
        assert excinfo.value.pointer == "/patternProperties/("

    def test_plain_strings_are_not_treated_as_patterns(self):
        schema = {
            "const": "[",
            "enum": ["(", "["],
            "default": "[",
            "examples": ["("],
            "title": "[",
            "properties": {"const": {"const": "pattern"}},
        }
        prepared = jsonschema.prepare(schema)
        assert list(prepared.patterns) == []
        assert prepared.is_valid("[")

    def test_boolean_schemas(self):
        assert jsonschema.prepare(True).is_valid("anything")
        assert not jsonschema.prepare(False).is_valid("anything")

    def test_non_string_pattern_values_are_ignored(self):
        prepared = jsonschema.prepare({"pattern": 12})
        assert list(prepared.patterns) == []

    def test_schema_is_frozen_at_preparation_time(self):
        schema = {"pattern": "^[a-z]+$"}
        prepared = jsonschema.prepare(schema)
        schema["pattern"] = "^[0-9]+$"
        schema["additionalProperties"] = False
        assert prepared.is_valid("abc")
        assert not prepared.is_valid("123")
        assert list(prepared.patterns) == ["^[a-z]+$"]

    def test_mutating_the_original_does_not_change_error_messages(self):
        schema = {"pattern": "^[a-z]+$"}
        prepared = jsonschema.prepare(schema)
        schema["pattern"] = "changed"
        (error,) = prepared.iter_errors("ABC")
        assert error.message == "'ABC' does not match '^[a-z]+$'"


class TestReferencedResources:
    def test_patterns_in_referenced_resources_share_compilation(self):
        referenced = {
            "$id": "https://example.com/referenced.json",
            "properties": {"foo": {"pattern": "^[a-z]+$"}},
        }
        schema = {
            "$id": "https://example.com/root.json",
            "properties": {
                "bar": {"pattern": "^[a-z]+$"},
                "ref": {"$ref": "referenced.json"},
            },
        }
        registry = Registry().with_resource(
            "https://example.com/referenced.json",
            referencing.jsonschema.DRAFT202012.create_resource(referenced),
        )
        prepared = jsonschema.prepare(schema, registry=registry)
        assert list(prepared.patterns) == ["^[a-z]+$"]
        assert set(prepared.locations["^[a-z]+$"]) == {
            PatternLocation(
                uri="https://example.com/root.json",
                pointer="/properties/bar/pattern",
            ),
            PatternLocation(
                uri="https://example.com/referenced.json",
                pointer="/properties/foo/pattern",
            ),
        }
        assert prepared.is_valid({"ref": {"foo": "abc"}})
        assert not prepared.is_valid({"ref": {"foo": "ABC"}})

    def test_unresolvable_references_are_left_to_validation_time(self):
        schema = {"$ref": "https://example.com/nowhere.json"}
        prepared = jsonschema.prepare(schema)
        assert list(prepared.patterns) == []

    def test_embedded_resources_get_their_own_locations(self):
        schema = {
            "$id": "https://example.com/root.json",
            "$defs": {
                "embedded": {
                    "$id": "embedded.json",
                    "pattern": "^embedded$",
                },
            },
            "$ref": "embedded.json",
        }
        prepared = jsonschema.prepare(schema)
        assert prepared.locations["^embedded$"] == (
            PatternLocation(
                uri=urljoin("https://example.com/root.json", "embedded.json"),
                pointer="/pattern",
            ),
        )
        assert prepared.is_valid("embedded")
        assert not prepared.is_valid("nope")

    def test_recursive_references_terminate(self):
        schema = {
            "$id": "https://example.com/root.json",
            "properties": {
                "child": {"$ref": "#"},
                "name": {"pattern": "^[a-z]+$"},
            },
        }
        prepared = jsonschema.prepare(schema)
        assert list(prepared.patterns) == ["^[a-z]+$"]
        assert prepared.is_valid({"child": {"name": "abc"}})
        assert not prepared.is_valid({"child": {"name": "ABC"}})


class TestExistingEntryPoints:
    def test_unprepared_validation_is_unchanged(self):
        validator = Draft202012Validator(SCHEMA)
        assert validator.is_valid({"name": "abc", "x-1": 3})
        assert not validator.is_valid({"name": "ABC"})
        assert not validator.is_valid({"x-1": "nope"})
        jsonschema.validate({"name": "abc"}, SCHEMA)
        with pytest.raises(jsonschema.ValidationError):
            jsonschema.validate({"name": "ABC"}, SCHEMA)

    def test_prepare_does_not_leak_into_existing_validators(self):
        jsonschema.prepare(SCHEMA)
        assert not hasattr(Draft202012Validator, "_prepared_patterns")

    def test_prepare_with_explicit_class_and_format_checker(self):
        schema = {"format": "ipv4", "pattern": "^[0-9.]+$"}
        prepared = jsonschema.prepare(
            schema,
            cls=Draft202012Validator,
            format_checker=Draft202012Validator.FORMAT_CHECKER,
        )
        assert prepared.is_valid("127.0.0.1")
        assert not prepared.is_valid("999.1.1.1")
        assert not prepared.is_valid("abc.def.ghi")

    def test_validator_for_picks_the_dialect(self):
        schema = {
            "$schema": "http://json-schema.org/draft-07/schema#",
            "pattern": "^[a-z]+$",
        }
        prepared = jsonschema.prepare(schema)
        assert prepared.is_valid("abc")
        assert not prepared.is_valid("ABC")
        assert (
            prepared.validator.META_SCHEMA
            == validators.Draft7Validator.META_SCHEMA
        )
