package msgpack

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

// ColKind identifies the Go type of a column requested from a batch of
// MessagePack records.
type ColKind int

const (
	// ColBool decodes true/false values into []bool.
	ColBool ColKind = iota + 1
	// ColInt decodes any msgpack signed/unsigned integer into []int64.
	ColInt
	// ColUint decodes any msgpack signed/unsigned integer into []uint64.
	ColUint
	// ColFloat decodes float32/float64 (and numeric) values into []float64.
	ColFloat
	// ColString decodes msgpack strings into []string.
	ColString
	// ColBytes decodes msgpack bin values into [][]byte.
	ColBytes
	// ColTime decodes the msgpack timestamp ext type into []time.Time.
	ColTime
)

// String returns the name of the column kind.
func (k ColKind) String() string {
	switch k {
	case ColBool:
		return "bool"
	case ColInt:
		return "int64"
	case ColUint:
		return "uint64"
	case ColFloat:
		return "float64"
	case ColString:
		return "string"
	case ColBytes:
		return "[]byte"
	case ColTime:
		return "time.Time"
	default:
		return fmt.Sprintf("ColKind(%d)", int(k))
	}
}

// ColSpec declares a single column: the dotted path inside each record and the
// type the value is expected to decode into.
//
// Path segments are map keys separated by dots (e.g. "user.address.city");
// segments that consist of digits address an array index (e.g. "items.0.id").
//
// Dst must be a non-nil pointer to the slice matching Kind, for example
// *[]int64 for ColInt. Reusing a non-empty slice resets it before decoding
// while keeping its backing capacity.
//
// Set TrackNull to also populate a Null bitmap: Null[i] is true when the
// selected path exists but the value is an explicit msgpack nil. The Present
// bitmap is always reported and distinguishes an absent path (Present=false)
// from an explicit nil (Present=true, Null=true).
type ColSpec struct {
	Path      string
	Kind      ColKind
	Dst       interface{}
	TrackNull bool
}

// ColumnErrorPolicy chooses what happens when a record cannot be projected into
// the requested columns, for example because it is not a map or a selected
// value has an incompatible type.
type ColumnErrorPolicy int

const (
	// StopOnBadRow aborts decoding at the first bad record and returns it.
	// Columns already contain every good record decoded before it.
	StopOnBadRow ColumnErrorPolicy = iota
	// SkipBadRows records bad rows in ColumnResult.BadRows and continues.
	// Every bad row still occupies exactly one aligned slot in every column.
	SkipBadRows
)

// ColumnOptions controls a batch columnar decode.
type ColumnOptions struct {
	// ErrorPolicy chooses between stopping at and skipping bad records.
	ErrorPolicy ColumnErrorPolicy
	// MaxRows caps the number of records consumed. Zero means no limit.
	// When the cap is reached the stream is left positioned exactly at the
	// first record not consumed; decode more with another DecodeColumns call.
	MaxRows int
	// RowsHint preallocates the output slices when the row count is known.
	RowsHint int
}

// BadRow describes one record that was framed successfully but could not be
// projected into the requested columns.
type BadRow struct {
	// Index is the zero-based ordinal of the record within this batch,
	// counting good and bad records.
	Index int
	// Offset is the byte offset of the record start relative to the stream.
	Offset int
	// Err is the projection error.
	Err error
}

func (b *BadRow) Error() string {
	return fmt.Sprintf("msgpack: bad row %d at byte %d: %v", b.Index, b.Offset, b.Err)
}

func (b *BadRow) Unwrap() error { return b.Err }

// FrameError is returned when the stream cannot even be framed as complete
// MessagePack values. MessagePack is not self-synchronizing, so decoding can
// not resume automatically.
type FrameError struct {
	// Offset is the byte offset at which framing failed. Records starting
	// before it were decoded completely.
	Offset int
	Err    error
}

func (e *FrameError) Error() string {
	return fmt.Sprintf("msgpack: framing error at byte %d: %v", e.Offset, e.Err)
}

func (e *FrameError) Unwrap() error { return e.Err }

