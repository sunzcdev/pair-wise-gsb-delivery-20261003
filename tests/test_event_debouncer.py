from __future__ import annotations

import threading
import time
import warnings
from queue import Queue
from types import SimpleNamespace

import pytest

from watchdog.events import FileModifiedEvent
from watchdog.utils import event_debouncer
from watchdog.utils.event_debouncer import EventDebouncer


def _callback(events):
    pass


def test_debounce_interval_positional():
    debouncer = EventDebouncer(0.5, _callback)
    assert debouncer.debounce_interval == 0.5
    assert debouncer.events_callback is _callback


def test_debounce_interval_keyword():
    debouncer = EventDebouncer(debounce_interval=0.5, events_callback=_callback)
    assert debouncer.debounce_interval == 0.5


def test_deprecated_debounce_interval_seconds_argument():
    with pytest.warns(DeprecationWarning, match="'debounce_interval_seconds' is deprecated") as record:
        debouncer = EventDebouncer(debounce_interval_seconds=0.5, events_callback=_callback)
    assert debouncer.debounce_interval == 0.5
    assert len(record) == 1
    # The warning points at the caller, not at watchdog.
    assert record[0].filename == __file__


def test_debounce_interval_and_deprecated_name_conflict():
    with pytest.raises(TypeError, match="Cannot pass both"):
        EventDebouncer(0.5, events_callback=_callback, debounce_interval_seconds=0.5)


def test_missing_arguments():
    with pytest.raises(TypeError, match="'debounce_interval'"):
        EventDebouncer(events_callback=_callback)
    with pytest.raises(TypeError, match="'events_callback'"):
        EventDebouncer(0.5)


def test_deprecated_debounce_interval_seconds_attribute():
    debouncer = EventDebouncer(0.5, _callback)

    with pytest.warns(DeprecationWarning, match="'debounce_interval_seconds' is deprecated") as record:
        assert debouncer.debounce_interval_seconds == 0.5
    assert record[0].filename == __file__

    with pytest.warns(DeprecationWarning, match="'debounce_interval_seconds' is deprecated"):
        debouncer.debounce_interval_seconds = 2
    assert debouncer.debounce_interval == 2


def test_no_warning_with_new_names():
    with warnings.catch_warnings():
        warnings.simplefilter("error")
        debouncer = EventDebouncer(0.5, _callback)
        assert debouncer.debounce_interval == 0.5


class ObservedCondition(threading.Condition):
    """Expose wait boundaries without sleeping to guess the worker's state."""

    def __init__(self):
        super().__init__()
        self.waits = Queue()

    def wait(self, timeout=None):
        self.waits.put(timeout)
        return super().wait(timeout)

    def expect_wait(self, timeout):
        actual = self.waits.get(timeout=5)
        assert actual == timeout
        # wait() publishes before releasing its lock. Acquiring it here ensures
        # the worker has actually entered the wait before the test continues.
        with self:
            pass

    def wait_published(self):
        """Wait until the worker enters any condition wait, without asserting
        its timeout (the real condition clock is not controllable here)."""
        self.waits.get(timeout=5)
        with self:
            pass


def test_event_debouncer_coalesce_and_drain(monkeypatch):
    called = []
    delivered = threading.Event()
    e1 = FileModifiedEvent("/some/path1")
    e2 = FileModifiedEvent("/some/path2")
    e3 = FileModifiedEvent("/some/path3")

    def callback(events):
        called.append(events)
        delivered.set()

    debouncer = EventDebouncer(0.01, callback)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
        with cond:
            assert not called
            # Both events arrive before the worker can inspect the batch.
            debouncer.handle_event(e1)
            debouncer.handle_event(e2)
        assert delivered.wait(5)
        # After delivery the worker returns to the unbounded wait. Under heavy
        # load the 0.01s timeout may already have elapsed, so drain any timed
        # wait records until the unbounded one is published.
        while cond.waits.get(timeout=5) is not None:
            pass
        assert called == [[e1, e2]]

        with cond:
            # Stop before the worker can process this event normally. It must
            # drain the pending event, without delivering the old batch twice.
            debouncer.handle_event(e3)
            debouncer.stop()
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == [[e1, e2], [e3]]


