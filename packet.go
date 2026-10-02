package milter

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultMaxPacketSize is the default maximum size of a milter packet,
// including the command byte. It is used when either [Server.MaxPacketSize]
// or [ClientOptions.MaxPacketSize] is left as zero.
const DefaultMaxPacketSize uint32 = 512 * 1024 * 1024 // 512 MiB

// Direction identifies the direction a malformed packet was traveling in.
type Direction uint8

const (
	// DirectionUnknown is used when the packet direction cannot be
	// determined.
	DirectionUnknown Direction = iota
	// DirectionMTA marks packets sent by the MTA (the milter client side)
	// and read by the milter server.
	DirectionMTA
	// DirectionMilter marks packets sent by the milter server and read by
	// the milter client (MTA side).
	DirectionMilter
)

func (d Direction) String() string {
	switch d {
	case DirectionMTA:
		return "MTA->milter"
	case DirectionMilter:
		return "milter->MTA"
	default:
		return "unknown direction"
	}
}

// PacketErrorKind classifies a packet framing error.
type PacketErrorKind uint8

const (
	// PacketLengthZero means the 4-byte length header advertised a packet
	// length of zero, so no command byte exists.
	PacketLengthZero PacketErrorKind = iota
	// PacketTooLarge means the advertised length exceeds the configured
	// maximum. The payload is rejected before any of it is read or
	// allocated.
	PacketTooLarge
	// PacketTruncated means the connection ended (or errored) before the
	// advertised packet was fully received, either in the length header or
	// in the payload.
	PacketTruncated
)

func (k PacketErrorKind) String() string {
	switch k {
	case PacketLengthZero:
		return "zero packet length"
	case PacketTooLarge:
		return "packet length exceeds maximum"
	case PacketTruncated:
		return "truncated packet"
	default:
		return "unknown packet error"
	}
}

// PacketError describes a packet framing error.
//
// It records the packet [Direction], the failure kind and the number of
// bytes consumed from the connection before the error was detected. It never
// contains packet payload data, in particular no message body or headers.
type PacketError struct {
	// Direction is the direction the packet was traveling in.
	Direction Direction
	// Kind classifies the framing error.
	Kind PacketErrorKind
	// DeclaredLength is the packet length advertised by the 4-byte length
	// header (including the command byte). It is zero when the length
	// header itself is truncated.
	DeclaredLength uint32
	// MaxPacketSize is the configured limit. It is non-zero only when Kind
	// is PacketTooLarge.
	MaxPacketSize uint32
	// Consumed is the number of bytes read from the connection before the
	// error: 0-4 length-header bytes plus any payload bytes.
	Consumed int
	// Err is the underlying I/O error, if any.
	Err error
}

func (e *PacketError) Error() string {
	switch e.Kind {
	case PacketLengthZero:
		return fmt.Sprintf("milter: %s: %s (%d bytes consumed)",
			e.Direction, e.Kind, e.Consumed)
	case PacketTooLarge:
		return fmt.Sprintf("milter: %s: %s: declared length %d, maximum %d (%d bytes consumed)",
			e.Direction, e.Kind, e.DeclaredLength, e.MaxPacketSize, e.Consumed)
	case PacketTruncated:
		if e.DeclaredLength == 0 {
			return fmt.Sprintf("milter: %s: %s: read %d of 4 length-header bytes",
				e.Direction, e.Kind, e.Consumed)
		}
		return fmt.Sprintf("milter: %s: %s: declared length %d, read %d of %d bytes",
			e.Direction, e.Kind, e.DeclaredLength, e.Consumed, 4+int(e.DeclaredLength))
	default:
		return fmt.Sprintf("milter: %s: %s", e.Direction, e.Kind)
	}
}

func (e *PacketError) Unwrap() error {
	return e.Err
}

// CommandOnly reports whether the message consists of a command byte with no
// payload (declared packet length of one). QUIT, EOH, EOB and the simple
// action responses are command-only packets.
func (m *Message) CommandOnly() bool {
	return len(m.Data) == 0
}

// readPacket reads a single milter packet.
//
// The 4-byte length header is validated before a payload buffer is
// allocated: a zero length and a length above maxPacketSize are reported as
// structured [PacketError] values, and an oversized packet is rejected
// without reading its payload. The same limit applies to option
// negotiation, macros and body packets; business-level body chunk limits
// are enforced independently by the callers.
//
// A declared length of one is valid and produces a command-only message.
func readPacket(conn net.Conn, timeout time.Duration, maxPacketSize uint32, direction Direction) (*Message, error) {
	if timeout != 0 {
		conn.SetReadDeadline(time.Now().Add(timeout))
		defer conn.SetReadDeadline(time.Time{})
	}

	if maxPacketSize == 0 {
		maxPacketSize = DefaultMaxPacketSize
	}

	// Read the length header byte by byte so a partial header can be
	// reported with the exact number of consumed bytes.
	var header [4]byte
	consumed, err := io.ReadFull(conn, header[:])
	if err != nil {
		if consumed == 0 && err == io.EOF {
			// Clean connection close before any data arrived.
			return nil, io.EOF
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, &PacketError{
				Direction: direction,
				Kind:      PacketTruncated,
				Consumed:  consumed,
				Err:       err,
			}
		}
		return nil, err
	}

	length := binary.BigEndian.Uint32(header[:])

	if length == 0 {
		return nil, &PacketError{
			Direction: direction,
			Kind:      PacketLengthZero,
			Consumed:  consumed,
			Err:       io.ErrUnexpectedEOF,
		}
	}

	// Reject before allocating or reading the advertised payload.
	if length > maxPacketSize {
		return nil, &PacketError{
			Direction:      direction,
			Kind:           PacketTooLarge,
			DeclaredLength: length,
			MaxPacketSize:  maxPacketSize,
			Consumed:       consumed,
		}
	}

	// Read the full packet, command byte included.
	data := make([]byte, length)
	n, err := io.ReadFull(conn, data)
	consumed += n
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, &PacketError{
				Direction:      direction,
				Kind:           PacketTruncated,
				DeclaredLength: length,
				Consumed:       consumed,
				Err:            err,
			}
		}
		return nil, err
	}

	return &Message{
		Code: data[0],
		Data: data[1:],
	}, nil
}
