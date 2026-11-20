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

import dataclasses
import os
import time
from collections import defaultdict
from functools import lru_cache
from queue import Empty, PriorityQueue
from threading import Event, RLock, Thread
from typing import TYPE_CHECKING, Iterable, Optional, Union

from .broker import Broker, Consumer, MessageProxy
from .common import current_millis, iter_queue, join_all, q_name
from .errors import ActorNotFound, BrokerConnectionError, RateLimitExceeded, Retry
from .logging import get_logger
from .middleware import Middleware, SkipMessage
from .results.middleware import Results

#: The number of milliseconds to wait before restarting consumers
#: after a connection error.
CONSUMER_RESTART_DELAY = int(os.getenv("dramatiq_restart_delay", 3000))
CONSUMER_RESTART_DELAY_SECS = CONSUMER_RESTART_DELAY / 1000

#: The number of seconds to wait before retrying post_process_message
#: calls after a connection error.
POST_PROCESS_MESSAGE_RETRY_DELAY_SECS = 5

#: The number of messages to prefetch from the queue for each worker.
#: In-progress messages are included in the count. When set to "1",
#: a new message is only fetched once a previous one is done. If no
#: provided, two times the number of configured worker threads are
#: prefetched.
QUEUE_PREFETCH = int(os.getenv("dramatiq_queue_prefetch", 0))
#: The number of messages to prefetch from the delay queue for each worker.
#: When set to "1", a new message is only fetched once a previous one is done. If no
#: provided, a thousand times the number of configured worker threads are
#: prefetched.
DELAY_QUEUE_PREFETCH = int(os.getenv("dramatiq_delay_queue_prefetch", 0))

#: The number of milliseconds workers should wake up after if the queue is idle.
WORKER_TIMEOUT = int(os.getenv("dramatiq_worker_timeout", 1000))


