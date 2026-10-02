package ksuid

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

func TestCompressedSetIndex(t *testing.T) {
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{scenario: "sparse", function: testCompressedSetIndexSparse},
		{scenario: "packed", function: testCompressedSetIndexPacked},
		{scenario: "mixed", function: testCompressedSetIndexMixed},
		{scenario: "target before the first member", function: testCompressedSetIndexBeforeFirst},
		{scenario: "target after the last member", function: testCompressedSetIndexAfterLast},
		{scenario: "empty set", function: testCompressedSetIndexEmpty},
		{scenario: "large range occupying a few bytes", function: testCompressedSetIndexLargeRange},
		{scenario: "index is reusable across seeks", function: testCompressedSetIndexReusable},
		{scenario: "malformed encodings are rejected at construction", function: testCompressedSetIndexCorrupt},
	}

	for _, test := range tests {
		t.Run(test.scenario, test.function)
	}
}

// assertSeekGEMatchesIteration produces a regular iterator scan of ids as the
// lower-bound reference and checks SeekGE against it for each target.
func assertSeekGEMatchesIteration(t *testing.T, ids []KSUID, targets []KSUID) {
	t.Helper()

	set := Compress(ids...)

	members := make([]KSUID, 0, len(ids))
	for it := set.Iter(); it.Next(); {
		members = append(members, it.KSUID)
	}

	index, err := set.Index()
	if err != nil {
		t.Fatalf("building index failed: %v", err)
	}

	for _, target := range targets {
		want, found := lowerBound(members, target)

		iter := index.SeekGE(target)
		if !iter.Next() {
			if found {
				t.Errorf("SeekGE(%s) returned no member, want %s", target, want)
			}
			continue
		}
		if !found {
			t.Errorf("SeekGE(%s) returned %s, want end of set", target, iter.KSUID)
			continue
		}
		if iter.KSUID != want {
			t.Errorf("SeekGE(%s) = %s, want %s", target, iter.KSUID, want)
			continue
		}

		// The located iterator continues through the remaining members.
		next := sort.Search(len(members), func(i int) bool {
			return Compare(members[i], target) >= 0
		}) + 1

		for iter.Next() {
			if next >= len(members) {
				t.Errorf("SeekGE(%s) produced an unexpected trailing member %s", target, iter.KSUID)
				break
			}
			if iter.KSUID != members[next] {
				t.Errorf("SeekGE(%s) continued with %s, want %s", target, iter.KSUID, members[next])
				break
			}
			next++
		}
		if next != len(members) {
			t.Errorf("SeekGE(%s) stopped early, %d members left after %s", target, len(members)-next, iter.KSUID)
		}
	}
}

func lowerBound(members []KSUID, target KSUID) (KSUID, bool) {
	i := sort.Search(len(members), func(i int) bool {
		return Compare(members[i], target) >= 0
	})
	if i == len(members) {
		return Nil, false
	}
	return members[i], true
}

// seekTargets builds a deterministic, boundary-heavy list of targets from the
// members: each member itself, values in between consecutive members, points
// across timestamp boundaries, and values before the first and after the last
// member (Nil and maxKSUID).
func seekTargets(ids []KSUID) []KSUID {
	targets := []KSUID{Nil, maxKSUID()}

	if len(ids) == 0 {
		return targets
	}

	targets = append(targets, beforeFirstTargets(ids[0])...)

	for i := 0; i+1 < len(ids); i++ {
		id := ids[i]
		next := ids[i+1]

		targets = append(targets, id)

		if v, ok := prevKSUID(next); ok {
			targets = append(targets, v)
		}

		targets = append(targets,
			ksuidFromParts(id.Timestamp(), uint128Payload(id)),
			ksuidFromParts(id.Timestamp()+1, uint128Payload(id)),
		)

		if next.Timestamp() != id.Timestamp() {
			targets = append(targets, ksuidFromParts(id.Timestamp(), maxPayload()))
			targets = append(targets, ksuidFromParts(next.Timestamp(), makeUint128(0, 0)))
		}
	}

	last := ids[len(ids)-1]
	targets = append(targets,
		last,
		ksuidFromParts(last.Timestamp(), maxPayload()),
		ksuidFromParts(math.MaxUint32, uint128Payload(last)),
	)

	return dedupeKSUIDs(targets)
}

