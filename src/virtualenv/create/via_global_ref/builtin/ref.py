"""Virtual environments in the traditional sense reference the host python.

This file allows declarative references to elements on the file system, allowing our system to automatically detect what
modes it can support given the constraints: e.g. can the file system symlink, can the files be read, executed, etc.

"""

from __future__ import annotations

__lazy_modules__ = [
    "collections",
    "virtualenv.util.path",
]

import os
import sys
from abc import ABC, abstractmethod
from collections import OrderedDict
from stat import S_IXGRP, S_IXOTH, S_IXUSR
from typing import TYPE_CHECKING

from virtualenv.info import fs_is_case_sensitive, fs_supports_symlink
from virtualenv.util.path import copy, make_exe, remove_file, symlink

if TYPE_CHECKING:
    from collections.abc import Callable
    from pathlib import Path


if sys.version_info >= (3, 11):  # pragma: no cover (py311+)
    from enum import StrEnum
else:  # pragma: no cover (py311+)
    from enum import Enum

    class StrEnum(str, Enum):
        pass


class RefMust(StrEnum):
    """Whether a reference must be linked, must be copied, or may use either.

    :var NA: the reference may be linked or copied
    :var COPY: the reference must be copied
    :var SYMLINK: the reference must be linked

    """

    NA = "NA"
    COPY = "copy"
    SYMLINK = "symlink"


class RefWhen(StrEnum):
    """The condition under which a reference applies: always, when copying, or when linking.

    :var ANY: the reference applies to both methods
    :var COPY: the reference applies only when copying
    :var SYMLINK: the reference applies only when linking

    """

    ANY = "ANY"
    COPY = "copy"
    SYMLINK = "symlink"


def applies(when: str, symlinks: bool) -> bool:
    """Whether a reference that applies under ``when`` is applied in the selected mode.

    :param when: the ``RefWhen`` value of the reference
    :param symlinks: ``True`` when the creator links instead of copying

    :returns: ``True`` when the creation applies the reference

    """
    if when == RefWhen.ANY:
        return True
    if when == RefWhen.SYMLINK:
        return symlinks is True
    if when == RefWhen.COPY:
        return symlinks is False
    return False


class PathRef(ABC):
    """Base class that checks if a file reference can be symlink/copied.

    :param src: the path to reference
    :param must: the ``RefMust`` value forcing a symlink or a copy, ``NA`` to pick either
    :param when: the ``RefWhen`` value deciding when the reference applies

    :var FS_SUPPORTS_SYMLINK: whether the file system supports symlinks
    :var FS_CASE_SENSITIVE: whether the file system is case sensitive
    :var exists: whether the referenced path exists

    """

    FS_SUPPORTS_SYMLINK = fs_supports_symlink()
    FS_CASE_SENSITIVE = fs_is_case_sensitive()

    def __init__(self, src: Path, must: str = RefMust.NA, when: str = RefWhen.ANY) -> None:
        self.must = must
        self.when = when
        self.src = src
        try:
            self.exists = src.exists()
        except OSError:
            self.exists = False
        self._can_read: bool | None = None if self.exists else False
        self._can_copy: bool | None = None if self.exists else False
        self._can_symlink: bool | None = None if self.exists else False

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(src={self.src})"

    @property
    def can_read(self) -> bool:
        """Whether the referenced file can be read."""
        if self._can_read is None:
            if self.src.is_file():
                try:
                    with self.src.open("rb"):
                        self._can_read = True
                except OSError:
                    self._can_read = False
            else:
                self._can_read = os.access(str(self.src), os.R_OK)
        return self._can_read

    @property
    def can_copy(self) -> bool:
        """Whether the referenced file can be copied."""
        if self._can_copy is None:
            if self.must == RefMust.SYMLINK:
                self._can_copy = self.can_symlink
            else:
                self._can_copy = self.can_read
        return self._can_copy

    @property
    def can_symlink(self) -> bool:
        """Whether the referenced file can be linked."""
        if self._can_symlink is None:
            if self.must == RefMust.COPY:
                self._can_symlink = self.can_copy
            else:
                self._can_symlink = self.FS_SUPPORTS_SYMLINK and self.can_read
        return self._can_symlink

    @abstractmethod
    def run(
        self,
        creator: object,  # simplint: ignore[imprecise-annotation]  # public signature
        symlinks: bool,
    ) -> None:
        """Link or copy the referenced file to its destination.

        :param creator: the creator the environment is created by
        :param symlinks: ``True`` to prefer symlinks over copies

        """
        raise NotImplementedError

    def method(self, symlinks: bool) -> Callable[..., None]:  # simplint: ignore[callable-ellipsis]  # public signature
        """Pick the function that performs the link or copy of this reference.

        :param symlinks: ``True`` to prefer symlinks over copies

        :returns: the ``symlink`` or ``copy`` function that applies

        """
        if self.method_kind(symlinks) == RefMust.SYMLINK:
            return symlink
        return copy

    def method_kind(self, symlinks: bool) -> RefMust:
        """Return which way the reference is applied, without performing it.

        :param symlinks: ``True`` to prefer symlinks over copies

        :returns: ``RefMust.SYMLINK`` when linking, ``RefMust.COPY`` when copying

        """
        if self.must == RefMust.SYMLINK:
            return RefMust.SYMLINK
        if self.must == RefMust.COPY:
            return RefMust.COPY
        return RefMust.SYMLINK if symlinks else RefMust.COPY

    def destinations(self, creator: object) -> list[Path]:
        """Return where the reference is written, without writing anything.

        A third-party reference that does not describe its destination returns an empty list, which the action view
        reports as an unknown destination rather than failing.

        :param creator: the creator the environment is created by

        :returns: the destination paths, in the order creation writes them

        """
        return []


