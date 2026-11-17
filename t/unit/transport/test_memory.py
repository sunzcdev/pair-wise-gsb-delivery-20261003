from __future__ import annotations

import socket

import pytest

from kombu import Connection, Consumer, Exchange, Producer, Queue

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


class test_MemoryTransportTopicRouting:
    """Topic routing semantics for the in-memory transport.

    These tests exercise the full declare/bind/publish path instead of
    the exchange internals, so they assert what actually lands in the
    real queues.
    """

    def setup_method(self):
        # The memory transport keeps its broker state (and the Channel
        # queues) in process-global attributes, so reset it to keep the
        # routing assertions independent from other tests.
        from kombu.transport import memory
        memory.Channel.queues = {}
        memory.Transport.global_state.clear()

        self.connection = Connection(transport='memory')
        self.channel = self.connection.channel()
        self.exchange = Exchange('test_memory_topic', type='topic')
        self.exchange(self.channel).declare()
        self.producer = Producer(self.channel, exchange=self.exchange)

    def teardown_method(self):
        self.connection.close()
        from kombu.transport import memory
        memory.Channel.queues = {}
        memory.Transport.global_state.clear()

    def _queue(self, name, routing_key):
        queue = Queue(name, exchange=self.exchange, routing_key=routing_key)
        queue(self.channel).declare()
        return queue

    def _sizes(self, *names):
        return {name: self.channel._size(name) for name in names}

    @pytest.mark.parametrize('routing_key,binding,delivered', [
        # '*' matches exactly one (non-empty) word
        ('stock.us', 'stock.*', True),
        ('stock', 'stock.*', False),
        ('stock.us.nasdaq', 'stock.*', False),
        ('word', '*', True),
        ('two.words', '*', False),
        # '#' matches zero or more words, at any position
        ('stock', 'stock.#', True),
        ('stock.us', 'stock.#', True),
        ('stock.eu.de.tech', 'stock.#', True),
        ('bonds', 'stock.#', False),
        ('nasdaq', '#.nasdaq', True),
        ('stock.us.nasdaq', '#.nasdaq', True),
        ('stock.nasdaq.eu', '#.nasdaq', False),
        ('stock.nasdaq', 'stock.#.nasdaq', True),
        ('stock.eu.de.nasdaq', 'stock.#.nasdaq', True),
        ('stock.eu.nyse', 'stock.#.nasdaq', False),
        # '#' alone matches everything, including an empty routing key
        ('anything.at.all', '#', True),
        ('', '#', True),
        # empty segments behave like literal empty words
        ('a..b', 'a..b', True),
        ('a.x.b', 'a..b', False),
    ])
    def test_topic_wildcard_routing(self, routing_key, binding, delivered):
        queue = self._queue('wildcard_q', binding)
        self.producer.publish({'rk': routing_key}, routing_key=routing_key)
        assert self.channel._size(queue.name) == (1 if delivered else 0)

    def test_message_with_multiple_bindings_to_same_queue_delivered_once(self):
        # One queue bound several times with overlapping patterns.  Even
        # though more than one binding matches, the queue must receive a
        # single copy of the message.
        queue = Queue(
            'multi_bind',
            exchange=self.exchange,
            bindings=[
                self.exchange.binding(routing_key='#'),
                self.exchange.binding(routing_key='stock.#'),
                self.exchange.binding(routing_key='stock.us.*'),
                self.exchange.binding(routing_key='stock.us.#'),
            ],
        )
        queue(self.channel).declare()

        self.producer.publish({'id': 1}, routing_key='stock.us.nasdaq')

        assert self.channel._size(queue.name) == 1
        message = self.channel.basic_get(queue.name)
        assert message.payload == {'id': 1}
        # no duplicate copy left behind
        assert self.channel.basic_get(queue.name) is None

    def test_distinct_matching_queues_each_get_one_copy(self):
        q1 = self._queue('q_hash', 'stock.#')
        q2 = self._queue('q_star', 'stock.us.*')
        q3 = self._queue('q_exact', 'stock.us.nasdaq')
        other = self._queue('q_other', 'bond.*')

        self.producer.publish({'id': 2}, routing_key='stock.us.nasdaq')

        assert self._sizes(q1.name, q2.name, q3.name, other.name) == {
            q1.name: 1, q2.name: 1, q3.name: 1, other.name: 0,
        }

    def test_unmatched_message_is_not_routed_to_any_queue(self):
        q1 = self._queue('q_stock', 'stock.#')
        q2 = self._queue('q_news', 'news.*')

        self.producer.publish({'id': 3}, routing_key='weather.today')

        # nothing delivered and no phantom queue created for the key
        assert self._sizes(q1.name, q2.name) == {q1.name: 0, q2.name: 0}
        assert 'weather.today' not in self.channel.queues

    def test_queue_delete_removes_its_routes(self):
        q1 = self._queue('q_deleted', 'stock.#')
        q2 = self._queue('q_kept', 'news.*')

        self.channel.queue_delete(q1.name)

        # the binding is gone from both routing registries
        assert q1.name not in self.channel.state.queue_index
        assert all(
            queue != q1.name
            for _rkey, _pattern, queue in self.channel.get_table(self.exchange.name)
        )

        # a key that used to match the deleted queue must not recreate it
        self.producer.publish({'id': 4}, routing_key='stock.eu')
        assert not self.channel._has_queue(q1.name)
        # and must never be redirected to an unrelated queue
        assert self.channel._size(q2.name) == 0

        # surviving bindings keep working
        self.producer.publish({'id': 5}, routing_key='news.sports')
        assert self.channel._size(q2.name) == 1

    def test_publish_uses_one_consistent_routing_view(self):
        # Capture the table seen by a publish while a binding is added in
        # between.  Every publish must evaluate against one complete view:
        # either the old table or the new one, never a half-updated mix.
        channel = self.channel
        seen_views = []
        original_lookup = channel.typeof(self.exchange.name).lookup

        def spy_lookup(table, exchange, routing_key, default):
            seen_views.append(tuple(table))
            return original_lookup(table, exchange, routing_key, default)

        channel.typeof(self.exchange.name).lookup = spy_lookup
        try:
            self._queue('q_first', 'a.#')
            self.producer.publish({'step': 'before'}, routing_key='a.b')

            self._queue('q_second', 'a.b')
            self.producer.publish({'step': 'after'}, routing_key='a.b')
        finally:
            channel.typeof(self.exchange.name).lookup = original_lookup

        # each publish saw a self-consistent snapshot: the first view has
        # exactly one binding, the second view has both -- never a partial
        # state where only some of the declared bindings are visible.
        assert len(seen_views[0]) == 1
        assert len(seen_views[1]) == 2
        assert {queue for _rk, _pat, queue in seen_views[1]} == {
            'q_first', 'q_second',
        }
        assert self.channel._size('q_first') == 2
        assert self.channel._size('q_second') == 1

    def test_rebind_after_delete_routes_to_new_queue_only(self):
        old = self._queue('q_rebind', 'stock.#')
        self.channel.queue_delete(old.name)
        new = self._queue('q_rebind', 'stock.#')

        self.producer.publish({'id': 6}, routing_key='stock.us')

        # exactly one copy in the recreated queue and no stale duplicate
        assert self.channel._size(new.name) == 1
        assert self.channel.basic_get(new.name).payload == {'id': 6}
        assert self.channel.basic_get(new.name) is None
