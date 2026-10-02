package ksuid

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
	"time"
)

// makeID constructs a KSUID from a timestamp and an arbitrary 16 byte payload.
func makeID(ts uint32, payload ...byte) KSUID {
	var id KSUID
	binary.BigEndian.PutUint32(id[:timestampLengthInBytes], ts)
	copy(id[timestampLengthInBytes:], payload)
	return id
}

// appendRaw appends a raw KSUID record to b.
func appendRaw(b []byte, id KSUID) []byte {
	b = append(b, byte(rawKSUID))
	return append(b, id[:]...)
}

// appendTimeDelta appends a time delta record carrying delta as a size-byte
// big-endian integer followed by the 16 byte payload.
func appendTimeDelta(b []byte, size byte, delta uint64, payload []byte) []byte {
	b = append(b, timeDelta|size)
	for shift := int(size-1) * 8; shift >= 0; shift -= 8 {
		b = append(b, byte(delta>>uint(shift)))
	}
	return append(b, payload...)
}

// appendPayloadDelta appends a payload delta record carrying delta as a
// size-byte big-endian integer.
func appendPayloadDelta(b []byte, size byte, delta ...byte) []byte {
	b = append(b, payloadDelta|size)
	return append(b, delta...)
}

// appendPayloadRange appends a payload range record carrying m as a size-byte
// big-endian integer.
func appendPayloadRange(b []byte, size byte, m ...byte) []byte {
	b = append(b, payloadRange|size)
	return append(b, m...)
}

func strictFixtureIDs() [][]KSUID {
	ts0 := uint32(1000)

	rawOnly := []KSUID{makeID(ts0, 1, 2, 3)}

	rangeOne := []KSUID{
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5),
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 6),
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7),
		makeID(ts0+5, 9),
	}

	deltaIDs := []KSUID{
		makeID(ts0, 1),
		makeID(ts0, 3),
		makeID(ts0, 20),
	}

	mixed := []KSUID{
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 10),
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 11),
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 12),
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 100),
		makeID(ts0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 200),
		makeID(ts0+1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2),
		makeID(ts0+1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3),
		makeID(ts0+3, 7),
	}

	concat := append(append([]KSUID{}, rangeOne...), mixed...)

	// A larger pseudo-random set that naturally produces every record type.
	rng := rand.New(rand.NewSource(42))
	random := make([]KSUID, 500)
	for i := range random {
		var payload [16]byte
		rng.Read(payload[:])
		binary.BigEndian.PutUint32(payload[:], 0) // ensure room for range/delta
		binary.BigEndian.PutUint32(payload[4:], uint32(i%23))
		random[i] = makeID(ts0+uint32(i%37), payload[:]...)
	}

	return [][]KSUID{
		nil,
		rawOnly,
		rangeOne,
		deltaIDs,
		mixed,
		concat,
		random,
	}
}

// collectTags walks a valid compressed set and returns the list of record
// tags it contains.
func collectTags(t *testing.T, set CompressedSet) []int {
	t.Helper()

	tags := []int{}
	content := []byte(set)

	for offset := 0; offset < len(content); {
		b := content[offset]
		tag := int(b) & tagMask
		cnt := int(b) & ^tagMask
		tags = append(tags, tag)
		offset++

		switch tag {
		case rawKSUID:
			offset += byteLength
		case timeDelta:
			offset += cnt + payloadLengthInBytes
		case payloadDelta:
			offset += cnt
		case payloadRange:
			offset += cnt
		default:
			t.Fatalf("unexpected tag %#x in valid fixture", tag)
		}

		if offset > len(content) {
			t.Fatalf("fixture walker overran the encoded set")
		}
	}

	return tags
}

func hasTag(tags []int, tag int) bool {
	for _, found := range tags {
		if found == tag {
			return true
		}
	}
	return false
}

func drainStrict(t *testing.T, set CompressedSet, max uint64) []KSUID {
	t.Helper()

	ids := []KSUID{}
	it := set.IterStrict(max)
	for it.Next() {
		ids = append(ids, it.KSUID)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("unexpected strict iteration error: %v", err)
	}
	return ids
}

