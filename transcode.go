package avro

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
)

// ErrReorderBufferExceeded is the cause of a [*TranscodeError] when a field
// whose writer position trails its reader position cannot be buffered because
// doing so would exceed the limit set with [WithReorderBuffer]. A nil error
// from [Transcode] never wraps it.
var ErrReorderBufferExceeded = errors.New("avro: reorder buffer limit exceeded")

// TranscodeStage names where a transcode stopped.
type TranscodeStage string

const (
	// StageRead is set when the failure happened while reading or parsing
	// the writer's binary input: a truncated or malformed buffer, or a
	// rejected writer value such as an out-of-range enum or union index.
	StageRead TranscodeStage = "read"
	// StageReorder is set when a field needed buffering for reordering but
	// the [WithReorderBuffer] limit did not allow it.
	StageReorder TranscodeStage = "reorder"
	// StageWrite is set when output could not be written: append capacity
	// on a growable slice never fails, so this stage only occurs with an
	// [io.Writer] passed to [Transcoder.Write].
	StageWrite TranscodeStage = "write"
)

// TranscodeError reports a failed binary transcode.
//
// Path is the dotted location inside the value, using the reader field
// names: "address.zip", "items[3]", "m['a key']". It is "(root)" when the
// failure is at the top level.
//
// Written reports how many reader-binary bytes had already been committed to
// the destination when the failure occurred. With an append destination
// those bytes that fit were appended to dst; with an [io.Writer], bytes
// already handed to Write are counted even if a later Write fails. A plain
// io.Writer cannot be rolled back, so Written is informational, not a
// restoration point: use a buffering writer (for example a [bytes.Buffer],
// or an [io.Writer] you control) when you need atomic output.
type TranscodeError struct {
	// Path is the dotted reader-field path, with array and map indexes
	// inlined ("items[3]", "m['a key']"), or "(root)" at the top.
	Path string
	// Stage is where the transcode stopped; see the [TranscodeStage] docs.
	Stage TranscodeStage
	// Written is the number of output bytes already committed.
	Written int
	// Err is the underlying error. It is a [*ShortBufferError] for a
	// truncated writer buffer.
	Err error
}

func (e *TranscodeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("avro: transcode at %s: %s: %v", truncForError(e.Path), e.Stage, e.Err)
}

func (e *TranscodeError) Unwrap() error { return e.Err }

// TranscodeOpt configures a [Transcoder].
type TranscodeOpt interface{ transcodeOpt() }

type reorderLimitOpt struct{ limit int }

func (reorderLimitOpt) transcodeOpt() {}

// defaultReorderLimit bounds the bytes a single record transcode may hold
// back to move a trailing writer field ahead of its reader position. Callers
// that know their payloads are larger set the limit explicitly with
// [WithReorderBuffer]; callers that never reorder set it to zero so a
// reordered wire fails instead of buffering.
const defaultReorderLimit = 1 << 20 // 1 MiB

// WithReorderBuffer caps the bytes held back for one record while its fields
// are reordered from writer order to reader order. The limit applies
// independently to each record being transcoded, including nested records;
// it never bounds total output, which streams directly to the destination.
//
// A positive n allows buffering up to n bytes. Zero disables reorder
// buffering, so a wire whose fields must move forward fails with an error
// wrapping [ErrReorderBufferExceeded] when the first such datum is
// transcoded. A negative n removes the limit, so a reordered field may grow
// to the size of the input; use it only when the input is already trusted
// to be bounded. With no option the limit is defaultReorderLimit.
//
// Only fields that genuinely need to move are buffered. Fields already in
// reader order, including the whole value when both schemas lay fields out
// identically, are copied straight through without touching the limit.
func WithReorderBuffer(n int) TranscodeOpt { return reorderLimitOpt{limit: n} }

// Transcoder converts a value encoded with one schema into the same value
// encoded with a second schema without materializing a Go value in between.
// It applies the schema-resolution rules [Resolve] uses:
//
//   - reader-only fields are filled from their defaults, including enum
//     defaults and union defaults;
//   - writer-only fields are skipped;
//   - fields shared by both are matched by name or alias and may appear in
//     either order, with trailing fields buffered up to the
//     [WithReorderBuffer] limit;
//   - numeric values are promoted (int to long/float/double, long to
//     float/double, float to double);
//   - string and bytes convert into one another;
//   - enum symbol indexes are remapped, with the reader enum default taking
//     a writer symbol the reader dropped;
//   - union branches are mapped by name, alias or promotion, rewriting the
//     branch index;
//   - named types match by full name, unqualified name, or alias.
//
// Sub-values whose wire bytes are identical under both schemas — matching
// primitives, fixed types, fully ordered records, and whole blocks inside
// arrays and maps — are copied straight from input to output without being
// parsed.
//
// A Transcoder is the compiled plan for one (writer, reader) pair. It is
// safe for concurrent use by independent [Transcoder.Append] and
// [Transcoder.Write] calls; per-call state is not shared.
type Transcoder struct {
	root  tfn
	limit int
}

