"""
Tests for the local SETTINGS update ledger.

The ledger reports, batch by batch, which local SETTINGS modifications the
remote peer has confirmed, while keeping the desired values separate from the
currently effective ones.
"""
from __future__ import annotations

import pytest

import h2.config
import h2.connection
import h2.events
import h2.exceptions
import h2.settings
from h2.settings import SettingCodes
from hyperframe.frame import SettingsFrame


def acknowledgement() -> bytes:
    """Build the serialized bytes of an empty SETTINGS acknowledgement."""
    frame = SettingsFrame(0)
    frame.flags.add("ACK")
    return frame.serialize()


class TestBatchIdentity:
    """
    Each emitted SETTINGS frame becomes its own batch with a stable identity.
    """

    def test_each_update_records_one_batch(self) -> None:
        """
        Successive update_settings calls create distinct batches in send order.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 1000})
        c.update_settings({
            SettingCodes.HEADER_TABLE_SIZE: 2000,
            SettingCodes.INITIAL_WINDOW_SIZE: 70000,
        })
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 3000})

        batches = c.local_settings.ledger.batches
        assert [b.batch_id for b in batches] == [0, 1, 2]
        assert dict(batches[0].settings) == {SettingCodes.HEADER_TABLE_SIZE: 1000}
        assert dict(batches[1].settings) == {
            SettingCodes.HEADER_TABLE_SIZE: 2000,
            SettingCodes.INITIAL_WINDOW_SIZE: 70000,
        }
        assert all(b.acknowledged is False for b in batches)

    def test_batch_values_are_a_read_only_snapshot(self) -> None:
        """
        The recorded values cannot be mutated after the fact, and later updates
        do not rewrite the earlier batch.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        sent = {SettingCodes.HEADER_TABLE_SIZE: 1000}
        c.update_settings(sent)
        sent[SettingCodes.HEADER_TABLE_SIZE] = 9999
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 2000})

        first = c.local_settings.ledger.batch(0)
        assert first is not None
        assert dict(first.settings) == {SettingCodes.HEADER_TABLE_SIZE: 1000}
        with pytest.raises(TypeError):
            first.settings[SettingCodes.HEADER_TABLE_SIZE] = 1

    def test_ledger_lookup_by_missing_id(self) -> None:
        """
        Looking up an unknown batch id returns None rather than raising.
        """
        c = h2.connection.H2Connection()
        assert c.local_settings.ledger.batch(7) is None


class TestAckCorrespondence:
    """
    Acknowledgements settle the oldest outstanding batch, one per ACK.
    """

    def test_acks_settle_batches_in_order(self) -> None:
        """
        Three ACKs settle three batches in send order, and every ACK records the
        batch it settled.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 1000})
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 2000})
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 3000})

        settled = []
        for _ in range(3):
            events = c.receive_data(acknowledgement())
            settled.append(events[-1].batch_id)

        assert settled == [0, 1, 2]
        ledger = c.local_settings.ledger
        assert ledger.pending == ()
        assert [b.ack_id for b in ledger.acknowledged] == [0, 1, 2]

    def test_ack_event_carries_batch_identity(self) -> None:
        """
        The SettingsAcknowledged event exposes the settled batch id and the
        ack identity, and is not marked anomalous.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 1234})

        event = c.receive_data(acknowledgement())[-1]

        assert isinstance(event, h2.events.SettingsAcknowledged)
        assert event.batch_id == 0
        assert event.ack_id == 0
        assert event.anomalous is False
        assert event.changed_settings[SettingCodes.HEADER_TABLE_SIZE].new_value == 1234

    def test_batch_records_the_changes_it_settled(self) -> None:
        """
        A settled batch keeps the ChangedSetting records produced by its ACK.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 88})

        c.receive_data(acknowledgement())

        batch = c.local_settings.ledger.batch(0)
        assert batch is not None
        assert batch.changed_settings is not None
        change = batch.changed_settings[SettingCodes.HEADER_TABLE_SIZE]
        assert change.original_value == 4096
        assert change.new_value == 88

    def test_settling_a_batch_ignores_directly_staged_values(self) -> None:
        """
        A value staged directly on the mapping (not through update_settings) is
        not attributed to a batch, so settling the batch leaves it untouched.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        # Stage a value directly, without recording a batch.
        c.local_settings[SettingCodes.MAX_FRAME_SIZE] = 17000
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 500})

        event = c.receive_data(acknowledgement())[-1]

        assert event.batch_id == 0
        assert list(event.changed_settings) == [SettingCodes.HEADER_TABLE_SIZE]
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 500
        # MAX_FRAME_SIZE was not part of the batch, so it is still outstanding.
        assert c.local_settings[SettingCodes.MAX_FRAME_SIZE] == 16384


