from __future__ import annotations

import contextvars
from collections.abc import AsyncGenerator
from pathlib import Path
from typing import Any

import anyio
import pytest

from starlette.applications import Starlette
from starlette.middleware.base import BaseHTTPMiddleware, RequestResponseEndpoint, register_context_vars
from starlette.requests import Request
from starlette.responses import FileResponse, PlainTextResponse, Response, StreamingResponse
from starlette.routing import Route
from starlette.testclient import TestClient
from starlette.types import ASGIApp, Message, Receive, Scope, Send
from tests.types import TestClientFactory

audit_id: contextvars.ContextVar[str] = contextvars.ContextVar("audit_id")
noise: contextvars.ContextVar[str] = contextvars.ContextVar("noise")


async def _noop_app(scope: Scope, receive: Receive, send: Send) -> None:  # pragma: no cover
    pass


async def _call(app: ASGIApp, path: str = "/") -> list[Message]:
    async def receive() -> Message:
        return {"type": "http.request", "body": b"", "more_body": False}

    sent: list[Message] = []

    async def send(message: Message) -> None:
        sent.append(message)

    await app({"type": "http", "method": "GET", "path": path, "headers": []}, receive, send)
    return sent


# --- value visible in the audit record --------------------------------------------


async def endpoint_writing_audit(request: Request) -> PlainTextResponse:
    audit_id.set("endpoint")
    noise.set("endpoint-noise")
    return PlainTextResponse("ok")


class AuditMiddleware(BaseHTTPMiddleware):
    def __init__(self, app: ASGIApp) -> None:
        super().__init__(app)
        self.records: list[tuple[str, Any]] = []

    async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
        register_context_vars(audit_id)
        response = await call_next(request)
        self.records.append((audit_id.get(), noise.get("unset")))
        return response


def test_registered_variable_is_visible_after_call_next(test_client_factory: TestClientFactory) -> None:
    middleware = AuditMiddleware(Starlette(routes=[Route("/", endpoint_writing_audit)]))
    client = test_client_factory(middleware)
    response = client.get("/")
    assert response.text == "ok"
    assert middleware.records == [("endpoint", "unset")]


def test_unregistered_variable_stays_isolated(test_client_factory: TestClientFactory) -> None:
    seen: list[tuple[str, Any]] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            seen.append((audit_id.get(), noise.get("unset")))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint_writing_audit)])))
    client.get("/")
    assert seen == [("endpoint", "unset")]


def test_variable_never_set_by_endpoint_is_left_alone(test_client_factory: TestClientFactory) -> None:
    other: contextvars.ContextVar[str] = contextvars.ContextVar("other")

    async def endpoint(request: Request) -> PlainTextResponse:
        return PlainTextResponse("ok")

    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(other)
            response = await call_next(request)
            seen.append(other.get("still-unset"))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    assert client.get("/").status_code == 200
    assert seen == ["still-unset"]


def test_only_the_registered_variables_that_were_set_are_returned(
    test_client_factory: TestClientFactory,
) -> None:
    # One layer registers two variables, but the endpoint only sets one of them.
    other: contextvars.ContextVar[str] = contextvars.ContextVar("other")

    async def endpoint(request: Request) -> PlainTextResponse:
        audit_id.set("endpoint")
        return PlainTextResponse("ok")

    seen: list[tuple[str, str]] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id, other)
            response = await call_next(request)
            seen.append((audit_id.get("unset"), other.get("unset")))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    assert client.get("/").status_code == 200
    assert seen == [("endpoint", "unset")]


def test_constructor_registers_variables(test_client_factory: TestClientFactory) -> None:
    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    client = test_client_factory(
        Recorder(Starlette(routes=[Route("/", endpoint_writing_audit)]), context_vars=[audit_id])
    )
    assert client.get("/").status_code == 200
    assert seen == ["endpoint"]


# --- exception behaviour -----------------------------------------------------------


