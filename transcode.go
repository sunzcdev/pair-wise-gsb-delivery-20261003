package avro

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

// defaultTranscodeBuffer is the default ceiling on the bytes a single
// reordering record may hold outside the destination while its fields are
// read in writer order and assembled in reader order. Verbatim subvalues are
// views of the input and do not count; only rewritten fragments are buffered.
const defaultTranscodeBuffer = 1 << 20

// Transcoder rewrites Avro binary data written with one schema into the
// binary form another schema expects, without materializing Go values. It
// applies the same schema-resolution rules as [Resolve]: field addition with
// defaults, field removal, name and alias matching, field reordering,
// numeric promotion, enum symbol remapping with the reader's enum default,
// and union branch remapping.
//
// A Transcoder is safe for concurrent use: the conversion plan is compiled
// once at construction, and each call keeps its own buffers. Subvalues whose
// wire bytes are identical under both schemas pass through as views of the
// input; only fields that must move or change are buffered, and only while
// the enclosing record is assembled.
type Transcoder struct {
	root   xcFn
	maxBuf int
}

// TranscodeOption configures a [Transcoder].
type TranscodeOption func(*transcodeConfig)

type transcodeConfig struct {
	maxBuf int
}

// WithMaxBufferedBytes bounds, per converted record, the bytes a reordering
// record may hold outside the destination while fields are read in writer
// order and assembled in reader order. Only rewritten fragments count:
// subvalues copied verbatim are zero-copy views of the input and are never
// charged. Fields emitted directly in reader order (the common case,
// including fields merely appended at the end) need no buffering at all.
//
// A negative value disables the limit (not recommended: a hostile or
// corrupted message could then drive an arbitrarily large buffer). Without
// the option the limit is 1 MiB.
func WithMaxBufferedBytes(n int) TranscodeOption {
	return func(c *transcodeConfig) { c.maxBuf = n }
}

// NewTranscoder compiles a plan that rewrites data written with writer into
// the binary form reader expects. It runs [CheckCompatibility] first and
// returns any incompatibility as a [*CompatibilityError], exactly as
// [Resolve] does.
func NewTranscoder(writer, reader *Schema, opts ...TranscodeOption) (*Transcoder, error) {
	if writer == nil || reader == nil {
		return nil, errors.New("avro: NewTranscoder: nil schema")
	}
	if err := CheckCompatibility(writer, reader); err != nil {
		return nil, err
	}
	cfg := transcodeConfig{maxBuf: defaultTranscodeBuffer}
	for _, opt := range opts {
		opt(&cfg)
	}
	// Canonical-equal schemas encode identically; the whole value is one
	// verbatim span. Compatibility already ran first because PCF strips
	// logicalType/precision/scale (see Resolve).
	if bytes.Equal(reader.Canonical(), writer.Canonical()) {
		mbw := newMinBytesWalk()
		return &Transcoder{root: &xcVerbatim{skip: buildSkip(writer.node, mbw)}, maxBuf: cfg.maxBuf}, nil
	}
	comp := &xcCompiler{
		seen: make(map[nodePair]*xcEntry),
		mbw:  newMinBytesWalk(),
	}
	root, err := comp.compile(reader.node, writer.node)
	if err != nil {
		return nil, err
	}
	// Every open cycle must have bound its trampoline.
	for pair, e := range comp.seen {
		if e.open || (e.tramp != nil && e.fn == nil) {
			return nil, fmt.Errorf("avro: internal: transcoder plan left an unbound cycle at %v", pair)
		}
	}
	return &Transcoder{root: root, maxBuf: cfg.maxBuf}, nil
}

// AppendTranscode rewrites one Avro value from src and appends the reader
// binary form to dst, returning the extended buffer and the unconsumed
// remainder of src. On error the returned slice carries every byte appended
// up to the failure point; see [TranscodeError] for the path and state.
// This slice form is append-only, so bytes appended before a failure sit
// where the caller put them; nothing is rolled back.
func (t *Transcoder) AppendTranscode(dst, src []byte) (out, rest []byte, err error) {
	st := newXCState(t.maxBuf)
	st.sink.mode = sinkAppend
	st.sink.buf = dst
	defer putXCState(st)

	rest, err = t.root.xcRun(st, &st.sink, src)
	return st.sink.buf, rest, st.finalizeErr(err)
}

