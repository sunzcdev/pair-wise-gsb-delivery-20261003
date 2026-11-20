from __future__ import annotations

__lazy_modules__ = [
    "json",
    "typing_extensions",
    "virtualenv.activation.batch",
    "virtualenv.completion",
    "virtualenv.config.cli.parser",
]

import json
import logging
import sys
from typing import TYPE_CHECKING, Final

from virtualenv.activation.batch import BatchActivator
from virtualenv.completion import (
    ACTION_CREATE,
    ACTION_REUSE,
    ACTION_RESUME,
    PHASE_ACTIVATED,
    PHASE_CONFIGURED,
    PHASE_CREATED,
    PHASE_SEEDED,
    CompletionStore,
    creator_finished,
    decide,
    seeder_finished,
)
from virtualenv.config.cli.parser import VirtualEnvOptions

if TYPE_CHECKING:
    from types import TracebackType

    from python_discovery import PythonInfo

    from virtualenv.activation.activator import Activator
    from virtualenv.app_data.base import AppData
    from virtualenv.create.creator import Creator
    from virtualenv.seed.seeder import Seeder

if sys.version_info >= (3, 11):
    from typing import Self
else:
    from typing_extensions import Self

LOGGER: Final[logging.Logger] = logging.getLogger(__name__)  # simplint: ignore[module-variable-prefix]  # existing name


class Session:
    """Represents a virtual environment creation session.

    :param verbosity: the verbosity level the creation runs with
    :param app_data: the application data folder
    :param interpreter: the host interpreter the environment is based on
    :param creator: the creator that builds the environment
    :param seeder: the seeder that installs the seed packages
    :param activators: the activators that generate the activation scripts
    :param complete: ``True`` to run the creation in complete mode, serializing concurrent runs for the destination and
        recording the completion of each phase so an interrupted creation can resume

    """

    # the session keeps each component of a creation as its own argument
    def __init__(  # ruff:ignore[too-many-arguments]
        self,
        verbosity: int,
        app_data: AppData,
        interpreter: PythonInfo,
        creator: Creator,
        seeder: Seeder,
        activators: list[Activator],
        complete: bool = False,
    ) -> None:
        self._verbosity = verbosity
        self._app_data = app_data
        self._interpreter = interpreter
        self._creator = creator
        self._seeder = seeder
        self._activators = activators
        self._complete = complete
        self._completion: CompletionStore | None = CompletionStore(creator.dest) if complete else None

    def __enter__(self) -> Self:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: TracebackType | None,
    ) -> None:
        self._app_data.close()

    @property
    def verbosity(self) -> int:
        """The verbosity of the run."""
        return self._verbosity

    @property
    def complete(self) -> bool:
        """Whether the session runs in complete mode, with a lock and per-phase completion state."""
        return self._complete

    def run(self) -> None:
        """Create the environment, seed it, activate it and write its configuration file.

        In complete mode the whole creation is guarded by a cross-process lock, and each finished phase is recorded so
        a concurrent or retried run reuses a finished environment or resumes an interrupted one instead of observing
        a half-built destination.

        """
        if not self._complete:
            self._create()
            self._seed()
            self._activate()
            self.creator.pyenv_cfg.write()
            return
        # the store is set whenever complete mode is enabled
        with self._completion.locked():  # ty: ignore[union-attr]
            self._run_complete()

    def _run_complete(self) -> None:
        store: Final[CompletionStore] = self._completion  # ty: ignore[assignment]
        state = store.load()
        action = decide(self.creator.dest, state, self.creator.clear, self.creator, self.seeder)
        if action == ACTION_REUSE:
            LOGGER.info("reuse complete virtual environment at %s", self.creator.dest)
            return
        if action == ACTION_CREATE:
            LOGGER.info("create virtual environment via %s (complete mode)", self.creator)
            store.clear()
            state = store.load()
        else:
            LOGGER.info(
                "resume virtual environment at %s after the %s phase (complete mode)",
                self.creator.dest,
                state.last_phase or "missing",
            )

        if PHASE_CREATED not in state or not creator_finished(self.creator):
            self._create()
            store.record(PHASE_CREATED)
        else:
            LOGGER.debug("skip creator, the environment already provides %s", self.creator.exe)

        if self.seeder is not None and self.seeder.enabled:
            if PHASE_SEEDED not in state or not seeder_finished(self.seeder, self.creator):
                self._seed()
                store.record(PHASE_SEEDED)
            else:
                LOGGER.debug("skip seeder, the seed packages are already installed")
        # a disabled seeder records no seeded phase, so a later run that enables seeding seeds instead of skipping it

        if PHASE_ACTIVATED not in state:
            self._activate()
            store.record(PHASE_ACTIVATED)
        else:
            LOGGER.debug("skip activators, the activation scripts already exist")

        self.creator.pyenv_cfg.write()
        store.record(PHASE_CONFIGURED)
        LOGGER.info("completed virtual environment at %s (complete mode)", self.creator.dest)

    def _create(self) -> None:
        LOGGER.info("create virtual environment via %s", self.creator)
        self.creator.run()
        LOGGER.debug(_DEBUG_MARKER)
        LOGGER.debug("%s", _Debug(self.creator))

    def _seed(self) -> None:
        if self.seeder is not None and self.seeder.enabled:
            LOGGER.info("add seed packages via %s", self.seeder)
            self.seeder.run(self.creator)

    @property
    def seeder(self) -> Seeder:
        """The mechanism used to provide the seed packages (pip, setuptools, wheel)."""
        return self._seeder

    def _activate(self) -> None:
        if BatchActivator.supports(self.interpreter) and not any(
            isinstance(i, BatchActivator) for i in self.activators
        ):
            BatchActivator(VirtualEnvOptions(prompt=None)).remove_unsafe(self.creator, selected=False)
        if self.activators:
            active = ", ".join(type(i).__name__.replace("Activator", "") for i in self.activators)
            LOGGER.info("add activators for %s", active)
            self._activators = [activator for activator in self.activators if activator.generate(self.creator)]

    @property
    def interpreter(self) -> PythonInfo:
        """Create a virtual environment based on this reference interpreter."""
        return self._interpreter

    @property
    def activators(self) -> list[Activator]:
        """Activators used to generate activations scripts."""
        return self._activators

    @property
    def creator(self) -> Creator:
        """The creator used to build the virtual environment (must be compatible with the interpreter)."""
        return self._creator


class _Debug:
    def __init__(self, creator: Creator) -> None:
        self.creator = creator

    def __repr__(self) -> str:
        return json.dumps(self.creator.debug, indent=2)


_DEBUG_MARKER: Final[str] = f"{'=' * 30} target debug {'=' * 30}"


__all__ = [
    "Session",
]
