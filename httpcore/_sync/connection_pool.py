from __future__ import annotations

import ssl
import sys
import types
import typing

from .._backends.sync import SyncBackend
from .._backends.base import SOCKET_OPTION, NetworkBackend
from .._exceptions import ConnectionNotAvailable, PoolTimeout, UnsupportedProtocol
from .._models import Origin, Proxy, Request, Response, URL, enforce_url
from .._synchronization import (
    Event,
    ShieldCancellation,
    ThreadLock,
    is_never_cancelled,
)
from .connection import HTTPConnection
from .interfaces import ConnectionInterface, RequestInterface


class PoolRequest:
    def __init__(self, request: Request) -> None:
        self.request = request
        self.connection: ConnectionInterface | None = None
        self._connection_acquired = Event()

    def assign_to_connection(self, connection: ConnectionInterface | None) -> None:
        self.connection = connection
        self._connection_acquired.set()

    def clear_connection(self) -> None:
        self.connection = None
        self._connection_acquired = Event()

    def wait_for_connection(
        self, timeout: float | None = None
    ) -> ConnectionInterface:
        if self.connection is None:
            self._connection_acquired.wait(timeout=timeout)
        assert self.connection is not None
        return self.connection

    def is_queued(self) -> bool:
        return self.connection is None


class Warmup:
    """
    Coordinates an in-flight connection warmup for a single origin.

    Only one warmup per origin is performed at a time. Any further warmup
    calls (or incoming requests) for the same origin join the in-flight
    warmup rather than opening a duplicate connection.
    """

    def __init__(self, origin: Origin) -> None:
        self.origin = origin
        self._completed = Event()
        self.exception: BaseException | None = None

    def set_exception(self, exc: BaseException) -> None:
        self.exception = exc

    def complete(self) -> None:
        self._completed.set()

    def wait(self, timeout: float | None = None) -> None:
        self._completed.wait(timeout=timeout)


