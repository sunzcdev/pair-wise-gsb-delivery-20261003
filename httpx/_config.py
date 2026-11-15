from __future__ import annotations

import os
import time
import typing

from ._models import Headers
from ._types import CertTypes, HeaderTypes, TimeoutTypes
from ._urls import URL

if typing.TYPE_CHECKING:
    import ssl  # pragma: no cover

    from ._models import Request  # pragma: no cover

__all__ = ["Limits", "Proxy", "Timeout", "create_ssl_context"]


class Clock(typing.Protocol):
    """
    A source of monotonic time, measured in seconds.

    The default implementation uses ``time.perf_counter()``. A different
    clock (including a manually controlled one) can be supplied to a client
    to make deadline behaviour deterministic in tests.
    """

    def now(self) -> float:
        ...  # pragma: no cover


class SystemClock:
    """
    The default clock, backed by ``time.perf_counter()``.
    """

    def now(self) -> float:
        return time.perf_counter()


class Deadline:
    """
    An absolute point in time on a given :class:`Clock`.

    A deadline captures the *overall* budget for a request, from the first
    byte being sent until the response body has been fully read. The same
    deadline is shared across every hop of a redirect or authentication
    chain, so following a redirect never resets the remaining budget.

    ``timeout=None`` means there is no overall deadline.
    """

    def __init__(
        self,
        timeout: float | None,
        *,
        clock: Clock,
        origin: Request | None = None,
    ) -> None:
        self.clock = clock
        self.origin = origin
        if timeout is None:
            self.deadline: float | None = None
        else:
            self.deadline = clock.now() + timeout

    def remaining(self) -> float | None:
        """
        The number of seconds still available, or ``None`` if the deadline
        is unbounded. Never negative: an expired deadline reports ``0.0``.
        """
        if self.deadline is None:
            return None
        return max(0.0, self.deadline - self.clock.now())

    def has_expired(self) -> bool:
        if self.deadline is None:
            return False
        return self.clock.now() >= self.deadline

    def cap(self, timeout: float | None) -> float | None:
        """
        Cap a single-stage timeout so that it cannot outlive the overall
        deadline. An unbounded stage timeout or an unbounded deadline is
        passed through unchanged.
        """
        remaining = self.remaining()
        if remaining is None:
            return timeout
        if timeout is None:
            return remaining
        return min(timeout, remaining)


class UnsetType:
    pass  # pragma: no cover


UNSET = UnsetType()


def create_ssl_context(
    verify: ssl.SSLContext | str | bool = True,
    cert: CertTypes | None = None,
    trust_env: bool = True,
) -> ssl.SSLContext:
    import ssl
    import warnings

    import certifi

    if verify is True:
        if trust_env and os.environ.get("SSL_CERT_FILE"):  # pragma: nocover
            ctx = ssl.create_default_context(cafile=os.environ["SSL_CERT_FILE"])
        elif trust_env and os.environ.get("SSL_CERT_DIR"):  # pragma: nocover
            ctx = ssl.create_default_context(capath=os.environ["SSL_CERT_DIR"])
        else:
            # Default case...
            ctx = ssl.create_default_context(cafile=certifi.where())
    elif verify is False:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    elif isinstance(verify, str):  # pragma: nocover
        message = (
            "`verify=<str>` is deprecated. "
            "Use `verify=ssl.create_default_context(cafile=...)` "
            "or `verify=ssl.create_default_context(capath=...)` instead."
        )
        warnings.warn(message, DeprecationWarning)
        if os.path.isdir(verify):
            return ssl.create_default_context(capath=verify)
        return ssl.create_default_context(cafile=verify)
    else:
        ctx = verify

    if cert:  # pragma: nocover
        message = (
            "`cert=...` is deprecated. Use `verify=<ssl_context>` instead,"
            "with `.load_cert_chain()` to configure the certificate chain."
        )
        warnings.warn(message, DeprecationWarning)
        if isinstance(cert, str):
            ctx.load_cert_chain(cert)
        else:
            ctx.load_cert_chain(*cert)

    return ctx


