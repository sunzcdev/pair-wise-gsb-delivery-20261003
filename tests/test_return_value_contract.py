import json
import math
import itertools

from tests import unittest

import jmespath
from jmespath import functions
from jmespath import exceptions


def search(expression, functions_instance, data=None):
    options = jmespath.Options(custom_functions=functions_instance)
    return jmespath.search(expression, data if data is not None else {},
                           options=options)


class TestReturnValueContract(unittest.TestCase):
    def test_accepts_all_json_value_types(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_everything(self, arg):
                return {
                    'string': 'value',
                    'integer': 1,
                    'float': 1.5,
                    'boolean': True,
                    'null': None,
                    'array': [1, 'two', False, None],
                    'nested': {'object': {'key': [1, 2, 3]}},
                }

        result = search('everything(@)', CustomFunctions())
        # The returned value must be a fully JSON serializable value.
        self.assertEqual(json.loads(json.dumps(result)), result)

    def test_rejects_generator_without_consuming_it(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_gen(self, arg):
                return (i for i in range(3))

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('gen(@)', CustomFunctions())
        self.assertEqual(ctx.exception.function_name, 'gen')
        self.assertIn('iterator', str(ctx.exception))

    def test_does_not_drain_infinite_iterator(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_infinite(self, arg):
                return itertools.count()

        with self.assertRaises(exceptions.ReturnValueContractError):
            search('infinite(@)', CustomFunctions())

    def test_rejects_nan(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_nan(self, arg):
                return float('nan')

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('nan(@)', CustomFunctions())
        self.assertIn('number', str(ctx.exception))

    def test_rejects_infinity(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_inf(self, arg):
                return float('inf')

        with self.assertRaises(exceptions.ReturnValueContractError):
            search('inf(@)', CustomFunctions())

    def test_accepts_finite_float(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_pi(self, arg):
                return math.pi

        self.assertEqual(search('pi(@)', CustomFunctions()), math.pi)

    def test_rejects_non_string_object_keys(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_bad_keys(self, arg):
                return {1: 'one', ('tuple',): 'key'}

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('bad_keys(@)', CustomFunctions())
        self.assertIn('keys must be strings', str(ctx.exception))

    def test_rejects_unsupported_python_type(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_a_set(self, arg):
                return {1, 2, 3}

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('a_set(@)', CustomFunctions())
        self.assertIn('set', str(ctx.exception))

    def test_locates_circular_array_with_path(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_cycle(self, arg):
                cyclic = []
                cyclic.append(cyclic)
                return {'outer': cyclic}

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('cycle(@)', CustomFunctions())
        self.assertIn('circular reference', str(ctx.exception))
        self.assertEqual(ctx.exception.path, '$.outer[0]')

    def test_locates_circular_object_with_path(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_cycle(self, arg):
                cyclic = {}
                cyclic['self'] = cyclic
                return cyclic

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('cycle(@)', CustomFunctions())
        self.assertEqual(ctx.exception.path, '$.self')

    def test_allows_repeated_non_circular_references(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_shared(self, arg):
                child = {'key': 'value'}
                return [child, child]

        result = search('shared(@)', CustomFunctions())
        self.assertEqual(result, [{'key': 'value'}, {'key': 'value'}])

    def test_reports_path_of_nested_invalid_value(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_nested(self, arg):
                return {'a': [1, {'b': float('nan')}]}

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('nested(@)', CustomFunctions())
        self.assertEqual(ctx.exception.path, '$.a[1].b')

    def test_rejects_iterator_nested_in_structure(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_nested_gen(self, arg):
                return {'items': (i for i in range(2))}

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('nested_gen(@)', CustomFunctions())
        self.assertEqual(ctx.exception.path, '$.items')


class TestReturnAdapters(unittest.TestCase):
    def test_declared_adapter_converts_unsupported_type(self):
        class CustomFunctions(functions.Functions):
            RETURN_ADAPTERS = {
                'datetime': lambda value: value.isoformat(),
            }

            @functions.signature({'types': []})
            def _func_when(self, arg):
                from datetime import datetime
                return {'now': datetime(2026, 11, 17)}

        result = search('when(@)', CustomFunctions())
        self.assertEqual(result, {'now': '2026-11-17T00:00:00'})

    def test_adapter_used_for_nested_values(self):
        class CustomFunctions(functions.Functions):
            RETURN_ADAPTERS = {
                'Decimal': lambda value: float(value),
            }

            @functions.signature({'types': []})
            def _func_amounts(self, arg):
                from decimal import Decimal
                return [Decimal('1.5'), {'total': Decimal('2')}]

        result = search('amounts(@)', CustomFunctions())
        self.assertEqual(result, [1.5, {'total': 2.0}])

    def test_adapter_output_is_revalidated(self):
        class CustomFunctions(functions.Functions):
            RETURN_ADAPTERS = {
                'Frob': lambda value: float('nan'),
            }

            @functions.signature({'types': []})
            def _func_frob(self, arg):
                class Frob(object):
                    pass
                return Frob()

        with self.assertRaises(exceptions.ReturnValueContractError):
            search('frob(@)', CustomFunctions())

    def test_adapter_failure_preserves_original_cause(self):
        def explode(value):
            raise RuntimeError('adapter exploded')

        class CustomFunctions(functions.Functions):
            RETURN_ADAPTERS = {'Boom': explode}

            @functions.signature({'types': []})
            def _func_boom(self, arg):
                class Boom(object):
                    pass
                return Boom()

        with self.assertRaises(exceptions.ReturnValueContractError) as ctx:
            search('boom(@)', CustomFunctions())
        self.assertIsInstance(ctx.exception.__cause__, RuntimeError)
        self.assertEqual(str(ctx.exception.__cause__), 'adapter exploded')
        self.assertIn('adapter for type Boom failed', str(ctx.exception))

    def test_adapter_can_convert_tuple_to_list(self):
        class CustomFunctions(functions.Functions):
            RETURN_ADAPTERS = {'tuple': list}

            @functions.signature({'types': []})
            def _func_tuple(self, arg):
                return (1, 2, 3)

        self.assertEqual(search('tuple(@)', CustomFunctions()), [1, 2, 3])


class TestBuiltinFunctionsAreTrusted(unittest.TestCase):
    def test_builtin_functions_are_not_revalidated(self):
        # All standard builtin functions keep their existing behavior.
        self.assertEqual(jmespath.search('avg(@)', [1, 2, 3]), 2)
        self.assertEqual(jmespath.search('length(@)', [1, 2]), 2)
        self.assertEqual(jmespath.search('keys(@)', {'a': 1}), ['a'])
        self.assertIsNone(jmespath.search('max(@)', []))

    def test_overridden_builtin_is_subject_to_contract(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': ['array']})
            def _func_length(self, arg):
                return iter(arg)

        with self.assertRaises(exceptions.ReturnValueContractError):
            search('length(@)', CustomFunctions(), [1, 2, 3])

    def test_inherited_builtin_remains_trusted(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': ['number']}, {'types': ['number']})
            def _func_add(self, x, y):
                return x + y

        options = jmespath.Options(custom_functions=CustomFunctions())
        # A custom function is validated...
        self.assertEqual(
            jmespath.search('add(`1`, `2`)', {}, options=options), 3)
        # ...while inherited builtins keep working unchanged.
        self.assertEqual(
            jmespath.search('length(@)', [1, 2, 3], options=options), 3)


class TestContractEnforcedAtBoundary(unittest.TestCase):
    def test_input_is_not_traversed_by_contract_check(self):
        # The contract check walks the value returned by the function, not
        # the interpreter input.  Returning a scalar extracted from the
        # input means the input is never iterated for validation.
        input_iterations = []
        real_list = [1, 2, 3]

        class InputProxy(object):
            def __getitem__(self, index):
                return real_list[index]

            def __iter__(self):
                input_iterations.append(1)
                return iter(real_list)

        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_first(self, arg):
                return arg[0]

        result = search('first(@)', CustomFunctions(), InputProxy())
        self.assertEqual(result, 1)
        # The scalar return value was accepted without the input ever
        # being iterated as part of contract validation.
        self.assertEqual(input_iterations, [])

    def test_returned_container_is_traversed_only_once(self):
        class CountingList(list):
            def __init__(self, *args, **kwargs):
                super(CountingList, self).__init__(*args, **kwargs)
                self.iterations = 0

            def __iter__(self):
                self.iterations += 1
                return super(CountingList, self).__iter__()

        returned = CountingList([1, 2, 3])

        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_items(self, arg):
                return returned

        result = search('items(@)', CustomFunctions())
        self.assertEqual(result, [1, 2, 3])
        # A single validation pass over the returned container.
        self.assertEqual(returned.iterations, 1)

    def test_error_is_jmespath_error_subclass(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': []})
            def _func_bad(self, arg):
                return object()

        with self.assertRaises(exceptions.JMESPathError):
            search('bad(@)', CustomFunctions())
