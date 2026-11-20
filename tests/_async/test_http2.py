import time
import typing

import hpack
import hyperframe.frame
import pytest

import httpcore


class FakeClock:
    """
    A fake clock, allowing tests to control the progression of time.
    """

    def __init__(self) -> None:
        self._now = 0.0

    def monotonic(self) -> float:
        return self._now

    def advance(self, seconds: float) -> None:
        self._now += seconds


class ScheduledStream(httpcore.AsyncNetworkStream):
    """
    A fake network stream that returns data according to a fixed schedule
    of `(due, data)` tuples, driven by a fake clock.

    Reads return any scheduled data that is due before the read deadline,
    advancing the clock as required. If no data is scheduled before the
    deadline, then the clock is advanced to the deadline, and a
    `ReadTimeout` is raised.
    """

    def __init__(
        self, clock: FakeClock, schedule: typing.List[typing.Tuple[float, bytes]]
    ) -> None:
        self._clock = clock
        self._schedule = list(schedule)

    async def read(
        self, max_bytes: int, timeout: typing.Optional[float] = None
    ) -> bytes:
        now = self._clock.monotonic()
        if self._schedule:
            due, data = self._schedule[0]
            if timeout is None or due <= now + timeout:
                self._clock.advance(max(due - now, 0.0))
                self._schedule.pop(0)
                return data
        assert timeout is not None
        self._clock.advance(timeout)
        raise httpcore.ReadTimeout("Read timed out.")

    async def write(
        self, buffer: bytes, timeout: typing.Optional[float] = None
    ) -> None:
        pass

    async def aclose(self) -> None:
        pass


@pytest.mark.anyio
async def test_http2_connection():
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=1, data=b"Hello, world!", flags=["END_STREAM"]
            ).serialize(),
        ]
    )
    async with httpcore.AsyncHTTP2Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        response = await conn.request("GET", "https://example.com/")
        assert response.status == 200
        assert response.content == b"Hello, world!"

        assert conn.is_idle()
        assert conn.is_available()
        assert not conn.is_closed()
        assert not conn.has_expired()
        assert (
            conn.info() == "'https://example.com:443', HTTP/2, IDLE, Request Count: 1"
        )
        assert (
            repr(conn)
            == "<AsyncHTTP2Connection ['https://example.com:443', IDLE, Request Count: 1]>"
        )


@pytest.mark.anyio
async def test_http2_connection_closed():
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=1, data=b"Hello, world!", flags=["END_STREAM"]
            ).serialize(),
            # Connection is closed after the first response
            hyperframe.frame.GoAwayFrame(
                stream_id=0, error_code=0, last_stream_id=1
            ).serialize(),
        ]
    )
    async with httpcore.AsyncHTTP2Connection(
        origin=origin, stream=stream, keepalive_expiry=5.0
    ) as conn:
        await conn.request("GET", "https://example.com/")

        with pytest.raises(httpcore.ConnectionNotAvailable):
            await conn.request("GET", "https://example.com/")

        assert not conn.is_available()


@pytest.mark.anyio
async def test_http2_connection_post_request():
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=1, data=b"Hello, world!", flags=["END_STREAM"]
            ).serialize(),
        ]
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        response = await conn.request(
            "POST",
            "https://example.com/",
            headers={b"content-length": b"17"},
            content=b'{"data": "upload"}',
        )
        assert response.status == 200
        assert response.content == b"Hello, world!"


@pytest.mark.anyio
async def test_http2_connection_with_remote_protocol_error():
    """
    If a remote protocol error occurs, then no response will be returned,
    and the connection will not be reusable.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream([b"Wait, this isn't valid HTTP!", b""])
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(httpcore.RemoteProtocolError):
            await conn.request("GET", "https://example.com/")


@pytest.mark.anyio
async def test_http2_connection_with_rst_stream():
    """
    If a stream reset occurs, then no response will be returned,
    but the connection will remain reusable for other requests.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            # Stream is closed midway through the first response...
            hyperframe.frame.RstStreamFrame(stream_id=1, error_code=8).serialize(),
            # ...Which doesn't prevent the second response.
            hyperframe.frame.HeadersFrame(
                stream_id=3,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=3, data=b"Hello, world!", flags=["END_STREAM"]
            ).serialize(),
            b"",
        ]
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(httpcore.RemoteProtocolError):
            await conn.request("GET", "https://example.com/")
        response = await conn.request("GET", "https://example.com/")
        assert response.status == 200


