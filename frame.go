package milter

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultMaxPacketSize is the maximum allowed frame size (command byte plus
// payload) used by both clients and servers unless overridden.
//
// It matches the largest frame the on-wire milter protocol can legitimately
// produce: one command byte plus a MaxBodyChunk-sized body chunk.
const DefaultMaxPacketSize = MaxBodyChunk + 1

// Direction reports which way the offending frame was travelling.
type Direction int

const (
	// DirRecv is a frame being read from the peer.
	DirRecv Direction = iota
	// DirSend is a frame being written to the peer.
	DirSend
)

func (d Direction) String() string {
	switch d {
	case DirRecv:
		return "recv"
	case DirSend:
		return "send"
	default:
		return "unknown"
	}
}

// FrameErrorKind classifies a frame read or write failure.
type FrameErrorKind int

const (
	_ FrameErrorKind = iota // reserve zero so it never matches a real kind
	// FrameZeroLength means the four-byte length header decoded to zero.
	// A frame must at least contain a command byte.
	FrameZeroLength
	// FrameTooLarge means the declared length exceeds the configured
	// maximum packet size. It is reported before any payload is read or
	// allocated.
	FrameTooLarge
	// FrameIOError means the connection returned an I/O error, including a
	// clean EOF or a truncated frame in the middle of its declared payload.
	FrameIOError
)

func (k FrameErrorKind) String() string {
	switch k {
	case FrameIOError:
		return "io error"
	case FrameZeroLength:
		return "zero-length frame"
	case FrameTooLarge:
		return "frame too large"
	default:
		return "unknown frame error"
	}
}

// Sentinel errors matching the structured FrameError values, for use with
// errors.Is.
var (
	ErrFrameZeroLength = errors.New("milter: zero-length frame")
	ErrFrameTooLarge   = errors.New("milter: frame exceeds maximum packet size")
	ErrFrameIO         = errors.New("milter: frame i/o error")
)

// FrameError is the structured result of a failed frame read or write.
//
// Direction reports whether the frame was being received or sent and
// Consumed is the number of bytes already consumed from the connection for
// the offending frame (including its four-byte length header when present).
//
// FrameError never carries frame payload, so it is safe to log even when the
// payload contains message bodies or other email content.
type FrameError struct {
	// Direction is the direction the offending frame was travelling.
	Direction Direction
	// Kind classifies the failure.
	Kind FrameErrorKind
	// Consumed is the number of on-wire bytes consumed before the failure.
	Consumed int
	// Declared is the payload-plus-command length read from the length
	// header, or zero if it could not be read completely.
	Declared int
	// MaxSize is the configured limit that was exceeded, if any.
	MaxSize int
	// Err is the underlying I/O error, if any. It is never the payload
	// itself.
	Err error
}

func (e *FrameError) Error() string {
	switch e.Kind {
	case FrameZeroLength:
		return fmt.Sprintf("milter: %s: frame length is zero after %d bytes", e.Direction, e.Consumed)
	case FrameTooLarge:
		return fmt.Sprintf("milter: %s: declared frame length %d exceeds maximum packet size %d after %d bytes",
			e.Direction, e.Declared, e.MaxSize, e.Consumed)
	default:
		if e.Err != nil {
			return fmt.Sprintf("milter: %s: frame i/o error after %d bytes: %v", e.Direction, e.Consumed, e.Err)
		}
		return fmt.Sprintf("milter: %s: frame i/o error after %d bytes", e.Direction, e.Consumed)
	}
}

// Unwrap exposes the underlying I/O error, if any.
func (e *FrameError) Unwrap() error {
	switch e.Kind {
	case FrameZeroLength:
		return ErrFrameZeroLength
	case FrameTooLarge:
		return ErrFrameTooLarge
	default:
		if e.Err != nil {
			return e.Err
		}
		return ErrFrameIO
	}
}

// Is maps structured errors onto the exported sentinel errors.
func (e *FrameError) Is(target error) bool {
	switch target {
	case ErrFrameZeroLength:
		return e.Kind == FrameZeroLength
	case ErrFrameTooLarge:
		return e.Kind == FrameTooLarge
	case ErrFrameIO:
		return e.Kind == FrameIOError
	default:
		// Delegate to the underlying I/O error (e.g. io.ErrUnexpectedEOF,
		// os.ErrDeadlineExceeded) while errors.Is walks the chain.
		return e.Err != nil && errors.Is(e.Err, target)
	}
}

// normalizeMaxSize returns the effective frame size limit.
func normalizeMaxSize(maxSize int) int {
	if maxSize <= 0 {
		return DefaultMaxPacketSize
	}
	return maxSize
}

// readPacket reads a single milter frame from conn.
//
// The wire format is a four-byte big-endian length followed by that many
// bytes, the first of which is the command byte. The declared length is
// validated before any payload buffer is allocated, so an oversize or
// zero-length header cannot trigger a large allocation or panic.
//
// A length of exactly one (a command byte with no payload) is a valid frame
// and yields a Message with an empty Data slice.
func readPacket(conn net.Conn, timeout time.Duration, maxSize int) (*Message, error) {
	if timeout != 0 {
		conn.SetReadDeadline(time.Now().Add(timeout))
		defer conn.SetReadDeadline(time.Time{})
	}

	maxSize = normalizeMaxSize(maxSize)

	// Read the length header, tracking how many header bytes actually
	// arrived so Consumed is accurate on a truncated header.
	var header [4]byte
	n, err := io.ReadFull(conn, header[:])
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, &FrameError{
			Direction: DirRecv,
			Kind:      FrameIOError,
			Consumed:  n,
			Err:       err,
		}
	}

	length := int(binary.BigEndian.Uint32(header[:]))
	const headerSize = 4

	switch {
	case length == 0:
		return nil, &FrameError{
			Direction: DirRecv,
			Kind:      FrameZeroLength,
			Consumed:  headerSize,
		}
	case length > maxSize:
		return nil, &FrameError{
			Direction: DirRecv,
			Kind:      FrameTooLarge,
			Consumed:  headerSize,
			Declared:  length,
			MaxSize:   maxSize,
		}
	}

	// The limit is enforced before this allocation.
	data := make([]byte, length)
	n, err = io.ReadFull(conn, data)
	if err != nil {
		return nil, &FrameError{
			Direction: DirRecv,
			Kind:      FrameIOError,
			Consumed:  headerSize + n,
			Declared:  length,
			MaxSize:   maxSize,
			Err:       err,
		}
	}

	return &Message{
		Code: data[0],
		Data: data[1:],
	}, nil
}

