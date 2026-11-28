from __future__ import annotations

import logging
import os.path
import platform
import threading
from dataclasses import dataclass
from typing import TYPE_CHECKING

from watchdog.events import (
    DirAttribEvent,
    DirCreatedEvent,
    DirDeletedEvent,
    DirModifiedEvent,
    DirMovedEvent,
    EventsLostEvent,
    FileAttribEvent,
    FileCreatedEvent,
    FileDeletedEvent,
    FileModifiedEvent,
    FileMovedEvent,
    FileSystemMovedEvent,
    generate_sub_created_events,
    generate_sub_moved_events,
)
from watchdog.observers.api import DEFAULT_EMITTER_TIMEOUT, DEFAULT_OBSERVER_TIMEOUT, BaseObserver, EventEmitter
from watchdog.observers.winapi import WATCHDOG_FILE_NOTIFY_FLAGS, DirectoryChangeReader, _require_notify_filter

if TYPE_CHECKING:
    from collections.abc import Iterable

    from watchdog.events import FileSystemEvent
    from watchdog.observers.api import EventQueue, ObservedWatch
    from watchdog.utils import Seconds


logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class ManifestDiff:
    """Difference between the last trusted manifest and a confirmed recovery scan.

    :param created:
        Paths present in the confirmed scan but not in the last trusted manifest.
    :type created:
        ``frozenset[str]``
    :param deleted:
        Paths present in the last trusted manifest but not in the confirmed scan.
    :type deleted:
        ``frozenset[str]``
    """

    created: frozenset[str]
    deleted: frozenset[str]


class ManifestRecovery:
    """Optional recovery entry point for subscribers that maintain a path manifest.

    A subscriber mirroring the watched tree as a set of paths cannot keep
    applying events after an :class:`~watchdog.events.EventsLostEvent`: an
    unknown number of events were dropped, so the manifest may be stale. Pass
    one instance per watch to :class:`WindowsApiEmitter` and it tracks, across
    losses, the last trusted manifest (the paths as of the most recent
    loss-free moment) and the coverage interval that is still unconfirmed (the
    loss episodes the trusted manifest does not account for yet).

    While the coverage is unconfirmed, normal events are not applied to the
    trusted manifest: an unknown share of their siblings was dropped, and the
    confirming scan accounts for them. To recover, scan the watched directory
    and ask for confirmation::

        token = recovery.begin_scan()
        try:
            scanned = scan_paths(watch.path)
        except OSError:
            recovery.abort_scan(token)  # e.g. access denied: stays uncertain
        else:
            diff = recovery.finish_scan(token, scanned)
            if diff is not None:  # None: a loss landed mid-scan, scan again
                deliver(diff)

    A scan confirms only when no loss arrived between :meth:`begin_scan` and
    :meth:`finish_scan`, and the diff is always computed against the last
    trusted manifest, never against an unconfirmed scan. Once a scan confirms,
    normal events apply to the trusted manifest again.
    """

    def __init__(self, manifest: Iterable[str] = ()) -> None:
        self._lock = threading.RLock()
        self._trusted: set[str] = set(manifest)
        # Loss episodes are counted so a scan can tell whether the coverage it
        # set out to confirm grew while it was running.
        self._losses = 0
        self._confirmed_at = 0

    @property
    def trusted_manifest(self) -> frozenset[str]:
        """The last trusted manifest: paths as of the most recent loss-free moment."""
        with self._lock:
            return frozenset(self._trusted)

    @property
    def uncertain(self) -> bool:
        """True while the trusted manifest does not cover every loss episode seen."""
        with self._lock:
            return self._confirmed_at != self._losses

    @property
    def unconfirmed_coverage(self) -> tuple[int, int]:
        """The not yet confirmable coverage interval, as loss-episode indices
        ``(confirmed, seen)``: empty (both equal) once a scan has confirmed
        every loss."""
        with self._lock:
            return (self._confirmed_at, self._losses)

    def note_event(self, event: FileSystemEvent) -> None:
        """Record an event produced by the emitter.

        Called by :class:`WindowsApiEmitter` for every event it produces,
        before event filtering; subscribers normally do not call this.
        """
        with self._lock:
            if isinstance(event, EventsLostEvent):
                self._losses += 1
                return
            if self._confirmed_at != self._losses:
                # Past a loss the manifest is stale and an unknown share of
                # events was dropped; the confirming scan accounts for these.
                return
            if isinstance(event, (FileCreatedEvent, DirCreatedEvent)):
                self._trusted.add(event.src_path)
            elif isinstance(event, (FileDeletedEvent, DirDeletedEvent)):
                self._trusted.discard(event.src_path)
            elif isinstance(event, FileSystemMovedEvent):
                self._trusted.discard(event.src_path)
                if event.dest_path:
                    self._trusted.add(event.dest_path)
            # Modification, attrib, open and close events leave the manifest unchanged.

    def begin_scan(self) -> int:
        """Return the token a recovery scan of the watched directory must confirm.

        The token identifies the coverage the scan sets out to confirm; pass it
        to :meth:`finish_scan` or :meth:`abort_scan`.
        """
        with self._lock:
            return self._losses

    def finish_scan(self, token: int, manifest: Iterable[str]) -> ManifestDiff | None:
        """Confirm a completed scan and return its diff against the last trusted manifest.

        Returns ``None`` when a loss arrived after :meth:`begin_scan` issued
        the token: the scan does not cover the whole loss interval, so it is
        not delivered and the trusted manifest is left untouched. Otherwise
        the scanned manifest becomes the trusted one and its diff against the
        previous trusted manifest is returned.
        """
        with self._lock:
            if token != self._losses:
                return None
            scanned = set(manifest)
            diff = ManifestDiff(
                created=frozenset(scanned - self._trusted),
                deleted=frozenset(self._trusted - scanned),
            )
            self._trusted = scanned
            self._confirmed_at = self._losses
            return diff

    def abort_scan(self, token: int) -> None:
        """Discard a scan that could not complete, e.g. on ``PermissionError``.

        The trusted manifest is left untouched and the coverage interval stays
        unconfirmed whichever token the scan carried; scan again to recover.
        """


