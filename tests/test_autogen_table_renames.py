from __future__ import annotations

import os

from sqlalchemy import Column
from sqlalchemy import create_engine
from sqlalchemy import ForeignKey
from sqlalchemy import ForeignKeyConstraint
from sqlalchemy import Index
from sqlalchemy import Integer
from sqlalchemy import MetaData
from sqlalchemy import String
from sqlalchemy import Table
from sqlalchemy import text
from sqlalchemy import UniqueConstraint
from sqlalchemy.pool import StaticPool

from alembic import autogenerate
from alembic import command
from alembic import op
from alembic import util
from alembic.autogenerate import api
from alembic.autogenerate.compare.renames import TableRename
from alembic.autogenerate.compare.renames import TableRenames
from alembic.migration import MigrationContext
from alembic.operations import ops
from alembic.testing import assert_raises_message
from alembic.testing import eq_
from alembic.testing import is_true
from alembic.testing import TestBase
from alembic.testing.env import _sqlite_file_db
from alembic.testing.env import _sqlite_testing_config
from alembic.testing.env import clear_staging_env
from alembic.testing.env import env_file_fixture
from alembic.testing.env import staging_env
from alembic.testing.fixtures import op_fixture
from alembic.testing.suite._autogen_fixtures import AutogenFixtureTest
from alembic.util import CommandError


class TableRenamesOptionTest(TestBase):
    """test the parsing of the table_renames parameter itself."""

    def test_plain_names(self):
        renames = TableRenames.from_option({"user": "account"}, None)

        eq_(
            renames.renames,
            [TableRename(None, "user", None, "account")],
        )

    def test_target_inherits_source_schema(self):
        renames = TableRenames.from_option(
            {("hr", "user"): "account"}, None
        )
        eq_(
            renames.renames,
            [TableRename("hr", "user", "hr", "account")],
        )

    def test_explicit_schemas(self):
        renames = TableRenames.from_option(
            {("hr", "user"): ("hr", "account")}, None
        )
        eq_(
            renames.renames,
            [TableRename("hr", "user", "hr", "account")],
        )

    def test_cross_schema_rejected(self):
        assert_raises_message(
            CommandError,
            "moves a table between schemas",
            TableRenames.from_option,
            {("hr", "user"): ("archive", "account")},
            None,
        )

    def test_default_schema_normalized(self):
        class FakeDialect:
            default_schema_name = "public"

        renames = TableRenames.from_option(
            {("public", "user"): ("public", "account")}, FakeDialect()
        )
        eq_(
            renames.renames,
            [TableRename("public", "user", "public", "account")],
        )
        eq_(renames._norm_key(("public", "user")), (None, "user"))

    def test_sequence_of_pairs(self):
        renames = TableRenames.from_option(
            [("user", "account"), ("order", "purchase")], None
        )
        eq_(
            renames.renames,
            [
                TableRename(None, "user", None, "account"),
                TableRename(None, "order", None, "purchase"),
            ],
        )

    def test_not_a_mapping(self):
        assert_raises_message(
            CommandError,
            "table_renames should be a mapping",
            TableRenames.from_option,
            "user",
            None,
        )

    def test_not_a_pair(self):
        assert_raises_message(
            CommandError,
            r"table_renames entries should be pairs",
            TableRenames.from_option,
            [("user", "account", "extra")],
            None,
        )

    def test_same_table(self):
        assert_raises_message(
            CommandError,
            "renames a table to itself",
            TableRenames.from_option,
            {"user": "user"},
            None,
        )

    def test_dupe_source(self):
        assert_raises_message(
            CommandError,
            "declares the old table user more than once",
            TableRenames.from_option,
            [("user", "account"), ("user", "other_account")],
            None,
        )

    def test_dupe_target(self):
        assert_raises_message(
            CommandError,
            "declares the new table account more than once",
            TableRenames.from_option,
            [("user", "account"), ("order", "account")],
            None,
        )

    def test_chained(self):
        assert_raises_message(
            CommandError,
            "is chained against another entry",
            TableRenames.from_option,
            [("user", "account"), ("account", "customer")],
            None,
        )

    def test_bad_schema(self):
        assert_raises_message(
            CommandError,
            "schema name should be a string or None",
            TableRenames.from_option,
            {(1, "user"): "account"},
            None,
        )

    def test_bad_name(self):
        assert_raises_message(
            CommandError,
            "table name should be a non-empty string",
            TableRenames.from_option,
            {"user": ("hr", None)},
            None,
        )


