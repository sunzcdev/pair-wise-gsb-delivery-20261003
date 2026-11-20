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

.. autoclass:: PartialDirectorySnapshot
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
    from collections.abc import Callable, Iterator
    from typing import Any


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

        if ignore_device:

            def get_inode(directory: DirectorySnapshot, full_path: bytes | str) -> int | tuple[int, int]:
                return directory.inode(full_path)[0]

        else:

            def get_inode(directory: DirectorySnapshot, full_path: bytes | str) -> int | tuple[int, int]:
                return directory.inode(full_path)

        if ignore_device:
            # Retain each snapshot's canonical path when hard links share an inode.
            ref_paths = {ref.inode(path)[0]: ref.path(ref.inode(path)) for path in ref.paths}
            snapshot_paths = {snapshot.inode(path)[0]: snapshot.path(snapshot.inode(path)) for path in snapshot.paths}

            def get_ref_path(uid: tuple[int, int]) -> bytes | str | None:
                return ref.path(uid) or ref_paths.get(uid[0])

            def get_snapshot_path(uid: tuple[int, int]) -> bytes | str | None:
                return snapshot.path(uid) or snapshot_paths.get(uid[0])

        else:
            get_ref_path = ref.path
            get_snapshot_path = snapshot.path

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

        # find moved paths
        moved: set[tuple[bytes | str, bytes | str]] = set()
        for path in set(deleted):
            inode = ref.inode(path)
            new_path = get_snapshot_path(inode)
            # Only treat this as a move if the inode is now reachable through a
            # genuinely new path. A symlink (or hard link) shares its target's
            # inode, so ``new_path`` may still point at the unchanged target,
            # which is not a move.
            if (
                new_path
                and (new_path not in ref.paths or ref.inode(new_path) != inode)
                and is_same_entry(path, new_path)
            ):
                # file is not deleted but moved
                deleted.remove(path)
                moved.add((path, new_path))

        for path in set(created):
            inode = snapshot.inode(path)
            old_path = get_ref_path(inode)
            # Only treat this as a move if the inode's previous path is really
            # gone. A symlink (or hard link) shares its target's inode, so
            # ``old_path`` may still exist with that inode, which means this is a
            # creation, not a move.
            if (
                old_path
                and (old_path not in snapshot.paths or snapshot.inode(old_path) != inode)
                and is_same_entry(old_path, path)
            ):
                created.remove(path)
                moved.add((old_path, path))

        # A hard link can make the two inode lookups select different paths.
        # A move found in either pass must not also remain a create or delete.
        for old_path, new_path in moved:
            deleted.discard(old_path)
            created.discard(new_path)

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

        self._dirs_created = [path for path in created if snapshot.isdir(path)]
        self._dirs_deleted = [path for path in deleted if ref.isdir(path)]
        self._dirs_modified = [path for path in modified if ref.isdir(path)]
        self._dirs_moved = [(frm, to) for (frm, to) in moved if ref.isdir(frm)]
        self._dirs_attrib = [path for path in attrib if ref.isdir(path)]

        self._files_created = list(created - set(self._dirs_created))
        self._files_deleted = list(deleted - set(self._dirs_deleted))
        self._files_modified = list(modified - set(self._dirs_modified))
        self._files_moved = list(moved - set(self._dirs_moved))
        self._files_attrib = list(attrib - set(self._dirs_attrib))

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
    :param previous:
        The snapshot taken by the previous poll. Ignored by this class; only
        subclasses that keep the last known state of the paths they cannot
        read, such as :class:`PartialDirectorySnapshot`, use it.

    Note
    ----
    A subdirectory that cannot be listed because of a :class:`PermissionError`
    is simply not walked (see `#408`_). Use :class:`PartialDirectorySnapshot`
    to keep the last known state of such directories instead.

    .. _#408: https://github.com/gorakhargosh/watchdog/pull/408
    """

    def __init__(
        self,
        path: str,
        *,
        recursive: bool = True,
        stat: Callable[[str], os.stat_result] = os.stat,
        listdir: Callable[[str | None], Iterator[os.DirEntry]] = os.scandir,
        previous: DirectorySnapshot | None = None,
    ) -> None:
        self.recursive = recursive
        self.stat = stat
        self.listdir = listdir

        self._stat_info: dict[bytes | str, os.stat_result] = {}
        self._inode_to_path: dict[tuple[int, int], bytes | str] = {}
        self._birthtimes: dict[bytes | str, int | None] = {}
        # Paths whose contents could not be read while taking this snapshot.
        self._unreadable: dict[bytes | str, OSError] = {}

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
            self._on_listdir_error(root, e)
            return

        entries = []
        for p in paths:
            try:
                entry = (p, self.stat(p))
            except OSError as e:
                self._on_stat_error(p, e)
            else:
                entries.append(entry)
                yield entry

        if self.recursive:
            for path, st in entries:
                with contextlib.suppress(PermissionError):
                    if S_ISDIR(st.st_mode):
                        yield from self.walk(path)

    @staticmethod
    def _is_missing(error: OSError) -> bool:
        """Whether ``error`` means the path is simply gone rather than unreadable."""
        return error.errno in (errno.ENOENT, errno.ENOTDIR, errno.EINVAL)

    def _on_listdir_error(self, root: str, error: OSError) -> None:
        """Handle a failure to list the contents of the directory ``root``.

        The directory may have been deleted between being found in its
        parent's listing and being read. Likewise, it may have been replaced
        with a file of the same name (less likely, but possible). In both
        cases it is treated as empty. Any other error is raised.

        :class:`PartialDirectorySnapshot` overrides this to remember the
        directory instead of silently dropping its entries.

        :raises OSError: For errors other than ``ENOENT``, ``ENOTDIR`` and
            ``EINVAL``.
        """
        if not self._is_missing(error):
            raise error

    def _on_stat_error(self, path: bytes | str, error: OSError) -> None:
        """Handle a failure to stat the entry ``path``.

        The entry is skipped and therefore left out of the snapshot.
        :class:`PartialDirectorySnapshot` overrides this to remember the entry
        instead of dropping it.
        """

    @property
    def unreadable(self) -> dict[bytes | str, OSError]:
        """Paths whose contents could not be read while taking the snapshot.

        Maps each such path (a directory that could not be listed, or an entry
        that could not be stat'ed) to the :class:`OSError` that prevented
        reading it. Always empty for a plain :class:`DirectorySnapshot`, which
        instead silently drops the entries it cannot read.
        """
        return dict(self._unreadable)

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
        self._unreadable: dict[bytes | str, OSError] = {}

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

    @property
    def unreadable(self) -> dict[bytes | str, OSError]:
        """Mock up method to return the unreadable paths. As the snapshot is
        intended to be empty, it always returns an empty dict.
        """
        return {}


class PartialDirectorySnapshot(DirectorySnapshot):
    """A snapshot that can carry an incomplete view of the directory tree.

    When some paths cannot be read for reasons other than disappearing (a
    ``PermissionError`` on a single subdirectory, an ``EIO`` while listing a
    network mount, ...), a plain :class:`DirectorySnapshot` silently drops
    them. Diffing two such snapshots then reports every entry of that subtree
    as deleted, and every entry added while it was unreadable as deleted as
    well once it becomes readable again.

    This class instead remembers, for every path it could not read, the state
    it had in the ``previous`` snapshot. Those paths are kept in this snapshot
    as they were, so a diff against ``previous`` reports no change for them,
    while the rest of the tree is compared normally.

    :param path:
        The directory path for which a snapshot should be taken.
    :param previous:
        The snapshot taken by the previous poll. The last known state of the
        paths that cannot be read is inherited from it. When ``None``,
        unreadable paths are simply not present, like
        :class:`DirectorySnapshot` does.

    :attr:`unreadable` lists the paths whose state could not be refreshed.
    A directory that is genuinely gone (``ENOENT``) is *not* listed there: its
    entries are really deleted, and are reported as such.
    """

    def __init__(
        self,
        path: str,
        *,
        recursive: bool = True,
        stat: Callable[[str], os.stat_result] = os.stat,
        listdir: Callable[[str | None], Iterator[os.DirEntry]] = os.scandir,
        previous: DirectorySnapshot | None = None,
    ) -> None:
        # Paths whose *entries* are carried over from ``previous``, i.e. the
        # unreadable subtree roots. Their state is not refreshed.
        self._carried_roots: set[bytes | str] = set()
        super().__init__(path, recursive=recursive, stat=stat, listdir=listdir, previous=previous)

        # Keep the last known state of every unreadable subtree.
        if previous is not None:
            for unreadable_path in self._unreadable:
                self._carry_over(unreadable_path, previous)

    @staticmethod
    def _is_below(path: bytes | str, root: bytes | str) -> bool:
        """Whether ``path`` is a descendant of ``root``."""
        empty = b"" if isinstance(root, bytes) else ""
        return path != root and path.startswith(os.path.join(root, empty))

    def _carry_over(self, root: bytes | str, previous: DirectorySnapshot) -> None:
        """Copy the state of ``root`` and its descendants from ``previous``."""
        self._carried_roots.add(root)
        for path in previous.paths:
            if path != root and not self._is_below(path, root):
                continue
            if path in self._stat_info:
                # The path has been read again (e.g. it was recreated); prefer
                # the fresh state.
                continue
            st = previous.stat_info(path)
            self._stat_info[path] = st
            # Do not steal the inode from a freshly read entry (e.g. a hard
            # link): the fresh state must win.
            self._inode_to_path.setdefault((st.st_ino, st.st_dev), path)
            # Reuse the cached birth time rather than querying the live path,
            # which is exactly the path we failed to read.
            birthtimes = getattr(previous, "_birthtimes", {})
            if path in birthtimes:
                self._birthtimes[path] = birthtimes[path]

    def _on_listdir_error(self, root: str, error: OSError) -> None:
        # A disappearing directory is not an incomplete read: leaving it out
        # is accurate, and its children (if any) are really gone too.
        if not self._is_missing(error):
            self._unreadable[root] = error

    def _on_stat_error(self, path: bytes | str, error: OSError) -> None:
        if not self._is_missing(error):
            self._unreadable[path] = error

    @property
    def carried_over(self) -> set[bytes | str]:
        """Subtree roots whose entries were inherited from the previous snapshot."""
        return set(self._carried_roots)
