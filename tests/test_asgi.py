import json

import anyio
import pytest

import httpx


async def hello_world(scope, receive, send):
    status = 200
    output = b"Hello, World!"
    headers = [(b"content-type", "text/plain"), (b"content-length", str(len(output)))]

    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": output})


async def echo_path(scope, receive, send):
    status = 200
    output = json.dumps({"path": scope["path"]}).encode("utf-8")
    headers = [(b"content-type", "text/plain"), (b"content-length", str(len(output)))]

    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": output})


async def echo_raw_path(scope, receive, send):
    status = 200
    output = json.dumps({"raw_path": scope["raw_path"].decode("ascii")}).encode("utf-8")
    headers = [(b"content-type", "text/plain"), (b"content-length", str(len(output)))]

    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": output})


async def echo_body(scope, receive, send):
    status = 200
    headers = [(b"content-type", "text/plain")]

    await send({"type": "http.response.start", "status": status, "headers": headers})
    more_body = True
    while more_body:
        message = await receive()
        body = message.get("body", b"")
        more_body = message.get("more_body", False)
        await send({"type": "http.response.body", "body": body, "more_body": more_body})


async def echo_headers(scope, receive, send):
    status = 200
    output = json.dumps(
        {"headers": [[k.decode(), v.decode()] for k, v in scope["headers"]]}
    ).encode("utf-8")
    headers = [(b"content-type", "text/plain"), (b"content-length", str(len(output)))]

    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": output})


async def raise_exc(scope, receive, send):
    raise RuntimeError()


async def raise_exc_after_response(scope, receive, send):
    status = 200
    output = b"Hello, World!"
    headers = [(b"content-type", "text/plain"), (b"content-length", str(len(output)))]

    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": output})
    raise RuntimeError()


@pytest.mark.anyio
async def test_asgi_transport():
    async with httpx.ASGITransport(app=hello_world) as transport:
        request = httpx.Request("GET", "http://www.example.com/")
        response = await transport.handle_async_request(request)
        await response.aread()
        assert response.status_code == 200
        assert response.content == b"Hello, World!"


@pytest.mark.anyio
async def test_asgi_transport_no_body():
    async with httpx.ASGITransport(app=echo_body) as transport:
        request = httpx.Request("GET", "http://www.example.com/")
        response = await transport.handle_async_request(request)
        await response.aread()
        assert response.status_code == 200
        assert response.content == b""


@pytest.mark.anyio
async def test_asgi():
    transport = httpx.ASGITransport(app=hello_world)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

    assert response.status_code == 200
    assert response.text == "Hello, World!"


@pytest.mark.anyio
async def test_asgi_urlencoded_path():
    transport = httpx.ASGITransport(app=echo_path)
    async with httpx.AsyncClient(transport=transport) as client:
        url = httpx.URL("http://www.example.org/").copy_with(path="/user@example.org")
        response = await client.get(url)

    assert response.status_code == 200
    assert response.json() == {"path": "/user@example.org"}


@pytest.mark.anyio
async def test_asgi_raw_path():
    transport = httpx.ASGITransport(app=echo_raw_path)
    async with httpx.AsyncClient(transport=transport) as client:
        url = httpx.URL("http://www.example.org/").copy_with(path="/user@example.org")
        response = await client.get(url)

    assert response.status_code == 200
    assert response.json() == {"raw_path": "/user@example.org"}


@pytest.mark.anyio
async def test_asgi_raw_path_should_not_include_querystring_portion():
    """
    See https://github.com/encode/httpx/issues/2810
    """
    transport = httpx.ASGITransport(app=echo_raw_path)
    async with httpx.AsyncClient(transport=transport) as client:
        url = httpx.URL("http://www.example.org/path?query")
        response = await client.get(url)

    assert response.status_code == 200
    assert response.json() == {"raw_path": "/path"}


@pytest.mark.anyio
async def test_asgi_upload():
    transport = httpx.ASGITransport(app=echo_body)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.post("http://www.example.org/", content=b"example")

    assert response.status_code == 200
    assert response.text == "example"


@pytest.mark.anyio
async def test_asgi_headers():
    transport = httpx.ASGITransport(app=echo_headers)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

    assert response.status_code == 200
    assert response.json() == {
        "headers": [
            ["host", "www.example.org"],
            ["accept", "*/*"],
            ["accept-encoding", "gzip, deflate, br, zstd"],
            ["connection", "keep-alive"],
            ["user-agent", f"python-httpx/{httpx.__version__}"],
        ]
    }


@pytest.mark.anyio
async def test_asgi_exc():
    transport = httpx.ASGITransport(app=raise_exc)
    async with httpx.AsyncClient(transport=transport) as client:
        with pytest.raises(RuntimeError):
            await client.get("http://www.example.org/")


