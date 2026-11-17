from __future__ import annotations

import pickle
from unittest.mock import Mock, call

import pytest

from kombu import Connection, Exchange, Producer, Queue, binding
from kombu.abstract import MaybeChannelBound
from kombu.exceptions import NotBoundError
from kombu.serialization import registry
from t.mocks import Transport


def get_conn() -> Connection:
    return Connection(transport=Transport)


class test_binding:

    def test_constructor(self) -> None:
        x = binding(
            Exchange('foo'), 'rkey',
            arguments={'barg': 'bval'},
            unbind_arguments={'uarg': 'uval'},
        )
        assert x.exchange == Exchange('foo')
        assert x.routing_key == 'rkey'
        assert x.arguments == {'barg': 'bval'}
        assert x.unbind_arguments == {'uarg': 'uval'}

    def test_declare(self) -> None:
        chan = get_conn().channel()
        x = binding(Exchange('foo'), 'rkey')
        x.declare(chan)
        assert 'exchange_declare' in chan

    def test_declare_no_exchange(self) -> None:
        chan = get_conn().channel()
        x = binding()
        x.declare(chan)
        assert 'exchange_declare' not in chan

    def test_bind(self) -> None:
        chan = get_conn().channel()
        x = binding(Exchange('foo'))
        x.bind(Exchange('bar')(chan))
        assert 'exchange_bind' in chan

    def test_unbind(self) -> None:
        chan = get_conn().channel()
        x = binding(Exchange('foo'))
        x.unbind(Exchange('bar')(chan))
        assert 'exchange_unbind' in chan

    def test_repr(self) -> None:
        b = binding(Exchange('foo'), 'rkey')
        assert 'foo' in repr(b)
        assert 'rkey' in repr(b)

    def test_declaration_identity(self) -> None:
        b1 = binding(Exchange('foo'), 'rkey', arguments={'x': 1})
        b2 = binding(Exchange('foo'), 'rkey', arguments={'x': 1})
        assert b1.declaration_identity() == b2.declaration_identity()
        assert hash(b1) == hash(b2)

        assert b1.declaration_identity() != binding(
            Exchange('bar'), 'rkey', arguments={'x': 1},
        ).declaration_identity()
        assert b1.declaration_identity() != binding(
            Exchange('foo'), 'other', arguments={'x': 1},
        ).declaration_identity()
        assert b1.declaration_identity() != binding(
            Exchange('foo'), 'rkey', arguments={'x': 2},
        ).declaration_identity()

    def test_declaration_identity_nested_arguments(self) -> None:
        args = {'nested': {'k': [1, 2]}}
        b1 = binding(Exchange('foo'), 'rkey', arguments=args)
        b2 = binding(
            Exchange('foo'), 'rkey', arguments={'nested': {'k': [1, 2]}},
        )
        assert b1.declaration_identity() == b2.declaration_identity()
        # The original configuration is left untouched.
        assert args == {'nested': {'k': [1, 2]}}

    def test_can_cache_declaration(self) -> None:
        assert binding(Exchange('foo')).can_cache_declaration

    def test_equality_matches_identity(self) -> None:
        b1 = binding(Exchange('foo'), 'rkey', arguments={'x': 1})
        b2 = binding(Exchange('foo'), 'rkey', arguments={'x': 1})
        assert b1 == b2
        assert hash(b1) == hash(b2)
        assert b1 != binding(Exchange('foo'), 'rkey', arguments={'x': 2})
        assert b1.__eq__(True) is NotImplemented

    def test_duplicate_bindings_dedupe_in_queue(self) -> None:
        b1 = binding(Exchange('e'), 'rk', arguments={'x': 1})
        b2 = binding(Exchange('e'), 'rk', arguments={'x': 1})
        queue = Queue('q', bindings=[b1, b2])
        assert len(queue.bindings) == 1

    def test_bind_to_channel(self) -> None:
        # A channel argument binds the declaration instead of issuing a
        # broker bind.
        chan = get_conn().channel()
        b = binding(Exchange('foo'))
        bound = b.bind(chan)
        assert bound.is_bound
        assert 'exchange_bind' not in chan


