"""
Tests for the explicit batch receive entry point,
``H2Connection.receive_data_batch``.
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.exceptions


class TestReceiveDataBatch:
    """
    A batch of received data that contains a corrupt frame must still
    yield the events and outbound control frames produced by the valid
    frames that preceded the error, without rolling back or replaying
    any frame.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    # A DATA frame whose padding is invalid: the pad length byte claims
    # more padding than the frame body contains.
    invalid_padding_data_frame = (
        b"\x00\x00\x05\x00\x0b\x00\x00\x00\x01\x06\x54\x65\x73\x74"
    )

    def make_server(self, frame_factory):
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()
        return c

    def test_valid_prefix_then_corrupt_frame(self, frame_factory) -> None:
        """
        Two valid frames followed by a corrupt one: the events and the
        control frames of the valid prefix are returned, along with the
        error, its offset, and the terminal state.
        """
        c = self.make_server(frame_factory)

        settings = frame_factory.build_settings_frame({}).serialize()
        headers = frame_factory.build_headers_frame(
            self.example_request_headers,
        ).serialize()
        valid_prefix = settings + headers
        batch = valid_prefix + self.invalid_padding_data_frame

        result = c.receive_data_batch(batch)

        assert not result.ok
        assert isinstance(result.error, h2.exceptions.ProtocolError)
        assert result.accepted == len(valid_prefix)
        assert result.error_offset == len(valid_prefix)
        assert result.terminated

        # The valid prefix produced its events: the settings change and
        # the received request.
        assert len(result.events) == 2
        assert isinstance(result.events[0], h2.events.RemoteSettingsChanged)
        assert isinstance(result.events[1], h2.events.RequestReceived)

        # The control frames for the valid prefix (the settings
        # acknowledgement) and the terminal GOAWAY are both available.
        expected = (
            frame_factory.build_settings_frame({}, ack=True).serialize()
            + frame_factory.build_goaway_frame(
                last_stream_id=1, error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
            ).serialize()
        )
        assert result.data_to_send == expected

    def test_events_from_valid_frames_are_not_replayed(self, frame_factory) -> None:
        """
        After a fatal error, further input is not parsed as though it
        belonged to a new connection, and previously delivered events are
        not emitted a second time.
        """
        c = self.make_server(frame_factory)

        headers = frame_factory.build_headers_frame(
            self.example_request_headers,
        ).serialize()
        first = c.receive_data_batch(headers + self.invalid_padding_data_frame)
        assert first.terminated
        assert len(first.events) == 1

        # The bytes after the error have been discarded, and new input on
        # the dead connection does not produce events or a fresh GOAWAY.
        more_headers = frame_factory.build_headers_frame(
            self.example_request_headers, stream_id=3,
        ).serialize()
        second = c.receive_data_batch(more_headers)

        assert second.events == []
        assert second.error is not None
        assert second.terminated
        assert second.accepted == 0
        assert second.data_to_send == b""

    def test_incomplete_trailing_frame_produces_no_events(self, frame_factory) -> None:
        """
        A half-received frame generates no events and is not counted in
        the accepted prefix; its bytes remain buffered for the next call.
        """
        c = self.make_server(frame_factory)

        headers = frame_factory.build_headers_frame(
            self.example_request_headers,
        ).serialize()
        split = 5

        first = c.receive_data_batch(headers[:split])
        assert first.ok
        assert first.events == []
        assert first.accepted == 0
        assert first.error_offset is None
        assert not first.terminated
        assert first.data_to_send == b""

        second = c.receive_data_batch(headers[split:])
        assert second.ok
        assert len(second.events) == 1
        assert isinstance(second.events[0], h2.events.RequestReceived)
        # Offsets are relative to this call's input, not to the total
        # buffered data.
        assert second.accepted == len(headers) - split

    def test_error_offset_is_relative_to_current_input(self, frame_factory) -> None:
        """
        When a frame was started in an earlier call, the error offset
        still refers to the input of the call that saw the error.
        """
        c = self.make_server(frame_factory)

        headers = frame_factory.build_headers_frame(
            self.example_request_headers,
        ).serialize()
        split = 4
        assert c.receive_data_batch(headers[:split]).ok

        rest = headers[split:] + self.invalid_padding_data_frame
        result = c.receive_data_batch(rest)

        assert isinstance(result.error, h2.exceptions.ProtocolError)
        assert result.accepted == len(headers) - split
        assert result.error_offset == len(headers) - split
        assert len(result.events) == 1
        assert isinstance(result.events[0], h2.events.RequestReceived)

    def test_frame_with_missing_data(self, frame_factory) -> None:
        """
        A frame that is too short for its type reports the specific
        error, and the GOAWAY carries the matching error code.
        """
        c = self.make_server(frame_factory)

        ping = frame_factory.build_ping_frame(b"\x00" * 8).serialize()
        invalid_window_update_frame = (
            b"\x00\x00\x03\x08\x00\x00\x00\x00\x00\x00\x00\x02"
        )
        batch = ping + invalid_window_update_frame

        result = c.receive_data_batch(batch)

        assert isinstance(result.error, h2.exceptions.FrameDataMissingError)
        assert result.accepted == len(ping)
        assert result.error_offset == len(ping)
        assert result.terminated
        assert len(result.events) == 1
        assert isinstance(result.events[0], h2.events.PingReceived)

        expected = (
            frame_factory.build_ping_frame(b"\x00" * 8, flags=["ACK"]).serialize()
            + frame_factory.build_goaway_frame(
                last_stream_id=0,
                error_code=h2.errors.ErrorCodes.FRAME_SIZE_ERROR,
            ).serialize()
        )
        assert result.data_to_send == expected

    def test_invalid_preamble(self, frame_factory) -> None:
        """
        A bad client preamble fails at offset zero of the batch.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        result = c.receive_data_batch(b"not a valid preamble at all")

        assert isinstance(result.error, h2.exceptions.ProtocolError)
        assert result.accepted == 0
        assert result.error_offset == 0
        assert result.events == []
        assert result.terminated

    def test_successful_batch(self, frame_factory) -> None:
        """
        A fully valid batch reports success and consumes every byte.
        """
        c = self.make_server(frame_factory)

        headers = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        ).serialize()
        result = c.receive_data_batch(headers)

        assert result.ok
        assert result.error is None
        assert result.error_offset is None
        assert not result.terminated
        assert result.accepted == len(headers)
        assert len(result.events) == 2
        assert isinstance(result.events[0], h2.events.RequestReceived)
        assert isinstance(result.events[1], h2.events.StreamEnded)

    def test_terminated_reflects_closed_connection(self, frame_factory) -> None:
        """
        A batch that closes the connection without error (the peer sent
        GOAWAY) reports the terminal state.
        """
        c = self.make_server(frame_factory)

        goaway = frame_factory.build_goaway_frame(last_stream_id=0).serialize()
        result = c.receive_data_batch(goaway)

        assert result.ok
        assert result.terminated
        assert result.accepted == len(goaway)
        assert any(
            isinstance(e, h2.events.ConnectionTerminated)
            for e in result.events
        )

    def test_memoryview_input(self, frame_factory) -> None:
        """
        Buffers other than bytes are accepted, as with receive_data.
        """
        c = self.make_server(frame_factory)

        headers = frame_factory.build_headers_frame(
            self.example_request_headers, flags=["END_STREAM"],
        ).serialize()
        result = c.receive_data_batch(memoryview(headers))

        assert result.ok
        assert result.accepted == len(headers)
