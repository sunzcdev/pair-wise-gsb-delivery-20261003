from __future__ import absolute_import, print_function, division


import os
import pickle
import tempfile
import weakref
from collections import deque

from petl.compat import next, integer_types, string_types, text_type


import petl.config as config
from petl.errors import ArgumentError, FieldSelectionError
from petl.util.base import Table, expr, fieldnames, Record
from petl.util.parsers import numparser


def convert(table, *args, **kwargs):
    """Transform values under one or more fields via arbitrary functions, method
    invocations or dictionary translations. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz'],
        ...           ['A', '2.4', 12],
        ...           ['B', '5.7', 34],
        ...           ['C', '1.2', 56]]
        >>> # using a built-in function:
        ... table2 = etl.convert(table1, 'bar', float)
        >>> table2
        +-----+-----+-----+
        | foo | bar | baz |
        +=====+=====+=====+
        | 'A' | 2.4 |  12 |
        +-----+-----+-----+
        | 'B' | 5.7 |  34 |
        +-----+-----+-----+
        | 'C' | 1.2 |  56 |
        +-----+-----+-----+

        >>> # using a lambda function::
        ... table3 = etl.convert(table1, 'baz', lambda v: v*2)
        >>> table3
        +-----+-------+-----+
        | foo | bar   | baz |
        +=====+=======+=====+
        | 'A' | '2.4' |  24 |
        +-----+-------+-----+
        | 'B' | '5.7' |  68 |
        +-----+-------+-----+
        | 'C' | '1.2' | 112 |
        +-----+-------+-----+

        >>> # a method of the data value can also be invoked by passing
        ... # the method name
        ... table4 = etl.convert(table1, 'foo', 'lower')
        >>> table4
        +-----+-------+-----+
        | foo | bar   | baz |
        +=====+=======+=====+
        | 'a' | '2.4' |  12 |
        +-----+-------+-----+
        | 'b' | '5.7' |  34 |
        +-----+-------+-----+
        | 'c' | '1.2' |  56 |
        +-----+-------+-----+

        >>> # arguments to the method invocation can also be given
        ... table5 = etl.convert(table1, 'foo', 'replace', 'A', 'AA')
        >>> table5
        +------+-------+-----+
        | foo  | bar   | baz |
        +======+=======+=====+
        | 'AA' | '2.4' |  12 |
        +------+-------+-----+
        | 'B'  | '5.7' |  34 |
        +------+-------+-----+
        | 'C'  | '1.2' |  56 |
        +------+-------+-----+

        >>> # values can also be translated via a dictionary
        ... table7 = etl.convert(table1, 'foo', {'A': 'Z', 'B': 'Y'})
        >>> table7
        +-----+-------+-----+
        | foo | bar   | baz |
        +=====+=======+=====+
        | 'Z' | '2.4' |  12 |
        +-----+-------+-----+
        | 'Y' | '5.7' |  34 |
        +-----+-------+-----+
        | 'C' | '1.2' |  56 |
        +-----+-------+-----+

        >>> # the same conversion can be applied to multiple fields
        ... table8 = etl.convert(table1, ('foo', 'bar', 'baz'), str)
        >>> table8
        +-----+-------+------+
        | foo | bar   | baz  |
        +=====+=======+======+
        | 'A' | '2.4' | '12' |
        +-----+-------+------+
        | 'B' | '5.7' | '34' |
        +-----+-------+------+
        | 'C' | '1.2' | '56' |
        +-----+-------+------+

        >>> # multiple conversions can be specified at the same time
        ... table9 = etl.convert(table1, {'foo': 'lower',
        ...                               'bar': float,
        ...                               'baz': lambda v: v * 2})
        >>> table9
        +-----+-----+-----+
        | foo | bar | baz |
        +=====+=====+=====+
        | 'a' | 2.4 |  24 |
        +-----+-----+-----+
        | 'b' | 5.7 |  68 |
        +-----+-----+-----+
        | 'c' | 1.2 | 112 |
        +-----+-----+-----+

        >>> # ...or alternatively via a list
        ... table10 = etl.convert(table1, ['lower', float, lambda v: v*2])
        >>> table10
        +-----+-----+-----+
        | foo | bar | baz |
        +=====+=====+=====+
        | 'a' | 2.4 |  24 |
        +-----+-----+-----+
        | 'b' | 5.7 |  68 |
        +-----+-----+-----+
        | 'c' | 1.2 | 112 |
        +-----+-----+-----+

        >>> # conversion can be conditional
        ... table11 = etl.convert(table1, 'baz', lambda v: v * 2,
        ...                       where=lambda r: r.foo == 'B')
        >>> table11
        +-----+-------+-----+
        | foo | bar   | baz |
        +=====+=======+=====+
        | 'A' | '2.4' |  12 |
        +-----+-------+-----+
        | 'B' | '5.7' |  68 |
        +-----+-------+-----+
        | 'C' | '1.2' |  56 |
        +-----+-------+-----+

        >>> # conversion can access other values from the same row
        ... table12 = etl.convert(table1, 'baz',
        ...                       lambda v, row: v * float(row.bar),
        ...                       pass_row=True)
        >>> table12
        +-----+-------+--------------------+
        | foo | bar   | baz                |
        +=====+=======+====================+
        | 'A' | '2.4' | 28.799999999999997 |
        +-----+-------+--------------------+
        | 'B' | '5.7' |              193.8 |
        +-----+-------+--------------------+
        | 'C' | '1.2' |               67.2 |
        +-----+-------+--------------------+
        >>> # conversion can use a custom function
        >>> def my_func(val, row):
        ...     return float(row.bar) + row.baz
        ... 
        >>> table13 = etl.convert(table1, 'foo', my_func, pass_row=True)
        >>> table13
        +------+-------+-----+
        | foo  | bar   | baz |
        +======+=======+=====+
        | 14.4 | '2.4' |  12 |
        +------+-------+-----+
        | 39.7 | '5.7' |  34 |
        +------+-------+-----+
        | 57.2 | '1.2' |  56 |
        +------+-------+-----+

    Note that either field names or indexes can be given.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    The ``pass_row`` keyword argument can be given, which if True will mean
    that both the value and the containing row will be passed as
    arguments to the conversion function (so, i.e., the conversion function
    should accept two arguments).

    The row supports attribute, field-name and zero-based index access, as
    described under :func:`petl.util.base.records`. Use field-name access for
    column names containing spaces or punctuation, which cannot be written
    as attributes. For example::

        >>> orders = [['Unit Price', 'Quantity', 'Total'],
        ...           [5, 2, None], [3, 4, None]]
        >>> totals = etl.convert(orders, 'Total',
        ...                      lambda value, row: row['Unit Price'] * row['Quantity'],
        ...                      pass_row=True)
        >>> list(totals)
        [('Unit Price', 'Quantity', 'Total'), (5, 2, 10), (3, 4, 12)]

    The same conversion can use indexes instead of field names::

        >>> totals = etl.convert(orders, 'Total',
        ...                      lambda value, row: row[0] * row[1],
        ...                      pass_row=True)
        >>> list(totals)
        [('Unit Price', 'Quantity', 'Total'), (5, 2, 10), (3, 4, 12)]

    Return the replacement value from the conversion function; the row is a
    read-only record, so do not assign to ``row['Total']``. The input table
    remains unchanged::

        >>> orders
        [['Unit Price', 'Quantity', 'Total'], [5, 2, None], [3, 4, None]]

    When multiple fields are converted in a single call, the conversions
    are independent of each other. Each conversion sees the original row::

        >>> # multiple conversions do not affect each other
        ... table13 = etl.convert(table1, {
        ...                           "foo": lambda foo, row: row.bar,
        ...                           "bar": lambda bar, row: row.foo,
        ...                       }, pass_row=True)
        >>> table13
        +-------+-----+-----+
        | foo   | bar | baz |
        +=======+=====+=====+
        | '2.4' | 'A' |  12 |
        +-------+-----+-----+
        | '5.7' | 'B' |  34 |
        +-------+-----+-----+
        | '1.2' | 'C' |  56 |
        +-------+-----+-----+

    Also accepts `failonerror` and `errorvalue` keyword arguments,
    documented under :func:`petl.config.failonerror`

    The ``trusted`` keyword argument can be used to specify whether the
    expression is trusted. See `:func:`petl.util.base.expr` for details.

    """

    converters = None
    if len(args) == 0:
        # no conversion specified, can be set afterwards via suffix notation
        pass
    elif len(args) == 1:
        converters = args[0]
    elif len(args) > 1:
        converters = dict()
        # assume first arg is field name or spec
        field = args[0]
        if len(args) == 2:
            conv = args[1]
        else:
            conv = args[1:]
        if isinstance(field, (list, tuple)):  # allow for multiple fields
            for f in field:
                converters[f] = conv
        else:
            converters[field] = conv
    return FieldConvertView(table, converters, **kwargs)


