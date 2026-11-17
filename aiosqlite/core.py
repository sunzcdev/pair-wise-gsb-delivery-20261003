# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Core implementation of aiosqlite proxies
"""

import asyncio
import logging
import sqlite3
from collections.abc import AsyncIterator, Generator, Iterable
from dataclasses import dataclass
from functools import partial
from itertools import count
from pathlib import Path
from queue import Empty, Queue, SimpleQueue
from threading import Thread, current_thread, get_ident
from typing import Any, Callable, Literal, Optional, Union
from warnings import warn

from .context import contextmanager
from .cursor import Cursor

__all__ = [
    "connect",
    "Connection",
    "Cursor",
    "UDFFailure",
    "UDFReturnValueError",
    "UDF_PHASE_CALL",
    "UDF_PHASE_RETURN",
]

AuthorizerCallback = Callable[[int, str, str, str, str], int]

LOG = logging.getLogger("aiosqlite")


IsolationLevel = Optional[Literal["DEFERRED", "IMMEDIATE", "EXCLUSIVE"]]

#: Phase reported when the Python callback itself raised an exception.
UDF_PHASE_CALL = "call"

#: Phase reported when the callback returned a value that SQLite cannot store.
UDF_PHASE_RETURN = "return"

_CONNECTION_IDS = count(1)


def set_result(fut: asyncio.Future, result: Any) -> None:
    """Set the result of a future if it hasn't been set already."""
    if not fut.done():
        fut.set_result(result)


def set_exception(fut: asyncio.Future, e: BaseException) -> None:
    """Set the exception of a future if it hasn't been set already."""
    if not fut.done():
        fut.set_exception(e)


class UDFReturnValueError(TypeError):
    """
    Describes a callback result that SQLite refuses to store.

    This is reported as :data:`UDF_PHASE_RETURN` and is always distinct from an
    exception raised by the callback itself (:data:`UDF_PHASE_CALL`).  The
    original return value is never retained, only its type name.
    """

    def __init__(self, function: str, returned_type: str, reason: str) -> None:
        self.function = function
        self.returned_type = returned_type
        self.reason = reason
        super().__init__(
            f"user-defined function {function!r} returned {reason} ({returned_type})"
        )


def _describe_argument(value: Any) -> str:
    """Render a single callback argument with a bounded, reference-free size."""
    kind = type(value).__name__

    if isinstance(value, (bytes, bytearray, memoryview)):
        size = getattr(value, "nbytes", len(value))
        return f"<{kind} of {size} bytes>"

    if isinstance(value, str):
        if len(value) > 80:
            return f"<{kind} of {len(value)} chars: {value[:60]!r}...>"
        return repr(value)

    try:
        text = repr(value)
    except BaseException:  # noqa B036 - hostile __repr__
        return f"<unrepresentable {kind}>"

    if len(text) > 80:
        text = text[:77] + "..."
    return text


def _summarize_arguments(args: tuple) -> tuple:
    """Render callback arguments for diagnostics without keeping references."""
    summary = [_describe_argument(value) for value in args[:8]]

    if len(args) > 8:
        summary.append(f"<{len(args) - 8} more arguments>")

    return tuple(summary)


@dataclass(frozen=True)
class UDFFailure:
    """
    A single Python-level failure observed while running a registered callback.

    Records are attached to the exception that SQLite raises for the failing
    operation, so the cause survives the trip from the connection worker thread
    back to the event loop.  User supplied argument values are only included
    when diagnostics are explicitly configured to capture them.
    """

    function: str
    """Name the callback was registered under."""

    phase: str
    """Either :data:`UDF_PHASE_CALL` or :data:`UDF_PHASE_RETURN`."""

    operation: Optional[str]
    """Logical operation that was in flight, e.g. ``"execute"``."""

    exception: BaseException
    """Exception raised by the callback, or :class:`UDFReturnValueError`."""

    thread_id: int
    """Identifier of the worker thread that ran the callback."""

    thread_name: str
    """Name of the worker thread that ran the callback."""

    connection_id: int
    """Identifier of the connection the callback was registered on."""

    sequence: int
    """Monotonic failure counter local to the connection."""

    kind: str = "function"
    """Kind of registered callback: ``"function"``, ``"authorizer"``, ..."""

    callable_name: Optional[str] = None
    """``__name__`` of the Python callable, when it has one."""

    arguments: Optional[tuple] = None
    """Rendered callback arguments, or ``None`` when capture is disabled."""

    sql: Optional[str] = None
    """Statement text, or ``None`` when capture is disabled."""

    returned_type: Optional[str] = None
    """Type name of a rejected return value, for ``UDF_PHASE_RETURN``."""

    @property
    def function_name(self) -> str:
        """Alias for :attr:`function`."""
        return self.function

    @property
    def cause(self) -> BaseException:
        """Alias for :attr:`exception`."""
        return self.exception

    def __str__(self) -> str:
        label = "user-defined function" if self.kind == "function" else self.kind
        name = f" {self.function!r}" if self.function != self.kind else ""

        if self.phase == UDF_PHASE_RETURN:
            detail = f"returned rejected value of type {self.returned_type}"
        else:
            detail = f"raised {type(self.exception).__name__}: {self.exception}"

        where = f" during {self.operation}" if self.operation else ""
        return (
            f"{label}{name} {detail}{where} "
            f"(connection {self.connection_id}, thread {self.thread_id})"
        )


