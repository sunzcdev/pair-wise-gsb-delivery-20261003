"""Virtualenv-specific Discover base class for plugin-based Python discovery."""

from __future__ import annotations

import os
from abc import ABC, abstractmethod
from typing import TYPE_CHECKING

from .probe_env import INHERIT, build_probe_env

if TYPE_CHECKING:
    from argparse import ArgumentParser
    from collections.abc import Mapping

    from python_discovery import PythonInfo

    from virtualenv.config.cli.parser import VirtualEnvOptions


class Discover(ABC):
    """Base for discovering a Python interpreter, which subclasses implement in ``run``.

    :param options: the parsed options, providing the environment to search

    :ivar probe_env: the policy for the environment handed to the interpreter probe

    """

    def __init__(self, options: VirtualEnvOptions) -> None:
        self._has_run = False
        self._interpreter: PythonInfo | None = None
        self._env: Mapping[str, str] = options.env if options.env is not None else os.environ
        # older callers build options without the flag, so the historical inherit behavior stays the default
        self.probe_env: str = getattr(options, "probe_env", INHERIT)

    @property
    def probe_environment(self) -> Mapping[str, str]:
        """The environment to hand to the interpreter probe, built from :attr:`probe_env`.

        The caller's environment is not mutated, so the creator still receives it as given.
        """
        return build_probe_env(self.probe_env, self._env)

    @property
    def interpreter(self) -> PythonInfo | None:
        """The interpreter as returned by :meth:`run`, cached."""
        if self._has_run is False:
            self._interpreter = self.run()
            self._has_run = True
        return self._interpreter

    @abstractmethod
    def run(self) -> PythonInfo | None:
        """Return the interpreter this discovery finds.

        :returns: the interpreter, ``None`` when none is found

        :raises NotImplementedError: always, subclasses implement it

        """
        raise NotImplementedError

    @classmethod
    def add_parser_arguments(cls, parser: ArgumentParser) -> None:
        """Add the command line options this discovery reads.

        :param parser: the parser to add the options to

        :raises NotImplementedError: always, subclasses implement it

        """
        raise NotImplementedError


__all__ = [
    "Discover",
]
