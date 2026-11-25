# test/sql/test_lineage.py
# Copyright (C) 2005-2026 the SQLAlchemy authors and contributors
# <see AUTHORS file>
#
# This module is part of SQLAlchemy and is released under
# the MIT License: https://www.opensource.org/licenses/mit-license.php

"""Tests for the column source / lineage introspection view
(:mod:`sqlalchemy.sql.lineage`)."""

from sqlalchemy import bindparam
from sqlalchemy import Column
from sqlalchemy import except_
from sqlalchemy import func
from sqlalchemy import Integer
from sqlalchemy import intersect
from sqlalchemy import literal
from sqlalchemy import literal_column
from sqlalchemy import MetaData
from sqlalchemy import select
from sqlalchemy import String
from sqlalchemy import Table
from sqlalchemy import testing
from sqlalchemy import union
from sqlalchemy import union_all
from sqlalchemy.sql import column
from sqlalchemy.sql import table
from sqlalchemy.sql.lineage import column_lineage
from sqlalchemy.sql.lineage import lineage_for_column
from sqlalchemy.testing import assert_raises_message
from sqlalchemy.testing import eq_
from sqlalchemy.testing import fixtures
from sqlalchemy.testing import is_
from sqlalchemy.testing import is_false
from sqlalchemy.testing import is_true


class LineageFixture(fixtures.TestBase):
    """Introspection-only fixture; statements are never executed here."""

    def setup_test(self):
        self.metadata = MetaData()
        self.a = Table(
            "a",
            self.metadata,
            Column("id", Integer, primary_key=True),
            Column("x", Integer),
            Column("y", Integer),
            Column("name", String(30)),
        )
        self.b = Table(
            "b",
            self.metadata,
            Column("id", Integer, primary_key=True),
            Column("x", Integer),
            Column("y", Integer),
            Column("name", String(30)),
        )
        # same alias name on both sides of the union
        self.a_t = self.a.alias("t")
        self.b_t = self.b.alias("t")

    def _union(self):
        return union_all(
            select(
                self.a_t.c.id,
                self.a_t.c.x,
                self.a_t.c.y,
                self.a_t.c.name,
            ),
            select(
                self.b_t.c.id,
                # deliberately cross-wired physical columns under same names
                self.b_t.c.y,
                self.b_t.c.x,
                self.b_t.c.name,
            ),
        )

    def _outer(self):
        cte = self._union().cte("u_cte")
        return cte, select(
            cte.c.name,
            cte.c.y,
            cte.c.x,
            cte.c.id,
            (cte.c.x + cte.c.y).label("total"),
            literal(7).label("seven"),
        )


