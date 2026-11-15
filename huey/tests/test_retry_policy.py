import datetime
import unittest

from huey.api import MemoryHuey
from huey.exceptions import CancelExecution
from huey.exceptions import RetryDecisionError
from huey.exceptions import RetryDeadlineExceeded
from huey.exceptions import RetryTask
from huey.exceptions import TaskException
from huey.retry import RETRY_ABORT
from huey.retry import RETRY_AFTER
from huey.retry import RETRY_AT
from huey.retry import RETRY_NOW
from huey.retry import RetryDecision
from huey.tests.base import BaseTestCase


# Local exceptions, so test behavior never depends on builtin/global state.
class TransientError(Exception):
    pass


class PermanentError(Exception):
    pass


class PolicyError(Exception):
    pass


class TestRetryDecision(unittest.TestCase):
    def test_factory_methods(self):
        self.assertEqual(RetryDecision.abort().action, RETRY_ABORT)
        self.assertEqual(RetryDecision.retry_now().action, RETRY_NOW)

        d = RetryDecision.retry_after(12)
        self.assertEqual((d.action, d.delay, d.eta), (RETRY_AFTER, 12.0, None))

        when = datetime.datetime(2026, 11, 16, 12, 0, 0)
        d = RetryDecision.retry_at(when)
        self.assertEqual((d.action, d.eta, d.delay), (RETRY_AT, when, None))

    def test_retry_after_validates_delay(self):
        RetryDecision.retry_after(datetime.timedelta(seconds=5))
        for bad in (0, -1, -0.5):
            self.assertRaises(ValueError, RetryDecision.retry_after, bad)
        self.assertRaises(TypeError, RetryDecision.retry_after, '30')
        self.assertRaises(TypeError, RetryDecision.retry_after, True)
        self.assertRaises(ValueError, RetryDecision.retry_after,
                          float('nan'))
        self.assertRaises(ValueError, RetryDecision.retry_after,
                          float('inf'))

    def test_retry_at_validates_eta(self):
        self.assertRaises(TypeError, RetryDecision.retry_at, 60)
        self.assertRaises(TypeError, RetryDecision.retry_at, 'later')

    def test_equality(self):
        when = datetime.datetime(2026, 11, 16)
        self.assertEqual(RetryDecision.abort(), RetryDecision.abort())
        self.assertEqual(RetryDecision.retry_at(when),
                         RetryDecision.retry_at(when))
        self.assertNotEqual(RetryDecision.retry_now(),
                            RetryDecision.abort())


