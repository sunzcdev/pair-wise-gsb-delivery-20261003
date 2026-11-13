import unittest
from collections import Counter

import sqlglot
from sqlglot import exp
from sqlglot.expressions.builders import parameterize


class TestParameterizer(unittest.TestCase):
    def test_named_values_are_typed(self):
        sql, parameters = sqlglot.parameterize(
            "SELECT * FROM t WHERE a = 1 AND b = '1' AND c = TRUE"
        )

        self.assertEqual(sql, "SELECT * FROM t WHERE a = :p0 AND b = :p1 AND c = :p2")
        self.assertEqual(parameters, {"p0": 1, "p1": "1", "p2": True})

    def test_builder_entry_point(self):
        sql, parameters = parameterize(sqlglot.parse_one("SELECT * FROM t WHERE a = 1"))

        self.assertEqual(sql, "SELECT * FROM t WHERE a = :p0")
        self.assertEqual(parameters, {"p0": 1})

    def test_literal_reuse_is_explicit(self):
        query = "SELECT * FROM t WHERE a = 1 AND b = '1' AND c = 1 AND d = TRUE"

        _, distinct = sqlglot.parameterize(query)
        _, reused = sqlglot.parameterize(query, reuse=True)

        self.assertEqual(distinct, {"p0": 1, "p1": "1", "p2": 1, "p3": True})
        self.assertEqual(reused, {"p0": 1, "p1": "1", "p2": True})

    def test_positional_values_follow_sql_order(self):
        sql, parameters = sqlglot.parameterize(
            "SELECT * FROM t WHERE a = 1 AND b = 2 LIMIT 3",
            named=False,
        )

        self.assertEqual(sql, "SELECT * FROM t WHERE a = ? AND b = ? LIMIT ?")
        self.assertEqual(parameters, [1, 2, 3])

    def test_existing_placeholders_are_preserved(self):
        sql, parameters = sqlglot.parameterize(
            "SELECT * FROM t WHERE a = 1 AND b = :p0 AND c = ?"
        )

        self.assertEqual(sql, "SELECT * FROM t WHERE a = :p1 AND b = :p0 AND c = ?")
        self.assertEqual(parameters, {"p1": 1})

    def test_structural_positions_depend_on_dialect(self):
        query = "SELECT CAST(x AS VARCHAR(10)), DATE_TRUNC('day', y) FROM t WHERE z = 2 LIMIT 3"

        postgres_sql, postgres_parameters = sqlglot.parameterize(
            query, dialect="postgres"
        )
        spark_sql, spark_parameters = sqlglot.parameterize(query, dialect="spark")

        self.assertIn("VARCHAR(10)", postgres_sql)
        self.assertIn("LIMIT %(p1)s", postgres_sql)
        self.assertEqual(postgres_parameters, {"p0": 2, "p1": 3})
        self.assertIn("CAST(x AS STRING)", spark_sql)
        self.assertIn("LIMIT 3", spark_sql)
        self.assertEqual(spark_parameters, {"p0": 2})

    def test_generated_sql_roundtrips_placeholders(self):
        sql, parameters = sqlglot.parameterize(
            "SELECT * FROM t WHERE a = 1 AND b = :p1"
        )
        reparsed = sqlglot.parse_one(sql)

        placeholders = Counter(
            placeholder.name
            for placeholder in reparsed.find_all(exp.Placeholder, exp.Parameter)
        )
        self.assertEqual(placeholders, Counter({"p0": 1, "p1": 1}))
        self.assertEqual(parameters, {"p0": 1})

    def test_original_expression_is_not_modified(self):
        expression = sqlglot.parse_one("SELECT * FROM t WHERE a = 1 AND b = '1'")
        original_sql = expression.sql()
        original_tree = expression.to_s()

        expression.parameterize(dialect="postgres", reuse=True)

        self.assertEqual(expression.sql(), original_sql)
        self.assertEqual(expression.to_s(), original_tree)

    def test_clickhouse_placeholder_types(self):
        sql, parameters = sqlglot.parameterize(
            "SELECT * FROM t WHERE a = 1 AND b = '1'",
            dialect="clickhouse",
        )

        self.assertEqual(
            sql,
            "SELECT * FROM t WHERE a = {p0: Nullable(Int64)} "
            "AND b = {p1: Nullable(String)}",
        )
        self.assertEqual(parameters, {"p0": 1, "p1": "1"})


if __name__ == "__main__":
    unittest.main()
