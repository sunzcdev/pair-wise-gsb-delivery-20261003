from __future__ import absolute_import, print_function, division


from petl.compat import integer_types, next


from petl.util.base import Table, asindices


_GROUP_OPTIONS = ('key', 'groupby', 'group_by', 'by', 'group')
_RESET_OPTIONS = ('reset', 'reset_field', 'reset_fields', 'resetby')
_MAX_MISSING_OPTIONS = (
    'max_missing',
    'max_consecutive_missing',
    'max_consecutive',
    'max_fill',
    'maxfill',
    'limit',
    'max_missing_values',
)


def _pop_option(kwargs, names, default=None):
    found = [name for name in names if name in kwargs]
    if len(found) > 1:
        raise TypeError('conflicting keyword arguments: %s' %
                        ', '.join(found))
    if found:
        return kwargs.pop(found[0])
    return default


def _value_at(row, index, missing):
    if index < len(row):
        return row[index]
    return missing


def _set_value(row, index, value, missing):
    if index >= len(row):
        row.extend([missing] * (index + 1 - len(row)))
    row[index] = value


def _is_reset(value, missing):
    return value != missing and bool(value)


def filldown(table, *fields, **kwargs):
    """
    Replace missing values with non-missing values from the row above. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           [1, 'a', None],
        ...           [1, None, .23],
        ...           [1, 'b', None],
        ...           [2, None, None],
        ...           [2, None, .56],
        ...           [2, 'c', None],
        ...           [None, 'c', .72]]
        >>> table2 = etl.filldown(table1)
        >>> table2.lookall()
        +-----+-----+------+
        | foo | bar | baz  |
        +=====+=====+======+
        |   1 | 'a' | None |
        +-----+-----+------+
        |   1 | 'a' | 0.23 |
        +-----+-----+------+
        |   1 | 'b' | 0.23 |
        +-----+-----+------+
        |   2 | 'b' | 0.23 |
        +-----+-----+------+
        |   2 | 'b' | 0.56 |
        +-----+-----+------+
        |   2 | 'c' | 0.56 |
        +-----+-----+------+
        |   2 | 'c' | 0.72 |
        +-----+-----+------+

        >>> table3 = etl.filldown(table1, 'bar')
        >>> table3.lookall()
        +------+-----+------+
        | foo  | bar | baz  |
        +======+=====+======+
        |    1 | 'a' | None |
        +------+-----+------+
        |    1 | 'a' | 0.23 |
        +------+-----+------+
        |    1 | 'b' | None |
        +------+-----+------+
        |    2 | 'b' | None |
        +------+-----+------+
        |    2 | 'b' | 0.56 |
        +------+-----+------+
        |    2 | 'c' | None |
        +------+-----+------+
        | None | 'c' | 0.72 |
        +------+-----+------+

        >>> table4 = etl.filldown(table1, 'bar', 'baz')
        >>> table4.lookall()
        +------+-----+------+
        | foo  | bar | baz  |
        +======+=====+======+
        |    1 | 'a' | None |
        +------+-----+------+
        |    1 | 'a' | 0.23 |
        +------+-----+------+
        |    1 | 'b' | 0.23 |
        +------+-----+------+
        |    2 | 'b' | 0.23 |
        +------+-----+------+
        |    2 | 'b' | 0.56 |
        +------+-----+------+
        |    2 | 'c' | 0.56 |
        +------+-----+------+
        | None | 'c' | 0.72 |
        +------+-----+------+

    Use the `missing` keyword argument to control which value is treated as
    missing (`None` by default). If no fields are specified, all columns are
    filled by position, including columns with non-string header values.

    By default a single fill state is shared by every row. Use the `key`
    keyword argument to maintain independent fill states for each combination
    of values in one or more fields. Rows where any key field is missing do
    not read or update the shared per-key state. E.g.::

        >>> import petl as etl
        >>> table5 = [['device', 'reading'],
        ...           ['A', 10],
        ...           ['B', 20],
        ...           ['A', None],
        ...           ['B', None],
        ...           ['A', 11],
        ...           ['B', None]]
        >>> table6 = etl.filldown(table5, 'reading', key='device')
        >>> etl.listoftuples(table6)
        [('device', 'reading'), ('A', 10), ('B', 20), ('A', 10),
         ('B', 20), ('A', 11), ('B', 20)]

    Use the `reset` keyword argument to specify a field whose truthy value
    clears the current key's fill state. The reset row itself is not filled,
    and other keys are unaffected. E.g.::

        >>> table7 = [['device', 'reading', 'reset'],
        ...           ['A', 10, False],
        ...           ['B', 20, False],
        ...           ['A', None, False],
        ...           ['A', None, True],
        ...           ['B', None, False],
        ...           ['A', None, False]]
        >>> table8 = etl.filldown(table7, 'reading', key='device',
        ...                       reset='reset')
        >>> etl.listoftuples(table8)
        [('device', 'reading', 'reset'), ('A', 10, False),
         ('B', 20, False), ('A', 10, False), ('A', None, True),
         ('B', 20, False), ('A', None, False)]

    The `max_missing` keyword argument limits how many consecutive missing
    values are filled for each field and key. A non-missing observation
    starts a new run. E.g.::

        >>> table9 = [['device', 'reading'],
        ...           ['A', 10],
        ...           ['A', None],
        ...           ['A', None],
        ...           ['A', None],
        ...           ['A', 11],
        ...           ['A', None]]
        >>> table10 = etl.filldown(table9, 'reading', key='device',
        ...                        max_missing=2)
        >>> etl.listoftuples(table10)
        [('device', 'reading'), ('A', 10), ('A', 10), ('A', 10),
         ('A', None), ('A', 11), ('A', 11)]

    State is created anew on each iteration of the returned view, so iterating
    the same view more than once gives the same result.

    """

    missing = kwargs.pop('missing', None)
    key = _pop_option(kwargs, _GROUP_OPTIONS)
    reset = _pop_option(kwargs, _RESET_OPTIONS)
    max_missing = _pop_option(kwargs, _MAX_MISSING_OPTIONS)
    if reset is True:
        reset = 'reset'
    elif reset is False:
        reset = None
    if kwargs:
        unexpected = next(iter(kwargs))
        raise TypeError("filldown() got an unexpected keyword argument '%s'"
                        % unexpected)
    if max_missing is not None:
        if isinstance(max_missing, bool) or \
                not isinstance(max_missing, integer_types):
            raise TypeError('max_missing must be an integer or None')
        if max_missing < 0:
            raise ValueError('max_missing must be greater than or equal to 0')

    return FillDownView(table, fields, missing=missing, key=key, reset=reset,
                        max_missing=max_missing)


