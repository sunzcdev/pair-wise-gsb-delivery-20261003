""":module: watchdog.observers.polling
:synopsis: Polling emitter implementation.
:author: yesudeep@google.com (Yesudeep Mangalapilly)
:author: Mickaël Schoentgen <contact@tiger-222.fr>

Classes
-------
.. autoclass:: PollingEmitter
   :members:
   :show-inheritance:

.. autoclass:: SharedPollingEmitter
   :members:
   :show-inheritance:

.. autoclass:: PollingObserver
   :members:
   :show-inheritance:

.. autoclass:: PollingObserverVFS
   :members:
   :show-inheritance:
   :special-members:
"""

from __future__ import annotations

import contextlib
import logging
import os
import threading
from functools import partial
from typing import TYPE_CHECKING

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
)
from watchdog.observers.api import (
    DEFAULT_EMITTER_TIMEOUT,
    DEFAULT_OBSERVER_TIMEOUT,
    BaseObserver,
    EventEmitter,
    ObservedWatch,
)
from watchdog.utils.dirsnapshot import DirectorySnapshot, DirectorySnapshotDiff, EmptyDirectorySnapshot

if TYPE_CHECKING:
    from collections.abc import Callable, Iterator
    from pathlib import Path
    from typing import Any

    from watchdog.events import FileSystemEvent, FileSystemEventHandler
    from watchdog.observers.api import EventQueue
    from watchdog.utils import Seconds

logger = logging.getLogger(__name__)


def _queue_diff_events(events: DirectorySnapshotDiff, queue_event: Callable[[FileSystemEvent], None]) -> None:
    """Queue one file system event per path in a snapshot diff."""
    # Files.
    for src_path in events.files_deleted:
        queue_event(FileDeletedEvent(src_path))
    for src_path in events.files_modified:
        queue_event(FileModifiedEvent(src_path))
    for src_path in events.files_created:
        queue_event(FileCreatedEvent(src_path))
    for src_path, dest_path in events.files_moved:
        queue_event(FileMovedEvent(src_path, dest_path))
    for src_path in events.files_attrib:
        queue_event(FileAttribEvent(src_path))

    # Directories.
    for src_path in events.dirs_deleted:
        queue_event(DirDeletedEvent(src_path))
    for src_path in events.dirs_modified:
        queue_event(DirModifiedEvent(src_path))
    for src_path in events.dirs_created:
        queue_event(DirCreatedEvent(src_path))
    for src_path, dest_path in events.dirs_moved:
        queue_event(DirMovedEvent(src_path, dest_path))
    for src_path in events.dirs_attrib:
        queue_event(DirAttribEvent(src_path))


def _scan_identity(path: str) -> str:
    """Return the canonical identity of a watched directory.

    Watches whose paths resolve to the same directory -- different spellings,
    letter case, separators, ``.`` components or symlinks -- share one scan.
    Directories that merely happen to share a name never do, because their
    resolved paths differ.
    """
    return os.path.normcase(os.path.realpath(path))


def _scoped_snapshot_view(
    snapshot: DirectorySnapshot,
    root: str,
    watch_path: str,
    *,
    recursive: bool,
) -> DirectorySnapshot:
    """Derive the snapshot a dedicated emitter rooted at ``watch_path`` would
    have collected, from a shared ``snapshot`` of ``root``, without touching
    the file system.

    Paths are re-rooted from the shared scan root to the watch's own spelling
    and, for non-recursive watches, limited to the root and its direct
    children.
    """
    sep = os.sep if isinstance(root, str) else os.fsencode(os.sep)
    prefix = root if root.endswith(sep) else root + sep

    mapping = {}
    for path in snapshot.paths:
        if path == root:
            mapped = watch_path
        elif path.startswith(prefix):
            rel = path[len(prefix) :]
            if not recursive and sep in rel:
                continue
            mapped = os.path.join(watch_path, rel)
        else:
            continue
        mapping[path] = mapped

    view = object.__new__(DirectorySnapshot)
    view.recursive = recursive
    view.stat = snapshot.stat
    view.listdir = snapshot.listdir
    # The view reuses the collector's internals instead of rescanning the
    # file system; both classes live in this package on purpose.
    view._stat_info = {mapping[path]: st for path, st in snapshot._stat_info.items() if path in mapping}  # noqa: SLF001
    view._inode_to_path = {
        uid: mapping[path]
        for uid, path in snapshot._inode_to_path.items()  # noqa: SLF001
        if path in mapping
    }
    view._birthtimes = {
        mapping[path]: birthtime
        for path, birthtime in snapshot._birthtimes.items()  # noqa: SLF001
        if path in mapping
    }
    return view


