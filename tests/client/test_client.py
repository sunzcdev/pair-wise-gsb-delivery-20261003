from __future__ import annotations

import typing
from datetime import timedelta

import chardet
import pytest

import httpx


def autodetect(content):
    return chardet.detect(content).get("encoding")


def test_get(server):
    url = server.url
    with httpx.Client(http2=True) as http:
        response = http.get(url)
    assert response.status_code == 200
    assert response.url == url
    assert response.content == b"Hello, world!"
    assert response.text == "Hello, world!"
    assert response.http_version == "HTTP/1.1"
    assert response.encoding == "utf-8"
    assert response.request.url == url
    assert response.headers
    assert response.is_redirect is False
    assert repr(response) == "<Response [200 OK]>"
    assert response.elapsed > timedelta(0)


@pytest.mark.parametrize(
    "url",
    [
        pytest.param("invalid://example.org", id="scheme-not-http(s)"),
        pytest.param("://example.org", id="no-scheme"),
        pytest.param("http://", id="no-host"),
    ],
)
def test_get_invalid_url(server, url):
    with httpx.Client() as client:
        with pytest.raises((httpx.UnsupportedProtocol, httpx.LocalProtocolError)):
            client.get(url)


def test_build_request(server):
    url = server.url.copy_with(path="/echo_headers")
    headers = {"Custom-header": "value"}

    with httpx.Client() as client:
        request = client.build_request("GET", url)
        request.headers.update(headers)
        response = client.send(request)

    assert response.status_code == 200
    assert response.url == url

    assert response.json()["Custom-header"] == "value"


def test_build_post_request(server):
    url = server.url.copy_with(path="/echo_headers")
    headers = {"Custom-header": "value"}

    with httpx.Client() as client:
        request = client.build_request("POST", url)
        request.headers.update(headers)
        response = client.send(request)

    assert response.status_code == 200
    assert response.url == url

    assert response.json()["Content-length"] == "0"
    assert response.json()["Custom-header"] == "value"


def test_post(server):
    with httpx.Client() as client:
        response = client.post(server.url, content=b"Hello, world!")
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_post_json(server):
    with httpx.Client() as client:
        response = client.post(server.url, json={"text": "Hello, world!"})
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_stream_response(server):
    with httpx.Client() as client:
        with client.stream("GET", server.url) as response:
            content = response.read()
    assert response.status_code == 200
    assert content == b"Hello, world!"


def test_stream_iterator(server):
    body = b""

    with httpx.Client() as client:
        with client.stream("GET", server.url) as response:
            for chunk in response.iter_bytes():
                body += chunk

    assert response.status_code == 200
    assert body == b"Hello, world!"


def test_raw_iterator(server):
    body = b""

    with httpx.Client() as client:
        with client.stream("GET", server.url) as response:
            for chunk in response.iter_raw():
                body += chunk

    assert response.status_code == 200
    assert body == b"Hello, world!"


def test_cannot_stream_async_request(server):
    async def hello_world() -> typing.AsyncIterator[bytes]:  # pragma: no cover
        yield b"Hello, "
        yield b"world!"

    with httpx.Client() as client:
        with pytest.raises(RuntimeError):
            client.post(server.url, content=hello_world())


def test_raise_for_status(server):
    with httpx.Client() as client:
        for status_code in (200, 400, 404, 500, 505):
            response = client.request(
                "GET", server.url.copy_with(path=f"/status/{status_code}")
            )
            if 400 <= status_code < 600:
                with pytest.raises(httpx.HTTPStatusError) as exc_info:
                    response.raise_for_status()
                assert exc_info.value.response == response
                assert exc_info.value.request.url.path == f"/status/{status_code}"
            else:
                assert response.raise_for_status() is response


def test_options(server):
    with httpx.Client() as client:
        response = client.options(server.url)
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_head(server):
    with httpx.Client() as client:
        response = client.head(server.url)
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_put(server):
    with httpx.Client() as client:
        response = client.put(server.url, content=b"Hello, world!")
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_patch(server):
    with httpx.Client() as client:
        response = client.patch(server.url, content=b"Hello, world!")
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_delete(server):
    with httpx.Client() as client:
        response = client.delete(server.url)
    assert response.status_code == 200
    assert response.reason_phrase == "OK"


