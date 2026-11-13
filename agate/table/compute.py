import datetime
import inspect
from collections import OrderedDict
from collections.abc import Mapping, Sequence
from copy import copy
from decimal import Decimal
from heapq import heappop, heappush

from agate.computations.spec import (
    AUTO,
    COMPUTED,
    ORIGINAL,
    CircularDependencyError,
    ComputationOutputError,
    ComputationSpec,
    Dependency,
    DuplicateComputationColumnError,
    MissingDependencyError,
    normalize_dependency_source,
)
from agate.data_types import Boolean, DataType, Date, DateTime, Number, Text, TimeDelta
from agate.rows import Row


def compute(self, computations, replace=False):
    """
    Create a new table by applying one or more :class:`.Computation` instances
    to each row.

    :param computations:
        A sequence of pairs of new column names and :class:`.Computation`
        instances.
    :param replace:
        If :code:`True` then new column names can match existing names, and
        those columns will be replaced with the computed data.
    :returns:
        A new :class:`.Table`.
    """
    column_names = list(copy(self._column_names))
    column_types = list(copy(self._column_types))

    for new_column_name, computation in computations:
        new_column_type = computation.get_computed_data_type(self)

        if new_column_name in column_names:
            if not replace:
                raise ValueError(
                    'New column name "%s" already exists. Specify replace=True to replace with computed data.'
                )

            i = column_names.index(new_column_name)
            column_types[i] = new_column_type
        else:
            column_names.append(new_column_name)
            column_types.append(new_column_type)

        computation.validate(self)

    new_columns = OrderedDict()

    for new_column_name, computation in computations:
        new_columns[new_column_name] = computation.run(self)

    new_rows = []

    for i, row in enumerate(self._rows):
        # Slow version if using replace
        if replace:
            values = []

            for j, column_name in enumerate(column_names):
                if column_name in new_columns:
                    values.append(new_columns[column_name][i])
                else:
                    values.append(row[j])
        # Faster version if not using replace
        else:
            values = row.values() + tuple(c[i] for c in new_columns.values())

        new_rows.append(Row(values, column_names))

    return self._fork(new_rows, column_names, column_types)


def _spec_value(spec, names, default=None):
    if isinstance(spec, Mapping):
        for name in names:
            if name in spec:
                return spec[name]

        return default

    for name in names:
        if hasattr(spec, name):
            return getattr(spec, name)

    return default


def _is_computation(value):
    return hasattr(value, 'run') and hasattr(value, 'get_computed_data_type')


def _call_dependencies(computation, table=None):
    method = getattr(computation, 'get_dependencies', None)

    if not callable(method):
        return None

    try:
        parameters = list(inspect.signature(method).parameters.values())
    except (TypeError, ValueError):
        parameters = []

    if parameters:
        return method(table)

    return method()


def _computation_dependencies(computation, table=None):
    dependencies = _call_dependencies(computation, table)

    if dependencies is not None:
        return dependencies

    for name in ('_dependencies', 'dependencies', 'depends_on', 'deps', 'inputs', 'source_columns'):
        dependencies = getattr(computation, name, None)

        if dependencies is not None and not callable(dependencies):
            return dependencies

    return ()


def _normalize_dependency_list(dependencies):
    if dependencies is None:
        return []

    if isinstance(dependencies, (Dependency, str)):
        return [dependencies]

    if isinstance(dependencies, Mapping):
        name_keys = ('name', 'column', 'column_name', 'target', 'field')

        if any(key in dependencies for key in name_keys):
            return [dependencies]

        result = []

        for name, value in dependencies.items():
            if value is None or isinstance(value, bool):
                result.append(Dependency(name, ORIGINAL if value else COMPUTED))
            elif isinstance(value, str):
                try:
                    source = normalize_dependency_source(value)
                except ValueError:
                    result.append(Dependency(value, alias=name))
                else:
                    result.append(Dependency(name, source))
            elif isinstance(value, Mapping) and not any(key in value for key in name_keys):
                source = _spec_value(
                    value,
                    ('source', 'from', 'from_source', 'read_from', 'read', 'reads', 'kind', 'type', 'origin')
                )
                result.append(Dependency(name, source))
            elif isinstance(value, Dependency):
                result.append(Dependency(value.name, value.source, name))
            elif isinstance(value, Sequence):
                result.append(tuple(value) + (name,))
            else:
                result.append(Dependency(name, alias=name))

        return result

    if isinstance(dependencies, (set, frozenset)):
        return list(dependencies)

    if isinstance(dependencies, Sequence) or hasattr(dependencies, '__iter__'):
        return list(dependencies)

    return [dependencies]


