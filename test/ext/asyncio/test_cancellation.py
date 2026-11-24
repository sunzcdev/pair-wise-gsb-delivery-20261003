"""Cancellation safety for the asyncio dialects.

An asyncio task may be cancelled at any ``await``.  These tests cancel a
simple, entirely reasonable piece of user code at each of its IO points in
turn and assert that the connection pool comes out of it intact.

See :mod:`sqlalchemy.testing.cancellation` for how the cancellation point
is made deterministic and for what "intact" means precisely.

"""

import asyncio

from sqlalchemy import event
from sqlalchemy import select
from sqlalchemy import testing
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy.ext.asyncio.base import _run_to_completion
from sqlalchemy.pool import AsyncAdaptedQueuePool
from sqlalchemy.pool.base import _finalize_fairy
from sqlalchemy.testing import async_test
from sqlalchemy.testing import cancellation
from sqlalchemy.testing import config
from sqlalchemy.testing import eq_
from sqlalchemy.testing import expect_raises
from sqlalchemy.testing import expect_warnings
from sqlalchemy.testing import fixtures
from sqlalchemy.testing import is_false
from sqlalchemy.testing import is_true


async def _session_execute_commit(async_engine):
    """A fixture that illustrates the session execute / commit pattern
    shown in discussion #13542."""

    async with AsyncSession(async_engine) as session:
        await session.execute(select(1))
        await session.commit()


async def _connect_execute(async_engine):
    """A fixture that illustrates the connection / execute pattern shown
    in issue #12710."""

    async with async_engine.connect() as conn:
        await conn.execute(select(1))


async def _connect_execute_except_exception(async_engine):
    """A fixture that illustrates a background task which treats an
    ordinary exception as something to retry, shown in issue #13678."""

    try:
        async with async_engine.connect() as conn:
            await conn.execute(select(1))
    except Exception:
        pass


async def _connect_close_then_reuse(async_engine):
    """A fixture that closes a connection directly, rather than through a
    context manager, and checks out another connection once that close()
    was cancelled, shown in issue #13699."""

    conn = await async_engine.connect()
    try:
        await conn.execute(select(1))
        await conn.commit()
    except asyncio.CancelledError:
        await conn.close()
        raise

    try:
        await conn.close()
    except asyncio.CancelledError:
        # the connection whose close() was cancelled reports itself
        # closed, so it can't act on the pool record that the new
        # checkout receives
        async with async_engine.connect() as conn2:
            if not conn.closed:
                await conn.invalidate()
            await conn2.execute(select(1))
        raise


