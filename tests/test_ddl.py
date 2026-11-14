import unittest

from sqlglot import GeneratedColumnError, exp, parse_one
from sqlglot.ddl import (
    GeneratedColumnAnalysis,
    analyze_generated_columns,
    extract_table_schemas,
)


class TestGeneratedColumns(unittest.TestCase):
    def test_basic_dependencies_and_order(self):
        analysis = analyze_generated_columns(
            """
            CREATE TABLE t (
              a INT,
              b INT AS (a + 1),
              c INT AS (b * 2),
              d INT AS (a + c)
            )
            """,
            dialect="mysql",
        )

        self.assertTrue(analysis.valid)
        self.assertEqual(analysis.columns, ("a", "b", "c", "d"))
        self.assertEqual(analysis.generated_columns, ("b", "c", "d"))
        self.assertEqual(
            {dep.column for dep in analysis.dependencies["b"]}, {"a"}
        )
        self.assertEqual(
            {dep.column for dep in analysis.dependencies["c"]}, {"b"}
        )
        self.assertEqual(
            {dep.column for dep in analysis.dependencies["d"]}, {"a", "c"}
        )
        self.assertEqual(analysis.computable_order, ("b", "c", "d"))

    def test_column_types_are_extracted(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b VARCHAR(10) AS (a), c AS (a + 1))",
            dialect="mysql",
        )

        self.assertEqual(analysis.column_types["a"].sql(), "INT")
        self.assertEqual(analysis.column_types["b"].sql(), "VARCHAR(10)")
        self.assertIsNone(analysis.column_types["c"])

    def test_dependency_flags(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b AS (a), c AS (b), d AS (a + b))",
            dialect="mysql",
        )
        flags = {dep.column: dep.generated for dep in analysis.dependencies["d"]}
        self.assertEqual(flags, {"a": False, "b": True})

        reference = analysis.dependencies["b"][0].reference
        self.assertIsInstance(reference, exp.Column)
        self.assertEqual(reference.sql(), "a")

    def test_string_literals_and_functions_are_not_dependencies(self):
        analysis = analyze_generated_columns(
            """
            CREATE TABLE t (
              a INT,
              b VARCHAR(20) AS (CAST(a AS VARCHAR)),
              c VARCHAR(20) AS (CASE WHEN a > 0 THEN 'positive' ELSE 'zero' END),
              d VARCHAR(20) AS (CONCAT('x', b)),
              e VARCHAR(20) AS (length('a') + 1)
            )
            """,
            dialect="mysql",
        )

        self.assertTrue(analysis.valid, analysis.issues)
        references = {
            dep.column
            for deps in analysis.dependencies.values()
            for dep in deps
        }
        self.assertEqual(references, {"a", "b"})

    def test_self_reference(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT AS (a + 1))", dialect="mysql"
        )

        self.assertFalse(analysis.valid)
        self.assertEqual(len(analysis.issues), 1)
        issue = analysis.issues[0]
        self.assertEqual(issue.kind, "self_reference")
        self.assertEqual(issue.column, "a")
        self.assertEqual(issue.chain, ("a", "a"))
        self.assertEqual(analysis.computable_order, ())

        with self.assertRaises(GeneratedColumnError):
            analysis.raise_on_issues()

    def test_indirect_cycle(self):
        analysis = analyze_generated_columns(
            """
            CREATE TABLE t (
              a INT,
              b INT AS (c + 1),
              c INT AS (b + 1),
              d INT AS (a + 1)
            )
            """,
            dialect="mysql",
        )

        self.assertEqual(len(analysis.issues), 1)
        issue = analysis.issues[0]
        self.assertEqual(issue.kind, "cycle")
        self.assertEqual(issue.chain[0], issue.chain[-1])
        self.assertEqual(set(issue.chain[:-1]), {"b", "c"})
        # The independent generated column remains computable.
        self.assertEqual(analysis.computable_order, ("d",))

    def test_cycle_chain_covers_every_participating_column(self):
        analysis = analyze_generated_columns(
            """
            CREATE TABLE t (
              a INT AS (d),
              b INT AS (a),
              c INT AS (b),
              d INT AS (c)
            )
            """,
            dialect="mysql",
        )

        self.assertEqual(len(analysis.issues), 1)
        chain = analysis.issues[0].chain
        self.assertEqual(chain[0], chain[-1])
        self.assertEqual(set(chain[:-1]), {"a", "b", "c", "d"})

    def test_multiple_disjoint_cycles(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a AS (b), b AS (a), c AS (d), d AS (c))",
            dialect="mysql",
        )

        self.assertEqual(len(analysis.issues), 2)
        chains = sorted(sorted(set(issue.chain)) for issue in analysis.issues)
        self.assertEqual(chains, [["a", "b"], ["c", "d"]])

    def test_unknown_column(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b INT AS (a + missing))", dialect="mysql"
        )

        self.assertEqual(len(analysis.issues), 1)
        issue = analysis.issues[0]
        self.assertEqual(issue.kind, "unknown_column")
        self.assertEqual(issue.column, "b")
        self.assertEqual(issue.chain, ("b", "missing"))
        self.assertEqual(analysis.computable_order, ())

    def test_unknown_column_does_not_block_independent_columns(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b AS (missing), c AS (a))",
            dialect="mysql",
        )

        self.assertEqual(analysis.computable_order, ("c",))

    def test_quoted_identifier_case_is_preserved(self):
        analysis = analyze_generated_columns(
            'CREATE TABLE t ("Foo" INT, bar INT AS ("Foo" + 1), baz INT AS (foo + 2))',
            dialect="postgres",
        )

        # The quoted "Foo" reference resolves; the unquoted foo folds to "foo"
        # and cannot match the case-sensitive quoted column.
        self.assertEqual(len(analysis.issues), 1)
        self.assertEqual(analysis.issues[0].kind, "unknown_column")
        self.assertEqual(analysis.issues[0].chain, ("baz", "foo"))
        self.assertEqual(analysis.computable_order, ("bar",))

    def test_unquoted_identifier_case_folding(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (foo INT, bar INT AS (FOO + 1))",
            dialect="snowflake",
        )

        self.assertTrue(analysis.valid, analysis.issues)
        self.assertEqual(analysis.dependencies["BAR"][0].column, "FOO")

    def test_qualified_column_reference(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b AS (t.a + 1))", dialect="mysql"
        )

        self.assertTrue(analysis.valid)
        self.assertEqual(analysis.dependencies["b"][0].column, "a")

    def test_postgres_generated_always_as(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b INT GENERATED ALWAYS AS (a * 2) STORED)",
            dialect="postgres",
        )

        self.assertTrue(analysis.valid)
        self.assertEqual(analysis.generated_columns, ("b",))
        self.assertEqual(analysis.computable_order, ("b",))

    def test_identity_columns_without_expression_are_ignored(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t ("
            "a INT GENERATED ALWAYS AS IDENTITY, "
            "b INT GENERATED ALWAYS AS (a + 1))",
            dialect="postgres",
        )

        self.assertEqual(analysis.generated_columns, ("b",))
        self.assertTrue(analysis.valid)

    def test_clickhouse_alias_and_materialized(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a Int32, b Int32 ALIAS a * 2, c Int32 MATERIALIZED a + 1)",
            dialect="clickhouse",
        )

        self.assertEqual(analysis.generated_columns, ("b", "c"))
        self.assertEqual(analysis.computable_order, ("b", "c"))
        self.assertEqual(
            {dep.column for dep in analysis.dependencies["b"]}, {"a"}
        )

    def test_no_generated_columns(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE t (a INT, b TEXT)", dialect="mysql"
        )

        self.assertTrue(analysis.valid)
        self.assertEqual(analysis.generated_columns, ())
        self.assertEqual(analysis.dependencies, {})
        self.assertEqual(analysis.computable_order, ())

    def test_ctas_and_like(self):
        for sql in (
            "CREATE TABLE t AS SELECT 1 AS a",
            "CREATE TABLE t LIKE other",
        ):
            with self.subTest(sql):
                analysis = analyze_generated_columns(sql, dialect="mysql")
                self.assertTrue(analysis.valid)
                self.assertEqual(analysis.columns, ())
                self.assertEqual(analysis.table, "t")

    def test_table_name_and_qualified_name(self):
        analysis = analyze_generated_columns(
            "CREATE TABLE db.t (a INT, b AS (a))", dialect="mysql"
        )
        self.assertEqual(analysis.table, "db.t")

    def test_accepts_parsed_expression(self):
        ast = parse_one("CREATE TABLE t (a INT, b AS (a + 1))", dialect="mysql")
        analysis = analyze_generated_columns(ast, dialect="mysql")
        self.assertTrue(analysis.valid)
        self.assertIsInstance(analysis, GeneratedColumnAnalysis)

    def test_rejects_non_create_table(self):
        for sql in ("SELECT 1", "CREATE VIEW v AS SELECT 1"):
            with self.subTest(sql):
                with self.assertRaises(ValueError):
                    analyze_generated_columns(sql)

    def test_batch_isolation(self):
        extraction = extract_table_schemas(
            """
            CREATE TABLE good (a INT, b AS (a + 1));
            CREATE TABLE self_ref (x INT AS (x));
            CREATE TABLE cyclic (p INT AS (q), q INT AS (p));
            """,
            dialect="mysql",
        )

        analyses = extraction.analyses
        self.assertEqual(extraction.errors, ())
        self.assertEqual(set(analyses), {"good", "self_ref", "cyclic"})
        self.assertTrue(analyses["good"].valid)
        self.assertEqual(
            analyses["self_ref"].issues[0].kind, "self_reference"
        )
        self.assertEqual(analyses["cyclic"].issues[0].kind, "cycle")
        # Same-named columns across tables do not create cross-table dependencies.
        self.assertEqual(
            analyses["good"].dependencies["b"][0].column, "a"
        )

    def test_batch_sequence_and_non_ddl_statements(self):
        extraction = extract_table_schemas(
            [
                "CREATE TABLE t (a INT, b AS (a))",
                "SELECT 1",
                parse_one("CREATE VIEW v AS SELECT 1"),
            ],
            dialect="mysql",
        )

        self.assertEqual(list(extraction.analyses), ["t"])
        self.assertEqual(extraction.errors, ())

    def test_batch_analyses_are_independent(self):
        extraction = extract_table_schemas(
            "CREATE TABLE x (a INT, b AS (a)); CREATE TABLE y (a INT, b AS (a))",
            dialect="mysql",
        )

        analyses = extraction.analyses
        self.assertEqual(set(analyses), {"x", "y"})
        for analysis in analyses.values():
            self.assertEqual(
                [dep.column for dep in analysis.dependencies["b"]], ["a"]
            )

    def test_batch_error_does_not_poison_other_tables(self):
        good = parse_one(
            "CREATE TABLE good (a INT, b AS (a + 1))", dialect="mysql"
        )
        # A malformed generated column whose reference cannot be normalized: the
        # analysis must fail for this table alone, leaving the good catalog untouched.
        broken = parse_one("CREATE TABLE broken (a INT, b AS (a))", dialect="mysql")

        class BrokenIdentifier(exp.Identifier):
            def copy(self) -> "BrokenIdentifier":
                raise RuntimeError("broken identifier")

        broken.find(exp.ComputedColumnConstraint).find(exp.Column).set(
            "this", BrokenIdentifier(this="a")
        )

        extraction = extract_table_schemas([good, broken], dialect="mysql")

        self.assertEqual(list(extraction.analyses), ["good"])
        self.assertTrue(extraction.analyses["good"].valid)
        self.assertEqual(len(extraction.errors), 1)
        self.assertEqual(extraction.errors[0].table, "broken")
        self.assertIsInstance(extraction.errors[0].error, GeneratedColumnError)

        with self.assertRaises(GeneratedColumnError):
            extraction.raise_on_errors()