// Transcode compiles a plan that rewrites writer-binary values into
// reader-binary values. It runs [CheckCompatibility] first, so every
// incompatibility is reported as a [*CompatibilityError] before any data
// flows, exactly as [Resolve] does.
func Transcode(writer, reader *Schema, opts ...TranscodeOpt) (*Transcoder, error) {
	if err := CheckCompatibility(writer, reader); err != nil {
		return nil, err
	}
	limit := defaultReorderLimit
	for _, o := range opts {
		if rl, ok := o.(reorderLimitOpt); ok {
			limit = rl.limit
		}
	}
	if bytes.Equal(reader.Canonical(), writer.Canonical()) {
		return &Transcoder{root: spanTfn(buildSpan(reader.node)), limit: limit}, nil
	}
	ctx := &tCompile{
		seen:  make(map[nodePair]*tbox),
		limit: limit,
	}
	fn, _, err := ctx.compile(reader.node, writer.node, "")
	if err != nil {
		return nil, err
	}
	return &Transcoder{root: fn, limit: limit}, nil
}

// Append transcodes one writer-binary value from src and appends its
// reader-binary encoding to dst. The returned slice is the bytes after the
// consumed value, like [Schema.Decode]'s remaining bytes; on failure it is
// src unchanged. A failure is a [*TranscodeError].
func (t *Transcoder) Append(dst, src []byte) ([]byte, []byte, error) {
	tc := &tctx{limit: t.limit, sink: &bufSink{b: dst}, base: len(dst)}
	rest, err := t.root(src, tc)
	if err != nil {
		return dst, src, asTranscodeError(err, tc.sink.written()-tc.base)
	}
	return tc.sink.(*bufSink).b, rest, nil
}

// Write transcodes one writer-binary value from src and writes its
// reader-binary encoding to w. Bytes are streamed as fields are emitted, so
// in the absence of reordering nearly the whole value is forwarded before
// later input is read. The returned slice is the bytes left after the
// consumed value.
//
// A failure is a [*TranscodeError] whose Written reports bytes already
// handed to w; Write is not rolled back on a plain io.Writer.
func (t *Transcoder) Write(w io.Writer, src []byte) ([]byte, error) {
	tc := &tctx{limit: t.limit, sink: &ioSink{w: w}}
	rest, err := t.root(src, tc)
	if err != nil {
		return src, asTranscodeError(err, tc.sink.written()-tc.base)
	}
	return rest, nil
}

// ---------- per-call state ----------

// tfn transcodes one value, returning the bytes after it. Errors are tagged
// with readFail/writeFail/reorderFail; asTranscodeError turns the first one
// encountered into a *TranscodeError.
type tfn func(src []byte, tc *tctx) ([]byte, error)

type tctx struct {
	depth int
	limit int
	sink  sink
	base  int // sink length/count when the call began; subtracted from Written
	// free holds record frames whose pending slices can be reused across
	// nested records within one call.
	free []*recFrame
}

// recFrame is one buffered record's reorder state.
type recFrame struct {
	// pending[ri] holds the reader-binary bytes captured for matched
	// reader field ri while a later reader slot has not been emitted.
	pending [][]byte
}

func (tc *tctx) frame(n int) *recFrame {
	if len(tc.free) > 0 {
		f := tc.free[len(tc.free)-1]
		tc.free = tc.free[:len(tc.free)-1]
		if cap(f.pending) >= n {
			f.pending = f.pending[:n]
			for i := range f.pending {
				f.pending[i] = nil
			}
			return f
		}
	}
	return &recFrame{pending: make([][]byte, n)}
}

func (tc *tctx) release(f *recFrame) {
	for i := range f.pending {
		f.pending[i] = nil
	}
	tc.free = append(tc.free, f)
}

func (tc *tctx) enter() error {
	tc.depth++
	if tc.depth >= maxDepth {
		return errTooDeep
	}
	return nil
}

func (tc *tctx) leave() { tc.depth-- }

// sink is the output target shared by every tfn in one call.
type sink interface {
	emit(b []byte) error
	written() int
}

type bufSink struct{ b []byte }

func (s *bufSink) emit(b []byte) error { s.b = append(s.b, b...); return nil }
func (s *bufSink) written() int        { return len(s.b) }

type ioSink struct {
	w     io.Writer
	count int
}

func (s *ioSink) emit(b []byte) error {
	n, err := s.w.Write(b)
	s.count += n
	return err
}
func (s *ioSink) written() int { return s.count }

// ---------- error tagging ----------

type taggedFail struct {
	stage TranscodeStage
	path  string
	err   error
}

func (f *taggedFail) Error() string { return f.err.Error() }
func (f *taggedFail) Unwrap() error { return f.err }

func readFail(err error) error {
	if err == nil || isTaggedFail(err) {
		return err
	}
	return &taggedFail{stage: StageRead, path: "", err: err}
}

func writeFail(err error) error {
	if err == nil || isTaggedFail(err) {
		return err
	}
	return &taggedFail{stage: StageWrite, path: "", err: err}
}

func reorderFail(err error) error {
	if err == nil || isTaggedFail(err) {
		return err
	}
	return &taggedFail{stage: StageReorder, path: "", err: err}
}

func isTaggedFail(err error) bool {
	var f *taggedFail
	return errors.As(err, &f)
}