class _UDFDiagnosticSink:
    """
    Per-connection collector for callback failures of the current operation.

    Only the worker thread records into the sink, and every queued operation
    starts by clearing it, so a failure can never be attributed to a later
    statement.

    :meta private:
    """

    def __init__(self, connection_id: int) -> None:
        self.connection_id = connection_id
        self.enabled = False
        self.capture_values = False
        self.capture_sql = False
        self.last_failures: tuple = ()
        self._failures: list = []
        self._operation: Optional[str] = None
        self._sql: Optional[str] = None
        self._sequence = 0

    def set_defaults(
        self,
        enabled: bool,
        capture_values: Optional[bool] = None,
        capture_sql: Optional[bool] = None,
    ) -> None:
        self.enabled = enabled
        if capture_values is not None:
            self.capture_values = capture_values
        if capture_sql is not None:
            self.capture_sql = capture_sql

    def begin(self, operation: Optional[str], sql: Optional[str] = None) -> None:
        """Reset per-operation state on the connection worker."""
        self._failures.clear()
        self.last_failures = ()
        self._operation = operation
        self._sql = sql

    def finish(self) -> tuple:
        """
        Close out a successful operation.

        SQLite swallows some callback errors outright (trace callbacks in
        particular).  Those records are kept in :attr:`last_failures` so
        callers can still inspect them, but they are never attached to an
        exception that did not happen.  The tuple is replaced on the next
        operation, so retention is bounded to one statement.
        """
        failures = tuple(self._failures)
        self._failures.clear()
        if failures:
            self.last_failures = failures
            LOG.debug("callback diagnostics recorded without a failure: %s", failures)
        return failures

    def record(
        self,
        function: str,
        phase: str,
        exception: BaseException,
        arguments: Optional[tuple] = None,
        returned_type: Optional[str] = None,
        kind: str = "function",
        callable_name: Optional[str] = None,
    ) -> None:
        self._sequence += 1
        self._failures.append(
            UDFFailure(
                function=function,
                phase=phase,
                operation=self._operation,
                exception=exception,
                thread_id=get_ident(),
                thread_name=current_thread().name,
                connection_id=self.connection_id,
                sequence=self._sequence,
                kind=kind,
                callable_name=callable_name,
                arguments=arguments,
                sql=self._sql if self.capture_sql else None,
                returned_type=returned_type,
            )
        )

    def take(self) -> tuple:
        """Return and clear the failures recorded for the current operation."""
        failures = tuple(self._failures)
        self._failures.clear()
        self.last_failures = failures
        return failures


def _supported_return(value: Any) -> Optional[str]:
    """
    Return ``None`` for values SQLite can store, otherwise a short reason.

    The check mirrors what ``sqlite3`` itself accepts from a callback and is
    deliberately permissive: anything exposing a contiguous buffer is treated
    as valid.  It never invokes the callback again, and it never converts or
    copies the value.
    """
    if value is None or isinstance(value, (bool, float, str, bytes, bytearray)):
        return None

    if isinstance(value, int):
        if -(2**63) <= value < 2**63:
            return None
        return "integer out of range of SQLite INTEGER"

    try:
        view = memoryview(value)
    except BaseException:  # noqa B036 - exotic buffer providers
        return "unsupported type"

    if not view.contiguous:
        return "non-contiguous buffer"
    return None


def _validate_authorizer_return(value: Any) -> Optional[str]:
    """Authorizer callbacks must answer with SQLITE_OK, DENY, or IGNORE."""
    if isinstance(value, int) and value in (
        sqlite3.SQLITE_OK,
        sqlite3.SQLITE_DENY,
        sqlite3.SQLITE_IGNORE,
    ):
        return None
    return "a value outside SQLITE_OK / SQLITE_DENY / SQLITE_IGNORE"


