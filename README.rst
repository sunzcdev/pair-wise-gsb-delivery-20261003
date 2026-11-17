aiosqlite\: Sqlite for AsyncIO
==============================

.. image:: https://readthedocs.org/projects/aiosqlite/badge/?version=latest
   :target: https://aiosqlite.omnilib.dev/en/latest/?badge=latest
   :alt: Documentation Status
.. image:: https://img.shields.io/pypi/v/aiosqlite.svg
   :target: https://pypi.org/project/aiosqlite
   :alt: PyPI Release
.. image:: https://img.shields.io/badge/change-log-blue
   :target: https://github.com/omnilib/aiosqlite/blob/master/CHANGELOG.md
   :alt: Changelog
.. image:: https://img.shields.io/pypi/l/aiosqlite.svg
   :target: https://github.com/omnilib/aiosqlite/blob/master/LICENSE
   :alt: MIT Licensed

aiosqlite provides a friendly, async interface to sqlite databases.

It replicates the standard ``sqlite3`` module, but with async versions
of all the standard connection and cursor methods, plus context managers for
automatically closing connections and cursors:

.. code-block:: python

    async with aiosqlite.connect(...) as db:
        await db.execute("INSERT INTO some_table ...")
        await db.commit()

        async with db.execute("SELECT * FROM some_table") as cursor:
            async for row in cursor:
                ...

It can also be used in the traditional, procedural manner:

.. code-block:: python

    db = await aiosqlite.connect(...)
    cursor = await db.execute('SELECT * FROM some_table')
    row = await cursor.fetchone()
    rows = await cursor.fetchall()
    await cursor.close()
    await db.close()

aiosqlite also replicates most of the advanced features of ``sqlite3``:

.. code-block:: python

    async with aiosqlite.connect(...) as db:
        db.row_factory = aiosqlite.Row
        async with db.execute('SELECT * FROM some_table') as cursor:
            async for row in cursor:
                value = row['column']

        await db.execute('INSERT INTO foo some_table')
        assert db.total_changes > 0


Install
-------

aiosqlite is compatible with Python 3.8 and newer.
You can install it from PyPI:

.. code-block:: console

    $ pip install aiosqlite


Details
-------

aiosqlite allows interaction with SQLite databases on the main AsyncIO event
loop without blocking execution of other coroutines while waiting for queries
or data fetches.  It does this by using a single, shared thread per connection.
This thread executes all actions within a shared request queue to prevent
overlapping actions.

Connection objects are proxies to the real connections, contain the shared
execution thread, and provide context managers to handle automatically closing
connections.  Cursors are similarly proxies to the real cursors, and provide
async iterators to query results.


Asynchronous lock waiting
~~~~~~~~~~~~~~~~~~~~~~~~~

SQLite serializes writers with database-level locks. When another connection
briefly holds the lock, a statement fails with ``SQLITE_BUSY``
(``"database is locked"``) or ``SQLITE_LOCKED``
(``"database table is locked"``). The synchronous ``timeout`` parameter
(SQLite's own busy handler) makes the connection's single worker thread block,
so no other statement on that connection can make progress while it waits.

As an alternative, ``aiosqlite.connect()`` accepts an optional ``busy_wait``
budget (in seconds). With it enabled, a statement executed through
``Connection.execute()`` or ``Cursor.execute()`` that fails with a
recognizable busy/locked result is retried on the event loop, sleeping for
``busy_wait_interval`` seconds between attempts, until it succeeds or the
total deadline is reached. Waiting never blocks the worker thread, so the
event loop and the connection stay responsive, and the wait can be
cancelled like any other coroutine:

.. code-block:: python

    async with aiosqlite.connect(
        "example.db",
        timeout=0,          # let busy_wait own the waiting, do not block the thread
        busy_wait=5.0,      # retry for at most five seconds in total
        busy_wait_interval=0.1,
    ) as db:
        await db.execute("INSERT INTO some_table ...")

Only statements that have produced neither rows nor write side effects are
retried, so helpers such as ``executemany()``, ``executescript()``,
``execute_insert()`` and fetch operations simply propagate the error rather
than being blindly retried. Integrity constraint violations (for example
``UNIQUE`` failures) and errors raised by user-defined functions are never
treated as lock contention. Transaction modes and every other
``sqlite3.connect()`` parameter are passed through unchanged. When
``busy_wait`` is left at its default of ``0.0``, behavior is unchanged and
busy/locked errors propagate immediately.


License
-------

aiosqlite is copyright `Amethyst Reese <https://noswap.com>`_, and licensed under the
MIT license.  I am providing code in this repository to you under an open source
license.  This is my personal repository; the license you receive to my code
is from me and not from my employer. See the `LICENSE`_ file for details.

.. _LICENSE: https://github.com/omnilib/aiosqlite/blob/master/LICENSE
