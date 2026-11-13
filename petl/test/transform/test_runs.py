from __future__ import absolute_import, print_function, division


import pytest

from petl.test.helpers import ieq, eq_
from petl.transform.runs import runlength, RunBreakError, _MISSING


def test_runlength_basic():
    table = (('foo', 'bar', 'baz'),
             ('a', 1, True),
             ('a', 2, True),
             ('b', 3, True),
             ('b', 4, False),
             ('a', 5, False))

    actual = runlength(table, 'foo', value='bar')
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),
              ('a', 2, 1, 2, 1, 2),
              ('b', 2, 3, 4, 3, 4),
              ('a', 1, 5, 5, 5, 5))
    ieq(expect, actual)
    ieq(expect, actual)


def test_runlength_repeated_key_makes_new_run():
    # a key that reappears after being interrupted must start a new run
    table = (('foo', 'bar'),
             ('x', 1),
             ('x', 2),
             ('y', 3),
             ('x', 4))

    actual = runlength(table, 'foo', value='bar')
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),
              ('x', 2, 1, 2, 1, 2),
              ('y', 1, 3, 3, 3, 3),
              ('x', 1, 4, 4, 4, 4))
    ieq(expect, actual)


def test_runlength_index_key_and_value():
    table = (('foo', 'bar'),
             ('a', 1),
             ('a', 2),
             ('b', 3))

    actual = runlength(table, 0, value=1)
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),
              ('a', 2, 1, 2, 1, 2),
              ('b', 1, 3, 3, 3, 3))
    ieq(expect, actual)


def test_runlength_compound_key():
    table = (('foo', 'bar', 'baz'),
             ('a', 1, 'p'),
             ('a', 1, 'q'),
             ('a', 2, 'r'),
             ('a', 2, 's'),
             ('b', 1, 't'))

    actual = runlength(table, ('foo', 'bar'), value='baz')
    expect = (('foo', 'bar', 'count', 'start', 'end', 'first', 'last'),
              ('a', 1, 2, 1, 2, 'p', 'q'),
              ('a', 2, 2, 3, 4, 'r', 's'),
              ('b', 1, 1, 5, 5, 't', 't'))
    ieq(expect, actual)


def test_runlength_multiple_values():
    table = (('foo', 'bar', 'baz'),
             ('a', 1, 10),
             ('a', 2, 20),
             ('b', 3, 30))

    actual = runlength(table, 'foo', value=('bar', 'baz'))
    expect = (('foo', 'count', 'start', 'end',
               'first_bar', 'first_baz', 'last_bar', 'last_baz'),
              ('a', 2, 1, 2, 1, 10, 2, 20),
              ('b', 1, 3, 3, 3, 30, 3, 30))
    ieq(expect, actual)


def test_runlength_no_value():
    table = (('foo', 'bar'),
             ('a', 1),
             ('a', 2),
             ('b', 3))

    actual = runlength(table, 'foo')
    expect = (('foo', 'count', 'start', 'end'),
              ('a', 2, 1, 2),
              ('b', 1, 3, 3))
    ieq(expect, actual)


def test_runlength_custom_column_names():
    table = (('foo', 'bar'),
             ('a', 1),
             ('a', 2),
             ('b', 3))

    actual = runlength(table, 'foo', value='bar', count='n',
                       start='lo', end='hi', first='head', last='tail')
    expect = (('foo', 'n', 'lo', 'hi', 'head', 'tail'),
              ('a', 2, 1, 2, 1, 2),
              ('b', 1, 3, 3, 3, 3))
    ieq(expect, actual)


def test_runlength_empty_and_header_only():
    ieq([], runlength([], 'foo'))

    table = (('foo', 'bar'),)
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),)
    ieq(expect, runlength(table, 'foo', value='bar'))


def test_runlength_missing_column_distinct_from_none():
    # row 2 has a genuine None in bar; row 3 is short and lacks bar entirely
    table = (('foo', 'bar'),
             ('a', 1),
             ('a', None),
             ('a',),
             ('b', 9))

    actual = list(runlength(table, 'foo', value='bar'))
    rows = actual[1:]
    # key segmentation only sees foo, so the 'a' rows stay one run even
    # though bar alternates between a real None and a missing column
    eq_(2, len(rows))
    key, count, start, end, first_val, last_val = rows[0]
    eq_(('a', 3, 1, 3, 1, _MISSING), rows[0])
    assert first_val == 1
    assert last_val is _MISSING
    assert last_val is not None

    # and the None row itself is not confused with the missing column
    none_row = table[2]
    missing_row = table[3]
    assert none_row[1] is None
    assert len(missing_row) == 1


