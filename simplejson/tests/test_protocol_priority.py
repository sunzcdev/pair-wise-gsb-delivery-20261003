"""Tests for configurable conversion-protocol priority.

An object may implement several of ``for_json()``, ``_asdict()`` and the
iterable protocol (``__iter__``). These tests pin down:

* the configurable, inspectable ``protocol_priority`` and the conflicts
  rejected while the encoder is constructed (before anything is encoded);
* that one conversion invokes only the selected protocol and never retries
  a lower-priority one;
* that self-returning and cyclic conversions fail with a path-annotated
  ``ValueError`` instead of looping or falling back to another protocol;
* that a priority order the C speedups cannot express takes the pure
  Python compatibility path, so both backends never diverge silently.
"""
from __future__ import absolute_import

import unittest

import simplejson as json
from simplejson import encoder
from simplejson.encoder import (
    DEFAULT_PROTOCOL_PRIORITY,
    PROTOCOL_ASDICT,
    PROTOCOL_FOR_JSON,
    PROTOCOL_ITERABLE,
)
from simplejson.tests._helpers import has_speedups


class AllProtocols(object):
    """Implements all three conversion protocols and records each call."""

    def __init__(self):
        self.calls = []

    def for_json(self):
        self.calls.append(PROTOCOL_FOR_JSON)
        return {'via': PROTOCOL_FOR_JSON}

    def _asdict(self):
        self.calls.append(PROTOCOL_ASDICT)
        return {'via': PROTOCOL_ASDICT}

    def __iter__(self):
        self.calls.append(PROTOCOL_ITERABLE)
        return iter([PROTOCOL_ITERABLE])


class OnlyIterable(object):
    def __init__(self, values):
        self.values = values

    def __iter__(self):
        return iter(self.values)


class TestProtocolPriorityConfig(unittest.TestCase):
    def test_default_priority_constant(self):
        self.assertEqual(
            DEFAULT_PROTOCOL_PRIORITY,
            (PROTOCOL_FOR_JSON, PROTOCOL_ASDICT, PROTOCOL_ITERABLE))

    def test_default_encoder_attribute_reflects_enabled_protocols(self):
        # Default switches: for_json and iterable disabled -> only _asdict.
        enc = json.JSONEncoder()
        self.assertEqual(enc.protocol_priority, (PROTOCOL_ASDICT,))
        enc = json.JSONEncoder(for_json=True, iterable_as_array=True)
        self.assertEqual(
            enc.protocol_priority,
            (PROTOCOL_FOR_JSON, PROTOCOL_ASDICT, PROTOCOL_ITERABLE))
        enc = json.JSONEncoder(iterable_as_array=True)
        self.assertEqual(enc.protocol_priority,
                         (PROTOCOL_ASDICT, PROTOCOL_ITERABLE))
        enc = json.JSONEncoder(namedtuple_as_object=False,
                               iterable_as_array=True)
        self.assertEqual(enc.protocol_priority, (PROTOCOL_ITERABLE,))
        enc = json.JSONEncoder(for_json=True, namedtuple_as_object=False)
        self.assertEqual(enc.protocol_priority, (PROTOCOL_FOR_JSON,))

    def test_explicit_priority_is_stored_normalized_as_tuple(self):
        enc = json.JSONEncoder(
            for_json=True, iterable_as_array=True,
            protocol_priority=[PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                               PROTOCOL_FOR_JSON])
        self.assertEqual(
            enc.protocol_priority,
            (PROTOCOL_ITERABLE, PROTOCOL_ASDICT, PROTOCOL_FOR_JSON))

    def test_conflicts_are_rejected_before_encoding(self):
        # Listing a protocol whose enabling switch is off.
        with self.assertRaises(ValueError):
            json.JSONEncoder(protocol_priority=(PROTOCOL_FOR_JSON,
                                                PROTOCOL_ASDICT))
        # Omitting an enabled protocol.
        with self.assertRaises(ValueError):
            json.JSONEncoder(for_json=True,
                             protocol_priority=(PROTOCOL_ASDICT,))
        # Enabled set is for_json+_asdict but priority names only for_json.
        with self.assertRaises(ValueError):
            json.JSONEncoder(for_json=True,
                             protocol_priority=(PROTOCOL_FOR_JSON,))
        # Iterable enabled but missing from the priority.
        with self.assertRaises(ValueError):
            json.JSONEncoder(iterable_as_array=True,
                             protocol_priority=(PROTOCOL_ASDICT,))

    def test_malformed_priority_is_rejected(self):
        with self.assertRaises(ValueError):
            json.JSONEncoder(protocol_priority=('for_json', 'nope'))
        with self.assertRaises(ValueError):
            json.JSONEncoder(for_json=True,
                             protocol_priority=('for_json', 'for_json'))
        with self.assertRaises(TypeError):
            json.JSONEncoder(protocol_priority='for_json')
        with self.assertRaises(TypeError):
            json.JSONEncoder(protocol_priority=42)
        with self.assertRaises(TypeError):
            json.JSONEncoder(protocol_priority=('for_json', 7))

    def test_dumps_rejects_conflict_without_touching_the_object(self):
        obj = AllProtocols()
        with self.assertRaises(ValueError):
            json.dumps(obj, protocol_priority=(PROTOCOL_FOR_JSON,
                                               PROTOCOL_ASDICT))
        # Construction failed, so no protocol method ever ran.
        self.assertEqual(obj.calls, [])

    def test_partial_priority_must_match_enabled_set(self):
        # Two enabled protocols -> priority must name exactly those two.
        enc = json.JSONEncoder(
            for_json=True, namedtuple_as_object=False,
            iterable_as_array=True,
            protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_FOR_JSON))
        self.assertEqual(enc.protocol_priority,
                         (PROTOCOL_ITERABLE, PROTOCOL_FOR_JSON))
        # Naming the disabled _asdict is a conflict.
        with self.assertRaises(ValueError):
            json.JSONEncoder(
                for_json=True, namedtuple_as_object=False,
                iterable_as_array=True,
                protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_FOR_JSON,
                                   PROTOCOL_ASDICT))


