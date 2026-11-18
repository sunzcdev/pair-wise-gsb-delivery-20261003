Advanced Usage
==============

Priority
--------

.. versionadded:: 2.0.0

`RFC 7540`_ has a fairly substantial and complex section describing how to
build a HTTP/2 priority tree, and the effect that should have on sending data
from a server.

h2 does not enforce any priority logic by default for servers. This is
because scheduling data sends is outside the scope of this library, as it
likely requires fairly substantial understanding of the scheduler being used.

However, for servers that *do* want to follow the priority recommendations
given by clients, the Hyper project provides `an implementation`_ of the
`RFC 7540`_ priority tree that will be useful to plug into a server. That,
combined with the :class:`PriorityUpdated <h2.events.PriorityUpdated>` event from
this library, can be used to build a server that conforms to RFC 7540's
recommendations for priority handling.

Related Events
--------------

.. versionadded:: 2.4.0

In the 2.4.0 release h2 added support for signaling "related events".
These are a HTTP/2-only construct that exist because certain HTTP/2 events can
occur simultaneously: that is, one HTTP/2 frame can cause multiple state
transitions to occur at the same time. One example of this is a HEADERS frame
that contains priority information and carries the END_STREAM flag: this would
cause three events to fire (one of the various request/response received
events, a :class:`PriorityUpdated <h2.events.PriorityUpdated>` event, and a
:class:`StreamEnded <h2.events.StreamEnded>` event).

Ordinarily h2's logic will emit those events to you one at a time. This
means that you may attempt to process, for example, a
:class:`DataReceived <h2.events.DataReceived>` event, not knowing that the next
event out will be a :class:`StreamEnded <h2.events.StreamEnded>` event.
h2 *does* know this, however, and so will forbid you from taking certain
actions that are a violation of the HTTP/2 protocol.

To avoid this asymmetry of information, events that can occur simultaneously
now carry properties for their "related events". These allow users to find the
events that can have occurred simultaneously with each other before the event
is emitted by h2. The following objects have "related events":

- :class:`RequestReceived <h2.events.RequestReceived>`:

    - :data:`stream_ended <h2.events.RequestReceived.stream_ended>`: any
      :class:`StreamEnded <h2.events.StreamEnded>` event that occurred at the
      same time as receiving this request.

    - :data:`priority_updated
      <h2.events.RequestReceived.priority_updated>`: any
      :class:`PriorityUpdated <h2.events.PriorityUpdated>` event that occurred
      at the same time as receiving this request.

- :class:`ResponseReceived <h2.events.ResponseReceived>`:

    - :data:`stream_ended <h2.events.ResponseReceived.stream_ended>`: any
      :class:`StreamEnded <h2.events.StreamEnded>` event that occurred at the
      same time as receiving this response.

    - :data:`priority_updated
      <h2.events.ResponseReceived.priority_updated>`: any
      :class:`PriorityUpdated <h2.events.PriorityUpdated>` event that occurred
      at the same time as receiving this response.

- :class:`TrailersReceived <h2.events.TrailersReceived>`:

    - :data:`stream_ended <h2.events.TrailersReceived.stream_ended>`: any
      :class:`StreamEnded <h2.events.StreamEnded>` event that occurred at the
      same time as receiving this set of trailers. This will **always** be
      present for trailers, as they must terminate streams.

    - :data:`priority_updated
      <h2.events.TrailersReceived.priority_updated>`: any
      :class:`PriorityUpdated <h2.events.PriorityUpdated>` event that occurred
      at the same time as receiving this response.

- :class:`InformationalResponseReceived
  <h2.events.InformationalResponseReceived>`:

    - :data:`priority_updated
      <h2.events.InformationalResponseReceived.priority_updated>`: any
      :class:`PriorityUpdated <h2.events.PriorityUpdated>` event that occurred
      at the same time as receiving this informational response.

- :class:`DataReceived <h2.events.DataReceived>`:

    - :data:`stream_ended <h2.events.DataReceived.stream_ended>`: any
      :class:`StreamEnded <h2.events.StreamEnded>` event that occurred at the
      same time as receiving this data.


