import datetime
import os
import shutil
import signal
import sys
import tempfile
import threading
import time
import unittest

from huey import SqliteHuey
from huey.api import crontab
from huey.consumer import Consumer
from huey.consumer import ConsumerStopped
from huey.consumer import DrainState
from huey.consumer import ForkSafeFlag
from huey.consumer import ProcessEnvironment
from huey.consumer import Scheduler
from huey.consumer import WORKER_TO_ENVIRONMENT
from huey.consumer_options import ConsumerConfig
from huey.consumer_options import OptionParserHandler
from huey.exceptions import TaskException
from huey.exceptions import TaskTimeout
from huey.signals import SIGNAL_COMPLETE
from huey.signals import SIGNAL_ERROR
from huey.signals import SIGNAL_INTERRUPTED
from huey.tests.base import BaseTestCase
from huey.tests.base import slow_test


class TestConsumer(Consumer):
    class _Scheduler(Scheduler):
        def sleep_for_interval(self, current, interval):
            pass
    scheduler_class = _Scheduler


class TestConsumerIntegration(BaseTestCase):
    consumer_class = TestConsumer

    def test_consumer_minimal(self):
        @self.huey.task()
        def task_a(n):
            return n + 1

        with self.consumer_context():
            result = task_a(1)
            self.assertEqual(result.get(blocking=True, timeout=2), 2)

    def work_on_tasks(self, consumer, n=1, now=None):
        worker, _ = consumer.worker_threads[0]
        for i in range(n):
            self.assertEqual(len(self.huey), n - i)
            worker.loop(now)

    def schedule_tasks(self, consumer, now=None):
        scheduler = consumer._create_scheduler()
        scheduler._next_loop = time.monotonic() + 60
        scheduler._next_periodic = time.monotonic() - 60
        scheduler.loop(now)

    @slow_test()
    def test_consumer_timeout(self):
        @self.huey.task(timeout=0.1, context=True)
        def t(n, task=None):
            if n:
                for _ in range(100):
                    task.check_timeout()
                    time.sleep(n / 100)
            return n

        r1 = t(0)
        r2 = t(0.2)
        consumer = self.consumer(workers=1)
        self.work_on_tasks(consumer, 2)
        self.assertEqual(r1.get(), 0)
        with self.assertRaises(TaskException):
            r2.get()
        try:
            r2.get()
        except TaskException as exc:
            self.assertEqual(exc.metadata['error'],
                             'TaskTimeout(\'timeout 0.1s\')')

    def test_consumer_schedule_task(self):
        @self.huey.task()
        def task_a(n):
            return n + 1

        now = datetime.datetime.now()
        eta = now + datetime.timedelta(days=1)
        r60 = task_a.schedule((2,), delay=60)
        rday = task_a.schedule((3,), eta=eta)

        consumer = self.consumer(workers=1)
        self.work_on_tasks(consumer, 2)  # Process the two messages.

        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 2)

        self.schedule_tasks(consumer, now)
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 2)

        # Ensure that the task that had a delay of 60s is read from schedule.
        later = now + datetime.timedelta(seconds=65)
        self.schedule_tasks(consumer, later)
        self.assertEqual(len(self.huey), 1)
        self.assertEqual(self.huey.scheduled_count(), 1)

        # We can now work on our scheduled task.
        self.work_on_tasks(consumer, 1, later)
        self.assertEqual(r60.get(), 3)

        # Verify the task was run and that there is only one task remaining to
        # be scheduled (in a day).
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 1)

        tomorrow = now + datetime.timedelta(days=1)
        self.schedule_tasks(consumer, tomorrow)
        self.work_on_tasks(consumer, 1, tomorrow)
        self.assertEqual(rday.get(), 4)
        self.assertEqual(len(self.huey), 0)
        self.assertEqual(self.huey.scheduled_count(), 0)

    def test_consumer_periodic_tasks(self):
        state = []

        @self.huey.periodic_task(crontab(minute='*/10'))
        def task_p1():
            state.append('p1')

        @self.huey.periodic_task(crontab(minute='0', hour='0'))
        def task_p2():
            state.append('p2')

        consumer = self.consumer(workers=1)
        dt = datetime.datetime(2000, 1, 1, 0, 0)
        self.schedule_tasks(consumer, dt)
        self.assertEqual(len(self.huey), 2)
        self.work_on_tasks(consumer, 2)
        self.assertEqual(state, ['p1', 'p2'])

        dt = datetime.datetime(2000, 1, 1, 12, 0)
        self.schedule_tasks(consumer, dt)
        self.assertEqual(len(self.huey), 1)
        self.work_on_tasks(consumer, 1)
        self.assertEqual(state, ['p1', 'p2', 'p1'])

        task_p1.revoke()
        self.schedule_tasks(consumer, dt)
        self.assertEqual(len(self.huey), 1)  # Enqueued despite being revoked.
        self.work_on_tasks(consumer, 1)
        self.assertEqual(state, ['p1', 'p2', 'p1'])  # No change, not executed.

    def test_scheduler_periodic_catch_up(self):
        @self.huey.periodic_task(crontab(minute='*'))
        def task_p():
            pass

        consumer = self.consumer(workers=1)
        scheduler = consumer._create_scheduler()
        scheduler._next_loop = time.monotonic() + 60

        # Simulate a 5-minute stall (e.g. suspend/resume): the periodic check
        # timestamp is far in the past. The scheduler skips the missed checks
        # and fires once for the current minute, rather than running the
        # back-to-back checks, which would enqueue duplicate tasks.
        scheduler._next_periodic = time.monotonic() - 300
        scheduler.loop(datetime.datetime(2000, 1, 1, 0, 0))
        self.assertEqual(len(self.huey), 1)
        self.assertTrue(scheduler._next_periodic > time.monotonic())

        # Subsequent iterations do not re-fire for the stalled period.
        scheduler.loop(datetime.datetime(2000, 1, 1, 0, 0))
        self.assertEqual(len(self.huey), 1)

    def test_scheduler_periodic_enqueue_error(self):
        @self.huey.periodic_task(crontab(minute='*'))
        def task_p():
            pass

        consumer = self.consumer(workers=1)
        scheduler = consumer._create_scheduler()
        scheduler._next_loop = time.monotonic() + 60
        scheduler._next_periodic = time.monotonic() - 1

        def broken_enqueue(task):
            raise ValueError('storage down')
        scheduler.huey.enqueue = broken_enqueue

        # The error is logged and the scheduler keeps running.
        scheduler.loop(datetime.datetime(2000, 1, 1, 0, 0))
        self.assertEqual(len(self.huey), 0)
        self.assertTrue(scheduler._next_periodic > time.monotonic())


