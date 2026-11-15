from __future__ import annotations

import typing

import pytest

import httpx

# Tests for the optional overall request deadline (`Timeout(total=...)`).
# The deadline is shared across the whole exchange -- from the first send
# until the response body is fully read -- so redirects and authentication
# challenges never reset the remaining budget. A controllable clock and a
# mock transport make the behaviour deterministic, including the key case
# where no individual hop exceeds its own stage timeout but the chain as a
# whole outlives the overall deadline.


class FakeClock:
    # A manually advanced clock implementing the duck-typed clock interface
    # expected by the clients (a `now()` method returning monotonic seconds).

    def __init__(self) -> None:
        self.current = 0.0

    def now(self) -> float:
        return self.current

    def advance(self, seconds: float) -> None:
        self.current += seconds


def redirect_handler(
    clock: FakeClock,
    seen_timeouts: list[dict[str, float | None]],
    *,
    hop_duration: float = 3.0,
) -> typing.Callable[[httpx.Request], httpx.Response]:
    def handler(request: httpx.Request) -> httpx.Response:
        # Record the per-stage timeouts the transport actually receives, so
        # the remaining budget can be asserted hop by hop.
        seen_timeouts.append(dict(request.extensions["timeout"]))
        clock.advance(hop_duration)
        if request.url.path == "/start":
            return httpx.Response(302, headers={"location": "/middle"})
        if request.url.path == "/middle":
            return httpx.Response(302, headers={"location": "/final"})
        return httpx.Response(200, content=b"done")

    return handler


def test_total_timeout_config_is_explicit() -> None:
    # The positional default must not implicitly enable an overall deadline.
    assert httpx.Timeout(5.0).total is None

    timeout = httpx.Timeout(5.0, total=10.0)
    assert timeout.total == 10.0
    assert timeout.as_dict()["total"] == 10.0
    assert repr(timeout).endswith("total=10.0)")

    # Equality accounts for the new field.
    assert timeout == httpx.Timeout(5.0, total=10.0)
    assert timeout != httpx.Timeout(5.0)

    # The default repr/equality of existing configurations is unchanged.
    assert repr(httpx.Timeout(5.0)) == "Timeout(timeout=5.0)"


def test_sync_chain_expires_without_any_hop_timing_out() -> None:
    # The headline scenario: every hop stays below its capped stage timeout,
    # but the cumulative chain exceeds the overall deadline.
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []
    transport = httpx.MockTransport(
        redirect_handler(clock, seen_timeouts, hop_duration=3.0)
    )

    # Generous 10s per-stage timeouts, but only a 5s overall deadline.
    timeout = httpx.Timeout(10.0, total=5.0)
    with httpx.Client(transport=transport, timeout=timeout, clock=clock) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            client.get("http://example.org/start", follow_redirects=True)

    exc = exc_info.value

    # The exception carries both the original request and the last one sent.
    assert str(exc.request.url).endswith("/start")
    assert str(exc.last_request.url).endswith("/final")
    assert isinstance(exc, httpx.TimeoutException)

    # Two hops completed (3s each); the third hop would start at t=6 which
    # is past the t=5 deadline, so only two transport calls were made.
    assert len(seen_timeouts) == 2

    # The remaining budget was handed to each hop and shrank over time. The
    # `total` key is a client concern and must never reach the transport.
    first, second = seen_timeouts
    assert set(first) == {"connect", "read", "write", "pool"}
    assert first == {"connect": 5.0, "read": 5.0, "write": 5.0, "pool": 5.0}
    assert second == {"connect": 2.0, "read": 2.0, "write": 2.0, "pool": 2.0}


def test_sync_chain_succeeds_when_deadline_is_large_enough() -> None:
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []
    transport = httpx.MockTransport(
        redirect_handler(clock, seen_timeouts, hop_duration=1.0)
    )

    with httpx.Client(
        transport=transport, timeout=httpx.Timeout(10.0, total=5.0), clock=clock
    ) as client:
        response = client.get("http://example.org/start", follow_redirects=True)

    assert response.status_code == 200
    assert response.text == "done"
    assert [r.url.path for r in response.history] == ["/start", "/middle"]
    assert len(seen_timeouts) == 3


def test_no_total_deadline_leaves_stage_timeouts_untouched() -> None:
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []
    transport = httpx.MockTransport(
        redirect_handler(clock, seen_timeouts, hop_duration=1.0)
    )

    with httpx.Client(
        transport=transport, timeout=httpx.Timeout(10.0), clock=clock
    ) as client:
        response = client.get("http://example.org/start", follow_redirects=True)

    assert response.status_code == 200
    # Without an overall deadline every hop keeps the full stage timeouts.
    assert all(
        hop == {"connect": 10.0, "read": 10.0, "write": 10.0, "pool": 10.0}
        for hop in seen_timeouts
    )


