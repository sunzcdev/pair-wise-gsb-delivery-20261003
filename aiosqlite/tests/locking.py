# Copyright Amethyst Reese
# Licensed under the MIT license

"""
Tests for the optional asynchronous busy/locked retry strategy.

Two connections sharing one database file are used to control exactly when
the write lock is taken and released: a holder connection keeps the lock
while a waiter connection's statement is retried on the event loop.
"""

import asyncio
import sqlite3
import time
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import IsolatedAsyncioTestCase
from unittest.mock import patch

import aiosqlite
from aiosqlite import core
from aiosqlite.core import _is_busy_error


def _counting_execute(calls: list[int]):
    """
    Patch ``Connection._execute`` with a counter wrapper.

    The C-accelerated ``sqlite3.Connection`` is immutable and cannot be
    patched directly, so invocation counting is done at the aiosqlite layer.
    """
    original = core.Connection._execute

    async def wrapper(self, *args, **kwargs):
        calls.append(None)
        return await original(self, *args, **kwargs)

    return patch.object(core.Connection, "_execute", wrapper)


class BusyWaitTest(IsolatedAsyncioTestCase):
    def setUp(self):
        # Windows may still be releasing file handles when the temp dir is
        # removed; ignore cleanup races rather than fail unrelated assertions.
        self._tmp = TemporaryDirectory(ignore_cleanup_errors=True)
        self.addCleanup(self._tmp.cleanup)
        self.db = Path(self._tmp.name).resolve() / "locking.db"

        seed = sqlite3.connect(self.db)
        seed.execute("create table t (i integer primary key, k integer)")
        seed.execute("insert into t (i, k) values (1, 10)")
        seed.commit()
        seed.close()

    async def _hold_write_lock(self, hold_for: float) -> None:
        """Take and later release the database write lock from another connection."""
        holder = await aiosqlite.connect(self.db, timeout=0, isolation_level=None)
        try:
            await holder.execute("begin immediate")
            await asyncio.sleep(hold_for)
            await holder.execute("rollback")
        finally:
            await holder.close()

    async def _locked_holder(self, hold_for: float) -> asyncio.Task:
        """Start a holder and wait until it actually owns the write lock."""
        task = asyncio.ensure_future(self._hold_write_lock(hold_for))
        # Yield control long enough for the holder's worker thread to run
        # ``begin immediate`` before the waiter touches the database.
        while not task.done():
            await asyncio.sleep(0.01)
            if self._write_lock_held():
                break
        if task.done():
            # Surface any failure (for example an unexpected lock error).
            await task
        return task

    def _write_lock_held(self) -> bool:
        probe = sqlite3.connect(self.db, timeout=0)
        try:
            probe.execute("begin immediate").close()
        except sqlite3.OperationalError:
            return True
        else:
            probe.rollback()
            return False
        finally:
            probe.close()

    async def test_retry_until_lock_released(self):
        async with aiosqlite.connect(
            self.db, timeout=0, busy_wait=5.0, busy_wait_interval=0.02
        ) as waiter:
            holder_task = await self._locked_holder(0.3)
            started = time.monotonic()

            # The event loop must stay responsive while the statement waits.
            loop_ticks = 0

            async def heartbeat():
                nonlocal loop_ticks
                while not holder_task.done():
                    await asyncio.sleep(0.02)
                    loop_ticks += 1

            heartbeat_task = asyncio.ensure_future(heartbeat())

            # An INSERT must wait until the holder releases the write lock.
            await waiter.execute("insert into t (i, k) values (2, 20)")
            await waiter.commit()
            elapsed = time.monotonic() - started

            await holder_task
            await heartbeat_task

            self.assertGreaterEqual(elapsed, 0.25)
            self.assertGreater(loop_ticks, 0)

            cursor = await waiter.execute("select count(*) from t")
            self.assertEqual(await cursor.fetchone(), (2,))

    async def test_deadline_exhausted_raises(self):
        async with aiosqlite.connect(
            self.db, timeout=0, busy_wait=0.2, busy_wait_interval=0.05
        ) as waiter:
            holder_task = await self._locked_holder(2.0)
            started = time.monotonic()

            with self.assertRaises(sqlite3.OperationalError):
                await waiter.execute("insert into t (i, k) values (3, 30)")
            elapsed = time.monotonic() - started

            holder_task.cancel()
            try:
                await holder_task
            except asyncio.CancelledError:
                pass

            # It gives up near the configured deadline, well before the
            # holder releases the lock, without blocking the worker thread.
            self.assertGreaterEqual(elapsed, 0.15)
            self.assertLess(elapsed, 1.0)

            cursor = await waiter.execute("select count(*) from t")
            self.assertEqual(await cursor.fetchone(), (1,))

    async def test_disabled_by_default_fails_fast(self):
        async with aiosqlite.connect(self.db, timeout=0) as waiter:
            holder_task = await self._locked_holder(1.0)

            started = time.monotonic()
            with self.assertRaises(sqlite3.OperationalError):
                await waiter.execute("insert into t (i, k) values (4, 40)")
            elapsed = time.monotonic() - started

            holder_task.cancel()
            try:
                await holder_task
            except asyncio.CancelledError:
                pass

            self.assertLess(elapsed, 0.25)

    async def test_cancellation_interrupts_wait(self):
        async with aiosqlite.connect(
            self.db, timeout=0, busy_wait=10.0, busy_wait_interval=0.05
        ) as waiter:
            holder_task = await self._locked_holder(5.0)

            async def blocked_insert():
                await waiter.execute("insert into t (i, k) values (5, 50)")

            insert_task = asyncio.ensure_future(blocked_insert())
            await asyncio.sleep(0.15)
            self.assertFalse(insert_task.done())

            insert_task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await insert_task

            holder_task.cancel()
            try:
                await holder_task
            except asyncio.CancelledError:
                pass

            # The connection remains usable and the statement had no effect.
            cursor = await waiter.execute("select count(*) from t")
            self.assertEqual(await cursor.fetchone(), (1,))

    async def test_integrity_error_is_not_retried(self):
        async with aiosqlite.connect(
            self.db, busy_wait=5.0, busy_wait_interval=0.02
        ) as db:
            calls: list = []
            with _counting_execute(calls):
                with self.assertRaises(sqlite3.IntegrityError):
                    await db.execute("insert into t (i, k) values (1, 99)")
            self.assertEqual(len(calls), 1)

    async def test_user_function_error_is_not_retried(self):
        async with aiosqlite.connect(
            ":memory:", busy_wait=5.0, busy_wait_interval=0.02
        ) as db:
            calls = 0

            def boom():
                nonlocal calls
                calls += 1
                raise RuntimeError("boom")

            await db.create_function("boom", 0, boom)

            with self.assertRaises(sqlite3.OperationalError):
                await db.execute("select boom()")
            self.assertEqual(calls, 1)

    async def test_non_retryable_operations_propagate_immediately(self):
        async with aiosqlite.connect(
            self.db, timeout=0, busy_wait=5.0, busy_wait_interval=0.02
        ) as waiter:
            holder_task = await self._locked_holder(2.0)

            calls: list = []
            with _counting_execute(calls):
                with self.assertRaises(sqlite3.OperationalError):
                    await waiter.executemany(
                        "insert into t (i, k) values (?, ?)",
                        [(6, 60), (7, 70)],
                    )
                with self.assertRaises(sqlite3.OperationalError):
                    await waiter.executescript("insert into t (i, k) values (8, 80)")
            # Each helper runs exactly once; neither is retried.
            self.assertEqual(len(calls), 2)

            holder_task.cancel()
            try:
                await holder_task
            except asyncio.CancelledError:
                pass

    async def test_execute_insert_helper_is_not_retried(self):
        async with aiosqlite.connect(
            self.db, timeout=0, busy_wait=5.0, busy_wait_interval=0.02
        ) as waiter:
            holder_task = await self._locked_holder(2.0)

            with patch.object(
                waiter, "_execute_insert", wraps=waiter._execute_insert
            ) as spy:
                with self.assertRaises(sqlite3.OperationalError):
                    await waiter.execute_insert("insert into t (i, k) values (9, 90)")
                self.assertEqual(spy.call_count, 1)

            holder_task.cancel()
            try:
                await holder_task
            except asyncio.CancelledError:
                pass

    async def test_cursor_execute_retries(self):
        async with aiosqlite.connect(
            self.db, timeout=0, busy_wait=5.0, busy_wait_interval=0.02
        ) as waiter:
            holder_task = await self._locked_holder(0.3)

            cursor = await waiter.cursor()
            await cursor.execute("insert into t (i, k) values (10, 100)")
            await waiter.commit()

            await holder_task
            await cursor.execute("select count(*) from t")
            self.assertEqual(await cursor.fetchone(), (2,))

    async def test_retry_succeeds_after_transient_busy(self):
        async with aiosqlite.connect(
            ":memory:", busy_wait=1.0, busy_wait_interval=0.01
        ) as db:
            real_connection = db._connection
            attempts = 0

            class FlakyConnection:
                """Proxy that reports busy twice before succeeding."""

                def execute(self, *args, **kwargs):
                    nonlocal attempts
                    attempts += 1
                    if attempts < 3:
                        raise sqlite3.OperationalError("database is locked")
                    return real_connection.execute(*args, **kwargs)

                def __getattr__(self, name):
                    return getattr(real_connection, name)

            # The C-accelerated sqlite3.Connection cannot be patched, so the
            # worker is pointed at a proxy exercising the real retry loop.
            db._connection = FlakyConnection()
            await db.execute("create table t (i integer)")

            self.assertEqual(attempts, 3)

    async def test_other_operational_errors_propagate(self):
        async with aiosqlite.connect(
            ":memory:", busy_wait=5.0, busy_wait_interval=0.02
        ) as db:
            calls: list = []
            with _counting_execute(calls):
                with self.assertRaises(sqlite3.OperationalError):
                    await db.execute("select * from nope")
            self.assertEqual(len(calls), 1)

    async def test_invalid_parameters_rejected(self):
        with self.assertRaises(ValueError):
            await aiosqlite.connect(self.db, busy_wait=-1)

        with self.assertRaises(ValueError):
            await aiosqlite.connect(self.db, busy_wait_interval=0)

        with self.assertRaises(ValueError):
            await aiosqlite.connect(self.db, busy_wait_interval=-0.1)

        # Validation happens before the connector runs, so no thread or
        # sqlite connection is created for a rejected configuration.
        raw = aiosqlite.Connection(
            lambda: sqlite3.connect(":memory:"),
            iter_chunk_size=64,
            busy_wait=0.0,
            busy_wait_interval=0.1,
        )
        self.assertIsNone(raw._connection)
        self.assertTrue(raw._running)

    async def test_transaction_mode_and_connect_kwargs_preserved(self):
        async with aiosqlite.connect(
            self.db,
            timeout=0,
            isolation_level="IMMEDIATE",
            busy_wait=1.0,
        ) as db:
            self.assertEqual(db.isolation_level, "IMMEDIATE")
            self.assertEqual(db._busy_wait, 1.0)


