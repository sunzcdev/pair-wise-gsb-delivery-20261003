""":module: watchdog.utils.dirsnapshot
:synopsis: Directory snapshots and comparison.
:author: yesudeep@google.com (Yesudeep Mangalapilly)
:author: Mickaël Schoentgen <contact@tiger-222.fr>

.. ADMONITION:: Where are the moved events? They "disappeared"

    This implementation does not take partition boundaries
    into consideration. It will only work when the directory
    tree is entirely on the same file system. More specifically,
    any part of the code that depends on inode numbers can
    break if partition boundaries are crossed. In these cases,
    the snapshot diff will represent file/directory movement as
    created and deleted events.

.. NOTE:: Hard links

    One inode may be reachable through several names. A move is only
    reported when an identity loses exactly one name and gains exactly
    one other, so the source and destination path are unambiguous.
    Adding or removing a link while other links survive is reported as
    a creation or deletion, and changes involving several names of the
    same identity are never paired arbitrarily.

Classes
-------
.. autoclass:: DirectorySnapshot
   :members:
   :show-inheritance:

.. autoclass:: DirectorySnapshotDiff
   :members:
   :show-inheritance:

.. autoclass:: EmptyDirectorySnapshot
   :members:
   :show-inheritance:

"""

from __future__ import annotations

import contextlib
import errno
import os
import sys
from stat import S_ISDIR
from typing import TYPE_CHECKING

from watchdog.utils import platform
from watchdog.utils.statx import get_statx_birthtime_ns, is_statx_available

if TYPE_CHECKING:
    from collections.abc import Callable, Iterable, Iterator
    from typing import Any


def _sort_key(path: bytes | str) -> tuple[bool, bytes | str]:
    """Ordering key that also keeps ``bytes`` and ``str`` paths comparable."""
    return (isinstance(path, bytes), path)


def _sorted_paths(paths: Iterable[bytes | str]) -> list[bytes | str]:
    """Return paths in a deterministic order.

    Diff results are exposed as lists, so derive them from sets in a stable
    order to keep repeated runs (and hard-link reports) reproducible.
    """
    return sorted(paths, key=_sort_key)


def _sorted_moves(moves: Iterable[tuple[bytes | str, bytes | str]]) -> list[tuple[bytes | str, bytes | str]]:
    """Return moves ordered by source path, then destination path."""
    return sorted(moves, key=lambda move: (_sort_key(move[0]), _sort_key(move[1])))


