from __future__ import absolute_import
from unittest import TestCase

import simplejson as json
from simplejson.compat import StringIO


class HookError(Exception):
    """Distinct exception type so tests can prove it is not converted
    into a JSONDecodeError."""


class NumberHookError(Exception):
    pass


class CallLog(object):
    """Records hook invocations as (name, value) tuples in call order."""

    def __init__(self):
        self.calls = []

    def pairs_hook(self, pairs):
        self.calls.append(('pairs', list(pairs)))
        return [('wrapped', list(pairs))]

    def object_hook(self, obj):
        self.calls.append(('object', obj))
        return ('object_hook_result', obj)

    def make_decoder(self, **kw):
        kw.setdefault('object_pairs_hook', self.pairs_hook)
        kw.setdefault('object_hook', self.object_hook)
        return json.JSONDecoder(**kw)


class TestHookModePriority(TestCase):
    def test_default_mode_is_priority(self):
        log = CallLog()
        result = log.make_decoder().decode('{"a": 1}')
        # historical behavior: object_pairs_hook wins, object_hook ignored
        self.assertEqual([name for name, _ in log.calls], ['pairs'])
        self.assertEqual(result, [('wrapped', [('a', 1)])])

    def test_explicit_priority_mode(self):
        log = CallLog()
        result = log.make_decoder(object_hook_mode='priority').decode('{"a": 1}')
        self.assertEqual([name for name, _ in log.calls], ['pairs'])
        self.assertEqual(result, [('wrapped', [('a', 1)])])

    def test_loads_default_mode_unchanged(self):
        log = CallLog()
        result = json.loads('{"a": 1}',
                            object_hook=log.object_hook,
                            object_pairs_hook=log.pairs_hook)
        self.assertEqual([name for name, _ in log.calls], ['pairs'])
        self.assertEqual(result, [('wrapped', [('a', 1)])])


