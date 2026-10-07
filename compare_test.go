package avro_test

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/twmb/avro"
	"github.com/twmb/avro/internal/avrotest"
)

// mustCompare compares a and b under s, failing on error, and requires
// both buffers to hold exactly one value.
func mustCompare(t *testing.T, s *avro.Schema, a, b []byte) int {
	t.Helper()
	order, restA, restB, err := s.CompareBinary(a, b)
	if err != nil {
		t.Fatalf("CompareBinary: %v", err)
	}
	if len(restA) != 0 || len(restB) != 0 {
		t.Fatalf("CompareBinary left tails: restA=%d restB=%d bytes", len(restA), len(restB))
	}
	return order
}

func TestCompareBinaryPrimitives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		a, b   any
		want   int
	}{
		{"null", `"null"`, nil, nil, 0},
		{"bool-false-true", `"boolean"`, false, true, -1},
		{"bool-equal", `"boolean"`, true, true, 0},
		{"int-neg", `"int"`, int32(-1), int32(0), -1},
		{"int-pos", `"int"`, int32(5), int32(10), -1},
		{"int-min-max", `"int"`, int32(math.MinInt32), int32(math.MaxInt32), -1},
		{"long", `"long"`, int64(-1 << 40), int64(1 << 40), -1},
		{"float", `"float"`, float32(1.5), float32(2.5), -1},
		{"float-negzero", `"float"`, float32(math.Copysign(0, -1)), float32(0), -1},
		{"float-nan-vs-inf", `"float"`, float32(math.NaN()), float32(math.Inf(1)), 1},
		{"float-nan-nan", `"float"`, float32(math.NaN()), float32(math.NaN()), 0},
		{"double", `"double"`, 1.5, 2.5, -1},
		{"double-negzero", `"double"`, math.Copysign(0, -1), 0.0, -1},
		{"double-nan-vs-inf", `"double"`, math.NaN(), math.Inf(1), 1},
		{"double-nan-neg-vs-pos", `"double"`, -math.NaN(), math.NaN(), 0},
		// A shared binary prefix must not decide a string comparison:
		// "ab" is a strict prefix of "abc" and sorts below it.
		{"string-prefix", `"string"`, "ab", "abc", -1},
		{"string-prefix-rev", `"string"`, "abc", "ab", 1},
		{"string-mid", `"string"`, "abc", "abd", -1},
		{"string-empty", `"string"`, "", "a", -1},
		{"bytes-prefix", `"bytes"`, []byte{1, 2}, []byte{1, 2, 3}, -1},
		{"bytes-order", `"bytes"`, []byte{0xff}, []byte{0x00, 0x00}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := avrotest.MustParse(t, tc.schema)
			ea := avrotest.MustEncode(t, s, tc.a)
			eb := avrotest.MustEncode(t, s, tc.b)
			if got := mustCompare(t, s, ea, eb); got != tc.want {
				t.Errorf("compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			if got := mustCompare(t, s, eb, ea); got != -tc.want {
				t.Errorf("compare(%v, %v) = %d, want %d (reversed)", tc.b, tc.a, got, -tc.want)
			}
			if got := mustCompare(t, s, ea, ea); got != 0 {
				t.Errorf("compare(%v, %v) = %d, want 0 (self)", tc.a, tc.a, got)
			}
		})
	}
}

