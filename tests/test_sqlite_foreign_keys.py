"""Tests for deferred SQLite foreign key enforcement (``sqlite_foreign_keys``).

These tests exercise the full migration command path against two separate
temporary SQLite databases: one where a migration finishes two mutually
referencing tables cleanly and is committed, and one where a migration
leaves a dangling reference behind and must fail with the responsible
revision identified.
"""

import io

from sqlalchemy import create_engine
from sqlalchemy.exc import IntegrityError

from alembic import command
from alembic.migration import MigrationContext
from alembic.script import ScriptDirectory
from alembic.testing import assert_raises_message
from alembic.testing import eq_
from alembic.testing import is_true
from alembic.testing.env import _sqlite_testing_config
from alembic.testing.env import clear_staging_env
from alembic.testing.env import env_file_fixture
from alembic.testing.env import staging_env
from alembic.testing.env import write_script
from alembic.testing.fixtures import TestBase
from alembic.util import CommandError


class SQLiteForeignKeyConfigTest(TestBase):
    __only_on__ = "sqlite"
    __backend__ = True

    def test_unknown_mode_rejected(self):
        eng = create_engine("sqlite://")
        with eng.connect() as conn:
            assert_raises_message(
                CommandError,
                "Unknown value 'bogus' for 'sqlite_foreign_keys'; expected "
                "one of 'manual', 'deferred', 'immediate'",
                MigrationContext.configure,
                conn,
                opts={"sqlite_foreign_keys": "bogus"},
            )

    def test_manual_mode_does_not_touch_pragma(self):
        eng = create_engine("sqlite://")
        with eng.connect() as conn:
            conn.commit()
            eq_(conn.exec_driver_sql("PRAGMA foreign_keys").scalar(), 0)
            conn.commit()
            ctx = MigrationContext.configure(
                conn, opts={"sqlite_foreign_keys": "manual"}
            )
            eq_(ctx.impl.foreign_key_mode, "manual")
            ctx.impl.begin_foreign_keys_deferred(False)
            eq_(conn.exec_driver_sql("PRAGMA foreign_keys").scalar(), 0)
            ctx.impl.end_foreign_keys_deferred()
            eq_(conn.exec_driver_sql("PRAGMA foreign_keys").scalar(), 0)


class _SQLiteForeignKeyMigrationBase(TestBase):
    __only_on__ = "sqlite"
    __backend__ = True

    def setUp(self):
        self.env = staging_env()
        self.cfg_dir = ScriptDirectory.from_config(
            self._make_config("deferred")
        )

    def tearDown(self):
        clear_staging_env()

    def _make_config(self, mode):
        cfg = _sqlite_testing_config()
        self._configure_env(cfg, mode)
        return cfg

    def _configure_env(self, cfg, mode):
        raise NotImplementedError()

    def _write_revision(self, rev_id, down_revision, body):
        if down_revision is None:
            head = "base"
        else:
            head = down_revision
        self.cfg_dir.generate_revision(
            rev_id, "rev %s" % rev_id, refresh=True, head=head
        )
        down = "None" if down_revision is None else "'%s'" % down_revision
        write_script(
            self.cfg_dir,
            rev_id,
            """\
"rev %s"
revision = '%s'
down_revision = %s

from alembic import op


def upgrade():
%s


def downgrade():
    pass
"""
            % (rev_id, rev_id, down, body),
        )

    def _pragma(self, engine):
        with engine.connect() as conn:
            return conn.exec_driver_sql("PRAGMA foreign_keys").scalar()


class SQLiteForeignKeyManagedTransactionTest(_SQLiteForeignKeyMigrationBase):
    """env.py without an enclosing external transaction."""

    def _configure_env(self, cfg, mode):
        env_file_fixture(
            """
from sqlalchemy import engine_from_config, pool

engine = engine_from_config(
    config.get_section(config.config_ini_section, {}),
    prefix="sqlalchemy.",
    poolclass=pool.NullPool,
)
with engine.connect() as connection:
    context.configure(
        connection=connection,
        sqlite_foreign_keys=%r,
    )
    with context.begin_transaction():
        context.run_migrations()
"""
            % mode
        )

    def test_mutual_tables_completed_then_committed(self):
        # both tables exist and reference each other by the time the
        # migration finishes, so the deferred check passes.
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute(
        "CREATE TABLE b (id INTEGER PRIMARY KEY, "
        "a_id INTEGER REFERENCES a(id))"
    )
    op.execute(
        "CREATE TABLE a (id INTEGER PRIMARY KEY, "
        "b_id INTEGER REFERENCES b(id))"
    )
    op.execute("INSERT INTO a (id, b_id) VALUES (1, 1)")
    op.execute("INSERT INTO b (id, a_id) VALUES (1, 1)")
