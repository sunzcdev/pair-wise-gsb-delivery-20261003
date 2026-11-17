"""Common Utilities."""

from __future__ import annotations

import os
import socket
import threading
from collections import deque, namedtuple
from contextlib import contextmanager
from functools import partial
from itertools import count
from uuid import NAMESPACE_OID, uuid3, uuid4, uuid5

from amqp import ChannelError, RecoverableConnectionError

from .entity import Exchange, Queue
from .log import get_logger
from .serialization import registry as serializers
from .utils.compat import get_gevent_concurrent_error
from .utils.uuid import uuid

__all__ = ('Broadcast', 'maybe_declare', 'uuid',
           'itermessages', 'send_reply',
           'collect_replies', 'insured', 'drain_consumer',
           'eventloop')

#: Prefetch count can't exceed short.
PREFETCH_COUNT_MAX = 0xFFFF

logger = get_logger(__name__)

_node_id = None


def get_node_id():
    global _node_id
    if _node_id is None:
        _node_id = uuid4().int
    return _node_id


def generate_oid(node_id, process_id, thread_id, instance):
    ent = '{:x}-{:x}-{:x}-{:x}'.format(
        node_id, process_id, thread_id, id(instance))
    try:
        ret = str(uuid3(NAMESPACE_OID, ent))
    except ValueError:
        ret = str(uuid5(NAMESPACE_OID, ent))
    return ret


def oid_from(instance, threads=True):
    return generate_oid(
        get_node_id(),
        os.getpid(),
        threading.get_ident() if threads else 0,
        instance,
    )


class Broadcast(Queue):
    """Broadcast queue.

    Convenience class used to define broadcast queues.

    Every queue instance will have a unique name,
    and both the queue and exchange is configured with auto deletion.

    Arguments:
    ---------
        name (str): This is used as the name of the exchange.
        queue (str): By default a unique id is used for the queue
            name for every consumer.  You can specify a custom
            queue name here.
        unique (bool): Always create a unique queue
            even if a queue name is supplied.
        ``**kwargs`` (Any): See :class:`~kombu.Queue` for a list
            of additional keyword arguments supported.
    """

    attrs = Queue.attrs + (('queue', None),)

    def __init__(self,
                 name=None,
                 queue=None,
                 unique=False,
                 auto_delete=True,
                 exchange=None,
                 alias=None,
                 **kwargs):
        if unique:
            queue = '{}.{}'.format(queue or 'bcast', uuid())
        else:
            queue = queue or f'bcast.{uuid()}'
        super().__init__(
            alias=alias or name,
            queue=queue,
            name=queue,
            auto_delete=auto_delete,
            exchange=(exchange if exchange is not None
                      else Exchange(name, type='fanout')),
            **kwargs
        )


def declaration_cached(entity, channel):
    return entity in channel.connection.client.declared_entities


def maybe_declare(entity, channel=None, retry=False, **retry_policy):
    """Declare entity (cached)."""
    if retry:
        return _imaybe_declare(entity, channel, **retry_policy)
    return _maybe_declare(entity, channel)


def _ensure_channel_is_bound(entity, channel):
    """Make sure the channel is bound to the entity.

    :param entity: generic kombu nomenclature, generally an exchange or queue
    :param channel: channel to bind to the entity
    :return: the updated entity
    """
    is_bound = entity.is_bound
    if not is_bound:
        if not channel:
            raise ChannelError(
                f"Cannot bind channel {channel} to entity {entity}")
        entity = entity.bind(channel)
    return entity


def _maybe_declare(entity, channel):
    # _maybe_declare sets name on original for autogen queues
    orig = entity

    _ensure_channel_is_bound(entity, channel)

    if channel is None or channel.connection is None:
        # If this was called from the `ensure()` method then the channel could have been invalidated
        # and the correct channel was re-bound to the entity by calling the `entity.revive()` method.
        if not entity.is_bound:
            raise ChannelError(
                f"channel is None and entity {entity} not bound.")
        channel = entity.channel

    declared = ident = None
    if channel.connection and entity.can_cache_declaration:
        declared = channel.connection.client.declared_entities
        ident = hash(entity)
        if ident in declared:
            return False

    if not channel.connection:
        raise RecoverableConnectionError('channel disconnected')
    entity.declare(channel=channel)
    if declared is not None and ident:
        declared.add(ident)
    if orig is not None:
        orig.name = entity.name
    return True