class Timeout:
    """
    Timeout configuration.

    **Usage**:

    Timeout(None)               # No timeouts.
    Timeout(5.0)                # 5s timeout on all operations.
    Timeout(None, connect=5.0)  # 5s timeout on connect, no other timeouts.
    Timeout(5.0, connect=10.0)  # 10s timeout on connect. 5s timeout elsewhere.
    Timeout(5.0, pool=None)     # No timeout on acquiring connection from pool.
                                # 5s timeout elsewhere.
    Timeout(5.0, total=10.0)    # 5s timeout per stage, with a 10s overall
                                # deadline shared across redirects/auth hops.
    """

    def __init__(
        self,
        timeout: TimeoutTypes | UnsetType = UNSET,
        *,
        connect: None | float | UnsetType = UNSET,
        read: None | float | UnsetType = UNSET,
        write: None | float | UnsetType = UNSET,
        pool: None | float | UnsetType = UNSET,
        total: None | float | UnsetType = UNSET,
    ) -> None:
        if isinstance(timeout, Timeout):
            # Passed as a single explicit Timeout.
            assert connect is UNSET
            assert read is UNSET
            assert write is UNSET
            assert pool is UNSET
            assert total is UNSET
            self.connect = timeout.connect  # type: typing.Optional[float]
            self.read = timeout.read  # type: typing.Optional[float]
            self.write = timeout.write  # type: typing.Optional[float]
            self.pool = timeout.pool  # type: typing.Optional[float]
            self.total = timeout.total  # type: typing.Optional[float]
        elif isinstance(timeout, tuple):
            # Passed as a tuple.
            self.connect = timeout[0]
            self.read = timeout[1]
            self.write = None if len(timeout) < 3 else timeout[2]
            self.pool = None if len(timeout) < 4 else timeout[3]
            self.total = None if isinstance(total, UnsetType) else total
        elif not (
            isinstance(connect, UnsetType)
            or isinstance(read, UnsetType)
            or isinstance(write, UnsetType)
            or isinstance(pool, UnsetType)
        ):
            self.connect = connect
            self.read = read
            self.write = write
            self.pool = pool
            self.total = None if isinstance(total, UnsetType) else total
        else:
            if isinstance(timeout, UnsetType):
                raise ValueError(
                    "httpx.Timeout must either include a default, or set all "
                    "four parameters explicitly."
                )
            self.connect = timeout if isinstance(connect, UnsetType) else connect
            self.read = timeout if isinstance(read, UnsetType) else read
            self.write = timeout if isinstance(write, UnsetType) else write
            self.pool = timeout if isinstance(pool, UnsetType) else pool
            # `total` is an overall deadline that is deliberately not covered
            # by the positional default; it is only enabled when set explicitly.
            self.total = None if isinstance(total, UnsetType) else total

    def as_dict(self) -> dict[str, float | None]:
        return {
            "connect": self.connect,
            "read": self.read,
            "write": self.write,
            "pool": self.pool,
            "total": self.total,
        }

    def __eq__(self, other: typing.Any) -> bool:
        return (
            isinstance(other, self.__class__)
            and self.connect == other.connect
            and self.read == other.read
            and self.write == other.write
            and self.pool == other.pool
            and self.total == other.total
        )

    def __repr__(self) -> str:
        class_name = self.__class__.__name__
        if self.total is None and len(
            {self.connect, self.read, self.write, self.pool}
        ) == 1:
            return f"{class_name}(timeout={self.connect})"
        class_repr = (
            f"{class_name}(connect={self.connect}, "
            f"read={self.read}, write={self.write}, pool={self.pool})"
        )
        if self.total is not None:
            class_repr = class_repr[:-1] + f", total={self.total})"
        return class_repr


class Limits:
    """
    Configuration for limits to various client behaviors.

    **Parameters:**

    * **max_connections** - The maximum number of concurrent connections that may be
            established.
    * **max_keepalive_connections** - Allow the connection pool to maintain
            keep-alive connections below this point. Should be less than or equal
            to `max_connections`.
    * **keepalive_expiry** - Time limit on idle keep-alive connections in seconds.
    """

    def __init__(
        self,
        *,
        max_connections: int | None = None,
        max_keepalive_connections: int | None = None,
        keepalive_expiry: float | None = 5.0,
    ) -> None:
        self.max_connections = max_connections
        self.max_keepalive_connections = max_keepalive_connections
        self.keepalive_expiry = keepalive_expiry

    def __eq__(self, other: typing.Any) -> bool:
        return (
            isinstance(other, self.__class__)
            and self.max_connections == other.max_connections
            and self.max_keepalive_connections == other.max_keepalive_connections
            and self.keepalive_expiry == other.keepalive_expiry
        )

    def __repr__(self) -> str:
        class_name = self.__class__.__name__
        return (
            f"{class_name}(max_connections={self.max_connections}, "
            f"max_keepalive_connections={self.max_keepalive_connections}, "
            f"keepalive_expiry={self.keepalive_expiry})"
        )


class Proxy:
    def __init__(
        self,
        url: URL | str,
        *,
        ssl_context: ssl.SSLContext | None = None,
        auth: tuple[str, str] | None = None,
        headers: HeaderTypes | None = None,
    ) -> None:
        url = URL(url)
        headers = Headers(headers)

        if url.scheme not in ("http", "https", "socks5", "socks5h"):
            raise ValueError(f"Unknown scheme for proxy URL {url!r}")

        if url.username or url.password:
            # Remove any auth credentials from the URL.
            auth = (url.username, url.password)
            url = url.copy_with(username=None, password=None)

        self.url = url
        self.auth = auth
        self.headers = headers
        self.ssl_context = ssl_context

    @property
    def raw_auth(self) -> tuple[bytes, bytes] | None:
        # The proxy authentication as raw bytes.
        return (
            None
            if self.auth is None
            else (self.auth[0].encode("utf-8"), self.auth[1].encode("utf-8"))
        )

    def __repr__(self) -> str:
        # The authentication is represented with the password component masked.
        auth = (self.auth[0], "********") if self.auth else None

        # Build a nice concise representation.
        url_str = f"{str(self.url)!r}"
        auth_str = f", auth={auth!r}" if auth else ""
        headers_str = f", headers={dict(self.headers)!r}" if self.headers else ""
        return f"Proxy({url_str}{auth_str}{headers_str})"


DEFAULT_TIMEOUT_CONFIG = Timeout(timeout=5.0)
DEFAULT_LIMITS = Limits(max_connections=100, max_keepalive_connections=20)
DEFAULT_MAX_REDIRECTS = 20