class ColumnLineageReportTest(LineageFixture):
    def test_output_positions_and_order(self):
        _, outer = self._outer()
        report = column_lineage(outer)

        eq_(
            [col.name for col in report],
            ["name", "y", "x", "id", "total", "seven"],
        )
        eq_(len(report), 6)
        eq_([col.index for col in report], [0, 1, 2, 3, 4, 5])
        # lookup by name and by position
        is_(report[2], report["x"])

    def test_report_keeps_original_statement(self):
        _, outer = self._outer()
        report = column_lineage(outer)
        is_(report.statement, outer)

    def test_introspection_does_not_modify_statement(self):
        _, outer = self._outer()
        before = str(outer)
        column_lineage(outer)
        eq_(str(outer), before)

    def test_union_has_two_branch_sources_per_position(self):
        _, outer = self._outer()
        report = column_lineage(outer)
        for name in ("name", "y", "x", "id"):
            eq_(
                len(report[name].sources),
                2,
                f"{name} should have one source per union branch",
            )

    def test_branch_identity_and_cross_wired_columns(self):
        _, outer = self._outer()
        report = column_lineage(outer)

        def physical(col_name):
            return {
                branch.branch_chain: [
                    origin.describe()
                    for origin in branch.origins
                    if origin.kind == "column"
                ]
                for branch in report[col_name].sources
            }

        # the outer select is branch 0; its two union sides are the trailing
        # 0 / 1 in the branch chain.
        # position 'y': left branch selects a.y, right branch selects b.x
        eq_(physical("y"), {(0, 0): ["a.y"], (0, 1): ["b.x"]})
        # position 'x': left branch selects a.x, right branch selects b.y
        eq_(physical("x"), {(0, 0): ["a.x"], (0, 1): ["b.y"]})

        is_true(report["x"].is_ambiguous)
        is_true(report["y"].is_ambiguous)

    def test_same_named_columns_keep_distinct_identity(self):
        # both aliases are named "t" and both expose "name"; the physical
        # table behind each side must still be reported separately
        _, outer = self._outer()
        report = column_lineage(outer)
        origins = {
            branch.branch_chain: {
                origin.describe() for origin in branch.origins
            }
            for branch in report["name"].sources
        }
        eq_(origins, {(0, 0): {"a.name"}, (0, 1): {"b.name"}})

    def test_expression_reports_self_and_leaves_per_branch(self):
        _, outer = self._outer()
        total = column_lineage(outer)["total"]

        for branch in total.sources:
            kinds = [origin.kind for origin in branch.origins]
            is_true("expression" in kinds)
            eq_(kinds.count("column"), 2)

        leaves = {
            branch.branch_chain: sorted(
                origin.table
                for origin in branch.origins
                if origin.kind == "column"
            )
            for branch in total.sources
        }
        eq_(leaves, {(0, 0): ["a", "a"], (0, 1): ["b", "b"]})

    def test_literal_origin(self):
        _, outer = self._outer()
        seven = column_lineage(outer)["seven"]
        eq_(len(seven.sources), 1)
        origin = seven.sources[0].origins[0]
        eq_(origin.kind, "literal")
        eq_(origin.value, 7)
        is_false(seven.is_ambiguous)

    def test_parameter_and_tableless_column_are_explained(self):
        stmt = select(
            self.a.c.id,
            literal_column("'raw'").label("raw"),
            bindparam("bp").label("bp"),
        )
        report = column_lineage(stmt)
        eq_(report["raw"].sources[0].origins[0].kind, "expression")
        eq_(report["bp"].sources[0].origins[0].kind, "parameter")

    def test_cte_keeps_references_to_both_branches(self):
        cte = self._union().cte("u_cte")
        lineage = lineage_for_column(cte, cte.c.x)
        eq_(lineage.name, "x")
        eq_(len(lineage.sources), 2)
        eq_(tuple(lineage.path), ("u_cte",))

    def test_subquery_keeps_references(self):
        subq = self._union().subquery("u_sq")
        report = column_lineage(select(subq.c.x, subq.c.y))
        eq_(len(report[0].sources), 2)
        is_true("u_sq" in report[0].path)

    def test_plain_select_and_join_single_branch(self):
        join = self.a.join(self.b, self.a.c.id == self.b.c.id)
        report = column_lineage(
            select(self.a.c.id, self.b.c.x).select_from(join)
        )
        eq_(len(report[0].sources), 1)
        eq_(report[0].sources[0].origins[0].describe(), "a.id")
        eq_(report[1].sources[0].origins[0].describe(), "b.x")

    def test_all_set_operations_report_branches(self):
        for op in (union, union_all, intersect, except_):
            compound = op(
                select(self.a_t.c.id, self.a_t.c.x),
                select(self.b_t.c.id, self.b_t.c.x),
            )
            report = column_lineage(compound.cte("c"))
            eq_(len(report[1].sources), 2, op.__name__)

    def test_three_way_union(self):
        c = Table(
            "c",
            self.metadata,
            Column("id", Integer, primary_key=True),
            Column("x", Integer),
            Column("y", Integer),
        )
        c_t = c.alias("t")
        compound = union_all(
            select(self.a_t.c.id, self.a_t.c.x),
            select(self.b_t.c.id, self.b_t.c.y),
            select(c_t.c.id, c_t.c.x + c_t.c.y),
        )
        report = column_lineage(compound)
        eq_(len(report[1].sources), 3)
        third = report[1].sources[2]
        eq_(
            sorted(
                origin.describe()
                for origin in third.origins
                if origin.kind == "column"
            ),
            ["c.x", "c.y"],
        )

    def test_nested_compound_branch_chains(self):
        inner = union_all(
            select(self.a_t.c.id, self.a_t.c.x),
            select(self.b_t.c.id, self.b_t.c.x),
        )
        outer_compound = union_all(
            inner, select(self.a_t.c.id, self.a_t.c.y)
        )
        report = column_lineage(outer_compound.cte("nc"))
        chains = {branch.branch_chain for branch in report[1].sources}
        eq_(chains, {(0, 0), (0, 1), (1,)})

    def test_nested_cte_reaches_physical_column(self):
        inner = select(
            self.a_t.c.id, (self.a_t.c.x * 2).label("dbl")
        ).cte("inner")
        outer = select(inner).cte("outer")
        report = column_lineage(select(outer.c.dbl))
        descriptions = [
            origin.describe()
            for branch in report[0].sources
            for origin in branch.origins
        ]
        is_true("a.x" in descriptions)
        is_true("inner" in report[0].path)
        is_true("outer" in report[0].path)

    def test_aliased_cte(self):
        base = self._union().cte("base")
        aliased = base.alias("al")
        report = column_lineage(select(aliased.c.x))
        eq_(len(report[0].sources), 2)
        is_true("al" in report[0].path)

    def test_function_expression_leaves(self):
        stmt = select(
            self.a_t.c.id,
            func.coalesce(self.a_t.c.x, self.a_t.c.y, 0).label("co"),
        )
        branch = column_lineage(stmt)["co"].sources[0]
        columns = {
            origin.describe()
            for origin in branch.origins
            if origin.kind == "column"
        }
        eq_(columns, {"a.x", "a.y"})
        is_true(
            any(origin.kind == "literal" for origin in branch.origins)
        )

    def test_scalar_subquery_leaves(self):
        scalar = (
            select(func.count())
            .select_from(self.b)
            .where(self.b.c.id == self.a.c.id)
            .scalar_subquery()
            .label("cnt")
        )
        report = column_lineage(select(self.a.c.id, scalar))
        descriptions = [
            origin.describe()
            for branch in report[1].sources
            for origin in branch.origins
        ]
        is_true(any("b.id" in d for d in descriptions))

    def test_lightweight_table(self):
        light = table("light", column("k", Integer))
        report = column_lineage(select(light.c.k))
        eq_(
            report[0].sources[0].origins[0].describe(),
            "light.k",
        )

    def test_lineage_for_column_rejects_foreign_column(self):
        stmt = select(self.a.c.id)
        assert_raises_message(
            ValueError,
            "is not an exported column",
            lineage_for_column,
            stmt,
            self.b.c.x,
        )


