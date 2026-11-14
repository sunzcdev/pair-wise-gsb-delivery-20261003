import unittest

import sqlglot
from sqlglot import exp
from sqlglot.optimizer.pushdown_predicates import pushdown_predicates
from sqlglot.optimizer.qualify import qualify
from sqlglot.serde import dump, load
from sqlglot.source_map import (
    SOURCE_META_KEY,
    SourceMapKind,
    SourceRange,
    build_source_map,
    merge_ranges,
)

source_map = sqlglot.get_source_map


def ranges(node):
    return [(r.start, r.end) for r in source_map(node).ranges]


def fragments(sql, node):
    return [sql[r.start : r.end] for r in source_map(node).ranges]


class TestSourceMap(unittest.TestCase):
    def test_merge_ranges_preserves_gaps(self):
        merged = merge_ranges(
            [
                SourceRange(0, 2, 1, 1),
                SourceRange(2, 4, 1, 3),  # touches the previous range
                SourceRange(10, 12, 1, 11),  # real gap 4..10 must survive
            ]
        )
        self.assertEqual([(r.start, r.end) for r in merged], [(0, 4), (10, 12)])

        nested = merge_ranges([SourceRange(5, 10, 1, 6), SourceRange(6, 8, 1, 7)])
        self.assertEqual([(r.start, r.end) for r in nested], [(5, 10)])

    def test_identity_single_fragment(self):
        sql = "SELECT a FROM t"
        tree = sqlglot.parse_one(sql, track_sources=True)

        column = tree.find(exp.Column)
        self.assertIs(source_map(column).kind, SourceMapKind.IDENTITY)
        self.assertEqual(fragments(sql, column), ["a"])

        table = tree.find(exp.Table)
        self.assertIs(source_map(table).kind, SourceMapKind.IDENTITY)
        self.assertEqual(fragments(sql, table), ["t"])

    def test_composite_keeps_non_contiguous_fragments(self):
        sql = "SELECT a + 1 FROM t"
        tree = sqlglot.parse_one(sql, track_sources=True)

        add = tree.find(exp.Add)
        self.assertIs(source_map(add).kind, SourceMapKind.COMPOSITE)
        # The " + " gap must not be swallowed into one big range.
        self.assertEqual(ranges(add), [(7, 8), (11, 12)])
        self.assertEqual(fragments(sql, add), ["a", "1"])

        whole = source_map(tree)
        self.assertIs(whole.kind, SourceMapKind.COMPOSITE)
        self.assertTrue(all(r.end <= nxt.start for r, nxt in zip(whole.ranges, whole.ranges[1:])))

    def test_synthetic_node_has_no_source(self):
        sql = "SELECT a FROM t"
        tree = sqlglot.parse_one(sql, track_sources=True)

        # A node built programmatically has no direct source and no tracked descendants.
        synthetic = exp.true()
        self.assertIs(source_map(synthetic).kind, SourceMapKind.SYNTHETIC)
        self.assertEqual(source_map(synthetic).ranges, ())
        self.assertIsNone(source_map(synthetic).start)
        self.assertIsNone(source_map(synthetic).end)

    def test_inserted_node_does_not_inherit_neighbor_position(self):
        sql = "SELECT a + 1 AS y, y * 2 AS z FROM t"
        tree = qualify(
            sqlglot.parse_one(sql, track_sources=True), schema={"t": {"a": "int"}}
        )

        # `y` in the second projection is expanded into a copy of the `a + 1` expression.
        expanded_add = tree.selects[1].find(exp.Add)
        self.assertIs(source_map(expanded_add).kind, SourceMapKind.COMPOSITE)
        self.assertEqual(fragments(sql, expanded_add), ["a", "1"])

        # Qualification introduces a fresh table identifier; it must be synthetic rather
        # than borrowing the adjacent column's source range.
        qualified_identifiers = [
            node
            for node in tree.find_all(exp.Identifier)
            if node.name == "t" and source_map(node).kind is SourceMapKind.SYNTHETIC
        ]
        self.assertTrue(qualified_identifiers)

    def test_predicate_pushdown_mapping(self):
        sql = "SELECT y.a AS a FROM (SELECT x.a AS a FROM x AS x) AS y WHERE y.a = 1"
        tree = pushdown_predicates(
            qualify(sqlglot.parse_one(sql, track_sources=True), schema={"x": {"a": "int"}})
        )

        outer_where = tree.args["where"]
        # The outer predicate was replaced by TRUE, which has no source.
        self.assertIs(source_map(outer_where).kind, SourceMapKind.SYNTHETIC)
        self.assertIs(source_map(outer_where.this).kind, SourceMapKind.SYNTHETIC)

        inner_where = tree.find(exp.Subquery).find(exp.Where)
        self.assertIs(source_map(inner_where).kind, SourceMapKind.COMPOSITE)
        self.assertEqual(fragments(sql, inner_where), ["x", "a", "1"])

    def test_unicode_comment_and_multi_statement_offsets(self):
        sql = "SELECT café FROM t; /* 注释 c */ SELECT 🍰 FROM u"
        statements = [s for s in sqlglot.parse(sql, track_sources=True) if s.expressions]
        self.assertEqual(len(statements), 2)

        # Offsets are Unicode code points and global across the whole multi-statement input.
        for statement in statements:
            for node in statement.walk():
                for rng in source_map(node).ranges:
                    self.assertTrue(sql[rng.start : rng.end])
                    self.assertTrue(rng.end > rng.start)

        cafe = next(
            node
            for node in statements[0].walk()
            if node.meta_get(SOURCE_META_KEY) is not None and "café" in node.sql()
        )
        self.assertEqual(fragments(sql, cafe), ["café"])

        emoji = next(
            node
            for node in statements[1].walk()
            if node.meta_get(SOURCE_META_KEY) is not None and "🍰" in node.sql()
        )
        self.assertEqual(fragments(sql, emoji), ["🍰"])
        # A single astral character occupies one code point offset.
        self.assertEqual(source_map(emoji).ranges[0].length, 1)

    def test_copy_preserves_mapping_with_new_identity(self):
        sql = "SELECT a + 1 FROM t"
        tree = sqlglot.parse_one(sql, track_sources=True)
        copied = tree.copy()

        original_map = build_source_map(tree)
        copied_map = build_source_map(copied)
        self.assertTrue(set(original_map).isdisjoint(set(copied_map)))

        original_add = tree.find(exp.Add)
        copied_add = copied.find(exp.Add)
        self.assertEqual(ranges(original_add), ranges(copied_add))
        self.assertIs(source_map(copied_add).kind, SourceMapKind.COMPOSITE)

    def test_serde_round_trip(self):
        sql = "SELECT a + 1 FROM t"
        tree = sqlglot.parse_one(sql, track_sources=True)
        restored = load(dump(tree))

        add = restored.find(exp.Add)
        self.assertIs(source_map(add).kind, SourceMapKind.COMPOSITE)
        self.assertEqual(ranges(add), [(7, 8), (11, 12)])

    def test_tracking_disabled_has_no_meta_overhead(self):
        tree = sqlglot.parse_one("SELECT a + 1 FROM t WHERE x = 2")
        for node in tree.walk():
            self.assertIsNone(node.meta_get(SOURCE_META_KEY))
            if node._meta is not None:
                self.assertNotIn(SOURCE_META_KEY, node._meta)

        # Without tracking, every node is reported synthetic since no source is recorded.
        self.assertIs(source_map(tree).kind, SourceMapKind.SYNTHETIC)

    def test_build_source_map_keys_and_line_col(self):
        sql = "SELECT a FROM t"
        tree = sqlglot.parse_one(sql, track_sources=True)
        mapping = build_source_map(tree)

        self.assertEqual(set(mapping), {id(node) for node in tree.walk()})
        column = tree.find(exp.Column)
        rng = source_map(column).ranges[0]
        self.assertEqual((rng.line, rng.col), (1, 8))
        self.assertEqual(mapping[id(column)].expression, column)


if __name__ == "__main__":
    unittest.main()
