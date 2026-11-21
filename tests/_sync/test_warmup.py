import typing

import hpack
import hyperframe.frame
import pytest
from tests import concurrency

import httpcore


class CountingAsyncStream(httpcore.NetworkStream):
    """
    A mock stream that records the connect/handshake/write activity of a
    single connection, so tests can assert that warming up performs real
    network establishment without sending any HTTP bytes.
    """

    def __init__(
        self, buffer: list[bytes], backend: "CountingAsyncBackend", http2: bool = False
    ) -> None:
        self._buffer = buffer
        self._backend = backend
        self._http2 = http2
        self._closed = False

    def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        if self._closed:
            raise httpcore.ReadError("Connection closed")
        if not self._buffer:
            return b""
        return self._buffer.pop(0)

    def write(self, buffer: bytes, timeout: float | None = None) -> None:
        self._backend.writes += 1

    def close(self) -> None:
        self._closed = True

    def start_tls(
        self,
        ssl_context,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> "httpcore.NetworkStream":
        self._backend.handshakes += 1
        return self

    def get_extra_info(self, info: str) -> typing.Any:
        return (
            httpcore._backends.mock.MockSSLObject(http2=self._http2)
            if info == "ssl_object"
            else None
        )


class CountingAsyncBackend(httpcore.NetworkBackend):
    def __init__(self, buffer: list[bytes], http2: bool = False) -> None:
        self._buffer = buffer
        self._http2 = http2
        self.connects = 0
        self.handshakes = 0
        self.writes = 0
        self._gate: concurrency.Event | None = None

    def set_gate(self, gate: concurrency.Event) -> None:
        self._gate = gate

    def _make_stream(self) -> httpcore.NetworkStream:
        return CountingAsyncStream(list(self._buffer), self, http2=self._http2)

    def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: typing.Iterable[httpcore.SOCKET_OPTION] | None = None,
    ) -> httpcore.NetworkStream:
        self.connects += 1
        if self._gate is not None:
            self._gate.wait()
        return self._make_stream()

    def connect_unix_socket(
        self,
        path: str,
        timeout: float | None = None,
        socket_options: typing.Iterable[httpcore.SOCKET_OPTION] | None = None,
    ) -> httpcore.NetworkStream:
        self.connects += 1
        if self._gate is not None:
            self._gate.wait()
        return self._make_stream()

    def sleep(self, seconds: float) -> None:
        pass


HTTP_RESPONSE = [
    b"HTTP/1.1 200 OK\r\n",
    b"Content-Type: plain/text\r\n",
    b"Content-Length: 13\r\n",
    b"\r\n",
    b"Hello, world!",
]



def test_warmup_establishes_connection_without_sending_request():
    """
    Warming up performs the TCP connect and TLS handshake, but never writes
    any HTTP request bytes.
    """
    network_backend = CountingAsyncBackend(HTTP_RESPONSE)

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        assert pool.connections == []

        pool.warmup("https://example.com/")

        # A real socket was opened and the TLS handshake negotiated, but no
        # HTTP bytes were written to the upstream.
        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert network_backend.writes == 0

        # The pool now holds a single, brand-new connection for the origin.
        # It has never serviced a request, so it is still in the NEW state.
        info = [repr(c) for c in pool.connections]
        assert info == [
            "<HTTPConnection ['https://example.com:443', HTTP/1.1, NEW, Request Count: 0]>"
        ]



def test_first_request_uses_warmed_up_connection():
    """
    The first real request after a warmup reuses the ready connection:
    no second connect/handshake occurs, and the request itself is written.
    """
    network_backend = CountingAsyncBackend(HTTP_RESPONSE)

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        pool.warmup("https://example.com/")
        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert network_backend.writes == 0

        response = pool.request("GET", "https://example.com/")

        assert response.status == 200
        assert response.content == b"Hello, world!"

        # The warmed-up connection was reused rather than re-established.
        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        # The headers, the body and the end-of-message marker are each
        # written separately by the HTTP/1.1 layer.
        assert network_backend.writes == 3

        info = [repr(c) for c in pool.connections]
        assert info == [
            "<HTTPConnection ['https://example.com:443', HTTP/1.1, IDLE, Request Count: 1]>"
        ]



def test_warmup_is_idempotent_for_an_existing_connection():
    """
    Repeated warmups to an origin that already has a connection are no-ops:
    no additional connection is opened.
    """
    network_backend = CountingAsyncBackend(HTTP_RESPONSE)

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        pool.warmup("https://example.com/")
        pool.warmup("https://example.com/")
        pool.warmup("https://example.com/")

        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert network_backend.writes == 0
        assert len(pool.connections) == 1



def test_http2_warmup_establishes_connection_without_sending_request():
    """
    Under negotiated HTTP/2, warming up builds the HTTP/2 connection (which
    sends its preface lazily on the first request) without writing any bytes.
    The first request then reuses the established connection.
    """
    buffer = [
        hyperframe.frame.SettingsFrame().serialize(),
        hyperframe.frame.HeadersFrame(
            stream_id=1,
            data=hpack.Encoder().encode([(b":status", b"200")]),
            flags=["END_HEADERS"],
        ).serialize(),
        hyperframe.frame.DataFrame(
            stream_id=1, data=b"Hello, world!", flags=["END_STREAM"]
        ).serialize(),
    ]
    network_backend = CountingAsyncBackend(buffer, http2=True)

    with httpcore.ConnectionPool(
        network_backend=network_backend, http2=True
    ) as pool:
        pool.warmup("https://example.com/")

        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert network_backend.writes == 0
        info = [repr(c) for c in pool.connections]
        assert info == [
            "<HTTPConnection ['https://example.com:443', HTTP/2, IDLE, Request Count: 0]>"
        ]

        response = pool.request("GET", "https://example.com/")
        assert response.status == 200

        # The request reused the warmed-up connection rather than reconnecting.
        assert network_backend.connects == 1
        assert network_backend.handshakes == 1