def test_event_debouncer_starvation(monkeypatch):
    called = []
    now = 0.0
    monkeypatch.setattr(event_debouncer, "time", SimpleNamespace(monotonic=lambda: now))
    debouncer = EventDebouncer(60, called.append)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    batches = [[FileModifiedEvent(f"/batch{batch}/path{i}") for i in range(4)] for batch in range(2)]
    debouncer.start()
    try:
        cond.expect_wait(None)
        for batch, events in enumerate(batches):
            with cond:
                now = batch * 100.0
                debouncer.handle_event(events[0])
            cond.expect_wait(60)
            for event, elapsed in zip(events[1:], [20.0, 40.0, 61.0], strict=True):
                with cond:
                    now = batch * 100.0 + elapsed
                    debouncer.handle_event(event)
                if elapsed < 60:
                    cond.expect_wait(60)
                    with cond:
                        assert called == batches[:batch]
            # Notifications did not let the worker time out. The elapsed-time
            # limit must still flush the batch, preventing starvation. Repeat
            # for another batch to verify the elapsed timer starts afresh.
            cond.expect_wait(None)
            assert called == batches[: batch + 1]
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()


def test_event_debouncer_stop_while_idle(monkeypatch):
    called = []
    debouncer = EventDebouncer(60, called.append)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == []


def test_event_debouncer_stop_during_debounce(monkeypatch):
    called = []
    event = FileModifiedEvent("/some/path")
    debouncer = EventDebouncer(60, called.append)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
        debouncer.handle_event(event)
        cond.expect_wait(60)
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == [[event]]


def test_event_debouncer_zero_interval(monkeypatch):
    called = []
    event = FileModifiedEvent("/some/path")
    debouncer = EventDebouncer(0, called.append)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
        debouncer.handle_event(event)
        # A zero interval must go straight back to waiting for another event,
        # without a timed debounce wait or an empty callback.
        cond.expect_wait(None)
        assert called == [[event]]
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()


def test_max_batch_age_defaults_to_debounce_interval():
    debouncer = EventDebouncer(0.5, _callback)
    assert debouncer.max_batch_age == 0.5
    debouncer = EventDebouncer(0.5, _callback, max_batch_age=0.2)
    assert debouncer.max_batch_age == 0.2


def test_invalid_intervals():
    with pytest.raises(ValueError, match="debounce_interval must be non-negative"):
        EventDebouncer(-1, _callback)
    with pytest.raises(ValueError, match="max_batch_age must be non-negative"):
        EventDebouncer(0.5, _callback, max_batch_age=-1)


def test_event_debouncer_max_batch_age_caps_continuous_input(monkeypatch):
    """A stream of events with never a quiet interval is still delivered once
    the batch reaches its maximum age, and later events start a new batch."""
    now = 0.0
    monkeypatch.setattr(event_debouncer, "time", SimpleNamespace(monotonic=lambda: now))
    # Quiet interval of 60, but a batch may never grow older than 30.
    called = []
    debouncer = EventDebouncer(60, called.append, max_batch_age=30)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    first = [FileModifiedEvent(f"/first/path{i}") for i in range(3)]
    second = [FileModifiedEvent(f"/second/path{i}") for i in range(2)]
    debouncer.start()
    try:
        cond.expect_wait(None)
        with cond:
            now = 0.0
            debouncer.handle_event(first[0])
        # The requested timeout is the remaining time until the age deadline.
        cond.expect_wait(30)
        # Events keep arriving well inside the quiet interval, which by itself
        # would postpone the flush forever.
        with cond:
            now = 10.0
            debouncer.handle_event(first[1])
        cond.expect_wait(20)
        with cond:
            now = 20.0
            debouncer.handle_event(first[2])
        cond.expect_wait(10)
        # The fake clock cannot fire real condition timeouts, so a plain
        # notification wakes the worker once the batch-age deadline is reached.
        # It notices the deadline and flushes the whole first batch as one.
        now = 30.0
        with cond:
            cond.notify()
        cond.wait_published()
        assert called == [first]

        # Events arriving afterwards start a fresh batch with its own deadline.
        with cond:
            now = 31.0
            debouncer.handle_event(second[0])
        cond.expect_wait(30)
        with cond:
            now = 50.0
            debouncer.handle_event(second[1])
        cond.expect_wait(11)
        now = 61.0
        with cond:
            cond.notify()
        # No more events: stop drains the second batch as the trailing one.
        debouncer.stop()
        debouncer.join(5)
        assert not debouncer.is_alive()
        assert called == [first, second]
    finally:
        debouncer.stop()
        debouncer.join(5)


