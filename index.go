package mbox

import (
	"bufio"
	"bytes"
	"io"
)

// IndexEntry describes the location of a single message in an mbox archive.
// All offsets are positions in the raw, on-disk byte stream of the archive:
// line endings are counted exactly as stored (so a CRLF ending contributes two
// bytes), no normalization is applied.
type IndexEntry struct {
	// Separator is the byte offset of the start of the "From " separator
	// line of the message.
	Separator int64
	// Body is the byte offset of the first byte of the message content
	// (immediately after the newline terminating the separator line).
	Body int64
	// End is the raw byte offset right after the last byte belonging to the
	// message. It is the start of the next message's separator line (blank
	// lines separating two messages belong to the preceding message's raw
	// range), or the end of the archive for the last message.
	End int64
}

// Reader returns an io.Reader for the message described by the IndexEntry. The
// mbox archive must be provided as an io.ReaderAt positioned at the same raw
// byte stream the index was built from. The whole message is not loaded into
// memory: only the requested range of the archive is accessed.
//
// The returned reader decodes the message exactly like Reader.NextMessage, so
// a message read from an index produces the same bytes as reading the archive
// sequentially. Readers may be created and read in any order; one that is
// only partially consumed may simply be discarded to move on to another
// message.
func (e *IndexEntry) Reader(r io.ReaderAt) io.Reader {
	return &messageReader{r: bufio.NewReader(io.NewSectionReader(r, e.Body, e.End-e.Body))}
}

// Index is an index of the messages contained in an mbox archive.
type Index struct {
	// Entries lists the messages in archive order.
	Entries []IndexEntry
}

// Len returns the number of indexed messages.
func (idx *Index) Len() int {
	return len(idx.Entries)
}

// BuildIndex builds an Index from the mbox archive read from r. The index is
// built in a streaming fashion: the archive is scanned line by line and only a
// single buffered line chunk is kept in memory at a time, so an arbitrarily
// large archive with arbitrarily long lines can be indexed without retaining
// any whole message.
//
// Offsets are counted in raw bytes of r, including line endings exactly as
// stored; they are not derived from CRLF-normalized message lengths.
func BuildIndex(r io.Reader) (*Index, error) {
	b := &indexBuilder{br: bufio.NewReader(r)}
	idx := &Index{}

	openIdx := -1
	for {
		start, empty, separator, terminated, hasData, err := b.readLine()
		if err != nil && err != io.EOF {
			return nil, err
		}

		switch {
		case separator:
			// A separator line at end of file is not a valid message.
			if !terminated {
				return nil, ErrInvalidFormat
			}
			if openIdx >= 0 {
				// start is either the beginning of the next separator line or
				// (with blank separating lines) the first blank line.
				idx.Entries[openIdx].End = start
			}
			idx.Entries = append(idx.Entries, IndexEntry{
				Separator: start,
				Body:      b.offset,
			})
			openIdx = len(idx.Entries) - 1
		case openIdx < 0 && hasData && !empty:
			// Before the first separator only blank lines are tolerated, just
			// like Reader.NextMessage.
			return nil, ErrInvalidFormat
		}

		if err == io.EOF {
			if openIdx >= 0 {
				idx.Entries[openIdx].End = b.offset
			}
			break
		}
	}

	return idx, nil
}

type indexBuilder struct {
	br     *bufio.Reader
	offset int64 // raw byte offset of the start of the next line
}

// readLine consumes a single physical line. It returns the line's start
// offset, whether the line is empty (consists only of a line ending), whether
// it is a "From " separator line, and whether it was terminated by a newline.
// hasData reports whether any byte was consumed (it is false on clean EOF).
//
// Lines longer than the buffer are drained chunk by chunk, so the working set
// stays bounded regardless of line length.
func (b *indexBuilder) readLine() (start int64, empty, separator, terminated, hasData bool, err error) {
	start = b.offset

	frag, err := b.br.ReadSlice('\n')
	b.offset += int64(len(frag))
	if len(frag) > 0 {
		hasData = true
		last := frag[len(frag)-1]
		switch {
		case last == '\n' && (len(frag) == 1 || len(frag) == 2 && frag[0] == '\r'):
			empty = true
		case last == '\r' && len(frag) == 1:
			empty = true
		}
		separator = bytes.HasPrefix(frag, header)
		if last == '\n' {
			terminated = true
		}
	}
	if err != bufio.ErrBufferFull {
		return start, empty, separator, terminated, hasData, err
	}

	// Long line: drain the remaining chunks of the same line.
	for {
		var rest []byte
		rest, err = b.br.ReadSlice('\n')
		b.offset += int64(len(rest))
		if len(rest) > 0 {
			hasData = true
		}
		if err != bufio.ErrBufferFull {
			if err == nil {
				terminated = true
			}
			return start, empty, separator, terminated, hasData, err
		}
	}
}
