import sys
import unittest

import simplejson as json
from simplejson import JSONEncoder
from simplejson.encoder import DEFAULT_PROTOCOL_PRIORITY
from simplejson.tests._helpers import skip_if_speedups_missing


class AllProtocols(object):
    """Implements for_json, _asdict and __iter__ at once, recording calls."""
    def __init__(self):
        self.calls = []

    def for_json(self):
        self.calls.append('for_json')
        return {'protocol': 'for_json'}

    def _asdict(self):
        self.calls.append('asdict')
        return {'protocol': 'asdict'}

    def __iter__(self):
        self.calls.append('iterable')
        return iter(['iterable'])


class SelfForJson(object):
    def for_json(self):
        return self


class TestProtocolPriority(unittest.TestCase):
    def encode(self, obj, **kw):
        kw.setdefault('for_json', True)
        kw.setdefault('iterable_as_array', True)
        return json.dumps(obj, **kw)

    def test_default_priority_is_inspectable(self):
        self.assertEqual(JSONEncoder().protocol_priority,
                         ('for_json', 'asdict', 'iterable'))
        self.assertEqual(JSONEncoder().protocol_priority,
                         DEFAULT_PROTOCOL_PRIORITY)
        enc = JSONEncoder(for_json=True, iterable_as_array=True,
                          protocol_priority=['iterable', 'for_json'])
        self.assertEqual(enc.protocol_priority, ('iterable', 'for_json'))

    def test_default_order_is_preserved(self):
        obj = AllProtocols()
        self.assertEqual(self.encode(obj), '{"protocol": "for_json"}')
        self.assertEqual(obj.calls, ['for_json'])

    def test_configured_order_selects_the_first_applicable_protocol(self):
        obj = AllProtocols()
        encoded = self.encode(obj, protocol_priority=(
            'iterable', 'asdict', 'for_json'))
        self.assertEqual(encoded, '["iterable"]')
        self.assertEqual(obj.calls, ['iterable'])

        obj = AllProtocols()
        encoded = self.encode(obj, protocol_priority=('asdict', 'for_json'))
        self.assertEqual(encoded, '{"protocol": "asdict"}')
        self.assertEqual(obj.calls, ['asdict'])

    def test_only_the_selected_protocol_is_called(self):
        # A protocol conversion that returns the original object raises a
        # circular error instead of falling through to other protocols.
        class SelfAsDictAndForJson(object):
            def __init__(self):
                self.calls = []
            def for_json(self):
                self.calls.append('for_json')
                return self
            def _asdict(self):
                self.calls.append('asdict')
                return {'should': 'not be reached'}
        obj = SelfAsDictAndForJson()
        self.assertRaises(ValueError, self.encode, obj,
                          protocol_priority=('for_json', 'asdict'))
        self.assertEqual(obj.calls, ['for_json'])

    def test_protocol_returning_self_is_circular(self):
        for check_circular in (True, False):
            with self.assertRaises(ValueError) as exc:
                self.encode(SelfForJson(), check_circular=check_circular)
            self.assertEqual(str(exc.exception),
                             'Circular reference detected')

    def test_protocol_conversion_cycle_is_circular(self):
        first, second = SelfForJson(), SelfForJson()
        first.for_json = lambda: second
        second.for_json = lambda: first
        for value in (first, [first], {'k': first}):
            with self.assertRaises(ValueError) as exc:
                self.encode(value)
            self.assertEqual(str(exc.exception),
                             'Circular reference detected')

    @unittest.skipIf(sys.version_info < (3, 11),
                     'add_note requires Python 3.11+')
    def test_circular_error_carries_the_path(self):
        with self.assertRaises(ValueError) as cm:
            self.encode({'k': [SelfForJson()]})
        self.assertEqual(cm.exception.__notes__, [
            'when serializing SelfForJson object',
            'when serializing list item 0',
            "when serializing dict item 'k'"])

    def test_iterable_priority_does_not_iterate_native_containers(self):
        import collections
        Point = collections.namedtuple('Point', 'x y')
        # tuples keep their native array encoding when 'asdict' is not
        # listed, and dicts are never encoded as key arrays.
        self.assertEqual(
            self.encode(Point(1, 2), protocol_priority=('iterable',)),
            '[1, 2]')
        self.assertEqual(
            self.encode({'a': 1}, protocol_priority=('iterable',)),
            '{"a": 1}')
        # ... but 'asdict' still applies when listed after 'iterable'.
        self.assertEqual(
            json.loads(self.encode(
                Point(1, 2), protocol_priority=('iterable', 'asdict'))),
            {'x': 1, 'y': 2})

    def test_tuple_iterated_when_tuple_as_array_is_off(self):
        # With tuple_as_array off, tuples still fall back to the
        # iterable protocol, as before protocol_priority existed.
        self.assertEqual(
            json.dumps((1, 2), tuple_as_array=False,
                       iterable_as_array=True),
            '[1, 2]')
        self.assertEqual(
            json.dumps((1, 2), tuple_as_array=False,
                       iterable_as_array=True,
                       protocol_priority=('iterable',)),
            '[1, 2]')

    def test_empty_priority_disables_all_protocols(self):
        enc = JSONEncoder(for_json=True, protocol_priority=())
        self.assertEqual(enc.protocol_priority, ())
        self.assertRaises(TypeError, enc.encode, AllProtocols())

    def test_conflicting_configuration_is_rejected_before_encoding(self):
        # A listed protocol whose enabling option is off is contradictory.
        with self.assertRaises(ValueError):
            JSONEncoder(protocol_priority=('for_json',))
        with self.assertRaises(ValueError):
            JSONEncoder(for_json=True,
                        protocol_priority=('for_json', 'iterable'))
        with self.assertRaises(ValueError):
            JSONEncoder(namedtuple_as_object=False,
                        protocol_priority=('asdict',))
        # dumps()/dump() raise before any output is produced.
        with self.assertRaises(ValueError):
            json.dumps(object(), protocol_priority=('iterable',))
        from simplejson.compat import StringIO
        sio = StringIO()
        with self.assertRaises(ValueError):
            json.dump(object(), sio, protocol_priority=('for_json',))
        self.assertEqual(sio.getvalue(), '')

    def test_malformed_priority_is_rejected_before_encoding(self):
        with self.assertRaises(TypeError):
            JSONEncoder(protocol_priority='for_json')
        with self.assertRaises(TypeError):
            JSONEncoder(protocol_priority=42)
        with self.assertRaises(ValueError):
            JSONEncoder(for_json=True,
                        protocol_priority=('for_json', 'for_json'))
        with self.assertRaises(ValueError):
            JSONEncoder(protocol_priority=('bogus',))

    def test_dump_and_iterencode_accept_protocol_priority(self):
        from simplejson.compat import StringIO
        obj = AllProtocols()
        sio = StringIO()
        json.dump(obj, sio, for_json=True, iterable_as_array=True,
                  protocol_priority=('iterable', 'for_json'))
        self.assertEqual(sio.getvalue(), '["iterable"]')
        chunks = list(JSONEncoder(
            for_json=True, iterable_as_array=True,
            protocol_priority=('asdict', 'for_json')).iterencode(obj))
        self.assertEqual(''.join(chunks), '{"protocol": "asdict"}')

    @skip_if_speedups_missing
    def test_non_default_priority_uses_the_python_encoder(self):
        # The C encoder only implements the default protocol order; a
        # configured priority must take the compatible pure-Python path
        # rather than silently encode with the C order.
        from simplejson import encoder
        calls = []
        real_make_encoder = encoder.c_make_encoder
        def spy(*args):
            calls.append(args)
            return real_make_encoder(*args)
        encoder.c_make_encoder = spy
        try:
            self.encode(AllProtocols(),
                        protocol_priority=('iterable', 'asdict', 'for_json'))
            self.assertEqual(calls, [])
            # The default order, spelled out explicitly, still matches
            # the C encoder's order and may use it.
            self.encode(AllProtocols(),
                        protocol_priority=DEFAULT_PROTOCOL_PRIORITY)
            self.assertEqual(len(calls), 1)
            self.encode(AllProtocols())
            self.assertEqual(len(calls), 2)
        finally:
            encoder.c_make_encoder = real_make_encoder


if __name__ == '__main__':
    unittest.main()