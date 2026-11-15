import os
import shutil
import sqlite3
import tempfile
import time
import unittest

from huey.api import MemoryHuey
from huey.api import SqliteHuey
from huey.deadletter import MemoryDeadLetterStorage
from huey.deadletter import SqliteDeadLetterStorage
from huey.deadletter import STAGE_DESERIALIZE
from huey.deadletter import STAGE_REGISTRY
from huey.deadletter import STATUS_REJECTED
from huey.deadletter import STATUS_REPLAYED
from huey.exceptions import ConfigurationError
from huey.tests.base import BaseTestCase


CORRUPT = b'\x80\x04-corrupt-payload-'


class DeadLetterTestMixin(object):
    """Shared behaviors, parameterized through ``get_huey()``."""

    def make_unknown_bytes(self, task):
        message = self.huey._registry.create_message(task)
        return self.huey.serializer.serialize(
            message._replace(name='missing.module.unknown_task'))

    def test_disabled_by_default(self):
        huey = MemoryHuey(utc=False)
        self.assertIsNone(huey.dead_letter)
        # Historical behavior: the corrupt message is logged and dropped.
        huey.storage.enqueue(CORRUPT)
        with self.assertLogs('huey', 'ERROR'):
            self.assertIsNone(huey.dequeue())
        self.assertEqual(huey.pending_count(), 0)

    def test_invalid_configuration(self):
        with self.assertRaises(ConfigurationError):
            MemoryHuey(utc=False, dead_letter='not-a-store')

    def test_bad_then_good_keeps_processing(self):
        @self.huey.task()
        def add(a, b):
            return a + b

        # One corrupt message and one referencing an unregistered task name.
        self.huey.storage.enqueue(CORRUPT)
        unknown_bytes = self.make_unknown_bytes(add.s(1, 1))
        self.huey.storage.enqueue(unknown_bytes)

        # A perfectly valid task behind the two bad ones.
        result = add(40, 2)

        self.assertIsNone(self.huey.dequeue())
        self.assertIsNone(self.huey.dequeue())
        good = self.huey.dequeue()
        self.assertIsNotNone(good)
        self.assertEqual(self.huey.execute(good), 42)

        letters = self.huey.dead_letters()
        self.assertEqual(len(letters), 2)
        by_stage = {dl.stage: dl for dl in letters}
        self.assertEqual(sorted(by_stage),
                         [STAGE_DESERIALIZE, STAGE_REGISTRY])

        # Original bytes, source queue, failure stage and receipt time kept.
        dl = by_stage[STAGE_DESERIALIZE]
        self.assertEqual(dl.data, CORRUPT)
        self.assertEqual(dl.queue, self.huey.storage.name)
        self.assertIn('UnpicklingError', dl.error)
        self.assertGreater(dl.enqueued_at, 0)
        self.assertGreaterEqual(dl.rejected_at, dl.enqueued_at)
        self.assertEqual(dl.status, STATUS_REJECTED)
        self.assertIsNone(dl.origin_id)

        rl = by_stage[STAGE_REGISTRY]
        self.assertEqual(rl.data, unknown_bytes)
        self.assertIn('not found in TaskRegistry', rl.error)

        # Rejections never share the normal result key/value store: the one
        # successful task wrote exactly one result, and the rejection records
        # are not reachable through the result-store API. (Check before reading
        # the result, since get() is a destructive pop on these backends.)
        self.assertEqual(self.huey.result_count(), 1)
        result_keys = set(self.huey.all_results().keys())
        self.assertNotIn(dl.id, result_keys)
        self.assertNotIn(rl.id, result_keys)

        self.assertEqual(result.get(), 42)

    def test_dead_letter_write_failure_is_safe(self):
        @self.huey.task()
        def add(a, b):
            return a + b

        class FailingStorage(MemoryDeadLetterStorage):
            def add(self, *args, **kwargs):
                raise RuntimeError('dead-letter disk full')

        self.huey.dead_letter = FailingStorage()
        self.huey.storage.enqueue(CORRUPT)

        # The write failure must not be reported as a safe save: it raises.
        with self.assertRaises(RuntimeError):
            self.huey.dequeue()

        # And the message is returned to the source queue, never lost.
        self.assertEqual(self.huey.pending_count(), 1)
        self.assertEqual(self.huey.storage.dequeue(), CORRUPT)
        self.assertEqual(self.huey.dead_letter_count(), 0)

    def test_replay_success_after_registry_fixed(self):
        @self.huey.task()
        def add(a, b):
            return a + b

        missing = '%s.future_task' % __name__
        message = self.huey._registry.create_message(add.s(5, 6))
        bad = self.huey.serializer.serialize(message._replace(name=missing))
        self.huey.storage.enqueue(bad)

        self.assertIsNone(self.huey.dequeue())
        original = self.huey.dead_letters(include_replayed=False)[0]

        # "Fix" the registry by registering the task under that exact name.
        @self.huey.task(name='future_task')
        def future_task(a, b):
            return a * b
        self.assertEqual(
            self.huey._registry.task_to_string(future_task.task_class),
            missing)

        # Replay must be triggered explicitly.
        self.assertEqual(self.huey.replay_dead_letter(original.id),
                         original.id)

        # Original bytes re-enter the queue; args are not rewritten and the
        # normal registry lookup (validation) runs again.
        task = self.huey.dequeue()
        self.assertIsNotNone(task)
        self.assertEqual(task.args, (5, 6))
        self.assertEqual(self.huey.execute(task), 30)

        record = self.huey.get_dead_letter(original.id)
        self.assertEqual(record.status, STATUS_REPLAYED)
        self.assertIsNotNone(record.replayed_at)
        # Original rejection retained for audit, no new record on success.
        self.assertEqual(self.huey.dead_letter_count(), 1)
        self.assertEqual(self.huey.dead_letter_count(include_replayed=False),
                         0)

    def test_unregister_then_reregister_and_replay(self):
        # Mirrors a real deployment: the task disappears from the registry,
        # its in-flight message is rejected, then the task is redeployed and
        # the old message is explicitly replayed.
        @self.huey.task()
        def multiply(a, b):
            return a * b

        payload = self.huey.serialize_task(multiply.s(6, 7))
        self.assertTrue(multiply.unregister())

        self.huey.storage.enqueue(payload)
        self.assertIsNone(self.huey.dequeue())
        rejection = self.huey.dead_letters(include_replayed=False)[0]
        self.assertEqual(rejection.stage, STAGE_REGISTRY)

        # Redeploy: re-register a task under the same fully-qualified name.
        @self.huey.task(name='multiply')
        def multiply_restored(a, b):
            return a * b

        self.huey.replay_dead_letter(rejection.id)
        task = self.huey.dequeue()
        self.assertIsNotNone(task)
        # Original arguments survive untouched and validation runs for real.
        self.assertEqual(task.args, (6, 7))
        self.assertEqual(self.huey.execute(task), 42)
        self.assertEqual(
            self.huey.dead_letter_count(include_replayed=False), 0)

    def test_replay_failure_preserves_association(self):
        @self.huey.task()
        def add(a, b):
            return a + b

        bad = self.make_unknown_bytes(add.s(1, 2))
        self.huey.storage.enqueue(bad)
        self.assertIsNone(self.huey.dequeue())
        original = self.huey.dead_letters(include_replayed=False)[0]

        # Explicit replay before the registry is fixed: rejected again.
        self.huey.replay_dead_letter(original.id)
        self.assertIsNone(self.huey.dequeue())

        live = self.huey.dead_letters(include_replayed=False)
        self.assertEqual(len(live), 1)
        new_record = live[0]
        self.assertEqual(new_record.origin_id, original.id)
        self.assertEqual(new_record.data, bad)
        self.assertEqual(new_record.stage, STAGE_REGISTRY)

        self.assertEqual(self.huey.get_dead_letter(original.id).status,
                         STATUS_REPLAYED)

    def test_replay_guards(self):
        self.huey.storage.enqueue(CORRUPT)
        self.assertIsNone(self.huey.dequeue())
        dl = self.huey.dead_letters()[0]

        with self.assertRaises(KeyError):
            self.huey.replay_dead_letter('unknown-id')

        self.huey.replay_dead_letter(dl.id)
        with self.assertRaises(KeyError):
            self.huey.replay_dead_letter(dl.id)  # Not replayed twice.

    def test_batch_replay(self):
        for i in range(3):
            self.huey.storage.enqueue(('b%d' % i).encode())
            self.assertIsNone(self.huey.dequeue())
        ids = {dl.id for dl in
               self.huey.dead_letters(include_replayed=False)}
        replayed = self.huey.replay_dead_letters()
        self.assertEqual(set(replayed), ids)
        self.assertEqual(self.huey.pending_count(), 3)
        self.assertEqual(
            self.huey.dead_letter_count(include_replayed=False), 0)

    def test_pending_association_lifecycle(self):
        # The in-flight association survives a successful replay (so a later
        # re-rejection stays linkable) but is removed once its origin record
        # is deleted, keeping the bookkeeping bounded.
        @self.huey.task()
        def add(a, b):
            return a + b
        bad = self.make_unknown_bytes(add.s(1, 2))
        data_hash = self.huey._dead_letter_hash(bad)

        self.huey.storage.enqueue(bad)
        self.assertIsNone(self.huey.dequeue())
        record = self.huey.dead_letters(include_replayed=False)[0]
        self.assertIsNone(
            self.huey.dead_letter.pending_origin(data_hash))

        self.huey.replay_dead_letter(record.id)
        self.assertEqual(
            self.huey.dead_letter.pending_origin(data_hash), record.id)

        self.assertTrue(self.huey.delete_dead_letter(record.id))
        self.huey.prune_dead_letters()
        self.assertIsNone(
            self.huey.dead_letter.pending_origin(data_hash))

    def test_delete_and_flush_isolation(self):
        for i in range(3):
            self.huey.storage.enqueue(('b%d' % i).encode())
            self.assertIsNone(self.huey.dequeue())
        target = self.huey.dead_letters()[0]
        self.assertTrue(self.huey.delete_dead_letter(target.id))
        self.assertFalse(self.huey.delete_dead_letter(target.id))
        self.assertEqual(self.huey.dead_letter_count(), 2)

        # Normal flush must not touch the independent dead-letter store.
        self.huey.flush()
        self.assertEqual(self.huey.dead_letter_count(), 2)

        self.huey.flush_dead_letters()
        self.assertEqual(self.huey.dead_letter_count(), 0)

    def test_no_store_raises_configuration_error(self):
        huey = MemoryHuey(utc=False)
        for call in (huey.dead_letters, huey.flush_dead_letters,
                     huey.prune_dead_letters):
            with self.assertRaises(ConfigurationError):
                call()
        with self.assertRaises(ConfigurationError):
            huey.replay_dead_letter('x')
        with self.assertRaises(ConfigurationError):
            huey.get_dead_letter('x')

    def test_worker_loop_survives_bad_messages(self):
        # The consumer Worker.loop() must keep going after a rejection.
        import threading
        from huey.consumer import Worker

        @self.huey.task()
        def add(a, b):
            return a + b

        self.huey.storage.enqueue(CORRUPT)
        result = add(7, 8)
        worker = Worker(self.huey, threading.Event(), 0.001, 0.001, 1.15)
        worker.loop()  # Rejects the corrupt message, does not raise.
        worker.loop()  # Executes the good task.
        self.assertEqual(result.get(), 15)
        self.assertEqual(self.huey.dead_letter_count(), 1)