func drainLegacy(set CompressedSet) []KSUID {
	ids := []KSUID{}
	for it := set.Iter(); it.Next(); {
		ids = append(ids, it.KSUID)
	}
	return ids
}

func assertReadError(t *testing.T, err error, offset int, record CompressedSetRecord, kind CompressedSetErrorKind) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected %s error at offset %d, got nil", kind, offset)
	}

	readErr, ok := err.(*CompressedSetReadError)
	if !ok {
		t.Fatalf("expected *CompressedSetReadError, got %T: %v", err, err)
	}

	if readErr.Offset != offset || readErr.Record != record || readErr.Kind != kind {
		t.Fatalf("bad read error: got (offset=%d record=%s kind=%s), want (offset=%d record=%s kind=%s)",
			readErr.Offset, readErr.Record, readErr.Kind, offset, record, kind)
	}
}

func TestCompressedSetStrictFixtures(t *testing.T) {
	for i, ids := range strictFixtureIDs() {
		set := Compress(ids...)

		// The strict iterator and the legacy iterator must agree on valid
		// data produced by the encoder.
		strictIDs := drainStrict(t, set, 0)
		legacyIDs := drainLegacy(set)

		if len(strictIDs) != len(legacyIDs) {
			t.Fatalf("fixture %d: strict produced %d ids, legacy %d", i, len(strictIDs), len(legacyIDs))
		}
		for j := range strictIDs {
			if strictIDs[j] != legacyIDs[j] {
				t.Fatalf("fixture %d: id %d mismatch: %s != %s", i, j, strictIDs[j], legacyIDs[j])
			}
		}
	}
}

func TestCompressedSetStrictFixtureTags(t *testing.T) {
	fixtures := strictFixtureIDs()

	// The mixed fixture must exercise every record type.
	mixed := Compress(fixtures[4]...)
	tags := collectTags(t, mixed)
	for _, tag := range []int{rawKSUID, timeDelta, payloadDelta, payloadRange} {
		if !hasTag(tags, tag) {
			t.Fatalf("mixed fixture is missing tag %#x, tags=%v", tag, tags)
		}
	}

	// The range-only fixture exercises payload ranges and time deltas.
	rangeOne := Compress(fixtures[2]...)
	rangeTags := collectTags(t, rangeOne)
	if !hasTag(rangeTags, payloadRange) {
		t.Fatalf("range fixture is missing a payload range record, tags=%v", rangeTags)
	}

	deltaSet := Compress(fixtures[3]...)
	deltaTags := collectTags(t, deltaSet)
	if !hasTag(deltaTags, payloadDelta) {
		t.Fatalf("delta fixture is missing a payload delta record, tags=%v", deltaTags)
	}
}

// TestCompressedSetStrictTruncation truncates every valid encoding one byte
// at a time; strict iteration must never panic and must report a stable
// error for truncated records.
func TestCompressedSetStrictTruncation(t *testing.T) {
	for i, ids := range strictFixtureIDs() {
		set := []byte(Compress(ids...))

		for n := 0; n <= len(set); n++ {
			prefix := CompressedSet(set[:n:n])

			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("fixture %d: strict iterator panicked on %d-byte prefix: %v", i, n, r)
					}
				}()

				got := []KSUID{}
				it := prefix.IterStrict(0)
				for it.Next() {
					got = append(got, it.KSUID)
				}

				// Repeated calls after the terminal state stay stable.
				if it.Next() {
					t.Fatalf("fixture %d: Next returned true again on %d-byte prefix", i, n)
				}

				if err := it.Err(); err != nil {
					if _, ok := err.(*CompressedSetReadError); !ok {
						t.Fatalf("fixture %d: unexpected error type on %d-byte prefix: %T", i, n, err)
					}

					// Every produced id must match the corresponding id of
					// the full set.
					want := drainLegacy(CompressedSet(set))
					if len(got) > len(want) {
						t.Fatalf("fixture %d: prefix produced %d ids, full set only has %d", i, len(got), len(want))
					}
					for j := range got {
						if got[j] != want[j] {
							t.Fatalf("fixture %d: id %d of %d-byte prefix mismatches full set", i, j, n)
						}
					}
				}
			}()
		}
	}
}

