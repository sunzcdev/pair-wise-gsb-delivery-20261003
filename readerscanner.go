package fastjson

import (
	"fmt"
	"io"
)

// ReaderScanner scans a series of JSON values read from r.
//
// Values may be delimited by whitespace. Adjacent container values
// (objects and arrays) and strings may be not delimited at all,
// while scalar values (numbers, true, false, null) must be delimited
// by whitespace or by the start of a container or a string.
//
// ReaderScanner reads from r only when more data is needed for parsing
// the next JSON value. It stops reading from r as soon as the caller
// stops calling Next, so the unread part of r is left intact.
//
// ReaderScanner may be re-used for subsequent parsing by calling Init again.
//
// ReaderScanner cannot be used from concurrent goroutines.
//
// Use Scanner for scanning JSON values already residing in memory.
type ReaderScanner struct {
	// MaxValueSize limits the maximum size in bytes of a single JSON value
	// read from the reader passed to Init.
	//
	// Next returns false and Error returns non-nil error if a JSON value
	// exceeding this limit is encountered.
	//
	// Zero value means no limit.
	MaxValueSize int

	// r is the reader to scan JSON values from.
	r io.Reader

	// buf contains input bytes read from r, but not consumed yet.
	buf []byte

	// vbuf contains a working copy of the last parsed JSON value.
	vbuf []byte

	// state is the current state of the value scanner.
	state readerScannerState

	// depth is the current nesting depth while scanning a container value.
	depth int

	// scanned is the number of bytes in buf already processed by feed.
	scanned int

	// valueStart is the offset in buf where the current JSON value starts.
	valueStart int

	// eof is set when r returned io.EOF.
	eof bool

	// emptyReads counts consecutive Read calls returning (0, nil).
	emptyReads int

	// err contains the last error.
	err error

	// v contains the last parsed JSON value.
	v *Value

	// c is used for caching JSON values.
	c cache
}

type readerScannerState int

const (
	// rsStateWS means the scanner skips whitespace before the next value.
	rsStateWS readerScannerState = iota

	// rsStateScalar means the scanner is inside a scalar value.
	rsStateScalar

	// rsStateString means the scanner is inside a top-level string value.
	rsStateString

	// rsStateStringEscape means the scanner is after '\' inside a top-level string value.
	rsStateStringEscape

	// rsStateContainer means the scanner is inside an object or an array.
	rsStateContainer

	// rsStateContainerString means the scanner is inside a string inside a container.
	rsStateContainerString

	// rsStateContainerStringEscape means the scanner is after '\' inside a string inside a container.
	rsStateContainerStringEscape
)

const readerScannerBufSize = 4096

// Init initializes sc with the given r.
//
// r may contain multiple JSON values, which may be delimited by whitespace.
//
// Init resets MaxValueSize to 0, so it must be set after Init if needed.
func (sc *ReaderScanner) Init(r io.Reader) {
	sc.MaxValueSize = 0
	sc.r = r
	sc.buf = sc.buf[:0]
	sc.state = rsStateWS
	sc.depth = 0
	sc.scanned = 0
	sc.valueStart = 0
	sc.eof = false
	sc.emptyReads = 0
	sc.err = nil
	sc.v = nil
}

// Next parses the next JSON value from r passed to Init.
//
// Returns true on success. The parsed value is available via Value call.
//
// Returns false either on error or on the end of r.
// Call Error in order to determine the cause of the returned false:
//
//   - Error returns nil on a clean end of r, when no more JSON values are left.
//   - Error returns a non-nil error if r ends in the middle of a JSON value,
//     i.e. the last value is truncated.
//   - Error returns a non-nil error wrapping the underlying error
//     if reading from r fails.
func (sc *ReaderScanner) Next() bool {
	if sc.err != nil {
		return false
	}

	for {
		if sc.state == rsStateWS && sc.scanned > 0 {
			// Drop the whitespace already scanned.
			sc.buf = append(sc.buf[:0], sc.buf[sc.scanned:]...)
			sc.scanned = 0
			sc.valueStart = 0
		}
		if end, ok := sc.feed(); ok {
			return sc.finishValue(sc.valueStart, end)
		}
		if sc.eof {
			switch sc.state {
			case rsStateWS:
				// Clean end of the stream.
				sc.err = errEOF
			case rsStateScalar:
				// A scalar value is terminated by the end of the stream.
				return sc.finishValue(sc.valueStart, len(sc.buf))
			default:
				sc.err = fmt.Errorf("unexpected end of stream: the last JSON value is truncated")
			}
			return false
		}
		if sc.err = sc.readMore(); sc.err != nil {
			return false
		}
		if sc.MaxValueSize > 0 && sc.state != rsStateWS && len(sc.buf)-sc.valueStart > sc.MaxValueSize {
			sc.err = fmt.Errorf("the size of JSON value exceeds %d bytes", sc.MaxValueSize)
			return false
		}
	}
}

