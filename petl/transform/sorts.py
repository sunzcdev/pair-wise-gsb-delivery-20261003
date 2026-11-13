from __future__ import absolute_import, print_function, division


import os
import heapq
from tempfile import NamedTemporaryFile
import itertools
import logging
from collections import namedtuple
import operator
from petl.compat import pickle, next, string_types, text_type


import petl.config as config
from petl.comparison import Comparable, comparable_itemgetter
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
        self._hdrcache = None
        self._memcache = None
        self._filecache = None
        self._getkey = None

    def clearcache(self):
        debug('clear cache')
        self._hdrcache = None
        self._memcache = None
        self._filecache = None
        self._getkey = None

    def __iter__(self):
        source = self.source
        key = self.key
        reverse = self.reverse
        if self._cache and self._memcache is not None:
            return self._iterfrommemcache()
        elif self._cache and self._filecache is not None:
            return self._iterfromfilecache()
        else:
            return self._iternocache(source, key, reverse)

    def _iterfrommemcache(self):
        debug('iterate from memory cache')
        yield tuple(self._hdrcache)
        for row in self._memcache:
            yield tuple(row)

    def _iterfromfilecache(self):
        # create a reference to the filecache here, so cleanup happens in the
        # correct order
        filecache = self._filecache
        filenames = list(map(operator.attrgetter('name'), filecache))
        debug('iterate from file cache: %r', filenames)
        yield tuple(self._hdrcache)
        chunkiters = [_iterchunk(fn) for fn in filenames]
        rows = _mergesorted(self._getkey, self.reverse, *chunkiters)
        try:
            for row in rows:
                yield tuple(row)
        finally:
            debug('attempt cleanup from generator')
            # N.B., need to ensure that any open files are closed **before**
            # temporary files are deleted, as deletion will fail on Windows
            # if file is in use (i.e., still open)
            del chunkiters
            del rows
            del filecache
            debug('exiting generator')

    def _iternocache(self, source, key, reverse):
        debug('iterate without cache')
        self.clearcache()
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
                self._hdrcache = hdr
                self._memcache = rows
                # actually not needed to iterate from memcache
                self._getkey = getkey

            for row in rows:
                yield tuple(row)

        else:
            # no, table is too big, need to sort in chunks

            chunkfiles = []

            while rows:

                # dump the chunk
                with NamedTemporaryFile(dir=self.tempdir, delete=False,
                                        mode='wb') as f:
                    # N.B., we **don't** want the file to be deleted on close,
                    # but we **do** want the file to be deleted when self
                    # is garbage collected, or when the program exits. When
                    # all references to the wrapper are gone, the file should
                    # get deleted.
                    wrapper = _NamedTempFileDeleteOnGC(f.name)
                    debug('created temporary chunk file %s' % f.name)
                    for row in rows:
                        pickle.dump(row, f, protocol=-1)
                    f.flush()
                    chunkfiles.append(wrapper)

                # grab the next chunk
                rows = list(itertools.islice(it, 0, self.buffersize))
                rows.sort(key=getkey, reverse=reverse)

            if self._cache:
                debug('caching files')
                self._hdrcache = hdr
                self._filecache = chunkfiles
                self._getkey = getkey

            chunkiters = [_iterchunk(f.name) for f in chunkfiles]
            for row in _mergesorted(getkey, reverse, *chunkiters):
                yield tuple(row)


class _NamedTempFileDeleteOnGC(object):

    def __init__(self, name):
        self.name = name

    def delete(self, unlink=os.unlink, log=logger.debug):
        name = self.name
        try:
            log('deleting %s' % name)
            unlink(name)
        except Exception as e:
            log('exception deleting %s: %s' % (name, e))
            raise
        else:
            log('deleted %s' % name)

    def __del__(self):
        self.delete()

    def __str__(self):
        return self.name

    def __repr__(self):
        return self.name


def _check_topk(k):
    try:
        k = operator.index(k)
    except TypeError:
        raise TypeError('k must be an integer')
    if k < 0:
        raise ValueError('k must be non-negative')
    return k


_NO_ALIAS = object()


def _take_alias(kwargs, *names):
    found = [name for name in names if name in kwargs]
    if len(found) > 1:
        raise TypeError('multiple aliases supplied: %s' % ', '.join(found))
    if found:
        return kwargs.pop(found[0])
    return _NO_ALIAS


def _normalise_direction(direction):
    if isinstance(direction, bool):
        return direction
    if isinstance(direction, string_types):
        value = direction.strip().lower()
        if value in ('asc', 'ascending', 'up'):
            return False
        if value in ('desc', 'descending', 'down'):
            return True
    if direction in (1, -1):
        return direction == -1
    raise ValueError('direction must be a bool, 1, -1 or one of '
                     '"asc"/"desc"')


def _is_direction(value):
    if isinstance(value, bool):
        return True
    if isinstance(value, string_types):
        try:
            _normalise_direction(value)
        except ValueError:
            return False
        return True
    return False


