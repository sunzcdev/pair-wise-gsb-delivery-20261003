"""
Class to read DBF files.
"""
import os
import sys
import datetime
import platform
import collections
import warnings

from .ifiles import ifind
from .struct_parser import StructParser
from .field_parser import FieldParser
from .memo import find_memofile, open_memofile, FakeMemoFile
from .codepages import guess_encoding
from .dbversions import get_dbversion_string
from .exceptions import (DBFNotFound, MissingMemoFile,
                         RecordCountMismatchWarning, TruncatedRecordWarning)

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


class RecordEnvelope(collections.namedtuple(
        'RecordEnvelope', ['index', 'deleted', 'record'])):
    """A record together with its physical position in the file.

    ``index`` is the zero-based physical record number, counted from
    the first record in the file. It does not change when deleted
    records are skipped or when only part of the file is read, so the
    same number always refers to the same record whether the records
    are streamed or loaded.

    ``deleted`` is ``True`` for records flagged as deleted in the file
    and ``False`` for active records.

    ``record`` is the value produced by the record factory (a ``dict``
    by default).

    Being a tuple, an envelope can also be unpacked::

        >>> for index, deleted, record in table.iter_records():
        ...     pass

    """
    __slots__ = ()

    @property
    def record_number(self):
        """One-based physical record number (``index + 1``)."""
        return self.index + 1

    @property
    def is_deleted(self):
        """``True`` if the record is flagged as deleted."""
        return self.deleted

    def __getitem__(self, key):
        # Tuple behaviour for numbers and slices, attribute behaviour
        # for strings, so both envelope[0] and envelope['index'] work.
        try:
            return tuple.__getitem__(self, key)
        except TypeError:
            try:
                return getattr(self, key)
            except (AttributeError, TypeError):
                raise KeyError(key)

    def get(self, key, default=None):
        """Dictionary-style access with a default."""
        try:
            return self[key]
        except KeyError:
            return default


class RecordEnvelopeIterator(object):
    """Iterate over :class:`RecordEnvelope` objects.

    This is returned by the ``envelopes`` attribute when the table is
    not loaded. Iterating over it or calling ``len()`` on it gives the
    same result, like the other iterators in dbfread.

    """
    def __init__(self, table):
        self._table = table

    def __iter__(self):
        return self._table._iter_envelopes()

    def __len__(self):
        return self._table._count_envelopes()