func dedupeKSUIDs(ids []KSUID) []KSUID {
	Sort(ids)
	out := ids[:0]

	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out
}

func ksuidFromParts(timestamp uint32, payload uint128) KSUID {
	return payload.ksuid(timestamp)
}

func maxPayload() uint128 {
	return makeUint128(math.MaxUint64, math.MaxUint64)
}

// beforeFirstTargets returns KSUIDs ordered just before the first member,
// covering both an earlier timestamp and an earlier payload at the same
// timestamp.
func beforeFirstTargets(first KSUID) []KSUID {
	var targets []KSUID
	if first.Timestamp() != 0 {
		targets = append(targets, ksuidFromParts(first.Timestamp()-1, maxPayload()))
	}
	if v, ok := prevPayload(uint128Payload(first)); ok {
		targets = append(targets, ksuidFromParts(first.Timestamp(), v))
	}
	return targets
}

func prevKSUID(id KSUID) (KSUID, bool) {
	v, ok := prevPayload(uint128Payload(id))
	if !ok {
		return Nil, false
	}
	return ksuidFromParts(id.Timestamp(), v), true
}

func prevPayload(v uint128) (uint128, bool) {
	if v == makeUint128(0, 0) {
		return v, false
	}
	p := v
	p[0]--
	return p, true
}

func maxKSUID() KSUID {
	return ksuidFromParts(math.MaxUint32, makeUint128(math.MaxUint64, math.MaxUint64))
}

func testCompressedSetIndexSparse(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	now := time.Now()

	for _, size := range []int{0, 1, 2, 15, 16, 17, 63, 200} {
		ids := make([]KSUID, 0, size)
		for i := 0; i < size; i++ {
			id, _ := NewRandomWithTime(now.Add(time.Duration(rng.Intn(30)) * time.Second))
			ids = append(ids, id)
		}
		Sort(ids)
		assertSeekGEMatchesIteration(t, ids, seekTargets(dedupeKSUIDs(append([]KSUID(nil), ids...))))
	}
}

func testCompressedSetIndexPacked(t *testing.T) {
	for _, runSize := range []int{1, 2, 10, 65, 300} {
		seq := Sequence{Seed: New()}
		ids := make([]KSUID, runSize)
		for i := range ids {
			ids[i], _ = seq.Next()
		}
		assertSeekGEMatchesIteration(t, ids, seekTargets(ids))
	}
}

func testCompressedSetIndexMixed(t *testing.T) {
	ids := makeIDs(1000, 42)
	assertSeekGEMatchesIteration(t, ids, seekTargets(ids))
}

func testCompressedSetIndexBeforeFirst(t *testing.T) {
	ids := []KSUID{New(), New()}
	Sort(ids)
	index, err := Compress(ids...).Index()
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range beforeFirstTargets(ids[0]) {
		iter := index.SeekGE(target)
		if !iter.Next() || iter.KSUID != ids[0] {
			t.Errorf("SeekGE(%s) did not land on %s", target, ids[0])
		}
	}

	iter := index.SeekGE(Nil)
	if !iter.Next() || iter.KSUID != ids[0] {
		t.Errorf("SeekGE(Nil) did not land on the first member %s", ids[0])
	}
}

func testCompressedSetIndexAfterLast(t *testing.T) {
	ids := []KSUID{New(), New()}
	Sort(ids)
	index, err := Compress(ids...).Index()
	if err != nil {
		t.Fatal(err)
	}

	last := ids[len(ids)-1]
	for _, target := range []KSUID{
		maxKSUID(),
		ksuidFromParts(math.MaxUint32, uint128Payload(last)),
		ksuidFromParts(last.Timestamp(), maxPayload()),
	} {
		if iter := index.SeekGE(target); iter.Next() {
			t.Errorf("SeekGE(%s) returned %s, want end of set", target, iter.KSUID)
		}
	}
}

func testCompressedSetIndexEmpty(t *testing.T) {
	index, err := CompressedSet(nil).Index()
	if err != nil {
		t.Fatalf("indexing an empty set failed: %v", err)
	}

	for _, target := range []KSUID{Nil, New(), maxKSUID()} {
		if iter := index.SeekGE(target); iter.Next() {
			t.Errorf("SeekGE(%s) on an empty set returned %s", target, iter.KSUID)
		}
	}
}

