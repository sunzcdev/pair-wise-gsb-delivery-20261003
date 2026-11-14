import math
from decimal import Decimal

from agate.aggregations.percentiles import Percentiles
from agate.computations.rank import Rank
from agate.data_types import Number
from agate.exceptions import DataTypeError
from agate.utils import Quantiles


def _percentiles_for_order(values_sorted):
    """
    Build the 101 percentile boundary values for an already-sorted sequence.

    Mirrors :class:`.Percentiles` (the "CDF" method) but accepts a sequence
    sorted by an arbitrary comparer instead of relying on natural ordering.
    """
    data = values_sorted
    quantiles = [data[0]]

    for percentile in range(1, 100):
        k = len(data) * (float(percentile) / 100)

        low = max(1, int(math.ceil(k)))
        high = min(len(data), int(math.floor(k + 1)))

        if low == high:
            value = data[low - 1]
        else:
            value = (data[low - 1] + data[high - 1]) / 2

        quantiles.append(value)

    quantiles.append(data[-1])

    return Quantiles(quantiles)


def _locate(comparer, quantiles, value):
    """
    Locate ``value`` within ``quantiles`` using ``comparer``.

    This is the comparer-aware equivalent of :meth:`.Quantiles.locate`, so
    values the comparer considers equivalent resolve to the same percentile.
    """
    i = 0

    if comparer(value, quantiles[0]) < 0:
        raise ValueError('Value is less than minimum quantile value.')

    if comparer(value, quantiles[-1]) > 0:
        raise ValueError('Value is greater than maximum quantile value.')

    if comparer(value, quantiles[-1]) == 0:
        return Decimal(len(quantiles) - 1)

    while comparer(value, quantiles[i + 1]) >= 0:
        i += 1

    return Decimal(i)


class PercentileRank(Rank):
    """
    Calculate the percentile into which each value falls.

    See :class:`.Percentiles` for implementation details.

    Equivalent values share a percentile, using the same equivalence relation
    as :class:`.Rank`. Without a ``comparer`` natural equality is used and null
    values produce a null percentile. With a ``comparer`` two values are
    equivalent when it returns zero, including null values when the comparer
    defines them as equivalent to something else.

    :param column_name:
        The name of a column containing the :class:`.Number` values.
    :param comparer:
        An optional comparison function. If provided it determines both the
        ordering and which values are equivalent for ranking purposes.
    :param reverse:
        Reverse the ranking direction before assigning percentiles.
    """
    def __init__(self, column_name, comparer=None, reverse=None):
        super().__init__(column_name, comparer=comparer, reverse=reverse)

    def validate(self, table):
        column = table.columns[self._column_name]

        if not isinstance(column.data_type, Number):
            raise DataTypeError('PercentileRank column must contain Number data.')

    def run(self, table):
        """
        :returns:
            :class:`int`
        """
        groups = self._group_rows(table)
        values = table.columns[self._column_name].values()
        percentile_ranks = [None] * len(values)

        if self._comparer:
            if groups:
                ordered_groups = list(reversed(groups)) if self._reverse else groups

                # Boundaries follow the ranking direction: with reverse they
                # are built from the reversed groups, so the leading groups
                # earn the lowest percentile without splitting tied values.
                quantiles = _percentiles_for_order([values[group[0]] for group in ordered_groups])
                locate_comparer = (
                    (lambda x, y: -self._comparer(x, y)) if self._reverse else self._comparer
                )

                for group in ordered_groups:
                    percentile = _locate(locate_comparer, quantiles, values[group[0]])

                    for i in group:
                        percentile_ranks[i] = percentile

            return percentile_ranks

        percentiles = Percentiles(self._column_name).run(table)

        for group in groups:
            representative = values[group[0]]

            if representative is None:
                continue

            percentile = percentiles.locate(representative)

            for i in group:
                percentile_ranks[i] = percentile

        return percentile_ranks
