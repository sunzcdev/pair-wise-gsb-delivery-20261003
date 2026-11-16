from __future__ import annotations

import queue
import threading
import time
import typing

import anyio
import pytest

import httpx

# -----------------------------------------------------------------------------
# Test helpers
# -----------------------------------------------------------------------------


class Recording:
    """
    Track how many requests to each origin are in flight.

    A request is counted as in flight from the moment the transport accepts it
    until its response body has been fully consumed or closed.
    """

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.in_flight: dict[str, int] = {}
        self.peak: dict[str, int] = {}
        self.started: list[str] = []

    def enter(self, host: str) -> None:
        with self.lock:
            self.in_flight[host] = self.in_flight.get(host, 0) + 1
            self.peak[host] = max(self.peak.get(host, 0), self.in_flight[host])
            self.started.append(host)

    def leave(self, host: str) -> None:
        with self.lock:
            self.in_flight[host] -= 1


class SyncBody(httpx.SyncByteStream):
    """A streamed response body that settles its recording when finished."""

    def __init__(
        self,
        recording: Recording | None = None,
        host: str = "",
        chunks: typing.Iterable[bytes] = (b"a", b"b"),
    ) -> None:
        self._chunks = list(chunks)
        self._recording = recording
        self._host = host
        self.closed = False

    def __iter__(self) -> typing.Iterator[bytes]:
        try:
            yield from self._chunks
        finally:
            self._settle()

    def close(self) -> None:
        self.closed = True
        self._settle()

    def _settle(self) -> None:
        if self._recording is not None:
            self._recording.leave(self._host)
            self._recording = None


class AsyncBody(httpx.AsyncByteStream):
    """A streamed response body that settles its recording when finished."""

    def __init__(
        self,
        recording: Recording | None = None,
        host: str = "",
        chunks: typing.Iterable[bytes] = (b"a", b"b"),
    ) -> None:
        self._chunks = list(chunks)
        self._recording = recording
        self._host = host
        self.closed = False

    async def __aiter__(self) -> typing.AsyncIterator[bytes]:
        try:
            for chunk in self._chunks:
                yield chunk
        finally:
            self._settle()

    async def aclose(self) -> None:
        self.closed = True
        self._settle()

    def _settle(self) -> None:
        if self._recording is not None:
            self._recording.leave(self._host)
            self._recording = None


class GatedSyncTransport(httpx.BaseTransport):
    """
    A transport that blocks each origin on its own gate.

    Requests are reported to the recording just before they block, and their
    in-flight count only drops once the response body has been consumed or the
    response has been closed.
    """

    def __init__(
        self,
        recording: Recording,
        gates: typing.Mapping[str, threading.Event] | None = None,
    ) -> None:
        self.recording = recording
        self.gates = gates or {}
        self.bodies: list[SyncBody] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        host = request.url.host
        self.recording.enter(host)
        gate = self.gates.get(host)
        try:
            if gate is not None:
                gate.wait(timeout=5)
        except BaseException:  # pragma: no cover - defensive
            self.recording.leave(host)
            raise
        body = SyncBody(self.recording, host)
        self.bodies.append(body)
        return httpx.Response(200, stream=body)


class GatedAsyncTransport(httpx.AsyncBaseTransport):
    """The async equivalent of `GatedSyncTransport`."""

    def __init__(
        self,
        recording: Recording,
        gates: typing.Mapping[str, anyio.Event] | None = None,
    ) -> None:
        self.recording = recording
        self.gates = gates or {}
        self.bodies: list[AsyncBody] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        host = request.url.host
        self.recording.enter(host)
        gate = self.gates.get(host)
        try:
            if gate is not None:
                await gate.wait()
        except BaseException:  # pragma: no cover - defensive
            self.recording.leave(host)
            raise
        body = AsyncBody(self.recording, host)
        self.bodies.append(body)
        return httpx.Response(200, stream=body)


def _origin_state(client: typing.Any, host: str) -> typing.Any:
    origin = ("http", host, 80)
    limiter = client._origin_limiter
    assert limiter is not None
    with limiter._lock:
        return limiter._states.get(origin)


