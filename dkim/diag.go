package dkim

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
)

// DiagStage identifies the processing stage a DiagEvent belongs to.
type DiagStage string

const (
	// DiagStageHeader to identify selection of a header field for hashing.
	// When a header field name occurs multiple times, the event Summary
	// reports which instance was picked. Instances are numbered from the
	// bottom of the header block, as mandated by RFC 6376 section 5.4.2.
	DiagStageHeaderPick DiagStage = "header-pick"
	// DiagStageBodyLine is emitted for each body line consumed by the body
	// canonicalizer.
	DiagStageBodyLine DiagStage = "body-line"
	// DiagStageBodySummary is emitted once, after the whole body has been
	// canonicalized. Its Summary reports how trailing empty lines
	// participated in canonicalization.
	DiagStageBodySummary DiagStage = "body-summary"
)

// A DiagEvent describes a single canonicalization step performed while
// signing or verifying a message.
//
// Events never carry message content unless DiagOptions.IncludeContent is
// set: by default only byte ranges, byte counts and summaries are reported,
// so a trace is safe to share with a remote peer. Note that a trace (like a
// hash) cannot reconstruct the other end's message: differences between two
// ends can only be pinpointed once the peer's raw message or the peer's own
// trace is available for comparison.
type DiagEvent struct {
	// Stage is the processing stage that produced the event.
	Stage DiagStage
	// InputOffset is the byte offset of the input consumed by this event.
	// For DiagStageHeaderPick it is relative to the start of the message
	// header block; for body events it is relative to the start of the
	// body. It is -1 when the event has no corresponding message input
	// (e.g. the generated DKIM-Signature field while signing).
	InputOffset int64
	// InputLength is the number of input bytes consumed by this event.
	InputLength int
	// OutputBytes is the number of canonicalized bytes fed to the hash for
	// this event. It may be zero, e.g. for trailing empty body lines which
	// are dropped by canonicalization.
	OutputBytes int
	// Summary is a short machine-readable description of the event, e.g.
	// "field=subject instance=1 of=2" for a picked header field, or
	// "lines=5 empty=2 trailing-empty=2 input=30 output=24" for the body
	// summary.
	Summary string
	// Content holds the canonicalized output bytes for this event. It is
	// always nil unless DiagOptions.IncludeContent is set.
	Content []byte
}

// DiagOptions configures diagnostic event delivery for Sign and Verify.
type DiagOptions struct {
	// Emit is called for each diagnostic event. It must not retain or
	// mutate the event. This field is required.
	Emit func(DiagEvent)
	// MaxEvents strictly caps the total number of events delivered to
	// Emit: once the budget is exhausted, further events are silently
	// dropped. Zero means no limit.
	MaxEvents int
	// IncludeContent allows events to carry canonicalized output bytes in
	// DiagEvent.Content. It is off by default, so that traces don't
	// contain message content.
	IncludeContent bool
}

// diagEmitter delivers events to a DiagOptions sink while strictly
// enforcing the event budget. It is safe for concurrent use.
type diagEmitter struct {
	opts *DiagOptions

	mu      sync.Mutex
	sent    int
	dropped int
}

// newDiagEmitter returns nil if opts is nil or has no sink.
func newDiagEmitter(opts *DiagOptions) *diagEmitter {
	if opts == nil || opts.Emit == nil {
		return nil
	}
	return &diagEmitter{opts: opts}
}

func (e *diagEmitter) emit(ev DiagEvent) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.opts.MaxEvents > 0 && e.sent >= e.opts.MaxEvents {
		e.dropped++
		e.mu.Unlock()
		return
	}
	e.sent++
	e.mu.Unlock()

	if !e.opts.IncludeContent {
		ev.Content = nil
	}
	e.opts.Emit(ev)
}

// headerPick emits an event for a header field selected by p. canon is the
// canonicalized form of the field.
func (e *diagEmitter) headerPick(p *headerPicker, idx int, canon string, offsets []int64) {
	if e == nil {
		return
	}
	kv := p.h[idx]
	k, _ := parseHeaderField(kv)
	e.emit(DiagEvent{
		Stage:       DiagStageHeaderPick,
		InputOffset: offsets[idx],
		InputLength: len(kv),
		OutputBytes: len(canon),
		Summary: fmt.Sprintf("field=%s instance=%d of=%d",
			strings.ToLower(k), p.picked[strings.ToLower(k)], countHeaderField(p.h, k)),
		Content: []byte(canon),
	})
}

