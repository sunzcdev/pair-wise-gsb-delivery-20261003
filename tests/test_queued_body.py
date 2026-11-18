"""
test_queued_body
~~~~~~~~~~~~~~~~

Tests of the queued body (``queue_data`` / ``flush_queued_data``) interface.

The queueing tests are all expressed in terms of two real ``H2Connection``
objects feeding serialized bytes to each other, so that ordering, framing and
flow control accounting are checked end-to-end without any real server.
"""
from __future__ import annotations

import pytest
from hyperframe.frame import DataFrame, Frame

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.exceptions
import h2.settings

REQUEST_HEADERS = [
    (":method", "POST"),
    (":path", "/"),
    (":authority", "example.com"),
    (":scheme", "https"),
]

RESPONSE_HEADERS = [
    (":status", "200"),
]


class PeerPair:
    """
    A client and a server ``H2Connection`` wired together over byte strings.

    ``pump`` moves every byte one side wants to send into the other side and
    returns the events that produced, which makes it trivial to drive a whole
    exchange through the automatic queued-body flushing.
    """

    def __init__(self) -> None:
        self.client = h2.connection.H2Connection()
        self.server = h2.connection.H2Connection(
            config=h2.config.H2Configuration(client_side=False),
        )
        self.client_events: list[h2.events.Event] = []
        self.server_events: list[h2.events.Event] = []

    def handshake(self) -> None:
        """Perform the HTTP/2 preamble dance."""
        self.client.initiate_connection()
        self.server.receive_data(self.client.data_to_send())
        self.server.initiate_connection()
        self.client.receive_data(self.server.data_to_send())
        # Drop the SETTINGS ACKs so later assertions start from a clean slate.
        self.pump()

    def send_request(self, stream_id: int = 1, end_stream: bool = False) -> None:
        """Open a request stream from the client, without sending a body."""
        self.client.send_headers(stream_id, REQUEST_HEADERS, end_stream=end_stream)
        self.pump()

    def pump(self) -> tuple[list[h2.events.Event], list[h2.events.Event]]:
        """
        Move bytes in both directions until both connections are quiet.

        Returns the events generated on the client and server respectively for
        this pump, and appends them to the running per-peer event lists.
        """
        client_events: list[h2.events.Event] = []
        server_events: list[h2.events.Event] = []

        # Alternate directions until neither connection has anything to say.
        while True:
            data = self.client.data_to_send()
            if data:
                server_events.extend(self.server.receive_data(data))
            data = self.server.data_to_send()
            if data:
                client_events.extend(self.client.receive_data(data))
            if not data:
                break

        self.client_events.extend(client_events)
        self.server_events.extend(server_events)
        return client_events, server_events


def _decode_data_frames(data: bytes) -> list[DataFrame]:
    """
    Parse a byte string into the list of DATA frames it contains.

    Any non-DATA frames (SETTINGS, WINDOW_UPDATE, ...) are ignored.
    """
    frames: list[DataFrame] = []
    offset = 0
    while offset < len(data):
        frame, length = Frame.parse_frame_header(memoryview(data[offset:offset + 9]))
        frame.parse_body(memoryview(data[offset + 9:offset + 9 + length]))
        if isinstance(frame, DataFrame):
            frames.append(frame)
        offset += 9 + length
    return frames


class TestQueueDataBasics:
    """Unit-level tests of queue_data's validation and bookkeeping."""

    def test_single_chunk_is_sent_without_explicit_flush(self) -> None:
        """
        queue_data emits data immediately when the window allows it.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"hello world")

        frames = _decode_data_frames(c.data_to_send())
        assert [f.data for f in frames] == [b"hello world"]
        assert not any("END_STREAM" in f.flags for f in frames)
        assert c.queued_body_size == 0
        assert c.local_flow_control_window(1) == 65535 - len(b"hello world")

    def test_large_chunk_is_split_by_max_frame_size(self) -> None:
        """
        A chunk bigger than max_outbound_frame_size is split into frames of at
        most that size, in order.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.max_outbound_frame_size = 10
        c._get_stream_by_id(1).max_outbound_frame_size = 10
        c.clear_outbound_data_buffer()

        body = bytes(range(25))
        c.queue_data(1, body)

        frames = _decode_data_frames(c.data_to_send())
        assert [len(f.data) for f in frames] == [10, 10, 5]
        assert b"".join(f.data for f in frames) == body
        assert c.queued_body_size == 0

    def test_data_is_split_by_stream_window(self) -> None:
        """
        A chunk bigger than the stream window is partially sent and the rest
        stays queued.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 4
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"0123456789")

        frames = _decode_data_frames(c.data_to_send())
        assert [f.data for f in frames] == [b"0123"]
        assert c.queued_body_size == 6
        assert c._get_stream_by_id(1).outbound_flow_control_window == 0
        # The unsent portion stayed behind, in order.
        assert b"".join(c._body_queues[1]) == b"456789"

    def test_data_is_split_by_connection_window(self) -> None:
        """
        A chunk bigger than the connection window is partially sent and the
        rest stays queued.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.outbound_flow_control_window = 3
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"abcdef")

        frames = _decode_data_frames(c.data_to_send())
        assert [f.data for f in frames] == [b"abc"]
        assert c.queued_body_size == 3
        assert c.outbound_flow_control_window == 0

    def test_end_stream_is_only_attached_to_the_last_frame(self) -> None:
        """
        END_STREAM is set on the very last DATA frame for the stream, and only
        there, even when the body spans several frames.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.max_outbound_frame_size = 4
        c._get_stream_by_id(1).max_outbound_frame_size = 4
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"abcdefghij", end_stream=True)

        frames = _decode_data_frames(c.data_to_send())
        assert [f.data for f in frames] == [b"abcd", b"efgh", b"ij"]
        assert [
            "END_STREAM" in f.flags for f in frames
        ] == [False, False, True]

    def test_end_stream_can_be_set_with_empty_body(self) -> None:
        """
        Queueing an empty body with end_stream=True emits a single empty
        END_STREAM DATA frame.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"", end_stream=True)

        frames = _decode_data_frames(c.data_to_send())
        assert len(frames) == 1
        assert frames[0].data == b""
        assert "END_STREAM" in frames[0].flags
        assert c.queued_body_size == 0

    def test_end_stream_cannot_be_lost_by_later_queue_data(self) -> None:
        """
        Once a stream is queued to end, no further body data may be queued for
        it: otherwise END_STREAM could be attached to a non-final frame.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 2

        c.queue_data(1, b"abcd", end_stream=True)
        with pytest.raises(h2.exceptions.ProtocolError):
            c.queue_data(1, b"more")

    def test_multiple_chunks_keep_order(self) -> None:
        """
        Several queue_data calls on one stream preserve call order.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"one")
        c.queue_data(1, b"two")
        c.queue_data(1, b"three", end_stream=True)

        frames = _decode_data_frames(c.data_to_send())
        assert b"".join(f.data for f in frames) == b"onetwothree"

    def test_empty_queue_data_without_end_stream_is_a_no_op(self) -> None:
        """
        Queueing an empty body without ending the stream touches nothing.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"", end_stream=False)

        assert not c.data_to_send()
        assert c.queued_body_size == 0
        assert c._body_queue_for(1) is None

    def test_bytearray_and_memoryview_are_accepted(self) -> None:
        """
        queue_data accepts any of the common byte-like types.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        c.queue_data(1, bytearray(b"abc"))
        c.queue_data(1, memoryview(b"def"))

        frames = _decode_data_frames(c.data_to_send())
        assert b"".join(f.data for f in frames) == b"abcdef"

    def test_invalid_data_type_is_rejected(self) -> None:
        """
        queue_data refuses things that are not byte-like.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)

        with pytest.raises(TypeError):
            c.queue_data(1, "not bytes")  # type: ignore[arg-type]

        assert c.queued_body_size == 0

    def test_send_data_is_rejected_while_body_is_queued(self) -> None:
        """
        Mixing send_data with the queued interface on one stream is refused, so
        that END_STREAM ordering cannot be violated.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 1
        c.queue_data(1, b"blocked")

        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_data(1, b"x")

    def test_end_stream_is_rejected_while_body_is_queued(self) -> None:
        """
        end_stream would end a stream while body bytes are still queued.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 1
        c.queue_data(1, b"blocked")

        with pytest.raises(h2.exceptions.ProtocolError):
            c.end_stream(1)

    def test_send_headers_cannot_end_stream_while_body_is_queued(self) -> None:
        """
        Trailers with END_STREAM are likewise refused while body is queued.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 1
        c.queue_data(1, b"blocked")

        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_headers(1, [("x-trailer", "y")], end_stream=True)

    def test_queue_data_requires_headers_first(self) -> None:
        """
        Data cannot be queued on a stream that has not been opened.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        # Stream 3 exists but is still IDLE: headers must come first.
        c._begin_new_stream(3, h2.connection.AllowedStreamIDs.ODD)

        with pytest.raises(h2.exceptions.ProtocolError):
            c.queue_data(3, b"data")

    def test_queue_data_on_closed_stream_raises(self) -> None:
        """
        Data cannot be queued on a locally half-closed or reset stream.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS, end_stream=True)

        with pytest.raises(h2.exceptions.StreamClosedError):
            c.queue_data(1, b"data")

        c.send_headers(3, REQUEST_HEADERS)
        c.reset_stream(3)
        with pytest.raises(h2.exceptions.StreamClosedError):
            c.queue_data(3, b"data")


