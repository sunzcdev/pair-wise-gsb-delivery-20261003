from __future__ import annotations

import threading
import time
from unittest.mock import Mock

import pytest

import dramatiq
from dramatiq import QueueJoinTimeout, QueueNotFound
from dramatiq.brokers.stub import WAVE_ID_OPTION


def test_stub_broker_raises_queue_error_when_consuming_undeclared_queues(stub_broker):
    # Given that I have a stub broker
    # If I attempt to consume a queue that wasn't declared
    # I expect a QueueNotFound error to be raised
    with pytest.raises(QueueNotFound):
        stub_broker.consume("idontexist")


def test_stub_broker_raises_queue_error_when_enqueueing_messages_on_undeclared_queues(stub_broker):
    # Given that I have a stub broker
    # If I attempt to enqueue a message on a queue that wasn't declared
    # I expect a QueueNotFound error to be raised
    with pytest.raises(QueueNotFound):
        stub_broker.enqueue(Mock(queue_name="idontexist"))


def test_stub_broker_raises_queue_error_when_joining_on_undeclared_queues(stub_broker):
    # Given that I have a stub broker
    # If I attempt to join on a queue that wasn't declared
    # I expect a QueueNotFound error to be raised
    with pytest.raises(QueueNotFound):
        stub_broker.join("idontexist")


def test_stub_broker_can_be_flushed(stub_broker):
    # Given that I have an actor
    @dramatiq.actor
    def do_work():
        pass

    # When I send that actor a message
    do_work.send()

    # And when there is already a message on that actor's dead-letter queue
    stub_broker.dead_letters_by_queue[do_work.queue_name].append("dead letter")

    # Then its queue should contain the right number of messages
    assert stub_broker.queues[do_work.queue_name].qsize() == 1
    assert len(stub_broker.dead_letters) == 1

    # When I flush all of the queues
    stub_broker.flush_all()

    # Then the queue should be empty
    assert stub_broker.queues[do_work.queue_name].qsize() == 0
    # and it should contain no in-progress tasks
    assert stub_broker.queues[do_work.queue_name].unfinished_tasks == 0
    # and the dead-letter queue should be empty
    assert len(stub_broker.dead_letters) == 0


def test_stub_broker_can_join_with_timeout(stub_broker, stub_worker):
    # Given that I have an actor that takes a long time to run
    @dramatiq.actor
    def do_work():
        time.sleep(1)

    # When I send that actor a message
    do_work.send()

    # And join on its queue with a timeout
    # Then I expect a QueueJoinTimeout to be raised
    with pytest.raises(QueueJoinTimeout):
        stub_broker.join(do_work.queue_name, timeout=500)


def test_stub_broker_join_reraises_actor_exceptions_in_the_joining_current_thread(stub_broker, stub_worker):
    # Given that I have an actor that always fails with a custom exception
    class CustomError(Exception):
        pass

    @dramatiq.actor(max_retries=0)
    def do_work():
        raise CustomError("well, shit")

    # When I send that actor a message
    do_work.send()

    # And join on its queue
    # Then that exception should be raised in my thread
    with pytest.raises(CustomError):
        stub_broker.join(do_work.queue_name, fail_fast=True)


def test_stub_broker_flush_all_does_not_break_dead_letters(stub_broker, stub_worker):
    class SomeError(Exception):
        pass

    @dramatiq.actor(max_retries=0)
    def do_work():
        raise SomeError(":(")

    do_work.send()
    with pytest.raises(SomeError):
        stub_broker.join(do_work.queue_name)

    # Flush queues then retry. Should have the same result
    stub_broker.flush_all()

    do_work.send()
    with pytest.raises(SomeError):
        stub_broker.join(do_work.queue_name)


def test_stub_broker_respects_prefetch(stub_broker, stub_worker):
    """Test that the stub broker respect the prefetch option."""

    # Adjust Worker to only prefetch 5 messages
    stub_worker.queue_prefetch = 5

    @dramatiq.actor()
    def do_work():
        pass

    # Pause just worker threads (not consumers) so messages are not removed from worker.work_queue
    for worker_thread in stub_worker.workers:
        worker_thread.pause()
        worker_thread.paused_event.wait()

    # Put 15 messages on the queue
    for _ in range(15):
        do_work.send()

    time.sleep(0.01)

    # 5 messages will be prefetched and should be on the work_queue
    assert stub_worker.work_queue.qsize() == 5
    # 10 messages will be left on the broker queue.
    assert stub_broker.queues[do_work.queue_name].qsize() == 10


def test_stub_broker_wave_raises_queue_error_for_undeclared_queues(stub_broker):
    # Given that I have a stub broker
    # If I open a wave on a queue that wasn't declared
    # I expect a QueueNotFound error to be raised
    with pytest.raises(QueueNotFound):
        with stub_broker.wave("idontexist"):
            pass


