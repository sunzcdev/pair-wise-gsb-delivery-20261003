from __future__ import annotations

import pytest

from kombu.transport.virtual.ack import (
    AckCoordinationError,
    AckCoordinator,
    DeliveryRecord,
    DeliveryState,
    StaleDeliveryError,
    UnknownDeliveryTag,
)


class RecordingChannel:
    """Stand-in channel recording the calls the coordinator issues."""

    def __init__(self, key='chan'):
        self.key = key
        self.acks = []
        self.rejects = []
        self.fail_ack_tags = set()
        self.fail_reject_tags = set()

    def on_ack(self, channel_key, delivery_tag, multiple=False):
        self.acks.append((delivery_tag, multiple))
        if delivery_tag in self.fail_ack_tags:
            raise RuntimeError(f'ack failed for {delivery_tag}')

    def on_reject(self, channel_key, delivery_tag, requeue=False):
        self.rejects.append((delivery_tag, requeue))
        if delivery_tag in self.fail_reject_tags:
            raise RuntimeError(f'reject failed for {delivery_tag}')


@pytest.fixture
def coordinator():
    channel = RecordingChannel()
    coord = AckCoordinator(
        on_ack=channel.on_ack, on_reject=channel.on_reject,
    )
    return coord, channel


def _track(coord, channel, *tags):
    for tag in tags:
        coord.track(channel.key, tag, message=f'msg-{tag}')


class test_AckCoordinator:

    def test_track_registers_pending_records(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2, 3)

        assert [r.delivery_tag for r in coord.pending] == [1, 2, 3]
        assert all(r.state is DeliveryState.PENDING
                   for r in coord.pending)
        assert coord.epoch(channel.key) == 0

    def test_in_order_completion_uses_single_multiple_ack(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2, 3)

        for tag in (1, 2, 3):
            coord.mark(tag)
        flushed = coord.flush()

        assert [r.delivery_tag for r in flushed] == [1, 2, 3]
        assert channel.acks == [(3, True)]
        assert all(r.state is DeliveryState.ACK_SENT for r in flushed)
        assert not coord.pending
        assert {r.delivery_tag for r in coord.settled} == {1, 2, 3}

    def test_out_of_order_completion_never_crosses_pending(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2, 3, 4)

        # Workers finish tag 2 and 3 while tag 1 is still in flight.
        coord.mark(2)
        coord.mark(3)
        flushed = coord.flush()

        # Nothing may be sent: the multiple ack up to tag 3 would include 1.
        assert flushed == []
        assert channel.acks == []
        assert [r.delivery_tag for r in coord.completed] == [2, 3]

        # Completing tag 1 unlocks the whole completed prefix in one call.
        coord.mark(1)
        flushed = coord.flush()
        assert [r.delivery_tag for r in flushed] == [1, 2, 3]
        assert channel.acks == [(3, True)]
        # Tag 4 remains untouched because it was never completed.
        assert [r.delivery_tag for r in coord.pending] == [4]

    def test_partial_prefix_flushes_then_extends(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2, 3, 4)

        coord.mark(1)
        coord.mark(2)
        first = coord.flush()
        assert [r.delivery_tag for r in first] == [1, 2]
        assert channel.acks == [(2, True)]

        # Tag 3 still blocks tag 4.
        coord.mark(4)
        assert coord.flush() == []

        coord.mark(3)
        second = coord.flush()
        assert [r.delivery_tag for r in second] == [3, 4]
        assert channel.acks == [(2, True), (4, True)]

    def test_single_ack_does_not_set_multiple(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2)
        coord.mark(1)
        coord.flush()

        assert channel.acks == [(1, False)]
        # The pending second delivery stays queued.
        assert [r.delivery_tag for r in coord.pending] == [2]

    def test_reject_breaks_ack_prefix_and_uses_own_call(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2, 3)
        coord.mark(1)
        coord.mark(2, outcome='reject')
        coord.mark(3)

        flushed = coord.flush()

        assert [r.delivery_tag for r in flushed] == [1, 2, 3]
        # Tag 1 acked individually (the reject prevents batching across it),
        # tag 2 rejected without requeue, tag 3 acked individually.
        assert channel.acks == [(1, False), (3, False)]
        assert channel.rejects == [(2, False)]
        states = {r.delivery_tag: r.state for r in coord.settled}
        assert states[1] is DeliveryState.ACK_SENT
        assert states[2] is DeliveryState.REJECTED
        assert states[3] is DeliveryState.ACK_SENT

    def test_requeue_is_distinct_terminal_state(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2)
        coord.mark(1, outcome='requeue')
        coord.mark(2)
        coord.flush()

        assert channel.rejects == [(1, True)]
        states = {r.delivery_tag: r.state for r in coord.settled}
        assert states[1] is DeliveryState.REQUEUED
        assert states[2] is DeliveryState.ACK_SENT

    def test_ack_failure_keeps_inspectable_uncertain_state(self, coordinator):
        coord, channel = coordinator
        channel.fail_ack_tags = {3}
        _track(coord, channel, 1, 2, 3)
        coord.mark(1)
        coord.mark(2)
        coord.mark(3)

        # The failing multiple-ack covers the whole completed prefix; the
        # exception propagates and the local state must not pretend the acks
        # succeeded.
        with pytest.raises(RuntimeError):
            coord.flush()

        uncertain = coord.uncertain
        assert {r.delivery_tag for r in uncertain} == {1, 2, 3}
        assert all(r.state is DeliveryState.ACK_UNCERTAIN
                   for r in uncertain)
        assert all(isinstance(r.error, RuntimeError) for r in uncertain)
        assert coord.settled == []
        # The failed prefix stays queued and the second flush makes no new
        # channel call while the records are still uncertain.
        assert coord.flush() == []
        assert channel.acks == [(3, True)]

    def test_uncertain_delivery_blocks_prefix_until_retried(self, coordinator):
        coord, channel = coordinator
        channel.fail_ack_tags = {1}
        _track(coord, channel, 1, 2)
        coord.mark(1)
        with pytest.raises(RuntimeError):
            coord.flush()

        assert [r.delivery_tag for r in coord.uncertain] == [1]

        # A later completion must not jump over the uncertain first delivery.
        coord.mark(2)
        assert coord.flush() == []
        assert channel.acks == [(1, False)]

        # Recover the channel and retry; the prefix can now be sent.
        channel.fail_ack_tags.clear()
        coord.retry_uncertain()
        assert [r.delivery_tag for r in coord.completed] == [1, 2]
        flushed = coord.flush()
        assert [r.delivery_tag for r in flushed] == [1, 2]
        assert (2, True) in channel.acks

    def test_reject_failure_is_uncertain_and_inspectable(self, coordinator):
        coord, channel = coordinator
        channel.fail_reject_tags = {1}
        _track(coord, channel, 1, 2)
        coord.mark(1, outcome='reject')
        coord.mark(2)

        with pytest.raises(RuntimeError):
            coord.flush()

        record = coord.get(1)
        assert record.state is DeliveryState.REJECT_UNCERTAIN
        assert isinstance(record.error, RuntimeError)
        # The later ack must not jump over the failed reject.
        assert coord.flush() == []
        assert channel.acks == []

    def test_revive_channel_quarantines_old_tags_and_bumps_epoch(
            self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1, 2)
        coord.mark(2)

        epoch = coord.revive_channel(channel.key)
        assert epoch == 1
        assert coord.epoch(channel.key) == 1

        stale = coord.stale
        assert {r.delivery_tag for r in stale} == {1, 2}
        assert all(r.state is DeliveryState.STALE for r in stale)
        assert coord.pending == []

        # Old tags can no longer be completed on the replacement channel.
        with pytest.raises(StaleDeliveryError):
            coord.mark(1)
        with pytest.raises(StaleDeliveryError):
            coord.mark(2)

        # A fresh delivery on the new epoch works and uses tag 1's numeric
        # value safely because the stale record is quarantined separately.
        new_record = coord.track(channel.key, 99)
        assert new_record.epoch == 1
        coord.mark(99)
        coord.flush()
        assert channel.acks == [(99, False)]

    def test_independent_channels_are_coordinated_separately(self):
        chan_a = RecordingChannel('a')
        chan_b = RecordingChannel('b')

        def on_ack(channel_key, delivery_tag, multiple=False):
            if channel_key == 'a':
                chan_a.on_ack(channel_key, delivery_tag, multiple)
            else:
                chan_b.on_ack(channel_key, delivery_tag, multiple)

        coord = AckCoordinator(on_ack, chan_a.on_reject)
        coord.register_channel('b')

        coord.track('a', 1)
        coord.track('b', 1)  # same tag value, different channel
        coord.mark(1, channel_key='a')
        coord.mark(1, channel_key='b')

        # Explicit per-channel flushing keeps the two channels isolated and
        # neither multiple-ack crosses into the other channel's tag space.
        flushed_a = coord.flush('a')
        flushed_b = coord.flush('b')
        assert [r.channel_key for r in flushed_a] == ['a']
        assert [r.channel_key for r in flushed_b] == ['b']
        assert chan_a.acks == [(1, False)]
        assert chan_b.acks == [(1, False)]

    def test_mark_without_channel_requires_unique_tag(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1)
        # Single registered channel: the convenience lookup works.
        coord.mark(1)
        coord.flush()
        assert channel.acks == [(1, False)]

    def test_unknown_and_double_completion_are_rejected(self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1)

        with pytest.raises(UnknownDeliveryTag):
            coord.mark(999)
        with pytest.raises(ValueError):
            coord.mark(1, outcome='bogus')

        coord.mark(1)
        with pytest.raises(AckCoordinationError):
            coord.mark(1)

    def test_redelivery_replaces_live_record_but_keeps_history(
            self, coordinator):
        coord, channel = coordinator
        _track(coord, channel, 1)
        coord.mark(1)
        coord.flush()
        assert coord.get(1).state is DeliveryState.ACK_SENT

        # Same tag redelivered after a restore becomes a fresh pending record.
        record = coord.track(channel.key, 1)
        assert record.state is DeliveryState.PENDING
        assert coord.get(1).state is DeliveryState.PENDING

    def test_forget_drops_live_and_uncertain_records(self, coordinator):
        coord, channel = coordinator
        channel.fail_ack_tags = {1}
        _track(coord, channel, 1, 2)
        coord.mark(1)
        with pytest.raises(RuntimeError):
            coord.flush()

        coord.forget(1)
        coord.forget(2)
        assert coord.uncertain == []
        assert coord.pending == []
        with pytest.raises(UnknownDeliveryTag):
            coord.get(1)

    def test_retry_unknown_uncertain_tag_raises(self, coordinator):
        coord, channel = coordinator
        with pytest.raises(UnknownDeliveryTag):
            coord.retry_uncertain('missing')

    def test_delivery_state_and_record_helpers(self):
        record = DeliveryRecord(
            delivery_tag=1, channel_key='c', epoch=0,
        )
        assert not record.uncertain
        assert not record.settled
        assert DeliveryState.ACK_UNCERTAIN.is_uncertain
        assert DeliveryState.ACK_UNCERTAIN.no_longer_active
        assert DeliveryState.ACK_SENT.is_settled
        assert DeliveryState.ACK_SENT.no_longer_active
        assert DeliveryState.STALE.no_longer_active
        assert not DeliveryState.PENDING.no_longer_active
        assert not DeliveryState.ACKED.is_settled