def _wait_for_waiters(client: typing.Any, host: str, count: int) -> None:
    for _ in range(500):
        state = _origin_state(client, host)
        if state is not None and len(state.waiters) >= count:
            return
        time.sleep(0.01)
    raise AssertionError(f"Requests for {host} never started waiting.")


async def _await_waiters(client: typing.Any, host: str, count: int) -> None:
    for _ in range(500):
        state = _origin_state(client, host)
        if state is not None and len(state.waiters) >= count:
            return
        await anyio.sleep(0.01)
    raise AssertionError(f"Requests for {host} never started waiting.")


def _wait_for_limiter_waiters(limiter: typing.Any, count: int) -> None:
    for _ in range(500):
        with limiter._lock:
            queued = sum(len(state.waiters) for state in limiter._states.values())
        if queued >= count:
            return
        time.sleep(0.01)
    raise AssertionError("The waiter never started waiting.")


# -----------------------------------------------------------------------------
# Sync client
# -----------------------------------------------------------------------------


def test_sync_per_origin_limit_caps_in_flight_requests() -> None:
    recording = Recording()
    gate = threading.Event()
    transport = GatedSyncTransport(recording, {"example.com": gate})
    limits = httpx.Limits(max_connections_per_origin=2)

    with httpx.Client(transport=transport, limits=limits) as client:
        threads = [
            threading.Thread(target=client.get, args=("http://example.com/",))
            for _ in range(6)
        ]
        for thread in threads:
            thread.start()

        # Exactly two requests may be admitted, and no more, while the origin
        # is blocked. Wait for saturation rather than relying on a fixed delay,
        # so that this is robust on a loaded machine.
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            with recording.lock:
                started = len(recording.started)
            if started >= 2:
                break
            time.sleep(0.01)
        time.sleep(0.05)
        assert recording.started == ["example.com", "example.com"]

        gate.set()
        for thread in threads:
            thread.join(timeout=5)
            assert not thread.is_alive()

    with recording.lock:
        assert recording.peak["example.com"] == 2
        assert recording.in_flight["example.com"] == 0


def test_sync_other_origins_progress_while_one_origin_is_queued() -> None:
    recording = Recording()
    slow_gate = threading.Event()
    other_gate = threading.Event()
    transport = GatedSyncTransport(
        recording, {"slow.example": slow_gate, "other.example": other_gate}
    )
    limits = httpx.Limits(max_connections_per_origin=1)

    with httpx.Client(transport=transport, limits=limits) as client:
        threads = [
            threading.Thread(target=client.get, args=("http://slow.example/",))
            for _ in range(3)
        ]
        for thread in threads:
            thread.start()

        for _ in range(300):
            if len(recording.started) == 1:
                break
            time.sleep(0.01)
        assert recording.started == ["slow.example"]

        other_started = threading.Event()

        def request_other_origin() -> None:
            try:
                client.get("http://other.example/")
            finally:
                other_started.set()

        other = threading.Thread(target=request_other_origin)
        other.start()

        # The other origin gets its own permit, even though requests for the
        # busy origin are still queued.
        for _ in range(300):
            if "other.example" in recording.started:
                break
            time.sleep(0.01)
        assert "other.example" in recording.started

        # Release the other origin first; it completes while the busy origin is
        # still blocking and queued.
        other_gate.set()
        assert other_started.wait(timeout=5)
        assert not slow_gate.is_set()

        slow_gate.set()
        for thread in threads:
            thread.join(timeout=5)

    with recording.lock:
        assert recording.peak["slow.example"] == 1
        assert recording.peak["other.example"] == 1


def test_sync_same_origin_admitted_in_arrival_order() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    admission_order: queue.Queue[str] = queue.Queue()

    class Transport(httpx.BaseTransport):
        def handle_request(self, request: httpx.Request) -> httpx.Response:
            admission_order.put(request.url.path)
            return httpx.Response(200, stream=SyncBody(chunks=()))

    with httpx.Client(transport=Transport(), limits=limits) as client:
        paths = [f"/{index}" for index in range(6)]
        threads = [
            threading.Thread(target=client.get, args=(f"http://example.com{path}",))
            for path in paths
        ]
        # Start the threads in order, so that arrival order is deterministic.
        for thread in threads:
            thread.start()
            time.sleep(0.03)
        for thread in threads:
            thread.join(timeout=5)

    seen = []
    while not admission_order.empty():
        seen.append(admission_order.get())
    assert seen == paths


