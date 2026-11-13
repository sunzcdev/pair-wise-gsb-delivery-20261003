from __future__ import absolute_import, print_function, division


import os
import gc
import logging
from datetime import datetime
import platform

import pytest

from petl.compat import next

from petl.errors import FieldSelectionError
from petl.test.helpers import ieq, eq_
from petl.util import nrows
from petl.transform.basics import cat
from petl.transform.sorts import sort, mergesort, issorted


logger = logging.getLogger(__name__)
debug = logger.debug


def test_sort_1():

    table = (('foo', 'bar'),
             ('C', '2'),
             ('A', '9'),
             ('A', '6'),
             ('F', '1'),
             ('D', '10'))

    result = sort(table, 'foo')
    expectation = (('foo', 'bar'),
                   ('A', '9'),
                   ('A', '6'),
                   ('C', '2'),
                   ('D', '10'),
                   ('F', '1'))
    ieq(expectation, result)


def test_sort_2():

    table = (('foo', 'bar'),
             ('C', '2'),
             ('A', '9'),
             ('A', '6'),
             ('F', '1'),
             ('D', '10'))

    result = sort(table, key=('foo', 'bar'))
    expectation = (('foo', 'bar'),
                   ('A', '6'),
                   ('A', '9'),
                   ('C', '2'),
                   ('D', '10'),
                   ('F', '1'))
    ieq(expectation, result)

    result = sort(table)  # default is lexical sort
    expectation = (('foo', 'bar'),
                   ('A', '6'),
                   ('A', '9'),
                   ('C', '2'),
                   ('D', '10'),
                   ('F', '1'))
    ieq(expectation, result)


def test_sort_3():

    table = (('foo', 'bar'),
             ('C', '2'),
             ('A', '9'),
             ('A', '6'),
             ('F', '1'),
             ('D', '10'))

    result = sort(table, 'bar')
    expectation = (('foo', 'bar'),
                   ('F', '1'),
                   ('D', '10'),
                   ('C', '2'),
                   ('A', '6'),
                   ('A', '9'))
    ieq(expectation, result)


def test_sort_4():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10))

    result = sort(table, 'bar')
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('C', 2),
                   ('A', 6),
                   ('A', 9),
                   ('D', 10))
    ieq(expectation, result)


def test_sort_5():

    table = (('foo', 'bar'),
             (2.3, 2),
             (1.2, 9),
             (2.3, 6),
             (3.2, 1),
             (1.2, 10))

    expectation = (('foo', 'bar'),
                   (1.2, 9),
                   (1.2, 10),
                   (2.3, 2),
                   (2.3, 6),
                   (3.2, 1))

    # can use either field names or indices (from 1) to specify sort key
    result = sort(table, key=('foo', 'bar'))
    ieq(expectation, result)
    result = sort(table, key=(0, 1))
    ieq(expectation, result)
    result = sort(table, key=('foo', 1))
    ieq(expectation, result)
    result = sort(table, key=(0, 'bar'))
    ieq(expectation, result)


def test_sort_6():

    table = (('foo', 'bar'),
             (2.3, 2),
             (1.2, 9),
             (2.3, 6),
             (3.2, 1),
             (1.2, 10))

    expectation = (('foo', 'bar'),
                   (3.2, 1),
                   (2.3, 6),
                   (2.3, 2),
                   (1.2, 10),
                   (1.2, 9))

    result = sort(table, key=('foo', 'bar'), reverse=True)
    ieq(expectation, result)


def test_sort_chained_mixed_directions():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10),
             ('A', 2))

    # foo descending, then bar ascending
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('D', 10),
                   ('C', 2),
                   ('A', 2),
                   ('A', 6),
                   ('A', 9))

    result = sort(sort(table, 'bar'), 'foo', reverse=True)
    ieq(expectation, result)
    result = sort(sort(table, 'bar', buffersize=2), 'foo', reverse=True,
                  buffersize=2)
    ieq(expectation, result)

    # foo ascending, then bar descending
    expectation = (('foo', 'bar'),
                   ('A', 9),
                   ('A', 6),
                   ('A', 2),
                   ('C', 2),
                   ('D', 10),
                   ('F', 1))

    result = sort(sort(table, 'bar', reverse=True), 'foo')
    ieq(expectation, result)
    result = sort(sort(table, 'bar', reverse=True, buffersize=2), 'foo',
                  buffersize=2)
    ieq(expectation, result)