class TestMemoryDeadLetter(DeadLetterTestMixin, BaseTestCase):
    def get_huey(self):
        return MemoryHuey(utc=False, dead_letter=True)

    def test_retention_max_items(self):
        self.huey.dead_letter = MemoryDeadLetterStorage(max_items=2)
        base = time.time()
        for i in range(4):
            self.huey.dead_letter.add(
                self.huey.storage.name, ('m%d' % i).encode(),
                STAGE_DESERIALIZE, 'err', base + i, now=base + i)
        live = self.huey.dead_letters(include_replayed=False)
        self.assertEqual([dl.data for dl in live], [b'm3', b'm2'])

    def test_retention_ttl(self):
        self.huey.dead_letter = MemoryDeadLetterStorage(ttl=10)
        self.huey.storage.enqueue(b'old')
        self.assertIsNone(self.huey.dequeue())
        self.assertEqual(self.huey.dead_letter_count(), 1)
        self.huey.prune_dead_letters(now=time.time() + 11)
        self.assertEqual(self.huey.dead_letter_count(), 0)

    def test_invalid_retention_arguments(self):
        with self.assertRaises(ValueError):
            MemoryDeadLetterStorage(max_items=0)
        with self.assertRaises(ValueError):
            MemoryDeadLetterStorage(ttl=0)


