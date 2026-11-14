import datetime
import os
import sqlite3
import threading
import unittest
from queue import Queue

from huey.api import MemoryHuey
from huey.api import SqliteHuey
from huey.exceptions import ConfigurationError
from huey.exceptions import TaskException
from huey.tests.base import BaseTestCase


class GroupSchedulingTests(object):
    """
    Shared test-suite for storages implementing weighted group scheduling.
    Subclasses must provide get_huey() returning a Huey instance whose
    storage is configured with group_weights={'a': 2, 'b': 1}.
    """
    weights = {'a': 2, 'b': 1}

    def setUp(self):
        super(GroupSchedulingTests, self).setUp()
        self._extra = []
        self.s = self.huey.storage
        self.s.flush_all()

    def tearDown(self):
        self.s.flush_all()
        for storage in self._extra:
            storage.close()
        super(GroupSchedulingTests, self).tearDown()

    def new_storage(self, weights, default_group_weight=1):
        raise NotImplementedError

    def enqueue_n(self, group, n, prefix=None):
        if prefix is None:
            prefix = group if group is not None else 'u'
        for i in range(n):
            self.s.enqueue(b'%s%d' % (prefix.encode(), i), group=group)

    def dequeue_groups(self, n):
        # Dequeue n tasks, returning the list of group names (inferred from
        # the task data prefix).
        return [self.s.dequeue().decode()[:1] for _ in range(n)]

    def test_fixed_sequence(self):
        # Fixed task sequence, weights a=2, b=1. Smooth weighted round-robin
        # interleaves the groups: a, b, a, ...
        self.enqueue_n('a', 4)
        self.enqueue_n('b', 2)

        # enqueued_items() reflects the exact dequeue order.
        self.assertEqual(self.s.enqueued_items(),
                         [b'a0', b'b0', b'a1', b'a2', b'b1', b'a3'])
        self.assertEqual(self.s.enqueued_items(2), [b'a0', b'b0'])
        self.assertEqual(self.s.queue_size(), 6)

        self.assertEqual([self.s.dequeue() for _ in range(6)],
                         [b'a0', b'b0', b'a1', b'a2', b'b1', b'a3'])
        self.assertTrue(self.s.dequeue() is None)
        self.assertEqual(self.s.queue_size(), 0)

    def test_priority_and_fifo_within_group(self):
        # Group scheduling only affects the ordering *between* groups. Within
        # a group, tasks are still ordered by priority, then FIFO.
        self.s = self.new_storage({'a': 1, 'b': 1})
        self.s.enqueue(b'a-p0', priority=0, group='a')
        self.s.enqueue(b'a-p2', priority=2, group='a')
        self.s.enqueue(b'a-p1', priority=1, group='a')
        self.s.enqueue(b'b0', group='b')
        self.s.enqueue(b'b1', group='b')

        self.assertEqual([self.s.dequeue() for _ in range(5)],
                         [b'a-p2', b'b0', b'a-p1', b'b1', b'a-p0'])

    def test_long_term_ratio(self):
        # With both groups continuously active, the long-term ratio of tasks
        # dequeued matches the configured weights exactly.
        self.enqueue_n('a', 200)
        self.enqueue_n('b', 100)
        groups = self.dequeue_groups(300)
        self.assertEqual(groups, ['a', 'b', 'a'] * 100)
        self.assertEqual(groups.count('a'), 200)
        self.assertEqual(groups.count('b'), 100)

    def test_short_term_wait_bound(self):
        # The smooth weighted round-robin scheduler provides a provable
        # short-term fairness bound. Writing cw_i(t) = w_i * t - T * N_i(t)
        # (where N_i(t) is the number of times group i was selected in the
        # first t dequeues and T is the total weight), the selected group
        # always has the maximum cw, and the maximum is at least the mean,
        # so cw_i(t) > -T at all times. This gives the wait bound:
        #
        #     N_i(t) > t * w_i / T - 1   for every prefix t
        #
        # i.e. no group can ever fall a full "expected turn" behind.
        weights = {'a': 5, 'b': 2, 'c': 1}
        self.s = self.new_storage(weights)
        self.enqueue_n('a', 500)
        self.enqueue_n('b', 200)
        self.enqueue_n('c', 100)

        total = sum(weights.values())
        groups = self.dequeue_groups(800)

        counts = dict((g, 0) for g in weights)
        first = {}
        for t, group in enumerate(groups, 1):
            counts[group] += 1
            first.setdefault(group, t)
            for g, w in weights.items():
                self.assertTrue(counts[g] >= -(-t * w // total) - 1,
                                'group %s behind at t=%s' % (g, t))

        # A group is never starved for long: from a fresh state, every group
        # is selected within ceil(total_weight / w_i) dequeues.
        for g, w in weights.items():
            self.assertTrue(first[g] <= -(-total // w),
                            'group %s first selected at %s' % (g, first[g]))

        # Exact long-term ratio.
        self.assertEqual(counts, {'a': 500, 'b': 200, 'c': 100})

    def test_empty_group_no_wasted_turns(self):
        # A group with no tasks does not consume any scheduling turns.
        self.enqueue_n('a', 5)
        self.assertEqual(self.dequeue_groups(5), ['a'] * 5)
        self.assertTrue(self.s.dequeue() is None)

    def test_reactivation_no_accumulated_quota(self):
        # A group that becomes active again does not accumulate quota for
        # the time it was not active.
        self.enqueue_n('a', 30)
        self.assertEqual(len(self.dequeue_groups(20)), 20)

        # "b" appears for the first time after many a-only rounds. It must
        # not receive a burst of turns; scheduling starts fresh.
        self.enqueue_n('a', 10)
        self.enqueue_n('b', 3)
        self.assertEqual(self.dequeue_groups(6), ['a', 'b', 'a', 'a', 'b',
                                                  'a'])

    def test_reactivation_after_empty(self):
        # Drain everything, then verify scheduling restarts from a clean
        # slate (no historical credit or debt carried over).
        self.enqueue_n('a', 3)
        self.enqueue_n('b', 3)
        self.dequeue_groups(6)
        self.assertEqual(self.s.queue_size(), 0)

        self.enqueue_n('a', 4)
        self.enqueue_n('b', 2)
        self.assertEqual(self.dequeue_groups(6),
                         ['a', 'b', 'a', 'a', 'b', 'a'])

    def test_ungrouped_tasks_default_group(self):
        # Tasks without a group form a default group with weight 1 (or
        # default_group_weight, if specified).
        self.s = self.new_storage({'a': 3})
        self.s.enqueue(b'u0')
        self.s.enqueue(b'u1')
        self.enqueue_n('a', 6)
        self.assertEqual([self.s.dequeue() for _ in range(8)],
                         [b'a0', b'u0', b'a1', b'a2', b'a3', b'u1', b'a4',
                          b'a5'])

    def test_default_group_weight(self):
        self.s = self.new_storage({'a': 1}, default_group_weight=3)
        self.s.enqueue(b'u0')
        self.s.enqueue(b'u1')
        self.enqueue_n('a', 2)
        # Weights: default=3, a=1.
        self.assertEqual([self.s.dequeue() for _ in range(4)],
                         [b'u0', b'u1', b'a0', b'a1'])

    def test_flush_queue_resets_groups(self):
        self.enqueue_n('a', 3)
        self.enqueue_n('b', 3)
        self.s.flush_queue()
        self.assertEqual(self.s.queue_size(), 0)
        self.assertEqual(self.s.enqueued_items(), [])
        self.assertTrue(self.s.dequeue() is None)

        # Scheduling restarts cleanly after a flush.
        self.enqueue_n('a', 4)
        self.enqueue_n('b', 2)
        self.assertEqual(self.dequeue_groups(6),
                         ['a', 'b', 'a', 'a', 'b', 'a'])

    def test_group_validation(self):
        self.assertRaises(ValueError, self.s.enqueue, b'x', None, '')
        self.assertRaises(ValueError, self.s.enqueue, b'x', None, 123)

    def test_multithreaded_dequeue(self):
        # Selection and removal happen as a single storage action, so
        # competing consumers cannot claim the same task.
        self.enqueue_n('a', 200)
        self.enqueue_n('b', 100)
        self.enqueue_n(None, 100)

        out_q = Queue()

        def dequeue_tasks():
            while True:
                data = self.s.dequeue()
                if data is None:
                    break
                out_q.put(data)

        threads = [threading.Thread(target=dequeue_tasks)
                   for _ in range(8)]
        for t in threads: t.start()
        for t in threads: t.join(timeout=10.)

        self.assertEqual(self.s.queue_size(), 0)
        seen = list(out_q.get() for _ in range(out_q.qsize()))
        # No duplicates and nothing lost: 400 unique tasks.
        self.assertEqual(len(seen), 400)
        self.assertEqual(len(set(seen)), 400)


class TestMemoryGroupScheduling(GroupSchedulingTests, BaseTestCase):
    def get_huey(self):
        return MemoryHuey(utc=False, group_weights=self.weights)

    def new_storage(self, weights, default_group_weight=1):
        storage = MemoryHuey(utc=False, group_weights=weights,
                             default_group_weight=default_group_weight).storage
        self._extra.append(storage)
        return storage


class TestSqliteGroupScheduling(GroupSchedulingTests, BaseTestCase):
    filename = 'huey_groups.db'

    def get_huey(self):
        return SqliteHuey(utc=False, filename=self.filename,
                          group_weights=self.weights)

    def new_storage(self, weights, default_group_weight=1):
        storage = SqliteHuey(utc=False, filename=self.filename,
                             group_weights=weights,
                             default_group_weight=default_group_weight).storage
        self._extra.append(storage)
        return storage

    def tearDown(self):
        super(TestSqliteGroupScheduling, self).tearDown()
        self.huey.storage.close()
        if os.path.exists(self.filename):
            os.unlink(self.filename)

    def test_scheduler_state_shared(self):
        # The scheduler state is stored in the database, so multiple storage
        # instances (e.g. separate consumer processes) make consistent
        # scheduling decisions and cannot claim the same task.
        other = SqliteHuey(name=self.huey.name, utc=False,
                           filename=self.filename,
                           group_weights=self.weights).storage
        self._extra.append(other)
        self.enqueue_n('a', 4)
        self.enqueue_n('b', 2)
        expected = [b'a0', b'b0', b'a1', b'a2', b'b1', b'a3']
        for i in range(0, 6, 2):
            # Alternate between the two storage handles.
            self.assertEqual(self.s.dequeue(), expected[i])
            self.assertEqual(other.dequeue(), expected[i + 1])

    def test_migration_existing_database(self):
        # Databases created before group scheduling was enabled are migrated
        # transparently: the grp column is added and pending tasks are
        # treated as belonging to the default group.
        self.huey.storage.close()
        if os.path.exists(self.filename):
            os.unlink(self.filename)

        # Simulate a database created by an older version (no grp column).
        conn = sqlite3.connect(self.filename)
        conn.execute('create table task ('
                     'id integer not null primary key, queue text not null, '
                     'data blob not null, priority real not null default 0.0)')
        conn.execute('insert into task (queue, data, priority) '
                     'values (?, ?, ?)', (self.huey.name, b'old1', 5))
        conn.execute('insert into task (queue, data, priority) '
                     'values (?, ?, ?)', (self.huey.name, b'old2', 0))
        conn.commit()
        conn.close()

        grouped = SqliteHuey(name=self.huey.name, utc=False,
                             filename=self.filename, group_weights={'a': 1})
        self._extra.append(grouped.storage)
        s = grouped.storage

        # Legacy rows (grp was NULL) are visible as the default group.
        s.enqueue(b'a0', group='a')
        self.assertEqual(s.queue_size(), 3)
        self.assertEqual(s.dequeue(), b'old1')
        self.assertEqual(s.dequeue(), b'a0')
        self.assertEqual(s.dequeue(), b'old2')
        self.assertTrue(s.dequeue() is None)


class TestGroupApi(BaseTestCase):
    def get_huey(self):
        return MemoryHuey(utc=False, group_weights={'a': 1, 'b': 1})

    def test_group_requires_configuration(self):
        huey = MemoryHuey(utc=False)  # No group_weights configured.

        @huey.task()
        def task_g():
            return True

        self.assertRaises(ConfigurationError, task_g, group='a')
        self.assertRaises(ConfigurationError,
                          huey.storage.enqueue, b'x', 0, 'a')

    def test_invalid_weights(self):
        self.assertRaises(ValueError, MemoryHuey, group_weights={'a': 0})
        self.assertRaises(ValueError, MemoryHuey, group_weights={'a': -1})
        self.assertRaises(ValueError, MemoryHuey, group_weights={1: 1})
        self.assertRaises(ValueError, MemoryHuey,
                          group_weights={'a': 1}, default_group_weight=0)

    def test_group_declaration_and_override(self):
        @self.huey.task(group='a')
        def task_a(n):
            return n

        @self.huey.task()
        def task_n(n):
            return n

        task_a(1)
        task_a(2, group='b')  # Per-call override.
        task_n(3)  # No group (default group, weight 1).

        groups = [self.huey.dequeue().group for _ in range(3)]
        self.assertEqual(groups, [None, 'a', 'b'])

    def test_group_survives_serialization(self):
        @self.huey.task(group='b')
        def task_b(n):
            return n

        r = task_b(1)
        task = self.huey.dequeue()
        self.assertEqual(task.id, r.id)
        self.assertEqual(task.group, 'b')

    def test_group_retry(self):
        state = []

        @self.huey.task(group='a')
        def task_a(n):
            state.append(n)
            return n

        @self.huey.task(group='b', retries=1)
        def task_b(n):
            state.append(n)
            if n < 0:
                raise ValueError('uh-oh')
            return n

        task_a(1)
        r1 = task_b(-1)
        task_a(2)

        # Dequeue order alternates between the equally-weighted groups.
        t1 = self.huey.dequeue()
        self.assertEqual(t1.group, 'a')
        self.assertEqual(self.huey.execute(t1), 1)

        t2 = self.huey.dequeue()
        self.assertEqual(t2.group, 'b')
        self.assertEqual(t2.id, r1.id)
        self.assertTrue(self.huey.execute(t2) is None)  # Raises, re-queued.

        t3 = self.huey.dequeue()
        self.assertEqual(t3.group, 'a')
        self.assertEqual(self.huey.execute(t3), 2)

        # The retried task remains in its original group.
        t4 = self.huey.dequeue()
        self.assertEqual(t4.group, 'b')
        self.assertEqual(t4.id, r1.id)
        self.assertEqual(t4.retries, 0)
        self.assertTrue(self.huey.execute(t4) is None)
        self.assertRaises(TaskException, r1.get)

        self.assertEqual(state, [1, -1, 2, -1])

    def test_group_retry_delay(self):
        # A retry with a delay goes through the schedule and only
        # participates in group scheduling once the delay has elapsed.
        @self.huey.task(group='b', retries=1, retry_delay=60)
        def task_b():
            raise ValueError('uh-oh')

        @self.huey.task(group='a')
        def task_a(n):
            return n

        task_b()
        task_a(1)
        task_a(2)

        t1 = self.huey.dequeue()
        self.assertEqual(t1.group, 'a')
        t2 = self.huey.dequeue()
        self.assertEqual(t2.group, 'b')
        self.assertTrue(self.huey.execute(t2) is None)  # Re-queued w/ delay.

        # The delayed retry is in the schedule and does not participate.
        self.assertEqual(self.huey.scheduled_count(), 1)
        t3 = self.huey.dequeue()
        self.assertEqual(t3.group, 'a')
        self.assertTrue(self.huey.dequeue() is None)

        # Once the delay elapses, the task is read from the schedule and
        # re-enqueued into its original group.
        eta = datetime.datetime.now() + datetime.timedelta(seconds=61)
        tasks = self.huey.read_schedule(eta)
        self.assertEqual(len(tasks), 1)
        self.assertEqual(tasks[0].group, 'b')
        self.huey.enqueue(tasks[0])
        t4 = self.huey.dequeue()
        self.assertEqual(t4.group, 'b')
        self.assertEqual(t4.id, t2.id)

    def test_group_scheduled_task(self):
        # Delayed tasks only participate in group scheduling once their eta
        # has arrived.
        @self.huey.task(group='b')
        def task_b(n):
            return n

        @self.huey.task(group='a')
        def task_a(n):
            return n

        eta = datetime.datetime.now() + datetime.timedelta(seconds=60)
        rb = task_b.schedule(('b',), eta=eta)
        task_a(1)

        t1 = self.huey.dequeue()
        self.assertEqual(t1.group, 'a')
        self.assertEqual(self.huey.execute(t1), 1)

        # The scheduled task is pulled from the queue but, not being ready,
        # is moved to the schedule when executed.
        t2 = self.huey.dequeue()
        self.assertEqual(t2.group, 'b')
        self.assertTrue(self.huey.execute(t2) is None)  # Moved to schedule.
        self.assertTrue(self.huey.dequeue() is None)

        # The group is preserved while the task sits in the schedule.
        tasks = self.huey.read_schedule(eta)
        self.assertEqual(len(tasks), 1)
        self.assertEqual(tasks[0].id, rb.id)
        self.assertEqual(tasks[0].group, 'b')

        # Once due, the task participates in its original group again.
        self.huey.enqueue(tasks[0])
        task_a(2)
        groups = [self.huey.dequeue().group for _ in range(2)]
        self.assertEqual(groups, ['a', 'b'])

    def test_schedule_kwarg_group(self):
        @self.huey.task()
        def task_n(n):
            return n

        eta = datetime.datetime.now() + datetime.timedelta(seconds=60)
        r = task_n.schedule(('x',), eta=eta, group='b')
        task = self.huey.dequeue()
        self.assertEqual(task.id, r.id)
        self.assertEqual(task.group, 'b')

    def test_reschedule_preserves_group(self):
        @self.huey.task(group='b')
        def task_b(n):
            return n

        res = task_b(1)
        eta = datetime.datetime.now() + datetime.timedelta(seconds=60)
        res2 = res.reschedule(eta=eta)
        self.assertEqual(len(self.huey), 2)

        # The original task was revoked and is discarded when executed.
        self.assertTrue(self.execute_next() is None)

        task = self.huey.dequeue()
        self.assertEqual(task.id, res2.id)
        self.assertEqual(task.group, 'b')

    def test_ungrouped_semantics_unchanged(self):
        # Without group_weights, the storage behaves as a plain priority
        # queue with FIFO for equal priorities.
        huey = MemoryHuey(utc=False)
        s = huey.storage
        s.enqueue(b'p0-a', priority=0)
        s.enqueue(b'p1', priority=1)
        s.enqueue(b'p0-b', priority=0)
        s.enqueue(b'p2', priority=2)
        self.assertEqual([s.dequeue() for _ in range(4)],
                         [b'p2', b'p1', b'p0-a', b'p0-b'])


class TestMemorySqliteConsistency(BaseTestCase):
    # Both storages implement the same scheduling algorithm and must produce
    # identical orders for an identical fixed sequence of operations.
    filename = 'huey_groups_consistency.db'

    def get_huey(self):
        return MemoryHuey(utc=False, group_weights={'a': 3, 'b': 2, 'c': 1})

    def tearDown(self):
        super(TestMemorySqliteConsistency, self).tearDown()
        if os.path.exists(self.filename):
            os.unlink(self.filename)

    def test_same_sequence(self):
        mem = self.huey.storage
        sql = SqliteHuey(utc=False, filename=self.filename,
                         group_weights={'a': 3, 'b': 2, 'c': 1}).storage
        try:
            ops = [('a', 5), ('b', 3), (None, 2), ('c', 4), ('a', 3)]
            i = 0
            for group, n in ops:
                for _ in range(n):
                    data = b't%d' % i
                    i += 1
                    mem.enqueue(data, group=group)
                    sql.enqueue(data, group=group)

            mem_items = mem.enqueued_items()
            sql_items = sql.enqueued_items()
            self.assertEqual(mem_items, sql_items)

            mem_order = [mem.dequeue() for _ in range(i)]
            sql_order = [sql.dequeue() for _ in range(i)]
            self.assertEqual(mem_order, sql_order)
            self.assertEqual(mem_order, mem_items)
        finally:
            sql.close()