class ConnectionPool(RequestInterface):
    """
    A connection pool for making HTTP requests.
    """

    def __init__(
        self,
        ssl_context: ssl.SSLContext | None = None,
        proxy: Proxy | None = None,
        max_connections: int | None = 10,
        max_keepalive_connections: int | None = None,
        keepalive_expiry: float | None = None,
        http1: bool = True,
        http2: bool = False,
        retries: int = 0,
        local_address: str | None = None,
        uds: str | None = None,
        network_backend: NetworkBackend | None = None,
        socket_options: typing.Iterable[SOCKET_OPTION] | None = None,
    ) -> None:
        """
        A connection pool for making HTTP requests.

        Parameters:
            ssl_context: An SSL context to use for verifying connections.
                If not specified, the default `httpcore.default_ssl_context()`
                will be used.
            max_connections: The maximum number of concurrent HTTP connections that
                the pool should allow. Any attempt to send a request on a pool that
                would exceed this amount will block until a connection is available.
            max_keepalive_connections: The maximum number of idle HTTP connections
                that will be maintained in the pool.
            keepalive_expiry: The duration in seconds that an idle HTTP connection
                may be maintained for before being expired from the pool.
            http1: A boolean indicating if HTTP/1.1 requests should be supported
                by the connection pool. Defaults to True.
            http2: A boolean indicating if HTTP/2 requests should be supported by
                the connection pool. Defaults to False.
            retries: The maximum number of retries when trying to establish a
                connection.
            local_address: Local address to connect from. Can also be used to connect
                using a particular address family. Using `local_address="0.0.0.0"`
                will connect using an `AF_INET` address (IPv4), while using
                `local_address="::"` will connect using an `AF_INET6` address (IPv6).
            uds: Path to a Unix Domain Socket to use instead of TCP sockets.
            network_backend: A backend instance to use for handling network I/O.
            socket_options: Socket options that have to be included
             in the TCP socket when the connection was established.
        """
        self._ssl_context = ssl_context
        self._proxy = proxy
        self._max_connections = (
            sys.maxsize if max_connections is None else max_connections
        )
        self._max_keepalive_connections = (
            sys.maxsize
            if max_keepalive_connections is None
            else max_keepalive_connections
        )
        self._max_keepalive_connections = min(
            self._max_connections, self._max_keepalive_connections
        )

        self._keepalive_expiry = keepalive_expiry
        self._http1 = http1
        self._http2 = http2
        self._retries = retries
        self._local_address = local_address
        self._uds = uds

        self._network_backend = (
            SyncBackend() if network_backend is None else network_backend
        )
        self._socket_options = socket_options

        # The mutable state on a connection pool is the queue of incoming requests,
        # and the set of connections that are servicing those requests.
        self._connections: list[ConnectionInterface] = []
        self._requests: list[PoolRequest] = []

        # In-flight warmups, keyed by the origin they are establishing a
        # connection to. This allows concurrent warmups and incoming requests
        # to join an existing warmup instead of racing to open duplicates.
        self._warmups: dict[Origin, Warmup] = {}

        # Events for warmups that are blocked waiting for pool capacity.
        # They are signalled whenever a connection leaves the pool.
        self._capacity_waiters: list[Event] = []

        # We only mutate the state of the connection pool within an 'optional_thread_lock'
        # context. This holds a threading lock unless we're running in async mode,
        # in which case it is a no-op.
        self._optional_thread_lock = ThreadLock()

    def create_connection(self, origin: Origin) -> ConnectionInterface:
        if self._proxy is not None:
            if self._proxy.url.scheme in (b"socks5", b"socks5h"):
                from .socks_proxy import Socks5Connection

                return Socks5Connection(
                    proxy_origin=self._proxy.url.origin,
                    proxy_auth=self._proxy.auth,
                    remote_origin=origin,
                    ssl_context=self._ssl_context,
                    keepalive_expiry=self._keepalive_expiry,
                    http1=self._http1,
                    http2=self._http2,
                    network_backend=self._network_backend,
                )
            elif origin.scheme == b"http":
                from .http_proxy import ForwardHTTPConnection

                return ForwardHTTPConnection(
                    proxy_origin=self._proxy.url.origin,
                    proxy_headers=self._proxy.headers,
                    proxy_ssl_context=self._proxy.ssl_context,
                    remote_origin=origin,
                    keepalive_expiry=self._keepalive_expiry,
                    network_backend=self._network_backend,
                )
            from .http_proxy import TunnelHTTPConnection

            return TunnelHTTPConnection(
                proxy_origin=self._proxy.url.origin,
                proxy_headers=self._proxy.headers,
                proxy_ssl_context=self._proxy.ssl_context,
                remote_origin=origin,
                ssl_context=self._ssl_context,
                keepalive_expiry=self._keepalive_expiry,
                http1=self._http1,
                http2=self._http2,
                network_backend=self._network_backend,
            )

        return HTTPConnection(
            origin=origin,
            ssl_context=self._ssl_context,
            keepalive_expiry=self._keepalive_expiry,
            http1=self._http1,
            http2=self._http2,
            retries=self._retries,
            local_address=self._local_address,
            uds=self._uds,
            network_backend=self._network_backend,
            socket_options=self._socket_options,
        )

    @property
    def connections(self) -> list[ConnectionInterface]:
        """
        Return a list of the connections currently in the pool.

        For example:

        ```python
        >>> pool.connections
        [
            <HTTPConnection ['https://example.com:443', HTTP/1.1, ACTIVE, Request Count: 6]>,
            <HTTPConnection ['https://example.com:443', HTTP/1.1, IDLE, Request Count: 9]> ,
            <HTTPConnection ['http://example.com:80', HTTP/1.1, IDLE, Request Count: 1]>,
        ]
        ```
        """
        return list(self._connections)

    def warmup(
        self,
        url: URL | bytes | str,
        *,
        extensions: typing.MutableMapping[str, typing.Any] | None = None,
    ) -> None:
        """
        Warm up a connection to the given origin.

        This establishes the socket connection and performs the TLS handshake
        (and HTTP/2 initialization, where applicable) without sending any HTTP
        request bytes. Once warmed up, the first real request to the origin can
        be serviced immediately over the ready connection.

        Warmups are coordinated per origin:

        * If an existing connection can already service the origin, the call
          returns without doing anything.
        * If a warmup to the origin is already in flight, concurrent callers
          join that warmup instead of opening another connection.
        * The pool's `max_connections` capacity is always respected. When the
          pool is full, an idle connection may be closed to make room; if all
          the connections are busy the call waits until one becomes available,
          subject to the `pool` timeout.

        Cancelling a joined warmup only cancels the caller's wait; the
        connection being established by the warmup owner continues. Cancelling
        the warmup owner aborts the establishment and removes the partially
        opened connection from the pool.
        """
        url = enforce_url(url, name="url")
        scheme = url.scheme.decode()
        if scheme == "":
            raise UnsupportedProtocol(
                "Request URL is missing an 'http://' or 'https://' protocol."
            )
        if scheme not in ("http", "https", "ws", "wss"):
            raise UnsupportedProtocol(
                f"Request URL has an unsupported protocol '{scheme}://'."
            )

        origin = url.origin
        extensions = {} if extensions is None else extensions
        timeouts = extensions.get("timeout", {})
        connect_timeout = timeouts.get("connect", None)
        pool_timeout = timeouts.get("pool", None)

        with self._optional_thread_lock:
            # An existing connection that can handle this origin means there
            # is nothing further to warm up.
            existing = [
                connection
                for connection in self._connections
                if connection.can_handle_request(origin)
                and not connection.is_closed()
                and not connection.has_expired()
            ]
            if existing:
                return

            # Join an in-flight warmup for this origin rather than opening
            # a duplicate connection.
            warmup = self._warmups.get(origin)
            if warmup is None:
                warmup = Warmup(origin)
                self._warmups[origin] = warmup
                is_owner = True
            else:
                is_owner = False

        if not is_owner:
            # Waiting joiners never raise the owner's failure. If the warmup
            # fails they simply fall through and may open their own connection
            # through the regular request flow.
            try:
                warmup.wait(timeout=pool_timeout)
            except (PoolTimeout, ConnectionNotAvailable):
                raise
            except Exception:
                pass
            return

        connection: ConnectionInterface | None = None
        try:
            # Wait until the pool has capacity for an additional connection,
            # closing an idle connection to make room where possible.
            self._acquire_warmup_capacity(warmup, timeout=pool_timeout)

            with self._optional_thread_lock:
                connection = self.create_connection(origin)
                self._connections.append(connection)

            # Perform the actual connect + handshake. No HTTP request is sent.
            connection.connect(timeout=connect_timeout)
        except BaseException as exc:
            # The owner's cancellation aborts the warmup. Joined callers are
            # woken but do not inherit the cancellation.
            if is_never_cancelled(exc):
                warmup.set_exception(ConnectionNotAvailable("Warmup cancelled."))
            else:
                warmup.set_exception(exc)

            with self._optional_thread_lock:
                if connection is not None and connection in self._connections:
                    self._connections.remove(connection)
                self._warmups.pop(origin, None)
                warmup.complete()

            if connection is not None:
                with ShieldCancellation():
                    connection.close()
            raise
        else:
            with self._optional_thread_lock:
                self._warmups.pop(origin, None)
                warmup.complete()

    def _acquire_warmup_capacity(
        self, warmup: Warmup, timeout: float | None
    ) -> None:
        """
        Block until the pool has room for another connection, respecting
        `max_connections`.

        Idle connections to other origins may be closed to reclaim a slot.
        If every connection is busy the call waits until one is released
        back to (or removed from) the pool, subject to the pool timeout.
        """
        while True:
            with self._optional_thread_lock:
                if len(self._connections) < self._max_connections:
                    return

                idle_connections = [
                    connection
                    for connection in self._connections
                    if connection.is_idle()
                    and not connection.can_handle_request(warmup.origin)
                ]
                if idle_connections:
                    # Reclaim a slot by closing a surplus idle connection.
                    connection = idle_connections[0]
                    self._connections.remove(connection)
                    closing = [connection]
                    waiter: Event | None = None
                else:
                    # Every connection is busy. Register interest in a
                    # freed slot and wait for it outside of the thread lock.
                    closing = []
                    waiter = Event()
                    self._capacity_waiters.append(waiter)

            if closing:
                self._close_connections(closing)
                continue

            try:
                waiter.wait(timeout=timeout)
            except BaseException:
                with self._optional_thread_lock:
                    if waiter in self._capacity_waiters:
                        self._capacity_waiters.remove(waiter)
                raise

    def _notify_capacity_waiters(self) -> None:
        """
        Wake up any warmups blocked waiting for pool capacity. Called
        whenever a connection is removed from the pool.
        """
        waiters = self._capacity_waiters
        self._capacity_waiters = []
        for waiter in waiters:
            waiter.set()

    def handle_request(self, request: Request) -> Response:
        """
        Send an HTTP request, and return an HTTP response.

        This is the core implementation that is called into by `.request()` or `.stream()`.
        """
        scheme = request.url.scheme.decode()
        if scheme == "":
            raise UnsupportedProtocol(
                "Request URL is missing an 'http://' or 'https://' protocol."
            )
        if scheme not in ("http", "https", "ws", "wss"):
            raise UnsupportedProtocol(
                f"Request URL has an unsupported protocol '{scheme}://'."
            )

        timeouts = request.extensions.get("timeout", {})
        timeout = timeouts.get("pool", None)

        # If a warmup for this origin is in flight then join it, so that the
        # request benefits from the connection being established rather than
        # racing to open a second one. A failed or cancelled warmup does not
        # fail the request: it proceeds with the normal request flow instead.
        warmup = self._warmups.get(request.url.origin)
        if warmup is not None:
            try:
                warmup.wait(timeout=timeout)
            except (PoolTimeout, ConnectionNotAvailable):
                raise
            except Exception:
                pass

        with self._optional_thread_lock:
            # Add the incoming request to our request queue.
            pool_request = PoolRequest(request)
            self._requests.append(pool_request)

        try:
            while True:
                with self._optional_thread_lock:
                    # Assign incoming requests to available connections,
                    # closing or creating new connections as required.
                    closing = self._assign_requests_to_connections()
                self._close_connections(closing)

                # Wait until this request has an assigned connection.
                connection = pool_request.wait_for_connection(timeout=timeout)

                try:
                    # Send the request on the assigned connection.
                    response = connection.handle_request(
                        pool_request.request
                    )
                except ConnectionNotAvailable:
                    # In some cases a connection may initially be available to
                    # handle a request, but then become unavailable.
                    #
                    # In this case we clear the connection and try again.
                    pool_request.clear_connection()
                else:
                    break  # pragma: nocover

        except BaseException as exc:
            with self._optional_thread_lock:
                # For any exception or cancellation we remove the request from
                # the queue, and then re-assign requests to connections.
                self._requests.remove(pool_request)
                closing = self._assign_requests_to_connections()

            self._close_connections(closing)
            raise exc from None

        # Return the response. Note that in this case we still have to manage
        # the point at which the response is closed.
        assert isinstance(response.stream, typing.Iterable)
        return Response(
            status=response.status,
            headers=response.headers,
            content=PoolByteStream(
                stream=response.stream, pool_request=pool_request, pool=self
            ),
            extensions=response.extensions,
        )

    def _assign_requests_to_connections(self) -> list[ConnectionInterface]:
        """
        Manage the state of the connection pool, assigning incoming
        requests to connections as available.

        Called whenever a new request is added or removed from the pool.

        Any closing connections are returned, allowing the I/O for closing
        those connections to be handled seperately.
        """
        closing_connections = []

        # First we handle cleaning up any connections that are closed,
        # have expired their keep-alive, or surplus idle connections.
        for connection in list(self._connections):
            if connection.is_closed():
                # log: "removing closed connection"
                self._connections.remove(connection)
                self._notify_capacity_waiters()
            elif connection.has_expired():
                # log: "closing expired connection"
                self._connections.remove(connection)
                closing_connections.append(connection)
                self._notify_capacity_waiters()
            elif (
                connection.is_idle()
                and sum(connection.is_idle() for connection in self._connections)
                > self._max_keepalive_connections
            ):
                # log: "closing idle connection"
                self._connections.remove(connection)
                closing_connections.append(connection)
                self._notify_capacity_waiters()

        # Assign queued requests to connections.
        queued_requests = [request for request in self._requests if request.is_queued()]
        for pool_request in queued_requests:
            origin = pool_request.request.url.origin
            available_connections = [
                connection
                for connection in self._connections
                if connection.can_handle_request(origin) and connection.is_available()
            ]
            idle_connections = [
                connection for connection in self._connections if connection.is_idle()
            ]

            # There are three cases for how we may be able to handle the request:
            #
            # 1. There is an existing connection that can handle the request.
            # 2. We can create a new connection to handle the request.
            # 3. We can close an idle connection and then create a new connection
            #    to handle the request.
            if available_connections:
                # log: "reusing existing connection"
                connection = available_connections[0]
                pool_request.assign_to_connection(connection)
            elif len(self._connections) < self._max_connections:
                # log: "creating new connection"
                connection = self.create_connection(origin)
                self._connections.append(connection)
                pool_request.assign_to_connection(connection)
            elif idle_connections:
                # log: "closing idle connection"
                connection = idle_connections[0]
                self._connections.remove(connection)
                closing_connections.append(connection)
                self._notify_capacity_waiters()
                # log: "creating new connection"
                connection = self.create_connection(origin)
                self._connections.append(connection)
                pool_request.assign_to_connection(connection)

        return closing_connections

    def _close_connections(self, closing: list[ConnectionInterface]) -> None:
        # Close connections which have been removed from the pool.
        with ShieldCancellation():
            for connection in closing:
                connection.close()

    def close(self) -> None:
        # Explicitly close the connection pool.
        # Clears all existing requests and connections.
        with self._optional_thread_lock:
            closing_connections = list(self._connections)
            self._connections = []
            self._warmups = {}
            self._notify_capacity_waiters()
        self._close_connections(closing_connections)

    def __enter__(self) -> ConnectionPool:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None = None,
        exc_value: BaseException | None = None,
        traceback: types.TracebackType | None = None,
    ) -> None:
        self.close()

    def __repr__(self) -> str:
        class_name = self.__class__.__name__
        with self._optional_thread_lock:
            request_is_queued = [request.is_queued() for request in self._requests]
            connection_is_idle = [
                connection.is_idle() for connection in self._connections
            ]

            num_active_requests = request_is_queued.count(False)
            num_queued_requests = request_is_queued.count(True)
            num_active_connections = connection_is_idle.count(False)
            num_idle_connections = connection_is_idle.count(True)

        requests_info = (
            f"Requests: {num_active_requests} active, {num_queued_requests} queued"
        )
        connection_info = (
            f"Connections: {num_active_connections} active, {num_idle_connections} idle"
        )

        return f"<{class_name} [{requests_info} | {connection_info}]>"


class PoolByteStream:
    def __init__(
        self,
        stream: typing.Iterable[bytes],
        pool_request: PoolRequest,
        pool: ConnectionPool,
    ) -> None:
        self._stream = stream
        self._pool_request = pool_request
        self._pool = pool
        self._closed = False

    def __iter__(self) -> typing.Iterator[bytes]:
        try:
            for part in self._stream:
                yield part
        except BaseException as exc:
            self.close()
            raise exc from None

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            with ShieldCancellation():
                if hasattr(self._stream, "close"):
                    self._stream.close()

            with self._pool._optional_thread_lock:
                self._pool._requests.remove(self._pool_request)
                closing = self._pool._assign_requests_to_connections()

            self._pool._close_connections(closing)
