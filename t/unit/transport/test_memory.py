from __future__ import annotations

import socket

import pytest

from kombu import Connection, Consumer, Exchange, Producer, Queue
from kombu.transport import virtual


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


class test_MemoryAlternateExchange:
    """Alternate exchange routing verified against real queue contents."""

    def setup_method(self):
        self.c = Connection(transport='memory')
        self.channel = self.c.channel()

    def teardown_method(self):
        self.c.close()

    def _declare(self, exchange, queue, routing_key, arguments=None):
        c = self.channel
        c.exchange_declare(exchange, 'direct', arguments=arguments)
        c.queue_declare(queue)
        c.queue_bind(queue, exchange, routing_key)

    def _publish(self, exchange, routing_key, body='msg', **kwargs):
        return self.channel.basic_publish(
            self.channel.prepare_message(body), exchange, routing_key,
            **kwargs)

    def _queue_size(self, queue):
        return self.channel.queues[queue].qsize()

    def test_fallback_delivery_preserves_message_identity(self):
        self._declare('mx.alt', 'mx.altq', 'rkey')
        self._declare('mx.main', 'mx.mainq', 'other-key',
                      arguments={'alternate-exchange': 'mx.alt'})

        self._publish('mx.main', 'rkey')

        # original exchange did not match: nothing delivered there.
        assert self._queue_size('mx.mainq') == 0
        # alternate exchange accepted exactly one copy.
        assert self._queue_size('mx.altq') == 1

        message = self.channel.basic_get('mx.altq', no_ack=True)
        assert message.body == b'msg'
        # original routing key and exchange are preserved.
        assert message.delivery_info['exchange'] == 'mx.main'
        assert message.delivery_info['routing_key'] == 'rkey'
        assert self._queue_size('mx.altq') == 0

    def test_no_fallback_when_exchange_routes(self):
        self._declare('my.alt', 'my.altq', 'rkey')
        self._declare('my.main', 'my.mainq', 'rkey',
                      arguments={'alternate-exchange': 'my.alt'})

        self._publish('my.main', 'rkey')

        assert self._queue_size('my.mainq') == 1
        assert self._queue_size('my.altq') == 0

    def test_chain_delivery_count(self):
        self._declare('mz.last', 'mz.lastq', 'rkey')
        self.channel.exchange_declare(
            'mz.mid', 'direct', arguments={'alternate-exchange': 'mz.last'})
        self.channel.exchange_declare(
            'mz.main', 'direct', arguments={'alternate-exchange': 'mz.mid'})

        for _ in range(3):
            self._publish('mz.main', 'rkey')

        # every message fell through the whole chain to the last exchange.
        assert self._queue_size('mz.lastq') == 3
        for _ in range(3):
            message = self.channel.basic_get('mz.lastq', no_ack=True)
            assert message.delivery_info['exchange'] == 'mz.main'
            assert message.delivery_info['routing_key'] == 'rkey'
        assert self.channel.basic_get('mz.lastq', no_ack=True) is None

    def test_same_queue_receives_single_copy(self):
        # queue bound to both the exchange and its alternate: a message
        # falling through to the alternate is still delivered only once.
        c = self.channel
        c.exchange_declare('mw.alt', 'direct')
        c.exchange_declare('mw.main', 'direct',
                           arguments={'alternate-exchange': 'mw.alt'})
        c.queue_declare('mw.q')
        c.queue_bind('mw.q', 'mw.main', 'key.main')
        c.queue_bind('mw.q', 'mw.alt', 'key.alt')

        self._publish('mw.main', 'key.alt')
        assert self._queue_size('mw.q') == 1

        self._publish('mw.main', 'key.main')
        assert self._queue_size('mw.q') == 2

    def test_mandatory_unrouted_raises_and_delivers_nothing(self):
        self._declare('mm.alt', 'mm.altq', 'other-key')
        self.channel.exchange_declare(
            'mm.main', 'direct', arguments={'alternate-exchange': 'mm.alt'})

        with pytest.raises(virtual.UndeliverableError):
            self._publish('mm.main', 'rkey', mandatory=True)

        assert self._queue_size('mm.altq') == 0

    def test_mandatory_missing_alternate_raises(self):
        self.channel.exchange_declare(
            'mn.main', 'direct',
            arguments={'alternate-exchange': 'mn.gone'})

        with pytest.raises(virtual.UndeliverableError) as exc_info:
            self._publish('mn.main', 'rkey', mandatory=True)
        assert 'mn.gone' in str(exc_info.value)

    def test_unrouted_not_mandatory_is_dropped(self):
        self.channel.exchange_declare('mo.main', 'direct')
        assert self._publish('mo.main', 'rkey') == []

    def test_mandatory_routed_returns_queues(self):
        self._declare('mp.main', 'mp.q', 'rkey')
        assert self._publish('mp.main', 'rkey', mandatory=True) == ['mp.q']
        assert self._queue_size('mp.q') == 1