def test_sort_buffered():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10))

    # test sort forwards
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('C', 2),
                   ('A', 6),
                   ('A', 9),
                   ('D', 10))
    result = sort(table, 'bar')
    ieq(expectation, result)
    result = sort(table, 'bar', buffersize=2)
    ieq(expectation, result)

    # sort in reverse
    expectation = (('foo', 'bar'),
                   ('D', 10),
                   ('A', 9),
                   ('A', 6),
                   ('C', 2),
                   ('F', 1))

    result = sort(table, 'bar', reverse=True)
    ieq(expectation, result)
    result = sort(table, 'bar', reverse=True, buffersize=2)
    ieq(expectation, result)

    # no key
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('D', 10),
                   ('C', 2),
                   ('A', 9),
                   ('A', 6))
    result = sort(table, reverse=True)
    ieq(expectation, result)
    result = sort(table, reverse=True, buffersize=2)
    ieq(expectation, result)


def test_sort_buffered_tempdir():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10))

    # test sort forwards
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('C', 2),
                   ('A', 6),
                   ('A', 9),
                   ('D', 10))
    result = sort(table, 'bar')
    ieq(expectation, result)
    tempdir = 'tmp'
    if not os.path.exists(tempdir):
        os.mkdir(tempdir)
    result = sort(table, 'bar', buffersize=2, tempdir=tempdir)
    ieq(expectation, result)


def test_sort_buffered_independent():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10))
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('C', 2),
                   ('A', 6),
                   ('A', 9),
                   ('D', 10))

    result = sort(table, 'bar', buffersize=4)
    nrows(result)  # cause data to be cached
    # check that two row iterators are independent, i.e., consuming rows
    # from one does not affect the other
    it1 = iter(result)
    it2 = iter(result)
    eq_(expectation[0], next(it1))
    eq_(expectation[1], next(it1))
    eq_(expectation[0], next(it2))
    eq_(expectation[1], next(it2))
    eq_(expectation[2], next(it2))
    eq_(expectation[2], next(it1))


def _get_names(l):
    return [x.name for x in l]


def test_sort_buffered_cleanup():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10))
    result = sort(table, 'bar', buffersize=2)
    debug('initially filecache should be empty')
    eq_(None, result._filecache)
    debug('pull rows through, should populate file cache')
    eq_(5, nrows(result))
    eq_(3, len(result._filecache))
    debug('check all files exist')
    filenames = _get_names(result._filecache)
    for fn in filenames:
        assert os.path.exists(fn), fn
    debug('delete object and garbage collect')
    del result
    gc.collect()
    debug('check all files have been deleted')
    for fn in filenames:
        assert not os.path.exists(fn), fn


@pytest.mark.skipif(platform.python_implementation() == 'PyPy', reason='SKIP sort cleanup test (PyPy)')
def test_sort_buffered_cleanup_open_iterator():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10))
    # check if cleanup is robust against open iterators
    result = sort(table, 'bar', buffersize=2)
    debug('pull rows through, should populate file cache')
    eq_(5, nrows(result))
    eq_(3, len(result._filecache))
    debug('check all files exist')
    filenames = _get_names(result._filecache)
    for fn in filenames:
        assert os.path.exists(fn), fn
    debug(filenames)
    debug('open an iterator')
    it = iter(result)
    next(it)
    next(it)
    debug('delete objects and garbage collect')
    del result
    del it
    gc.collect()
    for fn in filenames:
        assert not os.path.exists(fn), fn


def test_sort_empty():
    table = (('foo', 'bar'),)
    expect = (('foo', 'bar'),)
    actual = sort(table)
    ieq(expect, actual)


def test_sort_none():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', None),
             ('F', 1),
             ('D', 10))

    result = sort(table, 'bar')
    print(list(result))
    expectation = (('foo', 'bar'),
                   ('A', None),
                   ('F', 1),
                   ('C', 2),
                   ('A', 9),
                   ('D', 10))
    ieq(expectation, result)

    dt = datetime.now().replace

    table = (('foo', 'bar'),
             ('C', dt(hour=5)),
             ('A', dt(hour=1)),
             ('A', None),
             ('F', dt(hour=9)),
             ('D', dt(hour=17)))

    result = sort(table, 'bar')
    expectation = (('foo', 'bar'),
                   ('A', None),
                   ('A', dt(hour=1)),
                   ('C', dt(hour=5)),
                   ('F', dt(hour=9)),
                   ('D', dt(hour=17)))
    ieq(expectation, result)