Table.filldown = filldown


class FillDownView(Table):

    def __init__(self, table, fields, missing=None, key=None, reset=None,
                 max_missing=None):
        self.table = table
        self.fields = fields
        self.missing = missing
        self.key = key
        self.reset = reset
        self.max_missing = max_missing

    def __iter__(self):
        return iterfilldown(self.table, self.fields, self.missing,
                            key=self.key, reset=self.reset,
                            max_missing=self.max_missing)


def iterfilldown(table, fillfields, missing, key=None, reset=None,
                 max_missing=None):
    it = iter(table)
    try:
        hdr = next(it)
    except StopIteration:
        return
    yield tuple(hdr)
    if not fillfields:  # fill down all fields by position
        fillindices = range(len(hdr))
    else:
        fillindices = asindices(hdr, fillfields)
    if key is None:
        keyindices = None
    else:
        keyindices = asindices(hdr, key)
    if reset is None:
        resetindices = []
    else:
        resetindices = asindices(hdr, reset)

    states = {}
    for row in it:
        if keyindices is None:
            state_key = ()
        else:
            state_key = tuple(_value_at(row, index, missing)
                              for index in keyindices)
            if any(value == missing for value in state_key):
                yield tuple(row)
                continue

        reset_row = any(_is_reset(_value_at(row, index, missing), missing)
                        for index in resetindices)
        if reset_row:
            state = {}
            states[state_key] = state
        else:
            state = states.get(state_key)
            if state is None:
                state = {}
                states[state_key] = state

        outrow = list(row)
        for idx in fillindices:
            value = _value_at(outrow, idx, missing)
            fill_state = state.get(idx)
            if value == missing:
                if fill_state is not None:
                    fill_value, run_length = fill_state
                    if max_missing is None or run_length < max_missing:
                        _set_value(outrow, idx, fill_value, missing)
                    run_length += 1
                    state[idx] = (fill_value, run_length)
            else:
                state[idx] = (value, 0)  # new fill value
        yield tuple(outrow)


