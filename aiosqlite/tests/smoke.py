# Copyright Amethyst Reese
# Licensed under the MIT license

import asyncio
import sqlite3
import sys
from pathlib import Path
from sqlite3 import OperationalError
from tempfile import TemporaryDirectory
from threading import Barrier, Event, Thread
from unittest import IsolatedAsyncioTestCase, SkipTest
from unittest.mock import patch

import aiosqlite
from .helpers import setup_logger


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

    async def test_cancellable_interrupts_only_target_query(self):
        started = Event()
        release = Event()

        def gate():
            # Block the query until the test has issued the cancellation;
            # the recursive CTE then keeps the query running (and
            # interruptible) until the pending interrupt aborts it.
            started.set()
            release.wait(10)
            return 1

        async with aiosqlite.connect(":memory:") as db:
            await db.create_function("gate", 0, gate)

            token = db.execute_cancellable(
                "WITH RECURSIVE cnt(x) AS ("
                "  SELECT gate()"
                "  UNION ALL SELECT x + 1 FROM cnt WHERE x < 10000000"
                ") SELECT sum(x) FROM cnt"
            )
            self.assertIsInstance(token, aiosqlite.CancelToken)
            task = asyncio.ensure_future(token)

            loop = asyncio.get_event_loop()
            self.assertTrue(await loop.run_in_executor(None, started.wait, 10))

            # the target query is still executing, so cancellation is issued
            self.assertTrue(token.cancel())
            release.set()

            with self.assertRaisesRegex(OperationalError, "interrupted"):
                await task

            # a query started after the cancellation must not be affected
            async with db.execute("select 1, 2") as cursor:
                self.assertEqual(await cursor.fetchall(), [(1, 2)])

    async def test_cancellable_cancel_after_completion(self):
        async with aiosqlite.connect(":memory:") as db:
            token = db.execute_cancellable("select 1")
            cursor = await token
            self.assertIsInstance(cursor, aiosqlite.Cursor)
            self.assertEqual(await cursor.fetchall(), [(1,)])

            # too late: the operation already completed, so no interrupt
            # may be issued
            self.assertFalse(token.cancel())
            self.assertTrue(token.done)

            # the next query must not see any stray interrupt
            async with db.execute("select 2") as cursor:
                self.assertEqual(await cursor.fetchall(), [(2,)])

    async def test_cancellable_cancel_queued(self):
        entered = Event()
        release = Event()
        ran = []

        def gate():
            entered.set()
            release.wait(10)
            return 1

        def mark():
            ran.append(True)
            return 2

        async with aiosqlite.connect(":memory:") as db:
            await db.create_function("gate", 0, gate)
            await db.create_function("mark", 0, mark)

            token1 = db.execute_cancellable("select gate()")
            task1 = asyncio.ensure_future(token1)
            loop = asyncio.get_event_loop()
            self.assertTrue(await loop.run_in_executor(None, entered.wait, 10))

            # queued behind the running query
            token2 = db.execute_cancellable("select mark()")
            task2 = asyncio.ensure_future(token2)

            # cancelling a queued item must not interrupt the running query
            self.assertTrue(token2.cancel())
            self.assertTrue(token2.done)
            with self.assertRaises(asyncio.CancelledError):
                await task2

            release.set()
            cursor = await task1
            self.assertEqual(await cursor.fetchall(), [(1,)])

            # the cancelled item was skipped when reached in queue order
            self.assertEqual(ran, [])

            # the queue drained normally and later queries work
            async with db.execute("select 3") as cursor:
                self.assertEqual(await cursor.fetchall(), [(3,)])

    async def test_cancellable_completion_races_with_cancel(self):
        started = Event()
        barrier = Barrier(2)

        def controlled():
            started.set()
            # return at the same moment the cancellation is attempted
            barrier.wait(10)
            return 42

        async with aiosqlite.connect(":memory:") as db:
            await db.create_function("controlled", 0, controlled)

            token = db.execute_cancellable("select controlled()")
            task = asyncio.ensure_future(token)
            loop = asyncio.get_event_loop()
            self.assertTrue(await loop.run_in_executor(None, started.wait, 10))

            def canceller():
                barrier.wait(10)  # complete and cancel simultaneously
                return token.cancel()

            cancelled = await loop.run_in_executor(None, canceller)

            try:
                cursor = await task
                rows = await cursor.fetchall()
            except OperationalError:
                rows = None

            if cancelled:
                # The interrupt raced with completion: either the statement
                # was aborted, or it finished and the leftover interrupt was
                # absorbed before it could hit any other operation.
                self.assertIn(rows, (None, [(42,)]))
            else:
                # already completed: no interrupt was issued at all
                self.assertEqual(rows, [(42,)])

            # either way, no stray interrupt may affect the next query
            async with db.execute("select 1") as cursor:
                self.assertEqual(await cursor.fetchall(), [(1,)])

    async def test_cancellable_interrupt_transaction_state(self):
        entered = Event()
        release = Event()

        def blocker():
            entered.set()
            release.wait(10)
            return 1

        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table tx_state (i integer)")
            await db.commit()
            await db.create_function("blocker", 0, blocker)

            await db.execute("insert into tx_state values (1)")
            self.assertTrue(db.in_transaction)

            token = db.execute_cancellable(
                "INSERT INTO tx_state "
                "WITH RECURSIVE cnt(x) AS ("
                "  SELECT blocker()"
                "  UNION ALL SELECT x + 1 FROM cnt WHERE x < 10000000"
                ") SELECT x FROM cnt"
            )
            task = asyncio.ensure_future(token)
            loop = asyncio.get_event_loop()
            self.assertTrue(await loop.run_in_executor(None, entered.wait, 10))
            self.assertTrue(token.cancel())
            release.set()

            with self.assertRaises(OperationalError):
                await task

            # aiosqlite must report the transaction state that sqlite3
            # actually has after the interruption
            self.assertEqual(db.in_transaction, db._conn.in_transaction)

            async with db.execute("select count(*) from tx_state") as cursor:
                row = await cursor.fetchone()
            if db.in_transaction:
                # only the interrupted statement was rolled back; the first
                # insert is still pending in the transaction
                self.assertEqual(row[0], 1)
                await db.rollback()
            else:
                # sqlite rolled the whole transaction back
                self.assertEqual(row[0], 0)
            self.assertFalse(db.in_transaction)

            # no partial data from the interrupted statement remains
            async with db.execute("select count(*) from tx_state") as cursor:
                row = await cursor.fetchone()
                self.assertEqual(row[0], 0)

            # the connection is still fully usable afterwards
            await db.execute("insert into tx_state values (2)")
            await db.commit()
            async with db.execute("select * from tx_state") as cursor:
                self.assertEqual(await cursor.fetchall(), [(2,)])

    async def test_cancellable_on_closed_connection(self):
        db = await aiosqlite.connect(":memory:")
        await db.close()
        with self.assertRaisesRegex(ValueError, "Connection closed"):
            db.execute_cancellable("select 1")

    def test_stop_after_event_loop_closed(self):
        db = None

        async def inner():
            nonlocal db
            db = await aiosqlite.connect(":memory:")

        loop = asyncio.new_event_loop()
        loop.run_until_complete(inner())
        loop.close()

        db.stop()
