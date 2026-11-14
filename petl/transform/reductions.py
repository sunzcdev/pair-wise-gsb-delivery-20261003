from __future__ import absolute_import, print_function, division


import itertools
import operator
from collections import OrderedDict
from petl.compat import next, string_types, reduce, text_type


from petl.errors import ArgumentError, FieldSelectionError
from petl.util.base import Table, iterpeek, rowgroupby, asindices
from petl.util.base import values, records
from petl.util.counting import nrows
from petl.transform.sorts import sort, mergesort
from petl.transform.basics import cut
from petl.transform.dedup import distinct


def rowreduce(table, key, reducer, header=None, presorted=False,
              buffersize=None, tempdir=None, cache=True):
    """
    Group rows under the given key then apply `reducer` to produce a single
    output row for each input group of rows. E.g.::
    
        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 3],
        ...           ['a', 7],
        ...           ['b', 2],
        ...           ['b', 1],
        ...           ['b', 9],
        ...           ['c', 4]]
        >>> def sumbar(key, rows):
        ...     return [key, sum(row[1] for row in rows)]
        ...
        >>> table2 = etl.rowreduce(table1, key='foo', reducer=sumbar,
        ...                        header=['foo', 'barsum'])
        >>> table2
        +-----+--------+
        | foo | barsum |
        +=====+========+
        | 'a' |     10 |
        +-----+--------+
        | 'b' |     12 |
        +-----+--------+
        | 'c' |      4 |
        +-----+--------+
    
    N.B., this is not strictly a "reduce" in the sense of the standard Python
    :func:`reduce` function, i.e., the `reducer` function is *not* applied 
    recursively to values within a group, rather it is applied once to each row 
    group as a whole.
    
    See also :func:`petl.transform.reductions.aggregate` and
    :func:`petl.transform.reductions.fold`.
    
    """

    return RowReduceView(table, key, reducer, header=header,
                         presorted=presorted, 
                         buffersize=buffersize, tempdir=tempdir, cache=cache)


Table.rowreduce = rowreduce


class RowReduceView(Table):
    
    def __init__(self, source, key, reducer, header=None,
                 presorted=False, buffersize=None, tempdir=None, cache=True):
        if presorted:
            self.source = source
        else:
            self.source = sort(source, key, buffersize=buffersize, 
                               tempdir=tempdir, cache=cache)
        self.key = key
        self._header = header
        self.reducer = reducer

    def __iter__(self):
        return iterrowreduce(self.source, self.key, self.reducer, self._header)

    
def iterrowreduce(source, key, reducer, header):
    if header is None:
        # output header from source
        header, source = iterpeek(source)
    yield tuple(header)
    for key, rows in rowgroupby(source, key):
        yield tuple(reducer(key, rows))
        

