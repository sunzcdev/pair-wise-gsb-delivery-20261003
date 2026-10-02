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
	"time"
)

// ErrInvalidFormat is the error returned by the NextMessage method of Reader if
// its content is malformed in a way that it is not possible to extract a
// message.
var ErrInvalidFormat = errors.New("invalid mbox format")

// ansicDateLen is the fixed length of a date in time.ANSIC format, as used
// in mbox separator lines (e.g. "Thu Jan  1 00:00:01 2015").
const ansicDateLen = 24

// Envelope contains the metadata of a message, parsed from the "From "
// separator line that introduces the message in an mbox file.
type Envelope struct {
	// Line is the raw separator line, without the leading "From " prefix
	// stripped and without the trailing newline. It is always populated,
	// even if the line could not be fully parsed.
	Line string
	// From is the sender of the message, i.e. the first whitespace
	// delimited token after the "From " prefix. It may be empty.
	From string
	// Date is the timestamp of the message. It is the zero time if the
	// separator line contains no date or the date could not be parsed.
	Date time.Time
}

// ParseError describes a malformed "From " separator line. It is only
// returned if the Reader was created with ReaderOptions.Strict set to true.
type ParseError struct {
	// Message is the 1-based index of the message whose separator line is
	// malformed.
	Message int
	// Line is the raw separator line, without the trailing newline.
	Line string
	// Err describes what is wrong with the separator line.
	Err error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("mbox: message %d: invalid separator line %q: %v", e.Message, e.Line, e.Err)
}

// Unwrap returns the underlying parse error.
func (e *ParseError) Unwrap() error {
	return e.Err
}

// parseEnvelope parses an mbox separator line (without the trailing
// newline) into an Envelope. The line must start with the "From " prefix.
//
// The sender is the first whitespace delimited token after the prefix and
// may be empty. The remainder, after stripping leading spaces, is expected
// to start with a date in time.ANSIC format. Any text after the date is
// tolerated and ignored; it is preserved in Envelope.Line. A missing or
// unparseable date is reported as an error; the returned Envelope is still
// populated as far as possible.
func parseEnvelope(line []byte) (*Envelope, error) {
	env := &Envelope{Line: string(line)}

	rest := line[len(header):]
	if i := bytes.IndexByte(rest, ' '); i >= 0 {
		env.From = string(rest[:i])
		rest = rest[i+1:]
	} else {
		env.From = string(rest)
		rest = nil
	}

	rest = bytes.TrimLeft(rest, " ")
	if len(rest) == 0 {
		return env, errors.New("missing date")
	}

	date := rest
	if len(date) > ansicDateLen {
		date = date[:ansicDateLen]
	}
	t, err := time.Parse(time.ANSIC, string(date))
	if err != nil {
		return env, fmt.Errorf("cannot parse date %q", date)
	}
	env.Date = t
	return env, nil
}

// readLogicalLine returns the complete logical line whose first fragment
// (as returned by bufio.Reader.ReadLine) is first. If isPrefix is false the
// line already fits into the read buffer and first is returned as is.
// Otherwise the remaining fragments are read and appended, so that
// separator lines longer than the read buffer are captured in full.
func readLogicalLine(r *bufio.Reader, first []byte, isPrefix bool) []byte {
	if !isPrefix {
		return first
	}
	line := append([]byte(nil), first...)
	for isPrefix {
		frag, prefix, err := r.ReadLine()
		if err != nil {
			break
		}
		line = append(line, frag...)
		isPrefix = prefix
	}
	return line
}

type messageReader struct {
	r                  *bufio.Reader
	next               bytes.Buffer
	atEOF, atSeparator bool
	atMiddleOfLine     bool
	separator          []byte
}