class TestQueueDataResumption:
    """
    Tests that queued data resumes when flow control windows open, without
    blocking other streams and without over-restoring windows on reset.
    """

    def _client_with_queued_body(self, body: bytes) -> h2.connection.H2Connection:
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()
        c.queue_data(1, body)
        return c

    def test_window_update_resumes_queued_body(self, frame_factory) -> None:
        """
        Receiving a WINDOW_UPDATE automatically sends more queued body.
        """
        c = self._client_with_queued_body(b"0123456789")
        assert c.queued_body_size == 0

        # Squeeze the stream window shut, then queue a body that cannot be
        # fully flushed.
        c._get_stream_by_id(1).outbound_flow_control_window = 0
        c.queue_data(1, b"abcdef")
        assert c.queued_body_size == 6
        c.clear_outbound_data_buffer()

        f = frame_factory.build_window_update_frame(stream_id=1, increment=4)
        c.receive_data(f.serialize())

        frames = _decode_data_frames(c.data_to_send())
        assert [f.data for f in frames] == [b"abcd"]
        assert c.queued_body_size == 2

    def test_connection_window_update_resumes_queued_body(self, frame_factory) -> None:
        """
        A connection-level WINDOW_UPDATE also unblocks queued body.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.outbound_flow_control_window = 0
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"abcdef")
        assert c.queued_body_size == 6
        assert not c.data_to_send()

        f = frame_factory.build_window_update_frame(stream_id=0, increment=10)
        c.receive_data(f.serialize())

        frames = _decode_data_frames(c.data_to_send())
        assert b"".join(f.data for f in frames) == b"abcdef"
        assert c.queued_body_size == 0

    def test_end_stream_waits_for_the_last_queued_byte(self, frame_factory) -> None:
        """
        END_STREAM is not emitted until the whole queued body has gone out,
        even across several window updates.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.outbound_flow_control_window = 2
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"abcdef", end_stream=True)
        sent_frames: list[DataFrame] = []

        while c.queued_body_size:
            sent_frames.extend(_decode_data_frames(c.data_to_send()))
            for frame in sent_frames[:-1]:
                assert "END_STREAM" not in frame.flags
            f = frame_factory.build_window_update_frame(stream_id=0, increment=2)
            c.receive_data(f.serialize())

        sent_frames.extend(_decode_data_frames(c.data_to_send()))
        assert b"".join(f.data for f in sent_frames) == b"abcdef"
        assert "END_STREAM" in sent_frames[-1].flags
        assert sum(
            "END_STREAM" in f.flags for f in sent_frames
        ) == 1

    def test_reset_discards_body_without_restoring_window(self) -> None:
        """
        Resetting a stream drops its unsent body. Only bytes that were actually
        sent consumed window, so no window is handed back for the rest.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 4
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"0123456789", end_stream=True)

        # Four bytes were sent and consumed the window; six remain queued.
        assert c._get_stream_by_id(1).outbound_flow_control_window == 0
        assert c.queued_body_size == 6
        c.data_to_send()

        c.reset_stream(1)

        assert c.queued_body_size == 0
        assert c._body_queue_for(1) is None
        # The window is untouched by the reset: the six dropped bytes were
        # never sent, so they neither consumed nor restore any window.
        assert c._get_stream_by_id(1).outbound_flow_control_window == 0
        assert c.outbound_flow_control_window == 65535 - 4

    def test_remote_reset_discards_body(self, frame_factory) -> None:
        """
        A RST_STREAM from the peer also drops locally queued body.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 1
        c.queue_data(1, b"0123456789")

        assert c.queued_body_size == 9

        f = frame_factory.build_rst_stream_frame(
            stream_id=1, error_code=h2.errors.ErrorCodes.CANCEL,
        )
        c.receive_data(f.serialize())

        assert c.queued_body_size == 0
        assert c._body_queue_for(1) is None

    def test_closing_the_connection_discards_queued_body(self) -> None:
        """
        GOAWAY means nothing more can be sent: queued body is dropped and no
        window is restored.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 1
        c.queue_data(1, b"0123456789")

        window_before = c.outbound_flow_control_window
        c.close_connection()

        assert c.queued_body_size == 0
        assert c._body_queue_order == [] or not c._body_queue_order
        assert c.outbound_flow_control_window == window_before

    def test_peer_goaway_discards_queued_body(self, frame_factory) -> None:
        """
        Receiving GOAWAY likewise drops queued body.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 1
        c.queue_data(1, b"0123456789")

        f = frame_factory.build_goaway_frame(last_stream_id=0)
        c.receive_data(f.serialize())

        assert c.queued_body_size == 0

    def test_blocked_stream_does_not_block_others(self, frame_factory) -> None:
        """
        A stream stuck at a zero window must not stop another stream from
        sending its queued body.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.send_headers(3, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        # Stream 1 is completely blocked; stream 3 is wide open.
        c._get_stream_by_id(1).outbound_flow_control_window = 0
        c.queue_data(1, b"aaaa")
        c.queue_data(3, b"bbbb")

        assert c.queued_body_size == 4
        assert list(c._body_queues[1]) == [b"aaaa"]

        frames = _decode_data_frames(c.data_to_send())
        assert [f.stream_id for f in frames] == [3]
        assert frames[0].data == b"bbbb"

    def test_blocked_stream_resumes_after_its_own_window_update(self, frame_factory) -> None:
        """
        Once its own window opens, the previously blocked stream continues in
        order without duplicating or dropping bytes.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.send_headers(3, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        c._get_stream_by_id(1).outbound_flow_control_window = 0
        c.queue_data(1, b"aaaa")
        c.queue_data(3, b"bbbb")
        c.data_to_send()

        f = frame_factory.build_window_update_frame(stream_id=1, increment=4)
        c.receive_data(f.serialize())

        frames = _decode_data_frames(c.data_to_send())
        assert [(f.stream_id, f.data) for f in frames] == [(1, b"aaaa")]
        assert c.queued_body_size == 0

    def test_interleaves_streams_round_robin(self) -> None:
        """
        Two unblocked streams with large bodies are advanced in turn rather
        than one stream monopolising the connection window.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.send_headers(3, REQUEST_HEADERS)
        c.max_outbound_frame_size = 2
        c._get_stream_by_id(1).max_outbound_frame_size = 2
        c._get_stream_by_id(3).max_outbound_frame_size = 2
        # Keep the connection window shut so that both bodies are fully queued
        # before any of them can be sent.
        c.outbound_flow_control_window = 0
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"aaa")
        c.queue_data(3, b"bbb")
        assert c.queued_body_size == 6
        assert not c.data_to_send()

        # Open the connection window by exactly four bytes: enough for two
        # frames, which must be shared fairly between the two streams.
        c.outbound_flow_control_window = 4
        c.flush_queued_data()

        frames = _decode_data_frames(c.data_to_send())
        assert [(f.stream_id, f.data) for f in frames] == [
            (1, b"aa"), (3, b"bb"),
        ]
        assert c.queued_body_size == 2


class TestQueueFull:
    """
    The queue is bounded and a rejected call must not accept a partial body.
    """

    def _client(self) -> h2.connection.H2Connection:
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 0
        c.clear_outbound_data_buffer()
        c.max_queued_body_size = 8
        return c

    def test_oversized_chunk_is_rejected_whole(self) -> None:
        """
        A chunk that would overflow the queue raises and queues nothing.
        """
        c = self._client()

        with pytest.raises(h2.exceptions.QueueFullError):
            c.queue_data(1, b"x" * 9)

        assert c.queued_body_size == 0
        assert c._body_queue_for(1) is None

    def test_rejected_chunk_does_not_partially_queue(self) -> None:
        """
        After a rejection the previous contents of the queue are untouched.
        """
        c = self._client()

        c.queue_data(1, b"abc")
        assert c.queued_body_size == 3

        with pytest.raises(h2.exceptions.QueueFullError):
            c.queue_data(1, b"defghij")

        assert c.queued_body_size == 3
        assert b"".join(c._body_queues[1]) == b"abc"

    def test_queue_capacity_is_shared_across_streams(self) -> None:
        """
        The bound applies to the total buffered body, not per stream.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.send_headers(3, REQUEST_HEADERS)
        c.outbound_flow_control_window = 0
        c.clear_outbound_data_buffer()
        c.max_queued_body_size = 8

        c.queue_data(1, b"abcde")
        with pytest.raises(h2.exceptions.QueueFullError):
            c.queue_data(3, b"fghi")

        assert c.queued_body_size == 5
        assert c._body_queue_for(3) is None

    def test_flushing_frees_queue_capacity(self) -> None:
        """
        Once queued bytes are sent, they no longer count against the bound.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 4
        c.clear_outbound_data_buffer()
        c.max_queued_body_size = 4

        c.queue_data(1, b"abcd")
        assert c.queued_body_size == 0
        assert b"".join(
            f.data for f in _decode_data_frames(c.data_to_send())
        ) == b"abcd"

        # The stream window was consumed by that flush, so reopen it before
        # queueing another full-size chunk.
        c._get_stream_by_id(1).outbound_flow_control_window = 4
        c.queue_data(1, b"efgh")
        assert c.queued_body_size == 0
        frames = _decode_data_frames(c.data_to_send())
        assert b"".join(f.data for f in frames) == b"efgh"

    def test_queued_body_size_is_reported(self) -> None:
        """
        queued_body_size reflects the buffered bytes.
        """
        c = self._client()
        assert c.queued_body_size == 0

        c.queue_data(1, b"abc")
        assert c.queued_body_size == 3

        c.queue_data(1, b"de")
        assert c.queued_body_size == 5


class TestTwoPeerExchange:
    """
    End-to-end checks using two ``H2Connection`` objects feeding each other
    serialized bytes. No real server is involved.
    """

    def test_full_body_arrives_in_order(self) -> None:
        """
        A queued body larger than one frame is reassembled intact by the peer.
        """
        pair = PeerPair()
        pair.handshake()
        pair.server.receive_data(pair.client.data_to_send())
        pair.send_request(stream_id=1)

        body = bytes(range(256)) * 40  # 10 KiB
        pair.client.max_outbound_frame_size = 1024
        pair.client._get_stream_by_id(1).max_outbound_frame_size = 1024
        pair.client.queue_data(1, body, end_stream=True)
        pair.pump()

        received = b"".join(
            event.data
            for event in pair.server_events
            if isinstance(event, h2.events.DataReceived)
        )
        assert received == body
        assert any(
            isinstance(event, h2.events.StreamEnded) and event.stream_id == 1
            for event in pair.server_events
        )

    def test_stream_ended_exactly_once(self) -> None:
        """
        END_STREAM reaches the peer exactly once for a queued body, even when
        empty chunks are interleaved with the real body.
        """
        pair = PeerPair()
        pair.handshake()
        pair.send_request(stream_id=1)

        pair.client.queue_data(1, b"01234")
        pair.client.queue_data(1, b"", end_stream=False)
        pair.client.queue_data(1, b"56789", end_stream=True)
        pair.pump()

        ended = [
            event for event in pair.server_events
            if isinstance(event, h2.events.StreamEnded)
        ]
        assert len(ended) == 1
        assert ended[0].stream_id == 1

    def test_windows_agree_after_exchange(self) -> None:
        """
        After a body has been fully sent, both peers agree byte-for-byte on how
        much of the stream and connection windows was consumed.
        """
        pair = PeerPair()
        pair.handshake()
        pair.send_request(stream_id=1)

        body = b"z" * 5000
        pair.client.queue_data(1, body, end_stream=True)
        pair.pump()

        assert pair.client.queued_body_size == 0

        # The sender has consumed exactly len(body) from both its windows...
        assert pair.client.local_flow_control_window(1) == 65535 - len(body)
        assert pair.client.outbound_flow_control_window == 65535 - len(body)
        # ...and the receiver agrees, from its own accounting.
        assert pair.server.remote_flow_control_window(1) == 65535 - len(body)
        assert pair.server.inbound_flow_control_window == 65535 - len(body)

        # Acknowledging the received bytes lets h2 decide when to re-open the
        # windows; whatever it decides, both halves of the connection must end
        # up with exactly the same view of both windows.
        for event in pair.server_events:
            if isinstance(event, h2.events.DataReceived):
                pair.server.acknowledge_received_data(
                    event.flow_controlled_length, event.stream_id,
                )
        pair.pump()

        assert (
            pair.client.outbound_flow_control_window ==
            pair.server.inbound_flow_control_window
        )
        assert (
            pair.client.local_flow_control_window(1) ==
            pair.server.remote_flow_control_window(1)
        )
        assert pair.client.queued_body_size == 0

    def test_acknowledgement_reopens_windows_as_it_does_for_send_data(self) -> None:
        """
        Acknowledging received queued-body bytes re-opens windows using exactly
        the same coalescing strategy as data sent with send_data, so the sender
        sees the freed window and keeps both views in sync.
        """
        pair = PeerPair()
        pair.handshake()
        pair.send_request(stream_id=1)

        body = b"z" * 40000
        pair.client.queue_data(1, body, end_stream=True)
        pair.pump()

        connection_window_after_send = pair.client.outbound_flow_control_window
        for event in pair.server_events:
            if isinstance(event, h2.events.DataReceived):
                pair.server.acknowledge_received_data(
                    event.flow_controlled_length, event.stream_id,
                )
        pair.pump()

        # The acknowledgement freed a coalesced chunk of the window, and the
        # peer's WINDOW_UPDATE was applied by the sender.
        assert pair.client.outbound_flow_control_window > connection_window_after_send
        assert (
            pair.client.outbound_flow_control_window ==
            pair.server.inbound_flow_control_window
        )
        assert (
            pair.client.local_flow_control_window(1) ==
            pair.server.remote_flow_control_window(1)
        )

    def test_windows_agree_when_peer_limits_windows(self) -> None:
        """
        With a small server-advertised stream window, the queued body is sent
        in several rounds, each triggered by the server's WINDOW_UPDATE, and
        the final windows line up exactly.
        """
        pair = PeerPair()
        # Advertise an initial stream window of only 100 bytes.
        pair.server.local_settings = h2.settings.Settings(
            client=False,
            initial_values={
                h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS: 100,
                h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 100,
            },
        )
        pair.client.initiate_connection()
        pair.server.receive_data(pair.client.data_to_send())
        pair.server.initiate_connection()
        pair.client.receive_data(pair.server.data_to_send())
        pair.pump()

        pair.send_request(stream_id=1)
        assert pair.client.local_flow_control_window(1) == 100

        body = bytes(range(50)) * 10  # 500 bytes
        pair.client.queue_data(1, body, end_stream=True)
        # Only the first 100-byte frame fits right now.
        assert pair.client.queued_body_size == len(body) - 100

        received = b""
        rounds = 0
        for _ in range(20):
            pair.pump()
            rounds += 1
            for event in pair.server_events:
                if isinstance(event, h2.events.DataReceived):
                    received += event.data
                    pair.server.acknowledge_received_data(
                        event.flow_controlled_length, event.stream_id,
                    )
            pair.server_events.clear()
            if pair.client.queued_body_size == 0:
                break

        pair.pump()
        for event in pair.server_events:
            if isinstance(event, h2.events.DataReceived):
                received += event.data

        assert received == body
        assert pair.client.queued_body_size == 0
        # The body needed several rounds, driven by WINDOW_UPDATE frames.
        assert rounds > 1
        # The client consumed exactly the body length from the connection
        # window, and the server's view matches.
        assert pair.client.outbound_flow_control_window == 65535 - len(body)
        assert pair.server.inbound_flow_control_window == 65535 - len(body)
        # The stream window has been fully reopened by the acknowledgements.
        assert pair.server.remote_flow_control_window(1) == 100
        assert pair.client.local_flow_control_window(1) == 100

    def test_one_blocked_stream_does_not_stop_another(self) -> None:
        """
        Two concurrent uploads progress independently: one stream stuck on a
        zero stream window does not stop the other from being delivered whole.
        """
        pair = PeerPair()
        pair.handshake()
        pair.send_request(stream_id=1)
        pair.send_request(stream_id=3)

        pair.client._get_stream_by_id(1).outbound_flow_control_window = 0
        pair.client.queue_data(1, b"blocked-body")
        pair.client.queue_data(3, b"free-body", end_stream=True)
        pair.pump()

        received = b"".join(
            event.data
            for event in pair.server_events
            if isinstance(event, h2.events.DataReceived)
        )
        assert received == b"free-body"
        assert pair.client.queued_body_size == len(b"blocked-body")

    def test_reset_mid_body_drops_the_rest(self) -> None:
        """
        Resetting a stream mid-upload stops the body and the peer sees the
        stream reset rather than a truncated body that claims to be complete.
        """
        pair = PeerPair()
        pair.handshake()
        pair.send_request(stream_id=1)

        pair.client._get_stream_by_id(1).outbound_flow_control_window = 4
        pair.client.queue_data(1, b"0123456789", end_stream=True)
        pair.pump()
        assert pair.client.queued_body_size == 6

        pair.client.reset_stream(1, h2.errors.ErrorCodes.CANCEL)
        assert pair.client.queued_body_size == 0
        pair.client_events.clear()
        pair.server_events.clear()
        pair.pump()

        assert any(
            isinstance(event, h2.events.StreamReset)
            for event in pair.server_events
        )
        # The remaining bytes were never sent.
        assert not any(
            isinstance(event, h2.events.StreamEnded)
            for event in pair.server_events
        )

    def test_queue_full_retry_delivers_every_byte_once(self) -> None:
        """
        A body that overflows the queue can be retried chunk by chunk without
        duplicating or losing bytes.
        """
        pair = PeerPair()
        pair.handshake()
        pair.send_request(stream_id=1)

        pair.client.max_queued_body_size = 16
        body = b"".join(bytes([i]) for i in range(40))
        offset = 0
        chunk_size = 10
        while offset < len(body):
            chunk = body[offset:offset + chunk_size]
            try:
                pair.client.queue_data(
                    1, chunk, end_stream=offset + len(chunk) == len(body),
                )
            except h2.exceptions.QueueFullError:
                pair.pump()
                assert pair.client.queued_body_size < 16
                continue
            offset += len(chunk)

        pair.pump()

        received = b"".join(
            event.data
            for event in pair.server_events
            if isinstance(event, h2.events.DataReceived)
        )
        assert received == body
        assert any(
            isinstance(event, h2.events.StreamEnded)
            for event in pair.server_events
        )

    def test_queue_data_on_closed_connection_raises(self) -> None:
        """
        Once the connection is closed, queue_data refuses the whole chunk
        rather than leaving it half-queued behind a failing DATA send.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.close_connection()

        with pytest.raises(h2.exceptions.ProtocolError):
            c.queue_data(1, b"data")

        assert c.queued_body_size == 0
        assert c._body_queue_for(1) is None

    def test_locally_reset_stream_drops_queued_body(self, frame_factory) -> None:
        """
        A stream closed by h2 itself (here: a WINDOW_UPDATE that overflows the
        stream window) drops its queued body instead of leaving it orphaned.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c._get_stream_by_id(1).outbound_flow_control_window = 5
        c.queue_data(1, b"queued-body")
        # Five bytes fit, the rest stays queued; five bytes of window remain.
        assert c._get_stream_by_id(1).outbound_flow_control_window == 0
        assert c.queued_body_size == len(b"queued-body") - 5
        c._get_stream_by_id(1).outbound_flow_control_window = 5
        c.clear_outbound_data_buffer()

        # An increment that overflows the largest possible window forces h2 to
        # reset the stream locally.
        f = frame_factory.build_window_update_frame(
            stream_id=1, increment=2**31 - 1,
        )
        c.receive_data(f.serialize())

        assert c._get_stream_by_id(1).closed
        assert c.queued_body_size == 0
        assert c._body_queue_for(1) is None

    def test_empty_end_stream_survives_negative_window(self) -> None:
        """
        An empty queued body with end_stream=True must still be able to end the
        stream when a SETTINGS change has driven the window negative.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.outbound_flow_control_window = -10
        c._get_stream_by_id(1).outbound_flow_control_window = -10
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"", end_stream=True)

        frames = _decode_data_frames(c.data_to_send())
        assert len(frames) == 1
        assert frames[0].data == b""
        assert "END_STREAM" in frames[0].flags

    def test_queued_body_resumes_after_settings_shrink(self, frame_factory) -> None:
        """
        A SETTINGS_INITIAL_WINDOW_SIZE change that shrinks the window below the
        queued body's size simply leaves the remainder queued, and later
        WINDOW_UPDATE frames finish the job.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, REQUEST_HEADERS)
        c.clear_outbound_data_buffer()

        # Shrink the stream window to 5 bytes.
        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 5},
        )
        c.receive_data(f.serialize())
        c.clear_outbound_data_buffer()

        c.queue_data(1, b"0123456789", end_stream=True)
        assert c.queued_body_size == 5
        assert b"".join(
            frame.data for frame in _decode_data_frames(c.data_to_send())
        ) == b"01234"

        # Grow the window back; the rest is flushed automatically.
        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 65535},
        )
        c.receive_data(f.serialize())

        sent = _decode_data_frames(c.data_to_send())
        assert b"".join(frame.data for frame in sent) == b"56789"
        assert "END_STREAM" in sent[-1].flags
        assert c.queued_body_size == 0
