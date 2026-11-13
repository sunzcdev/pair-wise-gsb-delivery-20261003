from __future__ import absolute_import, print_function, division

from collections import Counter
from petl.compat import next
from petl.comparison import Comparable
from petl.util.base import header, Table
from petl.transform.sorts import sort
from petl.transform.basics import cut


def _resolve_multiset(strict, multiset):
    # `strict` selects set semantics, `multiset` explicitly selects
    # multiplicity-preserving semantics. The two are mutually exclusive.
    if strict and multiset:
        raise ValueError(
            'the strict and multiset arguments cannot both be True'
        )
    if strict:
        return False
    # default and explicit multiset both preserve row multiplicity
    return True


def complement(a, b, presorted=False, buffersize=None, tempdir=None,
               cache=True, strict=False, multiset=False):
    """
    Return rows in `a` that are not in `b`. E.g.::

        >>> import petl as etl
        >>> a = [['foo', 'bar', 'baz'],
        ...      ['A', 1, True],
        ...      ['C', 7, False],
        ...      ['B', 2, False],
        ...      ['C', 9, True]]
        >>> b = [['x', 'y', 'z'],
        ...      ['B', 2, False],
        ...      ['A', 9, False],
        ...      ['B', 3, True],
        ...      ['C', 9, True]]
        >>> aminusb = etl.complement(a, b)
        >>> aminusb
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'A' |   1 | True  |
        +-----+-----+-------+
        | 'C' |   7 | False |
        +-----+-----+-------+

        >>> bminusa = etl.complement(b, a)
        >>> bminusa
        +-----+---+-------+
        | x   | y | z     |
        +=====+===+=======+
        | 'A' | 9 | False |
        +-----+---+-------+
        | 'B' | 3 | True  |
        +-----+---+-------+

    Note that the field names of each table are ignored - rows are simply
    compared following a lexical sort. See also the
    :func:`petl.transform.setops.recordcomplement` function.

    If `presorted` is True, it is assumed that the data are already sorted by
    the given key, and the `buffersize`, `tempdir` and `cache` arguments are
    ignored. Otherwise, the data are sorted, see also the discussion of the
    `buffersize`, `tempdir` and `cache` arguments under the
    :func:`petl.transform.sorts.sort` function.

    Note that the default behaviour is not strictly set-like, because 
    duplicate rows are counted separately, e.g.::
    
        >>> a = [['foo', 'bar'],
        ...      ['A', 1],
        ...      ['B', 2],
        ...      ['B', 2],
        ...      ['C', 7]]
        >>> b = [['foo', 'bar'],
        ...      ['B', 2]]
        >>> aminusb = etl.complement(a, b)
        >>> aminusb
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   1 |
        +-----+-----+
        | 'B' |   2 |
        +-----+-----+
        | 'C' |   7 |
        +-----+-----+

    This behaviour can be changed with the `strict` keyword argument, e.g.::

        >>> aminusb = etl.complement(a, b, strict=True)
        >>> aminusb
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   1 |
        +-----+-----+
        | 'C' |   7 |
        +-----+-----+

    The multiplicity-preserving behaviour can also be requested explicitly
    with the `multiset` keyword argument. In multiset mode each occurrence
    of a row on the right cancels a single occurrence on the left, so a row
    that appears three times on the left and once on the right is kept
    twice::

        >>> aminusb = etl.complement(a, b, multiset=True)
        >>> aminusb
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'A' |   1 |
        +-----+-----+
        | 'B' |   2 |
        +-----+-----+
        | 'C' |   7 |
        +-----+-----+

    `strict` (set semantics, ignoring multiplicity) and `multiset` are
    mutually exclusive. The default, with neither flag given, preserves the
    historical multiset behaviour so existing calls are unchanged.

    .. versionchanged:: 1.1.0
    
    If `strict` is `True` then strict set-like behaviour is used, i.e., 
    only rows in `a` not found in `b` are returned.

    .. versionchanged:: 1.7.0

    Added the `multiset` argument for explicit multiset (bag) semantics.

    """

    return ComplementView(a, b, presorted=presorted, buffersize=buffersize,
                          tempdir=tempdir, cache=cache, strict=strict,
                          multiset=multiset)


