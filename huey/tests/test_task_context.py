import unittest

from huey import signals as S
from huey.api import chord
from huey.api import group
from huey.exceptions import SerializationError
from huey.tests.base import BaseTestCase


class TestTaskContext(BaseTestCase):
    def get_task(self, **kwargs):
        @self.huey.task(**kwargs)
        def task_a(a, b=0):
            return a + b
        return task_a

    def test_context_stored_separately_from_kwargs(self):
        task_a = self.get_task()
        task = task_a.s(1, b=2, context={'source': 'web', 'trace_id': 't-1'})

        # Business parameters are untouched by the context mapping.
        self.assertEqual(task.args, (1,))
        self.assertEqual(task.kwargs, {'b': 2})
        self.assertEqual(task.context,
                         {'source': 'web', 'trace_id': 't-1'})

    def test_context_copied_on_create(self):
        task_a = self.get_task()
        ctx = {'source': 'web'}
        task = task_a.s(1, context=ctx)
        ctx['source'] = 'mutated'
        self.assertEqual(task.context, {'source': 'web'})

    def test_context_survives_serialization(self):
        task_a = self.get_task()
        task = task_a.s(1, context={'source': 'cron', 'batch': 'b-9',
                                    'trace_id': 't-3'})
        data = self.huey.serialize_task(task)
        restored = self.huey.deserialize_task(data)
        self.assertEqual(restored.context,
                         {'source': 'cron', 'batch': 'b-9', 'trace_id': 't-3'})
        self.assertEqual(restored.args, (1,))
        self.assertEqual(restored.kwargs, {})

    def test_context_empty_by_default_and_not_serialized(self):
        task_a = self.get_task()
        task = task_a.s(1)
        self.assertEqual(task.context, {})
        restored = self.huey.deserialize_task(self.huey.serialize_task(task))
        self.assertEqual(restored.context, {})

    def test_nonserializable_context_rejected_before_enqueue(self):
        task_a = self.get_task()
        task = task_a.s(1, context={'callback': lambda: 1})
        self.assertRaises(SerializationError, self.huey.enqueue, task)
        # Nothing was stored in the queue.
        self.assertEqual(self.huey.pending_count(), 0)

    def test_nonserializable_context_in_pipeline_rejected(self):
        task_a = self.get_task()
        parent = task_a.s(1, context={'source': 'web'})
        child = task_a.s(2, context={'callback': lambda: 1})
        parent.then(child)
        self.assertRaises(SerializationError, self.huey.enqueue, parent)
        self.assertEqual(self.huey.pending_count(), 0)

    def test_pipeline_inherits_context(self):
        task_a = self.get_task()
        parent = task_a.s(1, context={'source': 'web', 'batch': 'b-1'})
        child = task_a.s(2)
        parent.then(child)

        self.huey.enqueue(parent)
        self.execute_next()  # Runs parent, enqueues child.

        child_task = self.huey.dequeue()
        self.assertEqual(child_task.context, {'source': 'web', 'batch': 'b-1'})
        # Parent mapping is unchanged and not shared with the child.
        self.assertEqual(parent.context, {'source': 'web', 'batch': 'b-1'})
        self.assertFalse(child_task.context is parent.context)

    def test_pipeline_child_explicit_override(self):
        task_a = self.get_task()
        parent = task_a.s(1, context={'source': 'web', 'batch': 'b-1'})
        child = task_a.s(2, context={'batch': 'b-2'})
        grandchild = task_a.s(3)
        parent.then(child)
        child.then(grandchild)

        self.huey.enqueue(parent)
        self.execute_next()  # parent -> child enqueued.
        self.execute_next()  # child -> grandchild enqueued.

        grandchild_task = self.huey.dequeue()
        # Child's explicit override only affects its own branch.
        self.assertEqual(grandchild_task.context,
                         {'source': 'web', 'batch': 'b-2'})
        self.assertEqual(parent.context, {'source': 'web', 'batch': 'b-1'})

    def test_error_handler_inherits_context(self):
        @self.huey.task()
        def failing():
            raise Exception('boom')

        task_a = self.get_task()
        task = failing.s(context={'source': 'web', 'trace_id': 't-7'})
        handler = task_a.s(0, context={'trace_id': 'override'})
        task.error(handler)

        self.huey.enqueue(task)
        self.execute_next()  # Fails, enqueues error handler.

        handler_task = self.huey.dequeue()
        self.assertEqual(handler_task.context,
                         {'source': 'web', 'trace_id': 'override'})
        # The shared on_error task's own mapping is not modified.
        self.assertEqual(handler.context, {'trace_id': 'override'})

    def test_retry_preserves_context(self):
        @self.huey.task(retries=1)
        def flaky():
            raise Exception('boom')

        task = flaky.s(context={'source': 'web', 'attempt': 1})
        self.huey.enqueue(task)
        self.execute_next()  # Fails, task requeued.

        retried = self.huey.dequeue()
        self.assertEqual(retried.context, {'source': 'web', 'attempt': 1})

    def test_chord_callback_inherits_member_context(self):
        task_a = self.get_task()
        t1 = task_a.s(1, context={'source': 'web', 'batch': 'b-1'})
        t2 = task_a.s(2, context={'batch': 'b-2', 'extra': 'x'})
        callback = task_a.s(0, context={'trace_id': 't-1'})

        self.huey.enqueue(chord([t1, t2], callback))

        # Members' own mappings are not modified by the merge.
        self.assertEqual(t1.context, {'source': 'web', 'batch': 'b-1'})
        self.assertEqual(t2.context, {'batch': 'b-2', 'extra': 'x'})

        # Execute both members; the callback is then enqueued.
        self.execute_next()
        self.execute_next()

        callback_task = self.huey.dequeue()
        self.assertEqual(callback_task.context, {
            'source': 'web',
            'batch': 'b-2',  # Later member wins among members.
            'extra': 'x',
            'trace_id': 't-1',  # Callback's explicit keys take precedence.
        })

    def test_chord_member_sibling_isolation(self):
        task_a = self.get_task()
        t1 = task_a.s(1, context={'source': 'web'})
        t2 = task_a.s(2)
        self.huey.enqueue(group([t1, t2]))
        # Sibling without context is unaffected by the other member.
        self.assertEqual(t2.context, {})

    def test_hooks_and_signals_can_read_context(self):
        task_a = self.get_task()
        seen = {}

        @self.huey.pre_execute()
        def pre(task):
            seen['pre'] = dict(task.context)

        @self.huey.post_execute()
        def post(task, value, exc):
            seen['post'] = dict(task.context)

        @self.huey.signal(S.SIGNAL_EXECUTING, S.SIGNAL_COMPLETE)
        def on_signal(signal, task, *args, **kwargs):
            seen.setdefault('signals', []).append((signal, dict(task.context)))

        task = task_a.s(1, context={'source': 'web', 'trace_id': 't-5'})
        self.huey.enqueue(task)
        self.execute_next()

        expected = {'source': 'web', 'trace_id': 't-5'}
        self.assertEqual(seen['pre'], expected)
        self.assertEqual(seen['post'], expected)
        self.assertEqual(seen['signals'],
                         [(S.SIGNAL_EXECUTING, expected),
                          (S.SIGNAL_COMPLETE, expected)])

    def test_get_context_selects_fields_for_logging(self):
        task_a = self.get_task()
        task = task_a.s(1, context={'source': 'web', 'batch': 'b-1',
                                    'trace_id': 't-1'})
        # Caller selects exactly which fields to log.
        self.assertEqual(task.get_context('trace_id'), {'trace_id': 't-1'})
        self.assertEqual(task.get_context('source', 'missing'),
                         {'source': 'web'})
        # No keys -> full copy; mutating it does not affect the task.
        full = task.get_context()
        self.assertEqual(full, task.context)
        full['source'] = 'mutated'
        self.assertEqual(task.context['source'], 'web')

    def test_repr_does_not_include_context(self):
        task_a = self.get_task()
        task = task_a.s(1, context={'source': 'web', 'trace_id': 't-1'})
        self.assertFalse('web' in repr(task))
        self.assertFalse('t-1' in repr(task))

    def test_schedule_accepts_context(self):
        task_a = self.get_task()
        task_a.schedule(args=(1,), delay=60, context={'source': 'scheduler'})
        pending = self.huey.pending()
        self.assertEqual(len(pending), 1)
        self.assertEqual(pending[0].context, {'source': 'scheduler'})

    def test_reschedule_preserves_context(self):
        task_a = self.get_task()
        task = task_a.s(1, context={'source': 'web'})
        result = self.huey.enqueue(task)
        new_result = result.reschedule(delay=60)
        # The original message is still pending (it is revoked by key), so
        # the rescheduled task is the last item in the queue.
        pending = self.huey.pending()
        self.assertEqual(len(pending), 2)
        self.assertEqual(pending[-1].context, {'source': 'web'})


if __name__ == '__main__':
    unittest.main()