def test_stage_timeout_still_fires_first_when_smaller_than_budget() -> None:
    # When the per-stage timeout and the overall deadline coexist, the
    # smaller of the two is used for each stage, so whichever fires first wins.
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []
    transport = httpx.MockTransport(
        redirect_handler(clock, seen_timeouts, hop_duration=0.0)
    )

    # Stage timeout of 2s is below the 10s overall budget on the first hop.
    timeout = httpx.Timeout(2.0, total=10.0)
    with httpx.Client(transport=transport, timeout=timeout, clock=clock) as client:
        client.get("http://example.org/start", follow_redirects=True)

    first = seen_timeouts[0]
    assert first == {"connect": 2.0, "read": 2.0, "write": 2.0, "pool": 2.0}


def test_sync_stream_closes_response_when_deadline_expires_mid_body() -> None:
    clock = FakeClock()
    close_count = 0

    class TrackingStream(httpx.SyncByteStream):
        def __iter__(self) -> typing.Iterator[bytes]:
            yield b"ab"
            clock.advance(6.0)
            yield b"cd"

        def close(self) -> None:
            nonlocal close_count
            close_count += 1

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, stream=TrackingStream())

    transport = httpx.MockTransport(handler)

    with httpx.Client(
        transport=transport, timeout=httpx.Timeout(None, total=5.0), clock=clock
    ) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            with client.stream("GET", "http://example.org/") as response:
                response.read()

    exc = exc_info.value
    assert exc.request.url.path == "/"
    assert exc.last_request.url.path == "/"
    # The current response is released when the deadline interrupts the read.
    assert close_count == 1


@pytest.mark.anyio
async def test_async_chain_expires_on_overall_deadline() -> None:
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []
    transport = httpx.MockTransport(
        redirect_handler(clock, seen_timeouts, hop_duration=3.0)
    )

    async with httpx.AsyncClient(
        transport=transport, timeout=httpx.Timeout(10.0, total=5.0), clock=clock
    ) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            await client.get("http://example.org/start", follow_redirects=True)

    exc = exc_info.value
    assert str(exc.request.url).endswith("/start")
    assert str(exc.last_request.url).endswith("/final")
    assert len(seen_timeouts) == 2


@pytest.mark.anyio
async def test_async_stream_closes_response_on_expired_deadline() -> None:
    clock = FakeClock()

    async def handler(request: httpx.Request) -> httpx.Response:
        async def chunks() -> typing.AsyncIterator[bytes]:
            yield b"ab"
            clock.advance(6.0)
            yield b"cd"

        return httpx.Response(200, content=chunks())

    transport = httpx.MockTransport(handler)

    async with httpx.AsyncClient(
        transport=transport, timeout=httpx.Timeout(None, total=5.0), clock=clock
    ) as client:
        with pytest.raises(httpx.TotalTimeout):
            async with client.stream("GET", "http://example.org/") as response:
                await response.aread()


class ChallengeAuth(httpx.Auth):
    def __init__(self) -> None:
        self.attempts = 0

    def auth_flow(
        self, request: httpx.Request
    ) -> typing.Generator[httpx.Request, httpx.Response, None]:
        response = yield request
        if response.status_code == 401:
            self.attempts += 1
            request.headers["Authorization"] = "Basic credentials"
            yield request


def test_authentication_challenge_does_not_reset_deadline() -> None:
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_timeouts.append(dict(request.extensions["timeout"]))
        clock.advance(3.0)
        if not request.headers.get("Authorization"):
            return httpx.Response(401, headers={"WWW-Authenticate": "Basic"})
        return httpx.Response(200, content=b"secret")

    transport = httpx.MockTransport(handler)
    auth = ChallengeAuth()

    with httpx.Client(
        transport=transport, timeout=httpx.Timeout(10.0, total=5.0), clock=clock
    ) as client:
        with pytest.raises(httpx.TotalTimeout) as exc_info:
            client.get("http://example.org/protected", auth=auth)

    exc = exc_info.value
    # Both requests share the URL; the deadline is shared rather than reset.
    assert exc.request.url.path == "/protected"
    assert exc.last_request.url.path == "/protected"
    assert auth.attempts == 1
    assert seen_timeouts[0]["read"] == 5.0
    assert seen_timeouts[1]["read"] == 2.0


def test_per_request_total_override() -> None:
    clock = FakeClock()
    seen_timeouts: list[dict[str, float | None]] = []
    transport = httpx.MockTransport(
        redirect_handler(clock, seen_timeouts, hop_duration=3.0)
    )

    with httpx.Client(
        transport=transport, timeout=httpx.Timeout(10.0), clock=clock
    ) as client:
        with pytest.raises(httpx.TotalTimeout):
            client.get(
                "http://example.org/start",
                follow_redirects=True,
                timeout=httpx.Timeout(10.0, total=5.0),
            )

    assert len(seen_timeouts) == 2
