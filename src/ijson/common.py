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


class JSONLimitError(JSONError):
    '''
    Raised when a parser resource limit is exceeded.

    The offending limit is available as ``limit_name``, its configured value
    as ``limit`` and the position of the token that exceeded it as
    ``position``. Positions are zero-based offsets into the decoded input.
    '''
    def __init__(self, limit_name, position, limit, actual=None):
        self.limit_name = limit_name
        self.position = position
        self.limit = limit
        self.actual = actual
        message = '%s exceeded at position %d (limit %d)' % (
            limit_name, position, limit
        )
        if actual is not None:
            message += ' [got %d]' % actual
        super(JSONLimitError, self).__init__(message)

    @property
    def name(self):
        return self.limit_name

    @property
    def pos(self):
        return self.position

    @property
    def offset(self):
        return self.position

    @property
    def limit_value(self):
        return self.limit

    @property
    def max_value(self):
        return self.limit

    @property
    def kind(self):
        return self.limit_name

    @property
    def where(self):
        return self.position

    @property
    def observed(self):
        return self.actual


# Historical/descriptive aliases for the same error
ResourceLimitError = JSONLimitError
LimitError = JSONLimitError
LimitExceededError = JSONLimitError


_RESOURCE_LIMIT_ALIASES = {
    'max_string_length': 'max_string_length',
    'max_string_chars': 'max_string_length',
    'max_string_characters': 'max_string_length',
    'max_string_len': 'max_string_length',
    'max_decoded_string_length': 'max_string_length',
    'max_decoded_chars': 'max_string_length',
    'max_decoded_characters': 'max_string_length',
    'max_string_size': 'max_string_length',
    'string_max_length': 'max_string_length',
    'string_length_limit': 'max_string_length',
    'string_limit': 'max_string_length',

    'max_number_length': 'max_number_length',
    'max_number_chars': 'max_number_length',
    'max_number_characters': 'max_number_length',
    'max_number_len': 'max_number_length',
    'max_number_token_length': 'max_number_length',
    'max_number_token_chars': 'max_number_length',
    'max_number_size': 'max_number_length',
    'max_numeric_token_length': 'max_number_length',
    'number_max_length': 'max_number_length',
    'number_length_limit': 'max_number_length',
    'number_limit': 'max_number_length',

    'max_depth': 'max_depth',
    'max_nesting_depth': 'max_depth',
    'max_nested_depth': 'max_depth',
    'max_nesting_level': 'max_depth',
    'max_nesting': 'max_depth',
    'max_nesting_levels': 'max_depth',
    'max_container_depth': 'max_depth',
    'max_container_level': 'max_depth',
    'max_container_levels': 'max_depth',
    'max_json_depth': 'max_depth',
    'max_recursion_depth': 'max_depth',
    'depth_limit': 'max_depth',
    'depth_max': 'max_depth',
    'nesting_depth_limit': 'max_depth',
}

_RESOURCE_LIMIT_NAMES = (
    'max_string_length',
    'max_number_length',
    'max_depth',
)


def _validate_limit_value(name, value):
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int):
        raise TypeError('%s must be a non-negative integer or None' % (name,))
    if value < 0:
        raise ValueError('%s must be a non-negative integer or None' % (name,))
    return value


def _resolve_resource_limits(config, remove=True):
    '''
    Extracts resource limit options from ``config``.

    Returns a mapping of canonical limit name to ``(value, provided_name)``.
    ``provided_name`` is the option name used by the caller, which is reported
    back in errors. If ``remove`` is true the recognized options are removed
    from ``config``.
    '''
    provided = {}

    if 'limits' in config:
        limits = config.pop('limits') if remove else config['limits']
        if limits is not None:
            if not hasattr(limits, 'items'):
                raise TypeError("'limits' must be a mapping or None")
            for name, value in limits.items():
                if name not in _RESOURCE_LIMIT_ALIASES:
                    raise ValueError('Unknown resource limit: %r' % (name,))
                provided[name] = value

    for name in list(config):
        if name in _RESOURCE_LIMIT_ALIASES:
            provided[name] = config.pop(name) if remove else config[name]

    resolved = {}
    for provided_name, value in provided.items():
        canonical = _RESOURCE_LIMIT_ALIASES[provided_name]
        value = _validate_limit_value(provided_name, value)
        previous = resolved.get(canonical)
        if previous is not None and previous[0] != value:
            raise ValueError(
                'Conflicting values for resource limit %s' % (canonical,)
            )
        resolved[canonical] = (value, provided_name)
    return resolved


def _active_resource_limits(config):
    return any(
        value is not None
        for value, _ in _resolve_resource_limits(dict(config), remove=False).values()
    )


def _reject_unsupported_resource_limits(config, backend_name):
    if _active_resource_limits(config):
        raise ValueError(
            'Resource limits (%s) are not supported by the %s backend' % (
                ', '.join(_RESOURCE_LIMIT_NAMES), backend_name
            )
        )


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
    config = dict(config)
    resolved = _resolve_resource_limits(config, remove=False)
    supported = getattr(backend.get('capabilities'), 'resource_limits', False)
    if not supported and any(value is not None for value, _ in resolved.values()):
        raise ValueError(
            'Resource limits (%s) are not supported by the %s backend' % (
                ', '.join(_RESOURCE_LIMIT_NAMES),
                backend.get('backend_name', 'this')
            )
        )
    if not supported:
        # Drop even inactive (None) limit options, as the underlying backend
        # wouldn't know what to do with them.
        _resolve_resource_limits(config, remove=True)
    return (
        (backend['basic_parse_basecoro'], [], config),
    )


def _parse_pipeline(backend, config):
    return (
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    )


def _items_pipeline(backend, prefix, map_type, config):
    return (
        (backend['items_basecoro'], (prefix,), {'map_type': map_type}),
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    )


def _kvitems_pipeline(backend, prefix, map_type, config):
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
            return utils.coros2gen(source,
                (backend['parse_basecoro'], (), {})
            )
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
            return utils.coros2gen(source,
                (backend['kvitems_basecoro'], (prefix,), {'map_type': map_type})
            )
        raise ValueError("Unknown source type: %r" % type(source))
    return kvitems


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
        'resource_limits': 'Per-token string/number length and nesting depth limits',
        'limits': 'Alias for resource_limits',
    }

    def __init__(self):
        self.c_comments = True
        self.multiple_values = True
        self.invalid_leading_zeros_detection = True
        self.incomplete_json_tokens_detection = True
        self.int64 = True
        self.resource_limits = False
        self.limits = False


def _wrap_with_resource_limit_check(func, backend):
    @functools.wraps(func)
    def wrapper(*args, **kwargs):
        _reject_unsupported_resource_limits(
            kwargs, backend.get('backend_name', 'this')
        )
        return func(*args, **kwargs)
    return wrapper


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

    if not capabilities.resource_limits:
        # Existing implementations might either pass unknown options down to
        # yajl or accept them as generic **kwargs. Make sure limits are never
        # silently ignored, regardless of the entry point used.
        for name in (
            'basic_parse_basecoro', 'basic_parse_gen', 'basic_parse_async',
            'parse_basecoro', 'parse_gen', 'parse_async',
            'items_basecoro', 'items_gen', 'items_async',
            'kvitems_basecoro', 'kvitems_gen', 'kvitems_async',
        ):
            if name in backend:
                backend[name] = _wrap_with_resource_limit_check(backend[name], backend)

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
