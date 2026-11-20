from __future__ import annotations

import gc
import logging
import time
import weakref
from unittest import mock

import pytest

import dramatiq
import dramatiq.middleware
import dramatiq.worker
from dramatiq.brokers.stub import StubBroker
from dramatiq.common import current_millis, dq_name

from .common import worker


def test_workers_dont_register_queues_that_arent_whitelisted(stub_broker):
    # Given that I have a worker object with a restricted set of queues
    with worker(stub_broker, queues={"a", "b"}) as stub_worker:
        # When I try to register a consumer for a queue that hasn't been whitelisted
        stub_broker.declare_queue("c")
        stub_broker.declare_queue("c.DQ")

        # Then a consumer should not get spun up for that queue
        assert "c" not in stub_worker.consumers
        assert "c.DQ" not in stub_worker.consumers


def test_workers_process_messages_in_order(stub_broker) -> None:
    """When worker is running with only one thread, messages are processed in order.

    This is effectively testing messages are pulled from work_queue in a FIFO order."""

    results = []

    @dramatiq.actor()
    def _do_work(value):
        results.append(value)

    try:
        # Worker with one worker, but prefetches 10 messages
        stub_worker = dramatiq.worker.Worker(stub_broker, worker_threads=1)
        stub_worker.queue_prefetch = 10
        stub_worker.start()

        # Pause just worker threads (not consumers) so messages are not removed from worker.work_queue
        for worker_thread in stub_worker.workers:
            worker_thread.pause()
            worker_thread.paused_event.wait()

        # Put 5 messages on the queue
        for i in range(5):
            _do_work.send(i)
            time.sleep(0.001)

        # Check work_queue has 5 messages
        assert stub_worker.work_queue.qsize() == 5

        # Resume worker so messages are processed from work_queue
        stub_worker.resume()
        stub_broker.join(queue_name=_do_work.queue_name)
    finally:
        stub_worker.stop()

    # Check messages were processed in order, i.e. messages came off work_queue in FIFO order.
    assert results == [0, 1, 2, 3, 4]


def test_queue_item_order():
    """Test ordering of the _WorkQueueItem class.

    This is important since this class is used with PriorityQueue.
    """

    @dramatiq.actor()
    def do_work():
        pass

    message1 = do_work.message()
    message2 = do_work.message()

    message_proxy1 = dramatiq.MessageProxy(message1)
    message_proxy2 = dramatiq.MessageProxy(message2)

    # messages/items with different priorities,
    message_1_item_priority_0 = dramatiq.worker._WorkQueueItem(priority=0, message=message_proxy1, _queued_time=0)
    message_2_item_priority_1 = dramatiq.worker._WorkQueueItem(priority=1, message=message_proxy2, _queued_time=0)
    assert message_1_item_priority_0 < message_2_item_priority_1
    assert message_1_item_priority_0 <= message_2_item_priority_1
    assert not message_1_item_priority_0 > message_2_item_priority_1
    assert not message_1_item_priority_0 >= message_2_item_priority_1
    assert message_1_item_priority_0 != message_2_item_priority_1
    assert message_2_item_priority_1 > message_1_item_priority_0
    assert message_2_item_priority_1 >= message_1_item_priority_0
    assert not message_2_item_priority_1 < message_1_item_priority_0
    assert not message_2_item_priority_1 <= message_1_item_priority_0
    assert message_2_item_priority_1 != message_1_item_priority_0

    # messages/items with same priority and different queue times,
    message_1_item_time_0 = dramatiq.worker._WorkQueueItem(priority=0, message=message_proxy1, _queued_time=0)
    message_2_item_time_1 = dramatiq.worker._WorkQueueItem(priority=0, message=message_proxy2, _queued_time=1)
    assert message_1_item_time_0 < message_2_item_time_1
    assert message_1_item_time_0 <= message_2_item_time_1
    assert not message_1_item_time_0 > message_2_item_time_1
    assert not message_1_item_time_0 >= message_2_item_time_1
    assert message_1_item_time_0 != message_2_item_time_1
    assert message_2_item_time_1 > message_1_item_time_0
    assert message_2_item_time_1 >= message_1_item_time_0
    assert not message_2_item_time_1 < message_1_item_time_0
    assert not message_2_item_time_1 <= message_1_item_time_0
    assert message_2_item_time_1 != message_1_item_time_0

    # Messages/items with same priority and queue time
    message_1_item_equal = dramatiq.worker._WorkQueueItem(priority=0, message=message_proxy1, _queued_time=0)
    message_2_item_equal = dramatiq.worker._WorkQueueItem(priority=0, message=message_proxy2, _queued_time=0)
    assert not message_1_item_equal < message_2_item_equal
    assert not message_1_item_equal > message_2_item_equal
    assert message_1_item_equal <= message_2_item_equal
    assert message_1_item_equal >= message_2_item_equal
    assert message_1_item_equal == message_2_item_equal
    assert not message_2_item_equal < message_1_item_equal
    assert not message_2_item_equal > message_1_item_equal
    assert message_2_item_equal <= message_1_item_equal
    assert message_2_item_equal >= message_1_item_equal
    assert message_2_item_equal == message_1_item_equal


