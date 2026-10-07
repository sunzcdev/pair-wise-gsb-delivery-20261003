package avro

import (
	"cmp"
	"fmt"
	"sync"
)

// cmpfn compares two Avro values already in binary encoding. It consumes one
// whole value from a and b per the compiled schema and returns the Avro order
// of a relative to b: -1 when a sorts before b, 0 when equal, +1 when a sorts
// after b. The slices returned start immediately after the consumed value, so
// a caller can check the remainder.
//
// A comparison walks both encodings to the end even after the first unequal
// sub-value: the order verdict is fixed by the first difference, but finishing
// still validates that both inputs are complete encodings of the schema. That
// keeps "compare equal with no trailing bytes" an exact-equality test and
// means a corrupt length or an out-of-range union/enum index later in either
// input is an error rather than a silently accepted order.
type cmpfn func(a, b []byte, sl *slab) (int, []byte, []byte, error)

// OrderingError reports that a value cannot be ordered under Avro's sorting
// rules. The only such built-in type is map: the specification defines no
// order for maps, so the Java reference comparator throws when it reaches one.
// A map inside an "ignore" record field never reaches comparison, the field is
// skipped instead; OrderingError means a map is actually in the ordered path.
type OrderingError struct {
	// AvroType is the type that has no order, currently always "map".
	AvroType string
	// Field is the dotted record-field path to the unorderable type, empty
	// when it is the schema's root.
	Field string
}

func (e *OrderingError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("avro: type %s at field %s has no Avro sort order (use field order \"ignore\")", e.AvroType, e.Field)
	}
	return fmt.Sprintf("avro: type %s has no Avro sort order", e.AvroType)
}

// TrailingBytesError reports that [Schema.Compare] found extra bytes after the
// one value its schema described. Compare is strict: it compares exactly one
// Avro value. To compare a value followed by more data, use [Schema.CompareAt].
type TrailingBytesError struct {
	// Left and Right are the bytes left unconsumed in the two inputs.
	Left, Right []byte
}

func (e *TrailingBytesError) Error() string {
	return fmt.Sprintf("avro: trailing bytes after compared value: %d byte(s) on the left, %d byte(s) on the right", len(e.Left), len(e.Right))
}

// negcmp reverses a comparator's verdict for a "descending" record field. It
// is wrapped only at the field edge: ordering inside the field's own schema
// stays the same, and the field's result is flipped on the way out.
func negcmp(c cmpfn) cmpfn {
	return func(a, b []byte, sl *slab) (int, []byte, []byte, error) {
		r, ra, rb, err := c(a, b, sl)
		return -r, ra, rb, err
	}
}

func cmpNull(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	return 0, a, b, nil
}

func cmpBoolean(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	if err := needLen(a, 1, "boolean"); err != nil {
		return 0, nil, nil, err
	}
	if err := needLen(b, 1, "boolean"); err != nil {
		return 0, nil, nil, err
	}
	// Canonical with deserBoolean: only 0x01 is true, every other byte false.
	av := 0
	if a[0] == 1 {
		av = 1
	}
	bv := 0
	if b[0] == 1 {
		bv = 1
	}
	return cmp.Compare(av, bv), a[1:], b[1:], nil
}

