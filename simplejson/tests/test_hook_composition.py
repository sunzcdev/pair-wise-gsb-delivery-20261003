from __future__ import absolute_import
from unittest import TestCase

import simplejson as json
from simplejson import decoder
from simplejson.compat import StringIO
from simplejson.scanner import py_make_scanner
from simplejson.tests._helpers import skip_if_speedups_missing


class HookFailure(Exception):
    """Deliberately not a ValueError, so a test can tell it apart from
    JSONDecodeError (which is a ValueError subclass)."""


class CallLog(object):
    """Hooks that record what they were called with, in call order.

    ``pairs_result`` is what the pairs hook returns; by default it is a
    list of pairs so tests can prove a non-mapping value is handed to
    the object hook without being re-encoded and re-decoded.
    """

    _unset = object()

    def __init__(self, pairs_result=_unset):
        self.calls = []
        self.pairs_result = pairs_result

    def pairs_hook(self, pairs):
        self.calls.append(('pairs', list(pairs)))
        if self.pairs_result is not CallLog._unset:
            return self.pairs_result
        return [('wrapped', list(pairs))]

    def object_hook(self, obj):
        self.calls.append(('object', obj))
        return ('object_result', obj)

    def names(self):
        return [name for name, _ in self.calls]


class TestDefaultPriorityMode(TestCase):
    """The historical behavior must be untouched when no mode is given."""

    def test_pairs_hook_wins_and_object_hook_is_skipped(self):
        log = CallLog()
        result = json.loads('{"a": 1}', object_hook=log.object_hook,
                            object_pairs_hook=log.pairs_hook)
        self.assertEqual(log.names(), ['pairs'])
        self.assertEqual(result, [('wrapped', [('a', 1)])])

    def test_explicit_priority_matches_default(self):
        log = CallLog()
        result = json.loads('{"a": 1}', object_hook=log.object_hook,
                            object_pairs_hook=log.pairs_hook,
                            object_hook_mode='priority')
        self.assertEqual(log.names(), ['pairs'])
        self.assertEqual(result, [('wrapped', [('a', 1)])])

    def test_decoder_accepts_mode_and_keeps_attributes(self):
        log = CallLog()
        decoder = json.JSONDecoder(object_hook=log.object_hook,
                                   object_pairs_hook=log.pairs_hook,
                                   object_hook_mode='priority')
        self.assertEqual(decoder.object_hook_mode, 'priority')
        self.assertEqual(decoder.decode('{}'), [('wrapped', [])])
        self.assertEqual(log.names(), ['pairs'])


