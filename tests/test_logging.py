from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
from typing import Any

import pytest

from hypercorn.config import Config
from hypercorn.logging import (
    AccessLogEntry,
    AccessLogQueue,
    EVICTED,
    Logger,
    QUEUED,
    REJECTED,
    AccessLogAtoms,
)
from hypercorn.typing import HTTPScope, ResponseSummary


def _entry(message: str, status: int | None) -> AccessLogEntry:
    return AccessLogEntry(message=message, status=status)


def _success(message: str) -> AccessLogEntry:
    return _entry(message, 200)


def _failure(message: str, status: int = 500) -> AccessLogEntry:
    return _entry(message, status)


async def _nap() -> None:
    """Yield to whichever event loop (asyncio or trio) is running."""
    try:
        import trio

        trio.lowlevel.current_task()
    except RuntimeError:
        await asyncio.sleep(0.001)
    else:
        await trio.sleep(0.001)


# Every blocking handler created during a test, so an autouse fixture can open
# their gates on teardown as a safety net.
_blocking_handlers: list["BlockingHandler"] = []


class BlockingHandler(logging.Handler):
    """Handler that can hold a write on a gate to simulate a stalled sink.

    ``emit`` blocks on :attr:`_gate` until the test opens it. Each test drives
    the sink to completion with ``Logger.aclose`` (which awaits the pump and
    joins the worker pool), so by teardown every write has finished and released
    the handler lock; the autouse fixture only guarantees the gate is open even
    if an assertion fails beforehand.
    """

    def __init__(self) -> None:
        super().__init__()
        self._gate = threading.Event()
        self._started = threading.Event()
        self.written: list[str] = []
        self._in_flight: str | None = None
        _blocking_handlers.append(self)

    def block(self) -> None:
        self._gate.clear()

    def open_gate(self) -> None:
        self._gate.set()

    def emit(self, record: logging.LogRecord) -> None:
        message = self.format(record)
        self._in_flight = message
        self._started.set()
        # Block until the test opens the gate.
        self._gate.wait()
        self.written.append(message)

    async def wait_in_flight(self) -> str:
        """Wait (yielding to the loop) for a write to reach the handler."""
        while not self._started.is_set():
            await _nap()
        assert self._in_flight is not None
        return self._in_flight


@pytest.fixture(autouse=True)
def _release_all_blocking_handlers() -> Any:
    yield
    # Safety net: make sure no gate is left closed if a test failed early.
    for handler in _blocking_handlers:
        handler.open_gate()
    _blocking_handlers.clear()


@pytest.mark.parametrize(
    "target, expected_name, expected_handler_type",
    [
        ("-", "hypercorn.access", logging.StreamHandler),
        ("/tmp/path", "hypercorn.access", logging.FileHandler),
        (logging.getLogger("test_special"), "test_special", None),
        (None, None, None),
    ],
)
def test_access_logger_init(
    target: logging.Logger | str | None,
    expected_name: str | None,
    expected_handler_type: type[logging.Handler] | None,
) -> None:
    config = Config()
    config.accesslog = target
    config.access_log_format = "%h"
    logger = Logger(config)
    assert logger.access_log_format == "%h"
    assert logger.getEffectiveLevel() == logging.INFO
    if target is None:
        assert logger.access_logger is None
    elif expected_name is None:
        assert logger.access_logger.handlers == []
    else:
        assert logger.access_logger.name == expected_name
        if expected_handler_type is None:
            assert logger.access_logger.handlers == []
        else:
            assert isinstance(logger.access_logger.handlers[0], expected_handler_type)


@pytest.mark.parametrize(
    "level, expected",
    [
        (logging.getLevelName(level_name), level_name)
        for level_name in range(logging.DEBUG, logging.CRITICAL + 1, 10)
    ],
)
def test_loglevel_option(level: str | None, expected: int) -> None:
    config = Config()
    config.loglevel = level
    logger = Logger(config)
    assert logger.error_logger.getEffectiveLevel() == expected


@pytest.fixture(name="response")
def _response_scope() -> dict:
    return {"status": 200, "headers": [(b"Content-Length", b"5"), (b"X-Hypercorn", b"Hypercorn")]}