def test_exception_from_actor_doesnt_leak(
    stub_broker: StubBroker,
    caplog: pytest.LogCaptureFixture,
) -> None:
    """Test that an Exception raised from an Actor doesn't leak.

    Specifically, test that the Exception is not caught in a reference cycle,
    which would prevent it from being promptly destroyed/deallocated.

    An Exception caught in a reference cycle should still be eventually destroyed
    when the cyclic garbage collector runs (See https://docs.python.org/3/library/gc.html),
    but relying on this is not ideal since memory will be held for longer than needed.

    How does an Exception get caught in a reference cycle?
    This typically happens because the Exception has a Traceback object, which holds a
    reference to each Frame in the stack, which hold references to the local variables
    in each Frame.
    So if any local variable in any Frame references the Exception,
    this creates a reference cycle.
    """

    # Disable log capturing to prevent pytest from holding exception references
    # via captured LogRecord.exc_info
    caplog.set_level(logging.CRITICAL)

    # Keep a weakref to each CustomError object created
    weak_refs: list[weakref.ref[CustomError]] = []

    # Custom Error class for this test
    class CustomError(Exception):
        def __init__(self, *args) -> None:
            super().__init__(*args)
            weak_refs.append(weakref.ref(self))

    # Actor that will raise Exception and fail and retry once.
    @dramatiq.actor(max_retries=1, max_backoff=1)
    def failing_actor():
        raise CustomError()

    try:
        # Disable cyclic GC during the test so that Exceptions caught in reference cycles
        # definitely WON'T be collected / deallocated.
        gc.disable()

        with (
            # Reinstate regular clear_exeption() method
            # so exceptions are not caught in reference cycles.
            mock.patch(
                "dramatiq.brokers.stub._StubMessageProxy.clear_exception",
                new=dramatiq.MessageProxy.clear_exception,
            ),
            worker(stub_broker, worker_timeout=10, worker_threads=1) as stub_worker,
        ):
            # Run actor that will raise an Exception
            failing_actor.send()
            stub_broker.join(failing_actor.queue_name, fail_fast=False)
            stub_worker.join()

        # Check expected number of exception were collected (first run + 1 retry)
        assert len(weak_refs) == 2
        # Check that all CustomErrors raised were deallocated.
        # Because the cyclic Garbage collector is disabled (gc.disable()),
        # If the error is deallocated, it means it must NOT be in any reference cycles.
        for i, weak_ref in enumerate(weak_refs):
            assert weak_ref() is None, f"CustomError object #{i} still alive"

    finally:
        # Re-enable GC after test is finished.
        gc.enable()


def _wait_until(predicate, timeout=5.0, interval=0.01):
    """Poll ``predicate`` until it returns a truthy value or the
    timeout elapses.  Returns the last predicate result.
    """
    deadline = time.monotonic() + timeout
    result = predicate()
    while not result and time.monotonic() < deadline:
        time.sleep(interval)
        result = predicate()
    return result