class TestProtocolSelection(unittest.TestCase):
    def _all_enabled_kwargs(self):
        return dict(for_json=True, iterable_as_array=True)

    def test_default_order_prefers_for_json(self):
        obj = AllProtocols()
        result = json.loads(json.dumps(obj, **self._all_enabled_kwargs()))
        self.assertEqual(result, {'via': PROTOCOL_FOR_JSON})
        self.assertEqual(obj.calls, [PROTOCOL_FOR_JSON])

    def test_iterable_can_outrank_the_methods(self):
        obj = AllProtocols()
        result = json.loads(json.dumps(
            obj, protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                                    PROTOCOL_FOR_JSON),
            **self._all_enabled_kwargs()))
        self.assertEqual(result, [PROTOCOL_ITERABLE])
        self.assertEqual(obj.calls, [PROTOCOL_ITERABLE])

    def test_asdict_can_outrank_for_json(self):
        obj = AllProtocols()
        result = json.loads(json.dumps(
            obj,
            protocol_priority=(PROTOCOL_ASDICT, PROTOCOL_FOR_JSON,
                               PROTOCOL_ITERABLE),
            **self._all_enabled_kwargs()))
        self.assertEqual(result, {'via': PROTOCOL_ASDICT})
        self.assertEqual(obj.calls, [PROTOCOL_ASDICT])

    def test_only_selected_protocol_runs_inside_containers(self):
        for wrap in (lambda o: o, lambda o: [o], lambda o: {'k': o}):
            obj = AllProtocols()
            json.loads(json.dumps(
                wrap(obj),
                protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                                   PROTOCOL_FOR_JSON),
                **self._all_enabled_kwargs()))
            self.assertEqual(obj.calls, [PROTOCOL_ITERABLE], wrap)

    def test_unavailable_top_protocol_falls_to_next_without_retry(self):
        # An object that is only iterable: method protocols are probed and
        # skipped, the iterable protocol is selected once.
        values = OnlyIterable([1, 2, 3])
        self.assertEqual(
            json.loads(json.dumps(values, iterable_as_array=True)),
            [1, 2, 3])

    def test_method_raising_type_error_is_not_retried_via_another_path(self):
        # Mirrors test_for_json's TypeError-means-unsupported contract.
        calls = []

        class Raises(object):
            def for_json(self):
                calls.append(PROTOCOL_FOR_JSON)
                raise TypeError('nope')

            def _asdict(self):
                calls.append(PROTOCOL_ASDICT)
                raise TypeError('nope')

        for value, expected in ((Raises(), '"d"'),
                                ([Raises()], '["d"]'),
                                ({'k': Raises()}, '{"k": "d"}')):
            del calls[:]
            self.assertEqual(
                json.dumps(value, for_json=True, default=lambda o: 'd'),
                expected)
            self.assertEqual(calls, [PROTOCOL_FOR_JSON, PROTOCOL_ASDICT])

    def test_iterable_priority_does_not_override_native_containers(self):
        # Even with iterable ranked first, a plain list/dict keeps its
        # native container encoding (the iterable protocol ranks behind the
        # container checks in the historical dispatch order).
        self.assertEqual(
            json.loads(json.dumps(
                [1, 2],
                protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                                   PROTOCOL_FOR_JSON),
                **self._all_enabled_kwargs())),
            [1, 2])
        self.assertEqual(
            json.loads(json.dumps(
                {'a': 1},
                protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                                   PROTOCOL_FOR_JSON),
                **self._all_enabled_kwargs())),
            {'a': 1})


