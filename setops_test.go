package ksuid

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSetOps(t *testing.T) {
	ops := []struct {
		name string
		op   func(a, b CompressedSet) (CompressedSet, error)
		ref  func(inA, inB bool) bool
	}{
		{"union", Union, func(x, y bool) bool { return x || y }},
		{"intersect", Intersect, func(x, y bool) bool { return x && y }},
		{"subtract", Subtract, func(x, y bool) bool { return x && !y }},
	}

	seq := sequenceIDs(0, 150) // one sequence of 150 consecutive identifiers
	rangeA := seq[:100]
	rangeB := seq[50:150] // overlaps rangeA on [50,100)
	edgeB := seq[90:110]  // partially overlaps the end of rangeA
	pointB := seq[50:51]  // a single identifier in the middle of rangeA
	pool := randomIDs(400, 8)

	scenarios := []struct {
		name string
		a, b []KSUID
	}{
		{"random sparse ids with shared timestamps", sampleIDs(pool, 1), sampleIDs(pool, 2)},
		{"overlapping sequences", rangeA, rangeB},
		{"disjoint timestamps", idsAtTime(1000000, 30), idsAtTime(2000000, 30)},
		{"partially overlapping ranges", rangeA, edgeB},
		{"single point inside a range", rangeA, pointB},
		{"identical sets", rangeA, rangeA},
		{"empty first", nil, seq[:10]},
		{"empty second", seq[:10], nil},
		{"both empty", nil, nil},
	}

	for _, scenario := range scenarios {
		for _, op := range ops {
			t.Run(scenario.name+"/"+op.name, func(t *testing.T) {
				setA := Compress(scenario.a...)
				setB := Compress(scenario.b...)

				result, err := op.op(setA, setB)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				// Cross-validate against a plain map-based set operation,
				// and verify the result is byte-compatible with what
				// Compress produces for the same logical set.
				expected := Compress(mapSetOp(scenario.a, scenario.b, op.ref)...)
				if !bytes.Equal(result, expected) {
					t.Errorf("result mismatch:\ngot:  %v\nwant: %v", result, expected)
				}

				// The result must decode with the existing iterator into a
				// strictly ascending, duplicate-free sequence.
				requireSortedUnique(t, result)
			})
		}
	}
}
func TestSetOpsLongRanges(t *testing.T) {
	const n = 10000000 // ten million consecutive identifiers

	base := makeUint128(0, 0x100000)
	point := add128(base, makeUint128(0, 5))
	last := add128(base, makeUint128(0, n-1))

	t.Run("subtracting a single point splits a long range in two", func(t *testing.T) {
		a := makeRangeSet(42, base, n)
		b := makeRangeSet(42, point, 1)

		if len(a) > 64 {
			t.Fatalf("precondition failed: %d elements should compress to a few bytes, got %d", n, len(a))
		}

		diff, err := Subtract(a, b)
		if err != nil {
			t.Fatal(err)
		}

		ranges := readRanges(t, diff)
		if len(ranges) != 2 {
			t.Fatalf("expected the range to be split in 2, got %d ranges", len(ranges))
		}
		expectRange(t, ranges[0], 42, base, decr128(point))
		expectRange(t, ranges[1], 42, incr128(point), last)

		// Merging the point back must reconstitute the full range.
		merged, err := Union(diff, b)
		if err != nil {
			t.Fatal(err)
		}
		if r := readRanges(t, merged); len(r) != 1 {
			t.Fatalf("expected the union to merge back into 1 range, got %d", len(r))
		} else {
			expectRange(t, r[0], 42, base, last)
		}
	})

	t.Run("merging ranges across timestamps", func(t *testing.T) {
		a := makeRangeSet(100, base, n)
		b := makeRangeSet(200, base, n)

		union, err := Union(a, b)
		if err != nil {
			t.Fatal(err)
		}
		ranges := readRanges(t, union)
		if len(ranges) != 2 {
			t.Fatalf("expected 2 ranges, got %d", len(ranges))
		}
		expectRange(t, ranges[0], 100, base, last)
		expectRange(t, ranges[1], 200, base, last)

		inter, err := Intersect(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if len(inter) != 0 {
			t.Errorf("expected an empty intersection across timestamps, got %d bytes", len(inter))
		}

		diff, err := Subtract(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(diff, a) {
			t.Errorf("expected subtracting a disjoint timestamp to be a no-op")
		}
	})

	t.Run("adjacent ranges from both sets merge into one", func(t *testing.T) {
		a := makeRangeSet(42, base, 100)
		b := makeRangeSet(42, add128(base, makeUint128(0, 100)), 100)

		union, err := Union(a, b)
		if err != nil {
			t.Fatal(err)
		}
		ranges := readRanges(t, union)
		if len(ranges) != 1 {
			t.Fatalf("expected adjacent ranges to merge into 1, got %d", len(ranges))
		}
		expectRange(t, ranges[0], 42, base, add128(base, makeUint128(0, 199)))
	})

	t.Run("empty results", func(t *testing.T) {
		a := makeRangeSet(42, base, n)

		diff, err := Subtract(a, a)
		if err != nil {
			t.Fatal(err)
		}
		if len(diff) != 0 {
			t.Errorf("expected an empty difference of identical sets, got %d bytes", len(diff))
		}

		inter, err := Intersect(a, makeRangeSet(43, base, n))
		if err != nil {
			t.Fatal(err)
		}
		if len(inter) != 0 {
			t.Errorf("expected an empty intersection of disjoint sets, got %d bytes", len(inter))
		}
	})

	t.Run("peak allocations", func(t *testing.T) {
		a := makeRangeSet(42, base, n)
		b := makeRangeSet(42, point, 1)
		c := makeRangeSet(43, base, n)

		t.Logf("logical elements per operand: %d", n)
		t.Logf("union allocs/op:     %.1f", testing.AllocsPerRun(20, func() { Union(a, c) }))
		t.Logf("intersect allocs/op: %.1f", testing.AllocsPerRun(20, func() { Intersect(a, c) }))
		t.Logf("subtract allocs/op:  %.1f", testing.AllocsPerRun(20, func() { Subtract(a, b) }))

		var m1, m2 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m1)
		if _, err := Union(a, c); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&m2)
		t.Logf("union of %d elements allocated %d bytes in total", 2*n, m2.TotalAlloc-m1.TotalAlloc)
	})
}
func TestSetOpsCorrupt(t *testing.T) {
	valid := Compress(New(), New())

	raw := func(ts uint32) []byte {
		id := New()
		binary.BigEndian.PutUint32(id[:timestampLengthInBytes], ts)
		return append([]byte{byte(rawKSUID)}, id[:]...)
	}

	corrupt := []struct {
		name string
		set  CompressedSet
	}{
		{"truncated raw id", CompressedSet([]byte{byte(rawKSUID), 1, 2, 3})},
		{"truncated range varint", append(append([]byte{}, valid...), payloadRange|4, 1, 2)},
		{"zero length range varint", append(append([]byte{}, valid...), payloadRange|0)},
		{"range of zero elements", append(append([]byte{}, valid...), payloadRange|1, 0)},
		{"out of order timestamps", CompressedSet(append(raw(200), raw(100)...))},
	}

	ops := []struct {
		name string
		op   func(a, b CompressedSet) (CompressedSet, error)
	}{
		{"union", Union},
		{"intersect", Intersect},
		{"subtract", Subtract},
	}

	for _, c := range corrupt {
		for _, op := range ops {
			for _, pos := range []string{"first", "second"} {
				t.Run(c.name+"/"+op.name+"/"+pos, func(t *testing.T) {
					var result CompressedSet
					var err error
					if pos == "first" {
						result, err = op.op(c.set, valid)
					} else {
						result, err = op.op(valid, c.set)
					}

					if err == nil {
						t.Fatal("expected an error, got none")
					}
					if result != nil {
						t.Error("a partial result was delivered for a corrupt input")
					}

					cerr, ok := err.(*CorruptSetError)
					if !ok {
						t.Fatalf("error type is %T, expected *CorruptSetError", err)
					}
					if cerr.Source != pos {
						t.Errorf("error source: got %q, want %q", cerr.Source, pos)
					}
					if cerr.Offset < 0 || cerr.Offset >= len(c.set) {
						t.Errorf("error offset %d out of bounds (set length %d)", cerr.Offset, len(c.set))
					}
					if !strings.Contains(err.Error(), "offset") {
						t.Errorf("error message should mention the offset: %v", err)
					}
				})
			}
		}
	}
}

