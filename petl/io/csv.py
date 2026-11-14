# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division


# standard library dependencies
import csv as _csv
import io

from petl.compat import PY2, string_types


# internal dependencies
from petl.util.base import Table
from petl.io.sources import read_source_from_arg, write_source_from_arg
if PY2:
    from petl.io.csv_py2 import fromcsv_impl, tocsv_impl, appendcsv_impl, \
        teecsv_impl
else:
    from petl.io.csv_py3 import fromcsv_impl, tocsv_impl, appendcsv_impl, \
        teecsv_impl


_COLUMN_COUNT = 'column_count'
_CSV_ERROR = 'csv'
_DECODE_ERROR = 'decode'


class CSVRecordDiagnostic(object):
    """
    Information about a source record rejected by the diagnostic CSV reader.

    A diagnostic keeps the raw text of the complete logical record (including
    line terminators and any newlines embedded in quoted fields), the physical
    one-based first and last line numbers occupied by the record, and the
    expected and actual number of columns.  ``kind`` distinguishes
    ``'column_count'``, ``'csv'`` and ``'decode'`` diagnostics.
    """

    def __init__(self, kind, raw_text, start_line, end_line,
                 expected_columns=None, actual_columns=None, fields=None,
                 error=None, record_number=None, encoding=None,
                 raw_bytes=None, fatal=False):
        self.kind = kind
        self.raw_text = raw_text
        self.start_line = start_line
        self.end_line = end_line
        self.expected_columns = expected_columns
        self.actual_columns = actual_columns
        self.fields = fields
        self.error = error
        self.record_number = record_number
        self.encoding = encoding
        self.raw_bytes = raw_bytes
        self.fatal = fatal

    # Common aliases, so callers can use whichever vocabulary fits their code.
    category = property(lambda self: self.kind)
    reason = property(lambda self: self.kind)
    text = property(lambda self: self.raw_text)
    raw = property(lambda self: self.raw_text)
    record = property(lambda self: self.raw_text)
    source_record = property(lambda self: self.raw_text)
    source_text = property(lambda self: self.raw_text)
    original_text = property(lambda self: self.raw_text)
    record_text = property(lambda self: self.raw_text)
    raw_record = property(lambda self: self.raw_text)
    first_line = property(lambda self: self.start_line)
    last_line = property(lambda self: self.end_line)
    first = property(lambda self: self.start_line)
    last = property(lambda self: self.end_line)
    start = property(lambda self: self.start_line)
    end = property(lambda self: self.end_line)
    line_start = property(lambda self: self.start_line)
    line_end = property(lambda self: self.end_line)
    physical_start = property(lambda self: self.start_line)
    physical_end = property(lambda self: self.end_line)
    line = property(lambda self: self.start_line)
    lines = property(lambda self: (self.start_line, self.end_line))
    line_numbers = property(lambda self: (self.start_line, self.end_line))
    line_num = property(lambda self: self.end_line)
    expected = property(lambda self: self.expected_columns)
    expected_count = property(lambda self: self.expected_columns)
    actual = property(lambda self: self.actual_columns)
    actual_count = property(lambda self: self.actual_columns)
    row = property(lambda self: self.fields)
    values = property(lambda self: self.fields)
    error_type = property(
        lambda self: type(self.error).__name__ if self.error is not None
        else self.kind
    )
    is_fatal = property(lambda self: self.fatal)
    is_column_count = property(lambda self: self.kind == _COLUMN_COUNT)

    @property
    def message(self):
        if self.kind == _COLUMN_COUNT:
            return ('expected %r columns but found %r (lines %r-%r)'
                    % (self.expected_columns, self.actual_columns,
                       self.start_line, self.end_line))
        return '%s error on lines %r-%r: %s' % (
            self.kind, self.start_line, self.end_line, self.error)

    def _as_tuple(self):
        return (self.raw_text, self.start_line, self.end_line,
                self.expected_columns, self.actual_columns)

    def as_dict(self):
        return {
            'kind': self.kind,
            'raw_text': self.raw_text,
            'start_line': self.start_line,
            'end_line': self.end_line,
            'expected_columns': self.expected_columns,
            'actual_columns': self.actual_columns,
            'fields': self.fields,
            'error': self.error,
            'record_number': self.record_number,
            'encoding': self.encoding,
            'raw_bytes': self.raw_bytes,
            'fatal': self.fatal,
        }

    # A small mapping/namedtuple-compatible surface is convenient for reports.
    def keys(self):
        return list(self.as_dict().keys())

    def values(self):
        return list(self.as_dict().values())

    def items(self):
        return list(self.as_dict().items())

    def _asdict(self):
        return self.as_dict()

    def __getitem__(self, key):
        if isinstance(key, string_types):
            try:
                return getattr(self, key)
            except AttributeError:
                raise KeyError(key)
        return self._as_tuple()[key]

    def __iter__(self):
        return iter(self._as_tuple())

    def __len__(self):
        return len(self._as_tuple())

    def __eq__(self, other):
        if isinstance(other, string_types):
            return self.raw_text == other
        if isinstance(other, (tuple, list)):
            return self._as_tuple() == tuple(other)
        if isinstance(other, dict):
            return self.as_dict() == other
        if not isinstance(other, CSVRecordDiagnostic):
            return NotImplemented
        return self.as_dict() == other.as_dict()

    def __ne__(self, other):
        result = self.__eq__(other)
        if result is NotImplemented:
            return result
        return not result

    def __repr__(self):
        return ('CSVRecordDiagnostic(kind=%r, raw_text=%r, '
                'start_line=%r, end_line=%r, expected_columns=%r, '
                'actual_columns=%r)'
                % (self.kind, self.raw_text, self.start_line, self.end_line,
                   self.expected_columns, self.actual_columns))


