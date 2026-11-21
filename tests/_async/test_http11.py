import typing

import pytest

import httpcore


@pytest.mark.anyio
async def test_http11_connection():
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, world!",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        assert response.status == 200
        assert response.content == b"Hello, world!"

        assert conn.is_idle()
        assert not conn.is_closed()
        assert conn.is_available()
        assert not conn.has_expired()
        assert (
            repr(conn)
            == "<AsyncHTTP11Connection ['https://example.com:443', IDLE, Request Count: 1]>"
        )


@pytest.mark.anyio
async def test_http11_connection_unread_response():
    """
    If the client releases the response without reading it to termination,
    then the connection will not be reusable.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, world!",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        async with conn.stream("GET", "https://example.com/") as response:
            assert response.status == 200

        assert not conn.is_idle()
        assert conn.is_closed()
        assert not conn.is_available()
        assert not conn.has_expired()
        assert (
            repr(conn)
            == "<AsyncHTTP11Connection ['https://example.com:443', CLOSED, Request Count: 1]>"
        )


@pytest.mark.anyio
async def test_http11_connection_with_remote_protocol_error():
    """
    If a remote protocol error occurs, then no response will be returned,
    and the connection will not be reusable.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream([b"Wait, this isn't valid HTTP!", b""])
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(httpcore.RemoteProtocolError):
            await conn.request("GET", "https://example.com/")

        assert not conn.is_idle()
        assert conn.is_closed()
        assert not conn.is_available()
        assert not conn.has_expired()
        assert (
            repr(conn)
            == "<AsyncHTTP11Connection ['https://example.com:443', CLOSED, Request Count: 1]>"
        )


@pytest.mark.anyio
async def test_http11_connection_with_incomplete_response():
    """
    We should be gracefully handling the case where the connection ends prematurely.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, wor",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(httpcore.RemoteProtocolError):
            await conn.request("GET", "https://example.com/")

        assert not conn.is_idle()
        assert conn.is_closed()
        assert not conn.is_available()
        assert not conn.has_expired()
        assert (
            repr(conn)
            == "<AsyncHTTP11Connection ['https://example.com:443', CLOSED, Request Count: 1]>"
        )


@pytest.mark.anyio
async def test_http11_connection_with_local_protocol_error():
    """
    If a local protocol error occurs, then no response will be returned,
    and the connection will not be reusable.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, world!",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(httpcore.LocalProtocolError) as exc_info:
            await conn.request("GET", "https://example.com/", headers={"Host": "\0"})

        assert str(exc_info.value) == "Illegal header value b'\\x00'"

        assert not conn.is_idle()
        assert conn.is_closed()
        assert not conn.is_available()
        assert not conn.has_expired()
        assert (
            repr(conn)
            == "<AsyncHTTP11Connection ['https://example.com:443', CLOSED, Request Count: 1]>"
        )


@pytest.mark.anyio
async def test_http11_connection_handles_one_active_request():
    """
    Attempting to send a request while one is already in-flight will raise
    a ConnectionNotAvailable exception.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, world!",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        async with conn.stream("GET", "https://example.com/"):
            with pytest.raises(httpcore.ConnectionNotAvailable):
                await conn.request("GET", "https://example.com/")


@pytest.mark.anyio
async def test_http11_connection_attempt_close():
    """
    A connection can only be closed when it is idle.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, world!",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        async with conn.stream("GET", "https://example.com/") as response:
            await response.aread()
            assert response.status == 200
            assert response.content == b"Hello, world!"


@pytest.mark.anyio
async def test_http11_request_to_incorrect_origin():
    """
    A connection can only send requests to whichever origin it is connected to.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream([])
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(RuntimeError):
            await conn.request("GET", "https://other.com/")


@pytest.mark.anyio
async def test_http11_expect_continue():
    """
    HTTP "100 Continue" is an interim response.
    We simply ignore it and return the final response.

    https://httpwg.org/specs/rfc9110.html#status.100
    https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/100
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 100 Continue\r\n",
            b"\r\n",
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: plain/text\r\n",
            b"Content-Length: 13\r\n",
            b"\r\n",
            b"Hello, world!",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        response = await conn.request(
            "GET",
            "https://example.com/",
            headers={"Expect": "continue"},
        )
        assert response.status == 200
        assert response.content == b"Hello, world!"


