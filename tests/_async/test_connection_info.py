import ssl
import typing

import hpack
import hyperframe.frame
import pytest

import httpcore

REMOTE_ADDR = ("93.184.216.34", 443)
LOCAL_ADDR = ("192.168.1.10", 51234)


class AddressableMockStream(httpcore.AsyncMockStream):
    """
    A mock stream that also exposes socket addresses, so the diagnostic
    snapshot has an outgoing address to record. Unknown extra info (such as
    `"is_readable"`) keeps the base mock behaviour of returning `None`.
    """

    def __init__(
        self,
        buffer: typing.List[bytes],
        http2: bool = False,
        local_addr: typing.Any = None,
        remote_addr: typing.Any = None,
    ) -> None:
        super().__init__(buffer, http2=http2)
        self._local_addr = local_addr
        self._remote_addr = remote_addr

    def get_extra_info(self, info: str) -> typing.Any:
        if info == "client_addr":
            return self._local_addr
        if info == "server_addr":
            return self._remote_addr
        return super().get_extra_info(info)


class PlainStream(httpcore.AsyncNetworkStream):
    """
    A network stream that exposes neither TLS information nor socket
    addresses, so the diagnostic snapshot must leave those fields unknown.
    """

    def __init__(self, buffer: typing.List[bytes]) -> None:
        self._buffer = list(buffer)
        self._closed = False

    async def read(
        self, max_bytes: int, timeout: typing.Optional[float] = None
    ) -> bytes:
        if self._closed:
            raise httpcore.ReadError("Connection closed")
        if not self._buffer:
            return b""
        return self._buffer.pop(0)

    async def write(
        self, buffer: bytes, timeout: typing.Optional[float] = None
    ) -> None:
        pass

    async def aclose(self) -> None:
        self._closed = True

    async def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: typing.Optional[str] = None,
        timeout: typing.Optional[float] = None,
    ) -> "PlainStream":
        return self

    def get_extra_info(self, info: str) -> typing.Any:
        # Match the base mock stream, which reports unknown extra information
        # (including `"is_readable"`) as `None`.
        return None


def http11_buffer() -> typing.List[bytes]:
    # A fresh buffer per stream, since a stream consumes it while reading.
    return [
        b"HTTP/1.1 200 OK\r\n",
        b"Content-Type: plain/text\r\n",
        b"Content-Length: 13\r\n",
        b"\r\n",
        b"Hello, world!",
    ]


def http11_stream(
    *,
    http2: bool = False,
    remote_addr: typing.Any = REMOTE_ADDR,
    local_addr: typing.Any = LOCAL_ADDR,
) -> AddressableMockStream:
    return AddressableMockStream(
        http11_buffer(),
        http2=http2,
        remote_addr=remote_addr,
        local_addr=local_addr,
    )


@pytest.mark.anyio
async def test_http11_connection_info_tls() -> None:
    """
    An HTTPS HTTP/1.1 response snapshots the outgoing address and the
    negotiated ALPN protocol. HTTP/1.1 has no stream identifier.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = http11_stream(http2=False)
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        response = await conn.request("GET", "https://example.com/")

    # The snapshot stays available after the response is read and closed,
    # even though the underlying connection may have been returned to a pool.
    info = response.extensions["connection_info"]
    assert info.remote_addr == REMOTE_ADDR
    assert info.alpn_protocol == "http/1.1"
    assert info.stream_id is None

    # The actionable stream handle is still exposed unchanged.
    assert response.extensions["network_stream"] is stream


@pytest.mark.anyio
async def test_http11_connection_info_unknowns() -> None:
    """
    Values the network stream does not provide stay unknown, rather than
    being guessed at.
    """
    origin = httpcore.Origin(b"http", b"example.com", 80)
    stream = PlainStream(http11_buffer())
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        response = await conn.request("GET", "http://example.com/")

    info = response.extensions["connection_info"]
    assert info.remote_addr is None
    assert info.alpn_protocol is None
    assert info.stream_id is None


@pytest.mark.anyio
async def test_http2_connection_info_tracks_each_stream() -> None:
    """
    Two HTTP/2 responses multiplexed over a single connection each keep their
    own snapshot. Closing the first response before inspecting them must not
    erase the per-stream identifiers.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    encoder = hpack.Encoder()
    stream = AddressableMockStream(
        [
            hyperframe.frame.SettingsFrame().serialize(),
            hyperframe.frame.HeadersFrame(
                stream_id=1,
                data=encoder.encode(
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
            hyperframe.frame.HeadersFrame(
                stream_id=3,
                data=encoder.encode(
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
        ],
        http2=True,
        remote_addr=REMOTE_ADDR,
        local_addr=LOCAL_ADDR,
    )

    async with httpcore.AsyncHTTP2Connection(origin=origin, stream=stream) as conn:
        first = await conn.request("GET", "https://example.com/")
        second = await conn.request("GET", "https://example.com/")

        # Read and release the first response before inspecting either view.
        await first.aread()
        await first.aclose()

        first_info = first.extensions["connection_info"]
        second_info = second.extensions["connection_info"]

        assert first_info.stream_id == 1
        assert second_info.stream_id == 3
        assert first_info.alpn_protocol == "h2"
        assert second_info.alpn_protocol == "h2"
        assert first_info.remote_addr == REMOTE_ADDR
        assert second_info.remote_addr == REMOTE_ADDR

        # Both views are distinct snapshots, not views onto shared live state.
        assert first_info is not second_info


@pytest.mark.anyio
async def test_connection_info_is_read_only() -> None:
    """
    The diagnostic view is an immutable snapshot.
    """
    origin = httpcore.Origin(b"https", b"example.com", 443)
    stream = http11_stream(http2=False)
    async with httpcore.AsyncHTTP11Connection(origin=origin, stream=stream) as conn:
        response = await conn.request("GET", "https://example.com/")

    info: httpcore.ConnectionInfo = response.extensions["connection_info"]
    assert isinstance(info, httpcore.ConnectionInfo)

    # A NamedTuple cannot be mutated in place...
    with pytest.raises(AttributeError):
        object.__setattr__(info, "stream_id", 7)

    # ...and "modifying" one yields a separate instance instead.
    changed = info._replace(stream_id=7)
    assert changed.stream_id == 7
    assert info.stream_id is None

    assert repr(info) == (
        "ConnectionInfo(remote_addr=('93.184.216.34', 443), "
        "alpn_protocol='http/1.1', stream_id=None)"
    )