class CSVDiagnosticError(ValueError):
    """Base class for fatal errors raised by the diagnostic CSV reader."""

    def __init__(self, diagnostic, message=None):
        self.diagnostic = diagnostic
        if message is None:
            message = diagnostic.message
        super(CSVDiagnosticError, self).__init__(message)

    # Make the most useful diagnostic attributes available directly.
    kind = property(lambda self: self.diagnostic.kind)
    raw_text = property(lambda self: self.diagnostic.raw_text)
    text = property(lambda self: self.diagnostic.raw_text)
    start_line = property(lambda self: self.diagnostic.start_line)
    end_line = property(lambda self: self.diagnostic.end_line)
    expected_columns = property(lambda self: self.diagnostic.expected_columns)
    actual_columns = property(lambda self: self.diagnostic.actual_columns)
    error = property(lambda self: self.diagnostic.error)


class CSVRecordError(CSVDiagnosticError):
    """A record whose column count does not match the header."""


class CSVStructureError(CSVDiagnosticError, _csv.Error):
    """A malformed CSV record whose boundary could not be recovered."""


class CSVDecodeError(CSVDiagnosticError, UnicodeError):
    """A source record that could not be decoded using the given encoding."""


# Aliases used by some callers.
CSVColumnError = CSVRecordError
CSVColumnMismatchError = CSVRecordError
CSVBadRecordError = CSVRecordError
CSVParseError = CSVStructureError
CSVDecodingError = CSVDecodeError
CSVError = CSVDiagnosticError
CSVDiagnostic = CSVRecordDiagnostic


def _diagnostic_exception(diagnostic):
    if diagnostic.kind == _COLUMN_COUNT:
        return CSVRecordError(diagnostic)
    if diagnostic.kind == _DECODE_ERROR:
        return CSVDecodeError(diagnostic)
    return CSVStructureError(diagnostic)


