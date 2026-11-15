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
from .memo import (find_memofile, open_memofile, FakeMemoFile,
                   GuardedMemoFile)
from .codepages import guess_encoding
from .dbversions import get_dbversion_string
from .exceptions import DBFNotFound, MissingMemoFile, DBFChangedError
from .consistency import (FileIdentity, copy_file_verified,
                          make_temp_dir, remove_temp_dir, open_shared)

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
                 char_decode_errors='strict',
                 consistent=False,
                 copy_to_temp=False):

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

        # Reading from a private temporary copy always requires the
        # consistent machinery to be enabled.
        self.consistent = consistent or copy_to_temp
        self.copy_to_temp = copy_to_temp

        # Name part before .dbf is the table name
        self.name = os.path.basename(filename)
        self.name = os.path.splitext(self.name)[0].lower()
        self._records = None
        self._deleted = None

        # Consistent-read state.
        self.closed = False
        self._tempdir = None
        self._identities = {}
        self._open_files = []
        # The original paths, kept even when reading from a copy.
        self.source_filename = None
        self.source_memofilename = None

        # Filled in by self._read_header() / self._read_field_headers().
        self.filename = None
        self.memofilename = None
        self.header = None
        self.fields = []       # namedtuples
        self.field_names = []  # strings

        if ignorecase:
            found = ifind(filename)
            if not found:
                raise DBFNotFound('could not find file {!r}'.format(filename))
        else:
            found = filename

        try:
            self._open_table(found)

            if load:
                self.load()
        except Exception:
            # A parse failure (or anything else) during construction must
            # not leave a private copy behind.  The borrowed source files
            # are never touched.
            self.close()
            raise

    def _open_table(self, found):
        if self.copy_to_temp:
            self._open_from_temp_copy(found)
        else:
            # The paths read from directly.
            self.filename = found
            self.source_filename = found

            if self.consistent:
                self._identities[found] = FileIdentity.pin(found)

            # The header handle is short-lived and closed by the with
            # block, so it is not added to the open-file tracking list.
            with self._open_source(found) as infile:
                self._read_header(infile)
                self._read_field_headers(infile)
                self._check_headers()
                self._read_date()

            if self.consistent:
                # Make sure the file was not swapped while the header was
                # being read.
                self._identities[found].check()

            # Resolve the memo file on the original path.
            self.memofilename = self._get_memofilename()
            self.source_memofilename = self.memofilename
            if self.consistent and self.memofilename:
                self._identities[self.memofilename] = FileIdentity.pin(
                    self.memofilename)

    def _open_from_temp_copy(self, source_dbf):
        """Copy the DBF and memo file into a private temp directory.

        The source files are pinned and re-checked around the copy so a
        replacement or truncation during the copy aborts the read.  All
        subsequent reading happens from the copies, which nobody else
        knows about.
        """
        self.source_filename = source_dbf

        # Parse the structure from the source first so the same, well
        # tested header code is used in every mode and so we know whether
        # there is a memo file to copy.
        with open_shared(source_dbf) as infile:
            self._read_header(infile)
            self._read_field_headers(infile)
            self._check_headers()
            self._read_date()

        source_memo = self._get_source_memofile(source_dbf)
        self.source_memofilename = source_memo

        # Pin the source files at copy time and verify them again once
        # the copy is complete.
        self._tempdir = make_temp_dir()
        copied_dbf = os.path.join(self._tempdir,
                                  os.path.basename(source_dbf))
        copy_file_verified(source_dbf, copied_dbf)
        self.filename = copied_dbf

        if source_memo is not None:
            copied_memo = os.path.join(self._tempdir,
                                       os.path.basename(source_memo))
            copy_file_verified(source_memo, copied_memo)
            self.memofilename = copied_memo
        else:
            self.memofilename = None

    def _get_source_memofile(self, source_dbf):
        """Resolve the memo file on the source path while copying."""
        field_types = [field.type for field in self.fields]
        if not set(field_types) & set('MGPB'):
            # No memo fields.
            return None

        path = find_memofile(source_dbf)
        if path is None:
            if self.ignore_missing_memofile:
                return None
            raise MissingMemoFile(
                'missing memo file for {}'.format(source_dbf))
        return path

    def _read_date(self):
        try:
            self.date = datetime.date(expand_year(self.header.year),
                                      self.header.month,
                                      self.header.day)
        except ValueError:
            # Invalid date or '\x00\x00\x00'.
            self.date = None

    def _open_source(self, path):
        """Open a source file, allowing other programs to replace it.

        Only used when reading from the original files in consistent
        mode.  In every other mode a plain binary open is used.
        """
        if self.consistent:
            return open_shared(path)
        return open(path, 'rb')

    def _open_dbf(self, path):
        """Open the DBF file, guarding it when consistent reading is on."""
        if self.consistent and path in self._identities:
            self._identities[path].check()
        infile = self._open_source(path)
        if self.consistent:
            self._open_files.append(infile)
        return infile

    def _ensure_open(self):
        if self.closed:
            raise DBFChangedError(
                'the DBF table {!r} has been closed'.format(self.filename))

    def close(self):
        """Close the table and remove any private temporary copy.

        This only removes files that dbfread copied itself into a
        temporary directory.  The original DBF and memo files are never
        modified or deleted.
        """
        for infile in self._open_files:
            try:
                infile.close()
            except Exception:
                pass
        self._open_files = []

        remove_temp_dir(self._tempdir)
        self._tempdir = None
        self.closed = True

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
        self._ensure_open()
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

            self.field_names.append(field.name)

            self.fields.append(field)

    def _open_memofile(self):
        if self.memofilename and not self.raw:
            identity = self._identities.get(self.memofilename)
            if identity is not None:
                # Streaming from the original memo file: open it so that
                # another program is still allowed to replace it, and
                # verify its identity around every lookup.
                memofile = open_memofile(
                    self.memofilename, self.header.dbversion,
                    opener=open_shared)
                return GuardedMemoFile(memofile, identity)
            return open_memofile(self.memofilename,
                                 self.header.dbversion)
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

    def _check_dbf_identity(self):
        identity = self._identities.get(self.filename)
        if identity is not None:
            identity.check()

    def _read_exact(self, infile, size):
        """Read exactly ``size`` bytes, detecting truncation.

        In consistent mode a short read (some, but not all, of the
        requested bytes) means the file was truncated, so it is reported
        as a consistency failure rather than returning partial bytes that
        would fail later with an obscure error.  Reading zero bytes at the
        end of the file is a normal, clean EOF.
        """
        data = infile.read(size)
        if self.consistent and 0 < len(data) < size:
            self._check_dbf_identity()
            raise DBFChangedError(
                '{} was truncated while it was being read; the result is '
                'incomplete'.format(self.filename))
        return data

    def _count_records(self, record_type=b' '):
        self._ensure_open()
        if self.consistent:
            # Reuse the guarded iterator so counting gets the same
            # replacement, truncation and length checks.
            return sum(1 for _ in self._iter_records(record_type))

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
        # This must stay a normal function (no ``yield`` here): otherwise
        # Python turns it into a generator and the early ``return`` below
        # would simply stop that outer generator instead of handing back
        # the consistent generator.
        self._ensure_open()
        if self.consistent:
            return self._iter_records_consistent(record_type)
        return self._iter_records_streaming(record_type)

    def _iter_records_streaming(self, record_type=b' '):
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

            while True:
                sep = read(1)

                if sep == record_type:
                    items = [
                        (field.name, parse(field, read(field.length)))
                        for field in self.fields
                    ]
                    yield self.recfactory(items)

                elif sep in (b'\x1a', b''):
                    # End of records.
                    break
                else:
                    skip_record(infile)

    def _iter_records_consistent(self, record_type):
        """Iterate records while re-validating the source files.

        The identity of the DBF (and, when streamed directly, the memo
        file) is pinned when the table opens and re-checked on every
        record and once more at the end.  The number of records is also
        compared with the count fixed in the header, so a truncated or
        grown file can not pass unnoticed.
        """
        memofile = None
        infile = None
        try:
            infile = self._open_dbf(self.filename)
            memofile = self._open_memofile()
            self._check_dbf_identity()

            # Skip to first record.
            infile.seek(self.header.headerlen, 0)

            if self.raw:
                def parse(_, data):
                    return data
            else:
                field_parser = self.parserclass(self, memofile)
                parse = field_parser.parse

            read_exact = self._read_exact
            record_length = self.header.recordlen
            seen_records = 0

            while True:
                # Re-validate before each record so a replacement that
                # happens between records is detected promptly.
                self._check_dbf_identity()

                sep = read_exact(infile, 1)

                if sep in (b'\x1a', b''):
                    # End of records.
                    break

                # Every physical record counts towards the total fixed in
                # the header, whether or not it is the type being yielded.
                seen_records += 1

                if sep == record_type:
                    items = []
                    for field in self.fields:
                        data = read_exact(infile, field.length)
                        items.append((field.name, parse(field, data)))
                    yield self.recfactory(items)
                else:
                    # A record of another type (for example a deleted one
                    # when iterating active records).  Skip the remainder
                    # of its fixed-width slot without trusting a seek over
                    # a possibly truncated file.
                    read_exact(infile, record_length - 1)

            # Final verification: identity, size and the record count
            # fixed in the header at open time must all still match.
            self._check_dbf_identity()
            if seen_records != self.header.numrecords:
                raise DBFChangedError(
                    '{} now contains {} record(s) but the header fixed at '
                    'open time reported {}; the result is incomplete'.format(
                        self.filename, seen_records,
                        self.header.numrecords))
        finally:
            if memofile is not None:
                close = getattr(memofile, 'close', None)
                if close is not None:
                    close()
                else:
                    memofile._close()
            if infile is not None:
                infile.close()
                if infile in self._open_files:
                    self._open_files.remove(infile)

    def __iter__(self):
        self._ensure_open()
        if self.loaded:
            return list.__iter__(self._records)
        else:
            return self._iter_records()

    def __len__(self):
        return len(self.records)

    def __repr__(self):
        if self.closed:
            status = 'closed'
        elif self.loaded:
            status = 'loaded'
        else:
            status = 'unloaded'
        return '<{} DBF table {!r}>'.format(status, self.filename)

    def __enter__(self):
        return self

    def __exit__(self, type, value, traceback):
        if self.copy_to_temp:
            # Leaving the context tears down the private copy; the source
            # files are left untouched.
            self.close()
        else:
            # Keep the historical behaviour for tables that read directly
            # from the original files.
            self.unload()
        return False

    def __del__(self):
        # Make sure a private temporary copy does not outlive the table.
        try:
            self.close()
        except Exception:
            pass