def fillright(table, missing=None):
    """
    Replace missing values with preceding non-missing values. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           [1, 'a', None],
        ...           [1, None, .23],
        ...           [1, 'b', None],
        ...           [2, None, None],
        ...           [2, None, .56],
        ...           [2, 'c', None],
        ...           [None, 'c', .72]]
        >>> table2 = etl.fillright(table1)
        >>> table2.lookall()
        +------+-----+------+
        | foo  | bar | baz  |
        +======+=====+======+
        |    1 | 'a' | 'a'  |
        +------+-----+------+
        |    1 |   1 | 0.23 |
        +------+-----+------+
        |    1 | 'b' | 'b'  |
        +------+-----+------+
        |    2 |   2 |    2 |
        +------+-----+------+
        |    2 |   2 | 0.56 |
        +------+-----+------+
        |    2 | 'c' | 'c'  |
        +------+-----+------+
        | None | 'c' | 0.72 |
        +------+-----+------+

    Use the `missing` keyword argument to control which value is treated as
    missing (`None` by default).

    """

    return FillRightView(table, missing=missing)


Table.fillright = fillright


class FillRightView(Table):

    def __init__(self, table, missing=None):
        self.table = table
        self.missing = missing

    def __iter__(self):
        return iterfillright(self.table, self.missing)


def iterfillright(table, missing):
    it = iter(table)
    try:
        hdr = next(it)
    except StopIteration:
        return
    yield tuple(hdr)
    for row in it:
        outrow = list(row)
        for i, _ in enumerate(outrow):
            if i > 0 and outrow[i] == missing and outrow[i-1] != missing:
                outrow[i] = outrow[i-1]
        yield tuple(outrow)


def fillleft(table, missing=None):
    """
    Replace missing values with following non-missing values. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           [1, 'a', None],
        ...           [1, None, .23],
        ...           [1, 'b', None],
        ...           [2, None, None],
        ...           [2, None, .56],
        ...           [2, 'c', None],
        ...           [None, 'c', .72]]
        >>> table2 = etl.fillleft(table1)
        >>> table2.lookall()
        +-----+------+------+
        | foo | bar  | baz  |
        +=====+======+======+
        |   1 | 'a'  | None |
        +-----+------+------+
        |   1 | 0.23 | 0.23 |
        +-----+------+------+
        |   1 | 'b'  | None |
        +-----+------+------+
        |   2 | None | None |
        +-----+------+------+
        |   2 | 0.56 | 0.56 |
        +-----+------+------+
        |   2 | 'c'  | None |
        +-----+------+------+
        | 'c' | 'c'  | 0.72 |
        +-----+------+------+

    Use the `missing` keyword argument to control which value is treated as
    missing (`None` by default).

    """

    return FillLeftView(table, missing=missing)


Table.fillleft = fillleft


class FillLeftView(Table):

    def __init__(self, table, missing=None):
        self.table = table
        self.missing = missing

    def __iter__(self):
        return iterfillleft(self.table, self.missing)


def iterfillleft(table, missing):
    it = iter(table)
    try:
        hdr = next(it)
    except StopIteration:
        return
    yield tuple(hdr)
    for row in it:
        outrow = list(reversed(row))
        for i, _ in enumerate(outrow):
            if i > 0 and outrow[i] == missing and outrow[i-1] != missing:
                outrow[i] = outrow[i-1]
        yield tuple(reversed(outrow))
