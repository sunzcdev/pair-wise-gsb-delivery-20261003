from collections.abc import Iterator
from typing import Any

import pytest

from starlette import status
from starlette.endpoints import HTTPEndpoint, WebSocketDecodeError, WebSocketEndpoint
from starlette.requests import Request
from starlette.responses import PlainTextResponse
from starlette.routing import Route, Router
from starlette.testclient import TestClient
from starlette.types import Message, Scope
from starlette.websockets import WebSocket, WebSocketDisconnect
from tests.types import TestClientFactory


class Homepage(HTTPEndpoint):
    async def get(self, request: Request) -> PlainTextResponse:
        username = request.path_params.get("username")
        if username is None:
            return PlainTextResponse("Hello, world!")
        return PlainTextResponse(f"Hello, {username}!")

    async def query(self, request: Request) -> PlainTextResponse:
        return PlainTextResponse("Hello, QUERY!")


app = Router(routes=[Route("/", endpoint=Homepage), Route("/{username}", endpoint=Homepage)])


@pytest.fixture
def client(test_client_factory: TestClientFactory) -> Iterator[TestClient]:
    with test_client_factory(app) as client:
        yield client


def test_http_endpoint_route(client: TestClient) -> None:
    response = client.get("/")
    assert response.status_code == 200
    assert response.text == "Hello, world!"


def test_http_endpoint_route_path_params(client: TestClient) -> None:
    response = client.get("/tomchristie")
    assert response.status_code == 200
    assert response.text == "Hello, tomchristie!"


def test_http_endpoint_route_method(client: TestClient) -> None:
    response = client.post("/")
    assert response.status_code == 405
    assert response.text == "Method Not Allowed"
    assert response.headers["allow"] == "GET, HEAD, QUERY"
    assert client.head("/").status_code == 200


def test_http_endpoint_route_query_method(client: TestClient) -> None:
    response = client.request("QUERY", "/")
    assert response.status_code == 200
    assert response.text == "Hello, QUERY!"


@pytest.mark.anyio
async def test_http_endpoint_supports_early_hints() -> None:
    class Endpoint(HTTPEndpoint):
        async def get(self, request: Request) -> PlainTextResponse:
            await request.send_early_hints("</style.css>; rel=preload; as=style")
            return PlainTextResponse("Hello, world!")

    scope: Scope = {
        "type": "http",
        "method": "GET",
        "extensions": {"http.response.early_hint": {}},
    }
    messages: list[Message] = []

    async def receive() -> Message:
        raise NotImplementedError

    async def send(message: Message) -> None:
        messages.append(message)

    await Endpoint(scope, receive, send)

    assert messages[0] == {
        "type": "http.response.early_hint",
        "links": [b"</style.css>; rel=preload; as=style"],
    }
    assert messages[1]["type"] == "http.response.start"
    assert messages[2]["type"] == "http.response.body"
    assert messages[2]["body"] == b"Hello, world!"


def test_http_endpoint_does_not_dispatch_non_verb_method(test_client_factory: TestClientFactory) -> None:
    class Endpoint(HTTPEndpoint):
        async def get(self, request: Request) -> PlainTextResponse:
            return PlainTextResponse("Hello, world!")  # pragma: no cover

        async def _do_delete(self, request: Request) -> PlainTextResponse:
            return PlainTextResponse("Privileged helper")  # pragma: no cover

    app = Router(routes=[Route("/", endpoint=Endpoint)])
    client = test_client_factory(app)

    response = client.request("_DO_DELETE", "/")
    assert response.status_code == 405
    assert response.text == "Method Not Allowed"
    assert response.headers["allow"] == "GET, HEAD"


def test_http_endpoint_allow_header_omits_head_without_get(test_client_factory: TestClientFactory) -> None:
    class Endpoint(HTTPEndpoint):
        async def post(self, request: Request) -> PlainTextResponse:
            return PlainTextResponse("Hello, world!")  # pragma: no cover

    client = test_client_factory(Router(routes=[Route("/", endpoint=Endpoint)]))

    response = client.head("/")
    assert response.status_code == 405
    assert response.headers["allow"] == "POST"


def test_websocket_endpoint_on_connect(test_client_factory: TestClientFactory) -> None:
    class WebSocketApp(WebSocketEndpoint):
        async def on_connect(self, websocket: WebSocket) -> None:
            assert websocket["subprotocols"] == ["soap", "wamp"]
            await websocket.accept(subprotocol="wamp")

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws", subprotocols=["soap", "wamp"]) as websocket:
        assert websocket.accepted_subprotocol == "wamp"


def test_websocket_endpoint_on_receive_bytes(
    test_client_factory: TestClientFactory,
) -> None:
    class WebSocketApp(WebSocketEndpoint):
        encoding = "bytes"

        async def on_receive(self, websocket: WebSocket, data: bytes) -> None:
            await websocket.send_bytes(b"Message bytes was: " + data)

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.send_bytes(b"Hello, world!")
        _bytes = websocket.receive_bytes()
        assert _bytes == b"Message bytes was: Hello, world!"

    with pytest.raises(RuntimeError):
        with client.websocket_connect("/ws") as websocket:
            websocket.send_text("Hello world")


