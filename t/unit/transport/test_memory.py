from __future__ import annotations

import socket

import pytest

from kombu import Connection, Consumer, Exchange, Producer, Queue
from kombu.transport.virtual.ack import DeliveryState


class test_MemoryTransport:

    def setup_method(self):
        self.c = Connection(transport='memory')
        self.e = Exchange('test_transport_memory')
        self.q = Queue('test_transport_memory',
                       exchange=self.e,
                       routing_key='test_transport_memory')
        self.q2 = Queue('test_transport_memory2',
                        exchange=self.e,
                        routing_key='test_transport_memory2')
        self.fanout = Exchange('test_transport_memory_fanout', type='fanout')
        self.q3 = Queue('test_transport_memory_fanout1',
                        exchange=self.fanout)
        self.q4 = Queue('test_transport_memory_fanout2',
                        exchange=self.fanout)

    def test_driver_version(self):
        assert self.c.transport.driver_version()

    def test_produce_consume_noack(self):
        channel = self.c.channel()
        producer = Producer(channel, self.e)
        consumer = Consumer(channel, self.q, no_ack=True)

        for i in range(10):
            producer.publish({'foo': i}, routing_key='test_transport_memory')

        _received = []

        def callback(message_data, message):
            _received.append(message)

        consumer.register_callback(callback)
        consumer.consume()

        while 1:
            if len(_received) == 10:
                break
            self.c.drain_events()

        assert len(_received) == 10

    def test_produce_consume_fanout(self):
        producer = self.c.Producer()
        consumer = self.c.Consumer([self.q3, self.q4])

        producer.publish(
            {'hello': 'world'},
            declare=consumer.queues,
            exchange=self.fanout,
        )

        assert self.q3(self.c).get().payload == {'hello': 'world'}
        assert self.q4(self.c).get().payload == {'hello': 'world'}
        assert self.q3(self.c).get() is None
        assert self.q4(self.c).get() is None

    def test_produce_consume(self):
        channel = self.c.channel()
        producer = Producer(channel, self.e)
        consumer1 = Consumer(channel, self.q)
        consumer2 = Consumer(channel, self.q2)
        self.q2(channel).declare()

        for i in range(10):
            producer.publish({'foo': i}, routing_key='test_transport_memory')
        for i in range(10):
            producer.publish({'foo': i}, routing_key='test_transport_memory2')

        _received1 = []
        _received2 = []

        def callback1(message_data, message):
            _received1.append(message)
            message.ack()

        def callback2(message_data, message):
            _received2.append(message)
            message.ack()

        consumer1.register_callback(callback1)
        consumer2.register_callback(callback2)

        consumer1.consume()
        consumer2.consume()

        while 1:
            if len(_received1) + len(_received2) == 20:
                break
            self.c.drain_events()

        assert len(_received1) + len(_received2) == 20

        # compression
        producer.publish({'compressed': True},
                         routing_key='test_transport_memory',
                         compression='zlib')
        m = self.q(channel).get()
        assert m.payload == {'compressed': True}

        # queue.delete
        for i in range(10):
            producer.publish({'foo': i}, routing_key='test_transport_memory')
        assert self.q(channel).get()
        self.q(channel).delete()
        self.q(channel).declare()
        assert self.q(channel).get() is None

        # queue.purge
        for i in range(10):
            producer.publish({'foo': i}, routing_key='test_transport_memory2')
        assert self.q2(channel).get()
        self.q2(channel).purge()
        assert self.q2(channel).get() is None

    def test_drain_events(self):
        with pytest.raises(socket.timeout):
            self.c.drain_events(timeout=0.1)

        c1 = self.c.channel()
        c2 = self.c.channel()

        with pytest.raises(socket.timeout):
            self.c.drain_events(timeout=0.1)

        del c1  # so pyflakes doesn't complain.
        del c2

    def test_drain_events_unregistered_queue(self):
        c1 = self.c.channel()
        producer = self.c.Producer()
        consumer = self.c.Consumer([self.q2])

        producer.publish(
            {'hello': 'world'},
            declare=consumer.queues,
            routing_key=self.q2.routing_key,
            exchange=self.q2.exchange,
        )
        message = consumer.queues[0].get()._raw

        class Cycle:

            def get(self, callback, timeout=None):
                return (message, 'foo'), c1

        self.c.transport.cycle = Cycle()
        self.c.drain_events()

    def test_queue_for(self):
        chan = self.c.channel()
        chan.queues.clear()

        x = chan._queue_for('foo')
        assert x
        assert chan._queue_for('foo') is x

    # see the issue
    # https://github.com/celery/kombu/issues/1050
    def test_producer_on_return(self):
        def on_return(_exception, _exchange, _routing_key, _message):
            pass
        channel = self.c.channel()
        producer = Producer(channel, on_return=on_return)
        consumer = self.c.Consumer([self.q3])

        producer.publish(
            {'hello': 'on return'},
            declare=consumer.queues,
            exchange=self.fanout,
        )

        assert self.q3(self.c).get().payload == {'hello': 'on return'}
        assert self.q3(self.c).get() is None