def test_sync_permit_released_when_response_is_closed_early() -> None:
    recording = Recording()
    limits = httpx.Limits(max_connections_per_origin=1)
    bodies: list[SyncBody] = []

    class Transport(httpx.BaseTransport):
        def handle_request(self, request: httpx.Request) -> httpx.Response:
            recording.enter("example.com")
            body = SyncBody(recording, "example.com")
            bodies.append(body)
            return httpx.Response(200, stream=body)

    with httpx.Client(transport=Transport(), limits=limits) as client:
        # A streamed response is closed without consuming the body.
        with client.stream("GET", "http://example.com/first") as response:
            assert response.status_code == 200

        # The permit must be available again, so a second request is admitted.
        second = client.get("http://example.com/second")
        assert second.status_code == 200

    assert bodies[0].closed


def test_sync_permit_released_when_body_is_exhausted() -> None:
    recording = Recording()
    limits = httpx.Limits(max_connections_per_origin=1)

    class Transport(httpx.BaseTransport):
        def handle_request(self, request: httpx.Request) -> httpx.Response:
            recording.enter("example.com")
            return httpx.Response(
                200, stream=SyncBody(recording, "example.com", chunks=(b"hi",))
            )

    with httpx.Client(transport=Transport(), limits=limits) as client:
        # Reading the body to completion returns the permit, without needing an
        # explicit close.
        assert client.get("http://example.com/one").content == b"hi"
        assert client.get("http://example.com/two").content == b"hi"

    with recording.lock:
        assert recording.in_flight["example.com"] == 0


def test_sync_permit_released_when_transport_fails() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if calls == 1:
            raise httpx.ConnectError("failure", request=request)
        return httpx.Response(200, content=b"ok")

    with httpx.Client(transport=httpx.MockTransport(handler), limits=limits) as client:
        with pytest.raises(httpx.ConnectError):
            client.get("http://example.com/first")

        # The failed request must not have kept its permit.
        response = client.get("http://example.com/second")
        assert response.content == b"ok"


def test_sync_permit_released_when_response_hook_raises() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"ok")

    def response_hook(response: httpx.Response) -> None:
        if response.request.url.path == "/hook":
            raise RuntimeError("hook failure")

    with httpx.Client(
        transport=httpx.MockTransport(handler),
        limits=limits,
        event_hooks={"response": [response_hook]},
    ) as client:
        with pytest.raises(RuntimeError):
            client.get("http://example.com/hook")

        # A hook failure closes the response, which returns the permit.
        response = client.get("http://example.com/after")
        assert response.content == b"ok"


def test_sync_permit_released_when_hook_raises_on_stream() -> None:
    """A hook failure must also close a streamed body, returning its permit."""

    recording = Recording()
    limits = httpx.Limits(max_connections_per_origin=1)
    bodies: list[SyncBody] = []

    class Transport(httpx.BaseTransport):
        def handle_request(self, request: httpx.Request) -> httpx.Response:
            recording.enter("example.com")
            body = SyncBody(recording, "example.com")
            bodies.append(body)
            return httpx.Response(200, stream=body)

    def response_hook(response: httpx.Response) -> None:
        raise RuntimeError("hook failure")

    with httpx.Client(
        transport=Transport(),
        limits=limits,
        event_hooks={"response": [response_hook]},
    ) as client:
        with pytest.raises(RuntimeError):
            client.get("http://example.com/hook")

        # The streamed response body was closed, which returns the permit.
        assert bodies[0].closed
        assert _origin_state(client, "example.com") is None

    with recording.lock:
        assert recording.in_flight["example.com"] == 0


