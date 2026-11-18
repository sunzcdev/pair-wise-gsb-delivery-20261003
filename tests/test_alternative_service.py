"""
Tests for the reviewable RFC 7838 Alternative Service model.

These cover the parsing of the ALTSVC field value and the scope/recording
behaviour h2 attaches to the resulting events. Nothing here should establish a
connection or trust a cross-origin address.
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.events
from h2.alternative_service import (
    AlternativeServiceScope,
    InvalidAlternativeServiceAdvertisement,
    MaxAgeStatus,
    OriginSource,
    parse_alternative_service_field,
    parse_origin,
    service_is_cross_origin,
    validate_advertisement_scope,
)


class TestParseFieldValue:
    """
    Parsing the field value into reviewable objects.
    """

    def test_single_implicit_port_service(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"; ma=60')

        assert not field.errors
        assert len(field.services) == 1

        service = field.services[0]
        assert service.protocol_id == b"h2"
        assert service.host is None
        assert service.port == 8000
        assert service.max_age == 60
        assert service.max_age_status is MaxAgeStatus.VALID
        assert service.raw == b'h2=":8000"; ma=60'
        assert not service.is_duplicate
        assert not service.is_clearing

        assert field.max_age == 60
        assert field.max_age_status is MaxAgeStatus.VALID
        assert field.raw == b'h2=":8000"; ma=60'

    def test_explicit_host_and_port(self) -> None:
        field = parse_alternative_service_field(b'h2="alt.example.net:8443"')

        service = field.services[0]
        assert service.host == b"alt.example.net"
        assert service.port == 8443
        assert service.max_age is None
        assert service.max_age_status is MaxAgeStatus.ABSENT

    def test_ipv6_literal(self) -> None:
        field = parse_alternative_service_field(b'h2="[2001:db8::1]:8443"')

        assert not field.errors
        service = field.services[0]
        assert service.host == b"2001:db8::1"
        assert service.port == 8443

    def test_multiple_services_preserve_order(self) -> None:
        field = parse_alternative_service_field(
            b'h3=":443", h2="alt.example.net:8443"',
        )

        assert [s.protocol_id for s in field.services] == [b"h3", b"h2"]
        assert field.protocol_ids == (b"h3", b"h2")

    def test_unknown_protocol_identifier_is_opaque(self) -> None:
        field = parse_alternative_service_field(
            b'foo-experimental=":9000"; ma=30',
        )

        assert not field.errors
        service = field.services[0]
        assert service.protocol_id == b"foo-experimental"
        assert service.port == 9000
        assert service.max_age == 30
        assert field.protocol_ids == (b"foo-experimental",)

    def test_unknown_parameters_are_preserved(self) -> None:
        field = parse_alternative_service_field(
            b'h2=":8000"; ma=60; foo=bar; "quoted"=baz',
        )

        assert not field.errors
        service = field.services[0]
        assert service.unknown_parameters == (b"foo=bar", b'"quoted"=baz')

    def test_persist_parameter(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"; persist=1')
        assert field.services[0].persist is True

        field = parse_alternative_service_field(b'h2=":8000"; persist=0')
        assert field.services[0].persist is False
        assert field.errors == ("invalid persist parameter: b'persist=0'",)

    def test_quoted_parameter_value_does_not_split_entry(self) -> None:
        field = parse_alternative_service_field(
            b'h2=":8000"; ma=60; note="a,b;c"',
        )

        assert not field.errors
        assert len(field.services) == 1
        assert field.services[0].unknown_parameters == (b'note="a,b;c"',)

    def test_escaped_quote_in_authority(self) -> None:
        field = parse_alternative_service_field(
            b'h2="alt\\"quoted.example:8443"',
        )

        assert not field.errors
        assert field.services[0].host == b'alt"quoted.example'
        assert field.services[0].port == 8443

    @pytest.mark.parametrize("value", [
        b'h2=":8000"',
        b'h2="alt.example:443"',
    ])
    def test_quoted_protocol_identifier(self, value) -> None:
        field = parse_alternative_service_field(b'"h2"=' + value[3:])

        assert not field.errors
        assert field.services[0].protocol_id == b"h2"

    @pytest.mark.parametrize("value", [
        b'h2=":8000"; ',
        b'h2=":8000"; ;ma=1',
    ])
    def test_empty_parameter_is_reported(self, value) -> None:
        field = parse_alternative_service_field(value)

        assert field.services[0].port == 8000
        assert field.errors == ("empty alternative service parameter",)

    def test_empty_entry_is_reported(self) -> None:
        field = parse_alternative_service_field(b' , h2=":8000"')

        assert len(field.services) == 1
        assert field.errors == ("empty alternative service entry",)

    def test_parameter_without_value_is_unknown(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"; upgrade')

        assert not field.errors
        assert field.services[0].unknown_parameters == (b"upgrade",)

    def test_entry_without_equals_is_reported(self) -> None:
        field = parse_alternative_service_field(b"garbage, h2=:8000")

        assert field.errors == (
            "alternative service has no protocol identifier: b'garbage'",
        )
        assert [s.protocol_id for s in field.services] == [b"h2"]

    @pytest.mark.parametrize(("value", "error"), [
        (b'h"2"=":8000"', 'non-token protocol identifier: b\'h"2"\' in alternative service: b\'h"2"=":8000"\''),
        (b'""=":8000"', 'empty protocol identifier in alternative service: b\'""=":8000"\''),
        (b'"h2"x=":8000"', 'unexpected data after quoted identifier: b\'x\' in alternative service: b\'\"h2\"x=\":8000\"\''),
        (b'"h2=":8000"', 'alternative service has no protocol identifier: b\'"h2=":8000"\''),
    ])
    def test_invalid_protocol_identifiers(self, value, error) -> None:
        field = parse_alternative_service_field(value)
        assert field.errors == (error,)

    @pytest.mark.parametrize(("value", "error"), [
        (b'h2=":8000"x', "unexpected data after quoted authority: b'x'"),
        (b'h2=[::1:8000', "unterminated IPv6 literal: b'[::1:8000'"),
        (b'h2="[::1]x"', "unexpected data after host: b'x'"),
        (b'h2="alt.example:443', "unterminated quoted authority: b'\"alt.example:443'"),
        (b'h2=":8000:"', "empty port"),
        (b'h2=": "', "non-numeric port: b' '"),
        (b'h2=":99999"', "port out of range: 99999"),
        (b'h2=""', "missing alternative service authority"),
        (b'h2="alt.example.com"', "missing port"),
    ])
    def test_invalid_authorities(self, value, error) -> None:
        field = parse_alternative_service_field(value)
        assert field.errors == (f"invalid alternative service host in {value!r}: {error}",)

    def test_parameter_with_escaped_quote(self) -> None:
        field = parse_alternative_service_field(
            b'h2=":8000"; "a\\"=b"=c',
        )

        assert not field.errors
        assert field.services[0].unknown_parameters == (b'"a\\"=b"=c',)

    def test_raw_bytes_survive_malformed_input(self) -> None:
        raw = b'h2=":notaport"; ma=abc'
        field = parse_alternative_service_field(raw)

        assert field.raw == raw
        assert field.services[0].raw == raw
        assert field.errors

    @pytest.mark.parametrize("value", [b':0', b':65536', b':abc', b'""'])
    def test_invalid_port_is_reported_not_raised(self, value) -> None:
        raw = b'h2="' + value + b'"'
        field = parse_alternative_service_field(raw)

        assert field.raw == raw
        assert field.services[0].port is None
        assert field.errors

    def test_missing_protocol_identifier(self) -> None:
        field = parse_alternative_service_field(b'=":8000"')

        assert field.services[0].protocol_id == b""
        assert field.errors == (
            'empty protocol identifier in alternative service: b\'=":8000"\'',
        )


class TestMaxAge:
    """
    The ``ma`` parameter and its parsing status.
    """

    def test_absent(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"')
        assert field.max_age is None
        assert field.max_age_status is MaxAgeStatus.ABSENT

    def test_invalid(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"; ma=forever')
        assert field.max_age is None
        assert field.max_age_status is MaxAgeStatus.INVALID

    def test_repeated_ma_is_invalid(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"; ma=60; ma=90')
        assert field.services[0].max_age is None
        assert field.services[0].max_age_status is MaxAgeStatus.INVALID
        assert field.max_age is None
        assert field.max_age_status is MaxAgeStatus.INVALID

    def test_mixed_values_are_reported(self) -> None:
        field = parse_alternative_service_field(
            b'h2=":8000"; ma=60, h3=":443"; ma=120',
        )

        assert [s.max_age for s in field.services] == [60, 120]
        assert field.max_age is None
        assert field.max_age_status is MaxAgeStatus.MIXED


class TestDuplicatesAndClearing:
    """
    Repeated and clearing advertisements have explicit representations.
    """

    def test_duplicate_within_one_field(self) -> None:
        field = parse_alternative_service_field(
            b'h2=":8000"; ma=60, h2=":8000"; ma=60',
        )

        assert len(field.services) == 2
        assert not field.services[0].is_duplicate
        assert field.services[1].is_duplicate
        assert field.services[1].duplicate_of == 0
        assert field.duplicates == (field.services[1],)

    def test_case_folded_identity(self) -> None:
        field = parse_alternative_service_field(
            b'H2="Alt.Example:443", h2="alt.example:443"',
        )

        assert field.services[1].is_duplicate

    def test_different_port_is_not_a_duplicate(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000", h2=":8001"')
        assert not field.duplicates

    def test_ma_zero_clears_a_service(self) -> None:
        field = parse_alternative_service_field(b'h2=":8000"; ma=0')

        service = field.services[0]
        assert service.is_clearing
        assert not field.clears_all
        assert field.is_clear
        assert field.cleared_services == (service,)

    def test_clear_directive(self) -> None:
        field = parse_alternative_service_field(b"clear")

        assert field.clears_all
        assert field.is_clear
        assert field.services == ()
        assert field.raw == b"clear"

    def test_mixed_clearing_and_advertising_is_not_clear(self) -> None:
        field = parse_alternative_service_field(
            b'h2=":8000"; ma=0, h3=":443"; ma=60',
        )

        assert not field.clears_all
        assert not field.is_clear


class TestOriginHelpers:
    """
    Origin interpretation stays descriptive.
    """

    @pytest.mark.parametrize(("origin", "expected"), [
        (b"example.com", (b"example.com", None)),
        (b"example.com:8443", (b"example.com", 8443)),
        (b"[2001:db8::1]:443", (b"2001:db8::1", 443)),
        (b"[2001:db8::1", (b"[2001:db8::1", None)),
        (b"example.com:notaport", (b"example.com", None)),
        (None, (None, None)),
        (b"", (None, None)),
    ])
    def test_parse_origin(self, origin, expected) -> None:
        assert parse_origin(origin) == expected

    def test_same_origin_service(self) -> None:
        field = parse_alternative_service_field(b'h2=":8443"')
        service = field.services[0]

        assert not service_is_cross_origin(service, b"example.com:8443")
        assert service_is_cross_origin(service, b"example.com:443")

    def test_unnamed_host_is_not_cross_origin(self) -> None:
        field = parse_alternative_service_field(b'h3=":443"')
        service = field.services[0]

        assert not service_is_cross_origin(service, b"example.com")

    def test_service_without_authority_is_not_cross_origin(self) -> None:
        field = parse_alternative_service_field(b'h2=')
        service = field.services[0]

        assert service.host is None and service.port is None
        assert not service_is_cross_origin(service, b"example.com")

    def test_named_different_host_is_cross_origin(self) -> None:
        field = parse_alternative_service_field(b'h2="other.example:8443"')
        service = field.services[0]

        assert service_is_cross_origin(service, b"example.com")

    def test_port_only_service_against_origin_without_port(self) -> None:
        field = parse_alternative_service_field(b'h2=":8443"')
        service = field.services[0]

        # The origin did not state a port, so h2 does not invent one to compare
        # against: the host is unchanged and nothing is claimed to be known.
        assert not service_is_cross_origin(service, b"example.com")


class TestScopeValidation:
    """
    Connection-level and stream-level forms are validated separately.
    """

    def test_connection_level_requires_origin(self) -> None:
        assert validate_advertisement_scope(0, b"example.com") is AlternativeServiceScope.CONNECTION

        with pytest.raises(InvalidAlternativeServiceAdvertisement):
            validate_advertisement_scope(0, b"")

        with pytest.raises(InvalidAlternativeServiceAdvertisement):
            validate_advertisement_scope(0, None)

    def test_stream_level_forbids_origin(self) -> None:
        assert validate_advertisement_scope(1, b"") is AlternativeServiceScope.STREAM

        with pytest.raises(InvalidAlternativeServiceAdvertisement):
            validate_advertisement_scope(1, b"example.com")


class TestConnectionEvents:
    """
    End-to-end behaviour through H2Connection.
    """

    example_request_headers = [
        (":authority", "example.com"),
        (":path", "/"),
        (":scheme", "https"),
        (":method", "GET"),
    ]

    def _client(self):
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        return c

    def test_connection_level_event_is_fully_described(self, frame_factory) -> None:
        c = self._client()

        f = frame_factory.build_alt_svc_frame(
            stream_id=0,
            origin=b"example.com",
            field=b'h2=":8000"; ma=60, weird="other.example:443"',
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]

        assert isinstance(event, h2.events.AlternativeServiceAvailable)
        assert event.stream_id == 0
        assert event.scope is AlternativeServiceScope.CONNECTION
        assert event.is_connection_level
        assert event.origin == b"example.com"
        assert event.origin_source is OriginSource.FRAME
        assert event.field_value == b'h2=":8000"; ma=60, weird="other.example:443"'
        assert event.raw_field_value == event.field_value
        assert not c.data_to_send()

    def test_stream_level_event_is_bound_to_stream_origin(self, frame_factory) -> None:
        c = self._client()
        c.send_headers(stream_id=1, headers=self.example_request_headers)
        c.clear_outbound_data_buffer()

        f = frame_factory.build_alt_svc_frame(
            stream_id=1, origin=b"", field=b'h2=":8000"; ma=60',
        )
        events = c.receive_data(f.serialize())

        assert len(events) == 1
        event = events[0]

        assert event.stream_id == 1
        assert event.scope is AlternativeServiceScope.STREAM
        assert not event.is_connection_level
        assert event.origin == b"example.com"
        assert event.origin_source is OriginSource.AUTHORITY
        assert event.protocol_ids == (b"h2",)
        assert event.max_age == 60
        assert event.max_age_status is MaxAgeStatus.VALID

    def test_stream_level_advertisement_is_not_applied_to_connection(self, frame_factory) -> None:
        c = self._client()
        c.send_headers(stream_id=1, headers=self.example_request_headers)
        c.clear_outbound_data_buffer()

        # A stream-level frame that also carries an origin is an illegal
        # combination and must not leak onto the connection.
        f = frame_factory.build_alt_svc_frame(
            stream_id=1, origin=b"example.com", field=b'h2=":8000"',
        )
        events = c.receive_data(f.serialize())

        assert events == []
        assert not c.data_to_send()

    def test_connection_level_advertisement_without_origin_is_ignored(self, frame_factory) -> None:
        c = self._client()

        f = frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"", field=b'h2=":8000"',
        )
        events = c.receive_data(f.serialize())

        assert events == []
        assert not c.data_to_send()

    def test_repeated_advertisement_is_reported(self, frame_factory) -> None:
        c = self._client()
        frame = frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=60',
        )

        first = c.receive_data(frame.serialize())
        assert first[0].repeated_services == ()
        assert not first[0].is_duplicate

        second = c.receive_data(frame.serialize())
        assert len(second[0].repeated_services) == 1
        assert second[0].repeated_services[0].port == 8000
        assert second[0].is_duplicate

    def test_clear_advertisement_withdraws_previous(self, frame_factory) -> None:
        c = self._client()

        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=60',
        ).serialize())

        cleared = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b"clear",
        ).serialize())[0]

        assert cleared.clears_all
        assert cleared.is_clearing

        # The cleared service is no longer known, so re-advertising it is new.
        again = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=60',
        ).serialize())[0]
        assert again.repeated_services == ()

    def test_ma_zero_withdraws_a_single_service(self, frame_factory) -> None:
        c = self._client()

        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=60',
        ).serialize())

        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=0',
        ).serialize())

        again = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=60',
        ).serialize())[0]
        assert again.repeated_services == ()

    def test_partial_clear_leaves_other_services_known(self, frame_factory) -> None:
        c = self._client()

        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0,
            origin=b"example.com",
            field=b'h2=":8000"; ma=60, h3=":443"; ma=60',
        ).serialize())

        # A field value that clears one service while advertising another is
        # not a blanket clear, so the untouched service stays known.
        event = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0,
            origin=b"example.com",
            field=b'h2=":8000"; ma=0, h3=":8443"; ma=60',
        ).serialize())[0]
        assert not event.is_clearing

        again = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h3=":443"',
        ).serialize())[0]
        assert len(again.repeated_services) == 1
        assert again.repeated_services[0].port == 443

    def test_origin_cache_is_bounded(self, frame_factory) -> None:
        c = self._client()
        c.MAX_ALTERNATIVE_SERVICE_ORIGINS = 1

        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"one.example", field=b'h2=":8000"',
        ).serialize())
        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"two.example", field=b'h2=":8000"',
        ).serialize())

        assert len(c._alternative_services) == 1
        assert (AlternativeServiceScope.CONNECTION, b"two.example") in c._alternative_services

    def test_uninterpretable_services_are_not_recorded(self, frame_factory) -> None:
        c = self._client()

        # A field value h2 could not fully interpret is still delivered, but it
        # must not be remembered as an advertisement: it has no reliable
        # identity, so later repeats of it would be meaningless.
        broken = frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":notaport"',
        )
        event = c.receive_data(broken.serialize())[0]
        assert event.advertisement.errors
        assert not c._alternative_services

        again = c.receive_data(broken.serialize())[0]
        assert again.repeated_services == ()

    def test_invalid_max_age_service_is_not_recorded(self, frame_factory) -> None:
        c = self._client()

        event = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=forever',
        ).serialize())[0]
        assert event.max_age_status is MaxAgeStatus.INVALID
        assert not c._alternative_services

    def test_recorded_services_are_bounded(self, frame_factory) -> None:
        c = self._client()
        c.MAX_ALTERNATIVE_SERVICES_PER_ORIGIN = 2

        field = b", ".join(
            b'h2=":%d"' % port for port in (8001, 8002, 8003)
        )
        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=field,
        ).serialize())

        key = (AlternativeServiceScope.CONNECTION, b"example.com")
        assert len(c._alternative_services[key]) == 2

        # The oldest entry was evicted, so re-advertising it is not a repeat.
        again = c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8001"',
        ).serialize())[0]
        assert again.repeated_services == ()

    def test_scopes_are_tracked_separately(self, frame_factory) -> None:
        c = self._client()
        c.send_headers(stream_id=1, headers=self.example_request_headers)
        c.clear_outbound_data_buffer()

        stream_frame = frame_factory.build_alt_svc_frame(
            stream_id=1, origin=b"", field=b'h2=":8000"',
        )
        connection_frame = frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"',
        )

        c.receive_data(stream_frame.serialize())
        event = c.receive_data(connection_frame.serialize())[0]

        # Same origin and service, but a different scope: not a repeat.
        assert event.repeated_services == ()
        assert not event.is_duplicate

    def test_cross_origin_services_are_described_not_trusted(self, frame_factory) -> None:
        c = self._client()

        f = frame_factory.build_alt_svc_frame(
            stream_id=0,
            origin=b"example.com",
            field=b'h2=":443", h3="cdn.example.net:443"',
        )
        event = c.receive_data(f.serialize())[0]

        assert [s.host for s in event.cross_origin_services] == [b"cdn.example.net"]
        # Reporting a cross-origin service sends nothing.
        assert not c.data_to_send()

    def test_unknown_protocol_survives_to_event(self, frame_factory) -> None:
        c = self._client()

        f = frame_factory.build_alt_svc_frame(
            stream_id=0,
            origin=b"example.com",
            field=b'exp-proto=":9000"; ma=15',
        )
        event = c.receive_data(f.serialize())[0]

        assert event.protocol_ids == (b"exp-proto",)
        assert event.services[0].max_age == 15

    def test_event_repr_is_unchanged(self, frame_factory) -> None:
        c = self._client()

        f = frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b'h2=":8000"; ma=60',
        )
        event = c.receive_data(f.serialize())[0]

        assert repr(event) == (
            '<AlternativeServiceAvailable origin:example.com, '
            'field_value:h2=":8000"; ma=60>'
        )

    def test_server_ignores_connection_level_advertisement(self, frame_factory) -> None:
        c = h2.connection.H2Connection(
            config=h2.config.H2Configuration(client_side=False),
        )
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        f = frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"example.com", field=b"clear",
        )
        events = c.receive_data(f.serialize())

        assert events == []
        assert not c.data_to_send()

    def test_no_state_is_recorded_when_frame_is_ignored(self, frame_factory) -> None:
        c = self._client()

        c.receive_data(frame_factory.build_alt_svc_frame(
            stream_id=0, origin=b"", field=b'h2=":8000"',
        ).serialize())

        assert not c._alternative_services


class TestEventAccessors:
    """
    The event's derived accessors work without a connection.
    """

    def test_advertisement_is_lazy_and_stable(self) -> None:
        event = h2.events.AlternativeServiceAvailable()
        event.field_value = b'h2=":8000"; ma=60'

        first = event.advertisement
        assert first is event.advertisement
        assert first.services[0].protocol_id == b"h2"
        assert event.services == first.services
        assert event.max_age == 60
        assert not event.is_clearing

    def test_empty_event_defaults(self) -> None:
        event = h2.events.AlternativeServiceAvailable()

        assert event.stream_id is None
        assert event.scope is None
        assert event.raw_field_value is None
        assert event.services == ()
        assert event.protocol_ids == ()
        assert event.duplicates == ()
        assert event.repeated_services == ()
        assert event.max_age is None
        assert event.max_age_status is MaxAgeStatus.ABSENT
        assert event.cross_origin_services == ()
        assert not event.is_duplicate
        assert not event.is_clearing
        assert not event.clears_all
        assert not event.is_connection_level
