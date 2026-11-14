from agate import utils
from agate.exceptions import NormalizationError
from agate.normalization import NormalizationSchema, describe_data_type
from agate.rows import Row
from agate.type_tester import TypeTester


def normalize(self, key, properties, property_column='property', value_column='value', column_types=None,
              record_schema=False):
    """
    Create a new table with columns converted into rows values.

    For example:

    +---------+----------+--------+-------+
    |  name   | gender   | race   | age   |
    +=========+==========+========+=======+
    |  Jane   | female   | black  | 24    |
    +---------+----------+--------+-------+
    |  Jack   | male     | white  | 35    |
    +---------+----------+--------+-------+
    |  Joe    | male     | black  | 28    |
    +---------+----------+--------+-------+

    can be normalized on columns 'gender', 'race' and 'age':

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

    This is the opposite of :meth:`.Table.denormalize`.

    By default the types of the property and value columns are inferred,
    which means that denormalizing the result may not restore the original
    column types. If :code:`record_schema` is :code:`True` the identity,
    order and type definitions of the original columns are instead recorded
    in a :class:`.NormalizationSchema`, which is attached to the new table
    as :attr:`.Table.normalization_schema` and used automatically by
    :meth:`.Table.denormalize` to restore the original column types and
    exact cell values. In this mode the values placed in the value column
    are kept as-is (they are not cast to the inferred type of the value
    column) so that no information is lost. Only agate's built-in data
    types are describable; columns using custom types require a restorer
    registered with :func:`.register_data_type`.

    :param key:
        A column name or a sequence of column names that should be
        maintained as they are in the normalized self. Typically these
        are the tables unique identifiers and any metadata about them.
    :param properties:
        A column name or a sequence of column names that should be
        converted to properties in the new self.
    :param property_column:
        The name to use for the column containing the property names.
    :param value_column:
        The name to use for the column containing the property values.
    :param column_types:
        A sequence of two column types for the property and value column in
        that order or an instance of :class:`.TypeTester`. Defaults to a
        generic :class:`.TypeTester`.
    :param record_schema:
        If :code:`True`, record a :class:`.NormalizationSchema` describing
        the original columns so the transformation can be exactly reversed
        by :meth:`.Table.denormalize`.
    :returns:
        A new :class:`.Table`.
    """
    from agate.table import Table

    if not utils.issequence(key):
        key = [key]
    else:
        key = list(key)

    if not utils.issequence(properties):
        properties = [properties]
    else:
        properties = list(properties)

    if record_schema:
        _validate_schema_mode(self, key, properties, property_column, value_column)

    new_rows = []

    new_column_names = key + [property_column, value_column]

    for row in self._rows:
        left_row = [row[n] for n in key]

        for f in properties:
            new_rows.append(Row((left_row + [f, row[f]]), new_column_names))

    key_column_types = [self._column_types[self._column_names.index(name)] for name in key]

    if column_types is None or isinstance(column_types, TypeTester):
        tester = TypeTester() if column_types is None else column_types
        force_update = dict(zip(key, key_column_types))
        force_update.update(tester._force)
        tester._force = force_update

        new_column_types = tester.run(new_rows, new_column_names)
    else:
        new_column_types = key_column_types + list(column_types)

    if not record_schema:
        return Table(new_rows, new_column_names, new_column_types)

    # In schema mode the rows already contain Row instances with native
    # values, so the table is forked to prevent the values in the value
    # column from being cast to its inferred type
    new_table = Table(new_rows, new_column_names, new_column_types, _is_fork=True)

    column_order = [n for n in self._column_names if n in key or n in properties]

    new_table._normalization_schema = NormalizationSchema(
        key=key,
        properties=properties,
        column_types={n: self._column_types[self._column_names.index(n)] for n in column_order},
        column_order=column_order,
        property_column=property_column,
        value_column=value_column,
        row_names=self._row_names
    )

    return new_table


def _validate_schema_mode(self, key, properties, property_column, value_column):
    """
    Validate that a table can be normalized in ``record_schema`` mode.
    """
    for name in key:
        if name not in self._column_names:
            raise NormalizationError('Key column "%s" does not exist in this table.' % name)

    for name in properties:
        if name not in self._column_names:
            raise NormalizationError('Property column "%s" does not exist in this table.' % name)

    if len(set(key)) != len(key):
        raise NormalizationError('Key column names must be unique.')

    if len(set(properties)) != len(properties):
        raise NormalizationError('Property column names must be unique.')

    overlap = set(key) & set(properties)

    if overlap:
        raise NormalizationError(
            'Columns can not be both keys and properties: %s.' % ', '.join(sorted(overlap))
        )

    if property_column == value_column:
        raise NormalizationError('The property and value columns must have different names.')

    for name in (property_column, value_column):
        if name in key:
            raise NormalizationError(
                'The column name "%s" is already used by a key column.' % name
            )

    for name in key + properties:
        data_type = self._column_types[self._column_names.index(name)]

        # Raises NormalizationError if the type is not describable
        describe_data_type(data_type)