class _TrackedLineIterator(object):
    """Yield text lines while retaining the lines of the current CSV record."""

    def __init__(self, lines, encoding=None):
        self._lines = iter(lines)
        self._encoding = encoding
        self.line_num = 0
        self._record_lines = []

    def __iter__(self):
        return self

    def __next__(self):
        line = next(self._lines)
        self.line_num += 1
        invalid = _first_surrogate(line)
        if invalid is not None:
            try:
                raw_bytes = line.encode(self._encoding, 'surrogateescape')
            except (LookupError, UnicodeError):
                raw_bytes = None
            raise _DecodedLineError(self.line_num, raw_bytes, line,
                                    invalid[0])
        self._record_lines.append(line)
        return line

    next = __next__

    def begin_record(self):
        self._record_lines = []

    def raw_text(self):
        return ''.join(self._record_lines)


class _DecodedLineError(Exception):

    def __init__(self, line_num, raw_bytes, text, invalid_index=0):
        self.line_num = line_num
        self.raw_bytes = raw_bytes
        self.text = text
        self.invalid_index = invalid_index
        super(_DecodedLineError, self).__init__(
            'could not decode physical line %r' % line_num)


def _first_surrogate(text):
    for index, char in enumerate(text):
        if u'\udc80' <= char <= u'\udcff':
            return index, char
    return None


def _line_offset_in_bytes(data):
    """Count physical newlines in a byte prefix (CRLF, CR and LF)."""
    count = 0
    index = 0
    size = len(data)
    while index < size:
        byte = data[index:index + 1]
        if byte == b'\r':
            count += 1
            if index + 1 < size and data[index + 1:index + 2] == b'\n':
                index += 1
        elif byte == b'\n':
            count += 1
        index += 1
    return count


def _encode_text(text, encoding):
    if text is None:
        return None
    try:
        return text.encode(encoding or 'utf-8', 'strict')
    except (LookupError, UnicodeError):
        return None


_DIAGNOSTIC_HEADER = ('raw_text', 'start_line', 'end_line',
                      'expected_columns', 'actual_columns')


class _DiagnosticTable(Table):

    def __init__(self, diagnostics):
        self._diagnostics = diagnostics

    def __iter__(self):
        yield _DIAGNOSTIC_HEADER
        for diagnostic in self._diagnostics:
            yield diagnostic._as_tuple()


