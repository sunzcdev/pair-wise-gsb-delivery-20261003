from __future__ import annotations

import ipaddress
import os
import re
import typing
from urllib.request import getproxies

from ._exceptions import InvalidURL
from ._types import PrimitiveData
from ._urlparse import normalize_port

if typing.TYPE_CHECKING:  # pragma: no cover
    from ._urls import URL


def primitive_value_to_str(value: PrimitiveData) -> str:
    """
    Coerce a primitive data type into a string value.

    Note that we prefer JSON-style 'true'/'false' for boolean values here.
    """
    if value is True:
        return "true"
    elif value is False:
        return "false"
    elif value is None:
        return ""
    return str(value)


def get_environment_proxies() -> dict[str, str | None]:
    """Gets proxy information from the environment"""

    # urllib.request.getproxies() falls back on System
    # Registry and Config for proxies on Windows and macOS.
    # We don't want to propagate non-HTTP proxies into
    # our configuration such as 'TRAVIS_APT_PROXY'.
    proxy_info = getproxies()
    mounts: dict[str, str | None] = {}

    for scheme in ("http", "https", "all"):
        if proxy_info.get(scheme):
            hostname = proxy_info[scheme]
            mounts[f"{scheme}://"] = (
                hostname if "://" in hostname else f"http://{hostname}"
            )

    no_proxy_hosts = [host.strip() for host in proxy_info.get("no", "").split(",")]
    for hostname in no_proxy_hosts:
        # See https://curl.haxx.se/libcurl/c/CURLOPT_NOPROXY.html for details
        # on how names in `NO_PROXY` are handled.
        if hostname == "*":
            # If NO_PROXY=* is used or if "*" occurs as any one of the comma
            # separated hostnames, then we should just bypass any information
            # from HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, and always ignore
            # proxies.
            return {}
        elif hostname:
            # NO_PROXY=.google.com is marked as "all://*.google.com,
            #   which disables "www.google.com" but not "google.com"
            # NO_PROXY=google.com is marked as "all://*google.com,
            #   which disables "www.google.com" and "google.com".
            #   (But not "wwwgoogle.com")
            # NO_PROXY can include domains, IPv6, IPv4 addresses and "localhost"
            #   NO_PROXY=example.com,::1,localhost,192.168.0.0/16
            # IPv4 or IPv6 CIDR networks, such as "192.168.0.0/16" or
            #   "[fd00::/8]", only match literal IP addresses in request URLs.
            if "://" in hostname:
                mounts[hostname] = None
            else:
                # Allow bracketed IPv6 forms such as "[::1]" or "[fd00::/8]",
                # normalizing them back to their unbracketed form.
                address = hostname
                if hostname.startswith("[") and "]" in hostname:
                    inside, _, suffix = hostname[1:].partition("]")
                    if not suffix or suffix.startswith("/"):
                        address = f"{inside}{suffix}"
                if is_ipv4_hostname(address):
                    mounts[f"all://{address}"] = None
                elif is_ipv6_hostname(address):
                    # Preserve any CIDR prefix, as in "all://[fd00::/8]".
                    mounts[f"all://[{address}]"] = None
                elif address.lower() == "localhost":
                    mounts[f"all://{address}"] = None
                else:
                    mounts[f"all://*{hostname}"] = None

    return mounts


def to_bytes(value: str | bytes, encoding: str = "utf-8") -> bytes:
    return value.encode(encoding) if isinstance(value, str) else value


def to_str(value: str | bytes, encoding: str = "utf-8") -> str:
    return value if isinstance(value, str) else value.decode(encoding)


def to_bytes_or_str(value: str, match_type_of: typing.AnyStr) -> typing.AnyStr:
    return value if isinstance(match_type_of, str) else value.encode()


def unquote(value: str) -> str:
    return value[1:-1] if value[0] == value[-1] == '"' else value


# Mount keys that specify an explicit IPv4 or IPv6 CIDR network, such as
# "all://10.0.0.0/8", "all://10.0.0.0/8:8080", "all://[fd00::/8]",
# "all://[fd00::/8]:8080" or "all://[fd00::]/8".
CIDR_PATTERN = re.compile(
    r"^(?P<scheme>[a-zA-Z][a-zA-Z0-9+.\-]*)://"
    r"(?:"
    r"\[(?P<ipv6>[^\[\]/]+)/(?P<ipv6_prefix>\d+)\]"
    r"|"
    r"\[(?P<ipv6_host>[^\[\]/]+)\]/(?P<ipv6_host_prefix>\d+)"
    r"|"
    r"(?P<ipv4>[0-9.]+)/(?P<ipv4_prefix>\d+)"
    r")"
    r"(?::(?P<port>[^:]*))?$"
)


