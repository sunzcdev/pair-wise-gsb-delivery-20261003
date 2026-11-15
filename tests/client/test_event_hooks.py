import pytest

import httpx


def app(request: httpx.Request) -> httpx.Response:
    if request.url.path == "/redirect":
        return httpx.Response(303, headers={"server": "testserver", "location": "/"})
    elif request.url.path.startswith("/status/"):
        status_code = int(request.url.path[-3:])
        return httpx.Response(status_code, headers={"server": "testserver"})

    return httpx.Response(200, headers={"server": "testserver"})


def body_app(request: httpx.Request) -> httpx.Response:
    if request.method == "HEAD":
        return httpx.Response(200, headers={"content-length": "13"})
    if request.url.path == "/empty":
        return httpx.Response(200)
    return httpx.Response(200, content=b"Hello, world!")


class FailingStream(httpx.SyncByteStream):
    def __iter__(self):
        yield b"partial"
        raise httpx.ReadError("boom")

    def close(self) -> None:
        pass


class AsyncFailingStream(httpx.AsyncByteStream):
    async def __aiter__(self):
        yield b"partial"
        raise httpx.ReadError("boom")

    async def aclose(self) -> None:
        pass


def failing_app(request: httpx.Request) -> httpx.Response:
    return httpx.Response(200, stream=FailingStream())


def async_failing_app(request: httpx.Request) -> httpx.Response:
    return httpx.Response(200, stream=AsyncFailingStream())


def test_event_hooks():
    events = []

    def on_request(request):
        events.append({"event": "request", "headers": dict(request.headers)})

    def on_response(response):
        events.append({"event": "response", "headers": dict(response.headers)})

    event_hooks = {"request": [on_request], "response": [on_response]}

    with httpx.Client(
        event_hooks=event_hooks, transport=httpx.MockTransport(app)
    ) as http:
        http.get("http://127.0.0.1:8000/", auth=("username", "password"))

    assert events == [
        {
            "event": "request",
            "headers": {
                "host": "127.0.0.1:8000",
                "user-agent": f"python-httpx/{httpx.__version__}",
                "accept": "*/*",
                "accept-encoding": "gzip, deflate, br, zstd",
                "connection": "keep-alive",
                "authorization": "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
            },
        },
        {
            "event": "response",
            "headers": {"server": "testserver"},
        },
    ]


def test_response_complete_event_hook():
    events = []

    def on_response_complete(response, reason, num_bytes, exception):
        events.append((reason, num_bytes, exception is None))

    with httpx.Client(
        event_hooks={"response_complete": [on_response_complete]},
        transport=httpx.MockTransport(body_app),
    ) as http:
        http.get("http://127.0.0.1:8000/")
        http.get("http://127.0.0.1:8000/empty")

    assert events == [("complete", 13, True), ("complete", 0, True)]


def test_response_complete_event_hook_for_head_request():
    events = []

    def on_response_complete(response, reason, num_bytes, exception):
        events.append((reason, num_bytes))

    with httpx.Client(
        event_hooks={"response_complete": [on_response_complete]},
        transport=httpx.MockTransport(body_app),
    ) as http:
        http.head("http://127.0.0.1:8000/")

    assert events == [("complete", 0)]


def test_response_complete_event_hook_on_failure():
    events = []

    def on_response_complete(response, reason, num_bytes, exception):
        events.append((reason, num_bytes, type(exception).__name__))

    with httpx.Client(
        event_hooks={"response_complete": [on_response_complete]},
        transport=httpx.MockTransport(failing_app),
    ) as http:
        with pytest.raises(httpx.ReadError):
            http.get("http://127.0.0.1:8000/")

    assert events == [("failed", 7, "ReadError")]


def test_response_complete_event_hook_with_redirect():
    events = []

    def on_response_complete(response, reason, num_bytes, exception):
        events.append((response.status_code, reason, num_bytes))

    with httpx.Client(
        event_hooks={"response_complete": [on_response_complete]},
        transport=httpx.MockTransport(app),
        follow_redirects=True,
    ) as http:
        http.get("http://127.0.0.1:8000/redirect")

    # The intermediate redirect response and the final response each settle once.
    assert events == [(303, "complete", 0), (200, "complete", 0)]


@pytest.mark.anyio
async def test_async_response_complete_event_hook():
    events = []

    async def on_response_complete(response, reason, num_bytes, exception):
        events.append((reason, num_bytes, exception is None))

    async with httpx.AsyncClient(
        event_hooks={"response_complete": [on_response_complete]},
        transport=httpx.MockTransport(body_app),
    ) as http:
        await http.get("http://127.0.0.1:8000/")
        await http.get("http://127.0.0.1:8000/empty")

    assert events == [("complete", 13, True), ("complete", 0, True)]


@pytest.mark.anyio
async def test_async_response_complete_event_hook_on_failure():
    events = []

    async def on_response_complete(response, reason, num_bytes, exception):
        events.append((reason, num_bytes, type(exception).__name__))

    async with httpx.AsyncClient(
        event_hooks={"response_complete": [on_response_complete]},
        transport=httpx.MockTransport(async_failing_app),
    ) as http:
        with pytest.raises(httpx.ReadError):
            await http.get("http://127.0.0.1:8000/")

    assert events == [("failed", 7, "ReadError")]


