from __future__ import annotations

import unittest

import sqlglot
from sqlglot.impact_lineage import (
    ImpactLineage,
    ImpactNode,
    ImpactType,
    NodeKind,
    impact_lineage,
)
from sqlglot.lineage import lineage
from sqlglot.schema import MappingSchema


def edges(node: ImpactNode) -> dict[str, set[ImpactType]]:
    return {edge.node.name: set(edge.roles) for edge in node.downstream}


def leaf_names(node: ImpactNode, role: ImpactType) -> set[str]:
    return {
        edge.node.name
        for current in node.walk()
        for edge in current.downstream
        if role in edge.roles and not edge.node.downstream and edge.node.kind is NodeKind.COLUMN
    }


class TestImpactLineage(unittest.TestCase):
    maxDiff = None

    @classmethod
    def setUpClass(cls) -> None:
        sqlglot.schema = MappingSchema()

    SCHEMA = {
        "orders": {"id": "int", "cid": "int", "amt": "int"},
        "customers": {"id": "int", "name": "text", "region": "text"},
        "x": {"a": "int", "b": "int", "k": "int"},
        "y": {"a": "int", "b": "int", "k": "int"},
    }

    def test_value_vs_row_set_in_filtered_join(self) -> None:
        sql = """
        SELECT o.id, c.name
        FROM orders AS o
        JOIN customers AS c ON o.cid = c.id
        WHERE o.amt > 10 AND c.region = 'EU'
        """
        result = impact_lineage(None, sql, schema=self.SCHEMA)

        id_node = result["id"]
        self.assertEqual(edges(id_node)["o.id"], {ImpactType.VALUE})
        for filter_column in ("o.cid", "c.id", "o.amt", "c.region"):
            self.assertEqual(edges(id_node)[filter_column], {ImpactType.ROW_SET})

        name_node = result["name"]
        self.assertEqual(edges(name_node)["c.name"], {ImpactType.VALUE})
        self.assertEqual(edges(name_node)["c.region"], {ImpactType.ROW_SET})

    def test_same_column_can_have_both_roles(self) -> None:
        node = impact_lineage("a", "SELECT a FROM x WHERE a > 1", schema=self.SCHEMA)
        edge = next(edge for edge in node.downstream if edge.node.name == "x.a")
        self.assertEqual(edge.roles, {ImpactType.VALUE, ImpactType.ROW_SET})

    def test_group_by_and_having_are_row_set(self) -> None:
        sql = """
        SELECT c.region, COUNT(*) AS cnt, SUM(o.amt) AS total
        FROM orders AS o
        JOIN customers AS c ON o.cid = c.id
        GROUP BY c.region
        HAVING COUNT(*) > 1
        """
        result = impact_lineage(None, sql, schema=self.SCHEMA)

        region = result["region"]
        self.assertEqual(
            edges(region)["c.region"], {ImpactType.VALUE, ImpactType.ROW_SET}
        )

        total = result["total"]
        self.assertEqual(edges(total)["o.amt"], {ImpactType.VALUE})
        # join keys never enter a projected value
        self.assertEqual(edges(total)["o.cid"], {ImpactType.ROW_SET})
        self.assertEqual(edges(total)["c.id"], {ImpactType.ROW_SET})

    def test_count_star_keeps_row_dependency_without_value_column(self) -> None:
        node = impact_lineage(
            "cnt", "SELECT COUNT(*) AS cnt FROM x WHERE b > 1", schema=self.SCHEMA
        )
        row_nodes = [
            edge.node
            for edge in node.downstream
            if edge.node.kind is NodeKind.ROWS
        ]
        self.assertEqual([n.name for n in row_nodes], ["x.*"])
        self.assertEqual(row_nodes[0].downstream, [])
        self.assertEqual(edges(node)["x.b"], {ImpactType.ROW_SET})

        # COUNT(1) has no column either and must still depend on the input rows
        node = impact_lineage("cnt", "SELECT COUNT(1) AS cnt FROM x", schema=self.SCHEMA)
        self.assertTrue(
            any(edge.node.kind is NodeKind.ROWS for edge in node.downstream)
        )

    def test_count_star_through_derived_table(self) -> None:
        node = impact_lineage(
            "cnt",
            "SELECT COUNT(*) AS cnt FROM (SELECT a, b FROM x) AS t",
            schema=self.SCHEMA,
        )
        row_nodes = [n for n in node.walk() if n.kind is NodeKind.ROWS]
        self.assertTrue(row_nodes)

    def test_exists_correlated_subquery_preserves_outer_scope(self) -> None:
        sql = """
        SELECT o.id
        FROM orders AS o
        WHERE EXISTS (
            SELECT 1 FROM customers AS c
            WHERE c.id = o.cid AND c.region = 'EU'
        )
        """
        node = impact_lineage("id", sql, schema=self.SCHEMA)
        self.assertEqual(edges(node)["o.id"], {ImpactType.VALUE})

        exists_node = next(
            edge.node for edge in node.downstream if edge.node.name == "1"
        )
        exists_edges = edges(exists_node)
        self.assertEqual(exists_edges["o.cid"], {ImpactType.ROW_SET})
        self.assertEqual(exists_edges["c.id"], {ImpactType.ROW_SET})
        self.assertEqual(exists_edges["c.region"], {ImpactType.ROW_SET})
        # an EXISTS predicate produces no value dependency
        roles = {
            role
            for current in exists_node.walk()
            for edge in current.downstream
            for role in edge.roles
        }
        self.assertNotIn(ImpactType.VALUE, roles)

    def test_in_subquery_keeps_inner_value_role(self) -> None:
        schema = {
            **self.SCHEMA,
            "customers": {"id": "int", "region": "text", "amt": "int"},
        }
        sql = """
        SELECT id FROM orders
        WHERE amt IN (SELECT amt FROM customers WHERE customers.id = orders.id)
        """
        node = impact_lineage("id", sql, schema=schema)
        sub_node = next(edge.node for edge in node.downstream if edge.node.name == "amt")
        sub_edges = edges(sub_node)
        self.assertEqual(sub_edges["customers.amt"], {ImpactType.VALUE})
        self.assertEqual(sub_edges["orders.id"], {ImpactType.ROW_SET})

    def test_set_operation_traces_every_branch(self) -> None:
        sql = """
        WITH u AS (
            SELECT a FROM x WHERE k > 0
            UNION ALL
            SELECT a FROM y WHERE k > 0
        )
        SELECT a FROM u
        """
        node = impact_lineage("a", sql, schema=self.SCHEMA)
        self.assertEqual(leaf_names(node, ImpactType.VALUE), {"x.a", "y.a"})
        self.assertEqual(leaf_names(node, ImpactType.ROW_SET), {"x.k", "y.k"})

    def test_correlated_reference_across_set_operation_branches(self) -> None:
        sql = """
        SELECT t.a, (
            SELECT max(v) FROM (
                SELECT x.a AS v FROM x WHERE x.k = t.a
                UNION ALL
                SELECT y.a FROM y WHERE y.k = t.a
            )
        ) AS s
        FROM (SELECT a FROM x) AS t
        """
        result = impact_lineage(None, sql, schema=self.SCHEMA)
        scalar = result["s"]
        row_set_leaves = leaf_names(scalar, ImpactType.ROW_SET)
        self.assertIn("x.k", row_set_leaves)
        self.assertIn("y.k", row_set_leaves)
        # the correlated outer column t.a resolves through the derived table t to its source
        correlated = [
            edge.node
            for current in scalar.walk()
            for edge in current.downstream
            if edge.node.name == "t.a" and ImpactType.ROW_SET in edge.roles
        ]
        self.assertTrue(correlated)
        self.assertEqual(
            leaf_names(correlated[0], ImpactType.VALUE), {"x.a"}
        )

    def test_unknown_dependencies_are_marked_not_guessed(self) -> None:
        # unqualified columns with two sources cannot be attributed deterministically
        node = impact_lineage(
            "a",
            "SELECT a FROM x JOIN y ON x.k = y.k WHERE b > 1",
            schema={},
        )
        unknown = {n.name: n for n in node.walk() if n.kind is NodeKind.UNKNOWN}
        self.assertIn("a", unknown)
        self.assertIn("b", unknown)
        self.assertTrue(node.uncertain)

        uncertain = {
            edge.node.name
            for current in node.walk()
            for edge in current.downstream
            if edge.uncertain
        }
        self.assertEqual(uncertain, {"a", "b"})

        # qualified join columns are still definite edges
        join_edges = {edge.node.name: edge for edge in node.downstream}
        self.assertFalse(join_edges["x.k"].uncertain)
        self.assertFalse(join_edges["y.k"].uncertain)

    def test_single_source_unqualified_column_is_definite(self) -> None:
        node = impact_lineage("a", "SELECT a FROM x WHERE b > 1", schema={})
        self.assertFalse(node.uncertain)
        self.assertEqual(edges(node)["x.a"], {ImpactType.VALUE})
        self.assertEqual(edges(node)["x.b"], {ImpactType.ROW_SET})

    def test_star_without_schema_is_unknown(self) -> None:
        result = impact_lineage(None, "SELECT * FROM z WHERE q > 1", schema={})
        node = result["*"]
        unknown = [n for n in node.walk() if n.kind is NodeKind.UNKNOWN]
        self.assertEqual([n.name for n in unknown], ["z.*"])
        self.assertTrue(node.uncertain)

    def test_star_with_schema_expands_to_columns(self) -> None:
        node = impact_lineage("a", "SELECT * FROM x WHERE b > 1", schema=self.SCHEMA)
        self.assertEqual(edges(node)["x.a"], {ImpactType.VALUE})
        self.assertEqual(edges(node)["x.b"], {ImpactType.ROW_SET})

    def test_distinct_adds_row_set_role_to_projections(self) -> None:
        node = impact_lineage(
            "a", "SELECT DISTINCT a, b FROM x WHERE k > 1", schema=self.SCHEMA
        )
        self.assertEqual(edges(node)["x.a"], {ImpactType.VALUE, ImpactType.ROW_SET})
        self.assertEqual(edges(node)["x.b"], {ImpactType.VALUE, ImpactType.ROW_SET})
        self.assertEqual(edges(node)["x.k"], {ImpactType.ROW_SET})

    def test_aggregate_filter_clause_is_row_set_only(self) -> None:
        node = impact_lineage(
            "s",
            "SELECT SUM(a) FILTER (WHERE b > 1) AS s FROM x",
            schema=self.SCHEMA,
        )
        self.assertEqual(edges(node)["x.a"], {ImpactType.VALUE})
        self.assertEqual(edges(node)["x.b"], {ImpactType.ROW_SET})

    def test_literal_projection_without_from_has_no_dependencies(self) -> None:
        node = impact_lineage("x", "SELECT 1 AS x", schema={})
        self.assertEqual(node.downstream, [])
        self.assertFalse(node.uncertain)

    def test_legacy_lineage_is_unchanged(self) -> None:
        sql = "SELECT a FROM x WHERE a > 1"
        legacy = lineage("a", sql, schema=self.SCHEMA)
        self.assertEqual(type(legacy).__name__, "Node")
        self.assertEqual([d.name for d in legacy.downstream], ["x.a"])
        self.assertFalse(hasattr(legacy, "roles"))

        impact = impact_lineage("a", sql, schema=self.SCHEMA)
        self.assertIsInstance(impact, ImpactNode)
        self.assertNotIsInstance(legacy, ImpactNode)

    def test_result_container_and_html(self) -> None:
        result = impact_lineage(
            None, "SELECT a, b FROM x WHERE k > 1", schema=self.SCHEMA
        )
        self.assertIsInstance(result, ImpactLineage)
        self.assertEqual(set(result), {"a", "b"})
        self.assertEqual(len(result), 2)
        self.assertEqual(result.unknowns, [])

        html = str(result["a"].to_html())
        self.assertIn("row_set", html)
        self.assertIn("value", html)

    def test_sources_mapping(self) -> None:
        node = impact_lineage(
            "a",
            "SELECT a FROM z",
            schema=self.SCHEMA,
            sources={"z": "SELECT a FROM x WHERE b > 1"},
        )
        self.assertEqual(leaf_names(node, ImpactType.VALUE), {"x.a"})
        self.assertEqual(leaf_names(node, ImpactType.ROW_SET), {"x.b"})

    def test_missing_column_raises(self) -> None:
        with self.assertRaises(sqlglot.errors.SqlglotError):
            impact_lineage("missing", "SELECT a FROM x", schema=self.SCHEMA)


if __name__ == "__main__":
    unittest.main()