Table.complement = complement


class ComplementView(Table):

    def __init__(self, a, b, presorted=False, buffersize=None, tempdir=None,
                 cache=True, strict=False, multiset=False):
        if presorted:
            self.a = a
            self.b = b
        else:
            self.a = sort(a, buffersize=buffersize, tempdir=tempdir,
                          cache=cache)
            self.b = sort(b, buffersize=buffersize, tempdir=tempdir,
                          cache=cache)
        self.multiset = _resolve_multiset(strict, multiset)

    def __iter__(self):
        return itercomplement(self.a, self.b, self.multiset)


def itercomplement(ta, tb, multiset):
    # coerce rows to tuples to ensure hashable and comparable
    ita = (tuple(row) for row in iter(ta))
    itb = (tuple(row) for row in iter(tb))
    ahdr = tuple(next(ita))
    next(itb)  # ignore b fields
    yield ahdr

    # single merge over both sorted streams; equal rows are grouped into runs
    # whose lengths are compared without ever materialising a whole run
    ita = _Peekable(ita)
    itb = _Peekable(itb)
    a = ita.peek()
    b = itb.peek()
    while a is not None:
        if b is None or Comparable(a) < Comparable(b):
            # row only present on the left
            yield a
            ita.advance()
            a = ita.peek()
        elif a == b:
            value = a
            na = ita.count_run(value)
            nb = itb.count_run(value)
            if multiset:
                # each occurrence on the right cancels one on the left
                remaining = na - nb
                for _ in range(remaining if remaining > 0 else 0):
                    yield value
            # in strict (set) mode a row found on the right is dropped entirely
            a = ita.peek()
            b = itb.peek()
        else:
            # row only present on the right
            itb.advance()
            b = itb.peek()


class _Peekable(object):
    """Wrap an iterator so the next row can be inspected without consuming it
    and runs of equal rows can be counted lazily, keeping only one row in
    memory at a time even when a run is very large."""

    __slots__ = ['_it', '_peeked', '_exhausted']

    def __init__(self, it):
        self._it = it
        self._peeked = None
        self._exhausted = False
        self._fill()

    def _fill(self):
        try:
            self._peeked = next(self._it)
        except StopIteration:
            self._peeked = None
            self._exhausted = True

    def peek(self):
        return self._peeked

    def advance(self):
        # consume the currently peeked row and load the next one
        if not self._exhausted:
            self._fill()

    def count_run(self, value):
        # current peeked row is known to equal value
        count = 0
        while not self._exhausted and self._peeked == value:
            count += 1
            self.advance()
        return count


def recordcomplement(a, b, buffersize=None, tempdir=None, cache=True,
                     strict=False, multiset=False):
    """
    Find records in `a` that are not in `b`. E.g.::

        >>> import petl as etl
        >>> a = [['foo', 'bar', 'baz'],
        ...      ['A', 1, True],
        ...      ['C', 7, False],
        ...      ['B', 2, False],
        ...      ['C', 9, True]]
        >>> b = [['bar', 'foo', 'baz'],
        ...      [2, 'B', False],
        ...      [9, 'A', False],
        ...      [3, 'B', True],
        ...      [9, 'C', True]]
        >>> aminusb = etl.recordcomplement(a, b)
        >>> aminusb
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'A' |   1 | True  |
        +-----+-----+-------+
        | 'C' |   7 | False |
        +-----+-----+-------+

        >>> bminusa = etl.recordcomplement(b, a)
        >>> bminusa
        +-----+-----+-------+
        | bar | foo | baz   |
        +=====+=====+=======+
        |   3 | 'B' | True  |
        +-----+-----+-------+
        |   9 | 'A' | False |
        +-----+-----+-------+

    Note that both tables must have the same set of fields, but that the order
    of the fields does not matter. See also the
    :func:`petl.transform.setops.complement` function.

    See also the discussion of the `buffersize`, `tempdir` and `cache` arguments
    under the :func:`petl.transform.sorts.sort` function.

    Rows are aligned by field name before comparison, so tables whose columns
    are in a different order are still matched correctly. The `strict` and
    `multiset` arguments have the same meaning as for
    :func:`petl.transform.setops.complement`.

    """

    # TODO possible with only one pass?

    ha = header(a)
    hb = header(b)
    assert set(ha) == set(hb), 'both tables must have the same set of fields'
    # make sure fields are in the same order
    bv = cut(b, *ha)
    return complement(a, bv, buffersize=buffersize, tempdir=tempdir,
                      cache=cache, strict=strict, multiset=multiset)