class TestChainMode(TestCase):
    def make_decoder(self, log, **kw):
        kw.setdefault('object_hook', log.object_hook)
        kw.setdefault('object_pairs_hook', log.pairs_hook)
        kw.setdefault('object_hook_mode', 'chain')
        return json.JSONDecoder(**kw)

    def test_pairs_result_is_passed_to_object_hook(self):
        log = CallLog()
        result = self.make_decoder(log).decode('{"a": 1}')
        self.assertEqual(log.names(), ['pairs', 'object'])
        self.assertEqual(log.calls[0][1], [('a', 1)])
        self.assertEqual(log.calls[1][1], [('wrapped', [('a', 1)])])
        self.assertEqual(result, ('object_result', [('wrapped', [('a', 1)])]))

    def test_empty_object_runs_both_hooks(self):
        log = CallLog()
        result = self.make_decoder(log).decode('{}')
        self.assertEqual(log.names(), ['pairs', 'object'])
        self.assertEqual(log.calls[0][1], [])
        self.assertEqual(log.calls[1][1], [('wrapped', [])])
        self.assertEqual(result, ('object_result', [('wrapped', [])]))

    def test_empty_nested_object_runs_both_hooks(self):
        log = CallLog()
        self.make_decoder(log).decode('{"outer": {}}')
        self.assertEqual(log.names(), ['pairs', 'object', 'pairs', 'object'])
        self.assertEqual(log.calls[0][1], [])
        hooked = ('object_result', [('wrapped', [])])
        self.assertEqual(log.calls[1][1], [('wrapped', [])])
        self.assertEqual(log.calls[2][1], [('outer', hooked)])
        self.assertEqual(log.calls[3][1], [('wrapped', [('outer', hooked)])])

    def test_nested_objects_run_inside_out(self):
        log = CallLog()
        result = self.make_decoder(log).decode(
            '{"outer": {"middle": {"inner": 1}}}')
        # three objects, each running pairs then object hook
        self.assertEqual(log.names(), ['pairs', 'object'] * 3)
        inner = ('object_result', [('wrapped', [('inner', 1)])])
        middle = ('object_result', [('wrapped', [('middle', inner)])])
        outer = ('object_result', [('wrapped', [('outer', middle)])])
        # each enclosing object saw the fully hooked inner value as-is
        self.assertEqual(log.calls[2][1], [('middle', inner)])
        self.assertEqual(log.calls[4][1], [('outer', middle)])
        self.assertEqual(result, outer)

    def test_sibling_objects_run_left_to_right(self):
        log = CallLog()
        result = self.make_decoder(log).decode(
            '{"a": {"x": 1}, "b": {"y": 2}}')
        self.assertEqual(log.names(), ['pairs', 'object'] * 3)
        hooked_a = ('object_result', [('wrapped', [('x', 1)])])
        hooked_b = ('object_result', [('wrapped', [('y', 2)])])
        # the outer object's pairs hook sees both siblings already hooked,
        # in document order
        self.assertEqual(log.calls[4][1], [('a', hooked_a), ('b', hooked_b)])
        self.assertEqual(result,
                         ('object_result',
                          [('wrapped', [('a', hooked_a), ('b', hooked_b)])]))

    def test_objects_nested_in_arrays(self):
        log = CallLog()
        result = self.make_decoder(log).decode('[{"a": {}}, [], [{"b": 2}]]')
        # array nesting does not disturb inside-out object hook order
        self.assertEqual(log.names(), ['pairs', 'object'] * 3)
        self.assertEqual(log.calls[0][1], [])
        hooked_a = ('object_result', [('wrapped', [])])
        self.assertEqual(log.calls[2][1], [('a', hooked_a)])
        self.assertEqual(log.calls[4][1], [('b', 2)])
        # the arrays themselves have no array_hook, so the outer list holds
        # the object hook result of each element it contained
        self.assertEqual(result,
                         [('object_result', [('wrapped', [('a', hooked_a)])]),
                          [],
                          [('object_result', [('wrapped', [('b', 2)])])]])

    def test_non_mapping_result_is_handed_over_untouched(self):
        sentinel = object()
        log = CallLog(pairs_result=sentinel)
        result = self.make_decoder(log).decode('{"a": 1}')
        self.assertIs(log.calls[1][1], sentinel)
        self.assertEqual(result, ('object_result', sentinel))
        self.assertIs(result[1], sentinel)

    def test_non_mapping_result_nested_is_passed_up_untouched(self):
        sentinel = object()

        def pairs_hook(pairs):
            return sentinel

        seen = []

        def object_hook(obj):
            seen.append(obj)
            return ['not', 'a', 'mapping']

        result = json.loads('{"a": {"b": 1}}', object_pairs_hook=pairs_hook,
                            object_hook=object_hook,
                            object_hook_mode='chain')
        # inner object first, then the outer one; both get the sentinel
        self.assertEqual(len(seen), 2)
        self.assertIs(seen[0], sentinel)
        self.assertIs(seen[1], sentinel)
        self.assertEqual(result, ['not', 'a', 'mapping'])

    def test_chain_with_only_object_hook(self):
        log = CallLog()
        result = json.loads('{"a": 1}', object_hook=log.object_hook,
                            object_hook_mode='chain')
        self.assertEqual(log.calls, [('object', {'a': 1})])
        self.assertEqual(result, ('object_result', {'a': 1}))

    def test_chain_with_only_pairs_hook(self):
        log = CallLog()
        result = json.loads('{"a": 1}', object_pairs_hook=log.pairs_hook,
                            object_hook_mode='chain')
        self.assertEqual(log.calls, [('pairs', [('a', 1)])])
        self.assertEqual(result, [('wrapped', [('a', 1)])])

    def test_chain_via_load(self):
        log = CallLog()
        result = json.load(StringIO('{"a": 1}'), object_hook=log.object_hook,
                           object_pairs_hook=log.pairs_hook,
                           object_hook_mode='chain')
        self.assertEqual(log.names(), ['pairs', 'object'])
        self.assertEqual(result, ('object_result', [('wrapped', [('a', 1)])]))

    def test_array_hook_composes_with_chain(self):
        log = CallLog()
        result = json.loads('[{"a": 1}]', object_hook=log.object_hook,
                            object_pairs_hook=log.pairs_hook,
                            array_hook=tuple,
                            object_hook_mode='chain')
        self.assertIsInstance(result, tuple)
        self.assertEqual(result, (('object_result',
                                   [('wrapped', [('a', 1)])]),))