""",
        )
        cfg = self._make_config("deferred")
        command.upgrade(cfg, "head")

        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            with engine.connect() as conn:
                eq_(
                    conn.exec_driver_sql(
                        "SELECT count(*) FROM a"
                    ).scalar(),
                    1,
                )
                eq_(
                    conn.exec_driver_sql(
                        "SELECT count(*) FROM b"
                    ).scalar(),
                    1,
                )
                eq_(
                    conn.exec_driver_sql(
                        "SELECT version_num FROM alembic_version"
                    ).scalar(),
                    "aaaa",
                )
            # the connection used by the migration was returned with its
            # original pragma setting restored.
            eq_(self._pragma(engine), 0)
        finally:
            engine.dispose()

    def test_invalid_reference_fails_and_names_revision(self):
        # table 'a' keeps a row pointing at a 'b' row that never exists.
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute(
        "CREATE TABLE b (id INTEGER PRIMARY KEY, "
        "a_id INTEGER REFERENCES a(id))"
    )
    op.execute(
        "CREATE TABLE a (id INTEGER PRIMARY KEY, "
        "b_id INTEGER REFERENCES b(id))"
    )
    op.execute("INSERT INTO a (id, b_id) VALUES (99, 123456)")
""",
        )
        cfg = self._make_config("deferred")
        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            assert_raises_message(
                CommandError,
                r"Foreign key constraint validation failed for "
                r"revision 'aaaa'; PRAGMA foreign_key_check reported 1 "
                r"violation\(s\): table 'a' row 99 -> table 'b' \(fk #0\)",
                command.upgrade,
                cfg,
                "head",
            )
            with engine.connect() as conn:
                # DML was rolled back; the revision was never stamped
                eq_(
                    conn.exec_driver_sql(
                        "SELECT count(*) FROM a"
                    ).scalar(),
                    0,
                )
                eq_(
                    conn.exec_driver_sql(
                        "SELECT count(*) FROM alembic_version"
                    ).scalar(),
                    0,
                )
            # pragma restored despite the failure
            eq_(self._pragma(engine), 0)
        finally:
            engine.dispose()

    def test_second_revision_violation_is_attributed_to_second_revision(self):
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute(
        "CREATE TABLE b (id INTEGER PRIMARY KEY, "
        "a_id INTEGER REFERENCES a(id))"
    )
    op.execute(
        "CREATE TABLE a (id INTEGER PRIMARY KEY, "
        "b_id INTEGER REFERENCES b(id))"
    )
    op.execute("INSERT INTO a (id, b_id) VALUES (1, 1)")
    op.execute("INSERT INTO b (id, a_id) VALUES (1, 1)")
""",
        )
        self._write_revision(
            "bbbb",
            "aaaa",
            """\
    op.execute("INSERT INTO a (id, b_id) VALUES (99, 123456)")
""",
        )
        cfg = self._make_config("deferred")
        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            assert_raises_message(
                CommandError,
                r"Foreign key constraint validation failed for "
                r"revision 'bbbb'",
                command.upgrade,
                cfg,
                "head",
            )
            with engine.connect() as conn:
                # the first revision was applied; only the second rolled back
                eq_(
                    conn.exec_driver_sql(
                        "SELECT version_num FROM alembic_version"
                    ).scalar(),
                    "aaaa",
                )
                eq_(
                    conn.exec_driver_sql(
                        "SELECT count(*) FROM a"
                    ).scalar(),
                    1,
                )
        finally:
            engine.dispose()

    def test_pragma_restored_when_initially_enabled(self):
        # turn enforcement on before the migration environment runs; the
        # deferred run must switch it off and switch it back on afterwards.
        cfg = self._make_config("deferred")
        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            with engine.connect() as conn:
                conn.commit()
                conn.exec_driver_sql("PRAGMA foreign_keys=ON")
                conn.commit()
            is_true(self._pragma(engine) == 1)

            self._write_revision(
                "aaaa",
                None,
                """\
    op.execute(
        "CREATE TABLE b (id INTEGER PRIMARY KEY, "
        "a_id INTEGER REFERENCES a(id))"
    )
    op.execute(
        "CREATE TABLE a (id INTEGER PRIMARY KEY, "
        "b_id INTEGER REFERENCES b(id))"
    )
    op.execute("INSERT INTO a (id, b_id) VALUES (1, 1)")
    op.execute("INSERT INTO b (id, a_id) VALUES (1, 1)")
""",
            )
            command.upgrade(cfg, "head")
            is_true(self._pragma(engine) == 1)
        finally:
            engine.dispose()


class SQLiteForeignKeyExternalTransactionTest(_SQLiteForeignKeyMigrationBase):
    """env.py that opens the transaction itself before configure()."""

    def _configure_env(self, cfg, mode):
        env_file_fixture(
            """
from sqlalchemy import engine_from_config, pool

engine = engine_from_config(
    config.get_section(config.config_ini_section, {}),
    prefix="sqlalchemy.",
    poolclass=pool.NullPool,
)
with engine.connect() as connection:
    with connection.begin():
        context.configure(
            connection=connection,
            sqlite_foreign_keys=%r,
        )
        context.run_migrations()
