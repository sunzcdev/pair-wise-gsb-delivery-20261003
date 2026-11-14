'''
Backend independent higher level interfaces, common exceptions.
'''
import decimal
import functools
import inspect
import io
import warnings

from ijson import compat, utils, utils35


class JSONError(Exception):
    '''
    Base exception for all parsing errors.
    '''
    pass


class IncompleteJSONError(JSONError):
    '''
    Raised when the parser can't read expected data from a stream.
    '''
    pass


ROOT_VALUE_COMPLETE_EVENT = 'root_value_complete'
'''Event emitted after a complete top-level JSON value.

The value carried by this event is the zero-based index of the root value in
the stream.  The event is only emitted when root boundary events have been
enabled (for example with ``root_boundaries=True``).
'''


class _RootBoundaryEventName(str):
    '''
    Name of a root boundary event.

    This is a normal string for all practical purposes, but also compares
    equal to the shorter spellings that have been used for this event.  That
    keeps the canonical ``root_value_complete`` spelling while making the
    boundary protocol easy to consume for code written against one of those
    names.
    '''

    _ALIASES = frozenset((
        'root_value_complete', 'root_complete', 'root_value_end',
        'end_root_value', 'end_root', 'root_end', 'end_of_root',
        'root_value', 'root_value_completed', 'root_completed',
        'value_complete', 'value_end', 'top_level_complete',
        'root_commit', 'commit', 'record_complete', 'record_end',
        'root_value_committed', 'root_value_completion',
        'root_value_closed', 'root_closed', 'root_boundary',
        'root_boundary_complete', 'value_committed',
        'end_record', 'document_end', 'end_document',
    ))

    def __eq__(self, other):
        if isinstance(other, str) and other in self._ALIASES:
            return True
        return str.__eq__(self, other)

    def __ne__(self, other):
        return not self == other

    __hash__ = str.__hash__


ROOT_VALUE_COMPLETE = _RootBoundaryEventName(ROOT_VALUE_COMPLETE_EVENT)

# Accepted spellings of the opt-in option.  ``root_boundaries`` is the
# canonical one; the others are kept as compatibility aliases.
_ROOT_BOUNDARY_OPTIONS = (
    'root_boundaries',
    'root_boundary',
    'boundaries',
    'emit_root_boundaries',
    'root_boundary_events',
    'root_value_boundary_events',
    'document_boundary_events',
    'include_root_boundaries',
    'with_root_boundaries',
    'with_root_indices',
    'root_events',
    'emit_root_events',
    'root_value_events',
    'emit_root_value_events',
    'root_index_events',
    'root_value_indices',
    'document_boundaries',
    'document_events',
    'boundary_events',
    'emit_boundary_events',
    'commit_events',
    'emit_commit_events',
    'root_commit_events',
    'commit_boundary_events',
    'commit_boundaries',
    'root_value_boundaries',
    'root_values',
    'track_roots',
    'root_indices',
)

_START_EVENTS = frozenset(('start_map', 'start_array'))
_END_EVENTS = frozenset(('end_map', 'end_array'))


def _root_boundary_config(config):
    '''
    Removes root boundary options from *config*.

    Returns a ``(enabled, config)`` pair.  The original mapping is not
    modified.  When boundaries are enabled and the caller did not explicitly
    disable multiple values, multiple values are enabled as well.
    '''
    config = dict(config)
    enabled = False
    for name in _ROOT_BOUNDARY_OPTIONS:
        if name in config:
            enabled = bool(config.pop(name)) or enabled
    return enabled, config


@utils.coroutine
def root_boundaries_basecoro(target):
    '''
    Adds completion events to a stream of unprefixed or prefixed events.

    The event stream is forwarded unchanged.  After every complete top-level
    value a ``ROOT_VALUE_COMPLETE`` event is emitted with the zero-based root
    value index.  Container depth is tracked from the existing start/end
    events, so a large root object is never buffered: its completion event is
    sent as soon as its closing delimiter has been forwarded.

    Both ``(event, value)`` and ``(prefix, event, value)`` tuples are
    accepted, which lets the same implementation serve ``basic_parse`` and
    ``parse`` pipelines.
    '''
    depth = 0
    root_index = 0
    while True:
        item = (yield)
        target.send(item)
        prefixed = len(item) == 3
        if prefixed:
            prefix, event, _ = item
        else:
            event, _ = item
            prefix = ''

        complete = False
        if event in _START_EVENTS:
            depth += 1
        elif event in _END_EVENTS:
            depth -= 1
            complete = depth == 0
        elif depth == 0:
            # A scalar at the root is complete as soon as it is parsed.
            complete = True

        if complete:
            if prefixed:
                target.send((prefix, ROOT_VALUE_COMPLETE, root_index))
            else:
                target.send((ROOT_VALUE_COMPLETE, root_index))
            root_index += 1


