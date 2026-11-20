from __future__ import annotations

import contextlib
import logging
import os
import queue
import threading
from collections import defaultdict
from concurrent.futures import Future
from pathlib import Path
from typing import TYPE_CHECKING

from watchdog.events import FileSystemMovedEvent
from watchdog.utils import BaseThread
from watchdog.utils.bricks import SkipRepeatsQueue

if TYPE_CHECKING:
    import types
    from typing import Any, Self

    from watchdog.events import FileSystemEvent, FileSystemEventHandler
    from watchdog.utils import Seconds

DEFAULT_EMITTER_TIMEOUT: Seconds = 1.0
DEFAULT_OBSERVER_TIMEOUT: Seconds = 1.0

logger = logging.getLogger(__name__)


class EventQueue(SkipRepeatsQueue):
    """Thread-safe event queue based on a special queue that skips adding
    the same event (:class:`watchdog.events.FileSystemEvent`) multiple times
    consecutively. Thus avoiding dispatching multiple event handling
    calls when multiple identical events are produced quicker than an observer
    can consume them.

    :param maxsize:
        Maximum number of items allowed in the queue. If <= 0, the queue size is infinite.
    :type maxsize:
        ``int``
    """


class ObservedWatch:
    """An scheduled watch.

    :param path:
        Path string.
    :type path:
        ``str`` or :class:`pathlib.Path`
    :param recursive:
        ``True`` if watch should include subdirectories; ``False`` otherwise.
    :type recursive:
        ``bool``
    :param event_filter:
        Optional collection of :class:`watchdog.events.FileSystemEvent` to watch.
    :type event_filter:
        Iterable[:class:`watchdog.events.FileSystemEvent`] | None
    :param follow_symlink:
        ``True`` if symlinks are followed; ``False`` otherwise.
    :type follow_symlink:
        ``bool``
    """

    def __init__(
        self,
        path: str | Path,
        *,
        recursive: bool,
        event_filter: list[type[FileSystemEvent]] | None = None,
        follow_symlink: bool = False,
    ):
        raw_path = str(path) if isinstance(path, Path) else path
        self.raw_path = raw_path
        self.follow_symlink = follow_symlink
        self.event_filter = frozenset(event_filter) if event_filter is not None else None

        self.is_file = os.path.isfile(raw_path)
        self.resolved_path = (
            os.path.realpath(os.path.abspath(raw_path)) if follow_symlink else os.path.abspath(raw_path)
        )

        if self.is_file:
            self.path = os.path.dirname(self.resolved_path)
            self.is_recursive = False
        else:
            self.path = raw_path
            self.is_recursive = recursive

    @property
    def key(self) -> tuple[str, bool, frozenset[type[FileSystemEvent]] | None, bool]:
        """A tuple key identifying the watch (raw_path, recursive, event_filter, follow_symlink)."""
        return self.raw_path, self.is_recursive, self.event_filter, self.follow_symlink

    def __eq__(self, watch: object) -> bool:
        if not isinstance(watch, ObservedWatch):
            return NotImplemented
        return self.key == watch.key

    def __ne__(self, watch: object) -> bool:
        if not isinstance(watch, ObservedWatch):
            return NotImplemented
        return self.key != watch.key

    def __hash__(self) -> int:
        return hash(self.key)

    def __repr__(self) -> str:
        if self.event_filter is not None:
            event_filter_str = "|".join(sorted(_cls.__name__ for _cls in self.event_filter))
            event_filter_str = f", event_filter={event_filter_str}"
        else:
            event_filter_str = ""
        return f"<{type(self).__name__}: path={self.path!r}, is_recursive={self.is_recursive}{event_filter_str}>"


