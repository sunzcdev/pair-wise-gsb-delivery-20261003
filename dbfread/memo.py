"""
Reads data from FPT and DBT (memo) files.

Memo files are used to store varying length text or binary data which
is too large to fit in a DBF field.

Each format has its own block size and its own way of ending a memo:

* dBase III has no file header and always uses 512 byte blocks. The
  memo text is terminated by ``\\x1a``.
* dBase IV stores the block size in the file header and prefixes every
  memo with an 8 byte header holding the memo length.
* Visual FoxPro stores the block size in the file header and prefixes
  every memo with a type and a length.

The memo reference (block number) always comes from the DBF file and
can be damaged. If ``strict=True`` is passed (via
``DBF(..., strict_memo=True)``) references are validated before they
are followed and a specific exception is raised:

* ``MemoBlockOutOfRange`` -- the block number is negative or starts
  beyond the end of the memo file
* ``MemoHeaderCorrupt`` -- the block header is incomplete or invalid
* ``MemoTruncated`` -- the memo body ends before its declared length
  (or, for dBase III, before the ``\\x1a`` terminator)

A block number of zero is an empty reference and returns ``None`` in
both modes. The default (``strict=False``) keeps the historic, lenient
behaviour unchanged.
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

# dBase III memo files always use this block size. It is also the
# default for dBase IV and Visual FoxPro files that store 0 in the
# file header.
DEFAULT_BLOCK_SIZE = 512
DB3_BLOCK_SIZE = DEFAULT_BLOCK_SIZE

# End of memo marker used by dBase III.
DB3_END_OF_MEMO = b'\x1a'

# A block must be large enough to hold its own header. Values smaller
# than this in a file header are treated as damaged.
MIN_BLOCK_SIZE = 16


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
        self.blocksize = DEFAULT_BLOCK_SIZE
        self._open()
        self._init()

    def _init(self):
        pass

    def _open(self):
        self.file = open(self.filename, 'rb')
        # Shortcuts for speed.
        self._read = self.file.read
        self._seek = self.file.seek
        # File size, used for bounds checking.
        self._size = os.fstat(self.file.fileno()).st_size
        # The position is always set with an absolute seek before
        # reading, so a failed read can never leave a stale cursor
        # behind for a later, independent read.
        self._seek(0)

    def _close(self):
        self.file.close()

    def _is_empty_reference(self, index):
        """Check if ``index`` is an empty (NULL) memo reference.

        Block number 0 is always an empty reference. A negative block
        number is damaged data: in strict mode it raises
        ``MemoBlockOutOfRange``, while the default compatible mode
        keeps the historic behaviour of treating it as empty.
        """
        if index == 0:
            return True

        if index < 0:
            if self.strict:
                raise MemoBlockOutOfRange(
                    'negative memo block number', block=index)
            return True

        return False

    def _check_block_range(self, index):
        """Check that the block starts inside the file (strict mode).

        Only the start of the block has to be inside the file. A block
        that starts inside the file but has a cut short header is a
        ``MemoHeaderCorrupt``, not an out of range reference.
        """
        offset = index * self.blocksize
        if offset < 0 or offset >= self._size:
            raise MemoBlockOutOfRange(
                'memo block {0} starts outside memo file '
                '(offset {1}, file size {2})'.format(
                    index, offset, self._size),
                block=index)
        return offset

    def _resolve_block_size(self, blocksize, source):
        """Turn a block size from a file header into a usable value.

        A stored value of 0 means the default of 512 bytes. A value
        that cannot describe any block in the file -- smaller than a
        block header, or larger than the file itself -- is treated as a
        damaged header: in strict mode it raises ``MemoHeaderCorrupt``
        and in compatible mode it falls back to the default so reading
        keeps working the way it did before.

        The file may still be truncated inside the last block, so the
        number of blocks recorded in the header is not used here; that
        case is reported per record as a corrupt or truncated memo.
        """
        if blocksize == 0:
            return DEFAULT_BLOCK_SIZE

        if MIN_BLOCK_SIZE <= blocksize <= self._size:
            return blocksize

        if self.strict:
            raise MemoHeaderCorrupt(
                'invalid block size {} in {}'.format(blocksize, source))
        return DEFAULT_BLOCK_SIZE

    def _read_block_header(self, index, parser):
        """Read and unpack a memo block header at ``index``.

        In strict mode an incomplete header is reported as
        ``MemoHeaderCorrupt``: a block that starts inside the file but
        is cut short is a damaged header, not an out of range
        reference. In compatible mode a short read makes
        ``struct.unpack()`` fail, exactly as it used to.
        """
        self._seek(index * self.blocksize)

        if self.strict:
            raw = self._read(parser.size)
            if len(raw) != parser.size:
                raise MemoHeaderCorrupt(
                    'incomplete memo block header ({} of {} bytes)'.format(
                        len(raw), parser.size),
                    block=index)
            return parser.unpack(raw)
        else:
            return parser.read(self.file)

    def _read_body(self, index, offset, header_size, length,
                   check_short_compat=False):
        """Read ``length`` bytes of memo body and validate the result.

        In strict mode a body that runs past the end of the file is a
        ``MemoTruncated``. In compatible mode the historic behaviour is
        kept: Visual FoxPro raised ``IOError`` for a short body, while
        dBase IV returned what could be read.
        """
        if self.strict and offset + header_size + length > self._size:
            raise MemoTruncated(
                'memo body of {} bytes extends past end of file'.format(
                    length),
                block=index)

        self._seek(offset + header_size)
        data = self._read(length)

        if len(data) != length:
            if self.strict:
                raise MemoTruncated(
                    'memo body ended after {} of {} bytes'.format(
                        len(data), length),
                    block=index)
            elif check_short_compat:
                raise IOError('EOF reached while reading memo')

        return data

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
        if not self.strict:
            # Historic behaviour: a short file header makes
            # struct.unpack() fail.
            self.header = VFPFileHeader.read(self.file)
        else:
            raw_header = self._read(VFPFileHeader.size)
            if len(raw_header) != VFPFileHeader.size:
                raise MemoHeaderCorrupt(
                    'could not read complete FPT file header')
            self.header = VFPFileHeader.unpack(raw_header)

        # A block size of 0 in the file header means the default of
        # 512 bytes. Anything the file cannot contain is a damaged
        # header.
        self.blocksize = self._resolve_block_size(
            self.header.blocksize, 'FPT file header')

    def __getitem__(self, index):
        """Get a memo from the file.

        A block number of zero is an empty reference and returns None.
        """
        if self._is_empty_reference(index):
            return None

        if self.strict:
            offset = self._check_block_range(index)
        else:
            offset = index * self.blocksize

        memo_header = self._read_block_header(index, VFPMemoHeader)

        if self.strict and memo_header.type not in VFP_TYPE_MAP:
            raise MemoHeaderCorrupt(
                'unknown memo type {:#x} in block header'.format(
                    memo_header.type),
                block=index)

        data = self._read_body(index, offset, VFPMemoHeader.size,
                               memo_header.length,
                               check_short_compat=True)
        return VFP_TYPE_MAP.get(memo_header.type, BinaryMemo)(data)


class DB3MemoFile(MemoFile):
    """dBase III memo file.

    dBase III has no file header, so the block size is always 512
    bytes.
    """
    def _init(self):
        self.blocksize = DB3_BLOCK_SIZE

    def __getitem__(self, index):
        if self._is_empty_reference(index):
            return None

        if self.strict:
            offset = self._check_block_range(index)
        else:
            offset = index * self.blocksize

        # dBase III memos are terminated by \x1a. That marker is only
        # an end marker for this format; the length based formats
        # (dBase IV and Visual FoxPro) never use it to cut the body.
        self._seek(offset)
        data = b''
        while True:
            newdata = self._read(self.blocksize)
            if not newdata:
                if self.strict:
                    raise MemoTruncated(
                        'memo has no \\x1a terminator before end of file',
                        block=index)
                return data
            data += newdata

            end_of_memo = data.find(DB3_END_OF_MEMO)
            if end_of_memo != -1:
                return data[:end_of_memo]


class DB4MemoFile(MemoFile):
    """dBase IV memo file."""
    def _init(self):
        raw_header = self._read(DB4FileHeader.size)
        if len(raw_header) == DB4FileHeader.size:
            header = DB4FileHeader.unpack(raw_header)
            self.blocksize = self._resolve_block_size(
                header.blocksize, 'DBT file header')
        elif self.strict:
            raise MemoHeaderCorrupt(
                'could not read complete DBT file header')
        else:
            self.blocksize = DEFAULT_BLOCK_SIZE

    def __getitem__(self, index):
        if self._is_empty_reference(index):
            return None

        if self.strict:
            offset = self._check_block_range(index)
        else:
            offset = index * self.blocksize

        memo_header = self._read_block_header(index, DB4MemoHeader)

        if self.strict and memo_header.reserved != DB4_MEMO_SIGNATURE:
            raise MemoHeaderCorrupt(
                'bad memo header signature {:#010x} '
                '(expected {:#010x})'.format(memo_header.reserved,
                                             DB4_MEMO_SIGNATURE),
                block=index)

        data = self._read_body(index, offset, DB4MemoHeader.size,
                               memo_header.length)

        if self.strict:
            # The length in the block header is authoritative. The
            # body is returned as is, so binary memos containing
            # terminator bytes such as \x1f or \x1a are not cut short.
            return data
        else:
            # Historic, lenient behaviour: memos were cut at \x1f.
            return data.split(b'\x1f', 1)[0]


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
        if dbversion == 0x83:
            # FoxBASE+/dBASE III PLUS with memo.
            return DB3MemoFile(filename, strict=strict)
        else:
            return DB4MemoFile(filename, strict=strict)
