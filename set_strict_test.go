package ksuid

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func TestCompressedSetStrict(t *testing.T) {
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "strict iteration matches regular iteration on valid sets",
			function: testCompressedSetStrictValid,
		},
		{
			scenario: "byte-by-byte truncation of valid encodings never panics",
			function: testCompressedSetStrictTruncation,
		},
		{
			scenario: "malformed encodings are rejected with offset and type",
			function: testCompressedSetStrictMalformed,
		},
		{
			scenario: "element count limit is enforced before expanding ranges",
			function: testCompressedSetStrictMaxElements,
		},
		{
			scenario: "calling Next after the terminal state remains stable",
			function: testCompressedSetStrictTerminalStable,
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, test.function)
	}
}

// makeStrictTestSet builds a compressed set exercising all four instruction
// types (raw, time delta, payload delta, payload range).
func makeStrictTestSet(t *testing.T) (CompressedSet, []KSUID) {
	t.Helper()
	now := time.Now()

	times := [10]time.Time{}
	for i := range times {
		times[i] = now.Add(time.Duration(i) * 2 * time.Second)
	}

	sequences := [20]Sequence{}
	for i := range sequences {
		seed, _ := NewRandomWithTime(times[i%len(times)])
		sequences[i] = Sequence{Seed: seed}
	}

	ksuids := [500]KSUID{}
	for i := range ksuids {
		ksuids[i], _ = sequences[i%len(sequences)].Next()
	}

	set := Compress(ksuids[:]...)
	// Append a second section to also exercise raw instructions appearing in
	// the middle of the encoding.
	set = AppendCompressed(set, ksuids[:20]...)

	return set, ksuids[:]
}

func testCompressedSetStrictValid(t *testing.T) {
	set, _ := makeStrictTestSet(t)

	it1 := set.Iter()
	it2 := set.StrictIter(0)

	count := uint64(0)
	for {
		ok1 := it1.Next()
		ok2 := it2.Next()
		if ok1 != ok2 {
			t.Fatalf("iterators diverged: regular=%v strict=%v", ok1, ok2)
		}
		if !ok1 {
			break
		}
		if it1.KSUID != it2.KSUID {
			t.Fatalf("KSUID mismatch at index %d: %s != %s", count, it1.KSUID, it2.KSUID)
		}
		count++
	}

	if err := it2.Err(); err != nil {
		t.Fatalf("strict iterator reported an error on a valid set: %v", err)
	}

	if _, err := ParseCompressedSet(set, 0); err != nil {
		t.Fatalf("ParseCompressedSet rejected a valid set: %v", err)
	}

	// A limit exactly equal to the element count must be accepted, one less
	// must be rejected.
	if _, err := ParseCompressedSet(set, count); err != nil {
		t.Errorf("ParseCompressedSet with limit=%d rejected a set of %d elements: %v", count, count, err)
	}
	if _, err := ParseCompressedSet(set, count-1); err == nil {
		t.Errorf("ParseCompressedSet with limit=%d accepted a set of %d elements", count-1, count)
	}
}

func testCompressedSetStrictTruncation(t *testing.T) {
	set, _ := makeStrictTestSet(t)

	for i := 0; i < len(set); i++ {
		sub := set[:i]

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("strict iteration panicked on truncation to %d bytes: %v", i, r)
				}
			}()

			it := sub.StrictIter(0)
			for it.Next() {
			}

			if err := it.Err(); err != nil {
				cerr, ok := err.(*CompressedSetError)
				if !ok {
					t.Fatalf("truncation to %d bytes: error is %T, not *CompressedSetError", i, err)
				}
				if cerr.Offset < 0 || cerr.Offset >= len(sub) {
					t.Errorf("truncation to %d bytes: error offset %d out of bounds", i, cerr.Offset)
				}
			}

			// ParseCompressedSet must agree with the iterator and never
			// panic either.
			_, perr := ParseCompressedSet(sub, 0)
			if (perr == nil) != (it.Err() == nil) {
				t.Errorf("truncation to %d bytes: iterator err=%v but parse err=%v", i, it.Err(), perr)
			}
		}()
	}
}