func TestCompressedSetStrictMalformed(t *testing.T) {
	baseID := makeID(1000, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16)
	baseBytes := appendRaw(nil, baseID)

	maxPayloadID := makeID(1000,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	maxPayloadBaseBytes := appendRaw(nil, maxPayloadID)

	// base returns an isolated copy so growing a record in one test case can
	// never overwrite the backing array shared by the others.
	base := func() []byte {
		b := make([]byte, len(baseBytes))
		copy(b, baseBytes)
		return b
	}
	maxPayloadBase := func() []byte {
		b := make([]byte, len(maxPayloadBaseBytes))
		copy(b, maxPayloadBaseBytes)
		return b
	}

	tests := []struct {
		name   string
		data   []byte
		offset int
		record CompressedSetRecord
		kind   CompressedSetErrorKind
	}{
		{
			name:   "time delta as first record",
			data:   appendTimeDelta(nil, 1, 1, make([]byte, payloadLengthInBytes)),
			offset: 0,
			record: RecordTimeDelta,
			kind:   ErrorIllegalStart,
		},
		{
			name:   "payload delta as first record",
			data:   appendPayloadDelta(nil, 1, 1),
			offset: 0,
			record: RecordPayloadDelta,
			kind:   ErrorIllegalStart,
		},
		{
			name:   "payload range as first record",
			data:   appendPayloadRange(nil, 1, 1),
			offset: 0,
			record: RecordPayloadRange,
			kind:   ErrorIllegalStart,
		},
		{
			name:   "raw record truncated",
			data:   appendRaw(nil, baseID)[:byteLength],
			offset: 0,
			record: RecordRaw,
			kind:   ErrorTruncated,
		},
		{
			name: "raw record with non-zero count bits",
			data: func() []byte {
				b := appendRaw(nil, baseID)
				b[0] = rawKSUID | 1
				return b
			}(),
			offset: 0,
			record: RecordRaw,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name:   "time delta count too large",
			data:   appendTimeDelta(base(), 5, 1, make([]byte, payloadLengthInBytes)),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorTruncated,
		},
		{
			name:   "time delta count zero",
			data:   append(append(base()[:len(base()):len(base())], timeDelta), make([]byte, payloadLengthInBytes)...),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorTruncated,
		},
		{
			name:   "time delta varint truncated",
			data:   append(base(), timeDelta|2, 1),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorTruncated,
		},
		{
			name:   "time delta payload truncated",
			data:   appendTimeDelta(base(), 1, 1, []byte{1, 2, 3}),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorTruncated,
		},
		{
			name:   "time delta non-canonical encoding",
			data:   appendTimeDelta(base(), 2, 1, make([]byte, payloadLengthInBytes)),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name:   "time delta non-canonical four byte encoding",
			data:   appendTimeDelta(base(), 4, 1, make([]byte, payloadLengthInBytes)),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name:   "time delta zero",
			data:   appendTimeDelta(base(), 1, 0, make([]byte, payloadLengthInBytes)),
			offset: len(base()),
			record: RecordTimeDelta,
			kind:   ErrorZeroDelta,
		},
		{
			name: "time delta overflow",
			data: appendTimeDelta(
				appendRaw(nil, makeID(math.MaxUint32-1)),
				1, 2, make([]byte, payloadLengthInBytes)),
			offset: byteLength + 1,
			record: RecordTimeDelta,
			kind:   ErrorOverflow,
		},
		{
			name:   "payload delta count too large",
			data:   appendPayloadDelta(base(), 17, 1),
			offset: len(base()),
			record: RecordPayloadDelta,
			kind:   ErrorTruncated,
		},
		{
			name:   "payload delta truncated",
			data:   appendPayloadDelta(base(), 2, 1),
			offset: len(base()),
			record: RecordPayloadDelta,
			kind:   ErrorTruncated,
		},
		{
			name:   "payload delta non-canonical encoding",
			data:   appendPayloadDelta(base(), 2, 0, 1),
			offset: len(base()),
			record: RecordPayloadDelta,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name: "payload delta non-canonical sixteen byte encoding",
			data: appendPayloadDelta(base(), 16,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1),
			offset: len(base()),
			record: RecordPayloadDelta,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name:   "payload delta zero",
			data:   appendPayloadDelta(base(), 1, 0),
			offset: len(base()),
			record: RecordPayloadDelta,
			kind:   ErrorZeroDelta,
		},
		{
			name:   "payload delta overflow",
			data:   appendPayloadDelta(maxPayloadBase(), 1, 1),
			offset: len(maxPayloadBase()),
			record: RecordPayloadDelta,
			kind:   ErrorOverflow,
		},
		{
			name:   "payload range count too large",
			data:   appendPayloadRange(base(), 9, 1),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorTruncated,
		},
		{
			name:   "payload range truncated",
			data:   appendPayloadRange(base(), 2, 1),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorTruncated,
		},
		{
			name:   "payload range non-canonical encoding",
			data:   appendPayloadRange(base(), 2, 0, 1),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name:   "payload range non-canonical three byte encoding",
			data:   appendPayloadRange(base(), 3, 0, 0, 1),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorNonCanonicalLength,
		},
		{
			name:   "payload range zero",
			data:   appendPayloadRange(base(), 1, 0),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorZeroRange,
		},
		{
			name:   "payload range overflow with length one",
			data:   appendPayloadRange(maxPayloadBase(), 1, 1),
			offset: len(maxPayloadBase()),
			record: RecordPayloadRange,
			kind:   ErrorOverflow,
		},
		{
			name: "payload range overflow with large length",
			data: appendPayloadRange(maxPayloadBase(), 8,
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF),
			offset: len(maxPayloadBase()),
			record: RecordPayloadRange,
			kind:   ErrorOverflow,
		},
		{
			name: "huge payload range rejected before expansion",
			data: appendPayloadRange(base(), 8,
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorElementLimit,
		},
		{
			name:   "payload range exceeding an exact limit rejected before expansion",
			data:   appendPayloadRange(base(), 1, 6),
			offset: len(base()),
			record: RecordPayloadRange,
			kind:   ErrorElementLimit,
		},
		{
			name: "element limit reached on a non-range record",
			data: func() []byte {
				b := appendRaw(nil, baseID)
				b = appendPayloadRange(b, 1, 3)
				b = appendRaw(b, makeID(1001))
				return b
			}(),
			offset: func() int {
				b := appendRaw(nil, baseID)
				b = appendPayloadRange(b, 1, 3)
				return len(b)
			}(),
			record: RecordRaw,
			kind:   ErrorElementLimit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("strict iterator panicked: %v", r)
					}
				}()

				it := CompressedSet(test.data).IterStrict(4)
				for it.Next() {
				}

				assertReadError(t, it.Err(), test.offset, test.record, test.kind)

				// The first error is sticky and repeated Next calls are
				// stable.
				if it.Next() {
					t.Fatal("Next returned true after an error")
				}
				assertReadError(t, it.Err(), test.offset, test.record, test.kind)
			}()
		})
	}
}