class Worker:
    """Workers consume messages off of all declared queues and
    distribute those messages to individual worker threads for
    processing.  Workers don't block the current thread so it's
    up to the caller to keep it alive.

    Don't run more than one Worker per process.

    Parameters:
      broker(Broker)
      queues(set[str]): An optional subset of queues to listen on.  By
        default, if this is not provided, the worker will listen on
        all declared queues.
      worker_timeout(int): The number of milliseconds workers should
        wake up after if the queue is idle.
      worker_threads(int): The number of worker threads to spawn.
    """

    def __init__(
        self,
        broker: Broker,
        *,
        queues: Optional[set[str]] = None,
        worker_timeout: int = WORKER_TIMEOUT,
        worker_threads: int = 8,
    ) -> None:
        self.logger = get_logger(__name__, type(self))
        self.broker = broker

        self.consumers: dict[str, "ConsumerThread"] = {}
        self.consumer_whitelist = queues and set(queues)
        # Load a small factor more messages than there are workers to
        # avoid waiting on network IO as much as possible.  The factor
        # must be small so we don't starve other workers out.
        self.queue_prefetch = QUEUE_PREFETCH or min(worker_threads * 2, 65535)
        # Load a large factor more delay messages than there are
        # workers as those messages could have far-future etas.
        self.delay_prefetch = DELAY_QUEUE_PREFETCH or min(worker_threads * 1000, 65535)

        self.workers: list[WorkerThread] = []
        self.work_queue: PriorityQueue[_WorkQueueItem] = PriorityQueue()
        self.worker_timeout = worker_timeout
        self.worker_threads = worker_threads

        # The canonical names of the queues that are individually
        # paused.  A queue's regular consumer and its delay consumer
        # share the same canonical name and so are paused together.
        # The lock guards this set as pause/resume calls may come from
        # any thread while the worker threads read from it.
        self.paused_queues: set[str] = set()
        self.paused_queues_lock = RLock()
        # Set whenever a queue is resumed so worker threads waiting on
        # a message from a previously paused queue wake up immediately.
        self.queue_resumed_event = Event()

    def start(self) -> None:
        """Initialize the worker boot sequence and start up all the
        worker threads.
        """
        self.broker.emit_before("worker_boot", self)

        worker_middleware = _WorkerMiddleware(self)
        self.broker.add_middleware(worker_middleware)
        for _ in range(self.worker_threads):
            self._add_worker()

        self.broker.emit_after("worker_boot", self)

    def pause(self) -> None:
        """Pauses all the worker threads."""
        child: Union[WorkerThread, ConsumerThread]

        for child in self.consumers.values():
            child.pause()
        for child in self.workers:
            child.pause()

        for child in self.consumers.values():
            child.paused_event.wait()
        for child in self.workers:
            child.paused_event.wait()

    def resume(self) -> None:
        """Resumes all the worker threads."""
        child: Union[WorkerThread, ConsumerThread]

        for child in self.consumers.values():
            child.resume()
        for child in self.workers:
            child.resume()

    def pause_queue(self, queue_name: str) -> None:
        """Pause consumption of a single queue without affecting the
        other queues the worker is listening on.

        The queue's regular consumer and its delay queue consumer are
        paused together, so no new messages are fetched and messages
        whose eta has yet to arrive keep their original eta instead of
        being promoted onto the regular queue.  Messages that were
        already handed off to worker threads when the pause took effect
        are still processed.

        Parameters:
          queue_name(str): The name of the queue to pause.  This may be
            either the canonical queue name or the name of its delay
            queue (e.g. ``"queue"`` or ``"queue.DQ"``); both refer to
            the same queue.
        """
        canonical_name = q_name(queue_name)
        self.logger.info("Pausing queue %r.", canonical_name)
        with self.paused_queues_lock:
            self.paused_queues.add(canonical_name)
            for name, consumer in self.consumers.items():
                if q_name(name) == canonical_name:
                    consumer.pause_queue()

    def resume_queue(self, queue_name: str) -> None:
        """Resume consumption of a previously paused queue.

        Resuming a queue while the whole worker is paused (see
        :meth:`pause`) does not override that global pause: the queue
        stays paused until the worker itself is resumed.  Delayed
        messages retain the eta they had when the queue was paused.

        Parameters:
          queue_name(str): The name of the queue to resume.  This may
            be either the canonical queue name or the name of its
            delay queue (e.g. ``"queue"`` or ``"queue.DQ"``); both
            refer to the same queue.
        """
        canonical_name = q_name(queue_name)
        self.logger.info("Resuming queue %r.", canonical_name)
        with self.paused_queues_lock:
            self.paused_queues.discard(canonical_name)
            for name, consumer in self.consumers.items():
                if q_name(name) == canonical_name:
                    consumer.resume_queue()

        # Wake up any worker thread that parked an already-fetched
        # message while the queue was paused.
        self.queue_resumed_event.set()
        self.queue_resumed_event.clear()

    def is_queue_paused(self, queue_name: str) -> bool:
        """Returns whether the given queue is currently paused at the
        queue level, independently of whether the whole worker is
        paused.

        Parameters:
          queue_name(str): The name of the queue, either canonical or
            suffixed (e.g. ``"queue"`` or ``"queue.DQ"``).
        """
        canonical_name = q_name(queue_name)
        with self.paused_queues_lock:
            return canonical_name in self.paused_queues

    def stop(self, timeout: int = 600000) -> None:
        """Gracefully stop the Worker and all of its consumers and
        workers.

        Parameters:
          timeout(int): The number of milliseconds to wait for
            everything to shut down.
        """
        self.broker.emit_before("worker_shutdown", self)
        self.logger.info("Shutting down...")

        thread: Union[WorkerThread, ConsumerThread]

        # Stop workers before consumers.  The consumers are kept alive
        # during this process so that heartbeats keep being sent to
        # the broker while workers finish their current tasks.
        self.logger.debug("Stopping workers...")
        for thread in self.workers:
            thread.stop()

        join_all(self.workers, timeout)
        self.logger.debug("Workers stopped.")
        self.logger.debug("Stopping consumers...")
        for thread in self.consumers.values():
            thread.stop()

        join_all(self.consumers.values(), timeout)
        self.logger.debug("Consumers stopped.")

        self.logger.debug("Requeueing in-memory messages...")
        messages_by_queue = defaultdict(list)
        for item in iter_queue(self.work_queue):
            messages_by_queue[item.message.queue_name].append(item.message)

        for queue_name, messages in messages_by_queue.items():
            try:
                self.consumers[queue_name].requeue_messages(messages)
            except BrokerConnectionError:
                self.logger.warning("Failed to requeue messages on queue %r.", queue_name, exc_info=True)
        self.logger.debug("Done requeueing in-progress messages.")

        self.logger.debug("Closing consumers...")
        for consumer in self.consumers.values():
            consumer.close()

        self.logger.debug("Consumers closed.")
        self.broker.emit_after("worker_shutdown", self)
        self.logger.info("Worker has been shut down.")

    def join(self) -> None:
        """Wait for this worker to complete its work in progress.
        This method is useful when testing code.
        """
        while True:
            for consumer in self.consumers.values():
                consumer.delay_queue.join()

            self.work_queue.join()

            # If nothing got put on the delay queues while we were
            # joining on the work queue then it should be safe to exit.
            # This could still miss stuff but the chances are slim.
            for consumer in self.consumers.values():
                if consumer.delay_queue.unfinished_tasks:
                    break
            else:
                if self.work_queue.unfinished_tasks:
                    continue
                return

    def _add_consumer(self, queue_name: str, *, delay: bool = False) -> None:
        if queue_name in self.consumers:
            self.logger.debug("A consumer for queue %r is already running.", queue_name)
            return

        canonical_name = q_name(queue_name)
        if self.consumer_whitelist and canonical_name not in self.consumer_whitelist:
            self.logger.debug("Dropping consumer for queue %r: not whitelisted.", queue_name)
            return

        consumer = self.consumers[queue_name] = ConsumerThread(
            broker=self.broker,
            queue_name=queue_name,
            canonical_name=canonical_name,
            prefetch=self.delay_prefetch if delay else self.queue_prefetch,
            work_queue=self.work_queue,
            paused_queues=self.paused_queues,
            paused_queues_lock=self.paused_queues_lock,
            worker_timeout=self.worker_timeout,
        )
        if self.is_queue_paused(canonical_name):
            self.logger.debug("Starting consumer for already-paused queue %r.", queue_name)
            consumer.pause_queue()
        consumer.start()

    def _add_worker(self) -> None:
        worker = WorkerThread(
            broker=self.broker,
            consumers=self.consumers,
            work_queue=self.work_queue,
            paused_queues=self.paused_queues,
            paused_queues_lock=self.paused_queues_lock,
            queue_resumed_event=self.queue_resumed_event,
            worker_timeout=self.worker_timeout,
        )
        worker.start()
        self.workers.append(worker)


