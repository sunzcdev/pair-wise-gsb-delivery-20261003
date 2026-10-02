package ksuid

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
)

// CompressedSetRecord identifies the kind of record that was being parsed when
// an error occurred while reading a compressed set.
type CompressedSetRecord uint8

const (
	// RecordRaw identifies a raw KSUID record.
	RecordRaw CompressedSetRecord = iota
	// RecordTimeDelta identifies a time delta record.
	RecordTimeDelta
	// RecordPayloadDelta identifies a payload delta record.
	RecordPayloadDelta
	// RecordPayloadRange identifies a payload range record.
	RecordPayloadRange
)

// String returns a human-readable representation of the record type.
func (r CompressedSetRecord) String() string {
	switch r {
	case RecordRaw:
		return "raw ksuid"
	case RecordTimeDelta:
		return "time delta"
	case RecordPayloadDelta:
		return "payload delta"
	case RecordPayloadRange:
		return "payload range"
	default:
		return fmt.Sprintf("unknown record type (%d)", uint8(r))
	}
}

// CompressedSetErrorKind classifies the errors reported while strictly reading
// a compressed set.
type CompressedSetErrorKind uint8

const (
	// ErrorTruncated means the record did not contain enough bytes.
	ErrorTruncated CompressedSetErrorKind = iota
	// ErrorNonCanonicalLength means an integer was not encoded in its shortest
	// possible representation.
	ErrorNonCanonicalLength
	// ErrorZeroDelta means a time or payload delta was zero.
	ErrorZeroDelta
	// ErrorZeroRange means a payload range had a length of zero.
	ErrorZeroRange
	// ErrorIllegalStart means the first record was not a raw KSUID.
	ErrorIllegalStart
	// ErrorOverflow means a time or payload addition wrapped around.
	ErrorOverflow
	// ErrorElementLimit means the set contains more KSUIDs than the configured
	// maximum.
	ErrorElementLimit
)

// String returns a human-readable description of the error kind.
func (k CompressedSetErrorKind) String() string {
	switch k {
	case ErrorTruncated:
		return "truncated record"
	case ErrorNonCanonicalLength:
		return "non-canonical integer length"
	case ErrorZeroDelta:
		return "zero delta"
	case ErrorZeroRange:
		return "zero range length"
	case ErrorIllegalStart:
		return "set must start with a raw ksuid"
	case ErrorOverflow:
		return "integer overflow"
	case ErrorElementLimit:
		return "maximum element count exceeded"
	default:
		return fmt.Sprintf("unknown error (%d)", uint8(k))
	}
}

// CompressedSetReadError describes a malformed compressed set record.
//
// Offset is the byte position of the record tag in the encoded set, Record is
// the type of the offending record and Kind classifies the error.
type CompressedSetReadError struct {
	Offset int
	Record CompressedSetRecord
	Kind   CompressedSetErrorKind
}

// Error satisfies the error interface.
func (e *CompressedSetReadError) Error() string {
	return fmt.Sprintf("ksuid: compressed set error at byte offset %d in %s record: %s",
		e.Offset, e.Record, e.Kind)
}

func setReadError(offset int, record CompressedSetRecord, kind CompressedSetErrorKind) *CompressedSetReadError {
	return &CompressedSetReadError{
		Offset: offset,
		Record: record,
		Kind:   kind,
	}
}

func recordOfTag(tag int) CompressedSetRecord {
	switch tag {
	case rawKSUID:
		return RecordRaw
	case timeDelta:
		return RecordTimeDelta
	case payloadDelta:
		return RecordPayloadDelta
	default:
		return RecordPayloadRange
	}
}

// add128Checked returns x+y and ok=false if the addition overflows 128 bits.
func add128Checked(x, y uint128) (z uint128, ok bool) {
	low, carry := bits.Add64(x[0], y[0], 0)
	high, carry := bits.Add64(x[1], y[1], carry)
	return uint128{low, high}, carry == 0
}

// add64Checked returns x+y and ok=false if the addition overflows 64 bits.
func add64Checked(x, y uint64) (uint64, bool) {
	sum, carry := bits.Add64(x, y, 0)
	return sum, carry == 0
}

// compressedSetDecoder holds the state used while walking the records of a
// compressed set in strict mode.
type compressedSetDecoder struct {
	content []byte
	offset  int

	started   bool
	seqlength uint64
	count     uint64
	timestamp uint32
	lastValue uint128
}

