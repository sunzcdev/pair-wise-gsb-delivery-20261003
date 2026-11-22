"""A read-only view of the file actions a creator would perform.

The view is built from the same references the creator applies when it generates an environment, so it cannot drift from
the real layout: the source list, the copy-or-link choice and the destination of each file all come from that code.

"""

from __future__ import annotations

__lazy_modules__ = []

import sys
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from pathlib import Path


if sys.version_info >= (3, 11):  # pragma: no cover (py311+)
    from enum import StrEnum
else:  # pragma: no cover (py311+)
    from enum import Enum

    class StrEnum(str, Enum):
        pass


class ActionMethod(StrEnum):
    """The way a creator applies a reference.

    :var COPY: the creator copies the source file
    :var SYMLINK: the creator links the destination to the source
    :var UNKNOWN: the creator does not say which way it applies the reference

    """

    COPY = "copy"
    SYMLINK = "symlink"
    UNKNOWN = "unknown"


class ActionKind(StrEnum):
    """What a reference points at.

    :var EXECUTABLE: an interpreter executable, which also gets its alias names
    :var FILE: a regular file, such as a DLL or a zip
    :var DIRECTORY: a folder tree
    :var UNKNOWN: the creator does not say what it references

    """

    EXECUTABLE = "executable"
    FILE = "file"
    DIRECTORY = "directory"
    UNKNOWN = "unknown"


class Fallback:
    """The method that applies instead when the chosen one is not permitted on this host.

    :param method: the method that would apply instead, ``None`` when neither method works
    :param reason: why the chosen method is not available
    :param available: ``True`` when the alternative method can be applied

    """

    # the three fields form one record, so they stay positional
    def __init__(  # ruff:ignore[boolean-type-hint-positional-argument]
        self, method: str | None, reason: str, available: bool
    ) -> None:
        self.method = method
        self.reason = reason
        self.available = available

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(method={self.method!r}, available={self.available!r})"


class Action:
    """One reference the creator would read from the host and write into the environment.

    :param src: the host path the action reads, ``None`` when the creator does not say
    :param dest: the path the action writes inside the environment, ``None`` when unknown
    :param kind: an ``ActionKind`` value describing what the source is
    :param method: an ``ActionMethod`` value saying whether the creator copies or links
    :param reason: why the creator picked that method
    :param aliases: the extra paths the creator writes for the same source, such as executable alias names
    :param exists: whether the source exists, ``None`` when unknown
    :param fallback: what applies instead when the chosen method is not permitted on this host

    """

    def __init__(  # ruff:ignore[too-many-arguments]
        self,
        src: Path | None = None,
        dest: Path | None = None,
        kind: str = ActionKind.UNKNOWN,
        method: str = ActionMethod.UNKNOWN,
        reason: str = "",
        aliases: list[Path] | None = None,
        exists: bool | None = None,
        fallback: Fallback | None = None,
    ) -> None:
        self.src = src
        self.dest = dest
        self.kind = kind
        self.method = method
        self.reason = reason
        self.aliases = [] if aliases is None else aliases
        self.exists = exists
        self.fallback = fallback

    @property
    def destinations(self) -> list[Path]:
        """Every path the action writes, the primary destination first.

        :returns: the paths the creator would write

        """
        return [] if self.dest is None else [self.dest, *self.aliases]

    def to_dict(self) -> dict[str, object]:
        """Encode the action for triage output.

        :returns: a JSON-friendly mapping of the action

        """
        return {
            "src": None if self.src is None else str(self.src),
            "destinations": [str(i) for i in self.destinations],
            "kind": self.kind,
            "method": self.method,
            "reason": self.reason,
            "exists": self.exists,
            "fallback": None
            if self.fallback is None
            else {
                "method": self.fallback.method,
                "reason": self.fallback.reason,
                "available": self.fallback.available,
            },
        }

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(src={self.src}, dest={self.dest}, method={self.method!r})"


class ActionPlan:
    """The read-only view of what a creator would do, before an environment is generated.

    :param creator: the name of the creator the plan was built for
    :param symlinks: whether the creator links instead of copying, ``None`` when that does not apply
    :param mode_reason: why the creator picked linking or copying
    :param actions: the actions the creator would perform, in the order it applies them

    """

    def __init__(
        self,
        creator: str,
        symlinks: bool | None = None,
        mode_reason: str = "",
        actions: list[Action] | None = None,
    ) -> None:
        self.creator = creator
        self.symlinks = symlinks
        self.mode_reason = mode_reason
        self.actions = [] if actions is None else actions

    def to_dict(self) -> dict[str, object]:
        """Encode the plan for triage output.

        :returns: a JSON-friendly mapping of the plan

        """
        return {
            "creator": self.creator,
            "symlinks": self.symlinks,
            "mode_reason": self.mode_reason,
            "actions": [i.to_dict() for i in self.actions],
        }

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(creator={self.creator!r}, actions={len(self.actions)})"


__all__ = [
    "Action",
    "ActionKind",
    "ActionMethod",
    "ActionPlan",
    "Fallback",
]
