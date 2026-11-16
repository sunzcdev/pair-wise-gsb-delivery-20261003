import json
import unittest

import jmespath
from jmespath import exceptions
from jmespath import functions


class CustomFunctions(functions.Functions):
    @functions.signature({'types': ['string']})
    def _func_unique_letters(self, value):
        return ''.join(sorted(set(value)))

    @functions.signature({'types': ['number']}, {'types': ['number']})
    def _func_my_add(self, x, y):
        return x + y

    @functions.signature({'types': ['object'], 'variadic': True})
    def _func_merge_objects(self, *objects):
        merged = {}
        for obj in objects:
            merged.update(obj)
        return merged


class RecordingFunctions(functions.Functions):
    """Records every call so tests can prove exprefs are not evaluated."""

    def __init__(self):
        super(RecordingFunctions, self).__init__()
        self.calls = []

    @functions.signature({'types': ['array']}, {'types': ['expref']})
    def _func_map_values(self, values, expref):
        self.calls.append('map_values')
        return [expref.visit(expref.expression, value) for value in values]

    @functions.signature({'types': []})
    def _func_boom(self, value):
        self.calls.append('boom')
        raise AssertionError('_func_boom should never run while preparing')


class SubsetFunctions(functions.Functions):
    @functions.signature({'types': ['string']})
    def _func_length(self, value):
        return len(value)


class ExtraFunctions(functions.Functions):
    @functions.signature({'types': ['string']})
    def _func_shout(self, value):
        return value.upper()


class UnregisteredOverrideFunctions(functions.Functions):
    # Overriding an inherited _func_* without @signature drops registration
    # for that name in the subclass table.
    def _func_length(self, value):
        return len(value)


class ReorderedSignatureFunctions(functions.Functions):
    # Same accepted types, different declaration order: capability matches.
    @functions.signature({'types': ['object', 'array', 'string']})
    def _func_length(self, value):
        return len(value)


class TestPrepareFindsProblems(unittest.TestCase):
    def test_unknown_function_is_reported(self):
        with self.assertRaises(exceptions.UnknownFunctionError) as e:
            jmespath.prepare('does_not_exist(@)')
        self.assertIn('does_not_exist()', str(e.exception))

    def test_unknown_function_inside_projection_is_reported(self):
        with self.assertRaises(exceptions.UnknownFunctionError):
            jmespath.prepare('foo[*].does_not_exist(@)')

    def test_unknown_function_inside_expref_body_is_reported(self):
        with self.assertRaises(exceptions.UnknownFunctionError):
            jmespath.prepare('sort_by(items, &does_not_exist(@))')

    def test_too_few_arguments(self):
        with self.assertRaises(exceptions.ArityError) as e:
            jmespath.prepare('length()')
        self.assertEqual(e.exception.expected_arity, 1)
        self.assertEqual(e.exception.actual_arity, 0)

    def test_too_many_arguments(self):
        with self.assertRaises(exceptions.ArityError):
            jmespath.prepare('length(@, @)')

    def test_variadic_requires_at_least_declared_arity(self):
        with self.assertRaises(exceptions.VariadictArityError):
            jmespath.prepare('not_null()')

    def test_variadic_accepts_extra_arguments(self):
        prepared = jmespath.prepare('not_null(a, b, c)')
        self.assertEqual(prepared.required_functions, ('not_null',))

    def test_custom_variadic_function_accepts_many_arguments(self):
        prepared = jmespath.prepare(
            'merge_objects(a, b, c)', functions=CustomFunctions())
        self.assertEqual(prepared.required_functions, ('merge_objects',))
        self.assertTrue(
            prepared.function_requirements['merge_objects']['variadic'])
        self.assertIsNone(
            prepared.function_requirements['merge_objects']['maximum_arity'])

    def test_static_literal_type_error(self):
        with self.assertRaises(exceptions.JMESPathTypeError) as e:
            jmespath.prepare("abs('not a number')")
        self.assertEqual(e.exception.function_name, 'abs')

    def test_static_literal_subtype_error(self):
        with self.assertRaises(exceptions.JMESPathTypeError):
            jmespath.prepare('sort(`[1, "two"]`)')

    def test_static_literal_matching_type_is_accepted(self):
        prepared = jmespath.prepare('sort(`[3, 1, 2]`)')
        self.assertEqual(prepared.requirements['type_requirements'], [])

    def test_expression_reference_for_non_expref_argument(self):
        with self.assertRaises(exceptions.JMESPathTypeError):
            jmespath.prepare('length(&foo)')

    def test_document_value_for_expref_only_argument(self):
        # Only an ``&expr`` can ever produce an expref.
        with self.assertRaises(exceptions.JMESPathTypeError):
            jmespath.prepare('sort_by(items, foo)')

    def test_nested_call_for_expref_argument_is_left_to_run_time(self):
        prepared = jmespath.prepare('map(to_array(foo), items)')
        indexes = sorted(
            r['argument_index'] for r in prepared.type_requirements)
        self.assertIn(0, indexes)

    def test_arity_errors_match_runtime_errors(self):
        with self.assertRaises(exceptions.ArityError) as static:
            jmespath.prepare('length()')
        with self.assertRaises(exceptions.ArityError) as runtime:
            jmespath.search('length()', {})
        self.assertEqual(str(static.exception), str(runtime.exception))


