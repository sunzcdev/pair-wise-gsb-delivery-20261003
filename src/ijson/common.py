'''
Backend independent higher level interfaces, common exceptions.
'''
import decimal
import enum
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


class DuplicateKeyError(JSONError):
    '''
    Raised when a JSON object contains two members with the same decoded
    key and the active :class:`DuplicateKeyPolicy` rejects duplicates.
    '''
    pass


class DuplicateKeyPolicy(enum.Enum):
    '''
    How :class:`ObjectBuilder` (and the ``items``/``kvitems`` routines that
    use it) handle two members of the same JSON object sharing an equal,
    decoded key.

    Duplicates are always judged on the *decoded* key: two members whose
    keys differ only in their escape sequences (e.g. ``"a"`` and ``"\\u0061"``)
    are considered duplicates. Each nested object keeps its own independent
    tracking, and distinct objects inside an array never interfere with
    each other.
    '''

    KEEP_LAST = 'keep_last'
    '''Keep the value of the last occurrence, silently overwriting previous
    values. This matches the historical behavior of writing straight into a
    mapping and is the default policy.'''

    KEEP_FIRST = 'keep_first'
    '''Keep the value of the first occurrence and ignore later ones. The
    whole document is still parsed, so syntactically invalid ignored values
    still raise an error rather than being skipped.'''

    REJECT = 'reject'
    '''Raise :class:`DuplicateKeyError` as soon as a duplicate member is
    encountered.'''


