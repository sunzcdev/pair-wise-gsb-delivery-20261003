""":module: watchdog.observers.polling
:synopsis: Polling emitter implementation.
:author: yesudeep@google.com (Yesudeep Mangalapilly)
:author: Mickaël Schoentgen <contact@tiger-222.fr>

Classes
-------
.. autoclass:: PollingEmitter
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
from watchdog.observers.api import DEFAULT_EMITTER_TIMEOUT, DEFAULT_OBSERVER_TIMEOUT, BaseObserver, EventEmitter
from watchdog.utils.dirsnapshot import (
    DirectorySnapshot,
    DirectorySnapshotDiff,
    EmptyDirectorySnapshot,
    PartialDirectorySnapshot,
)

if TYPE_CHECKING:
    from collections.abc import Callable, Iterator

    from watchdog.events import FileSystemEvent
    from watchdog.observers.api import EventQueue, ObservedWatch
    from watchdog.utils import Seconds

logger = logging.getLogger(__name__)


class PollingEmitter(EventEmitter):
    """Platform-independent emitter that polls a directory to detect file
    system changes.

    :param partial_snapshots:
        When ``True``, polls use :class:`~watchdog.utils.dirsnapshot.PartialDirectorySnapshot`
        instead of :class:`~watchdog.utils.dirsnapshot.DirectorySnapshot`. A
        path that cannot be read (for instance a subdirectory that is briefly
        unreadable, or an ``EIO`` from a network mount) then keeps the state it
        had during the previous poll: changes elsewhere in the tree are still
        reported, and nothing is reported as deleted for the part that could
        not be read. Once it becomes readable again, its actual changes are
        reported.
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
        partial_snapshots: bool = False,
    ) -> None:
        super().__init__(event_queue, watch, timeout=timeout, event_filter=event_filter)
        self._snapshot: DirectorySnapshot = EmptyDirectorySnapshot()
        self._lock = threading.Lock()
        self._snapshot_failing = False
        self._partial_snapshots = partial_snapshots
        self._unreadable_paths: set[bytes | str] = set()
        self._stat = stat
        self._listdir = listdir

    def _new_snapshot(self) -> DirectorySnapshot:
        if self._partial_snapshots:
            # Unreadable paths inherit their last known state from the previous
            # poll instead of being reported as deleted.
            return PartialDirectorySnapshot(
                self.watch.path,
                recursive=self.watch.is_recursive,
                stat=self._stat,
                listdir=self._listdir,
                previous=self._snapshot,
            )
        return DirectorySnapshot(
            self.watch.path,
            recursive=self.watch.is_recursive,
            stat=self._stat,
            listdir=self._listdir,
        )

    def on_thread_start(self) -> None:
        """Take the first snapshot of the watched directory.

        Later snapshots are compared against this one to detect changes.
        """

        self._snapshot = self._new_snapshot()

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

        When :attr:`partial_snapshots` is enabled and some paths could not be
        read, the events are still queued for the rest of the tree, and the
        paths that could not be read keep their previous state.

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
                new_snapshot = self._new_snapshot()
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

            if self._partial_snapshots:
                self._log_incomplete(new_snapshot)

            events = DirectorySnapshotDiff(self._snapshot, new_snapshot)
            self._snapshot = new_snapshot

            # Files.
            for src_path in events.files_deleted:
                self.queue_event(FileDeletedEvent(src_path))
            for src_path in events.files_modified:
                self.queue_event(FileModifiedEvent(src_path))
            for src_path in events.files_created:
                self.queue_event(FileCreatedEvent(src_path))
            for src_path, dest_path in events.files_moved:
                self.queue_event(FileMovedEvent(src_path, dest_path))
            for src_path in events.files_attrib:
                self.queue_event(FileAttribEvent(src_path))

            # Directories.
            for src_path in events.dirs_deleted:
                self.queue_event(DirDeletedEvent(src_path))
            for src_path in events.dirs_modified:
                self.queue_event(DirModifiedEvent(src_path))
            for src_path in events.dirs_created:
                self.queue_event(DirCreatedEvent(src_path))
            for src_path, dest_path in events.dirs_moved:
                self.queue_event(DirMovedEvent(src_path, dest_path))
            for src_path in events.dirs_attrib:
                self.queue_event(DirAttribEvent(src_path))

    @property
    def partial_snapshots(self) -> bool:
        """Whether polls may return an incomplete snapshot of the tree."""
        return self._partial_snapshots

    def _log_incomplete(self, snapshot: DirectorySnapshot) -> None:
        """Warn, once per outage, about paths whose state could not be refreshed."""
        unreadable = snapshot.unreadable
        if not unreadable:
            if self._unreadable_paths:
                self._unreadable_paths = set()
                logger.info("Polling %r is complete again", self.watch.path)
            return

        new_paths = set(unreadable) - self._unreadable_paths
        if new_paths:
            for path in sorted(new_paths, key=str):
                logger.warning(
                    "Cannot read %r while polling %r, keeping its last known state: %s",
                    path,
                    self.watch.path,
                    unreadable[path],
                )
        self._unreadable_paths = set(unreadable)


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
    """

    def __init__(self, *, timeout: Seconds = DEFAULT_OBSERVER_TIMEOUT) -> None:
        super().__init__(PollingEmitter, timeout=timeout)


class PollingObserverVFS(BaseObserver):
    """File system independent observer that polls a directory to detect changes.

    Polls are allowed to return an incomplete view of the tree: when a single
    subdirectory or file cannot be read (a ``PermissionError``, or a transient
    ``OSError`` such as ``EIO`` from a network share), it keeps the state it
    had during the previous poll. Changes elsewhere in the tree are still
    reported, and the entries of the part that could not be read are not
    reported as deleted. Once it can be read again, the changes that really
    happened in the meantime are reported.

    A directory that has genuinely disappeared is still reported as deleted:
    it is not merely unreadable, so it is not kept. This makes the difference
    between

    - an unreadable directory, whose last known state is preserved, and
    - a directory that was really removed, whose entries are reported deleted
      (``DirDeletedEvent`` for the directory and ``DirDeletedEvent`` or
      ``FileDeletedEvent`` for each entry), and
    - a directory that became readable again with fewer files, whose missing
      files are reported deleted at that point.
    """

    def __init__(
        self,
        stat: Callable[[str], os.stat_result],
        listdir: Callable[[str | None], Iterator[os.DirEntry]],
        *,
        polling_interval: Seconds = 1,
    ) -> None:
        """:param stat: stat function. See ``os.stat`` for details.
        :param listdir: listdir function. See ``os.scandir`` for details.
        :type polling_interval: Seconds
        :param polling_interval: interval between polling the file system.
        """
        emitter_cls = partial(PollingEmitter, stat=stat, listdir=listdir, partial_snapshots=True)
        super().__init__(emitter_cls, timeout=polling_interval)  # type: ignore[arg-type]