class TableRenamesResolveTest(TestBase):
    """test resolving declarations against the tables that really exist."""

    def _renames(self, mapping):
        return TableRenames.from_option(mapping, None)

    def test_applies(self):
        renames = self._renames({"user": "account"})
        applied = renames.resolve(
            [(None, "user"), (None, "order")],
            [(None, "account"), (None, "order")],
        )
        eq_(applied, [TableRename(None, "user", None, "account")])
        eq_(renames.pairs, [((None, "account"), (None, "user"))])
        eq_(renames.conn_key_for_model(None, "account"), (None, "user"))
        eq_(renames.model_key_for_conn(None, "user"), (None, "account"))

    def test_already_applied_is_ignored(self):
        renames = self._renames({"user": "account"})
        applied = renames.resolve(
            [(None, "account")],
            [(None, "account")],
        )
        eq_(applied, [])
        eq_(renames.pairs, [])

    def test_old_table_missing_is_ignored(self):
        renames = self._renames({"user": "account"})
        applied = renames.resolve([], [(None, "account")])
        eq_(applied, [])

    def test_new_table_missing_is_ignored(self):
        renames = self._renames({"user": "account"})
        applied = renames.resolve([(None, "user")], [])
        eq_(applied, [])

    def test_new_table_already_in_database(self):
        renames = self._renames({"user": "account"})
        assert_raises_message(
            CommandError,
            "the new table account already exists in the database",
            renames.resolve,
            [(None, "user"), (None, "account")],
            [(None, "account")],
        )

    def test_mapping_onto_two_real_tables(self):
        renames = self._renames({"user": "account"})
        assert_raises_message(
            CommandError,
            "the mapping points at two tables that really exist",
            renames.resolve,
            [(None, "user")],
            [(None, "user"), (None, "account")],
        )

    def test_unqualified_mapping_onto_two_real_tables(self):
        """a bare old name that could match a rename in two schemas."""

        renames = self._renames({"user": "account"})
        assert_raises_message(
            CommandError,
            "without a schema and could refer to more than one table",
            renames.resolve,
            [("hr", "user"), ("archive", "user")],
            [("hr", "account"), ("archive", "account")],
        )

    def test_unqualified_mapping_matches_nothing(self):
        """bare names in other schemas do not accidentally match."""

        renames = self._renames({"user": "account"})
        eq_(renames.resolve([("hr", "user")], [("hr", "account")]), [])

    def test_unqualified_mapping_matches_single_schema(self):
        renames = self._renames({"user": "account"})
        eq_(
            renames.resolve(
                [("hr", "user"), ("archive", "user")],
                [("hr", "account")],
            ),
            [],
        )

    def test_include_filter_skips(self):
        renames = self._renames({"user": "account"})
        applied = renames.resolve(
            [(None, "user")],
            [(None, "account")],
            include=lambda key: False,
        )
        eq_(applied, [])

    def test_other_schema_untouched(self):
        renames = self._renames({("hr", "user"): ("hr", "account")})
        applied = renames.resolve(
            [(None, "user"), ("hr", "user")],
            [(None, "user"), ("hr", "account")],
        )
        eq_(applied, [TableRename("hr", "user", "hr", "account")])
        eq_(renames.model_key_for_conn(None, "user"), None)


