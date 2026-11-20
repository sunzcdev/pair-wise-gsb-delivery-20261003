import os
import sys

from sqlalchemy.exc import OperationalError

from alembic import command
from alembic import testing
from alembic import util
from alembic.environment import EnvironmentContext
from alembic.migration import MigrationContext
from alembic.script import ScriptDirectory
from alembic.testing import config
from alembic.testing import eq_
from alembic.testing import expect_raises
from alembic.testing import is_
from alembic.testing import mock
from alembic.testing.assertions import expect_raises_message
from alembic.testing.env import _get_staging_directory
from alembic.testing.env import _no_sql_testing_config
from alembic.testing.env import _sqlite_file_db
from alembic.testing.env import _sqlite_testing_config
from alembic.testing.env import clear_staging_env
from alembic.testing.env import staging_env
from alembic.testing.env import write_script
from alembic.testing.fixtures import capture_context_buffer
from alembic.testing.fixtures import TestBase


class EnvironmentTest(TestBase):
    def setUp(self):
        staging_env()
        self.cfg = _no_sql_testing_config()

    def tearDown(self):
        clear_staging_env()

    def _fixture(self, **kw):
        script = ScriptDirectory.from_config(self.cfg)
        env = EnvironmentContext(self.cfg, script, **kw)
        return env

    def test_x_arg(self):
        env = self._fixture()
        self.cfg.cmd_opts = mock.Mock(x="y=5")
        eq_(env.get_x_argument(), "y=5")

    def test_x_arg_asdict(self):
        env = self._fixture()
        self.cfg.cmd_opts = mock.Mock(x=["y=5"])
        eq_(env.get_x_argument(as_dictionary=True), {"y": "5"})

    def test_x_arg_no_opts(self):
        env = self._fixture()
        eq_(env.get_x_argument(), [])

    def test_x_arg_no_opts_asdict(self):
        env = self._fixture()
        eq_(env.get_x_argument(as_dictionary=True), {})

    def test_x_arg_empty_value(self):
        env = self._fixture()
        self.cfg.cmd_opts = mock.Mock(x=["y"])
        eq_(env.get_x_argument(as_dictionary=True), {"y": ""})

    def test_tag_arg(self):
        env = self._fixture(tag="x")
        eq_(env.get_tag_argument(), "x")

    def test_migration_context_has_config(self):
        env = self._fixture()
        env.configure(url="sqlite://")
        ctx = env._migration_context
        is_(ctx.config, self.cfg)

        ctx = MigrationContext(ctx.dialect, None, {})
        is_(ctx.config, None)

    def test_sql_mode_parameters(self):
        env = self._fixture()

        a_rev = "arev"
        env.script.generate_revision(a_rev, "revision a", refresh=True)
        write_script(
            env.script,
            a_rev,
            """\
"Rev A"
revision = '{}'
down_revision = None

from alembic import op

def upgrade():
    op.execute('''
        do some SQL thing with a % percent sign %
    ''')

""".format(a_rev),
        )
        with capture_context_buffer(transactional_ddl=True) as buf:
            command.upgrade(self.cfg, "arev", sql=True)
        assert "do some SQL thing with a % percent sign %" in buf.getvalue()

    @config.requirements.legacy_engine
    @testing.uses_deprecated(
        r"The Engine.execute\(\) function/method is considered legacy"
    )
    def test_error_on_passing_engine(self):
        env = self._fixture()

        engine = _sqlite_file_db()

        a_rev = "arev"
        env.script.generate_revision(a_rev, "revision a", refresh=True)
        write_script(
            env.script,
            a_rev,
            """\
"Rev A"
revision = '%s'
down_revision = None

from alembic import op


def upgrade():
    pass


def downgrade():
    pass

""" % a_rev,
        )
        migration_fn = mock.MagicMock()

        def upgrade(rev, context):
            migration_fn(rev, context)
            return env.script._upgrade_revs(a_rev, rev)

        with expect_raises_message(
            util.CommandError,
            r"'connection' argument to configure\(\) is "
            r"expected to be a sqlalchemy.engine.Connection ",
        ):
            env.configure(
                connection=engine, fn=upgrade, transactional_ddl=False
            )


