"""
Tests for the total request `deadline`.

A deadline is a total time budget for a single `client.send()` call,
starting when the first request is sent and ending once the response
body has been fully read. Redirects and authentication challenges do
not reset the budget. When a deadline is used together with the
per-operation `timeout` configuration, whichever expires first wins.

These tests use a controllable clock together with a mock transport,
so that each individual hop stays well within the per-phase timeouts
while the chain as a whole overruns the total deadline.
"""

from __future__ import annotations

import typing

import pytest

import httpx
from httpx import _deadline


class FakeClock:
    """A controllable monotonic clock."""

    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


@pytest.fixture
def clock(monkeypatch: pytest.MonkeyPatch) -> FakeClock:
    clock = FakeClock()
    monkeypatch.setattr(_deadline, "monotonic", clock)
    return clock


def redirect_chain_transport(
    clock: FakeClock, hop_cost: float
) -> httpx.MockTransport:
    """
    A transport where `/redirect` issues a redirect to `/final`.
    Each hop takes `hop_cost` seconds of clock time.
    """

    def handler(request: httpx.Request) -> httpx.Response:
        clock.advance(hop_cost)
        if request.url.path == "/redirect":
            return httpx.Response(
                303, headers={"Location": "/final"}, content=b"redirect"
            )
        return httpx.Response(200, content=b"Hello, world!")

    return httpx.MockTransport(handler)


class ChallengeAuth(httpx.Auth):
    """An auth flow that retries once with credentials on a 401 challenge."""

    requires_response_body = True

    def auth_flow(
        self, request: httpx.Request
    ) -> typing.Generator[httpx.Request, httpx.Response, None]:
        response = yield request
        if response.status_code == 401:
            request.headers["Authorization"] = "Bearer token"
            yield request


def auth_challenge_transport(
    clock: FakeClock, hop_cost: float
) -> httpx.MockTransport:
    """
    A transport that issues a 401 authentication challenge, then accepts
    the retried request. Each hop takes `hop_cost` seconds of clock time.
    """

    def handler(request: httpx.Request) -> httpx.Response:
        clock.advance(hop_cost)
        if "Authorization" not in request.headers:
            return httpx.Response(401, content=b"unauthorized")
        return httpx.Response(200, content=b"Hello, world!")

    return httpx.MockTransport(handler)


class SlowSyncStream(httpx.SyncByteStream):
    """A response body stream that takes `chunk_cost` seconds per chunk."""

    def __init__(
        self, clock: FakeClock, chunks: list[bytes], chunk_cost: float
    ) -> None:
        self._clock = clock
        self._chunks = chunks
        self._chunk_cost = chunk_cost
        self.closed = False

    def __iter__(self) -> typing.Iterator[bytes]:
        for chunk in self._chunks:
            self._clock.advance(self._chunk_cost)
            yield chunk

    def close(self) -> None:
        self.closed = True


class SlowAsyncStream(httpx.AsyncByteStream):
    """An async response body stream that takes `chunk_cost` seconds per chunk."""

    def __init__(
        self, clock: FakeClock, chunks: list[bytes], chunk_cost: float
    ) -> None:
        self._clock = clock
        self._chunks = chunks
        self._chunk_cost = chunk_cost
        self.closed = False

    def __aiter__(self) -> typing.AsyncIterator[bytes]:
        return _SlowAsyncIterator(self._clock, self._chunks, self._chunk_cost)

    async def aclose(self) -> None:
        self.closed = True


class _SlowAsyncIterator:
    def __init__(
        self, clock: FakeClock, chunks: list[bytes], chunk_cost: float
    ) -> None:
        self._clock = clock
        self._iterator = iter(chunks)
        self._chunk_cost = chunk_cost

    def __aiter__(self) -> "_SlowAsyncIterator":
        return self

    async def __anext__(self) -> bytes:
        try:
            chunk = next(self._iterator)
        except StopIteration:
            raise StopAsyncIteration from None
        self._clock.advance(self._chunk_cost)
        return chunk