def _parse_rank_spec(spec):
    if isinstance(spec, dict):
        fields = list(spec.keys())
        directions = [_normalise_direction(spec[field]) for field in fields]
        return fields, directions

    if isinstance(spec, (list, tuple)):
        items = list(spec)
    else:
        items = [spec]

    if not items:
        return [], []

    if len(items) == 2 and _is_direction(items[1]) \
            and not _is_direction(items[0]):
        return [items[0]], [_normalise_direction(items[1])]

    if all(isinstance(item, (list, tuple)) and len(item) == 2
           and _is_direction(item[1]) for item in items):
        fields = [item[0] for item in items]
        directions = [_normalise_direction(item[1]) for item in items]
        return fields, directions

    return items, None


def _normalise_reverse(reverse, fields):
    if reverse is None:
        reverse = False
    if isinstance(reverse, dict):
        directions = []
        for field in fields:
            if field in reverse:
                directions.append(_normalise_direction(reverse[field]))
            elif text_type(field) in reverse:
                directions.append(
                    _normalise_direction(reverse[text_type(field)])
                )
            else:
                directions.append(False)
        return directions

    if isinstance(reverse, (list, tuple)):
        if len(reverse) != len(fields):
            raise ValueError('one direction is required per ranking field')
        return [_normalise_direction(direction) for direction in reverse]

    direction = _normalise_direction(reverse)
    return [direction] * len(fields)


def _normalise_none_position(none_position):
    if none_position is None:
        return 'last'
    if isinstance(none_position, bool):
        return 'first' if none_position else 'last'
    if isinstance(none_position, string_types):
        value = none_position.strip().lower()
        if value in ('first', 'before'):
            return 'first'
        if value in ('last', 'after'):
            return 'last'
    raise ValueError('none_position must be "first" or "last"')


def _safe_itemgetter(*indices):
    getter = operator.itemgetter(*indices)

    def _get(row):
        try:
            return getter(row)
        except (IndexError, KeyError):
            if len(indices) == 1:
                return None
            return tuple(None for _ in indices)

    return _get


def _value_getter(hdr, field):
    if callable(field):
        return field
    return _safe_itemgetter(*asindices(hdr, field))


def _group_getter(hdr, key):
    if key is None:
        return None
    if callable(key):
        return key
    return comparable_itemgetter(*asindices(hdr, key))


def _resolve_rank_fields(rank, hdr, key):
    if rank is not None:
        return _parse_rank_spec(rank)
    if key is None or callable(key):
        return list(range(len(hdr))), None
    key_indices = set(asindices(hdr, key))
    fields = [index for index in range(len(hdr))
              if index not in key_indices]
    return fields, None


def _rank_getter(hdr, fields, directions, none_position):
    getters = [_value_getter(hdr, field) for field in fields]

    def _getrank(row):
        return tuple(_RankValue(getter(row), reverse, none_position)
                     for getter, reverse in zip(getters, directions))

    return _getrank


class _RankValue(object):
    __slots__ = ('value', 'reverse', 'none_position')

    def __init__(self, value, reverse, none_position):
        self.value = None if value is None else Comparable(value)
        self.reverse = reverse
        self.none_position = none_position

    def _compare(self, other):
        if self.value is None and other.value is None:
            return 0
        if self.value is None:
            return -1 if self.none_position == 'first' else 1
        if other.value is None:
            return 1 if self.none_position == 'first' else -1
        if self.value < other.value:
            result = -1
        elif self.value == other.value:
            result = 0
        else:
            result = 1
        return -result if self.reverse else result

    def __lt__(self, other):
        return self._compare(other) < 0

    def __le__(self, other):
        return self._compare(other) <= 0

    def __eq__(self, other):
        if not isinstance(other, _RankValue):
            return NotImplemented
        return self._compare(other) == 0

    def __ne__(self, other):
        result = self.__eq__(other)
        if result is NotImplemented:
            return result
        return not result

    def __gt__(self, other):
        return self._compare(other) > 0

    def __ge__(self, other):
        return self._compare(other) >= 0


class _TopKItem(object):
    __slots__ = ('rank', 'sequence', 'row')

    def __init__(self, rank, sequence, row):
        self.rank = rank
        self.sequence = sequence
        self.row = row

    def better_than(self, other):
        if self.rank == other.rank:
            return self.sequence < other.sequence
        return self.rank < other.rank

    def __lt__(self, other):
        # heapq is a min-heap, so reverse the comparison to keep the
        # worst candidate at the root.
        if self.rank == other.rank:
            return self.sequence > other.sequence
        return other.rank < self.rank


def _select_topk(rows, getrank, k):
    heap = []
    for sequence, row in enumerate(rows):
        candidate = _TopKItem(getrank(row), sequence, row)
        if len(heap) < k:
            heapq.heappush(heap, candidate)
        elif candidate.better_than(heap[0]):
            heapq.heapreplace(heap, candidate)
    heap.sort(key=lambda item: (item.rank, item.sequence))
    return heap


