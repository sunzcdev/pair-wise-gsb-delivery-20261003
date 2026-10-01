package msgpack

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

// ColumnKind declares the target type of a column.
type ColumnKind int

const (
	ColumnInt64 ColumnKind = iota
	ColumnUint64
	ColumnFloat64
	ColumnBool
	ColumnString
	ColumnBytes
)

func (k ColumnKind) String() string {
	switch k {
	case ColumnInt64:
		return "int64"
	case ColumnUint64:
		return "uint64"
	case ColumnFloat64:
		return "float64"
	case ColumnBool:
		return "bool"
	case ColumnString:
		return "string"
	case ColumnBytes:
		return "bytes"
	}
	return fmt.Sprintf("ColumnKind(%d)", int(k))
}

// ColumnSpec declares a single output column: the path of the field inside
// each record and the Go type it should be decoded into.
type ColumnSpec struct {
	// Path is a dot-separated sequence of map keys, e.g. "user.id".
	// Records are expected to be msgpack maps; nested segments navigate
	// into nested maps.
	Path string
	Kind ColumnKind
}

// ErrorPolicy controls how ColumnarDecoder reacts to a record that cannot
// be decoded.
type ErrorPolicy int

const (
	// FailFast stops decoding at the first bad record. Decode returns
	// a *RowError; the stream is positioned at the next record boundary,
	// so Decode may be called again to resume with the following record.
	FailFast ErrorPolicy = iota
	// CollectErrors records the bad record in BadRows, appends an
	// all-missing row to keep columns aligned, and continues decoding
	// with the next record.
	CollectErrors
)

// BadRow describes a record that failed to decode.
type BadRow struct {
	// Index is the zero-based record index within the decoded batch.
	Index int
	Err   error
}

// RowError is returned by ColumnarDecoder.Decode in FailFast mode when
// a record cannot be decoded.
type RowError struct {
	Index int
	Err   error
}

func (e *RowError) Error() string {
	return fmt.Sprintf("msgpack: columnar: record %d: %s", e.Index, e.Err)
}

func (e *RowError) Unwrap() error {
	return e.Err
}

//------------------------------------------------------------------------------

type bitmap []uint64

func (b *bitmap) set(i int) {
	for len(*b) <= i/64 {
		*b = append(*b, 0)
	}
	(*b)[i/64] |= 1 << (uint(i) % 64)
}

func (b bitmap) get(i int) bool {
	if i/64 >= len(b) {
		return false
	}
	return b[i/64]&(1<<(uint(i)%64)) != 0
}

// truncate drops all bits at positions >= rows.
func (b *bitmap) truncate(rows int) {
	words := (rows + 63) / 64
	if words < len(*b) {
		*b = (*b)[:words]
	}
	if rem := uint(rows) % 64; rem != 0 && words > 0 && words <= len(*b) {
		(*b)[words-1] &= (uint64(1) << rem) - 1
	}
}

//------------------------------------------------------------------------------

// Column holds the decoded values of a single field across a batch of
// records. Exactly one of the typed slices is non-nil, matching the
// declared ColumnKind. Every typed slice has one entry per decoded record,
// so columns never drift out of alignment, even when some records are bad.
//
// The validity of row i is derived from two bitmaps:
//   - Present(i) == false: the field was missing from the record.
//   - Present(i) && Null(i): the field was present but explicitly nil.
//   - Present(i) && !Null(i): the typed slice holds a usable value at i.
type Column struct {
	kind ColumnKind

	Ints    []int64
	Uints   []uint64
	Floats  []float64
	Bools   []bool
	Strings []string
	Bytes   [][]byte

	present bitmap
	null    bitmap
}

// Kind returns the declared target type of the column.
func (c *Column) Kind() ColumnKind {
	return c.kind
}

// Len returns the number of rows in the column.
func (c *Column) Len() int {
	switch c.kind {
	case ColumnInt64:
		return len(c.Ints)
	case ColumnUint64:
		return len(c.Uints)
	case ColumnFloat64:
		return len(c.Floats)
	case ColumnBool:
		return len(c.Bools)
	case ColumnString:
		return len(c.Strings)
	case ColumnBytes:
		return len(c.Bytes)
	}
	return 0
}

