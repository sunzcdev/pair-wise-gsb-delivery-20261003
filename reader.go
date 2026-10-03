package fastjson

import (
	"fmt"
	"io"
)

// DefaultMaxValueSize is the default maximum size in bytes of a single JSON
// value read by Reader.
//
// The limit may be changed via Reader.MaxValueSize.
const DefaultMaxValueSize = 4 * 1024 * 1024

// Reader reads a series of JSON values from an io.Reader.
//
// Values may be delimited by whitespace. Adjacent container values
// (objects and arrays) may be used without a delimiter.
//
// Reader reads from the underlying io.Reader only when more data is needed
// for parsing the next JSON value. It doesn't read the remaining input
// after the caller stops iterating.
//
// Reader may be re-used for subsequent parsing by calling Init again.
//
// Reader cannot be used from concurrent goroutines.
//
// Use Scanner for scanning a series of JSON values already loaded in memory.
type Reader struct {
	// MaxValueSize is the maximum allowed size in bytes of a single JSON value.
	//
	// Next returns false and Error returns non-nil error if a value bigger
	// than MaxValueSize is encountered.
	//
	// If MaxValueSize is 0, then DefaultMaxValueSize is used.
	//
	// Init doesn't reset MaxValueSize.
	MaxValueSize int

	// r is the underlying reader.
	r io.Reader

	// buf contains data read from r, which hasn't been consumed yet.
	buf []byte

	// pos is the scan position in buf.
	pos int

	// start is the offset in buf of the value being scanned.
	start int

	// state is the current scan state.
	state scanState

	// depth is the current nesting depth for containers.
	depth int

	// inString is set when the scanner is inside a string.
	inString bool

	// escaped is set when the previous char inside a string was '\\'.
	escaped bool

	// eof is set when r returned io.EOF.
	eof bool

	// readErr contains a pending error from r, which must be reported
	// only after the data read together with the error is consumed.
	readErr error

	// err contains the last error.
	err error

	// v contains the last parsed JSON value.
	v *Value

	// c is used for caching JSON values.
	c cache
}

type scanState int

const (
	// scanStart means the scanner is looking for the start of the next value.
	scanStart scanState = iota

	// scanValue means the scanner is inside a container or a string.
	scanValue

	// scanScalar means the scanner is inside a scalar value
	// (number, true, false, null, etc).
	scanScalar
)

// Init initializes r with the given rd.
//
// rd may contain multiple JSON values, which may be delimited by whitespace.
//
// Init doesn't reset MaxValueSize.
func (r *Reader) Init(rd io.Reader) {
	r.r = rd
	r.buf = r.buf[:0]
	r.pos = 0
	r.start = 0
	r.state = scanStart
	r.depth = 0
	r.inString = false
	r.escaped = false
	r.eof = false
	r.readErr = nil
	r.err = nil
	r.v = nil
}

// Next reads and parses the next JSON value from rd passed to Init.
//
// Returns true on success. The parsed value is available via Value call.
//
// Returns false either on error or on the end of rd.
// Call Error in order to determine the cause of the returned false:
//
//   - Error returns nil on the normal end of rd.
//   - Error returns an error matching io.ErrUnexpectedEOF if rd ends
//     in the middle of a JSON value.
//   - Error returns the underlying reader error if rd fails.
//   - Error returns an error if a value exceeds MaxValueSize.
func (r *Reader) Next() bool {
	if r.err != nil {
		return false
	}
	r.c.reset()
	r.v = nil

	// Compact consumed space in buf. This invalidates the value returned
	// by the previous Next call, which is fine, since the value is valid
	// only until the Next call.
	if r.start > 0 {
		r.buf = r.buf[:copy(r.buf, r.buf[r.start:])]
		r.pos -= r.start
		r.start = 0
	}

	for {
		start, end, ok := r.scan()
		if ok {
			if end-start > r.maxValueSize() {
				r.err = fmt.Errorf("too big JSON value: it exceeds %d bytes", r.maxValueSize())
				return false
			}
			return r.parseValue(start, end)
		}
		if r.pos-r.start > r.maxValueSize() {
			r.err = fmt.Errorf("too big JSON value: it exceeds %d bytes", r.maxValueSize())
			return false
		}
		if r.readErr != nil {
			r.err = fmt.Errorf("cannot read from the underlying reader: %w", r.readErr)
			return false
		}
		if r.eof {
			switch r.state {
			case scanScalar:
				// A scalar value is terminated by the end of rd.
				start, end := r.start, len(r.buf)
				r.start = end
				r.state = scanStart
				return r.parseValue(start, end)
			case scanStart:
				r.err = errEOF
			default:
				r.err = fmt.Errorf("unexpected end of JSON value: %w", io.ErrUnexpectedEOF)
			}
			return false
		}
		if err := r.fill(); err != nil {
			r.err = fmt.Errorf("cannot read from the underlying reader: %w", err)
			return false
		}
	}
}

// Error returns the last error.
//
// It returns nil if Next returned false on the normal end of the input.
// See Next for the kinds of errors it may return.
func (r *Reader) Error() error {
	if r.err == errEOF {
		return nil
	}
	return r.err
}

// Value returns the last parsed value.
//
// The value is valid until the next call to Next or Init.
func (r *Reader) Value() *Value {
	return r.v
}

func (r *Reader) maxValueSize() int {
	if r.MaxValueSize > 0 {
		return r.MaxValueSize
	}
	return DefaultMaxValueSize
}

// fill reads more data from r into buf.
func (r *Reader) fill() error {
	if len(r.buf) == cap(r.buf) {
		newCap := 2 * cap(r.buf)
		if newCap < 4096 {
			newCap = 4096
		}
		newBuf := make([]byte, len(r.buf), newCap)
		copy(newBuf, r.buf)
		r.buf = newBuf
	}
	n, err := r.r.Read(r.buf[len(r.buf):cap(r.buf)])
	r.buf = r.buf[:len(r.buf)+n]
	switch {
	case err == io.EOF:
		// The data returned together with io.EOF is consumed first.
		r.eof = true
		return nil
	case err != nil:
		// The data returned together with the error is consumed first.
		r.readErr = err
		return nil
	default:
		return nil
	}
}

// scan scans buf for the end of the current JSON value.
//
// It returns the start and end offsets of the value in buf when ok is true.
// It returns ok=false when more data is needed.
func (r *Reader) scan() (start, end int, ok bool) {
	for r.pos < len(r.buf) {
		c := r.buf[r.pos]
		if r.inString {
			r.pos++
			switch {
			case r.escaped:
				r.escaped = false
			case c == '\\':
				r.escaped = true
			case c == '"':
				r.inString = false
				if r.depth == 0 {
					return r.finishValue()
				}
			}
			continue
		}
		switch r.state {
		case scanStart:
			r.pos++
			if isWSByte(c) {
				r.start = r.pos
				continue
			}
			switch c {
			case '{', '[':
				r.depth = 1
				r.state = scanValue
			case '"':
				r.inString = true
				r.state = scanValue
			default:
				r.state = scanScalar
			}
		case scanValue:
			r.pos++
			switch c {
			case '{', '[':
				r.depth++
			case '}', ']':
				r.depth--
				if r.depth == 0 {
					return r.finishValue()
				}
			case '"':
				r.inString = true
			}
		case scanScalar:
			if isWSByte(c) || isScalarDelimiter(c) {
				return r.finishValue()
			}
			r.pos++
		}
	}
	return 0, 0, false
}

// finishValue finishes the current value at r.pos and returns its offsets.
func (r *Reader) finishValue() (start, end int, ok bool) {
	start, end = r.start, r.pos
	r.start = end
	r.state = scanStart
	return start, end, true
}

// parseValue parses the value located at buf[start:end] and stores it
// into r.v.
func (r *Reader) parseValue(start, end int) bool {
	v, tail, err := r.c.parseValue(b2s(r.buf[start:end]), 0)
	if err != nil {
		r.err = err
		return false
	}
	if len(tail) > 0 {
		r.err = fmt.Errorf("unexpected tail after JSON value: %q", startEndString(tail))
		return false
	}
	r.v = v
	return true
}

func isWSByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isScalarDelimiter(c byte) bool {
	switch c {
	case '{', '[', '}', ']', ',', '"':
		return true
	default:
		return false
	}
}
