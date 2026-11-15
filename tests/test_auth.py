"""
Unit tests for auth classes.

Integration tests also exist in tests/client/test_auth.py
"""

from urllib.request import parse_keqv_list

import pytest

import httpx


def test_basic_auth():
    auth = httpx.BasicAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    # The initial request should include a basic auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert request.headers["Authorization"].startswith("Basic")

    # No other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def test_digest_auth_with_200():
    auth = httpx.DigestAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    # The initial request should not include an auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # If a 200 response is returned, then no other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def test_digest_auth_with_401():
    auth = httpx.DigestAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    # The initial request should not include an auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # If a 401 response is returned, then a digest auth request is made.
    headers = {
        "WWW-Authenticate": 'Digest realm="...", qop="auth", nonce="...", opaque="..."'
    }
    response = httpx.Response(
        content=b"Auth required", status_code=401, headers=headers, request=request
    )
    request = flow.send(response)
    assert request.headers["Authorization"].startswith("Digest")

    # No other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def test_digest_auth_with_401_nonce_counting():
    auth = httpx.DigestAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    # The initial request should not include an auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # If a 401 response is returned, then a digest auth request is made.
    headers = {
        "WWW-Authenticate": 'Digest realm="...", qop="auth", nonce="...", opaque="..."'
    }
    response = httpx.Response(
        content=b"Auth required", status_code=401, headers=headers, request=request
    )
    first_request = flow.send(response)
    assert first_request.headers["Authorization"].startswith("Digest")

    # Each subsequent request contains the digest header by default...
    request = httpx.Request("GET", "https://www.example.com")
    flow = auth.sync_auth_flow(request)
    second_request = next(flow)
    assert second_request.headers["Authorization"].startswith("Digest")

    # ... and the client nonce count (nc) is increased
    first_nc = parse_keqv_list(first_request.headers["Authorization"].split(", "))["nc"]
    second_nc = parse_keqv_list(second_request.headers["Authorization"].split(", "))[
        "nc"
    ]
    assert int(first_nc, 16) + 1 == int(second_nc, 16)

    # No other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def set_cookies(request: httpx.Request) -> httpx.Response:
    headers = {
        "Set-Cookie": "session=.session_value...",
        "WWW-Authenticate": 'Digest realm="...", qop="auth", nonce="...", opaque="..."',
    }
    if request.url.path == "/auth":
        return httpx.Response(
            content=b"Auth required", status_code=401, headers=headers
        )
    else:
        raise NotImplementedError()  # pragma: no cover


def test_digest_auth_setting_cookie_in_request():
    url = "https://www.example.com/auth"
    client = httpx.Client(transport=httpx.MockTransport(set_cookies))
    request = client.build_request("GET", url)

    auth = httpx.DigestAuth(username="user", password="pass")
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    response = client.get(url)
    assert len(response.cookies) > 0
    assert response.cookies["session"] == ".session_value..."

    request = flow.send(response)
    assert request.headers["Authorization"].startswith("Digest")
    assert request.headers["Cookie"] == "session=.session_value..."

    # No other requests are made.
    response = httpx.Response(
        content=b"Hello, world!", status_code=200, request=request
    )
    with pytest.raises(StopIteration):
        flow.send(response)


