import httpcore
import pytest

import httpx


def url_to_origin(url: str) -> httpcore.URL:
    """
    Given a URL string, return the origin in the raw tuple format that
    `httpcore` uses for it's representation.
    """
    u = httpx.URL(url)
    return httpcore.URL(scheme=u.raw_scheme, host=u.raw_host, port=u.port, target="/")


def test_socks_proxy():
    url = httpx.URL("http://www.example.com")

    for proxy in ("socks5://localhost/", "socks5h://localhost/"):
        client = httpx.Client(proxy=proxy)
        transport = client._transport_for_url(url)
        assert isinstance(transport, httpx.HTTPTransport)
        assert isinstance(transport._pool, httpcore.SOCKSProxy)

        async_client = httpx.AsyncClient(proxy=proxy)
        async_transport = async_client._transport_for_url(url)
        assert isinstance(async_transport, httpx.AsyncHTTPTransport)
        assert isinstance(async_transport._pool, httpcore.AsyncSOCKSProxy)


PROXY_URL = "http://[::1]"


@pytest.mark.parametrize(
    ["url", "proxies", "expected"],
    [
        ("http://example.com", {}, None),
        ("http://example.com", {"https://": PROXY_URL}, None),
        ("http://example.com", {"http://example.net": PROXY_URL}, None),
        # Using "*" should match any domain name.
        ("http://example.com", {"http://*": PROXY_URL}, PROXY_URL),
        ("https://example.com", {"http://*": PROXY_URL}, None),
        # Using "example.com" should match example.com, but not www.example.com
        ("http://example.com", {"http://example.com": PROXY_URL}, PROXY_URL),
        ("http://www.example.com", {"http://example.com": PROXY_URL}, None),
        # Using "*.example.com" should match www.example.com, but not example.com
        ("http://example.com", {"http://*.example.com": PROXY_URL}, None),
        ("http://www.example.com", {"http://*.example.com": PROXY_URL}, PROXY_URL),
        # Using "*example.com" should match example.com and www.example.com
        ("http://example.com", {"http://*example.com": PROXY_URL}, PROXY_URL),
        ("http://www.example.com", {"http://*example.com": PROXY_URL}, PROXY_URL),
        ("http://wwwexample.com", {"http://*example.com": PROXY_URL}, None),
        # ...
        ("http://example.com:443", {"http://example.com": PROXY_URL}, PROXY_URL),
        ("http://example.com", {"all://": PROXY_URL}, PROXY_URL),
        ("http://example.com", {"http://": PROXY_URL}, PROXY_URL),
        ("http://example.com", {"all://example.com": PROXY_URL}, PROXY_URL),
        ("http://example.com", {"http://example.com": PROXY_URL}, PROXY_URL),
        ("http://example.com", {"http://example.com:80": PROXY_URL}, PROXY_URL),
        ("http://example.com:8080", {"http://example.com:8080": PROXY_URL}, PROXY_URL),
        ("http://example.com:8080", {"http://example.com": PROXY_URL}, PROXY_URL),
        (
            "http://example.com",
            {
                "all://": PROXY_URL + ":1",
                "http://": PROXY_URL + ":2",
                "all://example.com": PROXY_URL + ":3",
                "http://example.com": PROXY_URL + ":4",
            },
            PROXY_URL + ":4",
        ),
        (
            "http://example.com",
            {
                "all://": PROXY_URL + ":1",
                "http://": PROXY_URL + ":2",
                "all://example.com": PROXY_URL + ":3",
            },
            PROXY_URL + ":3",
        ),
        (
            "http://example.com",
            {"all://": PROXY_URL + ":1", "http://": PROXY_URL + ":2"},
            PROXY_URL + ":2",
        ),
    ],
)
def test_transport_for_request(url, proxies, expected):
    mounts = {key: httpx.HTTPTransport(proxy=value) for key, value in proxies.items()}
    client = httpx.Client(mounts=mounts)

    transport = client._transport_for_url(httpx.URL(url))

    if expected is None:
        assert transport is client._transport
    else:
        assert isinstance(transport, httpx.HTTPTransport)
        assert isinstance(transport._pool, httpcore.HTTPProxy)
        assert transport._pool._proxy_url == url_to_origin(expected)


@pytest.mark.anyio
@pytest.mark.network
async def test_async_proxy_close():
    try:
        transport = httpx.AsyncHTTPTransport(proxy=PROXY_URL)
        client = httpx.AsyncClient(mounts={"https://": transport})
        await client.get("http://example.com")
    finally:
        await client.aclose()