func testCompressedSetIndexLargeRange(t *testing.T) {
	// A payloadRange run spanning 1e9+1 members encoded in a handful of bytes.
	const runLen uint64 = 1000000001
	first := ksuidFromParts(1000000, makeUint128(0, 1000))

	set := buildRangeSet(first, runLen)

	if len(set) > 32 {
		t.Fatalf("large run should occupy a few bytes, set is %d bytes", len(set))
	}

	index, err := set.Index()
	if err != nil {
		t.Fatal(err)
	}
	if len(index.samples) != 1 {
		t.Fatalf("large run is a raw KSUID plus one range record, got %d samples", len(index.samples))
	}

	runFirst := first
	runLast := add128(uint128Payload(first), makeUint128(0, runLen-1)).ksuid(first.Timestamp())

	// Member deep inside the run: found by arithmetic; only the raw KSUID and
	// the single range record are decoded, regardless of the run's 1e9 members.
	target := add128(uint128Payload(first), makeUint128(0, runLen-2)).ksuid(first.Timestamp())
	iter, steps := index.seekGE(target)
	if steps != 2 {
		t.Errorf("seeking inside a %d-member run decoded %d records, want 2", runLen, steps)
	}
	if !iter.Next() || iter.KSUID != target {
		t.Errorf("SeekGE inside the run did not land on %s", target)
	}

	// Boundary checks on the run itself.
	for _, tc := range []struct {
		target KSUID
		want   KSUID
		steps  int
	}{
		{beforeFirstTargets(first)[0], runFirst, 1},
		{runFirst, runFirst, 1},
		{runLast, runLast, 2},
	} {
		iter, steps := index.seekGE(tc.target)
		if steps != tc.steps {
			t.Errorf("SeekGE(%s) decoded %d records, want %d", tc.target, steps, tc.steps)
		}
		if !iter.Next() || iter.KSUID != tc.want {
			t.Errorf("SeekGE(%s) = %v, want %s", tc.target, iter.KSUID, tc.want)
		}
	}

	// After the run, at the same timestamp but a larger payload.
	after := add128(uint128Payload(runLast), makeUint128(0, 1)).ksuid(first.Timestamp())
	if iter := index.SeekGE(after); iter.Next() {
		t.Errorf("SeekGE(%s) after the run returned %s", after, iter.KSUID)
	}

	// After the run, in a later timestamp.
	after = ksuidFromParts(first.Timestamp()+1, makeUint128(0, 0))
	if iter := index.SeekGE(after); iter.Next() {
		t.Errorf("SeekGE(%s) in a later timestamp returned %s", after, iter.KSUID)
	}

	// A fresh seek lands exactly on the first member and streams the run.
	iter = index.SeekGE(runFirst)
	for i := uint64(0); i < 5 && iter.Next(); i++ {
		want := add128(uint128Payload(first), makeUint128(0, i)).ksuid(first.Timestamp())
		if iter.KSUID != want {
			t.Fatalf("iterating from the run start: got %s, want %s", iter.KSUID, want)
		}
	}
}

func testCompressedSetIndexReusable(t *testing.T) {
	ids := dedupeKSUIDs(makeIDs(500, 7))
	index, err := Compress(ids...).Index()
	if err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 3; round++ {
		for _, target := range ids {
			iter := index.SeekGE(target)
			if !iter.Next() || iter.KSUID != target {
				t.Fatalf("round %d: SeekGE(%s) = %v, want %s", round, target, iter.KSUID, target)
			}
		}

		if iter := index.SeekGE(maxKSUID()); iter.Next() {
			t.Fatalf("round %d: seek after the last member returned %s", round, iter.KSUID)
		}
	}

	// Two independent iterators can be active at once.
	a := index.SeekGE(ids[0])
	b := index.SeekGE(ids[len(ids)/2])
	if !a.Next() || a.KSUID != ids[0] {
		t.Fatalf("first iterator was disturbed: got %s", a.KSUID)
	}
	if !b.Next() || b.KSUID != ids[len(ids)/2] {
		t.Fatalf("second iterator was disturbed: got %s", b.KSUID)
	}
}

