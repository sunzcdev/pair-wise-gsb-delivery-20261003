from __future__ import annotations

import logging
import threading
import time
from typing import TYPE_CHECKING, overload

from watchdog.utils import BaseThread, renamed_attribute, resolve_renamed_argument

if TYPE_CHECKING:
    from collections.abc import Callable

    from watchdog.events import FileSystemEvent
    from watchdog.utils import Seconds

logger = logging.getLogger(__name__)


class EventDebouncer(BaseThread):
    """Background thread for debouncing event handling.

    When an event is received, wait until the configured debounce interval
    passes before calling the callback.  If additional events are received
    before the interval passes, keep waiting.  When the debouncing interval
    passes, the callback will be called with a list of events in the order in
    which they were received.

    A batch can never grow older than ``max_batch_age``: even when events keep
    arriving without a quiet interval, the batch is handed to the callback once
    that age is reached, and any events received afterwards start a new batch.
    This guarantees an upper bound on delivery latency under continuous input.

    The callback runs without the debouncer's internal lock held. Events that
    arrive (or are enqueued again from inside the callback) while a callback is
    running therefore cannot be swept into the batch being processed: they are
    buffered and delivered with the next batch instead.

    :param debounce_interval:
        How long to wait before calling the callback.
    :type debounce_interval:
        ``Seconds``
    :param events_callback:
        The callback function to be invoked with the debounced events.
    :type events_callback:
        ``Callable[[list[FileSystemEvent]], None]``
    :param max_batch_age:
        Maximum age of a batch before it is delivered even if events keep
        arriving, so a steady stream of events can never postpone delivery
        indefinitely. Defaults to ``debounce_interval``.
    :type max_batch_age:
        ``Seconds``
    :param debounce_interval_seconds:
        Deprecated, use ``debounce_interval`` instead.
    :type debounce_interval_seconds:
        ``Seconds``
    """

    @overload
    def __init__(
        self,
        debounce_interval: Seconds,
        events_callback: Callable[[list[FileSystemEvent]], None],
        max_batch_age: Seconds | None = None,
    ) -> None: ...

    @overload
    def __init__(
        self,
        *,
        debounce_interval_seconds: Seconds,
        events_callback: Callable[[list[FileSystemEvent]], None],
        max_batch_age: Seconds | None = None,
    ) -> None: ...

    def __init__(
        self,
        debounce_interval: Seconds | None = None,
        events_callback: Callable[[list[FileSystemEvent]], None] | None = None,
        max_batch_age: Seconds | None = None,
        *,
        debounce_interval_seconds: Seconds | None = None,
    ) -> None:
        super().__init__()
        interval = resolve_renamed_argument(
            "debounce_interval_seconds",
            debounce_interval_seconds,
            "debounce_interval",
            debounce_interval,
            default=None,
            removed_in="9.0.0",
        )
        if interval is None:
            error = "EventDebouncer() missing required argument: 'debounce_interval'"
            raise TypeError(error)
        if events_callback is None:
            error = "EventDebouncer() missing required argument: 'events_callback'"
            raise TypeError(error)
        if interval < 0:
            error = "debounce_interval must be non-negative."
            raise ValueError(error)
        if max_batch_age is None:
            max_batch_age = interval
        elif max_batch_age < 0:
            error = "max_batch_age must be non-negative."
            raise ValueError(error)
        self.debounce_interval = interval
        self.max_batch_age = max_batch_age
        self.events_callback = events_callback

        self._events: list[FileSystemEvent] = []
        self._cond = threading.Condition()
        self._flush_on_stop = True

    debounce_interval_seconds = renamed_attribute(
        "debounce_interval_seconds",
        "debounce_interval",
        removed_in="9.0.0",
    )

    def handle_event(self, event: FileSystemEvent) -> None:
        with self._cond:
            self._events.append(event)
            self._cond.notify()

    def stop(self, flush: bool = True) -> None:
        """Signal the thread to stop.

        :param flush:
            When ``True`` (the default), events still pending at shutdown are
            delivered in a final callback. When ``False``, the trailing batch
            is discarded without invoking the callback.
        """
        with self._cond:
            self._flush_on_stop = flush
            super().stop()
            self._cond.notify()

    def time_to_flush(self, started: float) -> bool:
        elapsed = time.monotonic() - started
        return elapsed > self.debounce_interval or elapsed >= self.max_batch_age

    def _take_events(self) -> list[FileSystemEvent]:
        """Detach the pending batch under the lock, so later events cannot
        leak into it while the callback runs without holding the lock."""
        with self._cond:
            events = self._events
            self._events = []
        return events

    def run(self) -> None:
        """Runs the event loop that waits for incoming filesystem events and triggers
        the callback once the debounce interval has passed."""
        with self._cond:
            while self.should_keep_running():
                # Wait for first event (or shutdown).
                while self.should_keep_running() and not self._events:
                    self._cond.wait()

                if not self.should_keep_running():
                    break

                started = time.monotonic()
                while self.should_keep_running() and self._events:
                    if not self.debounce_interval:
                        break
                    # Check the hard batch-age deadline before waiting again:
                    # an event whose notification wakes us at the deadline must
                    # start the next batch rather than join this one.
                    if self.time_to_flush(started):
                        break
                    # The full quiet interval is enough unless the hard
                    # batch-age deadline comes first, in which case cap the wait
                    # so the deadline is met even without a notification.
                    if self.max_batch_age < self.debounce_interval:
                        timeout = max(0.0, self.max_batch_age - (time.monotonic() - started))
                    else:
                        timeout = self.debounce_interval
                    if timeout:
                        self._cond.wait(timeout=timeout)
                    else:
                        # The deadline is already due; wait for a notification
                        # (stop or a new event) instead of busy-looping. The
                        # deadline is re-checked immediately on wake.
                        self._cond.wait()
                    if self.time_to_flush(started):
                        break

                # On shutdown, leave the batch to the trailing block below so a
                # caller that asked not to flush can have it discarded.
                if self._events and self.should_keep_running():
                    # The callback is invoked without holding the condition so
                    # producers (and the callback itself) can enqueue events for
                    # the next batch while it runs.
                    self._cond.release()
                    try:
                        self.events_callback(self._take_events())
                    finally:
                        self._cond.acquire()

            # Send any events accrued before leaving, unless the caller asked
            # for the trailing batch to be dropped.
            if self._events and self._flush_on_stop:
                self._cond.release()
                try:
                    self.events_callback(self._take_events())
                finally:
                    self._cond.acquire()
