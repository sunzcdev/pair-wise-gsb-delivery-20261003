import unittest

import jmespath
from jmespath import functions
from jmespath import exceptions
from jmespath.stream import (
    stream_search,
    StreamingTreeInterpreter,
    StreamingCapabilityValidator,
)


class _OneShotIterable(object):
    """Iterable that records how many times it is iterated and consumed."""

    def __init__(self, items):
        self.items = items
        self.iteration_passes = 0
        self.consumed = 0

    def __iter__(self):
        self.iteration_passes += 1
        for item in self.items:
            self.consumed += 1
            yield item


class TestStreamSearchEntry(unittest.TestCase):
    def test_module_level_entry_returns_an_iterator(self):
        result = stream_search("[*].a", [{"a": 1}, {"a": 2}])
        self.assertEqual(iter(result), iter(result))
        self.assertEqual(list(result), [1, 2])

    def test_top_level_package_entry(self):
        result = jmespath.stream_search("[*].a", [{"a": 1}])
        self.assertEqual(list(result), [1])

    def test_compiled_result_entry(self):
        compiled = jmespath.compile("[*].a")
        self.assertEqual(
            list(compiled.stream_search([{"a": 1}, {"a": 2}])), [1, 2])

    def test_accepts_a_generator_source(self):
        def gen():
            for value in range(3):
                yield {"a": value}

        self.assertEqual(list(stream_search("[*].a", gen())), [0, 1, 2])

    def test_matches_eager_search_for_wildcard_projection(self):
        data = [{"a": 1}, {"a": 2}, {"b": 3}]
        self.assertEqual(
            list(stream_search("[*].a", data)),
            jmespath.search("[*].a", data))

    def test_matches_eager_search_for_flatten_projection(self):
        data = [{"n": [1, 2]}, {"n": [3]}, {}]
        self.assertEqual(
            list(stream_search("[*].n[]", data)),
            jmespath.search("[*].n[]", data))
        self.assertEqual(
            list(stream_search("[].n", data)),
            jmespath.search("[].n", data))

    def test_matches_eager_search_for_filter_projection(self):
        data = [{"a": 1, "b": "x"}, {"a": 2}, {"a": 3, "b": "y"}]
        for expr in ["[?a > `1`].a", "[?b].b", "[?a >= `2`][0]"]:
            self.assertEqual(
                list(stream_search(expr, data)),
                jmespath.search(expr, data), expr)

    def test_non_list_source_yields_nothing(self):
        for source in (None, 42, "abc", {"k": 1}, True):
            self.assertEqual(list(stream_search("[*].a", source)), [])


class TestProjectionNullRules(unittest.TestCase):
    def setUp(self):
        self.data = [
            {"a": 1, "b": None},
            {"a": 2, "b": "x"},
            {"a": 3},
        ]

    def test_projection_drops_null_results(self):
        # Only the outer projection removes nulls.
        self.assertEqual(list(stream_search("[*].b", self.data)), ["x"])
        self.assertEqual(
            list(stream_search("[*].b", self.data)),
            jmespath.search("[*].b", self.data))

    def test_multi_select_hash_keeps_inner_nulls(self):
        result = list(stream_search("[*].{x: a, y: b}", self.data))
        self.assertEqual(result, [
            {"x": 1, "y": None},
            {"x": 2, "y": "x"},
            {"x": 3, "y": None},
        ])
        self.assertEqual(
            result, jmespath.search("[*].{x: a, y: b}", self.data))

    def test_multi_select_list_keeps_inner_nulls(self):
        result = list(stream_search("[*].[a, b]", self.data))
        self.assertEqual(result, [
            [1, None], [2, "x"], [3, None],
        ])
        self.assertEqual(
            result, jmespath.search("[*].[a, b]", self.data))

    def test_map_keeps_null_results(self):
        data = [{"n": [1, None]}, {"n": [None, 2]}, {"n": []}]
        # map() must preserve null elements; the outer projection only
        # drops a result when the whole map result is itself null.
        result = list(stream_search(
            "[*].map(&@, n)", data))
        self.assertEqual(result, [[1, None], [None, 2], []])
        self.assertEqual(
            result, jmespath.search("[*].map(&@, n)", data))