def _diagnostic_wrapper(
    sink: _UDFDiagnosticSink,
    name: str,
    func: Callable,
    diagnostics: Optional[bool],
    capture_values: Optional[bool],
    kind: str = "function",
    validate: Optional[Callable[[Any], Optional[str]]] = _supported_return,
) -> Callable:
    """
    Wrap ``func`` so failures are recorded, then reported exactly as before.

    The wrapper never retries, never inspects SQL text, never touches the
    database, and never changes the value handed back to SQLite.  When
    diagnostics are inactive it is a single branch away from the original
    callable.
    """

    force = diagnostics is True or capture_values is True
    never = diagnostics is False
    callable_name = getattr(func, "__name__", None)

    def wrapper(*args, **kwargs):
        if never or not (force or sink.enabled):
            return func(*args, **kwargs)

        capture = capture_values if capture_values is not None else sink.capture_values

        try:
            result = func(*args, **kwargs)
        except BaseException as e:  # noqa B036
            arguments = _summarize_arguments(args) if capture else None
            sink.record(
                name,
                UDF_PHASE_CALL,
                e,
                arguments=arguments,
                kind=kind,
                callable_name=callable_name,
            )
            raise

        if validate is not None:
            reason = validate(result)
            if reason is not None:
                returned_type = type(result).__name__
                arguments = _summarize_arguments(args) if capture else None
                sink.record(
                    name,
                    UDF_PHASE_RETURN,
                    UDFReturnValueError(name, returned_type, reason),
                    arguments=arguments,
                    returned_type=returned_type,
                    kind=kind,
                    callable_name=callable_name,
                )

        return result

    # Keep the callback looking like itself for tracing and introspection.
    for attribute in ("__name__", "__qualname__", "__module__", "__doc__"):
        try:
            setattr(wrapper, attribute, getattr(func, attribute))
        except (AttributeError, TypeError):  # pragma: no cover - exotic callables
            pass
    try:
        wrapper.__wrapped__ = func  # type: ignore[attr-defined]
    except (AttributeError, TypeError):  # pragma: no cover
        pass

    return wrapper


def _primary_failure(failures: tuple) -> UDFFailure:
    """
    Pick the record that best explains the raise for the whole operation.

    Trace callbacks run first and SQLite ignores their exceptions, so a later
    user-defined function failure should win over an earlier trace failure.
    """
    for failure in failures:
        if failure.kind != "trace_callback":
            return failure
    return failures[0]


def _annotate_exception(exc: BaseException, failures: tuple) -> None:
    """
    Attach callback diagnostics to the exception raised for the operation.

    The exception keeps its original type, so ``except sqlite3.Error`` works
    exactly as before.  The most relevant callback failure becomes the
    ``__cause__``, giving a Python traceback that points at the callback rather
    than only at SQLite.
    """
    if not isinstance(exc, sqlite3.Error):
        return

    primary = _primary_failure(failures)
    try:
        exc.udf_diagnostics = failures  # type: ignore[attr-defined]
        exc.udf_failure = primary  # type: ignore[attr-defined]

        note = f"aiosqlite: {primary}"
        if len(failures) > 1:
            note += f" (+{len(failures) - 1} other callback failures)"
        if hasattr(exc, "add_note"):
            exc.add_note(note)
        else:  # pragma: no cover - Python < 3.11
            LOG.debug("%s", note)

        if exc.__cause__ is None and exc.__context__ is None:
            exc.__cause__ = primary.exception
    except Exception:  # pragma: no cover - defensive
        LOG.debug("unable to attach UDF diagnostics", exc_info=True)


_STOP_RUNNING_SENTINEL = object()
_TxQueue = SimpleQueue[tuple[Optional[asyncio.Future], Callable[[], Any]]]