def aggregate(table, key, aggregation=None, value=None, presorted=False,
              buffersize=None, tempdir=None, cache=True, field='value'):
    """Apply aggregation functions.
    E.g.::

        >>> import petl as etl
        >>>
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['a', 3, True],
        ...           ['a', 7, False],
        ...           ['b', 2, True],
        ...           ['b', 2, False],
        ...           ['b', 9, False],
        ...           ['c', 4, True]]
        >>> # aggregate whole rows
        ... table2 = etl.aggregate(table1, 'foo', len)
        >>> table2
        +-----+-------+
        | foo | value |
        +=====+=======+
        | 'a' |     2 |
        +-----+-------+
        | 'b' |     3 |
        +-----+-------+
        | 'c' |     1 |
        +-----+-------+

        >>> # aggregate whole rows without a key
        >>> etl.aggregate(table1, None, len)
        +-------+
        | value |
        +=======+
        |     6 |
        +-------+

        >>> # aggregate single field
        ... table3 = etl.aggregate(table1, 'foo', sum, 'bar')
        >>> table3
        +-----+-------+
        | foo | value |
        +=====+=======+
        | 'a' |    10 |
        +-----+-------+
        | 'b' |    13 |
        +-----+-------+
        | 'c' |     4 |
        +-----+-------+

        >>> # aggregate single field without a key
        >>> etl.aggregate(table1, None, sum, 'bar')
        +-------+
        | value |
        +=======+
        |    27 |
        +-------+

        >>> # alternative signature using keyword args
        ... table4 = etl.aggregate(table1, key=('foo', 'bar'),
        ...                        aggregation=list, value=('bar', 'baz'))
        >>> table4
        +-----+-----+-------------------------+
        | foo | bar | value                   |
        +=====+=====+=========================+
        | 'a' |   3 | [(3, True)]             |
        +-----+-----+-------------------------+
        | 'a' |   7 | [(7, False)]            |
        +-----+-----+-------------------------+
        | 'b' |   2 | [(2, True), (2, False)] |
        +-----+-----+-------------------------+
        | 'b' |   9 | [(9, False)]            |
        +-----+-----+-------------------------+
        | 'c' |   4 | [(4, True)]             |
        +-----+-----+-------------------------+

        >>> # alternative signature using keyword args without a key
        >>> etl.aggregate(table1, key=None,
        ...                 aggregation=list, value=('bar', 'baz'))
        +-----------------------------------------------------------------------+
        | value                                                                 |
        +=======================================================================+
        | [(3, True), (7, False), (2, True), (2, False), (9, False), (4, True)] |
        +-----------------------------------------------------------------------+

        >>> # aggregate multiple fields
        ... from collections import OrderedDict
        >>> import petl as etl
        >>>
        >>> aggregation = OrderedDict()
        >>> aggregation['count'] = len
        >>> aggregation['minbar'] = 'bar', min
        >>> aggregation['maxbar'] = 'bar', max
        >>> aggregation['sumbar'] = 'bar', sum
        >>> # default aggregation function is list
        ... aggregation['listbar'] = 'bar'
        >>> aggregation['listbarbaz'] = ('bar', 'baz'), list
        >>> aggregation['bars'] = 'bar', etl.strjoin(', ')
        >>> table5 = etl.aggregate(table1, 'foo', aggregation)
        >>> table5
        +-----+-------+--------+--------+--------+-----------+-------------------------------------+-----------+
        | foo | count | minbar | maxbar | sumbar | listbar   | listbarbaz                          | bars      |
        +=====+=======+========+========+========+===========+=====================================+===========+
        | 'a' |     2 |      3 |      7 |     10 | [3, 7]    | [(3, True), (7, False)]             | '3, 7'    |
        +-----+-------+--------+--------+--------+-----------+-------------------------------------+-----------+
        | 'b' |     3 |      2 |      9 |     13 | [2, 2, 9] | [(2, True), (2, False), (9, False)] | '2, 2, 9' |
        +-----+-------+--------+--------+--------+-----------+-------------------------------------+-----------+
        | 'c' |     1 |      4 |      4 |      4 | [4]       | [(4, True)]                         | '4'       |
        +-----+-------+--------+--------+--------+-----------+-------------------------------------+-----------+

        >>> # aggregate multiple fields without a key
        >>> etl.aggregate(table1, None, aggregation)
        +-------+--------+--------+--------+--------------------+-----------------------------------------------------------------------+--------------------+
        | count | minbar | maxbar | sumbar | listbar            | listbarbaz                                                            | bars               |
        +=======+========+========+========+====================+=======================================================================+====================+
        |     6 |      2 |      9 |     27 | [3, 7, 2, 2, 9, 4] | [(3, True), (7, False), (2, True), (2, False), (9, False), (4, True)] | '3, 7, 2, 2, 9, 4' |
        +-------+--------+--------+--------+--------------------+-----------------------------------------------------------------------+--------------------+

    If `presorted` is True, it is assumed that the data are already sorted by
    the given key, and the `buffersize`, `tempdir` and `cache` arguments are 
    ignored. Otherwise, the data are sorted, see also the discussion of the 
    `buffersize`, `tempdir` and `cache` arguments under the
    :func:`petl.transform.sorts.sort` function.

    If `key` is None, sorting is not necessary.

    If both `key` and `value` are None, a callable `aggregation` receives an
    iterator of whole records, with values accessible by field name or index.

    A single-field key may also be specified as a one-item list or tuple,
    for both simple and multiple aggregations.

    The `aggregation` argument also accepts instances of
    :class:`petl.transform.reductions.StatefulAggregator` (e.g.,
    :class:`petl.transform.reductions.Count`,
    :class:`petl.transform.reductions.Mean`,
    :class:`petl.transform.reductions.Variance`), both for simple and for
    multiple aggregations. Stateful aggregators can also be computed
    incrementally over separate input chunks, see
    :func:`petl.transform.reductions.partial_aggregate` and
    :func:`petl.transform.reductions.merge_aggregate`.

    """

    if isinstance(aggregation, StatefulAggregator):
        return SimpleAggregateView(table, key,
                                   aggregation=_stateful_apply(aggregation),
                                   value=value, presorted=presorted,
                                   buffersize=buffersize, tempdir=tempdir,
                                   cache=cache, field=field)
    elif callable(aggregation):
        return SimpleAggregateView(table, key, aggregation=aggregation, 
                                   value=value, presorted=presorted, 
                                   buffersize=buffersize, tempdir=tempdir, 
                                   cache=cache, field=field)
    elif aggregation is None or isinstance(aggregation, (list, tuple, dict)):
        # ignore value arg
        return MultiAggregateView(table, key, aggregation=aggregation,  
                                  presorted=presorted, buffersize=buffersize, 
                                  tempdir=tempdir, cache=cache)
    else:
        raise ArgumentError('expected aggregation is callable, list, tuple, dict '
                        'or None')


Table.aggregate = aggregate


class SimpleAggregateView(Table):
    
    def __init__(self, table, key, aggregation=list, value=None, 
                 presorted=False, buffersize=None, tempdir=None,
                 cache=True, field='value'):
        if presorted or key is None:
            self.table = table
        else:
            self.table = sort(table, key, buffersize=buffersize, 
                              tempdir=tempdir, cache=cache)    
        self.key = key
        self.aggregation = aggregation
        self.value = value
        self.field = field
        
    def __iter__(self):
        return itersimpleaggregate(self.table, self.key, self.aggregation, 
                                   self.value, self.field)


def itersimpleaggregate(table, key, aggregation, value, field):

    # special case counting
    if aggregation == len and key is not None:
        aggregation = lambda g: sum(1 for _ in g)  # count length of iterable

    # special case where length of key is 1
    if isinstance(key, (list, tuple)) and len(key) == 1:
        key = key[0]

    # determine output header
    if isinstance(key, (list, tuple)):
        outhdr = tuple(key) + (field,)
    elif callable(key):
        outhdr = ('key', field)
    elif key is None:
        outhdr = field,
    else:
        outhdr = (key, field)
    yield outhdr

    # generate data
    if isinstance(key, (list, tuple)):
        for k, grp in rowgroupby(table, key, value):
            yield tuple(k) + (aggregation(grp),)
    elif key is None:
        # special case counting
        if aggregation == len:
            yield nrows(table),
        elif value is None:
            yield aggregation(iter(records(table))),
        else:
            yield aggregation(values(table, value)),
    else:
        for k, grp in rowgroupby(table, key, value):
            yield k, aggregation(grp)


class MultiAggregateView(Table):
    
    def __init__(self, source, key, aggregation=None, presorted=False, 
                 buffersize=None, tempdir=None, cache=True):
        if presorted or key is None:
            self.source = source
        else:
            self.source = sort(source, key, buffersize=buffersize, 
                               tempdir=tempdir, cache=cache)
        self.key = key
        if aggregation is None:
            self.aggregation = OrderedDict()
        elif isinstance(aggregation, (list, tuple)):
            self.aggregation = OrderedDict()
            for t in aggregation:
                self.aggregation[t[0]] = _finalise_stateful_spec(t[1:])
        elif isinstance(aggregation, dict):
            self.aggregation = OrderedDict(
                (outfld, _finalise_stateful_spec(spec))
                for outfld, spec in aggregation.items()
            )
        else:
            raise ArgumentError(
                'expected aggregation is None, list, tuple or dict, found %r'
                % aggregation
            )

    def __iter__(self):
        return itermultiaggregate(self.source, self.key, self.aggregation)
    
    def __setitem__(self, key, value):
        self.aggregation[key] = value

    