# Observer classes
class _HandlerSubscription:
    """Dispatch state for one (watch, handler) subscription.

    Each subscription owns a FIFO queue and a lazily started daemon worker
    thread, so a handler stuck in its callback never stalls dispatch to other
    handlers of the same watch. All mutable attributes are guarded by the
    owning observer's ``_lock``; methods ending in ``_locked`` must be called
    with that lock held.
    """

    _STOP = object()
    """Sentinel queued on close to terminate the worker thread."""

    def __init__(self, observer: BaseObserver, watch: ObservedWatch, handler: FileSystemEventHandler) -> None:
        self._observer = observer
        self.watch = watch
        self.handler = handler
        self._queue: queue.Queue[FileSystemEvent | object] = queue.Queue()
        self._thread: threading.Thread | None = None
        self._in_flight = 0
        self._closed = False
        self._done_future: Future[None] | None = None

    def _enqueue_locked(self, event: FileSystemEvent) -> None:
        """Queues an event for this subscription. Caller must hold the observer lock."""
        if self._closed:
            return
        if self._thread is None:
            self._thread = threading.Thread(
                target=self._run,
                name=f"watchdog-handler-{id(self):x}",
                daemon=True,
            )
            self._thread.start()
        self._queue.put(event)

    def _close_locked(self) -> Future[None]:
        """Stops new dispatches and returns a future for in-flight completion.

        Caller must hold the observer lock. Events still queued are dropped, so
        a handler re-registered later never receives this subscription's old
        queue. The returned future resolves once the currently executing
        callback (if any) returns; it resolves immediately when idle.
        """
        if self._done_future is not None:
            return self._done_future
        self._closed = True
        future: Future[None] = Future()
        self._done_future = future
        if self._in_flight == 0:
            future.set_result(None)
        if self._thread is not None:
            # Drop pending events: no new callback may start after close. The
            # worker dequeues under the observer lock too, so it cannot be
            # between the closed check and dispatch while we hold the lock.
            while True:
                try:
                    self._queue.get_nowait()
                except queue.Empty:
                    break
            self._queue.put(self._STOP)
        return future

    def _run(self) -> None:
        while True:
            event = self._queue.get()
            if event is self._STOP:
                return
            with self._observer._lock:  # noqa: SLF001
                if self._closed:
                    continue
                self._in_flight += 1
            try:
                self.handler.dispatch(event)  # type: ignore[union-attr]
            except Exception:
                logger.exception("Unhandled exception in event handler %r", self.handler)
            finally:
                with self._observer._lock:  # noqa: SLF001
                    self._in_flight -= 1
                    if self._closed and self._in_flight == 0 and self._done_future is not None:
                        self._done_future.set_result(None)


class EventEmitter(BaseThread):
    """Producer thread base class subclassed by event emitters
    that generate events and populate a queue with them.

    :param event_queue:
        The event queue to populate with generated events.
    :type event_queue:
        :class:`EventQueue`
    :param watch:
        The watch to observe and produce events for.
    :type watch:
        :class:`ObservedWatch`
    :param timeout:
        Timeout between successive attempts at reading events.
    :type timeout:
        ``Seconds``
    :param event_filter:
        Collection of event types to emit, or None for no filtering (default).
    :type event_filter:
        Iterable[:class:`watchdog.events.FileSystemEvent`] | None
    """

    def __init__(
        self,
        event_queue: EventQueue,
        watch: ObservedWatch,
        *,
        timeout: Seconds = DEFAULT_EMITTER_TIMEOUT,
        event_filter: list[type[FileSystemEvent]] | None = None,
    ) -> None:
        super().__init__()
        # bool subclasses int; timeout=True would silently become 1.0s
        if isinstance(timeout, bool):
            error = f"timeout must be a float, not bool (got {timeout!r})"
            raise TypeError(error)
        self._event_queue = event_queue
        self._watch = watch
        self._timeout = timeout
        self._event_filter = frozenset(event_filter) if event_filter is not None else None

    @property
    def timeout(self) -> Seconds:
        """Blocking timeout for reading events."""
        return self._timeout

    @property
    def watch(self) -> ObservedWatch:
        """The watch associated with this emitter."""
        return self._watch

    def queue_event(self, event: FileSystemEvent) -> None:
        """Queues a single event.

        :param event:
            Event to be queued.
        :type event:
            An instance of :class:`watchdog.events.FileSystemEvent`
            or a subclass.
        """
        if self._event_filter is None or any(isinstance(event, cls) for cls in self._event_filter):
            self._event_queue.put((event, self.watch))

    def queue_events(self, timeout: Seconds) -> None:
        """Override this method to populate the event queue with events
        per interval period.

        :param timeout:
            Timeout between successive attempts at
            reading events.
        :type timeout:
            ``Seconds``
        """

    def run(self) -> None:
        while self.should_keep_running():
            self.queue_events(self.timeout)