class TestAnomalousAcks:
    """
    An ACK that cannot be matched to a batch never advances a later batch.
    """

    def test_ack_without_pending_batch_is_recorded_and_ignored(self) -> None:
        """
        After every batch is settled, a further ACK is anomalous and leaves the
        effective values alone.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 500})

        ack = acknowledgement()
        c.receive_data(ack)

        # Now stage another update but do not let it be settled by the stray ack
        # that arrives before a batch is outstanding.
        events = c.receive_data(ack)
        event = events[-1]
        assert event.anomalous is True
        assert event.batch_id is None
        assert event.ack_id == 1
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 500
        assert len(c.local_settings.ledger.anomalies) == 1

    def test_anomalous_ack_does_not_advance_the_next_batch(self) -> None:
        """
        A stray ACK between batches is recorded, and the following real ACK
        still settles the correct batch.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        ack = acknowledgement()
        # Stray ACK before any batch exists behaves like the initial preface ACK.
        c.receive_data(ack)

        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 2000})
        event = c.receive_data(ack)[-1]
        assert event.batch_id == 0
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 2000

    def test_stray_ack_does_not_settle_a_later_batch(self) -> None:
        """
        A stray ACK that arrives after a batch is already settled does not
        silently settle the batch that is sent afterwards.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        ack = acknowledgement()

        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 500})
        assert c.receive_data(ack)[-1].batch_id == 0
        # A stray ACK now has nothing to settle: it must not be remembered and
        # applied to the next batch.
        stray = c.receive_data(ack)[-1]
        assert stray.anomalous is True

        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 900})
        assert c.local_settings.ledger.pending[0].batch_id == 1
        # The next batch is still pending until its own ACK arrives.
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 500
        settled = c.receive_data(ack)[-1]
        assert settled.batch_id == 1
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 900

    def test_ack_before_any_batch_keeps_legacy_behaviour(self) -> None:
        """
        With no batch recorded yet, an ACK belongs to the initial connection
        SETTINGS: it stages outstanding values but settles no batch and is not
        anomalous.
        """
        settings = h2.settings.Settings()
        settings[SettingCodes.HEADER_TABLE_SIZE] = 2048

        result = settings.acknowledge_local()

        assert result.batch is None
        assert result.ack_id is None
        assert result.anomalous is False
        assert settings[SettingCodes.HEADER_TABLE_SIZE] == 2048
        assert settings.ledger.anomalies == ()

    def test_ack_after_close_is_anomalous(self) -> None:
        """
        Once the ledger has been closed, a late ACK cannot settle an abandoned
        batch. This is checked directly, because the connection state machine
        refuses to deliver frames after a real close.
        """
        settings = h2.settings.Settings()
        settings[SettingCodes.HEADER_TABLE_SIZE] = 7000
        settings.record_batch({SettingCodes.HEADER_TABLE_SIZE: 7000})
        settings.close_ledger()

        result = settings.acknowledge_local()

        assert result.anomalous is True
        assert result.batch is None
        assert result.ack_id == 0
        assert result.changed_settings == {}
        assert settings.ledger.anomalies == (
            "SETTINGS ACK received after the connection closed",
        )
        # The effective value is untouched, and the batch stays abandoned.
        assert settings[SettingCodes.HEADER_TABLE_SIZE] == 4096
        assert [b.batch_id for b in settings.ledger.abandoned] == [0]


class TestEmptyAndDuplicateBatches:
    """
    Empty batches and repeated values are still settled independently.
    """

    def test_empty_batch_settles_on_its_own_ack(self) -> None:
        """
        An empty SETTINGS frame is still a batch: its ACK settles it without
        affecting the next batch's values.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        c.update_settings({})
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 2222})

        assert len(c.data_to_send()) > 0
        assert [b.batch_id for b in c.local_settings.ledger.pending] == [0, 1]

        ack = acknowledgement()

        first = c.receive_data(ack)[-1]
        assert first.batch_id == 0
        assert first.changed_settings == {}
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 4096

        second = c.receive_data(ack)[-1]
        assert second.batch_id == 1
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 2222

    def test_repeated_values_are_separate_batches(self) -> None:
        """
        Re-sending the same value still creates a new batch, settled by its own
        ACK.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 4096})
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 4096})

        ack = acknowledgement()

        assert c.receive_data(ack)[-1].batch_id == 0
        assert c.receive_data(ack)[-1].batch_id == 1
        assert c.local_settings.ledger.pending == ()


class TestConnectionCloseSettlement:
    """
    Closing the connection settles outstanding batches as abandoned.
    """

    def test_local_close_abandons_pending_batches(self) -> None:
        """
        A locally initiated close marks every pending batch abandoned.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 8000})

        c.close_connection()

        ledger = c.local_settings.ledger
        assert ledger.closed is True
        assert ledger.pending == ()
        assert [b.batch_id for b in ledger.abandoned] == [0]

    def test_peer_goaway_abandons_pending_batches(self, frame_factory) -> None:
        """
        A peer GOAWAY also closes the ledger and abandons outstanding batches.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 8000})

        goaway = frame_factory.build_goaway_frame(last_stream_id=0)
        c.receive_data(goaway.serialize())

        ledger = c.local_settings.ledger
        assert ledger.closed is True
        assert [b.batch_id for b in ledger.abandoned] == [0]

    def test_close_is_idempotent(self) -> None:
        """
        Closing the ledger twice keeps the abandoned batch set stable.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 8000})

        c.local_settings.close_ledger()
        c.local_settings.close_ledger()

        assert [b.batch_id for b in c.local_settings.ledger.abandoned] == [0]

    def test_protocol_error_teardown_abandons_batches(self, frame_factory) -> None:
        """
        A protocol error that tears the connection down also settles the ledger
        with the outstanding batch abandoned.
        """
        c = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=False))
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.INITIAL_WINDOW_SIZE: 1000})

        # An overlong DATA frame forces a connection teardown.
        oversized = frame_factory.build_data_frame(b"\x01" * 17000)
        with pytest.raises(h2.exceptions.ProtocolError):
            c.receive_data(oversized.serialize())

        ledger = c.local_settings.ledger
        assert ledger.closed is True
        assert [b.batch_id for b in ledger.abandoned] == [0]