def test_sync_permit_released_when_request_hook_raises() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:  # pragma: no cover
        nonlocal calls
        calls += 1
        return httpx.Response(200, content=b"ok")

    def request_hook(request: httpx.Request) -> None:
        raise RuntimeError("hook failure")

    with httpx.Client(
        transport=httpx.MockTransport(handler),
        limits=limits,
        event_hooks={"request": [request_hook]},
    ) as client:
        for path in ("/hook", "/again"):
            with pytest.raises(RuntimeError):
                client.get(f"http://example.com{path}")

    # The request hook runs after admission, so the permit must be returned.
    assert calls == 0


def test_sync_cross_origin_redirect_does_not_hold_two_permits() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    recording = Recording()
    held: list[str] = []
    max_held = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal max_held
        host = request.url.host
        recording.enter(host)
        held.append(host)
        max_held = max(max_held, len(held))

        try:
            if request.url.path == "/redirect":
                # Response bodies are consumed before the next redirect is
                # admitted, so this origin's permit is returned here.
                return httpx.Response(
                    302,
                    headers={"location": "http://target.example/"},
                    stream=SyncBody(recording, host, chunks=()),
                )
            return httpx.Response(200, stream=SyncBody(recording, host))
        finally:
            held.remove(host)

    with httpx.Client(transport=httpx.MockTransport(handler), limits=limits) as client:
        response = client.get("http://source.example/redirect", follow_redirects=True)
        assert response.url == httpx.URL("http://target.example/")
        assert response.content == b"ab"

        # Both origins have their permit back, so either can send again.
        client.get("http://source.example/")
        client.get("http://target.example/")

    assert max_held == 1
    with recording.lock:
        assert recording.peak["source.example"] == 1
        assert recording.peak["target.example"] == 1


def test_sync_redirect_without_following_keeps_permit_until_closed() -> None:
    recording = Recording()
    limits = httpx.Limits(max_connections_per_origin=1)
    calls: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.host)
        if request.url.path == "/redirect":
            # A streamed redirect body keeps its permit until it is closed.
            recording.enter("source.example")
            return httpx.Response(
                302,
                headers={"location": "http://target.example/"},
                stream=SyncBody(recording, "source.example", chunks=()),
            )
        return httpx.Response(200, content=b"ok")

    with httpx.Client(transport=httpx.MockTransport(handler), limits=limits) as client:
        response = client.send(
            client.build_request("GET", "http://source.example/redirect"),
            stream=True,
            follow_redirects=False,
        )
        assert response.next_request is not None
        assert not response.is_closed
        state = _origin_state(client, "source.example")
        assert state is not None and state.in_flight == 1

        # The unread response still owns its permit until it is closed.
        response.close()
        assert _origin_state(client, "source.example") is None

        following = client.send(response.next_request, follow_redirects=False)
        assert following.content == b"ok"

    assert calls == ["source.example", "target.example"]
    with recording.lock:
        assert recording.in_flight["source.example"] == 0


def test_sync_close_wakes_up_waiting_requests() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    first_started = threading.Event()
    release_first = threading.Event()
    errors: list[BaseException] = []
    results: list[httpx.Response] = []

    def handler(request: httpx.Request) -> httpx.Response:
        first_started.set()
        release_first.wait(timeout=5)
        return httpx.Response(200, content=b"ok")

    client = httpx.Client(transport=httpx.MockTransport(handler), limits=limits)

    def run(path: str) -> None:
        try:
            results.append(client.get(f"http://example.com{path}"))
        except BaseException as exc:
            errors.append(exc)

    first = threading.Thread(target=run, args=("/first",))
    first.start()
    assert first_started.wait(timeout=5)

    queued = threading.Thread(target=run, args=("/queued",))
    queued.start()
    _wait_for_waiters(client, "example.com", 1)

    client.close()
    release_first.set()

    first.join(timeout=5)
    queued.join(timeout=5)

    assert len(errors) == 1
    assert isinstance(errors[0], httpx.ClientClosed)
    assert errors[0].request.url.path == "/queued"
    assert len(results) == 1
    assert results[0].status_code == 200

    # Once closed the client rejects further requests with a clear error.
    with pytest.raises(httpx.ClientClosed):
        client.get("http://example.com/after")


