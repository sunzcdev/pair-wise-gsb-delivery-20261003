"""
Best-effort support for reading DBF and memo files consistently.

A DBF table is made up of a ``.dbf`` file and, when the table has memo
fields, a companion memo file (``.fpt`` or ``.dbt``).  Other programs may
replace, truncate or append to either of these files while they are being
read.  Simply checking that the file name still exists after reading is not
enough: a writer can replace the file at that path with a different one.

The tools in this module pin the *identity* of a file when reading starts
and re-check it while reading.  Identity is made up of:

* the device and inode (the file itself, rather than the directory entry
  pointing to it),
* the size (so truncation and growth are detected),
* the modification time (so an in-place rewrite that keeps the same size
  is still detected).

If any of these change, :class:`FileIdentity.check` raises
:class:`~dbfread.exceptions.DBFChangedError` and the partial result must be
considered incomplete.

This is deliberately best-effort.  A writer that replaces a file and then
restores the exact size and modification time can not be distinguished from
the original without help from the operating system or the writer, so this
module does not promise a true transactional snapshot.
"""
from __future__ import absolute_import

import os
import sys
import shutil
import tempfile

from .exceptions import DBFChangedError


def open_shared(path, mode='rb'):
    """Open ``path`` for binary reading without blocking other programs.

    On Windows a normal :func:`open` denies other processes the right to
    delete or replace the file, which would make it impossible to detect
    the common "writer replaces the file" case (and could even stop the
    writer).  The file is therefore opened with share flags for reading,
    writing and deletion.  On other platforms this is just an ordinary
    binary open.

    The ``mode`` argument is accepted for drop-in compatibility with
    :func:`open`; only read-only binary mode is meaningful here.

    The file is still only ever *read*, never modified.
    """
    if sys.platform != 'win32':
        return open(path, mode)

    import ctypes
    import msvcrt
    from ctypes import wintypes

    GENERIC_READ = 0x80000000
    FILE_SHARE_READ = 0x00000001
    FILE_SHARE_WRITE = 0x00000002
    FILE_SHARE_DELETE = 0x00000004
    OPEN_EXISTING = 3
    FILE_ATTRIBUTE_NORMAL = 0x80
    INVALID_HANDLE_VALUE = wintypes.HANDLE(-1).value

    kernel32 = ctypes.windll.kernel32
    kernel32.CreateFileW.restype = wintypes.HANDLE
    kernel32.CreateFileW.argtypes = [
        wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD,
        ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD,
        wintypes.HANDLE]

    handle = kernel32.CreateFileW(
        os.path.abspath(path),
        GENERIC_READ,
        FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
        None, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, None)

    if not handle or handle == INVALID_HANDLE_VALUE:
        raise ctypes.WinError()

    fd = msvcrt.open_osfhandle(handle, os.O_RDONLY | os.O_BINARY)
    return os.fdopen(fd, 'rb')


class FileIdentity(object):
    """The pinned identity and structure information of one file.

    Create one with :meth:`pin` when reading starts and call
    :meth:`check` before and after the file is used to make sure it has
    not been replaced or changed.
    """

    def __init__(self, path, stat_result):
        self.path = path
        self.device = stat_result.st_dev
        self.inode = stat_result.st_ino
        self.size = stat_result.st_size
        self.mtime_ns = stat_result.st_mtime_ns

    @classmethod
    def pin(cls, path):
        """Pin the identity of the file at ``path``.

        Raises :class:`~dbfread.exceptions.DBFChangedError` if the file
        disappears instead of the lower level ``OSError`` so callers only
        have to handle one kind of consistency failure.
        """
        try:
            stat_result = os.stat(path)
        except OSError as error:
            raise DBFChangedError(
                '{} is no longer available: {}'.format(path, error))
        return cls(path, stat_result)

    def check(self):
        """Raise if the file no longer matches the pinned identity.

        This detects the file being removed or replaced (different device
        or inode), truncated or grown (different size), or rewritten in
        place (different modification time).
        """
        try:
            current = os.stat(self.path)
        except OSError as error:
            raise DBFChangedError(
                '{} was removed or replaced while it was being read: '
                '{}'.format(self.path, error))

        if (current.st_dev, current.st_ino) != (self.device, self.inode):
            raise DBFChangedError(
                '{} was replaced by a different file while it was being '
                'read; the result is incomplete'.format(self.path))

        if current.st_size != self.size:
            raise DBFChangedError(
                '{} changed size from {} to {} bytes while it was being '
                'read; the result is incomplete'.format(
                    self.path, self.size, current.st_size))

        if current.st_mtime_ns != self.mtime_ns:
            raise DBFChangedError(
                '{} was modified while it was being read; the result is '
                'incomplete'.format(self.path))

    def __repr__(self):
        return 'FileIdentity(path={!r}, size={!r})'.format(
            self.path, self.size)


def copy_file_verified(source, destination):
    """Copy ``source`` to ``destination``, checking the source twice.

    The identity of the source file is pinned before the copy and checked
    again afterwards.  If the source was replaced, truncated or otherwise
    changed while it was being copied the partially written destination is
    removed and :class:`~dbfread.exceptions.DBFChangedError` is raised.

    The source file is only opened for reading and is never modified.
    """
    identity = FileIdentity.pin(source)

    try:
        shutil.copyfile(source, destination)
    except OSError as error:
        # Remove anything that was written so a half-copied file is never
        # left behind in the private temporary directory.
        if os.path.exists(destination):
            os.remove(destination)
        raise DBFChangedError(
            'could not copy {}: {}'.format(source, error))

    try:
        identity.check()
    except DBFChangedError:
        if os.path.exists(destination):
            os.remove(destination)
        raise

    if os.path.getsize(destination) != identity.size:
        if os.path.exists(destination):
            os.remove(destination)
        raise DBFChangedError(
            'the copy of {} has the wrong size; the source changed while '
            'it was being copied'.format(source))


def make_temp_dir(prefix='dbfread-'):
    """Create a private temporary directory for verified copies."""
    return tempfile.mkdtemp(prefix=prefix)


def remove_temp_dir(path):
    """Best-effort removal of a temporary directory created by dbfread."""
    if path and os.path.isdir(path):
        shutil.rmtree(path, ignore_errors=True)


__all__ = ['FileIdentity', 'copy_file_verified', 'make_temp_dir',
           'remove_temp_dir', 'open_shared']
