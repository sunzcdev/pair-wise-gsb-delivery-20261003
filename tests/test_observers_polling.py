from __future__ import annotations

import errno
import logging
import os
import stat
import threading
from queue import Empty, Queue
from time import monotonic, sleep

import pytest

from watchdog.events import (
    DirAttribEvent,
    DirCreatedEvent,
    DirDeletedEvent,
    DirModifiedEvent,
    DirMovedEvent,
    FileAttribEvent,
    FileCreatedEvent,
    FileDeletedEvent,
    FileModifiedEvent,
    FileMovedEvent,
    FileSystemEventHandler,
)
from watchdog.observers.api import ObservedWatch
from watchdog.observers.polling import PollingEmitter as Emitter
from watchdog.observers.polling import PollingObserver, SharedPollingEmitter
from watchdog.utils import platform
from watchdog.utils.dirsnapshot import EmptyDirectorySnapshot

from .shell import chmod, mkdir, msize, mv, rm, touch

SLEEP_TIME = 0.5


@pytest.fixture
def event_queue():
    return Queue()


@pytest.fixture
def emitter(event_queue, p):
    watch = ObservedWatch(str(p()), recursive=True)
    em = Emitter(event_queue, watch, timeout=0.2)
    em.start()
    yield em
    em.stop()
    em.join(5)


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test___init__(p, event_queue, emitter):
    sleep(SLEEP_TIME)
    mkdir(p("project"))

    sleep(SLEEP_TIME)
    mkdir(p("project", "blah"))

    sleep(SLEEP_TIME)
    touch(p("afile"))

    sleep(SLEEP_TIME)
    touch(p("fromfile"))

    sleep(SLEEP_TIME)
    mv(p("fromfile"), p("project", "tofile"))

    sleep(SLEEP_TIME)
    touch(p("afile"))

    sleep(SLEEP_TIME)
    mv(p("project", "blah"), p("project", "boo"))

    sleep(SLEEP_TIME)
    rm(p("project"), recursive=True)

    sleep(SLEEP_TIME)
    rm(p("afile"))

    sleep(SLEEP_TIME)
    msize(p("bfile"))

    sleep(SLEEP_TIME)
    rm(p("bfile"))

    sleep(SLEEP_TIME)
    emitter.stop()

    # What we need here for the tests to pass is a collection type
    # that is:
    #   * unordered
    #   * non-unique
    # A multiset! Python's collections.Counter class seems appropriate.
    expected = {
        DirModifiedEvent(p()),
        DirCreatedEvent(p("project")),
        DirModifiedEvent(p("project")),
        DirCreatedEvent(p("project", "blah")),
        FileCreatedEvent(p("afile")),
        DirModifiedEvent(p()),
        FileCreatedEvent(p("fromfile")),
        DirModifiedEvent(p()),
        DirModifiedEvent(p()),
        FileModifiedEvent(p("afile")),
        DirModifiedEvent(p("project")),
        DirModifiedEvent(p()),
        FileDeletedEvent(p("project", "tofile")),
        DirDeletedEvent(p("project", "boo")),
        DirDeletedEvent(p("project")),
        DirModifiedEvent(p()),
        FileDeletedEvent(p("afile")),
        DirModifiedEvent(p()),
        FileCreatedEvent(p("bfile")),
        FileModifiedEvent(p("bfile")),
        DirModifiedEvent(p()),
        FileDeletedEvent(p("bfile")),
    }

    expected.add(FileMovedEvent(p("fromfile"), p("project", "tofile")))
    expected.add(DirMovedEvent(p("project", "blah"), p("project", "boo")))

    got = set()

    while True:
        try:
            event, _ = event_queue.get_nowait()
            got.add(event)
        except Empty:
            break

    assert expected == got


def _wait_for_failed_polls(emitter, failures, count=2, timeout=5):
    deadline = monotonic() + timeout
    while len(failures) < count and emitter.should_keep_running() and monotonic() < deadline:
        sleep(0.05)


