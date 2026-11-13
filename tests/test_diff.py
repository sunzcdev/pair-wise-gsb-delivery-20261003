import unittest

from sqlglot import exp, parse_one
from sqlglot.diff import Insert, Keep, Move, Remove, Rename, Update, diff


def diff_delta_only(source, target, matchings=None, **kwargs):
    return diff(source, target, matchings=matchings, delta_only=True, **kwargs)


def diff_renames(source, target, **kwargs):
    return diff(source, target, delta_only=True, rename_sources=True, **kwargs)


def _edit_sql(edit):
    if isinstance(edit, Rename):
        return (edit.source.sql(), edit.target.sql())
    if isinstance(edit, Move):
        return (edit.source.sql(), edit.target.sql())
    if isinstance(edit, Update):
        return (edit.source.sql(), edit.target.sql())
    return edit.expression.sql()


class TestDiff(unittest.TestCase):
    def test_simple(self):
        self._validate_delta_only(
            diff_delta_only(parse_one("SELECT a + b"), parse_one("SELECT a - b")),
            [
                Remove(expression=parse_one("a + b")),  # the Add node
                Insert(expression=parse_one("a - b")),  # the Sub node
                Move(source=parse_one("a"), target=parse_one("a")),  # the `a` Column node
                Move(source=parse_one("b"), target=parse_one("b")),  # the `b` Column node
            ],
        )

        self._validate_delta_only(
            diff_delta_only(parse_one("SELECT a, b, c"), parse_one("SELECT a, c")),
            [
                Remove(expression=parse_one("b")),  # the Column node
            ],
        )

        self._validate_delta_only(
            diff_delta_only(parse_one("SELECT a, b"), parse_one("SELECT a, b, c")),
            [
                Insert(expression=parse_one("c")),  # the Column node
            ],
        )

        self._validate_delta_only(
            diff_delta_only(
                parse_one("SELECT a FROM table_one"),
                parse_one("SELECT a FROM table_two"),
            ),
            [
                Update(
                    source=exp.to_table("table_one", quoted=False),
                    target=exp.to_table("table_two", quoted=False),
                ),  # the Table node
            ],
        )

    def test_lambda(self):
        self._validate_delta_only(
            diff_delta_only(
                parse_one("SELECT a, b, c, x(a -> a)"), parse_one("SELECT a, b, c, x(b -> b)")
            ),
            [
                Update(
                    source=exp.Lambda(
                        this=exp.to_identifier("a"), expressions=[exp.to_identifier("a")]
                    ),
                    target=exp.Lambda(
                        this=exp.to_identifier("b"), expressions=[exp.to_identifier("b")]
                    ),
                ),
            ],
        )

    def test_udf(self):
        self._validate_delta_only(
            diff_delta_only(
                parse_one('SELECT a, b, "my.udf1"()'), parse_one('SELECT a, b, "my.udf2"()')
            ),
            [
                Insert(expression=parse_one('"my.udf2"()')),
                Remove(expression=parse_one('"my.udf1"()')),
            ],
        )
        self._validate_delta_only(
            diff_delta_only(
                parse_one('SELECT a, b, "my.udf"(x, y, z)'),
                parse_one('SELECT a, b, "my.udf"(x, y, w)'),
            ),
            [
                Insert(expression=exp.column("w")),
                Remove(expression=exp.column("z")),
            ],
        )

    def test_node_position_changed(self):
        expr_src = parse_one("SELECT a, b, c")
        expr_tgt = parse_one("SELECT c, a, b")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Move(source=expr_src.selects[2], target=expr_tgt.selects[0]),
            ],
        )

        expr_src = parse_one("SELECT a + b")
        expr_tgt = parse_one("SELECT b + a")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Move(source=expr_src.selects[0].left, target=expr_tgt.selects[0].right),
            ],
        )

        expr_src = parse_one("SELECT aaaa AND bbbb")
        expr_tgt = parse_one("SELECT bbbb AND aaaa")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Move(source=expr_src.selects[0].left, target=expr_tgt.selects[0].right),
            ],
        )

        expr_src = parse_one("SELECT aaaa OR bbbb OR cccc")
        expr_tgt = parse_one("SELECT cccc OR bbbb OR aaaa")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Move(source=expr_src.selects[0].left.left, target=expr_tgt.selects[0].right),
                Move(source=expr_src.selects[0].right, target=expr_tgt.selects[0].left.left),
            ],
        )

        expr_src = parse_one("SELECT a, b FROM t WHERE CONCAT('a', 'b') = 'ab'")
        expr_tgt = parse_one("SELECT a FROM t WHERE CONCAT('a', 'b', b) = 'ab'")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Move(source=expr_src.selects[1], target=expr_tgt.find(exp.Concat).expressions[-1]),
            ],
        )

        expr_src = parse_one("SELECT a as a, b as b FROM t WHERE CONCAT('a', 'b') = 'ab'")
        expr_tgt = parse_one("SELECT a as a FROM t WHERE CONCAT('a', 'b', b) = 'ab'")

        b_alias = expr_src.selects[1]

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Remove(expression=b_alias),
                Move(source=b_alias.this, target=expr_tgt.find(exp.Concat).expressions[-1]),
            ],
        )

    def test_cte(self):
        expr_src = """
            WITH
                cte1 AS (SELECT a, b, LOWER(c) AS c FROM table_one WHERE d = 'filter'),
                cte2 AS (SELECT d, e, f FROM table_two)
            SELECT a, b, d, e FROM cte1 JOIN cte2 ON f = c
        """
        expr_tgt = """
            WITH
                cte1 AS (SELECT a, b, c FROM table_one WHERE d = 'different_filter'),
                cte2 AS (SELECT d, e, f FROM table_two)
            SELECT a, b, d, e FROM cte1 JOIN cte2 ON f = c
        """

        self._validate_delta_only(
            diff_delta_only(parse_one(expr_src), parse_one(expr_tgt)),
            [
                Remove(expression=parse_one("LOWER(c) AS c")),  # the Alias node
                Remove(expression=parse_one("LOWER(c)")),  # the Lower node
                Remove(expression=parse_one("'filter'")),  # the Literal node
                Insert(expression=parse_one("'different_filter'")),  # the Literal node
                Move(source=parse_one("c"), target=parse_one("c")),  # the new Column c
            ],
        )

    def test_join(self):
        expr_src = parse_one("SELECT a, b FROM t1 LEFT JOIN t2 ON t1.key = t2.key")
        expr_tgt = parse_one("SELECT a, b FROM t1 RIGHT JOIN t2 ON t1.key = t2.key")

        src_join = expr_src.find(exp.Join)
        tgt_join = expr_tgt.find(exp.Join)

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Remove(expression=src_join),
                Insert(expression=tgt_join),
                Move(source=exp.to_table("t2"), target=exp.to_table("t2")),
                Move(source=src_join.args["on"], target=tgt_join.args["on"]),
            ],
        )

        expr_src = parse_one("SELECT a.x FROM a INNER JOIN b ON a.x = b.y LEFT JOIN c ON a.p = c.q")
        expr_tgt = parse_one("SELECT a.x FROM a inner JOIN b ON a.x = b.y left JOIN c ON a.p = c.q")

        self._validate_delta_only(diff_delta_only(expr_src, expr_tgt), [])

    def test_window_functions(self):
        expr_src = parse_one("SELECT ROW_NUMBER() OVER (PARTITION BY a ORDER BY b)")
        expr_tgt = parse_one("SELECT RANK() OVER (PARTITION BY a ORDER BY b)")

        self._validate_delta_only(diff_delta_only(expr_src, expr_src), [])

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Remove(expression=parse_one("ROW_NUMBER()")),
                Insert(expression=parse_one("RANK()")),
                Update(source=expr_src.selects[0], target=expr_tgt.selects[0]),
            ],
        )

        expr_src = parse_one("SELECT MAX(x) OVER (ORDER BY y) FROM z", "oracle")
        expr_tgt = parse_one("SELECT MAX(x) KEEP (DENSE_RANK LAST ORDER BY y) FROM z", "oracle")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [Update(source=expr_src.selects[0], target=expr_tgt.selects[0])],
        )

    def test_pre_matchings(self):
        expr_src = parse_one("SELECT 1")
        expr_tgt = parse_one("SELECT 1, 2, 3, 4")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Remove(expression=expr_src),
                Insert(expression=expr_tgt),
                Insert(expression=exp.Literal.number(2)),
                Insert(expression=exp.Literal.number(3)),
                Insert(expression=exp.Literal.number(4)),
                Move(source=exp.Literal.number(1), target=exp.Literal.number(1)),
            ],
        )

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt, matchings=[(expr_src, expr_tgt)]),
            [
                Insert(expression=exp.Literal.number(2)),
                Insert(expression=exp.Literal.number(3)),
                Insert(expression=exp.Literal.number(4)),
            ],
        )

        self._validate_delta_only(
            diff_delta_only(
                expr_src, expr_tgt, matchings=[(expr_src, expr_tgt), (expr_src, expr_tgt)]
            ),
            [
                Insert(expression=exp.Literal.number(2)),
                Insert(expression=exp.Literal.number(3)),
                Insert(expression=exp.Literal.number(4)),
            ],
        )

        expr_tgt.selects[0].replace(expr_src.selects[0])

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt, matchings=[(expr_src, expr_tgt)]),
            [
                Insert(expression=exp.Literal.number(2)),
                Insert(expression=exp.Literal.number(3)),
                Insert(expression=exp.Literal.number(4)),
            ],
        )

    def test_identifier(self):
        expr_src = parse_one("SELECT a FROM tbl")
        expr_tgt = parse_one("SELECT a, tbl.b from tbl")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Insert(expression=exp.to_column("tbl.b")),
            ],
        )

        expr_src = parse_one("SELECT 1 AS c1, 2 AS c2")
        expr_tgt = parse_one("SELECT 2 AS c1, 3 AS c2")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Remove(expression=exp.alias_(1, "c1")),
                Remove(expression=exp.Literal.number(1)),
                Insert(expression=exp.alias_(3, "c2")),
                Insert(expression=exp.Literal.number(3)),
                Update(source=exp.alias_(2, "c2"), target=exp.alias_(2, "c1")),
            ],
        )

    def test_dialect_aware_diff(self):
        from sqlglot.generator import logger

        with self.assertLogs(logger) as cm:
            # We want to assert there are no warnings, but the 'assertLogs' method does not support that.
            # Therefore, we are adding a dummy warning, and then we will assert it is the only warning.
            logger.warning("Dummy warning")

            expression = parse_one("SELECT foo FROM bar FOR UPDATE", dialect="oracle")
            self._validate_delta_only(
                diff_delta_only(expression, expression.copy(), dialect="oracle"), []
            )

        self.assertEqual(["WARNING:sqlglot:Dummy warning"], cm.output)

    def test_non_expression_leaf_delta(self):
        expr_src = parse_one("SELECT a UNION SELECT b")
        expr_tgt = parse_one("SELECT a UNION ALL SELECT b")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Update(source=expr_src, target=expr_tgt),
            ],
        )

        expr_src = parse_one("SELECT a FROM t ORDER BY b ASC")
        expr_tgt = parse_one("SELECT a FROM t ORDER BY b DESC")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Update(
                    source=expr_src.find(exp.Order).expressions[0],
                    target=expr_tgt.find(exp.Order).expressions[0],
                ),
            ],
        )

        expr_src = parse_one("SELECT a, b FROM t ORDER BY c ASC")
        expr_tgt = parse_one("SELECT b, a FROM t ORDER BY c DESC")

        self._validate_delta_only(
            diff_delta_only(expr_src, expr_tgt),
            [
                Update(
                    source=expr_src.find(exp.Order).expressions[0],
                    target=expr_tgt.find(exp.Order).expressions[0],
                ),
                Move(source=expr_src.selects[0], target=expr_tgt.selects[1]),
            ],
        )

    def test_none_args_are_not_treated_as_leaves(self):
        expr_src = exp.Column(
            this=exp.to_identifier("b"), table=exp.to_identifier("a"), db=None, catalog=None
        )
        expr_tgt = exp.Column(this=exp.to_identifier("b"), table=exp.to_identifier("a"))

        self.assertEqual(set(expr_src.args), {"this", "table", "db", "catalog"})
        self.assertEqual(set(expr_tgt.args), {"this", "table"})

        self._validate_delta_only(diff_delta_only(expr_src, expr_tgt), [])

    def test_edit_script_is_deterministic(self):
        # The unmatched node sets and the matching set are keyed by id(), so iterating them
        # directly orders the edit script by object address. Diffing two freshly parsed copies
        # of the same pair must therefore produce the same sequence of edits, not merely the
        # same set of them.
        sql_pairs = [
            ("SELECT a, b, c FROM t WHERE x > 1", "SELECT a, c, d FROM t WHERE x < 2"),
            (
                "SELECT x + 1 AS p, y + 2 AS q FROM t1 JOIN t2 ON t1.id = t2.id",
                "SELECT x * 3 AS p, y + 2 AS q FROM t1 JOIN t2 ON t1.id = t2.fid",
            ),
            (
                "WITH c AS (SELECT a FROM t) SELECT a FROM c",
                "WITH c AS (SELECT b FROM t) SELECT b FROM c",
            ),
        ]

        for source_sql, target_sql in sql_pairs:
            with self.subTest(source_sql=source_sql):
                scripts = []
                for _ in range(3):
                    edits = diff_delta_only(parse_one(source_sql), parse_one(target_sql))
                    scripts.append([(type(e).__name__, _edit_sql(e)) for e in edits])

                self.assertEqual(scripts[0], scripts[1])
                self.assertEqual(scripts[1], scripts[2])

    def test_comments_do_not_affect_diff(self):
        expr_src = parse_one("select a from tbl")
        expr_tgt = parse_one("select a from tbl -- this is comment")

        self.assertEqual(expr_tgt.args["from_"].this.comments, [" this is comment"])
        self._validate_delta_only(diff_delta_only(expr_src, expr_tgt), [])

    def test_rename_sources_disabled_by_default(self):
        expr_src = parse_one("SELECT a.x, a.y FROM tbl a")
        expr_tgt = parse_one("SELECT b.x, b.y FROM tbl b")

        default_edits = diff_delta_only(expr_src, expr_tgt)
        self.assertTrue(default_edits)
        self.assertFalse(any(isinstance(edit, Rename) for edit in default_edits))
        self.assertTrue(
            any(isinstance(edit, (Insert, Remove)) for edit in default_edits)
        )

    def test_rename_table_alias(self):
        expr_src = parse_one("SELECT a.x, a.y FROM tbl a")
        expr_tgt = parse_one("SELECT b.x, b.y FROM tbl b")

        rename = Rename(
            source=expr_src.find(exp.TableAlias),
            target=expr_tgt.find(exp.TableAlias),
            old=(
                (expr_src.find(exp.TableAlias), None),
                (expr_src.selects[0], expr_src.selects[0].args["table"]),
                (expr_src.selects[1], expr_src.selects[1].args["table"]),
                (expr_src.find(exp.Table), None),
            ),
            new=(
                (expr_tgt.find(exp.TableAlias), None),
                (expr_tgt.selects[0], expr_tgt.selects[0].args["table"]),
                (expr_tgt.selects[1], expr_tgt.selects[1].args["table"]),
                (expr_tgt.find(exp.Table), None),
            ),
        )
        self._validate_delta_only(diff_renames(expr_src, expr_tgt), [rename])

        # Every merged node remains traceable back to a node in its respective tree.
        source_ids = {id(node) for node in expr_src.walk()}
        target_ids = {id(node) for node in expr_tgt.walk()}
        for definition, reference in rename.old:
            self.assertIn(id(definition), source_ids)
            if reference is not None:
                self.assertIn(id(reference), source_ids)
        for definition, reference in rename.new:
            self.assertIn(id(definition), target_ids)
            if reference is not None:
                self.assertIn(id(reference), target_ids)

    def test_rename_keeps_real_expression_changes(self):
        expr_src = parse_one("SELECT a.x + 1 FROM tbl a")
        expr_tgt = parse_one("SELECT b.x + 2 FROM tbl b")

        self._validate_delta_only(
            diff_renames(expr_src, expr_tgt),
            [
                Remove(expression=exp.Literal.number(1)),
                Insert(expression=exp.Literal.number(2)),
                Rename(
                    source=expr_src.find(exp.TableAlias),
                    target=expr_tgt.find(exp.TableAlias),
                    old=(
                        (expr_src.find(exp.TableAlias), None),
                        (expr_src.selects[0].this, expr_src.selects[0].this.args["table"]),
                        (expr_src.find(exp.Table), None),
                    ),
                    new=(
                        (expr_tgt.find(exp.TableAlias), None),
                        (expr_tgt.selects[0].this, expr_tgt.selects[0].this.args["table"]),
                        (expr_tgt.find(exp.Table), None),
                    ),
                ),
            ],
        )

    def test_rename_partial_reference_update_is_not_merged(self):
        expr_src = parse_one("SELECT a.x, a.y FROM tbl a")
        expr_tgt = parse_one("SELECT b.x, a.y FROM tbl b")

        edits = diff_renames(expr_src, expr_tgt)
        self.assertFalse(any(isinstance(edit, Rename) for edit in edits))
        self.assertTrue(any(isinstance(edit, Remove) for edit in edits))
        self.assertTrue(any(isinstance(edit, Insert) for edit in edits))
        # The reference that kept the old qualifier is still represented as a move.
        self.assertTrue(
            any(
                isinstance(edit, Move)
                and edit.source == expr_src.selects[1]
                and edit.target == expr_tgt.selects[1]
                for edit in edits
            )
        )

    def test_rename_does_not_conflate_physical_table_change(self):
        expr_src = parse_one("SELECT a.x FROM tbl a")
        expr_tgt = parse_one("SELECT a.x FROM other a")

        edits = diff_renames(expr_src, expr_tgt)
        self.assertFalse(any(isinstance(edit, Rename) for edit in edits))
        self._validate_delta_only(
            edits,
            [
                Update(
                    source=expr_src.find(exp.Table),
                    target=expr_tgt.find(exp.Table),
                )
            ],
        )

    def test_rename_is_scoped(self):
        expr_src = parse_one(
            "SELECT a.x FROM (SELECT 1 AS x) a "
            "UNION ALL SELECT a.y FROM (SELECT 2 AS y) a"
        )
        expr_tgt = parse_one(
            "SELECT c.x FROM (SELECT 1 AS x) c "
            "UNION ALL SELECT c.y FROM (SELECT 2 AS y) c"
        )

        edits = diff_renames(expr_src, expr_tgt)
        self.assertEqual([type(edit) for edit in edits], [Rename, Rename])
        for edit in edits:
            self.assertEqual(edit.source.name, "a")
            self.assertEqual(edit.target.name, "c")

        # Renaming the alias in only one of the two subqueries yields a single rename, while the
        # untouched subquery produces no edits at all.
        expr_tgt = parse_one(
            "SELECT c.x FROM (SELECT 1 AS x) c "
            "UNION ALL SELECT a.y FROM (SELECT 2 AS y) a"
        )
        edits = diff_renames(expr_src, expr_tgt)
        self.assertEqual([type(edit) for edit in edits], [Rename])

    def test_rename_nested_subquery(self):
        expr_src = parse_one("SELECT x FROM (SELECT a.x FROM t a) s")
        expr_tgt = parse_one("SELECT x FROM (SELECT b.x FROM t b) s")
        src_column = next(c for c in expr_src.find_all(exp.Column) if c.args.get("table"))
        tgt_column = next(c for c in expr_tgt.find_all(exp.Column) if c.args.get("table"))
        src_table = next(t for t in expr_src.find_all(exp.Table) if t.alias == "a")
        tgt_table = next(t for t in expr_tgt.find_all(exp.Table) if t.alias == "b")

        self._validate_delta_only(
            diff_renames(expr_src, expr_tgt),
            [
                Rename(
                    source=src_table.args["alias"],
                    target=tgt_table.args["alias"],
                    old=(
                        (src_table.args["alias"], None),
                        (
                            src_column,
                            src_column.args["table"],
                        ),
                        (src_table, None),
                    ),
                    new=(
                        (tgt_table.args["alias"], None),
                        (
                            tgt_column,
                            tgt_column.args["table"],
                        ),
                        (tgt_table, None),
                    ),
                )
            ],
        )

    def test_rename_derived_table_and_cte(self):
        expr_src = parse_one("SELECT s.x FROM (SELECT x FROM t) s")
        expr_tgt = parse_one("SELECT q.x FROM (SELECT x FROM t) q")
        self.assertEqual(
            [type(edit) for edit in diff_renames(expr_src, expr_tgt)], [Rename]
        )

        expr_src = parse_one("WITH c AS (SELECT x FROM t) SELECT c.x FROM c")
        expr_tgt = parse_one("WITH d AS (SELECT x FROM t) SELECT d.x FROM d")
        edits = diff_renames(expr_src, expr_tgt)
        self.assertEqual([type(edit) for edit in edits], [Rename])
        self.assertEqual(edits[0].source.name, "c")
        self.assertEqual(edits[0].target.name, "d")

        # A change inside the CTE body must still be reported alongside the rename.
        expr_tgt = parse_one("WITH d AS (SELECT y FROM t) SELECT d.x FROM d")
        edits = diff_renames(expr_src, expr_tgt)
        self.assertEqual(sum(isinstance(edit, Rename) for edit in edits), 1)
        self.assertTrue(any(isinstance(edit, (Insert, Remove)) for edit in edits))

    def test_rename_direction_reverses_with_inputs(self):
        expr_src = parse_one("SELECT a.x, a.y FROM tbl a")
        expr_tgt = parse_one("SELECT b.x, b.y FROM tbl b")

        forward = diff_renames(expr_src, expr_tgt)
        reverse = diff_renames(expr_tgt, expr_src)

        self.assertEqual(len(forward), 1)
        self.assertEqual(len(reverse), 1)
        self.assertEqual(forward[0].source, reverse[0].target)
        self.assertEqual(forward[0].target, reverse[0].source)

    def test_rename_keeps_keep_edits_outside_delta_mode(self):
        expr_src = parse_one("SELECT a.x FROM tbl a")
        expr_tgt = parse_one("SELECT b.x FROM tbl b")

        edits = diff(expr_src, expr_tgt, rename_sources=True)
        self.assertTrue(any(isinstance(edit, Rename) for edit in edits))
        self.assertTrue(any(isinstance(edit, Keep) for edit in edits))

    def test_rename_is_deterministic(self):
        source_sql = "SELECT a.x + 1 AS p, b.y FROM t1 a JOIN t2 b ON a.id = b.id"
        target_sql = "SELECT z.x + 2 AS p, q.y FROM t1 z JOIN t2 q ON z.id = q.id"

        scripts = []
        for _ in range(3):
            edits = diff_renames(parse_one(source_sql), parse_one(target_sql))
            scripts.append([(type(edit).__name__, _edit_sql(edit)) for edit in edits])

        self.assertEqual(scripts[0], scripts[1])
        self.assertEqual(scripts[1], scripts[2])

    def _validate_delta_only(self, actual_delta, expected_delta):
        self.assertEqual(set(actual_delta), set(expected_delta))