func cmpInt(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	av, ra, err := readVarint(a)
	if err != nil {
		return 0, nil, nil, err
	}
	bv, rb, err := readVarint(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return cmp.Compare(int64(av), int64(bv)), ra, rb, nil
}

func cmpLong(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	av, ra, err := readVarlong(a)
	if err != nil {
		return 0, nil, nil, err
	}
	bv, rb, err := readVarlong(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return cmp.Compare(av, bv), ra, rb, nil
}

// cmpEnum compares symbol ordinals as signed ints, the rule the specification
// states directly. An ordinal outside the symbol table is invalid encoding,
// not an order, and errors exactly as deserializing that enum would.
func cmpEnum(symbols []string) cmpfn {
	return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
		av, ra, err := readVarint(a)
		if err != nil {
			return 0, nil, nil, err
		}
		bv, rb, err := readVarint(b)
		if err != nil {
			return 0, nil, nil, err
		}
		if av < 0 || int(av) >= len(symbols) {
			return 0, nil, nil, fmt.Errorf("enum index %d out of range [0, %d)", av, len(symbols))
		}
		if bv < 0 || int(bv) >= len(symbols) {
			return 0, nil, nil, fmt.Errorf("enum index %d out of range [0, %d)", bv, len(symbols))
		}
		return cmp.Compare(int64(av), int64(bv)), ra, rb, nil
	}
}

// floatSortKey maps IEEE-754 big-endian bits onto a key that sorts by
// Float.compare's total order: flip the sign bit for non-negatives, all bits
// for negatives. -0.0 then precedes +0.0 and every NaN sorts above +Inf. It is
// monotonic over the raw bits, so the two valid encodings of a value compare
// exactly as Java orders them without decoding a float. The encoder is
// canonical for NaN payloads (see appendAvroFloat32), so payload-bearing NaNs
// never appear on valid wire.
func floatSortKey(u uint32) uint32 {
	if u&0x80000000 != 0 {
		return ^u
	}
	return u ^ 0x80000000
}

func doubleSortKey(u uint64) uint64 {
	if u&0x8000000000000000 != 0 {
		return ^u
	}
	return u ^ 0x8000000000000000
}

func cmpFloat(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	au, ra, err := readUint32(a)
	if err != nil {
		return 0, nil, nil, err
	}
	bu, rb, err := readUint32(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return cmp.Compare(floatSortKey(au), floatSortKey(bu)), ra, rb, nil
}

func cmpDouble(a, b []byte, _ *slab) (int, []byte, []byte, error) {
	au, ra, err := readUint64(a)
	if err != nil {
		return 0, nil, nil, err
	}
	bu, rb, err := readUint64(b)
	if err != nil {
		return 0, nil, nil, err
	}
	return cmp.Compare(doubleSortKey(au), doubleSortKey(bu)), ra, rb, nil
}

// cmpByteRun compares a length-prefixed bytes or string payload as unsigned
// bytes. The zigzag length prefix must never take part in the comparison, and
// neither may the two encodings' raw bytes be compared from their start: the
// prefix differs per value, so a from-the-start prefix compare would order by
// the length varint rather than the content. We read the prefix off both and
// compare the payloads directly, with the shorter run first on a prefix match.
func cmpByteRun(typ string) cmpfn {
	return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
		an, ra, err := readLength(a, typ)
		if err != nil {
			return 0, nil, nil, err
		}
		bn, rb, err := readLength(b, typ)
		if err != nil {
			return 0, nil, nil, err
		}
		if err := needLen(ra, an, typ); err != nil {
			return 0, nil, nil, err
		}
		if err := needLen(rb, bn, typ); err != nil {
			return 0, nil, nil, err
		}
		ab, bb := ra[:an], rb[:bn]
		for i := 0; i < an && i < bn; i++ {
			if ab[i] != bb[i] {
				if ab[i] < bb[i] {
					return -1, ra[an:], rb[bn:], nil
				}
				return 1, ra[an:], rb[bn:], nil
			}
		}
		return cmp.Compare(an, bn), ra[an:], rb[bn:], nil
	}
}

func cmpFixed(size int) cmpfn {
	return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
		if err := needLen(a, size, "fixed"); err != nil {
			return 0, nil, nil, err
		}
		if err := needLen(b, size, "fixed"); err != nil {
			return 0, nil, nil, err
		}
		for i := 0; i < size; i++ {
			if a[i] != b[i] {
				if a[i] < b[i] {
					return -1, a[size:], b[size:], nil
				}
				return 1, a[size:], b[size:], nil
			}
		}
		return 0, a[size:], b[size:], nil
	}
}

