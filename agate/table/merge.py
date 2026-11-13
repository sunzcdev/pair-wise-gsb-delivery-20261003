from collections import OrderedDict

from agate.data_types.base import DataType
from agate.exceptions import CastError, DataTypeError
from agate.rows import Row


@classmethod
def merge(cls, tables, row_names=None, column_names=None, strict_types=False, type_overrides=None):
    """
    Create a new table from a sequence of similar tables.

    This method will not carry over row names from the merged tables, but new
    row names can be specified with the :code:`row_names` argument.

    It is possible to limit the columns included in the new :class:`.Table`
    with :code:`column_names` argument. For example, to only include columns
    from a specific table, set :code:`column_names` equal to
    :code:`table.column_names`.

    By default, columns with the same name are only required to use the same
    :class:`.DataType` class, and the type instance from the first table is
    reused for the merged column. Set :code:`strict_types` to :code:`True` to
    instead enforce a strict type contract: the configuration settings which
    affect parsing and output semantics must also match. If they do not, a
    :class:`.DataTypeError` is raised identifying the tables, the column and
    the conflicting settings, and no instance is silently preferred. If a
    data type can not determine whether its configurations are compatible,
    this is reported as unknown.

    Conflicts can be resolved explicitly with :code:`type_overrides`, which
    designates a target :class:`.DataType` for a column. Existing values in
    the column are converted to the target type. If any value can not be
    converted, a :class:`.CastError` is raised and no partially merged table
    is returned.

    :param tables:
        An sequence of :class:`.Table` instances.
    :param row_names:
        See :class:`.Table` for the usage of this parameter.
    :param column_names:
        A sequence of column names to include in the new :class:`.Table`. If
        not specified, all distinct column names from `tables` are included.
    :param strict_types:
        If :code:`True`, columns with the same name must also use
        semantically compatible :class:`.DataType` configurations, not merely
        the same class.
    :param type_overrides:
        An optional dict mapping column names to :class:`.DataType`
        instances. The specified type is used for the merged column and
        existing values are explicitly converted to it.
    :returns:
        A new :class:`.Table`.
    """
    from agate.table import Table

    if type_overrides is None:
        type_overrides = {}
    else:
        for column_name, column_type in type_overrides.items():
            if not isinstance(column_type, DataType):
                raise DataTypeError('Type override for column "%s" is not a DataType instance.' % column_name)

    new_columns = OrderedDict()
    column_tables = {}
    cast_targets = {}

    for table_index, table in enumerate(tables):
        for i in range(0, len(table.columns)):
            if column_names is None or table.column_names[i] in column_names:
                column_name = table.column_names[i]
                column_type = table.column_types[i]

                if column_name in type_overrides:
                    if column_name not in new_columns:
                        new_columns[column_name] = type_overrides[column_name]
                        column_tables[column_name] = table_index
                        cast_targets[column_name] = type_overrides[column_name]

                    continue

                if column_name in new_columns:
                    existing_type = new_columns[column_name]

                    if not isinstance(column_type, type(existing_type)):
                        raise DataTypeError('Tables contain columns with the same names, but different types.')

                    if strict_types:
                        if type(column_type) is not type(existing_type):
                            raise DataTypeError(
                                'Column "%s" uses different DataType classes in tables %i and %i: %s and %s.' % (
                                    column_name, column_tables[column_name], table_index,
                                    type(existing_type).__name__, type(column_type).__name__
                                )
                            )

                        conflicts = existing_type.check_compatible(column_type)

                        if conflicts is None:
                            raise DataTypeError(
                                'Column "%s" in tables %i and %i uses %s instances whose compatibility is '
                                'unknown. Specify a target type with the type_overrides argument to merge '
                                'them explicitly.' % (
                                    column_name, column_tables[column_name], table_index,
                                    type(existing_type).__name__
                                )
                            )

                        if conflicts:
                            details = '; '.join(
                                '%s (%r != %r)' % (name, values[0], values[1])
                                for name, values in conflicts.items()
                            )

                            raise DataTypeError(
                                'Column "%s" has incompatible %s configurations in tables %i and %i: %s. '
                                'Specify a target type with the type_overrides argument to resolve the '
                                'conflict.' % (
                                    column_name, type(existing_type).__name__,
                                    column_tables[column_name], table_index, details
                                )
                            )
                else:
                    new_columns[column_name] = column_type
                    column_tables[column_name] = table_index

    unused_overrides = set(type_overrides) - set(new_columns)

    if unused_overrides:
        raise DataTypeError(
            'Type overrides specified for columns not present in the merged tables: %s.' % (
                ', '.join(sorted(unused_overrides))
            )
        )

    column_keys = tuple(new_columns.keys())
    column_types = tuple(new_columns.values())

    rows = []

    for table_index, table in enumerate(tables):
        # Performance optimization for identical table structures
        if table.column_names == column_keys and table.column_types == column_types:
            rows.extend(table.rows)
        else:
            table_types = dict(zip(table.column_names, table.column_types))

            for row in table.rows:
                data = []

                for column_key in column_keys:
                    value = row.get(column_key, None)
                    target_type = cast_targets.get(column_key)

                    if target_type is not None and value is not None and table_types.get(column_key) is not target_type:
                        try:
                            value = target_type.cast(value)
                        except CastError:
                            raise CastError(
                                'Can not convert value %r in column "%s" from table %i to the specified '
                                'type override.' % (value, column_key, table_index)
                            )

                    data.append(value)

                rows.append(Row(data, column_keys))

    return Table(rows, column_keys, column_types, row_names=row_names, _is_fork=True)
