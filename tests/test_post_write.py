import importlib.util
from pathlib import Path
import sys

from sqlalchemy import create_engine

from alembic import command
from alembic import testing
from alembic import util
from alembic.operations import ops
from alembic.script import ScriptDirectory
from alembic.script import write_hooks
from alembic.testing import assert_raises_message
from alembic.testing import combinations
from alembic.testing import eq_
from alembic.testing import mock
from alembic.testing import TestBase
from alembic.testing.env import _get_staging_directory
from alembic.testing.env import _no_sql_pyproject_config
from alembic.testing.env import _no_sql_testing_config
from alembic.testing.env import clear_staging_env
from alembic.testing.env import staging_env
from alembic.util import compat


class HookTest(TestBase):
    def test_register(self):
        @write_hooks.register("my_writer")
        def my_writer(path, config):
            return path

        assert "my_writer" in write_hooks._registry

    def test_invoke(self):
        my_formatter = mock.Mock()
        write_hooks.register("my_writer")(my_formatter)

        write_hooks._invoke("my_writer", "/some/path", {"option": 1})

        my_formatter.assert_called_once_with("/some/path", {"option": 1})


class RunHookTest(TestBase):
    def setUp(self):
        self.env = staging_env()

    def tearDown(self):
        clear_staging_env()

    @testing.variation("config", ["ini", "toml"])
    def test_generic(self, config):
        hook1 = mock.Mock()
        hook2 = mock.Mock()

        write_hooks.register("hook1")(hook1)
        write_hooks.register("hook2")(hook2)

        if config.ini:
            self.cfg = _no_sql_testing_config(
                directives=(
                    "\n[post_write_hooks]\n"
                    "hooks=hook1,hook2\n"
                    "hook1.type=hook1\n"
                    "hook1.arg1=foo\n"
                    "hook2.type=hook2\n"
                    "hook2.arg1=bar\n"
                )
            )
        else:
            self.cfg = _no_sql_pyproject_config(directives="""

                [[tool.alembic.post_write_hooks]]
                name="hook1"
                type="hook1"
                arg1="foo"

                [[tool.alembic.post_write_hooks]]
                name="hook2"
                type="hook2"
                arg1="bar"
                """)
        rev = command.revision(self.cfg, message="x")

        eq_(
            hook1.mock_calls,
            [
                mock.call(
                    rev.path,
                    {"type": "hook1", "arg1": "foo", "_hook_name": "hook1"},
                )
            ],
        )
        eq_(
            hook2.mock_calls,
            [
                mock.call(
                    rev.path,
                    {"type": "hook2", "arg1": "bar", "_hook_name": "hook2"},
                )
            ],
        )

    def test_empty_section(self):
        self.cfg = _no_sql_testing_config(
            directives=("\n[post_write_hooks]\n")
        )
        command.revision(self.cfg, message="x")

    @testing.variation("config", ["ini", "toml"])
    def test_no_section(self, config):
        if config.ini:
            self.cfg = _no_sql_testing_config(directives="")
        else:
            self.cfg = _no_sql_pyproject_config(directives="""

                """)

        command.revision(self.cfg, message="x")

    def test_empty_hooks(self):
        self.cfg = _no_sql_testing_config(
            directives=("\n[post_write_hooks]\n" "hooks=\n")
        )

        command.revision(self.cfg, message="x")

    @testing.variation("config", ["ini", "toml"])
    def test_no_type(self, config):
        if config.ini:
            self.cfg = _no_sql_testing_config(
                directives=(
                    "\n[post_write_hooks]\n" "hooks=foo\n" "foo.bar=somebar\n"
                )
            )
        else:
            self.cfg = _no_sql_pyproject_config(directives="""

                [[tool.alembic.post_write_hooks]]
                name="foo"
                bar="somebar"
                """)

        assert_raises_message(
            util.CommandError,
            r"Key 'foo.type' \(or 'type' in toml\) is required "
            "for post write hook 'foo'",
            command.revision,
            self.cfg,
            message="x",
        )

    def test_console_scripts_entrypoint_missing(self):
        self.cfg = _no_sql_testing_config(
            directives=(
                "\n[post_write_hooks]\n"
                "hooks=black\n"
                "black.type=console_scripts\n"
            )
        )
        assert_raises_message(
            util.CommandError,
            "Key black.entrypoint is required for post write hook 'black'",
            command.revision,
            self.cfg,
            message="x",
        )

    def _run_black_with_config(
        self,
        input_config,
        expected_additional_arguments_fn,
        cwd=None,
        use_toml=False,
    ):
        if use_toml:
            self.cfg = _no_sql_pyproject_config(directives=input_config)
        else:
            self.cfg = _no_sql_testing_config(directives=input_config)

        retVal = [
            compat.EntryPoint(
                name="black",
                value="black.foo:patched_main",
                group="console_scripts",
            ),
            compat.EntryPoint(
                name="alembic",
                value="alembic.config:main",
                group="console_scripts",
            ),
        ]

        importlib_metadata_get = mock.Mock(return_value=retVal)
        with (
            mock.patch(
                "alembic.util.compat.importlib_metadata_get",
                importlib_metadata_get,
            ),
            mock.patch(
                "alembic.script.write_hooks.subprocess"
            ) as mock_subprocess,
        ):
            rev = command.revision(self.cfg, message="x")

        eq_(importlib_metadata_get.mock_calls, [mock.call("console_scripts")])
        eq_(
            mock_subprocess.mock_calls,
            [
                mock.call.run(
                    [
                        sys.executable,
                        "-c",
                        "import black.foo; black.foo.patched_main()",
                    ]
                    + expected_additional_arguments_fn(rev.path),
                    cwd=cwd,
                )
            ],
        )

    @testing.variation("config", ["ini", "toml"])
    def test_console_scripts(self, config):
        if config.ini:
            input_config = """
[post_write_hooks]
hooks = black
black.type = console_scripts
black.entrypoint = black
black.options = -l 79
            """
        else:
            input_config = """
    [[tool.alembic.post_write_hooks]]
    name = "black"
    type = "console_scripts"
    entrypoint = "black"
    options = "-l 79"
    """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path, "-l", "79"]

        self._run_black_with_config(
            input_config,
            expected_additional_arguments_fn,
            use_toml=config.toml,
        )

    @combinations(True, False, argnames="posix")
    @testing.variation("config", ["ini", "toml"])
    def test_filename_interpolation(self, posix, config):
        if config.ini:
            input_config = """
[post_write_hooks]
hooks = black
black.type = console_scripts
black.entrypoint = black
black.options = arg1 REVISION_SCRIPT_FILENAME 'multi-word arg' \
    --flag1='REVISION_SCRIPT_FILENAME'
        """
        else:
            input_config = """
[[tool.alembic.post_write_hooks]]
name = "black"
type = "console_scripts"
entrypoint = "black"
options = "arg1 REVISION_SCRIPT_FILENAME 'multi-word arg' --flag1='REVISION_SCRIPT_FILENAME'"
"""  # noqa: E501

        def expected_additional_arguments_fn(rev_path):
            if compat.is_posix:
                return [
                    "arg1",
                    rev_path,
                    "multi-word arg",
                    "--flag1=" + rev_path,
                ]
            else:
                return [
                    "arg1",
                    rev_path,
                    "'multi-word arg'",
                    "--flag1='%s'" % rev_path,
                ]

        with mock.patch("alembic.util.compat.is_posix", posix):
            self._run_black_with_config(
                input_config,
                expected_additional_arguments_fn,
                use_toml=config.toml,
            )

    def test_path_in_config(self):
        input_config = """
[post_write_hooks]
hooks = black
black.type = console_scripts
black.entrypoint = black
black.options = arg1 REVISION_SCRIPT_FILENAME --config %(here)s/pyproject.toml
        """

        def expected_additional_arguments_fn(rev_path):
            return [
                "arg1",
                rev_path,
                "--config",
                Path(_get_staging_directory(), "pyproject.toml")
                .absolute()
                .as_posix(),
            ]

        self._run_black_with_config(
            input_config, expected_additional_arguments_fn
        )

    def test_black_with_cwd(self):
        input_config = """
[post_write_hooks]
hooks = black
black.type = console_scripts
black.entrypoint = black
black.cwd = /path/to/cwd
        """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path]

        self._run_black_with_config(
            input_config, expected_additional_arguments_fn, cwd="/path/to/cwd"
        )

    def test_exec_executable_missing(self):
        self.cfg = _no_sql_testing_config(
            directives=(
                "\n[post_write_hooks]\n" "hooks=ruff\n" "ruff.type=exec\n"
            )
        )
        assert_raises_message(
            util.CommandError,
            "Key ruff.executable is required for post write hook 'ruff'",
            command.revision,
            self.cfg,
            message="x",
        )

    def _run_ruff_with_config(
        self,
        input_config,
        expected_additional_arguments_fn,
        executable="ruff",
        cwd=None,
    ):
        self.cfg = _no_sql_testing_config(directives=input_config)

        with mock.patch(
            "alembic.script.write_hooks.subprocess"
        ) as mock_subprocess:
            rev = command.revision(self.cfg, message="x")

        eq_(
            mock_subprocess.mock_calls,
            [
                mock.call.run(
                    [
                        executable,
                    ]
                    + expected_additional_arguments_fn(rev.path),
                    cwd=cwd,
                )
            ],
        )

    def test_exec_with_path_search(self):
        input_config = """
[post_write_hooks]
hooks = ruff
ruff.type = exec
ruff.executable = ruff
ruff.options = check --fix
        """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path, "check", "--fix"]

        self._run_ruff_with_config(
            input_config, expected_additional_arguments_fn
        )

    def test_exec_with_full_pathname(self):
        input_config = """
[post_write_hooks]
hooks = ruff
ruff.type = exec
ruff.executable = %(here)s/.venv/bin/ruff
ruff.options = check --fix
        """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path, "check", "--fix"]

        self._run_ruff_with_config(
            input_config,
            expected_additional_arguments_fn,
            executable=Path(_get_staging_directory(), ".venv/bin/ruff")
            .absolute()
            .as_posix(),
        )

    @combinations(True, False)
    def test_exec_with_filename_interpolation(self, posix):
        input_config = """
[post_write_hooks]
hooks = ruff
ruff.type = exec
ruff.executable = ruff
ruff.options = arg1 REVISION_SCRIPT_FILENAME 'multi-word arg' \
    --flag1='REVISION_SCRIPT_FILENAME'
        """

        def expected_additional_arguments_fn(rev_path):
            if compat.is_posix:
                return [
                    "arg1",
                    rev_path,
                    "multi-word arg",
                    "--flag1=" + rev_path,
                ]
            else:
                return [
                    "arg1",
                    rev_path,
                    "'multi-word arg'",
                    "--flag1='%s'" % rev_path,
                ]

        with mock.patch("alembic.util.compat.is_posix", posix):
            self._run_ruff_with_config(
                input_config, expected_additional_arguments_fn
            )

    def test_exec_with_path_in_config(self):
        input_config = """
[post_write_hooks]
hooks = ruff
ruff.type = exec
ruff.executable = ruff
ruff.options = arg1 REVISION_SCRIPT_FILENAME --config %(here)s/pyproject.toml
        """

        def expected_additional_arguments_fn(rev_path):
            return [
                "arg1",
                rev_path,
                "--config",
                Path(_get_staging_directory(), "pyproject.toml")
                .absolute()
                .as_posix(),
            ]

        self._run_ruff_with_config(
            input_config, expected_additional_arguments_fn
        )

    def test_exec_with_cwd(self):
        input_config = """
[post_write_hooks]
hooks = ruff
ruff.type = exec
ruff.executable = ruff
ruff.cwd = /path/to/cwd
        """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path]

        self._run_ruff_with_config(
            input_config, expected_additional_arguments_fn, cwd="/path/to/cwd"
        )

    def test_module_config_missing(self):
        self.cfg = _no_sql_testing_config(directives=("""
[post_write_hooks]
hooks = ruff
ruff.type = module
                """))
        assert_raises_message(
            util.CommandError,
            "Key ruff.module is required for post write hook 'ruff'",
            command.revision,
            self.cfg,
            message="x",
        )

    def test_module_not_found(self):
        self.cfg = _no_sql_testing_config(directives=("""
[post_write_hooks]
hooks = ruff
ruff.type = module
ruff.module = ruff_not_found
                """))
        assert_raises_message(
            util.CommandError,
            "Could not find module ruff_not_found",
            command.revision,
            self.cfg,
            message="x",
        )

    def _run_black_module_with_config(
        self,
        input_config,
        expected_additional_arguments_fn,
        cwd=None,
        use_toml=False,
    ):
        if use_toml:
            self.cfg = _no_sql_pyproject_config(directives=input_config)
        else:
            self.cfg = _no_sql_testing_config(directives=input_config)

        black_module_spec = importlib.util.find_spec("black")

        importlib_util_find_spec = mock.Mock(return_value=black_module_spec)
        with (
            mock.patch(
                "importlib.util.find_spec",
                importlib_util_find_spec,
            ),
            mock.patch(
                "alembic.script.write_hooks.subprocess"
            ) as mock_subprocess,
        ):
            rev = command.revision(self.cfg, message="x")

        eq_(importlib_util_find_spec.mock_calls, [mock.call("black")])
        eq_(
            mock_subprocess.mock_calls,
            [
                mock.call.run(
                    [
                        sys.executable,
                        "-m",
                        "black",
                    ]
                    + expected_additional_arguments_fn(rev.path),
                    cwd=cwd,
                )
            ],
        )

    @testing.variation("config", ["ini", "toml"])
    def test_module(self, config):
        if config.ini:
            input_config = """
[post_write_hooks]
hooks = black
black.type = module
black.module = black
black.options = -l 79
            """
        else:
            input_config = """
    [[tool.alembic.post_write_hooks]]
    name = "black"
    type = "module"
    module = "black"
    options = "-l 79"
    """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path, "-l", "79"]

        self._run_black_module_with_config(
            input_config,
            expected_additional_arguments_fn,
            use_toml=config.toml,
        )

    @combinations(True, False, argnames="posix")
    @testing.variation("config", ["ini", "toml"])
    def test_module_filename_interpolation(self, posix, config):
        if config.ini:
            input_config = """
[post_write_hooks]
hooks = black
black.type = module
black.module = black
black.options = arg1 REVISION_SCRIPT_FILENAME 'multi-word arg' \
    --flag1='REVISION_SCRIPT_FILENAME'
        """
        else:
            input_config = """
[[tool.alembic.post_write_hooks]]
name = "black"
type = "module"
module = "black"
options = "arg1 REVISION_SCRIPT_FILENAME 'multi-word arg' --flag1='REVISION_SCRIPT_FILENAME'"
"""  # noqa: E501

        def expected_additional_arguments_fn(rev_path):
            if compat.is_posix:
                return [
                    "arg1",
                    rev_path,
                    "multi-word arg",
                    "--flag1=" + rev_path,
                ]
            else:
                return [
                    "arg1",
                    rev_path,
                    "'multi-word arg'",
                    "--flag1='%s'" % rev_path,
                ]

        with mock.patch("alembic.util.compat.is_posix", posix):
            self._run_black_module_with_config(
                input_config,
                expected_additional_arguments_fn,
                use_toml=config.toml,
            )

    def test_module_path_in_config(self):
        input_config = """
[post_write_hooks]
hooks = black
black.type = module
black.module = black
black.options = arg1 REVISION_SCRIPT_FILENAME --config %(here)s/pyproject.toml
        """

        def expected_additional_arguments_fn(rev_path):
            return [
                "arg1",
                rev_path,
                "--config",
                Path(_get_staging_directory(), "pyproject.toml")
                .absolute()
                .as_posix(),
            ]

        self._run_black_module_with_config(
            input_config, expected_additional_arguments_fn
        )

    def test_module_black_with_cwd(self):
        input_config = """
[post_write_hooks]
hooks = black
black.type = module
black.module = black
black.cwd = /path/to/cwd
        """

        def expected_additional_arguments_fn(rev_path):
            return [rev_path]

        self._run_black_module_with_config(
            input_config, expected_additional_arguments_fn, cwd="/path/to/cwd"
        )