def itermultiaggregate(source, key, aggregation):
    # normalise singleton selectors as in itersimpleaggregate
    if isinstance(key, (list, tuple)) and len(key) == 1:
        key = key[0]

    aggregation = OrderedDict(aggregation.items())  # take a copy
    it = iter(source)
    hdr = next(it)
    # push back header to ensure we iterate only once
    it = itertools.chain([hdr], it)

    # normalise aggregators
    for outfld in aggregation:
        agg = aggregation[outfld]
        if callable(agg):
            aggregation[outfld] = None, agg
        elif isinstance(agg, string_types):
            aggregation[outfld] = agg, list  # list is default
        elif len(agg) == 1 and isinstance(agg[0], string_types):
            aggregation[outfld] = agg[0], list  # list is default
        elif len(agg) == 1 and callable(agg[0]):
            aggregation[outfld] = None, agg[0]  # aggregate whole rows
        elif len(agg) == 2:
            pass  # no need to normalise
        else:
            raise ArgumentError('invalid aggregation: %r, %r' % (outfld, agg))

    # determine output header
    if isinstance(key, (list, tuple)):
        outhdr = list(key)
    elif callable(key):
        outhdr = ['key']
    elif key is None:
        outhdr = []
    else:
        outhdr = [key]
    for outfld in aggregation:
        outhdr.append(outfld)
    yield tuple(outhdr)

    if key is None:
        grouped = rowgroupby(it, lambda x: None)
    else:
        grouped = rowgroupby(it, key)

    # generate data
    for k, rows in grouped:
        rows = list(rows)  # may need to iterate over these more than once
        # handle compound key
        if isinstance(key, (list, tuple)):
            outrow = list(k)
        elif key is None:
            outrow = []
        else:
            outrow = [k]
        for outfld in aggregation:
            srcfld, aggfun = aggregation[outfld]
            if srcfld is None:
                aggval = aggfun(rows)
                outrow.append(aggval)
            elif isinstance(srcfld, (list, tuple)):
                idxs = [hdr.index(f) for f in srcfld]
                valgetter = operator.itemgetter(*idxs)
                vals = (valgetter(row) for row in rows)
                aggval = aggfun(vals)
                outrow.append(aggval)
            else:
                idx = hdr.index(srcfld)
                # try using generator comprehension
                vals = (row[idx] for row in rows)
                aggval = aggfun(vals)
                outrow.append(aggval)
        yield tuple(outrow)


class StatefulAggregator(object):
    """
    Protocol for stateful, two-phase aggregation.

    A stateful aggregator accumulates the values of a group into an
    intermediate *state*, instead of directly producing a result. States
    from different input chunks can then be combined via :meth:`merge`,
    and the final result is produced from the merged state via
    :meth:`finalize`. This allows large extraction jobs to compute and
    store per-group partial states separately (e.g., per input chunk),
    then merge them later without re-reading the raw data.

    Subclasses must implement four methods:

    * ``initial()`` - return a fresh state object. Called once per group
      per iteration, so each group and each pass over the data always
      starts from an independent state. The initial state must be an
      identity for :meth:`merge`, i.e., ``merge(initial(), s)`` must be
      equivalent to ``s``.

    * ``accumulate(state, value)`` - fold a single value into the state
      and return the updated state. The ``state`` argument is always
      framework-owned (produced by ``initial()``, ``accumulate()`` or
      ``merge()`` during the current group), so it may be mutated and
      returned, or replaced by a new object.

    * ``merge(state, other)`` - combine two partial states and return the
      merged state. Must *not* mutate ``other``, which may be a
      user-supplied state object shared with other computations.

    * ``finalize(state)`` - produce the output value from a state. Must
      not mutate the state.

    Merging must be associative, and is assumed to be commutative unless
    the class attribute ``commutative`` is set to ``False``. Aggregators
    that are not commutative (i.e., the merge result depends on the order
    of the chunks) are rejected by
    :func:`petl.transform.reductions.merge_aggregate` unless ``ordered``
    is explicitly set to ``True``.

    Instances of this protocol can be used anywhere an aggregation
    function is accepted by
    :func:`petl.transform.reductions.aggregate`, e.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 3],
        ...           ['a', 7],
        ...           ['b', 2]]
        >>> etl.aggregate(table1, 'foo', etl.Mean(), 'bar')
        +-----+-------+
        | foo | value |
        +=====+=======+
        | 'a' |   5.0 |
        +-----+-------+
        | 'b' |   2.0 |
        +-----+-------+

    See also :class:`Count`, :class:`Mean`, :class:`Variance`,
    :func:`petl.transform.reductions.partial_aggregate` and
    :func:`petl.transform.reductions.merge_aggregate`.

    .. versionadded:: 1.8.0

    """

    commutative = True

    def initial(self):
        """Return a fresh, empty state object."""
        raise NotImplementedError

    def accumulate(self, state, value):
        """Fold `value` into `state`, return the updated state."""
        raise NotImplementedError

    def merge(self, state, other):
        """Merge partial state `other` into `state`, return the merged
        state. Must not mutate `other`."""
        raise NotImplementedError

    def finalize(self, state):
        """Produce the output value from `state`."""
        raise NotImplementedError


