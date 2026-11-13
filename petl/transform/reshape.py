from __future__ import absolute_import, print_function, division


import itertools
import collections
import operator
from petl.compat import binary_type, next, string_types, text_type


from petl.errors import ArgumentError, RecastConflictError, \
    RecastUnknownCategoryError
from petl.comparison import Comparable, comparable_itemgetter
from petl.util.base import Table, rowgetter, values, itervalues, \
    header, data, asindices
from petl.transform.sorts import sort


def melt(table, key=None, variables=None, variablefield='variable',
         valuefield='value'):
    """
    Reshape a table, melting fields into data. E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'gender', 'age'],
        ...           [1, 'F', 12],
        ...           [2, 'M', 17],
        ...           [3, 'M', 16]]
        >>> table2 = etl.melt(table1, 'id')
        >>> table2.lookall()
        +----+----------+-------+
        | id | variable | value |
        +====+==========+=======+
        |  1 | 'gender' | 'F'   |
        +----+----------+-------+
        |  1 | 'age'    |    12 |
        +----+----------+-------+
        |  2 | 'gender' | 'M'   |
        +----+----------+-------+
        |  2 | 'age'    |    17 |
        +----+----------+-------+
        |  3 | 'gender' | 'M'   |
        +----+----------+-------+
        |  3 | 'age'    |    16 |
        +----+----------+-------+

        >>> # compound keys are supported
        ... table3 = [['id', 'time', 'height', 'weight'],
        ...           [1, 11, 66.4, 12.2],
        ...           [2, 16, 53.2, 17.3],
        ...           [3, 12, 34.5, 9.4]]
        >>> table4 = etl.melt(table3, key=['id', 'time'])
        >>> table4.lookall()
        +----+------+----------+-------+
        | id | time | variable | value |
        +====+======+==========+=======+
        |  1 |   11 | 'height' |  66.4 |
        +----+------+----------+-------+
        |  1 |   11 | 'weight' |  12.2 |
        +----+------+----------+-------+
        |  2 |   16 | 'height' |  53.2 |
        +----+------+----------+-------+
        |  2 |   16 | 'weight' |  17.3 |
        +----+------+----------+-------+
        |  3 |   12 | 'height' |  34.5 |
        +----+------+----------+-------+
        |  3 |   12 | 'weight' |   9.4 |
        +----+------+----------+-------+

        >>> # a subset of variable fields can be selected
        ... table5 = etl.melt(table3, key=['id', 'time'],
        ...                   variables=['height'])
        >>> table5.lookall()
        +----+------+----------+-------+
        | id | time | variable | value |
        +====+======+==========+=======+
        |  1 |   11 | 'height' |  66.4 |
        +----+------+----------+-------+
        |  2 |   16 | 'height' |  53.2 |
        +----+------+----------+-------+
        |  3 |   12 | 'height' |  34.5 |
        +----+------+----------+-------+

    See also :func:`petl.transform.reshape.recast`.

    """

    return MeltView(table, key=key, variables=variables,
                    variablefield=variablefield,
                    valuefield=valuefield)


Table.melt = melt


class MeltView(Table):

    def __init__(self, source, key=None, variables=None,
                 variablefield='variable', valuefield='value'):
        self.source = source
        self.key = key
        self.variables = variables
        self.variablefield = variablefield
        self.valuefield = valuefield

    def __iter__(self):
        return itermelt(self.source, self.key, self.variables,
                        self.variablefield, self.valuefield)


def itermelt(source, key, variables, variablefield, valuefield):
    if key is None and variables is None:
        raise ValueError('either key or variables must be specified')

    it = iter(source)
    try:
        hdr = next(it)
    except StopIteration:
        return

    # determine key and variable field indices
    key_indices = variables_indices = None
    if key is not None:
        key_indices = asindices(hdr, key)
    if variables is not None:
        if not isinstance(variables, (list, tuple)):
            variables = (variables,)
        variables_indices = asindices(hdr, variables)

    if key is None:
        # assume key is fields not in variables
        selected = {i + len(hdr) if i < 0 else i for i in variables_indices}
        key_indices = [i for i in range(len(hdr))
                       if i not in selected]
    if variables is None:
        # assume variables are fields not in key
        selected = {i + len(hdr) if i < 0 else i for i in key_indices}
        variables_indices = [i for i in range(len(hdr))
                             if i not in selected]
        variables = [hdr[i] for i in variables_indices]

    getkey = rowgetter(*key_indices)

    # determine the output fields
    outhdr = [hdr[i] for i in key_indices]
    outhdr.append(variablefield)
    outhdr.append(valuefield)
    yield tuple(outhdr)

    # construct the output data
    for row in it:
        k = getkey(row)
        for v, i in zip(variables, variables_indices):
            try:
                o = list(k)  # populate with key values initially
                o.append(v)  # add variable
                o.append(row[i])  # add value
                yield tuple(o)
            except IndexError:
                # row is missing this value, and melt() should yield no row
                pass


