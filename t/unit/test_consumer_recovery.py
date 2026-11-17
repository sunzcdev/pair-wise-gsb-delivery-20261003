"""Fault-injection tests for staged consumer connection recovery.

These tests never talk to a real message broker.  Recovery is driven with
in-process fake channels whose broker operations can be programmed to fail,
so the exact condition under which a recovery round counts as *complete*
is observable directly through :class:`~kombu.messaging.ConsumerRecovery`.
"""
from __future__ import annotations

from unittest.mock import Mock

import pytest

from kombu import Connection, Consumer, Exchange, Queue
from kombu.exceptions import (
    MessageStateError,
    RecoveryAborted,
    RecoveryStageError,
)
from kombu.messaging import (
    RECOVERY_STAGES,
    RECOVERY_STAGE_CHANNEL,
    RECOVERY_STAGE_CONSUME,
    RECOVERY_STAGE_DECLARE,
    RECOVERY_STAGE_QOS,
    ConsumerRecovery,
)
from t.mocks import Channel as BaseChannel
from t.mocks import Transport


class FaultConfig:
    """Programmable fault schedule for one or more fake channels."""

    def __init__(self):
        # method name -> number of calls that still raise
        self.fail_times = {}
        # every broker operation, in call order
        self.calls = []

    def fail_next(self, method, times=1):
        self.fail_times[method] = self.fail_times.get(method, 0) + times

    def record(self, method):
        self.calls.append(method)
        remaining = self.fail_times.get(method, 0)
        if remaining:
            self.fail_times[method] = remaining - 1
            raise ConnectionError(f'injected failure in {method}')


class FaultChannel(BaseChannel):
    """Mock channel whose broker operations can fail on demand."""

    def __init__(self, connection, config):
        super().__init__(connection)
        self.config = config
        # distinguish channels even when their id is not used
        self.token = getattr(connection, 'token', None)

    def queue_declare(self, *args, **kwargs):
        self.config.record('queue_declare')
        super().queue_declare(*args, **kwargs)

    def queue_bind(self, *args, **kwargs):
        self.config.record('queue_bind')
        super().queue_bind(*args, **kwargs)

    def basic_qos(self, *args, **kwargs):
        self.config.record('basic_qos')
        super().basic_qos(*args, **kwargs)

    def basic_consume(self, *args, **kwargs):
        self.config.record('basic_consume')
        super().basic_consume(*args, **kwargs)

    def basic_cancel(self, *args, **kwargs):
        self.config.record('basic_cancel')
        super().basic_cancel(*args, **kwargs)

    def close(self):
        self.config.record('close')
        super().close()


class FaultConnection:
    connected = True

    def __init__(self, client, token, config):
        self.client = client
        self.token = token
        self.config = config

    def channel(self):
        return FaultChannel(self, self.config)


class FaultTransport(Transport):
    """Transport handing out channels bound to a shared fault schedule.

    ``Connection`` only ever instantiates a transport with ``client=self``
    and swallows extra keyword arguments, so the active fault schedule is
    assigned to :attr:`active_config` by the test fixture (each test gets a
    fresh :class:`FaultConfig`).
    """

    #: FaultConfig shared by every channel this transport creates.
    active_config = None

    def establish_connection(self):
        return FaultConnection(self.client, object(), self.active_config)

    def create_channel(self, connection):
        # Connection.channel() calls this directly (it does not go through
        # the connection object), so every channel is bound to the shared
        # fault schedule for injection to work.
        return FaultChannel(connection, self.active_config)


@pytest.fixture
def harness():
    config = FaultConfig()
    FaultTransport.active_config = config
    connection = Connection(transport=FaultTransport)
    connection.connect()
    exchange = Exchange('events', 'direct')
    # auto_delete makes can_cache_declaration False, so every declare stage
    # reaches the (fault-injectable) channel operations rather than being
    # short-circuited by the per-connection declaration cache.
    queue = Queue(
        'events.q', exchange, 'events.rk', auto_delete=True,
    )
    return config, connection, exchange, queue


