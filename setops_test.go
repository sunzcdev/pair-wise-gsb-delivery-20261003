package ksuid

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestCompressedSetOps(t *testing.T) {
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "small sets match map-based set operations",
			function: testCompressedSetOpsSmallSets,
		},
		{
			scenario: "subtracting a single point splits a long range in two",
			function: testCompressedSetOpsSplitRange,
		},
		{
			scenario: "subtracting a sub-range splits a long range in two",
			function: testCompressedSetOpsSplitRangeByRange,
		},
		{
			scenario: "union merges ranges across timestamps",
			function: testCompressedSetOpsCrossTimestamp,
		},
		{
			scenario: "union coalesces adjacent and overlapping ranges",
			function: testCompressedSetOpsCoalesce,
		},
		{
			scenario: "empty results",
			function: testCompressedSetOpsEmpty,
		},
		{
			scenario: "malformed inputs are rejected with source and offset",
			function: testCompressedSetOpsMalformed,
		},
		{
			scenario: "inputs sharing a backing array are not modified",
			function: testCompressedSetOpsSharedBackingArray,
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, test.function)
	}
}

// makeRangeIDs builds count KSUIDs with consecutive payloads starting at the
// given payload value, all sharing one timestamp.
func makeRangeIDs(timestamp uint32, start uint64, count int) []KSUID {
	ids := make([]KSUID, count)
	for i := range ids {
		ids[i] = makeUint128(0, start+uint64(i)).ksuid(timestamp)
	}
	return ids
}

func randomIDs(rng *rand.Rand, count, numTimestamps, payloadSpace int) []KSUID {
	ids := make([]KSUID, count)
	for i := range ids {
		ts := uint32(1000 + rng.Intn(numTimestamps))
		p := uint64(rng.Int63n(int64(payloadSpace)))
		ids[i] = makeUint128(0, p).ksuid(ts)
	}
	return ids
}

func expandCompressedSet(t *testing.T, set CompressedSet) []KSUID {
	t.Helper()
	var ids []KSUID
	for it := set.Iter(); it.Next(); {
		ids = append(ids, it.KSUID)
	}
	return ids
}

func assertAscendingUnique(t *testing.T, ids []KSUID) {
	t.Helper()
	for i := 1; i < len(ids); i++ {
		if Compare(ids[i-1], ids[i]) >= 0 {
			t.Fatalf("ids are not strictly ascending at index %d: %s >= %s", i, ids[i-1], ids[i])
		}
	}
}

func countSegments(t *testing.T, set CompressedSet) int {
	t.Helper()
	s := compressedSetStream{source: "test", data: []byte(set)}
	n := 0
	for s.next() {
		n++
	}
	if err := s.Err(); err != nil {
		t.Fatalf("counting segments: %v", err)
	}
	return n
}