def _imaybe_declare(entity, channel, **retry_policy):
    entity = _ensure_channel_is_bound(entity, channel)

    if not entity.channel.connection and getattr(channel, 'connection', None):
        # The entity is still bound to a channel that was closed, e.g. when
        # a Producer.publish() retry passes the exchange it declared before
        # the reconnect: declare it on the channel we were given instead.
        entity.revive(channel)

    if not entity.channel.connection:
        raise RecoverableConnectionError('channel disconnected')

    return entity.channel.connection.client.ensure(
        entity, _maybe_declare, **retry_policy)(entity, channel)


def drain_consumer(consumer, limit=1, timeout=None, callbacks=None):
    """Drain messages from consumer instance."""
    acc = deque()

    def on_message(body, message):
        acc.append((body, message))

    consumer.callbacks = [on_message] + (callbacks or [])

    with consumer:
        for _ in eventloop(consumer.channel.connection.client,
                           limit=limit, timeout=timeout, ignore_timeouts=True):
            try:
                yield acc.popleft()
            except IndexError:
                pass


def itermessages(conn, channel, queue, limit=1, timeout=None,
                 callbacks=None, **kwargs):
    """Iterator over messages."""
    return drain_consumer(
        conn.Consumer(queues=[queue], channel=channel, **kwargs),
        limit=limit, timeout=timeout, callbacks=callbacks,
    )


def eventloop(conn, limit=None, timeout=None, ignore_timeouts=False):
    """Best practice generator wrapper around ``Connection.drain_events``.

    Able to drain events forever, with a limit, and optionally ignoring
    timeout errors (a timeout of 1 is often used in environments where
    the socket can get "stuck", and is a best practice for Kombu consumers).

    ``eventloop`` is a generator.

    Examples
    --------
        >>> from kombu.common import eventloop

        >>> def run(conn):
        ...     it = eventloop(conn, timeout=1, ignore_timeouts=True)
        ...     next(it)   # one event consumed, or timed out.
        ...
        ...     for _ in eventloop(conn, timeout=1, ignore_timeouts=True):
        ...         pass  # loop forever.

    It also takes an optional limit parameter, and timeout errors
    are propagated by default::

        for _ in eventloop(connection, limit=1, timeout=1):
            pass

    See Also
    --------
        :func:`itermessages`, which is an event loop bound to one or more
        consumers, that yields any messages received.
    """
    for i in limit and range(limit) or count():
        try:
            yield conn.drain_events(timeout=timeout)
        except socket.timeout:
            if timeout and not ignore_timeouts:  # pragma: no cover
                raise


def send_reply(exchange, req, msg,
               producer=None, retry=False, retry_policy=None, **props):
    """Send reply for request.

    Arguments:
    ---------
        exchange (kombu.Exchange, str): Reply exchange
        req (~kombu.Message): Original request, a message with
            a ``reply_to`` property.
        producer (kombu.Producer): Producer instance
        retry (bool): If true must retry according to
            the ``reply_policy`` argument.
        retry_policy (Dict): Retry settings.
        ``**props`` (Any): Extra properties.
    """
    return producer.publish(
        msg, exchange=exchange,
        retry=retry, retry_policy=retry_policy,
        **dict({'routing_key': req.properties['reply_to'],
                'correlation_id': req.properties.get('correlation_id'),
                'serializer': serializers.type_to_name[req.content_type],
                'content_encoding': req.content_encoding}, **props)
    )


def collect_replies(conn, channel, queue, *args, **kwargs):
    """Generator collecting replies from ``queue``."""
    no_ack = kwargs.setdefault('no_ack', True)
    received = False
    try:
        for body, message in itermessages(conn, channel, queue,
                                          *args, **kwargs):
            if not no_ack:
                message.ack()
            received = True
            yield body
    finally:
        if received:
            channel.after_reply_message_received(queue.name)


def _ensure_errback(exc, interval):
    logger.error(
        'Connection error: %r. Retry in %ss\n', exc, interval,
        exc_info=True,
    )


@contextmanager
def _ignore_errors(conn):
    try:
        yield
    except conn.connection_errors + conn.channel_errors + (
        (conc_err,) if (conc_err := get_gevent_concurrent_error()) is not None else ()
    ):
        pass


def ignore_errors(conn, fun=None, *args, **kwargs):
    """Ignore connection and channel errors.

    The first argument must be a connection object, or any other object
    with ``connection_error`` and ``channel_error`` attributes.

    Can be used as a function:

    .. code-block:: python

        def example(connection):
            ignore_errors(connection, consumer.channel.close)

    or as a context manager:

    .. code-block:: python

        def example(connection):
            with ignore_errors(connection):
                consumer.channel.close()


    Note:
    ----
        Connection and channel errors should be properly handled,
        and not ignored.  Using this function is only acceptable in a cleanup
        phase, like when a connection is lost or at shutdown.
    """
    if fun:
        with _ignore_errors(conn):
            return fun(*args, **kwargs)
    return _ignore_errors(conn)


