import unittest

from sqlglot import AnalysisStatus, Catalog, analyze_script, exp, parse_one
from sqlglot.errors import ErrorLevel, ParseError
from sqlglot.schema import MappingSchema


class TestAnalyzer(unittest.TestCase):
    def test_create_table_updates_catalog(self):
        catalog = Catalog()
        status = catalog.apply(parse_one("CREATE TABLE t (a INT, b VARCHAR(10))"))

        self.assertEqual(status, AnalysisStatus.SUCCESS)
        self.assertEqual(catalog.version, 1)
        self.assertTrue(catalog.has_table(exp.to_table("t")))
        self.assertEqual(catalog.column_names(exp.to_table("t")), ["a", "b"])
        self.assertEqual(
            catalog.get_column_type(exp.to_table("t"), "a"), exp.DataType.build("int")
        )

    def test_read_only_statements_keep_version(self):
        catalog = Catalog()
        for sql in ("SELECT 1", "INSERT INTO t VALUES (1)", "BEGIN", "COMMIT"):
            status = catalog.apply(parse_one(sql))
            self.assertEqual(status, AnalysisStatus.SUCCESS)
            self.assertEqual(catalog.version, 0)

    def test_ctas_infers_columns(self):
        catalog = Catalog()
        status = catalog.apply(parse_one("CREATE TABLE t AS SELECT 1 AS x, 'y' AS y"))

        self.assertEqual(status, AnalysisStatus.SUCCESS)
        self.assertEqual(catalog.column_names(exp.to_table("t")), ["x", "y"])
        self.assertEqual(
            catalog.get_column_type(exp.to_table("t"), "x"), exp.DataType.build("int")
        )
        self.assertEqual(
            catalog.get_column_type(exp.to_table("t"), "y"), exp.DataType.build("varchar")
        )

    def test_ctas_unknown_star_is_unknown(self):
        catalog = Catalog()
        status = catalog.apply(parse_one("CREATE TABLE t AS SELECT * FROM missing"))

        self.assertEqual(status, AnalysisStatus.UNKNOWN)
        self.assertEqual(catalog.version, 0)
        self.assertFalse(catalog.has_table(exp.to_table("t")))

    def test_ctas_expands_known_star(self):
        catalog = Catalog({"src": {"a": "INT", "b": "TEXT"}})
        status = catalog.apply(parse_one("CREATE TABLE cpy AS SELECT * FROM src"))

        self.assertEqual(status, AnalysisStatus.SUCCESS)
        self.assertEqual(catalog.column_names(exp.to_table("cpy")), ["a", "b"])

        # A temporary source shadows its persistent namesake during star resolution
        shadow = Catalog({"src": {"persistent": "INT"}})
        shadow.apply(parse_one("CREATE TEMPORARY TABLE src (temp_col INT)"))
        shadow.apply(parse_one("CREATE TABLE cpy AS SELECT * FROM src"))
        self.assertEqual(shadow.column_names(exp.to_table("cpy")), ["temp_col"])

    def test_temporary_shadows_persistent_and_drop_restores(self):
        catalog = Catalog({"t": {"persistent_col": "INT"}})
        table = exp.to_table("t")

        self.assertEqual(catalog.column_names(table), ["persistent_col"])

        catalog.apply(parse_one("CREATE TEMPORARY TABLE t (temp_col INT)"))
        self.assertTrue(catalog.is_temporary(table))
        self.assertEqual(catalog.column_names(table), ["temp_col"])

        catalog.apply(parse_one("DROP TEMPORARY TABLE t"))
        self.assertFalse(catalog.is_temporary(table))
        self.assertEqual(catalog.column_names(table), ["persistent_col"])

    def test_plain_drop_removes_temporary_first(self):
        catalog = Catalog({"t": {"p": "INT"}})
        table = exp.to_table("t")

        catalog.apply(parse_one("CREATE TEMPORARY TABLE t (tmp INT)"))
        catalog.apply(parse_one("DROP TABLE t"))

        self.assertFalse(catalog.is_temporary(table))
        self.assertEqual(catalog.column_names(table), ["p"])

    def test_drop_persistent_table(self):
        catalog = Catalog({"t": {"a": "INT"}})
        status = catalog.apply(parse_one("DROP TABLE t"))

        self.assertEqual(status, AnalysisStatus.SUCCESS)
        self.assertFalse(catalog.has_table(exp.to_table("t")))

    def test_drop_unknown_table_errors_without_partial_state(self):
        catalog = Catalog({"keep": {"a": "INT"}})
        with self.assertRaises(ParseError):
            catalog.apply(parse_one("DROP TABLE missing"))

        self.assertTrue(catalog.has_table(exp.to_table("keep")))
        self.assertEqual(catalog.version, 0)

        # IF EXISTS makes dropping a missing table a no-op
        status = catalog.apply(parse_one("DROP TABLE IF EXISTS missing"))
        self.assertEqual(status, AnalysisStatus.SUCCESS)

    def test_multi_table_drop_is_atomic(self):
        catalog = Catalog({"a": {"x": "INT"}, "b": {"y": "INT"}})
        with self.assertRaises(ParseError):
            catalog.apply(parse_one("DROP TABLE a, missing"))

        # The whole statement is rolled back, so `a` survives
        self.assertTrue(catalog.has_table(exp.to_table("a")))
        self.assertTrue(catalog.has_table(exp.to_table("b")))
        self.assertEqual(catalog.version, 0)

    def test_unsupported_catalog_mutations_are_unknown(self):
        catalog = Catalog({"t": {"a": "INT"}})
        for sql in (
            "ALTER TABLE t ADD COLUMN b INT",
            "TRUNCATE TABLE t",
            "RENAME TABLE t TO u",
            "CREATE VIEW v AS SELECT 1 AS x",
        ):
            status = catalog.apply(parse_one(sql))
            self.assertEqual(status, AnalysisStatus.UNKNOWN, msg=sql)
            self.assertEqual(catalog.version, 0, msg=sql)

        self.assertEqual(catalog.column_names(exp.to_table("t")), ["a"])

    def test_unknown_statement_leaves_no_partial_structure(self):
        catalog = Catalog()
        # A view named the same as a not-yet-created table must not register a table.
        catalog.apply(parse_one("CREATE VIEW t AS SELECT 1 AS x"))
        self.assertFalse(catalog.has_table(exp.to_table("t")))

    def test_create_if_not_exists(self):
        catalog = Catalog({"t": {"a": "INT"}})
        table = exp.to_table("t")

        status = catalog.apply(parse_one("CREATE TABLE IF NOT EXISTS t (b INT)"))
        self.assertEqual(status, AnalysisStatus.SUCCESS)
        self.assertEqual(catalog.column_names(table), ["a"])

        status = catalog.apply(parse_one("CREATE TABLE IF NOT EXISTS u (b INT)"))
        self.assertEqual(status, AnalysisStatus.SUCCESS)
        self.assertTrue(catalog.has_table(exp.to_table("u")))

    def test_qualified_names_and_pruning(self):
        catalog = Catalog({"db": {"t": {"a": "INT"}}})
        table = exp.to_table("db.t")

        self.assertTrue(catalog.has_table(table))
        catalog.apply(parse_one("DROP TABLE db.t"))
        self.assertFalse(catalog.has_table(table))
        self.assertEqual(catalog.persistent.mapping, {})

    def test_analyze_script_versions_and_statuses(self):
        script = """
        CREATE TABLE t (a INT);
        SELECT * FROM t;
        ALTER TABLE t ADD COLUMN b INT;
        DROP TABLE t;
        ));
        """
        results = analyze_script(script)
        statuses = [result.status for result in results]
        versions = [result.catalog_version for result in results]

        self.assertEqual(
            statuses,
            [
                AnalysisStatus.SUCCESS,
                AnalysisStatus.SUCCESS,
                AnalysisStatus.UNKNOWN,
                AnalysisStatus.SUCCESS,
                AnalysisStatus.ERROR,
            ],
        )
        self.assertEqual(versions, [0, 1, 1, 1, 2])
        self.assertIsInstance(results[-1].error, ParseError)

    def test_analyze_script_uses_prior_ddl(self):
        script = """
        CREATE TABLE t (a INT);
        CREATE TEMPORARY TABLE t (b INT);
        DROP TEMPORARY TABLE t;
        """
        results = analyze_script(script)
        self.assertTrue(all(result.status == AnalysisStatus.SUCCESS for result in results))

        catalog = Catalog()
        for result in results:
            catalog.apply(result.expression)

        self.assertEqual(catalog.column_names(exp.to_table("t")), ["a"])
        self.assertEqual(catalog.version, 3)

    def test_initial_schema_is_not_mutated(self):
        initial = {"t": {"a": "INT"}}
        analyze_script("CREATE TABLE u (b INT); DROP TABLE t;", schema=initial)
        self.assertEqual(initial, {"t": {"a": "INT"}})

        analyze_script("CREATE TABLE v (c INT);", schema=initial)
        self.assertEqual(initial, {"t": {"a": "INT"}})

    def test_repeated_analysis_is_deterministic(self):
        script = "CREATE TABLE t (a INT); ALTER TABLE t ADD COLUMN b INT; DROP TABLE t;"
        first = [(r.status, r.catalog_version) for r in analyze_script(script)]
        second = [(r.status, r.catalog_version) for r in analyze_script(script)]
        self.assertEqual(first, second)

    def test_mapping_schema_input_is_copied(self):
        schema = MappingSchema({"t": {"a": "INT"}})
        catalog = Catalog(schema)
        catalog.apply(parse_one("CREATE TABLE u (b INT)"))

        self.assertIsNot(catalog.persistent, schema)
        self.assertNotIn("u", schema.mapping)

    def test_error_level_raise(self):
        with self.assertRaises(ParseError):
            analyze_script("SELECT FROM;", error_level=ErrorLevel.RAISE)

    def test_statement_collection_input(self):
        results = analyze_script(
            ["CREATE TABLE t (a INT)", "DROP TABLE t"],
        )
        self.assertEqual(
            [r.status for r in results],
            [AnalysisStatus.SUCCESS, AnalysisStatus.SUCCESS],
        )
        self.assertEqual([r.catalog_version for r in results], [0, 1])

    def test_dialect_normalization(self):
        catalog = Catalog(dialect="postgres")
        catalog.apply(parse_one('CREATE TABLE "T" ("A" INT)', dialect="postgres"))

        # postgres treats quoted identifiers as case-sensitive
        quoted = parse_one('"T"', into=exp.Table, dialect="postgres")
        lowercased = parse_one("t", into=exp.Table, dialect="postgres")
        self.assertTrue(catalog.has_table(quoted))
        self.assertFalse(catalog.has_table(lowercased))


if __name__ == "__main__":
    unittest.main()
