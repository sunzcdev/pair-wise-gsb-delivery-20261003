// "THE BEER-WARE LICENSE" (Revision 42):
// <tobias.rehbein@web.de> wrote this file. As long as you retain this notice
// you can do whatever you want with this stuff. If we meet some day, and you
// think this stuff is worth it, you can buy me a beer in return.
//                                                             Tobias Rehbein

package mbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
)

// ErrInvalidFormat is the error returned by the NextMessage method of Reader if
// its content is malformed in a way that it is not possible to extract a
// message.
var ErrInvalidFormat = errors.New("invalid mbox format")

// BudgetKind identifies which of the budgets configured in Reader.Limits a
// LimitError refers to.
type BudgetKind int

const (
	// BudgetRawBytes is the budget bounding the raw bytes a single message
	// may consume from the underlying stream.
	BudgetRawBytes BudgetKind = iota
	// BudgetOutputBytes is the budget bounding the unescaped bytes a single
	// message may deliver to the caller.
	BudgetOutputBytes
	// BudgetLineBytes is the budget bounding the length of a single raw
	// line.
	BudgetLineBytes
	// BudgetSkipScanBytes is the budget bounding how many raw bytes may be
	// scanned while skipping the remainder of a message.
	BudgetSkipScanBytes
)

func (k BudgetKind) String() string {
	switch k {
	case BudgetRawBytes:
		return "raw bytes"
	case BudgetOutputBytes:
		return "output bytes"
	case BudgetLineBytes:
		return "line length"
	case BudgetSkipScanBytes:
		return "skip scan"
	default:
		return "unknown"
	}
}

// LimitError is returned when a message exceeds one of the budgets configured
// in Reader.Limits.
type LimitError struct {
	// MessageIndex is the zero-based index of the offending message.
	MessageIndex int
	// RawOffset is the offset in the raw underlying stream at which the
	// budget was exceeded.
	RawOffset int64
	// Budget is the kind of budget that was exceeded.
	Budget BudgetKind
	// Limit is the configured budget that was exceeded, in bytes.
	Limit int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("mbox: message %d exceeded %s budget of %d bytes at raw offset %d",
		e.MessageIndex, e.Budget, e.Limit, e.RawOffset)
}

// Limits configures the per-message budgets enforced by Reader. A zero value
// disables the corresponding budget.
type Limits struct {
	// MaxRawBytes bounds the number of raw bytes a single message may
	// consume from the underlying stream, including line endings but
	// excluding the terminating "From " separator line.
	MaxRawBytes int64
	// MaxOutputBytes bounds the number of unescaped bytes a single message
	// may deliver to the caller.
	MaxOutputBytes int64
	// MaxLineBytes bounds the length of a single raw line.
	MaxLineBytes int64
	// MaxSkipScanBytes bounds the number of raw bytes scanned while
	// skipping the remainder of a message, either explicitly via
	// Reader.SkipMessage or implicitly while resynchronizing after a
	// LimitError.
	MaxSkipScanBytes int64
}

// countingReader counts the number of bytes read from an underlying reader.
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

type messageReader struct {
	r                  *bufio.Reader
	parent             *Reader
	index              int
	rawStart           int64
	next               bytes.Buffer
	atEOF, atSeparator bool
	atMiddleOfLine     bool
	lineBytes          int64
	outBytes           int64
	rawEnd             int64
	rawEndSet          bool
	err                error
}

// rawUsed returns the number of raw bytes consumed by this message so far.
func (mr *messageReader) rawUsed() int64 {
	off := mr.parent.rawOffset()
	if mr.rawEndSet {
		off = mr.rawEnd
	}
	return off - mr.rawStart
}

// fail records a sticky LimitError and returns it.
func (mr *messageReader) fail(kind BudgetKind, limit int64) error {
	err := &LimitError{
		MessageIndex: mr.index,
		RawOffset:    mr.parent.rawOffset(),
		Budget:       kind,
		Limit:        limit,
	}
	mr.err = err
	return err
}