def _take_recast_alias(kwargs, *names):
    for name in names:
        if name in kwargs:
            return kwargs.pop(name)
    return None


def _normalise_recast_unknown(unknown, overflowfield, explicit):
    if overflowfield is False:
        overflowfield = None
    elif overflowfield is True:
        overflowfield = 'overflow'
    if unknown is None:
        if overflowfield is not None:
            unknown = 'overflow'
        elif explicit:
            unknown = 'error'
        else:
            unknown = 'ignore'
    elif not isinstance(unknown, string_types):
        raise ArgumentError('invalid unknown category mode: %r' % (unknown,))

    mode = unknown.lower()
    if mode in ('error', 'raise', 'strict', 'fail'):
        mode = 'error'
    elif mode in ('overflow', 'column', 'collect'):
        mode = 'overflow'
    elif mode in ('ignore', 'skip', 'drop'):
        mode = 'ignore'
    else:
        raise ArgumentError('invalid unknown category mode: %s' % unknown)

    if mode == 'overflow' and overflowfield is None:
        overflowfield = 'overflow'
    if not mode == 'overflow':
        overflowfield = None
    return mode, overflowfield


def _normalise_recast_conflicts(conflicts):
    if conflicts is None:
        return False
    if isinstance(conflicts, string_types):
        return conflicts.lower() in ('error', 'raise', 'strict', 'fail')
    return bool(conflicts)


def _category_position(categories, category):
    for i, value in enumerate(categories):
        if value == category:
            return i
    return None


def _reducer_for(reducers, category):
    try:
        return reducers[category]
    except (KeyError, TypeError):
        for key, reducer in reducers.items():
            if key == category:
                return reducer
    return None


def _resolve_recast_values(values, category, reducers):
    if len(values) == 0:
        return None
    if len(values) == 1:
        return values[0]
    reducer = _reducer_for(reducers, category)
    if reducer is not None:
        return reducer(values)
    return list(values)


def _find_unknown_group(groups, category):
    for group in groups:
        if group[0] == category:
            return group
    return None


def _row_numbers(rows):
    return sorted(set(row[0] for row in rows))


def _raise_recast_conflict(key_value, category, rows):
    row_numbers = _row_numbers(rows)
    data_rows = [i - 1 for i in row_numbers]
    message = ('conflicting duplicate values for key %r, category %r '
               'at original source row%s %s (data row%s %s)'
               % (key_value, category,
                  '' if len(row_numbers) == 1 else 's',
                  ', '.join(str(i) for i in row_numbers),
                  '' if len(data_rows) == 1 else 's',
                  ', '.join(str(i) for i in data_rows)))
    error = RecastConflictError(message)
    error.key = key_value
    error.category = category
    error.rows = row_numbers
    error.row = row_numbers[0]
    error.row_numbers = row_numbers
    error.source_row_numbers = row_numbers
    error.source_rows = row_numbers
    error.data_rows = data_rows
    raise error


def _raise_recast_unknown(key_value, category, rows):
    row_numbers = _row_numbers(rows)
    data_rows = [i - 1 for i in row_numbers]
    message = ('unknown category %r for key %r at original source row %s '
               '(data row %s)'
               % (category, key_value, row_numbers[0], data_rows[0]))
    error = RecastUnknownCategoryError(message)
    error.key = key_value
    error.category = category
    error.rows = row_numbers
    error.row = row_numbers[0]
    error.row_numbers = row_numbers
    error.source_row_numbers = row_numbers
    error.source_rows = row_numbers
    error.data_rows = data_rows
    raise error


