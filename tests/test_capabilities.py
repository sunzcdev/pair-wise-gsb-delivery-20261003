from tests import unittest

import jmespath
from jmespath import ast
from jmespath import capabilities
from jmespath import exceptions
from jmespath import functions
from jmespath import visitor


def allow(*names):
    return jmespath.Options(
        capability_policy=capabilities.CapabilityPolicy(
            allowed_functions=list(names)))


def deny(*names):
    return jmespath.Options(
        capability_policy=capabilities.CapabilityPolicy(
            denied_functions=list(names)))


class CustomFunctions(functions.Functions):
    # Overrides the built in ``length`` implementation under the same
    # registered name and adds a brand new function.
    @functions.signature(
        {'types': ['string', 'array', 'object', 'null']})
    def _func_length(self, s):
        return 999 if s is not None else 0

    @functions.signature({'types': ['number']})
    def _func_double(self, x):
        return x * 2


class TestCapabilityPolicyConstruction(unittest.TestCase):
    def test_requires_a_rule_set(self):
        with self.assertRaises(ValueError):
            capabilities.CapabilityPolicy()

    def test_rejects_both_rule_sets(self):
        with self.assertRaises(ValueError):
            capabilities.CapabilityPolicy(
                allowed_functions=['a'], denied_functions=['b'])

    def test_names_are_stored_as_a_deduped_frozen_set(self):
        policy = capabilities.CapabilityPolicy(
            allowed_functions=['a', 'b', 'a'])
        self.assertEqual(policy.mode, 'allow')
        self.assertEqual(policy.functions, frozenset(['a', 'b']))
        self.assertIsInstance(policy.functions, frozenset)

    def test_deny_mode(self):
        policy = capabilities.CapabilityPolicy(
            denied_functions=['a'])
        self.assertEqual(policy.mode, 'deny')
        self.assertFalse(policy.is_allowed('a'))
        self.assertTrue(policy.is_allowed('b'))

    def test_allow_mode(self):
        policy = capabilities.CapabilityPolicy(
            allowed_functions=['a'])
        self.assertTrue(policy.is_allowed('a'))
        self.assertFalse(policy.is_allowed('b'))

    def test_check_raises_for_denied_name(self):
        policy = capabilities.CapabilityPolicy(
            denied_functions=['abs'])
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            policy.check('abs')
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertIsNone(cm.exception.position)


