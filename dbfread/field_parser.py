"""
Parser for DBF fields.
"""
import sys
import datetime
import struct
from decimal import Decimal
from .memo import BinaryMemo

PY2 = sys.version_info[0] == 2

if PY2:
    decode_text = unicode  # noqa: F821
else:
    decode_text = str


class InvalidValue(bytes):
    def __repr__(self):
        text = bytes.__repr__(self)
        if PY2:
            # Make sure the string starts with "b'" in
            # "InvalidValue(b'value here')".
            text = 'b' + text

        return 'InvalidValue({})'.format(text)


class NumericFormatError(ValueError):
    """Base class for errors raised by the exact numeric parser.

    This is a subclass of ``ValueError``, so existing error handlers
    keep working. The original field data is available in the
    ``data`` attribute, so no information is lost when a value can
    not be parsed.
    """
    def __init__(self, message, data=None):
        ValueError.__init__(self, message)
        self.data = data


class NumericOverflowError(NumericFormatError):
    """The field contains '*' overflow padding.

    Some programs fill a numeric field with '*' when a value does not
    fit in the declared field width. These bytes are never stripped,
    since that could turn an overflowed value into a valid-looking
    number.
    """


class DecimalPlacesError(NumericFormatError):
    """The value has more decimal places than the declared
    ``decimal_count`` of the field."""


class IllegalSignError(NumericFormatError):
    """The field contains a misplaced or repeated sign character."""


