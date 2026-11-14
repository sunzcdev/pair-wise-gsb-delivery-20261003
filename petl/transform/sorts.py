from __future__ import absolute_import, print_function, division


import os
import heapq
from tempfile import NamedTemporaryFile
import itertools
import logging
from collections import namedtuple
import operator
from petl.compat import pickle, next, text_type


import petl.config as config
from petl.comparison import comparable_itemgetter
from petl.util.base import Table, asindices


logger = logging.getLogger(__name__)
warning = logger.warning
info = logger.info
debug = logger.debug


def sort(table, key=None, reverse=False, buffersize=None, tempdir=None,
         cache=True):
    """
    Sort the table. Field names or indices (from zero) can be used to specify
    the key. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['C', 2],
        ...           ['A', 9],
        ...           ['A', 6],
        ...           ['F', 1],
        ...           ['D', 10]]
        >>> table2 = etl.sort(table1, 'foo')
        >>> table2
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   9 |
        +-----+-----+
        | 'A' |   6 |
        +-----+-----+
        | 'C' |   2 |
        +-----+-----+
        | 'D' |  10 |
        +-----+-----+
        | 'F' |   1 |
        +-----+-----+

        >>> # sorting by compound key is supported
        ... table3 = etl.sort(table1, key=['foo', 'bar'])
        >>> table3
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   6 |
        +-----+-----+
        | 'A' |   9 |
        +-----+-----+
        | 'C' |   2 |
        +-----+-----+
        | 'D' |  10 |
        +-----+-----+
        | 'F' |   1 |
        +-----+-----+

        >>> # if no key is specified, the default is a lexical sort
        ... table4 = etl.sort(table1)
        >>> table4
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   6 |
        +-----+-----+
        | 'A' |   9 |
        +-----+-----+
        | 'C' |   2 |
        +-----+-----+
        | 'D' |  10 |
        +-----+-----+
        | 'F' |   1 |
        +-----+-----+

        >>> # sorting is stable, so fields can be sorted in different
        ... # directions by chaining sorts, starting with the last field
        ... table5 = etl.sort(table1, 'bar').sort('foo', reverse=True)
        >>> table5
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'F' |   1 |
        +-----+-----+
        | 'D' |  10 |
        +-----+-----+
        | 'C' |   2 |
        +-----+-----+
        | 'A' |   6 |
        +-----+-----+
        | 'A' |   9 |
        +-----+-----+

    The `buffersize` argument should be an `int` or `None`.

    If the number of rows in the table is less than `buffersize`, the table
    will be sorted in memory. Otherwise, the table is sorted in chunks of
    no more than `buffersize` rows, each chunk is written to a temporary file,
    and then a merge sort is performed on the temporary files.

    If `buffersize` is `None`, the value of
    `petl.config.sort_buffersize` will be used. By default this is
    set to 100000 rows, but can be changed, e.g.::

        >>> import petl.config
        >>> petl.config.sort_buffersize = 500000

    If `petl.config.sort_buffersize` is set to `None`, this forces
    all sorting to be done entirely in memory.

    By default the results of the sort will be cached, and so a second pass over
    the sorted table will yield rows from the cache and will not repeat the
    sort operation. To turn off caching, set the `cache` argument to `False`.

    Cache invalidation is generational. Calling :meth:`SortView.clearcache`
    drops the current generation: iterators already open keep reading their own
    snapshot, while iterators created afterwards re-read the source and build a
    fresh cache. Old temporary chunk files are deleted only once every iterator
    of the retired generation has been closed. A failed re-sort never publishes
    a partial cache, so any snapshot still in use remains intact and valid.
    These guarantees hold for interleaved consumption within a single thread;
    concurrent access from multiple threads is not supported.
    The same rules apply whether the snapshot is held in memory or in temporary
    files.

    """

    return SortView(table, key=key, reverse=reverse, buffersize=buffersize,
                    tempdir=tempdir, cache=cache)


Table.sort = sort


