from __future__ import annotations

import math
import select
import socket
import sys
import typing


def is_socket_readable(sock: socket.socket | None) -> bool:
    """
    Return whether a socket, as identifed by its file descriptor, is readable.
    "A socket is readable" means that the read buffer isn't empty, i.e. that calling
    .recv() on it would immediately return some data.
    """
    # NOTE: we want check for readability without actually attempting to read, because
    # we don't want to block forever if it's not readable.

    # In the case that the socket no longer exists, or cannot return a file
    # descriptor, we treat it as being readable, as if it the next read operation
    # on it is ready to return the terminating `b""`.
    sock_fd = None if sock is None else sock.fileno()
    if sock_fd is None or sock_fd < 0:  # pragma: nocover
        return True

    # The implementation below was stolen from:
    # https://github.com/python-trio/trio/blob/20ee2b1b7376db637435d80e266212a35837ddcc/trio/_socket.py#L471-L478
    # See also: https://github.com/encode/httpcore/pull/193#issuecomment-703129316

    # Use select.select on Windows, and when poll is unavailable and select.poll
    # everywhere else. (E.g. When eventlet is in use. See #327)
    if (
        sys.platform == "win32" or getattr(select, "poll", None) is None
    ):  # pragma: nocover
        rready, _, _ = select.select([sock_fd], [], [], 0)
        return bool(rready)
    p = select.poll()
    p.register(sock_fd, select.POLLIN)
    return bool(p.poll(0))


class KeepAliveHint(typing.NamedTuple):
    """
    The limits advertised by an HTTP/1 backend in a `Keep-Alive` response header.

    * `timeout` is the number of seconds the backend is willing to keep the
      connection open for while it is idle.
    * `max_requests` is the maximum number of requests the backend is willing to
      service on the connection before closing it.

    Either value may be `None`, indicating that the backend did not advertise
    that particular limit, or that it could not be parsed.
    """

    timeout: float | None = None
    max_requests: int | None = None


def parse_keep_alive_header(
    headers: typing.Sequence[tuple[bytes, bytes]],
) -> KeepAliveHint:
    """
    Parse the parameters advertised by any `Keep-Alive` response headers.

    The header syntax is described by RFC 2068 section 19.7.1.1:

        Keep-Alive: timeout=5, max=100

    Only the `timeout` and `max` parameters are meaningful here. Unrecognised
    parameters are ignored, and a value that cannot be parsed is treated as if
    it were absent. That ensures a malformed hint leaves the client's existing
    keep-alive policy in charge, rather than raising an error while handling a
    response.

    If the header is repeated, the strictest (smallest) value for each of the
    two parameters is used.
    """
    timeout: float | None = None
    max_requests: int | None = None

    for name, value in headers:
        if name.lower() != b"keep-alive":
            continue

        for parameter in value.split(b","):
            key, separator, raw_value = parameter.partition(b"=")
            if not separator:
                continue
            key = key.strip().lower()
            raw_value = raw_value.strip()

            if key == b"timeout":
                try:
                    parsed_timeout = float(raw_value)
                except ValueError:
                    continue
                # Reject negative or non-finite values such as `nan`/`inf`.
                if not math.isfinite(parsed_timeout) or parsed_timeout < 0:
                    continue
                timeout = (
                    parsed_timeout if timeout is None else min(timeout, parsed_timeout)
                )
            elif key == b"max":
                try:
                    parsed_max = int(raw_value)
                except ValueError:
                    continue
                if parsed_max < 0:
                    continue
                max_requests = (
                    parsed_max if max_requests is None else min(max_requests, parsed_max)
                )

    return KeepAliveHint(timeout=timeout, max_requests=max_requests)
