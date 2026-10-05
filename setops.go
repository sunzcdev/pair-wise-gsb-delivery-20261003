package ksuid

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// CorruptSetError is the error type returned by Union, Intersect and Subtract
// when one of the compressed sets they operate on contains malformed data.
type CorruptSetError struct {
	// Source indicates which of the two operands was malformed, it is either
	// "first" or "second".
	Source string
	// Offset is the byte offset in the source set at which the corruption
	// was detected.
	Offset int
	// Err describes the corruption that was found.
	Err error
}

// Error satisfies the error interface.
func (e *CorruptSetError) Error() string {
	return fmt.Sprintf("ksuid: invalid %s compressed set at offset %d: %v", e.Source, e.Offset, e.Err)
}

// Unwrap returns the underlying error describing the corruption.
func (e *CorruptSetError) Unwrap() error { return e.Err }

// Union returns a compressed set containing all the KSUIDs present in either
// a or b. The operation is performed as a streaming merge of the two encoded
// inputs: consecutive runs of identifiers are processed as intervals and are
// never expanded into their individual elements.
//
// The inputs are only read from, they are never modified, and the result is
// a newly allocated set which is compatible with the CompressedSet format.
//
// If either input is malformed, a nil set is returned along with an error of
// type *CorruptSetError indicating which operand was corrupt and at which
// byte offset; no partially computed set is ever delivered.
func Union(a, b CompressedSet) (CompressedSet, error) {
	ra := newSetRangeReader("first", a)
	rb := newSetRangeReader("second", b)
	w := setRangeWriter{}

	va, oka, err := ra.next()
	if err != nil {
		return nil, err
	}
	vb, okb, err := rb.next()
	if err != nil {
		return nil, err
	}

	for oka || okb {
		if oka && (!okb || rangeStartsBefore(va, vb)) {
			w.push(va)
			if va, oka, err = ra.next(); err != nil {
				return nil, err
			}
		} else {
			w.push(vb)
			if vb, okb, err = rb.next(); err != nil {
				return nil, err
			}
		}
	}
	return w.finish(), nil
}