def test_sort_headerless_no_keys():
    """
    Sorting a headerless table without specifying cols should be a no-op.
    """
    table = []
    result = sort(table)
    expectation = []
    ieq(expectation, result)


def test_sort_headerless_explicit():
    """
    But if you specify keys, they must exist.
    """
    table = []
    with pytest.raises(FieldSelectionError):
        for i in sort(table, 'foo'):
            pass

# TODO test sort with native comparison


def test_mergesort_1():

    table1 = (('foo', 'bar'),
              ('A', 6),
              ('C', 2),
              ('D', 10),
              ('A', 9),
              ('F', 1))

    table2 = (('foo', 'bar'),
              ('B', 3),
              ('D', 10),
              ('A', 10),
              ('F', 4))

    # should be same as concatenate then sort (but more efficient, esp. when
    # presorted)
    expect = sort(cat(table1, table2))

    actual = mergesort(table1, table2)
    ieq(expect, actual)
    ieq(expect, actual)

    actual = mergesort(sort(table1), sort(table2), presorted=True)
    ieq(expect, actual)
    ieq(expect, actual)


def test_mergesort_2():

    table1 = (('foo', 'bar'),
              ('A', 9),
              ('C', 2),
              ('D', 10),
              ('A', 6),
              ('F', 1))

    table2 = (('foo', 'baz'),
              ('B', 3),
              ('D', 10),
              ('A', 10),
              ('F', 4))

    # should be same as concatenate then sort (but more efficient, esp. when
    # presorted)
    expect = sort(cat(table1, table2), key='foo')

    actual = mergesort(table1, table2, key='foo')
    ieq(expect, actual)
    ieq(expect, actual)

    actual = mergesort(sort(table1, key='foo'), sort(table2, key='foo'),
                       key='foo', presorted=True)
    ieq(expect, actual)
    ieq(expect, actual)


def test_mergesort_3():

    table1 = (('foo', 'bar'),
              ('A', 9),
              ('C', 2),
              ('D', 10),
              ('A', 6),
              ('F', 1))

    table2 = (('foo', 'baz'),
              ('B', 3),
              ('D', 10),
              ('A', 10),
              ('F', 4))

    # should be same as concatenate then sort (but more efficient, esp. when
    # presorted)
    expect = sort(cat(table1, table2), key='foo', reverse=True)

    actual = mergesort(table1, table2, key='foo', reverse=True)
    ieq(expect, actual)
    ieq(expect, actual)

    actual = mergesort(sort(table1, key='foo', reverse=True),
                       sort(table2, key='foo', reverse=True),
                       key='foo', reverse=True, presorted=True)
    ieq(expect, actual)
    ieq(expect, actual)


def test_mergesort_4():

    table1 = (('foo', 'bar', 'baz'),
              (1, 'A', True),
              (2, 'B', None),
              (4, 'C', True))
    table2 = (('bar', 'baz', 'quux'),
              ('A', True, 42.0),
              ('B', False, 79.3),
              ('C', False, 12.4))

    expect = sort(cat(table1, table2), key='bar')

    actual = mergesort(table1, table2, key='bar')
    ieq(expect, actual)
    ieq(expect, actual)


def test_mergesort_empty():

    table1 = (('foo', 'bar'),
              ('A', 9),
              ('C', 2),
              ('D', 10),
              ('F', 1))

    table2 = (('foo', 'bar'),)

    expect = table1
    actual = mergesort(table1, table2, key='foo')
    ieq(expect, actual)
    ieq(expect, actual)


def test_issorted():

    table1 = (('foo', 'bar', 'baz'),
              ('a', 1, True),
              ('b', 3, True),
              ('b', 2))
    assert issorted(table1, key='foo')
    assert not issorted(table1, key='foo', reverse=True)
    assert not issorted(table1, key='foo', strict=True)

    table2 = (('foo', 'bar', 'baz'),
              ('b', 2, True),
              ('a', 1, True),
              ('b', 3))
    assert not issorted(table2, key='foo')

    table3 = (('foo', 'bar', 'baz'),
              ('a', 1, True),
              ('b', 2, True),
              ('b', 3))
    assert issorted(table3, key=('foo', 'bar'))
    assert issorted(table3)

    table4 = (('foo', 'bar', 'baz'),
              ('a', 1, True),
              ('b', 3, True),
              ('b', 2))
    assert not issorted(table4, key=('foo', 'bar'))
    assert not issorted(table4)

    table5 = (('foo', 'bar', 'baz'),
              ('b', 3, True),
              ('b', 2),
              ('a', 1, True))
    assert not issorted(table5, key='foo')
    assert issorted(table5, key='foo', reverse=True)
    assert not issorted(table5, key='foo', reverse=True, strict=True)


