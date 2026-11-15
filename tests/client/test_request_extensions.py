import typing

import pytest

import httpx


class TraceContext:
    """
    A mutable extension value that declares how derived copies are made.
    """

    def __init__(self, hops: typing.List[str]) -> None:
        self.hops = hops

    def __httpx_extension_copy__(self) -> "TraceContext":
        return TraceContext(hops=list(self.hops))


def record_hop(request: httpx.Request) -> httpx.Response:
    context = request.extensions["trace_context"]
    context.hops.append(request.url.path)
    if request.url.path == "/hop1":
        return httpx.Response(303, headers={"location": "/hop2"})
    elif request.url.path == "/hop2":
        return httpx.Response(303, headers={"location": "/done"})
    return httpx.Response(200)


def test_redirect_extensions_derived_copies_do_not_leak_between_hops():
    client = httpx.Client(transport=httpx.MockTransport(record_hop))
    request = client.build_request(
        "GET",
        "http://example.org/hop1",
        extensions={"trace_context": TraceContext(hops=[])},
    )
    response = client.send(request, follow_redirects=True)

    assert response.status_code == 200
    assert len(response.history) == 2

    first_hop = response.history[0].request.extensions["trace_context"]
    second_hop = response.history[1].request.extensions["trace_context"]
    final_hop = response.request.extensions["trace_context"]

    assert first_hop is request.extensions["trace_context"]
    assert first_hop.hops == ["/hop1"]
    assert second_hop.hops == ["/hop1", "/hop2"]
    assert final_hop.hops == ["/hop1", "/hop2", "/done"]

    assert first_hop is not second_hop
    assert second_hop is not final_hop


@pytest.mark.anyio
async def test_async_redirect_extensions_derived_copies_do_not_leak_between_hops():
    async with httpx.AsyncClient(transport=httpx.MockTransport(record_hop)) as client:
        request = client.build_request(
            "GET",
            "http://example.org/hop1",
            extensions={"trace_context": TraceContext(hops=[])},
        )
        response = await client.send(request, follow_redirects=True)

    assert response.status_code == 200
    first_hop = response.history[0].request.extensions["trace_context"]
    second_hop = response.history[1].request.extensions["trace_context"]
    final_hop = response.request.extensions["trace_context"]
    assert first_hop.hops == ["/hop1"]
    assert second_hop.hops == ["/hop1", "/hop2"]
    assert final_hop.hops == ["/hop1", "/hop2", "/done"]


def test_redirect_extensions_immutable_values_are_inherited():
    def app(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/redirect":
            return httpx.Response(303, headers={"location": "/done"})
        return httpx.Response(200)

    client = httpx.Client(transport=httpx.MockTransport(app))
    request_id = ("trace", 123)
    request = client.build_request(
        "GET",
        "http://example.org/redirect",
        extensions={"request_id": request_id, "sampling": 0.5},
    )
    response = client.send(request, follow_redirects=True)

    assert response.status_code == 200
    assert response.request.extensions["request_id"] is request_id
    assert response.request.extensions["sampling"] == 0.5


def test_redirect_extensions_reserved_keys_are_inherited_as_is():
    def app(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/redirect":
            return httpx.Response(303, headers={"location": "/done"})
        return httpx.Response(200)

    client = httpx.Client(transport=httpx.MockTransport(app))
    timeout = {"connect": 5.0, "read": 5.0, "write": 5.0, "pool": 5.0}
    request = client.build_request(
        "GET", "http://example.org/redirect", extensions={"timeout": timeout}
    )
    response = client.send(request, follow_redirects=True)

    assert response.status_code == 200
    assert response.request.extensions["timeout"] is timeout


def test_copy_request_extensions_with_callbacks():
    extensions = {"routing": {"table": ["a"]}}
    copied = httpx.copy_request_extensions(
        extensions,
        copy_callbacks={"routing": lambda value: {"table": list(value["table"])}},
    )
    assert copied["routing"] == {"table": ["a"]}
    assert copied["routing"] is not extensions["routing"]


def test_copy_request_extensions_callbacks_cannot_override_reserved_keys():
    with pytest.raises(ValueError):
        httpx.copy_request_extensions(
            {}, copy_callbacks={"timeout": lambda value: value}
        )


def test_copy_request_extensions_requires_a_copy_protocol_for_mutable_values():
    with pytest.raises(httpx.ExtensionCopyError):
        httpx.copy_request_extensions({"routing": {"table": []}})


def test_copy_request_extensions_wraps_callback_failures():
    def broken(value: typing.Any) -> typing.Any:
        raise RuntimeError("cannot copy")

    with pytest.raises(httpx.ExtensionCopyError):
        httpx.copy_request_extensions(
            {"routing": [1, 2, 3]}, copy_callbacks={"routing": broken}
        )


class TrackingStream(httpx.SyncByteStream):
    def __init__(self) -> None:
        self.closed = False

    def __iter__(self) -> typing.Iterator[bytes]:
        yield b""

    def close(self) -> None:
        self.closed = True


class RedirectTransport(httpx.BaseTransport):
    def __init__(self) -> None:
        self.streams: typing.List[TrackingStream] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        stream = TrackingStream()
        self.streams.append(stream)
        return httpx.Response(303, headers={"location": "/next"}, stream=stream)


def test_extension_copy_failure_occurs_before_next_send_and_releases_response():
    transport = RedirectTransport()
    client = httpx.Client(transport=transport)

    with pytest.raises(httpx.ExtensionCopyError) as exc_info:
        client.get(
            "http://example.org/",
            extensions={"routing": {"table": []}},
            follow_redirects=True,
        )

    assert exc_info.value.request.url == "http://example.org/"
    assert len(transport.streams) == 1
    assert transport.streams[0].closed


class RebuildingAuth(httpx.Auth):
    """
    An auth flow that responds to a 401 by building a new request,
    reusing the extensions of the original request.
    """

    def auth_flow(
        self, request: httpx.Request
    ) -> typing.Generator[httpx.Request, httpx.Response, None]:
        response = yield request
        if response.status_code == 401:
            retry = httpx.Request(
                request.method,
                request.url,
                headers=request.headers,
                extensions=request.extensions,
            )
            retry.headers["Authorization"] = "Bearer token"
            yield retry


def test_auth_rebuilt_request_extensions_are_copied():
    def app(request: httpx.Request) -> httpx.Response:
        context = request.extensions["trace_context"]
        context.hops.append("attempt")
        if "Authorization" not in request.headers:
            return httpx.Response(401)
        return httpx.Response(200)

    client = httpx.Client(transport=httpx.MockTransport(app), auth=RebuildingAuth())
    request = client.build_request(
        "GET",
        "http://example.org/",
        extensions={"trace_context": TraceContext(hops=[])},
    )
    response = client.send(request)

    assert response.status_code == 200
    assert len(response.history) == 1

    first_attempt = response.history[0].request.extensions["trace_context"]
    retry_attempt = response.request.extensions["trace_context"]

    assert first_attempt is request.extensions["trace_context"]
    assert first_attempt.hops == ["attempt"]
    assert retry_attempt.hops == ["attempt", "attempt"]
    assert retry_attempt is not first_attempt