// ColumnResult holds the typed arrays and validity bitmaps produced by a batch
// decode. The Dst slices passed in ColSpec and the corresponding Values slices
// here are the same underlying slice.
type ColumnResult struct {
	Columns []Column
	// Rows is the number of records consumed (good and bad). Every column,
	// Present and Null bitmap has exactly this length.
	Rows int
	// BadRows lists records skipped under SkipBadRows.
	BadRows []BadRow
	// ResumeOffset is the stream byte offset of the first record not
	// consumed. On success it points just past the last decoded record (and at
	// EOF it equals the total stream length consumed). When a FrameError is
	// returned it points at the start of the record that could not be framed;
	// truncating the stream to that offset and feeding the remainder to a
	// fresh decoder resumes decoding once the caller knows where the damaged
	// value ends (MessagePack carries no resync marker).
	ResumeOffset int
	// Truncated is true when MaxRows stopped decoding before the stream ended.
	Truncated bool
}

// Column is the decoded output for one ColSpec.
type Column struct {
	Path    string
	Kind    ColKind
	Values  interface{}
	Present []bool
	Null    []bool
}

// bytesReader is a zero-allocation io.Reader + io.ByteScanner over a byte
// slice, used to project each framed record without creating a new decoder.
type bytesReader struct {
	b []byte
	i int
}

func (r *bytesReader) reset(b []byte) {
	r.b = b
	r.i = 0
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func (r *bytesReader) ReadByte() (byte, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.i]
	r.i++
	return c, nil
}

func (r *bytesReader) UnreadByte() error {
	if r.i <= 0 {
		return errors.New("msgpack: bytesReader: invalid UnreadByte")
	}
	r.i--
	return nil
}

// frameReader wraps the outer stream and tees every consumed byte into a frame
// buffer while the decoder structurally walks one top-level value. It supports
// the single-byte unread the decoder uses around map length and time codes.
type frameReader struct {
	r      io.Reader
	s      io.ByteScanner
	frame  *[]byte
	unread bool
}

func (f *frameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.unread {
		// The unread byte is the last one appended; callers always read it
		// back immediately, so re-emit that byte without re-recording.
		full := *f.frame
		p[0] = full[len(full)-1]
		f.unread = false
		if len(p) == 1 {
			return 1, nil
		}
		n, err := f.r.Read(p[1:])
		*f.frame = append(*f.frame, p[1:1+n]...)
		return n + 1, err
	}
	n, err := f.r.Read(p)
	*f.frame = append(*f.frame, p[:n]...)
	return n, err
}

func (f *frameReader) ReadByte() (byte, error) {
	if f.unread {
		f.unread = false
		full := *f.frame
		return full[len(full)-1], nil
	}
	c, err := f.s.ReadByte()
	if err != nil {
		return 0, err
	}
	*f.frame = append(*f.frame, c)
	return c, nil
}

func (f *frameReader) UnreadByte() error {
	if f.unread {
		return errors.New("msgpack: frameReader: repeated UnreadByte")
	}
	f.unread = true
	return f.s.UnreadByte()
}

type column struct {
	path      []string
	kind      ColKind
	present   []bool
	null      []bool
	trackNull bool

	bools  []bool
	ints   []int64
	uints  []uint64
	floats []float64
	strs   []string
	bytes  [][]byte
	times  []time.Time
}

