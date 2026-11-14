from __future__ import absolute_import, print_function, division


import datetime as dt
import math
import numbers
from collections import OrderedDict
from decimal import Decimal

from petl.compat import integer_types, next, string_types, text_type, xrange
from petl.errors import (ArgumentError, WindowDataError, WindowOrderError,
                         WindowTimeTypeError, WindowTimezoneError)
from petl.transform.sorts import sort
from petl.util.base import Record, Table, asindices


_UNSET = object()


def _option(value, kwargs, aliases, default, name):
    candidates = []
    if value is not _UNSET:
        candidates.append(value)
    for alias in aliases:
        if alias in kwargs:
            candidates.append(kwargs.pop(alias))
    if not candidates:
        return default
    chosen = candidates[0]
    for candidate in candidates[1:]:
        if candidate != chosen:
            raise ArgumentError(
                'conflicting values for %s: %r and %r'
                % (name, chosen, candidate))
    return chosen


def _resolve_options(width, step, key, time, origin, aggregation, value,
                     start, stop, field, kwargs):
    width = _option(width, kwargs, ('window', 'window_width', 'window_size',
                                    'size', 'duration'), None, 'window width')
    step = _option(step, kwargs, ('slide', 'window_step', 'hop', 'advance'),
                   None,
                   'window step')
    key = _option(key, kwargs, ('partition', 'partition_key',
                                'partition_by', 'keys', 'groupby'), None,
                  'business key')
    time = _option(time, kwargs, ('time_field', 'event_time', 'timestamp',
                                  'timestamp_field', 'time_column',
                                  'event_time_col', 'timestamp_col', 'on'),
                   'time', 'event-time field')
    origin = _option(origin, kwargs, ('epoch', 'anchor'), None, 'origin')
    aggregation = _option(aggregation, kwargs,
                          ('aggregate', 'agg', 'aggs', 'aggregators',
                           'aggregator', 'reducer'), _UNSET, 'aggregation')
    value = _option(value, kwargs, ('value_field',), None, 'value field')
    start = _option(start, kwargs, ('start_field', 'window_start_field',
                                    'window_start'), 'window_start',
                    'window start field')
    stop = _option(stop, kwargs, ('stop_field', 'end_field',
                                  'window_end_field', 'window_end'),
                   'window_end', 'window end field')
    field = _option(field, kwargs, ('aggregation_field',), 'value',
                    'aggregation field')
    if kwargs:
        raise ArgumentError(
            'unexpected window keyword argument(s): %s'
            % ', '.join(sorted(kwargs)))
    if width is None:
        raise ArgumentError('window width is required')
    if step is None:
        raise ArgumentError('window step is required')
    _check_positive(width, 'window width')
    _check_positive(step, 'window step')
    return (width, step, key, time, origin, aggregation, value, start, stop,
            field)


def _is_number(value):
    return isinstance(value, (numbers.Number, Decimal))


def _check_positive(value, name):
    if isinstance(value, dt.timedelta):
        positive = value > dt.timedelta(0)
    elif isinstance(value, bool) or not _is_number(value):
        raise ArgumentError(
            '%s must be a positive number or datetime.timedelta, found %r'
            % (name, value))
    else:
        try:
            positive = value > 0
        except Exception:
            positive = False
    if not positive:
        raise ArgumentError('%s must be positive, found %r' % (name, value))


def _as_timedelta(value, name):
    if isinstance(value, dt.timedelta):
        return value
    if isinstance(value, bool) or not _is_number(value):
        raise ArgumentError(
            '%s must be datetime.timedelta or a number of seconds, found %r'
            % (name, value))
    try:
        result = dt.timedelta(seconds=float(value))
    except (OverflowError, TypeError, ValueError):
        raise ArgumentError('%s is not a valid duration: %r'
                            % (name, value))
    if result <= dt.timedelta(0):
        raise ArgumentError('%s must be positive, found %r' % (name, value))
    return result


def _floor_div(dividend, divisor):
    try:
        quotient = dividend // divisor
    except TypeError:
        # Python 2 timedelta does not implement floor division.
        quotient = (dividend.total_seconds() //
                    divisor.total_seconds())
    if isinstance(quotient, integer_types):
        return quotient
    try:
        return int(quotient)
    except (TypeError, ValueError, OverflowError):
        return math.floor(quotient)