def _normalize_duplicate_keys(duplicate_keys):
    '''Turns a user-provided policy (enum member or its name/value) into a
    :class:`DuplicateKeyPolicy`, raising ``ValueError`` on unknown input.'''
    if isinstance(duplicate_keys, DuplicateKeyPolicy):
        return duplicate_keys
    if isinstance(duplicate_keys, str):
        try:
            return DuplicateKeyPolicy(duplicate_keys)
        except ValueError:
            try:
                return DuplicateKeyPolicy[duplicate_keys]
            except KeyError:
                pass
    raise ValueError(
        "Unknown duplicate key policy: %r (expected one of %s)" % (
            duplicate_keys,
            ", ".join(policy.value for policy in DuplicateKeyPolicy),
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
    def __init__(self, map_type=None, duplicate_keys=DuplicateKeyPolicy.KEEP_LAST):
        self.value = None
        self.containers = []
        # For every mapping currently in self.containers a flag telling
        # whether the member whose value is being built must be discarded
        # (KEEP_FIRST ignoring a duplicate). A synthetic root entry keeps the
        # indexing logic uniform.
        self.map_ignored = [False]
        self.map_type = map_type or dict
        self.duplicate_keys = _normalize_duplicate_keys(duplicate_keys)

    def _ignoring(self):
        return self.map_ignored[-1]

    def _add(self, value):
        # Storing the in-progress containers themselves (rather than setter
        # closures, which would reference back to self and create reference
        # cycles) keeps the builder collectable by plain refcounting
        if not self.containers:
            if not self._ignoring():
                self.value = value
            return
        top = self.containers[-1]
        if isinstance(top, list):
            top.append(value)
        elif not self._ignoring():
            top[self.key] = value

    def event(self, event, value):
        if event == 'map_key':
            # Keys are already decoded by the time they reach the builder,
            # so differently escaped but equivalent keys compare as equal.
            # Each mapping checks only itself; arrays and separate objects
            # therefore never interact.
            # A new member always starts as visible; the flag is only set
            # again below if this particular member turns out to be a
            # duplicate to discard.
            self.map_ignored[-1] = False
            self.key = value
            if (
                not self._ignoring() and
                self.duplicate_keys is not DuplicateKeyPolicy.KEEP_LAST and
                self.containers and
                value in self.containers[-1]
            ):
                if self.duplicate_keys is DuplicateKeyPolicy.REJECT:
                    raise DuplicateKeyError(
                        "Duplicate JSON object key found: %r" % (value,)
                    )
                self.map_ignored[-1] = True
        elif event == 'start_map':
            mappable = self.map_type()
            self._add(mappable)
            self.containers.append(mappable)
            self.map_ignored.append(False)
        elif event == 'start_array':
            array = []
            self._add(array)
            self.containers.append(array)
        elif event == 'end_array' or event == 'end_map':
            self.containers.pop()
            if event == 'end_map':
                self.map_ignored.pop()
        else:
            self._add(value)


@utils.coroutine
def items_basecoro(target, prefix, map_type=None,
                   duplicate_keys=DuplicateKeyPolicy.KEEP_LAST):
    '''
    An couroutine dispatching native Python objects constructed from the events
    under a given prefix.
    '''
    duplicate_keys = _normalize_duplicate_keys(duplicate_keys)
    while True:
        current, event, value = (yield)
        if current == prefix:
            if event in ('start_map', 'start_array'):
                object_depth = 1
                builder = ObjectBuilder(map_type=map_type,
                                        duplicate_keys=duplicate_keys)
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
def kvitems_basecoro(target, prefix, map_type=None,
                     duplicate_keys=DuplicateKeyPolicy.KEEP_LAST):
    '''
    An coroutine dispatching (key, value) pairs constructed from the events
    under a given prefix. The prefix should point to JSON objects
    '''
    duplicate_keys = _normalize_duplicate_keys(duplicate_keys)
    builder = None
    # Member keys already seen in the object currently sitting directly at
    # the prefix. A fresh set is installed whenever such an object opens, so
    # each nested object keeps its own independent tracking and distinct
    # objects (e.g. the elements of an array) never interact.
    seen_keys = None
    while True:
        path, event, value = (yield)
        if path == prefix and event == 'start_map':
            seen_keys = set()
        while path == prefix and event == 'map_key':
            object_depth = 0
            key = value
            # The event carrying the key is already decoded, so differently
            # escaped but equivalent names compare equal here.
            if key in seen_keys:
                if duplicate_keys is DuplicateKeyPolicy.REJECT:
                    raise DuplicateKeyError(
                        "Duplicate JSON object key found: %r" % (key,)
                    )
                emit_member = (
                    duplicate_keys is not DuplicateKeyPolicy.KEEP_FIRST
                )
            else:
                seen_keys.add(key)
                emit_member = True
            builder = ObjectBuilder(map_type=map_type,
                                    duplicate_keys=duplicate_keys)
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
            if emit_member:
                target.send((key, builder.value))
            # Back at the prefix after the whole object closed: the next
            # object at the prefix starts with its own independent set.
            if path != prefix:
                seen_keys = None


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
    return (
        (backend['basic_parse_basecoro'], [], config),
    )


def _parse_pipeline(backend, config):
    return (
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    )


def _items_pipeline(backend, prefix, map_type, duplicate_keys, config):
    return (
        (backend['items_basecoro'], (prefix,),
         {'map_type': map_type, 'duplicate_keys': duplicate_keys}),
        (backend['parse_basecoro'], [], {}),
        (backend['basic_parse_basecoro'], [], config)
    )


def _kvitems_pipeline(backend, prefix, map_type, duplicate_keys, config):
    return (
        (backend['kvitems_basecoro'], (prefix,),
         {'map_type': map_type, 'duplicate_keys': duplicate_keys}),
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
    def items_coro(target, prefix, map_type=None,
                   duplicate_keys=DuplicateKeyPolicy.KEEP_LAST, **config):
        return utils.chain(
            target,
            *_items_pipeline(backend, prefix, map_type, duplicate_keys, config)
        )
    return items_coro


def _make_kvitems_coro(backend):
    def kvitems_coro(target, prefix, map_type=None,
                     duplicate_keys=DuplicateKeyPolicy.KEEP_LAST, **config):
        return utils.chain(
            target,
            *_kvitems_pipeline(backend, prefix, map_type, duplicate_keys, config)
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
    def items_gen(file_obj, prefix, map_type=None,
                  duplicate_keys=DuplicateKeyPolicy.KEEP_LAST,
                  buf_size=64*1024, **config):
        return utils.coros2gen(
            file_source(file_obj, buf_size=buf_size),
            *_items_pipeline(backend, prefix, map_type, duplicate_keys, config)
        )
    return items_gen


def _make_kvitems_gen(backend):
    def kvitems_gen(file_obj, prefix, map_type=None,
                    duplicate_keys=DuplicateKeyPolicy.KEEP_LAST,
                    buf_size=64*1024, **config):
        return utils.coros2gen(
            file_source(file_obj, buf_size=buf_size),
            *_kvitems_pipeline(backend, prefix, map_type, duplicate_keys, config)
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
    def items(source, prefix, map_type=None,
              duplicate_keys=DuplicateKeyPolicy.KEEP_LAST,
              buf_size=64*1024, **config):
        source = _get_source(source)
        if is_async_file(source):
            return backend['items_async'](
                source, prefix, map_type=map_type,
                duplicate_keys=duplicate_keys, buf_size=buf_size, **config
            )
        elif is_file(source):
            return backend['items_gen'](
                source, prefix, map_type=map_type,
                duplicate_keys=duplicate_keys, buf_size=buf_size, **config
            )
        elif is_iterable(source):
            return utils.coros2gen(source,
                (backend['items_basecoro'], (prefix,),
                 {'map_type': map_type, 'duplicate_keys': duplicate_keys})
            )
        raise ValueError("Unknown source type: %r" % type(source))
    return items


def _make_kvitems(backend):
    def kvitems(source, prefix, map_type=None,
                duplicate_keys=DuplicateKeyPolicy.KEEP_LAST,
                buf_size=64*1024, **config):
        source = _get_source(source)
        if is_async_file(source):
            return backend['kvitems_async'](
                source, prefix, map_type=map_type,
                duplicate_keys=duplicate_keys, buf_size=buf_size, **config
            )
        elif is_file(source):
            return backend['kvitems_gen'](
                source, prefix, map_type=map_type,
                duplicate_keys=duplicate_keys, buf_size=buf_size, **config
            )
        elif is_iterable(source):
            return utils.coros2gen(source,
                (backend['kvitems_basecoro'], (prefix,),
                 {'map_type': map_type, 'duplicate_keys': duplicate_keys})
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


def kvitems(events, prefix, map_type=None,
            duplicate_keys=DuplicateKeyPolicy.KEEP_LAST):
    """Like ijson.kvitems, but takes events generated via ijson.parse instead of
    a file"""
    warnings.warn(_common_functions_warn, DeprecationWarning)
    return utils.coros2gen(events,
        (kvitems_basecoro, (prefix,),
         {'map_type': map_type, 'duplicate_keys': duplicate_keys})
    )


def items(events, prefix, map_type=None,
          duplicate_keys=DuplicateKeyPolicy.KEEP_LAST):
    """Like ijson.items, but takes events generated via ijson.parse instead of
    a file"""
    warnings.warn(_common_functions_warn, DeprecationWarning)
    return utils.coros2gen(events,
        (items_basecoro, (prefix,),
         {'map_type': map_type, 'duplicate_keys': duplicate_keys})
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