def test_access_log_standard_atoms(http_scope: HTTPScope, response: ResponseSummary) -> None:
    atoms = AccessLogAtoms(http_scope, response, 0.000_023)
    assert atoms["h"] == "127.0.0.1:80"
    assert atoms["l"] == "-"
    assert time.strptime(atoms["t"], "[%d/%b/%Y:%H:%M:%S %z]")
    assert int(atoms["s"]) == 200
    assert atoms["m"] == "GET"
    assert atoms["U"] == "/"
    assert atoms["q"] == "a=b"
    assert atoms["H"] == "2"
    assert int(atoms["b"]) == 5
    assert int(atoms["B"]) == 5
    assert atoms["f"] == "hypercorn"
    assert atoms["a"] == "Hypercorn"
    assert atoms["p"] == f"<{os.getpid()}>"
    assert atoms["not-atom"] == "-"
    assert int(atoms["T"]) == 0
    assert int(atoms["D"]) == 23
    assert atoms["L"] == "0.000023"
    assert atoms["r"] == "GET / 2"
    assert atoms["R"] == "GET /?a=b 2"
    assert atoms["Uq"] == "/?a=b"
    assert atoms["st"] == "OK"


def test_access_log_header_atoms(http_scope: HTTPScope, response: ResponseSummary) -> None:
    atoms = AccessLogAtoms(http_scope, response, 0)
    assert atoms["{X-Hypercorn}i"] == "Hypercorn"
    assert atoms["{X-HYPERCORN}i"] == "Hypercorn"
    assert atoms["{not-atom}i"] == "-"
    assert atoms["{X-Hypercorn}o"] == "Hypercorn"
    assert atoms["{X-HYPERCORN}o"] == "Hypercorn"


def test_access_no_log_header_atoms(http_scope: HTTPScope) -> None:
    atoms = AccessLogAtoms(http_scope, {"status": 200, "headers": []}, 0)
    assert atoms["{X-Hypercorn}i"] == "Hypercorn"
    assert atoms["{X-HYPERCORN}i"] == "Hypercorn"
    assert atoms["{not-atom}i"] == "-"
    assert not any(key.startswith("{") and key.endswith("}o") for key in atoms.keys())


def test_access_log_environ_atoms(http_scope: HTTPScope, response: ResponseSummary) -> None:
    os.environ["Random"] = "Environ"
    atoms = AccessLogAtoms(http_scope, response, 0)
    assert atoms["{random}e"] == "Environ"


def test_nonstandard_status_code(http_scope: HTTPScope) -> None:
    atoms = AccessLogAtoms(http_scope, {"status": 441, "headers": []}, 0)
    assert atoms["st"] == "<???441???>"


# ---------------------------------------------------------------------------
# AccessLogQueue policy (synchronous, no event loop required)
# ---------------------------------------------------------------------------


def test_queue_requires_positive_capacity() -> None:
    with pytest.raises(ValueError):
        AccessLogQueue(0)


def test_queue_never_exceeds_capacity_and_keeps_fifo_order() -> None:
    queue = AccessLogQueue(3)
    for index in range(3):
        assert queue.put(_success(f"s{index}")) == QUEUED
    assert len(queue) == 3

    # A failure evicts the oldest success, then takes its place at the tail.
    assert queue.put(_failure("f0")) == EVICTED
    assert len(queue) == queue.capacity
    assert [entry.message for entry in queue.snapshot()] == ["s1", "s2", "f0"]

    assert queue.put(_failure("f1", 404)) == EVICTED
    assert [entry.message for entry in queue.snapshot()] == ["s2", "f0", "f1"]

    # The last success is evicted by the next failure; now only failures are
    # retained, still in arrival order.
    assert queue.put(_failure("f2", 404)) == EVICTED
    assert [entry.message for entry in queue.snapshot()] == ["f0", "f1", "f2"]

    # Only failures remain: further failures are rejected and counted per
    # category, never growing the queue past its capacity.
    assert queue.put(_failure("f3", 404)) == REJECTED
    assert queue.put(_failure("f4", 500)) == REJECTED
    assert len(queue) == queue.capacity
    assert queue.dropped_failures() == {"404": 1, "500": 1}
    assert queue.evicted_successes == 3
    assert queue.lost_successes == 3
    assert queue.dropped() == {"404": 1, "500": 1, "success": 3}