// wrapPath prefixes a tagged failure's path with one record-field step.
func wrapPath(err error, field string) error {
	var f *taggedFail
	if !errors.As(err, &f) {
		return err
	}
	cp := *f
	cp.path = field + pathTail(f.path)
	return &cp
}

// wrapIndex prefixes with an array index step.
func wrapIndex(err error, i int) error {
	var f *taggedFail
	if !errors.As(err, &f) {
		return err
	}
	cp := *f
	cp.path = fmt.Sprintf("[%d]", i) + pathTail(f.path)
	return &cp
}

// wrapKey prefixes with a map-key step.
func wrapKey(err error, key string) error {
	var f *taggedFail
	if !errors.As(err, &f) {
		return err
	}
	cp := *f
	cp.path = "[" + quoteKey(key) + "]" + pathTail(f.path)
	return &cp
}

func pathTail(p string) string {
	if p == "" {
		return ""
	}
	return "." + p
}

func asTranscodeError(err error, written int) error {
	var f *taggedFail
	if !errors.As(err, &f) {
		return &TranscodeError{Path: pathOrRoot(""), Stage: StageRead, Written: written, Err: err}
	}
	return &TranscodeError{Path: pathOrRoot(f.path), Stage: f.stage, Written: written, Err: f.err}
}