class TestConsumerConfig(BaseTestCase):
    def test_default_config(self):
        cfg = ConsumerConfig()
        cfg.validate()
        consumer = self.huey.create_consumer(**cfg.values)
        self.assertEqual(consumer.workers, 1)
        self.assertEqual(consumer.worker_type, 'thread')
        self.assertTrue(consumer.periodic)
        self.assertEqual(consumer.default_delay, 0.1)
        self.assertEqual(consumer.scheduler_interval, 1)
        self.assertTrue(consumer._health_check)

    def test_consumer_config(self):
        cfg = ConsumerConfig(workers=3, worker_type='process', initial_delay=1,
                             backoff=2, max_delay=4, check_worker_health=False,
                             scheduler_interval=30, periodic=False)
        cfg.validate()
        consumer = self.huey.create_consumer(**cfg.values)

        self.assertEqual(consumer.workers, 3)
        self.assertEqual(consumer.worker_type, 'process')
        self.assertFalse(consumer.periodic)
        self.assertEqual(consumer.default_delay, 1)
        self.assertEqual(consumer.backoff, 2)
        self.assertEqual(consumer.max_delay, 4)
        self.assertEqual(consumer.scheduler_interval, 30)
        self.assertFalse(consumer._health_check)

    @unittest.skipIf(sys.platform == 'win32', 'requires fork()')
    def test_process_environment_uses_fork(self):
        # The worker/scheduler runnables cannot be pickled, so the process
        # environment must use the fork start-method regardless of the
        # platform default (spawn on MacOS 3.8+, forkserver on Linux 3.14+).
        cfg = ConsumerConfig(worker_type='process')
        cfg.validate()
        consumer = self.huey.create_consumer(**cfg.values)
        self.assertEqual(consumer.environment.mp.get_start_method(), 'fork')

    def test_invalid_values(self):
        def assertInvalid(**kwargs):
            cfg = ConsumerConfig(**kwargs)
            self.assertRaises(ValueError, cfg.validate)

        assertInvalid(backoff=0.5)
        assertInvalid(scheduler_interval=90)
        assertInvalid(scheduler_interval=7)
        assertInvalid(scheduler_interval=45)
        assertInvalid(graceful_signal='HUP')

    def test_shutdown_options(self):
        cfg = ConsumerConfig(shutdown_timeout=5, graceful_signal='TERM')
        cfg.validate()
        consumer = self.huey.create_consumer(**cfg.values)
        self.assertEqual(consumer.shutdown_timeout, 5)
        self.assertEqual(consumer.graceful_signal, 'TERM')

        parser = OptionParserHandler().get_option_parser()
        options, _ = parser.parse_args(['-t', '2.5', '--graceful-signal=TERM'])
        self.assertEqual(options.shutdown_timeout, 2.5)
        self.assertEqual(options.graceful_signal, 'TERM')

        consumer = self.huey.create_consumer(**ConsumerConfig().values)
        self.assertIsNone(consumer.shutdown_timeout)
        self.assertEqual(consumer.graceful_signal, 'INT')