// Transcode rewrites one Avro value and returns its reader binary form plus
// the unconsumed remainder of src. It is shorthand for
// AppendTranscode(nil, src).
func (t *Transcoder) Transcode(src []byte) (out, rest []byte, err error) {
	return t.AppendTranscode(nil, src)
}

// TranscodeTo writes one rewritten Avro value to w and returns the number of
// bytes written and the unconsumed remainder of src.
//
// Conversion reads and writes incrementally, so on error some bytes may
// already be flushed to w: an ordinary [io.Writer] offers no rollback, and
// none is attempted. The error reports the schema path and the number of
// bytes written when the failure occurred.
func (t *Transcoder) TranscodeTo(w io.Writer, src []byte) (written int, rest []byte, err error) {
	st := newXCState(t.maxBuf)
	st.sink.mode = sinkWriter
	st.sink.w = w
	defer putXCState(st)

	rest, err = t.root.xcRun(st, &st.sink, src)
	return st.sink.n, rest, st.finalizeErr(err)
}

// BufferLimitError reports that converting a reordering record required more
// buffered bytes than the Transcoder's limit. Raise the limit with
// [WithMaxBufferedBytes] when the schema's fields genuinely need it.
type BufferLimitError struct {
	// Limit is the configured ceiling in bytes.
	Limit int
	// Needed is the total the record's rewritten fragments required.
	Needed int
}

func (e *BufferLimitError) Error() string {
	return fmt.Sprintf("avro: transcode reorder buffer exceeded limit %d (needed %d); raise it with WithMaxBufferedBytes", e.Limit, e.Needed)
}

// TranscodeError wraps a conversion failure with the schema path at which it
// occurred and the state of the destination at that point.
type TranscodeError struct {
	// Path is the schema path of the value being converted: dotted field
	// names for records, [index] for array elements, ["key"] for map
	// entries, and union[branch] for a union branch.
	Path string
	// Written is the number of bytes handed to an [io.Writer] destination
	// before the failure; it is always 0 for a slice destination, where the
	// returned slice carries the same information.
	Written int
	// Err is the underlying failure.
	Err error
}

func (e *TranscodeError) Error() string {
	return "avro: transcode at " + e.Path + ": " + e.Err.Error()
}

func (e *TranscodeError) Unwrap() error { return e.Err }

// xcPathError carries one path segment attached during conversion; the
// public call boundary flattens the chain into one [TranscodeError].
type xcPathError struct {
	seg string
	err error
}

func (e *xcPathError) Error() string { return e.err.Error() }
func (e *xcPathError) Unwrap() error { return e.err }

// xcFail wraps err with a path segment. A nil err passes through, and an
// error already carrying a segment is rewrapped with the new (outer)
// segment rather than nesting two wrappers, since flattenXCPath reads the
// chain inner-most-first.
func xcFail(seg string, err error) error {
	if err == nil {
		return nil
	}
	if pe, ok := err.(*xcPathError); ok {
		err = pe.err
	}
	return &xcPathError{seg: seg, err: err}
}

// xcFailField wraps a record field error so that a child path already
// attached by an array, map or union is retained. Unlike xcFail it keeps
// the existing segment chain, producing "field.child" rather than
// replacing the inner-most segment.
func xcFailField(field string, err error) error {
	if err == nil {
		return nil
	}
	// Every wrapper already attached carries a child segment; wrap again
	// without unwrapping, so flattenXCPath keeps the full chain.
	return &xcPathError{seg: field, err: err}
}

// xcFailKeep attaches a segment while preserving any segment already on
// err. Container item/key wrappers use it so that an inner union branch
// segment survives and enclosing record fields prefix rather than replace
// it: field[0].union[1] rather than only union[1].
func xcFailKeep(seg string, err error) error {
	if err == nil {
		return nil
	}
	return &xcPathError{seg: seg, err: err}
}

// flattenXCPath renders the chain of path segments as one root-first path.
// Unwrapping xcPathError visits the outer-most segment first (a record
// field wraps an array item, which wraps a union branch), so segments are
// appended in unwrap order. Bracket segments ([i], ["k"], union[n]) join
// directly onto the preceding name; field segments join with a dot.
func flattenXCPath(err error) string {
	var segs []string
	for {
		var pe *xcPathError
		if !errors.As(err, &pe) {
			break
		}
		segs = append(segs, pe.seg)
		err = pe.err
	}
	path := ""
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		switch {
		case path == "":
			path = seg
		case seg[0] == '[':
			path += seg
		default:
			path += "." + seg
		}
	}
	return pathOrRoot(path)
}