def _connection_worker_thread(
    tx: _TxQueue, diagnostics: Optional[_UDFDiagnosticSink] = None
):
    """
    Execute function calls on a separate thread.

    :meta private:
    """
    while True:
        # Continues running until all queue items are processed,
        # even after connection is closed (so we can finalize all
        # futures)

        future, function = tx.get()
        operation = getattr(function, "_aiosqlite_operation", None)
        sql = getattr(function, "_aiosqlite_sql", None)
        if diagnostics is not None:
            diagnostics.begin(operation, sql)

        try:
            LOG.debug("executing %s", function)
            result = function()

            if diagnostics is not None:
                diagnostics.finish()

            if future:
                future.get_loop().call_soon_threadsafe(set_result, future, result)
            LOG.debug("operation %s completed", function)

            if result is _STOP_RUNNING_SENTINEL:
                break

        except BaseException as e:  # noqa B036
            LOG.debug("returning exception %s", e)
            if diagnostics is not None:
                failures = diagnostics.take()
                if failures:
                    _annotate_exception(e, failures)
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
        self._connection_id = next(_CONNECTION_IDS)
        self._udf_sink = _UDFDiagnosticSink(self._connection_id)
        self._thread = Thread(
            target=_connection_worker_thread,
            args=(self._tx, self._udf_sink),
            name=f"aiosqlite-{self._connection_id}",
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

    async def _execute(
        self,
        fn,
        *args,
        _operation: Optional[str] = None,
        _sql: Optional[str] = None,
        **kwargs,
    ):
        """Queue a function with the given arguments for execution."""
        if not self._running or not self._connection:
            raise ValueError("Connection closed")

        function = partial(fn, *args, **kwargs)
        # Tag the queued call so callback failures can be tied to the operation
        # that was running on the worker thread when they happened.
        function._aiosqlite_operation = _operation  # type: ignore[attr-defined]
        function._aiosqlite_sql = _sql  # type: ignore[attr-defined]
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
        return Cursor(self, await self._execute(self._conn.cursor, _operation="cursor"))

    async def commit(self) -> None:
        """Commit the current transaction."""
        await self._execute(self._conn.commit, _operation="commit")

    async def rollback(self) -> None:
        """Roll back the current transaction."""
        await self._execute(self._conn.rollback, _operation="rollback")

    async def close(self) -> None:
        """Complete queued queries/cursors and close the connection."""

        if self._connection is None:
            return

        try:
            await self._execute(self._conn.close, _operation="close")
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
        cursor = await self._execute(
            self._conn.execute, sql, parameters, _operation="execute", _sql=sql
        )
        return Cursor(self, cursor)

    @contextmanager
    async def execute_insert(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> Optional[sqlite3.Row]:
        """Helper to insert and get the last_insert_rowid."""
        if parameters is None:
            parameters = []
        return await self._execute(
            self._execute_insert,
            sql,
            parameters,
            _operation="execute_insert",
            _sql=sql,
        )

    @contextmanager
    async def execute_fetchall(
        self, sql: str, parameters: Optional[Iterable[Any]] = None
    ) -> Iterable[sqlite3.Row]:
        """Helper to execute a query and return all the data."""
        if parameters is None:
            parameters = []
        return await self._execute(
            self._execute_fetchall,
            sql,
            parameters,
            _operation="execute_fetchall",
            _sql=sql,
        )

    @contextmanager
    async def executemany(
        self, sql: str, parameters: Iterable[Iterable[Any]]
    ) -> Cursor:
        """Helper to create a cursor and execute the given multiquery."""
        cursor = await self._execute(
            self._conn.executemany, sql, parameters, _operation="executemany", _sql=sql
        )
        return Cursor(self, cursor)

    @contextmanager
    async def executescript(self, sql_script: str) -> Cursor:
        """Helper to create a cursor and execute a user script."""
        cursor = await self._execute(
            self._conn.executescript,
            sql_script,
            _operation="executescript",
            _sql=sql_script,
        )
        return Cursor(self, cursor)

    async def interrupt(self) -> None:
        """Interrupt pending queries."""
        return self._conn.interrupt()

    async def create_function(
        self,
        name: str,
        num_params: int,
        func: Callable,
        deterministic: bool = False,
        *,
        diagnostics: Optional[bool] = None,
        capture_values: Optional[bool] = None,
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

        SQLite reports every callback failure as the same opaque
        ``OperationalError("user-defined function raised exception")``.  When
        diagnostics are active, that exception is annotated with the Python
        failure that caused it:

        - ``exc.udf_diagnostics`` is a tuple of :class:`UDFFailure` records, one
          per callback failure seen while the operation was running.
        - ``exc.udf_failure`` is the record that best explains the failure.
        - the callback exception becomes ``exc.__cause__``, so a normal
          traceback walks from the failing statement into the callback.

        The exception type is unchanged, so ``except sqlite3.Error`` keeps
        working.  Nothing about the statement, the transaction, or the number of
        callback invocations changes: the wrapper observes the failure and
        re-raises (or returns) exactly what it saw.

        Failures are split by phase: :data:`UDF_PHASE_CALL` means the callback
        raised, while :data:`UDF_PHASE_RETURN` means it returned a value SQLite
        cannot store.  In the latter case the recorded exception is a
        :class:`UDFReturnValueError`; the rejected value itself is discarded.
        Each queued operation starts with a clean slate, so a failure from one
        statement is never attached to the next.

        :param diagnostics: Force diagnostics on (``True``) or off (``False``)
            for this callback, overriding :meth:`set_function_diagnostics`.
            ``None`` defers to the connection-level setting.  Passing ``False``
            hands the original callable to SQLite untouched, so there is no
            wrapper overhead at all.
        :param capture_values: Force argument capture on or off for this
            callback.  ``None`` defers to the connection-level setting, which
            by default does not record user values.  Passing ``True`` implies
            ``diagnostics`` for this callback, since captured values are only
            meaningful on a failure record.
        """
        if diagnostics is False and capture_values:
            raise ValueError("capture_values=True requires diagnostics to be enabled")

        if callable(func) and diagnostics is not False:
            func = _diagnostic_wrapper(
                self._udf_sink, name, func, diagnostics, capture_values
            )

        await self._execute(
            self._conn.create_function,
            name,
            num_params,
            func,
            deterministic=deterministic,
            _operation="create_function",
        )

    async def set_function_diagnostics(
        self,
        enabled: bool = True,
        *,
        capture_values: Optional[bool] = None,
        capture_sql: Optional[bool] = None,
    ) -> None:
        """
        Enable or disable UDF failure diagnostics for this connection.

        Diagnostics are off by default so existing code sees no behavioral
        change.  Turning them on does not re-run any callback: the wrapper
        inspects values that were already produced.  The setting applies to
        callbacks registered through :meth:`create_function` that did not pin
        their own ``diagnostics`` or ``capture_values`` option.

        :param enabled: Whether callback failures should be recorded.
        :param capture_values: Whether rendered callback arguments should be
            included in :class:`UDFFailure` records.  Defaults to ``False`` so
            user data is never captured unless explicitly requested.
        :param capture_sql: Whether the statement text in flight should be
            included in :class:`UDFFailure` records.  Defaults to ``False``,
            since SQL often embeds literal user data.
        """
        self._udf_sink.set_defaults(enabled, capture_values, capture_sql)

    @property
    def last_udf_failures(self) -> tuple:
        """
        Callback failures recorded for the most recent completed operation.

        This mirrors what was attached to a raised exception, and lets code on
        the event loop inspect the worker-thread cause without holding on to the
        exception.  It also surfaces callback failures that SQLite swallowed
        itself, such as trace callback errors, which never fail the statement.
        It is empty until an operation with diagnostics enabled records one.

        When several operations are queued at once, the value reflects whichever
        operation the worker finished most recently.  The diagnostics attached
        to an exception are always specific to that operation, so prefer
        ``exc.udf_failure`` when an exception is available.
        """
        return self._udf_sink.last_failures

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
        """
        Set a progress handler called every ``n`` virtual machine instructions.

        When :meth:`set_function_diagnostics` is enabled, an exception raised by
        the handler is recorded like a user-defined function failure, and an
        out-of-range return value is reported as :data:`UDF_PHASE_RETURN`.
        """
        if callable(handler):
            handler = _diagnostic_wrapper(
                self._udf_sink,
                "progress_handler",
                handler,
                None,
                None,
                kind="progress_handler",
                validate=None,
            )
        await self._execute(
            self._conn.set_progress_handler,
            handler,
            n,
            _operation="set_progress_handler",
        )

    async def set_trace_callback(self, handler: Callable) -> None:
        """
        Set a callback invoked for each SQL statement as it starts running.

        Exceptions raised by the callback are recorded as
        ``kind="trace_callback"`` failures when diagnostics are enabled.
        """
        if callable(handler):
            handler = _diagnostic_wrapper(
                self._udf_sink,
                "trace_callback",
                handler,
                None,
                None,
                kind="trace_callback",
                validate=None,
            )
        await self._execute(
            self._conn.set_trace_callback, handler, _operation="set_trace_callback"
        )

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

        When :meth:`set_function_diagnostics` is enabled, an exception raised
        by the authorizer, or a return value outside the three accepted codes,
        is recorded with ``kind="authorizer"`` and attached to the resulting
        ``sqlite3`` error.
        """
        if callable(authorizer_callback):
            authorizer_callback = _diagnostic_wrapper(
                self._udf_sink,
                "authorizer",
                authorizer_callback,
                None,
                None,
                kind="authorizer",
                validate=_validate_authorizer_return,
            )
        await self._execute(
            self._conn.set_authorizer,
            authorizer_callback,
            _operation="set_authorizer",
        )

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

        fut = self._execute(dumper, _operation="iterdump")
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
            _operation="backup",
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
