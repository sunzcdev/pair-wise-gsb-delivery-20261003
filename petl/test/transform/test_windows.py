from __future__ import absolute_import, print_function, division


from collections import OrderedDict
from datetime import datetime, timedelta, timezone

import pytest

import petl as etl
from petl.errors import (ArgumentError, WindowDataError, WindowOrderError,
                         WindowTimezoneError)
from petl.test.helpers import ieq


def test_windowaggregate_left_closed_right_open():
    table = (('t', 'v'),
             (0, 1),
             (9, 2),
             (10, 3),
             (19, 4),
             (20, 5))

    actual = etl.windowaggregate(
        table, width=10, step=10, time='t', origin=0, aggregation=len)
    expect = (('window_start', 'window_end', 'value'),
              (0, 10, 2),
              (10, 20, 2),
              (20, 30, 1))
    ieq(expect, actual)
    ieq(expect, actual)


def test_windowaggregate_overlapping_sparse_windows():
    table = (('t', 'v'),
             (0, 1),
             (5, 2),
             (10, 3),
             (30, 4))

    actual = etl.windowaggregate(
        table, width=10, step=5, time='t', origin=0,
        aggregation={'count': len, 'sumv': ('v', sum)})
    expect = (('window_start', 'window_end', 'count', 'sumv'),
              (-5, 5, 1, 1),
              (0, 10, 2, 3),
              (5, 15, 2, 5),
              (10, 20, 1, 3),
              (25, 35, 1, 4),
              (30, 40, 1, 4))
    ieq(expect, actual)


def test_windowaggregate_origin_alignment_and_negative_windows():
    table = (('t', 'v'),
             (-25, 1),
             (-20, 2),
             (-19, 3),
             (-10, 4),
             (0, 5),
             (5, 6))

    actual = etl.windowaggregate(
        table, width=10, step=10, time='t', origin=100, aggregation=len)
    expect = (('window_start', 'window_end', 'value'),
              (-30, -20, 1),
              (-20, -10, 2),
              (-10, 0, 1),
              (0, 10, 2))
    ieq(expect, actual)


def test_windowaggregate_step_wider_than_width_leaves_gaps():
    table = (('t', 'v'),
             (0, 1),
             (6, 2),
             (10, 3),
             (11, 4),
             (20, 5))

    actual = etl.windowaggregate(
        table, width=5, step=10, time='t', origin=0, aggregation=[])
    expect = (('window_start', 'window_end'),
              (0, 5),
              (10, 15),
              (20, 25))
    ieq(expect, actual)


def test_windowaggregate_business_keys():
    table = (('k', 't', 'v'),
             ('a', 0, 1),
             ('b', 1, 10),
             ('a', 6, 2),
             ('b', 11, 20))

    actual = etl.windowaggregate(
        table, width=10, step=10, key='k', time='t', origin=0,
        aggregation={'count': len, 'sumv': ('v', sum)})
    expect = (('k', 'window_start', 'window_end', 'count', 'sumv'),
              ('a', 0, 10, 2, 3),
              ('b', 0, 10, 1, 10),
              ('b', 10, 20, 1, 20))
    ieq(expect, actual)


def test_windowaggregate_datetime_and_negative_time():
    table = (('t', 'v'),
             (datetime(1969, 12, 31, 23, 59, 50), 1),
             (datetime(1970, 1, 1, 0, 0, 5), 2),
             (datetime(1970, 1, 1, 0, 0, 10), 3))
    origin = datetime(1970, 1, 1)

    actual = etl.windowaggregate(
        table, width=timedelta(seconds=10),
        step=timedelta(seconds=10), time='t', origin=origin,
        aggregation=len)
    expect = (('window_start', 'window_end', 'value'),
              (datetime(1969, 12, 31, 23, 59, 50),
               datetime(1970, 1, 1, 0, 0, 0), 1),
              (datetime(1970, 1, 1, 0, 0, 0),
               datetime(1970, 1, 1, 0, 0, 10), 1),
              (datetime(1970, 1, 1, 0, 0, 10),
               datetime(1970, 1, 1, 0, 0, 20), 1))
    ieq(expect, actual)


def test_windowaggregate_aware_datetimes():
    table = (('t', 'v'),
             (datetime(1970, 1, 1, tzinfo=timezone.utc), 1),
             (datetime(1970, 1, 1, 0, 0, 5, tzinfo=timezone.utc), 2))
    origin = datetime(1970, 1, 1, tzinfo=timezone.utc)

    actual = etl.windowaggregate(
        table, width=timedelta(seconds=10),
        step=timedelta(seconds=10), time='t', origin=origin,
        aggregation=len)
    expect = (('window_start', 'window_end', 'value'),
              (datetime(1970, 1, 1, tzinfo=timezone.utc),
               datetime(1970, 1, 1, 0, 0, 10, tzinfo=timezone.utc), 2))
    ieq(expect, actual)


def test_windowaggregate_rejects_out_of_order_with_row_number():
    table = (('t', 'v'),
             (5, 1),
             (6, 2),
             (1, 3))

    with pytest.raises(WindowOrderError) as excinfo:
        list(etl.windowaggregate(
            table, width=10, step=5, time='t', aggregation=len))
    assert excinfo.value.row == 3
    assert excinfo.value.source_row == 4
    assert 'data row 3' in str(excinfo.value)


