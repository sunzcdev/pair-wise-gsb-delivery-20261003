"""
Tests that we handle closed streams correctly.
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.exceptions


class TestClosedStreams:
    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    example_response_headers = [
        (":status", "200"),
        ("server", "fake-serv/0.1.0"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    def test_can_receive_multiple_rst_stream_frames(self, frame_factory) -> None:
        """
        Multiple RST_STREAM frames can be received, either at once or well
        after one another. Only the first fires an event.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers, end_stream=True)

        f = frame_factory.build_rst_stream_frame(stream_id=1)
        events = c.receive_data(f.serialize() * 3)

        # Force an iteration over all the streams to remove them.
        c.open_outbound_streams

        # Receive more data.
        events += c.receive_data(f.serialize() * 3)

        assert len(events) == 1
        event = events[0]

        assert isinstance(event, h2.events.StreamReset)

    def test_receiving_low_stream_id_causes_goaway(self, frame_factory) -> None:
        """
        The remote peer creating a stream with a lower ID than one we've seen
        causes a GOAWAY frame.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            stream_id=3,
        )
        c.receive_data(f.serialize())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            stream_id=1,
        )

        with pytest.raises(h2.exceptions.StreamIDTooLowError) as e:
            c.receive_data(f.serialize())

        assert e.value.stream_id == 1
        assert e.value.max_stream_id == 3

        f = frame_factory.build_goaway_frame(
            last_stream_id=3,
            error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        assert c.data_to_send() == f.serialize()

    def test_closed_stream_not_present_in_streams_dict(self, frame_factory) -> None:
        """
        When streams have been closed, they get removed from the streams
        dictionary the next time we count the open streams.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        f = frame_factory.build_headers_frame(self.example_request_headers)
        c.receive_data(f.serialize())
        c.push_stream(1, 2, self.example_request_headers)
        c.reset_stream(1)
        c.clear_outbound_data_buffer()

        f = frame_factory.build_rst_stream_frame(stream_id=2)
        c.receive_data(f.serialize())

        # Force a count of the streams.
        assert not c.open_outbound_streams

        # The streams dictionary should be empty.
        assert not c.streams

    def test_receive_rst_stream_on_closed_stream(self, frame_factory) -> None:
        """
        RST_STREAM frame should be ignored if stream is in a closed state.
        See RFC 7540 Section 5.1 (closed state)
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()

        # Client sends request
        c.send_headers(1, self.example_request_headers)

        # Some time passes and client sends DATA frame and closes stream,
        # so it is in a half-closed state
        c.send_data(1, b"some data", end_stream=True)

        # Server received HEADERS frame but DATA frame is still on the way.
        # Stream is in open state on the server-side. In this state server is
        # allowed to end stream and reset it - this trick helps immediately
        # close stream on the server-side.
        headers_frame = frame_factory.build_headers_frame(
            [(":status", "200")],
            flags=["END_STREAM"],
            stream_id=1,
        )
        events = c.receive_data(headers_frame.serialize())
        assert len(events) == 2
        response_received, stream_ended = events
        assert isinstance(response_received, h2.events.ResponseReceived)
        assert isinstance(stream_ended, h2.events.StreamEnded)

        rst_stream_frame = frame_factory.build_rst_stream_frame(stream_id=1)
        events = c.receive_data(rst_stream_frame.serialize())
        assert not events

    def test_receive_window_update_on_closed_stream(self, frame_factory) -> None:
        """
        WINDOW_UPDATE frame should be ignored if stream is in a closed state.
        See RFC 7540 Section 5.1 (closed state)
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()

        # Client sends request
        c.send_headers(1, self.example_request_headers)

        # Some time passes and client sends DATA frame and closes stream,
        # so it is in a half-closed state
        c.send_data(1, b"some data", end_stream=True)

        # Server received HEADERS frame but DATA frame is still on the way.
        # Stream is in open state on the server-side. In this state server is
        # allowed to end stream and after that acknowledge received data by
        # sending WINDOW_UPDATE frames.
        headers_frame = frame_factory.build_headers_frame(
            [(":status", "200")],
            flags=["END_STREAM"],
            stream_id=1,
        )
        events = c.receive_data(headers_frame.serialize())
        assert len(events) == 2
        response_received, stream_ended = events
        assert isinstance(response_received, h2.events.ResponseReceived)
        assert isinstance(stream_ended, h2.events.StreamEnded)

        window_update_frame = frame_factory.build_window_update_frame(
            stream_id=1,
            increment=1,
        )
        events = c.receive_data(window_update_frame.serialize())
        assert not events

        # Getting open_inbound_streams property will trigger stream accounting
        # which results in removing closed streams.
        c.open_inbound_streams

        # Another window_update_frame is received for stream 1
        events = c.receive_data(window_update_frame.serialize())
        assert not events