class AutogenTableRenameTest(AutogenFixtureTest, TestBase):
    __backend__ = True

    def _fixture(self, m1, m2, table_renames, **kw):
        opts = dict(kw.pop("opts", None) or {})
        opts["table_renames"] = table_renames
        return super()._fixture(m1, m2, opts=opts, **kw)

    def test_rename_produces_single_directive(self):
        m1 = MetaData()
        m2 = MetaData()

        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50), nullable=False),
        )
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50), nullable=False),
        )

        diffs = self._fixture(m1, m2, {"user": "account"})
        eq_(
            diffs,
            [("rename_table", None, "user", "account")],
        )

    def test_rename_still_compares_columns(self):
        m1 = MetaData()
        m2 = MetaData()

        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50), nullable=False),
            Column("gone", Integer),
        )
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50), nullable=False),
            Column("added", Integer),
        )

        diffs = self._fixture(m1, m2, {"user": "account"})
        eq_(diffs[0], ("rename_table", None, "user", "account"))
        eq_(diffs[1][0], "add_column")
        eq_(diffs[1][2], "account")
        eq_(diffs[1][3].name, "added")
        eq_(diffs[2][0], "remove_column")
        eq_(diffs[2][2], "account")
        eq_(diffs[2][3].name, "gone")

    def test_rename_still_compares_indexes(self):
        m1 = MetaData()
        m2 = MetaData()

        user = Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            Index("ix_user_name", "name"),
        )
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            Index("ix_user_name", "name"),
        )
        assert user.indexes

        diffs = self._fixture(m1, m2, {"user": "account"})
        eq_(diffs, [("rename_table", None, "user", "account")])

    def test_rename_still_compares_unique_constraints(self):
        m1 = MetaData()
        m2 = MetaData()

        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            UniqueConstraint("name", name="uq_user_name"),
        )
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            UniqueConstraint("name", name="uq_user_name"),
        )

        diffs = self._fixture(m1, m2, {"user": "account"})
        eq_(diffs, [("rename_table", None, "user", "account")])

    def test_rename_with_unrelated_fk(self):
        """a foreign key pointing at the renamed table must not change."""

        m1 = MetaData()
        m2 = MetaData()

        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
        )
        Table(
            "address",
            m1,
            Column("id", Integer, primary_key=True),
            Column("user_id", Integer, ForeignKey("user.id")),
        )

        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
        )
        Table(
            "address",
            m2,
            Column("id", Integer, primary_key=True),
            Column("user_id", Integer, ForeignKey("account.id")),
        )

        diffs = self._fixture(m1, m2, {"user": "account"})
        eq_(diffs, [("rename_table", None, "user", "account")])

    def test_rename_with_fk_on_renamed_table(self):
        """a foreign key owned by the renamed table must not change."""

        m1 = MetaData()
        m2 = MetaData()

        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
        )
        Table(
            "address",
            m1,
            Column("id", Integer, primary_key=True),
            Column("user_id", Integer),
            ForeignKeyConstraint(["user_id"], ["user.id"]),
        )

        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
        )
        Table(
            "billing_address",
            m2,
            Column("id", Integer, primary_key=True),
            Column("user_id", Integer),
            ForeignKeyConstraint(["user_id"], ["account.id"]),
        )

        diffs = self._fixture(
            m1, m2, {"user": "account", "address": "billing_address"}
        )
        eq_(
            diffs,
            [
                ("rename_table", None, "user", "account"),
                ("rename_table", None, "address", "billing_address"),
            ],
        )

    def test_rename_does_not_apply_when_old_table_gone(self):
        m1 = MetaData()
        m2 = MetaData()

        Table("account", m1, Column("id", Integer, primary_key=True))
        Table("account", m2, Column("id", Integer, primary_key=True))

        diffs = self._fixture(m1, m2, {"user": "account"})
        eq_(diffs, [])

    def test_new_table_already_present_raises(self):
        m1 = MetaData()
        m2 = MetaData()

        Table("user", m1, Column("id", Integer, primary_key=True))
        Table("account", m1, Column("id", Integer, primary_key=True))
        Table("account", m2, Column("id", Integer, primary_key=True))

        assert_raises_message(
            CommandError,
            "the new table account already exists in the database",
            self._fixture,
            m1,
            m2,
            {"user": "account"},
        )

    def test_mapping_onto_two_real_tables_raises(self):
        m1 = MetaData()
        m2 = MetaData()

        Table("user", m1, Column("id", Integer, primary_key=True))
        Table(
            "user", m2, Column("id", Integer, primary_key=True)
        )
        Table("account", m2, Column("id", Integer, primary_key=True))

        assert_raises_message(
            CommandError,
            "the mapping points at two tables that really exist",
            self._fixture,
            m1,
            m2,
            {"user": "account"},
        )

    def test_old_style_drop_add_without_mapping(self):
        m1 = MetaData()
        m2 = MetaData()

        Table("user", m1, Column("id", Integer, primary_key=True))
        Table("account", m2, Column("id", Integer, primary_key=True))

        diffs = self._fixture(m1, m2, None)
        eq_(
            [diff[0] for diff in diffs],
            ["add_table", "remove_table"],
        )

    def test_rename_skipped_when_target_excluded_by_include_object(self):
        m1 = MetaData()
        m2 = MetaData()

        Table("user", m1, Column("id", Integer, primary_key=True))
        Table("account", m2, Column("id", Integer, primary_key=True))

        def include_object(obj, name, type_, reflected, compare_to):
            if type_ == "table" and name == "account":
                return False
            return True

        diffs = self._fixture(
            m1,
            m2,
            {"user": "account"},
            object_filters=include_object,
        )
        eq_([diff[0] for diff in diffs], ["remove_table"])


    def test_rename_with_render_as_batch_not_wrapped(self):
        """the rename is a table-level operation and is not placed inside
        a batch block."""

        m1 = MetaData()
        m2 = MetaData()

        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
        )
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            Column("added", Integer),
        )

        uo = self._fixture(
            m1,
            m2,
            {"user": "account"},
            opts={"render_as_batch": True},
            return_ops=True,
        )
        eq_(
            autogenerate.render_python_code(uo, render_as_batch=True),
            "# ### commands auto generated by Alembic - please adjust! ###\n"
            "    op.rename_table('user', 'account')\n"
            "    with op.batch_alter_table('account', schema=None) "
            "as batch_op:\n"
            "        batch_op.add_column(sa.Column('added', sa.Integer(), "
            "nullable=True))\n"
            "\n"
            "    # ### end Alembic commands ###",
        )