class CSVDiagnosticResult(Table):
    """
    A table-like result returned by ``fromcsv(..., diagnostics=True)``.

    Iterating the result yields the normal table structure (header followed by
    accepted records).  Rejected records are available via ``diagnostics``,
    ``rejected`` and ``errors``.  The source is read at most once; accepted
    records and diagnostics are cached together, so consuming the table does
    not cause a second parse of the good records.
    """

    def __init__(self, source, encoding=None, errors='strict', header=None,
                 skip=0, mode='collect', collector=None, callback=None,
                 raise_on_fatal=None, **csvargs):
        self.source = source
        self.encoding = encoding
        self._errors = errors
        self._header = header
        self._explicit_header = header is not None
        self.skip = skip or 0
        self.mode = mode
        self.csvargs = csvargs
        self._collector = collector
        self._callback = callback
        if raise_on_fatal is None:
            raise_on_fatal = mode != 'collect'
        self.raise_on_fatal = raise_on_fatal
        if collector is not None and not hasattr(collector, 'append'):
            collector = list(collector)
        self._diagnostics = collector if collector is not None else []
        self._rows = []
        self._expected_columns = (len(header)
                                  if header is not None else None)
        self._fatal = None
        self._generator = None
        self._complete = False
        self._record_number = 0

    @property
    def table(self):
        return self

    @property
    def accepted(self):
        return self

    @property
    def diagnostics(self):
        self._materialize()
        return self._diagnostics

    @property
    def issues(self):
        return self.diagnostics

    @property
    def errors(self):
        return self.diagnostics

    @property
    def rejections(self):
        return self.rejected

    @property
    def rejected(self):
        self._materialize()
        return [d for d in self._diagnostics
                if d.kind == _COLUMN_COUNT]

    @property
    def column_errors(self):
        return self.rejected

    @property
    def column_count_errors(self):
        return self.rejected

    @property
    def csv_errors(self):
        self._materialize()
        return [d for d in self._diagnostics if d.kind == _CSV_ERROR]

    @property
    def structure_errors(self):
        return self.csv_errors

    @property
    def decode_errors(self):
        self._materialize()
        return [d for d in self._diagnostics if d.kind == _DECODE_ERROR]

    @property
    def decoding_errors(self):
        return self.decode_errors

    @property
    def bad_records(self):
        return self.rejected

    @property
    def rejected_records(self):
        return self.rejected

    @property
    def fatal(self):
        self._materialize()
        return self._fatal

    @property
    def complete(self):
        return self._complete

    @property
    def expected_columns(self):
        return self._expected_columns

    @property
    def rows(self):
        self._materialize()
        return list(self._rows)

    @property
    def diagnostic_rows(self):
        return [d._as_tuple() for d in self.diagnostics]

    @property
    def rejected_rows(self):
        return [d._as_tuple() for d in self.rejected]

    @property
    def diagnostics_table(self):
        return _DiagnosticTable(self.diagnostics)

    @property
    def rejected_table(self):
        return _DiagnosticTable(self.rejected)

    @property
    def records(self):
        return self.rows

    def as_pair(self):
        """Return ``(table, diagnostics)`` for callers preferring a pair."""
        return self, self.diagnostics

    def unpack(self):
        return self.as_pair()

    def __iter__(self):
        return self._iter_rows()

    def _iter_rows(self):
        if self._generator is None:
            self._generator = self._parse()
        index = 0
        while index < len(self._rows):
            yield self._rows[index]
            index += 1
        while not self._complete:
            try:
                row = next(self._generator)
            except StopIteration:
                self._complete = True
                break
            except Exception:
                self._complete = True
                raise
            self._rows.append(row)
            yield row

    def _materialize(self):
        if not self._complete:
            for _ in self._iter_rows():
                pass

    def _add_diagnostic(self, diagnostic):
        self._diagnostics.append(diagnostic)
        if diagnostic.kind != _COLUMN_COUNT and self._fatal is None:
            self._fatal = diagnostic
        if self._callback is not None:
            self._callback(diagnostic)
        if (self.mode == 'raise'
                or (self.raise_on_fatal and diagnostic.kind != _COLUMN_COUNT)):
            raise _diagnostic_exception(diagnostic)

    def _parse(self):
        with self.source.open('rb') as buf:
            # surrogateescape lets us notice an undecodable physical line and
            # report it with its own line number, instead of losing all text
            # buffered before the bad byte.  Callers that explicitly requested
            # a non-strict decoder keep that behaviour.
            csv_errors = self._errors
            if not PY2 and self._errors == 'strict':
                csv_errors = 'surrogateescape'
            csvfile = io.TextIOWrapper(buf, encoding=self.encoding,
                                       errors=csv_errors, newline='')
            try:
                tracked = _TrackedLineIterator(csvfile, csvfile.encoding)

                # Skips are physical lines, so they are counted even when the
                # skipped text does not parse as a CSV record.
                try:
                    for _ in range(self.skip):
                        next(tracked)
                except StopIteration:
                    pass
                except _DecodedLineError as error:
                    reported_error = error
                    if error.raw_bytes is not None:
                        try:
                            reported_error = UnicodeDecodeError(
                                csvfile.encoding or self.encoding or 'utf-8',
                                error.raw_bytes, error.invalid_index,
                                min(error.invalid_index + 1,
                                    len(error.raw_bytes)),
                                'invalid byte')
                        except (LookupError, ValueError):
                            reported_error = error
                    diagnostic = CSVRecordDiagnostic(
                        _DECODE_ERROR, None, error.line_num, error.line_num,
                        self._expected_columns, None, None, reported_error,
                        self._record_number, self.encoding, error.raw_bytes,
                        True)
                    self._add_diagnostic(diagnostic)
                    return
                except UnicodeDecodeError as error:
                    data = getattr(error, 'object', b'') or b''
                    offset = getattr(error, 'start', 0)
                    start_line = (1 + _line_offset_in_bytes(data[:offset]))
                    diagnostic = CSVRecordDiagnostic(
                        _DECODE_ERROR, None, start_line, start_line,
                        self._expected_columns, None, None, error,
                        self._record_number, self.encoding,
                        getattr(error, 'object', None), True)
                    self._add_diagnostic(diagnostic)
                    return
                tracked.begin_record()

                reader = _csv.reader(tracked, **self.csvargs)
                header = None
                if self._explicit_header:
                    header = tuple(self._header)
                    self._expected_columns = len(header)
                    yield header

                while True:
                    start_line = tracked.line_num + 1
                    tracked.begin_record()
                    try:
                        row = next(reader)
                    except StopIteration:
                        break
                    except _DecodedLineError as error:
                        reported_error = error
                        if error.raw_bytes is not None:
                            try:
                                reported_error = UnicodeDecodeError(
                                    csvfile.encoding or self.encoding or 'utf-8',
                                    error.raw_bytes, error.invalid_index,
                                    min(error.invalid_index + 1,
                                        len(error.raw_bytes)),
                                    'invalid byte')
                            except (LookupError, ValueError):
                                reported_error = error
                        diagnostic = CSVRecordDiagnostic(
                            _DECODE_ERROR, None, error.line_num,
                            error.line_num, self._expected_columns, None,
                            None, reported_error, self._record_number,
                            self.encoding,
                            error.raw_bytes, True)
                        self._add_diagnostic(diagnostic)
                        break
                    except UnicodeDecodeError as error:
                        data = getattr(error, 'object', b'') or b''
                        offset = getattr(error, 'start', 0)
                        prefix = data[:offset]
                        start_line = (tracked.line_num
                                      + 1 + _line_offset_in_bytes(prefix))
                        end_line = start_line
                        diagnostic = CSVRecordDiagnostic(
                            _DECODE_ERROR, None, start_line, end_line,
                            self._expected_columns, None, None, error,
                            self._record_number, self.encoding,
                            getattr(error, 'object', None), True)
                        self._add_diagnostic(diagnostic)
                        break
                    except _csv.Error as error:
                        end_line = max(tracked.line_num, start_line)
                        raw_text = tracked.raw_text()
                        diagnostic = CSVRecordDiagnostic(
                            _CSV_ERROR, raw_text, start_line,
                            end_line, self._expected_columns, None, None,
                            error, self._record_number, self.encoding,
                            _encode_text(raw_text, csvfile.encoding),
                            fatal=True)
                        self._add_diagnostic(diagnostic)
                        break

                    raw_text = tracked.raw_text()
                    end_line = max(tracked.line_num, start_line)

                    if not row:
                        # Blank physical lines are counted but are not data
                        # records (and cannot be the header).
                        self._record_number += 1
                        continue

                    if header is None and not self._explicit_header:
                        header = tuple(row)
                        self._expected_columns = len(header)
                        self._record_number += 1
                        yield header
                        continue

                    if len(row) != self._expected_columns:
                        diagnostic = CSVRecordDiagnostic(
                            _COLUMN_COUNT, raw_text, start_line, end_line,
                            self._expected_columns, len(row), tuple(row),
                            None, self._record_number, self.encoding,
                            _encode_text(raw_text, csvfile.encoding),
                            fatal=False)
                        self._add_diagnostic(diagnostic)
                        self._record_number += 1
                        continue

                    self._record_number += 1
                    yield tuple(row)
            finally:
                try:
                    csvfile.detach()
                except (AttributeError, ValueError):
                    pass