@pytest.mark.anyio
async def test_http11_upgrade_connection():
    """
    HTTP "101 Switching Protocols" indicates an upgraded connection.

    We should return the response, so that the network stream
    may be used for the upgraded connection.

    https://httpwg.org/specs/rfc9110.html#status.101
    https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/101
    """
    origin = httpcore.Origin(b"wss", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 101 Switching Protocols\r\n",
            b"Connection: upgrade\r\n",
            b"Upgrade: custom\r\n",
            b"\r\n",
            b"...",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        async with conn.stream(
            "GET",
            "wss://example.com/",
            headers={"Connection": "upgrade", "Upgrade": "custom"},
        ) as response:
            assert response.status == 101
            network_stream = response.extensions["network_stream"]
            content = await network_stream.read(max_bytes=1024)
            assert content == b"..."


@pytest.mark.anyio
async def test_http11_upgrade_with_trailing_data():
    """
    HTTP "101 Switching Protocols" indicates an upgraded connection.

    In `CONNECT` and `Upgrade:` requests, we need to handover the trailing data
    in the h11.Connection object.

    https://h11.readthedocs.io/en/latest/api.html#switching-protocols
    """
    origin = httpcore.Origin(b"wss", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        # The first element of this mock network stream buffer simulates networking
        # in which response headers and data are received at once.
        # This means that "foobar" becomes trailing data.
        [
            (
                b"HTTP/1.1 101 Switching Protocols\r\n"
                b"Connection: upgrade\r\n"
                b"Upgrade: custom\r\n"
                b"\r\n"
                b"foobar"
            ),
            b"baz",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        async with conn.stream(
            "GET",
            "wss://example.com/",
            headers={"Connection": "upgrade", "Upgrade": "custom"},
        ) as response:
            assert response.status == 101
            network_stream = response.extensions["network_stream"]

            content = await network_stream.read(max_bytes=3)
            assert content == b"foo"
            content = await network_stream.read(max_bytes=3)
            assert content == b"bar"
            content = await network_stream.read(max_bytes=3)
            assert content == b"baz"

            # Lazy tests for AsyncHTTP11UpgradeStream
            await network_stream.write(b"spam")
            invalid = network_stream.get_extra_info("invalid")
            assert invalid is None
            await network_stream.aclose()


@pytest.mark.anyio
async def test_http11_early_hints():
    """
    HTTP "103 Early Hints" is an interim response.
    We simply ignore it and return the final response.

    https://datatracker.ietf.org/doc/rfc8297/
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 103 Early Hints\r\n",
            b"Link: </style.css>; rel=preload; as=style\r\n",
            b"Link: </script.js.css>; rel=preload; as=style\r\n",
            b"\r\n",
            b"HTTP/1.1 200 OK\r\n",
            b"Content-Type: text/html; charset=utf-8\r\n",
            b"Content-Length: 30\r\n",
            b"Link: </style.css>; rel=preload; as=style\r\n",
            b"Link: </script.js>; rel=preload; as=script\r\n",
            b"\r\n",
            b"<html>Hello, world! ...</html>",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        response = await conn.request(
            "GET",
            "https://example.com/",
            headers={"Expect": "continue"},
        )
        assert response.status == 200
        assert response.content == b"<html>Hello, world! ...</html>"


@pytest.mark.anyio
async def test_http11_header_sub_100kb():
    """
    A connection should be able to handle a http header size up to 100kB.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            b"HTTP/1.1 200 OK\r\n",  # 17
            b"Content-Type: plain/text\r\n",  # 43
            b"Cookie: " + b"x" * (100 * 1024 - 72) + b"\r\n",  # 102381
            b"Content-Length: 0\r\n",  # 102400
            b"\r\n",
            b"",
        ]
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        assert response.status == 200
        assert response.content == b""


class FakeClock:
    """
    A controllable stand-in for `time.monotonic`, so keep-alive expiry can be
    observed without sleeping.
    """

    def __init__(self) -> None:
        self.now = 1000.0

    def monotonic(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


def _ok_response(
    extra_headers: typing.Optional[typing.List[typing.Tuple[bytes, bytes]]] = None,
) -> typing.List[bytes]:
    headers = list(extra_headers or [])
    lines = [b"HTTP/1.1 200 OK\r\n", b"Content-Length: 0\r\n"]
    lines.extend(name + b": " + value + b"\r\n" for name, value in headers)
    lines.append(b"\r\n")
    return lines


@pytest.mark.anyio
async def test_http11_keepalive_hint_timeout_starts_at_release(monkeypatch):
    """
    With `keepalive_hints` enabled, a `Keep-Alive: timeout=` hint bounds how
    long the connection may remain idle, counted from when the response is
    actually released rather than when its headers arrive.
    """
    clock = FakeClock()
    monkeypatch.setattr("httpcore._async.http11.time.monotonic", clock.monotonic)

    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(_ok_response([(b"Keep-Alive", b"timeout=5")]))
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_hints=True
    ) as conn:
        request = httpcore.Request(
            "GET", "https://example.com/", headers=[(b"Host", b"example.com")]
        )
        response = await conn.handle_async_request(request)

        # While the response is still held the timer has not started, so a
        # long pause here must not count against the idle timeout.
        clock.advance(100.0)
        assert not conn.is_idle()

        await response.aread()

        # Releasing the response starts the idle timer from this instant.
        await response.aclose()
        assert conn.is_idle()
        assert not conn.has_expired()

        clock.advance(4.9)
        assert not conn.has_expired()

        clock.advance(0.2)
        assert conn.has_expired()


@pytest.mark.anyio
async def test_http11_keepalive_hint_timeout_intersects_local_expiry(monkeypatch):
    """
    The effective idle timeout is the stricter of the local pool expiry and the
    backend's advertised timeout, whichever side is smaller.
    """
    clock = FakeClock()
    monkeypatch.setattr("httpcore._async.http11.time.monotonic", clock.monotonic)

    origin = httpcore.Origin(b"https", b"example.com", 443)

    # The local expiry (3s) is stricter than the advertised timeout (10s).
    stream = httpcore.AsyncMockStream(_ok_response([(b"Keep-Alive", b"timeout=10")]))
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=3.0, keepalive_hints=True
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        clock.advance(3.1)
        assert conn.has_expired()

    # The advertised timeout (2s) is stricter than the local expiry (10s).
    clock.now = 1000.0
    stream = httpcore.AsyncMockStream(_ok_response([(b"Keep-Alive", b"timeout=2")]))
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=10.0, keepalive_hints=True
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        clock.advance(2.1)
        assert conn.has_expired()


@pytest.mark.anyio
async def test_http11_keepalive_hint_max_requests_boundary():
    """
    A `Keep-Alive: max=` hint caps the number of requests the connection may
    service. Once the count reaches the advertised maximum, the connection is
    closed instead of being returned to the pool.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(_ok_response([(b"Keep-Alive", b"max=2")]) * 2)
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_hints=True
    ) as conn:
        first = await conn.request("GET", "https://example.com/")
        assert first.content == b""
        # The first request does not yet reach the advertised maximum.
        assert conn.is_idle()
        assert not conn.is_closed()

        second = await conn.request("GET", "https://example.com/")
        assert second.content == b""
        # The second request reaches the maximum, so the connection is closed.
        assert conn.is_closed()
        assert not conn.is_available()


@pytest.mark.anyio
async def test_http11_keepalive_hint_connection_close_wins():
    """
    A `Connection: close` response is closed even if it also advertises a
    permissive `Keep-Alive` hint.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        _ok_response(
            [
                (b"Connection", b"close"),
                (b"Keep-Alive", b"timeout=100, max=100"),
            ]
        )
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_hints=True
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        assert response.content == b""
        assert conn.is_closed()
        assert not conn.is_idle()


@pytest.mark.anyio
async def test_http11_keepalive_hint_unparseable_uses_local_policy(monkeypatch):
    """
    A hint that cannot be parsed is ignored, leaving the local policy in charge.
    """
    clock = FakeClock()
    monkeypatch.setattr("httpcore._async.http11.time.monotonic", clock.monotonic)

    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        _ok_response([(b"Keep-Alive", b"timeout=soon, max=lots")])
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_expiry=4.0, keepalive_hints=True
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        assert conn.is_idle()
        clock.advance(3.9)
        assert not conn.has_expired()
        clock.advance(0.2)
        assert conn.has_expired()


@pytest.mark.anyio
async def test_http11_keepalive_hint_is_scoped_to_its_response():
    """
    A `max` hint only applies to the keep-alive round of the response that
    carried it. A later response without the header clears the limit rather
    than leaving the connection bound to a stale hint.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        _ok_response([(b"Keep-Alive", b"max=2")])
        + _ok_response()
        + _ok_response()
    )
    async with httpcore.AsyncHTTP11Connection(
        origin=origin, stream=stream, keepalive_hints=True
    ) as conn:
        # The first response advertises a two-request budget (count is 1).
        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        assert conn.is_idle()

        # The second response carries no hint, so the stale `max=2` is cleared
        # and the connection is not closed even though the count is now 2.
        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        assert conn.is_idle()
        assert not conn.is_closed()

        third = await conn.request("GET", "https://example.com/")
        assert third.content == b""
        assert conn.is_idle()


@pytest.mark.anyio
async def test_http11_keepalive_hints_opt_in_only():
    """
    Without opting in, `Keep-Alive` hints have no effect and the connection is
    reused according to the local policy alone.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(_ok_response([(b"Keep-Alive", b"max=1")]) * 2)
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        assert conn.is_idle()

        response = await conn.request("GET", "https://example.com/")
        await response.aread()
        assert conn.is_idle()
        assert not conn.is_closed()


class KeepAliveHintBackend(httpcore.AsyncMockBackend):
    """
    A backend whose successive connections advertise different `Keep-Alive`
    limits, each carrying enough responses to reach its own boundary.
    """

    def __init__(self, hints: typing.List[str], responses: int) -> None:
        super().__init__([])
        self._hints = list(hints)
        self._responses = responses

    async def connect_tcp(self, *args: typing.Any, **kwargs: typing.Any) -> typing.Any:
        hint = self._hints.pop(0)
        return httpcore.AsyncMockStream(
            _ok_response([(b"Keep-Alive", hint.encode())]) * self._responses
        )


@pytest.mark.anyio
async def test_connection_pool_keepalive_hints_two_connections(monkeypatch):
    """
    Two connections advertising different request limits are reused until their
    own boundary: the `max=2` connection dies after two requests while the
    `max=3` one survives three, and a new connection is only opened when no
    idle connection can serve the request.
    """
    clock = FakeClock()
    monkeypatch.setattr("httpcore._async.http11.time.monotonic", clock.monotonic)

    network_backend = KeepAliveHintBackend(
        hints=["timeout=20, max=2", "timeout=20, max=3"], responses=5
    )

    async with httpcore.AsyncConnectionPool(
        network_backend=network_backend,
        keepalive_expiry=100.0,
        keepalive_hints=True,
    ) as pool:
        # The first request opens the "max=2" connection.
        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        first = pool.connections[0]
        assert first.is_idle()
        assert repr(first).endswith("Request Count: 1]>")

        # The second request reuses it and reaches the advertised boundary.
        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        assert first.is_closed()

        # The next request cannot reuse the dead connection and opens the
        # "max=3" connection instead.
        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        second = pool.connections[0]
        assert second is not first
        assert repr(second).endswith("Request Count: 1]>")

        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        assert second.is_idle()
        assert repr(second).endswith("Request Count: 2]>")

        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        assert second.is_closed()
        assert repr(second).endswith("Request Count: 3]>")


@pytest.mark.anyio
async def test_connection_pool_keepalive_hint_timeouts_expire_independently(monkeypatch):
    """
    Two connections advertising different idle timeouts expire at their own
    deadlines: after six idle seconds the connection that advertised `timeout=5`
    is no longer reusable, while the one that advertised `timeout=10` still is.
    """
    clock = FakeClock()
    monkeypatch.setattr("httpcore._async.http11.time.monotonic", clock.monotonic)

    network_backend = KeepAliveHintBackend(
        hints=["timeout=5", "timeout=10", "timeout=5"], responses=5
    )

    async with httpcore.AsyncConnectionPool(
        network_backend=network_backend,
        keepalive_expiry=100.0,
        keepalive_hints=True,
    ) as pool:
        # Two connections to different origins, released at the same instant
        # but advertising different idle timeouts.
        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        async with pool.stream("GET", "https://b.example.com/") as response:
            await response.aread()
        short_lived, long_lived = pool.connections
        assert short_lived.is_idle()
        assert long_lived.is_idle()

        clock.advance(6.0)

        # The `timeout=5` connection has expired and is replaced.
        async with pool.stream("GET", "https://a.example.com/") as response:
            await response.aread()
        a_origin = httpcore.Origin(b"https", b"a.example.com", 443)
        short_replacement = [
            conn for conn in pool.connections if conn.can_handle_request(a_origin)
        ][0]
        assert short_replacement is not short_lived

        # The `timeout=10` connection is still within its boundary and reused.
        async with pool.stream("GET", "https://b.example.com/") as response:
            await response.aread()
        b_origin = httpcore.Origin(b"https", b"b.example.com", 443)
        long_replacement = [
            conn for conn in pool.connections if conn.can_handle_request(b_origin)
        ][0]
        assert long_replacement is long_lived
