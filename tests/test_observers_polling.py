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
from watchdog.observers.polling import PollingObserverVFS
from watchdog.utils import platform
from watchdog.utils.dirsnapshot import EmptyDirectorySnapshot, PartialDirectorySnapshot

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


def _drain(event_queue):
    got = []
    while True:
        try:
            event, _ = event_queue.get_nowait()
        except Empty:
            return got
        got.append(event)


class _CollectingHandler(FileSystemEventHandler):
    """Puts every dispatched event on a queue, like an emitter's queue."""

    def __init__(self, event_queue):
        self.event_queue = event_queue

    def dispatch(self, event):
        self.event_queue.put((event, None))
        super().dispatch(event)


def _collect_events_until(event_queue, expected, timeout=10):
    """Collect events until every event in ``expected`` has been seen.

    Unlike :func:`_wait_for_event`, the events collected on the way are kept,
    so a single pass can assert about several of them at once.
    """
    got = set()
    deadline = monotonic() + timeout
    while not expected <= got:
        remaining = deadline - monotonic()
        if remaining <= 0:
            pytest.fail(f"Missing events {expected - got} (got {got})")
        got.add(event_queue.get(timeout=remaining)[0])
    return got


def _partial_emitter(p, event_queue, blocked, outage, *, error=errno.EACCES):
    """Emitter using partial snapshots, with ``blocked`` unreadable during ``outage``."""

    def flaky_listdir(path):
        if outage.is_set() and path == blocked:
            raise OSError(error, os.strerror(error), path)
        return os.scandir(path)

    watch = ObservedWatch(p(), recursive=True)
    return Emitter(event_queue, watch, timeout=0.2, listdir=flaky_listdir, partial_snapshots=True)


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_partial_snapshot_keeps_unreadable_subtree_and_reports_other_changes(p, event_queue, caplog):
    # One subdirectory is unreadable during a poll: the entries of the other
    # branch are still reported as created/deleted, and nothing is reported as
    # deleted for the unreadable branch.
    mkdir(p("sub"))
    mkdir(p("other"))
    touch(p("sub", "kept"))
    touch(p("other", "kept"))

    outage = threading.Event()
    emitter = _partial_emitter(p, event_queue, p("sub"), outage)
    emitter.start()
    try:
        outage.set()
        # Changes while "sub" is unreadable: one in each branch.
        touch(p("sub", "hidden"))
        touch(p("other", "created"))
        rm(p("other", "kept"))

        got = _collect_events_until(
            event_queue,
            {FileCreatedEvent(p("other", "created")), FileDeletedEvent(p("other", "kept"))},
        )

        assert FileDeletedEvent(p("sub", "kept")) not in got
        assert DirDeletedEvent(p("sub")) not in got
        assert emitter.should_keep_running()
        # The unreadable subtree did not make the whole poll fail.
        assert not emitter._snapshot_failing  # noqa: SLF001
        assert isinstance(emitter._snapshot, PartialDirectorySnapshot)  # noqa: SLF001
        assert p("sub", "kept") in emitter._snapshot.paths  # noqa: SLF001
    finally:
        emitter.stop()
        emitter.join(5)

    warnings = [r for r in caplog.records if r.name == "watchdog.observers.polling" and r.levelno == logging.WARNING]
    assert any("keeping its last known state" in r.getMessage() for r in warnings)


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_partial_snapshot_reports_both_branches_after_recovery(p, event_queue):
    # After the outage, every branch converges on the exact listing: the
    # changes made while unreadable are reported, and no deletion is invented.
    mkdir(p("sub"))
    mkdir(p("other"))
    touch(p("sub", "removed_during_outage"))
    touch(p("other", "removed_during_outage"))

    outage = threading.Event()
    emitter = _partial_emitter(p, event_queue, p("sub"), outage)
    emitter.start()
    try:
        outage.set()
        rm(p("sub", "removed_during_outage"))
        touch(p("sub", "created_during_outage"))
        rm(p("other", "removed_during_outage"))
        touch(p("other", "created_during_outage"))

        _collect_events_until(event_queue, {FileCreatedEvent(p("other", "created_during_outage"))})
        # While unreadable, the deletion inside "sub" is not reported.
        assert FileDeletedEvent(p("sub", "removed_during_outage")) not in _drain(event_queue)

        outage.clear()
        got = _collect_events_until(
            event_queue,
            {
                FileCreatedEvent(p("sub", "created_during_outage")),
                FileDeletedEvent(p("sub", "removed_during_outage")),
            },
        )

        assert DirDeletedEvent(p("sub")) not in got
        assert DirDeletedEvent(p()) not in got
        assert emitter.should_keep_running()
        # Both branches now match the filesystem exactly.
        snapshot = emitter._snapshot  # noqa: SLF001
        assert snapshot.unreadable == {}
        assert snapshot.paths == {path for path in snapshot.paths if os.path.exists(path)}
        assert snapshot.paths >= {p("sub"), p("sub", "created_during_outage"), p("other", "created_during_outage")}
    finally:
        emitter.stop()
        emitter.join(5)


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_partial_snapshot_still_reports_deleted_subtree(p, event_queue):
    # A subdirectory that really goes away is not an unreadable one: its
    # entries must be reported as deleted.
    mkdir(p("sub"))
    touch(p("sub", "a"))

    emitter = _partial_emitter(p, event_queue, p("nowhere"), threading.Event())
    emitter.start()
    try:
        rm(p("sub"), recursive=True)
        got = _wait_for_event(event_queue, DirDeletedEvent(p("sub")), timeout=10)
        assert FileDeletedEvent(p("sub", "a")) in got
        assert DirDeletedEvent(p()) not in got
        assert emitter.should_keep_running()
        assert emitter._snapshot.unreadable == {}  # noqa: SLF001
    finally:
        emitter.stop()
        emitter.join(5)