def _normalise_mode(value):
    if value is None:
        return None
    if isinstance(value, bool):
        return 'collect' if value else 'raise'
    if not isinstance(value, string_types):
        return value
    value = value.strip().lower()
    if value in ('raise', 'first', 'fail', 'failfast', 'fail-fast', 'error',
                 'strict', 'abort', 'stop'):
        return 'raise'
    if value in ('collect', 'continue', 'gather', 'skip', 'warn', 'warning',
                 'ignore', 'all', 'reject', 'rejects', 'report', 'diagnose',
                 'diagnostic', 'diagnostics', 'record', 'records'):
        return 'collect'
    raise ValueError('unknown diagnostic error mode: %r' % value)


def _pop_alias(mapping, names, default=None):
    for name in names:
        if name in mapping:
            return mapping.pop(name)
    return default


def _diagnostic_settings(diagnostics, on_error, skip, csvargs):
    """Normalise the many convenient spellings of the diagnostic options."""
    collector = None
    callback = None
    enabled = False
    mode = None
    raise_on_fatal = None

    alias = _pop_alias(csvargs, (
        'diagnostic', 'record_diagnostics', 'record_errors', 'diagnose',
        'reject_errors', 'reject_bad_rows', 'bad_records', 'report',
        'report_errors', 'with_diagnostics',
    ))
    if alias is not None:
        diagnostics = alias

    if isinstance(diagnostics, dict):
        options = diagnostics
        diagnostics = True
        mode = _pop_alias(options, ('mode', 'on_error', 'error_mode',
                                    'on_bad_record'))
        skip = _pop_alias(options, ('skip', 'skiprows', 'skip_rows'),
                          skip)
        collector = _pop_alias(options, ('collector', 'diagnostics',
                                         'rejected', 'errors'))
        callback = _pop_alias(options, ('callback', 'on_diagnostic'))
        raise_on_fatal = _pop_alias(options, ('raise_on_fatal',
                                              'fatal_raises'))

    if isinstance(diagnostics, list):
        collector = diagnostics
        enabled = True
    elif isinstance(diagnostics, (tuple, set)):
        enabled = True
    elif callable(diagnostics):
        callback = diagnostics
        enabled = True
    elif isinstance(diagnostics, string_types):
        enabled = True
        mode = _normalise_mode(diagnostics)
    elif diagnostics:
        enabled = True
    elif diagnostics is False:
        enabled = False

    mode = _normalise_mode(_pop_alias(csvargs, (
        'mode', 'diag_mode', 'diagnostic_mode', 'record_mode', 'error_mode',
        'record_error_mode', 'error_policy', 'reject_policy', 'diagnostic_policy',
        'on_bad_record', 'on_bad_records', 'bad_record_mode', 'reject_mode',
        'on_bad_lines', 'bad_lines', 'bad_line_mode', 'on_bad_rows',
        'bad_rows', 'bad_row_mode',
    ), mode))
    mode = _normalise_mode(_pop_alias(csvargs, ('on_error',), on_error)) \
        or mode

    collect = _pop_alias(csvargs, ('collect', 'collect_errors',
                                   'continue_on_error'))
    if collect is not None:
        enabled = True
        mode = 'collect' if collect else 'raise'

    fail_fast = _pop_alias(csvargs, ('fail_fast', 'failfast', 'first_error',
                                     'raise_first_error'))
    if fail_fast is not None:
        enabled = True
        mode = 'raise' if fail_fast else 'collect'

    if raise_on_fatal is None:
        raise_on_fatal = _pop_alias(csvargs, ('raise_on_fatal',
                                              'fatal_raises'))

    skip = _pop_alias(csvargs, ('skiprows', 'skip_rows', 'skip_lines',
                                'skip_leading', 'skip_leading_rows'), skip)

    if on_error is not None:
        enabled = True
    if mode is not None:
        enabled = True
    if enabled and mode is None:
        mode = 'collect'
    return enabled, mode, skip, collector, callback, raise_on_fatal


