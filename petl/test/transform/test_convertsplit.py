# -*- coding: utf-8 -*-
from __future__ import absolute_import, print_function, division

import glob
import os
import tempfile

import pytest

from petl.errors import ArgumentError
from petl.test.helpers import ieq
from petl.transform.conversions import convertsplit, REJECT_HEADER


class CountingConverter(object):
    """Converter that records how many times it is called."""

    def __init__(self):
        self.count = 0

    def __call__(self, value):
        self.count += 1
        return int(value)


class OneShotTable(object):
    """Table-like source whose underlying iterator can only be read once."""

    def __init__(self, rows):
        self._rows = rows
        self._iterated = False

    def __iter__(self):
        if self._iterated:
            raise AssertionError('source can only be traversed once')
        self._iterated = True
        return iter(self._rows)


def test_convertsplit_basic():
    table = (('foo', 'bar'),
             ('a', '1'),
             ('b', 'x'),
             ('c', '2'))

    success, reject = convertsplit(table, 'bar', int)

    expect_success = (('foo', 'bar'),
                      ('a', 1),
                      ('c', 2))
    ieq(expect_success, success)

    expect_reject = (REJECT_HEADER,
                     (2, ('b', 'x'), ('bar',), ('ValueError',)))
    ieq(expect_reject, reject)


def test_convertsplit_multiple_failures_one_record():
    table = (('foo', 'bar'),
             ('x', 'y'),
             ('1', '2'))

    success, reject = convertsplit(table, ('foo', 'bar'), int)

    expect_success = (('foo', 'bar'),
                      (1, 2))
    ieq(expect_success, success)

    expect_reject = (
        REJECT_HEADER,
        (1, ('x', 'y'), ('foo', 'bar'),
         ('ValueError', 'ValueError')),
    )
    ieq(expect_reject, reject)
    assert len(list(reject)) == 2


def test_convertsplit_reject_keeps_original_row():
    table = (('foo', 'bar'),
             ('a', '1'),
             ('b', 'bad'))

    success, reject = convertsplit(table, 'bar', int)
    rejected = list(reject)
    assert rejected[1][1] == ('b', 'bad')
    # rownum counts data rows only and is one based
    assert rejected[1][0] == 2


def test_convertsplit_where_bypass_goes_to_success():
    table = (('foo', 'bar'),
             ('a', '1'),
             ('b', 'x'),
             ('c', '3'))

    success, reject = convertsplit(table, 'bar', int,
                                   where=lambda r: r.foo != 'b')

    expect_success = (('foo', 'bar'),
                      ('a', 1),
                      ('b', 'x'),
                      ('c', 3))
    ieq(expect_success, success)
    ieq((REJECT_HEADER,), reject)


def test_convertsplit_where_expression_string():
    table = (('foo', 'bar'),
             ('a', '1'),
             ('b', 'x'))

    success, reject = convertsplit(table, 'bar', int,
                                   where="{foo} == 'a'")
    ieq((('foo', 'bar'), ('a', 1), ('b', 'x')), success)
    ieq((REJECT_HEADER,), reject)


def test_convertsplit_pass_row():
    table = (('foo', 'bar'),
             ('2', '1'),
             ('bad', '2'))

    def conv(value, row):
        return int(value) + int(row.bar)

    success, reject = convertsplit(table, 'foo', conv, pass_row=True)
    expect_success = (('foo', 'bar'),
                      (3, '1'))
    ieq(expect_success, success)
    rows = list(reject)
    assert rows[1][0] == 2
    assert rows[1][2] == ('foo',)
    assert rows[1][3] == ('ValueError',)


def test_convertsplit_converter_called_once_per_value():
    table = (('foo', 'bar'),
             ('1', 'x'),
             ('2', '3'),
             ('y', '4'))
    counter = CountingConverter()

    success, reject = convertsplit(table, ('foo', 'bar'), counter)

    # consume reject first, then success
    reject_rows = list(reject)
    success_rows = list(success)

    assert counter.count == 6
    # two rows fail (2 reject records including the header), one succeeds
    assert len(reject_rows) == 3
    assert len(success_rows) == 2


