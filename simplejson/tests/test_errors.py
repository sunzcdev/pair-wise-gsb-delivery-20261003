# -*- coding: utf-8 -*-
import sys, pickle
import unittest
from unittest import TestCase

import simplejson as json
from simplejson.compat import text_type, b

class TestErrors(TestCase):
    def test_string_keys_error(self):
        data = [{'a': 'A', 'b': (2, 4), 'c': 3.0, ('d',): 'D tuple'}]
        try:
            json.dumps(data)
        except TypeError:
            err = sys.exc_info()[1]
        else:
            self.fail('Expected TypeError')
        self.assertEqual(str(err),
                'keys must be str, int, float, bool or None, not tuple')

    def test_not_serializable(self):
        try:
            json.dumps(json)
        except TypeError:
            err = sys.exc_info()[1]
        else:
            self.fail('Expected TypeError')
        self.assertEqual(str(err),
                'Object of type module is not JSON serializable')

    def test_decode_error(self):
        err = None
        try:
            json.loads('{}\na\nb')
        except json.JSONDecodeError:
            err = sys.exc_info()[1]
        else:
            self.fail('Expected JSONDecodeError')
        self.assertEqual(err.lineno, 2)
        self.assertEqual(err.colno, 1)
        self.assertEqual(err.endlineno, 3)
        self.assertEqual(err.endcolno, 2)

    def test_scan_error(self):
        err = None
        for t in (text_type, b):
            try:
                json.loads(t('{"asdf": "'))
            except json.JSONDecodeError:
                err = sys.exc_info()[1]
            else:
                self.fail('Expected JSONDecodeError')
            self.assertEqual(err.lineno, 1)
            self.assertEqual(err.colno, 10)

    def test_error_is_pickable(self):
        err = None
        try:
            json.loads('{}\na\nb')
        except json.JSONDecodeError:
            err = sys.exc_info()[1]
        else:
            self.fail('Expected JSONDecodeError')
        s = pickle.dumps(err)
        e = pickle.loads(s)

        self.assertEqual(err.msg, e.msg)
        self.assertEqual(err.doc, e.doc)
        self.assertEqual(err.pos, e.pos)
        self.assertEqual(err.end, e.end)

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_override_preserves_exception(self):
        class NoteError(ValueError):
            def add_note(self, note):
                raise RuntimeError('note failed')

        error = NoteError('original error')

        def default(obj):
            raise error

        for value in (object(), [object()], {'key': object()}):
            try:
                json.dumps(value, default=default)
            except NoteError as exc:
                self.assertIs(exc, error)
            else:
                self.fail('Expected original NoteError')

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_key_repr_preserves_exception(self):
        class Key(str):
            def __repr__(self):
                raise RuntimeError('repr failed')

        error = ValueError('original error')

        def default(obj):
            raise error

        try:
            json.dumps({Key('key'): object()}, default=default)
        except ValueError as exc:
            self.assertIs(exc, error)
        else:
            self.fail('Expected original ValueError')

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_type_name_preserves_exception(self):
        class NameErrorType(type):
            def __getattribute__(cls, name):
                if name == '__name__':
                    raise RuntimeError('name failed')
                return type.__getattribute__(cls, name)

        error = ValueError('original error')

        def default(obj):
            raise error

        value = NameErrorType('Value', (object,), {})()
        values = (
            value,
            NameErrorType('List', (list,), {})([value]),
            NameErrorType('Dict', (dict,), {})({'key': value}),
        )
        for value in values:
            try:
                json.dumps(value, default=default)
            except ValueError as exc:
                self.assertIs(exc, error)
            else:
                self.fail('Expected original ValueError')

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_list_recursion(self):
        x = []
        x.append(x)
        try:
            json.dumps(x)
        except ValueError as exc:
            self.assertEqual(
                exc.__notes__, ['when serializing list item 0'])
        else:
            self.fail('Expected ValueError')

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_dict_recursion(self):
        x = {}
        x['test'] = x
        try:
            json.dumps(x)
        except ValueError as exc:
            self.assertEqual(
                exc.__notes__, ["when serializing dict item 'test'"])
        else:
            self.fail('Expected ValueError')

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_nested_error(self):
        try:
            json.dumps({'a': [1, object(), 3]})
        except TypeError as exc:
            self.assertEqual(len(exc.__notes__), 3)
            self.assertEqual(exc.__notes__[0],
                'when serializing object object')
            self.assertEqual(exc.__notes__[1],
                'when serializing list item 1')
            self.assertEqual(exc.__notes__[2],
                "when serializing dict item 'a'")
        else:
            self.fail('Expected TypeError')

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_uses_type_name(self):
        # Notes name type(obj).__name__, without the module prefix that
        # C types carry in tp_name.
        import collections
        import itertools
        cases = [
            (lambda: json.dumps(collections.OrderedDict(a=object())),
             ['when serializing object object',
              "when serializing OrderedDict item 'a'"]),
            (lambda: json.dumps(itertools.chain()),
             ['when serializing chain object']),
        ]
        for encode, notes in cases:
            with self.assertRaises(TypeError) as cm:
                encode()
            self.assertEqual(cm.exception.__notes__, notes)

    def test_key_error_uses_type_name(self):
        import decimal
        with self.assertRaises(TypeError) as cm:
            json.dumps({decimal.Decimal(1): 1}, use_decimal=False)
        self.assertEqual(
            str(cm.exception),
            'keys must be str, int, float, bool or None, not Decimal')

    def test_type_name_of_class_with_dotted_name(self):
        # A class's __name__ is used as is, even when it contains dots.
        Dotted = type('pkg.Dotted', (object,), {})
        with self.assertRaises(TypeError) as cm:
            json.dumps({Dotted(): 1})
        self.assertEqual(
            str(cm.exception),
            'keys must be str, int, float, bool or None, not pkg.Dotted')
        if sys.version_info >= (3, 11):
            DottedDict = type('pkg.DottedDict', (dict,), {})
            with self.assertRaises(TypeError) as cm:
                json.dumps(DottedDict(a=Dotted()))
            self.assertEqual(cm.exception.__notes__,
                             ['when serializing pkg.Dotted object',
                              "when serializing pkg.DottedDict item 'a'"])

    @unittest.skipIf(sys.version_info < (3,), 'Python 2 class names are bytes')
    def test_type_name_of_class_with_non_ascii_name(self):
        name = 'pkg.Über'
        Uber = type(name, (object,), {})
        with self.assertRaises(TypeError) as cm:
            json.dumps({Uber(): 1})
        self.assertEqual(
            str(cm.exception),
            'keys must be str, int, float, bool or None, not ' + name)

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_after_asdict_names_the_dict(self):
        import collections
        class D(dict):
            pass
        NT = collections.namedtuple('NT', 'x')
        with self.assertRaises(TypeError) as cm:
            json.dumps(D([('a', NT(1)), ('b', object())]))
        self.assertEqual(cm.exception.__notes__,
                         ['when serializing object object',
                          "when serializing D item 'b'"])

        class NotADict(object):
            def _asdict(self):
                return [1]
        with self.assertRaises(TypeError) as cm:
            json.dumps({'a': NotADict()})
        self.assertEqual(str(cm.exception),
                         '_asdict() must return a dict, not list')
        self.assertEqual(cm.exception.__notes__,
                         ["when serializing dict item 'a'"])


