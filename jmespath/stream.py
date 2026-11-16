"""Lazy, single pass evaluation of JMESPath expressions over an array.

The streaming entry point only accepts expressions that can be applied to
each element of an outer array independently: projections and filter
projections whose right hand sides operate on a single element.

Whether an expression is streamable is decided up front, purely from the
parsed AST and from the *capability declarations* of the functions it
uses (see :func:`jmespath.functions.streamable` and
:data:`jmespath.functions.Functions.STREAMABLE_FUNCTIONS`).  Expressions
that aggregate, reorder, reverse, or otherwise depend on the total length
of the outer array raise :class:`jmespath.exceptions.StreamingExpressionError`
before the source is ever read, so a one shot iterator is never partially
consumed.
"""
from jmespath import exceptions
from jmespath.visitor import TreeInterpreter


class StreamingCapabilityValidator(object):
    """Decide whether a parsed expression can be evaluated against a stream.

    The decision is made entirely from the AST and the function capability
    declarations.  No source data is inspected, which means rejection
    happens before a single element of the source has been read.
    """

    def __init__(self, functions):
        self._functions = functions

    def validate(self, node):
        # The streaming entry point iterates the outer array element by
        # element, so the outermost operation has to be an array projection
        # or a filter projection.
        if node['type'] not in ('projection', 'filter_projection'):
            raise exceptions.StreamingExpressionError(
                "Streaming search only accepts top level array projections "
                "or filter projections; got a '%s' node instead."
                % node['type'])
        self._validate_spine(node)
        self._validate_node(node)

    def _validate_spine(self, node):
        node_type = node['type']
        if node_type == 'identity':
            return
        elif node_type == 'flatten':
            self._validate_spine(node['children'][0])
        elif node_type == 'index_expression':
            for child in node['children']:
                self._validate_spine(child)
        elif node_type == 'slice':
            self._reject_length_dependent_slice(node)
        elif node_type == 'index':
            if node['value'] < 0:
                raise exceptions.StreamingExpressionError(
                    "Negative index %r cannot be streamed because it "
                    "depends on the total length of the source."
                    % node['value'])
        elif node_type in ('projection', 'filter_projection'):
            self._validate_spine(node['children'][0])
        else:
            raise exceptions.StreamingExpressionError(
                "Only projections and filters that iterate over the outer "
                "array can be streamed; got a '%s' node on the streaming "
                "spine." % node_type)

    def _validate_node(self, node):
        # Slice nodes store their start/stop/step as raw integers or None
        # rather than as child AST nodes.
        if not isinstance(node, dict):
            return
        node_type = node['type']
        if node_type == 'function_expression':
            function_name = node['value']
            if not self._functions.is_streamable(function_name):
                raise exceptions.StreamingExpressionError(
                    "Function %s() is not declared streamable, so the "
                    "expression cannot be evaluated against a streaming "
                    "source." % function_name)
        elif node_type == 'slice':
            self._reject_length_dependent_slice(node)
        elif node_type == 'index':
            if node['value'] < 0:
                raise exceptions.StreamingExpressionError(
                    "Negative index %r cannot be streamed because it "
                    "depends on the total length of the source."
                    % node['value'])
        for child in node.get('children', []):
            self._validate_node(child)

    def _reject_length_dependent_slice(self, node):
        start, stop, step = node['children']
        if step is not None and step < 0:
            raise exceptions.StreamingExpressionError(
                "Reverse slices (negative step) cannot be streamed because "
                "they require the full source to be read and reordered.")
        if (start is not None and start < 0) or \
                (stop is not None and stop < 0):
            raise exceptions.StreamingExpressionError(
                "A slice with a negative bound cannot be streamed because "
                "it depends on the total length of the source.")