class TestConsumerShutdown(BaseTestCase):
    def setUp(self):
        super(TestConsumerShutdown, self).setUp()
        self._handlers = [(s, signal.getsignal(s))
                          for s in (signal.SIGINT, signal.SIGTERM)]

    def tearDown(self):
        for signum, handler in self._handlers:
            signal.signal(signum, handler)
        super(TestConsumerShutdown, self).tearDown()

    def test_stop_flag_wait(self):
        for env_class in WORKER_TO_ENVIRONMENT.values():
            try:
                flag = env_class().get_stop_flag()
            except Exception:
                continue
            self.assertFalse(flag.wait(0))
            flag.set()
            self.assertTrue(flag.wait(0))

    def test_sleep_wakes_on_stop(self):
        consumer = self.huey.create_consumer(initial_delay=5, max_delay=5)
        worker, _ = consumer.worker_threads[0]
        self.assertIs(worker.stop_flag, consumer.stop_flag)

        threading.Timer(0.05, consumer.stop_flag.set).start()
        start = time.monotonic()
        worker.sleep()
        self.assertLess(time.monotonic() - start, 2)

        start = time.monotonic()
        worker.sleep_for_interval(start, 5)
        self.assertLess(time.monotonic() - start, 2)

    def test_shutdown_timeout(self):
        release = threading.Event()
        started = threading.Event()

        @self.huey.task()
        def block():
            started.set()
            release.wait()

        block()
        consumer = self.huey.create_consumer(shutdown_timeout=0.2)
        consumer.scheduler.start()
        _, worker_t = consumer.worker_threads[0]
        worker_t.start()
        self.assertTrue(started.wait(2))

        start = time.monotonic()
        with self.assertLogs('huey.consumer', 'WARNING') as logs:
            consumer.stop(graceful=True)
        self.assertLess(time.monotonic() - start, 5)
        self.assertIn('Shutdown timeout', logs.output[0])
        release.set()
        worker_t.join(2)

    @slow_test()
    def test_notify_interrupted_completing_task(self):
        release = threading.Event()
        started = threading.Event()

        @self.huey.task()
        def block():
            started.set()
            release.wait(2)  # Hang out in the task after stop-flag set.
            return 1337

        signals = []
        @self.huey.signal()
        def capture(sig, task, *args, **kwargs):
            signals.append(sig)

        result = block()
        consumer = self.huey.create_consumer()
        _, worker_t = consumer.worker_threads[0]
        worker_t.start()
        self.assertTrue(started.wait(2))  # Wait for consumer to start task.

        consumer.stop_flag.set()
        self.huey.notify_interrupted_tasks()  # Task blocked, fire interrupted.
        release.set()  # Task exits now.
        worker_t.join(2)
        self.assertFalse(worker_t.is_alive())

        self.assertEqual(result(), 1337)
        self.assertIn(SIGNAL_INTERRUPTED, signals)
        self.assertIn(SIGNAL_COMPLETE, signals)
        self.assertNotIn(SIGNAL_ERROR, signals)

    @slow_test()
    def test_notify_interrupted_pop_race(self):
        release = threading.Event()
        started = threading.Event()
        in_pop = threading.Event()
        resume_pop = threading.Event()

        class PausingSet(set):
            def pop(self):
                in_pop.set()
                resume_pop.wait(2)
                return set.pop(self)
        self.huey._tasks_in_flight = PausingSet()

        @self.huey.task()
        def block():
            started.set()
            release.wait(2)

        block()
        consumer = self.huey.create_consumer()
        _, worker_t = consumer.worker_threads[0]
        worker_t.start()
        self.assertTrue(started.wait(2))  # Like above, wait for task to start.
        consumer.stop_flag.set()  # Set stop flag on consumer.

        errors = []
        def notify():
            try:
                self.huey.notify_interrupted_tasks()
            except BaseException as exc:
                errors.append(exc)
        notify_t = threading.Thread(target=notify)
        notify_t.start()
        self.assertTrue(in_pop.wait(2))  # Wait for notify thread to start.
        release.set()  # Let task finish.
        worker_t.join(2)
        self.assertFalse(worker_t.is_alive())
        resume_pop.set()  # Now simulate "cleanup" of notifying interrupted.
        notify_t.join(2)
        self.assertEqual(errors, [])

    def test_signal_handlers(self):
        consumer = self.huey.create_consumer()
        consumer._set_signal_handlers()
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            self.assertEqual(signal.getsignal(signum), consumer._handle_signal)
        self.assertEqual(consumer._signal_actions, {
            signal.SIGINT: (False, True),
            signal.SIGTERM: (False, False),
            signal.SIGHUP: (True, True)})

        consumer = self.huey.create_consumer(graceful_signal='TERM')
        self.assertEqual(consumer._signal_actions[signal.SIGTERM],
                         (False, True))
        self.assertEqual(consumer._signal_actions[signal.SIGINT],
                         (False, False))

        consumer._handle_signal(signal.SIGTERM, None)
        self.assertTrue(consumer._graceful)
        self.assertFalse(consumer._restart)
        self.assertEqual(consumer._signum, signal.SIGTERM)
        self.assertEqual(signal.getsignal(signal.SIGTERM),
                         signal.default_int_handler)

        consumer._handle_signal(signal.SIGHUP, None)
        self.assertTrue(consumer._graceful)
        self.assertTrue(consumer._restart)

        consumer._handle_signal(signal.SIGINT, None)
        self.assertFalse(consumer._graceful)
        self.assertFalse(consumer._restart)

    def test_immediate_stop_ignores_stop_flag(self):
        # A worker interrupted inside stop_flag.wait() can exit holding the
        # flag's shared lock, so loop() must not touch the flag after stop().
        consumer = self.huey.create_consumer()
        def interrupt_workers():
            consumer.stop_flag = None
        consumer._interrupt_workers = interrupt_workers
        consumer._handle_signal(signal.SIGTERM, None)
        self.assertRaises(ConsumerStopped, consumer.loop)


