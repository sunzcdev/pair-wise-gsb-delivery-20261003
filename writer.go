package mbox

import (
	"bytes"
	"errors"
	"io"
	"time"
)

// mwState tracks how many bytes of a line start have been tentatively held
// back while checking whether the line begins with "From " (mboxo) or with a
// run of ">" followed by "From " (mboxrd). The state never buffers body bytes
// and never grows with the line length, so arbitrarily long lines (including
// arbitrarily long ">" runs) are streamed without unbounded buffering.
type mwState int

const (
	stateLineStart mwState = iota // at the start of a line, nothing held
	stateGt                       // mboxrd: holding a run of ">"
	stateFrom                     // holding a partial "From " prefix
	stateBody                     // regular line content
)

type messageWriter struct {
	w      io.Writer
	format Format

	state     mwState
	fromMatch int   // number of "From " bytes matched so far
	crPending bool  // a trailing CR is held, waiting to see if an LF follows
	err       error // first write error
}

// writeRaw writes bytes straight to the underlying writer without quoting.
func (mw *messageWriter) writeRaw(b []byte) error {
	if mw.err != nil {
		return mw.err
	}
	_, err := mw.w.Write(b)
	if err != nil {
		mw.err = err
	}
	return err
}

// flushHeld releases all bytes tentatively held at a line start, then emits c
// as ordinary content.
func (mw *messageWriter) flushHeld(c byte) error {
	if mw.fromMatch > 0 {
		if err := mw.writeRaw(header[:mw.fromMatch]); err != nil {
			return err
		}
		mw.fromMatch = 0
	}
	if c == '\n' {
		mw.state = stateLineStart
		return mw.writeRaw([]byte{'\n'})
	}
	mw.state = stateBody
	if mw.crPending {
		mw.crPending = false
		if err := mw.writeRaw([]byte{'\r'}); err != nil {
			return err
		}
	}
	return mw.writeRaw([]byte{c})
}

// emitQuotedFrom emits the single extra quoting ">" for a fully recognized
// "From " line and switches to body state. The matched "From " bytes have been
// held and must be emitted separately. The leading ">" run has already been
// streamed, so placing the extra ">" immediately before "From " is
// byte-equivalent to placing it at the front of the run.
func (mw *messageWriter) emitQuotedFrom() error {
	if err := mw.writeRaw([]byte{'>'}); err != nil {
		return err
	}
	if err := mw.writeRaw(header); err != nil {
		return err
	}
	mw.fromMatch = 0
	mw.state = stateBody
	return nil
}

// emitBodySegment streams bytes known to be inside a line while normalizing
// CRLF to LF. It consumes up to (and including) the first newline and leaves
// subsequent bytes to the line-start state machine. It stops early on the
// first error and returns the number of bytes consumed from p.
func (mw *messageWriter) emitBodySegment(p []byte) int {
	if len(p) == 0 {
		return 0
	}

	if mw.crPending {
		mw.crPending = false
		if p[0] == '\n' {
			if err := mw.writeRaw([]byte{'\n'}); err != nil {
				return 0
			}
			mw.state = stateLineStart
			return 1
		}
		if err := mw.writeRaw([]byte{'\r'}); err != nil {
			return 0
		}
		// The held CR is now flushed; continue processing p[0] below.
	}

	j := bytes.IndexByte(p, '\n')
	if j < 0 {
		end := len(p)
		if p[end-1] == '\r' {
			mw.crPending = true
			end--
		}
		if end > 0 {
			if err := mw.writeRaw(p[:end]); err != nil {
				return 0
			}
		}
		return len(p)
	}

	end := j
	if end > 0 && p[end-1] == '\r' {
		if err := mw.writeRaw(p[:end-1]); err != nil {
			return 0
		}
	} else if end > 0 {
		if err := mw.writeRaw(p[:end]); err != nil {
			return 0
		}
	}
	if err := mw.writeRaw([]byte{'\n'}); err != nil {
		return j
	}
	mw.state = stateLineStart
	return j + 1
}