class DirectorySnapshotDiff:
    """Compares two directory snapshots and creates an object that represents
    the difference between the two snapshots.

    :param ref:
        The reference directory snapshot.
    :type ref:
        :class:`DirectorySnapshot`
    :param snapshot:
        The directory snapshot which will be compared
        with the reference snapshot.
    :type snapshot:
        :class:`DirectorySnapshot`
    :param ignore_device:
        A boolean indicating whether to ignore the device id or not.
        By default, a file may be uniquely identified by a combination of its first
        inode and its device id. The problem is that the device id may (or may not)
        change between system boots. This problem would cause the DirectorySnapshotDiff
        to think a file has been deleted and created again but it would be the
        exact same file.
        Set to True only if you are sure you will always use the same device.
    :type ignore_device:
        :class:`bool`
    """

    def __init__(
        self,
        ref: DirectorySnapshot,
        snapshot: DirectorySnapshot,
        *,
        ignore_device: bool = False,
    ) -> None:
        created = snapshot.paths - ref.paths
        deleted = ref.paths - snapshot.paths

        # A file's identity is its inode together with its device, unless the
        # caller explicitly ignores the device id.
        if ignore_device:

            def get_inode(directory: DirectorySnapshot, full_path: bytes | str) -> int | tuple[int, int]:
                return directory.inode(full_path)[0]

        else:

            def get_inode(directory: DirectorySnapshot, full_path: bytes | str) -> int | tuple[int, int]:
                return directory.inode(full_path)

        def is_same_entry(ref_path: bytes | str, new_path: bytes | str) -> bool:
            # A file cannot be renamed into a directory or vice versa (#303).
            if ref.isdir(ref_path) != snapshot.isdir(new_path):
                return False

            # Compare available birth times to detect recycled inodes (#992).
            ref_btime = ref.birthtime(ref_path)
            new_btime = snapshot.birthtime(new_path)
            if ref_btime is not None and new_btime is not None:
                # FreeBSD utimes(2) can lower birth time when mtime is backdated.
                if ref_path == new_path and sys.platform.startswith("freebsd") and new_btime < ref_btime:
                    return True
                return ref_btime == new_btime

            return True

        # Paths present in both snapshots may refer to different entries even
        # when the inode has been reused. Treat those as replacements, too.
        for path in ref.paths & snapshot.paths:
            if get_inode(ref, path) != get_inode(snapshot, path) or not is_same_entry(path, path):
                created.add(path)
                deleted.add(path)

        # A single inode can be reachable through several names (hard links,
        # or symlinks that ``stat`` follows), so ``path(uid)`` alone cannot
        # describe where an entry went. Group every name by file identity
        # instead: the identity is ``(inode, device)``, or the inode alone when
        # ``ignore_device`` is set. Cross-device changes therefore keep their
        # existing boundary behavior and remain create/delete pairs.
        ref_names: dict[int | tuple[int, int], set[bytes | str]] = {}
        for path in ref.paths:
            ref_names.setdefault(get_inode(ref, path), set()).add(path)

        snapshot_names: dict[int | tuple[int, int], set[bytes | str]] = {}
        for path in snapshot.paths:
            snapshot_names.setdefault(get_inode(snapshot, path), set()).add(path)

        # find moved paths
        moved: set[tuple[bytes | str, bytes | str]] = set()
        for identity, old_names in ref_names.items():
            new_names = snapshot_names.get(identity, set())
            gone = old_names - new_names
            appeared = new_names - old_names

            # Only a single vanished name paired with a single new name for the
            # same identity is an unambiguous rename. Adding or removing one of
            # several hard links leaves no pair to report, and when several
            # names change at once there is no way to tell which source became
            # which destination; such names stay separate deletions/creations
            # instead of an arbitrary move.
            if len(gone) != 1 or len(appeared) != 1:
                continue

            old_path = gone.pop()
            new_path = appeared.pop()
            if old_path in deleted and new_path in created and is_same_entry(old_path, new_path):
                # file is not deleted but moved
                deleted.discard(old_path)
                created.discard(new_path)
                moved.add((old_path, new_path))

        # find modified paths
        # first check paths that have not moved
        modified: set[bytes | str] = set()
        attrib: set[bytes | str] = set()
        for path in ref.paths & snapshot.paths:
            if get_inode(ref, path) == get_inode(snapshot, path) and is_same_entry(path, path):
                if ref.mtime(path) != snapshot.mtime(path) or ref.size(path) != snapshot.size(path):
                    modified.add(path)
                if self.is_attrib_modified(ref.stat_info(path), snapshot.stat_info(path)):
                    attrib.add(path)

        for old_path, new_path in moved:
            if ref.mtime(old_path) != snapshot.mtime(new_path) or ref.size(old_path) != snapshot.size(new_path):
                modified.add(old_path)
            if self.is_attrib_modified(ref.stat_info(old_path), snapshot.stat_info(new_path)):
                attrib.add(old_path)

        self._dirs_created = _sorted_paths(path for path in created if snapshot.isdir(path))
        self._dirs_deleted = _sorted_paths(path for path in deleted if ref.isdir(path))
        self._dirs_modified = _sorted_paths(path for path in modified if ref.isdir(path))
        self._dirs_moved = _sorted_moves((frm, to) for (frm, to) in moved if ref.isdir(frm))
        self._dirs_attrib = _sorted_paths(path for path in attrib if ref.isdir(path))

        dirs_created = set(self._dirs_created)
        dirs_deleted = set(self._dirs_deleted)
        dirs_modified = set(self._dirs_modified)
        dirs_moved = set(self._dirs_moved)
        dirs_attrib = set(self._dirs_attrib)

        self._files_created = _sorted_paths(created - dirs_created)
        self._files_deleted = _sorted_paths(deleted - dirs_deleted)
        self._files_modified = _sorted_paths(modified - dirs_modified)
        self._files_moved = _sorted_moves(moved - dirs_moved)
        self._files_attrib = _sorted_paths(attrib - dirs_attrib)

    @staticmethod
    def is_attrib_modified(ref_stat: os.stat_result, snap_stat: os.stat_result) -> bool:
        """Determines whether permissions, file attributes, or ownership have changed."""
        return (
            # File mode and permission bits (e.g. chmod, read/write/execute bits)
            getattr(ref_stat, "st_mode", None) != getattr(snap_stat, "st_mode", None)
            # Windows file attributes bitmask (e.g. attrib +h, readonly, system, archive)
            or getattr(ref_stat, "st_file_attributes", None) != getattr(snap_stat, "st_file_attributes", None)
            # Owner user ID (e.g. chown)
            or getattr(ref_stat, "st_uid", None) != getattr(snap_stat, "st_uid", None)
            # Owner group ID (e.g. chown, chgrp)
            or getattr(ref_stat, "st_gid", None) != getattr(snap_stat, "st_gid", None)
            # macOS & BSD file flags (e.g. chflags immutable, hidden, append)
            or getattr(ref_stat, "st_flags", None) != getattr(snap_stat, "st_flags", None)
        )

    def __str__(self) -> str:
        return self.__repr__()

    def __repr__(self) -> str:
        fmt = (
            "<{0} files(created={1}, deleted={2}, modified={3}, moved={4}, attrib={5}),"
            " folders(created={6}, deleted={7}, modified={8}, moved={9}, attrib={10})>"
        )
        return fmt.format(
            type(self).__name__,
            len(self._files_created),
            len(self._files_deleted),
            len(self._files_modified),
            len(self._files_moved),
            len(self._files_attrib),
            len(self._dirs_created),
            len(self._dirs_deleted),
            len(self._dirs_modified),
            len(self._dirs_moved),
            len(self._dirs_attrib),
        )

    def __len__(self) -> int:
        return sum(len(getattr(self, attr)) for attr in dir(self) if attr.startswith(("_dirs_", "_files_")))

    @property
    def files_created(self) -> list[bytes | str]:
        """List of files that were created."""
        return self._files_created

    @property
    def files_deleted(self) -> list[bytes | str]:
        """List of files that were deleted."""
        return self._files_deleted

    @property
    def files_modified(self) -> list[bytes | str]:
        """List of files that were modified."""
        return self._files_modified

    @property
    def files_moved(self) -> list[tuple[bytes | str, bytes | str]]:
        """List of files that were moved.

        Each event is a two-tuple the first item of which is the path
        that has been renamed to the second item in the tuple.
        """
        return self._files_moved

    @property
    def files_attrib(self) -> list[bytes | str]:
        """List of files whose attributes or permissions were modified."""
        return self._files_attrib

    @property
    def dirs_modified(self) -> list[bytes | str]:
        """List of directories that were modified."""
        return self._dirs_modified

    @property
    def dirs_moved(self) -> list[tuple[bytes | str, bytes | str]]:
        """List of directories that were moved.

        Each event is a two-tuple the first item of which is the path
        that has been renamed to the second item in the tuple.
        """
        return self._dirs_moved

    @property
    def dirs_deleted(self) -> list[bytes | str]:
        """List of directories that were deleted."""
        return self._dirs_deleted

    @property
    def dirs_created(self) -> list[bytes | str]:
        """List of directories that were created."""
        return self._dirs_created

    @property
    def dirs_attrib(self) -> list[bytes | str]:
        """List of directories whose attributes or permissions were modified."""
        return self._dirs_attrib

    class ContextManager:
        """Context manager that creates two directory snapshots and a
        diff object that represents the difference between the two snapshots.

        :param path:
            The directory path for which a snapshot should be taken.
        :type path:
            ``str``
        :param recursive:
            ``True`` if the entire directory tree should be included in the
            snapshot; ``False`` otherwise.
        :type recursive:
            ``bool``
        :param stat:
            Use custom stat function that returns a stat structure for path.
            Currently only st_dev, st_ino, st_mode and st_mtime are needed.

            A function taking a ``path`` as argument which will be called
            for every entry in the directory tree.
        :param listdir:
            Use custom listdir function. For details see ``os.scandir``.
        :param ignore_device:
            A boolean indicating whether to ignore the device id or not.
            By default, a file may be uniquely identified by a combination of its first
            inode and its device id. The problem is that the device id may (or may not)
            change between system boots. This problem would cause the DirectorySnapshotDiff
            to think a file has been deleted and created again but it would be the
            exact same file.
            Set to True only if you are sure you will always use the same device.
        :type ignore_device:
            :class:`bool`
        """

        def __init__(
            self,
            path: str,
            *,
            recursive: bool = True,
            stat: Callable[[str], os.stat_result] = os.stat,
            listdir: Callable[[str | None], Iterator[os.DirEntry]] = os.scandir,
            ignore_device: bool = False,
        ) -> None:
            self.path = path
            self.recursive = recursive
            self.stat = stat
            self.listdir = listdir
            self.ignore_device = ignore_device

        def __enter__(self) -> None:
            self.pre_snapshot = self.get_snapshot()

        def __exit__(self, *args: object) -> None:
            self.post_snapshot = self.get_snapshot()
            self.diff = DirectorySnapshotDiff(
                self.pre_snapshot,
                self.post_snapshot,
                ignore_device=self.ignore_device,
            )

        def get_snapshot(self) -> DirectorySnapshot:
            return DirectorySnapshot(
                path=self.path,
                recursive=self.recursive,
                stat=self.stat,
                listdir=self.listdir,
            )