func TestCompareBinaryFixedAndEnum(t *testing.T) {
	fixed := avrotest.MustParse(t, `{"type":"fixed","name":"F","size":4}`)
	ea := avrotest.MustEncode(t, fixed, []byte{1, 2, 3, 4})
	eb := avrotest.MustEncode(t, fixed, []byte{1, 2, 3, 5})
	if got := mustCompare(t, fixed, ea, eb); got != -1 {
		t.Errorf("fixed compare = %d, want -1", got)
	}

	// Enum order is symbol position, not lexicographic: Z < A < M here.
	enum := avrotest.MustParse(t, `{"type":"enum","name":"E","symbols":["Z","A","M"]}`)
	za := avrotest.MustEncode(t, enum, "Z")
	aa := avrotest.MustEncode(t, enum, "A")
	ma := avrotest.MustEncode(t, enum, "M")
	if got := mustCompare(t, enum, za, aa); got != -1 {
		t.Errorf("Z vs A = %d, want -1 (symbol position)", got)
	}
	if got := mustCompare(t, enum, aa, ma); got != -1 {
		t.Errorf("A vs M = %d, want -1", got)
	}
	if got := mustCompare(t, enum, ma, za); got != 1 {
		t.Errorf("M vs Z = %d, want 1", got)
	}
}
func TestCompareBinaryRecordOrders(t *testing.T) {
	s := avrotest.MustParse(t, `{
		"type": "record", "name": "R",
		"fields": [
			{"name": "asc", "type": "int"},
			{"name": "desc", "type": "int", "order": "descending"},
			{"name": "ign", "type": "string", "order": "ignore"},
			{"name": "tail", "type": "int"}
		]
	}`)
	enc := func(asc, desc int32, ign string, tail int32) []byte {
		return avrotest.MustEncode(t, s, map[string]any{
			"asc": asc, "desc": desc, "ign": ign, "tail": tail,
		})
	}
	for _, tc := range []struct {
		name string
		a, b []byte
		want int
	}{
		{"asc-decides", enc(1, 9, "x", 0), enc(2, 0, "x", 0), -1},
		{"desc-flips", enc(0, 5, "x", 0), enc(0, 3, "x", 0), -1}, // descending: 5 sorts before 3
		{"ignore-skips", enc(0, 0, "aaa", 7), enc(0, 0, "zzz", 7), 0},
		{"tail-decides", enc(0, 0, "a", 1), enc(0, 0, "b", 2), -1},
		{"all-equal", enc(1, 2, "same", 3), enc(1, 2, "diff", 3), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustCompare(t, s, tc.a, tc.b); got != tc.want {
				t.Errorf("compare = %d, want %d", got, tc.want)
			}
			if got := mustCompare(t, s, tc.b, tc.a); got != -tc.want {
				t.Errorf("reversed compare = %d, want %d", got, -tc.want)
			}
		})
	}
}

func TestCompareBinaryUnion(t *testing.T) {
	s := avrotest.MustParse(t, `["null", "string", "int"]`)
	nullEnc := avrotest.MustEncode(t, s, nil)
	strA := avrotest.MustEncode(t, s, "abc")
	strB := avrotest.MustEncode(t, s, "abd")
	intEnc := avrotest.MustEncode(t, s, int32(7))

	// Branch index orders first: null(0) < string(1) < int(2).
	if got := mustCompare(t, s, nullEnc, strA); got != -1 {
		t.Errorf("null vs string = %d, want -1", got)
	}
	if got := mustCompare(t, s, strA, intEnc); got != -1 {
		t.Errorf("string vs int = %d, want -1 (branch index)", got)
	}
	if got := mustCompare(t, s, intEnc, nullEnc); got != 1 {
		t.Errorf("int vs null = %d, want 1", got)
	}
	// Same branch: by value.
	if got := mustCompare(t, s, strA, strB); got != -1 {
		t.Errorf("abc vs abd = %d, want -1", got)
	}
}

func TestCompareBinaryArray(t *testing.T) {
	s := avrotest.MustParse(t, `{"type":"array","items":"int"}`)
	enc := func(vals ...int32) []byte {
		return avrotest.MustEncode(t, s, vals)
	}
	for _, tc := range []struct {
		name string
		a, b []byte
		want int
	}{
		{"empty-equal", enc(), enc(), 0},
		{"empty-first", enc(), enc(1), -1},
		{"prefix-first", enc(1, 2), enc(1, 2, 3), -1},
		{"elem-decides", enc(1, 3), enc(1, 2, 9), 1},
		{"first-decides", enc(0, 99), enc(1), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustCompare(t, s, tc.a, tc.b); got != tc.want {
				t.Errorf("compare = %d, want %d", got, tc.want)
			}
		})
	}
}

// appendZigzag appends n zigzag-varlong-encoded, for hand-framed wire data.
func appendZigzag(dst []byte, n int64) []byte {
	u := uint64(n)<<1 ^ uint64(n>>63)
	for u >= 0x80 {
		dst = append(dst, byte(u)|0x80)
		u >>= 7
	}
	return append(dst, byte(u))
}