def recast(table, key=None, variablefield='variable', valuefield='value',
           samplesize=1000, reducers=None, missing=None, categories=None,
           unknown=None, overflowfield=None, conflicts=None, **kwargs):
    """
    Recast molten data. E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'variable', 'value'],
        ...           [3, 'age', 16],
        ...           [1, 'gender', 'F'],
        ...           [2, 'gender', 'M'],
        ...           [2, 'age', 17],
        ...           [1, 'age', 12],
        ...           [3, 'gender', 'M']]
        >>> table2 = etl.recast(table1)
        >>> table2
        +----+-----+--------+
        | id | age | gender |
        +====+=====+========+
        |  1 |  12 | 'F'    |
        +----+-----+--------+
        |  2 |  17 | 'M'    |
        +----+-----+--------+
        |  3 |  16 | 'M'    |
        +----+-----+--------+

        >>> # specifying variable and value fields
        ... table3 = [['id', 'vars', 'vals'],
        ...           [3, 'age', 16],
        ...           [1, 'gender', 'F'],
        ...           [2, 'gender', 'M'],
        ...           [2, 'age', 17],
        ...           [1, 'age', 12],
        ...           [3, 'gender', 'M']]
        >>> table4 = etl.recast(table3, variablefield='vars', valuefield='vals')
        >>> table4
        +----+-----+--------+
        | id | age | gender |
        +====+=====+========+
        |  1 |  12 | 'F'    |
        +----+-----+--------+
        |  2 |  17 | 'M'    |
        +----+-----+--------+
        |  3 |  16 | 'M'    |
        +----+-----+--------+

        >>> # if there are multiple values for each key/variable pair, and no
        ... # reducers function is provided, then all values will be listed
        ... table6 = [['id', 'time', 'variable', 'value'],
        ...           [1, 11, 'weight', 66.4],
        ...           [1, 14, 'weight', 55.2],
        ...           [2, 12, 'weight', 53.2],
        ...           [2, 16, 'weight', 43.3],
        ...           [3, 12, 'weight', 34.5],
        ...           [3, 17, 'weight', 49.4]]
        >>> table7 = etl.recast(table6, key='id')
        >>> table7
        +----+--------------+
        | id | weight       |
        +====+==============+
        |  1 | [66.4, 55.2] |
        +----+--------------+
        |  2 | [53.2, 43.3] |
        +----+--------------+
        |  3 | [34.5, 49.4] |
        +----+--------------+

        >>> # multiple values can be reduced via an aggregation function
        ... def mean(values):
        ...     return float(sum(values)) / len(values)
        ...
        >>> table8 = etl.recast(table6, key='id', reducers={'weight': mean})
        >>> table8
        +----+--------------------+
        | id | weight             |
        +====+====================+
        |  1 | 60.800000000000004 |
        +----+--------------------+
        |  2 |              48.25 |
        +----+--------------------+
        |  3 |              41.95 |
        +----+--------------------+

        >>> # missing values are padded with whatever is provided via the
        ... # missing keyword argument (None by default)
        ... table9 = [['id', 'variable', 'value'],
        ...           [1, 'gender', 'F'],
        ...           [2, 'age', 17],
        ...           [1, 'age', 12],
        ...           [3, 'gender', 'M']]
        >>> table10 = etl.recast(table9, key='id')
        >>> table10
        +----+------+--------+
        | id | age  | gender |
        +====+======+========+
        |  1 |   12 | 'F'    |
        +----+------+--------+
        |  2 |   17 | None   |
        +----+------+--------+
        |  3 | None | 'M'    |
        +----+------+--------+

    Fixed category domains can be supplied via `categories`, which makes the
    output columns independent of `samplesize`. The order of the supplied
    categories is retained. For a single variable field, pass a sequence of
    categories. For multiple variable fields, pass a mapping from variable
    field name to a sequence of categories. The legacy
    `variablefield={field: categories}` form remains supported.

    Unknown categories raise :class:`petl.errors.RecastUnknownCategoryError`
    by default when a fixed domain is supplied. Set
    `overflowfield='name'` (or `unknown='overflow'`) to collect them in an
    additional output field instead. Each overflow cell is a list of
    ``(category, value)`` pairs, so categories remain associated with their
    values. A value of ``None`` is an ordinary category when it is present in
    the fixed domain; it is distinct from a missing category, which is filled
    with `missing`.

    Duplicate key/category combinations retain the existing reducer/list
    behaviour. Set `conflicts=True` (or `conflicts='error'`) to reject them
    instead. Conflict and unknown-category errors report the original source
    row number, even though recast sorts rows internally.

    Note that the table is scanned once to discover variables, then a second
    time to reshape the data and recast variables as fields. How many rows are
    scanned in the first pass is determined by the `samplesize` argument.
    Discovered variable names are sorted using PETL's mixed-type ordering,
    so names such as integers and strings can be recast together. Names
    explicitly supplied in a `variablefield` dictionary retain their order.

    When the result is iterated, invalid key, variable or value field names
    raise :class:`petl.errors.ArgumentError`. The value field must exist in the
    input header and must not also be selected as a key or variable field.
    This also applies to an empty table with no fields; define the expected
    header before recasting it.

    The sorted rows and discovered fields are cached after the first data
    read. Later iterations use this cache, as with :func:`sort`. Create a new
    recast view to read the source again. Cached rows are shallow tuple
    copies, so changing fields in a source row does not change the cache.
    Mutable objects stored in cells are not copied by this step. Do not
    change these objects while using the view. Do not change source fields
    while iterators are active.

    See also :func:`petl.transform.reshape.melt`.

    """

    category_alias = _take_recast_alias(
        kwargs, 'category', 'categorydomain', 'category_domain',
        'categorylist', 'category_list', 'domain', 'domaincategories',
        'domain_categories',
        'variablecategories', 'variable_categories', 'pivotcategories',
        'pivot_categories')
    if categories is None and category_alias is not None:
        categories = category_alias

    unknown_alias = _take_recast_alias(
        kwargs, 'onunknown', 'on_unknown', 'unknownmode', 'unknown_mode',
        'unknowncategories', 'unknown_categories', 'unknown_category_mode',
        'raiseonunknown', 'raise_on_unknown', 'failonunknown',
        'fail_on_unknown', 'unknownerror', 'unknown_error',
        'erroronunknown', 'error_on_unknown')
    if unknown is None and unknown_alias is not None:
        if unknown_alias is True:
            unknown = 'error'
        elif unknown_alias is False:
            unknown = 'ignore'
        else:
            unknown = unknown_alias

    overflow_alias = _take_recast_alias(
        kwargs, 'overflow', 'overflow_field', 'unknownfield',
        'unknown_field', 'overflowcolumn', 'overflow_column',
        'unknowncolumn', 'unknown_column')
    if overflowfield is None and overflow_alias is not None:
        if (isinstance(overflow_alias, string_types)
                and overflow_alias.lower() in (
                'error', 'raise', 'strict', 'fail', 'overflow', 'column',
                'collect', 'ignore', 'skip', 'drop')):
            if unknown is None:
                unknown = overflow_alias
        elif isinstance(overflow_alias, bool):
            if overflow_alias:
                overflowfield = 'overflow'
        else:
            overflowfield = overflow_alias

    conflicts_alias = _take_recast_alias(
        kwargs, 'conflict', 'conflicts_mode', 'onconflict', 'on_conflict',
        'duplicate', 'duplicates', 'duplicateconflict',
        'duplicate_conflict', 'raiseconflicts',
        'raise_conflicts', 'raiseonconflict', 'raise_on_conflict',
        'conflicterror', 'conflict_error')
    if conflicts is None and conflicts_alias is not None:
        conflicts = conflicts_alias

    if kwargs:
        name = next(iter(kwargs))
        raise TypeError(
            "recast() got an unexpected keyword argument %r" % name)

    unknown, overflowfield = _normalise_recast_unknown(
        unknown, overflowfield, categories is not None)
    conflicts = _normalise_recast_conflicts(conflicts)

    return RecastView(table, key=key, variablefield=variablefield,
                      valuefield=valuefield, samplesize=samplesize,
                      reducers=reducers, missing=missing,
                      categories=categories, unknown=unknown,
                      overflowfield=overflowfield, conflicts=conflicts)