class TestProtocolCycles(unittest.TestCase):
    def assert_circular(self, value, **kwargs):
        with self.assertRaises(ValueError) as exc:
            json.dumps(value, **kwargs)
        self.assertEqual(str(exc.exception), 'Circular reference detected')
        return exc.exception

    def test_for_json_returning_self_calls_method_once(self):
        class Self(object):
            def __init__(self):
                self.n = 0

            def for_json(self):
                self.n += 1
                return self

        obj = Self()
        self.assert_circular(obj, for_json=True)
        self.assertEqual(obj.n, 1)

    def test_asdict_self_reference_calls_method_once(self):
        class AsSelf(object):
            def __init__(self):
                self.n = 0

            def _asdict(self):
                self.n += 1
                return {'self': self}

        obj = AsSelf()
        self.assert_circular(obj)
        self.assertEqual(obj.n, 1)

    def test_indirect_for_json_cycle_each_protocol_called_once(self):
        class Node(object):
            def __init__(self):
                self.n = 0
                self.other = None

            def for_json(self):
                self.n += 1
                return self.other

        a, b = Node(), Node()
        a.other, b.other = b, a
        self.assert_circular(a, for_json=True)
        self.assertEqual((a.n, b.n), (1, 1))

    def test_iterable_self_cycle_iterates_once(self):
        class Counting(object):
            def __init__(self):
                self.calls = 0

            def __iter__(self):
                self.calls += 1
                return iter([self])

        for wrap in (lambda o: o, lambda o: [o], lambda o: {'v': o}):
            obj = Counting()
            self.assert_circular(wrap(obj), iterable_as_array=True)
            self.assertEqual(obj.calls, 1, wrap)

    def test_cycle_keeps_other_protocols_untried(self):
        # When the selected (highest priority) protocol cycles, a lower
        # priority protocol must not be attempted as a fallback.
        class CycledForJson(object):
            def __init__(self):
                self.calls = []

            def for_json(self):
                self.calls.append(PROTOCOL_FOR_JSON)
                return self

            def _asdict(self):
                self.calls.append(PROTOCOL_ASDICT)
                return {}

            def __iter__(self):
                self.calls.append(PROTOCOL_ITERABLE)
                return iter(())

        obj = CycledForJson()
        self.assert_circular(obj, for_json=True, iterable_as_array=True)
        self.assertEqual(obj.calls, [PROTOCOL_FOR_JSON])

    def test_cycle_error_is_path_annotated_inside_containers(self):
        class Self(object):
            def for_json(self):
                return self

        with self.assertRaises(ValueError) as exc:
            json.dumps({'items': [Self()]}, for_json=True)
        self.assertEqual(str(exc.exception), 'Circular reference detected')
        if hasattr(exc.exception, '__notes__'):
            joined = '\n'.join(exc.exception.__notes__)
            self.assertIn("when serializing list item 0", joined)
            self.assertIn("when serializing dict item 'items'", joined)

    def test_repeated_reference_is_not_a_cycle(self):
        class Node(object):
            def for_json(self):
                return {'x': 1}

        value = Node()
        self.assertEqual(
            json.loads(json.dumps([value, value], for_json=True)),
            [{'x': 1}, {'x': 1}])

    def test_circular_checks_can_be_disabled(self):
        class Self(object):
            def for_json(self):
                return self

        with self.assertRaises(RuntimeError):
            json.dumps(Self(), for_json=True, check_circular=False)


