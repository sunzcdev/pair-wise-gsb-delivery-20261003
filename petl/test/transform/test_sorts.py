from __future__ import absolute_import, print_function, division


import os
import gc
import logging
import operator
import shutil
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
    debug('initially there should be no cache generation')
    eq_(None, result._generation)
    debug('pull rows through, should populate a file-backed generation')
    eq_(5, nrows(result))
    generation = result._generation
    eq_(3, len(generation.filecache))
    debug('check all files exist')
    filenames = _get_names(generation.filecache)
    for fn in filenames:
        assert os.path.exists(fn), fn
    debug('delete object and garbage collect')
    del result
    del generation
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
    generation = result._generation
    eq_(3, len(generation.filecache))
    debug('check all files exist')
    filenames = _get_names(generation.filecache)
    for fn in filenames:
        assert os.path.exists(fn), fn
    debug(filenames)
    debug('open an iterator')
    it = iter(result)
    next(it)
    next(it)
    debug('delete objects and garbage collect')
    del result
    del generation
    del it
    gc.collect()
    for fn in filenames:
        assert not os.path.exists(fn), fn


class MutableSource(object):
    """A re-readable source whose underlying rows can change between passes.

    Every call to ``iter()`` re-reads the current rows, which is what lets the
    cache invalidation tests observe the difference between a snapshot taken
    before and after a change to the source.

    """

    def __init__(self, header, rows):
        self.header = header
        self.rows = rows

    def __iter__(self):
        yield tuple(self.header)
        for row in self.rows:
            yield tuple(row)


def _sorted_rows(rows, key_index=0):
    return tuple(sorted(rows, key=operator.itemgetter(key_index)))


def test_sort_clearcache_memory_generation():
    # Memory-backed cache: an iterator already open keeps its snapshot while a
    # later iterator re-reads a changed source and builds a new generation.
    source = MutableSource(('foo',), [('C',), ('A',), ('B',)])
    view = sort(source, 'foo')

    old_expect = (('foo',), ('A',), ('B',), ('C',))
    ieq(old_expect, view)  # first pass builds the memory cache

    old_generation = view._generation
    assert old_generation is not None
    assert old_generation.memcache is not None

    old_it = iter(view)
    eq_(('foo',), next(old_it))
    eq_(('A',), next(old_it))

    # invalidate while the old iterator is mid-flight, then change the source
    view.clearcache()
    eq_(None, view._generation)
    source.rows = [('Z',), ('Y',)]

    # old iterator finishes its own snapshot, unaffected by the change
    eq_(('B',), next(old_it))
    eq_(('C',), next(old_it))

    # a brand new iterator re-reads the source and caches the new content
    new_expect = (('foo',), ('Y',), ('Z',))
    ieq(new_expect, view)
    new_generation = view._generation
    assert new_generation is not old_generation
    ieq(new_expect, view)  # further passes are served from the new cache


def test_sort_clearcache_file_generation():
    # Same generational contract as the memory case, but backed by temporary
    # chunk files because the data exceeds the buffer size.
    source = MutableSource(('foo',),
                           [('C',), ('A',), ('B',), ('D',), ('E',)])
    view = sort(source, 'foo', buffersize=2)

    old_rows = _sorted_rows(tuple(source.rows))
    old_expect = (('foo',),) + old_rows
    eq_(5, nrows(view))  # build the file-backed generation
    old_generation = view._generation
    assert old_generation.filecache is not None
    old_files = _get_names(old_generation.filecache)
    for fn in old_files:
        assert os.path.exists(fn), fn

    old_it = iter(view)
    eq_(('foo',), next(old_it))

    view.clearcache()
    eq_(None, view._generation)
    # old reader still open: its snapshot files must survive
    for fn in old_files:
        assert os.path.exists(fn), fn

    source.rows = [('Z',), ('Y',), ('X',)]

    # old iterator keeps reading the retired snapshot to completion
    remaining = [row for row in old_it]
    eq_(list(old_rows), remaining)

    # only now, with the last old reader closed, are the old files reclaimed
    del old_it
    gc.collect()
    for fn in old_files:
        assert not os.path.exists(fn), fn

    # new iterator re-reads the source and builds a fresh file generation
    new_rows = _sorted_rows(tuple(source.rows))
    new_expect = (('foo',),) + new_rows
    ieq(new_expect, view)
    new_generation = view._generation
    assert new_generation is not old_generation
    new_files = _get_names(new_generation.filecache)
    assert set(new_files).isdisjoint(old_files)
    for fn in new_files:
        assert os.path.exists(fn), fn