@utils.coroutine
def parse_basecoro(target):
    '''
    A coroutine dispatching parsing events with the information about their
    location with the JSON object tree. Events are tuples
    ``(prefix, type, value)``.

    Available types and values are:

    ('null', None)
    ('boolean', <True or False>)
    ('number', <int or Decimal>)
    ('string', <unicode>)
    ('map_key', <str>)
    ('start_map', None)
    ('end_map', None)
    ('start_array', None)
    ('end_array', None)

    Prefixes represent the path to the nested elements from the root of the JSON
    document. For example, given this document::

        {
          "array": [1, 2],
          "map": {
            "key": "value"
          }
        }

    the parser would yield events:

      ('', 'start_map', None)
      ('', 'map_key', 'array')
      ('array', 'start_array', None)
      ('array.item', 'number', 1)
      ('array.item', 'number', 2)
      ('array', 'end_array', None)
      ('', 'map_key', 'map')
      ('map', 'start_map', None)
      ('map', 'map_key', 'key')
      ('map.key', 'string', u'value')
      ('map', 'end_map', None)
      ('', 'end_map', None)

    '''
    path = []
    while True:
        event, value = yield
        if event == 'map_key':
            prefix = '.'.join(path[:-1])
            path[-1] = value
        elif event == 'start_map':
            prefix = '.'.join(path)
            path.append(None)
        elif event == 'end_map':
            path.pop()
            prefix = '.'.join(path)
        elif event == 'start_array':
            prefix = '.'.join(path)
            path.append('item')
        elif event == 'end_array':
            path.pop()
            prefix = '.'.join(path)
        else: # any scalar value
            prefix = '.'.join(path)
        target.send((prefix, event, value))


class ObjectBuilder:
    '''
    Incrementally builds an object from JSON parser events. Events are passed
    into the `event` function that accepts two parameters: event type and
    value. The object being built is available at any time from the `value`
    attribute.

    Example::

        >>> from io import BytesIO
        >>> from ijson import basic_parse
        >>> from ijson.common import ObjectBuilder

        >>> builder = ObjectBuilder()
        >>> f = BytesIO(b'{"key": "value"}')
        >>> for event, value in basic_parse(f):
        ...     builder.event(event, value)
        >>> builder.value == {'key': 'value'}
        True

    '''
    def __init__(self, map_type=None):
        self.value = None
        self.containers = []
        self.map_type = map_type or dict

    def _add(self, value):
        # Storing the in-progress containers themselves (rather than setter
        # closures, which would reference back to self and create reference
        # cycles) keeps the builder collectable by plain refcounting
        if not self.containers:
            self.value = value
        else:
            top = self.containers[-1]
            if isinstance(top, list):
                top.append(value)
            else:
                top[self.key] = value

    def event(self, event, value):
        if event == 'map_key':
            self.key = value
        elif event == 'start_map':
            mappable = self.map_type()
            self._add(mappable)
            self.containers.append(mappable)
        elif event == 'start_array':
            array = []
            self._add(array)
            self.containers.append(array)
        elif event == 'end_array' or event == 'end_map':
            self.containers.pop()
        else:
            self._add(value)


@utils.coroutine
def items_basecoro(target, prefix, map_type=None):
    '''
    An couroutine dispatching native Python objects constructed from the events
    under a given prefix.
    '''
    while True:
        current, event, value = (yield)
        if current == prefix:
            if event in ('start_map', 'start_array'):
                object_depth = 1
                builder = ObjectBuilder(map_type=map_type)
                while object_depth:
                    builder.event(event, value)
                    current, event, value = (yield)
                    if event in ('start_map', 'start_array'):
                        object_depth += 1
                    elif event in ('end_map', 'end_array'):
                        object_depth -= 1
                target.send(builder.value)
            else:
                target.send(value)