class AutogenTableRenameSchemaTest(TestBase):
    """the schema-qualified form of the mapping.

    These tests use a SQLite "attached database", which SQLAlchemy exposes
    as a schema.  A ``StaticPool`` is used so that the attached database
    stays visible to the connections used by the comparison.

    """

    __only_on__ = "sqlite"

    def setUp(self):
        staging_env()
        self.bind = create_engine("sqlite://", poolclass=StaticPool)

        # with a StaticPool, every connection shares the one DBAPI
        # connection, so the attached database is available to all of them
        with self.bind.connect() as conn:
            conn.execute(text("attach database ':memory:' as hr"))

        self.m1 = m1 = MetaData()
        Table(
            "user",
            m1,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            schema="hr",
        )

    def tearDown(self):
        self.bind.dispose()
        clear_staging_env()

    def _fixture(self, m1, m2, table_renames):
        for m in util.to_list(m1):
            m.create_all(self.bind)

        with self.bind.connect() as conn:
            ctx_opts = {
                "compare_type": True,
                "compare_server_default": True,
                "target_metadata": m2,
                "upgrade_token": "upgrades",
                "downgrade_token": "downgrades",
                "alembic_module_prefix": "op.",
                "sqlalchemy_module_prefix": "sa.",
                "include_schemas": True,
                "table_renames": table_renames,
            }
            context = MigrationContext.configure(
                connection=conn, opts=ctx_opts
            )
            autogen_context = api.AutogenContext(context, m2)
            uo = ops.UpgradeOps(ops=[])
            autogenerate._produce_net_changes(autogen_context, uo)
            return uo.as_diffs()

    def test_cross_schema_rename(self):
        m2 = MetaData()
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            schema="hr",
        )

        diffs = self._fixture(
            self.m1,
            m2,
            {("hr", "user"): ("hr", "account")},
        )
        eq_(diffs, [("rename_table", "hr", "user", "account")])

    def test_same_name_in_other_schema_untouched(self):
        """a same-named table in the default schema is not confused with
        the renamed table in the "hr" schema."""

        m2 = MetaData()
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            schema="hr",
        )
        Table(
            "user",
            m2,
            Column("id", Integer, primary_key=True),
        )

        diffs = self._fixture(
            self.m1,
            m2,
            {("hr", "user"): ("hr", "account")},
        )
        eq_(diffs[0], ("rename_table", "hr", "user", "account"))
        eq_(
            [
                (diff[0], diff[1].schema, diff[1].name)
                for diff in diffs[1:]
                if diff[0] == "add_table"
            ],
            [("add_table", None, "user")],
        )

    def test_rename_keeps_fk_in_schema(self):
        m1 = self.m1
        Table(
            "address",
            m1,
            Column("id", Integer, primary_key=True),
            Column("user_id", Integer),
            ForeignKeyConstraint(["user_id"], ["hr.user.id"]),
            schema="hr",
        )

        m2 = MetaData()
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50)),
            schema="hr",
        )
        Table(
            "address",
            m2,
            Column("id", Integer, primary_key=True),
            Column("user_id", Integer),
            ForeignKeyConstraint(["user_id"], ["hr.account.id"]),
            schema="hr",
        )

        diffs = self._fixture(
            m1,
            m2,
            {("hr", "user"): ("hr", "account")},
        )
        eq_(diffs, [("rename_table", "hr", "user", "account")])


    def test_compare_metadata_reports_rename(self):
        """the diff-tuple returned by compare_metadata names both tables."""

        bind = create_engine("sqlite://")
        with bind.begin() as conn:
            conn.execute(
                text(
                    "create table user ("
                    "id integer primary key not null, name varchar not null)"
                )
            )

        m2 = MetaData()
        Table(
            "account",
            m2,
            Column("id", Integer, primary_key=True),
            Column("name", String(50), nullable=False),
        )

        with bind.connect() as conn:
            context = MigrationContext.configure(
                conn,
                opts={
                    "target_metadata": m2,
                    "table_renames": {"user": "account"},
                },
            )
            eq_(
                autogenerate.compare_metadata(context, m2),
                [("rename_table", None, "user", "account")],
            )
        bind.dispose()