def test_base_url(server):
    base_url = server.url
    with httpx.Client(base_url=base_url) as client:
        response = client.get("/")
    assert response.status_code == 200
    assert response.url == base_url


def test_merge_absolute_url():
    client = httpx.Client(base_url="https://www.example.com/")
    request = client.build_request("GET", "http://www.example.com/")
    assert request.url == "http://www.example.com/"


def test_merge_relative_url():
    client = httpx.Client(base_url="https://www.example.com/")
    request = client.build_request("GET", "/testing/123")
    assert request.url == "https://www.example.com/testing/123"


def test_merge_relative_url_with_path():
    client = httpx.Client(base_url="https://www.example.com/some/path")
    request = client.build_request("GET", "/testing/123")
    assert request.url == "https://www.example.com/some/path/testing/123"


def test_merge_relative_url_with_dotted_path():
    client = httpx.Client(base_url="https://www.example.com/some/path")
    request = client.build_request("GET", "../testing/123")
    assert request.url == "https://www.example.com/some/testing/123"


def test_merge_relative_url_with_path_including_colon():
    client = httpx.Client(base_url="https://www.example.com/some/path")
    request = client.build_request("GET", "/testing:123")
    assert request.url == "https://www.example.com/some/path/testing:123"


def test_merge_relative_url_with_encoded_slashes():
    client = httpx.Client(base_url="https://www.example.com/")
    request = client.build_request("GET", "/testing%2F123")
    assert request.url == "https://www.example.com/testing%2F123"

    client = httpx.Client(base_url="https://www.example.com/base%2Fpath")
    request = client.build_request("GET", "/testing")
    assert request.url == "https://www.example.com/base%2Fpath/testing"


def test_context_managed_transport():
    class Transport(httpx.BaseTransport):
        def __init__(self) -> None:
            self.events: list[str] = []

        def close(self):
            # The base implementation of httpx.BaseTransport just
            # calls into `.close`, so simple transport cases can just override
            # this method for any cleanup, where more complex cases
            # might want to additionally override `__enter__`/`__exit__`.
            self.events.append("transport.close")

        def __enter__(self):
            super().__enter__()
            self.events.append("transport.__enter__")

        def __exit__(self, *args):
            super().__exit__(*args)
            self.events.append("transport.__exit__")

    transport = Transport()
    with httpx.Client(transport=transport):
        pass

    assert transport.events == [
        "transport.__enter__",
        "transport.close",
        "transport.__exit__",
    ]


def test_context_managed_transport_and_mount():
    class Transport(httpx.BaseTransport):
        def __init__(self, name: str) -> None:
            self.name: str = name
            self.events: list[str] = []

        def close(self):
            # The base implementation of httpx.BaseTransport just
            # calls into `.close`, so simple transport cases can just override
            # this method for any cleanup, where more complex cases
            # might want to additionally override `__enter__`/`__exit__`.
            self.events.append(f"{self.name}.close")

        def __enter__(self):
            super().__enter__()
            self.events.append(f"{self.name}.__enter__")

        def __exit__(self, *args):
            super().__exit__(*args)
            self.events.append(f"{self.name}.__exit__")

    transport = Transport(name="transport")
    mounted = Transport(name="mounted")
    with httpx.Client(transport=transport, mounts={"http://www.example.org": mounted}):
        pass

    assert transport.events == [
        "transport.__enter__",
        "transport.close",
        "transport.__exit__",
    ]
    assert mounted.events == [
        "mounted.__enter__",
        "mounted.close",
        "mounted.__exit__",
    ]


class CloseCountingTransport(httpx.BaseTransport):
    """
    A stand-in transport that records how many times it is closed and can
    optionally fail when entered, to exercise ownership and rollback.
    """

    def __init__(self, *, fail_enter: bool = False) -> None:
        self.close_count = 0
        self.enter_count = 0
        self.fail_enter = fail_enter

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        return httpx.Response(200)

    def close(self) -> None:
        self.close_count += 1

    def __enter__(self) -> "CloseCountingTransport":
        self.enter_count += 1
        if self.fail_enter:
            raise RuntimeError("transport failed to open")
        return self