def _iterchunk(fn):
    # reopen so iterators from file cache are independent
    debug('iterchunk, opening %s' % fn)
    with open(fn, 'rb') as f:
        try:
            while True:
                yield pickle.load(f)
        except EOFError:
            pass
    debug('end of iterchunk, closed %s' % fn)


class _Keyed(namedtuple('Keyed', ['key', 'obj'])):
    #  Override default behavior of namedtuple comparisons, only keys need to be compared for heapmerge
    def __eq__(self, other):
        return self.key == other.key

    def __lt__(self, other):
        return self.key < other.key

    def __le__(self, other):
        return self.key <= other.key

    def __ne__(self, other):
        return self.key != other.key

    def __gt__(self, other):
        return self.key > other.key

    def __ge__(self, other):
        return self.key >= other.key


def _heapqmergesorted(key=None, *iterables):
    """Return a single iterator over the given iterables, sorted by the
    given `key` function, assuming the input iterables are already sorted by
    the same function. (I.e., the merge part of a general merge sort.) Uses
    :func:`heapq.merge` for the underlying implementation."""

    if key is None:
        keyed_iterables = iterables
        for element in heapq.merge(*keyed_iterables):
            yield element
    else:
        keyed_iterables = [(_Keyed(key(obj), obj) for obj in iterable)
                           for iterable in iterables]
        for element in heapq.merge(*keyed_iterables):
            yield element.obj


def _shortlistmergesorted(key=None, reverse=False, *iterables):
    """Return a single iterator over the given iterables, sorted by the
    given `key` function, assuming the input iterables are already sorted by
    the same function. (I.e., the merge part of a general merge sort.) Uses
    :func:`min` (or :func:`max` if ``reverse=True``) for the underlying
    implementation."""

    if reverse:
        op = max
    else:
        op = min
    if key is not None:
        opkwargs = {'key': key}
    else:
        opkwargs = dict()
    # populate initial shortlist
    # (remember some iterables might be empty)
    iterators = list()
    shortlist = list()
    for iterable in iterables:
        it = iter(iterable)
        try:
            first = next(it)
            iterators.append(it)
            shortlist.append(first)
        except StopIteration:
            pass
    # do the mergesort
    while iterators:
        nxt = op(shortlist, **opkwargs)
        yield nxt
        nextidx = shortlist.index(nxt)
        try:
            shortlist[nextidx] = next(iterators[nextidx])
        except StopIteration:
            del shortlist[nextidx]
            del iterators[nextidx]


def _mergesorted(key=None, reverse=False, *iterables):
    # N.B., I've used heapq for normal merge sort and shortlist merge sort for
    # reverse merge sort because I've assumed that heapq.merge is faster and
    # so is preferable but it doesn't support reverse sorting so the shortlist
    # merge sort has to be used for reverse sorting. Some casual profiling
    # suggests there isn't much between the two in terms of speed, but might be
    # worth profiling more carefully

    if reverse:
        return _shortlistmergesorted(key, True, *iterables)
    else:
        return _heapqmergesorted(key, *iterables)


