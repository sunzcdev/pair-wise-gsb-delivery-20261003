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


Diagnosing user-defined functions
---------------------------------

SQLite reports every callback failure as the same opaque
``OperationalError("user-defined function raised exception")``.  Enable
diagnostics on the connection to keep the real Python failure attached:

.. code-block:: python

    import sqlite3

    async with aiosqlite.connect(...) as db:
        await db.set_function_diagnostics(True)

        def parse_amount(value):
            return float(value)  # raises ValueError on garbage input

        await db.create_function("parse_amount", 1, parse_amount)

        try:
            await db.execute("SELECT parse_amount(?)", ["not a number"])
        except sqlite3.OperationalError as exc:
            failure = exc.udf_failure
            print(failure.function)   # parse_amount
            print(failure.phase)      # call
            print(failure.exception)  # could not convert string to float

The raised exception is still a normal ``sqlite3`` error, so existing
``except sqlite3.Error`` handlers keep working.  ``exc.udf_diagnostics`` holds
one record per callback failure seen during the operation, and the original
callback exception is set as ``exc.__cause__`` so a normal traceback walks from
the failing statement into your code.  Callback argument values and statement
text are not recorded unless ``capture_values`` / ``capture_sql`` are passed
explicitly, and enabling diagnostics never re-runs the callback or changes the
number of rows a statement affects.

Failures are split by phase: ``UDF_PHASE_CALL`` means the callback raised, while
``UDF_PHASE_RETURN`` means it returned a value SQLite cannot store.  Each
statement starts with a fresh record set, so a failure is never carried over to
the next query.

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
