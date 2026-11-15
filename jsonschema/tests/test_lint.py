from __future__ import annotations

from copy import deepcopy
from unittest import TestCase

from referencing import Registry
from referencing.jsonschema import DRAFT202012

from jsonschema import (
    Draft7Validator,
    Draft201909Validator,
    Draft202012Validator,
    ExtensionVocabulary,
    lint,
    validators,
)


class TestUnknownKeywordLint(TestCase):
    def test_reports_a_misspelled_keyword(self):
        issues = lint({"type": "string", "minLenght": 1})

        self.assertEqual(len(issues), 1)
        issue = issues[0]
        self.assertEqual(issue.resource, "")
        self.assertEqual(issue.pointer, "/minLenght")
        self.assertEqual(issue.keyword, "minLenght")
        self.assertEqual(issue.suggestion, "minLength")
        self.assertEqual(issue.code, "unknown-keyword")
        self.assertIn("minLength", issue.message)
        self.assertIn("minLenght", str(issue))

    def test_does_not_change_validation_or_the_schema(self):
        schema = {"type": "string", "minLenght": 3}
        original = deepcopy(schema)

        self.assertTrue(Draft202012Validator(schema).is_valid("ab"))
        lint(schema)
        self.assertEqual(schema, original)
        self.assertTrue(Draft202012Validator(schema).is_valid("ab"))

    def test_recognized_keywords_are_not_reported(self):
        schema = {
            "type": "object",
            "properties": {"name": {"type": "string", "minLength": 1}},
            "required": ["name"],
        }

        self.assertEqual(lint(schema), [])

    def test_property_names_are_not_keywords(self):
        schema = {
            "properties": {
                "minLenght": {"type": "string"},
                "x-anything": {"type": "number"},
            },
        }

        self.assertEqual(lint(schema), [])

    def test_property_values_are_subschemas(self):
        schema = {"properties": {"name": {"minLenght": 1}}}

        issues = lint(schema)

        self.assertEqual(len(issues), 1)
        self.assertEqual(
            issues[0].pointer,
            "/properties/name/minLenght",
        )

    def test_annotation_data_is_not_recursed(self):
        schema = {
            "default": {"minLenght": 1},
            "examples": [{"properties": {"minLenght": 2}}],
            "const": {"minLenght": 3},
            "enum": [{"minLenght": 4}],
            "title": "minLenght",
            "description": "minLenght",
            "$comment": "minLenght",
        }

        self.assertEqual(lint(schema), [])

    def test_unknown_values_are_not_recursed(self):
        schema = {"x-unknown": {"minLenght": 1}}

        self.assertEqual(
            [issue.keyword for issue in lint(schema)],
            ["x-unknown"],
        )

    def test_pointer_segments_are_escaped(self):
        issues = lint({"a/b~c": 1})

        self.assertEqual(issues[0].pointer, "/a~1b~0c")


class TestExtensionVocabularies(TestCase):
    def test_exact_extension_keyword(self):
        schema = {"x-acme": True, "minLenght": 1}

        issues = lint(
            schema,
            extensions=ExtensionVocabulary(keywords={"x-acme"}),
        )

        self.assertEqual([issue.keyword for issue in issues], ["minLenght"])

    def test_namespace_prefix(self):
        schema = {"x-acme": {"minLenght": 1}}

        self.assertEqual(lint(schema, extra_prefixes=("x-",)), [])

    def test_extension_mapping(self):
        self.assertEqual(
            lint({"x-acme": True}, extensions={"x-": ("acme",)}),
            [],
        )

    def test_mapping_can_declare_a_prefix(self):
        self.assertEqual(
            lint({"x-acme": True}, extensions={"x-": None}),
            [],
        )

    def test_mapping_can_declare_an_exact_keyword(self):
        self.assertEqual(
            lint({"x-acme": True}, extensions={"x-acme": True}),
            [],
        )

    def test_mapping_can_declare_vocabulary_keywords(self):
        self.assertEqual(
            lint(
                {"acme": True},
                extensions={
                    "https://example.com/vocabulary": ("acme",),
                },
            ),
            [],
        )

    def test_non_matching_keyword_is_still_reported(self):
        issues = lint({"other": True}, extra_prefixes=("x-",))

        self.assertEqual([issue.keyword for issue in issues], ["other"])

    def test_custom_validator_keywords_are_known(self):
        Validator = validators.extend(
            Draft202012Validator,
            validators={"acme": lambda *args: ()},
        )

        self.assertEqual(lint({"acme": True}, cls=Validator), [])


class TestDraftKeywordSets(TestCase):
    def test_prefix_items_is_unknown_before_draft_2020_12(self):
        issues = lint({"prefixItems": []}, cls=Draft7Validator)

        self.assertEqual([issue.keyword for issue in issues], ["prefixItems"])
        self.assertEqual(
            lint({"prefixItems": []}, cls=Draft202012Validator),
            [],
        )

    def test_dependent_schemas_is_unknown_before_draft_2019_09(self):
        schema = {"dependentSchemas": {"name": {"type": "string"}}}

        issues = lint(schema, cls=Draft7Validator)

        self.assertEqual(
            [issue.keyword for issue in issues],
            ["dependentSchemas"],
        )
        self.assertEqual(lint(schema, cls=Draft201909Validator), [])

    def test_schema_uri_selects_the_dialect(self):
        issues = lint(
            {
                "$schema": "http://json-schema.org/draft-07/schema#",
                "prefixItems": [],
            },
        )

        self.assertEqual([issue.keyword for issue in issues], ["prefixItems"])