@pytest.mark.anyio
async def test_asgi_exc_after_response():
    transport = httpx.ASGITransport(app=raise_exc_after_response)
    async with httpx.AsyncClient(transport=transport) as client:
        with pytest.raises(RuntimeError):
            await client.get("http://www.example.org/")


@pytest.mark.anyio
async def test_asgi_disconnect_after_response_complete():
    disconnect = False

    async def read_body(scope, receive, send):
        nonlocal disconnect

        status = 200
        headers = [(b"content-type", "text/plain")]

        await send(
            {"type": "http.response.start", "status": status, "headers": headers}
        )
        more_body = True
        while more_body:
            message = await receive()
            more_body = message.get("more_body", False)

        await send({"type": "http.response.body", "body": b"", "more_body": False})

        # The ASGI spec says of the Disconnect message:
        # "Sent to the application when a HTTP connection is closed or if receive is
        # called after a response has been sent."
        # So if receive() is called again, the disconnect message should be received
        message = await receive()
        disconnect = message.get("type") == "http.disconnect"

    transport = httpx.ASGITransport(app=read_body)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.post("http://www.example.org/", content=b"example")

    assert response.status_code == 200
    assert disconnect


@pytest.mark.anyio
async def test_asgi_exc_no_raise():
    transport = httpx.ASGITransport(app=raise_exc, raise_app_exceptions=False)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

        assert response.status_code == 500


@pytest.mark.anyio
async def test_asgi_streaming_transport():
    transport = httpx.ASGITransport(app=hello_world, streaming=True)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

    assert response.status_code == 200
    assert response.text == "Hello, World!"
    assert not transport._open_streams


@pytest.mark.anyio
async def test_asgi_streaming_upload():
    transport = httpx.ASGITransport(app=echo_body, streaming=True)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.post("http://www.example.org/", content=b"example")

    assert response.status_code == 200
    assert response.text == "example"


@pytest.mark.anyio
async def test_asgi_streaming_client_driven():
    """
    The application only sends the next chunk once the client has read the
    previous one. A transport that buffered the whole response would never
    return it here, so this deadlocks unless the response is genuinely
    streamed back to the client.
    """
    chunk_read = [anyio.Event() for _ in range(5)]

    async def client_driven(scope, receive, send):
        headers = [(b"content-type", "text/plain")]
        await send(
            {"type": "http.response.start", "status": 200, "headers": headers}
        )
        for index in range(5):
            await send(
                {
                    "type": "http.response.body",
                    "body": f"chunk-{index}".encode(),
                    "more_body": True,
                }
            )
            await chunk_read[index].wait()
        await send({"type": "http.response.body", "body": b""})

    transport = httpx.ASGITransport(
        app=client_driven, streaming=True, max_buffered_chunks=1
    )
    async with httpx.AsyncClient(transport=transport) as client:
        async with client.stream("GET", "http://www.example.org/") as response:
            chunks = []
            async for chunk in response.aiter_bytes():
                chunks.append(chunk)
                chunk_read[len(chunks) - 1].set()

    assert response.status_code == 200
    assert chunks == [f"chunk-{index}".encode() for index in range(5)]


@pytest.mark.anyio
async def test_asgi_streaming_backpressure():
    """
    The application may not run further ahead of the client than the
    bounded chunk buffer allows.
    """
    sent_chunks = []

    async def backpressure(scope, receive, send):
        headers = [(b"content-type", "text/plain")]
        await send(
            {"type": "http.response.start", "status": 200, "headers": headers}
        )
        for index in range(5):
            await send(
                {
                    "type": "http.response.body",
                    "body": f"chunk-{index}".encode(),
                    "more_body": True,
                }
            )
            sent_chunks.append(index)
        await send({"type": "http.response.body", "body": b""})

    transport = httpx.ASGITransport(
        app=backpressure, streaming=True, max_buffered_chunks=2
    )
    async with httpx.AsyncClient(transport=transport) as client:
        async with client.stream("GET", "http://www.example.org/") as response:
            # Give the application the opportunity to run ahead.
            await anyio.sleep(0.1)
            assert len(sent_chunks) <= 2
            content = await response.aread()

    assert sent_chunks == [0, 1, 2, 3, 4]
    assert content == b"chunk-0chunk-1chunk-2chunk-3chunk-4"


