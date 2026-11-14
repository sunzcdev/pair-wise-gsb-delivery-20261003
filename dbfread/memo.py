"""
Reads data from FPT (memo) files.

FPT files are used to varying lenght text or binary data which is too
large to fit in a DBF field.

VFP == Visual FoxPro
DB3 == dBase III
DB4 == dBase IV

Each format has its own block size and way of ending a memo:

* dBase III uses a fixed block size of 512 bytes and terminates memos
  with \\x1a.
* dBase IV stores the block size in the file header and prefixes each
  memo with an 8 byte header containing the memo length.
* Visual FoxPro stores the block size in the file header and prefixes
  each block with a type and a length.

If ``strict=True`` is passed to the memo file (via
``DBF(..., strict_memo=True)``) references from the DBF file are
checked before they are followed, and one of the following exceptions
is raised if something is wrong:

* ``MemoBlockOutOfRange`` -- the block number points outside the file
* ``MemoHeaderCorrupt`` -- the block header is damaged
* ``MemoTruncated`` -- the memo data ends prematurely

An empty reference (block number 0) is not an error and returns
``None`` in both modes.  The default (``strict=False``) keeps the
historic, lenient behaviour.
"""
import os

from .ifiles import ifind
from .struct_parser import StructParser
from .exceptions import (MemoBlockOutOfRange, MemoHeaderCorrupt,
                         MemoTruncated)


VFPFileHeader = StructParser(
    'FPTHeader',
    '>LHH504s',
    ['nextblock',
     'reserved1',
     'blocksize',
     'reserved2'])

VFPMemoHeader = StructParser(
    'FoxProMemoHeader',
    '>LL',
    ['type',
     'length'])

DB4MemoHeader = StructParser(
    'DBase4MemoHeader',
    '<LL',
    ['reserved',  # Always 0xff 0xff 0x08 0x08.
     'length'])

# The 'reserved' field of a dBase IV memo header always contains the
# bytes 0xff 0xff 0x08 0x08, which read as a little endian uint32 is:
DB4_MEMO_SIGNATURE = 0x0808ffff

DB4FileHeader = StructParser(
    'DB4FileHeader',
    '<L16sH490s',
    ['nextblock',
     'reserved1',
     'blocksize',
     'reserved2'])

# dBase III memo files always use this block size.  It is also the
# default for dBase IV files that store 0 in the header.
DB3_BLOCK_SIZE = 512


class VFPMemo(bytes):
    pass


# Used for Visual FoxPro memos to distinguish binary from text memos.
class BinaryMemo(VFPMemo):
    pass


class PictureMemo(BinaryMemo):
    pass


class ObjectMemo(BinaryMemo):
    pass


class TextMemo(VFPMemo):
    pass


VFP_TYPE_MAP = {
    0x0: PictureMemo,
    0x1: TextMemo,
    0x2: ObjectMemo,
}


class MemoFile(object):
    def __init__(self, filename, strict=False):
        self.filename = filename
        self.strict = strict
        self._open()
        self._init()

    def _init(self):
        pass

    def _open(self):
        self.file = open(self.filename, 'rb')
        # Shortcuts for speed.
        self._read = self.file.read
        self._seek = self.file.seek
        # File size, used for bounds checking in strict mode.
        self._size = os.fstat(self.file.fileno()).st_size

    def _close(self):
        self.file.close()

    def _check_block_range(self, index, header_size):
        """Raise MemoBlockOutOfRange if the block is outside the file.

        Only called in strict mode.  ``header_size`` is the size of
        the block header which must fit inside the file for the block
        to be readable at all.
        """
        offset = index * self.blocksize
        if offset < 0 or offset + header_size > self._size:
            raise MemoBlockOutOfRange(
                'memo block is outside memo file {!r}'.format(
                    self.filename),
                block=index)
        return offset

    def __getitem__(self, index):
        raise NotImplementedError

    def __enter__(self):
        return self

    def __exit__(self, type, value, traceback):
        self._close()
        return False


class FakeMemoFile(MemoFile):
    def __getitem__(self, i):
        return None

    def _open(self):
        pass

    _init = _close = _open