class TestResourcesAndReferences(TestCase):
    def test_base_uri_is_used_for_a_root_without_an_id(self):
        issues = lint(
            {"minLenght": 1},
            base_uri="https://example.com/root",
        )

        self.assertEqual(issues[0].resource, "https://example.com/root")

    def test_referenced_documents_are_linted(self):
        other = {
            "$id": "https://example.com/other",
            "properties": {"name": {"minLenght": 1}},
        }
        registry = Registry().with_resource(
            "https://example.com/other",
            DRAFT202012.create_resource(other),
        )
        schema = {
            "$id": "https://example.com/root",
            "$ref": "https://example.com/other",
        }

        issues = lint(schema, registry=registry)

        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0].resource, "https://example.com/other")
        self.assertEqual(
            issues[0].pointer,
            "/properties/name/minLenght",
        )

    def test_references_can_be_ignored(self):
        other = {"$id": "https://example.com/other", "minLenght": 1}
        registry = Registry().with_resource(
            "https://example.com/other",
            DRAFT202012.create_resource(other),
        )
        schema = {"$ref": "https://example.com/other"}

        self.assertEqual(
            lint(schema, registry=registry, follow_references=False),
            [],
        )

    def test_resources_are_linted_only_once(self):
        other = {"$id": "https://example.com/other", "minLenght": 1}
        registry = Registry().with_resource(
            "https://example.com/other",
            DRAFT202012.create_resource(other),
        )
        schema = {
            "properties": {
                "first": {"$ref": "https://example.com/other"},
                "second": {"$ref": "https://example.com/other"},
            },
        }

        self.assertEqual(len(lint(schema, registry=registry)), 1)

    def test_embedded_resource_reference_does_not_retrieve(self):
        def retrieve(uri):
            raise AssertionError(f"Unexpected retrieval of {uri}")

        schema = {
            "$id": "https://example.com/root",
            "allOf": [{"$ref": "https://example.com/child"}],
            "properties": {
                "child": {
                    "$id": "https://example.com/child",
                    "minLenght": 1,
                },
            },
        }

        issues = lint(schema, registry=Registry(retrieve=retrieve))

        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0].resource, "https://example.com/child")

    def test_recursive_references_terminate(self):
        schema = {
            "$id": "https://example.com/recursive",
            "properties": {"next": {"$ref": "#"}},
            "minLenght": 1,
        }

        self.assertEqual(len(lint(schema)), 1)

    def test_local_pointer_references_do_not_duplicate(self):
        schema = {
            "properties": {"name": {"minLenght": 1}},
            "allOf": [{"$ref": "#/properties/name"}],
        }

        self.assertEqual(len(lint(schema)), 1)

    def test_nested_id_starts_a_new_resource_and_pointer(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {
                "child": {
                    "$id": "https://example.com/child",
                    "minLenght": 1,
                },
            },
        }

        issues = lint(schema)

        self.assertEqual(len(issues), 1)
        self.assertEqual(issues[0].resource, "https://example.com/child")
        self.assertEqual(issues[0].pointer, "/minLenght")

    def test_nested_resource_can_switch_dialects(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {
                "child": {
                    "$id": "https://example.com/child",
                    "$schema": "http://json-schema.org/draft-07/schema#",
                    "dependentSchemas": {"name": {"type": "string"}},
                },
            },
        }

        issues = lint(schema)

        self.assertEqual(
            [issue.keyword for issue in issues],
            ["dependentSchemas"],
        )

    def test_referenced_resource_uses_its_own_dialect(self):
        other = {
            "$id": "https://example.com/other",
            "$schema": "http://json-schema.org/draft-07/schema#",
            "dependentSchemas": {"name": {"type": "string"}},
        }
        registry = Registry().with_resource(
            "https://example.com/other",
            DRAFT202012.create_resource(other),
        )
        schema = {"$ref": "https://example.com/other"}

        issues = lint(schema, registry=registry)

        self.assertEqual(
            [issue.keyword for issue in issues],
            ["dependentSchemas"],
        )

    def test_missing_referenced_resource_is_skipped(self):
        schema = {"$ref": "https://example.com/missing"}

        self.assertEqual(lint(schema), [])


class TestSorting(TestCase):
    def test_issues_are_sorted_by_resource_and_pointer(self):
        schema = {
            "$id": "https://example.com/z",
            "properties": {
                "z": {"minLenght": 1},
                "a": {"minLenght": 1},
            },
        }

        issues = lint(schema)

        self.assertEqual(
            [issue.pointer for issue in issues],
            [
                "/properties/a/minLenght",
                "/properties/z/minLenght",
            ],
        )

    def test_issues_are_sorted_across_resources(self):
        other = {"$id": "https://example.com/a", "minLenght": 1}
        registry = Registry().with_resource(
            "https://example.com/a",
            DRAFT202012.create_resource(other),
        )
        schema = {
            "$id": "https://example.com/z",
            "$ref": "https://example.com/a",
            "minLenght": 1,
        }

        issues = lint(schema, registry=registry)

        self.assertEqual(
            [issue.resource for issue in issues],
            ["https://example.com/a", "https://example.com/z"],
        )
