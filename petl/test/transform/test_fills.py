from __future__ import absolute_import, print_function, division


import datetime
from decimal import Decimal

import pytest

from petl.test.helpers import ieq
from petl.transform.fills import filldown, fillleft, filllinear, fillright


def test_filldown():

    table = (('foo', 'bar', 'baz'),
             (1, 'a', None),
             (1, None, .23),
             (1, 'b', None),
             (2, None, None),
             (2, None, .56),
             (2, 'c', None),
             (None, 'c', .72))

    actual = filldown(table)
    expect = (('foo', 'bar', 'baz'),
              (1, 'a', None),
              (1, 'a', .23),
              (1, 'b', .23),
              (2, 'b', .23),
              (2, 'b', .56),
              (2, 'c', .56),
              (2, 'c', .72))
    ieq(expect, actual)
    ieq(expect, actual)

    actual = filldown(table, 'bar')
    expect = (('foo', 'bar', 'baz'),
              (1, 'a', None),
              (1, 'a', .23),
              (1, 'b', None),
              (2, 'b', None),
              (2, 'b', .56),
              (2, 'c', None),
              (None, 'c', .72))
    ieq(expect, actual)
    ieq(expect, actual)

    actual = filldown(table, 'foo', 'bar')
    expect = (('foo', 'bar', 'baz'),
              (1, 'a', None),
              (1, 'a', .23),
              (1, 'b', None),
              (2, 'b', None),
              (2, 'b', .56),
              (2, 'c', None),
              (2, 'c', .72))
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_headerless():
    table = []
    actual = filldown(table, 'foo')
    expect = []
    ieq(expect, actual)


def test_filldown_header_only():
    table = (('foo', 'bar', 'baz'),)
    expect = (('foo', 'bar', 'baz'),)

    actual = filldown(table)
    ieq(expect, actual)
    ieq(expect, actual)

    actual = filldown(table, 'bar')
    ieq(expect, actual)
    ieq(expect, actual)


@pytest.mark.parametrize('header', [
    (2024, 2025),
    (0, 0),
    ('value', 0),
    ('value', 'value'),
])
@pytest.mark.parametrize('missing', [None, ''])
def test_filldown_all_columns_by_position(header, missing):
    table = (header,
             (missing, missing),
             (1, 10),
             (missing, missing),
             (2, missing),
             (missing, 20))
    expect = (header,
              (missing, missing),
              (1, 10),
              (1, 10),
              (2, 10),
              (2, 20))
    actual = filldown(table, missing=missing)
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_numeric_header_only():
    table = ((2024, 2025),)
    actual = filldown(table)
    ieq(table, actual)
    ieq(table, actual)


@pytest.mark.parametrize('field', [1, '2025'])
def test_filldown_explicit_field_with_numeric_header(field):
    table = ((2024, 2025),
             (1, 10),
             (None, None))
    expect = ((2024, 2025),
              (1, 10),
              (None, 10))
    actual = filldown(table, field)
    ieq(expect, actual)
    ieq(expect, actual)


def test_fillright():

    table = (('foo', 'bar', 'baz'),
             (1, 'a', None),
             (1, None, .23),
             (1, 'b', None),
             (2, None, None),
             (2, None, .56),
             (2, 'c', None),
             (None, 'c', .72))

    actual = fillright(table)
    expect = (('foo', 'bar', 'baz'),
              (1, 'a', 'a'),
              (1, 1, .23),
              (1, 'b', 'b'),
              (2, 2, 2),
              (2, 2, .56),
              (2, 'c', 'c'),
              (None, 'c', .72))
    ieq(expect, actual)
    ieq(expect, actual)


def test_fillright_headerless():
    table = []
    actual = fillright(table, 'foo')
    expect = []
    ieq(expect, actual)


def test_fillleft():

    table = (('foo', 'bar', 'baz'),
             (1, 'a', None),
             (1, None, .23),
             (1, 'b', None),
             (2, None, None),
             (None, None, .56),
             (2, 'c', None),
             (None, 'c', .72))

    actual = fillleft(table)
    expect = (('foo', 'bar', 'baz'),
              (1, 'a', None),
              (1, .23, .23),
              (1, 'b', None),
              (2, None, None),
              (.56, .56, .56),
              (2, 'c', None),
              ('c', 'c', .72))
    ieq(expect, actual)
    ieq(expect, actual)


def test_fillleft_headerless():
    table = []
    actual = fillleft(table, 'foo')
    expect = []
    ieq(expect, actual)


def test_filllinear():

    table = (('id', 'time', 'value'),
             ('a', 1, 10.0),
             ('a', 2, None),
             ('a', 3, None),
             ('a', 4, 16.0),
             ('b', 1, None),
             ('b', 2, 5.0),
             ('b', 3, None))

    actual = filllinear(table, 'time', 'value', by='id')
    expect = (('id', 'time', 'value'),
              ('a', 1, 10.0),
              ('a', 2, 12.0),
              ('a', 3, 14.0),
              ('a', 4, 16.0),
              ('b', 1, None),
              ('b', 2, 5.0),
              ('b', 3, None))
    ieq(expect, actual)
    ieq(expect, actual)


def test_filllinear_no_group():

    table = (('time', 'value'),
             (1, 0.0),
             (2, None),
             (3, None),
             (4, None),
             (5, 8.0))

    actual = filllinear(table, 'time', 'value')
    expect = (('time', 'value'),
              (1, 0.0),
              (2, 2.0),
              (3, 4.0),
              (4, 6.0),
              (5, 8.0))
    ieq(expect, actual)


