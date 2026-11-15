"""
Reads data from FPT (memo) files.

FPT files are used to varying lenght text or binary data which is too
large to fit in a DBF field.

VFP == Visual FoxPro
DB3 == dBase III
DB4 == dBase IV
"""
from .ifiles import ifind
from .struct_parser import StructParser
from .exceptions import DBFChangedError


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
    def __init__(self, filename, opener=open):
        self.filename = filename
        self._opener = opener
        self._open()
        self._init()

    def _init(self):
        pass

    def _open(self):
        self.file = self._opener(self.filename, 'rb')
        # Shortcuts for speed.
        self._read = self.file.read
        self._seek = self.file.seek

    def _close(self):
        self.file.close()

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

    def __getitem__(self, index):
        """Get a memo from the file."""
        if index <= 0:
            return None

        self._seek(index * self.header.blocksize)
        memo_header = VFPMemoHeader.read(self.file)

        data = self._read(memo_header.length)
        if len(data) != memo_header.length:
            raise IOError('EOF reached while reading memo')

        return VFP_TYPE_MAP.get(memo_header.type, BinaryMemo)(data)


class DB3MemoFile(MemoFile):
    """dBase III memo file."""
    # Code from dbf.py
    def __getitem__(self, index):
        if index <= 0:
            return None

        block_size = 512
        self._seek(index * block_size)
        data = b''
        while True:
            newdata = self._read(block_size)
            if not newdata:
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

        # TODO: this is never reached. Whey is it here?
        # return data[:eom]


class DB4MemoFile(MemoFile):
    """dBase IV memo file"""
    def __getitem__(self, index):
        if index <= 0:
            return None

        # Todo: read this from the file header.
        block_size = 512

        self._seek(index * block_size)
        memo_header = DB4MemoHeader.read(self.file)
        data = self._read(memo_header.length)
        # Todo: fields are terminated in different ways.
        # \x1a is one of them
        # \x1f seems to be another (dbase_8b.dbt)
        return data.split(b'\x1f', 1)[0]


def find_memofile(dbf_filename):
    for ext in ['.fpt', '.dbt']:
        name = ifind(dbf_filename, ext=ext)
        if name:
            return name
    else:
        return None


def open_memofile(filename, dbversion, opener=open):
    if filename.lower().endswith('.fpt'):
        return VFPMemoFile(filename, opener=opener)
    else:
        # print('######', dbversion)
        if dbversion == 0x83:
            return DB3MemoFile(filename, opener=opener)
        else:
            return DB4MemoFile(filename, opener=opener)


class GuardedMemoFile(object):
    """Wrap a memo file and verify its identity around every lookup.

    This is used for consistent reads that stream directly from the
    original files.  The wrapped memo file is never modified; only its
    identity is re-checked.  If the memo file has been replaced,
    truncated or otherwise changed a
    :class:`~dbfread.exceptions.DBFChangedError` is raised instead of
    silently returning data from the wrong file.
    """

    def __init__(self, memofile, identity):
        self._memofile = memofile
        self._identity = identity

    def __getitem__(self, index):
        # Check before the seek/read and again afterwards so a replacement
        # that happens between DBF records is caught on the next lookup.
        self._identity.check()
        try:
            return self._memofile[index]
        except DBFChangedError:
            raise
        except IOError as error:
            # A short read inside the memo file usually means it was
            # truncated while the table was being read.
            self._identity.check()
            raise DBFChangedError(
                'memo file {} could not be read completely; the result '
                'is incomplete: {}'.format(self._identity.path, error))
        finally:
            self._identity.check()

    def close(self):
        self._memofile._close()

    def __enter__(self):
        return self

    def __exit__(self, type, value, traceback):
        self.close()
        return False