class VFPMemoFile(MemoFile):
    def _init(self):
        self.header = VFPFileHeader.read(self.file)
        self.blocksize = self.header.blocksize
        if self.blocksize <= 0:
            if self.strict:
                raise MemoHeaderCorrupt(
                    'invalid block size {} in FPT file header'.format(
                        self.header.blocksize))
            else:
                # A block size of 0 means the default of 512 bytes.
                self.blocksize = 512

    def __getitem__(self, index):
        """Get a memo from the file."""
        if index <= 0:
            # Empty reference (NULL memo).
            return None

        if self.strict:
            return self._get_memo_strict(index)

        self._seek(index * self.blocksize)
        memo_header = VFPMemoHeader.read(self.file)

        data = self._read(memo_header.length)
        if len(data) != memo_header.length:
            raise IOError('EOF reached while reading memo')

        return VFP_TYPE_MAP.get(memo_header.type, BinaryMemo)(data)

    def _get_memo_strict(self, index):
        offset = self._check_block_range(index, VFPMemoHeader.size)

        self._seek(offset)
        raw_header = self._read(VFPMemoHeader.size)
        if len(raw_header) != VFPMemoHeader.size:
            raise MemoHeaderCorrupt(
                'could not read complete memo header', block=index)
        memo_header = VFPMemoHeader.unpack(raw_header)

        if memo_header.type not in VFP_TYPE_MAP:
            raise MemoHeaderCorrupt(
                'unknown memo type {:#x} in block header'.format(
                    memo_header.type),
                block=index)

        if offset + VFPMemoHeader.size + memo_header.length > self._size:
            raise MemoTruncated(
                'memo data of {} bytes extends past end of file'.format(
                    memo_header.length),
                block=index)

        data = self._read(memo_header.length)
        if len(data) != memo_header.length:
            raise MemoTruncated(
                'EOF reached while reading memo', block=index)

        return VFP_TYPE_MAP[memo_header.type](data)


class DB3MemoFile(MemoFile):
    """dBase III memo file."""
    # Code from dbf.py
    blocksize = DB3_BLOCK_SIZE

    def __getitem__(self, index):
        if index <= 0:
            # Empty reference (NULL memo).
            return None

        if self.strict:
            self._check_block_range(index, 1)

        self._seek(index * self.blocksize)
        data = b''
        while True:
            newdata = self._read(self.blocksize)
            if not newdata:
                if self.strict and data:
                    # EOF was reached before the memo terminator.
                    raise MemoTruncated(
                        'memo has no \\x1a terminator before end of file',
                        block=index)
                return data
            data += newdata

            # Todo: some files (help.dbt) has only one field separator.
            # Is this enough for all file though?
            end_of_memo = data.find(b'\x1a')
            if end_of_memo != -1:
                return data[:end_of_memo]

            # Alternative end of memo markers:
            # \x1a\x1a
            # \x0d\x0a


class DB4MemoFile(MemoFile):
    """dBase IV memo file"""
    def _init(self):
        raw_header = self._read(DB4FileHeader.size)
        if len(raw_header) == DB4FileHeader.size:
            header = DB4FileHeader.unpack(raw_header)
            # A block size of 0 means the default of 512 bytes.
            self.blocksize = header.blocksize or DB3_BLOCK_SIZE
        elif self.strict:
            raise MemoHeaderCorrupt(
                'could not read complete DBT file header')
        else:
            self.blocksize = DB3_BLOCK_SIZE

    def __getitem__(self, index):
        if index <= 0:
            # Empty reference (NULL memo).
            return None

        if self.strict:
            return self._get_memo_strict(index)

        self._seek(index * self.blocksize)
        memo_header = DB4MemoHeader.read(self.file)
        data = self._read(memo_header.length)
        # Todo: fields are terminated in different ways.
        # \x1a is one of them
        # \x1f seems to be another (dbase_8b.dbt)
        return data.split(b'\x1f', 1)[0]

    def _get_memo_strict(self, index):
        offset = self._check_block_range(index, DB4MemoHeader.size)

        self._seek(offset)
        raw_header = self._read(DB4MemoHeader.size)
        if len(raw_header) != DB4MemoHeader.size:
            raise MemoHeaderCorrupt(
                'could not read complete memo header', block=index)
        memo_header = DB4MemoHeader.unpack(raw_header)

        if memo_header.reserved != DB4_MEMO_SIGNATURE:
            raise MemoHeaderCorrupt(
                'bad memo header signature {:#010x} '
                '(expected {:#010x})'.format(memo_header.reserved,
                                             DB4_MEMO_SIGNATURE),
                block=index)

        if offset + DB4MemoHeader.size + memo_header.length > self._size:
            raise MemoTruncated(
                'memo data of {} bytes extends past end of file'.format(
                    memo_header.length),
                block=index)

        data = self._read(memo_header.length)
        if len(data) != memo_header.length:
            raise MemoTruncated(
                'EOF reached while reading memo', block=index)

        # The length in the header is authoritative.  The memo is
        # returned as is instead of being cut at a terminator byte,
        # which would corrupt binary memos containing that byte.
        return data


def find_memofile(dbf_filename):
    for ext in ['.fpt', '.dbt']:
        name = ifind(dbf_filename, ext=ext)
        if name:
            return name
    else:
        return None


def open_memofile(filename, dbversion, strict=False):
    if filename.lower().endswith('.fpt'):
        return VFPMemoFile(filename, strict=strict)
    else:
        # print('######', dbversion)
        if dbversion == 0x83:
            return DB3MemoFile(filename, strict=strict)
        else:
            return DB4MemoFile(filename, strict=strict)
