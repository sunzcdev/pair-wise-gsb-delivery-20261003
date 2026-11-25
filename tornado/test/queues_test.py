# Licensed under the Apache License, Version 2.0 (the "License"); you may
# not use this file except in compliance with the License. You may obtain
# a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
# WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
# License for the specific language governing permissions and limitations
# under the License.

import asyncio
import unittest
from datetime import timedelta
from random import random
import typing

from tornado import gen, queues
from tornado.test.util import AsyncTestCase
from tornado.testing import gen_test


class QueueBasicTest(AsyncTestCase):
    def test_repr_and_str(self):
        q: queues.Queue[None] = queues.Queue(maxsize=1)
        self.assertIn(hex(id(q)), repr(q))
        self.assertNotIn(hex(id(q)), str(q))
        q.get()

        for q_str in repr(q), str(q):
            self.assertTrue(q_str.startswith("<Queue"))
            self.assertIn("maxsize=1", q_str)
            self.assertIn("getters[1]", q_str)
            self.assertNotIn("putters", q_str)
            self.assertNotIn("tasks", q_str)

        q.put(None)
        q.put(None)
        # Now the queue is full, this putter blocks.
        q.put(None)

        for q_str in repr(q), str(q):
            self.assertNotIn("getters", q_str)
            self.assertIn("putters[1]", q_str)
            self.assertIn("tasks=2", q_str)

    def test_order(self):
        q: queues.Queue[int] = queues.Queue()
        for i in [1, 3, 2]:
            q.put_nowait(i)

        items = [q.get_nowait() for _ in range(3)]
        self.assertEqual([1, 3, 2], items)

    @gen_test
    def test_maxsize(self):
        self.assertRaises(TypeError, queues.Queue, maxsize=None)
        self.assertRaises(ValueError, queues.Queue, maxsize=-1)

        q: queues.Queue[int] = queues.Queue(maxsize=2)
        self.assertTrue(q.empty())
        self.assertFalse(q.full())
        self.assertEqual(2, q.maxsize)
        self.assertTrue(q.put(0).done())
        self.assertTrue(q.put(1).done())
        self.assertFalse(q.empty())
        self.assertTrue(q.full())
        put2 = q.put(2)
        self.assertFalse(put2.done())
        self.assertEqual(0, (yield q.get()))  # Make room.
        self.assertTrue(put2.done())
        self.assertFalse(q.empty())
        self.assertTrue(q.full())


