"""
test_flow_control
~~~~~~~~~~~~~~~~~

Tests of the flow control management in h2
"""
from __future__ import annotations

import pytest
from hypothesis import HealthCheck, given, settings
from hypothesis.strategies import integers
from hyperframe.frame import Frame, WindowUpdateFrame

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.exceptions
import h2.settings


class TestFlowControl:
    """
    Tests of the flow control management in the connection objects.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    DEFAULT_FLOW_WINDOW = 65535

    def test_flow_control_initializes_properly(self) -> None:
        """
        The flow control window for a stream should initially be the default
        flow control value.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        assert c.local_flow_control_window(1) == self.DEFAULT_FLOW_WINDOW
        assert c.remote_flow_control_window(1) == self.DEFAULT_FLOW_WINDOW

    def test_flow_control_decreases_with_sent_data(self) -> None:
        """
        When data is sent on a stream, the flow control window should drop.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)
        c.send_data(1, b"some data")

        remaining_length = self.DEFAULT_FLOW_WINDOW - len(b"some data")
        assert (c.local_flow_control_window(1) == remaining_length)

    @pytest.mark.parametrize("pad_length", [5, 0])
    def test_flow_control_decreases_with_sent_data_with_padding(self,
                                                                pad_length) -> None:
        """
        When padded data is sent on a stream, the flow control window drops
        by the length of the padding plus 1 for the 1-byte padding length
        field.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        c.send_data(1, b"some data", pad_length=pad_length)
        remaining_length = (
            self.DEFAULT_FLOW_WINDOW - len(b"some data") - pad_length - 1
        )
        assert c.local_flow_control_window(1) == remaining_length

    def test_flow_control_decreases_with_received_data(self, frame_factory) -> None:
        """
        When data is received on a stream, the remote flow control window
        should drop.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        f1 = frame_factory.build_headers_frame(self.example_request_headers)
        f2 = frame_factory.build_data_frame(b"some data")

        c.receive_data(f1.serialize() + f2.serialize())

        remaining_length = self.DEFAULT_FLOW_WINDOW - len(b"some data")
        assert (c.remote_flow_control_window(1) == remaining_length)

    def test_flow_control_decreases_with_padded_data(self, frame_factory) -> None:
        """
        When padded data is received on a stream, the remote flow control
        window drops by an amount that includes the padding.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        f1 = frame_factory.build_headers_frame(self.example_request_headers)
        f2 = frame_factory.build_data_frame(b"some data", padding_len=10)

        c.receive_data(f1.serialize() + f2.serialize())

        remaining_length = (
            self.DEFAULT_FLOW_WINDOW - len(b"some data") - 10 - 1
        )
        assert (c.remote_flow_control_window(1) == remaining_length)
        
    def test_can_send_zero_bytes_when_window_negative(self) -> None:
        """
        Sending 0 bytes when the flow control window is negative should not
        raise a FlowControlError, allowing empty END_STREAM frames to be sent.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)
        c.outbound_flow_control_window = -100
        c._get_stream_by_id(1).outbound_flow_control_window = -100
        c.send_data(1, b"", end_stream=True)

    def test_flow_control_is_limited_by_connection(self) -> None:
        """
        The flow control window is limited by the flow control of the
        connection.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)
        c.send_data(1, b"some data")
        c.send_headers(3, self.example_request_headers)

        remaining_length = self.DEFAULT_FLOW_WINDOW - len(b"some data")
        assert (c.local_flow_control_window(3) == remaining_length)

    def test_remote_flow_control_is_limited_by_connection(self, frame_factory) -> None:
        """
        The remote flow control window is limited by the flow control of the
        connection.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())
        f1 = frame_factory.build_headers_frame(self.example_request_headers)
        f2 = frame_factory.build_data_frame(b"some data")
        f3 = frame_factory.build_headers_frame(
            self.example_request_headers,
            stream_id=3,
        )
        c.receive_data(f1.serialize() + f2.serialize() + f3.serialize())

        remaining_length = self.DEFAULT_FLOW_WINDOW - len(b"some data")
        assert (c.remote_flow_control_window(3) == remaining_length)

    def test_cannot_send_more_data_than_window(self) -> None:
        """
        Sending more data than the remaining flow control window raises a
        FlowControlError.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)
        c.outbound_flow_control_window = 5

        with pytest.raises(h2.exceptions.FlowControlError):
            c.send_data(1, b"some data")

    def test_increasing_connection_window_allows_sending(self, frame_factory) -> None:
        """
        Confirm that sending a WindowUpdate frame on the connection frees
        up space for further frames.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)
        c.outbound_flow_control_window = 5

        with pytest.raises(h2.exceptions.FlowControlError):
            c.send_data(1, b"some data")

        f = frame_factory.build_window_update_frame(
            stream_id=0,
            increment=5,
        )
        c.receive_data(f.serialize())

        c.clear_outbound_data_buffer()
        c.send_data(1, b"some data")
        assert c.data_to_send()

    def test_increasing_stream_window_allows_sending(self, frame_factory) -> None:
        """
        Confirm that sending a WindowUpdate frame on the connection frees
        up space for further frames.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)
        c._get_stream_by_id(1).outbound_flow_control_window = 5

        with pytest.raises(h2.exceptions.FlowControlError):
            c.send_data(1, b"some data")

        f = frame_factory.build_window_update_frame(
            stream_id=1,
            increment=5,
        )
        c.receive_data(f.serialize())

        c.clear_outbound_data_buffer()
        c.send_data(1, b"some data")
        assert c.data_to_send()

    def test_flow_control_shrinks_in_response_to_settings(self, frame_factory) -> None:
        """
        Acknowledging SETTINGS_INITIAL_WINDOW_SIZE shrinks the flow control
        window.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        assert c.local_flow_control_window(1) == 65535

        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 1280},
        )
        c.receive_data(f.serialize())

        assert c.local_flow_control_window(1) == 1280

    def test_flow_control_grows_in_response_to_settings(self, frame_factory) -> None:
        """
        Acknowledging SETTINGS_INITIAL_WINDOW_SIZE grows the flow control
        window.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        # Greatly increase the connection flow control window.
        f = frame_factory.build_window_update_frame(
            stream_id=0, increment=128000,
        )
        c.receive_data(f.serialize())

        # The stream flow control window is the bottleneck here.
        assert c.local_flow_control_window(1) == 65535

        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 128000},
        )
        c.receive_data(f.serialize())

        # The stream window is still the bottleneck, but larger now.
        assert c.local_flow_control_window(1) == 128000

    def test_flow_control_settings_blocked_by_conn_window(self, frame_factory) -> None:
        """
        Changing SETTINGS_INITIAL_WINDOW_SIZE does not affect the effective
        flow control window if the connection window isn't changed.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        assert c.local_flow_control_window(1) == 65535

        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 128000},
        )
        c.receive_data(f.serialize())

        assert c.local_flow_control_window(1) == 65535

    def test_new_streams_have_flow_control_per_settings(self, frame_factory) -> None:
        """
        After a SETTINGS_INITIAL_WINDOW_SIZE change is received, new streams
        have appropriate new flow control windows.
        """
        c = h2.connection.H2Connection()

        f = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 128000},
        )
        c.receive_data(f.serialize())

        # Greatly increase the connection flow control window.
        f = frame_factory.build_window_update_frame(
            stream_id=0, increment=128000,
        )
        c.receive_data(f.serialize())

        c.send_headers(1, self.example_request_headers)
        assert c.local_flow_control_window(1) == 128000

    def test_window_update_no_stream(self, frame_factory) -> None:
        """
        WindowUpdate frames received without streams fire an appropriate
        WindowUpdated event.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())

        f = frame_factory.build_window_update_frame(
            stream_id=0,
            increment=5,
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]

        assert isinstance(event, h2.events.WindowUpdated)
        assert event.stream_id == 0
        assert event.delta == 5

    def test_window_update_with_stream(self, frame_factory) -> None:
        """
        WindowUpdate frames received with streams fire an appropriate
        WindowUpdated event.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())

        f1 = frame_factory.build_headers_frame(self.example_request_headers)
        f2 = frame_factory.build_window_update_frame(
            stream_id=1,
            increment=66,
        )
        data = b"".join(f.serialize() for f in [f1, f2])
        events = c.receive_data(data)

        assert len(events) == 2
        event = events[1]

        assert isinstance(event, h2.events.WindowUpdated)
        assert event.stream_id == 1
        assert event.delta == 66

    def test_we_can_increment_stream_flow_control(self, frame_factory) -> None:
        """
        It is possible for the user to increase the flow control window for
        streams.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers, end_stream=True)
        c.clear_outbound_data_buffer()

        expected_frame = frame_factory.build_window_update_frame(
            stream_id=1,
            increment=5,
        )

        events = c.increment_flow_control_window(increment=5, stream_id=1)
        assert not events
        assert c.data_to_send() == expected_frame.serialize()

    def test_we_can_increment_connection_flow_control(self, frame_factory) -> None:
        """
        It is possible for the user to increase the flow control window for
        the entire connection.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers, end_stream=True)
        c.clear_outbound_data_buffer()

        expected_frame = frame_factory.build_window_update_frame(
            stream_id=0,
            increment=5,
        )

        events = c.increment_flow_control_window(increment=5)
        assert not events
        assert c.data_to_send() == expected_frame.serialize()

    def test_we_enforce_our_flow_control_window(self, frame_factory) -> None:
        """
        The user can set a low flow control window, which leads to connection
        teardown if violated.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.receive_data(frame_factory.preamble())

        # Change the flow control window to 80 bytes.
        c.update_settings(
            {h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 80},
        )
        f = frame_factory.build_settings_frame({}, ack=True)
        c.receive_data(f.serialize())

        # Receive a new stream.
        f = frame_factory.build_headers_frame(self.example_request_headers)
        c.receive_data(f.serialize())

        # Attempt to violate the flow control window.
        c.clear_outbound_data_buffer()
        f = frame_factory.build_data_frame(b"\x01" * 100)

        with pytest.raises(h2.exceptions.FlowControlError):
            c.receive_data(f.serialize())

        # Verify we tear down appropriately.
        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=1,
            error_code=h2.errors.ErrorCodes.FLOW_CONTROL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_shrink_remote_flow_control_settings(self, frame_factory) -> None:
        """
        The remote peer acknowledging our SETTINGS_INITIAL_WINDOW_SIZE shrinks
        the flow control window.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        assert c.remote_flow_control_window(1) == 65535

        c.update_settings({h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 1280})

        f = frame_factory.build_settings_frame({}, ack=True)
        c.receive_data(f.serialize())

        assert c.remote_flow_control_window(1) == 1280

    def test_grow_remote_flow_control_settings(self, frame_factory) -> None:
        """
        The remote peer acknowledging our SETTINGS_INITIAL_WINDOW_SIZE grows
        the flow control window.
        """
        c = h2.connection.H2Connection()
        c.send_headers(1, self.example_request_headers)

        # Increase the connection flow control window greatly.
        c.increment_flow_control_window(increment=128000)

        assert c.remote_flow_control_window(1) == 65535

        c.update_settings(
            {h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 128000},
        )
        f = frame_factory.build_settings_frame({}, ack=True)
        c.receive_data(f.serialize())

        assert c.remote_flow_control_window(1) == 128000

    def test_new_streams_have_remote_flow_control(self, frame_factory) -> None:
        """
        After a SETTINGS_INITIAL_WINDOW_SIZE change is acknowledged by the
        remote peer, new streams have appropriate new flow control windows.
        """
        c = h2.connection.H2Connection()

        c.update_settings(
            {h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 128000},
        )
        f = frame_factory.build_settings_frame({}, ack=True)
        c.receive_data(f.serialize())

        # Increase the connection flow control window greatly.
        c.increment_flow_control_window(increment=128000)

        c.send_headers(1, self.example_request_headers)
        assert c.remote_flow_control_window(1) == 128000

    @pytest.mark.parametrize(
        "increment", [0, -15, 2**31],
    )
    def test_reject_bad_attempts_to_increment_flow_control(self, increment) -> None:
        """
        Attempting to increment a flow control increment outside the valid
        range causes a ValueError to be raised.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers, end_stream=True)
        c.clear_outbound_data_buffer()

        # Fails both on and off streams.
        with pytest.raises(ValueError):
            c.increment_flow_control_window(increment=increment, stream_id=1)

        with pytest.raises(ValueError):
            c.increment_flow_control_window(increment=increment)

    @pytest.mark.parametrize("stream_id", [0, 1])
    def test_reject_bad_remote_increments(self, frame_factory, stream_id) -> None:
        """
        Remote peers attempting to increment flow control outside the valid
        range cause connection errors of type PROTOCOL_ERROR.
        """
        # The only number that can be encoded in a WINDOW_UPDATE frame but
        # isn't valid is 0.
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers, end_stream=True)
        c.clear_outbound_data_buffer()

        f = frame_factory.build_window_update_frame(
            stream_id=stream_id, increment=0,
        )

        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(f.serialize())

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0,
            error_code=h2.errors.ErrorCodes.PROTOCOL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_increasing_connection_window_too_far(self, frame_factory) -> None:
        """
        Attempts by the remote peer to increase the connection flow control
        window beyond 2**31 - 1 are rejected.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        increment = 2**31 - c.outbound_flow_control_window

        f = frame_factory.build_window_update_frame(
            stream_id=0, increment=increment,
        )

        with pytest.raises(h2.exceptions.FlowControlError):
            c.receive_data(f.serialize())

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0,
            error_code=h2.errors.ErrorCodes.FLOW_CONTROL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_increasing_stream_window_too_far(self, frame_factory) -> None:
        """
        Attempts by the remote peer to increase the stream flow control window
        beyond 2**31 - 1 are rejected.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers)
        c.clear_outbound_data_buffer()

        increment = 2**31 - c.outbound_flow_control_window

        f = frame_factory.build_window_update_frame(
            stream_id=1, increment=increment,
        )

        events = c.receive_data(f.serialize())
        assert len(events) == 1

        event = events[0]
        assert isinstance(event, h2.events.StreamReset)
        assert event.stream_id == 1
        assert event.error_code == h2.errors.ErrorCodes.FLOW_CONTROL_ERROR
        assert not event.remote_reset

        expected_frame = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.FLOW_CONTROL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_overlarge_conn_window_settings(self, frame_factory) -> None:
        """
        SETTINGS frames cannot change the size of the connection flow control
        window.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()

        # Go one byte smaller than the limit.
        increment = 2**31 - 1 - c.outbound_flow_control_window

        f = frame_factory.build_window_update_frame(
            stream_id=0, increment=increment,
        )
        c.receive_data(f.serialize())

        # Receive an increment to the initial window size.
        f = frame_factory.build_settings_frame(
            settings={
                h2.settings.SettingCodes.INITIAL_WINDOW_SIZE:
                    self.DEFAULT_FLOW_WINDOW + 1,
            },
        )
        c.clear_outbound_data_buffer()

        # No error is encountered.
        events = c.receive_data(f.serialize())
        assert len(events) == 1
        assert isinstance(events[0], h2.events.RemoteSettingsChanged)

        expected_frame = frame_factory.build_settings_frame(
            settings={},
            ack=True,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_overlarge_stream_window_settings(self, frame_factory) -> None:
        """
        Remote attempts to create overlarge stream windows via SETTINGS frames
        are rejected.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers)

        # Go one byte smaller than the limit.
        increment = 2**31 - 1 - c.outbound_flow_control_window

        f = frame_factory.build_window_update_frame(
            stream_id=1, increment=increment,
        )
        c.receive_data(f.serialize())

        # Receive an increment to the initial window size.
        f = frame_factory.build_settings_frame(
            settings={
                h2.settings.SettingCodes.INITIAL_WINDOW_SIZE:
                    self.DEFAULT_FLOW_WINDOW + 1,
            },
        )
        c.clear_outbound_data_buffer()
        with pytest.raises(h2.exceptions.FlowControlError):
            c.receive_data(f.serialize())

        expected_frame = frame_factory.build_goaway_frame(
            last_stream_id=0,
            error_code=h2.errors.ErrorCodes.FLOW_CONTROL_ERROR,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_reject_local_overlarge_increase_connection_window(self) -> None:
        """
        Local attempts to increase the connection window too far are rejected.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()

        increment = 2**31 - c.inbound_flow_control_window

        with pytest.raises(h2.exceptions.FlowControlError):
            c.increment_flow_control_window(increment=increment)

    def test_reject_local_overlarge_increase_stream_window(self) -> None:
        """
        Local attempts to increase the connection window too far are rejected.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers)

        increment = 2**31 - c.inbound_flow_control_window

        with pytest.raises(h2.exceptions.FlowControlError):
            c.increment_flow_control_window(increment=increment, stream_id=1)

    def test_send_update_on_closed_streams(self, frame_factory) -> None:
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.send_headers(1, self.example_request_headers)
        c.reset_stream(1)

        c.clear_outbound_data_buffer()
        c.open_outbound_streams
        c.open_inbound_streams

        f = frame_factory.build_data_frame(b"some data"*1500)
        events = c.receive_data(f.serialize()*3)
        assert not events

        expected = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize() * 2 + frame_factory.build_window_update_frame(
            stream_id=0,
            increment=40500,
        ).serialize() + frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected

        f = frame_factory.build_data_frame(b"")
        events = c.receive_data(f.serialize())
        assert not events

        expected = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected


class TestAutomaticFlowControl:
    """
    Tests for the automatic flow control logic.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    DEFAULT_FLOW_WINDOW = 65535

    def _setup_connection_and_send_headers(self, frame_factory):
        """
        Setup a server-side H2Connection and send a headers frame, and then
        clear the outbound data buffer. Also increase the maximum frame size.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())

        c.update_settings(
            {h2.settings.SettingCodes.MAX_FRAME_SIZE: self.DEFAULT_FLOW_WINDOW},
        )
        settings_frame = frame_factory.build_settings_frame(
            settings={}, ack=True,
        )
        c.receive_data(settings_frame.serialize())
        c.clear_outbound_data_buffer()

        headers_frame = frame_factory.build_headers_frame(
            headers=self.example_request_headers,
        )
        c.receive_data(headers_frame.serialize())
        c.clear_outbound_data_buffer()
        return c

    @given(stream_id=integers(max_value=0))
    @settings(suppress_health_check=[HealthCheck.function_scoped_fixture])
    def test_must_acknowledge_for_stream(self, frame_factory, stream_id) -> None:
        """
        Flow control acknowledgements must be done on a stream ID that is
        greater than zero.
        """
        # We need to refresh the encoder because hypothesis has a problem with
        # integrating with py.test, meaning that we use the same frame factory
        # for all tests.
        # See https://github.com/HypothesisWorks/hypothesis-python/issues/377
        frame_factory.refresh_encoder()

        # Create a connection in a state that might actually accept
        # data acknowledgement.
        c = self._setup_connection_and_send_headers(frame_factory)
        data_frame = frame_factory.build_data_frame(
            b"some data", flags=["END_STREAM"],
        )
        c.receive_data(data_frame.serialize())

        with pytest.raises(ValueError):
            c.acknowledge_received_data(
                acknowledged_size=5, stream_id=stream_id,
            )

    @given(size=integers(max_value=-1))
    @settings(suppress_health_check=[HealthCheck.function_scoped_fixture])
    def test_cannot_acknowledge_less_than_zero(self, frame_factory, size) -> None:
        """
        The user must acknowledge at least 0 bytes.
        """
        # We need to refresh the encoder because hypothesis has a problem with
        # integrating with py.test, meaning that we use the same frame factory
        # for all tests.
        # See https://github.com/HypothesisWorks/hypothesis-python/issues/377
        frame_factory.refresh_encoder()

        # Create a connection in a state that might actually accept
        # data acknowledgement.
        c = self._setup_connection_and_send_headers(frame_factory)
        data_frame = frame_factory.build_data_frame(
            b"some data", flags=["END_STREAM"],
        )
        c.receive_data(data_frame.serialize())

        with pytest.raises(ValueError):
            c.acknowledge_received_data(acknowledged_size=size, stream_id=1)

    def test_acknowledging_small_chunks_does_nothing(self, frame_factory) -> None:
        """
        When a small amount of data is received and acknowledged, no window
        update is emitted.
        """
        c = self._setup_connection_and_send_headers(frame_factory)

        data_frame = frame_factory.build_data_frame(
            b"some data", flags=["END_STREAM"],
        )
        data_event = c.receive_data(data_frame.serialize())[0]

        c.acknowledge_received_data(
            data_event.flow_controlled_length, stream_id=1,
        )

        assert not c.data_to_send()

    def test_acknowledging_no_data_does_nothing(self, frame_factory) -> None:
        """
        If a user accidentally acknowledges no data, nothing happens.
        """
        c = self._setup_connection_and_send_headers(frame_factory)

        # Send an empty data frame, just to give the user impetus to ack the
        # data.
        data_frame = frame_factory.build_data_frame(b"")
        c.receive_data(data_frame.serialize())

        c.acknowledge_received_data(0, stream_id=1)
        assert not c.data_to_send()

    @pytest.mark.parametrize("force_cleanup", [True, False])
    def test_acknowledging_data_on_closed_stream(self,
                                                 frame_factory,
                                                 force_cleanup) -> None:
        """
        When a stream is terminated with RST_STREAM the unacknowledged data is
        settled back into the connection flow control window at reset time.
        A late acknowledgement of that data then does nothing: the credit
        must not be added to the connection window a second time, and no
        meaningless stream level WINDOW_UPDATE is emitted for a closed
        stream.
        """
        c = self._setup_connection_and_send_headers(frame_factory)

        data_to_send = b"\x00" * self.DEFAULT_FLOW_WINDOW
        data_frame = frame_factory.build_data_frame(data_to_send)
        c.receive_data(data_frame.serialize())

        rst_frame = frame_factory.build_rst_stream_frame(
            stream_id=1,
        )
        c.receive_data(rst_frame.serialize())

        # The whole stream was still unacknowledged, so the reset settles all
        # 65535 bytes back into the connection window. The window had been
        # fully consumed, so a connection level WINDOW_UPDATE is emitted
        # immediately. A received RST_STREAM does not itself trigger a reply
        # RST_STREAM.
        expected_reset = frame_factory.build_window_update_frame(
            stream_id=0, increment=self.DEFAULT_FLOW_WINDOW,
        ).serialize()
        assert c.data_to_send() == expected_reset
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

        c.clear_outbound_data_buffer()

        if force_cleanup:
            # Check how many streams are open to force the old one to be
            # cleaned up.
            assert c.open_outbound_streams == 0

        # A late acknowledgement must be a complete no-op: the bytes were
        # returned already, so crediting them again would be a double refund.
        c.acknowledge_received_data(2048, stream_id=1)
        assert not c.data_to_send()
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

        # Repeated late acknowledgements are equally inert.
        c.acknowledge_received_data(2048, stream_id=1)
        assert not c.data_to_send()

    def test_acknowledging_streams_we_never_saw(self, frame_factory) -> None:
        """
        If the user acknowledges a stream ID we've never seen, that raises a
        NoSuchStreamError.
        """
        c = self._setup_connection_and_send_headers(frame_factory)
        c.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.NoSuchStreamError):
            c.acknowledge_received_data(2048, stream_id=101)

    @given(integers(min_value=1025, max_value=DEFAULT_FLOW_WINDOW))
    @settings(suppress_health_check=[HealthCheck.function_scoped_fixture])
    def test_acknowledging_1024_bytes_when_empty_increments(self,
                                                            frame_factory,
                                                            increment) -> None:
        """
        If the flow control window is empty and we acknowledge 1024 bytes or
        more, we will emit a WINDOW_UPDATE frame just to move the connection
        forward.
        """
        # We need to refresh the encoder because hypothesis has a problem with
        # integrating with py.test, meaning that we use the same frame factory
        # for all tests.
        # See https://github.com/HypothesisWorks/hypothesis-python/issues/377
        frame_factory.refresh_encoder()

        c = self._setup_connection_and_send_headers(frame_factory)

        data_to_send = b"\x00" * self.DEFAULT_FLOW_WINDOW
        data_frame = frame_factory.build_data_frame(data_to_send)
        c.receive_data(data_frame.serialize())

        c.acknowledge_received_data(increment, stream_id=1)

        first_expected = frame_factory.build_window_update_frame(
            stream_id=0, increment=increment,
        )
        second_expected = frame_factory.build_window_update_frame(
            stream_id=1, increment=increment,
        )
        expected_data = b"".join(
            [first_expected.serialize(), second_expected.serialize()],
        )
        assert c.data_to_send() == expected_data

    # This test needs to use a lower cap, because otherwise the algo will
    # increment the stream window anyway.
    @given(integers(min_value=1025, max_value=(DEFAULT_FLOW_WINDOW // 4) - 1))
    @settings(suppress_health_check=[HealthCheck.function_scoped_fixture])
    def test_connection_only_empty(self, frame_factory, increment) -> None:
        """
        If the connection flow control window is empty, but the stream flow
        control windows aren't, and 1024 bytes or more are acknowledged by the
        user, we increment the connection window only.
        """
        # We need to refresh the encoder because hypothesis has a problem with
        # integrating with py.test, meaning that we use the same frame factory
        # for all tests.
        # See https://github.com/HypothesisWorks/hypothesis-python/issues/377
        frame_factory.refresh_encoder()

        # Here we'll use 4 streams. Set them up.
        c = self._setup_connection_and_send_headers(frame_factory)

        for stream_id in [3, 5, 7]:
            f = frame_factory.build_headers_frame(
                headers=self.example_request_headers, stream_id=stream_id,
            )
            c.receive_data(f.serialize())

        # Now we send 1/4 of the connection window per stream. Annoyingly,
        # that's an odd number, so we need to round the last frame up.
        data_to_send = b"\x00" * (self.DEFAULT_FLOW_WINDOW // 4)
        for stream_id in [1, 3, 5]:
            f = frame_factory.build_data_frame(
                data_to_send, stream_id=stream_id,
            )
            c.receive_data(f.serialize())

        data_to_send = b"\x00" * c.remote_flow_control_window(7)
        data_frame = frame_factory.build_data_frame(data_to_send, stream_id=7)
        c.receive_data(data_frame.serialize())

        # Ok, now the actual test.
        c.acknowledge_received_data(increment, stream_id=1)

        expected_data = frame_factory.build_window_update_frame(
            stream_id=0, increment=increment,
        ).serialize()
        assert c.data_to_send() == expected_data

    @given(integers(min_value=1025, max_value=DEFAULT_FLOW_WINDOW))
    @settings(suppress_health_check=[HealthCheck.function_scoped_fixture])
    def test_mixing_update_forms(self, frame_factory, increment) -> None:
        """
        If the user mixes acknowledging data with manually incrementing windows,
        we still keep track of what's going on.
        """
        # We need to refresh the encoder because hypothesis has a problem with
        # integrating with py.test, meaning that we use the same frame factory
        # for all tests.
        # See https://github.com/HypothesisWorks/hypothesis-python/issues/377
        frame_factory.refresh_encoder()

        # Empty the flow control window.
        c = self._setup_connection_and_send_headers(frame_factory)
        data_to_send = b"\x00" * self.DEFAULT_FLOW_WINDOW
        data_frame = frame_factory.build_data_frame(data_to_send)
        c.receive_data(data_frame.serialize())

        # Manually increment the connection flow control window back to fully
        # open, but leave the stream window closed.
        c.increment_flow_control_window(
            stream_id=None, increment=self.DEFAULT_FLOW_WINDOW,
        )
        c.clear_outbound_data_buffer()

        # Now, acknowledge the receipt of that data. This should cause the
        # stream window to be widened, but not the connection window, because
        # it is already open.
        c.acknowledge_received_data(increment, stream_id=1)

        # We expect to see one window update frame only, for the stream.
        expected_data = frame_factory.build_window_update_frame(
            stream_id=1, increment=increment,
        ).serialize()
        assert c.data_to_send() == expected_data


class TestResetFlowControlSettlement:
    """
    Tests that terminating a stream with RST_STREAM settles its inbound flow
    control accounting correctly: unconsumed connection-level credit is
    returned exactly once, bytes already acknowledged are not returned again,
    no stream level WINDOW_UPDATE is emitted for a closed stream, and
    abandoning one stream can never permanently stall another stream.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    DEFAULT_FLOW_WINDOW = 65535

    def _server_with_stream(self, frame_factory, stream_id=1):
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        # Allow full-window DATA frames in a single frame.
        c.update_settings(
            {h2.settings.SettingCodes.MAX_FRAME_SIZE: self.DEFAULT_FLOW_WINDOW},
        )
        c.receive_data(frame_factory.build_settings_frame({}, ack=True).serialize())
        c.clear_outbound_data_buffer()
        headers = frame_factory.build_headers_frame(
            self.example_request_headers, stream_id=stream_id,
        )
        c.receive_data(headers.serialize())
        c.clear_outbound_data_buffer()
        return c

    def test_local_reset_returns_unacknowledged_connection_credit(
        self, frame_factory,
    ) -> None:
        """
        Resetting a stream locally hands the unacknowledged bytes back to the
        connection flow control window (via a stream-0 WINDOW_UPDATE), never
        to a stream level WINDOW_UPDATE.
        """
        c = self._server_with_stream(frame_factory)

        data = b"\x00" * self.DEFAULT_FLOW_WINDOW
        c.receive_data(
            frame_factory.build_data_frame(data, stream_id=1).serialize(),
        )
        assert c.inbound_flow_control_window == 0
        c.clear_outbound_data_buffer()

        c.reset_stream(1)

        # A connection-level WINDOW_UPDATE followed by the RST_STREAM, and
        # nothing on stream 1.
        expected = (
            frame_factory.build_window_update_frame(
                stream_id=0, increment=self.DEFAULT_FLOW_WINDOW,
            ).serialize()
            + frame_factory.build_rst_stream_frame(stream_id=1).serialize()
        )
        assert c.data_to_send() == expected
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_local_reset_only_returns_unacknowledged_balance(
        self, frame_factory,
    ) -> None:
        """
        Bytes the application already acknowledged must not be returned a
        second time when the stream is reset: only the outstanding balance is
        settled.
        """
        c = self._server_with_stream(frame_factory)

        chunk = self.DEFAULT_FLOW_WINDOW // 2
        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * chunk, stream_id=1,
            ).serialize(),
        )
        # Acknowledge the whole chunk. The connection window was half full, so
        # this reaches the half-window threshold and emits a connection level
        # WINDOW_UPDATE for the full chunk.
        c.acknowledge_received_data(chunk, stream_id=1)
        c.clear_outbound_data_buffer()

        # A second, unacknowledged chunk fills the window again.
        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * chunk, stream_id=1,
            ).serialize(),
        )
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW - chunk
        c.clear_outbound_data_buffer()

        c.reset_stream(1)

        # Only the second, unacknowledged chunk is settled: the already
        # acknowledged first chunk is never returned again. The connection
        # window happened to be below the half-window threshold, so the
        # coalescing strategy emits a connection WINDOW_UPDATE for exactly the
        # outstanding balance, followed by the RST_STREAM.
        expected = (
            frame_factory.build_window_update_frame(
                stream_id=0, increment=chunk,
            ).serialize()
            + frame_factory.build_rst_stream_frame(stream_id=1).serialize()
        )
        assert c.data_to_send() == expected
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

        # The credit cannot be refunded twice: a late acknowledgement of the
        # reset stream is a no-op.
        c.acknowledge_received_data(chunk, stream_id=1)
        assert not c.data_to_send()
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_late_acknowledgement_after_local_reset_is_a_noop(
        self, frame_factory,
    ) -> None:
        """
        An acknowledgement arriving after a local reset must not add any
        credit: the bytes were already settled, so a second refund would
        corrupt the connection window.
        """
        c = self._server_with_stream(frame_factory)

        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * self.DEFAULT_FLOW_WINDOW, stream_id=1,
            ).serialize(),
        )
        c.reset_stream(1)
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW
        c.clear_outbound_data_buffer()

        c.acknowledge_received_data(4096, stream_id=1)
        c.acknowledge_received_data(self.DEFAULT_FLOW_WINDOW, stream_id=1)

        assert not c.data_to_send()
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_late_acknowledgement_after_remote_reset_is_a_noop(
        self, frame_factory,
    ) -> None:
        """
        The same protection applies when the peer resets the stream, including
        after the stream object has been cleaned up.
        """
        c = self._server_with_stream(frame_factory)

        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * self.DEFAULT_FLOW_WINDOW, stream_id=1,
            ).serialize(),
        )
        c.receive_data(
            frame_factory.build_rst_stream_frame(stream_id=1).serialize(),
        )
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW
        c.clear_outbound_data_buffer()

        # Force cleanup of the stream object.
        assert c.open_inbound_streams == 0
        assert 1 not in c.streams

        c.acknowledge_received_data(4096, stream_id=1)
        assert not c.data_to_send()
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_cancelling_one_stream_does_not_stall_another(
        self, frame_factory,
    ) -> None:
        """
        With two concurrent streams, filling the connection window on one
        stream and then resetting it must restore the full connection window
        so the other stream can keep receiving data.
        """
        c = self._server_with_stream(frame_factory, stream_id=1)
        c.receive_data(
            frame_factory.build_headers_frame(
                self.example_request_headers, stream_id=3,
            ).serialize(),
        )
        c.clear_outbound_data_buffer()

        # Exhaust the whole connection window on stream 1.
        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * self.DEFAULT_FLOW_WINDOW, stream_id=1,
            ).serialize(),
        )
        assert c.remote_flow_control_window(3) == 0
        c.clear_outbound_data_buffer()

        # The application gives up on stream 1.
        c.reset_stream(1)

        # The connection credit is fully restored.
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW
        assert c.remote_flow_control_window(3) == self.DEFAULT_FLOW_WINDOW

        # Stream 3 is therefore still able to receive a full window of data,
        # which would have raised FlowControlError before the fix.
        c.receive_data(
            frame_factory.build_data_frame(
                b"\x01" * self.DEFAULT_FLOW_WINDOW, stream_id=3,
            ).serialize(),
        )
        assert c.remote_flow_control_window(3) == 0

    def test_padding_counts_in_reset_settlement(self, frame_factory) -> None:
        """
        Padding is flow controlled: the settlement uses
        ``flow_controlled_length`` (data + padding + padding-length field),
        not the length of the application data.
        """
        c = self._server_with_stream(frame_factory)

        payload = b"some data"
        padding = 10
        flow_controlled = len(payload) + padding + 1
        frame = frame_factory.build_data_frame(
            payload, stream_id=1, padding_len=padding,
        )
        events = c.receive_data(frame.serialize())
        data_event = next(
            e for e in events if isinstance(e, h2.events.DataReceived)
        )
        assert data_event.flow_controlled_length == flow_controlled
        c.clear_outbound_data_buffer()

        c.reset_stream(1)

        # The amount is below the automatic WINDOW_UPDATE thresholds, so the
        # credit is coalesced and only the RST_STREAM goes on the wire, but
        # the inbound window bookkeeping reflects the full flow controlled
        # length.
        assert c.data_to_send() == (
            frame_factory.build_rst_stream_frame(stream_id=1).serialize()
        )
        assert c.inbound_flow_control_window == (
            self.DEFAULT_FLOW_WINDOW - flow_controlled
        )

    def test_reset_with_no_data_emits_no_window_update(
        self, frame_factory,
    ) -> None:
        """
        Resetting a stream that never received flow controlled data has
        nothing to settle: only the RST_STREAM frame is emitted.
        """
        c = self._server_with_stream(frame_factory)
        c.clear_outbound_data_buffer()

        c.reset_stream(1)

        assert c.data_to_send() == (
            frame_factory.build_rst_stream_frame(stream_id=1).serialize()
        )
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_acknowledgement_on_cleanly_closed_stream_still_opens_window(
        self, frame_factory,
    ) -> None:
        """
        A late acknowledgement on a stream closed cleanly with END_STREAM
        (not RST_STREAM) still returns connection level credit as before: the
        no-op behaviour is strictly scoped to reset streams.
        """
        c = self._server_with_stream(frame_factory)

        data_frame = frame_factory.build_data_frame(
            b"\x00" * self.DEFAULT_FLOW_WINDOW,
            stream_id=1,
            flags=["END_STREAM"],
        )
        c.receive_data(data_frame.serialize())
        c.clear_outbound_data_buffer()

        c.acknowledge_received_data(self.DEFAULT_FLOW_WINDOW, stream_id=1)

        # A stream closed cleanly with END_STREAM is not a reset: the normal
        # acknowledgement path runs, opening both the connection window and
        # (while the stream object is still present) the stream window.
        expected = (
            frame_factory.build_window_update_frame(
                stream_id=0, increment=self.DEFAULT_FLOW_WINDOW,
            ).serialize()
            + frame_factory.build_window_update_frame(
                stream_id=1, increment=self.DEFAULT_FLOW_WINDOW,
            ).serialize()
        )
        assert c.data_to_send() == expected
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_remote_reset_emits_no_stream_level_window_update(
        self, frame_factory,
    ) -> None:
        """
        Handling a received RST_STREAM never emits a stream level
        WINDOW_UPDATE: only stream 0 may be updated, and only when there is
        unacknowledged credit to settle.
        """
        c = self._server_with_stream(frame_factory)

        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * self.DEFAULT_FLOW_WINDOW, stream_id=1,
            ).serialize(),
        )
        c.clear_outbound_data_buffer()

        c.receive_data(
            frame_factory.build_rst_stream_frame(stream_id=1).serialize(),
        )

        wire = c.data_to_send()
        assert wire == frame_factory.build_window_update_frame(
            stream_id=0, increment=self.DEFAULT_FLOW_WINDOW,
        ).serialize()

    def test_flow_control_error_auto_reset_settles_inbound_credit(
        self, frame_factory,
    ) -> None:
        """
        When an overlarge stream WINDOW_UPDATE forces h2 to reset the stream
        locally, inbound flow controlled bytes already held by the dying
        stream are still returned to the connection window.
        """
        c = self._server_with_stream(frame_factory)

        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * self.DEFAULT_FLOW_WINDOW, stream_id=1,
            ).serialize(),
        )
        assert c.inbound_flow_control_window == 0
        c.clear_outbound_data_buffer()

        # An increment that would push the outbound stream window past the
        # legal maximum triggers a local FLOW_CONTROL_ERROR reset.
        stream = c._get_stream_by_id(1)
        increment = 2**31 - stream.outbound_flow_control_window
        bad_update = frame_factory.build_window_update_frame(
            stream_id=1, increment=increment,
        )
        events = c.receive_data(bad_update.serialize())

        assert len(events) == 1
        assert isinstance(events[0], h2.events.StreamReset)

        # The unacknowledged inbound data is returned to the connection
        # window and a RST_STREAM is emitted, but no stream level
        # WINDOW_UPDATE.
        expected = (
            frame_factory.build_window_update_frame(
                stream_id=0, increment=self.DEFAULT_FLOW_WINDOW,
            ).serialize()
            + frame_factory.build_rst_stream_frame(
                stream_id=1,
                error_code=h2.errors.ErrorCodes.FLOW_CONTROL_ERROR,
            ).serialize()
        )
        assert c.data_to_send() == expected
        assert c.inbound_flow_control_window == self.DEFAULT_FLOW_WINDOW

    def test_auto_reset_on_data_after_end_stream_settles_prior_data(
        self, frame_factory,
    ) -> None:
        """
        If the state machine forcibly resets a stream because DATA arrives
        after END_STREAM, data that had legitimately been delivered on the
        stream earlier but never acknowledged is still settled back into the
        connection window.
        """
        c = self._server_with_stream(frame_factory)

        # A legitimate DATA frame (ending the stream) that consumes most, but
        # not all, of the connection window and is never acknowledged. Leaving
        # head room ensures the stray frame below is still inside the
        # connection window so it reaches the stream state machine.
        first_chunk = self.DEFAULT_FLOW_WINDOW - 64
        c.receive_data(
            frame_factory.build_data_frame(
                b"\x00" * first_chunk,
                stream_id=1,
                flags=["END_STREAM"],
            ).serialize(),
        )
        c.clear_outbound_data_buffer()

        # More DATA on the now half-closed (remote) stream forces a local
        # reset.
        stray = frame_factory.build_data_frame(b"extra", stream_id=1)
        events = c.receive_data(stray.serialize())

        assert any(
            isinstance(e, h2.events.StreamReset) for e in events
        )
        total_owed = first_chunk + len(b"extra")
        conn_manager = c._inbound_flow_control_window_manager

        wire = c.data_to_send()
        # A stream-0 WINDOW_UPDATE may be emitted, but never a WINDOW_UPDATE
        # on the closed stream 1.
        stream_zero_updates = 0
        stream_one_updates = 0
        while wire:
            f, fl = Frame.parse_frame_header(memoryview(wire[:9]))
            f.parse_body(memoryview(wire[9:9 + fl]))
            if isinstance(f, WindowUpdateFrame):
                if f.stream_id == 0:
                    stream_zero_updates += f.window_increment
                elif f.stream_id == 1:
                    stream_one_updates += 1
            wire = wire[9 + fl:]
        assert stream_one_updates == 0

        # No credit is lost and none is double counted. Before the reset the
        # connection window had been reduced by ``total_owed`` bytes. Every one
        # of those bytes is either already reflected in an enlarged connection
        # window or held in the connection window manager's coalescing counter
        # to be emitted by a later acknowledgement.
        window_growth = c.inbound_flow_control_window - (
            self.DEFAULT_FLOW_WINDOW - total_owed
        )
        assert window_growth + conn_manager._bytes_processed == total_owed
        # The emitted stream-0 updates exactly match the window growth.
        assert stream_zero_updates == window_growth

        # A late acknowledgement of the reset stream is a no-op, proving the
        # credit cannot be returned a second time.
        before = c.inbound_flow_control_window
        c.acknowledge_received_data(first_chunk, stream_id=1)
        assert c.inbound_flow_control_window == before
        assert not c.data_to_send()
