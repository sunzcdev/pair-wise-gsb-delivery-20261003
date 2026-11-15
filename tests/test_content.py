import io
import typing
from urllib.parse import quote_plus

import pytest

import httpx

method = "POST"
url = "https://www.example.com"


@pytest.mark.anyio
async def test_empty_content():
    request = httpx.Request(method, url)
    assert isinstance(request.stream, httpx.SyncByteStream)
    assert isinstance(request.stream, httpx.AsyncByteStream)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {"Host": "www.example.com", "Content-Length": "0"}
    assert sync_content == b""
    assert async_content == b""


@pytest.mark.anyio
async def test_bytes_content():
    request = httpx.Request(method, url, content=b"Hello, world!")
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {"Host": "www.example.com", "Content-Length": "13"}
    assert sync_content == b"Hello, world!"
    assert async_content == b"Hello, world!"

    # Support 'data' for compat with requests.
    with pytest.warns(DeprecationWarning):
        request = httpx.Request(method, url, data=b"Hello, world!")  # type: ignore
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {"Host": "www.example.com", "Content-Length": "13"}
    assert sync_content == b"Hello, world!"
    assert async_content == b"Hello, world!"


@pytest.mark.anyio
async def test_bytesio_content():
    request = httpx.Request(method, url, content=io.BytesIO(b"Hello, world!"))
    assert isinstance(request.stream, typing.Iterable)
    assert not isinstance(request.stream, typing.AsyncIterable)

    content = b"".join(list(request.stream))

    assert request.headers == {"Host": "www.example.com", "Content-Length": "13"}
    assert content == b"Hello, world!"


@pytest.mark.anyio
async def test_async_bytesio_content():
    class AsyncBytesIO:
        def __init__(self, content: bytes) -> None:
            self._idx = 0
            self._content = content

        async def aread(self, chunk_size: int) -> bytes:
            chunk = self._content[self._idx : self._idx + chunk_size]
            self._idx = self._idx + chunk_size
            return chunk

        async def __aiter__(self):
            yield self._content  # pragma: no cover

    request = httpx.Request(method, url, content=AsyncBytesIO(b"Hello, world!"))
    assert not isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Transfer-Encoding": "chunked",
    }
    assert content == b"Hello, world!"


@pytest.mark.anyio
async def test_iterator_content():
    def hello_world() -> typing.Iterator[bytes]:
        yield b"Hello, "
        yield b"world!"

    request = httpx.Request(method, url, content=hello_world())
    assert isinstance(request.stream, typing.Iterable)
    assert not isinstance(request.stream, typing.AsyncIterable)

    content = b"".join(list(request.stream))

    assert request.headers == {
        "Host": "www.example.com",
        "Transfer-Encoding": "chunked",
    }
    assert content == b"Hello, world!"

    with pytest.raises(httpx.StreamConsumed):
        list(request.stream)

    # Support 'data' for compat with requests.
    with pytest.warns(DeprecationWarning):
        request = httpx.Request(method, url, data=hello_world())  # type: ignore
    assert isinstance(request.stream, typing.Iterable)
    assert not isinstance(request.stream, typing.AsyncIterable)

    content = b"".join(list(request.stream))

    assert request.headers == {
        "Host": "www.example.com",
        "Transfer-Encoding": "chunked",
    }
    assert content == b"Hello, world!"


@pytest.mark.anyio
async def test_aiterator_content():
    async def hello_world() -> typing.AsyncIterator[bytes]:
        yield b"Hello, "
        yield b"world!"

    request = httpx.Request(method, url, content=hello_world())
    assert not isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Transfer-Encoding": "chunked",
    }
    assert content == b"Hello, world!"

    with pytest.raises(httpx.StreamConsumed):
        [part async for part in request.stream]

    # Support 'data' for compat with requests.
    with pytest.warns(DeprecationWarning):
        request = httpx.Request(method, url, data=hello_world())  # type: ignore
    assert not isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Transfer-Encoding": "chunked",
    }
    assert content == b"Hello, world!"


@pytest.mark.anyio
async def test_json_content():
    request = httpx.Request(method, url, json={"Hello": "world!"})
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "18",
        "Content-Type": "application/json",
    }
    assert sync_content == b'{"Hello":"world!"}'
    assert async_content == b'{"Hello":"world!"}'