// accountLine accounts a raw line chunk against the line length budget.
func (mr *messageReader) accountLine(n int, isPrefix bool) error {
	mr.lineBytes += int64(n)
	if lim := mr.parent.Limits.MaxLineBytes; lim > 0 && mr.lineBytes > lim {
		return mr.fail(BudgetLineBytes, lim)
	}
	if !isPrefix {
		mr.lineBytes = 0
	}
	return nil
}

// fill reads the next chunk of the raw message into mr.next.
func (mr *messageReader) fill() error {
	offBefore := mr.parent.rawOffset()
	b, isPrefix, err := mr.r.ReadLine()
	if err != nil {
		mr.atEOF = true
		return err
	}
	if err := mr.accountLine(len(b), isPrefix); err != nil {
		return err
	}

	if !mr.atMiddleOfLine {
		if bytes.HasPrefix(b, header) {
			mr.atSeparator = true
			mr.rawEnd, mr.rawEndSet = offBefore, true
			return nil
		} else if len(b) == 0 {
			// Check if the next line is separator. In such case the new
			// line should not be written to not have double new line.
			offBefore = mr.parent.rawOffset()
			b, isPrefix, err = mr.r.ReadLine()
			if err != nil {
				mr.atEOF = true
				return err
			}
			if err := mr.accountLine(len(b), isPrefix); err != nil {
				return err
			}

			if bytes.HasPrefix(b, header) {
				mr.atSeparator = true
				mr.rawEnd, mr.rawEndSet = offBefore, true
				return nil
			}

			mr.next.Write([]byte("\r\n"))
		}

		if bytes.HasPrefix(b, escapedHeader) {
			b = b[1:]
		}
	}

	mr.next.Write(b)
	if !isPrefix {
		mr.next.Write([]byte("\r\n"))
	}
	mr.atMiddleOfLine = isPrefix

	if lim := mr.parent.Limits.MaxRawBytes; lim > 0 && mr.rawUsed() > lim {
		return mr.fail(BudgetRawBytes, lim)
	}
	return nil
}

func (mr *messageReader) Read(p []byte) (int, error) {
	if mr.err != nil {
		return 0, mr.err
	}
	if mr.atEOF || mr.atSeparator {
		return 0, io.EOF
	}

	if mr.next.Len() == 0 {
		if err := mr.fill(); err != nil {
			return 0, err
		}
		if mr.atEOF || mr.atSeparator {
			return 0, io.EOF
		}
	}

	if lim := mr.parent.Limits.MaxOutputBytes; lim > 0 {
		rem := lim - mr.outBytes
		if rem <= 0 {
			return 0, mr.fail(BudgetOutputBytes, lim)
		}
		if int64(len(p)) > rem {
			p = p[:rem]
		}
	}

	n, _ := mr.next.Read(p)
	mr.outBytes += int64(n)
	return n, nil
}

// Reader reads an mbox archive.
type Reader struct {
	cr *countingReader
	r  *bufio.Reader
	mr *messageReader

	// Limits bounds the resources a single message may consume. It should
	// be set before the first call to NextMessage and must not be modified
	// concurrently with Reader method calls.
	Limits Limits

	started       bool
	eof           bool
	nextIndex     int
	needResync    bool
	resyncMidLine bool
	resyncIndex   int
}

// NewReader returns a new Reader to read messages from mbox file format data
// provided by io.Reader r.
func NewReader(r io.Reader) *Reader {
	cr := &countingReader{r: r}
	return &Reader{cr: cr, r: bufio.NewReader(cr)}
}

// rawOffset returns the current logical offset in the raw underlying stream.
func (r *Reader) rawOffset() int64 {
	return r.cr.n - int64(r.r.Buffered())
}

