from __future__ import annotations

from contextlib import contextmanager

import pytest

import dramatiq
import dramatiq.worker
from dramatiq import MessageProxy, MessageStatus, Worker
from dramatiq.errors import ConnectionClosed
from dramatiq.middleware import Middleware
from dramatiq.results import ResultFailure, Results
from dramatiq.results.backends import StubBackend


@contextmanager
def worker(*args, **kwargs):
    try:
        worker = Worker(*args, **kwargs)
        worker.start()
        yield worker
    finally:
        worker.stop()


class FlakyConsumer:
    """A consumer stand-in that blocks settlement (ack/nack) a number
    of times with a connection error and then recovers.
    """

    def __init__(self, inner, *, failing_attempts):
        self._inner = inner
        self.failing_attempts = failing_attempts
        self.ack_attempts = 0
        self.nack_attempts = 0
        self.fetched = []
        self.acked = []
        self.nacked = []

    def __iter__(self):
        return self

    def __next__(self):
        message = next(self._inner)
        if message is not None:
            self.fetched.append(message)
        return message

    def ack(self, message):
        self.ack_attempts += 1
        if self.ack_attempts <= self.failing_attempts:
            raise ConnectionClosed("ack blocked by test")
        self.acked.append(message)
        return self._inner.ack(message)

    def nack(self, message):
        self.nack_attempts += 1
        if self.nack_attempts <= self.failing_attempts:
            raise ConnectionClosed("nack blocked by test")
        self.nacked.append(message)
        return self._inner.nack(message)

    def requeue(self, messages):
        return self._inner.requeue(messages)

    def close(self):
        return self._inner.close()


class StatusRecorder(Middleware):
    """Records the message status observed by each callback."""

    def __init__(self):
        self.events = []

    def after_message_status_change(self, broker, message, *, status, previous_status):
        self.events.append(("status_change", message.message_id, previous_status, status))

    def after_process_message(self, broker, message, *, result=None, exception=None):
        self.events.append(("after_process_message", message.message_id, message.status))

    def before_ack(self, broker, message):
        self.events.append(("before_ack", message.message_id, message.status))

    def after_ack(self, broker, message):
        self.events.append(("after_ack", message.message_id, message.status))

    def before_nack(self, broker, message):
        self.events.append(("before_nack", message.message_id, message.status))

    def after_nack(self, broker, message):
        self.events.append(("after_nack", message.message_id, message.status))


def patch_consume_with_flaky_consumer(monkeypatch, broker, queue_name, *, failing_attempts):
    """Make ``broker.consume`` return a consumer double for ``queue_name``
    whose ack/nack fails ``failing_attempts`` times before recovering.
    """
    doubles = []
    original_consume = broker.consume
    target_queue_name = queue_name

    def consume(queue_name, prefetch=1, timeout=100):
        consumer = original_consume(queue_name, prefetch, timeout)
        if queue_name == target_queue_name:
            double = FlakyConsumer(consumer, failing_attempts=failing_attempts)
            doubles.append(double)
            return double
        return consumer

    monkeypatch.setattr(broker, "consume", consume)
    return doubles


def test_message_proxies_start_out_as_received(stub_broker):
    # Given a message
    @dramatiq.actor
    def do_work():
        pass

    # When I wrap it in a message proxy (as consumers do on delivery)
    proxy = MessageProxy(do_work.message())

    # Then its delivery status should start out as received
    assert proxy.status == MessageStatus.RECEIVED