"""
            % mode
        )

    def test_deferred_mode_rejected_with_external_transaction(self):
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute("CREATE TABLE t (id INTEGER)")
""",
        )
        cfg = self._make_config("deferred")
        assert_raises_message(
            CommandError,
            r"sqlite_foreign_keys='deferred' cannot be used with a "
            r"connection that is already inside a transaction",
            command.upgrade,
            cfg,
            "head",
        )

    def test_manual_mode_runs_under_external_transaction(self):
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute("CREATE TABLE t (id INTEGER)")
""",
        )
        cfg = self._make_config("manual")
        command.upgrade(cfg, "head")
        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            with engine.connect() as conn:
                eq_(
                    conn.exec_driver_sql(
                        "SELECT count(*) FROM sqlite_master "
                        "WHERE name='t'"
                    ).scalar(),
                    1,
                )
        finally:
            engine.dispose()

    def test_whole_run_violation_reported_without_revision_scope(self):
        # manual setup but with deferred enabled through a connection that
        # is NOT externally transactional would scope per revision; here we
        # validate the whole-run message shape directly on the impl.
        eng = create_engine("sqlite://")
        with eng.connect() as conn:
            conn.commit()
            ctx = MigrationContext.configure(
                conn, opts={"sqlite_foreign_keys": "deferred"}
            )
            conn.exec_driver_sql(
                "CREATE TABLE b (id INTEGER PRIMARY KEY, "
                "a_id INTEGER REFERENCES a(id))"
            )
            conn.exec_driver_sql(
                "CREATE TABLE a (id INTEGER PRIMARY KEY, "
                "b_id INTEGER REFERENCES b(id))"
            )
            conn.exec_driver_sql(
                "INSERT INTO a (id, b_id) VALUES (99, 123456)"
            )
            assert_raises_message(
                CommandError,
                r"Foreign key constraint validation failed for the "
                r"migration run; PRAGMA foreign_key_check reported 1 "
                r"violation\(s\): table 'a' row 99 -> table 'b' \(fk #0\)",
                ctx.impl.check_foreign_keys,
                None,
            )


class SQLiteForeignKeyImmediateModeTest(_SQLiteForeignKeyMigrationBase):
    def _configure_env(self, cfg, mode):
        env_file_fixture(
            """
from sqlalchemy import engine_from_config, pool

engine = engine_from_config(
    config.get_section(config.config_ini_section, {}),
    prefix="sqlalchemy.",
    poolclass=pool.NullPool,
)
with engine.connect() as connection:
    context.configure(
        connection=connection,
        sqlite_foreign_keys=%r,
    )
    with context.begin_transaction():
        context.run_migrations()
"""
            % mode
        )

    def test_immediate_mode_enforces_and_restores_pragma(self):
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute("CREATE TABLE t (id INTEGER)")
""",
        )
        cfg = self._make_config("immediate")
        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            command.upgrade(cfg, "head")
            # originally OFF -> restored to OFF after the run
            eq_(self._pragma(engine), 0)
        finally:
            engine.dispose()

    def test_immediate_mode_rejects_dangling_insert(self):
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute(
        "CREATE TABLE b (id INTEGER PRIMARY KEY, "
        "a_id INTEGER REFERENCES a(id))"
    )
    op.execute(
        "CREATE TABLE a (id INTEGER PRIMARY KEY, "
        "b_id INTEGER REFERENCES b(id))"
    )
    op.execute("INSERT INTO a (id, b_id) VALUES (99, 123456)")
""",
        )
        cfg = self._make_config("immediate")
        # SQLite itself raises (IntegrityError), not the deferred check;
        # the important contract is that the pragma is restored afterwards.
        assert_raises_message(
            IntegrityError,
            "FOREIGN KEY constraint failed",
            command.upgrade,
            cfg,
            "head",
        )
        engine = create_engine(cfg.get_main_option("sqlalchemy.url"))
        try:
            eq_(self._pragma(engine), 0)
        finally:
            engine.dispose()


class SQLiteForeignKeyOfflineTest(_SQLiteForeignKeyMigrationBase):
    def _configure_env(self, cfg, mode):
        env_file_fixture(
            """
url = config.get_main_option("sqlalchemy.url")
context.configure(
    url=url,
    literal_binds=True,
    dialect_opts={"paramstyle": "named"},
    sqlite_foreign_keys=%r,
)
with context.begin_transaction():
    context.run_migrations()
"""
            % mode
        )

    def test_offline_mode_emits_no_pragma_management(self):
        self._write_revision(
            "aaaa",
            None,
            """\
    op.execute("CREATE TABLE t (id INTEGER)")
""",
        )
        cfg = self._make_config("deferred")
        buf = io.StringIO()
        cfg.stdout = buf
        command.upgrade(cfg, "head", sql=True)
        output = buf.getvalue()
        assert "PRAGMA foreign_keys" not in output
        assert "PRAGMA foreign_key_check" not in output
