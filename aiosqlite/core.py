# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Core implementation of aiosqlite proxies
"""

import asyncio
import inspect
import logging
import sqlite3
from collections.abc import AsyncIterator, Generator, Iterable
from contextlib import asynccontextmanager
from functools import partial
from pathlib import Path
from queue import Empty, Queue, SimpleQueue
from threading import Lock, Thread
from typing import Any, Callable, Literal, Optional, Union
from warnings import warn

from .context import contextmanager
from .cursor import Cursor

__all__ = ["connect", "Connection", "Cursor"]

AuthorizerCallback = Callable[[int, str, str, str, str], int]
ProgressCallback = Callable[[int, bool], Any]

LOG = logging.getLogger("aiosqlite")


IsolationLevel = Optional[Literal["DEFERRED", "IMMEDIATE", "EXCLUSIVE"]]


def set_result(fut: asyncio.Future, result: Any) -> None:
    """Set the result of a future if it hasn't been set already."""
    if not fut.done():
        fut.set_result(result)


def set_exception(fut: asyncio.Future, e: BaseException) -> None:
    """Set the exception of a future if it hasn't been set already."""
    if not fut.done():
        fut.set_exception(e)


_STOP_RUNNING_SENTINEL = object()
_TxQueue = SimpleQueue[tuple[Optional[asyncio.Future], Callable[[], Any]]]


def _connection_worker_thread(tx: _TxQueue):
    """
    Execute function calls on a separate thread.

    :meta private:
    """
    while True:
        # Continues running until all queue items are processed,
        # even after connection is closed (so we can finalize all
        # futures)

        future, function = tx.get()

        try:
            LOG.debug("executing %s", function)
            result = function()

            if future:
                future.get_loop().call_soon_threadsafe(set_result, future, result)
            LOG.debug("operation %s completed", function)

            if result is _STOP_RUNNING_SENTINEL:
                break

        except BaseException as e:  # noqa B036
            LOG.debug("returning exception %s", e)
            if future:
                future.get_loop().call_soon_threadsafe(set_exception, future, e)


class _ProgressDispatcher:
    """
    Operation-scoped sqlite3 progress handler.

    Installed on the worker thread for the duration of a single operation.
    Each invocation is forwarded to the owning event loop via
    ``loop.call_soon_threadsafe`` -- async functions cannot run inside the
    sqlite3 callback itself.  At most ``max_pending`` deliveries may be in
    flight; further events are coalesced into the next delivery and flagged.

    :meta private:
    """

    def __init__(
        self,
        loop: asyncio.AbstractEventLoop,
        callback: ProgressCallback,
        max_pending: int,
    ) -> None:
        self._loop = loop
        self._callback = callback
        self._max_pending = max(1, max_pending)
        self._pending = 0
        self._coalesced = 0
        self._active = True
        self._lock = Lock()

    def __call__(self) -> int:
        """sqlite3 progress handler, invoked on the worker thread."""
        with self._lock:
            if not self._active:
                return 0
            if self._pending >= self._max_pending:
                # Backlog is full: merge this event into the next delivery.
                self._coalesced += 1
                return 0
            self._pending += 1
            coalesced = self._coalesced
            self._coalesced = 0

        try:
            self._loop.call_soon_threadsafe(self._deliver, 1 + coalesced, coalesced > 0)
        except RuntimeError:
            # Owning event loop is already closed; drop the event.
            with self._lock:
                self._pending -= 1
        return 0

    def _deliver(self, ticks: int, coalesced: bool) -> None:
        """Run the user callback on the owning event loop."""
        with self._lock:
            self._pending -= 1
        try:
            result = self._callback(ticks, coalesced)
        except Exception:
            LOG.exception("progress callback raised")
            return
        if inspect.isawaitable(result):
            asyncio.ensure_future(self._await_result(result))

    @staticmethod
    async def _await_result(awaitable: Any) -> None:
        try:
            await awaitable
        except Exception:
            LOG.exception("progress callback raised")

    def close(self) -> None:
        """Stop delivering events, even if sqlite3 still invokes the handler."""
        with self._lock:
            self._active = False