// rawInstruction builds an encoding starting with a raw KSUID made of the
// given timestamp and 128-bit payload.
func rawInstruction(ts uint32, hi, lo uint64) []byte {
	b := make([]byte, 1+byteLength)
	b[0] = rawKSUID
	binary.BigEndian.PutUint32(b[1:5], ts)
	binary.BigEndian.PutUint64(b[5:13], hi)
	binary.BigEndian.PutUint64(b[13:21], lo)
	return b
}

func testCompressedSetStrictMalformed(t *testing.T) {
	base := rawInstruction(1000, 0x0102030405060708, 0x090A0B0C0D0E0F10)
	payload := base[5:21] // a valid 16-byte payload to append after time deltas

	appendTo := func(b []byte, more ...byte) []byte {
		c := make([]byte, 0, len(b)+len(more))
		c = append(c, b...)
		return append(c, more...)
	}

	tests := []struct {
		name   string
		data   []byte
		offset int
		tag    byte
	}{
		{
			name:   "time delta before any raw KSUID",
			data:   appendTo([]byte{timeDelta | 1, 0x01}, payload...),
			offset: 0,
			tag:    timeDelta,
		},
		{
			name:   "payload delta before any raw KSUID",
			data:   []byte{payloadDelta | 1, 0x01},
			offset: 0,
			tag:    payloadDelta,
		},
		{
			name:   "payload range before any raw KSUID",
			data:   []byte{payloadRange | 1, 0x01},
			offset: 0,
			tag:    payloadRange,
		},
		{
			name:   "raw instruction with non-zero length bits",
			data:   rawInstruction(1000, 1, 2),
			offset: 0,
			tag:    rawKSUID,
		},
		{
			name:   "truncated raw KSUID",
			data:   base[:10],
			offset: 0,
			tag:    rawKSUID,
		},
		{
			name:   "zero time delta",
			data:   appendTo(appendTo(base, timeDelta|1, 0x00), payload...),
			offset: 21,
			tag:    timeDelta,
		},
		{
			name:   "non-minimal time delta",
			data:   appendTo(appendTo(base, timeDelta|2, 0x00, 0x2A), payload...),
			offset: 21,
			tag:    timeDelta,
		},
		{
			name:   "invalid time delta length",
			data:   appendTo(appendTo(base, timeDelta|5, 1, 2, 3, 4, 5), payload...),
			offset: 21,
			tag:    timeDelta,
		},
		{
			name:   "truncated time delta payload",
			data:   appendTo(base, timeDelta|1, 0x01, 0x00, 0x00),
			offset: 21,
			tag:    timeDelta,
		},
		{
			name:   "timestamp overflow",
			data:   appendTo(appendTo(rawInstruction(math.MaxUint32, 1, 2), timeDelta|1, 0x01), payload...),
			offset: 21,
			tag:    timeDelta,
		},
		{
			name:   "zero payload delta",
			data:   appendTo(base, payloadDelta|1, 0x00),
			offset: 21,
			tag:    payloadDelta,
		},
		{
			name:   "non-minimal payload delta",
			data:   appendTo(base, payloadDelta|2, 0x00, 0x2A),
			offset: 21,
			tag:    payloadDelta,
		},
		{
			name:   "invalid payload delta length",
			data:   appendTo(base, payloadDelta|17, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17),
			offset: 21,
			tag:    payloadDelta,
		},
		{
			name:   "truncated payload delta",
			data:   appendTo(base, payloadDelta|4, 0x01),
			offset: 21,
			tag:    payloadDelta,
		},
		{
			name:   "payload delta overflow",
			data:   appendTo(rawInstruction(1000, math.MaxUint64, math.MaxUint64), payloadDelta|1, 0x01),
			offset: 21,
			tag:    payloadDelta,
		},
		{
			name:   "zero payload range",
			data:   appendTo(base, payloadRange|1, 0x00),
			offset: 21,
			tag:    payloadRange,
		},
		{
			name:   "non-minimal payload range",
			data:   appendTo(base, payloadRange|2, 0x00, 0x05),
			offset: 21,
			tag:    payloadRange,
		},
		{
			name:   "invalid payload range length",
			data:   appendTo(base, payloadRange|9, 1, 2, 3, 4, 5, 6, 7, 8, 9),
			offset: 21,
			tag:    payloadRange,
		},
		{
			name:   "truncated payload range",
			data:   appendTo(base, payloadRange|2, 0x01),
			offset: 21,
			tag:    payloadRange,
		},
		{
			name:   "payload range overflow",
			data:   appendTo(rawInstruction(1000, math.MaxUint64, math.MaxUint64-1), payloadRange|1, 0x02),
			offset: 21,
			tag:    payloadRange,
		},
	}

	// The "raw with non-zero length bits" case needs a handcrafted first
	// byte; fix it up here.
	tests[3].data[0] = rawKSUID | 5

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			it := CompressedSet(test.data).StrictIter(0)
			for it.Next() {
			}

			err := it.Err()
			if err == nil {
				t.Fatal("strict iterator accepted a malformed encoding")
			}

			cerr, ok := err.(*CompressedSetError)
			if !ok {
				t.Fatalf("error is %T, not *CompressedSetError", err)
			}
			if cerr.Offset != test.offset {
				t.Errorf("error offset: got %d, want %d", cerr.Offset, test.offset)
			}
			if cerr.Tag != test.tag {
				t.Errorf("error tag: got %#02x, want %#02x", cerr.Tag, test.tag)
			}

			if _, err := ParseCompressedSet(test.data, 0); err == nil {
				t.Error("ParseCompressedSet accepted a malformed encoding")
			} else if _, ok := err.(*CompressedSetError); !ok {
				t.Errorf("ParseCompressedSet error is %T, not *CompressedSetError", err)
			}
		})
	}
}