class PollingEmitter(EventEmitter):
    """Platform-independent emitter that polls a directory to detect file
    system changes.
    """

    def __init__(
        self,
        event_queue: EventQueue,
        watch: ObservedWatch,
        *,
        timeout: Seconds = DEFAULT_EMITTER_TIMEOUT,
        event_filter: list[type[FileSystemEvent]] | None = None,
        stat: Callable[[str], os.stat_result] = os.stat,
        listdir: Callable[[str | None], Iterator[os.DirEntry]] = os.scandir,
    ) -> None:
        super().__init__(event_queue, watch, timeout=timeout, event_filter=event_filter)
        self._snapshot: DirectorySnapshot = EmptyDirectorySnapshot()
        self._lock = threading.Lock()
        self._snapshot_failing = False
        self._take_snapshot: Callable[[], DirectorySnapshot] = lambda: DirectorySnapshot(
            self.watch.path,
            recursive=self.watch.is_recursive,
            stat=stat,
            listdir=listdir,
        )

    def on_thread_start(self) -> None:
        """Take the first snapshot of the watched directory.

        Later snapshots are compared against this one to detect changes.
        """

        self._snapshot = self._take_snapshot()

    def queue_events(self, timeout: Seconds) -> None:
        """Wait ``timeout`` seconds, then queue events for any changes.

        Takes a new snapshot of the watched directory and compares it with the
        previous one. An event is queued for each file or directory that was
        created, deleted, modified or moved. Returns early if the emitter is
        stopped while waiting.

        If the watched directory no longer exists or is not a directory, a
        :class:`~watchdog.events.DirDeletedEvent` is queued and the emitter
        stops. Other :class:`OSError` exceptions are logged and retried on the
        next poll.

        :param timeout:
            Seconds to wait between successive snapshots. Polling emitters use
            this as the polling interval.
        """

        # We don't want to hit the disk continuously.
        # timeout behaves like an interval for polling emitters.
        if self.stopped_event.wait(timeout):
            return

        with self._lock:
            if not self.should_keep_running():
                return

            # Get event diff between fresh snapshot and previous snapshot.
            # Update snapshot.
            try:
                new_snapshot = self._take_snapshot()
            except (FileNotFoundError, NotADirectoryError):
                self.queue_event(DirDeletedEvent(self.watch.path))
                self.stop()
                return
            except OSError as exc:
                # Other errors (EIO, ESTALE, EHOSTDOWN, ...) are often temporary,
                # e.g. a network share that is briefly unreachable. Keep the last
                # snapshot and try again on the next poll, so that changes made
                # in the meantime are still reported.
                if not self._snapshot_failing:
                    self._snapshot_failing = True
                    logger.warning("Cannot take a snapshot of %r, will retry: %s", self.watch.path, exc)
                return

            if self._snapshot_failing:
                self._snapshot_failing = False
                logger.info("Snapshot of %r succeeded again, resuming", self.watch.path)

            events = DirectorySnapshotDiff(self._snapshot, new_snapshot)
            self._snapshot = new_snapshot

            _queue_diff_events(events, self.queue_event)


