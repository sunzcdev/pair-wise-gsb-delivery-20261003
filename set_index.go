package ksuid

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrMalformedCompressedSet is returned when an index is constructed from a
// byte slice which is not a valid encoding of a sorted CompressedSet.
var ErrMalformedCompressedSet = errors.New("malformed compressed set")

// defaultIndexStride is the default distance, measured in encoded records,
// between two samples of a CompressedSetIndex.
const defaultIndexStride = 16

// CompressedSetIndex is a reusable seek index bound to an immutable
// CompressedSet. It samples only the boundaries of the encoded records and the
// state required to resume decoding deltas from a boundary; the members of the
// set are never decompressed or stored by the index.
//
// The index borrows the byte slice of the CompressedSet it was created from:
// the CompressedSet is immutable and must not be modified (including through an
// aliased slice handed to AppendCompressed) for as long as the index is used.
// The index itself is read-only after construction, so SeekGE may be called
// concurrently and any number of times on the same index; each call returns an
// independent CompressedSetIter which is not safe to share between goroutines.
type CompressedSetIndex struct {
	content []byte
	stride  int
	samples []setIndexSample
}

// setIndexSample is a checkpoint at the boundary of an encoded record: it
// stores the first member produced when decoding starts at offset, and the
// iterator state (timestamp and last payload) from which subsequent deltas can
// be resumed. Samples are only placed at record boundaries, so no in-progress
// payloadRange state needs to be saved.
type setIndexSample struct {
	ksuid     KSUID
	offset    int
	timestamp uint32
	lastValue uint128
}

// Index builds a reusable seek index over the set.
//
// The set is fully validated while the index is constructed, so a malformed
// encoding is reported here rather than surfacing as corrupt results or
// iterator panics during later queries. SeekGE requires the set to be sorted;
// sets produced by a single call to Compress or AppendCompressed always are,
// but concatenating independent sets with AppendCompressed is not supported by
// the index unless the union is globally sorted.
//
// The returned index borrows the set, see CompressedSetIndex for the ownership
// contract.
func (set CompressedSet) Index() (*CompressedSetIndex, error) {
	return NewCompressedSetIndex([]byte(set), defaultIndexStride)
}

// NewCompressedSetIndex builds a seek index over a compressed set, sampling an
// entry every stride encoded records. The work done by SeekGE is bounded by the
// stride and the size of the sampled records: it does not depend on the number
// of members in the set.
//
// The index borrows content, which must remain unchanged for the lifetime of
// the index.
func NewCompressedSetIndex(content []byte, stride int) (*CompressedSetIndex, error) {
	if stride <= 0 {
		stride = defaultIndexStride
	}

	index := &CompressedSetIndex{
		content: content,
		stride:  stride,
	}

	if len(content) == 0 {
		return index, nil
	}

	samples := make([]setIndexSample, 0, len(content)/byteLength+1)

	var (
		offset    int
		timestamp uint32
		lastValue uint128
		prevLast  KSUID
		seenFirst bool
	)

	for record := 0; offset != len(content); record++ {
		startOffset := offset
		startTimestamp := timestamp
		startLastValue := lastValue

		rec, err := parseCompressedSetRecord(content, offset, timestamp, lastValue)
		if err != nil {
			return nil, err
		}

		if !seenFirst {
			// The encoding always starts with a raw KSUID, which anchors the
			// delta chain.
			if rec.tag != rawKSUID {
				return nil, ErrMalformedCompressedSet
			}
			seenFirst = true
		} else if Compare(rec.ksuid, prevLast) <= 0 {
			// Records must produce strictly increasing members; duplicates and
			// out-of-order encodings have no deterministic seek result.
			return nil, ErrMalformedCompressedSet
		}

		if record%stride == 0 {
			samples = append(samples, setIndexSample{
				ksuid:     rec.ksuid,
				offset:    startOffset,
				timestamp: startTimestamp,
				lastValue: startLastValue,
			})
		}

		offset = rec.next
		timestamp = rec.timestamp
		lastValue = rec.lastValue
		prevLast = lastValue.ksuid(timestamp)
	}

	index.samples = samples
	return index, nil
}