def revive_connection(connection, channel, on_revive=None):
    if on_revive:
        on_revive(channel)


def insured(pool, fun, args, kwargs, errback=None, on_revive=None, **opts):
    """Function wrapper to handle connection errors.

    Ensures function performing broker commands completes
    despite intermittent connection failures.
    """
    errback = errback or _ensure_errback

    with pool.acquire(block=True) as conn:
        conn.ensure_connection(errback=errback)
        # we cache the channel for subsequent calls, this has to be
        # reset on revival.
        channel = conn.default_channel
        revive = partial(revive_connection, conn, on_revive=on_revive)
        insured = conn.autoretry(fun, channel, errback=errback,
                                 on_revive=revive, **opts)
        retval, _ = insured(*args, **dict(kwargs, connection=conn))
        return retval


class QoSState(namedtuple('QoSState', (
        'desired', 'sent', 'pending', 'applied', 'failures', 'last_error'))):
    """Snapshot of the QoS state for a single consumer or channel.

    The state tracks the local expectation separately from what was
    actually sent to the broker:

    Arguments:
    ---------
        desired (int): The prefetch count the local platform wants.
        sent (int or None): The last value accepted by the broker, or
            :const:`None` when nothing has been sent yet (or the channel
            was lost and the value has to be sent again).
        pending (int or None): The value that still has to be applied, or
            :const:`None` when nothing is outstanding.  A failed call keeps
            the value here so it is never reported as applied.
        applied (bool): :const:`True` when no value is pending.
        failures (int): Consecutive failed applies since the last success.
        last_error (Exception or None): The error raised by the most recent
            failed apply, if any.
    """

    __slots__ = ()

    @property
    def in_sync(self):
        """Return true if the broker accepted the current desired value.

        A clamped value (one exceeding the protocol limit) keeps its
        ``desired`` value, so ``sent`` differs from it and the state is
        not reported as in sync.  Use ``sent`` to see what actually went
        over the wire.
        """
        return (self.applied and self.sent is not None and
                self.sent == self.desired)


