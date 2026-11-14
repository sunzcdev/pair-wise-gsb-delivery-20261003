"""
Class to read DBF files.
"""
import os
import sys
import datetime
import platform
import collections

from .ifiles import ifind
from .struct_parser import StructParser
from .field_parser import FieldParser
from .memo import find_memofile, open_memofile, FakeMemoFile
from .codepages import guess_encoding
from .dbversions import get_dbversion_string
from .exceptions import DBFNotFound, MissingMemoFile

_py_version = (sys.version_info.major, sys.version_info.minor)
_py_impl = platform.python_implementation()
if _py_version >= (3, 7) or (_py_version == (3, 6) and _py_impl == 'CPython'):
    ORDERED_DICT = dict
else:
    ORDERED_DICT = collections.OrderedDict


DBFHeader = StructParser(
    'DBFHeader',
    '<BBBBLHHHBBLLLBBH',
    ['dbversion',
     'year',
     'month',
     'day',
     'numrecords',
     'headerlen',
     'recordlen',
     'reserved1',
     'incomplete_transaction',
     'encryption_flag',
     'free_record_thread',
     'reserved2',
     'reserved3',
     'mdx_flag',
     'language_driver',
     'reserved4',
     ])

DBFField = StructParser(
    'DBFField',
    '<11scLBBHBBBB7sB',
    ['name',
     'type',
     'address',
     'length',
     'decimal_count',
     'reserved1',
     'workarea_id',
     'reserved2',
     'reserved3',
     'set_fields_flag',
     'reserved4',
     'index_field_flag',
     ])


def expand_year(year):
    """Convert 2-digit year to 4-digit year."""

    if year < 80:
        return 2000 + year
    else:
        return 1900 + year


# Visual FoxPro field flags. These are found in byte 18 of the field
# descriptor, which the DBFField struct stores in the low byte of
# 'reserved1'.
VFP_FLAG_SYSTEM = 0x01
VFP_FLAG_NULLABLE = 0x02
VFP_FLAG_BINARY = 0x04

# Name of the hidden Visual FoxPro field that stores the null bit map.
# (Compared case-insensitively.)
NULLFLAGS_FIELD_NAME = '_nullflags'


def _null_bit_is_set(bitmap, index):
    """Check if bit ``index`` is set in a Visual FoxPro null bit map.

    ``bitmap`` is a ``bytearray`` where bit 0 of byte 0 belongs to the
    first field in the table.
    """
    return bool(bitmap[index >> 3] & (1 << (index & 7)))


class RecordIterator(object):
    def __init__(self, table, record_type):
        self._record_type = record_type
        self._table = table

    def __iter__(self):
        return self._table._iter_records(self._record_type)

    def __len__(self):
        return self._table._count_records(self._record_type)


