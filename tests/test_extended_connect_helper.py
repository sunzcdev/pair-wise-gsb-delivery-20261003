"""
Tests for the explicit extended CONNECT (RFC 8441) handshake helper.

These tests drive *real* ``H2Connection`` objects end to end: the helper only
ever observes genuine h2 events and delegates every wire operation to the
connection. No WebSocket frame codec is involved, and no header validation is
bypassed.
"""

from __future__ import annotations

import pytest

import h2.events
from h2.config import H2Configuration
from h2.connect import (
    PROTOCOL_PSEUDO_HEADER,
    ExtendedConnectHelper,
    ExtendedConnectState,
)
from h2.connection import H2Connection
from h2.errors import ErrorCodes
from h2.exceptions import ProtocolError
from h2.settings import SettingCodes

EXTENDED_CONNECT_HEADERS = [
    (":method", "CONNECT"),
    (":protocol", "websocket"),
    (":scheme", "https"),
    (":path", "/chat?room=1"),
    (":authority", "ws.example.com"),
]

EXTENDED_CONNECT_HEADERS_BYTES = [
    (b":method", b"CONNECT"),
    (b":protocol", b"websocket"),
    (b":scheme", b"https"),
    (b":path", b"/chat?room=1"),
    (b":authority", b"ws.example.com"),
]

ORDINARY_CONNECT_HEADERS = [
    (":method", "CONNECT"),
    (":authority", "example.com:443"),
]


def _make_connection(client_side: bool) -> H2Connection:
    return H2Connection(
        H2Configuration(client_side=client_side, header_encoding="utf-8"),
    )


def _exchange(a: H2Connection, b: H2Connection) -> None:
    """Flush whatever a has queued into b."""
    data = a.data_to_send()
    if data:
        b.receive_data(data)


def _negotiated_pair() -> tuple[H2Connection, H2Connection]:
    """
    A client/server pair that has completed the connection preface and a full
    SETTINGS exchange in which the server advertises ENABLE_CONNECT_PROTOCOL=1.
    """
    client = _make_connection(True)
    server = _make_connection(False)
    client.initiate_connection()
    server.initiate_connection()
    server.update_settings({SettingCodes.ENABLE_CONNECT_PROTOCOL: 1})

    _exchange(server, client)
    _exchange(client, server)
    # Flush the SETTINGS acknowledgements as well.
    _exchange(client, server)
    _exchange(server, client)

    assert client.remote_settings.enable_connect_protocol == 1
    assert server.local_settings.enable_connect_protocol == 1
    return client, server


def _open_extended_connect(
    stream_id: int = 1,
) -> tuple[H2Connection, H2Connection, ExtendedConnectHelper, ExtendedConnectHelper]:
    client, server = _negotiated_pair()
    client_helper = ExtendedConnectHelper(client, stream_id)
    server_helper = ExtendedConnectHelper(server, stream_id)

    client_helper.initiate(EXTENDED_CONNECT_HEADERS)
    server_helper.process_events(server.receive_data(client.data_to_send()))

    return client, server, client_helper, server_helper