class ExePathRef(PathRef, ABC):
    """Base class that checks if a executable can be references via symlink/copy.

    :param src: the path of the executable to reference
    :param must: the ``RefMust`` value forcing a symlink or a copy, ``NA`` to pick either
    :param when: the ``RefWhen`` value deciding when the reference applies

    """

    def __init__(self, src: Path, must: str = RefMust.NA, when: str = RefWhen.ANY) -> None:
        super().__init__(src, must, when)
        self._can_run: bool | None = None

    @property
    def can_symlink(self) -> bool:
        """Whether the executable can be linked: symlinks are supported and the file is runnable."""
        if self.FS_SUPPORTS_SYMLINK:
            return self.can_run
        return False

    @property
    def can_run(self) -> bool:
        """Whether the executable file has any execute permission bit set."""
        if self._can_run is None:
            self._can_run = bool(self.src.stat().st_mode & (S_IXUSR | S_IXGRP | S_IXOTH))
        return self._can_run


class PathRefToDest(PathRef):
    """Link a path on the file system.

    :param src: the path to link
    :param dest: callable returning the destination path, or list of paths, for the creator
    :param must: the ``RefMust`` value forcing a symlink or a copy, ``NA`` to pick either
    :param when: the ``RefWhen`` value deciding when the reference applies

    """

    def __init__(
        self,
        src: Path,
        dest: Callable[..., Path],  # simplint: ignore[callable-ellipsis]  # public signature
        must: str = RefMust.NA,
        when: str = RefWhen.ANY,
    ) -> None:
        super().__init__(src, must, when)
        self.dest = dest

    def run(
        self,
        creator: object,  # simplint: ignore[imprecise-annotation]  # public signature
        symlinks: bool,
    ) -> None:
        """Link or copy the referenced file to the destination path or each destination path in the list.

        :param creator: the creator the environment is created by
        :param symlinks: ``True`` to prefer symlinks over copies

        """
        dest = self.dest(creator, self.src)
        method = self.method(symlinks)
        dest_iterable = dest if isinstance(dest, list) else (dest,)
        if not dest.parent.exists():
            dest.parent.mkdir(parents=True, exist_ok=True)
        for dst in dest_iterable:
            method(self.src, dst)

    def destinations(self, creator: object) -> list[Path]:
        """Return the destination path or paths, without writing anything.

        :param creator: the creator the environment is created by

        :returns: the destination paths

        """
        dest = self.dest(creator, self.src)
        return dest if isinstance(dest, list) else [dest]


class ExePathRefToDest(PathRefToDest, ExePathRef):
    """Link a exe path on the file system.

    :param src: the path of the executable to link
    :param targets: the file names the executable is linked as, the first one is the base name
    :param dest: callable returning the directory the executable is linked into, for the creator
    :param must: the ``RefMust`` value forcing a symlink or a copy, ``NA`` to pick either
    :param when: the ``RefWhen`` value deciding when the reference applies

    :var base: the primary file name of the executable
    :var aliases: the remaining file names of the executable

    """

    def __init__(
        self,
        src: Path,
        targets: list[str],
        dest: Callable[..., Path],  # simplint: ignore[callable-ellipsis]  # public signature
        must: str = RefMust.NA,
        when: str = RefWhen.ANY,
    ) -> None:
        ExePathRef.__init__(self, src, must, when)
        PathRefToDest.__init__(self, src, dest, must, when)
        if not self.FS_CASE_SENSITIVE:
            targets = list(OrderedDict((i.lower(), None) for i in targets).keys())
        self.base = targets[0]
        self.aliases = targets[1:]
        self.dest = dest

    def __repr__(self) -> str:
        return f"{self.__class__.__name__}(src={self.src}, alias={self.aliases})"

    def run(
        self,
        creator: object,  # simplint: ignore[imprecise-annotation]  # public signature
        symlinks: bool,
    ) -> None:
        """Link or copy the executable, then its aliases, into the destination folder, marking copies executable.

        :param creator: the creator the environment is created by
        :param symlinks: ``True`` to prefer symlinks over copies

        """
        bin_dir = self.dest(creator, self.src).parent
        dest = bin_dir / self.base
        self.method(symlinks)(self.src, dest)
        if not symlinks:
            make_exe(dest)
        for extra in self.aliases:
            link_file = bin_dir / extra
            if link_file.is_symlink() or link_file.exists():
                remove_file(link_file)
            if symlinks:
                link_file.symlink_to(self.base)
            else:
                copy(self.src, link_file)
            if not symlinks:
                make_exe(link_file)

    def destinations(self, creator: object) -> list[Path]:
        """Return the executable's own path followed by its alias paths, without writing anything.

        :param creator: the creator the environment is created by

        :returns: the destination paths, the base name first

        """
        bin_dir = self.dest(creator, self.src).parent
        return [bin_dir / self.base, *(bin_dir / alias for alias in self.aliases)]


__all__ = [
    "ExePathRef",
    "ExePathRefToDest",
    "PathRef",
    "PathRefToDest",
    "RefMust",
    "RefWhen",
]