class EventDispatcher(BaseThread):
    """Consumer thread base class subclassed by event observer threads
    that dispatch events from an event queue to appropriate event handlers.

    :param timeout:
        Timeout value passed to emitters
        constructions in the child class BaseObserver.
    :type timeout:
        ``Seconds``
    """

    stop_event = object()
    """Event inserted into the queue to signal a requested stop."""

    def __init__(self, *, timeout: Seconds = DEFAULT_OBSERVER_TIMEOUT) -> None:
        super().__init__()
        # bool subclasses int; timeout=True would silently become 1.0s
        if isinstance(timeout, bool):
            error = f"timeout must be a float, not bool (got {timeout!r})"
            raise TypeError(error)
        self._event_queue = EventQueue()
        self._timeout = timeout

    @property
    def timeout(self) -> Seconds:
        """Timeout value to construct emitters with."""
        return self._timeout

    def stop(self) -> None:
        BaseThread.stop(self)
        with contextlib.suppress(queue.Full):
            self.event_queue.put_nowait(EventDispatcher.stop_event)

    @property
    def event_queue(self) -> EventQueue:
        """The event queue which is populated with file system events
        by emitters and from which events are dispatched by a dispatcher
        thread.
        """
        return self._event_queue

    def dispatch_events(self, event_queue: EventQueue) -> None:
        """Override this method to consume events from an event queue, blocking
        on the queue for the specified timeout before raising :class:`queue.Empty`.

        :param event_queue:
            Event queue to populate with one set of events.
        :type event_queue:
            :class:`EventQueue`
        :raises:
            :class:`queue.Empty`
        """

    def run(self) -> None:
        while self.should_keep_running():
            try:
                self.dispatch_events(self.event_queue)
            except queue.Empty:
                continue


