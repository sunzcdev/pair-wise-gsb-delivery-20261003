# Copyright 2015 The Tornado Authors
#
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

"""Asynchronous queues for coroutines. These classes are very similar
to those provided in the standard library's `asyncio package
<https://docs.python.org/3/library/asyncio-queue.html>`_.

.. warning::

   Unlike the standard library's `queue` module, the classes defined here
   are *not* thread-safe. To use these queues from another thread,
   use `.IOLoop.add_callback` to transfer control to the `.IOLoop` thread
   before calling any queue methods.

"""

from __future__ import annotations

import collections
import datetime
import heapq
from collections.abc import Awaitable
from typing import Any, Generic, TypeVar, Union

from tornado import ioloop
from tornado.concurrent import Future, future_set_result_unless_cancelled
from tornado.locks import Event

_T = TypeVar("_T")

# Entries on ``Queue._getters`` are either a plain `.Future` (for ``get``) or
# a `_BatchGetter` (for ``get_batch``).
_Getter = Union["Future[_T]", "_BatchGetter[_T]"]

__all__ = ["Queue", "PriorityQueue", "LifoQueue", "QueueFull", "QueueEmpty"]


class QueueEmpty(Exception):
    """Raised by `.Queue.get_nowait` when the queue has no items."""

    pass


class QueueFull(Exception):
    """Raised by `.Queue.put_nowait` when a queue is at its maximum size."""

    pass


def _set_timeout(
    future: Future[Any], timeout: None | float | datetime.timedelta
) -> None:
    if timeout is not None:

        def on_timeout() -> None:
            if not future.done():
                future.set_exception(TimeoutError())

        io_loop = ioloop.IOLoop.current()
        timeout_handle = io_loop.add_timeout(timeout, on_timeout)
        future.add_done_callback(lambda _: io_loop.remove_timeout(timeout_handle))


class _BatchGetter(Generic[_T]):
    """A waiting ``get_batch`` consumer.

    A batch getter eagerly pulls items out of the queue as they arrive,
    collecting them in ``items`` until it has at least ``min_items``. If the
    awaitable is cancelled (or times out without delivering) before that, the
    collected items are handed back to the queue in their original order.
    """

    def __init__(
        self,
        min_items: int,
        max_items: int,
        future: "Future[list[_T]]",
        return_partial_on_timeout: bool,
    ) -> None:
        self.min_items = min_items
        self.max_items = max_items
        self.future = future
        self.return_partial_on_timeout = return_partial_on_timeout
        self.items: list[_T] = []
        # True once ``items`` has been handed over to the consumer; from then
        # on the queue must not put them back.
        self.delivered = False

    def add_item(self, item: _T) -> None:
        self.items.append(item)

    def satisfied(self) -> bool:
        return len(self.items) >= self.min_items

    def full(self) -> bool:
        return len(self.items) >= self.max_items


class _QueueIterator(Generic[_T]):
    def __init__(self, q: Queue[_T]) -> None:
        self.q = q

    def __anext__(self) -> Awaitable[_T]:
        return self.q.get()


