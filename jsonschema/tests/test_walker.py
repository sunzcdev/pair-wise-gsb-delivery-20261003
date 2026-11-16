from __future__ import annotations

from unittest import TestCase

import referencing
import referencing.jsonschema

from jsonschema import walker
from jsonschema.validators import (
    Draft201909Validator,
    Draft202012Validator,
    Draft3Validator,
    Draft4Validator,
    Draft6Validator,
    Draft7Validator,
)


def nodes(schema, **kwargs):
    return list(walker.walk(schema, **kwargs))


def pointers(schema, **kwargs):
    return [(each.pointer, each.schema) for each in nodes(schema, **kwargs)]


class TestWalkerBasics(TestCase):
    def test_root_node_is_yielded_first(self):
        schema = {"type": "object"}
        result = nodes(schema)
        self.assertEqual(result[0].schema, schema)
        self.assertEqual(result[0].pointer, "")
        self.assertTrue(result[0].is_resource_root)
        self.assertIs(result[0].validator, Draft202012Validator)

    def test_walker_can_be_iterated_more_than_once(self):
        schema = {"type": "object", "not": False}
        w = walker.walk(schema)
        first = [each.pointer for each in w]
        second = [each.pointer for each in w]
        self.assertEqual(first, second)
        self.assertEqual(first, ["", "/not"])

    def test_default_examples_and_enum_dicts_are_not_contracts(self):
        schema = {
            "type": "object",
            "properties": {
                "name": {
                    "type": "string",
                    "default": {"$ref": "#/not-a-contract"},
                    "examples": [{"$ref": "#/also-not"}],
                },
                "choice": {
                    "enum": [
                        {"$ref": "#/neither"},
                        {"another": "plain object"},
                    ],
                },
                "const": {"$ref": "#/nope"},
            },
        }
        result = nodes(schema)
        # Only the root and the three property subschemas are visited.
        self.assertEqual(
            sorted(each.pointer for each in result),
            ["", "/properties/choice", "/properties/const",
             "/properties/name"],
        )

    def test_boolean_schemas_are_yielded_as_nodes(self):
        schema = {
            "type": "object",
            "properties": {"allowed": True, "forbidden": False},
            "not": False,
            "allOf": [True, False],
        }
        result = {each.pointer: each.schema for each in nodes(schema)}
        self.assertIs(result["/properties/allowed"], True)
        self.assertIs(result["/properties/forbidden"], False)
        self.assertIs(result["/not"], False)
        self.assertIs(result["/allOf/0"], True)
        self.assertIs(result["/allOf/1"], False)

    def test_boolean_root_schemas(self):
        for root in (True, False):
            result = nodes(root)
            self.assertEqual(len(result), 1)
            self.assertIs(result[0].schema, root)
            self.assertEqual(result[0].pointer, "")
            self.assertTrue(result[0].is_resource_root)

    def test_pointer_escaping(self):
        schema = {
            "properties": {
                "a/b": {"type": "string"},
                "c~d": {"type": "integer"},
            },
        }
        result = {each.pointer for each in nodes(schema)}
        self.assertIn("/properties/a~1b", result)
        self.assertIn("/properties/c~0d", result)

    def test_absolute_uri(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {"x": {"type": "string"}},
        }
        result = {each.pointer: each.absolute_uri for each in nodes(schema)}
        self.assertEqual(result[""], "https://example.com/root")
        self.assertEqual(
            result["/properties/x"],
            "https://example.com/root#/properties/x",
        )