class AsyncCancellationTest(fixtures.TestBase):
    __requires__ = ("async_dialect",)
    __backend__ = True

    @config.fixture()
    def engine_factory(self, async_testing_engine):
        # the pool class is pinned so that what these tests measure is the
        # dialect's behavior rather than whichever pool a given URL
        # defaults to; a StaticPool never releases its one connection and
        # so cannot exhibit these failures at all.  pool_timeout is given
        # explicitly and kept short as testing_engine() otherwise sets it
        # to zero, which a pool of size one cannot tolerate even when
        # nothing has gone wrong.
        return lambda: async_testing_engine(
            options={
                "poolclass": AsyncAdaptedQueuePool,
                "pool_size": 1,
                "max_overflow": 0,
                "pool_timeout": 1,
            }
        )

    @testing.crashes(
        "+aioodbc",
        "aioodbc abandons the cancelled pyodbc call in its executor "
        "thread; the next call on that connection frees the ODBC "
        "statement handle underneath it and msodbcsql segfaults",
    )
    @async_test
    async def test_cancel_anywhere(self, engine_factory):
        eq_(
            await cancellation.sweep(
                engine_factory, _session_execute_commit, warm=False
            ),
            [],
        )

    @testing.crashes(
        "+aioodbc",
        "aioodbc abandons the cancelled pyodbc call in its executor "
        "thread; the next call on that connection frees the ODBC "
        "statement handle underneath it and msodbcsql segfaults",
    )
    @testing.combinations((False,), (True,), argnames="warm")
    @async_test
    async def test_cancel_anywhere_connection(self, engine_factory, warm):
        """#12710"""
        eq_(
            await cancellation.sweep(
                engine_factory, _connect_execute, warm=warm
            ),
            [],
        )

    @testing.crashes(
        "+aioodbc",
        "aioodbc abandons the cancelled pyodbc call in its executor "
        "thread; the next call on that connection frees the ODBC "
        "statement handle underneath it and msodbcsql segfaults",
    )
    @testing.combinations((False,), (True,), argnames="warm")
    @async_test
    async def test_cancel_anywhere_direct_close(self, engine_factory, warm):
        """#13699"""
        eq_(
            await cancellation.sweep(
                engine_factory, _connect_close_then_reuse, warm=warm
            ),
            [],
        )

    @testing.crashes(
        "+aioodbc",
        "aioodbc abandons the cancelled pyodbc call in its executor "
        "thread; the next call on that connection frees the ODBC "
        "statement handle underneath it and msodbcsql segfaults",
    )
    @async_test
    async def test_cancel_anywhere_warm_pool(self, engine_factory):
        eq_(
            await cancellation.sweep(
                engine_factory, _session_execute_commit, warm=True
            ),
            [],
        )

    @testing.crashes(
        "+aioodbc",
        "aioodbc abandons the cancelled pyodbc call in its executor "
        "thread; the next call on that connection frees the ODBC "
        "statement handle underneath it and msodbcsql segfaults",
    )
    @async_test
    async def test_cancel_anywhere_connect_event_fails(self, engine_factory):
        """a connect event raising an ordinary exception has the new
        connection closed; a cancellation reaching that close is what the
        task ends with, not the exception.  #13678"""

        def failing_engine_factory():
            async_engine = engine_factory()

            @event.listens_for(async_engine.sync_engine, "connect")
            def connect(dbapi_connection, connection_record):
                raise RuntimeError("connect event failed")

            return async_engine

        eq_(
            await cancellation.sweep(
                failing_engine_factory, _connect_execute_except_exception
            ),
            [],
        )

    @testing.only_on(
        ["postgresql", "oracle"],
        "terminate of a connection whose close was cancelled is only "
        "robust on the PostgreSQL and Oracle async drivers; aiosqlite "
        "deadlocks waiting on its stopped worker thread",
    )
    @testing.combinations((1,), (2,), argnames="number_of_cancels")
    @async_test
    async def test_cancel_anywhere_recycle(
        self, engine_factory, number_of_cancels
    ):
        """checkout closes the invalidated connection before opening a new
        one; with two cancellations the second lands in the cleanup of the
        first."""

        eq_(
            await cancellation.sweep(
                engine_factory,
                _session_execute_commit,
                warm=True,
                recycle=True,
                cancels=number_of_cancels,
            ),
            [],
        )

    @testing.only_on(
        ["postgresql", "oracle"],
        "terminate of a connection whose close was cancelled is only "
        "robust on the PostgreSQL and Oracle async drivers; aiosqlite "
        "deadlocks waiting on its stopped worker thread",
    )
    @testing.combinations((False,), (True,), argnames="warm")
    @async_test
    async def test_cancel_anywhere_twice(self, engine_factory, warm):
        eq_(
            await cancellation.sweep(
                engine_factory, _session_execute_commit, warm=warm, cancels=2
            ),
            [],
        )

    @testing.fails_if(
        lambda config: not config.db.dialect.has_terminate,
        "dialect provides no AsyncAdapt_terminate; tracked separately",
    )
    @async_test
    async def test_gc_of_checked_out_connection(self, engine_factory):
        async_engine = engine_factory()
        with cancellation.connection_accounting(async_engine) as accounting:
            pool_connection = await async_engine.raw_connection()
            record = pool_connection._connection_record

            record.fairy_ref = lambda: None
            assert record.needs_gc

            # invoke the finalizer the way the garbage collector would,
            # rather than dropping the reference and collecting, so that
            # the warning below is raised somewhere it can be caught; a
            # warning raised from within a weakref callback is unraisable
            with expect_warnings(
                "The garbage collector is trying to clean up.*"
            ):
                _finalize_fairy(
                    None,
                    record,
                    pool_connection._pool,
                    False,
                    transaction_was_reset=False,
                    is_gc_cleanup=True,
                )

            eq_(accounting.leaked, set())

        await async_engine.dispose()

    @testing.fails_on(
        ["+psycopg", "+oracledb<26", "+aioodbc"],
        "dialect has not been given AsyncAdapt_terminate; tracked separately",
    )
    def test_dialect_supports_terminate(self):
        is_true(config.db.dialect.has_terminate)


