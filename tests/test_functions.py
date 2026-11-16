#!/usr/bin/env python
from tests import unittest
from datetime import datetime, timedelta
import json

import jmespath
from jmespath import exceptions


class CountingObject(object):
    """Records every field access made by a key expression."""
    def __init__(self, name, value):
        self.name = name
        self.value = value
        self.accesses = 0

    def get(self, field):
        self.accesses += 1
        if field == 'name':
            return self.name
        return self.value


class ExplodingObject(object):
    """Raises a non-JMESPath error when its ``boom`` field is accessed."""
    def __init__(self, value):
        self.value = value

    def get(self, field):
        if field == 'boom':
            raise RuntimeError('key expression exploded')
        if field == 'guarded':
            return self.value
        return self.value


class TestFunctions(unittest.TestCase):

    def test_can_max_datetimes(self):
        # This is python specific behavior, but JMESPath does not specify what
        # you should do with language specific types.  We're going to add the
        # ability that ``to_string`` will always default to str()'ing values it
        # doesn't understand.
        data = [datetime.now(), datetime.now() + timedelta(seconds=1)]
        result = jmespath.search('max([*].to_string(@))', data)
        self.assertEqual(json.loads(result), str(data[-1]))

    def test_type_error_messages(self):
        with self.assertRaises(exceptions.JMESPathTypeError) as e:
            jmespath.search('length(@)', 2)
        exception = e.exception
        # 1. Function name should be in error message
        self.assertIn('length()', str(exception))
        # 2. Mention it's an invalid type
        self.assertIn('invalid type for value: 2', str(exception))
        # 3. Mention the valid types:
        self.assertIn("expected one of: ['string', 'array', 'object']",
                      str(exception))
        # 4. Mention the actual type.
        self.assertIn('received: "number"', str(exception))

    def test_singular_in_error_message(self):
        with self.assertRaises(exceptions.ArityError) as e:
            jmespath.search('length(@, @)', [0, 1])
        exception = e.exception
        self.assertEqual(
            str(exception),
            'Expected 1 argument for function length(), received 2')

    def test_error_message_is_pluralized(self):
        with self.assertRaises(exceptions.ArityError) as e:
            jmespath.search('sort_by(@)', [0, 1])
        exception = e.exception
        self.assertEqual(
            str(exception),
            'Expected 2 arguments for function sort_by(), received 1')

    def test_variadic_is_pluralized(self):
        with self.assertRaises(exceptions.VariadictArityError) as e:
            jmespath.search('not_null()', 'foo')
        exception = e.exception
        self.assertEqual(
            str(exception),
            'Expected at least 1 argument for function not_null(), received 0')


