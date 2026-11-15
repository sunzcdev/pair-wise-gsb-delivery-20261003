# Copyright Amethyst Reese
# Licensed under the MIT license

import sqlite3
from collections.abc import AsyncIterator, Iterable
from typing import Any, Callable, Optional, TYPE_CHECKING

if TYPE_CHECKING:
    from .core import Connection


class Cursor:
    def __init__(self, conn: "Connection", cursor: sqlite3.Cursor) -> None:
        self.iter_chunk_size = conn._iter_chunk_size
        self._conn = conn
        self._cursor = cursor

    def __aiter__(self) -> AsyncIterator[sqlite3.Row]:
        """The cursor proxy is also an async iterator."""
        return self._fetch_chunked()

    async def _fetch_chunked(self):
        while True:
            rows = await self.fetchmany(self.iter_chunk_size)
            if not rows:
                return
            for row in rows:
                yield row

    async def _execute(self, fn, *args, **kwargs):
        """Execute the given function on the shared connection's thread."""
        return await self._conn._execute(fn, *args, **kwargs)

    async def execute(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> "Cursor":
        """Execute the given query."""
        if parameters is None:
            parameters = []
        await self._execute(self._cursor.execute, sql, parameters)
        return self

    async def executemany(
        self, sql: str, parameters: Iterable[Iterable[Any]]
    ) -> "Cursor":
        """Execute the given multiquery."""
        await self._execute(self._cursor.executemany, sql, parameters)
        return self

    async def executescript(self, sql_script: str) -> "Cursor":
        """Execute a user script."""
        await self._execute(self._cursor.executescript, sql_script)
        return self

    async def fetchone(self) -> Optional[sqlite3.Row]:
        """Fetch a single row."""
        return await self._execute(self._cursor.fetchone)

    async def fetchmany(self, size: Optional[int] = None) -> Iterable[sqlite3.Row]:
        """
        Fetch up to `cursor.arraysize` number of rows.

        When `size` is not given, the cursor's `arraysize` is read on the
        connection thread as part of this queued operation. The batch size
        therefore reflects every :meth:`set_arraysize` call queued before
        this fetch, and none queued after it; no previously cached value
        is reused by subsequent fetches.
        """
        if size is None:

            def fetch_arraysize() -> Iterable[sqlite3.Row]:
                return self._cursor.fetchmany(self._cursor.arraysize)

            return await self._execute(fetch_arraysize)
        return await self._execute(self._cursor.fetchmany, size)

    async def fetchall(self) -> Iterable[sqlite3.Row]:
        """Fetch all remaining rows."""
        return await self._execute(self._cursor.fetchall)

    async def close(self) -> None:
        """Close the cursor."""
        await self._execute(self._cursor.close)

    @property
    def rowcount(self) -> int:
        return self._cursor.rowcount

    @property
    def lastrowid(self) -> Optional[int]:
        return self._cursor.lastrowid

    @property
    def arraysize(self) -> int:
        """
        The default number of rows fetched by :meth:`fetchmany`.

        .. note::

            This synchronous property accesses the underlying ``sqlite3``
            cursor directly from the event loop thread, bypassing the
            connection's operation queue. It is provided for compatibility
            and is only well-defined when no operations on this connection
            are queued or in flight (for example, before the first
            ``await`` on a fresh cursor). When interleaving configuration
            with queued fetches, use :meth:`set_arraysize` and
            :meth:`get_arraysize` instead.
        """
        return self._cursor.arraysize

    @arraysize.setter
    def arraysize(self, value: int) -> None:
        self._cursor.arraysize = value

    async def set_arraysize(self, value: int) -> None:
        """
        Set the default fetch batch size, applied in operation queue order.

        The assignment is queued on the connection thread, so it takes
        effect after every operation queued before this call and before
        every operation queued after it. The value is validated before
        being queued: invalid values raise immediately and the previous
        setting is left untouched (no partial application).
        """
        if not isinstance(value, int):
            raise TypeError(
                f"arraysize must be an int, not {type(value).__name__}"
            )
        await self._execute(setattr, self._cursor, "arraysize", value)

    async def get_arraysize(self) -> int:
        """
        Read a snapshot of `arraysize`, consistent with queue order.

        The read is queued on the connection thread, so the result
        reflects every :meth:`set_arraysize` call queued before this
        call and none queued after it.
        """
        return await self._execute(getattr, self._cursor, "arraysize")

    @property
    def description(self) -> tuple[tuple[str, None, None, None, None, None, None], ...]:
        return self._cursor.description

    @property
    def row_factory(self) -> Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]]:
        """
        The callable used to convert result rows.

        .. note::

            This synchronous property accesses the underlying ``sqlite3``
            cursor directly from the event loop thread, bypassing the
            connection's operation queue. It is provided for compatibility
            and is only well-defined when no operations on this connection
            are queued or in flight (for example, before the first
            ``await`` on a fresh cursor). When interleaving configuration
            with queued fetches, use :meth:`set_row_factory` and
            :meth:`get_row_factory` instead.
        """
        return self._cursor.row_factory

    @row_factory.setter
    def row_factory(self, factory: Optional[type]) -> None:
        self._cursor.row_factory = factory

    async def set_row_factory(
        self, factory: Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]]
    ) -> None:
        """
        Set the row factory, applied in operation queue order.

        The assignment is queued on the connection thread, so it takes
        effect after every operation queued before this call and before
        every operation queued after it. Because ``sqlite3`` consults the
        row factory when rows are produced, a queued change also affects
        fetches from a query that was executed earlier. The value is
        validated before being queued: invalid values raise immediately
        and the previous setting is left untouched (no partial
        application).
        """
        if factory is not None and not callable(factory):
            raise TypeError(
                f"row_factory must be callable or None, not {type(factory).__name__}"
            )
        await self._execute(setattr, self._cursor, "row_factory", factory)

    async def get_row_factory(
        self,
    ) -> Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]]:
        """
        Read a snapshot of `row_factory`, consistent with queue order.

        The read is queued on the connection thread, so the result
        reflects every :meth:`set_row_factory` call queued before this
        call and none queued after it.
        """
        return await self._execute(getattr, self._cursor, "row_factory")

    @property
    def connection(self) -> sqlite3.Connection:
        return self._cursor.connection

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        await self.close()
