# This file is a part of Dramatiq.
#
# Copyright (C) 2017,2018,2019,2020 CLEARTYPE SRL <bogdan@cleartype.io>
#
# Dramatiq is free software; you can redistribute it and/or modify it
# under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# Dramatiq is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
# License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

from __future__ import annotations

import threading
import time
import uuid
from collections import defaultdict
from contextlib import contextmanager
from itertools import chain
from queue import Empty, Queue
from typing import Iterator, Optional

from ..broker import Broker, Consumer, MessageProxy
from ..common import current_millis, dq_name, iter_queue, join_queue
from ..errors import QueueJoinTimeout, QueueNotFound
from ..message import Message
from ..middleware import Middleware

#: The name of the message option used to tag messages with the wave
#: they belong to.  It is only ever set on messages that flow through a
#: :class:`StubBroker`, so it never leaks onto real brokers.
WAVE_ID_OPTION = "dramatiq_stub_wave_id"


class Wave:
    """A single submission wave tracked by a :class:`StubBroker`.

    A wave groups together the messages enqueued within the
    :meth:`StubBroker.wave` context as well as every message those
    jobs derive while they are being processed -- retries, pipeline
    follow-ups, delayed re-enqueues and so on.  Messages enqueued
    outside of the wave (for example, by an unrelated test sharing the
    same broker) are not counted towards it and therefore never keep
    it from completing.

    Wave objects are created by :meth:`StubBroker.wave` and are not
    meant to be instantiated directly.
    """

    def __init__(self, broker: "StubBroker", queue_name: str) -> None:
        self.broker = broker
        self.queue_name = queue_name
        self.id = uuid.uuid4().hex

        # Guards ``pending`` and ``dead_letters`` and backs ``cond``.
        self.cond = threading.Condition(threading.RLock())
        #: The number of messages belonging to this wave that have
        #: been enqueued but not yet acked or nacked.
        self.pending = 0
        #: Message proxies belonging to this wave that were nacked.
        self.dead_letters: list[MessageProxy] = []

    def join(self, *, timeout: Optional[int] = None, fail_fast: Optional[bool] = None) -> None:
        """Wait for every message belonging to this wave to settle.

        A message settles once it is acknowledged or dead-lettered.
        Messages derived while wave messages are being processed
        (retries, pipeline follow-ups, ...) are part of the wave, so
        the join blocks until they settle as well.  Messages enqueued
        by other, independent work on the same queues are ignored.

        Raises:
          QueueJoinTimeout: When the timeout elapses before the wave
            settles.

        Parameters:
          fail_fast(bool): When this is True and any wave message gets
            dead-lettered during the join, the captured actor
            exception is raised.  Defaults to the broker's
            ``fail_fast_default``.
          timeout(Optional[int]): The max amount of time, in
            milliseconds, to wait on this wave.
        """
        start = time.monotonic()
        deadline = timeout and start + timeout / 1000
        should_fail_fast = fail_fast if fail_fast is not None else self.broker.fail_fast_default
        with self.cond:
            while self.pending:
                if deadline is None:
                    self.cond.wait()
                else:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise QueueJoinTimeout("timed out after %.02f seconds" % (time.monotonic() - start))
                    self.cond.wait(remaining)

            if should_fail_fast:
                for message in self.dead_letters:
                    raise (message._exception or Exception("Message failed with unknown error")) from None

    @property
    def is_settled(self) -> bool:
        """Whether every message in this wave has been processed."""
        with self.cond:
            return self.pending == 0


class _WavePropagationMiddleware(Middleware):
    """Internal middleware that propagates wave ids across worker threads.

    While a wave message is being processed, any message it enqueues
    (retries, pipeline follow-ups, callbacks, ...) inherits the wave
    via the broker's thread-local wave stack.  The stack is managed
    around ``process_message`` so worker threads -- which handle one
    message at a time -- always run with the wave of the message in
    flight, regardless of which wave the previous message belonged to.
    """

    def before_process_message(self, broker: Broker, message: MessageProxy) -> None:
        if isinstance(broker, StubBroker):
            wave_id = message.options.get(WAVE_ID_OPTION)
            if wave_id is not None:
                broker._wave_stack.append(wave_id)

    def after_process_message(self, broker: Broker, message: MessageProxy, *, result=None, exception=None) -> None:
        self._pop_wave(broker, message)

    def after_skip_message(self, broker: Broker, message: MessageProxy) -> None:
        self._pop_wave(broker, message)

    @staticmethod
    def _pop_wave(broker: Broker, message: MessageProxy) -> None:
        if isinstance(broker, StubBroker) and message.options.get(WAVE_ID_OPTION) is not None:
            stack = broker._wave_stack
            if stack:
                stack.pop()