// prepareColumn validates a ColSpec and turns it into an internal column.
func prepareColumn(spec ColSpec, hint int) (*column, error) {
	if strings.TrimSpace(spec.Path) == "" {
		return nil, errors.New("msgpack: column path must not be empty")
	}
	if spec.Dst == nil {
		return nil, fmt.Errorf("msgpack: column %q: Dst is nil", spec.Path)
	}

	col := &column{
		path:      strings.Split(spec.Path, "."),
		kind:      spec.Kind,
		trackNull: spec.TrackNull,
	}

	switch spec.Kind {
	case ColBool:
		dst, ok := spec.Dst.(*[]bool)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[]bool", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([]bool, 0, hint)
		}
		col.bools = (*dst)[:0]
	case ColInt:
		dst, ok := spec.Dst.(*[]int64)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[]int64", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([]int64, 0, hint)
		}
		col.ints = (*dst)[:0]
	case ColUint:
		dst, ok := spec.Dst.(*[]uint64)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[]uint64", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([]uint64, 0, hint)
		}
		col.uints = (*dst)[:0]
	case ColFloat:
		dst, ok := spec.Dst.(*[]float64)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[]float64", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([]float64, 0, hint)
		}
		col.floats = (*dst)[:0]
	case ColString:
		dst, ok := spec.Dst.(*[]string)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[]string", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([]string, 0, hint)
		}
		col.strs = (*dst)[:0]
	case ColBytes:
		dst, ok := spec.Dst.(*[][]byte)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[][]byte", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([][]byte, 0, hint)
		}
		col.bytes = (*dst)[:0]
	case ColTime:
		dst, ok := spec.Dst.(*[]time.Time)
		if !ok {
			return nil, fmt.Errorf("msgpack: column %q: Kind %s requires *[]time.Time", spec.Path, spec.Kind)
		}
		if *dst == nil {
			*dst = make([]time.Time, 0, hint)
		}
		col.times = (*dst)[:0]
	default:
		return nil, fmt.Errorf("msgpack: column %q: unsupported kind %d", spec.Path, spec.Kind)
	}

	col.present = make([]bool, 0, hint)
	if spec.TrackNull {
		col.null = make([]bool, 0, hint)
	}

	return col, nil
}

// DecodeColumns decodes a stream of MessagePack records directly into typed
// columnar arrays, without materializing each record as a map first.
//
// Each top-level value in the stream is treated as one record (typically a
// map). Only the keys referenced by specs are inspected; every other value is
// skipped structurally. All columns stay aligned: each record appends exactly
// one slot per column, including bad rows.
func (d *Decoder) DecodeColumns(specs []ColSpec, opts *ColumnOptions) (*ColumnResult, error) {
	if len(specs) == 0 {
		return nil, errors.New("msgpack: DecodeColumns requires at least one column")
	}

	hint := 0
	maxRows := 0
	policy := StopOnBadRow
	if opts != nil {
		hint = opts.RowsHint
		maxRows = opts.MaxRows
		policy = opts.ErrorPolicy
	}
	if hint < 0 {
		hint = 0
	}
	if hint > 0 && maxRows > 0 && hint > maxRows {
		hint = maxRows
	}

	cols := make([]*column, len(specs))
	seen := make(map[string]bool, len(specs))
	for i, spec := range specs {
		if seen[spec.Path] {
			return nil, fmt.Errorf("msgpack: duplicate column path %q", spec.Path)
		}
		seen[spec.Path] = true
		col, err := prepareColumn(spec, hint)
		if err != nil {
			return nil, err
		}
		cols[i] = col
	}

	res := &ColumnResult{
		Columns: make([]Column, len(cols)),
	}

	// Save the outer stream. Each record is framed through a tee reader and
	// then projected from the captured bytes, reusing this decoder (no
	// per-record decoder allocation).
	outerR, outerS := d.r, d.s
	outerRec := d.rec
	defer func() {
		d.r, d.s = outerR, outerS
		d.rec = outerRec
	}()

	var frame []byte
	offset := 0
	for {
		if maxRows > 0 && res.Rows >= maxRows {
			res.Truncated = true
			break
		}

		// Tee one structurally complete top-level value into frame.
		frame = frame[:0]
		d.fr = frameReader{r: outerR, s: outerS, frame: &frame}
		d.r, d.s = &d.fr, &d.fr
		d.rec = nil
		err := d.Skip()
		if err != nil {
			d.r, d.s = outerR, outerS
			if errors.Is(err, io.EOF) && res.Rows > 0 {
				break
			}
			if errors.Is(err, io.EOF) {
				res.finalizeColumns(specs, cols)
				return res, io.EOF
			}
			res.ResumeOffset = offset
			res.finalizeColumns(specs, cols)
			return res, &FrameError{Offset: offset, Err: err}
		}

		// Project the captured frame into columns.
		rowErr := d.projectRecord(frame, cols)
		d.r, d.s = outerR, outerS

		// The frame is complete: it counts as one row for every column,
		// regardless of whether projection succeeds, so columns never shift.
		offset += len(frame)
		res.Rows++

		if rowErr != nil {
			if policy == StopOnBadRow {
				res.ResumeOffset = offset
				bad := &BadRow{
					Index:  res.Rows - 1,
					Offset: offset - len(frame),
					Err:    rowErr,
				}
				res.finalizeColumns(specs, cols)
				return res, bad
			}
			res.BadRows = append(res.BadRows, BadRow{
				Index:  res.Rows - 1,
				Offset: offset - len(frame),
				Err:    rowErr,
			})
		}
	}

	res.ResumeOffset = offset

	res.finalizeColumns(specs, cols)

	return res, nil
}

