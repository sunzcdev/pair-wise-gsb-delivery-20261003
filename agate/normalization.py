"""
Explicitly round-trippable wide/long table transformations.

The inference-based :meth:`.Table.normalize` and :meth:`.Table.denormalize`
place values of many different column types into a single value column, so
reversing the transformation can not reliably restore the original column
types. This module provides the metadata half of an explicit, lossless
alternative: a :class:`NormalizationSchema` records the identity, order and
type definitions of the original columns *alongside* the normalized data,
rather than encoding them as special values in data rows.

Only the built-in, describable data types (:class:`.Boolean`,
:class:`.Date`, :class:`.DateTime`, :class:`.Number`, :class:`.Text` and
:class:`.TimeDelta`) are supported out of the box. Custom
:class:`.DataType` subclasses must register a restorer with
:func:`register_data_type` before they can take part in a round trip.
"""

import datetime
from collections import OrderedDict
from decimal import Decimal

from agate.data_types import Boolean, DataType, Date, DateTime, Number, Text, TimeDelta
from agate.exceptions import CastError, NormalizationError

#: Version of the serialized schema format produced by
#: :meth:`NormalizationSchema.to_dict`.
SCHEMA_VERSION = 1


class TypeAdapter:
    """
    Describes how a :class:`.DataType` class participates in explicit
    round trips: how instances are described, how they are restored from a
    description and which Python type their native values have.

    :param name:
        The name used to identify the type in serialized descriptors.
    :param data_type:
        The :class:`.DataType` subclass being adapted.
    :param restore:
        A callable which takes a dictionary of described parameters and
        returns a :class:`.DataType` instance.
    :param describe:
        An optional callable which takes a :class:`.DataType` instance and
        returns a dictionary of JSON-serializable parameters. Defaults to
        no parameters.
    :param python_type:
        An optional type (or tuple of types) which native values of this
        data type are instances of. Used to avoid re-casting values that
        are already of the correct type.
    """
    def __init__(self, name, data_type, restore, describe=None, python_type=None):
        self.name = name
        self.data_type = data_type
        self.restore = restore
        self.describe = describe or (lambda instance: {})
        self.python_type = python_type


#: Registry of type adapters, keyed both by name and by DataType class.
_TYPE_ADAPTERS_BY_NAME = OrderedDict()
_TYPE_ADAPTERS_BY_CLASS = OrderedDict()


def register_data_type(data_type, restore, describe=None, python_type=None, name=None):
    """
    Register a :class:`.DataType` class so that columns using it can take
    part in explicit round-trip transformations.

    Only agate's built-in data types are describable by default. To use a
    custom data type with :meth:`.Table.normalize`'s ``record_schema`` mode
    a restorer must be registered with this function.

    :param data_type:
        The :class:`.DataType` subclass being registered.
    :param restore:
        A callable which takes a dictionary of described parameters and
        returns an instance of ``data_type``. This is the "restorer" used
        to rebuild the type when a schema is deserialized.
    :param describe:
        An optional callable which takes an instance of ``data_type`` and
        returns a dictionary of JSON-serializable parameters that will be
        passed back to ``restore``. Defaults to describing the type with no
        parameters.
    :param python_type:
        An optional type (or tuple of types) which native values of this
        data type are instances of. Values which are already instances of
        this type are passed through unchanged when a table is restored;
        other values are cast using the restored data type.
    :param name:
        The name used to identify the type in serialized descriptors.
        Defaults to the class name.
    """
    if not (isinstance(data_type, type) and issubclass(data_type, DataType)):
        raise ValueError('data_type must be a DataType subclass.')

    if not hasattr(restore, '__call__'):
        raise ValueError('restore must be a callable which returns a DataType instance.')

    adapter = TypeAdapter(name or data_type.__name__, data_type, restore, describe, python_type)

    _TYPE_ADAPTERS_BY_NAME[adapter.name] = adapter
    _TYPE_ADAPTERS_BY_CLASS[data_type] = adapter

    return adapter


def _adapter_for_instance(data_type):
    """
    Look up the adapter for a :class:`.DataType` instance.
    """
    for cls in type(data_type).__mro__:
        if cls in _TYPE_ADAPTERS_BY_CLASS:
            return _TYPE_ADAPTERS_BY_CLASS[cls]

    return None


def describe_data_type(data_type):
    """
    Serialize a :class:`.DataType` instance into a JSON-compatible
    descriptor of the form ``{'name': ..., 'params': {...}}``.

    Raises :class:`.NormalizationError` if the type has not been
    registered. Only agate's built-in types are registered by default;
    custom types must be registered with :func:`register_data_type`.
    """
    adapter = _adapter_for_instance(data_type)

    if adapter is None:
        raise NormalizationError(
            'Columns of type "%s" can not be described. Only built-in data types are supported by default; '
            'register a restorer for custom types with agate.register_data_type.' % type(data_type).__name__
        )

    return {
        'name': adapter.name,
        'params': adapter.describe(data_type)
    }