Table.recast = recast


class RecastView(Table):

    def __init__(self, source, key=None, variablefield='variable',
                 valuefield='value', samplesize=1000, reducers=None,
                 missing=None, categories=None, unknown=None,
                 overflowfield=None, conflicts=False):
        self.source = source
        self.key = key
        self.variablefield = variablefield
        self.valuefield = valuefield
        self.samplesize = samplesize
        if reducers is None:
            self.reducers = dict()
        else:
            self.reducers = reducers
        self.missing = missing
        self.categories = categories
        unknown, overflowfield = _normalise_recast_unknown(
            unknown, overflowfield, categories is not None)
        self.unknown = unknown
        self.overflowfield = overflowfield
        self.conflicts = _normalise_recast_conflicts(conflicts)
        self._cache = []

    def __iter__(self):
        return iterrecast(self.source, self.key, self.variablefield,
                          self.valuefield, self.samplesize, self.reducers,
                          self.missing, cache=self._cache,
                          categories=self.categories, unknown=self.unknown,
                          overflowfield=self.overflowfield,
                          conflicts=self.conflicts)


class _RecastSource(object):

    def __init__(self, source):
        self.source = source

    def __iter__(self):
        it = iter(self.source)
        try:
            hdr = next(it)
        except StopIteration:
            return
        # The original row number is carried alongside each row so errors keep
        # pointing at the input after the internal sort.
        yield (None,) + tuple(hdr)
        for rownumber, row in enumerate(it, start=2):
            yield (rownumber,) + tuple(row)