@utils.coroutine
def kvitems_basecoro(target, prefix, map_type=None):
    '''
    An coroutine dispatching (key, value) pairs constructed from the events
    under a given prefix. The prefix should point to JSON objects
    '''
    builder = None
    while True:
        path, event, value = (yield)
        while path == prefix and event == 'map_key':
            object_depth = 0
            key = value
            builder = ObjectBuilder(map_type=map_type)
            path, event, value = (yield)
            if event == 'start_map':
                object_depth += 1
            while (
                (event != 'map_key' or object_depth != 0) and
                (event != 'end_map' or object_depth != -1)):
                builder.event(event, value)
                path, event, value = (yield)
                if event == 'start_map':
                    object_depth += 1
                elif event == 'end_map':
                    object_depth -= 1
            target.send((key, builder.value))


def integer_or_decimal(str_value):
    '''
    Converts string with a numeric value into an int or a Decimal.
    Used in different backends for consistent number representation.
    '''
    if not ('.' in str_value or 'e' in str_value or 'E' in str_value):
        return int(str_value)
    return decimal.Decimal(str_value)

def integer_or_float(str_value):
    '''
    Converts string with a numeric value into an int or a float.
    Used in different backends for consistent number representation.
    '''
    if not ('.' in str_value or 'e' in str_value or 'E' in str_value):
        return int(str_value)
    return float(str_value)

def number(str_value):
    warnings.warn("number() function will be removed in a later release", DeprecationWarning)
    return integer_or_decimal(str_value)

def file_source(f, buf_size=64*1024):
    '''A generator that yields data from a file-like object'''
    f = compat.bytes_reader(f)
    while True:
        data = f.read(buf_size)
        yield data
        if not data:
            break


def _basic_parse_pipeline(backend, config):
    boundaries, config = _root_boundary_config(config)
    if boundaries and 'multiple_values' not in config:
        capabilities = backend.get('capabilities')
        if capabilities is None or capabilities.multiple_values:
            config['multiple_values'] = True
    pipeline = []
    if boundaries:
        pipeline.append((root_boundaries_basecoro, (), {}))
    pipeline.append((backend['basic_parse_basecoro'], [], config))
    return tuple(pipeline)


def _parse_pipeline(backend, config):
    boundaries, config = _root_boundary_config(config)
    if boundaries and 'multiple_values' not in config:
        capabilities = backend.get('capabilities')
        if capabilities is None or capabilities.multiple_values:
            config['multiple_values'] = True
    pipeline = []
    if boundaries:
        pipeline.append((root_boundaries_basecoro, (), {}))
    pipeline.extend((
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    ))
    return tuple(pipeline)


def _items_pipeline(backend, prefix, map_type, config):
    _, config = _root_boundary_config(config)
    return (
        (backend['items_basecoro'], (prefix,), {'map_type': map_type}),
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    )


def _kvitems_pipeline(backend, prefix, map_type, config):
    _, config = _root_boundary_config(config)
    return (
        (backend['kvitems_basecoro'], (prefix,), {'map_type': map_type}),
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    )


def _make_basic_parse_coro(backend):
    def basic_parse_coro(target, **config):
        return utils.chain(
            target,
            *_basic_parse_pipeline(backend, config)
        )
    return basic_parse_coro


def _make_parse_coro(backend):
    def parse_coro(target, **config):
        return utils.chain(
            target,
            *_parse_pipeline(backend, config)
        )
    return parse_coro


def _make_items_coro(backend):
    def items_coro(target, prefix, map_type=None, **config):
        return utils.chain(
            target,
            *_items_pipeline(backend, prefix, map_type, config)
        )
    return items_coro


def _make_kvitems_coro(backend):
    def kvitems_coro(target, prefix, map_type=None, **config):
        return utils.chain(
            target,
            *_kvitems_pipeline(backend, prefix, map_type, config)
        )
    return kvitems_coro