Table.recordcomplement = recordcomplement


def diff(a, b, presorted=False, buffersize=None, tempdir=None, cache=True,
         strict=False, multiset=False):
    """
    Find the difference between rows in two tables. Returns a pair of tables.
    E.g.::

        >>> import petl as etl
        >>> a = [['foo', 'bar', 'baz'],
        ...      ['A', 1, True],
        ...      ['C', 7, False],
        ...      ['B', 2, False],
        ...      ['C', 9, True]]
        >>> b = [['x', 'y', 'z'],
        ...      ['B', 2, False],
        ...      ['A', 9, False],
        ...      ['B', 3, True],
        ...      ['C', 9, True]]
        >>> added, subtracted = etl.diff(a, b)
        >>> # rows in b not in a
        ... added
        +-----+---+-------+
        | x   | y | z     |
        +=====+===+=======+
        | 'A' | 9 | False |
        +-----+---+-------+
        | 'B' | 3 | True  |
        +-----+---+-------+

        >>> # rows in a not in b
        ... subtracted
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'A' |   1 | True  |
        +-----+-----+-------+
        | 'C' |   7 | False |
        +-----+-----+-------+

    Convenient shorthand for ``(complement(b, a), complement(a, b))``. See also
    :func:`petl.transform.setops.complement`.

    If `presorted` is True, it is assumed that the data are already sorted by
    the given key, and the `buffersize`, `tempdir` and `cache` arguments are
    ignored. Otherwise, the data are sorted, see also the discussion of the
    `buffersize`, `tempdir` and `cache` arguments under the
    :func:`petl.transform.sorts.sort` function.

    .. versionchanged:: 1.1.0

    If `strict` is `True` then strict set-like behaviour is used.

    .. versionchanged:: 1.7.0

    Added the `multiset` argument for explicit multiset (bag) semantics.

    """

    if not presorted:
        a = sort(a)
        b = sort(b)
    added = complement(b, a, presorted=True, buffersize=buffersize,
                       tempdir=tempdir, cache=cache, strict=strict,
                       multiset=multiset)
    subtracted = complement(a, b, presorted=True, buffersize=buffersize,
                            tempdir=tempdir, cache=cache, strict=strict,
                            multiset=multiset)
    return added, subtracted


Table.diff = diff


def recorddiff(a, b, buffersize=None, tempdir=None, cache=True, strict=False,
               multiset=False):
    """
    Find the difference between records in two tables. E.g.::

        >>> import petl as etl
        >>> a = [['foo', 'bar', 'baz'],
        ...      ['A', 1, True],
        ...      ['C', 7, False],
        ...      ['B', 2, False],
        ...      ['C', 9, True]]
        >>> b = [['bar', 'foo', 'baz'],
        ...      [2, 'B', False],
        ...      [9, 'A', False],
        ...      [3, 'B', True],
        ...      [9, 'C', True]]
        >>> added, subtracted = etl.recorddiff(a, b)
        >>> added
        +-----+-----+-------+
        | bar | foo | baz   |
        +=====+=====+=======+
        |   3 | 'B' | True  |
        +-----+-----+-------+
        |   9 | 'A' | False |
        +-----+-----+-------+

        >>> subtracted
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'A' |   1 | True  |
        +-----+-----+-------+
        | 'C' |   7 | False |
        +-----+-----+-------+

    Convenient shorthand for
    ``(recordcomplement(b, a), recordcomplement(a, b))``. See also
    :func:`petl.transform.setops.recordcomplement`.

    See also the discussion of the `buffersize`, `tempdir` and `cache`
    arguments under the :func:`petl.transform.sorts.sort` function.

    .. versionchanged:: 1.1.0

    If `strict` is `True` then strict set-like behaviour is used.

    .. versionchanged:: 1.7.0

    Added the `multiset` argument for explicit multiset (bag) semantics.

    """

    added = recordcomplement(b, a, buffersize=buffersize, tempdir=tempdir,
                             cache=cache, strict=strict, multiset=multiset)
    subtracted = recordcomplement(a, b, buffersize=buffersize, tempdir=tempdir,
                                  cache=cache, strict=strict,
                                  multiset=multiset)
    return added, subtracted


