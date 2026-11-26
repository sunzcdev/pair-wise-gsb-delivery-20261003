# Copyright 2016 Alethea Katherine Flowers
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

__lazy_modules__ = {
    "datetime",
    "hashlib",
    "humanize",
    "inspect",
    "nox.command",
    "nox.logger",
    "nox.virtualenv",
    "pathlib",
    "re",
    "shutil",
    "unicodedata",
}

import contextlib
import datetime as dt
import enum
import functools
import hashlib
import inspect
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time
import typing
import unicodedata

import humanize

import nox.command
import nox.virtualenv
from nox.diagnostics import (
    PHASE_ABORTED,
    PHASE_FAILED,
    PHASE_NOT_STARTED,
    PHASE_RUNNING,
    PHASE_SKIPPED,
    PHASE_SUCCESS,
    DiagnosticAction,
    HostAction,
    HostCheck,
    HostDiagnostic,
    HostDiagnosticAction,
    PhaseReport,
    PhaseState,
    host_action,
    host_check,
    host_diagnostic,
)
from nox.logger import logger
from nox.popen import DEFAULT_INTERRUPT_TIMEOUT, DEFAULT_TERMINATE_TIMEOUT
from nox.virtualenv import (
    CondaEnv,
    PassthroughEnv,
    ProcessEnv,
    RattlerEnv,
    VirtualEnv,
    get_virtualenv,
)

if typing.TYPE_CHECKING:
    from collections.abc import (
        Callable,
        Generator,
        Iterable,
        Iterator,
        Mapping,
        Sequence,
    )
    from types import TracebackType
    from typing import (
        IO,
        Any,
        Literal,
        NoReturn,
    )

    from nox._decorators import Func
    from nox._options import NoxConfig
    from nox.command import ExternalType
    from nox.manifest import Manifest

__all__ = [
    "PHASE_ABORTED",
    "PHASE_FAILED",
    "PHASE_NOT_STARTED",
    "PHASE_RUNNING",
    "PHASE_SKIPPED",
    "PHASE_SUCCESS",
    "DiagnosticAction",
    "HostAction",
    "HostCheck",
    "HostDiagnostic",
    "HostDiagnosticAction",
    "PhaseReport",
    "PhaseState",
    "Result",
    "Session",
    "SessionRunner",
    "Status",
    "host_action",
    "host_check",
    "host_diagnostic",
    "nox",
]


def __dir__() -> list[str]:
    return __all__


@contextlib.contextmanager
def _chdir(path: str) -> Generator[None, None, None]:
    """
    Change the current working directory to the given path.
    Follows python 3.11's chdir behaviour.
    """
    cwd = os.getcwd()
    try:
        os.chdir(path)
        yield
    finally:
        os.chdir(cwd)


def _normalize_path(envdir: str, path: str) -> str:
    """Normalizes a string to be a "safe" filesystem path for a virtualenv."""
    orig_path = path
    path = unicodedata.normalize("NFKD", path).encode("ascii", "ignore").decode("ascii")
    path = re.sub(r"[^\w\s-]", "-", path).strip().lower()
    path = re.sub(r"[-\s]+", "-", path)
    path = path.strip("-")

    if not path:
        path = hashlib.sha1(orig_path.encode("utf-8")).hexdigest()[:8]  # noqa: S324
        logger.warning(
            "The virtualenv name was hashed because it contains no usable "
            "ASCII characters."
        )

    full_path = os.path.join(envdir, path)
    if len(full_path) > 100 - len("bin/pythonX.Y"):
        if len(envdir) < 100 - 9:
            path = hashlib.sha1(path.encode("ascii")).hexdigest()[:8]  # noqa: S324
            full_path = os.path.join(envdir, path)
            logger.warning("The virtualenv name was hashed to avoid being too long.")
        else:
            logger.error(
                f"The virtualenv path {full_path} is too long and will cause issues on "
                "some environments. Use the --envdir path to modify where "
                "Nox stores virtualenvs."
            )

    return full_path


class _SessionQuit(Exception):
    pass


class _SessionSkip(Exception):
    pass


class Status(enum.Enum):
    ABORTED = -1
    FAILED = 0
    SUCCESS = 1
    SKIPPED = 2


class _WorkingDirContext:
    def __init__(self, dir: str | os.PathLike[str]) -> None:
        self._prev_working_dir = os.getcwd()
        os.chdir(dir)

    def __enter__(self) -> _WorkingDirContext:  # noqa: PYI034
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        os.chdir(self._prev_working_dir)


