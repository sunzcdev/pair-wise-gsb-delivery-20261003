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
from watchdog.utils.dirsnapshot import DirectorySnapshot, DirectorySnapshotDiff, EmptyDirectorySnapshot

if TYPE_CHECKING:
    from collections.abc import Callable, Iterator

    from watchdog.events import FileSystemEvent
    from watchdog.observers.api import EventQueue, ObservedWatch
    from watchdog.utils import Seconds

logger = logging.getLogger(__name__)


class PollingEmitter(EventEmitter):
    """Platform-independent emitter that polls a directory to detect file
    system changes.

    Each emitter owns an adaptive polling cadence for its watch: while
    consecutive polls find no changes the interval grows by
    ``polling_backoff_factor`` up to ``max_polling_interval``, and the first
    poll that finds changes (or fails to read the directory) drops it back to
    ``timeout`` so new activity is picked up promptly.
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
        max_polling_interval: Seconds | None = None,
        polling_backoff_factor: float = 2.0,
    ) -> None:
        super().__init__(event_queue, watch, timeout=timeout, event_filter=event_filter)
        if max_polling_interval is not None and max_polling_interval < timeout:
            error = f"max_polling_interval ({max_polling_interval}) must be >= the polling interval ({timeout})"
            raise ValueError(error)
        if polling_backoff_factor < 1:
            error = f"polling_backoff_factor must be >= 1 (got {polling_backoff_factor!r})"
            raise ValueError(error)
        self._snapshot: DirectorySnapshot = EmptyDirectorySnapshot()
        self._lock = threading.Lock()
        self._snapshot_failing = False
        self._min_interval: Seconds = timeout
        self._max_interval: Seconds = timeout if max_polling_interval is None else max_polling_interval
        self._backoff_factor = polling_backoff_factor
        self._current_interval: Seconds = self._min_interval
        self._take_snapshot: Callable[[], DirectorySnapshot] = lambda: DirectorySnapshot(
            self.watch.path,
            recursive=self.watch.is_recursive,
            stat=stat,
            listdir=listdir,
        )

    @property
    def current_polling_interval(self) -> Seconds:
        """The interval this watch currently polls at, between ``timeout``
        and ``max_polling_interval``.
        """
        return self._current_interval

    def _note_poll_idle(self) -> None:
        """A poll found no changes: back off gradually, up to the cap."""
        self._current_interval = min(self._current_interval * self._backoff_factor, self._max_interval)

    def _note_poll_activity(self) -> None:
        """A poll found changes: return to the shortest interval right away."""
        self._current_interval = self._min_interval

    def _note_poll_failure(self) -> None:
        """A poll failed to read the directory.

        A read failure is a distinct state, not silence: it must not back the
        interval off as an idle poll would, and retries stay on the shortest
        interval so a recovering directory is picked up promptly.
        """
        self._current_interval = self._min_interval

    def on_thread_start(self) -> None:
        """Take the first snapshot of the watched directory.

        The baseline is established as soon as the emitter starts — before
        the first polling interval elapses — so a newly scheduled watch never
        reports its pre-existing content as new. Later snapshots are compared
        against this one to detect changes.
        """

        self._snapshot = self._take_snapshot()

    def queue_events(self, timeout: Seconds) -> None:
        """Wait for the current polling interval, then queue events for any
        changes.

        Takes a new snapshot of the watched directory and compares it with the
        previous one. An event is queued for each file or directory that was
        created, deleted, modified or moved. Returns early if the emitter is
        stopped while waiting.

        If the watched directory no longer exists or is not a directory, a
        :class:`~watchdog.events.DirDeletedEvent` is queued and the emitter
        stops. Other :class:`OSError` exceptions are logged and retried on the
        next poll.

        :param timeout:
            Configured polling interval, used as the shortest interval. The
            actual wait adapts per watch between ``timeout`` and
            ``max_polling_interval``; an explicit ``timeout`` of 0 skips the
            wait and scans immediately.
        """

        # We don't want to hit the disk continuously.
        # timeout behaves like an interval for polling emitters, stretched
        # per watch by the adaptive scheduler.
        if timeout and self.stopped_event.wait(self._current_interval):
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
                # in the meantime are still reported. A failure is not silence:
                # it keeps the watch on its shortest polling interval.
                self._note_poll_failure()
                if not self._snapshot_failing:
                    self._snapshot_failing = True
                    logger.warning("Cannot take a snapshot of %r, will retry: %s", self.watch.path, exc)
                return

            if self._snapshot_failing:
                self._snapshot_failing = False
                logger.info("Snapshot of %r succeeded again, resuming", self.watch.path)

            events = DirectorySnapshotDiff(self._snapshot, new_snapshot)
            self._snapshot = new_snapshot

            if len(events):
                self._note_poll_activity()
            else:
                self._note_poll_idle()

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
        Shortest polling interval, used by every watch right after it is
        scheduled and whenever its directory keeps changing.
    :param max_polling_interval:
        Optional upper bound enabling adaptive polling. Each watch that finds
        no changes stretches its own interval by ``polling_backoff_factor``
        per poll, up to this cap; a poll that finds changes — or fails to
        read the directory — drops that watch back to ``timeout``. A change
        in a quiet directory is therefore always delivered within
        ``max_polling_interval``. ``None`` (default) polls every ``timeout``.
    :param polling_backoff_factor:
        Multiplier applied to a watch's interval after each poll that finds
        no changes. Must be >= 1.
    """

    def __init__(
        self,
        *,
        timeout: Seconds = DEFAULT_OBSERVER_TIMEOUT,
        max_polling_interval: Seconds | None = None,
        polling_backoff_factor: float = 2.0,
    ) -> None:
        super().__init__(
            PollingEmitter,
            timeout=timeout,
            max_polling_interval=max_polling_interval,
            polling_backoff_factor=polling_backoff_factor,
        )


class PollingObserverVFS(BaseObserver):
    """File system independent observer that polls a directory to detect changes."""

    def __init__(
        self,
        stat: Callable[[str], os.stat_result],
        listdir: Callable[[str | None], Iterator[os.DirEntry]],
        *,
        polling_interval: Seconds = 1,
        max_polling_interval: Seconds | None = None,
        polling_backoff_factor: float = 2.0,
    ) -> None:
        """:param stat: stat function. See ``os.stat`` for details.
        :param listdir: listdir function. See ``os.scandir`` for details.
        :type polling_interval: Seconds
        :param polling_interval: shortest interval between polls of the file
            system; also the interval used while a directory keeps changing.
        :type max_polling_interval: Seconds
        :param max_polling_interval: optional cap enabling adaptive polling;
            quiet watches back off up to this interval. See
            :class:`PollingObserver`.
        :type polling_backoff_factor: float
        :param polling_backoff_factor: multiplier applied to a quiet watch's
            interval after each poll that finds no changes. Must be >= 1.
        """
        emitter_cls = partial(
            PollingEmitter,
            stat=stat,
            listdir=listdir,
            max_polling_interval=max_polling_interval,
            polling_backoff_factor=polling_backoff_factor,
        )
        super().__init__(emitter_cls, timeout=polling_interval)  # type: ignore[arg-type]