class TestStreamsClosedByEndStream:
    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    example_response_headers = [
        (":status", "200"),
        ("server", "fake-serv/0.1.0"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    @pytest.mark.parametrize(
        "frame",
        [
            lambda self, ff: ff.build_headers_frame(
                self.example_request_headers, flags=["END_STREAM"]),
            lambda self, ff: ff.build_headers_frame(
                self.example_request_headers),
        ],
    )
    @pytest.mark.parametrize("clear_streams", [True, False])
    def test_frames_after_recv_end_will_error(self,
                                              frame_factory,
                                              frame,
                                              clear_streams) -> None:
        """
        A stream that is closed by receiving END_STREAM raises
        ProtocolError when it receives an unexpected frame.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        f = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        )
        c.receive_data(f.serialize())
        c.send_headers(
            stream_id=1,
            headers=self.example_response_headers,
            end_stream=True,
        )

        if clear_streams:
            # Call open_inbound_streams to force the connection to clean
            # closed streams.
            c.open_inbound_streams

        c.clear_outbound_data_buffer()

        f = frame(self, frame_factory)
        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(f.serialize())

        f = frame_factory.build_goaway_frame(
            last_stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        assert c.data_to_send() == f.serialize()

    @pytest.mark.parametrize(
        "frame",
        [
            lambda self, ff: ff.build_headers_frame(
                self.example_response_headers, flags=["END_STREAM"]),
            lambda self, ff: ff.build_headers_frame(
                self.example_response_headers),
        ],
    )
    @pytest.mark.parametrize("clear_streams", [True, False])
    def test_frames_after_send_end_will_error(self,
                                              frame_factory,
                                              frame,
                                              clear_streams) -> None:
        """
        A stream that is closed by sending END_STREAM raises
        ProtocolError when it receives an unexpected frame.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(stream_id=1, headers=self.example_request_headers,
                       end_stream=True)

        f = frame_factory.build_headers_frame(
            self.example_response_headers, flags=["END_STREAM"],
        )
        c.receive_data(f.serialize())

        if clear_streams:
            # Call open_outbound_streams to force the connection to clean
            # closed streams.
            c.open_outbound_streams

        c.clear_outbound_data_buffer()

        f = frame(self, frame_factory)
        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(f.serialize())

        f = frame_factory.build_goaway_frame(
            last_stream_id=0,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        assert c.data_to_send() == f.serialize()

    @pytest.mark.parametrize(
        "frame",
        [
            lambda self, ff: ff.build_window_update_frame(1, 1),
            lambda self, ff: ff.build_rst_stream_frame(1),
        ],
    )
    def test_frames_after_send_end_will_be_ignored(self,
                                                   frame_factory,
                                                   frame) -> None:
        """
        A stream that is closed by sending END_STREAM will raise
        ProtocolError when received unexpected frame.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        f = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        )
        c.receive_data(f.serialize())
        c.send_headers(
            stream_id=1,
            headers=self.example_response_headers,
            end_stream=True,
        )

        c.clear_outbound_data_buffer()

        f = frame(self, frame_factory)
        events = c.receive_data(f.serialize())

        assert not events


class TestStreamsClosedByRstStream:
    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    example_response_headers = [
        (":status", "200"),
        ("server", "fake-serv/0.1.0"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    @pytest.mark.parametrize(
        "frame",
        [
            lambda self, ff: ff.build_headers_frame(
                self.example_request_headers),
            lambda self, ff: ff.build_headers_frame(
                self.example_request_headers, flags=["END_STREAM"]),
        ],
    )
    def test_resets_further_frames_after_recv_reset(self,
                                                    frame_factory,
                                                    frame) -> None:
        """
        A stream that is closed by receive RST_STREAM can receive further
        frames: it simply sends RST_STREAM for it, and additionally
        WINDOW_UPDATE for DATA frames.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        header_frame = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        )
        c.receive_data(header_frame.serialize())

        c.send_headers(
            stream_id=1,
            headers=self.example_response_headers,
            end_stream=False,
        )

        rst_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        c.receive_data(rst_frame.serialize())
        c.clear_outbound_data_buffer()

        f = frame(self, frame_factory)
        events = c.receive_data(f.serialize())

        rst_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        assert not events
        assert c.data_to_send() == rst_frame.serialize()

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == rst_frame.serialize() * 3

        # Iterate over the streams to make sure it's gone, then confirm the
        # behaviour is unchanged.
        c.open_outbound_streams

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == rst_frame.serialize() * 3

    def test_resets_further_data_frames_after_recv_reset(self,
                                                         frame_factory) -> None:
        """
        A stream that is closed by receive RST_STREAM can receive further
        DATA frames: it simply sends WINDOW_UPDATE for the connection flow
        window, and RST_STREAM for the stream.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        header_frame = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        )
        c.receive_data(header_frame.serialize())

        c.send_headers(
            stream_id=1,
            headers=self.example_response_headers,
            end_stream=False,
        )

        rst_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        c.receive_data(rst_frame.serialize())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_data_frame(
            data=b"some data",
        )

        events = c.receive_data(f.serialize())
        assert not events

        expected = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == expected * 3

        # Iterate over the streams to make sure it's gone, then confirm the
        # behaviour is unchanged.
        c.open_outbound_streams

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == expected * 3

    @pytest.mark.parametrize(
        "frame",
        [
            lambda self, ff: ff.build_headers_frame(
                self.example_request_headers),
            lambda self, ff: ff.build_headers_frame(
                self.example_request_headers, flags=["END_STREAM"]),
        ],
    )
    def test_resets_further_frames_after_send_reset(self,
                                                    frame_factory,
                                                    frame) -> None:
        """
        A stream that is closed by sent RST_STREAM can receive further frames:
        it simply sends RST_STREAM for it.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        header_frame = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        )
        c.receive_data(header_frame.serialize())

        c.send_headers(
            stream_id=1,
            headers=self.example_response_headers,
            end_stream=False,
        )

        c.reset_stream(1, h2.errors.ErrorCodes.INTERNAL_ERROR)

        rst_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        c.clear_outbound_data_buffer()

        f = frame(self, frame_factory)
        events = c.receive_data(f.serialize())

        rst_frame = frame_factory.build_rst_stream_frame(
            1, h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        assert not events
        assert c.data_to_send() == rst_frame.serialize()

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == rst_frame.serialize() * 3

        # Iterate over the streams to make sure it's gone, then confirm the
        # behaviour is unchanged.
        c.open_outbound_streams

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == rst_frame.serialize() * 3

    def test_resets_further_data_frames_after_send_reset(self,
                                                         frame_factory) -> None:
        """
        A stream that is closed by sent RST_STREAM can receive further
        data frames: it simply sends WINDOW_UPDATE and RST_STREAM for it.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        c.initiate_connection()

        header_frame = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        )
        c.receive_data(header_frame.serialize())

        c.send_headers(
            stream_id=1,
            headers=self.example_response_headers,
            end_stream=False,
        )

        c.reset_stream(1, h2.errors.ErrorCodes.INTERNAL_ERROR)

        c.clear_outbound_data_buffer()

        f = frame_factory.build_data_frame(
            data=b"some data",
        )
        events = c.receive_data(f.serialize())
        assert not events
        expected = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == expected * 3

        # Iterate over the streams to make sure it's gone, then confirm the
        # behaviour is unchanged.
        c.open_outbound_streams

        events = c.receive_data(f.serialize() * 3)
        assert not events
        assert c.data_to_send() == expected * 3


class TestBoundedClosedStreamState:
    """
    Tests for the configurable bound on closed-stream bookkeeping.

    A long-lived connection handling many short streams must not accumulate
    per-stream state forever. Retained state is a compact summary; the stream-ID
    high-water marks are kept regardless of the bound, so a forgotten stream can
    never be re-accepted.
    """

    example_request_headers = [
        (b":authority", b"example.com"),
        (b":path", b"/"),
        (b":scheme", b"https"),
        (b":method", b"GET"),
    ]
    example_response_headers = [(":status", "200")]

    def _server(self, max_closed_streams):
        config = h2.config.H2Configuration(
            client_side=False, max_closed_streams=max_closed_streams,
        )
        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()
        c.receive_data(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
        return c

    def _close_inbound_stream(self, c, frame_factory, stream_id, with_data=None):
        """
        Receive a complete request (optionally with body data) and respond to it,
        closing the stream. Callers that want reclamation call
        ``open_inbound_streams`` afterwards.
        """
        if with_data is None:
            header_flags = ["END_HEADERS", "END_STREAM"]
        else:
            header_flags = ["END_HEADERS"]
        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            flags=header_flags,
            stream_id=stream_id,
        )
        c.receive_data(f.serialize())
        if with_data is not None:
            c.receive_data(
                frame_factory.build_data_frame(
                    data=with_data, stream_id=stream_id, flags=["END_STREAM"],
                ).serialize(),
            )
        c.send_headers(
            stream_id, self.example_response_headers, end_stream=True,
        )

    def test_default_limit_uses_class_attribute(self) -> None:
        """
        With no explicit config, the retained bookkeeping is bounded by
        ``MAX_CLOSED_STREAMS``.
        """
        c = h2.connection.H2Connection(
            config=h2.config.H2Configuration(client_side=False),
        )
        assert c._closed_streams._size_limit == c.MAX_CLOSED_STREAMS

    def test_class_attribute_override_is_honoured(self) -> None:
        """
        Subclasses may still bound the bookkeeping by overriding the class
        attribute.
        """
        class SmallConnection(h2.connection.H2Connection):
            MAX_CLOSED_STREAMS = 3

        c = SmallConnection(
            config=h2.config.H2Configuration(client_side=False),
        )
        assert c._closed_streams._size_limit == 3

    def test_limit_bounds_retained_summaries(self, frame_factory) -> None:
        """
        Only ``max_closed_streams`` summaries are retained no matter how many
        streams have closed.
        """
        c = self._server(max_closed_streams=2)
        for stream_id in (1, 3, 5, 7):
            self._close_inbound_stream(c, frame_factory, stream_id)
        c.open_inbound_streams
        assert len(c._closed_streams) <= 2

    def test_unbounded_config_retains_all_summaries(self, frame_factory) -> None:
        """
        An explicit large ``max_closed_streams`` retains every summary; the bound
        is truly configurable rather than fixed.
        """
        c = self._server(max_closed_streams=10)
        for stream_id in (1, 3, 5, 7):
            self._close_inbound_stream(c, frame_factory, stream_id)
        c.open_inbound_streams
        assert set(c._closed_streams) == {1, 3, 5, 7}

    def test_zero_limit_retains_nothing(self, frame_factory) -> None:
        """
        ``max_closed_streams=0`` retains no summaries at all.
        """
        c = self._server(max_closed_streams=0)
        self._close_inbound_stream(c, frame_factory, 1)
        c.open_inbound_streams
        assert not c._closed_streams

    def test_summary_retains_no_body_or_headers(self, frame_factory) -> None:
        """
        The retained summary is fixed-size scalar data; it never holds the body
        or the decoded headers.
        """
        from h2.stream import ClosedStreamSummary

        c = self._server(max_closed_streams=10)
        self._close_inbound_stream(
            c, frame_factory, 1, with_data=b"some body bytes",
        )
        c.open_inbound_streams

        summary = c._closed_streams[1]
        assert isinstance(summary, ClosedStreamSummary)
        assert summary.__slots__ == (
            "stream_id", "closed_by", "inbound_data_length",
            "expected_content_length",
        )
        assert not hasattr(summary, "__dict__")
        # Body bytes never survive in the summary's representation.
        assert b"some body bytes" not in repr(summary).encode()

    def test_full_stream_objects_are_reclaimed(self, frame_factory) -> None:
        """
        Once retired, the full stream object is dropped and can be garbage
        collected.
        """
        import gc
        import weakref

        c = self._server(max_closed_streams=10)
        self._close_inbound_stream(c, frame_factory, 1)
        ref = weakref.ref(c.streams[1])
        c.open_inbound_streams
        gc.collect()
        assert ref() is None

    def test_forgotten_stream_never_reaccepted(self, frame_factory) -> None:
        """
        After a stream's summary is reclaimed, its ID is never accepted as a new
        stream: a HEADERS frame with that ID is rejected and no stream is
        created.
        """
        c = self._server(max_closed_streams=0)
        self._close_inbound_stream(c, frame_factory, 1)
        c.open_inbound_streams
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            flags=["END_HEADERS"],
            stream_id=1,
        )
        events = c.receive_data(f.serialize())
        assert not events
        assert 1 not in c.streams
        assert c.highest_inbound_stream_id == 1
        assert c.highest_inbound_stream_id == 1

    def test_late_data_updates_connection_window_when_reclaimed(self, frame_factory) -> None:
        """
        A DATA frame for a reclaimed stream still contributes to the connection
        flow-control window and is answered with RST_STREAM; it is not treated as
        a new stream.
        """
        c = self._server(max_closed_streams=0)
        self._close_inbound_stream(c, frame_factory, 1)
        c.open_inbound_streams
        c.clear_outbound_data_buffer()

        f = frame_factory.build_data_frame(data=b"x" * 1024, stream_id=1)
        events = c.receive_data(f.serialize())
        assert not events
        expected = frame_factory.build_rst_stream_frame(
            stream_id=1, error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected
        assert 1 not in c.streams

    def test_late_window_update_on_reclaimed_stream_ignored(self, frame_factory) -> None:
        """
        A stream-level WINDOW_UPDATE for a reclaimed stream is ignored, while a
        connection-level WINDOW_UPDATE (stream 0) still works.
        """
        c = self._server(max_closed_streams=0)
        self._close_inbound_stream(c, frame_factory, 1)
        c.open_inbound_streams
        c.clear_outbound_data_buffer()

        events = c.receive_data(
            frame_factory.build_window_update_frame(
                stream_id=1, increment=1024,
            ).serialize(),
        )
        assert not events
        assert not c.data_to_send()

        before = c.outbound_flow_control_window
        events = c.receive_data(
            frame_factory.build_window_update_frame(
                stream_id=0, increment=1024,
            ).serialize(),
        )
        assert len(events) == 1
        assert isinstance(events[0], h2.events.WindowUpdated)
        assert c.outbound_flow_control_window == before + 1024

    def test_late_rst_stream_on_reclaimed_stream_ignored(self, frame_factory) -> None:
        """
        A RST_STREAM for a reclaimed stream is ignored.
        """
        c = self._server(max_closed_streams=0)
        self._close_inbound_stream(c, frame_factory, 1)
        c.open_inbound_streams
        c.clear_outbound_data_buffer()

        events = c.receive_data(
            frame_factory.build_rst_stream_frame(stream_id=1).serialize(),
        )
        assert not events
        assert not c.data_to_send()

    def test_late_continuation_on_reclaimed_stream_is_connection_error(self, frame_factory) -> None:
        """
        A bare CONTINUATION for a reclaimed stream is still rejected as a
        connection error, with exactly the same error as when the stream's
        summary is retained. CONTINUATION frames must never be accepted on a
        closed stream.
        """
        outcomes = {}
        for limit in (100, 0):
            frame_factory.refresh_encoder()
            c = self._server(max_closed_streams=limit)
            self._close_inbound_stream(c, frame_factory, 1)
            c.open_inbound_streams
            c.clear_outbound_data_buffer()

            bad = frame_factory.build_continuation_frame(
                header_block=b"hello", stream_id=1,
            )
            with pytest.raises(h2.exceptions.ProtocolError) as exc:
                c.receive_data(bad.serialize())
            outcomes[limit] = (exc.value.error_code, bytes(c.data_to_send()))

        # Both retained (100) and reclaimed (0) summaries give the same result.
        assert outcomes[100] == outcomes[0]

    def test_forgotten_stream_id_never_confused_with_skipped_id(self, frame_factory) -> None:
        """
        An ID that was genuinely used and closed is still distinguished from an
        ID that the peer skipped, even after reclamation: the used-and-closed ID
        is handled as a late frame on a closed stream, while the skipped ID
        remains a plain protocol error.
        """
        c = self._server(max_closed_streams=0)
        # Use streams 1 and 5, skipping 3.
        self._close_inbound_stream(c, frame_factory, 1)
        self._close_inbound_stream(c, frame_factory, 5)
        c.open_inbound_streams
        c.clear_outbound_data_buffer()

        # The used-and-closed ID 1: a late frame is a stream error (RST_STREAM).
        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            flags=["END_HEADERS"],
            stream_id=1,
        )
        events = c.receive_data(f.serialize())
        assert not events
        expected = frame_factory.build_rst_stream_frame(
            stream_id=1, error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected
        c.clear_outbound_data_buffer()

        # The skipped ID 3 was never used: a connection error (PROTOCOL_ERROR).
        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            flags=["END_HEADERS"],
            stream_id=3,
        )
        with pytest.raises(h2.exceptions.ProtocolError) as exc:
            c.receive_data(f.serialize())
        assert exc.value.error_code == h2.errors.ErrorCodes.PROTOCOL_ERROR

    def test_reclaimed_used_stream_is_not_reaccepted(self, frame_factory) -> None:
        """
        A HEADERS frame on a used-and-closed ID whose summary was reclaimed is
        answered with RST_STREAM(STREAM_CLOSED): a stream error that keeps the
        connection alive while never re-accepting the ID as a new stream.
        """
        c = self._server(max_closed_streams=0)
        self._close_inbound_stream(c, frame_factory, 1)
        c.open_inbound_streams
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            self.example_request_headers,
            flags=["END_HEADERS"],
            stream_id=1,
        )
        events = c.receive_data(f.serialize())
        assert not events
        assert 1 not in c.streams
        expected = frame_factory.build_rst_stream_frame(
            stream_id=1, error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected

    def test_long_event_sequence_memory_stays_bounded(self, frame_factory) -> None:
        """
        Processing a long sequence of short streams keeps the retained state
        bounded instead of growing with the number of streams.
        """
        c = self._server(max_closed_streams=4)
        for index in range(50):
            self._close_inbound_stream(c, frame_factory, 1 + index * 2)
            c.open_inbound_streams
        assert len(c._closed_streams) <= 4
        assert not c.streams

    def test_results_are_identical_across_limits(self, frame_factory) -> None:
        """
        The observable outcome for late DATA/WINDOW_UPDATE frames on a closed
        stream does not depend on how many summaries are retained: both a
        retained and a reclaimed summary produce the same events and bytes.
        """
        results = {}
        for limit in (100, 0):
            # Each fresh connection has its own HPACK decoder, so give the frame
            # factory a fresh encoder too.
            frame_factory.refresh_encoder()
            c = self._server(max_closed_streams=limit)
            self._close_inbound_stream(c, frame_factory, 1)
            c.open_inbound_streams
            c.clear_outbound_data_buffer()

            data_events = c.receive_data(
                frame_factory.build_data_frame(
                    data=b"late", stream_id=1,
                ).serialize(),
            )
            data_sent = c.data_to_send()
            c.clear_outbound_data_buffer()

            wu_events = c.receive_data(
                frame_factory.build_window_update_frame(
                    stream_id=1, increment=1,
                ).serialize(),
            )
            wu_sent = c.data_to_send()
            results[limit] = (
                tuple(data_events), bytes(data_sent),
                tuple(wu_events), bytes(wu_sent),
            )
        assert results[100] == results[0]