def test_stub_broker_wave_waits_only_on_its_own_messages(stub_broker, stub_worker):
    # Given that I have an actor
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    # When I send a batch of messages inside a wave
    with stub_broker.wave(do_work.queue_name) as wave:
        for x in range(5):
            do_work.send(x)

        # Then the wave should already know how many messages it owns
        assert wave.pending == 5

    # And wait on the wave
    wave.join(timeout=10000)

    # Then every message in the wave should have been processed
    assert wave.is_settled
    assert sorted(seen) == [0, 1, 2, 3, 4]


def test_stub_broker_wave_join_blocks_without_a_timeout(stub_broker, stub_worker):
    # Given an actor
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    # When I submit a wave and join it without passing a timeout
    with stub_broker.wave(do_work.queue_name) as wave:
        do_work.send(1)

    # Then the join blocks indefinitely until the wave settles
    wave.join()
    assert wave.is_settled
    assert seen == [1]


def test_stub_broker_wave_ignores_independent_messages_on_the_same_queue(stub_broker, stub_worker):
    # Given an actor whose jobs keep producing more jobs
    stop = threading.Event()
    runs = {"n": 0}

    @dramatiq.actor
    def chatter():
        if not stop.is_set():
            runs["n"] += 1
            chatter.send()

    # And an actor that just does a fixed amount of work
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    # When one wave keeps the shared queue busy forever
    with stub_broker.wave(chatter.queue_name) as noisy_wave:
        chatter.send()

    # And a second, finite wave is submitted to the same queue
    with stub_broker.wave(do_work.queue_name) as quiet_wave:
        for x in range(3):
            do_work.send(x)

    # Then the finite wave completes even though the noisy wave never runs dry
    quiet_wave.join(timeout=10000)
    assert quiet_wave.is_settled
    assert sorted(seen) == [0, 1, 2]

    # And the noisy wave still has unsettled messages of its own
    assert not noisy_wave.is_settled
    assert noisy_wave.pending > 0
    assert runs["n"] > 0

    # Until its source of new work is cut off, after which it settles too
    stop.set()
    noisy_wave.join(timeout=10000)
    assert noisy_wave.is_settled


def test_stub_broker_wave_counts_retries_as_part_of_the_same_round(stub_broker, stub_worker):
    # Given an actor that fails a couple of times before succeeding
    attempts = {"n": 0}

    @dramatiq.actor(max_retries=2, min_backoff=10, max_backoff=20)
    def flaky():
        attempts["n"] += 1
        if attempts["n"] < 3:
            raise RuntimeError("boom")

    # When I run it inside a wave
    with stub_broker.wave(flaky.queue_name) as wave:
        flaky.send()
    wave.join(timeout=10000)

    # Then all retry attempts are attributed to the wave and it settles
    assert attempts["n"] == 3
    assert wave.is_settled


def test_stub_broker_wave_counts_pipeline_followups_as_part_of_the_same_round(stub_broker, stub_worker):
    # Given two pipelined actors
    seen = []

    @dramatiq.actor
    def stage1(x):
        seen.append(("stage1", x))
        return x + 1

    @dramatiq.actor
    def stage2(x):
        seen.append(("stage2", x))

    # When I run the pipeline inside a wave
    with stub_broker.wave(stage1.queue_name) as wave:
        (stage1.message(1) | stage2.message()).run()
    wave.join(timeout=10000)

    # Then both stages -- including the follow-up derived mid-processing --
    # are part of the wave and it only settles once the last one finishes
    assert wave.is_settled
    assert [list(item) for item in seen] == [["stage1", 1], ["stage2", 2]]


def test_stub_broker_wave_waits_on_delayed_messages(stub_broker, stub_worker):
    # Given an actor
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    # When I submit a delayed message inside a wave
    with stub_broker.wave(do_work.queue_name) as wave:
        message = do_work.send_with_options(args=(1,), delay=200)
        assert wave.pending == 1

    # Then the wave id survives the move onto the delay queue, where it
    # sits until the consumer thread re-enqueues it on the main queue.
    assert message.options[WAVE_ID_OPTION] == wave.id

    # Then the wave stays unsettled while the message sits on the delay queue
    assert not wave.is_settled

    # And settles once the eta passes and the message is processed
    wave.join(timeout=10000)
    assert seen == [1]
    assert wave.is_settled


def test_stub_broker_wave_empty_batch_settles_immediately(stub_broker, stub_worker):
    # Given a wave to which nothing is ever submitted
    @dramatiq.actor
    def do_work():
        pass

    # When I wait on it
    with stub_broker.wave(do_work.queue_name) as wave:
        pass

    # Then it is already settled and joining does not block
    assert wave.is_settled
    wave.join(timeout=1000)


