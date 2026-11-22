from __future__ import annotations

import asyncio
import gc
import inspect
import logging
import weakref
from threading import Event, Thread, get_ident
from unittest import mock

import pytest

from dramatiq import actor, threading
from dramatiq.asyncio import (
    EventLoopThread,
    async_to_sync,
    get_event_loop_thread,
    set_event_loop_thread,
)
from dramatiq.broker import MessageProxy
from dramatiq.brokers.stub import StubBroker
from dramatiq.logging import get_logger
from dramatiq.middleware import CurrentMessage, Retries, TimeLimit
from dramatiq.middleware.asyncio import AsyncIO
from dramatiq.middleware.time_limit import TimeLimitExceeded

from ..common import worker


@pytest.fixture
def started_thread():
    thread = EventLoopThread(logger=get_logger(__name__))
    thread.start()
    set_event_loop_thread(thread)
    yield thread
    thread.stop()
    set_event_loop_thread(None)


def test_event_loop_thread_start():
    thread = EventLoopThread(logger=get_logger(__name__))
    try:
        thread.start(timeout=1.0)
        assert isinstance(thread.loop, asyncio.BaseEventLoop)
        assert thread.loop.is_running()
    finally:
        thread.stop()
        thread.join()


def test_event_loop_thread_start_timeout():
    thread = EventLoopThread(logger=get_logger(__name__))
    loop_mock = mock.Mock()
    # Store the original thread loop and replace it with a mock.
    original_loop = thread.loop
    thread.loop = loop_mock
    with pytest.raises(RuntimeError, match="Event loop failed to start"):
        thread.start(timeout=0.1)
    # Close the original event loop to prevent a ResourceWarning.
    original_loop.close()


def test_event_loop_thread_run_coroutine(started_thread: EventLoopThread):
    result = {}

    async def get_thread_id():
        return get_ident()

    result = started_thread.run_coroutine(get_thread_id())

    # the coroutine executed in the event loop thread
    assert result == started_thread.ident


def test_event_loop_thread_run_coroutine_exception(started_thread: EventLoopThread):
    async def raise_actual_error():
        raise TypeError("bla")

    async def raise_error():
        await raise_actual_error()

    coro = raise_error()

    with pytest.raises(TypeError, match="bla") as e:
        started_thread.run_coroutine(coro)

    # the error has the correct traceback
    assert e.traceback[-2].name == "raise_error"
    assert e.traceback[-1].name == "raise_actual_error"


def test_event_loop_thread_run_coroutine_timeout_exception(started_thread: EventLoopThread):
    """Test that TimeoutError in coroutine doesn't lead to infinite loop.

    Regression test for https://github.com/Bogdanp/dramatiq/issues/791
    """

    async def raise_actual_error():
        raise TimeoutError("something took too long")

    async def raise_error():
        await raise_actual_error()

    coro = raise_error()

    with pytest.raises(TimeoutError, match="something took too long"):
        started_thread.run_coroutine(coro)


@pytest.mark.skipif(
    threading.current_platform not in threading.supported_platforms,
    reason="Threading not supported on this platform.",
)
@pytest.mark.skipif(threading.is_gevent_active(), reason="Thread exceptions not supported with gevent.")
def test_event_loop_thread_run_coroutine_interrupted(started_thread: EventLoopThread):
    """Test that when EventLoopThread.run_coroutine() is interrupted,
    the coroutine cleanup is allowed to run.

    Note: in this test the main test thread calls EventLoopThread.run_coroutine()
    AND is interrupted with a threading.Interrupt exception.
    This simulates how the Worker threads call EventLoopThread.run_coroutine()
    and are interrupted by TimeLimit/Shutdown middleware.
    """
    side_effect_target = {"cleanup": False}
    ready_for_interrupt = Event()

    # Set up a thread to interrupt the "worker thread" (i.e. the main test thread)
    # This simulates TimeLimit/Shutdown middleware.
    def interrupter(*, worker_thread_id: int):
        ready_for_interrupt.wait()
        threading.raise_thread_exception(worker_thread_id, threading.Interrupt)

    test_thread_id = get_ident()
    interrupter_thread = Thread(target=interrupter, kwargs={"worker_thread_id": test_thread_id})
    interrupter_thread.start()

    # This will be canceled when the interrupt happens.
    # This cleanup code in the 'finally' block should run.
    async def sleeper():
        try:
            ready_for_interrupt.set()
            while True:
                await asyncio.sleep(0.01)
        finally:
            await asyncio.sleep(0.01)
            side_effect_target["cleanup"] = True

    with pytest.raises(threading.Interrupt):
        started_thread.run_coroutine(sleeper())

    # Assert cleanup code was run
    assert side_effect_target["cleanup"]

    # ensure interrupter thread has finished.
    interrupter_thread.join(timeout=1)
    assert not interrupter_thread.is_alive()