def test_filllinear_other_fields_unchanged():

    table = (('id', 'time', 'label', 'value'),
             ('a', 1, 'p', 0.0),
             ('a', 2, 'q', None),
             ('a', 3, 'r', 4.0))

    actual = filllinear(table, 'time', 'value', by='id')
    expect = (('id', 'time', 'label', 'value'),
              ('a', 1, 'p', 0.0),
              ('a', 2, 'q', 2.0),
              ('a', 3, 'r', 4.0))
    ieq(expect, actual)


def test_filllinear_maxgap():

    table = (('time', 'value'),
             (1, 0.0),
             (2, None),
             (3, None),
             (10, 9.0),
             (11, None),
             (12, 11.0))

    actual = filllinear(table, 'time', 'value', maxgap=5)
    expect = (('time', 'value'),
              (1, 0.0),
              (2, None),
              (3, None),
              (10, 9.0),
              (11, 10.0),
              (12, 11.0))
    ieq(expect, actual)


def test_filllinear_datetime_timezone():

    utc = datetime.timezone.utc
    plus1 = datetime.timezone(datetime.timedelta(hours=1))
    table = (('time', 'value'),
             (datetime.datetime(2026, 1, 1, 0, 0, tzinfo=utc), 0.0),
             (datetime.datetime(2026, 1, 1, 0, 30, tzinfo=utc), None),
             # 02:00 at +01:00 is 01:00 UTC, one hour after the first row
             (datetime.datetime(2026, 1, 1, 2, 0, tzinfo=plus1), 60.0))

    actual = filllinear(table, 'time', 'value')
    expect = (('time', 'value'),
              (datetime.datetime(2026, 1, 1, 0, 0, tzinfo=utc), 0.0),
              (datetime.datetime(2026, 1, 1, 0, 30, tzinfo=utc), 30.0),
              (datetime.datetime(2026, 1, 1, 2, 0, tzinfo=plus1), 60.0))
    ieq(expect, actual)


def test_filllinear_datetime_maxgap():

    utc = datetime.timezone.utc
    table = (('time', 'value'),
             (datetime.datetime(2026, 1, 1, 0, 0, tzinfo=utc), 0.0),
             (datetime.datetime(2026, 1, 1, 1, 0, tzinfo=utc), None),
             (datetime.datetime(2026, 1, 1, 2, 0, tzinfo=utc), 20.0),
             (datetime.datetime(2026, 1, 2, 2, 0, tzinfo=utc), None),
             (datetime.datetime(2026, 1, 3, 2, 0, tzinfo=utc), 68.0))

    actual = filllinear(table, 'time', 'value',
                        maxgap=datetime.timedelta(hours=3))
    expect = (('time', 'value'),
              (datetime.datetime(2026, 1, 1, 0, 0, tzinfo=utc), 0.0),
              (datetime.datetime(2026, 1, 1, 1, 0, tzinfo=utc), 10.0),
              (datetime.datetime(2026, 1, 1, 2, 0, tzinfo=utc), 20.0),
              (datetime.datetime(2026, 1, 2, 2, 0, tzinfo=utc), None),
              (datetime.datetime(2026, 1, 3, 2, 0, tzinfo=utc), 68.0))
    ieq(expect, actual)


def test_filllinear_decimal():

    table = (('time', 'value'),
             (1, Decimal('1.0')),
             (2, None),
             (3, None),
             (4, Decimal('2.2')))

    actual = filllinear(table, 'time', 'value')
    rows = list(actual)
    expect = (('time', 'value'),
              (1, Decimal('1.0')),
              (2, Decimal('1.4')),
              (3, Decimal('1.8')),
              (4, Decimal('2.2')))
    ieq(expect, rows)
    for row in rows[1:]:
        assert isinstance(row[1], Decimal)


def test_filllinear_datetime_decimal():

    utc = datetime.timezone.utc
    table = (('time', 'value'),
             (datetime.datetime(2026, 1, 1, 0, 0, tzinfo=utc),
              Decimal('0.0')),
             (datetime.datetime(2026, 1, 1, 0, 30, tzinfo=utc), None),
             (datetime.datetime(2026, 1, 1, 1, 0, tzinfo=utc),
              Decimal('1.5')))

    actual = filllinear(table, 'time', 'value')
    rows = list(actual)
    assert rows[2][1] == Decimal('0.75')
    assert isinstance(rows[2][1], Decimal)


def test_filllinear_bad_coordinate():

    table = (('time', 'value'),
             (1, 0.0),
             (2, None),
             (2, None),
             (3, 6.0))
    actual = filllinear(table, 'time', 'value')
    with pytest.raises(ValueError):
        list(actual)

    table = (('time', 'value'),
             (1, 0.0),
             (3, None),
             (2, 6.0))
    actual = filllinear(table, 'time', 'value')
    with pytest.raises(ValueError):
        list(actual)


def test_filllinear_buffersize_error():

    table = (('time', 'value'),
             (1, 0.0),
             (2, None),
             (3, None),
             (4, None),
             (5, 8.0))
    actual = filllinear(table, 'time', 'value', buffersize=2)
    with pytest.raises(ValueError):
        list(actual)


def test_filllinear_buffersize_keep():

    table = (('time', 'value'),
             (1, 0.0),
             (2, None),
             (3, None),
             (4, None),
             (5, 8.0),
             (6, None),
             (7, 12.0))
    actual = filllinear(table, 'time', 'value', buffersize=2,
                        onoverflow='keep')
    expect = (('time', 'value'),
              (1, 0.0),
              (2, None),
              (3, None),
              (4, None),
              (5, 8.0),
              (6, 10.0),
              (7, 12.0))
    ieq(expect, actual)


def test_filllinear_headerless():
    table = []
    actual = filllinear(table, 'time', 'value')
    expect = []
    ieq(expect, actual)