// next loads the following KSUID into id. It returns io.EOF when the set has
// been fully consumed, or a *CompressedSetReadError describing a malformed
// record. maxElements limits the number of KSUIDs produced; zero disables it.
func (d *compressedSetDecoder) next(id *KSUID, maxElements uint64) error {
	if d.seqlength != 0 {
		value := incr128(d.lastValue)
		*id = value.ksuid(d.timestamp)
		d.seqlength--
		d.lastValue = value
		return nil
	}

	if d.offset == len(d.content) {
		return io.EOF
	}

	tagOffset := d.offset
	b := d.content[d.offset]
	d.offset++

	tag := int(b) & tagMask
	cnt := int(b) & ^tagMask
	record := recordOfTag(tag)

	if !d.started && tag != rawKSUID {
		return setReadError(tagOffset, record, ErrorIllegalStart)
	}

	switch tag {
	case rawKSUID:
		if cnt != 0 {
			return setReadError(tagOffset, record, ErrorNonCanonicalLength)
		}

		off1 := d.offset + byteLength
		if off1 > len(d.content) || off1 < d.offset {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		copy(id[:], d.content[d.offset:off1])

		d.offset = off1
		d.timestamp = id.Timestamp()
		d.lastValue = uint128Payload(*id)

		if maxElements != 0 && d.count >= maxElements {
			return setReadError(tagOffset, record, ErrorElementLimit)
		}

	case timeDelta:
		if cnt < 1 || cnt > 4 {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		off1 := d.offset + cnt
		off2 := off1 + payloadLengthInBytes
		if off1 > len(d.content) || off2 > len(d.content) || off1 < d.offset {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		delta := uint64(varint32(d.content[d.offset:off1]))
		if delta == 0 {
			return setReadError(tagOffset, record, ErrorZeroDelta)
		}
		if d.content[d.offset] == 0 {
			return setReadError(tagOffset, record, ErrorNonCanonicalLength)
		}

		ts, ok := add64Checked(uint64(d.timestamp), delta)
		if !ok || ts > math.MaxUint32 {
			return setReadError(tagOffset, record, ErrorOverflow)
		}
		d.timestamp = uint32(ts)

		binary.BigEndian.PutUint32(id[:timestampLengthInBytes], d.timestamp)
		copy(id[timestampLengthInBytes:], d.content[off1:off2])

		d.offset = off2
		d.lastValue = uint128Payload(*id)

		if maxElements != 0 && d.count >= maxElements {
			return setReadError(tagOffset, record, ErrorElementLimit)
		}

	case payloadDelta:
		if cnt < 1 || cnt > 16 {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		off1 := d.offset + cnt
		if off1 > len(d.content) || off1 < d.offset {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		delta := varint128(d.content[d.offset:off1])
		if delta == (uint128{}) {
			return setReadError(tagOffset, record, ErrorZeroDelta)
		}
		if d.content[d.offset] == 0 {
			return setReadError(tagOffset, record, ErrorNonCanonicalLength)
		}

		value, ok := add128Checked(d.lastValue, delta)
		if !ok {
			return setReadError(tagOffset, record, ErrorOverflow)
		}

		*id = value.ksuid(d.timestamp)
		d.offset = off1
		d.lastValue = value

		if maxElements != 0 && d.count >= maxElements {
			return setReadError(tagOffset, record, ErrorElementLimit)
		}

	case payloadRange:
		if cnt < 1 || cnt > 8 {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		off1 := d.offset + cnt
		if off1 > len(d.content) || off1 < d.offset {
			return setReadError(tagOffset, record, ErrorTruncated)
		}

		length := varint64(d.content[d.offset:off1])
		if length == 0 {
			return setReadError(tagOffset, record, ErrorZeroRange)
		}
		if d.content[d.offset] == 0 {
			return setReadError(tagOffset, record, ErrorNonCanonicalLength)
		}

		if _, ok := add128Checked(d.lastValue, uint128{length, 0}); !ok {
			return setReadError(tagOffset, record, ErrorOverflow)
		}

		total, ok := add64Checked(d.count, length)
		if !ok || (maxElements != 0 && total > maxElements) {
			return setReadError(tagOffset, record, ErrorElementLimit)
		}

		value := incr128(d.lastValue)
		*id = value.ksuid(d.timestamp)
		d.offset = off1
		d.seqlength = length - 1
		d.count = total - 1
		d.lastValue = value
	}

	d.started = true
	d.count++
	return nil
}

// CompressedSetStrictIter is an iterator returned by CompressedSet.IterStrict
// which validates every record read from the compressed set.
//
// Usage:
//
//	for it := set.IterStrict(0); it.Next(); {
//		id := it.KSUID
//		// ...
//	}
//	if err := it.Err(); err != nil {
//		// handle malformed data
//	}
//
// Once Next has returned false because of an error or the end of the set,
// further calls to Next keep returning false and Err remains stable.
// CompressedSetStrictIter values are not safe to use concurrently from
// multiple goroutines.
type CompressedSetStrictIter struct {
	// KSUID is modified by calls to the Next method to hold the KSUID loaded
	// by the iterator.
	KSUID KSUID

	decoder     compressedSetDecoder
	maxElements uint64
	err         error
	done        bool
}

// Next moves the iterator forward, returning true if a KSUID was loaded. It
// returns false when the end of the set is reached or a malformed record is
// encountered; in the latter case Err describes the error.
func (it *CompressedSetStrictIter) Next() bool {
	if it.done {
		return false
	}

	err := it.decoder.next(&it.KSUID, it.maxElements)
	if err != nil {
		it.done = true
		if err != io.EOF {
			it.err = err
		}
		return false
	}
	return true
}

// Err returns the first error encountered while reading the set, or nil if
// the set was fully consumed without errors.
func (it *CompressedSetStrictIter) Err() error {
	return it.err
}