class StubBroker(Broker):
    """A broker that can be used within unit tests.

    Parameters:
      middleware: See :class:`Broker<dramatiq.Broker>`.
      fail_fast_default: Specifies the default value for the ``fail_fast``
        argument of :meth:`join<dramatiq.brokers.stub.StubBroker.join>`.
    """

    def __init__(self, middleware: Optional[list[Middleware]] = None, *, fail_fast_default: bool = True):
        super().__init__(middleware)

        self.dead_letters_by_queue: defaultdict[str, list[MessageProxy]] = defaultdict(list)
        self.fail_fast_default: bool = fail_fast_default

        # All waves that have ever been opened on this broker, keyed
        # by wave id.  Finished waves are discarded by ``flush_all``.
        self.waves: dict[str, Wave] = {}
        self._waves_lock = threading.RLock()
        # Per-thread wave ids, outermost first.  Test threads push via
        # ``wave()``; worker threads push transiently through
        # ``_WavePropagationMiddleware`` while a wave message runs.
        self._wave_local = threading.local()
        # Insert first so the wave context is established before any
        # other middleware's ``before_process_message`` runs and torn
        # down after every other ``after_process_message`` hook.  That
        # guarantees messages derived inside those hooks (retries,
        # pipeline follow-ups, callbacks, ...) inherit the wave.
        self.middleware.insert(0, _WavePropagationMiddleware())

    @property
    def _wave_stack(self) -> list[str]:
        """The wave-id stack of the current thread."""
        stack = getattr(self._wave_local, "stack", None)
        if stack is None:
            stack = self._wave_local.stack = []
        return stack

    @property
    def dead_letters(self) -> list[MessageProxy]:
        """The dead-lettered messages for all defined queues."""
        return [message for messages in self.dead_letters_by_queue.values() for message in messages]

    def consume(self, queue_name: str, prefetch: int = 1, timeout: int = 100) -> Consumer:
        """Create a new consumer for a queue.

        Parameters:
          queue_name(str): The queue to consume.
          prefetch(int): The number of messages to prefetch.
          timeout(int): The idle timeout in milliseconds.

        Raises:
          QueueNotFound: If the queue hasn't been declared.

        Returns:
          Consumer: A consumer that retrieves messages from Redis.
        """
        try:
            return _StubConsumer(
                self.queues[queue_name],
                self.dead_letters_by_queue[queue_name],
                prefetch,
                timeout,
                self,
            )
        except KeyError:
            raise QueueNotFound(queue_name) from None

    def declare_queue(self, queue_name: str) -> None:
        """Declare a queue.  Has no effect if a queue with the given
        name has already been declared.

        Parameters:
          queue_name(str): The name of the new queue.
        """
        if queue_name in self.queues:
            return

        self.emit_before("declare_queue", queue_name)
        self.queues[queue_name] = Queue()
        self.emit_after("declare_queue", queue_name)

        delayed_name = dq_name(queue_name)
        self.queues[delayed_name] = Queue()
        self.delay_queues.add(delayed_name)
        self.emit_after("declare_delay_queue", delayed_name)

    @contextmanager
    def wave(self, queue_name: str) -> Iterator[Wave]:
        """Open a new submission wave on the given queue.

        Every message enqueued within the ``with`` block becomes part
        of the returned :class:`Wave`, along with anything those
        messages derive while being processed (retries, pipeline
        follow-ups, delayed re-enqueues, ...).  Independent messages
        enqueued on the same queue by other code -- for instance,
        another test sharing the broker -- are not part of the wave
        and never keep :meth:`Wave.join` from returning.

        The wave must be waited on explicitly with :meth:`Wave.join`;
        exiting the context does not wait for anything::

            with stub_broker.wave(do_work.queue_name) as wave:
                do_work.send()
            wave.join()

        Raises:
          QueueNotFound: If the given queue was never declared.

        Parameters:
          queue_name(str): The queue the wave's jobs are submitted on.
        """
        if queue_name not in self.queues:
            raise QueueNotFound(queue_name)

        wave = Wave(self, queue_name)
        with self._waves_lock:
            self.waves[wave.id] = wave

        self._wave_stack.append(wave.id)
        try:
            yield wave
        finally:
            self._wave_stack.pop()

    def enqueue(self, message: Message, *, delay: Optional[int] = None) -> Message:
        """Enqueue a message.

        Parameters:
          message(Message): The message to enqueue.
          delay(int): The minimum amount of time, in milliseconds, to
            delay the message by.

        Raises:
          QueueNotFound: If the queue the message is being enqueued on
            doesn't exist.
        """
        queue_name = message.queue_name
        if delay is not None:
            queue_name = dq_name(queue_name)
            message_eta = current_millis() + delay
            # Preserve the wave id on the delayed copy.  When the eta
            # passes, the worker's consumer thread moves the message
            # back onto the main queue -- outside of any wave context
            # -- so the id has to travel with the message rather than
            # being re-derived from the thread-local wave stack then.
            dq_options = {"eta": message_eta}
            wave_id = message.options.get(WAVE_ID_OPTION)
            if wave_id is None and self._wave_stack:
                wave_id = self._wave_stack[-1]
            if wave_id is not None:
                dq_options[WAVE_ID_OPTION] = wave_id
            message = message.copy(
                queue_name=queue_name,
                options=dq_options,
            )

        if queue_name not in self.queues:
            raise QueueNotFound(queue_name)

        # Tag the message with the wave it belongs to, if any.  A
        # message that already carries a wave id (e.g. a delayed
        # message being moved back onto its main queue) keeps it, so
        # the wave is never double-counted across the re-enqueue.
        wave_id = message.options.get(WAVE_ID_OPTION)
        if wave_id is None and self._wave_stack:
            wave_id = self._wave_stack[-1]
            message = message.copy(options={WAVE_ID_OPTION: wave_id})

        self.emit_before("enqueue", message, delay)
        self.queues[queue_name].put(message.encode())
        self.emit_after("enqueue", message, delay)

        if wave_id is not None:
            self._track_enqueue(wave_id)
        return message

    def _track_enqueue(self, wave_id: str) -> None:
        """Account for a newly enqueued wave message."""
        with self._waves_lock:
            wave = self.waves.get(wave_id)
        if wave is None:
            # The wave was discarded (e.g. via flush_all); the
            # message is untracked and behaves like an ordinary one.
            return

        with wave.cond:
            wave.pending += 1
            wave.cond.notify_all()

    def _track_settlement(self, message: MessageProxy, *, failed: bool) -> None:
        """Account for an acked or nacked wave message."""
        wave_id = message.options.get(WAVE_ID_OPTION)
        if wave_id is None:
            return

        with self._waves_lock:
            wave = self.waves.get(wave_id)
        if wave is None:
            return

        with wave.cond:
            wave.pending -= 1
            if failed:
                wave.dead_letters.append(message)
            wave.cond.notify_all()

    def flush(self, queue_name: str) -> None:
        """Drop all the messages from a queue.

        Wave messages dropped this way are also removed from their
        wave's outstanding count, so a flushed wave can still settle.

        Parameters:
          queue_name(str): The queue to flush.
        """
        for data in iter_queue(self.queues[queue_name]):
            self.queues[queue_name].task_done()
            self._settle_encoded_message(data)

    def flush_all(self) -> None:
        """Drop all messages from all declared queues."""
        for queue_name in chain(self.queues, self.delay_queues):
            self.flush(queue_name)

        # NOTE: do not clear the dead_letters_by_queue to avoid orphaning the references in existing consumers.
        for dlq in self.dead_letters_by_queue.values():
            dlq.clear()

        # Drop all waves so their counters and dead-letter references
        # don't accumulate across tests that share a broker.
        with self._waves_lock:
            self.waves.clear()

    def _settle_encoded_message(self, data: bytes) -> None:
        """Settle the wave (if any) of an encoded message removed
        from a queue without going through a consumer, e.g. on flush.
        """
        try:
            message = Message.decode(data)
        except Exception:
            return

        wave_id = message.options.get(WAVE_ID_OPTION)
        if wave_id is None:
            return

        with self._waves_lock:
            wave = self.waves.get(wave_id)
        if wave is None:
            return

        with wave.cond:
            wave.pending -= 1
            wave.cond.notify_all()

    def join(
        self,
        queue_name: str,
        *,
        timeout: Optional[int] = None,
        fail_fast: Optional[bool] = None,
        wave: Optional[Wave] = None,
    ) -> None:
        """Wait for all the messages on the given queue to be
        processed.  This method is only meant to be used in tests
        to wait for all the messages in a queue to be processed.

        When ``wave`` is given, only the messages belonging to that
        wave are waited on; unrelated jobs sharing the queue do not
        keep this call from returning.  Without it, the whole queue
        (including its delay queue) is drained exactly as before.

        Raises:
          QueueJoinTimeout: When the timeout elapses.
          QueueNotFound: If the given queue was never declared.

        Parameters:
          queue_name(str): The queue to wait on.
          fail_fast(bool): When this is True and any message gets
            dead-lettered during the join, then an exception will be
            raised. When False, no exception will be raised.
            Defaults to None, which means use the value of the
            ``fail_fast_default`` instance attribute
            (which defaults to True).
          timeout(Optional[int]): The max amount of time, in
            milliseconds, to wait on this queue.
          wave(Optional[Wave]): A wave obtained from
            :meth:`wave` to wait on instead of the whole queue.

        .. versionchanged:: 2.0.0
           The ``fail_fast`` parameter now defaults to ``self.fail_fast_default``
           (which defaults to True).
        """
        try:
            queues = [
                self.queues[queue_name],
                self.queues[dq_name(queue_name)],
            ]
        except KeyError:
            raise QueueNotFound(queue_name) from None

        if wave is not None:
            wave.join(timeout=timeout, fail_fast=fail_fast)
            return

        deadline = timeout and time.monotonic() + timeout / 1000
        should_fail_fast = fail_fast if fail_fast is not None else self.fail_fast_default
        while True:
            for queue in queues:
                join_timeout = deadline and deadline - time.monotonic()
                join_queue(queue, timeout=join_timeout)

            # We cycle through $queue then $queue.DQ then $queue
            # again in case the messages that were on the DQ got
            # moved back on $queue.
            for queue in queues:
                if queue.unfinished_tasks:
                    break
            else:
                if should_fail_fast:
                    for message in self.dead_letters_by_queue[queue_name]:
                        raise (message._exception or Exception("Message failed with unknown error")) from None

                return