def test_event_loop_thread_terminate_coroutine_cancels_and_waits(started_thread: EventLoopThread):
    """terminate_coroutine() must cancel a still-running coroutine so it
    receives a CancelledError, run its cleanup, and only return once the
    cleanup has finished.
    """
    side_effect_target = {"cancelled": False, "cleanup": False}
    coro_started = Event()

    async def stuck_coro():
        try:
            coro_started.set()
            await asyncio.sleep(10)
        except asyncio.CancelledError:
            side_effect_target["cancelled"] = True
            raise
        finally:
            # Give the event loop something to do during cleanup; this
            # would keep running in the background if termination didn't
            # wait for it.
            await asyncio.sleep(0.01)
            side_effect_target["cleanup"] = True

    done = Event()

    async def wrapped():
        try:
            return await stuck_coro()
        finally:
            done.set()

    future = asyncio.run_coroutine_threadsafe(wrapped(), started_thread.loop)
    assert coro_started.wait(timeout=1.0)

    # The future is still pending while the coroutine is stuck.
    assert not future.done()

    started_thread.terminate_coroutine(future, done)

    # By the time terminate_coroutine() returns, the coroutine must have
    # observed the cancellation and finished its cleanup.
    assert side_effect_target["cancelled"]
    assert side_effect_target["cleanup"]
    assert future.done()
    assert future.cancelled()


def test_event_loop_thread_terminate_coroutine_on_finished_future(started_thread: EventLoopThread):
    """terminate_coroutine() must be a no-op (aside from confirming the
    cleanup) when the coroutine already completed on its own.
    """
    cleanup_ran = Event()

    async def fast_coro():
        try:
            return "done"
        finally:
            cleanup_ran.set()

    done = Event()

    async def wrapped():
        try:
            return await fast_coro()
        finally:
            done.set()

    future = asyncio.run_coroutine_threadsafe(wrapped(), started_thread.loop)
    assert future.result(timeout=1.0) == "done"

    # Must not raise even though the future is already done.
    started_thread.terminate_coroutine(future, done)
    assert cleanup_ran.wait(timeout=1.0)
    assert future.result(timeout=0) == "done"


@pytest.mark.skipif(
    threading.current_platform not in threading.supported_platforms,
    reason="Threading not supported on this platform.",
)
@pytest.mark.skipif(threading.is_gevent_active(), reason="Thread exceptions not supported with gevent.")
def test_time_limit_cancels_stuck_async_actor_before_settling(stub_broker: StubBroker):
    """End-to-end: when an async actor exceeds its time limit, the
    coroutine actually running on the event loop must be cancelled and
    finish its cleanup before the worker thread delivers the
    TimeLimitExceeded failure and the message is retried/dead-lettered.
    """
    stub_broker.add_middleware(AsyncIO())
    stub_broker.add_middleware(TimeLimit(interval=20))
    stub_broker.add_middleware(Retries())

    side_effect_target = {"cancellations": 0, "cleanups": 0, "runs": 0}
    coro_started = Event()

    @actor(
        time_limit=100,
        max_retries=1,
        min_backoff=1,
        max_backoff=1,
        queue_name="stuck_async",
    )
    async def stuck_actor():
        side_effect_target["runs"] += 1
        try:
            coro_started.set()
            await asyncio.sleep(10)
        except asyncio.CancelledError:
            side_effect_target["cancellations"] += 1
            raise
        finally:
            # Await something so the cleanup itself is scheduled on the
            # event loop; this proves termination waits for cleanup.
            await asyncio.sleep(0.01)
            side_effect_target["cleanups"] += 1

    with worker(stub_broker, worker_timeout=50, worker_threads=1):
        stuck_actor.send()
        assert coro_started.wait(timeout=1.0)

        # Both the initial attempt and the retry must get time-limited.
        stub_broker.join("stuck_async", fail_fast=False, timeout=30000)

    # The coroutine was cancelled on every attempt and always ran its
    # cleanup before the worker moved on.
    assert side_effect_target["runs"] == 2
    assert side_effect_target["cancellations"] == 2
    assert side_effect_target["cleanups"] == 2

    # The message settled as a TimeLimitExceeded failure on each
    # attempt (the stub broker records one dead letter per nack).
    dead_letters = stub_broker.dead_letters_by_queue["stuck_async"]
    assert len(dead_letters) == 2
    assert all(isinstance(letter._exception, TimeLimitExceeded) for letter in dead_letters)


