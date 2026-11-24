from __future__ import annotations

import asyncio

import h2
import h11
import pytest
import wsproto

from hypercorn.app_wrappers import ASGIWrapper
from hypercorn.asyncio.tcp_server import TCPServer
from hypercorn.asyncio.worker_context import WorkerContext
from hypercorn.config import Config
from .helpers import MemoryReader, MemoryWriter
from ..helpers import SANITY_BODY, sanity_framework


@pytest.mark.asyncio
async def test_http1_request() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    client = h11.Connection(h11.CLIENT)
    await server.reader.send(  # type: ignore
        client.send(
            h11.Request(
                method="POST",
                target="/",
                headers=[
                    (b"host", b"hypercorn"),
                    (b"connection", b"close"),
                    (b"content-length", b"%d" % len(SANITY_BODY)),
                ],
            )
        )
    )
    await server.reader.send(client.send(h11.Data(data=SANITY_BODY)))  # type: ignore
    await server.reader.send(client.send(h11.EndOfMessage()))  # type: ignore
    events = []
    while True:
        event = client.next_event()
        if event == h11.NEED_DATA:
            data = await server.writer.receive()  # type: ignore
            client.receive_data(data)
        elif isinstance(event, h11.ConnectionClosed):
            break
        else:
            events.append(event)

    assert events == [
        h11.Response(
            status_code=200,
            headers=[
                (b"content-length", b"15"),
                (b"date", b"Thu, 01 Jan 1970 01:23:20 GMT"),
                (b"server", b"hypercorn-h11"),
                (b"connection", b"close"),
            ],
            http_version=b"1.1",
            reason=b"",
        ),
        h11.Data(data=b"Hello & Goodbye"),
        h11.EndOfMessage(headers=[]),
    ]
    server.reader.close()  # type: ignore
    await task


@pytest.mark.asyncio
async def test_http1_websocket() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    client = wsproto.WSConnection(wsproto.ConnectionType.CLIENT)
    await server.reader.send(  # type: ignore
        client.send(wsproto.events.Request(host="hypercorn", target="/"))
    )
    client.receive_data(await server.writer.receive())  # type: ignore
    assert list(client.events()) == [
        wsproto.events.AcceptConnection(
            extra_headers=[
                (b"date", b"Thu, 01 Jan 1970 01:23:20 GMT"),
                (b"server", b"hypercorn-h11"),
            ]
        )
    ]
    await server.reader.send(  # type: ignore
        client.send(wsproto.events.BytesMessage(data=SANITY_BODY))
    )
    client.receive_data(await server.writer.receive())  # type: ignore
    assert list(client.events()) == [wsproto.events.TextMessage(data="Hello & Goodbye")]
    await server.reader.send(client.send(wsproto.events.CloseConnection(code=1000)))  # type: ignore
    client.receive_data(await server.writer.receive())  # type: ignore
    assert list(client.events()) == [wsproto.events.CloseConnection(code=1000, reason="")]
    assert server.writer.is_closed  # type: ignore
    server.reader.close()  # type: ignore
    await task


@pytest.mark.asyncio
async def test_http2_request() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(http2=True),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    client = h2.connection.H2Connection()
    client.initiate_connection()
    await server.reader.send(client.data_to_send())  # type: ignore
    stream_id = client.get_next_available_stream_id()
    client.send_headers(
        stream_id,
        [
            (":method", "POST"),
            (":path", "/"),
            (":authority", "hypercorn"),
            (":scheme", "https"),
            ("content-length", "%d" % len(SANITY_BODY)),
        ],
    )
    client.send_data(stream_id, SANITY_BODY)
    client.end_stream(stream_id)
    await server.reader.send(client.data_to_send())  # type: ignore
    events = []
    open_ = True
    while open_:
        data = await server.writer.receive()  # type: ignore
        if data == b"":
            open_ = False

        h2_events = client.receive_data(data)
        for event in h2_events:
            if isinstance(event, h2.events.DataReceived):
                client.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
            elif isinstance(
                event,
                (h2.events.ConnectionTerminated, h2.events.StreamEnded, h2.events.StreamReset),
            ):
                open_ = False
                break
            else:
                events.append(event)
        await server.reader.send(client.data_to_send())  # type: ignore
    assert isinstance(events[2], h2.events.ResponseReceived)
    assert events[2].headers == [
        (b":status", b"200"),
        (b"content-length", b"15"),
        (b"date", b"Thu, 01 Jan 1970 01:23:20 GMT"),
        (b"server", b"hypercorn-h2"),
    ]
    client.close_connection()
    await server.reader.send(client.data_to_send())  # type: ignore
    await server.writer.receive()  # type: ignore
    assert server.writer.is_closed  # type: ignore
    server.reader.close()  # type: ignore
    await task


