import unittest

from sqlglot import exp, parse_one
from sqlglot.executor.python import PythonExecutor
from sqlglot.executor.table import ensure_tables
from sqlglot.optimizer import optimize
from sqlglot.optimizer.helpers import (
    contains_volatile,
    function_volatility,
    normalize_function_volatilities,
)
from sqlglot.optimizer.qualify import qualify
from sqlglot.planner import Plan


class TestFunctionVolatility(unittest.TestCase):
    """Call-level function volatility and the optimizer boundaries that respect it.

    Execution probes run queries on sqlglot's own executor with a locally countable
    NEXT_VAL/RAND function (each call returns the next integer), so tests compare
    actual call counts and row values before and after rewriting, not just SQL text.
    """

    @classmethod
    def setUpClass(cls):
        cls.schema = {"t": {"i": "INT"}}
        cls.tables = ensure_tables({"t": [{"i": 1}, {"i": 2}, {"i": 3}]})

    def run_probe(self, expression, func_name="NEXT_VAL"):
        calls = []

        def next_val():
            calls.append(None)
            return len(calls)

        executor = PythonExecutor(env={func_name: next_val}, tables=self.tables)
        result = executor.execute(Plan(expression))
        return [tuple(row) for row in result.rows], len(calls)

    def optimize(self, sql, **kwargs):
        return optimize(sql, schema=self.schema, **kwargs)

    def qualify(self, sql, **kwargs):
        return qualify(parse_one(sql), schema=self.schema, **kwargs)

    def test_classification(self):
        # per-call volatile: random values, uuids, sequences
        self.assertEqual(exp.Rand.volatility, exp.Volatility.VOLATILE)
        self.assertEqual(exp.Uuid.volatility, exp.Volatility.VOLATILE)
        self.assertEqual(exp.NextValueFor.volatility, exp.Volatility.VOLATILE)

        # statement-stable: constant within a single statement
        self.assertEqual(exp.CurrentTimestamp.volatility, exp.Volatility.STABLE)
        self.assertEqual(exp.CurrentDate.volatility, exp.Volatility.STABLE)
        self.assertEqual(exp.CurrentUser.volatility, exp.Volatility.STABLE)

        # pure: ordinary arithmetic and scalar functions
        self.assertEqual(exp.Floor.volatility, exp.Volatility.PURE)
        self.assertEqual(exp.Concat.volatility, exp.Volatility.PURE)

        # unknown (custom) functions are conservatively volatile
        self.assertEqual(exp.Anonymous.volatility, exp.Volatility.VOLATILE)
        self.assertEqual(exp.AnonymousAggFunc.volatility, exp.Volatility.VOLATILE)

    def test_no_name_based_guessing(self):
        # an unknown function is volatile because it is unknown, not because of its name
        anonymous = parse_one("SELECT grand_total()").expressions[0]
        self.assertIsInstance(anonymous, exp.Anonymous)
        self.assertEqual(function_volatility(anonymous), exp.Volatility.VOLATILE)

        # a known pure function is pure regardless of what it is called
        floor = parse_one("SELECT FLOOR(1.5)").expressions[0]
        self.assertEqual(function_volatility(floor), exp.Volatility.PURE)

        # RAND is volatile because of its declared class volatility, and the same
        # holds for its RANDOM alias
        self.assertIsInstance(parse_one("SELECT RANDOM()").expressions[0], exp.Rand)

        # call-level declarations override the classification, in either direction
        overrides = normalize_function_volatilities({"grand_total": "pure", "FLOOR": "volatile"})
        self.assertEqual(function_volatility(anonymous, overrides), exp.Volatility.PURE)
        self.assertEqual(function_volatility(floor, overrides), exp.Volatility.VOLATILE)

    def test_merge_derived_table_preserves_volatile_calls(self):
        sql = "SELECT q.r AS a, q.r AS b FROM (SELECT RAND() AS r FROM t) q"
        baseline = self.run_probe(
            self.qualify("WITH q AS (SELECT RAND() AS r FROM t) SELECT q.r AS a, q.r AS b FROM q"),
            func_name="RAND",
        )
        # one evaluation per row; both references observe the same value
        self.assertEqual(baseline, ([(1, 1), (2, 2), (3, 3)], 3))

        optimized = self.optimize(sql)
        # the boundary is kept: the volatile projection is not inlined into both references
        self.assertIn("WITH", optimized.sql())
        self.assertEqual(self.run_probe(optimized, func_name="RAND"), baseline)

    def test_merge_cte_preserves_volatile_calls(self):
        sql = "WITH c AS (SELECT RAND() AS r FROM t) SELECT c.r AS a, c.r AS b FROM c"
        baseline = self.run_probe(self.qualify(sql), func_name="RAND")
        self.assertEqual(baseline, ([(1, 1), (2, 2), (3, 3)], 3))

        optimized = self.optimize(sql)
        self.assertIn("WITH", optimized.sql())
        self.assertEqual(self.run_probe(optimized, func_name="RAND"), baseline)

    def test_merge_single_reference_still_merges(self):
        # a volatile projection referenced once is evaluated once either way
        optimized = self.optimize("SELECT q.r AS a FROM (SELECT RAND() AS r FROM t) q")
        self.assertNotIn("WITH", optimized.sql())
        self.assertEqual(
            self.run_probe(optimized, func_name="RAND"),
            ([(1,), (2,), (3,)], 3),
        )

    def test_merge_pure_projection_still_merges(self):
        sql = "SELECT q.r AS a, q.r AS b FROM (SELECT i + 1 AS r FROM t) q"
        baseline = self.run_probe(
            self.qualify("WITH q AS (SELECT i + 1 AS r FROM t) SELECT q.r AS a, q.r AS b FROM q")
        )

        optimized = self.optimize(sql)
        # ordinary arithmetic is still inlined into every reference
        self.assertNotIn("WITH", optimized.sql())
        self.assertEqual(
            optimized.sql(),
            'SELECT "t"."i" + 1 AS "a", "t"."i" + 1 AS "b" FROM "t" AS "t"',
        )
        self.assertEqual(self.run_probe(optimized), baseline)
        self.assertEqual(baseline, ([(2, 2), (3, 3), (4, 4)], 0))

    def test_alias_expansion_preserves_volatile_calls(self):
        sql = "SELECT NEXT_VAL() AS r, r + 1 AS s FROM t"
        # equivalent executable probe for the original query's semantics
        baseline = self.run_probe(
            self.qualify(
                "WITH q AS (SELECT NEXT_VAL() AS r FROM t) SELECT q.r AS r, q.r + 1 AS s FROM q"
            )
        )
        # one evaluation per row; s is derived from the same value as r
        self.assertEqual(baseline, ([(1, 2), (2, 3), (3, 4)], 3))

        # expanding the alias would double the evaluations and change every row
        expanded = self.qualify(sql, function_volatilities={"NEXT_VAL": "pure"})
        self.assertEqual(
            self.run_probe(expanded),
            ([(1, 3), (3, 5), (5, 7)], 6),
        )

        optimized = self.optimize(sql)
        # the boundary is kept: a single volatile call, referenced by name
        self.assertEqual(optimized.sql().upper().count("NEXT_VAL("), 1)
        self.assertEqual(
            optimized.sql(),
            'SELECT NEXT_VAL() AS "r", "r" + 1 AS "s" FROM "t" AS "t"',
        )

    def test_alias_expansion_pure_still_expands(self):
        optimized = self.optimize("SELECT i + 1 AS r, r * 2 AS s FROM t")
        self.assertEqual(
            optimized.sql(),
            'SELECT "t"."i" + 1 AS "r", ("t"."i" + 1) * 2 AS "s" FROM "t" AS "t"',
        )
        self.assertEqual(self.run_probe(optimized), ([(2, 4), (3, 6), (4, 8)], 0))

    def test_alias_expansion_stable_still_expands(self):
        # statement-stable functions may be duplicated within a statement
        optimized = self.optimize("SELECT CURRENT_TIMESTAMP AS ts, ts + 1 AS s FROM t")
        self.assertEqual(optimized.sql().upper().count("CURRENT_TIMESTAMP"), 2)

    def test_unused_volatile_cte_is_kept(self):
        optimized = self.optimize("WITH c AS (SELECT RAND() AS r FROM t) SELECT 1 AS one")
        self.assertIn("WITH", optimized.sql())
        self.assertIn("RAND", optimized.sql())

        # pure unused CTEs are still eliminated
        optimized = self.optimize("WITH c AS (SELECT i AS r FROM t) SELECT 1 AS one")
        self.assertNotIn("WITH", optimized.sql())

    def test_call_level_overrides(self):
        sql = "SELECT q.r AS a, q.r AS b FROM (SELECT RAND() AS r FROM t) q"

        # declaring RAND pure re-enables the merge; the caller accepts the extra calls
        optimized = self.optimize(sql, function_volatilities={"RAND": "pure"})
        self.assertNotIn("WITH", optimized.sql())
        self.assertEqual(
            self.run_probe(optimized, func_name="RAND"),
            ([(1, 2), (3, 4), (5, 6)], 6),
        )

        # declaring a pure function volatile blocks both merging and alias expansion
        optimized = self.optimize(
            "SELECT FLOOR(i) AS r, r + 1 AS s FROM t",
            function_volatilities={"floor": "volatile"},
        )
        self.assertEqual(optimized.sql().upper().count("FLOOR("), 1)

        # overrides match declared SQL names, including aliases, case-insensitively
        optimized = self.optimize(sql, function_volatilities={"random": "pure"})
        self.assertNotIn("WITH", optimized.sql())


if __name__ == "__main__":
    unittest.main()