// skipScan scans the raw stream for the next "From " separator line,
// consuming at most Limits.MaxSkipScanBytes raw bytes. It returns io.EOF if
// the stream ended before a boundary was found.
func (r *Reader) skipScan() error {
	lim := r.Limits.MaxSkipScanBytes
	var scanned int64
	for {
		before := r.rawOffset()
		b, isPrefix, err := r.r.ReadLine()
		if err != nil {
			return err
		}
		scanned += r.rawOffset() - before
		if lim > 0 && scanned > lim {
			return &LimitError{
				MessageIndex: r.resyncIndex,
				RawOffset:    r.rawOffset(),
				Budget:       BudgetSkipScanBytes,
				Limit:        lim,
			}
		}

		if !r.resyncMidLine && bytes.HasPrefix(b, header) {
			// Consume the rest of the separator line.
			for isPrefix {
				before = r.rawOffset()
				_, isPrefix, err = r.r.ReadLine()
				if err != nil {
					return err
				}
				scanned += r.rawOffset() - before
				if lim > 0 && scanned > lim {
					return &LimitError{
						MessageIndex: r.resyncIndex,
						RawOffset:    r.rawOffset(),
						Budget:       BudgetSkipScanBytes,
						Limit:        lim,
					}
				}
			}
			r.resyncMidLine = false
			return nil
		}
		r.resyncMidLine = isPrefix
	}
}

// SkipMessage discards the remainder of the current message without
// delivering it to the caller. The scan for the next message boundary
// consumes at most Limits.MaxSkipScanBytes raw bytes; if no boundary is found
// within that budget a *LimitError of kind BudgetSkipScanBytes is returned
// and the skip may be resumed by calling SkipMessage again. After a
// successful skip, NextMessage continues with the following message if a
// boundary was found, or reports io.EOF if the stream ended.
func (r *Reader) SkipMessage() error {
	if r.mr != nil {
		if r.mr.atEOF {
			r.mr = nil
			r.eof = true
			return nil
		}
		if r.mr.atSeparator {
			r.mr = nil
			return nil
		}
		r.resyncMidLine = r.mr.atMiddleOfLine
		r.resyncIndex = r.mr.index
		r.mr = nil
		r.needResync = true
	}
	if !r.needResync {
		return nil
	}
	if err := r.skipScan(); err != nil {
		if err == io.EOF {
			r.needResync = false
			r.eof = true
			return nil
		}
		return err
	}
	r.needResync = false
	return nil
}

// NextMessage returns the next message text (containing both the header and the
// body). It will return io.EOF if there are no messages left.
//
// If the previous message was not fully read, its remainder is discarded;
// the discard is subject to the budgets configured in Limits. If a budget is
// exceeded, NextMessage returns a *LimitError; the following call to
// NextMessage resumes at the next message boundary found within
// Limits.MaxSkipScanBytes, or reports io.EOF if the stream ended first.
func (r *Reader) NextMessage() (io.Reader, error) {
	if r.eof {
		return nil, io.EOF
	}

	if r.mr != nil {
		if _, err := io.Copy(ioutil.Discard, r.mr); err != nil {
			if le, ok := err.(*LimitError); ok {
				r.resyncMidLine = r.mr.atMiddleOfLine
				r.resyncIndex = r.mr.index
				r.needResync = true
				r.mr = nil
				return nil, le
			}
			return nil, err
		}
		if r.mr.atEOF {
			r.eof = true
			return nil, io.EOF
		}
		r.mr = nil
	} else if r.needResync {
		if err := r.skipScan(); err != nil {
			if err == io.EOF {
				r.needResync = false
				r.eof = true
				return nil, io.EOF
			}
			return nil, err
		}
		r.needResync = false
	} else if !r.started {
		for {
			b, isPrefix, err := r.r.ReadLine()
			if err != nil {
				return nil, err
			}

			isFromLine := bytes.HasPrefix(b, header)

			// Discard the rest of the line.
			for isPrefix {
				_, isPrefix, err = r.r.ReadLine()
				if err != nil {
					return nil, err
				}
			}
			if len(b) == 0 {
				continue
			}
			if isFromLine {
				break
			} else {
				return nil, ErrInvalidFormat
			}
		}
		r.started = true
	}

	mr := &messageReader{r: r.r, parent: r, index: r.nextIndex, rawStart: r.rawOffset()}
	r.nextIndex++
	r.mr = mr
	return mr, nil
}