@pytest.mark.anyio
async def test_urlencoded_content():
    request = httpx.Request(method, url, data={"Hello": "world!"})
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "14",
        "Content-Type": "application/x-www-form-urlencoded",
    }
    assert sync_content == b"Hello=world%21"
    assert async_content == b"Hello=world%21"


@pytest.mark.anyio
async def test_urlencoded_boolean():
    request = httpx.Request(method, url, data={"example": True})
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "12",
        "Content-Type": "application/x-www-form-urlencoded",
    }
    assert sync_content == b"example=true"
    assert async_content == b"example=true"


@pytest.mark.anyio
async def test_urlencoded_none():
    request = httpx.Request(method, url, data={"example": None})
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "8",
        "Content-Type": "application/x-www-form-urlencoded",
    }
    assert sync_content == b"example="
    assert async_content == b"example="


@pytest.mark.anyio
async def test_urlencoded_list():
    request = httpx.Request(method, url, data={"example": ["a", 1, True]})
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "32",
        "Content-Type": "application/x-www-form-urlencoded",
    }
    assert sync_content == b"example=a&example=1&example=true"
    assert async_content == b"example=a&example=1&example=true"


@pytest.mark.anyio
async def test_urlencoded_default_charset_is_utf8_without_header_charset():
    request = httpx.Request(method, url, data={"greeting": "café"})

    content = b"".join(list(request.stream))

    assert request.headers["Content-Type"] == "application/x-www-form-urlencoded"
    assert content == b"greeting=caf%C3%A9"


@pytest.mark.anyio
async def test_urlencoded_custom_charset_encodes_name_and_value():
    request = httpx.Request(
        method, url, data={"greeting": "café"}, form_charset="latin-1"
    )

    content = b"".join(list(request.stream))

    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=iso8859-1"
    )
    assert content == b"greeting=caf%E9"


@pytest.mark.anyio
async def test_urlencoded_custom_charset_non_ascii_field_name():
    request = httpx.Request(
        method, url, data={"名前": "値"}, form_charset="shift_jis"
    )

    content = b"".join(list(request.stream))

    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=shift_jis"
    )
    expected = (
        quote_plus("名前", encoding="shift_jis")
        + "="
        + quote_plus("値", encoding="shift_jis")
    ).encode("ascii")
    assert content == expected
    assert content == b"%96%BC%91O=%92l"


@pytest.mark.anyio
async def test_urlencoded_preserves_repeat_and_ordered_keys():
    request = httpx.Request(
        method,
        url,
        data={"k": ["a", "b", "c"], "x": "1"},
        form_charset="latin-1",
    )

    content = b"".join(list(request.stream))

    assert content == b"k=a&k=b&k=c&x=1"


@pytest.mark.anyio
async def test_urlencoded_unencodable_value_reports_field_position():
    with pytest.raises(httpx.FormEncodingError) as exc_info:
        httpx.Request(
            method,
            url,
            data={"ok": "a", "bad": "中文"},
            form_charset="latin-1",
        )

    message = str(exc_info.value)
    assert "field 2" in message
    assert "item 1" in message
    assert "iso8859-1" in message


@pytest.mark.anyio
async def test_urlencoded_unencodable_name_reports_field_position():
    with pytest.raises(httpx.FormEncodingError) as exc_info:
        httpx.Request(method, url, data={"中": "x"}, form_charset="latin-1")

    message = str(exc_info.value)
    assert "name of field 1" in message


@pytest.mark.anyio
async def test_urlencoded_rejects_conflicting_explicit_charset():
    with pytest.raises(httpx.FormEncodingError, match="Conflicting form encodings"):
        httpx.Request(
            method,
            url,
            data={"k": "café"},
            headers={
                "Content-Type": "application/x-www-form-urlencoded; charset=shift_jis"
            },
            form_charset="latin-1",
        )


@pytest.mark.anyio
async def test_urlencoded_accepts_matching_explicit_charset_alias():
    request = httpx.Request(
        method,
        url,
        data={"k": "café"},
        headers={
            "Content-Type": "application/x-www-form-urlencoded; charset=ISO-8859-1"
        },
        form_charset="latin-1",
    )

    content = b"".join(list(request.stream))
    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=ISO-8859-1"
    )
    assert content == b"k=caf%E9"