func equalIDs(a, b []KSUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// expectedSet computes the expected result of a set operation with maps.
func expectedSet(idsA, idsB []KSUID, keep func(inA, inB bool) bool) []KSUID {
	inA := make(map[KSUID]struct{}, len(idsA))
	inB := make(map[KSUID]struct{}, len(idsB))
	all := make(map[KSUID]struct{}, len(idsA)+len(idsB))
	for _, id := range idsA {
		inA[id] = struct{}{}
		all[id] = struct{}{}
	}
	for _, id := range idsB {
		inB[id] = struct{}{}
		all[id] = struct{}{}
	}
	out := make([]KSUID, 0, len(all))
	for id := range all {
		_, a := inA[id]
		_, b := inB[id]
		if keep(a, b) {
			out = append(out, id)
		}
	}
	Sort(out)
	return out
}

var compressedSetOps = []struct {
	name  string
	apply func(a, b CompressedSet) (CompressedSet, error)
	keep  func(inA, inB bool) bool
}{
	{"union", Union, func(a, b bool) bool { return a || b }},
	{"intersect", Intersect, func(a, b bool) bool { return a && b }},
	{"subtract", Subtract, func(a, b bool) bool { return a && !b }},
}

func testCompressedSetOpsSmallSets(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for trial := 0; trial < 300; trial++ {
		idsA := randomIDs(rng, rng.Intn(40), 4, 60)
		idsB := randomIDs(rng, rng.Intn(40), 3, 60)
		setA := Compress(idsA...)
		setB := Compress(idsB...)
		snapshotA := append([]byte(nil), setA...)
		snapshotB := append([]byte(nil), setB...)

		for _, op := range compressedSetOps {
			got, err := op.apply(setA, setB)
			if err != nil {
				t.Fatalf("trial %d: %s: %v", trial, op.name, err)
			}
			gotIDs := expandCompressedSet(t, got)
			assertAscendingUnique(t, gotIDs)
			if want := expectedSet(idsA, idsB, op.keep); !equalIDs(gotIDs, want) {
				t.Fatalf("trial %d: %s: got %d ids, want %d ids", trial, op.name, len(gotIDs), len(want))
			}
		}

		if !bytes.Equal(setA, snapshotA) {
			t.Fatalf("trial %d: left input set was modified", trial)
		}
		if !bytes.Equal(setB, snapshotB) {
			t.Fatalf("trial %d: right input set was modified", trial)
		}
	}
}

func testCompressedSetOpsSplitRange(t *testing.T) {
	const n = 1 << 20
	const ts = 1000
	const cut = 777777

	setA := Compress(makeRangeIDs(ts, 0, n)...)
	setB := Compress(makeRangeIDs(ts, cut, 1)...)

	diff, err := Subtract(setA, setB)
	if err != nil {
		t.Fatal(err)
	}

	if len(diff) > 64 {
		t.Errorf("difference of long ranges was expanded: %d bytes", len(diff))
	}
	if segs := countSegments(t, diff); segs != 2 {
		t.Errorf("expected the difference to be split into 2 segments, got %d", segs)
	}

	ids := expandCompressedSet(t, diff)
	assertAscendingUnique(t, ids)
	if len(ids) != n-1 {
		t.Fatalf("expected %d ids in the difference, got %d", n-1, len(ids))
	}
	if got := uint128Payload(ids[cut-1]); got != makeUint128(0, cut-1) {
		t.Errorf("bad id before the cut point: %v", got)
	}
	if got := uint128Payload(ids[cut]); got != makeUint128(0, cut+1) {
		t.Errorf("bad id after the cut point: %v", got)
	}

	// The subtracted point was already in A, so the union is A itself and
	// the intersection is the single point.
	if u, err := Union(setA, setB); err != nil {
		t.Fatal(err)
	} else if ids := expandCompressedSet(t, u); len(ids) != n {
		t.Errorf("expected %d ids in the union, got %d", n, len(ids))
	}
	if i, err := Intersect(setA, setB); err != nil {
		t.Fatal(err)
	} else if ids := expandCompressedSet(t, i); len(ids) != 1 || ids[0] != makeRangeIDs(ts, cut, 1)[0] {
		t.Errorf("expected the intersection to hold only the cut point, got %d ids", len(ids))
	}
}

func testCompressedSetOpsSplitRangeByRange(t *testing.T) {
	const n = 1 << 20
	const ts = 1000
	const lo = 100000
	const hi = 200000

	setA := Compress(makeRangeIDs(ts, 0, n)...)
	setB := Compress(makeRangeIDs(ts, lo, hi-lo)...)

	diff, err := Subtract(setA, setB)
	if err != nil {
		t.Fatal(err)
	}

	if len(diff) > 64 {
		t.Errorf("difference of long ranges was expanded: %d bytes", len(diff))
	}
	if segs := countSegments(t, diff); segs != 2 {
		t.Errorf("expected the difference to be split into 2 segments, got %d", segs)
	}

	ids := expandCompressedSet(t, diff)
	assertAscendingUnique(t, ids)
	if len(ids) != n-(hi-lo) {
		t.Fatalf("expected %d ids in the difference, got %d", n-(hi-lo), len(ids))
	}
	if got := uint128Payload(ids[lo-1]); got != makeUint128(0, lo-1) {
		t.Errorf("bad id before the removed range: %v", got)
	}
	if got := uint128Payload(ids[lo]); got != makeUint128(0, hi) {
		t.Errorf("bad id after the removed range: %v", got)
	}
}

func testCompressedSetOpsCrossTimestamp(t *testing.T) {
	const n = 1 << 16

	setA := Compress(makeRangeIDs(1000, 0, n)...)
	setB := Compress(makeRangeIDs(2000, 0, n)...)

	u, err := Union(setA, setB)
	if err != nil {
		t.Fatal(err)
	}
	if segs := countSegments(t, u); segs != 2 {
		t.Errorf("expected ranges at different timestamps to stay separate, got %d segments", segs)
	}
	ids := expandCompressedSet(t, u)
	assertAscendingUnique(t, ids)
	if len(ids) != 2*n {
		t.Fatalf("expected %d ids in the union, got %d", 2*n, len(ids))
	}
	if ids[0].Timestamp() != 1000 || ids[len(ids)-1].Timestamp() != 2000 {
		t.Errorf("ids are not ordered by timestamp: %d .. %d", ids[0].Timestamp(), ids[len(ids)-1].Timestamp())
	}

	// The union is commutative.
	u2, err := Union(setB, setA)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(expandCompressedSet(t, u2), ids) {
		t.Error("union is not commutative")
	}

	// Sets with disjoint timestamps have an empty intersection.
	if i, err := Intersect(setA, setB); err != nil {
		t.Fatal(err)
	} else if len(i) != 0 {
		t.Errorf("expected an empty intersection, got %d bytes", len(i))
	}

	// Subtracting a set with a disjoint timestamp is the identity.
	if d, err := Subtract(setA, setB); err != nil {
		t.Fatal(err)
	} else if !equalIDs(expandCompressedSet(t, d), expandCompressedSet(t, setA)) {
		t.Error("subtracting a disjoint set changed the result")
	}
}

func testCompressedSetOpsCoalesce(t *testing.T) {
	const ts = 1000

	setA := Compress(makeRangeIDs(ts, 0, 100)...)   // [0, 100)
	setB := Compress(makeRangeIDs(ts, 100, 100)...) // [100, 200), adjacent to A
	setC := Compress(makeRangeIDs(ts, 50, 100)...)  // [50, 150), overlaps A and B

	if u, err := Union(setA, setB); err != nil {
		t.Fatal(err)
	} else {
		if segs := countSegments(t, u); segs != 1 {
			t.Errorf("adjacent ranges were not coalesced: %d segments", segs)
		}
		if ids := expandCompressedSet(t, u); len(ids) != 200 {
			t.Errorf("expected 200 ids in the union of adjacent ranges, got %d", len(ids))
		}
	}

	if u, err := Union(setA, setC); err != nil {
		t.Fatal(err)
	} else {
		if segs := countSegments(t, u); segs != 1 {
			t.Errorf("overlapping ranges were not coalesced: %d segments", segs)
		}
		if ids := expandCompressedSet(t, u); len(ids) != 150 {
			t.Errorf("expected 150 ids in the union of overlapping ranges, got %d", len(ids))
		}
	}

	if i, err := Intersect(setA, setC); err != nil {
		t.Fatal(err)
	} else {
		if segs := countSegments(t, i); segs != 1 {
			t.Errorf("expected a single intersection segment, got %d", segs)
		}
		if ids := expandCompressedSet(t, i); len(ids) != 50 {
			t.Errorf("expected 50 ids in the intersection, got %d", len(ids))
		}
	}

	if d, err := Subtract(setA, setC); err != nil {
		t.Fatal(err)
	} else if ids := expandCompressedSet(t, d); len(ids) != 50 {
		t.Errorf("expected 50 ids in the difference, got %d", len(ids))
	}

	if d, err := Subtract(setC, setA); err != nil {
		t.Fatal(err)
	} else if ids := expandCompressedSet(t, d); len(ids) != 50 {
		t.Errorf("expected 50 ids in the difference, got %d", len(ids))
	}
}

func testCompressedSetOpsEmpty(t *testing.T) {
	setA := Compress(makeRangeIDs(1000, 0, 100)...)
	setB := Compress(makeRangeIDs(1000, 1000, 100)...) // disjoint payloads
	setC := Compress(makeRangeIDs(2000, 0, 100)...)    // disjoint timestamp
	sup := Compress(makeRangeIDs(1000, 0, 200)...)     // superset of A
	empty := CompressedSet(nil)

	for _, op := range compressedSetOps {
		if got, err := op.apply(empty, empty); err != nil || len(got) != 0 {
			t.Errorf("%s of two empty sets: got %d bytes, err=%v", op.name, len(got), err)
		}
	}

	if got, err := Intersect(setA, setB); err != nil || len(got) != 0 {
		t.Errorf("intersection of disjoint sets: got %d bytes, err=%v", len(got), err)
	}
	if got, err := Intersect(setA, setC); err != nil || len(got) != 0 {
		t.Errorf("intersection of disjoint timestamps: got %d bytes, err=%v", len(got), err)
	}
	if got, err := Intersect(setA, empty); err != nil || len(got) != 0 {
		t.Errorf("intersection with an empty set: got %d bytes, err=%v", len(got), err)
	}
	if got, err := Subtract(setA, setA); err != nil || len(got) != 0 {
		t.Errorf("subtracting a set from itself: got %d bytes, err=%v", len(got), err)
	}
	if got, err := Subtract(setA, sup); err != nil || len(got) != 0 {
		t.Errorf("subtracting a superset: got %d bytes, err=%v", len(got), err)
	}
	if got, err := Subtract(empty, setA); err != nil || len(got) != 0 {
		t.Errorf("subtracting from an empty set: got %d bytes, err=%v", len(got), err)
	}

	if got, err := Union(setA, empty); err != nil {
		t.Fatal(err)
	} else if !equalIDs(expandCompressedSet(t, got), expandCompressedSet(t, setA)) {
		t.Error("union with an empty set changed the result")
	}
	if got, err := Union(empty, setA); err != nil {
		t.Fatal(err)
	} else if !equalIDs(expandCompressedSet(t, got), expandCompressedSet(t, setA)) {
		t.Error("union with an empty set changed the result")
	}
}

func testCompressedSetOpsMalformed(t *testing.T) {
	valid := Compress(makeRangeIDs(1000, 0, 10)...)

	hi := makeRangeIDs(1000, 100, 1)[0]
	lo := makeRangeIDs(1000, 0, 1)[0]

	malformed := []struct {
		name string
		set  CompressedSet
	}{
		{"truncated set", valid[:len(valid)-1]},
		{"records out of order", append(Compress(hi), Compress(lo)...)},
		{"empty payload range", append(Compress(hi), byte(payloadRange|1), 0x00)},
		{"invalid record header", CompressedSet{0x01}},
	}

	for _, m := range malformed {
		for _, op := range compressedSetOps {
			for _, side := range []struct {
				name string
				a, b CompressedSet
			}{
				{"left", m.set, valid},
				{"right", valid, m.set},
			} {
				got, err := op.apply(side.a, side.b)
				if err == nil {
					t.Errorf("%s with %s in the %s set: expected an error, got none", op.name, m.name, side.name)
					continue
				}
				if got != nil {
					t.Errorf("%s with %s in the %s set: expected a nil set, got %d bytes", op.name, m.name, side.name, len(got))
				}
				csErr, ok := err.(*CompressedSetError)
				if !ok {
					t.Errorf("%s with %s in the %s set: unexpected error type %T", op.name, m.name, side.name, err)
					continue
				}
				if csErr.Source != side.name {
					t.Errorf("%s with %s in the %s set: error reports source %q", op.name, m.name, side.name, csErr.Source)
				}
				if csErr.Offset < 0 || csErr.Offset >= len(m.set) {
					t.Errorf("%s with %s in the %s set: error offset %d is out of range", op.name, m.name, side.name, csErr.Offset)
				}
			}
		}
	}
}

func testCompressedSetOpsSharedBackingArray(t *testing.T) {
	rng := rand.New(rand.NewSource(99))

	setA := Compress(randomIDs(rng, 30, 3, 50)...)
	setB := Compress(randomIDs(rng, 30, 3, 50)...)

	buf := make([]byte, 0, len(setA)+len(setB))
	buf = append(buf, setA...)
	buf = append(buf, setB...)
	a := CompressedSet(buf[:len(setA)])
	b := CompressedSet(buf[len(setA):])
	snapshot := append([]byte(nil), buf...)

	for _, op := range compressedSetOps {
		if _, err := op.apply(a, b); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
		if _, err := op.apply(b, a); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
	}

	if !bytes.Equal(buf, snapshot) {
		t.Error("input sets sharing a backing array were modified")
	}
}

func TestCompressedSetOpsAllocs(t *testing.T) {
	const n = 1 << 20

	setA := Compress(makeRangeIDs(1000, 0, n)...)
	setB := Compress(makeRangeIDs(1000, n/2, n)...)

	var out CompressedSet
	var err error
	allocs := testing.AllocsPerRun(100, func() {
		out, err = Union(setA, setB)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(expandCompressedSet(t, out)); got != n+n/2 {
		t.Fatalf("expected %d ids in the union, got %d", n+n/2, got)
	}
	t.Logf("union of two ranges of %d ids: %.1f allocs/op", n, allocs)
	if allocs > 10 {
		t.Errorf("too many allocations: %.1f allocs/op", allocs)
	}
}

func BenchmarkCompressedSetOps(b *testing.B) {
	const n = 1 << 20

	setA := Compress(makeRangeIDs(1000, 0, n)...)
	setB := Compress(makeRangeIDs(1000, n/2, n)...)

	b.Run("union", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Union(setA, setB); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("intersect", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Intersect(setA, setB); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("subtract", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Subtract(setA, setB); err != nil {
				b.Fatal(err)
			}
		}
	})
}
