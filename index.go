package mbox

import (
	"bufio"
	"bytes"
	"io"
)

// IndexEntry describes the location of a single message within an mbox
// archive. All offsets are byte offsets into the raw mbox data as stored on
// disk; they are computed from the original bytes and not from any CRLF
// normalized representation.
type IndexEntry struct {
	// Separator is the offset of the first byte of the message's "From "
	// separator line.
	Separator int64
	// Body is the offset of the first byte of the message content, i.e. the
	// first byte after the separator line.
	Body int64
	// End is the offset of the first byte after the message content. It is
	// either the offset of the next message's separator line or the end of
	// the archive.
	End int64
}

// Reader returns an io.Reader reading the content of the indexed message
// from r, which must provide the raw mbox data the index was built from.
//
// The returned reader behaves like the readers returned by Reader.NextMessage:
// it unescapes ">From " lines and normalizes line endings to CRLF.
func (e IndexEntry) Reader(r io.ReaderAt) io.Reader {
	sr := io.NewSectionReader(r, e.Body, e.End-e.Body)
	return &messageReader{r: bufio.NewReader(sr)}
}

// Index maps the messages contained in an mbox archive to their raw byte
// offsets.
type Index []IndexEntry

// BuildIndex scans the mbox data provided by r and returns an index of the
// messages it contains. The data is scanned as a stream; messages are never
// buffered in memory as a whole.
func BuildIndex(r io.Reader) (Index, error) {
	br := bufio.NewReader(r)

	var idx Index
	var offset int64
	atLineStart := true
	pendingBody := false

	for {
		frag, err := br.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, err
		}

		lineStart := offset
		offset += int64(len(frag))

		if atLineStart && len(frag) > 0 {
			if bytes.HasPrefix(frag, header) {
				if len(idx) > 0 {
					idx[len(idx)-1].End = lineStart
				}
				idx = append(idx, IndexEntry{Separator: lineStart})
				pendingBody = true
			} else if len(idx) == 0 && !isBlankLine(frag) {
				return nil, ErrInvalidFormat
			}
		}

		if err == bufio.ErrBufferFull {
			// The line is longer than the read buffer; only its first
			// fragment is relevant for separator detection.
			atLineStart = false
			continue
		}

		if pendingBody {
			idx[len(idx)-1].Body = offset
			pendingBody = false
		}
		atLineStart = true

		if err == io.EOF {
			break
		}
	}

	if len(idx) > 0 {
		idx[len(idx)-1].End = offset
	}

	return idx, nil
}

// isBlankLine reports whether frag is a complete line containing no
// characters other than the line ending.
func isBlankLine(frag []byte) bool {
	return bytes.Equal(frag, []byte("\n")) || bytes.Equal(frag, []byte("\r\n"))
}