// SeekGE locates the first member of the set that is greater than or equal to
// target and returns an iterator positioned on it. The returned iterator can
// be used like one produced by Iter: the first call to Next yields the located
// member, and subsequent calls continue forward through the set.
//
// If target is greater than every member, or the set is empty, the returned
// iterator is exhausted and its first call to Next returns false. Seeking is
// decided across the full 20-byte KSUID ordering, so it has a deterministic
// result across timestamp boundaries.
func (index *CompressedSetIndex) SeekGE(target KSUID) CompressedSetIter {
	iter, _ := index.seekGE(target)
	return iter
}

// seekGE implements SeekGE and additionally returns the number of encoded
// records that were decoded after the nearest sample was selected. This is
// bounded by the index stride regardless of how many members the set holds.
func (index *CompressedSetIndex) seekGE(target KSUID) (iter CompressedSetIter, steps int) {
	iter.content = index.content

	if len(index.samples) == 0 {
		return iter, 0
	}

	// Binary search for the last sample whose first member is not greater than
	// target: that sample's boundary is the earliest one the answer can follow.
	lo, hi := 0, len(index.samples)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if Compare(index.samples[mid].ksuid, target) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	// Step back to the last sample not greater than target; when target is
	// ordered before every sample (in particular the first member) start from
	// the first sample instead.
	if lo > 0 {
		lo--
	}
	sample := index.samples[lo]

	iter.offset = sample.offset
	iter.timestamp = sample.timestamp
	iter.lastValue = sample.lastValue

	var (
		offset    = sample.offset
		timestamp = sample.timestamp
		lastValue = sample.lastValue
	)

	for offset != len(index.content) {
		steps++

		rec, err := parseCompressedSetRecord(index.content, offset, timestamp, lastValue)
		if err != nil {
			// Construction validated the encoding; reaching this point means
			// the borrowed set was mutated, which violates the index contract.
			panic("KSUID set index is reading malformed data")
		}

		if rec.tag == payloadRange {
			runLast := rec.lastValue.ksuid(timestamp)

			if Compare(target, runLast) <= 0 {
				// Target falls within the run (its timestamp is necessarily the
				// run timestamp): compute the position directly instead of
				// stepping through the members one by one.
				firstValue := uint128Payload(rec.ksuid)
				distance := sub128(uint128Payload(target), firstValue)

				if distance == (uint128{}) {
					// The target is the first member of the run; leave the
					// iterator at the record boundary so Next decodes it.
					iter.offset = offset
					iter.timestamp = timestamp
					iter.lastValue = lastValue
					return iter, steps
				}

				// The varint bounds every run to a length that fits in 64
				// bits, so the distance inside the run cannot spill into the
				// high word; construction would have rejected it otherwise.
				if distance[1] != 0 {
					panic("KSUID set index is reading malformed data")
				}

				// The seqlength branch of Next increments lastValue before
				// producing a member. Position the decoder on the member
				// preceding the target and leave every member from the target
				// onward to produce: this matches the iterator state right
				// after it yielded the member before the target.
				one := makeUint128(0, 1)
				before := sub128(distance, one)
				iter.timestamp = timestamp
				iter.lastValue = add128(firstValue, before)
				iter.seqlength = rec.length - distance[0]
				iter.offset = rec.next
				return iter, steps
			}

			offset = rec.next
			lastValue = rec.lastValue
			continue
		}

		if Compare(target, rec.ksuid) <= 0 {
			// Leave the iterator at the record boundary so Next produces this
			// member; no per-member iteration is needed.
			iter.offset = offset
			iter.timestamp = timestamp
			iter.lastValue = lastValue
			return iter, steps
		}

		offset = rec.next
		timestamp = rec.timestamp
		lastValue = rec.lastValue
	}

	iter.offset = offset
	iter.timestamp = timestamp
	iter.lastValue = lastValue
	return iter, steps
}

