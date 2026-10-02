package mbox

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
)

// ErrRecoveryLimit is returned by RecoveringReader.NextMessage when more than
// MaxScanBytes bytes were scanned while looking for the next valid message
// separator. The scan can be resumed by calling NextMessage again.
var ErrRecoveryLimit = errors.New("mbox: recovery scan limit exceeded")

// DefaultMaxScanBytes is the default value of RecoveringReader.MaxScanBytes.
const DefaultMaxScanBytes = 1 << 20

// maxHeaderProbeLines bounds how many lines following a candidate separator
// line are probed for header fields during recovery.
const maxHeaderProbeLines = 8

// SkippedRegion describes a raw region of the mbox stream that was skipped by
// a RecoveringReader because it could not be parsed as (part of) a message.
type SkippedRegion struct {
	// Offset is the byte offset of the first skipped byte in the stream.
	Offset int64
	// Length is the number of skipped bytes.
	Length int64
	// InvalidSeparator is true if the region consists of lines that look
	// like mbox "From " separator lines but failed the recovery validation
	// rules.
	InvalidSeparator bool
}

// Message is a single message read by a RecoveringReader.
type Message struct {
	io.Reader

	// Offset is the byte offset of the message separator line in the
	// stream.
	Offset int64
	// Recovered is true if the RecoveringReader skipped malformed data
	// between the previous message and this one. If set, the archive is
	// known to be incomplete and Skipped describes the lost raw regions.
	Recovered bool
	// Skipped lists the raw regions that were skipped while scanning for
	// this message.
	Skipped []SkippedRegion
}

// peekLine is a raw line read from the stream, including its line ending.
type peekLine struct {
	raw    []byte
	offset int64
}

// linePeeker reads lines from a bufio.Reader, tracking stream offsets and
// supporting bounded look-ahead and push-back.
type linePeeker struct {
	r       *bufio.Reader
	pending []peekLine
	readOff int64
	err     error
}

func newLinePeeker(r io.Reader) *linePeeker {
	return &linePeeker{r: bufio.NewReader(r)}
}

// fill reads one more raw line from the underlying reader into pending.
func (p *linePeeker) fill() error {
	if p.err != nil {
		return p.err
	}
	raw, err := p.r.ReadBytes('\n')
	if len(raw) > 0 {
		p.pending = append(p.pending, peekLine{raw: raw, offset: p.readOff})
		p.readOff += int64(len(raw))
	}
	if err != nil {
		p.err = err
	}
	return err
}

// readLine consumes and returns the next line.
func (p *linePeeker) readLine() (peekLine, error) {
	if len(p.pending) == 0 {
		if err := p.fill(); err != nil && len(p.pending) == 0 {
			return peekLine{}, err
		}
	}
	l := p.pending[0]
	p.pending = p.pending[1:]
	return l, nil
}

// unreadLine pushes a previously read line back, so it is returned by the
// next readLine call.
func (p *linePeeker) unreadLine(l peekLine) {
	p.pending = append([]peekLine{l}, p.pending...)
}

// peek returns the n-th not yet consumed line without consuming it.
func (p *linePeeker) peek(n int) (peekLine, error) {
	for len(p.pending) <= n {
		if err := p.fill(); err != nil {
			if len(p.pending) > n {
				return p.pending[n], nil
			}
			return peekLine{}, err
		}
	}
	return p.pending[n], nil
}

// trimLineEnding strips a trailing CRLF or LF from a raw line.
func trimLineEnding(raw []byte) []byte {
	raw = bytes.TrimSuffix(raw, []byte{'\n'})
	raw = bytes.TrimSuffix(raw, []byte{'\r'})
	return raw
}

// RecoveringReader reads an mbox archive, recovering from malformed data.
//
// Unlike Reader, which stops at the first format error, RecoveringReader
// scans forward for the next line passing the separator validation rules and
// reports the raw regions it skipped. A candidate separator line is validated
// by checking the line prefix, the envelope structure and a bounded probe of
// the subsequent header fields. The scan itself is bounded by MaxScanBytes;
// if there is not enough evidence for a new message within this budget,
// NextMessage stops with ErrRecoveryLimit instead of scanning unboundedly.
type RecoveringReader struct {
	// MaxScanBytes bounds the number of bytes a single NextMessage call
	// scans while looking for the next valid separator. If zero,
	// DefaultMaxScanBytes is used.
	MaxScanBytes int64

	lines *linePeeker
	cur   *recoveringMessageReader
}

// NewRecoveringReader returns a new RecoveringReader to read messages from
// possibly malformed mbox file format data provided by io.Reader r.
func NewRecoveringReader(r io.Reader) *RecoveringReader {
	return &RecoveringReader{lines: newLinePeeker(r)}
}

