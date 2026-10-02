package ksuid

import (
	"encoding/binary"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestCompressedSetIndex(t *testing.T) {
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "SeekGE matches the lower bound found by plain iteration",
			function: testIndexSeekGECrossCheck,
		},
		{
			scenario: "SeekGE has deterministic results at the set boundaries",
			function: testIndexSeekGEBoundaries,
		},
		{
			scenario: "SeekGE on an empty set is immediately exhausted",
			function: testIndexSeekGEEmpty,
		},
		{
			scenario: "SeekGE work depends on the encoding and index size, not the range length",
			function: testIndexSeekGEBigRange,
		},
		{
			scenario: "building the index rejects malformed encodings",
			function: testIndexMalformed,
		},
		{
			scenario: "copies of the index share the immutable encoding",
			function: testIndexCopy,
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, test.function)
	}
}

func testIndexSeekGECrossCheck(t *testing.T) {
	now := time.Now()

	sparse := make([]KSUID, 200)
	for i := range sparse {
		sparse[i], _ = NewRandomWithTime(now.Add(time.Duration(i%10) * time.Second))
	}

	sequence := Sequence{Seed: New()}
	packed := make([]KSUID, 300)
	for i := range packed {
		var err error
		if packed[i], err = sequence.Next(); err != nil {
			t.Fatal(err)
		}
	}

	mixed := make([]KSUID, 0, len(sparse)+len(packed))
	mixed = append(mixed, sparse[:100]...)
	mixed = append(mixed, packed[:100]...)
	mixed = append(mixed, sparse[100:]...)
	mixed = append(mixed, packed[100:]...)

	sets := map[string][]KSUID{
		"sparse": sparse,
		"packed": packed,
		"mixed":  mixed,
		"single": {New()},
	}

	for name, ids := range sets {
		t.Run(name, func(t *testing.T) {
			set := Compress(ids...)
			members := collectSet(set)

			targets := []KSUID{Nil, Max}
			for _, id := range members {
				targets = append(targets, id, id.Next(), id.Prev())
			}

			for _, stride := range []int{0, 1, 2, 3, 7, 64} {
				idx, err := set.Index(stride)
				if err != nil {
					t.Fatalf("stride %d: %v", stride, err)
				}

				for _, target := range targets {
					got := collectSeek(idx, target)
					want := lowerBoundSuffix(members, target)

					if !equalKSUIDs(got, want) {
						t.Errorf("stride %d, target %s: got %d ids, want %d", stride, target, len(got), len(want))
						for i := 0; i < len(got) && i < len(want); i++ {
							if got[i] != want[i] {
								t.Errorf("first mismatch at index %d: got %s, want %s", i, got[i], want[i])
								break
							}
						}
					}
				}
			}
		})
	}
}

func testIndexSeekGEBoundaries(t *testing.T) {
	const ts1 = uint32(1600000000)
	const ts2 = ts1 + 3600

	makeIDs := func(ts uint32, payloads ...uint64) []KSUID {
		ids := make([]KSUID, len(payloads))
		for i, p := range payloads {
			binary.BigEndian.PutUint32(ids[i][:timestampLengthInBytes], ts)
			binary.BigEndian.PutUint64(ids[i][byteLength-8:], p)
		}
		return ids
	}

	ids := append(makeIDs(ts1, 10, 20, 30), makeIDs(ts2, 5, 15, 25)...)
	set := Compress(ids...)

	idx, err := set.Index(1)
	if err != nil {
		t.Fatal(err)
	}

	between := makeIDs(ts1+1800, 0)[0]
	betweenMax := makeIDs(ts1+1800, ^uint64(0))[0]
	insideGap := makeIDs(ts2, 10)[0]
	pastGroup := makeIDs(ts1, 35)[0]

	tests := []struct {
		target KSUID
		want   []KSUID
	}{
		{Nil, ids},            // before the first member
		{ids[0].Prev(), ids},  // just before the first member
		{ids[0], ids},         // exactly the first member
		{ids[5], ids[5:]},     // exactly the last member
		{ids[5].Next(), nil},  // just after the last member
		{Max, nil},            // after the last member
		{between, ids[3:]},    // timestamp between two groups
		{betweenMax, ids[3:]}, // max payload between two groups
		{insideGap, ids[4:]},  // payload gap within a timestamp
		{pastGroup, ids[3:]},  // past the payloads of a timestamp
	}

	for _, test := range tests {
		if got := collectSeek(idx, test.target); !equalKSUIDs(got, test.want) {
			t.Errorf("target %s: got %v, want %v", test.target, got, test.want)
		}
	}
}

