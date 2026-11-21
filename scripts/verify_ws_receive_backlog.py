"""Verify the WebSocketsSansIOProtocol receive backlog policy.

Drives an in-process WebSocketsSansIOProtocol with a sans-io client (no sockets):
the app pauses consumption while fragmented messages, pings and close frames arrive,
and the script checks what the peer and the app each observe when the configured
backlog limits (pending messages / pending bytes) are reached.

Run: python scripts/verify_ws_receive_backlog.py
"""

import asyncio
import sys
from typing import Any

from websockets.client import ClientProtocol
from websockets.frames import Close, Frame, Opcode
from websockets.uri import parse_uri

from uvicorn.config import Config
from uvicorn.protocols.websockets.websockets_sansio_impl import WebSocketsSansIOProtocol
from uvicorn.server import ServerState

FAILURES = []


def check(name: str, condition: bool, detail: str = "") -> None:
    status = "PASS" if condition else "FAIL"
    print(f"  [{status}] {name}" + (f" -- {detail}" if detail and not condition else ""))
    if not condition:
        FAILURES.append(name)


class MockWriteTransport:
    """Minimal transport for driving the protocol's write path in-process."""

    def __init__(self) -> None:
        self.buffer = b""
        self.closed = False
        self.reading_paused = False

    def get_extra_info(self, name: str, default: Any = None) -> Any:
        return {"sockname": ("127.0.0.1", 8000), "peername": ("127.0.0.1", 8001)}.get(name, default)

    def write(self, data: bytes) -> None:
        self.buffer += data

    def close(self) -> None:
        self.closed = True

    def is_closing(self) -> bool:
        return self.closed

    def pause_reading(self) -> None:
        self.reading_paused = True

    def resume_reading(self) -> None:
        self.reading_paused = False


class BacklogTestConnection:
    """An in-process WebSocket connection: a sans-io client wired to the server protocol."""

    def __init__(
        self,
        app: Any,
        ws_receive_backlog_messages: int | None = None,
        ws_receive_backlog_bytes: int | None = None,
    ) -> None:
        config = Config(
            app=app,
            ws=WebSocketsSansIOProtocol,
            lifespan="off",
            ws_receive_backlog_messages=ws_receive_backlog_messages,
            ws_receive_backlog_bytes=ws_receive_backlog_bytes,
        )
        config.load()
        self._server_state = ServerState()
        self.protocol = WebSocketsSansIOProtocol(config=config, server_state=self._server_state, app_state={})
        self.transport = MockWriteTransport()
        self.client = ClientProtocol(parse_uri("ws://127.0.0.1/"))
        self._bytes_read = 0
        self._outbox = b""

    async def __aenter__(self) -> "BacklogTestConnection":
        self.protocol.connection_made(self.transport)  # type: ignore[arg-type]
        self.client.send_request(self.client.connect())
        self.protocol.data_received(b"".join(self.client.data_to_send()))
        return self

    async def __aexit__(self, *exc_info: object) -> None:
        self.protocol.connection_lost(None)

    async def app_finished(self) -> None:
        await asyncio.gather(*self._server_state.tasks)

    # Client actions (each delivered to the server as its own read).

    def send_message_fragments(self, parts: list[bytes], data_type: str = "text", final: bool = True) -> None:
        """Send a (possibly fragmented) message. With `final=False` the last frame
        carries no FIN, leaving the message incomplete."""
        self._read_response()
        first_opcode = Opcode.TEXT if data_type == "text" else Opcode.BINARY
        for index, part in enumerate(parts):
            opcode = first_opcode if index == 0 else Opcode.CONT
            fin = final and index == len(parts) - 1
            self.client.send_frame(Frame(opcode, part, fin=fin))
        self._flush()

    def send_continuation(self, part: bytes, final: bool = False) -> None:
        """Send a single continuation frame for a message started earlier."""
        self._read_response()
        self.client.send_frame(Frame(Opcode.CONT, part, fin=final))
        self._flush()

    def send_ping(self, payload: bytes = b"ping!") -> None:
        self._read_response()
        self.client.send_ping(payload)
        self._flush()

    def send_close(self, code: int = 1000) -> None:
        self._read_response()
        self.client.send_close(code)
        self._flush()

    # Server-side observations.

    def server_events(self) -> list[Any]:
        """Feed unread server output to the client and return the events it parsed."""
        self._read_response()
        data = self.transport.buffer[self._bytes_read :]
        self._bytes_read = len(self.transport.buffer)
        self.client.receive_data(data)
        return self.client.events_received()

    def close_received_by_peer(self) -> Close | None:
        self.server_events()
        return self.client.close_rcvd

    def _flush(self) -> None:
        self._outbox += b"".join(self.client.data_to_send())
        data, self._outbox = self._outbox, b""
        self.protocol.data_received(data)

    def _read_response(self) -> None:
        if self._bytes_read == 0:
            self._bytes_read = self.transport.buffer.index(b"\r\n\r\n") + 4
            self.client.receive_data(self.transport.buffer[: self._bytes_read])


def pausing_app(accepted: asyncio.Event, resume: asyncio.Event, received: list) -> Any:
    """ASGI app that accepts, then stops calling receive() until `resume` is set."""

    async def app(scope: dict, receive: Any, send: Any) -> None:
        message = await receive()  # websocket.connect
        await send({"type": "websocket.accept"})
        accepted.set()
        await resume.wait()  # consumption paused: the backlog can build up
        while True:
            event = await receive()
            received.append(event)
            if event["type"] == "websocket.disconnect":
                break

    return app


