import math
import json
from collections.abc import Iterator

from jmespath import exceptions
from jmespath.compat import string_type as STRING_TYPE
from jmespath.compat import get_methods


# Attribute used to mark the functions shipped with the library.  The
# return values of builtin functions are trusted and therefore skip the
# explicit return value contract validation that user defined functions
# are subject to.
_BUILTIN_FUNCTION = '_jmespath_builtin_function'


# python types -> jmespath types
TYPES_MAP = {
    'bool': 'boolean',
    'list': 'array',
    'dict': 'object',
    'NoneType': 'null',
    'unicode': 'string',
    'str': 'string',
    'float': 'number',
    'int': 'number',
    'long': 'number',
    'OrderedDict': 'object',
    '_Projection': 'array',
    '_Expression': 'expref',
}


# jmespath types -> python types
REVERSE_TYPES_MAP = {
    'boolean': ('bool',),
    'array': ('list', '_Projection'),
    'object': ('dict', 'OrderedDict',),
    'null': ('NoneType',),
    'string': ('unicode', 'str'),
    'number': ('float', 'int', 'long'),
    'expref': ('_Expression',),
}


def signature(*arguments):
    def _record_signature(func):
        func.signature = arguments
        return func
    return _record_signature


class FunctionRegistry(type):
    def __init__(cls, name, bases, attrs):
        cls._populate_function_table()
        super(FunctionRegistry, cls).__init__(name, bases, attrs)

    def _populate_function_table(cls):
        function_table = {}
        # Any method with a @signature decorator that also
        # starts with "_func_" is registered as a function.
        # _func_max_by -> max_by function.
        for name, method in get_methods(cls):
            if not name.startswith('_func_'):
                continue
            signature = getattr(method, 'signature', None)
            if signature is not None:
                function_table[name[6:]] = {
                    'function': method,
                    'signature': signature,
                }
        cls.FUNCTION_TABLE = function_table


