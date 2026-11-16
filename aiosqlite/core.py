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
from threading import Thread
from typing import Any, Callable, Literal, Optional, Union
from warnings import warn

from .context import contextmanager
from .cursor import Cursor

__all__ = ["connect", "Connection", "Cursor", "Transaction"]

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

        # Explicit transaction context state. All of this is per-connection;
        # no global locks are used, so other connections are never blocked.
        self._txn_owner: Optional[asyncio.Task] = None
        self._txn_depth = 0
        self._txn_waiters: list[asyncio.Future] = []
        self._txn_cursors: list[Cursor] = []

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

    async def _txn_wait_while(self, blocked: Callable[[], bool]) -> None:
        """Wait until the given transaction-state predicate no longer holds."""
        loop = asyncio.get_running_loop()
        while blocked():
            waiter = loop.create_future()
            self._txn_waiters.append(waiter)
            try:
                await waiter
            finally:
                try:
                    self._txn_waiters.remove(waiter)
                except ValueError:
                    pass

    async def _txn_gate(self) -> None:
        """
        Block statements from other tasks while a transaction context is active.

        The task that owns the active transaction context passes through;
        every other task waits here until the transaction has ended.
        """
        task = asyncio.current_task()
        await self._txn_wait_while(
            lambda: self._txn_owner is not None and self._txn_owner is not task
        )

    async def _txn_acquire(self) -> None:
        """Take ownership of the connection for the current task's transaction."""
        await self._txn_wait_while(lambda: self._txn_owner is not None)
        # No await between the check above and here, so this is atomic with
        # respect to other tasks on the event loop.
        self._txn_owner = asyncio.current_task()
        self._txn_depth = 0

    def _txn_release(self) -> None:
        """Release transaction ownership and wake all waiting tasks."""
        self._txn_owner = None
        self._txn_depth = 0
        waiters, self._txn_waiters = self._txn_waiters, []
        for waiter in waiters:
            if not waiter.done():
                waiter.set_result(None)

    def _track_cursor(self, cursor: Cursor) -> Cursor:
        """Track cursors created inside a transaction context."""
        if self._txn_depth > 0:
            self._txn_cursors.append(cursor)
        return cursor

    def _invalidate_txn_cursors(self) -> list[sqlite3.Cursor]:
        """
        Invalidate cursors created inside the transaction context, so that
        they cannot be used after the transaction has ended.

        Returns the underlying sqlite3 cursors so they can be closed on the
        connection thread.
        """
        cursors, self._txn_cursors = self._txn_cursors, []
        raw = []
        for cursor in cursors:
            if not cursor._closed:
                cursor._closed = True
                cursor._close_message = (
                    "cursor was created inside a transaction context "
                    "that has already ended"
                )
                raw.append(cursor._cursor)
        return raw

    async def _execute(self, fn, *args, **kwargs):
        """Queue a function with the given arguments for execution."""
        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        await self._txn_gate()
        return await self._dispatch(fn, *args, **kwargs)

    async def _dispatch(self, fn, *args, **kwargs):
        """
        Queue a function for execution without consulting the transaction gate.

        Only for use by the transaction context manager, whose task already
        owns the transaction gate.
        """
        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        function = partial(fn, *args, **kwargs)
        future = asyncio.get_running_loop().create_future()

        self._tx.put_nowait((future, function))

        return await future

    async def _execute_critical(self, fn, *args, **kwargs):
        """
        Execute a function to completion even if the current task is cancelled.

        Used for transaction rollback, so that the database is left in a clean
        state before waiting tasks are allowed to run. The function is queued
        on the connection thread synchronously, guaranteeing it is ordered
        before any statement queued by tasks waiting on the transaction gate.
        """
        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        function = partial(fn, *args, **kwargs)
        future = asyncio.get_running_loop().create_future()
        self._tx.put_nowait((future, function))

        try:
            return await asyncio.shield(future)
        except asyncio.CancelledError:
            # The operation is already queued on the connection thread; wait
            # for it to actually finish before propagating the cancellation,
            # so that waiters are only released after it has completed.
            while not future.done():
                try:
                    await asyncio.wait([future])
                except asyncio.CancelledError:
                    pass
            if not future.cancelled() and future.exception() is not None:
                LOG.warning(
                    "critical operation failed during cancellation: %s",
                    future.exception(),
                )
            raise

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
        return self._track_cursor(Cursor(self, await self._execute(self._conn.cursor)))

    def _check_manual_txn_control(self, operation: str) -> None:
        if self._txn_depth > 0 and self._txn_owner is asyncio.current_task():
            raise sqlite3.ProgrammingError(
                f"cannot manually {operation} while inside a transaction "
                "context; exit the context normally to commit, or raise "
                "an exception to roll back"
            )

    async def commit(self) -> None:
        """Commit the current transaction."""
        self._check_manual_txn_control("commit")
        await self._execute(self._conn.commit)

    async def rollback(self) -> None:
        """Roll back the current transaction."""
        self._check_manual_txn_control("rollback")
        await self._execute(self._conn.rollback)

    def transaction(self, isolation: IsolationLevel = None) -> "Transaction":
        """
        Create an explicit transaction context bound to the current task.

        While the context is active, statements from every *other* task using
        this connection wait until the transaction is committed or rolled
        back, so statements from concurrent tasks can no longer interleave
        between ``BEGIN`` and ``COMMIT``. Other connections are not affected.

        Example::

            async with db.transaction():
                await db.execute("insert into foo values (1)")
                await db.execute("insert into foo values (2)")

        The transaction is committed when the context exits normally, and
        rolled back if the body raises or the task is cancelled. Nested
        ``transaction()`` contexts in the same task use SAVEPOINTs. If the
        connection is already inside a manually-started transaction, entering
        the context raises ``sqlite3.ProgrammingError`` instead of taking
        over that transaction.

        Cursors created inside the context are invalidated when it exits;
        using them afterwards raises ``sqlite3.ProgrammingError``.

        :param isolation: Optional transaction mode: ``"DEFERRED"``,
            ``"IMMEDIATE"``, or ``"EXCLUSIVE"``.
        """
        return Transaction(self, isolation)

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
        return self._track_cursor(Cursor(self, cursor))

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
        return self._track_cursor(Cursor(self, cursor))

    @contextmanager
    async def executescript(self, sql_script: str) -> Cursor:
        """Helper to create a cursor and execute a user script."""
        cursor = await self._execute(self._conn.executescript, sql_script)
        return self._track_cursor(Cursor(self, cursor))

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