def test_sync_close_leaves_no_pending_queue() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    first_started = threading.Event()
    release_first = threading.Event()
    errors: list[BaseException] = []

    def handler(request: httpx.Request) -> httpx.Response:
        first_started.set()
        release_first.wait(timeout=5)
        return httpx.Response(200, content=b"ok")

    client = httpx.Client(transport=httpx.MockTransport(handler), limits=limits)

    first = threading.Thread(target=client.get, args=("http://example.com/first",))
    first.start()
    assert first_started.wait(timeout=5)

    def queued() -> None:
        try:
            client.get("http://example.com/queued")
        except BaseException as exc:
            errors.append(exc)

    threads = [threading.Thread(target=queued) for _ in range(3)]
    for thread in threads:
        thread.start()

    _wait_for_waiters(client, "example.com", 3)

    client.close()
    release_first.set()

    first.join(timeout=5)
    for thread in threads:
        thread.join(timeout=5)

    assert len(errors) == 3
    assert all(isinstance(exc, httpx.ClientClosed) for exc in errors)

    # The client is closed, and the waiting queue has been torn down, so that it
    # cannot act as an unbounded backlog of pending connections.
    assert client._origin_limiter.is_closed
    limiter = client._origin_limiter
    with limiter._lock:
        assert not limiter._states


# -----------------------------------------------------------------------------
# Async client
# -----------------------------------------------------------------------------


@pytest.mark.anyio
async def test_async_per_origin_limit_caps_in_flight_requests() -> None:
    recording = Recording()
    gate = anyio.Event()
    transport = GatedAsyncTransport(recording, {"example.com": gate})
    limits = httpx.Limits(max_connections_per_origin=2)
    outcomes: list[httpx.Response] = []

    async with httpx.AsyncClient(transport=transport, limits=limits) as client:

        async def run() -> None:
            outcomes.append(await client.get("http://example.com/"))

        async with anyio.create_task_group() as tg:
            for _ in range(6):
                tg.start_soon(run)

            # Wait until the origin is saturated, rather than relying on a fixed
            # delay, so that this is robust on a loaded machine.
            with anyio.fail_after(10):
                while True:
                    with recording.lock:
                        started = len(recording.started)
                    if started >= 2:
                        break
                    await anyio.sleep(0.01)

            # Every admitted request is blocked on the gate, so the cap must hold
            # and no further requests may be admitted.
            await anyio.sleep(0.05)
            assert recording.started == ["example.com", "example.com"]

            gate.set()

    assert len(outcomes) == 6
    with recording.lock:
        assert recording.peak["example.com"] == 2
        assert recording.in_flight["example.com"] == 0


@pytest.mark.anyio
async def test_async_other_origins_progress_while_one_origin_is_queued() -> None:
    recording = Recording()
    slow_gate = anyio.Event()
    other_gate = anyio.Event()
    transport = GatedAsyncTransport(
        recording, {"slow.example": slow_gate, "other.example": other_gate}
    )
    limits = httpx.Limits(max_connections_per_origin=1)
    other_done = anyio.Event()

    async with httpx.AsyncClient(transport=transport, limits=limits) as client:

        async def slow() -> None:
            await client.get("http://slow.example/")

        async def other() -> None:
            await client.get("http://other.example/")
            other_done.set()

        async with anyio.create_task_group() as tg:
            for _ in range(3):
                tg.start_soon(slow)

            for _ in range(300):
                if len(recording.started) == 1:
                    break
                await anyio.sleep(0.01)
            assert recording.started == ["slow.example"]

            tg.start_soon(other)

            for _ in range(300):
                if "other.example" in recording.started:
                    break
                await anyio.sleep(0.01)
            assert "other.example" in recording.started

            # The other origin completes while the busy origin is still queued.
            other_gate.set()
            with anyio.fail_after(5):
                await other_done.wait()

            slow_gate.set()

    with recording.lock:
        assert recording.peak["slow.example"] == 1
        assert recording.peak["other.example"] == 1


