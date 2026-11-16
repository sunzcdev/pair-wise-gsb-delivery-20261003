# Copyright Amethyst Reese
# Licensed under the MIT license

import asyncio
import sqlite3
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import IsolatedAsyncioTestCase

import aiosqlite
from .helpers import setup_logger


class TransactionTest(IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        setup_logger()

    def setUp(self):
        td = TemporaryDirectory()
        self.addCleanup(td.cleanup)
        self.db = Path(td.name).resolve() / "test.db"

    async def test_transaction_commit(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            async with db.transaction():
                await db.execute("insert into foo values (1)")
                await db.execute("insert into foo values (2)")

            cursor = await db.execute("select i from foo order by i")
            self.assertEqual(await cursor.fetchall(), [(1,), (2,)])
            self.assertFalse(db.in_transaction)

    async def test_transaction_rollback_on_exception(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            with self.assertRaises(RuntimeError):
                async with db.transaction():
                    await db.execute("insert into foo values (1)")
                    raise RuntimeError("boom")

            cursor = await db.execute("select count(*) from foo")
            self.assertEqual(await cursor.fetchall(), [(0,)])
            self.assertFalse(db.in_transaction)

    async def test_transaction_isolation_levels(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            for isolation in ("DEFERRED", "IMMEDIATE", "EXCLUSIVE"):
                async with db.transaction(isolation=isolation):
                    await db.execute("insert into foo values (1)")

            cursor = await db.execute("select count(*) from foo")
            self.assertEqual(await cursor.fetchall(), [(3,)])

            with self.assertRaises(ValueError):
                db.transaction(isolation="INVALID")

    async def test_transaction_blocks_other_tasks(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            entered = asyncio.Event()
            proceed = asyncio.Event()
            order = []

            async def txn_task():
                async with db.transaction():
                    await db.execute("insert into foo values (1)")
                    entered.set()
                    await proceed.wait()
                    order.append("txn")

            async def other_task():
                await entered.wait()
                await db.execute("insert into foo values (2)")
                await db.commit()
                order.append("other")

            task1 = asyncio.ensure_future(txn_task())
            task2 = asyncio.ensure_future(other_task())

            await entered.wait()
            # Give the other task plenty of chances to run; its statements
            # must remain blocked behind the transaction.
            await asyncio.sleep(0.2)
            self.assertEqual(order, [])

            proceed.set()
            await asyncio.gather(task1, task2)
            self.assertEqual(order, ["txn", "other"])

            cursor = await db.execute("select count(*) from foo")
            self.assertEqual(await cursor.fetchall(), [(2,)])

    async def test_transaction_contexts_serialize(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            first_entered = asyncio.Event()
            first_proceed = asyncio.Event()
            order = []

            async def txn_one():
                async with db.transaction():
                    await db.execute("insert into foo values (1)")
                    first_entered.set()
                    await first_proceed.wait()
                    order.append("one")

            async def txn_two():
                await first_entered.wait()
                async with db.transaction():
                    await db.execute("insert into foo values (2)")
                    order.append("two")

            task1 = asyncio.ensure_future(txn_one())
            task2 = asyncio.ensure_future(txn_two())

            await first_entered.wait()
            await asyncio.sleep(0.2)
            self.assertEqual(order, [])

            first_proceed.set()
            await asyncio.gather(task1, task2)
            self.assertEqual(order, ["one", "two"])

    async def test_transaction_nested_savepoints(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            async with db.transaction():
                await db.execute("insert into foo values (1)")
                with self.assertRaises(RuntimeError):
                    async with db.transaction():
                        await db.execute("insert into foo values (2)")
                        raise RuntimeError("boom")
                async with db.transaction():
                    await db.execute("insert into foo values (3)")

            cursor = await db.execute("select i from foo order by i")
            self.assertEqual(await cursor.fetchall(), [(1,), (3,)])

    async def test_transaction_cancel_rolls_back(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            entered = asyncio.Event()

            async def txn_task():
                async with db.transaction():
                    await db.execute("insert into foo values (1)")
                    entered.set()
                    await asyncio.sleep(60)

            task = asyncio.ensure_future(txn_task())
            await entered.wait()
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await task

            # Transaction was rolled back before waiters were released.
            cursor = await db.execute("select count(*) from foo")
            self.assertEqual(await cursor.fetchall(), [(0,)])
            self.assertFalse(db.in_transaction)

            # The connection is usable again from other tasks.
            async def other_task():
                await db.execute("insert into foo values (2)")
                await db.commit()

            await asyncio.ensure_future(other_task())
            cursor = await db.execute("select count(*) from foo")
            self.assertEqual(await cursor.fetchall(), [(1,)])

    async def test_transaction_manual_transaction_rejected(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            # Explicitly started manual transaction must not be taken over.
            await db.execute("BEGIN")
            with self.assertRaises(sqlite3.ProgrammingError):
                async with db.transaction():
                    pass
            await db.rollback()

            # Implicit transaction from sqlite3 auto-begin is also rejected.
            await db.execute("insert into foo values (1)")
            with self.assertRaises(sqlite3.ProgrammingError):
                async with db.transaction():
                    pass
            await db.rollback()

            # Afterwards the context works again.
            async with db.transaction():
                await db.execute("insert into foo values (2)")

            cursor = await db.execute("select i from foo")
            self.assertEqual(await cursor.fetchall(), [(2,)])

    async def test_transaction_manual_commit_rollback_rejected(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()

            async with db.transaction():
                with self.assertRaises(sqlite3.ProgrammingError):
                    await db.commit()
                with self.assertRaises(sqlite3.ProgrammingError):
                    await db.rollback()
                await db.execute("insert into foo values (1)")

            cursor = await db.execute("select count(*) from foo")
            self.assertEqual(await cursor.fetchall(), [(1,)])

    async def test_transaction_cursor_cannot_escape(self):
        async with aiosqlite.connect(self.db) as db:
            await db.execute("create table foo (i integer)")
            await db.commit()
            await db.execute("insert into foo values (1)")
            await db.commit()

            async with db.transaction():
                cursor = await db.execute("select i from foo")
                self.assertEqual(await cursor.fetchall(), [(1,)])
                cursor2 = await db.cursor()
                await cursor2.execute("select i from foo")

            # Cursors created inside the context are invalid afterwards.
            with self.assertRaises(sqlite3.ProgrammingError):
                await cursor.fetchall()
            with self.assertRaises(sqlite3.ProgrammingError):
                await cursor2.fetchall()

            # Cursors created outside the context are unaffected.
            outside = await db.execute("select i from foo")
            self.assertEqual(await outside.fetchall(), [(1,)])

    async def test_transaction_other_connections_unaffected(self):
        async with aiosqlite.connect(self.db) as db1:
            async with aiosqlite.connect(self.db) as db2:
                await db1.execute("create table foo (i integer)")
                await db1.commit()

                entered = asyncio.Event()
                proceed = asyncio.Event()
                done = []

                async def txn_task():
                    async with db1.transaction():
                        await db1.execute("select count(*) from foo")
                        entered.set()
                        await proceed.wait()

                async def other_conn_task():
                    await entered.wait()
                    cursor = await db2.execute("select count(*) from foo")
                    await cursor.fetchall()
                    done.append("db2")

                task1 = asyncio.ensure_future(txn_task())
                task2 = asyncio.ensure_future(other_conn_task())

                await entered.wait()
                # The second connection is not blocked by db1's transaction.
                await asyncio.wait_for(task2, timeout=5)
                self.assertEqual(done, ["db2"])

                proceed.set()
                await task1