def test_queue_success_cannot_evict() -> None:
    queue = AccessLogQueue(2)
    queue.put(_failure("f0", 500))
    queue.put(_failure("f1", 500))
    # A success entry never force-enters a full queue; it is dropped rather
    # than blocking the producer.
    assert queue.put(_success("s0")) == REJECTED
    assert [entry.message for entry in queue.snapshot()] == ["f0", "f1"]
    # The rejected success is counted separately from dropped failures.
    assert queue.dropped_failures() == {}
    assert queue.lost_successes == 1
    assert queue.dropped() == {"success": 1}


def test_queue_pop_returns_entries_in_order() -> None:
    queue = AccessLogQueue(2)
    queue.put(_success("s0"))
    queue.put(_failure("f0", 503))
    assert queue.pop().message == "s0"  # type: ignore[union-attr]
    assert queue.pop().message == "f0"  # type: ignore[union-attr]
    assert queue.pop() is None


def test_queue_disconnect_category() -> None:
    queue = AccessLogQueue(1)
    queue.put(_failure("f0", 500))
    disconnected = _entry("d0", None)
    assert disconnected.is_failure
    assert disconnected.failure_category == "disconnect"
    assert queue.put(disconnected) == REJECTED
    assert queue.dropped_failures() == {"disconnect": 1}


def test_entry_failure_classification() -> None:
    assert _entry("ok", 200).is_failure is False
    assert _entry("redirect", 301).is_failure is False
    assert _entry("bad", 404).is_failure is True
    assert _entry("boom", 500).is_failure is True
    assert _entry("gone", None).is_failure is True


# ---------------------------------------------------------------------------
# QueuedAccessLogSink / Logger integration (asyncio)
# ---------------------------------------------------------------------------


def _queued_logger(queue_size: int, handler: logging.Handler) -> Logger:
    access_logger = logging.getLogger(f"hypercorn.access.test.{id(handler)}")
    access_logger.handlers = [handler]
    access_logger.propagate = False
    access_logger.setLevel(logging.INFO)
    handler.setFormatter(logging.Formatter("%(message)s"))

    config = Config()
    config.accesslog = access_logger
    config.access_log_queue_size = queue_size
    config.access_log_format = "%(U)s"
    return Logger(config)


def _scope(path: str) -> HTTPScope:
    return {
        "type": "http",
        "asgi": {},
        "http_version": "1.1",
        "method": "GET",
        "scheme": "http",
        "path": path,
        "raw_path": path.encode(),
        "query_string": b"",
        "root_path": "",
        "headers": [],
        "client": None,
        "server": None,
        "extensions": {},
        "state": {},
    }


async def _log(logger_instance: Logger, path: str, status: int | None) -> None:
    response = None if status is None else {"status": status, "headers": []}
    await logger_instance.access(_scope(path), response, 0.0)


