"""
Tests for the per-request `form_charset` option, which selects the charset
used when encoding `application/x-www-form-urlencoded` request bodies.

All tests run against a receiving `MockTransport`, so the actual bytes
that would be sent over the wire are checked without any network access.
"""

import pytest

import httpx


class RecordingTransport(httpx.MockTransport):
    """
    A receiving transport that records the exact request headers and
    body bytes it is called with.
    """

    def __init__(self) -> None:
        self.requests: list[httpx.Request] = []
        self.bodies: list[bytes] = []

        def handler(request: httpx.Request) -> httpx.Response:
            self.requests.append(request)
            self.bodies.append(request.read())
            return httpx.Response(200, content=b"ok")

        super().__init__(handler)


@pytest.fixture
def transport() -> RecordingTransport:
    return RecordingTransport()


def test_form_charset_encodes_names_and_values(transport):
    client = httpx.Client(transport=transport)
    client.post(
        "http://example.org/collect",
        data={"名字": "值", "page": 1},
        form_charset="gbk",
    )

    (request,) = transport.requests
    (body,) = transport.bodies
    # '名字'.encode('gbk') == b'\xc3\xfb\xd7\xd6', '值'.encode('gbk') == b'\xd6\xb5'
    assert body == b"%C3%FB%D7%D6=%D6%B5&page=1"
    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=gbk"
    )
    assert request.headers["Content-Length"] == str(len(body))


def test_form_charset_body_decodes_with_matching_charset(transport):
    client = httpx.Client(transport=transport)
    client.post(
        "http://example.org/collect",
        data={"名字": "值"},
        form_charset="gb18030",
    )

    (request,) = transport.requests
    (body,) = transport.bodies
    assert request.headers["Content-Type"] == (
        "application/x-www-form-urlencoded; charset=gb18030"
    )
    # The percent-encoded bytes on the wire must decode back with the
    # same charset that the Content-Type header advertises.
    from urllib.parse import unquote_to_bytes

    name, _, value = body.partition(b"=")
    assert unquote_to_bytes(name).decode("gb18030") == "名字"
    assert unquote_to_bytes(value).decode("gb18030") == "值"


def test_form_charset_preserves_order_and_duplicate_keys(transport):
    client = httpx.Client(transport=transport)
    client.post(
        "http://example.org/collect",
        data=[("b", "1"), ("a", "2"), ("b", "3"), ("名", "字")],
        form_charset="gbk",
    )
    assert transport.bodies[0] == b"b=1&a=2&b=3&%C3%FB=%D7%D6"

    client.post(
        "http://example.org/collect",
        data={"k": ["x", "y"], "z": "1"},
        form_charset="gbk",
    )
    assert transport.bodies[1] == b"k=x&k=y&z=1"


def test_form_charset_reports_field_position_on_failure(transport):
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError, match=r"form field #1 \(key='bad'\)"):
        client.post(
            "http://example.org/collect",
            data=[("ok", "fine"), ("bad", "\U0001f600")],
            form_charset="gbk",
        )
    # The failure must be raised before anything is sent.
    assert transport.requests == []


def test_form_charset_unknown_charset_rejected(transport):
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError, match="Unknown charset"):
        client.post(
            "http://example.org/collect",
            data={"a": "b"},
            form_charset="not-a-charset",
        )
    assert transport.requests == []


def test_form_charset_does_not_affect_raw_content(transport):
    client = httpx.Client(transport=transport)
    client.post(
        "http://example.org/collect",
        content=b"\xff\xfe-binary",
        form_charset="gbk",
    )

    (request,) = transport.requests
    (body,) = transport.bodies
    assert body == b"\xff\xfe-binary"
    assert "Content-Type" not in request.headers


def test_form_charset_does_not_affect_multipart(transport):
    client = httpx.Client(transport=transport)
    client.post(
        "http://example.org/collect",
        data={"name": "值"},
        files={"file": ("a.txt", b"\xff\x00")},
        form_charset="gbk",
    )

    (request,) = transport.requests
    (body,) = transport.bodies
    assert request.headers["Content-Type"].startswith(
        "multipart/form-data; boundary="
    )
    assert b"\xff\x00" in body


def test_form_charset_conflicting_content_type_rejected(transport):
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError, match="Conflicting charsets"):
        client.post(
            "http://example.org/collect",
            data={"a": "b"},
            form_charset="gbk",
            headers={
                "Content-Type": "application/x-www-form-urlencoded; charset=utf-8"
            },
        )
    assert transport.requests == []


def test_form_charset_matching_content_type_accepted(transport):
    client = httpx.Client(transport=transport)
    client.post(
        "http://example.org/collect",
        data={"名字": "值"},
        form_charset="gbk",
        headers={"Content-Type": "application/x-www-form-urlencoded; charset=GBK"},
    )

    (request,) = transport.requests
    (body,) = transport.bodies
    assert body == b"%C3%FB%D7%D6=%D6%B5"
    # The explicitly provided header is preserved, and matches the body.
    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=GBK"
    )


def test_default_form_encoding_unchanged(transport):
    client = httpx.Client(transport=transport)
    client.post("http://example.org/collect", data={"name": "值"})

    (request,) = transport.requests
    (body,) = transport.bodies
    # '值'.encode('utf-8') == b'\xe5\x80\xbc'
    assert body == b"name=%E5%80%BC"
    assert request.headers["Content-Type"] == "application/x-www-form-urlencoded"


def test_build_request_form_charset():
    client = httpx.Client()
    request = client.build_request(
        "POST",
        "http://example.org/collect",
        data={"名字": "值"},
        form_charset="gbk",
    )
    assert request.read() == b"%C3%FB%D7%D6=%D6%B5"
    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=gbk"
    )


@pytest.mark.anyio
async def test_async_form_charset():
    transport = RecordingTransport()
    async with httpx.AsyncClient(transport=transport) as client:
        await client.post(
            "http://example.org/collect",
            data={"名字": "值"},
            form_charset="gbk",
        )

    (request,) = transport.requests
    (body,) = transport.bodies
    assert body == b"%C3%FB%D7%D6=%D6%B5"
    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=gbk"
    )
