from __future__ import annotations

import sys
import typing
import wsgiref.validate
from functools import partial
from io import StringIO

import pytest

import httpx

if typing.TYPE_CHECKING:  # pragma: no cover
    from _typeshed.wsgi import StartResponse, WSGIApplication, WSGIEnvironment


def application_factory(output: typing.Iterable[bytes]) -> WSGIApplication:
    def application(environ, start_response):
        status = "200 OK"

        response_headers = [
            ("Content-type", "text/plain"),
        ]

        start_response(status, response_headers)

        for item in output:
            yield item

    return wsgiref.validate.validator(application)


def echo_body(
    environ: WSGIEnvironment, start_response: StartResponse
) -> typing.Iterable[bytes]:
    status = "200 OK"
    output = environ["wsgi.input"].read()

    response_headers = [
        ("Content-type", "text/plain"),
    ]

    start_response(status, response_headers)

    return [output]


def echo_body_with_response_stream(
    environ: WSGIEnvironment, start_response: StartResponse
) -> typing.Iterable[bytes]:
    status = "200 OK"

    response_headers = [("Content-Type", "text/plain")]

    start_response(status, response_headers)

    def output_generator(f: typing.IO[bytes]) -> typing.Iterator[bytes]:
        while True:
            output = f.read(2)
            if not output:
                break
            yield output

    return output_generator(f=environ["wsgi.input"])


def raise_exc(
    environ: WSGIEnvironment,
    start_response: StartResponse,
    exc: type[Exception] = ValueError,
) -> typing.Iterable[bytes]:
    status = "500 Server Error"
    output = b"Nope!"

    response_headers = [
        ("Content-type", "text/plain"),
    ]

    try:
        raise exc()
    except exc:
        exc_info = sys.exc_info()
        start_response(status, response_headers, exc_info)

    return [output]


def log_to_wsgi_log_buffer(environ, start_response):
    print("test1", file=environ["wsgi.errors"])
    environ["wsgi.errors"].write("test2")
    return echo_body(environ, start_response)


def test_wsgi():
    transport = httpx.WSGITransport(app=application_factory([b"Hello, World!"]))
    client = httpx.Client(transport=transport)
    response = client.get("http://www.example.org/")
    assert response.status_code == 200
    assert response.text == "Hello, World!"


def test_wsgi_upload():
    transport = httpx.WSGITransport(app=echo_body)
    client = httpx.Client(transport=transport)
    response = client.post("http://www.example.org/", content=b"example")
    assert response.status_code == 200
    assert response.text == "example"


def test_wsgi_upload_with_response_stream():
    transport = httpx.WSGITransport(app=echo_body_with_response_stream)
    client = httpx.Client(transport=transport)
    response = client.post("http://www.example.org/", content=b"example")
    assert response.status_code == 200
    assert response.text == "example"


def test_wsgi_exc():
    transport = httpx.WSGITransport(app=raise_exc)
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError):
        client.get("http://www.example.org/")


def test_wsgi_http_error():
    transport = httpx.WSGITransport(app=partial(raise_exc, exc=RuntimeError))
    client = httpx.Client(transport=transport)
    with pytest.raises(RuntimeError):
        client.get("http://www.example.org/")


def test_wsgi_generator():
    output = [b"", b"", b"Some content", b" and more content"]
    transport = httpx.WSGITransport(app=application_factory(output))
    client = httpx.Client(transport=transport)
    response = client.get("http://www.example.org/")
    assert response.status_code == 200
    assert response.text == "Some content and more content"


def test_wsgi_generator_empty():
    output = [b"", b"", b"", b""]
    transport = httpx.WSGITransport(app=application_factory(output))
    client = httpx.Client(transport=transport)
    response = client.get("http://www.example.org/")
    assert response.status_code == 200
    assert response.text == ""


def test_logging():
    buffer = StringIO()
    transport = httpx.WSGITransport(app=log_to_wsgi_log_buffer, wsgi_errors=buffer)
    client = httpx.Client(transport=transport)
    response = client.post("http://www.example.org/", content=b"example")
    assert response.status_code == 200  # no errors
    buffer.seek(0)
    assert buffer.read() == "test1\ntest2"


@pytest.mark.parametrize(
    "url, expected_server_port",
    [
        pytest.param("http://www.example.org", "80", id="auto-http"),
        pytest.param("https://www.example.org", "443", id="auto-https"),
        pytest.param("http://www.example.org:8000", "8000", id="explicit-port"),
    ],
)
def test_wsgi_server_port(url: str, expected_server_port: str) -> None:
    """
    SERVER_PORT is populated correctly from the requested URL.
    """
    hello_world_app = application_factory([b"Hello, World!"])
    server_port: str | None = None

    def app(environ, start_response):
        nonlocal server_port
        server_port = environ["SERVER_PORT"]
        return hello_world_app(environ, start_response)

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    response = client.get(url)
    assert response.status_code == 200
    assert response.text == "Hello, World!"
    assert server_port == expected_server_port


def test_wsgi_server_protocol():
    server_protocol = None

    def app(environ, start_response):
        nonlocal server_protocol
        server_protocol = environ["SERVER_PROTOCOL"]
        start_response("200 OK", [("Content-Type", "text/plain")])
        return [b"success"]

    transport = httpx.WSGITransport(app=app)
    with httpx.Client(transport=transport, base_url="http://testserver") as client:
        response = client.get("/")

    assert response.status_code == 200
    assert response.text == "success"
    assert server_protocol == "HTTP/1.1"