@pytest.mark.anyio
async def test_http2_connection_with_goaway():
    """
    If a GoAway frame occurs, then no response will be returned,
    and the connection will not be reusable for other requests.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            # Connection is closed midway through the first response...
            hyperframe.frame.GoAwayFrame(stream_id=0, error_code=0).serialize(),
            # ...We'll never get to this second response.
            hyperframe.frame.HeadersFrame(
                stream_id=3,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=3, data=b"Hello, world!", flags=["END_STREAM"]
            ).serialize(),
            b"",
        ]
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        # The initial request has been closed midway, with an unrecoverable error.
        with pytest.raises(httpcore.RemoteProtocolError):
            await conn.request("GET", "https://example.com/")

        # The second request can receive a graceful `ConnectionNotAvailable`,
        # and may be retried on a new connection.
        with pytest.raises(httpcore.ConnectionNotAvailable):
            await conn.request("GET", "https://example.com/")


@pytest.mark.anyio
async def test_http2_connection_with_flow_control():
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            # Available flow: 65,535
            hyperframe.frame.WindowUpdateFrame(
                stream_id=0, window_increment=10_000
            ).serialize(),
            hyperframe.frame.WindowUpdateFrame(
                stream_id=1, window_increment=10_000
            ).serialize(),
            # Available flow: 75,535
            hyperframe.frame.WindowUpdateFrame(
                stream_id=0, window_increment=10_000
            ).serialize(),
            hyperframe.frame.WindowUpdateFrame(
                stream_id=1, window_increment=10_000
            ).serialize(),
            # Available flow: 85,535
            hyperframe.frame.WindowUpdateFrame(
                stream_id=0, window_increment=10_000
            ).serialize(),
            hyperframe.frame.WindowUpdateFrame(
                stream_id=1, window_increment=10_000
            ).serialize(),
            # Available flow: 95,535
            hyperframe.frame.WindowUpdateFrame(
                stream_id=0, window_increment=10_000
            ).serialize(),
            hyperframe.frame.WindowUpdateFrame(
                stream_id=1, window_increment=10_000
            ).serialize(),
            # Available flow: 105,535
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=1, data=b"100,000 bytes received", flags=["END_STREAM"]
            ).serialize(),
        ]
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        response = await conn.request(
            "POST",
            "https://example.com/",
            content=b"x" * 100_000,
        )
        assert response.status == 200
        assert response.content == b"100,000 bytes received"


@pytest.mark.anyio
async def test_http2_connection_attempt_close():
    """
    A connection can only be closed when it is idle.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=1, data=b"Hello, world!", flags=["END_STREAM"]
            ).serialize(),
        ]
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        async with conn.stream("GET", "https://example.com/") as response:
            await response.aread()
            assert response.status == 200
            assert response.content == b"Hello, world!"

        await conn.aclose()
        with pytest.raises(httpcore.ConnectionNotAvailable):
            await conn.request("GET", "https://example.com/")


@pytest.mark.anyio
async def test_http2_request_to_incorrect_origin():
    """
    A connection can only send requests to whichever origin it is connected to.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream([])
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        with pytest.raises(RuntimeError):
            await conn.request("GET", "https://other.com/")


@pytest.mark.anyio
async def test_http2_remote_max_streams_update():
    """
    If the remote server updates the maximum concurrent streams value, we should
    be adjusting how many streams we will allow.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = httpcore.AsyncMockStream(
        [
            hyperframe.frame.SettingsFrame(
                settings={hyperframe.frame.SettingsFrame.MAX_CONCURRENT_STREAMS: 1000}
            ).serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=hpack.Encoder().encode(
                    [
                        (b":status", b"200"),
                        (b"content-type", b"plain/text"),
                    ]
                ),
                flags=["END_HEADERS"],
            ).serialize(),
            hyperframe.frame.DataFrame(stream_id=1, data=b"Hello, world!").serialize(),
            hyperframe.frame.SettingsFrame(
                settings={hyperframe.frame.SettingsFrame.MAX_CONCURRENT_STREAMS: 50}
            ).serialize(),
            hyperframe.frame.DataFrame(
                stream_id=1, data=b"Hello, world...again!", flags=["END_STREAM"]
            ).serialize(),
        ]
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        async with conn.stream("GET", "https://example.com/") as response:
            i = 0
            async for chunk in response.aiter_stream():
                if i == 0:
                    assert chunk == b"Hello, world!"
                    assert conn._h2_state.remote_settings.max_concurrent_streams == 1000
                    assert conn._max_streams == min(
                        conn._h2_state.remote_settings.max_concurrent_streams,
                        conn._h2_state.local_settings.max_concurrent_streams,
                    )
                elif i == 1:
                    assert chunk == b"Hello, world...again!"
                    assert conn._h2_state.remote_settings.max_concurrent_streams == 50
                    assert conn._max_streams == min(
                        conn._h2_state.remote_settings.max_concurrent_streams,
                        conn._h2_state.local_settings.max_concurrent_streams,
                    )
                i += 1


