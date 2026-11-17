# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Core implementation of aiosqlite proxies
"""

import asyncio
import itertools
import logging
import sqlite3
import threading
from collections.abc import AsyncIterator, Generator, Iterable, Mapping
from functools import partial
from pathlib import Path
from queue import Empty, Queue, SimpleQueue
from threading import Thread
from typing import Any, Callable, Literal, Optional, Union
from warnings import warn

from .context import contextmanager
from .cursor import Cursor
from .diagnostics import DiagnosticEvent, DiagnosticsListener, freeze_labels

__all__ = ["connect", "Connection", "Cursor"]

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

_connection_ids = itertools.count(1)

# Worker-thread local holding the operation currently being executed, so that
# trace callbacks (which run on the worker thread) can be correlated with the
# operation that triggered them.  Context variables of the submitting task
# are not visible here, hence the explicit hand-off via the queue item.
_worker_state = threading.local()


class _Operation:
    """Identity and frozen metadata for a single queued operation."""

    __slots__ = ("id", "name", "labels", "sql", "parameters")

    _ids = itertools.count(1)

    def __init__(
        self,
        name: str,
        labels: Mapping[str, str],
        sql: Optional[str] = None,
        parameters: Any = None,
    ) -> None:
        self.id = next(self._ids)
        self.name = name
        self.labels = labels
        self.sql = sql
        self.parameters = parameters


class _Diagnostics:
    """
    Mutable diagnostics state shared between a connection and its worker.

    Kept as a separate object (not referencing the connection) so the worker
    thread does not keep the connection alive.
    """

    __slots__ = ("connection_id", "listener", "include_parameters")

    def __init__(
        self,
        listener: Optional[DiagnosticsListener],
        include_parameters: bool,
    ) -> None:
        self.connection_id = next(_connection_ids)
        self.listener = listener
        self.include_parameters = include_parameters


def _emit(
    diagnostics: _Diagnostics,
    kind: str,
    operation: Optional[_Operation],
    sql: Optional[str] = None,
    error: Optional[BaseException] = None,
) -> None:
    """
    Emit a diagnostic event to the configured listener, if any.

    Listener errors are logged and swallowed: observability must never
    interrupt the normal work queue.
    """
    listener = diagnostics.listener
    if listener is None:
        return

    event = DiagnosticEvent(
        kind=kind,
        connection_id=diagnostics.connection_id,
        operation_id=operation.id if operation is not None else 0,
        operation=operation.name if operation is not None else "",
        labels=operation.labels if operation is not None else freeze_labels(),
        sql=sql if sql is not None else (operation.sql if operation else None),
        parameters=(
            operation.parameters
            if (operation is not None and diagnostics.include_parameters)
            else None
        ),
        error=error,
    )
    try:
        listener(event)
    except Exception:  # noqa: BLE001
        LOG.exception("aiosqlite diagnostics listener raised; ignoring")


def _current_operation() -> Optional[_Operation]:
    """The operation currently executing on this (worker) thread, if any."""
    return getattr(_worker_state, "operation", None)


_TxQueue = SimpleQueue[
    tuple[Optional[asyncio.Future], Callable[[], Any], Optional[_Operation]]
]


def _connection_worker_thread(tx: _TxQueue, diagnostics: _Diagnostics):
    """
    Execute function calls on a separate thread.

    :meta private:
    """
    while True:
        # Continues running until all queue items are processed,
        # even after connection is closed (so we can finalize all
        # futures)

        future, function, operation = tx.get()
        _worker_state.operation = operation
        _emit(diagnostics, "started", operation)

        try:
            LOG.debug("executing %s", function)
            result = function()

            if future:
                future.get_loop().call_soon_threadsafe(set_result, future, result)
            LOG.debug("operation %s completed", function)
            _emit(diagnostics, "succeeded", operation)

            if result is _STOP_RUNNING_SENTINEL:
                break

        except BaseException as e:  # noqa B036
            LOG.debug("returning exception %s", e)
            _emit(diagnostics, "failed", operation, error=e)
            if future:
                future.get_loop().call_soon_threadsafe(set_exception, future, e)

        finally:
            _worker_state.operation = None


class Connection:
    def __init__(
        self,
        connector: Callable[[], sqlite3.Connection],
        iter_chunk_size: int,
        loop: Optional[asyncio.AbstractEventLoop] = None,
        diagnostics_listener: Optional[DiagnosticsListener] = None,
        diagnostics_include_parameters: bool = False,
    ) -> None:
        self._running = True
        self._connection: Optional[sqlite3.Connection] = None
        self._connector = connector
        self._diagnostics = _Diagnostics(
            diagnostics_listener, diagnostics_include_parameters
        )
        self._trace_handler: Optional[Callable[[str], Any]] = None
        self._tx: _TxQueue = SimpleQueue()
        self._iter_chunk_size = iter_chunk_size
        self._thread = Thread(
            target=_connection_worker_thread, args=(self._tx, self._diagnostics)
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

        self._tx.put_nowait((future, close_and_stop, self._new_operation("stop")))
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

    def _new_operation(
        self,
        name: str,
        labels: Optional[Mapping[str, str]] = None,
        sql: Optional[str] = None,
        parameters: Any = None,
    ) -> _Operation:
        """
        Create an operation identity, freezing the labels *now* (in the
        submitting task) so later context changes cannot leak into this
        already-queued item.
        """
        return _Operation(
            name=name,
            labels=freeze_labels(labels),
            sql=sql,
            parameters=parameters,
        )

    async def _execute(
        self, fn, *args, _labels=None, _sql=None, _parameters=None, **kwargs
    ):
        """Queue a function with the given arguments for execution."""
        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        function = partial(fn, *args, **kwargs)
        future = asyncio.get_event_loop().create_future()

        name = getattr(fn, "__qualname__", None) or getattr(fn, "__name__", None)
        operation = self._new_operation(
            name=name or str(fn),
            labels=_labels,
            sql=_sql,
            parameters=_parameters,
        )
        self._tx.put_nowait((future, function, operation))
        _emit(self._diagnostics, "queued", operation)

        return await future

    async def _connect(self) -> "Connection":
        """Connect to the actual sqlite database."""
        if self._connection is None:
            try:
                future = asyncio.get_event_loop().create_future()
                self._tx.put_nowait(
                    (future, self._connector, self._new_operation("connect"))
                )
                self._connection = await future
            except BaseException:
                self.stop()
                self._connection = None
                raise

            if self._diagnostics.listener is not None or self._trace_handler:
                await self._execute(self._install_trace_callback)

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

    async def commit(self, *, labels: Optional[Mapping[str, str]] = None) -> None:
        """Commit the current transaction."""
        await self._execute(self._conn.commit, _labels=labels)

    async def rollback(self, *, labels: Optional[Mapping[str, str]] = None) -> None:
        """Roll back the current transaction."""
        await self._execute(self._conn.rollback, _labels=labels)

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
        self,
        sql: str,
        parameters: Optional[Iterable[Any]] = None,
        *,
        labels: Optional[Mapping[str, str]] = None,
    ) -> Cursor:
        """Helper to create a cursor and execute the given query."""
        if parameters is None:
            parameters = []
        cursor = await self._execute(
            self._conn.execute,
            sql,
            parameters,
            _labels=labels,
            _sql=sql,
            _parameters=parameters,
        )
        return Cursor(self, cursor)

    @contextmanager
    async def execute_insert(
        self,
        sql: str,
        parameters: Optional[Iterable[Any]] = None,
        *,
        labels: Optional[Mapping[str, str]] = None,
    ) -> Optional[sqlite3.Row]:
        """Helper to insert and get the last_insert_rowid."""
        if parameters is None:
            parameters = []
        return await self._execute(
            self._execute_insert,
            sql,
            parameters,
            _labels=labels,
            _sql=sql,
            _parameters=parameters,
        )

    @contextmanager
    async def execute_fetchall(
        self,
        sql: str,
        parameters: Optional[Iterable[Any]] = None,
        *,
        labels: Optional[Mapping[str, str]] = None,
    ) -> Iterable[sqlite3.Row]:
        """Helper to execute a query and return all the data."""
        if parameters is None:
            parameters = []
        return await self._execute(
            self._execute_fetchall,
            sql,
            parameters,
            _labels=labels,
            _sql=sql,
            _parameters=parameters,
        )

    @contextmanager
    async def executemany(
        self,
        sql: str,
        parameters: Iterable[Iterable[Any]],
        *,
        labels: Optional[Mapping[str, str]] = None,
    ) -> Cursor:
        """Helper to create a cursor and execute the given multiquery."""
        cursor = await self._execute(
            self._conn.executemany,
            sql,
            parameters,
            _labels=labels,
            _sql=sql,
            _parameters=parameters,
        )
        return Cursor(self, cursor)

    @contextmanager
    async def executescript(
        self,
        sql_script: str,
        *,
        labels: Optional[Mapping[str, str]] = None,
    ) -> Cursor:
        """Helper to create a cursor and execute a user script."""
        cursor = await self._execute(
            self._conn.executescript,
            sql_script,
            _labels=labels,
            _sql=sql_script,
        )
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

    async def set_diagnostics_listener(
        self, listener: Optional[DiagnosticsListener]
    ) -> None:
        """
        Set (or remove, with ``None``) a listener receiving
        :class:`aiosqlite.DiagnosticEvent` objects for every operation.

        The listener may be invoked from the event loop thread (``queued``
        events) or from the connection worker thread (all other events).
        Exceptions raised by the listener are logged and ignored so that
        observability can never interrupt the work queue.
        """
        self._diagnostics.listener = listener
        if self._connection is not None:
            await self._execute(self._install_trace_callback)

    def _install_trace_callback(self) -> None:
        """
        Install the sqlite trace callback wrapper on the real connection.

        Runs on the worker thread.  The wrapper emits ``trace`` diagnostic
        events correlated with the operation currently being executed, then
        forwards to the user's trace callback (if any).  Errors raised by the
        user's callback are recorded as ``callback_error`` events, separate
        from query failures, and are never re-raised into the work queue.
        """
        if self._trace_handler is None and self._diagnostics.listener is None:
            self._conn.set_trace_callback(None)
            return

        def traced(sql: str) -> None:
            operation = _current_operation()
            _emit(self._diagnostics, "trace", operation, sql=sql)

            handler = self._trace_handler
            if handler is not None:
                try:
                    handler(sql)
                except Exception as e:  # noqa: BLE001
                    LOG.exception("aiosqlite trace callback raised")
                    _emit(
                        self._diagnostics,
                        "callback_error",
                        operation,
                        sql=sql,
                        error=e,
                    )

        self._conn.set_trace_callback(traced)

    async def set_trace_callback(self, handler: Optional[Callable[[str], Any]]) -> None:
        """
        Set a trace callback invoked with the text of each SQL statement
        actually executed, as it runs on the worker thread.

        The callback is wrapped so that statement text is also correlated
        with the enqueued operation's labels in diagnostic events, and so
        that errors raised by the callback are recorded separately instead
        of disrupting the work queue.
        """
        self._trace_handler = handler
        await self._execute(self._install_trace_callback)

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
    diagnostics_listener: Optional[DiagnosticsListener] = None,
    diagnostics_include_parameters: bool = False,
    **kwargs: Any,
) -> Connection:
    """
    Create and return a connection proxy to the sqlite database.

    :param diagnostics_listener: Optional callable receiving
        :class:`aiosqlite.DiagnosticEvent` objects for each operation.
    :param diagnostics_include_parameters: If true, diagnostic events include
        bound parameter values.  Defaults to false so parameter values are
        never emitted inadvertently.
    """

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

    return Connection(
        connector,
        iter_chunk_size,
        diagnostics_listener=diagnostics_listener,
        diagnostics_include_parameters=diagnostics_include_parameters,
    )