def test_concurrent_warmups_open_a_single_connection():
    """
    Several warmups for the same origin arriving together are deduplicated:
    the later callers join the in-flight warmup rather than each opening
    their own connection.
    """
    gate = concurrency.Event()
    network_backend = CountingAsyncBackend(HTTP_RESPONSE)
    network_backend.set_gate(gate)

    def warm(pool) -> None:
        pool.warmup("https://example.com/")

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        with concurrency.open_nursery() as nursery:
            for _ in range(5):
                nursery.start_soon(warm, pool)
            # Give every warmup a chance to reach the gated connect. The
            # first call blocks on the gate, while the rest join its warmup.
            concurrency.sleep(0.1)
            gate.set()

        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert network_backend.writes == 0
        assert len(pool.connections) == 1



def test_warmup_respects_pool_capacity():
    """
    A warmup never causes the pool to exceed `max_connections`. When the
    pool is full it waits for a slot, raising a pool timeout if none frees
    within the configured pool timeout.
    """
    network_backend = CountingAsyncBackend(HTTP_RESPONSE * 3)

    with httpcore.ConnectionPool(
        network_backend=network_backend, max_connections=1
    ) as pool:
        # Occupy the single connection with an in-flight request.
        with pool.stream("GET", "https://example.com/"):
            with pytest.raises(httpcore.PoolTimeout):
                pool.warmup(
                    "https://other.example.com/",
                    extensions={"timeout": {"pool": 0.05}},
                )

        # Only the request's connection was ever established.
        assert network_backend.connects == 1



def test_warmup_reclaims_idle_connection_when_pool_full():
    """
    When the pool is full but one of the connections is idle, a warmup for a
    different origin may close it to make room, while staying within the
    pool's capacity.
    """
    network_backend = CountingAsyncBackend(HTTP_RESPONSE * 2)

    with httpcore.ConnectionPool(
        network_backend=network_backend, max_connections=1
    ) as pool:
        response = pool.request("GET", "https://a.example.com/")
        assert response.status == 200
        assert len(pool.connections) == 1

        pool.warmup("https://b.example.com/")

        # Still only ever one connection at a time.
        assert len(pool.connections) == 1
        assert network_backend.connects == 2
        info = [repr(c) for c in pool.connections]
        assert info == [
            "<HTTPConnection ['https://b.example.com:443', HTTP/1.1, NEW, Request Count: 0]>"
        ]



def test_warmup_failure_is_removed_from_pool():
    """
    If establishing a warmed-up connection fails, the half-open connection
    is removed from the pool and the error propagates to the warmup caller.
    """

    class FailedConnectBackend(CountingAsyncBackend):
        def connect_tcp(self, *args, **kwargs):
            self.connects += 1
            raise httpcore.ConnectError("Could not connect")

    network_backend = FailedConnectBackend([])

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        with pytest.raises(httpcore.ConnectError):
            pool.warmup("https://example.com/")

        assert pool.connections == []
        assert network_backend.writes == 0

        # A later request is not blocked by the failed warmup and retries
        # the connection through the normal flow.
        with pytest.raises(httpcore.ConnectError):
            pool.request("GET", "https://example.com/")



def test_joined_warmup_failure_does_not_fail_request():
    """
    A request arriving while a warmup for its origin is in flight joins the
    warmup. If that warmup fails, the request still proceeds with its own
    connection attempt rather than inheriting the warmup's exception.
    """
    attempts = 0

    class FlakyBackend(CountingAsyncBackend):
        def connect_tcp(self, *args, **kwargs):
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                raise httpcore.ConnectError("warmup failed")
            return super().connect_tcp(*args, **kwargs)

    network_backend = FlakyBackend(HTTP_RESPONSE)

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        with pytest.raises(httpcore.ConnectError):
            pool.warmup("https://example.com/")

        response = pool.request("GET", "https://example.com/")
        assert response.status == 200
        assert response.content == b"Hello, world!"



def test_connection_aconnect_does_not_send_bytes():
    """
    The connection-level `connect()` performs the connect and handshake
    but writes nothing, and a following request reuses the established
    connection.
    """
    network_backend = CountingAsyncBackend(HTTP_RESPONSE)
    origin = httpcore.URL("https://example.com/").origin

    connection = httpcore.HTTPConnection(
        origin=origin, network_backend=network_backend
    )
    try:
        connection.connect()

        assert network_backend.connects == 1
        assert network_backend.handshakes == 1
        assert network_backend.writes == 0
        assert not connection.is_closed()

        # A second connect is a no-op.
        connection.connect()
        assert network_backend.connects == 1

        request = httpcore.Request(
            "GET",
            "https://example.com/",
            headers={"Host": "example.com"},
        )
        response = connection.handle_request(request)
        response.read()
        assert response.status == 200
        assert network_backend.connects == 1
        assert network_backend.writes == 3
    finally:
        connection.close()



def test_warmup_unsupported_protocol():
    network_backend = CountingAsyncBackend(HTTP_RESPONSE)

    with httpcore.ConnectionPool(
        network_backend=network_backend,
    ) as pool:
        with pytest.raises(httpcore.UnsupportedProtocol):
            pool.warmup("ftp://example.com/")
        with pytest.raises(httpcore.UnsupportedProtocol):
            pool.warmup("://example.com/")
