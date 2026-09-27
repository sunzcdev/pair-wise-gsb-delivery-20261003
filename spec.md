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

# Optional Extension: Service Metadata

The service metadata extension lets the opening side attach a bounded
set of attributes (a service name plus key/value pairs) to a new stream,
so the receiving side can accept or reject the stream before handing it
to the application. The extension is optional and fully negotiated:
endpoints that do not implement it see no wire difference.

## Capability negotiation

Extensions must be negotiated before any extension frame is sent.
Negotiation reuses ordinary Ping frames so that unaware peers are never
exposed to a frame type they cannot parse:

* The initiator sends a Ping with the SYN flag and the magic Length
  value `0x59584D51` ("YXMQ").

* A peer that implements the extension replies with a Ping ACK whose
  Length is `0x59584D00 | caps`, where `caps` is a bit mask. Bit 0
  (`0x01`) indicates support for service metadata.

* An unaware peer simply echoes the opaque ping value back
  (`0x59584D51`), which the initiator reads as "no capabilities".

Sending the query itself declares support for the extension, so a
receiver of the query can record the peer as capable immediately.

An endpoint must not set the META flag or send Stream Meta frames
unless both sides support the extension.

## Opening a stream with metadata

To attach metadata, the opener sets the META flag (`0x10`) on the
initial SYN frame and immediately sends a Stream Meta frame (type 0x4)
for the same StreamID, before any data frames. The Length field of the
Stream Meta frame carries the payload size; receivers must bound it
(1 MiB hard limit) and should apply a smaller configured limit.

The payload is encoded in network order as:

* Service name length (16 bits), followed by the service name bytes
* Attribute count (16 bits), followed by that many attributes, each
  encoded as key length (16 bits), key bytes, value length (16 bits),
  value bytes

## Admission

Upon receiving the Stream Meta frame, the receiver runs its admission
decision asynchronously, so a slow decision never blocks data or
control frames of other streams:

* If the stream is admitted, it is queued for the application and
  acknowledged with an ACK as usual once accepted.

* If the stream is rejected, the receiver sends a RST for the stream
  and never surfaces it to the application. The opener observes the
  reset as a stream rejection.

Data frames pipelined by the opener before the admission decision are
buffered within the usual receive window and discarded if the stream is
rejected.