class SortView(Table):
    def __init__(self, source, key=None, reverse=False, buffersize=None,
                 tempdir=None, cache=True):
        self.source = source
        self.key = key
        self.reverse = reverse
        if buffersize is None:
            self.buffersize = config.sort_buffersize
        else:
            self.buffersize = buffersize
        self.tempdir = tempdir
        self._cache = cache
        # The published cache generation, or None if there is none. Retired
        # generations stay alive until the last iterator reading them closes.
        self._generation = None

    def clearcache(self):
        # Drop the current generation only. Open iterators captured their own
        # reference to it and keep reading the old snapshot; later iterators
        # see no published generation and re-read the source.
        debug('clear cache')
        generation = self._generation
        self._generation = None
        if generation is not None:
            generation.retire()

    def __iter__(self):
        generation = self._generation
        if self._cache and generation is not None:
            # Snapshot the generation for the whole life of this iterator.
            return _SortCacheIterator(generation)
        return self._builditer()

    def _publish(self, generation):
        # Atomically install a fully built generation, retiring the previous
        # one. Only complete generations are ever published, so a failed sort
        # can never leave a half-built cache visible to later iterators.
        previous = self._generation
        self._generation = generation
        if previous is not None:
            previous.retire()

    def _builditer(self):
        debug('iterate without cache')
        source = self.source
        key = self.key
        reverse = self.reverse
        it = iter(source)

        try:
            hdr = next(it)
        except StopIteration:
            if key is None:
                return  # nothing to do on a table without headers
            hdr = []
        yield tuple(hdr)

        if key is not None:
            # convert field selection into field indices
            indices = asindices(hdr, key)
        else:
            indices = range(len(hdr))
        # now use field indices to construct a _getkey function
        getkey = comparable_itemgetter(*indices)

        # TODO support native comparison

        # initialise the first chunk
        rows = list(itertools.islice(it, 0, self.buffersize))
        rows.sort(key=getkey, reverse=reverse)

        # have we exhausted the source iterator?
        if self.buffersize is None or len(rows) < self.buffersize:
            # yes, table fits within sort buffer

            if self._cache:
                debug('caching mem')
                self._publish(_SortCacheGeneration(hdr, rows, None, getkey,
                                                    reverse))
            for row in rows:
                yield tuple(row)

        else:
            # no, table is too big, need to sort in chunks

            chunkfiles = []

            try:
                while rows:

                    # dump the chunk
                    with NamedTemporaryFile(dir=self.tempdir, delete=False,
                                            mode='wb') as f:
                        # N.B., we **don't** want the file to be deleted on
                        # close, but we **do** want the file to be deleted when
                        # the last reader of this generation is gone, or when
                        # the program exits.
                        wrapper = _NamedTempFileDeleteOnGC(f.name)
                        debug('created temporary chunk file %s' % f.name)
                        for row in rows:
                            pickle.dump(row, f, protocol=-1)
                        f.flush()
                        chunkfiles.append(wrapper)

                    # grab the next chunk
                    rows = list(itertools.islice(it, 0, self.buffersize))
                    rows.sort(key=getkey, reverse=reverse)
            except BaseException:
                # Never publish a half-built cache: discard the chunks written
                # so far and propagate the failure, leaving any older snapshot
                # untouched.
                for wrapper in chunkfiles:
                    wrapper.delete()
                raise

            if self._cache:
                debug('caching files')
                self._publish(_SortCacheGeneration(hdr, None, chunkfiles,
                                                    getkey, reverse))

            filenames = [f.name for f in chunkfiles]
            chunkiters = [_iterchunk(fn) for fn in filenames]
            try:
                for row in _mergesorted(getkey, reverse, *chunkiters):
                    yield tuple(row)
            finally:
                # Close open chunk files before their temporary files can be
                # deleted; deletion fails on Windows while a handle is open.
                for chunkiter in chunkiters:
                    chunkiter.close()


class _SortCacheGeneration(object):
    """One immutable, self-contained snapshot of a sort result.

    Holds either an in-memory row list or a list of temporary chunk files,
    together with the header and key function. Generations are never mutated
    once published; invalidation creates a brand new generation instead, so an
    iterator that captured an older generation keeps reading a valid snapshot.

    A reader count tracks live iterators. When a generation has been retired
    (via :meth:`retire`) its backing files are reclaimed as soon as the final
    reader closes; while readers remain, the snapshot is kept alive.

    """

    def __init__(self, header, memcache, filecache, getkey, reverse=False):
        self.header = header
        self.memcache = memcache
        self.filecache = filecache
        self.getkey = getkey
        self.reverse = reverse
        self._readers = 0
        self._retired = False

    def acquire(self):
        self._readers += 1
        return self

    def release(self):
        self._readers -= 1
        if self._retired and self._readers == 0:
            self._discard(suppress_errors=True)

    def retire(self):
        # Mark the generation as no longer reachable for new iterators. Its
        # data survives until every in-flight reader has finished.
        already_retired = self._retired
        self._retired = True
        if not already_retired and self._readers == 0:
            self._discard(suppress_errors=True)

    def _discard(self, suppress_errors=False):
        # Reclaim temporary chunk files. Memory snapshots simply drop out of
        # scope. Safe to call more than once.
        if self.filecache is None:
            return
        for wrapper in self.filecache:
            try:
                wrapper.delete()
            except Exception:
                if not suppress_errors:
                    raise
        self.filecache = None

    def __del__(self):
        # Backstop in case a reader is dropped without being closed.
        self._discard()