def test_pausing_one_queue_doesnt_pause_the_other_queues(stub_broker: StubBroker) -> None:
    """Pausing a single queue must leave the other queues' consumers
    and worker threads running normally."""
    results_a: list[int] = []
    results_b: list[int] = []

    @dramatiq.actor(queue_name="queue_a")
    def work_a(value):
        results_a.append(value)

    @dramatiq.actor(queue_name="queue_b")
    def work_b(value):
        results_b.append(value)

    with worker(stub_broker, worker_timeout=25, worker_threads=4) as stub_worker:
        # Sanity check: a regular queue and its delay queue share one
        # canonical name, so both consumers are covered by the pause.
        assert "queue_a" in stub_worker.consumers
        assert dq_name("queue_a") in stub_worker.consumers

        stub_worker.pause_queue("queue_a")
        assert stub_worker.is_queue_paused("queue_a")
        assert stub_worker.is_queue_paused(dq_name("queue_a"))
        assert not stub_worker.is_queue_paused("queue_b")

        for i in range(5):
            work_a.send(i)
            work_b.send(i)

        # queue_b must drain completely while queue_a is paused.
        stub_broker.join("queue_b")
        stub_worker.join()
        assert sorted(results_b) == [0, 1, 2, 3, 4]
        assert results_a == []

        # Nothing from queue_a should have been processed.  Its
        # messages are either still on the broker's queue or parked in
        # the consumer's paused queue if they were fetched right before
        # the pause took effect.
        parked = stub_worker.consumers["queue_a"].paused_queue.qsize()
        assert stub_broker.queues["queue_a"].qsize() + parked == 5

        stub_worker.resume_queue("queue_a")
        assert not stub_worker.is_queue_paused("queue_a")

        stub_broker.join("queue_a")
        stub_worker.join()
        assert sorted(results_a) == [0, 1, 2, 3, 4]


def test_paused_queue_delay_messages_keep_their_eta_while_other_queue_runs(stub_broker: StubBroker) -> None:
    """Regression test: two queues, both carrying delayed messages.
    Pausing one side must not stop the other side, and on resume the
    paused queue's delayed messages must run against their original
    eta rather than being processed immediately."""
    runs: dict[str, list[int]] = {"a": [], "b": []}

    @dramatiq.actor(queue_name="queue_a")
    def work_a():
        runs["a"].append(current_millis())

    @dramatiq.actor(queue_name="queue_b")
    def work_b():
        runs["b"].append(current_millis())

    with worker(stub_broker, worker_timeout=25, worker_threads=4) as stub_worker:
        # Wait for both delay consumers to be up before pausing.
        assert _wait_until(
            lambda: dq_name("queue_a") in stub_worker.consumers and dq_name("queue_b") in stub_worker.consumers
        )
        dq_consumer_a = stub_worker.consumers[dq_name("queue_a")]

        # Pause queue_a before sending anything so its consumers are
        # guaranteed to be in their paused state when the messages
        # arrive.
        stub_worker.pause_queue("queue_a")

        delay_ms = 1500
        start_a = current_millis()
        message_a = work_a.send_with_options(delay=delay_ms)
        start_b = current_millis()
        work_b.send_with_options(delay=delay_ms)

        original_eta = message_a.options["eta"]
        assert original_eta >= start_a + delay_ms

        def delayed_message_parked():
            # The delayed message is either still on the broker's delay
            # queue (the consumer was sleeping when it arrived) or has
            # been fetched into the consumer's in-memory delay queue
            # (it arrived just before the pause took effect).  Either
            # way it must not have been promoted onto the work queue.
            # qsize() (not unfinished_tasks) is used for the broker's
            # queue because fetched messages are only counted as done
            # when they're acked after processing.
            on_broker = stub_broker.queues[dq_name("queue_a")].qsize()
            in_memory = dq_consumer_a.delay_queue.qsize()
            return on_broker + in_memory == 1

        assert _wait_until(delayed_message_parked)

        # Let the original eta pass while the queue stays paused.
        time.sleep(delay_ms / 1000 + 0.4)

        # queue_a must not have run anything and the message must still
        # be parked somewhere with its original eta intact.
        assert runs["a"] == []
        assert delayed_message_parked()
        if dq_consumer_a.delay_queue.qsize():
            assert dq_consumer_a.delay_queue.queue[0].priority == original_eta

        # queue_b is unaffected: its delayed message ran on schedule.
        assert _wait_until(lambda: len(runs["b"]) == 1)
        assert runs["b"][0] - start_b >= delay_ms

        # Resume after the original eta has already expired.  The
        # consumer fetches (or promotes) the message, whose eta is
        # already due, so it gets processed promptly against the
        # original eta.
        resume_time = current_millis()
        stub_worker.resume_queue("queue_a")

        stub_broker.join("queue_a")
        stub_worker.join()

        assert len(runs["a"]) == 1
        # The message can never run before its original eta, and once
        # resumed it should be promoted and processed promptly since
        # that eta has already passed.
        assert runs["a"][0] >= original_eta
        assert runs["a"][0] - resume_time < 1000