def _prepare_recast(source, key, variablefield, valuefield, samplesize,
                    categories=None, unknown='ignore', overflowfield=None,
                    conflicts=False):
    """Find the output fields and prepare a lazy sort."""

    it = iter(source)
    try:
        hdr = next(it)
    except StopIteration:
        return
    flds = list(map(text_type, hdr))

    # normalise some stuff
    keyfields = key
    variablefields = variablefield  # N.B., could be more than one

    # A mapping of fixed categories can supply the variable field names when
    # the caller did not pass variablefield explicitly.
    if (isinstance(categories, dict)
            and variablefields == 'variable'
            and 'variable' not in flds):
        category_fields = [f for f in categories if f in flds]
        if category_fields:
            variablefields = tuple(category_fields)

    # normalise key fields
    if keyfields and not isinstance(keyfields, (list, tuple)):
        keyfields = (keyfields,)

    # normalise variable fields
    if variablefields:
        if isinstance(variablefields, dict):
            pass  # handle this later
        elif not isinstance(variablefields, (list, tuple)):
            variablefields = (variablefields,)

    # infer key fields
    if not keyfields:
        # assume keyfields is fields not in variables
        keyfields = [f for f in flds
                     if f not in variablefields and f != valuefield]

    # infer key fields
    if not variablefields:
        # assume variables are fields not in keyfields
        variablefields = [f for f in flds
                          if f not in keyfields and f != valuefield]

    # Validate user-supplied fields even when assertions are disabled.
    if valuefield not in flds:
        raise ArgumentError('invalid value field: %s' % valuefield)
    if valuefield in keyfields:
        raise ArgumentError('value field cannot be a key field')
    if valuefield in variablefields:
        raise ArgumentError('value field cannot be a variable field')
    for f in keyfields:
        if f not in flds:
            raise ArgumentError('invalid key field: %s' % f)
    for f in variablefields:
        if f not in flds:
            raise ArgumentError('invalid variable field: %s' % f)

    # we'll need these later
    valueindex = flds.index(valuefield)
    keyindices = [flds.index(f) for f in keyfields]
    variableindices = [flds.index(f) for f in variablefields]

    # determine the actual variable names to be cast as fields
    if categories is not None:
        if isinstance(categories, dict):
            supplied = {}
            for f in variablefields:
                if f in categories:
                    supplied[f] = categories[f]
                elif len(variablefields) == 1 and len(categories) == 1:
                    supplied[f] = next(iter(categories.values()))
                else:
                    raise ArgumentError(
                        'missing categories for variable field: %s' % f)
        else:
            if len(variablefields) != 1:
                raise ArgumentError(
                    'categories must be a mapping when recasting multiple '
                    'variable fields')
            supplied = {variablefields[0]: categories}

        variables = []
        for f in variablefields:
            field_categories = supplied[f]
            if isinstance(field_categories, (string_types, binary_type)):
                field_categories = (field_categories,)
            try:
                field_categories = tuple(field_categories)
            except TypeError:
                field_categories = (field_categories,)
            unique = []
            for category in field_categories:
                if _category_position(unique, category) is not None:
                    raise ArgumentError(
                        'duplicate category for variable field %s: %r'
                        % (f, category))
                unique.append(category)
            variables.append(tuple(unique))
    elif isinstance(variablefields, dict):
        # user supplied dictionary
        variables = [tuple(variablefields[f]) for f in variablefields]
    else:
        variables = collections.defaultdict(set)
        # sample the data to discover variables to be cast as fields
        for row in itertools.islice(it, 0, samplesize):
            for i, f in zip(variableindices, variablefields):
                variables[f].add(row[i])
        for f in variables:
            # turn from sets to sorted lists
            variables[f] = sorted(variables[f], key=Comparable)
        # Keep the sample order when the sorted rows are read again.
        variables = [tuple(variables[f]) for f in variablefields]

    outhdr = tuple(keyfields) + tuple(itertools.chain.from_iterable(variables))
    if overflowfield is not None:
        if _category_position(outhdr, overflowfield) is not None:
            raise ArgumentError(
                'overflow field conflicts with output field: %s'
                % overflowfield)
        outhdr = outhdr + (overflowfield,)

    # The wrapped source carries the original row number in field zero.
    wrapped_keyindices = [i + 1 for i in keyindices]
    wrapped_variableindices = [i + 1 for i in variableindices]
    wrapped_valueindex = valueindex + 1
    if wrapped_keyindices:
        sorted_source = sort(_RecastSource(source), key=wrapped_keyindices)
    else:
        sorted_source = _RecastSource(source)
    return (sorted_source, outhdr, wrapped_keyindices,
            wrapped_variableindices, variables, wrapped_valueindex,
            list(variablefields), unknown, overflowfield, conflicts)


