from __future__ import absolute_import, print_function, division


import math
from decimal import localcontext


from petl.compat import Decimal, integer_types, numeric_types
from petl.util.base import Table, asindices


def standardize(table, fields, newfields=None, ddof=0, nonfinite='ignore',
                missing=None):
    """
    Standardize numeric values under one or more fields to have a mean of 0
    and a standard deviation of 1. E.g.::

        >>> import petl as etl
        >>> table1 = [['id', 'score'],
        ...           [1, 10],
        ...           [2, 20],
        ...           [3, 30]]
        >>> table2 = etl.standardize(table1, 'score')
        >>> table2.values('score').list()
        [-1.224744871391589, 0.0, 1.224744871391589]

    The `fields` argument can select one field or a list or tuple of fields.
    Field names and indexes are both supported. By default, standardized
    values replace the selected fields. Use `newfields` to append the values
    under new field names instead. The number of `newfields` must match the
    number of selected fields.

    Use `ddof` to set the delta degrees of freedom used when calculating the
    standard deviation. The default, 0, uses the population standard
    deviation; use 1 for the sample standard deviation.

    Non-numeric values, including ``None`` and booleans, are always passed
    through unchanged and never contribute to the statistics. Non-finite
    numbers (NaN and infinities, as ``float`` or ``Decimal``) are handled
    according to the `nonfinite` argument:

    - ``'ignore'`` (default): excluded from the mean, standard deviation,
      valid sample count and `ddof` check, and passed through unchanged.
    - ``'reject'``: raise a ``ValueError`` identifying the field and the
      data row where the value occurs.
    - ``'replace'``: excluded from the statistics like ``'ignore'``, and
      replaced with the `missing` value in the output.

    Constant fields are standardized to 0.0.

    Note that the source table is materialized before values are returned.

    """

    return StandardizeView(table, fields, newfields=newfields, ddof=ddof,
                           nonfinite=nonfinite, missing=missing)


Table.standardize = standardize


class StandardizeView(Table):

    def __init__(self, source, fields, newfields=None, ddof=0,
                 nonfinite='ignore', missing=None):
        self.source = source
        self.fields = fields
        self.newfields = newfields
        self.ddof = ddof
        self.nonfinite = nonfinite
        self.missing = missing

    def __iter__(self):
        return iterstandardize(self.source, self.fields, self.newfields,
                               self.ddof, self.nonfinite, self.missing)


NONFINITE_POLICIES = ('ignore', 'reject', 'replace')


def iterstandardize(source, fields, newfields, ddof, nonfinite, missing):
    if (not isinstance(ddof, integer_types) or isinstance(ddof, bool) or
            ddof < 0):
        raise ValueError('ddof must be a non-negative integer')
    if nonfinite not in NONFINITE_POLICIES:
        raise ValueError("nonfinite must be one of 'ignore', 'reject' or "
                         "'replace'")

    rows = list(iter(source))
    if not rows:
        return

    hdr = rows[0]
    data = rows[1:]
    indices = asindices(hdr, fields)

    if newfields is not None:
        newfields = _as_list(newfields)
        if len(newfields) != len(indices):
            raise ValueError('newfields must be the same length as fields')
        outhdr = tuple(hdr) + tuple(newfields)
    else:
        outhdr = tuple(hdr)

    stats = []
    for index in indices:
        values = []
        for rownum, row in enumerate(data, start=1):
            if not _has_index(row, index):
                continue
            value = row[index]
            if _is_finite_number(value):
                values.append(value)
            elif _is_number(value):
                _handle_nonfinite(nonfinite, hdr, index, rownum, value)
        stats.append(_mean_std(values, ddof))

    yield outhdr

    for row in data:
        outrow = list(row)
        standardized = []
        for index, field_stats in zip(indices, stats):
            if _has_index(row, index):
                value = row[index]
            else:
                value = None
            if _is_finite_number(value):
                value = _standardize_value(value, field_stats)
            elif _is_number(value) and nonfinite == 'replace':
                value = missing
            standardized.append(value)

        if newfields is None:
            for index, value in zip(indices, standardized):
                if _has_index(outrow, index):
                    outrow[index] = value
        else:
            outrow.extend(standardized)
        yield tuple(outrow)


def _as_list(value):
    if isinstance(value, (list, tuple)):
        return list(value)
    return [value]


def _is_number(value):
    return (isinstance(value, numeric_types) and
            not isinstance(value, bool))


def _is_finite_number(value):
    if not _is_number(value):
        return False
    if isinstance(value, Decimal):
        return value.is_finite()
    if isinstance(value, float):
        return not math.isnan(value) and not math.isinf(value)
    return True


def _has_index(row, index):
    return -len(row) <= index < len(row)


def _handle_nonfinite(nonfinite, hdr, index, rownum, value):
    if nonfinite == 'reject':
        raise ValueError('non-finite value %r in field %r at data row %d'
                         % (value, hdr[index], rownum))


def _as_decimal(value):
    if isinstance(value, Decimal):
        return value
    if isinstance(value, integer_types):
        return Decimal(value)
    return Decimal(repr(value))


def _decimal_precision(values):
    adjusted = max(value.adjusted() for value in values)
    exponent = min(value.as_tuple().exponent for value in values)
    return max(28, adjusted - exponent + 3)


def _mean_std(values, ddof):
    if not values:
        return None
    divisor = len(values) - ddof
    if divisor <= 0:
        raise ValueError('ddof must be less than the number of numeric values')
    values = [_as_decimal(value) for value in values]
    precision = _decimal_precision(values)
    with localcontext() as context:
        context.prec = precision
        origin = values[0]
        offsets = [value - origin for value in values]
        mean = sum(offsets) / len(offsets)
        variance = sum((value - mean) ** 2
                       for value in offsets) / divisor
        std = variance.sqrt()
    return origin, mean, std, precision


def _standardize_value(value, stats):
    origin, mean, std, precision = stats
    if std == 0:
        return 0.0
    with localcontext() as context:
        context.prec = precision
        return float((_as_decimal(value) - origin - mean) / std)