class QueueGetTest(AsyncTestCase):
    @gen_test
    def test_blocking_get(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        self.assertEqual(0, (yield q.get()))

    def test_nonblocking_get(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        self.assertEqual(0, q.get_nowait())

    def test_nonblocking_get_exception(self):
        q: queues.Queue[int] = queues.Queue()
        self.assertRaises(queues.QueueEmpty, q.get_nowait)

    @gen_test
    def test_get_with_putters(self):
        q: queues.Queue[int] = queues.Queue(1)
        q.put_nowait(0)
        put = q.put(1)
        self.assertEqual(0, (yield q.get()))
        self.assertIsNone((yield put))

    @gen_test
    def test_blocking_get_wait(self):
        q: queues.Queue[int] = queues.Queue()
        q.put(0)
        self.io_loop.call_later(0.01, q.put_nowait, 1)
        self.io_loop.call_later(0.02, q.put_nowait, 2)
        self.assertEqual(0, (yield q.get(timeout=timedelta(seconds=1))))
        self.assertEqual(1, (yield q.get(timeout=timedelta(seconds=1))))

    @gen_test
    def test_get_timeout(self):
        q: queues.Queue[int] = queues.Queue()
        get_timeout = q.get(timeout=timedelta(seconds=0.01))
        get = q.get()
        with self.assertRaises(TimeoutError):
            yield get_timeout

        q.put_nowait(0)
        self.assertEqual(0, (yield get))

    @gen_test
    def test_get_zero_timeout(self):
        q: queues.Queue[int] = queues.Queue()
        with self.assertRaises(TimeoutError):
            yield q.get(timeout=timedelta(seconds=0))
        with self.assertRaises(TimeoutError):
            yield q.get(timeout=0)

    @gen_test
    def test_get_timeout_preempted(self):
        q: queues.Queue[int] = queues.Queue()
        get = q.get(timeout=timedelta(seconds=0.01))
        q.put(0)
        yield gen.sleep(0.02)
        self.assertEqual(0, (yield get))

    @gen_test
    def test_get_clears_timed_out_putters(self):
        q: queues.Queue[int] = queues.Queue(1)
        # First putter succeeds, remainder block.
        putters = [q.put(i, timedelta(seconds=0.01)) for i in range(10)]
        put = q.put(10)
        self.assertEqual(10, len(q._putters))
        yield gen.sleep(0.02)
        self.assertEqual(10, len(q._putters))
        self.assertFalse(put.done())  # Final waiter is still active.
        q.put(11)
        self.assertEqual(0, (yield q.get()))  # get() clears the waiters.
        self.assertEqual(1, len(q._putters))
        for putter in putters[1:]:
            self.assertRaises(TimeoutError, putter.result)

    @gen_test
    def test_get_clears_timed_out_getters(self):
        q: queues.Queue[int] = queues.Queue()
        getters = [
            asyncio.ensure_future(q.get(timedelta(seconds=0.01))) for _ in range(10)
        ]
        get = asyncio.ensure_future(q.get())
        self.assertEqual(11, len(q._getters))
        yield gen.sleep(0.02)
        self.assertEqual(11, len(q._getters))
        self.assertFalse(get.done())  # Final waiter is still active.
        q.get()  # get() clears the waiters.
        self.assertEqual(2, len(q._getters))
        for getter in getters:
            self.assertRaises(TimeoutError, getter.result)

    @gen_test
    def test_async_for(self):
        q: queues.Queue[int] = queues.Queue()
        for i in range(5):
            q.put(i)

        async def f():
            results = []
            async for i in q:
                results.append(i)
                if i == 4:
                    return results

        results = yield f()
        self.assertEqual(results, list(range(5)))


class QueueBatchTest(AsyncTestCase):
    def test_invalid_arguments(self):
        q: queues.Queue[int] = queues.Queue()
        self.assertRaises(ValueError, q.get_batch, 0)
        self.assertRaises(ValueError, q.get_batch, -1)
        self.assertRaises(ValueError, q.get_batch, 2, 0)
        self.assertRaises(ValueError, q.get_batch, 2, 3)
        self.assertRaises(ValueError, q.get_batch, 1.5)  # type: ignore

    def test_immediate_batch_collects_up_to_max(self):
        q: queues.Queue[int] = queues.Queue()
        for i in range(5):
            q.put_nowait(i)
        # min_items defaults to 1, so the batch resolves immediately; it
        # collects up to max_items even though the minimum was met sooner.
        batch = q.get_batch(3)
        self.assertEqual([0, 1, 2], batch.result())
        self.assertEqual([3, 4], [q.get_nowait(), q.get_nowait()])

    def test_immediate_batch_respects_min(self):
        q: queues.Queue[int] = queues.Queue()
        for i in range(2):
            q.put_nowait(i)
        # min_items == max_items and enough items are available.
        batch = q.get_batch(2, min_items=2)
        self.assertEqual([0, 1], batch.result())
        self.assertTrue(q.empty())

    @gen_test
    def test_batch_waits_for_min_items(self):
        q: queues.Queue[int] = queues.Queue()
        batch = asyncio.ensure_future(q.get_batch(5, min_items=3))
        yield gen.moment
        self.assertFalse(batch.done())

        q.put_nowait(0)
        q.put_nowait(1)
        yield gen.moment
        # Only two items; still waiting for the third.
        self.assertFalse(batch.done())

        q.put_nowait(2)
        yield gen.moment
        # The minimum is met; it returns immediately without waiting for the
        # remaining capacity to fill up.
        self.assertEqual([0, 1, 2], (yield batch))
        self.assertTrue(q.empty())

    @gen_test
    def test_batch_returns_as_soon_as_min_met_with_more_available(self):
        # While waiting, the batch fills toward max_items, but once a new item
        # pushes it to min_items it is delivered even though max is larger.
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        # Holds 0 and 1 (max 5), needs one more to reach min 3.
        batch = asyncio.ensure_future(q.get_batch(5, min_items=3))
        yield gen.moment
        self.assertFalse(batch.done())
        q.put_nowait(2)
        q.put_nowait(3)
        yield gen.moment
        # The third item (2) satisfied the minimum and delivery happened; 3
        # stays in the queue for others.
        self.assertEqual([0, 1, 2], (yield batch))
        self.assertEqual(3, q.get_nowait())

    @gen_test
    def test_max_items_caps_items_held_while_waiting(self):
        # A delivered batch never takes more than max_items, leaving the rest
        # for other consumers.
        q: queues.Queue[int] = queues.Queue()
        for i in range(4):
            q.put_nowait(i)
        batch = asyncio.ensure_future(q.get_batch(2, min_items=2))
        self.assertEqual([0, 1], (yield batch))
        self.assertEqual([2, 3], [q.get_nowait(), q.get_nowait()])

    @gen_test
    def test_waiting_batch_reaches_min_when_more_items_arrive(self):
        # The batch holds items below its minimum and waits; once enough items
        # arrive it is delivered and any surplus stays in the queue.
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        # Holds 0 and 1, needs one more to reach a minimum of three.
        batch = asyncio.ensure_future(q.get_batch(3, min_items=3))
        yield gen.moment
        self.assertFalse(batch.done())
        self.assertEqual(0, q.qsize())

        # Item 2 arrives and completes the batch; item 3 (arriving at the same
        # time) is surplus and stays queued.
        q.put_nowait(2)
        q.put_nowait(3)
        yield gen.moment
        self.assertEqual([0, 1, 2], (yield batch))
        self.assertEqual(3, q.get_nowait())

    @gen_test
    def test_batch_timeout_raises_and_returns_items_in_order(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        batch = asyncio.ensure_future(
            q.get_batch(3, min_items=3, timeout=timedelta(seconds=0.01))
        )
        with self.assertRaises(TimeoutError):
            yield batch

        # The two items already collected are back in the queue, in order.
        self.assertEqual(2, q.qsize())
        self.assertEqual(0, q.get_nowait())
        self.assertEqual(1, q.get_nowait())

    @gen_test
    def test_batch_timeout_requeues_to_waiting_plain_getter(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        batch = asyncio.ensure_future(
            q.get_batch(3, min_items=3, timeout=timedelta(seconds=0.02))
        )
        yield gen.moment
        # A plain getter waits behind the batch.
        single = asyncio.ensure_future(q.get())
        yield gen.moment
        self.assertFalse(single.done())

        with self.assertRaises(TimeoutError):
            yield batch
        # The requeued items immediately satisfy the plain getter in order.
        self.assertEqual(0, (yield single))
        self.assertEqual(1, q.get_nowait())

    @gen_test
    def test_batch_timeout_without_any_items(self):
        q: queues.Queue[int] = queues.Queue()
        with self.assertRaises(TimeoutError):
            yield q.get_batch(2, min_items=2, timeout=timedelta(seconds=0.01))
        self.assertTrue(q.empty())

    @gen_test
    def test_batch_timeout_returns_partial(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        batch = q.get_batch(
            4,
            min_items=3,
            timeout=timedelta(seconds=0.01),
            return_partial_on_timeout=True,
        )
        # The partial collection is handed over rather than raising.
        self.assertEqual([0, 1], (yield batch))
        self.assertTrue(q.empty())

    @gen_test
    def test_batch_timeout_returns_empty_partial(self):
        q: queues.Queue[int] = queues.Queue()
        batch = q.get_batch(
            2,
            min_items=2,
            timeout=timedelta(seconds=0.01),
            return_partial_on_timeout=True,
        )
        self.assertEqual([], (yield batch))

    @gen_test
    def test_batch_zero_timeout(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        with self.assertRaises(TimeoutError):
            yield q.get_batch(2, min_items=2, timeout=timedelta(seconds=0))
        # The item was never delivered, so it stays in the queue.
        self.assertEqual(0, q.get_nowait())

    @gen_test
    def test_cancel_returns_items_in_order_with_other_getter(self):
        # The scenario from the design: a batch getter has collected two items
        # and is waiting for a third, while a plain consumer and a cancellation
        # both become runnable. Items not handed to the cancelled consumer must
        # be returned in their original order.
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)

        # The batch getter (cap 5, minimum 4) eagerly holds 0 and 1, then
        # waits for more items.
        batch = asyncio.ensure_future(q.get_batch(5, min_items=4))
        yield gen.moment
        self.assertFalse(batch.done())
        self.assertEqual(0, q.qsize())

        # A plain consumer lines up behind the still-waiting batch getter.
        single = asyncio.ensure_future(q.get())
        yield gen.moment
        self.assertFalse(single.done())

        # A third item arrives and is collected, but the minimum (4) is still
        # not met, so the batch getter keeps waiting.
        q.put_nowait(2)
        yield gen.moment
        self.assertFalse(batch.done())

        # Cancel the batch; its three items (0, 1, 2) go back in their
        # original order, and the plain getter receives the first one.
        batch.cancel()
        yield gen.moment
        self.assertEqual(0, (yield single))
        self.assertEqual([1, 2], [q.get_nowait(), q.get_nowait()])
        self.assertTrue(q.empty())
        with self.assertRaises(asyncio.CancelledError):
            yield batch

    @gen_test
    def test_cancel_with_later_put_preserves_order(self):
        # A cancelled batch getter returns its items ahead of items that are
        # put into the queue afterwards.
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        batch = asyncio.ensure_future(q.get_batch(5, min_items=4))
        yield gen.moment
        self.assertFalse(batch.done())

        batch.cancel()
        yield gen.moment
        with self.assertRaises(asyncio.CancelledError):
            yield batch

        # New items go in after the returned ones.
        q.put_nowait(2)
        self.assertEqual([0, 1, 2], [q.get_nowait() for _ in range(3)])

    @gen_test
    def test_items_beyond_cap_stay_queued(self):
        # A batch fills to its cap and leaves any extra items queued; the
        # delivered batch's items plus the leftover preserve the global order.
        q: queues.Queue[int] = queues.Queue()
        getter = asyncio.ensure_future(q.get_batch(2, min_items=2))
        yield gen.moment
        q.put_nowait(0)
        q.put_nowait(1)
        q.put_nowait(2)
        yield gen.moment
        # 0 and 1 fill and complete the batch; item 2 was beyond the cap.
        self.assertEqual([0, 1], (yield getter))
        self.assertEqual(2, q.get_nowait())

    @gen_test
    def test_cancel_before_any_items(self):
        q: queues.Queue[int] = queues.Queue()
        batch = asyncio.ensure_future(q.get_batch(2, min_items=2))
        yield gen.moment
        batch.cancel()
        yield gen.moment
        with self.assertRaises(asyncio.CancelledError):
            yield batch
        self.assertTrue(q.empty())

        # The queue still works normally afterwards.
        q.put_nowait(7)
        self.assertEqual(7, (yield q.get()))

    @gen_test
    def test_batch_interleaved_with_plain_getters(self):
        q: queues.Queue[int] = queues.Queue()
        # A batch getter (min 2) waits on an empty queue.
        batch = asyncio.ensure_future(q.get_batch(3, min_items=2))
        yield gen.moment

        # First item goes to the batch getter (still one short).
        q.put_nowait(0)
        yield gen.moment
        self.assertFalse(batch.done())

        # A plain getter queues behind the batch getter.
        single = asyncio.ensure_future(q.get())
        yield gen.moment

        # The next item completes the batch (0, 1); the plain getter is still
        # waiting.
        q.put_nowait(1)
        yield gen.moment
        self.assertEqual([0, 1], (yield batch))
        self.assertFalse(single.done())

        # The following item reaches the plain getter in order.
        q.put_nowait(2)
        yield gen.moment
        self.assertEqual(2, (yield single))

    @gen_test
    def test_batch_task_done_and_join(self):
        q: queues.Queue[int] = queues.Queue()
        for i in range(6):
            q.put_nowait(i)

        consumed = []

        async def batch_consumer():
            batch = await q.get_batch(4, min_items=2)
            consumed.extend(batch)
            for _ in batch:
                q.task_done()

        async def plain_consumer():
            for _ in range(2):
                item = await q.get()
                consumed.append(item)
                q.task_done()

        async def run():
            await asyncio.gather(batch_consumer(), plain_consumer())
            # All items were delivered in order across both consumers.
            self.assertEqual(list(range(6)), consumed)
            # join() completes because every item was settled with task_done.
            await q.join()

        yield run()

    @gen_test
    def test_batch_cancel_then_join_completes(self):
        q: queues.Queue[int] = queues.Queue()
        q.put_nowait(0)
        q.put_nowait(1)
        waiting = asyncio.ensure_future(q.get_batch(3, min_items=3))
        yield gen.moment
        # It holds the two items while waiting for a third.
        self.assertEqual(0, q.qsize())
        waiting.cancel()
        yield gen.moment
        with self.assertRaises(asyncio.CancelledError):
            yield waiting

        # The returned items are still outstanding tasks: consume and settle
        # them, after which join() must finish.
        items = []
        while not q.empty():
            items.append(q.get_nowait())
        self.assertEqual([0, 1], items)
        for _ in items:
            q.task_done()
        yield q.join()

    @gen_test
    def test_batch_with_bounded_queue_unblocks_putters(self):
        q: queues.Queue[int] = queues.Queue(maxsize=2)
        q.put_nowait(0)
        q.put_nowait(1)
        # This putter blocks until the batch getter frees room.
        blocked_put = q.put(2)
        yield gen.moment
        self.assertFalse(blocked_put.done())

        # The batch getter drains the queue, making room for the blocked put.
        batch = asyncio.ensure_future(q.get_batch(2, min_items=2))
        self.assertEqual([0, 1], (yield batch))
        # The blocked putter completes as soon as a slot opens.
        yield blocked_put
        self.assertEqual([2], [q.get_nowait()])

    @gen_test
    def test_bounded_queue_put_with_waiting_batch_then_cancel(self):
        # Standard queue semantics: a put succeeds while a consumer is waiting.
        # The waiting batch takes the new item too; cancelling it returns every
        # item it held, so nothing is lost and all tasks remain settleable.
        q: queues.Queue[int] = queues.Queue(maxsize=2)
        q.put_nowait(0)
        q.put_nowait(1)
        # The batch holds both items while waiting for four in total.
        waiting = asyncio.ensure_future(q.get_batch(4, min_items=4))
        yield gen.moment
        self.assertFalse(waiting.done())

        # A producer can put because a consumer (the batch) is waiting; the
        # batch collects the new item but is still one short of its minimum.
        put = q.put(2)
        yield gen.moment
        self.assertTrue(put.done())
        self.assertFalse(waiting.done())

        # Cancelling returns every held item in the original order.
        waiting.cancel()
        yield gen.moment
        with self.assertRaises(asyncio.CancelledError):
            yield waiting
        items = []
        while not q.empty():
            items.append(q.get_nowait())
        self.assertEqual([0, 1, 2], items)
        for _ in items:
            q.task_done()
        yield q.join()


class PriorityQueueBatchTest(AsyncTestCase):
    @gen_test
    def test_batch_preserves_priority_order(self):
        q: queues.PriorityQueue[tuple[int, str]] = queues.PriorityQueue()
        q.put_nowait((2, "b"))
        q.put_nowait((1, "a"))
        q.put_nowait((3, "c"))
        batch = asyncio.ensure_future(q.get_batch(2, min_items=2))
        self.assertEqual([(1, "a"), (2, "b")], (yield batch))
        self.assertEqual((3, "c"), q.get_nowait())

    @gen_test
    def test_cancel_restores_priority_order(self):
        q: queues.PriorityQueue[tuple[int, str]] = queues.PriorityQueue()
        q.put_nowait((2, "b"))
        q.put_nowait((1, "a"))
        waiting = asyncio.ensure_future(q.get_batch(3, min_items=3))
        yield gen.moment
        self.assertFalse(waiting.done())
        waiting.cancel()
        yield gen.moment
        with self.assertRaises(asyncio.CancelledError):
            yield waiting
        # Both items are back and still come out in priority order.
        self.assertEqual((1, "a"), q.get_nowait())
        self.assertEqual((2, "b"), q.get_nowait())


class LifoQueueBatchTest(AsyncTestCase):
    @gen_test
    def test_cancel_restores_lifo_order(self):
        q: queues.LifoQueue[int] = queues.LifoQueue()
        q.put_nowait(0)
        q.put_nowait(1)
        waiting = asyncio.ensure_future(q.get_batch(3, min_items=3))
        yield gen.moment
        waiting.cancel()
        yield gen.moment
        with self.assertRaises(asyncio.CancelledError):
            yield waiting
        # LIFO order is preserved after the items are returned.
        self.assertEqual(1, q.get_nowait())
        self.assertEqual(0, q.get_nowait())


class QueuePutTest(AsyncTestCase):
    @gen_test
    def test_blocking_put(self):
        q: queues.Queue[int] = queues.Queue()
        q.put(0)
        self.assertEqual(0, q.get_nowait())

    def test_nonblocking_put_exception(self):
        q: queues.Queue[int] = queues.Queue(1)
        q.put(0)
        self.assertRaises(queues.QueueFull, q.put_nowait, 1)

    @gen_test
    def test_put_with_getters(self):
        q: queues.Queue[int] = queues.Queue()
        get0 = q.get()
        get1 = q.get()
        yield q.put(0)
        self.assertEqual(0, (yield get0))
        yield q.put(1)
        self.assertEqual(1, (yield get1))

    @gen_test
    def test_nonblocking_put_with_getters(self):
        q: queues.Queue[int] = queues.Queue()
        get0 = q.get()
        get1 = q.get()
        q.put_nowait(0)
        # put_nowait does *not* immediately unblock getters.
        yield gen.moment
        self.assertEqual(0, (yield get0))
        q.put_nowait(1)
        yield gen.moment
        self.assertEqual(1, (yield get1))

    @gen_test
    def test_blocking_put_wait(self):
        q: queues.Queue[int] = queues.Queue(1)
        q.put_nowait(0)

        def get_and_discard():
            q.get()

        self.io_loop.call_later(0.01, get_and_discard)
        self.io_loop.call_later(0.02, get_and_discard)
        futures = [q.put(0), q.put(1)]
        self.assertFalse(any(f.done() for f in futures))
        yield futures

    @gen_test
    def test_put_timeout(self):
        q: queues.Queue[int] = queues.Queue(1)
        q.put_nowait(0)  # Now it's full.
        put_timeout = q.put(1, timeout=timedelta(seconds=0.01))
        put = q.put(2)
        with self.assertRaises(TimeoutError):
            yield put_timeout

        self.assertEqual(0, q.get_nowait())
        # 1 was never put in the queue.
        self.assertEqual(2, (yield q.get()))

        # Final get() unblocked this putter.
        yield put

    @gen_test
    def test_put_timeout_preempted(self):
        q: queues.Queue[int] = queues.Queue(1)
        q.put_nowait(0)
        put = q.put(1, timeout=timedelta(seconds=0.01))
        q.get()
        yield gen.sleep(0.02)
        yield put  # No TimeoutError.

    @gen_test
    def test_put_clears_timed_out_putters(self):
        q: queues.Queue[int] = queues.Queue(1)
        # First putter succeeds, remainder block.
        putters = [q.put(i, timedelta(seconds=0.01)) for i in range(10)]
        put = q.put(10)
        self.assertEqual(10, len(q._putters))
        yield gen.sleep(0.02)
        self.assertEqual(10, len(q._putters))
        self.assertFalse(put.done())  # Final waiter is still active.
        q.put(11)  # put() clears the waiters.
        self.assertEqual(2, len(q._putters))
        for putter in putters[1:]:
            self.assertRaises(TimeoutError, putter.result)

    @gen_test
    def test_put_clears_timed_out_getters(self):
        q: queues.Queue[int] = queues.Queue()
        getters = [
            asyncio.ensure_future(q.get(timedelta(seconds=0.01))) for _ in range(10)
        ]
        get = asyncio.ensure_future(q.get())
        q.get()
        self.assertEqual(12, len(q._getters))
        yield gen.sleep(0.02)
        self.assertEqual(12, len(q._getters))
        self.assertFalse(get.done())  # Final waiters still active.
        q.put(0)  # put() clears the waiters.
        self.assertEqual(1, len(q._getters))
        self.assertEqual(0, (yield get))
        for getter in getters:
            self.assertRaises(TimeoutError, getter.result)

    @gen_test
    def test_float_maxsize(self):
        # If a float is passed for maxsize, a reasonable limit should
        # be enforced, instead of being treated as unlimited.
        # It happens to be rounded up.
        # http://bugs.python.org/issue21723
        q = queues.Queue(maxsize=1.3)  # type: ignore
        self.assertTrue(q.empty())
        self.assertFalse(q.full())
        q.put_nowait(0)
        q.put_nowait(1)
        self.assertFalse(q.empty())
        self.assertTrue(q.full())
        self.assertRaises(queues.QueueFull, q.put_nowait, 2)
        self.assertEqual(0, q.get_nowait())
        self.assertFalse(q.empty())
        self.assertFalse(q.full())

        yield q.put(2)
        put = q.put(3)
        self.assertFalse(put.done())
        self.assertEqual(1, (yield q.get()))
        yield put
        self.assertTrue(q.full())


class QueueJoinTest(AsyncTestCase):
    queue_class = queues.Queue

    def test_task_done_underflow(self):
        q: queues.Queue[typing.Any] = self.queue_class()
        self.assertRaises(ValueError, q.task_done)

    @gen_test
    def test_task_done(self):
        q: queues.Queue[typing.Any] = self.queue_class()
        for i in range(100):
            q.put_nowait(i)

        self.accumulator = 0

        @gen.coroutine
        def worker():
            while True:
                item = yield q.get()
                self.accumulator += item
                q.task_done()
                yield gen.sleep(random() * 0.01)

        # Two coroutines share work.
        worker()
        worker()
        yield q.join()
        self.assertEqual(sum(range(100)), self.accumulator)

    @gen_test
    def test_task_done_delay(self):
        # Verify it is task_done(), not get(), that unblocks join().
        q: queues.Queue[typing.Any] = self.queue_class()
        q.put_nowait(0)
        join = asyncio.ensure_future(q.join())
        self.assertFalse(join.done())
        yield q.get()
        self.assertFalse(join.done())
        yield gen.moment
        self.assertFalse(join.done())
        q.task_done()
        self.assertTrue(join.done())

    @gen_test
    def test_join_empty_queue(self):
        q: queues.Queue[typing.Any] = self.queue_class()
        yield q.join()
        yield q.join()

    @gen_test
    def test_join_timeout(self):
        q: queues.Queue[typing.Any] = self.queue_class()
        q.put(0)
        with self.assertRaises(TimeoutError):
            yield q.join(timeout=timedelta(seconds=0.01))


class PriorityQueueJoinTest(QueueJoinTest):
    queue_class = queues.PriorityQueue

    @gen_test
    def test_order(self):
        q: queues.PriorityQueue[tuple[int, str]] = self.queue_class(maxsize=2)
        q.put_nowait((1, "a"))
        q.put_nowait((0, "b"))
        self.assertTrue(q.full())
        q.put((3, "c"))
        q.put((2, "d"))
        self.assertEqual((0, "b"), q.get_nowait())
        self.assertEqual((1, "a"), (yield q.get()))
        self.assertEqual((2, "d"), q.get_nowait())
        self.assertEqual((3, "c"), (yield q.get()))
        self.assertTrue(q.empty())


class LifoQueueJoinTest(QueueJoinTest):
    queue_class = queues.LifoQueue

    @gen_test
    def test_order(self):
        q: queues.LifoQueue[int] = self.queue_class(maxsize=2)
        q.put_nowait(1)
        q.put_nowait(0)
        self.assertTrue(q.full())
        q.put(3)
        q.put(2)
        self.assertEqual(3, q.get_nowait())
        self.assertEqual(2, (yield q.get()))
        self.assertEqual(0, q.get_nowait())
        self.assertEqual(1, (yield q.get()))
        self.assertTrue(q.empty())


class ProducerConsumerTest(AsyncTestCase):
    @gen_test
    def test_producer_consumer(self):
        q: queues.Queue[int] = queues.Queue(maxsize=3)
        history = []

        # We don't yield between get() and task_done(), so get() must wait for
        # the next tick. Otherwise we'd immediately call task_done and unblock
        # join() before q.put() resumes, and we'd only process the first four
        # items.
        @gen.coroutine
        def consumer():
            while True:
                history.append((yield q.get()))
                q.task_done()

        @gen.coroutine
        def producer():
            for item in range(10):
                yield q.put(item)

        consumer()
        yield producer()
        yield q.join()
        self.assertEqual(list(range(10)), history)


if __name__ == "__main__":
    unittest.main()
