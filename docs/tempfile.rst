Asynchronous Temporary File and Directory
=========================================

.. py:currentmodule:: anyio

This module provides asynchronous wrappers for handling temporary files and directories
using the :mod:`tempfile` module. The asynchronous methods execute blocking operations in worker threads.

Temporary File
--------------

:class:`TemporaryFile` creates a temporary file that is automatically deleted upon closure.

**Example:**

.. literalinclude:: snippets/tempfile/temporary_file.py

Named Temporary File
--------------------

:class:`NamedTemporaryFile` works similarly to :class:`TemporaryFile`, but the file has a visible name in the filesystem.

**Example:**

.. literalinclude:: snippets/tempfile/named_temporary_file.py

Publishing a Named Temporary File
---------------------------------

A named temporary file can be promoted to a permanent file with
:meth:`NamedTemporaryFile.publish`. This is particularly useful on Windows, where a
temporary file opened with delete-on-close semantics cannot simply be renamed while it
is open.

The ``publish()`` method waits for any writes already accepted by the file object to
finish, then transfers the contents to the target path. The transfer is shielded from
cancellation once it has started, and on Windows the contents are copied via a staging
file in the target directory which is then atomically renamed, so the target path never
contains a half-written file. If publishing fails or is cancelled while waiting for
pending writes, the temporary file is still cleaned up when the context manager exits.
Once publishing has succeeded, exiting the context manager no longer deletes the
published file.

**Example:**

.. literalinclude:: snippets/tempfile/publish_named_temporary_file.py

Spooled Temporary File
----------------------

:class:`SpooledTemporaryFile` is useful when temporary data is small and should be kept in memory rather than written to disk.

**Example:**

.. literalinclude:: snippets/tempfile/spooled_temporary_file.py

Temporary Directory
-------------------

The :class:`TemporaryDirectory` provides an asynchronous way to create temporary directories.

**Example:**

.. literalinclude:: snippets/tempfile/temporary_directory.py

Low-Level Temporary File and Directory Creation
-----------------------------------------------

For more control, the module provides lower-level functions:

- :func:`mkstemp` - Creates a temporary file and returns a tuple of file descriptor and path.
- :func:`mkdtemp` - Creates a temporary directory and returns the directory path.
- :func:`gettempdir` - Returns the path of the default temporary directory.
- :func:`gettempdirb` - Returns the path of the default temporary directory in bytes.

**Example:**

.. literalinclude:: snippets/tempfile/mkstemp_mkdtemp.py

.. note::
    Using these functions requires manual cleanup of the created files and directories.

.. seealso::

    - Python Standard Library: :mod:`tempfile` (`official documentation <https://docs.python.org/3/library/tempfile.html>`_)
