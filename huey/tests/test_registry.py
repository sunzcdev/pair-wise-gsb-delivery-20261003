from huey.api import Task
from huey.exceptions import HueyException
from huey.registry import CURRENT_TASK_VERSION
from huey.registry import LEGACY_TASK_VERSION
from huey.registry import Message
from huey.tests.base import BaseTestCase
from huey.utils import ChordConfig


class TestRegistry(BaseTestCase):
    def setUp(self):
        super(TestRegistry, self).setUp()
        self.registry = self.huey._registry

    def test_register_unique(self):
        def task_a(): pass
        def task_b(): pass

        ta = self.huey.task()(task_a)
        self.assertRaises(ValueError, self.huey.task(), task_a)
        self.assertRaises(ValueError, self.huey.task(name='task_a'), task_b)

        # We can register task_b and re-register task_a providing a new name.
        tb = self.huey.task()(task_b)
        ta2 = self.huey.task(name='task_a2')(task_a)

        t1 = ta.s()
        t2 = ta2.s()
        self.assertTrue(t1.name != t2.name)

    def test_register_unregister(self):
        @self.huey.task()
        def task_a():
            pass

        self.assertTrue(task_a.unregister())
        self.assertFalse(task_a.unregister())

    def test_message_wrapping(self):
        @self.huey.task(retries=1)
        def task_a(p1, p2, p3=3, p4=None):
            pass

        task = task_a.s('v1', 'v2', p4='v4')
        message = self.registry.create_message(task)
        self.assertEqual(message.id, task.id)
        self.assertEqual(message.retries, 1)
        self.assertEqual(message.retry_delay, 0)
        self.assertEqual(message.args, ('v1', 'v2'))
        self.assertEqual(message.kwargs, {'p4': 'v4'})
        self.assertTrue(message.on_complete is None)
        self.assertTrue(message.on_error is None)
        self.assertTrue(message.expires is None)
        self.assertTrue(message.expires_resolved is None)

        task2 = self.registry.create_task(message)
        self.assertEqual(task2.id, task.id)
        self.assertEqual(task2.retries, 1)
        self.assertEqual(task2.retry_delay, 0)
        self.assertEqual(task2.args, ('v1', 'v2'))
        self.assertEqual(task2.kwargs, {'p4': 'v4'})
        self.assertTrue(task2.on_complete is None)
        self.assertTrue(task2.on_error is None)
        self.assertTrue(task2.expires is None)
        self.assertTrue(task2.expires_resolved is None)

    def test_user_task_kwarg_preserved(self):
        @self.huey.task()
        def task_t(**kwargs):
            return kwargs

        task = task_t.s(task='user-value')
        message = self.registry.create_message(task)
        self.assertEqual(message.kwargs, {'task': 'user-value'})

        task2 = self.registry.create_task(message)
        self.assertEqual(task2.kwargs, {'task': 'user-value'})
        self.assertEqual(task2.execute(), {'task': 'user-value'})

    def test_context_task_reserialization(self):
        @self.huey.task(context=True)
        def task_c(n, task=None):
            return (n, task.id)

        t = task_c.s(3)
        self.assertEqual(t.execute(), (3, t.id))

        # Executing a context task does not pollute its kwargs, so it can be
        # re-serialized (e.g. for retry) without leaking the task instance.
        self.assertEqual(t.kwargs, {})
        message = self.registry.create_message(t)
        self.assertEqual(message.kwargs, {})

    def test_missing_task(self):
        @self.huey.task()
        def task_a():
            pass

        # Serialize the task invocation.
        task = task_a.s()
        message = self.registry.create_message(task)

        # Unregister the task, which will raise an error when we try to
        # deserialize the message back into a task instance.
        self.assertTrue(task_a.unregister())
        self.assertRaises(HueyException, self.registry.create_task, message)

        # Similarly, we can no longer serialize the task to a message.
        self.assertRaises(HueyException, self.registry.create_message, task)

    def test_periodic_tasks(self):
        def task_fn(): pass
        self.huey.task(name='a')(task_fn)
        p1 = self.huey.periodic_task(lambda _: False, name='p1')(task_fn)
        p2 = self.huey.periodic_task(lambda _: False, name='p2')(task_fn)
        self.huey.task(name='b')(task_fn)

        periodic = sorted(t.name for t in self.registry.periodic_tasks)
        self.assertEqual(periodic, ['p1', 'p2'])

        self.assertTrue(p1.unregister())
        periodic = sorted(t.name for t in self.registry.periodic_tasks)
        self.assertEqual(periodic, ['p2'])


