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
_TxQueue = SimpleQueue[tuple[Optional[asyncio.Future], Callable[[], Any]]]


def _deliver(
    fut: Optional[asyncio.Future],
    result: Any = None,
    exception: Optional[BaseException] = None,
) -> None:
    """Deliver a result or exception to a future from any thread.

    Failures to schedule the delivery (for example because the future's event
    loop is already closed) are swallowed: the waiter is gone and the worker
    thread must never be prevented from terminating because of it.
    """
    if fut is None:
        return

    try:
        loop = fut.get_loop()
        if exception is None:
            loop.call_soon_threadsafe(set_result, fut, result)
        else:
            loop.call_soon_threadsafe(set_exception, fut, exception)
    except Exception:
        LOG.debug("unable to deliver result: event loop is unavailable")


def _resolve(waiter: asyncio.Future, exception: Optional[BaseException]) -> None:
    """Deliver the outcome of a shutdown to a single waiter."""
    _deliver(waiter, result=None, exception=exception)


class _ConnectionState:
    """State shared between a :class:`Connection` and its worker thread.

    The worker thread only ever receives this object (never the ``Connection``
    itself), so a live worker cannot keep the connection from being garbage
    collected and triggering ``Connection.__del__``.

    A single lock protects the connection reference, the "accepting new work"
    flag, the connect handshake, and the one-shot shutdown bookkeeping, so that
    accepting an operation and starting the shutdown cannot interleave.
    """

    def __init__(self) -> None:
        self.lock = Lock()
        self.connection: Optional[sqlite3.Connection] = None
        self.running = True
        self.started = False
        self.closing = False
        self.close_enqueued = False
        self.close_complete = False
        self.close_exception: Optional[BaseException] = None
        self.close_waiters: list[asyncio.Future] = []

    def add_waiter(self, waiter: Optional[asyncio.Future]) -> bool:
        """Register a shutdown waiter; return True if already complete.

        Must be called with :attr:`lock` held.
        """
        if waiter is None:
            return self.close_complete
        if self.close_complete:
            return True
        self.close_waiters.append(waiter)
        return False

    def notify(self) -> None:
        """Resolve every pending waiter with the recorded outcome, once."""
        with self.lock:
            self.close_complete = True
            waiters, self.close_waiters = self.close_waiters, []
            exception = self.close_exception

        for waiter in waiters:
            _resolve(waiter, exception)


def _connect_worker(state: _ConnectionState, connector: Callable[[], Any]) -> Any:
    """Open the underlying connection on the worker thread.

    The connection is published while holding the lock, so a shutdown queued
    behind this job is guaranteed to see it and close it.
    """
    connection = connector()
    with state.lock:
        state.connection = connection
    return connection


def _close_worker(state: _ConnectionState) -> object:
    """Close the connection and ask the worker thread to stop.

    Runs on the worker thread, after every previously accepted operation has
    completed, so ordering is preserved. Any error raised by the underlying
    ``close()`` is recorded and later delivered to every waiter instead of
    escaping, so the worker thread always terminates.
    """
    with state.lock:
        connection = state.connection
        state.connection = None

    if connection is not None:
        try:
            connection.close()
        except BaseException as e:  # noqa B036
            LOG.info("exception occurred while closing connection")
            with state.lock:
                state.close_exception = e

    return _STOP_RUNNING_SENTINEL


def _connection_worker_thread(tx: _TxQueue, state: _ConnectionState):
    """
    Execute function calls on a separate thread.

    The worker always notifies ``state`` when it terminates, so that shutdown
    waiters can never hang, even if the worker exits unexpectedly.

    :meta private:
    """
    try:
        while True:
            # Continues running until all queue items are processed,
            # even after connection is closed (so we can finalize all
            # futures)

            future, function = tx.get()
            stop = False
            result: Any = None

            try:
                LOG.debug("executing %s", function)
                result = function()
                stop = result is _STOP_RUNNING_SENTINEL
                LOG.debug("operation %s completed", function)

            except BaseException as e:  # noqa B036
                LOG.debug("returning exception %s", e)
                _deliver(future, exception=e)

            else:
                _deliver(future, result=result)

            finally:
                # Don't keep the callable, its future, or its result alive
                # while idle: any of them may reference the connection, which
                # would prevent ``Connection.__del__`` from ever running.
                del future, function, result

            # Decided outside the try/except above, so a failure to notify a
            # waiter can never leave the worker thread running forever.
            if stop:
                break
    finally:
        state.notify()


