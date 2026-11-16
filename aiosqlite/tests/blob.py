# Copyright Amethyst Reese
# Licensed under the MIT license

import os
import sqlite3
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import IsolatedAsyncioTestCase

import aiosqlite
from .helpers import setup_logger


class BlobTest(IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        setup_logger()

    def setUp(self):
        td = TemporaryDirectory()
        self.addCleanup(td.cleanup)
        self.db = Path(td.name).resolve() / "test.db"

    async def make_db(self, size=16):
        db = await aiosqlite.connect(self.db)
        self.addAsyncCleanup(db.close)
        await db.execute("create table files (id integer primary key, data blob)")
        await db.execute("insert into files (id, data) values (1, zeroblob(?))", [size])
        await db.commit()
        return db

    async def test_chunked_write_and_read(self):
        db = await self.make_db(size=10)
        async with await db.blobopen("files", "data", 1) as blob:
            self.assertIsInstance(blob, aiosqlite.Blob)
            await blob.write(b"hello")
            await blob.write(b"world")
            self.assertEqual(await blob.tell(), 10)
            await db.commit()

            await blob.seek(0)
            self.assertEqual(await blob.read(3), b"hel")
            self.assertEqual(await blob.tell(), 3)
            self.assertEqual(await blob.read(3), b"low")
            self.assertEqual(await blob.read(), b"orld")
            # EOF: reads past the end return b""
            self.assertEqual(await blob.read(4), b"")

        # persisted to the database
        async with db.execute("select data from files where id = 1") as cursor:
            (data,) = await cursor.fetchone()
            self.assertEqual(data, b"helloworld")

    async def test_zero_length_read_is_not_eof(self):
        db = await self.make_db(size=4)
        async with await db.blobopen("files", "data", 1) as blob:
            # zero-length read returns b"" without consuming anything
            self.assertEqual(await blob.read(0), b"")
            self.assertEqual(await blob.tell(), 0)
            self.assertEqual(await blob.read(2), b"\x00\x00")
            # ...while a read at EOF also returns b""
            await blob.seek(4)
            self.assertEqual(await blob.read(2), b"")

    async def test_seek_origin(self):
        db = await self.make_db(size=8)
        async with await db.blobopen("files", "data", 1) as blob:
            await blob.seek(-2, os.SEEK_END)
            self.assertEqual(await blob.tell(), 6)
            await blob.write(b"xy")
            await blob.seek(2, os.SEEK_SET)
            await blob.write(b"BC")
            await db.commit()

        async with db.execute("select data from files where id = 1") as cursor:
            (data,) = await cursor.fetchone()
            self.assertEqual(data, b"\x00\x00BC\x00\x00xy")

    async def test_independent_positions(self):
        db = await self.make_db(size=8)
        await db.execute("update files set data = ? where id = 1", [b"abcdefgh"])
        await db.commit()
        async with await db.blobopen("files", "data", 1) as blob1:
            async with await db.blobopen("files", "data", 1) as blob2:
                self.assertEqual(await blob1.read(3), b"abc")
                self.assertEqual(await blob2.read(2), b"ab")
                # positions do not leak between handles
                self.assertEqual(await blob1.tell(), 3)
                self.assertEqual(await blob2.tell(), 2)
                self.assertEqual(await blob1.read(1), b"d")
                self.assertEqual(await blob2.read(1), b"c")

    async def test_row_update_invalidates_handle(self):
        db = await self.make_db(size=8)
        async with await db.blobopen("files", "data", 1) as blob:
            await blob.read(2)
            # modifying the row expires the open blob handle
            await db.execute("update files set data = ? where id = 1", [b"new-data"])
            with self.assertRaises(sqlite3.OperationalError):
                await blob.read(2)
            # sqlite3 reports the expired handle on write as well
            # (OperationalError, or ValueError from its length pre-check)
            with self.assertRaises((ValueError, sqlite3.OperationalError)):
                await blob.write(b"zz")

    async def test_readonly_rejects_write(self):
        db = await self.make_db(size=8)
        async with await db.blobopen("files", "data", 1, readonly=True) as blob:
            self.assertEqual(await blob.read(2), b"\x00\x00")
            with self.assertRaises(sqlite3.OperationalError):
                await blob.write(b"no")

    async def test_connection_closed_error(self):
        db = await self.make_db(size=8)
        blob = await db.blobopen("files", "data", 1)
        await db.close()
        with self.assertRaisesRegex(ValueError, "Connection closed"):
            await blob.read(1)
        with self.assertRaisesRegex(ValueError, "Connection closed"):
            await blob.write(b"x")
        # closing the handle afterwards is tolerated
        await blob.close()

    async def test_closed_handle_error(self):
        db = await self.make_db(size=8)
        blob = await db.blobopen("files", "data", 1)
        await blob.close()
        with self.assertRaisesRegex(ValueError, "closed blob"):
            await blob.read(1)
        with self.assertRaisesRegex(ValueError, "closed blob"):
            await blob.write(b"x")
        with self.assertRaisesRegex(ValueError, "closed blob"):
            await blob.tell()
        # double close is a no-op
        await blob.close()
