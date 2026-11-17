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

    async def test_create_function_errors_are_opaque_by_default(self):
        """Without diagnostics, callback failures look exactly like before"""

        def boom(value):
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.create_function("boom", 1, boom)
            with self.assertRaisesRegex(
                OperationalError, "user-defined function raised exception"
            ) as caught:
                await db.execute("select boom(1)")

        exc = caught.exception
        self.assertFalse(hasattr(exc, "udf_diagnostics"))
        self.assertIsNone(exc.__cause__)
        self.assertEqual(db.last_udf_failures, ())

    async def test_create_function_diagnostics_capture_cause(self):
        """Diagnostics attach the real Python exception and keep the type"""

        def boom(value):
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("boom", 1, boom)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom(1)")

            failures = caught.exception.udf_diagnostics
            self.assertIs(caught.exception.udf_failure, failures[0])
            self.assertEqual(len(failures), 1)

            failure = failures[0]
            self.assertEqual(failure.function, "boom")
            self.assertEqual(failure.phase, aiosqlite.UDF_PHASE_CALL)
            self.assertEqual(failure.operation, "execute")
            self.assertIsInstance(failure.exception, ValueError)
            self.assertEqual(str(failure.exception), "kaboom")
            self.assertIs(failure.cause, failure.exception)
            self.assertEqual(failure.callable_name, "boom")
            self.assertIn("boom", str(failure))

            # The SQLite error keeps its identity, and the Python cause travels
            # from the worker thread back to the event loop.
            self.assertIsInstance(caught.exception, sqlite3.Error)
            self.assertIs(caught.exception.__cause__, failure.exception)
            self.assertEqual(
                caught.exception.args,
                ("user-defined function raised exception",),
            )

            # Values are not captured by default.
            self.assertIsNone(failure.arguments)
            self.assertIsNone(failure.sql)

            self.assertEqual(db.last_udf_failures, failures)

    async def test_create_function_diagnostics_include_worker_thread(self):
        """Failure records identify the worker thread and connection"""

        def boom(value):
            raise RuntimeError("nope")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("boom", 1, boom)
            worker = db._thread

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom(1)")

        failure = caught.exception.udf_diagnostics[0]
        self.assertEqual(failure.thread_name, worker.name)
        self.assertNotEqual(failure.thread_id, None)
        self.assertEqual(failure.connection_id, db._connection_id)

    async def test_create_function_diagnostics_distinguish_return_phase(self):
        """A rejected return value is separate from a raised exception"""

        def raised(value):
            raise TypeError("internal blow up")

        def returned(value):
            return object()

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("raised", 1, raised)
            await db.create_function("returned", 1, returned)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select raised(1)")
            raised_failure = caught.exception.udf_diagnostics[0]

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select returned(1)")
            returned_failure = caught.exception.udf_diagnostics[0]

        self.assertEqual(raised_failure.phase, aiosqlite.UDF_PHASE_CALL)
        self.assertIsInstance(raised_failure.exception, TypeError)
        self.assertEqual(raised_failure.function, "raised")

        self.assertEqual(returned_failure.phase, aiosqlite.UDF_PHASE_RETURN)
        self.assertEqual(returned_failure.function, "returned")
        self.assertEqual(returned_failure.returned_type, "object")
        self.assertIsInstance(
            returned_failure.exception, aiosqlite.UDFReturnValueError
        )
        # Both surface as the same SQLite error class.
        self.assertNotEqual(raised_failure.phase, returned_failure.phase)

    async def test_create_function_diagnostics_do_not_leak_between_statements(self):
        """A failing statement must not taint the next one"""

        def boom(value):
            raise ValueError("kaboom")

        def fine(value):
            return value

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("boom", 1, boom)
            await db.create_function("fine", 1, fine)

            with self.assertRaises(OperationalError):
                await db.execute("select boom(1)")

            async with db.execute("select fine(7)") as cursor:
                self.assertEqual(await cursor.fetchall(), [(7,)])

            # A completely unrelated sqlite error carries no stale diagnostics.
            with self.assertRaises(OperationalError) as caught:
                await db.execute("select * from not_a_table")
            self.assertFalse(hasattr(caught.exception, "udf_diagnostics"))

    async def test_create_function_diagnostics_keep_transaction_state(self):
        """Diagnostics must not change transaction outcomes or re-run calls"""

        calls = []

        def boom(value):
            calls.append(value)
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.execute("create table usage (value integer)")
            await db.create_function("boom", 1, boom)

            with self.assertRaises(OperationalError):
                await db.execute("insert into usage values (boom(11))")

            self.assertEqual(calls, [11])
            self.assertTrue(db.in_transaction)
            cursor = await db.execute("select count(*) from usage")
            self.assertEqual(await cursor.fetchone(), (0,))
            await db.rollback()
            self.assertFalse(db.in_transaction)

    async def test_create_function_capture_values_is_opt_in(self):
        """Argument values are only recorded when explicitly requested"""

        def boom(value):
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(
                True, capture_values=True, capture_sql=True
            )
            await db.create_function("boom", 1, boom)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom(?)", ["secret-value"])

            failure = caught.exception.udf_diagnostics[0]
            self.assertEqual(failure.arguments, ("'secret-value'",))
            self.assertEqual(failure.sql, "select boom(?)")

    async def test_create_function_capture_sql_is_opt_in(self):
        """Statement text is withheld unless capture_sql is requested"""

        def boom(value):
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True, capture_values=True)
            await db.create_function("boom", 1, boom)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom(?)", ["secret-value"])

        failure = caught.exception.udf_diagnostics[0]
        self.assertEqual(failure.arguments, ("'secret-value'",))
        self.assertIsNone(failure.sql)

    async def test_create_function_diagnostics_bound_recorded_arguments(self):
        """Captured arguments stay short instead of holding whole payloads"""

        def boom(value):
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True, capture_values=True)
            await db.create_function("boom", 1, boom)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom(?)", ["z" * 5000])

        rendered = caught.exception.udf_diagnostics[0].arguments[0]
        self.assertLess(len(rendered), 100)
        self.assertIn("5000 chars", rendered)

    async def test_create_function_diagnostics_can_be_pinned_per_function(self):
        """Per-function settings override the connection default"""

        def boom(value):
            raise ValueError("kaboom")

        def other(value):
            raise ValueError("also kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(False)
            await db.create_function("boom", 1, boom, diagnostics=True)
            await db.create_function("other", 1, other)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom(1)")
            self.assertEqual(caught.exception.udf_diagnostics[0].function, "boom")

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select other(1)")
            self.assertFalse(hasattr(caught.exception, "udf_diagnostics"))

    async def test_create_function_diagnostics_ignore_valid_returns(self):
        """Normal SQLite return types are never reported as return failures"""

        values = {
            "nothing": None,
            "number": 7,
            "flag": True,
            "ratio": 1.5,
            "text": "value",
            "blob": b"value",
            "buffer": bytearray(b"value"),
            "limit": 2**63 - 1,
        }

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            for name, value in values.items():
                await db.create_function("value_" + name, 0, lambda value=value: value)

            for name in values:
                cursor = await db.execute(f"select value_{name}()")
                row = await cursor.fetchone()
                self.assertEqual(len(row), 1)
            self.assertEqual(db.last_udf_failures, ())

    async def test_create_function_diagnostics_keep_sqlite_error_type(self):
        """A callback raising a sqlite3 error still yields a sqlite3 error"""

        def boom(value):
            raise sqlite3.IntegrityError("conflict")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("boom", 1, boom)

            with self.assertRaises(sqlite3.Error) as caught:
                await db.execute("select boom(1)")

        failure = caught.exception.udf_diagnostics[0]
        self.assertEqual(failure.phase, aiosqlite.UDF_PHASE_CALL)
        self.assertIsInstance(failure.exception, sqlite3.IntegrityError)

    async def test_create_function_diagnostics_survive_fetch(self):
        """Failures raised while fetching rows are attributed too"""

        def boom(value):
            if value == 3:
                raise ValueError("kaboom")
            return value

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("boom", 1, boom)
            await db.execute("create table rows (value integer)")
            await db.executemany(
                "insert into rows values (?)", [(i,) for i in range(1, 4)]
            )

            cursor = await db.cursor()
            # The first row is computed eagerly, so the failing third row is
            # only reached by the fetch call.
            await cursor.execute("select boom(value) from rows")
            with self.assertRaises(OperationalError) as caught:
                await cursor.fetchall()

            failure = caught.exception.udf_diagnostics[0]
            self.assertEqual(failure.operation, "fetchall")
            self.assertEqual(failure.function, "boom")

    async def test_create_function_diagnostics_sequence_is_per_statement(self):
        """Each statement records its own failures with fresh context"""

        def boom(value):
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.create_function("boom", 1, boom)

            first = second = None
            for _ in range(2):
                with self.assertRaises(OperationalError) as caught:
                    await db.execute("select boom(1)")
                failures = caught.exception.udf_diagnostics
                self.assertEqual(len(failures), 1)
                if first is None:
                    first = failures[0]
                else:
                    second = failures[0]

        # SQLite aborts the statement on the first callback failure, so an
        # operation never inherits the previous statement's record.
        self.assertLess(first.sequence, second.sequence)
        self.assertIsNot(first.exception, second.exception)
        self.assertIsNot(first, second)

    async def test_create_function_diagnostics_survive_row_level_failure(self):
        """A failure mid-way through executemany is still attributable"""

        def boom(value):
            if value == 2:
                raise ValueError("kaboom")
            return value

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.execute("create table pairs (value integer)")
            await db.create_function("boom", 1, boom)

            with self.assertRaises(OperationalError) as caught:
                await db.executemany(
                    "insert into pairs values (boom(?))", [(1,), (2,), (3,)]
                )

            failure = caught.exception.udf_diagnostics[0]
            self.assertEqual(failure.operation, "executemany")
            self.assertEqual(failure.function, "boom")
            self.assertEqual(str(failure.exception), "kaboom")

    async def test_create_function_diagnostics_without_arguments(self):
        """Zero-argument callbacks are recorded with an empty argument tuple"""

        def boom():
            raise ValueError("kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True, capture_values=True)
            await db.create_function("boom", 0, boom)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("select boom()")

        failure = caught.exception.udf_diagnostics[0]
        self.assertEqual(failure.arguments, ())
        self.assertEqual(failure.function, "boom")

    async def test_authorizer_diagnostics(self):
        """Authorizer failures are recorded with their own callback kind"""

        def boom(action_code, arg1, arg2, db_name, trigger_name):
            raise ValueError("authorizer kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.set_authorizer(boom)

            with self.assertRaises(sqlite3.DatabaseError) as caught:
                await db.execute("create table denied (id integer)")

        failure = caught.exception.udf_diagnostics[0]
        self.assertEqual(failure.kind, "authorizer")
        self.assertEqual(failure.phase, aiosqlite.UDF_PHASE_CALL)
        self.assertIsInstance(failure.exception, ValueError)
        self.assertEqual(failure.operation, "execute")

    async def test_progress_handler_diagnostics(self):
        """Progress handler failures carry a distinct callback kind"""

        def boom():
            raise ValueError("progress kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.set_progress_handler(boom, 1)

            with self.assertRaises(OperationalError) as caught:
                await db.execute("create table never (id integer)")

        failure = caught.exception.udf_diagnostics[0]
        self.assertEqual(failure.kind, "progress_handler")
        self.assertEqual(failure.function, "progress_handler")
        self.assertEqual(str(failure.exception), "progress kaboom")

    async def test_trace_callback_diagnostics_do_not_change_results(self):
        """Trace callbacks keep running even when diagnostics record failures"""

        statements = []

        def boom(statement):
            statements.append(statement)
            raise ValueError("trace kaboom")

        async with aiosqlite.connect(self.db) as db:
            await db.set_function_diagnostics(True)
            await db.set_trace_callback(boom)

            cursor = await db.execute("select 1")

            # SQLite swallows trace callback errors, so nothing is attached to
            # an exception; the records stay inspectable on the connection.
            trace_failures = [
                failure
                for failure in db.last_udf_failures
                if failure.kind == "trace_callback"
            ]
            self.assertEqual(len(trace_failures), 1)
            self.assertEqual(str(trace_failures[0].exception), "trace kaboom")

            self.assertEqual(await cursor.fetchall(), [(1,)])
            self.assertIn("select 1", statements)

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
