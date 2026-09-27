// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"encoding/binary"
	"fmt"
)

// NetError implements net.Error
type NetError struct {
	err       error
	timeout   bool
	temporary bool
}

func (e *NetError) Error() string {
	return e.err.Error()
}

func (e *NetError) Timeout() bool {
	return e.timeout
}

func (e *NetError) Temporary() bool {
	return e.temporary
}

var (
	// ErrInvalidVersion means we received a frame with an
	// invalid version
	ErrInvalidVersion = fmt.Errorf("invalid protocol version")

	// ErrInvalidMsgType means we received a frame with an
	// invalid message type
	ErrInvalidMsgType = fmt.Errorf("invalid msg type")

	// ErrSessionShutdown is used if there is a shutdown during
	// an operation
	ErrSessionShutdown = fmt.Errorf("session shutdown")

	// ErrStreamsExhausted is returned if we have no more
	// stream ids to issue
	ErrStreamsExhausted = fmt.Errorf("streams exhausted")

	// ErrDuplicateStream is used if a duplicate stream is
	// opened inbound
	ErrDuplicateStream = fmt.Errorf("duplicate stream initiated")

	// ErrReceiveWindowExceeded indicates the window was exceeded
	ErrRecvWindowExceeded = fmt.Errorf("recv window exceeded")

	// ErrTimeout is used when we reach an IO deadline
	ErrTimeout = &NetError{
		err: fmt.Errorf("i/o deadline reached"),

		// Error should meet net.Error interface for timeouts for compatability
		// with standard library expectations, such as http servers.
		timeout: true,
	}

	// ErrStreamClosed is returned when using a closed stream
	ErrStreamClosed = fmt.Errorf("stream closed")

	// ErrUnexpectedFlag is set when we get an unexpected flag
	ErrUnexpectedFlag = fmt.Errorf("unexpected flag")

	// ErrRemoteGoAway is used when we get a go away from the other side
	ErrRemoteGoAway = fmt.Errorf("remote end is not accepting connections")

	// ErrConnectionReset is sent if a stream is reset. This can happen
	// if the backlog is exceeded, or if there was a remote GoAway.
	ErrConnectionReset = fmt.Errorf("connection reset")

	// ErrConnectionWriteTimeout indicates that we hit the "safety valve"
	// timeout writing to the underlying stream connection.
	ErrConnectionWriteTimeout = fmt.Errorf("connection write timeout")

	// ErrKeepAliveTimeout is sent if a missed keepalive caused the stream close
	ErrKeepAliveTimeout = fmt.Errorf("keepalive timeout")

	// ErrServiceMetaUnsupported is returned by OpenService when the peer
	// did not negotiate the service metadata extension.
	ErrServiceMetaUnsupported = fmt.Errorf("service metadata extension not supported by peer")

	// ErrMetadataTooLarge is returned when service metadata exceeds the
	// configured or hard size bound.
	ErrMetadataTooLarge = fmt.Errorf("service metadata too large")

	// ErrInvalidMetadata is returned when a service metadata payload
	// cannot be decoded.
	ErrInvalidMetadata = fmt.Errorf("invalid service metadata")

	// ErrStreamRejected is returned to the opener when the remote side
	// rejected the stream during admission control.
	ErrStreamRejected = fmt.Errorf("stream rejected by peer")
)

const (
	// protoVersion is the only version we support
	protoVersion uint8 = 0
)

const (
	// Data is used for data frames. They are followed
	// by length bytes worth of payload.
	typeData uint8 = iota

	// WindowUpdate is used to change the window of
	// a given stream. The length indicates the delta
	// update to the window.
	typeWindowUpdate

	// Ping is sent as a keep-alive or to measure
	// the RTT. The StreamID and Length value are echoed
	// back in the response.
	typePing

	// GoAway is sent to terminate a session. The StreamID
	// should be 0 and the length is an error code.
	typeGoAway

	// StreamMeta carries bounded service metadata for a newly opened
	// stream. It is part of the optional service metadata extension and
	// must only be sent after capability negotiation succeeded. It is
	// sent immediately after the SYN frame carrying flagMETA, before any
	// data frames for the stream.
	typeStreamMeta
)

const (
	// SYN is sent to signal a new stream. May
	// be sent with a data payload
	flagSYN uint16 = 1 << iota

	// ACK is sent to acknowledge a new stream. May
	// be sent with a data payload
	flagACK

	// FIN is sent to half-close the given stream.
	// May be sent with a data payload.
	flagFIN

	// RST is used to hard close a given stream.
	flagRST

	// META is set on a SYN frame to signal that a StreamMeta frame
	// follows for this stream. Only used after the service metadata
	// extension has been negotiated. Peers that do not know the flag
	// ignore it, but senders must not set it before negotiation.
	flagMETA
)

const (
	// initialStreamWindow is the initial stream window size
	initialStreamWindow uint32 = 256 * 1024
)

const (
	// extPingQuery is sent as the Length of a Ping frame to negotiate
	// optional extensions. A peer that implements the extension replies
	// with extPingRespPrefix|caps; an unaware peer simply echoes the
	// value back, which is detected as "not supported".
	extPingQuery uint32 = 0x59584D51 // "YXMQ"

	// extPingRespPrefix is the high 24 bits of a capability response.
	extPingRespPrefix uint32 = 0x59584D00 // "YXM\0"
	extPingRespMask   uint32 = 0xFFFFFF00

	// extCapServiceMeta indicates support for the service metadata
	// extension (flagMETA + typeStreamMeta frames).
	extCapServiceMeta uint32 = 1 << 0
)

const (
	// defaultMaxStreamMetadataSize is the default bound on the encoded
	// service metadata payload.
	defaultMaxStreamMetadataSize uint32 = 4096

	// hardMaxStreamMetadataSize is the absolute bound on a StreamMeta
	// payload. Frames announcing a larger payload are a protocol error,
	// so a receiver never allocates beyond this limit.
	hardMaxStreamMetadataSize uint32 = 1 << 20
)

const (
	// goAwayNormal is sent on a normal termination
	goAwayNormal uint32 = iota

	// goAwayProtoErr sent on a protocol error
	goAwayProtoErr

	// goAwayInternalErr sent on an internal error
	goAwayInternalErr
)

const (
	sizeOfVersion  = 1
	sizeOfType     = 1
	sizeOfFlags    = 2
	sizeOfStreamID = 4
	sizeOfLength   = 4
	headerSize     = sizeOfVersion + sizeOfType + sizeOfFlags +
		sizeOfStreamID + sizeOfLength
)

type header []byte

func (h header) Version() uint8 {
	return h[0]
}

func (h header) MsgType() uint8 {
	return h[1]
}

func (h header) Flags() uint16 {
	return binary.BigEndian.Uint16(h[2:4])
}

func (h header) StreamID() uint32 {
	return binary.BigEndian.Uint32(h[4:8])
}

func (h header) Length() uint32 {
	return binary.BigEndian.Uint32(h[8:12])
}

func (h header) String() string {
	return fmt.Sprintf("Vsn:%d Type:%d Flags:%d StreamID:%d Length:%d",
		h.Version(), h.MsgType(), h.Flags(), h.StreamID(), h.Length())
}

func (h header) encode(msgType uint8, flags uint16, streamID uint32, length uint32) {
	h[0] = protoVersion
	h[1] = msgType
	binary.BigEndian.PutUint16(h[2:4], flags)
	binary.BigEndian.PutUint32(h[4:8], streamID)
	binary.BigEndian.PutUint32(h[8:12], length)
}
