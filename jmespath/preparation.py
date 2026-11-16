"""Optional static preparation for compiled JMESPath expressions.

The regular :func:`jmespath.compile` entry point deliberately knows nothing
about the ``Functions`` implementation an expression will eventually run
against.  That keeps compilation cheap, but it also means a query with a
typo in a function name, or with an argument count that can never work, is
only discovered when the query finally evaluates a document.

:func:`jmespath.prepare` (and ``ParsedResult.prepare`` /
``Functions.prepare``) adds an *opt in* pre-flight step.  Given a concrete
``Functions`` instance it walks the parsed AST and reports:

* which functions the expression calls and how many arguments each call
  passes (``function_requirements`` / ``required_functions``), and
* the argument type checks that can only be confirmed at run time because
  the argument value comes from the document
  (``type_requirements``).

Unknown functions and impossible argument counts raise immediately, using
the same ``UnknownFunctionError`` / ``ArityError`` /
``VariadictArityError`` exceptions the evaluator raises so the two paths
cannot drift apart.

Expression references (``&expr``) are never evaluated while preparing.  An
expref argument is statically known to be of the ``expref`` type; its body
is only traversed so nested calls are checked too.

The resulting :class:`PreparedExpression` is bound to the capability
fingerprint of the functions it needs.  Running it against a different,
incompatible function table fails loudly instead of silently pretending to
still be prepared.
"""
from jmespath import exceptions
from jmespath import functions as functions_module


_EXPREF_TYPE = 'expref'


def _jmespath_type_of(value):
    return functions_module.TYPES_MAP.get(type(value).__name__, 'unknown')


def _static_argument(node):
    """Describe an argument that is fully known before run time.

    Returns a ``(jmespath_type, value)`` tuple, or ``(None, None)`` when the
    value depends on the document (or on the result of another call) and
    therefore can only be checked at run time.
    """
    node_type = node['type']
    if node_type == 'expref':
        # Never evaluate the referenced expression while preparing.
        return _EXPREF_TYPE, None
    if node_type == 'literal':
        value = node['value']
        return _jmespath_type_of(value), value
    return None, None


def _declared_types(signature, index):
    """Return the declared types for argument ``index`` of a signature."""
    if not signature or index >= len(signature):
        return ()
    return tuple(signature[index].get('types') or ())


def _only_accepts_expref(types):
    """Return ``True`` if ``expref`` is the only accepted type.

    Only an expression reference can produce an ``expref`` value, so a
    document value (or a nested call result) can never satisfy such an
    argument.  This is the one type mismatch that is provable statically
    even though the argument itself is not a literal.
    """
    return bool(types) and set(types) == set([_EXPREF_TYPE])


def _function_table(functions):
    table = getattr(functions, 'FUNCTION_TABLE', None)
    if not isinstance(table, dict):
        raise TypeError(
            'Expected a jmespath Functions instance or subclass, got %r'
            % (functions,))
    return table


def _resolve_functions(functions, options):
    if functions is not None:
        if options is not None and options.custom_functions is not None:
            raise ValueError(
                'Specify either functions or options.custom_functions, '
                'not both.')
    elif options is not None and options.custom_functions is not None:
        functions = options.custom_functions
    else:
        functions = functions_module.Functions()
    if isinstance(functions, type):
        functions = functions()
    _function_table(functions)
    return functions


def _capability_fingerprint(function_table, names):
    """Fingerprint the capabilities a prepared expression depends on.

    Only the functions actually called by the expression are included, so
    an unrelated function being added to or removed from a table does not
    falsely invalidate a prepared expression.  Any change to a required
    function (missing, renamed, or re-signatured) does.
    """
    subset = dict((name, function_table[name]) for name in names)
    return functions_module.function_table_fingerprint(subset)