def restore_data_type(descriptor):
    """
    Rebuild a :class:`.DataType` instance from a descriptor produced by
    :func:`describe_data_type`.

    Raises :class:`.NormalizationError` if the descriptor is malformed or
    names a type for which no restorer has been registered.
    """
    if not isinstance(descriptor, dict) or 'name' not in descriptor:
        raise NormalizationError('Invalid data type descriptor: %r.' % (descriptor,))

    adapter = _TYPE_ADAPTERS_BY_NAME.get(descriptor['name'])

    if adapter is None:
        raise NormalizationError(
            'No restorer has been registered for data type "%s".' % descriptor['name']
        )

    params = descriptor.get('params') or {}

    try:
        restored = adapter.restore(params)
    except NormalizationError:
        raise
    except Exception as e:
        raise NormalizationError(
            'Could not restore data type "%s": %s' % (descriptor['name'], e)
        )

    if not isinstance(restored, DataType):
        raise NormalizationError(
            'The restorer registered for data type "%s" did not return a DataType instance.' % descriptor['name']
        )

    return restored


def python_type_for(data_type):
    """
    Return the Python type (or tuple of types) that native values of the
    given :class:`.DataType` instance are expected to be, or :code:`None`
    if it is not known.
    """
    adapter = _adapter_for_instance(data_type)

    if adapter is None:
        return None

    return adapter.python_type


def restore_value(value, data_type, column_name=None):
    """
    Restore a single value to the native type described by ``data_type``.

    Values which are :code:`None` or already instances of the type's
    registered Python type are returned unchanged. All other values are
    cast using the data type. If the cast fails a
    :class:`.NormalizationError` is raised, since the data does not match
    the recorded metadata.
    """
    if value is None:
        return None

    python_type = python_type_for(data_type)

    if python_type is not None:
        # datetime.datetime is a subclass of datetime.date, but a datetime
        # is not a native value of a Date column
        if isinstance(value, python_type) and not (
            python_type is datetime.date and isinstance(value, datetime.datetime)
        ):
            return value
    elif not isinstance(value, str):
        # For types without a known python_type (unregistered custom types
        # can not occur, but registered ones may omit it) non-string values
        # are trusted to already be native
        return value

    try:
        return data_type.cast(value)
    except CastError as e:
        where = ' for column "%s"' % column_name if column_name is not None else ''
        raise NormalizationError(
            'Value %r%s does not match the recorded type %s: %s' % (
                value, where, type(data_type).__name__, e
            )
        )