def test_exception_propagates_and_nothing_is_applied() -> None:
    async def test() -> None:
        records: list[tuple[str, Any]] = []

        async def raising_app(scope: Scope, receive: Receive, send: Send) -> None:
            audit_id.set("set-before-raise")
            raise ValueError("boom")

        class Recorder(BaseHTTPMiddleware):
            async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
                register_context_vars(audit_id)
                try:
                    return await call_next(request)
                except ValueError as exc:
                    records.append((str(exc), audit_id.get("unset")))
                    raise

        app = Recorder(raising_app)
        with pytest.raises(ValueError, match="boom"):
            await _call(app)

        assert records == [("boom", "unset")]
        assert audit_id.get("unset-after") == "unset-after"

    anyio.run(test)


# --- cleanup ----------------------------------------------------------------------


def test_context_is_restored_after_request(test_client_factory: TestClientFactory) -> None:
    middleware = AuditMiddleware(Starlette(routes=[Route("/", endpoint_writing_audit)]))
    client = test_client_factory(middleware)

    assert audit_id.get("before") == "before"
    client.get("/")
    assert audit_id.get("before") == "before"


def test_context_is_restored_after_repeated_requests(test_client_factory: TestClientFactory) -> None:
    middleware = AuditMiddleware(Starlette(routes=[Route("/", endpoint_writing_audit)]))
    client = test_client_factory(middleware)

    for _ in range(3):
        client.get("/")
        assert audit_id.get("between") == "between"
    assert middleware.records == [("endpoint", "unset")] * 3


# --- registration rules -----------------------------------------------------------


def test_register_outside_dispatch_raises() -> None:
    with pytest.raises(RuntimeError, match="dispatch"):
        register_context_vars(audit_id)


def test_register_after_response_started_raises() -> None:
    async def test() -> None:
        async def endpoint(request: Request) -> PlainTextResponse:
            return PlainTextResponse("ok")

        errors: list[str] = []

        class Recorder(BaseHTTPMiddleware):
            async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
                register_context_vars(audit_id)
                response = await call_next(request)
                try:
                    register_context_vars(noise)
                except RuntimeError as exc:
                    errors.append(str(exc))
                return response

        client = TestClient(Recorder(Starlette(routes=[Route("/", endpoint)])))
        client.get("/")
        assert len(errors) == 1

    anyio.run(test)


def test_register_non_context_var_raises(test_client_factory: TestClientFactory) -> None:
    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars("not-a-context-var")  # type: ignore[arg-type]
            return await call_next(request)

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint_writing_audit)])))
    with pytest.raises(TypeError, match="ContextVar"):
        client.get("/")


def test_value_is_captured_only_once_per_response_start() -> None:
    # A downstream app that sends `http.response.start` twice is malformed, but it must
    # not cause the same value to be applied twice in the middleware's task.
    async def malformed_app(scope: Scope, receive: Receive, send: Send) -> None:
        audit_id.set("endpoint")
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    app = Recorder(malformed_app)

    async def receive() -> Message:
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message: Message) -> None:
        pass

    with pytest.raises(AssertionError, match="Unexpected message"):
        anyio.run(app, {"type": "http", "method": "GET", "path": "/"}, receive, send)

    # The value is captured for the first response.start and not applied a second time.
    assert seen == ["endpoint"]
    assert audit_id.get("after") == "after"


def test_constructor_rejects_non_context_var() -> None:
    with pytest.raises(TypeError, match="ContextVar"):
        BaseHTTPMiddleware(_noop_app, context_vars=["nope"])  # type: ignore[list-item]


# --- visibility point: before the caller emits http.response.start ---------------


def test_value_is_visible_before_response_start(test_client_factory: TestClientFactory) -> None:
    events: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            events.append(f"audit:{audit_id.get('unset')}")
            return response

    class RecordingSend:
        def __init__(self, app: ASGIApp) -> None:
            self.app = app

        async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
            async def recording_send(message: Message) -> None:
                if message["type"] == "http.response.start":
                    events.append("response.start")
                await send(message)

            await self.app(scope, receive, recording_send)

    app = RecordingSend(Recorder(Starlette(routes=[Route("/", endpoint_writing_audit)])))
    client = test_client_factory(app)
    assert client.get("/").text == "ok"
    assert events == ["audit:endpoint", "response.start"]


