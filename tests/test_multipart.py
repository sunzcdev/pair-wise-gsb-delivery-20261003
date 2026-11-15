from __future__ import annotations

import asyncio
import io
import tempfile
import typing

import pytest

import httpx


def echo_request_content(request: httpx.Request) -> httpx.Response:
    return httpx.Response(200, content=request.content)


@pytest.mark.parametrize(("value,output"), (("abc", b"abc"), (b"abc", b"abc")))
def test_multipart(value, output):
    client = httpx.Client(transport=httpx.MockTransport(echo_request_content))

    # Test with a single-value 'data' argument, and a plain file 'files' argument.
    data = {"text": value}
    files = {"file": io.BytesIO(b"<file content>")}
    response = client.post("http://127.0.0.1:8000/", data=data, files=files)
    boundary = response.request.headers["Content-Type"].split("boundary=")[-1]
    boundary_bytes = boundary.encode("ascii")

    assert response.status_code == 200
    assert response.content == b"".join(
        [
            b"--" + boundary_bytes + b"\r\n",
            b'Content-Disposition: form-data; name="text"\r\n',
            b"\r\n",
            b"abc\r\n",
            b"--" + boundary_bytes + b"\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--" + boundary_bytes + b"--\r\n",
        ]
    )


@pytest.mark.parametrize(
    "header",
    [
        "multipart/form-data; boundary=+++; charset=utf-8",
        "multipart/form-data; charset=utf-8; boundary=+++",
        "multipart/form-data; boundary=+++",
        "multipart/form-data; boundary=+++ ;",
        'multipart/form-data; boundary="+++"; charset=utf-8',
        'multipart/form-data; charset=utf-8; boundary="+++"',
        'multipart/form-data; boundary="+++"',
        'multipart/form-data; boundary="+++" ;',
    ],
)
def test_multipart_explicit_boundary(header: str) -> None:
    client = httpx.Client(transport=httpx.MockTransport(echo_request_content))

    files = {"file": io.BytesIO(b"<file content>")}
    headers = {"content-type": header}
    response = client.post("http://127.0.0.1:8000/", files=files, headers=headers)
    boundary_bytes = b"+++"

    assert response.status_code == 200
    assert response.request.headers["Content-Type"] == header
    assert response.content == b"".join(
        [
            b"--" + boundary_bytes + b"\r\n",
            b'Content-Disposition: form-data; name="file"; filename="upload"\r\n',
            b"Content-Type: application/octet-stream\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--" + boundary_bytes + b"--\r\n",
        ]
    )


@pytest.mark.parametrize(
    "header",
    [
        "multipart/form-data; charset=utf-8",
        "multipart/form-data; charset=utf-8; ",
    ],
)
def test_multipart_header_without_boundary(header: str) -> None:
    client = httpx.Client(transport=httpx.MockTransport(echo_request_content))

    files = {"file": io.BytesIO(b"<file content>")}
    headers = {"content-type": header}
    response = client.post("http://127.0.0.1:8000/", files=files, headers=headers)

    assert response.status_code == 200
    assert response.request.headers["Content-Type"] == header


@pytest.mark.parametrize(("key"), (b"abc", 1, 2.3, None))
def test_multipart_invalid_key(key):
    client = httpx.Client(transport=httpx.MockTransport(echo_request_content))

    data = {key: "abc"}
    files = {"file": io.BytesIO(b"<file content>")}
    with pytest.raises(TypeError) as e:
        client.post(
            "http://127.0.0.1:8000/",
            data=data,
            files=files,
        )
    assert "Invalid type for name" in str(e.value)
    assert repr(key) in str(e.value)


@pytest.mark.parametrize(("value"), (object(), {"key": "value"}))
def test_multipart_invalid_value(value):
    client = httpx.Client(transport=httpx.MockTransport(echo_request_content))

    data = {"text": value}
    files = {"file": io.BytesIO(b"<file content>")}
    with pytest.raises(TypeError) as e:
        client.post("http://127.0.0.1:8000/", data=data, files=files)
    assert "Invalid type for value" in str(e.value)