class TestCapabilityPolicyEnforcement(unittest.TestCase):
    def setUp(self):
        self.data = {'a': {'b': 'hello'}, 'nums': [3, 1, 2]}

    def test_no_policy_keeps_default_behavior(self):
        self.assertEqual(
            jmespath.search('length(a.b)', self.data), 5)

    def test_allowed_function_runs(self):
        self.assertEqual(
            jmespath.search('length(a.b)', self.data,
                            allow('length')), 5)

    def test_denied_function_is_rejected_with_position(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('length(a.b)', self.data,
                            deny('length'))
        error = cm.exception
        self.assertEqual(error.function_name, 'length')
        self.assertEqual(error.position, 0)
        self.assertEqual(error.expression, 'length(a.b)')
        message = str(error)
        self.assertIn('length()', message)
        self.assertIn('capability policy', message)
        self.assertIn('"length(a.b)"', message)
        # The caret must point exactly at the function name token.
        self.assertIn('\n^', message)

    def test_allow_list_denies_everything_else(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('abs(`-1`)', self.data, allow('length'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 0)

    def test_position_points_at_the_function_name_token(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('  foo .  abs ( @ )',
                            {'foo': -1}, deny('abs'))
        self.assertEqual(cm.exception.position, 9)

    def test_short_circuited_and_branch_is_still_checked(self):
        # The right hand side would never evaluate because the left
        # side is false, but the whole tree must be validated up front.
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('`false` && abs(`-1`)', self.data,
                            allow('length'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 11)

    def test_short_circuited_or_branch_is_still_checked(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('`true` || abs(`-1`)', self.data,
                            allow('length'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 10)

    def test_function_inside_filter_is_checked(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('a[?abs(b) > `0`]',
                            {'a': [{'b': 1}]}, allow('length'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 3)

    def test_function_inside_expref_is_checked(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('sort_by(a, &abs(b))',
                            {'a': [{'b': 1}]},
                            allow('sort_by', 'length'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 12)

    def test_nested_calls_report_first_position(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('not_null(length(@), length(@))',
                            'hi', deny('length'))
        # First offending call in document order wins.
        self.assertEqual(cm.exception.position, 9)

    def test_second_nested_call_position(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('not_null(length(@), abs(`1`))',
                            'hi', allow('not_null', 'length'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 20)

    def test_function_inside_projection_is_checked(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('a[*].abs(c)',
                            {'a': [{'c': -1}]}, deny('abs'))
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertEqual(cm.exception.position, 5)

    def test_deeply_nested_expref_is_checked(self):
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('map(&map(&abs(@), @), m)',
                            {'m': [[1, -2]]}, deny('map', 'abs'))
        self.assertEqual(cm.exception.function_name, 'map')
        self.assertEqual(cm.exception.position, 0)

    def test_policy_runs_before_any_evaluation(self):
        # Even when the data would trigger a runtime type error, the
        # policy rejection happens first because the tree is checked
        # before evaluation starts.
        with self.assertRaises(
                exceptions.FunctionNotAllowedError):
            jmespath.search('abs(@)', 'a string', deny('abs'))

    def test_decision_uses_registered_name_not_repr(self):
        # The policy names the function the way it is registered in
        # the function table; the underlying _func_length method is
        # irrelevant.
        options = jmespath.Options(
            custom_functions=CustomFunctions(),
            capability_policy=capabilities.CapabilityPolicy(
                denied_functions=['length']))
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            jmespath.search('length(a.b)', self.data, options)
        self.assertEqual(cm.exception.function_name, 'length')

    def test_overriding_a_builtin_cannot_bypass_the_policy(self):
        # A custom subclass replaces the length implementation, but
        # the registered name stays "length" and remains blocked.
        options = jmespath.Options(
            custom_functions=CustomFunctions(),
            capability_policy=capabilities.CapabilityPolicy(
                denied_functions=['length']))
        with self.assertRaises(
                exceptions.FunctionNotAllowedError):
            jmespath.search('length(a.b)', self.data, options)

    def test_custom_function_allowed_by_registered_name(self):
        options = jmespath.Options(
            custom_functions=CustomFunctions(),
            capability_policy=capabilities.CapabilityPolicy(
                allowed_functions=['double']))
        self.assertEqual(
            jmespath.search('double(`21`)', {}, options), 42)

    def test_custom_function_not_in_allow_list_is_blocked(self):
        options = jmespath.Options(
            custom_functions=CustomFunctions(),
            capability_policy=capabilities.CapabilityPolicy(
                allowed_functions=['double']))
        with self.assertRaises(
                exceptions.FunctionNotAllowedError):
            jmespath.search('length(a.b)', self.data, options)

    def test_policy_does_not_mutate_global_function_table(self):
        before = dict(functions.Functions.FUNCTION_TABLE)
        with self.assertRaises(
                exceptions.FunctionNotAllowedError):
            jmespath.search('abs(`-1`)', self.data, deny('abs'))
        self.assertEqual(
            functions.Functions.FUNCTION_TABLE, before)
        # And the built in function still works for an unrestricted
        # search.
        self.assertEqual(
            jmespath.search('abs(`-1`)', self.data), 1)

    def test_policy_is_pure_data_no_code_loading(self):
        policy = capabilities.CapabilityPolicy(
            allowed_functions=['length', 'abs'])
        # Only plain strings are retained; nothing callable is
        # imported or executed while constructing or consulting the
        # policy.
        self.assertTrue(
            all(isinstance(name, str) for name in policy.functions))

    def test_data_manipulation_builtins_still_run_when_allowed(self):
        options = allow('length', 'keys', 'sum')
        self.assertEqual(
            jmespath.search('length(a.b)', self.data, options), 5)
        self.assertEqual(
            jmespath.search('keys(a)', self.data, options), ['b'])
        self.assertEqual(
            jmespath.search('sum(`[1, 2, 3]`)', self.data, options),
            6)

    def test_same_compiled_expression_uses_per_search_policy(self):
        compiled = jmespath.compile('abs(`-1`)')
        self.assertEqual(compiled.search({}, allow('abs')), 1)
        with self.assertRaises(
                exceptions.FunctionNotAllowedError):
            compiled.search({}, deny('abs'))
        # And again with no policy at all.
        self.assertEqual(compiled.search({}), 1)

    def test_hand_built_ast_without_expression_has_no_position(self):
        root = ast.function_expression('abs', [ast.literal(1)])
        with self.assertRaises(
                exceptions.FunctionNotAllowedError) as cm:
            visitor.TreeInterpreter(deny('abs'), parsed=root)
        self.assertEqual(cm.exception.function_name, 'abs')
        self.assertIsNone(cm.exception.position)
        self.assertEqual(
            str(cm.exception), 'Function abs() is not allowed')

    def test_error_is_a_jmespath_error(self):
        self.assertTrue(issubclass(
            exceptions.FunctionNotAllowedError,
            exceptions.JMESPathError))


if __name__ == '__main__':
    unittest.main()
