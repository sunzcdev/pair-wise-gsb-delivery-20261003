"""Virtual AMQ Exchange.

Implementations of the standard exchanges defined
by the AMQ protocol  (excluding the `headers` exchange).
"""

from __future__ import annotations


class ExchangeType:
    """Base class for exchanges.

    Implements the specifics for an exchange type.

    Arguments:
    ---------
        channel (ChannelT): AMQ Channel.
    """

    type = None

    def __init__(self, channel):
        self.channel = channel

    def lookup(self, table, exchange, routing_key, default):
        """Lookup all queues matching `routing_key` in `exchange`.

        Returns
        -------
            str: queue name, or 'default' if no queues matched.
        """
        raise NotImplementedError('subclass responsibility')

    def prepare_bind(self, queue, exchange, routing_key, arguments):
        """Prepare queue-binding.

        Returns
        -------
            Tuple[str, object, str]: of `(routing_key, pattern, queue)`
                to be stored for bindings to this exchange.  The pattern
                is exchange specific: ``None`` for the direct exchange,
                and a tuple of words for the topic exchange.
        """
        return routing_key, None, queue

    def equivalent(self, prev, exchange, type,
                   durable, auto_delete, arguments):
        """Return true if `prev` and `exchange` is equivalent."""
        return (type == prev['type'] and
                durable == prev['durable'] and
                auto_delete == prev['auto_delete'] and
                (arguments or {}) == (prev['arguments'] or {}))


class DirectExchange(ExchangeType):
    """Direct exchange.

    The `direct` exchange routes based on exact routing keys.
    """

    type = 'direct'

    def lookup(self, table, exchange, routing_key, default):
        return {
            queue for rkey, _, queue in table
            if rkey == routing_key
        }

    def deliver(self, message, exchange, routing_key, **kwargs):
        _lookup = self.channel._lookup
        _put = self.channel._put
        for queue in _lookup(exchange, routing_key):
            _put(queue, message, **kwargs)


class TopicExchange(ExchangeType):
    """Topic exchange.

    The `topic` exchange routes messages based on words separated by
    dots, using wildcard characters ``*`` (exactly one word), and ``#``
    (zero or more words).
    """

    type = 'topic'

    #: wildcard words and their meaning.
    STAR = '*'   #: matches exactly one word.
    HASH = '#'   #: matches zero or more words.

    #: cache of binding key -> tuple of words used for matching.
    _compiled = {}

    def lookup(self, table, exchange, routing_key, default):
        return {
            queue for rkey, words, queue in table
            if self._match(words, routing_key)
        }

    def deliver(self, message, exchange, routing_key, **kwargs):
        _lookup = self.channel._lookup
        _put = self.channel._put
        deadletter = self.channel.deadletter_queue
        for queue in [q for q in _lookup(exchange, routing_key)
                      if q and q != deadletter]:
            _put(queue, message, **kwargs)

    def prepare_bind(self, queue, exchange, routing_key, arguments):
        return routing_key, self.key_to_pattern(routing_key), queue

    def key_to_pattern(self, rkey):
        """Get the matching pattern (tuple of words) for a routing key.

        The binding key is split on dots into its words.  Literal words
        keep their literal meaning while ``*`` and ``#`` stay available
        as wildcard words for :meth:`_match`.  The resulting tuple is
        cached and reused for subsequent bindings with the same key.
        """
        try:
            return self._compiled[rkey]
        except KeyError:
            pattern = tuple(rkey.split('.'))
            self._compiled[rkey] = pattern
            return pattern

    def _match(self, pattern, string):
        """Match a message routing key against a binding pattern.

        Arguments:
        ---------
            pattern (Tuple[str, ...]): words of the binding routing key,
                as produced by :meth:`key_to_pattern`.  ``*`` matches
                exactly one word and ``#`` matches zero or more words.
            string (str): the message routing key, a dot separated list
                of words.

        Returns:
        -------
            bool: :const:`True` when the routing key matches the binding.
        """
        words = string.split('.')
        # ``pi``/``wi`` walk the pattern and routing key words.  When a
        # ``#`` is seen, remember its position and let it match zero words
        # first; backtrack by extending it one word whenever the rest of
        # the pattern fails to line up.
        pi = wi = 0
        hash_pi = -1
        hash_wi = -1
        plen, wlen = len(pattern), len(words)
        while wi < wlen:
            if pi < plen and pattern[pi] == self.HASH:
                hash_pi = pi
                hash_wi = wi
                pi += 1
            elif pi < plen and pattern[pi] == words[wi]:
                # a literal word (including an empty word) matches itself.
                pi += 1
                wi += 1
            elif pi < plen and pattern[pi] == self.STAR and words[wi]:
                # '*' matches exactly one, non-empty word.
                pi += 1
                wi += 1
            elif hash_pi != -1:
                # backtrack: the previous '#' consumes one more word.
                pi = hash_pi + 1
                hash_wi += 1
                wi = hash_wi
            else:
                return False
        # remaining pattern words may only be '#' (matching zero words).
        return all(word == self.HASH for word in pattern[pi:])


class FanoutExchange(ExchangeType):
    """Fanout exchange.

    The `fanout` exchange implements broadcast messaging by delivering
    copies of all messages to all queues bound to the exchange.

    To support fanout the virtual channel needs to store the table
    as shared state.  This requires that the `Channel.supports_fanout`
    attribute is set to true, and the `Channel._queue_bind` and
    `Channel.get_table` methods are implemented.

    See Also
    --------
        the redis backend for an example implementation of these methods.
    """

    type = 'fanout'

    def lookup(self, table, exchange, routing_key, default):
        return {queue for _, _, queue in table}

    def deliver(self, message, exchange, routing_key, **kwargs):
        if self.channel.supports_fanout:
            self.channel._put_fanout(
                exchange, message, routing_key, **kwargs)


#: Map of standard exchange types and corresponding classes.
STANDARD_EXCHANGE_TYPES = {
    'direct': DirectExchange,
    'topic': TopicExchange,
    'fanout': FanoutExchange,
}
