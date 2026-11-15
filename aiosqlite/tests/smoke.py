# Copyright Amethyst Reese
# Licensed under the MIT license

import asyncio
import sqlite3
import sys
from pathlib import Path
from sqlite3 import OperationalError
from tempfile import TemporaryDirectory
from threading import Thread
from unittest import IsolatedAsyncioTestCase, SkipTest
from unittest.mock import patch

import aiosqlite
from aiosqlite import core as aiosqlite_core
from .helpers import setup_logger


class CountAggregate:
    """Aggregate that counts how many rows it has seen."""

    instances: "list[CountAggregate]" = []

    def __init__(self) -> None:
        self.count = 0
        type(self).instances.append(self)

    def step(self, value) -> None:
        self.count += 1

    def finalize(self) -> int:
        return self.count


class SumAggregate:
    """Aggregate that sums non-null integer values."""

    def __init__(self) -> None:
        self.total = 0

    def step(self, value) -> None:
        self.total += value

    def finalize(self) -> int:
        return self.total


class StepErrorAggregate:
    def __init__(self) -> None:
        pass

    def step(self, value) -> None:
        raise ValueError("boom in step")

    def finalize(self):
        return None


class FinalizeErrorAggregate:
    def __init__(self) -> None:
        pass

    def step(self, value) -> None:
        pass

    def finalize(self):
        raise RuntimeError("boom in finalize")


class SumWindow:
    """Aggregate window function maintaining a running sum of a frame."""

    instances: "list[SumWindow]" = []

    def __init__(self) -> None:
        self.total = 0
        type(self).instances.append(self)

    def step(self, value) -> None:
        self.total += value

    def inverse(self, value) -> None:
        self.total -= value

    def value(self) -> int:
        return self.total

    def finalize(self) -> int:
        return self.total


class WindowStepError:
    def __init__(self) -> None:
        pass

    def step(self, value) -> None:
        raise ValueError("boom in window step")

    def inverse(self, value) -> None:
        pass

    def value(self):
        return None

    def finalize(self):
        return None


class WindowInverseError:
    def __init__(self) -> None:
        self.total = 0

    def step(self, value) -> None:
        self.total += value

    def inverse(self, value) -> None:
        raise RuntimeError("boom in window inverse")

    def value(self) -> int:
        return self.total

    def finalize(self) -> int:
        return self.total


class WindowValueError:
    def __init__(self) -> None:
        pass

    def step(self, value) -> None:
        pass

    def inverse(self, value) -> None:
        pass

    def value(self):
        raise KeyError("boom in window value")

    def finalize(self):
        return None