class _SortCacheIterator(object):
    """Iterator over a single, fixed cache generation.

    Registers as a reader for the generation's whole lifetime, including the
    period before the first row is requested, so retiring a generation can
    never reclaim files that an un-started iterator still needs.

    """

    def __init__(self, generation):
        self._generation = generation.acquire()
        self._closed = False
        self._rows = self._iterrows(generation)

    @staticmethod
    def _iterrows(generation):
        yield tuple(generation.header)
        if generation.memcache is not None:
            for row in generation.memcache:
                yield tuple(row)
        else:
            filenames = [f.name for f in generation.filecache]
            debug('iterate from file cache: %r', filenames)
            chunkiters = [_iterchunk(fn) for fn in filenames]
            try:
                rows = _mergesorted(generation.getkey, generation.reverse,
                                    *chunkiters)
                for row in rows:
                    yield tuple(row)
            finally:
                # Close handles before the generation may delete the files.
                for chunkiter in chunkiters:
                    chunkiter.close()

    def __iter__(self):
        return self

    def __next__(self):
        return next(self._rows)

    next = __next__

    def close(self):
        if self._closed:
            return
        self._closed = True
        try:
            self._rows.close()
        finally:
            self._generation.release()

    def __del__(self):
        self.close()


class _NamedTempFileDeleteOnGC(object):

    def __init__(self, name):
        self.name = name
        self._deleted = False

    def delete(self, unlink=os.unlink, log=logger.debug):
        if self._deleted:
            return
        name = self.name
        try:
            log('deleting %s' % name)
            unlink(name)
        except Exception as e:
            log('exception deleting %s: %s' % (name, e))
            raise
        else:
            self._deleted = True
            log('deleted %s' % name)

    def __del__(self):
        self.delete()

    def __str__(self):
        return self.name

    def __repr__(self):
        return self.name


def mergesort(*tables, **kwargs):
    """
    Combine multiple input tables into one sorted output table. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['A', 9],
        ...           ['C', 2],
        ...           ['D', 10],
        ...           ['A', 6],
        ...           ['F', 1]]
        >>> table2 = [['foo', 'bar'],
        ...           ['B', 3],
        ...           ['D', 10],
        ...           ['A', 10],
        ...           ['F', 4]]
        >>> table3 = etl.mergesort(table1, table2, key='foo')
        >>> table3.lookall()
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   9 |
        +-----+-----+
        | 'A' |   6 |
        +-----+-----+
        | 'A' |  10 |
        +-----+-----+
        | 'B' |   3 |
        +-----+-----+
        | 'C' |   2 |
        +-----+-----+
        | 'D' |  10 |
        +-----+-----+
        | 'D' |  10 |
        +-----+-----+
        | 'F' |   1 |
        +-----+-----+
        | 'F' |   4 |
        +-----+-----+

    If the input tables are already sorted by the given key, give
    ``presorted=True`` as a keyword argument.

    This function is equivalent to concatenating the input tables using
    :func:`cat` then sorting, however this function will typically be more
    efficient, especially if the input tables are presorted.

    Keyword arguments:

    key : string or tuple of strings, optional
        Field name or tuple of fields to sort by (defaults to `None` lexical
        sort)
    reverse : bool, optional
        `True` if sort in reverse (descending) order (defaults to `False`)
    presorted : bool, optional
        `True` if inputs are already sorted by the given key (defaults to
        `False`)
    missing : object
        Value to fill with when input tables have different fields (defaults to
        `None`)
    header : sequence of strings, optional
        Specify a fixed header for the output table
    buffersize : int, optional
        Limit the number of rows in memory per input table when inputs are not
        presorted

    """

    return MergeSortView(tables, **kwargs)


