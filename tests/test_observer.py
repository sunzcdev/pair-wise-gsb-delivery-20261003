from __future__ import annotations

import contextlib
import os
import threading
import time
from typing import TYPE_CHECKING
from unittest.mock import Mock, patch

import pytest

from watchdog.events import FileModifiedEvent, FileReplacedEvent, FileSystemEventHandler
from watchdog.observers import Observer
from watchdog.observers.api import BaseObserver, EventEmitter

if TYPE_CHECKING:
    from collections.abc import Iterator


@pytest.fixture
def observer() -> Iterator[BaseObserver]:
    obs = BaseObserver(EventEmitter)
    yield obs
    obs.stop()
    with contextlib.suppress(RuntimeError):
        obs.join()


@pytest.fixture
def observer2():
    obs = BaseObserver(EventEmitter)
    yield obs
    obs.stop()
    with contextlib.suppress(RuntimeError):
        obs.join()


def test_schedule_should_start_emitter_if_running(observer):
    observer.start()
    observer.schedule(None, "")
    (emitter,) = observer.emitters
    assert emitter.is_alive()


def test_schedule_should_not_start_emitter_if_not_running(observer):
    observer.schedule(None, "")
    (emitter,) = observer.emitters
    assert not emitter.is_alive()


def test_start_should_start_emitter(observer):
    observer.schedule(None, "")
    observer.start()
    (emitter,) = observer.emitters
    assert emitter.is_alive()


def test_stop_should_stop_emitter(observer):
    observer.schedule(None, "")
    observer.start()
    (emitter,) = observer.emitters
    assert emitter.is_alive()
    observer.stop()
    observer.join()
    assert not observer.is_alive()
    assert not emitter.is_alive()


def test_unschedule_self(observer):
    """
    Tests that unscheduling a watch from within an event handler correctly
    correctly unregisters emitter and handler without deadlocking.
    """

    class EventHandler(FileSystemEventHandler):
        def on_modified(self, event):
            observer.unschedule(watch)
            unschedule_finished.set()

    unschedule_finished = threading.Event()
    watch = observer.schedule(EventHandler(), "")
    observer.start()

    (emitter,) = observer.emitters
    emitter.queue_event(FileModifiedEvent(""))

    assert unschedule_finished.wait()
    assert len(observer.emitters) == 0


def test_schedule_after_unschedule_all(observer):
    observer.start()
    observer.schedule(None, "")
    assert len(observer.emitters) == 1

    observer.unschedule_all()
    assert len(observer.emitters) == 0

    observer.schedule(None, "")
    assert len(observer.emitters) == 1


def test_2_observers_on_the_same_path(observer, observer2):
    assert observer is not observer2

    observer.schedule(None, "")
    assert len(observer.emitters) == 1

    observer2.schedule(None, "")
    assert len(observer2.emitters) == 1


def test_start_failure_should_not_prevent_further_try(observer):
    observer.schedule(None, "")
    emitters = observer.emitters
    assert len(emitters) == 1

    # Make the emitter to fail on start()

    def mocked_start():
        raise OSError("Mock'ed!")

    emitter = next(iter(emitters))
    with patch.object(emitter, "start", new=mocked_start), pytest.raises(OSError, match="Mock'ed!"):
        observer.start()
    # The emitter should be removed from the list
    assert len(observer.emitters) == 0

    # Restoring the original behavior should work like there never be emitters
    observer.start()
    assert len(observer.emitters) == 0

    # Re-scheduling the watch should work
    observer.schedule(None, "")
    assert len(observer.emitters) == 1


def test_schedule_failure_should_not_prevent_future_schedules(observer):
    observer.start()

    # Make the emitter fail on start(), and subsequently the observer to fail on schedule()
    def bad_start(_):
        raise OSError("Mock'ed!")

    with patch.object(EventEmitter, "start", new=bad_start), pytest.raises(OSError, match="Mock'ed!"):
        observer.schedule(None, "")
    # The emitter should not be in the list
    assert not observer.emitters

    # Re-scheduling the watch should work
    observer.schedule(None, "")
    assert len(observer.emitters) == 1


@pytest.mark.parametrize(("running", "emitter_method"), [(False, "__init__"), (True, "__init__"), (True, "start")])
def test_schedule_failure_does_not_keep_handler(observer, running, emitter_method):
    rejected_handler = Mock(spec=FileSystemEventHandler)
    accepted_handler = Mock(spec=FileSystemEventHandler)
    dispatched = threading.Event()
    accepted_handler.dispatch.side_effect = lambda event: dispatched.set()

    if running:
        observer.start()

    with (
        patch.object(EventEmitter, emitter_method, side_effect=OSError("Cannot watch path")),
        pytest.raises(OSError, match="Cannot watch path"),
    ):
        observer.schedule(rejected_handler, "/watched")

    observer.schedule(accepted_handler, "/watched")
    if not running:
        observer.start()

    (emitter,) = observer.emitters
    event = FileModifiedEvent("/watched/file")
    emitter.queue_event(event)
    assert dispatched.wait(timeout=5)
    observer.stop()
    observer.join()

    accepted_handler.dispatch.assert_called_once_with(event)
    rejected_handler.dispatch.assert_not_called()