class _CallAnalyzer(object):
    def __init__(self, function_table):
        self._table = function_table
        self._function_requirements = {}
        self._type_requirements = {}
        self._expression_references = {}
        self._referenced_arguments = set()

    def analyze(self, node):
        self._walk(node)
        return (self._function_requirements, self._type_requirements,
                list(self._expression_references.values()))

    def _walk(self, node):
        if node['type'] == 'function_expression':
            self._check_call(node)
        for child in node.get('children', ()):
            # Some node types (for example ``slice``) carry plain values in
            # their children list, so only recurse into AST nodes.
            if isinstance(child, dict):
                self._walk(child)

    def _check_call(self, node):
        name = node['value']
        arguments = node['children']
        try:
            spec = self._table[name]
        except KeyError:
            raise exceptions.UnknownFunctionError(
                'Unknown function: %s()' % name)
        signature = spec['signature']
        functions_module.check_arity(signature, len(arguments), name)
        self._record_requirement(name, signature, len(arguments))
        self._check_argument_types(name, signature, arguments)

    def _record_requirement(self, name, signature, argument_count):
        requirement = self._function_requirements.get(name)
        if requirement is None:
            minimum, maximum = functions_module.signature_arity(signature)
            requirement = {
                'name': name,
                'minimum_arity': minimum,
                'maximum_arity': maximum,
                'variadic': maximum is None,
                'signature': functions_module.canonical_signature(signature),
                'calls': 0,
                'argument_counts': set(),
            }
            self._function_requirements[name] = requirement
        requirement['calls'] += 1
        requirement['argument_counts'].add(argument_count)

    def _check_argument_types(self, name, signature, arguments):
        # Mirror the run time ``Functions._type_check``: only the declared
        # signature positions are type checked.  Extra arguments to a
        # variadic function carry no declared type, so they are not
        # validated either statically or at run time.
        for index in range(min(len(arguments), len(signature))):
            argument = arguments[index]
            expected_types = _declared_types(signature, index)
            if not expected_types:
                # An empty type list means "any type"; nothing to confirm.
                continue
            static_type, value = _static_argument(argument)
            if static_type is None:
                if (_only_accepts_expref(expected_types) and
                        argument['type'] != 'function_expression'):
                    # Only an ``&expr`` argument can ever produce an
                    # expref, so this argument (a document value) can never
                    # satisfy the parameter.  A nested function call is
                    # excluded because its return type is not modelled.
                    raise exceptions.JMESPathTypeError(
                        name, '<document value>', 'unknown',
                        list(expected_types))
                # The value depends on the document, so the type check can
                # only happen at run time.  Report it, do not guess.
                self._record_type_requirement(
                    name, index, expected_types)
                continue
            if static_type == _EXPREF_TYPE:
                if _EXPREF_TYPE not in expected_types:
                    raise exceptions.JMESPathTypeError(
                        name, '<expression reference>', _EXPREF_TYPE,
                        list(expected_types))
                self._record_expression_reference(name, index, argument)
                continue
            if not functions_module.value_matches_types(
                    value, expected_types):
                raise exceptions.JMESPathTypeError(
                    name, value, static_type, list(expected_types))

    def _record_type_requirement(self, name, index, expected_types):
        key = (name, index, expected_types)
        requirement = self._type_requirements.get(key)
        if requirement is None:
            requirement = {
                'function': name,
                'argument_index': index,
                'expected_types': list(expected_types),
                'confirmed_at': 'runtime',
            }
            self._type_requirements[key] = requirement

    def _record_expression_reference(self, name, index, argument):
        # Record that the argument is an expression reference, but never
        # evaluate it here: the referenced expression is applied to document
        # values later, by the function that receives it.
        key = (name, index)
        if key in self._referenced_arguments:
            return
        self._referenced_arguments.add(key)
        self._expression_references[key] = {
            'function': name,
            'argument_index': index,
            'type': _EXPREF_TYPE,
            'confirmed_at': 'prepare',
        }