// quoteKey renders a map key as a single-quoted token, so a key containing a
// quote or newline stays one unambiguous path segment.
func quoteKey(k string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range k {
		switch r {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// ---------- compilation ----------

// tCompile is one Transcode plan build. seen memoizes (reader, writer)
// pairs so a DAG is linear work, and a pair in progress on the recursion
// stack closes a recursive schema through a trampoline tbox, the same
// three-state scheme resolveNode uses.
type tCompile struct {
	seen  map[nodePair]*tbox
	limit int
}

// tbox is a forward-reference cell: its fn field is filled in once the pair
// compiling it returns. Every tfn that met the pair closed over box.fn, so
// filling it later reaches every holder.
type tbox struct{ fn tfn }

// compile builds the tfn for one (reader, writer) pair. identical reports
// whether the pair's reader- and writer-wire encodings are produced by the
// same operation, so an enclosing record/array/map/union may copy the
// subtree verbatim. Compilation terminates on recursive schemas the same
// way [Resolve] does: a pair reached while it is already compiling closes
// through a placeholder.
func (c *tCompile) compile(r, w *schemaNode, path string) (fn tfn, identical bool, err error) {
	pair := nodePair{r, w}
	if box, ok := c.seen[pair]; ok {
		if box == nil {
			box = &tbox{}
			box.fn = func(src []byte, tc *tctx) ([]byte, error) { return box.fn(src, tc) }
			c.seen[pair] = box
		}
		// A pair on a cycle (or reached through one) cannot claim wire
		// identity: the claim is a property of the full subtree, and a
		// back edge does not see it. The false answer only forfeits an
		// optimization, never changes the output.
		return box.fn, false, nil
	}
	c.seen[pair] = nil

	fn, identical, err = c.compileEntry(r, w, path)
	if err != nil {
		return nil, false, err
	}
	if placeholder := c.seen[pair]; placeholder != nil {
		placeholder.fn = fn
		return placeholder.fn, false, nil
	}
	c.seen[pair] = &tbox{fn: fn}
	return fn, identical, nil
}

func (c *tCompile) compileEntry(r, w *schemaNode, path string) (tfn, bool, error) {
	switch {
	case w.kind == "union" && r.kind != "union":
		fn, err := c.compileWriterUnion(r, w, path)
		return fn, false, err
	case r.kind == "union" && w.kind != "union":
		fn, err := c.compileReaderUnion(r, w, path)
		return fn, false, err
	case r.kind == "union" && w.kind == "union":
		return c.compileUnionUnion(r, w, path)
	}

	if r.kind != w.kind {
		fn, ok := c.compilePromotion(r, w, path)
		if !ok {
			return nil, false, &CompatibilityError{
				Path:       pathOrRoot(path),
				ReaderType: r.kind,
				WriterType: w.kind,
				Detail:     "incompatible types",
			}
		}
		return fn, false, nil
	}

	switch r.kind {
	case "record":
		return c.compileRecord(r, w, path)
	case "enum":
		return c.compileEnum(r, w, path)
	case "array":
		return c.compileArray(r, w, path)
	case "map":
		return c.compileMap(r, w, path)
	default:
		// Primitives and fixed: same kind is the same wire. CheckCompatibility
		// already required a fixed's name and size to match.
		return spanTfn(buildSpan(r)), true, nil
	}
}

// compilePromotion builds the eight cross-kind leaves CheckCompatibility
// allows. A copy leaf (string<->bytes) keeps the same bytes; the numeric
// leaves re-encode into the reader width. No container kind ever reaches
// here.
func (c *tCompile) compilePromotion(r, w *schemaNode, path string) (tfn, bool) {
	switch w.kind + ">" + r.kind {
	case "int>long":
		return transcodeIntLong, true
	case "int>float":
		return transcodeIntFloat, true
	case "int>double":
		return transcodeIntDouble, true
	case "long>float":
		return transcodeLongFloat, true
	case "long>double":
		return transcodeLongDouble, true
	case "float>double":
		return transcodeFloatDouble, true
	case "string>bytes", "bytes>string":
		// A string and bytes have the identical length-prefixed wire.
		return spanTfn(buildSpan(w)), true
	}
	return nil, false
}

// compileEnum mirrors resolveEnum's mapping, including the reader enum
// default for a writer symbol the reader dropped. When every writer symbol
// keeps its ordinal the whole enum is a copy.
func (c *tCompile) compileEnum(r, w *schemaNode, path string) (tfn, bool, error) {
	readerIdx := make(map[string]int, len(r.symbols))
	for i, s := range r.symbols {
		readerIdx[s] = i
	}
	identity := len(r.symbols) == len(w.symbols)
	mapping := make([]int, len(w.symbols))
	for i, ws := range w.symbols {
		if ri, ok := readerIdx[ws]; ok {
			mapping[i] = ri
			if ri != i {
				identity = false
			}
			continue
		}
		identity = false
		if !r.hasEnumDef {
			return nil, false, &CompatibilityError{
				Path:       pathOrRoot(path),
				ReaderType: r.name,
				WriterType: w.name,
				Detail:     fmt.Sprintf("writer symbol %q not in reader and no default", truncForError(ws)),
			}
		}
		defIdx, ok := readerIdx[r.enumDef]
		if !ok {
			return nil, false, fmt.Errorf("enum default %q not found in reader symbols", truncForError(r.enumDef))
		}
		mapping[i] = defIdx
	}
	if identity {
		return spanTfn(buildSpan(r)), true, nil
	}
	m := mapping
	fn := tfn(func(src []byte, tc *tctx) ([]byte, error) {
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, readFail(err)
		}
		if idx < 0 || int(idx) >= len(m) {
			return nil, readFail(fmt.Errorf("enum index %d out of range [0, %d)", idx, len(m)))
		}
		if err := tc.sink.emit(appendVarint(nil, int32(m[idx]))); err != nil {
			return nil, writeFail(err)
		}
		return rest, nil
	})
	return fn, false, nil
}

func (c *tCompile) compileArray(r, w *schemaNode, path string) (tfn, bool, error) {
	item, identical, err := c.compile(r.items, w.items, path+".items")
	if err != nil {
		return nil, false, err
	}
	if identical {
		return spanTfn(buildSpan(w)), true, nil
	}
	fn := makeBlockTranscode(false, item)
	return fn, false, nil
}

func (c *tCompile) compileMap(r, w *schemaNode, path string) (tfn, bool, error) {
	val, identical, err := c.compile(r.values, w.values, path+".values")
	if err != nil {
		return nil, false, err
	}
	if identical {
		return spanTfn(buildSpan(w)), true, nil
	}
	fn := makeBlockTranscode(true, val)
	return fn, false, nil
}

// makeBlockTranscode shares the array and map block walk. keys is true for a
// map, whose each entry leads with a length-prefixed string key; the key is
// identical under both schemas and is copied, so it is read with a string
// span and the value tfn alone may rewrite.
func makeBlockTranscode(keys bool, val tfn) tfn {
	keySpan := sfn(nil)
	if keys {
		keySpan = buildSpan(&schemaNode{kind: "string"})
	}
	blockType := "array"
	if keys {
		blockType = "map"
	}
	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.enter(); err != nil {
			return nil, readFail(err)
		}
		defer tc.leave()
		var total int64
		for {
			count, _, rest, end, err := readBlockHeader(src, blockType, false)
			if err != nil {
				return nil, readFail(err)
			}
			src = rest
			if end {
				if err := tc.sink.emit([]byte{0}); err != nil {
					return nil, writeFail(err)
				}
				return src, nil
			}
			if err := guardBlockCount(count, total, len(src), keys); err != nil {
				return nil, readFail(err)
			}
			total += count
			var head [10]byte
			n := appendVarlong(head[:0], count)
			if err := tc.sink.emit(n); err != nil {
				return nil, writeFail(err)
			}
			for i := int64(0); i < count; i++ {
				if keys {
					rest, err := keySpan(src, tc)
					if err != nil {
						return nil, readFail(err)
					}
					key := src[:len(src)-len(rest)]
					if err := tc.sink.emit(key); err != nil {
						return nil, writeFail(err)
					}
					src = rest
					rest, err = val(src, tc)
					if err != nil {
						label := mapKeyLabel(key)
						return nil, wrapKey(err, label)
					}
					src = rest
				} else {
					rest, err := val(src, tc)
					if err != nil {
						return nil, wrapIndex(err, int(total-count+i))
					}
					src = rest
				}
			}
		}
	}
}

// mapKeyLabel extracts the string content of a wire map key for an error
// path, tolerating truncation by reporting the bytes available.
func mapKeyLabel(wireKey []byte) string {
	n, rest, err := readVarlong(wireKey)
	if err != nil || n < 0 || int(n) > len(rest) {
		return truncForError(string(wireKey))
	}
	return string(rest[:n])
}

// guardBlockCount applies the same DoS bounds the decode and skip paths do,
// using only the input length and, for zero-byte-item arrays, the same
// absolute cap. minBytesUnknown is the stand-in when no per-item minimum is
// to hand: the two sound rules' union never false-rejects a valid block.
func guardBlockCount(count, total int64, srcLen int, keys bool) error {
	if keys {
		// A map entry always carries at least its key's length varint.
		return checkMapBlockBounds(count, srcLen, 1)
	}
	return checkArrayBlockBounds(count, total, srcLen, minBytesUnknown)
}

// recStepKind tags one streaming record step.
type recStepKind uint8

const (
	stepSkip    recStepKind = iota // writer-only field; advance past it
	stepField                      // matched field, already in emit position
	stepDefault                    // reader-only default to emit
)

type recStep struct {
	kind recStepKind
	span sfn    // stepSkip: advance past this writer field
	fn   tfn    // stepField: transcode into the current reader slot
	def  []byte // stepDefault: pre-encoded reader default
	name string // stepSkip: writer field name, for the error path
}

// recWireField is one resolved writer field: readerIdx is -1 for a
// writer-only field, span advances past the field on the wire in every case,
// and fn transcodes a matched field.
type recWireField struct {
	readerIdx int
	span      sfn
	fn        tfn
	name      string
	identical bool // true when this matched field's bytes pass through
}

// compileRecord resolves the two field lists, then picks the streaming plan
// when matched reader indices arrive in strictly ascending writer order
// (identity order is one such case), and otherwise compiles the buffered
// plan that holds out-of-place fields until their reader slot is reached.
func (c *tCompile) compileRecord(r, w *schemaNode, path string) (tfn, bool, error) {
	readerByName := newReaderFieldLookup(r)

	wire := make([]recWireField, 0, len(w.fields))
	matched := make([]bool, len(r.fields))
	matchedBy := make([]string, len(r.fields))

	streamable := true
	lastMatched := -1

	for i := range w.fields {
		wf := &w.fields[i]
		ri := readerByName.index(wf.name)
		wspan := buildSpan(wf.node)
		if ri < 0 {
			wire = append(wire, recWireField{readerIdx: -1, span: wspan, name: wf.name})
			continue
		}
		if matched[ri] {
			return nil, false, &CompatibilityError{
				Path:       pathOrRoot(path),
				ReaderType: "record",
				WriterType: "record",
				Detail: fmt.Sprintf("writer fields %q and %q both resolve to reader field %q (via name + alias collision); rename the writer or drop the alias to disambiguate",
					truncForError(matchedBy[ri]), truncForError(wf.name), truncForError(r.fields[ri].name)),
			}
		}
		matched[ri] = true
		matchedBy[ri] = wf.name
		fn, fieldIdentical, err := c.compile(r.fields[ri].node, wf.node, fieldPath(path, r.fields[ri].name))
		if err != nil {
			return nil, false, err
		}
		// A matched field only forces buffering when it sits at or behind
		// a reader slot an earlier matched field already took; a higher
		// slot with default-only gaps in between streams, the defaults
		// being inserted in compileRecordStream's slot loop.
		if ri <= lastMatched {
			streamable = false
		}
		lastMatched = ri
		wire = append(wire, recWireField{readerIdx: ri, span: wspan, fn: fn, name: wf.name, identical: fieldIdentical})
	}

	// Pre-encode reader-only defaults once, at compile time.
	defaults := make([][]byte, len(r.fields))
	for i := range r.fields {
		if matched[i] {
			continue
		}
		rf := &r.fields[i]
		encoded, err := encodeDefault(nil, rf.defaultVal, rf.node)
		if err != nil {
			return nil, false, fmt.Errorf("field %s: %w", truncForError(fieldPath(path, rf.name)), err)
		}
		defaults[i] = encoded
	}

	if streamable {
		return c.compileRecordStream(r, wire, defaults), recordIsIdentity(r, wire), nil
	}

	// Buffered reordering is genuinely needed; a zero limit forbids it.
	if c.limit == 0 {
		return nil, false, fmt.Errorf("%s: record %s reorders fields but reorder buffering is disabled: %w",
			pathOrRoot(path), truncForError(r.name), ErrReorderBufferExceeded)
	}
	return c.compileRecordBuffered(r, wire, matched, defaults, path), false, nil
}

// recordIsIdentity reports whether the streaming plan is a pure copy: every
// writer field matched, no defaults interleave, and matched fields kept
// their ordinal. streamable guarantees the order here.
func recordIsIdentity(r *schemaNode, wire []recWireField) bool {
	if len(wire) != len(r.fields) {
		return false
	}
	for _, wf := range wire {
		if wf.readerIdx < 0 || !wf.identical {
			return false
		}
	}
	return true
}

// compileRecordStream emits a linear program: writer fields in wire order,
// with a reader default inserted wherever a reader slot has no preceding
// writer field. A matched field always sits one slot after the previous
// matched field, by the streamable test, so the slots stay ordered.
func (c *tCompile) compileRecordStream(r *schemaNode, wire []recWireField, defaults [][]byte) tfn {
	steps := make([]recStep, 0, len(wire)+len(r.fields))
	fieldNames := make([]string, 0, len(wire))

	slot := 0
	for _, wf := range wire {
		if wf.readerIdx < 0 {
			steps = append(steps, recStep{kind: stepSkip, span: wf.span, name: wf.name})
			continue
		}
		for slot < wf.readerIdx {
			steps = append(steps, recStep{kind: stepDefault, def: defaults[slot]})
			slot++
		}
		steps = append(steps, recStep{kind: stepField, fn: wf.fn})
		fieldNames = append(fieldNames, r.fields[wf.readerIdx].name)
		slot++
	}
	for slot < len(r.fields) {
		steps = append(steps, recStep{kind: stepDefault, def: defaults[slot]})
		slot++
	}

	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.enter(); err != nil {
			return nil, readFail(err)
		}
		defer tc.leave()
		fi := 0
		for _, st := range steps {
			switch st.kind {
			case stepSkip:
				rest, err := st.span(src, tc)
				if err != nil {
					return nil, wrapPath(readFail(err), st.name)
				}
				src = rest
			case stepField:
				rest, err := st.fn(src, tc)
				if err != nil {
					return nil, wrapPath(err, fieldNames[fi])
				}
				fi++
				src = rest
			case stepDefault:
				if err := tc.sink.emit(st.def); err != nil {
					return nil, writeFail(err)
				}
			}
		}
		return src, nil
	}
}