class TestPreparedRequirements(unittest.TestCase):
    def test_function_requirements_are_reported(self):
        prepared = jmespath.prepare('sort_by(items, &length(name))')
        self.assertEqual(prepared.function_names, ('length', 'sort_by'))
        requirements = prepared.requirements['functions']
        by_name = dict((item['name'], item) for item in requirements)
        self.assertEqual(by_name['sort_by']['minimum_arity'], 2)
        self.assertEqual(by_name['sort_by']['maximum_arity'], 2)
        self.assertEqual(by_name['sort_by']['argument_counts'], [2])
        self.assertEqual(by_name['length']['calls'], 1)

    def test_repeated_calls_are_counted(self):
        prepared = jmespath.prepare('length(a) == length(b)')
        self.assertEqual(
            prepared.function_requirements['length']['calls'], 2)
        self.assertEqual(
            prepared.requirements['functions'][0]['argument_counts'], [1])

    def test_variadic_call_sites_record_each_argument_count(self):
        prepared = jmespath.prepare('not_null(a) == not_null(a, b, c)')
        summary = prepared.requirements['functions'][0]
        self.assertEqual(summary['calls'], 2)
        self.assertEqual(summary['argument_counts'], [1, 3])
        self.assertEqual(summary['minimum_arity'], 1)
        self.assertIsNone(summary['maximum_arity'])

    def test_inherited_registration_is_respected(self):
        class Base(functions.Functions):
            @functions.signature({'types': [], 'variadic': True})
            def _func_pick(self, *args):
                return args[0] if args else None

        class Child(Base):
            @functions.signature({'types': ['string']})
            def _func_extra(self, value):
                return value

        self.assertIn('extra', Child.FUNCTION_TABLE)
        self.assertIn('pick', Child.FUNCTION_TABLE)
        prepared = Child().prepare('extra(pick(@))')
        self.assertEqual(prepared.required_functions, ('extra', 'pick'))
        self.assertEqual(prepared.search('hi'), 'hi')

    def test_document_dependent_types_are_runtime_requirements(self):
        prepared = jmespath.prepare('length(@)')
        self.assertEqual(len(prepared.type_requirements), 1)
        requirement = prepared.type_requirements[0]
        self.assertEqual(requirement['function'], 'length')
        self.assertEqual(requirement['argument_index'], 0)
        self.assertEqual(requirement['confirmed_at'], 'runtime')
        self.assertEqual(
            sorted(requirement['expected_types']),
            ['array', 'object', 'string'])

    def test_expression_reference_is_statically_confirmed(self):
        # An ``&expr`` argument is statically known to be an expref, so it
        # is checked at preparation time rather than reported as a runtime
        # requirement.  Its body must not be evaluated.
        prepared = jmespath.prepare('sort_by(items, &foo.bar)')
        indexes = [r['argument_index'] for r in prepared.type_requirements]
        self.assertEqual(indexes, [0])
        self.assertEqual(prepared.type_requirements[0]['function'],
                         'sort_by')
        self.assertEqual(prepared.type_requirements[0]['expected_types'],
                         ['array'])
        self.assertEqual(prepared.expression_references, ({
            'function': 'sort_by',
            'argument_index': 1,
            'type': 'expref',
            'confirmed_at': 'prepare',
        },))

    def test_expression_reference_is_not_executed_by_prepare(self):
        recording = RecordingFunctions()
        prepared = jmespath.prepare(
            'map_values(items, &boom(@))', functions=recording)
        # boom() is inside the expref; it is checked but never run.
        self.assertEqual(recording.calls, [])
        self.assertEqual(prepared.required_functions, ('boom', 'map_values'))
        self.assertEqual(len(prepared.expression_references), 1)

    def test_preparation_never_calls_functions(self):
        recording = RecordingFunctions()
        prepared = jmespath.prepare(
            'map_values(items, &boom(@)) | boom(`1`)',
            functions=recording)
        # The expref body references boom(), so it is a function
        # requirement, but neither call was executed.
        self.assertEqual(recording.calls, [])
        self.assertEqual(prepared.function_names, ('boom', 'map_values'))

    def test_any_type_signature_needs_no_runtime_requirement(self):
        prepared = jmespath.prepare('to_string(@)')
        self.assertEqual(prepared.type_requirements, ())

    def test_requirements_summary_is_json_serializable(self):
        prepared = jmespath.prepare('sort_by(items, &length(name))')
        encoded = json.dumps(prepared.requirements, sort_keys=True)
        self.assertIn('sort_by', encoded)

    def test_requirements_summary_is_detached(self):
        prepared = jmespath.prepare('sort_by(items, &length(name))')
        summary = prepared.requirements
        summary['functions'][0]['name'] = 'tampered'
        summary['functions'][0]['signature'][0]['types'].append('null')
        summary['type_requirements'][0]['expected_types'].append('null')
        self.assertEqual(prepared.requirements, prepared.requirements)
        self.assertEqual(
            prepared.requirements['functions'][0]['name'], 'length')

    def test_no_functions_means_no_requirements(self):
        prepared = jmespath.prepare('foo.bar[*].baz')
        self.assertEqual(prepared.required_functions, ())
        self.assertEqual(prepared.type_requirements, ())