class TestDesiredVersusEffective:
    """
    Desired values and effective values are exposed separately.
    """

    def test_desired_values_include_unacked_updates(self) -> None:
        """
        Desired values reflect staged changes while effective values do not.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.INITIAL_WINDOW_SIZE: 1000})

        assert c.local_settings.desired_values[SettingCodes.INITIAL_WINDOW_SIZE] == 1000
        assert c.local_settings.effective_values[SettingCodes.INITIAL_WINDOW_SIZE] == 65535

    def test_effective_values_follow_ack(self) -> None:
        """
        Acknowledging moves effective values forward without changing desired.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.INITIAL_WINDOW_SIZE: 1000})

        ack = acknowledgement()
        c.receive_data(ack)

        assert c.local_settings.desired_values[SettingCodes.INITIAL_WINDOW_SIZE] == 1000
        assert c.local_settings.effective_values[SettingCodes.INITIAL_WINDOW_SIZE] == 1000

    def test_value_mappings_are_read_only(self) -> None:
        """
        The desired and effective snapshots cannot be mutated in place.
        """
        c = h2.connection.H2Connection()
        with pytest.raises(TypeError):
            c.local_settings.effective_values[SettingCodes.HEADER_TABLE_SIZE] = 1
        with pytest.raises(TypeError):
            c.local_settings.desired_values[SettingCodes.HEADER_TABLE_SIZE] = 1


