# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Async proxy for sqlite3.Blob incremental I/O handles
"""

import os
import sqlite3
from typing import Any, Optional, TYPE_CHECKING, Union

if TYPE_CHECKING:
    from .core import Connection


class Blob:
    """
    Async wrapper around a ``sqlite3.Blob`` handle.

    All underlying blob operations are executed on the owning connection's
    worker thread. Each handle keeps its own independent read/write position,
    maintained by sqlite itself, so multiple handles never share state.

    Use ``Connection.blobopen()`` to create instances of this class.
    """

    def __init__(self, conn: "Connection", blob: sqlite3.Blob) -> None:
        self._conn = conn
        self._blob: Optional[sqlite3.Blob] = blob

    @property
    def _raw(self) -> sqlite3.Blob:
        if self._blob is None:
            raise ValueError("I/O operation on a closed blob")
        return self._blob

    async def _execute(self, fn, *args, **kwargs):
        """Run a blob method on the shared connection's thread."""
        return await self._conn._execute(fn, *args, **kwargs)

    async def read(self, length: int = -1) -> bytes:
        """
        Read up to ``length`` bytes from the current position.

        Returns fewer bytes than requested at end of blob, and ``b""`` once
        the end has been reached. A zero-length read returns ``b""`` without
        touching the blob, so it is never confused with EOF.
        """
        if length == 0:
            # zero-length read is not EOF; don't bother the worker thread
            self._raw  # still raise if the handle is closed
            return b""
        return await self._execute(self._raw.read, length)

    async def write(self, data: Union[bytes, bytearray, memoryview]) -> None:
        """
        Write ``data`` at the current position.

        Raises ``sqlite3.OperationalError`` if the blob was opened read-only,
        or if the handle was invalidated by a change to the underlying row.
        """
        await self._execute(self._raw.write, data)

    async def seek(self, offset: int, origin: int = os.SEEK_SET) -> None:
        """Move the read/write position of this handle."""
        await self._execute(self._raw.seek, offset, origin)

    async def tell(self) -> int:
        """Return this handle's current read/write position."""
        return await self._execute(self._raw.tell)

    async def close(self) -> None:
        """
        Close the blob handle.

        Tolerates being called after the connection has been closed; the
        underlying handle is then finalized by sqlite3 itself.
        """
        blob, self._blob = self._blob, None
        if blob is None:
            return
        try:
            await self._conn._execute(blob.close)
        except ValueError:
            # connection already closed; sqlite3 finalizes the handle itself
            pass

    async def __aenter__(self) -> "Blob":
        return self

    async def __aexit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        await self.close()