// setRecord describes one encoded record of a CompressedSet without holding
// any members beyond its boundaries.
type setRecord struct {
	tag byte

	// next is the offset of the first byte after the record.
	next int

	// ksuid is the first (and, for non-range records, only) member produced by
	// the record.
	ksuid KSUID

	// timestamp and lastValue are the decoder state after the record has been
	// consumed, from which following deltas resume.
	timestamp uint32
	lastValue uint128

	// length is only meaningful for payloadRange records: it mirrors the wire
	// encoding, i.e. the number of members produced after the first member of
	// the run. For range records, lastValue is the payload of the last member
	// of the run (the decoder state after the run is exhausted).
	length uint64
}

// parseCompressedSetRecord decodes a single record of a CompressedSet starting
// at offset, given the decoder state carried from the preceding records. It
// performs structural validation (tag, sizes, overflows) and reports malformed
// input as an error instead of producing bogus values.
func parseCompressedSetRecord(content []byte, offset int, timestamp uint32, lastValue uint128) (setRecord, error) {
	if offset >= len(content) {
		return setRecord{}, ErrMalformedCompressedSet
	}

	b := content[offset]

	const mask = rawKSUID | timeDelta | payloadDelta | payloadRange
	tag := b & mask
	count := int(b) &^ mask

	rec := setRecord{
		tag:       tag,
		timestamp: timestamp,
	}

	switch tag {
	case rawKSUID:
		// A raw KSUID record carries no count nibble.
		if count != 0 {
			return setRecord{}, ErrMalformedCompressedSet
		}
		end := offset + 1 + byteLength
		if end > len(content) {
			return setRecord{}, ErrMalformedCompressedSet
		}
		copy(rec.ksuid[:], content[offset+1:end])
		rec.next = end
		rec.timestamp = rec.ksuid.Timestamp()
		rec.lastValue = uint128Payload(rec.ksuid)

	case timeDelta:
		if count < 1 || count > 4 {
			return setRecord{}, ErrMalformedCompressedSet
		}
		end := offset + 1 + count + payloadLengthInBytes
		if end > len(content) {
			return setRecord{}, ErrMalformedCompressedSet
		}

		delta := uint64(varint32(content[offset+1 : offset+1+count]))
		if delta == 0 || uint64(timestamp)+delta > math.MaxUint32 {
			return setRecord{}, ErrMalformedCompressedSet
		}

		rec.timestamp = timestamp + uint32(delta)
		binary.BigEndian.PutUint32(rec.ksuid[:timestampLengthInBytes], rec.timestamp)
		copy(rec.ksuid[timestampLengthInBytes:], content[offset+1+count:end])
		rec.next = end
		rec.lastValue = uint128Payload(rec.ksuid)

	case payloadDelta:
		if count < 1 || count > 16 {
			return setRecord{}, ErrMalformedCompressedSet
		}
		end := offset + 1 + count
		if end > len(content) {
			return setRecord{}, ErrMalformedCompressedSet
		}

		delta := varint128(content[offset+1 : end])
		value := add128(lastValue, delta)
		// A positive unsigned delta cannot wrap around the 128-bit payload.
		if cmp128(value, lastValue) <= 0 {
			return setRecord{}, ErrMalformedCompressedSet
		}

		rec.ksuid = value.ksuid(timestamp)
		rec.next = end
		rec.lastValue = value

	case payloadRange:
		if count < 1 || count > 8 {
			return setRecord{}, ErrMalformedCompressedSet
		}
		end := offset + 1 + count
		if end > len(content) {
			return setRecord{}, ErrMalformedCompressedSet
		}

		// The varint stores the number of members produced after the first one
		// of the run, matching the seqlength semantics of the iterator.
		length := varint64(content[offset+1 : end])
		if length == 0 {
			return setRecord{}, ErrMalformedCompressedSet
		}

		first := incr128(lastValue)
		if cmp128(first, lastValue) <= 0 {
			return setRecord{}, ErrMalformedCompressedSet
		}

		runEnd := add128(first, makeUint128(0, length))
		if cmp128(runEnd, first) <= 0 {
			return setRecord{}, ErrMalformedCompressedSet
		}

		rec.ksuid = first.ksuid(timestamp)
		rec.next = end
		rec.lastValue = sub128(runEnd, makeUint128(0, 1))
		rec.length = length

	default:
		return setRecord{}, ErrMalformedCompressedSet
	}

	return rec, nil
}
