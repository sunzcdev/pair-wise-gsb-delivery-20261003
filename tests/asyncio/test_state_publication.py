from __future__ import annotations

import asyncio
import json

import httpx  # type: ignore
import pytest
import wsproto

import hypercorn.asyncio
from hypercorn.config import Config
from hypercorn.state import StateStore
from hypercorn.typing import ASGIReceiveCallable, ASGISendCallable, Scope

PORT = 15571


async def _respond(send: ASGISendCallable, body: bytes, status: int = 200) -> None:
    await send(
        {
            "type": "http.response.start",
            "status": status,
            "headers": [(b"content-type", b"application/json")],
        }
    )
    await send({"type": "http.response.body", "body": body, "more_body": False})


async def _ws_connect() -> tuple:
    reader, writer = await asyncio.open_connection("127.0.0.1", PORT)
    connection = wsproto.WSConnection(wsproto.ConnectionType.CLIENT)
    writer.write(connection.send(wsproto.events.Request(host="hypercorn", target="/ws")))
    await writer.drain()
    while True:
        connection.receive_data(await reader.read(2**16))
        events = list(connection.events())
        if any(isinstance(event, wsproto.events.AcceptConnection) for event in events):
            return connection, reader, writer


async def _ws_query(
    connection: wsproto.WSConnection,
    reader: asyncio.StreamReader,
    writer: asyncio.StreamWriter,
) -> str:
    writer.write(connection.send(wsproto.events.TextMessage(data="query")))
    await writer.drain()
    while True:
        connection.receive_data(await reader.read(2**16))
        for event in connection.events():
            if isinstance(event, wsproto.events.TextMessage):
                return event.data


@pytest.mark.asyncio
async def test_paired_state_publication() -> None:
    store = StateStore({"primary": "primary-0", "secondary": "secondary-0"})
    startup_complete = asyncio.Event()
    hold_acquired = asyncio.Event()
    hold_release = asyncio.Event()

    async def app(scope: Scope, receive: ASGIReceiveCallable, send: ASGISendCallable) -> None:
        if scope["type"] == "lifespan":
            while True:
                message = await receive()
                if message["type"] == "lifespan.startup":
                    # Register the paired resources in the lifespan state
                    scope["state"]["resources"] = store
                    await send({"type": "lifespan.startup.complete"})
                    startup_complete.set()
                elif message["type"] == "lifespan.shutdown":
                    await send({"type": "lifespan.shutdown.complete"})
                    return
        elif scope["type"] == "http":
            path = scope["path"]
            if path == "/read":
                publication = scope["state"]["resources"].acquire()
                body = {
                    "version": publication.version,
                    "primary": publication["primary"],
                    "secondary": publication["secondary"],
                }
                await _respond(send, json.dumps(body).encode())
            elif path == "/hold":
                # Acquire a release, then block in-flight until released
                publication = scope["state"]["resources"].acquire()
                hold_acquired.set()
                await hold_release.wait()
                body = {
                    "version": publication.version,
                    "primary": publication["primary"],
                    "secondary": publication["secondary"],
                }
                await _respond(send, json.dumps(body).encode())
            elif path == "/publish":
                resources = scope["state"]["resources"]
                version = resources.version + 1
                release = resources.publish(
                    {"primary": f"primary-{version}", "secondary": f"secondary-{version}"}
                )
                await _respond(send, json.dumps({"version": release.version}).encode())
            elif path == "/publish-broken":
                resources = scope["state"]["resources"]
                try:
                    with resources.prepare() as staged:
                        staged["primary"] = "broken-primary"
                        staged["secondary"] = "broken-secondary"
                        raise RuntimeError("release failed")
                except RuntimeError:
                    pass
                await _respond(send, json.dumps({"version": resources.version}).encode())
            elif path == "/local-write":
                # Local request.state modifications must not leak back
                scope["state"]["resources"] = "clobbered"
                scope["state"]["primary"] = "clobbered"
                await _respond(send, b"{}")
            else:
                await _respond(send, b"{}", status=404)
        elif scope["type"] == "websocket":
            # A long lived scope keeps the release it acquired at connect
            publication = scope["state"]["resources"].acquire()
            while True:
                message = await receive()
                if message["type"] == "websocket.connect":
                    await send({"type": "websocket.accept"})
                elif message["type"] == "websocket.receive":
                    text = (
                        f"{publication.version}:{publication['primary']}"
                        f":{publication['secondary']}"
                    )
                    await send({"type": "websocket.send", "text": text})
                elif message["type"] == "websocket.disconnect":
                    return

    config = Config()
    config.bind = [f"127.0.0.1:{PORT}"]
    config.accesslog = "-"
    config.errorlog = "-"
    shutdown = asyncio.Event()
    serve_task = asyncio.get_running_loop().create_task(
        hypercorn.asyncio.serve(app, config, shutdown_trigger=shutdown.wait)
    )
    await asyncio.wait_for(startup_complete.wait(), timeout=5)
    try:
        async with httpx.AsyncClient(base_url=f"http://127.0.0.1:{PORT}") as client:
            # The initial release is a matched pair
            data = (await client.get("/read")).json()
            assert data == {"version": 0, "primary": "primary-0", "secondary": "secondary-0"}

            # An HTTP request enters the app and acquires release 0
            hold_task = asyncio.ensure_future(client.get("/hold"))
            await asyncio.wait_for(hold_acquired.wait(), timeout=5)

            # Publish the next release whilst /hold is in flight
            data = (await client.post("/publish")).json()
            assert data["version"] == 1

            # A new WebSocket acquires the new release as a matched pair
            connection, reader, writer = await _ws_connect()
            try:
                assert await _ws_query(connection, reader, writer) == "1:primary-1:secondary-1"

                # A new HTTP request also sees the new release as a matched pair
                data = (await client.get("/read")).json()
                assert data == {"version": 1, "primary": "primary-1", "secondary": "secondary-1"}

                # A deliberately failed release is discarded
                data = (await client.post("/publish-broken")).json()
                assert data["version"] == 1
                data = (await client.get("/read")).json()
                assert data == {"version": 1, "primary": "primary-1", "secondary": "secondary-1"}

                # The WebSocket continues to use the release it acquired
                assert await _ws_query(connection, reader, writer) == "1:primary-1:secondary-1"

                # The in-flight HTTP request kept the release it acquired
                hold_release.set()
                data = (await hold_task).json()
                assert data == {"version": 0, "primary": "primary-0", "secondary": "secondary-0"}

                # Local request.state writes do not clobber the global store
                await client.post("/local-write")
                data = (await client.get("/read")).json()
                assert data == {"version": 1, "primary": "primary-1", "secondary": "secondary-1"}

                # The next successful release becomes current for new scopes
                data = (await client.post("/publish")).json()
                assert data["version"] == 2
                data = (await client.get("/read")).json()
                assert data == {"version": 2, "primary": "primary-2", "secondary": "secondary-2"}

                # The long lived WebSocket still uses its acquired release
                assert await _ws_query(connection, reader, writer) == "1:primary-1:secondary-1"
            finally:
                writer.write(connection.send(wsproto.events.CloseConnection(code=1000)))
                await writer.drain()
                writer.close()
                await writer.wait_closed()
    finally:
        shutdown.set()
        await serve_task