class Queue(Generic[_T]):
    """Coordinate producer and consumer coroutines.

    If maxsize is 0 (the default) the queue size is unbounded.

    .. testcode::

        import asyncio
        from tornado.ioloop import IOLoop
        from tornado.queues import Queue

        q = Queue(maxsize=2)

        async def consumer():
            async for item in q:
                try:
                    print('Doing work on %s' % item)
                    await asyncio.sleep(0.01)
                finally:
                    q.task_done()

        async def producer():
            for item in range(5):
                await q.put(item)
                print('Put %s' % item)

        async def main():
            # Start consumer without waiting (since it never finishes).
            IOLoop.current().spawn_callback(consumer)
            await producer()     # Wait for producer to put all tasks.
            await q.join()       # Wait for consumer to finish all tasks.
            print('Done')

        asyncio.run(main())

    .. testoutput::

        Put 0
        Put 1
        Doing work on 0
        Put 2
        Doing work on 1
        Put 3
        Doing work on 2
        Put 4
        Doing work on 3
        Doing work on 4
        Done


    In versions of Python without native coroutines (before 3.5),
    ``consumer()`` could be written as::

        @gen.coroutine
        def consumer():
            while True:
                item = yield q.get()
                try:
                    print('Doing work on %s' % item)
                    yield gen.sleep(0.01)
                finally:
                    q.task_done()

    .. versionchanged:: 4.3
       Added ``async for`` support in Python 3.5.

    """

    # Exact type depends on subclass. Could be another generic
    # parameter and use protocols to be more precise here.
    _queue: Any = None

    def __init__(self, maxsize: int = 0) -> None:
        if maxsize is None:
            raise TypeError("maxsize can't be None")

        if maxsize < 0:
            raise ValueError("maxsize can't be negative")

        self._maxsize = maxsize
        self._init()
        self._getters: collections.deque[_Getter[_T]] = collections.deque([])
        self._putters: collections.deque[tuple[_T, Future[None]]] = collections.deque(
            []
        )
        self._unfinished_tasks = 0
        self._finished = Event()
        self._finished.set()

    @property
    def maxsize(self) -> int:
        """Number of items allowed in the queue."""
        return self._maxsize

    def qsize(self) -> int:
        """Number of items in the queue."""
        return len(self._queue)

    def empty(self) -> bool:
        return not self._queue

    def full(self) -> bool:
        if self.maxsize == 0:
            return False
        else:
            return self.qsize() >= self.maxsize

    def put(
        self, item: _T, timeout: float | datetime.timedelta | None = None
    ) -> Future[None]:
        """Put an item into the queue, perhaps waiting until there is room.

        Returns a Future, which raises `TimeoutError` after a
        timeout.

        ``timeout`` may be a number denoting a time (on the same
        scale as `tornado.ioloop.IOLoop.time`, normally `time.time`), or a
        `datetime.timedelta` object for a deadline relative to the
        current time.

        .. versionchanged:: 6.6
           A ``timeout`` argument of zero will either return or raise immediately.
           Previously, zero was treated equivalent to ``None`` (wait forever).
        """
        future: Future[None] = Future()
        try:
            self.put_nowait(item)
        except QueueFull:
            self._putters.append((item, future))
            _set_timeout(future, timeout)
        else:
            future.set_result(None)
        return future

    def put_nowait(self, item: _T) -> None:
        """Put an item into the queue without blocking.

        If no free slot is immediately available, raise `QueueFull`.
        """
        self._consume_expired()
        if self._getters:
            # A cancelled getter may still be queued until its completion
            # callback runs; its items (if a batch getter) are returned then.
            # Only a live getter implies the queue is empty.
            if self._first_getter_alive() and not self.empty():
                raise AssertionError(
                    "queue non-empty, why are getters waiting?"
                )
            self.__put_internal(item)
            self._feed_getters()
        elif self.full():
            raise QueueFull
        else:
            self.__put_internal(item)

    def _first_getter_alive(self) -> bool:
        if not self._getters:
            return False
        getter = self._getters[0]
        if isinstance(getter, _BatchGetter):
            return not getter.future.done()
        return not getter.done()

    def _feed_getters(self) -> None:
        # Hand queued items to waiting getters in FIFO order. A plain getter
        # takes a single item; a batch getter keeps taking items up to its
        # ``max_items`` capacity, but it is only delivered once it has
        # gathered at least ``min_items``. Items past its capacity stay in the
        # queue, but cannot reach any later getter until this one is settled.
        while self._getters and self.qsize():
            getter = self._getters[0]
            if isinstance(getter, _BatchGetter):
                if getter.future.done():
                    # A cancelled/timed-out batch getter; drop it. Its items,
                    # if any, are returned by its completion callback.
                    self._getters.popleft()
                    continue
                while self.qsize() and not getter.full():
                    getter.add_item(self._get())
                if getter.satisfied():
                    self._getters.popleft()
                    self._deliver_batch(getter)
                else:
                    # It is full but still short of its minimum, waiting for
                    # more items; nothing left can reach a later getter.
                    break
            else:
                self._getters.popleft()
                future_set_result_unless_cancelled(getter, self._get())

    def _deliver_batch(self, getter: "_BatchGetter[_T]") -> None:
        if not getter.delivered:
            getter.delivered = True
            future_set_result_unless_cancelled(
                getter.future, list(getter.items)
            )

    def get(self, timeout: float | datetime.timedelta | None = None) -> Awaitable[_T]:
        """Remove and return an item from the queue.

        Returns an awaitable which resolves once an item is available, or raises
        `TimeoutError` after a timeout.

        ``timeout`` may be a number denoting a time (on the same
        scale as `tornado.ioloop.IOLoop.time`, normally `time.time`), or a
        `datetime.timedelta` object for a deadline relative to the
        current time.

        .. note::

           The ``timeout`` argument of this method differs from that
           of the standard library's `queue.Queue.get`. That method
           interprets numeric values as relative timeouts; this one
           interprets them as absolute deadlines and requires
           ``timedelta`` objects for relative timeouts (consistent
           with other timeouts in Tornado).

        .. versionchanged:: 6.6
           A ``timeout`` argument of zero will either return or raise immediately.
           Previously, zero was treated equivalent to ``None`` (wait forever).
        """
        future: Future[_T] = Future()
        try:
            future.set_result(self.get_nowait())
        except QueueEmpty:
            self._getters.append(future)
            _set_timeout(future, timeout)
        return future

    def get_batch(
        self,
        max_items: int,
        min_items: int = 1,
        timeout: None | float | datetime.timedelta = None,
        *,
        return_partial_on_timeout: bool = False,
    ) -> Awaitable[list[_T]]:
        """Remove and return a batch of items from the queue.

        The awaitable resolves with a list of at least ``min_items`` and at
        most ``max_items`` items, in the order in which they were put into the
        queue. Available items are collected eagerly, up to ``max_items``; the
        batch is delivered as soon as it holds at least ``min_items`` items.
        If fewer than ``min_items`` items are available, the caller waits for
        more items to arrive. A waiting batch holds up to ``max_items`` items
        at a time, so items beyond that cap remain in the queue (though behind
        the waiting batch) until it is settled.

        After ``timeout`` elapses before ``min_items`` have been gathered, the
        behavior depends on ``return_partial_on_timeout``:

        * When ``False`` (the default), `TimeoutError` is raised and every
          item collected so far is put back into the queue in its original
          position, so it remains available to other consumers and must still
          be settled with `.task_done`.
        * When ``True``, the items collected so far (possibly an empty list)
          are returned instead of raising.

        If the awaitable is cancelled before the batch is delivered, the
        collected items are likewise returned to the queue in their original
        order and remain subject to `.task_done`.

        ``timeout`` may be a number denoting a time (on the same scale as
        `tornado.ioloop.IOLoop.time`, normally `time.time`), or a
        `datetime.timedelta` object for a deadline relative to the current
        time. A timeout of zero resolves or raises immediately.

        .. versionadded:: 6.7
        """
        if not isinstance(max_items, int) or max_items < 1:
            raise ValueError("max_items must be a positive integer")
        if not isinstance(min_items, int) or min_items < 1:
            raise ValueError("min_items must be a positive integer")
        if min_items > max_items:
            raise ValueError("min_items must not be greater than max_items")

        future: Future[list[_T]] = Future()
        getter = _BatchGetter(
            min_items, max_items, future, return_partial_on_timeout
        )

        # Collect up to ``max_items`` items immediately (this also unblocks
        # putters when the queue is bounded). The batch is only delivered once
        # at least ``min_items`` have been gathered.
        while len(getter.items) < max_items:
            try:
                getter.add_item(self.get_nowait())
            except QueueEmpty:
                break

        if len(getter.items) >= min_items:
            self._deliver_batch(getter)
            return future

        # Wait for enough items to arrive.
        self._getters.append(getter)
        if timeout is not None:
            io_loop = ioloop.IOLoop.current()
            timeout_handle = io_loop.add_timeout(
                timeout, lambda: self._on_batch_timeout(getter)
            )
            future.add_done_callback(
                lambda _: io_loop.remove_timeout(timeout_handle)
            )
        future.add_done_callback(lambda _: self._on_batch_done(getter))
        return future

    def _on_batch_timeout(self, getter: "_BatchGetter[_T]") -> None:
        if getter.future.done():
            return
        if getter.return_partial_on_timeout:
            self._deliver_batch(getter)
        else:
            getter.future.set_exception(TimeoutError())

    def _on_batch_done(self, getter: "_BatchGetter[_T]") -> None:
        if getter.delivered:
            # The items were handed to the consumer; nothing to return.
            return
        getter.delivered = True
        self._return_items(getter.items)

    def _return_items(self, items: list[_T]) -> None:
        # Put items back in their original relative order, ahead of any other
        # items still in the queue. No task accounting changes: these items
        # were never delivered and still require a matching ``task_done``.
        for item in reversed(items):
            self._put_back(item)
        self._feed_getters()

    def get_nowait(self) -> _T:
        """Remove and return an item from the queue without blocking.

        Return an item if one is immediately available, else raise
        `QueueEmpty`.
        """
        self._consume_expired()
        if self._putters:
            assert self.full(), "queue not full, why are putters waiting?"
            item, putter = self._putters.popleft()
            self.__put_internal(item)
            future_set_result_unless_cancelled(putter, None)
            return self._get()
        elif self.qsize():
            return self._get()
        else:
            raise QueueEmpty

    def task_done(self) -> None:
        """Indicate that a formerly enqueued task is complete.

        Used by queue consumers. For each `.get` used to fetch a task, a
        subsequent call to `.task_done` tells the queue that the processing
        on the task is complete.

        If a `.join` is blocking, it resumes when all items have been
        processed; that is, when every `.put` is matched by a `.task_done`.

        Raises `ValueError` if called more times than `.put`.
        """
        if self._unfinished_tasks <= 0:
            raise ValueError("task_done() called too many times")
        self._unfinished_tasks -= 1
        if self._unfinished_tasks == 0:
            self._finished.set()

    def join(
        self, timeout: float | datetime.timedelta | None = None
    ) -> Awaitable[None]:
        """Block until all items in the queue are processed.

        Returns an awaitable, which raises `TimeoutError` after a
        timeout.

        .. versionchanged:: 6.6
           A ``timeout`` argument of zero will either return or raise immediately.
           Previously, zero was treated equivalent to ``None`` (wait forever).
        """
        return self._finished.wait(timeout)

    def __aiter__(self) -> _QueueIterator[_T]:
        return _QueueIterator(self)

    # These three are overridable in subclasses.
    def _init(self) -> None:
        self._queue = collections.deque()

    def _get(self) -> _T:
        item: _T = self._queue.popleft()
        return item

    def _put(self, item: _T) -> None:
        self._queue.append(item)

    def _put_back(self, item: _T) -> None:
        # Restore an item that had been removed, keeping it ahead of the items
        # still queued so the original ordering is preserved.
        self._queue.appendleft(item)

    # End of the overridable methods.

    def __put_internal(self, item: _T) -> None:
        self._unfinished_tasks += 1
        self._finished.clear()
        self._put(item)

    def _consume_expired(self) -> None:
        # Remove timed-out waiters.
        while self._putters and self._putters[0][1].done():
            self._putters.popleft()

        while self._getters:
            getter = self._getters[0]
            if isinstance(getter, _BatchGetter):
                done = getter.future.done()
            else:
                done = getter.done()
            if not done:
                break
            self._getters.popleft()
            if isinstance(getter, _BatchGetter):
                # Return any items the expired batch getter was holding.
                if not getter.delivered:
                    getter.delivered = True
                    self._return_items(getter.items)

    def __repr__(self) -> str:
        return f"<{type(self).__name__} at {hex(id(self))} {self._format()}>"

    def __str__(self) -> str:
        return f"<{type(self).__name__} {self._format()}>"

    def _format(self) -> str:
        result = f"maxsize={self.maxsize!r}"
        if getattr(self, "_queue", None):
            result += " queue=%r" % self._queue
        if self._getters:
            result += " getters[%s]" % len(self._getters)
        if self._putters:
            result += " putters[%s]" % len(self._putters)
        if self._unfinished_tasks:
            result += " tasks=%s" % self._unfinished_tasks
        return result