// countingWriter counts bytes successfully written through it.
type countingWriter struct {
	w       io.Writer
	written int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.written += n
	return n, err
}

// writePacket writes a single milter frame to conn. Any failure is wrapped in
// a structured FrameError carrying the send direction and the number of
// on-wire bytes already consumed; frame payload is never included.
func writePacket(conn net.Conn, msg *Message, timeout time.Duration) error {
	if timeout != 0 {
		conn.SetWriteDeadline(time.Now().Add(timeout))
		defer conn.SetWriteDeadline(time.Time{})
	}

	cw := &countingWriter{w: conn}
	buffer := bufio.NewWriter(cw)

	if err := binary.Write(buffer, binary.BigEndian, uint32(len(msg.Data)+1)); err != nil {
		return frameWriteError(cw, err)
	}
	if err := buffer.WriteByte(msg.Code); err != nil {
		return frameWriteError(cw, err)
	}
	if _, err := buffer.Write(msg.Data); err != nil {
		return frameWriteError(cw, err)
	}
	if err := buffer.Flush(); err != nil {
		return frameWriteError(cw, err)
	}

	return nil
}

func frameWriteError(cw *countingWriter, err error) error {
	return &FrameError{
		Direction: DirSend,
		Kind:      FrameIOError,
		Consumed:  cw.written,
		Err:       err,
	}
}