// leafError unwraps every path segment and returns the underlying error.
func leafError(err error) error {
	for {
		pe, ok := err.(*xcPathError)
		if !ok {
			return err
		}
		err = pe.err
	}
}

// ---------- per-call state ----------

const (
	sinkAppend = iota
	sinkWriter
	sinkCapture
)

// xcSink is the destination every xcFn writes through. In append mode it
// grows a byte slice; in writer mode it flushes through an io.Writer; in
// capture mode a reordering record collects fields out of order and
// reassembles them.
//
// emitView forwards bytes that are a view of the conversion input and
// outlive the call; emitBytes forwards freshly produced bytes. Capture
// mode copies only the latter into its bounded scratch buffer, while views
// stay zero-copy.
type xcSink struct {
	mode int

	// append mode
	buf []byte
	// writer mode
	w io.Writer
	n int

	// capture mode
	parent  *xcSink
	fields  []xcFrag
	cur     *xcFrag
	scratch []byte
	maxBuf  int

	err error
}

func (s *xcSink) emitView(b []byte) error {
	if len(b) == 0 {
		return s.err
	}
	if s.err != nil {
		return s.err
	}
	if s.mode == sinkCapture {
		if s.cur == nil {
			return errors.New("avro: internal: transcoder capture outside a field")
		}
		s.cur.ranges = append(s.cur.ranges, xcRange{view: b})
		return nil
	}
	if s.mode == sinkAppend {
		s.buf = append(s.buf, b...)
		return nil
	}
	n, err := s.w.Write(b)
	s.n += n
	if err != nil {
		s.err = err
		return err
	}
	if n != len(b) {
		s.err = io.ErrShortWrite
		return io.ErrShortWrite
	}
	return nil
}

func (s *xcSink) emitBytes(b []byte) error {
	if s.mode != sinkCapture {
		return s.emitView(b)
	}
	if s.err != nil {
		return s.err
	}
	if s.cur == nil {
		return errors.New("avro: internal: transcoder capture outside a field")
	}
	if s.maxBuf >= 0 && len(s.scratch)+len(b) > s.maxBuf {
		s.err = &BufferLimitError{Limit: s.maxBuf, Needed: len(s.scratch) + len(b)}
		return s.err
	}
	start := len(s.scratch)
	s.scratch = append(s.scratch, b...)
	s.cur.ranges = append(s.cur.ranges, xcRange{start: start, end: len(s.scratch)})
	return nil
}

func (s *xcSink) written() int {
	if s.mode == sinkWriter {
		return s.n
	}
	return 0
}

// xcRange is one byte run of a captured field: either an input view (view
// set) or a run [start:end] of the capture's bounded scratch buffer.
type xcRange struct {
	view       []byte
	start, end int
}

// xcFrag collects a reader field's runs while a buffering record reads
// writer fields. present distinguishes a matched field from one the
// assembly must fill from a default.
type xcFrag struct {
	ranges  []xcRange
	present bool
}

// xcState is one conversion call's private state. A compiled Transcoder
// never mutates after construction, so independent concurrent calls use
// independent states.
type xcState struct {
	slab slab
	sink xcSink

	// freeCaps pools capture sinks buffered records borrow; captures are
	// heap allocated so nested captures cannot invalidate a parent's
	// pointer (a slice-based stack could reallocate mid-descent).
	freeCaps []*xcSink

	maxBuf int
}

var xcStatePool = sync.Pool{}

func newXCState(maxBuf int) *xcState {
	if v := xcStatePool.Get(); v != nil {
		st := v.(*xcState)
		*st = xcState{maxBuf: maxBuf}
		return st
	}
	return &xcState{maxBuf: maxBuf}
}

func putXCState(st *xcState) {
	*st = xcState{}
	xcStatePool.Put(st)
}

func (st *xcState) finalizeErr(err error) error {
	if err == nil {
		return nil
	}
	return &TranscodeError{
		Path:    flattenXCPath(err),
		Written: st.sink.written(),
		Err:     leafError(err),
	}
}