def test_digest_auth_rfc_2069():
    # Example from https://datatracker.ietf.org/doc/html/rfc2069#section-2.4
    # with corrected response from https://www.rfc-editor.org/errata/eid749

    auth = httpx.DigestAuth(username="Mufasa", password="CircleOfLife")
    request = httpx.Request("GET", "https://www.example.com/dir/index.html")

    # The initial request should not include an auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # If a 401 response is returned, then a digest auth request is made.
    headers = {
        "WWW-Authenticate": (
            'Digest realm="testrealm@host.com", '
            'nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", '
            'opaque="5ccc069c403ebaf9f0171e9517f40e41"'
        )
    }
    response = httpx.Response(
        content=b"Auth required", status_code=401, headers=headers, request=request
    )
    request = flow.send(response)
    assert request.headers["Authorization"].startswith("Digest")
    assert 'username="Mufasa"' in request.headers["Authorization"]
    assert 'realm="testrealm@host.com"' in request.headers["Authorization"]
    assert (
        'nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093"' in request.headers["Authorization"]
    )
    assert 'uri="/dir/index.html"' in request.headers["Authorization"]
    assert (
        'opaque="5ccc069c403ebaf9f0171e9517f40e41"' in request.headers["Authorization"]
    )
    assert (
        'response="1949323746fe6a43ef61f9606e7febea"'
        in request.headers["Authorization"]
    )

    # No other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def test_digest_auth_rfc_7616_md5(monkeypatch):
    # Example from https://datatracker.ietf.org/doc/html/rfc7616#section-3.9.1

    def mock_get_client_nonce(nonce_count: int, nonce: bytes) -> bytes:
        return "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ".encode()

    auth = httpx.DigestAuth(username="Mufasa", password="Circle of Life")
    monkeypatch.setattr(auth, "_get_client_nonce", mock_get_client_nonce)

    request = httpx.Request("GET", "https://www.example.com/dir/index.html")

    # The initial request should not include an auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # If a 401 response is returned, then a digest auth request is made.
    headers = {
        "WWW-Authenticate": (
            'Digest realm="http-auth@example.org", '
            'qop="auth, auth-int", '
            "algorithm=MD5, "
            'nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", '
            'opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"'
        )
    }
    response = httpx.Response(
        content=b"Auth required", status_code=401, headers=headers, request=request
    )
    request = flow.send(response)
    assert request.headers["Authorization"].startswith("Digest")
    assert 'username="Mufasa"' in request.headers["Authorization"]
    assert 'realm="http-auth@example.org"' in request.headers["Authorization"]
    assert 'uri="/dir/index.html"' in request.headers["Authorization"]
    assert "algorithm=MD5" in request.headers["Authorization"]
    assert (
        'nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v"'
        in request.headers["Authorization"]
    )
    assert "nc=00000001" in request.headers["Authorization"]
    assert (
        'cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"'
        in request.headers["Authorization"]
    )
    assert "qop=auth" in request.headers["Authorization"]
    assert (
        'opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"'
        in request.headers["Authorization"]
    )
    assert (
        'response="8ca523f5e9506fed4657c9700eebdbec"'
        in request.headers["Authorization"]
    )

    # No other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def test_digest_auth_rfc_7616_sha_256(monkeypatch):
    # Example from https://datatracker.ietf.org/doc/html/rfc7616#section-3.9.1

    def mock_get_client_nonce(nonce_count: int, nonce: bytes) -> bytes:
        return "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ".encode()

    auth = httpx.DigestAuth(username="Mufasa", password="Circle of Life")
    monkeypatch.setattr(auth, "_get_client_nonce", mock_get_client_nonce)

    request = httpx.Request("GET", "https://www.example.com/dir/index.html")

    # The initial request should not include an auth header.
    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # If a 401 response is returned, then a digest auth request is made.
    headers = {
        "WWW-Authenticate": (
            'Digest realm="http-auth@example.org", '
            'qop="auth, auth-int", '
            "algorithm=SHA-256, "
            'nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", '
            'opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"'
        )
    }
    response = httpx.Response(
        content=b"Auth required", status_code=401, headers=headers, request=request
    )
    request = flow.send(response)
    assert request.headers["Authorization"].startswith("Digest")
    assert 'username="Mufasa"' in request.headers["Authorization"]
    assert 'realm="http-auth@example.org"' in request.headers["Authorization"]
    assert 'uri="/dir/index.html"' in request.headers["Authorization"]
    assert "algorithm=SHA-256" in request.headers["Authorization"]
    assert (
        'nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v"'
        in request.headers["Authorization"]
    )
    assert "nc=00000001" in request.headers["Authorization"]
    assert (
        'cnonce="f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ"'
        in request.headers["Authorization"]
    )
    assert "qop=auth" in request.headers["Authorization"]
    assert (
        'opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"'
        in request.headers["Authorization"]
    )
    assert (
        'response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"'
        in request.headers["Authorization"]
    )

    # No other requests are made.
    response = httpx.Response(content=b"Hello, world!", status_code=200)
    with pytest.raises(StopIteration):
        flow.send(response)