class VersionFinalizeCallbackTest(TestBase):
    """Run the same two-step migration, where the second step fails with
    a database error, on both sides of the transaction_per_migration
    switch (plus an externally-managed transaction), and verify the
    on_version_finalize events against the versions left in the database.
    """

    def setUp(self):
        staging_env()
        self.cfg = _sqlite_testing_config()
        self.bind = _sqlite_file_db()

    def tearDown(self):
        clear_staging_env()

    def _env_with_scripts(self):
        env = EnvironmentContext(
            self.cfg, ScriptDirectory.from_config(self.cfg)
        )
        a_rev = "arev"
        b_rev = "brev"
        env.script.generate_revision(a_rev, "revision a", refresh=True)
        write_script(
            env.script,
            a_rev,
            """\
"Rev A"
revision = 'arev'
down_revision = None

from alembic import op
from sqlalchemy import Column, Integer


def upgrade():
    op.create_table("foo", Column("x", Integer))


def downgrade():
    op.drop_table("foo")

""",
        )
        env.script.generate_revision(b_rev, "revision b", refresh=True)
        write_script(
            env.script,
            b_rev,
            """\
"Rev B"
revision = 'brev'
down_revision = 'arev'

from alembic import op


def upgrade():
    # fails with the database's own error
    op.execute("INSERT INTO table_that_does_not_exist (x) VALUES (1)")


def downgrade():
    pass

""",
        )
        return env

    def _run_failing_migration(
        self, transaction_per_migration, external_transaction=False
    ):
        env = self._env_with_scripts()
        events = []

        connection = self.bind.connect()
        if external_transaction:
            outer_transaction = connection.begin()
        else:
            outer_transaction = None

        env.configure(
            connection=connection,
            fn=lambda heads, context: env.script._upgrade_revs(
                "brev", heads
            ),
            transactional_ddl=True,
            transaction_per_migration=transaction_per_migration,
            on_version_apply=lambda **kw: events.append(("apply", kw)),
            on_version_finalize=lambda **kw: events.append(("finalize", kw)),
        )
        context = env.get_context()

        # the original database exception from the failing second step
        # propagates to the caller unchanged
        with expect_raises(OperationalError):
            with context.begin_transaction():
                env.run_migrations()

        if outer_transaction is not None:
            outer_transaction.rollback()
        connection.close()
        return events

    def _current_heads(self):
        with self.bind.connect() as conn:
            context = MigrationContext.configure(conn)
            return context.get_current_heads()

    def _assert_events(self, events, expected_finalize_state):
        eq_([kind for kind, kw in events], ["apply", "finalize"])

        apply_kw = events[0][1]
        eq_(apply_kw["step"].up_revision_id, "arev")
        assert hasattr(apply_kw["ctx"], "get_current_revision")

        finalize_kw = events[1][1]
        eq_(finalize_kw["step"].up_revision_id, "arev")
        eq_(finalize_kw["heads"], {"arev"})
        eq_(finalize_kw["run_args"], {})
        eq_(finalize_kw["state"], expected_finalize_state)

    def test_transaction_per_migration(self):
        events = self._run_failing_migration(transaction_per_migration=True)

        # step one's transaction was committed before step two failed
        self._assert_events(events, "committed")
        eq_(self._current_heads(), ("arev",))

    def test_transaction_per_run(self):
        events = self._run_failing_migration(transaction_per_migration=False)

        # step one was only executed; its transaction was still open and
        # rolled back along with the failing second step
        self._assert_events(events, "executed")
        eq_(self._current_heads(), ())

    def test_external_transaction(self):
        events = self._run_failing_migration(
            transaction_per_migration=False, external_transaction=True
        )

        # the transaction belongs to the caller, who rolls it back
        self._assert_events(events, "external")
        eq_(self._current_heads(), ())


class CWDTest(TestBase):
    def setUp(self):
        self.env = staging_env()
        self.cfg = _sqlite_testing_config()

    def tearDown(self):
        clear_staging_env()

    @testing.combinations(
        (
            ".",
            None,
            ["."],
        ),
        ("/tmp/foo:/tmp/bar", None, ["/tmp/foo", "/tmp/bar"]),
        ("/tmp/foo:/tmp/bar", ":", ["/tmp/foo", "/tmp/bar"]),
        ("/tmp/foo /tmp/bar", None, ["/tmp/foo", "/tmp/bar"]),
        ("/tmp/foo,/tmp/bar", None, ["/tmp/foo", "/tmp/bar"]),
        (". /tmp/foo", None, [".", "/tmp/foo"]),
        (". /tmp/foo", "space", [".", "/tmp/foo"]),
    )
    def test_sys_path_prepend(self, config_value, path_separator, expected):
        if path_separator is not None:
            self.cfg.set_main_option("path_separator", path_separator)
        self.cfg.set_main_option("prepend_sys_path", config_value)

        if path_separator is None:
            with testing.expect_deprecated(
                "No path_separator found in configuration;"
            ):
                script = ScriptDirectory.from_config(self.cfg)
        else:
            script = ScriptDirectory.from_config(self.cfg)
        env = EnvironmentContext(self.cfg, script)

        target = os.path.abspath(_get_staging_directory())

        def assert_(heads, context):
            eq_(
                [os.path.abspath(p) for p in sys.path[0 : len(expected)]],
                [os.path.abspath(p) for p in expected],
            )
            return []

        p = [p for p in sys.path if os.path.abspath(p) != target]
        with mock.patch.object(sys, "path", p):
            env.configure(url="sqlite://", fn=assert_)
            with env:
                script.run_env()