def _wait_for_event(event_queue, expected, timeout=5):
    got = set()
    while expected not in got:
        event, _ = event_queue.get(timeout=timeout)
        got.add(event)
    return got


@pytest.mark.flaky(max_runs=3, min_passes=1)
@pytest.mark.parametrize("error", ["EACCES", "EBUSY", "EHOSTDOWN", "ESTALE"])
def test_transient_error_on_watched_dir(p, event_queue, caplog, error):
    # A network share that is briefly unreachable must not be reported as
    # deleted, and changes made during the outage must be reported once it
    # is back (#1065).
    code = getattr(errno, error)
    watch = ObservedWatch(p(), recursive=True)
    outage = threading.Event()
    failures = []

    def flaky_stat(path):
        if outage.is_set() and path == watch.path:
            failures.append(path)
            raise OSError(code, os.strerror(code), path)
        return os.stat(path)

    emitter = Emitter(event_queue, watch, timeout=0.2, stat=flaky_stat)
    emitter.start()
    try:
        outage.set()
        touch(p("during_outage"))
        _wait_for_failed_polls(emitter, failures)
        assert emitter.should_keep_running()
        assert len(failures) >= 2
        outage.clear()

        got = _wait_for_event(event_queue, FileCreatedEvent(p("during_outage")))
        assert DirDeletedEvent(p()) not in got
        assert emitter.should_keep_running()
    finally:
        emitter.stop()
        emitter.join(5)

    warnings = [r for r in caplog.records if r.name == "watchdog.observers.polling" and r.levelno == logging.WARNING]
    assert len(warnings) == 1


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_transient_error_in_subdirectory(p, event_queue):
    mkdir(p("sub"))
    watch = ObservedWatch(p(), recursive=True)
    outage = threading.Event()
    failures = []

    def flaky_listdir(path):
        if outage.is_set() and path == p("sub"):
            failures.append(path)
            raise OSError(errno.EIO, os.strerror(errno.EIO), path)
        return os.scandir(path)

    emitter = Emitter(event_queue, watch, timeout=0.2, listdir=flaky_listdir)
    emitter.start()
    try:
        outage.set()
        touch(p("sub", "during_outage"))
        _wait_for_failed_polls(emitter, failures)
        assert emitter.should_keep_running()
        assert len(failures) >= 2
        outage.clear()

        got = _wait_for_event(event_queue, FileCreatedEvent(p("sub", "during_outage")))
        assert DirDeletedEvent(p()) not in got
        assert emitter.should_keep_running()
    finally:
        emitter.stop()
        emitter.join(5)


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_delete_watched_dir(p, event_queue, emitter):
    rm(p(), recursive=True)

    sleep(SLEEP_TIME)
    emitter.stop()

    # What we need here for the tests to pass is a collection type
    # that is:
    #   * unordered
    #   * non-unique
    # A multiset! Python's collections.Counter class seems appropriate.
    expected = {
        DirDeletedEvent(p()),
    }

    got = set()

    while True:
        try:
            event, _ = event_queue.get_nowait()
            got.add(event)
        except Empty:
            break

    assert expected == got


def test_polling_emitter_attrib(event_queue, tmp_path):
    # Use tmp_path so this test runs in an isolated directory and is not
    # impacted by test_delete_watched_dir removing the module-level TEMP_DIR.
    temp_dir = str(tmp_path)
    file_path = os.path.join(temp_dir, "attr_file.txt")
    dir_path = os.path.join(temp_dir, "attr_dir")
    touch(file_path)
    mkdir(dir_path)

    watch = ObservedWatch(temp_dir, recursive=True)
    emitter = Emitter(event_queue, watch, timeout=0.1)
    emitter.start()

    # Wait until the emitter thread finishes its initial snapshot before modifying attributes
    while isinstance(emitter._snapshot, EmptyDirectorySnapshot):  # noqa: SLF001
        sleep(0.01)

    mode = stat.S_IREAD if platform.is_windows() else 0o777
    try:
        chmod(file_path, mode)
        chmod(dir_path, mode)

        # Wait for both attribute events with a timeout to prevent flakiness on slow/busy CI runners
        got = set()
        while len(got) < 2:
            event, _ = event_queue.get(timeout=5)
            got.add(event)

        assert FileAttribEvent(file_path) in got
        assert DirAttribEvent(dir_path) in got
        assert FileModifiedEvent(file_path) not in got
        assert DirModifiedEvent(dir_path) not in got
    finally:
        if platform.is_windows():
            chmod(file_path, stat.S_IWRITE)
            chmod(dir_path, stat.S_IWRITE)
        emitter.stop()
        emitter.join(5)


