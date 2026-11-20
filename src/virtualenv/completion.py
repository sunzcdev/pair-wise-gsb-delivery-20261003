"""Complete creation mode, which makes a creation observable as one atomic operation.

Two concurrent runners asking for the same destination could previously interleave: one saw the Python interpreter
already copied and started using the environment while the other was still seeding ``pip`` into it. The complete mode
guards the whole creation behind a cross-process lock and records the phases that already finished in a state file
inside the environment. A second request then either reuses the finished environment, or resumes the creation at the
phase the previous attempt failed in.
"""

from __future__ import annotations

__lazy_modules__ = [
    "json",
    "os",
    "virtualenv.create.pyenv_cfg",
    "virtualenv.util.lock",
    "virtualenv.util.path",
]

import json
import logging
import os
from contextlib import contextmanager
from pathlib import Path
from typing import TYPE_CHECKING, Final

from virtualenv.create.pyenv_cfg import PyEnvCfg
from virtualenv.util.lock import ReentrantFileLock
from virtualenv.util.path import ensure_dir, safe_delete, write_file

if TYPE_CHECKING:
    from collections.abc import Generator, Sequence

    from virtualenv.create.creator import Creator
    from virtualenv.seed.seeder import Seeder

LOGGER: Final[logging.Logger] = logging.getLogger(__name__)  # simplint: ignore[module-variable-prefix]  # existing name

#: Name of the folder, inside the environment, that holds the complete-mode state.
STATE_DIR: Final[str] = ".virtualenv-complete"
#: Name of the JSON file recording the phases that completed.
STATE_FILE: Final[str] = "state.json"
#: Name of the lock taken around the whole creation, beside the environment so it survives a recreated destination.
LOCK_NAME: Final[str] = ".virtualenv-complete.lock"

#: The creator copied the interpreter and wrote the environment metadata.
PHASE_CREATED: Final[str] = "created"
#: The seeder installed the seed packages (only recorded when seeding is enabled).
PHASE_SEEDED: Final[str] = "seeded"
#: The activation scripts were generated.
PHASE_ACTIVATED: Final[str] = "activated"
#: The ``pyvenv.cfg`` was written; reaching this phase means the environment is complete.
PHASE_CONFIGURED: Final[str] = "configured"

#: Phases in the order the session performs them.
PHASES: Final[tuple[str, ...]] = (PHASE_CREATED, PHASE_SEEDED, PHASE_ACTIVATED, PHASE_CONFIGURED)

#: The complete-mode run reuses an environment that already reached its final phase.
ACTION_REUSE: Final[str] = "reuse"
#: The complete-mode run continues an environment a previous attempt left unfinished.
ACTION_RESUME: Final[str] = "resume"
#: The complete-mode run builds the environment from scratch.
ACTION_CREATE: Final[str] = "create"


class ForeignEnvironmentError(RuntimeError):
    """The destination exists but is neither a complete-mode environment nor one virtualenv can recognize.

    :param dest: the destination directory that blocks the creation

    """

    def __init__(self, dest: Path) -> None:
        self.dest = dest
        super().__init__(
            f"the destination {dest} already exists and is not a virtual environment; pass --clear to replace it"
        )


class CompletionState:
    """Phases a complete-mode creation already finished for a destination.

    :param dest: the destination directory of the environment
    :param phases: the phases recorded as completed, in creation order

    :var dest: the destination directory of the environment
    :var phases: the phases recorded as completed

    """

    def __init__(self, dest: Path, phases: Sequence[str]) -> None:
        self.dest = Path(dest)
        self.phases: tuple[str, ...] = tuple(phases)

    def __contains__(self, phase: object) -> bool:
        return phase in self.phases

    @property
    def last_phase(self) -> str | None:
        """The last phase recorded as completed, or ``None`` when no phase completed."""
        return self.phases[-1] if self.phases else None

    @property
    def complete(self) -> bool:
        """Whether the creation reached its final phase."""
        return PHASE_CONFIGURED in self.phases