class TestDrainState(unittest.TestCase):
    def test_drain_state(self):
        drain = DrainState()
        self.assertTrue(drain.is_drained)
        self.assertTrue(drain.wait(0))

        drain.admit('a')
        drain.admit('b')
        drain.admit('a')  # Admitting twice is idempotent.
        self.assertFalse(drain.is_drained)
        self.assertEqual(drain.remaining, 2)
        self.assertEqual(drain.remaining_ids(), ['a', 'b'])

        drain.task_done('a')
        self.assertFalse(drain.is_drained)
        self.assertEqual(drain.completed, 1)
        drain.task_done('unknown')  # Unknown tasks are ignored.
        self.assertEqual(drain.remaining, 1)

        drain.task_done('b')
        self.assertTrue(drain.is_drained)
        self.assertTrue(drain.wait(0))
        self.assertEqual(drain.completed, 2)

    def test_within_horizon(self):
        now = datetime.datetime(2026, 11, 16, 12, 0)
        drain = DrainState(eta_limit=60)
        self.assertTrue(drain.within_horizon(
            now + datetime.timedelta(seconds=59), now))
        self.assertFalse(drain.within_horizon(
            now + datetime.timedelta(seconds=61), now))
        self.assertTrue(drain.within_horizon(None, now))

        # No limit: everything is within the horizon.
        drain = DrainState()
        self.assertTrue(drain.within_horizon(
            now + datetime.timedelta(days=365), now))


