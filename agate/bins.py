"""
Reusable bin definitions that are independent of any particular data.

A :class:`.Bins` stores exact boundaries, the open/closed rule and the labels.
It can be applied to any number of :class:`.Table` or :class:`.TableSet`
instances so that separate partitions are counted against identical bins.
"""

from bisect import bisect_right
from decimal import Decimal

from babel.numbers import format_decimal

from agate import utils
from agate.data_types import Number, Text
from agate.exceptions import BinOutOfRangeError


def _to_decimal(value):
    """
    Convert a boundary to :class:`decimal.Decimal` without ever going through
    :class:`float`, which would lose exact precision.
    """
    if isinstance(value, Decimal):
        return value

    if isinstance(value, float):
        raise TypeError('Bin boundaries must be Decimal, int or str, not float. Use Decimal(str(value)) instead.')

    return Decimal(value)


class Bins:
    """
    A reusable definition of a series of bins.

    The boundaries, open/closed rule and labels are stored on this object and
    are completely independent of any data. The same instance can be passed to
    :meth:`.Table.bins` for many tables, including each table in a
    :class:`.TableSet`, guaranteeing that every partition is counted against
    the same bins in the same order.

    Bins are half-open intervals. With the default :code:`closed='right'`, a
    value belongs to the first bin whose right boundary is greater than or
    equal to it, so bin :code:`i` covers :code:`(start_i, end_i]`. With
    :code:`closed='left'`, a bin instead covers :code:`[start_i, end_i)`.

    :param boundaries:
        A sequence of at least two exact boundary values. Values may be
        :class:`decimal.Decimal`, :class:`int` or :class:`str`. They are never
        converted through :class:`float`. Boundaries must be strictly
        increasing.
    :param closed:
        Which side of each bin is closed. Either :code:`'right'` (the default)
        for :code:`(start, end]` bins or :code:`'left'` for
        :code:`[start, end)` bins.
    :param labels:
        An optional sequence of one label per bin. If omitted, labels are
        generated from the boundaries, such as :code:`[0 - 10)`.
    :param underflow_label:
        The label used for the explicit underflow bucket that collects values
        below the first boundary.
    :param overflow_label:
        The label used for the explicit overflow bucket that collects values
        above the last boundary.
    """
    def __init__(self, boundaries, closed='right', labels=None, underflow_label='(-\u221e - {first})',
                 overflow_label='({last} - \u221e)'):
        if closed not in ('left', 'right'):
            raise ValueError("closed must be either 'left' or 'right'.")

        converted = tuple(_to_decimal(boundary) for boundary in boundaries)

        if len(converted) < 2:
            raise ValueError('At least two boundaries are required to define a bin.')

        if any(converted[i] >= converted[i + 1] for i in range(len(converted) - 1)):
            raise ValueError('Bin boundaries must be strictly increasing.')

        if labels is not None:
            labels = tuple(labels)

            if len(labels) != len(converted) - 1:
                raise ValueError('The number of labels must match the number of bins (one fewer than boundaries).')

        self._boundaries = converted
        self._closed = closed
        self._labels = labels
        self._underflow_label = underflow_label.format(first=self._format(converted[0]))
        self._overflow_label = overflow_label.format(last=self._format(converted[-1]))

        if self._underflow_label == self._overflow_label:
            raise ValueError('The underflow and overflow labels must be distinct.')

        if self._underflow_label in self.labels or self._overflow_label in self.labels:
            raise ValueError('Bin labels must not duplicate the underflow or overflow label.')

    def _format(self, value):
        decimal_places = utils.max_precision(self._boundaries)
        formatter = utils.make_number_formatter(decimal_places)

        return format_decimal(value, format=formatter)

    @property
    def boundaries(self):
        """
        The exact :class:`decimal.Decimal` boundaries of the bins.
        """
        return self._boundaries

    @property
    def closed(self):
        """
        The open/closed rule: either :code:`'left'` or :code:`'right'`.
        """
        return self._closed

    @property
    def labels(self):
        """
        The labels of the interior bins, in bin order.
        """
        if self._labels is not None:
            return self._labels

        generated = []

        for i in range(len(self._boundaries) - 1):
            start = self._format(self._boundaries[i])
            end = self._format(self._boundaries[i + 1])

            if self._closed == 'right':
                generated.append(f'({start} - {end}]')
            else:
                generated.append(f'[{start} - {end})')

        return tuple(generated)

    @property
    def underflow_label(self):
        """
        The label of the explicit underflow bucket.
        """
        return self._underflow_label

    @property
    def overflow_label(self):
        """
        The label of the explicit overflow bucket.
        """
        return self._overflow_label

    def __eq__(self, other):
        if not isinstance(other, Bins):
            return NotImplemented

        return (
            self._boundaries == other._boundaries
            and self._closed == other._closed
            and self.labels == other.labels
        )

    def __repr__(self):
        return f'agate.Bins({list(self._boundaries)!r}, closed={self._closed!r})'

    def classify(self, value):
        """
        Return the label of the bin that contains a value.

        Null values return :code:`None`. Values outside of the boundaries
        return the underflow or overflow label.
        """
        if value is None:
            return None

        boundaries = self._boundaries

        if self._closed == 'right':
            # Bins are (start, end], so a value equal to the first boundary
            # belongs to the first bin, while a value equal to the last
            # boundary belongs to the last bin.
            if value < boundaries[0]:
                return self._underflow_label

            if value >= boundaries[-1]:
                return self._overflow_label if value > boundaries[-1] else self.labels[-1]

            # First boundary strictly greater than the value, minus one gives
            # the index of the (start, end] bin containing the value.
            index = bisect_right(boundaries, value) - 1
        else:
            # Bins are [start, end), so the last boundary itself overflows.
            if value < boundaries[0]:
                return self._underflow_label

            if value >= boundaries[-1]:
                return self._overflow_label

            # First boundary strictly greater than the value, minus one gives
            # the index of the [start, end) bin containing the value.
            index = bisect_right(boundaries, value) - 1

        return self.labels[index]

    def count(self, table, column_name, on_bad='bucket'):
        """
        Count the values of a column against this bin definition.

        Every interior bin appears in the result in a fixed position, even
        when it is empty. Null values are counted separately and never enter
        any numeric bin.

        :param table:
            The :class:`.Table` to count values from.
        :param column_name:
            The name of a :class:`.Number` column to count.
        :param on_bad:
            How to treat values outside of the boundaries. :code:`'bucket'`
            (the default) places them in explicit underflow and overflow
            buckets. :code:`'raise'` raises a :class:`.BinOutOfRangeError`
            instead. :code:`'drop'` discards them.
        :returns:
            A new :class:`.Table` with a :class:`.Text` column of bin labels
            named after the counted column and a :class:`.Number`
            :code:`Count` column.
        """
        if on_bad not in ('bucket', 'raise', 'drop'):
            raise ValueError("on_bad must be one of 'bucket', 'raise' or 'drop'.")

        from agate.table import Table

        column = table.columns[column_name]

        if not isinstance(column.data_type, Number):
            raise ValueError('Bins can only be applied to Number columns.')

        labels = list(self.labels)
        keys = list(labels)
        counts = {label: 0 for label in labels}

        underflow_count = 0
        overflow_count = 0
        null_count = 0

        for value in column.values():
            if value is None:
                null_count += 1
                continue

            label = self.classify(value)

            if label in (self._underflow_label, self._overflow_label):
                if on_bad == 'raise':
                    raise BinOutOfRangeError(
                        f'Value {value} is outside of the bin boundaries '
                        f'[{self._boundaries[0]}, {self._boundaries[-1]}].'
                    )

                if on_bad == 'drop':
                    continue

                if label == self._underflow_label:
                    underflow_count += 1
                else:
                    overflow_count += 1

                continue

            counts[label] += 1

        if on_bad == 'bucket':
            keys.insert(0, self._underflow_label)
            labels.insert(0, self._underflow_label)
            counts[self._underflow_label] = underflow_count

            keys.append(self._overflow_label)
            labels.append(self._overflow_label)
            counts[self._overflow_label] = overflow_count

        keys.append(None)
        rows = [[label, counts.get(label, 0)] for label in labels]
        rows.append([None, null_count])

        return Table(
            rows,
            [column_name, 'Count'],
            [Text(), Number()],
            row_names=[str(key) if key is not None else None for key in keys],
        )