def test_borrowed_transport_is_not_closed():
    shared = CloseCountingTransport()

    owner = httpx.Client(transport=shared)
    borrower = httpx.Client(transport=shared, borrow_transport=True)

    owner.close()
    borrower.close()

    # The owning client closes it exactly once; the borrower leaves it alone.
    assert shared.close_count == 1


def test_borrowed_mounts_are_not_closed():
    shared = CloseCountingTransport()

    client = httpx.Client(
        mounts={"http://www.example.org": shared},
        borrow_mounts=True,
    )
    client.close()

    assert shared.close_count == 0


def test_same_transport_mounted_on_several_routes_closes_once():
    shared = CloseCountingTransport()

    client = httpx.Client(
        mounts={
            "http://a.example.org": shared,
            "http://b.example.org": shared,
        }
    )
    client.close()

    assert shared.close_count == 1


def test_same_transport_as_main_and_mount_closes_once():
    shared = CloseCountingTransport()

    client = httpx.Client(
        transport=shared,
        mounts={"http://www.example.org": shared},
    )
    client.close()

    assert shared.close_count == 1


def test_closing_client_twice_closes_owned_transport_once():
    transport = CloseCountingTransport()
    client = httpx.Client(transport=transport)

    client.close()
    client.close()

    assert transport.close_count == 1


def test_borrowed_transport_kept_open_across_context_exit():
    shared = CloseCountingTransport()

    with httpx.Client(transport=shared, borrow_transport=True):
        pass

    assert shared.close_count == 0

    # The caller is still responsible for the lifetime of the borrowed
    # transport, which remains usable for another client.
    with httpx.Client(transport=shared):
        pass

    assert shared.close_count == 1


def test_context_entry_failure_respects_ownership():
    main = CloseCountingTransport()
    already_entered = CloseCountingTransport()
    failing = CloseCountingTransport(fail_enter=True)

    with pytest.raises(RuntimeError, match="failed to open"):
        with httpx.Client(
            transport=main,
            mounts={
                "http://good-long.example.org": already_entered,
                "http://x-owned.example.org": failing,
            },
        ):
            pass  # pragma: no cover

    # Owned transports that were already opened are rolled back...
    assert main.enter_count == 1
    assert main.close_count == 1
    assert already_entered.enter_count == 1
    assert already_entered.close_count == 1
    # ...the transport that failed is not closed a second time by the client...
    assert failing.enter_count == 1
    assert failing.close_count == 0


def test_borrowed_mounts_are_not_entered_on_rollback():
    # The main transport is owned and fails on entry; the mounted transport
    # is borrowed, so the client must never enter or close it.
    failing = CloseCountingTransport(fail_enter=True)
    borrowed = CloseCountingTransport()

    with pytest.raises(RuntimeError, match="failed to open"):
        with httpx.Client(
            transport=failing,
            mounts={
                "http://www.example.org": borrowed,
            },
            borrow_mounts=True,
        ):
            pass  # pragma: no cover

    # The owned main transport failed while opening...
    assert failing.enter_count == 1
    assert failing.close_count == 0
    # ...and the borrowed mount is never entered or closed by the client.
    assert borrowed.enter_count == 0
    assert borrowed.close_count == 0


def hello_world(request):
    return httpx.Response(200, text="Hello, world!")


def test_client_closed_state_using_implicit_open():
    client = httpx.Client(transport=httpx.MockTransport(hello_world))

    assert not client.is_closed
    client.get("http://example.com")

    assert not client.is_closed
    client.close()

    assert client.is_closed

    # Once we're close we cannot make any more requests.
    with pytest.raises(RuntimeError):
        client.get("http://example.com")

    # Once we're closed we cannot reopen the client.
    with pytest.raises(RuntimeError):
        with client:
            pass  # pragma: no cover


def test_client_closed_state_using_with_block():
    with httpx.Client(transport=httpx.MockTransport(hello_world)) as client:
        assert not client.is_closed
        client.get("http://example.com")

    assert client.is_closed
    with pytest.raises(RuntimeError):
        client.get("http://example.com")


def echo_raw_headers(request: httpx.Request) -> httpx.Response:
    data = [
        (name.decode("ascii"), value.decode("ascii"))
        for name, value in request.headers.raw
    ]
    return httpx.Response(200, json=data)