def _parse_cidr_key(
    pattern: str,
) -> tuple[str, ipaddress.IPv4Network | ipaddress.IPv6Network, int | None] | None:
    """
    Parse a mount key that specifies an explicit CIDR network.

    Supported forms are `"all://10.0.0.0/8"`, `"all://10.0.0.0/8:8080"`,
    `"all://[fd00::/8]"` and `"all://[fd00::/8]:8080"`, using any scheme.
    `"all://[fd00::]/8"` is also accepted.

    Returns `None` if the key does not specify a network, otherwise a
    `(scheme, network, port)` tuple, with the port already normalized
    against the scheme's default port.

    Raises `ValueError` if the key does specify a network, but the network
    or the port is invalid.
    """
    match = CIDR_PATTERN.match(pattern)
    if match is None:
        return None

    address = match["ipv4"] or match["ipv6"] or match["ipv6_host"]
    prefix = match["ipv4_prefix"] or match["ipv6_prefix"] or match["ipv6_host_prefix"]
    try:
        network = ipaddress.ip_network(f"{address}/{prefix}")
    except ValueError:
        raise ValueError(f"Invalid CIDR network in proxy key: {pattern!r}") from None

    scheme = match["scheme"].lower()
    port_text = match["port"]
    try:
        # Normalize default ports in the same way that `httpx.URL` does, so
        # that "http://10.0.0.0/8:80" is equivalent to "http://10.0.0.0/8",
        # and the pattern compares against the URL's real origin.
        port = normalize_port(port_text, scheme)
    except InvalidURL:
        raise ValueError(f"Invalid CIDR network in proxy key: {pattern!r}") from None
    return scheme, network, port


def peek_filelike_length(stream: typing.Any) -> int | None:
    """
    Given a file-like stream object, return its length in number of bytes
    without reading it into memory.
    """
    try:
        # Is it an actual file?
        fd = stream.fileno()
        # Yup, seems to be an actual file.
        length = os.fstat(fd).st_size
    except (AttributeError, OSError):
        # No... Maybe it's something that supports random access, like `io.BytesIO`?
        try:
            # Assuming so, go to end of stream to figure out its length,
            # then put it back in place.
            offset = stream.tell()
            length = stream.seek(0, os.SEEK_END)
            stream.seek(offset)
        except (AttributeError, OSError):
            # Not even that? Sorry, we're doomed...
            return None

    return length


