from agate.data_types import Boolean
from agate.rows import Row
from agate.table import Table


def _aggregate(self, aggregations=[]):
    """
    Recursive aggregation allowing for TableSet's to be nested inside
    one another.
    """
    from agate.tableset import TableSet

    output = []

    # Process nested TableSet's
    if isinstance(self._values[0], TableSet):
        for key, nested_tableset in self.items():
            column_names, column_types, nested_output, row_name_columns = _aggregate(nested_tableset, aggregations)

            for row in nested_output:
                row.insert(0, key)

                output.append(row)

        column_names.insert(0, self._key_name)
        column_types.insert(0, self._key_type)
        row_name_columns.insert(0, self._key_name)
    # Regular Tables
    else:
        column_names = [self._key_name]
        column_types = [self._key_type]
        row_name_columns = [self._key_name]

        for new_column_name, aggregation in aggregations:
            column_names.append(new_column_name)
            column_types.append(aggregation.get_aggregate_data_type(self._sample_table))

        for name, table in self.items():
            for new_column_name, aggregation in aggregations:
                aggregation.validate(table)

        for name, table in self.items():
            new_row = [name]

            for new_column_name, aggregation in aggregations:
                new_row.append(aggregation.run(table))

            output.append(new_row)

    return column_names, column_types, output, row_name_columns


def _hierarchy(tableset):
    """
    Collect the ``(key_name, key_type)`` pairs of this TableSet and every
    nested TableSet, in outer-to-inner order.
    """
    from agate.tableset import TableSet

    levels = [(tableset._key_name, tableset._key_type)]

    if isinstance(tableset._values[0], TableSet):
        levels.extend(_hierarchy(tableset._values[0]))

    return levels


def _flatten(tableset, prefix=(), key_names=None):
    """
    Flatten a (possibly nested) TableSet into the single table that would
    result from merging every table, with the grouping keys prepended as
    columns. Aggregating this table recomputes rollups from the source rows
    instead of combining already-aggregated values. Source columns that
    share a name with a grouping key are skipped, since that value is already
    represented by the prepended key column.
    """
    from agate.tableset import TableSet

    hierarchy = _hierarchy(tableset)

    if key_names is None:
        key_names = [name for name, data_type in hierarchy]

    rows = []

    def add_row(key_values, source_columns, source_values):
        column_names = list(key_names) + [
            name for name in source_columns if name not in key_names
        ]
        rows.append(Row(key_values + source_values, column_names))

    for key, value in tableset.items():
        key_values = prefix + (key,)

        if isinstance(value, TableSet):
            rows.extend(_flatten(value, key_values, key_names))
        else:
            source_columns = tuple(value.column_names)

            for row in value._rows:
                source_values = tuple(
                    cell for name, cell in zip(source_columns, row)
                    if name not in key_names
                )
                add_row(key_values, source_columns, source_values)

    if prefix:
        return rows

    column_names = list(key_names)
    column_types = [data_type for name, data_type in hierarchy]

    sample_table = tableset._sample_table

    for name, data_type in zip(sample_table.column_names, sample_table.column_types):
        if name not in key_names:
            column_names.append(name)
            column_types.append(data_type)

    return Table(rows, column_names, column_types, _is_fork=True)


def _normalize_grouping_sets(grouping_sets, hierarchy):
    """
    Validate the requested dimension combinations and normalize each one to
    the order in which the dimensions appear in the TableSet hierarchy.

    Duplicate combinations are rejected rather than silently producing
    duplicate rows.
    """
    if grouping_sets is None:
        return None

    if not grouping_sets:
        raise ValueError('grouping_sets must contain at least one dimension combination; use an empty combination '
                         '() for the grand total.')

    dimension_names = [name for name, data_type in hierarchy]

    normalized = []
    seen = set()

    for grouping_set in grouping_sets:
        if isinstance(grouping_set, str) or not hasattr(grouping_set, '__iter__'):
            raise ValueError('Each grouping set must be a sequence of grouping key names.')

        dimensions = tuple(grouping_set)

        for dimension in dimensions:
            if dimension not in dimension_names:
                raise KeyError('Unknown grouping key "%s". Valid grouping keys are: %s.'
                               % (dimension, ', '.join(dimension_names)))

        ordered = tuple(name for name in dimension_names if name in dimensions)

        if ordered in seen:
            raise ValueError('Duplicate grouping set: %s.' % ', '.join(ordered))

        seen.add(ordered)
        normalized.append(ordered)

    return normalized