def test_single_file_track_by_path_atomic_save(tmp_path):
    """A path-tracking file watch survives atomic-save replacements.

    Editors save by writing a temporary file in the same directory and renaming
    it over the original. A single-file watch scheduled with track_by_path=True
    keeps watching the target path across two such replacements, reporting each
    takeover as a FileReplacedEvent and a later in-place edit as a plain
    FileModifiedEvent, while events for a sibling file never reach the handler.
    """
    target = tmp_path / "config.ini"
    target.write_text("one")
    other = tmp_path / "other.ini"
    other.write_text("other")

    received = []
    received_lock = threading.Lock()

    class Handler(FileSystemEventHandler):
        def on_any_event(self, event):
            with received_lock:
                received.append(event)

    observer = Observer()
    observer.schedule(Handler(), str(target), track_by_path=True)
    observer.start()
    try:
        # Wait until the emitter thread runs before making changes.
        deadline = time.monotonic() + 5
        while not any(emitter.is_alive() for emitter in observer.emitters) and time.monotonic() < deadline:
            time.sleep(0.05)
        time.sleep(0.5)

        def atomic_save(contents):
            temp = tmp_path / "config.ini.tmp"
            temp.write_text(contents)
            os.replace(temp, target)

        def replaced_count(events):
            return sum(isinstance(e, FileReplacedEvent) for e in events)

        def wait_for(predicate, timeout=10):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                with received_lock:
                    if predicate(list(received)):
                        return
                time.sleep(0.05)
            msg = "timed out waiting for expected events"
            raise AssertionError(msg)

        atomic_save("two")
        wait_for(lambda events: replaced_count(events) >= 1)
        other.write_text("other-two")  # sibling noise: must not leak in

        atomic_save("three")
        wait_for(lambda events: replaced_count(events) >= 2)
        other.write_text("other-three")

        target.write_text("four")  # in-place modification
        wait_for(
            lambda events: any(
                isinstance(e, FileModifiedEvent) and not isinstance(e, FileReplacedEvent) for e in events
            )
        )

        time.sleep(1)  # grace period for straggler events
    finally:
        observer.stop()
        observer.join()

    with received_lock:
        events = list(received)

    target_path = os.path.abspath(str(target))
    # Nothing about the sibling file or the temporary file may leak in.
    for event in events:
        assert os.path.abspath(event.src_path) == target_path
        assert not event.dest_path

    # Each replacement was reported exactly once, as a replacement.
    replaced = [i for i, e in enumerate(events) if isinstance(e, FileReplacedEvent)]
    assert len(replaced) == 2

    # The in-place edit is a plain modification, reported after the replacements.
    first_modified = next(
        i for i, e in enumerate(events) if isinstance(e, FileModifiedEvent) and not isinstance(e, FileReplacedEvent)
    )
    assert first_modified > replaced[-1]


def test_context_manager_starts_observer():
    """Test that context manager starts observer if not already started."""
    obs = BaseObserver(EventEmitter)
    obs.schedule(None, "")
    (emitter,) = obs.emitters
    assert not obs.is_alive()
    assert not emitter.is_alive()

    with obs:
        assert obs.is_alive()
        assert emitter.is_alive()

    assert not obs.is_alive()
    assert not emitter.is_alive()


def test_context_manager_with_already_started_observer():
    """Test that context manager doesn't try to start observer if already started."""
    obs = BaseObserver(EventEmitter)
    obs.schedule(None, "")
    obs.start()
    (emitter,) = obs.emitters
    assert obs.is_alive()
    assert emitter.is_alive()

    # Using context manager with already started observer should not raise
    with obs:
        assert obs.is_alive()
        assert emitter.is_alive()

    assert not obs.is_alive()
    assert not emitter.is_alive()


def test_context_manager_stops_on_exit():
    """Test that context manager stops and joins observer on exit."""
    obs = BaseObserver(EventEmitter)
    obs.schedule(None, "")
    (emitter,) = obs.emitters

    with obs:
        assert obs.is_alive()
        assert emitter.is_alive()
        # Exit context

    # After exiting context, observer should be stopped
    assert not obs.is_alive()
    assert not emitter.is_alive()


def test_context_manager_handles_exceptions():
    """Test that context manager properly cleans up even when exceptions occur."""
    obs = BaseObserver(EventEmitter)
    obs.schedule(None, "")
    (emitter,) = obs.emitters

    def _raise_in_context() -> None:
        with obs:
            assert obs.is_alive()
            assert emitter.is_alive()
            raise ValueError

    with pytest.raises(ValueError, match=r".*"):
        _raise_in_context()

    # Observer should still be stopped even after exception
    assert not obs.is_alive()
    assert not emitter.is_alive()


def test_context_manager_with_scheduled_watches():
    """Test that context manager works correctly with scheduled watches."""
    obs = BaseObserver(EventEmitter)
    handler = FileSystemEventHandler()
    obs.schedule(handler, "/foobar", recursive=True)
    assert len(obs.emitters) == 1
    (emitter,) = obs.emitters
    assert not obs.is_alive()
    assert not emitter.is_alive()

    with obs:
        assert obs.is_alive()
        assert emitter.is_alive()
        assert len(obs.emitters) == 1

    assert not obs.is_alive()
    assert not emitter.is_alive()
