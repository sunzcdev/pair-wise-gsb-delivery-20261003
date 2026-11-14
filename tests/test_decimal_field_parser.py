"""
Tests for the exact numeric field parser (DecimalFieldParser).
"""
import struct
from decimal import Decimal

import pytest
from pytest import raises

from dbfread import DBF, FieldParser, InvalidValue
from dbfread.field_parser import (
    DecimalFieldParser,
    NumericFormatError,
    NumericOverflowError,
    DecimalPlacesError,
    IllegalSignError,
)


class MockHeader(object):
    dbversion = 0x02


class MockDBF(object):
    def __init__(self):
        self.header = MockHeader()
        self.encoding = 'ascii'
        self.char_decode_errors = 'strict'


class MockField(object):
    def __init__(self, type='', **kwargs):
        self.type = type
        self.__dict__.update(kwargs)


def parse_n(data, decimal_count=2):
    parser = DecimalFieldParser(MockDBF())
    field = MockField('N', name='VALUE', length=len(data),
                      decimal_count=decimal_count)
    return parser.parse(field, data)


def make_dbf(path, fields, records):
    """Write a minimal DBF file.

    fields is a list of (name, type, length, decimal_count) and
    records is a list of raw record bodies (bytes without the
    deletion flag).
    """
    headerlen = 32 + 32 * len(fields) + 1
    recordlen = 1 + sum(length for (_, _, length, _) in fields)

    header = struct.pack(
        '<BBBBLHHHBBLLLBBH',
        0x03,            # dbversion
        126, 11, 14,     # date 2026-11-14 (year is years since 1900)
        len(records),
        headerlen,
        recordlen,
        0, 0, 0, 0, 0, 0, 0, 0, 0)

    parts = [header]
    for name, type_, length, decimals in fields:
        parts.append(struct.pack(
            '<11scLBBHBBBB7sB',
            name.encode('ascii'),
            type_.encode('ascii'),
            0,               # address
            length,
            decimals,
            0, 0, 0, 0, 0,   # reserved bytes
            b'\0' * 7,       # reserved
            0))              # index flag
    parts.append(b'\r')

    for record in records:
        assert len(record) == recordlen - 1
        parts.append(b' ' + record)

    parts.append(b'\x1a')

    with open(str(path), 'wb') as outfile:
        outfile.write(b''.join(parts))


# Byte samples compared against the Decimal value and its number of
# decimal places (as_tuple().exponent).
@pytest.mark.parametrize('decimal_count,raw,expected', [
    (2, b'     3.10', Decimal('3.10')),
    (2, b'      3.1', Decimal('3.10')),  # scale from decimal_count
    (2, b'        3', Decimal('3.00')),
    (2, b'       .5', Decimal('0.50')),
    (2, b'    -3.10', Decimal('-3.10')),
    (2, b'    +3.10', Decimal('3.10')),
    (2, b'     3,10', Decimal('3.10')),  # historical comma form
    (2, b'      0.1', Decimal('0.10')),  # exact, unlike float
    (4, b'   3.1415', Decimal('3.1415')),
    (0, b'        3', 3),
    (0, b'      -99', -99),
    (0, b'       +7', 7),
])
def test_byte_samples(decimal_count, raw, expected):
    value = parse_n(raw, decimal_count)
    assert value == expected
    assert type(value) is type(expected)
    if isinstance(expected, Decimal):
        # The Decimal carries the declared number of decimal places.
        assert value.as_tuple().exponent == expected.as_tuple().exponent


def test_exact_value_unlike_float():
    # 0.1 can not be represented exactly as a float.
    value = parse_n(b'0.1', 2)
    assert value == Decimal('0.10')
    assert value != 0.1


def test_pure_integer_field_returns_int():
    value = parse_n(b'  42', 0)
    assert value == 42
    assert isinstance(value, int)


def test_negative_zero_decimal_keeps_sign():
    value = parse_n(b' -0.00', 2)
    assert value == 0
    assert value.is_signed()
    assert value.as_tuple().exponent == -2


def test_negative_zero_integer_becomes_zero():
    assert parse_n(b'-0', 0) == 0


def test_empty_field_is_none():
    assert parse_n(b'      ') is None
    assert parse_n(b'\0\0\0\0') is None
    assert parse_n(b'  \0  ') is None


def test_comma_decimal_separator():
    assert parse_n(b'3,14') == Decimal('3.14')
    # Both separators in one field is ambiguous.
    with raises(NumericFormatError):
        parse_n(b'1,2.3')