// TestCompressedSetStrictHugeRangeFast ensures that a gigantic payload range
// is rejected by the element limit without materializing any of its members.
func TestCompressedSetStrictHugeRangeFast(t *testing.T) {
	id := makeID(1000)
	data := appendRaw(nil, id)
	data = appendPayloadRange(data, 8,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)

	start := time.Now()

	produced := 0
	it := CompressedSet(data).IterStrict(1000)
	for it.Next() {
		produced++
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("iterator spent %s expanding a huge range", elapsed)
	}
	if produced != 1 {
		t.Fatalf("expected only the raw id to be produced, got %d", produced)
	}
	assertReadError(t, it.Err(), len(appendRaw(nil, id)), RecordPayloadRange, ErrorElementLimit)
}

// TestCompressedSetStrictHugeRangeNoLimitFast checks that even without an
// element limit the payload overflow of a max-width range near the top of the
// payload space is detected
// immediately instead of expanding the range.
func TestCompressedSetStrictHugeRangeNoLimitFast(t *testing.T) {
	maxID := makeID(1000,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	data := appendRaw(nil, maxID)
	data = appendPayloadRange(data, 8,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)

	start := time.Now()

	produced := 0
	it := CompressedSet(data).IterStrict(0)
	for it.Next() {
		produced++
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("iterator spent %s expanding an overflowing range", elapsed)
	}
	if produced != 1 {
		t.Fatalf("expected only the raw id to be produced, got %d", produced)
	}

	err := it.Err()
	if readErr, ok := err.(*CompressedSetReadError); !ok || readErr.Kind != ErrorOverflow {
		t.Fatalf("expected an overflow error, got %v", err)
	}
}

func TestCompressedSetStrictLimitExact(t *testing.T) {
	ids := strictFixtureIDs()[4]
	set := Compress(ids...)

	all := drainStrict(t, set, 0)

	// A limit exactly equal to the element count must accept the whole set.
	if got := drainStrict(t, set, uint64(len(all))); len(got) != len(all) {
		t.Fatalf("exact limit produced %d ids, want %d", len(got), len(all))
	}

	// One less must surface an element limit error on the offending record.
	it := set.IterStrict(uint64(len(all) - 1))
	for it.Next() {
	}
	if err := it.Err(); err == nil {
		t.Fatal("expected an element limit error")
	} else if readErr, ok := err.(*CompressedSetReadError); !ok || readErr.Kind != ErrorElementLimit {
		t.Fatalf("expected element limit error, got %v", err)
	}
}

func TestCompressedSetStrictTerminalStable(t *testing.T) {
	set := Compress(strictFixtureIDs()[4]...)

	t.Run("end of set", func(t *testing.T) {
		it := set.IterStrict(0)

		count := 0
		for it.Next() {
			count++
		}
		if count == 0 {
			t.Fatal("set produced no ids")
		}
		if err := it.Err(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for i := 0; i < 3; i++ {
			if it.Next() {
				t.Fatal("Next returned true after reaching the end of the set")
			}
			if err := it.Err(); err != nil {
				t.Fatalf("Err changed to %v after the end of the set", err)
			}
		}
	})

	t.Run("empty set", func(t *testing.T) {
		it := CompressedSet(nil).IterStrict(0)

		for i := 0; i < 3; i++ {
			if it.Next() {
				t.Fatal("Next returned true on an empty set")
			}
			if err := it.Err(); err != nil {
				t.Fatalf("unexpected error on empty set: %v", err)
			}
		}
	})

	t.Run("malformed set", func(t *testing.T) {
		// Time delta with a bogus count bits, no following payload.
		data := append(appendRaw(nil, makeID(1)), timeDelta|3, 0)

		it := CompressedSet(data).IterStrict(0)

		if it.Next() {
			if it.Next() {
				t.Fatal("Next returned true on a malformed record")
			}
		}

		first := it.Err()
		if first == nil {
			t.Fatal("expected an error on malformed data")
		}

		for i := 0; i < 3; i++ {
			if it.Next() {
				t.Fatal("Next returned true after a malformed record")
			}
			if it.Err() != first {
				t.Fatalf("Err changed: %v != %v", it.Err(), first)
			}
		}
	})
}

func TestCompressedSetLegacyInvalidTagPanics(t *testing.T) {
	// The legacy iterator keeps its historical behavior on malformed tags.
	data := []byte{0x1F}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected the legacy iterator to panic on a malformed tag")
		}
	}()

	for it := CompressedSet(data).Iter(); it.Next(); {
	}
}

func TestCompressedSetStrictDoesNotSwallowError(t *testing.T) {
	// A non-canonical varint followed by otherwise valid bytes: the strict
	// iterator must stop at the first defect and never return KSUIDs decoded
	// past it.
	id := makeID(1000)
	data := appendRaw(nil, id)
	data = appendTimeDelta(data, 2, 1, make([]byte, payloadLengthInBytes))

	it := CompressedSet(data).IterStrict(0)

	ids := 0
	for it.Next() {
		ids++
	}

	if ids != 1 {
		t.Fatalf("expected iteration to stop after the raw id, got %d ids", ids)
	}
	assertReadError(t, it.Err(), byteLength+1, RecordTimeDelta, ErrorNonCanonicalLength)
}
