"""
Tests that exercise the HTTP Upgrade functionality of
hyper-h2, ensuring that clients and servers can upgrade their plaintext
HTTP/1.1 connections to HTTP/2.
"""
from __future__ import annotations

import base64
import struct

import pytest

import h2.config
import h2.connection
import h2.errors
import h2.events
import h2.exceptions
import h2.settings
import h2.stream
from h2.utilities import utf8_encode_headers

EXAMPLE_REQUEST_HEADERS = [
    (":authority", "example.com"),
    (":path", "/"),
    (":scheme", "https"),
    (":method", "GET"),
]
EXAMPLE_REQUEST_HEADERS_BYTES = [
    (b":authority", b"example.com"),
    (b":path", b"/"),
    (b":scheme", b"https"),
    (b":method", b"GET"),
]


class TestClientUpgrade:
    """
    Tests of the client-side of the HTTP/2 upgrade dance.
    """

    example_response_headers = [
        (b":status", b"200"),
        (b"server", b"fake-serv/0.1.0"),
    ]

    def test_returns_http2_settings(self, frame_factory) -> None:
        """
        Calling initiate_upgrade_connection returns a base64url encoded
        Settings frame with the settings used by the connection.
        """
        conn = h2.connection.H2Connection()
        data = conn.initiate_upgrade_connection()

        # The base64 encoding must not be padded.
        assert not data.endswith(b"=")

        # However, SETTINGS frames should never need to be padded.
        decoded_frame = base64.urlsafe_b64decode(data)
        expected_frame = frame_factory.build_settings_frame(
            settings=conn.local_settings,
        )
        assert decoded_frame == expected_frame.serialize_body()

    def test_emits_preamble(self, frame_factory) -> None:
        """
        Calling initiate_upgrade_connection emits the connection preamble.
        """
        conn = h2.connection.H2Connection()
        conn.initiate_upgrade_connection()

        data = conn.data_to_send()
        assert data.startswith(frame_factory.preamble())

        data = data[len(frame_factory.preamble()):]
        expected_frame = frame_factory.build_settings_frame(
            settings=conn.local_settings,
        )
        assert data == expected_frame.serialize()

    def test_can_receive_response(self, frame_factory) -> None:
        """
        After upgrading, we can safely receive a response.
        """
        c = h2.connection.H2Connection()
        c.initiate_upgrade_connection()
        c.clear_outbound_data_buffer()

        f1 = frame_factory.build_headers_frame(
            stream_id=1,
            headers=self.example_response_headers,
        )
        f2 = frame_factory.build_data_frame(
            stream_id=1,
            data=b"some data",
            flags=["END_STREAM"],
        )
        events = c.receive_data(f1.serialize() + f2.serialize())
        assert len(events) == 3

        assert isinstance(events[0], h2.events.ResponseReceived)
        assert isinstance(events[1], h2.events.DataReceived)
        assert isinstance(events[2], h2.events.StreamEnded)

        assert events[0].headers == self.example_response_headers
        assert events[1].data == b"some data"
        assert all(e.stream_id == 1 for e in events)

        assert not c.data_to_send()

    @pytest.mark.parametrize("headers", [EXAMPLE_REQUEST_HEADERS, EXAMPLE_REQUEST_HEADERS_BYTES])
    def test_can_receive_pushed_stream(self, frame_factory, headers) -> None:
        """
        After upgrading, we can safely receive a pushed stream.
        """
        c = h2.connection.H2Connection()
        c.initiate_upgrade_connection()
        c.clear_outbound_data_buffer()

        f = frame_factory.build_push_promise_frame(
            stream_id=1,
            promised_stream_id=2,
            headers=headers,
        )
        events = c.receive_data(f.serialize())
        assert len(events) == 1

        assert isinstance(events[0], h2.events.PushedStreamReceived)
        assert events[0].headers == utf8_encode_headers(headers)
        assert events[0].parent_stream_id == 1
        assert events[0].pushed_stream_id == 2

    @pytest.mark.parametrize("headers", [EXAMPLE_REQUEST_HEADERS, EXAMPLE_REQUEST_HEADERS_BYTES])
    def test_cannot_send_headers_stream_1(self, frame_factory, headers) -> None:
        """
        After upgrading, we cannot send headers on stream 1.
        """
        c = h2.connection.H2Connection()
        c.initiate_upgrade_connection()
        c.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_headers(stream_id=1, headers=headers)

    def test_cannot_send_data_stream_1(self, frame_factory) -> None:
        """
        After upgrading, we cannot send data on stream 1.
        """
        c = h2.connection.H2Connection()
        c.initiate_upgrade_connection()
        c.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.ProtocolError):
            c.send_data(stream_id=1, data=b"some data")


