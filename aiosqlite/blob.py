# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Async proxy for sqlite3 Blob handles, enabling incremental I/O on BLOB values
"""

import sqlite3
from os import SEEK_SET
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .core import Connection


class Blob:
    """
    Async proxy for a single sqlite3 Blob handle.

    Supports chunked reads and writes, seeking, and context-managed closing.
    Every underlying operation is executed on the owning connection's thread,
    so data is streamed in chunks rather than loaded into memory up front.

    Each Blob instance tracks its own position, independent of any other
    handle opened on the same value.
    """

    def __init__(self, conn: "Connection", blob: sqlite3.Blob, length: int) -> None:
        self._conn = conn
        self._blob = blob
        self._length = length

    async def _execute(self, fn, *args, **kwargs):
        """Execute the given function on the shared connection's thread."""
        return await self._conn._execute(fn, *args, **kwargs)

    @staticmethod
    def _translate_error(exc: sqlite3.OperationalError) -> sqlite3.OperationalError:
        """Map low-level sqlite errors to clearer blob-specific messages."""
        message = str(exc)
        if "readonly" in message:
            return sqlite3.OperationalError("cannot write to a read-only blob")
        if "aborted" in message:
            return sqlite3.OperationalError(
                "blob handle is no longer valid; "
                "the underlying row was modified after the blob was opened"
            )
        return exc

    async def read(self, length: int = -1) -> bytes:
        """
        Read up to ``length`` bytes from the current position and advance the
        position by the number of bytes read. If ``length`` is negative, read
        all remaining bytes.

        Returns ``b""`` when the end of the blob is reached. A zero-length
        read also returns ``b""`` but does not indicate EOF and does not
        change the position.

        Raises ``ValueError`` if the connection is closed, and
        ``sqlite3.OperationalError`` if the handle was invalidated by an
        update to the underlying row.
        """
        try:
            return await self._execute(self._blob.read, length)
        except sqlite3.OperationalError as e:
            raise self._translate_error(e) from e

    async def write(self, data: bytes) -> None:
        """
        Write ``data`` at the current position, advancing the position by
        ``len(data)``. The blob cannot be extended; writing past the end
        raises ``ValueError``.

        Raises ``sqlite3.OperationalError`` if the blob was opened read-only
        or if the handle was invalidated by an update to the underlying row,
        and ``ValueError`` if the connection is closed.
        """
        try:
            await self._execute(self._blob.write, data)
        except sqlite3.OperationalError as e:
            raise self._translate_error(e) from e

    async def tell(self) -> int:
        """Return the current read/write position within the blob."""
        return await self._execute(self._blob.tell)

    async def seek(self, offset: int, origin: int = SEEK_SET) -> None:
        """
        Move the current position to ``offset`` relative to ``origin``
        (``os.SEEK_SET``, ``os.SEEK_CUR``, or ``os.SEEK_END``).
        """
        await self._execute(self._blob.seek, offset, origin)

    async def close(self) -> None:
        """Close the blob handle."""
        await self._execute(self._blob.close)

    def __len__(self) -> int:
        """The total length of the blob in bytes, captured when it was opened."""
        return self._length

    async def __aenter__(self) -> "Blob":
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        await self.close()