class test_QoSCoordinatorIntegration:
    """The virtual QoS must drive the coordinator for transport subclasses."""

    def _qos(self):
        from kombu import Connection
        from kombu.transport import virtual

        connection = Connection(transport='memory')
        channel = connection.channel()
        qos = virtual.QoS(channel, prefetch_count=2)
        return qos, channel

    def test_out_of_order_ack_batches_only_completed_prefix(self):
        qos, channel = self._qos()
        try:
            for tag in ('a', 'b', 'c'):
                qos.append(f'msg-{tag}', tag)

            # Completing the middle delivery must not reach the channel yet.
            qos.ack('b')
            assert qos.ack_coordinator.settled == []
            assert qos._delivered  # nothing locally flushed past the prefix

            # Completing the head unlocks the [a, b] prefix in one call.
            qos.ack('a')
            settled = [r.delivery_tag
                       for r in qos.ack_coordinator.settled]
            assert settled == ['a', 'b']
            # The still-pending 'c' blocks nothing now but remains unacked.
            assert [r.delivery_tag for r in qos.ack_coordinator.pending] == [
                'c']
            qos.ack('c')
            assert not qos.ack_coordinator.pending
        finally:
            qos._on_collect.cancel()

    def test_prefetch_window_counts_uncertain_delivery_as_unfinished(self):
        qos, channel = self._qos()
        try:
            qos.append('m1', 'a')
            qos.append('m2', 'b')
            assert not qos.can_consume()  # prefetch_count == 2 reached

            def failing_ack(channel_key, delivery_tag, multiple=False):
                raise OSError('nope')

            qos._coordinator_ack = failing_ack
            with pytest.raises(OSError):
                qos.ack('a')

            # The uncertain delivery keeps occupying a prefetch slot.
            assert not qos.can_consume()
            assert qos.uncertain[0].state is DeliveryState.ACK_UNCERTAIN
        finally:
            qos._on_collect.cancel()

    def test_subclass_override_primitive_still_coordinates(self):
        from kombu import Connection
        from kombu.transport import virtual

        class RedisLikeQoS(virtual.QoS):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                self.removed = []

            def _coordinator_ack(self, channel_key, delivery_tag,
                                 multiple=False):
                self.removed.append((delivery_tag, multiple))
                super()._coordinator_ack(
                    channel_key, delivery_tag, multiple=multiple)

        connection = Connection(transport='memory')
        channel = connection.channel()
        qos = RedisLikeQoS(channel)
        try:
            for tag in ('a', 'b'):
                qos.append(f'msg-{tag}', tag)
            qos.ack('b')
            assert qos.removed == []  # blocked by the unfinished head 'a'
            qos.ack('a')
            assert qos.removed == [('b', True)]
        finally:
            qos._on_collect.cancel()