class TestExclusiveMode(TestCase):
    def test_both_hooks_rejected_at_construction(self):
        def unused(value):
            raise AssertionError('hook must not run')

        self.assertRaises(ValueError, json.JSONDecoder,
                          object_hook=unused, object_pairs_hook=unused,
                          object_hook_mode='exclusive')
        self.assertRaises(ValueError, json.loads, '{"a": 1}',
                          object_hook=unused, object_pairs_hook=unused,
                          object_hook_mode='exclusive')

    def test_single_hook_allowed(self):
        self.assertEqual(
            json.loads('{"a": 1}', object_hook=lambda d: ('hooked', d),
                       object_hook_mode='exclusive'),
            ('hooked', {'a': 1}))
        self.assertEqual(
            json.loads('{"a": 1}', object_pairs_hook=lambda p: ('pairs', p),
                       object_hook_mode='exclusive'),
            ('pairs', [('a', 1)]))

    def test_unknown_mode_rejected(self):
        for mode in ('sequential', 'both', '', 'PRIORITY', 1, True):
            self.assertRaises(ValueError, json.JSONDecoder,
                              object_hook_mode=mode)
            self.assertRaises(ValueError, json.loads, '{}',
                              object_hook_mode=mode)


class TestFailurePaths(TestCase):
    def test_pairs_hook_error_is_not_a_json_error(self):
        def bad_pairs_hook(pairs):
            raise HookFailure('pairs hook failed')

        try:
            json.loads('{"a": 1}', object_pairs_hook=bad_pairs_hook)
        except HookFailure as e:
            self.assertEqual(str(e), 'pairs hook failed')
        else:
            self.fail('expected HookFailure')

    def test_object_hook_error_is_not_a_json_error(self):
        def bad_object_hook(obj):
            raise HookFailure('object hook failed')

        for kwargs in ({'object_hook': bad_object_hook},
                       {'object_hook': bad_object_hook,
                        'object_pairs_hook': lambda pairs: pairs,
                        'object_hook_mode': 'chain'},
                       {'object_hook': bad_object_hook,
                        'object_hook_mode': 'exclusive'}):
            try:
                json.loads('{"a": 1}', **kwargs)
            except HookFailure as e:
                self.assertEqual(str(e), 'object hook failed')
            else:
                self.fail('expected HookFailure for %r' % (kwargs,))

    def test_failing_inner_hook_stops_outer_hooks(self):
        log = CallLog()

        def object_hook(obj):
            log.object_hook(obj)
            raise HookFailure('inner failure')

        decoder = json.JSONDecoder(object_hook=object_hook)
        self.assertRaises(HookFailure, decoder.decode,
                          '{"outer": {"inner": {}}}')
        # only the innermost object's hook ran; the error travelled
        # outwards without invoking the enclosing object's hook
        self.assertEqual(log.calls, [('object', {})])

    def test_failing_pairs_hook_skips_object_hook(self):
        log = CallLog()

        def bad_pairs_hook(pairs):
            log.pairs_hook(pairs)
            raise HookFailure('pairs failure')

        decoder = json.JSONDecoder(object_pairs_hook=bad_pairs_hook,
                                   object_hook=log.object_hook,
                                   object_hook_mode='chain')
        self.assertRaises(HookFailure, decoder.decode, '{"a": {"b": 1}}')
        # the inner pairs hook failed first, so nothing else ran
        self.assertEqual(log.names(), ['pairs'])

    def test_hook_failure_on_empty_object(self):
        def bad_object_hook(obj):
            raise HookFailure('empty')

        self.assertRaises(HookFailure, json.loads, '{}',
                          object_hook=bad_object_hook)
        self.assertRaises(HookFailure, json.loads, '{}',
                          object_hook=bad_object_hook,
                          object_hook_mode='exclusive')

    def test_syntax_error_after_hooks_still_raises_json_error(self):
        log = CallLog()

        def decoder(**kw):
            kw.setdefault('object_hook', log.object_hook)
            kw.setdefault('object_pairs_hook', log.pairs_hook)
            kw.setdefault('object_hook_mode', 'chain')
            return json.JSONDecoder(**kw)

        # the inner object decoded (and hooked) before the syntax error
        self.assertRaises(json.JSONDecodeError, decoder().decode,
                          '{"a": {"b": 1}, "c": }')
        self.assertEqual(log.names(), ['pairs', 'object'])