class TestHookModeChain(TestCase):
    def test_chain_passes_pairs_result_to_object_hook(self):
        log = CallLog()
        result = log.make_decoder(object_hook_mode='chain').decode('{"a": 1}')
        self.assertEqual([name for name, _ in log.calls], ['pairs', 'object'])
        pairs_arg = log.calls[0][1]
        object_arg = log.calls[1][1]
        self.assertEqual(pairs_arg, [('a', 1)])
        # object_hook received exactly what object_pairs_hook returned
        self.assertEqual(object_arg, [('wrapped', [('a', 1)])])
        self.assertEqual(result,
                         ('object_hook_result', [('wrapped', [('a', 1)])]))

    def test_chain_empty_object(self):
        log = CallLog()
        result = log.make_decoder(object_hook_mode='chain').decode('{}')
        self.assertEqual([name for name, _ in log.calls], ['pairs', 'object'])
        self.assertEqual(log.calls[0][1], [])
        self.assertEqual(log.calls[1][1], [('wrapped', [])])
        self.assertEqual(result, ('object_hook_result', [('wrapped', [])]))

    def test_chain_nested_objects_inside_out(self):
        log = CallLog()
        result = log.make_decoder(object_hook_mode='chain').decode(
            '{"outer": {"inner": {}}, "z": 0}')
        names = [name for name, _ in log.calls]
        # three objects, innermost first: {}, {"inner": ...}, then the
        # outermost object
        self.assertEqual(names, ['pairs', 'object'] * 3)
        # innermost object hooks ran first, with the empty pair list
        self.assertEqual(log.calls[0][1], [])
        self.assertEqual(log.calls[1][1], [('wrapped', [])])
        # each enclosing object saw the fully hooked inner value, as-is
        r0 = ('object_hook_result', [('wrapped', [])])
        self.assertEqual(log.calls[2][1], [('inner', r0)])
        r1 = ('object_hook_result', [('wrapped', [('inner', r0)])])
        self.assertEqual(log.calls[4][1], [('outer', r1), ('z', 0)])
        self.assertEqual(
            result,
            ('object_hook_result', [('wrapped', [('outer', r1), ('z', 0)])]))

    def test_chain_objects_nested_in_arrays(self):
        log = CallLog()
        result = log.make_decoder(object_hook_mode='chain').decode(
            '[{"a": {}}, []]')
        names = [name for name, _ in log.calls]
        # array nesting does not change inside-out object hook order
        self.assertEqual(names, ['pairs', 'object', 'pairs', 'object'])
        inner_result = ('object_hook_result', [('wrapped', [])])
        self.assertEqual(log.calls[2][1], [('a', inner_result)])
        outer_result = ('object_hook_result',
                        [('wrapped', [('a', inner_result)])])
        self.assertEqual(result, [outer_result, []])

    def test_chain_non_mapping_passthrough(self):
        # hooks may return non-mapping objects; the value is passed along
        # as-is, never re-encoded and re-decoded
        sentinel = object()
        seen = []

        def pairs_hook(pairs):
            return sentinel

        def object_hook(obj):
            seen.append(obj)
            return ['not', 'a', 'mapping']

        result = json.loads('{"a": {"b": 1}}',
                            object_pairs_hook=pairs_hook,
                            object_hook=object_hook,
                            object_hook_mode='chain')
        self.assertEqual(seen, [sentinel, sentinel])
        self.assertIs(seen[0], sentinel)
        self.assertEqual(result, ['not', 'a', 'mapping'])

    def test_chain_with_only_object_hook(self):
        log = CallLog()
        decoder = json.JSONDecoder(object_hook=log.object_hook,
                                   object_hook_mode='chain')
        result = decoder.decode('{"a": 1}')
        # with no pairs hook, object_hook still receives the dict
        self.assertEqual(log.calls, [('object', {'a': 1})])
        self.assertEqual(result, ('object_hook_result', {'a': 1}))

    def test_chain_with_only_pairs_hook(self):
        log = CallLog()
        decoder = json.JSONDecoder(object_pairs_hook=log.pairs_hook,
                                   object_hook_mode='chain')
        result = decoder.decode('{"a": 1}')
        self.assertEqual(log.calls, [('pairs', [('a', 1)])])
        self.assertEqual(result, [('wrapped', [('a', 1)])])

    def test_loads_and_load_accept_mode(self):
        log = CallLog()
        kw = dict(object_hook=log.object_hook,
                  object_pairs_hook=log.pairs_hook,
                  object_hook_mode='chain')
        result = json.loads('{"a": 1}', **kw)
        self.assertEqual([name for name, _ in log.calls], ['pairs', 'object'])
        self.assertEqual(result,
                         ('object_hook_result', [('wrapped', [('a', 1)])]))
        log2 = CallLog()
        kw2 = dict(object_hook=log2.object_hook,
                   object_pairs_hook=log2.pairs_hook,
                   object_hook_mode='chain')
        result2 = json.load(StringIO('{"a": 1}'), **kw2)
        self.assertEqual([name for name, _ in log2.calls], ['pairs', 'object'])
        self.assertEqual(result2, result)


class TestHookModeExclusive(TestCase):
    def test_exclusive_rejects_both_hooks(self):
        self.assertRaises(
            ValueError,
            json.JSONDecoder,
            object_hook=lambda d: d,
            object_pairs_hook=lambda p: p,
            object_hook_mode='exclusive')

    def test_exclusive_rejects_both_hooks_via_loads(self):
        self.assertRaises(
            ValueError,
            json.loads, '{"a": 1}',
            object_hook=lambda d: d,
            object_pairs_hook=lambda p: p,
            object_hook_mode='exclusive')

    def test_exclusive_allows_single_hooks(self):
        self.assertEqual(
            json.loads('{"a": 1}',
                       object_hook=lambda d: ('hooked', d),
                       object_hook_mode='exclusive'),
            ('hooked', {'a': 1}))
        self.assertEqual(
            json.loads('{"a": 1}',
                       object_pairs_hook=lambda p: ('pairs', p),
                       object_hook_mode='exclusive'),
            ('pairs', [('a', 1)]))

    def test_invalid_mode_rejected(self):
        self.assertRaises(
            ValueError, json.JSONDecoder, object_hook_mode='bogus')
        self.assertRaises(
            ValueError, json.loads, '{"a": 1}', object_hook_mode='bogus')