class Session:
    """The Session object is passed into each user-defined session function.

    This is your primary means for installing package and running commands in
    your Nox session.
    """

    __slots__ = ("_runner",)

    def __init__(self, runner: SessionRunner) -> None:
        self._runner = runner

    @property
    def __dict__(self) -> dict[str, SessionRunner]:  # type: ignore[override]
        """Attribute dictionary for object inspection.

        This is needed because ``__slots__`` turns off ``__dict__`` by
        default. Unlike a typical object, modifying the result of this
        dictionary won't allow modification of the instance.
        """
        return {"_runner": self._runner}

    @property
    def name(self) -> str:
        """The name of this session."""
        return self._runner.friendly_name

    @property
    def env(self) -> dict[str, str | None]:
        """A dictionary of environment variables to pass into all commands."""
        return self.virtualenv.env

    @property
    def env_dir(self) -> pathlib.Path:
        """The path to the environment of this session."""
        return pathlib.Path(self._runner.envdir)

    @property
    def posargs(self) -> list[str]:
        """Any extra arguments from the ``nox`` commandline or :class:`Session.notify`."""
        return self._runner.posargs

    @property
    def virtualenv(self) -> ProcessEnv:
        """The virtualenv that all commands are run in."""
        venv = self._runner.venv
        if venv is None:
            msg = "A virtualenv has not been created for this session"
            raise ValueError(msg)
        return venv

    @property
    def venv_backend(self) -> str:
        """The venv_backend selected."""
        venv = self._runner.venv
        if venv is None:
            return "none"
        return venv.venv_backend

    @property
    def python(self) -> str | Sequence[str] | bool | None:
        """The python version passed into ``@nox.session``."""
        return self._runner.func.python

    @property
    def bin_paths(self) -> list[str] | None:
        """The bin directories for the virtualenv."""
        return self.virtualenv.bin_paths

    @property
    def bin(self) -> str:
        """The first bin directory for the virtualenv."""
        paths = self.bin_paths
        if paths is None:
            msg = "The environment does not have a bin directory."
            raise ValueError(msg)
        return paths[0]

    def create_tmp(self) -> str:
        """Create, and return, a temporary directory."""
        tmpdir = os.path.join(self._runner.envdir, "tmp")
        os.makedirs(tmpdir, exist_ok=True)
        self.env["TMPDIR"] = os.path.abspath(tmpdir)
        return tmpdir

    @property
    def cache_dir(self) -> pathlib.Path:
        """Create and return a 'shared cache' directory to be used across sessions."""
        path = pathlib.Path(self._runner.global_config.envdir).joinpath(".cache")
        path.mkdir(parents=True, exist_ok=True)
        return path

    @property
    def interactive(self) -> bool:
        """Returns True if Nox is being run in an interactive session or False otherwise."""
        return not self._runner.global_config.non_interactive and sys.stdin.isatty()

    @property
    def parallel(self) -> bool:
        """Whether this session is running under Nox's parallel scheduler.

        This describes the execution mode, not the number of sessions
        currently running. It remains true for a session that runs alone
        because of dependencies or ``allow_parallel=False``.
        """
        return self._runner.global_config.parallel_worker

    def install_and_run_script(
        self,
        script: str | os.PathLike[str],
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: bool = False,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> str | bool | None:
        """
        Install dependencies and run a Python script.
        """
        deps = nox.project.load_toml(script).get("dependencies", [])
        if deps:
            self.install(*deps)

        return self.run(
            "python",
            script,
            *args,
            env=env,
            include_outer_env=include_outer_env,
            silent=silent,
            success_codes=success_codes,
            external=None,
            log=log,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    @property
    def invoked_from(self) -> str:
        """The directory that Nox was originally invoked from.

        Since you can use the ``--noxfile / -f`` command-line
        argument to run a Noxfile in a location different from your shell's
        current working directory, Nox automatically changes the working directory
        to the Noxfile's directory before running any sessions. This gives
        you the original working directory that Nox was invoked form.
        """
        return self._runner.global_config.invoked_from

    def chdir(self, dir: str | os.PathLike[str]) -> _WorkingDirContext:
        """Change the current working directory.

        Can be used as a context manager to automatically restore the working directory::

            with session.chdir("somewhere/deep/in/monorepo"):
                # Runs in "/somewhere/deep/in/monorepo"
                session.run("pytest")

            # Runs in original working directory
            session.run("flake8")

        """
        self.log(f"cd {dir}")
        return _WorkingDirContext(dir)

    cd = chdir
    """An alias for :meth:`chdir`."""

    def _run_func(self, func: Callable[..., Any], args: Iterable[Any]) -> Any:
        """Legacy support for running a function through :func`run`."""
        self.log(f"{func}(args={args!r})")
        try:
            return func(*args)
        except Exception as e:
            logger.exception(f"Function {func!r} raised {e!r}.")
            raise nox.command.CommandFailed() from e

    @typing.overload
    def run(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: Literal[False] = ...,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> bool | None: ...

    @typing.overload
    def run(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: Literal[True],
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> str | None: ...

    @typing.overload
    def run(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: bool,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> str | bool | None: ...

    def run(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: bool = False,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> str | bool | None:
        """Run a command.

        Commands must be specified as a list of strings, for example::

            session.run('pytest', '-k', 'fast', 'tests/')
            session.run('flake8', '--import-order-style=google')

        You **can not** just pass everything as one string. For example, this
        **will not work**::

            session.run('pytest -k fast tests/')

        You can set environment variables for the command using ``env``::

            session.run(
                'bash', '-c', 'echo $SOME_ENV',
                env={'SOME_ENV': 'Hello'})

        You can extend the shutdown timeout to allow long-running cleanup tasks to
        complete before being terminated. For example, if you wanted to allow ``pytest``
        extra time to clean up large projects in the case that Nox receives an
        interrupt signal from your build system and needs to terminate its child
        processes::

            session.run(
                'pytest', '-k', 'long_cleanup',
                interrupt_timeout=10.0,
                terminate_timeout=2.0)

        You can also tell Nox to treat non-zero exit codes as success using
        ``success_codes``. For example, if you wanted to treat the ``pytest``
        "tests discovered, but none selected" error as success::

            session.run(
                'pytest', '-k', 'not slow',
                success_codes=[0, 5])

        On Windows, builtin commands like ``del`` cannot be directly invoked,
        but you can use ``cmd /c`` to invoke them::

            session.run('cmd', '/c', 'del', 'docs/modules.rst')

        If ``session.run`` fails, it will stop the session and will not run the next steps.
        Basically, this will raise a Python exception. Taking this in count, you can use a
        ``try...finally`` block for cleanup runs, that will run even if the other runs fail::

           try:
               session.run("coverage", "run", "-m", "pytest")
           finally:
               # Display coverage report even when tests fail.
               session.run("coverage", "report")

        If you pass ``silent=True``, you can capture the output of a command that would
        otherwise be shown to the user. For example to get the current Git commit ID::

            out = session.run(
                "git", "rev-parse", "--short", "HEAD",
                external=True, silent=True
            )

            print("Current Git commit is", out.strip())

        :param env: A dictionary of environment variables to expose to the
            command. By default, all environment variables are passed. You
            can block an environment variable from the outer environment by
            setting it to None.
        :type env: dict or None
        :param include_outer_env: Boolean parameter that determines if the
            environment variables from the nox invocation environment should
            be passed to the command. ``True`` by default.
        :type include_outer_env: bool
        :param bool silent: Silence command output, unless the command fails.
            If ``True``, returns the command output (unless the command fails).
            ``False`` by default.
        :param success_codes: A list of return codes that are considered
            successful. By default, only ``0`` is considered success.
        :type success_codes: list, tuple, or None
        :param external: If False (the default) then programs not in the
            virtualenv path will cause a warning. If True, no warning will be
            emitted. These warnings can be turned into errors using
            ``--error-on-external-run``. This has no effect for sessions that
            do not have a virtualenv.
        :type external: bool
        :param interrupt_timeout: The timeout (in seconds) that Nox should wait after it
            and its children receive an interrupt signal before sending a terminate
            signal to its children. Set to ``None`` to never send a terminate signal.
            Default: ``0.3``
        :type interrupt_timeout: float or None
        :param terminate_timeout: The timeout (in seconds) that Nox should wait after it
            sends a terminate signal to its children before sending a kill signal to
            them. Set to ``None`` to never send a kill signal.
            Default: ``0.2``
        :type terminate_timeout: float or None
        :param stdout: Redirect standard output of the command into a file. Can't be
            combined with *silent*.
        :type stdout: file or file descriptor
        :param stderr: Redirect standard output of the command into a file. Can't be
            combined with *silent*.
        :type stderr: file or file descriptor
        """
        if not args:
            msg = "At least one argument required to run()."
            raise ValueError(msg)

        if len(args) == 1 and isinstance(args[0], (list, tuple)):
            msg = "First argument to `session.run` is a list. Did you mean to use `session.run(*args)`?"
            raise ValueError(msg)

        if self._runner.global_config.install_only:
            logger.info(f"Skipping {args[0]} run, as --install-only is set.")
            return None

        return self._run_user_command(
            *args,
            env=env,
            include_outer_env=include_outer_env,
            silent=silent,
            success_codes=success_codes,
            log=log,
            external=external,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    def run_install(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: bool = False,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> str | bool | None:
        """Run a command in the install step.

        This is a variant of :meth:`run` that runs even in the presence of
        ``--install-only``. This method returns early if ``--no-install`` is
        specified and the virtualenv is being reused. (In nox 2023.04.22 and
        earlier, this was called ``run_always``, and that continues to be
        available as an alias.)

        Here are some cases where this method is useful:

        - You need to install packages using a command other than ``pip
          install`` or ``conda install``.
        - You need to run a command as a prerequisite of package installation,
          such as building a package or compiling a binary extension.

        :param env: A dictionary of environment variables to expose to the
            command. By default, all environment variables are passed.
        :type env: dict or None
        :param include_outer_env: Boolean parameter that determines if the
            environment variables from the nox invocation environment should
            be passed to the command. ``True`` by default.
        :type include_outer_env: bool
        :param bool silent: Silence command output, unless the command fails.
            ``False`` by default.
        :param success_codes: A list of return codes that are considered
            successful. By default, only ``0`` is considered success.
        :type success_codes: list, tuple, or None
        :param external: If False (the default) then programs not in the
            virtualenv path will cause a warning. If True, no warning will be
            emitted. These warnings can be turned into errors using
            ``--error-on-external-run``. This has no effect for sessions that
            do not have a virtualenv.
        :type external: bool
        :param interrupt_timeout: The timeout (in seconds) that Nox should wait after it
            and its children receive an interrupt signal before sending a terminate
            signal to its children. Set to ``None`` to never send a terminate signal.
            Default: ``0.3``
        :type interrupt_timeout: float or None
        :param terminate_timeout: The timeout (in seconds) that Nox should wait after it
            sends a terminate signal to its children before sending a kill signal to
            them. Set to ``None`` to never send a kill signal.
            Default: ``0.2``
        :type terminate_timeout: float or None
        """
        if (
            self._runner.global_config.no_install
            and self._runner.venv is not None
            and self._runner.venv._reused
        ):
            # Preserve run_always()'s historical skip rule while making a
            # cleanup-phase skip visible in structured reports.  Do not erase
            # an earlier cleanup command's success/failure settlement.
            if self._runner.resolve_command_phase() == "cleanup":
                cleanup_phase = self._runner.phases.cleanup
                if not cleanup_phase.started:
                    cleanup_phase.skip("run_always skipped by its existing reuse rule")
            return None

        if not args:
            msg = "At least one argument required to run_install()"
            raise ValueError(msg)

        run_call = functools.partial(
            self._run,
            env=env,
            include_outer_env=include_outer_env,
            silent=silent,
            success_codes=success_codes,
            log=log,
            external=external,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

        # run_always()/run_install() keep their original environment and skip
        # rules. When one is used as a finally/except cleanup step, only add
        # report bookkeeping; do not change how the command executes.
        if self._runner.resolve_command_phase() == "cleanup":
            with self._runner.command_phase("cleanup"):
                return run_call(*args)
        return run_call(*args)

    def run_always(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: bool = False,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> str | bool | None:
        """This is an alias to ``run_install``, which better describes the use case.

        :meta private:
        """

        return self.run_install(
            *args,
            env=env,
            include_outer_env=include_outer_env,
            silent=silent,
            success_codes=success_codes,
            log=log,
            external=external,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    @contextlib.contextmanager
    def phase(self, name: str) -> Generator[PhaseState, None, None]:
        """Explicitly mark commands in a report phase (for example cleanup)."""

        with self._runner.phase(name) as phase:
            yield phase

    def cleanup(
        self,
        *args: str | os.PathLike[str],
        **kwargs: Any,
    ) -> str | bool | None:
        """Run an ordinary cleanup command and record its separate state."""

        with self.phase("cleanup"):
            return self.run(*args, **kwargs)

    # Common aliases used by noxfiles coming from other task-runner idioms.
    clean = cleanup
    teardown = cleanup

    def register_host_diagnostic(
        self, target: Any = None, /, *args: Any, **options: Any
    ) -> HostDiagnostic:
        """Register a host diagnostic for this runner.

        Registrations made from the session body are useful to prepared
        sessions. To survive interpreter/creation failures, register on the
        decorated :class:`~nox._decorators.Func` before Nox executes the
        session.
        """

        if isinstance(target, HostDiagnostic) and not args and not options:
            action = target
        else:
            action = HostDiagnostic(target, *args, **options)
        # Keep a persistent registration as well as the runtime list used by
        # older callers; _registered_diagnostic_actions() de-duplicates them.
        self._runner.host_diagnostics.append(action)
        self._runner.runtime_diagnostics.append(action)
        return action

    register_diagnostic = register_host_diagnostic
    add_diagnostic = register_host_diagnostic
    add_host_diagnostic = register_host_diagnostic
    register_diagnostic_action = register_host_diagnostic
    add_diagnostic_action = register_host_diagnostic
    register_host_diagnostic_action = register_host_diagnostic
    add_host_diagnostic_action = register_host_diagnostic
    register_host_action = register_host_diagnostic
    add_host_action = register_host_diagnostic

    def register_action(
        self,
        kind: Any = "host_diagnostic",
        target: Any = None,
        /,
        *args: Any,
        **options: Any,
    ) -> HostDiagnostic:
        """Register an explicitly named pre-environment action.

        The common form is ``register_action("host_diagnostic", target)``; the
        action kind may be omitted when the first argument is the target.
        """
        if target is None and not isinstance(kind, str):
            kind, target = "host_diagnostic", kind
        normalized_kind = (
            kind.lower().replace("-", "_")
            if isinstance(kind, str)
            else "host_diagnostic"
        )
        if target is None:
            def decorate(func: Callable[..., Any]) -> Callable[..., Any]:
                self.register_host_diagnostic(
                    func, *args, kind=normalized_kind, **options
                )
                return func

            return decorate
        if isinstance(kind, str):
            if "diagnostic" not in normalized_kind and normalized_kind not in {
                "host",
                "host_action",
            }:
                msg = f"Unsupported pre-environment action kind {kind!r}"
                raise ValueError(msg)
            options.setdefault("kind", normalized_kind)
        return self.register_host_diagnostic(target, *args, **options)

    register = register_action
    add_action = register_action

    def diagnose(
        self, target: Any = None, /, *args: Any, **options: Any
    ) -> dict[str, Any]:
        """Run one host-only diagnostic immediately and record its state."""

        action = (
            target
            if isinstance(target, HostDiagnostic) and not args and not options
            else HostDiagnostic(target, *args, **options)
        )
        return self._runner.run_diagnostic_now(action)

    # Verb-first and domain-language aliases. These execute on the host even
    # when called from a prepared session; ordinary run()/run_always() still
    # use the session's real environment.
    host_diagnostic = diagnose
    host_diagnose = diagnose
    diagnose_host = diagnose
    run_diagnostic = diagnose
    run_host_diagnostic = diagnose
    diagnostic = diagnose
    action = diagnose
    run_action = diagnose
    run_host = diagnose
    host_run = diagnose
    host_check = diagnose
    run_host_check = diagnose

    @property
    def host(self) -> HostCommandNamespace:
        """Namespace for explicit host-only diagnostic calls."""

        return HostCommandNamespace(self)

    @property
    def diagnostics(self) -> HostCommandNamespace:
        """Namespace for registering/running diagnostic actions."""

        return HostCommandNamespace(self)

    def _run_user_command(self, *args: Any, **kwargs: Any) -> str | bool | None:
        """Run a public ``session.run`` call and settle its report phase."""

        phase_name = self._runner.resolve_command_phase()
        with self._runner.command_phase(phase_name):
            return self._run(*args, **kwargs)

    def _run(
        self,
        *args: str | os.PathLike[str],
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool,
        silent: bool,
        success_codes: Iterable[int] | None,
        log: bool,
        external: ExternalType | None,
        stdout: int | IO[str] | None,
        stderr: int | IO[str] | None,
        interrupt_timeout: float | None,
        terminate_timeout: float | None,
    ) -> str | bool:
        """Like run(), except that it runs even if --install-only is provided."""
        # Legacy support - run a function given.
        if callable(args[0]):
            return self._run_func(args[0], args[1:])  # type: ignore[unreachable]

        # Using `"uv"` or `"uvx" when `uv` is the backend is guaranteed to
        # work, even if it was co-installed with nox.
        if self.virtualenv.venv_backend == "uv" and nox.virtualenv.UV != "uv":
            if (
                args[0] == "uv"
                and shutil.which("uv", path=self.bin)
                is None  # Session uv takes priority
            ):
                args = (nox.virtualenv.UV, *args[1:])
            elif args[0] == "uvx" and shutil.which("uvx", path=self.bin) is None:
                args = (nox.virtualenv.uvx_path(nox.virtualenv.UV), *args[1:])

        # Combine the env argument with our virtualenv's env vars.
        env = self.virtualenv._get_env(env or {}, include_outer_env=include_outer_env)

        # If --error-on-external-run is specified, error on external programs.
        if self._runner.global_config.error_on_external_run and external is None:
            external = "error"

        # Allow all external programs when running outside a sandbox.
        if (
            not self.virtualenv.is_sandboxed
            or args[0] in self.virtualenv.allowed_globals
        ):
            external = True

        if external is None:
            external = False

        # Run a shell command.
        return nox.command.run(
            args,
            env=env,
            paths=self.bin_paths,
            silent=silent,
            success_codes=success_codes,
            log=log,
            external=external,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    def conda_install(
        self,
        *args: str,
        auto_offline: bool = True,
        channel: str | Sequence[str] = "",
        env: Mapping[str, str] | None = None,
        include_outer_env: bool = True,
        silent: bool | None = None,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> None:
        """Install invokes `conda install`_ to install packages inside of the
        session's environment.

        To install packages directly::

            session.conda_install('pandas')
            session.conda_install('numpy', 'scipy')
            session.conda_install('dask==2.1.0', channel='conda-forge')

        To install packages from a ``requirements.txt`` file::

            session.conda_install('--file', 'requirements.txt')
            session.conda_install('--file', 'requirements-dev.txt')

        By default this method will detect when internet connection is not
        available and will add the `--offline` flag automatically in that case.
        To disable this behaviour, set `auto_offline=False`.

        To install the current package without clobbering conda-installed
        dependencies::

            session.install('.', '--no-deps')
            # Install in editable mode.
            session.install('-e', '.', '--no-deps')

        You can specify a conda channel using `channel=`; a falsey value will
        not change the current channels. You can specify a list of channels if
        needed. It is highly recommended to specify this; micromamba does not
        set default channels, and default channels vary for conda. Note that
        "defaults" is also not permissively licensed like "conda-forge" is.

        The ``rattler`` backend runs in-process and accepts specs,
        ``--channel`` and ``--file`` only; other ``conda install`` options
        raise ``ValueError``.

        Additional keyword args are the same as for :meth:`run`.

        .. _conda install:
        """
        venv = self._runner.venv

        prefix_args: tuple[str, ...] = ()
        if isinstance(venv, CondaEnv):
            prefix_args = ("--prefix", venv.location)
        elif not isinstance(venv, PassthroughEnv):
            msg = (
                "A session without a conda environment can not install dependencies"
                " from conda."
            )
            raise TypeError(msg)

        if not args:
            msg = "At least one argument required to install()."
            raise ValueError(msg)

        if self._runner.global_config.no_install and (
            isinstance(venv, PassthroughEnv) or venv._reused
        ):
            return

        if silent is None:
            silent = not self._runner.global_config.verbose

        offline = auto_offline and venv.is_offline()
        if offline:
            logger.warning(
                "Automatically setting the `--offline` flag as conda repo seems"
                " unreachable."
            )
        channels = [channel] if isinstance(channel, str) else list(channel)
        channels = [c for c in channels if c]

        if isinstance(venv, RattlerEnv):
            if log:
                logger.info(f"rattler install {' '.join(args)}")
            venv.install(*args, channel=channels, offline=offline)
            return

        extraopts: list[str] = []
        if offline:
            extraopts.append("--offline")
        extraopts += [f"--channel={c}" for c in channels]

        self._run(
            venv.conda_cmd,
            "install",
            "--yes",
            *extraopts,
            *prefix_args,
            *args,
            env=env,
            include_outer_env=include_outer_env,
            silent=silent,
            success_codes=success_codes,
            log=log,
            external="error",
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    def install(
        self,
        *args: str,
        env: Mapping[str, str | None] | None = None,
        include_outer_env: bool = True,
        silent: bool | None = None,
        success_codes: Iterable[int] | None = None,
        log: bool = True,
        external: ExternalType | None = None,  # noqa: ARG002
        stdout: int | IO[str] | None = None,
        stderr: int | IO[str] | None = subprocess.STDOUT,
        interrupt_timeout: float | None = DEFAULT_INTERRUPT_TIMEOUT,
        terminate_timeout: float | None = DEFAULT_TERMINATE_TIMEOUT,
    ) -> None:
        """Install invokes `pip`_ to install packages inside of the session's
        virtualenv.

        To install packages directly::

            session.install('pytest')
            session.install('requests', 'mock')
            session.install('requests[security]==2.9.1')

        To install packages from a ``requirements.txt`` file::

            session.install('-r', 'requirements.txt')
            session.install('-r', 'requirements-dev.txt')

        To install the current package::

            session.install('.')
            # Install in editable mode.
            session.install('-e', '.')

        Additional keyword args are the same as for :meth:`run`.

        .. warning::

            Running ``session.install`` without a virtual environment
            is no longer supported. If you still want to do that, please
            use ``session.run("pip", "install", ...)`` instead.

        .. warning::

           The ``uv`` backend does not reinstall, even for local packages, so
           you need to include ``--reinstall-package <pkg-name>`` (uv-only) if
           reusing the environment.

        .. _pip: https://pip.readthedocs.org
        """
        venv = self._runner.venv

        if not isinstance(
            venv, (CondaEnv, VirtualEnv, PassthroughEnv)
        ):  # pragma: no cover
            msg = f"A session without a virtualenv (got {venv!r}) can not install dependencies."
            raise TypeError(msg)
        if isinstance(venv, PassthroughEnv):
            if self._runner.global_config.no_install:
                return
            msg = (
                f"Session {self.name} does not have a virtual environment, so use of"
                " session.install() is no longer allowed since it would modify the"
                " global Python environment. If you're really sure that is what you"
                ' want to do, use session.run("pip", "install", ...) instead.'
            )
            raise ValueError(msg)
        if not args:
            msg = "At least one argument required to install()."
            raise ValueError(msg)

        if self._runner.global_config.no_install and venv._reused:
            return

        if silent is None:
            silent = not self._runner.global_config.verbose

        if isinstance(venv, VirtualEnv) and venv.venv_backend == "uv":
            cmd = ["uv", "pip", "install"]
        else:
            cmd = ["python", "-m", "pip", "install"]
        self._run(
            *cmd,
            *args,
            env=env,
            include_outer_env=include_outer_env,
            external="error",
            silent=silent,
            success_codes=success_codes,
            log=log,
            stdout=stdout,
            stderr=stderr,
            interrupt_timeout=interrupt_timeout,
            terminate_timeout=terminate_timeout,
        )

    def notify(
        self,
        target: str | SessionRunner,
        posargs: Iterable[str] | None = None,
    ) -> None:
        """Place the given session at the end of the queue.

        This method is idempotent; multiple notifications to the same session
        have no effect.

        A common use case is to notify a code coverage analysis session
        from a test session::

            @nox.session
            def test(session):
                session.run("pytest")
                session.notify("coverage")

            @nox.session
            def coverage(session):
                session.run("coverage")

        Now if you run `nox -s test`, the coverage session will run afterwards.

        Args:
            target (str): The session name to notify, as used for ``nox -s``.
                Passing the decorated function object is not currently
                supported.
            posargs (Optional[Iterable[str]]): If given, sets the positional
                arguments *only* for the queued session. Otherwise, the
                standard globally available positional arguments will be
                used instead.

        Raises:
            ValueError: If called under ``--no-dependencies`` (which includes
                every session run by ``--parallel``): the notified session
                would run here *in addition to* anywhere else it is scheduled,
                racing on its virtualenv. Use ``requires=`` instead.
        """
        if self._runner.global_config.no_dependencies:
            msg = (
                "session.notify() is not supported with --no-dependencies (which"
                " --parallel uses to run each session); declare the ordering with"
                " requires= instead."
            )
            raise ValueError(msg)
        if posargs is not None:
            posargs = list(posargs)
        self._runner.manifest.notify(target, posargs)

    def log(self, *args: Any, **kwargs: Any) -> None:
        """Outputs a log during the session."""
        logger.info(*args, **kwargs)

    def warn(self, *args: Any, **kwargs: Any) -> None:
        """Outputs a warning during the session."""
        logger.warning(*args, **kwargs)

    def debug(self, *args: Any, **kwargs: Any) -> None:
        """Outputs a debug-level message during the session."""
        logger.debug(*args, **kwargs)

    def error(self, *args: Any) -> NoReturn:
        """Immediately aborts the session and optionally logs an error."""
        raise _SessionQuit(*args)

    def skip(self, *args: Any) -> NoReturn:
        """Immediately skips the session and optionally logs a warning."""
        raise _SessionSkip(*args)


class HostCommandNamespace:
    """``session.host`` facade for host-only diagnostic actions."""

    __slots__ = ("_session",)

    def __init__(self, session: Session) -> None:
        self._session = session

    def diagnose(self, *args: Any, **kwargs: Any) -> dict[str, Any]:
        return self._session.diagnose(*args, **kwargs)

    def register(self, *args: Any, **kwargs: Any) -> HostDiagnostic:
        return self._session.register_host_diagnostic(*args, **kwargs)

    def register_action(self, *args: Any, **kwargs: Any) -> HostDiagnostic:
        return self._session.register_action(*args, **kwargs)

    run = diagnose
    diagnostic = diagnose
    host_diagnostic = diagnose
    action = diagnose
    check = diagnose
    register_diagnostic = register
    register_host_diagnostic = register
    add = register
    add_diagnostic = register
    add_host_diagnostic = register


def resolve_venv_backends(global_config: NoxConfig, func: Func) -> list[str]:
    """The venv backends to try for a session: forced, session-declared, or default."""
    backends = (
        global_config.force_venv_backend
        or func.venv_backend
        or global_config.default_venv_backend
    )
    return backends.split("|")


def resolve_allow_parallel(global_config: NoxConfig, func: Func) -> bool:
    """Whether a session may run concurrently under ``--parallel``.

    A session's own ``allow_parallel=`` wins; unset sessions fall back to the
    global ``--allow-parallel`` / ``nox.options.allow_parallel`` default.
    """
    if func.allow_parallel is None:
        return bool(global_config.allow_parallel)
    return func.allow_parallel


def resolve_download_python(
    global_config: NoxConfig, func: Func
) -> Literal["auto", "never", "always"]:
    """Whether to download missing interpreters for a session."""
    return global_config.download_python or func.download_python or "auto"


def resolve_reuse_existing_venv(global_config: NoxConfig, func: Func) -> bool:
    """Determines whether to reuse an existing virtual environment.

    The decision matrix is as follows:

    +--------------------------+-----------------+-------------+
    | global_config.reuse_venv | func.reuse_venv | Reuse venv? |
    +==========================+=================+=============+
    | "always"                 | N/A             | Yes         |
    +--------------------------+-----------------+-------------+
    | "never"                  | N/A             | No          |
    +--------------------------+-----------------+-------------+
    | "yes"                    | True|None       | Yes         |
    +--------------------------+-----------------+-------------+
    | "yes"                    | False           | No          |
    +--------------------------+-----------------+-------------+
    | "no"                     | True            | Yes         |
    +--------------------------+-----------------+-------------+
    | "no"                     | False|None      | No          |
    +--------------------------+-----------------+-------------+

    Summary
    ~~~~~~~
    - "always" forces reuse regardless of `func.reuse_venv`.
    - "never" forces recreation regardless of `func.reuse_venv`.
    - "yes" and "no" respect `func.reuse_venv` being ``False`` or ``True`` respectively.
    """
    return any(
        (
            # "always" forces reuse regardless of func.reuse_venv
            global_config.reuse_venv == "always",
            # Respect func.reuse_venv when it's explicitly True, unless global_config is "never"
            func.reuse_venv is True and global_config.reuse_venv != "never",
            # Delegate to reuse ("yes") when func.reuse_venv is not explicitly False
            func.reuse_venv is not False and global_config.reuse_venv == "yes",
        )
    )


class SessionRunner:
    def __init__(
        self,
        name: str,
        signatures: Sequence[str],
        func: Func,
        global_config: NoxConfig,
        manifest: Manifest,
        *,
        multi: bool = False,
    ) -> None:
        self.name = name
        self.signatures = signatures
        self.func = func
        self.global_config = global_config
        self.manifest = manifest
        self.venv: ProcessEnv | None = None
        self.posargs: list[str] = list(global_config.posargs)
        self.result: Result | None = None
        self.multi = multi
        self.phases = PhaseReport()
        self.runtime_diagnostics: list[HostDiagnostic] = []
        self.host_diagnostics: list[HostDiagnostic] = []
        self._phase_stack: list[str] = []
        self._diagnostics_settled = False

        if getattr(func, "parametrize", None):
            self.multi = True

    def __repr__(self) -> str:
        return (
            f"<{self.__class__.__name__} {self.name}: {self.signatures!r} {self.multi}>"
        )

    @property
    def description(self) -> str | None:
        doc = self.func.__doc__
        if doc:
            return doc.strip().split("\n")[0]
        return None

    @property
    def full_description(self) -> str | None:
        doc = self.func.__doc__
        if doc:
            return inspect.cleandoc(doc)
        return None

    def __str__(self) -> str:
        sigs = ", ".join(self.signatures)
        return f"Session(name={self.name}, signatures={sigs})"

    @property
    def friendly_name(self) -> str:
        return self.signatures[0] if self.signatures else self.name

    @property
    def tags(self) -> list[str]:
        return self.func.tags

    @property
    def envdir(self) -> str:
        return _normalize_path(os.fspath(self.global_config.envdir), self.friendly_name)

    def get_direct_dependencies(
        self, sessions_by_id: Mapping[str, SessionRunner] | None = None
    ) -> Iterator[SessionRunner]:
        """Yields the sessions of the session's direct dependencies.

        Args:
            sessions_by_id (Mapping[str, ~nox.sessions.SessionRunner] | None): An
                optional mapping from both dependency signatures and names to
                corresponding ``SessionRunner``s. If this is not provided,
                ``self.manifest.all_sessions_by_signature`` will be used to find the
                sessions corresponding to signatures in ``self.func.requires``, and
                non-signature names (i.e. names of sessions that were parameterized with
                multiple Pythons) in ``self.func.requires`` will be resolved via
                ``self.manifest.parametrized_sessions_by_name``.

        Returns:
            Iterator[~nox.session.SessionRunner]

        Raises:
            KeyError: If a dependency's session could not be found.
        """
        try:
            if sessions_by_id is None:
                sessions_by_signature = self.manifest.all_sessions_by_signature
                parametrized_sessions_by_name = (
                    self.manifest.parametrized_sessions_by_name
                )
                for requirement in self.func.requires:
                    if requirement in sessions_by_signature:
                        yield sessions_by_signature[requirement]
                    else:
                        yield from parametrized_sessions_by_name[requirement]
            else:
                yield from map(sessions_by_id.__getitem__, self.func.requires)
        except KeyError as exc:
            msg = f"Session not found: {exc.args[0]}"
            raise KeyError(msg) from exc

    def _create_venv(self) -> None:
        self.venv = get_virtualenv(
            *resolve_venv_backends(self.global_config, self.func),
            download_python=resolve_download_python(self.global_config, self.func),
            envdir=self.envdir,
            reuse_existing=self.reuse_existing_venv(),
            interpreter=self.func.python,
            venv_params=self.func.venv_params,
        )

        self.venv.create()

    def reuse_existing_venv(self) -> bool:
        """
        Determines whether to reuse an existing virtual environment.

        See :func:`resolve_reuse_existing_venv` for the decision matrix.

        Returns:
            bool: True if the existing virtual environment should be reused, False otherwise.
        """
        return resolve_reuse_existing_venv(self.global_config, self.func)

    def register_host_diagnostic(
        self, target: Any = None, /, *args: Any, **options: Any
    ) -> HostDiagnostic:
        """Register a diagnostic that can run with only host inputs."""
        action = (
            target
            if isinstance(target, HostDiagnostic) and not args and not options
            else HostDiagnostic(target, *args, **options)
        )
        self.host_diagnostics.append(action)
        return action

    # Common alternative names used by integrations and tests.
    add_host_diagnostic = register_host_diagnostic
    add_diagnostic = register_host_diagnostic
    register_diagnostic = register_host_diagnostic
    register_diagnostic_action = register_host_diagnostic
    register_host_diagnostic_action = register_host_diagnostic
    register_host_action = register_host_diagnostic

    @contextlib.contextmanager
    def phase(self, name: str) -> Generator[PhaseState, None, None]:
        """Activate a named report phase for commands run in the context."""

        phase = self.phases.get(name)
        if phase is None:
            phase = PhaseState(name)
        self._phase_stack.append(name)
        phase.start()
        try:
            yield phase
        except nox.command.CommandFailed as exc:
            phase.finish(
                PHASE_FAILED,
                returncode=exc.return_code,
                reason=exc.reason or str(exc),
            )
            raise
        except _SessionQuit as exc:
            phase.finish(PHASE_ABORTED, reason=str(exc))
            raise
        except _SessionSkip as exc:
            phase.finish(PHASE_SKIPPED, reason=str(exc))
            raise
        except Exception as exc:  # noqa: BLE001
            phase.finish(PHASE_FAILED, reason=str(exc) or type(exc).__name__)
            raise
        else:
            phase.finish(PHASE_SUCCESS)
        finally:
            with contextlib.suppress(ValueError):
                self._phase_stack.remove(name)

    def resolve_command_phase(self) -> str:
        """Choose a report phase for a public ``session.run`` call."""

        if self._phase_stack:
            return self._phase_stack[-1]
        # A command in a ``finally:`` or ``except:`` block runs while the
        # original test exception is active. Classify it as ordinary cleanup
        # without requiring noxfiles to learn a new cleanup API.
        if sys.exc_info()[1] is not None and self.phases.test.started:
            return "cleanup"
        return "test"

    @contextlib.contextmanager
    def command_phase(self, name: str) -> Generator[PhaseState, None, None]:
        with self.phase(name) as phase:
            yield phase

    def _registered_diagnostic_actions(self) -> list[HostDiagnostic]:
        # ``diagnostics`` is normally the same list as ``host_diagnostics``.
        # Reading several public attribute names also tolerates noxfiles or
        # test doubles that assign a short-named list after decoration.
        raw_static: list[Any] = []
        seen_object_ids: set[int] = set()
        for attr_name in (
            "host_diagnostics",
            "diagnostics",
            "host_actions",
            "diagnostic_actions",
            "host_checks",
        ):
            value = getattr(self.func, attr_name, None)
            if value is None:
                continue
            values = value if isinstance(value, (list, tuple)) else [value]
            for raw in values:
                object_id = id(raw)
                if object_id in seen_object_ids:
                    continue
                seen_object_ids.add(object_id)
                raw_static.append(raw)

        raw_actions = [
            *raw_static,
            *self.host_diagnostics,
            *self.runtime_diagnostics,
        ]
        actions: list[HostDiagnostic] = []
        for raw in raw_actions:
            if isinstance(raw, HostDiagnostic):
                actions.append(raw)
            elif isinstance(raw, dict):
                actions.append(HostDiagnostic(**raw))
            elif callable(raw) or isinstance(raw, (str, os.PathLike, list, tuple)):
                actions.append(HostDiagnostic(raw))
        return actions

    @contextlib.contextmanager
    def _host_command_session(self) -> Generator[Session, None, None]:
        previous_venv = self.venv
        # A PassthroughEnv performs no creation and resolves programs through
        # the host PATH. It must only be used by explicitly host-registered
        # diagnostics; ordinary run_always() never enters this context.
        self.venv = PassthroughEnv()
        try:
            yield Session(self)
        finally:
            self.venv = previous_venv

    def _call_diagnostic_function(
        self, action: HostDiagnostic, host_session: Session
    ) -> Any:
        if action.func is None:
            # Support declarative action objects that expose a conventional
            # execution method instead of a constructor-provided callback.
            for method_name in ("run", "execute", "diagnose", "__call__"):
                candidate = getattr(action, method_name, None)
                if callable(candidate):
                    action.func = candidate
                    break
        assert action.func is not None

        try:
            signature = inspect.signature(action.func)
        except (TypeError, ValueError):
            return action.func(host_session)

        kwargs: dict[str, Any] = {}
        host_inputs = action.available_inputs(host_session)
        parameters = list(signature.parameters.values())
        accepts_var_keyword = any(
            p.kind is inspect.Parameter.VAR_KEYWORD for p in parameters
        )
        for parameter in parameters:
            if parameter.kind in (
                inspect.Parameter.VAR_POSITIONAL,
                inspect.Parameter.VAR_KEYWORD,
            ):
                continue
            if parameter.name in {"inputs", "host_inputs", "declared_inputs"}:
                kwargs[parameter.name] = host_inputs
            elif parameter.name in {"session", "host", "host_session"}:
                kwargs[parameter.name] = host_session
            elif parameter.name in host_inputs:
                kwargs[parameter.name] = host_inputs[parameter.name]
            elif parameter.default is inspect.Parameter.empty and parameter is parameters[0]:
                # Common one-argument Nox callable: def diagnose(session): ...
                kwargs[parameter.name] = host_session
            elif parameter.default is inspect.Parameter.empty:
                msg = f"Diagnostic input {parameter.name!r} was not declared as available"
                raise TypeError(msg)

        if accepts_var_keyword:
            kwargs.setdefault("inputs", host_inputs)
        return action.func(**kwargs)

    def run_diagnostic_now(self, action: HostDiagnostic) -> dict[str, Any]:
        """Execute one host diagnostic and append its settlement."""

        phase = self.phases.diagnostic.start()
        started = time.perf_counter()
        description = action.describe()
        status = PHASE_SUCCESS
        returncode: int | None = 0
        reason: str | None = None

        # Calls made by a diagnostic callable through its host Session must be
        # settled as diagnostics, not accidentally start the test phase.
        self._phase_stack.append("diagnostic")
        try:
            with self._host_command_session() as host_session:
                if action.command is not None:
                    options = dict(action.options)
                    options.setdefault("external", True)
                    host_session.run(*action.command, **options)
                else:
                    value = self._call_diagnostic_function(action, host_session)
                    if isinstance(value, int) and value != 0:
                        raise nox.command.CommandFailed(
                            f"Diagnostic exited with code {value}", return_code=value
                        )
                    if value is False:
                        raise nox.command.CommandFailed(
                            "Diagnostic returned false", return_code=1
                        )
        except nox.command.CommandFailed as exc:
            status = PHASE_FAILED
            returncode = exc.return_code if exc.return_code is not None else 1
            reason = exc.reason or str(exc)
        except _SessionQuit as exc:
            status = PHASE_ABORTED
            reason = str(exc)
            returncode = 1
        except _SessionSkip as exc:
            status = PHASE_SKIPPED
            reason = str(exc)
            returncode = 0
        except SystemExit as exc:
            code = exc.code
            if code is None or code == 0:
                status = PHASE_SUCCESS
                returncode = 0
            else:
                status = PHASE_FAILED
                returncode = code if isinstance(code, int) else 1
                reason = str(code)
        except KeyboardInterrupt:
            raise
        except Exception as exc:  # noqa: BLE001
            status = PHASE_FAILED
            returncode = 1
            reason = str(exc) or type(exc).__name__
        finally:
            with contextlib.suppress(ValueError):
                self._phase_stack.remove("diagnostic")

        record = {
            **description,
            "status": status,
            "result": status,
            "returncode": returncode,
            "reason": reason,
            "duration": max(0.0, time.perf_counter() - started),
        }
        phase.record_action(record)
        if status == PHASE_FAILED:
            phase.status = PHASE_FAILED
            phase.reason = reason
        elif status == PHASE_ABORTED and phase.status != PHASE_FAILED:
            phase.status = PHASE_ABORTED
            phase.reason = reason
        return record

    def _run_registered_diagnostics(self) -> PhaseState | None:
        if self._diagnostics_settled:
            return self.phases.diagnostic
        self._diagnostics_settled = True

        actions = self._registered_diagnostic_actions()
        if not actions:
            return None

        phase = self.phases.diagnostic
        phase.start()
        records = [self.run_diagnostic_now(action) for action in actions]
        statuses = [record["status"] for record in records]
        if PHASE_ABORTED in statuses and PHASE_FAILED not in statuses:
            phase.finish(PHASE_ABORTED, reason=next(
                (r.get("reason") for r in records if r["status"] == PHASE_ABORTED), None
            ))
        elif PHASE_FAILED in statuses:
            phase.finish(PHASE_FAILED, reason=next(
                (r.get("reason") for r in records if r["status"] == PHASE_FAILED), None
            ))
        elif statuses and all(status == PHASE_SKIPPED for status in statuses):
            phase.finish(PHASE_SKIPPED)
        else:
            phase.finish(PHASE_SUCCESS)
        return phase

    def _settle_result_phases(self, primary: Result, started_at: float) -> Result:
        # If the environment never became ready, ordinary cleanup commands
        # bound to that environment must not be silently downgraded to host
        # commands. Mark that phase skipped; host diagnostics have their own
        # separate phase.
        if (
            self.phases.environment.status in {PHASE_FAILED, PHASE_ABORTED, PHASE_SKIPPED}
            and not self.phases.cleanup.started
        ):
            self.phases.cleanup.skip("Environment is not ready; venv cleanup was not run")

        diagnostic = self._run_registered_diagnostics()
        primary.phases = self.phases
        primary.duration = max(0.0, time.perf_counter() - started_at)

        if diagnostic is not None and diagnostic.status in {PHASE_FAILED, PHASE_ABORTED}:
            # A diagnostic failure is real, but an existing environment/test
            # settlement remains the primary reason when there was one.
            if primary.status is Status.SUCCESS or primary.status is Status.SKIPPED:
                primary.status = (
                    Status.ABORTED if diagnostic.status == PHASE_ABORTED else Status.FAILED
                )
                if primary.reason is None:
                    primary.reason = diagnostic.reason
        return primary

    def execute(self) -> Result:
        logger.session_info(f"Running session {self.friendly_name}")

        # With --no-dependencies, prerequisites aren't queued (and may not have
        # run in this process), so skip the dependency-result check entirely.
        if not self.global_config.no_dependencies:
            for dependency in self.get_direct_dependencies():
                if not dependency.result:
                    self.result = Result.aborted_prerequisite(self, dependency)
                    return self.result

        # Execute() may be called more than once in unit/integration harnesses;
        # never duplicate this runner's phase settlements.
        self.phases = PhaseReport()
        self.runtime_diagnostics = []
        self._phase_stack = []
        self._diagnostics_settled = False

        start = time.perf_counter()
        primary: Result
        try:
            cwd = os.path.realpath(os.path.dirname(self.global_config.noxfile))

            with _chdir(cwd):
                self.phases.environment.start()
                self._create_venv()
                self.phases.environment.finish(PHASE_SUCCESS)

                session = Session(self)
                session.env["NOX_CURRENT_SESSION"] = session.name
                self.phases.test.start()
                self.func(session)
                if self.phases.test.status == PHASE_RUNNING:
                    self.phases.test.finish(PHASE_SUCCESS)

            # Nothing went wrong; return a success.
            primary = Result(
                self, Status.SUCCESS, duration=time.perf_counter() - start
            )

        except nox.virtualenv.InterpreterNotFound as exc:
            if self.global_config.error_on_missing_interpreters:
                status = Status.FAILED
                phase_status = PHASE_FAILED
            else:
                logger.warning(
                    "Missing interpreters will error by default on CI systems."
                )
                status = Status.SKIPPED
                phase_status = PHASE_SKIPPED
            self.phases.environment.finish(
                phase_status, reason=str(exc), returncode=None
            )
            primary = Result(
                self,
                status,
                reason=str(exc),
                duration=time.perf_counter() - start,
            )

        except _SessionQuit as exc:
            if self.phases.environment.status == PHASE_RUNNING:
                self.phases.environment.finish(PHASE_ABORTED, reason=str(exc))
            else:
                self.phases.test.finish(PHASE_ABORTED, reason=str(exc))
            primary = Result(
                self,
                Status.ABORTED,
                reason=str(exc),
                duration=time.perf_counter() - start,
            )

        except _SessionSkip as exc:
            if self.phases.environment.status == PHASE_RUNNING:
                self.phases.environment.finish(PHASE_SKIPPED, reason=str(exc))
            else:
                self.phases.test.finish(PHASE_SKIPPED, reason=str(exc))
            primary = Result(
                self,
                Status.SKIPPED,
                reason=str(exc),
                duration=time.perf_counter() - start,
            )

        except nox.command.CommandFailed as exc:
            failure_reason = exc.reason or str(exc)
            if self.phases.environment.status == PHASE_RUNNING:
                self.phases.environment.finish(
                    PHASE_FAILED,
                    returncode=exc.return_code,
                    reason=failure_reason,
                )
            elif self.phases.test.status == PHASE_RUNNING:
                self.phases.test.finish(
                    PHASE_FAILED,
                    returncode=exc.return_code,
                    reason=failure_reason,
                )
            primary = Result(
                self,
                Status.FAILED,
                reason=exc.reason,
                duration=time.perf_counter() - start,
            )

        except KeyboardInterrupt:
            logger.error(f"Session {self.friendly_name} interrupted.")
            raise

        except Exception as exc:  # noqa: BLE001
            logger.exception(f"Session {self.friendly_name} raised exception {exc!r}")
            if self.phases.environment.status == PHASE_RUNNING:
                self.phases.environment.finish(
                    PHASE_FAILED, reason=str(exc) or type(exc).__name__
                )
            elif self.phases.test.status == PHASE_RUNNING:
                self.phases.test.finish(
                    PHASE_FAILED, reason=str(exc) or type(exc).__name__
                )
            primary = Result(
                self,
                Status.FAILED,
                reason=str(exc) or type(exc).__name__,
                duration=time.perf_counter() - start,
            )

        self.result = self._settle_result_phases(primary, start)
        return self.result


def _duration_str(seconds: float, text: str) -> str:
    time_str = humanize.naturaldelta(dt.timedelta(seconds=seconds))

    # Might be "a moment" if short, return empty string in that case
    if time_str == "a moment":
        return ""

    return text.format(time=time_str)


class Result:
    """An object representing the result of a session."""

    def __init__(
        self,
        session: SessionRunner,
        status: Status,
        reason: str | None = None,
        *,
        duration: float = 0.0,
        phases: PhaseReport | Mapping[str, Any] | None = None,
        phase_report: PhaseReport | Mapping[str, Any] | None = None,
        test: PhaseState | Mapping[str, Any] | None = None,
        cleanup: PhaseState | Mapping[str, Any] | None = None,
        environment: PhaseState | Mapping[str, Any] | None = None,
        diagnostic: PhaseState | Mapping[str, Any] | None = None,
        diagnostics: Sequence[Mapping[str, Any]] | None = None,
        **metadata: Any,
    ) -> None:
        """Initialize the Result object.

        Args:
            session (~nox.sessions.SessionRunner):
                The session runner which ran.
            status (~nox.sessions.Status): The final result status.
            reason (str): Additional info.
            duration (float): Time taken in seconds.
        """
        self.session = session
        self.status = status
        self.reason = reason
        self.duration = duration
        report = PhaseReport.from_value(phases) or PhaseReport.from_value(phase_report)
        if report is None and any(
            value is not None
            for value in (test, cleanup, environment, diagnostic, diagnostics)
        ):
            report = PhaseReport(
                test=test,
                cleanup=cleanup,
                environment=environment,
                diagnostic=diagnostic or diagnostics,  # type: ignore[arg-type]
            )
        self.phases: PhaseReport | None = report
        self.metadata = metadata

    def __bool__(self) -> bool:
        return self.status.value > 0

    @classmethod
    def aborted_prerequisite(
        cls, session: SessionRunner, dependency: SessionRunner
    ) -> Result:
        """Return the result for a session whose prerequisite did not succeed."""
        return cls(
            session,
            Status.ABORTED,
            reason=(
                f"Prerequisite session {dependency.friendly_name} was not successful"
            ),
            duration=0,
        )

    @property
    def environment_phase(self) -> PhaseState | None:
        return self.phases.environment if self.phases else None

    @property
    def test_phase(self) -> PhaseState | None:
        return self.phases.test if self.phases else None

    @property
    def test(self) -> PhaseState | None:
        return self.test_phase

    @property
    def cleanup_phase(self) -> PhaseState | None:
        return self.phases.cleanup if self.phases else None

    @property
    def cleanup(self) -> PhaseState | None:
        return self.cleanup_phase

    @property
    def diagnostic_phase(self) -> PhaseState | None:
        return self.phases.diagnostic if self.phases else None

    @property
    def diagnostic(self) -> PhaseState | None:
        return self.diagnostic_phase

    @property
    def environment(self) -> PhaseState | None:
        return self.environment_phase

    # Convenience scalar accessors for in-process callers.
    @property
    def phase_report(self) -> PhaseReport | None:
        """Alias for :attr:`phases` for callers using the full report name."""
        return self.phases

    @property
    def settlements(self) -> dict[str, PhaseState] | None:
        """Phase settlements keyed by their short phase name."""
        return self.phases.phases if self.phases is not None else None

    @property
    def phase_states(self) -> dict[str, str] | None:
        """Plain phase-name to status mapping."""
        return self.phases.statuses() if self.phases is not None else None

    @property
    def states(self) -> dict[str, str] | None:
        """Alias for :attr:`phase_states` used in structured reports."""
        return self.phase_states

    @property
    def statuses(self) -> dict[str, str] | None:
        """Alias for :attr:`phase_states`."""
        return self.phase_states

    @property
    def diagnostic_settlement(self) -> dict[str, Any] | None:
        """The single diagnostic settlement when exactly one was recorded."""
        records = self.diagnostic_settlements
        return records[0] if len(records) == 1 else None

    @property
    def diagnostic_settlements(self) -> list[dict[str, Any]]:
        """Only this result's diagnostic action settlements."""
        return self.phases.diagnostic_records() if self.phases is not None else []

    @property
    def environment_status(self) -> str | None:
        return self.environment_phase.status if self.environment_phase else None

    @property
    def test_status(self) -> str | None:
        return self.test_phase.status if self.test_phase else None

    @property
    def cleanup_status(self) -> str | None:
        return self.cleanup_phase.status if self.cleanup_phase else None

    @property
    def diagnostic_status(self) -> str | None:
        return self.diagnostic_phase.status if self.diagnostic_phase else None

    @property
    def imperfect(self) -> str:
        """Return the English imperfect tense for the status.

        Returns:
            str: A word or phrase representing the status.
        """
        match self.status:
            case Status.SUCCESS:
                return "was successful" + _duration_str(self.duration, " in {time}")
            case _:
                status = self.status.name.lower()
                if self.reason:
                    duration_err = _duration_str(self.duration, " (took {time})")
                    return f"{status}: {self.reason}{duration_err}"
                return status

    def log(self, message: str) -> None:
        """Log a message using the appropriate log function.

        Args:
            message (str): The message to be logged.
        """
        match self.status:
            case Status.SUCCESS:
                log_function = logger.success
            case Status.SKIPPED:
                log_function = logger.warning
            case status:
                log_function = logger.error if status.value <= 0 else logger.info
        log_function(message)

    def serialize(self) -> dict[str, Any]:
        """Return a serialized representation of this result.

        Returns:
            dict: The serialized result.
        """
        data: dict[str, Any] = {
            "args": getattr(self.session.func, "call_spec", {}),
            "name": self.session.name,
            "result": self.status.name.lower(),
            "result_code": self.status.value,
            "reason": self.reason,
            "signatures": self.session.signatures,
            "duration": self.duration,
        }
        if self.phases is not None:
            data.update(self.phases.to_dict())
        # Arbitrary caller/metadata fields survive child -> parent report
        # reconstruction and the final parent --report file.
        for key, value in self.metadata.items():
            if key not in data:
                data[key] = value
        return data

    @classmethod
    def from_dict(cls, session: SessionRunner, entry: dict[str, Any]) -> Result:
        """Rebuild a result from a :meth:`serialize` entry."""
        status = Status[entry["result"].upper()]
        phase_keys = {
            "phases",
            "settlements",
            "phase_settlements",
            "steps",
            "phase_results",
            "phase_list",
            "phases_list",
            "status_report",
            "tests",
            "normal_cleanup",
            "ordinary_cleanup",
            "teardown",
            "environment_settlement",
            "test_settlement",
            "cleanup_settlement",
            "diagnostic_settlement",
            "test",
            "cleanup",
            "diagnostic",
            "host_diagnostic",
            "host_diagnostic_status",
            "host_diagnostic_state",
            "host_diagnostic_result",
            "host_diagnostic_settlement",
            "diagnostics",
            "environment",
            "env",
            "test_status",
            "test_state",
            "test_result",
            "cleanup_status",
            "cleanup_state",
            "cleanup_result",
            "diagnostic_status",
            "diagnostic_state",
            "diagnostic_result",
            "environment_status",
            "environment_state",
            "environment_result",
            "env_status",
            "env_state",
            "env_result",
        }
        phases = PhaseReport.from_dict(entry) if phase_keys.intersection(entry) else None
        known = {
            "args",
            "name",
            "result",
            "result_code",
            "reason",
            "signatures",
            "duration",
            "phases",
            "settlements",
            "phase_settlements",
            "steps",
            "phase_results",
            "environment_settlement",
            "test_settlement",
            "cleanup_settlement",
            "diagnostic_settlement",
            "environment",
            "env",
            "test",
            "cleanup",
            "diagnostic",
            "host_diagnostic",
            "host_diagnostic_status",
            "host_diagnostic_state",
            "host_diagnostic_result",
            "host_diagnostic_settlement",
            "diagnostics",
            "host_diagnostics",
            "diagnostic_actions",
            "host_diagnostic_actions",
            "actions",
            "diagnostic_results",
            "diagnostic_settlements",
            "states",
            "state",
            "statuses",
            "phase_states",
            "phase_status",
            "phase_statuses",
            "test_status",
            "cleanup_status",
            "diagnostic_status",
            "environment_status",
            "env_status",
        }
        metadata = {k: v for k, v in entry.items() if k not in known}
        return cls(
            session,
            status,
            entry.get("reason"),
            duration=entry.get("duration", 0.0),
            phases=phases,
            **metadata,
        )