.. warning:: h2 does not know if you are looking for related events or
             expecting to find events in the event stream. Therefore, it will
             always emit "related events" in the event stream. If you are using
             the "related events" event pattern, you will want to be careful to
             avoid double-processing related events.

.. _h2-connection-advanced:

Connections: Advanced
---------------------

Thread Safety
~~~~~~~~~~~~~

``H2Connection`` objects are *not* thread-safe. They cannot safely be accessed
from multiple threads at once. This is a deliberate design decision: it is not
trivially possible to design the ``H2Connection`` object in a way that would
be either lock-free or have the locks at a fine granularity.

Your implementations should bear this in mind, and handle it appropriately. It
should be simple enough to use locking alongside the ``H2Connection``: simply
lock around the connection object itself. Because the ``H2Connection`` object
does no I/O it should be entirely safe to do that. Alternatively, have a single
thread take ownership of the ``H2Connection`` and use a message-passing
interface to serialize access to the ``H2Connection``.

If you are using a non-threaded concurrency approach (e.g. Twisted), this
should not affect you.

Internal Buffers
~~~~~~~~~~~~~~~~

In order to avoid doing I/O, the ``H2Connection`` employs an internal buffer.
This buffer is *unbounded* in size: it can potentially grow infinitely. This
means that, if you are not making sure to regularly empty it, you are at risk
of exceeding the memory limit of a single process and finding your program
crashes.

It is highly recommended that you send data at regular intervals, ideally as
soon as possible.

.. _advanced-sending-data:

Sending Data
~~~~~~~~~~~~

When sending data on the network, it's important to remember that you may not
be able to send an unbounded amount of data at once. Particularly when using
TCP, it is often the case that there are limits on how much data may be in
flight at any one time. These limits can be very low, and your operating system
will only buffer so much data in memory before it starts to complain.

For this reason, it is possible to consume only a subset of the data available
when you call :meth:`data_to_send <h2.connection.H2Connection.data_to_send>`.
However, once you have pulled the data out of the ``H2Connection`` internal
buffer, it is *not* possible to put it back on again. For that reason, it is
advisable that you confirm how much space is available in the OS buffer before
sending.

Alternatively, use tools made available by your framework. For example, the
Python standard library :mod:`socket <python:socket>` module provides a
:meth:`sendall <python:socket.socket.sendall>` method that will automatically
block until all the data has been sent. This will enable you to always use the
unbounded form of
:meth:`data_to_send <h2.connection.H2Connection.data_to_send>`, and will help
you avoid subtle bugs.

When To Send
~~~~~~~~~~~~

In addition to knowing how much data to send (see :ref:`advanced-sending-data`)
it is important to know when to send data. For h2, this amounts to
knowing when to call :meth:`data_to_send
<h2.connection.H2Connection.data_to_send>`.

h2 may write data into its send buffer at two times. The first is
whenever :meth:`receive_data <h2.connection.H2Connection.receive_data>` is
called. This data is sent in response to some control frames that require no
user input: for example, responding to PING frames. The second time is in
response to user action: whenever a user calls a method like
:meth:`send_headers <h2.connection.H2Connection.send_headers>`, data may be
written into the buffer.

In a standard design for a h2 consumer, then, that means there are two
places where you'll potentially want to send data. The first is in your
"receive data" loop. This is where you take the data you receive, pass it into
:meth:`receive_data <h2.connection.H2Connection.receive_data>`, and then
dispatch events. For this loop, it is usually best to save sending data until
the loop is complete: that allows you to empty the buffer only once.

The other place you'll want to send the data is when initiating requests or
taking any other active, unprompted action on the connection. In this instance,
you'll want to make all the relevant ``send_*`` calls, and *then* call
:meth:`data_to_send <h2.connection.H2Connection.data_to_send>`.

Headers
-------

HTTP/2 defines several "special header fields" which are used to encode data
that was previously sent in either the request or status line of HTTP/1.1.
These header fields are distinguished from ordinary header fields because their
field name begins with a ``:`` character. The special header fields defined in
`RFC 7540`_ are:

- ``:status``
- ``:path``
- ``:method``
- ``:scheme``
- ``:authority``