class TestDialectApplicators(TestCase):
    def test_draft202012_prefix_items_and_items(self):
        schema = {
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "prefixItems": [{"type": "string"}, False],
            "items": {"type": "number"},
            "contains": {"type": "boolean"},
            "dependentSchemas": {"a": {"type": "object"}},
            "unevaluatedItems": True,
            "unevaluatedProperties": False,
        }
        result = dict(pointers(schema))
        self.assertEqual(result["/prefixItems/0"], {"type": "string"})
        self.assertIs(result["/prefixItems/1"], False)
        self.assertEqual(result["/items"], {"type": "number"})
        self.assertEqual(result["/contains"], {"type": "boolean"})
        self.assertEqual(result["/dependentSchemas/a"], {"type": "object"})
        self.assertIs(result["/unevaluatedItems"], True)
        self.assertIs(result["/unevaluatedProperties"], False)
        self.assertTrue(
            all(
                each.validator is Draft202012Validator
                for each in nodes(schema)
            ),
        )

    def test_draft201909_tuple_items_without_prefix_items(self):
        schema = {
            "$schema": "https://json-schema.org/draft/2019-09/schema",
            "items": [{"type": "string"}, {"type": "number"}],
            "additionalItems": False,
        }
        result = dict(pointers(schema))
        self.assertEqual(result["/items/0"], {"type": "string"})
        self.assertEqual(result["/items/1"], {"type": "number"})
        self.assertIs(result["/additionalItems"], False)
        self.assertTrue(
            all(
                each.validator is Draft201909Validator
                for each in nodes(schema)
            ),
        )

    def test_draft7_dependencies_mixed_form(self):
        schema = {
            "$schema": "http://json-schema.org/draft-07/schema#",
            "dependencies": {
                "names": ["a", "b"],
                "subschema": {"type": "object"},
            },
            "items": [{"type": "string"}],
            "additionalItems": {"type": "boolean"},
        }
        result = dict(pointers(schema))
        self.assertEqual(result["/dependencies/subschema"], {"type": "object"})
        self.assertNotIn("/dependencies/names", result)
        self.assertEqual(result["/items/0"], {"type": "string"})
        self.assertEqual(
            result["/additionalItems"],
            {"type": "boolean"},
        )
        self.assertTrue(
            all(each.validator is Draft7Validator for each in nodes(schema)),
        )

    def test_draft6_dependencies_and_items(self):
        schema = {
            "$schema": "http://json-schema.org/draft-06/schema#",
            "dependencies": {"a": ["b"], "c": {"not": True}},
            "items": {"type": "string"},
            "propertyNames": {"type": "string"},
        }
        result = dict(pointers(schema))
        self.assertEqual(result["/dependencies/c"], {"not": True})
        self.assertNotIn("/dependencies/a", result)
        self.assertEqual(result["/items"], {"type": "string"})
        self.assertEqual(result["/propertyNames"], {"type": "string"})
        self.assertTrue(
            all(each.validator is Draft6Validator for each in nodes(schema)),
        )

    def test_draft4_dependencies_mixed_form(self):
        schema = {
            "$schema": "http://json-schema.org/draft-04/schema#",
            "dependencies": {
                "array": ["x"],
                "object": {"properties": {"y": {"type": "null"}}},
            },
            "items": [{"type": "string"}],
            "not": {"type": "integer"},
        }
        result = dict(pointers(schema))
        self.assertEqual(
            result["/dependencies/object/properties/y"],
            {"type": "null"},
        )
        self.assertNotIn("/dependencies/array", result)
        self.assertEqual(result["/items/0"], {"type": "string"})
        self.assertEqual(result["/not"], {"type": "integer"})
        self.assertTrue(
            all(each.validator is Draft4Validator for each in nodes(schema)),
        )

    def test_content_schema_is_a_subschema_location(self):
        for dialect in (
            "https://json-schema.org/draft/2020-12/schema",
            "https://json-schema.org/draft/2019-09/schema",
        ):
            schema = {
                "$schema": dialect,
                "contentSchema": {"type": "string"},
            }
            result = dict(pointers(schema))
            self.assertEqual(result["/contentSchema"], {"type": "string"})

    def test_draft3_special_keywords(self):
        schema = {
            "$schema": "http://json-schema.org/draft-03/schema#",
            "type": [{"name": "string"}, "integer"],
            "disallow": [{"type": "boolean"}, "null"],
            "extends": [{"type": "string"}, {"type": "number"}],
            "dependencies": {
                "string_dep": "other",
                "array_dep": ["a"],
                "schema_dep": {"type": "integer"},
            },
            "items": [{"type": "string"}],
            "additionalItems": True,
        }
        result = dict(pointers(schema))
        self.assertEqual(result["/type/0"], {"name": "string"})
        self.assertEqual(result["/disallow/0"], {"type": "boolean"})
        self.assertEqual(result["/extends/0"], {"type": "string"})
        self.assertEqual(result["/extends/1"], {"type": "number"})
        self.assertEqual(
            result["/dependencies/schema_dep"],
            {"type": "integer"},
        )
        self.assertNotIn("/dependencies/string_dep", result)
        self.assertNotIn("/dependencies/array_dep", result)
        self.assertIs(result["/additionalItems"], True)
        self.assertTrue(
            all(each.validator is Draft3Validator for each in nodes(schema)),
        )