// finalizeColumns publishes typed slices back to caller Dsts and to result.
func (res *ColumnResult) finalizeColumns(specs []ColSpec, cols []*column) {
	for i, col := range cols {
		switch col.kind {
		case ColBool:
			*specs[i].Dst.(*[]bool) = col.bools
		case ColInt:
			*specs[i].Dst.(*[]int64) = col.ints
		case ColUint:
			*specs[i].Dst.(*[]uint64) = col.uints
		case ColFloat:
			*specs[i].Dst.(*[]float64) = col.floats
		case ColString:
			*specs[i].Dst.(*[]string) = col.strs
		case ColBytes:
			*specs[i].Dst.(*[][]byte) = col.bytes
		case ColTime:
			*specs[i].Dst.(*[]time.Time) = col.times
		}
		res.Columns[i] = col.publicColumn(specs[i])
	}
}

// projectRecord appends one aligned slot to every column from the record bytes.
func (d *Decoder) projectRecord(frame []byte, cols []*column) error {
	for _, col := range cols {
		col.present = append(col.present, false)
		if col.trackNull {
			col.null = append(col.null, false)
		}
		col.appendZero()
	}

	d.br.reset(frame)
	d.r, d.s = &d.br, &d.br
	d.rec = nil

	code, err := d.readCode()
	if err != nil {
		return err
	}

	if code == msgpcode.Nil {
		return errors.New("msgpack: record is nil, expected a map")
	}

	if !isMapCode(code) {
		// Drain the (small) value so stream state is irrelevant afterwards.
		_ = d.skipValueCode(code)
		return fmt.Errorf("msgpack: record is %s, expected a map", codeKindName(code))
	}

	n, err := d.mapLen(code)
	if err != nil {
		return err
	}

	return d.projectMap(n, cols, 0)
}

// maxActiveColumns bounds the stack buffers used while descending. Selecting
// more columns than this falls back to a heap-allocated subset (rare).
const maxActiveColumns = 16

// match holds dense indices (into the enclosing subset) of the columns
// selected by one map key or array index. The fixed array stays on the stack.
type match struct {
	idx   [maxActiveColumns]int
	count int
	heap  []int
}

// add records one matched column index.
func (m *match) add(i int) {
	if m.count < len(m.idx) {
		m.idx[m.count] = i
		m.count++
		return
	}
	m.heap = append(m.heap, i)
	m.count++
}

// at returns the matched index at position k.
func (m *match) at(k int) int {
	if k < len(m.idx) {
		return m.idx[k]
	}
	return m.heap[k-len(m.idx)]
}

// matchColumns collects columns whose path segment at depth equals key.
// Selected-column counts are small, so a linear scan beats a hash map and
// allocates nothing in the common case.
func matchColumns(cols []*column, depth int, key string) match {
	var m match
	for ci, col := range cols {
		if depth < len(col.path) && col.path[depth] == key {
			m.add(ci)
		}
	}
	return m
}

