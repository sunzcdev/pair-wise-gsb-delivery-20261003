package ksuid

import (
	"encoding/binary"
	"fmt"
	"math"
)

// CompressedSetError is the type of errors reported when decoding a malformed
// or invalid compressed set with the strict decoding entry points
// (ParseCompressedSet and CompressedSet.StrictIter).
//
// The error records the byte offset of the faulty instruction in the set
// encoding, along with the type of that instruction.
type CompressedSetError struct {
	// Offset is the byte offset of the instruction that failed to decode in
	// the compressed set encoding.
	Offset int

	// Tag is the type of the instruction that failed to decode (one of
	// rawKSUID, timeDelta, payloadDelta, or payloadRange).
	Tag byte

	// Reason is a human-readable description of what was wrong with the
	// instruction.
	Reason string
}

// Error satisfies the error interface.
func (e *CompressedSetError) Error() string {
	return fmt.Sprintf("ksuid: invalid compressed set: %s at byte offset %d (%s instruction)",
		e.Reason, e.Offset, compressedSetInstructionName(e.Tag))
}

func compressedSetInstructionName(tag byte) string {
	switch tag {
	case rawKSUID:
		return "raw"
	case timeDelta:
		return "time-delta"
	case payloadDelta:
		return "payload-delta"
	case payloadRange:
		return "payload-range"
	default:
		return "unknown"
	}
}

var maxUint128 = uint128{math.MaxUint64, math.MaxUint64}