def _window_indices(event_time, origin, width, step):
    # A time t belongs to window k iff
    # origin + k * step <= t < origin + k * step + width.
    latest = _floor_div(event_time - origin, step)
    earliest = _floor_div(event_time - width - origin, step) + 1
    return earliest, latest


def _window_bounds(origin, index, width, step):
    start = origin + index * step
    return start, start + width


def _time_is_aware(value):
    return value.tzinfo is not None and value.utcoffset() is not None


def _time_mode(event_time, origin, width, step, row_number):
    if isinstance(event_time, dt.datetime):
        aware = _time_is_aware(event_time)
        if origin is None:
            tz = event_time.tzinfo if aware else None
            origin = dt.datetime(1970, 1, 1, tzinfo=tz)
        elif not isinstance(origin, dt.datetime):
            raise WindowTimeTypeError(
                'origin is not a datetime while event time is',
                row=row_number, value=event_time, previous=origin)
        elif _time_is_aware(origin) != aware:
            raise WindowTimezoneError(
                'origin and event time disagree on timezone awareness',
                row=row_number, value=event_time, previous=origin)
        width = _as_timedelta(width, 'window width')
        step = _as_timedelta(step, 'window step')
        return 'datetime', origin, width, step, aware

    if isinstance(event_time, dt.date):
        raise WindowTimeTypeError(
            'date event times are not supported; use datetime',
            row=row_number, value=event_time)

    if isinstance(origin, (dt.datetime, dt.date)):
        raise WindowTimeTypeError(
            'origin is a datetime while event time is not',
            row=row_number, value=event_time, previous=origin)
    if isinstance(width, dt.timedelta) or isinstance(step, dt.timedelta):
        raise WindowTimeTypeError(
            'timedelta width/step cannot be used with numeric event times',
            row=row_number, value=event_time)
    if origin is None:
        origin = 0
    return 'numeric', origin, width, step, None


def _validate_time(event_time, mode, aware, previous, row_number):
    if mode == 'datetime':
        if not isinstance(event_time, dt.datetime):
            raise WindowTimeTypeError(
                'mixed datetime and non-datetime event times',
                row=row_number, value=event_time, previous=previous)
        current_aware = _time_is_aware(event_time)
        if current_aware != aware:
            raise WindowTimezoneError(
                'mixed timezone-aware and naive event times',
                row=row_number, value=event_time, previous=previous)
    elif isinstance(event_time, (dt.datetime, dt.date)):
        raise WindowTimeTypeError(
            'mixed non-datetime and datetime event times',
            row=row_number, value=event_time, previous=previous)

    if previous is not None:
        try:
            backwards = event_time < previous
        except TypeError:
            raise WindowTimeTypeError(
                'event time is not comparable with the previous event time',
                row=row_number, value=event_time, previous=previous)
        if backwards:
            raise WindowOrderError(
                'event time went backwards; source must be sorted by event '
                'time', row=row_number, value=event_time, previous=previous)


def _partition(row, key, key_indices, flds, missing, row_number):
    if key is None:
        return ()
    if callable(key):
        value = key(Record(row, flds, missing=missing))
        partition = (value,)
    else:
        values = []
        for index in key_indices:
            try:
                values.append(row[index])
            except IndexError:
                values.append(missing)
        partition = tuple(values)
    try:
        hash(partition)
    except TypeError:
        raise WindowDataError(
            'business key is not hashable', row=row_number, value=partition)
    return partition


