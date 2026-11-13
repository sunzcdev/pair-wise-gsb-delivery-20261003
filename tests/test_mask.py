import unittest

import sqlglot
from sqlglot import exp
from sqlglot.errors import MaskingError
from sqlglot.mask import mask_columns


class TestMaskColumns(unittest.TestCase):
    def setUp(self):
        self.schema = {
            "db": {
                "users": {"id": "int", "ssn": "text", "name": "text"},
                "orders": {"id": "int", "ssn": "text", "uid": "int"},
            }
        }
        self.masks = {("db.users", "ssn"): "CONCAT('***', RIGHT(:source, 4))"}

    def mask(self, sql, masks=None, **kwargs):
        kwargs.setdefault("schema", self.schema)
        return mask_columns(sql, masks or self.masks, **kwargs).sql()

    def test_only_final_projection_is_masked(self):
        self.assertEqual(
            self.mask(
                "SELECT u.ssn, u.name FROM db.users u "
                "WHERE u.ssn > '1' GROUP BY u.ssn, u.name HAVING u.ssn > '2'"
            ),
            "SELECT CONCAT('***', RIGHT(\"u\".\"ssn\", 4)) AS \"ssn\", \"u\".\"name\" AS \"name\" "
            "FROM \"db\".\"users\" AS \"u\" "
            "WHERE \"u\".\"ssn\" > '1' "
            "GROUP BY \"u\".\"ssn\", \"u\".\"name\" HAVING \"u\".\"ssn\" > '2'",
        )

    def test_star_expanded_with_schema(self):
        self.assertEqual(
            self.mask("SELECT * FROM db.users u"),
            "SELECT \"u\".\"id\" AS \"id\", CONCAT('***', RIGHT(\"u\".\"ssn\", 4)) AS \"ssn\", "
            "\"u\".\"name\" AS \"name\" FROM \"db\".\"users\" AS \"u\"",
        )

    def test_cte_rename_and_multilevel_projection(self):
        self.assertEqual(
            self.mask(
                "WITH a AS (SELECT u.ssn AS secret FROM db.users u), "
                "b AS (SELECT a.secret AS renamed FROM a) "
                "SELECT b.renamed AS out_col FROM b"
            ),
            "WITH \"a\" AS (SELECT \"u\".\"ssn\" AS \"secret\" FROM \"db\".\"users\" AS \"u\"), "
            "\"b\" AS (SELECT \"a\".\"secret\" AS \"renamed\" FROM \"a\" AS \"a\") "
            "SELECT CONCAT('***', RIGHT(\"b\".\"renamed\", 4)) AS \"out_col\" FROM \"b\" AS \"b\"",
        )

    def test_derived_table_projection(self):
        self.assertEqual(
            self.mask("SELECT t.x FROM (SELECT u.ssn AS x FROM db.users u) t"),
            "SELECT CONCAT('***', RIGHT(\"t\".\"x\", 4)) AS \"x\" "
            "FROM (SELECT \"u\".\"ssn\" AS \"x\" FROM \"db\".\"users\" AS \"u\") AS \"t\"",
        )

    def test_same_name_column_from_other_table_is_untouched(self):
        self.assertEqual(
            self.mask("SELECT u.ssn, o.ssn FROM db.users u JOIN db.orders o ON o.uid = u.id"),
            "SELECT CONCAT('***', RIGHT(\"u\".\"ssn\", 4)) AS \"ssn\", \"o\".\"ssn\" AS \"ssn\" "
            "FROM \"db\".\"users\" AS \"u\" "
            "JOIN \"db\".\"orders\" AS \"o\" ON \"o\".\"uid\" = \"u\".\"id\"",
        )

    def test_order_by_output_alias(self):
        masked = self.mask("SELECT u.ssn AS ssn FROM db.users u ORDER BY ssn DESC")
        self.assertEqual(
            masked,
            "SELECT CONCAT('***', RIGHT(\"u\".\"ssn\", 4)) AS \"ssn\" "
            "FROM \"db\".\"users\" AS \"u\" ORDER BY \"ssn\" DESC",
        )
        # The ORDER BY term still targets the masked output column
        self.assertNotIn("ORDER BY \"u\"", masked)

    def test_union_branches(self):
        self.assertEqual(
            self.mask("SELECT u.ssn FROM db.users u UNION SELECT o.ssn FROM db.orders o"),
            "SELECT CONCAT('***', RIGHT(\"u\".\"ssn\", 4)) AS \"ssn\" "
            "FROM \"db\".\"users\" AS \"u\" "
            "UNION SELECT \"o\".\"ssn\" AS \"ssn\" FROM \"db\".\"orders\" AS \"o\"",
        )

    def test_spec_forms(self):
        self.assertEqual(
            self.mask(
                "SELECT u.ssn, u.name FROM db.users u",
                masks={
                    ("db.users", "ssn"): lambda column: exp.func("REDACT", column),
                    ("db.users", "name"): "'N/A'",
                },
            ),
            "SELECT REDACT(\"u\".\"ssn\") AS \"ssn\", 'N/A' AS \"name\" "
            "FROM \"db\".\"users\" AS \"u\"",
        )

    def test_unqualified_table_matches_by_suffix(self):
        self.assertEqual(
            self.mask("SELECT u.ssn FROM users u", schema={"users": {"ssn": "text"}}),
            "SELECT CONCAT('***', RIGHT(\"u\".\"ssn\", 4)) AS \"ssn\" FROM \"users\" AS \"u\"",
        )

    def test_single_source_inference_without_schema(self):
        self.assertEqual(
            self.mask("SELECT ssn FROM db.users", schema=None),
            "SELECT CONCAT('***', RIGHT(\"ssn\", 4)) AS \"ssn\" FROM \"db\".\"users\" AS \"users\"",
        )

    def test_unresolvable_sensitive_projection_raises(self):
        with self.assertRaises(MaskingError) as ctx:
            self.mask("SELECT ssn, x FROM db.users u JOIN db.orders o ON o.uid = u.id", schema=None)
        message = str(ctx.exception)
        self.assertIn("ssn", message)
        self.assertIn("Line: 1, Col: 10", message)

    def test_unexpandable_star_over_protected_table_raises(self):
        with self.assertRaises(MaskingError):
            self.mask("SELECT * FROM users", schema=None)

    def test_star_over_unprotected_table_is_untouched(self):
        self.assertEqual(
            self.mask("SELECT * FROM other", schema=None),
            'SELECT * FROM "other" AS "other"',
        )

    def test_non_query_raises(self):
        with self.assertRaises(MaskingError):
            mask_columns("users", self.masks)

    def test_original_ast_is_preserved(self):
        original = sqlglot.parse_one("SELECT u.ssn FROM db.users u")
        before = original.sql()
        masked = mask_columns(original, self.masks, schema=self.schema)
        self.assertEqual(original.sql(), before)
        self.assertNotEqual(masked.sql(), before)

    def test_dialect_normalization(self):
        self.assertEqual(
            mask_columns(
                "SELECT ssn FROM users",
                {("USERS", "SSN"): "MASK_IT(:source)"},
                dialect="snowflake",
            ).sql(dialect="snowflake"),
            'SELECT MASK_IT("SSN") AS "SSN" FROM "USERS" AS "USERS"',
        )

    def test_scalar_subquery_in_projection(self):
        self.assertEqual(
            self.mask("SELECT (SELECT u.ssn FROM db.users u LIMIT 1) AS ssn FROM db.orders o"),
            "SELECT CONCAT('***', RIGHT((SELECT \"u\".\"ssn\" AS \"ssn\" "
            "FROM \"db\".\"users\" AS \"u\" LIMIT 1), 4)) AS \"ssn\" "
            "FROM \"db\".\"orders\" AS \"o\"",
        )


if __name__ == "__main__":
    unittest.main()