class Connection:
    def __init__(
        self,
        connector: Callable[[], sqlite3.Connection],
        iter_chunk_size: int,
        loop: Optional[asyncio.AbstractEventLoop] = None,
    ) -> None:
        self._running = True
        self._connection: Optional[sqlite3.Connection] = None
        self._connector = connector
        self._tx: _TxQueue = SimpleQueue()
        self._iter_chunk_size = iter_chunk_size
        self._thread = Thread(target=_connection_worker_thread, args=(self._tx,))
        # Last global progress handler installed via set_progress_handler(),
        # used to restore state after an operation-scoped subscription ends.
        self._progress_handler: Optional[tuple[Callable[[], Optional[int]], int]] = (
            None
        )

        if loop is not None:
            warn(
                "aiosqlite.Connection no longer uses the `loop` parameter",
                DeprecationWarning,
            )

    def __del__(self):
        if self._connection is None:
            return

        warn(
            (
                f"{self!r} was deleted before being closed. "
                "Please use 'async with' or '.close()' to close the connection properly."
            ),
            ResourceWarning,
            stacklevel=1,
        )

        # Don't try to be creative here, the event loop may have already been closed.
        # Simply stop the worker thread, and let the underlying sqlite3 connection
        # be finalized by its own __del__.
        self.stop()

    def stop(self) -> Optional[asyncio.Future]:
        """Stop the background thread. Prefer `async with` or `await close()`"""
        self._running = False

        def close_and_stop():
            if self._connection is not None:
                self._connection.close()
                self._connection = None
            return _STOP_RUNNING_SENTINEL

        try:
            future = asyncio.get_event_loop().create_future()
        except Exception:
            future = None

        self._tx.put_nowait((future, close_and_stop))
        return future

    @property
    def _conn(self) -> sqlite3.Connection:
        if self._connection is None:
            raise ValueError("no active connection")

        return self._connection

    def _execute_insert(self, sql: str, parameters: Any) -> Optional[sqlite3.Row]:
        cursor = self._conn.execute(sql, parameters)
        cursor.execute("SELECT last_insert_rowid()")
        return cursor.fetchone()

    def _execute_fetchall(self, sql: str, parameters: Any) -> Iterable[sqlite3.Row]:
        cursor = self._conn.execute(sql, parameters)
        return cursor.fetchall()

    async def _execute(self, fn, *args, **kwargs):
        """Queue a function with the given arguments for execution."""
        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        function = partial(fn, *args, **kwargs)
        future = asyncio.get_event_loop().create_future()

        self._tx.put_nowait((future, function))

        return await future

    async def _connect(self) -> "Connection":
        """Connect to the actual sqlite database."""
        if self._connection is None:
            try:
                future = asyncio.get_event_loop().create_future()
                self._tx.put_nowait((future, self._connector))
                self._connection = await future
            except BaseException:
                self.stop()
                self._connection = None
                raise

        return self

    def __await__(self) -> Generator[Any, None, "Connection"]:
        self._thread.start()
        return self._connect().__await__()

    async def __aenter__(self) -> "Connection":
        return await self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        await self.close()

    @contextmanager
    async def cursor(self) -> Cursor:
        """Create an aiosqlite cursor wrapping a sqlite3 cursor object."""
        return Cursor(self, await self._execute(self._conn.cursor))

    async def commit(self) -> None:
        """Commit the current transaction."""
        await self._execute(self._conn.commit)

    async def rollback(self) -> None:
        """Roll back the current transaction."""
        await self._execute(self._conn.rollback)

    async def close(self) -> None:
        """Complete queued queries/cursors and close the connection."""

        if self._connection is None:
            return

        try:
            await self._execute(self._conn.close)
        except Exception:
            LOG.info("exception occurred while closing connection")
            raise
        finally:
            self._connection = None
            self._progress_handler = None
            future = self.stop()
            if future:
                await future

    @contextmanager
    async def execute(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> Cursor:
        """Helper to create a cursor and execute the given query."""
        if parameters is None:
            parameters = []
        cursor = await self._execute(self._conn.execute, sql, parameters)
        return Cursor(self, cursor)

    @contextmanager
    async def execute_insert(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> Optional[sqlite3.Row]:
        """Helper to insert and get the last_insert_rowid."""
        if parameters is None:
            parameters = []
        return await self._execute(self._execute_insert, sql, parameters)

    @contextmanager
    async def execute_fetchall(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> Iterable[sqlite3.Row]:
        """Helper to execute a query and return all the data."""
        if parameters is None:
            parameters = []
        return await self._execute(self._execute_fetchall, sql, parameters)

    @contextmanager
    async def executemany(
        self, sql: str, parameters: Iterable[Iterable[Any]]
    ) -> Cursor:
        """Helper to create a cursor and execute the given multiquery."""
        cursor = await self._execute(self._conn.executemany, sql, parameters)
        return Cursor(self, cursor)

    @contextmanager
    async def executescript(self, sql_script: str) -> Cursor:
        """Helper to create a cursor and execute a user script."""
        cursor = await self._execute(self._conn.executescript, sql_script)
        return Cursor(self, cursor)

    async def interrupt(self) -> None:
        """Interrupt pending queries."""
        return self._conn.interrupt()

    async def create_function(
        self, name: str, num_params: int, func: Callable, deterministic: bool = False
    ) -> None:
        """
        Create user-defined function that can be later used
        within SQL statements. Must be run within the same thread
        that query executions take place so instead of executing directly
        against the connection, we defer this to `run` function.

        If ``deterministic`` is true, the created function is marked as deterministic,
        which allows SQLite to perform additional optimizations. This flag is supported
        by SQLite 3.8.3 or higher, ``NotSupportedError`` will be raised if used with
        older versions.
        """
        await self._execute(
            self._conn.create_function,
            name,
            num_params,
            func,
            deterministic=deterministic,
        )

    @property
    def in_transaction(self) -> bool:
        return self._conn.in_transaction

    @property
    def isolation_level(self) -> Optional[str]:
        return self._conn.isolation_level

    @isolation_level.setter
    def isolation_level(self, value: IsolationLevel) -> None:
        self._conn.isolation_level = value

    @property
    def row_factory(self) -> Optional[type]:
        return self._conn.row_factory

    @row_factory.setter
    def row_factory(self, factory: Optional[type]) -> None:
        self._conn.row_factory = factory

    @property
    def text_factory(self) -> Callable[[bytes], Any]:
        return self._conn.text_factory

    @text_factory.setter
    def text_factory(self, factory: Callable[[bytes], Any]) -> None:
        self._conn.text_factory = factory

    @property
    def total_changes(self) -> int:
        return self._conn.total_changes

    async def enable_load_extension(self, value: bool) -> None:
        await self._execute(self._conn.enable_load_extension, value)  # type: ignore

    async def load_extension(self, path: str):
        await self._execute(self._conn.load_extension, path)  # type: ignore

    async def set_progress_handler(
        self, handler: Callable[[], Optional[int]], n: int
    ) -> None:
        await self._execute(self._conn.set_progress_handler, handler, n)
        self._progress_handler = (handler, n) if handler is not None else None

    async def set_trace_callback(self, handler: Callable) -> None:
        await self._execute(self._conn.set_trace_callback, handler)

    @asynccontextmanager
    async def progress_subscription(
        self,
        progress: ProgressCallback,
        n: int = 1000,
        max_pending: int = 16,
    ) -> AsyncIterator[None]:
        """
        Subscribe to query progress for the duration of a single operation.

        Installs an operation-scoped progress handler before the wrapped
        statements run, and restores the previous global handler (if any)
        when the block exits -- including on cancellation, callback errors,
        or event loop shutdown.  Statements executed afterwards without a
        subscription will not receive this operation's events.

        The sqlite3 progress handler runs on the connection's worker thread,
        where async functions cannot run, so events are safely delivered back
        to the event loop that owns the subscription.  At most ``max_pending``
        deliveries may be in flight; additional events are coalesced into the
        next delivery and flagged.

        Example::

            async def on_progress(ticks: int, coalesced: bool) -> None:
                ...

            async with db.progress_subscription(on_progress, n=100):
                async with db.execute("select ...") as cursor:
                    rows = await cursor.fetchall()

        :param progress: Callback invoked on the owning event loop as
            ``progress(ticks, coalesced)``, where ``ticks`` is the number of
            progress events represented by this delivery and ``coalesced`` is
            ``True`` if events were merged because the backlog limit was
            reached.  May be a plain or async function; its return value
            cannot abort the query (use ``interrupt()`` for that).
        :param n: Number of sqlite3 VM instructions between progress events.
        :param max_pending: Maximum number of undelivered events to buffer
            before coalescing.
        """
        loop = asyncio.get_running_loop()
        dispatcher = _ProgressDispatcher(loop, progress, max_pending)
        previous = self._progress_handler
        await self._execute(self._conn.set_progress_handler, dispatcher, n)
        try:
            yield
        finally:
            dispatcher.close()
            try:
                await self._restore_progress_handler(previous, loop)
            except Exception:
                LOG.exception("failed to restore previous progress handler")

    async def _restore_progress_handler(
        self,
        previous: Optional[tuple[Callable[[], Optional[int]], int]],
        loop: asyncio.AbstractEventLoop,
    ) -> None:
        """Restore the global progress handler after a subscription ends."""
        if not self._running or self._connection is None:
            return
        handler, n = previous if previous is not None else (None, 0)
        if loop.is_closed():
            # The owning loop is gone; queue the restore directly on the
            # worker thread so later operations don't see a stale handler.
            self._tx.put_nowait(
                (None, partial(self._conn.set_progress_handler, handler, n))
            )
            return
        await self._execute(self._conn.set_progress_handler, handler, n)

    @contextmanager
    async def execute_with_progress(
        self,
        sql: str,
        parameters: Optional[Iterable[Any]] = None,
        *,
        progress: ProgressCallback,
        n: int = 1000,
        max_pending: int = 16,
    ) -> Cursor:
        """
        Execute a query with an operation-scoped progress subscription.

        Convenience wrapper combining ``progress_subscription`` and
        ``execute``; see ``progress_subscription`` for details.
        """
        if parameters is None:
            parameters = []
        async with self.progress_subscription(progress, n=n, max_pending=max_pending):
            cursor = await self._execute(self._conn.execute, sql, parameters)
        return Cursor(self, cursor)


    async def set_authorizer(
        self, authorizer_callback: Optional[AuthorizerCallback]
    ) -> None:
        """
        Set an authorizer callback to control database access.

        The authorizer callback is invoked for each SQL statement that is prepared,
        and controls whether specific operations are permitted.

        Example::

            import sqlite3

            def restrict_drops(action_code, arg1, arg2, db_name, trigger_name):
                # Deny all DROP operations
                if action_code == sqlite3.SQLITE_DROP_TABLE:
                    return sqlite3.SQLITE_DENY
                # Allow everything else
                return sqlite3.SQLITE_OK

            await conn.set_authorizer(restrict_drops)

        See ``sqlite3`` documentation for details:
        https://docs.python.org/3/library/sqlite3.html#sqlite3.Connection.set_authorizer

        :param authorizer_callback: An optional callable that receives five arguments:

            - ``action_code`` (int): The action to be authorized (e.g., ``SQLITE_READ``)
            - ``arg1`` (str): First argument, meaning depends on ``action_code``
            - ``arg2`` (str): Second argument, meaning depends on ``action_code``
            - ``db_name`` (str): Database name (e.g., ``"main"``, ``"temp"``)
            - ``trigger_name`` (str): Name of trigger or view that is doing the access,
              or ``None``

            The callback should return:

            - ``SQLITE_OK`` (0): Allow the operation
            - ``SQLITE_DENY`` (1): Deny the operation, raise ``sqlite3.DatabaseError``
            - ``SQLITE_IGNORE`` (2): Treat operation as no-op

            Pass ``None`` to remove the authorizer.
        """
        await self._execute(self._conn.set_authorizer, authorizer_callback)

    async def iterdump(self) -> AsyncIterator[str]:
        """
        Return an async iterator to dump the database in SQL text format.

        Example::

            async for line in db.iterdump():
                ...

        """
        dump_queue: Queue = Queue()

        def dumper():
            try:
                for line in self._conn.iterdump():
                    dump_queue.put_nowait(line)
                dump_queue.put_nowait(None)

            except Exception:
                LOG.exception("exception while dumping db")
                dump_queue.put_nowait(None)
                raise

        fut = self._execute(dumper)
        task = asyncio.ensure_future(fut)

        while True:
            try:
                line: Optional[str] = dump_queue.get_nowait()
                if line is None:
                    break
                yield line

            except Empty:
                if task.done():
                    LOG.warning("iterdump completed unexpectedly")
                    break

                await asyncio.sleep(0.01)

        await task

    async def backup(
        self,
        target: Union["Connection", sqlite3.Connection],
        *,
        pages: int = 0,
        progress: Optional[Callable[[int, int, int], None]] = None,
        name: str = "main",
        sleep: float = 0.250,
    ) -> None:
        """
        Make a backup of the current database to the target database.

        Takes either a standard sqlite3 or aiosqlite Connection object as the target.
        """
        if isinstance(target, Connection):
            target = target._conn

        await self._execute(
            self._conn.backup,
            target,
            pages=pages,
            progress=progress,
            name=name,
            sleep=sleep,
        )


def connect(
    database: Union[str, Path],
    *,
    iter_chunk_size=64,
    loop: Optional[asyncio.AbstractEventLoop] = None,
    **kwargs: Any,
) -> Connection:
    """Create and return a connection proxy to the sqlite database."""

    if loop is not None:
        warn(
            "aiosqlite.connect() no longer uses the `loop` parameter",
            DeprecationWarning,
        )

    def connector() -> sqlite3.Connection:
        if isinstance(database, str):
            loc = database
        elif isinstance(database, bytes):
            loc = database.decode("utf-8")
        else:
            loc = str(database)

        return sqlite3.connect(loc, **kwargs)

    return Connection(connector, iter_chunk_size)