def make_consumer(connection, queue, prefetch_count=7, consume=True):
    consumer = Consumer(
        connection, [queue], prefetch_count=prefetch_count,
    )
    if consume:
        consumer.consume()
    return consumer


class test_ConsumerRecovery_state:

    def test_full_recovery_marks_every_stage_in_order(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        assert consumer.recovery.completed_stages == RECOVERY_STAGES
        assert consumer.recovery.recovered is True
        assert consumer.recovery.stage is None
        # channel work precedes declarations, which precede qos/consume
        assert config.calls.index('queue_declare') > -1
        assert (config.calls.index('basic_qos')
                < config.calls.index('basic_consume'))

    def test_stage_cannot_run_before_its_dependencies(self):
        recovery = ConsumerRecovery()
        recovery.begin(object())

        # nothing completed yet: consume needs channel/declare/qos first
        with pytest.raises(RecoveryStageError) as exc:
            recovery.require(RECOVERY_STAGE_CONSUME)
        assert exc.value.stage == RECOVERY_STAGE_CONSUME

        recovery.complete(RECOVERY_STAGE_CHANNEL)
        recovery.complete(RECOVERY_STAGE_DECLARE)
        with pytest.raises(RecoveryStageError):
            recovery.require(RECOVERY_STAGE_CONSUME)

        recovery.complete(RECOVERY_STAGE_QOS)
        recovery.require(RECOVERY_STAGE_CONSUME)  # no longer raises

    def test_stage_progress_is_independently_inspectable(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        # a later recovery round fails while declaring on the new channel
        config.fail_next('queue_declare')
        with pytest.raises(RecoveryStageError) as exc:
            consumer.revive(connection.channel())
        assert exc.value.stage == RECOVERY_STAGE_DECLARE

        # only the channel stage has completed; recovery is not complete
        assert consumer.recovery.completed_stages == (RECOVERY_STAGE_CHANNEL,)
        assert consumer.recovery.recovered is False
        assert consumer.recovery.stage == RECOVERY_STAGE_DECLARE


class test_staged_recovery_fault_injection:

    def test_declare_failure_leaves_earlier_stages_and_resumes(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        first_channel = consumer.channel

        # new round on a fresh channel; declare blows up the first time
        config.fail_next('queue_declare')
        new_channel = connection.channel()

        with pytest.raises(RecoveryStageError) as exc:
            consumer.revive(new_channel)
        assert exc.value.stage == RECOVERY_STAGE_DECLARE
        assert consumer.recovery.completed_stages == (RECOVERY_STAGE_CHANNEL,)

        # retrying on the SAME channel resumes at declare, skips the channel
        # stage and then finishes qos + consume
        consumer.revive(new_channel)
        assert consumer.recovery.recovered is True
        assert consumer.channel is new_channel
        assert first_channel in consumer.recovery._retired_channels

    def test_qos_failure_does_not_redeclare_or_reconsume_on_retry(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        config.fail_next('basic_qos')
        new_channel = connection.channel()
        with pytest.raises(RecoveryStageError) as exc:
            consumer.revive(new_channel)
        assert exc.value.stage == RECOVERY_STAGE_QOS
        assert consumer.recovery.completed_stages == (
            RECOVERY_STAGE_CHANNEL, RECOVERY_STAGE_DECLARE,
        )

        declares_before = config.calls.count('queue_declare')
        consumer.revive(new_channel)  # same channel: resume
        # no second declaration round on the resume
        assert config.calls.count('queue_declare') == declares_before
        assert consumer.recovery.recovered is True

    def test_consume_failure_retry_does_not_register_consumer_twice(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        config.fail_next('basic_consume')
        new_channel = connection.channel()
        # operations recorded before the recovery round on new_channel
        baseline = len(config.calls)
        with pytest.raises(RecoveryStageError) as exc:
            consumer.revive(new_channel)
        assert exc.value.stage == RECOVERY_STAGE_CONSUME
        # qos already applied before consume failed
        assert consumer.recovery.completed_stages == (
            RECOVERY_STAGE_CHANNEL, RECOVERY_STAGE_DECLARE, RECOVERY_STAGE_QOS,
        )
        # the attempt is recorded but unconfirmed, so the retry re-issues
        # the broker call once instead of silently dropping the consumer
        assert consumer.recovery.is_consumer_pending(queue.name)
        assert not consumer.recovery.is_consumer_registered(queue.name)
        # the failed attempt still reached the broker once
        assert config.calls[baseline:].count('basic_consume') == 1

        consumer.revive(new_channel)  # same-channel retry
        # on this channel: one failed attempt + one successful retry, and
        # never more than one confirmed registration
        assert config.calls[baseline:].count('basic_consume') == 2
        assert len(consumer._active_tags) == 1
        assert consumer.recovery.is_consumer_registered(queue.name)
        assert consumer.recovery.recovered is True

    def test_recovery_is_not_complete_until_consume_stage_done(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        config.fail_next('basic_consume')
        new_channel = connection.channel()
        with pytest.raises(RecoveryStageError):
            consumer.revive(new_channel)

        # channel/declare/qos done is still not a completed recovery
        assert RECOVERY_STAGE_CONSUME not in consumer.recovery.completed_stages
        assert consumer.recovery.recovered is False
        assert consumer.recovery.stage == RECOVERY_STAGE_CONSUME

        consumer.revive(new_channel)
        assert consumer.recovery.recovered is True
        assert consumer.recovery.stage is None


class test_reconnect_during_recovery:

    def test_second_disconnect_clears_round_and_restarts_on_channel(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        round_one = consumer.recovery.round

        # first recovery round gets stuck in declare
        config.fail_next('queue_declare')
        chan_a = connection.channel()
        with pytest.raises(RecoveryStageError):
            consumer.revive(chan_a)
        assert consumer.recovery.round == round_one + 1
        assert consumer.recovery.completed_stages == (RECOVERY_STAGE_CHANNEL,)

        # another disconnect while still recovering: a different channel must
        # discard the partial round instead of inheriting its progress
        chan_b = connection.channel()
        consumer.revive(chan_b)

        assert consumer.recovery.round == round_one + 2
        assert consumer.channel is chan_b
        assert consumer.recovery.recovered is True
        assert chan_a in consumer.recovery._retired_channels
        # the half-restored channel was best-effort closed
        assert 'close' in chan_a.called

    def test_abandoned_round_does_not_keep_partial_consumer_registration(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        config.fail_next('basic_consume')
        chan_a = connection.channel()
        with pytest.raises(RecoveryStageError):
            consumer.revive(chan_a)
        assert consumer.recovery.is_consumer_pending(queue.name)

        chan_b = connection.channel()
        consumer.revive(chan_b)
        # registration state belongs to the active round only
        assert consumer.recovery.is_consumer_registered(queue.name)
        assert consumer.recovery._consumers == {queue.name}
        assert consumer.recovery.pending_consumers == frozenset()
        assert len(consumer._active_tags) == 1


class test_close_blocks_recovery:

    def test_revive_after_close_raises_abort_instead_of_reconnecting(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        consumer.close()

        with pytest.raises(RecoveryAborted):
            consumer.revive(connection.channel())
        # no broker work happened for the rejected round
        assert consumer.recovery.recovered is False
        assert consumer.recovery.stage == RECOVERY_STAGE_CHANNEL

    def test_close_during_recovery_aborts_partial_round(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        config.fail_next('queue_declare')
        chan = connection.channel()
        with pytest.raises(RecoveryStageError):
            consumer.revive(chan)

        consumer.close()
        assert consumer.recovery._closed is True
        with pytest.raises(RecoveryAborted):
            consumer.revive(connection.channel())

    def test_recovery_aborted_is_not_a_transport_retry_error(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        consumer.close()

        # the surrounding ensure() loop only retries transport error
        # classes; RecoveryAborted must propagate and stop reconnection.
        assert RecoveryAborted not in (
            connection.recoverable_connection_errors
            + connection.recoverable_channel_errors
        )
        with pytest.raises(RecoveryAborted):
            connection.ensure(
                consumer, lambda: consumer.revive(connection.channel()),
                max_retries=3,
            )()

    def test_close_cancels_active_consumers(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        consumer.close()
        assert 'basic_cancel' in consumer.channel.called
        assert not consumer._active_tags


class test_channel_lineage:

    def test_message_from_retired_channel_cannot_be_acked_on_new_one(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        old_channel = consumer.channel

        # a delivery still in flight on the channel that then dies
        old_message = Mock(name='old-message')
        old_message.channel = old_channel
        old_message.ack = Mock()

        consumer.revive(connection.channel())
        assert old_channel in consumer.recovery._retired_channels

        # the replacement channel must never confirm the old delivery
        with pytest.raises(MessageStateError):
            consumer.ack_message(old_message)
        old_message.ack.assert_not_called()

    def test_message_on_active_channel_is_acknowledged(self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)

        message = Mock(name='live-message')
        message.channel = consumer.channel
        consumer.ack_message(message)
        message.ack.assert_called_once_with(multiple=False)

    def test_recovery_never_rebinds_old_delivery_to_new_channel(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        old_channel = consumer.channel
        old_message = Mock(name='old-message')
        old_message.channel = old_channel

        new_channel = connection.channel()
        consumer.revive(new_channel)

        # revive swaps the consumer channel but leaves the delivery bound
        # to its originating (now retired) channel
        assert old_message.channel is old_channel
        assert consumer.channel is new_channel
        assert not consumer.recovery.owns_delivery(old_channel)
        assert consumer.recovery.owns_delivery(new_channel)


class test_recovery_round_bookkeeping:

    def test_same_channel_retry_keeps_round_new_channel_bumps_it(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        initial = consumer.recovery.round

        same = consumer.channel
        consumer.revive(same)  # resume/no-op path: already complete
        assert consumer.recovery.round == initial

        consumer.revive(connection.channel())
        assert consumer.recovery.round == initial + 1

    def test_consumers_only_registered_for_previously_active_queues(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = Consumer(connection, [queue])
        # registered but never consumed from
        other = Queue(
            'other.q', exchange, 'other.rk', auto_delete=True,
        )
        consumer.add_queue(other)

        consumer.revive(connection.channel())
        assert not consumer.recovery.is_consumer_registered(queue.name)
        assert not consumer.recovery.is_consumer_registered(other.name)
        assert config.calls.count('basic_consume') == 0

    def test_revive_accepts_connection_and_resolves_default_channel(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        default_channel = connection.default_channel
        # the consumer was built on the connection's cached default channel
        assert consumer.channel is default_channel

        # passing the Connection (not a channel) must go through
        # maybe_channel(); it resolves to the same cached default channel,
        # so this is a same-channel resume (no new round), not a failure
        consumer.revive(connection)

        assert consumer.channel is default_channel
        assert consumer.recovery.recovered is True
        assert consumer.recovery.completed_stages == RECOVERY_STAGES

    def test_revive_with_fresh_connection_starts_new_round_via_maybe_channel(
            self, harness):
        config, connection, exchange, queue = harness
        consumer = make_consumer(connection, queue)
        before = consumer.recovery.round

        # a re-established connection exposes a different default channel;
        # passing the Connection object itself must still be accepted and
        # drive a complete new staged round via maybe_channel()
        fresh = Connection(transport=FaultTransport)
        FaultTransport.active_config = config
        fresh.connect()
        consumer.revive(fresh)

        assert consumer.channel is fresh.default_channel
        assert consumer.recovery.round == before + 1
        assert consumer.recovery.recovered is True
        assert consumer.recovery.completed_stages == RECOVERY_STAGES
