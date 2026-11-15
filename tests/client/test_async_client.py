from __future__ import annotations

import typing
from datetime import timedelta

import pytest

import httpx


@pytest.mark.anyio
async def test_get(server):
    url = server.url
    async with httpx.AsyncClient(http2=True) as client:
        response = await client.get(url)
    assert response.status_code == 200
    assert response.text == "Hello, world!"
    assert response.http_version == "HTTP/1.1"
    assert response.headers
    assert repr(response) == "<Response [200 OK]>"
    assert response.elapsed > timedelta(seconds=0)


@pytest.mark.parametrize(
    "url",
    [
        pytest.param("invalid://example.org", id="scheme-not-http(s)"),
        pytest.param("://example.org", id="no-scheme"),
        pytest.param("http://", id="no-host"),
    ],
)
@pytest.mark.anyio
async def test_get_invalid_url(server, url):
    async with httpx.AsyncClient() as client:
        with pytest.raises((httpx.UnsupportedProtocol, httpx.LocalProtocolError)):
            await client.get(url)


@pytest.mark.anyio
async def test_build_request(server):
    url = server.url.copy_with(path="/echo_headers")
    headers = {"Custom-header": "value"}
    async with httpx.AsyncClient() as client:
        request = client.build_request("GET", url)
        request.headers.update(headers)
        response = await client.send(request)

    assert response.status_code == 200
    assert response.url == url

    assert response.json()["Custom-header"] == "value"


@pytest.mark.anyio
async def test_post(server):
    url = server.url
    async with httpx.AsyncClient() as client:
        response = await client.post(url, content=b"Hello, world!")
    assert response.status_code == 200


@pytest.mark.anyio
async def test_post_json(server):
    url = server.url
    async with httpx.AsyncClient() as client:
        response = await client.post(url, json={"text": "Hello, world!"})
    assert response.status_code == 200


@pytest.mark.anyio
async def test_stream_response(server):
    async with httpx.AsyncClient() as client:
        async with client.stream("GET", server.url) as response:
            body = await response.aread()

    assert response.status_code == 200
    assert body == b"Hello, world!"
    assert response.content == b"Hello, world!"


@pytest.mark.anyio
async def test_access_content_stream_response(server):
    async with httpx.AsyncClient() as client:
        async with client.stream("GET", server.url) as response:
            pass

    assert response.status_code == 200
    with pytest.raises(httpx.ResponseNotRead):
        response.content  # noqa: B018


@pytest.mark.anyio
async def test_stream_request(server):
    async def hello_world() -> typing.AsyncIterator[bytes]:
        yield b"Hello, "
        yield b"world!"

    async with httpx.AsyncClient() as client:
        response = await client.post(server.url, content=hello_world())
    assert response.status_code == 200


@pytest.mark.anyio
async def test_cannot_stream_sync_request(server):
    def hello_world() -> typing.Iterator[bytes]:  # pragma: no cover
        yield b"Hello, "
        yield b"world!"

    async with httpx.AsyncClient() as client:
        with pytest.raises(RuntimeError):
            await client.post(server.url, content=hello_world())


@pytest.mark.anyio
async def test_raise_for_status(server):
    async with httpx.AsyncClient() as client:
        for status_code in (200, 400, 404, 500, 505):
            response = await client.request(
                "GET", server.url.copy_with(path=f"/status/{status_code}")
            )

            if 400 <= status_code < 600:
                with pytest.raises(httpx.HTTPStatusError) as exc_info:
                    response.raise_for_status()
                assert exc_info.value.response == response
            else:
                assert response.raise_for_status() is response


@pytest.mark.anyio
async def test_options(server):
    async with httpx.AsyncClient() as client:
        response = await client.options(server.url)
    assert response.status_code == 200
    assert response.text == "Hello, world!"


@pytest.mark.anyio
async def test_head(server):
    async with httpx.AsyncClient() as client:
        response = await client.head(server.url)
    assert response.status_code == 200
    assert response.text == ""


@pytest.mark.anyio
async def test_put(server):
    async with httpx.AsyncClient() as client:
        response = await client.put(server.url, content=b"Hello, world!")
    assert response.status_code == 200


@pytest.mark.anyio
async def test_patch(server):
    async with httpx.AsyncClient() as client:
        response = await client.patch(server.url, content=b"Hello, world!")
    assert response.status_code == 200


@pytest.mark.anyio
async def test_delete(server):
    async with httpx.AsyncClient() as client:
        response = await client.delete(server.url)
    assert response.status_code == 200
    assert response.text == "Hello, world!"


