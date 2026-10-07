package avro

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math"
	"sync"
)

// ErrNotComparable is returned by [Schema.CompareBinary] when the schema
// places a value with no defined sort order, a map, in a position that
// participates in the comparison. A map reachable only through record
// fields whose order is "ignore" is skipped like any other ignored value
// and never triggers this error.
var ErrNotComparable = errors.New("avro: schema is not comparable")

// cmpfn compares one encoded value at the head of a against one at the head
// of b, returning their order (-1, 0, +1) and the unconsumed tails of both
// buffers. Both values are always fully consumed, even once the order is
// decided, so the tails are exact and corrupt trailing structure still
// errors. The slab rides along only for its recursion-depth bound, as in
// skipfn; comparison never carves.
type cmpfn func(a, b []byte, sl *slab) (int, []byte, []byte, error)

// CompareBinary compares two values in their Avro binary encodings against
// the sort order s defines, without decoding either into a Go value. It
// returns -1, 0, or +1 as the first value orders before, equal to, or after
// the second, plus the unconsumed tails of a and b: exactly one value is
// read from each buffer, so trailing bytes are allowed and come back as
// restA and restB (both empty when each buffer held exactly one value).
//
// The order is the Avro specification's sort order:
//
//   - null: equal
//   - boolean: false before true
//   - int, long: ascending numeric
//   - float, double: as Java's Float.compare and Double.compare, so NaN of
//     any payload equals itself and sorts after every other value, and -0.0
//     sorts before +0.0
//   - bytes, string, fixed: lexicographic by unsigned byte
//   - enum: by symbol position in the schema
//   - array: lexicographic by element
//   - union: by branch index first, then by value within the branch
//   - record: field by field in declaration order, each field ascending,
//     descending, or skipped per its "order" attribute
//
// Maps have no defined order. A schema whose comparison would consult a map
// yields [ErrNotComparable]; a map reachable only through "ignore"-ordered
// fields is skipped and stays comparable. Logical types compare by their
// underlying encoding, as the spec directs.
//
// Corrupt input, a truncated value, a length that overruns the buffer, a
// union or enum index out of range, or nesting past the recursion limit,
// returns the error instead of an order: the same [ShortBufferError] and
// recursion-limit errors [Schema.Decode] reports. The comparison never
// copies value bytes; an ignored field is skipped in place, so an ignored
// megabyte blob costs no allocation.
func (s *Schema) CompareBinary(a, b []byte) (order int, restA, restB []byte, err error) {
	s.cmpOnce.Do(func() {
		s.cmp, s.cmpErr = buildCmp(s.node, newMinBytesWalk())
	})
	if s.cmpErr != nil {
		return 0, nil, nil, s.cmpErr
	}
	// The pooled slab carries only the depth bound here; nothing carves, so
	// the pool round trip is the whole cost.
	sl := slabPool.Get().(*slab)
	order, restA, restB, err = s.cmp(a, b, sl)
	sl.put()
	return order, restA, restB, err
}

func cmpNull(a, b []byte, _ *slab) (int, []byte, []byte, error) { return 0, a, b, nil }

func cmpBoolean(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	if err := needLen(a, 1, "boolean"); err != nil {
		return 0, nil, nil, err
	}
	if err := needLen(b, 1, "boolean"); err != nil {
		return 0, nil, nil, err
	}
	// Only 0x01 is true, matching deserBoolean: a non-canonical byte orders
	// as the value decode would read.
	x, y := a[0] == 1, b[0] == 1
	switch {
	case x == y:
		return 0, a[1:], b[1:], nil
	case y:
		return -1, a[1:], b[1:], nil
	default:
		return 1, a[1:], b[1:], nil
	}
}