// cmpRecordField is one compared record field. cmp is nil for an "ignore"
// field, whose encoded bytes are skipped on both sides without ever being
// inspected or copied: a multi-megabyte ignored bytes field costs one length
// read and a slice reslice, no heap allocation.
type cmpRecordField struct {
	name string
	cmp  cmpfn
	skip skipfn
}

type cmpRecord struct {
	once   sync.Once
	fields []cmpRecordField
	node   *schemaNode
	mbw    *minBytesWalk
	path   string
}

func (r *cmpRecord) cmp(a, b []byte, sl *slab) (int, []byte, []byte, error) {
	if sl.depth >= maxDepth {
		return 0, nil, nil, errTooDeep
	}
	sl.depth++
	defer func() { sl.depth-- }()
	r.once.Do(func() {
		r.fields = make([]cmpRecordField, len(r.node.fields))
		for i := range r.node.fields {
			f := &r.node.fields[i]
			cf := cmpRecordField{name: f.name}
			fp := fieldPath(r.path, f.name)
			switch f.order {
			case orderIgnore:
				cf.skip = buildSkip(f.node, r.mbw)
			case orderDesc:
				cf.cmp = negcmp(buildCompare(f.node, fp, r.mbw))
			default:
				cf.cmp = buildCompare(f.node, fp, r.mbw)
			}
			r.fields[i] = cf
		}
	})
	order := 0
	var err error
	for _, f := range r.fields {
		if f.skip != nil {
			// Skipping handles every type, map included, and bounds
			// recursive schemas and corrupt block/length framing itself.
			if a, err = f.skip(a, sl); err != nil {
				return 0, nil, nil, err
			}
			if b, err = f.skip(b, sl); err != nil {
				return 0, nil, nil, err
			}
			continue
		}
		var r0 int
		if r0, a, b, err = f.cmp(a, b, sl); err != nil {
			return 0, nil, nil, err
		}
		if order == 0 && r0 != 0 {
			order = r0
		}
	}
	return order, a, b, nil
}

// cmpArray walks both arrays in lockstep, one element at a time, across block
// boundaries. Array order is lexicographic: compare elements by item order; if
// every shared element is equal the shorter array sorts first. Both sides are
// read to their terminators so a corrupt block in the longer array still
// errors.
type cmpArray struct {
	item         cmpfn
	itemSkip     skipfn
	minItemBytes int
}

// arrCursor tracks one side's current block while comparing an array. It is a
// stack value, so the per-element loop allocates nothing.
type arrCursor struct {
	src   []byte
	left  int64 // elements remaining in the current block
	total int64 // cumulative elements across blocks
	done  bool  // terminator read
}

// headBlock reads the next block when the cursor has no elements left. It
// reports done at the terminator. Bounds match deserArray/skipArray exactly:
// a block count must fit the remaining buffer, and arrays of zero-byte items
// share the cumulative maxZeroByteItems cap.
func (c *arrCursor) headBlock(minItemBytes int) error {
	for c.left == 0 && !c.done {
		count, _, rest, end, err := readBlockHeader(c.src, "array", false)
		if err != nil {
			return err
		}
		c.src = rest
		if end {
			c.done = true
			return nil
		}
		if err := checkArrayBlockBounds(count, c.total, len(c.src), minItemBytes); err != nil {
			return err
		}
		c.total += count
		c.left = count
	}
	return nil
}