// NextMessage returns the next message in the archive. It returns io.EOF if
// there are no messages left. If malformed data had to be skipped to find the
// message, the returned Message carries the Recovered flag and the skipped
// raw regions.
func (rr *RecoveringReader) NextMessage() (*Message, error) {
	if rr.cur != nil {
		if _, err := io.Copy(ioutil.Discard, rr.cur); err != nil {
			return nil, err
		}
		cur := rr.cur
		rr.cur = nil
		if cur.atEOF {
			return nil, io.EOF
		}
	}

	limit := rr.MaxScanBytes
	if limit <= 0 {
		limit = DefaultMaxScanBytes
	}

	var skipped []SkippedRegion
	var scanned int64
	for {
		line, err := rr.lines.readLine()
		if err != nil {
			return nil, err
		}
		scanned += int64(len(line.raw))
		if scanned > limit {
			return nil, ErrRecoveryLimit
		}

		content := trimLineEnding(line.raw)
		if len(content) == 0 {
			continue
		}
		if rr.validSeparator(content) {
			mr := &recoveringMessageReader{lines: rr.lines}
			rr.cur = mr
			return &Message{
				Reader:    mr,
				Offset:    line.offset,
				Recovered: len(skipped) > 0,
				Skipped:   skipped,
			}, nil
		}
		skipped = appendSkipped(skipped, line)
	}
}

// appendSkipped records line as skipped, merging it into the previous region
// if both are adjacent and of the same kind.
func appendSkipped(regions []SkippedRegion, l peekLine) []SkippedRegion {
	invalidSeparator := bytes.HasPrefix(trimLineEnding(l.raw), header)
	if n := len(regions); n > 0 {
		last := &regions[n-1]
		if last.InvalidSeparator == invalidSeparator && last.Offset+last.Length == l.offset {
			last.Length += int64(len(l.raw))
			return regions
		}
	}
	return append(regions, SkippedRegion{
		Offset:           l.offset,
		Length:           int64(len(l.raw)),
		InvalidSeparator: invalidSeparator,
	})
}

// validSeparator reports whether the trimmed line starting with "From " is a
// valid mbox separator line: it must have a plausible envelope structure and
// be followed by header fields within a bounded probe.
func (rr *RecoveringReader) validSeparator(line []byte) bool {
	if !bytes.HasPrefix(line, header) {
		return false
	}
	if !validEnvelope(line[len(header):]) {
		return false
	}
	return rr.probeHeaders()
}

// validEnvelope checks the structure of the envelope part of a separator
// line: a sender token followed by a date, which always contains digits.
func validEnvelope(rest []byte) bool {
	fields := bytes.Fields(rest)
	if len(fields) < 2 {
		return false
	}
	for _, f := range fields[1:] {
		if bytes.ContainsAny(f, "0123456789") {
			return true
		}
	}
	return false
}

// probeHeaders looks at the lines following a candidate separator line and
// reports whether they form the beginning of an RFC 5322 header block. The
// probe is bounded to maxHeaderProbeLines lines.
func (rr *RecoveringReader) probeHeaders() bool {
	found := false
	for i := 0; i < maxHeaderProbeLines; i++ {
		l, err := rr.lines.peek(i)
		if err != nil {
			// Not enough data left to gather more evidence.
			return found
		}
		content := trimLineEnding(l.raw)
		if len(content) == 0 {
			// End of the header block.
			return found
		}
		if isHeaderLine(content) {
			found = true
			continue
		}
		if found && (content[0] == ' ' || content[0] == '\t') {
			// Folded header continuation line.
			continue
		}
		return false
	}
	return found
}

// isHeaderLine reports whether the line looks like an RFC 5322 header field.
func isHeaderLine(line []byte) bool {
	i := bytes.IndexByte(line, ':')
	if i <= 0 {
		return false
	}
	for _, c := range line[:i] {
		if !isHeaderNameByte(c) {
			return false
		}
	}
	return true
}

func isHeaderNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '-'
}

// recoveringMessageReader reads a single message body, stopping at the next
// line that looks like a separator. The candidate line is not consumed; the
// RecoveringReader validates it when scanning for the next message.
type recoveringMessageReader struct {
	lines       *linePeeker
	next        bytes.Buffer
	atEOF       bool
	atSeparator bool
}

func (mr *recoveringMessageReader) Read(p []byte) (int, error) {
	if mr.atEOF || mr.atSeparator {
		return 0, io.EOF
	}

	for mr.next.Len() == 0 {
		line, err := mr.lines.readLine()
		if err != nil {
			mr.atEOF = true
			return 0, err
		}

		content := trimLineEnding(line.raw)
		if bytes.HasPrefix(content, header) {
			mr.lines.unreadLine(line)
			mr.atSeparator = true
			return 0, io.EOF
		}

		if len(content) == 0 {
			// Check if the next line looks like a separator. In such
			// case the blank line is not written to not have a double
			// new line.
			next, err := mr.lines.readLine()
			if err != nil {
				mr.atEOF = true
				return 0, err
			}
			if bytes.HasPrefix(trimLineEnding(next.raw), header) {
				mr.lines.unreadLine(next)
				mr.atSeparator = true
				return 0, io.EOF
			}
			mr.lines.unreadLine(next)
			mr.next.Write([]byte("\r\n"))
			continue
		}

		if bytes.HasPrefix(content, escapedHeader) {
			content = content[1:]
		}
		mr.next.Write(content)
		mr.next.Write([]byte("\r\n"))
	}

	return mr.next.Read(p)
}
