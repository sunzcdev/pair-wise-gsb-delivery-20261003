"""
Tests for the optional PING probe bookkeeping.
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.events
import h2.exceptions
import h2.ping


class FakeClock:
    """
    A manually advanced monotonic clock.
    """

    def __init__(self, start: float = 0.0) -> None:
        self.now = start

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


@pytest.fixture
def clock() -> FakeClock:
    return FakeClock()


@pytest.fixture
def manager(clock) -> h2.ping.PingProbeManager:
    return h2.ping.PingProbeManager(default_timeout=10.0, monotonic=clock)


@pytest.fixture
def client() -> h2.connection.H2Connection:
    config = h2.config.H2Configuration(client_side=True)
    c = h2.connection.H2Connection(config=config)
    c.initiate_connection()
    c.clear_outbound_data_buffer()
    return c


class TestPingProbeRegistration:
    def test_register_allocates_distinct_identities(self, manager) -> None:
        first = manager.register()
        second = manager.register()

        assert len(first) == 8
        assert len(second) == 8
        assert first != second
        assert len(manager) == 2

    def test_register_records_send_time_and_deadline(self, clock, manager) -> None:
        clock.advance(5.0)
        identity = manager.register(timeout=2.0)

        probe = manager.snapshot(identity)
        assert probe is not None
        assert probe.state is h2.ping.PingProbeState.PENDING
        assert probe.sent_at == 5.0
        assert probe.deadline == 7.0
        assert probe.resolved_at is None
        assert probe.rtt is None

    def test_register_default_timeout_used_when_unspecified(self, manager) -> None:
        identity = manager.register()

        probe = manager.snapshot(identity)
        assert probe is not None
        assert probe.deadline == 10.0

    def test_register_without_default_timeout_never_expires(self, clock) -> None:
        manager = h2.ping.PingProbeManager(monotonic=clock)
        identity = manager.register()

        clock.advance(1e9)
        assert manager.poll() == []
        assert manager.snapshot(identity).state is h2.ping.PingProbeState.PENDING

    @pytest.mark.parametrize(
        "opaque_data",
        [
            b"",
            b"\x01\x02\x03\x04\x05\x06\x07",
            "abcdefgh",
            b"too many bytes",
        ],
    )
    def test_register_rejects_malformed_identity(self, manager, opaque_data) -> None:
        with pytest.raises(ValueError):
            manager.register(opaque_data)

    def test_duplicate_pending_identity_is_rejected_before_mutation(self, manager) -> None:
        identity = manager.register(b"\x00\x00\x00\x00\x00\x00\x00\x01")
        pending_before = manager.pending_probes

        with pytest.raises(h2.exceptions.DuplicatePingProbeError):
            manager.register(identity)

        assert manager.pending_probes == pending_before
        assert len(manager) == 1

    def test_identity_can_be_reused_after_resolution(self, manager) -> None:
        identity = manager.register(b"\x00\x00\x00\x00\x00\x00\x00\x01")
        manager.acknowledge(identity)
        manager.register(b"\x00\x00\x00\x00\x00\x00\x00\x02")

        # A resolved identity is no longer outstanding, so it is usable again.
        reused = manager.register(identity)
        assert reused == identity
        assert len(manager) == 2

    def test_outstanding_limit_is_enforced(self, clock) -> None:
        manager = h2.ping.PingProbeManager(max_outstanding=2, monotonic=clock)
        manager.register()
        manager.register()

        with pytest.raises(h2.exceptions.TooManyPingProbesError):
            manager.register()

        assert len(manager) == 2

    def test_outstanding_limit_frees_after_acknowledgment(self, clock) -> None:
        manager = h2.ping.PingProbeManager(max_outstanding=1, monotonic=clock)
        identity = manager.register()
        manager.acknowledge(identity)

        rediscovered = manager.register()
        assert len(rediscovered) == 8

    def test_negative_timeout_rejected(self, manager) -> None:
        with pytest.raises(ValueError):
            manager.register(timeout=-1.0)


class TestPingProbeAcknowledgement:
    def test_acknowledge_returns_snapshot_with_rtt(self, clock, manager) -> None:
        identity = manager.register()
        clock.advance(0.25)

        probe = manager.acknowledge(identity)

        assert probe is not None
        assert probe.opaque_data == identity
        assert probe.state is h2.ping.PingProbeState.ACKED
        assert probe.rtt == pytest.approx(0.25)
        assert len(manager) == 0

    def test_unknown_acknowledgment_is_ignored(self, clock, manager) -> None:
        identity = manager.register()
        clock.advance(1.0)

        assert manager.acknowledge(b"\xff" * 8) is None

        probe = manager.snapshot(identity)
        assert probe is not None
        assert probe.state is h2.ping.PingProbeState.PENDING
        assert len(manager) == 1

    def test_unknown_acknowledgment_does_not_settle_another_probe(self, clock, manager) -> None:
        first = manager.register()
        second = manager.register()
        clock.advance(1.0)

        assert manager.acknowledge(b"\xff" * 8) is None

        assert manager.snapshot(first).state is h2.ping.PingProbeState.PENDING
        assert manager.snapshot(second).state is h2.ping.PingProbeState.PENDING

    def test_repeated_acknowledgment_is_ignored(self, clock, manager) -> None:
        identity = manager.register()
        clock.advance(0.5)
        assert manager.acknowledge(identity) is not None

        clock.advance(5.0)
        assert manager.acknowledge(identity) is None

        probe = manager.snapshot(identity)
        assert probe is not None
        assert probe.rtt == pytest.approx(0.5)

    def test_acknowledging_one_probe_leaves_others_pending(self, clock, manager) -> None:
        first = manager.register()
        second = manager.register()

        clock.advance(1.0)
        first_probe = manager.acknowledge(first)

        assert first_probe is not None
        assert first_probe.state is h2.ping.PingProbeState.ACKED
        assert manager.snapshot(second).state is h2.ping.PingProbeState.PENDING
        assert len(manager) == 1


class TestPingProbeTimeout:
    def test_poll_before_deadline_reports_nothing(self, clock, manager) -> None:
        manager.register(timeout=5.0)
        clock.advance(4.999)

        assert manager.poll() == []

    def test_poll_reports_expired_once(self, clock, manager) -> None:
        identity = manager.register(timeout=5.0)
        clock.advance(5.0)

        expired = manager.poll()

        assert len(expired) == 1
        assert expired[0].opaque_data == identity
        assert expired[0].state is h2.ping.PingProbeState.TIMED_OUT
        assert expired[0].resolved_at == pytest.approx(5.0)
        assert manager.poll() == []
        assert len(manager) == 0

    def test_poll_accepts_explicit_time(self, manager) -> None:
        identity = manager.register(timeout=1.0)

        expired = manager.poll(now=10.0)

        assert [probe.opaque_data for probe in expired] == [identity]

    def test_timed_out_probe_cannot_be_acknowledged(self, clock, manager) -> None:
        identity = manager.register(timeout=1.0)
        clock.advance(2.0)
        manager.poll()

        assert manager.acknowledge(identity) is None

        probe = manager.snapshot(identity)
        assert probe is not None
        assert probe.state is h2.ping.PingProbeState.TIMED_OUT

    def test_poll_reports_multiple_probes_in_registration_order(self, clock, manager) -> None:
        first = manager.register(timeout=1.0)
        second = manager.register(timeout=2.0)
        clock.advance(2.0)

        expired = manager.poll()

        assert [probe.opaque_data for probe in expired] == [first, second]


class TestPingProbeClose:
    def test_close_abandons_outstanding_probes(self, clock, manager) -> None:
        first = manager.register()
        second = manager.register()
        clock.advance(3.0)

        abandoned = manager.close()

        assert [probe.opaque_data for probe in abandoned] == [first, second]
        assert all(probe.state is h2.ping.PingProbeState.ABANDONED for probe in abandoned)
        assert all(probe.rtt is None for probe in abandoned)
        assert len(manager) == 0

    def test_close_is_idempotent(self, manager) -> None:
        manager.register()

        assert len(manager.close()) == 1
        assert manager.close() == []


class TestResolvedHistory:
    def test_resolved_history_is_bounded(self, clock) -> None:
        manager = h2.ping.PingProbeManager(max_resolved=2, monotonic=clock)
        identities = [manager.register() for _ in range(4)]
        for identity in identities:
            manager.acknowledge(identity)

        resolved = manager.resolved_probes
        assert [probe.opaque_data for probe in resolved] == identities[-2:]

    def test_resolved_history_can_be_disabled(self, clock) -> None:
        manager = h2.ping.PingProbeManager(max_resolved=0, monotonic=clock)
        identity = manager.register()
        manager.acknowledge(identity)

        assert manager.resolved_probes == ()
        assert manager.snapshot(identity) is None

    def test_reresolving_identity_refreshes_history_order(self, clock) -> None:
        manager = h2.ping.PingProbeManager(max_resolved=2, monotonic=clock)
        identity = b"\x00" * 7 + b"\x09"

        manager.register(identity)
        manager.acknowledge(identity)

        other = manager.register()
        manager.acknowledge(other)

        # Resolving the first identity again moves it to the end rather than
        # leaving it in its original slot.
        manager.register(identity)
        manager.acknowledge(identity)

        assert [probe.opaque_data for probe in manager.resolved_probes] == [other, identity]

    def test_allocator_skips_resolved_identities(self, clock) -> None:
        manager = h2.ping.PingProbeManager(monotonic=clock)
        first = manager.register()
        manager.acknowledge(first)

        second = manager.register()
        assert second != first


class TestPingProbeManagerValidation:
    @pytest.mark.parametrize(
        "kwargs",
        [
            {"max_outstanding": 0},
            {"default_timeout": -1.0},
            {"max_resolved": -1},
        ],
    )
    def test_invalid_configuration_rejected(self, kwargs) -> None:
        with pytest.raises(ValueError):
            h2.ping.PingProbeManager(**kwargs)


class TestConnectionPingProbes:
    def test_send_ping_probe_emits_frame_and_registers(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)

        identity = client.send_ping_probe()

        expected = frame_factory.build_ping_frame(identity).serialize()
        assert client.data_to_send() == expected
        assert len(manager) == 1
        assert manager.snapshot(identity).state is h2.ping.PingProbeState.PENDING

    def test_send_ping_probe_with_explicit_identity(self, frame_factory, clock, client) -> None:
        client.enable_ping_probes(monotonic=clock)
        identity = b"\x01\x02\x03\x04\x05\x06\x07\x08"

        assert client.send_ping_probe(identity) == identity

        expected = frame_factory.build_ping_frame(identity).serialize()
        assert client.data_to_send() == expected

    def test_send_ping_probe_rejects_duplicate_without_sending(self, clock, client) -> None:
        client.enable_ping_probes(monotonic=clock)
        identity = b"\x01\x02\x03\x04\x05\x06\x07\x08"
        client.send_ping_probe(identity)
        client.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.DuplicatePingProbeError):
            client.send_ping_probe(identity)

        assert client.data_to_send() == b""

    def test_send_ping_probe_rejects_overflow_without_sending(self, clock, client) -> None:
        client.enable_ping_probes(max_outstanding=1, monotonic=clock)
        client.send_ping_probe()
        client.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.TooManyPingProbesError):
            client.send_ping_probe()

        assert client.data_to_send() == b""

    def test_send_ping_probe_rejects_malformed_identity(self, clock, client) -> None:
        client.enable_ping_probes(monotonic=clock)

        with pytest.raises(ValueError):
            client.send_ping_probe(b"short")

    def test_send_ping_probe_requires_enabled_bookkeeping(self, client) -> None:
        assert client.ping_probes is None
        with pytest.raises(RuntimeError):
            client.send_ping_probe()
        with pytest.raises(RuntimeError):
            client.poll_ping_probes()

    def test_ack_settles_matching_probe(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        identity = client.send_ping_probe(opaque_data=b"\x01" * 8)
        client.clear_outbound_data_buffer()

        clock.advance(0.75)
        ack = frame_factory.build_ping_frame(identity, flags=["ACK"])
        events = client.receive_data(ack.serialize())

        assert len(events) == 1
        assert isinstance(events[0], h2.events.PingAckReceived)
        probe = manager.snapshot(identity)
        assert probe.state is h2.ping.PingProbeState.ACKED
        assert probe.rtt == pytest.approx(0.75)
        # The event itself identifies the probe it settled.
        assert events[0].probe == probe

    def test_ack_for_unknown_identity_does_not_settle_probe(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        identity = client.send_ping_probe(opaque_data=b"\x01" * 8)
        client.clear_outbound_data_buffer()

        ack = frame_factory.build_ping_frame(b"\xff" * 8, flags=["ACK"])
        events = client.receive_data(ack.serialize())

        assert isinstance(events[0], h2.events.PingAckReceived)
        assert events[0].probe is None
        assert manager.snapshot(identity).state is h2.ping.PingProbeState.PENDING

    def test_ack_only_settles_the_probe_it_answers(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        first = client.send_ping_probe(opaque_data=b"\x01" * 8)
        second = client.send_ping_probe(opaque_data=b"\x02" * 8)
        client.clear_outbound_data_buffer()

        ack = frame_factory.build_ping_frame(second, flags=["ACK"])
        events = client.receive_data(ack.serialize())

        assert events[0].probe is not None
        assert events[0].probe.opaque_data == second
        assert manager.snapshot(first).state is h2.ping.PingProbeState.PENDING
        assert manager.snapshot(second).state is h2.ping.PingProbeState.ACKED

    def test_poll_ping_probes_does_not_emit_data(self, clock, client) -> None:
        client.enable_ping_probes(monotonic=clock, default_timeout=1.0)
        client.send_ping_probe()
        client.clear_outbound_data_buffer()
        clock.advance(2.0)

        expired = client.poll_ping_probes()

        assert len(expired) == 1
        assert expired[0].state is h2.ping.PingProbeState.TIMED_OUT
        assert client.data_to_send() == b""

    def test_plain_ping_is_not_tracked(self, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)

        client.ping(b"\x01" * 8)

        assert len(manager) == 0

    def test_local_goaway_abandons_probes(self, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        identity = client.send_ping_probe()

        client.close_connection()

        probe = manager.snapshot(identity)
        assert probe.state is h2.ping.PingProbeState.ABANDONED
        assert probe.rtt is None
        assert len(manager) == 0

    def test_remote_goaway_abandons_probes(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        identity = client.send_ping_probe()
        client.clear_outbound_data_buffer()

        goaway = frame_factory.build_goaway_frame(last_stream_id=0)
        client.receive_data(goaway.serialize())

        assert manager.snapshot(identity).state is h2.ping.PingProbeState.ABANDONED

    def test_protocol_error_abandons_probes(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        identity = client.send_ping_probe()
        client.clear_outbound_data_buffer()

        # A PING on a stream is a connection error, terminating the connection.
        bad_ping = frame_factory.build_ping_frame(b"\x00" * 8)
        bad_ping.stream_id = 1
        with pytest.raises(h2.exceptions.ProtocolError):
            client.receive_data(bad_ping.serialize())

        assert manager.snapshot(identity).state is h2.ping.PingProbeState.ABANDONED

    def test_enable_ping_probes_returns_manager(self, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)

        assert manager is client.ping_probes
        assert isinstance(manager, h2.ping.PingProbeManager)

class TestProbeEdgeCases:
    def test_poll_with_explicit_now_advances_connection_clock(self, clock, client) -> None:
        client.enable_ping_probes(monotonic=clock, default_timeout=1.0)
        client.send_ping_probe()
        client.clear_outbound_data_buffer()

        # The injected clock has not moved, but an explicit timestamp drives
        # the deadline evaluation forward.
        expired = client.poll_ping_probes(now=5.0)

        assert len(expired) == 1
        assert expired[0].state is h2.ping.PingProbeState.TIMED_OUT
        assert client.data_to_send() == b""

    def test_rejected_send_does_not_fabricate_ack_frame(self, frame_factory, clock, client) -> None:
        client.enable_ping_probes(max_outstanding=1, monotonic=clock)
        client.send_ping_probe()
        client.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.TooManyPingProbesError):
            client.send_ping_probe()

        # A failed registration must not enqueue anything, and in particular
        # no PING frame: there is nothing to send at all.
        assert client.data_to_send() == b""

    def test_ack_after_connection_closed_is_ignored(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)
        identity = client.send_ping_probe()
        client.clear_outbound_data_buffer()
        client.close_connection()

        ack = frame_factory.build_ping_frame(identity, flags=["ACK"])
        with pytest.raises(h2.exceptions.ProtocolError):
            client.receive_data(ack.serialize())

        # The probe keeps its terminal state even though a stray ACK arrived.
        probe = manager.snapshot(identity)
        assert probe is not None
        assert probe.state is h2.ping.PingProbeState.ABANDONED

    def test_received_ping_does_not_create_probe(self, frame_factory, clock, client) -> None:
        manager = client.enable_ping_probes(monotonic=clock)

        inbound = frame_factory.build_ping_frame(b"\x07" * 8)
        events = client.receive_data(inbound.serialize())

        assert isinstance(events[0], h2.events.PingReceived)
        assert len(manager) == 0
        # The automatic ACK is still emitted for an inbound PING.
        assert client.data_to_send() != b""

    def test_probe_snapshot_is_immutable(self, clock, manager) -> None:
        identity = manager.register()
        snapshot = manager.snapshot(identity)

        clock.advance(1.0)
        manager.acknowledge(identity)

        assert snapshot.state is h2.ping.PingProbeState.PENDING
        assert snapshot.resolved_at is None
        assert manager.snapshot(identity).state is h2.ping.PingProbeState.ACKED

    def test_allocate_identity_wraps_from_max(self, clock) -> None:
        manager = h2.ping.PingProbeManager(monotonic=clock)
        manager._next_identity = 2**64 - 2

        max_identity = manager.register()
        assert max_identity == b"\xff" * 8

        wrapped = manager.register()
        assert wrapped == b"\x00" * 7 + b"\x01"

    def test_pending_probes_are_ordered_by_registration(self, clock, manager) -> None:
        identities = [manager.register() for _ in range(3)]

        assert [probe.opaque_data for probe in manager.pending_probes] == identities

    def test_abandon_settles_single_probe(self, clock, manager) -> None:
        first = manager.register()
        second = manager.register()

        abandoned = manager.abandon(first)

        assert abandoned is not None
        assert abandoned.state is h2.ping.PingProbeState.ABANDONED
        assert abandoned.rtt is None
        assert manager.snapshot(second).state is h2.ping.PingProbeState.PENDING
        assert manager.abandon(first) is None