class TestPreparedCapabilityBinding(unittest.TestCase):
    def test_prepared_expression_searches(self):
        prepared = jmespath.prepare('length(foo)')
        self.assertEqual(prepared.search({'foo': 'abcd'}), 4)

    def test_runtime_type_error_still_raised_by_prepared_search(self):
        prepared = jmespath.prepare('length(@)')
        with self.assertRaises(exceptions.JMESPathTypeError):
            prepared.search(2)

    def test_matching_instance_is_prepared(self):
        prepared = jmespath.prepare('unique_letters(@)',
                                    functions=CustomFunctions())
        self.assertTrue(prepared.is_prepared_for(CustomFunctions()))
        self.assertEqual(prepared.search('ccbbadd'), 'abcd')

    def test_inherited_functions_are_visible_to_subclasses(self):
        prepared = jmespath.prepare(
            'my_add(length(@), `1`)', functions=CustomFunctions())
        self.assertEqual(prepared.required_functions,
                         ('length', 'my_add'))
        self.assertEqual(prepared.search('abcabc'), 7)

    def test_subclass_removing_a_function_breaks_preparation(self):
        with self.assertRaises(exceptions.UnknownFunctionError):
            jmespath.prepare('length(@)',
                             functions=UnregisteredOverrideFunctions())

    def test_signature_change_breaks_capability(self):
        base = jmespath.prepare('length(@)')
        self.assertFalse(base.is_prepared_for(SubsetFunctions()))
        with self.assertRaises(
                exceptions.FunctionCapabilityMismatchError):
            base.verify(SubsetFunctions())

    def test_reordered_equivalent_signature_keeps_capability(self):
        base = jmespath.prepare('length(@)')
        self.assertTrue(base.is_prepared_for(ReorderedSignatureFunctions()))

    def test_search_with_incompatible_functions_raises(self):
        prepared = jmespath.prepare('unique_letters(@)',
                                    functions=CustomFunctions())
        options = jmespath.Options(
            custom_functions=functions.Functions())
        with self.assertRaises(
                exceptions.FunctionCapabilityMismatchError) as e:
            prepared.search('abc', options=options)
        self.assertEqual(e.exception.missing, ['unique_letters'])

    def test_unrelated_function_does_not_invalidate(self):
        prepared = jmespath.prepare('length(@)')
        self.assertTrue(prepared.is_prepared_for(ExtraFunctions()))
        self.assertIn('shout', ExtraFunctions.FUNCTION_TABLE)

    def test_fingerprint_identifies_capabilities_not_instances(self):
        first = jmespath.prepare('length(@)', functions=functions.Functions())
        second = jmespath.prepare('length(@)', functions=functions.Functions())
        self.assertEqual(first.capability_fingerprint,
                         second.capability_fingerprint)
        other = jmespath.prepare('unique_letters(@)',
                                 functions=CustomFunctions())
        self.assertNotEqual(first.capability_fingerprint,
                            other.capability_fingerprint)

    def test_mutated_function_table_invalidates_preparation(self):
        instance = CustomFunctions()
        prepared = jmespath.prepare('unique_letters(@)', functions=instance)
        self.assertTrue(prepared.is_prepared_for(instance))
        # Simulate an instance-level table that lost the function.
        instance.FUNCTION_TABLE = {
            name: spec for name, spec in instance.FUNCTION_TABLE.items()
            if name != 'unique_letters'
        }
        self.assertFalse(prepared.is_prepared_for(instance))
        with self.assertRaises(
                exceptions.FunctionCapabilityMismatchError):
            prepared.search('abc')

    def test_search_accepts_prepared_functions_instance(self):
        instance = CustomFunctions()
        prepared = jmespath.prepare('unique_letters(@)', functions=instance)
        self.assertEqual(prepared.search('bbaa'), 'ab')

    def test_repr_names_expression_and_functions(self):
        prepared = jmespath.prepare('sort_by(items, &length(name))')
        text = repr(prepared)
        self.assertIn('sort_by(items, &length(name))', text)
        self.assertIn('sort_by', text)
        self.assertIn('length', text)