def iterrecast(source, key, variablefield, valuefield,
               samplesize, reducers, missing, cache=None, categories=None,
               unknown='ignore', overflowfield=None, conflicts=False):
    if cache:
        prepared = cache[0]
    else:
        prepared = _prepare_recast(source, key, variablefield, valuefield,
                                  samplesize, categories, unknown,
                                  overflowfield, conflicts)
        if prepared is None:
            return
    yield prepared[1]

    # Another iterator can fill the cache after this header is read.
    if cache:
        if prepared[1:] != cache[0][1:]:
            raise ArgumentError('recast fields changed during iteration')
        prepared = cache[0]
    (source, outhdr, keyindices, variableindices, variables, valueindex,
     variablefields, unknown, overflowfield, conflicts) = prepared
    it = itertools.islice(source, 1, None)
    # A data read completes the sort. Do not retain a failed or unread sort.
    first = list(itertools.islice(it, 1))
    if cache is not None and not cache:
        cache.append(prepared)
    it = itertools.chain(first, it)
    if keyindices:
        getsortablekey = comparable_itemgetter(*keyindices)
        getactualkey = operator.itemgetter(*keyindices)
    else:
        getsortablekey = lambda row: None
        getactualkey = lambda row: ()

    # process sorted data in newfields
    groups = itertools.groupby(it, key=getsortablekey)
    for _, group in groups:
        # may need to iterate over the group more than once
        group = list(group)
        # N.B., key returned by groupby may be wrapped as SortableItem, we want
        # to output the actual key value, get it from the first row in the group
        key_value = getactualkey(group[0])
        if len(keyindices) > 1:
            out_row = list(key_value)
        elif len(keyindices) == 0:
            out_row = []
        else:
            out_row = [key_value]

        overflow_groups = []
        for i, field_name, field_variables in zip(
                variableindices, variablefields, variables):
            per_category = [[] for _ in field_variables]
            unknown_for_field = []
            for row in group:
                category = row[i]
                position = _category_position(field_variables, category)
                value = row[valueindex]
                if position is None:
                    if unknown == 'error':
                        unknown_for_field.append(
                            (category, [(row[0], value)]))
                    elif unknown == 'overflow':
                        group_entry = _find_unknown_group(
                            unknown_for_field, category)
                        if group_entry is None:
                            unknown_for_field.append(
                                (category, [(row[0], value)]))
                        else:
                            group_entry[1].append((row[0], value))
                else:
                    per_category[position].append((row[0], value))

            if unknown == 'error' and unknown_for_field:
                earliest = min(unknown_for_field,
                               key=lambda entry: min(
                                   row[0] for row in entry[1]))
                _raise_recast_unknown(key_value, earliest[0], earliest[1])

            if conflicts:
                for variable, rows in zip(field_variables, per_category):
                    if len(rows) > 1:
                        _raise_recast_conflict(key_value, variable, rows)
                for variable, rows in unknown_for_field:
                    if len(rows) > 1:
                        _raise_recast_conflict(key_value, variable, rows)

            for variable, rows in zip(field_variables, per_category):
                if not rows:
                    value = missing
                else:
                    value = _resolve_recast_values(
                        [row[1] for row in rows], variable, reducers)
                out_row.append(value)

            if unknown == 'overflow':
                for variable, rows in unknown_for_field:
                    rows = sorted(rows, key=operator.itemgetter(0))
                    value = _resolve_recast_values(
                        [row[1] for row in rows], variable, reducers)
                    if len(variables) == 1:
                        overflow_groups.append(
                            (variable, value, rows[0][0]))
                    else:
                        overflow_groups.append(
                            (field_name, variable, value, rows[0][0]))

        if overflowfield is not None:
            overflow_groups.sort(key=operator.itemgetter(-1))
            if len(variables) == 1:
                out_row.append(
                    [(category, value)
                     for category, value, _ in overflow_groups])
            else:
                out_row.append(
                    [(field_name, category, value)
                     for field_name, category, value, _ in overflow_groups])
        yield tuple(out_row)