class DBF(object):
    """DBF table."""
    def __init__(self, filename, encoding=None, ignorecase=True,
                 lowernames=False,
                 parserclass=FieldParser,
                 recfactory=ORDERED_DICT,
                 load=False,
                 raw=False,
                 ignore_missing_memofile=False,
                 char_decode_errors='strict'):

        self.encoding = encoding
        self.ignorecase = ignorecase
        self.lowernames = lowernames
        self.parserclass = parserclass
        self.raw = raw
        self.ignore_missing_memofile = ignore_missing_memofile
        self.char_decode_errors = char_decode_errors

        if recfactory is None:
            self.recfactory = lambda items: items
        else:
            self.recfactory = recfactory

        # Name part before .dbf is the table name
        self.name = os.path.basename(filename)
        self.name = os.path.splitext(self.name)[0].lower()
        self._records = None
        self._deleted = None

        if ignorecase:
            self.filename = ifind(filename)
            if not self.filename:
                raise DBFNotFound('could not find file {!r}'.format(filename))
        else:
            self.filename = filename

        # Filled in by self._read_headers()
        self.memofilename = None
        self.header = None
        self.fields = []       # namedtuples
        self.field_names = []  # strings

        # Filled in by self._read_field_headers(). These describe the
        # Visual FoxPro null bit map (the hidden '_NullFlags' field).
        self._all_fields = []       # all fields, including '_NullFlags'
        self._nullflags_index = None
        self._nullable_indices = set()

        with open(self.filename, mode='rb') as infile:
            self._read_header(infile)
            self._read_field_headers(infile)
            self._check_headers()

            try:
                self.date = datetime.date(expand_year(self.header.year),
                                          self.header.month,
                                          self.header.day)
            except ValueError:
                # Invalid date or '\x00\x00\x00'.
                self.date = None

        self.memofilename = self._get_memofilename()

        if load:
            self.load()

    @property
    def dbversion(self):
        return get_dbversion_string(self.header.dbversion)

    def _get_memofilename(self):
        # Does the table have a memo field?
        field_types = [field.type for field in self.fields]
        if not set(field_types) & set('MGPB'):
            # No memo fields.
            return None

        path = find_memofile(self.filename)
        if path is None:
            if self.ignore_missing_memofile:
                return None
            else:
                raise MissingMemoFile('missing memo file for {}'.format(
                    self.filename))
        else:
            return path

    @property
    def loaded(self):
        """``True`` if records are loaded into memory."""
        return self._records is not None

    def load(self):
        """Load records into memory.

        This loads both records and deleted records. The ``records``
        and ``deleted`` attributes will now be lists of records.

        """
        if not self.loaded:
            self._records = list(self._iter_records(b' '))
            self._deleted = list(self._iter_records(b'*'))

    def unload(self):
        """Unload records from memory.

        The records and deleted attributes will now be instances of
        ``RecordIterator``, which streams records from disk.
        """
        self._records = None
        self._deleted = None

    @property
    def records(self):
        """Records (not included deleted ones). When loaded a list of records,
        when not loaded a new ``RecordIterator`` object.
        """
        if self.loaded:
            return self._records
        else:
            return RecordIterator(self, b' ')

    @property
    def deleted(self):
        """Deleted records. When loaded a list of records, when not loaded a
        new ``RecordIterator`` object.
        """
        if self.loaded:
            return self._deleted
        else:
            return RecordIterator(self, b'*')

    def _read_header(self, infile):
        # Todo: more checks?
        self.header = DBFHeader.read(infile)

        if self.encoding is None:
            try:
                self.encoding = guess_encoding(self.header.language_driver)
            except LookupError:
                self.encoding = 'ascii'

    def _decode_text(self, data):
        return data.decode(self.encoding, errors=self.char_decode_errors)

    def _read_field_headers(self, infile):
        while True:
            sep = infile.read(1)
            if sep in (b'\r', b'\n', b''):
                # End of field headers
                break

            field = DBFField.unpack(sep + infile.read(DBFField.size - 1))

            field.type = chr(ord(field.type))

            # For character fields > 255 bytes the high byte
            # is stored in decimal_count.
            if field.type in 'C':
                field.length |= field.decimal_count << 8
                field.decimal_count = 0

            # Field name is b'\0' terminated.
            field.name = self._decode_text(field.name.split(b'\0')[0])
            if self.lowernames:
                field.name = field.name.lower()

            self._all_fields.append(field)

        self._setup_null_flags()

    def _setup_null_flags(self):
        """Look for the Visual FoxPro null flags field.

        Visual FoxPro stores null values in a hidden system field
        named '_NullFlags' (type '0'). The field is a bit map with one
        bit per field in the table (including itself). A set bit means
        the value of the corresponding field is NULL. A field can only
        be NULL if it is marked as nullable in its field flags.

        The '_NullFlags' field is an implementation detail and is
        removed from ``self.fields`` so it doesn't show up as a normal
        data column. (It is still available in ``self._all_fields``
        and is included in records in raw mode.)
        """
        for index, field in enumerate(self._all_fields):
            # The Visual FoxPro field flags are stored in the low byte
            # of 'reserved1' (byte 18 of the field descriptor).
            flags = field.reserved1 & 0xff

            if (field.type == '0' and
                    field.name.lower() == NULLFLAGS_FIELD_NAME and
                    self._nullflags_index is None):
                self._nullflags_index = index
            elif flags & VFP_FLAG_NULLABLE:
                self._nullable_indices.add(index)

        for index, field in enumerate(self._all_fields):
            if index != self._nullflags_index:
                self.fields.append(field)
                self.field_names.append(field.name)

    def _check_nullflags(self, record_number, bitmap):
        """Check the null flags layout for a record.

        Raises ValueError (with the position of the record) if the
        field layout or the length of the null bit map is invalid.
        """
        offset = self.header.headerlen + record_number * self.header.recordlen
        where = 'record {} (byte offset {})'.format(record_number, offset)

        if self._nullflags_index is None:
            if self._nullable_indices:
                raise ValueError(
                    '{}: table has nullable fields but no '
                    '_NullFlags field'.format(where))
            else:
                # No null flags in this table.
                return

        # The bit map must have room for one bit per field in the
        # table (including the _NullFlags field itself).
        expected = (len(self._all_fields) + 7) // 8
        if len(bitmap) < expected:
            raise ValueError(
                '{}: _NullFlags field is too short: '
                'expected at least {} byte(s) for {} fields, '
                'got {}'.format(where, expected,
                                len(self._all_fields), len(bitmap)))

    def _open_memofile(self):
        if self.memofilename and not self.raw:
            return open_memofile(self.memofilename, self.header.dbversion)
        else:
            return FakeMemoFile(self.memofilename)

    def _check_headers(self):
        field_parser = self.parserclass(self)

        """Check headers for possible format errors."""
        for field in self.fields:

            if field.type == 'I' and field.length != 4:
                message = 'Field type I must have length 4 (was {})'
                raise ValueError(message.format(field.length))

            elif field.type == 'L' and field.length != 1:
                message = 'Field type L must have length 1 (was {})'
                raise ValueError(message.format(field.length))

            elif not field_parser.field_type_supported(field.type):
                # Todo: return as byte string?
                raise ValueError('Unknown field type: {!r}'.format(field.type))

    def _skip_record(self, infile):
        # -1 for the record separator which was already read.
        infile.seek(self.header.recordlen - 1, 1)

    def _count_records(self, record_type=b' '):
        count = 0

        with open(self.filename, 'rb') as infile:
            # Skip to first record.
            infile.seek(self.header.headerlen, 0)

            while True:
                sep = infile.read(1)
                if sep == record_type:
                    count += 1
                    self._skip_record(infile)
                elif sep in (b'\x1a', b''):
                    # End of records.
                    break
                else:
                    self._skip_record(infile)

        return count

    def _iter_records(self, record_type=b' '):
        with open(self.filename, 'rb') as infile, \
             self._open_memofile() as memofile:

            # Skip to first record.
            infile.seek(self.header.headerlen, 0)

            if self.raw:
                def parse(_, data):
                    return data
            else:
                field_parser = self.parserclass(self, memofile)
                parse = field_parser.parse

            # Shortcuts for speed.
            skip_record = self._skip_record
            read = infile.read

            # In raw mode the record is returned exactly as stored,
            # including the '_NullFlags' field.
            if self.raw:
                fields = self._all_fields
            else:
                fields = self.fields
            nullflags_index = self._nullflags_index
            nullable_indices = self._nullable_indices
            nullflags_checked = False

            record_number = 0
            while True:
                sep = read(1)

                if sep == record_type:
                    if self.raw or nullflags_index is None:
                        if not (self.raw or nullflags_checked):
                            # Nullable fields without a '_NullFlags'
                            # field is an invalid layout.
                            self._check_nullflags(record_number, b'')
                            nullflags_checked = True

                        items = [
                            (field.name, parse(field, read(field.length)))
                            for field in fields
                        ]
                    else:
                        # The record is read according to the full
                        # field layout, which includes the hidden
                        # '_NullFlags' field.
                        data = [read(field.length)
                                for field in self._all_fields]

                        if not nullflags_checked:
                            self._check_nullflags(record_number,
                                                  data[nullflags_index])
                            nullflags_checked = True

                        # bytearray is used because indexing it returns
                        # an integer in both Python 2 and 3.
                        bitmap = bytearray(data[nullflags_index])
                        items = []
                        for index, field in enumerate(self._all_fields):
                            if index == nullflags_index:
                                continue
                            elif (index in nullable_indices and
                                    _null_bit_is_set(bitmap, index)):
                                # The null bit map says this value is
                                # NULL, no matter what is stored in the
                                # field.
                                value = None
                            else:
                                value = parse(field, data[index])
                            items.append((field.name, value))

                    yield self.recfactory(items)

                elif sep in (b'\x1a', b''):
                    # End of records.
                    break
                else:
                    skip_record(infile)

                record_number += 1

    def __iter__(self):
        if self.loaded:
            return list.__iter__(self._records)
        else:
            return self._iter_records()

    def __len__(self):
        return len(self.records)

    def __repr__(self):
        if self.loaded:
            status = 'loaded'
        else:
            status = 'unloaded'
        return '<{} DBF table {!r}>'.format(status, self.filename)

    def __enter__(self):
        return self

    def __exit__(self, type, value, traceback):
        self.unload()
        return False