def test_websocket_endpoint_on_receive_json(
    test_client_factory: TestClientFactory,
) -> None:
    class WebSocketApp(WebSocketEndpoint):
        encoding = "json"

        async def on_receive(self, websocket: WebSocket, data: str) -> None:
            await websocket.send_json({"message": data})

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.send_json({"hello": "world"})
        data = websocket.receive_json()
        assert data == {"message": {"hello": "world"}}

    with pytest.raises(RuntimeError):
        with client.websocket_connect("/ws") as websocket:
            websocket.send_text("Hello world")


def test_websocket_endpoint_on_receive_json_binary(
    test_client_factory: TestClientFactory,
) -> None:
    class WebSocketApp(WebSocketEndpoint):
        encoding = "json"

        async def on_receive(self, websocket: WebSocket, data: str) -> None:
            await websocket.send_json({"message": data}, mode="binary")

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.send_json({"hello": "world"}, mode="binary")
        data = websocket.receive_json(mode="binary")
        assert data == {"message": {"hello": "world"}}


def test_websocket_endpoint_on_receive_text(
    test_client_factory: TestClientFactory,
) -> None:
    class WebSocketApp(WebSocketEndpoint):
        encoding = "text"

        async def on_receive(self, websocket: WebSocket, data: str) -> None:
            await websocket.send_text(f"Message text was: {data}")

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.send_text("Hello, world!")
        _text = websocket.receive_text()
        assert _text == "Message text was: Hello, world!"

    with pytest.raises(RuntimeError):
        with client.websocket_connect("/ws") as websocket:
            websocket.send_bytes(b"Hello world")


def test_websocket_endpoint_on_default(test_client_factory: TestClientFactory) -> None:
    class WebSocketApp(WebSocketEndpoint):
        encoding = None

        async def on_receive(self, websocket: WebSocket, data: str) -> None:
            await websocket.send_text(f"Message text was: {data}")

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.send_text("Hello, world!")
        _text = websocket.receive_text()
        assert _text == "Message text was: Hello, world!"


def test_websocket_endpoint_on_default_with_empty_text(test_client_factory: TestClientFactory) -> None:
    class WebSocketApp(WebSocketEndpoint):
        encoding = None

        async def on_receive(self, websocket: WebSocket, data: str) -> None:
            await websocket.send_text(f"Message text was: {data!r}")

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.send_text("")
        _text = websocket.receive_text()
        assert _text == "Message text was: ''"


@pytest.mark.anyio
async def test_websocket_endpoint_decode_text_rejects_message_without_text() -> None:
    # Per the ASGI spec, "one or both keys may be present" on a receive event, so a
    # compliant server may send both "text" and "bytes", with "text" set to None.
    class Echo(WebSocketEndpoint):
        encoding = "text"

    sent: list[Message] = []

    async def receive() -> Message:
        raise AssertionError("receive should not be called")  # pragma: no cover

    async def send(message: Message) -> None:
        sent.append(message)

    ep = Echo({"type": "websocket", "path": "/", "headers": [], "query_string": b""}, receive=receive, send=send)
    websocket = WebSocket(ep.scope, receive=ep.receive, send=send)
    message: Message = {"type": "websocket.receive", "text": None, "bytes": b"hello"}

    with pytest.raises(RuntimeError, match="Expected text websocket messages, but got bytes"):
        await ep.decode(websocket, message)

    assert sent == [{"type": "websocket.close", "code": status.WS_1003_UNSUPPORTED_DATA, "reason": ""}]


@pytest.mark.anyio
async def test_websocket_endpoint_decode_bytes_rejects_message_without_bytes() -> None:
    class Echo(WebSocketEndpoint):
        encoding = "bytes"

    sent: list[Message] = []

    async def receive() -> Message:
        raise AssertionError("receive should not be called")  # pragma: no cover

    async def send(message: Message) -> None:
        sent.append(message)

    ep = Echo({"type": "websocket", "path": "/", "headers": [], "query_string": b""}, receive=receive, send=send)
    websocket = WebSocket(ep.scope, receive=ep.receive, send=send)
    message: Message = {"type": "websocket.receive", "text": "hi", "bytes": None}

    with pytest.raises(RuntimeError, match="Expected bytes websocket messages, but got text"):
        await ep.decode(websocket, message)

    assert sent == [{"type": "websocket.close", "code": status.WS_1003_UNSUPPORTED_DATA, "reason": ""}]


def test_websocket_endpoint_on_disconnect(
    test_client_factory: TestClientFactory,
) -> None:
    class WebSocketApp(WebSocketEndpoint):
        async def on_disconnect(self, websocket: WebSocket, close_code: int) -> None:
            assert close_code == 1001
            await websocket.close(code=close_code)

    client = test_client_factory(WebSocketApp)
    with client.websocket_connect("/ws") as websocket:
        websocket.close(code=1001)


