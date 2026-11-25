from __future__ import annotations

import asyncio
import json
import logging
import os
import sys
import threading
import time
from collections import deque
from collections.abc import Mapping
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from http import HTTPStatus
from logging.config import dictConfig, fileConfig
from typing import Any, IO, TYPE_CHECKING

if sys.version_info >= (3, 11):
    import tomllib
else:
    import tomli as tomllib


if TYPE_CHECKING:
    from .config import Config
    from .typing import ResponseSummary, WWWScope


# Result of an ``AccessLogQueue.put`` attempt.
QUEUED = "queued"
# The entry could not be kept. For a failure this means the queue only held
# failure entries; for a success it means the queue was full. The rejection
# has already been counted against the entry's category.
REJECTED = "rejected"
# The entry was enqueued after an already queued success entry was evicted to
# make room for it. The evicted success entry will never be delivered.
EVICTED = "evicted"

# Category used for access entries that never produced a response, i.e. the
# request was disconnected before a status was available.
DISCONNECTED_CATEGORY = "disconnect"

logger = logging.getLogger("hypercorn.logging")


def _create_logger(
    name: str,
    target: logging.Logger | str | None,
    level: str | None,
    sys_default: IO,
    *,
    propagate: bool = True,
) -> logging.Logger | None:
    if isinstance(target, logging.Logger):
        return target

    if target:
        logger = logging.getLogger(name)
        logger.handlers = [
            logging.StreamHandler(sys_default) if target == "-" else logging.FileHandler(target)
        ]
        logger.propagate = propagate
        formatter = logging.Formatter(
            "%(asctime)s [%(process)d] [%(levelname)s] %(message)s",
            "[%Y-%m-%d %H:%M:%S %z]",
        )
        logger.handlers[0].setFormatter(formatter)
        if level is not None:
            logger.setLevel(logging.getLevelName(level.upper()))
        return logger
    else:
        return None


@dataclass(frozen=True)
class AccessLogEntry:
    """A single access log record waiting to be written to the sink."""

    message: str
    status: int | None

    @property
    def failure_category(self) -> str:
        return DISCONNECTED_CATEGORY if self.status is None else str(self.status)

    @property
    def is_failure(self) -> bool:
        return self.status is None or self.status >= 400


class AccessLogQueue:
    """Bounded, in-order queue for access log entries with a drop policy.

    Entries are always delivered in the order they were accepted. When the
    queue is full a new failure entry makes room by evicting the oldest
    *success* entry still waiting to be written. The entry currently being
    written is never held in this queue and therefore can never be revoked.

    If no success entry can be evicted the new entry is rejected: a rejected
    failure is counted against its failure category, a rejected success (the
    queue was full of failures) under the ``success`` category. Nothing ever
    blocks, so an HTTP response can never wait on the log target.

    The counters are queryable via :meth:`dropped_failures`,
    :attr:`lost_successes` and :meth:`dropped`.
    """

    def __init__(self, capacity: int) -> None:
        if capacity < 1:
            raise ValueError("The access log queue size must be a positive integer")
        self._capacity = capacity
        self._entries: deque[AccessLogEntry] = deque(maxlen=capacity)
        self._dropped_failures: dict[str, int] = {}
        # Success entries evicted because a failure entry needed the slot.
        self._evicted_successes = 0
        # All success entries not delivered (evicted or rejected).
        self._lost_successes = 0

    @property
    def capacity(self) -> int:
        return self._capacity

    @property
    def lost_successes(self) -> int:
        """Number of success entries not delivered (evicted or rejected)."""
        return self._lost_successes

    @property
    def evicted_successes(self) -> int:
        """Number of success entries evicted to make room for a failure."""
        return self._evicted_successes

    def dropped_failures(self) -> Mapping[str, int]:
        """Rejected failure entries keyed by their failure category."""
        return dict(self._dropped_failures)

    def dropped(self) -> Mapping[str, int]:
        """All discarded entries keyed by category.

        Lost success entries are grouped under the ``success`` key, rejected
        failure entries under their failure category.
        """
        dropped = dict(self._dropped_failures)
        if self._lost_successes:
            dropped["success"] = self._lost_successes
        return dropped

    def __len__(self) -> int:
        return len(self._entries)

    def snapshot(self) -> list[AccessLogEntry]:
        """Entries currently waiting, in delivery order."""
        return list(self._entries)

    def has_space(self) -> bool:
        return len(self._entries) < self._capacity

    def put(self, entry: AccessLogEntry) -> str:
        """Attempt to add ``entry`` without blocking.

        Returns :data:`QUEUED`, :data:`EVICTED` or :data:`REJECTED`.
        """
        if self.has_space():
            self._entries.append(entry)
            return QUEUED

        if not entry.is_failure:
            # A success entry never force-enters a full queue; it is dropped
            # so the caller never has to wait on the log target.
            self._lost_successes += 1
            return REJECTED

        for index, queued in enumerate(self._entries):
            if not queued.is_failure:
                del self._entries[index]
                self._evicted_successes += 1
                self._lost_successes += 1
                self._entries.append(entry)
                return EVICTED

        category = entry.failure_category
        self._dropped_failures[category] = self._dropped_failures.get(category, 0) + 1
        return REJECTED

    def pop(self) -> AccessLogEntry | None:
        """Remove and return the oldest entry, or ``None`` if empty."""
        if not self._entries:
            return None
        return self._entries.popleft()