func (mw *messageWriter) Write(p []byte) (int, error) {
	i := 0
	for i < len(p) {
		c := p[i]

		switch mw.state {
		case stateLineStart:
			if mw.crPending {
				mw.crPending = false
				if c == '\n' {
					if err := mw.writeRaw([]byte{'\n'}); err != nil {
						return i, err
					}
					i++
					continue
				}
				if err := mw.writeRaw([]byte{'\r'}); err != nil {
					return i, err
				}
				// Reprocess c below as a normal line-start byte.
			}
			if c == '\r' {
				mw.crPending = true
			} else if c == '\n' {
				if err := mw.writeRaw([]byte{'\n'}); err != nil {
					return i, err
				}
			} else if c == '>' && mw.format == Mboxrd {
				if err := mw.writeRaw([]byte{'>'}); err != nil {
					return i, err
				}
				mw.state = stateGt
			} else if c == 'F' {
				mw.state = stateFrom
				mw.fromMatch = 1
			} else {
				mw.state = stateBody
			}

		case stateGt:
			if mw.crPending {
				mw.crPending = false
				if c == '\n' {
					if err := mw.writeRaw([]byte{'\n'}); err != nil {
						return i, err
					}
					mw.state = stateLineStart
					i++
					continue
				}
				if err := mw.writeRaw([]byte{'\r'}); err != nil {
					return i, err
				}
				// c is a normal line byte after a lone CR; fall through.
			}
			if c == '\r' {
				mw.crPending = true
			} else if c == '\n' {
				if err := mw.flushHeld('\n'); err != nil {
					return i, err
				}
				mw.state = stateLineStart
			} else if c == '>' {
				if err := mw.writeRaw([]byte{'>'}); err != nil {
					return i, err
				}
			} else if c == 'F' {
				mw.state = stateFrom
				mw.fromMatch = 1
			} else {
				if err := mw.flushHeld(c); err != nil {
					return i, err
				}
				i++
				n := mw.emitBodySegment(p[i:])
				i += n
				if mw.err != nil {
					return i, mw.err
				}
				continue
			}

		case stateFrom:
			if mw.crPending {
				mw.crPending = false
				if c == '\n' {
					if err := mw.writeRaw(header[:mw.fromMatch]); err != nil {
						return i, err
					}
					mw.fromMatch = 0
					if err := mw.writeRaw([]byte{'\n'}); err != nil {
						return i, err
					}
					mw.state = stateLineStart
					i++
					continue
				}
				if err := mw.writeRaw(header[:mw.fromMatch]); err != nil {
					return i, err
				}
				mw.fromMatch = 0
				if err := mw.flushHeld('\r'); err != nil {
					return i, err
				}
				// Reprocess c as a body byte.
				i++
				n := mw.emitBodySegment(p[i:])
				i += n
				if mw.err != nil {
					return i, mw.err
				}
				continue
			}
			if c == '\r' {
				mw.crPending = true
				i++
				continue
			} else if c == '\n' {
				if err := mw.flushHeld('\n'); err != nil {
					return i, err
				}
				mw.state = stateLineStart
				i++
				continue
			}
			if mw.fromMatch < len(header) && c == header[mw.fromMatch] {
				mw.fromMatch++
				if mw.fromMatch == len(header) {
					if err := mw.emitQuotedFrom(); err != nil {
						return i, err
					}
					// "From " was emitted as a whole; bulk-process the line
					// body beginning after the trailing space.
					i++
					n := mw.emitBodySegment(p[i:])
					i += n
					if mw.err != nil {
						return i, mw.err
					}
					continue
				}
			} else {
				if err := mw.flushHeld(c); err != nil {
					return i, err
				}
				i++
				n := mw.emitBodySegment(p[i:])
				i += n
				if mw.err != nil {
					return i, mw.err
				}
				continue
			}
		}

		if mw.state == stateBody {
			// Bulk path: emit the byte that caused us to enter body state
			// (p[i]) and stream the remainder of the line, stopping at the
			// first newline (inclusive).
			n := mw.emitBodySegment(p[i:])
			i += n
			if mw.err != nil {
				return i, mw.err
			}
			continue
		}
		i++
	}
	return len(p), mw.err
}

func (mw *messageWriter) Close() error {
	// Flush anything still tentatively held at a line start as plain content;
	// an unterminated line never matched "From ".
	switch mw.state {
	case stateGt:
		// The ">" run was already streamed; nothing is held.
	case stateFrom:
		if mw.fromMatch > 0 {
			if err := mw.writeRaw(header[:mw.fromMatch]); err != nil {
				return err
			}
		}
	}
	if mw.crPending {
		if err := mw.writeRaw([]byte{'\r'}); err != nil {
			return err
		}
	}
	if _, err := mw.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return mw.err
}

// Writer writes messages to a mbox stream. The Close method must be called to
// end the stream.
type Writer struct {
	w      io.Writer
	format Format
	last   *messageWriter
	closed bool
}

// NewWriterFormat creates a new Writer that writes messages to w using the
// given format.
func NewWriterFormat(w io.Writer, format Format) *Writer {
	return &Writer{w: w, format: format}
}

// NewWriter creates a new Writer that writes messages to w.
func NewWriter(w io.Writer) *Writer {
	return NewWriterFormat(w, Mboxo)
}

// CreateMessage appends a message to the mbox stream. The message text
// (including both the header and the body) should be written to the returned
// io.Writer.
func (w *Writer) CreateMessage(from string, t time.Time) (io.Writer, error) {
	if w.closed {
		return nil, errors.New("mbox: Writer.CreateMessage called after Close")
	}
	if w.last != nil {
		if err := w.last.Close(); err != nil {
			return nil, err
		}
		w.last = nil
	}

	if from == "" {
		from = "???@???"
	}

	if t.IsZero() {
		t = time.Now()
	}
	date := t.UTC().Format(time.ANSIC)

	line := "From " + from + " " + date + "\n"
	if _, err := io.WriteString(w.w, line); err != nil {
		return nil, err
	}

	w.last = &messageWriter{w: w.w, format: w.format}
	return w.last, nil
}

func (w *Writer) Close() error {
	if w.closed {
		return errors.New("mbox: Writer already closed")
	}
	w.closed = true
	if w.last != nil {
		return w.last.Close()
	}
	return nil
}