def test_wsgi_deferred_start_response():
    """
    Applications may defer calling `start_response` until the first
    iteration. The response headers are picked up, and the application
    iterable is closed once it is exhausted.
    """
    events: list[str] = []

    def app(environ, start_response):
        events.append("started")
        try:
            start_response("200 OK", [("Content-Type", "text/plain")])
            events.append("responded")
            yield b"Hello, "
            events.append("yielded")
            yield b"World!"
        finally:
            events.append("closed")

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    response = client.get("http://www.example.org/")
    assert response.status_code == 200
    assert response.text == "Hello, World!"
    assert events == ["started", "responded", "yielded", "closed"]


def test_wsgi_close_called_once_on_exhaustion():
    events: list[str] = []

    class Body:
        def __iter__(self):
            events.append("iterated")
            yield b"Hello, World!"

        def close(self):
            events.append("closed")

    def app(environ, start_response):
        start_response("200 OK", [("Content-Type", "text/plain")])
        return Body()

    transport = httpx.WSGITransport(app=app)
    with httpx.Client(transport=transport) as client:
        response = client.get("http://www.example.org/")
        assert response.text == "Hello, World!"
    assert events == ["iterated", "closed"]


def test_wsgi_close_on_client_disconnect():
    """
    Closing the response before it is fully read closes the
    application iterable.
    """
    events: list[str] = []

    def app(environ, start_response):
        start_response("200 OK", [("Content-Type", "text/plain")])
        try:
            yield b"chunk-1"
            yield b"chunk-2"
            yield b"chunk-3"
        finally:
            events.append("closed")

    transport = httpx.WSGITransport(app=app)
    with httpx.Client(transport=transport) as client:
        with client.stream("GET", "http://www.example.org/") as response:
            assert next(response.iter_bytes()) == b"chunk-1"
    assert events == ["closed"]


def test_wsgi_close_on_iteration_error():
    """
    An exception raised while iterating the application still closes
    the application iterable, and propagates to the client.
    """
    events: list[str] = []

    class Body:
        def __iter__(self):
            events.append("iterated")
            yield b"partial"
            raise ValueError("iteration failed")

        def close(self):
            events.append("closed")

    def app(environ, start_response):
        start_response("200 OK", [("Content-Type", "text/plain")])
        return Body()

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError, match="iteration failed"):
        client.get("http://www.example.org/")
    assert events == ["iterated", "closed"]


def test_wsgi_close_error_does_not_mask_iteration_error():
    """
    A failing `close()` must not hide an exception raised while
    iterating the application.
    """

    class Body:
        def __iter__(self):
            yield b"partial"
            raise ValueError("iteration failed")

        def close(self):
            raise RuntimeError("close failed")

    def app(environ, start_response):
        start_response("200 OK", [("Content-Type", "text/plain")])
        return Body()

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError, match="iteration failed"):
        client.get("http://www.example.org/")


def test_wsgi_write_callable():
    """
    Output passed to the `write()` callable is delivered in the order
    the application produced it, interleaved with the returned iterable.
    """
    events: list[str] = []

    def app(environ, start_response):
        write = start_response("200 OK", [("Content-Type", "text/plain")])
        events.append("responded")
        write(b"1")
        yield b"2"
        write(b"3")
        yield b"4"
        write(b"5")

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    response = client.get("http://www.example.org/")
    assert response.status_code == 200
    assert response.text == "12345"
    assert events == ["responded"]


def test_wsgi_write_callable_with_eager_start_response():
    def app(environ, start_response):
        write = start_response("200 OK", [("Content-Type", "text/plain")])
        write(b"Hello, ")
        return [b"World!"]

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    response = client.get("http://www.example.org/")
    assert response.status_code == 200
    assert response.text == "Hello, World!"


def test_wsgi_head_request_runs_app_cleanup():
    """
    HEAD responses have no body, but the application is still run to
    completion and its iterable is closed.
    """
    events: list[str] = []

    def app(environ, start_response):
        events.append("started")
        try:
            start_response(
                "200 OK",
                [("Content-Type", "text/plain"), ("Content-Length", "13")],
            )
            events.append("responded")
            if environ["REQUEST_METHOD"] != "HEAD":
                yield b"Hello, World!"
        finally:
            events.append("closed")

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    response = client.head("http://www.example.org/")
    assert response.status_code == 200
    assert response.content == b""
    assert events == ["started", "responded", "closed"]


def test_wsgi_exc_info_closes_iterable():
    """
    An application reporting an error through `exc_info` still has its
    returned iterable closed when the exception is re-raised.
    """
    events: list[str] = []

    class Body:
        def __iter__(self):
            return iter([b"Nope!"])

        def close(self):
            events.append("closed")

    def app(environ, start_response):
        try:
            raise ValueError("application failed")
        except ValueError:
            start_response(
                "500 Server Error",
                [("Content-Type", "text/plain")],
                sys.exc_info(),
            )
        return Body()

    transport = httpx.WSGITransport(app=app)
    client = httpx.Client(transport=transport)
    with pytest.raises(ValueError, match="application failed"):
        client.get("http://www.example.org/")
    assert events == ["closed"]