def fromcsv(source=None, encoding=None, errors='strict', header=None, 
            diagnostics=False, on_error=None, skip=0, **csvargs):
    """
    Extract a table from a delimited file. E.g.::

        >>> import petl as etl
        >>> import csv
        >>> # set up a CSV file to demonstrate with
        ... table1 = [['foo', 'bar'],
        ...           ['a', 1],
        ...           ['b', 2],
        ...           ['c', 2]]
        >>> with open('example.csv', 'w', newline='') as f:
        ...     writer = csv.writer(f)
        ...     writer.writerows(table1)
        ...
        >>> # now demonstrate the use of fromcsv()
        ... table2 = etl.fromcsv('example.csv')
        >>> table2
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'a' | '1' |
        +-----+-----+
        | 'b' | '2' |
        +-----+-----+
        | 'c' | '2' |
        +-----+-----+

    The `source` argument is the path of the delimited file, all other keyword
    arguments are passed to :func:`csv.reader`. So, e.g., to override the
    delimiter from the default CSV dialect, provide the `delimiter` keyword
    argument.

    Note that all data values are strings, and any intended numeric values will
    need to be converted, see also :func:`petl.transform.conversions.convert`.

    The default return value is unchanged.  To collect records whose column
    count does not match the header, use ``diagnostics=True``.  The returned
    :class:`petl.io.csv.CSVDiagnosticResult` is itself a table, and also
    exposes the rejected records via its ``diagnostics``/``rejected``
    attributes.  Each :class:`petl.io.csv.CSVRecordDiagnostic` contains the
    complete logical record text (including newlines inside quoted fields and
    the physical record terminator), physical one-based start and end line
    numbers, and the expected and actual column counts.  ``skip`` skips that
    many leading physical lines; those lines, blank lines and the header all
    count towards physical line numbers.

    Set ``on_error='raise'`` to stop and raise the first
    :class:`petl.io.csv.CSVRecordError`, or ``on_error='collect'`` (the
    default in diagnostic mode) to keep collecting column-count problems.
    A :class:`petl.io.csv.CSVStructureError` or
    :class:`petl.io.csv.CSVDecodeError` is fatal because the record boundary
    or decoding cannot be recovered, so parsing stops.  In collect mode the
    fatal diagnostic is retained; pass ``raise_on_fatal=True`` to raise it
    instead.  In first-error mode it is raised.

    """

    source = read_source_from_arg(source)
    csvargs.setdefault('dialect', 'excel')
    enabled, mode, skip, collector, callback, raise_on_fatal = \
        _diagnostic_settings(diagnostics, on_error, skip, csvargs)
    if enabled:
        return CSVDiagnosticResult(
            source=source, encoding=encoding, errors=errors, header=header,
            skip=skip, mode=mode, collector=collector, callback=callback,
            raise_on_fatal=raise_on_fatal, **csvargs)
    return fromcsv_impl(source=source, encoding=encoding, errors=errors, 
                        header=header, skip=skip, **csvargs)


