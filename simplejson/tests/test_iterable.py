import os
import subprocess
import sys
import unittest
from simplejson.compat import StringIO

import simplejson as json
from simplejson.tests._helpers import skip_if_speedups_missing

def iter_dumps(obj, **kw):
    return ''.join(json.JSONEncoder(**kw).iterencode(obj))

def sio_dump(obj, **kw):
    sio = StringIO()
    json.dumps(obj, **kw)
    return sio.getvalue()

class BadIter:
    """Object whose __iter__ raises a non-TypeError exception."""
    def __init__(self, exc_type):
        self.exc_type = exc_type
    def __iter__(self):
        raise self.exc_type("from __iter__")


class IterableValues(object):
    def __init__(self, values):
        self.values = values

    def __iter__(self):
        return iter(self.values)


class RecursiveIterator(object):
    def __iter__(self):
        return self

    def __next__(self):
        return self

    next = __next__


class FiniteRecursiveIterable(object):
    def __init__(self):
        self.remaining = 2

    def __iter__(self):
        if self.remaining:
            self.remaining -= 1
            return iter((self,))
        return iter(())


class FiniteRecursiveIterator(RecursiveIterator):
    def __init__(self):
        self.remaining = 2

    def __next__(self):
        if self.remaining:
            self.remaining -= 1
            return self
        raise StopIteration

    next = __next__


class CountingRecursiveIterable(object):
    def __init__(self):
        self.calls = 0

    def __iter__(self):
        self.calls += 1
        return iter([self])


class FalsySelfIterator(object):
    # Its truth value is False while it still has items to yield.
    def __init__(self):
        self.items = [1, 1]

    def __len__(self):
        return 0

    def __iter__(self):
        return self

    def __next__(self):
        if self.items:
            return self.items.pop()
        raise StopIteration

    next = __next__


class OneShotIterator(object):
    # An iterator whose __iter__ may run only once, like a custom
    # collection's draining iterator: the encoder must consume the
    # single iterator its probe built, not call __iter__ again.
    def __init__(self, values):
        self.values = list(values)
        self.calls = 0

    def __iter__(self):
        self.calls += 1
        if self.calls > 1:
            raise RuntimeError("__iter__ called more than once")
        return self

    def __next__(self):
        if self.values:
            return self.values.pop(0)
        raise StopIteration

    next = __next__


class MidIterationError(object):
    def __iter__(self):
        return self

    def __next__(self):
        raise RuntimeError("mid-iteration")

    next = __next__


class IterableForJson(object):
    def __init__(self):
        self.iter_calls = 0

    def for_json(self):
        return {'via': 'for_json'}

    def __iter__(self):
        self.iter_calls += 1
        return iter([1])


class IterableAsDict(object):
    def __init__(self):
        self.iter_calls = 0

    def _asdict(self):
        return {'via': 'asdict'}

    def __iter__(self):
        self.iter_calls += 1
        return iter([1])


# Every level is a new object, so circular checks cannot stop the
# encoder; only the C encoder's recursion guard does. On Python 3.14+ the
# interpreter's own stack checks also catch Deep, whose __iter__ is a
# Python call, so the nested frozensets are what crash the child there
# without the guard: with namedtuple_as_object=False the encoder walks
# them without calling into Python (otherwise each level's _asdict lookup
# raises AttributeError, which passes through the interpreter's stack
# check). Before 3.14 the guard's limit is a fixed count, so the encodes
# run in a thread with a large stack to let the guard, not the platform's
# stack size, decide the outcome.
UNBOUNDED_ITERABLE_SCRIPT = '''
import sys
import threading
sys.path.insert(0, sys.argv[1])
import simplejson
from simplejson import encoder
assert encoder.c_make_encoder is not None

class Deep(object):
    def __iter__(self):
        return iter([Deep()])

nested = frozenset()
for _ in range(100000):
    nested = frozenset([nested])

results = []

def run():
    try:
        for value, kw in ((Deep(), {}),
                          (nested, {'namedtuple_as_object': False})):
            for check_circular in (True, False):
                try:
                    simplejson.dumps(value, iterable_as_array=True,
                                     check_circular=check_circular, **kw)
                except RuntimeError:
                    pass
                else:
                    results.append('expected a recursion error')
        results.append('done')
    except BaseException as exc:
        results.append(repr(exc))

if sys.version_info < (3, 14):
    threading.stack_size(32 * 1024 * 1024)
thread = threading.Thread(target=run)
thread.start()
thread.join()
assert results == ['done'], results
'''