// compileRecordBuffered emits reader slots in order. A matched field whose
// reader slot is due now is transcoded directly; one whose slot comes later
// is captured into the frame after transcoding, and flushed in its slot.
// Defaults fill every gap at the end (and while flushing).
func (c *tCompile) compileRecordBuffered(r *schemaNode, wire []recWireField, matched []bool, defaults [][]byte, path string) tfn {
	nf := len(r.fields)
	limit := c.limit
	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.enter(); err != nil {
			return nil, readFail(err)
		}
		defer tc.leave()
		frame := tc.frame(nf)
		defer tc.release(frame)
		next := 0
		buffered := 0

		for _, wf := range wire {
			if wf.readerIdx < 0 {
				rest, err := wf.span(src, tc)
				if err != nil {
					return nil, wrapPath(readFail(err), wf.name)
				}
				src = rest
				continue
			}
			ri := wf.readerIdx
			if ri < next {
				// Should be impossible given one claim per reader index and
				// monotone flushing; keep the guard for clarity.
				return nil, readFail(fmt.Errorf("internal: record field %s emitted out of order", r.fields[ri].name))
			}
			if ri == next {
				rest, err := wf.fn(src, tc)
				if err != nil {
					return nil, wrapPath(err, r.fields[ri].name)
				}
				src = rest
				next++
				// Flush the direct chain now buffered behind it.
				for next < nf && frame.pending[next] != nil {
					if err := tc.sink.emit(frame.pending[next]); err != nil {
						return nil, writeFail(err)
					}
					buffered -= len(frame.pending[next])
					frame.pending[next] = nil
					next++
				}
				continue
			}
			// ri > next: transcode the field into a private buffer and park it.
			sub := &tctx{depth: tc.depth, limit: limit, sink: &bufSink{}}
			rest, err := wf.fn(src, sub)
			if err != nil {
				return nil, wrapPath(err, r.fields[ri].name)
			}
			src = rest
			captured := sub.sink.(*bufSink).b
			buffered += len(captured)
			if limit > 0 && buffered > limit {
				err := reorderFail(fmt.Errorf("field %s needs %d buffered bytes (%d held, limit %d): %w",
					truncForError(r.fields[ri].name), len(captured), buffered, limit, ErrReorderBufferExceeded))
				return nil, wrapPath(err, r.fields[ri].name)
			}
			frame.pending[ri] = captured
		}

		// Emit remaining reader slots in order: a buffered value where one
		// exists, otherwise the field default.
		for next < nf {
			if b := frame.pending[next]; b != nil {
				if err := tc.sink.emit(b); err != nil {
					return nil, writeFail(err)
				}
			} else if d := defaults[next]; d != nil {
				if err := tc.sink.emit(d); err != nil {
					return nil, writeFail(err)
				}
			}
			next++
		}
		return src, nil
	}
}