def test_issorted_empty():

    table = (('foo', 'bar'),)
    assert issorted(table)
    assert issorted(table, key='foo')
    assert issorted(table, key='foo', reverse=True, strict=True)
    assert issorted([])


def test_sort_missing_cell_numeric():
    """ Sorting table with missing values raises IndexError #385 """
    tbl = (('a', 'b'), ('4',), ('2', '1'), ('1',))
    expect = (('a', 'b'), ('1',), ('2', '1'), ('4',))

    tbl_sorted = sort(tbl)
    ieq(expect, tbl_sorted)


def test_sort_missing_cell_text():
    """ Sorting table with missing values raises IndexError #385 """
    tbl = (('a', 'b', 'c'), ('C',), ('A', '4', '5'))
    expect = (('a', 'b', 'c'), ('A', '4', '5'), ('C',))

    tbl_sorted = sort(tbl)
    ieq(expect, tbl_sorted)


def test_sort_maxfanin_invalid():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9))
    # a fan-in of less than 2 cannot reduce the number of chunk files
    with pytest.raises(ValueError):
        sort(table, 'bar', buffersize=1, maxfanin=1)
    with pytest.raises(ValueError):
        sort(table, 'bar', buffersize=1, maxfanin=0)


def test_sort_maxfanin():

    table = (('foo', 'bar'),
             ('C', 2),
             ('A', 9),
             ('A', 6),
             ('F', 1),
             ('D', 10),
             ('B', 4),
             ('E', 8),
             ('A', 3),
             ('G', 5))

    # sort forwards
    expectation = (('foo', 'bar'),
                   ('F', 1),
                   ('C', 2),
                   ('A', 3),
                   ('B', 4),
                   ('G', 5),
                   ('A', 6),
                   ('E', 8),
                   ('A', 9),
                   ('D', 10))
    # tiny buffersize and fan-in force multiple merge rounds
    result = sort(table, 'bar', buffersize=2, maxfanin=2)
    ieq(expectation, result)
    debug('second pass should come from the file cache')
    ieq(expectation, result)
    result = sort(table, 'bar', buffersize=2, maxfanin=2, cache=False)
    ieq(expectation, result)
    ieq(expectation, result)

    # sort in reverse
    expectation = (('foo', 'bar'),
                   ('D', 10),
                   ('A', 9),
                   ('E', 8),
                   ('A', 6),
                   ('G', 5),
                   ('B', 4),
                   ('A', 3),
                   ('C', 2),
                   ('F', 1))
    result = sort(table, 'bar', reverse=True, buffersize=2, maxfanin=2)
    ieq(expectation, result)
    ieq(expectation, result)
    result = sort(table, 'bar', reverse=True, buffersize=2, maxfanin=2,
                  cache=False)
    ieq(expectation, result)


def test_sort_maxfanin_stable():

    # rows with equal keys must keep their input order, and reverse must
    # only change the direction of the key comparison
    rows = [(i % 4, i) for i in range(25)]
    table = [('key', 'seq')] + rows
    expect_fwd = [tuple(table[0])] + \
        [tuple(r) for r in sorted(rows, key=lambda r: r[0])]
    expect_rev = [tuple(table[0])] + \
        [tuple(r) for r in sorted(rows, key=lambda r: r[0], reverse=True)]

    for reverse, expect in ((False, expect_fwd), (True, expect_rev)):
        debug('in-memory sort')
        eq_(expect, list(sort(table, 'key', reverse=reverse)))
        debug('buffered sort with multi-round merging')
        result = sort(table, 'key', reverse=reverse, buffersize=2, maxfanin=2)
        eq_(expect, list(result))
        debug('second pass from the file cache')
        eq_(expect, list(result))
        debug('buffered sort without cache')
        result = sort(table, 'key', reverse=reverse, buffersize=2, maxfanin=2,
                      cache=False)
        eq_(expect, list(result))


class _CountingOpen(object):
    # wrap the builtin open to track the number of simultaneously open files

    def __init__(self, wrapped):
        self.wrapped = wrapped
        self.current = 0
        self.peak = 0

    def __call__(self, *args, **kwargs):
        f = self.wrapped(*args, **kwargs)
        self.current += 1
        self.peak = max(self.peak, self.current)
        return _CountedFile(f, self)