def test_multipart_file_tuple():
    client = httpx.Client(transport=httpx.MockTransport(echo_request_content))

    # Test with a list of values 'data' argument,
    #     and a tuple style 'files' argument.
    data = {"text": ["abc"]}
    files = {"file": ("name.txt", io.BytesIO(b"<file content>"))}
    response = client.post("http://127.0.0.1:8000/", data=data, files=files)
    boundary = response.request.headers["Content-Type"].split("boundary=")[-1]
    boundary_bytes = boundary.encode("ascii")

    assert response.status_code == 200
    assert response.content == b"".join(
        [
            b"--" + boundary_bytes + b"\r\n",
            b'Content-Disposition: form-data; name="text"\r\n',
            b"\r\n",
            b"abc\r\n",
            b"--" + boundary_bytes + b"\r\n",
            b'Content-Disposition: form-data; name="file"; filename="name.txt"\r\n',
            b"Content-Type: text/plain\r\n",
            b"\r\n",
            b"<file content>\r\n",
            b"--" + boundary_bytes + b"--\r\n",
        ]
    )


@pytest.mark.parametrize("file_content_type", [None, "text/plain"])
def test_multipart_file_tuple_headers(file_content_type: str | None) -> None:
    file_name = "test.txt"
    file_content = io.BytesIO(b"<file content>")
    file_headers = {"Expires": "0"}

    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": (file_name, file_content, file_content_type, file_headers)}

    request = httpx.Request("POST", url, headers=headers, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        f'--BOUNDARY\r\nContent-Disposition: form-data; name="file"; '
        f'filename="{file_name}"\r\nExpires: 0\r\nContent-Type: '
        f"text/plain\r\n\r\n<file content>\r\n--BOUNDARY--\r\n"
        "".encode("ascii")
    )


def test_multipart_headers_include_content_type() -> None:
    """
    Content-Type from 4th tuple parameter (headers) should
    override the 3rd parameter (content_type)
    """
    file_name = "test.txt"
    file_content = io.BytesIO(b"<file content>")
    file_content_type = "text/plain"
    file_headers = {"Content-Type": "image/png"}

    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": (file_name, file_content, file_content_type, file_headers)}

    request = httpx.Request("POST", url, headers=headers, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        f'--BOUNDARY\r\nContent-Disposition: form-data; name="file"; '
        f'filename="{file_name}"\r\nContent-Type: '
        f"image/png\r\n\r\n<file content>\r\n--BOUNDARY--\r\n"
        "".encode("ascii")
    )


def test_multipart_encode(tmp_path: typing.Any) -> None:
    path = str(tmp_path / "name.txt")
    with open(path, "wb") as f:
        f.write(b"<file content>")

    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    data = {
        "a": "1",
        "b": b"C",
        "c": ["11", "22", "33"],
        "d": "",
        "e": True,
        "f": "",
    }
    with open(path, "rb") as input_file:
        files = {"file": ("name.txt", input_file)}

        request = httpx.Request("POST", url, headers=headers, data=data, files=files)
        request.read()

        assert request.headers == {
            "Host": "www.example.com",
            "Content-Type": "multipart/form-data; boundary=BOUNDARY",
            "Content-Length": str(len(request.content)),
        }
        assert request.content == (
            '--BOUNDARY\r\nContent-Disposition: form-data; name="a"\r\n\r\n1\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="b"\r\n\r\nC\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="c"\r\n\r\n11\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="c"\r\n\r\n22\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="c"\r\n\r\n33\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="d"\r\n\r\n\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="e"\r\n\r\ntrue\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="f"\r\n\r\n\r\n'
            '--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
            ' filename="name.txt"\r\n'
            "Content-Type: text/plain\r\n\r\n<file content>\r\n"
            "--BOUNDARY--\r\n"
            "".encode("ascii")
        )


def test_multipart_encode_unicode_file_contents() -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": ("name.txt", b"<bytes content>")}

    request = httpx.Request("POST", url, headers=headers, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
        b' filename="name.txt"\r\n'
        b"Content-Type: text/plain\r\n\r\n<bytes content>\r\n"
        b"--BOUNDARY--\r\n"
    )


def test_multipart_encode_files_allows_filenames_as_none() -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": (None, io.BytesIO(b"<file content>"))}

    request = httpx.Request("POST", url, headers=headers, data={}, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        '--BOUNDARY\r\nContent-Disposition: form-data; name="file"\r\n\r\n'
        "<file content>\r\n--BOUNDARY--\r\n"
        "".encode("ascii")
    )


