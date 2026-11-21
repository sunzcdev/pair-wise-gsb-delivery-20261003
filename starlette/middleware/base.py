from __future__ import annotations

from collections.abc import AsyncGenerator, AsyncIterable, Awaitable, Callable, Iterable, Mapping, MutableMapping
from contextlib import aclosing
from contextvars import ContextVar, Token, copy_context
from typing import Any, TypeVar

import anyio

from starlette._utils import create_collapsing_task_group
from starlette.requests import ClientDisconnect, Request
from starlette.responses import Response
from starlette.types import ASGIApp, Message, Receive, Scope, Send

RequestResponseEndpoint = Callable[[Request], Awaitable[Response]]
DispatchFunction = Callable[[Request, RequestResponseEndpoint], Awaitable[Response]]
BodyStreamGenerator = AsyncGenerator[bytes | MutableMapping[str, Any], None]
AsyncContentStream = AsyncIterable[str | bytes | memoryview | MutableMapping[str, Any]]
T = TypeVar("T")


# Internal envelope used to hand the captured values from the task running the downstream
# app to the task running the middleware. It travels on the private stream between the
# two tasks, so it never reaches the server, and it is a distinct type rather than an ASGI
# message, so it can never be confused with anything the app sends.
class _ContextReturnValues:
    __slots__ = ("values",)

    def __init__(self, values: dict[ContextVar[Any], Any]) -> None:
        self.values = values


# The layer whose `dispatch()` is currently running, if any, so `register_context_vars()`
# resolves the innermost `BaseHTTPMiddleware` of the request.
_CONTEXT_RETURN_LAYER: ContextVar[_ContextReturn | None] = ContextVar("starlette.context_return_layer", default=None)


class _ContextReturn:
    """Per-request state of the `ContextVar` return channel for one middleware layer.

    `BaseHTTPMiddleware` runs the downstream app in a separate task, which gets a copy of
    the caller's context, so changes made there are invisible to the caller. A middleware
    can opt individual variables back in with `register_context_vars()` or with the
    `context_vars` constructor argument.

    Each layer links to the enclosing one. The innermost layer captures the values in the
    task that runs the endpoint, and every layer passes them up as data together with its
    response. A layer only writes the variables it registered into its own task, so a
    variable registered by an outer layer never shows up in an intermediate layer that did
    not ask for it, and variables nobody registered stay isolated as usual.
    """

    __slots__ = ("_captured", "_forwarded", "_parent", "_registered", "_started", "_tokens")

    def __init__(self, parent: _ContextReturn | None) -> None:
        self._parent = parent
        # A dict used as an ordered set, so registration order is stable and duplicates
        # are ignored.
        self._registered: dict[ContextVar[Any], None] = {}
        self._started = False
        self._captured = False
        self._tokens: list[Token[Any]] = []
        # Values captured closer to the endpoint, passed up from the layer below.
        self._forwarded: dict[ContextVar[Any], Any] = {}

    def _chain(self) -> Iterable[_ContextReturn]:
        node: _ContextReturn | None = self
        while node is not None:
            yield node
            node = node._parent

    def _registered_vars(self) -> Iterable[ContextVar[Any]]:
        for node in self._chain():
            yield from node._registered

    def register(self, context_vars: Iterable[ContextVar[Any]]) -> None:
        """Add variables to return. Must happen before the response starts."""
        if self._started:
            raise RuntimeError(
                "Context variables must be registered before the response starts. "
                "Register them before calling call_next()."
            )
        for context_var in context_vars:
            self._registered[context_var] = None

    def capture(self) -> dict[ContextVar[Any], Any]:
        """Collect the values to pass to the enclosing layer.

        Runs in the task that starts the response. Variables registered anywhere in the
        chain are read from that task's context, and values forwarded from below are
        merged in; a forwarded value is closer to the endpoint, so it wins. Capturing
        closes registration for the whole chain, because anything registered later could
        no longer be captured before the response starts. Calling it again is a no-op, so
        `http.response.start` is captured exactly once per layer.
        """
        if self._captured:
            return {}
        self._captured = True
        captured: dict[ContextVar[Any], Any] = {}
        context = copy_context()
        for context_var in self._registered_vars():
            if context_var in context:
                captured[context_var] = context[context_var]
        captured.update(self._forwarded)
        for node in self._chain():
            node._started = True
        return captured

    def receive(self, values: Mapping[ContextVar[Any], Any]) -> None:
        """Apply this layer's variables and pass the rest up to the enclosing layer."""
        self._forwarded.update(values)
        for context_var in self._registered:
            if context_var in self._forwarded:
                self._tokens.append(context_var.set(self._forwarded[context_var]))
        if self._parent is not None:
            self._parent._forwarded.update(self._forwarded)

    def restore(self) -> None:
        """Undo every value this layer applied, restoring its task's context."""
        while self._tokens:
            token = self._tokens.pop()
            try:
                token.var.reset(token)
            except ValueError:  # pragma: no cover
                # The token was created in a context that no longer exists, e.g.
                # `call_next()` was awaited from a nested task.
                pass


