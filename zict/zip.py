from __future__ import annotations

import os
import tempfile
import zipfile
from collections.abc import Iterator, MutableMapping
from typing import TYPE_CHECKING, Any, Literal

if TYPE_CHECKING:
    # TODO: import from typing (needs Python >=3.10)
    from typing_extensions import TypeAlias

FileMode: TypeAlias = Literal["r", "w", "x", "a"]


class Zip(MutableMapping[str, bytes]):
    """Mutable Mapping interface to a Zip file

    Keys must be strings, values must be bytes

    Parameters
    ----------
    filename: string
    mode: string, ('r', 'w', 'a'), defaults to 'a'

    Notes
    -----
    None of this class is thread-safe - not even normally trivial methods such as
    ``__len__ `` or ``__contains__``.

    Examples
    --------
    >>> z = Zip('myfile.zip')  # doctest: +SKIP
    >>> z['x'] = b'123'  # doctest: +SKIP
    >>> z['x']  # doctest: +SKIP
    b'123'
    >>> z.flush()  # flush and write metadata to disk  # doctest: +SKIP
    """

    filename: str
    mode: FileMode | Literal["closed"]
    _file: zipfile.ZipFile | None

    def __init__(self, filename: str, mode: FileMode = "a"):
        super().__init__()
        self.filename = filename
        self.mode = mode
        self._file = None

    @property
    def file(self) -> zipfile.ZipFile:
        if self.mode == "closed":
            raise OSError("File closed")
        if not self._file or not self._file.fp:
            self._file = zipfile.ZipFile(self.filename, mode=self.mode)
        return self._file

    def __getitem__(self, key: str) -> bytes:
        if not isinstance(key, str):
            raise KeyError(key)
        return self.file.read(key)

    def __setitem__(self, key: str, value: bytes | bytearray | memoryview) -> None:
        if not isinstance(key, str):
            raise TypeError(key)
        if not isinstance(value, (bytes, bytearray, memoryview)):
            raise TypeError(value)
        if key in self:
            raise NotImplementedError("Not supported by stdlib zipfile")
        self.file.writestr(key, value)

    def __iter__(self) -> Iterator[str]:
        return (zi.filename for zi in self.file.filelist)

    def __contains__(self, key: object) -> bool:
        if not isinstance(key, str):
            return False
        try:
            self.file.getinfo(key)
            return True
        except KeyError:
            return False

    def __delitem__(self, key: str) -> None:  # pragma: nocover
        raise NotImplementedError("Not supported by stdlib zipfile")

    def __len__(self) -> int:
        return len(self.file.filelist)

    def flush(self) -> None:
        if self._file:
            if self._file.fp:
                self._file.fp.flush()
            self._file.close()
            self._file = None

    def compact(self) -> None:
        """Rewrite the zip archive on disk so that each key appears exactly
        once, with its current value.

        Archives produced by other tools may contain multiple entries for the
        same key; reading always yields the last one. This method rewrites the
        archive, dropping all superseded entries. It never changes which
        values are visible through the mapping.

        The rewrite is atomic: the compacted contents are first written to a
        temporary file in the same directory, which then replaces the
        original archive. If the rewrite fails partway through (e.g. the disk
        fills up), the temporary file is deleted and the original archive is
        left untouched, so readers that already hold it open - as well as
        mappings opened afterwards - keep seeing the old contents.

        On Windows, the final replacement fails with :class:`PermissionError`
        if another process still holds the old archive open. In that case the
        archive on disk is still the old version; the call can simply be
        retried once the readers are done.

        Raises
        ------
        ValueError
            If the mapping was opened in read-only mode
        OSError
            If the mapping is closed, or if writing or replacing the archive
            failed (the original archive is left untouched)
        """
        if self.mode == "closed":
            raise OSError("File closed")
        if self.mode == "r":
            raise ValueError("Cannot compact a Zip file opened in read-only mode")
        # Flush pending writes and release our own handle, so that it can't
        # block the replacement below on Windows.
        self.flush()
        if not os.path.exists(self.filename):
            return

        dirname, basename = os.path.split(os.path.abspath(self.filename))
        fd, tmpname = tempfile.mkstemp(prefix=basename + ".", suffix=".tmp", dir=dirname)
        os.close(fd)
        try:
            with zipfile.ZipFile(self.filename, mode="r") as src:
                # Deduplicate keys; the last entry for each key is the
                # one that reads resolve to.
                infos = {}
                for zi in src.infolist():
                    infos[zi.filename] = zi
                with zipfile.ZipFile(tmpname, mode="w") as dst:
                    for zi in infos.values():
                        dst.writestr(zi, src.read(zi.filename))
            os.replace(tmpname, self.filename)
        except BaseException:
            # The partially written output must not be mistaken for the
            # archive; the old version is still the current one.
            try:
                os.remove(tmpname)
            except OSError:
                pass
            raise

    def close(self) -> None:
        self.flush()
        self.mode = "closed"

    def __enter__(self) -> Zip:
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()