class TestLazinessAndSinglePass(unittest.TestCase):
    def test_source_is_consumed_lazily(self):
        source = _OneShotIterable(
            [{"a": 1}, {"a": 2}, {"a": 3}, {"a": 4}])
        iterator = stream_search("[*].a", source)
        self.assertEqual(next(iterator), 1)
        # Only the first element should have been pulled so far.
        self.assertEqual(source.consumed, 1)
        self.assertEqual(list(iterator), [2, 3, 4])
        self.assertEqual(source.consumed, 4)

    def test_source_is_traversed_only_once(self):
        source = _OneShotIterable([{"a": 1}, {"a": 2}, {"a": 3}])
        result = list(stream_search("[*].{x: a, y: a}", source))
        self.assertEqual(len(result), 3)
        self.assertEqual(source.iteration_passes, 1)
        self.assertEqual(source.consumed, 3)

    def test_partial_consumption_does_not_read_rest(self):
        source = _OneShotIterable(
            [{"a": 1}, {"a": 2}, {"a": 3}, {"a": 4}])
        iterator = stream_search("[*].a", source)
        self.assertEqual(next(iterator), 1)
        self.assertEqual(next(iterator), 2)
        # Never exhaust the iterator; the remaining items stay unread.
        self.assertEqual(source.consumed, 2)
        self.assertEqual(source.iteration_passes, 1)

    def test_nested_projection_still_single_pass(self):
        source = _OneShotIterable(
            [{"n": [1, 2]}, {"n": [3]}, {"n": []}])
        self.assertEqual(list(stream_search("[*].n[]", source)),
                         [1, 2, 3])
        self.assertEqual(source.iteration_passes, 1)


class TestStreamingRejection(unittest.TestCase):
    def _assert_rejected(self, expression):
        # Rejection must happen eagerly, at call time, before iteration.
        source = _OneShotIterable([{"a": 1, "n": [1]}])
        with self.assertRaises(exceptions.StreamingExpressionError):
            stream_search(expression, source)
        # The source must not have been touched at all.
        self.assertEqual(source.consumed, 0)
        self.assertEqual(source.iteration_passes, 0)

    def test_sort_functions_rejected_before_reading_source(self):
        self._assert_rejected("sort(@)")
        self._assert_rejected("sort_by(@, &a)")
        self._assert_rejected("[*].sort(n)")

    def test_aggregating_functions_rejected(self):
        for expr in [
                "[*].sum(n)", "[*].avg(n)", "[*].min(n)", "[*].max(n)",
                "[*].min_by(n, &@)", "[*].max_by(n, &@)",
                "[*].length(n)", "[*].join(',', n)"]:
            self._assert_rejected(expr)

    def test_reverse_operation_rejected(self):
        self._assert_rejected("reverse(@)")
        self._assert_rejected("[*].reverse(n)")

    def test_reverse_slice_rejected(self):
        self._assert_rejected("[::-1].a")
        self._assert_rejected("[5::-1].a")
        self._assert_rejected("[*].n[::-1]")

    def test_length_dependent_slice_rejected(self):
        self._assert_rejected("[:-1].a")
        self._assert_rejected("[-1:].a")
        self._assert_rejected("[*].n[:-1]")

    def test_negative_index_rejected(self):
        self._assert_rejected("[-1].a")
        self._assert_rejected("[*].n[-1]")

    def test_non_projection_expressions_rejected(self):
        for expr in [
                "a", "a.b", "@", "length(@)", "sum([*].a)",
                "[*] | [0]", "a | b", "*.a", "{x: a}"]:
            self._assert_rejected(expr)

    def test_rejection_error_type(self):
        with self.assertRaises(exceptions.JMESPathError):
            stream_search("sort(@)", [])

    def test_forward_slice_and_non_negative_index_accepted(self):
        data = [{"a": 1}, {"a": 2}, {"a": 3}]
        self.assertEqual(
            list(stream_search("[0:2].a", data)),
            jmespath.search("[0:2].a", data))
        self.assertEqual(
            list(stream_search("[::2].a", data)),
            jmespath.search("[::2].a", data))


