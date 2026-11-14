from decimal import Decimal
from functools import cmp_to_key

from agate.columns import null_handler
from agate.computations.base import Computation
from agate.data_types import Number


class Rank(Computation):
    """
    Calculate rank order of the values in a column.

    Uses the "competition" ranking method: if there are four values and the
    middle two are tied, then the output will be `[1, 2, 2, 4]`.

    Null values will always be ranked last.

    :param column_name:
        The name of the column to rank.
    :param comparer:
        An optional comparison function. If not specified ranking will be
        ascending, with nulls ranked last.
    :param reverse:
        Reverse sort order before ranking.
    """
    def __init__(self, column_name, comparer=None, reverse=None):
        self._column_name = column_name
        self._comparer = comparer
        self._reverse = reverse

    def get_computed_data_type(self, table):
        return Number()

    def _group_rows(self, table):
        """
        Group the row indices by the equivalence relation used for ranking.

        Each group is a tuple of row indices in their original table order.
        The groups are returned in the direction used to assign ranks. With a
        ``comparer`` two values are equivalent when it returns zero, so the
        comparer fully determines both ordering and equivalence. Without one
        natural equality is used and null values always form the final group,
        regardless of ``reverse``.
        """
        column = table.columns[self._column_name]
        values = column.values()

        if self._comparer:
            ordered_indices = sorted(
                range(len(values)),
                key=cmp_to_key(lambda i, j: self._comparer(values[i], values[j]))
            )

            groups = []

            for i in ordered_indices:
                if groups and self._comparer(values[groups[-1][0]], values[i]) == 0:
                    groups[-1].append(i)
                else:
                    groups.append([i])
        else:
            null_indices = []
            non_null_groups = {}

            for i, value in enumerate(values):
                if value is None:
                    null_indices.append(i)
                elif value in non_null_groups:
                    non_null_groups[value].append(i)
                else:
                    non_null_groups[value] = [i]

            distinct = sorted(non_null_groups, key=null_handler)

            if self._reverse:
                distinct.reverse()

            groups = [non_null_groups[value] for value in distinct]

            if null_indices:
                groups.append(null_indices)

        return [tuple(group) for group in groups]

    def run(self, table):
        """
        :returns:
            :class:`int`
        """
        groups = self._group_rows(table)

        if self._comparer and self._reverse:
            groups = list(reversed(groups))

        ranks = [None] * len(table.rows)
        position = 0

        for group in groups:
            rank = Decimal(position + 1)

            for i in group:
                ranks[i] = rank

            position += len(group)

        return ranks