@pytest.mark.parametrize(("source", "destination"), [("a", "c"), ("c", "a")])
def test_hard_link_rename_emits_only_move(p, source, destination):
    touch(p(source))
    try:
        os.link(p(source), p("b"))
    except OSError as e:
        pytest.skip(f"Hard links are unavailable: {e}")

    def listdir(path):
        with os.scandir(path) as entries:
            yield from sorted(entries, key=lambda entry: entry.name)

    queue = Queue()
    emitter = Emitter(queue, ObservedWatch(p(""), recursive=True), listdir=listdir)
    emitter.on_thread_start()
    mv(p(source), p(destination))
    emitter.queue_events(0)
    events = []
    while not queue.empty():
        event, _ = queue.get_nowait()
        if not event.is_directory:
            events.append(event)

    assert events == [FileMovedEvent(p(source), p(destination))]


@pytest.mark.parametrize(
    ("was_dir", "deleted_cls", "created_cls"),
    [(False, FileDeletedEvent, DirCreatedEvent), (True, DirDeletedEvent, FileCreatedEvent)],
)
def test_reused_inode_replacement_emits_delete_and_create(p, event_queue, was_dir, deleted_cls, created_cls):
    path = p("entry")
    (mkdir if was_dir else touch)(path)

    def reused_inode_stat(name):
        result = os.stat(name)
        if name != path:
            return result
        # Model an inode reused after deletion through the supported VFS hook.
        values = list(result)
        values[1] = 42
        return os.stat_result(values)

    emitter = Emitter(event_queue, ObservedWatch(p(""), recursive=True), stat=reused_inode_stat)
    emitter.on_thread_start()
    (os.rmdir if was_dir else os.unlink)(path)
    (touch if was_dir else mkdir)(path)
    emitter.queue_events(0)
    events = set()
    while not event_queue.empty():
        event, _ = event_queue.get_nowait()
        if event.src_path == path:
            events.add(event)

    assert events == {deleted_cls(path), created_cls(path)}


def _drain_events(event_queue):
    events = {}
    while True:
        try:
            event, watch = event_queue.get_nowait()
        except Empty:
            break
        events.setdefault(watch, set()).add(event)
    return events


@pytest.mark.skipif(not platform.is_windows(), reason="Windows-specific path spellings")
def test_shared_scan_merges_windows_path_spellings(tmp_path):
    # Two schedules that spell the same directory differently must share one
    # scan; unscheduling the first subscription must not stop the second.
    root = str(tmp_path)
    alt = root.replace(os.sep, "/")
    assert alt != root

    reads = []

    def counting_listdir(path):
        reads.append(path)
        return os.scandir(path)

    observer = PollingObserver(timeout=0.1, share_scans=True, listdir=counting_listdir)
    try:
        watch1 = observer.schedule(FileSystemEventHandler(), root, recursive=True)
        watch2 = observer.schedule(FileSystemEventHandler(), alt, recursive=True)

        # Two logical entries, one shared collection.
        assert len(observer.emitters) == 1
        (emitter,) = observer.emitters
        assert isinstance(emitter, SharedPollingEmitter)
        assert emitter.reader_count == 2

        emitter.on_thread_start()
        touch(os.path.join(root, "first"))

        reads.clear()
        emitter.queue_events(0)
        # Both watches were served by a single read of the tree.
        assert reads == [emitter.root]

        events = _drain_events(observer.event_queue)
        assert FileCreatedEvent(os.path.join(root, "first")) in events[watch1]
        # Each watch gets events spelled with its own logical root.
        assert FileCreatedEvent(os.path.join(alt, "first")) in events[watch2]

        # Unscheduling the first subscription keeps the second one polling.
        observer.unschedule(watch1)
        assert emitter.reader_count == 1
        assert emitter.should_keep_running()

        touch(os.path.join(root, "second"))
        reads.clear()
        emitter.queue_events(0)
        assert reads == [emitter.root]

        events = _drain_events(observer.event_queue)
        assert watch1 not in events
        assert FileCreatedEvent(os.path.join(alt, "second")) in events[watch2]

        # Unscheduling the last subscription stops the shared emitter.
        observer.unschedule(watch2)
        assert not observer.emitters
        assert not emitter.should_keep_running()
    finally:
        observer.unschedule_all()