class StreamingTreeInterpreter(TreeInterpreter):
    """Interpreter that yields projection results one element at a time."""

    def __init__(self, options=None):
        super(StreamingTreeInterpreter, self).__init__(options)
        self._validator = StreamingCapabilityValidator(self._functions)

    def stream_search(self, node, value):
        # Validate eagerly, before the source iterator is created, so an
        # unsupported expression never consumes even one source element.
        self._validator.validate(node)
        return self._stream(node, self._as_stream(value))

    def _as_stream(self, value):
        # JMESPath strings and objects are not iterable arrays; anything
        # that is not iterable behaves like an empty source.
        if isinstance(value, (str, bytes, dict)):
            return iter(())
        try:
            return iter(value)
        except TypeError:
            return iter(())

    def _stream(self, node, source):
        node_type = node['type']
        if node_type == 'projection':
            left_node, right_node = node['children']
            base = self._stream_node(left_node, source, on_source=True)
            for element in base:
                # Projections silently drop null results; this is the only
                # place nulls are removed.  Nulls produced inside the right
                # hand side (multi select, map(), ...) are preserved.
                current = self.visit(right_node, element)
                if current is not None:
                    yield current
        elif node_type == 'filter_projection':
            left_node, right_node, comparator_node = node['children']
            base = self._stream_node(left_node, source, on_source=True)
            for element in base:
                if self._is_true(self.visit(comparator_node, element)):
                    current = self.visit(right_node, element)
                    if current is not None:
                        yield current
        else:
            raise exceptions.StreamingExpressionError(
                "Cannot stream a '%s' node; only projections and filter "
                "projections can be streamed." % node_type)

    def _stream_node(self, node, source, on_source=False):
        node_type = node['type']
        if node_type == 'identity':
            return source
        elif node_type == 'flatten':
            return self._stream_flatten(node, source, on_source)
        elif node_type == 'slice':
            return self._stream_slice(node, source, on_source)
        elif node_type == 'index':
            return self._stream_index(node, source, on_source)
        elif node_type == 'index_expression':
            result = source
            for child in node['children']:
                result = self._stream_node(child, result, on_source)
            return result
        elif node_type in ('projection', 'filter_projection'):
            return self._stream_nested_projection(node, source)
        else:
            raise exceptions.StreamingExpressionError(
                "Node '%s' cannot be applied lazily to a streaming source."
                % node_type)

    def _stream_nested_projection(self, node, source):
        # A nested projection on the spine receives the incoming stream as
        # its base directly.  Its left hand side is therefore streamed with
        # ``on_source`` semantics (a slice or index addresses the base list
        # itself), and the right hand side is applied to each item of the
        # resulting base one at a time.  Null results are dropped exactly as
        # in visit_projection / visit_filter_projection.  This keeps the
        # outer source lazily traversed and exactly preserves pipelines such
        # as "[*].nums[]" and "[1:].n[]".
        left_node = node['children'][0]
        base = self._stream_node(left_node, source, on_source=True)
        if node['type'] == 'projection':
            right_node = node['children'][1]
            for item in base:
                current = self.visit(right_node, item)
                if current is not None:
                    yield current
        else:
            right_node, comparator_node = node['children'][1], \
                node['children'][2]
            for item in base:
                if self._is_true(self.visit(comparator_node, item)):
                    current = self.visit(right_node, item)
                    if current is not None:
                        yield current

    def _stream_flatten(self, node, source, on_source):
        # First evaluate the flatten's child expression against the
        # incoming stream, then merge any list values one level deep.  This
        # matches the regular interpreter's visit_flatten while keeping the
        # whole pipeline lazy and single pass.
        flattened = self._stream_node(
            node['children'][0], source, on_source)
        for element in flattened:
            if isinstance(element, list):
                for sub_element in element:
                    yield sub_element
            else:
                yield element

    def _stream_slice(self, node, source, on_source):
        start, stop, step = node['children']
        step = 1 if step is None else step
        start = 0 if start is None else start
        if on_source:
            # Slice applied to the outer source itself.  The validator
            # guarantees non negative start/stop and a positive step, so the
            # source needs no total length and can be consumed lazily.
            for index, element in enumerate(source):
                if stop is not None and index >= stop:
                    break
                if index >= start and (index - start) % step == 0:
                    yield element
        else:
            # Slice applied to each element of the current stream; a non
            # list element yields nothing, which the outer projection drops.
            for value in source:
                if not isinstance(value, list):
                    continue
                for index, element in enumerate(value):
                    if stop is not None and index >= stop:
                        break
                    if index >= start and (index - start) % step == 0:
                        yield element

    def _stream_index(self, node, source, on_source):
        target = node['value']
        if on_source:
            # Indexing the outer source only needs to walk up to the target
            # position; negative indexes are rejected by the validator.
            for index, element in enumerate(source):
                if index == target:
                    yield element
                    return
        else:
            for value in source:
                if not isinstance(value, list):
                    continue
                if target < len(value):
                    yield value[target]


def stream_search(expression, data, options=None):
    """Evaluate ``expression`` over an outer array ``data`` lazily.

    ``data`` may be a list or any single pass iterable (a generator, a
    file object, ...).  It is traversed at most once and only as far as the
    caller consumes the returned iterator.

    Only expressions that can be processed element by element are accepted;
    anything that needs the whole source (sorting, reversing, length
    dependent slices or indexes, non streamable functions) raises
    :class:`~jmespath.exceptions.StreamingExpressionError` before the source
    is read.
    """
    from jmespath.parser import Parser
    parsed = Parser().parse(expression)
    interpreter = StreamingTreeInterpreter(options)
    return interpreter.stream_search(parsed.parsed, data)