class TableRenameOpTest(TestBase):
    def test_reverse(self):
        op = ops.RenameTableOp("user", "account", schema="hr")
        reversed_op = op.reverse()
        is_true(isinstance(reversed_op, ops.RenameTableOp))
        eq_(reversed_op.table_name, "account")
        eq_(reversed_op.new_table_name, "user")
        eq_(reversed_op.schema, "hr")

    def test_diff_tuple(self):
        op = ops.RenameTableOp("user", "account")
        eq_(op.to_diff_tuple(), ("rename_table", None, "user", "account"))

    def test_batch_rename_table(self):
        context = op_fixture("sqlite")
        op.rename_table("user", "account")
        context.assert_(
            "ALTER TABLE user RENAME TO account",
        )

    def test_render_upgrade_and_downgrade(self):
        migration_script = ops.MigrationScript(
            None,
            ops.UpgradeOps([ops.RenameTableOp("user", "account")]),
            ops.DowngradeOps([]),
        )
        migration_script.upgrade_ops.reverse_into(
            migration_script.downgrade_ops
        )

        eq_(
            autogenerate.render_python_code(migration_script.upgrade_ops),
            "# ### commands auto generated by Alembic - please adjust! ###\n"
            "    op.rename_table('user', 'account')\n"
            "    # ### end Alembic commands ###",
        )
        eq_(
            autogenerate.render_python_code(migration_script.downgrade_ops),
            "# ### commands auto generated by Alembic - please adjust! ###\n"
            "    op.rename_table('account', 'user')\n"
            "    # ### end Alembic commands ###",
        )

    def test_render_with_schema(self):
        migration_script = ops.MigrationScript(
            None,
            ops.UpgradeOps(
                [ops.RenameTableOp("user", "account", schema="hr")]
            ),
            ops.DowngradeOps([]),
        )
        eq_(
            autogenerate.render_python_code(migration_script.upgrade_ops),
            "# ### commands auto generated by Alembic - please adjust! ###\n"
            "    op.rename_table('user', 'account', schema='hr')\n"
            "    # ### end Alembic commands ###",
        )