def _normalize_aggregation(aggregation, value, field, hdr):
    """Normalize the aggregation forms accepted by petl.aggregate."""
    if callable(aggregation):
        if value is None:
            source_indices = None
        else:
            source_indices = tuple(asindices(hdr, value))
        return [field], [(field, source_indices, aggregation, 'simple')]

    if aggregation is None:
        return [], []

    if not isinstance(aggregation, (list, tuple, dict)):
        raise ArgumentError(
            'expected aggregation is callable, list, tuple, dict or None, '
            'found %r' % (aggregation,))

    if isinstance(aggregation, dict):
        items = list(aggregation.items())
    else:
        items = []
        if aggregation and not isinstance(aggregation[0], (list, tuple)):
            items.append((aggregation[0], aggregation[1:]))
        else:
            for item in aggregation:
                if not isinstance(item, (list, tuple)) or not item:
                    raise ArgumentError(
                        'invalid aggregation item: %r' % (item,))
                items.append((item[0], item[1:]))

    outfields = []
    specs = []
    for outfield, spec in items:
        if callable(spec):
            source, func = None, spec
        elif isinstance(spec, string_types):
            source, func = spec, list
        elif isinstance(spec, integer_types):
            source, func = spec, list
        elif isinstance(spec, (list, tuple)):
            if len(spec) == 1 and isinstance(spec[0], string_types):
                source, func = spec[0], list
            elif len(spec) == 1 and isinstance(spec[0], integer_types):
                source, func = spec[0], list
            elif len(spec) == 1 and callable(spec[0]):
                source, func = None, spec[0]
            elif len(spec) == 2:
                source, func = spec
            else:
                raise ArgumentError(
                    'invalid aggregation: %r, %r' % (outfield, spec))
        else:
            raise ArgumentError(
                'invalid aggregation: %r, %r' % (outfield, spec))

        if source is None:
            indices = None
        else:
            indices = tuple(asindices(hdr, source))
        outfields.append(outfield)
        specs.append((outfield, indices, func, 'multi'))
    return outfields, specs


def _check_aggregation(aggregation):
    if not (callable(aggregation) or aggregation is None or
            isinstance(aggregation, (list, tuple, dict))):
        raise ArgumentError(
            'expected aggregation is callable, list, tuple, dict or None, '
            'found %r' % (aggregation,))


def _apply_aggregation(specs, rows, flds, missing):
    records = [Record(row, flds, missing=missing) for row in rows]
    result = []
    for _, indices, func, kind in specs:
        if func is len:
            result.append(len(records))
        elif indices is None:
            if kind == 'simple':
                result.append(func(iter(records)))
            else:
                result.append(func(records))
        elif len(indices) == 1:
            index = indices[0]
            result.append(func(record[index] for record in records))
        else:
            def items(record, idxs=indices):
                return tuple(record[index] for index in idxs)
            result.append(func(items(record) for record in records))
    return result


class WindowAggregateState(object):
    """Live state of a window aggregation iterator.

    ``watermark`` is the latest event time consumed.  Active windows are the
    windows that may still receive records.  ``max_active_end`` is the
    exclusive event-time upper bound of that retained state.
    """

    def __init__(self):
        self.watermark = None
        self.last_time = None
        self.closed_through = None
        self.rows_read = 0
        self.windows_emitted = 0
        self.active_windows = OrderedDict()
        self.finished = False
        self.width = None
        self.step = None
        self.origin = None
        self.time_field = None
        self.key = None

    @property
    def max_active_end(self):
        if not self.active_windows:
            return None
        return max(window.end for window in self.active_windows.values())

    @property
    def active_window_count(self):
        return len(self.active_windows)

    active_window_bound = max_active_end
    active_window_upper_bound = max_active_end
    window_upper_bound = max_active_end
    upper_bound = max_active_end
    retained_state_upper_bound = max_active_end
    safe_until = max_active_end
    active_end = max_active_end
    active_bound = max_active_end
    max_window_end = max_active_end
    retained_until = max_active_end

    def __repr__(self):
        return (
            'WindowAggregateState(rows_read=%r, windows_emitted=%r, '
            'watermark=%r, active_window_count=%r, max_active_end=%r)'
            % (self.rows_read, self.windows_emitted, self.watermark,
               self.active_window_count, self.max_active_end)
        )

    def get_active_window_bound(self):
        return self.max_active_end

    def get_active_window_upper_bound(self):
        return self.max_active_end


class _ActiveWindow(object):

    __slots__ = ('partition', 'partition_id', 'index', 'start', 'end',
                 'key_values', 'rows')

    def __init__(self, partition, partition_id, index, start, end,
                 key_values):
        self.partition = partition
        self.partition_id = partition_id
        self.index = index
        self.start = start
        self.end = end
        self.key_values = key_values
        self.rows = []