def transpose(table):
    """
    Transpose rows into columns. E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'colour'],
        ...           [1, 'blue'],
        ...           [2, 'red'],
        ...           [3, 'purple'],
        ...           [5, 'yellow'],
        ...           [7, 'orange']]
        >>> table2 = etl.transpose(table1)
        >>> table2
        +----------+--------+-------+----------+----------+----------+
        | id       | 1      | 2     | 3        | 5        | 7        |
        +==========+========+=======+==========+==========+==========+
        | 'colour' | 'blue' | 'red' | 'purple' | 'yellow' | 'orange' |
        +----------+--------+-------+----------+----------+----------+

    See also :func:`petl.transform.reshape.recast`.

    """

    return TransposeView(table)


Table.transpose = transpose


class TransposeView(Table):

    def __init__(self, source):
        self.source = source

    def __iter__(self):
        return itertranspose(self.source)


def itertranspose(source):
    hdr = header(source)
    its = [iter(source) for _ in hdr]
    for i in range(len(hdr)):
        yield tuple(row[i] for row in its[i])


def pivot(table, f1, f2, f3, aggfun, missing=None,
          presorted=False, buffersize=None, tempdir=None, cache=True):
    """
    Construct a pivot table. E.g.::

        >>> import petl as etl
        >>> table1 = [['region', 'gender', 'style', 'units'],
        ...           ['east', 'boy', 'tee', 12],
        ...           ['east', 'boy', 'golf', 14],
        ...           ['east', 'boy', 'fancy', 7],
        ...           ['east', 'girl', 'tee', 3],
        ...           ['east', 'girl', 'golf', 8],
        ...           ['east', 'girl', 'fancy', 18],
        ...           ['west', 'boy', 'tee', 12],
        ...           ['west', 'boy', 'golf', 15],
        ...           ['west', 'boy', 'fancy', 8],
        ...           ['west', 'girl', 'tee', 6],
        ...           ['west', 'girl', 'golf', 16],
        ...           ['west', 'girl', 'fancy', 1]]
        >>> table2 = etl.pivot(table1, 'region', 'gender', 'units', sum)
        >>> table2
        +--------+-----+------+
        | region | boy | girl |
        +========+=====+======+
        | 'east' |  33 |   29 |
        +--------+-----+------+
        | 'west' |  35 |   23 |
        +--------+-----+------+

        >>> table3 = etl.pivot(table1, 'region', 'style', 'units', sum)
        >>> table3
        +--------+-------+------+-----+
        | region | fancy | golf | tee |
        +========+=======+======+=====+
        | 'east' |    25 |   22 |  15 |
        +--------+-------+------+-----+
        | 'west' |     9 |   31 |  18 |
        +--------+-------+------+-----+

        >>> table4 = etl.pivot(table1, 'gender', 'style', 'units', sum)
        >>> table4
        +--------+-------+------+-----+
        | gender | fancy | golf | tee |
        +========+=======+======+=====+
        | 'boy'  |    15 |   29 |  24 |
        +--------+-------+------+-----+
        | 'girl' |    19 |   24 |   9 |
        +--------+-------+------+-----+

    See also :func:`petl.transform.reshape.recast`.

    """

    return PivotView(table, f1, f2, f3, aggfun, missing=missing,
                     presorted=presorted, buffersize=buffersize,
                     tempdir=tempdir, cache=cache)


Table.pivot = pivot