class TestSortFamily(unittest.TestCase):
    FUNCTIONS = ('sort_by', 'min_by', 'max_by')

    def _expression(self, function, field='age'):
        return '%s(@, &%s)' % (function, field)

    def test_sort_by_evaluates_key_expression_once_per_member(self):
        data = [
            CountingObject('a', 3),
            CountingObject('b', 1),
            CountingObject('c', 2),
        ]
        jmespath.search('sort_by(@, &value)', data)
        self.assertEqual([member.accesses for member in data], [1, 1, 1])

    def test_min_by_and_max_by_evaluate_key_expression_once_per_member(self):
        for function in ('min_by', 'max_by'):
            data = [
                CountingObject('a', 3),
                CountingObject('b', 1),
                CountingObject('c', 2),
            ]
            jmespath.search('%s(@, &value)' % function, data)
            self.assertEqual([member.accesses for member in data],
                             [1, 1, 1], function)

    def test_sort_by_does_not_evaluate_expression_on_empty_array(self):
        # An empty list is returned without ever touching the expref.
        self.assertEqual(jmespath.search('sort_by(@, &boom)', []), [])

    def test_min_by_and_max_by_skip_expression_on_empty_array(self):
        for function in ('min_by', 'max_by'):
            self.assertIsNone(
                jmespath.search('%s(@, &boom)' % function, []))

    def test_sort_by_keeps_input_order_for_equal_keys(self):
        data = [
            {'key': 1, 'label': 'a'},
            {'key': 1, 'label': 'b'},
            {'key': 0, 'label': 'c'},
            {'key': 1, 'label': 'd'},
            {'key': 0, 'label': 'e'},
        ]
        result = jmespath.search('sort_by(@, &key)', data)
        self.assertEqual([item['label'] for item in result],
                         ['c', 'e', 'a', 'b', 'd'])

    def test_sort_by_accepts_a_homogeneous_string_domain(self):
        data = [{'s': 'c'}, {'s': 'a'}, {'s': 'b'}]
        result = jmespath.search('sort_by(@, &s)', data)
        self.assertEqual([item['s'] for item in result], ['a', 'b', 'c'])

    def test_sort_by_accepts_a_homogeneous_number_domain(self):
        data = [{'n': 3}, {'n': 1}, {'n': 2}]
        result = jmespath.search('sort_by(@, &n)', data)
        self.assertEqual([item['n'] for item in result], [1, 2, 3])

    def test_mixed_number_and_string_keys_are_rejected(self):
        data = [{'age': 1}, {'age': 'two'}, {'age': 3}]
        for function in self.FUNCTIONS:
            with self.assertRaises(exceptions.JMESPathTypeError) as cm:
                jmespath.search(self._expression(function), data)
            self.assertEqual(cm.exception.function_name, function)
            self.assertEqual(cm.exception.actual_type, 'string')

    def test_type_mismatch_anywhere_in_array_is_rejected(self):
        # The offending member is not the first one; every member must be
        # validated before the ordering can take place.
        data = [
            {'age': 1},
            {'age': 2},
            {'age': 'three'},
            {'age': 4},
        ]
        for function in self.FUNCTIONS:
            with self.assertRaises(exceptions.JMESPathTypeError) as cm:
                jmespath.search(self._expression(function), data)
            self.assertEqual(cm.exception.current_value, 'three')

    def test_unsupported_key_types_are_rejected(self):
        data = [{'flag': True}, {'flag': False}]
        for function in self.FUNCTIONS:
            with self.assertRaises(exceptions.JMESPathTypeError) as cm:
                jmespath.search('%s(@, &flag)' % function, data)
            self.assertEqual(cm.exception.actual_type, 'boolean')

    def test_failure_does_not_mutate_the_input_array(self):
        data = [{'age': 1}, {'age': 'x'}, {'age': 3}, {'age': 2}]
        original = [dict(member) for member in data]
        original_order = [member['age'] for member in data]
        for function in self.FUNCTIONS:
            with self.assertRaises(exceptions.JMESPathError):
                jmespath.search(self._expression(function), data)
            self.assertEqual(data, original)
            self.assertEqual(
                [member['age'] for member in data], original_order)

    def test_successful_sort_does_not_mutate_the_input_array(self):
        data = [{'n': 3}, {'n': 1}, {'n': 2}]
        original = [dict(member) for member in data]
        jmespath.search('sort_by(@, &n)', data)
        self.assertEqual(data, original)

    def test_key_expression_failure_reports_original_member_index(self):
        data = [
            ExplodingObject(3),
            ExplodingObject(1),
            ExplodingObject(2),
        ]
        for function in self.FUNCTIONS:
            with self.assertRaises(exceptions.JMESPathSortError) as cm:
                jmespath.search('%s(@, &boom)' % function, data)
            error = cm.exception
            self.assertIsInstance(error, exceptions.JMESPathError)
            self.assertEqual(error.function_name, function)
            self.assertEqual(error.index, 0)
            self.assertIn('index 0', str(error))

    def test_key_expression_failure_index_is_the_original_position(self):
        data = [
            ExplodingObject(3),
            ExplodingObject(1),
            ExplodingObject(2),
        ]
        # The third member (original index 2) is the only one that blows up.
        def get(field):
            raise RuntimeError('boom')
        data[2].get = get
        for function in self.FUNCTIONS:
            with self.assertRaises(exceptions.JMESPathSortError) as cm:
                jmespath.search('%s(@, &guarded)' % function, data)
            self.assertEqual(cm.exception.index, 2)
            self.assertIn('index 2', str(cm.exception))

    def test_min_by_and_max_by_return_first_member_on_tied_keys(self):
        data = [
            {'age': 5, 'name': 'first'},
            {'age': 5, 'name': 'second'},
            {'age': 2, 'name': 'low'},
        ]
        self.assertEqual(
            jmespath.search('max_by(@, &age)', data)['name'], 'first')
        self.assertEqual(
            jmespath.search('min_by(@, &age)', data)['name'], 'low')
        tied_min = [
            {'age': 2, 'name': 'low1'},
            {'age': 9, 'name': 'high'},
            {'age': 2, 'name': 'low2'},
        ]
        self.assertEqual(
            jmespath.search('min_by(@, &age)', tied_min)['name'], 'low1')

    def test_min_by_and_max_by_basic_results(self):
        data = [{'age': 20}, {'age': 50}, {'age': 10}, {'age': 40}]
        self.assertEqual(
            jmespath.search('min_by(@, &age)', data), {'age': 10})
        self.assertEqual(
            jmespath.search('max_by(@, &age)', data), {'age': 50})