@pytest.mark.anyio
async def test_asgi_streaming_disconnect():
    """
    Closing the response before it has completed must notify the application
    with an `http.disconnect` message and wind down the application task.
    """
    disconnect_received = anyio.Event()
    app_finished = anyio.Event()

    async def streaming_app(scope, receive, send):
        try:
            headers = [(b"content-type", "text/plain")]
            await send(
                {"type": "http.response.start", "status": 200, "headers": headers}
            )
            await send(
                {
                    "type": "http.response.body",
                    "body": b"chunk-0",
                    "more_body": True,
                }
            )
            while True:
                message = await receive()
                if message["type"] == "http.disconnect":
                    disconnect_received.set()
                    return
        finally:
            app_finished.set()

    async with httpx.ASGITransport(app=streaming_app, streaming=True) as transport:
        request = httpx.Request("GET", "http://www.example.org/")
        response = await transport.handle_async_request(request)
        assert response.status_code == 200

        assert isinstance(response.stream, httpx.AsyncByteStream)
        stream = response.stream.__aiter__()
        assert await stream.__anext__() == b"chunk-0"
        # The client goes away part-way through the response.
        await stream.aclose()  # type: ignore[attr-defined]
        await response.aclose()

    assert disconnect_received.is_set()
    assert app_finished.is_set()
    assert not transport._open_streams


@pytest.mark.anyio
async def test_asgi_streaming_disconnect_after_response_complete():
    """
    If the application is still running once the response is complete, then
    closing the response must notify it with an `http.disconnect` message.
    """
    disconnect_received = anyio.Event()

    async def streaming_app(scope, receive, send):
        headers = [(b"content-type", "text/plain")]
        await send(
            {"type": "http.response.start", "status": 200, "headers": headers}
        )
        await send({"type": "http.response.body", "body": b"Hello, World!"})

        more_body = True
        while more_body:
            message = await receive()
            more_body = message.get("more_body", False)

        message = await receive()
        if message["type"] == "http.disconnect":
            disconnect_received.set()

    transport = httpx.ASGITransport(app=streaming_app, streaming=True)
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

    assert response.status_code == 200
    assert response.text == "Hello, World!"
    assert disconnect_received.is_set()


@pytest.mark.anyio
async def test_asgi_streaming_transport_close_cleans_up_tasks():
    """
    Closing the transport must wind down application tasks, even if the
    response was never consumed or closed.
    """
    app_finished = anyio.Event()

    async def endless_app(scope, receive, send):
        try:
            headers = [(b"content-type", "text/plain")]
            await send(
                {"type": "http.response.start", "status": 200, "headers": headers}
            )
            while True:
                await send(
                    {
                        "type": "http.response.body",
                        "body": b"chunk",
                        "more_body": True,
                    }
                )
        finally:
            app_finished.set()

    transport = httpx.ASGITransport(
        app=endless_app, streaming=True, max_buffered_chunks=2
    )
    async with transport:
        request = httpx.Request("GET", "http://www.example.org/")
        response = await transport.handle_async_request(request)
        assert response.status_code == 200
        # Deliberately neither consume nor close the response.

    assert app_finished.is_set()
    assert not transport._open_streams


@pytest.mark.anyio
async def test_asgi_streaming_exc_before_response():
    transport = httpx.ASGITransport(app=raise_exc, streaming=True)
    async with httpx.AsyncClient(transport=transport) as client:
        with pytest.raises(RuntimeError):
            await client.get("http://www.example.org/")


@pytest.mark.anyio
async def test_asgi_streaming_exc_before_response_no_raise():
    transport = httpx.ASGITransport(
        app=raise_exc, raise_app_exceptions=False, streaming=True
    )
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

    assert response.status_code == 500


@pytest.mark.anyio
async def test_asgi_streaming_exc_after_response():
    transport = httpx.ASGITransport(app=raise_exc_after_response, streaming=True)
    async with httpx.AsyncClient(transport=transport) as client:
        with pytest.raises(RuntimeError):
            await client.get("http://www.example.org/")


@pytest.mark.anyio
async def test_asgi_streaming_exc_after_response_no_raise():
    transport = httpx.ASGITransport(
        app=raise_exc_after_response, raise_app_exceptions=False, streaming=True
    )
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.get("http://www.example.org/")

    assert response.status_code == 200
    assert response.content == b"Hello, World!"


@pytest.mark.anyio
async def test_asgi_streaming_head_body_not_buffered():
    """
    For HEAD requests the response body is discarded. The discarded body
    must not be buffered, otherwise the application would block once the
    bounded buffer fills up.
    """
    app_finished = anyio.Event()

    async def body_for_head(scope, receive, send):
        headers = [(b"content-type", "text/plain")]
        await send(
            {"type": "http.response.start", "status": 200, "headers": headers}
        )
        for _ in range(100):
            await send(
                {
                    "type": "http.response.body",
                    "body": b"x" * 64,
                    "more_body": True,
                }
            )
        await send({"type": "http.response.body", "body": b""})
        app_finished.set()

    transport = httpx.ASGITransport(
        app=body_for_head, streaming=True, max_buffered_chunks=1
    )
    async with httpx.AsyncClient(transport=transport) as client:
        response = await client.head("http://www.example.org/")

    assert response.status_code == 200
    assert response.content == b""
    assert app_finished.is_set()