@pytest.mark.parametrize(
    "file_name,expected_content_type",
    [
        ("example.json", "application/json"),
        ("example.txt", "text/plain"),
        ("no-extension", "application/octet-stream"),
    ],
)
def test_multipart_encode_files_guesses_correct_content_type(
    file_name: str, expected_content_type: str
) -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": (file_name, io.BytesIO(b"<file content>"))}

    request = httpx.Request("POST", url, headers=headers, data={}, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        f'--BOUNDARY\r\nContent-Disposition: form-data; name="file"; '
        f'filename="{file_name}"\r\nContent-Type: '
        f"{expected_content_type}\r\n\r\n<file content>\r\n--BOUNDARY--\r\n"
        "".encode("ascii")
    )


def test_multipart_encode_files_allows_bytes_content() -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": ("test.txt", b"<bytes content>", "text/plain")}

    request = httpx.Request("POST", url, headers=headers, data={}, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        '--BOUNDARY\r\nContent-Disposition: form-data; name="file"; '
        'filename="test.txt"\r\n'
        "Content-Type: text/plain\r\n\r\n<bytes content>\r\n"
        "--BOUNDARY--\r\n"
        "".encode("ascii")
    )


def test_multipart_encode_files_allows_str_content() -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    files = {"file": ("test.txt", "<str content>", "text/plain")}

    request = httpx.Request("POST", url, headers=headers, data={}, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Content-Length": str(len(request.content)),
    }
    assert request.content == (
        '--BOUNDARY\r\nContent-Disposition: form-data; name="file"; '
        'filename="test.txt"\r\n'
        "Content-Type: text/plain\r\n\r\n<str content>\r\n"
        "--BOUNDARY--\r\n"
        "".encode("ascii")
    )


def test_multipart_encode_files_raises_exception_with_StringIO_content() -> None:
    url = "https://www.example.com"
    files = {"file": ("test.txt", io.StringIO("content"), "text/plain")}
    with pytest.raises(TypeError):
        httpx.Request("POST", url, data={}, files=files)  # type: ignore


def test_multipart_encode_files_raises_exception_with_text_mode_file() -> None:
    url = "https://www.example.com"
    with tempfile.TemporaryFile(mode="w") as upload:
        files = {"file": ("test.txt", upload, "text/plain")}
        with pytest.raises(TypeError):
            httpx.Request("POST", url, data={}, files=files)  # type: ignore


def test_multipart_encode_non_seekable_filelike() -> None:
    """
    Test that special readable but non-seekable filelike objects are supported.
    In this case uploads with use 'Transfer-Encoding: chunked', instead of
    a 'Content-Length' header.
    """

    class IteratorIO(io.IOBase):
        def __init__(self, iterator: typing.Iterator[bytes]) -> None:
            self._iterator = iterator

        def read(self, *args: typing.Any) -> bytes:
            return b"".join(self._iterator)

    def data() -> typing.Iterator[bytes]:
        yield b"Hello"
        yield b"World"

    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
    fileobj: typing.Any = IteratorIO(data())
    files = {"file": fileobj}

    request = httpx.Request("POST", url, headers=headers, files=files)
    request.read()

    assert request.headers == {
        "Host": "www.example.com",
        "Content-Type": "multipart/form-data; boundary=BOUNDARY",
        "Transfer-Encoding": "chunked",
    }
    assert request.content == (
        b"--BOUNDARY\r\n"
        b'Content-Disposition: form-data; name="file"; filename="upload"\r\n'
        b"Content-Type: application/octet-stream\r\n"
        b"\r\n"
        b"HelloWorld\r\n"
        b"--BOUNDARY--\r\n"
    )


def test_multipart_rewinds_files():
    with tempfile.TemporaryFile() as upload:
        upload.write(b"Hello, world!")

        transport = httpx.MockTransport(echo_request_content)
        client = httpx.Client(transport=transport)

        files = {"file": upload}
        response = client.post("http://127.0.0.1:8000/", files=files)
        assert response.status_code == 200
        assert b"\r\nHello, world!\r\n" in response.content

        # POSTing the same file instance a second time should have the same content.
        files = {"file": upload}
        response = client.post("http://127.0.0.1:8000/", files=files)
        assert response.status_code == 200
        assert b"\r\nHello, world!\r\n" in response.content