class FieldParser:
    def __init__(self, table, memofile=None):
        """Create a new field parser

        encoding is the character encoding to use when parsing
        strings."""
        self.table = table
        self.dbversion = self.table.header.dbversion
        self.encoding = table.encoding
        self.char_decode_errors = table.char_decode_errors
        self._lookup = self._create_lookup_table()
        if memofile:
            self.get_memo = memofile.__getitem__
        else:
            self.get_memo = lambda x: None

    def decode_text(self, text):
        return decode_text(text, self.encoding, errors=self.char_decode_errors)

    def _create_lookup_table(self):
        """Create a lookup table for field types."""
        lookup = {}

        for name in dir(self):
            if name.startswith('parse'):
                field_type = name[5:]
                if len(field_type) == 1:
                    lookup[field_type] = getattr(self, name)
                elif len(field_type) == 2:
                    # Hexadecimal ASCII code for field name.
                    # Example: parse2B() ('+' field)
                    field_type = chr(int(field_type, 16))
                    lookup[field_type] = getattr(self, name)

        return lookup

    def field_type_supported(self, field_type):
        """Checks if the field_type is supported by the parser

        field_type should be a one-character string like 'C' and 'N'.
        Returns a boolen which is True if the field type is supported.
        """
        return field_type in self._lookup

    def parse(self, field, data):
        """Parse field and return value"""
        try:
            func = self._lookup[field.type]
        except KeyError:
            raise ValueError('Unknown field type: {!r}'.format(field.type))
        else:
            return func(field, data)

    def parse0(self, field, data):
        """Parse flags field and return as byte string"""
        return data

    def parseC(self, field, data):
        """Parse char field and return unicode string"""
        return self.decode_text(data.rstrip(b'\0 '))

    def parseD(self, field, data):
        """Parse date field and return datetime.date or None"""
        try:
            return datetime.date(int(data[:4]), int(data[4:6]), int(data[6:8]))
        except ValueError:
            if data.strip(b' 0\0') == b'':
                # A record containing only spaces and/or zeros is
                # a NULL value.
                return None
            else:
                raise ValueError('invalid date {!r}'.format(data))

    def parseF(self, field, data):
        """Parse float field and return float or None"""
        # In some files * is used for padding.
        data = data.strip().strip(b'*')

        if data:
            return float(data)
        else:
            return None

    def parseI(self, field, data):
        """Parse integer or autoincrement field and return int."""
        # Todo: is this 4 bytes on every platform?
        return struct.unpack('<i', data)[0]

    def parseL(self, field, data):
        """Parse logical field and return True, False or None"""
        if data in b'TtYy':
            return True
        elif data in b'FfNn':
            return False
        elif data in b'? \0':
            return None
        else:
            # Todo: return something? (But that would be misleading!)
            message = 'Illegal value for logical field: {!r}'
            raise ValueError(message.format(data))

    def _parse_memo_index(self, data):
        if len(data) == 4:
            return struct.unpack('<I', data)[0]
        else:
            try:
                return int(data)
            except ValueError:
                if data.strip(b' \x00') == b'':
                    return 0
                else:
                    raise ValueError(
                        'Memo index is not an integer: {!r}'.format(data))

    def parseM(self, field, data):
        """Parse memo field (M, G, B or P)

        Returns memo index (an integer), which can be used to look up
        the corresponding memo in the memo file.
        """
        memo = self.get_memo(self._parse_memo_index(data))
        # Visual FoxPro allows binary data in memo fields.
        # These should not be decoded as string.
        if isinstance(memo, BinaryMemo):
            return memo
        else:
            if memo is None:
                return None
            else:
                return self.decode_text(memo)

    def parseN(self, field, data):
        """Parse numeric field (N)

        Returns int, float or None if the field is empty.
        """
        # In some files * is used for padding.
        data = data.strip().strip(b'*\0')

        try:
            return int(data)
        except ValueError:
            if not data.strip():
                return None
            else:
                # Account for , in numeric fields
                return float(data.replace(b',', b'.'))

    def parseO(self, field, data):
        """Parse long field (O) and return float."""
        return struct.unpack('d', data)[0]

    def parseT(self, field, data):
        """Parse time field (T)

        Returns datetime.datetime or None"""
        # Julian day (32-bit little endian)
        # Milliseconds since midnight (32-bit little endian)
        #
        # "The Julian day or Julian day number (JDN) is the number of days
        # that have elapsed since 12 noon Greenwich Mean Time (UT or TT) on
        # Monday, January 1, 4713 BC in the proleptic Julian calendar
        # 1. That day is counted as Julian day zero. The Julian day system
        # was intended to provide astronomers with a single system of dates
        # that could be used when working with different calendars and to
        # unify different historical chronologies." - wikipedia.org

        # Offset from julian days (used in the file) to proleptic Gregorian
        # ordinals (used by the datetime module)
        offset = 1721425  # Todo: will this work?

        if data.strip():
            # Note: if the day number is 0, we return None
            # I've seen data where the day number is 0 and
            # msec is 2 or 4. I think we can safely return None for those.
            # (At least I hope so.)
            #
            day, msec = struct.unpack('<LL', data)
            if day:
                dt = datetime.datetime.fromordinal(day - offset)
                delta = datetime.timedelta(seconds=msec / 1000)
                return dt + delta
            else:
                return None
        else:
            return None

    def parseY(self, field, data):
        """Parse currency field (Y) and return decimal.Decimal.

        The field is encoded as a 8-byte little endian integer
        with 4 digits of precision."""
        value = struct.unpack('<q', data)[0]

        # Currency fields are stored with 4 points of precision
        return Decimal(value) / 10000

    def parseB(self, field, data):
        """Binary memo field or double precision floating point number

        dBase uses B to represent a memo index (10 bytes), while
        Visual FoxPro uses it to store a double precision floating
        point number (8 bytes).
        """
        if self.dbversion in [0x30, 0x31, 0x32]:
            return struct.unpack('d', data)[0]
        else:
            return self.get_memo(self._parse_memo_index(data))

    def parseG(self, field, data):
        """OLE Object stored in memofile.

        The raw data is returned as a binary string."""
        return self.get_memo(self._parse_memo_index(data))

    def parseP(self, field, data):
        """Picture stored in memofile.

        The raw data is returned as a binary string."""
        return self.get_memo(self._parse_memo_index(data))

    # Autoincrement field ('+')
    parse2B = parseI

    # Timestamp field ('@')
    parse40 = parseT

    # Varchar field ('V') (Visual FoxPro)
    parseV = parseC