def test_asterisk_padding_is_overflow():
    # All-asterisk overflow padding.
    with raises(NumericOverflowError):
        parse_n(b'******')
    # Partial padding must not be stripped and parsed as a number.
    with raises(NumericOverflowError):
        parse_n(b'0.01**')
    with raises(NumericOverflowError):
        parse_n(b'**0.01')


def test_too_many_decimal_places():
    with raises(DecimalPlacesError):
        parse_n(b'3.141', 2)
    # A fraction in a pure integer field.
    with raises(DecimalPlacesError):
        parse_n(b'3.5', 0)


def test_illegal_sign():
    for raw in (b'1-2', b'--1', b'+-1', b'1+', b'1-'):
        with raises(IllegalSignError):
            parse_n(raw)


def test_not_a_number():
    for raw in (b'okasd', b'1.2.3', b'-', b'+', b'.', b'1 2'):
        with raises(NumericFormatError):
            parse_n(raw)


def test_diagnostics_are_value_errors_and_keep_data():
    # All diagnostics are ValueErrors so existing handlers keep
    # working, and the original field text is kept in .data.
    for raw, error in [(b'******', NumericOverflowError),
                       (b'3.141', DecimalPlacesError),
                       (b'1-2', IllegalSignError),
                       (b'okasd', NumericFormatError)]:
        try:
            parse_n(raw)
        except ValueError as err:
            assert isinstance(err, error)
            assert isinstance(err, NumericFormatError)
            assert err.data == raw
        else:
            raise AssertionError('no error raised for {!r}'.format(raw))


def test_diagnostics_are_independent():
    assert not issubclass(NumericOverflowError, DecimalPlacesError)
    assert not issubclass(DecimalPlacesError, NumericOverflowError)
    assert not issubclass(IllegalSignError, NumericOverflowError)
    assert not issubclass(IllegalSignError, DecimalPlacesError)


def test_other_field_types_still_work():
    parser = DecimalFieldParser(MockDBF())
    field = MockField('C', name='NAME', length=4, decimal_count=0)
    assert parser.parse(field, b'test') == u'test'


def _write_test_table(path):
    fields = [('VALUE', 'N', 6, 2),
              ('NAME', 'C', 4, 0)]
    records = [b'******ABCD',   # overflow padding in first field
               b'  1.25WXYZ']
    make_dbf(path, fields, records)


def test_format_error_does_not_move_field_boundary(tmpdir):
    # A format error in one field must not change the read boundary
    # of the next field or record.
    path = str(tmpdir.join('test.dbf'))
    _write_test_table(path)

    class CollectingParser(DecimalFieldParser):
        def parse(self, field, data):
            try:
                return DecimalFieldParser.parse(self, field, data)
            except NumericFormatError:
                return InvalidValue(data)

    records = list(DBF(path, parserclass=CollectingParser))

    assert records[0]['VALUE'] == InvalidValue(b'******')
    # The field after the broken one is read at the right boundary.
    assert records[0]['NAME'] == u'ABCD'
    # The next record is also unaffected.
    assert records[1]['VALUE'] == Decimal('1.25')
    assert records[1]['NAME'] == u'WXYZ'


def test_raw_mode_still_returns_bytes(tmpdir):
    path = str(tmpdir.join('test.dbf'))
    _write_test_table(path)

    table = DBF(path, raw=True, parserclass=DecimalFieldParser)
    records = list(table)

    assert records[0]['VALUE'] == b'******'
    assert records[1]['VALUE'] == b'  1.25'
    assert records[1]['NAME'] == b'WXYZ'


def test_custom_parserclass_still_works(tmpdir):
    path = str(tmpdir.join('test.dbf'))
    _write_test_table(path)

    class MyFieldParser(FieldParser):
        def parseN(self, field, data):
            return 'custom'

    records = list(DBF(path, parserclass=MyFieldParser))

    assert records[0]['VALUE'] == 'custom'
    assert records[1]['VALUE'] == 'custom'
    assert records[1]['NAME'] == u'WXYZ'


def test_default_parser_unchanged(tmpdir):
    # The default parser still returns floats and strips asterisks.
    path = str(tmpdir.join('test.dbf'))
    _write_test_table(path)

    records = list(DBF(path))

    assert records[0]['VALUE'] is None       # b'******'
    assert records[1]['VALUE'] == 1.25
    assert isinstance(records[1]['VALUE'], float)