class _WorkerMiddleware(Middleware):
    def __init__(self, worker: Worker) -> None:
        self.logger = get_logger(__name__, type(self))
        self.worker = worker

    def after_declare_queue(self, broker: Broker, queue_name: str) -> None:
        self.logger.debug("Adding consumer for queue %r.", queue_name)
        self.worker._add_consumer(queue_name)

    def after_declare_delay_queue(self, broker: Broker, queue_name: str) -> None:
        self.logger.debug("Adding consumer for delay queue %r.", queue_name)
        self.worker._add_consumer(queue_name, delay=True)


@dataclasses.dataclass(frozen=True, slots=True, eq=True, order=True)
class _WorkQueueItem:
    """This class is used as wrapper around MessageProxy when they are placed in a PriorityQueue.

    It uses order=True to define __lt__ etc., so the "lowest" item will be first in the queue.
    The lowest item is the one with the lowest priority, or lowest _queued_time if priority matches.
    Note the message itself is not compared.

    The intention of this is to make the PriorityQueue operate in a roughly FIFO manner for items with equal priority.
    """

    # The priority of the message on the queue. This is either actor.priority, or options.eta for delayed messages.
    priority: int
    message: MessageProxy = dataclasses.field(compare=False)
    _queued_time: int = dataclasses.field(default_factory=time.monotonic_ns)