class test_AckCoordinationMemory:
    """End-to-end coordination checks on the in-memory transport."""

    def setup_method(self):
        self.connection = Connection(transport='memory')
        self.exchange = Exchange('test_ack_coordination')
        self.queue = Queue(
            'test_ack_coordination',
            exchange=self.exchange,
            routing_key='test_ack_coordination',
        )
        self.channel = self.connection.channel()
        self.queue(self.channel).declare()
        self.producer = Producer(self.channel, self.exchange)

    def teardown_method(self):
        if self.channel._qos is not None:
            self.channel._qos._on_collect.cancel()
        self.channel.close()
        self.connection.close()

    def _publish(self, count):
        for i in range(count):
            self.producer.publish(
                {'seq': i}, routing_key='test_ack_coordination')

    def _consume(self, count):
        messages = []

        def callback(body, message):
            messages.append(message)

        consumer = Consumer(self.channel, [self.queue],
                            callbacks=[callback])
        consumer.consume()
        while len(messages) < count:
            self.connection.drain_events()
        consumer.cancel()
        return messages

    def test_out_of_order_acks_only_batch_completed_prefix(self):
        self._publish(4)
        messages = self._consume(4)
        qos = self.channel.qos
        coordinator = qos.ack_coordinator

        tags = [m.delivery_tag for m in messages]

        # Parallel workers finish the later messages first.
        messages[3].ack()
        messages[1].ack()

        # Nothing left pending in the coordinator list beyond the still
        # in-flight messages 0 and 2, and no tag was wrongly settled past
        # the first unfinished delivery.
        pending_tags = {r.delivery_tag for r in coordinator.pending}
        assert tags[0] in pending_tags
        assert tags[2] in pending_tags
        assert coordinator.settled == []

        # Finishing the head of the queue flushes the contiguous completed
        # prefix [0, 1] in one batched acknowledgment.  Tag 3 stays queued
        # because tag 2 is still in flight and a multiple ack cannot cross it.
        messages[0].ack()
        settled_tags = {r.delivery_tag for r in coordinator.settled}
        assert {tags[0], tags[1]} <= settled_tags
        assert tags[3] not in settled_tags
        # Message 2 was never completed, so it must remain pending together
        # with the blocked, already-completed message 3.
        pending_tags = {r.delivery_tag for r in coordinator.pending}
        assert tags[2] in pending_tags
        assert tags[3] not in pending_tags

        messages[2].ack()
        assert not coordinator.pending
        assert all(r.state is DeliveryState.ACK_SENT
                   for r in coordinator.settled)

    def test_reject_and_requeue_have_distinct_terminal_states(self):
        self._publish(3)
        messages = self._consume(3)
        coordinator = self.channel.qos.ack_coordinator

        messages[0].ack()
        messages[1].reject()
        messages[2].requeue()

        states = {r.delivery_tag: r.state for r in coordinator.settled}
        assert states[messages[0].delivery_tag] is DeliveryState.ACK_SENT
        assert states[messages[1].delivery_tag] is DeliveryState.REJECTED
        assert states[messages[2].delivery_tag] is DeliveryState.REQUEUED

        # The requeued message is available for redelivery.
        redelivered = self.queue(self.channel).get()
        assert redelivered is not None
        assert redelivered.payload == {'seq': 2}

    def test_channel_revive_quarantines_old_delivery_tags(self):
        self._publish(2)
        messages = self._consume(2)
        coordinator = self.channel.qos.ack_coordinator
        old_tags = [m.delivery_tag for m in messages]

        # Simulate a reconnect: the old channel's tags must never be used
        # against the replacement channel.
        self.channel.revive()

        assert coordinator.epoch(self.channel) == 1
        stale_tags = {r.delivery_tag for r in coordinator.stale}
        assert set(old_tags) == stale_tags
        for tag in old_tags:
            record = coordinator.get(tag, channel_key=self.channel)
            assert record.state is DeliveryState.STALE

        # New deliveries on the revived channel coordinate independently.
        self._publish(1)
        new_messages = self._consume(1)
        assert new_messages[0].delivery_tag not in stale_tags
        new_messages[0].ack()
        assert coordinator.get(
            new_messages[0].delivery_tag,
            channel_key=self.channel).state is DeliveryState.ACK_SENT

    def test_failed_ack_keeps_uncertain_state_and_blocks_prefix(self):
        self._publish(3)
        messages = self._consume(3)
        qos = self.channel.qos
        coordinator = qos.ack_coordinator

        real_primitive = qos._coordinator_ack

        def flaky_ack(channel_key, delivery_tag, multiple=False):
            if delivery_tag == messages[0].delivery_tag:
                raise OSError('broker gone')
            return real_primitive(channel_key, delivery_tag,
                                  multiple=multiple)

        qos._coordinator_ack = flaky_ack

        # The failure must propagate instead of being swallowed, and local
        # state must not claim success while the broker outcome is unknown.
        with pytest.raises(OSError):
            messages[0].ack()

        uncertain = coordinator.uncertain
        assert len(uncertain) == 1
        record = uncertain[0]
        assert record.state is DeliveryState.ACK_UNCERTAIN
        assert isinstance(record.error, OSError)
        # The original Message is also still unacknowledged locally.
        assert not messages[0].acknowledged

        # A later completion cannot jump over the uncertain head delivery.
        messages[1].ack()
        messages[2].ack()
        assert coordinator.flush() == []

        # After recovery the uncertain delivery can be retried and the full
        # prefix then settles.
        qos._coordinator_ack = real_primitive
        coordinator.retry_uncertain()
        flushed = coordinator.flush()
        assert {r.delivery_tag for r in flushed} == {
            m.delivery_tag for m in messages}
        assert not coordinator.uncertain

    def test_qos_revive_clears_local_transactional_state(self):
        self._publish(2)
        messages = self._consume(2)
        qos = self.channel.qos

        assert len(qos._delivered) == 2
        qos.revive(channel=self.channel)
        assert qos._delivered == {}
        assert qos._dirty == set()
        assert qos.ack_coordinator.pending == []
        assert len(qos.ack_coordinator.stale) == 2
        # The local messages are still reported as not acknowledged.
        assert all(not m.acknowledged for m in messages)

    def test_message_multiple_ack_is_mediated_by_coordinator(self):
        self._publish(3)
        messages = self._consume(3)
        qos = self.channel.qos
        calls = []
        real_primitive = qos._coordinator_ack

        def recording_ack(channel_key, delivery_tag, multiple=False):
            calls.append((delivery_tag, multiple))
            return real_primitive(channel_key, delivery_tag,
                                  multiple=multiple)

        qos._coordinator_ack = recording_ack

        # A caller requests multiple=True while the middle message is still
        # in flight.  The coordinator must not honor an unsafe batch.
        messages[0].ack()
        messages[2].ack(multiple=True)
        assert calls == [(messages[0].delivery_tag, False)]

        # Completing the remaining head delivery unlocks one batched ack that
        # covers the contiguous completed prefix only.
        messages[1].ack(multiple=True)
        assert calls[-1] == (messages[2].delivery_tag, True)
        assert not qos.ack_coordinator.pending
