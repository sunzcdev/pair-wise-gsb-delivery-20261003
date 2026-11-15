import contextlib
import logging
import os
import threading
import unittest

from huey.api import chord
from huey.api import MemoryHuey
from huey.consumer import Consumer
from huey.exceptions import TaskException


class NullHandler(logging.Handler):
    def emit(self, record): pass


logger = logging.getLogger('huey')
logger.addHandler(NullHandler())

SLOW_TESTS = bool(os.environ.get('HUEY_SLOW_TESTS'))
CI = bool(os.environ.get('HUEY_CI'))


class BaseTestCase(unittest.TestCase):
    consumer_class = Consumer

    def setUp(self):
        super(BaseTestCase, self).setUp()
        self.huey = self.get_huey()

    def get_huey(self):
        return MemoryHuey(utc=False)

    def execute_next(self, timestamp=None):
        task = self.huey.dequeue()
        self.assertTrue(task is not None)
        return self.huey.execute(task, timestamp=timestamp)

    def trap_exception(self, fn, exc_type=TaskException):
        try:
            fn()
        except exc_type as exc_val:
            return exc_val
        raise AssertionError('trap_exception() failed to catch %s' % exc_type)

    def consumer(self, **params):
        params.setdefault('initial_delay', 0.001)
        params.setdefault('max_delay', 0.001)
        params.setdefault('workers', 2)
        params.setdefault('check_worker_health', False)
        return self.consumer_class(self.huey, **params)

    @contextlib.contextmanager
    def consumer_context(self, **kwargs):
        consumer = self.consumer(**kwargs)
        consumer.start()
        try:
            yield
        finally:
            consumer.stop(graceful=True)


def slow_test():
    def decorator(method):
        return unittest.skipUnless(SLOW_TESTS, 'skipping slow test')(method)
    return decorator