def make_message(name, args=(), kwargs=None, version=CURRENT_TASK_VERSION,
                 on_complete=None, on_error=None, chord_config=None):
    return Message(id=None, name=name, eta=None, retries=0, retry_delay=0,
                   priority=None, args=tuple(args),
                   kwargs={} if kwargs is None else kwargs,
                   on_complete=on_complete, on_error=on_error,
                   expires=None, expires_resolved=None, timeout=None,
                   chord_config=chord_config, retry_backoff=None,
                   version=version)


class TestTaskAliases(BaseTestCase):
    def setUp(self):
        super(TestTaskAliases, self).setUp()
        self.registry = self.huey._registry

        @self.huey.task()
        def renamed_task(value):
            return ('new', value)

        self.renamed_task = renamed_task
        self.canonical = self.registry.task_to_string(
            renamed_task.task_class)

    def test_new_messages_use_current_name_and_version(self):
        task = self.renamed_task.s(1)
        message = self.registry.create_message(task)
        self.assertEqual(message.name, self.canonical)
        self.assertEqual(message.version, CURRENT_TASK_VERSION)

    def test_alias_resolves_old_name(self):
        self.registry.add_alias(self.renamed_task, 'legacy.module.old_fn')
        old_message = make_message('legacy.module.old_fn', args=(42,),
                                   version=CURRENT_TASK_VERSION)

        self.assertEqual(self.registry.resolve_name('legacy.module.old_fn'),
                         self.canonical)
        task = self.registry.create_task(old_message)
        self.assertEqual(type(task), self.renamed_task.task_class)
        self.assertEqual(task.execute(), ('new', 42))

    def test_alias_accepts_task_wrapper_and_class(self):
        self.registry.add_alias(self.renamed_task.task_class, 'a.b')
        self.registry.add_alias(self.renamed_task, 'c.d')
        self.assertEqual(self.registry.resolve_name('a.b'), self.canonical)
        self.assertEqual(self.registry.resolve_name('c.d'), self.canonical)

    def test_alias_cannot_shadow_registered_task(self):
        # The canonical name of a real task may not be claimed as an alias.
        with self.assertRaises(ValueError):
            self.registry.add_alias(self.renamed_task, self.canonical)

        @self.huey.task()
        def other_task():
            pass
        other_name = self.registry.task_to_string(other_task.task_class)
        with self.assertRaises(ValueError):
            self.registry.add_alias(self.renamed_task, other_name)

    def test_alias_cannot_target_unregistered_task(self):
        class Ghost(Task):
            pass
        Ghost.__module__ = 'ghost'
        with self.assertRaises(HueyException):
            self.registry.add_alias('ghost.Ghost', 'old.ghost')

    def test_aliases_cannot_form_chain_or_cycle(self):
        # An alias may not reuse another alias's name (it would become an
        # alias-of-an-alias and open the door to cycles).
        self.registry.add_alias(self.renamed_task, 'legacy.one')

        @self.huey.task()
        def other_task():
            pass
        with self.assertRaises(ValueError):
            self.registry.add_alias(other_task, 'legacy.one')

        # Re-registering the same alias for the same target is idempotent.
        self.registry.add_alias(self.renamed_task, 'legacy.one')

        # An alias name cannot later be registered as a real task with the
        # same fully-qualified identifier.
        class Collision(Task):
            pass
        Collision.__module__ = 'legacy'
        Collision.__name__ = Collision.__qualname__ = 'one'
        with self.assertRaises(ValueError):
            self.registry.register(Collision)

    def test_unknown_name_raises_without_importing(self):
        # Resolution must rely solely on the registry, never dynamic imports.
        with self.assertRaises(HueyException):
            self.registry.resolve_name('some.uninstalled.module.fn')
        with self.assertRaises(HueyException):
            self.registry.create_task(
                make_message('some.uninstalled.module.fn'))

    def test_remove_alias_and_unregister_cleans_up(self):
        self.registry.add_alias(self.renamed_task, 'legacy.one')
        self.assertTrue(self.registry.remove_alias('legacy.one'))
        self.assertFalse(self.registry.remove_alias('legacy.one'))
        with self.assertRaises(HueyException):
            self.registry.resolve_name('legacy.one')

        self.registry.add_alias(self.renamed_task, 'legacy.two')
        self.renamed_task.unregister()
        with self.assertRaises(HueyException):
            self.registry.resolve_name('legacy.two')

    def test_alias_on_complete_on_error_and_chord(self):
        @self.huey.task()
        def pipeline_task(v):
            return v + 1

        @self.huey.task()
        def error_task(exc):
            return ('error', exc)

        @self.huey.task()
        def chord_callback(results):
            return ('chord', results)

        self.registry.add_alias(self.renamed_task, 'legacy.renamed')
        self.registry.add_alias(pipeline_task, 'legacy.pipeline')
        self.registry.add_alias(error_task, 'legacy.error')
        self.registry.add_alias(chord_callback, 'legacy.callback')

        callback_msg = make_message('legacy.callback', version=1)
        chord_config = ('cid', 1, 0, callback_msg)
        old = make_message(
            'legacy.renamed', args=(1,), version=1,
            on_complete=make_message('legacy.pipeline', version=1),
            on_error=make_message('legacy.error', version=1),
            chord_config=chord_config)

        task = self.registry.create_task(old)
        self.assertEqual(type(task), self.renamed_task.task_class)
        self.assertEqual(type(task.on_complete),
                         pipeline_task.task_class)
        self.assertEqual(type(task.on_error), error_task.task_class)
        self.assertIsInstance(task.chord_config, ChordConfig)
        self.assertEqual(type(task.chord_config.callback),
                         chord_callback.task_class)