class ConsumerThread(Thread):
    def __init__(
        self,
        *,
        broker: Broker,
        queue_name: str,
        canonical_name: str,
        prefetch: int,
        work_queue: PriorityQueue[_WorkQueueItem],
        paused_queues: set[str],
        paused_queues_lock: RLock,
        worker_timeout: int,
    ) -> None:
        super().__init__(daemon=True)

        self.logger = get_logger(__name__, "ConsumerThread(%s)" % queue_name)
        self.running = False
        self.paused = False
        self.paused_event = Event()
        self.consumer: Optional[Consumer] = None
        self.broker = broker
        self.prefetch = prefetch
        self.queue_name = queue_name
        self.canonical_name = canonical_name
        self.work_queue = work_queue
        # Shared with the Worker and the other consumer of the same
        # queue (its delay queue consumer) as well as the worker
        # threads.  Reads are lock-free; writes take the lock.
        self.paused_queues = paused_queues
        self.paused_queues_lock = paused_queues_lock
        # Serializes pause/resume transitions against handling a single
        # message so a message fetched just before a pause is always
        # either parked or placed on the work queue, never dropped.
        self.message_lock = RLock()
        self.worker_timeout = worker_timeout
        self.delay_queue: PriorityQueue[_WorkQueueItem] = PriorityQueue()
        # Messages received from the broker while this consumer's queue
        # is individually paused are parked here instead of being put on
        # the shared work queue.  They're moved onto the work queue when
        # the queue is resumed.  Keeping them out of the work queue means
        # worker threads never spin requeueing paused messages and
        # ``work_queue.join()`` isn't blocked by paused work.
        self.paused_queue: PriorityQueue[_WorkQueueItem] = PriorityQueue()

    def is_paused(self) -> bool:
        """Returns whether this consumer is paused, either because the
        whole worker was paused or because its queue was paused
        individually.
        """
        return self.paused or self.canonical_name in self.paused_queues

    def park_message(self, message: MessageProxy) -> None:
        """Hold onto a message that was fetched from the broker while
        the queue was paused.  Parked messages stay out of the shared
        work queue until :meth:`resume_queue` moves them back, and
        delayed messages keep their original eta.
        """
        self.logger.debug("Queue %r is paused. Parking message %r.", self.queue_name, message.message_id)
        if "eta" in message.options:
            self.delay_queue.put(_WorkQueueItem(message.options.get("eta", 0), message))
        else:
            actor = self.broker.get_actor(message.actor_name)
            self.paused_queue.put(_WorkQueueItem(actor.priority, message))

    def run(self) -> None:
        self.logger.debug("Running consumer thread...")
        self.running = True
        self.broker.emit_after("consumer_thread_boot", self)
        while self.running:
            if self.is_paused():
                self.logger.debug("Consumer is paused. Sleeping for %.02fms...", self.worker_timeout)
                # Only the global pause waits on ``paused_event``; a
                # queue-level pause doesn't touch it so a globally
                # paused worker can still wait for every thread.
                if self.paused:
                    self.paused_event.set()
                time.sleep(self.worker_timeout / 1000)
                continue

            try:
                self.consumer = self.broker.consume(
                    queue_name=self.queue_name,
                    prefetch=self.prefetch,
                    timeout=self.worker_timeout,
                )

                # Use an explicit iterator so the pause state can be
                # checked *before* each fetch.  A blocking fetch may
                # still have been in flight when the pause happened, in
                # which case the returned message is parked instead of
                # being processed or lost.
                consumer_iter = iter(self.consumer)
                while self.running:
                    if self.is_paused():
                        break

                    try:
                        message = next(consumer_iter)
                    except StopIteration:
                        # The underlying consumer gave up its iterator;
                        # drop back so a fresh consumer is created.
                        break

                    if message is not None:
                        with self.message_lock:
                            if self.is_paused():
                                # The pause landed while the fetch was
                                # blocking.  Hold onto the message (it's
                                # already off the broker) until resume.
                                self.park_message(message)
                                break
                            self.handle_message(message)

                    # Recheck after handling the message: the queue may
                    # have been paused while this iteration was running,
                    # in which case delayed messages must keep their eta
                    # instead of being promoted.
                    if self.is_paused():
                        break

                    with self.message_lock:
                        if self.is_paused():
                            break
                        self.handle_delayed_messages()

                # A pause interrupts the consume loop without breaking
                # the underlying consumer, so skip the restart path and
                # go straight back to the outer pause handling.
                if self.is_paused():
                    continue

            except BrokerConnectionError as e:
                self.logger.critical("Consumer encountered a connection error: %s", e)
                self.delay_queue = PriorityQueue()

            except Exception:
                self.logger.critical("Consumer encountered an unexpected error.", exc_info=True)
                # Avoid leaving any open file descriptors around when
                # an exception occurs.
                self.close()

            # While the consumer is running (i.e. hasn't been shut down),
            # try to restart it once a second.
            if self.running:
                self.logger.info("Restarting consumer in %0.2f seconds.", CONSUMER_RESTART_DELAY_SECS)
                self.close()
                time.sleep(CONSUMER_RESTART_DELAY_SECS)

        # If it's no longer running, then shut it down gracefully.
        self.broker.emit_before("consumer_thread_shutdown", self)
        self.logger.debug("Consumer thread stopped.")

    def handle_delayed_messages(self) -> None:
        """Enqueue any delayed messages whose eta has passed."""
        # Don't promote delayed messages while this queue is paused.
        # The messages stay in the delay queue with their original eta
        # so resuming the queue doesn't change when they become due.
        if self.is_paused():
            return

        for item in iter_queue(self.delay_queue):
            eta = item.priority
            message = item.message
            if eta > current_millis():
                self.delay_queue.put(item)
                self.delay_queue.task_done()
                break

            queue_name = q_name(message.queue_name)
            new_message = message.copy(queue_name=queue_name)
            del new_message.options["eta"]

            self.broker.enqueue(new_message)
            self.post_process_message(message)
            self.delay_queue.task_done()

    def handle_message(self, message: MessageProxy) -> None:
        """Handle a message received off of the underlying consumer.
        If the message has an eta, delay it.  Otherwise, put it on the
        work queue.
        """
        if "eta" in message.options:
            if message.options["eta"] is None:
                del message.options["eta"]
            elif not isinstance(message.options["eta"], (int, float)):
                self.logger.warning(
                    "Invalid eta value for message %r: %r. Setting eta to current_millis().",
                    message.message_id,
                    message.options["eta"],
                )
                message.options["eta"] = current_millis()

        try:
            if "eta" in message.options:
                self.logger.debug("Pushing message %r onto delay queue.", message.message_id)
                self.broker.emit_before("delay_message", message)
                self.delay_queue.put(_WorkQueueItem(message.options.get("eta", 0), message))

            else:
                actor = self.broker.get_actor(message.actor_name)
                self.logger.debug("Pushing message %r onto work queue.", message.message_id)
                self.work_queue.put(_WorkQueueItem(actor.priority, message))
        except ActorNotFound as e:
            self.logger.error(
                "Received message for undefined actor %r. Moving it to the DLQ.",
                message.actor_name,
                exc_info=True,
            )
            message.fail()
            message.stuff_exception(e)
            self.post_process_message(message)

    def post_process_message(self, message: MessageProxy) -> None:
        """Called by worker threads whenever they're done processing
        individual messages, signaling that each message is ready to
        be acked or rejected.
        """
        if TYPE_CHECKING:
            assert self.consumer
        while True:
            try:
                if message.failed:
                    self.logger.debug("Rejecting message %r.", message.message_id)
                    self.broker.emit_before("nack", message)
                    self.consumer.nack(message)
                    self.broker.emit_after("nack", message)

                else:
                    self.logger.debug("Acknowledging message %r.", message.message_id)
                    self.broker.emit_before("ack", message)
                    self.consumer.ack(message)
                    self.broker.emit_after("ack", message)

                return

            # This applies to the Redis broker.  The alternative to
            # constantly retrying would be to give up here and let the
            # message be re-processed after the worker is eventually
            # stopped or restarted, but we'd be doing the same work
            # twice in that case and the behaviour would surprise
            # users who don't deploy frequently.
            except BrokerConnectionError as e:
                self.logger.warning(
                    "Failed to post_process_message(%s) due to a connection error: %s\n"
                    "The operation will be retried in %s seconds until the connection recovers.\n"
                    "If you restart this worker before this operation succeeds, the message will be re-processed later.",
                    message,
                    e,
                    POST_PROCESS_MESSAGE_RETRY_DELAY_SECS,
                )

                time.sleep(POST_PROCESS_MESSAGE_RETRY_DELAY_SECS)
                continue

            # Not much point retrying here so we bail.  Most likely,
            # the message will be re-run after the worker is stopped
            # or restarted (because its ack lease will have expired).
            except Exception:  # pragma: no cover
                self.logger.exception(
                    "Unhandled error during post_process_message(%s).  You've found a bug in Dramatiq.  Please report it!\n"
                    "Although your message has been processed, it will be processed again once this worker is restarted.",
                    message,
                )

                return

    def requeue_messages(self, messages: Iterable[MessageProxy]) -> None:
        """Called on worker shutdown and whenever there is a
        connection error to move unacked messages back to their
        respective queues asap.
        """
        if TYPE_CHECKING:
            assert self.consumer
        self.consumer.requeue(messages)

    def pause(self) -> None:
        """Pause this consumer."""
        self.paused = True
        self.paused_event.clear()

    def resume(self) -> None:
        """Resume this consumer."""
        self.paused = False
        self.paused_event.clear()

    def pause_queue(self) -> None:
        """Pause this consumer as part of a queue-level pause.

        Unlike :meth:`pause`, this doesn't touch ``paused_event`` so a
        global :meth:`Worker.pause` can still independently wait on
        every consumer thread.  Acquiring ``message_lock`` makes sure no
        message handling is in flight when the pause takes effect.
        """
        with self.message_lock:
            with self.paused_queues_lock:
                self.paused_queues.add(self.canonical_name)

    def resume_queue(self) -> None:
        """Undo a queue-level pause of this consumer.

        If the whole worker is paused, the consumer stays effectively
        paused through its ``paused`` flag until the worker resumes.
        Any messages parked while the queue was paused are moved back
        onto the shared work queue.
        """
        with self.message_lock:
            with self.paused_queues_lock:
                self.paused_queues.discard(self.canonical_name)

        for item in iter_queue(self.paused_queue):
            self.logger.debug("Moving parked message %r onto work queue.", item.message.message_id)
            self.work_queue.put(item)
            self.paused_queue.task_done()

    def stop(self) -> None:
        """Initiate the ConsumerThread shutdown sequence.

        Code calling this method should then join on the thread and
        wait for it to finish shutting down.
        """
        self.logger.debug("Stopping consumer thread...")
        self.running = False

    def close(self) -> None:
        """Close this consumer thread and its underlying connection."""
        try:
            if self.consumer:
                # Messages parked by a queue-level pause were already
                # fetched from the broker but never acked, so they need
                # to be requeued just like the in-memory delay messages.
                parked_messages = [item.message for item in iter_queue(self.paused_queue)]
                delayed_messages = [item.message for item in iter_queue(self.delay_queue)]
                self.requeue_messages(delayed_messages + parked_messages)
                self.consumer.close()
        except BrokerConnectionError:
            pass