class RecordCountReport(collections.namedtuple(
        'RecordCountReport',
        ['header_count', 'actual_count', 'difference', 'truncated'])):
    """Result of :meth:`DBF.check_record_count`.

    ``header_count`` is the record count stored in the file header and
    ``actual_count`` is the number of complete records found in the
    file, deleted ones included.

    ``difference`` is ``header_count - actual_count``, so a positive
    value means the header claims more records than the file holds.

    ``truncated`` is ``True`` if the file ends with a partial record.
    Such a record is never returned as data.

    """
    __slots__ = ()

    @property
    def ok(self):
        """``True`` if the counts match and the file is not truncated."""
        return self.difference == 0 and not self.truncated

    @property
    def missing(self):
        """Records claimed by the header but not found in the file."""
        return max(self.difference, 0)

    @property
    def extra(self):
        """Records found in the file but not counted by the header."""
        return max(-self.difference, 0)


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
        self._envelopes = None

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

        The file is read in a single pass, so the records keep the
        order they have in the file and the physical record numbers in
        ``envelopes`` are the same as when streaming.

        """
        if not self.loaded:
            envelopes = list(self._iter_envelopes(report=True))
            self._envelopes = envelopes
            self._records = [envelope.record for envelope in envelopes
                             if not envelope.deleted]
            self._deleted = [envelope.record for envelope in envelopes
                             if envelope.deleted]

    def unload(self):
        """Unload records from memory.

        The records and deleted attributes will now be instances of
        ``RecordIterator``, which streams records from disk.
        """
        self._records = None
        self._deleted = None
        self._envelopes = None

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

    @property
    def envelopes(self):
        """All records with physical position and delete flag.

        When the table is loaded this is a list of
        :class:`RecordEnvelope` objects, otherwise it is a
        :class:`RecordEnvelopeIterator` that streams from disk. In both
        cases iterating over it or calling ``len()`` gives the same
        result.

        """
        if self.loaded:
            if self._envelopes is None:
                self._envelopes = list(self._iter_envelopes())
            return self._envelopes
        else:
            return RecordEnvelopeIterator(self)

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

    def _record_bodylen(self):
        """Number of bytes in a record body (excluding the flag byte).

        Normally this is ``recordlen - 1``. Field lengths can be
        changed after the table is opened (see docs/dbf_objects.rst),
        and if they then add up to more than the record length the
        longer value is used so that all fields can still be read.
        """
        field_len = sum(field.length for field in self.fields)
        return max(self.header.recordlen - 1, field_len)

    @staticmethod
    def _read_record(infile, bodylen):
        """Read one physical record slot.

        Returns ``(flag, data)`` where ``flag`` is the record flag byte
        and ``data`` is the record body. ``flag`` is ``b'\\x1a'`` for
        the end-of-file marker and ``None`` at the end of the file. In
        both cases ``data`` is ``None``.

        ``data`` is shorter than ``bodylen`` when the file ends in the
        middle of a record.
        """
        flag = infile.read(1)
        if not flag:
            return None, None
        if flag == b'\x1a':
            return flag, None

        return flag, infile.read(bodylen)

    @staticmethod
    def _is_padding(flag, data):
        """Check if a short tail is padding and not a partial record.

        Some writers pad the end of the file to a block boundary with
        spaces or NUL bytes instead of writing an end-of-file marker.
        Such a tail must not be reported as a partial record.
        """
        if flag not in (b' ', b'\x00'):
            return False

        return (flag + data).strip(b' \x00') == b''

    def _scan_records(self, infile, last=None, stats=None, warn=True):
        """Yield ``(index, deleted, data)`` for complete records.

        Records are numbered from zero in file order and that number is
        independent of which records are returned, so skipping deleted
        (or otherwise skipped) records never renumbers the rest.

        Only complete records are yielded. A partial record at the end
        of the file is reported (unless ``warn`` is false) and then
        abandoned, because its missing bytes cannot be told apart from
        real field data.

        A slot whose flag byte is neither space (active) nor asterisk
        (deleted) is not returned, but it still occupies its physical
        position so the numbering of the following records does not
        move.

        If ``last`` is given the scan stops after that physical record.

        ``stats``, if given, is filled with the number of complete
        record slots (``actual``), whether a partial record was found
        (``truncated``) and whether the end of the records was reached
        (``complete``).
        """
        if self.header.recordlen < 1:
            raise ValueError('record length must be at least 1 (was {})'
                             .format(self.header.recordlen))

        if stats is None:
            stats = {}
        stats['actual'] = 0
        stats['truncated'] = False
        stats['complete'] = False

        bodylen = self._record_bodylen()

        # Skip to first record.
        infile.seek(self.header.headerlen, 0)

        index = 0
        while True:
            flag, data = self._read_record(infile, bodylen)

            if flag is None or flag == b'\x1a':
                # End of file or end-of-file marker (not a record).
                stats['complete'] = True
                return

            if len(data) != bodylen:
                # A partial record. Space or NUL padding to a block
                # boundary is not a partial record and is ignored.
                if self._is_padding(flag, data):
                    stats['complete'] = True
                else:
                    stats['truncated'] = True
                    if warn:
                        self._report_truncated(index)
                return

            stats['actual'] = index + 1

            if flag in (b' ', b'*'):
                yield index, flag == b'*', data

            if last is not None and index >= last:
                return

            index += 1

    def _report_truncated(self, index):
        warnings.warn(
            'file {!r} ends with an incomplete record at physical '
            'position {}; it is not returned'.format(self.filename, index),
            TruncatedRecordWarning, stacklevel=3)

    def _report_record_count(self, actual_count):
        header_count = self.header.numrecords
        if header_count == actual_count:
            return

        warnings.warn(
            'header of {!r} says {} records but {} complete records were '
            'found (difference {})'.format(self.filename, header_count,
                                           actual_count,
                                           header_count - actual_count),
            RecordCountMismatchWarning, stacklevel=3)

    def check_record_count(self):
        """Compare the header record count with the records in the file.

        Returns a :class:`RecordCountReport` with the count from the
        header, the number of complete records found, their difference
        and whether the file ends with a partial record.

        The difference is ``header_count - actual_count``: a positive
        value means the header claims records that are not in the file.
        """
        stats = {}
        with open(self.filename, 'rb') as infile:
            for _ in self._scan_records(infile, stats=stats, warn=False):
                pass

        return RecordCountReport(self.header.numrecords,
                                 stats['actual'],
                                 self.header.numrecords - stats['actual'],
                                 stats['truncated'])

    def _make_parser(self, memofile):
        """Return a function that parses one field value."""
        if self.raw:
            def parse(_, data):
                return data
            return parse
        else:
            return self.parserclass(self, memofile).parse

    def _parse_record(self, data, parse):
        """Parse a record body with the configured record factory."""
        items = []
        offset = 0
        for field in self.fields:
            next_offset = offset + field.length
            items.append((field.name, parse(field, data[offset:next_offset])))
            offset = next_offset

        return self.recfactory(items)

    def _count_records(self, record_type=b' '):
        count = 0

        with open(self.filename, 'rb') as infile:
            # Count complete records only, so len() agrees with what
            # iteration returns even if the file ends in a partial
            # record.
            for (_, deleted, _) in self._scan_records(infile, warn=False):
                if (b'*' if deleted else b' ') == record_type:
                    count += 1

        return count

    def _count_envelopes(self):
        if self.loaded and self._envelopes is not None:
            return len(self._envelopes)

        count = 0
        with open(self.filename, 'rb') as infile:
            for _ in self._scan_records(infile, warn=False):
                count += 1

        return count

    @staticmethod
    def _validate_range(first, last):
        """Check a physical record range.

        ``first`` and ``last`` are zero-based physical record numbers
        and both are inclusive. ``None`` means open-ended.
        """
        for name, value in [('first', first), ('last', last)]:
            if value is None:
                continue
            if isinstance(value, bool) or not isinstance(value, int):
                raise TypeError(
                    '{} must be an integer or None, was {!r}'
                    .format(name, value))
            if value < 0:
                raise ValueError('{} must be non-negative, was {}'
                                 .format(name, value))

        if first is not None and last is not None and last < first:
            raise ValueError('last ({}) must not be smaller than first ({})'
                             .format(last, first))

    @staticmethod
    def _select_envelopes(envelopes, first=None, last=None):
        selected = []
        for envelope in envelopes:
            if first is not None and envelope.index < first:
                continue
            if last is not None and envelope.index > last:
                continue
            selected.append(envelope)
        return selected

    def _iter_envelopes(self, first=None, last=None, report=False):
        """Stream records with physical position and delete flag.

        The main file and the memo file (when needed) are opened once
        for the whole iteration.

        If ``report`` is true the header record count is compared with
        the file when the scan reaches the end, and a warning is issued
        on a mismatch.
        """
        self._validate_range(first, last)

        if self.loaded and self._envelopes is not None:
            for envelope in self._select_envelopes(self._envelopes,
                                                   first, last):
                yield envelope
            return

        with open(self.filename, 'rb') as infile, \
                self._open_memofile() as memofile:

            parse = self._make_parser(memofile)

            stats = {}
            for (index, deleted, data) in self._scan_records(
                    infile, last=last, stats=stats):
                if first is not None and index < first:
                    continue
                yield RecordEnvelope(index, deleted,
                                     self._parse_record(data, parse))

            # The header can only be compared with the whole file, so
            # skip the check when the scan did not reach the end.
            if report and stats['complete']:
                self._report_record_count(stats['actual'])

    def iter_records(self, first=None, last=None):
        """Iterate over records with their physical position.

        Yields :class:`RecordEnvelope` objects in file order, so active
        and deleted records are interleaved exactly as they are stored.
        Each envelope carries the zero-based physical record number
        (``index``), the delete flag (``deleted``) and the parsed field
        values (``record``).

        ``first`` and ``last`` select an inclusive range of physical
        record numbers. Numbers are positions counted from the first
        record in the file, so they do not shift when deleted records
        are skipped or when only a range is read. ``None`` means
        open-ended; values outside the file simply yield nothing.

        The result is the same whether the table is loaded or not. The
        main file (and the memo file when needed) is opened once per
        iteration, and custom ``parserclass`` and ``recfactory`` are
        used. Iterating over the table itself is unchanged and still
        returns only active records.
        """
        self._validate_range(first, last)
        return self._iter_envelopes(first=first, last=last, report=True)

    # More descriptive alias.
    iter_envelopes = iter_records

    def _iter_records(self, record_type=b' '):
        """Stream records with the given flag byte.

        This is the default iteration used by ``records``, ``deleted``
        and ``DBF.__iter__``. It returns records only and does not
        expose physical numbers or the end-of-file diagnostics.
        """
        with open(self.filename, 'rb') as infile, \
                self._open_memofile() as memofile:

            parse = self._make_parser(memofile)
            for (_, deleted, data) in self._scan_records(infile, warn=False):
                if (b'*' if deleted else b' ') == record_type:
                    yield self._parse_record(data, parse)

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