class TestMessageMigrations(BaseTestCase):
    def setUp(self):
        super(TestMessageMigrations, self).setUp()
        self.registry = self.huey._registry

        @self.huey.task()
        def evolving_task(value, extra=None):
            return (value, extra)

        self.evolving_task = evolving_task
        self.name = self.registry.task_to_string(
            evolving_task.task_class)
        evolving_task.task_class.message_version = 2

    def _migrate_0_to_1(self, message):
        # Legacy positional value gets a default for the new ``extra`` kwarg.
        kwargs = dict(message.kwargs)
        kwargs.setdefault('extra', 'default')
        return message._replace(kwargs=kwargs)

    def _migrate_1_to_2(self, message):
        # The positional value gets doubled.
        (value,) = message.args
        return message._replace(args=(value * 2,))

    def test_legacy_unversioned_message_is_treated_as_version_zero(self):
        self.assertEqual(LEGACY_TASK_VERSION, 0)
        self.registry.add_message_migration(
            self.evolving_task, 0, 1, self._migrate_0_to_1)
        self.registry.add_message_migration(
            self.evolving_task, 1, 2, self._migrate_1_to_2)

        legacy = make_message(self.name, args=(3,), version=None)
        upgraded = self.registry.upgrade_message(legacy)
        self.assertEqual(upgraded.version, 2)
        self.assertEqual(upgraded.name, self.name)
        self.assertEqual(upgraded.args, (6,))
        self.assertEqual(upgraded.kwargs, {'extra': 'default'})

        task = self.registry.create_task(legacy)
        self.assertEqual(task.execute(), (6, 'default'))

    def test_explicit_legacy_version_uses_same_path(self):
        self.registry.add_message_migration(
            self.evolving_task, 0, 1, self._migrate_0_to_1)
        self.registry.add_message_migration(
            self.evolving_task, 1, 2, self._migrate_1_to_2)

        message = make_message(self.name, args=(3,),
                               version=LEGACY_TASK_VERSION)
        task = self.registry.create_task(message)
        self.assertEqual(task.execute(), (6, 'default'))

    def test_migration_only_follows_declared_edges(self):
        # Declare 0->1 but not 1->2: reaching version 2 must fail.
        self.registry.add_message_migration(
            self.evolving_task, 0, 1, self._migrate_0_to_1)
        message = make_message(self.name, args=(3,), version=1)
        with self.assertRaises(HueyException):
            self.registry.create_task(message)

        # With no edges declared at all, even version 0 cannot be upgraded.
        self.registry._migrations.pop(self.name, None)
        message = make_message(self.name, args=(3,), version=0)
        with self.assertRaises(HueyException):
            self.registry.create_task(message)

    def test_multi_step_migration_path(self):
        self.registry.add_message_migration(
            self.evolving_task, 0, 1, self._migrate_0_to_1)
        self.registry.add_message_migration(
            self.evolving_task, 1, 2, self._migrate_1_to_2)
        message = make_message(self.name, args=(5,), version=0)
        task = self.registry.create_task(message)
        self.assertEqual(task.execute(), (10, 'default'))

    def test_migration_cycle_is_rejected(self):
        self.registry.add_message_migration(
            self.evolving_task, 0, 1, self._migrate_0_to_1)
        self.registry.add_message_migration(
            self.evolving_task, 1, 2, self._migrate_1_to_2)
        # Closing 2 -> 0 / 2 -> 1 would make the edge graph cyclic.
        with self.assertRaises(ValueError):
            self.registry.add_message_migration(
                self.evolving_task, 2, 1, self._migrate_1_to_2)
        # The rejected edge was not stored.
        self.assertNotIn((2, 1), self.registry._migrations[self.name])

    def test_self_migration_is_rejected(self):
        with self.assertRaises(ValueError):
            self.registry.add_message_migration(
                self.evolving_task, 1, 1, self._migrate_1_to_2)

    def test_migration_for_unknown_task_is_rejected(self):
        with self.assertRaises(HueyException):
            self.registry.add_message_migration(
                'not.a.task', 0, 1, self._migrate_0_to_1)

    def test_newer_than_supported_message_is_rejected(self):
        message = make_message(self.name, args=(1,), version=99)
        with self.assertRaises(HueyException):
            self.registry.create_task(message)

    def test_migrator_error_surfaces(self):
        def broken(message):
            raise RuntimeError('boom')

        self.registry.add_message_migration(
            self.evolving_task, 0, 1, broken)
        self.registry.add_message_migration(
            self.evolving_task, 1, 2, self._migrate_1_to_2)
        message = make_message(self.name, args=(1,), version=0)
        with self.assertRaises(RuntimeError):
            self.registry.create_task(message)

    def test_alias_and_migration_combined(self):
        self.registry.add_alias(self.evolving_task, 'legacy.module.old_fn')
        self.registry.add_message_migration(
            self.evolving_task, 0, 1, self._migrate_0_to_1)
        self.registry.add_message_migration(
            self.evolving_task, 1, 2, self._migrate_1_to_2)

        old = make_message('legacy.module.old_fn', args=(4,), version=None)
        task = self.registry.create_task(old)
        self.assertEqual(type(task), self.evolving_task.task_class)
        self.assertEqual(task.execute(), (8, 'default'))

    def test_migration_nested_pipeline_messages(self):
        @self.huey.task()
        def child_task(v):
            return v

        child_task.task_class.message_version = 2
        child_name = self.registry.task_to_string(child_task.task_class)
        self.registry.add_alias(child_task, 'legacy.child')
        self.registry.add_message_migration(
            child_task, 1, 2, self._migrate_1_to_2)

        old_child = make_message('legacy.child', args=(7,), version=1)
        old_parent = make_message(self.name, args=(1,), version=2,
                                  on_complete=old_child)
        task = self.registry.create_task(old_parent)
        self.assertEqual(task.on_complete.args, (14,))
        self.assertEqual(type(task.on_complete), child_task.task_class)


