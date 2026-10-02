package mbox

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Envelope contains the information carried by the mbox "From " separator
// line that precedes a message.
type Envelope struct {
	// RawLine is the original separator line, without the trailing line
	// break (neither LF nor CRLF). The line is reassembled in full, even if
	// it exceeds the underlying reader's buffer size.
	RawLine string

	// From is the sender as it appears in the separator line, i.e. the
	// whitespace-delimited token right after "From ". It may be empty if the
	// separator line does not contain a sender.
	From string

	// Date is the time parsed from the separator line. It is the zero value
	// if the separator line does not carry a recognizable date. In lenient
	// mode this is not an error, in strict mode NextMessage and
	// NextMessageWithEnvelope return an *EnvelopeError instead.
	Date time.Time
}

// ErrUnknownDate is returned, wrapped in an *EnvelopeError, by a strict
// Reader when a separator line does not carry a recognizable date.
var ErrUnknownDate = errors.New("mbox: unrecognized date in separator line")

// ErrTrailingText is returned, wrapped in an *EnvelopeError, by a strict
// Reader when a separator line carries unexpected text after the date.
var ErrTrailingText = errors.New("mbox: unexpected trailing text in separator line")

// EnvelopeError is returned by a strict Reader when a separator line cannot
// be parsed completely. It locates the offending line and carries the raw
// separator text.
type EnvelopeError struct {
	// Line is the 1-based number of the offending separator line.
	Line int
	// RawLine is the raw separator line, without the trailing line break.
	RawLine string
	// Err is either ErrUnknownDate or ErrTrailingText.
	Err error
}

func (e *EnvelopeError) Error() string {
	return fmt.Sprintf("mbox: line %d: %v: %q", e.Line, e.Err, e.RawLine)
}

// parseEnvelope parses a full "From " separator line. The line must not carry
// a trailing line break. In lenient mode a message is always returned, using
// the zero time for any unreadable date. In strict mode an *EnvelopeError is
// returned when the date is missing or unrecognized, or when unexpected text
// follows the date.
func parseEnvelope(line string, lineNo int, strict bool) (*Envelope, error) {
	env := &Envelope{RawLine: line}

	rest := line[len(header):]

	// The sender is the first whitespace-delimited token. A line that starts
	// with whitespace or has no token at all simply has an empty sender.
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		env.From = rest[:i]
		rest = rest[i+1:]
	} else {
		env.From = rest
		rest = ""
	}

	datePart := strings.TrimRight(rest, " \t")

	// The mbox date uses time.ANSIC, which has a fixed length. Accept a
	// recognizable prefix in lenient mode; strict mode rejects any trailing
	// text that is not mere whitespace.
	if len(datePart) < lenANSIC {
		if strict {
			return env, &EnvelopeError{Line: lineNo, RawLine: line, Err: ErrUnknownDate}
		}
		return env, nil
	}

	date, err := time.Parse(time.ANSIC, datePart[:lenANSIC])
	if err != nil {
		if strict {
			return env, &EnvelopeError{Line: lineNo, RawLine: line, Err: ErrUnknownDate}
		}
		return env, nil
	}
	env.Date = date

	if strict && len(datePart) > lenANSIC {
		return env, &EnvelopeError{Line: lineNo, RawLine: line, Err: ErrTrailingText}
	}

	return env, nil
}

// lenANSIC is the length of a time.ANSIC timestamp, e.g.
// "Thu Jan  1 00:00:01 2015".
const lenANSIC = len("Mon Jan _2 15:04:05 2006")