def _parse_dependency(dependency, default_source=None):
    if isinstance(dependency, Dependency):
        source = dependency.source or default_source
        return Dependency(dependency.name, source, dependency.alias)

    if isinstance(dependency, str):
        return Dependency(dependency, default_source)

    if isinstance(dependency, Mapping):
        name = _spec_value(dependency, ('name', 'column', 'column_name', 'target', 'field'))
        source = _spec_value(
            dependency,
            ('source', 'from', 'from_source', 'read_from', 'read', 'reads', 'kind', 'type', 'origin'),
            default_source
        )
        alias = _spec_value(dependency, ('alias', 'as', 'as_name', 'as_column'))

        if _spec_value(dependency, ('original', 'read_original', 'from_original'), False) is True:
            source = ORIGINAL
        elif (
            _spec_value(dependency, ('new', 'computed'), False) is True
            or _spec_value(dependency, ('read_computed', 'from_computed'), False) is True
        ):
            source = COMPUTED

        return Dependency(name, source, alias)

    if isinstance(dependency, Sequence) and not isinstance(dependency, str):
        values = tuple(dependency)

        if len(values) == 1:
            return Dependency(values[0], default_source)

        if len(values) == 2:
            first, second = values

            try:
                first_source = normalize_dependency_source(first)
            except ValueError:
                first_source = None

            if first_source is not None:
                return Dependency(second, first_source)

            if isinstance(second, bool):
                return Dependency(first, ORIGINAL if second else COMPUTED)

            try:
                second_source = normalize_dependency_source(second)
            except ValueError:
                second_source = None

            if second_source is not None:
                return Dependency(first, second_source)

            return Dependency(first, default_source, alias=second)

        if len(values) >= 3:
            first, second, third = values[:3]

            try:
                first_source = normalize_dependency_source(first)
            except ValueError:
                first_source = None

            if first_source is not None:
                return Dependency(second, first_source, third)

            try:
                third_source = normalize_dependency_source(third)
            except ValueError:
                third_source = None

            if third_source is not None:
                return Dependency(first, third_source, second)

            return Dependency(first, second, third)

    name = _spec_value(dependency, ('name', 'column', 'column_name', 'target', 'field'))

    if name is None:
        raise ValueError('Invalid dependency specification: %r' % (dependency,))

    source = _spec_value(
        dependency,
        ('source', 'from', 'from_source', 'read_from', 'read', 'reads', 'kind', 'type', 'origin'),
        default_source
    )
    alias = _spec_value(dependency, ('alias', 'as', 'as_name', 'as_column'))

    if _spec_value(dependency, ('original', 'read_original', 'from_original'), False) is True:
        source = ORIGINAL
    elif (
        _spec_value(dependency, ('new', 'computed'), False) is True
        or _spec_value(dependency, ('read_computed', 'from_computed'), False) is True
    ):
        source = COMPUTED

    return Dependency(name, source, alias)


def _spec_default_source(spec):
    source = _spec_value(
        spec,
        ('dependency_source', 'dependency_kind', 'read_from', 'from_source', 'reads', 'read', 'source', 'use')
    )

    if source is not None:
        return normalize_dependency_source(source)

    if _spec_value(
        spec,
        ('read_original', 'reads_original', 'use_original', 'from_original', 'original'),
        False
    ) is True:
        return ORIGINAL

    if _spec_value(
        spec,
        ('read_computed', 'reads_computed', 'read_new', 'reads_new', 'use_computed',
         'from_computed', 'new', 'computed'),
        False
    ) is True:
        return COMPUTED

    return AUTO


