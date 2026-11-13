"""Specifications used by dependency-aware table computations."""

from collections.abc import Mapping

from agate.exceptions import (
    CircularDependencyError,
    ComputationDependencyError,
    ComputationError,
    ComputationOutputError,
    DuplicateComputationColumnError,
    MissingDependencyError,
)

__all__ = [
    'AUTO',
    'COMPUTED',
    'FROM_COMPUTED',
    'FROM_ORIGINAL',
    'NEW',
    'ORIGINAL',
    'SOURCE_AUTO',
    'SOURCE_COMPUTED',
    'SOURCE_ORIGINAL',
    'CircularDependencyError',
    'ColumnComputation',
    'ComputationColumn',
    'ComputationDependency',
    'ComputationDependencyError',
    'ComputationDependencySpec',
    'ComputationError',
    'ComputationOutputError',
    'ComputationSpec',
    'ComputeColumn',
    'ComputeSpec',
    'ComputedColumn',
    'Dependency',
    'DependencySpec',
    'DuplicateComputationColumnError',
    'MissingDependencyError',
    'normalize_dependency_source',
]

SOURCE_AUTO = 'auto'
SOURCE_ORIGINAL = 'original'
SOURCE_COMPUTED = 'computed'

ORIGINAL = SOURCE_ORIGINAL
COMPUTED = SOURCE_COMPUTED
NEW = SOURCE_COMPUTED
AUTO = SOURCE_AUTO
FROM_ORIGINAL = SOURCE_ORIGINAL
FROM_COMPUTED = SOURCE_COMPUTED

_SOURCE_ALIASES = {
    'auto': SOURCE_AUTO,
    'default': SOURCE_AUTO,
    'infer': SOURCE_AUTO,
    'original': SOURCE_ORIGINAL,
    'raw': SOURCE_ORIGINAL,
    'source': SOURCE_ORIGINAL,
    'existing': SOURCE_ORIGINAL,
    'before': SOURCE_ORIGINAL,
    'input': SOURCE_ORIGINAL,
    'base': SOURCE_ORIGINAL,
    'computed': SOURCE_COMPUTED,
    'compute': SOURCE_COMPUTED,
    'new': SOURCE_COMPUTED,
    'result': SOURCE_COMPUTED,
    'output': SOURCE_COMPUTED,
    'derived': SOURCE_COMPUTED,
    'current': SOURCE_COMPUTED,
    'replacement': SOURCE_COMPUTED,
    'after': SOURCE_COMPUTED,
}


def normalize_dependency_source(source):
    """
    Normalize a dependency source name to :data:`.ORIGINAL`,
    :data:`.COMPUTED` or :data:`.AUTO`.
    """
    if source is None:
        return None

    if isinstance(source, bool):
        return SOURCE_ORIGINAL if source else SOURCE_COMPUTED

    if not isinstance(source, str):
        value = getattr(source, 'value', None)

        if isinstance(value, str):
            source = value
        elif isinstance(getattr(source, 'name', None), str):
            source = source.name

    if isinstance(source, str):
        normalized = _SOURCE_ALIASES.get(source.strip().lower())

        if normalized is None:
            raise ValueError('Unknown dependency source "%s".' % source)

        return normalized

    raise ValueError('Dependency source must be one of: auto, original, computed.')


def _value(obj, keys, default=None):
    if isinstance(obj, Mapping):
        for key in keys:
            if key in obj:
                return obj[key]

        return default

    for key in keys:
        if hasattr(obj, key):
            return getattr(obj, key)

    return default