class TestConsumerDrain(BaseTestCase):
    consumer_class = TestConsumer

    def test_drain_excludes_new_submissions(self):
        state = []

        @self.huey.task()
        def task_a(n):
            state.append(n)
            return n

        task_a(1)
        task_a(2)
        consumer = self.consumer(workers=1)
        worker, _ = consumer.worker_threads[0]

        drain = self.huey.start_drain()
        self.assertEqual(drain.remaining, 2)

        # Submitted after the drain began: not part of the batch.
        r3 = task_a(3)
        self.assertEqual(drain.remaining, 2)

        worker.loop()
        worker.loop()
        self.assertEqual(state, [1, 2])
        self.assertTrue(drain.is_drained)
        self.assertEqual(drain.completed, 2)

        # The new task is deferred and left in the queue for the next run.
        worker.loop()
        self.assertEqual(state, [1, 2])
        self.assertEqual(len(self.huey), 1)
        self.assertIsNone(r3.get_raw_result())

    def test_drain_includes_success_chain(self):
        state = []

        @self.huey.task()
        def task_a(n):
            state.append(('a', n))
            return n + 1

        @self.huey.task()
        def task_b(n):
            state.append(('b', n))
            return n

        pipeline = task_a.s(1)
        pipeline.then(task_b)
        self.huey.enqueue(pipeline)

        consumer = self.consumer(workers=1)
        worker, _ = consumer.worker_threads[0]
        drain = self.huey.start_drain()
        self.assertEqual(drain.remaining, 1)

        worker.loop()  # Runs task_a, enqueues task_b as part of the batch.
        self.assertEqual(drain.remaining, 1)
        worker.loop()  # Runs task_b.
        self.assertTrue(drain.is_drained)
        self.assertEqual(state, [('a', 1), ('b', 2)])

    def test_drain_includes_error_handler(self):
        state = []

        @self.huey.task()
        def task_a():
            raise ValueError('nope')

        @self.huey.task()
        def err_handler(exc):
            state.append(str(exc))

        task = task_a.s()
        task.error(err_handler)
        self.huey.enqueue(task)

        consumer = self.consumer(workers=1)
        worker, _ = consumer.worker_threads[0]
        drain = self.huey.start_drain()

        worker.loop()  # Fails, enqueues the error handler within the batch.
        self.assertEqual(drain.remaining, 1)
        worker.loop()
        self.assertTrue(drain.is_drained)
        self.assertEqual(state, ['nope'])

    def test_drain_includes_retries(self):
        attempts = []

        @self.huey.task(retries=1)
        def task_a():
            attempts.append(1)
            if len(attempts) == 1:
                raise ValueError('try again')
            return 'ok'

        r = task_a()
        consumer = self.consumer(workers=1)
        worker, _ = consumer.worker_threads[0]
        drain = self.huey.start_drain()

        worker.loop()  # Fails and is immediately requeued (same batch).
        self.assertEqual(drain.remaining, 1)
        worker.loop()  # Retry succeeds.
        self.assertTrue(drain.is_drained)
        self.assertEqual(r.get(), 'ok')

    def test_drain_retry_beyond_eta_limit(self):
        @self.huey.task(retries=1, retry_delay=3600)
        def task_a():
            raise ValueError('nope')

        task_a()
        consumer = self.consumer(workers=1)
        worker, _ = consumer.worker_threads[0]

        # With no ETA limit, the drain waits for the scheduled retry.
        drain = self.huey.start_drain()
        worker.loop()
        self.assertFalse(drain.is_drained)
        self.assertEqual(drain.remaining, 1)
        self.assertEqual(self.huey.scheduled_count(), 1)
        self.huey.stop_drain()
        self.huey.flush()

        # With an ETA limit, the far-future retry is retained in the
        # schedule for the next run and does not hold up the drain.
        task_a()
        drain = self.huey.start_drain(eta_limit=60)
        worker.loop()
        self.assertTrue(drain.is_drained)
        self.assertEqual(self.huey.scheduled_count(), 1)
        self.assertEqual(len(self.huey), 0)

    def test_drain_scheduled_horizon(self):
        state = []

        @self.huey.task()
        def task_a(n):
            state.append(n)
            return n

        now = datetime.datetime.now()
        near = task_a.s(1, eta=now + datetime.timedelta(seconds=30))
        far = task_a.s(2, eta=now + datetime.timedelta(seconds=120))
        self.huey.add_schedule(near)
        self.huey.add_schedule(far)

        drain = self.huey.start_drain(eta_limit=60)
        self.assertTrue(drain.is_admitted(near.id))
        self.assertFalse(drain.is_admitted(far.id))
        self.assertEqual(drain.remaining, 1)

        # The scheduler enqueues the admitted task when it comes due, and
        # leaves the other scheduled for the next run.
        consumer = self.consumer(workers=1)
        scheduler = consumer._create_scheduler()
        scheduler._next_loop = time.monotonic() + 60
        scheduler._next_periodic = time.monotonic() + 60
        later = now + datetime.timedelta(seconds=300)
        scheduler.loop(later)
        self.assertEqual(len(self.huey), 1)
        self.assertEqual(self.huey.scheduled_count(), 1)

        worker, _ = consumer.worker_threads[0]
        worker.loop(later)
        self.assertEqual(state, [1])  # The "near" task ran.
        self.assertTrue(drain.is_drained)

    def test_drain_skips_periodic_tasks(self):
        @self.huey.periodic_task(crontab(minute='*'))
        def task_p():
            pass

        consumer = self.consumer(workers=1)
        scheduler = consumer._create_scheduler()
        scheduler._next_loop = time.monotonic() + 60
        scheduler._next_periodic = time.monotonic() - 60

        self.huey.start_drain()
        scheduler.loop(datetime.datetime(2000, 1, 1, 0, 0))
        self.assertEqual(len(self.huey), 0)  # Not enqueued while draining.

    def test_drain_revoked_task(self):
        @self.huey.task()
        def task_a():
            return 1

        r = task_a()
        r.revoke()
        consumer = self.consumer(workers=1)
        worker, _ = consumer.worker_threads[0]

        drain = self.huey.start_drain()
        worker.loop()
        self.assertTrue(drain.is_drained)
        self.assertIsNone(r.get_raw_result())

    def test_drain_timeout_reports_remaining(self):
        release = threading.Event()
        started = threading.Event()

        @self.huey.task()
        def block():
            started.set()
            release.wait(5)
            return 'done'

        r = block()
        consumer = self.consumer(workers=1, shutdown_timeout=0.5)
        consumer.scheduler.start()
        _, worker_t = consumer.worker_threads[0]
        worker_t.start()
        self.assertTrue(started.wait(5))
        try:
            with self.assertLogs('huey.consumer', 'WARNING') as logs:
                consumer.drain(timeout=0.2)
            self.assertTrue(any('Drain incomplete' in line
                                for line in logs.output))
            # The unfinished task is reported, not marked as successful.
            self.assertIsNone(r.get_raw_result())
            self.assertEqual(self.huey.drain.remaining, 1)
        finally:
            release.set()
            worker_t.join(5)
        self.assertFalse(worker_t.is_alive())

        # Once the task finishes, the drain state reflects it.
        self.assertEqual(r.get(), 'done')
        self.assertTrue(self.huey.drain.is_drained)

    def test_drain_end_to_end(self):
        @self.huey.task()
        def task_a(n):
            return n + 1

        r1 = task_a(1)
        r2 = task_a(2)
        consumer = self.consumer(workers=2, drain_timeout=10)
        consumer.start()
        try:
            drain_t = threading.Thread(target=consumer.drain)
            drain_t.start()
            while self.huey.drain is None:
                time.sleep(0.001)
            r3 = task_a(3)  # Submitted after the drain began.
            drain_t.join(10)
            self.assertFalse(drain_t.is_alive())
        finally:
            consumer.stop()

        self.assertEqual(r1.get(), 2)
        self.assertEqual(r2.get(), 3)
        self.assertTrue(self.huey.drain.is_drained)
        # The late submission was left in the queue for the next run.
        self.assertIsNone(r3.get_raw_result())
        self.assertEqual(len(self.huey), 1)

    def test_drain_on_shutdown(self):
        @self.huey.task()
        def task_a(n):
            return n + 1

        r1 = task_a(1)
        consumer = self.consumer(workers=1, drain_on_shutdown=True,
                                 drain_timeout=10)
        consumer.start()
        consumer.stop(graceful=True)  # Graceful stop drains first.
        self.assertEqual(r1.get(), 2)
        self.assertIsNotNone(self.huey.drain)
        self.assertTrue(self.huey.drain.is_drained)
        self.assertTrue(consumer.stop_flag.is_set())

    def test_drain_options(self):
        cfg = ConsumerConfig(drain_on_shutdown=True, drain_timeout=30,
                             drain_eta_limit=60)
        cfg.validate()
        consumer = self.huey.create_consumer(**cfg.values)
        self.assertTrue(consumer.drain_on_shutdown)
        self.assertEqual(consumer.drain_timeout, 30)
        self.assertEqual(consumer.drain_eta_limit, 60)

        parser = OptionParserHandler().get_option_parser()
        options, _ = parser.parse_args(['-D', '-T', '15', '-E', '30'])
        self.assertTrue(options.drain_on_shutdown)
        self.assertEqual(options.drain_timeout, 15)
        self.assertEqual(options.drain_eta_limit, 30)

        consumer = self.huey.create_consumer(**ConsumerConfig().values)
        self.assertFalse(consumer.drain_on_shutdown)
        self.assertIsNone(consumer.drain_timeout)
        self.assertIsNone(consumer.drain_eta_limit)

        for kwargs in ({'drain_timeout': 0}, {'drain_timeout': -1},
                       {'drain_eta_limit': -1}):
            cfg = ConsumerConfig(**kwargs)
            self.assertRaises(ValueError, cfg.validate)


