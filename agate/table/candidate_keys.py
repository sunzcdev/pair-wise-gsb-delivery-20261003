"""
Candidate key analysis for :class:`.Table`.

This module finds minimal combinations of columns whose values uniquely
identify each row of a table, e.g. in preparation for loading the data into
a database and choosing a primary key.
"""

from collections import OrderedDict, namedtuple
from itertools import combinations

#: A single conflict sample for a combination of columns that is not unique.
#:
#: ``values`` is the tuple of values (aligned with the combination's columns,
#: in the order the columns were passed to the analysis) that appears on more
#: than one row, or that contains a disallowed null. ``row_indices`` holds
#: the positional indices of rows involved in the conflict: two indices for a
#: duplicate value, one index for a null-policy violation.
KeyConflict = namedtuple('KeyConflict', ['values', 'row_indices'])

#: NULL strategies supported by :meth:`.Table.candidate_keys`.
NULL_STRATEGIES = ('fail', 'equal', 'distinct')


class CandidateKeyResult:
    """
    The result of a :meth:`.Table.candidate_keys` analysis.

    Instances of this class are not created directly; they are returned by
    :meth:`.Table.candidate_keys`.
    """
    def __init__(self, keys, conflicts, unchecked):
        self._keys = tuple(keys)
        self._conflicts = conflicts
        self._unchecked = tuple(unchecked)

    def __repr__(self):
        return '<CandidateKeyResult: %i keys, %i conflicting combinations, %s>' % (
            len(self._keys), len(self._conflicts),
            'exhaustive' if self.exhaustive else 'NOT exhaustive'
        )

    @property
    def keys(self):
        """
        A tuple of tuples of column names. Each inner tuple is a minimal
        combination of columns that uniquely identifies every row, ordered by
        the position of the columns in the candidate field list. The keys
        themselves are ordered by width, then by candidate field order.

        No reported key is a subset or superset of another reported key.
        """
        return self._keys

    @property
    def conflicts(self):
        """
        An :class:`~collections.OrderedDict` mapping each checked combination
        that was *not* unique (a tuple of column names) to a tuple of
        :class:`KeyConflict` samples. The number of samples per combination is
        capped by the ``max_conflicts`` argument of the analysis.
        """
        return self._conflicts

    @property
    def unchecked(self):
        """
        A tuple of combinations (tuples of column names) that were not
        evaluated because the work budget (``max_checks``) was exhausted.
        Nothing is claimed about these combinations: they may or may not be
        keys. Combinations skipped because a subset was already found to be a
        key are not included here.
        """
        return self._unchecked

    @property
    def exhaustive(self):
        """
        :code:`True` if every candidate combination was either checked or
        pruned because a subset of it is a key. :code:`False` if the work
        budget was exhausted first, in which case :attr:`unchecked` lists the
        combinations that were never evaluated.
        """
        return not self._unchecked


def _check_combination(rows, indices, null_strategy, max_conflicts):
    """
    Test a single combination of column indices for uniqueness.

    Returns :code:`None` if the combination is unique, otherwise a tuple of
    up to ``max_conflicts`` :class:`KeyConflict` samples.
    """
    seen = {}
    sampled = set()
    samples = []
    failed = False

    for i, row in enumerate(rows):
        values = tuple(row[j] for j in indices)

        if None in values:
            if null_strategy == 'distinct':
                # Nulls are never equal to anything, so this row can not
                # participate in a conflict.
                continue
            elif null_strategy == 'fail':
                failed = True

                if len(samples) < max_conflicts:
                    samples.append(KeyConflict(values, (i,)))

                if len(samples) >= max_conflicts:
                    break

                continue

        # 'equal' strategy, or a row without nulls under any strategy.
        if values in seen:
            failed = True

            if values not in sampled:
                sampled.add(values)

                if len(samples) < max_conflicts:
                    samples.append(KeyConflict(values, (seen[values], i)))

                if len(samples) >= max_conflicts:
                    break
        else:
            seen[values] = i

    if not failed:
        return None

    return tuple(samples)