class _CountedFile(object):

    def __init__(self, f, counter):
        self._f = f
        self._counter = counter

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()
        return False

    def close(self):
        f = self._f
        self._f = None
        if f is not None:
            f.close()
            self._counter.current -= 1

    def __getattr__(self, name):
        return getattr(self._f, name)


def test_sort_maxfanin_limits_open_files(monkeypatch):

    import petl.transform.sorts as sorts
    counter = _CountingOpen(open)
    monkeypatch.setattr(sorts, 'open', counter, raising=False)

    # count temporary files created, to prove multiple merge rounds happen
    created = []
    real_namedtemporaryfile = sorts.NamedTemporaryFile

    def counting_namedtemporaryfile(*args, **kwargs):
        f = real_namedtemporaryfile(*args, **kwargs)
        created.append(f.name)
        return f

    monkeypatch.setattr(sorts, 'NamedTemporaryFile',
                        counting_namedtemporaryfile)

    table = [('foo', 'bar')] + [(i % 5, i) for i in range(21)]
    expect = list(sort(table, 'bar'))
    result = sort(table, 'bar', buffersize=2, maxfanin=3, cache=False)
    eq_(expect, list(result))

    # 21 rows in chunks of 2 make 11 chunk files, more than the fan-in
    nchunks = 11
    debug('temporary files created: %d', len(created))
    assert len(created) > nchunks, 'expected intermediate merge files'
    debug('peak open read handles: %d', counter.peak)
    assert 1 < counter.peak <= 3
    eq_(0, counter.current)


class _FailingPickle(object):
    # shim for the pickle module that fails after a number of dump calls

    def __init__(self, real, fail_after):
        self._real = real
        self._fail_after = fail_after
        self.dumps = 0

    def dump(self, row, f, protocol=None):
        self.dumps += 1
        if self.dumps > self._fail_after:
            raise IOError('simulated merge write failure')
        return self._real.dump(row, f, protocol=protocol)

    def load(self, f):
        return self._real.load(f)


def _maketempdir():
    # N.B., do not use tempfile.mkdtemp() here, the directories it creates
    # are not writable in some sandboxed environments
    import uuid
    name = 'tmp_sort_maxfanin_' + uuid.uuid4().hex
    os.mkdir(name)
    return name


def test_sort_maxfanin_merge_write_failure(monkeypatch):

    import petl.transform.sorts as sorts

    table = [('foo', 'bar')] + [(i % 5, i) for i in range(9)]
    tempdir = _maketempdir()

    # 9 rows in chunks of 2 take 9 dump calls, so the first write of an
    # intermediate merge file will fail
    monkeypatch.setattr(sorts, 'pickle', _FailingPickle(sorts.pickle, 9))

    result = sort(table, 'bar', buffersize=2, maxfanin=2, tempdir=tempdir,
                  cache=False)
    try:
        nrows(result)
        raise AssertionError('expected the merge write failure')
    except IOError:
        pass
    del result
    gc.collect()
    debug('tempdir contents after failure: %r', os.listdir(tempdir))
    eq_([], os.listdir(tempdir))
    os.rmdir(tempdir)


@pytest.mark.skipif(platform.python_implementation() == 'PyPy', reason='SKIP sort cleanup test (PyPy)')
def test_sort_maxfanin_early_close():

    table = [('foo', 'bar')] + [(i % 5, i) for i in range(20)]
    tempdir = _maketempdir()

    # without cache, closing the iterator after a few rows must release all
    # handles and remove all temporary files
    result = sort(table, 'bar', buffersize=2, maxfanin=2, tempdir=tempdir,
                  cache=False)
    it = iter(result)
    next(it)
    next(it)
    next(it)
    it.close()
    del it
    del result
    gc.collect()
    eq_([], os.listdir(tempdir))
    os.rmdir(tempdir)

    # with cache, the cached chunk files survive until the view is released,
    # but intermediate merge files are removed when the iterator is closed
    tempdir = _maketempdir()
    result = sort(table, 'bar', buffersize=2, maxfanin=2, tempdir=tempdir)
    eq_(20, nrows(result))
    filenames = _get_names(result._filecache)
    it = iter(result)
    next(it)
    next(it)
    it.close()
    del it
    gc.collect()
    debug('tempdir contents after close: %r', os.listdir(tempdir))
    eq_(len(filenames), len(os.listdir(tempdir)))
    for fn in filenames:
        assert os.path.exists(fn), fn
    del result
    gc.collect()
    for fn in filenames:
        assert not os.path.exists(fn), fn
    os.rmdir(tempdir)
