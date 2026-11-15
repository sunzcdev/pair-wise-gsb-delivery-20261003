"""
Tests for reference-chain attribution of validation errors.
"""

from unittest import TestCase

import referencing
import referencing.exceptions
from referencing.jsonschema import DRAFT202012

from jsonschema import (
    Draft202012Validator,
    exceptions,
)


class TestReferenceChain(TestCase):
    def resource(self, uri, schema):
        return uri, DRAFT202012.create_resource(schema)

    def test_multihop_reports_entry_and_declaration(self):
        registry = referencing.Registry().with_resources([
            self.resource(
                "https://example.com/a.json",
                {
                    "$id": "https://example.com/a.json",
                    "$ref": "b.json",
                },
            ),
            self.resource(
                "https://example.com/b.json",
                {
                    "$id": "https://example.com/b.json",
                    "$ref": "c.json#/$defs/contract",
                },
            ),
            self.resource(
                "https://example.com/c.json",
                {
                    "$id": "https://example.com/c.json",
                    "$defs": {
                        "contract": {"type": "integer", "minimum": 5},
                    },
                },
            ),
        ])
        schema = {
            "$id": "https://example.com/root.json",
            "properties": {"x": {"$ref": "a.json"}},
        }
        (error,) = Draft202012Validator(
            schema,
            registry=registry,
        ).iter_errors({"x": 1})

        self.assertEqual(list(error.path), ["x"])
        self.assertEqual(
            error.entry_location,
            "https://example.com/root.json#/properties/x/$ref",
        )
        self.assertEqual(
            error.declaration_location,
            "https://example.com/c.json#/$defs/contract/minimum",
        )
        self.assertEqual(len(error.ref_chain), 3)
        self.assertEqual(
            [
                (step.source, step.ref, step.target)
                for step in error.ref_chain
            ],
            [
                (
                    "https://example.com/root.json",
                    "a.json",
                    "https://example.com/a.json",
                ),
                (
                    "https://example.com/a.json",
                    "b.json",
                    "https://example.com/b.json",
                ),
                (
                    "https://example.com/b.json",
                    "c.json#/$defs/contract",
                    "https://example.com/c.json#/$defs/contract",
                ),
            ],
        )
        self.assertIn(error.entry_location, str(error))
        self.assertIn(error.declaration_location, str(error))

    def test_two_entry_references_to_one_contract_stay_distinct(self):
        schema = {
            "$defs": {"contract": {"type": "integer", "minimum": 5}},
            "properties": {
                "a": {"$ref": "#/$defs/contract"},
                "b": {"$ref": "#/$defs/contract"},
            },
        }
        errors = list(
            Draft202012Validator(schema).iter_errors({"a": 1, "b": 2}),
        )
        self.assertEqual(len(errors), 2)
        by_instance = {tuple(error.path): error for error in errors}
        self.assertEqual(
            by_instance[("a",)].entry_location,
            "#/properties/a/$ref",
        )
        self.assertEqual(
            by_instance[("b",)].entry_location,
            "#/properties/b/$ref",
        )
        for error in errors:
            self.assertEqual(
                error.declaration_location,
                "#/$defs/contract/minimum",
            )
            self.assertEqual(len(error.ref_chain), 1)

    def test_recursive_chain_is_the_actual_finite_path(self):
        node = {
            "$id": "https://example.com/node.json",
            "type": "object",
            "properties": {
                "value": {"type": "integer"},
                "next": {"$ref": "https://example.com/node.json"},
            },
            "required": ["value"],
        }
        registry = referencing.Registry().with_resources([
            self.resource("https://example.com/node.json", node),
        ])
        schema = {"$ref": "https://example.com/node.json"}
        instance = {
            "value": 1,
            "next": {
                "value": 2,
                "next": {"value": "not an integer"},
            },
        }
        (error,) = Draft202012Validator(
            schema,
            registry=registry,
        ).iter_errors(instance)

        self.assertEqual(list(error.path), ["next", "next", "value"])
        self.assertEqual(len(error.ref_chain), 3)
        self.assertEqual(
            [step.source_path for step in error.ref_chain],
            [
                ("$ref",),
                ("properties", "next", "$ref"),
                ("properties", "next", "$ref"),
            ],
        )

    def test_resolution_failure_is_not_a_validation_error(self):
        registry = referencing.Registry().with_resources([
            self.resource(
                "https://example.com/a.json",
                {
                    "$id": "https://example.com/a.json",
                    "properties": {"x": {"$ref": "missing.json"}},
                },
            ),
        ])
        schema = {
            "$id": "https://example.com/root.json",
            "$ref": "a.json",
        }
        validator = Draft202012Validator(schema, registry=registry)
        with self.assertRaises(
            exceptions.ReferenceResolutionError,
        ) as caught:
            validator.validate({"x": 1})

        error = caught.exception
        self.assertNotIsInstance(error, exceptions.ValidationError)
        self.assertIsInstance(
            error,
            referencing.exceptions.Unresolvable,
        )
        self.assertEqual(
            error.entry_location,
            "https://example.com/root.json#/$ref",
        )
        self.assertEqual(len(error.chain), 2)
        self.assertEqual(
            error.chain[-1].target,
            "https://example.com/missing.json",
        )

    def test_resolution_failure_escapes_anyOf(self):
        schema = {"anyOf": [{"$ref": "urn:not-registered"}]}
        with self.assertRaises(
            exceptions.ReferenceResolutionError,
        ) as caught:
            list(Draft202012Validator(schema).iter_errors(12))
        self.assertNotIsInstance(
            caught.exception,
            exceptions.ValidationError,
        )
