import math
import json
import numbers

from jmespath import exceptions
from jmespath.compat import string_type as STRING_TYPE
from jmespath.compat import get_methods


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


#: Modes accepted by ``join_by()``.  ``inner`` keeps only matches,
#: ``left`` is a left outer join that keeps unmatched left records with a
#: null right side.
JOIN_MODES = ('inner', 'left')


def _canonical_number(value):
    # JSON only has one number type, so 1 and 1.0 are the same key.
    # ``bool`` is checked before ``int`` by ``_json_key`` because
    # ``bool`` subclasses ``int`` in python.
    if isinstance(value, float) and not math.isfinite(value):
        # JSON numbers are finite, so NaN and infinity have no well
        # defined JSON equality and cannot be used as a key.
        raise TypeError('number is not finite')
    try:
        as_int = int(value)
    except (TypeError, ValueError, OverflowError):
        raise TypeError('number is not a real JSON number')
    if value == as_int:
        # ``int()`` is exact for the integer valued numbers we accept
        # (including arbitrary precision int), so this does not lose
        # information.  The equality check keeps 1.5 as a float key.
        return as_int
    return value


def _json_key(value):
    """Return a hashable key implementing JSON type equality.

    Every JSON value maps to a unique representation, so ``true`` never
    equals ``1``, ``"1"`` never equals ``1``, and two objects are equal
    regardless of their key order.  Values outside of the JSON domain
    (for example a ``datetime`` returned by a custom function) raise a
    ``TypeError`` because there is no JSON equality to apply to them.
    """
    if value is None:
        return ('null',)
    if isinstance(value, bool):
        return ('boolean', value)
    if isinstance(value, numbers.Number):
        return ('number', _canonical_number(value))
    if isinstance(value, STRING_TYPE):
        return ('string', value)
    if isinstance(value, list):
        return ('array', tuple(_json_key(element) for element in value))
    if isinstance(value, dict):
        pairs = []
        for key, element in value.items():
            if not isinstance(key, STRING_TYPE):
                raise TypeError('object key is not a string')
            pairs.append((key, _json_key(element)))
        # Objects are equal by their members, so the key order of an
        # object must not affect key equality.
        return ('object', frozenset(pairs))
    raise TypeError('value of type %s is not a JSON value' % (
        type(value).__name__,))


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

    def call_function(self, function_name, resolved_args):
        try:
            spec = self.FUNCTION_TABLE[function_name]
        except KeyError:
            raise exceptions.UnknownFunctionError(
                "Unknown function: %s()" % function_name)
        function = spec['function']
        signature = spec['signature']
        self._validate_arguments(resolved_args, signature, function_name)
        return function(self, *resolved_args)

    def _validate_arguments(self, args, signature, function_name):
        if signature and signature[-1].get('variadic'):
            if len(args) < len(signature):
                raise exceptions.VariadictArityError(
                    len(signature), len(args), function_name)
        else:
            # Signature entries are required by default.  A trailing
            # entry may be marked ``optional`` to let a function take a
            # shorter argument list (join_by() uses this for its mode).
            required = self._required_arity(signature)
            if len(args) < required:
                if required == len(signature):
                    raise exceptions.ArityError(
                        required, len(args), function_name)
                raise exceptions.OptionalArityError(
                    required, len(signature), len(args), function_name)
            if len(args) > len(signature):
                raise exceptions.ArityError(
                    len(signature), len(args), function_name)
        return self._type_check(args, signature, function_name)

    def _required_arity(self, signature):
        required = 0
        for argument in signature:
            if argument.get('optional'):
                break
            required += 1
        return required

    def _type_check(self, actual, signature, function_name):
        # Optional trailing arguments may be absent from ``actual``.
        for i in range(min(len(signature), len(actual))):
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

    @signature({'types': ['array']}, {'types': ['expref']},
               {'types': ['array']}, {'types': ['expref']},
               {'types': ['string'], 'optional': True})
    def _func_join_by(self, left, left_expref, right, right_expref,
                      mode='inner'):
        return self._join_by(left, left_expref, right, right_expref, mode)

    def _join_by(self, left, left_expref, right, right_expref, mode):
        if mode not in JOIN_MODES:
            raise exceptions.JMESPathValueError(
                'join_by', mode, 'one of: %s' % ', '.join(
                    repr(m) for m in JOIN_MODES))
        # Only the right hand side is indexed.  A right record whose key
        # expression is null (which includes a missing field) is left out
        # of the index, and a left record with a null key never looks up,
        # so null and missing keys never join records together.
        index = {}
        for element in right:
            key = self._join_key(right_expref, element)
            if key is not None:
                # Keeping every right record in encounter order means a
                # duplicated key expands in right order within a left
                # record, and left records are walked in left order.
                index.setdefault(key, []).append(element)
        result = []
        for element in left:
            key = self._join_key(left_expref, element)
            matches = () if key is None else index.get(key, ())
            if matches:
                for match in matches:
                    result.append({'left': element, 'right': match})
            elif mode == 'left':
                result.append({'left': element, 'right': None})
        return result

    def _join_key(self, expref, element):
        key = expref.visit(expref.expression, element)
        if key is None:
            return None
        try:
            return _json_key(key)
        except TypeError as e:
            raise exceptions.JMESPathValueError(
                'join_by', key, 'a JSON value (%s)' % e)

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
