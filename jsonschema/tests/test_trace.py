from unittest import TestCase

from referencing import Registry
from referencing.jsonschema import DRAFT202012

from jsonschema._trace import (
    DynamicRefTrace,
    RefResolutionError,
    SchemaNode,
    collect,
)


def _registry(*pairs):
    return Registry().with_resources(
        [
            (uri, DRAFT202012.create_resource(schema))
            for uri, schema in pairs
        ],
    )


OUTER = {
    "$id": "https://example.com/outer",
    "$dynamicAnchor": "node",
    "type": "object",
    "properties": {
        "tree": {"$ref": "inner"},
        "leaf": {"$dynamicRef": "#node"},
    },
}

INNER = {
    "$id": "https://example.com/inner",
    "$dynamicAnchor": "node",
    "type": "object",
    "properties": {
        "child": {"$dynamicRef": "#node"},
        "name": {"type": "string"},
    },
}


class TestCollect(TestCase):
    def test_returns_root_node_and_traces(self):
        registry = _registry(
            ("https://example.com/outer", OUTER),
            ("https://example.com/inner", INNER),
        )
        root, traces = collect(OUTER, registry)

        self.assertIsInstance(root, SchemaNode)
        self.assertEqual(root.pointer, "")
        self.assertEqual(root.resource_uri, "https://example.com/outer")
        self.assertTrue(all(isinstance(t, DynamicRefTrace) for t in traces))
        self.assertTrue(traces)

    def test_dynamic_ref_adopts_outer_anchor_from_nested_scope(self):
        registry = _registry(
            ("https://example.com/outer", OUTER),
            ("https://example.com/inner", INNER),
        )
        _, traces = collect(OUTER, registry)

        inner_trace = next(
            trace
            for trace in traces
            if trace.static_target.uri == "https://example.com/inner"
        )
        self.assertEqual(
            inner_trace.adopted_resource_uri,
            "https://example.com/outer",
        )
        self.assertEqual(
            inner_trace.adopted_via,
            "https://example.com/outer",
        )
        self.assertTrue(inner_trace.adopted_dynamically)
        # The static target is the nested resource's own anchor.
        self.assertEqual(
            inner_trace.static_target.uri,
            "https://example.com/inner",
        )
        self.assertEqual(inner_trace.static_target.dynamic_anchor, "node")
        # Only outer-scope dynamic anchors are candidates; the static
        # resource is not replayed as a scope candidate.
        self.assertEqual(
            [c.resource_uri for c in inner_trace.candidates],
            ["https://example.com/outer"],
        )

    def test_outer_dynamic_ref_keeps_static_binding(self):
        registry = _registry(
            ("https://example.com/outer", OUTER),
            ("https://example.com/inner", INNER),
        )
        _, traces = collect(OUTER, registry)

        outer_trace = next(
            trace
            for trace in traces
            if trace.node.pointer == "/properties/leaf"
        )
        self.assertEqual(
            outer_trace.adopted_resource_uri,
            "https://example.com/outer",
        )
        self.assertIsNone(outer_trace.adopted_via)
        self.assertFalse(outer_trace.adopted_dynamically)
        self.assertEqual(outer_trace.candidates, ())

    def test_same_named_anchors_in_distinct_resources(self):
        first = {
            "$id": "https://example.com/r1",
            "$dynamicAnchor": "k",
            "type": "string",
        }
        second = {
            "$id": "https://example.com/r2",
            "$dynamicAnchor": "k",
            "type": "integer",
        }
        schema = {
            "$id": "https://example.com/main",
            "properties": {
                "one": {"$ref": "r1"},
                "two": {"$ref": "r2"},
            },
        }
        registry = _registry(
            ("https://example.com/r1", first),
            ("https://example.com/r2", second),
        )
        root, _ = collect(schema, registry)

        uris = {node.resource_uri for node in root.iter_nodes()}
        self.assertIn("https://example.com/r1", uris)
        self.assertIn("https://example.com/r2", uris)

        r1_nodes = [
            node
            for node in root.iter_nodes()
            if node.resource_uri == "https://example.com/r1"
        ]
        r2_nodes = [
            node
            for node in root.iter_nodes()
            if node.resource_uri == "https://example.com/r2"
        ]
        self.assertTrue(all(node.schema is first for node in r1_nodes))
        self.assertTrue(all(node.schema is second for node in r2_nodes))

    def test_sibling_does_not_inherit_previous_scope(self):
        nested = {
            "$id": "https://example.com/nested",
            "$dynamicAnchor": "a",
        }
        schema = {
            "$id": "https://example.com/root",
            "$dynamicAnchor": "a",
            "properties": {
                "first": {"$ref": "nested"},
                "second": {"$dynamicRef": "#a"},
            },
        }
        registry = _registry(("https://example.com/nested", nested))
        _, traces = collect(schema, registry)

        second = next(
            trace
            for trace in traces
            if trace.node.pointer == "/properties/second"
        )
        self.assertEqual(
            second.adopted_resource_uri,
            "https://example.com/root",
        )
        self.assertIsNone(second.adopted_via)
        self.assertEqual(second.candidates, ())

    def test_scope_restored_after_leaving_reference(self):
        registry = _registry(
            ("https://example.com/outer", OUTER),
            ("https://example.com/inner", INNER),
        )
        root, _ = collect(OUTER, registry)

        leaf = next(
            node
            for node in root.iter_nodes()
            if node.pointer == "/properties/leaf"
        )
        self.assertEqual(leaf.resource_uri, "https://example.com/outer")

    def test_plain_anchor_is_not_a_dynamic_candidate(self):
        schema = {
            "$id": "https://example.com/mixed",
            "$defs": {"a": {"$anchor": "thing", "type": "string"}},
            "properties": {"x": {"$dynamicRef": "#thing"}},
        }
        _, traces = collect(schema)
        trace = traces[0]
        self.assertIsNone(trace.adopted_via)
        self.assertEqual(trace.candidates, ())
        self.assertIsNone(trace.static_target.dynamic_anchor)

    def test_pointer_fragment_is_not_anchor_lookup(self):
        schema = {
            "$id": "https://example.com/ptr",
            "$defs": {"a": {"type": "integer"}},
            "properties": {"x": {"$dynamicRef": "#/$defs/a"}},
        }
        _, traces = collect(schema)
        trace = traces[0]
        self.assertIsNone(trace.anchor)
        self.assertEqual(trace.static_target.pointer, "/$defs/a")
        self.assertEqual(trace.candidates, ())
        self.assertIsNone(trace.adopted_via)

    def test_unregistered_reference_raises_with_location(self):
        schema = {
            "$id": "https://example.com/broken",
            "properties": {"x": {"$ref": "missing"}},
        }
        with self.assertRaises(RefResolutionError) as caught:
            collect(schema)
        self.assertEqual(caught.exception.ref, "missing")
        self.assertEqual(
            caught.exception.node.pointer,
            "/properties/x",
        )

    def test_recursive_reference_is_a_back_edge(self):
        schema = {
            "$id": "https://example.com/rec",
            "type": "object",
            "properties": {"next": {"$ref": "#"}},
        }
        root, _ = collect(schema)
        back_edges = [node for node in root.iter_nodes() if node.back_edge]
        self.assertTrue(back_edges)
        self.assertTrue(all(node.ref == "#" for node in back_edges))

    def test_trace_refs_false_builds_tree_without_traces(self):
        registry = _registry(
            ("https://example.com/outer", OUTER),
            ("https://example.com/inner", INNER),
        )
        root, traces = collect(OUTER, registry, trace_refs=False)
        self.assertEqual(traces, ())
        self.assertTrue(
            any(node.schema is INNER for node in root.iter_nodes()),
        )

    def test_tree_links_parents_and_children(self):
        registry = _registry(
            ("https://example.com/outer", OUTER),
            ("https://example.com/inner", INNER),
        )
        root, _ = collect(OUTER, registry)
        for node in root.iter_nodes():
            if node is root:
                self.assertIsNone(node.parent)
            else:
                self.assertIsNotNone(node.parent)
                self.assertIn(node, node.parent.children)

    def test_boolean_schemas_are_collected(self):
        schema = {
            "$id": "https://example.com/bools",
            "properties": {"ok": True, "no": False},
            "allOf": [True],
        }
        root, _ = collect(schema)
        pointers = {node.pointer for node in root.iter_nodes()}
        self.assertIn("/properties/ok", pointers)
        self.assertIn("/properties/no", pointers)
        self.assertIn("/allOf/0", pointers)

    def test_root_without_id_uses_empty_base_uri(self):
        schema = {
            "$defs": {"a": {"$dynamicAnchor": "x"}},
            "properties": {"p": {"$dynamicRef": "#x"}},
        }
        root, traces = collect(schema)
        self.assertEqual(root.resource_uri, "")
        self.assertEqual(traces[0].static_target.uri, "")
        self.assertEqual(traces[0].adopted_resource_uri, "")

    def test_entry_path_controls_dynamic_scope(self):
        schema = {
            "$id": "https://ex/root",
            "$dynamicAnchor": "meta",
            "$ref": "extended",
            "$defs": {
                "extended": {
                    "$id": "extended",
                    "$dynamicAnchor": "meta",
                    "properties": {"bar": {"$ref": "bar"}},
                },
                "bar": {
                    "$id": "bar",
                    "properties": {
                        "baz": {"$dynamicRef": "extended#meta"},
                    },
                },
            },
        }
        _, traces = collect(schema)
        by_pointer = {trace.node.pointer: trace for trace in traces}

        # Reached purely syntactically: root's reference scope is absent.
        syntactic = by_pointer[
            "/$defs/extended/properties/bar/properties/baz"
        ]
        self.assertEqual(
            [c.resource_uri for c in syntactic.candidates],
            ["https://ex/extended"],
        )
        self.assertEqual(
            syntactic.adopted_resource_uri,
            "https://ex/extended",
        )

        # Reached through the root $ref: root stays in the dynamic scope.
        referenced = by_pointer["/properties/bar/properties/baz"]
        self.assertIn(
            "https://ex/root",
            [c.resource_uri for c in referenced.candidates],
        )
        self.assertEqual(
            referenced.adopted_resource_uri,
            "https://ex/root",
        )
        self.assertIsNot(syntactic.node, referenced.node)