class TestNumericCallbacks(TestCase):
    def test_callbacks_only_see_numeric_tokens(self):
        ints = []
        floats = []

        def parse_int(s):
            ints.append(s)
            return int(s)

        def parse_float(s):
            floats.append(s)
            return float(s)

        result = json.loads(
            '{"i": 1, "f": 1.5, "s": "2", "t": true, "n": null,'
            ' "a": [-3, 2e4, "4.5"], "o": {"n": -0.5}}',
            parse_int=parse_int, parse_float=parse_float)
        self.assertEqual(ints, ['1', '-3'])
        self.assertEqual(floats, ['1.5', '2e4', '-0.5'])
        self.assertEqual(result['s'], '2')
        self.assertEqual(result['a'][2], '4.5')

    def test_constants_do_not_reach_numeric_callbacks(self):
        ints = []
        floats = []
        constants = []

        def parse_int(token):
            ints.append(token)
            return 'int'

        def parse_float(token):
            floats.append(token)
            return 'float'

        result = json.loads('[NaN, Infinity, -Infinity, 1, 1.5]',
                            parse_int=parse_int, parse_float=parse_float,
                            parse_constant=constants.append, allow_nan=True)
        # the numeric callbacks only ever saw numeric tokens
        self.assertEqual(ints, ['1'])
        self.assertEqual(floats, ['1.5'])
        self.assertEqual(constants, ['NaN', 'Infinity', '-Infinity'])
        self.assertEqual(result[:3], [None, None, None])
        self.assertEqual(result[3:], ['int', 'float'])

    def test_parse_int_error_is_not_a_json_error(self):
        def parse_int(s):
            raise HookFailure('bad int: %s' % s)

        for doc in ('1', '[1, 2]', '{"a": 1}', '{"a": {"b": 13}}'):
            try:
                json.loads(doc, parse_int=parse_int)
            except HookFailure as e:
                self.assertTrue(str(e).startswith('bad int: '))
            else:
                self.fail('expected HookFailure for %r' % (doc,))

    def test_parse_float_error_is_not_a_json_error(self):
        def parse_float(s):
            raise HookFailure('bad float: %s' % s)

        for doc in ('1.5', '[1.5]', '{"a": 1.5}', '{"a": [-2.25]}'):
            try:
                json.loads(doc, parse_float=parse_float)
            except HookFailure as e:
                self.assertTrue(str(e).startswith('bad float: '))
            else:
                self.fail('expected HookFailure for %r' % (doc,))

    def test_numeric_error_does_not_run_object_hooks(self):
        log = CallLog()

        def parse_int(token):
            raise HookFailure('boom')

        decoder = json.JSONDecoder(object_hook=log.object_hook,
                                   object_pairs_hook=log.pairs_hook,
                                   object_hook_mode='chain',
                                   parse_int=parse_int)
        self.assertRaises(HookFailure, decoder.decode, '{"a": {"b": 1}}')
        self.assertEqual(log.calls, [])


class TestScannerIntegration(TestCase):
    """The mode has to survive the trip to the scanner, in both
    implementations, without disturbing old custom scanners."""

    def make_python_scanner_decoder(self, parse_object):
        dec = json.JSONDecoder()
        dec.parse_object = parse_object
        dec.scan_once = py_make_scanner(dec)
        return dec

    def test_custom_parse_object_with_legacy_signature(self):
        # Third-party contexts registered a 7-argument parse_object
        # before object_hook_mode existed; the default mode must not
        # start passing an eighth argument to it.
        calls = []
        real = decoder.JSONObject

        def legacy_parse_object(state, encoding, strict, scan_once,
                                object_hook, object_pairs_hook, memo=None):
            calls.append('legacy')
            return real(state, encoding, strict, scan_once, object_hook,
                        object_pairs_hook, memo)

        dec = self.make_python_scanner_decoder(legacy_parse_object)
        self.assertEqual(dec.decode('{"a": 1}'), {'a': 1})
        self.assertEqual(calls, ['legacy'])

        # 'priority' is the same behavior, so it stays compatible too
        del calls[:]
        dec = json.JSONDecoder(object_hook_mode='priority')
        dec.parse_object = legacy_parse_object
        dec.scan_once = py_make_scanner(dec)
        self.assertEqual(dec.decode('{"a": 1}'), {'a': 1})
        self.assertEqual(calls, ['legacy'])

    def test_custom_parse_object_receives_non_default_mode(self):
        calls = []
        real = decoder.JSONObject

        def recording_parse_object(*args, **kwargs):
            calls.append(args[7:])
            return real(*args, **kwargs)

        dec = json.JSONDecoder(object_hook_mode='chain')
        dec.parse_object = recording_parse_object
        dec.scan_once = py_make_scanner(dec)
        self.assertEqual(dec.decode('{"a": 1}'), {'a': 1})
        self.assertEqual(calls, [('chain',)])

    @skip_if_speedups_missing
    def test_c_scanner_exposes_mode_and_behaves(self):
        from simplejson._speedups import make_scanner
        dec = json.JSONDecoder(object_hook_mode='chain')
        self.assertIsInstance(dec.scan_once, make_scanner)
        self.assertEqual(dec.scan_once.object_hook_mode, 'chain')
        self.assertEqual(dec.decode('{"a": 1}'), {'a': 1})

        dec = json.JSONDecoder(object_hook_mode='exclusive')
        self.assertEqual(dec.scan_once.object_hook_mode, 'exclusive')