class TestServerUpgrade:
    """
    Tests of the server-side of the HTTP/2 upgrade dance.
    """

    example_response_headers = [
        (b":status", b"200"),
        (b"server", b"fake-serv/0.1.0"),
    ]
    server_config = h2.config.H2Configuration(client_side=False)

    def test_returns_nothing(self, frame_factory) -> None:
        """
        Calling initiate_upgrade_connection returns nothing.
        """
        conn = h2.connection.H2Connection(config=self.server_config)
        curl_header = b"AAMAAABkAAQAAP__"
        data = conn.initiate_upgrade_connection(curl_header)
        assert data is None

    def test_emits_preamble(self, frame_factory) -> None:
        """
        Calling initiate_upgrade_connection emits the connection preamble.
        """
        conn = h2.connection.H2Connection(config=self.server_config)
        conn.initiate_upgrade_connection()

        data = conn.data_to_send()
        expected_frame = frame_factory.build_settings_frame(
            settings=conn.local_settings,
        )
        assert data == expected_frame.serialize()

    def test_can_send_response(self, frame_factory) -> None:
        """
        After upgrading, we can safely send a response.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_upgrade_connection()
        c.clear_outbound_data_buffer()

        c.send_headers(stream_id=1, headers=self.example_response_headers)
        c.send_data(stream_id=1, data=b"some data", end_stream=True)

        f1 = frame_factory.build_headers_frame(
            stream_id=1,
            headers=self.example_response_headers,
        )
        f2 = frame_factory.build_data_frame(
            stream_id=1,
            data=b"some data",
            flags=["END_STREAM"],
        )

        expected_data = f1.serialize() + f2.serialize()
        assert c.data_to_send() == expected_data

    @pytest.mark.parametrize("headers", [EXAMPLE_REQUEST_HEADERS, EXAMPLE_REQUEST_HEADERS_BYTES])
    def test_can_push_stream(self, frame_factory, headers) -> None:
        """
        After upgrading, we can safely push a stream.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_upgrade_connection()
        c.clear_outbound_data_buffer()

        c.push_stream(
            stream_id=1,
            promised_stream_id=2,
            request_headers=headers,
        )

        f = frame_factory.build_push_promise_frame(
            stream_id=1,
            promised_stream_id=2,
            headers=headers,
        )
        assert c.data_to_send() == f.serialize()

    @pytest.mark.parametrize("headers", [EXAMPLE_REQUEST_HEADERS, EXAMPLE_REQUEST_HEADERS_BYTES])
    def test_cannot_receive_headers_stream_1(self, frame_factory, headers) -> None:
        """
        After upgrading, we cannot receive headers on stream 1.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_upgrade_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_headers_frame(
            stream_id=1,
            headers=headers,
        )
        c.receive_data(f.serialize())

        expected_frame = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        )
        assert c.data_to_send() == expected_frame.serialize()

    def test_cannot_receive_data_stream_1(self, frame_factory) -> None:
        """
        After upgrading, we cannot receive data on stream 1.
        """
        c = h2.connection.H2Connection(config=self.server_config)
        c.initiate_upgrade_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_data_frame(
            stream_id=1,
            data=b"some data",
        )
        c.receive_data(f.serialize())

        expected = frame_factory.build_rst_stream_frame(
            stream_id=1,
            error_code=h2.errors.ErrorCodes.STREAM_CLOSED,
        ).serialize()
        assert c.data_to_send() == expected

    def test_client_settings_are_applied(self, frame_factory) -> None:
        """
        The settings provided by the client are applied and immediately
        ACK'ed.
        """
        server = h2.connection.H2Connection(config=self.server_config)
        client = h2.connection.H2Connection()

        # As a precaution, let's confirm that the server and client, at the
        # start of the connection, do not agree on their initial settings
        # state.
        assert (
            client.local_settings != server.remote_settings
        )

        # Get the client header data and pass it to the server.
        header_data = client.initiate_upgrade_connection()
        server.initiate_upgrade_connection(header_data)

        # This gets complex, but here we go.
        # RFC 7540 § 3.2.1 says that "explicit acknowledgement" of the settings
        # in the header is "not necessary". That's annoyingly vague, but we
        # interpret that to mean "should not be sent". So to test that this
        # worked we need to test that the server has only sent the preamble,
        # and has not sent a SETTINGS ack, and also that the server has the
        # correct settings.
        expected_frame = frame_factory.build_settings_frame(
            server.local_settings,
        )
        assert server.data_to_send() == expected_frame.serialize()

        assert (
            client.local_settings == server.remote_settings
        )


def _encode_settings_header(entries) -> bytes:
    """
    Build an unpadded base64url ``HTTP2-Settings`` value from (id, value)
    pairs, bypassing the client so malformed payloads can be constructed.
    """
    body = b"".join(struct.pack(">HI", int(identifier), value) for identifier, value in entries)
    return base64.urlsafe_b64encode(body).rstrip(b"=")


def _decode_settings_header(header: bytes) -> dict:
    """
    Decode an unpadded base64url ``HTTP2-Settings`` value into a settings dict.
    """
    body = base64.urlsafe_b64decode(header + b"=" * (-len(header) % 4))
    settings = {}
    for offset in range(0, len(body), 6):
        identifier, value = struct.unpack(">HI", body[offset:offset + 6])
        settings[identifier] = value
    return settings


def _settings_body(settings) -> bytes:
    """
    Serialise a settings mapping to a SETTINGS frame payload.
    """
    return b"".join(
        struct.pack(">HI", int(identifier), value)
        for identifier, value in settings.items()
    )


class TestStrictUpgrade:
    """
    Tests of the strict ``Upgrade: h2c`` entry point.
    """

    server_config = h2.config.H2Configuration(client_side=False)

    def test_client_reports_upgrade_state(self) -> None:
        """
        A strict client upgrade records an inspectable UpgradeState.
        """
        conn = h2.connection.H2Connection()
        header = conn.initiate_upgrade_connection(strict=True)

        state = conn.upgrade_state
        assert state is not None
        assert state.role is h2.connection.UpgradeRole.CLIENT
        assert state.client_side is True
        assert state.stream_id == 1
        assert state.request_complete is True
        assert state.settings_header is None
        assert state.consumed_settings_header is False
        assert state.settings == {
            setting: conn.local_settings[setting]
            for setting in conn.local_settings
        }

        # The returned header must decode to exactly those settings.
        assert _decode_settings_header(header) == {
            int(setting): value for setting, value in state.settings.items()
        }
        assert _settings_body(state.settings) == base64.urlsafe_b64decode(
            header + b"=" * (-len(header) % 4),
        )

    def test_client_preface_carries_empty_settings(self, frame_factory) -> None:
        """
        A strict client advertises its settings once, in HTTP2-Settings, so its
        connection preface carries an empty SETTINGS frame.
        """
        conn = h2.connection.H2Connection()
        conn.initiate_upgrade_connection(strict=True)

        data = conn.data_to_send()
        assert data == (
            frame_factory.preamble() +
            frame_factory.build_settings_frame({}).serialize()
        )

    def test_client_stream_1_half_closed_local(self) -> None:
        """
        The upgraded request belongs to the client, which has finished sending
        it, so stream 1 is half-closed (local).
        """
        conn = h2.connection.H2Connection()
        conn.initiate_upgrade_connection(strict=True)

        assert conn.streams[1].state_machine.state is h2.stream.StreamState.HALF_CLOSED_LOCAL
        assert conn.streams[1].state_machine.client is True
        assert conn.highest_outbound_stream_id == 1

    def test_server_stream_1_half_closed_remote(self) -> None:
        """
        On the server the same request is half-closed (remote).
        """
        client = h2.connection.H2Connection()
        header = client.initiate_upgrade_connection(strict=True)

        server = h2.connection.H2Connection(config=self.server_config)
        assert server.initiate_upgrade_connection(header, strict=True) is None

        assert server.streams[1].state_machine.state is h2.stream.StreamState.HALF_CLOSED_REMOTE
        assert server.streams[1].state_machine.client is False
        assert server.highest_inbound_stream_id == 1

        state = server.upgrade_state
        assert state is not None
        assert state.role is h2.connection.UpgradeRole.SERVER
        assert state.settings_header == header
        assert state.consumed_settings_header is True
        assert client.local_settings == server.remote_settings

    def test_incomplete_request_opens_stream(self) -> None:
        """
        When the original request was not complete, stream 1 stays open in both
        directions so the body can continue.
        """
        client = h2.connection.H2Connection()
        header = client.initiate_upgrade_connection(strict=True, request_complete=False)
        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(header, strict=True, request_complete=False)

        assert client.streams[1].state_machine.state is h2.stream.StreamState.OPEN
        assert server.streams[1].state_machine.state is h2.stream.StreamState.OPEN
        assert client.upgrade_state is not None
        assert client.upgrade_state.request_complete is False

        # The body may now be sent and received on stream 1.
        client.send_data(stream_id=1, data=b"body")
        events = server.receive_data(client.data_to_send())
        data_events = [e for e in events if isinstance(e, h2.events.DataReceived)]
        assert len(data_events) == 1
        assert data_events[0].data == b"body"

    @pytest.mark.parametrize(
        "bad_header",
        [
            b"AAA",                       # not a whole number of bytes
            b"AAAAA",                     # impossible base64url length
            b"AAMAAABkAAQAAP__=",         # padded
            b"!!!!",                      # outside the base64url alphabet
            b"AAMAAABkAAQAAP",            # truncated trailing setting
        ],
    )
    def test_rejects_bad_encoding(self, bad_header) -> None:
        """
        A malformed HTTP2-Settings value is rejected before the connection is
        touched, so the caller can keep using HTTP/1.1.
        """
        server = h2.connection.H2Connection(config=self.server_config)
        with pytest.raises(h2.exceptions.InvalidUpgradeSettingsError):
            server.initiate_upgrade_connection(bad_header, strict=True)

        assert server.upgrade_state is None
        assert server.state_machine.state is h2.connection.ConnectionState.IDLE
        assert server.streams == {}
        assert server.data_to_send() == b""

    def test_rejects_duplicate_settings(self) -> None:
        """
        A setting listed twice has no unambiguous value, so it is rejected.
        """
        header = _encode_settings_header([
            (h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS, 100),
            (h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS, 200),
        ])
        server = h2.connection.H2Connection(config=self.server_config)
        with pytest.raises(h2.exceptions.InvalidUpgradeSettingsError, match="more than once"):
            server.initiate_upgrade_connection(header, strict=True)
        assert server.upgrade_state is None
        assert server.state_machine.state is h2.connection.ConnectionState.IDLE

    def test_unknown_setting_is_carried_through(self) -> None:
        """
        A setting this implementation does not know is preserved as an integer,
        just as it would be in a SETTINGS frame, rather than rejected.
        """
        header = _encode_settings_header([(0xF0, 1)])
        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(header, strict=True)

        assert server.upgrade_state is not None
        assert server.upgrade_state.settings == {0xF0: 1}
        assert server.remote_settings[0xF0] == 1

    @pytest.mark.parametrize(
        ("entries", "error_code"),
        [
            ((h2.settings.SettingCodes.ENABLE_PUSH, 2), h2.errors.ErrorCodes.PROTOCOL_ERROR),
            ((h2.settings.SettingCodes.INITIAL_WINDOW_SIZE, 2**31), h2.errors.ErrorCodes.FLOW_CONTROL_ERROR),
            ((h2.settings.SettingCodes.MAX_FRAME_SIZE, 100), h2.errors.ErrorCodes.PROTOCOL_ERROR),
        ],
    )
    def test_rejects_invalid_setting_values_by_role(self, entries, error_code) -> None:
        """
        Upgrade settings are validated against the role of the peer that sent
        them: a value a client may not advertise is rejected with its protocol
        error code.
        """
        header = _encode_settings_header([entries])
        server = h2.connection.H2Connection(config=self.server_config)
        with pytest.raises(h2.exceptions.InvalidUpgradeSettingsError) as exc_info:
            server.initiate_upgrade_connection(header, strict=True)

        assert exc_info.value.error_code == error_code
        assert server.upgrade_state is None
        assert server.state_machine.state is h2.connection.ConnectionState.IDLE

    def test_upgrade_settings_are_committed(self) -> None:
        """
        The validated settings take effect on the connection and on stream 1,
        which is created after they are applied.
        """
        header = _encode_settings_header([
            (h2.settings.SettingCodes.INITIAL_WINDOW_SIZE, 100000),
            (h2.settings.SettingCodes.MAX_FRAME_SIZE, 32768),
            (h2.settings.SettingCodes.MAX_CONCURRENT_STREAMS, 7),
        ])
        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(header, strict=True)

        assert server.remote_settings.initial_window_size == 100000
        assert server.remote_settings.max_frame_size == 32768
        assert server.remote_settings.max_concurrent_streams == 7
        assert server.max_outbound_frame_size == 32768
        assert server.streams[1].outbound_flow_control_window == 100000
        assert server.streams[1].max_outbound_frame_size == 32768

    def test_rejects_non_bytes_header(self) -> None:
        """
        The header field value must be a byte-like object.
        """
        server = h2.connection.H2Connection(config=self.server_config)
        with pytest.raises(h2.exceptions.InvalidUpgradeSettingsError, match="must be bytes"):
            server.initiate_upgrade_connection("AA", strict=True)  # type: ignore[arg-type]
        assert server.upgrade_state is None

    def test_accepts_bytearray_header(self) -> None:
        """
        A bytearray header value is accepted and remembered.
        """
        client = h2.connection.H2Connection()
        header = client.initiate_upgrade_connection(strict=True)

        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(bytearray(header), strict=True)

        assert server.upgrade_state is not None
        assert server.upgrade_state.settings_header == header
        assert server.upgrade_state.consumed_settings_header is True
        assert server.upgrade_state.client_side is False
        assert client.local_settings == server.remote_settings

    def test_empty_header_is_accepted(self) -> None:
        """
        An empty HTTP2-Settings value carries no settings; the defaults stand.
        """
        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(b"", strict=True)

        assert server.upgrade_state is not None
        assert server.upgrade_state.settings == {}
        assert server.upgrade_state.settings_header == b""

    def test_role_must_match_connection(self) -> None:
        """
        A declared role that contradicts the connection's configuration is
        refused rather than silently ignored.
        """
        client = h2.connection.H2Connection()
        with pytest.raises(ValueError, match="Declared role"):
            client.initiate_upgrade_connection(strict=True, role=h2.connection.UpgradeRole.SERVER)

        server = h2.connection.H2Connection(config=self.server_config)
        with pytest.raises(ValueError, match="Declared role"):
            server.initiate_upgrade_connection(b"", strict=True, role=h2.connection.UpgradeRole.CLIENT)

        explicit = h2.connection.H2Connection()
        explicit.initiate_upgrade_connection(strict=True, role=h2.connection.UpgradeRole.CLIENT)
        assert explicit.upgrade_state is not None
        assert explicit.upgrade_state.role is h2.connection.UpgradeRole.CLIENT

    def test_server_requires_settings_header(self) -> None:
        """
        A strict server upgrade needs the client's HTTP2-Settings value.
        """
        server = h2.connection.H2Connection(config=self.server_config)
        with pytest.raises(ValueError, match="must be given"):
            server.initiate_upgrade_connection(strict=True)

    def test_client_rejects_settings_header(self) -> None:
        """
        A client generates HTTP2-Settings itself and must not be handed one.
        """
        client = h2.connection.H2Connection()
        with pytest.raises(ValueError, match="generates HTTP2-Settings"):
            client.initiate_upgrade_connection(b"AA", strict=True)

    def test_role_arguments_require_strict(self) -> None:
        """
        The strict-only keyword arguments are refused on the legacy path.
        """
        client = h2.connection.H2Connection()
        with pytest.raises(ValueError, match="strict=True"):
            client.initiate_upgrade_connection(role=h2.connection.UpgradeRole.CLIENT)

        client = h2.connection.H2Connection()
        with pytest.raises(ValueError, match="strict=True"):
            client.initiate_upgrade_connection(request_complete=False)

    def test_second_upgrade_is_refused(self) -> None:
        """
        A connection can only be established once.
        """
        client = h2.connection.H2Connection()
        client.initiate_upgrade_connection(strict=True)
        with pytest.raises(ValueError, match="already been initialised"):
            client.initiate_upgrade_connection(strict=True)

    def test_strict_after_initiate_connection_is_refused(self, frame_factory) -> None:
        """
        A connection that already emitted a prior-knowledge preface cannot also
        be upgraded.
        """
        client = h2.connection.H2Connection()
        client.initiate_connection()
        preface = client.data_to_send()

        with pytest.raises(ValueError, match="initiate_connection"):
            client.initiate_upgrade_connection(strict=True)

        # Nothing was appended by the refused upgrade.
        assert client.data_to_send() == b""
        assert preface == frame_factory.preamble() + frame_factory.build_settings_frame(
            client.local_settings,
        ).serialize()

    def test_preface_does_not_reapply_settings(self, frame_factory) -> None:
        """
        The client preface carries no settings, and even a restatement of the
        same values must not move flow-control windows twice.
        """
        client = h2.connection.H2Connection()
        header = client.initiate_upgrade_connection(strict=True)

        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(header, strict=True)
        server.clear_outbound_data_buffer()

        window_before = server.streams[1].outbound_flow_control_window

        # Feed the client's actual preface: preamble plus its empty SETTINGS.
        events = server.receive_data(client.data_to_send())
        assert all(
            isinstance(e, h2.events.RemoteSettingsChanged) and not e.changed_settings
            for e in events
        )
        assert server.streams[1].outbound_flow_control_window == window_before

        # A same-valued restatement must also not double-apply.
        frame = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: window_before},
        )
        server.receive_data(frame.serialize())
        assert server.streams[1].outbound_flow_control_window == window_before

    def test_preface_with_new_values_is_applied_once(self, frame_factory) -> None:
        """
        A genuine change in the peer's preface is still committed, once.
        """
        client = h2.connection.H2Connection()
        header = client.initiate_upgrade_connection(strict=True)

        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(header, strict=True)
        server.clear_outbound_data_buffer()

        events = server.receive_data(client.data_to_send())
        assert isinstance(events[0], h2.events.RemoteSettingsChanged)

        frame = frame_factory.build_settings_frame(
            settings={h2.settings.SettingCodes.INITIAL_WINDOW_SIZE: 123456},
        )
        events = server.receive_data(frame.serialize())

        assert server.remote_settings.initial_window_size == 123456
        assert server.streams[1].outbound_flow_control_window == 123456
        settings_events = [e for e in events if isinstance(e, h2.events.RemoteSettingsChanged)]
        assert len(settings_events) == 1
        assert settings_events[0].changed_settings[h2.settings.SettingCodes.INITIAL_WINDOW_SIZE].new_value == 123456

    def test_round_trip_over_bytes(self) -> None:
        """
        Both ends can exchange bytes after a strict upgrade: the request is on
        stream 1 and the server can answer it.
        """
        client = h2.connection.H2Connection()
        header = client.initiate_upgrade_connection(strict=True)

        server = h2.connection.H2Connection(config=self.server_config)
        server.initiate_upgrade_connection(header, strict=True)

        # Exchange the two prefaces and acknowledgements.
        client.receive_data(server.data_to_send())
        server.receive_data(client.data_to_send())
        client.clear_outbound_data_buffer()
        server.clear_outbound_data_buffer()

        server.send_headers(
            stream_id=1,
            headers=[(b":status", b"200")],
            end_stream=True,
        )
        events = client.receive_data(server.data_to_send())
        response = [e for e in events if isinstance(e, h2.events.ResponseReceived)]
        assert len(response) == 1
        assert response[0].stream_id == 1
        assert response[0].headers == [(b":status", b"200")]
        assert any(isinstance(e, h2.events.StreamEnded) for e in events)


class TestPriorKnowledgeUnaffected:
    """
    Strict upgrades must not change how a prior-knowledge connection behaves.
    """

    def test_prior_knowledge_has_no_upgrade_state(self, frame_factory) -> None:
        conn = h2.connection.H2Connection()
        conn.initiate_connection()

        assert conn.upgrade_state is None
        assert conn.data_to_send() == (
            frame_factory.preamble() +
            frame_factory.build_settings_frame(conn.local_settings).serialize()
        )
