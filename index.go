package ksuid

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// ErrMalformedCompressedSet is the error returned by CompressedSet.Index when
// the encoding of the set is invalid. Errors are detected while the index is
// being built, so corrupt input can never surface later as a panic or as
// incorrect query results.
var ErrMalformedCompressedSet = errors.New("ksuid: malformed compressed set")

// defaultIndexStride is the number of encoded records between two consecutive
// index samples when CompressedSet.Index is called with a non-positive
// stride.
const defaultIndexStride = 32

// CompressedSetIndex is a reusable seek index over an immutable
// CompressedSet.
//
// The index only samples the encoding at record boundaries: each sample
// stores the byte offset of a record, the decoder state (timestamp and last
// payload value) required to resume delta decoding from that record, and the
// first KSUID the record produces. The set is never decompressed into the
// index, so the memory footprint of the index stays proportional to the size
// of the encoded representation, not to the number of KSUIDs in the set.
//
// Copy and borrowing contract: the index borrows the byte slice of the
// CompressedSet it was built from instead of copying it. Because compressed
// sets are immutable this aliasing is safe, as long as the program does not
// modify the set (for example by appending into its backing array) while the
// index is in use. CompressedSetIndex values are safe to copy: copies share
// the same immutable sample table and set encoding, and may be used
// concurrently from multiple goroutines.
type CompressedSetIndex struct {
	set     CompressedSet
	samples []indexSample
}

// indexSample records the state needed to resume decoding at a record
// boundary, along with the first KSUID produced by the sampled record.
type indexSample struct {
	offset    int     // byte offset of the record in the set encoding
	timestamp uint32  // timestamp in effect before the record
	lastValue uint128 // last payload value before the record
	firstID   KSUID   // first KSUID produced by the record
}

// Index builds a reusable seek index over the set, sampling the encoding
// every stride records. A stride less than or equal to zero selects a
// sensible default; a stride of one samples every record.
//
// The encoding is fully validated while the index is being built: malformed
// sets are reported by the returned error (wrapping
// ErrMalformedCompressedSet) instead of surfacing later during a seek.
func (set CompressedSet) Index(stride int) (CompressedSetIndex, error) {
	if stride <= 0 {
		stride = defaultIndexStride
	}

	content := []byte(set)
	samples := make([]indexSample, 0, 1+len(content)/stride)

	var timestamp uint32
	var lastValue uint128

	for offset, record := 0, 0; offset < len(content); record++ {
		next, ts, lv, first, err := scanSetRecord(content, offset, timestamp, lastValue)
		if err != nil {
			return CompressedSetIndex{}, err
		}
		if record%stride == 0 {
			samples = append(samples, indexSample{
				offset:    offset,
				timestamp: timestamp,
				lastValue: lastValue,
				firstID:   first,
			})
		}
		offset, timestamp, lastValue = next, ts, lv
	}

	return CompressedSetIndex{set: set, samples: samples}, nil
}