class WorkerThread(Thread):
    """WorkerThreads process incoming messages off of the work queue
    on a loop.  By themselves, they don't do any sort of network IO.

    Parameters:
      broker(Broker)
      consumers(dict[str, ConsumerThread])
      work_queue(PriorityQueue[_WorkQueueItem])
      worker_timeout(int)
    """

    def __init__(
        self,
        *,
        broker: Broker,
        consumers: dict[str, ConsumerThread],
        work_queue: PriorityQueue[_WorkQueueItem],
        paused_queues: set[str],
        paused_queues_lock: RLock,
        queue_resumed_event: Event,
        worker_timeout: int,
    ) -> None:
        super().__init__(daemon=True)

        self.logger = get_logger(__name__, "WorkerThread")
        self.running = False
        self.paused = False
        self.paused_event = Event()
        self.broker = broker
        self.consumers = consumers
        self.work_queue = work_queue
        # Shared with the Worker and its consumer threads.  Reads are
        # lock-free; writes take the lock.
        self.paused_queues = paused_queues
        self.paused_queues_lock = paused_queues_lock
        # Signalled by the Worker whenever a queue is resumed so worker
        # threads that parked a work-queue item can wake up promptly
        # instead of polling.
        self.queue_resumed_event = queue_resumed_event
        self.timeout = worker_timeout / 1000

    def is_queue_paused(self, queue_name: str) -> bool:
        """Returns whether the given message's queue is individually
        paused.  Queue names are canonicalized so delay queues share
        the fate of their regular queue.
        """
        return q_name(queue_name) in self.paused_queues

    def run(self) -> None:
        self.logger.debug("Running worker thread...")
        self.running = True
        self.broker.emit_after("worker_thread_boot", self)
        while self.running:
            if self.paused:
                self.logger.debug("Worker is paused. Sleeping for %.02f...", self.timeout)
                self.paused_event.set()
                time.sleep(self.timeout)
                continue

            try:
                item = self.work_queue.get(timeout=self.timeout)
            except Empty:
                continue

            if self.is_queue_paused(item.message.queue_name):
                # The queue got paused in the window between its
                # consumer fetching this message and a worker thread
                # picking it up.  Put it back untouched and wait for the
                # queue to resume so other queues' work isn't blocked
                # and this thread doesn't spin on the same item.
                self.logger.debug(
                    "Queue %r is paused. Requeueing message %r.",
                    item.message.queue_name,
                    item.message.message_id,
                )
                self.work_queue.put(item)
                self.work_queue.task_done()
                self.queue_resumed_event.wait(timeout=self.timeout)
                continue

            self.process_message(item.message)

        self.broker.emit_before("worker_thread_shutdown", self)
        self.logger.debug("Worker thread stopped.")

    def process_message(self, message: MessageProxy) -> None:
        """Process a message pulled off of the work queue then push it
        back to its associated consumer for post processing. Stuff any SkipMessage
        exception or BaseException into the message [proxy] so that it may be used
        by the stub broker to provide a nicer testing experience. Also used by the
        results middleware to pass exceptions into results.

        Parameters:
          message(MessageProxy)
        """
        actor = None
        try:
            self.logger.debug("Received message %s with id %r.", message, message.message_id)
            self.broker.emit_before("process_message", message)

            res = None
            if not message.failed:
                actor = self.broker.get_actor(message.actor_name)
                res = actor(*message.args, **message.kwargs)
                if (
                    res is not None
                    and message.options.get("pipe_target") is None
                    and not has_results_middleware(self.broker)
                ):
                    self.logger.warning(
                        "Actor '%s' returned a value that is not None, and you haven't added the "
                        "Results middleware to the broker, so the value has been discarded. "
                        "Consider adding the Results middleware to your broker or piping the "
                        "result into another actor." % actor.actor_name
                    )

            self.broker.emit_after("process_message", message, result=res)

        except SkipMessage as e:
            if message.failed:
                message.stuff_exception(e)
            self.logger.warning("Message %s was skipped.", message)
            self.broker.emit_after("skip_message", message)

        except BaseException as e:
            message.stuff_exception(e)

            throws = message.options.get("throws") or (actor and actor.options.get("throws"))
            if isinstance(e, RateLimitExceeded):
                self.logger.debug("Rate limit exceeded in message %s: %s.", message, e)
            elif throws and isinstance(e, throws):
                self.logger.info(
                    "Failed to process message %s with expected exception %s.",
                    message,
                    type(e).__name__,
                )
            elif not isinstance(e, Retry):
                self.logger.error(
                    "Failed to process message %s with unhandled exception.",
                    message,
                    exc_info=True,
                )

            self.broker.emit_after("process_message", message, exception=e)

        finally:
            # NOTE: There is no race here as any message that was
            # processed must have come off of a consumer.  Therefore,
            # there has to be a consumer for that message's queue so
            # this is safe.  Probably.
            self.consumers[message.queue_name].post_process_message(message)
            self.work_queue.task_done()

            # See discussion #351.  Keeping a reference to the
            # exception can lead to memory bloat because it may be a
            # while before a GC triggers.
            message.clear_exception()

    def pause(self) -> None:
        """Pause this worker."""
        self.paused = True
        self.paused_event.clear()

    def resume(self) -> None:
        """Resume this worker."""
        self.paused = False
        self.paused_event.clear()

    def stop(self) -> None:
        """Initiate the WorkerThread shutdown process.

        Code calling this method should then join on the thread and
        wait for it to finish shutting down.
        """
        self.logger.debug("Stopping worker thread...")
        self.running = False


@lru_cache(maxsize=128)
def has_results_middleware(broker: Broker) -> bool:
    return any(type(m) is Results for m in broker.middleware)


_ConsumerThread = ConsumerThread
_WorkerThread = WorkerThread