class WindowAggregateIterator(object):
    """Iterator over :func:`windowaggregate` results with live state."""

    def __init__(self, generator, state):
        self._generator = generator
        self.state = state

    def __iter__(self):
        return self

    def __next__(self):
        return next(self._generator)

    next = __next__

    @property
    def watermark(self):
        return self.state.watermark

    @property
    def max_active_end(self):
        return self.state.max_active_end

    active_window_bound = max_active_end
    active_window_upper_bound = max_active_end
    window_upper_bound = max_active_end
    upper_bound = max_active_end
    retained_state_upper_bound = max_active_end
    safe_until = max_active_end
    active_end = max_active_end
    active_bound = max_active_end
    max_window_end = max_active_end
    retained_until = max_active_end

    @property
    def active_window_count(self):
        return self.state.active_window_count

    @property
    def active_windows(self):
        return self.state.active_windows

    @property
    def closed_through(self):
        return self.state.closed_through

    @property
    def rows_read(self):
        return self.state.rows_read

    @property
    def windows_emitted(self):
        return self.state.windows_emitted

    @property
    def finished(self):
        return self.state.finished

    def get_active_window_bound(self):
        return self.state.max_active_end

    def get_active_window_upper_bound(self):
        return self.state.max_active_end

    def close(self):
        return self._generator.close()

    def send(self, value):
        return self._generator.send(value)

    def throw(self, *args):
        return self._generator.throw(*args)


class WindowAggregateView(Table):

    def __init__(self, table, width, step, key=None, time='time', origin=None,
                 aggregation=None, value=None, start='window_start',
                 stop='window_end', field='value', missing=None,
                 presorted=True):
        if not presorted:
            table = sort(table, time)
        self.table = table
        self.width = width
        self.step = step
        self.key = key
        self.time = time
        self.origin = origin
        self.aggregation = aggregation
        self.value = value
        self.start = start
        self.stop = stop
        self.field = field
        self.missing = missing
        self.presorted = presorted
        self._last_state = None

    def __len__(self):
        raise TypeError(
            'length is not available for a streaming window view; consume '
            'the iterator instead')

    def __bool__(self):
        return True

    __nonzero__ = __bool__

    def __iter__(self):
        iterator = iterwindowaggregate(
            self.table, self.width, self.step, key=self.key, time=self.time,
            origin=self.origin, aggregation=self.aggregation,
            value=self.value, start=self.start, stop=self.stop,
            field=self.field, missing=self.missing, presorted=True)
        self._last_state = iterator.state
        return iterator

    @property
    def state(self):
        return self._last_state

    @property
    def watermark(self):
        if self._last_state is None:
            return None
        return self._last_state.watermark

    @property
    def max_active_end(self):
        if self._last_state is None:
            return None
        return self._last_state.max_active_end

    active_window_bound = max_active_end
    active_window_upper_bound = max_active_end
    window_upper_bound = max_active_end
    upper_bound = max_active_end
    retained_state_upper_bound = max_active_end
    safe_until = max_active_end
    active_end = max_active_end
    active_bound = max_active_end
    max_window_end = max_active_end
    retained_until = max_active_end

    @property
    def active_window_count(self):
        if self._last_state is None:
            return 0
        return self._last_state.active_window_count

    @property
    def active_windows(self):
        if self._last_state is None:
            return None
        return self._last_state.active_windows

    @property
    def closed_through(self):
        if self._last_state is None:
            return None
        return self._last_state.closed_through

    @property
    def rows_read(self):
        if self._last_state is None:
            return 0
        return self._last_state.rows_read

    @property
    def windows_emitted(self):
        if self._last_state is None:
            return 0
        return self._last_state.windows_emitted

    @property
    def finished(self):
        return self._last_state is not None and self._last_state.finished

    def get_active_window_bound(self):
        return self.max_active_end

    def get_active_window_upper_bound(self):
        return self.max_active_end


