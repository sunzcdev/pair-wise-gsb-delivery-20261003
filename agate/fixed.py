"""
This module contains a generic parser for fixed-width files. It operates
similar to Python's built-in CSV reader.
"""

from collections import OrderedDict, namedtuple

Field = namedtuple('Field', ['name', 'start', 'length'])

LAYOUT_CHARACTER = 'character'
LAYOUT_BYTE = 'byte'

SHORT_PAD = 'pad'
SHORT_ERROR = 'error'

TRAILING_IGNORE = 'ignore'
TRAILING_ERROR = 'error'

_LAYOUT_ALIASES = {
    'character': LAYOUT_CHARACTER,
    'char': LAYOUT_CHARACTER,
    'text': LAYOUT_CHARACTER,
    'string': LAYOUT_CHARACTER,
    'str': LAYOUT_CHARACTER,
    'byte': LAYOUT_BYTE,
    'bytes': LAYOUT_BYTE,
    'binary': LAYOUT_BYTE,
}

_SHORT_ALIASES = {
    'pad': SHORT_PAD,
    'truncate': SHORT_PAD,
    'shorten': SHORT_PAD,
    'fill': SHORT_PAD,
    'error': SHORT_ERROR,
    'raise': SHORT_ERROR,
    'strict': SHORT_ERROR,
}

_TRAILING_ALIASES = {
    'ignore': TRAILING_IGNORE,
    'truncate': TRAILING_IGNORE,
    'skip': TRAILING_IGNORE,
    'error': TRAILING_ERROR,
    'raise': TRAILING_ERROR,
    'strict': TRAILING_ERROR,
}

# Common alternative keyword argument names accepted by :class:`Reader`.
_KWARG_ALIASES = {
    'layout_mode': 'layout',
    'mode': 'layout',
    'byte_layout': 'layout',
    'on_short_record': 'short_records',
    'short_record_policy': 'short_records',
    'short': 'short_records',
    'on_trailing_bytes': 'trailing_bytes',
    'trailing': 'trailing_bytes',
    'extra_bytes': 'trailing_bytes',
}


def _resolve(value, aliases, label):
    if isinstance(value, bool):
        normalized = 'byte' if value else 'character'
    else:
        normalized = str(value).lower()

    try:
        return aliases[normalized]
    except KeyError:
        raise ValueError('Invalid %s "%s". Valid options are: %s.' % (
            label, value, ', '.join(sorted(set(aliases)))))