def _parse_spec(spec, target=None, table=None):
    if target is not None and _is_computation(spec):
        new_column_name = target
        computation = spec
        dependencies = _computation_dependencies(computation, table)
        replace = False
        data_type = None
    elif target is not None and isinstance(spec, Sequence) and not isinstance(spec, str) \
            and spec and _is_computation(spec[0]):
        values = tuple(spec)
        new_column_name = target
        computation = values[0]
        dependencies = values[1] if len(values) > 1 else _computation_dependencies(computation, table)
        replace = values[2] if len(values) > 2 and isinstance(values[2], bool) else False
        data_type = values[3] if len(values) > 3 else None
    elif target is not None and isinstance(spec, Mapping):
        new_column_name = target
        computation = _spec_value(spec, ('computation', 'compute', 'func'))
        dependencies = _spec_value(
            spec, ('dependencies', 'depends_on', 'depends', 'deps', 'sources', 'inputs', 'source_columns')
        )
        replace = _spec_value(spec, ('replace', 'replace_existing', 'overwrite'), False)
        data_type = _spec_value(spec, ('data_type', 'declared_type', 'output_type', 'result_type', 'type'))
    elif isinstance(spec, ComputationSpec):
        new_column_name = spec.new_column_name
        computation = spec.computation
        dependencies = spec.dependencies
        replace = spec.replace or bool(_spec_value(spec, ('replace_existing', 'overwrite'), False))
        data_type = spec.data_type
    elif isinstance(spec, Mapping) or (not isinstance(spec, Sequence) and not isinstance(spec, str)):
        new_column_name = _spec_value(
            spec,
            ('new_column_name', 'target_column_name', 'target_column', 'target_name',
             'column_name', 'name', 'target')
        )
        computation = _spec_value(spec, ('computation', 'compute', 'func'))
        dependencies = _spec_value(
            spec, ('dependencies', 'depends_on', 'depends', 'deps', 'sources', 'inputs', 'source_columns')
        )
        replace = _spec_value(spec, ('replace', 'replace_existing', 'overwrite'), False)
        data_type = _spec_value(
            spec, ('data_type', 'declared_type', 'output_type', 'result_type', 'type')
        )
    elif isinstance(spec, Sequence) and not isinstance(spec, str):
        values = tuple(spec)

        if len(values) < 2:
            raise ValueError('Computation specifications must contain a name and a computation.')

        if _is_computation(values[0]) and isinstance(values[1], str):
            values = (values[1], values[0]) + values[2:]

        new_column_name, computation = values[:2]
        dependencies = None
        replace = False
        data_type = None

        if len(values) >= 3:
            if isinstance(values[2], bool):
                replace = values[2]
            else:
                dependencies = values[2]

        if len(values) >= 4:
            if isinstance(values[3], bool):
                replace = values[3]
            elif data_type is None:
                data_type = values[3]

        if len(values) >= 5:
            data_type = values[4]

        if len(values) > 5:
            raise ValueError('Computation specifications have too many values.')
    else:
        raise ValueError('Invalid computation specification: %r' % (spec,))

    if new_column_name is None:
        raise ValueError('Computation specification requires a target column name.')

    if computation is None or not _is_computation(computation):
        raise ValueError('Computation specification requires a Computation instance.')

    if dependencies is None:
        dependencies = _computation_dependencies(computation, table)

    dependencies = _normalize_dependency_list(dependencies)

    for keys, source in (
        (
            ('original_dependencies', 'original_columns', 'read_original_columns', 'read_original', 'original'),
            ORIGINAL,
        ),
        (
            ('computed_dependencies', 'computed_columns', 'new_dependencies', 'new_columns',
             'read_computed_columns', 'read_computed', 'read_new', 'computed', 'new'),
            COMPUTED,
        ),
    ):
        extra = _spec_value(spec, keys)

        if extra is None or isinstance(extra, bool):
            continue

        dependencies.extend(
            _parse_dependency(dependency, source)
            for dependency in _normalize_dependency_list(extra)
        )

    default_source = _spec_default_source(spec)

    return (
        new_column_name,
        computation,
        [_parse_dependency(dependency, default_source) for dependency in dependencies],
        bool(replace),
        data_type,
    )


def _find_cycle(nodes, edges):
    visiting = set()
    visited = set()
    stack = []

    def visit(node):
        if node in visiting:
            index = stack.index(node)
            return stack[index:] + [node]

        if node in visited:
            return None

        visiting.add(node)
        stack.append(node)

        for dependency in edges.get(node, ()):
            cycle = visit(dependency)

            if cycle is not None:
                return cycle

        stack.pop()
        visiting.remove(node)
        visited.add(node)

        return None

    for node in nodes:
        cycle = visit(node)

        if cycle is not None:
            return cycle

    return None