func TestSetOpsDoNotModifyInputs(t *testing.T) {
	idsA := sequenceIDs(0, 50)
	idsB := sequenceIDs(25, 75)

	// Both sets share the same backing array.
	buf := make([]byte, 0, 4096)
	a := AppendCompressed(buf, idsA...)
	b := AppendCompressed(a[len(a):len(a)], idsB...)

	snapA := append([]byte{}, a...)
	snapB := append([]byte{}, b...)

	for _, op := range []func(a, b CompressedSet) (CompressedSet, error){
		Union, Intersect, Subtract,
	} {
		if _, err := op(a, b); err != nil {
			t.Fatal(err)
		}
		if _, err := op(b, a); err != nil {
			t.Fatal(err)
		}
	}

	if !bytes.Equal(a, snapA) {
		t.Error("the first input set was modified")
	}
	if !bytes.Equal(b, snapB) {
		t.Error("the second input set was modified")
	}
}

func BenchmarkSetOps(b *testing.B) {
	const n = 1000000
	a := makeRangeSet(42, makeUint128(0, 0x1000), n)
	c := makeRangeSet(42, makeUint128(0, 0x1000+n/2), n)

	b.Run("union", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			Union(a, c)
		}
	})

	b.Run("intersect", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			Intersect(a, c)
		}
	})

	b.Run("subtract", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			Subtract(a, c)
		}
	})
}