def test_deadline_tracks_remaining_budget() -> None:
    clock = FakeClock()
    deadline = _deadline.Deadline(5.0, clock=clock)
    assert deadline.is_active
    assert deadline.remaining == 5.0
    assert not deadline.expired

    clock.advance(3.0)
    assert deadline.remaining == 2.0
    assert not deadline.expired

    clock.advance(2.0)
    assert deadline.remaining == 0.0
    assert deadline.expired

    request = httpx.Request("GET", "http://example.org/")
    with pytest.raises(httpx.TotalTimeout):
        deadline.check(request, request)

    # An inactive deadline never expires.
    deadline = _deadline.Deadline(None, clock=clock)
    clock.advance(1000.0)
    assert not deadline.is_active
    assert not deadline.expired
    assert deadline.remaining is None


def test_total_deadline_exceeded_across_redirects(clock: FakeClock) -> None:
    """
    Each individual hop completes within the per-phase timeout, but the
    redirect chain as a whole overruns the total deadline. The deadline
    is not reset by the redirect.
    """
    transport = redirect_chain_transport(clock, hop_cost=3.0)
    with httpx.Client(transport=transport, follow_redirects=True) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            client.get(
                "http://example.org/redirect",
                # Each 3s hop is well within the per-phase timeouts...
                timeout=httpx.Timeout(10.0),
                # ...but two hops exceed the 5s total deadline.
                deadline=5.0,
            )

    exc = exc_info.value
    assert isinstance(exc, httpx.TimeoutException)
    # The exception carries the original request...
    assert exc.request.url == "http://example.org/redirect"
    # ...and the last request that was actually in flight.
    assert exc.last_request.url == "http://example.org/final"


def test_no_deadline_allows_slow_redirect_chain(clock: FakeClock) -> None:
    transport = redirect_chain_transport(clock, hop_cost=3.0)
    with httpx.Client(transport=transport, follow_redirects=True) as client:
        response = client.get(
            "http://example.org/redirect", timeout=httpx.Timeout(10.0)
        )
    assert response.status_code == 200
    assert response.text == "Hello, world!"


def test_total_deadline_exceeded_across_auth_challenge(clock: FakeClock) -> None:
    """
    An authentication challenge does not reset the total deadline either.
    """
    transport = auth_challenge_transport(clock, hop_cost=3.0)
    with httpx.Client(transport=transport) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            client.get(
                "http://example.org/",
                auth=ChallengeAuth(),
                timeout=httpx.Timeout(10.0),
                deadline=5.0,
            )

    exc = exc_info.value
    assert exc.request.url == "http://example.org/"
    # The last request in flight was the retried, authenticated one.
    assert exc.last_request.headers["Authorization"] == "Bearer token"


def test_total_deadline_exceeded_while_reading_body(clock: FakeClock) -> None:
    """
    The deadline spans until the response body has been fully read.
    On expiry the current response is closed.
    """
    stream = SlowSyncStream(clock, chunks=[b"aa", b"bb", b"cc"], chunk_cost=2.0)

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, stream=stream)

    transport = httpx.MockTransport(handler)
    with httpx.Client(transport=transport) as client:
        with pytest.raises(httpx.TotalTimeout):
            # Three 2s chunks overrun the 5s total deadline, although
            # each chunk is well within the 10s read timeout.
            client.get(
                "http://example.org/",
                timeout=httpx.Timeout(10.0),
                deadline=5.0,
            )
    assert stream.closed


def test_total_deadline_exceeded_while_streaming_body(clock: FakeClock) -> None:
    """
    The `client.stream()` entry point enforces the deadline while the
    response body is being consumed.
    """
    stream = SlowSyncStream(clock, chunks=[b"aa", b"bb", b"cc"], chunk_cost=2.0)

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, stream=stream)

    transport = httpx.MockTransport(handler)
    with httpx.Client(transport=transport) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            with client.stream(
                "GET",
                "http://example.org/",
                timeout=httpx.Timeout(10.0),
                deadline=5.0,
            ) as response:
                b"".join(response.iter_bytes())

    assert exc_info.value.request.url == "http://example.org/"
    assert stream.closed