// Present reports whether the field was found in record i.
func (c *Column) Present(i int) bool {
	return c.present.get(i)
}

// Null reports whether the field was present but explicitly nil in record i.
func (c *Column) Null(i int) bool {
	return c.null.get(i)
}

// Valid reports whether record i holds a usable value (present and not nil).
func (c *Column) Valid(i int) bool {
	return c.present.get(i) && !c.null.get(i)
}

func (c *Column) extend() {
	switch c.kind {
	case ColumnInt64:
		c.Ints = append(c.Ints, 0)
	case ColumnUint64:
		c.Uints = append(c.Uints, 0)
	case ColumnFloat64:
		c.Floats = append(c.Floats, 0)
	case ColumnBool:
		c.Bools = append(c.Bools, false)
	case ColumnString:
		c.Strings = append(c.Strings, "")
	case ColumnBytes:
		c.Bytes = append(c.Bytes, nil)
	}
}

func (c *Column) truncate(rows int) {
	switch c.kind {
	case ColumnInt64:
		c.Ints = c.Ints[:rows]
	case ColumnUint64:
		c.Uints = c.Uints[:rows]
	case ColumnFloat64:
		c.Floats = c.Floats[:rows]
	case ColumnBool:
		c.Bools = c.Bools[:rows]
	case ColumnString:
		c.Strings = c.Strings[:rows]
	case ColumnBytes:
		c.Bytes = c.Bytes[:rows]
	}
	c.present.truncate(rows)
	c.null.truncate(rows)
}

//------------------------------------------------------------------------------

type pathNode struct {
	children map[string]*pathNode
	col      int // column index for leaf nodes, -1 otherwise
}

// typeMismatchError marks a record-level type mismatch: the record is bad,
// but the offending value was fully consumed, so the stream is still
// positioned at a value boundary and decoding can resume with the next
// record. Any other error is structural (corrupt stream or I/O failure)
// and aborts decoding.
type typeMismatchError struct {
	msg string
}

func (e *typeMismatchError) Error() string {
	return e.msg
}

// ColumnarDecoder decodes a stream of msgpack map records into typed
// columns. Only the declared fields are decoded; every other field is
// skipped without allocating, which makes it much cheaper than decoding
// each record into a map and transposing afterwards.
type ColumnarDecoder struct {
	dec    *Decoder
	root   *pathNode
	cols   []*Column
	policy ErrorPolicy

	rows    int
	badRows []BadRow
}

// NewColumnarDecoder returns a decoder that reads records from dec and
// extracts the fields described by specs into typed columns.
func NewColumnarDecoder(dec *Decoder, specs []ColumnSpec, policy ErrorPolicy) (*ColumnarDecoder, error) {
	if len(specs) == 0 {
		return nil, errors.New("msgpack: columnar: no columns declared")
	}

	cd := &ColumnarDecoder{
		dec:    dec,
		root:   &pathNode{col: -1},
		policy: policy,
	}

	for i, spec := range specs {
		if spec.Kind < ColumnInt64 || spec.Kind > ColumnBytes {
			return nil, fmt.Errorf("msgpack: columnar: column %q: unknown kind %d", spec.Path, spec.Kind)
		}

		segs := strings.Split(spec.Path, ".")
		node := cd.root
		for _, seg := range segs {
			if seg == "" {
				return nil, fmt.Errorf("msgpack: columnar: column %q: empty path segment", spec.Path)
			}
			if node.col >= 0 {
				return nil, fmt.Errorf("msgpack: columnar: column %q: conflicts with column %q", spec.Path, specs[node.col].Path)
			}
			child := node.children[seg]
			if child == nil {
				child = &pathNode{col: -1}
				if node.children == nil {
					node.children = make(map[string]*pathNode)
				}
				node.children[seg] = child
			}
			node = child
		}
		if node.col >= 0 || len(node.children) > 0 {
			return nil, fmt.Errorf("msgpack: columnar: duplicate or conflicting column %q", spec.Path)
		}
		node.col = i

		col := &Column{kind: spec.Kind}
		cd.cols = append(cd.cols, col)
	}

	return cd, nil
}