def test_queue_resume_cannot_override_a_global_pause(stub_broker: StubBroker) -> None:
    """Resuming a single queue while the whole worker is paused must
    not let that queue's messages through until the worker resumes."""
    results: list[int] = []

    @dramatiq.actor(queue_name="queue_a")
    def work_a(value):
        results.append(value)

    with worker(stub_broker, worker_timeout=25, worker_threads=2) as stub_worker:
        # Pause the whole worker, then pause and "resume" the queue.
        stub_worker.pause()
        stub_worker.pause_queue("queue_a")
        assert stub_worker.is_queue_paused("queue_a")

        work_a.send(1)
        time.sleep(0.2)
        assert results == []

        stub_worker.resume_queue("queue_a")
        # The queue-level pause is gone, but the global pause remains.
        assert not stub_worker.is_queue_paused("queue_a")
        time.sleep(0.2)
        assert results == []

        # Only the global resume gets messages flowing again.
        stub_worker.resume()
        stub_broker.join("queue_a")
        stub_worker.join()
        assert results == [1]


def test_global_resume_does_not_clear_a_queue_level_pause(stub_broker: StubBroker) -> None:
    """Resuming the whole worker must not drop a queue-level pause that
    was applied while the worker was paused."""
    results: list[int] = []

    @dramatiq.actor(queue_name="queue_a")
    def work_a(value):
        results.append(value)

    with worker(stub_broker, worker_timeout=25, worker_threads=2) as stub_worker:
        assert _wait_until(lambda: "queue_a" in stub_worker.consumers)

        stub_worker.pause()
        stub_worker.pause_queue("queue_a")
        work_a.send(1)

        # Resuming the worker must not resume the individually paused queue.
        stub_worker.resume()
        assert stub_worker.is_queue_paused("queue_a")
        time.sleep(0.2)
        assert results == []

        # The queue stays blocked until its own resume is called.
        stub_worker.resume_queue("queue_a")
        stub_broker.join("queue_a")
        stub_worker.join()
        assert results == [1]


def test_pausing_a_queue_covers_its_delay_queue_consumer(stub_broker: StubBroker) -> None:
    """Passing the delay queue name must pause the same logical queue
    as passing the canonical queue name, in both directions."""

    @dramatiq.actor(queue_name="queue_a")
    def work_a():
        pass

    with worker(stub_broker, worker_timeout=25, worker_threads=2) as stub_worker:
        assert _wait_until(lambda: "queue_a" in stub_worker.consumers and dq_name("queue_a") in stub_worker.consumers)

        stub_worker.pause_queue(dq_name("queue_a"))
        try:
            assert stub_worker.is_queue_paused("queue_a")
            assert stub_worker.is_queue_paused(dq_name("queue_a"))
            assert stub_worker.consumers["queue_a"].is_paused()
            assert stub_worker.consumers[dq_name("queue_a")].is_paused()
        finally:
            stub_worker.resume_queue("queue_a")

        assert not stub_worker.consumers["queue_a"].is_paused()
        assert not stub_worker.consumers[dq_name("queue_a")].is_paused()


def test_parked_messages_are_processed_after_resume(stub_broker: StubBroker) -> None:
    """A message fetched from the broker while its queue is paused must
    be parked (not lost or processed) and then handled normally once
    the queue is resumed."""
    results: list[int] = []

    @dramatiq.actor(queue_name="queue_a")
    def work_a(value):
        results.append(value)

    with worker(stub_broker, worker_timeout=25, worker_threads=2) as stub_worker:
        assert _wait_until(lambda: "queue_a" in stub_worker.consumers)
        consumer = stub_worker.consumers["queue_a"]

        # Pause first so the message can never be processed, then send.
        stub_worker.pause_queue("queue_a")
        work_a.send(1)

        # The message lands either on the broker's queue or, if a fetch
        # was already in flight, in the consumer's parked queue.
        assert _wait_until(lambda: stub_broker.queues["queue_a"].qsize() + consumer.paused_queue.qsize() == 1)
        time.sleep(0.1)
        assert results == []

        stub_worker.resume_queue("queue_a")
        stub_broker.join("queue_a")
        stub_worker.join()
        assert results == [1]
        assert consumer.paused_queue.qsize() == 0
