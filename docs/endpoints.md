
Starlette includes the classes `HTTPEndpoint` and `WebSocketEndpoint` that provide a class-based view pattern for
handling HTTP method dispatching and WebSocket sessions.

### HTTPEndpoint

The `HTTPEndpoint` class can be used as an ASGI application:

```python
from starlette.responses import PlainTextResponse
from starlette.endpoints import HTTPEndpoint


class App(HTTPEndpoint):
    async def get(self, request):
        return PlainTextResponse("Hello, world!")
```

If you're using a Starlette application instance to handle routing, you can
dispatch to an `HTTPEndpoint` class. Make sure to dispatch to the class itself,
rather than to an instance of the class:

```python
from starlette.applications import Starlette
from starlette.responses import PlainTextResponse
from starlette.endpoints import HTTPEndpoint
from starlette.routing import Route


class Homepage(HTTPEndpoint):
    async def get(self, request):
        return PlainTextResponse("Hello, world!")

    async def query(self, request):
        return PlainTextResponse("Hello, query!")

class User(HTTPEndpoint):
    async def get(self, request):
        username = request.path_params['username']
        return PlainTextResponse(f"Hello, {username}")

routes = [
    Route("/", Homepage),
    Route("/{username}", User)
]

app = Starlette(routes=routes)
```

HTTP endpoint classes will respond with "405 Method not allowed" responses for any
request methods which do not map to a corresponding handler. A `get` handler also serves
`HEAD` requests, unless the endpoint defines its own `head` handler, so `HEAD` appears in the
`Allow` header of those 405 responses.

### WebSocketEndpoint

The `WebSocketEndpoint` class is an ASGI application that presents a wrapper around
the functionality of a `WebSocket` instance.

The ASGI connection scope is accessible on the endpoint instance via `.scope` and
has an attribute `encoding` which may optionally be set, in order to validate the expected websocket data in the `on_receive` method.

The encoding types are:

* `'json'`
* `'bytes'`
* `'text'`

There are three overridable methods for handling specific ASGI websocket message types:

* `async def on_connect(websocket, **kwargs)`
* `async def on_receive(websocket, data)`
* `async def on_disconnect(websocket, close_code)`

An additional hook, `async def select_encoding(websocket)`, controls how
incoming messages are decoded, as described below.

```python
from starlette.endpoints import WebSocketEndpoint


class App(WebSocketEndpoint):
    encoding = 'bytes'

    async def on_connect(self, websocket):
        await websocket.accept()

    async def on_receive(self, websocket, data):
        await websocket.send_bytes(b"Message: " + data)

    async def on_disconnect(self, websocket, close_code):
        pass
```

#### Selecting the encoding per connection

A single endpoint can serve connections that use different message formats by
setting `subprotocol_encodings`, which maps the negotiated subprotocol to an
encoding. The encoding is selected once per connection — after `on_connect()`
has run — and is then fixed for the lifetime of that connection, without
mutating the class-level `encoding` attribute shared with other connections.

The negotiated subprotocol is available as `websocket.accepted_subprotocol`
once `websocket.accept()` has been called. If no subprotocol was negotiated, or
the negotiated subprotocol is not present in `subprotocol_encodings`, the
connection is closed with a 1002 (protocol error) close code. A connection that
did negotiate but then receives a frame of the wrong type is closed with a 1003
(unsupported data) close code instead, and `on_disconnect` receives the
corresponding close code in each case.

```python
from starlette.endpoints import WebSocketEndpoint


class App(WebSocketEndpoint):
    subprotocol_encodings = {"json-v1": "json", "bin-v2": "bytes"}

    async def on_connect(self, websocket):
        for subprotocol in ("json-v1", "bin-v2"):
            if subprotocol in websocket["subprotocols"]:
                await websocket.accept(subprotocol=subprotocol)
                return
        await websocket.accept()

    async def on_receive(self, websocket, data):
        if isinstance(data, bytes):
            await websocket.send_bytes(b"Message: " + data)
        else:
            await websocket.send_json({"message": data})
```

For full control over how the encoding is chosen, override
`async def select_encoding(websocket)`, which is called exactly once per
connection and must return one of `'json'`, `'bytes'`, `'text'` or `None`.

The `WebSocketEndpoint` can also be used with the `Starlette` application class:

```python
import uvicorn
from starlette.applications import Starlette
from starlette.endpoints import WebSocketEndpoint, HTTPEndpoint
from starlette.responses import HTMLResponse
from starlette.routing import Route, WebSocketRoute


html = """
<!DOCTYPE html>
<html>
    <head>
        <title>Chat</title>
    </head>
    <body>
        <h1>WebSocket Chat</h1>
        <form action="" onsubmit="sendMessage(event)">
            <input type="text" id="messageText" autocomplete="off"/>
            <button>Send</button>
        </form>
        <ul id='messages'>
        </ul>
        <script>
            var ws = new WebSocket("ws://localhost:8000/ws");
            ws.onmessage = function(event) {
                var messages = document.getElementById('messages')
                var message = document.createElement('li')
                var content = document.createTextNode(event.data)
                message.appendChild(content)
                messages.appendChild(message)
            };
            function sendMessage(event) {
                var input = document.getElementById("messageText")
                ws.send(input.value)
                input.value = ''
                event.preventDefault()
            }
        </script>
    </body>
</html>
"""

class Homepage(HTTPEndpoint):
    async def get(self, request):
        return HTMLResponse(html)

class Echo(WebSocketEndpoint):
    encoding = "text"

    async def on_receive(self, websocket, data):
        await websocket.send_text(f"Message text was: {data}")

routes = [
    Route("/", Homepage),
    WebSocketRoute("/ws", Echo)
]

app = Starlette(routes=routes)
```