def _topological_order(nodes, edges, indices):
    indegree = {node: len(edges.get(node, ())) for node in nodes}
    dependents = {node: [] for node in nodes}

    for node in nodes:
        for dependency in edges.get(node, ()):
            dependents[dependency].append(node)

    ready = []

    for node in nodes:
        if indegree[node] == 0:
            heappush(ready, (indices[node], node))

    order = []

    while ready:
        _, node = heappop(ready)
        order.append(node)

        for dependent in dependents[node]:
            indegree[dependent] -= 1

            if indegree[dependent] == 0:
                heappush(ready, (indices[dependent], dependent))

    if len(order) != len(nodes):
        cycle = _find_cycle(nodes, edges)
        raise CircularDependencyError(
            'Circular computation dependency detected: %s.' % ' -> '.join(str(node) for node in cycle)
        )

    return order


def _build_view(original_names, original_types, original_values, current_names, current_types,
                current_values, dependencies, computed_values, computed_types):
    names = list(current_names)
    types = list(current_types)
    values = list(current_values)
    indices = {name: i for i, name in enumerate(names)}
    original_indices = {name: i for i, name in enumerate(original_names)}

    for dependency in dependencies:
        name = dependency.name

        if dependency.source == ORIGINAL:
            if name not in original_indices:
                raise MissingDependencyError('Missing original column "%s".' % name)

            index = original_indices[name]
            source_values = original_values[index]
            source_type = original_types[index]
        else:
            if name not in computed_values:
                raise MissingDependencyError('Missing computed column "%s".' % name)

            source_values = computed_values[name]
            source_type = computed_types[name]

        if name in indices:
            values[indices[name]] = source_values
            types[indices[name]] = source_type
        else:
            indices[name] = len(names)
            names.append(name)
            types.append(source_type)
            values.append(source_values)

        if dependency.alias is not None and dependency.alias != name:
            if dependency.alias in indices:
                values[indices[dependency.alias]] = source_values
                types[indices[dependency.alias]] = source_type
            else:
                indices[dependency.alias] = len(names)
                names.append(dependency.alias)
                types.append(source_type)
                values.append(source_values)

    return names, types, values


def _view_table(table, names, types, values):
    row_count = len(table.rows)
    rows = [
        Row(tuple(column[i] for column in values), names)
        for i in range(row_count)
    ]

    return table._fork(rows, names, types)


def _validate_output(data_type, output, row_count, target):
    try:
        output = tuple(output)
    except TypeError:
        raise ComputationOutputError(
            'Computation for column "%s" did not return a sequence.' % target
        )

    if len(output) != row_count:
        raise ComputationOutputError(
            'Computation for column "%s" returned %i values, but the table has %i rows.' % (
                target, len(output), row_count
            )
        )

    for i, value in enumerate(output):
        if not _value_matches_type(data_type, value):
            raise ComputationOutputError(
                'Computation for column "%s" returned a value of the wrong type at row %i: %r.' % (
                    target, i, value
                )
            )

    return output


def _value_matches_type(data_type, value):
    if value is None:
        return True

    if isinstance(data_type, Boolean):
        return type(value) is bool
    if isinstance(data_type, Number):
        return isinstance(value, Decimal)
    if isinstance(data_type, TimeDelta):
        return isinstance(value, datetime.timedelta)
    if isinstance(data_type, DateTime):
        return isinstance(value, datetime.datetime)
    if isinstance(data_type, Date):
        return isinstance(value, datetime.date) and not isinstance(value, datetime.datetime)
    if isinstance(data_type, Text):
        return isinstance(value, str)

    return data_type.test(value)


