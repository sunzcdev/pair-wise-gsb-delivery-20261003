.. highlight:: python

Reading Files That May Change
=============================

A DBF table is stored in two files: the ``.dbf`` file and, when the
table has memo fields, a companion memo file (``.fpt`` or ``.dbt``).
In a live system another program may replace, truncate or append to
either of these files while dbfread is streaming records from them.

By default dbfread simply reads whatever is on disk at the moment each
record is needed.  This keeps the old, fast behaviour unchanged, but it
means a file that is swapped out halfway through an iteration can give
you records from two different tables or fail with an obscure parsing
error.

Two optional keyword arguments add protection against this.  Both are
off by default, so existing code behaves exactly as before.


Pinning the Files: ``consistent=True``
--------------------------------------

Pass ``consistent=True`` to pin the *identity* of both files when the
table is opened::

    >>> table = DBF('people.dbf', consistent=True)
    >>> for record in table:
    ...     print(record)

For each file dbfread records the device and inode (which identify the
file itself, not just the directory entry), the size and the
modification time.  This identity is re-checked before every record and
once more when the end of the file is reached.  The number of records
is also compared with the count stored in the header at open time.

If the file is removed, replaced by a different file, truncated, grown
or rewritten in place, dbfread raises
:class:`~dbfread.DBFChangedError` instead of silently mixing old and
new data::

    >>> from dbfread import DBFChangedError
    >>> try:
    ...     records = list(DBF('people.dbf', consistent=True))
    ... except DBFChangedError as error:
    ...     print('incomplete read:', error)

Any records returned before the exception is raised must be treated as
incomplete.  The exception message always says so.

This is deliberately a *best-effort* check.  Without co-operation from
the writer (or an operating system snapshot facility) there is no way
to obtain a true transactional snapshot.  In particular, a writer that
replaces a file and then restores the exact same size and modification
time is indistinguishable from the original.  dbfread does not pretend
otherwise.


Copying First: ``copy_to_temp=True``
------------------------------------

For stronger isolation, pass ``copy_to_temp=True``::

    >>> with DBF('people.dbf', copy_to_temp=True) as table:
    ...     records = list(table)

dbfread then:

1. parses the structure of the original DBF file,
2. creates a private temporary directory,
3. copies the DBF file and the memo file into it, pinning each source
   file's identity before the copy and verifying it again afterwards,
4. reads all records from the copies.

If a source file is replaced or truncated while it is being copied the
verification after the copy fails, the half-written copy is deleted and
``DBFChangedError`` is raised.  Once the copy is complete nobody else
knows where it is, so later changes to the original files can not
affect the read.

``copy_to_temp=True`` implies ``consistent=True``.  The original files
are only ever opened for reading and are never modified or deleted.


Cleaning Up
-----------

When reading from a temporary copy the temporary directory is removed:

* when you call ``table.close()``,
* automatically at the end of a ``with`` block,
* if anything goes wrong while the table is being constructed (for
  example a parsing error), so no half-copied files are left behind.

Tables that read directly from the original files have nothing to
clean up; ``close()`` is still accepted for a uniform API.  After a
table has been closed, iterating or counting its records raises
``DBFChangedError``.

The temporary directory is also removed in the object's destructor as a
last resort if a table is garbage collected without being closed.


What This Does Not Guarantee
----------------------------

Neither option is a substitute for a real snapshot taken by the
operating system or by the writing application:

* ``consistent=True`` can not notice a change that leaves the device,
  inode, size and modification time untouched.
* ``copy_to_temp=True`` protects against everything that happens after
  the copy, but a change during the copy itself is detected and
  reported rather than hidden.  The two files are copied one after the
  other, so they are not guaranteed to represent the same instant in
  time if the writer does not co-operate.

If you need an exact point-in-time view, have the writing application
publish an atomic snapshot (for example by writing new files and
renaming them into place) or take an operating system level snapshot
and point dbfread at that.
