package wsutil

import (
	"errors"

	"github.com/gobwas/ws"
)

// ErrObserverTerminated is returned from Reader's methods after its
// FrameObserver has returned an error.
//
// A non-nil error returned by the observer terminates the Reader: no further
// events are delivered to it and every subsequent NextFrame(), Read() and
// Discard() call fails with ErrObserverTerminated, until the Reader is reset.
var ErrObserverTerminated = errors.New("frame observer terminated")

// FrameEventKind describes the kind of a FrameEvent.
type FrameEventKind byte

const (
	// EventHeaderChecked is reported when a frame header has been read and
	// has passed all enabled protocol checks (RFC6455 validation, size limit
	// and negotiated extensions). The frame payload has not been consumed
	// yet.
	EventHeaderChecked FrameEventKind = iota + 1

	// EventPayloadConsumed is reported when all payload bytes of a frame
	// have actually been consumed from the underlying reader — either by the
	// application reading them, or by an internal discard. It is never
	// reported for a frame whose payload was only partially consumed.
	EventPayloadConsumed

	// EventFailed is reported when reading a frame or its payload fails
	// midway (for example an unexpected underlying io.EOF or a header
	// parsing error). An event carries the partial frame information
	// available at the moment of failure.
	EventFailed
)

// FrameEvent represents a single observation of a frame lifecycle point.
//
// FrameIndex is zero-based sequential number of the frame on the connection,
// counting every frame, including control frames.
//
// MessageIndex is zero-based sequential number of the data message the frame
// belongs to. Intermediate control frames do not start a new data message, so
// they reuse the index of the in-flight fragmented message. Standalone
// control frames that are received outside of any data message report -1.
//
// WireLength is the total number of bytes the frame occupies on the wire:
// header bytes plus payload length. For events that do not carry a fully
// parsed header (header read failure) it is zero.
type FrameEvent struct {
	Kind         FrameEventKind
	FrameIndex   int64
	MessageIndex int64
	Header       ws.Header
	WireLength   int64
	Err          error
}

// FrameObserver receives notifications about frames processed by a Reader.
//
// Every method call is synchronous and happens before the corresponding
// Reader call returns. Events are never buffered, so a Reader reset does not
// result in any stale, previously buffered events being delivered.
//
// Returning a non-nil error from any method terminates the Reader (see
// ErrObserverTerminated). No EventFailed is emitted in that case and the
// error is returned to the caller of the Reader method that triggered the
// event.
type FrameObserver interface {
	ObserveFrame(FrameEvent) error
}

// FrameObserverFunc is an adapter to allow the use of an ordinary function as
// a FrameObserver.
type FrameObserverFunc func(FrameEvent) error

// ObserveFrame implements FrameObserver.
func (fn FrameObserverFunc) ObserveFrame(e FrameEvent) error {
	return fn(e)
}
