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
	"io"
	"io/ioutil"
)

// ErrInvalidFormat is the error returned by the NextMessage method of Reader if
// its content is malformed in a way that it is not possible to extract a
// message.
var ErrInvalidFormat = errors.New("invalid mbox format")

// lineReader wraps bufio.Reader.ReadLine and counts completed logical lines.
// A long line delivered in several fragments counts once, when its final
// fragment has been read.
type lineReader struct {
	r      *bufio.Reader
	lineNo int
}

func (l *lineReader) readFragment() ([]byte, bool, error) {
	b, isPrefix, err := l.r.ReadLine()
	if !isPrefix {
		l.lineNo++
	}
	return b, isPrefix, err
}

// readLine reads a complete logical line, reassembling fragments of long
// lines, and returns the reassembled line and its 1-based line number.
func (l *lineReader) readLine() ([]byte, int, error) {
	b, isPrefix, err := l.readFragment()
	// ReadLine reuses its internal buffer on the next call, so copy the
	// first fragment before reading the rest of a long line.
	line := append([]byte(nil), b...)
	for isPrefix && err == nil {
		var more []byte
		more, isPrefix, err = l.readFragment()
		line = append(line, more...)
	}
	return line, l.lineNo, err
}

type messageReader struct {
	lr *lineReader

	next           bytes.Buffer
	atEOF          bool
	sep            string
	sepLine        int
	atMiddleOfLine bool
}

func (mr *messageReader) Read(p []byte) (int, error) {
	if mr.atEOF || mr.sep != "" {
		return 0, io.EOF
	}

	if mr.next.Len() == 0 {
		b, isPrefix, err := mr.lr.readFragment()
		if err != nil {
			mr.atEOF = true
			return 0, err
		}

		if !mr.atMiddleOfLine {
			if bytes.HasPrefix(b, header) {
				mr.readSeparator(b, isPrefix)
				return 0, io.EOF
			} else if len(b) == 0 {
				// Check if the next line is separator. In such case the new
				// line should not be written to not have double new line.
				b, isPrefix, err = mr.lr.readFragment()
				if err != nil {
					mr.atEOF = true
					return 0, err
				}

				if bytes.HasPrefix(b, header) {
					mr.readSeparator(b, isPrefix)
					return 0, io.EOF
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
	}

	return mr.next.Read(p)
}

// readSeparator consumes any remaining fragments of a "From " separator line
// whose first fragment is b and records the reassembled raw line together
// with its 1-based line number.
func (mr *messageReader) readSeparator(b []byte, isPrefix bool) {
	line := append([]byte(nil), b...)
	for isPrefix {
		more, prefix, err := mr.lr.readFragment()
		line = append(line, more...)
		isPrefix = prefix
		if err != nil {
			break
		}
	}
	mr.sep = string(line)
	mr.sepLine = mr.lr.lineNo
}

// Reader reads an mbox archive.
type Reader struct {
	lr  *lineReader
	mr  *messageReader
	err error

	// Strict enables strict parsing of "From " separator lines. When set,
	// NextMessage and NextMessageWithEnvelope return an *EnvelopeError if a
	// separator carries an unrecognized date or unexpected trailing text.
	// When false (the default) messages are read regardless, and unreadable
	// dates are reported as the zero time in the Envelope. Strict must be
	// set before the first call to NextMessage or
	// NextMessageWithEnvelope. A strict envelope error skips the offending
	// message; calling NextMessage or NextMessageWithEnvelope again resumes
	// with the following message.
	Strict bool
}

// NewReader returns a new Reader to read messages from mbox file format data
// provided by io.Reader r.
func NewReader(r io.Reader) *Reader {
	return &Reader{lr: &lineReader{r: bufio.NewReader(r)}}
}

// NextMessage returns the next message text (containing both the header and
// the body). It will return io.EOF if there are no messages left.
func (r *Reader) NextMessage() (io.Reader, error) {
	rc, _, err := r.next()
	return rc, err
}

// NextMessageWithEnvelope behaves like NextMessage, but additionally returns
// the Envelope parsed from the "From " separator line preceding the message.
// The returned envelope belongs to the returned message and is never replaced
// by information from a later separator. It will return io.EOF if there are
// no messages left.
func (r *Reader) NextMessageWithEnvelope() (io.Reader, *Envelope, error) {
	return r.next()
}

func (r *Reader) next() (io.Reader, *Envelope, error) {
	if r.err != nil {
		return nil, nil, r.err
	}

	var line string
	var lineNo int
	if r.mr == nil {
		var err error
		line, lineNo, err = r.readFirstSeparator()
		if err != nil {
			r.err = err
			return nil, nil, err
		}
	} else {
		if _, err := io.Copy(ioutil.Discard, r.mr); err != nil {
			r.err = err
			return nil, nil, err
		}
		if r.mr.sep == "" {
			r.err = io.EOF
			return nil, nil, io.EOF
		}
		line, lineNo = r.mr.sep, r.mr.sepLine
	}

	env, err := parseEnvelope(line, lineNo, r.Strict)
	r.mr = &messageReader{lr: r.lr}
	if err != nil {
		// The offending message has already been skipped; reading again
		// resumes at the next separator.
		return nil, nil, err
	}
	return r.mr, env, nil
}

// readFirstSeparator scans up to the first "From " separator line, skipping
// leading blank lines, and returns the reassembled raw separator together
// with its 1-based line number. A non-empty line that is not a separator is
// ErrInvalidFormat.
func (r *Reader) readFirstSeparator() (string, int, error) {
	for {
		b, lineNo, err := r.lr.readLine()
		if err != nil {
			return "", 0, err
		}

		if len(b) == 0 {
			continue
		}
		if !bytes.HasPrefix(b, header) {
			return "", 0, ErrInvalidFormat
		}

		return string(b), lineNo, nil
	}
}