class TestHeaderParamHTML5Formatting:
    def test_unicode(self):
        filename = "n\u00e4me"
        expected = b'filename="n\xc3\xa4me"'
        files = {"upload": (filename, b"<file content>")}
        request = httpx.Request("GET", "https://www.example.com", files=files)
        assert expected in request.read()

    def test_ascii(self):
        filename = "name"
        expected = b'filename="name"'
        files = {"upload": (filename, b"<file content>")}
        request = httpx.Request("GET", "https://www.example.com", files=files)
        assert expected in request.read()

    def test_unicode_escape(self):
        filename = "hello\\world\u0022"
        expected = b'filename="hello\\\\world%22"'
        files = {"upload": (filename, b"<file content>")}
        request = httpx.Request("GET", "https://www.example.com", files=files)
        assert expected in request.read()

    def test_unicode_with_control_character(self):
        filename = "hello\x1a\x1b\x1c"
        expected = b'filename="hello%1A\x1b%1C"'
        files = {"upload": (filename, b"<file content>")}
        request = httpx.Request("GET", "https://www.example.com", files=files)
        assert expected in request.read()


class NonSeekableStream(io.IOBase):
    """
    A readable, non-seekable file-like object whose length cannot be
    inferred through `fileno()`, `seek()` or `tell()`.
    """

    def __init__(self, data: bytes, chunk_size: int = 64 * 1024) -> None:
        self._buffer = io.BytesIO(data)
        self._chunk_size = chunk_size

    def seekable(self) -> bool:
        return False

    def seek(self, *args: typing.Any) -> int:
        raise io.UnsupportedOperation

    def tell(self) -> int:
        raise io.UnsupportedOperation

    def fileno(self) -> int:
        raise OSError

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            size = self._chunk_size
        return self._buffer.read(min(size, self._chunk_size))


class TestExplicitFileLength:
    def test_declared_length_sets_content_length_for_non_seekable_stream(
        self,
    ) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"<file content>"),
                "text/plain",
                {},
                len(b"<file content>"),
            )
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        assert request.headers["Content-Length"] == str(
            len(
                b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
                b' filename="name.txt"\r\nContent-Type: text/plain\r\n\r\n'
                b"<file content>\r\n--BOUNDARY--\r\n"
            )
        )
        assert "Transfer-Encoding" not in request.headers
        assert request.read() == (
            b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
            b' filename="name.txt"\r\nContent-Type: text/plain\r\n\r\n'
            b"<file content>\r\n--BOUNDARY--\r\n"
        )

    def test_declared_length_without_headers_tuple(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"<file content>"),
                None,
                {},
                14,
            )
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        assert request.headers["Content-Length"] == str(
            len(
                b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
                b' filename="name.txt"\r\nContent-Type: text/plain\r\n\r\n'
                b"<file content>\r\n--BOUNDARY--\r\n"
            )
        )

    def test_empty_file_with_declared_length_is_verified(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": (
                "empty.txt",
                NonSeekableStream(b""),
                "text/plain",
                {},
                0,
            )
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        assert request.headers["Content-Length"] == str(
            len(
                b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
                b' filename="empty.txt"\r\nContent-Type: text/plain\r\n\r\n'
                b"\r\n--BOUNDARY--\r\n"
            )
        )
        assert request.read() == (
            b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
            b' filename="empty.txt"\r\nContent-Type: text/plain\r\n\r\n'
            b"\r\n--BOUNDARY--\r\n"
        )

    def test_empty_file_declared_nonzero_length_fails_during_transfer(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": (
                "empty.txt",
                NonSeekableStream(b""),
                "text/plain",
                {},
                5,
            )
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        with pytest.raises(ValueError, match="shorter") as exc_info:
            request.read()
        assert "file" in str(exc_info.value)

    def test_stream_shorter_than_declared_length_fails_during_transfer(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"short"),
                "text/plain",
                {},
                14,
            )
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        with pytest.raises(ValueError, match="shorter") as exc_info:
            b"".join(request.stream)
        assert "file" in str(exc_info.value)
        assert "14" in str(exc_info.value)

    def test_longer_stream_does_not_leak_extra_bytes_into_next_part(
        self,
    ) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"<file content> plus extra bytes"),
                "text/plain",
                {},
                14,
            )
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        emitted: list[bytes] = []
        with pytest.raises(ValueError, match="longer") as exc_info:
            for chunk in request.stream:
                emitted.append(chunk)
        body = b"".join(emitted)

        assert "file" in str(exc_info.value)
        assert "14" in str(exc_info.value)
        # The closing boundary of the part must never have been written,
        # and the surplus bytes must not appear anywhere in the output.
        assert b"--BOUNDARY--\r\n" not in body
        assert b" plus extra bytes" not in body
        assert body.endswith(b"<file content>")

    def test_mismatch_on_second_field_names_the_offending_field(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = [
            (
                "first",
                (
                    "a.txt",
                    NonSeekableStream(b"aaaa"),
                    "text/plain",
                    {},
                    4,
                ),
            ),
            (
                "second",
                (
                    "b.txt",
                    NonSeekableStream(b"bb"),
                    "text/plain",
                    {},
                    4,
                ),
            ),
        ]

        request = httpx.Request("POST", url, headers=headers, files=files)

        with pytest.raises(ValueError) as exc_info:
            b"".join(request.stream)
        assert "second" in str(exc_info.value)

    def test_unknown_field_length_still_uses_chunked_encoding(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "declared": (
                "a.txt",
                NonSeekableStream(b"aaaa"),
                "text/plain",
                {},
                4,
            ),
            "undeclared": ("b.txt", NonSeekableStream(b"bbbb")),
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        assert "Content-Length" not in request.headers
        assert request.headers["Transfer-Encoding"] == "chunked"

    def test_does_not_read_the_file_before_streaming(self) -> None:
        class ReadTrackingStream(NonSeekableStream):
            def __init__(self, data: bytes) -> None:
                super().__init__(data)
                self.read_count = 0

            def read(self, size: int = -1) -> bytes:
                self.read_count += 1
                return super().read(size)

        stream = ReadTrackingStream(b"<file content>")
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {
            "file": ("name.txt", stream, "text/plain", {}, 14),
        }

        request = httpx.Request("POST", url, headers=headers, files=files)

        assert stream.read_count == 0
        # Building the headers must not consume any of the body either.
        request.stream.get_headers()
        assert stream.read_count == 0

        request.read()
        assert stream.read_count >= 1

    def test_invalid_length_type_raises_type_error(self) -> None:
        url = "https://www.example.com/"
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"<file content>"),
                "text/plain",
                {},
                "14",
            )
        }

        with pytest.raises(TypeError, match="length"):
            httpx.Request("POST", url, files=files)

    def test_negative_length_raises_value_error(self) -> None:
        url = "https://www.example.com/"
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"<file content>"),
                "text/plain",
                {},
                -1,
            )
        }

        with pytest.raises(ValueError, match="file"):
            httpx.Request("POST", url, files=files)

    def test_boolean_length_is_rejected(self) -> None:
        url = "https://www.example.com/"
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"x"),
                "text/plain",
                {},
                True,
            )
        }

        with pytest.raises(TypeError, match="length"):
            httpx.Request("POST", url, files=files)

    def test_bytes_content_with_declared_length_is_verified(self) -> None:
        url = "https://www.example.com/"
        headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}
        files = {"file": ("name.txt", b"<bytes content>", "text/plain", {}, 15)}

        request = httpx.Request("POST", url, headers=headers, files=files)
        assert request.read() == (
            b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
            b' filename="name.txt"\r\nContent-Type: text/plain\r\n\r\n'
            b"<bytes content>\r\n--BOUNDARY--\r\n"
        )

        wrong_files = {"file": ("name.txt", b"<bytes content>", "text/plain", {}, 5)}
        wrong_request = httpx.Request("POST", url, headers=headers, files=wrong_files)
        with pytest.raises(ValueError, match="longer"):
            b"".join(wrong_request.stream)