func TestCompareBinaryArrayFramingIgnored(t *testing.T) {
	s := avrotest.MustParse(t, `{"type":"array","items":"int"}`)
	// One block of two items versus two blocks of one item each, plus a
	// negative-count byte-size-framed variant: same array, equal order.
	oneBlock := appendZigzag(nil, 2)
	oneBlock = appendZigzag(oneBlock, 10)
	oneBlock = appendZigzag(oneBlock, 20)
	oneBlock = appendZigzag(oneBlock, 0)

	twoBlocks := appendZigzag(nil, 1)
	twoBlocks = appendZigzag(twoBlocks, 10)
	twoBlocks = appendZigzag(twoBlocks, 1)
	twoBlocks = appendZigzag(twoBlocks, 20)
	twoBlocks = appendZigzag(twoBlocks, 0)

	var items []byte
	items = appendZigzag(items, 10)
	items = appendZigzag(items, 20)
	sized := appendZigzag(nil, -2)
	sized = appendZigzag(sized, int64(len(items)))
	sized = append(sized, items...)
	sized = appendZigzag(sized, 0)

	for _, b := range [][]byte{twoBlocks, sized} {
		if got := mustCompare(t, s, oneBlock, b); got != 0 {
			t.Errorf("differently framed equal arrays = %d, want 0", got)
		}
	}
}
func TestCompareBinaryMap(t *testing.T) {
	// A map in a compared position has no defined order.
	top := avrotest.MustParse(t, `{"type":"map","values":"int"}`)
	enc := avrotest.MustEncode(t, top, map[string]any{"a": int32(1)})
	if _, _, _, err := top.CompareBinary(enc, enc); !errors.Is(err, avro.ErrNotComparable) {
		t.Errorf("top-level map: err = %v, want ErrNotComparable", err)
	}

	rec := avrotest.MustParse(t, `{
		"type": "record", "name": "R",
		"fields": [
			{"name": "id", "type": "int"},
			{"name": "m", "type": {"type": "map", "values": "int"}}
		]
	}`)
	a := avrotest.MustEncode(t, rec, map[string]any{"id": int32(1), "m": map[string]any{"k": int32(1)}})
	if _, _, _, err := rec.CompareBinary(a, a); !errors.Is(err, avro.ErrNotComparable) {
		t.Errorf("compared map field: err = %v, want ErrNotComparable", err)
	}

	// The same map under an "ignore"-ordered field is skipped and the
	// record stays comparable.
	ign := avrotest.MustParse(t, `{
		"type": "record", "name": "R",
		"fields": [
			{"name": "id", "type": "int"},
			{"name": "m", "type": {"type": "map", "values": "int"}, "order": "ignore"}
		]
	}`)
	ma := map[string]any{"id": int32(1), "m": map[string]any{"k": int32(1), "j": int32(2)}}
	mb := map[string]any{"id": int32(2), "m": map[string]any{"z": int32(9)}}
	ea := avrotest.MustEncode(t, ign, ma)
	eb := avrotest.MustEncode(t, ign, mb)
	if got := mustCompare(t, ign, ea, eb); got != -1 {
		t.Errorf("ignored map field: compare = %d, want -1", got)
	}
}

func TestCompareBinaryTrailingBytes(t *testing.T) {
	s := avrotest.MustParse(t, `"int"`)
	ea := avrotest.MustEncode(t, s, int32(1))
	eb := avrotest.MustEncode(t, s, int32(2))
	order, restA, restB, err := s.CompareBinary(append(ea, 9, 9, 9), append(eb, 7))
	if err != nil {
		t.Fatalf("CompareBinary: %v", err)
	}
	if order != -1 {
		t.Errorf("order = %d, want -1", order)
	}
	if !bytes.Equal(restA, []byte{9, 9, 9}) || !bytes.Equal(restB, []byte{7}) {
		t.Errorf("rests = %v, %v; want [9 9 9], [7]", restA, restB)
	}
}