func (ca *cmpArray) cmp(a, b []byte, sl *slab) (int, []byte, []byte, error) {
	if sl.depth >= maxDepth {
		return 0, nil, nil, errTooDeep
	}
	sl.depth++
	defer func() { sl.depth-- }()
	ca0 := arrCursor{src: a}
	cb := arrCursor{src: b}
	order := 0
	orderSet := false
	setOrder := func(r int) {
		if !orderSet && r != 0 {
			order = r
			orderSet = true
		}
	}
	for {
		if err := ca0.headBlock(ca.minItemBytes); err != nil {
			return 0, nil, nil, err
		}
		if err := cb.headBlock(ca.minItemBytes); err != nil {
			return 0, nil, nil, err
		}
		adone, bdone := ca0.done || ca0.left == 0, cb.done || cb.left == 0
		if adone || bdone {
			if !adone || !bdone {
				// One array is a strict prefix of the other. The longer one
				// sorts later; finish it to validate its remaining framing.
				longer := &ca0
				verdict := 1 // a is longer
				if bdone {
					longer = &cb
					verdict = -1
				}
				setOrder(verdict)
				for {
					if err := longer.headBlock(ca.minItemBytes); err != nil {
						return 0, nil, nil, err
					}
					if longer.done || longer.left == 0 {
						break
					}
					var err error
					longer.src, err = ca.itemSkip(longer.src, sl)
					if err != nil {
						return 0, nil, nil, err
					}
					longer.left--
				}
			}
			return order, ca0.src, cb.src, nil
		}
		var err error
		var r int
		r, ca0.src, cb.src, err = ca.item(ca0.src, cb.src, sl)
		if err != nil {
			return 0, nil, nil, err
		}
		setOrder(r)
		ca0.left--
		cb.left--
	}
}

// cmpUnion compares the branch tag (a zigzag int) first: two values on
// different branches order by ordinal; two on the same branch order by that
// branch's own rule. An out-of-range tag is invalid encoding. When tags differ
// both values are still consumed, the unselected side by skipping, so a
// malformed payload after the differing tag still errors.
type cmpUnion struct {
	branches []cmpfn
	skips    []skipfn
}

func (u *cmpUnion) cmp(a, b []byte, sl *slab) (int, []byte, []byte, error) {
	if sl.depth >= maxDepth {
		return 0, nil, nil, errTooDeep
	}
	sl.depth++
	defer func() { sl.depth-- }()
	ai, ra, err := readVarint(a)
	if err != nil {
		return 0, nil, nil, err
	}
	bi, rb, err := readVarint(b)
	if err != nil {
		return 0, nil, nil, err
	}
	if ai < 0 || int(ai) >= len(u.branches) {
		return 0, nil, nil, fmt.Errorf("union index %d out of range [0, %d)", ai, len(u.branches))
	}
	if bi < 0 || int(bi) >= len(u.branches) {
		return 0, nil, nil, fmt.Errorf("union index %d out of range [0, %d)", bi, len(u.branches))
	}
	if ai != bi {
		ra, err = u.skips[ai](ra, sl)
		if err != nil {
			return 0, nil, nil, err
		}
		rb, err = u.skips[bi](rb, sl)
		if err != nil {
			return 0, nil, nil, err
		}
		return cmp.Compare(int64(ai), int64(bi)), ra, rb, nil
	}
	r, ra, rb, err := u.branches[ai](ra, rb, sl)
	return r, ra, rb, err
}

// cmpUnorderable is the compiled comparator for a type the ordering rules do
// not cover, currently only map. It is built only for a map reached by an
// ordered path; a map inside an "ignore" record field is skipped instead and
// never builds one.
func cmpUnorderable(kind, path string) cmpfn {
	return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
		return 0, nil, nil, &OrderingError{AvroType: kind, Field: path}
	}
}

