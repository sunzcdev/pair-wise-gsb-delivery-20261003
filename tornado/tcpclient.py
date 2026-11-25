#
# Copyright 2014 Facebook
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may
# not use this file except in compliance with the License. You may obtain
# a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
# WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
# License for the specific language governing permissions and limitations
# under the License.

"""A non-blocking TCP connection factory."""

import asyncio
import datetime
import functools
import numbers
import socket
import ssl
from collections.abc import Callable, Iterator
from typing import Any, Tuple

from tornado import gen
from tornado.concurrent import Future, future_add_done_callback
from tornado.ioloop import IOLoop
from tornado.iostream import IOStream
from tornado.netutil import Resolver

_INITIAL_CONNECT_TIMEOUT = 0.3


def _format_addr(addr: Any) -> str:
    """Format a socket address for use in error messages.

    IPv4 addresses are formatted as ``1.2.3.4:443`` and IPv6 addresses as
    ``[::1]:443``. Anything else is formatted with `str`.
    """
    if isinstance(addr, tuple) and len(addr) >= 2:
        host, port = addr[:2]
        if isinstance(host, str) and ":" in host:
            return f"[{host}]:{port}"
        return f"{host}:{port}"
    return str(addr)


class _Connector:
    """A stateless implementation of the "Happy Eyeballs" algorithm.

    "Happy Eyeballs" is documented in RFC6555 as the recommended practice
    for when both IPv4 and IPv6 addresses are available.

    In this implementation, we partition the addresses by family, and
    make the first connection attempt to whichever address was
    returned first by ``getaddrinfo``.  If that connection fails or
    times out, we begin a connection in parallel to the first address
    of the other family.  If there are additional failures we retry
    with other addresses, keeping one connection attempt per family
    in flight at a time.

    http://tools.ietf.org/html/rfc6555

    The ``connect`` callable returns a ``(closable, future)`` pair for
    each attempt.  The future completes with the connected stream once
    it is ready for use (including any TLS handshake), so the race is
    won by the first fully usable connection rather than simply the
    first TCP connection to be established.  Closing the closable
    abandons the attempt at any stage.  If the closable has a boolean
    ``handshaking`` attribute, it is used to distinguish the TCP and
    TLS phases of the attempt in timeout error messages.

    """

    def __init__(
        self,
        addrinfo: list[tuple[Any, ...]],
        connect: Callable[
            [socket.AddressFamily, tuple[Any, ...]], tuple[Any, "Future[IOStream]"]
        ],
    ) -> None:
        self.io_loop = IOLoop.current()
        self.connect = connect

        self.future: Future[tuple[socket.AddressFamily, Any, IOStream]] = Future()
        self.timeout: object | None = None
        self.connect_timeout: object | None = None
        self.last_error: Exception | None = None
        self.remaining = len(addrinfo)
        self.primary_addrs, self.secondary_addrs = self.split(addrinfo)
        self.streams: set[Any] = set()
        # Addresses with connection attempts in progress, mapped to the
        # closable returned by ``connect`` for that attempt.  Used for
        # error messages and to remove the winning attempt from
        # ``self.streams``.
        self.pending: dict[tuple[Any, ...], Any] = {}

    @staticmethod
    def split(
        addrinfo: list[tuple[Any, ...]],
    ) -> tuple[
        list[tuple[socket.AddressFamily, tuple[Any, ...]]],
        list[tuple[socket.AddressFamily, tuple[Any, ...]]],
    ]:
        """Partition the ``addrinfo`` list by address family.

        Returns two lists.  The first list contains the first entry from
        ``addrinfo`` and all others with the same family, and the
        second list contains all other addresses (normally one list will
        be AF_INET and the other AF_INET6, although non-standard resolvers
        may return additional families).
        """
        primary = []
        secondary = []
        primary_af = addrinfo[0][0]
        for af, addr in addrinfo:
            if af == primary_af:
                primary.append((af, addr))
            else:
                secondary.append((af, addr))
        return primary, secondary

    def start(
        self,
        timeout: float = _INITIAL_CONNECT_TIMEOUT,
        connect_timeout: float | datetime.timedelta | None = None,
    ) -> "Future[Tuple[socket.AddressFamily, Any, IOStream]]":
        # Set the timeouts first: try_connect may fail synchronously, and
        # on_connect_done will then skip ahead to the secondary addresses.
        self.set_timeout(timeout)
        if connect_timeout is not None:
            self.set_connect_timeout(connect_timeout)
        self.try_connect(iter(self.primary_addrs))
        future_add_done_callback(self.future, self.on_future_done)
        return self.future

    def try_connect(
        self, addrs: Iterator[tuple[socket.AddressFamily, tuple[Any, ...]]]
    ) -> None:
        try:
            af, addr = next(addrs)
        except StopIteration:
            # We've reached the end of our queue, but the other queue
            # might still be working.  Send a final error on the future
            # only when both queues are finished.
            if self.remaining == 0 and not self.future.done():
                self.future.set_exception(
                    self.last_error or IOError("connection failed")
                )
                # Every attempt has finished, but a cancelled attempt's
                # stream may still be open.
                self.close_streams()
            return
        try:
            stream, future = self.connect(af, addr)
        except Exception as e:
            # Report synchronous errors (e.g. an address family that is
            # not supported on this system) through the same path as
            # asynchronous ones, so we move on to the next address.
            future = Future()
            future.set_exception(e)
        else:
            self.streams.add(stream)
            self.pending[addr] = stream
        future_add_done_callback(
            future, functools.partial(self.on_connect_done, addrs, af, addr)
        )

    def on_connect_done(
        self,
        addrs: Iterator[tuple[socket.AddressFamily, tuple[Any, ...]]],
        af: socket.AddressFamily,
        addr: tuple[Any, ...],
        future: "Future[IOStream]",
    ) -> None:
        self.remaining -= 1
        attempt = self.pending.pop(addr, None)
        try:
            stream = future.result()
        except (Exception, asyncio.CancelledError) as e:
            if self.future.done():
                return
            # Error: try again (but remember what happened so we have an
            # error to raise in the end). A cancelled attempt is treated
            # as a failure, but is not reported as the error because
            # our caller has not been cancelled.
            if not isinstance(e, asyncio.CancelledError):
                self.last_error = e
            self.try_connect(addrs)
            if self.timeout is not None:
                # If the first attempt failed, don't wait for the
                # timeout to try an address from the secondary queue.
                self.io_loop.remove_timeout(self.timeout)
                self.on_timeout()
            return
        self.clear_timeouts()
        if self.future.done():
            # This is a late arrival; just drop it.
            stream.close()
        else:
            # The object tracked while the attempt was in progress may
            # differ from the stream it produced (see _TLSAttempt); only
            # the latter is handed to our caller.
            self.streams.discard(attempt if attempt is not None else stream)
            self.future.set_result((af, addr, stream))
            self.close_streams()

    def on_future_done(
        self, future: "Future[Tuple[socket.AddressFamily, Any, IOStream]]"
    ) -> None:
        if future.cancelled():
            # The caller gave up (e.g. asyncio.wait_for). Close any
            # connection attempts that are still in progress instead of
            # leaving them open until the OS gives up on them.
            self.clear_timeouts()
            self.close_streams()

    def set_timeout(self, timeout: float) -> None:
        self.timeout = self.io_loop.add_timeout(
            self.io_loop.time() + timeout, self.on_timeout
        )

    def on_timeout(self) -> None:
        self.timeout = None
        if not self.future.done():
            self.try_connect(iter(self.secondary_addrs))

    def clear_timeout(self) -> None:
        if self.timeout is not None:
            self.io_loop.remove_timeout(self.timeout)

    def set_connect_timeout(self, connect_timeout: float | datetime.timedelta) -> None:
        self.connect_timeout = self.io_loop.add_timeout(
            connect_timeout, self.on_connect_timeout
        )

    def on_connect_timeout(self) -> None:
        if not self.future.done():
            connecting = []
            handshaking = []
            for addr, attempt in self.pending.items():
                if getattr(attempt, "handshaking", False):
                    handshaking.append(addr)
                else:
                    connecting.append(addr)
            messages = []
            if connecting:
                messages.append(
                    "Timeout while connecting to %s"
                    % ", ".join(sorted(_format_addr(addr) for addr in connecting))
                )
            if handshaking:
                messages.append(
                    "Timeout during TLS handshake with %s"
                    % ", ".join(sorted(_format_addr(addr) for addr in handshaking))
                )
            self.future.set_exception(
                TimeoutError("; ".join(messages) or "Timeout while connecting")
            )
        self.close_streams()

    def clear_timeouts(self) -> None:
        if self.timeout is not None:
            self.io_loop.remove_timeout(self.timeout)
        if self.connect_timeout is not None:
            self.io_loop.remove_timeout(self.connect_timeout)

    def close_streams(self) -> None:
        for stream in self.streams:
            stream.close()