def _as_context_vars(context_vars: Iterable[ContextVar[Any]]) -> tuple[ContextVar[Any], ...]:
    context_vars = tuple(context_vars)
    for context_var in context_vars:
        if not isinstance(context_var, ContextVar):
            raise TypeError(f"Expected a ContextVar instance, got {context_var!r}.")
    return context_vars


def register_context_vars(*context_vars: ContextVar[Any]) -> None:
    """Opt `ContextVar` instances into the return channel of `BaseHTTPMiddleware`.

    Call this from `dispatch()` before `call_next()` to have the values the downstream
    app sets for these variables copied back into the middleware's task. Only the
    variables registered this way are returned: unregistered variables keep their
    usual task isolation.

    The values are captured in the downstream task right before it sends
    `http.response.start`, and applied in the calling task before `call_next()`
    returns. The caller therefore sees them from the moment `call_next()` returns, and
    at every later point up to when it emits `http.response.start` itself. Values set
    after the response has started, e.g. while streaming the body, are not returned.
    If the downstream app raises instead of responding, nothing is applied and the
    exception propagates unchanged.

    Raises `RuntimeError` when called outside a `BaseHTTPMiddleware` dispatch, or once
    the response has started.
    """
    layer = _CONTEXT_RETURN_LAYER.get()
    if layer is None:
        raise RuntimeError(
            "register_context_vars() must be called from the dispatch() method of a BaseHTTPMiddleware instance."
        )
    layer.register(_as_context_vars(context_vars))


class _CachedRequest(Request):
    """
    If the user calls Request.body() from their dispatch function
    we cache the entire request body in memory and pass that to downstream middlewares,
    but if they call Request.stream() then all we do is send an
    empty body so that downstream things don't hang forever.
    """

    def __init__(self, scope: Scope, receive: Receive, send: Send):
        super().__init__(scope, receive, send)
        self._wrapped_rcv_disconnected = False
        self._wrapped_rcv_consumed = False

    async def wrapped_receive(self) -> Message:
        # wrapped_rcv state 1: disconnected
        if self._wrapped_rcv_disconnected:
            # we've already sent a disconnect to the downstream app
            # we don't need to wait to get another one
            # (although most ASGI servers will just keep sending it)
            return {"type": "http.disconnect"}
        # wrapped_rcv state 1: consumed but not yet disconnected
        if self._wrapped_rcv_consumed:
            # since the downstream app has consumed us all that is left
            # is to send it a disconnect
            if self._is_disconnected:
                # the middleware has already seen the disconnect
                # since we know the client is disconnected no need to wait
                # for the message
                self._wrapped_rcv_disconnected = True
                return {"type": "http.disconnect"}
            # we don't know yet if the client is disconnected or not
            # so we'll wait until we get that message
            msg = await self.receive()
            if msg["type"] != "http.disconnect":  # pragma: no cover
                # at this point a disconnect is all that we should be receiving
                # if we get something else, things went wrong somewhere
                raise RuntimeError(f"Unexpected message received: {msg['type']}")
            self._wrapped_rcv_disconnected = True
            return msg

        # wrapped_rcv state 3: not yet consumed
        if getattr(self, "_body", None) is not None:
            # body() was called, we return it even if the client disconnected
            self._wrapped_rcv_consumed = True
            return {
                "type": "http.request",
                "body": self._body,
                "more_body": False,
            }
        elif self._stream_consumed:
            # stream() was called to completion
            # return an empty body so that downstream apps don't hang
            # waiting for a disconnect
            self._wrapped_rcv_consumed = True
            return {
                "type": "http.request",
                "body": b"",
                "more_body": False,
            }
        else:
            # body() was never called and stream() wasn't consumed
            try:
                async with aclosing(self.stream()) as stream:
                    chunk = await stream.__anext__()
                self._wrapped_rcv_consumed = self._stream_consumed
                return {
                    "type": "http.request",
                    "body": chunk,
                    "more_body": not self._stream_consumed,
                }
            except ClientDisconnect:
                self._wrapped_rcv_disconnected = True
                return {"type": "http.disconnect"}


