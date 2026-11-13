from __future__ import absolute_import, print_function, division


import pytest

from petl.test.helpers import ieq
from petl.transform.fills import filldown, fillleft, fillright


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


def test_filldown_key_interleaved():
    table = (('device', 'reading'),
             ('A', 10),
             ('B', 20),
             ('A', None),
             ('B', None),
             ('A', 11),
             ('B', None))
    expect = (('device', 'reading'),
              ('A', 10),
              ('B', 20),
              ('A', 10),
              ('B', 20),
              ('A', 11),
              ('B', 20))

    actual = filldown(table, 'reading', key='device')
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_multiple_fields():
    table = (('site', 'device', 'reading'),
             ('x', 'A', 10),
             ('x', 'B', 20),
             ('y', 'A', 30),
             ('x', 'A', None),
             ('x', 'B', None),
             ('y', 'A', None))
    expect = (('site', 'device', 'reading'),
              ('x', 'A', 10),
              ('x', 'B', 20),
              ('y', 'A', 30),
              ('x', 'A', 10),
              ('x', 'B', 20),
              ('y', 'A', 30))

    actual = filldown(table, 'reading', key=('site', 'device'))
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_missing_key_not_shared():
    table = (('device', 'reading'),
             ('A', 10),
             (None, 99),
             (None, None),
             ('A', None),
             ('B', None),
             ('B', 20),
             ('A', None))
    expect = (('device', 'reading'),
              ('A', 10),
              (None, 99),
              (None, None),
              ('A', 10),
              ('B', None),
              ('B', 20),
              ('A', 10))

    actual = filldown(table, 'reading', key='device')
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_custom_missing():
    table = (('device', 'reading'),
             ('A', 10),
             ('B', 20),
             ('A', ''),
             ('B', ''),
             ('A', 11),
             ('B', ''))
    expect = (('device', 'reading'),
              ('A', 10),
              ('B', 20),
              ('A', 10),
              ('B', 20),
              ('A', 11),
              ('B', 20))

    actual = filldown(table, 'reading', key='device', missing='')
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_reset():
    table = (('device', 'reading', 'reset'),
             ('A', 10, False),
             ('B', 20, False),
             ('A', None, False),
             ('B', None, False),
             ('A', None, True),
             ('B', None, False),
             ('A', None, False))
    expect = (('device', 'reading', 'reset'),
              ('A', 10, False),
              ('B', 20, False),
              ('A', 10, False),
              ('B', 20, False),
              ('A', None, True),
              ('B', 20, False),
              ('A', None, False))

    actual = filldown(table, 'reading', key='device', reset='reset')
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_reset_row_can_start_new_state():
    table = (('device', 'reading', 'reset'),
             ('A', 10, False),
             ('A', None, False),
             ('A', 11, True),
             ('A', None, False))
    expect = (('device', 'reading', 'reset'),
              ('A', 10, False),
              ('A', 10, False),
              ('A', 11, True),
              ('A', 11, False))

    actual = filldown(table, 'reading', key='device', reset='reset')
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_max_missing():
    table = (('device', 'reading'),
             ('A', 10),
             ('B', 20),
             ('A', None),
             ('B', None),
             ('A', None),
             ('B', None),
             ('A', None),
             ('B', None),
             ('A', 11),
             ('B', 21),
             ('A', None),
             ('B', None))
    expect = (('device', 'reading'),
              ('A', 10),
              ('B', 20),
              ('A', 10),
              ('B', 20),
              ('A', 10),
              ('B', 20),
              ('A', None),
              ('B', None),
              ('A', 11),
              ('B', 21),
              ('A', 11),
              ('B', 21))

    actual = filldown(table, 'reading', key='device', max_missing=2)
    ieq(expect, actual)
    ieq(expect, actual)

    actual = filldown(table, 'reading', groupby='device',
                      max_consecutive_missing=2)
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_max_missing_zero():
    table = (('device', 'reading'),
             ('A', 10),
             ('A', None),
             ('A', 11),
             ('A', None))
    expect = (('device', 'reading'),
              ('A', 10),
              ('A', None),
              ('A', 11),
              ('A', None))

    actual = filldown(table, 'reading', key='device', max_missing=0)
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_multiple_targets_independent():
    table = (('device', 'reading', 'status'),
             ('A', 10, 'ok'),
             ('A', None, None),
             ('B', 20, 'warn'),
             ('A', None, 'error'),
             ('B', None, None))
    expect = (('device', 'reading', 'status'),
              ('A', 10, 'ok'),
              ('A', 10, 'ok'),
              ('B', 20, 'warn'),
              ('A', 10, 'error'),
              ('B', 20, 'warn'))

    actual = filldown(table, 'reading', 'status', key='device')
    ieq(expect, actual)
    ieq(expect, actual)


def test_filldown_key_short_rows():
    table = (('device', 'reading', 'status'),
             ('A', 10, 'ok'),
             ('A',),
             ('B',),
             ('A', None, 'warn'),
             ('B', 20))
    expect = (('device', 'reading', 'status'),
              ('A', 10, 'ok'),
              ('A', 10, 'ok'),
              ('B',),
              ('A', 10, 'warn'),
              ('B', 20))

    actual = filldown(table, 'reading', 'status', key='device')
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
