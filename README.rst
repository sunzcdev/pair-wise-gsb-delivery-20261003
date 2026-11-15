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


Custom functions, aggregates and window functions
--------------------------------------------------

User-defined scalar functions, aggregates and window functions are registered
asynchronously on the same worker thread that runs every query, so
registrations take effect in order relative to the queries around them:

.. code-block:: python

    class RunningTotal:
        def __init__(self):
            self.total = 0

        def step(self, value):
            self.total += value

        def inverse(self, value):
            self.total -= value

        def value(self):
            return self.total

        def finalize(self):
            return self.total

    async with aiosqlite.connect(...) as db:
        await db.create_function("double", 1, lambda x: x * 2)
        await db.create_aggregate("running_total", 1, RunningTotal)
        await db.create_window_function("running_total_win", 1, RunningTotal)

        await db.execute("SELECT running_total(column) FROM table")
        await db.execute(
            "SELECT running_total_win(column) OVER ("
            "ORDER BY column ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)"
            " FROM table"
        )

A new aggregate or window function object is created for every query, so
function state is never shared between queries. Exceptions raised by the
callback methods propagate through SQLite as standard ``sqlite3`` errors.
Window functions require Python 3.11 or newer; attempting to register one on an
older interpreter raises ``sqlite3.NotSupportedError`` at registration time.


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


License
-------

aiosqlite is copyright `Amethyst Reese <https://noswap.com>`_, and licensed under the
MIT license.  I am providing code in this repository to you under an open source
license.  This is my personal repository; the license you receive to my code
is from me and not from my employer. See the `LICENSE`_ file for details.

.. _LICENSE: https://github.com/omnilib/aiosqlite/blob/master/LICENSE