class DecimalFieldParser(FieldParser):
    """Field parser that parses numeric (N) fields exactly.

    The default parser returns floats for non-integer N fields, which
    loses precision when fixed-point data is migrated. This parser
    instead uses the declared field width and ``decimal_count``:

    * fields with ``decimal_count == 0`` (pure integers) are returned
      as ``int``
    * other fields are returned as ``decimal.Decimal`` carrying
      exactly ``decimal_count`` decimal places
    * all-empty fields (only spaces and/or NUL bytes) return ``None``
    * historical files using ``,`` as the decimal separator are
      accepted (``3,14`` becomes ``Decimal('3.14')``)
    * negative zero keeps its sign in decimal fields
      (``Decimal('-0.00')``) and becomes ``0`` in integer fields

    Unlike the default parser, '*' overflow padding is never
    stripped, since that could turn an overflowed value into a
    valid-looking number. Format problems raise dedicated exceptions
    (all subclasses of ``NumericFormatError`` and ``ValueError``):

    * ``DecimalPlacesError`` -- more decimal places than the declared
      ``decimal_count``
    * ``NumericOverflowError`` -- '*' overflow padding in the field
    * ``IllegalSignError`` -- misplaced or repeated sign character
    * ``NumericFormatError`` -- anything else that is not a number

    Since fields are read as fixed-width byte strings before they are
    parsed, a format error never changes the read boundary of the
    next field.
    """

    def parseN(self, field, data):
        """Parse numeric field (N) and return int, Decimal or None."""
        decimals = field.decimal_count

        # An all-empty field (only spaces and/or NUL bytes) is NULL.
        if not data.strip(b' \0'):
            return None

        # In some files '*' is used as overflow padding. Unlike the
        # default parser we never strip these, since that could turn
        # an overflowed value into a valid-looking number.
        if b'*' in data:
            raise NumericOverflowError(
                'numeric field contains overflow padding: '
                '{!r}'.format(data),
                data)

        # Strip leading and trailing padding.
        text = data.strip(b' \0')

        # The sign must be a single leading '+' or '-'.
        sign = b''
        if text[:1] in (b'+', b'-'):
            sign, text = text[:1], text[1:]
        if b'+' in text or b'-' in text:
            raise IllegalSignError(
                'misplaced or repeated sign in numeric field: '
                '{!r}'.format(data),
                data)

        # Some historical files use ',' as the decimal separator.
        text = text.replace(b',', b'.')
        if text.count(b'.') > 1:
            raise NumericFormatError(
                'multiple decimal separators in numeric field: '
                '{!r}'.format(data),
                data)

        if b'.' in text:
            intpart, fracpart = text.split(b'.')
        else:
            intpart, fracpart = text, b''

        if not (intpart or fracpart):
            # Only a sign and/or a decimal point.
            raise NumericFormatError(
                'not a number: {!r}'.format(data), data)

        if ((intpart and not intpart.isdigit()) or
                (fracpart and not fracpart.isdigit())):
            raise NumericFormatError(
                'not a number: {!r}'.format(data), data)

        if len(fracpart) > decimals:
            raise DecimalPlacesError(
                'value has {} decimal places but field declares '
                '{}: {!r}'.format(len(fracpart), decimals, data),
                data)

        if decimals:
            # Normalize the fraction to the declared number of
            # decimal places, so the returned Decimal carries the
            # declared scale. (Negative zero keeps its sign.)
            text = (sign + (intpart or b'0') + b'.' +
                    fracpart.ljust(decimals, b'0'))
            return Decimal(text.decode('ascii'))
        else:
            # Pure integer field. (Negative zero becomes int 0.)
            return int(sign + (intpart or b'0'))