class _ReusableEvent:
    """An asyncio-like event (set, clear, wait) usable on any backend."""

    def set(self) -> None:
        raise NotImplementedError()

    def clear(self) -> None:
        raise NotImplementedError()

    async def wait(self) -> None:
        raise NotImplementedError()


class _AsyncioReusableEvent(_ReusableEvent):
    def __init__(self) -> None:
        self._event = asyncio.Event()

    def set(self) -> None:
        self._event.set()

    def clear(self) -> None:
        self._event.clear()

    async def wait(self) -> None:
        await self._event.wait()


class _TrioReusableEvent(_ReusableEvent):
    """A repeatedly clearable event built on trio low level primitives.

    ``trio.Event`` can only be set once, which is insufficient for the
    producer/pump signalling here.
    """

    def __init__(self) -> None:
        import trio

        self._set = False
        self._tasks: set[trio.lowlevel.Task] = set()

    def set(self) -> None:
        self._set = True
        tasks, self._tasks = self._tasks, set()
        for task in tasks:
            trio.lowlevel.reschedule(task)

    def clear(self) -> None:
        self._set = False

    async def wait(self) -> None:
        import trio

        if self._set:
            return
        task = trio.lowlevel.current_task()
        self._tasks.add(task)

        def _abort(_: Any) -> Any:
            self._tasks.discard(task)
            return trio.lowlevel.Abort.FAILED

        await trio.lowlevel.wait_task_rescheduled(_abort)


class _DaemonThreadPoolExecutor(ThreadPoolExecutor):
    """A pool whose worker threads run as daemons.

    Daemon workers cannot keep the interpreter alive, so a write parked in a
    stalled sink never blocks process shutdown. A sink owns one of these and
    shuts it down in ``aclose`` once its queue has been fully drained.
    """

    def _adjust_thread_count(self) -> None:  # type: ignore[override]
        # Mirrors ThreadPoolExecutor._adjust_thread_count but marks each new
        # worker a daemon before it is started (the flag cannot change once the
        # thread is active).
        import weakref
        from concurrent.futures.thread import _threads_queues, _worker

        if self._idle_semaphore.acquire(timeout=0):
            return

        def _weakref_cb(_: Any, queue: Any = self._work_queue) -> None:
            queue.put(None)

        num_threads = len(self._threads)
        if num_threads < self._max_workers:
            thread_name = f"{self._thread_name_prefix or self}_{num_threads}"
            thread = threading.Thread(
                name=thread_name,
                target=_worker,
                args=(
                    weakref.ref(self, _weakref_cb),
                    self._work_queue,
                    self._initializer,
                    self._initargs,
                ),
            )
            thread.daemon = True
            thread.start()
            self._threads.add(thread)
            _threads_queues[thread] = self._work_queue