def candidate_keys(self, columns=None, max_width=None, null_strategy='fail',
                   max_conflicts=3, max_checks=None):
    """
    Find minimal combinations of columns ("candidate keys") whose values
    uniquely identify each row of this table.

    Combinations are tested in order of increasing width. Once a combination
    is found to be a key, none of its supersets are tested or reported, since
    they can not be minimal. A combination that *fails* the uniqueness test
    does not prune any wider combinations.

    The empty combination is never considered. Consequently, on an empty
    table every single candidate column is vacuously unique and is reported
    as a key. Conversely, if the table contains fully duplicated rows, no
    combination of columns can be a key and the result will contain no keys.

    :param columns:
        A sequence of column names to consider as candidate fields, in the
        order they should be tested and reported. If :code:`None`, all of the
        table's columns are used, in table order.
    :param max_width:
        The maximum number of columns in a tested combination. Defaults to
        the number of candidate columns. Values larger than the number of
        candidate columns are clamped.
    :param null_strategy:
        How null values are treated when testing uniqueness:

        * ``'fail'`` (default): any null in a combination disqualifies it as
          a key, matching primary key semantics in most databases.
        * ``'equal'``: nulls are considered equal to each other, so two rows
          with nulls in the same columns conflict like any duplicated value.
        * ``'distinct'``: nulls are considered distinct from one another
          (SQL ``UNIQUE`` semantics), so rows with nulls in the tested
          columns can never cause a conflict.
    :param max_conflicts:
        The maximum number of :class:`KeyConflict` samples recorded per
        failing combination. Must be non-negative.
    :param max_checks:
        The maximum number of combinations to test (the work budget). If the
        budget is exhausted before all combinations have been tested or
        pruned, the remaining combinations are listed in
        :attr:`CandidateKeyResult.unchecked` and
        :attr:`CandidateKeyResult.exhaustive` is :code:`False`. If
        :code:`None` (the default), the budget is unlimited.
    :returns:
        A :class:`CandidateKeyResult`.
    """
    if columns is None:
        names = list(self._column_names)
    else:
        names = list(columns)

        if len(set(names)) != len(names):
            raise ValueError('columns must not contain duplicates.')

        for name in names:
            if name not in self._column_names:
                raise ValueError('Column "%s" does not exist in this table.' % name)

    if null_strategy not in NULL_STRATEGIES:
        raise ValueError('null_strategy must be one of %s.' % (NULL_STRATEGIES,))

    if max_width is None:
        max_width = len(names)
    else:
        if not isinstance(max_width, int) or max_width < 1:
            raise ValueError('max_width must be a positive integer.')

        max_width = min(max_width, len(names))

    if max_conflicts < 0:
        raise ValueError('max_conflicts must be non-negative.')

    if max_checks is not None and max_checks < 0:
        raise ValueError('max_checks must be non-negative or None.')

    indices = [self._column_names.index(name) for name in names]
    rows = list(self._rows)

    keys = []
    key_sets = []
    conflicts = OrderedDict()
    unchecked = []
    checks = 0

    for width in range(1, max_width + 1):
        for combo in combinations(range(len(names)), width):
            combo_set = frozenset(combo)

            # A superset of a known key can not be minimal: prune it. Note
            # that failing combinations never prune anything.
            if any(key_set <= combo_set for key_set in key_sets):
                continue

            combo_names = tuple(names[i] for i in combo)

            if max_checks is not None and checks >= max_checks:
                unchecked.append(combo_names)
                continue

            checks += 1

            samples = _check_combination(
                rows, [indices[i] for i in combo], null_strategy, max_conflicts
            )

            if samples is None:
                keys.append(combo_names)
                key_sets.append(combo_set)
            else:
                conflicts[combo_names] = samples

    return CandidateKeyResult(keys, conflicts, unchecked)
