from __future__ import annotations

from unittest.mock import Mock

import pytest

from kombu import Connection
from kombu.transport.virtual import exchange
from t.mocks import Transport


class ExchangeCase:
    type = None

    def setup_method(self):
        if self.type:
            self.e = self.type(Connection(transport=Transport).channel())


class test_Direct(ExchangeCase):
    type = exchange.DirectExchange
    table = [('rFoo', None, 'qFoo'),
             ('rFoo', None, 'qFox'),
             ('rBar', None, 'qBar'),
             ('rBaz', None, 'qBaz')]

    @pytest.mark.parametrize('exchange,routing_key,default,expected', [
        ('eFoo', 'rFoo', None, {'qFoo', 'qFox'}),
        ('eMoz', 'rMoz', 'DEFAULT', set()),
        ('eBar', 'rBar', None, {'qBar'}),
    ])
    def test_lookup(self, exchange, routing_key, default, expected):
        assert self.e.lookup(
            self.table, exchange, routing_key, default) == expected


class test_Fanout(ExchangeCase):
    type = exchange.FanoutExchange
    table = [(None, None, 'qFoo'),
             (None, None, 'qFox'),
             (None, None, 'qBar')]

    def test_lookup(self):
        assert self.e.lookup(self.table, 'eFoo', 'rFoo', None) == {
            'qFoo', 'qFox', 'qBar',
        }

    def test_deliver_when_fanout_supported(self):
        self.e.channel = Mock()
        self.e.channel.supports_fanout = True
        message = Mock()

        self.e.deliver(message, 'exchange', 'rkey')
        self.e.channel._put_fanout.assert_called_with(
            'exchange', message, 'rkey',
        )

    def test_deliver_when_fanout_unsupported(self):
        self.e.channel = Mock()
        self.e.channel.supports_fanout = False

        self.e.deliver(Mock(), 'exchange', None)
        self.e.channel._put_fanout.assert_not_called()


class test_Topic(ExchangeCase):
    type = exchange.TopicExchange
    table = [
        ('stock.#', None, 'rFoo'),
        ('stock.us.*', None, 'rBar'),
    ]

    def setup_method(self):
        super().setup_method()
        self.table = [(rkey, self.e.key_to_pattern(rkey), queue)
                      for rkey, _, queue in self.table]

    def test_prepare_bind(self):
        x = self.e.prepare_bind('qFoo', 'eFoo', 'stock.#', {})
        assert x == ('stock.#', ('stock', '#'), 'qFoo')

    @pytest.mark.parametrize('exchange,routing_key,default,expected', [
        ('eFoo', 'stock.us.nasdaq', None, {'rFoo', 'rBar'}),
        ('eFoo', 'stock.europe.OSE', None, {'rFoo'}),
        ('eFoo', 'stockxeuropexOSE', None, set()),
        ('eFoo', 'candy.schleckpulver.snap_crackle', None, set()),
        # '*' is a single word: only 'stock.#' may match a longer key.
        ('eFoo', 'stock.us.nasdaq.tech', None, {'rFoo'}),
        # '#' matches zero words as well (AMQP/RabbitMQ topic semantics).
        ('eFoo', 'stock', None, {'rFoo'}),
    ])
    def test_lookup(self, exchange, routing_key, default, expected):
        assert self.e.lookup(
            self.table, exchange, routing_key, default) == expected
        assert self.e._compiled

    def test_deliver(self):
        self.e.channel = Mock()
        self.e.channel._lookup.return_value = ('a', 'b')
        message = Mock()
        self.e.deliver(message, 'exchange', 'rkey')

        assert self.e.channel._put.call_args_list == [
            (('a', message), {}),
            (('b', message), {}),
        ]


class test_TopicMultibind(ExchangeCase):
    # Testing message delivery in case of multiple overlapping
    # bindings for the same queue. As AMQP states, in case of
    # overlapping bindings, a message must be delivered once to
    # each matching queue.
    type = exchange.TopicExchange
    table = [
        ('stock', None, 'rFoo'),
        ('stock.#', None, 'rFoo'),
        ('stock.us.*', None, 'rFoo'),
        ('#', None, 'rFoo'),
    ]

    def setup_method(self):
        super().setup_method()
        self.table = [(rkey, self.e.key_to_pattern(rkey), queue)
                      for rkey, _, queue in self.table]

    @pytest.mark.parametrize('exchange,routing_key,default,expected', [
        ('eFoo', 'stock.us.nasdaq', None, {'rFoo'}),
        ('eFoo', 'stock.europe.OSE', None, {'rFoo'}),
        ('eFoo', 'stockxeuropexOSE', None, {'rFoo'}),
        ('eFoo', 'candy.schleckpulver.snap_crackle', None, {'rFoo'}),
    ])
    def test_lookup(self, exchange, routing_key, default, expected):
        assert self.e._compiled
        assert self.e.lookup(
            self.table, exchange, routing_key, default) == expected