func testIndexSeekGEEmpty(t *testing.T) {
	for _, set := range []CompressedSet{nil, Compress()} {
		idx, err := set.Index(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []KSUID{Nil, New(), Max} {
			if it := idx.SeekGE(target); it.Next() {
				t.Errorf("expected exhausted iterator, got %s", it.KSUID)
			}
		}
	}
}

func testIndexSeekGEBigRange(t *testing.T) {
	// A range of more than a trillion consecutive ids, encoded in a few
	// bytes: one raw KSUID followed by a single payload range record.
	const rangeLen = uint64(1) << 40

	var seed KSUID
	binary.BigEndian.PutUint32(seed[:timestampLengthInBytes], 1500000000)

	set := CompressedSet{byte(rawKSUID)}
	set = append(set, seed[:]...)
	n := varintLength64(rangeLen)
	set = append(set, payloadRange|byte(n))
	set = appendVarint64(set, rangeLen, n)

	if len(set) > 32 {
		t.Fatalf("a range of %d ids should compress to a few bytes, got %d", rangeLen, len(set))
	}

	idx, err := set.Index(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.samples) != 1 {
		t.Fatalf("expected a single index sample, got %d", len(idx.samples))
	}

	// If the seek stepped through the range values one by one, this test
	// could never terminate: completing it at all demonstrates that the
	// amount of work is a function of the encoding and index sizes.
	mkid := func(payload uint64) KSUID {
		var id KSUID
		binary.BigEndian.PutUint32(id[:timestampLengthInBytes], 1500000000)
		binary.BigEndian.PutUint64(id[byteLength-8:], payload)
		return id
	}

	// Seek to the exact middle of the range.
	mid := mkid(rangeLen / 2)
	it := idx.SeekGE(mid)
	if !it.Next() || it.KSUID != mid {
		t.Fatalf("expected %s, got %s", mid, it.KSUID)
	}
	if !it.Next() || it.KSUID != mid.Next() {
		t.Fatalf("expected %s, got %s", mid.Next(), it.KSUID)
	}

	// Seek to the last value of the range.
	last := mkid(rangeLen)
	it = idx.SeekGE(last)
	if !it.Next() || it.KSUID != last {
		t.Fatalf("expected %s, got %s", last, it.KSUID)
	}
	if it.Next() {
		t.Errorf("expected exhausted iterator, got %s", it.KSUID)
	}

	// Seek past the end of the range.
	if it := idx.SeekGE(last.Next()); it.Next() {
		t.Errorf("expected exhausted iterator, got %s", it.KSUID)
	}

	// Seek before the first value of the set.
	it = idx.SeekGE(mkid(0))
	if !it.Next() || it.KSUID != seed {
		t.Fatalf("expected %s, got %s", seed, it.KSUID)
	}
}

func testIndexMalformed(t *testing.T) {
	id := New()
	raw := append([]byte{byte(rawKSUID)}, id[:]...)
	record := func(tag byte, tail ...byte) CompressedSet {
		return append(append(append([]byte{}, raw...), tag), tail...)
	}

	tests := []struct {
		name string
		set  CompressedSet
	}{
		{"truncated raw ksuid", CompressedSet{byte(rawKSUID), 0x01, 0x02}},
		{"time delta with zero length", record(timeDelta, make([]byte, payloadLengthInBytes)...)},
		{"time delta with invalid length", record(timeDelta|5, make([]byte, 5+payloadLengthInBytes)...)},
		{"truncated time delta", record(timeDelta|2, 0x01)},
		{"payload delta with zero length", record(payloadDelta)},
		{"payload delta with invalid length", record(payloadDelta|17, make([]byte, 17)...)},
		{"truncated payload delta", record(payloadDelta|4, 0x01)},
		{"payload range with zero length", record(payloadRange)},
		{"payload range with invalid length", record(payloadRange|9, make([]byte, 9)...)},
		{"truncated payload range", record(payloadRange|8, 0x01)},
		{"empty payload range", record(payloadRange|1, 0x00)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.set.Index(0); !errors.Is(err, ErrMalformedCompressedSet) {
				t.Errorf("expected ErrMalformedCompressedSet, got %v", err)
			}
		})
	}

	// Valid and empty sets are accepted.
	if _, err := Compress(id, New(), New()).Index(0); err != nil {
		t.Error(err)
	}
	if _, err := CompressedSet(nil).Index(0); err != nil {
		t.Error(err)
	}
}

func testIndexCopy(t *testing.T) {
	ids := make([]KSUID, 100)
	for i := range ids {
		ids[i] = New()
	}

	set := Compress(ids...)
	idx, err := set.Index(3)
	if err != nil {
		t.Fatal(err)
	}

	// Copies of the index value share the immutable sample table and set
	// encoding, and must behave identically.
	cp := idx

	for _, target := range []KSUID{Nil, ids[0], ids[50], ids[99], Max} {
		if got, want := collectSeek(cp, target), collectSeek(idx, target); !equalKSUIDs(got, want) {
			t.Errorf("target %s: copy returned %v, original returned %v", target, got, want)
		}
	}
}

func collectSet(set CompressedSet) []KSUID {
	ids := []KSUID{}
	for it := set.Iter(); it.Next(); {
		ids = append(ids, it.KSUID)
	}
	return ids
}

func collectSeek(idx CompressedSetIndex, target KSUID) []KSUID {
	ids := []KSUID{}
	for it := idx.SeekGE(target); it.Next(); {
		ids = append(ids, it.KSUID)
	}
	return ids
}

func lowerBoundSuffix(members []KSUID, target KSUID) []KSUID {
	i := sort.Search(len(members), func(i int) bool {
		return Compare(members[i], target) >= 0
	})
	return members[i:]
}

func equalKSUIDs(a, b []KSUID) bool {
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