// makeRangeSet builds a compressed set holding count consecutive identifiers
// starting at the given timestamp and payload, without materializing them.
func makeRangeSet(timestamp uint32, lo uint128, count uint64) CompressedSet {
	id := lo.ksuid(timestamp)
	buf := make([]byte, 0, 32)
	buf = append(buf, byte(rawKSUID))
	buf = append(buf, id[:]...)
	if m := count - 1; m != 0 {
		n := varintLength64(m)
		buf = append(buf, payloadRange|byte(n))
		buf = appendVarint64(buf, m, n)
	}
	return CompressedSet(buf)
}

func readRanges(t *testing.T, set CompressedSet) []setRange {
	t.Helper()
	r := newSetRangeReader("test", set)
	var ranges []setRange
	for {
		rng, ok, err := r.next()
		if err != nil {
			t.Fatalf("reading result ranges: %v", err)
		}
		if !ok {
			return ranges
		}
		ranges = append(ranges, rng)
	}
}

func expectRange(t *testing.T, r setRange, timestamp uint32, lo, hi uint128) {
	t.Helper()
	if r.timestamp != timestamp || r.lo != lo || r.hi != hi {
		t.Errorf("bad range: got (ts=%d, lo=%s, hi=%s), want (ts=%d, lo=%s, hi=%s)",
			r.timestamp, r.lo, r.hi, timestamp, lo, hi)
	}
}

func mapSetOp(a, b []KSUID, op func(inA, inB bool) bool) []KSUID {
	inA := make(map[KSUID]struct{}, len(a))
	inB := make(map[KSUID]struct{}, len(b))
	for _, id := range a {
		inA[id] = struct{}{}
	}
	for _, id := range b {
		inB[id] = struct{}{}
	}

	var out []KSUID
	for id := range inA {
		_, xb := inB[id]
		if op(true, xb) {
			out = append(out, id)
		}
	}
	for id := range inB {
		if _, xa := inA[id]; !xa && op(false, true) {
			out = append(out, id)
		}
	}
	Sort(out)
	return out
}

func requireSortedUnique(t *testing.T, set CompressedSet) {
	t.Helper()
	var prev KSUID
	i := 0
	for it := set.Iter(); it.Next(); i++ {
		if i > 0 && bytes.Compare(prev[:], it.KSUID[:]) >= 0 {
			t.Fatalf("set is not strictly ascending at index %d: %s then %s", i, prev, it.KSUID)
		}
		prev = it.KSUID
	}
}

func randomIDs(n, timestamps int) []KSUID {
	rng := rand.New(rand.NewSource(42))
	base := time.Unix(1600000000, 0)
	ids := make([]KSUID, n)
	for i := range ids {
		t := base.Add(time.Duration(rng.Intn(timestamps)) * time.Second)
		ids[i], _ = NewRandomWithTime(t)
	}
	return ids
}

func sampleIDs(pool []KSUID, seed int64) []KSUID {
	rng := rand.New(rand.NewSource(seed))
	var ids []KSUID
	for _, id := range pool {
		if rng.Intn(2) == 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func sequenceIDs(from, to int) []KSUID {
	seq := Sequence{Seed: New()}
	ids := make([]KSUID, 0, to-from)
	for i := 0; i < to; i++ {
		id, _ := seq.Next()
		if i >= from {
			ids = append(ids, id)
		}
	}
	return ids
}

func idsAtTime(unixSec int64, n int) []KSUID {
	ts := time.Unix(unixSec, 0)
	ids := make([]KSUID, n)
	for i := range ids {
		ids[i], _ = NewRandomWithTime(ts)
	}
	return ids
}