@pytest.mark.asyncio
async def test_queued_logger_evicts_success_rejects_failures_and_preserves_order() -> None:
    handler = BlockingHandler()
    handler.block()
    logger_instance = _queued_logger(3, handler)

    # Lock one entry as the in-flight write; it is outside the queue and so
    # can never be revoked.
    await _log(logger_instance, "/in-flight", 200)
    assert await handler.wait_in_flight() == "/in-flight"

    # Interleave success and failure requests while the sink cannot drain.
    # Every call returns immediately, so responses never wait on the sink.
    await _log(logger_instance, "/s1", 200)  # queued
    await _log(logger_instance, "/s2", 200)  # queued
    await _log(logger_instance, "/s3", 200)  # queued -> [s1, s2, s3]
    await _log(logger_instance, "/f1", 404)  # evicts s1 -> [s2, s3, f1]
    await _log(logger_instance, "/f2", 500)  # evicts s2 -> [s3, f1, f2]
    await _log(logger_instance, "/f3", 404)  # evicts s3 -> [f1, f2, f3]
    await _log(logger_instance, "/f4", 404)  # rejected (only failures queued)
    await _log(logger_instance, "/f5", 500)  # rejected (only failures queued)
    await _log(logger_instance, "/s4", 200)  # rejected success (full of failures)

    queue = logger_instance.access_log_queue
    assert queue is not None
    # Only failure entries remain waiting, in arrival order.
    assert [entry.message for entry in queue.snapshot()] == ["/f1", "/f2", "/f3"]
    assert len(queue) == queue.capacity
    # Failures are counted per category; lost successes are tracked separately.
    # Only the two entries refused once the queue held solely failures count.
    assert queue.dropped_failures() == {"404": 1, "500": 1}
    assert queue.evicted_successes == 3  # s1, s2, s3 evicted
    assert queue.lost_successes == 4  # ...plus s4 rejected
    assert logger_instance.access_log_dropped() == {
        "404": 1,
        "500": 1,
        "success": 4,
    }
    # The in-flight entry cannot be revoked and has not been written yet.
    assert handler.written == []

    # Resume the sink.
    handler.open_gate()
    await logger_instance.aclose()

    # The in-flight entry is written first, then the retained failures, all in
    # arrival order. No evicted or rejected success is ever written.
    assert handler.written == ["/in-flight", "/f1", "/f2", "/f3"]
    assert not {"/s1", "/s2", "/s3", "/s4"} & set(handler.written)


@pytest.mark.asyncio
async def test_queued_logger_response_does_not_wait_for_sink() -> None:
    handler = BlockingHandler()
    handler.block()
    logger_instance = _queued_logger(1, handler)

    await _log(logger_instance, "/first", 200)
    assert await handler.wait_in_flight() == "/first"

    # The queue holds one entry and the sink is stalled; a failure entry must
    # still return promptly (it is accepted by evicting nothing, then later
    # rejected), proving the HTTP response path is not blocked on the sink.
    start = time.perf_counter()
    await _log(logger_instance, "/queued", 500)
    await _log(logger_instance, "/rejected", 500)
    assert time.perf_counter() - start < 0.5

    assert logger_instance.access_log_dropped() == {"500": 1}
    handler.open_gate()
    await logger_instance.aclose()
    assert handler.written == ["/first", "/queued"]


@pytest.mark.asyncio
async def test_queued_logger_success_dropped_when_queue_holds_only_failures() -> None:
    handler = BlockingHandler()
    handler.block()
    logger_instance = _queued_logger(1, handler)

    await _log(logger_instance, "/in-flight", 200)
    await handler.wait_in_flight()
    await _log(logger_instance, "/failure", 500)  # fills the queue

    # A success when the queue contains only failures is dropped immediately;
    # the response never waits on the stalled sink.
    start = time.perf_counter()
    await _log(logger_instance, "/dropped-success", 200)
    assert time.perf_counter() - start < 0.5

    assert logger_instance.access_log_dropped() == {"success": 1}
    handler.open_gate()
    await logger_instance.aclose()
    assert handler.written == ["/in-flight", "/failure"]
    assert "/dropped-success" not in handler.written


@pytest.mark.asyncio
async def test_queued_logger_capacity_never_exceeded_under_load() -> None:
    handler = BlockingHandler()
    handler.block()
    capacity = 3
    logger_instance = _queued_logger(capacity, handler)

    await _log(logger_instance, "/in-flight", 200)
    await handler.wait_in_flight()

    for index in range(20):
        status = 200 if index % 2 == 0 else 500
        await _log(logger_instance, f"/r{index}", status)
        queue = logger_instance.access_log_queue
        assert queue is not None
        assert len(queue) <= capacity

    # Every submission returned even though the sink never drained.
    handler.open_gate()
    await logger_instance.aclose()

    # Every written entry is distinct and the queue never overflowed.
    assert len(handler.written) == len(set(handler.written))
    assert handler.written[0] == "/in-flight"
    assert len(handler.written) <= capacity + 1
    # Everything lost is accounted for in the queryable counters.
    accounted = sum(logger_instance.access_log_dropped().values()) + len(handler.written)
    assert accounted == 21


