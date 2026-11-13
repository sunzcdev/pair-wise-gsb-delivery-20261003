import unittest

from sqlglot.dialect_compare import (
    CandidateStatus,
    compare_dialects,
)


class TestDialectCompare(unittest.TestCase):
    def test_identical_interpretations_share_a_group(self):
        report = compare_dialects("SELECT 1", ["mysql", "postgres", "duckdb"])

        self.assertEqual(len(report.statements), 1)
        statement = report.statements[0]
        self.assertEqual(
            [c.status for c in statement.candidates], [CandidateStatus.PARSED] * 3
        )
        self.assertEqual(len(statement.groups), 1)
        self.assertEqual(statement.groups[0].dialects, ["mysql", "postgres", "duckdb"])
        self.assertEqual(statement.differences, [])

    def test_quoting_difference_is_located(self):
        report = compare_dialects('SELECT "a" FROM t', ["mysql", "postgres", "duckdb"])
        statement = report.statements[0]

        self.assertEqual(len(statement.groups), 2)
        self.assertEqual(statement.groups[0].dialects, ["mysql"])
        self.assertEqual(statement.groups[1].dialects, ["postgres", "duckdb"])

        _, _, differences = statement.differences[0]
        self.assertTrue(differences)
        self.assertEqual(differences[0].location, "Select.expressions[0]")
        self.assertEqual(differences[0].source, "'a'")
        self.assertEqual(differences[0].target, '"a"')

    def test_array_subscript_offset_difference_is_located(self):
        report = compare_dialects("SELECT a[1] FROM t", ["mysql", "postgres"])
        statement = report.statements[0]

        self.assertEqual(len(statement.groups), 2)
        _, _, differences = statement.differences[0]
        locations = [d.location for d in differences]
        self.assertIn("Select.expressions[0].expressions[0].this", locations)

    def test_command_fallback_is_reported(self):
        report = compare_dialects("SHOW TABLES", ["mysql", "postgres"])
        statement = report.statements[0]
        statuses = {c.dialect: c.status for c in statement.candidates}

        self.assertEqual(statuses["mysql"], CandidateStatus.PARSED)
        self.assertEqual(statuses["postgres"], CandidateStatus.COMMAND_FALLBACK)
        self.assertEqual(len(statement.groups), 1)

    def test_failures_are_isolated_per_candidate(self):
        report = compare_dialects("SELECT 1", ["mysql", "not_a_dialect"])
        statement = report.statements[0]
        statuses = {c.dialect: c.status for c in statement.candidates}

        self.assertEqual(statuses["mysql"], CandidateStatus.PARSED)
        self.assertEqual(statuses["not_a_dialect"], CandidateStatus.FAILED)
        self.assertIn("Unknown dialect", statement.candidates[1].error)

    def test_statement_order_and_independent_conclusions(self):
        report = compare_dialects("SELECT 1; SELECT * FROM; SELECT 2", ["mysql", "postgres"])

        self.assertEqual([s.index for s in report.statements], [0, 1, 2])
        self.assertEqual(
            [s.sql for s in report.statements], ["SELECT 1", "SELECT * FROM", "SELECT 2"]
        )
        self.assertEqual(
            [c.status for c in report.statements[0].candidates],
            [CandidateStatus.PARSED, CandidateStatus.PARSED],
        )
        self.assertEqual(
            [c.status for c in report.statements[1].candidates],
            [CandidateStatus.FAILED, CandidateStatus.FAILED],
        )
        self.assertEqual(
            [c.status for c in report.statements[2].candidates],
            [CandidateStatus.PARSED, CandidateStatus.PARSED],
        )

    def test_render_mentions_limitations(self):
        report = compare_dialects("SELECT 1", ["mysql", "postgres"])
        rendered = report.render()

        self.assertIn("Statement 1: SELECT 1", rendered)
        self.assertIn("no query is executed", rendered)
        self.assertIn("do not uniquely identify the source dialect", rendered)
