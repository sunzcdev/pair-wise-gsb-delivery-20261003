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
        Fetch up to ``size`` rows, or up to the cursor's ``arraysize`` number
        of rows when ``size`` is omitted.

        The effective ``arraysize`` is read from the underlying cursor on the
        connection's operation thread when this fetch runs, so it follows the
        operation queue order rather than a cached value: a fetch queued before
        a :meth:`set_arraysize` call uses the old value and later fetches use
        the new one.
        """
        args: tuple[int, ...] = ()
        if size is not None:
            args = (size,)
        return await self._execute(self._cursor.fetchmany, *args)

    async def fetchall(self) -> Iterable[sqlite3.Row]:
        """Fetch all remaining rows."""
        return await self._execute(self._cursor.fetchall)

    async def close(self) -> None:
        """Close the cursor."""
        await self._execute(self._cursor.close)

    def _set_arraysize(self, value: int) -> None:
        self._cursor.arraysize = value

    def _set_row_factory(
        self, factory: Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]]
    ) -> None:
        self._cursor.row_factory = factory

    async def set_arraysize(self, value: int) -> None:
        """
        Set the number of rows fetched by :meth:`fetchmany` without an explicit
        ``size`` argument.

        The assignment is queued on the connection's operation thread and takes
        effect in queue order: fetches already queued or running keep using the
        previous value, while every later fetch observes the new one, as no
        fetch caches this setting. An invalid value raises (for example
        :class:`TypeError` for a non-integer) and the assignment is not applied
        at all, leaving the previous value in place.
        """
        await self._execute(self._set_arraysize, value)

    async def get_arraysize(self) -> int:
        """
        Return the current ``arraysize`` as a snapshot read from the
        connection's operation thread.

        The read is queued like any other connection operation, so it reflects
        the value at this point in the operation queue rather than whatever the
        event loop happens to see through the synchronous property.
        """
        return await self._execute(getattr, self._cursor, "arraysize")

    async def set_row_factory(
        self,
        factory: Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]],
    ) -> None:
        """
        Set the row factory used to build rows returned by later fetches.

        The assignment is queued on the connection's operation thread and takes
        effect in queue order: a fetch already queued or running builds its rows
        with the previous factory, while every later fetch uses the new one.
        The factory itself is never cached by queued fetches, so resetting it to
        ``None`` restores plain tuple rows for subsequent fetches. The
        assignment is a single, atomic operation that cannot be partially
        applied; a factory sqlite3 cannot call is stored as-is and raises
        :class:`TypeError` only when the next fetch tries to build a row,
        leaving the cursor recoverable by setting a valid factory.
        """
        await self._execute(self._set_row_factory, factory)

    async def get_row_factory(
        self,
    ) -> Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]]:
        """
        Return the current row factory as a snapshot read from the
        connection's operation thread.

        The read is queued like any other connection operation, so it reflects
        the factory at this point in the operation queue rather than whatever
        the event loop happens to see through the synchronous property.
        """
        return await self._execute(getattr, self._cursor, "row_factory")

    @property
    def rowcount(self) -> int:
        return self._cursor.rowcount

    @property
    def lastrowid(self) -> Optional[int]:
        return self._cursor.lastrowid

    @property
    def arraysize(self) -> int:
        """
        Direct, synchronous access to the underlying cursor's ``arraysize``.

        Kept for compatibility with the standard :class:`sqlite3.Cursor` API.
        This bypasses the connection's operation queue and provides no ordering
        guarantee relative to queued fetches; use :meth:`set_arraysize` and
        :meth:`get_arraysize` when interleaving configuration changes with
        asynchronous operations.
        """
        return self._cursor.arraysize

    @arraysize.setter
    def arraysize(self, value: int) -> None:
        self._cursor.arraysize = value

    @property
    def description(self) -> tuple[tuple[str, None, None, None, None, None, None], ...]:
        return self._cursor.description

    @property
    def row_factory(self) -> Optional[Callable[[sqlite3.Cursor, sqlite3.Row], object]]:
        """
        Direct, synchronous access to the underlying cursor's row factory.

        Kept for compatibility with the standard :class:`sqlite3.Cursor` API.
        This bypasses the connection's operation queue and provides no ordering
        guarantee relative to queued fetches; use :meth:`set_row_factory` and
        :meth:`get_row_factory` when interleaving configuration changes with
        asynchronous operations.
        """
        return self._cursor.row_factory

    @row_factory.setter
    def row_factory(self, factory: Optional[type]) -> None:
        self._cursor.row_factory = factory

    @property
    def connection(self) -> sqlite3.Connection:
        return self._cursor.connection

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        await self.close()