def test_ack_retry_observes_processed_phase_and_notifies_success_once(stub_broker, monkeypatch):
    # Given a broker with results storage and a status-recording middleware
    monkeypatch.setattr(dramatiq.worker, "POST_PROCESS_MESSAGE_RETRY_DELAY_SECS", 0.01)
    backend = StubBackend()
    stub_broker.add_middleware(Results(backend=backend))
    recorder = StatusRecorder()
    stub_broker.add_middleware(recorder)

    results_seen_before_settlement = []

    class ResultProbe(Middleware):
        def before_ack(self, broker, message):
            # The result must already be readable while the ack is
            # still stuck: the processing outcome is observable before
            # the broker settlement completes.
            results_seen_before_settlement.append(backend.get_result(message))

    stub_broker.add_middleware(ResultProbe())

    @dramatiq.actor(store_results=True, queue_name="flaky-ack")
    def add(x, y):
        return x + y

    # And a consumer stand-in that blocks the ack twice and then recovers
    doubles = patch_consume_with_flaky_consumer(monkeypatch, stub_broker, add.queue_name, failing_attempts=2)

    # When I send a message and wait for it to be processed
    with worker(stub_broker, worker_timeout=10, worker_threads=1):
        message = add.send(1, 2)
        stub_broker.join(add.queue_name)

    # Then the message should have been fetched exactly once (same
    # delivery, no redelivery) and the ack retried on that delivery
    # until it went through
    assert len(doubles) == 1
    double = doubles[0]
    assert len(double.fetched) == 1
    assert double.ack_attempts == 3
    assert double.acked == double.fetched

    # And the callbacks should have observed the processing outcome
    # (processed) separately from the broker settlement (acknowledged)
    events = [event for event in recorder.events if event[1] == message.message_id]
    assert ("after_process_message", message.message_id, MessageStatus.PROCESSED) in events
    assert ("before_ack", message.message_id, MessageStatus.PROCESSED) in events
    assert ("after_ack", message.message_id, MessageStatus.ACKNOWLEDGED) in events

    # And the phase transitions should have been observed in order
    status_changes = [(event[2], event[3]) for event in events if event[0] == "status_change"]
    assert status_changes == [
        (MessageStatus.RECEIVED, MessageStatus.PROCESSED),
        (MessageStatus.PROCESSED, MessageStatus.ACKNOWLEDGED),
    ]

    # And the success notification should have been sent exactly once
    # for the delivery, even though the ack itself was retried
    hook_names = [event[0] for event in events]
    assert hook_names.count("before_ack") == 1
    assert hook_names.count("after_ack") == 1

    # And the result should have been readable before the broker
    # settlement completed
    assert results_seen_before_settlement == [3]

    # And reading the result should work as before
    assert message.get_result(backend=backend) == 3

    # And nothing should have been dead-lettered
    assert stub_broker.dead_letters == []


def test_nack_retry_observes_processed_phase_and_notifies_failure_once(stub_broker, monkeypatch):
    # Given a broker with results storage and a status-recording middleware
    monkeypatch.setattr(dramatiq.worker, "POST_PROCESS_MESSAGE_RETRY_DELAY_SECS", 0.01)
    backend = StubBackend()
    stub_broker.add_middleware(Results(backend=backend))
    recorder = StatusRecorder()
    stub_broker.add_middleware(recorder)

    @dramatiq.actor(store_results=True, max_retries=0, queue_name="flaky-nack")
    def fail():
        raise RuntimeError("boom")

    # And a consumer stand-in that blocks the nack once and then recovers
    doubles = patch_consume_with_flaky_consumer(monkeypatch, stub_broker, fail.queue_name, failing_attempts=1)

    # When I send a message that fails and wait for it to be settled
    with worker(stub_broker, worker_timeout=10, worker_threads=1):
        message = fail.send()
        stub_broker.join(fail.queue_name, fail_fast=False)

    # Then the message should have been fetched exactly once and the
    # nack retried on the same delivery until it went through
    assert len(doubles) == 1
    double = doubles[0]
    assert len(double.fetched) == 1
    assert double.nack_attempts == 2
    assert double.nacked == double.fetched

    # And the callbacks should have observed the processing outcome
    # (processed) separately from the broker settlement (rejected)
    events = [event for event in recorder.events if event[1] == message.message_id]
    assert ("after_process_message", message.message_id, MessageStatus.PROCESSED) in events
    assert ("before_nack", message.message_id, MessageStatus.PROCESSED) in events
    assert ("after_nack", message.message_id, MessageStatus.REJECTED) in events

    # And the phase transitions should have been observed in order
    status_changes = [(event[2], event[3]) for event in events if event[0] == "status_change"]
    assert status_changes == [
        (MessageStatus.RECEIVED, MessageStatus.PROCESSED),
        (MessageStatus.PROCESSED, MessageStatus.REJECTED),
    ]

    # And the failure notification should have been sent exactly once
    # for the delivery, even though the nack itself was retried
    hook_names = [event[0] for event in events]
    assert hook_names.count("before_nack") == 1
    assert hook_names.count("after_nack") == 1

    # And the message should have been dead-lettered
    assert [m.message_id for m in stub_broker.dead_letters] == [message.message_id]

    # And reading the failure should work as before
    with pytest.raises(ResultFailure):
        message.get_result(backend=backend)
