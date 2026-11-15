from unittest import TestCase

from jsonschema import (
    Draft202012Validator,
    Draft7Validator,
    ValidationError,
    suggest_defaults,
    validate,
)
from jsonschema._suggest import CONFLICT, INVALID, OK


class TestSuggestDefaults(TestCase):
    def test_missing_property_gets_candidate_with_location(self):
        schema = {
            "type": "object",
            "properties": {"name": {"type": "string", "default": "anon"}},
        }
        (suggestion,) = suggest_defaults({}, schema)

        self.assertEqual(suggestion.path, ())
        self.assertEqual(suggestion.property, "name")
        self.assertEqual(suggestion.status, OK)
        self.assertEqual(suggestion.value, "anon")
        (candidate,) = suggestion.candidates
        self.assertEqual(candidate.schema_path, ("properties", "name"))
        self.assertIsNone(candidate.branch)
        self.assertEqual(candidate.errors, ())

    def test_neither_instance_nor_schema_is_modified(self):
        schema = {"properties": {"tags": {"default": []}}}
        instance = {}

        (suggestion,) = suggest_defaults(instance, schema)
        suggestion.value.append("mutated")

        self.assertEqual(instance, {})
        self.assertEqual(schema, {"properties": {"tags": {"default": []}}})

    def test_explicit_null_is_not_missing(self):
        schema = {"properties": {"name": {"default": "anon"}}}
        self.assertEqual(suggest_defaults({"name": None}, schema), [])

    def test_present_property_is_not_missing(self):
        schema = {"properties": {"name": {"default": "anon"}}}
        self.assertEqual(suggest_defaults({"name": "given"}, schema), [])

    def test_allOf_with_differing_candidates_is_a_conflict(self):
        schema = {
            "allOf": [
                {"properties": {"x": {"default": 1}}},
                {"properties": {"x": {"default": 2}}},
            ],
        }
        (suggestion,) = suggest_defaults({}, schema)

        self.assertEqual(suggestion.status, CONFLICT)
        self.assertIsNone(suggestion.value)
        self.assertEqual(
            [(c.value, c.branch) for c in suggestion.candidates],
            [(1, "allOf[0]"), (2, "allOf[1]")],
        )

    def test_allOf_with_agreeing_candidates_is_ok(self):
        schema = {
            "allOf": [
                {"properties": {"x": {"default": 1}}},
                {"properties": {"x": {"default": 1}}},
            ],
        }
        (suggestion,) = suggest_defaults({}, schema)

        self.assertEqual(suggestion.status, OK)
        self.assertEqual(suggestion.value, 1)
        self.assertEqual(len(suggestion.candidates), 2)

    def test_oneOf_uniquely_selected_contributes_candidates(self):
        schema = {
            "properties": {"kind": {"type": "string"}},
            "oneOf": [
                {
                    "properties": {"kind": {"const": "a"}},
                    "required": ["kind"],
                },
                {
                    "properties": {
                        "kind": {"const": "b"},
                        "b_opt": {"default": "B"},
                    },
                    "required": ["kind"],
                },
            ],
        }
        (suggestion,) = suggest_defaults({"kind": "b"}, schema)

        self.assertEqual(suggestion.property, "b_opt")
        self.assertEqual(suggestion.value, "B")
        self.assertEqual(suggestion.candidates[0].branch, "oneOf[1]")

    def test_oneOf_without_unique_match_suggests_nothing(self):
        schema = {
            "oneOf": [
                {"properties": {"x": {"default": 1}}},
                {"properties": {"x": {"default": 2}}},
            ],
        }
        # the empty instance matches both branches
        self.assertEqual(suggest_defaults({}, schema), [])

    def test_oneOf_with_no_match_suggests_nothing(self):
        schema = {
            "oneOf": [
                {"required": ["a"], "properties": {"x": {"default": 1}}},
                {"required": ["b"], "properties": {"x": {"default": 2}}},
            ],
        }
        self.assertEqual(suggest_defaults({}, schema), [])

    def test_candidate_violating_the_property_contract_is_reported(self):
        schema = {
            "properties": {
                "port": {"type": "integer", "minimum": 1, "default": 0},
            },
        }
        (suggestion,) = suggest_defaults({}, schema)

        self.assertEqual(suggestion.status, INVALID)
        self.assertEqual(suggestion.value, 0)
        (candidate,) = suggestion.candidates
        self.assertEqual(len(candidate.errors), 1)
        self.assertIn("minimum", candidate.errors[0])

    def test_nested_missing_objects_are_suggested(self):
        schema = {
            "properties": {
                "a": {"properties": {"b": {"default": 1}}},
            },
        }
        (suggestion,) = suggest_defaults({}, schema)

        self.assertEqual(suggestion.path, ("a",))
        self.assertEqual(suggestion.property, "b")
        self.assertEqual(suggestion.value, 1)

    def test_defaults_within_present_subinstances(self):
        schema = {
            "properties": {
                "a": {"properties": {"b": {"default": 1}}},
            },
        }
        (suggestion,) = suggest_defaults({"a": {}}, schema)

        self.assertEqual(suggestion.path, ("a",))
        self.assertEqual(suggestion.property, "b")

    def test_ref_is_followed(self):
        schema = {
            "$defs": {"base": {"properties": {"y": {"default": 3}}}},
            "$ref": "#/$defs/base",
        }
        (suggestion,) = suggest_defaults({}, schema)

        self.assertEqual(suggestion.property, "y")
        self.assertEqual(suggestion.value, 3)

    def test_recursive_schemas_terminate(self):
        schema = {
            "properties": {
                "next": {"$ref": "#"},
                "v": {"default": 0},
            },
        }
        suggestions = suggest_defaults({}, schema)
        self.assertEqual(
            {(s.path, s.property) for s in suggestions},
            {((), "v"), (("next",), "v")},
        )

    def test_if_then_else_branches(self):
        schema = {
            "if": {
                "properties": {"kind": {"const": "a"}},
                "required": ["kind"],
            },
            "then": {"properties": {"t": {"default": "T"}}},
            "else": {"properties": {"e": {"default": "E"}}},
        }
        (then,) = suggest_defaults({"kind": "a"}, schema)
        self.assertEqual((then.property, then.value), ("t", "T"))
        self.assertEqual(then.candidates[0].branch, "then")

        (else_,) = suggest_defaults({}, schema)
        self.assertEqual((else_.property, else_.value), ("e", "E"))
        self.assertEqual(else_.candidates[0].branch, "else")

    def test_array_items_are_suggested(self):
        schema = {
            "items": {
                "type": "object",
                "properties": {"x": {"default": 1}},
            },
        }
        suggestions = suggest_defaults([{}, {}], schema)
        self.assertEqual(
            [(s.path, s.property, s.value) for s in suggestions],
            [((0,), "x", 1), ((1,), "x", 1)],
        )

    def test_validate_does_not_fill_defaults(self):
        schema = {
            "required": ["x"],
            "properties": {"x": {"default": 1}},
        }
        instance = {}
        with self.assertRaises(ValidationError):
            validate(instance, schema)
        self.assertEqual(instance, {})

    def test_validator_method(self):
        validator = Draft7Validator({"properties": {"z": {"default": 9}}})
        (suggestion,) = validator.suggest_defaults({})
        self.assertEqual((suggestion.property, suggestion.value), ("z", 9))

    def test_specific_validator_class_is_used(self):
        schema = {"properties": {"z": {"default": 9}}}
        (suggestion,) = suggest_defaults({}, schema, cls=Draft202012Validator)
        self.assertEqual(suggestion.value, 9)