class TestForkSafeFlag(unittest.TestCase):
    def test_set_wait(self):
        flag = ForkSafeFlag()
        self.assertFalse(flag.is_set())
        start = time.monotonic()
        self.assertFalse(flag.wait(0.05))
        self.assertGreaterEqual(time.monotonic() - start, 0.04)
        flag.set()
        flag.set()
        self.assertTrue(flag.is_set())
        self.assertTrue(flag.wait(0))
        self.assertTrue(flag.wait(1))

    @unittest.skipIf(sys.platform == 'win32', 'requires fork()')
    def test_killed_waiter(self):
        # A multiprocessing.Event would block set() forever here.
        flag = ForkSafeFlag()
        waiter = ProcessEnvironment().create_process(flag.wait, 'waiter')
        waiter.start()
        time.sleep(0.1)
        os.kill(waiter.pid, signal.SIGKILL)
        waiter.join()
        flag.set()
        self.assertTrue(flag.wait(0))


class TestProcessTimeout(unittest.TestCase):
    def test_subsecond_timeout(self):
        # Sub-second timeouts fire. Previously alarm(int(0.15)) == alarm(0),
        # which cancelled the timer, silently disabling the timeout.
        with self.assertRaises(TaskTimeout):
            with ProcessEnvironment().timeout(0.15):
                time.sleep(2)

    def test_timeout_cancel(self):
        with ProcessEnvironment().timeout(0.15):
            pass
        time.sleep(0.2)  # No stray alarm fires after the context exits.