class BaseObserver(EventDispatcher):
    """Base observer.

    :param emitter_class:
        The class of the emitter to use (e.g., a subclass of :class:`EventEmitter`).
    :type emitter_class:
        ``type[EventEmitter]``
    :param timeout:
        The timeout for the observer to wait on events.
    :type timeout:
        ``Seconds``
    :param kwargs:
        Extra keyword arguments forwarded to every emitter this observer
        constructs, for platform-specific emitter options.
    """

    def __init__(
        self,
        emitter_class: type[EventEmitter],
        *,
        timeout: Seconds = DEFAULT_OBSERVER_TIMEOUT,
        **kwargs: Any,
    ) -> None:
        super().__init__(timeout=timeout)
        self._emitter_class = emitter_class
        self._emitter_kwargs = kwargs
        self._lock = threading.RLock()
        self._watches: set[ObservedWatch] = set()
        self._handlers: defaultdict[ObservedWatch, set[FileSystemEventHandler]] = defaultdict(set)
        self._subscriptions: dict[tuple[ObservedWatch, FileSystemEventHandler], _HandlerSubscription] = {}
        self._emitters: set[EventEmitter] = set()
        self._emitter_for_watch: dict[ObservedWatch, EventEmitter] = {}

    def _add_emitter(self, emitter: EventEmitter) -> None:
        self._emitter_for_watch[emitter.watch] = emitter
        self._emitters.add(emitter)

    def _remove_emitter(self, emitter: EventEmitter) -> None:
        del self._emitter_for_watch[emitter.watch]
        self._emitters.remove(emitter)
        emitter.stop()
        with contextlib.suppress(RuntimeError):
            emitter.join()

    def _clear_emitters(self) -> None:
        for emitter in self._emitters:
            emitter.stop()
        for emitter in self._emitters:
            with contextlib.suppress(RuntimeError):
                emitter.join()
        self._emitters.clear()
        self._emitter_for_watch.clear()

    def _add_handler_for_watch(self, event_handler: FileSystemEventHandler, watch: ObservedWatch) -> None:
        self._handlers[watch].add(event_handler)
        key = (watch, event_handler)
        if key not in self._subscriptions:
            self._subscriptions[key] = _HandlerSubscription(self, watch, event_handler)

    def _remove_handlers_for_watch(self, watch: ObservedWatch) -> None:
        del self._handlers[watch]
        for key in [key for key in self._subscriptions if key[0] == watch]:
            self._subscriptions.pop(key)._close_locked()

    @property
    def emitters(self) -> set[EventEmitter]:
        """Returns event emitter created by this observer."""
        return self._emitters

    def start(self) -> None:
        """Starts the observer thread and all scheduled emitters under synchronization lock.

        Overrides :meth:`watchdog.utils.BaseThread.start` to guarantee atomicity between
        observer startup and concurrent :meth:`schedule` invocations (prevents #1044).
        """
        with self._lock:
            super().start()

    def on_thread_start(self) -> None:
        """Start all scheduled emitters before the observer thread begins dispatching."""
        with self._lock:
            for emitter in self._emitters.copy():
                try:
                    emitter.start()
                except Exception:
                    self._remove_emitter(emitter)
                    raise

    def __enter__(self) -> Self:
        """Context manager entry. Starts the observer if not already running."""
        if not self.is_alive():
            self.start()
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: types.TracebackType | None,
    ) -> None:
        """Context manager exit. Stops the observer and waits for it to finish."""
        if self.is_alive():
            self.stop()
            self.join()

    def schedule(
        self,
        event_handler: FileSystemEventHandler,
        path: str | Path,
        *,
        recursive: bool = False,
        event_filter: list[type[FileSystemEvent]] | None = None,
        follow_symlink: bool = False,
    ) -> ObservedWatch:
        """Schedules watching a path and calls appropriate methods specified
        in the given event handler in response to file system events.

        :param event_handler:
            An event handler instance that has appropriate event handling
            methods which will be called by the observer in response to
            file system events.
        :type event_handler:
            :class:`watchdog.events.FileSystemEventHandler` or a subclass
        :param path:
            Directory or file path that will be monitored.
        :type path:
            ``str`` or :class:`pathlib.Path`
        :param recursive:
            ``True`` if events will be emitted for sub-directories
            traversed recursively; ``False`` otherwise.
        :type recursive:
            ``bool``
        :param event_filter:
            Collection of event types to emit, or None for no filtering (default).
        :type event_filter:
            Iterable[:class:`watchdog.events.FileSystemEvent`] | None
        :param follow_symlink:
            ``True`` if symlinks should be followed; ``False`` otherwise.
        :type follow_symlink:
            ``bool``
        :return:
            An :class:`ObservedWatch` object instance representing
            a watch.
        """
        with self._lock:
            watch = ObservedWatch(path, recursive=recursive, event_filter=event_filter, follow_symlink=follow_symlink)

            # If we don't have an emitter for this watch already, create it.
            if watch not in self._emitter_for_watch:
                emitter = self._emitter_class(
                    self.event_queue,
                    watch,
                    timeout=self.timeout,
                    event_filter=event_filter,
                    **self._emitter_kwargs,
                )
                if self.is_alive():
                    emitter.start()
                self._add_emitter(emitter)
            self._add_handler_for_watch(event_handler, watch)
            self._watches.add(watch)
        return watch

    def add_handler_for_watch(self, event_handler: FileSystemEventHandler, watch: ObservedWatch) -> None:
        """Adds a handler for the given watch.

        :param event_handler:
            An event handler instance that has appropriate event handling
            methods which will be called by the observer in response to
            file system events.
        :type event_handler:
            :class:`watchdog.events.FileSystemEventHandler` or a subclass
        :param watch:
            The watch to add a handler for.
        :type watch:
            An instance of :class:`ObservedWatch` or a subclass of
            :class:`ObservedWatch`
        """
        with self._lock:
            self._add_handler_for_watch(event_handler, watch)

    def unregister_handler(self, event_handler: FileSystemEventHandler, watch: ObservedWatch) -> Future[None]:
        """Unregisters a handler from a watch and returns an awaitable completion handle.

        No new callback is dispatched to this subscription once this method
        returns, and events still queued for it are dropped (a handler
        re-registered afterwards starts from a fresh queue). The returned
        :class:`concurrent.futures.Future` resolves when the callback currently
        executing for this subscription, if any, has finished; it is already
        resolved when no callback is in flight.

        The method itself never blocks and never waits while holding the
        observer lock, so a handler may call it from within its own callback
        without deadlocking on itself. Callers running outside that callback
        may wait on the returned future to learn when in-flight work ends.

        :param event_handler:
            The event handler to unregister.
        :type event_handler:
            :class:`watchdog.events.FileSystemEventHandler` or a subclass
        :param watch:
            The watch the handler is registered for.
        :type watch:
            An instance of :class:`ObservedWatch` or a subclass of
            :class:`ObservedWatch`
        :return:
            A :class:`concurrent.futures.Future` resolving to ``None`` once the
            subscription's in-flight callbacks have finished.
        :raises KeyError:
            If the handler is not registered for the watch.
        """
        with self._lock:
            # Use .get() rather than indexing: _handlers is a defaultdict, so
            # indexing it here would resurrect a watch that unschedule() already
            # removed, leaving a permanent empty-set entry behind.
            handlers = self._handlers.get(watch)
            if not handlers or event_handler not in handlers:
                error = f"{event_handler!r} is not registered for {watch!r}"
                raise KeyError(error)
            handlers.discard(event_handler)
            subscription = self._subscriptions.pop((watch, event_handler), None)
            if subscription is None:  # pragma: no cover - defensive; kept in sync with _handlers
                future: Future[None] = Future()
                future.set_result(None)
            else:
                future = subscription._close_locked()
        return future

    def remove_handler_for_watch(self, event_handler: FileSystemEventHandler, watch: ObservedWatch) -> None:
        """Removes a handler for the given watch.

        Equivalent to :meth:`unregister_handler` without waiting on the
        returned completion future.

        :param event_handler:
            An event handler instance that has appropriate event handling
            methods which will be called by the observer in response to
            file system events.
        :type event_handler:
            :class:`watchdog.events.FileSystemEventHandler` or a subclass
        :param watch:
            The watch to remove a handler for.
        :type watch:
            An instance of :class:`ObservedWatch` or a subclass of
            :class:`ObservedWatch`
        """
        self.unregister_handler(event_handler, watch)

    def unschedule(self, watch: ObservedWatch) -> None:
        """Unschedules a watch.

        :param watch:
            The watch to unschedule.
        :type watch:
            An instance of :class:`ObservedWatch` or a subclass of
            :class:`ObservedWatch`
        """
        with self._lock:
            emitter = self._emitter_for_watch[watch]
            self._remove_handlers_for_watch(watch)
            self._remove_emitter(emitter)
            self._watches.remove(watch)

    def unschedule_all(self) -> None:
        """Unschedules all watches and detaches all associated event handlers."""
        with self._lock:
            self._handlers.clear()
            for subscription in self._subscriptions.values():
                subscription._close_locked()
            self._subscriptions.clear()
            self._clear_emitters()
            self._watches.clear()

    def on_thread_stop(self) -> None:
        self.unschedule_all()

    def dispatch_events(self, event_queue: EventQueue) -> None:
        entry = event_queue.get(block=True)
        if entry is EventDispatcher.stop_event:
            event_queue.task_done()
            return

        event, watch = entry

        if watch.is_file:
            target = watch.resolved_path
            src_path = (
                os.path.realpath(os.path.abspath(event.src_path))
                if watch.follow_symlink
                else os.path.abspath(event.src_path)
            )
            dest_path = None
            if isinstance(event, FileSystemMovedEvent) and event.dest_path:
                dest_path = (
                    os.path.realpath(os.path.abspath(event.dest_path))
                    if watch.follow_symlink
                    else os.path.abspath(event.dest_path)
                )
            if src_path != target and dest_path != target:
                event_queue.task_done()
                return

        # To allow unschedule/stop and safe removal of event handlers
        # within event handlers itself, check if the handler is still
        # registered before dispatching.
        # Use .get() rather than indexing: _handlers is a defaultdict, so
        # indexing it here would resurrect a watch that unschedule() removed
        # but whose events were already queued.
        # Each subscription owns a worker thread that invokes its handler
        # serially, so a handler blocked in its callback never stalls the
        # other handlers of the same watch, and this dispatcher never runs
        # user code while holding self._lock.
        event.watch_path = watch.path
        with self._lock:
            for handler in self._handlers.get(watch, ()):
                subscription = self._subscriptions.get((watch, handler))
                if subscription is not None:
                    subscription._enqueue_locked(event)
        event_queue.task_done()