def _make_daemon_thread_executor(max_workers: int) -> ThreadPoolExecutor:
    return _DaemonThreadPoolExecutor(
        max_workers=max_workers,
        thread_name_prefix="hypercorn-access-log",
    )


class _AsyncBackend:
    """Small abstraction over the async primitives of the running library."""

    def new_event(self) -> _ReusableEvent:
        raise NotImplementedError()

    def new_one_shot_event(self) -> Any:
        """An event that is set at most once and never cleared."""
        raise NotImplementedError()

    def create_worker(self, func: Any) -> Any:
        """Start ``func`` (an async callable taking no arguments) as a task."""
        raise NotImplementedError()

    async def join_worker(self, worker: Any, finished: Any) -> None:
        """Block until the worker signals completion through ``finished``."""
        raise NotImplementedError()

    async def to_thread(self, func: Any, *args: Any, **kwargs: Any) -> Any:
        raise NotImplementedError()

    def shutdown(self) -> None:
        """Release any resources owned by the backend."""


class _AsyncioBackend(_AsyncBackend):
    def __init__(self) -> None:
        # One pool per sink. Two workers let the single sequential pump keep
        # moving while one write is parked in a slow sink. The pool is closed
        # in ``shutdown`` after the pump has drained everything.
        self._executor = _make_daemon_thread_executor(2)

    def new_event(self) -> _ReusableEvent:
        return _AsyncioReusableEvent()

    def new_one_shot_event(self) -> Any:
        return asyncio.Event()

    def create_worker(self, func: Any) -> asyncio.Task:
        return asyncio.get_event_loop().create_task(func())

    async def join_worker(self, worker: asyncio.Task, finished: Any) -> None:
        await finished.wait()

    async def to_thread(self, func: Any, *args: Any, **kwargs: Any) -> Any:
        loop = asyncio.get_event_loop()
        return await loop.run_in_executor(self._executor, lambda: func(*args, **kwargs))

    def shutdown(self) -> None:
        # By the time this runs the pump has drained the queue and every
        # submitted write has completed, but waiting here guarantees the worker
        # threads have fully exited and released every handler lock before the
        # interpreter reaches ``logging.shutdown``.
        self._executor.shutdown(wait=True)


class _TrioBackend(_AsyncBackend):
    def new_event(self) -> _ReusableEvent:
        return _TrioReusableEvent()

    def new_one_shot_event(self) -> Any:
        import trio

        return trio.Event()

    def create_worker(self, func: Any) -> Any:
        import trio

        return trio.lowlevel.spawn_system_task(func)

    async def join_worker(self, worker: Any, finished: Any) -> None:
        await finished.wait()

    async def to_thread(self, func: Any, *args: Any, **kwargs: Any) -> Any:
        import trio

        return await trio.to_thread.run_sync(lambda: func(*args, **kwargs))


def _select_backend() -> _AsyncBackend:
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        pass
    else:
        return _AsyncioBackend()

    try:
        import trio
    except ImportError:
        pass
    else:
        try:
            trio.lowlevel.current_task()
        except RuntimeError:
            pass
        else:
            return _TrioBackend()

    raise RuntimeError(
        "The queued access logger requires a running asyncio or trio event loop"
    )