def test_runlength_real_none_value_is_preserved():
    table = (('foo', 'bar'),
             ('a', None),
             ('a', None),
             ('b', 1))

    actual = list(runlength(table, 'foo', value='bar'))
    eq_((('foo', 'count', 'start', 'end', 'first', 'last'),
         ('a', 2, 1, 2, None, None),
         ('b', 1, 3, 3, 1, 1)),
        tuple(actual))
    assert actual[1][4] is None
    assert actual[1][5] is None


def test_runlength_missing_key_column():
    table = (('foo', 'bar'),
             ('a', 1),
             ('a', 2),
             (None, 3),
             (),
             ('a', 4))

    actual = list(runlength(table, 'foo', value='bar'))
    # None key and the missing-column sentinel key are different runs
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),
              ('a', 2, 1, 2, 1, 2),
              (None, 1, 3, 3, 3, 3),
              (_MISSING, 1, 4, 4, _MISSING, _MISSING),
              ('a', 1, 5, 5, 4, 4))
    ieq(expect, actual)


def test_runlength_breakat_splits_on_gap():
    table = (('id', 'ts'),
             ('a', 1),
             ('a', 2),
             ('a', 9),
             ('a', 10),
             ('b', 11))

    def gap_too_large(prev, cur):
        return cur['ts'] - prev['ts'] > 5

    actual = runlength(table, 'id', value='ts', breakat=gap_too_large)
    expect = (('id', 'count', 'start', 'end', 'first', 'last'),
              ('a', 2, 1, 2, 1, 2),
              ('a', 2, 3, 4, 9, 10),
              ('b', 1, 5, 5, 11, 11))
    ieq(expect, actual)


def test_runlength_breakat_index_access():
    table = (('id', 'ts'),
             ('a', 1),
             ('a', 8),
             ('a', 9))

    def gap_too_large(prev, cur):
        return cur[1] - prev[1] > 5

    actual = runlength(table, 'id', value='ts', breakat=gap_too_large)
    expect = (('id', 'count', 'start', 'end', 'first', 'last'),
              ('a', 1, 1, 1, 1, 1),
              ('a', 2, 2, 3, 8, 9))
    ieq(expect, actual)


def test_runlength_breakat_error_keeps_source_position():
    table = (('id', 'ts'),
             ('a', 1),
             ('a', 2),
             ('a', 'boom'),
             ('a', 4))

    def bad_predicate(prev, cur):
        return cur['ts'] - prev['ts'] > 5

    view = runlength(table, 'id', value='ts', breakat=bad_predicate)
    with pytest.raises(RunBreakError) as excinfo:
        list(view)

    err = excinfo.value
    # the predicate fails while examining the third data row
    eq_(3, err.rownum)
    eq_(('a', 'boom'), err.row)
    assert isinstance(err.cause, TypeError)
    assert err.__cause__ is err.cause


def test_runlength_is_streaming_with_one_row_lookahead():
    seen = []

    def source():
        yield ('foo', 'bar')
        for i, key in enumerate(('a', 'a', 'b', 'b', 'a'), start=1):
            seen.append(i)
            yield (key, i)

    view = runlength(source(), 'foo', value='bar')
    it = iter(view)
    eq_(('foo', 'count', 'start', 'end', 'first', 'last'), next(it))

    # pulling the first run only requires reading up to the first row of the
    # next run (row 3); later rows must not have been consumed
    first_run = next(it)
    eq_(('a', 2, 1, 2, 1, 2), first_run)
    eq_([1, 2, 3], seen)

    rest = list(it)
    eq_((('b', 2, 3, 4, 3, 4),
         ('a', 1, 5, 5, 5, 5)), tuple(rest))
    eq_([1, 2, 3, 4, 5], seen)


def test_runlength_does_not_sort():
    # ordering is preserved exactly; no global reordering happens
    table = (('foo', 'bar'),
             ('c', 1),
             ('a', 2),
             ('a', 3),
             ('b', 4),
             ('c', 5))

    actual = runlength(table, 'foo', value='bar')
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),
              ('c', 1, 1, 1, 1, 1),
              ('a', 2, 2, 3, 2, 3),
              ('b', 1, 4, 4, 4, 4),
              ('c', 1, 5, 5, 5, 5))
    ieq(expect, actual)


def test_runlength_table_method_and_top_level_export():
    import petl as etl
    table = etl.wrap((('foo', 'bar'),
                      ('a', 1),
                      ('a', 2)))
    expect = (('foo', 'count', 'start', 'end', 'first', 'last'),
              ('a', 2, 1, 2, 1, 2))
    ieq(expect, etl.runlength(table, 'foo', value='bar'))
    ieq(expect, table.runlength('foo', value='bar'))
    assert etl.RunBreakError is RunBreakError
