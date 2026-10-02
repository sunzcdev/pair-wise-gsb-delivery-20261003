package mbox

import (
	"bytes"
	"errors"
	"io"
	"time"
)

type messageWriter struct {
	w    io.Writer
	mode Mode

	// MBOXO: an incomplete line (no newline yet) is buffered here.
	buf bytes.Buffer

	// MBOXRD: streaming escaper state. The escaper only ever holds a
	// single leading '>', at most headerLen-1 bytes of a tentative
	// "From " match and an optional dangling CR, so the retained state
	// is bounded even for arbitrarily long lines.
	state   rdWriterState
	hold    bool // a single leading '>' is held back
	cr      bool // a dangling CR is waiting for the next byte
	fromBuf [headerLen]byte
	fromLen int
	staged  []byte
}

type rdWriterState int

const (
	rdLineStart rdWriterState = iota
	rdGT
	rdFrom
	rdBody
)

// mboxrdFlushSize bounds the bytes accumulated in the writer before they are
// forwarded to the underlying writer.
const mboxrdFlushSize = 4096

func (mw *messageWriter) writeLine(l []byte) (int, error) {
	if bytes.HasPrefix(l, header) {
		if _, err := mw.w.Write([]byte{'>'}); err != nil {
			return 0, err
		}
	}

	return mw.w.Write(l)
}

// stage appends a byte to the staging buffer.
func (mw *messageWriter) stage(b byte) {
	mw.staged = append(mw.staged, b)
}

func (mw *messageWriter) stageBytes(b []byte) {
	mw.staged = append(mw.staged, b...)
}

// releaseHold emits a held-back leading '>', if any.
func (mw *messageWriter) releaseHold() {
	if mw.hold {
		mw.stage('>')
		mw.hold = false
	}
}

func (mw *messageWriter) flushStaged() error {
	if len(mw.staged) == 0 {
		return nil
	}
	if _, err := mw.w.Write(mw.staged); err != nil {
		mw.staged = mw.staged[:0]
		return err
	}
	mw.staged = mw.staged[:0]
	return nil
}

// endLine emits any bytes held back from the current line followed by a LF.
func (mw *messageWriter) endLine() {
	mw.releaseHold()
	if mw.fromLen > 0 {
		mw.stageBytes(mw.fromBuf[:mw.fromLen])
		mw.fromLen = 0
	}
	mw.stage('\n')
	mw.state = rdLineStart
}

func (mw *messageWriter) writeMBOXRD(p []byte) (int, error) {
	for i := 0; i < len(p); i++ {
		c := p[i]

		if mw.cr {
			if c == '\n' {
				// CRLF is normalized to LF.
				mw.cr = false
				mw.endLine()
				goto flush
			}

			// A bare CR is ordinary in-line content: it does not end
			// the line, but it breaks any leading '>' run or tentative
			// "From " match.
			mw.cr = false
			mw.releaseHold()
			if mw.fromLen > 0 {
				mw.stageBytes(mw.fromBuf[:mw.fromLen])
				mw.fromLen = 0
			}
			mw.stage('\r')
			mw.state = rdBody
			mw.stage(c)
			goto flush
		}

		switch c {
		case '\r':
			mw.cr = true
			continue
		case '\n':
			mw.endLine()
		default:
			switch mw.state {
			case rdLineStart:
				switch c {
				case '>':
					mw.hold = true
					mw.state = rdGT
				case 'F':
					mw.fromBuf[0] = 'F'
					mw.fromLen = 1
					mw.state = rdFrom
				default:
					mw.stage(c)
					mw.state = rdBody
				}
			case rdGT:
				switch c {
				case '>':
					// Emit the previously held '>' and hold the new
					// one, so the leading run is never buffered as a
					// whole.
					mw.releaseHold()
					mw.hold = true
				case 'F':
					mw.fromBuf[0] = 'F'
					mw.fromLen = 1
					mw.state = rdFrom
				default:
					mw.releaseHold()
					mw.stage(c)
					mw.state = rdBody
				}
			case rdFrom:
				mw.fromBuf[mw.fromLen] = c
				mw.fromLen++
				switch {
				case mw.fromLen == len(header) && bytes.Equal(mw.fromBuf[:], header):
					// A leading '>' run followed by "From ": add one
					// quoting layer. The extra '>' is inserted before
					// the held byte, which is indistinguishable from
					// inserting it at the run start.
					mw.stage('>')
					mw.releaseHold()
					mw.stageBytes(header)
					mw.fromLen = 0
					mw.state = rdBody
				case !bytes.HasPrefix(header, mw.fromBuf[:mw.fromLen]):
					mw.releaseHold()
					mw.stageBytes(mw.fromBuf[:mw.fromLen])
					mw.fromLen = 0
					mw.state = rdBody
				}
			case rdBody:
				mw.stage(c)
			}
		}

	flush:
		if len(mw.staged) >= mboxrdFlushSize {
			if err := mw.flushStaged(); err != nil {
				return i + 1, err
			}
		}
	}

	return len(p), mw.flushStaged()
}

func (mw *messageWriter) Write(p []byte) (int, error) {
	if mw.mode == MBOXRD {
		return mw.writeMBOXRD(p)
	}

	// We will return the number of bytes *from p* that were written. Since
	// we'll scan all the bytes already in the buffer before the write and
	// those in p, we need to remember the initial buffer length.
	initBufLen := mw.buf.Len()

	mw.buf.Write(p)
	b := mw.buf.Bytes()
	mw.buf.Reset()

	var N int = 0
	var err error
	for {
		var n int
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			n, err = mw.buf.Write(b)
			N += n
			break
		}

		var l []byte
		l, b = b[:i+1], b[i+1:]

		// Replace CRLF with LF
		replCRLF := false
		if len(l) > 1 && l[len(l)-2] == '\r' {
			l = l[:len(l)-2]
			l = append(l, '\n')
			replCRLF = true
		}

		n, err = mw.writeLine(l)
		if n == len(l) && replCRLF {
			n++
		}
		N += n
		if err != nil {
			break
		}
	}

	N -= initBufLen
	if N < 0 {
		N = 0
	}
	return N, err
}

func (mw *messageWriter) Close() error {
	if mw.mode == MBOXRD {
		mw.releaseHold()
		if mw.fromLen > 0 {
			mw.stageBytes(mw.fromBuf[:mw.fromLen])
			mw.fromLen = 0
		}
		if mw.cr {
			mw.stage('\r')
			mw.cr = false
		}
		if err := mw.flushStaged(); err != nil {
			return err
		}

		_, err := mw.w.Write([]byte("\n\n"))
		return err
	}

	b := mw.buf.Bytes()
	mw.buf.Reset()
	if _, err := mw.writeLine(b); err != nil {
		return err
	}

	_, err := mw.w.Write([]byte("\n\n"))
	return err
}

// Writer writes messages to a mbox stream. The Close method must be called to
// end the stream.
type Writer struct {
	w      io.Writer
	mode   Mode
	last   *messageWriter
	closed bool
}

// NewWriter creates a new Writer that writes messages to w.
func NewWriter(w io.Writer) *Writer {
	return NewWriterMode(w, MBOXO)
}

// NewWriterMode creates a new Writer that writes messages to w using the
// provided escaping mode.
func NewWriterMode(w io.Writer, mode Mode) *Writer {
	return &Writer{w: w, mode: mode}
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

	w.last = &messageWriter{w: w.w, mode: w.mode}
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