func (mr *messageReader) Read(p []byte) (int, error) {
	if mr.atEOF || mr.atSeparator {
		return 0, io.EOF
	}

	if mr.next.Len() == 0 {
		b, isPrefix, err := mr.r.ReadLine()
		if err != nil {
			mr.atEOF = true
			return 0, err
		}

		if !mr.atMiddleOfLine {
			if bytes.HasPrefix(b, header) {
				mr.atSeparator = true
				mr.separator = readLogicalLine(mr.r, b, isPrefix)
				return 0, io.EOF
			} else if len(b) == 0 {
				// Check if the next line is separator. In such case the new
				// line should not be written to not have double new line.
				b, isPrefix, err = mr.r.ReadLine()
				if err != nil {
					mr.atEOF = true
					return 0, err
				}

				if bytes.HasPrefix(b, header) {
					mr.atSeparator = true
					mr.separator = readLogicalLine(mr.r, b, isPrefix)
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

// Reader reads an mbox archive.
type Reader struct {
	r      *bufio.Reader
	mr     *messageReader
	strict bool
	msg    int
	err    error
}

// NewReader returns a new Reader to read messages from mbox file format data
// provided by io.Reader r. It is equivalent to NewReaderWithOptions with
// the zero ReaderOptions.
func NewReader(r io.Reader) *Reader {
	return NewReaderWithOptions(r, ReaderOptions{})
}

// ReaderOptions configures a Reader.
type ReaderOptions struct {
	// Strict controls how malformed "From " separator lines are handled.
	// If Strict is false (the default), a separator line with a missing or
	// unparseable date is tolerated: the message is returned and the Date
	// field of its Envelope is left as the zero time. If Strict is true,
	// such a separator line makes NextMessage and NextMessageWithEnvelope
	// return a *ParseError describing the position of the offending
	// message; once returned, the error is sticky and is returned by all
	// subsequent calls.
	Strict bool
}

// NewReaderWithOptions returns a new Reader to read messages from mbox file
// format data provided by io.Reader r, configured by opts.
func NewReaderWithOptions(r io.Reader, opts ReaderOptions) *Reader {
	return &Reader{r: bufio.NewReader(r), strict: opts.Strict}
}

// NextMessage returns the next message text (containing both the header and
// the body). It will return io.EOF if there are no messages left.
//
// NextMessage and NextMessageWithEnvelope share the same read position and
// may be mixed freely: each call, regardless of the entry point used,
// advances to the next message in the stream.
func (r *Reader) NextMessage() (io.Reader, error) {
	_, mr, err := r.next()
	return mr, err
}

// NextMessageWithEnvelope is like NextMessage but additionally returns the
// Envelope of the message, parsed from its "From " separator line. The
// returned Envelope always describes the message returned by the same call,
// even if a previous message was not read to completion.
func (r *Reader) NextMessageWithEnvelope() (*Envelope, io.Reader, error) {
	return r.next()
}

func (r *Reader) next() (*Envelope, io.Reader, error) {
	if r.err != nil {
		return nil, nil, r.err
	}

	var line []byte
	if r.mr != nil {
		// Discard the rest of the previous message. The separator line
		// that terminated it introduces the message we are about to
		// return, so its metadata belongs to this call.
		mr := r.mr
		if _, err := io.Copy(ioutil.Discard, mr); err != nil {
			return nil, nil, err
		}
		if mr.atEOF {
			return nil, nil, io.EOF
		}
		line = mr.separator
		r.mr = nil
	} else {
		for {
			b, isPrefix, err := r.r.ReadLine()
			if err != nil {
				return nil, nil, err
			}

			isFromLine := bytes.HasPrefix(b, header)
			if isFromLine {
				// Capture the rest of the line, which may be
				// longer than the read buffer.
				b = readLogicalLine(r.r, b, isPrefix)
			} else {
				// Discard the rest of the line.
				for isPrefix {
					_, isPrefix, err = r.r.ReadLine()
					if err != nil {
						return nil, nil, err
					}
				}
			}
			if len(b) == 0 {
				continue
			}
			if !isFromLine {
				return nil, nil, ErrInvalidFormat
			}
			line = b
			break
		}
	}

	r.msg++
	env, err := parseEnvelope(line)
	if err != nil && r.strict {
		r.err = &ParseError{Message: r.msg, Line: env.Line, Err: err}
		return nil, nil, r.err
	}

	r.mr = &messageReader{r: r.r}
	return env, r.mr, nil
}
