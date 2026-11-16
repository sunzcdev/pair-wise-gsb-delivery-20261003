# Copyright (c) 2013, Mahmoud Hashemi
#
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions are
# met:
#
#    * Redistributions of source code must retain the above copyright
#      notice, this list of conditions and the following disclaimer.
#
#    * Redistributions in binary form must reproduce the above
#      copyright notice, this list of conditions and the following
#      disclaimer in the documentation and/or other materials provided
#      with the distribution.
#
#    * The names of the contributors may not be used to endorse or
#      promote products derived from this software without specific
#      prior written permission.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
# "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
# LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
# A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
# OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
# SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
# LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
# DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
# THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
# (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
# OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.


# Coding decl above needed for rendering the emdash properly in the
# documentation.

"""
Module ``ioutils`` implements a number of helper classes and functions which
are useful when dealing with input, output, and bytestreams in a variety of
ways.
"""
import os
from io import BytesIO, IOBase
from abc import (
    ABCMeta,
    abstractmethod,
    abstractproperty,
)
from bisect import bisect_right
from errno import EINVAL
from codecs import EncodedFile
from tempfile import TemporaryFile
from itertools import zip_longest

READ_CHUNK_SIZE = 21333
"""
Number of bytes to read at a time. The value is ~ 1/3rd of 64k which means that
the value will easily fit in the L2 cache of most processors even if every
codepoint in a string is three bytes long which makes it a nice fast default
value.
"""


class SpooledIOBase(IOBase):
    """
    A base class shared by the SpooledBytesIO and SpooledStringIO classes.

    The SpooledTemporaryFile class is missing several attributes and methods
    present in the StringIO implementation. This brings the api as close to
    parity as possible so that classes derived from SpooledIOBase can be used
    as near drop-in replacements to save memory.
    """
    __metaclass__ = ABCMeta

    def __init__(self, max_size=5000000, dir=None):
        self._max_size = max_size
        self._dir = dir

    def _checkClosed(self, msg=None):
        """Raise a ValueError if file is closed"""
        if self.closed:
            raise ValueError('I/O operation on closed file.'
                             if msg is None else msg)
    @abstractmethod
    def read(self, n=-1):
        """Read n characters from the buffer"""

    @abstractmethod
    def write(self, s):
        """Write into the buffer"""

    @abstractmethod
    def seek(self, pos, mode=0):
        """Seek to a specific point in a file"""

    @abstractmethod
    def readline(self, length=None):
        """Returns the next available line"""

    @abstractmethod
    def readlines(self, sizehint=0):
        """Returns a list of all lines from the current position forward"""

    def writelines(self, lines):
        """
        Write lines to the file from an interable.

        NOTE: writelines() does NOT add line separators.
        """
        self._checkClosed()
        for line in lines:
            self.write(line)

    @abstractmethod
    def rollover(self):
        """Roll file-like-object over into a real temporary file"""

    @abstractmethod
    def tell(self):
        """Return the current position"""

    @abstractproperty
    def buffer(self):
        """Should return a flo instance"""

    @abstractproperty
    def _rolled(self):
        """Returns whether the file has been rolled to a real file or not"""

    @abstractproperty
    def len(self):
        """Returns the length of the data"""

    def _get_softspace(self):
        return self.buffer.softspace

    def _set_softspace(self, val):
        self.buffer.softspace = val

    softspace = property(_get_softspace, _set_softspace)

    @property
    def _file(self):
        return self.buffer

    def close(self):
        return self.buffer.close()

    def flush(self):
        self._checkClosed()
        return self.buffer.flush()

    def isatty(self):
        self._checkClosed()
        return self.buffer.isatty()

    @property
    def closed(self):
        return self.buffer.closed

    @property
    def pos(self):
        return self.tell()

    @property
    def buf(self):
        return self.getvalue()

    def fileno(self):
        self.rollover()
        return self.buffer.fileno()

    def truncate(self, size=None):
        """
        Truncate the contents of the buffer.

        Custom version of truncate that takes either no arguments (like the
        real SpooledTemporaryFile) or a single argument that truncates the
        value to a certain index location. Like :meth:`io.IOBase.truncate`,
        the current position is left unchanged.
        """
        self._checkClosed()
        if size is None:
            return self.buffer.truncate()

        if size < 0:
            raise OSError(EINVAL, "Negative size not allowed")

        # Emulate truncation to a particular location
        pos = self.tell()
        self.seek(size)
        self.buffer.truncate()
        self.seek(pos)

    def getvalue(self):
        """Return the entire files contents."""
        self._checkClosed()
        pos = self.tell()
        self.seek(0)
        val = self.read()
        self.seek(pos)
        return val

    def seekable(self):
        return True

    def readable(self):
        return True

    def writable(self):
        return True

    def __next__(self):
        self._checkClosed()
        line = self.readline()
        if not line:
            pos = self.buffer.tell()
            self.buffer.seek(0, os.SEEK_END)
            if pos == self.buffer.tell():
                raise StopIteration
            else:
                self.buffer.seek(pos)
        return line

    next = __next__

    def __len__(self):
        return self.len

    def __iter__(self):
        self._checkClosed()
        return self

    def __enter__(self):
        self._checkClosed()
        return self

    def __exit__(self, *args):
        self._file.close()

    def __eq__(self, other):
        if isinstance(other, self.__class__):
            self_pos = self.tell()
            other_pos = other.tell()
            try:
                self.seek(0)
                other.seek(0)
                eq = True
                for self_line, other_line in zip_longest(self, other):
                    if self_line != other_line:
                        eq = False
                        break
                self.seek(self_pos)
                other.seek(other_pos)
            except Exception:
                # Attempt to return files to original position if there were any errors
                try:
                    self.seek(self_pos)
                except Exception:
                    pass
                try:
                    other.seek(other_pos)
                except Exception:
                    pass
                raise
            else:
                return eq
        return False

    def __ne__(self, other):
        return not self.__eq__(other)

    def __bool__(self):
        return True

    def __del__(self):
        """Can fail when called at program exit so suppress traceback."""
        try:
            self.close()
        except Exception:
            pass


