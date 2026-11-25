from __future__ import annotations

import asyncio
from types import SimpleNamespace
from unittest.mock import AsyncMock, call, Mock

import pytest
from h2.config import H2Configuration
from h2.connection import H2Connection
from h2.events import ConnectionTerminated, PushedStreamReceived

from hypercorn.asyncio.worker_context import EventWrapper, WorkerContext
from hypercorn.config import Config
from hypercorn.events import Closed, RawData
from hypercorn.protocol.events import Request
from hypercorn.protocol.h2 import (
    BUFFER_HIGH_WATER,
    BufferCompleteError,
    H2Protocol,
    PushDeduplicator,
    StreamBuffer,
    push_identity,
)
from hypercorn.typing import ConnectionState


@pytest.mark.asyncio
async def test_stream_buffer_push_and_pop() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    stream_buffer = StreamBuffer(EventWrapper)

    async def _push_over_limit() -> bool:
        await stream_buffer.push(b"a" * (BUFFER_HIGH_WATER + 1))
        return True

    task = event_loop.create_task(_push_over_limit())
    assert not task.done()  # Blocked as over high water
    await stream_buffer.pop(BUFFER_HIGH_WATER // 4)
    assert not task.done()  # Blocked as over low water
    await stream_buffer.pop(BUFFER_HIGH_WATER // 4)
    assert (await task) is True


@pytest.mark.asyncio
async def test_stream_buffer_drain() -> None:
    event_loop: asyncio.AbstractEventLoop = asyncio.get_running_loop()

    stream_buffer = StreamBuffer(EventWrapper)
    await stream_buffer.push(b"a" * 10)

    async def _drain() -> bool:
        await stream_buffer.drain()
        return True

    task = event_loop.create_task(_drain())
    assert not task.done()  # Blocked
    await stream_buffer.pop(20)
    assert (await task) is True


@pytest.mark.asyncio
async def test_stream_buffer_closed() -> None:
    stream_buffer = StreamBuffer(EventWrapper)
    await stream_buffer.close()
    await stream_buffer._is_empty.wait()
    await stream_buffer._paused.wait()
    assert True
    with pytest.raises(BufferCompleteError):
        await stream_buffer.push(b"a")


@pytest.mark.asyncio
async def test_stream_buffer_complete() -> None:
    stream_buffer = StreamBuffer(EventWrapper)
    await stream_buffer.push(b"a" * 10)
    assert not stream_buffer.complete
    stream_buffer.set_complete()
    assert not stream_buffer.complete
    await stream_buffer.pop(20)
    assert stream_buffer.complete


@pytest.mark.asyncio
async def test_protocol_handle_protocol_error() -> None:
    protocol = H2Protocol(
        Mock(),
        Config(),
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        False,
        None,
        None,
        AsyncMock(),
    )
    await protocol.handle(RawData(data=b"broken nonsense\r\n\r\n"))
    protocol.send.assert_awaited()  # type: ignore
    assert protocol.send.call_args_list == [call(Closed())]  # type: ignore


@pytest.mark.asyncio
async def test_protocol_keep_alive_max_requests() -> None:
    protocol = H2Protocol(
        Mock(),
        Config(),
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        False,
        None,
        None,
        AsyncMock(),
    )
    protocol.config.keep_alive_max_requests = 0
    client = H2Connection()
    client.initiate_connection()
    headers = [
        (":method", "GET"),
        (":path", "/reqinfo"),
        (":authority", "hypercorn"),
        (":scheme", "https"),
    ]
    client.send_headers(1, headers, end_stream=True)
    await protocol.handle(RawData(data=client.data_to_send()))
    protocol.send.assert_awaited()  # type: ignore
    events = client.receive_data(protocol.send.call_args_list[1].args[0].data)  # type: ignore
    assert isinstance(events[-1], ConnectionTerminated)


def _push_request(stream_id: int, language: bytes) -> Request:
    return Request(
        stream_id=stream_id,
        headers=[
            (b":scheme", b"https"),
            (b":authority", b"example.com"),
            (b"accept-language", language),
        ],
        http_version="2",
        method="GET",
        raw_path=b"/cached-resource",
        state=ConnectionState({}),
    )


def test_push_identity_distinguishes_context_and_representation_but_not_parent() -> None:
    first = _push_request(1, b"en")
    equivalent = _push_request(3, b"en")
    different_language = _push_request(5, b"fr")
    different_context = Request(
        stream_id=7,
        headers=first.headers,
        http_version="2",
        method="GET",
        raw_path=first.raw_path,
        state=ConnectionState({"tenant": "other"}),
    )

    assert push_identity(first) == push_identity(equivalent)
    assert push_identity(first) != push_identity(different_language)
    assert push_identity(first) != push_identity(different_context)


def test_push_deduplicator_releases_candidate_but_remembers_sent_promise() -> None:
    rule = PushDeduplicator()
    identity = push_identity(_push_request(1, b"en"))

    assert rule.admit(identity, 1)
    assert not rule.admit(identity, 3)

    rule.close_stream(1)
    assert rule.admit(identity, 3)
    assert rule.begin_write(identity, 3)
    assert rule.commit(identity, 3, promised_stream_id=2)

    rule.close_stream(3)
    assert not rule.admit(identity, 5)
    assert rule.sent[identity] == 3
    assert rule.promised_stream_ids[identity] == 2


@pytest.mark.asyncio
async def test_deduplicated_push_admission_revokes_unwritten_candidate() -> None:
    config = Config()
    config.push_deduplication = True
    protocol = H2Protocol(
        Mock(),
        config,
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        True,
        None,
        None,
        AsyncMock(),
    )
    protocol.connection = Mock()
    protocol.connection.data_to_send.return_value = b""
    protocol.connection.get_next_available_stream_id.side_effect = [2, 4, 6, 8]

    async def _create_push_stream(event: object) -> None:
        stream_id = getattr(event, "stream_id")
        protocol.streams[stream_id] = SimpleNamespace(handle=AsyncMock())

    protocol._create_stream = _create_push_stream  # type: ignore[method-assign]

    first_push = asyncio.create_task(protocol.stream_send(_push_request(1, b"en")))
    await asyncio.sleep(0)  # Reserve before the write checkpoint.
    assert isinstance(protocol.push_admission, PushDeduplicator)
    identity = push_identity(_push_request(1, b"en"))
    assert protocol.push_admission.is_candidate(identity, 1)

    await protocol._close_stream(1)
    await first_push

    await asyncio.gather(
        protocol.stream_send(_push_request(3, b"en")),
        protocol.stream_send(_push_request(5, b"en")),
        protocol.stream_send(_push_request(7, b"fr")),
    )

    promises = [
        {
            "parent": promise.kwargs["stream_id"],
            "language": dict(promise.kwargs["request_headers"])[b"accept-language"],
        }
        for promise in protocol.connection.push_stream.call_args_list
    ]
    assert promises == [
        {"parent": 3, "language": b"en"},
        {"parent": 7, "language": b"fr"},
    ]

    # Closing the owner after the PUSH_PROMISE was emitted must not forget it.
    await protocol._close_stream(3)
    await protocol.stream_send(_push_request(9, b"en"))
    assert len(protocol.connection.push_stream.call_args_list) == 2


@pytest.mark.asyncio
async def test_legacy_connections_still_push_every_application_request() -> None:
    protocol = H2Protocol(
        Mock(),
        Config(),
        WorkerContext(None),
        AsyncMock(),
        ConnectionState({}),
        True,
        None,
        None,
        AsyncMock(),
    )
    protocol.connection = Mock()
    protocol.connection.data_to_send.return_value = b""
    protocol.connection.get_next_available_stream_id.side_effect = [2, 4, 6]

    async def _create_push_stream(event: object) -> None:
        stream_id = getattr(event, "stream_id")
        protocol.streams[stream_id] = SimpleNamespace(handle=AsyncMock())

    protocol._create_stream = _create_push_stream  # type: ignore[method-assign]

    for stream_id, language in ((1, b"en"), (3, b"en"), (5, b"fr")):
        await protocol.stream_send(_push_request(stream_id, language))

    assert [
        call.kwargs["stream_id"] for call in protocol.connection.push_stream.call_args_list
    ] == [1, 3, 5]


@pytest.mark.asyncio
async def test_deduplicated_push_uses_actual_http2_promise_frames() -> None:
    config = Config()
    config.h2_push_deduplicate = True
    client = H2Connection(config=H2Configuration(client_side=True, header_encoding=None))
    client.initiate_connection()
    wire_events: list[object] = []

    async def send(event: object) -> None:
        if isinstance(event, RawData):
            wire_events.extend(client.receive_data(event.data))

    protocol = H2Protocol(
        Mock(),
        config,
        WorkerContext(None),
        Mock(),
        ConnectionState({}),
        True,
        None,
        ("127.0.0.1", 443),
        send,
    )

    async def _create_push_stream(event: object) -> None:
        stream_id = getattr(event, "stream_id")
        protocol.streams[stream_id] = SimpleNamespace(handle=AsyncMock())

    protocol._create_stream = _create_push_stream  # type: ignore[method-assign]
    await protocol.initiate()
    await protocol.handle(RawData(data=client.data_to_send()))
    await protocol.handle(RawData(data=client.data_to_send()))

    parent_headers = [
        (":method", "GET"),
        (":path", "/page"),
        (":scheme", "https"),
        (":authority", "example.com"),
    ]
    for stream_id in (1, 3, 5, 7, 9):
        client.send_headers(stream_id, parent_headers, end_stream=True)
    await protocol.handle(RawData(data=client.data_to_send()))
    await protocol.handle(RawData(data=client.data_to_send()))

    first_push = asyncio.create_task(protocol.stream_send(_push_request(1, b"en")))
    await asyncio.sleep(0)
    await protocol._close_stream(1)
    await first_push

    await asyncio.gather(
        protocol.stream_send(_push_request(3, b"en")),
        protocol.stream_send(_push_request(5, b"en")),
        protocol.stream_send(_push_request(7, b"fr")),
    )
    await protocol._close_stream(3)
    await protocol.stream_send(_push_request(9, b"en"))

    promises = [event for event in wire_events if isinstance(event, PushedStreamReceived)]
    actual = [
        (event.parent_stream_id, dict(event.headers)[b"accept-language"])
        for event in promises
    ]
    assert actual == [(3, b"en"), (7, b"fr")]