// projectMap walks n map entries, decoding only values matched by columns.
func (d *Decoder) projectMap(n int, cols []*column, depth int) error {
	var firstErr error
	for i := 0; i < n; i++ {
		key, matched, err := d.readMapKeyString()
		if err != nil {
			return err
		}
		m := matchColumns(cols, depth, key)
		if !matched || m.count == 0 {
			if err := d.Skip(); err != nil {
				return err
			}
			continue
		}
		if err := d.consumeMatched(&m, cols, depth, &firstErr); err != nil {
			return err
		}
	}
	return firstErr
}

// projectArray walks array elements addressed by numeric path segments.
func (d *Decoder) projectArray(n int, cols []*column, depth int) error {
	var firstErr error
	for i := 0; i < n; i++ {
		m := matchColumns(cols, depth, strconv.Itoa(i))
		if m.count == 0 {
			if err := d.Skip(); err != nil {
				return err
			}
			continue
		}
		if err := d.consumeMatched(&m, cols, depth, &firstErr); err != nil {
			return err
		}
	}
	return firstErr
}

// consumeMatched reads exactly one value shared by every matched column and
// dispatches it to leaf decoders or into nested traversal. The value is
// consumed exactly once, so the enclosing map/array never loses alignment.
func (d *Decoder) consumeMatched(m *match, cols []*column, depth int, firstErr *error) error {
	code, err := d.readCode()
	if err != nil {
		return err
	}

	row := len(cols[0].present) - 1

	if code == msgpcode.Nil {
		// Explicit nil: leaf columns become Present+Null; paths that continue
		// deeper simply end here and stay absent. The nil is fully consumed.
		for k := 0; k < m.count; k++ {
			col := cols[m.at(k)]
			if depth == len(col.path)-1 {
				col.markNull(row)
			}
		}
		return nil
	}

	if isMapCode(code) || isArrayCode(code) {
		return d.descendMatched(code, m, cols, depth)
	}

	// Scalar: only leaf columns can accept it; branch columns stay absent.
	consumed := false
	for k := 0; k < m.count; k++ {
		col := cols[m.at(k)]
		if depth != len(col.path)-1 {
			continue
		}
		if !col.kind.acceptsCode(code) {
			if *firstErr == nil {
				*firstErr = col.leafTypeMismatch(code)
			}
			continue
		}
		if consumed {
			// Defensive: two leaf columns sharing one key (duplicate paths
			// are rejected up front, so this is normally unreachable). Re-read
			// the same in-memory value for the second leaf.
			if err := d.s.UnreadByte(); err != nil {
				return err
			}
			if err := col.decodeLeaf(d, row); err != nil && *firstErr == nil {
				*firstErr = err
			}
			continue
		}
		if err := col.decodeLeafCode(d, code, row); err != nil && *firstErr == nil {
			*firstErr = err
		}
		consumed = true
	}
	if !consumed {
		// Mismatched scalar for every leaf (or pure branches): consume the
		// payload once to keep the enclosing container aligned.
		return d.skipValueCode(code)
	}
	return nil
}

// descendMatched continues into a map/array value already read, restricted to
// the matched columns that still have path segments below this node.
func (d *Decoder) descendMatched(code byte, m *match, cols []*column, depth int) error {
	var stack [maxActiveColumns]*column
	subset := stack[:0]
	for k := 0; k < m.count; k++ {
		col := cols[m.at(k)]
		if depth < len(col.path)-1 {
			if len(subset) < maxActiveColumns {
				subset = append(subset, col)
			} else {
				// Extremely wide fan-out: fall back to a heap slice.
				heapSubset := make([]*column, len(subset), m.count)
				copy(heapSubset, subset)
				for k2 := k; k2 < m.count; k2++ {
					c := cols[m.at(k2)]
					if depth < len(c.path)-1 {
						heapSubset = append(heapSubset, c)
					}
				}
				return d.descendInto(code, heapSubset, depth)
			}
		}
	}
	return d.descendInto(code, subset, depth)
}

// descendInto reads the length of an already-coded map/array and walks it with
// the given column subset. A nil/empty container contributes nothing.
func (d *Decoder) descendInto(code byte, subset []*column, depth int) error {
	if isMapCode(code) {
		n, err := d.mapLen(code)
		if err != nil {
			return err
		}
		if n < 0 {
			return nil
		}
		return d.projectMap(n, subset, depth+1)
	}
	n, err := d.arrayLen(code)
	if err != nil {
		return err
	}
	if n < 0 {
		return nil
	}
	return d.projectArray(n, subset, depth+1)
}