class SpooledBytesIO(SpooledIOBase):
    """
    SpooledBytesIO is a spooled file-like object accepting bytes-like objects,
    including bytes, bytearray, and memoryview. Writes return the number of
    bytes written. Bytes are retrieved exactly as given; writing text raises
    TypeError.

    Example::

        >>> from boltons import ioutils
        >>> with ioutils.SpooledBytesIO() as f:
        ...     _ = f.write(b"Happy IO")
        ...     _ = f.seek(0)
        ...     isinstance(f.getvalue(), bytes)
        True
    """

    def read(self, n=-1):
        self._checkClosed()
        return self.buffer.read(n)

    def write(self, s):
        self._checkClosed()
        with memoryview(s) as view:
            if self.tell() + view.nbytes >= self._max_size:
                self.rollover()
            return self.buffer.write(view)

    def seek(self, pos, mode=0):
        self._checkClosed()
        return self.buffer.seek(pos, mode)

    def readline(self, length=None):
        self._checkClosed()
        if length is not None:
            return self.buffer.readline(length)
        else:
            return self.buffer.readline()

    def readlines(self, sizehint=0):
        return self.buffer.readlines(sizehint)

    def rollover(self):
        """Roll the StringIO over to a TempFile"""
        if not self._rolled:
            tmp = TemporaryFile(dir=self._dir)
            pos = self.buffer.tell()
            tmp.write(self.buffer.getvalue())
            tmp.seek(pos)
            self.buffer.close()
            self._buffer = tmp

    @property
    def _rolled(self):
        return not isinstance(self.buffer, BytesIO)

    @property
    def buffer(self):
        try:
            return self._buffer
        except AttributeError:
            self._buffer = BytesIO()
        return self._buffer

    @property
    def len(self):
        """Determine the length of the file"""
        pos = self.tell()
        if self._rolled:
            self.seek(0)
            val = os.fstat(self.fileno()).st_size
        else:
            self.seek(0, os.SEEK_END)
            val = self.tell()
        self.seek(pos)
        return val

    def tell(self):
        self._checkClosed()
        return self.buffer.tell()