class TestIterable(unittest.TestCase):
    def test_iterable_cycles(self):
        direct = IterableValues([])
        direct.values.append(direct)
        first = IterableValues([])
        second = IterableValues([first])
        first.values.append(second)
        for value in (direct, first, [direct], {'value': direct}, RecursiveIterator()):
            for indent in (None, 2):
                with self.assertRaises(ValueError) as exc:
                    json.dumps(value, iterable_as_array=True, indent=indent)
                self.assertEqual(str(exc.exception), 'Circular reference detected')

    def test_iterable_repeated_reference_is_not_a_cycle(self):
        value = IterableValues([1, 2])
        for circular in (True, False):
            for indent in (None, 2):
                encoded = json.dumps([value, value], iterable_as_array=True,
                                     check_circular=circular, indent=indent)
                self.assertEqual(json.loads(encoded), [[1, 2], [1, 2]])

    def test_iterable_circular_checks_can_be_disabled(self):
        # Bounded self-references exercise disabled circular checks without
        # depending on a platform's recovery from stack exhaustion.
        for cycle_type in (FiniteRecursiveIterable, FiniteRecursiveIterator):
            for indent in (None, 2):
                self.assertRaises(ValueError, json.dumps, cycle_type(),
                                  iterable_as_array=True, indent=indent)
                encoded = json.dumps(cycle_type(), iterable_as_array=True,
                                     check_circular=False, indent=indent)
                self.assertEqual(json.loads(encoded), [[[]]])

    def test_iterable_cycle_does_not_iterate_again(self):
        for wrap in (lambda v: v, lambda v: [v], lambda v: {'v': v}):
            for indent in (None, 2):
                value = CountingRecursiveIterable()
                self.assertRaises(ValueError, json.dumps, wrap(value),
                                  iterable_as_array=True, indent=indent)
                self.assertEqual(value.calls, 1)

    def test_falsy_self_iterator_is_iterated(self):
        for indent in (None, 2):
            encoded = json.dumps(FalsySelfIterator(), iterable_as_array=True,
                                 indent=indent)
            self.assertEqual(json.loads(encoded), [1, 1])

    def test_one_shot_iterator_is_itered_once(self):
        # The probe's iterator is the one that gets encoded: __iter__
        # runs exactly once per source per encode, at every depth.
        for indent in (None, 2):
            value = OneShotIterator([1, 2, 3])
            encoded = json.dumps(value, iterable_as_array=True, indent=indent)
            self.assertEqual(json.loads(encoded), [1, 2, 3])
            self.assertEqual(value.calls, 1)
            value = OneShotIterator(
                [OneShotIterator([1]), OneShotIterator([])])
            encoded = json.dumps(value, iterable_as_array=True, indent=indent)
            self.assertEqual(json.loads(encoded), [[1], []])
            self.assertEqual(value.calls, 1)
            for wrap in (lambda v: [v], lambda v: {'v': v}):
                value = OneShotIterator([1])
                encoded = json.dumps(wrap(value), iterable_as_array=True,
                                     indent=indent)
                self.assertEqual(json.loads(encoded), json.loads(
                    json.dumps(wrap([1]), iterable_as_array=True,
                               indent=indent)))
                self.assertEqual(value.calls, 1)

    def test_one_shot_iterator_empty(self):
        for indent in (None, 2):
            value = OneShotIterator([])
            self.assertEqual(
                json.dumps(value, iterable_as_array=True, indent=indent), '[]')
            self.assertEqual(value.calls, 1)

    def test_iteration_error_does_not_leak_markers(self):
        for indent in (None, 2):
            encoder = json.JSONEncoder(iterable_as_array=True, indent=indent)
            self.assertRaises(RuntimeError, encoder.encode,
                              MidIterationError())
            # A subsequent independent encode with the same encoder
            # starts with a clean circular-reference state.
            self.assertEqual(json.loads(encoder.encode([1, 2])), [1, 2])
            self.assertEqual(
                json.loads(encoder.encode(OneShotIterator([4]))), [4])

    def test_rereadable_iterable_encodes_repeatedly(self):
        encoder = json.JSONEncoder(iterable_as_array=True)
        value = IterableValues([1, 2])
        for _ in range(2):
            self.assertEqual(json.loads(encoder.encode(value)), [1, 2])

    def test_iterable_with_encoding_protocol_prefers_the_protocol(self):
        for cls, expected, enable, disable in (
                (IterableForJson, {'via': 'for_json'},
                 {'for_json': True}, {'for_json': False}),
                (IterableAsDict, {'via': 'asdict'},
                 {'namedtuple_as_object': True},
                 {'namedtuple_as_object': False})):
            value = cls()
            encoded = json.dumps(value, iterable_as_array=True, **enable)
            self.assertEqual(json.loads(encoded), expected)
            self.assertEqual(value.iter_calls, 0)
            # With the protocol disabled the iterable path is used.
            value = cls()
            self.assertEqual(json.loads(json.dumps(
                value, iterable_as_array=True, **disable)), [1])
            self.assertEqual(value.iter_calls, 1)

    @skip_if_speedups_missing
    def test_unbounded_iterable_recursion_is_guarded(self):
        # Run in a child process so a regression crashes the child, not
        # the test suite.
        if sys.platform == 'emscripten':
            self.skipTest('subprocess not available on Emscripten')
        root = os.path.dirname(os.path.dirname(os.path.abspath(json.__file__)))
        proc = subprocess.Popen(
            [sys.executable, '-c', UNBOUNDED_ITERABLE_SCRIPT, root],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        out, err = proc.communicate()
        self.assertEqual(proc.returncode, 0, err)

    @unittest.skipIf(sys.version_info < (3, 11), 'add_note requires Python 3.11+')
    def test_add_note_names_the_iterable(self):
        with self.assertRaises(TypeError) as cm:
            json.dumps([IterableValues([object()])], iterable_as_array=True)
        self.assertEqual(cm.exception.__notes__, [
            'when serializing object object',
            'when serializing IterableValues item 0',
            'when serializing list item 0'])
        # A C iterable is named by type(o).__name__, as in the Python
        # encoder, not by its tp_name with the module prefix.
        import collections
        with self.assertRaises(TypeError) as cm:
            json.dumps(collections.deque([object()]), iterable_as_array=True)
        self.assertEqual(cm.exception.__notes__, [
            'when serializing object object',
            'when serializing deque item 0'])

    def test_iterable(self):
        for l in ([], [1], [1, 2], [1, 2, 3]):
            for opts in [{}, {'indent': 2}]:
                for dumps in (json.dumps, iter_dumps, sio_dump):
                    expect = dumps(l, **opts)
                    default_expect = dumps(sum(l), **opts)
                    # Default is False
                    self.assertRaises(TypeError, dumps, iter(l), **opts)
                    self.assertRaises(TypeError, dumps, iter(l), iterable_as_array=False, **opts)
                    self.assertEqual(expect, dumps(iter(l), iterable_as_array=True, **opts))
                    # Ensure that the "default" gets called
                    self.assertEqual(default_expect, dumps(iter(l), default=sum, **opts))
                    self.assertEqual(default_expect, dumps(iter(l), iterable_as_array=False, default=sum, **opts))
                    # Ensure that the "default" does not get called
                    self.assertEqual(
                        expect,
                        dumps(iter(l), iterable_as_array=True, default=sum, **opts))

    def test_iterable_as_array_propagates_non_typeerror(self):
        # Regression test: MemoryError from __iter__ must not be
        # swallowed by PyErr_Clear and replaced with TypeError.
        # Test against the C encoder directly since json.dumps may
        # use the Python fallback encoder.
        try:
            import simplejson._speedups as sp
            import decimal
        except ImportError:
            return  # C extension not available
        def noop_default(obj):
            raise TypeError('not serializable')
        c_enc = sp.make_encoder(
            {}, noop_default, sp.encode_basestring_ascii, None,
            ', ', ': ', False, False, True, {}, False, False,
            False, None, None, 'utf-8', False, False,
            decimal.Decimal, True,  # iterable_as_array=True
        )
        for exc_type in (MemoryError, RuntimeError):
            with self.assertRaises(exc_type):
                c_enc(BadIter(exc_type), 0)