class QueuedAccessLogSink:
    """Drains an :class:`AccessLogQueue` into a stdlib logger off the event loop.

    A single pump task writes one entry at a time, so the entry currently
    being written is outside the queue and can never be revoked. The actual
    (potentially blocking) handler call runs in a worker thread, meaning a
    stalled sink cannot block the event loop or the HTTP response.
    """

    def __init__(self, access_logger: logging.Logger, capacity: int) -> None:
        self._access_logger = access_logger
        self.queue = AccessLogQueue(capacity)
        self._backend: _AsyncBackend | None = None
        self._worker: Any = None
        self._has_entries: Any = None
        self._finished: Any = None
        self._closed = False

    def _ensure_started(self) -> _AsyncBackend:
        if self._backend is None:
            backend = _select_backend()
            self._backend = backend
            self._has_entries = backend.new_event()
            self._finished = backend.new_one_shot_event()
            self._worker = backend.create_worker(self._pump)
        return self._backend

    async def submit(self, entry: AccessLogEntry) -> None:
        """Enqueue ``entry`` without ever blocking on the log target."""
        self._ensure_started()
        result = self.queue.put(entry)
        if result is not REJECTED:
            self._has_entries.set()

    async def _pump(self) -> None:
        assert self._backend is not None
        backend = self._backend
        try:
            while True:
                entry = self.queue.pop()
                if entry is None:
                    if self._closed:
                        return
                    self._has_entries.clear()
                    # Re-check after clearing: an entry submitted between the
                    # pop and the clear set the event again and must not be
                    # missed.
                    if len(self.queue):
                        continue
                    await self._has_entries.wait()
                    continue

                try:
                    await backend.to_thread(self._access_logger.info, entry.message)
                except Exception:
                    logger.warning("Failed to write an access log entry", exc_info=True)
        finally:
            self._finished.set()

    async def aclose(self) -> None:
        """Flush every queued entry in order and stop the pump.

        The pump remains the sole writer: closing only stops it from blocking
        on an empty queue and lets it run until the queue is drained, so no two
        writes ever compete for the same handler lock.
        """
        backend = self._backend
        if backend is None:
            return
        self._closed = True
        self._has_entries.set()
        assert self._worker is not None
        await backend.join_worker(self._worker, self._finished)
        backend.shutdown()