class MultiRevisionHookTransactionTest(TestBase):
    """Test that a batch of revisions generated in one run is finalized
    atomically: a post write hook failure on a later revision rolls back
    every file written earlier in the batch, and keeps them out of the
    revision map until the whole group succeeds."""

    rev_id_a = "batchreva"
    rev_id_b = "batchrevb"

    def setUp(self):
        self.env = staging_env()
        self.cfg = _no_sql_testing_config(
            directives=(
                "\n[post_write_hooks]\n"
                "hooks=batch_hook\n"
                "batch_hook.type=batch_hook\n"
            )
        )
        self.cfg.set_main_option("revision_environment", "true")

        # hook state: while True, the hook raises when it sees the second
        # revision of the batch.
        self.fail_on_second = True
        self.hook_calls: list[str] = []

        @write_hooks.register("batch_hook")
        def batch_hook(path, options):
            # the hook must keep receiving the real path of the generated
            # revision file, unchanged by the batching machinery.
            self.hook_calls.append(path)
            if self.fail_on_second and Path(path).name.startswith(
                self.rev_id_b
            ):
                raise RuntimeError("simulated second-file hook failure")

        self._batch_hook = batch_hook
        self.engine = create_engine("sqlite://")

    def tearDown(self):
        write_hooks._registry.pop("batch_hook", None)
        clear_staging_env()

    def _env_fixture(self, fn):
        def run_env(self_):
            from alembic import context

            with self.engine.connect() as connection:
                context.configure(
                    connection=connection,
                    process_revision_directives=fn,
                )
                with context.begin_transaction():
                    context.run_migrations()

        return mock.patch(
            "alembic.script.base.ScriptDirectory.run_env", run_env
        )

    def _two_revision_directives(self, context, revision, directives):
        directives[:] = [
            ops.MigrationScript(
                self.rev_id_a,
                ops.UpgradeOps(),
                ops.DowngradeOps(),
                message="first",
            ),
            ops.MigrationScript(
                self.rev_id_b,
                ops.UpgradeOps(),
                ops.DowngradeOps(),
                message="second",
            ),
        ]

    def _version_dir(self):
        return Path(_get_staging_directory(), "scripts", "versions")

    def test_hook_failure_rolls_back_whole_batch_then_rerun_succeeds(self):
        # first run: the hook fails on the second revision file
        with self._env_fixture(self._two_revision_directives):
            assert_raises_message(
                RuntimeError,
                "simulated second-file hook failure",
                command.revision,
                self.cfg,
                message="batch",
            )

        # nothing from the failed batch may remain on disk ...
        remaining = sorted(p.name for p in self._version_dir().glob("*.py"))
        eq_(remaining, [])

        # ... nor in a freshly loaded revision map
        rolled_back = ScriptDirectory.from_config(self.cfg)
        eq_(rolled_back.get_heads(), [])
        for missing_rev_id in (self.rev_id_a, self.rev_id_b):
            assert_raises_message(
                util.CommandError,
                "Can't locate revision identified by '%s'" % missing_rev_id,
                rolled_back.get_revision,
                missing_rev_id,
            )

        # the first file's hook did run with its real path before the second
        # file's hook failed
        eq_(len(self.hook_calls), 2)
        assert Path(self.hook_calls[0]).name.startswith(self.rev_id_a)
        assert Path(self.hook_calls[1]).name.startswith(self.rev_id_b)

        # recover: same revision ids, hook now succeeds
        self.fail_on_second = False
        with self._env_fixture(self._two_revision_directives):
            revs = command.revision(self.cfg, message="batch")
        eq_([r.revision for r in revs], [self.rev_id_a, self.rev_id_b])

        # exactly one group of files is present on disk
        remaining = sorted(p.name for p in self._version_dir().glob("*.py"))
        eq_(len(remaining), 2)
        assert remaining[0].startswith(self.rev_id_a)
        assert remaining[1].startswith(self.rev_id_b)

        # exactly one group is present in the revision graph, with the second
        # revision chained onto the first
        script = ScriptDirectory.from_config(self.cfg)
        eq_(
            sorted(script.get_heads()),
            [self.rev_id_b],
        )
        rev_a = script.get_revision(self.rev_id_a)
        rev_b = script.get_revision(self.rev_id_b)
        eq_(rev_b.down_revision, self.rev_id_a)
        eq_(rev_a.nextrev, {self.rev_id_b})

        # the hooks were invoked once per regenerated file with real paths
        eq_(len(self.hook_calls), 4)

    def test_rollback_does_not_remove_pre_existing_file(self):
        # place a pre-existing, valid revision file whose name does not
        # collide with the generated files.  Rollback of the failed batch
        # must leave this file untouched.
        pre_existing_rev_id = "preexist0"
        version_dir = self._version_dir()
        version_dir.mkdir(parents=True, exist_ok=True)
        pre_existing_name = "%s_keepme.py" % pre_existing_rev_id
        pre_existing = version_dir / pre_existing_name
        pre_existing.write_text(
            '"""pre-existing user revision"""\n'
            "revision = %r\n"
            "down_revision = None\n"
            "branch_labels = None\n"
            "depends_on = None\n\n"
            "def upgrade():\n"
            "    pass\n\n"
            "def downgrade():\n"
            "    pass\n" % pre_existing_rev_id
        )

        with self._env_fixture(self._two_revision_directives):
            assert_raises_message(
                RuntimeError,
                "simulated second-file hook failure",
                command.revision,
                self.cfg,
                message="batch",
            )

        # both files created by the failed batch are removed ...
        remaining = sorted(p.name for p in version_dir.glob("*.py"))
        eq_(remaining, [pre_existing_name])

        # ... while the pre-existing file survives
        assert pre_existing.exists()

        # the revision map reflects only the pre-existing revision; the
        # revisions of the failed batch are absent
        rolled_back = ScriptDirectory.from_config(self.cfg)
        eq_(rolled_back.get_heads(), [pre_existing_rev_id])
        for missing_rev_id in (self.rev_id_a, self.rev_id_b):
            assert_raises_message(
                util.CommandError,
                "Can't locate revision identified by '%s'" % missing_rev_id,
                rolled_back.get_revision,
                missing_rev_id,
            )

    def test_rollback_keeps_same_named_pre_existing_file(self):
        # a file that already exists at the target path of a generated
        # revision must never be deleted by rollback, even though its
        # contents are overwritten by template generation.  The pre-existing
        # file uses its own revision id (only the filename collides) and
        # serves as the head the batch generates against.
        pre_existing_rev_id = "preexist0"
        version_dir = self._version_dir()
        version_dir.mkdir(parents=True, exist_ok=True)
        collision_name = "%s_first.py" % self.rev_id_a
        collision_path = version_dir / collision_name
        collision_path.write_text(
            '"""user owned revision, do not delete"""\n'
            "revision = %r\n"
            "down_revision = None\n"
            "branch_labels = None\n"
            "depends_on = None\n\n"
            "def upgrade():\n"
            "    pass\n\n"
            "def downgrade():\n"
            "    pass\n" % pre_existing_rev_id
        )

        with self._env_fixture(self._two_revision_directives):
            assert_raises_message(
                RuntimeError,
                "simulated second-file hook failure",
                command.revision,
                self.cfg,
                message="batch",
            )

        # the second, newly created file is gone ...
        assert not (
            version_dir / ("%s_second.py" % self.rev_id_b)
        ).exists()
        # ... but the same-named pre-existing path is preserved
        assert collision_path.exists()

