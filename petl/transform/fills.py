from __future__ import absolute_import, print_function, division


import datetime
from decimal import Decimal

from petl.compat import next


from petl.errors import ArgumentError
from petl.util.base import Table, asindices


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

    """

    return FillDownView(table, fields, **kwargs)


Table.filldown = filldown


class FillDownView(Table):

    def __init__(self, table, fields, missing=None):
        self.table = table
        self.fields = fields
        self.missing = missing

    def __iter__(self):
        return iterfilldown(self.table, self.fields, self.missing)


def iterfilldown(table, fillfields, missing):
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
    try:
        fill = list(next(it))  # fill values
    except StopIteration:
        return
    yield tuple(fill)
    for row in it:
        outrow = list(row)
        for idx in fillindices:
            if row[idx] == missing:
                outrow[idx] = fill[idx]  # fill down
            else:
                fill[idx] = row[idx]  # new fill value
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


def filllinear(table, x, y, by=None, maxgap=None, missing=None,
               buffersize=1000, onoverflow='error'):
    """
    Fill missing values by linear interpolation between the surrounding
    non-missing values, using the values of the `x` field as coordinates.
    E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'time', 'value'],
        ...           ['a', 1, 10.0],
        ...           ['a', 2, None],
        ...           ['a', 3, None],
        ...           ['a', 4, 16.0],
        ...           ['b', 1, None],
        ...           ['b', 2, 5.0],
        ...           ['b', 3, None]]
        >>> table2 = etl.filllinear(table1, 'time', 'value', by='id')
        >>> table2.lookall()
        +-----+------+-------+
        | id  | time | value |
        +=====+======+=======+
        | 'a' |    1 |  10.0 |
        +-----+------+-------+
        | 'a' |    2 |  12.0 |
        +-----+------+-------+
        | 'a' |    3 |  14.0 |
        +-----+------+-------+
        | 'a' |    4 |  16.0 |
        +-----+------+-------+
        | 'b' |    1 |  None |
        +-----+------+-------+
        | 'b' |    2 |   5.0 |
        +-----+------+-------+
        | 'b' |    3 |  None |
        +-----+------+-------+

    Only missing values between two non-missing endpoints within the same
    group are interpolated; missing values at the start or end of a group
    are left as-is. Rows are assumed to be ordered by group and by
    coordinate within each group; a duplicate or out-of-order coordinate
    raises a `ValueError`.

    The `by` argument selects the field (or fields) used to group rows;
    interpolation never crosses group boundaries. If `by` is not given,
    the whole table is treated as a single group.

    The `y` argument selects the target field (or fields) to interpolate.
    A row counts as an endpoint only if all target fields are non-missing.

    The `maxgap` argument optionally limits the coordinate distance
    between the two endpoints that a missing segment may span; segments
    spanning a larger distance are left as-is. For `datetime` coordinates
    this should be a `datetime.timedelta`. Timezone-aware `datetime`
    coordinates are interpolated by real elapsed time.

    If the target values are `decimal.Decimal` instances, the interpolated
    values are computed with `Decimal` arithmetic and are not converted
    to `float`.

    While waiting for the right endpoint of a missing segment, only the
    rows of the current segment are buffered, up to `buffersize` rows.
    If the segment grows beyond that budget, a `ValueError` is raised
    (`onoverflow='error'`, the default) or the segment is emitted
    unchanged (`onoverflow='keep'`).

    Use the `missing` keyword argument to control which value is treated
    as missing (`None` by default). The order of the rows and the values
    of all other fields are passed through unchanged.

    """

    return FillLinearView(table, x, y, by=by, maxgap=maxgap, missing=missing,
                          buffersize=buffersize, onoverflow=onoverflow)


Table.filllinear = filllinear


class FillLinearView(Table):

    def __init__(self, table, x, y, by=None, maxgap=None, missing=None,
                 buffersize=1000, onoverflow='error'):
        if onoverflow not in ('error', 'keep'):
            raise ArgumentError(
                "onoverflow must be 'error' or 'keep': %r" % (onoverflow,))
        if not isinstance(buffersize, int) or buffersize < 0:
            raise ArgumentError(
                'buffersize must be a non-negative integer: %r'
                % (buffersize,))
        self.table = table
        self.x = x
        self.y = y
        self.by = by
        self.maxgap = maxgap
        self.missing = missing
        self.buffersize = buffersize
        self.onoverflow = onoverflow

    def __iter__(self):
        return iterfilllinear(self.table, self.x, self.y, self.by,
                              self.maxgap, self.missing, self.buffersize,
                              self.onoverflow)


def _asdist(delta):
    # normalise a coordinate difference to a plain number, using exact
    # integer microseconds for time deltas
    if isinstance(delta, datetime.timedelta):
        return (delta.days * 86400 + delta.seconds) * 1000000 \
            + delta.microseconds
    return delta


def _xdist(a, b):
    return _asdist(b - a)


def _interp(y0, y1, num, den):
    if isinstance(y0, Decimal) or isinstance(y1, Decimal):
        frac = Decimal(num) / Decimal(den)
    else:
        frac = num / den
    return y0 + (y1 - y0) * frac


def iterfilllinear(table, x, y, by, maxgap, missing, buffersize, onoverflow):
    it = iter(table)
    try:
        hdr = next(it)
    except StopIteration:
        return
    yield tuple(hdr)
    xindex = asindices(hdr, x)[0]
    yindices = asindices(hdr, y)
    byindices = asindices(hdr, by) if by is not None else None
    maxgapdist = _asdist(maxgap) if maxgap is not None else None
    group = None
    have_group = False
    last_x = None
    have_x = False
    left = None  # (x value, y values) of the current left endpoint
    segment = []  # buffered rows of the current missing segment
    overflowed = False
    for row in it:
        if byindices is not None:
            rowgroup = tuple(row[i] for i in byindices)
        else:
            rowgroup = None
        if not have_group or rowgroup != group:
            # trailing segment of the previous group stays as-is
            for bufrow in segment:
                yield tuple(bufrow)
            segment = []
            overflowed = False
            left = None
            have_x = False
            group = rowgroup
            have_group = True
        xval = row[xindex]
        if xval == missing:
            # cannot be placed on the coordinate axis, pass through
            yield tuple(row)
            continue
        if have_x and _xdist(last_x, xval) <= 0:
            raise ValueError(
                'duplicate or out-of-order coordinate: %r' % (xval,))
        yvals = [row[i] for i in yindices]
        if all(v != missing for v in yvals):
            # valid right endpoint, resolve any pending segment
            den = _xdist(left[0], xval) if left is not None else None
            if left is not None and segment and not overflowed \
                    and (maxgapdist is None or den <= maxgapdist):
                for bufrow in segment:
                    outrow = list(bufrow)
                    num = _xdist(left[0], bufrow[xindex])
                    for j, idx in enumerate(yindices):
                        if outrow[idx] == missing:
                            outrow[idx] = _interp(left[1][j], yvals[j],
                                                  num, den)
                    yield tuple(outrow)
            else:
                for bufrow in segment:
                    yield tuple(bufrow)
            segment = []
            overflowed = False
            left = (xval, yvals)
            yield tuple(row)
        elif left is None or overflowed:
            # leading gap of the group, or a segment already given up on
            yield tuple(row)
        else:
            segment.append(list(row))
            if len(segment) > buffersize:
                if onoverflow == 'error':
                    raise ValueError(
                        'missing segment exceeds buffersize of %d rows '
                        'while waiting for the right endpoint'
                        % buffersize)
                for bufrow in segment:
                    yield tuple(bufrow)
                segment = []
                overflowed = True
        last_x = xval
        have_x = True
    # trailing segment of the last group stays as-is
    for bufrow in segment:
        yield tuple(bufrow)