class _TLSAttempt:
    """Tracks a single connection attempt that includes a TLS handshake.

    This is the closable used by `_Connector` for connections made with
    ``ssl_options``.  Closing it abandons the attempt at any stage:
    while the TCP connection is in progress, during the TLS handshake,
    or after it has completed.
    """

    def __init__(self, stream: IOStream) -> None:
        self.stream = stream
        # Set once the TCP connection has been established and the TLS
        # handshake has begun.  Used by `_Connector` to describe this
        # attempt in timeout error messages.
        self.handshaking = False
        self.tls_future: Future[IOStream] | None = None

    def close(self) -> None:
        if self.tls_future is not None:
            # The socket now belongs to the SSLIOStream created by
            # start_tls (the original stream must not be closed once
            # its socket has been detached).  Cancelling the future
            # returned by `IOStream.start_tls` closes the new
            # `SSLIOStream` and its socket.  This is a no-op if the
            # handshake has already completed.
            self.tls_future.cancel()
        else:
            self.stream.close()


class TCPClient:
    """A non-blocking TCP connection factory.

    .. versionchanged:: 5.0
       The ``io_loop`` argument (deprecated since version 4.1) has been removed.
    """

    def __init__(self, resolver: Resolver | None = None) -> None:
        if resolver is not None:
            self.resolver = resolver
            self._own_resolver = False
        else:
            self.resolver = Resolver()
            self._own_resolver = True

    def close(self) -> None:
        if self._own_resolver:
            self.resolver.close()

    async def connect(
        self,
        host: str,
        port: int,
        af: socket.AddressFamily = socket.AF_UNSPEC,
        ssl_options: dict[str, Any] | ssl.SSLContext | None = None,
        max_buffer_size: int | None = None,
        source_ip: str | None = None,
        source_port: int | None = None,
        timeout: float | datetime.timedelta | None = None,
    ) -> IOStream:
        """Connect to the given host and port.

        Asynchronously returns an `.IOStream` (or `.SSLIOStream` if
        ``ssl_options`` is not None).

        Using the ``source_ip`` kwarg, one can specify the source
        IP address to use when establishing the connection.
        In case the user needs to resolve and
        use a specific interface, it has to be handled outside
        of Tornado as this depends very much on the platform.

        Raises `TimeoutError` if the input future does not complete before
        ``timeout``, which may be specified in any form allowed by
        `.IOLoop.add_timeout` (i.e. a `datetime.timedelta` or an absolute time
        relative to `.IOLoop.time`)

        The message of the `TimeoutError` indicates which phase of the
        connection timed out: resolving the hostname, establishing the TCP
        connection (listing the addresses being tried), or performing the
        TLS handshake.

        Similarly, when the user requires a certain source port, it can
        be specified using the ``source_port`` arg.

        .. versionchanged:: 4.5
           Added the ``source_ip`` and ``source_port`` arguments.

        .. versionchanged:: 5.0
           Added the ``timeout`` argument.

        .. versionchanged:: 6.6
           Timeout errors now have messages describing which phase of
           the connection process timed out.

        .. versionchanged:: 6.7
           When ``ssl_options`` is used, the TLS handshake is part of
           the "happy eyeballs" race between candidate addresses: a
           candidate only wins the race once its handshake has completed,
           and a failed handshake immediately allows another candidate
           to be tried.  All candidates share the same ``timeout``
           deadline.  Previously the first TCP connection to complete
           won the race, even if its TLS handshake subsequently failed.
        """
        if timeout is not None:
            if isinstance(timeout, numbers.Real):
                timeout = IOLoop.current().time() + timeout
            elif isinstance(timeout, datetime.timedelta):
                timeout = IOLoop.current().time() + timeout.total_seconds()
            else:
                raise TypeError("Unsupported timeout %r" % timeout)
        if timeout is not None:
            addrinfo = await gen.with_timeout(
                timeout,
                self.resolver.resolve(host, port, af),
                message=f"Timeout while resolving {host}",
            )
        else:
            addrinfo = await self.resolver.resolve(host, port, af)
        if ssl_options is not None:
            # Each candidate performs its TLS handshake as part of the
            # race, so the winner is the first connection that is
            # actually usable rather than merely the first to complete
            # a TCP connection.  A failed handshake immediately frees
            # up a slot for the next candidate, and all candidates
            # share the original deadline.
            connect_factory = functools.partial(
                self._create_ssl_stream,
                max_buffer_size,
                ssl_options=ssl_options,
                server_hostname=host,
                source_ip=source_ip,
                source_port=source_port,
            )
        else:
            connect_factory = functools.partial(
                self._create_stream,
                max_buffer_size,
                source_ip=source_ip,
                source_port=source_port,
            )
        connector = _Connector(addrinfo, connect_factory)
        connect_future = connector.start(connect_timeout=timeout)
        try:
            af, addr, stream = await connect_future
        except asyncio.CancelledError:
            # Cancellation can arrive after the connection succeeds but
            # before this coroutine receives the stream.
            if not connect_future.cancelled() and connect_future.exception() is None:
                connect_future.result()[2].close()
            raise
        # TODO: For better performance we could cache the (af, addr)
        # information here and reuse it on subsequent connections to
        # the same host. (http://tools.ietf.org/html/rfc6555#section-4.2)
        return stream

    def _create_stream(
        self,
        max_buffer_size: int | None,
        af: socket.AddressFamily,
        addr: tuple[Any, ...],
        source_ip: str | None = None,
        source_port: int | None = None,
    ) -> tuple[IOStream, "Future[IOStream]"]:
        # Always connect in plaintext; if ssl_options were given,
        # _create_ssl_stream converts each candidate stream to TLS as
        # soon as its TCP connection is established.
        source_port_bind = source_port if isinstance(source_port, int) else 0
        source_ip_bind = source_ip
        if source_port_bind and not source_ip:
            # User required a specific port, but did not specify
            # a certain source IP, will bind to the default loopback.
            source_ip_bind = "::1" if af == socket.AF_INET6 else "127.0.0.1"
            # Trying to use the same address family as the requested af socket:
            # - 127.0.0.1 for IPv4
            # - ::1 for IPv6
        socket_obj = socket.socket(af)
        try:
            if source_port_bind or source_ip_bind:
                # If the user requires binding also to a specific IP/port.
                socket_obj.bind((source_ip_bind, source_port_bind))
            stream = IOStream(socket_obj, max_buffer_size=max_buffer_size)
        except BaseException:
            socket_obj.close()
            raise
        return stream, stream.connect(addr)

    def _create_ssl_stream(
        self,
        max_buffer_size: int | None,
        af: socket.AddressFamily,
        addr: tuple[Any, ...],
        ssl_options: dict[str, Any] | ssl.SSLContext | None = None,
        server_hostname: str | None = None,
        source_ip: str | None = None,
        source_port: int | None = None,
    ) -> tuple[_TLSAttempt, "Future[IOStream]"]:
        # Connect in plaintext and begin the TLS handshake as soon as the
        # TCP connection is established.  The returned future does not
        # complete until the handshake is done, so the `_Connector` race
        # is won by the first connection that is actually usable.
        stream, tcp_future = self._create_stream(
            max_buffer_size, af, addr, source_ip=source_ip, source_port=source_port
        )
        attempt = _TLSAttempt(stream)
        ssl_future: Future[IOStream] = Future()

        def on_tcp_connected(future: "Future[IOStream]") -> None:
            try:
                connected_stream = future.result()
            except (Exception, asyncio.CancelledError) as e:
                if isinstance(e, asyncio.CancelledError):
                    ssl_future.cancel()
                else:
                    ssl_future.set_exception(e)
                return
            try:
                tls_future = connected_stream.start_tls(
                    False, ssl_options=ssl_options, server_hostname=server_hostname
                )
            except Exception as e:
                ssl_future.set_exception(e)
                return
            attempt.handshaking = True
            attempt.tls_future = tls_future
            future_add_done_callback(tls_future, on_tls_handshake_done)

        def on_tls_handshake_done(future: "Future[IOStream]") -> None:
            try:
                ssl_stream = future.result()
            except (Exception, asyncio.CancelledError) as e:
                if isinstance(e, asyncio.CancelledError):
                    ssl_future.cancel()
                else:
                    ssl_future.set_exception(e)
                return
            ssl_future.set_result(ssl_stream)

        future_add_done_callback(tcp_future, on_tcp_connected)
        return attempt, ssl_future