def test_shared_scan_does_not_merge_same_named_directories(tmp_path):
    # Directories that merely happen to share a name are different scans.
    first = tmp_path / "a" / "same"
    second = tmp_path / "b" / "same"
    first.mkdir(parents=True)
    second.mkdir(parents=True)

    observer = PollingObserver(share_scans=True)
    try:
        observer.schedule(FileSystemEventHandler(), str(first))
        observer.schedule(FileSystemEventHandler(), str(second))
        assert len(observer.emitters) == 2
    finally:
        observer.unschedule_all()


def test_shared_scan_distributes_per_scope_and_filter(tmp_path):
    root = str(tmp_path)
    sub = os.path.join(root, "sub")
    mkdir(sub)

    observer = PollingObserver(share_scans=True)
    try:
        deep = observer.schedule(FileSystemEventHandler(), root, recursive=True)
        flat = observer.schedule(FileSystemEventHandler(), root, recursive=False)
        created_only = observer.schedule(
            FileSystemEventHandler(), root, recursive=True, event_filter=[FileCreatedEvent]
        )

        assert len(observer.emitters) == 1
        (emitter,) = observer.emitters
        assert emitter.reader_count == 3

        emitter.on_thread_start()
        touch(os.path.join(sub, "nested"))
        touch(os.path.join(root, "top"))
        emitter.queue_events(0)
        events = _drain_events(observer.event_queue)

        # Recursive scope sees the nested file; the flat watch does not.
        assert FileCreatedEvent(os.path.join(sub, "nested")) in events[deep]
        assert FileCreatedEvent(os.path.join(sub, "nested")) not in events[flat]
        assert FileCreatedEvent(os.path.join(root, "top")) in events[flat]
        # The filtered watch only receives the event types it asked for.
        assert events[created_only] == {
            FileCreatedEvent(os.path.join(sub, "nested")),
            FileCreatedEvent(os.path.join(root, "top")),
        }
    finally:
        observer.unschedule_all()


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_shared_scan_threaded(p):
    class Sink(FileSystemEventHandler):
        def __init__(self):
            self.events = []

        def on_any_event(self, event):
            self.events.append(event)

    def received(sink, event, timeout=5):
        deadline = monotonic() + timeout
        while monotonic() < deadline:
            if event in sink.events:
                return True
            sleep(0.05)
        return False

    sink1, sink2 = Sink(), Sink()
    observer = PollingObserver(timeout=0.1, share_scans=True)
    watch1 = observer.schedule(sink1, p(), recursive=True)
    # A trailing separator spells the same directory differently.
    observer.schedule(sink2, p(""), recursive=True)
    assert len(observer.emitters) == 1

    observer.start()
    try:
        touch(p("one"))
        assert received(sink1, FileCreatedEvent(p("one")))
        assert received(sink2, FileCreatedEvent(p("one")))

        observer.unschedule(watch1)
        assert len(observer.emitters) == 1

        touch(p("two"))
        assert received(sink2, FileCreatedEvent(p("two")))
    finally:
        observer.stop()
        observer.join(5)