def test_explicit_file_length_sync_and_async_match() -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}

    def build_request() -> httpx.Request:
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"<file content>"),
                "text/plain",
                {},
                14,
            )
        }
        return httpx.Request("POST", url, headers=headers, files=files)

    sync_body = b"".join(build_request().stream)
    async_body = asyncio.run(_collect_async(build_request()))

    assert async_body == sync_body
    assert sync_body == (
        b'--BOUNDARY\r\nContent-Disposition: form-data; name="file";'
        b' filename="name.txt"\r\nContent-Type: text/plain\r\n\r\n'
        b"<file content>\r\n--BOUNDARY--\r\n"
    )


def test_explicit_file_length_mismatch_sync_and_async_match() -> None:
    url = "https://www.example.com/"
    headers = {"Content-Type": "multipart/form-data; boundary=BOUNDARY"}

    def build_request() -> httpx.Request:
        files = {
            "file": (
                "name.txt",
                NonSeekableStream(b"short"),
                "text/plain",
                {},
                14,
            )
        }
        return httpx.Request("POST", url, headers=headers, files=files)

    with pytest.raises(ValueError, match="shorter"):
        b"".join(build_request().stream)

    with pytest.raises(ValueError, match="shorter"):
        asyncio.run(_collect_async(build_request()))


async def _collect_async(request: httpx.Request) -> bytes:
    return b"".join([chunk async for chunk in request.stream])