def windowaggregate(table, width=None, step=None, key=_UNSET, time=_UNSET,
                    origin=_UNSET, aggregation=_UNSET, value=_UNSET,
                    start=_UNSET, stop=_UNSET, field=_UNSET, missing=None,
                    presorted=True, **kwargs):
    """Aggregate a presorted event-time table into sliding time windows.

    A window with index ``k`` covers
    ``[origin + k * step, origin + k * step + width)``.  Indices can be
    negative, so times before ``origin`` are aligned by the same rule.  Only
    windows that received at least one row are emitted.

    The source is read once.  As the event time advances, windows whose end
    has been reached are emitted immediately.  A row belonging to several
    overlapping windows is attached to each window from the same read.

    ``aggregation`` accepts the same callable/list/tuple/dict forms as
    :func:`petl.aggregate`.  With a callable, ``value`` selects the field(s)
    passed to it; if ``value`` is ``None`` the callable receives an iterator
    of records.  The default is ``len``.  Passing ``None`` explicitly emits
    only the partition and window-boundary fields.

    The output fields are the partition key fields (if any), ``start`` and
    ``stop``, then the aggregation fields.

    If the source raises an exception, that exception propagates and active
    (incomplete) windows are not flushed.  Use :func:`iterwindowaggregate`
    and its ``state.max_active_end`` to inspect the retained-state upper bound
    after consuming a finite prefix.
    """
    (width, step, key, time, origin, aggregation, value, start, stop,
     field) = _resolve_options(
        width, step, key, time, origin, aggregation, value, start, stop,
        field, kwargs)
    if aggregation is _UNSET:
        aggregation = len
    _check_aggregation(aggregation)
    return WindowAggregateView(
        table, width, step, key=key, time=time, origin=origin,
        aggregation=aggregation, value=value, start=start, stop=stop,
        field=field, missing=missing, presorted=presorted)


def iterwindowaggregate(table, width=None, step=None, key=_UNSET, time=_UNSET,
                        origin=_UNSET, aggregation=_UNSET, value=_UNSET,
                        start=_UNSET, stop=_UNSET, field=_UNSET, missing=None,
                        presorted=True, **kwargs):
    """Return an iterator over :func:`windowaggregate` results.

    The iterator has a ``state`` attribute.  After a finite prefix has been
    consumed, ``state.watermark`` is the latest event time read and
    ``state.max_active_end`` (also ``active_window_bound``) is the exclusive
    upper bound of event time held by active windows.
    """
    (width, step, key, time, origin, aggregation, value, start, stop,
     field) = _resolve_options(
        width, step, key, time, origin, aggregation, value, start, stop,
        field, kwargs)
    if aggregation is _UNSET:
        aggregation = len
    _check_aggregation(aggregation)
    if not presorted:
        table = sort(table, time)
    state = WindowAggregateState()
    generator = _iter_windowaggregate(
        table, width, step, key, time, origin, aggregation, value, start,
        stop, field, missing, state)
    return WindowAggregateIterator(generator, state)


