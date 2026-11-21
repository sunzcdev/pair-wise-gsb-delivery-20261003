import datetime
import io

from sqlalchemy import Column
from sqlalchemy import create_engine
from sqlalchemy import Date
from sqlalchemy import Integer
from sqlalchemy import MetaData
from sqlalchemy import String
from sqlalchemy import Table
from sqlalchemy import text
from sqlalchemy.exc import CompileError
from sqlalchemy.sql import column
from sqlalchemy.sql import table
from sqlalchemy.types import TypeEngine

from alembic import op
from alembic.migration import MigrationContext
from alembic.testing import assert_raises_message
from alembic.testing import config
from alembic.testing import eq_
from alembic.testing.fixtures import op_fixture
from alembic.testing.fixtures import TestBase


class BulkInsertTest(TestBase):
    def _table_fixture(self, dialect, as_sql):
        context = op_fixture(dialect, as_sql)
        t1 = table(
            "ins_table",
            column("id", Integer),
            column("v1", String()),
            column("v2", String()),
        )
        return context, t1

    def _big_t_table_fixture(self, dialect, as_sql):
        context = op_fixture(dialect, as_sql)
        t1 = Table(
            "ins_table",
            MetaData(),
            Column("id", Integer, primary_key=True),
            Column("v1", String()),
            Column("v2", String()),
        )
        return context, t1

    def _test_bulk_insert(self, dialect, as_sql):
        context, t1 = self._table_fixture(dialect, as_sql)

        op.bulk_insert(
            t1,
            [
                {"id": 1, "v1": "row v1", "v2": "row v5"},
                {"id": 2, "v1": "row v2", "v2": "row v6"},
                {"id": 3, "v1": "row v3", "v2": "row v7"},
                {"id": 4, "v1": "row v4", "v2": "row v8"},
            ],
        )
        return context

    def _test_bulk_insert_single(self, dialect, as_sql):
        context, t1 = self._table_fixture(dialect, as_sql)

        op.bulk_insert(t1, [{"id": 1, "v1": "row v1", "v2": "row v5"}])
        return context

    def _test_bulk_insert_single_bigt(self, dialect, as_sql):
        context, t1 = self._big_t_table_fixture(dialect, as_sql)

        op.bulk_insert(t1, [{"id": 1, "v1": "row v1", "v2": "row v5"}])
        return context

    def test_bulk_insert(self):
        context = self._test_bulk_insert("default", False)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) VALUES (:id, :v1, :v2)"
        )

    def test_bulk_insert_wrong_cols(self):
        context = op_fixture("postgresql+psycopg2")
        t1 = table(
            "ins_table",
            column("id", Integer),
            column("v1", String()),
            column("v2", String()),
        )
        op.bulk_insert(t1, [{"v1": "row v1"}])
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (%(id)s, %(v1)s, %(v2)s)"
        )

    def test_bulk_insert_no_rows(self):
        context, t1 = self._table_fixture("default", False)

        op.bulk_insert(t1, [])
        context.assert_()

    def test_bulk_insert_pg(self):
        context = self._test_bulk_insert("postgresql+psycopg2", False)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (%(id)s, %(v1)s, %(v2)s)"
        )

    def test_bulk_insert_pg_single(self):
        context = self._test_bulk_insert_single("postgresql+psycopg2", False)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (%(id)s, %(v1)s, %(v2)s)"
        )

    def test_bulk_insert_pg_single_as_sql(self):
        context = self._test_bulk_insert_single("postgresql", True)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) VALUES (1, 'row v1', 'row v5')"
        )

    def test_bulk_insert_pg_single_big_t_as_sql(self):
        context = self._test_bulk_insert_single_bigt("postgresql", True)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (1, 'row v1', 'row v5')"
        )

    def test_bulk_insert_mssql(self):
        context = self._test_bulk_insert("mssql", False)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) VALUES (:id, :v1, :v2)"
        )

    def test_bulk_insert_inline_literal_as_sql(self):
        context = op_fixture("postgresql", True)

        class MyType(TypeEngine):
            pass

        t1 = table("t", column("id", Integer), column("data", MyType()))

        op.bulk_insert(
            t1,
            [
                {"id": 1, "data": op.inline_literal("d1")},
                {"id": 2, "data": op.inline_literal("d2")},
            ],
        )
        context.assert_(
            "INSERT INTO t (id, data) VALUES (1, 'd1')",
            "INSERT INTO t (id, data) VALUES (2, 'd2')",
        )

    def test_bulk_insert_as_sql(self):
        context = self._test_bulk_insert("default", True)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (1, 'row v1', 'row v5')",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (2, 'row v2', 'row v6')",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (3, 'row v3', 'row v7')",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (4, 'row v4', 'row v8')",
        )

    def test_bulk_insert_as_sql_pg(self):
        context = self._test_bulk_insert("postgresql", True)
        context.assert_(
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (1, 'row v1', 'row v5')",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (2, 'row v2', 'row v6')",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (3, 'row v3', 'row v7')",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (4, 'row v4', 'row v8')",
        )

    def test_bulk_insert_as_sql_mssql(self):
        context = self._test_bulk_insert("mssql", True)
        # SQL server requires IDENTITY_INSERT
        # TODO: figure out if this is safe to enable for a table that
        # doesn't have an IDENTITY column
        context.assert_(
            "SET IDENTITY_INSERT ins_table ON",
            "GO",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (1, 'row v1', 'row v5')",
            "GO",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (2, 'row v2', 'row v6')",
            "GO",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (3, 'row v3', 'row v7')",
            "GO",
            "INSERT INTO ins_table (id, v1, v2) "
            "VALUES (4, 'row v4', 'row v8')",
            "GO",
            "SET IDENTITY_INSERT ins_table OFF",
            "GO",
        )

    def test_bulk_insert_from_new_table(self):
        context = op_fixture("postgresql", True)
        t1 = op.create_table(
            "ins_table",
            Column("id", Integer),
            Column("v1", String()),
            Column("v2", String()),
        )
        op.bulk_insert(
            t1,
            [
                {"id": 1, "v1": "row v1", "v2": "row v5"},
                {"id": 2, "v1": "row v2", "v2": "row v6"},
            ],
        )
        context.assert_(
            "CREATE TABLE ins_table (id INTEGER, v1 VARCHAR, v2 VARCHAR)",
            "INSERT INTO ins_table (id, v1, v2) VALUES "
            "(1, 'row v1', 'row v5')",
            "INSERT INTO ins_table (id, v1, v2) VALUES "
            "(2, 'row v2', 'row v6')",
        )

    def test_bulk_insert_as_sql_nulls_dates_inline_literal(self):
        context = op_fixture("sqlite", True)

        class MyType(TypeEngine):
            pass

        t1 = table(
            "ins_table",
            column("id", Integer),
            column("v1", String()),
            column("d", Date),
            column("custom", MyType()),
        )
        op.bulk_insert(
            t1,
            [
                {
                    "id": 1,
                    "v1": "row v1",
                    "d": datetime.date(2026, 11, 22),
                    "custom": None,
                },
                {"id": 2, "v1": None, "d": None, "custom": None},
                {
                    "id": 3,
                    "v1": op.inline_literal("lit v3"),
                    "d": op.inline_literal("2020-01-01"),
                    "custom": op.inline_literal("c-lit"),
                },
            ],
        )
        context.assert_(
            "INSERT INTO ins_table (id, v1, d, custom) "
            "VALUES (1, 'row v1', '2026-11-22', NULL)",
            "INSERT INTO ins_table (id, v1, d, custom) "
            "VALUES (2, NULL, NULL, NULL)",
            "INSERT INTO ins_table (id, v1, d, custom) "
            "VALUES (3, 'lit v3', '2020-01-01', 'c-lit')",
        )

    def test_bulk_insert_as_sql_unrenderable_reports_row_and_column(self):
        class MyType(TypeEngine):
            pass

        class Weird:
            def __repr__(self):
                return "Weird()"

        t1 = table(
            "ins_table",
            column("id", Integer),
            column("custom", MyType()),
        )
        # the bad value is on row 2 / column "custom"; rows 1 and 3 are
        # otherwise fine
        rows = [
            {"id": 1, "custom": None},
            {"id": 2, "custom": Weird()},
            {"id": 3, "custom": None},
        ]
        for dialect in ("sqlite", "postgresql", "mssql"):
            context = op_fixture(dialect, True)
            assert_raises_message(
                CompileError,
                r"Could not render value for row 2, column 'custom' in "
                r"bulk_insert\(\) against table ins_table",
                op.bulk_insert,
                t1,
                rows,
            )

    def test_bulk_insert_as_sql_unrenderable_in_inline_literal(self):
        class MyType(TypeEngine):
            pass

        class Weird:
            def __repr__(self):
                return "Weird()"

        context = op_fixture("sqlite", True)
        t1 = table(
            "ins_table", column("id", Integer), column("custom", MyType())
        )
        assert_raises_message(
            CompileError,
            r"Could not render value for row 1, column 'custom' in "
            r"bulk_insert\(\) against table ins_table",
            op.bulk_insert,
            t1,
            [{"id": 1, "custom": op.inline_literal(Weird())}],
        )

    def test_bulk_insert_as_sql_failure_emits_nothing(self):
        class MyType(TypeEngine):
            pass

        class Weird:
            def __repr__(self):
                return "Weird()"

        t1 = table(
            "ins_table",
            column("id", Integer),
            column("v1", String()),
            column("custom", MyType()),
        )
        rows = [
            {"id": 1, "v1": "good row", "custom": None},
            {"id": 2, "v1": "bad row", "custom": Weird()},
            {"id": 3, "v1": "good row", "custom": None},
        ]
        # default dialect: no partial INSERT may be written
        context = op_fixture("sqlite", True)
        assert_raises_message(
            CompileError,
            r"Could not render value for row 2, column 'custom'",
            op.bulk_insert,
            t1,
            rows,
        )
        context.assert_()

        # mssql: not even the SET IDENTITY_INSERT bookends may leak
        context = op_fixture("mssql", True)
        assert_raises_message(
            CompileError,
            r"Could not render value for row 2, column 'custom'",
            op.bulk_insert,
            t1,
            rows,
        )
        context.assert_()

    def test_invalid_format(self):
        context, t1 = self._table_fixture("sqlite", False)
        assert_raises_message(
            TypeError, "List expected", op.bulk_insert, t1, {"id": 5}
        )

        assert_raises_message(
            TypeError,
            "List of dictionaries expected",
            op.bulk_insert,
            t1,
            [(5,)],
        )


