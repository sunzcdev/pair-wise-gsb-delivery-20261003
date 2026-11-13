import math
import warnings
from decimal import Decimal, InvalidOperation

from agate.aggregations.base import Aggregation
from agate.aggregations.has_nulls import HasNulls
from agate.data_types import Number, TimeDelta
from agate.exceptions import DataTypeError
from agate.utils import Quantiles
from agate.warns import NullCalculationWarning, warn_null_calculation


class Percentiles(Aggregation):
    """
    Divide a column into 100 equal-size groups using the "CDF" method.

    See `this explanation <http://www.amstat.org/publications/jse/v14n3/langford.html>`_
    of the various methods for computing percentiles.

    "Zeroth" (min value) and "Hundredth" (max value) percentiles are included
    for reference and intuitive indexing.

    A reference implementation was provided by
    `pycalcstats <https://code.google.com/p/pycalcstats/>`_.

    This aggregation can not be applied to a :class:`.TableSet`.

    :param column_name:
        The name of a column containing :class:`.Number` or :class:`.TimeDelta` data.
    """
    def __init__(self, column_name):
        self._column_name = column_name

    def validate(self, table):
        column = table.columns[self._column_name]

        if not isinstance(column.data_type, (Number, TimeDelta)):
            raise DataTypeError('Percentiles can only be applied to columns containing Number or TimeDelta data.')

        has_nulls = HasNulls(self._column_name).run(table)

        if has_nulls:
            warn_null_calculation(self, column)

    def run(self, table):
        """
        :returns:
            An instance of :class:`Quantiles`.
        """
        column = table.columns[self._column_name]

        data = column.values_without_nulls_sorted()

        if not data:
            return Quantiles([None for percentile in range(101)])

        # Zeroth percentile is first datum
        quantiles = [data[0]]

        for percentile in range(1, 100):
            k = len(data) * (float(percentile) / 100)

            low = max(1, int(math.ceil(k)))
            high = min(len(data), int(math.floor(k + 1)))

            # No remainder
            if low == high:
                value = data[low - 1]
            # Remainder
            else:
                value = (data[low - 1] + data[high - 1]) / 2

            quantiles.append(value)

        # Hundredth percentile is final datum
        quantiles.append(data[-1])

        return Quantiles(quantiles)