func TestCompareBinaryCorrupt(t *testing.T) {
	str := avrotest.MustParse(t, `"string"`)

	// Declared length overruns the buffer.
	long := appendZigzag(nil, 100)
	long = append(long, 'a')
	if _, _, _, err := str.CompareBinary(long, long); err == nil {
		t.Error("overlong string length: want error")
	} else {
		var sb *avro.ShortBufferError
		if !errors.As(err, &sb) {
			t.Errorf("overlong string length: err = %v, want ShortBufferError", err)
		}
	}

	// Negative length.
	neg := appendZigzag(nil, -3)
	if _, _, _, err := str.CompareBinary(neg, neg); err == nil {
		t.Error("negative string length: want error")
	}

	// Truncated value.
	if _, _, _, err := str.CompareBinary(nil, nil); err == nil {
		t.Error("empty input: want error")
	}

	// Union index out of range.
	u := avrotest.MustParse(t, `["null", "int"]`)
	bad := appendZigzag(nil, 5)
	if _, _, _, err := u.CompareBinary(bad, bad); err == nil {
		t.Error("union index out of range: want error")
	}

	// Enum index out of range.
	e := avrotest.MustParse(t, `{"type":"enum","name":"E","symbols":["A"]}`)
	if _, _, _, err := e.CompareBinary(bad, bad); err == nil {
		t.Error("enum index out of range: want error")
	}

	// A corrupt array block count is bounded against the buffer.
	arr := avrotest.MustParse(t, `{"type":"array","items":"int"}`)
	huge := appendZigzag(nil, 1<<40)
	huge = appendZigzag(huge, 0)
	if _, _, _, err := arr.CompareBinary(huge, huge); err == nil {
		t.Error("huge array block count: want error")
	}
}

func TestCompareBinaryRecursiveDepth(t *testing.T) {
	s := avrotest.MustParse(t, `{
		"type": "record", "name": "N",
		"fields": [
			{"name": "v", "type": "int"},
			{"name": "next", "type": ["null", "N"]}
		]
	}`)
	// Hand-build a chain deeper than the recursion limit: each level is
	// v=0 (0x00) followed by union branch 1 (zigzag 1 = 0x02); the innermost
	// next is null (branch 0).
	var wire []byte
	for range 1100 {
		wire = append(wire, 0x00, 0x02)
	}
	wire = append(wire, 0x00, 0x00)
	_, _, _, err := s.CompareBinary(wire, wire)
	if err == nil {
		t.Fatal("deeply nested recursive value: want error")
	}
	if !strings.Contains(err.Error(), "recursion limit") {
		t.Errorf("err = %v, want recursion-limit error", err)
	}
}

// ---------- randomized differential test ----------

// diffSchema exercises every comparable kind plus descending and ignore
// orders, with a map and a large bytes blob reachable only through ignored
// fields.
const diffSchema = `{
	"type": "record", "name": "Diff",
	"fields": [
		{"name": "id", "type": "int"},
		{"name": "score", "type": "double"},
		{"name": "rank", "type": "long", "order": "descending"},
		{"name": "name", "type": "string"},
		{"name": "flag", "type": "boolean"},
		{"name": "color", "type": {"type": "enum", "name": "Color", "symbols": ["RED", "GREEN", "BLUE", "ALPHA"]}},
		{"name": "tags", "type": {"type": "array", "items": "string"}},
		{"name": "maybe", "type": ["null", "string", "int"]},
		{"name": "blob", "type": "bytes"},
		{"name": "sig", "type": {"type": "fixed", "name": "Sig", "size": 4}, "order": "descending"},
		{"name": "f", "type": "float"},
		{"name": "nested", "type": {"type": "record", "name": "Inner", "fields": [
			{"name": "x", "type": "int"},
			{"name": "y", "type": "string", "order": "descending"},
			{"name": "aux", "type": "bytes", "order": "ignore"}
		]}},
		{"name": "ignoredBig", "type": "bytes", "order": "ignore"},
		{"name": "ignoredMap", "type": {"type": "map", "values": "int"}, "order": "ignore"}
	]
}`

var diffNames = []string{"", "a", "ab", "abc", "abd", "b", "zebra", "Zebra", "长", "长尾巴"}

func randString(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		return diffNames[r.Intn(len(diffNames))]
	}
	b := make([]byte, r.Intn(6))
	for i := range b {
		b[i] = byte('a' + r.Intn(26))
	}
	return string(b)
}

func randBytes(r *rand.Rand, max int) []byte {
	b := make([]byte, r.Intn(max+1))
	_, _ = r.Read(b)
	return b
}

func randDouble(r *rand.Rand) float64 {
	switch r.Intn(12) {
	case 0:
		return math.NaN()
	case 1:
		return math.Copysign(0, -1)
	case 2:
		return 0
	case 3:
		return math.Inf(1)
	case 4:
		return math.Inf(-1)
	}
	return float64(r.Intn(41)-20) * 0.5
}

