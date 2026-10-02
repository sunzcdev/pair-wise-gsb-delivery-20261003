package mbox

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
)

// DefaultMaxScanBytes is the default maximum number of bytes a
// RecoveringReader scans while searching for the next message separator
// after malformed data.
const DefaultMaxScanBytes = 1 << 20

// maxProbeLines bounds the number of lines inspected after a separator
// candidate to confirm that it is followed by message headers.
const maxProbeLines = 10

// ErrRecoveryLimit is returned by the NextMessage method of RecoveringReader
// if more than maxScanBytes bytes are scanned while searching for the next
// message separator, without finding one. There is not enough evidence to
// continue the recovery, so the scan stops instead of searching unboundedly.
var ErrRecoveryLimit = errors.New("mbox: recovery scan limit exceeded")

// Region describes a contiguous range of bytes in the underlying stream.
type Region struct {
	Offset int64 // offset of the first byte of the region
	Length int64 // number of bytes in the region
}

// RecoveryInfo describes how a message returned by a RecoveringReader
// relates to the underlying stream.
type RecoveryInfo struct {
	// Offset is the byte offset of the message separator line in the
	// underlying stream.
	Offset int64
	// Recovered is true if malformed data had to be skipped before this
	// message could be extracted. In this case the data of the previous
	// message may be incomplete.
	Recovered bool
	// Skipped contains the raw regions of the stream that were skipped
	// before this message.
	Skipped []Region
}

// rawLine is a single line read from the underlying stream.
type rawLine struct {
	data   []byte // line content, without the trailing newline
	offset int64  // stream offset of the first byte of the line
	length int64  // number of bytes the line occupies in the stream
}

// lineReader reads lines from the underlying stream, tracking stream
// offsets and supporting lookahead and pushback.
type lineReader struct {
	r      *bufio.Reader
	queue  []rawLine
	offset int64
	eof    bool
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: bufio.NewReader(r)}
}

func (lr *lineReader) readRaw() (rawLine, error) {
	if lr.eof {
		return rawLine{}, io.EOF
	}
	b, err := lr.r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return rawLine{}, err
	}
	if len(b) == 0 {
		lr.eof = true
		return rawLine{}, io.EOF
	}
	if err == io.EOF {
		// Last line of the stream, not terminated by a newline.
		lr.eof = true
	}
	l := rawLine{offset: lr.offset, length: int64(len(b))}
	lr.offset += l.length
	d := b
	if d[len(d)-1] == '\n' {
		d = d[:len(d)-1]
		if len(d) > 0 && d[len(d)-1] == '\r' {
			d = d[:len(d)-1]
		}
	}
	l.data = d
	return l, nil
}

// next returns the next line, consuming it.
func (lr *lineReader) next() (rawLine, error) {
	if len(lr.queue) > 0 {
		l := lr.queue[0]
		lr.queue = lr.queue[1:]
		return l, nil
	}
	return lr.readRaw()
}

// peek returns the line n positions ahead, without consuming it.
func (lr *lineReader) peek(n int) (rawLine, error) {
	for len(lr.queue) <= n {
		l, err := lr.readRaw()
		if err != nil {
			return rawLine{}, err
		}
		lr.queue = append(lr.queue, l)
	}
	return lr.queue[n], nil
}

// unread pushes a consumed line back, so it is returned by the next call to
// next or peek.
func (lr *lineReader) unread(l rawLine) {
	lr.queue = append([]rawLine{l}, lr.queue...)
}

// validEnvelope reports whether line has the structure of an mbox envelope
// separator: "From <sender> <date>". The check is heuristic: the sender
// must be a single token and the date portion must contain at least one
// digit.
func validEnvelope(line []byte) bool {
	if !bytes.HasPrefix(line, header) {
		return false
	}
	fields := bytes.Fields(line[len(header):])
	if len(fields) < 2 {
		return false
	}
	for _, f := range fields[1:] {
		if bytes.IndexAny(f, "0123456789") >= 0 {
			return true
		}
	}
	return false
}

// isHeaderLine reports whether line looks like an RFC 5322 header line.
func isHeaderLine(line []byte) bool {
	i := bytes.IndexByte(line, ':')
	if i <= 0 {
		return false
	}
	for _, c := range line[:i] {
		if c <= 32 || c > 126 {
			return false
		}
	}
	return true
}

// recoveringMessageReader reads a single message from the stream. It stops
// at any line starting with "From " (a separator candidate) or at EOF.
type recoveringMessageReader struct {
	lr                 *lineReader
	next               bytes.Buffer
	atEOF, atSeparator bool
}