class test_TopicMatching(ExchangeCase):
    # The topic exchange must follow AMQP/RabbitMQ topic semantics:
    # '*' matches exactly one (non-empty) word, '#' matches zero or more
    # words, and words are separated by dots.
    type = exchange.TopicExchange

    @pytest.mark.parametrize('binding,routing_key,matches', [
        # exact words
        ('stock.us.nasdaq', 'stock.us.nasdaq', True),
        ('stock.us.nasdaq', 'stock.us.nyse', False),
        # '*' matches exactly one word
        ('stock.*.nasdaq', 'stock.us.nasdaq', True),
        ('stock.*', 'stock.us', True),
        ('stock.*', 'stock', False),
        ('stock.*', 'stock.us.nasdaq', False),
        ('*', 'stock', True),
        ('*', 'stock.us', False),
        ('*', '', False),
        # '#' matches zero or more words, including a leading/trailing '#'
        ('stock.#', 'stock', True),
        ('stock.#', 'stock.us', True),
        ('stock.#', 'stock.us.nasdaq.tech', True),
        ('stock.#', 'bond', False),
        ('#.nasdaq', 'nasdaq', True),
        ('#.nasdaq', 'stock.us.nasdaq', True),
        ('#.nasdaq', 'nasdaq.stock', False),
        ('#', '', True),
        ('#', 'stock.us.nasdaq', True),
        # '#' in the middle, including the zero-words case and backtracking
        ('stock.#.nasdaq', 'stock.nasdaq', True),
        ('stock.#.nasdaq', 'stock.us.nasdaq', True),
        ('stock.#.nasdaq', 'stock.eu.de.nasdaq', True),
        ('stock.#.nasdaq', 'stock.us.nyse', False),
        ('#.a.#', 'a', True),
        ('#.a.#', 'x.a', True),
        ('#.a.#', 'a.y', True),
        ('#.a.#', 'x.a.y', True),
        ('#.a.#', 'x.y', False),
        ('#.#', '', True),
        ('#.#', 'stock', True),
        ('#.#', 'stock.us', True),
        # empty segments: a literal empty word only matches an empty word
        ('', '', True),
        ('', 'stock', False),
        ('a..b', 'a..b', True),
        ('a..b', 'a.x.b', False),
        ('.', '.', True),
        ('.', '', False),
        ('a.', 'a.', True),
        ('.b', '.b', True),
        # wildcard '*' never matches an empty word, '#' does
        ('a.*', 'a.', False),
        ('a.#', 'a.', True),
        ('a.#.b', 'a..b', True),
        # regular regex metacharacters are treated as literal characters
        ('a+b', 'a+b', True),
        ('a+b', 'aab', False),
        ('a(b)', 'a(b)', True),
        ('a(b)', 'ab', False),
    ])
    def test_match(self, binding, routing_key, matches):
        pattern = self.e.key_to_pattern(binding)
        assert self.e._match(pattern, routing_key) is matches

    def test_lookup_deduplicates_overlapping_bindings(self):
        # Several bindings of the same queue matching the same key must
        # produce the queue only once.
        table = [
            (rkey, self.e.key_to_pattern(rkey), 'q')
            for rkey in ('#', 'stock.#', 'stock.us.*', 'stock.us.#')
        ]
        table += [
            ('other.*', self.e.key_to_pattern('other.*'), 'q2'),
        ]
        assert self.e.lookup(
            table, 'e', 'stock.us.nasdaq', None) == {'q'}

    def test_key_to_pattern_is_cached(self):
        first = self.e.key_to_pattern('stock.#')
        second = self.e.key_to_pattern('stock.#')
        assert first is second
        assert first == ('stock', '#')


class test_ExchangeType(ExchangeCase):
    type = exchange.ExchangeType

    def test_lookup(self):
        with pytest.raises(NotImplementedError):
            self.e.lookup([], 'eFoo', 'rFoo', None)

    def test_prepare_bind(self):
        assert self.e.prepare_bind('qFoo', 'eFoo', 'rFoo', {}) == (
            'rFoo', None, 'qFoo',
        )

    e1 = {
        'type': 'direct',
        'durable': True,
        'auto_delete': True,
        'arguments': {},
    }
    e2 = dict(e1, arguments={'expires': 3000})

    @pytest.mark.parametrize('ex,eq,name,type,durable,auto_delete,arguments', [
        (e1, True, 'eFoo', 'direct', True, True, {}),
        (e1, False, 'eFoo', 'topic', True, True, {}),
        (e1, False, 'eFoo', 'direct', False, True, {}),
        (e1, False, 'eFoo', 'direct', True, False, {}),
        (e1, False, 'eFoo', 'direct', True, True, {'expires': 3000}),
        (e2, True, 'eFoo', 'direct', True, True, {'expires': 3000}),
        (e2, False, 'eFoo', 'direct', True, True, {'expires': 6000}),
    ])
    def test_equivalent(
            self, ex, eq, name, type, durable, auto_delete, arguments):
        is_eq = self.e.equivalent(
            ex, name, type, durable, auto_delete, arguments)
        assert is_eq if eq else not is_eq