func randFloat(r *rand.Rand) float32 {
	switch r.Intn(12) {
	case 0:
		return float32(math.NaN())
	case 1:
		return float32(math.Copysign(0, -1))
	case 2:
		return 0
	}
	return float32(r.Intn(41)-20) * 0.5
}

// randDiffValue generates a random Go value for diffSchema in the shapes
// the encoder accepts: a record is map[string]any, and the union branches
// null, string, and int are disjoint in Go type, so branch selection is
// unambiguous.
func randDiffValue(r *rand.Rand) map[string]any {
	var maybe any
	switch r.Intn(3) {
	case 0:
		maybe = nil
	case 1:
		maybe = randString(r)
	default:
		maybe = int32(r.Intn(2001) - 1000)
	}
	tags := make([]any, r.Intn(4))
	for i := range tags {
		tags[i] = randString(r)
	}
	return map[string]any{
		"id":    int32(r.Intn(2001) - 1000),
		"score": randDouble(r),
		"rank":  int64(r.Intn(2001) - 1000),
		"name":  randString(r),
		"flag":  r.Intn(2) == 0,
		"color": []string{"RED", "GREEN", "BLUE", "ALPHA"}[r.Intn(4)],
		"tags":  tags,
		"maybe": maybe,
		"blob":  randBytes(r, 8),
		"sig":   randFixed4(r),
		"f":     randFloat(r),
		"nested": map[string]any{
			"x":   int32(r.Intn(21) - 10),
			"y":   randString(r),
			"aux": randBytes(r, 4),
		},
		"ignoredBig": randBytes(r, 32),
		"ignoredMap": map[string]any{"k": int32(r.Intn(10))},
	}
}

// randFixed4 returns a fresh 4-byte random fixed value.
func randFixed4(r *rand.Rand) []byte {
	b := make([]byte, 4)
	_, _ = r.Read(b)
	return b
}

// refJavaFloat32Order and refJavaFloat64Order mirror the Java Float.compare
// and Double.compare semantics CompareBinary documents, independently
// re-derived for the differential check.
func refJavaFloat32Order(x, y float32) int {
	if x < y {
		return -1
	}
	if x > y {
		return 1
	}
	xb, yb := math.Float32bits(x), math.Float32bits(y)
	if x != x {
		xb = 0x7fc00000
	}
	if y != y {
		yb = 0x7fc00000
	}
	switch {
	case int32(xb) < int32(yb):
		return -1
	case int32(xb) > int32(yb):
		return 1
	}
	return 0
}

func refJavaFloat64Order(x, y float64) int {
	if x < y {
		return -1
	}
	if x > y {
		return 1
	}
	xb, yb := math.Float64bits(x), math.Float64bits(y)
	if x != x {
		xb = 0x7ff8000000000000
	}
	if y != y {
		yb = 0x7ff8000000000000
	}
	switch {
	case int64(xb) < int64(yb):
		return -1
	case int64(xb) > int64(yb):
		return 1
	}
	return 0
}

func refOrder[T int | int32 | int64 | string](x, y T) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// refUnionBranch reports the branch index of a decoded union value for
// diffSchema's ["null", "string", "int"] union, whose branches are disjoint
// in Go type.
func refUnionBranch(t *testing.T, n *avro.SchemaNode, v any) int {
	t.Helper()
	kind := "null"
	switch v.(type) {
	case nil:
	case string:
		kind = "string"
	case int32:
		kind = "int"
	default:
		t.Fatalf("unexpected union value type %T", v)
	}
	for i := range n.Branches {
		if n.Branches[i].Type == kind {
			return i
		}
	}
	t.Fatalf("no branch %q in union", kind)
	return -1
}