// readMapKeyString reads a map key. String keys return matched=true with their
// value; non-string keys can never match a dotted path, so the key value is
// consumed and matched=false is returned for the caller to skip the value.
func (d *Decoder) readMapKeyString() (key string, matched bool, err error) {
	c, err := d.readCode()
	if err != nil {
		return "", false, err
	}
	if !isStringCode(c) {
		if err := d.skipValueCode(c); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	n, err := d.bytesLen(c)
	if err != nil {
		return "", false, err
	}
	if n == -1 {
		return "", true, nil
	}
	// Keys are only used to match paths and the frame stays alive for the
	// whole projection, so a zero-copy bytes->string view avoids an
	// allocation per key (the same trick decodeStringTemp uses).
	if cap(d.buf) < n {
		d.buf = make([]byte, n)
	}
	buf := d.buf[:n]
	if err := d.readFull(buf); err != nil {
		return "", false, err
	}
	return bytesToString(buf), true, nil
}

// readColumnString reads a string value body of n bytes. Unlike keys, values
// are stored in the output, so they allocate their own immutable string.
func (d *Decoder) readColumnString(n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(d.r, b); err != nil {
		return "", err
	}
	return bytesToString(b), nil
}

func isMapCode(c byte) bool {
	return msgpcode.IsFixedMap(c) || c == msgpcode.Map16 || c == msgpcode.Map32
}

func isArrayCode(c byte) bool {
	return msgpcode.IsFixedArray(c) || c == msgpcode.Array16 || c == msgpcode.Array32
}

func isStringCode(c byte) bool {
	return msgpcode.IsFixedString(c) ||
		c == msgpcode.Str8 || c == msgpcode.Str16 || c == msgpcode.Str32
}

func codeKindName(c byte) string {
	switch {
	case c == msgpcode.Nil:
		return "nil"
	case c == msgpcode.False || c == msgpcode.True:
		return "bool"
	case msgpcode.IsFixedNum(c),
		c == msgpcode.Uint8, c == msgpcode.Uint16, c == msgpcode.Uint32, c == msgpcode.Uint64,
		c == msgpcode.Int8, c == msgpcode.Int16, c == msgpcode.Int32, c == msgpcode.Int64:
		return "integer"
	case c == msgpcode.Float || c == msgpcode.Double:
		return "float"
	case isStringCode(c):
		return "string"
	case msgpcode.IsBin(c):
		return "bytes"
	case isArrayCode(c):
		return "array"
	case msgpcode.IsExt(c):
		return "ext"
	default:
		return fmt.Sprintf("code %#x", c)
	}
}

// markNull records Present=true and, when requested, Null=true for the row.
func (c *column) markNull(row int) {
	c.present[row] = true
	if c.trackNull {
		c.ensureNull(row)
		c.null[row] = true
	}
}

// ensureNull lazily grows the Null bitmap, backfilling earlier rows false.
func (c *column) ensureNull(row int) {
	if c.null == nil {
		c.null = make([]bool, len(c.present))
	}
	for len(c.null) <= row {
		c.null = append(c.null, false)
	}
}

// markPresent records a successfully decoded value at row.
func (c *column) markPresent(row int) {
	c.present[row] = true
	if c.trackNull {
		c.ensureNull(row)
	}
}

// appendZero reserves the typed value slot for one record.
func (c *column) appendZero() {
	switch c.kind {
	case ColBool:
		c.bools = append(c.bools, false)
	case ColInt:
		c.ints = append(c.ints, 0)
	case ColUint:
		c.uints = append(c.uints, 0)
	case ColFloat:
		c.floats = append(c.floats, 0)
	case ColString:
		c.strs = append(c.strs, "")
	case ColBytes:
		c.bytes = append(c.bytes, nil)
	case ColTime:
		c.times = append(c.times, time.Time{})
	}
}

// leafTypeMismatch builds the error for an incompatible value code.
func (c *column) leafTypeMismatch(code byte) error {
	return fmt.Errorf("msgpack: column %q: cannot decode %s into %s",
		strings.Join(c.path, "."), codeKindName(code), c.kind)
}

// acceptsCode reports whether a value with the given code can decode into
// this column's Go type.
func (k ColKind) acceptsCode(c byte) bool {
	if c == msgpcode.Nil {
		return true
	}
	isInt := msgpcode.IsFixedNum(c) ||
		c == msgpcode.Uint8 || c == msgpcode.Uint16 || c == msgpcode.Uint32 || c == msgpcode.Uint64 ||
		c == msgpcode.Int8 || c == msgpcode.Int16 || c == msgpcode.Int32 || c == msgpcode.Int64
	switch k {
	case ColBool:
		return c == msgpcode.False || c == msgpcode.True
	case ColInt, ColUint:
		return isInt
	case ColFloat:
		return isInt || c == msgpcode.Float || c == msgpcode.Double
	case ColString:
		return isStringCode(c)
	case ColBytes:
		return msgpcode.IsBin(c)
	case ColTime:
		return msgpcode.IsExt(c) || isStringCode(c) ||
			c == msgpcode.FixedArrayLow|2
	default:
		return false
	}
}

// decodeLeaf reads and decodes one value into the typed slice at row.
func (c *column) decodeLeaf(d *Decoder, row int) error {
	code, err := d.readCode()
	if err != nil {
		return err
	}
	return c.decodeLeafCode(d, code, row)
}

// decodeLeafCode decodes the value starting with an already read code.
func (c *column) decodeLeafCode(d *Decoder, code byte, row int) error {
	if code == msgpcode.Nil {
		c.markNull(row)
		return nil
	}

	switch c.kind {
	case ColBool:
		v, err := d.bool(code)
		if err != nil {
			return err
		}
		c.bools[row] = v
	case ColInt:
		v, err := d.int(code)
		if err != nil {
			return err
		}
		c.ints[row] = v
	case ColUint:
		v, err := d.uint(code)
		if err != nil {
			return err
		}
		c.uints[row] = v
	case ColFloat:
		v, err := d.float64(code)
		if err != nil {
			return err
		}
		c.floats[row] = v
	case ColString:
		n, err := d.bytesLen(code)
		if err != nil {
			return err
		}
		v, err := d.readColumnString(n)
		if err != nil {
			return err
		}
		c.strs[row] = v
	case ColBytes:
		v, err := d.bytes(code, nil)
		if err != nil {
			return err
		}
		c.bytes[row] = v
	case ColTime:
		if err := d.s.UnreadByte(); err != nil {
			return err
		}
		v, err := d.DecodeTime()
		if err != nil {
			return err
		}
		c.times[row] = v
	default:
		return fmt.Errorf("msgpack: column %q: unsupported kind", strings.Join(c.path, "."))
	}

	c.markPresent(row)
	return nil
}

func (c *column) publicColumn(spec ColSpec) Column {
	out := Column{
		Path:    spec.Path,
		Kind:    spec.Kind,
		Present: c.present,
		Null:    c.null,
	}
	switch c.kind {
	case ColBool:
		out.Values = c.bools
	case ColInt:
		out.Values = c.ints
	case ColUint:
		out.Values = c.uints
	case ColFloat:
		out.Values = c.floats
	case ColString:
		out.Values = c.strs
	case ColBytes:
		out.Values = c.bytes
	case ColTime:
		out.Values = c.times
	}
	return out
}

// UnmarshalColumns decodes a buffer of concatenated MessagePack records into
// typed columns. It is a convenience wrapper around Decoder.DecodeColumns.
func UnmarshalColumns(data []byte, specs []ColSpec, opts *ColumnOptions) (*ColumnResult, error) {
	dec := GetDecoder()
	dec.Reset(&bytesReader{b: data})
	res, err := dec.DecodeColumns(specs, opts)
	PutDecoder(dec)
	return res, err
}