def topk(table, key, k, rank=None, reverse=False, none_position=None,
         presorted=False, buffersize=None, tempdir=None, cache=True,
         **kwargs):
    """
    Select the top `k` rows within each group under `key`.

    The grouping key and the ranking fields are independent. The `rank`
    argument may be a single field or a sequence of fields. Each ranking field
    can have its own direction, supplied as a sequence to `reverse`, e.g.
    ``reverse=[True, False]`` means descending on the first ranking field and
    ascending on the second. A dictionary mapping fields to directions is also
    accepted. Ties are resolved by input order.

    The output is ordered first by group, then by rank. In the default mode,
    groups are sorted by `key` ascending and rows within each group are
    returned in rank order. With ``presorted=True`` the input is assumed to be
    grouped by `key`, and the group order of the input is preserved.

    ``none_position`` controls where ``None`` values appear in the ranking,
    independently of the ascending/descending direction. It must be either
    ``'first'`` or ``'last'`` (the default is ``'last'``). Numeric values are
    compared numerically, including across numeric types.

    ``k`` must be a non-negative integer. A negative value raises
    :class:`ValueError` immediately; ``k=0`` returns only the header.

    In ``presorted=True`` mode only the current group's best `k` candidates
    are retained. In the normal mode the table is sorted by group key first,
    using the usual :func:`petl.transform.sorts.sort` buffering options.

    The `rank` argument can also be supplied as `order`, and `none_position`
    as `nulls`, `null_position`, `na_position` or `none_order`.

    """

    k = _check_topk(k)

    rank_alias = _take_alias(kwargs, 'order', 'rankkey', 'sortkey',
                             'sort_by', 'orderby', 'order_by', 'by', 'field',
                             'fields', 'value')
    if rank_alias is not _NO_ALIAS:
        if rank is not None:
            raise TypeError('rank and its alias cannot both be supplied')
        rank = rank_alias

    direction_alias = _take_alias(kwargs, 'directions', 'direction')
    if direction_alias is not _NO_ALIAS:
        if reverse not in (False, None):
            raise TypeError('reverse and its alias cannot both be supplied')
        reverse = direction_alias

    none_alias = _take_alias(kwargs, 'nulls', 'null_position',
                             'na_position', 'none_order', 'none', 'missing')
    if none_alias is not _NO_ALIAS:
        if none_position is not None:
            raise TypeError(
                'none_position and its alias cannot both be supplied'
            )
        none_position = none_alias

    none_first = _take_alias(kwargs, 'none_first', 'nulls_first',
                             'na_first')
    none_last = _take_alias(kwargs, 'none_last', 'nulls_last', 'na_last')
    if none_first is not _NO_ALIAS or none_last is not _NO_ALIAS:
        if none_position is not None:
            raise TypeError(
                'none_position and its alias cannot both be supplied'
            )
        if none_first is not _NO_ALIAS and none_last is not _NO_ALIAS:
            raise TypeError('none_first and none_last cannot both be supplied')
        if none_first is not _NO_ALIAS:
            none_position = 'first' if none_first else 'last'
        else:
            none_position = 'last' if none_last else 'first'

    if kwargs:
        name = next(iter(kwargs))
        raise TypeError("topk() got an unexpected keyword argument %r" % name)

    none_position = _normalise_none_position(none_position)
    return TopKView(table, key=key, k=k, rank=rank, reverse=reverse,
                    none_position=none_position, presorted=presorted,
                    buffersize=buffersize, tempdir=tempdir, cache=cache)


Table.topk = topk
top_k = topk
groupselecttopk = topk
Table.top_k = topk
Table.groupselecttopk = topk


class TopKView(Table):

    def __init__(self, source, key, k, rank=None, reverse=False,
                 none_position='last', presorted=False, buffersize=None,
                 tempdir=None, cache=True):
        self.k = _check_topk(k)
        self.key = key
        self.rank = rank
        self.reverse = reverse
        self.none_position = _normalise_none_position(none_position)
        self.presorted = presorted
        if presorted or key is None:
            self.source = source
        else:
            self.source = sort(source, key, buffersize=buffersize,
                               tempdir=tempdir, cache=cache)

    def __iter__(self):
        return itertopk(self.source, self.key, self.k, self.rank,
                        self.reverse, self.none_position)


def itertopk(source, key, k, rank, reverse, none_position):
    it = iter(source)
    try:
        hdr = next(it)
    except StopIteration:
        return
    yield tuple(hdr)

    if k == 0:
        return

    rank_fields, embedded_directions = _resolve_rank_fields(rank, hdr,
                                                             key)
    if embedded_directions is None:
        directions = _normalise_reverse(reverse, rank_fields)
    else:
        if reverse not in (False, None):
            raise TypeError('reverse cannot be combined with per-field '
                            'rank directions')
        directions = embedded_directions
    getrank = _rank_getter(hdr, rank_fields, directions, none_position)

    if key is None:
        heap = _select_topk(it, getrank, k)
        for item in heap:
            yield tuple(item.row)
        return

    getkey = _group_getter(hdr, key)
    for _, rows in itertools.groupby(it, key=getkey):
        heap = _select_topk(rows, getrank, k)
        for item in heap:
            yield tuple(item.row)


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