class RunToCompletionTest(fixtures.TestBase):
    """cleanup run by _run_to_completion() is waited for even when the
    calling task is cancelled.  #12710"""

    __requires__ = ("asyncio",)

    async def _run(self, number_of_cancels):
        started = asyncio.Event()
        release = asyncio.Event()
        completed = []

        async def cleanup():
            started.set()
            await release.wait()
            completed.append(True)
            return "result"

        async def caller():
            return await _run_to_completion(cleanup())

        task = asyncio.create_task(caller())
        await started.wait()
        for _ in range(number_of_cancels):
            task.cancel()
            await asyncio.sleep(0.01)

        # the caller has not returned while its cleanup is still running
        is_true(not task.done())

        release.set()
        return task, completed

    @async_test
    async def test_not_cancelled(self):
        task, completed = await self._run(0)
        eq_(await task, "result")
        eq_(completed, [True])

    @testing.combinations((1,), (2,), argnames="number_of_cancels")
    @async_test
    async def test_cancelled(self, number_of_cancels):
        task, completed = await self._run(number_of_cancels)
        with expect_raises(asyncio.CancelledError, check_context=False):
            await task
        eq_(completed, [True])

    @async_test
    async def test_cleanup_raises(self):
        async def cleanup():
            raise ValueError("cleanup failed")

        with expect_raises(ValueError):
            await _run_to_completion(cleanup())


class _StubCursor:
    """A DBAPI cursor for a local stand-in of the aiosqlite driver.

    Its ``close()`` can be held open by the test once it has started, which
    is what makes the point at which the calling task is cancelled while a
    result close is in progress reproducible.

    """

    description = [("x", None, None, None, None, None, None)]

    def __init__(self, connection, server_side):
        self._connection = connection
        self.server_side = server_side
        self.closed = False
        self.rowcount = -1
        self.arraysize = 1
        self.lastrowid = 1
        self._rows = []
        self._fetches = 0
        self.is_query = False

    async def __aenter__(self):
        return self

    async def __aexit__(self, *arg):
        await self.close()

    async def execute(self, operation, parameters=None):
        if "read_uncommitted" in operation:
            # the dialect's own startup query, not the one under test
            self._rows = [(0,)]
        else:
            self._rows = [(1,), (2,), (3,)]
            self.is_query = True
        return self

    async def executemany(self, operation, parameters):
        return self

    async def fetchone(self):
        self._fetches += 1
        if self._rows:
            return self._rows.pop(0)
        return None

    async def fetchmany(self, size=1):
        rows, self._rows = self._rows[:size], self._rows[size:]
        return rows

    async def fetchall(self):
        rows, self._rows = self._rows, []
        return rows

    async def setinputsizes(self, sizes):
        pass

    def setoutputsize(self, size, column):
        pass

    async def nextset(self):
        return None

    def __aiter__(self):
        return self

    async def __anext__(self):
        row = await self.fetchone()
        if row is None:
            raise StopAsyncIteration
        return row

    async def close(self):
        # the close of the cursor that ran the statement under test is the
        # one the application's task awaits, and so the one the test
        # observes and can hold open; the dialect's startup query has its
        # own cursor, which is closed during connection setup
        if self.is_query:
            connection = self._connection
            connection.closes_started.append(self)
            if connection.close_gate is not None:
                await connection.close_gate.wait()
            if connection.fail_close:
                # a driver whose close never completed, as aiosqlite's does
                # when its worker thread has stopped
                raise asyncio.CancelledError()
            connection.closes_completed.append(self)
        self.closed = True


class _StubConnection:
    """A connection for the local stand-in of the aiosqlite driver."""

    def __init__(self):
        self.isolation_level = ""
        # aiosqlite's adapted connection reaches through to the driver's
        # own connection object, which for this stand-in is itself
        self._connection = self
        self.closes_started = []
        self.closes_completed = []
        self.close_gate = None
        self.fail_close = False
        self.closed = False
        self.stopped = False

    def cursor(self, server_side=False):
        return _StubCursor(self, server_side)

    async def commit(self):
        pass

    async def rollback(self):
        pass

    async def create_function(self, *arg, **kw):
        pass

    async def close(self):
        self.closed = True

    def stop(self):
        self.stopped = True


class _StubConnectHandle:
    def __init__(self, connection):
        self._connection = connection
        self._thread = type("_Thread", (), {"daemon": False})()

    def __await__(self):
        async def go():
            return self._connection

        return go().__await__()