class BaseHTTPMiddleware:
    def __init__(
        self,
        app: ASGIApp,
        dispatch: DispatchFunction | None = None,
        *,
        context_vars: Iterable[ContextVar[Any]] = (),
    ) -> None:
        self.app = app
        self.dispatch_func = self.dispatch if dispatch is None else dispatch
        self.context_vars = _as_context_vars(context_vars)

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        request = _CachedRequest(scope, receive, send)
        wrapped_receive = request.wrapped_receive
        response_sent = anyio.Event()
        app_exc: Exception | None = None
        exception_already_raised = False
        context_return = _ContextReturn(_CONTEXT_RETURN_LAYER.get())
        context_return.register(self.context_vars)

        async def call_next(request: Request) -> Response:
            async def receive_or_disconnect() -> Message:
                if response_sent.is_set():
                    return {"type": "http.disconnect"}

                async with anyio.create_task_group() as task_group:

                    async def wrap(func: Callable[[], Awaitable[T]]) -> T:
                        result = await func()
                        task_group.cancel_scope.cancel()
                        return result

                    task_group.start_soon(wrap, response_sent.wait)
                    message = await wrap(wrapped_receive)

                if response_sent.is_set():
                    return {"type": "http.disconnect"}

                return message

            async def send_no_error(message: Message) -> None:
                if message["type"] == "http.response.start":
                    # Take the values from the task running the downstream app, right
                    # before it hands the response over to the middleware task.
                    values = context_return.capture()
                    if values:
                        try:
                            await send_stream.send(_ContextReturnValues(values))
                        except anyio.BrokenResourceError:  # pragma: no cover
                            pass

                try:
                    await send_stream.send(message)
                except anyio.BrokenResourceError:
                    # recv_stream has been closed, i.e. response_sent has been set.
                    return

                if message["type"] == "http.response.pathsend" or (
                    message["type"] == "http.response.body" and not message.get("more_body", False)
                ):
                    await response_sent.wait()

            async def coro() -> None:
                nonlocal app_exc

                with send_stream:
                    try:
                        await self.app(scope, receive_or_disconnect, send_no_error)
                    except Exception as exc:
                        app_exc = exc

            task_group.start_soon(coro)

            try:
                info = None
                while True:
                    message = await recv_stream.receive()
                    if isinstance(message, _ContextReturnValues):
                        # Applied in the middleware's own task, so `dispatch()` can
                        # read the values as soon as `call_next()` returns.
                        context_return.receive(message.values)
                        continue
                    if message["type"] == "http.response.debug":
                        info = message["info"]
                        continue
                    elif message["type"] == "http.response.early_hint":
                        await send(message)
                        continue
                    break
            except anyio.EndOfStream:
                if app_exc is not None:
                    nonlocal exception_already_raised
                    exception_already_raised = True
                    # Prevent `anyio.EndOfStream` from polluting app exception context.
                    # If both cause and context are None then the context is suppressed
                    # and `anyio.EndOfStream` is not present in the exception traceback.
                    # If exception cause is not None then it is propagated with
                    # reraising here.
                    # If exception has no cause but has context set then the context is
                    # propagated as a cause with the reraise. This is necessary in order
                    # to prevent `anyio.EndOfStream` from polluting the exception
                    # context.
                    raise app_exc from app_exc.__cause__ or app_exc.__context__
                raise RuntimeError("No response returned.")

            assert message["type"] == "http.response.start"

            async def body_stream() -> BodyStreamGenerator:
                async for message in recv_stream:
                    if isinstance(message, _ContextReturnValues):  # pragma: no cover
                        # The envelope is consumed before the response starts, so this
                        # is only reachable if an app sent a second response.start.
                        continue
                    if message["type"] == "http.response.pathsend":
                        yield message
                        break
                    assert message["type"] == "http.response.body", f"Unexpected message: {message}"
                    body = message.get("body", b"")
                    if body:
                        yield body
                    if not message.get("more_body", False):
                        break

            response = _StreamingResponse(status_code=message["status"], content=body_stream(), info=info)
            response.raw_headers = message.get("headers", [])
            return response

        send_stream, recv_stream = anyio.create_memory_object_stream[Message | _ContextReturnValues]()
        with recv_stream, send_stream:
            async with create_collapsing_task_group() as task_group:
                layer_token = _CONTEXT_RETURN_LAYER.set(context_return)
                try:
                    response = await self.dispatch_func(request, call_next)
                    await response(scope, wrapped_receive, send)
                    response_sent.set()
                    recv_stream.close()
                finally:
                    # Leave the caller's context as it was before this request,
                    # whatever the downstream app changed and however it ended.
                    _CONTEXT_RETURN_LAYER.reset(layer_token)
                    context_return.restore()
        if app_exc is not None and not exception_already_raised:
            raise app_exc

    async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
        raise NotImplementedError()  # pragma: no cover


class _StreamingResponse(Response):
    def __init__(
        self,
        content: AsyncContentStream,
        status_code: int = 200,
        headers: Mapping[str, str] | None = None,
        media_type: str | None = None,
        info: Mapping[str, Any] | None = None,
    ) -> None:
        self.info = info
        self.body_iterator = content
        self.status_code = status_code
        self.media_type = media_type
        self.init_headers(headers)
        self.background = None

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if self.info is not None:
            await send({"type": "http.response.debug", "info": self.info})
        await send(
            {
                "type": "http.response.start",
                "status": self.status_code,
                "headers": self.raw_headers,
            }
        )

        should_close_body = True
        async for chunk in self.body_iterator:
            if isinstance(chunk, dict):
                # We got an ASGI message which is not response body (eg: pathsend)
                should_close_body = False
                await send(chunk)
                continue
            await send({"type": "http.response.body", "body": chunk, "more_body": True})

        if should_close_body:
            await send({"type": "http.response.body", "body": b"", "more_body": False})

        if self.background:
            await self.background()