Table.recorddiff = recorddiff


def intersection(a, b, presorted=False, buffersize=None, tempdir=None,
                 cache=True, strict=False, multiset=False):
    """
    Return rows in `a` that are also in `b`. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['A', 1, True],
        ...           ['C', 7, False],
        ...           ['B', 2, False],
        ...           ['C', 9, True]]
        >>> table2 = [['x', 'y', 'z'],
        ...           ['B', 2, False],
        ...           ['A', 9, False],
        ...           ['B', 3, True],
        ...           ['C', 9, True]]
        >>> table3 = etl.intersection(table1, table2)
        >>> table3
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'B' |   2 | False |
        +-----+-----+-------+
        | 'C' |   9 | True  |
        +-----+-----+-------+

    If `presorted` is True, it is assumed that the data are already sorted by
    the given key, and the `buffersize`, `tempdir` and `cache` arguments are
    ignored. Otherwise, the data are sorted, see also the discussion of the
    `buffersize`, `tempdir` and `cache` arguments under the
    :func:`petl.transform.sorts.sort` function.

    By default rows are compared as a multiset (bag), so a row occurring
    `n` times on the left and `m` times on the right is emitted
    ``min(n, m)`` times. Passing `multiset=True` requests this behaviour
    explicitly, while `strict=True` selects set semantics where each matching
    row is emitted at most once::

        >>> table1 = [['foo', 'bar'],
        ...           ['B', 2], ['B', 2], ['B', 2]]
        >>> table2 = [['foo', 'bar'],
        ...           ['B', 2], ['B', 2]]
        >>> list(etl.intersection(table1, table2).data())
        [('B', 2), ('B', 2)]
        >>> list(etl.intersection(table1, table2, strict=True).data())
        [('B', 2)]

    `strict` and `multiset` are mutually exclusive; with neither flag given
    the historical multiset behaviour is retained.

    .. versionchanged:: 1.7.0

    Added the `strict` and `multiset` arguments.

    """

    return IntersectionView(a, b, presorted=presorted, buffersize=buffersize,
                            tempdir=tempdir, cache=cache, strict=strict,
                            multiset=multiset)


Table.intersection = intersection


class IntersectionView(Table):

    def __init__(self, a, b, presorted=False, buffersize=None, tempdir=None,
                 cache=True, strict=False, multiset=False):
        if presorted:
            self.a = a
            self.b = b
        else:
            self.a = sort(a, buffersize=buffersize, tempdir=tempdir,
                          cache=cache)
            self.b = sort(b, buffersize=buffersize, tempdir=tempdir,
                          cache=cache)
        self.multiset = _resolve_multiset(strict, multiset)

    def __iter__(self):
        return iterintersection(self.a, self.b, self.multiset)


def iterintersection(a, b, multiset):
    ita = _Peekable(tuple(row) for row in iter(a))
    itb = _Peekable(tuple(row) for row in iter(b))
    ahdr = ita.peek()
    ita.advance()  # move past the a header
    itb.advance()  # ignore b header, assume same fields as a
    yield tuple(ahdr)
    left = ita.peek()
    right = itb.peek()
    while left is not None and right is not None:
        if Comparable(left) < Comparable(right):
            ita.advance()
            left = ita.peek()
        elif left == right:
            value = left
            nleft = ita.count_run(value)
            nright = itb.count_run(value)
            if multiset:
                common = nleft if nleft < nright else nright
            else:
                common = 1
            for _ in range(common):
                yield value
            left = ita.peek()
            right = itb.peek()
        else:
            itb.advance()
            right = itb.peek()