def _iter_windowaggregate(table, width, step, key, time_field, origin,
                           aggregation, value, start_field, stop_field, field,
                           missing, state):
    it = iter(table)
    try:
        hdr = next(it)
    except StopIteration:
        state.finished = True
        return

    flds = list(map(text_type, hdr))
    time_indices = tuple(asindices(hdr, time_field))
    if len(time_indices) != 1:
        raise ArgumentError('event time must select exactly one field')
    time_index = time_indices[0]

    if key is None or callable(key):
        key_indices = None
    else:
        key_indices = tuple(asindices(hdr, key))
        if not key_indices:
            raise ArgumentError('business key selects no fields')

    agg_fields, agg_specs = _normalize_aggregation(
        aggregation, value, field, hdr)

    if key is None:
        outhdr = []
    elif callable(key):
        outhdr = ['key']
    elif isinstance(key, (list, tuple)):
        outhdr = list(key)
    else:
        outhdr = [key]
    outhdr.extend([start_field, stop_field])
    outhdr.extend(agg_fields)
    yield tuple(outhdr)

    state.time_field = time_field
    state.key = key
    mode = None
    aware = None
    resolved_origin = origin
    resolved_width = width
    resolved_step = step
    partition_orders = {}
    next_partition_order = 0
    previous_time = None

    for row_number, row in enumerate(it, 1):
        state.rows_read = row_number
        try:
            event_time = row[time_index]
        except IndexError:
            raise WindowDataError(
                'missing event time', row=row_number, value=None)

        if mode is None:
            mode, resolved_origin, resolved_width, resolved_step, aware = \
                _time_mode(event_time, origin, width, step, row_number)
            state.origin = resolved_origin
            state.width = resolved_width
            state.step = resolved_step
        else:
            _validate_time(event_time, mode, aware, previous_time, row_number)

        previous_time = event_time
        state.last_time = event_time
        state.watermark = event_time

        partition = _partition(
            row, key, key_indices, flds, missing, row_number)

        try:
            earliest, latest = _window_indices(
                event_time, resolved_origin, resolved_width, resolved_step)
        except (ArithmeticError, TypeError, ValueError):
            raise WindowTimeTypeError(
                'event time cannot be aligned to the window origin',
                row=row_number, value=event_time, previous=resolved_origin)

        if earliest <= latest:
            if partition not in partition_orders:
                partition_orders[partition] = next_partition_order
                next_partition_order += 1
            partition_id = partition_orders[partition]

        for window_index in xrange(earliest, latest + 1):
            window_key = (partition, window_index)
            window = state.active_windows.get(window_key)
            if window is None:
                start, stop = _window_bounds(
                    resolved_origin, window_index, resolved_width,
                    resolved_step)
                window = _ActiveWindow(
                    partition, partition_id, window_index, start, stop,
                    partition)
                state.active_windows[window_key] = window
            window.rows.append(row)

        closed = [
            window for window in state.active_windows.values()
            if window.end <= event_time
        ]
        if closed:
            closed.sort(key=lambda window: (
                window.start, window.partition_id, window.index))
            outrows = []
            for window in closed:
                del state.active_windows[(window.partition, window.index)]
                values = _apply_aggregation(
                    agg_specs, window.rows, flds, missing)
                outrows.append(tuple(
                    list(window.key_values) + [window.start, window.end] +
                    values))
            active_partitions = set(
                window.partition for window in state.active_windows.values())
            for partition in list(partition_orders):
                if partition not in active_partitions:
                    del partition_orders[partition]
            state.closed_through = event_time
            for outrow in outrows:
                state.windows_emitted += 1
                yield outrow
        else:
            state.closed_through = event_time

    # Only normal exhaustion flushes active windows.  Source exceptions skip
    # this block and therefore cannot look like a successful end of stream.
    remaining = sorted(
        state.active_windows.values(),
        key=lambda window: (
            window.start, window.partition_id, window.index))
    outrows = []
    for window in remaining:
        del state.active_windows[(window.partition, window.index)]
        values = _apply_aggregation(agg_specs, window.rows, flds, missing)
        outrows.append(tuple(
            list(window.key_values) + [window.start, window.end] + values))
    for outrow in outrows:
        state.windows_emitted += 1
        yield outrow
    state.closed_through = state.last_time
    state.finished = True


Table.windowaggregate = windowaggregate


# Convenient aliases for call sites that use a more explicit name.
timewindow = windowaggregate
timewindowaggregate = windowaggregate
slidingwindow = windowaggregate
slidingwindowaggregate = windowaggregate
windowedaggregate = windowaggregate
eventtimewindow = windowaggregate
eventtimewindowaggregate = windowaggregate
window_aggregate = windowaggregate
sliding_window_aggregate = windowaggregate
time_window_aggregate = windowaggregate
event_time_window = windowaggregate
event_time_window_aggregate = windowaggregate

Table.timewindow = timewindow
Table.timewindowaggregate = timewindowaggregate
Table.slidingwindow = slidingwindow
Table.slidingwindowaggregate = slidingwindowaggregate
Table.windowedaggregate = windowedaggregate
Table.eventtimewindow = eventtimewindow
Table.eventtimewindowaggregate = eventtimewindowaggregate
Table.window_aggregate = window_aggregate
Table.sliding_window_aggregate = sliding_window_aggregate
Table.time_window_aggregate = time_window_aggregate
Table.event_time_window = event_time_window
Table.event_time_window_aggregate = event_time_window_aggregate