def _compute_dependencies(self, computations, replace=False):
    if isinstance(computations, Mapping):
        parsed_specs = [
            _parse_spec(spec, target=target, table=self)
            for target, spec in computations.items()
        ]
    else:
        parsed_specs = [_parse_spec(spec, table=self) for spec in computations]

    original_names = list(self._column_names)
    original_types = list(self._column_types)
    original_values = [column.values() for column in self.columns]

    target_names = []
    indices = {}
    specs = {}

    for index, (target, computation, dependencies, spec_replace, data_type) in enumerate(parsed_specs):
        if target in indices:
            raise DuplicateComputationColumnError(
                'Duplicate target column "%s" in computation specification.' % target
            )

        if target in original_names and not (replace or spec_replace):
            raise ValueError(
                'New column name "%s" already exists. Specify replace=True to replace with computed data.'
                % target
            )

        indices[target] = index
        target_names.append(target)
        specs[target] = (computation, dependencies, spec_replace, data_type)

    target_set = set(target_names)
    original_set = set(original_names)
    edges = {}
    resolved_dependencies = {}

    for target in target_names:
        _, dependencies, _, _ = specs[target]
        resolved = []
        graph_dependencies = []

        for dependency in dependencies:
            name = dependency.name
            source = dependency.source

            if source is None or source == AUTO:
                if name in target_set:
                    if name == target:
                        source = ORIGINAL if name in original_set else COMPUTED
                    else:
                        source = COMPUTED
                elif name in original_set:
                    source = ORIGINAL
                else:
                    raise MissingDependencyError(
                        'Missing column "%s" required by computation for "%s".' % (name, target)
                    )

            if source == ORIGINAL:
                if name not in original_set:
                    raise MissingDependencyError(
                        'Missing original column "%s" required by computation for "%s".' % (name, target)
                    )
            else:
                if name not in target_set:
                    raise MissingDependencyError(
                        'Missing computed column "%s" required by computation for "%s".' % (name, target)
                    )

                if name not in graph_dependencies:
                    graph_dependencies.append(name)

            resolved.append(Dependency(name, source, dependency.alias))

        edges[target] = graph_dependencies
        resolved_dependencies[target] = resolved

    order = _topological_order(target_names, edges, indices)

    current_names = list(original_names)
    current_types = list(original_types)
    current_values = list(original_values)
    current_indices = {name: i for i, name in enumerate(current_names)}
    computed_values = {}
    computed_types = {}
    row_count = len(self.rows)

    for target in order:
        computation, _, _, spec_data_type = specs[target]
        dependencies = resolved_dependencies[target]

        names, types, values = _build_view(
            original_names,
            original_types,
            original_values,
            current_names,
            current_types,
            current_values,
            dependencies,
            computed_values,
            computed_types,
        )
        view = _view_table(self, names, types, values)

        if spec_data_type is None:
            data_type = computation.get_computed_data_type(view)
        else:
            data_type = spec_data_type

        if not isinstance(data_type, DataType):
            raise ComputationOutputError(
                'Computation for column "%s" did not declare a DataType.' % target
            )

        computation.validate(view)
        output = _validate_output(data_type, computation.run(view), row_count, target)

        computed_values[target] = output
        computed_types[target] = data_type

        if target in current_indices:
            index = current_indices[target]
            current_values[index] = output
            current_types[index] = data_type
        else:
            current_indices[target] = len(current_names)
            current_names.append(target)
            current_types.append(data_type)
            current_values.append(output)

    rows = [
        Row(tuple(column[i] for column in current_values), current_names)
        for i in range(row_count)
    ]

    return self._fork(rows, current_names, current_types)


def compute_dependencies(self, computations, replace=False):
    """
    Create a new table by applying a graph of interdependent computations.

    Each item in :code:`computations` declares a target column, a
    :class:`.Computation`, and the columns that computation reads. Items may be
    :class:`.ComputationSpec` instances, dictionaries, or tuples in the form
    :code:`(column_name, computation, dependencies)`. Dependencies can be
    column names or :class:`.Dependency` instances. A dependency that names
    another target in this call reads that computed value; use
    :data:`.ORIGINAL` to read the original table instead and :data:`.COMPUTED`
    to read the value produced in this call.

    The complete dependency graph, including missing columns, cycles and
    duplicate target names, is validated before any computation runs. Results
    are returned only after every computation has succeeded.

    :param computations:
        A sequence of computation specifications.
    :param replace:
        If :code:`True` then target names may match existing names, and those
        columns will be replaced with the computed data.
    :returns:
        A new :class:`.Table`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_with_dependencies(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_dependency(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_ordered(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_plan(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_graph(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_dependency_graph(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_with_dependency_graph(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_dependency_plan(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)


def compute_columns(self, computations, replace=False):
    """
    Alias for :meth:`.Table.compute_dependencies`.
    """
    return _compute_dependencies(self, computations, replace=replace)