class Reader:
    """
    Reads a fixed-width file using a column schema in CSV format.

    This works almost exactly like Python's built-in CSV reader.

    Schemas must be in the "ffs" format, with :code:`column`, :code:`start`,
    and :code:`length` columns. There is a repository of such schemas
    maintained at `wireservice/ffs <https://github.com/wireservice/ffs>`_.

    :param f:
        A file-like object. It must be a text stream for the character layout
        and a binary stream (opened with :code:`rb`) for the byte layout. The
        caller is responsible for closing it.
    :param schema_f:
        A text file-like object containing the "ffs" schema.
    :param layout:
        How to interpret :code:`start` and :code:`length` in the schema.
        :code:`"character"` (the default) slices the decoded text string, as
        in previous versions. :code:`"byte"` slices raw bytes and decodes
        each field independently, so multibyte encodings no longer shift the
        field boundaries.
    :param encoding:
        Character encoding used to decode each field when :code:`layout` is
        :code:`"byte"`. Defaults to UTF-8.
    :param short_records:
        Strategy for records that end before a field's range: :code:`"pad"`
        (the default) keeps whatever content is present, as in previous
        versions; :code:`"error"` raises a :class:`ValueError`.
    :param trailing_bytes:
        Strategy for bytes or characters left over after the last field:
        :code:`"ignore"` (the default) silently discards them, as in previous
        versions; :code:`"error"` raises a :class:`ValueError`.
    """
    def __init__(self, f, schema_f, layout=LAYOUT_CHARACTER, encoding='utf-8',
                 short_records=SHORT_PAD, trailing_bytes=TRAILING_IGNORE, **kwargs):
        from agate import csv

        for alias, canonical in _KWARG_ALIASES.items():
            if alias in kwargs:
                if canonical == 'layout':
                    layout = kwargs.pop(alias)
                elif canonical == 'short_records':
                    short_records = kwargs.pop(alias)
                else:
                    trailing_bytes = kwargs.pop(alias)

        # Boolean flags are accepted as a shorthand for the byte layout.
        for flag in ('byte_mode', 'binary'):
            if flag in kwargs:
                layout = kwargs.pop(flag)

        if kwargs:
            raise TypeError('Unexpected keyword argument(s): %s.' % ', '.join(sorted(kwargs)))

        self.file = f
        self.layout = _resolve(layout, _LAYOUT_ALIASES, 'layout')
        self.encoding = encoding
        self.short_records = _resolve(short_records, _SHORT_ALIASES, 'short_records option')
        self.trailing_bytes = _resolve(trailing_bytes, _TRAILING_ALIASES, 'trailing_bytes option')
        self.line_num = 0
        self.fields = []

        reader = csv.reader(schema_f)
        header = next(reader)

        if header != ['column', 'start', 'length']:
            raise ValueError('Schema must contain exactly three columns: "column", "start", and "length".')

        seen_names = set()

        for row in reader:
            name = row[0]
            start = int(row[1])
            length = int(row[2])

            if length < 0:
                raise ValueError('Schema field "%s" has a negative length of %d.' % (name, length))

            if name in seen_names:
                raise ValueError('Schema contains a duplicate column name "%s".' % name)

            seen_names.add(name)
            self.fields.append(Field(name, start, length))

        ordered = sorted(self.fields, key=lambda field: field.start)

        for previous, current in zip(ordered, ordered[1:]):
            if current.start < previous.start + previous.length:
                raise ValueError(
                    'Schema fields "%s" (%d:%d) and "%s" (%d:%d) have overlapping ranges.' % (
                        previous.name, previous.start, previous.start + previous.length,
                        current.name, current.start, current.start + current.length))

    def __iter__(self):
        return self

    def __next__(self):
        return self._parse_line(self._next_line())

    def _next_line(self):
        line = next(self.file)
        self.line_num += 1

        if self.layout == LAYOUT_BYTE:
            if not isinstance(line, bytes):
                raise TypeError('Reader requires a binary file (opened in "rb" mode) when layout="byte".')

            line = self._strip_newline(line, bytes)
        else:
            if not isinstance(line, str):
                raise TypeError('Reader requires a text file when layout="character".')

            line = self._strip_newline(line, str)

        return line

    @staticmethod
    def _strip_newline(line, kind):
        if kind is bytes:
            if line.endswith(b'\r\n'):
                return line[:-2]
            if line.endswith(b'\n') or line.endswith(b'\r'):
                return line[:-1]
        else:
            if line.endswith('\r\n'):
                return line[:-2]
            if line.endswith('\n') or line.endswith('\r'):
                return line[:-1]

        return line

    def _parse_line(self, line):
        width = len(line)
        last_end = max((field.start + field.length for field in self.fields), default=0)

        if self.trailing_bytes == TRAILING_ERROR and width > last_end:
            unit = 'bytes' if self.layout == LAYOUT_BYTE else 'characters'
            raise ValueError(
                'Record %d contains %d extra trailing %s after the last field '
                '(record width %d, schema width %d).' % (
                    self.line_num, width - last_end, unit, width, last_end))

        values = []

        for field in self.fields:
            end = field.start + field.length

            if end > width and self.short_records == SHORT_ERROR:
                raise ValueError(
                    'Record %d is shorter than field "%s" requires (record width %d, field range %d:%d).' % (
                        self.line_num, field.name, width, field.start, end))

            chunk = line[field.start:end]

            if self.layout == LAYOUT_BYTE:
                try:
                    value = chunk.decode(self.encoding)
                except UnicodeDecodeError:
                    raise ValueError(
                        'Field "%s" of record %d ends in the middle of a multibyte character; '
                        'byte range %d:%d is not valid %s.' % (
                            field.name, self.line_num, field.start, end, self.encoding))
            else:
                value = chunk

            values.append(value.strip())

        return values

    @property
    def fieldnames(self):
        """
        The names of the columns read from the schema.
        """
        return [field.name for field in self.fields]


class DictReader(Reader):
    """
    A fixed-width reader that returns :class:`collections.OrderedDict` rather
    than a list.
    """
    def __next__(self):
        line = self._next_line()
        values = OrderedDict()

        for field, value in zip(self.fields, self._parse_line(line)):
            values[field.name] = value

        return values


def reader(*args, **kwargs):
    """
    A wrapper around :class:`.fixed.Reader`, so that it can be used in the same
    way as a normal CSV reader.
    """
    return Reader(*args, **kwargs)
