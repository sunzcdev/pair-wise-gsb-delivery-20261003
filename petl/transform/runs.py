from __future__ import absolute_import, print_function, division


from petl.compat import text_type
from petl.util.base import Table, asindices


# Sentinel used to mark a field that is absent because the source row is
# shorter than the table header, so that a missing column can be told apart
# from an actual None value stored in the row.
_MISSING = object()


class RunBreakError(Exception):
    """Raised when a user supplied break predicate fails.

    The original exception is preserved as ``__cause__`` (and remains visible
    in the traceback), while ``row`` and ``rownum`` record the position of the
    source row that was being examined when the failure happened.

    """

    def __init__(self, rownum, row, cause):
        self.rownum = rownum
        self.row = row
        self.cause = cause
        super(RunBreakError, self).__init__(
            'break predicate raised %s while examining source row %d: %s'
            % (type(cause).__name__, rownum, cause)
        )

    def __str__(self):
        return self.args[0]


def runlength(table, key, value=None, count='count', start='start',
              end='end', first='first', last='last', breakat=None):
    """
    Group adjacent rows into runs (run-length encoding) without sorting the
    data. Rows are folded together only while they are next to each other and
    share the same key value, so a key that disappears and reappears later is
    always reported as a separate run. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['a', 1, True],
        ...           ['a', 2, True],
        ...           ['b', 3, True],
        ...           ['b', 4, False],
        ...           ['a', 5, False]]
        >>> table2 = etl.runlength(table1, 'foo', value='bar')
        >>> table2
        +-----+-------+-------+-----+-------+------+
        | foo | count | start | end | first | last |
        +=====+=======+=======+=====+=======+======+
        | 'a' |     2 |     1 |   2 |     1 |    2 |
        +-----+-------+-------+-----+-------+------+
        | 'b' |     2 |     3 |   4 |     3 |    4 |
        +-----+-------+-------+-----+-------+------+
        | 'a' |     1 |     5 |   5 |     5 |    5 |
        +-----+-------+-------+-----+-------+------+

    The `key` argument selects one or more fields, by name or index (indices
    start from zero). Compound keys produce one output column per key field.

    The `value` argument likewise selects one or more fields whose first and
    last values inside each run are reported. It may be a single field name or
    index, or a list/tuple of them. If `value` is None no first/last columns
    are emitted.

    `count`, `start`, `end`, `first` and `last` name the output columns. With
    multiple value fields, the first and last column names are suffixed with
    each value field name, e.g. ``first_bar``.

    Row numbers (`start`, `end`) count data rows from one, matching
    :func:`petl.transform.basics.addrownumbers`, and exclude the header row.

    Because the source is scanned in order and only the run currently being
    built is held in memory, a run output row is available as soon as the
    first row of the following run has been read; the source is never sorted
    and no global state is accumulated.

    A `breakat` callable may be supplied to split runs even when adjacent keys
    match, for example when a time gap is too large. It is called with the
    previous and current rows (as
    :class:`petl.util.base.Record` objects, so fields work by name or index)
    and should return a true value to start a new run, e.g.::

        >>> def gap_too_large(prev, cur):
        ...     return cur.timestamp - prev.timestamp > 10

    If `breakat` raises, the exception is wrapped in
    :class:`petl.transform.runs.RunBreakError` which records the source row
    number and row being examined; the original exception is retained as the
    cause.

    Missing columns (rows shorter than the header) are reported using an
    internal sentinel rather than None, so they remain distinguishable from
    rows that genuinely contain a None value.

    """

    return RunLengthView(table, key, value=value, count=count, start=start,
                         end=end, first=first, last=last, breakat=breakat)


Table.runlength = runlength


class RunLengthView(Table):

    def __init__(self, source, key, value=None, count='count', start='start',
                 end='end', first='first', last='last', breakat=None):
        self.source = source
        self.key = key
        self.value = value
        self.count = count
        self.start = start
        self.end = end
        self.first = first
        self.last = last
        self.breakat = breakat

    def __iter__(self):
        return iterrunlength(self.source, self.key, self.value, self.count,
                             self.start, self.end, self.first, self.last,
                             self.breakat)


def _fieldgetter(indices):
    """Return a callable extracting the fields at `indices` from a row.

    Short rows yield an internal sentinel for missing columns so that a
    missing column can be distinguished from an actual None value.

    """

    if len(indices) == 1:
        index = indices[0]

        def _getone(row):
            if index < len(row):
                return row[index]
            return _MISSING
        return _getone

    def _getmany(row):
        return tuple(row[i] if i < len(row) else _MISSING
                     for i in indices)
    return _getmany


def iterrunlength(source, key, value, count, start, end, first, last,
                  breakat):
    it = iter(source)
    try:
        hdr = tuple(next(it))
    except StopIteration:
        return

    key_indices = asindices(hdr, key)
    getkey = _fieldgetter(key_indices)
    key_names = [text_type(hdr[i]) for i in key_indices]

    if value is None:
        value_indices = []
        value_names = []
    else:
        value_indices = asindices(hdr, value)
        value_names = [text_type(hdr[i]) for i in value_indices]
    getvalues = _fieldgetter(value_indices) if value_indices else None

    # build the output header
    outhdr = list(key_names)
    outhdr.append(count)
    outhdr.append(start)
    outhdr.append(end)
    if value_names:
        if len(value_names) == 1:
            outhdr.append(first)
            outhdr.append(last)
        else:
            for name in value_names:
                outhdr.append(first + '_' + name)
            for name in value_names:
                outhdr.append(last + '_' + name)
    yield tuple(outhdr)

    if breakat is not None:
        from petl.util.base import Record
        flds = list(map(text_type, hdr))

    def close_run(run_key, run_count, run_start, run_end, first_val,
                  last_val):
        if len(key_indices) == 1:
            outrow = [run_key]
        else:
            outrow = list(run_key)
        outrow.extend([run_count, run_start, run_end])
        if value_names:
            if len(value_names) == 1:
                outrow.extend([first_val, last_val])
            else:
                outrow.extend(first_val)
                outrow.extend(last_val)
        return tuple(outrow)

    # prime the first run with the first data row
    try:
        first_row = next(it)
    except StopIteration:
        return

    rownum = 1
    run_key = getkey(first_row)
    run_count = 1
    run_start = rownum
    run_end = rownum
    first_val = getvalues(first_row) if getvalues else None
    last_val = first_val
    if breakat is not None:
        prev_record = Record(first_row, flds)

    for row in it:
        rownum += 1
        curr_key = getkey(row)

        split = curr_key != run_key
        if not split and breakat is not None:
            curr_record = Record(row, flds)
            try:
                split = bool(breakat(prev_record, curr_record))
            except Exception as cause:
                raise RunBreakError(rownum, tuple(row), cause) from cause
            prev_record = curr_record

        if split:
            # the previous run is complete once this row has been inspected
            yield close_run(run_key, run_count, run_start, run_end,
                            first_val, last_val)
            # begin a new run; this row is its first member
            run_key = curr_key
            run_count = 1
            run_start = rownum
            first_val = getvalues(row) if getvalues else None
        else:
            run_count += 1

        run_end = rownum
        last_val = getvalues(row) if getvalues else None

    # flush the final run
    yield close_run(run_key, run_count, run_start, run_end, first_val,
                    last_val)