class TestSpeedupsCompatibility(unittest.TestCase):
    def test_default_priority_is_marked_speedups_supported(self):
        enc = json.JSONEncoder(for_json=True, iterable_as_array=True)
        self.assertTrue(enc._protocol_priority_supported_by_speedups())
        # Restricted enabled sets that keep the default relative order are
        # supported too.
        self.assertTrue(
            json.JSONEncoder(iterable_as_array=True)
            ._protocol_priority_supported_by_speedups())
        self.assertTrue(
            json.JSONEncoder(for_json=True)
            ._protocol_priority_supported_by_speedups())
        self.assertTrue(
            json.JSONEncoder()
            ._protocol_priority_supported_by_speedups())

    def test_non_default_priority_is_marked_unsupported(self):
        enc = json.JSONEncoder(
            for_json=True, iterable_as_array=True,
            protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                               PROTOCOL_FOR_JSON))
        self.assertFalse(enc._protocol_priority_supported_by_speedups())
        enc = json.JSONEncoder(
            for_json=True,
            protocol_priority=(PROTOCOL_ASDICT, PROTOCOL_FOR_JSON))
        self.assertFalse(enc._protocol_priority_supported_by_speedups())

    def test_unsupported_priority_takes_pure_python_compatibility_path(self):
        # Even though an unsupported order cannot use the C encoder, the
        # observable encoding must follow the configured order. This guards
        # against a future change that silently selected the C encoder (and
        # its hard-wired order) for an unsupported configuration.
        enc = json.JSONEncoder(
            for_json=True, iterable_as_array=True,
            protocol_priority=(PROTOCOL_ITERABLE, PROTOCOL_ASDICT,
                               PROTOCOL_FOR_JSON))
        obj = AllProtocols()
        self.assertEqual(
            json.loads(''.join(enc.iterencode(obj))),
            [PROTOCOL_ITERABLE])
        self.assertEqual(obj.calls, [PROTOCOL_ITERABLE])

    @unittest.skipUnless(has_speedups(), 'C extension not available')
    def test_default_priority_still_uses_c_encoder(self):
        enc = json.JSONEncoder(for_json=True, iterable_as_array=True)
        self.assertIsNotNone(encoder.c_make_encoder)
        obj = AllProtocols()
        self.assertEqual(
            json.loads(''.join(enc.iterencode(obj))),
            {'via': PROTOCOL_FOR_JSON})
        self.assertEqual(obj.calls, [PROTOCOL_FOR_JSON])

    def test_default_priority_selects_for_json_on_either_backend(self):
        # Regardless of which backend is active, the default order must
        # select for_json over _asdict over the iterable protocol.
        obj = AllProtocols()
        enc = json.JSONEncoder(for_json=True, iterable_as_array=True)
        self.assertEqual(
            json.loads(''.join(enc.iterencode(obj))),
            {'via': PROTOCOL_FOR_JSON})
        self.assertEqual(obj.calls, [PROTOCOL_FOR_JSON])