def test_raw_client_header():
    """
    Set a header in the Client.
    """
    url = "http://example.org/echo_headers"
    headers = {"Example-Header": "example-value"}

    client = httpx.Client(
        transport=httpx.MockTransport(echo_raw_headers), headers=headers
    )
    response = client.get(url)

    assert response.status_code == 200
    assert response.json() == [
        ["Host", "example.org"],
        ["Accept", "*/*"],
        ["Accept-Encoding", "gzip, deflate, br, zstd"],
        ["Connection", "keep-alive"],
        ["User-Agent", f"python-httpx/{httpx.__version__}"],
        ["Example-Header", "example-value"],
    ]


def unmounted(request: httpx.Request) -> httpx.Response:
    data = {"app": "unmounted"}
    return httpx.Response(200, json=data)


def mounted(request: httpx.Request) -> httpx.Response:
    data = {"app": "mounted"}
    return httpx.Response(200, json=data)


def test_mounted_transport():
    transport = httpx.MockTransport(unmounted)
    mounts = {"custom://": httpx.MockTransport(mounted)}

    client = httpx.Client(transport=transport, mounts=mounts)

    response = client.get("https://www.example.com")
    assert response.status_code == 200
    assert response.json() == {"app": "unmounted"}

    response = client.get("custom://www.example.com")
    assert response.status_code == 200
    assert response.json() == {"app": "mounted"}


def test_all_mounted_transport():
    mounts = {"all://": httpx.MockTransport(mounted)}

    client = httpx.Client(mounts=mounts)

    response = client.get("https://www.example.com")
    assert response.status_code == 200
    assert response.json() == {"app": "mounted"}


def test_server_extensions(server):
    url = server.url.copy_with(path="/http_version_2")
    with httpx.Client(http2=True) as client:
        response = client.get(url)
    assert response.status_code == 200
    assert response.extensions["http_version"] == b"HTTP/1.1"


def test_client_decode_text_using_autodetect():
    # Ensure that a 'default_encoding=autodetect' on the response allows for
    # encoding autodetection to be used when no "Content-Type: text/plain; charset=..."
    # info is present.
    #
    # Here we have some french text encoded with ISO-8859-1, rather than UTF-8.
    text = (
        "Non-seulement Despréaux ne se trompait pas, mais de tous les écrivains "
        "que la France a produits, sans excepter Voltaire lui-même, imprégné de "
        "l'esprit anglais par son séjour à Londres, c'est incontestablement "
        "Molière ou Poquelin qui reproduit avec l'exactitude la plus vive et la "
        "plus complète le fond du génie français."
    )

    def cp1252_but_no_content_type(request):
        content = text.encode("ISO-8859-1")
        return httpx.Response(200, content=content)

    transport = httpx.MockTransport(cp1252_but_no_content_type)
    with httpx.Client(transport=transport, default_encoding=autodetect) as client:
        response = client.get("http://www.example.com")

        assert response.status_code == 200
        assert response.reason_phrase == "OK"
        assert response.encoding == "ISO-8859-1"
        assert response.text == text


def test_client_decode_text_using_explicit_encoding():
    # Ensure that a 'default_encoding="..."' on the response is used for text decoding
    # when no "Content-Type: text/plain; charset=..."" info is present.
    #
    # Here we have some french text encoded with ISO-8859-1, rather than UTF-8.
    text = (
        "Non-seulement Despréaux ne se trompait pas, mais de tous les écrivains "
        "que la France a produits, sans excepter Voltaire lui-même, imprégné de "
        "l'esprit anglais par son séjour à Londres, c'est incontestablement "
        "Molière ou Poquelin qui reproduit avec l'exactitude la plus vive et la "
        "plus complète le fond du génie français."
    )

    def cp1252_but_no_content_type(request):
        content = text.encode("ISO-8859-1")
        return httpx.Response(200, content=content)

    transport = httpx.MockTransport(cp1252_but_no_content_type)
    with httpx.Client(transport=transport, default_encoding=autodetect) as client:
        response = client.get("http://www.example.com")

        assert response.status_code == 200
        assert response.reason_phrase == "OK"
        assert response.encoding == "ISO-8859-1"
        assert response.text == text