class Count(StatefulAggregator):
    """
    Stateful aggregator counting the values in a group. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 3],
        ...           ['a', None],
        ...           ['b', 2]]
        >>> etl.aggregate(table1, 'foo', etl.Count(), 'bar')
        +-----+-------+
        | foo | value |
        +=====+=======+
        | 'a' |     2 |
        +-----+-------+
        | 'b' |     1 |
        +-----+-------+

    If `skip_missing` is True, missing values (``None``) are not counted.

    .. versionadded:: 1.8.0

    """

    def __init__(self, skip_missing=False):
        self.skip_missing = skip_missing

    def initial(self):
        return 0

    def accumulate(self, state, value):
        if self.skip_missing and value is None:
            return state
        return state + 1

    def merge(self, state, other):
        return state + other

    def finalize(self, state):
        return state


class Mean(StatefulAggregator):
    """
    Stateful aggregator computing the arithmetic mean of the non-missing
    values in a group. Missing values (``None``) are skipped; if a group
    contains no non-missing values, the result is ``None``. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 3],
        ...           ['a', 7],
        ...           ['b', None],
        ...           ['c', 2]]
        >>> etl.aggregate(table1, 'foo', etl.Mean(), 'bar')
        +-----+-------+
        | foo | value |
        +=====+=======+
        | 'a' |   5.0 |
        +-----+-------+
        | 'b' |  None |
        +-----+-------+
        | 'c' |   2.0 |
        +-----+-------+

    Works with any numeric type supporting addition and division,
    including :class:`decimal.Decimal`.

    .. versionadded:: 1.8.0

    """

    def initial(self):
        # (total, count)
        return 0, 0

    def accumulate(self, state, value):
        if value is None:
            return state
        total, count = state
        return total + value, count + 1

    def merge(self, state, other):
        total, count = state
        ototal, ocount = other
        return total + ototal, count + ocount

    def finalize(self, state):
        total, count = state
        if count == 0:
            return None
        return total / count


class Variance(StatefulAggregator):
    """
    Stateful aggregator computing the variance of the non-missing values
    in a group, together with the number of values seen. Missing values
    (``None``) are skipped. The result is a ``(variance, n)`` tuple, where
    `n` is the number of non-missing values and `variance` is ``None`` if
    `n` is too small to compute it. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 3],
        ...           ['a', 7],
        ...           ['b', None],
        ...           ['c', 2]]
        >>> etl.aggregate(table1, 'foo', etl.Variance(), 'bar')
        +-----+-----------+
        | foo | value     |
        +=====+===========+
        | 'a' | (8.0, 2)  |
        +-----+-----------+
        | 'b' | (None, 0) |
        +-----+-----------+
        | 'c' | (None, 1) |
        +-----+-----------+

    By default the sample variance (divisor ``n - 1``) is computed; pass
    ``sample=False`` for the population variance (divisor ``n``).

    States are combined using the parallel variance algorithm of Chan et
    al., so partial states computed over separate chunks of a group merge
    to the same result (up to floating point rounding) as a single pass.

    .. versionadded:: 1.8.0

    """

    def __init__(self, sample=True):
        self.sample = sample

    def initial(self):
        # (count, mean, m2) - Welford's algorithm
        return 0, 0, 0

    def accumulate(self, state, value):
        if value is None:
            return state
        count, mean, m2 = state
        count += 1
        delta = value - mean
        mean += delta / count
        m2 += delta * (value - mean)
        return count, mean, m2

    def merge(self, state, other):
        count, mean, m2 = state
        ocount, omean, om2 = other
        if ocount == 0:
            return count, mean, m2
        if count == 0:
            return ocount, omean, om2
        total = count + ocount
        delta = omean - mean
        mean = mean + delta * ocount / total
        m2 = m2 + om2 + delta * delta * count * ocount / total
        return total, mean, m2

    def finalize(self, state):
        count, mean, m2 = state
        if self.sample:
            variance = m2 / (count - 1) if count > 1 else None
        else:
            variance = m2 / count if count > 0 else None
        return variance, count


def _stateful_accumulate(aggregator):
    """Wrap a StatefulAggregator as a callable mapping an iterable of
    values to a partial state. A fresh state is created on each call, so
    states are never shared between groups or between iterations."""
    def accumulate_all(vals):
        state = aggregator.initial()
        for v in vals:
            state = aggregator.accumulate(state, v)
        return state
    return accumulate_all


def _stateful_apply(aggregator):
    """Wrap a StatefulAggregator as a plain aggregation function mapping
    an iterable of values to a final result."""
    accumulate_all = _stateful_accumulate(aggregator)

    def apply(vals):
        return aggregator.finalize(accumulate_all(vals))
    return apply


def _finalise_stateful_spec(spec):
    """Replace any StatefulAggregator found in a multi-aggregation spec
    with an equivalent plain callable."""
    if isinstance(spec, StatefulAggregator):
        return _stateful_apply(spec)
    elif isinstance(spec, (list, tuple)):
        if len(spec) == 1 and isinstance(spec[0], StatefulAggregator):
            return _stateful_apply(spec[0]),
        elif len(spec) == 2 and isinstance(spec[1], StatefulAggregator):
            return spec[0], _stateful_apply(spec[1])
    return spec


def _normalise_stateful_spec(spec):
    """Normalise a multi-aggregation spec to a (source field,
    StatefulAggregator) pair, raising ArgumentError if the spec does not
    contain a StatefulAggregator."""
    if isinstance(spec, StatefulAggregator):
        return None, spec
    elif isinstance(spec, (list, tuple)):
        if len(spec) == 1 and isinstance(spec[0], StatefulAggregator):
            return None, spec[0]
        elif len(spec) == 2 and isinstance(spec[1], StatefulAggregator):
            return spec[0], spec[1]
    raise ArgumentError(
        'expected a StatefulAggregator specification, found %r' % (spec,)
    )