class LineageExecutionTest(fixtures.TablesTest):
    """The introspection must not interfere with executing the very same
    statement, and result row mapping must keep working normally."""

    __backend__ = True

    @classmethod
    def define_tables(cls, metadata):
        Table(
            "a",
            metadata,
            Column("id", Integer, primary_key=True),
            Column("x", Integer),
            Column("y", Integer),
            Column("name", String(30)),
        )
        Table(
            "b",
            metadata,
            Column("id", Integer, primary_key=True),
            Column("x", Integer),
            Column("y", Integer),
            Column("name", String(30)),
        )

    @classmethod
    def insert_data(cls, connection):
        a, b = cls.tables.a, cls.tables.b
        connection.execute(
            a.insert(),
            [
                {"id": 1, "x": 10, "y": 100, "name": "a1"},
                {"id": 2, "x": 20, "y": 200, "name": "a2"},
            ],
        )
        connection.execute(
            b.insert(),
            [{"id": 1, "x": 30, "y": 300, "name": "b1"}],
        )

    def _statement(self):
        a = self.tables.a.alias("t")
        b = self.tables.b.alias("t")
        cte = union_all(
            select(a.c.id, a.c.x, a.c.y, a.c.name),
            select(b.c.id, b.c.y, b.c.x, b.c.name),
        ).cte("u_cte")
        return select(
            cte.c.name,
            cte.c.y,
            cte.c.x,
            cte.c.id,
            (cte.c.x + cte.c.y).label("total"),
        ).order_by("name")

    def test_report_then_execute_same_statement(self, connection):
        stmt = self._statement()

        # introspect first ...
        report = column_lineage(stmt)
        eq_(
            [
                origin.describe()
                for origin in report["y"].sources[0].origins
            ],
            ["a.y"],
        )
        eq_(
            [
                origin.describe()
                for origin in report["y"].sources[1].origins
            ],
            ["b.x"],
        )

        # ... then execute the unchanged statement and read rows normally
        result = connection.execute(stmt)
        eq_(result.keys(), ["name", "y", "x", "id", "total"])
        rows = result.all()

        mapping = {row.name: row for row in rows}
        # the data values line up with the lineage report
        eq_((mapping["a1"].y, mapping["a1"].x), (100, 10))
        eq_((mapping["b1"].y, mapping["b1"].x), (30, 300))
        eq_(mapping["b1"].total, 330)

        # positional and mapping access both work as usual
        eq_(
            [dict(row._mapping) for row in rows],
            [
                {
                    "name": "a1",
                    "y": 100,
                    "x": 10,
                    "id": 1,
                    "total": 110,
                },
                {
                    "name": "a2",
                    "y": 200,
                    "x": 20,
                    "id": 2,
                    "total": 220,
                },
                {
                    "name": "b1",
                    "y": 30,
                    "x": 300,
                    "id": 1,
                    "total": 330,
                },
            ],
        )
