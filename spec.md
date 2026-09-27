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

* 0x4 Stream Metadata - Optional extension frame. Announces a new
  stream and carries bounded service metadata. Must only be sent
  after the stream metadata extension has been negotiated; see
  "Stream Metadata Extension" below.

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
* Stream Metadata - provides the length of metadata bytes following
  the header

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

# Stream Metadata Extension

This optional extension lets the side opening a stream attach bounded,
opaque service metadata (for example a service name) to the handshake,
so the receiver can accept or reject the stream before handing it to
the application. It is designed so that endpoints with and without the
extension interoperate safely.

## Capability negotiation

Support must be negotiated before any Stream Metadata frame is sent.
The negotiation reuses the Ping message so that peers without the
extension never receive a frame type they cannot parse:

* The ping ID 0xFFFFFFFF is reserved as a capability probe. An
  endpoint with the extension enabled sends a Ping (SYN, StreamID 0)
  with this ID when the session starts.
* A peer that implements and enables the extension replies with a
  Ping (ACK, StreamID 0) using the reserved ID 0xFFFFFFFE.
* A peer without the extension simply echoes the probe ID back, as it
  would for any other ping. Receiving the echoed probe ID (or any
  other value) means the extension is not supported.

The IDs 0xFFFFFFFF and 0xFFFFFFFE are reserved and must not be used
for ordinary pings. If the negotiation does not confirm support, the
sender must not emit Stream Metadata frames.

## Opening a stream with metadata

To open a stream with metadata, the opener first sends a Stream
Metadata frame with the new StreamID and the metadata payload (at most
1024 bytes), immediately followed by the usual SYN frame for that
StreamID.

The receiver registers the stream as pending and evaluates its
admission policy (for example an application callback) asynchronously,
so a slow decision never stalls the receive loop or other streams.
While pending, the stream is not yet surfaced to the application:

* If admitted, the stream is queued for acceptance and the normal
  ACK is sent when the application accepts it.
* If rejected, the receiver replies with RST and the stream is never
  surfaced to the application.

Streams opened without a Stream Metadata frame follow the regular
message flow unchanged.