// buildCompare compiles a schema-based comparator for node w. path is the
// dotted record-field location for OrderingError messages. mbw is the one
// minimum-bytes walk of the comparator set, shared as it is for skips, so an
// array item pointed at by many containers computes its minimum once.
func buildCompare(w *schemaNode, path string, mbw *minBytesWalk) cmpfn {
	switch w.kind {
	case "null":
		return cmpNull
	case "boolean":
		return cmpBoolean
	case "int":
		return cmpInt
	case "long":
		return cmpLong
	case "float":
		return cmpFloat
	case "double":
		return cmpDouble
	case "string":
		return cmpByteRun("string")
	case "bytes":
		return cmpByteRun("bytes")
	case "enum":
		return cmpEnum(w.symbols)
	case "fixed":
		return cmpFixed(w.size)
	case "record":
		r := &cmpRecord{node: w, mbw: mbw, path: path}
		return r.cmp
	case "array":
		return (&cmpArray{
			item:         buildCompare(w.items, path, mbw),
			itemSkip:     buildSkip(w.items, mbw),
			minItemBytes: mbw.minBytesOf(w.items),
		}).cmp
	case "union":
		u := &cmpUnion{}
		u.branches = make([]cmpfn, len(w.branches))
		u.skips = make([]skipfn, len(w.branches))
		for i, br := range w.branches {
			u.branches[i] = buildCompare(br, path, mbw)
			u.skips[i] = buildSkip(br, mbw)
		}
		return u.cmp
	case "map":
		return cmpUnorderable("map", path)
	default:
		return func(a, b []byte, _ *slab) (int, []byte, []byte, error) {
			return 0, nil, nil, fmt.Errorf("cannot compare unknown type %q", w.kind)
		}
	}
}

func (s *Schema) compileCompare() (cmpfn, error) {
	s.cmpOnce.Do(func() {
		// A dedicated walk, separate from parse's skipWalk: a comparator is
		// built lazily after parse returns and must not drain the allowance a
		// later decode's skip compilation expects.
		s.cmpfn = buildCompare(s.node, "", newMinBytesWalk())
	})
	return s.cmpfn, s.cmpErr
}

// CompareAt compares two binary-encoded Avro values of this schema without
// decoding them into Go objects, following Avro's binary sort order, and
// returns the remaining bytes after each value.
//
// The order result is -1 when a sorts before b, 0 when equal, and +1 when a
// sorts after b. Record fields honor their declared order: "ascending"
// compares normally, "descending" reverses the field's verdict, and "ignore"
// skips that field's bytes on both sides without reading or copying them.
// Primitives, records, enums, arrays and unions follow the specification's
// ordering rules. map has no defined order: reaching one in an ordered path
// returns [*OrderingError]; place such a field in an "ignore" field instead.
//
// CompareAt consumes exactly one value from each input but does not require
// the buffers to end there; the returned slices hold whatever follows. A
// truncated buffer, a corrupt length or block header, an out-of-range union or
// enum index, or a value nested deeper than the recursion limit returns
// [*ShortBufferError] or an error describing the corruption. Recursive schemas
// are bounded by the same recursion limit as Encode/Decode.
func (s *Schema) CompareAt(a, b []byte) (order int, left, right []byte, err error) {
	c, err := s.compileCompare()
	if err != nil {
		return 0, nil, nil, err
	}
	sl := slabPool.Get().(*slab)
	order, ra, rb, err := c(a, b, sl)
	sl.put()
	return order, ra, rb, err
}

// Compare compares two binary-encoded Avro values of this schema without
// decoding them into Go objects, following Avro's binary sort order.
//
// It returns -1 when a sorts before b, 0 when equal, +1 when a sorts after b.
// Compare is strict about framing: it compares exactly one Avro value, so any
// byte left over on either side is [*TrailingBytesError], even when the values
// compare equal. To compare a value followed by more data (for instance a key
// prefix inside a larger buffer), use [Schema.CompareAt].
//
// Record fields honor their declared order: "ascending" compares normally,
// "descending" reverses the field's verdict, and "ignore" skips that field's
// bytes without reading or copying them. Primitives, records, enums, arrays
// and unions follow the specification's ordering rules. map has no defined
// order: reaching one in an ordered path returns [*OrderingError], so a field
// carrying a map should declare "order":"ignore".
func (s *Schema) Compare(a, b []byte) (int, error) {
	order, ra, rb, err := s.CompareAt(a, b)
	if err != nil {
		return 0, err
	}
	if len(ra) != 0 || len(rb) != 0 {
		return 0, &TrailingBytesError{Left: ra, Right: rb}
	}
	return order, nil
}