@pytest.mark.asyncio
async def test_http2_prior_knowledge() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    client = h2.connection.H2Connection()
    client.initiate_connection()
    preface = client.data_to_send()
    # The preface arrives in pieces, with the final preface bytes merged into
    # the same chunk as the first SETTINGS frame.
    split = 10
    await server.reader.send(preface[:split])  # type: ignore
    await asyncio.sleep(0)
    # No H2 SETTINGS may be sent before the full preface is confirmed.
    assert server.writer.data.empty()  # type: ignore
    await server.reader.send(preface[split:])  # type: ignore
    settings = await server.writer.receive()  # type: ignore
    events = client.receive_data(settings)
    assert any(isinstance(event, h2.events.RemoteSettingsChanged) for event in events)
    # The switch happens exactly once.
    assert server.protocol.protocol.__class__.__name__ == "H2Protocol"
    stream_id = client.get_next_available_stream_id()
    client.send_headers(
        stream_id,
        [
            (":method", "POST"),
            (":path", "/"),
            (":authority", "hypercorn"),
            (":scheme", "http"),
            ("content-length", "%d" % len(SANITY_BODY)),
        ],
    )
    client.send_data(stream_id, SANITY_BODY)
    client.end_stream(stream_id)
    await server.reader.send(client.data_to_send())  # type: ignore
    open_ = True
    while open_:
        data = await server.writer.receive()  # type: ignore
        if data == b"":
            open_ = False
        for event in client.receive_data(data):
            if isinstance(event, h2.events.DataReceived):
                client.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
            elif isinstance(
                event,
                (h2.events.ConnectionTerminated, h2.events.StreamEnded, h2.events.StreamReset),
            ):
                open_ = False
                break
        await server.reader.send(client.data_to_send())  # type: ignore
    client.close_connection()
    await server.reader.send(client.data_to_send())  # type: ignore
    server.reader.close()  # type: ignore
    await task


@pytest.mark.asyncio
async def test_http2_prior_knowledge_truncated_preface() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    # A partial preface followed by the end of the connection must result in a
    # plain close without HTTP/2 being initialised or a SETTINGS being sent.
    await server.reader.send(b"PRI * HTTP/2.0\r\n\r\nSM")  # type: ignore
    await asyncio.sleep(0)
    assert server.writer.data.empty()  # type: ignore
    assert server.protocol.protocol.__class__.__name__ == "H11Protocol"
    server.reader.close()  # type: ignore
    await task
    data = b""
    while not server.writer.data.empty():  # type: ignore
        data += server.writer.data.get_nowait()  # type: ignore
    assert data == b""
    assert server.writer.is_closed  # type: ignore