class NormalizationSchema:
    """
    Metadata required to exactly reverse a wide-to-long transformation
    performed by :meth:`.Table.normalize` with ``record_schema=True``.

    The schema records the identity, order and type definitions of the
    original columns separately from the data, so that
    :meth:`.Table.denormalize` can restore the original column types and
    exact cell values. It also records the original row names, if any.

    :param key:
        A sequence of column names that were kept as-is in the normalized
        table.
    :param properties:
        A sequence of column names that were converted into property rows,
        in the order they were normalized.
    :param column_types:
        A dictionary mapping each key and property column name to the
        :class:`.DataType` instance of the original column.
    :param column_order:
        The order of the columns in the original table, restricted to the
        key and property columns. Defaults to the key columns followed by
        the property columns.
    :param property_column:
        The name of the column in the normalized table which contains the
        property names.
    :param value_column:
        The name of the column in the normalized table which contains the
        property values.
    :param row_names:
        The row names of the original table, or :code:`None` if it did not
        have any.
    """
    def __init__(self, key, properties, column_types, column_order=None,
                 property_column='property', value_column='value', row_names=None):
        self.key = tuple(key)
        self.properties = tuple(properties)
        self.column_types = dict(column_types)
        self.column_order = tuple(column_order) if column_order is not None else self.key + self.properties
        self.property_column = property_column
        self.value_column = value_column
        self.row_names = tuple(row_names) if row_names is not None else None

        expected = set(self.key) | set(self.properties)

        if set(self.column_order) != expected or len(self.column_order) != len(expected):
            raise NormalizationError(
                'column_order must contain each key and property column exactly once.'
            )

        missing_types = [name for name in self.column_order if name not in self.column_types]

        if missing_types:
            raise NormalizationError(
                'column_types is missing definitions for columns: %s.' % ', '.join(missing_types)
            )

        for name, data_type in self.column_types.items():
            if not isinstance(data_type, DataType):
                raise NormalizationError(
                    'column_types entry for "%s" is not a DataType instance.' % name
                )

    def to_dict(self):
        """
        Serialize this schema to a JSON-compatible dictionary. Column type
        definitions are described using :func:`describe_data_type`, so only
        registered (built-in or user-registered) data types are supported.
        """
        return {
            'version': SCHEMA_VERSION,
            'key': list(self.key),
            'properties': list(self.properties),
            'column_order': list(self.column_order),
            'column_types': {
                name: describe_data_type(data_type)
                for name, data_type in self.column_types.items()
            },
            'property_column': self.property_column,
            'value_column': self.value_column,
            'row_names': list(self.row_names) if self.row_names is not None else None
        }

    @classmethod
    def from_dict(cls, d):
        """
        Rebuild a :class:`NormalizationSchema` from a dictionary produced
        by :meth:`to_dict`.

        Raises :class:`.NormalizationError` if the dictionary is malformed
        or a column type can not be restored.
        """
        if not isinstance(d, dict):
            raise NormalizationError('A normalization schema must be a dictionary.')

        required = ('key', 'properties', 'column_order', 'column_types', 'property_column', 'value_column')

        missing = [name for name in required if name not in d]

        if missing:
            raise NormalizationError(
                'Normalization schema is missing required entries: %s.' % ', '.join(missing)
            )

        version = d.get('version')

        if version != SCHEMA_VERSION:
            raise NormalizationError(
                'Unsupported normalization schema version: %r.' % (version,)
            )

        column_types = {}

        for name, descriptor in d['column_types'].items():
            column_types[name] = restore_data_type(descriptor)

        return cls(
            key=d['key'],
            properties=d['properties'],
            column_types=column_types,
            column_order=d['column_order'],
            property_column=d['property_column'],
            value_column=d['value_column'],
            row_names=d.get('row_names')
        )

    def __eq__(self, other):
        if not isinstance(other, NormalizationSchema):
            return NotImplemented

        return (
            self.key == other.key and
            self.properties == other.properties and
            self.column_order == other.column_order and
            self.property_column == other.property_column and
            self.value_column == other.value_column and
            self.row_names == other.row_names and
            {name: describe_data_type(dt) for name, dt in self.column_types.items()} ==
            {name: describe_data_type(dt) for name, dt in other.column_types.items()}
        )

    def __repr__(self):
        return '<NormalizationSchema: key=%s properties=%s>' % (
            ', '.join(self.key), ', '.join(self.properties)
        )


def _describe_text(data_type):
    return {
        'cast_nulls': data_type.cast_nulls,
        'null_values': list(data_type.null_values)
    }


def _describe_number(data_type):
    return {
        'locale': str(data_type.locale),
        'group_symbol': data_type.group_symbol,
        'decimal_symbol': data_type.decimal_symbol,
        'currency_symbols': list(data_type.currency_symbols),
        'no_leading_zeroes': data_type.no_leading_zeroes,
        'null_values': list(data_type.null_values)
    }


def _describe_boolean(data_type):
    return {
        'true_values': list(data_type.true_values),
        'false_values': list(data_type.false_values),
        'null_values': list(data_type.null_values)
    }


def _describe_date(data_type):
    return {
        'date_format': data_type.date_format,
        'locale': data_type.locale,
        'null_values': list(data_type.null_values)
    }


def _describe_datetime(data_type):
    timezone = data_type.timezone

    return {
        'datetime_format': data_type.datetime_format,
        'timezone': str(timezone) if timezone is not None else None,
        'locale': data_type.locale,
        'null_values': list(data_type.null_values)
    }


def _restore_datetime(params):
    params = dict(params)
    timezone = params.get('timezone')

    if isinstance(timezone, str):
        try:
            from zoneinfo import ZoneInfo
            params['timezone'] = ZoneInfo(timezone)
        except Exception:
            raise NormalizationError(
                'Could not restore timezone "%s" of a DateTime column.' % timezone
            )

    return DateTime(**params)


def _describe_time_delta(data_type):
    return {
        'null_values': list(data_type.null_values)
    }


register_data_type(
    Boolean,
    restore=lambda params: Boolean(**params),
    describe=_describe_boolean,
    python_type=bool
)
register_data_type(
    Number,
    restore=lambda params: Number(**params),
    describe=_describe_number,
    python_type=Decimal
)
register_data_type(
    TimeDelta,
    restore=lambda params: TimeDelta(**params),
    describe=_describe_time_delta,
    python_type=datetime.timedelta
)
register_data_type(
    Date,
    restore=lambda params: Date(**params),
    describe=_describe_date,
    python_type=datetime.date
)
register_data_type(
    DateTime,
    restore=_restore_datetime,
    describe=_describe_datetime,
    python_type=datetime.datetime
)
register_data_type(
    Text,
    restore=lambda params: Text(**params),
    describe=_describe_text,
    python_type=str
)