class WindowsApiEmitter(EventEmitter):
    """Windows API-based emitter that uses ``ReadDirectoryChangesW`` or
    ``ReadDirectoryChangesExW`` to detect file system changes for a watch.

    :param notify_filter:
        ``dwNotifyFilter`` bitmask for this watch. See :class:`WindowsApiObserver`.
    :type notify_filter:
        ``int``
    :param recovery:
        Optional :class:`ManifestRecovery` for a subscriber that maintains a
        path manifest for this watch. When given, every event this emitter
        produces updates the recovery before ``event_filter`` is applied, and
        a buffer overflow discards a pending rename half instead of letting it
        pair with a rename that completed after the loss. When omitted,
        overflow handling is unchanged.
    :type recovery:
        :class:`ManifestRecovery` | None
    """

    def __init__(
        self,
        event_queue: EventQueue,
        watch: ObservedWatch,
        *,
        timeout: Seconds = DEFAULT_EMITTER_TIMEOUT,
        event_filter: list[type[FileSystemEvent]] | None = None,
        notify_filter: int = WATCHDOG_FILE_NOTIFY_FLAGS,
        recovery: ManifestRecovery | None = None,
    ) -> None:
        super().__init__(event_queue, watch, timeout=timeout, event_filter=event_filter)
        self.notify_filter = _require_notify_filter(notify_filter)
        self.recovery = recovery
        self._lock = threading.RLock()
        self._reader: DirectoryChangeReader | None = None
        # The two halves of a rename are not guaranteed to reach one
        # get_events() batch, so the pairing outlives a queue_events() call.
        self._last_renamed_src_path = ""

    def on_thread_start(self) -> None:
        with self._lock:
            assert self._reader is None
            reader = DirectoryChangeReader(
                self.watch.path,
                recursive=self.watch.is_recursive,
                notify_filter=self.notify_filter,
            )
            self._reader = reader
        reader.start()

    if platform.python_implementation() == "PyPy":

        def start(self) -> None:
            """PyPy needs some time before receiving events, see #792."""
            from time import sleep

            super().start()
            sleep(0.01)

    def on_thread_stop(self) -> None:
        with self._lock:
            reader = self._reader
            self._reader = None
        if reader is not None:
            reader.stop()

    def queue_event(self, event: FileSystemEvent) -> None:
        recovery = self.recovery
        if recovery is not None:
            recovery.note_event(event)
        super().queue_event(event)

    def queue_events(self, timeout: Seconds) -> None:
        with self._lock:
            reader = self._reader
        if reader is None:
            return  # reader has been stopped
        should_stop = False
        for winapi_event in reader.get_events(timeout):
            src_path = os.path.join(self.watch.path, winapi_event.src_path)

            if winapi_event.is_renamed_old:
                self._last_renamed_src_path = src_path
            elif winapi_event.is_renamed_new:
                dest_path = src_path
                src_path = self._last_renamed_src_path
                self._last_renamed_src_path = ""
                if os.path.isdir(dest_path):
                    self.queue_event(DirMovedEvent(src_path, dest_path))
                    if self.watch.is_recursive:
                        for sub_moved_event in generate_sub_moved_events(src_path, dest_path):
                            self.queue_event(sub_moved_event)
                else:
                    self.queue_event(FileMovedEvent(src_path, dest_path))
            elif winapi_event.is_attrib:
                self.queue_event((DirAttribEvent if os.path.isdir(src_path) else FileAttribEvent)(src_path))
            elif winapi_event.is_modified:
                self.queue_event((DirModifiedEvent if os.path.isdir(src_path) else FileModifiedEvent)(src_path))
            elif winapi_event.is_added:
                isdir = os.path.isdir(src_path)
                self.queue_event((DirCreatedEvent if isdir else FileCreatedEvent)(src_path))
                if isdir and self.watch.is_recursive:
                    for sub_created_event in generate_sub_created_events(src_path):
                        self.queue_event(sub_created_event)
            elif winapi_event.is_removed:
                self.queue_event(FileDeletedEvent(src_path))
            elif winapi_event.is_removed_self:
                self.queue_event(DirDeletedEvent(self.watch.path))
                should_stop = True
            elif winapi_event.is_overflow:
                if self.recovery is not None:
                    # A rename split by a loss must not pair across it: the
                    # matching new-name record may be among the lost events,
                    # and a late one belongs to a rename the trusted manifest
                    # never saw.
                    self._last_renamed_src_path = ""
                logger.warning(
                    "Some file system events were lost due to a buffer overflow while watching %s",
                    self.watch.path,
                )
                self.queue_event(
                    EventsLostEvent(
                        self.watch.resolved_path if self.watch.is_file else self.watch.path,
                        is_directory=not self.watch.is_file,
                    )
                )
        if should_stop:
            # watched directory was deleted, stop observer threads
            self.stop()