class QoS:
    """Thread safe increment/decrement of a channels prefetch_count.

    The object keeps the local expectation (``value``) apart from the
    value that was actually accepted by the broker (``prev``), so callers
    can inspect whether an update is still pending with :meth:`state`.
    Adjustments made with :meth:`increment_eventually` and
    :meth:`decrement_eventually` are coalesced: several changes made
    before the next :meth:`update` result in a single callback call.

    Each instance owns its own bookkeeping, so the prefetch counts of
    different consumers or channels never overwrite each other.  A zero
    value keeps its protocol meaning of "no limit": it is still sent
    once, and increment/decrement calls leave it untouched.

    A failed callback does not mark the value as applied: the desired
    value is kept pending, :attr:`failures` is incremented and the
    exception is propagated.  :meth:`reapply` sends the value again,
    which is what a consumer should do after the connection revived.

    Arguments:
    ---------
        callback (Callable): Function used to set new prefetch count,
            e.g. ``consumer.qos`` or ``channel.basic_qos``.  Will be called
            with a single ``prefetch_count`` keyword argument.
        initial_value (int): Initial prefetch count value..
        max_prefetch (int or None): Maximum allowed prefetch count. If specified
            as an integer, increment_eventually will not allow the value to exceed this limit.
            If None (the default), there is no upper limit on the prefetch count.

    Example:
    -------
        >>> from kombu import Consumer, Connection
        >>> connection = Connection('amqp://')
        >>> consumer = Consumer(connection)
        >>> qos = QoS(consumer.qos, initial_prefetch_count=2)
        >>> qos.update()  # set initial

        >>> qos.value
        2

        >>> def in_some_thread():
        ...     qos.increment_eventually()

        >>> def in_some_other_thread():
        ...     qos.decrement_eventually()

        >>> while 1:
        ...    if qos.state().pending is not None:
        ...        qos.update()  # prefetch changed so update.

    It can be used with any function supporting a ``prefetch_count`` keyword
    argument::

        >>> channel = connection.channel()
        >>> QoS(channel.basic_qos, 10)


        >>> def set_qos(prefetch_count):
        ...     print('prefetch count now: %r' % (prefetch_count,))
        >>> QoS(set_qos, 10)
    """

    def __init__(self, callback, initial_value, max_prefetch=None):
        self.callback = callback
        self._mutex = threading.RLock()
        self.value = initial_value or 0
        self.max_prefetch = max_prefetch
        #: last requested value accepted by the broker
        #: (``None`` before the first successful call).
        self.prev = None
        #: last value actually handed to the callback.  This may differ
        #: from :attr:`prev` when the value had to be clamped.
        self._sent = None
        self._failures = 0
        self._last_error = None

    @property
    def desired(self):
        """The prefetch count this object wants to be in effect."""
        return self.value

    @property
    def sent(self):
        """The last value accepted by the broker, or :const:`None`."""
        return self._sent

    @property
    def pending(self):
        """The value waiting to be applied, or :const:`None`."""
        with self._mutex:
            return None if self.prev == self.value else self.value

    @property
    def applied(self):
        """Return true if no update is waiting to be applied."""
        return self.pending is None

    @property
    def failures(self):
        """Number of consecutive failed applies since the last success."""
        return self._failures

    @property
    def last_error(self):
        """The exception raised by the most recent failed apply."""
        return self._last_error

    def state(self):
        """Return a :class:`QoSState` snapshot of this object.

        The snapshot makes it possible to check whether the local
        expectation matches the value that was sent to the broker, to see
        the value still pending (for example after a failure), and to
        inspect the last error.
        """
        with self._mutex:
            pending = None if self.prev == self.value else self.value
            return QoSState(
                desired=self.value,
                sent=self._sent,
                pending=pending,
                applied=pending is None,
                failures=self._failures,
                last_error=self._last_error,
            )

    def increment_eventually(self, n=1):
        """Increment the value, but do not update the channels QoS.

        Note:
        ----
            The MainThread will be responsible for calling :meth:`update`
            when necessary. If max_prefetch is set, the value will not
            exceed this limit.

            Several increments made before the next :meth:`update` are
            coalesced into a single call, and a zero value (no limit) is
            left untouched.
        """
        with self._mutex:
            if self.value:
                new_value = self.value + max(n, 0)
                if self.max_prefetch is not None and new_value > self.max_prefetch:
                    new_value = self.max_prefetch
                self.value = new_value
        return self.value

    def decrement_eventually(self, n=1):
        """Decrement the value, but do not update the channels QoS.

        Note:
        ----
            The MainThread will be responsible for calling :meth:`update`
            when necessary.

            As with :meth:`increment_eventually`, a zero value keeps its
            protocol meaning and is not decremented.
        """
        with self._mutex:
            if self.value:
                self.value -= n
                if self.value < 1:
                    self.value = 1
        return self.value

    def request(self, pcount):
        """Record a new desired value without sending it yet.

        The value only becomes pending; a later :meth:`update` (or
        :meth:`reapply`) is what actually calls the broker.  This is what
        makes it possible to merge several adjustments made in one round
        into a single call.
        """
        with self._mutex:
            self.value = pcount
        return self.value

    def set(self, pcount):
        """Set channel prefetch_count setting.

        The value is recorded as the new local expectation, and only
        marked as applied once ``callback`` returns successfully.  If the
        callback raises, the value is kept pending so it can be retried
        with :meth:`update` or :meth:`reapply`, and the exception is
        propagated to the caller.
        """
        with self._mutex:
            self.value = pcount
            if pcount == self.prev:
                return pcount
            new_value = pcount
            if pcount > PREFETCH_COUNT_MAX:
                logger.warning('QoS: Disabled: prefetch_count exceeds %r',
                               PREFETCH_COUNT_MAX)
                new_value = 0
            logger.debug('basic.qos: prefetch_count->%s', new_value)
            try:
                self.callback(prefetch_count=new_value)
            except Exception as exc:
                # The broker never accepted this value, so it must not be
                # reported as applied.  ``prev`` is left untouched and the
                # desired value stays visible as pending.
                self._failures += 1
                self._last_error = exc
                raise
            self.prev = pcount
            self._sent = new_value
            self._failures = 0
            self._last_error = None
            return pcount

    def update(self):
        """Update prefetch count with current value."""
        with self._mutex:
            return self.set(self.value)

    def reset(self):
        """Forget the sent value, e.g. when the channel was lost.

        The desired value is kept, so it shows up as pending and the next
        :meth:`update` or :meth:`reapply` sends it again on the new
        channel.
        """
        with self._mutex:
            self.prev = None
            self._sent = None

    def reapply(self):
        """Send the current value again, e.g. after a connection revived.

        Unlike :meth:`update` this does not assume the broker still knows
        the value: the sent state is reset first, so the value is always
        sent.  A failure leaves it pending for a later retry.
        """
        with self._mutex:
            self.prev = None
            self._sent = None
            return self.update()