// compileWriterUnion maps every writer branch onto the single reader node.
// The writer's branch index is consumed and not rewritten (the reader is not
// a union), mirroring resolveWriterUnion.
func (c *tCompile) compileWriterUnion(r, w *schemaNode, path string) (tfn, error) {
	branchFns := make([]tfn, len(w.branches))
	for i, wb := range w.branches {
		fn, _, err := c.compile(r, wb, path)
		if err != nil {
			return nil, err
		}
		branchFns[i] = fn
	}
	return func(src []byte, tc *tctx) ([]byte, error) {
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, readFail(err)
		}
		if idx < 0 || int(idx) >= len(branchFns) {
			return nil, readFail(fmt.Errorf("union index %d out of range [0, %d)", idx, len(branchFns)))
		}
		return branchFns[idx](rest, tc)
	}, nil
}

// compileReaderUnion maps a non-union writer onto its one reader branch and
// emits that branch's index ahead of the value, mirroring
// resolveReaderUnion's first-match rule.
func (c *tCompile) compileReaderUnion(r, w *schemaNode, path string) (tfn, error) {
	rb := newReaderBranchLookup(r).match(w)
	if rb == nil {
		return nil, &CompatibilityError{
			Path:       pathOrRoot(path),
			ReaderType: "union",
			WriterType: w.kind,
			Detail:     detailWriterTypeNoReaderBranch,
		}
	}
	idx := branchIndex(r, rb)
	if idx < 0 {
		return nil, fmt.Errorf("avro: internal: matched reader branch not present in union")
	}
	fn, _, err := c.compile(rb, w, path)
	if err != nil {
		return nil, err
	}
	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.sink.emit(appendVarint(nil, int32(idx))); err != nil {
			return nil, writeFail(err)
		}
		return fn(src, tc)
	}, nil
}

// compileUnionUnion maps each writer branch to its best reader branch and
// rewrites the branch index. When both unions line up branch-for-branch and
// every branch is a wire copy, the union passes through untouched.
func (c *tCompile) compileUnionUnion(r, w *schemaNode, path string) (tfn, bool, error) {
	readerBranches := newReaderBranchLookup(r)
	branchFns := make([]tfn, len(w.branches))
	branchIdx := make([]int, len(w.branches))
	identity := len(w.branches) == len(r.branches)
	for i, wb := range w.branches {
		rb := readerBranches.match(wb)
		if rb == nil {
			return nil, false, &CompatibilityError{
				Path:       pathOrRoot(path),
				ReaderType: "union",
				WriterType: fmt.Sprintf("union[%d]:%s", i, wb.kind),
				Detail:     detailWriterBranchNoReaderBranch,
			}
		}
		ri := branchIndex(r, rb)
		branchIdx[i] = ri
		if ri != i {
			identity = false
		}
		fn, identical, err := c.compile(rb, wb, path)
		if err != nil {
			return nil, false, err
		}
		if !identical {
			identity = false
		}
		branchFns[i] = fn
	}
	if identity {
		return spanTfn(buildSpan(w)), true, nil
	}
	return func(src []byte, tc *tctx) ([]byte, error) {
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, readFail(err)
		}
		if idx < 0 || int(idx) >= len(branchFns) {
			return nil, readFail(fmt.Errorf("union index %d out of range [0, %d)", idx, len(branchFns)))
		}
		if err := tc.sink.emit(appendVarint(nil, int32(branchIdx[idx]))); err != nil {
			return nil, writeFail(err)
		}
		return branchFns[idx](rest, tc)
	}, false, nil
}

// branchIndex returns rb's position in r.branches, or -1 if absent.
func branchIndex(r, rb *schemaNode) int {
	for i := range r.branches {
		if r.branches[i] == rb {
			return i
		}
	}
	return -1
}

// ---------- numeric promotion leaves ----------

func transcodeIntLong(src []byte, tc *tctx) ([]byte, error) {
	n, rest, err := readVarint(src)
	if err != nil {
		return nil, readFail(err)
	}
	if err := tc.sink.emit(appendVarlong(nil, int64(n))); err != nil {
		return nil, writeFail(err)
	}
	return rest, nil
}

func transcodeIntFloat(src []byte, tc *tctx) ([]byte, error) {
	n, rest, err := readVarint(src)
	if err != nil {
		return nil, readFail(err)
	}
	if err := tc.sink.emit(appendUint32(nil, math.Float32bits(float32(n)))); err != nil {
		return nil, writeFail(err)
	}
	return rest, nil
}

func transcodeIntDouble(src []byte, tc *tctx) ([]byte, error) {
	n, rest, err := readVarint(src)
	if err != nil {
		return nil, readFail(err)
	}
	if err := tc.sink.emit(appendUint64(nil, math.Float64bits(float64(n)))); err != nil {
		return nil, writeFail(err)
	}
	return rest, nil
}

func transcodeLongFloat(src []byte, tc *tctx) ([]byte, error) {
	n, rest, err := readVarlong(src)
	if err != nil {
		return nil, readFail(err)
	}
	if err := tc.sink.emit(appendUint32(nil, math.Float32bits(float32(n)))); err != nil {
		return nil, writeFail(err)
	}
	return rest, nil
}

func transcodeLongDouble(src []byte, tc *tctx) ([]byte, error) {
	n, rest, err := readVarlong(src)
	if err != nil {
		return nil, readFail(err)
	}
	if err := tc.sink.emit(appendUint64(nil, math.Float64bits(float64(n)))); err != nil {
		return nil, writeFail(err)
	}
	return rest, nil
}

func transcodeFloatDouble(src []byte, tc *tctx) ([]byte, error) {
	u, rest, err := readUint32(src)
	if err != nil {
		return nil, readFail(err)
	}
	if err := tc.sink.emit(appendUint64(nil, math.Float64bits(float64(math.Float32frombits(u))))); err != nil {
		return nil, writeFail(err)
	}
	return rest, nil
}

// ---------- raw spans (zero-copy) ----------

// sfn advances past exactly one writer value without decoding it, threading
// the same recursion-depth guard the tfn walkers use. It is the transcode
// side's analogue of skipfn, written independently because it bounds itself
// with the input length alone and needs no per-schema min-bytes walk.
type sfn func(src []byte, tc *tctx) ([]byte, error)

