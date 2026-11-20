from __future__ import annotations

import ssl
import typing

from .._exceptions import ReadError
from .base import (
    CONNECT_CANDIDATE,
    SOCKET_OPTION,
    AsyncNetworkBackend,
    AsyncNetworkStream,
    NetworkBackend,
    NetworkStream,
    candidate_addresses,
)


class MockSSLObject:
    def __init__(self, http2: bool):
        self._http2 = http2

    def selected_alpn_protocol(self) -> str:
        return "h2" if self._http2 else "http/1.1"


class MockStream(NetworkStream):
    def __init__(self, buffer: list[bytes], http2: bool = False) -> None:
        self._buffer = buffer
        self._http2 = http2
        self._closed = False
        # Recorded so that tests can inspect which hostname was used for the
        # TLS handshake, and which address the connection was made against.
        self.tls_server_hostname: str | None = None
        self.connect_address: tuple[str, int] | None = None

    def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        if self._closed:
            raise ReadError("Connection closed")
        if not self._buffer:
            return b""
        return self._buffer.pop(0)

    def write(self, buffer: bytes, timeout: float | None = None) -> None:
        pass

    def close(self) -> None:
        self._closed = True

    def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> NetworkStream:
        self.tls_server_hostname = server_hostname
        return self

    def get_extra_info(self, info: str) -> typing.Any:
        return MockSSLObject(http2=self._http2) if info == "ssl_object" else None

    def __repr__(self) -> str:
        return "<httpcore.MockStream>"


class MockBackend(NetworkBackend):
    def __init__(self, buffer: list[bytes], http2: bool = False) -> None:
        self._buffer = buffer
        self._http2 = http2
        # Record of the addresses that connections were attempted against,
        # allowing tests to verify candidate address handling.
        self.connect_calls: list[tuple[str, int]] = []
        self.streams: list[MockStream] = []

    def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
        candidates: typing.Sequence[CONNECT_CANDIDATE] | None = None,
    ) -> NetworkStream:
        # The mock backend always connects successfully, so any candidates are
        # reduced to the first address that would have been attempted.
        address = candidate_addresses(host, port, candidates)[0]
        self.connect_calls.append(address)
        stream = MockStream(list(self._buffer), http2=self._http2)
        stream.connect_address = address
        self.streams.append(stream)
        return stream

    def connect_unix_socket(
        self,
        path: str,
        timeout: float | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
    ) -> NetworkStream:
        return MockStream(list(self._buffer), http2=self._http2)

    def sleep(self, seconds: float) -> None:
        pass


class AsyncMockStream(AsyncNetworkStream):
    def __init__(self, buffer: list[bytes], http2: bool = False) -> None:
        self._buffer = buffer
        self._http2 = http2
        self._closed = False
        # Recorded so that tests can inspect which hostname was used for the
        # TLS handshake, and which address the connection was made against.
        self.tls_server_hostname: str | None = None
        self.connect_address: tuple[str, int] | None = None

    async def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        if self._closed:
            raise ReadError("Connection closed")
        if not self._buffer:
            return b""
        return self._buffer.pop(0)

    async def write(self, buffer: bytes, timeout: float | None = None) -> None:
        pass

    async def aclose(self) -> None:
        self._closed = True

    async def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> AsyncNetworkStream:
        self.tls_server_hostname = server_hostname
        return self

    def get_extra_info(self, info: str) -> typing.Any:
        return MockSSLObject(http2=self._http2) if info == "ssl_object" else None

    def __repr__(self) -> str:
        return "<httpcore.AsyncMockStream>"


class AsyncMockBackend(AsyncNetworkBackend):
    def __init__(self, buffer: list[bytes], http2: bool = False) -> None:
        self._buffer = buffer
        self._http2 = http2
        # Record of the addresses that connections were attempted against,
        # allowing tests to verify candidate address handling.
        self.connect_calls: list[tuple[str, int]] = []
        self.streams: list[AsyncMockStream] = []

    async def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
        candidates: typing.Sequence[CONNECT_CANDIDATE] | None = None,
    ) -> AsyncNetworkStream:
        # The mock backend always connects successfully, so any candidates are
        # reduced to the first address that would have been attempted.
        address = candidate_addresses(host, port, candidates)[0]
        self.connect_calls.append(address)
        stream = AsyncMockStream(list(self._buffer), http2=self._http2)
        stream.connect_address = address
        self.streams.append(stream)
        return stream

    async def connect_unix_socket(
        self,
        path: str,
        timeout: float | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
    ) -> AsyncNetworkStream:
        return AsyncMockStream(list(self._buffer), http2=self._http2)

    async def sleep(self, seconds: float) -> None:
        pass