class TestOptionsIntegration(unittest.TestCase):
    def test_options_custom_functions_are_used(self):
        options = jmespath.Options(custom_functions=CustomFunctions())
        prepared = jmespath.prepare('my_add(`1`, `2`)', options=options)
        self.assertEqual(prepared.required_functions, ('my_add',))
        self.assertEqual(prepared.search({}), 3)

    def test_functions_and_options_cannot_both_be_given(self):
        options = jmespath.Options(custom_functions=CustomFunctions())
        with self.assertRaises(ValueError):
            jmespath.prepare('length(@)', functions=CustomFunctions(),
                             options=options)

    def test_prepared_search_respects_dict_cls(self):
        from collections import OrderedDict
        prepared = jmespath.prepare('{a: a, b: b}')
        result = prepared.search(
            {'a': 1, 'b': 2},
            options=jmespath.Options(dict_cls=OrderedDict))
        self.assertIsInstance(result, OrderedDict)


class TestCompilationStaysIndependent(unittest.TestCase):
    def test_compile_does_not_require_functions(self):
        parsed = jmespath.compile('length(@)')
        self.assertEqual(parsed.search([1, 2]), 2)

    def test_unknown_function_still_compiles(self):
        parsed = jmespath.compile('does_not_exist(@)')
        with self.assertRaises(exceptions.UnknownFunctionError):
            parsed.search({})

    def test_parsed_result_prepare_accepts_instance(self):
        parsed = jmespath.compile('unique_letters(@)')
        prepared = parsed.prepare(functions=CustomFunctions())
        self.assertEqual(prepared.search('aabb'), 'ab')

    def test_functions_prepare_is_instance_first(self):
        prepared = CustomFunctions().prepare('unique_letters(@)')
        self.assertEqual(prepared.search('zzyy'), 'yz')

    def test_top_level_and_parsed_prepare_agree(self):
        left = jmespath.prepare('my_add(`1`, `2`)',
                                functions=CustomFunctions())
        right = jmespath.compile('my_add(`1`, `2`)').prepare(
            functions=CustomFunctions())
        self.assertEqual(left.capability_fingerprint,
                         right.capability_fingerprint)


