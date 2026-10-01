# Specification

We use this document to detail the internal specification of Yamux.
This is used both as a guide for implementing Yamux, but also for
alternative interoperable libraries to be built.

# Framing

Yamux uses a streaming connection underneath, but imposes a message
framing so that it can be shared between many logical streams. Each
frame contains a header like:

* Version (8 bits)
* Type (8 bits)
* Flags (16 bits)
* StreamID (32 bits)
* Length (32 bits)

This means that each header has a 12 byte overhead.
All fields are encoded in network order (big endian).
Each field is described below:

## Version Field

The version field is used for future backward compatibility. At the
current time, the field is always set to 0, to indicate the initial
version.

## Type Field

The type field is used to switch the frame message type. The following
message types are supported:

* 0x0 Data - Used to transmit data. May transmit zero length payloads
  depending on the flags.

* 0x1 Window Update - Used to updated the senders receive window size.
  This is used to implement per-session flow control.

* 0x2 Ping - Used to measure RTT. It can also be used to heart-beat
  and do keep-alives over TCP.

* 0x3 Go Away - Used to close a session.

* 0x4 Message - Optional extension. Carries one complete short message
  on the session-scoped message channel. With the SYN or ACK flag it is
  used to negotiate the channel and carries no payload.

## Flag Field

The flags field is used to provide additional information related
to the message type. The following flags are supported:

* 0x1 SYN - Signals the start of a new stream. May be sent with a data or
  window update message. Also sent with a ping to indicate outbound.

* 0x2 ACK - Acknowledges the start of a new stream. May be sent with a data
  or window update message. Also sent with a ping to indicate response.

* 0x4 FIN - Performs a half-close of a stream. May be sent with a data
  message or window update.

* 0x8 RST - Reset a stream immediately. May be sent with a data or
  window update message.

For Message frames only the SYN and ACK flags are defined, and only
during negotiation. FIN and RST must not be used.

## StreamID Field

The StreamID field is used to identify the logical stream the frame
is addressing. The client side should use odd ID's, and the server even.
This prevents any collisions. Additionally, the 0 ID is reserved to represent
the session.

Both Ping and Go Away messages should always use the 0 StreamID.

## Length Field

The meaning of the length field depends on the message type:

* Data - provides the length of bytes following the header
* Window update - provides a delta update to the window size
* Ping - Contains an opaque value, echoed back
* Go Away - Contains an error code
* Message - For data frames, the length of the message payload following
  the header. For SYN/ACK negotiation frames, the maximum message size
  the sender is willing to receive, with no payload following.

# Message Flow

There is no explicit connection setup, as Yamux relies on an underlying
transport to be provided. However, there is a distinction between client
and server side of the connection.

## Opening a stream

To open a stream, an initial data or window update frame is sent
with a new StreamID. The SYN flag should be set to signal a new stream.

The receiver must then reply with either a data or window update frame
with the StreamID along with the ACK flag to accept the stream or with
the RST flag to reject the stream.

Because we are relying on the reliable stream underneath, a connection
can begin sending data once the SYN flag is sent. The corresponding
ACK does not need to be received. This is particularly well suited
for an RPC system where a client wants to open a stream and immediately
fire a request without waiting for the RTT of the ACK.

This does introduce the possibility of a connection being rejected
after data has been sent already. This is a slight semantic difference
from TCP, where the connection cannot be refused after it is opened.
Clients should be prepared to handle this by checking for an error
that indicates a RST was received.

## Closing a stream

To close a stream, either side sends a data or window update frame
along with the FIN flag. This does a half-close indicating the sender
will send no further data.

Once both sides have closed the connection, the stream is closed.

Alternatively, if an error occurs, the RST flag can be used to
hard close a stream immediately.

## Flow Control

When Yamux is initially starts each stream with a 256KB window size.
There is no window size for the session.

To prevent the streams from stalling, window update frames should be
sent regularly. Yamux can be configured to provide a larger limit for
windows sizes. Both sides assume the initial 256KB window, but can
immediately send a window update as part of the SYN/ACK indicating a
larger window.

Both sides should track the number of bytes sent in Data frames
only, as only they are tracked as part of the window size.

## Session termination

When a session is being terminated, the Go Away message should
be sent. The Length should be set to one of the following to
provide an error code:

* 0x0 Normal termination
* 0x1 Protocol error
* 0x2 Internal error

# Message Channel (Optional Extension)

The message channel is an optional, session-scoped channel for short,
independent messages, intended for data such as real-time location
updates where stale values may be discarded. It shares the underlying
connection with normal streams and does not change their reliable,
ordered, stream semantics in any way.

## Negotiation

The channel must be enabled on both sides. When enabled, each side
sends a Message frame with the SYN flag and StreamID 0 when the session
starts. The Length field announces the maximum message size the sender
is willing to receive; no payload follows. The receiver of a SYN
replies with a Message frame carrying the ACK flag, again announcing
its own maximum message size.

Once a side has received the peer's SYN or ACK, the channel is ready.
The effective maximum size for outgoing messages is the minimum of the
locally configured and the peer-announced maximum.

An implementation that does not enable the extension will reject the
unknown frame type as a protocol error, so the extension must only be
enabled when both peers support it.

## Message delivery

Each Message data frame (no flags, StreamID 0) carries exactly one
complete message as its payload. A message is never split across
frames, so a receiver always delivers a message in full or not at all;
it cannot be divided across multiple application reads and a partial
message is never delivered. Messages larger than the negotiated maximum
are a protocol error.

## Local drop semantics

The underlying transport remains reliable: this extension does not
provide unreliable network transport over TCP. The only place messages
are dropped is the receiver's local, bounded queue. If the application
does not read messages fast enough and the queue is full, the oldest
queued message is dropped to make room for the newer one. The drop
happens locally and is counted so the receiving application can observe
it; the sender is never notified and never retransmits.

Because every message payload is always drained from the wire in full,
congestion of the message queue never blocks the receive loop: Ping,
Window Update, and stream data frames continue to make progress while
messages are being dropped.