class Transaction:
    """
    Async context manager binding an explicit transaction to the current task.

    Created by :meth:`Connection.transaction`; see its documentation for
    details. Not intended to be instantiated directly.
    """

    _VALID_ISOLATION_LEVELS = (None, "DEFERRED", "IMMEDIATE", "EXCLUSIVE")

    def __init__(self, conn: Connection, isolation: IsolationLevel = None) -> None:
        if isolation not in self._VALID_ISOLATION_LEVELS:
            raise ValueError(f"invalid transaction isolation level: {isolation!r}")
        self._conn = conn
        self._isolation = isolation
        self._savepoint: Optional[str] = None
        self._outermost = False

    async def __aenter__(self) -> "Transaction":
        conn = self._conn

        if conn._txn_owner is asyncio.current_task():
            # Nested transaction from the owning task: use a savepoint.
            conn._txn_depth += 1
            self._savepoint = f"aiosqlite_savepoint_{conn._txn_depth}"
            try:
                await conn._dispatch(conn._conn.execute, f"SAVEPOINT {self._savepoint}")
            except BaseException:
                conn._txn_depth -= 1
                self._savepoint = None
                raise
            return self

        # Wait for any transaction owned by another task to finish, then
        # take ownership so other tasks' statements queue behind us.
        await conn._txn_acquire()
        self._outermost = True
        try:
            in_transaction = await conn._dispatch(lambda: conn._conn.in_transaction)
            if in_transaction:
                raise sqlite3.ProgrammingError(
                    "cannot start a transaction context: the connection "
                    "already has an active transaction that was started "
                    "outside of the context"
                )
            begin = "BEGIN" if self._isolation is None else f"BEGIN {self._isolation}"
            await conn._dispatch(conn._conn.execute, begin)
            conn._txn_depth = 1
        except BaseException:
            self._outermost = False
            conn._txn_release()
            raise
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        conn = self._conn
        try:
            if self._savepoint is not None:
                # Nested transaction: release or roll back the savepoint.
                conn._txn_depth -= 1
                try:
                    if exc_type is None:
                        await conn._dispatch(
                            conn._conn.execute,
                            f"RELEASE SAVEPOINT {self._savepoint}",
                        )
                    else:
                        await conn._execute_critical(
                            conn._conn.execute,
                            f"ROLLBACK TO SAVEPOINT {self._savepoint}",
                        )
                        await conn._execute_critical(
                            conn._conn.execute,
                            f"RELEASE SAVEPOINT {self._savepoint}",
                        )
                finally:
                    self._savepoint = None
                return

            # Outermost transaction: invalidate cursors created inside the
            # context so they cannot escape it, and close them on the
            # connection thread (best effort).
            raw_cursors = conn._invalidate_txn_cursors()
            if raw_cursors:

                def close_cursors() -> None:
                    for cursor in raw_cursors:
                        try:
                            cursor.close()
                        except Exception:  # noqa B036
                            pass

                try:
                    await conn._dispatch(close_cursors)
                except Exception:  # noqa B036
                    LOG.info("exception while closing transaction cursors")

            if exc_type is None:
                try:
                    await conn._dispatch(conn._conn.execute, "COMMIT")
                except Exception:  # noqa B036
                    # Commit failed; attempt to roll back so the connection
                    # is not left inside a transaction for waiting tasks.
                    try:
                        await conn._dispatch(conn._conn.execute, "ROLLBACK")
                    except Exception:  # noqa B036
                        pass
                    raise
            else:
                # Roll back to completion before releasing waiting tasks,
                # even if the owning task is being cancelled.
                await conn._execute_critical(conn._conn.execute, "ROLLBACK")
        finally:
            if self._outermost:
                self._outermost = False
                conn._txn_release()


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
