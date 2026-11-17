.. _guide-consumers:

===========
 Consumers
===========

.. _consumer-basics:

Basics
======

The :class:`~kombu.messaging.Consumer` takes a connection (or channel) and a list of queues to
consume from. Several consumers can be mixed to consume from different
channels, as they all bind to the same connection, and ``drain_events`` will
drain events from all channels on that connection.

.. note::

    Kombu since 3.0 will only accept json/binary or text messages by default,
    to allow deserialization of other formats you have to specify them
    in the ``accept`` argument (in addition to setting the right content type for your messages):

    .. code-block:: python

        >>> Consumer(conn, accept=['json', 'pickle', 'msgpack', 'yaml'])

You can create a consumer using a Connection. This consumer is consuming from a single queue with name `'queue'`:

.. code-block:: python

    >>> queue = Queue('queue', routing_key='queue')
    >>> consumer = connection.Consumer(queue)

You can also instantiate Consumer directly, it takes a channel or a connection as an argument. This consumer also
consumes from single queue with name `'queue'`:

.. code-block:: python

    >>> queue = Queue('queue', routing_key='queue')
    >>> with Connection('amqp://') as conn:
    ...     with conn.channel() as channel:
    ...         consumer = Consumer(channel, queue)

A consumer needs to specify a handler for received data. This handler is specified in the form of a callback. The callback function is called
by kombu every time a new message is received. The callback is called with two parameters: ``body``, containing deserialized
data sent by a producer, and a :class:`~kombu.message.Message` instance ``message``. The user is responsible for acknowledging messages when manual
acknowledgement is set.

.. code-block:: python

    >>> def callback(body, message):
    ...     print(body)
    ...     message.ack()

    >>> consumer.register_callback(callback)

Draining events from a single consumer
--------------------------------------

The method ``drain_events`` blocks indefinitely by default. This example sets the timeout to 1 second:
 
.. code-block:: python

    >>> with consumer:
    ...     connection.drain_events(timeout=1)

Draining events from several consumers
--------------------------------------

Each consumer has its own list of queues. Each consumer accepts data in `'json'` format:

.. code-block:: python

    >>> from kombu.utils.compat import nested

    >>> queues1 = [Queue('queue11', routing_key='queue11'),
                   Queue('queue12', routing_key='queue12')]
    >>> queues2 = [Queue('queue21', routing_key='queue21'),
                   Queue('queue22', routing_key='queue22')]
    >>> with connection.channel(), connection.channel() as (channel1, channel2):
    ...     with nested(Consumer(channel1, queues1, accept=['json']),
    ...                 Consumer(channel2, queues2, accept=['json'])):
    ...         connection.drain_events(timeout=1)

The full example will look as follows:

.. code-block:: python

    from kombu import Connection, Consumer, Queue

    def callback(body, message):
        print('RECEIVED MESSAGE: {0!r}'.format(body))
        message.ack()

    queue1 = Queue('queue1', routing_key='queue1')
    queue2 = Queue('queue2', routing_key='queue2')

    with Connection('amqp://') as conn:
        with conn.channel() as channel:
            consumer = Consumer(conn, [queue1, queue2], accept=['json'])
            consumer.register_callback(callback)
            with consumer:
                conn.drain_events(timeout=1)

Consumer mixin classes
======================

Kombu provides predefined mixin classes in module :py:mod:`~kombu.mixins`. It contains two classes:
:class:`~kombu.mixins.ConsumerMixin` for creating consumers and :class:`~kombu.mixins.ConsumerProducerMixin`
for creating consumers supporting also publishing messages. Consumers can be created just by subclassing
mixin class and overriding some of the methods:

.. code-block:: python

    from kombu.mixins import ConsumerMixin

    class C(ConsumerMixin):

        def __init__(self, connection):
            self.connection = connection

        def get_consumers(self, Consumer, channel):
            return [
                Consumer(channel, callbacks=[self.on_message], accept=['json']),
            ]

        def on_message(self, body, message):
            print('RECEIVED MESSAGE: {0!r}'.format(body))
            message.ack()

    C(connection).run()


and with multiple channels again:

.. code-block:: python

    from kombu import Consumer
    from kombu.mixins import ConsumerMixin

    class C(ConsumerMixin):
        channel2 = None

        def __init__(self, connection):
            self.connection = connection

        def get_consumers(self, _, default_channel):
            self.channel2 = default_channel.connection.channel()
            return [Consumer(default_channel, queues1,
                             callbacks=[self.on_message],
                             accept=['json']),
                    Consumer(self.channel2, queues2,
                             callbacks=[self.on_special_message],
                             accept=['json'])]

        def on_consume_end(self, connection, default_channel):
            if self.channel2:
                self.channel2.close()

    C(connection).run()


The main use of :class:`~kombu.mixins.ConsumerProducerMixin` is to create consumers
that need to also publish messages on a separate connection (e.g. sending rpc
replies, streaming results):

.. code-block:: python

    from kombu import Producer, Queue
    from kombu.mixins import ConsumerProducerMixin

    rpc_queue = Queue('rpc_queue')

    class Worker(ConsumerProducerMixin):

        def __init__(self, connection):
            self.connection = connection

        def get_consumers(self, Consumer, channel):
            return [Consumer(
                queues=[rpc_queue],
                on_message=self.on_request,
                accept={'application/json'},
                prefetch_count=1,
            )]

        def on_request(self, message):
            n = message.payload['n']
            print(' [.] fib({0})'.format(n))
            result = fib(n)

            self.producer.publish(
                {'result': result},
                exchange='', routing_key=message.properties['reply_to'],
                correlation_id=message.properties['correlation_id'],
                serializer='json',
                retry=True,
            )
            message.ack()

.. seealso::

    :file:`examples/rpc-tut6/` in the Github repository.


Advanced Topics
===============

RabbitMQ
--------

Consumer Priorities
~~~~~~~~~~~~~~~~~~~

RabbitMQ defines a consumer priority extension to the amqp protocol,
that can be enabled by setting the ``x-priority`` argument to
``basic.consume``.

In kombu you can specify this argument on the :class:`~kombu.Queue`, like
this:

.. code-block:: python

    queue = Queue('name', Exchange('exchange_name', type='direct'),
                  consumer_arguments={'x-priority': 10})

Read more about consumer priorities here:
https://www.rabbitmq.com/consumer-priority.html


Adjusting the prefetch count at runtime
=======================================

The prefetch count can be changed after the consumer was created.  Each
:class:`~kombu.Consumer` keeps its own bookkeeping, so adjusting one consumer
never overwrites the setting of another consumer or channel.  A
``prefetch_count`` of zero keeps its protocol meaning of "no limit".

.. note::

    ``basic.qos`` applies to the whole channel, so consumers that share one
    channel still share the broker-side prefetch window.  Use one channel
    per consumer when their prefetch settings need to be independent.

The :meth:`~kombu.Consumer.qos` method returns a
:class:`~kombu.common.QoSState` describing the update:

``desired``
    the prefetch count the local process wants.

``sent``
    the last value the broker accepted, or ``None`` if nothing was sent yet.

``pending``
    the value still waiting to be applied, or ``None`` when nothing is
    outstanding.

``applied`` / ``in_sync``
    whether the update is fully applied, and whether the local expectation
    matches the value that was sent.

``failures`` / ``last_error``
    the number of consecutive failed attempts and the error raised by the
    last one.

To merge several adjustments made in one round into a single call, use
:meth:`~kombu.Consumer.qos_batch`; leaving the block applies the merged value
once.  If the block raises, nothing is sent and the desired value is kept
pending for a later retry::

    with consumer.qos_batch():
        consumer.qos_update(prefetch_count=10)
        consumer.qos_update(prefetch_count=25)

    state = consumer.qos_state()
    assert state.sent == 25

A failed call never marks the value as applied.  The value is kept pending
and the exception is re-raised, so the platform can inspect the state and
retry later::

    try:
        consumer.qos(prefetch_count=12)
    except ConnectionError:
        assert consumer.qos_state().pending == 12

After the connection was re-established the replacement channel has no
knowledge of the previous value.  :meth:`~kombu.Consumer.revive` therefore
sends the prefetch count again automatically, and
:meth:`~kombu.Consumer.reapply_qos` can be used to force that explicitly::

    consumer.reapply_qos()
    assert consumer.qos_state().in_sync

See :file:`examples/qos_update_state.py` for a complete offline example
using a controlled ``basic_qos`` callback.


Reference
=========

.. autoclass:: kombu.Consumer
    :noindex:
    :members:
