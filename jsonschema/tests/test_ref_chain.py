"""
Tests for the reference chain view on validation errors.

When validation descends through references (``$ref``, ``$dynamicRef``,
``$recursiveRef``), errors are expected to carry a ``ref_chain`` which
preserves, for each hop, the source resource, the reference text and the
resolution target -- alongside the usual instance ``path`` and
``schema_path`` information.
"""

from unittest import TestCase

import referencing
import referencing.exceptions

from jsonschema import (
    Draft201909Validator,
    Draft202012Validator,
    exceptions,
)


class TestRefChain(TestCase):
    def test_single_ref_failure_records_one_segment(self):
        schema = {
            "$ref": "#/$defs/name",
            "$defs": {"name": {"type": "string"}},
        }
        error = next(Draft202012Validator(schema).iter_errors(12))

        self.assertEqual(
            error.ref_chain,
            (
                exceptions.RefChainSegment(
                    source="",
                    ref="#/$defs/name",
                    target="#/$defs/name",
                ),
            ),
        )
        # the final constraint declaration location is still reported
        self.assertEqual(list(error.absolute_schema_path), ["type"])
        self.assertEqual(error.validator, "type")

    def test_multi_hop_ref_reports_entry_and_declaration(self):
        schema = {
            "$id": "https://example.com/root",
            "$ref": "a.json",
            "$defs": {
                "a": {"$id": "a.json", "$ref": "b.json"},
                "b": {
                    "$id": "b.json",
                    "properties": {"x": {"type": "string"}},
                },
            },
        }
        instance = {"x": 12}
        error = next(Draft202012Validator(schema).iter_errors(instance))

        # one segment per hop, outermost (entry) first
        self.assertEqual(
            list(error.ref_chain),
            [
                exceptions.RefChainSegment(
                    source="https://example.com/root",
                    ref="a.json",
                    target="https://example.com/a.json",
                ),
                exceptions.RefChainSegment(
                    source="https://example.com/a.json",
                    ref="b.json",
                    target="https://example.com/b.json",
                ),
            ],
        )
        # the entry usage location...
        self.assertEqual(
            error.entry_ref,
            exceptions.RefChainSegment(
                source="https://example.com/root",
                ref="a.json",
                target="https://example.com/a.json",
            ),
        )
        # ...and the final constraint declaration location
        self.assertEqual(
            error.ref_chain[-1].target,
            "https://example.com/b.json",
        )
        self.assertEqual(
            list(error.absolute_schema_path),
            ["properties", "x", "type"],
        )
        # the instance error keeps its original path
        self.assertEqual(list(error.absolute_path), ["x"])
        self.assertEqual(error.json_path, "$.x")
        self.assertIs(error.instance, instance["x"])

    def test_entry_ref_is_none_without_references(self):
        schema = {"type": "string"}
        error = next(Draft202012Validator(schema).iter_errors(12))
        self.assertEqual(error.ref_chain, ())
        self.assertIsNone(error.entry_ref)

    def test_two_entries_to_same_contract_are_attributed_separately(self):
        schema = {
            "properties": {
                "a": {"$ref": "#/$defs/contract"},
                "b": {"$ref": "#/$defs/contract"},
            },
            "$defs": {"contract": {"type": "string"}},
        }
        errors = list(
            Draft202012Validator(schema).iter_errors({"a": 1, "b": 2}),
        )
        self.assertEqual(len(errors), 2)

        by_instance_path = {tuple(e.absolute_path): e for e in errors}
        error_a = by_instance_path[("a",)]
        error_b = by_instance_path[("b",)]

        # each failure is attributed to its own call site
        self.assertEqual(
            list(error_a.absolute_schema_path)[:2],
            ["properties", "a"],
        )
        self.assertEqual(
            list(error_b.absolute_schema_path)[:2],
            ["properties", "b"],
        )

        # each carries its own chain back to its own entry $ref
        self.assertIsNot(error_a.ref_chain, error_b.ref_chain)
        for error in (error_a, error_b):
            self.assertEqual(len(error.ref_chain), 1)
            self.assertEqual(error.ref_chain[0].ref, "#/$defs/contract")
            self.assertEqual(error.ref_chain[0].target, "#/$defs/contract")

    def test_same_target_via_two_resources(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {
                "a": {"$ref": "left.json"},
                "b": {"$ref": "right.json"},
            },
            "$defs": {
                "left": {"$id": "left.json", "$ref": "contract.json"},
                "right": {"$id": "right.json", "$ref": "contract.json"},
                "contract": {"$id": "contract.json", "type": "string"},
            },
        }
        errors = list(
            Draft202012Validator(schema).iter_errors({"a": 1, "b": 2}),
        )
        self.assertEqual(len(errors), 2)

        by_instance_path = {tuple(e.absolute_path): e for e in errors}
        error_a, error_b = by_instance_path[("a",)], by_instance_path[("b",)]

        # both failures reached the same final contract...
        self.assertEqual(
            error_a.ref_chain[-1].target,
            "https://example.com/contract.json",
        )
        self.assertEqual(
            error_b.ref_chain[-1].target,
            "https://example.com/contract.json",
        )
        # ...but the chains point back to the respective call sites
        self.assertEqual(
            [segment.target for segment in error_a.ref_chain],
            [
                "https://example.com/left.json",
                "https://example.com/contract.json",
            ],
        )
        self.assertEqual(
            [segment.target for segment in error_b.ref_chain],
            [
                "https://example.com/right.json",
                "https://example.com/contract.json",
            ],
        )

    def test_recursion_records_only_the_hops_actually_walked(self):
        schema = {
            "type": "object",
            "properties": {"child": {"$ref": "#"}},
        }
        instance = {"child": {"child": {"child": 12}}}
        errors = list(Draft202012Validator(schema).iter_errors(instance))
        self.assertEqual(len(errors), 1)
        (error,) = errors

        # the instance was three "$ref": "#" hops deep, no more, no less
        self.assertEqual(len(error.ref_chain), 3)
        for segment in error.ref_chain:
            self.assertEqual(segment.ref, "#")
        self.assertEqual(
            list(error.absolute_path),
            ["child", "child", "child"],
        )

    def test_dynamic_ref_records_a_segment(self):
        schema = {
            "$id": "https://example.com/main",
            "$ref": "other",
            "$defs": {
                "other": {
                    "$id": "other",
                    "$dynamicRef": "#thing",
                    "$defs": {
                        "thing": {
                            "$dynamicAnchor": "thing",
                            "type": "string",
                        },
                    },
                },
            },
        }
        error = next(Draft202012Validator(schema).iter_errors(12))

        self.assertEqual(
            [segment.ref for segment in error.ref_chain],
            ["other", "#thing"],
        )
        self.assertEqual(
            error.ref_chain[0].target,
            "https://example.com/other",
        )

    def test_recursive_ref_records_a_segment(self):
        schema = {
            "$id": "https://example.com/tree",
            "$recursiveAnchor": True,
            "type": "object",
            "properties": {"child": {"$recursiveRef": "#"}},
        }
        instance = {"child": {"child": 12}}
        errors = list(Draft201909Validator(schema).iter_errors(instance))
        self.assertEqual(len(errors), 1)
        (error,) = errors

        # one segment per recursive hop actually walked
        self.assertEqual(len(error.ref_chain), 2)
        for segment in error.ref_chain:
            self.assertEqual(segment.ref, "#")
            self.assertEqual(
                segment.target,
                "https://example.com/tree",
            )
        self.assertEqual(list(error.absolute_path), ["child", "child"])

    def test_unresolvable_ref_is_not_wrapped_as_a_validation_error(self):
        schema = {"$ref": "https://example.com/does-not-exist.json"}
        validator = Draft202012Validator(
            schema,
            registry=referencing.Registry(),
        )

        with self.assertRaises(referencing.exceptions.Unresolvable):
            validator.validate(12)
        with self.assertRaises(referencing.exceptions.Unresolvable):
            list(validator.iter_errors(12))

    def test_unresolvable_nested_ref_is_not_wrapped_either(self):
        schema = {
            "$defs": {"inner": {"$ref": "https://example.com/missing.json"}},
            "$ref": "#/$defs/inner",
        }
        validator = Draft202012Validator(
            schema,
            registry=referencing.Registry(),
        )
        with self.assertRaises(referencing.exceptions.Unresolvable):
            list(validator.iter_errors(12))

    def test_ref_chain_appears_in_stringified_error(self):
        schema = {
            "$id": "https://example.com/root",
            "$ref": "a.json",
            "$defs": {"a": {"$id": "a.json", "type": "string"}},
        }
        error = next(Draft202012Validator(schema).iter_errors(12))
        shown = str(error)

        self.assertIn("Via reference chain", shown)
        self.assertIn("https://example.com/root", shown)
        self.assertIn("'a.json'", shown)
        self.assertIn("https://example.com/a.json", shown)

    def test_ref_chain_survives_create_from(self):
        schema = {"$ref": "#/$defs/a", "$defs": {"a": {"type": "string"}}}
        error = next(Draft202012Validator(schema).iter_errors(12))

        clone = exceptions.ValidationError.create_from(error)
        self.assertEqual(clone.ref_chain, error.ref_chain)

    def test_context_errors_carry_ref_chains(self):
        schema = {
            "anyOf": [
                {"$ref": "#/$defs/a"},
                {"$ref": "#/$defs/b"},
            ],
            "$defs": {
                "a": {"type": "string"},
                "b": {"type": "array"},
            },
        }
        error = next(Draft202012Validator(schema).iter_errors(12))
        self.assertEqual(error.validator, "anyOf")
        self.assertEqual(len(error.context), 2)
        for suberror, ref in zip(error.context, ["#/$defs/a", "#/$defs/b"]):
            self.assertEqual(len(suberror.ref_chain), 1)
            self.assertEqual(suberror.ref_chain[0].ref, ref)
