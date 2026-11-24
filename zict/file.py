from __future__ import annotations

import mmap
import os
import pathlib
import uuid
from collections.abc import Iterator
from urllib.parse import quote, unquote

from zict.common import ZictBase, locked

#: Suffix of the staging file that :meth:`File.__setitem__` writes to before renaming
#: it over the final, committed name. A committed name never ends with it: new-style
#: names always end with ``#<version>`` and ``#`` is percent-escaped inside keys, so
#: legacy (zict < 3.0) names, which have no version, contain no ``#`` either.
_TMP_SUFFIX = ".tmp"


class File(ZictBase[str, bytes]):
    """Mutable Mapping interface to a directory

    Keys must be strings, values must be buffers

    Note this shouldn't be used for interprocess persistence, as keys
    are cached in memory.

    Parameters
    ----------
    directory: str
        Directory to write to. If it already exists, existing files will be imported as
        mapping elements. If it doesn't exists, it will be created.
    memmap: bool (optional)
        If True, use `mmap` for reading. Defaults to False.

    Notes
    -----
    If you call methods of this class from multiple threads, access will be fast as long
    as atomic disk access such as ``open``, ``os.fstat``, and ``os.remove`` is fast.
    This is not always the case, e.g. in case of slow network mounts or spun-down
    magnetic drives.
    Bytes read/write in the files is not protected by locks; this could cause failures
    on Windows, NFS, and in general whenever it's not OK to delete a file while there
    are file descriptors open on it.

    ``__setitem__`` is atomic: the value is first written in full to a staging file
    next to its final destination, which is then renamed over the final, unique file
    name. If the write fails halfway through (disk full, short write, invalid value,
    ...) or the process dies, the previously committed value is left untouched and
    still readable both by this instance and by any other instance that opens the same
    directory. Staging files are recognizable by their ``.tmp`` suffix and are never
    imported as keys when a directory is scanned.

    A committed file that is superseded by a newer version of the same key is unlinked
    as soon as possible. When the OS refuses - typically on Windows, while a reader
    that fetched the old value still holds it open - the cleanup is retried later, and
    the file is invisible to both this instance and any other one in the meantime.

    Examples
    --------
    >>> z = File('myfile')  # doctest: +SKIP
    >>> z['x'] = b'123'  # doctest: +SKIP
    >>> z['x']  # doctest: +SKIP
    b'123'

    Also supports writing lists of bytes objects

    >>> z['y'] = [b'123', b'4567']  # doctest: +SKIP
    >>> z['y']  # doctest: +SKIP
    b'1234567'

    Or anything that can be used with file.write, like a memoryview

    >>> z['data'] = np.ones(5).data  # doctest: +SKIP
    """

    directory: str
    memmap: bool
    filenames: dict[str, str]
    _inc: int
    _stale: set[str]

    def __init__(self, directory: str | pathlib.Path, memmap: bool = False):
        super().__init__()
        self.directory = str(directory)
        self.memmap = memmap
        self.filenames = {}
        self._inc = 0
        self._stale = set()

        if not os.path.exists(self.directory):
            os.makedirs(self.directory, exist_ok=True)
        else:
            versions: dict[str, int] = {}
            max_version = -1
            for fn in os.listdir(self.directory):
                # Take uncommitted staging files into account too, so that a version
                # number is never handed out twice - not even to replace a write that
                # another instance is still in the middle of.
                version = self._name_version(fn)
                if version is not None:
                    max_version = max(max_version, version)

                parsed = self._parse_name(fn)
                if parsed is None:
                    # Leftover of an interrupted __setitem__ (short write, crash, ...).
                    # It was never committed, so it is not a key. Don't delete it
                    # either: another File instance may still be writing to it.
                    continue
                key, version = parsed
                if key not in versions:
                    versions[key] = version
                    self.filenames[key] = fn
                elif version > versions[key]:
                    # A newer committed version; the older one is now unreachable
                    self._stale.add(self.filenames[key])
                    versions[key] = version
                    self.filenames[key] = fn
                else:
                    # An older version of an already seen key; it must never be served.
                    # Don't delete it here: another live instance may still have it in
                    # its own index. Just remember it, so that __delitem__ can't let it
                    # resurrect the key from the dead.
                    self._stale.add(fn)
            self._inc = max_version + 1

    @staticmethod
    def _name_version(fn: str) -> int | None:
        """Return the version number embedded in a file name, committed or not."""
        _, sep, version = fn.rpartition("#")
        if not sep:
            # Name written by zict < 3.0, which had no version suffix
            return None
        if fn.endswith(_TMP_SUFFIX):
            # "<key>#<version>.<uuid>.tmp"
            version = version.split(".", 1)[0]
        return int(version) if version.isdecimal() else None

    @staticmethod
    def _parse_name(fn: str) -> tuple[str, int] | None:
        """Parse a file name found in the directory.

        Return ``(key, version)``, or None if *fn* is the staging file of an
        ``__setitem__`` that hasn't been committed (e.g. it failed or the process died
        halfway through). Uncommitted files must never be imported as keys.
        """
        if "#" in fn and fn.endswith(_TMP_SUFFIX):
            # A staging file. A name without "#" cannot be one of ours; it is a
            # legacy key instead.
            return None

        key, sep, version = fn.rpartition("#")
        if sep and version.isdecimal():
            return unquote(key), int(version)
        # Name written by zict < 3.0, which had no version suffix (and possibly a
        # literal "#", which is now percent-escaped inside keys)
        return unquote(fn), -1

    def _safe_key(self, key: str) -> str:
        """Escape key so that it is usable on all filesystems.

        Append to the filenames a unique suffix that changes every time this method is
        called. This prevents race conditions when another thread accesses the same
        key, e.g. ``__setitem__`` on one thread and ``__getitem__`` on another.
        """
        # `#` is escaped by quote and is supported by most file systems
        key = quote(key, safe="") + f"#{self._inc}"
        self._inc += 1
        return key

    @staticmethod
    def _tmp_key(fn: str) -> str:
        """Name of the staging file used to write the committed file *fn*.

        The ``.tmp`` suffix (and the uuid, which keeps two instances writing the same
        key from stepping on each other's staging file) makes
        :meth:`_parse_name` reject it, so that neither this instance nor any other one
        opening the directory ever mistakes a partially written file for a key.
        """
        return f"{fn}.{uuid.uuid4().hex}{_TMP_SUFFIX}"

    @staticmethod
    def _discard_tmp(path: str) -> None:
        """Best-effort removal of the staging file of a failed ``__setitem__``.

        Failing to remove it is harmless: an uncommitted file is never imported as a
        key. It is not an error worth masking the original exception with either.
        """
        try:
            os.remove(path)
        except OSError:
            pass

    def _drop(self, fn: str) -> None:
        """Remove the superseded committed file *fn*, remembering it if that fails.

        A file that is still held open (e.g. by a reader that fetched the old value
        just before the key was replaced, on Windows) cannot be unlinked. It must then
        be retried later: otherwise a later instance opening the directory would
        resurrect the stale value once the current version is gone.
        """
        try:
            os.remove(os.path.join(self.directory, fn))
        except FileNotFoundError:
            pass
        except OSError:
            self._stale.add(fn)
        else:
            self._stale.discard(fn)

    def _stale_files(self, key: str) -> list[str]:
        """Names of the already superseded files of *key* that couldn't be unlinked."""
        out = []
        for fn in list(self._stale):
            parsed = self._parse_name(fn)
            if parsed is not None and parsed[0] == key:
                out.append(fn)
        return out

    def _drop_stale(self, key: str, raise_on_error: bool = False) -> None:
        """Retry removing the superseded files of *key* that a reader kept locked.

        ``__delitem__`` passes ``raise_on_error=True``: the deletion must be
        all-or-nothing, or a leftover file would bring the key back to life when the
        directory is reopened.
        """
        for fn in self._stale_files(key):
            try:
                os.remove(os.path.join(self.directory, fn))
            except FileNotFoundError:
                self._stale.discard(fn)
            except OSError:
                if raise_on_error:
                    raise
                # Still locked; try again on the next replacement or deletion
            else:
                self._stale.discard(fn)

    def __str__(self) -> str:
        return f"<File: {self.directory}, {len(self)} elements>"

    __repr__ = __str__

    @locked
    def __getitem__(self, key: str) -> bytearray | memoryview:
        fn = os.path.join(self.directory, self.filenames[key])

        # distributed.protocol.numpy.deserialize_numpy_ndarray makes sure that, if the
        # numpy array was writeable before serialization, remains writeable afterwards.
        # If it receives a read-only buffer (e.g. from fh.read() or from a mmap to a
        # read-only file descriptor), it performs an expensive memcpy.
        # Note that this is a dask-specific feature; vanilla pickle.loads will instead
        # return an array with flags.writeable=False.

        if self.memmap:
            with open(fn, "r+b") as fh:
                return memoryview(mmap.mmap(fh.fileno(), 0))
        else:
            with open(fn, "rb") as fh:
                size = os.fstat(fh.fileno()).st_size
                buf = bytearray(size)
                with self.unlock():
                    nread = fh.readinto(buf)
                assert nread == size
                return buf

    @locked
    def __setitem__(
        self,
        key: str,
        value: bytes
        | bytearray
        | memoryview
        | list[bytes | bytearray | memoryview]
        | tuple[bytes | bytearray | memoryview, ...],
    ) -> None:
        fn = self._safe_key(key)
        tmp_path = os.path.join(self.directory, self._tmp_key(fn))
        # Write the whole value to a staging file first, then atomically rename it over
        # its final name. Until this succeeds, the previously committed file of the key
        # - and the mapping entry that points at it - are left untouched, so readers of
        # this instance and of any other File opening the same directory keep getting
        # the old, complete value. A half-written value is never observable: if the
        # write fails we just drop the staging file and re-raise.
        try:
            with open(tmp_path, "wb") as fh, self.unlock():
                if isinstance(value, (tuple, list)):
                    fh.writelines(value)
                else:
                    fh.write(value)
            os.replace(tmp_path, os.path.join(self.directory, fn))
        except BaseException:
            self._discard_tmp(tmp_path)
            raise

        old_fn = self.filenames.get(key)
        self.filenames[key] = fn
        if old_fn is not None:
            self._drop(old_fn)
        # Retry cleaning up whatever a reader kept locked on previous replacements
        self._drop_stale(key)

    def __contains__(self, key: object) -> bool:
        return key in self.filenames

    def __iter__(self) -> Iterator[str]:
        return iter(self.filenames)

    @locked
    def __delitem__(self, key: str) -> None:
        fn = self.filenames[key]
        # Get rid of any superseded file that a reader prevented us from unlinking
        # earlier first; if it can't go, the whole deletion fails, so that the key
        # can't come back by itself when the directory is reopened.
        self._drop_stale(key, raise_on_error=True)
        os.remove(os.path.join(self.directory, fn))
        del self.filenames[key]

    def __len__(self) -> int:
        return len(self.filenames)