Table.convert = convert


def convertall(table, *args, **kwargs):
    """
    Convenience function to convert all fields in the table using a common
    function or mapping. See also :func:`convert`.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    # TODO don't read the data twice!
    return convert(table, fieldnames(table), *args, **kwargs)


Table.convertall = convertall


def replace(table, field, a, b, **kwargs):
    """
    Convenience function to replace all occurrences of `a` with `b` under the
    given field. See also :func:`convert`.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    return convert(table, field, {a: b}, **kwargs)


Table.replace = replace


def replaceall(table, a, b, **kwargs):
    """
    Convenience function to replace all instances of `a` with `b` under all
    fields. See also :func:`convertall`.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    return convertall(table, {a: b}, **kwargs)


Table.replaceall = replaceall


def update(table, field, value, **kwargs):
    """
    Convenience function to convert a field to a fixed value. Accepts the
    ``where`` keyword argument. See also :func:`convert`.

    """

    return convert(table, field, lambda v: value, **kwargs)


Table.update = update


def convertnumbers(table, strict=False, **kwargs):
    """
    Convenience function to convert all field values to numbers where
    possible. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar', 'baz', 'quux'],
        ...           ['1', '3.0', '9+3j', 'aaa'],
        ...           ['2', '1.3', '7+2j', None]]
        >>> table2 = etl.convertnumbers(table1)
        >>> table2
        +-----+-----+--------+-------+
        | foo | bar | baz    | quux  |
        +=====+=====+========+=======+
        |   1 | 3.0 | (9+3j) | 'aaa' |
        +-----+-----+--------+-------+
        |   2 | 1.3 | (7+2j) | None  |
        +-----+-----+--------+-------+

    """

    return convertall(table, numparser(strict), **kwargs)


Table.convertnumbers = convertnumbers


class FieldConvertView(Table):

    def __init__(self, source, converters=None, failonerror=None,
                 errorvalue=None, where=None, pass_row=False, trusted=True):
        self.source = source
        if converters is None:
            self.converters = dict()
        elif isinstance(converters, dict):
            self.converters = converters
        elif isinstance(converters, (tuple, list)):
            self.converters = dict([(i, v) for i, v in enumerate(converters)])
        else:
            raise ArgumentError('unexpected converters: %r' % converters)
        self.failonerror = (config.failonerror if failonerror is None
                                else failonerror)
        self.errorvalue = errorvalue
        self.where = where
        self.pass_row = pass_row
        self.trusted = trusted

    def __iter__(self):
        return iterfieldconvert(self.source, self.converters, self.failonerror,
                                self.errorvalue, self.where, self.pass_row, self.trusted)

    def __setitem__(self, key, value):
        self.converters[key] = value


def iterfieldconvert(source, converters, failonerror, errorvalue, where,
                     pass_row, trusted):

    # grab the fields in the source table
    it = iter(source)
    try:
        hdr = next(it)
        flds = list(map(text_type, hdr))
        yield tuple(hdr)  # these are not modified
    except StopIteration:
        hdr = flds = []  # converters will fail selecting a field

    # build converter functions
    converter_functions = dict()
    for k, c in converters.items():

        # turn field names into row indices
        if not isinstance(k, integer_types):
            try:
                k = flds.index(k)
            except ValueError:  # not in list
                raise FieldSelectionError(k)
        assert isinstance(k, int), 'expected integer, found %r' % k

        # is converter a function?
        if callable(c):
            converter_functions[k] = c

        # is converter a method name?
        elif isinstance(c, string_types):
            converter_functions[k] = methodcaller(c)

        # is converter a method name with arguments?
        elif isinstance(c, (tuple, list)) and isinstance(c[0], string_types):
            methnm = c[0]
            methargs = c[1:]
            converter_functions[k] = methodcaller(methnm, *methargs)

        # is converter a dictionary?
        elif isinstance(c, dict):
            converter_functions[k] = dictconverter(c)

        # is it something else?
        elif c is None:
            pass  # ignore
        else:
            raise ArgumentError(
                'unexpected converter specification on field %r: %r' % (k, c)
            )

    # define a function to transform a value
    def transform_value(i, v, *args):
        if i not in converter_functions:
            # no converter defined on this field, return value as-is
            return v
        else:
            try:
                return converter_functions[i](v, *args)
            except Exception as e:
                if failonerror == 'inline':
                    return e
                elif failonerror:
                    raise e
                else:
                    return errorvalue

    # define a function to transform a row
    if pass_row:
        def transform_row(_row):
            return tuple(transform_value(i, v, _row)
                         for i, v in enumerate(_row))
    else:
        def transform_row(_row):
            return tuple(transform_value(i, v)
                         for i, v in enumerate(_row))

    # prepare where function
    if isinstance(where, string_types):
        where = expr(where, trusted=trusted)
    elif where is not None:
        assert callable(where), 'expected callable for "where" argument, ' \
                                'found %r' % where

    # prepare iterator
    if pass_row or where:
        # wrap rows as records
        it = (Record(row, flds) for row in it)

    # construct the data rows
    if where is None:
        # simple case, transform all rows
        for row in it:
            yield transform_row(row)
    else:
        # conditionally transform rows
        for row in it:
            if where(row):
                yield transform_row(row)
            else:
                yield row


def convertsplit(table, *args, **kwargs):
    """Convert values and split the rows into a success table and a
    rejection table in a single pass over the source.

    Unlike :func:`convert`, which silently replaces failed conversions with
    ``None`` (or the given ``errorvalue``) and so mixes bad values together
    with genuine empty values, ``convertsplit`` routes every row on which at
    least one field conversion raises an exception into a separate rejection
    table. Rows on which every conversion succeeds, including rows skipped by
    a ``where`` predicate, are returned unchanged apart from the successful
    conversions in the success table. E.g.::

        >>> import petl as etl
        >>> table1 = [['foo', 'bar'],
        ...           ['a', '1'],
        ...           ['b', 'x'],
        ...           ['c', '2']]
        >>> success, reject = etl.convertsplit(table1, 'bar', int)
        >>> success
        +-----+-----+
        | foo | bar |
        +=====+=====+
        | 'a' |   1 |
        +-----+-----+
        | 'c' |   2 |
        +-----+-----+
        >>> reject
        +--------+------------+----------+-----------------+
        | rownum | row        | field    | error           |
        +========+============+==========+=================+
        |      2 | ('b', 'x') | ('bar',) | ('ValueError',) |
        +--------+------------+----------+-----------------+

    The source is traversed only once and each converter is called only once
    per value, so sources that support a single iteration and converters with
    side effects (e.g. counters) work correctly. The two returned tables may
    be consumed in either order and interleaved; rows waiting for the slower
    consumer are buffered in memory up to ``buffersize`` rows per table (the
    default is 1000) and then spilled to pickled temporary files. The
    temporary files and buffers are released by calling ``close()`` on either
    returned table, or automatically once the tables and their underlying
    split are garbage collected.

    The success table has the same header as the source and contains the
    fully converted row. The rejection table has the header
    ``('rownum', 'row', 'field', 'error')`` where:

    * ``rownum`` is the one-based data row number in the original source
      (the header is not counted),
    * ``row`` is the original, unconverted row,
    * ``field`` is a tuple of the fields whose conversions failed,
    * ``error`` is a tuple of the corresponding exception type names.

    A row with several failing fields produces exactly one rejection record.

    The conversion arguments are interpreted exactly as for :func:`convert`.
    The ``where`` keyword argument behaves as for :func:`convert`: rows for
    which the predicate returns false bypass every conversion and pass into
    the success table unchanged. The ``failonerror`` and ``errorvalue``
    arguments are not accepted, because failures are reported via the
    rejection table.

    """

    buffersize = kwargs.pop('buffersize', 1000)
    if 'failonerror' in kwargs or 'errorvalue' in kwargs:
        raise ArgumentError(
            'convertsplit does not accept "failonerror" or "errorvalue"; '
            'failed conversions are reported in the rejection table'
        )

    converters = None
    if len(args) == 0:
        # no conversion specified, can be set afterwards via suffix notation
        pass
    elif len(args) == 1:
        converters = args[0]
    elif len(args) > 1:
        converters = dict()
        # assume first arg is field name or spec
        field = args[0]
        if len(args) == 2:
            conv = args[1]
        else:
            conv = args[1:]
        if isinstance(field, (list, tuple)):  # allow for multiple fields
            for f in field:
                converters[f] = conv
        else:
            converters[field] = conv

    return _ConvertSplitter(table, converters=converters,
                            buffersize=buffersize, **kwargs).tables()


Table.convertsplit = convertsplit


REJECT_HEADER = ('rownum', 'row', 'field', 'error')


REJECT_KIND_SUCCESS = 'success'
REJECT_KIND_REJECT = 'reject'


class _ConvertSplitter(object):

    def __init__(self, source, converters=None, buffersize=1000,
                 where=None, pass_row=False, trusted=True):
        self.source = source
        if converters is None:
            self.converters = dict()
        elif isinstance(converters, dict):
            self.converters = converters
        elif isinstance(converters, (tuple, list)):
            self.converters = dict([(i, v) for i, v in enumerate(converters)])
        else:
            raise ArgumentError('unexpected converters: %r' % converters)
        self.buffersize = buffersize
        self.where = where
        self.pass_row = pass_row
        self.trusted = trusted
        self._driver = None
        self.success = _SplitTableView(self, REJECT_KIND_SUCCESS)
        self.reject = _SplitTableView(self, REJECT_KIND_REJECT)

    def tables(self):
        return self.success, self.reject

    def get_driver(self):
        # A driver owns one traversal of the source. Once it is exhausted and
        # both branches have been consumed it is discarded, so re-iterating
        # either table starts a fresh traversal (as every petl view does).
        driver = self._driver
        if driver is None or driver.closed:
            driver = _ConvertSplitDriver(
                self.source, self.converters, self.buffersize, self.where,
                self.pass_row, self.trusted
            )
            self._driver = driver
        return driver


class _SplitTableView(Table):

    def __init__(self, splitter, kind):
        self.splitter = splitter
        self.kind = kind

    def __iter__(self):
        driver = self.splitter.get_driver()
        if self.kind == REJECT_KIND_SUCCESS:
            if not driver._has_header:
                return
            yield tuple(driver.header)
            queue = driver.success_queue
        else:
            yield REJECT_HEADER
            queue = driver.reject_queue
        queue.rewind()
        while True:
            item = queue.get()
            if driver.producer_error is not None:
                raise driver.producer_error
            if item is _QUEUE_SENTINEL:
                break
            yield item

    def close(self):
        """Release any buffered rows and temporary files held for this
        split."""

        driver = self.splitter._driver
        if driver is not None:
            driver.close()


class _QueueSentinel(object):
    """Singleton marker for the end of a :class:`_SpillableQueue`.

    A plain object would lose its identity when round-tripped through the
    spill pickle file, so unpickling always restores the singleton.

    """

    _instance = None

    def __new__(cls):
        if cls._instance is None:
            cls._instance = super(_QueueSentinel, cls).__new__(cls)
        return cls._instance

    def __reduce__(self):
        return (_QueueSentinel, ())

    def __repr__(self):
        return '<end of queue>'


_QUEUE_SENTINEL = _QueueSentinel()


class _SpillableQueue(object):
    """A FIFO queue that holds up to ``buffersize`` items in memory and
    appends further items to a pickled temporary file.

    The first ``buffersize`` items are kept in a frozen in-memory buffer.
    Every later item (including the end sentinel) is appended to a pickled
    temporary file. The reader walks the memory buffer first and then the
    file; :meth:`rewind` moves both cursors back to the start so a branch can
    be iterated several times within the life of the owning driver.

    """

    def __init__(self, buffersize):
        self.buffersize = buffersize
        self._memory = deque()
        self._mem_cursor = 0
        self._spill_path = None
        self._spill_write = None
        self._spill_read = None
        self._closed = False
        # total number of items stored in the spill file, including any
        # end sentinel
        self._spill_count = 0
        # number of spill-file items already read in this pass
        self._spill_cursor = 0

    def put(self, item):
        if self._spill_path is None and len(self._memory) < self.buffersize:
            self._memory.append(item)
            return
        if self._spill_path is None:
            spill = tempfile.NamedTemporaryFile(
                mode='wb', suffix='.petl-split', delete=False
            )
            self._spill_path = spill.name
            self._spill_write = spill
        pickle.dump(item, self._spill_write,
                    protocol=pickle.HIGHEST_PROTOCOL)
        self._spill_count += 1

    def get(self):
        if self._mem_cursor < len(self._memory):
            item = self._memory[self._mem_cursor]
            self._mem_cursor += 1
            return item
        if self._spill_cursor >= self._spill_count:
            return _QUEUE_SENTINEL
        if self._spill_read is None:
            self._spill_write.flush()
            self._spill_read = open(self._spill_path, 'rb')
        item = pickle.load(self._spill_read)
        self._spill_cursor += 1
        return item

    def rewind(self):
        """Move the read cursors back to the first item.

        Petl views are expected to be re-iterable, and helpers such as
        ``len()`` consume a full pass before the real iteration starts.
        Re-reading the in-memory buffer is free; spilled items are re-read
        from the temporary file from the beginning. Items appended after a
        rewind become visible once the reader reaches their position.

        """
        if self._spill_read is not None:
            try:
                self._spill_read.close()
            except Exception:
                pass
            self._spill_read = None
        self._mem_cursor = 0
        self._spill_cursor = 0

    def close(self):
        if self._closed:
            return
        self._closed = True
        self._memory.clear()
        self._mem_cursor = 0
        self._spill_count = 0
        self._spill_cursor = 0
        for handle in (self._spill_write, self._spill_read):
            if handle is not None:
                try:
                    handle.close()
                except Exception:
                    pass
        self._spill_write = None
        self._spill_read = None
        if self._spill_path is not None:
            try:
                os.remove(self._spill_path)
            except OSError:
                pass
            self._spill_path = None


class _ConvertSplitDriver(object):

    def __init__(self, source, converters, buffersize, where, pass_row,
                 trusted):
        self.source = source
        self.converters = converters
        self.buffersize = buffersize
        self.where = where
        self.pass_row = pass_row
        self.trusted = trusted
        self.success_queue = _SpillableQueue(buffersize)
        self.reject_queue = _SpillableQueue(buffersize)
        self.header = ()
        self._has_header = False
        self._producer_done = False
        self.producer_error = None
        self.closed = False
        self._finalizer = None
        self._run()

    def _build_converter_functions(self, flds):
        converter_functions = dict()
        for k, c in self.converters.items():
            # turn field names into row indices
            if not isinstance(k, integer_types):
                try:
                    k = flds.index(k)
                except ValueError:  # not in list
                    raise FieldSelectionError(k)
            assert isinstance(k, int), 'expected integer, found %r' % k

            # is converter a function?
            if callable(c):
                converter_functions[k] = c

            # is converter a method name?
            elif isinstance(c, string_types):
                converter_functions[k] = methodcaller(c)

            # is converter a method name with arguments?
            elif isinstance(c, (tuple, list)) and isinstance(c[0],
                                                             string_types):
                methnm = c[0]
                methargs = c[1:]
                converter_functions[k] = methodcaller(methnm, *methargs)

            # is converter a dictionary?
            elif isinstance(c, dict):
                converter_functions[k] = dictconverter(c)

            # is it something else?
            elif c is None:
                pass  # ignore
            else:
                raise ArgumentError(
                    'unexpected converter specification on field %r: %r'
                    % (k, c)
                )
        return converter_functions

    def _run(self):
        # Normalise the source once and evaluate every converter exactly once
        # per value, routing each resulting row to exactly one queue.
        it = iter(self.source)
        try:
            hdr = next(it)
        except StopIteration:
            self.header = ()
            self._has_header = False
            self._finish()
            return
        flds = list(map(text_type, hdr))
        self.header = tuple(hdr)
        self._has_header = True

        converter_functions = self._build_converter_functions(flds)

        def attempt(i, v, row):
            if i not in converter_functions:
                return v, None
            try:
                if self.pass_row:
                    return converter_functions[i](v, row), None
                return converter_functions[i](v), None
            except Exception as e:
                return None, e

        where = self.where
        if isinstance(where, string_types):
            where = expr(where, trusted=self.trusted)
        elif where is not None:
            assert callable(where), 'expected callable for "where" ' \
                                    'argument, found %r' % where

        use_records = self.pass_row or where is not None

        try:
            for rownum, raw_row in enumerate(it, start=1):
                row = tuple(raw_row)
                record = Record(row, flds) if use_records else row

                if where is not None and not where(record):
                    # rows that do not match the predicate are untouched
                    self.success_queue.put(row)
                    continue

                converted = list(row)
                failed_fields = []
                failed_errors = []
                for i, v in enumerate(row):
                    new_value, error = attempt(i, v, record)
                    if error is None:
                        converted[i] = new_value
                    else:
                        failed_fields.append(flds[i] if i < len(flds) else i)
                        failed_errors.append(type(error).__name__)

                if failed_errors:
                    self.reject_queue.put(
                        (rownum, row, tuple(failed_fields),
                         tuple(failed_errors))
                    )
                else:
                    self.success_queue.put(tuple(converted))
        except BaseException as error:
            # remember the failure so either consumer sees it, but make sure
            # buffered rows and temp files do not leak meanwhile
            self.producer_error = error
            self._finish()
            return

        self._finish()

    def _finish(self):
        self._producer_done = True
        self.success_queue.put(_QUEUE_SENTINEL)
        self.reject_queue.put(_QUEUE_SENTINEL)
        # last resort cleanup if the caller never iterates either table;
        # reference the queues rather than the driver so the finalizer does
        # not keep the driver alive through a bound-method reference cycle
        self._finalizer = weakref.finalize(
            self, _finalize_split_queues,
            self.success_queue, self.reject_queue
        )

    def close(self):
        if self.closed:
            return
        self.closed = True
        if self._finalizer is not None:
            self._finalizer.detach()
        _finalize_split_queues(self.success_queue, self.reject_queue)


def _finalize_split_queues(success_queue, reject_queue):
    success_queue.close()
    reject_queue.close()


def methodcaller(nm, *args):
    return lambda v: getattr(v, nm)(*args)


def dictconverter(d):
    def conv(v):
        try:
            if v in d:
                return d[v]
            else:
                return v
        except TypeError:
            # value is not hashable
            return v
    return conv


def format(table, field, fmt, **kwargs):
    """
    Convenience function to format all values in the given `field` using the
    `fmt` format string.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    conv = lambda v: fmt.format(v)
    return convert(table, field, conv, **kwargs)


Table.format = format


def formatall(table, fmt, **kwargs):
    """
    Convenience function to format all values in all fields using the
    `fmt` format string.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    conv = lambda v: fmt.format(v)
    return convertall(table, conv, **kwargs)


Table.formatall = formatall


def interpolate(table, field, fmt, **kwargs):
    """
    Convenience function to interpolate all values in the given `field` using
    the `fmt` string.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    conv = lambda v: fmt % v
    return convert(table, field, conv, **kwargs)


Table.interpolate = interpolate


def interpolateall(table, fmt, **kwargs):
    """
    Convenience function to interpolate all values in all fields using
    the `fmt` string.

    The ``where`` keyword argument can be given with a callable or expression
    which is evaluated on each row and which should return True if the
    conversion should be applied on that row, else False.

    """

    conv = lambda v: fmt % v
    return convertall(table, conv, **kwargs)


Table.interpolateall = interpolateall