def test_per_phase_timeouts_clamped_to_remaining_budget(clock: FakeClock) -> None:
    """
    When a total deadline and per-phase timeouts coexist, the transport
    sees each per-phase timeout clamped to the remaining budget, so that
    whichever expires first takes effect.
    """
    seen_timeouts: list[dict[str, float | None]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_timeouts.append(dict(request.extensions["timeout"]))
        clock.advance(2.0)
        if len(seen_timeouts) == 1:
            return httpx.Response(
                303, headers={"Location": "/final"}, content=b"redirect"
            )
        return httpx.Response(200, content=b"Hello, world!")

    transport = httpx.MockTransport(handler)
    with httpx.Client(transport=transport, follow_redirects=True) as client:
        response = client.get(
            "http://example.org/redirect",
            timeout=httpx.Timeout(10.0),
            deadline=5.0,
        )
    assert response.status_code == 200

    # The first hop gets the full budget, clamping the 10s per-phase timeouts.
    assert seen_timeouts[0] == {
        "connect": 5.0,
        "read": 5.0,
        "write": 5.0,
        "pool": 5.0,
    }
    # The second hop only gets the remaining budget, not a fresh one.
    assert seen_timeouts[1] == {
        "connect": 3.0,
        "read": 3.0,
        "write": 3.0,
        "pool": 3.0,
    }


def test_tighter_per_phase_timeout_takes_precedence(clock: FakeClock) -> None:
    """
    A per-phase timeout tighter than the remaining budget is left as-is.
    """
    seen_timeouts: list[dict[str, float | None]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_timeouts.append(dict(request.extensions["timeout"]))
        return httpx.Response(200, content=b"Hello, world!")

    transport = httpx.MockTransport(handler)
    with httpx.Client(transport=transport) as client:
        client.get(
            "http://example.org/",
            timeout=httpx.Timeout(10.0, read=1.0),
            deadline=100.0,
        )
    assert seen_timeouts[0] == {
        "connect": 10.0,
        "read": 1.0,
        "write": 10.0,
        "pool": 10.0,
    }


def test_client_level_deadline_default(clock: FakeClock) -> None:
    transport = redirect_chain_transport(clock, hop_cost=3.0)
    with httpx.Client(
        transport=transport, follow_redirects=True, deadline=5.0
    ) as client:
        with pytest.raises(httpx.TotalTimeout):
            client.get("http://example.org/redirect", timeout=httpx.Timeout(10.0))

        # A per-request `deadline=None` disables the client-level default.
        response = client.get(
            "http://example.org/redirect",
            timeout=httpx.Timeout(10.0),
            deadline=None,
        )
        assert response.status_code == 200


def test_request_without_deadline_is_unaffected(clock: FakeClock) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert "deadline" in request.extensions
        assert request.extensions["deadline"] is None
        clock.advance(100.0)
        return httpx.Response(200, content=b"Hello, world!")

    transport = httpx.MockTransport(handler)
    with httpx.Client(transport=transport) as client:
        response = client.get("http://example.org/")
    assert response.status_code == 200


@pytest.mark.anyio
async def test_async_total_deadline_exceeded_across_redirects(
    clock: FakeClock,
) -> None:
    transport = redirect_chain_transport(clock, hop_cost=3.0)
    async with httpx.AsyncClient(
        transport=transport, follow_redirects=True
    ) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            await client.get(
                "http://example.org/redirect",
                timeout=httpx.Timeout(10.0),
                deadline=5.0,
            )

    exc = exc_info.value
    assert isinstance(exc, httpx.TimeoutException)
    assert exc.request.url == "http://example.org/redirect"
    assert exc.last_request.url == "http://example.org/final"


@pytest.mark.anyio
async def test_async_total_deadline_exceeded_while_streaming_body(
    clock: FakeClock,
) -> None:
    stream = SlowAsyncStream(clock, chunks=[b"aa", b"bb", b"cc"], chunk_cost=2.0)

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, stream=stream)

    transport = httpx.MockTransport(handler)
    async with httpx.AsyncClient(transport=transport) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            async with client.stream(
                "GET",
                "http://example.org/",
                timeout=httpx.Timeout(10.0),
                deadline=5.0,
            ) as response:
                b"".join([part async for part in response.aiter_bytes()])

    assert exc_info.value.request.url == "http://example.org/"
    assert stream.closed


@pytest.mark.anyio
async def test_async_no_deadline_allows_slow_redirect_chain(
    clock: FakeClock,
) -> None:
    transport = redirect_chain_transport(clock, hop_cost=3.0)
    async with httpx.AsyncClient(
        transport=transport, follow_redirects=True
    ) as client:
        response = await client.get(
            "http://example.org/redirect", timeout=httpx.Timeout(10.0)
        )
    assert response.status_code == 200
    assert response.text == "Hello, world!"