@pytest.mark.anyio
async def test_urlencoded_unknown_charset_raises_lookup_error():
    with pytest.raises(LookupError):
        httpx.Request(
            method, url, data={"k": "a"}, form_charset="not-a-real-charset"
        )


@pytest.mark.anyio
async def test_urlencoded_form_charset_does_not_affect_raw_bytes():
    request = httpx.Request(
        method, url, content=b"\xff\xfe raw", form_charset="latin-1"
    )

    content = b"".join(list(request.stream))
    assert content == b"\xff\xfe raw"
    assert "Content-Type" not in request.headers


@pytest.mark.anyio
async def test_multipart_files_content():
    files = {"file": io.BytesIO(b"<file content>")}
    headers = {"Content-Type": "multipart/form-data; boundary=+++"}
    request = httpx.Request(
        method,
        url,
        files=files,
        headers=headers,
    )
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "138",
        "Content-Type": "multipart/form-data; boundary=+++",
    }
    assert sync_content == b"".join(
        [
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--+++--\r\n",
        ]
    )
    assert async_content == b"".join(
        [
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--+++--\r\n",
        ]
    )


@pytest.mark.anyio
async def test_multipart_data_and_files_content():
    data = {"message": "Hello, world!"}
    files = {"file": io.BytesIO(b"<file content>")}
    headers = {"Content-Type": "multipart/form-data; boundary=+++"}
    request = httpx.Request(method, url, data=data, files=files, headers=headers)
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "210",
        "Content-Type": "multipart/form-data; boundary=+++",
    }
    assert sync_content == b"".join(
        [
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="message"\r\n',
            b"\r\n",
            b"Hello, world!\r\n",
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--+++--\r\n",
        ]
    )
    assert async_content == b"".join(
        [
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="message"\r\n',
            b"\r\n",
            b"Hello, world!\r\n",
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--+++--\r\n",
        ]
    )


@pytest.mark.anyio
async def test_empty_request():
    request = httpx.Request(method, url, data={}, files={})
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {"Host": "www.example.com", "Content-Length": "0"}
    assert sync_content == b""
    assert async_content == b""


def test_invalid_argument():
    with pytest.raises(TypeError):
        httpx.Request(method, url, content=123)  # type: ignore

    with pytest.raises(TypeError):
        httpx.Request(method, url, content={"a": "b"})  # type: ignore


@pytest.mark.anyio
async def test_multipart_multiple_files_single_input_content():
    files = [
        ("file", io.BytesIO(b"<file content 1>")),
        ("file", io.BytesIO(b"<file content 2>")),
    ]
    headers = {"Content-Type": "multipart/form-data; boundary=+++"}
    request = httpx.Request(method, url, files=files, headers=headers)
    assert isinstance(request.stream, typing.Iterable)
    assert isinstance(request.stream, typing.AsyncIterable)

    sync_content = b"".join(list(request.stream))
    async_content = b"".join([part async for part in request.stream])

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Length": "271",
        "Content-Type": "multipart/form-data; boundary=+++",
    }
    assert sync_content == b"".join(
        [
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content 1>\r\n",
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content 2>\r\n",
            b"--+++--\r\n",
        ]
    )
    assert async_content == b"".join(
        [
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content 1>\r\n",
            b"--+++\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content 2>\r\n",
            b"--+++--\r\n",
        ]
    )


@pytest.mark.anyio
async def test_response_empty_content():
    response = httpx.Response(200)
    assert isinstance(response.stream, typing.Iterable)
    assert isinstance(response.stream, typing.AsyncIterable)

    sync_content = b"".join(list(response.stream))
    async_content = b"".join([part async for part in response.stream])

    assert response.headers == {}
    assert sync_content == b""
    assert async_content == b""


@pytest.mark.anyio
async def test_response_bytes_content():
    response = httpx.Response(200, content=b"Hello, world!")
    assert isinstance(response.stream, typing.Iterable)
    assert isinstance(response.stream, typing.AsyncIterable)

    sync_content = b"".join(list(response.stream))
    async_content = b"".join([part async for part in response.stream])

    assert response.headers == {"Content-Length": "13"}
    assert sync_content == b"Hello, world!"
    assert async_content == b"Hello, world!"