@pytest.mark.parametrize('order', ['success-first', 'reject-first'])
def test_convertsplit_consumption_order_does_not_change_result(order):
    table = (('foo', 'bar'),
             ('1', 'x'),
             ('2', '3'),
             ('y', '4'),
             ('5', '6'))
    counter = CountingConverter()
    success, reject = convertsplit(table, ('foo', 'bar'), counter)

    if order == 'success-first':
        success_rows = list(success)
        reject_rows = list(reject)
    else:
        reject_rows = list(reject)
        success_rows = list(success)

    # every one of the 8 field values was converted exactly once, regardless
    # of which table was consumed first
    assert counter.count == 8
    assert success_rows == [
        ('foo', 'bar'), (2, 3), (5, 6)
    ]
    assert [row[0] for row in reject_rows[1:]] == [1, 3]


def test_convertsplit_single_pass_source():
    table = OneShotTable((('foo', 'bar'),
                          ('1', '2'),
                          ('x', '3')))

    success, reject = convertsplit(table, 'foo', int)

    first_success = list(success)
    first_reject = list(reject)
    assert first_success == [('foo', 'bar'), (1, '2')]
    assert first_reject[1] == (2, ('x', '3'), ('foo',), ('ValueError',))

    # a fresh traversal is allowed but each traversal itself runs once
    second_success = list(success)
    second_reject = list(reject)
    assert second_success == first_success
    assert second_reject == first_reject


def test_convertsplit_interleaved_consumption():
    rows = [('foo', 'bar')]
    for i in range(20):
        rows.append((str(i), 'bad' if i % 3 == 0 else str(i)))
    table = tuple(rows)

    success, reject = convertsplit(table, 'bar', int, buffersize=2)

    sit = iter(success)
    rit = iter(reject)
    got_success = [next(sit)]
    got_reject = [next(rit)]
    for _ in range(4):
        got_success.append(next(sit))
        got_reject.append(next(rit))
    got_success.extend(sit)
    got_reject.extend(rit)

    expected_success_rows = [r for r in rows[1:] if r[1] != 'bad']
    assert len(got_success) == 1 + len(expected_success_rows)
    assert got_success[1:] == [
        (foo, int(bar)) for foo, bar in expected_success_rows
    ]
    expected_reject_rows = [r for r in rows[1:] if r[1] == 'bad']
    assert len(got_reject) == 1 + len(expected_reject_rows)
    for expected_rownum, record in zip(
            [i + 1 for i in range(20) if i % 3 == 0], got_reject[1:]):
        assert record[0] == expected_rownum
        assert record[2] == ('bar',)
        assert record[3] == ('ValueError',)


def test_convertsplit_spills_to_tempfile_and_cleans_up():
    tmpdir = tempfile.gettempdir()
    before = set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
    try:
        rows = [('foo', 'bar')]
        for i in range(20):
            rows.append((str(i), 'bad' if i % 2 else str(i)))
        table = tuple(rows)

        success, reject = convertsplit(table, 'bar', int, buffersize=2)

        def spill_files():
            return (set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
                    - before)

        # driving one branch far ahead of the other forces spilling
        success_rows = list(success)
        assert len(success_rows) == 11
        assert spill_files()

        reject_rows = list(reject)
        assert len(reject_rows) == 1 + 10

        # the spill is still available while the views are alive
        assert spill_files()

        # explicit close releases every cache and temp file
        success.close()
        assert not spill_files()
    finally:
        pass


def test_convertsplit_tempfiles_removed_when_views_garbage_collected():
    import gc
    import weakref

    tmpdir = tempfile.gettempdir()
    before = set(glob.glob(os.path.join(tmpdir, '*.petl-split')))

    def make_driver_ref():
        from petl.transform.conversions import _ConvertSplitter
        rows = [('foo', 'bar')]
        for i in range(12):
            rows.append((str(i), 'bad' if i % 2 else str(i)))
        splitter = _ConvertSplitter(tuple(rows), {'bar': int},
                                    buffersize=1)
        success, reject = splitter.tables()
        list(success)
        list(reject)
        return weakref.ref(splitter._driver)

    driver_ref = make_driver_ref()
    gc.collect()
    assert driver_ref() is None
    leftover = (set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
                - before)
    assert not leftover


def test_convertsplit_explicit_close_removes_tempfiles():
    tmpdir = tempfile.gettempdir()
    before = set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
    try:
        rows = [('foo', 'bar')]
        for i in range(10):
            rows.append((str(i), 'bad' if i % 2 else str(i)))
        table = tuple(rows)

        success, reject = convertsplit(table, 'bar', int, buffersize=1)
        list(success)  # run ahead and spill the reject branch
        assert (set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
                - before)

        success.close()
        assert not (set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
                    - before)
    finally:
        pass