func (mr *recoveringMessageReader) Read(p []byte) (int, error) {
	if mr.atEOF || mr.atSeparator {
		return 0, io.EOF
	}

	if mr.next.Len() == 0 {
		for {
			l, err := mr.lr.next()
			if err != nil {
				mr.atEOF = true
				return 0, err
			}

			if bytes.HasPrefix(l.data, header) {
				// Separator candidate: push it back so the
				// RecoveringReader can validate it.
				mr.lr.unread(l)
				mr.atSeparator = true
				return 0, io.EOF
			}

			if len(l.data) == 0 {
				// Check if the next line is a separator candidate.
				// In such case the empty line is not written, to not
				// have a double new line.
				l, err = mr.lr.next()
				if err != nil {
					mr.atEOF = true
					return 0, err
				}
				if bytes.HasPrefix(l.data, header) {
					mr.lr.unread(l)
					mr.atSeparator = true
					return 0, io.EOF
				}
				mr.next.Write([]byte("\r\n"))
			}

			b := l.data
			if bytes.HasPrefix(b, escapedHeader) {
				b = b[1:]
			}
			mr.next.Write(b)
			mr.next.Write([]byte("\r\n"))
			break
		}
	}

	return mr.next.Read(p)
}

// RecoveringReader reads an mbox archive, recovering from malformed data.
//
// Unlike Reader, which stops at the first format error, RecoveringReader
// skips malformed data and searches for the next line that qualifies as a
// message separator: it must start with "From ", have a plausible envelope
// structure and be followed by message headers (a bounded probe). The
// search itself is bounded by maxScanBytes; if no separator is found within
// that limit, recovery stops with ErrRecoveryLimit instead of scanning
// unboundedly.
type RecoveringReader struct {
	lr           *lineReader
	mr           *recoveringMessageReader
	maxScanBytes int64
}

// NewRecoveringReader returns a new RecoveringReader to read messages from
// mbox file format data provided by io.Reader r, recovering from malformed
// data. maxScanBytes bounds the number of bytes scanned while searching for
// the next message separator; if it is not positive, DefaultMaxScanBytes is
// used.
func NewRecoveringReader(r io.Reader, maxScanBytes int64) *RecoveringReader {
	if maxScanBytes <= 0 {
		maxScanBytes = DefaultMaxScanBytes
	}
	return &RecoveringReader{lr: newLineReader(r), maxScanBytes: maxScanBytes}
}

// validSeparator reports whether l is a valid message separator: it must
// have a plausible envelope structure and be followed by message headers.
func (rr *RecoveringReader) validSeparator(l rawLine) bool {
	return validEnvelope(l.data) && rr.probeHeaders()
}

// probeHeaders performs a bounded probe of the lines following the current
// position, checking whether they look like message headers. It returns
// true only if at least one header line is found within maxProbeLines
// lines.
func (rr *RecoveringReader) probeHeaders() bool {
	headers := 0
	for i := 0; i < maxProbeLines; i++ {
		l, err := rr.lr.peek(i)
		if err != nil {
			// EOF: the evidence gathered so far is all we get.
			return headers > 0
		}
		if len(l.data) == 0 {
			// An empty line ends the header block.
			return headers > 0
		}
		if l.data[0] == ' ' || l.data[0] == '\t' {
			// Header continuation line.
			continue
		}
		if !isHeaderLine(l.data) {
			return false
		}
		headers++
	}
	return headers > 0
}

// NextMessage returns the next message text (containing both the header and
// the body) along with information about how the message was recovered from
// the stream. It will return io.EOF if there are no messages left.
func (rr *RecoveringReader) NextMessage() (io.Reader, *RecoveryInfo, error) {
	if rr.mr != nil {
		if _, err := io.Copy(ioutil.Discard, rr.mr); err != nil {
			return nil, nil, err
		}
		if rr.mr.atEOF {
			return nil, nil, io.EOF
		}
		rr.mr = nil
	}

	info := &RecoveryInfo{}
	var scanned int64
	regionStart, regionEnd := int64(-1), int64(0)
	scanning := false

	closeRegion := func() {
		if regionStart >= 0 {
			info.Skipped = append(info.Skipped, Region{
				Offset: regionStart,
				Length: regionEnd - regionStart,
			})
			regionStart = -1
		}
	}

	for {
		l, err := rr.lr.next()
		if err != nil {
			return nil, nil, err
		}

		if !scanning {
			// Leading empty lines are valid mbox.
			if len(l.data) == 0 {
				continue
			}
			if rr.validSeparator(l) {
				info.Offset = l.offset
				break
			}
			// Format error: start the recovery scan.
			scanning = true
			info.Recovered = true
		}

		scanned += l.length
		if scanned > rr.maxScanBytes {
			return nil, nil, ErrRecoveryLimit
		}

		if len(l.data) == 0 {
			// Empty lines separate clusters of malformed data.
			closeRegion()
			continue
		}
		if rr.validSeparator(l) {
			closeRegion()
			info.Offset = l.offset
			break
		}
		if regionStart < 0 {
			regionStart = l.offset
		}
		regionEnd = l.offset + l.length
	}

	rr.mr = &recoveringMessageReader{lr: rr.lr}
	return rr.mr, info, nil
}