class test_Exchange:

    def test_bound(self) -> None:
        exchange = Exchange('foo', 'direct')
        assert not exchange.is_bound
        assert '<unbound' in repr(exchange)

        chan = get_conn().channel()
        bound = exchange.bind(chan)
        assert bound.is_bound
        assert bound.channel is chan
        assert f'bound to chan:{chan.channel_id!r}' in repr(bound)

    def test_hash(self) -> None:
        assert hash(Exchange('a')) == hash(Exchange('a'))
        assert hash(Exchange('a')) != hash(Exchange('b'))

    def test_declaration_identity_distinguishes_arguments(self) -> None:
        # Same name, different broker-facing configuration must yield
        # different declaration identities.
        base = Exchange('a', 'direct')
        assert base.declaration_identity() == Exchange('a', 'direct') \
            .declaration_identity()
        assert base.declaration_identity() != \
            Exchange('a', 'topic').declaration_identity()
        assert base.declaration_identity() != Exchange(
            'a', 'direct', durable=False).declaration_identity()
        assert base.declaration_identity() != Exchange(
            'a', 'direct', auto_delete=True).declaration_identity()
        assert base.declaration_identity() != Exchange(
            'a', 'direct', arguments={'x': 1}).declaration_identity()

    def test_declaration_identity_compares_nested_arguments(self) -> None:
        left = Exchange('a', arguments={'nested': {'k': [1, 2]}})
        right = Exchange('a', arguments={'nested': {'k': [1, 2]}})
        other = Exchange('a', arguments={'nested': {'k': [1, 3]}})
        assert left.declaration_identity() == right.declaration_identity()
        assert left.declaration_identity() != other.declaration_identity()
        # Building the identity must not mutate the original arguments.
        original = {'nested': {'k': [1, 2]}}
        Exchange('a', arguments=original).declaration_identity()
        assert original == {'nested': {'k': [1, 2]}}

    def test_declaration_identity_mixed_type_argument_keys(self) -> None:
        # Mapping keys of different types must not break the hashable
        # identity, and key order must not matter.
        left = Exchange('a', arguments={1: 'x', 'b': [2]})
        right = Exchange('a', arguments={'b': [2], 1: 'x'})
        assert left.declaration_identity() == right.declaration_identity()

    def test_can_cache_declaration(self) -> None:
        assert Exchange('a', durable=True).can_cache_declaration
        assert Exchange('a', durable=False).can_cache_declaration
        assert not Exchange('a', auto_delete=True).can_cache_declaration
        assert not Exchange(
            'a', durable=True, auto_delete=True,
        ).can_cache_declaration

    def test_pickle(self) -> None:
        e1 = Exchange('foo', 'direct')
        e2 = pickle.loads(pickle.dumps(e1))
        assert e1 == e2

    def test_eq(self) -> None:
        e1 = Exchange('foo', 'direct')
        e2 = Exchange('foo', 'direct')
        assert e1 == e2

        e3 = Exchange('foo', 'topic')
        assert e1 != e3

        assert e1.__eq__(True) == NotImplemented

    def test_revive(self) -> None:
        exchange = Exchange('foo', 'direct')
        conn = get_conn()
        chan = conn.channel()

        # reviving unbound channel is a noop.
        exchange.revive(chan)
        assert not exchange.is_bound
        assert exchange._channel is None

        bound = exchange.bind(chan)
        assert bound.is_bound
        assert bound.channel is chan

        chan2 = conn.channel()
        bound.revive(chan2)
        assert bound.is_bound
        assert bound._channel is chan2

    def test_assert_is_bound(self) -> None:
        exchange = Exchange('foo', 'direct')
        with pytest.raises(NotBoundError):
            exchange.declare()
        conn = get_conn()

        chan = conn.channel()
        exchange.bind(chan).declare()
        assert 'exchange_declare' in chan

    def test_set_transient_delivery_mode(self) -> None:
        exc = Exchange('foo', 'direct', delivery_mode='transient')
        assert exc.delivery_mode == Exchange.TRANSIENT_DELIVERY_MODE

    def test_set_passive_mode(self) -> None:
        exc = Exchange('foo', 'direct', passive=True)
        assert exc.passive

    def test_set_persistent_delivery_mode(self) -> None:
        exc = Exchange('foo', 'direct', delivery_mode='persistent')
        assert exc.delivery_mode == Exchange.PERSISTENT_DELIVERY_MODE

    def test_bind_at_instantiation(self) -> None:
        assert Exchange('foo', channel=get_conn().channel()).is_bound

    def test_create_message(self) -> None:
        chan = get_conn().channel()
        Exchange('foo', channel=chan).Message({'foo': 'bar'})
        assert 'prepare_message' in chan

    def test_create_message_delivery_mode(self) -> None:
        chan = get_conn().channel()
        exchange = Exchange('foo', channel=chan, delivery_mode='persistent')

        # an explicit delivery_mode overrides the exchange default
        message = exchange.Message({'foo': 'bar'}, delivery_mode='transient')
        assert message['properties']['delivery_mode'] == \
            Exchange.TRANSIENT_DELIVERY_MODE

        # without one, the exchange default is used
        message = exchange.Message({'foo': 'bar'})
        assert message['properties']['delivery_mode'] == \
            Exchange.PERSISTENT_DELIVERY_MODE

    def test_publish(self) -> None:
        chan = get_conn().channel()
        Exchange('foo', channel=chan).publish('the quick brown fox')
        assert 'basic_publish' in chan

    def test_delete(self) -> None:
        chan = get_conn().channel()
        Exchange('foo', channel=chan).delete()
        assert 'exchange_delete' in chan

    def test__repr__(self) -> None:
        b = Exchange('foo', 'topic')
        assert 'foo(topic)' in repr(b)
        assert 'Exchange' in repr(b)

    def test_bind_to(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('foo', 'topic')
        bar = Exchange('bar', 'topic')
        foo(chan).bind_to(bar)
        assert 'exchange_bind' in chan

    def test_bind_to_by_name(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('foo', 'topic')
        foo(chan).bind_to('bar')
        assert 'exchange_bind' in chan

    def test_unbind_from(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('foo', 'topic')
        bar = Exchange('bar', 'topic')
        foo(chan).unbind_from(bar)
        assert 'exchange_unbind' in chan

    def test_unbind_from_by_name(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('foo', 'topic')
        foo(chan).unbind_from('bar')
        assert 'exchange_unbind' in chan

    def test_declare__no_declare(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('foo', 'topic', no_declare=True)
        foo(chan).declare()
        assert 'exchange_declare' not in chan

    def test_declare__internal_exchange(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('amq.rabbitmq.trace', 'topic')
        foo(chan).declare()
        assert 'exchange_declare' not in chan

    def test_declare(self) -> None:
        chan = get_conn().channel()
        foo = Exchange('foo', 'topic', no_declare=False)
        foo(chan).declare()
        assert 'exchange_declare' in chan


class test_Queue:

    def setup_method(self) -> None:
        self.exchange = Exchange('foo', 'direct')

    def test_constructor_with_actual_exchange(self) -> None:
        exchange = Exchange('exchange_name', 'direct')
        queue = Queue(name='queue_name', exchange=exchange)
        assert queue.exchange == exchange

    def test_constructor_with_string_exchange(self) -> None:
        exchange_name = 'exchange_name'
        queue = Queue(name='queue_name', exchange=exchange_name)
        assert queue.exchange == Exchange(exchange_name)

    def test_constructor_with_default_exchange(self) -> None:
        queue = Queue(name='queue_name')
        assert queue.exchange == Exchange('')

    def test_hash(self) -> None:
        assert hash(Queue('a')) == hash(Queue('a'))
        assert hash(Queue('a')) != hash(Queue('b'))

    def test_declaration_identity_same_name_different_arguments(self) -> None:
        # A queue name alone must not determine the declaration identity.
        base = Queue('a', exchange=Exchange('e'), routing_key='rk')
        assert base.declaration_identity() == Queue(
            'a', exchange=Exchange('e'), routing_key='rk',
        ).declaration_identity()

        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('e'), routing_key='rk', durable=False,
        ).declaration_identity()
        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('e'), routing_key='rk', exclusive=True,
        ).declaration_identity()
        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('e'), routing_key='rk',
            queue_arguments={'x-max-length': 100},
        ).declaration_identity()
        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('e'), routing_key='rk',
            binding_arguments={'x-match': 'all'},
        ).declaration_identity()
        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('e'), routing_key='rk', message_ttl=30,
        ).declaration_identity()
        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('other'), routing_key='rk',
        ).declaration_identity()
        assert base.declaration_identity() != Queue(
            'a', exchange=Exchange('e'), routing_key='other',
        ).declaration_identity()

    def test_declaration_identity_nested_arguments(self) -> None:
        args = {'nested': {'k': [1, 2]}}
        q1 = Queue('a', queue_arguments=args)
        q2 = Queue('a', queue_arguments={'nested': {'k': [1, 2]}})
        q3 = Queue('a', queue_arguments={'nested': {'k': [1, 3]}})
        assert q1.declaration_identity() == q2.declaration_identity()
        assert q1.declaration_identity() != q3.declaration_identity()
        # Original configuration must not be mutated.
        assert args == {'nested': {'k': [1, 2]}}

    def test_repr_with_bindings(self) -> None:
        ex = Exchange('foo')
        x = Queue('foo', bindings=[ex.binding('A'), ex.binding('B')])
        assert repr(x)

    def test_anonymous(self) -> None:
        chan = Mock()
        x = Queue(bindings=[binding(Exchange('foo'), 'rkey')])
        chan.queue_declare.return_value = 'generated', 0, 0
        xx = x(chan)
        xx.declare()
        assert xx.name == 'generated'

    def test_basic_get__accept_disallowed(self) -> None:
        conn = Connection('memory://')
        q = Queue('foo', exchange=self.exchange)
        p = Producer(conn)
        p.publish(
            {'complex': object()},
            declare=[q], exchange=self.exchange, serializer='pickle',
        )

        message = q(conn).get(no_ack=True)
        assert message is not None

        with pytest.raises(q.ContentDisallowed):
            message.decode()

    def test_basic_get__accept_allowed(self) -> None:
        conn = Connection('memory://')
        q = Queue('foo', exchange=self.exchange)
        p = Producer(conn)
        p.publish(
            {'complex': object()},
            declare=[q], exchange=self.exchange, serializer='pickle',
        )

        message = q(conn).get(accept=['pickle'], no_ack=True)
        assert message is not None

        payload = message.decode()
        assert payload['complex']

    def test_when_bound_but_no_exchange(self) -> None:
        q = Queue('a')
        q.exchange = None
        assert q.when_bound() is None

    def test_declare_but_no_exchange(self) -> None:
        q = Queue('a')
        q.queue_declare = Mock()
        q.queue_bind = Mock()
        q.exchange = None

        q.declare()
        q.queue_declare.assert_called_with(
            channel=None, nowait=False, passive=False)

    def test_declare__no_declare(self) -> None:
        q = Queue('a', no_declare=True)
        q.queue_declare = Mock()
        q.queue_bind = Mock()
        q.exchange = None

        q.declare()
        q.queue_declare.assert_not_called()
        q.queue_bind.assert_not_called()

    def test_bind_to_when_name(self) -> None:
        chan = Mock()
        q = Queue('a')
        q(chan).bind_to('ex')
        chan.queue_bind.assert_called()

    def test_get_when_no_m2p(self) -> None:
        chan = Mock()
        q = Queue('a')(chan)
        chan.message_to_python = None
        assert q.get()

    def test_multiple_bindings(self) -> None:
        chan = Mock()
        q = Queue('mul', [
            binding(Exchange('mul1'), 'rkey1'),
            binding(Exchange('mul2'), 'rkey2'),
            binding(Exchange('mul3'), 'rkey3'),
        ])
        q(chan).declare()
        assert call(
            nowait=False,
            exchange='mul1',
            auto_delete=False,
            passive=False,
            arguments=None,
            type='direct',
            durable=True,
        ) in chan.exchange_declare.call_args_list

    def test_can_cache_declaration(self) -> None:
        assert Queue('a', durable=True).can_cache_declaration
        assert Queue('a', durable=False).can_cache_declaration
        assert not Queue(
            'a', queue_arguments={'x-expires': 100}
        ).can_cache_declaration

    def test_eq(self) -> None:
        q1 = Queue('xxx', Exchange('xxx', 'direct'), 'xxx')
        q2 = Queue('xxx', Exchange('xxx', 'direct'), 'xxx')
        assert q1 == q2
        assert q1.__eq__(True) == NotImplemented

        q3 = Queue('yyy', Exchange('xxx', 'direct'), 'xxx')
        assert q1 != q3

    def test_exclusive_implies_auto_delete(self) -> None:
        assert Queue('foo', self.exchange, exclusive=True).auto_delete

    def test_binds_at_instantiation(self) -> None:
        assert Queue('foo', self.exchange,
                     channel=get_conn().channel()).is_bound

    def test_also_binds_exchange(self) -> None:
        chan = get_conn().channel()
        b = Queue('foo', self.exchange)
        assert not b.is_bound
        assert not b.exchange.is_bound
        b = b.bind(chan)
        assert b.is_bound
        assert b.exchange.is_bound
        assert b.channel is b.exchange.channel
        assert b.exchange is not self.exchange

    def test_declare(self) -> None:
        chan = get_conn().channel()
        b = Queue('foo', self.exchange, 'foo', channel=chan)
        assert b.is_bound
        b.declare()
        assert 'exchange_declare' in chan
        assert 'queue_declare' in chan
        assert 'queue_bind' in chan

    def test_get(self) -> None:
        b = Queue('foo', self.exchange, 'foo', channel=get_conn().channel())
        b.get()
        assert 'basic_get' in b.channel

    def test_purge(self) -> None:
        b = Queue('foo', self.exchange, 'foo', channel=get_conn().channel())
        b.purge()
        assert 'queue_purge' in b.channel

    def test_consume(self) -> None:
        b = Queue('foo', self.exchange, 'foo', channel=get_conn().channel())
        b.consume('fifafo', None)
        assert 'basic_consume' in b.channel

    def test_consume_with_callbacks(self) -> None:
        chan = Mock()
        b = Queue('foo', self.exchange, 'foo', channel=chan)
        callback = Mock()
        on_cancel = Mock()
        b.consume('fifafo', callback=callback, on_cancel=on_cancel)
        chan.basic_consume.assert_called_with(
            queue='foo',
            no_ack=False,
            consumer_tag='fifafo',
            callback=callback,
            nowait=False,
            arguments=None,
            on_cancel=on_cancel
        )

    def test_cancel(self) -> None:
        b = Queue('foo', self.exchange, 'foo', channel=get_conn().channel())
        b.cancel('fifafo')
        assert 'basic_cancel' in b.channel

    def test_delete(self) -> None:
        b = Queue('foo', self.exchange, 'foo', channel=get_conn().channel())
        b.delete()
        assert 'queue_delete' in b.channel

    def test_queue_unbind(self) -> None:
        b = Queue('foo', self.exchange, 'foo', channel=get_conn().channel())
        b.queue_unbind()
        assert 'queue_unbind' in b.channel

    def test_as_dict(self) -> None:
        q = Queue('foo', self.exchange, 'rk')
        d = q.as_dict(recurse=True)
        assert d['exchange']['name'] == self.exchange.name

    def test_as_dict_from_dict_roundtrip(self) -> None:
        q = Queue(
            'foo', Exchange('fooex', type='topic'), 'rk',
            durable=False, exclusive=True, auto_delete=True, no_ack=True,
            alias='fooalias', no_declare=True,
            expires=30.0, message_ttl=20.0,
            max_length=100, max_length_bytes=1000, max_priority=9,
            queue_arguments={'x-queue-mode': 'lazy'},
            binding_arguments={'x-match': 'all'},
            consumer_arguments={'x-priority': 5},
        )
        d = q.as_dict(recurse=True)
        # ``from_dict`` describes the exchange by name/type, not by object.
        d['exchange'], d['exchange_type'] = q.exchange.name, q.exchange.type
        q2 = Queue.from_dict(d['name'], **d)

        for attr, _ in Queue.attrs:
            if attr == 'exchange':
                continue
            assert getattr(q2, attr) == getattr(q, attr), attr
        assert q2.exchange.name == 'fooex'
        assert q2.exchange.type == 'topic'

    def test_from_dict_does_not_override_defaults(self) -> None:
        q = Queue.from_dict('foo', exchange='fooex')
        assert q.max_length is None
        assert q.expires is None
        assert q.durable
        assert not q.auto_delete

    def test_queue_dump(self) -> None:
        b = binding(self.exchange, 'rk')
        q = Queue('foo', self.exchange, 'rk', bindings=[b])
        d = q.as_dict(recurse=True)
        assert d['bindings'][0]['routing_key'] == 'rk'
        registry.dumps(d)

    def test__repr__(self) -> None:
        b = Queue('foo', self.exchange, 'foo')
        assert 'foo' in repr(b)
        assert 'Queue' in repr(b)


class test_MaybeChannelBound:

    def test_repr(self) -> None:
        assert repr(MaybeChannelBound())