class TestWindowTiming:
    """
    Window-related settings change at the acknowledgement point only.
    """

    @pytest.mark.parametrize("client_side", [True, False])
    def test_initial_window_applies_only_after_ack(self, client_side, frame_factory) -> None:
        """
        A stream opened before the ACK keeps the old window; a stream opened
        afterwards uses the new one.
        """
        config = h2.config.H2Configuration(client_side=client_side)
        c = h2.connection.H2Connection(config=config)
        c.initiate_connection()
        if not client_side:
            c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        c.update_settings({SettingCodes.INITIAL_WINDOW_SIZE: 1000})

        # The connection-level window is unaffected by INITIAL_WINDOW_SIZE.
        assert c.local_settings.effective_values[SettingCodes.INITIAL_WINDOW_SIZE] == 65535
        assert c.local_settings.desired_values[SettingCodes.INITIAL_WINDOW_SIZE] == 1000

        if client_side:
            c.send_headers(
                1,
                [(":method", "GET"), (":scheme", "https"),
                 (":authority", "example.com"), (":path", "/")],
                end_stream=True,
            )
            assert c.remote_flow_control_window(1) == 65535
        else:
            headers = frame_factory.build_headers_frame(
                [(":method", "GET"), (":scheme", "https"),
                 (":authority", "example.com"), (":path", "/")],
                stream_id=1,
            )
            c.receive_data(headers.serialize())
            assert c.streams[1].inbound_flow_control_window == 65535

        c.receive_data(acknowledgement())

        # Existing streams are adjusted by the delta, and the effective value
        # only now reflects the change.
        if client_side:
            assert c.remote_flow_control_window(1) == 1000
        else:
            assert c.streams[1].inbound_flow_control_window == 1000

        assert c.local_settings.effective_values[SettingCodes.INITIAL_WINDOW_SIZE] == 1000

    def test_ledger_does_not_apply_window_early(self) -> None:
        """
        Merely reading the ledger never applies a staged window change.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.INITIAL_WINDOW_SIZE: 2048})

        # Reading every ledger surface must not change what is effective.
        assert c.local_settings.ledger.pending[0].settings[SettingCodes.INITIAL_WINDOW_SIZE] == 2048
        assert c.local_settings.effective_values[SettingCodes.INITIAL_WINDOW_SIZE] == 65535
        assert c.local_settings[SettingCodes.INITIAL_WINDOW_SIZE] == 65535


class TestPeerSettingsIsolation:
    """
    Settings the peer sends never enter the local ledger.
    """

    def test_received_settings_do_not_touch_local_ledger(self, frame_factory) -> None:
        """
        Receiving a SETTINGS frame fills the remote ledger, not the local one.
        """
        c = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=False))
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        received = frame_factory.build_settings_frame(
            {SettingCodes.HEADER_TABLE_SIZE: 1234},
        )
        c.receive_data(received.serialize())

        assert len(c.local_settings.ledger) == 0
        assert c.remote_settings[SettingCodes.HEADER_TABLE_SIZE] == 1234

    def test_local_and_remote_ledgers_are_distinct(self, frame_factory) -> None:
        """
        The two Settings objects hold independent ledgers.
        """
        c = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=False))
        c.initiate_connection()
        c.receive_data(frame_factory.preamble())
        c.clear_outbound_data_buffer()

        received = frame_factory.build_settings_frame(
            {SettingCodes.MAX_CONCURRENT_STREAMS: 5},
        )
        c.receive_data(received.serialize())
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 200})

        assert len(c.local_settings.ledger) == 1
        assert c.local_settings.ledger.batch(0) is not None
        assert c.remote_settings.ledger is not c.local_settings.ledger
        assert len(c.remote_settings.ledger) == 0


class TestInvalidBatchRejectedAtomically:
    """
    An invalid update is rejected before anything is staged or recorded.
    """

    def test_invalid_value_leaves_no_batch(self) -> None:
        """
        Rejecting an invalid update records no batch and stages no value.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()

        with pytest.raises(h2.exceptions.InvalidSettingsValueError):
            c.update_settings({
                SettingCodes.HEADER_TABLE_SIZE: 1000,
                SettingCodes.ENABLE_PUSH: 5,
            })

        assert len(c.local_settings.ledger) == 0
        assert c.local_settings[SettingCodes.HEADER_TABLE_SIZE] == 4096
        assert c.local_settings.desired_values[SettingCodes.HEADER_TABLE_SIZE] == 4096
        assert c.data_to_send() == b""