class TestDeadLetters(BaseTestCase):
    def setUp(self):
        super(TestDeadLetters, self).setUp()
        self.registry = self.huey._registry

        @self.huey.task()
        def current_task(v):
            return v

        self.current_task = current_task

    def test_dequeue_unknown_task_preserves_raw_message(self):
        raw = self.huey.serializer.serialize(
            make_message('missing.module.gone', args=(1,), version=1))
        self.huey.storage.enqueue(raw)

        # Dequeue does not raise; the undeliverable message is retained.
        self.assertIsNone(self.huey.dequeue())
        dead = self.registry.dead_letters
        self.assertEqual(len(dead), 1)
        record = dead[0]
        # The exact original bytes survive -- nothing was re-serialized.
        self.assertEqual(record.raw, raw)
        self.assertIsInstance(record.error, HueyException)
        self.assertEqual(record.message.name, 'missing.module.gone')
        self.assertIsNotNone(record.timestamp)

        # Accessors and cleanup.
        self.assertEqual(self.registry.get_dead_letter(record.id), record)
        self.assertEqual(self.registry.clear_dead_letters(), 1)
        self.assertEqual(self.registry.dead_letters, [])

    def test_failed_migration_is_retained_not_dropped(self):
        self.current_task.task_class.message_version = 2

        def broken(message):
            raise ValueError('bad params')

        self.huey.add_task_migration(self.current_task, 1, 2, broken)
        name = self.registry.task_to_string(self.current_task.task_class)
        raw = self.huey.serializer.serialize(
            make_message(name, args=(1,), version=1))
        self.huey.storage.enqueue(raw)

        self.assertIsNone(self.huey.dequeue())
        record = self.registry.dead_letters[0]
        self.assertEqual(record.raw, raw)
        self.assertIsInstance(record.error, ValueError)

    def test_unserializable_bytes_are_retained(self):
        raw = b'this is not a serialized message at all'
        self.huey.storage.enqueue(raw)
        self.assertIsNone(self.huey.dequeue())
        record = self.registry.dead_letters[0]
        self.assertEqual(record.raw, raw)
        self.assertIsNone(record.message)

    def test_valid_message_does_not_create_dead_letter(self):
        self.current_task(1)
        task = self.huey.dequeue()
        self.assertEqual(task.args, (1,))
        self.assertEqual(self.registry.dead_letters, [])

    def test_roundtrip_old_message_through_queue(self):
        self.huey.add_task_alias(self.current_task, 'legacy.old_name')

        def bump(message):
            (value,) = message.args
            return message._replace(args=(value + 100,))

        self.current_task.task_class.message_version = 2
        self.huey.add_task_migration(self.current_task, 1, 2, bump)

        raw = self.huey.serializer.serialize(
            make_message('legacy.old_name', args=(1,), version=1))
        self.huey.storage.enqueue(raw)

        task = self.huey.dequeue()
        self.assertEqual(type(task), self.current_task.task_class)
        self.assertEqual(task.execute(), 101)
        self.assertEqual(self.registry.dead_letters, [])

        # Re-serialization (e.g. a retry) writes the current name/version.
        rewritten = self.registry.create_message(task)
        self.assertEqual(
            rewritten.name,
            self.registry.task_to_string(self.current_task.task_class))
        self.assertEqual(rewritten.version, 2)