def test_windowaggregate_rejects_mixed_timezone_with_row_number():
    table = (('t', 'v'),
             (datetime(2020, 1, 1), 1),
             (datetime(2020, 1, 1, tzinfo=timezone.utc), 2))

    with pytest.raises(WindowTimezoneError) as excinfo:
        list(etl.windowaggregate(
            table, width=timedelta(days=1), step=timedelta(days=1),
            time='t', aggregation=len))
    assert excinfo.value.row == 2
    assert 'data row 2' in str(excinfo.value)


def test_windowaggregate_missing_event_time_reports_row():
    table = (('t', 'v'),
             (0, 1),
             ())

    with pytest.raises(WindowDataError) as excinfo:
        list(etl.windowaggregate(
            table, width=10, step=10, time='t', aggregation=len))
    assert excinfo.value.row == 2
    assert 'missing event time' in str(excinfo.value)


def test_windowaggregate_source_error_does_not_flush_active_windows():
    rows = [('t', 'v'), (0, 1), (5, 2), (20, 3)]

    def source():
        for row in rows:
            if row[0] == 20:
                raise RuntimeError('source failed')
            yield row

    view = etl.windowaggregate(
        source(), width=10, step=10, time='t', aggregation=len)
    with pytest.raises(RuntimeError, match='source failed'):
        list(view)
    assert view.state.finished is False
    assert view.state.active_window_count == 1
    assert view.state.max_active_end == 10


def test_windowaggregate_reads_source_once_for_overlapping_windows():
    rows = [('t', 'v'), (0, 1), (5, 2), (10, 3)]
    reads = []

    def source():
        for row in rows:
            reads.append(row)
            yield row

    actual = list(etl.windowaggregate(
        source(), width=10, step=5, time='t', origin=0,
        aggregation={'count': len, 'rows': list}))
    expect = (('window_start', 'window_end', 'count', 'rows'),
              (-5, 5, 1, [(0, 1)]),
              (0, 10, 2, [(0, 1), (5, 2)]),
              (5, 15, 2, [(5, 2), (10, 3)]),
              (10, 20, 1, [(10, 3)]))
    ieq(expect, actual)
    assert reads == list(rows)


def test_windowaggregate_partial_consumption_exposes_active_bound():
    rows = [('t', 'v'), (0, 1), (5, 2), (20, 3)]

    def source():
        for row in rows:
            yield row

    iterator = iter(etl.windowaggregate(
        source(), width=10, step=10, time='t', aggregation=len))
    assert next(iterator) == ('window_start', 'window_end', 'value')
    assert iterator.state.max_active_end is None

    first = next(iterator)
    assert first == (0, 10, 2)
    assert iterator.state.watermark == 20
    assert iterator.state.max_active_end == 30
    assert iterator.active_window_bound == 30
    assert iterator.state.active_window_count == 1

    remaining = list(iterator)
    assert remaining == [(20, 30, 1)]
    assert iterator.state.finished is True


def test_windowaggregate_emits_before_source_is_exhausted():
    pulled = []

    def source():
        for row in [('t', 'v'), (0, 1), (9, 2), (10, 3)]:
            pulled.append(row)
            yield row
        raise AssertionError('source was read past the closed windows')

    iterator = iter(etl.windowaggregate(
        source(), width=10, step=5, time='t', origin=0, aggregation=len))
    assert next(iterator) == ('window_start', 'window_end', 'value')

    assert next(iterator) == (-5, 5, 1)
    assert pulled == [('t', 'v'), (0, 1), (9, 2)]
    assert iterator.state.max_active_end == 15

    assert next(iterator) == (0, 10, 2)
    assert pulled[-1] == (10, 3)
    assert iterator.state.max_active_end == 20

    with pytest.raises(AssertionError, match='read past'):
        next(iterator)
    assert iterator.state.finished is False


def test_windowaggregate_aggregation_interfaces_and_custom_fields():
    table = (('t', 'v'),
             (0, 1),
             (1, 2))
    actual = etl.windowaggregate(
        table, width=2, step=2, time='t', origin=0,
        aggregation=OrderedDict([
            ('sumv', ('v', sum)),
            ('rows', list),
        ]),
        start='ws', stop='we')
    expect = (('ws', 'we', 'sumv', 'rows'),
              (0, 2, 3, [(0, 1), (1, 2)]))
    ieq(expect, actual)

    actual = etl.windowaggregate(
        table, width=2, step=2, time='t', origin=0,
        aggregation=lambda rows: sum(row['v'] for row in rows),
        field='sumv')
    expect = (('window_start', 'window_end', 'sumv'), (0, 2, 3))
    ieq(expect, actual)


def test_windowaggregate_empty_table_and_table_method():
    empty = (('t', 'v'),)
    expect = (('window_start', 'window_end', 'value'),)
    ieq(expect, etl.windowaggregate(
        empty, width=2, step=1, time='t', origin=0, aggregation=len))

    table = (('t', 'v'), (1, 1), (0, 2))
    actual = etl.wrap(table).windowaggregate(
        width=2, step=2, time='t', origin=0, aggregation=len,
        presorted=False)
    expect = (('window_start', 'window_end', 'value'), (0, 2, 2))
    ieq(expect, actual)


@pytest.mark.parametrize('width, step', [
    (0, 1),
    (1, 0),
    (-1, 1),
    (1, -1),
])
def test_windowaggregate_rejects_invalid_dimensions(width, step):
    with pytest.raises(ArgumentError):
        etl.windowaggregate(
            (('t',),), width=width, step=step, time='t',
            aggregation=len)