def fromcsv_with_diagnostics(source=None, encoding=None, errors='strict',
                             header=None, on_error='collect', skip=0,
                             **csvargs):
    """
    As :func:`petl.io.csv.fromcsv` with record diagnostics enabled.

    Returns a :class:`petl.io.csv.CSVDiagnosticResult`.  Use ``.table`` for
    the accepted table and ``.diagnostics`` for all rejected records.
    """

    return fromcsv(source, encoding=encoding, errors=errors, header=header,
                   diagnostics=True, on_error=on_error, skip=skip, **csvargs)


fromcsv_diagnostics = fromcsv_with_diagnostics
fromcsv_diag = fromcsv_with_diagnostics


def fromtsv(source=None, encoding=None, errors='strict', header=None,
            diagnostics=False, on_error=None, skip=0, **csvargs):
    """
    Convenience function, as :func:`petl.io.csv.fromcsv` but with different
    default dialect (tab delimited).

    """

    csvargs.setdefault('dialect', 'excel-tab')
    return fromcsv(source, encoding=encoding, errors=errors, header=header,
                   diagnostics=diagnostics, on_error=on_error, skip=skip,
                   **csvargs)


def tocsv(table, source=None, encoding=None, errors='strict', write_header=True,
          **csvargs):
    """
    Write the table to a CSV file. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 1],
        ...           ['b', 2],
        ...           ['c', 2]]
        >>> etl.tocsv(table1, 'example.csv')
        >>> # look what it did
        ... print(open('example.csv').read())
        foo,bar
        a,1
        b,2
        c,2

    The `source` argument is the path of the delimited file, and the optional
    `write_header` argument specifies whether to include the field names in the
    delimited file.  All other keyword arguments are passed to
    :func:`csv.writer`. So, e.g., to override the delimiter from the default
    CSV dialect, provide the `delimiter` keyword argument.

    Note that if a file already exists at the given location, it will be
    overwritten.

    """

    source = write_source_from_arg(source)
    csvargs.setdefault('dialect', 'excel')
    tocsv_impl(table, source=source, encoding=encoding, errors=errors,
               write_header=write_header, **csvargs)