def test_value_set_while_streaming_body_is_not_returned(test_client_factory: TestClientFactory) -> None:
    seen: list[str] = []

    async def endpoint(request: Request) -> StreamingResponse:
        audit_id.set("set-before-start")

        async def body() -> AsyncGenerator[bytes, None]:
            audit_id.set("set-while-streaming")
            yield b"ok"

        return StreamingResponse(body())

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    response = client.get("/")
    assert response.text == "ok"
    assert seen == ["set-before-start"]
    # The value set during streaming never leaks out of the request.
    assert audit_id.get("after") == "after"


def test_value_set_by_middleware_itself_is_kept(test_client_factory: TestClientFactory) -> None:
    async def endpoint(request: Request) -> PlainTextResponse:
        return PlainTextResponse("ok")

    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            audit_id.set("set-by-middleware")
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    assert client.get("/").text == "ok"
    # The endpoint did not set the variable, so the middleware's own value stands.
    assert seen == ["set-by-middleware"]


def test_none_value_is_returned(test_client_factory: TestClientFactory) -> None:
    optional: contextvars.ContextVar[str | None] = contextvars.ContextVar("optional", default="default")
    seen: list[Any] = []

    async def endpoint(request: Request) -> PlainTextResponse:
        optional.set(None)
        return PlainTextResponse("ok")

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(optional)
            response = await call_next(request)
            seen.append(optional.get())
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    assert client.get("/").text == "ok"
    assert seen == [None]


def test_sync_endpoint_changes_are_not_returned(test_client_factory: TestClientFactory) -> None:
    # Synchronous endpoints run in a threadpool worker: their context changes are not
    # observable even in the task that calls them, so there is nothing to return.
    def endpoint(request: Request) -> PlainTextResponse:
        audit_id.set("set-in-threadpool")
        return PlainTextResponse("ok")

    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    assert client.get("/").text == "ok"
    assert seen == ["unset"]


