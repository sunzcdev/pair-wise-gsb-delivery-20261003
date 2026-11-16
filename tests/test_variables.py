from tests import unittest

import jmespath
from jmespath import ast
from jmespath import exceptions
from jmespath import parser
from jmespath.visitor import Options


PEOPLE = {"people": [
    {"name": "a", "age": 20, "tags": ["x"]},
    {"name": "b", "age": 30, "tags": ["x", "y"]},
    {"name": "c", "age": 40, "tags": ["y", "z"]},
]}


class TestExternalVariableParsing(unittest.TestCase):
    def setUp(self):
        self.parser = parser.Parser()

    def test_parses_simple_variable(self):
        self.assert_parsed_ast('$foo', ast.external_variable('foo'))

    def test_parses_braced_variable(self):
        self.assert_parsed_ast('${foo}', ast.external_variable('foo'))

    def assert_parsed_ast(self, expression, expected_ast):
        parsed = self.parser.parse(expression)
        self.assertEqual(parsed.parsed, expected_ast)

    def test_ast_does_not_store_binding_values(self):
        parsed = self.parser.parse('$foo')
        self.assertEqual(
            parsed.parsed,
            {'type': 'external_variable', 'children': [], 'value': 'foo'})

    def test_variable_name_validated_at_parse_time(self):
        with self.assertRaises(exceptions.LexerError):
            self.parser.parse('$1bad')


class TestExternalVariableSearch(unittest.TestCase):
    def test_variable_as_top_level_value(self):
        self.assertEqual(
            jmespath.search('$threshold', {}, bindings={'threshold': 42}),
            42)

    def test_variable_in_filter_projection(self):
        result = jmespath.search(
            'people[?age > $min_age].name', PEOPLE,
            bindings={'min_age': 25})
        self.assertEqual(result, ['b', 'c'])

    def test_braced_variable_in_filter_projection(self):
        result = jmespath.search(
            'people[?name == ${name}].age', PEOPLE,
            bindings={'name': 'b'})
        self.assertEqual(result, [30])

    def test_variable_in_function_arguments(self):
        result = jmespath.search(
            'contains(`["x", "y", "z"]`, $needle)', {},
            bindings={'needle': 'y'})
        self.assertTrue(result)

    def test_variable_in_multi_select_hash(self):
        result = jmespath.search(
            '{a: $x, b: $y}', {}, bindings={'x': 1, 'y': 2})
        self.assertEqual(result, {'a': 1, 'b': 2})

    def test_variable_in_or_expression(self):
        result = jmespath.search(
            'missing || $fallback', {}, bindings={'fallback': 'fb'})
        self.assertEqual(result, 'fb')

    def test_variable_in_not_expression(self):
        result = jmespath.search('!$flag', {}, bindings={'flag': False})
        self.assertTrue(result)

    def test_variable_holding_complex_value(self):
        result = jmespath.search('$items[1]', {},
                                 bindings={'items': [10, 20, 30]})
        self.assertEqual(result, 20)

    def test_variable_does_not_resolve_from_document(self):
        # A variable must never be guessed from the target document,
        # even when a matching top-level key exists.
        with self.assertRaises(exceptions.UndefinedVariableError):
            jmespath.search('$foo', {'foo': 'document-value'}, bindings={})

    def test_missing_binding_raises_undefined_variable_error(self):
        with self.assertRaises(exceptions.UndefinedVariableError) as cm:
            jmespath.search('$foo', {}, bindings={})
        self.assertEqual(cm.exception.variable_name, 'foo')

    def test_null_binding_is_distinct_from_missing_binding(self):
        # An explicit None binding evaluates to None, it is not an
        # undefined variable.
        result = jmespath.search('$foo', {}, bindings={'foo': None})
        self.assertIsNone(result)

    def test_null_binding_inside_filter(self):
        result = jmespath.search(
            'people[?name == $name].name', PEOPLE,
            bindings={'name': None})
        self.assertEqual(result, [])

    def test_bindings_via_options(self):
        options = Options(bindings={'threshold': 30})
        result = jmespath.search(
            'people[?age >= $threshold].name', PEOPLE, options=options)
        self.assertEqual(result, ['b', 'c'])

    def test_bindings_argument_merges_with_options_bindings(self):
        options = Options(bindings={'a': 1, 'b': 2})
        result = jmespath.search(
            '{a: $a, b: $b, c: $c}', {},
            options=options, bindings={'b': 20, 'c': 30})
        self.assertEqual(result, {'a': 1, 'b': 20, 'c': 30})

    def test_string_variable_appearance_is_preserved(self):
        self.assertEqual(
            jmespath.search("'$foo and ${bar}'", {}),
            '$foo and ${bar}')
        self.assertEqual(jmespath.search('`"$foo"`', {}), '$foo')

    def test_quoted_identifier_with_dollar_is_still_a_field(self):
        self.assertEqual(jmespath.search('"$foo"', {'$foo': 'v'}), 'v')


class TestExternalVariableCaching(unittest.TestCase):
    def test_compiled_expression_reused_with_different_bindings(self):
        parser.Parser.purge()
        compiled = jmespath.compile('people[?age >= $cut].name')
        first = compiled.search(PEOPLE, bindings={'cut': 30})
        second = compiled.search(PEOPLE, bindings={'cut': 40})
        self.assertEqual(first, ['b', 'c'])
        self.assertEqual(second, ['c'])

    def test_cache_does_not_retain_last_binding_values(self):
        parser.Parser.purge()
        expression = 'people[?age >= $cut].name'
        jmespath.search(expression, PEOPLE, bindings={'cut': 40})
        # The cached, compiled AST must only hold the variable name.
        cached = parser.Parser._CACHE[expression]
        flat = repr(cached.parsed)
        self.assertIn('external_variable', flat)
        self.assertNotIn('40', flat)
        # Reusing the cache without a binding must raise rather than
        # silently use the previous binding value.
        with self.assertRaises(exceptions.UndefinedVariableError):
            cached.search(PEOPLE)

    def test_no_bindings_parameter_keeps_legacy_behavior(self):
        # An expression without variables behaves exactly as before.
        self.assertEqual(
            jmespath.search('people[0].name', PEOPLE), 'a')


if __name__ == '__main__':
    unittest.main()
