import math

from tests import unittest

import jmespath
from jmespath import exceptions
from jmespath import functions


class CountingFunctions(functions.Functions):
    def __init__(self):
        self.counter = 0

    @functions.signature({'types': []})
    def _func_counted(self, arg):
        self.counter += 1
        return arg


class TestGroupBy(unittest.TestCase):
    def test_scalar_key_groups_ordered_by_first_appearance(self):
        data = [
            {'group': 'b', 'n': 1},
            {'group': 'a', 'n': 2},
            {'group': 'b', 'n': 3},
            {'group': 'a', 'n': 4},
            {'group': 'c', 'n': 5},
        ]
        result = jmespath.search('group_by(@, &group)', data)
        self.assertEqual(result, [
            {'key': 'b', 'members': [
                {'group': 'b', 'n': 1}, {'group': 'b', 'n': 3}]},
            {'key': 'a', 'members': [
                {'group': 'a', 'n': 2}, {'group': 'a', 'n': 4}]},
            {'key': 'c', 'members': [{'group': 'c', 'n': 5}]},
        ])

    def test_members_keep_their_original_order(self):
        data = [{'k': 'x', 'id': 1}, {'k': 'x', 'id': 2},
                {'k': 'x', 'id': 3}]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual([m['id'] for m in result[0]['members']], [1, 2, 3])

    def test_keys_are_not_stringified(self):
        data = [{'k': 1}, {'k': '1'}, {'k': True}, {'k': 1}]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual([g['key'] for g in result], [1, '1', True])
        self.assertIsInstance(result[0]['key'], int)
        self.assertNotIsInstance(result[0]['key'], bool)
        self.assertIsInstance(result[1]['key'], str)
        self.assertIsInstance(result[2]['key'], bool)

    def test_boolean_and_number_keys_are_distinct(self):
        data = [{'k': True}, {'k': 1}, {'k': False},
                {'k': 0}, {'k': True}]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual([(g['key'], len(g['members'])) for g in result],
                         [(True, 2), (1, 1), (False, 1), (0, 1)])

    def test_object_key_order_does_not_affect_equality(self):
        data = [{'k': {'a': 1, 'b': 2}, 'id': 1},
                {'k': {'b': 2, 'a': 1}, 'id': 2}]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]['key'], {'a': 1, 'b': 2})
        self.assertEqual([m['id'] for m in result[0]['members']], [1, 2])

    def test_array_order_is_significant(self):
        data = [{'k': [1, 2], 'id': 1},
                {'k': [2, 1], 'id': 2},
                {'k': [1, 2], 'id': 3}]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual([g['key'] for g in result], [[1, 2], [2, 1]])
        self.assertEqual([[m['id'] for m in g['members']] for g in result],
                         [[1, 3], [2]])

    def test_nested_composite_keys(self):
        data = [
            {'k': {'a': [1, 2]}, 'id': 1},
            {'k': {'a': [1, 2]}, 'id': 2},
            {'k': {'a': [2, 1]}, 'id': 3},
        ]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual(len(result), 2)
        self.assertEqual([m['id'] for m in result[0]['members']], [1, 2])
        self.assertEqual([m['id'] for m in result[1]['members']], [3])

    def test_composite_key_from_multi_select(self):
        data = [{'a': 1, 'b': 'x'}, {'a': 1, 'b': 'y'},
                {'a': 1, 'b': 'x'}]
        result = jmespath.search('group_by(@, &[a, b])', data)
        self.assertEqual([g['key'] for g in result], [[1, 'x'], [1, 'y']])
        result = jmespath.search('group_by(@, &{x: a, y: b})', data)
        self.assertEqual([g['key'] for g in result],
                         [{'x': 1, 'y': 'x'}, {'x': 1, 'y': 'y'}])

    def test_null_is_a_legal_key(self):
        data = [{'k': None, 'id': 1}, {'id': 2}, {'k': None, 'id': 3}]
        result = jmespath.search('group_by(@, &k)', data)
        self.assertEqual(len(result), 1)
        self.assertIsNone(result[0]['key'])
        self.assertEqual([m['id'] for m in result[0]['members']], [1, 2, 3])

    def test_empty_array_returns_empty_array(self):
        self.assertEqual(jmespath.search('group_by(@, &k)', []), [])

    def test_expression_evaluated_exactly_once_per_member(self):
        funcs = CountingFunctions()
        options = jmespath.Options(custom_functions=funcs)
        data = [{'k': 'a'}, {'k': 'b'}, {'k': 'a'}]
        result = jmespath.search(
            'group_by(@, &counted(k))', data, options)
        self.assertEqual(funcs.counter, 3)
        self.assertEqual([g['key'] for g in result], ['a', 'b'])

    def test_nan_key_reports_member_index(self):
        data = [{'k': 1}, {'k': float('nan')}]
        with self.assertRaises(exceptions.JMESPathError) as cm:
            jmespath.search('group_by(@, &k)', data)
        message = str(cm.exception)
        self.assertIn('group_by()', message)
        self.assertIn('index 1', message)

    def test_infinity_key_reports_member_index(self):
        data = [{'k': float('inf')}, {'k': 1}]
        with self.assertRaises(exceptions.JMESPathError) as cm:
            jmespath.search('group_by(@, &k)', data)
        self.assertIn('index 0', str(cm.exception))

    def test_non_json_key_reports_member_index(self):
        import datetime
        data = [{'k': 1}, {'k': datetime.date(2020, 1, 1)}]
        with self.assertRaises(exceptions.JMESPathError) as cm:
            jmespath.search('group_by(@, &k)', data)
        self.assertIn('index 1', str(cm.exception))

    def test_requires_an_array_first_argument(self):
        with self.assertRaises(exceptions.JMESPathTypeError):
            jmespath.search('group_by(`1`, &k)', [])

    def test_requires_an_expref_second_argument(self):
        with self.assertRaises(exceptions.JMESPathTypeError):
            jmespath.search('group_by(@, k)', [{'k': 1}])

    def test_arity_errors(self):
        with self.assertRaises(exceptions.ArityError):
            jmespath.search('group_by(@)', [])
        with self.assertRaises(exceptions.ArityError):
            jmespath.search('group_by(@, &k, &k)', [])