func cmpInt(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	x, a, err := readVarint(a)
	if err != nil {
		return 0, nil, nil, err
	}
	y, b, err := readVarint(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return cmp.Compare(x, y), a, b, nil
}

func cmpLong(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	x, a, err := readVarlong(a)
	if err != nil {
		return 0, nil, nil, err
	}
	y, b, err := readVarlong(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return cmp.Compare(x, y), a, b, nil
}

func cmpFloat(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	xb, a, err := readUint32(a)
	if err != nil {
		return 0, nil, nil, err
	}
	yb, b, err := readUint32(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return javaFloat32Order(math.Float32frombits(xb), math.Float32frombits(yb)), a, b, nil
}

func cmpDouble(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	xb, a, err := readUint64(a)
	if err != nil {
		return 0, nil, nil, err
	}
	yb, b, err := readUint64(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return javaFloat64Order(math.Float64frombits(xb), math.Float64frombits(yb)), a, b, nil
}

// javaFloat32Order and javaFloat64Order implement the Float.compare and
// Double.compare orderings the Avro sort-order section defers to: NaN of
// any payload or sign canonicalizes to one value sorting above +Inf, and
// -0.0 sorts below +0.0. Once NaN is canonicalized, the bit patterns order
// correctly as signed integers, which is exactly what Java's
// floatToIntBits and doubleToLongBits rely on.
func javaFloat32Order(x, y float32) int {
	if x < y {
		return -1
	}
	if x > y {
		return 1
	}
	return cmp.Compare(javaFloat32Bits(x), javaFloat32Bits(y))
}

func javaFloat32Bits(f float32) int32 {
	if f != f {
		return 0x7fc00000
	}
	return int32(math.Float32bits(f))
}

func javaFloat64Order(x, y float64) int {
	if x < y {
		return -1
	}
	if x > y {
		return 1
	}
	return cmp.Compare(javaFloat64Bits(x), javaFloat64Bits(y))
}

func javaFloat64Bits(f float64) int64 {
	if f != f {
		return 0x7ff8000000000000
	}
	return int64(math.Float64bits(f))
}

// cmpBytes compares two length-prefixed byte runs; bytes and string share
// the wire form and the lexicographic order. The comparison spans each
// value's own declared length, never a prefix of the remaining buffer:
// comparing buffer prefixes instead would equate "ab" with "abc" and let
// the following value's bytes leak into the order.
func cmpBytes(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	na, a, err := readLength(a, "bytes")
	if err != nil {
		return 0, nil, nil, err
	}
	nb, b, err := readLength(b, "bytes")
	if err != nil {
		return 0, nil, nil, err
	}
	return bytes.Compare(a[:na], b[:nb]), a[na:], b[nb:], nil
}

func cmpFixed(size int) cmpfn {
	return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
		if err := needLen(a, size, "fixed"); err != nil {
			return 0, nil, nil, err
		}
		if err := needLen(b, size, "fixed"); err != nil {
			return 0, nil, nil, err
		}
		return bytes.Compare(a[:size], b[:size]), a[size:], b[size:], nil
	}
}

// cmpEnum orders by symbol position, not by symbol text, and validates the
// index as deserEnum does so a corrupt ordinal errors rather than silently
// sorting.
func cmpEnum(symbols int) cmpfn {
	return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
		x, a, err := readVarint(a)
		if err != nil {
			return 0, nil, nil, err
		}
		if x < 0 || int(x) >= symbols {
			return 0, nil, nil, fmt.Errorf("enum index %d out of range [0, %d)", x, symbols)
		}
		y, b, err := readVarint(b)
		if err != nil {
			return 0, nil, nil, err
		}
		if y < 0 || int(y) >= symbols {
			return 0, nil, nil, fmt.Errorf("enum index %d out of range [0, %d)", y, symbols)
		}
		return cmp.Compare(x, y), a, b, nil
	}
}

// cmpRecordFields is the record comparator's lazily compiled field set,
// mirroring skipRecordFields: a recursive schema reaches its own record
// node again during compilation, so the per-field comparators build on
// first use, not inside buildCmp. mbw is the operation's one min-bytes
// walk, shared with the ignored-field skippers exactly as skipRecord
// shares it, and minBytesOf locks for this decode-time compile.
type cmpRecordFields struct {
	once   sync.Once
	fields []cmpRecordField
	node   *schemaNode
	mbw    *minBytesWalk
	err    error
}

type cmpRecordField struct {
	cmp  cmpfn  // nil where the field's order is "ignore"
	skip skipfn // every field: ignored fields and post-decision tails
	desc bool
}

func cmpRecord(w *schemaNode, mbw *minBytesWalk) cmpfn {
	r := &cmpRecordFields{node: w, mbw: mbw}
	return func(a, b []byte, sl *slab) (int, []byte, []byte, error) {
		if sl.depth >= maxDepth {
			return 0, nil, nil, errTooDeep
		}
		sl.depth++
		defer func() { sl.depth-- }()
		r.once.Do(func() {
			r.fields = make([]cmpRecordField, len(r.node.fields))
			for i := range r.node.fields {
				f := &r.node.fields[i]
				r.fields[i].skip = buildSkip(f.node, r.mbw)
				if f.order == "ignore" {
					continue
				}
				c, err := buildCmp(f.node, r.mbw)
				if err != nil {
					r.err = err
					return
				}
				r.fields[i].cmp = c
				r.fields[i].desc = f.order == "descending"
			}
		})
		if r.err != nil {
			return 0, nil, nil, r.err
		}
		// Walk every field even after the order is decided: cmpfn promises
		// exact tails, so the remainder of both values is skipped in place,
		// never decoded.
		order := 0
		for i := range r.fields {
			if order != 0 || r.fields[i].cmp == nil {
				var err error
				if a, err = r.fields[i].skip(a, sl); err != nil {
					return 0, nil, nil, err
				}
				if b, err = r.fields[i].skip(b, sl); err != nil {
					return 0, nil, nil, err
				}
				continue
			}
			c, ra, rb, err := r.fields[i].cmp(a, b, sl)
			if err != nil {
				return 0, nil, nil, err
			}
			a, b = ra, rb
			if c != 0 {
				order = c
				if r.fields[i].desc {
					order = -order
				}
			}
		}
		return order, a, b, nil
	}
}

// cmpArray compares two arrays element by element, reading each side's
// block framing independently: the same elements may be framed in
// different blocks in the two buffers, and framing never participates in
// the order. Once the order is decided the remaining elements of both
// sides are skipped, keeping the returned tails exact.
func cmpArray(w *schemaNode, mbw *minBytesWalk) (cmpfn, error) {
	itemCmp, err := buildCmp(w.items, mbw)
	if err != nil {
		return nil, err
	}
	itemSkip := buildSkip(w.items, mbw)
	minItemBytes := mbw.minBytesOf(w.items)
	return func(a, b []byte, sl *slab) (int, []byte, []byte, error) {
		if sl.depth >= maxDepth {
			return 0, nil, nil, errTooDeep
		}
		sl.depth++
		defer func() { sl.depth-- }()
		var totalA, totalB, remA, remB int64
		endA, endB := false, false
		order := 0
		for {
			if !endA && remA == 0 {
				count, _, rest, end, err := readBlockHeader(a, "array", true)
				if err != nil {
					return 0, nil, nil, err
				}
				a = rest
				if end {
					endA = true
				} else {
					if err := checkArrayBlockBounds(count, totalA, len(a), minItemBytes); err != nil {
						return 0, nil, nil, err
					}
					totalA += count
					remA = count
				}
			}
			if !endB && remB == 0 {
				count, _, rest, end, err := readBlockHeader(b, "array", true)
				if err != nil {
					return 0, nil, nil, err
				}
				b = rest
				if end {
					endB = true
				} else {
					if err := checkArrayBlockBounds(count, totalB, len(b), minItemBytes); err != nil {
						return 0, nil, nil, err
					}
					totalB += count
					remB = count
				}
			}
			if endA && endB {
				return order, a, b, nil
			}
			var err error
			switch {
			case endA:
				// a's elements are a proper prefix of b's: the shorter
				// array sorts first, and b's remaining elements are
				// skipped.
				if order == 0 {
					order = -1
				}
				if b, err = itemSkip(b, sl); err != nil {
					return 0, nil, nil, err
				}
				remB--
			case endB:
				if order == 0 {
					order = 1
				}
				if a, err = itemSkip(a, sl); err != nil {
					return 0, nil, nil, err
				}
				remA--
			case order != 0:
				if a, err = itemSkip(a, sl); err != nil {
					return 0, nil, nil, err
				}
				if b, err = itemSkip(b, sl); err != nil {
					return 0, nil, nil, err
				}
				remA--
				remB--
			default:
				var c int
				c, a, b, err = itemCmp(a, b, sl)
				if err != nil {
					return 0, nil, nil, err
				}
				remA--
				remB--
				if c != 0 {
					order = c
				}
			}
		}
	}, nil
}

// cmpUnion orders by branch index first, as the spec directs, then by the
// value within the branch. Two values in different branches still have
// their branch payloads skipped, keeping the tails exact.
func cmpUnion(w *schemaNode, mbw *minBytesWalk) (cmpfn, error) {
	branchCmps := make([]cmpfn, len(w.branches))
	branchSkips := make([]skipfn, len(w.branches))
	for i, br := range w.branches {
		c, err := buildCmp(br, mbw)
		if err != nil {
			return nil, err
		}
		branchCmps[i] = c
		branchSkips[i] = buildSkip(br, mbw)
	}
	return func(a, b []byte, sl *slab) (int, []byte, []byte, error) {
		if sl.depth >= maxDepth {
			return 0, nil, nil, errTooDeep
		}
		sl.depth++
		defer func() { sl.depth-- }()
		idxA, a, err := readVarint(a)
		if err != nil {
			return 0, nil, nil, err
		}
		if idxA < 0 || int(idxA) >= len(branchCmps) {
			return 0, nil, nil, fmt.Errorf("union index %d out of range [0, %d)", idxA, len(branchCmps))
		}
		idxB, b, err := readVarint(b)
		if err != nil {
			return 0, nil, nil, err
		}
		if idxB < 0 || int(idxB) >= len(branchCmps) {
			return 0, nil, nil, fmt.Errorf("union index %d out of range [0, %d)", idxB, len(branchCmps))
		}
		if idxA != idxB {
			if a, err = branchSkips[idxA](a, sl); err != nil {
				return 0, nil, nil, err
			}
			if b, err = branchSkips[idxB](b, sl); err != nil {
				return 0, nil, nil, err
			}
			return cmp.Compare(idxA, idxB), a, b, nil
		}
		return branchCmps[idxA](a, b, sl)
	}, nil
}

// buildCmp compiles a comparator for node w. It mirrors buildSkip: mbw is
// the operation's one min-bytes walk, and the record arm defers its field
// compilation to first use so recursive schemas terminate. A map in a
// compared position fails the build with ErrNotComparable; a map under an
// "ignore"-ordered field never reaches here, since the record arm routes
// those fields to buildSkip.
func buildCmp(w *schemaNode, mbw *minBytesWalk) (cmpfn, error) {
	switch w.kind {
	case "null":
		return cmpNull, nil
	case "boolean":
		return cmpBoolean, nil
	case "int":
		return cmpInt, nil
	case "long":
		return cmpLong, nil
	case "float":
		return cmpFloat, nil
	case "double":
		return cmpDouble, nil
	case "bytes", "string":
		// A string is a length-prefixed byte run on the wire, exactly as
		// in primitiveSkips, and both order lexicographically by byte.
		return cmpBytes, nil
	case "fixed":
		return cmpFixed(w.size), nil
	case "enum":
		return cmpEnum(len(w.symbols)), nil
	case "record":
		return cmpRecord(w, mbw), nil
	case "array":
		return cmpArray(w, mbw)
	case "union":
		return cmpUnion(w, mbw)
	case "map":
		return nil, fmt.Errorf("%w: map values have no defined order", ErrNotComparable)
	default:
		return nil, fmt.Errorf("cannot compare unknown type %q", w.kind)
	}
}