Table.mergesort = mergesort


class MergeSortView(Table):
    def __init__(self, tables, key=None, reverse=False, presorted=False,
                 missing=None, header=None, buffersize=None, tempdir=None,
                 cache=True):
        self.key = key
        if presorted:
            self.tables = tables
        else:
            self.tables = [sort(t, key=key, reverse=reverse,
                                buffersize=buffersize, tempdir=tempdir,
                                cache=cache)
                           for t in tables]
        self.missing = missing
        self._header = header
        self.reverse = reverse

    def __iter__(self):
        return itermergesort(self.tables, self.key, self._header, self.missing,
                             self.reverse)


def itermergesort(sources, key, header, missing, reverse):
    # first need to standardise headers of all input tables
    # borrow this from itercat - TODO remove code smells

    its = [iter(t) for t in sources]
    src_hdrs = []
    for it in its:
        try:
            src_hdrs.append(next(it))
        except StopIteration:
            src_hdrs.append([])

    if header is None:
        # determine output fields by gathering all fields found in the sources
        outhdr = list()
        for hdr in src_hdrs:
            for f in list(map(text_type, hdr)):
                if f not in outhdr:
                    # add any new fields as we find them
                    outhdr.append(f)
    else:
        # predetermined output fields
        outhdr = header
    yield tuple(outhdr)

    def _standardisedata(it, hdr, ofs):
        flds = list(map(text_type, hdr))
        # now construct and yield the data rows
        for _row in it:
            try:
                # should be quickest to do this way
                yield tuple(_row[flds.index(fo)] if fo in flds else missing
                            for fo in ofs)
            except IndexError:
                # handle short rows
                outrow = [missing] * len(ofs)
                for i, fi in enumerate(flds):
                    try:
                        outrow[ofs.index(fi)] = _row[i]
                    except IndexError:
                        pass  # be relaxed about short rows
                yield tuple(outrow)

    # wrap all iterators to standardise fields
    sits = [_standardisedata(it, hdr, outhdr)
            for hdr, it in zip(src_hdrs, its)]

    # now determine key function
    getkey = None
    if key is not None:
        # convert field selection into field indices
        indices = asindices(outhdr, key)
        # now use field indices to construct a _getkey function
        # N.B., this will probably raise an exception on short rows
        getkey = comparable_itemgetter(*indices)

    # OK, do the merge sort
    for row in _shortlistmergesorted(getkey, reverse, *sits):
        yield row


def issorted(table, key=None, reverse=False, strict=False):
    """
    Return True if the table is ordered (i.e., sorted) by the given key. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['a', 1, True],
        ...           ['b', 3, True],
        ...           ['b', 2]]
        >>> etl.issorted(table1, key='foo')
        True
        >>> etl.issorted(table1, key='bar')
        False
        >>> etl.issorted(table1, key='foo', strict=True)
        False
        >>> etl.issorted(table1, key='foo', reverse=True)
        False

    """

    # determine the operator to use when comparing rows
    if reverse and strict:
        op = operator.lt
    elif reverse and not strict:
        op = operator.le
    elif strict:
        op = operator.gt
    else:
        op = operator.ge

    it = iter(table)
    try:
        flds = [text_type(f) for f in next(it)]
    except StopIteration:
        flds = []
    if key is not None:
        getkey = comparable_itemgetter(*asindices(flds, key))
    try:
        prev = next(it)
    except StopIteration:
        return True  # no data rows
    if key is None:
        for curr in it:
            if not op(curr, prev):
                return False
            prev = curr
    else:
        prevkey = getkey(prev)
        for curr in it:
            currkey = getkey(curr)
            if not op(currkey, prevkey):
                return False
            prevkey = currkey
    return True


Table.issorted = issorted
