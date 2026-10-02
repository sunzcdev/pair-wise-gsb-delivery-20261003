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

type unescapePhase int

const (
	ueStart unescapePhase = iota
	ueGT
	ueFrom
	uePass
)

type messageReader struct {
	r                  *bufio.Reader
	next               bytes.Buffer
	atEOF, atSeparator bool
	atMiddleOfLine     bool
	mode               Mode

	// Streaming line unescaper state. At most one leading '>' and at
	// most len(header)-1 bytes of a tentative "From " match are retained,
	// so state stays bounded even for arbitrarily long lines.
	uePhase unescapePhase
	ueHold  bool
	ueBuf   [headerLen]byte
	ueLen   int
}

// resetUnescape prepares the streaming unescaper for a new line.
func (mr *messageReader) resetUnescape() {
	mr.uePhase = ueStart
	mr.ueHold = false
	mr.ueLen = 0
}

// flushUnescapeLine drains bytes held back at the end of a logical line.
func (mr *messageReader) flushUnescapeLine() {
	switch mr.uePhase {
	case ueGT:
		if mr.ueHold {
			mr.next.WriteByte('>')
			mr.ueHold = false
		}
	case ueFrom:
		if mr.ueHold {
			mr.next.WriteByte('>')
			mr.ueHold = false
		}
		mr.next.Write(mr.ueBuf[:mr.ueLen])
		mr.ueLen = 0
	}
}

// unescapeSegment feeds one ReadLine fragment through the unescaper.
func (mr *messageReader) unescapeSegment(b []byte) {
	for _, c := range b {
		switch mr.uePhase {
		case ueStart:
			switch c {
			case '>':
				mr.ueHold = true
				mr.uePhase = ueGT
			case 'F':
				mr.next.WriteByte(c)
				mr.uePhase = uePass
			default:
				mr.next.WriteByte(c)
				mr.uePhase = uePass
			}
		case ueGT:
			switch c {
			case '>':
				if mr.mode == MBOXO {
					// mboxo only removes the quoting '>' when the line
					// starts with exactly one '>' followed by "From ".
					mr.next.WriteByte('>')
					mr.ueHold = false
					mr.next.WriteByte('>')
					mr.uePhase = uePass
				} else if mr.ueHold {
					mr.next.WriteByte('>')
				} else {
					mr.ueHold = true
				}
			case 'F':
				mr.ueBuf[0] = 'F'
				mr.ueLen = 1
				mr.uePhase = ueFrom
			default:
				mr.next.WriteByte('>')
				mr.ueHold = false
				mr.next.WriteByte(c)
				mr.uePhase = uePass
			}
		case ueFrom:
			mr.ueBuf[mr.ueLen] = c
			mr.ueLen++
			switch {
			case mr.ueLen == len(header) && bytes.Equal(mr.ueBuf[:], header):
				// Drop exactly one quoting layer: the held '>' is
				// discarded, the rest of the line is passed through.
				mr.ueHold = false
				mr.next.Write(header)
				mr.ueLen = 0
				mr.uePhase = uePass
			case !bytes.HasPrefix(header, mr.ueBuf[:mr.ueLen]):
				mr.next.WriteByte('>')
				mr.ueHold = false
				mr.next.Write(mr.ueBuf[:mr.ueLen])
				mr.ueLen = 0
				mr.uePhase = uePass
			}
		case uePass:
			mr.next.WriteByte(c)
		}
	}
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
			mr.resetUnescape()

			if bytes.HasPrefix(b, header) {
				mr.atSeparator = true
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
					return 0, io.EOF
				}

				mr.next.Write([]byte("\r\n"))
				mr.resetUnescape()
			}
		}

		mr.unescapeSegment(b)
		if !isPrefix {
			mr.flushUnescapeLine()
			mr.next.Write([]byte("\r\n"))
		}
		mr.atMiddleOfLine = isPrefix
	}

	return mr.next.Read(p)
}

// Reader reads an mbox archive.
type Reader struct {
	r    *bufio.Reader
	mode Mode
	mr   *messageReader
}

// NewReader returns a new Reader to read messages from mbox file format data
// provided by io.Reader r.
func NewReader(r io.Reader) *Reader {
	return NewReaderMode(r, MBOXO)
}

// NewReaderMode returns a new Reader that reads messages from mbox file
// format data provided by io.Reader r, using the provided unescaping mode.
func NewReaderMode(r io.Reader, mode Mode) *Reader {
	return &Reader{r: bufio.NewReader(r), mode: mode}
}

// NextMessage returns the next message text (containing both the header and the
// body). It will return io.EOF if there are no messages left.
func (r *Reader) NextMessage() (io.Reader, error) {
	if r.mr == nil {
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
	} else {
		if _, err := io.Copy(ioutil.Discard, r.mr); err != nil {
			return nil, err
		}
		if r.mr.atEOF {
			return nil, io.EOF
		}
	}
	r.mr = &messageReader{r: r.r, mode: r.mode}
	return r.mr, nil
}