class WeightedQuantile(Aggregation):
    """
    Calculate a discrete quantile of a column using a column of weights.

    Values are sorted and weights for equal values are combined. The result is
    the first value whose cumulative weight reaches the target proportion. No
    linear interpolation is performed.

    Weights must be finite, non-negative :class:`.Number` values. Rows with a
    zero weight are ignored. By default, rows with either a null value or a
    null weight are excluded. Set :code:`missing='error'` to raise a
    :class:`ValueError` instead.

    If there are no usable rows, or if all usable weights are zero, this
    aggregation returns :code:`None`.

    This aggregation is stateless and can be reused when aggregating a
    :class:`.TableSet`.

    :param column_name:
        The name of a column containing :class:`.Number` or :class:`.TimeDelta`
        data.
    :param weight_column_name:
        The name of a column containing :class:`.Number` weights.
    :param quantile:
        The target proportion, between 0 and 1 inclusive. Defaults to 0.5.
        A quantile of 0 returns the minimum usable value. A quantile of 1
        returns the maximum usable value.
    :param missing:
        How to handle a null value or null weight: :code:`'exclude'` (the
        default) or :code:`'error'`.
    """
    _MISSING_POLICIES = {
        'exclude': 'exclude',
        'drop': 'exclude',
        'ignore': 'exclude',
        'omit': 'exclude',
        'skip': 'exclude',
        'error': 'error',
        'raise': 'error',
        'strict': 'error'
    }

    def __init__(self, column_name, weight_column_name, quantile=0.5, missing='exclude'):
        self._column_name = column_name
        self._weight_column_name = weight_column_name
        self._quantile = self._coerce_quantile(quantile)
        self._missing = self._coerce_missing_policy(missing)

    @staticmethod
    def _coerce_quantile(quantile):
        if isinstance(quantile, Decimal):
            value = quantile
        else:
            try:
                value = Decimal(str(quantile))
            except (InvalidOperation, TypeError, ValueError):
                raise ValueError('Quantile must be a number between 0 and 1.')

        if not value.is_finite() or value < 0 or value > 1:
            raise ValueError('Quantile must be a finite number between 0 and 1.')

        return value

    @classmethod
    def _coerce_missing_policy(cls, missing):
        if not isinstance(missing, str):
            raise ValueError("Missing policy must be 'exclude' or 'error'.")

        try:
            return cls._MISSING_POLICIES[missing.lower()]
        except KeyError:
            raise ValueError("Missing policy must be 'exclude' or 'error'.")

    @staticmethod
    def _coerce_weight(weight, row_number):
        if isinstance(weight, Decimal):
            value = weight
        else:
            try:
                value = Decimal(str(weight))
            except (InvalidOperation, TypeError, ValueError):
                raise ValueError(
                    'Weight must be a finite, non-negative number (row {}).'.format(row_number)
                )

        if not value.is_finite() or value < 0:
            raise ValueError(
                'Weight must be a finite, non-negative number (row {}).'.format(row_number)
            )

        return value

    def get_aggregate_data_type(self, table):
        column = table.columns[self._column_name]

        if isinstance(column.data_type, (Number, TimeDelta)):
            return column.data_type

    def _validate_columns(self, table):
        column = table.columns[self._column_name]
        weight_column = table.columns[self._weight_column_name]

        if not isinstance(column.data_type, (Number, TimeDelta)):
            raise DataTypeError(
                'WeightedQuantile can only be applied to columns containing Number or TimeDelta data.'
            )

        if not isinstance(weight_column.data_type, Number):
            raise DataTypeError('WeightedQuantile weights must be contained in a Number column.')

        return column, weight_column

    def _collect(self, table):
        column, weight_column = self._validate_columns(table)
        missing_rows = []
        weights_by_value = {}

        for row_number, (value, weight) in enumerate(zip(column.values(), weight_column.values())):
            if value is None or weight is None:
                if self._missing == 'error':
                    missing_rows.append(row_number)
                continue

            weight = self._coerce_weight(weight, row_number)

            if weight == 0:
                continue

            if value in weights_by_value:
                weights_by_value[value] += weight
            else:
                weights_by_value[value] = weight

        if missing_rows:
            raise ValueError(
                'WeightedQuantile does not allow null values or weights when missing="error" '
                '(rows {}).'.format(', '.join(str(row) for row in missing_rows[:10]))
            )

        return column, weight_column, weights_by_value

    def validate(self, table):
        column, weight_column, _ = self._collect(table)
        values = column.values()
        weights = weight_column.values()

        has_missing = any(value is None or weight is None for value, weight in zip(values, weights))

        if self._missing == 'exclude' and has_missing:
            warnings.warn(
                'Column "{}" or weight column "{}" contains nulls. These will be excluded from '
                'WeightedQuantile calculation.'.format(column.name, weight_column.name),
                NullCalculationWarning,
                stacklevel=2
            )

    def run(self, table):
        """
        :returns:
            The weighted quantile, preserving the value column's data type, or
            :code:`None` if there are no usable rows.
        """
        _, _, weights_by_value = self._collect(table)

        if not weights_by_value:
            return None

        data = sorted(weights_by_value.items(), key=lambda item: item[0])

        if self._quantile == 0:
            return data[0][0]

        if self._quantile == 1:
            return data[-1][0]

        total_weight = sum(weights_by_value.values(), Decimal(0))
        target = total_weight * self._quantile
        cumulative_weight = Decimal(0)

        for value, weight in data:
            cumulative_weight += weight

            if cumulative_weight >= target:
                return value