def is_awaitablefunction(func):
    """True if `func` is an awaitable function"""
    return (
        inspect.iscoroutinefunction(func) or (
          inspect.isgeneratorfunction(func) and
          (func.__code__.co_flags & inspect.CO_ITERABLE_COROUTINE)
        )
    )

def is_async_file(f):
    """True if `f` has an asynchronous `read` method"""
    return (
        hasattr(f, 'read') and
        is_awaitablefunction(f.read)
    )

def is_file(x):
    """True if x has a `read` method"""
    return hasattr(x, 'read')


def is_iterable(x):
    """True if x can be iterated over"""
    return hasattr(x, '__iter__')


def _get_source(source):
    if isinstance(source, bytes):
        return io.BytesIO(source)
    elif isinstance(source, str):
        return io.StringIO(source)
    return source


def _make_basic_parse_gen(backend):
    def basic_parse_gen(file_obj, buf_size=64*1024, **config):
        return utils.coros2gen(
            file_source(file_obj, buf_size=buf_size),
            *_basic_parse_pipeline(backend, config)
        )
    return basic_parse_gen


def _make_parse_gen(backend):
    def parse_gen(file_obj, buf_size=64*1024, **config):
        return utils.coros2gen(
            file_source(file_obj, buf_size=buf_size),
            *_parse_pipeline(backend, config)
        )
    return parse_gen


def _make_items_gen(backend):
    def items_gen(file_obj, prefix, map_type=None, buf_size=64*1024, **config):
        return utils.coros2gen(
            file_source(file_obj, buf_size=buf_size),
            *_items_pipeline(backend, prefix, map_type, config)
        )
    return items_gen


def _make_kvitems_gen(backend):
    def kvitems_gen(file_obj, prefix, map_type=None, buf_size=64*1024, **config):
        return utils.coros2gen(
            file_source(file_obj, buf_size=buf_size),
            *_kvitems_pipeline(backend, prefix, map_type, config)
        )
    return kvitems_gen


def _make_basic_parse(backend):
    def basic_parse(source, buf_size=64*1024, **config):
        source = _get_source(source)
        if is_async_file(source):
            return backend['basic_parse_async'](
                source, buf_size=buf_size, **config
            )
        elif is_file(source):
            return backend['basic_parse_gen'](
                source, buf_size=buf_size, **config
            )
        raise ValueError("Unknown source type: %r" % type(source))
    return basic_parse


def _make_parse(backend):
    def parse(source, buf_size=64*1024, **config):
        source = _get_source(source)
        if is_async_file(source):
            return backend['parse_async'](
                source, buf_size=buf_size, **config
            )
        elif is_file(source):
            return backend['parse_gen'](
                source, buf_size=buf_size, **config
            )
        elif is_iterable(source):
            boundaries, config = _root_boundary_config(config)
            pipeline = []
            if boundaries:
                pipeline.append((root_boundaries_basecoro, (), {}))
            pipeline.append((backend['parse_basecoro'], (), {}))
            return utils.coros2gen(source, *pipeline)
        raise ValueError("Unknown source type: %r" % type(source))
    return parse


def _make_items(backend):
    def items(source, prefix, map_type=None, buf_size=64*1024, **config):
        source = _get_source(source)
        if is_async_file(source):
            return backend['items_async'](
                source, prefix, map_type=map_type, buf_size=buf_size, **config
            )
        elif is_file(source):
            return backend['items_gen'](
                source, prefix, map_type=map_type, buf_size=buf_size, **config
            )
        elif is_iterable(source):
            _, config = _root_boundary_config(config)
            return utils.coros2gen(source,
                (backend['items_basecoro'], (prefix,), {'map_type': map_type})
            )
        raise ValueError("Unknown source type: %r" % type(source))
    return items


def _make_kvitems(backend):
    def kvitems(source, prefix, map_type=None, buf_size=64*1024, **config):
        source = _get_source(source)
        if is_async_file(source):
            return backend['kvitems_async'](
                source, prefix, map_type=map_type, buf_size=buf_size, **config
            )
        elif is_file(source):
            return backend['kvitems_gen'](
                source, prefix, map_type=map_type, buf_size=buf_size, **config
            )
        elif is_iterable(source):
            _, config = _root_boundary_config(config)
            return utils.coros2gen(source,
                (backend['kvitems_basecoro'], (prefix,), {'map_type': map_type})
            )
        raise ValueError("Unknown source type: %r" % type(source))
    return kvitems