def test_event_debouncer_zero_max_batch_age_flushes_immediately(monkeypatch):
    """A zero batch age means every delivery is due immediately, without
    busy-looping or losing events."""
    now = 0.0
    monkeypatch.setattr(event_debouncer, "time", SimpleNamespace(monotonic=lambda: now))
    called = []
    debouncer = EventDebouncer(60, called.append, max_batch_age=0)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    first = FileModifiedEvent("/first")
    second = FileModifiedEvent("/second")
    debouncer.start()
    try:
        cond.expect_wait(None)
        with cond:
            now = 1.0
            debouncer.handle_event(first)
        # The age deadline is already due, so the worker parks on an unbounded
        # wait only long enough to be notified by the next event or stop.
        cond.wait_published()
        with cond:
            now = 2.0
            debouncer.handle_event(second)
            debouncer.stop()
        debouncer.join(5)
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == [[first], [second]]


def test_event_debouncer_continuous_input_delivery_is_bounded():
    """End-to-end check with a real clock: continuously fed events are still
    delivered periodically, capped by max_batch_age rather than starved."""
    called = []
    stop_producing = threading.Event()

    def callback(events):
        called.append(events)

    debouncer = EventDebouncer(30.0, callback, max_batch_age=0.1)
    debouncer.start()
    counter = iter(range(1000))

    def produce():
        while not stop_producing.wait(0.01):
            debouncer.handle_event(FileModifiedEvent(f"/path{next(counter)}"))

    producer = threading.Thread(target=produce)
    producer.start()
    try:
        deadline = time.monotonic() + 2.0
        while time.monotonic() < deadline and len(called) < 3:
            time.sleep(0.02)
    finally:
        stop_producing.set()
        producer.join(5)
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    # The endless stream was split into several capped, non-empty batches.
    assert len(called) >= 3
    assert all(batch for batch in called)


def test_event_debouncer_events_during_callback_go_to_next_batch(monkeypatch):
    """Events enqueued while a callback is running (including from inside that
    callback) must not be swept into the batch being delivered."""
    in_callback = threading.Event()
    release_callback = threading.Event()
    second_batch_done = threading.Event()
    called = []
    early = FileModifiedEvent("/early")
    from_callback = FileModifiedEvent("/from-callback")
    while_busy = FileModifiedEvent("/while-busy")

    def callback(events):
        called.append(events)
        if len(called) == 1:
            # Re-enqueue an event from inside the callback and let another
            # thread add one too, both before this batch has finished.
            debouncer.handle_event(from_callback)
            in_callback.set()
            release_callback.wait(5)
        else:
            second_batch_done.set()

    debouncer = EventDebouncer(0.01, callback)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
        with cond:
            debouncer.handle_event(early)
        assert in_callback.wait(5)
        assert called == [[early]]
        # The callback is still running; this event must survive for later.
        with cond:
            debouncer.handle_event(while_busy)
        release_callback.set()
        # Both late events are delivered together with the next batch.
        assert second_batch_done.wait(5)
        assert called == [[early], [from_callback, while_busy]]
    finally:
        release_callback.set()
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()


def test_event_debouncer_stop_flushes_trailing_batch_by_default(monkeypatch):
    called = []
    event = FileModifiedEvent("/some/path")
    debouncer = EventDebouncer(60, called.append)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
        with cond:
            debouncer.handle_event(event)
        cond.expect_wait(60)
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == [[event]]


def test_event_debouncer_stop_can_discard_trailing_batch(monkeypatch):
    called = []
    event = FileModifiedEvent("/some/path")
    debouncer = EventDebouncer(60, called.append)
    cond = ObservedCondition()
    monkeypatch.setattr(debouncer, "_cond", cond)
    debouncer.start()
    try:
        cond.expect_wait(None)
        with cond:
            debouncer.handle_event(event)
        cond.expect_wait(60)
    finally:
        debouncer.stop(flush=False)
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == []


def test_event_debouncer_stop_flush_false_after_delivery_keeps_running_events():
    """An event arriving after the last delivery but before a non-flushing stop
    must be discarded rather than delivered at shutdown."""
    delivered = threading.Event()
    called = []
    first = FileModifiedEvent("/first")
    trailing = FileModifiedEvent("/trailing")

    def callback(events):
        called.append(events)
        delivered.set()

    debouncer = EventDebouncer(0.01, callback)
    debouncer.start()
    try:
        debouncer.handle_event(first)
        assert delivered.wait(5)
        # Ensure the worker has finished the callback and parked before adding
        # the trailing event, so it unambiguously belongs to a new batch.
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and debouncer._events:
            time.sleep(0.005)
        time.sleep(0.05)
        debouncer.handle_event(trailing)
        debouncer.stop(flush=False)
        debouncer.join(5)
    finally:
        debouncer.stop()
        debouncer.join(5)
    assert not debouncer.is_alive()
    assert called == [[first]]