def _stub_aiosqlite_dbapi():
    """Build an ``aiosqlite`` DBAPI whose every connection is a local
    stand-in, so that a close of a database cursor can be held open at
    will.  Returns the DBAPI and the list of connections it created."""

    import sqlite3
    import types

    from sqlalchemy.dialects.sqlite.aiosqlite import (
        AsyncAdapt_aiosqlite_dbapi,
    )

    aiosqlite = types.ModuleType("stub_aiosqlite")
    for name in (
        "DatabaseError",
        "Error",
        "IntegrityError",
        "NotSupportedError",
        "OperationalError",
        "ProgrammingError",
        "sqlite_version",
        "sqlite_version_info",
    ):
        setattr(aiosqlite, name, getattr(sqlite3, name))
    aiosqlite.Connection = _StubConnection
    aiosqlite.__version__ = "0.22.1"

    created = []

    def connect(*arg, **kw):
        connection = _StubConnection()
        created.append(connection)
        return _StubConnectHandle(connection)

    aiosqlite.connect = connect
    return AsyncAdapt_aiosqlite_dbapi(aiosqlite, sqlite3), created


class StreamResultCancelTest(fixtures.TestBase):
    """A result delivered by :meth:`.AsyncConnection.stream` is closed, and
    that close waited for, before its connection is returned to the pool,
    even when the close is interrupted by a cancellation.  A connection
    whose cursor could not be closed is not handed to the next checkout.

    A local stand-in for the aiosqlite driver is used, so that the moment
    at which the task is cancelled is the one this test chooses: after the
    first row was read, with the driver's cursor ``close()`` already
    entered and not yet complete.

    """

    __requires__ = ("asyncio",)

    async def _engine(self):
        from sqlalchemy.ext.asyncio import create_async_engine

        dbapi, created = _stub_aiosqlite_dbapi()
        engine = create_async_engine(
            "sqlite+aiosqlite://",
            module=dbapi,
            poolclass=AsyncAdaptedQueuePool,
            pool_size=1,
            max_overflow=0,
            pool_timeout=1,
        )
        return engine, created

    async def _take_first_row_then_wait(self, engine, state):
        async with engine.connect() as conn:
            async with conn.stream(select(1)) as result:
                state["result"] = result
                state["row"] = await result.fetchone()
                await asyncio.Event().wait()

    @staticmethod
    async def _wait_for_close_start(driver_connection):
        """Wait for the driver's cursor close to have been entered, failing
        rather than hanging if it never is."""

        while not driver_connection.closes_started:
            await asyncio.sleep(0.01)

    @classmethod
    async def _cancel_and_wait_for_close_start(cls, task, driver_connection):
        task.cancel()
        await asyncio.wait_for(cls._wait_for_close_start(driver_connection), 5)
        return driver_connection.closes_started[0]

    @async_test
    async def test_cancel_during_close_does_not_reuse_connection(self):
        engine, created = await self._engine()
        state = {}

        task = asyncio.create_task(
            self._take_first_row_then_wait(engine, state)
        )
        while "row" not in state:
            await asyncio.sleep(0.01)
        eq_(state["row"], (1,))

        driver_connection = created[-1]
        close_gate = asyncio.Event()
        driver_connection.close_gate = close_gate

        # the caller unwinds; its result close is entered and held open
        cursor_under_test = await self._cancel_and_wait_for_close_start(
            task, driver_connection
        )
        is_false(task.done())

        # a second cancellation reaches the same task while that close is
        # still pending
        task.cancel()
        await asyncio.sleep(0.05)
        is_false(task.done())

        # the pool has one slot, and it remains checked out until the
        # connection is safely returned
        eq_(engine.pool.checkedout(), 1)

        # a second caller asking for a connection right now cannot be given
        # the one whose cursor is still being closed; it either waits for
        # the return or is given a connection of its own
        state["hold"] = asyncio.Event()
        second_task = asyncio.create_task(self._second_use(engine, state))
        await asyncio.sleep(0.2)
        is_false(second_task.done())

        # the driver close completes, and only then may the connection be
        # either returned or replaced
        close_gate.set()
        while "second_rows" not in state:
            await asyncio.sleep(0.01)
        eq_(state["second_rows"], [(1,), (2,), (3,)])

        # the original task is still cancelled, as the application requires
        with expect_raises(asyncio.CancelledError, check_context=False):
            await task

        # the result the cancelled task left behind no longer refers to a
        # cursor, and closing it again cannot act on the record the pool
        # has since handed to the second caller, which is still holding it
        is_true(state["result"].closed)
        is_true(state["result"]._real_result.cursor is None)
        await state["result"].close()
        eq_(engine.pool.checkedout(), 1)

        # the cursor under test was closed exactly once, and no further
        # driver connection had to be opened: the slot was reused
        eq_(driver_connection.closes_completed.count(cursor_under_test), 1)
        eq_(len(created), 1)

        state["hold"].set()
        eq_(await asyncio.wait_for(second_task, 5), [(1,), (2,), (3,)])
        eq_(engine.pool.checkedout(), 0)

        await engine.dispose()

    @async_test
    async def test_cancel_during_close_invalidates_connection(self):
        """a close that could not be completed leaves the connection
        unusable; the next checkout receives a new one rather than the
        connection whose cursor is stuck."""

        engine, created = await self._engine()
        state = {}

        task = asyncio.create_task(
            self._take_first_row_then_wait(engine, state)
        )
        while "row" not in state:
            await asyncio.sleep(0.01)

        driver_connection = created[-1]
        close_gate = asyncio.Event()
        driver_connection.close_gate = close_gate

        await self._cancel_and_wait_for_close_start(task, driver_connection)

        # the driver's close could not complete; it ends cancelled, as
        # aiosqlite's own close does when its worker thread has stopped
        driver_connection.close_gate = None
        driver_connection.fail_close = True
        close_gate.set()
        with expect_raises(asyncio.CancelledError, check_context=False):
            await task

        # the connection whose cursor could not be closed is not returned:
        # the next checkout opens a replacement rather than receiving it
        state["hold"] = asyncio.Event()
        state["hold"].set()
        eq_(
            await asyncio.wait_for(self._second_use(engine, state), 5),
            [
                (1,),
                (2,),
                (3,),
            ],
        )
        eq_(len(created), 2)

        await engine.dispose()

    async def _second_use(self, engine, state):
        """Open a connection while ``state["hold"]`` is unset, then read the
        rows; the connection stays checked out until the test releases the
        hold, so that what the pool did with it can be examined."""

        async with engine.connect() as conn:
            rows = (await conn.execute(select(1))).fetchall()
            state["second_conn"] = conn
            state["second_rows"] = rows
            await state["hold"].wait()
        return rows

    @async_test
    async def test_uncancelled_close_is_reused(self):
        """the control run: without a cancellation the close completes and
        the single pool slot is reused rather than replaced."""

        engine, created = await self._engine()

        async def once():
            async with engine.connect() as conn:
                async with conn.stream(select(1)) as result:
                    eq_(await result.fetchone(), (1,))
                is_true(result.closed)

        await once()
        driver_connection = created[-1]
        eq_(engine.pool.checkedout(), 0)
        eq_(len(driver_connection.closes_completed), 1)

        # the one slot is reused, not replaced
        await once()
        eq_(len(created), 1)
        eq_(engine.pool.checkedout(), 0)
        eq_(len(driver_connection.closes_completed), 2)

        await engine.dispose()

    @async_test
    async def test_explicit_close_is_reused(self):
        """the ``await fn()`` style, where the caller closes the result
        itself, continues to work and continues to reuse the slot."""

        engine, created = await self._engine()

        async with engine.connect() as conn:
            result = await conn.stream(select(1))
            eq_(await result.fetchone(), (1,))
            await result.close()
            is_true(result.closed)

        driver_connection = created[-1]
        eq_(engine.pool.checkedout(), 0)
        eq_(len(driver_connection.closes_completed), 1)

        async with engine.connect() as conn:
            result = await conn.stream(select(1))
            eq_(await result.fetchall(), [(1,), (2,), (3,)])
            await result.close()

        eq_(len(created), 1)
        eq_(engine.pool.checkedout(), 0)

        await engine.dispose()

    @async_test
    async def test_stream_scalars_closes_on_cancel(self):
        """``stream_scalars`` is built on ``stream``, so a cancellation
        inside its block closes the cursor as well."""

        engine, created = await self._engine()
        state = {}

        async def caller():
            async with engine.connect() as conn:
                async with conn.stream_scalars(select(1)) as result:
                    state["result"] = result
                    state["row"] = await result.fetchmany(1)
                    await asyncio.Event().wait()

        task = asyncio.create_task(caller())
        while "row" not in state:
            await asyncio.sleep(0.01)
        eq_(list(state["row"]), [1])

        driver_connection = created[-1]
        await self._cancel_and_wait_for_close_start(task, driver_connection)

        with expect_raises(asyncio.CancelledError, check_context=False):
            await task
        is_true(state["result"].closed)
        eq_(engine.pool.checkedout(), 0)

        await engine.dispose()