@pytest.mark.skipif(
    threading.current_platform not in threading.supported_platforms,
    reason="Threading not supported on this platform.",
)
@pytest.mark.skipif(threading.is_gevent_active(), reason="Thread exceptions not supported with gevent.")
def test_fast_async_actor_completes_and_next_message_processes(stub_broker: StubBroker):
    """End-to-end: an async actor that finishes immediately must not be
    affected by the time-limit/termination path, and the worker must go
    on to process the following message.
    """
    stub_broker.add_middleware(AsyncIO())
    stub_broker.add_middleware(TimeLimit(interval=20))
    stub_broker.add_middleware(Retries())

    side_effect_target = {"fast_runs": 0, "next_runs": 0, "cleanups": 0}

    @actor(time_limit=1000, max_retries=0, queue_name="fast_async")
    async def fast_actor():
        try:
            side_effect_target["fast_runs"] += 1
            return "ok"
        finally:
            side_effect_target["cleanups"] += 1

    @actor(max_retries=0, queue_name="next_async")
    async def next_actor():
        side_effect_target["next_runs"] += 1

    with worker(stub_broker, worker_timeout=50, worker_threads=1):
        fast_actor.send()
        stub_broker.join("fast_async", fail_fast=False, timeout=10000)

        # The worker thread is free, so the next message is processed.
        next_actor.send()
        stub_broker.join("next_async", fail_fast=False, timeout=10000)

    assert side_effect_target["fast_runs"] == 1
    assert side_effect_target["cleanups"] == 1
    assert side_effect_target["next_runs"] == 1

    # Nothing failed, so no dead letters.
    assert stub_broker.dead_letters_by_queue["fast_async"] == []
    assert stub_broker.dead_letters_by_queue["next_async"] == []


@mock.patch("dramatiq.middleware.asyncio.EventLoopThread")
def test_async_middleware_before_worker_boot(EventLoopThreadMock):
    middleware = AsyncIO()
    try:
        middleware.before_worker_boot(None, None)
        assert get_event_loop_thread() is EventLoopThreadMock.return_value
    finally:
        set_event_loop_thread(None)


def test_async_middleware_after_worker_shutdown():
    middleware = AsyncIO()
    event_loop_thread = mock.Mock()
    set_event_loop_thread(event_loop_thread)
    try:
        middleware.after_worker_shutdown(None, None)
        assert get_event_loop_thread() is None
    finally:
        set_event_loop_thread(None)


@mock.patch("dramatiq.asyncio.get_event_loop_thread")
def test_async_to_sync(get_event_loop_thread_mocked):
    mock_async_function = mock.Mock()
    thread = get_event_loop_thread_mocked()
    set_event_loop_thread(thread)

    # Check that a callable sync function is returned.
    fn = async_to_sync(mock_async_function)
    assert callable(fn) and inspect.isfunction(fn) and not inspect.iscoroutinefunction(fn)

    # Call the sync function
    actual = fn(2, foo="bar")
    try:
        # Check that the (kw)args were passed through to the async function.
        mock_async_function.assert_called_once_with(2, foo="bar")

        # Check that run_coroutine was called once with return value from async function
        thread.run_coroutine.assert_called_once_with(mock_async_function.return_value)
        assert actual is thread.run_coroutine.return_value

    finally:
        set_event_loop_thread(None)


async def async_fn(value: int = 2) -> int:
    return value + 1


def test_async_to_sync_with_actual_thread(started_thread):
    assert async_to_sync(async_fn)(2) == 3


def test_async_to_sync_no_thread():
    with pytest.raises(RuntimeError):
        async_to_sync(async_fn)(2)


def test_anyio_currrent_message_middleware_exposes_the_current_message(stub_broker):
    # Given that I have a CurrentMessage middleware
    stub_broker.add_middleware(AsyncIO())
    stub_broker.add_middleware(CurrentMessage())

    with worker(stub_broker, worker_timeout=100, worker_threads=1):
        # And an actor that accesses the current message
        sent_messages = []
        received_messages = []

        @actor
        async def accessor(x):
            message_proxy = CurrentMessage.get_current_message()
            received_messages.append(message_proxy._message)

        # When I send it a couple messages
        sent_messages.append(accessor.send(1))
        sent_messages.append(accessor.send(2))

        # And wait for it to finish its work
        stub_broker.join(accessor.queue_name)

        # Then the sent messages and the received messages should be the same
        assert sorted(sent_messages) == sorted(received_messages)

        # When I try to access the current message from a non-worker thread
        # Then I should get back None
        assert CurrentMessage.get_current_message() is None


def test_exception_from_async_actor_doesnt_leak(
    stub_broker: StubBroker,
    caplog: pytest.LogCaptureFixture,
) -> None:
    """Test that an Exception raised from an async Actor doesn't leak.

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

    # Add AsyncIO middleware being tested
    stub_broker.add_middleware(AsyncIO())

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

    # Actor that will raise exception and fail and retry once.
    @actor(max_retries=1, max_backoff=1)
    async def failing_actor():
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
                new=MessageProxy.clear_exception,
            ),
            worker(stub_broker, worker_timeout=10, worker_threads=1) as stub_worker,
        ):
            # Run async actor that will raise an Exception
            failing_actor.send()
            stub_broker.join(failing_actor.queue_name, fail_fast=False)
            stub_worker.join()

        # Check expected number of exception were collected (first run + 1 retry)
        assert len(weak_refs) == 2
        # Check that all CustomErrors raised were deallocated.
        # Because the cyclic Garbage collector is disabled (gc.disable()),
        # ff the error is deallocated, it means it must NOT be in any reference cycles.
        for i, weak_ref in enumerate(weak_refs):
            assert weak_ref() is None, f"CustomError object #{i} still alive"

    finally:
        # Re-enable GC after test is finished.
        gc.enable()