@pytest.mark.asyncio
async def test_queued_logger_disconnect_counted_under_disconnect_category() -> None:
    handler = BlockingHandler()
    handler.block()
    logger_instance = _queued_logger(1, handler)

    await _log(logger_instance, "/in-flight", 200)
    await handler.wait_in_flight()
    await _log(logger_instance, "/disconnected", None)
    await _log(logger_instance, "/disconnected-2", None)  # rejected

    assert logger_instance.access_log_dropped() == {"disconnect": 1}
    handler.open_gate()
    await logger_instance.aclose()
    assert handler.written == ["/in-flight", "/disconnected"]


class RecordingHandler(logging.Handler):
    """Non-blocking handler that records formatted messages."""

    def __init__(self) -> None:
        super().__init__()
        self.written: list[str] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.written.append(self.format(record))


def test_synchronous_configuration_is_unchanged() -> None:
    config = Config()
    assert config.access_log_queue_size is None

    handler = RecordingHandler()
    logger_instance = _queued_logger_without_queue(handler)
    assert logger_instance.access_log_sink is None
    assert logger_instance.access_log_queue is None
    assert logger_instance.access_log_dropped() == {}


def _queued_logger_without_queue(handler: logging.Handler) -> Logger:
    access_logger = logging.getLogger(f"hypercorn.access.test.sync.{id(handler)}")
    access_logger.handlers = [handler]
    access_logger.propagate = False
    access_logger.setLevel(logging.INFO)
    handler.setFormatter(logging.Formatter("%(message)s"))

    config = Config()
    config.accesslog = access_logger
    config.access_log_format = "%(U)s"
    return Logger(config)


@pytest.mark.asyncio
async def test_synchronous_access_log_still_writes_inline() -> None:
    handler = RecordingHandler()
    logger_instance = _queued_logger_without_queue(handler)

    # In synchronous mode the write is performed inline rather than handed to
    # a background pump.
    await _log(logger_instance, "/inline", 200)
    assert handler.written == ["/inline"]
    assert logger_instance.access_log_sink is None
    # Nothing to flush; closing is a no-op in synchronous mode.
    await logger_instance.aclose()


def test_invalid_queue_size_raises() -> None:
    config = Config()
    config.accesslog = logging.getLogger("hypercorn.access.invalid")
    config.access_log_queue_size = 0
    with pytest.raises(ValueError):
        Logger(config)


@pytest.mark.asyncio
async def test_no_access_logger_is_a_noop() -> None:
    config = Config()
    config.accesslog = None
    config.access_log_queue_size = 5
    logger_instance = Logger(config)
    assert logger_instance.access_log_sink is None
    await logger_instance.access(_scope("/x"), {"status": 200, "headers": []}, 0.0)
    assert logger_instance.access_log_dropped() == {}
    await logger_instance.aclose()


@pytest.mark.trio
async def test_queued_logger_works_on_trio() -> None:
    import trio

    capacity = 3
    handler = BlockingHandler()
    handler.block()
    logger_instance = _queued_logger(capacity, handler)

    with trio.fail_after(5):
        # Hold one entry in-flight so the pump cannot drain the queue while we
        # saturate it, regardless of the runtime's task scheduling.
        await _log(logger_instance, "/in-flight", 200)
        await handler.wait_in_flight()

        # Submit far more entries than the queue can hold; every call must
        # return promptly (the response never waits on the stalled sink).
        for index in range(capacity + 5):
            await _log(logger_instance, f"/s{index}", 200)
        for index in range(capacity + 5):
            await _log(logger_instance, f"/f{index}", 500)

        queue = logger_instance.access_log_queue
        assert queue is not None
        # The in-flight entry lives outside the queue, which never overflows.
        assert len(queue) <= capacity
        # Counters account for every entry that was not retained.
        retained_messages = [entry.message for entry in queue.snapshot()]
        retained = len(retained_messages)
        dropped = sum(queue.dropped().values())
        assert retained + dropped == 2 * (capacity + 5)

        handler.open_gate()
        with trio.CancelScope(shield=True):
            await logger_instance.aclose()

    # Delivery is strictly ordered: the in-flight entry first, then the
    # retained queue entries in their original arrival order.
    assert handler.written[0] == "/in-flight"
    assert handler.written[1:] == retained_messages
    assert len(handler.written) == retained + 1