class WindowsApiObserver(BaseObserver):
    """Observer thread that schedules watching directories and dispatches
    calls to event handlers.

    :param timeout:
        Timeout for the observer to wait on events.
    :type timeout:
        ``Seconds``
    :param notify_filter:
        ``dwNotifyFilter`` bitmask passed to ``ReadDirectoryChangesW`` and
        ``ReadDirectoryChangesExW`` for every watch this observer creates.
        Combine the ``FILE_NOTIFY_CHANGE_*`` constants in
        :mod:`watchdog.observers.winapi`. Defaults to
        :data:`~watchdog.observers.winapi.WATCHDOG_FILE_NOTIFY_FLAGS`, which
        includes ``FILE_NOTIFY_CHANGE_LAST_ACCESS``. Access-time updates can
        be far more frequent than content changes; mask that flag out when
        they are not useful, for example
        ``WATCHDOG_FILE_NOTIFY_FLAGS & ~FILE_NOTIFY_CHANGE_LAST_ACCESS``.
        Watches that need different masks belong on different observers.
    :type notify_filter:
        ``int``
    """

    def __init__(
        self,
        *,
        timeout: Seconds = DEFAULT_OBSERVER_TIMEOUT,
        notify_filter: int = WATCHDOG_FILE_NOTIFY_FLAGS,
    ) -> None:
        super().__init__(
            WindowsApiEmitter,
            timeout=timeout,
            notify_filter=_require_notify_filter(notify_filter),
        )