@pytest.mark.asyncio
async def test_http1_short_prefix_keep_alive() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    client = h11.Connection(h11.CLIENT)
    request = client.send(
        h11.Request(
            method="POST",
            target="/",
            headers=[
                (b"host", b"hypercorn"),
                (b"content-length", b"%d" % len(SANITY_BODY)),
            ],
        )
    )
    # Only a short prefix, which is enough to rule out the HTTP/2 preface but
    # not to form an HTTP/1 request, is sent first.
    await server.reader.send(request[:2])  # type: ignore
    await asyncio.sleep(0)
    assert server.writer.data.empty()  # type: ignore
    assert server.protocol.protocol.__class__.__name__ == "H11Protocol"
    await server.reader.send(request[2:])  # type: ignore
    await server.reader.send(client.send(h11.Data(data=SANITY_BODY)))  # type: ignore
    await server.reader.send(client.send(h11.EndOfMessage()))  # type: ignore
    while True:
        event = client.next_event()
        if event == h11.NEED_DATA:
            data = await server.writer.receive()  # type: ignore
            if data:  # the memory writer queues empty writes as no-op frames
                client.receive_data(data)
        elif isinstance(event, h11.EndOfMessage):
            break
    # A second request over the same keep-alive connection is still HTTP/1.
    client.start_next_cycle()
    await server.reader.send(  # type: ignore
        client.send(
            h11.Request(
                method="POST",
                target="/",
                headers=[
                    (b"host", b"hypercorn"),
                    (b"connection", b"close"),
                    (b"content-length", b"%d" % len(SANITY_BODY)),
                ],
            )
        )
    )
    await server.reader.send(client.send(h11.Data(data=SANITY_BODY)))  # type: ignore
    await server.reader.send(client.send(h11.EndOfMessage()))  # type: ignore
    while True:
        event = client.next_event()
        if event == h11.NEED_DATA:
            data = await server.writer.receive()  # type: ignore
            if data:
                client.receive_data(data)
        elif isinstance(event, h11.EndOfMessage):
            break
    assert server.protocol.protocol.__class__.__name__ == "H11Protocol"
    server.reader.close()  # type: ignore
    await task


@pytest.mark.asyncio
async def test_http2_websocket() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    server = TCPServer(
        ASGIWrapper(sanity_framework),
        event_loop,
        Config(),
        WorkerContext(None),
        {},
        MemoryReader(),  # type: ignore
        MemoryWriter(http2=True),  # type: ignore
    )
    task = event_loop.create_task(server.run())
    h2_client = h2.connection.H2Connection()
    h2_client.initiate_connection()
    await server.reader.send(h2_client.data_to_send())  # type: ignore
    stream_id = h2_client.get_next_available_stream_id()
    h2_client.send_headers(
        stream_id,
        [
            (":method", "CONNECT"),
            (":path", "/"),
            (":authority", "hypercorn"),
            (":scheme", "https"),
            ("sec-websocket-version", "13"),
        ],
    )
    await server.reader.send(h2_client.data_to_send())  # type: ignore
    events = h2_client.receive_data(await server.writer.receive())  # type: ignore
    await server.reader.send(h2_client.data_to_send())  # type: ignore
    events = h2_client.receive_data(await server.writer.receive())  # type: ignore
    events = h2_client.receive_data(await server.writer.receive())  # type: ignore
    assert isinstance(events[0], h2.events.ResponseReceived)
    assert events[0].headers == [
        (b":status", b"200"),
        (b"date", b"Thu, 01 Jan 1970 01:23:20 GMT"),
        (b"server", b"hypercorn-h2"),
    ]
    client = wsproto.connection.Connection(wsproto.ConnectionType.CLIENT)
    h2_client.send_data(stream_id, client.send(wsproto.events.BytesMessage(data=SANITY_BODY)))
    await server.reader.send(h2_client.data_to_send())  # type: ignore
    events = h2_client.receive_data(await server.writer.receive())  # type: ignore
    assert isinstance(events[0], h2.events.DataReceived)
    client.receive_data(events[0].data)
    assert list(client.events()) == [wsproto.events.TextMessage(data="Hello & Goodbye")]
    h2_client.send_data(stream_id, client.send(wsproto.events.CloseConnection(code=1000)))
    await server.reader.send(h2_client.data_to_send())  # type: ignore
    events = h2_client.receive_data(await server.writer.receive())  # type: ignore
    assert isinstance(events[0], h2.events.DataReceived)
    client.receive_data(events[0].data)
    assert list(client.events()) == [wsproto.events.CloseConnection(code=1000, reason="")]
    h2_client.close_connection()
    await server.reader.send(h2_client.data_to_send())  # type: ignore
    server.reader.close()  # type: ignore
    await task