class SharedPollingEmitter(EventEmitter):
    """Polling emitter that scans a directory once per interval on behalf of
    several watches, then distributes the events to each of them.

    Watches that point at the same directory -- possibly through different
    path spellings -- share the snapshot collection. Each watch still receives
    events spelled with its own path, limited to its own recursive scope and
    event filter. The emitter keeps polling as long as it has at least one
    subscribed watch; unscheduling one watch never stops the others.
    """

    def __init__(
        self,
        event_queue: EventQueue,
        root: str,
        *,
        timeout: Seconds = DEFAULT_EMITTER_TIMEOUT,
        stat: Callable[[str], os.stat_result] = os.stat,
        listdir: Callable[[str | None], Iterator[os.DirEntry]] = os.scandir,
    ) -> None:
        # The shared emitter has no single watch; subscriptions come and go.
        super().__init__(event_queue, None, timeout=timeout)
        self.root = root
        self._stat = stat
        self._listdir = listdir
        self._lock = threading.Lock()
        self._snapshot_failing = False
        self._recursive = False
        self._latest: DirectorySnapshot | None = None
        # Subscribed watches, mapped to their previous scoped snapshot.
        self._subscriptions: dict[ObservedWatch, DirectorySnapshot] = {}

    @property
    def reader_count(self) -> int:
        """Number of watches currently sharing this emitter's scan."""
        return len(self._subscriptions)

    @property
    def watches(self) -> set[ObservedWatch]:
        """The watches currently subscribed to this emitter's scan."""
        return set(self._subscriptions)

    def add_watch(self, watch: ObservedWatch) -> None:
        """Subscribe ``watch`` to this emitter's shared scan."""
        with self._lock:
            if watch in self._subscriptions:
                return
            self._recursive = self._recursive or watch.is_recursive
            # A watch that joins after the first scan starts from the current
            # state of the directory, like a dedicated emitter does.
            baseline = EmptyDirectorySnapshot() if self._latest is None else self._scoped_view(watch, self._latest)
            self._subscriptions[watch] = baseline

    def remove_watch(self, watch: ObservedWatch) -> None:
        """Unsubscribe ``watch`` from this emitter's shared scan."""
        with self._lock:
            self._subscriptions.pop(watch, None)
            self._recursive = any(w.is_recursive for w in self._subscriptions)

    def _take_snapshot(self) -> DirectorySnapshot:
        return DirectorySnapshot(
            self.root,
            recursive=self._recursive,
            stat=self._stat,
            listdir=self._listdir,
        )

    def _scoped_view(self, watch: ObservedWatch, snapshot: DirectorySnapshot) -> DirectorySnapshot:
        return _scoped_snapshot_view(snapshot, self.root, watch.path, recursive=watch.is_recursive)

    def _queue_event(self, watch: ObservedWatch, event: FileSystemEvent) -> None:
        event_filter = watch.event_filter
        if event_filter is None or any(isinstance(event, cls) for cls in event_filter):
            self._event_queue.put((event, watch))

    def on_thread_start(self) -> None:
        """Take the first shared snapshot and baseline every subscribed watch."""
        snapshot = self._take_snapshot()
        with self._lock:
            self._latest = snapshot
            for watch in self._subscriptions:
                self._subscriptions[watch] = self._scoped_view(watch, snapshot)

    def queue_events(self, timeout: Seconds) -> None:
        """Wait ``timeout`` seconds, scan once, then queue events per watch.

        Behaves like :meth:`PollingEmitter.queue_events`, except that a single
        snapshot of the shared root serves every subscribed watch. Each watch
        is diffed against its own previous scoped view, so moves across a
        watch's scope boundary are reported as creations or deletions for that
        watch, exactly as a dedicated emitter would report them.
        """

        # We don't want to hit the disk continuously.
        # timeout behaves like an interval for polling emitters.
        if self.stopped_event.wait(timeout):
            return

        with self._lock:
            if not self.should_keep_running():
                return

            try:
                new_snapshot = self._take_snapshot()
            except (FileNotFoundError, NotADirectoryError):
                for watch in self._subscriptions:
                    self._queue_event(watch, DirDeletedEvent(watch.path))
                self.stop()
                return
            except OSError as exc:
                # See PollingEmitter.queue_events: transient errors are
                # logged once and retried on the next poll.
                if not self._snapshot_failing:
                    self._snapshot_failing = True
                    logger.warning("Cannot take a snapshot of %r, will retry: %s", self.root, exc)
                return

            if self._snapshot_failing:
                self._snapshot_failing = False
                logger.info("Snapshot of %r succeeded again, resuming", self.root)

            self._latest = new_snapshot
            for watch, previous in self._subscriptions.items():
                view = self._scoped_view(watch, new_snapshot)
                events = DirectorySnapshotDiff(previous, view)
                self._subscriptions[watch] = view
                _queue_diff_events(events, partial(self._queue_event, watch))