// spanTfn adapts a span into a tfn that emits the spanned bytes verbatim.
// A single slicing expression copies the whole value straight from input to
// the sink: no parse, no intermediate value.
func spanTfn(span sfn) tfn {
	return func(src []byte, tc *tctx) ([]byte, error) {
		rest, err := span(src, tc)
		if err != nil {
			return nil, readFail(err)
		}
		if err := tc.sink.emit(src[:len(src)-len(rest)]); err != nil {
			return nil, writeFail(err)
		}
		return rest, nil
	}
}

func buildSpan(n *schemaNode) sfn {
	if n == nil {
		return func(_ []byte, _ *tctx) ([]byte, error) {
			return nil, errors.New("avro: internal: cannot span nil schema node")
		}
	}
	switch n.kind {
	case "null":
		return func(src []byte, _ *tctx) ([]byte, error) { return src, nil }
	case "boolean":
		return func(src []byte, _ *tctx) ([]byte, error) { return skipBytesN(src, 1, "boolean") }
	case "int", "enum":
		return func(src []byte, _ *tctx) ([]byte, error) {
			_, rest, err := readVarint(src)
			return rest, err
		}
	case "long":
		return func(src []byte, _ *tctx) ([]byte, error) {
			_, rest, err := readVarlong(src)
			return rest, err
		}
	case "float":
		return func(src []byte, _ *tctx) ([]byte, error) { return skipBytesN(src, 4, "float") }
	case "double":
		return func(src []byte, _ *tctx) ([]byte, error) { return skipBytesN(src, 8, "double") }
	case "string", "bytes":
		return func(src []byte, _ *tctx) ([]byte, error) {
			length, rest, err := readVarlong(src)
			if err != nil {
				return nil, err
			}
			if length < 0 {
				return nil, fmt.Errorf("invalid negative %s length %d", n.kind, length)
			}
			return skipBytesN(rest, int(length), n.kind)
		}
	case "fixed":
		return func(src []byte, _ *tctx) ([]byte, error) { return skipBytesN(src, n.size, "fixed") }
	case "record":
		return buildRecordSpan(n)
	case "array":
		return buildContainerSpan(n.items, false)
	case "map":
		return buildContainerSpan(n.values, true)
	case "union":
		return buildUnionSpan(n)
	default:
		return func(_ []byte, _ *tctx) ([]byte, error) {
			return nil, fmt.Errorf("cannot span unknown type %q", n.kind)
		}
	}
}

// buildRecordSpan compiles field spans lazily on first use, like
// skipRecordFields, so a schema not yet finalized at construction still
// spans correctly.
type recordSpan struct {
	node   *schemaNode
	once   sync.Once
	fields []sfn
}

func buildRecordSpan(n *schemaNode) sfn {
	rs := &recordSpan{node: n}
	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.enter(); err != nil {
			return nil, err
		}
		defer tc.leave()
		rs.once.Do(func() {
			rs.fields = make([]sfn, len(rs.node.fields))
			for i := range rs.node.fields {
				rs.fields[i] = buildSpan(rs.node.fields[i].node)
			}
		})
		var err error
		for _, f := range rs.fields {
			if src, err = f(src, tc); err != nil {
				return nil, err
			}
		}
		return src, nil
	}
}

// buildContainerSpan walks array (keys=false) or map (keys=true) blocks
// without decoding values. Negative-count framing fast-skips the whole
// block by its advertised byte size; positive blocks run the value span per
// item (and the key span per map entry). Block counts are bounded exactly as
// the skip path bounds them.
func buildContainerSpan(value *schemaNode, keys bool) sfn {
	valueSpan := buildSpan(value)
	keySpan := buildSpan(&schemaNode{kind: "string"})
	blockType := "array"
	if keys {
		blockType = "map"
	}
	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.enter(); err != nil {
			return nil, err
		}
		defer tc.leave()
		var total int64
		for {
			count, byteSize, rest, end, err := readBlockHeader(src, blockType, true)
			if err != nil {
				return nil, err
			}
			src = rest
			if end {
				return src, nil
			}
			if byteSize > 0 {
				// Negative-count framing: the block body is one indivisible
				// run we copy as-is.
				src = src[byteSize:]
				continue
			}
			if err := guardBlockCount(count, total, len(src), keys); err != nil {
				return nil, err
			}
			total += count
			for i := int64(0); i < count; i++ {
				if keys {
					if src, err = keySpan(src, tc); err != nil {
						return nil, err
					}
				}
				if src, err = valueSpan(src, tc); err != nil {
					return nil, err
				}
			}
		}
	}
}

func buildUnionSpan(n *schemaNode) sfn {
	branchSpans := make([]sfn, len(n.branches))
	for i, br := range n.branches {
		branchSpans[i] = buildSpan(br)
	}
	return func(src []byte, tc *tctx) ([]byte, error) {
		if err := tc.enter(); err != nil {
			return nil, err
		}
		defer tc.leave()
		idx, rest, err := readVarint(src)
		if err != nil {
			return nil, err
		}
		if idx < 0 || int(idx) >= len(branchSpans) {
			return nil, fmt.Errorf("union index %d out of range [0, %d)", idx, len(branchSpans))
		}
		return branchSpans[idx](rest, tc)
	}
}