// Intersect returns a compressed set containing only the KSUIDs present in
// both a and b. Like Union, the operation is a streaming merge which treats
// consecutive runs of identifiers as intervals instead of expanding them.
//
// See Union for the guarantees offered on the inputs, the result, and the
// errors returned when one of the operands is corrupt.
func Intersect(a, b CompressedSet) (CompressedSet, error) {
	ra := newSetRangeReader("first", a)
	rb := newSetRangeReader("second", b)
	w := setRangeWriter{}

	va, oka, err := ra.next()
	if err != nil {
		return nil, err
	}
	vb, okb, err := rb.next()
	if err != nil {
		return nil, err
	}

	for oka && okb {
		switch {
		case va.timestamp < vb.timestamp:
			va, oka, err = ra.next()
		case va.timestamp > vb.timestamp:
			vb, okb, err = rb.next()
		default:
			lo := max128(va.lo, vb.lo)
			hi := min128(va.hi, vb.hi)
			if cmp128(lo, hi) <= 0 {
				w.push(setRange{timestamp: va.timestamp, lo: lo, hi: hi})
			}
			switch c := cmp128(va.hi, vb.hi); {
			case c < 0:
				va, oka, err = ra.next()
			case c > 0:
				vb, okb, err = rb.next()
			default:
				if va, oka, err = ra.next(); err == nil {
					vb, okb, err = rb.next()
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return w.finish(), nil
}

// Subtract returns a compressed set containing the KSUIDs present in a but
// absent from b (the set difference a \ b). Like Union, the operation is a
// streaming merge which treats consecutive runs of identifiers as intervals
// instead of expanding them, so removing a single identifier from the middle
// of a large range simply splits it into two ranges.
//
// See Union for the guarantees offered on the inputs, the result, and the
// errors returned when one of the operands is corrupt.
func Subtract(a, b CompressedSet) (CompressedSet, error) {
	ra := newSetRangeReader("first", a)
	rb := newSetRangeReader("second", b)
	w := setRangeWriter{}

	va, oka, err := ra.next()
	if err != nil {
		return nil, err
	}
	vb, okb, err := rb.next()
	if err != nil {
		return nil, err
	}

	for oka {
		switch {
		case !okb:
			w.push(va)
			va, oka, err = ra.next()
		case va.timestamp != vb.timestamp:
			if va.timestamp < vb.timestamp {
				w.push(va)
				va, oka, err = ra.next()
			} else {
				vb, okb, err = rb.next()
			}
		case cmp128(vb.hi, va.lo) < 0:
			vb, okb, err = rb.next()
		case cmp128(va.hi, vb.lo) < 0:
			w.push(va)
			va, oka, err = ra.next()
		default:
			if cmp128(va.lo, vb.lo) < 0 {
				w.push(setRange{timestamp: va.timestamp, lo: va.lo, hi: decr128(vb.lo)})
			}
			if cmp128(va.hi, vb.hi) <= 0 {
				va, oka, err = ra.next()
			} else {
				va.lo = incr128(vb.hi)
				vb, okb, err = rb.next()
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return w.finish(), nil
}

// setRange represents an inclusive interval of KSUID payloads sharing a
// single timestamp.
type setRange struct {
	timestamp uint32
	lo        uint128
	hi        uint128
}

func rangeStartsBefore(x, y setRange) bool {
	return x.timestamp < y.timestamp ||
		(x.timestamp == y.timestamp && cmp128(x.lo, y.lo) < 0)
}

// setRangeReader decodes a compressed set into a stream of coalesced,
// strictly ascending intervals without expanding ranges into their
// individual elements. Malformed input is reported as a *CorruptSetError
// carrying the reader name and the offset of the offending record.
type setRangeReader struct {
	name    string
	content []byte
	offset  int
	record  int

	timestamp uint32
	lastValue uint128

	pending    setRange
	hasPending bool
}

func newSetRangeReader(name string, set CompressedSet) *setRangeReader {
	return &setRangeReader{name: name, content: []byte(set)}
}

func (r *setRangeReader) fail(offset int, format string, args ...interface{}) error {
	return &CorruptSetError{
		Source: r.name,
		Offset: offset,
		Err:    fmt.Errorf(format, args...),
	}
}

// next returns the next coalesced interval of the set, or false when the set
// has been fully consumed.
func (r *setRangeReader) next() (setRange, bool, error) {
	for {
		if r.offset == len(r.content) {
			if !r.hasPending {
				return setRange{}, false, nil
			}
			rng := r.pending
			r.hasPending = false
			return rng, true, nil
		}

		rng, err := r.decode()
		if err != nil {
			return setRange{}, false, err
		}

		if !r.hasPending {
			r.pending = rng
			r.hasPending = true
			continue
		}

		p := r.pending
		if rng.timestamp < p.timestamp {
			return setRange{}, false, r.fail(r.record, "records are not ordered by timestamp")
		}
		if rng.timestamp == p.timestamp {
			if cmp128(rng.hi, p.lo) < 0 && incr128(rng.hi) != p.lo {
				return setRange{}, false, r.fail(r.record, "records are not ordered by payload")
			}
			if cmp128(rng.lo, p.hi) <= 0 || incr128(p.hi) == rng.lo {
				// The records overlap or are adjacent, coalesce them so
				// that duplicate and touching runs are only seen once by
				// the merge algorithms.
				if cmp128(rng.lo, p.lo) < 0 {
					p.lo = rng.lo
				}
				if cmp128(rng.hi, p.hi) > 0 {
					p.hi = rng.hi
				}
				r.pending = p
				continue
			}
		}

		r.pending = rng
		return p, true, nil
	}
}

// decode reads a single record from the encoded stream and returns the
// interval it covers.
func (r *setRangeReader) decode() (setRange, error) {
	start := r.offset
	r.record = start
	b := r.content[start]
	r.offset++

	const mask = rawKSUID | timeDelta | payloadDelta | payloadRange
	tag := int(b) & mask
	cnt := int(b) & ^mask

	need := func(n int) ([]byte, error) {
		if len(r.content)-r.offset < n {
			return nil, r.fail(start, "truncated record: needs %d bytes but only %d remain",
				n, len(r.content)-r.offset)
		}
		c := r.content[r.offset : r.offset+n]
		r.offset += n
		return c, nil
	}

	switch tag {
	case rawKSUID:
		c, err := need(byteLength)
		if err != nil {
			return setRange{}, err
		}
		r.timestamp = binary.BigEndian.Uint32(c[:timestampLengthInBytes])
		r.lastValue = makeUint128FromPayload(c[timestampLengthInBytes:])

	case timeDelta:
		if cnt < 1 || cnt > 4 {
			return setRange{}, r.fail(start, "invalid timestamp delta length %d", cnt)
		}
		c, err := need(cnt + payloadLengthInBytes)
		if err != nil {
			return setRange{}, err
		}
		r.timestamp += varint32(c[:cnt])
		r.lastValue = makeUint128FromPayload(c[cnt:])

	case payloadDelta:
		if cnt < 1 || cnt > 16 {
			return setRange{}, r.fail(start, "invalid payload delta length %d", cnt)
		}
		c, err := need(cnt)
		if err != nil {
			return setRange{}, err
		}
		delta := varint128(c)
		value := add128(r.lastValue, delta)
		if delta != (uint128{}) && cmp128(value, r.lastValue) <= 0 {
			return setRange{}, r.fail(start, "payload delta overflows")
		}
		r.lastValue = value

	case payloadRange:
		if cnt < 1 || cnt > 8 {
			return setRange{}, r.fail(start, "invalid payload range length %d", cnt)
		}
		c, err := need(cnt)
		if err != nil {
			return setRange{}, err
		}
		m := varint64(c)
		if m == 0 {
			return setRange{}, r.fail(start, "payload range of zero elements")
		}
		lo, carry := bits.Add64(r.lastValue[0], 1, 0)
		hi := r.lastValue[1] + carry
		if hi < r.lastValue[1] {
			return setRange{}, r.fail(start, "payload range overflows")
		}
		end, carry := bits.Add64(r.lastValue[0], m, 0)
		endHi := r.lastValue[1] + carry
		if endHi < r.lastValue[1] {
			return setRange{}, r.fail(start, "payload range overflows")
		}
		rng := setRange{
			timestamp: r.timestamp,
			lo:        uint128{lo, hi},
			hi:        uint128{end, endHi},
		}
		r.lastValue = rng.hi
		return rng, nil
	}

	return setRange{timestamp: r.timestamp, lo: r.lastValue, hi: r.lastValue}, nil
}

// setRangeWriter encodes a stream of ascending intervals into the compressed
// set format. Adjacent or overlapping intervals are coalesced so the output
// is strictly ascending and free of duplicates.
type setRangeWriter struct {
	buf       []byte
	started   bool
	timestamp uint32
	lastValue uint128

	pending    setRange
	hasPending bool
}

func (w *setRangeWriter) push(r setRange) {
	if w.hasPending && r.timestamp == w.pending.timestamp &&
		(cmp128(r.lo, w.pending.hi) <= 0 || incr128(w.pending.hi) == r.lo) {
		if cmp128(r.hi, w.pending.hi) > 0 {
			w.pending.hi = r.hi
		}
		return
	}
	w.flush()
	w.pending = r
	w.hasPending = true
}

func (w *setRangeWriter) flush() {
	if !w.hasPending {
		return
	}
	r := w.pending
	w.hasPending = false

	switch {
	case !w.started:
		id := r.lo.ksuid(r.timestamp)
		w.buf = append(w.buf, byte(rawKSUID))
		w.buf = append(w.buf, id[:]...)
		w.started = true
		w.timestamp = r.timestamp
		w.lastValue = r.lo

	case r.timestamp != w.timestamp:
		d := r.timestamp - w.timestamp
		n := varintLength32(d)
		id := r.lo.ksuid(r.timestamp)
		w.buf = append(w.buf, timeDelta|byte(n))
		w.buf = appendVarint32(w.buf, d, n)
		w.buf = append(w.buf, id[timestampLengthInBytes:]...)
		w.timestamp = r.timestamp
		w.lastValue = r.lo

	default:
		d := sub128(r.lo, w.lastValue)
		n := varintLength128(d)
		w.buf = append(w.buf, payloadDelta|byte(n))
		w.buf = appendVarint128(w.buf, d, n)
		w.lastValue = r.lo
	}

	// Emit the remainder of the interval as payload range records, chunked
	// to what a varint64 element count can express.
	for cmp128(w.lastValue, r.hi) < 0 {
		rem := sub128(r.hi, w.lastValue)
		m := rem[0]
		if rem[1] != 0 {
			m = math.MaxUint64
		}
		n := varintLength64(m)
		w.buf = append(w.buf, payloadRange|byte(n))
		w.buf = appendVarint64(w.buf, m, n)
		w.lastValue = add128(w.lastValue, makeUint128(0, m))
	}
}

func (w *setRangeWriter) finish() CompressedSet {
	w.flush()
	return CompressedSet(w.buf)
}

func min128(x, y uint128) uint128 {
	if cmp128(x, y) <= 0 {
		return x
	}
	return y
}

func max128(x, y uint128) uint128 {
	if cmp128(x, y) >= 0 {
		return x
	}
	return y
}

func decr128(x uint128) (z uint128) {
	var b uint64
	z[0], b = bits.Sub64(x[0], 1, 0)
	z[1] = x[1] - b
	return
}