def recordintersection(a, b, buffersize=None, tempdir=None, cache=True,
                       strict=False, multiset=False):
    """
    Find records common to `a` and `b`, aligning columns by field name.

    Like :func:`petl.transform.setops.intersection` but the order of the
    fields in the two tables does not matter; rows are rearranged so matching
    field names are compared. Both tables must expose the same set of fields.

    The `strict` and `multiset` arguments control whether row multiplicity is
    preserved, exactly as for
    :func:`petl.transform.setops.intersection`.

    .. versionadded:: 1.7.0

    """

    ha = header(a)
    hb = header(b)
    assert set(ha) == set(hb), 'both tables must have the same set of fields'
    bv = cut(b, *ha)
    return intersection(a, bv, buffersize=buffersize, tempdir=tempdir,
                        cache=cache, strict=strict, multiset=multiset)


Table.recordintersection = recordintersection


def hashcomplement(a, b, strict=False, multiset=False):
    """
    Alternative implementation of :func:`petl.transform.setops.complement`,
    where the complement is executed by constructing an in-memory set for all
    rows found in the right hand table, then iterating over rows from the
    left hand table.

    May be faster and/or more resource efficient where the right table is small
    and the left table is large.
    
    .. versionchanged:: 1.1.0
    
    If `strict` is `True` then strict set-like behaviour is used, i.e., 
    only rows in `a` not found in `b` are returned.

    .. versionchanged:: 1.7.0

    Added the `multiset` argument for explicit multiset (bag) semantics.

    """

    return HashComplementView(a, b, strict=strict, multiset=multiset)


Table.hashcomplement = hashcomplement


class HashComplementView(Table):

    def __init__(self, a, b, strict=False, multiset=False):
        self.a = a
        self.b = b
        self.multiset = _resolve_multiset(strict, multiset)

    def __iter__(self):
        return iterhashcomplement(self.a, self.b, self.multiset)


def iterhashcomplement(a, b, multiset):
    ita = iter(a)
    ahdr = next(ita)
    yield tuple(ahdr)
    itb = iter(b)
    next(itb)  # discard b header, assume same as a

    # N.B., need to account for possibility of duplicate rows
    bcnt = Counter(tuple(row) for row in itb)
    for ar in ita:
        t = tuple(ar)
        if bcnt[t] > 0:
            if multiset:
                bcnt[t] -= 1
            # in strict mode a single match removes every left-hand duplicate
        else:
            yield t


def hashintersection(a, b, strict=False, multiset=False):
    """
    Alternative implementation of
    :func:`petl.transform.setops.intersection`, where the intersection
    is executed by constructing an in-memory set for all rows found in the
    right hand table, then iterating over rows from the left hand table.

    May be faster and/or more resource efficient where the right table is small
    and the left table is large.

    By default rows are matched as a multiset (bag): a row is emitted once for
    every occurrence available on both sides. Pass `multiset=True` to request
    this explicitly or `strict=True` for set semantics where each matching row
    is emitted at most once. The two flags are mutually exclusive.

    .. versionchanged:: 1.7.0

    Added the `strict` and `multiset` arguments.

    """

    return HashIntersectionView(a, b, strict=strict, multiset=multiset)


Table.hashintersection = hashintersection


class HashIntersectionView(Table):

    def __init__(self, a, b, strict=False, multiset=False):
        self.a = a
        self.b = b
        self.multiset = _resolve_multiset(strict, multiset)

    def __iter__(self):
        return iterhashintersection(self.a, self.b, self.multiset)


def iterhashintersection(a, b, multiset):
    ita = iter(a)
    ahdr = next(ita)
    yield tuple(ahdr)
    itb = iter(b)
    next(itb)  # discard b header, assume same as a

    # N.B., need to account for possibility of duplicate rows
    bcnt = Counter(tuple(row) for row in itb)
    if not multiset:
        # set semantics: membership only, multiplicity ignored. A plain set is
        # used rather than Counter(...keys()), because a Counter retains keys
        # even once their count falls to zero.
        bset = set(bcnt)
        for ar in ita:
            t = tuple(ar)
            if t in bset:
                yield t
                bset.discard(t)
        return
    for ar in ita:
        t = tuple(ar)
        if bcnt[t] > 0:
            yield t
            bcnt[t] -= 1
