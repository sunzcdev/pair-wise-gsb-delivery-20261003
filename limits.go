package mbox

import (
	"errors"
	"strconv"
)

// Budget identifies the kind of budget a Reader refuses to exceed while
// processing a single message.
type Budget int

const (
	// RawBytes limits the number of raw, escaped bytes a single message
	// occupies in the mbox input stream.
	RawBytes Budget = iota
	// OutputBytes limits the number of unescaped bytes delivered to the
	// caller for a single message.
	OutputBytes
	// LineBytes limits the length (excluding the line terminator) of a
	// single raw input line within a message.
	LineBytes
	// SkipBytes limits the number of raw bytes scanned while looking for the
	// boundary of a skipped message.
	SkipBytes
)

// String returns a human readable description of the budget.
func (b Budget) String() string {
	switch b {
	case RawBytes:
		return "raw message size"
	case OutputBytes:
		return "unescaped message size"
	case LineBytes:
		return "line length"
	case SkipBytes:
		return "skip scan size"
	default:
		return "unknown"
	}
}

// Limits bounds the amount of data a Reader is willing to process for a single
// message. Every field is a number of bytes. The zero value of a field (or a
// negative value) disables the corresponding limit.
type Limits struct {
	// MaxRawBytes bounds the raw size of a single message, counted from the
	// start of the message body (right after its opening "From " separator
	// line) up to, but not including, the next separator line.
	MaxRawBytes int64
	// MaxOutputBytes bounds the size of the unescaped message delivered to
	// the caller (with "\n" line endings normalized to "\r\n" and leading
	// ">" escape characters removed from body lines starting with "From ").
	MaxOutputBytes int64
	// MaxLineBytes bounds the length, excluding the line terminator, of a
	// single raw line within a message.
	MaxLineBytes int64
	// MaxSkipBytes bounds the number of raw bytes scanned while searching
	// for the next message boundary in Reader.SkipMessage.
	MaxSkipBytes int64
}

// LimitError reports that a message exceeds one of the configured Limits.
//
// Message is the zero-based index of the offending message. Offset is the
// number of raw bytes consumed from the input stream at the point the limit
// was reached; for RawBytes and LineBytes violations it points at the first
// byte that would cross the budget. Budget identifies the exceeded budget and
// Limit is the configured budget value.
type LimitError struct {
	Message int
	Offset  int64
	Budget  Budget
	Limit   int64
}

func (e *LimitError) Error() string {
	return "mbox: message " + strconv.Itoa(e.Message) + " exceeds " + e.Budget.String() +
		" limit of " + strconv.FormatInt(e.Limit, 10) + " bytes at raw offset " +
		strconv.FormatInt(e.Offset, 10)
}

// ErrBoundaryNotFound is returned by NextMessage when the previous message is
// in an error state and its boundary has not been located yet. SkipMessage
// must be used to move past it.
var ErrBoundaryNotFound = errors.New("mbox: message boundary not found, call SkipMessage")