class TestResourceBoundaries(TestCase):
    def test_modern_dollar_id_boundary(self):
        schema = {
            "$id": "https://example.com/root",
            "$defs": {
                "child": {"$id": "child.json", "type": "string"},
            },
        }
        result = {each.pointer: each for each in nodes(schema)}
        child = result[""]
        embedded = next(
            each for each in nodes(schema) if each.uri.endswith("child.json")
        )
        self.assertTrue(embedded.is_resource_root)
        self.assertEqual(embedded.pointer, "")
        self.assertEqual(embedded.uri, "https://example.com/child.json")
        self.assertIsNot(child, embedded)

    def test_legacy_id_boundary(self):
        schema = {
            "$schema": "http://json-schema.org/draft-04/schema#",
            "id": "https://example.com/root",
            "definitions": {
                "child": {"id": "child.json", "type": "string"},
            },
        }
        embedded = next(
            each for each in nodes(schema) if each.uri.endswith("child.json")
        )
        self.assertTrue(embedded.is_resource_root)
        self.assertEqual(embedded.pointer, "")

    def test_draft3_fragment_only_id_is_not_a_boundary(self):
        schema = {
            "$schema": "http://json-schema.org/draft-03/schema#",
            "id": "https://example.com/root",
            "properties": {"x": {"id": "#anchor", "type": "string"}},
        }
        uris = {each.uri for each in nodes(schema)}
        self.assertEqual(uris, {"https://example.com/root"})
        prop = next(
            each for each in nodes(schema) if each.pointer == "/properties/x"
        )
        self.assertFalse(prop.is_resource_root)

    def test_dialect_switch_inside_embedded_resource(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {
                "legacy": {
                    "$schema": "http://json-schema.org/draft-07/schema#",
                    "$id": "legacy.json",
                    "dependencies": {"a": {"type": "string"}},
                },
            },
        }
        result = list(nodes(schema))
        legacy_nodes = [
            each for each in result if each.uri.endswith("legacy.json")
        ]
        self.assertTrue(legacy_nodes)
        self.assertTrue(
            all(each.validator is Draft7Validator for each in legacy_nodes),
        )
        dep = next(
            each
            for each in result
            if each.pointer == "/dependencies/a"
        )
        self.assertIs(dep.validator, Draft7Validator)