def test_convertsplit_close_from_either_view_is_idempotent():
    tmpdir = tempfile.gettempdir()
    before = set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
    rows = [('foo',)] + [(str(i),) for i in range(12)]
    success, reject = convertsplit(rows, 'foo', int, buffersize=1)
    list(success)
    assert (set(glob.glob(os.path.join(tmpdir, '*.petl-split'))) - before)

    # closing through the rejection table releases the same buffers
    reject.close()
    assert not (set(glob.glob(os.path.join(tmpdir, '*.petl-split')))
                - before)
    # repeated closes are harmless
    success.close()
    reject.close()


def test_convertsplit_producer_error_propagates_to_both_branches():
    class BoomTable(object):

        def __iter__(self):
            yield ('foo', 'bar')
            yield ('1', '2')
            raise RuntimeError('source exploded')

    success, reject = convertsplit(BoomTable(), 'bar', int)

    with pytest.raises(RuntimeError):
        list(success)

    # a fresh traversal surfaces the same failure on the other branch
    with pytest.raises(RuntimeError):
        list(reject)


def test_convertsplit_converter_exception_goes_to_reject_table():
    # an ordinary converter exception is a rejected row, not a fatal error,
    # and the converter is still only called once per value
    calls = {'n': 0}

    def conv(value):
        calls['n'] += 1
        if calls['n'] == 3:
            raise RuntimeError('converter exploded')
        return int(value)

    table = (('foo',), ('1',), ('2',), ('3',), ('4',))
    success, reject = convertsplit(table, 'foo', conv)

    ieq((('foo',), (1,), (2,), (4,)), success)
    rejected = list(reject)
    assert rejected[1] == (3, ('3',), ('foo',), ('RuntimeError',))
    assert calls['n'] == 4


def test_convertsplit_source_error_after_partial_output():
    class PartialBoomTable(object):

        def __iter__(self):
            yield ('foo',)
            yield ('1',)
            yield ('2',)
            raise RuntimeError('source exploded')

    success, reject = convertsplit(PartialBoomTable(), 'foo', int)

    with pytest.raises(RuntimeError):
        list(success)
    with pytest.raises(RuntimeError):
        list(reject)


def test_convertsplit_empty_table():
    success, reject = convertsplit((), 'foo', int)
    assert list(success) == []
    ieq((REJECT_HEADER,), reject)


def test_convertsplit_header_only_table():
    table = (('foo', 'bar'),)
    success, reject = convertsplit(table, 'bar', int)
    ieq(table, success)
    ieq((REJECT_HEADER,), reject)


def test_convertsplit_rejects_failonerror_kwargs():
    table = (('foo',), ('1',))
    with pytest.raises(ArgumentError):
        convertsplit(table, 'foo', int, failonerror=True)
    with pytest.raises(ArgumentError):
        convertsplit(table, 'foo', int, errorvalue=None)


def test_convertsplit_dict_converter():
    table = (('foo', 'bar'),
             ('A', '1'),
             ('B', '2'))
    success, reject = convertsplit(
        table, 'foo', {'A': 'a'}, where=lambda r: r.foo == 'A'
    )
    ieq((('foo', 'bar'), ('a', '1'), ('B', '2')), success)
    ieq((REJECT_HEADER,), reject)


def test_convertsplit_buffersize_zero_spills_everything():
    table = (('foo',),
             ('1',),
             ('x',),
             ('2',))

    success, reject = convertsplit(table, 'foo', int, buffersize=0)

    ieq((('foo',), (1,), (2,)), success)
    ieq((REJECT_HEADER,
         (2, ('x',), ('foo',), ('ValueError',))), reject)


def test_convertsplit_branch_can_be_re_iterated():
    table = (('foo',),
             ('1',),
             ('x',))

    success, reject = convertsplit(table, 'foo', int)

    first = list(success)
    second = list(success)
    assert first == second == [('foo',), (1,)]

    first_reject = list(reject)
    second_reject = list(reject)
    assert first_reject == second_reject


def test_convertsplit_method_name_converter():
    table = (('foo', 'bar'),
             ('A', '1'),
             ('B', 'bad'))
    success, reject = convertsplit(table, 'foo', 'lower')
    ieq((('foo', 'bar'), ('a', '1'), ('b', 'bad')), success)
    ieq((REJECT_HEADER,), reject)


def test_convertsplit_index_based_field():
    table = (('foo', 'bar'),
             ('1', 'a'),
             ('x', 'b'))
    success, reject = convertsplit(table, 0, int)
    ieq((('foo', 'bar'), (1, 'a')), success)
    rows = list(reject)
    assert rows[1] == (2, ('x', 'b'), ('foo',), ('ValueError',))