class SpooledStringIO(SpooledIOBase):
    """
    SpooledStringIO is a spooled file-like-object that only accepts unicode
    values. On Python 2.x this means the 'unicode' type and on Python 3.x this
    means the 'str' type. Values are accepted as unicode and then coerced into
    utf-8 encoded bytes for storage. On retrieval, the values are returned as
    unicode.

    Example::

        >>> from boltons import ioutils
        >>> with ioutils.SpooledStringIO() as f:
        ...     _ = f.write(u"\u2014 Hey, an emdash!")
        ...     _ = f.seek(0)
        ...     isinstance(f.read(), str)
        True

    """
    def __init__(self, *args, **kwargs):
        self._tell = 0
        super().__init__(*args, **kwargs)

    def read(self, n=-1):
        self._checkClosed()
        ret = self.buffer.reader.read(n, n)
        self._tell = self.tell() + len(ret)
        return ret

    def write(self, s):
        self._checkClosed()
        if not isinstance(s, str):
            raise TypeError("str expected, got {}".format(
                type(s).__name__
            ))
        current_pos = self.tell()
        if self.buffer.tell() + len(s.encode('utf-8')) >= self._max_size:
            self.rollover()
        self.buffer.write(s.encode('utf-8'))
        self._tell = current_pos + len(s)
        return len(s)

    def _traverse_codepoints(self, current_position, n):
        """Traverse from current position to the right n codepoints"""
        dest = current_position + n
        while True:
            if current_position == dest:
                # By chance we've landed on the right position, break
                break

            # If the read would take us past the intended position then
            # seek only enough to cover the offset
            if current_position + READ_CHUNK_SIZE > dest:
                self.read(dest - current_position)
                break
            else:
                ret = self.read(READ_CHUNK_SIZE)

            # Increment our current position
            current_position += READ_CHUNK_SIZE

            # If we kept reading but there was nothing here, break
            # as we are at the end of the file
            if not ret:
                break

        return dest

    def seek(self, pos, mode=0):
        """Traverse from offset to the specified codepoint"""
        self._checkClosed()
        # Seek to position from the start of the file
        if mode == os.SEEK_SET:
            if pos < 0:
                raise ValueError("negative seek position")
            self.buffer.seek(0)
            self._traverse_codepoints(0, pos)
            self._tell = pos
        # Seek to new position relative to current position
        elif mode == os.SEEK_CUR:
            start_pos = self.tell()
            destination = start_pos + pos
            if destination < 0:
                raise ValueError("negative seek position")
            if pos < 0:
                # UTF-8 offsets count characters, so traverse from the start.
                return self.seek(destination)
            self._traverse_codepoints(start_pos, pos)
            self._tell = destination
        elif mode == os.SEEK_END:
            dest_position = self.len + pos
            if dest_position < 0:
                raise ValueError("negative seek position")
            return self.seek(dest_position)
        else:
            raise ValueError(
                f"Invalid whence ({mode}, should be 0, 1, or 2)"
            )
        return self.tell()

    def readline(self, length=None):
        self._checkClosed()
        if length == 0:
            # codecs.StreamReader.readline treats size=0 as "no limit"
            return ''
        ret = self.buffer.readline(length).decode('utf-8')
        self._tell = self.tell() + len(ret)
        return ret

    def readlines(self, sizehint=0):
        ret = [x.decode('utf-8') for x in self.buffer.readlines(sizehint)]
        self._tell = self.tell() + sum(len(x) for x in ret)
        return ret

    @property
    def buffer(self):
        try:
            return self._buffer
        except AttributeError:
            self._buffer = EncodedFile(BytesIO(), data_encoding='utf-8')
        return self._buffer

    @property
    def _rolled(self):
        return not isinstance(self.buffer.stream, BytesIO)

    def rollover(self):
        """Roll the buffer over to a TempFile"""
        if not self._rolled:
            tmp = EncodedFile(TemporaryFile(dir=self._dir),
                              data_encoding='utf-8')
            char_pos = self._tell
            tmp.write(self.buffer.getvalue())
            self.buffer.close()
            self._buffer = tmp
            self.seek(char_pos)

    def tell(self):
        """Return the codepoint position"""
        self._checkClosed()
        return self._tell

    @property
    def len(self):
        """Determine the number of codepoints in the file"""
        pos = self.buffer.tell()
        self.buffer.seek(0)
        total = 0
        while True:
            ret = self.buffer.reader.read(READ_CHUNK_SIZE, READ_CHUNK_SIZE)
            if not ret:
                break
            total += len(ret)
        self.buffer.seek(pos)
        return total


def is_text_fileobj(fileobj):
    if getattr(fileobj, 'encoding', False):
        # codecs.open and io.TextIOBase
        return True
    if getattr(fileobj, 'getvalue', False):
        # StringIO.StringIO / io.StringIO
        try:
            if isinstance(fileobj.getvalue(), str):
                return True
        except Exception:
            pass
    return False