@pytest.mark.network
def test_sync_proxy_close():
    try:
        transport = httpx.HTTPTransport(proxy=PROXY_URL)
        client = httpx.Client(mounts={"https://": transport})
        client.get("http://example.com")
    finally:
        client.close()


def test_unsupported_proxy_scheme():
    with pytest.raises(ValueError):
        httpx.Client(proxy="ftp://127.0.0.1")


@pytest.mark.parametrize(
    ["url", "env", "expected"],
    [
        ("http://google.com", {}, None),
        (
            "http://google.com",
            {"HTTP_PROXY": "http://example.com"},
            "http://example.com",
        ),
        # Auto prepend http scheme
        ("http://google.com", {"HTTP_PROXY": "example.com"}, "http://example.com"),
        (
            "http://google.com",
            {"HTTP_PROXY": "http://example.com", "NO_PROXY": "google.com"},
            None,
        ),
        # Everything proxied when NO_PROXY is empty/unset
        (
            "http://127.0.0.1",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": ""},
            "http://localhost:123",
        ),
        # Not proxied if NO_PROXY matches URL.
        (
            "http://127.0.0.1",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "127.0.0.1"},
            None,
        ),
        # Proxied if NO_PROXY scheme does not match URL.
        (
            "http://127.0.0.1",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "https://127.0.0.1"},
            "http://localhost:123",
        ),
        # Proxied if NO_PROXY scheme does not match host.
        (
            "http://127.0.0.1",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "1.1.1.1"},
            "http://localhost:123",
        ),
        # Not proxied if NO_PROXY matches host domain suffix.
        (
            "http://courses.mit.edu",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "mit.edu"},
            None,
        ),
        # Proxied even though NO_PROXY matches host domain *prefix*.
        (
            "https://mit.edu.info",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "mit.edu"},
            "http://localhost:123",
        ),
        # Not proxied if one item in NO_PROXY case matches host domain suffix.
        (
            "https://mit.edu.info",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "mit.edu,edu.info"},
            None,
        ),
        # Not proxied if one item in NO_PROXY case matches host domain suffix.
        # May include whitespace.
        (
            "https://mit.edu.info",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "mit.edu, edu.info"},
            None,
        ),
        # Proxied if no items in NO_PROXY match.
        (
            "https://mit.edu.info",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "mit.edu,mit.info"},
            "http://localhost:123",
        ),
        # Proxied if NO_PROXY domain doesn't match.
        (
            "https://foo.example.com",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "www.example.com"},
            "http://localhost:123",
        ),
        # Not proxied for subdomains matching NO_PROXY, with a leading ".".
        (
            "https://www.example1.com",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": ".example1.com"},
            None,
        ),
        # Proxied, because NO_PROXY subdomains only match if "." separated.
        (
            "https://www.example2.com",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "ample2.com"},
            "http://localhost:123",
        ),
        # No requests are proxied if NO_PROXY="*" is set.
        (
            "https://www.example3.com",
            {"ALL_PROXY": "http://localhost:123", "NO_PROXY": "*"},
            None,
        ),
    ],
)
@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxies_environ(monkeypatch, client_class, url, env, expected):
    for name, value in env.items():
        monkeypatch.setenv(name, value)

    client = client_class()
    transport = client._transport_for_url(httpx.URL(url))

    if expected is None:
        assert transport == client._transport
    else:
        assert transport._pool._proxy_url == url_to_origin(expected)


@pytest.mark.parametrize(
    ["proxies", "is_valid"],
    [
        ({"http": "http://127.0.0.1"}, False),
        ({"https": "http://127.0.0.1"}, False),
        ({"all": "http://127.0.0.1"}, False),
        ({"http://": "http://127.0.0.1"}, True),
        ({"https://": "http://127.0.0.1"}, True),
        ({"all://": "http://127.0.0.1"}, True),
    ],
)
def test_for_deprecated_proxy_params(proxies, is_valid):
    mounts = {key: httpx.HTTPTransport(proxy=value) for key, value in proxies.items()}

    if not is_valid:
        with pytest.raises(ValueError):
            httpx.Client(mounts=mounts)
    else:
        httpx.Client(mounts=mounts)


def test_proxy_with_mounts():
    proxy_transport = httpx.HTTPTransport(proxy="http://127.0.0.1")
    client = httpx.Client(mounts={"http://": proxy_transport})

    transport = client._transport_for_url(httpx.URL("http://example.com"))
    assert transport == proxy_transport