def partial_aggregate(table, key, aggregation, value=None, presorted=False,
                      buffersize=None, tempdir=None, cache=True,
                      field='state'):
    """
    Compute per-group partial aggregation states, for later merging via
    :func:`petl.transform.reductions.merge_aggregate`. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', 3],
        ...           ['a', 7],
        ...           ['b', 2]]
        >>> p = etl.partial_aggregate(table1, 'foo', etl.Mean(), 'bar')
        >>> p
        +-----+---------+
        | foo | state   |
        +=====+=========+
        | 'a' | (10, 2) |
        +-----+---------+
        | 'b' | (2, 1)  |
        +-----+---------+

    The `aggregation` argument must be a
    :class:`petl.transform.reductions.StatefulAggregator` instance, or -
    as with :func:`petl.transform.reductions.aggregate` - a dict, list or
    tuple of aggregation specifications, in which case one state column
    per output field is emitted.

    Each group is accumulated from a fresh state on every iteration, so
    state objects in the output are never shared between groups or
    between passes over the table.

    .. versionadded:: 1.8.0

    """

    if isinstance(aggregation, StatefulAggregator):
        return SimplePartialAggregateView(table, key, aggregation,
                                          value=value, presorted=presorted,
                                          buffersize=buffersize,
                                          tempdir=tempdir, cache=cache,
                                          field=field)
    elif isinstance(aggregation, (list, tuple, dict)):
        return MultiPartialAggregateView(table, key, aggregation,
                                         presorted=presorted,
                                         buffersize=buffersize,
                                         tempdir=tempdir, cache=cache)
    else:
        raise ArgumentError(
            'expected a StatefulAggregator or a collection of '
            'StatefulAggregator specifications, found %r' % (aggregation,)
        )


Table.partial_aggregate = partial_aggregate


class SimplePartialAggregateView(Table):

    def __init__(self, table, key, aggregation, value=None,
                 presorted=False, buffersize=None, tempdir=None,
                 cache=True, field='state'):
        if presorted or key is None:
            self.table = table
        else:
            self.table = sort(table, key, buffersize=buffersize,
                              tempdir=tempdir, cache=cache)
        self.key = key
        self.aggregation = aggregation
        self.value = value
        self.field = field

    def __iter__(self):
        return itersimplepartial(self.table, self.key, self.aggregation,
                                 self.value, self.field)


def itersimplepartial(table, key, aggregator, value, field):

    # special case where length of key is 1
    if isinstance(key, (list, tuple)) and len(key) == 1:
        key = key[0]

    accumulate = _stateful_accumulate(aggregator)

    # determine output header
    if isinstance(key, (list, tuple)):
        outhdr = tuple(key) + (field,)
    elif callable(key):
        outhdr = ('key', field)
    elif key is None:
        outhdr = field,
    else:
        outhdr = (key, field)
    yield outhdr

    # generate data
    if isinstance(key, (list, tuple)):
        for k, grp in rowgroupby(table, key, value):
            yield tuple(k) + (accumulate(grp),)
    elif key is None:
        if value is None:
            yield accumulate(iter(records(table))),
        else:
            yield accumulate(values(table, value)),
    else:
        for k, grp in rowgroupby(table, key, value):
            yield k, accumulate(grp)


class MultiPartialAggregateView(Table):

    def __init__(self, source, key, aggregation, presorted=False,
                 buffersize=None, tempdir=None, cache=True):
        if presorted or key is None:
            self.source = source
        else:
            self.source = sort(source, key, buffersize=buffersize,
                               tempdir=tempdir, cache=cache)
        self.key = key
        if isinstance(aggregation, dict):
            items = list(aggregation.items())
        else:
            items = [(t[0], t[1:]) for t in aggregation]
        self.aggregation = OrderedDict()
        for outfld, spec in items:
            srcfld, aggregator = _normalise_stateful_spec(spec)
            self.aggregation[outfld] = srcfld, _stateful_accumulate(aggregator)

    def __iter__(self):
        return itermultiaggregate(self.source, self.key, self.aggregation)


def merge_aggregate(partials, key, aggregation, ordered=False,
                    presorted=False, buffersize=None, tempdir=None,
                    cache=True, field='value'):
    """
    Merge partial aggregation states computed by
    :func:`petl.transform.reductions.partial_aggregate` and produce final
    results. E.g.::

        >>> import petl as etl
        >>> chunk1 = [['foo', 'bar'], ['a', 3], ['b', 2]]
        >>> chunk2 = [['foo', 'bar'], ['a', 7], ['b', 4], ['b', 1]]
        >>> p1 = etl.partial_aggregate(chunk1, 'foo', etl.Mean(), 'bar')
        >>> p2 = etl.partial_aggregate(chunk2, 'foo', etl.Mean(), 'bar')
        >>> etl.merge_aggregate([p1, p2], 'foo', etl.Mean())
        +-----+--------------------+
        | foo | value              |
        +=====+====================+
        | 'a' |                5.0 |
        +-----+--------------------+
        | 'b' | 2.3333333333333335 |
        +-----+--------------------+

    The `partials` argument may be a single table of partial states or a
    sequence of such tables, e.g., one per input chunk. States for the
    same group are merged with the
    :meth:`petl.transform.reductions.StatefulAggregator.merge` method of
    `aggregation`, which must be a
    :class:`petl.transform.reductions.StatefulAggregator` instance, or a
    dict, list or tuple of aggregation specifications as accepted by
    :func:`petl.transform.reductions.partial_aggregate`.

    Unless `presorted` is True, the merged input is sorted by `key`
    before grouping. If `ordered` is True, the input is assumed to be
    already grouped by `key` and the states within each group are merged
    in the order they are encountered; this is required for aggregators
    whose merge is not commutative (``commutative = False``), which are
    otherwise rejected with an
    :exc:`petl.errors.ArgumentError`.

    For floating point aggregations, results may differ slightly from a
    one-shot computation because values are combined in a different
    association order; expect agreement within a relative tolerance of
    about 1e-9.

    .. versionadded:: 1.8.0

    """

    if isinstance(aggregation, StatefulAggregator):
        return MergeAggregateView(partials, key, aggregation,
                                  ordered=ordered, presorted=presorted,
                                  buffersize=buffersize, tempdir=tempdir,
                                  cache=cache, field=field)
    elif isinstance(aggregation, (list, tuple, dict)):
        return MultiMergeAggregateView(partials, key, aggregation,
                                       ordered=ordered, presorted=presorted,
                                       buffersize=buffersize, tempdir=tempdir,
                                       cache=cache)
    else:
        raise ArgumentError(
            'expected a StatefulAggregator or a collection of '
            'StatefulAggregator specifications, found %r' % (aggregation,)
        )