class PriorityQueue(Queue[_T]):
    """A `.Queue` that retrieves entries in priority order, lowest first.

    Entries are typically tuples like ``(priority number, data)``.

    .. testcode::

        import asyncio
        from tornado.queues import PriorityQueue

        async def main():
            q = PriorityQueue()
            q.put((1, 'medium-priority item'))
            q.put((0, 'high-priority item'))
            q.put((10, 'low-priority item'))

            print(await q.get())
            print(await q.get())
            print(await q.get())

        asyncio.run(main())

    .. testoutput::

        (0, 'high-priority item')
        (1, 'medium-priority item')
        (10, 'low-priority item')
    """

    def _init(self) -> None:
        self._queue = []

    def _put(self, item: _T) -> None:
        heapq.heappush(self._queue, item)

    def _get(self) -> _T:
        item: _T = heapq.heappop(self._queue)
        return item

    def _put_back(self, item: _T) -> None:
        heapq.heappush(self._queue, item)


class LifoQueue(Queue[_T]):
    """A `.Queue` that retrieves the most recently put items first.

    .. testcode::

        import asyncio
        from tornado.queues import LifoQueue

        async def main():
            q = LifoQueue()
            q.put(3)
            q.put(2)
            q.put(1)

            print(await q.get())
            print(await q.get())
            print(await q.get())

        asyncio.run(main())

    .. testoutput::

        1
        2
        3
    """

    def _init(self) -> None:
        self._queue = []

    def _put(self, item: _T) -> None:
        self._queue.append(item)

    def _get(self) -> _T:
        item: _T = self._queue.pop()
        return item

    def _put_back(self, item: _T) -> None:
        self._queue.append(item)
