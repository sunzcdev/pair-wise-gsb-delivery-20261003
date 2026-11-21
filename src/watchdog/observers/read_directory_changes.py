from __future__ import annotations

import logging
import os.path
import platform
import threading
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
    generate_sub_created_events,
    generate_sub_moved_events,
)
from watchdog.observers.api import DEFAULT_EMITTER_TIMEOUT, DEFAULT_OBSERVER_TIMEOUT, BaseObserver, EventEmitter
from watchdog.observers.winapi import WATCHDOG_FILE_NOTIFY_FLAGS, DirectoryChangeReader, _require_notify_filter

if TYPE_CHECKING:
    from watchdog.events import FileSystemEvent
    from watchdog.observers.api import EventQueue, ObservedWatch
    from watchdog.utils import Seconds


logger = logging.getLogger(__name__)


class WindowsApiEmitter(EventEmitter):
    """Windows API-based emitter that uses ``ReadDirectoryChangesW`` or
    ``ReadDirectoryChangesExW`` to detect file system changes for a watch.

    :param notify_filter:
        ``dwNotifyFilter`` bitmask for this watch. See :class:`WindowsApiObserver`.
    :type notify_filter:
        ``int``
    """

    def __init__(
        self,
        event_queue: EventQueue,
        watch: ObservedWatch,
        *,
        timeout: Seconds = DEFAULT_EMITTER_TIMEOUT,
        event_filter: list[type[FileSystemEvent]] | None = None,
        notify_filter: int = WATCHDOG_FILE_NOTIFY_FLAGS,
    ) -> None:
        super().__init__(event_queue, watch, timeout=timeout, event_filter=event_filter)
        self.notify_filter = _require_notify_filter(notify_filter)
        self._lock = threading.RLock()
        self._reader: DirectoryChangeReader | None = None
        # The two halves of a rename are not guaranteed to reach one
        # get_events() batch: ReadDirectoryChangesW can end a buffer right after
        # the old name, leaving the new name as the first record of the next
        # read. ReadDirectoryChangesW emits the halves adjacently, so the old
        # name is only ever held until the next record is known; see
        # _settle_unpaired_rename() for what happens when it is not a new name.
        self._unpaired_rename_src_path = ""

    def on_thread_start(self) -> None:
        with self._lock:
            assert self._reader is None
            self._unpaired_rename_src_path = ""
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

    def _settle_unpaired_rename(self) -> None:
        """Report the old half of a rename whose new half never arrived.

        ``ReadDirectoryChangesW`` reports the old and the new name of a rename
        back to back, so the old half only waits for the next read to bring its
        new half. Once reads go quiet without it, all that is observable is
        that the old name is gone: the rename went outside the watch, or its
        new half was lost. A deletion is also how the other observers report a
        move out of the watched tree, and rescanning the watch would show no
        such name either.
        """
        src_path = self._unpaired_rename_src_path
        if not src_path:
            return
        self._unpaired_rename_src_path = ""
        # The Windows API does not say whether a removed path was a file or a
        # directory, see the is_removed branch below.
        self.queue_event(FileDeletedEvent(src_path))

    def _queue_created(self, src_path: str) -> None:
        isdir = os.path.isdir(src_path)
        self.queue_event((DirCreatedEvent if isdir else FileCreatedEvent)(src_path))
        if isdir and self.watch.is_recursive:
            for sub_created_event in generate_sub_created_events(src_path):
                self.queue_event(sub_created_event)

    def queue_events(self, timeout: Seconds) -> None:
        with self._lock:
            reader = self._reader
        if reader is None:
            return  # reader has been stopped
        winapi_events = reader.get_events(timeout)
        if not winapi_events:
            # A whole read went by without the new half of the last old half,
            # which ReadDirectoryChangesW reports right after it (its buffer may
            # end between the two). The pair is not coming: settle it now so it
            # cannot capture the new name of a later rename.
            self._settle_unpaired_rename()
            return
        should_stop = False
        for winapi_event in winapi_events:
            src_path = os.path.join(self.watch.path, winapi_event.src_path)

            if winapi_event.is_overflow:
                # Events were lost, so the new half of a pending old half may be
                # among them: pairing it with a new name read afterwards would
                # report a move that never happened. Drop it without a deletion
                # either, since the EventsLostEvent asks the watch to rescan.
                self._unpaired_rename_src_path = ""
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
            elif winapi_event.is_removed_self:
                # The watched directory itself is gone: a pending old half went
                # with it, and the root deletion below reports that directly.
                self._unpaired_rename_src_path = ""
                self.queue_event(DirDeletedEvent(self.watch.path))
                should_stop = True
            else:
                # ReadDirectoryChangesW reports the old half of a rename and its
                # new half back to back, so anything but the new half means the
                # old one will never be paired, whether it comes later in this
                # read or first in the next one.
                if self._unpaired_rename_src_path and not winapi_event.is_renamed_new:
                    self._settle_unpaired_rename()

                if winapi_event.is_renamed_old:
                    # The buffer can end right after the old half, leaving the
                    # new half as the first record of the next read: keep the
                    # name for it. A second old half settles the first.
                    self._unpaired_rename_src_path = src_path
                elif winapi_event.is_renamed_new:
                    dest_path = src_path
                    src_path = self._unpaired_rename_src_path
                    self._unpaired_rename_src_path = ""
                    if not src_path:
                        # Renamed into the watch from outside, or its old half
                        # was lost: only the new name is observable, and it
                        # exists.
                        self._queue_created(dest_path)
                    elif os.path.isdir(dest_path):
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
                    self._queue_created(src_path)
                elif winapi_event.is_removed:
                    self.queue_event(FileDeletedEvent(src_path))
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