Table.merge_aggregate = merge_aggregate


def _as_table_list(partials):
    """Normalise the `partials` argument of merge_aggregate to a list of
    tables."""
    if isinstance(partials, Table):
        return [partials]
    elif isinstance(partials, (list, tuple)):
        if len(partials) == 0:
            return []
        first = partials[0]
        if isinstance(first, Table):
            return list(partials)
        elif isinstance(first, (list, tuple)) and len(first) > 0 \
                and isinstance(first[0], (list, tuple, Table)):
            # a sequence of tables, each given as a sequence of rows
            return list(partials)
        else:
            # a single table given as a sequence of rows
            return [partials]
    else:
        # some other iterable, assume a single table
        return [partials]


def _iterchaintables(tables):
    """Chain the data rows of several tables, verifying that all headers
    are identical."""
    hdr = None
    for table in tables:
        it = iter(table)
        try:
            thdr = tuple(next(it))
        except StopIteration:
            continue
        if hdr is None:
            hdr = thdr
            yield hdr
        elif thdr != hdr:
            raise ArgumentError(
                'partial tables have mismatched headers: %r and %r'
                % (hdr, thdr)
            )
        for row in it:
            yield row


def _check_commutative(aggregators, ordered):
    if ordered:
        return
    for aggregator in aggregators:
        if not aggregator.commutative:
            raise ArgumentError(
                'aggregator %r is not commutative, pass ordered=True to '
                'merge states in input order' % (aggregator,)
            )


class MergeAggregateView(Table):

    def __init__(self, partials, key, aggregation, ordered=False,
                 presorted=False, buffersize=None, tempdir=None,
                 cache=True, field='value'):
        _check_commutative([aggregation], ordered)
        self.tables = _as_table_list(partials)
        self.key = key
        self.aggregation = aggregation
        self.field = field
        self.presorted = presorted or ordered or key is None
        self._buffersize = buffersize
        self._tempdir = tempdir
        self._cache = cache

    def __iter__(self):
        source = _iterchaintables(self.tables)
        if not self.presorted:
            source = sort(source, self.key, buffersize=self._buffersize,
                          tempdir=self._tempdir, cache=self._cache)
        return itersimplemerge(source, self.key, self.aggregation,
                               self.field)


def itersimplemerge(table, key, aggregator, field):

    # special case where length of key is 1
    if isinstance(key, (list, tuple)) and len(key) == 1:
        key = key[0]

    it = iter(table)
    hdr = next(it, None)
    if hdr is not None and len(hdr) == 0:
        hdr = None  # sorting an empty table yields an empty header
    if hdr is not None:
        # push back header
        it = itertools.chain([hdr], it)

    # determine output header
    if isinstance(key, (list, tuple)):
        outhdr = tuple(key) + (field,)
    elif callable(key):
        outhdr = ('key', field)
    elif key is None:
        outhdr = field,
    else:
        outhdr = (key, field)
    yield outhdr

    if hdr is None:
        # no partial data at all
        if key is None:
            yield aggregator.finalize(aggregator.initial()),
        return

    # states are expected in the last column
    stateidx = len(hdr) - 1

    if key is None:
        state = aggregator.initial()
        datait = iter(it)
        next(datait, None)  # skip header
        for row in datait:
            state = aggregator.merge(state, row[stateidx])
        yield aggregator.finalize(state),
    else:
        for k, grp in rowgroupby(it, key, stateidx):
            state = aggregator.initial()
            for other in grp:
                state = aggregator.merge(state, other)
            if isinstance(key, (list, tuple)):
                yield tuple(k) + (aggregator.finalize(state),)
            else:
                yield k, aggregator.finalize(state)


class MultiMergeAggregateView(Table):

    def __init__(self, partials, key, aggregation, ordered=False,
                 presorted=False, buffersize=None, tempdir=None,
                 cache=True):
        self.tables = _as_table_list(partials)
        self.key = key
        if isinstance(aggregation, dict):
            items = list(aggregation.items())
        else:
            items = [(t[0], t[1:]) for t in aggregation]
        self.aggregation = OrderedDict()
        for outfld, spec in items:
            srcfld, aggregator = _normalise_stateful_spec(spec)
            # source fields are only used when computing partials, at
            # merge time states are located by output field name
            self.aggregation[outfld] = aggregator
        _check_commutative(self.aggregation.values(), ordered)
        self.presorted = presorted or ordered or key is None
        self._buffersize = buffersize
        self._tempdir = tempdir
        self._cache = cache

    def __iter__(self):
        source = _iterchaintables(self.tables)
        if not self.presorted:
            source = sort(source, self.key, buffersize=self._buffersize,
                          tempdir=self._tempdir, cache=self._cache)
        return itermultimerge(source, self.key, self.aggregation)


def itermultimerge(table, key, aggregation):

    # special case where length of key is 1
    if isinstance(key, (list, tuple)) and len(key) == 1:
        key = key[0]

    it = iter(table)
    hdr = next(it, None)
    if hdr is not None and len(hdr) == 0:
        hdr = None  # sorting an empty table yields an empty header
    if hdr is not None:
        # push back header
        it = itertools.chain([hdr], it)

    outflds = list(aggregation.keys())
    aggregators = list(aggregation.values())

    # determine output header
    if isinstance(key, (list, tuple)):
        outhdr = list(key)
    elif callable(key):
        outhdr = ['key']
    elif key is None:
        outhdr = []
    else:
        outhdr = [key]
    outhdr += outflds
    yield tuple(outhdr)

    if hdr is None:
        # no partial data at all
        if key is None:
            yield tuple(a.finalize(a.initial()) for a in aggregators)
        return

    stateidxs = [list(hdr).index(outfld) for outfld in outflds]

    if key is None:
        states = [a.initial() for a in aggregators]
        datait = iter(it)
        next(datait, None)  # skip header
        for row in datait:
            for i, idx in enumerate(stateidxs):
                states[i] = aggregators[i].merge(states[i], row[idx])
        yield tuple(a.finalize(s) for a, s in zip(aggregators, states))
    else:
        for k, rows in rowgroupby(it, key):
            states = [a.initial() for a in aggregators]
            for row in rows:
                for i, idx in enumerate(stateidxs):
                    states[i] = aggregators[i].merge(states[i], row[idx])
            if isinstance(key, (list, tuple)):
                outrow = list(k)
            else:
                outrow = [k]
            outrow += [a.finalize(s) for a, s in zip(aggregators, states)]
            yield tuple(outrow)