@pytest.mark.anyio
async def test_async_same_origin_admitted_in_arrival_order() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    admission_order: list[str] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        admission_order.append(request.url.path)
        await anyio.sleep(0)
        return httpx.Response(200, stream=AsyncBody(chunks=()))

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), limits=limits
    ) as client:
        paths = [f"/{index}" for index in range(6)]

        async def run(path: str) -> None:
            await client.get(f"http://example.com{path}")

        async with anyio.create_task_group() as tg:
            for path in paths:
                tg.start_soon(run, path)
                await anyio.sleep(0.02)

    assert admission_order == paths


@pytest.mark.anyio
async def test_async_cancelled_waiter_gives_up_its_place() -> None:
    recording = Recording()
    limits = httpx.Limits(max_connections_per_origin=1)
    gate = anyio.Event()
    transport = GatedAsyncTransport(recording, {"example.com": gate})
    served: list[str] = []

    class RecordingTransport(GatedAsyncTransport):
        async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
            served.append(request.url.path)
            return await super().handle_async_request(request)

    transport = RecordingTransport(recording, {"example.com": gate})

    async with httpx.AsyncClient(transport=transport, limits=limits) as client:

        async def first() -> None:
            await client.get("http://example.com/first")

        async with anyio.create_task_group() as tg:
            tg.start_soon(first)
            for _ in range(300):
                if served:
                    break
                await anyio.sleep(0.01)

            with anyio.move_on_after(0.05):
                await client.get("http://example.com/cancelled")

            # The cancelled waiter must not block the origin, and must not leave
            # bookkeeping behind.
            state = _origin_state(client, "example.com")
            assert state is not None
            with client._origin_limiter._lock:
                assert len(state.waiters) == 0
                assert state.in_flight == 1

            gate.set()

        follow_up = await client.get("http://example.com/after")

    assert follow_up.content == b"ab"
    assert served == ["/first", "/after"]


@pytest.mark.anyio
async def test_async_permit_released_when_response_is_closed_early() -> None:
    recording = Recording()
    limits = httpx.Limits(max_connections_per_origin=1)
    bodies: list[AsyncBody] = []

    class Transport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
            recording.enter("example.com")
            body = AsyncBody(recording, "example.com")
            bodies.append(body)
            return httpx.Response(200, stream=body)

    async with httpx.AsyncClient(transport=Transport(), limits=limits) as client:
        async with client.stream("GET", "http://example.com/first") as response:
            assert response.status_code == 200

        second = await client.get("http://example.com/second")
        assert second.status_code == 200

    assert bodies[0].closed


@pytest.mark.anyio
async def test_async_permit_released_when_transport_fails() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    calls = 0

    async def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if calls == 1:
            raise httpx.ConnectError("failure", request=request)
        return httpx.Response(200, content=b"ok")

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), limits=limits
    ) as client:
        with pytest.raises(httpx.ConnectError):
            await client.get("http://example.com/first")

        response = await client.get("http://example.com/second")
        assert response.content == b"ok"


@pytest.mark.anyio
async def test_async_permit_released_when_hook_raises() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)

    async def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"ok")

    async def response_hook(response: httpx.Response) -> None:
        if response.request.url.path == "/hook":
            raise RuntimeError("hook failure")

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler),
        limits=limits,
        event_hooks={"response": [response_hook]},
    ) as client:
        with pytest.raises(RuntimeError):
            await client.get("http://example.com/hook")

        response = await client.get("http://example.com/after")
        assert response.content == b"ok"


@pytest.mark.anyio
async def test_async_cross_origin_redirect_does_not_hold_two_permits() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    recording = Recording()
    held: list[str] = []
    max_held = 0

    async def handler(request: httpx.Request) -> httpx.Response:
        nonlocal max_held
        host = request.url.host
        recording.enter(host)
        held.append(host)
        max_held = max(max_held, len(held))

        try:
            if request.url.path == "/redirect":
                # Response bodies are consumed before the next redirect is
                # admitted, so this origin's permit is returned here.
                return httpx.Response(
                    302,
                    headers={"location": "http://target.example/"},
                    stream=AsyncBody(recording, host, chunks=()),
                )
            return httpx.Response(200, stream=AsyncBody(recording, host))
        finally:
            held.remove(host)

    async with httpx.AsyncClient(
        transport=httpx.MockTransport(handler), limits=limits
    ) as client:
        response = await client.get(
            "http://source.example/redirect", follow_redirects=True
        )
        assert response.url == httpx.URL("http://target.example/")
        assert response.content == b"ab"

        await client.get("http://source.example/")
        await client.get("http://target.example/")

    assert max_held == 1
    with recording.lock:
        assert recording.peak["source.example"] == 1
        assert recording.peak["target.example"] == 1