class PreparedExpression(object):
    """A compiled expression checked against a specific function table.

    Instances are produced by :func:`jmespath.prepare` and are safe to reuse
    across documents, provided the function table still offers the
    capabilities recorded in ``capability_fingerprint``.
    """

    def __init__(self, parsed_result, functions, function_requirements,
                 type_requirements, expression_references=()):
        self.expression = parsed_result.expression
        self.parsed = parsed_result
        self.functions = functions
        self.function_requirements = function_requirements
        self.required_functions = tuple(sorted(function_requirements))
        self.type_requirements = tuple(type_requirements.values())
        self.expression_references = tuple(expression_references)
        function_table = _function_table(functions)
        self.table_fingerprint = \
            functions_module.function_table_fingerprint(function_table)
        self.capability_fingerprint = _capability_fingerprint(
            function_table, self.required_functions)

    @property
    def function_names(self):
        """The names of every function the expression calls."""
        return self.required_functions

    @property
    def requirements(self):
        """A plain, JSON-friendly summary of what preparation found.

        The returned structure is a detached copy; mutating it does not
        change the prepared expression.
        """
        return {
            'expression': self.expression,
            'functions': [
                self._requirement_summary(name)
                for name in self.required_functions
            ],
            'type_requirements': [
                dict(r, expected_types=list(r['expected_types']))
                for r in self.type_requirements
            ],
            'expression_references': [
                dict(r) for r in self.expression_references],
            'capability_fingerprint': self.capability_fingerprint,
            'table_fingerprint': self.table_fingerprint,
        }

    def _requirement_summary(self, name):
        requirement = self.function_requirements[name]
        return {
            'name': requirement['name'],
            'minimum_arity': requirement['minimum_arity'],
            'maximum_arity': requirement['maximum_arity'],
            'variadic': requirement['variadic'],
            'signature': [dict(argument, types=list(argument['types']))
                          for argument in requirement['signature']],
            'calls': requirement['calls'],
            'argument_counts': sorted(requirement['argument_counts']),
        }

    def _incompatibilities(self, functions):
        function_table = _function_table(functions)
        missing = []
        changed = []
        for name in self.required_functions:
            if name not in function_table:
                missing.append(name)
                continue
            expected = self.function_requirements[name]['signature']
            actual = functions_module.canonical_signature(
                function_table[name]['signature'])
            if expected != actual:
                changed.append(name)
        return missing, changed

    def is_prepared_for(self, functions=None):
        """Return ``True`` if ``functions`` still provides what we need."""
        if functions is None:
            functions = self.functions
        if isinstance(functions, type):
            functions = functions()
        missing, changed = self._incompatibilities(functions)
        return not missing and not changed

    def verify(self, functions=None):
        """Return ``functions`` if it can run this expression, else raise.

        Raises :class:`jmespath.exceptions.FunctionCapabilityMismatchError`
        naming the functions that are missing or whose signature changed.
        """
        if functions is None:
            functions = self.functions
        if isinstance(functions, type):
            functions = functions()
        missing, changed = self._incompatibilities(functions)
        if not missing and not changed:
            return functions
        details = []
        if missing:
            details.append('missing functions: %s' % ', '.join(
                '%s()' % name for name in missing))
        if changed:
            details.append('incompatible signatures: %s' % ', '.join(
                '%s()' % name for name in changed))
        raise exceptions.FunctionCapabilityMismatchError(
            'Expression was prepared against a different function table (%s)'
            % '; '.join(details),
            missing=missing, changed=changed)

    def search(self, value, options=None):
        from jmespath.visitor import Options
        if options is not None and options.custom_functions is not None:
            target_functions = self.verify(options.custom_functions)
            dict_cls = options.dict_cls
        else:
            target_functions = self.verify(self.functions)
            dict_cls = None if options is None else options.dict_cls
        prepared_options = Options(
            dict_cls=dict_cls, custom_functions=target_functions)
        return self.parsed.search(value, options=prepared_options)

    def __repr__(self):
        return 'PreparedExpression(%r, functions=%s)' % (
            self.expression, sorted(self.required_functions))


def prepare(expression, functions=None, options=None):
    """Statically check ``expression`` against a function table.

    :param expression: a JMESPath string or an already parsed result.
    :param functions: an optional ``jmespath.functions.Functions`` instance
        (or subclass) to check against.  Defaults to the built-in table.
    :param options: an optional ``jmespath.Options`` whose
        ``custom_functions`` should be used when ``functions`` is omitted.

    Returns a :class:`PreparedExpression`.
    """
    from jmespath import parser
    resolved_functions = _resolve_functions(functions, options)
    if isinstance(expression, parser.ParsedResult):
        parsed_result = expression
    else:
        parsed_result = parser.Parser().parse(expression)
    analyzer = _CallAnalyzer(_function_table(resolved_functions))
    (function_requirements, type_requirements,
     expression_references) = analyzer.analyze(parsed_result.parsed)
    return PreparedExpression(
        parsed_result, resolved_functions, function_requirements,
        type_requirements, expression_references)