def test_websocket_endpoint_subprotocol_encodings_per_connection(
    test_client_factory: TestClientFactory,
) -> None:
    # One endpoint serving both JSON-speaking old nodes ("json-v1") and
    # binary-frame new nodes ("bin-v2"), selected by negotiated subprotocol.
    disconnects: list[tuple[str | None, int]] = []

    class WebSocketApp(WebSocketEndpoint):
        subprotocol_encodings = {"json-v1": "json", "bin-v2": "bytes"}

        async def on_connect(self, websocket: WebSocket) -> None:
            for subprotocol in ("json-v1", "bin-v2"):
                if subprotocol in websocket["subprotocols"]:
                    await websocket.accept(subprotocol=subprotocol)
                    return
            await websocket.accept()

        async def on_receive(self, websocket: WebSocket, data: Any) -> None:
            if isinstance(data, bytes):
                await websocket.send_bytes(b"bin: " + data)
            else:
                await websocket.send_json({"echo": data})

        async def on_disconnect(self, websocket: WebSocket, close_code: int) -> None:
            disconnects.append((websocket.accepted_subprotocol, close_code))

    client = test_client_factory(WebSocketApp)

    with client.websocket_connect("/ws", subprotocols=["json-v1"]) as json_ws:
        assert json_ws.accepted_subprotocol == "json-v1"
        json_ws.send_json({"hello": "world"})
        assert json_ws.receive_json() == {"echo": {"hello": "world"}}

        # Open a second, concurrent connection using binary frames.
        with pytest.raises(WebSocketDecodeError, match="Expected bytes websocket messages, but got text"):
            with client.websocket_connect("/ws", subprotocols=["bin-v2"]) as bin_ws:
                assert bin_ws.accepted_subprotocol == "bin-v2"
                bin_ws.send_bytes(b"\x00\x01")
                assert bin_ws.receive_bytes() == b"bin: \x00\x01"
                # The subprotocol negotiated fine, but the frame type is wrong
                # for this connection's encoding: closed with 1003.
                bin_ws.send_text("not binary")
                with pytest.raises(WebSocketDisconnect) as exc_info:
                    bin_ws.receive_bytes()
                assert exc_info.value.code == status.WS_1003_UNSUPPORTED_DATA

        # The misbehaving connection was closed, but the JSON connection is
        # unaffected and keeps working.
        json_ws.send_json({"still": "alive"})
        assert json_ws.receive_json() == {"echo": {"still": "alive"}}

    # The class-level encoding was never mutated: each connection used its own.
    assert WebSocketApp.encoding is None
    assert disconnects == [
        ("bin-v2", status.WS_1003_UNSUPPORTED_DATA),
        ("json-v1", status.WS_1000_NORMAL_CLOSURE),
    ]


def test_websocket_endpoint_subprotocol_not_negotiated(
    test_client_factory: TestClientFactory,
) -> None:
    # Failing to negotiate a subprotocol is distinct from receiving a wrong
    # message type: the connection is closed with 1002, not 1003, and
    # `on_disconnect` observes that code.
    disconnects: list[int] = []

    class WebSocketApp(WebSocketEndpoint):
        subprotocol_encodings = {"json-v1": "json", "bin-v2": "bytes"}

        async def on_disconnect(self, websocket: WebSocket, close_code: int) -> None:
            disconnects.append(close_code)

    client = test_client_factory(WebSocketApp)

    # The client offers no subprotocols, so none can be negotiated.
    with pytest.raises(WebSocketDecodeError, match="no subprotocol was negotiated"):
        with client.websocket_connect("/ws") as websocket:
            assert websocket.accepted_subprotocol is None
            with pytest.raises(WebSocketDisconnect) as exc_info:
                websocket.receive_text()
            assert exc_info.value.code == status.WS_1002_PROTOCOL_ERROR

    assert disconnects == [status.WS_1002_PROTOCOL_ERROR]


def test_websocket_endpoint_unknown_subprotocol(
    test_client_factory: TestClientFactory,
) -> None:
    disconnects: list[int] = []

    class WebSocketApp(WebSocketEndpoint):
        subprotocol_encodings = {"json-v1": "json"}

        async def on_connect(self, websocket: WebSocket) -> None:
            await websocket.accept(subprotocol="unknown-proto")

        async def on_disconnect(self, websocket: WebSocket, close_code: int) -> None:
            disconnects.append(close_code)

    client = test_client_factory(WebSocketApp)

    with pytest.raises(WebSocketDecodeError, match="no encoding configured for subprotocol 'unknown-proto'"):
        with client.websocket_connect("/ws", subprotocols=["unknown-proto"]) as websocket:
            assert websocket.accepted_subprotocol == "unknown-proto"
            with pytest.raises(WebSocketDisconnect) as exc_info:
                websocket.receive_text()
            assert exc_info.value.code == status.WS_1002_PROTOCOL_ERROR

    assert disconnects == [status.WS_1002_PROTOCOL_ERROR]