// refCompare is the object-level reference: it compares two values decoded
// into any, walking the schema tree and applying the Avro sort order,
// including per-field ascending, descending, and ignore.
func refCompare(t *testing.T, n *avro.SchemaNode, x, y any) int {
	t.Helper()
	switch n.Type {
	case "null":
		return 0
	case "boolean":
		xb, yb := x.(bool), y.(bool)
		switch {
		case xb == yb:
			return 0
		case yb:
			return -1
		}
		return 1
	case "int":
		return refOrder(x.(int32), y.(int32))
	case "long":
		return refOrder(x.(int64), y.(int64))
	case "float":
		return refJavaFloat32Order(x.(float32), y.(float32))
	case "double":
		return refJavaFloat64Order(x.(float64), y.(float64))
	case "string":
		return refOrder(x.(string), y.(string))
	case "bytes", "fixed":
		return bytes.Compare(x.([]byte), y.([]byte))
	case "enum":
		idx := func(s string) int {
			for i, sym := range n.Symbols {
				if sym == s {
					return i
				}
			}
			t.Fatalf("symbol %q not in enum", s)
			return -1
		}
		return refOrder(idx(x.(string)), idx(y.(string)))
	case "array":
		xs, ys := x.([]any), y.([]any)
		for i := 0; i < len(xs) && i < len(ys); i++ {
			if c := refCompare(t, n.Items, xs[i], ys[i]); c != 0 {
				return c
			}
		}
		return refOrder(len(xs), len(ys))
	case "union":
		xb, yb := refUnionBranch(t, n, x), refUnionBranch(t, n, y)
		if xb != yb {
			return refOrder(xb, yb)
		}
		return refCompare(t, &n.Branches[xb], x, y)
	case "record":
		xm, ym := x.(map[string]any), y.(map[string]any)
		for i := range n.Fields {
			f := &n.Fields[i]
			if f.Order == "ignore" {
				continue
			}
			c := refCompare(t, &f.Type, xm[f.Name], ym[f.Name])
			if c != 0 {
				if f.Order == "descending" {
					return -c
				}
				return c
			}
		}
		return 0
	case "map":
		t.Fatal("reference comparator reached a map; it must be ignored")
	}
	t.Fatalf("unhandled schema type %q", n.Type)
	return 0
}

func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	}
	return 0
}

func TestCompareBinaryRandomDifferential(t *testing.T) {
	s := avrotest.MustParse(t, diffSchema)
	root := s.Root()
	r := rand.New(rand.NewSource(20261008))
	for i := range 3000 {
		va, vb := randDiffValue(r), randDiffValue(r)
		ea := avrotest.MustEncode(t, s, va)
		eb := avrotest.MustEncode(t, s, vb)

		var da, db any
		avrotest.MustDecode(t, s, ea, &da)
		avrotest.MustDecode(t, s, eb, &db)
		want := sign(refCompare(t, root, da, db))

		got := mustCompare(t, s, ea, eb)
		if got != want {
			t.Fatalf("iteration %d: CompareBinary = %d, reference = %d\na=%#v\nb=%#v", i, got, want, da, db)
		}
		if gotRev := mustCompare(t, s, eb, ea); gotRev != -want {
			t.Fatalf("iteration %d: reversed CompareBinary = %d, want %d", i, gotRev, -want)
		}
	}
}

// An ignored field must be skipped in place: comparing two records that
// differ only after a 1 MiB ignored bytes blob neither decodes nor copies
// the blob, so the comparison allocates nothing.
func TestCompareBinaryIgnoredBytesNotCopied(t *testing.T) {
	s := avrotest.MustParse(t, `{
		"type": "record", "name": "R",
		"fields": [
			{"name": "id", "type": "int"},
			{"name": "blob", "type": "bytes", "order": "ignore"},
			{"name": "tail", "type": "string"}
		]
	}`)
	blobA := make([]byte, 1<<20)
	blobB := make([]byte, 1<<20)
	r := rand.New(rand.NewSource(1))
	_, _ = r.Read(blobA)
	_, _ = r.Read(blobB)
	ea := avrotest.MustEncode(t, s, map[string]any{"id": int32(1), "blob": blobA, "tail": "x"})
	eb := avrotest.MustEncode(t, s, map[string]any{"id": int32(2), "blob": blobB, "tail": "y"})

	// Warm up once so the lazily compiled record comparator is built
	// before the measurement.
	if got := mustCompare(t, s, ea, eb); got != -1 {
		t.Fatalf("warm-up compare = %d, want -1", got)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if got := mustCompare(t, s, ea, eb); got != -1 {
			t.Fatalf("compare = %d, want -1", got)
		}
	})
	if allocs != 0 {
		t.Errorf("CompareBinary allocated %.0f times per run over a 1 MiB ignored blob; the blob must be skipped, not copied", allocs)
	}
}
