from __future__ import annotations

import ssl
import time
import typing

SOCKET_OPTION = typing.Union[
    typing.Tuple[int, int, int],
    typing.Tuple[int, int, typing.Union[bytes, bytearray]],
    typing.Tuple[int, int, None, int],
]

# A single pre-resolved address that may be used instead of resolving the
# hostname that the request was made against.
#
# Either a plain host (in which case the port of the request is used), or
# an explicit `(host, port)` pair.
CONNECT_CANDIDATE = typing.Union[
    str,
    bytes,
    typing.Tuple[typing.Union[str, bytes], typing.Optional[int]],
]


def candidate_addresses(
    host: str,
    port: int,
    candidates: typing.Sequence[CONNECT_CANDIDATE] | None = None,
) -> list[tuple[str, int]]:
    """
    Expand a set of connection candidates into an ordered list of `(host, port)`
    addresses that should be attempted in turn.

    Candidates are used *only* to determine where the TCP connection is made.
    The hostname that the request was made against remains the identity used for
    TLS SNI/hostname verification, and for `Origin` based connection reuse.

    If no candidates are provided then the original `(host, port)` pair is returned,
    which keeps the behavior of backends that do not support candidates unchanged.
    """
    if not candidates:
        return [(host, port)]

    addresses: list[tuple[str, int]] = []
    for candidate in candidates:
        if isinstance(candidate, (str, bytes)):
            candidate_host: str | bytes = candidate
            candidate_port: int | None = None
        else:
            candidate_host = candidate[0]
            candidate_port = candidate[1]

        if isinstance(candidate_host, bytes):
            address_host = candidate_host.decode("ascii")
        else:
            address_host = candidate_host

        address_port = port if candidate_port is None else candidate_port
        addresses.append((address_host, address_port))
    return addresses


class NetworkStream:
    def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        raise NotImplementedError()  # pragma: nocover

    def write(self, buffer: bytes, timeout: float | None = None) -> None:
        raise NotImplementedError()  # pragma: nocover

    def close(self) -> None:
        raise NotImplementedError()  # pragma: nocover

    def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> NetworkStream:
        raise NotImplementedError()  # pragma: nocover

    def get_extra_info(self, info: str) -> typing.Any:
        return None  # pragma: nocover


class NetworkBackend:
    def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
        candidates: typing.Sequence[CONNECT_CANDIDATE] | None = None,
    ) -> NetworkStream:
        raise NotImplementedError()  # pragma: nocover

    def connect_unix_socket(
        self,
        path: str,
        timeout: float | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
    ) -> NetworkStream:
        raise NotImplementedError()  # pragma: nocover

    def sleep(self, seconds: float) -> None:
        time.sleep(seconds)  # pragma: nocover


class AsyncNetworkStream:
    async def read(self, max_bytes: int, timeout: float | None = None) -> bytes:
        raise NotImplementedError()  # pragma: nocover

    async def write(self, buffer: bytes, timeout: float | None = None) -> None:
        raise NotImplementedError()  # pragma: nocover

    async def aclose(self) -> None:
        raise NotImplementedError()  # pragma: nocover

    async def start_tls(
        self,
        ssl_context: ssl.SSLContext,
        server_hostname: str | None = None,
        timeout: float | None = None,
    ) -> AsyncNetworkStream:
        raise NotImplementedError()  # pragma: nocover

    def get_extra_info(self, info: str) -> typing.Any:
        return None  # pragma: nocover


class AsyncNetworkBackend:
    async def connect_tcp(
        self,
        host: str,
        port: int,
        timeout: float | None = None,
        local_address: str | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
        candidates: typing.Sequence[CONNECT_CANDIDATE] | None = None,
    ) -> AsyncNetworkStream:
        raise NotImplementedError()  # pragma: nocover

    async def connect_unix_socket(
        self,
        path: str,
        timeout: float | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
    ) -> AsyncNetworkStream:
        raise NotImplementedError()  # pragma: nocover

    async def sleep(self, seconds: float) -> None:
        raise NotImplementedError()  # pragma: nocover