class TestClassification:
    def test_extended_connect_request_recognised(self) -> None:
        assert ExtendedConnectHelper.is_extended_connect_request(
            EXTENDED_CONNECT_HEADERS,
        )

    def test_extended_connect_request_recognised_bytes(self) -> None:
        assert ExtendedConnectHelper.is_extended_connect_request(
            EXTENDED_CONNECT_HEADERS_BYTES,
        )

    def test_ordinary_connect_is_not_extended(self) -> None:
        assert not ExtendedConnectHelper.is_extended_connect_request(
            ORDINARY_CONNECT_HEADERS,
        )

    def test_non_connect_method_is_not_extended(self) -> None:
        headers = [
            (":method", "GET"),
            (":scheme", "https"),
            (":path", "/"),
            (":authority", "example.com"),
        ]
        assert not ExtendedConnectHelper.is_extended_connect_request(headers)

    def test_protocol_pseudo_header_constant(self) -> None:
        assert PROTOCOL_PSEUDO_HEADER == b":protocol"

    def test_initial_state_is_idle(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        assert helper.state is ExtendedConnectState.IDLE
        assert not helper.tunnel_accepted
        assert not helper.is_reset
        assert helper.reset_by_remote is None
        assert not helper.local_end_stream
        assert not helper.remote_end_stream
        assert not helper.both_ended


class TestCapabilityNegotiation:
    def test_cannot_initiate_before_settings_exchange(self) -> None:
        client = _make_connection(True)
        client.initiate_connection()
        helper = ExtendedConnectHelper(client, 1)
        assert not helper.can_initiate()

    def test_can_initiate_after_server_advertises_setting(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        assert helper.can_initiate(EXTENDED_CONNECT_HEADERS)

    def test_cannot_initiate_when_server_keeps_setting_disabled(self) -> None:
        client = _make_connection(True)
        server = _make_connection(False)
        client.initiate_connection()
        server.initiate_connection()
        _exchange(server, client)
        _exchange(client, server)
        _exchange(client, server)
        _exchange(server, client)

        assert client.remote_settings.enable_connect_protocol == 0
        helper = ExtendedConnectHelper(client, 1)
        assert not helper.can_initiate(EXTENDED_CONNECT_HEADERS)

    def test_initiate_raises_without_negotiated_capability(self) -> None:
        client = _make_connection(True)
        client.initiate_connection()
        client.receive_data(b"\x00\x00\x00\x04\x01\x00\x00\x00\x00")
        helper = ExtendedConnectHelper(client, 1)
        with pytest.raises(ProtocolError):
            helper.initiate(EXTENDED_CONNECT_HEADERS)

    def test_connection_refuses_extended_connect_without_setting(self) -> None:
        # The underlying connection enforces the rule on its own, even without
        # using the helper.
        client = _make_connection(True)
        client.initiate_connection()
        client.receive_data(b"\x00\x00\x00\x04\x01\x00\x00\x00\x00")
        with pytest.raises(ProtocolError):
            client.send_headers(1, EXTENDED_CONNECT_HEADERS)

    def test_ordinary_connect_still_sendable_without_setting(self) -> None:
        client = _make_connection(True)
        client.initiate_connection()
        client.receive_data(b"\x00\x00\x00\x04\x01\x00\x00\x00\x00")
        # Must not raise: ordinary CONNECT is unrelated to the extension.
        client.send_headers(1, ORDINARY_CONNECT_HEADERS, end_stream=True)
        assert client.data_to_send()

    def test_non_connect_request_still_sendable_without_setting(self) -> None:
        client = _make_connection(True)
        client.initiate_connection()
        client.receive_data(b"\x00\x00\x00\x04\x01\x00\x00\x00\x00")
        get_headers = [
            (":method", "GET"),
            (":scheme", "https"),
            (":path", "/"),
            (":authority", "example.com"),
        ]
        client.send_headers(1, get_headers)
        assert client.data_to_send()

    def test_can_initiate_rejects_incomplete_pseudo_header_tuple(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        incomplete = [
            (":method", "CONNECT"),
            (":protocol", "websocket"),
            (":authority", "ws.example.com"),
        ]
        # The capability is negotiated, so the remaining failure is the
        # incomplete pseudo-header tuple which h2's own validation rejects.
        # The helper must not bypass that validation.
        with pytest.raises(ProtocolError):
            helper.can_initiate(incomplete)

    def test_can_initiate_rejects_ordinary_connect_headers(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        assert not helper.can_initiate(ORDINARY_CONNECT_HEADERS)

    def test_can_initiate_rejects_on_server_connection(self) -> None:
        _client, server = _negotiated_pair()
        helper = ExtendedConnectHelper(server, 1)
        assert not helper.can_initiate(EXTENDED_CONNECT_HEADERS)

    def test_cannot_initiate_twice(self) -> None:
        _client, _server, client_helper, _server_helper = _open_extended_connect()
        assert not client_helper.can_initiate(EXTENDED_CONNECT_HEADERS)
        with pytest.raises(ProtocolError):
            client_helper.initiate(EXTENDED_CONNECT_HEADERS)


class TestHandshakeRoundTrip:
    def test_server_observes_pending_request(self) -> None:
        _client, _server, client_helper, server_helper = _open_extended_connect()
        assert client_helper.state is ExtendedConnectState.REQUEST_PENDING
        assert server_helper.state is ExtendedConnectState.REQUEST_PENDING
        assert not server_helper.tunnel_accepted

    def test_successful_2xx_accepts_tunnel(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))

        assert client_helper.state is ExtendedConnectState.TUNNEL_READY
        assert client_helper.tunnel_accepted
        assert server_helper.state is ExtendedConnectState.TUNNEL_READY

    def test_accept_sends_real_response_event(self) -> None:
        client, server, _client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        events = client.receive_data(server.data_to_send())
        response = next(e for e in events if isinstance(e, h2.events.ResponseReceived))
        status = dict(response.headers)[":status"]
        assert status == "200"

    def test_informational_response_does_not_decide_tunnel(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server.send_headers(1, [(":status", "103")])
        client_helper.process_events(client.receive_data(server.data_to_send()))
        assert client_helper.state is ExtendedConnectState.REQUEST_PENDING
        assert not client_helper.tunnel_accepted

        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))
        assert client_helper.tunnel_accepted

    def test_non_2xx_response_rejects_and_keeps_http_body_semantics(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.reject(403)
        client_helper.process_events(client.receive_data(server.data_to_send()))

        assert client_helper.state is ExtendedConnectState.REJECTED
        assert not client_helper.tunnel_accepted
        assert server_helper.state is ExtendedConnectState.REJECTED

        # A rejected stream is still an ordinary HTTP response/body stream:
        # DATA frames continue to flow and are surfaced normally.
        body = b"forbidden body"
        server.send_data(1, body, end_stream=True)
        events = client.receive_data(server.data_to_send())
        client_helper.process_events(events)

        data_event = next(e for e in events if isinstance(e, h2.events.DataReceived))
        assert data_event.data == body
        assert client_helper.remote_end_stream

    def test_reject_rejects_2xx_status(self) -> None:
        _client, _server, _client_helper, server_helper = _open_extended_connect()
        with pytest.raises(ProtocolError):
            server_helper.reject(200)

    def test_server_cannot_accept_without_advertising_setting(self) -> None:
        # Build a pair where the server never enabled the extension, but feed
        # it an extended CONNECT anyway via raw connection calls (the client
        # guard is bypassed by constructing the frame on the server directly).
        client = _make_connection(True)
        server = _make_connection(False)
        client.initiate_connection()
        server.initiate_connection()
        _exchange(server, client)
        _exchange(client, server)
        _exchange(client, server)
        _exchange(server, client)

        # Manually craft an inbound extended CONNECT for the server using the
        # test frame machinery, so the client-side guard is not involved.
        from tests.helpers import FrameFactory  # noqa: PLC0415

        factory = FrameFactory()
        frame = factory.build_headers_frame(
            EXTENDED_CONNECT_HEADERS_BYTES,
            stream_id=1,
        )
        events = server.receive_data(frame.serialize())
        helper = ExtendedConnectHelper(server, 1)
        helper.process_events(events)
        assert helper.state is ExtendedConnectState.REQUEST_PENDING
        with pytest.raises(ProtocolError):
            helper.accept()

    def test_accept_twice_is_invalid(self) -> None:
        _client, _server, _client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        with pytest.raises(ProtocolError):
            server_helper.accept()

    def test_client_helper_methods_rejected_on_server(self) -> None:
        _client, _server, _client_helper, server_helper = _open_extended_connect()
        with pytest.raises(ProtocolError):
            server_helper.initiate(EXTENDED_CONNECT_HEADERS)

    def test_server_helper_methods_rejected_on_client(self) -> None:
        _client, _server, client_helper, _server_helper = _open_extended_connect()
        with pytest.raises(ProtocolError):
            client_helper.accept()
        with pytest.raises(ProtocolError):
            client_helper.reject()


class TestHalfCloseAndReset:
    def test_one_sided_end_stream_does_not_imply_other_side(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))

        # Client finishes sending first.
        client_helper.end_stream()
        server_helper.process_events(server.receive_data(client.data_to_send()))

        assert server_helper.remote_end_stream
        assert not server_helper.local_end_stream
        assert not server_helper.both_ended
        assert not client_helper.remote_end_stream
        assert client_helper.local_end_stream

    def test_both_sides_must_end_independently(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))

        client_helper.end_stream()
        server_helper.process_events(server.receive_data(client.data_to_send()))
        assert not server_helper.both_ended

        server_helper.end_stream()
        client_helper.process_events(client.receive_data(server.data_to_send()))
        assert client_helper.both_ended
        assert server_helper.both_ended
        # Graceful half-closes do not reset the stream.
        assert not client_helper.is_reset
        assert not server_helper.is_reset

    def test_end_stream_in_data_frame_is_observed(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))

        client_helper.send_data(b"bye", end_stream=True)
        events = server.receive_data(client.data_to_send())
        server_helper.process_events(events)

        assert any(isinstance(e, h2.events.DataReceived) for e in events)
        assert server_helper.remote_end_stream
        assert not server_helper.local_end_stream

    def test_request_end_stream_marks_remote_half_on_server(self) -> None:
        client, server = _negotiated_pair()
        client_helper = ExtendedConnectHelper(client, 1)
        server_helper = ExtendedConnectHelper(server, 1)

        client_helper.initiate(EXTENDED_CONNECT_HEADERS, end_stream=True)
        assert client_helper.local_end_stream

        events = server.receive_data(client.data_to_send())
        server_helper.process_events(events)
        request = next(e for e in events if isinstance(e, h2.events.RequestReceived))
        assert request.stream_ended is not None
        assert server_helper.remote_end_stream
        assert not server_helper.local_end_stream

    def test_remote_reset_does_not_imply_local_end(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))

        server.reset_stream(1, ErrorCodes.PROTOCOL_ERROR)
        client_helper.process_events(client.receive_data(server.data_to_send()))

        assert client_helper.is_reset
        assert client_helper.reset_by_remote is True
        assert client_helper.remote_end_stream
        assert not client_helper.local_end_stream

    def test_local_reset_does_not_imply_remote_end(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        client_helper.reset_stream(ErrorCodes.CANCEL)
        server_helper.process_events(server.receive_data(client.data_to_send()))

        # The server observes a remote reset.
        assert server_helper.is_reset
        assert server_helper.reset_by_remote is True
        assert server_helper.remote_end_stream
        assert not server_helper.local_end_stream

        # The client knows it reset locally.
        assert client_helper.is_reset
        assert client_helper.reset_by_remote is False
        assert client_helper.local_end_stream
        assert not client_helper.remote_end_stream

    def test_reset_event_is_real_h2_event(self) -> None:
        client, server, client_helper, _server_helper = _open_extended_connect()
        server.reset_stream(1)
        events = client.receive_data(server.data_to_send())
        reset = next(e for e in events if isinstance(e, h2.events.StreamReset))
        assert reset.stream_id == 1
        client_helper.process_events(events)
        assert client_helper.state is ExtendedConnectState.RESET


class TestOrdinaryConnectIsolation:
    def test_ordinary_connect_does_not_enter_extended_states(self) -> None:
        client, server = _negotiated_pair()
        server_helper = ExtendedConnectHelper(server, 1)

        client.send_headers(1, ORDINARY_CONNECT_HEADERS, end_stream=True)
        server_helper.process_events(server.receive_data(client.data_to_send()))

        assert server_helper.state is ExtendedConnectState.IDLE
        assert not server_helper.tunnel_accepted
        # The helper must not claim any half-close ownership of a stream it
        # does not recognise as an extended CONNECT.
        assert not server_helper.remote_end_stream
        assert not server_helper.local_end_stream

    def test_ordinary_connect_round_trips_through_connection(self) -> None:
        client, server = _negotiated_pair()
        client.send_headers(1, ORDINARY_CONNECT_HEADERS, end_stream=True)
        events = server.receive_data(client.data_to_send())
        request = next(e for e in events if isinstance(e, h2.events.RequestReceived))
        pseudo = dict(request.headers)
        assert pseudo[":method"] == "CONNECT"
        assert ":protocol" not in pseudo
        assert ":scheme" not in pseudo
        assert ":path" not in pseudo

    def test_events_from_other_streams_are_ignored(self) -> None:
        client, server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        client.send_headers(
            3,
            [
                (":method", "GET"),
                (":scheme", "https"),
                (":path", "/"),
                (":authority", "example.com"),
            ],
        )
        server.receive_data(client.data_to_send())
        # Feeding unrelated events must leave the helper untouched.
        helper.process_events(
            [
                h2.events.ResponseReceived(stream_id=3),
                h2.events.StreamEnded(stream_id=3),
            ],
        )
        assert helper.state is ExtendedConnectState.IDLE


class TestCoverageBranches:
    """Exercise the remaining defensive branches of the helper."""

    def test_repr_contains_stream_and_state(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 7)
        text = repr(helper)
        assert "7" in text
        assert ExtendedConnectState.IDLE.value in text

    def test_can_initiate_false_when_stream_already_used(self) -> None:
        client, _server = _negotiated_pair()
        # Open stream 1 with an unrelated request so the ID is already in use.
        client.send_headers(
            1,
            [
                (":method", "GET"),
                (":scheme", "https"),
                (":path", "/"),
                (":authority", "example.com"),
            ],
        )
        helper = ExtendedConnectHelper(client, 1)
        assert not helper.can_initiate(EXTENDED_CONNECT_HEADERS)

    def test_initiate_raises_when_stream_already_used(self) -> None:
        client, _server = _negotiated_pair()
        client.send_headers(
            1,
            [
                (":method", "GET"),
                (":scheme", "https"),
                (":path", "/"),
                (":authority", "example.com"),
            ],
        )
        helper = ExtendedConnectHelper(client, 1)
        with pytest.raises(ProtocolError):
            helper.initiate(EXTENDED_CONNECT_HEADERS)

    def test_initiate_raises_for_non_extended_headers(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        with pytest.raises(ProtocolError):
            helper.initiate(ORDINARY_CONNECT_HEADERS)

    def test_accept_with_extra_headers(self) -> None:
        client, server, _client_helper, server_helper = _open_extended_connect()
        server_helper.accept([("sec-websocket-protocol", "chat")])
        events = client.receive_data(server.data_to_send())
        response = next(e for e in events if isinstance(e, h2.events.ResponseReceived))
        decoded = dict(response.headers)
        assert decoded[":status"] == "200"
        assert decoded["sec-websocket-protocol"] == "chat"

    def test_reject_with_extra_headers_and_custom_status(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.reject(501, [("content-length", "0")])
        client_helper.process_events(client.receive_data(server.data_to_send()))
        assert client_helper.state is ExtendedConnectState.REJECTED

    def test_accept_invalid_when_idle(self) -> None:
        _client, server = _negotiated_pair()
        helper = ExtendedConnectHelper(server, 1)
        with pytest.raises(ProtocolError):
            helper.accept()

    def test_reject_invalid_when_idle(self) -> None:
        _client, server = _negotiated_pair()
        helper = ExtendedConnectHelper(server, 1)
        with pytest.raises(ProtocolError):
            helper.reject()

    def test_send_data_with_end_stream_marks_local_half(self) -> None:
        _client, _server, client_helper, _server_helper = _open_extended_connect()
        # A plain DATA frame without END_STREAM leaves the local half open.
        client_helper.send_data(b"keeping going")
        assert not client_helper.local_end_stream

        client_helper.send_data(b"last", end_stream=True)
        assert client_helper.local_end_stream

    def test_trailers_mark_remote_half_close(self) -> None:
        client, server, client_helper, server_helper = _open_extended_connect()
        server_helper.accept()
        client_helper.process_events(client.receive_data(server.data_to_send()))

        # Server sends trailers, which always carry END_STREAM.
        server.send_headers(
            1,
            [("x-trace", "abc")],
            end_stream=True,
        )
        client_helper.process_events(client.receive_data(server.data_to_send()))
        assert client_helper.remote_end_stream
        assert not client_helper.local_end_stream

    def test_response_end_stream_marks_remote_half(self) -> None:
        client, server, client_helper, _server_helper = _open_extended_connect()
        # Accept and immediately half-close the server side in the HEADERS.
        server.send_headers(1, [(":status", "200")], end_stream=True)
        client_helper.process_events(client.receive_data(server.data_to_send()))
        assert client_helper.tunnel_accepted
        assert client_helper.remote_end_stream

    def test_reset_observed_before_other_tracked_event(self) -> None:
        # A RST_STREAM arriving when the helper is still IDLE must be ignored
        # because this helper does not own that stream.
        client, server = _negotiated_pair()
        client.send_headers(
            1,
            [
                (":method", "GET"),
                (":scheme", "https"),
                (":path", "/"),
                (":authority", "example.com"),
            ],
        )
        server.receive_data(client.data_to_send())
        client.reset_stream(1)

        helper = ExtendedConnectHelper(server, 1)
        helper.process_events(server.receive_data(client.data_to_send()))
        assert helper.state is ExtendedConnectState.IDLE
        assert not helper.is_reset

    def test_non_numeric_status_treated_as_rejection(self) -> None:
        client, server, client_helper, _server_helper = _open_extended_connect()
        # Build an invalid-but-parseable status header directly on the wire.
        server.send_headers(1, [(":status", "XYZ")])
        client_helper.process_events(client.receive_data(server.data_to_send()))
        # An unparsable status cannot be a 2xx, so the tunnel is not accepted.
        assert client_helper.state is ExtendedConnectState.REJECTED
        assert not client_helper.tunnel_accepted

    def test_can_initiate_without_headers_only_checks_capability(self) -> None:
        client, _server = _negotiated_pair()
        helper = ExtendedConnectHelper(client, 1)
        # No headers supplied: negotiation alone governs the answer.
        assert helper.can_initiate()

    def test_locally_generated_reset_event_marks_local_half(self) -> None:
        # When h2 itself emits a StreamReset(remote_reset=False) and the user
        # feeds that event in, the helper attributes it to the local side.
        _client, _server, client_helper, _server_helper = _open_extended_connect()
        synthetic = h2.events.StreamReset(
            stream_id=1,
            error_code=ErrorCodes.PROTOCOL_ERROR,
            remote_reset=False,
        )
        client_helper.process_events([synthetic])
        assert client_helper.is_reset
        assert client_helper.reset_by_remote is False
        assert client_helper.local_end_stream
        assert not client_helper.remote_end_stream

    def test_response_without_status_header_is_rejection(self) -> None:
        _client, _server, client_helper, _server_helper = _open_extended_connect()
        # A response block lacking :status: the helper must not accept it.
        client_helper._state = ExtendedConnectState.REQUEST_PENDING
        client_helper.process_events(
            [h2.events.ResponseReceived(stream_id=1, headers=[("x-custom", "v")])],
        )
        assert client_helper.state is ExtendedConnectState.REJECTED