class TestProcessWorkers(unittest.TestCase):
    @unittest.skipIf(sys.platform == 'win32', 'requires fork()')
    @slow_test()
    def test_process_worker_integration(self):
        # End-to-end check that tasks execute in forked worker processes,
        # regardless of the platform's default start-method. Memory storage
        # cannot be used here (the forked workers would operate on copies),
        # so run against a sqlite database shared by parent and children.
        tmp_dir = tempfile.mkdtemp()
        huey = SqliteHuey('proctest', filename=os.path.join(tmp_dir, 'h.db'))

        @huey.task()
        def add_pid(a, b):
            return (a + b, os.getpid())

        huey.pending_count()  # Parent opens a connection before forking.
        consumer = huey.create_consumer(workers=2, worker_type='process')
        consumer.start()
        try:
            self.assertIsNone(huey.storage._conn)  # Closed before forking.
            r1, r2 = add_pid(1, 2), add_pid(3, 4)
            val1, pid1 = r1.get(blocking=True, timeout=10)
            val2, pid2 = r2.get(blocking=True, timeout=10)
            self.assertEqual((val1, val2), (3, 7))
            # The tasks ran in worker processes, not in this process.
            self.assertNotEqual(pid1, os.getpid())
            self.assertNotEqual(pid2, os.getpid())
        finally:
            consumer.stop(graceful=True)
            shutil.rmtree(tmp_dir, ignore_errors=True)