async def scenario_message_limit_with_fragments_and_ping() -> None:
    """Message-count limit: fragmented messages queue up, ping is still answered,
    and the message that crosses the limit fails the connection."""
    print("Scenario A: message-count limit, fragmented messages + ping while paused")
    accepted, resume, received = asyncio.Event(), asyncio.Event(), []

    async with BacklogTestConnection(pausing_app(accepted, resume, received), ws_receive_backlog_messages=2) as conn:
        await accepted.wait()

        conn.send_message_fragments([b"Hello, ", b"world"], data_type="text")
        conn.send_message_fragments([b"\x01", b"\x02", b"\x03"], data_type="bytes")
        check("reads paused while the app is not consuming", conn.transport.reading_paused)
        check("two messages pending", conn.protocol.backlog_messages == 2)
        check("pending bytes counted", conn.protocol.backlog_bytes == len(b"Hello, world") + 3)

        conn.send_ping()
        peer_events = conn.server_events()
        pongs = [e for e in peer_events if isinstance(e, Frame) and e.opcode == Opcode.PONG]
        check("ping answered with pong despite the backlog", len(pongs) == 1)
        check("connection still open below the limit", not conn.transport.closed)

        conn.send_message_fragments([b"boom"], data_type="text")  # third pending message
        close = conn.close_received_by_peer()
        check("peer gets close 1009 when the limit is crossed", close is not None and close.code == 1009, repr(close))
        check(
            "peer close reason names the backlog",
            close is not None and close.reason == "receive backlog exceeded",
            repr(close),
        )
        check("transport closed after failing the connection", conn.transport.closed)

        resume.set()  # the app resumes consuming
        await conn.app_finished()

    types = [event["type"] for event in received]
    check("app drains queued messages before the disconnect", types == ["websocket.receive"] * 3 + ["websocket.disconnect"], repr(types))
    check("first message reassembled from fragments", received[0].get("text") == "Hello, world", repr(received[0]))
    check("second message reassembled from fragments", received[1].get("bytes") == b"\x01\x02\x03", repr(received[1]))
    check("message order preserved after resume", received[2].get("text") == "boom", repr(received[2]))
    disconnect = received[-1]
    check("app gets disconnect 1009", disconnect["code"] == 1009, repr(disconnect))
    check("backlog counters return to zero", conn.protocol.backlog_messages == 0 and conn.protocol.backlog_bytes == 0)


async def scenario_byte_limit_on_incomplete_fragments() -> None:
    """Byte limit: a fragmented message is charged by its actual footprint before
    it is fully assembled."""
    print("Scenario B: byte limit tripped by an incomplete fragmented message")
    accepted, resume, received = asyncio.Event(), asyncio.Event(), []

    async with BacklogTestConnection(pausing_app(accepted, resume, received), ws_receive_backlog_bytes=10) as conn:
        await accepted.wait()

        conn.send_message_fragments([b"Hello, ", b"wor"], data_type="text", final=False)  # 10 bytes, no FIN yet
        check("incomplete fragments charged by footprint", conn.protocol.backlog_bytes == 10)
        check("connection open at (not past) the byte limit", not conn.transport.closed)

        conn.send_continuation(b"ld!")  # 13 bytes total > 10, message still incomplete

        close = conn.close_received_by_peer()
        check("peer gets close 1009 on incomplete fragments", close is not None and close.code == 1009, repr(close))
        check("no complete message was ever queued", conn.protocol.backlog_messages == 0)

        resume.set()
        await conn.app_finished()

    check("app gets disconnect 1009 with no prior message", [e["type"] for e in received] == ["websocket.disconnect"], repr(received))
    check("disconnect code is 1009", received[0]["code"] == 1009, repr(received[0]))


async def scenario_close_and_ping_conventions_preserved() -> None:
    """Control frames and terminal events keep their conventions while message data
    is pending: ping is answered, a peer close is echoed and delivered in order."""
    print("Scenario C: ping and close handling with a pending (in-limit) backlog")
    accepted, resume, received = asyncio.Event(), asyncio.Event(), []

    async with BacklogTestConnection(
        pausing_app(accepted, resume, received),
        ws_receive_backlog_messages=4,
        ws_receive_backlog_bytes=64,
    ) as conn:
        await accepted.wait()

        conn.send_message_fragments([b"pending"], data_type="text")
        conn.send_ping()
        conn.send_close(1000)

        peer_events = conn.server_events()
        frames = [e for e in peer_events if isinstance(e, Frame)]
        check("pong sent for the ping", any(f.opcode == Opcode.PONG for f in frames), repr(frames))
        close_frames = [f for f in frames if f.opcode == Opcode.CLOSE]
        check("peer close echoed back", len(close_frames) == 1 and Close.parse(close_frames[0].data).code == 1000, repr(frames))
        check("transport closed after the close handshake", conn.transport.closed)
        check("backlog not failed by control frames", not conn.protocol.backlog_exceeded)

        resume.set()
        await conn.app_finished()

    types = [event["type"] for event in received]
    check("app gets the pending message then the disconnect", types == ["websocket.receive", "websocket.disconnect"], repr(types))
    check("pending message intact", received[0].get("text") == "pending", repr(received[0]))
    check("disconnect carries the peer's close code", received[1]["code"] == 1000, repr(received[1]))


async def main() -> int:
    await scenario_message_limit_with_fragments_and_ping()
    await scenario_byte_limit_on_incomplete_fragments()
    await scenario_close_and_ping_conventions_preserved()
    print()
    if FAILURES:
        print(f"{len(FAILURES)} check(s) FAILED: {FAILURES}")
        return 1
    print("All receive backlog checks passed.")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