`RFC 7540`_ **mandates** that all of these header fields appear *first* in the
header block, before the ordinary header fields. This could cause difficulty if
the :meth:`send_headers <h2.connection.H2Connection.send_headers>` method
accepted a plain ``dict`` for the ``headers`` argument, because ``dict``
objects are unordered. For this reason, we require that you provide a list of
two-tuples.

.. _RFC 7540: https://tools.ietf.org/html/rfc7540
.. _an implementation: http://python-hyper.org/projects/priority/en/latest/

Flow Control
------------

HTTP/2 defines a complex flow control system that uses a sliding window of
data on both a per-stream and per-connection basis. Essentially, each
implementation allows its peer to send a specific amount of data at any time
(the "flow control window") before it must stop. Each stream has a separate
window, and the connection as a whole has a window. Each window can be opened
by an implementation by sending a ``WINDOW_UPDATE`` frame, either on a specific
stream (causing the window for that stream to be opened), or on stream ``0``,
which causes the window for the entire connection to be opened.

In HTTP/2, only data in ``DATA`` frames is flow controlled. All other frames
are exempt from flow control. Each ``DATA`` frame consumes both stream and
connection flow control window bytes. This means that the maximum amount of
data that can be sent on any one stream before a ``WINDOW_UPDATE`` frame is
received is the *lower* of the stream and connection windows. The maximum
amount of data that can be sent on *all* streams before a ``WINDOW_UPDATE``
frame is received is the size of the connection flow control window.

Working With Flow Control
~~~~~~~~~~~~~~~~~~~~~~~~~

The amount of flow control window a ``DATA`` frame consumes is the sum of both
its contained application data *and* the amount of padding used. h2 shows
this to the user in a :class:`DataReceived <h2.events.DataReceived>` event by
using the :data:`flow_controlled_length
<h2.events.DataReceived.flow_controlled_length>` field. When working with flow
control in h2, users *must* use this field: simply using
``len(datareceived.data)`` can eventually lead to deadlock.

When data has been received and given to the user in a :class:`DataReceived
<h2.events.DataReceived>`, it is the responsibility of the user to re-open the
flow control window when the user is ready for more data. h2 does not do
this automatically to avoid flooding the user with data: if we did, the remote
peer could send unbounded amounts of data that the user would need to buffer
before processing.

To re-open the flow control window, then, the user must call
:meth:`increment_flow_control_window
<h2.connection.H2Connection.increment_flow_control_window>` with the
:data:`flow_controlled_length <h2.events.DataReceived.flow_controlled_length>`
of the received data. h2 requires that you manage both the connection
and the stream flow control windows separately, so you may need to increment
both the stream the data was received on and stream ``0``.

When sending data, a HTTP/2 implementation must not send more than flow control
window available for that stream. As noted above, the maximum amount of data
that can be sent on the stream is the minimum of the stream and the connection
flow control windows. You can find out how much data you can send on a given
stream by using the :meth:`local_flow_control_window
<h2.connection.H2Connection.local_flow_control_window>` method, which will do
all of these calculations for you. If you attempt to send more than this amount
of data on a stream, h2 will throw a :class:`ProtocolError
<h2.exceptions.ProtocolError>` and refuse to send the data.

In h2, receiving a ``WINDOW_UPDATE`` frame causes a :class:`WindowUpdated
<h2.events.WindowUpdated>` event to fire. This will notify you that there is
potentially more room in a flow control window. Note that, just because an
increment of a given size was received *does not* mean that that much more data
can be sent: remember that both the connection and stream flow control windows
constrain how much data can be sent.

As a result, when a :class:`WindowUpdated <h2.events.WindowUpdated>` event
fires with a non-zero stream ID, and the user has more data to send on that
stream, the user should call :meth:`local_flow_control_window
<h2.connection.H2Connection.local_flow_control_window>` to check if there
really is more room to send data on that stream.

When a :class:`WindowUpdated <h2.events.WindowUpdated>` event fires with a
stream ID of ``0``, that may have unblocked *all* streams that are currently
blocked. The user should use :meth:`local_flow_control_window
<h2.connection.H2Connection.local_flow_control_window>` to check all blocked
streams to see if more data is available.