// Error returns the last error.
//
// It returns nil if Next returned false because of a clean end of r.
// See Next for the list of possible errors.
func (sc *ReaderScanner) Error() error {
	if sc.err == errEOF {
		return nil
	}
	return sc.err
}

// Value returns the last parsed value.
//
// The value is valid until the next call to Next.
func (sc *ReaderScanner) Value() *Value {
	return sc.v
}

// feed consumes the unscanned part of sc.buf according to the current
// scanner state.
//
// It returns the offset of the end of the JSON value in sc.buf and true
// once the value end is found. The value starts at sc.valueStart.
// The scanner state is preserved between calls, so the value may be
// delivered by an arbitrary number of Read calls.
func (sc *ReaderScanner) feed() (int, bool) {
	b := sc.buf
	for i := sc.scanned; i < len(b); i++ {
		ch := b[i]
		switch sc.state {
		case rsStateWS:
			if isWSByte(ch) {
				continue
			}
			sc.valueStart = i
			switch ch {
			case '{', '[':
				sc.depth = 1
				sc.state = rsStateContainer
			case '"':
				sc.state = rsStateString
			default:
				sc.state = rsStateScalar
			}
		case rsStateScalar:
			if isWSByte(ch) || ch == '{' || ch == '[' || ch == '"' {
				// The scalar ends before ch. Leave ch unconsumed,
				// so it is scanned as whitespace or as the start
				// of the next value on the next feed call.
				sc.state = rsStateWS
				sc.scanned = i
				return i, true
			}
		case rsStateString:
			switch ch {
			case '\\':
				sc.state = rsStateStringEscape
			case '"':
				sc.state = rsStateWS
				sc.scanned = i + 1
				return i + 1, true
			}
		case rsStateStringEscape:
			sc.state = rsStateString
		case rsStateContainer:
			switch ch {
			case '"':
				sc.state = rsStateContainerString
			case '{', '[':
				sc.depth++
			case '}', ']':
				sc.depth--
				if sc.depth == 0 {
					sc.state = rsStateWS
					sc.scanned = i + 1
					return i + 1, true
				}
			}
		case rsStateContainerString:
			switch ch {
			case '\\':
				sc.state = rsStateContainerStringEscape
			case '"':
				sc.state = rsStateContainer
			}
		case rsStateContainerStringEscape:
			sc.state = rsStateContainerString
		}
	}
	sc.scanned = len(b)
	return len(b), false
}

// finishValue parses the complete JSON value located in sc.buf[start:end],
// removes it from the buffer and stores the parsed value.
func (sc *ReaderScanner) finishValue(start, end int) bool {
	value := sc.buf[start:end]

	if sc.MaxValueSize > 0 && len(value) > sc.MaxValueSize {
		sc.err = fmt.Errorf("the size of JSON value exceeds %d bytes", sc.MaxValueSize)
		return false
	}

	// Parse a copy of the value, since sc.buf is compacted below and
	// overwritten by subsequent reads, and since string unescaping may
	// modify the parsed bytes in place.
	sc.c.reset()
	sc.vbuf = append(sc.vbuf[:0], value...)
	sc.buf = append(sc.buf[:0], sc.buf[end:]...)
	sc.scanned = 0
	sc.valueStart = 0
	sc.state = rsStateWS
	v, tail, err := sc.c.parseValue(b2s(sc.vbuf), 0)
	if err != nil {
		sc.err = fmt.Errorf("cannot parse JSON value: %s", err)
		return false
	}
	if tail = skipWS(tail); len(tail) > 0 {
		sc.err = fmt.Errorf("unexpected tail after JSON value: %q", startEndString(tail))
		return false
	}
	sc.v = v
	return true
}

// readMore reads the next chunk of data from r into sc.buf.
//
// Data returned together with an error is consumed before the error
// is reported.
func (sc *ReaderScanner) readMore() error {
	var b [readerScannerBufSize]byte
	n, err := sc.r.Read(b[:])
	if n > 0 {
		sc.buf = append(sc.buf, b[:n]...)
		sc.emptyReads = 0
	}
	if err == io.EOF {
		// The data returned together with io.EOF has been
		// appended to sc.buf above and is consumed first.
		sc.eof = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read JSON values: %w", err)
	}
	if n == 0 {
		sc.emptyReads++
		if sc.emptyReads > 100 {
			return fmt.Errorf("cannot read JSON values: %w", io.ErrNoProgress)
		}
	}
	return nil
}

func isWSByte(ch byte) bool {
	return ch == 0x20 || ch == 0x0A || ch == 0x09 || ch == 0x0D
}