def test_sort_clearcache_reclaim_after_last_reader():
    # Files of a retired generation are deleted when the last reader closes,
    # even if a newer generation has been published in the meantime.
    source = MutableSource(('foo',),
                           [('C',), ('A',), ('B',), ('D',), ('E',)])
    view = sort(source, 'foo', buffersize=2)
    eq_(5, nrows(view))
    old_files = _get_names(view._generation.filecache)

    old_it = iter(view)
    next(old_it)
    view.clearcache()

    # a fresh pass publishes a new generation while the old reader is alive
    source.rows = [('Z',), ('Y',), ('X',), ('W',), ('V',)]
    eq_(5, nrows(view))
    new_files = _get_names(view._generation.filecache)

    for fn in old_files:
        assert os.path.exists(fn), fn  # old reader still holds the snapshot
    for fn in new_files:
        assert os.path.exists(fn), fn

    del old_it
    gc.collect()
    for fn in old_files:
        assert not os.path.exists(fn), fn
    for fn in new_files:
        assert os.path.exists(fn), fn  # current generation stays cached


def test_sort_unstarted_iterator_survives_clearcache():
    # An iterator created but never advanced still counts as a reader, so
    # retiring the generation cannot reclaim files underneath it.
    source = MutableSource(('foo',),
                           [('C',), ('A',), ('B',), ('D',), ('E',)])
    view = sort(source, 'foo', buffersize=2)
    eq_(5, nrows(view))
    old_files = _get_names(view._generation.filecache)

    unstarted = iter(view)
    view.clearcache()
    gc.collect()
    for fn in old_files:
        assert os.path.exists(fn), fn

    old_expect = (('foo',),) + _sorted_rows(tuple(source.rows))
    ieq(old_expect, unstarted)
    del unstarted
    gc.collect()
    for fn in old_files:
        assert not os.path.exists(fn), fn


class _FailingSource(MutableSource):
    """Raises partway through the data rows, after the given number yielded."""

    def __init__(self, header, rows, fail_after):
        super(_FailingSource, self).__init__(header, rows)
        self.fail_after = fail_after

    def __iter__(self):
        yield tuple(self.header)
        for index, row in enumerate(self.rows):
            if index == self.fail_after:
                raise RuntimeError('boom')
            yield tuple(row)


def test_sort_failed_resort_preserves_old_snapshot():
    # A failed new pass must not destroy the old snapshot or publish a
    # half-built cache, for both memory and file-backed generations.
    for buffersize in (None, 2):
        source = MutableSource(('foo',), [('C',), ('A',), ('B',)])
        view = sort(source, 'foo', buffersize=buffersize)
        old_expect = (('foo',), ('A',), ('B',), ('C',))
        ieq(old_expect, view)
        old_generation = view._generation

        view.clearcache()
        failing = _FailingSource(('foo',), [('Z',), ('Y',), ('X',)],
                                 fail_after=1)
        view.source = failing

        with pytest.raises(RuntimeError):
            list(view)

        # no partial cache was published
        eq_(None, view._generation)

        # restore a good source: the next pass re-reads and sorts cleanly
        view.source = MutableSource(('foo',), [('Z',), ('Y',), ('X',)])
        new_expect = (('foo',), ('X',), ('Y',), ('Z',))
        ieq(new_expect, view)
        assert view._generation is not old_generation


def test_sort_failed_resort_keeps_old_file_snapshot():
    # While an old file-backed snapshot is still being read, a failed new
    # round must neither delete its chunk files early nor leave a partial
    # cache behind.
    source = MutableSource(('foo',),
                           [('C',), ('A',), ('B',), ('D',), ('E',)])
    view = sort(source, 'foo', buffersize=2)
    eq_(5, nrows(view))
    old_files = _get_names(view._generation.filecache)

    old_it = iter(view)
    next(old_it)
    view.clearcache()  # retire the old generation; reader stays open

    view.source = _FailingSource(
        ('foo',), [('Z',), ('Y',), ('X',), ('W',), ('V',), ('U',)],
        fail_after=4,
    )
    with pytest.raises(RuntimeError):
        list(view)

    # no partial cache was published, and the old snapshot survives intact
    eq_(None, view._generation)
    for fn in old_files:
        assert os.path.exists(fn), fn

    # the header was already consumed from this iterator, only data remains
    eq_(list(_sorted_rows(tuple(source.rows))), list(old_it))
    del old_it
    gc.collect()
    for fn in old_files:
        assert not os.path.exists(fn), fn


def test_sort_failed_build_cleans_temp_files():
    # When the file-backed build itself fails, every partial chunk file is
    # removed and nothing is left behind to be mistaken for a valid cache.
    tempdir = 'tmp_sort_fail'
    if not os.path.exists(tempdir):
        os.mkdir(tempdir)
    try:
        source = _FailingSource(
            ('foo',),
            [('C',), ('A',), ('B',), ('D',), ('E',), ('F',), ('G',)],
            fail_after=5,
        )
        view = sort(source, 'foo', buffersize=2, tempdir=tempdir)

        with pytest.raises(RuntimeError):
            list(view)

        eq_(None, view._generation)
        gc.collect()
        # every partial chunk created before the failure was removed
        eq_([], os.listdir(tempdir))
    finally:
        shutil.rmtree(tempdir, ignore_errors=True)


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