class TestLedgerReadOnlySurface:
    """
    The ledger collections are immutable snapshots.
    """

    def test_collections_are_tuples(self) -> None:
        """
        Every ledger collection is returned as an immutable tuple.
        """
        c = h2.connection.H2Connection()
        ledger = c.local_settings.ledger
        assert isinstance(ledger.batches, tuple)
        assert isinstance(ledger.pending, tuple)
        assert isinstance(ledger.acknowledged, tuple)
        assert isinstance(ledger.abandoned, tuple)
        assert isinstance(ledger.anomalies, tuple)

    def test_ledger_iteration_and_length(self) -> None:
        """
        Iterating the ledger yields the batches in order and len matches.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 100})
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 200})

        ledger = c.local_settings.ledger
        assert len(ledger) == 2
        assert [b.batch_id for b in ledger] == [0, 1]
        assert ledger.next_batch_id == 2

    def test_state_enum_names(self) -> None:
        """
        The batch state enum exposes pending/acknowledged/abandoned identities.
        """
        assert h2.settings.SettingsBatchState.PENDING.value == "pending"
        assert h2.settings.SettingsBatchState.ACKNOWLEDGED.value == "acknowledged"
        assert h2.settings.SettingsBatchState.ABANDONED.value == "abandoned"

    def test_reprs(self) -> None:
        """
        The ledger, batch and ack-result objects have useful debug reprs.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({SettingCodes.HEADER_TABLE_SIZE: 100})

        ledger = c.local_settings.ledger
        assert repr(ledger) == (
            "SettingsLedger(batches=1, pending=1, closed=False, anomalies=0)"
        )

        batch = ledger.batch(0)
        assert batch is not None
        assert repr(batch) == (
            "SettingsBatch(batch_id=0, state='pending', "
            f"settings={{{SettingCodes.HEADER_TABLE_SIZE!r}: 100}})"
        )

        result = c.local_settings.acknowledge_local()
        assert "SettingsAckResult(batch=" in repr(result)
        assert "anomalous=False" in repr(result)


class TestUnsetSettingPlaceholders:
    """
    A staged update for a brand-new setting code has no effective value yet.
    """

    def test_unknown_setting_is_desired_but_not_effective(self) -> None:
        """
        Until acknowledged, a new setting code appears only in desired values.
        """
        c = h2.connection.H2Connection()
        c.initiate_connection()
        c.clear_outbound_data_buffer()
        c.update_settings({97: 1})

        assert 97 not in c.local_settings.effective_values
        assert c.local_settings.desired_values[97] == 1

        ack = acknowledgement()
        c.receive_data(ack)

        assert c.local_settings.effective_values[97] == 1
        assert c.local_settings.desired_values[97] == 1

    def test_directly_staged_new_setting_has_no_effective_value(self) -> None:
        """
        A value staged directly on the mapping (the legacy pattern) starts with
        a placeholder and therefore has no effective value until acknowledged.
        """
        settings = h2.settings.Settings()
        settings[97] = 1

        assert 97 not in settings.effective_values
        assert settings.desired_values[97] == 1

        settings.acknowledge()
        assert settings.effective_values[97] == 1