def test_event_hooks_raising_exception(server):
    def raise_on_4xx_5xx(response):
        response.raise_for_status()

    event_hooks = {"response": [raise_on_4xx_5xx]}

    with httpx.Client(
        event_hooks=event_hooks, transport=httpx.MockTransport(app)
    ) as http:
        try:
            http.get("http://127.0.0.1:8000/status/400")
        except httpx.HTTPStatusError as exc:
            assert exc.response.is_closed


@pytest.mark.anyio
async def test_async_event_hooks():
    events = []

    async def on_request(request):
        events.append({"event": "request", "headers": dict(request.headers)})

    async def on_response(response):
        events.append({"event": "response", "headers": dict(response.headers)})

    event_hooks = {"request": [on_request], "response": [on_response]}

    async with httpx.AsyncClient(
        event_hooks=event_hooks, transport=httpx.MockTransport(app)
    ) as http:
        await http.get("http://127.0.0.1:8000/", auth=("username", "password"))

    assert events == [
        {
            "event": "request",
            "headers": {
                "host": "127.0.0.1:8000",
                "user-agent": f"python-httpx/{httpx.__version__}",
                "accept": "*/*",
                "accept-encoding": "gzip, deflate, br, zstd",
                "connection": "keep-alive",
                "authorization": "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
            },
        },
        {
            "event": "response",
            "headers": {"server": "testserver"},
        },
    ]


@pytest.mark.anyio
async def test_async_event_hooks_raising_exception():
    async def raise_on_4xx_5xx(response):
        response.raise_for_status()

    event_hooks = {"response": [raise_on_4xx_5xx]}

    async with httpx.AsyncClient(
        event_hooks=event_hooks, transport=httpx.MockTransport(app)
    ) as http:
        try:
            await http.get("http://127.0.0.1:8000/status/400")
        except httpx.HTTPStatusError as exc:
            assert exc.response.is_closed


def test_event_hooks_with_redirect():
    """
    A redirect request should trigger additional 'request' and 'response' event hooks.
    """

    events = []

    def on_request(request):
        events.append({"event": "request", "headers": dict(request.headers)})

    def on_response(response):
        events.append({"event": "response", "headers": dict(response.headers)})

    event_hooks = {"request": [on_request], "response": [on_response]}

    with httpx.Client(
        event_hooks=event_hooks,
        transport=httpx.MockTransport(app),
        follow_redirects=True,
    ) as http:
        http.get("http://127.0.0.1:8000/redirect", auth=("username", "password"))

    assert events == [
        {
            "event": "request",
            "headers": {
                "host": "127.0.0.1:8000",
                "user-agent": f"python-httpx/{httpx.__version__}",
                "accept": "*/*",
                "accept-encoding": "gzip, deflate, br, zstd",
                "connection": "keep-alive",
                "authorization": "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
            },
        },
        {
            "event": "response",
            "headers": {"location": "/", "server": "testserver"},
        },
        {
            "event": "request",
            "headers": {
                "host": "127.0.0.1:8000",
                "user-agent": f"python-httpx/{httpx.__version__}",
                "accept": "*/*",
                "accept-encoding": "gzip, deflate, br, zstd",
                "connection": "keep-alive",
                "authorization": "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
            },
        },
        {
            "event": "response",
            "headers": {"server": "testserver"},
        },
    ]


@pytest.mark.anyio
async def test_async_event_hooks_with_redirect():
    """
    A redirect request should trigger additional 'request' and 'response' event hooks.
    """

    events = []

    async def on_request(request):
        events.append({"event": "request", "headers": dict(request.headers)})

    async def on_response(response):
        events.append({"event": "response", "headers": dict(response.headers)})

    event_hooks = {"request": [on_request], "response": [on_response]}

    async with httpx.AsyncClient(
        event_hooks=event_hooks,
        transport=httpx.MockTransport(app),
        follow_redirects=True,
    ) as http:
        await http.get("http://127.0.0.1:8000/redirect", auth=("username", "password"))

    assert events == [
        {
            "event": "request",
            "headers": {
                "host": "127.0.0.1:8000",
                "user-agent": f"python-httpx/{httpx.__version__}",
                "accept": "*/*",
                "accept-encoding": "gzip, deflate, br, zstd",
                "connection": "keep-alive",
                "authorization": "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
            },
        },
        {
            "event": "response",
            "headers": {"location": "/", "server": "testserver"},
        },
        {
            "event": "request",
            "headers": {
                "host": "127.0.0.1:8000",
                "user-agent": f"python-httpx/{httpx.__version__}",
                "accept": "*/*",
                "accept-encoding": "gzip, deflate, br, zstd",
                "connection": "keep-alive",
                "authorization": "Basic dXNlcm5hbWU6cGFzc3dvcmQ=",
            },
        },
        {
            "event": "response",
            "headers": {"server": "testserver"},
        },
    ]