// ParseCompressedSet strictly validates the given compressed set encoding and
// returns it as a CompressedSet value which is then safe to iterate over with
// the (trusted) CompressedSet.Iter method.
//
// Unlike CompressedSet.Iter, which trusts the encoding and may panic on
// corrupt data, this function validates the encoding and reports errors
// instead: truncated instructions, zero deltas, instructions appearing before
// the initial raw KSUID, timestamp and payload overflows, and non-minimal
// integer encodings (values stored in more bytes than strictly necessary) are
// all rejected.
//
// If maxElements is not zero, encodings that would produce more than
// maxElements KSUIDs are rejected. The limit is checked before expanding
// payload ranges, so hostile encodings cannot force the parser to expand
// billions of elements.
func ParseCompressedSet(data []byte, maxElements uint64) (CompressedSet, error) {
	it := CompressedSet(data).StrictIter(maxElements)
	for it.Next() {
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return CompressedSet(data), nil
}

// StrictIter returns an iterator that produces all KSUIDs in the set while
// strictly validating the encoding, reporting errors through its Err method
// instead of panicking like CompressedSetIter does on malformed data.
//
// If maxElements is not zero, the iterator stops with an error as soon as the
// set would produce more than maxElements KSUIDs; the limit is enforced
// before expanding payload ranges.
func (set CompressedSet) StrictIter(maxElements uint64) CompressedSetStrictIter {
	return CompressedSetStrictIter{
		content:     []byte(set),
		maxElements: maxElements,
	}
}

// CompressedSetStrictIter is an iterator type returned by
// CompressedSet.StrictIter to produce the list of KSUIDs stored in a set
// while validating the encoding.
//
// Here's is how the iterator type is commonly used:
//
//	for it := set.StrictIter(0); it.Next(); {
//		id := it.KSUID
//		// ...
//	}
//	if err := it.Err(); err != nil {
//		// the set was malformed
//	}
//
// Once Next has returned false (either because the end of the set was reached
// or because an error was encountered), further calls to Next keep returning
// false and the iterator state remains unchanged.
//
// CompressedSetStrictIter values are not safe to use concurrently from
// multiple goroutines.
type CompressedSetStrictIter struct {
	// KSUID is modified by calls to the Next method to hold the KSUID loaded
	// by the iterator.
	KSUID KSUID

	content []byte
	offset  int

	seqlength uint64
	timestamp uint32
	lastValue uint128

	maxElements uint64
	numElements uint64
	started     bool
	done        bool
	err         error
}

// Err returns the first error encountered by the iterator, or nil if the set
// was decoded successfully. The error is never cleared nor replaced once set.
func (it *CompressedSetStrictIter) Err() error {
	return it.err
}

// Next moves the iterator forward, returning true if a KSUID was found, or
// false if the iterator has reached the end of the set or encountered an
// error (which can be retrieved with the Err method).
func (it *CompressedSetStrictIter) Next() bool {
	if it.err != nil || it.done {
		return false
	}

	if it.seqlength != 0 {
		// Expanding a payload range: the element count limit and the
		// overflow checks were already performed when the range instruction
		// was decoded.
		value := incr128(it.lastValue)
		it.KSUID = value.ksuid(it.timestamp)
		it.seqlength--
		it.lastValue = value
		it.numElements++
		return true
	}

	if it.offset == len(it.content) {
		it.done = true
		return false
	}

	off := it.offset
	b := it.content[off]
	it.offset++

	const mask = rawKSUID | timeDelta | payloadDelta | payloadRange
	tag := int(b) & mask
	cnt := int(b) & ^mask

	switch tag {
	case rawKSUID:
		if cnt != 0 {
			return it.fail(off, byte(tag), "raw KSUID instruction carries a non-zero length")
		}
		if len(it.content)-it.offset < byteLength {
			return it.fail(off, byte(tag), "truncated raw KSUID")
		}

		copy(it.KSUID[:], it.content[it.offset:it.offset+byteLength])

		it.offset += byteLength
		it.timestamp = it.KSUID.Timestamp()
		it.lastValue = uint128Payload(it.KSUID)

	case timeDelta:
		if !it.started {
			return it.fail(off, byte(tag), "time delta instruction before any raw KSUID")
		}
		if cnt < 1 || cnt > 4 {
			return it.fail(off, byte(tag), "invalid time delta length")
		}
		if len(it.content)-it.offset < cnt+payloadLengthInBytes {
			return it.fail(off, byte(tag), "truncated time delta")
		}

		d := varint32(it.content[it.offset : it.offset+cnt])

		if d == 0 {
			return it.fail(off, byte(tag), "zero time delta")
		}
		if varintLength32(d) != cnt {
			return it.fail(off, byte(tag), "non-minimal time delta encoding")
		}
		if math.MaxUint32-it.timestamp < d {
			return it.fail(off, byte(tag), "timestamp overflow")
		}

		it.timestamp += d

		binary.BigEndian.PutUint32(it.KSUID[:timestampLengthInBytes], it.timestamp)
		copy(it.KSUID[timestampLengthInBytes:], it.content[it.offset+cnt:it.offset+cnt+payloadLengthInBytes])

		it.offset += cnt + payloadLengthInBytes
		it.lastValue = uint128Payload(it.KSUID)

	case payloadDelta:
		if !it.started {
			return it.fail(off, byte(tag), "payload delta instruction before any raw KSUID")
		}
		if cnt < 1 || cnt > 16 {
			return it.fail(off, byte(tag), "invalid payload delta length")
		}
		if len(it.content)-it.offset < cnt {
			return it.fail(off, byte(tag), "truncated payload delta")
		}

		d := varint128(it.content[it.offset : it.offset+cnt])

		if d == (uint128{}) {
			return it.fail(off, byte(tag), "zero payload delta")
		}
		if varintLength128(d) != cnt {
			return it.fail(off, byte(tag), "non-minimal payload delta encoding")
		}

		value := add128(it.lastValue, d)

		if cmp128(value, it.lastValue) < 0 {
			return it.fail(off, byte(tag), "payload overflow")
		}

		it.KSUID = value.ksuid(it.timestamp)
		it.offset += cnt
		it.lastValue = value

	case payloadRange:
		if !it.started {
			return it.fail(off, byte(tag), "payload range instruction before any raw KSUID")
		}
		if cnt < 1 || cnt > 8 {
			return it.fail(off, byte(tag), "invalid payload range length")
		}
		if len(it.content)-it.offset < cnt {
			return it.fail(off, byte(tag), "truncated payload range")
		}

		m := varint64(it.content[it.offset : it.offset+cnt])

		if m == 0 {
			return it.fail(off, byte(tag), "zero payload range")
		}
		if varintLength64(m) != cnt {
			return it.fail(off, byte(tag), "non-minimal payload range encoding")
		}
		if cmp128(makeUint128(0, m), sub128(maxUint128, it.lastValue)) > 0 {
			return it.fail(off, byte(tag), "payload range overflow")
		}
		// Check the element count limit before expanding the range, so a
		// hostile encoding cannot force the iteration of billions of ids.
		if it.maxElements != 0 && m > it.maxElements-it.numElements {
			return it.fail(off, byte(tag), "element count limit exceeded")
		}

		value := incr128(it.lastValue)
		it.KSUID = value.ksuid(it.timestamp)
		it.offset += cnt
		it.seqlength = m - 1
		it.lastValue = value
	}

	it.started = true

	if it.maxElements != 0 && it.numElements == it.maxElements {
		return it.fail(off, byte(tag), "element count limit exceeded")
	}
	it.numElements++

	return true
}

func (it *CompressedSetStrictIter) fail(offset int, tag byte, reason string) bool {
	it.err = &CompressedSetError{
		Offset: offset,
		Tag:    tag,
		Reason: reason,
	}
	return false
}