class CompletionStore:
    """State of a complete-mode creation for one destination.

    :param dest: the directory the environment is created in

    :var dest: the destination directory of the environment
    :var state_path: the JSON state file inside the destination
    :var lock_path: the lock file beside the destination

    """

    def __init__(self, dest: Path) -> None:
        self.dest = Path(dest)
        self.state_path = self.dest / STATE_DIR / STATE_FILE
        self.lock_path = self.dest.parent / LOCK_NAME

    def __repr__(self) -> str:
        return f"{type(self).__name__}(dest={self.dest})"

    @contextmanager
    def locked(self) -> Generator[None, None, None]:
        """Hold the cross-process lock guarding the destination for the whole creation.

        :yields: nothing

        """
        folder_lock = ReentrantFileLock(self.lock_path.parent)
        with folder_lock.lock_for_key(self.lock_path.name.removesuffix(".lock")):
            yield

    def load(self) -> CompletionState:
        """Read the recorded state, returning an empty state when no state file exists yet.

        :returns: the state of the previous attempts

        """
        if not self.state_path.exists():
            return CompletionState(self.dest, ())
        try:
            data = json.loads(self.state_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exception:
            # an unreadable or truncated state file means an attempt died while writing it, so start the phases over
            LOGGER.warning("ignoring unreadable complete-mode state %s: %s", self.state_path, exception)
            return CompletionState(self.dest, ())
        phases = tuple(phase for phase in data.get("phases", ()) if phase in PHASES)
        return CompletionState(self.dest, phases)

    def record(self, phase: str) -> None:
        """Mark a phase as completed, writing the state atomically so a crash never leaves a half-written file.

        :param phase: the phase that just finished

        """
        state = self.load()
        phases = [done for done in PHASES if done in state.phases or done == phase]
        ensure_dir(self.state_path.parent)
        payload = json.dumps({"version": 1, "dest": os.fspath(self.dest), "phases": phases}, indent=2)
        # replacing through a temp file keeps the swap atomic, including on Windows where open files cannot be replaced
        temporary = self.state_path.with_name(f"{self.state_path.name}.tmp")
        write_file(temporary, f"{payload}\n".encode("utf-8"))
        os.replace(temporary, self.state_path)

    def clear(self) -> None:
        """Drop the destination together with its recorded state, for a from-scratch creation."""
        if self.dest.exists():
            safe_delete(self.dest)


def looks_like_virtualenv(dest: Path) -> bool:
    """Whether the destination holds a virtualenv-created environment.

    A ``pyvenv.cfg`` carrying the ``virtualenv`` key identifies environments virtualenv created, including ones made
    before complete mode existed, so they can be resumed instead of being treated as foreign folders.

    :param dest: the directory to inspect

    :returns: ``True`` when the destination is a recognizable virtual environment

    """
    if not dest.is_dir():
        return False
    return "virtualenv" in PyEnvCfg.from_folder(dest).content


def creator_finished(creator: Creator) -> bool:
    """Whether the interpreter the creator installs is already present.

    :param creator: the creator that would build the environment

    :returns: ``True`` when the environment's executable exists

    """
    exe = creator.exe
    return exe is not None and Path(exe).exists()


def seeder_finished(seeder: Seeder | None, creator: Creator) -> bool:
    """Whether a previously seeded environment already provides the seed packages.

    A disabled seeder has nothing to install, so it is considered done. An enabled seeder only succeeds once the
    environment's purelib exists and holds the packages it installed.

    :param seeder: the seeder of the current session, or ``None`` when seeding is disabled
    :param creator: the creator of the environment

    :returns: ``True`` when no seeding is required or the seeded package folder is present

    """
    if seeder is None or not seeder.enabled:
        return True
    purelib = Path(creator.purelib)
    return purelib.exists() and any(purelib.iterdir())


def decide(
    dest: Path,
    state: CompletionState,
    clear: bool,
    creator: Creator,
    seeder: Seeder | None = None,
) -> str:
    """Decide what a complete-mode run should do with its destination.

    :param dest: the requested destination
    :param state: the state recorded for the destination
    :param clear: whether the caller asked to replace an existing destination
    :param creator: the creator of the current session
    :param seeder: the seeder of the current session, or ``None`` when seeding is disabled

    :returns: :data:`ACTION_REUSE` for a finished environment, :data:`ACTION_RESUME` for an interrupted one and
        :data:`ACTION_CREATE` for a fresh or explicitly cleared one

    :raises ForeignEnvironmentError: when the destination is not a virtual environment and no clear was requested

    """
    if not dest.exists() or clear:
        return ACTION_CREATE
    # a finished environment is only reusable when this run's seeding request is already satisfied: an environment built
    # without seed packages must resume into seeding once a run asks for them, instead of being reused as-is
    seeding_satisfied = seeder is None or not seeder.enabled or seeder_finished(seeder, creator)
    if state.complete and looks_like_virtualenv(dest) and creator_finished(creator) and seeding_satisfied:
        return ACTION_REUSE
    if state.phases or looks_like_virtualenv(dest):
        # a recorded phase proves the folder belongs to us; a recognizable pre-complete-mode env is resumed as well
        return ACTION_RESUME
    raise ForeignEnvironmentError(dest)


__all__ = [
    "ACTION_CREATE",
    "ACTION_RESUME",
    "ACTION_REUSE",
    "PHASE_ACTIVATED",
    "PHASE_CONFIGURED",
    "PHASE_CREATED",
    "PHASE_SEEDED",
    "PHASES",
    "CompletionState",
    "CompletionStore",
    "ForeignEnvironmentError",
    "creator_finished",
    "decide",
    "looks_like_virtualenv",
    "seeder_finished",
]