class DirectorySnapshot:
    """A snapshot of stat information of files in a directory.

    :param path:
        The directory path for which a snapshot should be taken.
    :type path:
        ``str``
    :param recursive:
        ``True`` if the entire directory tree should be included in the
        snapshot; ``False`` otherwise.
    :type recursive:
        ``bool``
    :param stat:
        Use custom stat function that returns a stat structure for path.
        Currently only st_dev, st_ino, st_mode and st_mtime are needed.

        A function taking a ``path`` as argument which will be called
        for every entry in the directory tree.
    :param listdir:
        Use custom listdir function. For details see ``os.scandir``.
    """

    def __init__(
        self,
        path: str,
        *,
        recursive: bool = True,
        stat: Callable[[str], os.stat_result] = os.stat,
        listdir: Callable[[str | None], Iterator[os.DirEntry]] = os.scandir,
    ) -> None:
        self.recursive = recursive
        self.stat = stat
        self.listdir = listdir

        self._stat_info: dict[bytes | str, os.stat_result] = {}
        self._inode_to_path: dict[tuple[int, int], bytes | str] = {}
        self._birthtimes: dict[bytes | str, int | None] = {}

        st = self.stat(path)
        self._stat_info[path] = st
        self._inode_to_path[(st.st_ino, st.st_dev)] = path
        # Linux statx reads the live path, which may no longer exist at diff time.
        self.birthtime(path)

        for p, st in self.walk(path):
            i = (st.st_ino, st.st_dev)
            self._inode_to_path[i] = p
            self._stat_info[p] = st
            self.birthtime(p)

    def walk(self, root: str) -> Iterator[tuple[str, os.stat_result]]:
        try:
            paths = [os.path.join(root, entry.name) for entry in self.listdir(root)]
        except OSError as e:
            # Directory may have been deleted between finding it in the directory
            # list of its parent and trying to delete its contents. If this
            # happens we treat it as empty. Likewise if the directory was replaced
            # with a file of the same name (less likely, but possible).
            if e.errno in (errno.ENOENT, errno.ENOTDIR, errno.EINVAL):
                return
            else:
                raise

        entries = []
        for p in paths:
            with contextlib.suppress(OSError):
                entry = (p, self.stat(p))
                entries.append(entry)
                yield entry

        if self.recursive:
            for path, st in entries:
                with contextlib.suppress(PermissionError):
                    if S_ISDIR(st.st_mode):
                        yield from self.walk(path)

    @property
    def paths(self) -> set[bytes | str]:
        """Set of file/directory paths in the snapshot."""
        return set(self._stat_info.keys())

    def path(self, uid: tuple[int, int]) -> bytes | str | None:
        """Returns path for id. None if id is unknown to this snapshot."""
        return self._inode_to_path.get(uid)

    def inode(self, path: bytes | str) -> tuple[int, int]:
        """Returns an id for path."""
        st = self._stat_info[path]
        return (st.st_ino, st.st_dev)

    def isdir(self, path: bytes | str) -> bool:
        """Returns ``True`` if the path is a directory."""
        return S_ISDIR(self._stat_info[path].st_mode)

    def mtime(self, path: bytes | str) -> float:
        """Returns the last modification time for path."""
        return self._stat_info[path].st_mtime

    def birthtime(self, path: bytes | str) -> int | None:
        """Returns the birth/creation time in nanoseconds for path, or None if unsupported.

        - macOS / BSD: uses ``st.st_birthtime_ns``
        - Linux: uses ``statx(2)`` ``stx_btime``, captured when taking the snapshot
          (see https://man7.org/linux/man-pages/man2/statx.2.html)
        """
        if platform.is_windows():
            return None

        # Preserve cached None and zero without querying the live path again.
        if (birthtime := self._birthtimes.get(path, ...)) is not ...:
            return birthtime

        if st := self._stat_info.get(path):
            # macOS / BSD provides st_birthtime_ns natively
            btime_ns: int | None = getattr(st, "st_birthtime_ns", None)
            if btime_ns is not None:
                self._birthtimes[path] = btime = int(btime_ns)
                return btime
            # Fallback to float st_birthtime if nanoseconds attribute is unavailable
            btime_sec: float | None = getattr(st, "st_birthtime", None)
            if btime_sec is not None:
                self._birthtimes[path] = btime = int(btime_sec * 1_000_000_000)
                return btime

        if platform.is_linux() and is_statx_available():
            follow = self.stat == os.stat
            btime_ns = get_statx_birthtime_ns(path, follow_symlinks=follow)
            self._birthtimes[path] = btime_ns
            return btime_ns

        self._birthtimes[path] = None
        return None

    def size(self, path: bytes | str) -> int:
        """Returns the size of the file identified by path."""
        return self._stat_info[path].st_size

    def mode(self, path: bytes | str) -> int:
        """Returns the mode/permissions for path."""
        return self._stat_info[path].st_mode

    def stat_info(self, path: bytes | str) -> os.stat_result:
        """Returns a stat information object for the specified path from
        the snapshot.

        Attached information is subject to change. Do not use unless
        you specify `stat` in constructor. Use :meth:`inode`, :meth:`mtime`,
        :meth:`isdir` instead.

        :param path:
            The path for which stat information should be obtained
            from a snapshot.
        """
        return self._stat_info[path]

    def __sub__(self, previous_dirsnap: DirectorySnapshot) -> DirectorySnapshotDiff:
        """Allow subtracting a DirectorySnapshot object instance from
        another.

        :returns:
            A :class:`DirectorySnapshotDiff` object.
        """
        return DirectorySnapshotDiff(previous_dirsnap, self)

    def __str__(self) -> str:
        return self.__repr__()

    def __repr__(self) -> str:
        return str(self._stat_info)


class EmptyDirectorySnapshot(DirectorySnapshot):
    """Class to implement an empty snapshot. This is used together with
    DirectorySnapshot and DirectorySnapshotDiff in order to get all the files/folders
    in the directory as created.
    """

    def __init__(self) -> None:
        self._stat_info: dict[bytes | str, os.stat_result] = {}

    @staticmethod
    def path(_: Any) -> None:
        """Mock up method to return the path of the received inode. As the snapshot
        is intended to be empty, it always returns None.

        :returns:
            None.
        """
        return

    @property
    def paths(self) -> set:
        """Mock up method to return a set of file/directory paths in the snapshot. As
        the snapshot is intended to be empty, it always returns an empty set.

        :returns:
            An empty set.
        """
        return set()

    def birthtime(self, path: bytes | str) -> int | None:
        """Mock up method to return birth time. As the snapshot is intended to be
        empty, it always returns None.
        """
        return None
