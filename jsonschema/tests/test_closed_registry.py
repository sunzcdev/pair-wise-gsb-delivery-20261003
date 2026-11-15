"""
Tests for closed reference mode (see jsonschema.closed_registry).
"""

from unittest import TestCase, mock
import json

import referencing
import referencing.exceptions
from referencing.jsonschema import DRAFT202012

from jsonschema import Draft202012Validator, closed_registry, exceptions
from jsonschema.validators import _refuse_retrieval


def registered(uri, schema):
    return uri, DRAFT202012.create_resource(schema)


class TestClosedRegistry(TestCase):
    def test_unregistered_reference_raises_closed_reference_error(self):
        schema = {"$ref": "https://example.com/missing.json"}
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError) as caught:
            validator.validate(12)

        error = caught.exception
        self.assertEqual(error.uri, "https://example.com/missing.json")
        self.assertEqual(error.ref, "https://example.com/missing.json")
        self.assertEqual(error.chain, ("https://example.com/missing.json",))

    def test_closed_reference_error_is_not_a_validation_error(self):
        schema = {"$ref": "https://example.com/missing.json"}
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError) as caught:
            validator.validate(12)

        error = caught.exception
        self.assertNotIsInstance(error, exceptions.ValidationError)
        self.assertIsInstance(
            error,
            referencing.exceptions.Unresolvable,
        )

    def test_is_valid_does_not_convert_refusal_to_false(self):
        schema = {"$ref": "https://example.com/missing.json"}
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError):
            validator.is_valid(12)

    def test_iter_errors_propagates_refusal(self):
        schema = {"$ref": "https://example.com/missing.json"}
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError):
            list(validator.iter_errors(12))

    def test_error_message_shows_missing_resource_and_chain(self):
        registry = closed_registry().with_resources([
            registered(
                "https://example.com/a.json",
                {
                    "$id": "https://example.com/a.json",
                    "properties": {"x": {"$ref": "b.json"}},
                },
            ),
        ])
        schema = {"$ref": "https://example.com/a.json"}
        validator = Draft202012Validator(schema, registry=registry)

        with self.assertRaises(exceptions.ClosedReferenceError) as caught:
            validator.validate({"x": 1})

        error = caught.exception
        self.assertEqual(error.uri, "https://example.com/b.json")
        self.assertEqual(error.ref, "b.json")
        self.assertEqual(
            error.chain,
            ("https://example.com/a.json", "b.json"),
        )
        message = str(error)
        self.assertIn("https://example.com/b.json", message)
        self.assertIn("https://example.com/a.json", message)
        self.assertIn("b.json", message)

    def test_reference_chain_spans_multiple_registered_resources(self):
        registry = closed_registry().with_resources([
            registered(
                "https://example.com/a.json",
                {
                    "$id": "https://example.com/a.json",
                    "$ref": "b.json",
                },
            ),
            registered(
                "https://example.com/b.json",
                {
                    "$id": "https://example.com/b.json",
                    "$ref": "c.json",
                },
            ),
        ])
        schema = {"$ref": "https://example.com/a.json"}
        validator = Draft202012Validator(schema, registry=registry)

        with self.assertRaises(exceptions.ClosedReferenceError) as caught:
            validator.validate(12)

        error = caught.exception
        self.assertEqual(error.uri, "https://example.com/c.json")
        self.assertEqual(
            error.chain,
            ("https://example.com/a.json", "b.json", "c.json"),
        )

    def test_no_network_or_filesystem_access(self):
        schema = {"$ref": "https://example.com/missing.json"}
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with (
            mock.patch("urllib.request.urlopen") as urlopen,
            mock.patch("builtins.open") as open_,
            self.assertRaises(exceptions.ClosedReferenceError),
        ):
            validator.validate(12)
        urlopen.assert_not_called()
        open_.assert_not_called()

    def test_closed_registry_discards_existing_retrieval_behavior(self):
        calls = []

        def retrieve(uri):  # pragma: no cover
            calls.append(uri)
            return DRAFT202012.create_resource({"type": "integer"})

        registry = closed_registry(referencing.Registry(retrieve=retrieve))
        schema = {"$ref": "https://example.com/missing.json"}
        validator = Draft202012Validator(schema, registry=registry)

        with self.assertRaises(exceptions.ClosedReferenceError):
            validator.validate(12)
        self.assertEqual(calls, [])

    def test_wrapping_preserves_registered_resources(self):
        registry = referencing.Registry().with_resources([
            registered("https://example.com/int.json", {"type": "integer"}),
        ])
        validator = Draft202012Validator(
            {"$ref": "https://example.com/int.json"},
            registry=closed_registry(registry),
        )
        self.assertTrue(validator.is_valid(12))
        self.assertFalse(validator.is_valid("foo"))

    def test_closed_registry_defaults_to_empty(self):
        registry = closed_registry()
        self.assertEqual(len(registry.crawl()), 0)

    def test_relative_ids_and_nested_resources_resolve_by_base_uri(self):
        root = {
            "$id": "https://example.com/schemas/root.json",
            "$defs": {
                "child": {"$id": "child.json", "type": "integer"},
            },
            "properties": {"child": {"$ref": "child.json"}},
        }
        registry = closed_registry().with_resources([
            registered("https://example.com/schemas/root.json", root),
        ])
        schema = {"$ref": "https://example.com/schemas/root.json"}
        validator = Draft202012Validator(schema, registry=registry)

        self.assertTrue(validator.is_valid({"child": 12}))
        errors = list(validator.iter_errors({"child": "nope"}))
        self.assertEqual(len(errors), 1)
        self.assertIsInstance(errors[0], exceptions.ValidationError)

    def test_fragment_anchors_resolve(self):
        root = {
            "$id": "https://example.com/anchored.json",
            "$defs": {
                "positive": {
                    "$anchor": "positive",
                    "type": "integer",
                    "minimum": 0,
                },
            },
            "$ref": "#positive",
        }
        registry = closed_registry().with_resources([
            registered("https://example.com/anchored.json", root),
        ])
        schema = {"$ref": "https://example.com/anchored.json"}
        validator = Draft202012Validator(schema, registry=registry)

        self.assertTrue(validator.is_valid(3))
        self.assertFalse(validator.is_valid(-3))

    def test_json_pointer_fragments_resolve(self):
        root = {
            "$id": "https://example.com/pointed.json",
            "$defs": {"thing": {"type": "string"}},
        }
        registry = closed_registry().with_resources([
            registered("https://example.com/pointed.json", root),
        ])
        schema = {"$ref": "https://example.com/pointed.json#/$defs/thing"}
        validator = Draft202012Validator(schema, registry=registry)

        self.assertTrue(validator.is_valid("foo"))
        self.assertFalse(validator.is_valid(12))

    def test_recursion_within_registered_resources(self):
        node = {
            "$id": "https://example.com/node.json",
            "type": "object",
            "properties": {
                "value": {"type": "string"},
                "next": {"$ref": "node.json"},
            },
            "required": ["value"],
        }
        registry = closed_registry().with_resources([
            registered("https://example.com/node.json", node),
        ])
        schema = {"$ref": "https://example.com/node.json"}
        validator = Draft202012Validator(schema, registry=registry)

        valid = {"value": "a", "next": {"value": "b", "next": {"value": "c"}}}
        invalid = {"value": "a", "next": {"value": 12}}
        self.assertTrue(validator.is_valid(valid))
        self.assertFalse(validator.is_valid(invalid))

    def test_mutual_recursion_across_registered_resources(self):
        registry = closed_registry().with_resources([
            registered(
                "https://example.com/even.json",
                {
                    "$id": "https://example.com/even.json",
                    "type": "object",
                    "properties": {"next": {"$ref": "odd.json"}},
                    "required": ["next"],
                },
            ),
            registered(
                "https://example.com/odd.json",
                {
                    "$id": "https://example.com/odd.json",
                    "type": "object",
                    "properties": {"next": {"$ref": "even.json"}},
                },
            ),
        ])
        schema = {"$ref": "https://example.com/even.json"}
        validator = Draft202012Validator(schema, registry=registry)

        self.assertTrue(validator.is_valid({"next": {}}))
        self.assertFalse(validator.is_valid({"next": {"next": 12}}))

    def test_anyof_does_not_swallow_closed_reference_errors(self):
        schema = {
            "anyOf": [
                {"type": "string"},
                {"$ref": "https://example.com/missing.json"},
            ],
        }
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError):
            validator.validate(12)

    def test_oneof_does_not_swallow_closed_reference_errors(self):
        schema = {
            "oneOf": [
                {"type": "string"},
                {"$ref": "https://example.com/missing.json"},
            ],
        }
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError):
            validator.validate(12)

    def test_anyof_still_reports_normal_validation_failures(self):
        registry = closed_registry().with_resources([
            registered("https://example.com/int.json", {"type": "integer"}),
        ])
        schema = {
            "anyOf": [
                {"type": "string"},
                {"$ref": "https://example.com/int.json"},
            ],
        }
        validator = Draft202012Validator(schema, registry=registry)

        self.assertTrue(validator.is_valid("foo"))
        self.assertTrue(validator.is_valid(12))
        errors = list(validator.iter_errors(12.5))
        self.assertEqual(len(errors), 1)
        self.assertEqual(errors[0].validator, "anyOf")

    def test_nested_closed_reference_error_keeps_its_chain(self):
        registry = closed_registry().with_resources([
            registered(
                "https://example.com/a.json",
                {
                    "$id": "https://example.com/a.json",
                    "properties": {"x": {"$ref": "missing.json"}},
                },
            ),
        ])
        schema = {
            "properties": {
                "outer": {"$ref": "https://example.com/a.json"},
            },
        }
        validator = Draft202012Validator(schema, registry=registry)

        with self.assertRaises(exceptions.ClosedReferenceError) as caught:
            validator.validate({"outer": {"x": 1}})

        error = caught.exception
        self.assertEqual(error.uri, "https://example.com/missing.json")
        self.assertEqual(
            error.chain,
            ("https://example.com/a.json", "missing.json"),
        )

    def test_unresolvable_pointers_are_not_confused_with_refusals(self):
        registry = closed_registry().with_resources([
            registered("https://example.com/root.json", {"type": "integer"}),
        ])
        schema = {"$ref": "https://example.com/root.json#/nope"}
        validator = Draft202012Validator(schema, registry=registry)

        with self.assertRaises(referencing.exceptions.Unresolvable) as caught:
            validator.validate(12)
        self.assertNotIsInstance(
            caught.exception,
            exceptions.ClosedReferenceError,
        )

    def test_refuse_retrieval_raises(self):
        with self.assertRaises(Exception) as caught:
            _refuse_retrieval("https://example.com/missing.json")
        self.assertEqual(
            caught.exception.uri,
            "https://example.com/missing.json",
        )

    def test_meta_schemas_remain_available(self):
        schema = {
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "$ref": "https://example.com/missing.json",
        }
        validator = Draft202012Validator(
            schema,
            registry=closed_registry(),
        )
        with self.assertRaises(exceptions.ClosedReferenceError):
            validator.validate(12)

    def test_serializing_the_error(self):
        error = exceptions.ClosedReferenceError(
            ref="missing.json",
            uri="https://example.com/missing.json",
            chain=("a.json", "missing.json"),
        )
        self.assertEqual(
            json.loads(json.dumps(str(error))),
            str(error),
        )
        self.assertEqual(
            error,
            exceptions.ClosedReferenceError(
                ref="missing.json",
                uri="https://example.com/missing.json",
                chain=("a.json", "missing.json"),
            ),
        )