def _digest_challenge(nonce: str = "...", stale: str | None = None) -> dict[str, str]:
    fields = [
        'realm="..."',
        'qop="auth"',
        f'nonce="{nonce}"',
        'opaque="..."',
    ]
    if stale is not None:
        fields.append(f"stale={stale}")
    return {"WWW-Authenticate": "Digest " + ", ".join(fields)}


def test_digest_auth_stale_challenge_triggers_one_more_retry():
    auth = httpx.DigestAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    flow = auth.sync_auth_flow(request)
    request = next(flow)
    assert "Authorization" not in request.headers

    # First 401 challenge: an authenticated request is produced.
    response = httpx.Response(
        status_code=401, headers=_digest_challenge(nonce="n1"), request=request
    )
    first_request = flow.send(response)
    first_nc = parse_keqv_list(first_request.headers["Authorization"].split(", "))["nc"]
    assert first_nc == "00000001"

    # A second challenge marked stale=true, reusing the same nonce, is answered
    # again with an incremented nonce count.
    stale_response = httpx.Response(
        status_code=401,
        headers=_digest_challenge(nonce="n1", stale='"true"'),
        request=first_request,
    )
    second_request = flow.send(stale_response)
    second_fields = parse_keqv_list(second_request.headers["Authorization"].split(", "))
    assert second_fields["nc"] == "00000002"
    assert second_fields["nonce"] == "n1"

    # A final success ends the flow.
    success = httpx.Response(status_code=200, request=second_request)
    with pytest.raises(StopIteration):
        flow.send(success)


def test_digest_auth_repeated_challenge_without_stale_stops():
    auth = httpx.DigestAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    flow = auth.sync_auth_flow(request)
    next(flow)

    response = httpx.Response(
        status_code=401, headers=_digest_challenge(nonce="n1"), request=request
    )
    flow.send(response)

    # A further 401 without stale=true (e.g. wrong credentials) must not be
    # answered: the flow terminates and yields the last 401 response.
    rejected = httpx.Response(
        status_code=401,
        headers=_digest_challenge(nonce="n2", stale='"false"'),
        request=request,
    )
    with pytest.raises(StopIteration):
        flow.send(rejected)


def test_digest_auth_endless_stale_challenges_are_bounded():
    auth = httpx.DigestAuth(username="user", password="pass")
    request = httpx.Request("GET", "https://www.example.com")

    flow = auth.sync_auth_flow(request)
    next(flow)

    challenge_count = 0
    for _ in range(httpx.DigestAuth._MAX_CHALLENGE_ATTEMPTS + 3):
        response = httpx.Response(
            status_code=401,
            headers=_digest_challenge(nonce="n1", stale='"true"'),
            request=request,
        )
        try:
            request = flow.send(response)
            challenge_count += 1
        except StopIteration:
            break

    # Only the bounded number of challenges are answered, then the flow ends.
    assert challenge_count == httpx.DigestAuth._MAX_CHALLENGE_ATTEMPTS


def test_digest_auth_state_isolated_between_origins():
    auth = httpx.DigestAuth(username="user", password="pass")

    def establish(host: str, nonce: str) -> None:
        request = httpx.Request("GET", f"https://{host}/")
        flow = auth.sync_auth_flow(request)
        next(flow)
        response = httpx.Response(
            status_code=401,
            headers=_digest_challenge(nonce=nonce),
            request=request,
        )
        authenticated = flow.send(response)
        assert authenticated.headers["Authorization"].startswith("Digest")
        flow.close()

    establish("a.example", "nonce-a")
    establish("b.example", "nonce-b")

    state_a = auth._get_state(httpx.Request("GET", "https://a.example/"))
    state_b = auth._get_state(httpx.Request("GET", "https://b.example/"))
    assert state_a is not None and state_b is not None
    assert state_a is not state_b
    assert state_a.challenge.nonce == b"nonce-a"
    assert state_b.challenge.nonce == b"nonce-b"
    # Each protection space keeps its own nonce count starting at one.
    assert state_a.nonce_count == state_b.nonce_count == 2