// Columns returns the output columns in the order the specs were declared.
// The slices are valid after Decode returns and grow on every call.
func (cd *ColumnarDecoder) Columns() []*Column {
	return cd.cols
}

// Rows returns the number of rows currently held by the columns.
func (cd *ColumnarDecoder) Rows() int {
	return cd.rows
}

// BadRows returns the records that failed to decode, in the order they
// were encountered.
func (cd *ColumnarDecoder) BadRows() []BadRow {
	return cd.badRows
}

// Reset discards all decoded rows and bad rows, keeping the column
// declarations, the underlying decoder, and the allocated buffers,
// so the decoder can be reused for the next batch.
func (cd *ColumnarDecoder) Reset() {
	cd.truncate(0)
	cd.rows = 0
	cd.badRows = cd.badRows[:0]
}

// Decode decodes records until the underlying stream is exhausted and
// appends one row per record to every column.
//
// Error handling and stream resumption:
//
//   - A record whose selected fields have unexpected types is a bad row.
//     All columns stay aligned: the partial row is rolled back and replaced
//     by an all-missing row, and the remainder of the record's map is
//     skipped so that decoding resumes at the first byte after the bad
//     record. In CollectErrors mode the row is reported via BadRows and
//     decoding continues; in FailFast mode Decode returns a *RowError and
//     the stream is left positioned at the next record boundary, so the
//     caller may call Decode again to resume with the following record.
//
//   - If the stream itself is corrupt (invalid msgpack, truncated data,
//     or an I/O error), Decode returns the raw (non-*RowError) error. The
//     stream position is then undefined and the Decoder must be reset
//     before further use. Rows reports how many aligned rows the columns
//     hold; they remain valid.
//
// Note: columnar decoding does not support interned strings or dicts.
func (cd *ColumnarDecoder) Decode() error {
	for {
		_, err := cd.dec.PeekCode()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := cd.decodeRecord(); err != nil {
			return err
		}
	}
}

func (cd *ColumnarDecoder) decodeRecord() error {
	row := cd.rows
	cd.extendRow()

	n, err := cd.dec.DecodeMapLen()
	if err != nil {
		// The record header could not be read; the stream position is
		// unknown and decoding cannot resume.
		cd.truncate(row)
		return err
	}
	if n == -1 {
		// A nil record decodes as an all-missing row.
		cd.rows++
		return nil
	}

	var rowErr error
	for i := 0; i < n; i++ {
		if rowErr != nil {
			// The row already failed: skip the remaining entries to
			// reach the next record boundary.
			if err := cd.skipEntry(); err != nil {
				cd.truncate(row)
				return err
			}
			continue
		}

		key, err := cd.decodeKey()
		if err != nil {
			cd.truncate(row)
			return err
		}

		node := cd.root.children[key]
		if node == nil {
			// Unselected field: skip without allocating.
			if err := cd.dec.Skip(); err != nil {
				cd.truncate(row)
				return err
			}
			continue
		}

		if err := cd.decodeNode(node, row); err != nil {
			var mismatch *typeMismatchError
			if errors.As(err, &mismatch) {
				rowErr = mismatch
				continue
			}
			// Structural error: the stream position is unknown and
			// decoding cannot resume.
			cd.truncate(row)
			return err
		}
	}

	if rowErr != nil {
		// Roll back the partial row and replace it with an all-missing
		// row so that columns stay aligned.
		cd.truncate(row)
		cd.extendRow()
		cd.badRows = append(cd.badRows, BadRow{Index: row, Err: rowErr})
		cd.rows++
		if cd.policy == FailFast {
			return &RowError{Index: row, Err: rowErr}
		}
		return nil
	}

	cd.rows++
	return nil
}