class TestReferences(TestCase):
    def _registry(self, uri, contents, specification):
        return referencing.Registry().with_resource(
            uri,
            specification.create_resource(contents),
        )

    def test_refs_are_not_followed_by_default(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {"a": {"$ref": "other.json"}},
        }
        w = walker.walk(schema)
        result = list(w)
        # The root, the ``properties`` object... only two schema locations
        # exist: the root itself and the ``$ref`` property subschema.
        self.assertEqual(len(result), 2)
        self.assertEqual(w.edges, ())
        self.assertEqual(w.back_edges, ())

    def test_follow_ref_into_registered_resource(self):
        root = {
            "$id": "https://example.com/root",
            "properties": {"a": {"$ref": "other.json"}},
        }
        other = {
            "$id": "https://example.com/other.json",
            "type": "object",
            "properties": {"k": {"type": "string"}},
        }
        registry = self._registry(
            "https://example.com/other.json",
            other,
            referencing.jsonschema.DRAFT202012,
        )
        w = walker.walk(root, registry=registry, follow_refs=True)
        list(w)
        self.assertEqual(len(w.edges), 1)
        edge = w.edges[0]
        self.assertEqual(edge.ref, "other.json")
        self.assertEqual(edge.target.uri, "https://example.com/other.json")
        self.assertFalse(edge.is_back_edge)
        uris = {each.uri for each in nodes(
            root, registry=registry, follow_refs=True,
        )}
        self.assertIn("https://example.com/other.json", uris)

    def test_pointer_fragment_ref(self):
        # The referenced location is positioned so DFS reaches it before the
        # reference itself, making the edge a back edge.
        root = {
            "$id": "https://example.com/root",
            "allOf": [
                {"$defs": {"y": {"type": "integer"}}},
                {"$ref": "#/allOf/0/$defs/y"},
            ],
        }
        w = walker.walk(root, follow_refs=True)
        list(w)
        edge = w.edges[0]
        self.assertEqual(edge.ref, "#/allOf/0/$defs/y")
        self.assertEqual(edge.target.uri, "https://example.com/root")
        self.assertEqual(edge.target.pointer, "/allOf/0/$defs/y")
        self.assertTrue(edge.is_back_edge)
        self.assertEqual(w.back_edges, (edge,))

    def test_anchor_fragment_ref(self):
        root = {
            "$id": "https://example.com/root",
            "allOf": [
                {"$defs": {"x": {"$anchor": "myAnchor", "type": "string"}}},
                {"$ref": "#myAnchor"},
            ],
        }
        w = walker.walk(root, follow_refs=True)
        list(w)
        edge = w.edges[0]
        self.assertEqual(edge.target.pointer, "/allOf/0/$defs/x")
        self.assertTrue(edge.is_back_edge)

    def test_recursive_resource_reports_back_edge_and_stops(self):
        root = {
            "$id": "https://example.com/root",
            "type": "object",
            "properties": {
                "a": {"$ref": "other.json"},
            },
        }
        other = {
            "$id": "https://example.com/other.json",
            "type": "object",
            "properties": {"back": {"$ref": "root"}},
        }
        registry = self._registry(
            "https://example.com/other.json",
            other,
            referencing.jsonschema.DRAFT202012,
        )
        w = walker.walk(root, registry=registry, follow_refs=True)
        result = list(w)
        back = w.back_edges
        self.assertEqual(len(back), 1)
        self.assertEqual(back[0].ref, "root")
        self.assertEqual(back[0].target.uri, "https://example.com/root")
        # Termination: a finite number of unique nodes was visited.
        identities = {(each.uri, each.pointer) for each in result}
        self.assertEqual(len(result), len(identities))

    def test_diamond_reference_does_not_reexpand(self):
        root = {
            "$id": "https://example.com/root",
            "allOf": [
                {"$ref": "other.json"},
                {"$ref": "other.json"},
            ],
        }
        other = {
            "$id": "https://example.com/other.json",
            "type": "string",
        }
        registry = self._registry(
            "https://example.com/other.json",
            other,
            referencing.jsonschema.DRAFT202012,
        )
        w = walker.walk(root, registry=registry, follow_refs=True)
        result = list(w)
        other_visits = [
            each for each in result
            if each.uri == "https://example.com/other.json"
        ]
        self.assertEqual(len(other_visits), 1)
        self.assertEqual(len(w.back_edges), 1)

    def test_unresolvable_ref_does_not_raise(self):
        schema = {
            "$id": "https://example.com/root",
            "properties": {"p": {"$ref": "missing.json#"}},
        }
        result = nodes(schema, follow_refs=True)
        # Root plus the single ``$ref`` property subschema.
        self.assertEqual(len(result), 2)

    def test_cross_dialect_ref_target_uses_target_validator(self):
        root = {
            "$id": "https://example.com/root",
            "properties": {"p": {"$ref": "legacy.json"}},
        }
        legacy = {
            "$schema": "http://json-schema.org/draft-07/schema#",
            "$id": "https://example.com/legacy.json",
            "dependencies": {"a": ["b"], "c": {"type": "string"}},
        }
        registry = self._registry(
            "https://example.com/legacy.json",
            legacy,
            referencing.jsonschema.DRAFT7,
        )
        result = nodes(root, registry=registry, follow_refs=True)
        legacy_nodes = [
            each
            for each in result
            if each.uri == "https://example.com/legacy.json"
        ]
        self.assertTrue(legacy_nodes)
        self.assertTrue(
            all(each.validator is Draft7Validator for each in legacy_nodes),
        )

    def test_dynamic_ref_cycle_reports_back_edge(self):
        root = {
            "$id": "https://example.com/root",
            "$defs": {
                "node": {
                    "$dynamicAnchor": "thing",
                    "type": "object",
                    "properties": {
                        "child": {"$dynamicRef": "#thing"},
                    },
                },
            },
            "properties": {"n": {"$ref": "#/$defs/node"}},
        }
        w = walker.walk(root, follow_refs=True)
        result = list(w)
        self.assertTrue(w.back_edges)
        dynamic = next(
            each for each in w.edges if each.ref == "#thing"
        )
        self.assertTrue(dynamic.is_back_edge)
        self.assertEqual(dynamic.target.pointer, "/$defs/node")
        # The walk terminated without expanding any node twice.
        identities = {(each.uri, each.pointer) for each in result}
        self.assertEqual(len(result), len(identities))

    def test_recursive_ref_is_recognized_in_2019_09(self):
        schema = {
            "$schema": "https://json-schema.org/draft/2019-09/schema",
            "$id": "https://example.com/root",
            "properties": {"p": {"$recursiveRef": "#"}},
        }
        w = walker.walk(schema, follow_refs=True)
        list(w)
        self.assertEqual(len(w.edges), 1)
        self.assertEqual(w.edges[0].ref, "#")
        self.assertTrue(w.edges[0].is_back_edge)