class RoundTripTest(TestBase):
    __only_on__ = "sqlite"

    def setUp(self):
        self.conn = config.db.connect()
        with self.conn.begin():
            self.conn.execute(text("""
                create table foo(
                    id integer primary key,
                    data varchar(50),
                    x integer
                )
            """))
        context = MigrationContext.configure(self.conn)
        self.op = op.Operations(context)
        self.t1 = table("foo", column("id"), column("data"), column("x"))

        self.trans = self.conn.begin()

    def tearDown(self):
        self.trans.rollback()
        with self.conn.begin():
            self.conn.execute(text("drop table foo"))
        self.conn.close()

    def test_single_insert_round_trip(self):
        self.op.bulk_insert(self.t1, [{"data": "d1", "x": "x1"}])

        eq_(
            self.conn.execute(text("select id, data, x from foo")).fetchall(),
            [(1, "d1", "x1")],
        )

    def test_bulk_insert_round_trip(self):
        self.op.bulk_insert(
            self.t1,
            [
                {"data": "d1", "x": "x1"},
                {"data": "d2", "x": "x2"},
                {"data": "d3", "x": "x3"},
            ],
        )

        eq_(
            self.conn.execute(text("select id, data, x from foo")).fetchall(),
            [(1, "d1", "x1"), (2, "d2", "x2"), (3, "d3", "x3")],
        )

    def test_bulk_insert_inline_literal(self):
        class MyType(TypeEngine):
            pass

        t1 = table("foo", column("id", Integer), column("data", MyType()))

        self.op.bulk_insert(
            t1,
            [
                {"id": 1, "data": self.op.inline_literal("d1")},
                {"id": 2, "data": self.op.inline_literal("d2")},
            ],
            multiinsert=False,
        )

        eq_(
            self.conn.execute(text("select id, data from foo")).fetchall(),
            [(1, "d1"), (2, "d2")],
        )

    def test_bulk_insert_from_new_table(self):
        t1 = self.op.create_table(
            "ins_table",
            Column("id", Integer),
            Column("v1", String()),
            Column("v2", String()),
        )
        self.op.bulk_insert(
            t1,
            [
                {"id": 1, "v1": "row v1", "v2": "row v5"},
                {"id": 2, "v1": "row v2", "v2": "row v6"},
            ],
        )
        eq_(
            self.conn.execute(
                text("select id, v1, v2 from ins_table order by id")
            ).fetchall(),
            [(1, "row v1", "row v5"), (2, "row v2", "row v6")],
        )

    def test_offline_sql_matches_online_context(self):
        """The same batch, rendered as offline SQL and executed against a
        scratch SQLite database, must produce the same rows as running the
        batch through an online MigrationContext."""

        def _table():
            return Table(
                "roundtrip_t",
                MetaData(),
                Column("id", Integer, primary_key=True),
                Column("name", String(50)),
                Column("d", Date),
                Column("custom", String(50)),
            )

        rows = [
            {
                "id": 1,
                "name": "alice",
                "d": datetime.date(2026, 11, 22),
                "custom": None,
            },
            {"id": 2, "name": None, "d": None, "custom": None},
            {
                "id": 3,
                "name": self.op.inline_literal("carol"),
                "d": self.op.inline_literal("2020-01-01"),
                "custom": self.op.inline_literal("inline value"),
            },
            {
                "id": 4,
                "name": "dave",
                "d": datetime.date(1999, 12, 31),
                "custom": "plain bound",
            },
        ]

        # --- offline path: generate literal SQL with an as_sql context ---
        buffer = io.StringIO()
        offline_ctx = MigrationContext.configure(
            dialect_name="sqlite",
            opts={"as_sql": True, "output_buffer": buffer},
        )
        offline_op = op.Operations(offline_ctx)
        # table construct usable for both paths; recreate per context to
        # avoid any shared compile state
        offline_table = _table()
        offline_op.bulk_insert(offline_table, rows, multiinsert=False)
        offline_sql = buffer.getvalue()

        # sanity check: every row got its own INSERT, fully inlined
        eq_(offline_sql.count("INSERT INTO roundtrip_t"), len(rows))

        # --- apply the offline SQL to a scratch in-memory SQLite engine ---
        offline_engine = create_engine("sqlite:///:memory:")
        with offline_engine.begin() as conn:
            _table().create(conn)
            for statement in offline_sql.split(";"):
                statement = statement.strip()
                if statement:
                    conn.execute(text(statement))
            offline_rows = conn.execute(
                text(
                    "select id, name, d, custom from roundtrip_t "
                    "order by id"
                )
            ).fetchall()

        # --- online path: ordinary connected MigrationContext ---
        online_engine = create_engine("sqlite:///:memory:")
        with online_engine.begin() as conn:
            _table().create(conn)
            online_ctx = MigrationContext.configure(conn)
            online_op = op.Operations(online_ctx)
            online_table = _table()
            online_op.bulk_insert(online_table, rows, multiinsert=False)
            online_rows = conn.execute(
                text(
                    "select id, name, d, custom from roundtrip_t "
                    "order by id"
                )
            ).fetchall()

        eq_(offline_rows, online_rows)
        # explicit expectation, including NULL handling and inlined values
        eq_(
            offline_rows,
            [
                (1, "alice", "2026-11-22", None),
                (2, None, None, None),
                (3, "carol", "2020-01-01", "inline value"),
                (4, "dave", "1999-12-31", "plain bound"),
            ],
        )