class TestFunctionTableFingerprint(unittest.TestCase):
    def test_subclass_table_does_not_mutate_base_table(self):
        before = functions.Functions().capability_fingerprint()
        CustomFunctions()  # class creation already happened at import time
        after = functions.Functions().capability_fingerprint()
        self.assertEqual(before, after)

    def test_instance_fingerprint_is_stable(self):
        self.assertEqual(
            CustomFunctions().capability_fingerprint(),
            CustomFunctions().capability_fingerprint())

    def test_fingerprint_is_hex_digest(self):
        fingerprint = functions.Functions().capability_fingerprint()
        self.assertEqual(len(fingerprint), 64)
        int(fingerprint, 16)

    def test_signature_arity_for_fixed_signature(self):
        sig = [{'types': ['number']}, {'types': ['number']}]
        self.assertEqual(functions.signature_arity(sig), (2, 2))

    def test_signature_arity_for_variadic_signature(self):
        sig = [{'types': ['object'], 'variadic': True}]
        self.assertEqual(functions.signature_arity(sig), (1, None))

    def test_canonical_signature_normalizes_type_order(self):
        left = functions.canonical_signature(
            [{'types': ['object', 'string', 'array']}])
        right = functions.canonical_signature(
            [{'types': ['array', 'object', 'string']}])
        self.assertEqual(left, right)

    def test_check_arity_matches_validate_arguments(self):
        instance = functions.Functions()
        signature = [{'types': ['string']}, {'types': ['string']}]
        with self.assertRaises(exceptions.ArityError):
            functions.check_arity(signature, 1, 'starts_with')
        with self.assertRaises(exceptions.ArityError):
            instance._validate_arguments(['a'], signature, 'starts_with')


class TestPreparationPrimitives(unittest.TestCase):
    def test_prepare_does_not_mutate_parsed_ast(self):
        parsed = jmespath.compile('sort_by(items, &length(name))')
        before = json.dumps(parsed.parsed, sort_keys=True)
        parsed.prepare()
        self.assertEqual(json.dumps(parsed.parsed, sort_keys=True), before)

    def test_prepare_returns_same_parsed_result(self):
        parsed = jmespath.compile('length(@)')
        prepared = parsed.prepare()
        self.assertIs(prepared.parsed, parsed)

    def test_invalid_functions_argument_is_rejected(self):
        with self.assertRaises(TypeError):
            jmespath.prepare('length(@)', functions=object())

    def test_accepts_functions_subclass_helper(self):
        prepared = jmespath.prepare('length(@)', functions=SubsetFunctions)
        self.assertTrue(prepared.is_prepared_for(SubsetFunctions()))

    def test_requirements_ignores_slice_plain_children(self):
        prepared = jmespath.prepare('a[0:2:1][*].b')
        self.assertEqual(prepared.required_functions, ())


if __name__ == '__main__':
    unittest.main()