class TestRetryPolicy(BaseTestCase):
    """Retry-policy tests drive execution with an explicit fixed clock."""

    def get_huey(self):
        return MemoryHuey(utc=False)

    def run_once(self, timestamp):
        task = self.huey.dequeue()
        self.assertIsNotNone(task)
        self.huey.execute(task, timestamp=timestamp)
        return task

    def error_of(self, result):
        return self.trap_exception(result).metadata['error']

    def test_policy_abort_is_final(self):
        @self.huey.task(
            retries=5,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.abort())
        def task_a():
            raise PermanentError('nope')

        r = task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        self.run_once(ts)

        # Nothing is enqueued or scheduled: the failure is definitive.
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 0)
        self.assertEqual(self.error_of(r), 'PermanentError(\'nope\')')

    def test_policy_retry_now_requeues_immediately(self):
        seen = []

        def policy(exc, attempts, remaining):
            seen.append((attempts, remaining))
            return RetryDecision.retry_now()

        @self.huey.task(retries=2, retry_policy=policy)
        def task_a():
            raise TransientError('again')

        r = task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        self.run_once(ts)

        self.assertEqual(len(self.huey), 1)
        self.assertEqual(self.huey.scheduled_count(), 0)
        task = self.huey.dequeue()
        self.assertIsNone(task.eta)
        self.assertEqual(task.retries, 1)
        self.assertEqual(task.attempts, 1)
        # No deadline configured -> remaining budget is None.
        self.assertEqual(seen, [(1, None)])

    def test_policy_retry_after_schedules_with_fixed_clock(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(
            retries=3,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(45))
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)

        task, = self.huey.scheduled()
        self.assertEqual(task.eta, ts + datetime.timedelta(seconds=45))
        self.assertEqual(task.retries, 2)
        # Not ready at the original instant, ready once the eta arrives.
        self.assertFalse(self.huey.ready_to_run(task, ts))
        self.assertTrue(self.huey.ready_to_run(
            task, ts + datetime.timedelta(seconds=45)))

    def test_policy_retry_after_timedelta(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(
            retries=2,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(datetime.timedelta(seconds=9)))
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)
        task, = self.huey.scheduled()
        self.assertEqual(task.eta, ts + datetime.timedelta(seconds=9))

    def test_policy_retry_at_schedules_absolute_time(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        eta = ts + datetime.timedelta(seconds=120)

        @self.huey.task(
            retries=3,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_at(eta))
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)
        task, = self.huey.scheduled()
        self.assertEqual(task.eta, eta)

    def test_policy_receives_attempts_and_remaining_deadline(self):
        seen = []
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts + datetime.timedelta(seconds=60)

        def policy(exc, attempts, remaining):
            seen.append((attempts, remaining))
            return RetryDecision.abort()

        @self.huey.task(retries=3, retry_deadline=deadline,
                        retry_policy=policy)
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)
        self.assertEqual(seen, [(1, datetime.timedelta(seconds=60))])

    def test_policy_distinguishes_transient_and_permanent(self):
        def policy(exc, attempts, remaining):
            if isinstance(exc, PermanentError):
                return RetryDecision.abort()
            return RetryDecision.retry_now()

        @self.huey.task(retries=3, retry_policy=policy)
        def task_a(fail_permanent):
            if fail_permanent:
                raise PermanentError('forever')
            raise TransientError('temporary')

        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        r_perm = task_a(True)
        self.run_once(ts)
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.error_of(r_perm),
                         'PermanentError(\'forever\')')

        r_temp = task_a(False)
        self.run_once(ts)
        self.assertEqual(len(self.huey), 1)
        self.assertEqual(self.huey.scheduled_count(), 0)

    def test_retries_still_cap_attempts(self):
        # Even a policy that always asks to retry cannot retry past the
        # configured retries budget, preventing infinite retry loops.
        attempts = [0]

        @self.huey.task(
            retries=2,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_now())
        def task_a():
            attempts[0] += 1
            raise TransientError('again')

        task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        for _ in range(5):
            task = self.huey.dequeue()
            if task is None:
                break
            self.huey.execute(task, timestamp=ts)

        self.assertEqual(attempts[0], 3)  # Initial attempt + 2 retries.
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 0)

    def test_past_eta_cannot_reach_schedule_and_is_final(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(
            retries=3,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_at(ts - datetime.timedelta(seconds=1)))
        def task_a():
            raise TransientError('again')

        r = task_a()
        self.run_once(ts)

        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 0)
        error = self.error_of(r)
        self.assertIn('RetryDecisionError', error)
        self.assertIn('TransientError', error)
        self.assertIn('past', error)

    def test_illegal_delay_is_final(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(
            retries=3,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(-5))
        def task_a():
            raise TransientError('again')

        r = task_a()
        self.run_once(ts)
        self.assertEqual(self.huey.scheduled_count(), 0)
        error = self.error_of(r)
        self.assertIn('RetryDecisionError', error)
        self.assertIn('TransientError', error)

    def test_policy_returning_invalid_type_is_final(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(retries=3, retry_policy=lambda *a: 60)
        def task_a():
            raise TransientError('again')

        r = task_a()
        self.run_once(ts)
        self.assertEqual(self.huey.scheduled_count(), 0)
        error = self.error_of(r)
        self.assertIn('RetryDecisionError', error)

    def test_policy_exception_preserves_both_errors_and_is_final(self):
        def policy(exc, attempts, remaining):
            raise PolicyError('decision logic broken')

        @self.huey.task(retries=3, retry_policy=policy)
        def task_a():
            raise TransientError('task failed')

        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        r = task_a()
        self.run_once(ts)

        # No recursion: the task is not retried again.
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 0)

        error = self.error_of(r)
        # Both the original task exception and the policy error are retained.
        self.assertIn('RetryDecisionError', error)
        self.assertIn('TransientError', error)
        self.assertIn('PolicyError', error)

    def test_policy_exception_does_not_infinite_loop(self):
        calls = [0]

        def policy(exc, attempts, remaining):
            calls[0] += 1
            raise PolicyError('boom')

        @self.huey.task(retries=10, retry_policy=policy)
        def task_a():
            raise TransientError('again')

        task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        task = self.huey.dequeue()
        self.huey.execute(task, timestamp=ts)

        # The broken policy is consulted exactly once, then the task ends.
        self.assertEqual(calls, [1])
        self.assertIsNone(self.huey.dequeue())

    def test_retry_after_deadline_exceeded_is_final(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts + datetime.timedelta(seconds=30)

        @self.huey.task(
            retries=3, retry_deadline=deadline,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(60))
        def task_a():
            raise TransientError('again')

        r = task_a()
        self.run_once(ts)

        self.assertEqual(self.huey.scheduled_count(), 0)
        error = self.error_of(r)
        self.assertIn('RetryDeadlineExceeded', error)
        self.assertIn('TransientError', error)

    def test_retry_at_deadline_exceeded_is_final(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts + datetime.timedelta(seconds=30)

        @self.huey.task(
            retries=3, retry_deadline=deadline,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_at(ts + datetime.timedelta(seconds=31)))
        def task_a():
            raise TransientError('again')

        r = task_a()
        self.run_once(ts)
        self.assertEqual(self.huey.scheduled_count(), 0)
        self.assertIn('RetryDeadlineExceeded', self.error_of(r))

    def test_deadline_already_elapsed_is_final_even_for_retry_now(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts - datetime.timedelta(seconds=1)

        @self.huey.task(
            retries=3, retry_deadline=deadline,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_now())
        def task_a():
            raise TransientError('again')

        r = task_a()
        self.run_once(ts)
        self.assertEqual(len(self.huey), 0)
        self.assertIn('RetryDeadlineExceeded', self.error_of(r))

    def test_retry_within_deadline_proceeds(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts + datetime.timedelta(seconds=60)

        @self.huey.task(
            retries=3, retry_deadline=deadline,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(30))
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)
        task, = self.huey.scheduled()
        self.assertEqual(task.eta, ts + datetime.timedelta(seconds=30))

    def test_cancel_execution_bypasses_policy(self):
        policy_calls = [0]

        def policy(exc, attempts, remaining):
            policy_calls[0] += 1
            return RetryDecision.abort()

        @self.huey.task(retries=3, retry_policy=policy)
        def task_a():
            raise CancelExecution(retry=False)

        task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        self.run_once(ts)
        self.assertEqual(policy_calls, [0])
        self.assertEqual(len(self.huey), 0)

    def test_retry_task_bypasses_policy(self):
        policy_calls = [0]

        def policy(exc, attempts, remaining):
            policy_calls[0] += 1
            return RetryDecision.abort()

        @self.huey.task(retries=3, retry_policy=policy)
        def task_a():
            raise RetryTask(delay=15)

        task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        self.run_once(ts)
        self.assertEqual(policy_calls, [0])
        self.assertEqual(self.huey.scheduled_count(), 1)

    def test_legacy_retries_delay_backoff_still_work_without_policy(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(retries=2, retry_delay=10, retry_backoff=2)
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)
        task, = self.huey.scheduled()
        self.assertEqual(task.retry_delay, 20)  # Grown by the backoff.
        self.assertEqual(task.retries, 1)

    def test_legacy_immediate_retry_without_policy_or_delay(self):
        @self.huey.task(retries=1)
        def task_a():
            raise TransientError('again')

        task_a()
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        self.run_once(ts)
        # Immediate re-enqueue, no schedule entry.
        self.assertEqual(len(self.huey), 1)
        self.assertEqual(self.huey.scheduled_count(), 0)

    def test_policy_with_retry_delay_uses_policy_eta(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.task(
            retries=3, retry_delay=99,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(7))
        def task_a():
            raise TransientError('again')

        task_a()
        self.run_once(ts)
        task, = self.huey.scheduled()
        # The policy's eta wins over the configured retry_delay.
        self.assertEqual(task.eta, ts + datetime.timedelta(seconds=7))

    def test_retry_deadline_resolved_and_serialized(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts + datetime.timedelta(seconds=120)

        @self.huey.task(
            retries=2, retry_deadline=deadline,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.abort())
        def task_a():
            raise TransientError('again')

        task_a()
        queued = self.huey.dequeue()
        self.assertEqual(queued.retry_deadline_resolved, deadline)

        # Round-trip through the serializer, like a real broker would.
        restored = self.huey.deserialize_task(
            self.huey.serialize_task(queued))
        self.assertEqual(restored.retry_deadline_resolved, deadline)
        self.assertEqual(restored.attempts, 0)

        self.huey.execute(restored, timestamp=ts)
        self.assertEqual(len(self.huey), 0)

    def test_attempts_increment_across_retries(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        observed = []

        def policy(exc, attempts, remaining):
            observed.append(attempts)
            return RetryDecision.retry_now()

        @self.huey.task(retries=2, retry_policy=policy)
        def task_a():
            raise TransientError('again')

        task_a()
        for _ in range(3):
            task = self.huey.dequeue()
            self.huey.execute(task, timestamp=ts)
        # The policy is consulted on the two failures that still have retries;
        # the final (third) failure is exhausted before the policy runs.
        self.assertEqual(observed, [1, 2])

    def test_periodic_task_supports_retry_policy(self):
        from huey.api import crontab
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)

        @self.huey.periodic_task(
            crontab(), retries=2,
            retry_policy=lambda exc, attempts, remaining:
                RetryDecision.retry_after(20))
        def task_p():
            raise TransientError('again')

        task_p()
        self.run_once(ts)
        task, = self.huey.scheduled()
        self.assertEqual(task.eta, ts + datetime.timedelta(seconds=20))

    def test_wrapped_exception_repr_contains_both_errors(self):
        original = TransientError('boom')
        decision = PolicyError('bad callback')
        wrapped = RetryDecisionError(original, decision)
        text = repr(wrapped)
        self.assertIn('RetryDecisionError', text)
        self.assertIn('TransientError', text)
        self.assertIn('PolicyError', text)
        self.assertIs(wrapped.task_error, original)
        self.assertIs(wrapped.decision_error, decision)

    def test_deadline_exception_carries_context(self):
        ts = datetime.datetime(2026, 11, 16, 12, 0, 0)
        deadline = ts + datetime.timedelta(seconds=5)
        eta = ts + datetime.timedelta(seconds=9)
        original = TransientError('boom')
        exc = RetryDeadlineExceeded(original, deadline, eta)
        self.assertIs(exc.task_error, original)
        self.assertEqual(exc.deadline, deadline)
        self.assertEqual(exc.eta, eta)
        self.assertIn('RetryDeadlineExceeded', repr(exc))