def test_polling_observer_vfs_uses_partial_snapshots(p, event_queue):
    # The VFS observer opts in to partial snapshots for every emitter it creates.
    observer = PollingObserverVFS(stat=os.stat, listdir=os.scandir, polling_interval=0.1)
    observer.schedule(_CollectingHandler(event_queue), p(), recursive=True)
    observer.start()
    try:
        (emitter,) = observer.emitters
        assert emitter.partial_snapshots is True
    finally:
        observer.stop()
        observer.join(5)


@pytest.mark.flaky(max_runs=3, min_passes=1)
def test_polling_observer_vfs_survives_unreadable_subdirectory(p, event_queue):
    # End-to-end through the observer: a failing listdir for a single
    # subdirectory must not hide the other branch, and must not look like a
    # deletion of the unreadable one.
    mkdir(p("sub"))
    mkdir(p("other"))
    touch(p("sub", "kept"))
    outage = threading.Event()

    def flaky_listdir(path):
        if outage.is_set() and path == p("sub"):
            raise OSError(errno.EACCES, os.strerror(errno.EACCES), path)
        return os.scandir(path)

    observer = PollingObserverVFS(stat=os.stat, listdir=flaky_listdir, polling_interval=0.1)
    watch = observer.schedule(_CollectingHandler(event_queue), p(), recursive=True)
    observer.start()
    try:
        outage.set()
        touch(p("other", "created"))
        got = _collect_events_until(event_queue, {FileCreatedEvent(p("other", "created"))})
        # The unreadable subtree did not take the watch down, nor look deleted.
        assert watch in observer._watches  # noqa: SLF001
        assert not any(isinstance(event, DirDeletedEvent) and event.src_path == p("sub") for event in got)
    finally:
        observer.stop()
        observer.join(5)


def test_transient_error_in_subdirectory_with_plain_snapshots(p, event_queue):
    # Without partial snapshots (the historical behavior), a single failing
    # listdir aborts the whole poll. This documents the difference.
    mkdir(p("sub"))
    outage = threading.Event()
    failures = []

    def flaky_listdir(path):
        if outage.is_set() and path == p("sub"):
            failures.append(path)
            raise OSError(errno.EIO, os.strerror(errno.EIO), path)
        return os.scandir(path)

    emitter = Emitter(event_queue, ObservedWatch(p(), recursive=True), timeout=0.2, listdir=flaky_listdir)
    emitter.start()
    try:
        outage.set()
        touch(p("other"))
        _wait_for_failed_polls(emitter, failures)
        assert emitter._snapshot_failing  # noqa: SLF001
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
