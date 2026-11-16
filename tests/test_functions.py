#!/usr/bin/env python
from tests import unittest
from datetime import datetime, timedelta
import json

import jmespath
from jmespath import exceptions
from jmespath import functions


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


class TestJoinBy(unittest.TestCase):

    def setUp(self):
        self.data = {
            'left': [
                {'id': 1, 'key': 'a'},
                {'id': 2, 'key': 'b'},
                {'id': 3, 'key': 'z'},
                {'id': 4},
            ],
            'right': [
                {'rid': 'r1', 'key': 'a'},
                {'rid': 'r2', 'key': 'a'},
                {'rid': 'r3', 'key': 'b'},
                {'rid': 'r4', 'key': None},
                {'rid': 'r5'},
            ],
        }

    def _join(self, expression=None):
        if expression is None:
            expression = 'join_by(left, &key, right, &key)'
        return jmespath.search(expression, self.data)

    def test_inner_join_is_default(self):
        result = self._join()
        self.assertEqual(
            [(r['left']['id'], r['right']['rid']) for r in result],
            [(1, 'r1'), (1, 'r2'), (2, 'r3')])

    def test_result_has_left_and_right_keys(self):
        for record in self._join():
            self.assertEqual(sorted(record), ['left', 'right'])

    def test_multiples_expand_in_left_then_right_order(self):
        data = {
            'left': [{'k': 'same', 'n': 1}, {'k': 'same', 'n': 2}],
            'right': [{'k': 'same', 'n': 'x'}, {'k': 'same', 'n': 'y'}],
        }
        result = jmespath.search('join_by(left, &k, right, &k)', data)
        self.assertEqual(
            [(r['left']['n'], r['right']['n']) for r in result],
            [(1, 'x'), (1, 'y'), (2, 'x'), (2, 'y')])

    def test_left_join_keeps_unmatched_left_records_in_order(self):
        result = self._join("join_by(left, &key, right, &key, 'left')")
        self.assertEqual(
            [(r['left']['id'], r['right'] and r['right']['rid'])
             for r in result],
            [(1, 'r1'), (1, 'r2'), (2, 'r3'), (3, None), (4, None)])

    def test_missing_and_null_keys_do_not_match(self):
        data = {
            'left': [{'k': None}, {}],
            'right': [{'k': None}, {}],
        }
        self.assertEqual(
            jmespath.search('join_by(left, &k, right, &k, \'left\')', data),
            [{'left': {'k': None}, 'right': None},
             {'left': {}, 'right': None}])

    def test_null_keys_never_match_each_other(self):
        data = {'left': [{'k': None}], 'right': [{'k': None}]}
        self.assertEqual(
            jmespath.search('join_by(left, &k, right, &k)', data), [])

    def test_key_expression_can_return_compound_values(self):
        data = {
            'left': [{'k': {'a': 1, 'b': [1, 2]}}],
            'right': [{'k': {'b': [1, 2], 'a': 1.0}}],
        }
        self.assertEqual(
            len(jmespath.search('join_by(left, &k, right, &k)', data)), 1)

    def test_array_key_order_is_significant(self):
        data = {
            'left': [{'k': [1, 2]}],
            'right': [{'k': [2, 1]}],
        }
        self.assertEqual(
            jmespath.search('join_by(left, &k, right, &k)', data), [])

    def test_booleans_do_not_match_numbers(self):
        data = {
            'left': [{'k': True}, {'k': False}, {'k': 1}, {'k': 0}],
            'right': [{'k': 1}, {'k': 0}, {'k': True}, {'k': False}],
        }
        result = jmespath.search('join_by(left, &k, right, &k)', data)
        pairs = [(r['left']['k'], r['right']['k']) for r in result]
        self.assertEqual(pairs, [(True, True), (False, False),
                                 (1, 1), (0, 0)])

    def test_string_numbers_do_not_match_numbers(self):
        data = {'left': [{'k': '1'}], 'right': [{'k': 1}]}
        self.assertEqual(
            jmespath.search('join_by(left, &k, right, &k)', data), [])

    def test_non_json_key_is_rejected(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({'types': ['string']})
            def _func_datetime_key(self, value):
                # A datetime is not a JSON value, so it has no JSON
                # equality and cannot be used as a join key.
                return datetime.now()

        data = {'left': [{'k': '1'}], 'right': [{'k': '1'}]}
        options = jmespath.Options(custom_functions=CustomFunctions())
        with self.assertRaises(exceptions.JMESPathValueError) as e:
            jmespath.search('join_by(left, &datetime_key(k), right, '
                            '&datetime_key(k))', data, options=options)
        self.assertIn('join_by()', str(e.exception))

    def test_non_finite_number_key_is_rejected(self):
        data = {'left': [{'k': float('nan')}], 'right': [{'k': 1}]}
        with self.assertRaises(exceptions.JMESPathValueError):
            jmespath.search('join_by(left, &k, right, &k)', data)
        data = {'left': [{'k': 1}], 'right': [{'k': float('inf')}]}
        with self.assertRaises(exceptions.JMESPathValueError):
            jmespath.search('join_by(left, &k, right, &k)', data)

    def test_key_expressions_run_once_per_record(self):
        # The index is built from the right array only and each key
        # expression is evaluated once per record, never once per pair.
        class CountingFunctions(functions.Functions):
            def __init__(self):
                self.evaluated = []

            def _join_key(self, expref, element):
                self.evaluated.append(element)
                return super(CountingFunctions, self)._join_key(
                    expref, element)

        data = {
            'left': [{'k': 1}, {'k': 1}],
            'right': [{'k': 1}, {'k': 1}, {'k': 1}],
        }
        instance = CountingFunctions()
        options = jmespath.Options(custom_functions=instance)
        result = jmespath.search('join_by(left, &k, right, &k)', data,
                                 options=options)
        self.assertEqual(len(result), 6)
        self.assertEqual(len(instance.evaluated), 5)

    def test_inner_mode_can_be_requested_explicitly(self):
        explicit = self._join("join_by(left, &key, right, &key, 'inner')")
        self.assertEqual(explicit, self._join())

    def test_empty_arrays(self):
        self.assertEqual(
            jmespath.search('join_by(`[]`, &k, `[]`, &k, \'left\')', {}), [])
        self.assertEqual(
            jmespath.search('join_by(`[{"k": 1}]`, &k, `[]`, &k, \'left\')',
                            {}),
            [{'left': {'k': 1}, 'right': None}])
        self.assertEqual(
            jmespath.search('join_by(`[]`, &k, `[{"k": 1}]`, &k)', {}), [])

    def test_invalid_mode_is_rejected(self):
        with self.assertRaises(exceptions.JMESPathValueError) as e:
            self._join("join_by(left, &key, right, &key, 'outer')")
        self.assertIn('join_by()', str(e.exception))

    def test_mode_must_be_a_string(self):
        with self.assertRaises(exceptions.JMESPathTypeError):
            self._join('join_by(left, &key, right, &key, `1`)')

    def test_arity_requires_four_arguments(self):
        with self.assertRaises(exceptions.OptionalArityError) as e:
            self._join('join_by(left, &key, right)')
        self.assertEqual(
            str(e.exception),
            'Expected 4 or 5 arguments for function join_by(), received 3')

    def test_arity_rejects_extra_arguments(self):
        with self.assertRaises(exceptions.ArityError) as e:
            self._join("join_by(left, &key, right, &key, 'left', 'left')")
        self.assertEqual(
            str(e.exception),
            'Expected 5 arguments for function join_by(), received 6')

    def test_optional_signature_entries_stay_compatible(self):
        # Functions without optional arguments keep reporting the exact
        # required arity they had before.
        with self.assertRaises(exceptions.ArityError) as e:
            jmespath.search('length()', [])
        self.assertEqual(
            str(e.exception),
            'Expected 1 argument for function length(), received 0')

    def test_ordinary_functions_are_unaffected(self):
        self.assertEqual(jmespath.search('length(@)', [1, 2, 3]), 3)
        self.assertEqual(jmespath.search('sort(@)', [3, 1, 2]), [1, 2, 3])
        self.assertEqual(
            jmespath.search('sort_by(@, &k)[*].k',
                            [{'k': 2}, {'k': 1}]), [1, 2])
