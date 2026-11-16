import hashlib
import json
import math

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


def signature(*arguments):
    def _record_signature(func):
        func.signature = arguments
        return func
    return _record_signature


def canonical_signature(signature):
    """Return a normalized, JSON-friendly form of a function signature.

    Normalizing the declared types (sorting/deduplicating them) means two
    signatures that accept the same set of types compare equal, which is
    what matters when deciding whether a function table still provides the
    capability an expression was prepared against.
    """
    canonical = []
    for argument in signature or ():
        canonical.append({
            'types': sorted(set(argument.get('types') or ())),
            'variadic': bool(argument.get('variadic')),
        })
    return canonical


def signature_arity(signature):
    """Return the ``(minimum, maximum)`` argument count for a signature.

    ``maximum`` is ``None`` for variadic signatures.  The minimum is the
    number of declared arguments, mirroring the run time behavior of
    ``Functions._validate_arguments``: a variadic function such as
    ``not_null`` still requires at least one argument because its
    signature declares a single variadic argument.
    """
    if signature and signature[-1].get('variadic'):
        return len(signature), None
    return len(signature), len(signature)


def function_table_fingerprint(function_table):
    """Compute a stable capability fingerprint for a function table.

    The fingerprint covers the registered function names and their
    (normalized) signatures, so it changes whenever a function is added,
    removed, renamed, re-signatured or loses its registration (for
    example by a subclass that overrides ``_func_*`` without a
    ``@signature`` decorator).
    """
    entries = []
    for name in sorted(function_table):
        entries.append([
            name,
            canonical_signature(function_table[name]['signature']),
        ])
    payload = json.dumps(entries, sort_keys=True, separators=(',', ':'))
    return hashlib.sha256(payload.encode('utf-8')).hexdigest()


def check_arity(signature, argument_count, function_name):
    """Raise if ``argument_count`` cannot satisfy ``signature``.

    This is the single source of truth for arity validation so that static
    preparation and run time evaluation agree exactly.  Variadic functions
    require at least as many arguments as the signature declares (a
    variadic signature still declares its variadic argument), and accept
    any number beyond that.
    """
    minimum, maximum = signature_arity(signature)
    if maximum is None:
        if argument_count < minimum:
            raise exceptions.VariadictArityError(
                minimum, argument_count, function_name)
    elif argument_count != maximum:
        raise exceptions.ArityError(minimum, argument_count, function_name)


def allowed_pytypes(types):
    """Resolve declared jmespath types to python type names.

    Returns ``(allowed_types, allowed_subtypes)``.  ``allowed_subtypes`` is
    only non-empty for ``array-<subtype>`` declarations.  This is shared by
    run time type checking and static preparation so both agree.
    """
    allowed_types = []
    allowed_subtypes = []
    for declared in types:
        parts = declared.split('-', 1)
        if len(parts) == 2:
            base, subtype = parts
            allowed_types.extend(REVERSE_TYPES_MAP[base])
            allowed_subtypes.append(REVERSE_TYPES_MAP[subtype])
        else:
            allowed_types.extend(REVERSE_TYPES_MAP[declared])
    return allowed_types, allowed_subtypes


def value_matches_types(value, types):
    """Return ``True`` if ``value`` satisfies the declared ``types``.

    This mirrors ``Functions._type_check_single``/``_subtype_check`` and is
    used by static preparation, where it is only applied to values that are
    known ahead of time (JSON literals), so ``False`` is a definite error.
    """
    allowed_types, allowed_subtypes = allowed_pytypes(types)
    if type(value).__name__ not in allowed_types:
        return False
    if not allowed_subtypes:
        return True
    if not isinstance(value, list):
        return False
    if len(allowed_subtypes) == 1:
        allowed = allowed_subtypes[0]
        return all(type(element).__name__ in allowed for element in value)
    # Multiple subtype groups: like the run time implementation, the first
    # element decides which group the whole array must use.
    if not value:
        return True
    first = type(value[0]).__name__
    for subtypes in allowed_subtypes:
        if first in subtypes:
            return all(type(element).__name__ in subtypes
                       for element in value)
    return False


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

    def capability_fingerprint(self):
        """Return a fingerprint of this instance's function capabilities.

        Two ``Functions`` instances with the same registered functions and
        signatures produce the same fingerprint, regardless of class name or
        instance identity.
        """
        return function_table_fingerprint(self.FUNCTION_TABLE)

    def prepare(self, expression):
        """Statically check ``expression`` against this function table.

        ``expression`` may be a JMESPath string or an already compiled
        result.  Returns a ``jmespath.preparation.PreparedExpression``.
        """
        from jmespath import preparation
        return preparation.prepare(expression, functions=self)

    def _validate_arguments(self, args, signature, function_name):
        check_arity(signature, len(args), function_name)
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
        return allowed_pytypes(types)

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