def test_stub_broker_wave_join_times_out(stub_broker, stub_worker):
    # Given an actor that takes a while to run
    @dramatiq.actor
    def slow():
        time.sleep(1)

    # When I submit it inside a wave and join with a short timeout
    with stub_broker.wave(slow.queue_name) as wave:
        slow.send()

    # Then a QueueJoinTimeout is raised
    with pytest.raises(QueueJoinTimeout):
        wave.join(timeout=50)

    # And the wave can still be joined once the work is done
    wave.join(timeout=10000)
    assert wave.is_settled


def test_stub_broker_wave_fail_fast_reraises_actor_exceptions(stub_broker, stub_worker):
    # Given an actor that always fails
    class CustomError(Exception):
        pass

    @dramatiq.actor(max_retries=0)
    def doomed():
        raise CustomError("nope")

    # When I run it inside a wave and join with fail_fast enabled
    with stub_broker.wave(doomed.queue_name) as wave:
        doomed.send()

    # Then the actor's exception is raised in the joining thread
    with pytest.raises(CustomError):
        wave.join(timeout=10000, fail_fast=True)

    # And the wave is still considered settled since the message was dead-lettered
    assert wave.is_settled
    assert len(wave.dead_letters) == 1


def test_stub_broker_wave_join_with_fail_fast_disabled_does_not_raise(stub_broker, stub_worker):
    class CustomError(Exception):
        pass

    @dramatiq.actor(max_retries=0)
    def doomed():
        raise CustomError("nope")

    with stub_broker.wave(doomed.queue_name) as wave:
        doomed.send()

    # No exception escapes and the wave settles via the dead letter
    wave.join(timeout=10000, fail_fast=False)
    assert wave.is_settled
    assert len(wave.dead_letters) == 1


def test_stub_broker_join_accepts_a_wave(stub_broker, stub_worker):
    # Given an actor and a wave
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    with stub_broker.wave(do_work.queue_name) as wave:
        do_work.send(1)

    # When I join through the broker's queue-level entry point
    stub_broker.join(do_work.queue_name, wave=wave, timeout=10000)

    # Then the wave is awaited just like calling wave.join() directly
    assert wave.is_settled
    assert seen == [1]


def test_stub_broker_wave_stamps_messages_with_its_id(stub_broker):
    # Given an actor
    @dramatiq.actor
    def do_work():
        pass

    # When I send a message inside a wave
    with stub_broker.wave(do_work.queue_name) as wave:
        message = do_work.send()

    # Then the enqueued message is tagged with the wave's id
    assert message.options[WAVE_ID_OPTION] == wave.id
    assert stub_broker.waves[wave.id] is wave

    # While messages sent outside of any wave remain untagged
    plain = do_work.send()
    assert WAVE_ID_OPTION not in plain.options


def test_stub_broker_wave_flush_settles_outstanding_wave_messages(stub_broker):
    # Given an actor and a wave with unprocessed messages
    @dramatiq.actor
    def do_work():
        pass

    with stub_broker.wave(do_work.queue_name) as wave:
        do_work.send()
        do_work.send()
    assert wave.pending == 2

    # When the queue is flushed without a worker running
    stub_broker.flush(do_work.queue_name)

    # Then the flushed messages are no longer outstanding on the wave
    assert wave.is_settled


def test_stub_broker_legacy_queue_join_still_waits_on_the_whole_queue(stub_broker, stub_worker):
    # Given an actor with messages submitted both inside and outside of a wave
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    with stub_broker.wave(do_work.queue_name):
        do_work.send(1)
    do_work.send(2)

    # When I use the ordinary, wave-agnostic join
    stub_broker.join(do_work.queue_name, timeout=10000)

    # Then every message on the queue -- wave or not -- is processed
    assert sorted(seen) == [1, 2]


def test_stub_broker_nested_waves_attribute_messages_to_the_innermost_round(stub_broker, stub_worker):
    seen = []

    @dramatiq.actor
    def do_work(x):
        seen.append(x)

    # Given an outer wave with an inner wave
    with stub_broker.wave(do_work.queue_name) as outer:
        do_work.send("outer")

        with stub_broker.wave(do_work.queue_name) as inner:
            do_work.send("inner")
            assert inner.pending == 1
            assert outer.pending == 1

        # After the inner context closes, new messages go back to the outer wave
        do_work.send("outer-2")
        assert outer.pending == 2

    inner.join(timeout=10000)
    assert inner.is_settled
    outer.join(timeout=10000)
    assert outer.is_settled
    assert sorted(seen) == ["inner", "outer", "outer-2"]