class Functions(metaclass=FunctionRegistry):

    FUNCTION_TABLE = {
    }

    #: Mapping of python type names to adapter callables.  Adapters declare
    #: which non JSON values a custom function is allowed to return.  When a
    #: value whose type name matches a key is encountered, the adapter is
    #: called with the value and must return a JSON representable value.
    #: Subclasses can extend or override this mapping to declare the adapters
    #: they support.
    RETURN_ADAPTERS = {}

    def call_function(self, function_name, resolved_args):
        try:
            spec = self.FUNCTION_TABLE[function_name]
        except KeyError:
            raise exceptions.UnknownFunctionError(
                "Unknown function: %s()" % function_name)
        function = spec['function']
        signature = spec['signature']
        self._validate_arguments(resolved_args, signature, function_name)
        result = function(self, *resolved_args)
        # The return value contract is only enforced at the function
        # boundary and only for user defined functions.  The legal output
        # of the standard builtin functions is trusted and left untouched.
        if not getattr(function, _BUILTIN_FUNCTION, False):
            result = self._validate_return_value(function_name, result)
        return result

    def _validate_return_value(self, function_name, result):
        # Validation walks the value the function actually returned and
        # nothing else, so the interpreter inputs are never retraversed.
        return self._check_json_value(
            function_name, result, '$', set())

    def _check_json_value(self, function_name, current, path, seen):
        # Iterators (and therefore generators) are rejected up front,
        # without ever being iterated.  Probing them would consume their
        # items, potentially draining an infinite iterator.
        if isinstance(current, Iterator):
            raise exceptions.ReturnValueContractError(
                function_name,
                'expected a JSON value but received %s, which is an '
                'iterator/generator and cannot be returned from a '
                'function' % type(current).__name__,
                path)
        if current is None or isinstance(current, (str, bool, int)):
            return current
        if isinstance(current, float):
            if math.isnan(current) or math.isinf(current):
                raise exceptions.ReturnValueContractError(
                    function_name,
                    'expected a finite JSON number but received %s'
                    % current,
                    path)
            return current
        if isinstance(current, list):
            return self._check_json_container(
                function_name, current, path, seen)
        if isinstance(current, dict):
            return self._check_json_object(
                function_name, current, path, seen)
        return self._adapt_return_value(
            function_name, current, path, seen)

    def _check_json_container(self, function_name, current, path, seen):
        object_id = id(current)
        # A list is a concrete, re-iterable container, so locating a cycle
        # by identity is safe and does not risk exhausting an iterator.
        if object_id in seen:
            raise exceptions.ReturnValueContractError(
                function_name,
                'circular reference detected in returned array',
                path)
        seen.add(object_id)
        try:
            return [
                self._check_json_value(
                    function_name, element,
                    '%s[%d]' % (path, index), seen)
                for index, element in enumerate(current)
            ]
        finally:
            seen.remove(object_id)

    def _check_json_object(self, function_name, current, path, seen):
        object_id = id(current)
        if object_id in seen:
            raise exceptions.ReturnValueContractError(
                function_name,
                'circular reference detected in returned object',
                path)
        for key in current:
            if not isinstance(key, STRING_TYPE):
                raise exceptions.ReturnValueContractError(
                    function_name,
                    'object keys must be strings but received key %r of '
                    'type %s' % (key, type(key).__name__),
                    path)
        seen.add(object_id)
        try:
            checked = {}
            for key, value in current.items():
                checked[key] = self._check_json_value(
                    function_name, value,
                    '%s.%s' % (path, key), seen)
            return checked
        finally:
            seen.remove(object_id)

    def _adapt_return_value(self, function_name, current, path, seen):
        adapter = self.RETURN_ADAPTERS.get(type(current).__name__)
        if adapter is None:
            raise exceptions.ReturnValueContractError(
                function_name,
                'expected a JSON value (string, number, boolean, array, '
                'object or null) but received unsupported type %s'
                % type(current).__name__,
                path)
        try:
            adapted = adapter(current)
        except exceptions.ReturnValueContractError:
            raise
        except Exception as exc:
            # Preserve the original failure so misbehaving adapters remain
            # explainable.
            raise exceptions.ReturnValueContractError(
                function_name,
                'adapter for type %s failed: %s'
                % (type(current).__name__, exc),
                path) from exc
        # The adapted value has to satisfy the same contract as any other
        # returned value.
        return self._check_json_value(
            function_name, adapted, path, seen)

    def _validate_arguments(self, args, signature, function_name):
        if signature and signature[-1].get('variadic'):
            if len(args) < len(signature):
                raise exceptions.VariadictArityError(
                    len(signature), len(args), function_name)
        elif len(args) != len(signature):
            raise exceptions.ArityError(
                len(signature), len(args), function_name)
        return self._type_check(args, signature, function_name)

    def _type_check(self, actual, signature, function_name):
        for i in range(len(signature)):
            allowed_types = signature[i]['types']
            if allowed_types:
                self._type_check_single(actual[i], allowed_types,
                                        function_name)

    def _type_check_single(self, current, types, function_name):
        # Type checking involves checking the top level type,
        # and in the case of arrays, potentially checking the types
        # of each element.
        allowed_types, allowed_subtypes = self._get_allowed_pytypes(types)
        # We're not using isinstance() on purpose.
        # The type model for jmespath does not map
        # 1-1 with python types (booleans are considered
        # integers in python for example).
        actual_typename = type(current).__name__
        if actual_typename not in allowed_types:
            raise exceptions.JMESPathTypeError(
                function_name, current,
                self._convert_to_jmespath_type(actual_typename), types)
        # If we're dealing with a list type, we can have
        # additional restrictions on the type of the list
        # elements (for example a function can require a
        # list of numbers or a list of strings).
        # Arrays are the only types that can have subtypes.
        if allowed_subtypes:
            self._subtype_check(current, allowed_subtypes,
                                types, function_name)

    def _get_allowed_pytypes(self, types):
        allowed_types = []
        allowed_subtypes = []
        for t in types:
            type_ = t.split('-', 1)
            if len(type_) == 2:
                type_, subtype = type_
                allowed_subtypes.append(REVERSE_TYPES_MAP[subtype])
            else:
                type_ = type_[0]
            allowed_types.extend(REVERSE_TYPES_MAP[type_])
        return allowed_types, allowed_subtypes

    def _subtype_check(self, current, allowed_subtypes, types, function_name):
        if len(allowed_subtypes) == 1:
            # The easy case, we know up front what type
            # we need to validate.
            allowed_subtypes = allowed_subtypes[0]
            for element in current:
                actual_typename = type(element).__name__
                if actual_typename not in allowed_subtypes:
                    raise exceptions.JMESPathTypeError(
                        function_name, element, actual_typename, types)
        elif len(allowed_subtypes) > 1 and current:
            # Dynamic type validation.  Based on the first
            # type we see, we validate that the remaining types
            # match.
            first = type(current[0]).__name__
            for subtypes in allowed_subtypes:
                if first in subtypes:
                    allowed = subtypes
                    break
            else:
                raise exceptions.JMESPathTypeError(
                    function_name, current[0], first, types)
            for element in current:
                actual_typename = type(element).__name__
                if actual_typename not in allowed:
                    raise exceptions.JMESPathTypeError(
                        function_name, element, actual_typename, types)

    @signature({'types': ['number']})
    def _func_abs(self, arg):
        return abs(arg)

    @signature({'types': ['array-number']})
    def _func_avg(self, arg):
        if arg:
            return sum(arg) / len(arg)
        else:
            return None

    @signature({'types': [], 'variadic': True})
    def _func_not_null(self, *arguments):
        for argument in arguments:
            if argument is not None:
                return argument

    @signature({'types': []})
    def _func_to_array(self, arg):
        if isinstance(arg, list):
            return arg
        else:
            return [arg]

    @signature({'types': []})
    def _func_to_string(self, arg):
        if isinstance(arg, STRING_TYPE):
            return arg
        else:
            return json.dumps(arg, separators=(',', ':'),
                              default=str)

    @signature({'types': []})
    def _func_to_number(self, arg):
        if isinstance(arg, (list, dict, bool)):
            return None
        elif arg is None:
            return None
        elif isinstance(arg, (int, float)):
            return arg
        else:
            try:
                return int(arg)
            except ValueError:
                try:
                    return float(arg)
                except ValueError:
                    return None

    @signature({'types': ['array', 'string']}, {'types': []})
    def _func_contains(self, subject, search):
        return search in subject

    @signature({'types': ['string', 'array', 'object']})
    def _func_length(self, arg):
        return len(arg)

    @signature({'types': ['string']}, {'types': ['string']})
    def _func_ends_with(self, search, suffix):
        return search.endswith(suffix)

    @signature({'types': ['string']}, {'types': ['string']})
    def _func_starts_with(self, search, suffix):
        return search.startswith(suffix)

    @signature({'types': ['array', 'string']})
    def _func_reverse(self, arg):
        if isinstance(arg, STRING_TYPE):
            return arg[::-1]
        else:
            return list(reversed(arg))

    @signature({"types": ['number']})
    def _func_ceil(self, arg):
        return math.ceil(arg)

    @signature({"types": ['number']})
    def _func_floor(self, arg):
        return math.floor(arg)

    @signature({"types": ['string']}, {"types": ['array-string']})
    def _func_join(self, separator, array):
        return separator.join(array)

    @signature({'types': ['expref']}, {'types': ['array']})
    def _func_map(self, expref, arg):
        result = []
        for element in arg:
            result.append(expref.visit(expref.expression, element))
        return result

    @signature({"types": ['array-number', 'array-string']})
    def _func_max(self, arg):
        if arg:
            return max(arg)
        else:
            return None

    @signature({"types": ["object"], "variadic": True})
    def _func_merge(self, *arguments):
        merged = {}
        for arg in arguments:
            merged.update(arg)
        return merged

    @signature({"types": ['array-number', 'array-string']})
    def _func_min(self, arg):
        if arg:
            return min(arg)
        else:
            return None

    @signature({"types": ['array-string', 'array-number']})
    def _func_sort(self, arg):
        return list(sorted(arg))

    @signature({"types": ['array-number']})
    def _func_sum(self, arg):
        return sum(arg)

    @signature({"types": ['object']})
    def _func_keys(self, arg):
        # To be consistent with .values()
        # should we also return the indices of a list?
        return list(arg.keys())

    @signature({"types": ['object']})
    def _func_values(self, arg):
        return list(arg.values())

    @signature({'types': []})
    def _func_type(self, arg):
        if isinstance(arg, STRING_TYPE):
            return "string"
        elif isinstance(arg, bool):
            return "boolean"
        elif isinstance(arg, list):
            return "array"
        elif isinstance(arg, dict):
            return "object"
        elif isinstance(arg, (float, int)):
            return "number"
        elif arg is None:
            return "null"

    @signature({'types': ['array']}, {'types': ['expref']})
    def _func_sort_by(self, array, expref):
        if not array:
            return array
        # sort_by allows for the expref to be either a number of
        # a string, so we have some special logic to handle this.
        # We evaluate the first array element and verify that it's
        # either a string of a number.  We then create a key function
        # that validates that type, which requires that remaining array
        # elements resolve to the same type as the first element.
        required_type = self._convert_to_jmespath_type(
            type(expref.visit(expref.expression, array[0])).__name__)
        if required_type not in ['number', 'string']:
            raise exceptions.JMESPathTypeError(
                'sort_by', array[0], required_type, ['string', 'number'])
        keyfunc = self._create_key_func(expref,
                                        [required_type],
                                        'sort_by')
        return list(sorted(array, key=keyfunc))

    @signature({'types': ['array']}, {'types': ['expref']})
    def _func_min_by(self, array, expref):
        keyfunc = self._create_key_func(expref,
                                        ['number', 'string'],
                                        'min_by')
        if array:
            return min(array, key=keyfunc)
        else:
            return None

    @signature({'types': ['array']}, {'types': ['expref']})
    def _func_max_by(self, array, expref):
        keyfunc = self._create_key_func(expref,
                                        ['number', 'string'],
                                        'max_by')
        if array:
            return max(array, key=keyfunc)
        else:
            return None

    def _create_key_func(self, expref, allowed_types, function_name):
        def keyfunc(x):
            result = expref.visit(expref.expression, x)
            actual_typename = type(result).__name__
            jmespath_type = self._convert_to_jmespath_type(actual_typename)
            # allowed_types is in term of jmespath types, not python types.
            if jmespath_type not in allowed_types:
                raise exceptions.JMESPathTypeError(
                    function_name, result, jmespath_type, allowed_types)
            return result
        return keyfunc

    def _convert_to_jmespath_type(self, pyobject):
        return TYPES_MAP.get(pyobject, 'unknown')


# Every function declared directly on the standard Functions class is a
# builtin: its legal output is part of the library's contract and must not
# be revalidated.  Methods that subclasses override lose this marker and
# are checked against the return value contract instead.
for _builtin_spec in Functions.FUNCTION_TABLE.values():
    setattr(_builtin_spec['function'], _BUILTIN_FUNCTION, True)