class IsBusyErrorTest(IsolatedAsyncioTestCase):
    @staticmethod
    def _with_code(message: str, code: int) -> sqlite3.OperationalError:
        error = sqlite3.OperationalError(message)
        error.sqlite_errorcode = code  # type: ignore[attr-defined]
        return error

    async def test_busy_and_locked_codes(self):
        self.assertTrue(
            _is_busy_error(self._with_code("database is locked", sqlite3.SQLITE_BUSY))
        )
        self.assertTrue(
            _is_busy_error(
                self._with_code("database table is locked", sqlite3.SQLITE_LOCKED)
            )
        )
        # An extended busy code still maps to the busy primary result.
        self.assertTrue(
            _is_busy_error(
                self._with_code("database is locked", sqlite3.SQLITE_BUSY | (2 << 8))
            )
        )

    async def test_message_fallback_without_code(self):
        self.assertTrue(_is_busy_error(sqlite3.OperationalError("database is locked")))
        self.assertTrue(
            _is_busy_error(sqlite3.OperationalError("database table is locked: t"))
        )
        self.assertFalse(
            _is_busy_error(
                sqlite3.OperationalError("user-defined function raised exception")
            )
        )
        self.assertFalse(_is_busy_error(sqlite3.OperationalError("no such table: t")))

    async def test_other_errors_never_count_as_busy(self):
        self.assertFalse(
            _is_busy_error(sqlite3.IntegrityError("UNIQUE constraint failed: t.i"))
        )
        self.assertFalse(_is_busy_error(RuntimeError("database is locked")))
        self.assertFalse(_is_busy_error(ValueError("nope")))