class URLPattern:
    """
    A utility class currently used for making lookups against proxy keys...

    # Wildcard matching...
    >>> pattern = URLPattern("all://")
    >>> pattern.matches(httpx.URL("http://example.com"))
    True

    # Witch scheme matching...
    >>> pattern = URLPattern("https://")
    >>> pattern.matches(httpx.URL("https://example.com"))
    True
    >>> pattern.matches(httpx.URL("http://example.com"))
    False

    # With domain matching...
    >>> pattern = URLPattern("https://example.com")
    >>> pattern.matches(httpx.URL("https://example.com"))
    True
    >>> pattern.matches(httpx.URL("http://example.com"))
    False
    >>> pattern.matches(httpx.URL("https://other.com"))
    False

    # Wildcard scheme, with domain matching...
    >>> pattern = URLPattern("all://example.com")
    >>> pattern.matches(httpx.URL("https://example.com"))
    True
    >>> pattern.matches(httpx.URL("http://example.com"))
    True
    >>> pattern.matches(httpx.URL("https://other.com"))
    False

    # With port matching...
    >>> pattern = URLPattern("https://example.com:1234")
    >>> pattern.matches(httpx.URL("https://example.com:1234"))
    True
    >>> pattern.matches(httpx.URL("https://example.com"))
    False

    # With CIDR matching, against literal IP addresses in the URL...
    >>> pattern = URLPattern("all://10.0.0.0/8")
    >>> pattern.matches(httpx.URL("http://10.1.2.3"))
    True
    >>> pattern.matches(httpx.URL("http://11.0.0.1"))
    False

    # Hostnames are never resolved to decide routing...
    >>> pattern.matches(httpx.URL("http://example.com"))
    False

    # IPv6 CIDR matching...
    >>> pattern = URLPattern("all://[fd00::/8]")
    >>> pattern.matches(httpx.URL("http://[fd00::1]:8080"))
    True
    """

    def __init__(self, pattern: str) -> None:
        from ._urls import URL

        if pattern and ":" not in pattern:
            raise ValueError(
                f"Proxy keys should use proper URL forms rather "
                f"than plain scheme strings. "
                f'Instead of "{pattern}", use "{pattern}://"'
            )

        self.pattern = pattern
        self.cidr: ipaddress.IPv4Network | ipaddress.IPv6Network | None = None
        # How specific the host portion of the pattern is. For a network this
        # is the prefix length, so that more specific networks match first.
        self.host_specificity = 0

        cidr_key = _parse_cidr_key(pattern)
        if cidr_key is not None:
            # A mount key that specifies an explicit CIDR network, such as
            # "all://10.0.0.0/8" or "all://[fd00::/8]". These only match
            # literal IP addresses in request URLs, so they never trigger
            # DNS resolution.
            scheme, self.cidr, self.port = cidr_key
            self.scheme = "" if scheme == "all" else scheme
            self.host = ""
            self.host_regex: typing.Pattern[str] | None = None
            self.host_specificity = self.cidr.prefixlen
            return

        url = URL(pattern)
        self.scheme = "" if url.scheme == "all" else url.scheme
        self.host = "" if url.host == "*" else url.host
        self.port = url.port
        if not url.host or url.host == "*":
            self.host_regex = None
        elif url.host.startswith("*."):
            # *.example.com should match "www.example.com", but not "example.com"
            domain = re.escape(url.host[2:])
            self.host_regex = re.compile(f"^.+\\.{domain}$")
        elif url.host.startswith("*"):
            # *example.com should match "www.example.com" and "example.com"
            domain = re.escape(url.host[1:])
            self.host_regex = re.compile(f"^(.+\\.)?{domain}$")
        else:
            # example.com should match "example.com" but not "www.example.com"
            domain = re.escape(url.host)
            self.host_regex = re.compile(f"^{domain}$")

        # A literal IP address mount is equivalent to a full-length network,
        # so that "all://10.1.2.3" is more specific than "all://10.0.0.0/8".
        self.host_specificity = self._host_specificity(url.host)

    @staticmethod
    def _host_specificity(host: str) -> int:
        if not host or host == "*":
            return 0
        try:
            address = ipaddress.ip_address(host)
        except ValueError:
            # Hostnames are ranked by length, matching the previous behaviour.
            return len(host)
        return address.max_prefixlen

    def matches(self, other: URL) -> bool:
        if self.scheme and self.scheme != other.scheme:
            return False
        if self.cidr is not None:
            # CIDR patterns only match literal IP addresses in the URL.
            # Hostnames are never assumed to belong to a network, and are
            # never resolved to decide routing.
            try:
                address = ipaddress.ip_address(other.host)
            except ValueError:
                return False
            if address not in self.cidr:
                return False
        elif (
            self.host
            and self.host_regex is not None
            and not self.host_regex.match(other.host)
        ):
            return False
        if self.port is not None and self.port != other.port:
            return False
        return True

    @property
    def priority(self) -> tuple[int, int, int]:
        """
        The priority allows URLPattern instances to be sortable, so that
        we can match from most specific to least specific.
        """
        # URLs with a port should take priority over URLs without a port.
        port_priority = 0 if self.port is not None else 1
        # More specific hosts or networks should match first.
        host_priority = -self.host_specificity
        # Longer schemes should match first.
        scheme_priority = -len(self.scheme)
        return (port_priority, host_priority, scheme_priority)

    def __hash__(self) -> int:
        return hash(self.pattern)

    def __lt__(self, other: URLPattern) -> bool:
        return self.priority < other.priority

    def __eq__(self, other: typing.Any) -> bool:
        return isinstance(other, URLPattern) and self.pattern == other.pattern


def is_ipv4_hostname(hostname: str) -> bool:
    try:
        ipaddress.IPv4Address(hostname.split("/")[0])
    except Exception:
        return False
    return True


def is_ipv6_hostname(hostname: str) -> bool:
    try:
        ipaddress.IPv6Address(hostname.split("/")[0])
    except Exception:
        return False
    return True
