import unittest

from sqlglot import parse_one
from sqlglot.errors import AmbiguousTableMappingError
from sqlglot.optimizer.scope import rewrite_table_references


class TestTableRewrite(unittest.TestCase):
    def test_rewrites_only_matching_qualification(self):
        sql = "SELECT * FROM orders"
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )
        self.assertEqual(result.sql, "SELECT * FROM orders")
        self.assertEqual(result.changes, [])

        result = rewrite_table_references(
            "SELECT * FROM old_catalog.old_schema.orders",
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )
        self.assertEqual(result.sql, "SELECT * FROM new_catalog.new_schema.orders")
        self.assertEqual(len(result.changes), 1)

        result = rewrite_table_references(
            "SELECT * FROM old_schema.orders",
            {"orders": "new_orders"},
            dialect="postgres",
        )
        self.assertEqual(result.sql, "SELECT * FROM old_schema.orders")
        self.assertEqual(result.changes, [])

    def test_no_match_preserves_the_original_sql_text(self):
        sql = "select  *\nfrom legacy_orders"
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )
        self.assertEqual(result.sql, sql)
        self.assertEqual(result.changes, [])

    def test_cte_derived_table_and_column_names_are_not_rewritten(self):
        sql = """
        WITH orders AS (
          SELECT order_id FROM old_catalog.old_schema.orders
        )
        SELECT orders.order_id
        FROM orders
        JOIN (SELECT order_id FROM old_catalog.old_schema.orders) AS derived_orders
          ON orders.order_id = derived_orders.order_id
        """
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )
        self.assertNotIn("new_catalog.new_schema.orders AS", result.sql)
        self.assertIn("FROM new_catalog.new_schema.orders", result.sql)
        self.assertIn("JOIN (SELECT order_id FROM new_catalog.new_schema.orders)", result.sql)
        self.assertIn("SELECT orders.order_id", result.sql)
        self.assertEqual(len(result.changes), 2)

    def test_recursive_cte_self_reference_is_not_rewritten(self):
        sql = """
        WITH RECURSIVE walk AS (
          SELECT id FROM old_catalog.old_schema.nodes
          UNION ALL
          SELECT n.id FROM walk AS n
        )
        SELECT * FROM walk
        """
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "nodes"): ("new_catalog", "new_schema", "nodes")},
            dialect="postgres",
        )
        self.assertIn("FROM new_catalog.new_schema.nodes", result.sql)
        self.assertIn("FROM walk AS n", result.sql)
        self.assertEqual(len(result.changes), 1)

    def test_local_shadowing_uses_the_innermost_source(self):
        sql = """
        WITH items AS (SELECT * FROM old_catalog.old_schema.items)
        SELECT *
        FROM (
          WITH items AS (SELECT * FROM old_catalog.old_schema.items)
          SELECT * FROM items
        ) AS local_items
        """
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "items"): ("new_catalog", "new_schema", "items")},
            dialect="postgres",
        )
        self.assertEqual(result.sql.count("new_catalog.new_schema.items"), 2)
        self.assertEqual(result.sql.count("SELECT * FROM items"), 1)
        self.assertEqual(len(result.changes), 2)

    def test_physical_alias_shadows_an_outer_cte(self):
        sql = """
        WITH items AS (SELECT * FROM old_catalog.old_schema.items)
        SELECT *
        FROM (
          SELECT * FROM old_catalog.old_schema.items AS items
        ) AS local_items
        """
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "items"): ("new_catalog", "new_schema", "items")},
            dialect="postgres",
        )
        self.assertEqual(result.sql.count("new_catalog.new_schema.items"), 2)
        self.assertIn("new_catalog.new_schema.items AS items", result.sql)
        self.assertEqual(len(result.changes), 2)

    def test_qualified_table_is_not_shadowed_by_a_derived_alias(self):
        result = rewrite_table_references(
            "SELECT * FROM (SELECT 1 AS id) AS t JOIN old_catalog.old_schema.t ON 1 = 1",
            {("old_catalog", "old_schema", "t"): ("new_catalog", "new_schema", "t2")},
            dialect="postgres",
        )
        self.assertIn("JOIN new_catalog.new_schema.t2", result.sql)
        self.assertEqual(len(result.changes), 1)

    def test_quoted_identifiers_follow_the_dialect(self):
        result = rewrite_table_references(
            'SELECT * FROM "Old"."Schema"."Orders"',
            {'"Old"."Schema"."Orders"': "`New`.`Schema`.`Orders`"},
            dialect="bigquery",
        )
        self.assertEqual(result.sql, "SELECT * FROM `New`.`Schema`.`Orders`")

        result = rewrite_table_references(
            "SELECT * FROM [Old].[Schema].[Orders]",
            {"[Old].[Schema].[Orders]": "[New].[Schema].[Orders]"},
            dialect="tsql",
        )
        self.assertEqual(result.sql, "SELECT * FROM [New].[Schema].[Orders]")

    def test_qualified_columns_bound_to_the_table_are_rewritten(self):
        result = rewrite_table_references(
            "SELECT old_catalog.old_schema.orders.id, orders FROM old_catalog.old_schema.orders",
            {
                ("old_catalog", "old_schema", "orders"): (
                    "new_catalog",
                    "new_schema",
                    "orders_v2",
                )
            },
            dialect="postgres",
        )
        self.assertEqual(
            result.sql,
            "SELECT new_catalog.new_schema.orders_v2.id, orders "
            "FROM new_catalog.new_schema.orders_v2",
        )

        again = rewrite_table_references(
            result,
            {
                ("old_catalog", "old_schema", "orders"): (
                    "new_catalog",
                    "new_schema",
                    "orders_v2",
                )
            },
            dialect="postgres",
        )
        self.assertEqual(again.sql, result.sql)
        self.assertEqual(again.changes, [])

    def test_alias_and_cte_column_qualifiers_are_not_rewritten(self):
        sql = """
        WITH recent AS (SELECT * FROM old_catalog.old_schema.orders)
        SELECT recent.id, o.id
        FROM recent
        JOIN old_catalog.old_schema.orders AS o ON recent.id = o.id
        """
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders_v2")},
            dialect="postgres",
        )
        self.assertIn("SELECT recent.id, o.id", result.sql)
        self.assertIn("JOIN new_catalog.new_schema.orders_v2 AS o", result.sql)
        self.assertEqual(len(result.changes), 2)

    def test_correlated_qualified_column_is_rewritten_despite_local_alias(self):
        sql = """
        SELECT *
        FROM old_catalog.old_schema.orders
        WHERE EXISTS (
          SELECT 1
          FROM (SELECT 1 AS id) AS orders
          WHERE old_catalog.old_schema.orders.id = orders.id
        )
        """
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders_v2")},
            dialect="postgres",
        )
        self.assertIn("new_catalog.new_schema.orders_v2.id = orders.id", result.sql)
        self.assertEqual(len(result.changes), 1)

    def test_target_collision_is_reported_before_mutation(self):
        expression = parse_one(
            "SELECT * FROM old_a.old_schema.t JOIN old_b.old_schema.t ON 1 = 1"
        )
        original = expression.sql()
        with self.assertRaises(AmbiguousTableMappingError):
            rewrite_table_references(
                expression,
                {
                    ("old_a", "old_schema", "t"): ("new_catalog", "new_schema", "t"),
                    ("old_b", "old_schema", "t"): ("new_catalog", "new_schema", "t"),
                },
                dialect="postgres",
                copy=False,
            )
        self.assertEqual(expression.sql(), original)

    def test_existing_aliases_disambiguate_the_same_target(self):
        result = rewrite_table_references(
            "SELECT * FROM old_a.old_schema.t AS a JOIN old_b.old_schema.t AS b ON a.id = b.id",
            {
                ("old_a", "old_schema", "t"): ("new_catalog", "new_schema", "t"),
                ("old_b", "old_schema", "t"): ("new_catalog", "new_schema", "t"),
            },
            dialect="postgres",
        )
        self.assertEqual(
            result.sql,
            "SELECT * FROM new_catalog.new_schema.t AS a "
            "JOIN new_catalog.new_schema.t AS b ON a.id = b.id",
        )
        self.assertEqual(len(result.changes), 2)

    def test_colliding_target_table_names_require_existing_aliases(self):
        with self.assertRaises(AmbiguousTableMappingError):
            rewrite_table_references(
                "SELECT * FROM old_a.old_schema.t JOIN old_b.old_schema.t ON 1 = 1",
                {
                    ("old_a", "old_schema", "t"): ("new_a", "new_schema", "t"),
                    ("old_b", "old_schema", "t"): ("new_b", "new_schema", "t"),
                },
                dialect="postgres",
            )

    def test_unqualified_target_matching_a_cte_is_reported(self):
        with self.assertRaises(AmbiguousTableMappingError):
            rewrite_table_references(
                "WITH new_items AS (SELECT 1) SELECT * FROM old_catalog.old_schema.items",
                {("old_catalog", "old_schema", "items"): "new_items"},
                dialect="postgres",
            )

    def test_unqualified_target_matching_an_aliased_derived_table_is_allowed(self):
        result = rewrite_table_references(
            "SELECT * FROM (SELECT 1 AS id) AS new_items "
            "JOIN old_catalog.old_schema.items AS item ON 1 = 1",
            {("old_catalog", "old_schema", "items"): "new_items"},
            dialect="postgres",
        )
        self.assertIn("JOIN new_items AS item", result.sql)
        self.assertEqual(len(result.changes), 1)

    def test_chained_mapping_is_reported(self):
        with self.assertRaises(AmbiguousTableMappingError):
            rewrite_table_references(
                "SELECT * FROM old_items",
                {"old_items": "items", "items": "new_items"},
                dialect="postgres",
            )

    def test_returns_positions_and_is_idempotent(self):
        sql = "SELECT *\nFROM old_catalog.old_schema.orders"
        result = rewrite_table_references(
            sql,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )

        change = result.changes[0]
        self.assertEqual(change.old, "old_catalog.old_schema.orders")
        self.assertEqual(change.new, "new_catalog.new_schema.orders")
        self.assertEqual(change.line, 2)
        self.assertEqual(change.column, 6)
        self.assertEqual(change.start, 14)
        self.assertEqual(change.path, (("from_", None), ("this", None)))

        again = rewrite_table_references(
            str(result.sql),
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )
        self.assertEqual(again.sql, result.sql)
        self.assertEqual(again.changes, [])

    def test_ast_copy_is_preserved_by_default(self):
        expression = parse_one("SELECT * FROM old_catalog.old_schema.orders")
        result = rewrite_table_references(
            expression,
            {("old_catalog", "old_schema", "orders"): ("new_catalog", "new_schema", "orders")},
            dialect="postgres",
        )
        self.assertEqual(expression.sql(), "SELECT * FROM old_catalog.old_schema.orders")
        self.assertEqual(result.sql, "SELECT * FROM new_catalog.new_schema.orders")

    def test_nested_mapping_and_dml_target(self):
        mapping = {
            "old_catalog": {
                "old_schema": {
                    "orders": {"catalog": "new_catalog", "schema": "new_schema", "table": "orders"}
                }
            }
        }
        result = rewrite_table_references(
            "INSERT INTO old_catalog.old_schema.orders SELECT * FROM old_catalog.old_schema.orders",
            mapping,
            dialect="postgres",
        )
        self.assertEqual(
            result.sql,
            "INSERT INTO new_catalog.new_schema.orders SELECT * FROM new_catalog.new_schema.orders",
        )
        self.assertEqual(len(result.changes), 2)


if __name__ == "__main__":
    unittest.main()