// decodeNode decodes the value at node. On a type mismatch the offending
// value is fully consumed before the error is returned, so the caller can
// skip the remaining map entries and resume at the next record boundary.
func (cd *ColumnarDecoder) decodeNode(node *pathNode, row int) error {
	if node.col >= 0 {
		return cd.decodeColumnValue(cd.cols[node.col], row)
	}

	c, err := cd.dec.readCode()
	if err != nil {
		return err
	}
	n, err := cd.dec.mapLen(c)
	if err != nil {
		if serr := cd.dec.skipValue(c); serr != nil {
			return serr
		}
		return &typeMismatchError{
			msg: fmt.Sprintf("msgpack: columnar: expected a map, got code=%x", c),
		}
	}
	if n == -1 {
		// A nil map leaves all descendant columns missing for this row.
		return nil
	}

	for i := 0; i < n; i++ {
		key, err := cd.decodeKey()
		if err != nil {
			return err
		}

		child := node.children[key]
		if child == nil {
			if err := cd.dec.Skip(); err != nil {
				return err
			}
			continue
		}

		if err := cd.decodeNode(child, row); err != nil {
			// Skip the remaining entries of this nested map so the
			// stream stays positioned at the map boundary.
			for j := i + 1; j < n; j++ {
				if serr := cd.skipEntry(); serr != nil {
					return serr
				}
			}
			return err
		}
	}
	return nil
}

func (cd *ColumnarDecoder) decodeColumnValue(col *Column, row int) error {
	c, err := cd.dec.readCode()
	if err != nil {
		return err
	}
	if c == msgpcode.Nil {
		col.present.set(row)
		col.null.set(row)
		return nil
	}
	if !kindAccepts(col.kind, c) {
		// Consume the unexpected value so the stream stays positioned
		// at the next map entry.
		if err := cd.dec.skipValue(c); err != nil {
			return err
		}
		return &typeMismatchError{
			msg: fmt.Sprintf("msgpack: columnar: cannot decode code=%x as %s", c, col.kind),
		}
	}

	col.present.set(row)
	switch col.kind {
	case ColumnInt64:
		v, err := cd.dec.int(c)
		if err != nil {
			return err
		}
		col.Ints[row] = v
	case ColumnUint64:
		v, err := cd.dec.uint(c)
		if err != nil {
			return err
		}
		col.Uints[row] = v
	case ColumnFloat64:
		v, err := cd.dec.float64(c)
		if err != nil {
			return err
		}
		col.Floats[row] = v
	case ColumnBool:
		v, err := cd.dec.bool(c)
		if err != nil {
			return err
		}
		col.Bools[row] = v
	case ColumnString:
		v, err := cd.dec.string(c)
		if err != nil {
			return err
		}
		col.Strings[row] = v
	case ColumnBytes:
		v, err := cd.dec.bytes(c, nil)
		if err != nil {
			return err
		}
		col.Bytes[row] = v
	}
	return nil
}

// decodeKey decodes a map key. Non-string keys are consumed and reported
// as no match, since declared paths only contain string segments.
func (cd *ColumnarDecoder) decodeKey() (string, error) {
	c, err := cd.dec.readCode()
	if err != nil {
		return "", err
	}
	if !msgpcode.IsString(c) && !msgpcode.IsBin(c) {
		if err := cd.dec.skipValue(c); err != nil {
			return "", err
		}
		return "", nil
	}

	n, err := cd.dec.bytesLen(c)
	if err != nil {
		return "", err
	}
	if n <= 0 {
		return "", nil
	}
	b, err := cd.dec.readN(n)
	if err != nil {
		return "", err
	}
	return bytesToString(b), nil
}

func (cd *ColumnarDecoder) skipEntry() error {
	if err := cd.dec.Skip(); err != nil {
		return err
	}
	return cd.dec.Skip()
}

func (cd *ColumnarDecoder) extendRow() {
	for _, col := range cd.cols {
		col.extend()
	}
}

func (cd *ColumnarDecoder) truncate(rows int) {
	for _, col := range cd.cols {
		col.truncate(rows)
	}
}

func kindAccepts(kind ColumnKind, c byte) bool {
	switch kind {
	case ColumnInt64, ColumnUint64:
		return msgpcode.IsFixedNum(c) || (c >= msgpcode.Uint8 && c <= msgpcode.Int64)
	case ColumnFloat64:
		return msgpcode.IsFixedNum(c) ||
			(c >= msgpcode.Uint8 && c <= msgpcode.Int64) ||
			c == msgpcode.Float || c == msgpcode.Double
	case ColumnBool:
		return c == msgpcode.False || c == msgpcode.True
	case ColumnString, ColumnBytes:
		return msgpcode.IsString(c) || msgpcode.IsBin(c)
	}
	return false
}