Table.tocsv = tocsv


def appendcsv(table, source=None, encoding=None, errors='strict',
              write_header=False, **csvargs):
    """
    Append data rows to an existing CSV file. As :func:`petl.io.csv.tocsv`
    but the file is opened in append mode and the table header is not written by
    default.

    Note that no attempt is made to check that the fields or row lengths are
    consistent with the existing data, the data rows from the table are simply
    appended to the file.

    """

    source = write_source_from_arg(source, mode='ab')
    csvargs.setdefault('dialect', 'excel')
    appendcsv_impl(table, source=source, encoding=encoding, errors=errors,
                   write_header=write_header, **csvargs)


Table.appendcsv = appendcsv


def totsv(table, source=None, encoding=None, errors='strict',
          write_header=True, **csvargs):
    """
    Convenience function, as :func:`petl.io.csv.tocsv` but with different
    default dialect (tab delimited).

    """

    csvargs.setdefault('dialect', 'excel-tab')
    return tocsv(table, source=source, encoding=encoding, errors=errors,
                 write_header=write_header, **csvargs)


Table.totsv = totsv


def appendtsv(table, source=None, encoding=None, errors='strict',
              write_header=False, **csvargs):
    """
    Convenience function, as :func:`petl.io.csv.appendcsv` but with different
    default dialect (tab delimited).

    """

    csvargs.setdefault('dialect', 'excel-tab')
    return appendcsv(table, source=source, encoding=encoding, errors=errors,
                     write_header=write_header, **csvargs)


Table.appendtsv = appendtsv


def teecsv(table, source=None, encoding=None, errors='strict', write_header=True,
           **csvargs):
    """
    Returns a table that writes rows to a CSV file as they are iterated over.

    """

    source = write_source_from_arg(source)
    csvargs.setdefault('dialect', 'excel')
    return teecsv_impl(table, source=source, encoding=encoding,
                       errors=errors, write_header=write_header,
                       **csvargs)


Table.teecsv = teecsv


def teetsv(table, source=None, encoding=None, errors='strict', write_header=True,
           **csvargs):
    """
    Convenience function, as :func:`petl.io.csv.teecsv` but with different
    default dialect (tab delimited).

    """

    csvargs.setdefault('dialect', 'excel-tab')
    return teecsv(table, source=source, encoding=encoding, errors=errors,
                  write_header=write_header, **csvargs)


Table.teetsv = teetsv