@pytest.mark.anyio
async def test_async_close_wakes_queued_requests() -> None:
    limits = httpx.Limits(max_connections_per_origin=1)
    first_started = anyio.Event()
    release_first = anyio.Event()
    errors: list[BaseException] = []
    results: list[httpx.Response] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        first_started.set()
        await release_first.wait()
        return httpx.Response(200, content=b"ok")

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler), limits=limits)

    async def run(path: str) -> None:
        try:
            results.append(await client.get(f"http://example.com{path}"))
        except BaseException as exc:
            errors.append(exc)

    async with anyio.create_task_group() as tg:
        tg.start_soon(run, "/first")
        await first_started.wait()

        tg.start_soon(run, "/queued")
        await _await_waiters(client, "example.com", 1)

        await client.aclose()
        release_first.set()

    assert len(errors) == 1
    assert isinstance(errors[0], httpx.ClientClosed)
    assert errors[0].request.url.path == "/queued"
    assert len(results) == 1

    with pytest.raises(httpx.ClientClosed):
        await client.get("http://example.com/after")


# -----------------------------------------------------------------------------
# Configuration
# -----------------------------------------------------------------------------


def test_limits_per_origin_default_is_none() -> None:
    assert httpx.Limits().max_connections_per_origin is None


def test_limits_per_origin_equality() -> None:
    limits = httpx.Limits(max_connections_per_origin=3)
    assert limits == httpx.Limits(max_connections_per_origin=3)
    assert limits != httpx.Limits(max_connections_per_origin=2)


def test_limits_per_origin_repr_only_when_set() -> None:
    assert "max_connections_per_origin=3" in repr(
        httpx.Limits(max_connections_per_origin=3)
    )
    assert "max_connections_per_origin" not in repr(httpx.Limits())


@pytest.mark.parametrize("value", [0, -1])
def test_limits_per_origin_must_be_positive(value: int) -> None:
    with pytest.raises(ValueError):
        httpx.Limits(max_connections_per_origin=value)


@pytest.mark.parametrize("value", [2.0, "2", True])
def test_limits_per_origin_must_be_an_integer(value: typing.Any) -> None:
    with pytest.raises(TypeError):
        httpx.Limits(max_connections_per_origin=value)


def test_client_without_per_origin_limit_has_no_limiter() -> None:
    with httpx.Client() as client:
        assert client._origin_limiter is None


def test_origin_key_includes_scheme_host_and_port() -> None:
    from httpx._origin_limits import origin_key

    assert origin_key(httpx.URL("https://example.com/")) == (
        "https",
        "example.com",
        443,
    )
    assert origin_key(httpx.URL("http://example.com:8080/")) == (
        "http",
        "example.com",
        8080,
    )
    assert origin_key(httpx.URL("http://example.com/")) != origin_key(
        httpx.URL("https://example.com/")
    )


def test_sync_limiter_handover_losing_to_close_returns_client_closed() -> None:
    """
    A permit handed over at the same moment the client closes must not be used.
    """
    from httpx._origin_limits import OriginLimiter

    limiter = OriginLimiter(1)
    url = httpx.URL("http://example.com/")
    held = limiter.acquire(url)
    outcome: dict[str, object] = {}

    def waiter() -> None:
        try:
            outcome["permit"] = limiter.acquire(url)
        except BaseException as exc:
            outcome["error"] = exc

    thread = threading.Thread(target=waiter)
    thread.start()
    _wait_for_limiter_waiters(limiter, 1)

    # Hand the permit over, and then close the limiter before the waiter runs.
    held.release()
    limiter.close()
    thread.join(timeout=5)

    assert isinstance(outcome.get("error"), httpx.ClientClosed)
    with limiter._lock:
        assert not limiter._states