def groupcountdistinctvalues(table, key, value):
    """Group by the `key` field then count the number of distinct values in the
    `value` field."""
    
    s1 = cut(table, key, value)
    s2 = distinct(s1)
    s3 = aggregate(s2, key, len)
    return s3


Table.groupcountdistinctvalues = groupcountdistinctvalues


def groupselectfirst(table, key, presorted=False, buffersize=None,
                     tempdir=None, cache=True):
    """Group by the `key` field then return the first row within each group.
    E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['A', 1, True],
        ...           ['C', 7, False],
        ...           ['B', 2, False],
        ...           ['C', 9, True]]
        >>> table2 = etl.groupselectfirst(table1, key='foo')
        >>> table2
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'A' |   1 | True  |
        +-----+-----+-------+
        | 'B' |   2 | False |
        +-----+-----+-------+
        | 'C' |   7 | False |
        +-----+-----+-------+

    See also :func:`petl.transform.reductions.groupselectlast`,
    :func:`petl.transform.dedup.distinct`.

    """

    def _reducer(k, rows):
        return next(rows)

    return rowreduce(table, key, reducer=_reducer, presorted=presorted,
                     buffersize=buffersize, tempdir=tempdir, cache=cache)


Table.groupselectfirst = groupselectfirst


def groupselectlast(table, key, presorted=False, buffersize=None,
                    tempdir=None, cache=True):
    """Group by the `key` field then return the last row within each group.
    E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['A', 1, True],
        ...           ['C', 7, False],
        ...           ['B', 2, False],
        ...           ['C', 9, True]]
        >>> table2 = etl.groupselectlast(table1, key='foo')
        >>> table2
        +-----+-----+-------+
        | foo | bar | baz   |
        +=====+=====+=======+
        | 'A' |   1 | True  |
        +-----+-----+-------+
        | 'B' |   2 | False |
        +-----+-----+-------+
        | 'C' |   9 | True  |
        +-----+-----+-------+

    See also :func:`petl.transform.reductions.groupselectfirst`,
    :func:`petl.transform.dedup.distinct`.

    .. versionadded:: 1.1.0

    """

    def _reducer(k, rows):
        row = None
        for row in rows:
            pass
        return row

    return rowreduce(table, key, reducer=_reducer, presorted=presorted,
                     buffersize=buffersize, tempdir=tempdir, cache=cache)


Table.groupselectlast = groupselectlast


def groupselectmin(table, key, value, presorted=False, buffersize=None,
                   tempdir=None, cache=True):
    """Group by the `key` field then return the row with the minimum of the
    `value` field within each group. N.B., will only return one row for each
    group, even if multiple rows have the same (minimum) value."""

    return groupselectfirst(sort(table, value, reverse=False), key,
                            presorted=presorted, buffersize=buffersize,
                            tempdir=tempdir, cache=cache)


Table.groupselectmin = groupselectmin

    
def groupselectmax(table, key, value, presorted=False, buffersize=None,
                   tempdir=None, cache=True):
    """Group by the `key` field then return the row with the maximum of the
    `value` field within each group. N.B., will only return one row for each
    group, even if multiple rows have the same (maximum) value."""

    return groupselectfirst(sort(table, value, reverse=True), key,
                            presorted=presorted, buffersize=buffersize,
                            tempdir=tempdir, cache=cache)


Table.groupselectmax = groupselectmax


def mergeduplicates(table, key, missing=None, presorted=False, buffersize=None,
                    tempdir=None, cache=True):
    """
    Merge duplicate rows under the given key. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['A', 1, 2.7],
        ...           ['B', 2, None],
        ...           ['D', 3, 9.4],
        ...           ['B', None, 7.8],
        ...           ['E', None, 42.],
        ...           ['D', 3, 12.3],
        ...           ['A', 2, None]]
        >>> table2 = etl.mergeduplicates(table1, 'foo')
        >>> table2
        +-----+------------------+-----------------------+
        | foo | bar              | baz                   |
        +=====+==================+=======================+
        | 'A' | Conflict({1, 2}) |                   2.7 |
        +-----+------------------+-----------------------+
        | 'B' |                2 |                   7.8 |
        +-----+------------------+-----------------------+
        | 'D' |                3 | Conflict({9.4, 12.3}) |
        +-----+------------------+-----------------------+
        | 'E' | None             |                  42.0 |
        +-----+------------------+-----------------------+

    Missing values are overridden by non-missing values. Conflicting values are
    reported as an instance of the Conflict class (sub-class of frozenset).

    If `presorted` is True, it is assumed that the data are already sorted by
    the given key, and the `buffersize`, `tempdir` and `cache` arguments are
    ignored. Otherwise, the data are sorted, see also the discussion of the
    `buffersize`, `tempdir` and `cache` arguments under the
    :func:`petl.transform.sorts.sort` function.

    See also :func:`petl.transform.dedup.conflicts`.

    """

    return MergeDuplicatesView(table, key, missing=missing, presorted=presorted,
                               buffersize=buffersize, tempdir=tempdir,
                               cache=cache)


Table.mergeduplicates = mergeduplicates