class MultiFileReader:
    """Takes a list of open files or file-like objects and provides an
    interface to read from them all contiguously. Like
    :func:`itertools.chain()`, but for reading files.

       >>> mfr = MultiFileReader(BytesIO(b'ab'), BytesIO(b'cd'), BytesIO(b'e'))
       >>> mfr.read(3).decode('ascii')
       u'abc'
       >>> mfr.read(3).decode('ascii')
       u'de'

    The constructor takes as many fileobjs as you hand it, and will
    raise a TypeError on non-file-like objects. A ValueError is raised
    when file-like objects are a mix of bytes- and text-handling
    objects (for instance, BytesIO and StringIO).

    In addition to sequential reading, a logical, contiguous byte
    (or character) stream is exposed through :meth:`seek` and
    :meth:`tell`. A lazily-built index of the fixed-length sources
    makes it possible to position the cursor at any logical offset
    without rereading any of the data, and empty files are spanned
    transparently. The underlying files are borrowed: they are never
    closed by the reader. If a source grows or shrinks after the index
    has been built, the stale index is rejected with a ValueError.
    """

    def __init__(self, *fileobjs):
        if not all([callable(getattr(f, 'read', None)) and
                    callable(getattr(f, 'seek', None)) for f in fileobjs]):
            raise TypeError('MultiFileReader expected file-like objects'
                            ' with .read() and .seek()')
        if all([is_text_fileobj(f) for f in fileobjs]):
            # codecs.open and io.TextIOBase
            self._joiner = ''
        elif any([is_text_fileobj(f) for f in fileobjs]):
            raise ValueError('All arguments to MultiFileReader must handle'
                             ' bytes OR text, not a mix')
        else:
            # open/file and io.BytesIO
            self._joiner = b''
        self._fileobjs = fileobjs
        self._index = 0
        # Logical offset at which each file starts and its length when
        # indexed. Both stay None until the index is lazily built.
        self._offsets = None
        self._lengths = None
        self._size = None

    @staticmethod
    def _get_len(fileobj):
        """Return the fixed length of *fileobj* without reading its
        contents. The size is taken with seek/tell and the cursor is
        restored to exactly where it was.
        """
        cur = fileobj.tell()
        fileobj.seek(0, os.SEEK_END)
        length = fileobj.tell()
        fileobj.seek(cur)
        return length

    def _build_index(self):
        """Build the shard index from the current lengths of the
        sources. No data is read, and every cursor is left exactly
        where it was found.
        """
        lengths = [self._get_len(f) for f in self._fileobjs]
        offsets = []
        running = 0
        for length in lengths:
            offsets.append(running)
            running += length
        self._lengths = lengths
        self._offsets = offsets
        self._size = running

    def _check_index(self):
        """Ensure a built index still matches the sources. Reusing an
        index after any source has changed length would silently
        misposition reads, so raise a ValueError instead. Only lengths
        are revisited; contents are never read and every cursor is
        returned to its prior position.
        """
        if self._lengths is None:
            self._build_index()
            return
        for idx, fileobj in enumerate(self._fileobjs):
            if self._get_len(fileobj) != self._lengths[idx]:
                raise ValueError(
                    'MultiFileReader source length changed; index is stale.'
                    ' Construct a new MultiFileReader instead of reusing one'
                    ' whose files have been grown or truncated')

    def read(self, amt=None):
        """Read up to the specified *amt*, seamlessly bridging across
        files. Returns the appropriate type of string (bytes or text)
        for the input, and returns an empty string when the files are
        exhausted.
        """
        if amt is None or amt < 0:
            ret = self._joiner.join(f.read()
                                    for f in self._fileobjs[self._index:])
            self._index = len(self._fileobjs)
            return ret
        parts = []
        while amt > 0 and self._index < len(self._fileobjs):
            parts.append(self._fileobjs[self._index].read(amt))
            got = len(parts[-1])
            if got < amt:
                self._index += 1
            amt -= got
        return self._joiner.join(parts)

    def tell(self):
        """Return the current logical position across the concatenated
        stream, spanning empty files as needed.
        """
        self._check_index()
        if self._index >= len(self._fileobjs):
            return self._size
        return self._offsets[self._index] + self._fileobjs[self._index].tell()

    def seek(self, offset, whence=os.SEEK_SET):
        """Set the logical cursor position and return the resulting
        absolute offset.

        Supports all three standard modes: ``os.SEEK_SET`` for an
        absolute offset, ``os.SEEK_CUR`` for an offset relative to the
        current position, and ``os.SEEK_END`` for one measured back
        from the end of the logical stream. Offsets landing past the
        end are clamped to the end of the stream, matching ordinary
        file behavior. Seeking to ``0`` keeps working exactly as it
        always has.
        """
        self._check_index()
        if whence == os.SEEK_SET:
            target = offset
        elif whence == os.SEEK_CUR:
            target = self.tell() + offset
        elif whence == os.SEEK_END:
            target = self._size + offset
        else:
            raise ValueError(
                'invalid whence for MultiFileReader.seek(): %r' % (whence,))
        if target < 0:
            raise ValueError('negative seek position %r' % (target,))
        if target > self._size:
            target = self._size

        # The rightmost file starting at or below the target owns it.
        # bisect_right naturally skips empty shards sharing an offset.
        idx = bisect_right(self._offsets, target) - 1
        if idx < 0:
            # No files at all; nothing to reposition.
            return target
        # Rewind every borrowed shard before positioning: shards read to
        # EOF in a previous pass are otherwise still parked there and a
        # later read bridging forward from an earlier shard would get
        # nothing from them. This also preserves the historical
        # seek(0) behavior of rewinding every underlying file.
        for fileobj in self._fileobjs:
            fileobj.seek(0)
        local = target - self._offsets[idx]
        self._fileobjs[idx].seek(local)
        self._index = idx
        return target