class TableRenameEndToEndTest(TestBase):
    """run the full revision / upgrade / downgrade cycle against a real
    SQLite database that contains rows.

    The rows must survive both directions, and must remain reachable
    through the foreign key relationship between the two tables.

    """

    __only_on__ = "sqlite"

    def setUp(self):
        self.env = staging_env()
        self.cfg = _sqlite_testing_config()
        self.bind = _sqlite_file_db()

        with self.bind.begin() as conn:
            conn.execute(
                text(
                    "create table user ("
                    "id integer primary key not null, name varchar not null)"
                )
            )
            conn.execute(
                text(
                    "create table address ("
                    "id integer primary key not null, email varchar, "
                    "user_id integer references user (id))"
                )
            )
            conn.execute(text("insert into user values (1, 'ed')"))
            conn.execute(text("insert into address values (1, 'ed@x', 1)"))

        env_file_fixture(
            """
import sqlalchemy as sa
from sqlalchemy import engine_from_config

target_metadata = sa.MetaData()
sa.Table(
    "account",
    target_metadata,
    sa.Column("id", sa.Integer, primary_key=True),
    sa.Column("name", sa.String(50), nullable=False),
)
sa.Table(
    "address",
    target_metadata,
    sa.Column("id", sa.Integer, primary_key=True),
    sa.Column("email", sa.String(50)),
    sa.Column("user_id", sa.Integer, sa.ForeignKey("account.id")),
)

engine = engine_from_config(
    config.get_section(config.config_ini_section), prefix="sqlalchemy."
)
connection = engine.connect()

context.configure(
    connection=connection,
    target_metadata=target_metadata,
    table_renames={"user": "account"},
)

try:
    with context.begin_transaction():
        context.run_migrations()
finally:
    connection.close()
    engine.dispose()
"""
        )

    def tearDown(self):
        clear_staging_env()

    def _rows(self, table_name):
        with self.bind.connect() as conn:
            return conn.execute(
                text(
                    "select a.name, b.email from %s a "
                    "join address b on b.user_id = a.id" % table_name
                )
            ).fetchall()

    def test_revision_contains_rename_in_both_directions(self):
        rev = command.revision(self.cfg, autogenerate=True)
        assert rev is not None

        with open(rev.path) as f:
            contents = f.read()

        assert "op.rename_table('user', 'account')" in contents
        assert "op.rename_table('account', 'user')" in contents
        assert "op.create_table('account'" not in contents
        assert "op.drop_table('user')" not in contents
        assert "drop_constraint" not in contents
        assert "create_foreign_key" not in contents

        eq_(self._rows("user"), [("ed", "ed@x")])

    def test_upgrade_downgrade_round_trip_keeps_rows(self):
        command.revision(self.cfg, autogenerate=True)

        command.upgrade(self.cfg, "head")
        eq_(self._rows("account"), [("ed", "ed@x")])

        command.downgrade(self.cfg, "base")
        eq_(self._rows("user"), [("ed", "ed@x")])

    def test_second_autogenerate_after_upgrade_is_empty(self):
        command.revision(self.cfg, autogenerate=True)
        command.upgrade(self.cfg, "head")

        rev = command.revision(self.cfg, autogenerate=True)
        assert rev is not None
        with open(rev.path) as f:
            contents = f.read()

        assert "rename_table" not in contents
        assert "create_table" not in contents
        assert "drop_table" not in contents

    def test_conflicting_mapping_reported_before_file_generated(self):
        """a mapping whose new table already exists in the database fails
        before any revision file is written."""

        with self.bind.begin() as conn:
            conn.execute(
                text("create table account (id integer primary key not null)")
            )

        script_location = self.cfg.get_main_option("script_location")
        assert script_location is not None
        versions = os.path.join(script_location, "versions")

        assert_raises_message(
            CommandError,
            "the new table account already exists in the database",
            command.revision,
            self.cfg,
            autogenerate=True,
        )
        eq_(os.listdir(versions), [])