def server_settings() -> bytes:
    return hyperframe.frame.SettingsFrame(
        settings={hyperframe.frame.SettingsFrame.MAX_CONCURRENT_STREAMS: 1000}
    ).serialize()


def response_headers(stream_id: int) -> bytes:
    return hyperframe.frame.HeadersFrame(
        stream_id=stream_id,
        data=hpack.Encoder().encode(
            [
                (b":status", b"200"),
                (b"content-type", b"plain/text"),
            ]
        ),
        flags=["END_HEADERS"],
    ).serialize()


def response_data(stream_id: int, data: bytes, end_stream: bool = False) -> bytes:
    return hyperframe.frame.DataFrame(
        stream_id=stream_id, data=data, flags=["END_STREAM"] if end_stream else []
    ).serialize()


@pytest.mark.anyio
async def test_http2_read_timeout_is_a_per_stream_idle_timeout(monkeypatch):
    """
    The read timeout on an HTTP/2 connection is applied as a per-stream
    idle timeout, calculated from the most recent activity on each stream.

    A stream that is not making progress times out, even if other streams
    on the same connection are still transferring data. The timeout only
    fails the idle request, and the connection remains usable.
    """
    clock = FakeClock()
    monkeypatch.setattr(time, "monotonic", clock.monotonic)

    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = ScheduledStream(
        clock,
        schedule=[
            (0.0, server_settings()),
            (1.0, response_headers(stream_id=1)),
            (2.0, response_headers(stream_id=3)),
            (3.0, response_data(stream_id=1, data=b"Hello, ")),
            (4.0, response_data(stream_id=1, data=b"world!", end_stream=True)),
            # The response body for stream 3 never arrives.
            # A final response is included to demonstrate that the
            # connection remains usable after the read timeout.
            (8.0, response_headers(stream_id=5)),
            (9.0, response_data(stream_id=5, data=b"Again.", end_stream=True)),
        ],
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        async with conn.stream(
            "GET",
            "https://example.com/fast",
            extensions={"timeout": {"read": 5.0}},
        ) as fast_response:
            async with conn.stream(
                "GET",
                "https://example.com/slow",
                extensions={"timeout": {"read": 5.0}},
            ) as slow_response:
                # The slow stream sees no further activity after its response
                # headers arrive at t=2. The connection reads at t=3 and t=4
                # are woken by data for the fast stream, but the idle deadline
                # for the slow stream is still calculated from its own most
                # recent activity, so it times out at t=2+5=7.
                with pytest.raises(httpcore.ReadTimeout):
                    await slow_response.aread()
                assert clock.monotonic() == 7.0

                # The fast stream is unaffected, and reads to completion.
                assert await fast_response.aread() == b"Hello, world!"

        # A per-stream read timeout does not indicate a connection failure.
        assert conn.is_available()
        assert not conn.is_closed()

        # The connection remains usable for any further requests.
        response = await conn.request(
            "GET",
            "https://example.com/afterwards",
            extensions={"timeout": {"read": 5.0}},
        )
        assert response.status == 200
        assert response.content == b"Again."


@pytest.mark.anyio
async def test_http2_read_timeout_not_reset_by_other_streams(monkeypatch):
    """
    The deadline for a per-stream read timeout is calculated from the most
    recent activity on that stream. Connection reads that are woken by
    activity on other streams do not reset the deadline.
    """
    clock = FakeClock()
    monkeypatch.setattr(time, "monotonic", clock.monotonic)

    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = ScheduledStream(
        clock,
        schedule=[
            (0.0, server_settings()),
            (1.0, response_headers(stream_id=1)),
            (2.0, response_headers(stream_id=3)),
            (3.0, response_data(stream_id=1, data=b"a")),
            (4.0, response_data(stream_id=1, data=b"b")),
            (5.0, response_data(stream_id=1, data=b"c")),
            (6.0, response_data(stream_id=1, data=b"d")),
            (7.0, response_data(stream_id=1, data=b"e", end_stream=True)),
            # The response body for stream 3 never arrives.
        ],
    )
    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        async with conn.stream(
            "GET",
            "https://example.com/fast",
            extensions={"timeout": {"read": 5.0}},
        ) as fast_response:
            async with conn.stream(
                "GET",
                "https://example.com/slow",
                extensions={"timeout": {"read": 5.0}},
            ) as slow_response:
                # Each connection read up until t=7 is woken by activity on
                # the fast stream. Once the slow stream has been idle for the
                # full 5 second read timeout it fails immediately, at t=2+5=7,
                # without requiring any further network activity.
                with pytest.raises(httpcore.ReadTimeout):
                    await slow_response.aread()
                assert clock.monotonic() == 7.0

                # The fast stream is unaffected, and reads to completion.
                assert await fast_response.aread() == b"abcde"