// SeekGE returns an iterator over the indexed set positioned so that the
// first call to Next produces the first member of the set which is greater
// than or equal to target, and subsequent calls produce the remaining members
// in order, like the regular set iterator would.
//
// The results are fully determined by the set and the target: if target sorts
// before the first member of the set the iterator starts at the beginning of
// the set; if it sorts after the last member, or if the set is empty, the
// returned iterator is immediately exhausted.
//
// The amount of work performed is bounded by the number of index samples and
// the number of encoded records between two samples: payload ranges are
// skipped, or jumped into, arithmetically instead of being stepped through
// value by value.
func (idx CompressedSetIndex) SeekGE(target KSUID) CompressedSetIter {
	if len(idx.samples) == 0 {
		return CompressedSetIter{}
	}

	// Locate the last sampled record whose first KSUID is less than or equal
	// to the target. The encoding is sorted, so the lower bound cannot live
	// in a record preceding that sample.
	i := sort.Search(len(idx.samples), func(i int) bool {
		return Compare(idx.samples[i].firstID, target) > 0
	}) - 1
	if i < 0 {
		i = 0
	}

	sample := idx.samples[i]
	it := CompressedSetIter{
		content:   []byte(idx.set),
		offset:    sample.offset,
		timestamp: sample.timestamp,
		lastValue: sample.lastValue,
	}

	one := makeUint128(0, 1)

	for it.offset < len(it.content) {
		b := it.content[it.offset]
		cnt := int(b) &^ recordMask

		if int(b)&recordMask != payloadRange {
			// Single-value record: decode it through the regular iterator and
			// stop as soon as the lower bound is reached, keeping the
			// iterator positioned on the record so Next replays it.
			save := it
			if !it.Next() {
				return it
			}
			if Compare(it.KSUID, target) >= 0 {
				return save
			}
			continue
		}

		// Payload range: the record produces the m consecutive payload values
		// (lastValue, lastValue+m] at the current timestamp.
		m := varint64(it.content[it.offset+1 : it.offset+1+cnt])
		end := add128(it.lastValue, makeUint128(0, m))

		if t := target.Timestamp(); t <= it.timestamp {
			if t < it.timestamp {
				return it // target sorts before the whole range
			}
			if value := uint128Payload(target); cmp128(value, end) <= 0 {
				if cmp128(value, it.lastValue) <= 0 {
					return it // target sorts at or before the first range value
				}
				// The target lives inside the range: jump directly to it by
				// computing the offset within the range instead of stepping
				// through the values one by one.
				k := sub128(value, it.lastValue) // 1 <= k <= m
				it.offset += 1 + cnt
				it.lastValue = sub128(value, one)
				it.seqlength = m - k[0] + 1
				return it
			}
		}

		// The target sorts past the whole range, skip it in constant time.
		it.offset += 1 + cnt
		it.lastValue = end
	}

	return it
}

// scanSetRecord decodes the record starting at offset in content, returning
// the offset of the next record, the decoder state after the record, and the
// first KSUID the record produces. Payload ranges are skipped arithmetically,
// so scanning a set never takes time proportional to the number of KSUIDs it
// contains. Malformed records are reported as errors wrapping
// ErrMalformedCompressedSet.
func scanSetRecord(content []byte, offset int, timestamp uint32, lastValue uint128) (next int, ts uint32, lv uint128, first KSUID, err error) {
	start := offset
	b := content[offset]
	tag := int(b) & recordMask
	cnt := int(b) &^ recordMask
	offset++

	switch tag {
	case rawKSUID:
		if len(content)-offset < byteLength {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "truncated raw KSUID")
		}
		copy(first[:], content[offset:offset+byteLength])
		offset += byteLength
		timestamp = first.Timestamp()
		lastValue = uint128Payload(first)

	case timeDelta:
		if cnt < 1 || cnt > timestampLengthInBytes {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "invalid time delta length")
		}
		if len(content)-offset < cnt+payloadLengthInBytes {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "truncated time delta")
		}
		timestamp += varint32(content[offset : offset+cnt])
		binary.BigEndian.PutUint32(first[:timestampLengthInBytes], timestamp)
		copy(first[timestampLengthInBytes:], content[offset+cnt:offset+cnt+payloadLengthInBytes])
		offset += cnt + payloadLengthInBytes
		lastValue = uint128Payload(first)

	case payloadDelta:
		if cnt < 1 || cnt > 16 {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "invalid payload delta length")
		}
		if len(content)-offset < cnt {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "truncated payload delta")
		}
		lastValue = add128(lastValue, varint128(content[offset:offset+cnt]))
		offset += cnt
		first = lastValue.ksuid(timestamp)

	case payloadRange:
		if cnt < 1 || cnt > 8 {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "invalid payload range length")
		}
		if len(content)-offset < cnt {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "truncated payload range")
		}
		m := varint64(content[offset : offset+cnt])
		if m == 0 {
			return 0, 0, uint128{}, Nil, malformedCompressedSetError(start, "empty payload range")
		}
		offset += cnt
		first = incr128(lastValue).ksuid(timestamp)
		lastValue = add128(lastValue, makeUint128(0, m))
	}

	return offset, timestamp, lastValue, first, nil
}

func malformedCompressedSetError(offset int, reason string) error {
	return fmt.Errorf("%w at offset %d: %s", ErrMalformedCompressedSet, offset, reason)
}