class PivotView(Table):

    def __init__(self, source, f1, f2, f3, aggfun, missing=None,
                 presorted=False, buffersize=None, tempdir=None, cache=True):
        if presorted:
            self.source = source
        else:
            self.source = sort(source, key=(f1, f2), buffersize=buffersize,
                               tempdir=tempdir, cache=cache)
        self.f1, self.f2, self.f3 = f1, f2, f3
        self.aggfun = aggfun
        self.missing = missing

    def __iter__(self):
        return iterpivot(self.source, self.f1, self.f2, self.f3, self.aggfun,
                         self.missing)


def iterpivot(source, f1, f2, f3, aggfun, missing):

    # first pass - collect fields
    f2vals = set(itervalues(source, f2))  # TODO only make one pass
    f2vals = list(f2vals)
    f2vals.sort()
    outhdr = [f1]
    outhdr.extend(f2vals)
    yield tuple(outhdr)

    # second pass - generate output
    it = iter(source)
    try:
        hdr = next(it)
    except StopIteration:
        hdr = []
    flds = list(map(text_type, hdr))
    f1i = flds.index(f1)
    f2i = flds.index(f2)
    f3i = flds.index(f3)
    for v1, v1rows in itertools.groupby(it, key=operator.itemgetter(f1i)):
        outrow = [v1] + [missing] * len(f2vals)
        for v2, v12rows in itertools.groupby(v1rows,
                                             key=operator.itemgetter(f2i)):
            aggval = aggfun([row[f3i] for row in v12rows])
            outrow[1 + f2vals.index(v2)] = aggval
        yield tuple(outrow)


def flatten(table):
    """
    Convert a table to a sequence of values in row-major order. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['A', 1, True],
        ...           ['C', 7, False],
        ...           ['B', 2, False],
        ...           ['C', 9, True]]
        >>> list(etl.flatten(table1))
        ['A', 1, True, 'C', 7, False, 'B', 2, False, 'C', 9, True]

    See also :func:`petl.transform.reshape.unflatten`.

    """

    return FlattenView(table)


Table.flatten = flatten


class FlattenView(Table):

    def __init__(self, table):
        self.table = table

    def __iter__(self):
        for row in data(self.table):
            for value in row:
                yield value


def unflatten(*args, **kwargs):
    """
    Convert a sequence of values in row-major order into a table. E.g.::

        >>> import petl as etl
        >>> a = ['A', 1, True, 'C', 7, False, 'B', 2, False, 'C', 9]
        >>> table1 = etl.unflatten(a, 3)
        >>> table1
        +-----+----+-------+
        | f0  | f1 | f2    |
        +=====+====+=======+
        | 'A' |  1 | True  |
        +-----+----+-------+
        | 'C' |  7 | False |
        +-----+----+-------+
        | 'B' |  2 | False |
        +-----+----+-------+
        | 'C' |  9 | None  |
        +-----+----+-------+

        >>> # a table and field name can also be provided as arguments
        ... table2 = [['lines'],
        ...           ['A'],
        ...           [1],
        ...           [True],
        ...           ['C'],
        ...           [7],
        ...           [False],
        ...           ['B'],
        ...           [2],
        ...           [False],
        ...           ['C'],
        ...           [9]]
        >>> table3 = etl.unflatten(table2, 'lines', 3)
        >>> table3
        +-----+----+-------+
        | f0  | f1 | f2    |
        +=====+====+=======+
        | 'A' |  1 | True  |
        +-----+----+-------+
        | 'C' |  7 | False |
        +-----+----+-------+
        | 'B' |  2 | False |
        +-----+----+-------+
        | 'C' |  9 | None  |
        +-----+----+-------+

    See also :func:`petl.transform.reshape.flatten`.

    """

    return UnflattenView(*args, **kwargs)


Table.unflatten = unflatten


class UnflattenView(Table):

    def __init__(self, *args, **kwargs):
        if len(args) == 2:
            self.input = args[0]
            self.period = args[1]
        elif len(args) == 3:
            self.input = values(args[0], args[1])
            self.period = args[2]
        else:
            assert False, 'invalid arguments'
        self.missing = kwargs.get('missing', None)

    def __iter__(self):
        inpt = self.input
        period = self.period
        missing = self.missing

        # generate header row
        outhdr = tuple('f%s' % i for i in range(period))
        yield outhdr

        # generate data rows
        row = list()
        for v in inpt:
            if len(row) < period:
                row.append(v)
            else:
                yield tuple(row)
                row = [v]

        # deal with last row
        if len(row) > 0:
            if len(row) < period:
                row.extend([missing] * (period - len(row)))
            yield tuple(row)