class Connection:
    def __init__(
        self,
        connector: Callable[[], sqlite3.Connection],
        iter_chunk_size: int,
        loop: Optional[asyncio.AbstractEventLoop] = None,
    ) -> None:
        self._state = _ConnectionState()
        self._connector = connector
        self._tx: _TxQueue = SimpleQueue()
        self._iter_chunk_size = iter_chunk_size
        self._thread = Thread(
            target=_connection_worker_thread,
            args=(self._tx, self._state),
        )

        if loop is not None:
            warn(
                "aiosqlite.Connection no longer uses the `loop` parameter",
                DeprecationWarning,
            )

    @property
    def _connection(self) -> Optional[sqlite3.Connection]:
        return self._state.connection

    @property
    def _running(self) -> bool:
        return self._state.running

    def __del__(self):
        state = getattr(self, "_state", None)
        if state is None or state.connection is None:
            return

        if state.closing:
            # A shutdown is already in progress and will close the connection;
            # the caller did ask for it to be closed, so don't complain.
            return

        warn(
            (
                f"{self!r} was deleted before being closed. "
                "Please use 'async with' or '.close()' to close the connection properly."
            ),
            ResourceWarning,
            stacklevel=1,
        )

        # Don't try to be creative here, the event loop may have already been
        # closed, so there may be no one to await the shutdown. `stop()` still
        # closes the underlying connection (if needed) and stops the worker
        # thread without depending on a running loop.
        self.stop()

    def stop(self) -> Optional[asyncio.Future]:
        """Stop the background thread and close the connection if still open.

        This is the synchronous counterpart of :meth:`close`, intended for
        cleanup paths that run without a usable event loop. Prefer
        ``async with`` or ``await close()``.

        Returns a future that resolves once the connection is closed and the
        worker thread has finished, or ``None`` if no event loop is available
        to await on. If the connection was already closed, the returned future
        carries the outcome of that (single) close operation.
        """
        return self._begin_close()

    def _new_waiter(self) -> Optional[asyncio.Future]:
        """Create a future tied to the caller's event loop, if there is one.

        Only an already-running loop is used. Cleanup paths such as ``__del__``
        or ``stop()`` may run without any loop (or after the loop was closed);
        in that case there is nothing to await on and ``None`` is returned
        rather than creating a spurious event loop.
        """
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return None

        try:
            waiter = loop.create_future()
        except Exception:
            return None

        # Cleanup paths often discard the returned future. Mark its exception
        # as retrieved so that a failed close does not turn into an "exception
        # was never retrieved" warning; awaiting the future still raises.
        def _consume(fut: asyncio.Future) -> None:
            try:
                if not fut.cancelled():
                    fut.exception()
            except BaseException:  # noqa B036
                pass

        try:
            waiter.add_done_callback(_consume)
        except RuntimeError:
            pass

        return waiter

    def _begin_close(self) -> Optional[asyncio.Future]:
        """Start the single, shared shutdown sequence for this connection.

        The first caller enqueues the close-and-stop job; every caller (first
        or not) gets its own waiter, so canceling one waiter can neither
        cancel the shutdown nor affect the other waiters. New work is refused
        from this point on, while already accepted work still runs to
        completion, in order, before the connection is closed.

        The waiter resolves only once the worker thread has actually finished,
        and it resolves with the error raised by the underlying ``close()``, if
        any, rather than reporting an immediate success.
        """
        waiter = self._new_waiter()
        state = self._state
        enqueue = False
        completed = False
        exception: Optional[BaseException] = None

        with state.lock:
            # Stop accepting new work immediately, before anything else.
            state.running = False
            state.closing = True

            if state.started and not state.close_enqueued:
                state.close_enqueued = True
                enqueue = True
            elif not state.started:
                # Nothing was ever started: no worker thread, no connection,
                # and nothing to stop, so the shutdown is already complete.
                state.close_complete = True

            completed = state.add_waiter(waiter)
            if completed:
                exception = state.close_exception

            if enqueue:
                # Enqueued while holding the same lock that gates accepting new
                # work, so it lands after every operation accepted so far.
                self._tx.put_nowait((None, partial(_close_worker, state)))

        if completed and waiter is not None:
            # Report the real terminal state, including a failure: a repeat
            # close must not be mistaken for an immediate success.
            _resolve(waiter, exception)

        return waiter

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
        """Queue a function with the given arguments for execution.

        Accepting the work and starting the shutdown are serialized by the same
        lock, so an operation either lands in the queue before the close job
        (and therefore runs, in order, before the connection is closed) or is
        rejected outright once closing has begun.
        """
        state = self._state
        function = partial(fn, *args, **kwargs)
        future = asyncio.get_event_loop().create_future()

        with state.lock:
            if not state.running or state.closing or state.connection is None:
                raise ValueError("Connection closed")
            self._tx.put_nowait((future, function))

        return await future

    async def _connect(self) -> "Connection":
        """Connect to the actual sqlite database."""
        state = self._state
        if state.connection is None:
            try:
                future = asyncio.get_event_loop().create_future()
                with state.lock:
                    if state.closing:
                        raise ValueError("Connection closed")
                    self._tx.put_nowait(
                        (future, partial(_connect_worker, state, self._connector))
                    )
                await future
            except BaseException:
                # Ensure the worker thread does not linger, even if this task
                # was cancelled. A close job queued behind a still-running
                # connect job will pick up and close the connection it made.
                self.stop()
                raise

        return self

    def __await__(self) -> Generator[Any, None, "Connection"]:
        state = self._state
        with state.lock:
            if state.closing:
                raise ValueError("Connection closed")
            if not state.started:
                self._thread.start()
                state.started = True
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
        """Complete queued queries/cursors and close the connection.

        Safe to call more than once and concurrently. The underlying
        connection is closed and the worker thread stopped exactly once, by
        whichever call starts the shutdown first; every other call waits on
        that same shutdown and observes its real outcome. Canceling one waiter
        does not cancel the shutdown or disturb the other waiters.

        Any error raised by the underlying ``close()`` is propagated to all
        waiters; it is never hidden by a later call returning early.
        """
        future = self._begin_close()
        if future is not None:
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
