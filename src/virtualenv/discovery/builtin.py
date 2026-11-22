"""Virtualenv-specific Builtin discovery wrapping py_discovery."""

from __future__ import annotations

__lazy_modules__ = [
    "python_discovery",
    "virtualenv.config.cli.parser",
]

import os
import sys
from typing import TYPE_CHECKING

import python_discovery

from virtualenv.config.cli.parser import ExtendAction

from .discover import Discover
from .probe_env import CLEAN, INHERIT, POLICIES, PROBE_ERRORS, SCRUB, probe_interpreter

if TYPE_CHECKING:
    from argparse import ArgumentParser
    from collections.abc import Iterable, Mapping, Sequence

    from python_discovery import PyInfoCache, PythonInfo

    from virtualenv.config.cli.parser import VirtualEnvOptions


class Builtin(Discover):
    """Discover the interpreter from the specs given on the command line, trying each spec in turn.

    :param options: the parsed options, providing the cache and the environment

    :var python_spec: the interpreter specs to try, in order
    :var app_data: the cache of interpreter information
    :var try_first_with: the interpreter paths to check before the rest

    """

    python_spec: Sequence[str]
    app_data: PyInfoCache
    try_first_with: Sequence[str]

    def __init__(self, options: VirtualEnvOptions) -> None:
        super().__init__(options)
        self.python_spec = options.python or [sys.executable]
        self.app_data = options.app_data
        self.try_first_with = options.try_first_with

    def __repr__(self) -> str:
        spec = self.python_spec[0] if len(self.python_spec) == 1 else self.python_spec
        return f"{self.__class__.__name__} discover of python_spec={spec!r}"

    @classmethod
    def add_parser_arguments(cls, parser: ArgumentParser) -> None:
        """Add the options that select the interpreter and the specs to try.

        :param parser: the parser to add the options to

        """
        parser.add_argument(
            "-p",
            "--python",
            dest="python",
            metavar="py",
            type=str,
            # ExtendAction extends the list with the values it gets, so a lone string would split into characters
            nargs=1,
            action=ExtendAction,
            default=[],
            help="interpreter based on what to create environment (path/identifier/version-specifier) "
            "- by default use the interpreter where the tool is installed - first found wins. "
            "Version specifiers (e.g., >=3.12, ~=3.11.0, ==3.10) are also supported",
        )
        parser.add_argument(
            "--try-first-with",
            dest="try_first_with",
            metavar="py_exe",
            type=str,
            nargs=1,
            action=ExtendAction,
            default=[],
            help="try first these interpreters before starting the discovery",
        )
        parser.add_argument(
            "--probe-env",
            dest="probe_env",
            metavar="policy",
            type=str,
            choices=POLICIES,
            default=INHERIT,
            help="which parent environment variables the interpreter probe inherits: "
            f"{INHERIT} passes them all, {SCRUB} drops the ones that steer a Python at start-up, "
            f"{CLEAN} passes only the ones a process needs to start",
        )

    def run(self) -> PythonInfo | None:
        """Return the first interpreter that one of the specs matches.

        A spec that names an existing interpreter but cannot be probed is re-probed under every narrower policy, so a
        failure the caller's environment caused is told apart from an interpreter that cannot report itself.

        :returns: the interpreter, ``None`` when no spec matches

        :raises ProbeEnvironmentError: when an explicitly named interpreter cannot be probed

        """
        probe_env = self.probe_environment
        for python_spec in self.python_spec:
            try:
                result = get_interpreter(
                    python_spec,
                    self.try_first_with,
                    app_data=self.app_data,
                    env=self._env,
                    probe_env=probe_env,
                )
            except PROBE_ERRORS:
                if _names_existing_interpreter(python_spec):
                    # raises with the classification, which tells a disturbed probe from a broken interpreter
                    probe_interpreter(python_spec, policy=self.probe_env, env=self._env)
                raise
            if result:
                return result
        return None


def _names_existing_interpreter(spec: str) -> bool:
    # only an absolute path makes the probe's target known without searching first
    return os.path.isabs(spec) and os.path.exists(spec)


def get_interpreter(
    key: str,
    try_first_with: Iterable[str],
    cache: PyInfoCache | None = None,
    env: Mapping[str, str] | None = None,
    app_data: PyInfoCache | None = None,
    probe_env: Mapping[str, str] | None = None,
) -> PythonInfo | None:
    """Find the interpreter that matches a spec, looking at the cache first.

    :param key: the interpreter spec, a path or a name, kept exactly as given
    :param try_first_with: the interpreter paths to check before the rest
    :param cache: the cache of interpreter information, ``None`` for the application data cache
    :param env: the environment variables, ``None`` for the process environment
    :param app_data: the application data cache, used when no cache is given
    :param probe_env: the environment handed to the interpreter probe, ``None`` to use ``env``

    :returns: the interpreter, ``None`` when none matches

    """
    return python_discovery.get_interpreter(
        key, try_first_with, cache or app_data, env if probe_env is None else probe_env
    )


__all__ = [
    "Builtin",
    "get_interpreter",
]