Auto Flow Control
~~~~~~~~~~~~~~~~~

.. versionadded:: 2.5.0

In most cases, there is no advantage for users in managing their own flow
control strategies. While particular high performance or specific-use-case
applications may gain value from directly controlling the emission of
``WINDOW_UPDATE`` frames, the average application can use a
lowest-common-denominator strategy to emit those frames. As of version 2.5.0,
h2 now provides this automatic strategy for users, if they want to use
it.

This automatic strategy is built around a single method:
:meth:`acknowledge_received_data
<h2.connection.H2Connection.acknowledge_received_data>`. This method
flags to the connection object that your application has dealt with a certain
number of flow controlled bytes, and that the window should be incremented in
some way. Whenever your application has "processed" some received bytes, this
method should be called to signal that they have been processed.

The key difference between this method and :meth:`increment_flow_control_window
<h2.connection.H2Connection.increment_flow_control_window>` is that the method
:meth:`acknowledge_received_data
<h2.connection.H2Connection.acknowledge_received_data>` does not guarantee that
it will emit a ``WINDOW_UPDATE`` frame, and if it does it will not necessarily
emit them for *only* the stream or *only* the frame. Instead, the
``WINDOW_UPDATE`` frames will be *coalesced*: they will be emitted only when
a certain number of bytes have been freed up.

For most applications, this method should be preferred to the manual flow
control mechanism.

Draining a connection after GOAWAY
-----------------------------------

When the remote peer sends a GOAWAY frame, RFC 7540 requires that no new
streams are created on the connection, while streams that were already in
flight are allowed to finish. The application is also responsible for
deciding what to do with requests whose fate is unclear: h2 never retries a
request on its own.

The :attr:`H2Connection.goaway_drain_view
<h2.connection.H2Connection.goaway_drain_view>` attribute exposes an
inspectable, live :class:`~h2.events.GoAwayDrainView` for exactly this
purpose. Before any GOAWAY frame has been received the view is empty; once a
GOAWAY frame arrives, every stream known to the connection is classified
relative to the GOAWAY ``last_stream_id``, the stream direction and the
response state observed so far:

* :data:`~h2.events.GoAwayStreamState.RESUBMITTABLE` streams are outbound
  streams whose identifier is strictly greater than the GOAWAY
  ``last_stream_id``. The remote peer explicitly signalled that it did not
  process them, so their requests may safely be re-submitted, typically on a
  brand new connection.
* :data:`~h2.events.GoAwayStreamState.PROCESSED` streams are known to have
  been dealt with: a final response has been received, the remote peer sent
  a RST_STREAM frame, or the stream is inbound and therefore cannot be
  re-submitted by this endpoint.
* :data:`~h2.events.GoAwayStreamState.UNKNOWN` streams are outbound streams
  at or below ``last_stream_id`` for which no final response has been seen.
  It is impossible to tell whether the remote peer processed them, so they
  must not be blindly re-submitted.

The classification is deliberately *not* based solely on whether a stream
is currently closed. A stream can be closed locally (for example because we
sent a RST_STREAM) while still being resubmittable or unknown.

A peer is allowed to send several GOAWAY frames while shutting down, each
potentially carrying a smaller ``last_stream_id`` as in-flight streams
complete. The view always uses the smallest value received and updates live:
a stream that was previously unknown can become resubmittable, but h2 never
silently marks an unknown stream as processed. Frames belonging to
pre-existing streams continue to be processed normally after the GOAWAY, so
the view also reflects responses that arrive during the drain.

A typical client-side drain looks like this::

    view = connection.goaway_drain_view
    for stream_id in view.resubmittable_streams:
        # The peer never saw this request: replay it on a fresh connection.
        resubmit(request_for(stream_id))

    for stream_id in view.unknown_streams:
        # The peer may have processed this request. Use an idempotency key,
        # a status check, or application-specific logic rather than blindly
        # replaying the request.
        handle_ambiguous(request_for(stream_id))

    # Processed streams need no action; keep reading data until the
    # connection actually closes so their outstanding frames are handled.