@pytest.mark.anyio
async def test_100_continue(server):
    headers = {"Expect": "100-continue"}
    content = b"Echo request body"

    async with httpx.AsyncClient() as client:
        response = await client.post(
            server.url.copy_with(path="/echo_body"), headers=headers, content=content
        )

    assert response.status_code == 200
    assert response.content == content


@pytest.mark.anyio
async def test_context_managed_transport():
    class Transport(httpx.AsyncBaseTransport):
        def __init__(self) -> None:
            self.events: list[str] = []

        async def aclose(self):
            # The base implementation of httpx.AsyncBaseTransport just
            # calls into `.aclose`, so simple transport cases can just override
            # this method for any cleanup, where more complex cases
            # might want to additionally override `__aenter__`/`__aexit__`.
            self.events.append("transport.aclose")

        async def __aenter__(self):
            await super().__aenter__()
            self.events.append("transport.__aenter__")

        async def __aexit__(self, *args):
            await super().__aexit__(*args)
            self.events.append("transport.__aexit__")

    transport = Transport()
    async with httpx.AsyncClient(transport=transport):
        pass

    assert transport.events == [
        "transport.__aenter__",
        "transport.aclose",
        "transport.__aexit__",
    ]


@pytest.mark.anyio
async def test_context_managed_transport_and_mount():
    class Transport(httpx.AsyncBaseTransport):
        def __init__(self, name: str) -> None:
            self.name: str = name
            self.events: list[str] = []

        async def aclose(self):
            # The base implementation of httpx.AsyncBaseTransport just
            # calls into `.aclose`, so simple transport cases can just override
            # this method for any cleanup, where more complex cases
            # might want to additionally override `__aenter__`/`__aexit__`.
            self.events.append(f"{self.name}.aclose")

        async def __aenter__(self):
            await super().__aenter__()
            self.events.append(f"{self.name}.__aenter__")

        async def __aexit__(self, *args):
            await super().__aexit__(*args)
            self.events.append(f"{self.name}.__aexit__")

    transport = Transport(name="transport")
    mounted = Transport(name="mounted")
    async with httpx.AsyncClient(
        transport=transport, mounts={"http://www.example.org": mounted}
    ):
        pass

    assert transport.events == [
        "transport.__aenter__",
        "transport.aclose",
        "transport.__aexit__",
    ]
    assert mounted.events == [
        "mounted.__aenter__",
        "mounted.aclose",
        "mounted.__aexit__",
    ]


class CloseCountingTransport(httpx.AsyncBaseTransport):
    """
    A stand-in transport that records how many times it is closed and can
    optionally fail when entered, to exercise ownership and rollback.
    """

    def __init__(self, *, fail_enter: bool = False) -> None:
        self.close_count = 0
        self.enter_count = 0
        self.fail_enter = fail_enter

    async def handle_async_request(
        self, request: httpx.Request
    ) -> httpx.Response:
        return httpx.Response(200)

    async def aclose(self) -> None:
        self.close_count += 1

    async def __aenter__(self) -> "CloseCountingTransport":
        self.enter_count += 1
        if self.fail_enter:
            raise RuntimeError("transport failed to open")
        return self


@pytest.mark.anyio
async def test_borrowed_transport_is_not_closed():
    shared = CloseCountingTransport()

    owner = httpx.AsyncClient(transport=shared)
    borrower = httpx.AsyncClient(transport=shared, borrow_transport=True)

    await owner.aclose()
    await borrower.aclose()

    # The owning client closes it exactly once; the borrower leaves it alone.
    assert shared.close_count == 1


@pytest.mark.anyio
async def test_borrowed_mounts_are_not_closed():
    shared = CloseCountingTransport()

    client = httpx.AsyncClient(
        mounts={"http://www.example.org": shared},
        borrow_mounts=True,
    )
    await client.aclose()

    assert shared.close_count == 0


@pytest.mark.anyio
async def test_same_transport_mounted_on_several_routes_closes_once():
    shared = CloseCountingTransport()

    client = httpx.AsyncClient(
        mounts={
            "http://a.example.org": shared,
            "http://b.example.org": shared,
        }
    )
    await client.aclose()

    assert shared.close_count == 1


@pytest.mark.anyio
async def test_same_transport_as_main_and_mount_closes_once():
    shared = CloseCountingTransport()

    client = httpx.AsyncClient(
        transport=shared,
        mounts={"http://www.example.org": shared},
    )
    await client.aclose()

    assert shared.close_count == 1


@pytest.mark.anyio
async def test_closing_client_twice_closes_owned_transport_once():
    transport = CloseCountingTransport()
    client = httpx.AsyncClient(transport=transport)

    await client.aclose()
    await client.aclose()

    assert transport.close_count == 1


@pytest.mark.anyio
async def test_borrowed_transport_kept_open_across_context_exit():
    shared = CloseCountingTransport()

    async with httpx.AsyncClient(transport=shared, borrow_transport=True):
        pass

    assert shared.close_count == 0

    # The caller is still responsible for the lifetime of the borrowed
    # transport, which remains usable for another client.
    async with httpx.AsyncClient(transport=shared):
        pass

    assert shared.close_count == 1


@pytest.mark.anyio
async def test_context_entry_failure_respects_ownership():
    main = CloseCountingTransport()
    already_entered = CloseCountingTransport()
    failing = CloseCountingTransport(fail_enter=True)

    with pytest.raises(RuntimeError, match="failed to open"):
        async with httpx.AsyncClient(
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


@pytest.mark.anyio
async def test_borrowed_mounts_are_not_entered_on_rollback():
    # The main transport is owned and fails on entry; the mounted transport
    # is borrowed, so the client must never enter or close it.
    failing = CloseCountingTransport(fail_enter=True)
    borrowed = CloseCountingTransport()

    with pytest.raises(RuntimeError, match="failed to open"):
        async with httpx.AsyncClient(
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


@pytest.mark.anyio
async def test_client_closed_state_using_implicit_open():
    client = httpx.AsyncClient(transport=httpx.MockTransport(hello_world))

    assert not client.is_closed
    await client.get("http://example.com")

    assert not client.is_closed
    await client.aclose()

    assert client.is_closed
    # Once we're close we cannot make any more requests.
    with pytest.raises(RuntimeError):
        await client.get("http://example.com")

    # Once we're closed we cannot reopen the client.
    with pytest.raises(RuntimeError):
        async with client:
            pass  # pragma: no cover


@pytest.mark.anyio
async def test_client_closed_state_using_with_block():
    async with httpx.AsyncClient(transport=httpx.MockTransport(hello_world)) as client:
        assert not client.is_closed
        await client.get("http://example.com")

    assert client.is_closed
    with pytest.raises(RuntimeError):
        await client.get("http://example.com")


def unmounted(request: httpx.Request) -> httpx.Response:
    data = {"app": "unmounted"}
    return httpx.Response(200, json=data)


def mounted(request: httpx.Request) -> httpx.Response:
    data = {"app": "mounted"}
    return httpx.Response(200, json=data)


@pytest.mark.anyio
async def test_mounted_transport():
    transport = httpx.MockTransport(unmounted)
    mounts = {"custom://": httpx.MockTransport(mounted)}

    async with httpx.AsyncClient(transport=transport, mounts=mounts) as client:
        response = await client.get("https://www.example.com")
        assert response.status_code == 200
        assert response.json() == {"app": "unmounted"}

        response = await client.get("custom://www.example.com")
        assert response.status_code == 200
        assert response.json() == {"app": "mounted"}


@pytest.mark.anyio
async def test_async_mock_transport():
    async def hello_world(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, text="Hello, world!")

    transport = httpx.MockTransport(hello_world)

    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("https://www.example.com")
        assert response.status_code == 200
        assert response.text == "Hello, world!"


@pytest.mark.anyio
async def test_cancellation_during_stream():
    """
    If any BaseException is raised during streaming the response, then the
    stream should be closed.

    This includes:

    * `asyncio.CancelledError` (A subclass of BaseException from Python 3.8 onwards.)
    * `trio.Cancelled`
    * `KeyboardInterrupt`
    * `SystemExit`

    See https://github.com/encode/httpx/issues/2139
    """
    stream_was_closed = False

    def response_with_cancel_during_stream(request):
        class CancelledStream(httpx.AsyncByteStream):
            async def __aiter__(self) -> typing.AsyncIterator[bytes]:
                yield b"Hello"
                raise KeyboardInterrupt()
                yield b", world"  # pragma: no cover

            async def aclose(self) -> None:
                nonlocal stream_was_closed
                stream_was_closed = True

        return httpx.Response(
            200, headers={"Content-Length": "12"}, stream=CancelledStream()
        )

    transport = httpx.MockTransport(response_with_cancel_during_stream)

    async with httpx.AsyncClient(transport=transport) as client:
        with pytest.raises(KeyboardInterrupt):
            await client.get("https://www.example.com")
        assert stream_was_closed


@pytest.mark.anyio
async def test_server_extensions(server):
    url = server.url
    async with httpx.AsyncClient(http2=True) as client:
        response = await client.get(url)
    assert response.status_code == 200
    assert response.extensions["http_version"] == b"HTTP/1.1"