class ChordBarrierTests(object):
    """
    Deterministic interleaved-execution tests for the chord completion
    barrier. This mixin is combined with a TestCase whose storage backend is
    configured via ``get_huey()``, so the same interleavings are verified
    against every backend (memory and sqlite, at minimum).

    The invariants under test:

    * the callback only fires once every *distinct* member is terminal,
    * a duplicate notification never advances the barrier,
    * the callback is claimed exactly once when consumers race on the last
      members,
    * results are ordered by the member's original index, and
    * no chord bookkeeping (member slots or counters) lingers afterwards.

    Subclasses must override ``chord_counter_count()`` and may override the
    ``_setup_chord_huey()`` / ``_teardown_chord_huey()`` hooks for cleanup.
    """
    def setUp(self):
        super(ChordBarrierTests, self).setUp()
        self._setup_chord_huey()

    def tearDown(self):
        self._teardown_chord_huey()
        super(ChordBarrierTests, self).tearDown()

    def _setup_chord_huey(self):
        pass

    def _teardown_chord_huey(self):
        pass

    def chord_counter_count(self, huey):
        raise NotImplementedError

    def make_chord_tasks(self, huey):
        @huey.task()
        def ident(v):
            return v

        @huey.task()
        def agg(vs):
            return vs

        return ident, agg

    def chord_keys(self, huey):
        return sorted(k for k in huey.all_results() if k.startswith('chord:'))

    def test_barrier_waits_for_every_distinct_member(self):
        huey = self.huey
        ident, agg = self.make_chord_tasks(huey)

        r = huey.enqueue(chord(
            [ident.s('a'), ident.s('b'), ident.s('c')], agg))
        tasks = [huey.dequeue() for _ in range(3)]

        # Member 1 reports twice; the second report is a duplicate.
        self.assertEqual(huey.execute(tasks[1]), 'b')
        cc = tasks[1].chord_config
        self.assertFalse(huey._check_chord(cc, 'b'))
        self.assertEqual(huey.pending_count(), 0)

        # Member 2 reports; still waiting on member 0.
        self.assertEqual(huey.execute(tasks[2]), 'c')
        self.assertEqual(huey.pending_count(), 0)

        # Member 0 is the last distinct member: now the callback is enqueued.
        self.assertEqual(huey.execute(tasks[0]), 'a')
        self.assertEqual(huey.pending_count(), 1)

        # Late duplicates cannot enqueue a second callback.
        self.assertFalse(huey._check_chord(cc, 'b'))
        self.assertFalse(huey._check_chord(cc, 'c'))
        self.assertEqual(huey.pending_count(), 1)

        self.assertEqual(huey.execute(huey.dequeue()), ['a', 'b', 'c'])
        self.assertEqual(r(), ['a', 'b', 'c'])
        self.assertEqual(r.results(), ['a', 'b', 'c'])

    def test_member_reported_before_others_does_not_fire(self):
        # Reporting the *last* member first must not fire the callback: it
        # waits for all distinct members.
        huey = self.huey
        ident, agg = self.make_chord_tasks(huey)

        r = huey.enqueue(chord([ident.s(0), ident.s(1), ident.s(2)], agg))
        tasks = [huey.dequeue() for _ in range(3)]

        self.assertEqual(huey.execute(tasks[2]), 2)
        self.assertEqual(huey.pending_count(), 0)
        self.assertEqual(huey.execute(tasks[1]), 1)
        self.assertEqual(huey.pending_count(), 0)

        # A duplicate of an already-recorded member must not complete it.
        cc = tasks[2].chord_config
        self.assertFalse(huey._check_chord(cc, 2))
        self.assertEqual(huey.pending_count(), 0)

        self.assertEqual(huey.execute(tasks[0]), 0)
        self.assertEqual(huey.pending_count(), 1)
        self.assertEqual(huey.execute(huey.dequeue()), [0, 1, 2])
        self.assertEqual(r(), [0, 1, 2])

    def test_results_ordered_by_member_index(self):
        huey = self.huey
        ident, agg = self.make_chord_tasks(huey)

        r = huey.enqueue(chord(
            [ident.s('a'), ident.s('b'), ident.s('c'), ident.s('d')], agg))
        tasks = [huey.dequeue() for _ in range(4)]

        # Complete out of order, replaying each member's notification once.
        for task in (tasks[2], tasks[0], tasks[3], tasks[1]):
            huey.execute(task)
            huey._check_chord(task.chord_config, task.args[0])

        self.assertEqual(huey.pending_count(), 1)
        self.assertEqual(huey.execute(huey.dequeue()), ['a', 'b', 'c', 'd'])
        self.assertEqual(r(), ['a', 'b', 'c', 'd'])
        self.assertEqual(r.results(), ['a', 'b', 'c', 'd'])

    def test_concurrent_final_members_claim_callback_once(self):
        # The last two members report from separate threads simultaneously.
        # Exactly one callback may be enqueued, and it must contain both
        # results in member order.
        huey = self.huey
        ident, agg = self.make_chord_tasks(huey)

        r = huey.enqueue(chord([ident.s('a'), ident.s('b')], agg))
        task_a, task_b = [huey.dequeue() for _ in range(2)]
        cc_a, cc_b = task_a.chord_config, task_b.chord_config

        barrier = threading.Barrier(2)
        outcomes = []
        errors = []

        def report(cc, value):
            try:
                barrier.wait()
                # Report the member twice from this consumer.
                outcomes.append(huey._check_chord(cc, value))
                huey._check_chord(cc, value)
            except Exception as exc:  # pragma: no cover
                errors.append(exc)

        threads = [threading.Thread(target=report, args=(cc_a, 'a')),
                   threading.Thread(target=report, args=(cc_b, 'b'))]
        for t in threads: t.start()
        for t in threads: t.join()

        self.assertEqual(errors, [])
        # Exactly one consumer observed the barrier; the other observed a
        # duplicate or non-final report.
        self.assertEqual(sorted(bool(o) for o in outcomes), [False, True])
        self.assertEqual(huey.pending_count(), 1)
        self.assertEqual(huey.execute(huey.dequeue()), ['a', 'b'])
        self.assertEqual(r(), ['a', 'b'])

    def test_many_duplicate_notifications_enqueue_callback_once(self):
        # Once complete, replaying every notification from many threads must
        # not re-fire the callback.
        huey = self.huey
        ident, agg = self.make_chord_tasks(huey)

        r = huey.enqueue(chord([ident.s('a'), ident.s('b'), ident.s('c')],
                               agg))
        tasks = [huey.dequeue() for _ in range(3)]
        for task in tasks:
            huey.execute(task)

        self.assertEqual(huey.pending_count(), 1)
        cc = tasks[0].chord_config
        barrier = threading.Barrier(8)
        errors = []

        def replay(idx):
            try:
                barrier.wait()
                for _ in range(5):
                    huey._check_chord(cc, tasks[idx].args[0])
            except Exception as exc:  # pragma: no cover
                errors.append(exc)

        threads = [threading.Thread(target=replay, args=(i % 3,))
                   for i in range(8)]
        for t in threads: t.start()
        for t in threads: t.join()

        self.assertEqual(errors, [])
        self.assertEqual(huey.pending_count(), 1)  # Exactly one callback.
        self.assertEqual(huey.execute(huey.dequeue()), ['a', 'b', 'c'])
        self.assertEqual(r(), ['a', 'b', 'c'])

    def test_bookkeeping_cleared(self):
        huey = self.huey
        ident, agg = self.make_chord_tasks(huey)

        r = huey.enqueue(chord([ident.s(1), ident.s(2)], agg))
        for _ in range(3):
            huey.execute(huey.dequeue())

        self.assertEqual(r(), [1, 2])
        self.assertEqual(r.results(), [1, 2])
        self.assertEqual(self.chord_keys(huey), [])
        self.assertEqual(self.chord_counter_count(huey), 0)
