# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Core implementation of aiosqlite proxies
"""

import asyncio
import logging
import sqlite3
from collections.abc import AsyncIterator, Generator, Iterable
from functools import partial
from pathlib import Path
from queue import Empty, Queue, SimpleQueue
from threading import Lock, Thread
from typing import Any, Callable, Literal, Optional, Union
from warnings import warn

from .context import contextmanager
from .cursor import Cursor

__all__ = ["connect", "Connection", "Cursor", "CancelToken"]

AuthorizerCallback = Callable[[int, str, str, str, str], int]

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


def cancel_future(fut: asyncio.Future) -> None:
    """Cancel a future if it hasn't completed already."""
    if not fut.done():
        fut.cancel()


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


class CancelToken:
    """
    Handle identifying a single queued or executing operation, allowing
    targeted cancellation without affecting any other operation.

    Awaiting the token yields the operation's result. Use `cancel()` to
    cancel only this specific operation.

    Cancellation is operation-scoped: an interrupt is only issued while this
    operation is still executing, and any interrupt that the target statement
    did not consume is absorbed before the connection moves on to the next
    queued operation, so a cancellation can never spill over onto a later
    query.

    Instances are created via `Connection.execute_cancellable`.
    """

    def __init__(
        self,
        connection: "Connection",
        fn: Callable[..., Any],
        args: tuple,
        kwargs: dict,
        wrapper: Optional[Callable[[Any], Any]] = None,
    ) -> None:
        self._connection = connection
        self._function = partial(fn, *args, **kwargs)
        self._wrapper = wrapper
        self._future = asyncio.get_event_loop().create_future()
        # Guards _started/_completed/_cancelled/_interrupt_issued against
        # races between the worker thread (executing the operation) and
        # cancel() callers.
        self._lock = Lock()
        self._started = False
        self._completed = False
        self._cancelled = False
        self._interrupt_issued = False

    def __await__(self) -> Generator[Any, None, Any]:
        return self._future.__await__()

    def __repr__(self) -> str:
        if self._cancelled:
            state = "cancelled"
        elif self._completed:
            state = "completed"
        elif self._started:
            state = "running"
        else:
            state = "queued"
        return f"<CancelToken {state} {self._function}>"

    @property
    def done(self) -> bool:
        """True if the operation finished executing or was cancelled."""
        return self._completed or self._cancelled

    def _run(self) -> Any:
        """Execute the operation on the connection's worker thread.

        :meta private:
        """
        with self._lock:
            if self._cancelled:
                # Cancelled while queued: follow queue rules and skip
                # execution when the item is reached in queue order.
                raise asyncio.CancelledError("operation cancelled while queued")
            self._started = True

        try:
            result = self._function()
        except BaseException:
            # Mark completion before the exception is delivered to the
            # future, so that cancel() can observe a just-finished operation
            # and never issues an interrupt that would land on the next
            # operation. Failed statements are reset by sqlite3, so a
            # sacrificial statement can absorb any leftover interrupt.
            with self._lock:
                self._completed = True
                drain = self._interrupt_issued
            if drain:
                self._drain_interrupt()
            raise

        # Mark completion before the result is delivered to the future, so
        # that cancel() can observe a just-finished operation and never
        # issues an interrupt that would land on the next operation.
        with self._lock:
            self._completed = True
            drain = self._interrupt_issued

        if drain and not self._drain_interrupt():
            # The interrupt issued for this operation is still pending: the
            # statement completed without consuming it and is still active
            # (e.g. an unfetched SELECT), which keeps the interrupt flag set,
            # so SQLite would apply it to whatever statement runs next on the
            # connection. Discard the result, reset the statement, and clear
            # the flag, so that later operations are never affected; the
            # operation itself reports the cancellation instead.
            if isinstance(result, sqlite3.Cursor):
                try:
                    result.close()
                except sqlite3.Error:
                    pass
            self._drain_interrupt()
            raise sqlite3.OperationalError("interrupted")

        if self._wrapper is not None:
            result = self._wrapper(result)
        return result

    def _drain_interrupt(self) -> bool:
        """Absorb a leftover interrupt before the next queued operation runs.

        An interrupt that was issued while this operation was executing may
        outlive the target statement (e.g. when the statement was blocked
        inside a user-defined function and already passed its last interrupt
        checkpoint). SQLite would then apply the pending interrupt to whatever
        statement runs next on the connection. Running a sacrificial no-op
        statement here, on the worker thread and before the next queued item
        is processed, consumes or clears that stray interrupt so later
        operations are never affected.

        :meta private:
        :return: True if no interrupt is pending anymore, False if the
            sacrificial statement was itself interrupted.
        """
        try:
            connection = self._connection._connection
            if connection is None:
                return True
            connection.execute("SELECT 1").fetchall()
            return True
        except sqlite3.Error:
            # The sacrificial statement absorbed the pending interrupt.
            return False

    def cancel(self) -> bool:
        """
        Cancel this specific operation, if it is still pending.

        - If the operation is still queued, it is marked cancelled and will be
          skipped when reached in the queue; awaiting it raises
          ``asyncio.CancelledError``. Nothing is executing on its behalf yet,
          so no interrupt is issued and the currently running operation (if
          any) is left alone.
        - If the operation is currently executing, a ``sqlite3`` interrupt is
          issued, aborting only the running statement. Awaiting the token
          raises ``sqlite3.OperationalError`` if the statement is aborted.
        - If the operation already finished executing — including the race
          where it completed just before this call — nothing happens, so no
          interrupt is left behind that could hit the next operation.

        :return: True if cancellation was issued, False if the operation had
            already completed or been cancelled.
        """
        with self._lock:
            if self._completed or self._cancelled:
                return False

            if not self._started:
                self._cancelled = True
                try:
                    loop = self._future.get_loop()
                    loop.call_soon_threadsafe(cancel_future, self._future)
                except RuntimeError:
                    # Event loop already closed; cancel directly.
                    cancel_future(self._future)
                return True

            # The worker thread is executing exactly this operation right now,
            # so the interrupt can only hit this operation.
            try:
                self._connection._conn.interrupt()
            except (ValueError, sqlite3.ProgrammingError):
                # Connection already closed; nothing left to interrupt.
                return False
            # Remember that an interrupt was issued, so that the worker can
            # absorb it if the target statement finishes without consuming it.
            self._interrupt_issued = True
            return True


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

    def execute_cancellable(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> CancelToken:
        """
        Queue a query for execution and return a `CancelToken` identifying
        this specific operation.

        Unlike `interrupt()`, which aborts whatever statement happens to be
        running on the connection at the time, the returned token carries the
        identity of this operation: `CancelToken.cancel()` only issues an
        interrupt if this operation is still executing, and is a no-op
        (returning False) if it already completed — so a slow query can be
        cancelled without any risk of interrupting a later query that has
        already started.

        Awaiting the token returns a `Cursor` for the query.

        Example::

            token = db.execute_cancellable("select * from slow_table")
            task = asyncio.ensure_future(token)
            ...
            token.cancel()  # cancels only this query, if still running
            cursor = await task  # raises OperationalError if interrupted
        """
        if parameters is None:
            parameters = []

        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        token = CancelToken(
            self,
            self._connection.execute,
            (sql, parameters),
            {},
            wrapper=partial(Cursor, self),
        )
        self._tx.put_nowait((token._future, token._run))
        return token

    async def interrupt(self) -> None:
        """
        Interrupt whatever query is currently executing on the connection.

        Note that this affects any statement running at the time of the call,
        which may not be the one you intend to cancel. Prefer
        `execute_cancellable()` and `CancelToken.cancel()` for targeted
        cancellation of a specific operation.
        """
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

    async def set_trace_callback(self, handler: Callable) -> None:
        await self._execute(self._conn.set_trace_callback, handler)

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