class MergeDuplicatesView(Table):

    def __init__(self, table, key, missing=None, presorted=False,
                 buffersize=None, tempdir=None, cache=True):
        self.table = table
        self.key = key
        self.missing = missing
        self.presorted = presorted
        self.buffersize = buffersize
        self.tempdir = tempdir
        self._cache = cache
        self._sorted = None
        self._sortkey = None

    def __iter__(self):
        table = self.table
        if not self.presorted:
            key = self.key
            if any(isinstance(k, int) and k < 0 for k in _keyspec(key)):
                # sort by the same field indices that rows are grouped by, so
                # a negative index selects the same field in rows shorter or
                # longer than the header; the header can change between
                # uncached iterations, so resolve the index every time
                # against the header of the sorted table (the cached one,
                # if the sort is cached)
                sortedtable = table if self._sorted is None else self._sorted
                key = _mergekeyindices(next(iter(sortedtable), ()), key)
            if self._sorted is None or key != self._sortkey:
                self._sorted = sort(table, key, buffersize=self.buffersize,
                                    tempdir=self.tempdir, cache=self._cache)
                self._sortkey = key
            table = self._sorted
        return itermergeduplicates(table, self.key, self.missing)


def _keyspec(key):
    return key if isinstance(key, (list, tuple)) else (key,)


def _mergekeyindices(hdr, key):
    # resolve the key to non-negative field indices, so the key can also be
    # given by field index or as a list with a single field
    keyidxs = []
    for i in asindices(hdr, key):
        if i < -len(hdr):
            raise FieldSelectionError(i)
        keyidxs.append(i % len(hdr))
    return keyidxs


def itermergeduplicates(table, key, missing):
    it = iter(table)
    hdr, it = iterpeek(it)
    flds = list(map(text_type, hdr))
    keyidxs = _mergekeyindices(hdr, key)

    # determine output fields
    outhdr = [flds[i] for i in keyidxs]
    valfldidxs = [i for i in range(len(flds)) if i not in keyidxs]
    outhdr.extend(flds[i] for i in valfldidxs)
    yield tuple(outhdr)

    # do the work
    for k, grp in rowgroupby(it, keyidxs):
        grp = list(grp)
        if len(keyidxs) == 1:
            outrow = [k]
        else:
            outrow = list(k)
        mergedvals = [set(row[i] for row in grp
                          if len(row) > i and row[i] != missing)
                      for i in valfldidxs]
        normedvals = [vals.pop() if len(vals) == 1
                      else missing if len(vals) == 0
                      else Conflict(vals)
                      for vals in mergedvals]
        outrow.extend(normedvals)
        yield tuple(outrow)


def merge(*tables, **kwargs):
    """
    Convenience function to combine multiple tables (via
    :func:`petl.transform.sorts.mergesort`) then combine duplicate rows by
    merging under the given key (via
    :func:`petl.transform.reductions.mergeduplicates`). E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           [1, 'A', True],
        ...           [2, 'B', None],
        ...           [4, 'C', True]]
        >>> table2 = [['bar', 'baz', 'quux'],
        ...           ['A', True, 42.0],
        ...           ['B', False, 79.3],
        ...           ['C', False, 12.4]]
        >>> table3 = etl.merge(table1, table2, key='bar')
        >>> table3
        +-----+-----+-------------------------+------+
        | bar | foo | baz                     | quux |
        +=====+=====+=========================+======+
        | 'A' |   1 | True                    | 42.0 |
        +-----+-----+-------------------------+------+
        | 'B' |   2 | False                   | 79.3 |
        +-----+-----+-------------------------+------+
        | 'C' |   4 | Conflict({False, True}) | 12.4 |
        +-----+-----+-------------------------+------+

    Keyword arguments are the same as for
    :func:`petl.transform.sorts.mergesort`, except `key` is required.

    """

    assert 'key' in kwargs, 'keyword argument "key" is required'
    key = kwargs['key']
    if any(isinstance(k, int) for k in _keyspec(key)):
        # mergesort() would sort each table by the index in its own header,
        # so select the key fields by name from the combined header instead
        hdr = next(iter(mergesort(*tables, presorted=True,
                                  header=kwargs.get('header'))))
        key = kwargs['key'] = [hdr[i] for i in _mergekeyindices(hdr, key)]
    t1 = mergesort(*tables, **kwargs)
    t2 = mergeduplicates(t1, key=key, presorted=True)
    return t2


Table.merge = merge


class Conflict(frozenset):

    def __new__(cls, items):
        s = super(Conflict, cls).__new__(cls, items)
        return s


def fold(table, key, f, value=None, presorted=False, buffersize=None,
         tempdir=None, cache=True):
    """
    Reduce rows recursively via the Python standard :func:`reduce` function.
    E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'count'],
        ...           [1, 3],
        ...           [1, 5],
        ...           [2, 4],
        ...           [2, 8]]
        >>> import operator
        >>> table2 = etl.fold(table1, 'id', operator.add, 'count',
        ...                   presorted=True)
        >>> table2
        +-----+-------+
        | key | value |
        +=====+=======+
        |   1 |     8 |
        +-----+-------+
        |   2 |    12 |
        +-----+-------+

    See also :func:`petl.transform.reductions.aggregate`,
    :func:`petl.transform.reductions.rowreduce`.

    """

    return FoldView(table, key, f, value=value, presorted=presorted,
                    buffersize=buffersize, tempdir=tempdir, cache=cache)


Table.fold = fold


class FoldView(Table):

    def __init__(self, table, key, f, value=None, presorted=False,
                 buffersize=None, tempdir=None, cache=True):
        if presorted:
            self.table = table
        else:
            self.table = sort(table, key, buffersize=buffersize,
                              tempdir=tempdir, cache=cache)
        self.key = key
        self.f = f
        self.value = value

    def __iter__(self):
        return iterfold(self.table, self.key, self.f, self.value)


def iterfold(table, key, f, value):
    yield ('key', 'value')
    for k, grp in rowgroupby(table, key, value):
        yield k, reduce(f, grp)
