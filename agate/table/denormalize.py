from collections import OrderedDict
from decimal import Decimal

from agate import utils
from agate.data_types import Number
from agate.exceptions import NormalizationError
from agate.normalization import NormalizationSchema, restore_value
from agate.rows import Row
from agate.type_tester import TypeTester


def denormalize(self, key=None, property_column='property', value_column='value', default_value=utils.default,
                column_types=None, schema=None):
    """
    Create a new table with row values converted into columns.

    For example:

    +---------+-----------+---------+
    |  name   | property  | value   |
    +=========+===========+=========+
    |  Jane   | gender    | female  |
    +---------+-----------+---------+
    |  Jane   | race      | black   |
    +---------+-----------+---------+
    |  Jane   | age       | 24      |
    +---------+-----------+---------+
    |  ...    |  ...      |  ...    |
    +---------+-----------+---------+

    Can be denormalized so that each unique value in `field` becomes a
    column with `value` used for its values.

    +---------+----------+--------+-------+
    |  name   | gender   | race   | age   |
    +=========+==========+========+=======+
    |  Jane   | female   | black  | 24    |
    +---------+----------+--------+-------+
    |  Jack   | male     | white  | 35    |
    +---------+----------+--------+-------+
    |  Joe    | male     | black  | 28    |
    +---------+----------+--------+-------+

    If one or more keys are specified then the resulting table will
    automatically have :code:`row_names` set to those keys.

    This is the opposite of :meth:`.Table.normalize`.

    By default the types of the new columns are inferred. If a
    :class:`.NormalizationSchema` is passed as :code:`schema`--or this
    table was created by :meth:`.Table.normalize` with
    :code:`record_schema=True`, in which case the schema is picked up
    automatically--the original column names, order and types recorded in
    the schema are restored exactly, along with the original row names.
    In this mode the data is validated against the schema: unknown
    properties, duplicate cells, missing columns or values that do not
    match the recorded types raise a :class:`.NormalizationError`. Pass
    :code:`schema=False` to force type inference on a table which carries
    a schema.

    :param key:
        A column name or a sequence of column names that should be
        maintained as they are in the normalized table. Typically these
        are the tables unique identifiers and any metadata about them. Or,
        :code:`None` if there are no key columns.
    :param field_column:
        The column whose values should become column names in the new table.
    :param property_column:
        The column whose values should become the values of the property
        columns in the new table.
    :param default_value:
        Value to be used for missing values in the pivot table. If not
        specified :code:`Decimal(0)` will be used for aggregations that
        return :class:`.Number` data and :code:`None` will be used for
        all others.
    :param column_types:
        A sequence of column types with length equal to number of unique
        values in field_column or an instance of :class:`.TypeTester`.
        Defaults to a generic :class:`.TypeTester`.
    :param schema:
        A :class:`.NormalizationSchema` describing the original table, or
        :code:`False` to force type inference even if this table carries a
        schema. Defaults to the schema attached to this table by
        :meth:`.Table.normalize`, if any.
    :returns:
        A new :class:`.Table`.
    """
    from agate.table import Table

    if schema is None:
        schema = self.normalization_schema

    if schema is False:
        schema = None

    if schema is not None:
        if not isinstance(schema, NormalizationSchema):
            raise NormalizationError('schema must be a NormalizationSchema instance.')

        return _denormalize_with_schema(self, schema, default_value)

    if key is None:
        key = []
    elif not utils.issequence(key):
        key = [key]

    field_names = []
    row_data = OrderedDict()

    for row in self.rows:
        row_key = tuple(row[k] for k in key)

        if row_key not in row_data:
            row_data[row_key] = OrderedDict()

        f = str(row[property_column])
        v = row[value_column]

        if f not in field_names:
            field_names.append(f)

        row_data[row_key][f] = v

    if default_value == utils.default:
        if isinstance(self.columns[value_column].data_type, Number):
            default_value = Decimal(0)
        else:
            default_value = None

    new_column_names = key + field_names

    new_rows = []
    row_names = []

    for k, v in row_data.items():
        row = list(k)

        if len(k) == 1:
            row_names.append(k[0])
        else:
            row_names.append(k)

        for f in field_names:
            if f in v:
                row.append(v[f])
            else:
                row.append(default_value)

        new_rows.append(Row(row, new_column_names))

    key_column_types = [self.column_types[self.column_names.index(name)] for name in key]

    if column_types is None or isinstance(column_types, TypeTester):
        tester = TypeTester() if column_types is None else column_types
        force_update = dict(zip(key, key_column_types))
        force_update.update(tester._force)
        tester._force = force_update

        new_column_types = tester.run(new_rows, new_column_names)
    else:
        new_column_types = key_column_types + list(column_types)

    return Table(new_rows, new_column_names, new_column_types, row_names=row_names)


def _denormalize_with_schema(self, schema, default_value):
    """
    Restore a table created by :meth:`.Table.normalize` with
    ``record_schema=True`` using the recorded :class:`.NormalizationSchema`.
    """
    from agate.table import Table

    required_columns = list(schema.key) + [schema.property_column, schema.value_column]

    missing = [name for name in required_columns if name not in self._column_names]

    if missing:
        raise NormalizationError(
            'This table does not match the normalization schema. Missing columns: %s.' % ', '.join(missing)
        )

    known_properties = set(schema.properties)
    key_indices = {name: i for i, name in enumerate(schema.key)}

    row_data = OrderedDict()

    for row in self._rows:
        row_key = tuple(row[name] for name in schema.key)
        prop = row[schema.property_column]

        if prop not in known_properties:
            raise NormalizationError(
                'Property "%s" is not described by the normalization schema.' % prop
            )

        if row_key not in row_data:
            row_data[row_key] = {}

        cells = row_data[row_key]

        if prop in cells:
            raise NormalizationError(
                'Duplicate cell for key %s and property "%s".' % (row_key, prop)
            )

        cells[prop] = row[schema.value_column]

    if default_value == utils.default:
        default_value = None

    new_column_names = list(schema.column_order)
    new_column_types = [schema.column_types[name] for name in new_column_names]

    new_rows = []

    for row_key, cells in row_data.items():
        values = []

        for name, data_type in zip(new_column_names, new_column_types):
            if name in key_indices:
                value = row_key[key_indices[name]]
            elif name in cells:
                value = cells[name]
            else:
                value = default_value

            values.append(restore_value(value, data_type, name))

        new_rows.append(Row(values, new_column_names))

    if schema.row_names is not None and len(schema.row_names) != len(new_rows):
        raise NormalizationError(
            'The normalization schema describes %i named rows, but the data contains %i rows.' % (
                len(schema.row_names), len(new_rows)
            )
        )

    # The rows already contain Row instances with values restored to their
    # recorded native types, so the table is forked to prevent re-casting
    return Table(new_rows, new_column_names, new_column_types, row_names=schema.row_names, _is_fork=True)