class Logger:
    def __init__(self, config: Config) -> None:
        self.access_log_format = config.access_log_format

        self.access_logger = _create_logger(
            "hypercorn.access",
            config.accesslog,
            config.loglevel,
            sys.stdout,
            propagate=False,
        )
        self.error_logger = _create_logger(
            "hypercorn.error", config.errorlog, config.loglevel, sys.stderr
        )

        self.access_log_sink: QueuedAccessLogSink | None = None
        if self.access_logger is not None and config.access_log_queue_size is not None:
            if config.access_log_queue_size < 1:
                raise ValueError("The access log queue size must be a positive integer")
            self.access_log_sink = QueuedAccessLogSink(
                self.access_logger, config.access_log_queue_size
            )

        if config.logconfig is not None:
            if config.logconfig.startswith("json:"):
                with open(config.logconfig[5:]) as file_:
                    dictConfig(json.load(file_))
            elif config.logconfig.startswith("toml:"):
                with open(config.logconfig[5:], "rb") as file_:
                    dictConfig(tomllib.load(file_))
            else:
                log_config = {
                    "__file__": config.logconfig,
                    "here": os.path.dirname(config.logconfig),
                }
                fileConfig(config.logconfig, defaults=log_config, disable_existing_loggers=False)
        else:
            if config.logconfig_dict is not None:
                dictConfig(config.logconfig_dict)

    async def access(
        self, request: WWWScope, response: ResponseSummary | None, request_time: float
    ) -> None:
        if self.access_logger is None:
            return

        if self.access_log_sink is None:
            # Synchronous configuration: the handler is awaited inline as before.
            self.access_logger.info(
                self.access_log_format, self.atoms(request, response, request_time)
            )
            return

        # Queued configuration: rendering happens here, the write is drained
        # by the sink so the HTTP response never waits on the log target.
        message = self.access_log_format % self.atoms(request, response, request_time)
        status = None if response is None else int(response["status"])
        await self.access_log_sink.submit(AccessLogEntry(message, status))

    @property
    def access_log_queue(self) -> AccessLogQueue | None:
        """The bounded access log queue, or ``None`` in synchronous mode."""
        return None if self.access_log_sink is None else self.access_log_sink.queue

    def access_log_dropped(self) -> Mapping[str, int]:
        """Discarded access log entries keyed by category (empty if not queued)."""
        if self.access_log_sink is None:
            return {}
        return self.access_log_sink.queue.dropped()

    async def aclose(self) -> None:
        """Flush and stop the queued access log sink, if configured."""
        if self.access_log_sink is not None:
            await self.access_log_sink.aclose()

    async def critical(self, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.critical(message, *args, **kwargs)

    async def error(self, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.error(message, *args, **kwargs)

    async def warning(self, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.warning(message, *args, **kwargs)

    async def info(self, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.info(message, *args, **kwargs)

    async def debug(self, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.debug(message, *args, **kwargs)

    async def exception(self, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.exception(message, *args, **kwargs)

    async def log(self, level: int, message: str, *args: Any, **kwargs: Any) -> None:
        if self.error_logger is not None:
            self.error_logger.log(level, message, *args, **kwargs)

    def atoms(
        self, request: WWWScope, response: ResponseSummary | None, request_time: float
    ) -> Mapping[str, str]:
        """Create and return an access log atoms dictionary.

        This can be overidden and customised if desired. It should
        return a mapping between an access log format key and a value.
        """
        return AccessLogAtoms(request, response, request_time)

    def __getattr__(self, name: str) -> Any:
        return getattr(self.error_logger, name)


class AccessLogAtoms(dict):
    def __init__(
        self, request: WWWScope, response: ResponseSummary | None, request_time: float
    ) -> None:
        for name, value in request["headers"]:
            self[f"{{{name.decode('latin1').lower()}}}i"] = value.decode("latin1")
        for name, value in os.environ.items():
            self[f"{{{name.lower()}}}e"] = value
        protocol = request.get("http_version", "ws")
        client = request.get("client")
        if client is None:
            remote_addr = None
        elif len(client) == 2:
            remote_addr = f"{client[0]}:{client[1]}"
        elif len(client) == 1:
            remote_addr = client[0]
        else:  # make sure not to throw UnboundLocalError
            remote_addr = f"<???{client}???>"
        if request["type"] == "http":
            method = request["method"]
        else:
            method = "GET"
        query_string = request["query_string"].decode()
        path_with_qs = request["path"] + ("?" + query_string if query_string else "")

        status_code = "-"
        status_phrase = "-"
        if response is not None:
            for name, value in response.get("headers", []):  # type: ignore
                self[f"{{{name.decode('latin1').lower()}}}o"] = value.decode("latin1")  # type: ignore # noqa: E501
            status_code = str(response["status"])
            try:
                status_phrase = HTTPStatus(response["status"]).phrase
            except ValueError:
                status_phrase = f"<???{status_code}???>"
        self.update(
            {
                "h": remote_addr,
                "l": "-",
                "t": time.strftime("[%d/%b/%Y:%H:%M:%S %z]"),
                "r": f"{method} {request['path']} {protocol}",
                "R": f"{method} {path_with_qs} {protocol}",
                "s": status_code,
                "st": status_phrase,
                "S": request["scheme"],
                "m": method,
                "U": request["path"],
                "Uq": path_with_qs,
                "q": query_string,
                "H": protocol,
                "b": self["{Content-Length}o"],
                "B": self["{Content-Length}o"],
                "f": self["{Referer}i"],
                "a": self["{User-Agent}i"],
                "T": int(request_time),
                "D": int(request_time * 1_000_000),
                "L": f"{request_time:.6f}",
                "p": f"<{os.getpid()}>",
            }
        )

    def __getitem__(self, key: str) -> str:
        try:
            if key.startswith("{"):
                return super().__getitem__(key.lower())
            else:
                return super().__getitem__(key)
        except KeyError:
            return "-"