class _StubConsumer(Consumer):
    def __init__(self, queue, dead_letters, prefetch, timeout, broker):
        self.queue = queue
        self.dead_letters = dead_letters
        self.prefetch = prefetch
        self.timeout = timeout
        self.broker = broker

        # Use a Semaphore to manage 'slots' for prefetched messages.
        self.prefetch_semaphore = threading.Semaphore(value=prefetch)

    def ack(self, message):
        self.queue.task_done()
        self.broker._track_settlement(message, failed=False)
        self.prefetch_semaphore.release()

    def nack(self, message):
        self.queue.task_done()
        self.dead_letters.append(message)
        self.broker._track_settlement(message, failed=True)
        self.prefetch_semaphore.release()

    def __next__(self):
        # Reserve a prefetch 'slot' for the message being fetched.
        if not self.prefetch_semaphore.acquire(timeout=self.timeout / 1000):
            return None  # No slot available, return None

        try:
            data = self.queue.get(timeout=self.timeout / 1000)
            message = Message.decode(data)
            return _StubMessageProxy(message)
        except Empty:
            # No message available, release 'slot' that was reserved.
            self.prefetch_semaphore.release()
            return None


class _StubMessageProxy(MessageProxy):
    def clear_exception(self) -> None:
        """Override so that Exceptions are NOT cleared from MessageProxy
        when the StubBroker is used.

        Normally, clearing exceptions is required to break a reference cycle involving
        the Exception, to avoid memory bloat.

        Instead, when using the StubBroker, let the GC handle the cycle once the message
        is no longer in use.
        This lets us keep showing full stack traces in failing tests.
        See comment in 'Worker' where clear_exception() is called for details.

        Not clearing Exceptions is also required for the StubBroker.join() function
        to be able to re-raise the exception from the actor, when fail_fast=True is used.
        """