class SmokeTest(IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        setup_logger()

    def setUp(self):
        td = TemporaryDirectory()
        self.addCleanup(td.cleanup)
        self.db = Path(td.name).resolve() / "test.db"

    async def test_connection_await(self):
        db = await aiosqlite.connect(self.db)
        self.assertIsInstance(db, aiosqlite.Connection)

        async with db.execute("select 1, 2") as cursor:
            rows = await cursor.fetchall()
            self.assertEqual(rows, [(1, 2)])

        await db.close()

    async def test_connection_context(self):
        async with aiosqlite.connect(self.db) as db:
            self.assertIsInstance(db, aiosqlite.Connection)

            async with db.execute("select 1, 2") as cursor:
                rows = await cursor.fetchall()
                self.assertEqual(rows, [(1, 2)])

    async def test_connection_locations(self):
        TEST_DB = self.db.as_posix()

        class Fake:  # pylint: disable=too-few-public-methods
            def __str__(self):
                return TEST_DB

        locs = (Path(TEST_DB), TEST_DB, TEST_DB.encode(), Fake())

        async with aiosqlite.connect(locs[0]) as db:
            await db.execute("create table foo (i integer, k integer)")
            await db.execute("insert into foo (i, k) values (1, 5)")
            await db.commit()

            cursor = await db.execute("select * from foo")
            rows = await cursor.fetchall()

        for loc in locs:
            async with aiosqlite.connect(loc) as db:
                cursor = await db.execute("select * from foo")
                self.assertEqual(await cursor.fetchall(), rows)

    async def test_multiple_connections(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute(
                "create table multiple_connections "
                "(i integer primary key asc, k integer)"
            )

        async def do_one_conn(i):
            async with aiosqlite.connect(self.db) as db:
                await db.execute("insert into multiple_connections (k) values (?)", [i])
                await db.commit()

        await asyncio.gather(*[do_one_conn(i) for i in range(10)])

        async with aiosqlite.connect(self.db) as db:
            cursor = await db.execute("select * from multiple_connections")
            rows = await cursor.fetchall()

        assert len(rows) == 10

    async def test_multiple_queries(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute(
                "create table multiple_queries "
                "(i integer primary key asc, k integer)"
            )

            await asyncio.gather(
                *[
                    db.execute("insert into multiple_queries (k) values (?)", [i])
                    for i in range(10)
                ]
            )

            await db.commit()

        async with aiosqlite.connect(self.db) as db:
            cursor = await db.execute("select * from multiple_queries")
            rows = await cursor.fetchall()

        assert len(rows) == 10

    async def test_iterable_cursor(self):
        async with aiosqlite.connect(self.db) as db:
            cursor = await db.cursor()
            await cursor.execute(
                "create table iterable_cursor " "(i integer primary key asc, k integer)"
            )
            await cursor.executemany(
                "insert into iterable_cursor (k) values (?)", [[i] for i in range(10)]
            )
            await db.commit()

        async with aiosqlite.connect(self.db) as db:
            cursor = await db.execute("select * from iterable_cursor")
            rows = []
            async for row in cursor:
                rows.append(row)

        assert len(rows) == 10

    async def test_multi_loop_usage(self):
        results = {}

        def runner(k, conn):
            async def query():
                async with conn.execute("select * from foo") as cursor:
                    rows = await cursor.fetchall()
                    self.assertEqual(len(rows), 2)
                    return rows

            with self.subTest(k):
                loop = asyncio.new_event_loop()
                rows = loop.run_until_complete(query())
                loop.close()
                results[k] = rows

        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id int, name varchar)")
            await db.execute(
                "insert into foo values (?, ?), (?, ?)", (1, "Sally", 2, "Janet")
            )
            await db.commit()

            threads = [Thread(target=runner, args=(k, db)) for k in range(4)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()

        self.assertEqual(len(results), 4)
        for rows in results.values():
            self.assertEqual(len(rows), 2)

    async def test_context_cursor(self):
        async with aiosqlite.connect(self.db) as db:
            async with db.cursor() as cursor:
                await cursor.execute(
                    "create table context_cursor "
                    "(i integer primary key asc, k integer)"
                )
                await cursor.executemany(
                    "insert into context_cursor (k) values (?)",
                    [[i] for i in range(10)],
                )
                await db.commit()

        async with aiosqlite.connect(self.db) as db:
            async with db.execute("select * from context_cursor") as cursor:
                rows = []
                async for row in cursor:
                    rows.append(row)

        assert len(rows) == 10

    async def test_cursor_return_self(self):
        async with aiosqlite.connect(self.db) as db:
            cursor = await db.cursor()

            result = await cursor.execute(
                "create table test_cursor_return_self (i integer, k integer)"
            )
            self.assertEqual(result, cursor, "cursor execute returns itself")

            result = await cursor.executemany(
                "insert into test_cursor_return_self values (?, ?)", [(1, 1), (2, 2)]
            )
            self.assertEqual(result, cursor)

            result = await cursor.executescript(
                "insert into test_cursor_return_self values (3, 3);"
                "insert into test_cursor_return_self values (4, 4);"
                "insert into test_cursor_return_self values (5, 5);"
            )
            self.assertEqual(result, cursor)

    async def test_connection_properties(self):
        async with aiosqlite.connect(self.db) as db:
            self.assertEqual(db.total_changes, 0)

            async with db.cursor() as cursor:
                self.assertFalse(db.in_transaction)
                await cursor.execute(
                    "create table test_properties "
                    "(i integer primary key asc, k integer, d text)"
                )
                await cursor.execute(
                    "insert into test_properties (k, d) values (1, 'hi')"
                )
                self.assertTrue(db.in_transaction)
                await db.commit()
                self.assertFalse(db.in_transaction)

            self.assertEqual(db.total_changes, 1)

            self.assertIsNone(db.row_factory)
            self.assertEqual(db.text_factory, str)

            async with db.cursor() as cursor:
                await cursor.execute("select * from test_properties")
                row = await cursor.fetchone()
                self.assertIsInstance(row, tuple)
                self.assertEqual(row, (1, 1, "hi"))
                with self.assertRaises(TypeError):
                    _ = row["k"]

            async with db.cursor() as cursor:
                cursor.row_factory = aiosqlite.Row
                self.assertEqual(cursor.row_factory, aiosqlite.Row)
                await cursor.execute("select * from test_properties")
                row = await cursor.fetchone()
                self.assertIsInstance(row, aiosqlite.Row)
                self.assertEqual(row[1], 1)
                self.assertEqual(row[2], "hi")
                self.assertEqual(row["k"], 1)
                self.assertEqual(row["d"], "hi")

            db.row_factory = aiosqlite.Row
            db.text_factory = bytes
            self.assertEqual(db.row_factory, aiosqlite.Row)
            self.assertEqual(db.text_factory, bytes)

            async with db.cursor() as cursor:
                await cursor.execute("select * from test_properties")
                row = await cursor.fetchone()
                self.assertIsInstance(row, aiosqlite.Row)
                self.assertEqual(row[1], 1)
                self.assertEqual(row[2], b"hi")
                self.assertEqual(row["k"], 1)
                self.assertEqual(row["d"], b"hi")

    async def test_fetch_all(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute(
                "create table test_fetch_all (i integer primary key asc, k integer)"
            )
            await db.execute(
                "insert into test_fetch_all (k) values (10), (24), (16), (32)"
            )
            await db.commit()

        async with aiosqlite.connect(self.db) as db:
            cursor = await db.execute("select k from test_fetch_all where k < 30")
            rows = await cursor.fetchall()
            self.assertEqual(rows, [(10,), (24,), (16,)])

    async def test_enable_load_extension(self):
        """Assert that after enabling extension loading, they can be loaded"""
        async with aiosqlite.connect(self.db) as db:
            try:
                await db.enable_load_extension(True)
                await db.load_extension("test")
            except OperationalError as e:
                assert "not authorized" not in e.args
            except AttributeError as e:
                raise SkipTest(
                    "python was not compiled with sqlite3 "
                    "extension support, so we can't test it"
                ) from e

    async def test_set_progress_handler(self):
        """
        Assert that after setting a progress handler returning 1, DB operations are aborted
        """
        async with aiosqlite.connect(self.db) as db:
            await db.set_progress_handler(lambda: 1, 1)
            with self.assertRaises(OperationalError):
                await db.execute(
                    "create table test_progress_handler (i integer primary key asc, k integer)"
                )

    async def test_create_function(self):
        """Assert that after creating a custom function, it can be used"""

        def no_arg():
            return "no arg"

        def one_arg(num):
            return num * 2

        async with aiosqlite.connect(self.db) as db:
            await db.create_function("no_arg", 0, no_arg)
            await db.create_function("one_arg", 1, one_arg)

            async with db.execute("SELECT no_arg();") as res:
                row = await res.fetchone()
                self.assertEqual(row[0], "no arg")

            async with db.execute("SELECT one_arg(10);") as res:
                row = await res.fetchone()
                self.assertEqual(row[0], 20)

    async def test_create_function_deterministic(self):
        """Assert that after creating a deterministic custom function, it can be used.

        https://sqlite.org/deterministic.html
        """

        def one_arg(num):
            return num * 2

        async with aiosqlite.connect(self.db) as db:
            await db.create_function("one_arg", 1, one_arg, deterministic=True)
            await db.execute("create table foo (id int, bar int)")

            # Non-deterministic functions cannot be used in indexes
            await db.execute("create index t on foo(one_arg(bar))")

    async def test_create_aggregate(self):
        """Registered aggregate functions can be used inside SQL queries."""
        CountAggregate.instances.clear()

        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id integer)")
            await db.executemany("insert into foo values (?)", [(1,), (2,), (3,), (4,)])
            await db.create_aggregate("my_count", 1, CountAggregate)
            await db.create_aggregate("my_sum", 1, SumAggregate)

            async with db.execute("select my_count(id), my_sum(id) from foo") as cur:
                self.assertEqual(await cur.fetchone(), (4, 10))

            # zero-row aggregate: no rows means step() is never called and
            # SQLite reports NULL without ever instantiating the class
            async with db.execute("select my_count(id) from foo where id > 100") as cur:
                self.assertIsNone((await cur.fetchone())[0])

    async def test_create_aggregate_distinct_instance_per_query(self):
        """A new aggregate object must back every query, never a shared one."""
        CountAggregate.instances.clear()

        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id integer)")
            await db.executemany("insert into foo values (?)", [(1,), (2,)])
            await db.create_aggregate("my_count", 1, CountAggregate)

            await db.execute_fetchall("select my_count(id) from foo")
            await db.execute_fetchall("select my_count(id) from foo")
            await db.execute("create table bar (id integer)")
            await db.executemany("insert into bar values (?)", [(1,), (2,), (3,)])
            await db.execute_fetchall("select my_count(id) from bar")

        # three queries, three independent objects, each counting its own rows
        self.assertEqual(len(CountAggregate.instances), 3)
        self.assertEqual(len({id(obj) for obj in CountAggregate.instances}), 3)
        self.assertEqual(
            sorted(obj.count for obj in CountAggregate.instances), [2, 2, 3]
        )

    async def test_create_aggregate_exception_propagation(self):
        """step/finalize errors surface through sqlite3 error semantics."""
        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id integer)")
            await db.executemany("insert into foo values (?)", [(1,), (2,)])

            await db.create_aggregate("step_boom", 1, StepErrorAggregate)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select step_boom(id) from foo")

            await db.create_aggregate("finalize_boom", 1, FinalizeErrorAggregate)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select finalize_boom(id) from foo")

            # the connection stays usable after a callback error
            async with db.execute("select 1") as cur:
                self.assertEqual(await cur.fetchone(), (1,))

    async def test_create_aggregate_deregister(self):
        """Replacing a registration with None removes the SQL function."""
        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id integer)")
            await db.execute("insert into foo values (1)")
            await db.create_aggregate("my_sum", 1, SumAggregate)

            async with db.execute("select my_sum(id) from foo") as cur:
                self.assertEqual(await cur.fetchone(), (1,))

            await db.create_aggregate("my_sum", 1, None)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select my_sum(id) from foo")

    async def test_registration_runs_on_worker_thread_in_order(self):
        """Registrations/deregistrations queue on the worker thread in order."""
        events: list[tuple[str, str]] = []

        class LoggingConnection(aiosqlite_core.Connection):
            async def _execute(self, fn, *args, **kwargs):
                name = getattr(fn, "__name__", repr(fn))
                events.append(("queued", name))
                try:
                    return await super()._execute(fn, *args, **kwargs)
                finally:
                    events.append(("done", name))

        db = LoggingConnection(lambda: sqlite3.connect(":memory:"), 64)
        await db
        try:
            await db.create_function("scalar", 1, lambda value: value)
            await db.create_aggregate("agg", 1, SumAggregate)
            await db.execute("create table foo (id integer)")
            for value in (1, 2, 3):
                await db.execute("insert into foo values (?)", [value])
            self.assertEqual(
                await db.execute_fetchall("select agg(id) from foo"), [(6,)]
            )
            await db.create_aggregate("agg", 1, None)

            expected = [
                "create_function",
                "create_aggregate",
                "execute",  # create table
                "execute",  # insert x3
                "execute",
                "execute",
                "_execute_fetchall",  # select agg
                "create_aggregate",  # deregister
            ]
            names = [name for _, name in events]
            self.assertEqual(names, [x for name in expected for x in (name, name)])
            # every operation finished before the next one started (FIFO)
            self.assertEqual(
                [phase for phase, _ in events],
                ["queued", "done"] * len(expected),
            )
            # all queued work has drained and the deregistration took effect
            self.assertTrue(db._tx.empty())
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select agg(id) from foo")
        finally:
            await db.close()

    async def test_create_window_function(self):
        """Window functions support frames, inverse and value callbacks."""
        if not hasattr(sqlite3.Connection, "create_window_function"):
            raise SkipTest("window functions require Python 3.11 or newer")

        SumWindow.instances.clear()

        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (grp text, id integer)")
            await db.executemany(
                "insert into foo values (?, ?)",
                [("a", 1), ("a", 2), ("a", 3), ("b", 4)],
            )
            await db.create_window_function("win_sum", 1, SumWindow)

            async with db.execute(
                "select grp, id, win_sum(id) over ("
                "partition by grp order by id "
                "rows between 1 preceding and current row"
                ") from foo order by grp, id"
            ) as cur:
                self.assertEqual(
                    await cur.fetchall(),
                    [("a", 1, 1), ("a", 2, 3), ("a", 3, 5), ("b", 4, 4)],
                )

            # unpartitioned frame: the same object accumulates every row
            async with db.execute("select win_sum(id) over () from foo") as cur:
                self.assertEqual([row[0] for row in await cur.fetchall()], [10] * 4)

        # each query got its own window function instance
        self.assertGreaterEqual(len(SumWindow.instances), 2)
        self.assertEqual(
            len({id(obj) for obj in SumWindow.instances}),
            len(SumWindow.instances),
        )

    async def test_create_window_function_exception_propagation(self):
        """step/inverse/value errors propagate via sqlite3 error semantics."""
        if not hasattr(sqlite3.Connection, "create_window_function"):
            raise SkipTest("window functions require Python 3.11 or newer")

        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id integer)")
            await db.executemany("insert into foo values (?)", [(1,), (2,), (3,)])

            await db.create_window_function("step_boom", 1, WindowStepError)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select step_boom(id) over () from foo")

            await db.create_window_function("inverse_boom", 1, WindowInverseError)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall(
                    "select inverse_boom(id) over ("
                    "order by id rows between 1 preceding and current row"
                    ") from foo"
                )

            await db.create_window_function("value_boom", 1, WindowValueError)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select value_boom(id) over () from foo")

            async with db.execute("select 1") as cur:
                self.assertEqual(await cur.fetchone(), (1,))

    async def test_create_window_function_deregister(self):
        """Passing None removes a previously registered window function."""
        if not hasattr(sqlite3.Connection, "create_window_function"):
            raise SkipTest("window functions require Python 3.11 or newer")

        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (id integer)")
            await db.executemany("insert into foo values (?)", [(1,), (2,)])
            await db.create_window_function("win_sum", 1, SumWindow)

            async with db.execute("select win_sum(id) over () from foo") as cur:
                self.assertEqual([row[0] for row in await cur.fetchall()], [3, 3])

            await db.create_window_function("win_sum", 1, None)
            with self.assertRaises(sqlite3.OperationalError):
                await db.execute_fetchall("select win_sum(id) over () from foo")

    async def test_create_window_function_unsupported(self):
        """Missing window-function support is rejected at registration time."""
        original = aiosqlite_core._supports_window_functions
        aiosqlite_core._supports_window_functions = lambda: False
        try:
            async with aiosqlite.connect(":memory:") as db:
                with self.assertRaises(sqlite3.NotSupportedError):
                    await db.create_window_function("win_sum", 1, SumWindow)

                # nothing was queued: the connection remains fully usable
                async with db.execute("select 1") as cur:
                    self.assertEqual(await cur.fetchone(), (1,))
        finally:
            aiosqlite_core._supports_window_functions = original

    async def test_set_trace_callback(self):
        statements = []

        def callback(statement: str):
            statements.append(statement)

        async with aiosqlite.connect(self.db) as db:
            await db.set_trace_callback(callback)

            await db.execute("select 10")
            self.assertIn("select 10", statements)

    async def test_set_authorizer_deny_drops(self):
        """Test authorizer that denies DROP operations"""

        def deny_drops(action_code, arg1, arg2, db_name, trigger_name):
            if action_code == sqlite3.SQLITE_DROP_TABLE:
                return sqlite3.SQLITE_DENY
            return sqlite3.SQLITE_OK

        async with aiosqlite.connect(self.db) as db:
            await db.set_authorizer(deny_drops)

            # Other operations should succeed
            await db.execute("CREATE TABLE test_drop (id INTEGER)")
            await db.execute("INSERT INTO test_drop VALUES (1)")
            await db.execute("SELECT * FROM test_drop")

            # DROP should fail
            with self.assertRaises(sqlite3.DatabaseError):
                await db.execute("DROP TABLE test_drop")

            if sys.version_info >= (3, 11):
                # Disabling the authorizer re-enables DROP
                await db.set_authorizer(None)
                await db.execute("DROP TABLE test_drop")

    async def test_set_authorizer_exception_propagation(self):
        """Test that exceptions raised in authorizer callback are caught by SQLite"""

        def raise_exception(action_code, arg1, arg2, db_name, trigger_name):
            raise ValueError("Test exception from authorizer")

        async with aiosqlite.connect(self.db) as db:
            await db.set_authorizer(raise_exception)
            with self.assertRaises(sqlite3.DatabaseError):
                await db.execute("CREATE TABLE test_exception (id INTEGER)")

    async def test_connect_error(self):
        bad_db = Path("/something/that/shouldnt/exist.db")
        with self.assertRaisesRegex(OperationalError, "unable to open database"):
            async with aiosqlite.connect(bad_db) as db:
                self.assertIsNone(db)  # should never be reached

        with self.assertRaisesRegex(OperationalError, "unable to open database"):
            await aiosqlite.connect(bad_db)

    async def test_connect_base_exception(self):
        # Check if connect task is cancelled, thread is properly closed.
        def _raise_cancelled_error(*_, **__):
            raise asyncio.CancelledError("I changed my mind")

        connection = aiosqlite.Connection(lambda: sqlite3.connect(":memory:"), 64)
        with (
            patch.object(sqlite3, "connect", side_effect=_raise_cancelled_error),
            self.assertRaisesRegex(asyncio.CancelledError, "I changed my mind"),
        ):
            async with connection:
                ...
        # Terminate the thread here if the test fails to have a clear error.
        if connection._running:
            connection.stop()
            raise AssertionError("connection thread was not stopped")

    async def test_iterdump(self):
        async with aiosqlite.connect(":memory:") as db:
            await db.execute("create table foo (i integer, k charvar(250))")
            await db.executemany(
                "insert into foo values (?, ?)", [(1, "hello"), (2, "world")]
            )

            lines = [line async for line in db.iterdump()]
            self.assertEqual(
                lines,
                [
                    "BEGIN TRANSACTION;",
                    "CREATE TABLE foo (i integer, k charvar(250));",
                    "INSERT INTO \"foo\" VALUES(1,'hello');",
                    "INSERT INTO \"foo\" VALUES(2,'world');",
                    "COMMIT;",
                ],
            )

    async def test_cursor_on_closed_connection(self):
        db = await aiosqlite.connect(self.db)

        cursor = await db.execute("select 1, 2")
        await db.close()
        with self.assertRaisesRegex(ValueError, "Connection closed"):
            await cursor.fetchall()
        with self.assertRaisesRegex(ValueError, "Connection closed"):
            await cursor.fetchall()

    async def test_cursor_on_closed_connection_loop(self):
        db = await aiosqlite.connect(self.db)

        cursor = await db.execute("select 1, 2")
        tasks = []
        for i in range(100):
            if i == 50:
                tasks.append(asyncio.ensure_future(db.close()))
            tasks.append(asyncio.ensure_future(cursor.fetchall()))
        for task in tasks:
            try:
                await task
            except sqlite3.ProgrammingError:
                pass

    async def test_close_blocking_until_transaction_queue_empty(self):
        db = await aiosqlite.connect(self.db)
        # Insert transactions into the
        # transaction queue '_tx'
        for i in range(1000):
            await db.execute(f"select 1, {i}")
        # Wait for all transactions to complete
        await db.close()
        # Check no more transaction pending
        self.assertEqual(db._tx.empty(), True)

    async def test_close_twice(self):
        db = await aiosqlite.connect(self.db)

        await db.close()

        # no error
        await db.close()

    async def test_backup_aiosqlite(self):
        def progress(a, b, c):
            print(a, b, c)

        async with (
            aiosqlite.connect(":memory:") as db1,
            aiosqlite.connect(":memory:") as db2,
        ):
            await db1.execute("create table foo (i integer, k charvar(250))")
            await db1.executemany(
                "insert into foo values (?, ?)", [(1, "hello"), (2, "world")]
            )
            await db1.commit()

            with self.assertRaisesRegex(OperationalError, "no such table: foo"):
                await db2.execute("select * from foo")

            await db1.backup(db2, progress=progress)

            async with db2.execute("select * from foo") as cursor:
                rows = await cursor.fetchall()
                self.assertEqual(rows, [(1, "hello"), (2, "world")])

    async def test_backup_sqlite(self):
        async with aiosqlite.connect(":memory:") as db1:
            with sqlite3.connect(":memory:") as db2:
                await db1.execute("create table foo (i integer, k charvar(250))")
                await db1.executemany(
                    "insert into foo values (?, ?)", [(1, "hello"), (2, "world")]
                )
                await db1.commit()

                with self.assertRaisesRegex(OperationalError, "no such table: foo"):
                    db2.execute("select * from foo")

                await db1.backup(db2)

                cursor = db2.execute("select * from foo")
                rows = cursor.fetchall()
                self.assertEqual(rows, [(1, "hello"), (2, "world")])

    async def test_emits_warning_when_left_open(self):
        db = await aiosqlite.connect(":memory:")

        with self.assertWarnsRegex(
            ResourceWarning, r".*was deleted before being closed.*"
        ):
            del db

    async def test_stop_without_close(self):
        db = await aiosqlite.connect(":memory:")
        await db.stop()

    def test_stop_after_event_loop_closed(self):
        db = None

        async def inner():
            nonlocal db
            db = await aiosqlite.connect(":memory:")

        loop = asyncio.new_event_loop()
        loop.run_until_complete(inner())
        loop.close()

        db.stop()