def test_nested_middlewares_return_their_own_values(test_client_factory: TestClientFactory) -> None:
    inner_records: list[str] = []
    outer_records: list[str] = []

    class Inner(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            inner_records.append(audit_id.get("unset"))
            return response

    class Outer(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(noise)
            response = await call_next(request)
            outer_records.append(noise.get("unset"))
            return response

    app = Outer(Inner(Starlette(routes=[Route("/", endpoint_writing_audit)])))
    client = test_client_factory(app)
    assert client.get("/").text == "ok"
    assert inner_records == ["endpoint"]
    assert outer_records == ["endpoint-noise"]


def test_outer_registration_does_not_reach_untouched_inner_layer(
    test_client_factory: TestClientFactory,
) -> None:
    # Only the outer layer registers. The layer in between did not ask for the variable,
    # so it must not see the endpoint's value in its own task.
    records: dict[str, str] = {}

    class Outer(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            records["outer"] = audit_id.get("unset")
            return response

    class Middle(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            response = await call_next(request)
            records["middle"] = audit_id.get("unset")
            return response

    app = Outer(Middle(Starlette(routes=[Route("/", endpoint_writing_audit)])))
    client = test_client_factory(app)
    assert client.get("/").text == "ok"
    assert records == {"middle": "unset", "outer": "endpoint"}


def test_each_layer_returns_only_the_variables_it_registered(
    test_client_factory: TestClientFactory,
) -> None:
    records: dict[str, tuple[str, str]] = {}

    class Outer(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            records["outer"] = (audit_id.get("unset"), noise.get("unset"))
            return response

    class Inner(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(noise)
            response = await call_next(request)
            records["inner"] = (audit_id.get("unset"), noise.get("unset"))
            return response

    app = Outer(Inner(Starlette(routes=[Route("/", endpoint_writing_audit)])))
    client = test_client_factory(app)
    assert client.get("/").text == "ok"
    assert records == {"inner": ("unset", "endpoint-noise"), "outer": ("endpoint", "unset")}


# --- interleaved requests ----------------------------------------------------------


@pytest.mark.anyio
async def test_interleaved_requests_only_see_their_own_value() -> None:
    records: dict[str, tuple[str, str]] = {}
    entered = anyio.Event()
    release_first = anyio.Event()

    async def endpoint(request: Request) -> PlainTextResponse:
        name = request.path_params["name"]
        audit_id.set(name)
        noise.set(f"noise-{name}")
        if name == "first":
            entered.set()
            await release_first.wait()
        return PlainTextResponse(name)

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            records[request.path_params["name"]] = (audit_id.get(), noise.get("unset"))
            return response

    app = Recorder(Starlette(routes=[Route("/{name}", endpoint)]))

    async def request(name: str) -> bytes:
        scope: Scope = {"type": "http", "method": "GET", "path": f"/{name}", "headers": []}
        body = b""

        async def receive() -> Message:
            return {"type": "http.request", "body": b"", "more_body": False}

        async def send(message: Message) -> None:
            nonlocal body
            if message["type"] == "http.response.body":
                body += message.get("body", b"")

        await app(scope, receive, send)
        return body

    async with anyio.create_task_group() as task_group:
        task_group.start_soon(request, "first")
        await entered.wait()
        # The second request runs to completion while the first waits in its endpoint.
        assert await request("second") == b"second"
        release_first.set()

    # Each audit record only contains what its own request returned.
    assert records["second"] == ("second", "unset")
    assert records["first"] == ("first", "unset")
    assert audit_id.get("after") == "after"
    assert noise.get("after") == "after"


@pytest.mark.anyio
async def test_interleaved_requests_through_stacked_middlewares() -> None:
    # Two requests interleave inside the endpoint, with two `BaseHTTPMiddleware` layers on
    # the stack. Every layer's record must contain only its own request's value.
    records: dict[tuple[str, str], str] = {}
    entered = anyio.Event()
    release_first = anyio.Event()

    async def endpoint(request: Request) -> PlainTextResponse:
        name = request.path_params["name"]
        audit_id.set(name)
        if name == "first":
            entered.set()
            await release_first.wait()
        return PlainTextResponse(name)

    def make_layer(label: str) -> type[BaseHTTPMiddleware]:
        class Layer(BaseHTTPMiddleware):
            async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
                register_context_vars(audit_id)
                response = await call_next(request)
                records[(label, request.path_params["name"])] = audit_id.get("unset")
                return response

        return Layer

    app = make_layer("outer")(make_layer("inner")(Starlette(routes=[Route("/{name}", endpoint)])))

    async def request(name: str) -> None:
        scope: Scope = {"type": "http", "method": "GET", "path": f"/{name}", "headers": []}

        async def receive() -> Message:
            return {"type": "http.request", "body": b"", "more_body": False}

        async def send(message: Message) -> None:
            pass

        await app(scope, receive, send)

    async with anyio.create_task_group() as task_group:
        task_group.start_soon(request, "first")
        await entered.wait()
        await request("second")
        release_first.set()

    assert records == {
        ("inner", "second"): "second",
        ("outer", "second"): "second",
        ("inner", "first"): "first",
        ("outer", "first"): "first",
    }


def test_pathsend_response_still_returns_values(tmp_path: Path) -> None:
    path = tmp_path / "example.txt"
    path.write_text("pathsend")

    async def endpoint(request: Request) -> FileResponse:
        audit_id.set("endpoint")
        return FileResponse(path)

    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    app = Recorder(Starlette(routes=[Route("/", endpoint)]))
    messages: list[Message] = []

    async def receive() -> Message:  # pragma: no cover
        raise NotImplementedError

    async def send(message: Message) -> None:
        messages.append(message)

    scope: Scope = {
        "type": "http",
        "method": "GET",
        "path": "/",
        "headers": [],
        "extensions": {"http.response.pathsend": {}},
    }
    anyio.run(app, scope, receive, send)

    assert seen == ["endpoint"]
    assert [message["type"] for message in messages] == ["http.response.start", "http.response.pathsend"]


def test_early_hint_messages_are_not_affected(test_client_factory: TestClientFactory) -> None:
    async def endpoint(request: Request) -> PlainTextResponse:
        audit_id.set("endpoint")
        await request.send_early_hints("</endpoint.css>; rel=preload; as=style")
        return PlainTextResponse("ok")

    seen: list[str] = []

    class Recorder(BaseHTTPMiddleware):
        async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
            register_context_vars(audit_id)
            await request.send_early_hints("</middleware.css>; rel=preload; as=style")
            response = await call_next(request)
            seen.append(audit_id.get("unset"))
            return response

    client = test_client_factory(Recorder(Starlette(routes=[Route("/", endpoint)])))
    assert client.get("/").text == "ok"
    assert seen == ["endpoint"]
