Since Uvicorn is an ASGI server, it supports the
[ASGI lifespan protocol](https://asgi.readthedocs.io/en/latest/specs/lifespan.html).
This allows you to run **startup** and **shutdown** events for your application.

The lifespan protocol is useful for initializing resources that need to be available throughout
the lifetime of the application, such as database connections, caches, or other services.

Keep in mind that the lifespan is executed **only once per application instance**. If you have
multiple workers, each worker will execute the lifespan independently.

## Lifespan Architecture

The lifespan protocol runs as a sibling task alongside your main application, allowing both to execute concurrently.

Let's see how Uvicorn handles the lifespan and main application tasks:

```mermaid
sequenceDiagram
    participant Server as Uvicorn Server
    participant LifespanTask as Lifespan Task
    participant AppTask as Application Task
    participant UserApp as User Application

    Note over Server: ✅ Server starts

    Server->>+LifespanTask: spawn_task(lifespan_handler)

    LifespanTask->>UserApp: {"type": "lifespan.startup"}

    Note over UserApp: Initialize databases, caches, etc.

    UserApp-->>LifespanTask: {"type": "lifespan.startup.complete"}
    LifespanTask->>Server: ✅ Startup complete

    Server->>+AppTask: spawn_task(application_handler)
    Note over AppTask: ✅ Ready for requests

    rect rgb(240, 248, 255)
        Note over LifespanTask, AppTask: Both tasks running concurrently

        par Lifespan maintains state
            LifespanTask->>LifespanTask: Keep lifespan connection alive
        and Application serves requests
            AppTask->>UserApp: HTTP/WebSocket requests
            UserApp-->>AppTask: Responses
        end
    end

    Note over Server: Shutdown signal received

    Server->>AppTask: Stop accepting new connections
    AppTask->>AppTask: Complete pending requests

    LifespanTask->>UserApp: {"type": "lifespan.shutdown"}

    Note over UserApp: Cleanup databases, caches, etc.

    UserApp-->>LifespanTask: {"type": "lifespan.shutdown.complete"}

    LifespanTask->>-Server: Lifespan task complete
    AppTask->>-Server: Application task complete

    Note over Server: ✅ Server stopped
```

Having the lifespan task run as a sibling task is a deliberate design choice. It could have been implemented as a parent task that spawns the
application task. This decision has the implication that if you create a [`ContextVar`][contextvars.ContextVar]
in the lifespan task, it will not be available in the application task.

## Usage

Let's see an example of a minimal (but complete) ASGI application that implements the lifespan protocol:

```python title="ASGI application with lifespan" hl_lines="3-11"
async def app(scope, receive, send):
    if scope['type'] == 'lifespan':
        while True:
            message = await receive()
            if message['type'] == 'lifespan.startup':
                print("Application is starting up...")
                await send({'type': 'lifespan.startup.complete'})
            elif message['type'] == 'lifespan.shutdown':
                print("Application is shutting down...")
                await send({'type': 'lifespan.shutdown.complete'})
                return
    elif scope['type'] == 'http':
        await send({
            'type': 'http.response.start',
            'status': 200,
            'headers': [(b'content-type', b'text/plain')],
        })
        await send({'type': 'http.response.body', 'body': b'Hello, World!'})
    else:
        raise RuntimeError("This server doesn't support WebSocket.")
```

You can run the above application with `uvicorn main:app`. Then you'll see the print statements when the
application starts. You can also try to send some HTTP requests to it, and it will respond with "Hello, World!".
And if you stop the server (`CTRL + C`), it will print `"Application is shutting down..."`.

## Disabling Lifespan

If you want to disable the lifespan protocol, you can do so by setting the `lifespan` option to `off` when running Uvicorn:

```bash
uvicorn main:app --lifespan off
```

By default, Uvicorn will automatically enable the lifespan protocol if the application supports it.

## Startup Timeout

An application that implements the lifespan protocol is expected to reply to the `lifespan.startup` event with either
`lifespan.startup.complete` or `lifespan.startup.failed`. A buggy application may instead accept the startup event and
never reply. Without protection, Uvicorn would stay alive but never reach the listening phase, appearing healthy while
serving no traffic.

Use `timeout_lifespan_startup` (or `--timeout-lifespan-startup`) to bound how long Uvicorn waits for that reply:

```bash
uvicorn main:app --timeout-lifespan-startup 30
```

If the timeout expires after the application has already taken over the startup event, Uvicorn:

1. reports a startup failure and exits with the startup-failure code, instead of opening any listener;
2. does **not** fall back to treating the application as one without a lifespan (this applies even with
   `lifespan="auto"`, since the application clearly implements the protocol);
3. cancels the application lifespan task so it receives a clear `CancelledError` and can run any cleanup it has
   registered; and
4. does not enter the shutdown phase a second time, since the cancelled lifespan is already being torn down.

An application that rejects the lifespan scope immediately (the classic signal that it does not implement the
lifespan protocol) is still handled by `lifespan="auto"` as before, and is unaffected by this timeout. When the option
is left unset, Uvicorn waits indefinitely, matching the historical behavior.