class TestHookFailurePaths(TestCase):
    def test_pairs_hook_exception_not_wrapped(self):
        def bad_pairs_hook(pairs):
            raise HookError('pairs hook failed')
        try:
            json.loads('{"a": 1}', object_pairs_hook=bad_pairs_hook)
            self.fail('expected HookError')
        except HookError as e:
            self.assertEqual(str(e), 'pairs hook failed')
        except json.JSONDecodeError as e:
            self.fail('hook exception was converted to JSONDecodeError: %r'
                      % (e,))

    def test_object_hook_exception_not_wrapped(self):
        def bad_object_hook(obj):
            raise HookError('object hook failed')
        for kw in ({'object_hook': bad_object_hook},
                   {'object_hook': bad_object_hook,
                    'object_pairs_hook': lambda p: p,
                    'object_hook_mode': 'chain'}):
            try:
                json.loads('{"a": 1}', **kw)
                self.fail('expected HookError')
            except HookError as e:
                self.assertEqual(str(e), 'object hook failed')
            except json.JSONDecodeError as e:
                self.fail('hook exception was converted to JSONDecodeError: '
                          '%r' % (e,))

    def test_inner_hook_failure_stops_outer_hooks(self):
        log = CallLog()

        def object_hook(obj):
            log.object_hook(obj)
            raise HookError('inner failure')

        decoder = json.JSONDecoder(object_hook=object_hook)
        self.assertRaises(HookError, decoder.decode,
                          '{"outer": {"inner": {}}}')
        # only the innermost object hook ran; the failure propagated
        # outward without invoking the outer object's hook
        self.assertEqual(log.calls, [('object', {})])

    def test_chain_pairs_hook_failure_skips_object_hook(self):
        log = CallLog()

        def bad_pairs_hook(pairs):
            log.pairs_hook(pairs)
            raise HookError('pairs failure')

        decoder = json.JSONDecoder(object_pairs_hook=bad_pairs_hook,
                                   object_hook=log.object_hook,
                                   object_hook_mode='chain')
        self.assertRaises(HookError, decoder.decode, '{"a": 1}')
        self.assertEqual([name for name, _ in log.calls], ['pairs'])

    def test_hook_failure_on_empty_object(self):
        def bad_object_hook(obj):
            raise HookError('empty object hook failed')
        self.assertRaises(HookError, json.loads, '{}',
                          object_hook=bad_object_hook)


class TestNumberCallbacks(TestCase):
    def test_number_callbacks_only_see_numeric_tokens(self):
        ints = []
        floats = []

        def parse_int(s):
            ints.append(s)
            return int(s)

        def parse_float(s):
            floats.append(s)
            return float(s)

        result = json.loads(
            '{"i": 1, "f": 1.5, "s": "2", "n": null, "b": true,'
            ' "a": [-3, 2e4, "4.5"]}',
            parse_int=parse_int, parse_float=parse_float)
        self.assertEqual(ints, ['1', '-3'])
        self.assertEqual(floats, ['1.5', '2e4'])
        self.assertEqual(result['s'], '2')
        self.assertEqual(result['a'][2], '4.5')

    def test_parse_int_exception_not_json_error(self):
        def parse_int(s):
            raise NumberHookError('bad int: %s' % s)
        for doc in ('1', '[1, 2]', '{"a": 1}'):
            try:
                json.loads(doc, parse_int=parse_int)
                self.fail('expected NumberHookError')
            except NumberHookError as e:
                self.assertTrue(str(e).startswith('bad int: '))
            except json.JSONDecodeError as e:
                self.fail('parse_int exception was converted to '
                          'JSONDecodeError: %r' % (e,))

    def test_parse_float_exception_not_json_error(self):
        def parse_float(s):
            raise NumberHookError('bad float: %s' % s)
        for doc in ('1.5', '[1.5]', '{"a": 1.5}'):
            try:
                json.loads(doc, parse_float=parse_float)
                self.fail('expected NumberHookError')
            except NumberHookError as e:
                self.assertTrue(str(e).startswith('bad float: '))
            except json.JSONDecodeError as e:
                self.fail('parse_float exception was converted to '
                          'JSONDecodeError: %r' % (e,))

    def test_parse_constant_not_routed_to_number_callbacks(self):
        ints = []
        floats = []
        constants = []
        json.loads('[NaN, Infinity, -Infinity, 1, 1.5]',
                   parse_int=ints.append,
                   parse_float=floats.append,
                   parse_constant=constants.append,
                   allow_nan=True)
        self.assertEqual(ints, ['1'])
        self.assertEqual(floats, ['1.5'])
        self.assertEqual(constants, ['NaN', 'Infinity', '-Infinity'])