func testCompressedSetIndexCorrupt(t *testing.T) {
	ids := makeIDs(200, 99)
	set := []byte(Compress(ids...))

	if _, err := NewCompressedSetIndex(set, defaultIndexStride); err != nil {
		t.Fatalf("a well-formed encoding must validate: %v", err)
	}

	// Random garbage never produces a usable index or panics.
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		garbage := make([]byte, 1+rng.Intn(64))
		rng.Read(garbage)
		// Skip the tiny chance of a valid single raw KSUID encoding.
		if len(garbage) == 1+byteLength && garbage[0] == rawKSUID {
			continue
		}
		if _, err := NewCompressedSetIndex(garbage, defaultIndexStride); err == nil {
			t.Fatalf("garbage encoding %d was accepted: % x", i, garbage)
		}
	}

	corrupt := func(mutate func([]byte) []byte) {
		t.Helper()
		b := mutate(append([]byte(nil), set...))
		if _, err := NewCompressedSetIndex(b, defaultIndexStride); err == nil {
			t.Fatalf("corrupt encoding was accepted: % x", b)
		}
	}

	corrupt(func(b []byte) []byte { return b[:len(b)-1] }) // truncated record
	corrupt(func(b []byte) []byte { return b[1:] })        // missing raw KSUID header
	corrupt(func(b []byte) []byte { b[21] = 0xFF; return b })
	corrupt(func(b []byte) []byte { b[0] = 0xFF; return b }) // invalid first tag

	// A zero-length payloadRange record.
	zeroRange := append(
		buildRaw(ksuidFromParts(1, makeUint128(0, 0))),
		payloadRange|1, 0,
	)
	if _, err := NewCompressedSetIndex(zeroRange, defaultIndexStride); err == nil {
		t.Fatal("a zero-length payloadRange was accepted")
	}

	// A payloadDelta that wraps the 128-bit payload space.
	wrap := append(
		buildRaw(ksuidFromParts(1, makeUint128(math.MaxUint64, math.MaxUint64))),
		payloadDelta|1, 1,
	)
	if _, err := NewCompressedSetIndex(wrap, defaultIndexStride); err == nil {
		t.Fatal("a payload overflow delta was accepted")
	}
}

func buildRaw(id KSUID) []byte {
	b := make([]byte, 0, 1+byteLength)
	b = append(b, rawKSUID)
	b = append(b, id[:]...)
	return b
}

// buildRangeSet encodes first followed by a payloadRange run of runLen total
// members without materializing any of the members.
func buildRangeSet(first KSUID, runLen uint64) CompressedSet {
	b := buildRaw(first)
	after := runLen - 1 // members produced after the first
	n := varintLength64(after)
	b = append(b, payloadRange|byte(n))
	b = appendVarint64(b, after, n)
	return CompressedSet(b)
}

// makeIDs produces a sorted, deduplicated set which mixes payload deltas,
// payload ranges, and timestamp deltas.
func makeIDs(n int, seed int64) []KSUID {
	rng := rand.New(rand.NewSource(seed))
	now := time.Unix(1700000000, 0)
	ids := make([]KSUID, 0, n)

	for len(ids) < n {
		ts := uint32(now.Unix()) + uint32(rng.Intn(10))

		switch rng.Intn(3) {
		case 0, 2:
			id := ksuidFromParts(ts, makeUint128(rng.Uint64(), rng.Uint64()))
			ids = append(ids, id)

		case 1:
			payload := makeUint128(rng.Uint64(), rng.Uint64())
			run := 1 + rng.Intn(10)
			for j := 0; j < run && len(ids) < n; j++ {
				ids = append(ids, payload.ksuid(ts))
				payload = incr128(payload)
			}
		}
	}

	Sort(ids)
	return dedupeKSUIDs(ids)
}

// BenchmarkCompressedSetIndexSeekGE shows that seeking inside a run stays
// constant while the number of represented members grows: the work is set by
// the stride and record sizes, not by the number of members.
func BenchmarkCompressedSetIndexSeekGE(b *testing.B) {
	first := ksuidFromParts(1000000, makeUint128(0, 0))
	set := buildRangeSet(first, 1000000001)
	index, err := set.Index()
	if err != nil {
		b.Fatal(err)
	}
	target := add128(uint128Payload(first), makeUint128(0, 1000000000)).ksuid(first.Timestamp())

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		iter := index.SeekGE(target)
		if !iter.Next() || iter.KSUID != target {
			b.Fatal("seek did not land on the target")
		}
	}
}