class _Unserializable(object):
    pass


class _ForJsonRaises(object):
    def __init__(self, exc):
        self.exc = exc
        self.calls = 0

    def for_json(self):
        self.calls += 1
        raise self.exc


class TestDiagnoseErrors(TestCase):
    """Location diagnostics for failed custom conversions.

    ``diagnose_errors=True`` turns a failure inside ``default()``,
    ``for_json()``, ``_asdict()`` or object-key conversion into a
    ``JSONEncodeError`` naming the container path, with the original
    exception preserved as ``__cause__``.
    """

    def test_default_failure_reports_record_and_field(self):
        records = [
            {'id': 1, 'name': 'ok'},
            {'id': 2, 'name': _Unserializable()},
        ]
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps(records, diagnose_errors=True)
        exc = cm.exception
        self.assertEqual(exc.path, '$[1].name')
        self.assertEqual(exc.path_parts, (1, 'name'))
        self.assertEqual(exc.kind, 'value')
        self.assertEqual(exc.value_type, '_Unserializable')
        self.assertIn('$[1].name', str(exc))

    def test_preserves_original_as_cause(self):
        error = ValueError('conversion blew up')

        def default(obj):
            raise error

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'a': _Unserializable()}, default=default,
                       diagnose_errors=True)
        exc = cm.exception
        self.assertIs(exc.__cause__, error)
        self.assertIs(exc.original, error)
        # The original message is not folded into the wrapper's text;
        # callers reach it through the chain.
        self.assertNotIn('conversion blew up', str(exc))
        # The chain renders as "direct cause", not the noisier
        # "during handling of the above exception".
        if sys.version_info >= (3,):
            import traceback
            text = ''.join(traceback.format_exception(
                type(exc), exc, exc.__traceback__))
            self.assertIn('direct cause', text)
            self.assertNotIn('during handling', text)
        # Existing PEP 678 notes are still attached to the wrapper.
        if sys.version_info >= (3, 11):
            self.assertIn("when serializing dict item 'a'", exc.__notes__)

    def test_wrapper_is_catchable_as_typeerror_and_valueerror(self):
        def default(obj):
            raise TypeError('nope')

        for caught in (TypeError, ValueError):
            with self.assertRaises(caught):
                json.dumps([_Unserializable()], default=default,
                           diagnose_errors=True)

    def test_for_json_failure_reports_path(self):
        failing = _ForJsonRaises(RuntimeError('no'))
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'outer': [0, failing]}, for_json=True,
                       diagnose_errors=True)
        self.assertEqual(cm.exception.path, '$.outer[1]')
        self.assertIn('RuntimeError', str(cm.exception))

    def test_root_failure_reports_dollar(self):
        failing = _ForJsonRaises(RuntimeError('no'))
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps(failing, for_json=True, diagnose_errors=True)
        self.assertEqual(cm.exception.path, '$')
        self.assertEqual(cm.exception.path_parts, ())

    def test_array_indices_only_for_entered_containers(self):
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps([[0, [1, [_Unserializable()]]]], diagnose_errors=True)
        self.assertEqual(cm.exception.path, '$[0][1][1][0]')
        self.assertEqual(cm.exception.path_parts, (0, 1, 1, 0))

    def test_path_quotes_keys_that_are_not_identifiers(self):
        value = {'a b': [{'c\'d': _Unserializable()}]}
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps(value, diagnose_errors=True)
        exc = cm.exception
        self.assertEqual(exc.path_parts, ('a b', 0, "c'd"))
        # Non-identifier keys are bracketed and repr-quoted, so the path
        # stays unambiguous.
        self.assertTrue(exc.path.startswith("$['a b'][0]["))
        self.assertTrue(exc.path.endswith("]"))
        self.assertIn("c'd", exc.path)

    def test_key_conversion_error_is_kind_key(self):
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'records': [{('bad',): 1}]}, diagnose_errors=True)
        exc = cm.exception
        self.assertEqual(exc.kind, 'key')
        self.assertEqual(exc.path, '$.records[0]')
        self.assertEqual(exc.value_type, 'tuple')
        self.assertIn('key of type tuple', str(exc))
        self.assertIsInstance(exc.__cause__, TypeError)

    def test_key_conversion_error_under_sort_keys(self):
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'r': [{'nested': {('bad',): 1}}]},
                       sort_keys=True, diagnose_errors=True)
        exc = cm.exception
        self.assertEqual(exc.kind, 'key')
        self.assertEqual(exc.path, '$.r[0].nested')

    def test_skipkeys_still_skips_without_diagnostic(self):
        self.assertEqual(
            json.dumps({'a': 1, ('bad',): 2}, skipkeys=True,
                       diagnose_errors=True),
            '{"a": 1}')

    def test_value_error_and_key_error_are_distinguished(self):
        with self.assertRaises(json.JSONEncodeError) as value_cm:
            json.dumps({'a': _Unserializable()}, diagnose_errors=True)
        with self.assertRaises(json.JSONEncodeError) as key_cm:
            json.dumps({('bad',): 1}, diagnose_errors=True)
        self.assertEqual(value_cm.exception.kind, 'value')
        self.assertEqual(key_cm.exception.kind, 'key')
        self.assertNotEqual(value_cm.exception.path,
                            key_cm.exception.path)

    def test_sensitive_value_not_in_message(self):
        secret = 'AKIA-very-secret-token'

        class Leaky(object):
            def for_json(self):
                raise ValueError('failed for ' + secret)

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'records': [{'token': Leaky()}]}, for_json=True,
                       diagnose_errors=True)
        exc = cm.exception
        self.assertNotIn(secret, str(exc))
        self.assertNotIn(secret, ' '.join(getattr(exc, '__notes__', [])))
        # The underlying failure is still reachable through the chain, so
        # a caller who opts in can inspect it deliberately.
        self.assertIn(secret, str(exc.__cause__))

    def test_sensitive_key_not_in_message(self):
        class SecretKey(object):
            def __str__(self):
                return 'password=hunter2'

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'a': {SecretKey(): 1}}, diagnose_errors=True)
        exc = cm.exception
        self.assertNotIn('hunter2', str(exc))
        self.assertEqual(exc.path_parts, ('a',))

    def test_conversion_callbacks_are_not_re_run(self):
        calls = []

        class Outer(object):
            def for_json(self):
                calls.append('outer')
                return {'inner': _Inner()}

        class _Inner(object):
            def for_json(self):
                calls.append('inner')
                raise ValueError('boom')

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps([Outer()], for_json=True, diagnose_errors=True)
        self.assertEqual(calls, ['outer', 'inner'])
        self.assertEqual(cm.exception.path, '$[0].inner')

    def test_path_tracks_entered_containers_only(self):
        # default() returns a container whose later member fails; the path
        # extends through the container actually entered, without the
        # encoder inventing a position for the object that produced it.
        class Wrapper(object):
            pass

        def default(obj):
            if isinstance(obj, Wrapper):
                return {'a': [1, _Unserializable()]}
            raise TypeError('not serializable')

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'top': Wrapper()}, default=default,
                       diagnose_errors=True)
        self.assertEqual(cm.exception.path, '$.top.a[1]')

    def test_asdict_returning_non_dict_is_diagnosed(self):
        class NotADict(object):
            def _asdict(self):
                return [1]

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'records': [{'x': NotADict()}]}, diagnose_errors=True)
        exc = cm.exception
        self.assertEqual(exc.path, '$.records[0].x')
        self.assertEqual(exc.kind, 'value')
        self.assertIn('must return a dict', str(exc))

    def test_output_is_not_atomic_before_first_chunk(self):
        failing = _ForJsonRaises(ValueError('boom'))
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps(failing, for_json=True, diagnose_errors=True)
        exc = cm.exception
        self.assertFalse(exc.complete)
        self.assertFalse(exc.prefix_emitted)

    def test_iterencode_prefix_is_marked_incomplete(self):
        failing = _ForJsonRaises(ValueError('boom'))
        chunks = []
        encoder = json.JSONEncoder(for_json=True, diagnose_errors=True)
        with self.assertRaises(json.JSONEncodeError) as cm:
            for chunk in encoder.iterencode([1, 2, failing]):
                chunks.append(chunk)
        exc = cm.exception
        self.assertEqual(''.join(chunks), '[1, 2, ')
        self.assertFalse(exc.complete)
        self.assertTrue(exc.prefix_emitted)
        # The truncated text is deliberately not valid JSON.
        self.assertRaises(ValueError, json.loads, ''.join(chunks))

    def test_encode_does_not_report_a_prefix(self):
        # encode() discards the partial list, so no truncated document
        # escaped to the caller even though iterencode had yielded chunks.
        failing = _ForJsonRaises(ValueError('boom'))
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps([1, 2, failing], for_json=True, diagnose_errors=True)
        exc = cm.exception
        self.assertFalse(exc.complete)
        self.assertFalse(exc.prefix_emitted)

    def test_disabled_by_default(self):
        with self.assertRaises(ValueError) as cm:
            json.dumps([_ForJsonRaises(ValueError('boom'))], for_json=True)
        self.assertNotIsInstance(cm.exception, json.JSONEncodeError)
        self.assertEqual(str(cm.exception), 'boom')

    def test_diagnose_errors_option_round_trips_through_encoder(self):
        encoder = json.JSONEncoder(diagnose_errors=True)
        self.assertTrue(encoder.diagnose_errors)
        self.assertFalse(json.JSONEncoder().diagnose_errors)

    def test_interpreter_exceptions_are_not_wrapped(self):
        for exc_type in (KeyboardInterrupt, SystemExit):
            failing = _ForJsonRaises(exc_type())
            with self.assertRaises(exc_type):
                json.dumps([failing], for_json=True, diagnose_errors=True)

    def test_c_speedups_are_bypassed_when_diagnosing(self):
        # The C encoder has no location tracking.  When the speedups are
        # loaded, diagnose_errors must take the Python path rather than
        # silently dropping the diagnostic.
        from simplejson import encoder as encoder_module
        original = encoder_module.c_make_encoder
        calls = []

        def fake_c_make_encoder(*args, **kwargs):
            calls.append(1)
            return lambda o, level: iter(['{}'])

        encoder_module.c_make_encoder = fake_c_make_encoder
        try:
            with self.assertRaises(json.JSONEncodeError) as cm:
                json.dumps([_Unserializable()], diagnose_errors=True)
            self.assertEqual(cm.exception.path, '$[0]')
            self.assertEqual(calls, [])
        finally:
            encoder_module.c_make_encoder = original

    def test_dump_reports_prefix_written_to_file(self):
        import io
        failing = _ForJsonRaises(ValueError('boom'))
        fp = io.StringIO()
        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dump([1, 2, failing], fp, for_json=True,
                      diagnose_errors=True)
        exc = cm.exception
        self.assertEqual(fp.getvalue(), '[1, 2, ')
        self.assertTrue(exc.prefix_emitted)
        self.assertFalse(exc.complete)

    def test_typeerror_still_means_method_not_supported(self):
        # A TypeError from for_json()/_asdict() means "not supported" and
        # must still fall through to default() even while diagnosing.
        calls = []

        class Raises(object):
            def for_json(self):
                calls.append('for_json')
                raise TypeError('nope')

            def _asdict(self):
                calls.append('_asdict')
                raise TypeError('nope')

        self.assertEqual(
            json.dumps([Raises()], for_json=True, default=lambda o: 'd',
                       diagnose_errors=True),
            '["d"]')
        self.assertEqual(calls, ['for_json', '_asdict'])

    def test_diagnostic_is_picklable_with_cause(self):
        import pickle
        error = ValueError('boom')

        def default(obj):
            raise error

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps({'a': _Unserializable()}, default=default,
                       diagnose_errors=True)
        exc = cm.exception
        copy = pickle.loads(pickle.dumps(exc))
        self.assertEqual(copy.path, exc.path)
        self.assertEqual(copy.path_parts, exc.path_parts)
        self.assertEqual(copy.kind, exc.kind)
        self.assertEqual(copy.value_type, exc.value_type)
        self.assertEqual(str(copy), str(exc))
        self.assertIsInstance(copy.__cause__, ValueError)
        self.assertEqual(str(copy.__cause__), 'boom')

    def test_nested_diagnostic_is_re_raised_verbatim(self):
        # JSONEncodeError is a TypeError subclass so old handlers keep
        # working, but a diagnostic escaping a for_json()/default() call
        # means "a nested encode failed", not "this protocol is
        # unsupported": it must not fall through to _asdict()/default().
        error = json.JSONEncodeError('user made', path_parts=('custom',))

        class RaisesDiagnostic(object):
            def for_json(self):
                raise error

            def _asdict(self):
                raise AssertionError('must not be called')

        with self.assertRaises(json.JSONEncodeError) as cm:
            json.dumps([RaisesDiagnostic()], for_json=True,
                       default=lambda o: 'not reached',
                       diagnose_errors=True)
        self.assertIs(cm.exception, error)
        self.assertEqual(cm.exception.path, '$.custom')

    def test_diagnostic_does_not_leak_markers(self):
        # A failed diagnostic encode must release its circular marker so a
        # reused encoder does not report a false cycle on the next call.
        encoder = json.JSONEncoder(for_json=True, diagnose_errors=True)
        for _ in range(2):
            self.assertRaises(
                json.JSONEncodeError, encoder.encode,
                [_ForJsonRaises(ValueError('boom'))])