func testCompressedSetStrictMaxElements(t *testing.T) {
	// A range of math.MaxUint64 elements: the limit must be enforced before
	// the range is expanded, so this test completes instantly instead of
	// generating billions of ids.
	huge := rawInstruction(1000, 0, 0)
	huge = append(huge, payloadRange|8)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], math.MaxUint64)
	huge = append(huge, c[:]...)

	it := CompressedSet(huge).StrictIter(100)
	for it.Next() {
	}

	err := it.Err()
	if err == nil {
		t.Fatal("strict iterator accepted a range exceeding the element limit")
	}
	cerr, ok := err.(*CompressedSetError)
	if !ok {
		t.Fatalf("error is %T, not *CompressedSetError", err)
	}
	if cerr.Offset != 21 || cerr.Tag != payloadRange {
		t.Errorf("unexpected error location: offset=%d tag=%#02x", cerr.Offset, cerr.Tag)
	}

	if _, err := ParseCompressedSet(huge, 100); err == nil {
		t.Error("ParseCompressedSet accepted a range exceeding the element limit")
	}

	// A range of exactly the limit must be accepted and produce the right
	// number of ids.
	seq := Sequence{Seed: New()}
	ids := make([]KSUID, 10)
	for i := range ids {
		ids[i], _ = seq.Next()
	}
	set := Compress(ids...)

	n := 0
	for it := set.StrictIter(10); it.Next(); {
		if it.KSUID != ids[n] {
			t.Errorf("mismatched id at index %d: %s != %s", n, it.KSUID, ids[n])
		}
		n++
	}
	if n != 10 {
		t.Errorf("expected 10 ids, got %d", n)
	}

	if _, err := ParseCompressedSet(set, 9); err == nil {
		t.Error("ParseCompressedSet with limit 9 accepted a set of 10 elements")
	}
}

func testCompressedSetStrictTerminalStable(t *testing.T) {
	// Natural end of the set.
	set := Compress(New(), New(), New())
	it := set.StrictIter(0)
	for it.Next() {
	}
	last := it.KSUID
	for i := 0; i < 3; i++ {
		if it.Next() {
			t.Error("Next returned true after the end of the set")
		}
		if it.Err() != nil {
			t.Errorf("Err changed after the end of the set: %v", it.Err())
		}
		if it.KSUID != last {
			t.Error("KSUID changed after the end of the set")
		}
	}

	// Error termination: the first error must be preserved, not swallowed.
	bad := rawInstruction(1000, 1, 2)
	bad = append(bad, payloadDelta|1, 0x00) // zero payload delta

	it2 := CompressedSet(bad).StrictIter(0)
	for it2.Next() {
	}
	err := it2.Err()
	if err == nil {
		t.Fatal("expected an error on the malformed set")
	}
	for i := 0; i < 3; i++ {
		if it2.Next() {
			t.Error("Next returned true after an error")
		}
		if it2.Err() != err {
			t.Errorf("Err changed after the error: %v != %v", it2.Err(), err)
		}
	}
}