@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxy_with_cidr_exclusion_mounts(client_class):
    proxy_transport = httpx.HTTPTransport(proxy="http://127.0.0.1")
    client = client_class(
        trust_env=False,
        mounts={
            "all://": proxy_transport,
            "all://10.0.0.0/8": None,
            "all://[fd00::/8]": None,
        },
    )

    # Literal IP addresses within the excluded networks bypass the proxy.
    for url in [
        "http://10.1.2.3",
        "http://10.1.2.3:8080",
        "https://10.0.0.1",
        "http://[fd00::1]",
        "http://[fd00::1]:8080",
    ]:
        assert client._transport_for_url(httpx.URL(url)) is client._transport

    # Everything else is routed through the proxy. Hostnames are never
    # resolved, so they cannot match an excluded network.
    for url in ["http://11.0.0.1", "http://[::1]", "http://example.com"]:
        assert client._transport_for_url(httpx.URL(url)) is proxy_transport


@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxy_with_cidr_mounts_route_to_transport(client_class):
    proxy_transport = httpx.HTTPTransport(proxy="http://127.0.0.1")
    client = client_class(trust_env=False, mounts={"all://10.0.0.0/8": proxy_transport})

    # CIDR mounts are not only exclusions, but may also route to a transport.
    transport = client._transport_for_url(httpx.URL("http://10.1.2.3"))
    assert transport is proxy_transport
    assert client._transport_for_url(httpx.URL("http://11.0.0.1")) is client._transport


@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxy_with_port_qualified_cidr_mounts(client_class):
    proxy_transport = httpx.HTTPTransport(proxy="http://127.0.0.1")
    other_transport = httpx.HTTPTransport()
    client = client_class(
        trust_env=False,
        mounts={
            "all://10.0.0.0/8": proxy_transport,
            "all://10.0.0.0/8:8080": other_transport,
        },
    )

    # Port-qualified mounts take priority over plain mounts.
    transport = client._transport_for_url(httpx.URL("http://10.1.2.3:8080"))
    assert transport is other_transport
    transport = client._transport_for_url(httpx.URL("http://10.1.2.3:9090"))
    assert transport is proxy_transport


@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxy_argument_with_cidr_exclusion_mounts(client_class):
    client = client_class(
        trust_env=False,
        proxy="http://127.0.0.1",
        mounts={"all://10.0.0.0/8": None},
    )

    # The broad `proxy=` mount is overridden by an explicit CIDR exclusion.
    assert client._transport_for_url(httpx.URL("http://10.1.2.3")) is client._transport
    transport = client._transport_for_url(httpx.URL("http://11.1.1.1"))
    assert transport is not client._transport
    assert isinstance(transport._pool, (httpcore.HTTPProxy, httpcore.AsyncHTTPProxy))


@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxy_with_invalid_cidr_mount(client_class):
    with pytest.raises(ValueError):
        client_class(trust_env=False, mounts={"all://10.0.0.0/33": None})
    with pytest.raises(ValueError):
        client_class(trust_env=False, mounts={"all://[fd00::/129]": None})
    with pytest.raises(ValueError):
        client_class(trust_env=False, mounts={"all://10.0.0.0/8:not-a-port": None})


@pytest.mark.parametrize(
    ["url", "no_proxy", "expected"],
    [
        # Not proxied if NO_PROXY excludes a matching IPv4 network.
        ("http://192.168.1.1", "192.168.0.0/16", None),
        ("http://192.168.1.1:8080", "192.168.0.0/16", None),
        # Proxied if the IP address is outside the NO_PROXY network.
        ("http://192.167.1.1", "192.168.0.0/16", "http://localhost:123"),
        # Not proxied if NO_PROXY excludes a matching IPv6 network.
        ("http://[fd00::1]", "fd00::/8", None),
        ("http://[fd00::1]:8080", "fd00::/8", None),
        ("http://[fd00::1]", "[fd00::/8]", None),
        ("http://[::1]", "fd00::/8", "http://localhost:123"),
        # Hostnames are never resolved, so they cannot match a NO_PROXY network.
        ("http://example.com", "0.0.0.0/0", "http://localhost:123"),
    ],
)
@pytest.mark.parametrize("client_class", [httpx.Client, httpx.AsyncClient])
def test_proxies_environ_cidr(monkeypatch, client_class, url, no_proxy, expected):
    monkeypatch.setenv("ALL_PROXY", "http://localhost:123")
    monkeypatch.setenv("NO_PROXY", no_proxy)

    client = client_class()
    transport = client._transport_for_url(httpx.URL(url))

    if expected is None:
        assert transport is client._transport
    else:
        assert transport._pool._proxy_url == url_to_origin(expected)