def _leaf_groups(grouped, key_prefix=()):
    """
    Yield ``(composite_key, table)`` pairs for every leaf table of a
    (possibly nested) grouped :class:`.TableSet`.
    """
    from agate.tableset import TableSet

    for key, value in grouped.items():
        composite_key = key_prefix + (key,)

        if isinstance(value, TableSet):
            yield from _leaf_groups(value, composite_key)
        else:
            yield composite_key, value


def _aggregate_grouping_sets(self, aggregations, grouping_sets):
    hierarchy = _hierarchy(self)
    grouping_sets = _normalize_grouping_sets(grouping_sets, hierarchy)

    dimension_names = [name for name, data_type in hierarchy]
    marker_names = ['grouping_%s' % name for name in dimension_names]

    reserved_names = marker_names + [name for name, aggregation in aggregations]

    if len(set(reserved_names)) != len(reserved_names):
        raise ValueError('Aggregation and grouping column names must be unique and must not collide with the '
                         'grouping marker columns: %s.' % ', '.join(marker_names))

    merged = _flatten(self)

    column_names = list(dimension_names) + marker_names
    column_types = [data_type for name, data_type in hierarchy] + [Boolean()] * len(dimension_names)

    for new_column_name, aggregation in aggregations:
        column_names.append(new_column_name)
        column_types.append(aggregation.get_aggregate_data_type(self._sample_table))

    for new_column_name, aggregation in aggregations:
        aggregation.validate(merged)

    output = []
    row_name_columns = list(dimension_names)

    for dimensions in grouping_sets:
        markers = tuple(name not in dimensions for name in dimension_names)

        if dimensions:
            grouped = merged

            for dimension in dimensions:
                grouped = grouped.group_by(dimension)

            group_tables = _leaf_groups(grouped)
        else:
            # The grand total is always computed, even for empty input, by
            # running each aggregation against the full (possibly empty)
            # table so that its own empty-input semantics apply.
            group_tables = [((), merged)]

        for group_key, group_table in group_tables:
            new_row = [None] * len(dimension_names)

            if dimensions:
                for dimension, value in zip(dimensions, group_key):
                    new_row[dimension_names.index(dimension)] = value

            new_row.extend(markers)

            for new_column_name, aggregation in aggregations:
                new_row.append(aggregation.run(group_table))

            output.append(new_row)

    if len(row_name_columns) == 1:
        row_names = row_name_columns[0]
    else:
        def row_names(r):
            return tuple(r[n] for n in row_name_columns)

    return Table(output, column_names, column_types, row_names=row_names)


def aggregate(self, aggregations, grouping_sets=None):
    """
    Aggregate data from the tables in this set by performing some
    set of column operations on the groups and coalescing the results into
    a new :class:`.Table`.

    :code:`aggregations` must be a sequence of tuples, where each has two
    parts: a :code:`new_column_name` and a :class:`.Aggregation` instance.

    The resulting table will have the keys from this :class:`TableSet` (and
    any nested TableSets) set as its :code:`row_names`. See
    :meth:`.Table.__init__` for more details.

    :param aggregations:
        A list of tuples in the format :code:`(new_column_name, aggregation)`,
        where each :code:`aggregation` is an instance of :class:`.Aggregation`.
    :param grouping_sets:
        An optional sequence of dimension combinations that selects which
        subtotals to compute, similar to SQL's ``GROUPING SETS``. Each
        combination is a sequence of grouping key names (the
        :attr:`.TableSet.key_name` of this set and any nested sets). An
        empty combination :code:`()` requests the grand total over every
        row. When omitted, only the full combination of grouping keys is
        aggregated, preserving the standard behavior.

        Rows are emitted in the order of the grouping sets, and within each
        set in the original order of the groups. For every grouping key a
        boolean :code:`grouping_<key>` column marks the level of the row:
        :code:`True` means the dimension is rolled up for that row (its key
        column is :code:`None` because the dimension is not part of the
        combination), whereas :code:`False` means the key value is a real
        group value, which may itself be :code:`None`.

        Every subtotal is computed by running the aggregations against the
        underlying rows of the combined tables, so a mean of means is never
        taken. The grand total of an empty input is still produced using
        each aggregation's own empty-table result. Duplicate dimension
        combinations raise a :class:`ValueError`, and unknown key names raise
        a :class:`KeyError`.
    :returns:
        A new :class:`.Table`.
    """
    if grouping_sets is None:
        column_names, column_types, output, row_name_columns = _aggregate(self, aggregations)

        if len(row_name_columns) == 1:
            row_names = row_name_columns[0]
        else:
            def row_names(r):
                return tuple(r[n] for n in row_name_columns)

        return Table(output, column_names, column_types, row_names=row_names)

    return _aggregate_grouping_sets(self, aggregations, grouping_sets)