class PollingObserver(BaseObserver):
    """Platform-independent observer that polls a directory to detect file
    system changes.

    If a poll fails because the watched directory no longer exists
    (``FileNotFoundError`` or ``NotADirectoryError``), a
    :class:`~watchdog.events.DirDeletedEvent` is emitted and watching stops.
    Any other ``OSError``, for instance while a network share is briefly
    unreachable, is logged once and the next poll tries again. Changes made
    in the meantime are reported once the directory can be read again. An
    outage that the OS reports as ``ENOENT`` still looks like a deletion.

    :param timeout:
        Seconds to wait between successive polls of the file system.
    :param share_scans:
        ``True`` to let watches that resolve to the same directory share one
        snapshot collection per poll (a :class:`SharedPollingEmitter`) instead
        of scanning the tree once per watch. Events are still distributed per
        watch, spelled with the watch's own path and limited to its recursive
        scope and event filter. Watches on different directories that happen
        to share a name are never merged. Defaults to ``False``.
    :param emitter_kwargs:
        Extra keyword arguments forwarded to every emitter this observer
        constructs, such as ``stat`` and ``listdir``.
    """

    def __init__(
        self,
        *,
        timeout: Seconds = DEFAULT_OBSERVER_TIMEOUT,
        share_scans: bool = False,
        **emitter_kwargs: Any,
    ) -> None:
        super().__init__(PollingEmitter, timeout=timeout, **emitter_kwargs)
        self._share_scans = share_scans
        self._shared_emitters: dict[str, SharedPollingEmitter] = {}

    def schedule(
        self,
        event_handler: FileSystemEventHandler,
        path: str | Path,
        *,
        recursive: bool = False,
        event_filter: list[type[FileSystemEvent]] | None = None,
        follow_symlink: bool = False,
    ) -> ObservedWatch:
        """Schedules watching a path; see :meth:`BaseObserver.schedule`.

        With ``share_scans`` enabled, watches that resolve to the same
        directory subscribe to a single shared emitter instead of getting a
        dedicated one.
        """
        if not self._share_scans:
            return super().schedule(
                event_handler,
                path,
                recursive=recursive,
                event_filter=event_filter,
                follow_symlink=follow_symlink,
            )
        with self._lock:
            watch = ObservedWatch(path, recursive=recursive, event_filter=event_filter, follow_symlink=follow_symlink)

            # If we don't have an emitter for this watch already, subscribe it
            # to the shared emitter for its directory, creating it if needed.
            if watch not in self._emitter_for_watch:
                identity = _scan_identity(watch.path)
                emitter = self._shared_emitters.get(identity)
                if emitter is None:
                    emitter = SharedPollingEmitter(
                        self.event_queue,
                        os.path.realpath(watch.path),
                        timeout=self.timeout,
                        **self._emitter_kwargs,
                    )
                    self._shared_emitters[identity] = emitter
                    emitter.add_watch(watch)
                    if self.is_alive():
                        try:
                            emitter.start()
                        except Exception:
                            emitter.remove_watch(watch)
                            del self._shared_emitters[identity]
                            raise
                else:
                    emitter.add_watch(watch)
                self._emitter_for_watch[watch] = emitter
                self._emitters.add(emitter)
            self._add_handler_for_watch(event_handler, watch)
            self._watches.add(watch)
        return watch

    def unschedule(self, watch: ObservedWatch) -> None:
        """Unschedules a watch.

        With ``share_scans`` enabled, the shared emitter keeps polling for the
        remaining watches; it is stopped only when its last watch goes away.
        """
        if not self._share_scans:
            super().unschedule(watch)
            return
        with self._lock:
            emitter = self._emitter_for_watch[watch]
            del self._handlers[watch]
            emitter.remove_watch(watch)
            del self._emitter_for_watch[watch]
            if emitter.reader_count == 0:
                self._shared_emitters.pop(_scan_identity(emitter.root), None)
                self._emitters.discard(emitter)
                emitter.stop()
                with contextlib.suppress(RuntimeError):
                    emitter.join()
            self._watches.remove(watch)

    def _remove_emitter(self, emitter: EventEmitter) -> None:
        if isinstance(emitter, SharedPollingEmitter):
            for watch, owner in list(self._emitter_for_watch.items()):
                if owner is emitter:
                    del self._emitter_for_watch[watch]
            for identity, shared in list(self._shared_emitters.items()):
                if shared is emitter:
                    del self._shared_emitters[identity]
            self._emitters.discard(emitter)
            emitter.stop()
            with contextlib.suppress(RuntimeError):
                emitter.join()
            return
        super()._remove_emitter(emitter)

    def _clear_emitters(self) -> None:
        self._shared_emitters.clear()
        super()._clear_emitters()


class PollingObserverVFS(PollingObserver):
    """File system independent observer that polls a directory to detect changes."""

    def __init__(
        self,
        stat: Callable[[str], os.stat_result],
        listdir: Callable[[str | None], Iterator[os.DirEntry]],
        *,
        polling_interval: Seconds = 1,
        share_scans: bool = False,
    ) -> None:
        """:param stat: stat function. See ``os.stat`` for details.
        :param listdir: listdir function. See ``os.scandir`` for details.
        :type polling_interval: Seconds
        :param polling_interval: interval between polling the file system.
        :param share_scans: share one scan between watches on the same
            directory. See :class:`PollingObserver`.
        """
        super().__init__(timeout=polling_interval, share_scans=share_scans, stat=stat, listdir=listdir)