class TestFunctionCapabilityDeclaration(unittest.TestCase):
    def test_builtin_streamable_functions_accepted(self):
        data = [{"a": 1, "b": None, "s": "xy", "n": [1]}]
        for expr in [
                "[*].abs(a)", "[*].not_null(b, a)", "[*].type(a)",
                "[*].to_string(a)", "[*].contains(s, 'x')",
                "[*].starts_with(s, 'x')", "[*].map(&@, n)"]:
            self.assertEqual(
                list(stream_search(expr, data)),
                jmespath.search(expr, data), expr)

    def test_builtin_non_streamable_functions_rejected(self):
        builtins = functions.Functions()
        for name in ["sort", "sort_by", "reverse", "sum", "avg", "min",
                     "max", "length", "join", "min_by", "max_by"]:
            self.assertFalse(
                builtins.is_streamable(name), name)

    def test_unknown_function_rejected(self):
        with self.assertRaises(exceptions.StreamingExpressionError):
            stream_search("[*].does_not_exist(a)", [{"a": 1}])

    def test_custom_function_requires_streamable_marker(self):
        class CustomFunctions(functions.Functions):
            @functions.signature({"types": ["number"]})
            def _func_double(self, value):
                return value * 2

            @functions.signature({"types": ["number"]})
            @functions.streamable
            def _func_triple(self, value):
                return value * 3

        options = jmespath.Options(
            custom_functions=CustomFunctions())

        # Unmarked custom function is rejected up front.
        with self.assertRaises(exceptions.StreamingExpressionError):
            stream_search("[*].double(a)", [{"a": 1}], options=options)

        # Explicitly marked custom function is streamable.
        self.assertEqual(
            list(stream_search(
                "[*].triple(a)", [{"a": 1}, {"a": 2}], options=options)),
            [3, 6])

    def test_streamable_marker_sets_attribute(self):
        @functions.streamable
        def some_func():
            pass
        self.assertTrue(some_func.streamable)


class TestSlicingAndIndexing(unittest.TestCase):
    def test_forward_slice_matches_eager_search(self):
        data = [{"a": 1}, {"a": 2}, {"a": 3}, {"a": 4}]
        for expr in ["[0:2].a", "[1:3].a", "[::2].a", "[1:4:2].a",
                     "[2:].a", "[:3].a", "[*].a"]:
            self.assertEqual(
                list(stream_search(expr, data)),
                jmespath.search(expr, data), expr)

    def test_nested_non_negative_index_matches_eager_search(self):
        data = [{"n": [10, 20]}, {"n": [30]}, {"n": []}, {}]
        for expr in ["[*].n[0]", "[*].n[1]"]:
            self.assertEqual(
                list(stream_search(expr, data)),
                jmespath.search(expr, data), expr)


class TestValidatorOnlyTouchesAst(unittest.TestCase):
    def test_validate_raises_without_needing_source(self):
        parsed = jmespath.compile("sort(@)").parsed
        validator = StreamingCapabilityValidator(functions.Functions())
        with self.assertRaises(exceptions.StreamingExpressionError):
            validator.validate(parsed)

    def test_validate_accepts_supported_projection(self):
        parsed = jmespath.compile("[*].{x: a, y: b}").parsed
        validator = StreamingCapabilityValidator(functions.Functions())
        self.assertIsNone(validator.validate(parsed))

    def test_interpreter_is_a_tree_interpreter(self):
        # The streaming interpreter reuses the standard per-element
        # evaluation semantics.
        self.assertTrue(
            issubclass(StreamingTreeInterpreter,
                       jmespath.visitor.TreeInterpreter))


if __name__ == "__main__":
    unittest.main()