class TestSqliteDeadLetter(DeadLetterTestMixin, BaseTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.mkdtemp()
        super(TestSqliteDeadLetter, self).setUp()

    def tearDown(self):
        self.huey.storage.close()
        self.huey.dead_letter.close()
        super(TestSqliteDeadLetter, self).tearDown()
        shutil.rmtree(self.tmp_dir, ignore_errors=True)

    def get_huey(self):
        self.queue_db = os.path.join(self.tmp_dir, 'queue.db')
        self.dead_db = os.path.join(self.tmp_dir, 'dead.db')
        return SqliteHuey(
            'app', filename=self.queue_db,
            dead_letter=SqliteDeadLetterStorage(filename=self.dead_db),
            utc=False)

    def test_separate_database_and_tables(self):
        self.huey.storage.enqueue(CORRUPT)
        self.assertIsNone(self.huey.dequeue())

        queue_tables = {row[0] for row in sqlite3.connect(
            self.queue_db).execute(
            "select name from sqlite_master where type='table'")}
        self.assertNotIn('dead_letter', queue_tables)

        dead_tables = {row[0] for row in sqlite3.connect(
            self.dead_db).execute(
            "select name from sqlite_master where type='table'")}
        self.assertIn('dead_letter', dead_tables)
        self.assertIn('dead_letter_pending', dead_tables)

    def test_persistence_across_instances(self):
        self.huey.storage.enqueue(b'persisted')
        self.assertIsNone(self.huey.dequeue())
        record = self.huey.dead_letters()[0]
        self.huey.dead_letter.close()

        reopened = SqliteDeadLetterStorage(filename=self.dead_db)
        try:
            self.assertEqual(reopened.count(), 1)
            loaded = reopened.list()[0]
            self.assertEqual(loaded.id, record.id)
            self.assertEqual(loaded.data, b'persisted')
            self.assertEqual(loaded.queue, 'app')
        finally:
            reopened.close()

    def test_sqlite_retention(self):
        store = SqliteDeadLetterStorage(
            filename=os.path.join(self.tmp_dir, 'r.db'), max_items=2)
        try:
            base = time.time()
            for i in range(3):
                store.add('app', ('r%d' % i).encode(), STAGE_DESERIALIZE,
                          'err', base + i, now=base + i)
            self.assertEqual(
                [dl.data for dl in store.list(include_replayed=False)],
                [b'r2', b'r1'])
        finally:
            store.close()


if __name__ == '__main__':
    unittest.main()