class Dependency:
    """
    A single column dependency.

    :param name:
        The name of the column that will be read.
    :param source:
        Whether the value comes from the original table
        (:data:`.ORIGINAL`) or from another computation in the same plan
        (:data:`.COMPUTED`). If omitted, the source is inferred.
    :param alias:
        An optional alternate name under which the value is exposed to the
        computation.
    """
    ORIGINAL = SOURCE_ORIGINAL
    COMPUTED = SOURCE_COMPUTED
    NEW = SOURCE_COMPUTED
    AUTO = SOURCE_AUTO

    def __init__(self, name=None, source=None, alias=None, **kwargs):
        if name is None:
            name = _value(kwargs, ('column_name', 'column', 'target', 'field'))

        if source is None:
            source = _value(
                kwargs,
                ('from_column', 'from', 'from_source', 'read_from', 'kind', 'type',
                 'read', 'reads', 'source')
            )

        if alias is None:
            alias = _value(kwargs, ('as_column', 'as_name', 'as'))

        if (
            kwargs.get('original') is True
            or kwargs.get('read_original') is True
            or kwargs.get('from_original') is True
        ):
            source = ORIGINAL
        elif (
            kwargs.get('new') is True
            or kwargs.get('computed') is True
            or kwargs.get('read_computed') is True
            or kwargs.get('from_computed') is True
        ):
            source = COMPUTED

        if name is None:
            raise ValueError('Dependency requires a column name.')

        self.name = name
        self.source = normalize_dependency_source(source)
        self.alias = alias

    @property
    def column_name(self):
        return self.name

    @property
    def is_original(self):
        return self.source == ORIGINAL

    @property
    def is_computed(self):
        return self.source == COMPUTED

    def __iter__(self):
        return iter((self.name, self.source, self.alias))

    def __repr__(self):
        return 'Dependency(%r, %r)' % (self.name, self.source)


class ComputationSpec:
    """
    Declares one target column, its computation and the columns it reads.

    :param new_column_name:
        The name of the column produced by this computation.
    :param computation:
        A :class:`.Computation` instance.
    :param dependencies:
        A sequence of column names or :class:`.Dependency` instances. If
        omitted, dependencies declared by the computation are used.
    :param replace:
        If :code:`True`, the target may replace an existing column.
    :param data_type:
        An optional explicit type. If omitted, the computation's declared type
        is used.
    """
    def __init__(self, new_column_name=None, computation=None, dependencies=None,
                 replace=False, data_type=None, **kwargs):
        if new_column_name is None:
            new_column_name = _value(
                kwargs,
                ('target_column_name', 'target_column', 'target_name', 'column_name', 'name', 'target')
            )

        if computation is None:
            computation = _value(kwargs, ('compute', 'func'))

        if dependencies is None:
            dependencies = _value(
                kwargs,
                ('depends_on', 'depends', 'deps', 'sources', 'inputs', 'source_columns')
            )

        if data_type is None:
            data_type = _value(kwargs, ('declared_type', 'output_type', 'result_type', 'type'))

        if not replace:
            replace = bool(_value(kwargs, ('replace_existing', 'overwrite'), False))

        if computation is not None and not hasattr(computation, 'run') \
                and hasattr(dependencies, 'run'):
            computation, dependencies = dependencies, computation

        if isinstance(computation, str) and not isinstance(new_column_name, str) \
                and hasattr(new_column_name, 'run'):
            new_column_name, computation = computation, new_column_name

        if new_column_name is None:
            raise ValueError('ComputationSpec requires a target column name.')

        if computation is None:
            raise ValueError('ComputationSpec requires a computation.')

        self.new_column_name = new_column_name
        self.computation = computation
        self.dependencies = dependencies
        self.replace = replace
        self.data_type = data_type

        for name, value in kwargs.items():
            if not hasattr(self, name):
                setattr(self, name, value)

    @property
    def column_name(self):
        return self.new_column_name

    @property
    def name(self):
        return self.new_column_name

    @property
    def target(self):
        return self.new_column_name

    @property
    def target_column(self):
        return self.new_column_name

    @property
    def depends_on(self):
        return self.dependencies

    def __iter__(self):
        return iter((self.new_column_name, self.computation, self.dependencies))

    def __repr__(self):
        return 'ComputationSpec(%r, %r, dependencies=%r, replace=%r)' % (
            self.new_column_name, self.computation, self.dependencies, self.replace
        )


ComputeSpec = ComputationSpec
ComputedColumn = ComputationSpec
ColumnComputation = ComputationSpec
ComputationColumn = ComputationSpec
ComputeColumn = ComputationSpec
ComputationDependencySpec = ComputationSpec
DependencySpec = ComputationSpec
ComputationDependency = Dependency
