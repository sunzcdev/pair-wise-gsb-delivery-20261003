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

Spooled Temporary File
----------------------

:class:`SpooledTemporaryFile` is useful when temporary data is small and should be kept in memory rather than written to disk.
Once the buffered data grows past ``max_size`` (or :meth:`~SpooledTemporaryFile.rollover` is called explicitly), the contents are handed off to an on-disk temporary file.

The memory-to-disk handoff follows an explicit, all-or-nothing protocol whose current
stage is exposed by :attr:`~SpooledTemporaryFile.rollover_phase`:

- ``SpooledTemporaryFile.RolloverPhase.SPOOLED``: the data lives in the in-memory buffer.
- ``SpooledTemporaryFile.RolloverPhase.ROLLING_OVER``: a handoff is in progress. The
  in-memory buffer remains the authoritative copy while the disk file is being populated.
- ``SpooledTemporaryFile.RolloverPhase.ROLLED_OVER``: the handoff has completed and the
  disk file, holding exactly the same contents, is now authoritative.

While a handoff is in progress every other read or write waits for it to settle, so a
concurrent reader can never observe a partially copied disk stream. If creating or
populating the disk file fails, the partial disk file is discarded, the phase returns to
``SPOOLED`` and the rollover can be retried; on success the original cursor position is
preserved.

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