@pytest.mark.anyio
async def test_response_iterator_content():
    def hello_world() -> typing.Iterator[bytes]:
        yield b"Hello, "
        yield b"world!"

    response = httpx.Response(200, content=hello_world())
    assert isinstance(response.stream, typing.Iterable)
    assert not isinstance(response.stream, typing.AsyncIterable)

    content = b"".join(list(response.stream))

    assert response.headers == {"Transfer-Encoding": "chunked"}
    assert content == b"Hello, world!"

    with pytest.raises(httpx.StreamConsumed):
        list(response.stream)


@pytest.mark.anyio
async def test_response_aiterator_content():
    async def hello_world() -> typing.AsyncIterator[bytes]:
        yield b"Hello, "
        yield b"world!"

    response = httpx.Response(200, content=hello_world())
    assert not isinstance(response.stream, typing.Iterable)
    assert isinstance(response.stream, typing.AsyncIterable)

    content = b"".join([part async for part in response.stream])

    assert response.headers == {"Transfer-Encoding": "chunked"}
    assert content == b"Hello, world!"

    with pytest.raises(httpx.StreamConsumed):
        [part async for part in response.stream]


def test_response_invalid_argument():
    with pytest.raises(TypeError):
        httpx.Response(200, content=123)  # type: ignore


def test_ensure_ascii_false_with_french_characters():
    data = {"greeting": "Bonjour, ça va ?"}
    response = httpx.Response(200, json=data)
    assert "ça va" in response.text, (
        "ensure_ascii=False should preserve French accented characters"
    )
    assert response.headers["Content-Type"] == "application/json"


def test_separators_for_compact_json():
    data = {"clé": "valeur", "liste": [1, 2, 3]}
    response = httpx.Response(200, json=data)
    assert response.text == '{"clé":"valeur","liste":[1,2,3]}', (
        "separators=(',', ':') should produce a compact representation"
    )
    assert response.headers["Content-Type"] == "application/json"


def test_allow_nan_false():
    data_with_nan = {"nombre": float("nan")}
    data_with_inf = {"nombre": float("inf")}

    with pytest.raises(
        ValueError, match="Out of range float values are not JSON compliant"
    ):
        httpx.Response(200, json=data_with_nan)
    with pytest.raises(
        ValueError, match="Out of range float values are not JSON compliant"
    ):
        httpx.Response(200, json=data_with_inf)


def test_urlencoded_charset_via_build_request():
    client = httpx.Client()
    request = client.build_request(
        method, url, data={"greeting": "café"}, form_charset="latin-1"
    )

    assert (
        request.headers["Content-Type"]
        == "application/x-www-form-urlencoded; charset=iso8859-1"
    )
    assert request.content == b"greeting=caf%E9"


@pytest.mark.anyio
async def test_urlencoded_charset_bytes_seen_by_transport():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["content_type"] = request.headers.get("Content-Type")
        seen["content"] = request.content
        return httpx.Response(200)

    transport = httpx.MockTransport(handler)

    with httpx.Client(transport=transport) as client:
        client.request(
            method,
            url,
            data={"greeting": "café"},
            form_charset="latin-1",
        )

    assert (
        seen["content_type"]
        == "application/x-www-form-urlencoded; charset=iso8859-1"
    )
    assert seen["content"] == b"greeting=caf%E9"


@pytest.mark.anyio
async def test_urlencoded_charset_fails_before_reaching_transport():
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        return httpx.Response(200)

    transport = httpx.MockTransport(handler)

    with httpx.Client(transport=transport) as client:
        with pytest.raises(httpx.FormEncodingError):
            client.request(
                method,
                url,
                data={"bad": "中文"},
                form_charset="latin-1",
            )

    assert calls == 0


@pytest.mark.anyio
async def test_form_charset_does_not_affect_multipart():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["content_type"] = request.headers.get("Content-Type")
        seen["content"] = request.content
        return httpx.Response(200)

    transport = httpx.MockTransport(handler)
    files = {"file": ("upload", b"<file content>", "application/octet-stream")}

    with httpx.Client(transport=transport) as client:
        client.request(
            method,
            url,
            data={"message": "中文"},
            files=files,
            form_charset="latin-1",
        )

    assert seen["content_type"].startswith("multipart/form-data")
    assert "charset" not in seen["content_type"]
    # Multipart fields keep their existing UTF-8 behaviour.
    assert "中文".encode("utf-8") in seen["content"]
