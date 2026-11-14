"""
Tests for the optional in-flight deduplication key (dedupe_key).

A task submitted with a dedupe_key while an identical submission (same key and
same parameter fingerprint) is still in-flight is coalesced into the existing
task: the caller receives a result handle bound to the existing task id and no
duplicate message is enqueued. The binding lives for the whole life-cycle of
the task - queued, scheduled (eta/delay) and retrying - and is only released
once the task reaches a terminal state. Submissions sharing the key but not the
fingerprint raise DedupeConflict. Serialization/enqueue failures and terminal
states must never leave a permanent placeholder behind.
"""
import datetime
import threading
import time

from huey.api import group
from huey.api import MemoryHuey
from huey.api import Result
from huey.exceptions import CancelExecution
from huey.exceptions import DedupeConflict
from huey.exceptions import RetryTask
from huey.tests.base import BaseTestCase


class TestDedupe(BaseTestCase):
    def get_huey(self):
        return MemoryHuey(utc=False)

    # ------------------------------------------------------------------ basic
    def test_dedupe_returns_existing_handle(self):
        @self.huey.task()
        def task_a(n):
            return n + 1

        r1 = task_a(1, dedupe_key='k1')
        self.assertIsInstance(r1, Result)
        self.assertEqual(self.huey.pending_count(), 1)

        # Re-submit the same partition before the first run finished.
        r2 = task_a(1, dedupe_key='k1')
        self.assertIsInstance(r2, Result)
        self.assertEqual(r2.id, r1.id)  # Handle points at existing task.
        self.assertEqual(self.huey.pending_count(), 1)  # No duplicate enqueue.

        task = self.huey.dequeue()
        self.assertEqual(task.id, r1.id)
        self.assertEqual(self.huey.execute(task), 2)
        # The result store is pop-style: both handles point at the same task
        # id, and reading through either one returns the produced value once.
        self.assertEqual(r2.id, r1.id)
        self.assertEqual(r2(), 2)

    def test_no_dedupe_key_unchanged(self):
        # Plain enqueues without a key behave exactly as before.
        @self.huey.task()
        def task_a(n):
            return n

        r1 = task_a(1)
        r2 = task_a(1)
        self.assertNotEqual(r1.id, r2.id)
        self.assertEqual(self.huey.pending_count(), 2)

    def test_different_keys_are_independent(self):
        @self.huey.task()
        def task_a(n):
            return n

        r1 = task_a(1, dedupe_key='ka')
        r2 = task_a(1, dedupe_key='kb')
        self.assertNotEqual(r1.id, r2.id)
        self.assertEqual(self.huey.pending_count(), 2)

    def test_binding_released_after_terminal_state(self):
        @self.huey.task()
        def task_a(n):
            return n + 1

        key = self.huey._dedupe_storage_key('k1')
        r1 = task_a(1, dedupe_key='k1')
        self.assertTrue(self.huey.storage.has_data_for_key(key))

        task = self.huey.dequeue()
        self.huey.execute(task)
        # Binding released once the task completed - a new submission is allowed.
        self.assertFalse(self.huey.storage.has_data_for_key(key))

        r2 = task_a(1, dedupe_key='k1')
        self.assertNotEqual(r2.id, r1.id)
        self.assertEqual(self.huey.pending_count(), 1)

    def test_conflict_on_different_fingerprint(self):
        @self.huey.task()
        def task_a(n):
            return n

        r1 = task_a(1, dedupe_key='k1')
        # Same key, different arguments -> different fingerprint -> conflict.
        with self.assertRaises(DedupeConflict) as ctx:
            task_a(2, dedupe_key='k1')

        exc = ctx.exception
        self.assertEqual(exc.dedupe_key, 'k1')
        self.assertEqual(exc.existing_id, r1.id)
        self.assertNotEqual(exc.fingerprint, exc.existing_fingerprint)
        # The conflicting submission must not have enqueued anything.
        self.assertEqual(self.huey.pending_count(), 1)

    def test_conflict_on_different_task_type(self):
        @self.huey.task()
        def task_a(n):
            return n

        @self.huey.task()
        def task_b(n):
            return n

        r1 = task_a(1, dedupe_key='k1')
        with self.assertRaises(DedupeConflict):
            task_b(1, dedupe_key='k1')
        self.assertEqual(self.huey.pending_count(), 1)
        self.assertEqual(r1.id, self.huey.dequeue().id)

    def test_different_kwargs_conflict(self):
        @self.huey.task()
        def task_a(n, m=0):
            return n

        task_a(1, m=1, dedupe_key='k1')
        with self.assertRaises(DedupeConflict):
            task_a(1, m=2, dedupe_key='k1')

    # ------------------------------------------------------------- scheduling
    def test_dedupe_while_scheduled(self):
        # A task parked in the schedule (eta in the future) still holds its
        # binding, so a duplicate submission coalesces onto it.
        @self.huey.task()
        def task_a(n):
            return n

        r1 = task_a.schedule((1,), delay=60, dedupe_key='k1')
        # schedule() enqueues with an eta; the worker parks it in the schedule
        # when it sees the task is not yet ready to run.
        not_ready = self.huey.dequeue()
        self.huey.execute(not_ready)
        self.assertEqual(self.huey.scheduled_count(), 1)

        r2 = task_a(1, dedupe_key='k1')
        self.assertEqual(r2.id, r1.id)
        self.assertEqual(self.huey.scheduled_count(), 1)
        self.assertEqual(self.huey.pending_count(), 0)

        # After the scheduler moves the due task back onto the queue and it
        # completes, a fresh submission is accepted.
        due = datetime.datetime.now() + datetime.timedelta(seconds=61)
        tasks = self.huey.read_schedule(due)
        self.assertEqual(len(tasks), 1)
        for t in tasks:
            self.huey.enqueue(t, _is_dedupe=True)
        self.assertEqual(self.huey.pending_count(), 1)
        # Pass a timestamp past the eta so the worker runs the task instead of
        # re-parking it in the schedule.
        self.assertEqual(self.huey.execute(self.huey.dequeue(), due), 1)

        r3 = task_a(1, dedupe_key='k1')
        self.assertNotEqual(r3.id, r1.id)

    def test_dedupe_with_delay_via_s(self):
        @self.huey.task()
        def task_a(n):
            return n

        r1 = self.huey.enqueue(task_a.s(1, delay=60, dedupe_key='k1'))
        not_ready = self.huey.dequeue()
        self.huey.execute(not_ready)
        self.assertEqual(self.huey.scheduled_count(), 1)
        r2 = task_a(1, dedupe_key='k1')
        self.assertEqual(r2.id, r1.id)

    # ----------------------------------------------------------------- retries
    def test_dedupe_survives_retry_chain(self):
        state = {'calls': 0}

        @self.huey.task(retries=2)
        def task_a(n):
            state['calls'] += 1
            if state['calls'] < 3:
                raise RetryTask()
            return n

        key = self.huey._dedupe_storage_key('k1')
        r1 = task_a(1, dedupe_key='k1')

        # While the retry chain is in progress the binding stays in place and
        # duplicate submissions keep coalescing onto the same task.
        task = self.huey.dequeue()
        self.huey.execute(task)  # Fails, retried immediately (no delay).
        self.assertTrue(self.huey.storage.has_data_for_key(key))
        r2 = task_a(1, dedupe_key='k1')
        self.assertEqual(r2.id, r1.id)
        self.assertEqual(self.huey.pending_count(), 1)

        self.huey.execute(self.huey.dequeue())  # Second failure, last retry.
        self.assertTrue(self.huey.storage.has_data_for_key(key))
        r3 = task_a(1, dedupe_key='k1')
        self.assertEqual(r3.id, r1.id)

        self.huey.execute(self.huey.dequeue())  # Success.
        self.assertFalse(self.huey.storage.has_data_for_key(key))
        self.assertEqual(r1(), 1)
        self.assertEqual(state['calls'], 3)

        # A new submission after the terminal state gets a brand-new task.
        r4 = task_a(1, dedupe_key='k1')
        self.assertNotEqual(r4.id, r1.id)

    def test_dedupe_released_when_retries_exhausted(self):
        @self.huey.task(retries=1)
        def task_a():
            raise ValueError('boom')

        key = self.huey._dedupe_storage_key('k1')
        r1 = task_a(dedupe_key='k1')
        self.huey.execute(self.huey.dequeue())  # Fail, requeue.
        self.assertTrue(self.huey.storage.has_data_for_key(key))
        self.huey.execute(self.huey.dequeue())  # Fail, retries exhausted.
        self.assertFalse(self.huey.storage.has_data_for_key(key))

        # The failed terminal state is observable through either handle, and a
        # new submission is permitted.
        r2 = task_a(dedupe_key='k1')
        self.assertNotEqual(r2.id, r1.id)

    def test_dedupe_survives_delayed_retry(self):
        @self.huey.task(retries=1, retry_delay=60)
        def task_a():
            raise ValueError('boom')

        key = self.huey._dedupe_storage_key('k1')
        r1 = task_a(dedupe_key='k1')
        self.huey.execute(self.huey.dequeue())  # Fail -> scheduled retry.
        self.assertEqual(self.huey.scheduled_count(), 1)
        self.assertTrue(self.huey.storage.has_data_for_key(key))

        # Duplicate while the retry sits in the schedule.
        r2 = task_a(dedupe_key='k1')
        self.assertEqual(r2.id, r1.id)

    # ------------------------------------------------------------- revocation
    def test_dedupe_released_when_revoked(self):
        @self.huey.task()
        def task_a(n):
            return n

        key = self.huey._dedupe_storage_key('k1')
        r1 = task_a(1, dedupe_key='k1')
        task = self.huey.dequeue()
        self.huey.revoke(task)
        self.huey.execute(task)  # Revoked -> terminal.
        self.assertFalse(self.huey.storage.has_data_for_key(key))

        r2 = task_a(1, dedupe_key='k1')
        self.assertNotEqual(r2.id, r1.id)

    def test_dedupe_released_when_expired(self):
        @self.huey.task()
        def task_a(n):
            return n

        key = self.huey._dedupe_storage_key('k1')
        past = datetime.datetime.now() - datetime.timedelta(seconds=60)
        r1 = task_a(1, dedupe_key='k1', expires=past)
        task = self.huey.dequeue()
        self.huey.execute(task)  # Expired -> terminal.
        self.assertFalse(self.huey.storage.has_data_for_key(key))

        r2 = task_a(1, dedupe_key='k1')
        self.assertNotEqual(r2.id, r1.id)

    def test_dedupe_released_on_pre_execute_cancel(self):
        @self.huey.task()
        def task_a(n):
            return n

        @self.huey.pre_execute(name='cancel')
        def cancel(task):
            raise CancelExecution()

        try:
            key = self.huey._dedupe_storage_key('k1')
            r1 = task_a(1, dedupe_key='k1')
            self.huey.execute(self.huey.dequeue())
            self.assertFalse(self.huey.storage.has_data_for_key(key))
            r2 = task_a(1, dedupe_key='k1')
            self.assertNotEqual(r2.id, r1.id)
        finally:
            self.huey.unregister_pre_execute('cancel')

    # ----------------------------------------------------------- failure paths
    def test_serialization_failure_releases_binding(self):
        @self.huey.task()
        def task_a(n):
            return n

        key = self.huey._dedupe_storage_key('k1')

        class NotPicklable:
            def __reduce__(self):
                raise TypeError('cannot pickle')

        with self.assertRaises(TypeError):
            task_a(NotPicklable(), dedupe_key='k1')
        # No permanent placeholder left behind.
        self.assertFalse(self.huey.storage.has_data_for_key(key))

        # The partition is immediately usable again.
        r = task_a(1, dedupe_key='k1')
        self.assertEqual(self.huey.pending_count(), 1)
        self.assertEqual(r.id, self.huey.dequeue().id)

    def test_enqueue_failure_releases_binding(self):
        @self.huey.task()
        def task_a(n):
            return n

        key = self.huey._dedupe_storage_key('k1')
        original_enqueue = self.huey.storage.enqueue

        def broken_enqueue(data, priority=None):
            raise RuntimeError('broker down')

        self.huey.storage.enqueue = broken_enqueue
        try:
            with self.assertRaises(RuntimeError):
                task_a(1, dedupe_key='k1')
            self.assertFalse(self.huey.storage.has_data_for_key(key))
        finally:
            self.huey.storage.enqueue = original_enqueue

        # The partition is reusable after the broker recovers.
        r = task_a(1, dedupe_key='k1')
        self.assertEqual(self.huey.pending_count(), 1)
        self.assertEqual(r.id, self.huey.dequeue().id)

    # ------------------------------------------------------------- concurrency
    def test_concurrent_submissions_coalesce(self):
        @self.huey.task()
        def task_a(n):
            return n

        results = []
        errors = []
        barrier = threading.Barrier(4)

        def submit():
            try:
                barrier.wait()
                results.append(task_a(1, dedupe_key='k1'))
            except Exception as exc:
                errors.append(exc)

        # Two submitters + main thread = 4 participants (2 consumers below act
        # as the other participants via two submitter threads here).
        threads = [threading.Thread(target=submit) for _ in range(4)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        self.assertEqual(errors, [])
        self.assertEqual(len(results), 4)
        ids = {r.id for r in results}
        self.assertEqual(len(ids), 1)  # Exactly one in-flight task.
        self.assertEqual(self.huey.pending_count(), 1)

    def test_concurrent_conflicting_fingerprints_single_winner(self):
        @self.huey.task()
        def task_a(n):
            return n

        winners = []
        conflicts = []
        nthreads = 20
        barrier = threading.Barrier(nthreads)

        def submit(i):
            barrier.wait()
            try:
                winners.append(task_a(i, dedupe_key='k1').id)
            except DedupeConflict:
                conflicts.append(i)

        threads = [threading.Thread(target=submit, args=(i,))
                   for i in range(nthreads)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        # Exactly one submission wins the slot; every other fingerprint
        # conflicts rather than silently reusing the wrong result.
        self.assertEqual(len(winners), 1)
        self.assertEqual(len(conflicts), nthreads - 1)
        self.assertEqual(self.huey.pending_count(), 1)

    def test_two_consumers_two_submitters_single_task(self):
        # Interleave two consumer threads draining the queue with two submitter
        # threads hammering the same partition. Only one task may ever be
        # enqueued while it is in-flight.
        @self.huey.task()
        def task_a(n):
            time.sleep(0.01)
            return n

        ids = []
        id_lock = threading.Lock()
        start = threading.Event()

        def submitter():
            start.wait()
            for _ in range(25):
                r = task_a(1, dedupe_key='k1')
                with id_lock:
                    ids.append(r.id)

        def consumer():
            start.wait()
            for _ in range(50):
                task = self.huey.dequeue()
                if task is not None:
                    self.huey.execute(task)
                else:
                    time.sleep(0.005)

        threads = ([threading.Thread(target=submitter) for _ in range(2)] +
                   [threading.Thread(target=consumer) for _ in range(2)])
        for t in threads:
            t.start()
        start.set()
        for t in threads:
            t.join()

        # While a task was in-flight all handles shared its id; across serial
        # runs new ids appear, but the queue never held duplicates.
        self.assertTrue(len(ids) == 50)
        self.assertEqual(self.huey.pending_count(), 0)
        # Every observed id belongs to a task that actually ran and completed,
        # i.e. no handle ever pointed at a phantom task.
        self.assertEqual(self.huey.scheduled_count(), 0)

    # ------------------------------------------------------------------ misc
    def test_dedupe_key_roundtrips_through_serialization(self):
        @self.huey.task()
        def task_a(n):
            return n

        r = task_a(1, dedupe_key='k1')
        task = self.huey.dequeue()
        self.assertEqual(task.dedupe_key, 'k1')
        restored = self.huey.deserialize_task(self.huey.serialize_task(task))
        self.assertEqual(restored.dedupe_key, 'k1')
        self.assertEqual(restored.id, task.id)

    def test_dedupe_with_disabled_result_store(self):
        huey = MemoryHuey(utc=False, results=False)

        @huey.task()
        def task_a(n):
            return n

        ret1 = task_a(1, dedupe_key='k1')
        self.assertIsNone(ret1)
        ret2 = task_a(1, dedupe_key='k1')
        self.assertIsNone(ret2)
        # Coalescing still works even when result handles are not returned.
        self.assertEqual(huey.pending_count(), 1)

    def test_immediate_mode_dedupe(self):
        huey = MemoryHuey(utc=False, immediate=True)

        @huey.task()
        def task_a(n):
            return n + 1

        r1 = task_a(1, dedupe_key='k1')
        # Immediate execution is synchronous, so by the time the call returns
        # the task has reached a terminal state and the binding is released.
        self.assertEqual(r1(), 2)
        key = huey._dedupe_storage_key('k1')
        self.assertFalse(huey.storage.has_data_for_key(key))

        r2 = task_a(1, dedupe_key='k1')
        self.assertNotEqual(r2.id, r1.id)
        self.assertEqual(r2(), 2)

    def test_dedupe_key_per_call_not_decorator_default(self):
        # Mixing keyed and unkeyed calls of the same task must not interfere.
        @self.huey.task()
        def task_a(n):
            return n

        r_keyed = task_a(1, dedupe_key='k1')
        r_plain = task_a(1)
        self.assertNotEqual(r_keyed.id, r_plain.id)
        self.assertEqual(self.huey.pending_count(), 2)
        r_keyed_2 = task_a(1, dedupe_key='k1')
        self.assertEqual(r_keyed_2.id, r_keyed.id)
        self.assertEqual(self.huey.pending_count(), 2)

    def test_dedupe_with_pipeline(self):
        # The dedupe binding belongs to the head task only; a downstream
        # callback is an independent, un-keyed task and must not be coalesced.
        @self.huey.task()
        def task_a(n):
            return n + 1

        @self.huey.task()
        def task_b(n):
            return n * 2

        key = self.huey._dedupe_storage_key('k1')
        pipeline = task_a.s(1, dedupe_key='k1').then(task_b)
        self.assertEqual(pipeline.dedupe_key, 'k1')
        self.assertIsNone(pipeline.on_complete.dedupe_key)

        result = self.huey.enqueue(pipeline)
        self.assertEqual(self.huey.pending_count(), 1)
        head = self.huey.dequeue()
        self.huey.execute(head)  # Runs head, enqueues callback.
        # Head reached its terminal state: binding released.
        self.assertFalse(self.huey.storage.has_data_for_key(key))
        self.assertEqual(self.huey.pending_count(), 1)
        callback = self.huey.dequeue()
        self.assertIsNone(callback.dedupe_key)
        self.assertEqual(self.huey.execute(callback), 4)  # (1 + 1) * 2

    def test_dedupe_with_group_members(self):
        @self.huey.task()
        def task_a(n):
            return n

        g = group([task_a.s(1, dedupe_key='g1'),
                   task_a.s(2, dedupe_key='g2')])
        results = self.huey.enqueue(g)
        self.assertEqual(len(results), 2)
        self.assertEqual(self.huey.pending_count(), 2)
        self.assertTrue(self.huey.storage.has_data_for_key(
            self.huey._dedupe_storage_key('g1')))
        self.assertTrue(self.huey.storage.has_data_for_key(
            self.huey._dedupe_storage_key('g2')))