// headerOffsets returns the byte offset of each header field within the
// header block.
func headerOffsets(h header) []int64 {
	offsets := make([]int64, len(h))
	var off int64
	for i, kv := range h {
		offsets[i] = off
		off += int64(len(kv))
	}
	return offsets
}

// countHeaderField counts the occurrences of a header field name.
func countHeaderField(h header, key string) int {
	n := 0
	for _, kv := range h {
		k, _ := parseHeaderField(kv)
		if strings.EqualFold(k, key) {
			n++
		}
	}
	return n
}

// diagCountWriter counts (and optionally captures) the canonicalized bytes
// a body canonicalizer writes to the hash.
type diagCountWriter struct {
	w       io.Writer
	n       int64
	capture bool
	buf     []byte
}

func (w *diagCountWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	w.n += int64(n)
	if w.capture {
		w.buf = append(w.buf, b[:n]...)
	}
	return n, err
}

// diagBodyWriter wraps a body canonicalizer and emits diagnostic events at
// body line boundaries. Line boundaries depend only on the input byte
// stream, and the underlying canonicalizers are deterministic functions of
// the bytes consumed so far, so the emitted offsets, output byte counts and
// summaries are identical no matter how the input is chunked into Write
// calls.
type diagBodyWriter struct {
	wc  io.WriteCloser
	out *diagCountWriter
	em  *diagEmitter

	in        int64 // total input bytes consumed
	lineStart int64 // offset of the current line
	lastOut   int64 // output bytes attributed to previous events

	lines              int
	emptyLines         int
	trailingEmptyLines int
}

func newDiagBodyWriter(wc io.WriteCloser, out *diagCountWriter, em *diagEmitter) *diagBodyWriter {
	return &diagBodyWriter{wc: wc, out: out, em: em}
}

func (w *diagBodyWriter) Write(b []byte) (int, error) {
	n, err := w.wc.Write(b)
	w.advance(b[:n])
	return n, err
}

func (w *diagBodyWriter) advance(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			w.in += int64(len(b))
			return
		}
		w.in += int64(i + 1)
		w.endLine(b[:i+1])
		b = b[i+1:]
	}
}

// endLine emits an event for the line ending at offset w.in.
func (w *diagBodyWriter) endLine(line []byte) {
	length := int(w.in - w.lineStart)
	empty := len(line) > 0 && len(bytes.TrimRight(string(line), "\r\n")) == 0 &&
	(len(line) == 1 || line[len(line)-2] == '\r')

	w.lines++
	if empty {
		w.emptyLines++
		w.trailingEmptyLines++
	} else {
		w.trailingEmptyLines = 0
	}

	out := w.out.n
	var content []byte
	if w.out.capture {
		content = append([]byte(nil), w.out.buf[w.lastOut:]...)
	}
	w.em.emit(DiagEvent{
		Stage:       DiagStageBodyLine,
		InputOffset: w.lineStart,
		InputLength: length,
		OutputBytes: int(out - w.lastOut),
		Summary:     fmt.Sprintf("line=%d empty=%t", w.lines, empty),
		Content:     content,
	})
	w.lastOut = out
	w.lineStart = w.in
}

func (w *diagBodyWriter) Close() error {
	// Flush a final line not terminated by \n. It cannot be empty.
	if w.lineStart < w.in {
		w.endLine(nil)
	}
	if err := w.wc.Close(); err != nil {
		return err
	}

	var content []byte
	if w.out.capture {
		content = append([]byte(nil), w.out.buf...)
	}
	w.em.emit(DiagEvent{
		Stage:       DiagStageBodySummary,
		InputOffset: 0,
		InputLength: int(w.in),
		OutputBytes: int(w.out.n),
		Summary: fmt.Sprintf("lines=%d empty=%d trailing-empty=%d input=%d output=%d",
			w.lines, w.emptyLines, w.trailingEmptyLines, w.in, w.out.n),
		Content: content,
	})
	return nil
}