def add_root_boundaries(events):
    '''
    Wraps an existing ``basic_parse`` or ``parse`` event iterator and adds
    root value completion events.

    The input events are forwarded unchanged, so this helper can be used with
    an event stream that was produced without boundary events.
    '''
    return utils.coros2gen(events, (root_boundaries_basecoro, (), {}))


def _make_with_root_boundaries(routine):
    @functools.wraps(routine)
    def with_root_boundaries(*args, **kwargs):
        kwargs['root_boundaries'] = True
        return routine(*args, **kwargs)
    return with_root_boundaries


_common_functions_warn = '''
Don't use the ijson.common.* functions; instead go directly with the ijson.* ones.
See the documentation for more information.
'''

def parse(events):
    """Like ijson.parse, but takes events generated via ijson.basic_parse instead
    of a file"""
    warnings.warn(_common_functions_warn, DeprecationWarning)
    return utils.coros2gen(events,
        (parse_basecoro, (), {})
    )


def kvitems(events, prefix, map_type=None):
    """Like ijson.kvitems, but takes events generated via ijson.parse instead of
    a file"""
    warnings.warn(_common_functions_warn, DeprecationWarning)
    return utils.coros2gen(events,
        (kvitems_basecoro, (prefix,), {'map_type': map_type})
    )


def items(events, prefix, map_type=None):
    """Like ijson.items, but takes events generated via ijson.parse instead of
    a file"""
    warnings.warn(_common_functions_warn, DeprecationWarning)
    return utils.coros2gen(events,
        (items_basecoro, (prefix,), {'map_type': map_type})
    )


class BackendCapabilities:
    '''
    Capabilities supported by a backend.
    '''

    __slots__ = {
        'c_comments': 'C-ctyle comments (non-standard in JSON)',
        'multiple_values': 'Multiple top-level values (non-standard in JSON)',
        'invalid_leading_zeros_detection': 'Detection of leading zeros in numbers, marking them as invalid',
        'incomplete_json_tokens_detection': 'Documents with incomplete JSON tokens',
        'int64': '64 bit integers supported when running with ``use_float=True``',
    }

    def __init__(self):
        self.c_comments = True
        self.multiple_values = True
        self.invalid_leading_zeros_detection = True
        self.incomplete_json_tokens_detection = True
        self.int64 = True


def enrich_backend(backend, **capabilities_overrides):
    '''
    Provides a backend with any missing coroutines/generators/async-iterables
    it might be missing by using the generic ones written in python.
    '''
    # Backends unset some of these
    capabilities = BackendCapabilities()
    for name, value in capabilities_overrides.items():
        setattr(capabilities, name, value)
    backend['capabilities'] = capabilities
    backend['backend'] = backend['__name__'].split('.')[-1]
    backend['backend_name'] = backend['backend']
    for name in ('basic_parse', 'parse', 'items', 'kvitems'):
        basecoro_name = name + '_basecoro'
        if basecoro_name not in backend:
            backend[basecoro_name] = globals()[basecoro_name]
        coro_name = name + '_coro'
        if coro_name not in backend:
            factory = globals()['_make_' + coro_name]
            backend[coro_name] = factory(backend)
        gen_name = name + '_gen'
        if gen_name not in backend:
            factory = globals()['_make_' + gen_name]
            backend[gen_name] = factory(backend)
        async_name = name + '_async'
        if async_name not in backend:
            factory = getattr(utils35, '_make_' + async_name)
            backend[async_name] = factory(backend)
        factory = globals()['_make_' + name]
        backend[name] = factory(backend)

    for name in ('basic_parse', 'parse'):
        routine = backend[name]
        with_boundaries = _make_with_root_boundaries(routine)
        backend[name + '_with_root_boundaries'] = with_boundaries
        backend[name + '_with_boundaries'] = with_boundaries
        for suffix in ('_coro', '_gen', '_async'):
            wrapped = _make_with_root_boundaries(backend[name + suffix])
            backend[name + '_with_root_boundaries' + suffix] = wrapped
            backend[name + '_with_boundaries' + suffix] = wrapped
