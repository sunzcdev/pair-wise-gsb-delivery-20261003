package ksuid

import (
	"errors"
	"fmt"
)

// CompressedSetError is the error type returned by Union, Intersect, and
// Subtract when one of the two input sets is malformed.
type CompressedSetError struct {
	// Source identifies which of the two input sets contained the malformed
	// record: "left" for the first operand, or "right" for the second.
	Source string

	// Offset is the byte offset of the malformed record within the source
	// set.
	Offset int

	// Err describes the corruption that was detected.
	Err error
}

// Error satisfies the error interface.
func (e *CompressedSetError) Error() string {
	return fmt.Sprintf("ksuid: malformed compressed set (%s set, offset %d): %v", e.Source, e.Offset, e.Err)
}

// Unwrap returns the underlying cause of the error.
func (e *CompressedSetError) Unwrap() error { return e.Err }

var (
	errTruncatedSet      = errors.New("unexpected end of compressed set")
	errZeroTimeDelta     = errors.New("timestamp delta of zero")
	errZeroPayloadDelta  = errors.New("payload delta of zero")
	errEmptyPayloadRange = errors.New("payload range of zero")
	errPayloadOverflow   = errors.New("payload overflow")
	errNotAscending      = errors.New("records are not in strictly ascending order")
)

var maxUint128 = uint128{^uint64(0), ^uint64(0)}

// Union returns the set union of a and b, computed directly on the compressed
// representation without expanding contiguous ranges into individual KSUIDs.
//
// The result is a new CompressedSet in the same format as produced by
// Compress: strictly ascending, deduplicated, and compatible with the
// CompressedSetIter decoder. Neither a nor b is modified, and the returned
// set does not share memory with the inputs.
//
// If either input is malformed, the function returns a nil set and a
// *CompressedSetError identifying the source set and the offset of the
// invalid record; no partial result is delivered.
func Union(a, b CompressedSet) (CompressedSet, error) {
	sa := compressedSetStream{source: "left", data: []byte(a)}
	sb := compressedSetStream{source: "right", data: []byte(b)}
	w := compressedSetWriter{out: make([]byte, 0, len(a)+len(b))}

	aok, bok := sa.next(), sb.next()
	for aok || bok {
		if aok && (!bok || sa.before(&sb)) {
			w.append(sa.segTs, sa.segStart, sa.segEnd)
			aok = sa.next()
		} else {
			w.append(sb.segTs, sb.segStart, sb.segEnd)
			bok = sb.next()
		}
	}

	if err := sa.Err(); err != nil {
		return nil, err
	}
	if err := sb.Err(); err != nil {
		return nil, err
	}
	return w.finish(), nil
}

// Intersect returns the set intersection of a and b, computed directly on the
// compressed representation without expanding contiguous ranges into
// individual KSUIDs.
//
// The result is a new CompressedSet in the same format as produced by
// Compress: strictly ascending, deduplicated, and compatible with the
// CompressedSetIter decoder. Neither a nor b is modified, and the returned
// set does not share memory with the inputs.
//
// If either input is malformed, the function returns a nil set and a
// *CompressedSetError identifying the source set and the offset of the
// invalid record; no partial result is delivered.
func Intersect(a, b CompressedSet) (CompressedSet, error) {
	sa := compressedSetStream{source: "left", data: []byte(a)}
	sb := compressedSetStream{source: "right", data: []byte(b)}

	hint := len(a)
	if len(b) < hint {
		hint = len(b)
	}
	w := compressedSetWriter{out: make([]byte, 0, hint)}

	aok, bok := sa.next(), sb.next()
	for aok && bok {
		switch {
		case sa.segTs < sb.segTs:
			aok = sa.next()
		case sb.segTs < sa.segTs:
			bok = sb.next()
		default:
			aEnd, bEnd := sa.segEnd, sb.segEnd
			lo := max128(sa.segStart, sb.segStart)
			hi := min128(aEnd, bEnd)
			if cmp128(lo, hi) <= 0 {
				w.append(sa.segTs, lo, hi)
			}
			if cmp128(aEnd, bEnd) <= 0 {
				aok = sa.next()
			}
			if cmp128(bEnd, aEnd) <= 0 {
				bok = sb.next()
			}
		}
	}

	// Drain both inputs so malformed records in the unread tail are still
	// reported instead of delivering a partial result.
	sa.drain()
	sb.drain()

	if err := sa.Err(); err != nil {
		return nil, err
	}
	if err := sb.Err(); err != nil {
		return nil, err
	}
	return w.finish(), nil
}

// Subtract returns the set difference of a and b (the elements of a that are
// not present in b), computed directly on the compressed representation
// without expanding contiguous ranges into individual KSUIDs.
//
// The result is a new CompressedSet in the same format as produced by
// Compress: strictly ascending, deduplicated, and compatible with the
// CompressedSetIter decoder. Neither a nor b is modified, and the returned
// set does not share memory with the inputs.
//
// If either input is malformed, the function returns a nil set and a
// *CompressedSetError identifying the source set and the offset of the
// invalid record; no partial result is delivered.
func Subtract(a, b CompressedSet) (CompressedSet, error) {
	sa := compressedSetStream{source: "left", data: []byte(a)}
	sb := compressedSetStream{source: "right", data: []byte(b)}
	w := compressedSetWriter{out: make([]byte, 0, len(a))}

	aok, bok := sa.next(), sb.next()
	for aok {
		switch {
		case !bok || sb.segTs > sa.segTs:
			w.append(sa.segTs, sa.segStart, sa.segEnd)
			aok = sa.next()
		case sb.segTs < sa.segTs:
			bok = sb.next()
		default:
			aStart, aEnd := sa.segStart, sa.segEnd
			bStart, bEnd := sb.segStart, sb.segEnd
			switch {
			case cmp128(bEnd, aStart) < 0:
				bok = sb.next()
			case cmp128(bStart, aEnd) > 0:
				w.append(sa.segTs, aStart, aEnd)
				aok = sa.next()
			default:
				// The segments overlap: emit the part of a that precedes
				// b, then keep the remainder of a (if any) for the next
				// round.
				if cmp128(bStart, aStart) > 0 {
					w.append(sa.segTs, aStart, decr128(bStart))
				}
				if cmp128(bEnd, aEnd) >= 0 {
					aok = sa.next()
				} else {
					sa.segStart = incr128(bEnd)
					bok = sb.next()
				}
			}
		}
	}

	// Drain both inputs so malformed records in the unread tail are still
	// reported instead of delivering a partial result.
	sa.drain()
	sb.drain()

	if err := sa.Err(); err != nil {
		return nil, err
	}
	if err := sb.Err(); err != nil {
		return nil, err
	}
	return w.finish(), nil
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

// compressedSetStream decodes a compressed set as a sequence of contiguous
// segments. A segment represents the KSUIDs sharing one timestamp with
// payloads in [segStart, segEnd] (inclusive bounds), so contiguous runs of
// any length are handled in constant space and time instead of being
// expanded into individual elements.
type compressedSetStream struct {
	source string
	data   []byte
	off    int

	timestamp uint32
	lastValue uint128
	hasLast   bool

	segTs    uint32
	segStart uint128
	segEnd   uint128

	hasStash   bool
	stashTs    uint32
	stashStart uint128
	stashEnd   uint128

	err error
}

// Err returns the first decoding error encountered by the stream, if any.
func (s *compressedSetStream) Err() error { return s.err }

// before reports whether the current segment of s sorts before the current
// segment of o.
func (s *compressedSetStream) before(o *compressedSetStream) bool {
	if s.segTs != o.segTs {
		return s.segTs < o.segTs
	}
	return cmp128(s.segStart, o.segStart) < 0
}

// drain consumes the rest of the stream, discarding the segments.
func (s *compressedSetStream) drain() {
	for s.next() {
	}
}

// next advances the stream to the next contiguous segment, returning false
// when the set is exhausted or a decoding error occurred (see Err).
//
// Records that are contiguous with the current segment (same timestamp, payload
// immediately following the segment end) are folded into it, so a segment
// may span multiple encoded records.
func (s *compressedSetStream) next() bool {
	if s.err != nil {
		return false
	}

	if s.hasStash {
		s.hasStash = false
		s.segTs, s.segStart, s.segEnd = s.stashTs, s.stashStart, s.stashEnd
	} else {
		if s.off == len(s.data) {
			return false
		}
		ts, start, end, ok := s.readRecord()
		if !ok {
			return false
		}
		s.segTs, s.segStart, s.segEnd = ts, start, end
	}

	for s.off < len(s.data) {
		ts, start, end, ok := s.readRecord()
		if !ok {
			return false
		}
		if ts == s.segTs && s.segEnd != maxUint128 && incr128(s.segEnd) == start {
			s.segEnd = end
			continue
		}
		s.hasStash = true
		s.stashTs, s.stashStart, s.stashEnd = ts, start, end
		break
	}
	return true
}

// readRecord decodes the record at the current offset, returning the
// timestamp and inclusive payload bounds it describes. It returns false when
// the input is exhausted or malformed; in the latter case the stream error
// is set (see Err).
func (s *compressedSetStream) readRecord() (ts uint32, start, end uint128, ok bool) {
	if s.err != nil || s.off == len(s.data) {
		return 0, uint128{}, uint128{}, false
	}

	recOffset := s.off
	fail := func(err error) (uint32, uint128, uint128, bool) {
		s.err = &CompressedSetError{Source: s.source, Offset: recOffset, Err: err}
		return 0, uint128{}, uint128{}, false
	}

	b := s.data[s.off]
	s.off++

	const mask = rawKSUID | timeDelta | payloadDelta | payloadRange
	tag := int(b) & mask
	cnt := int(b) &^ mask

	read := func(n int) []byte {
		if len(s.data)-s.off < n {
			return nil
		}
		p := s.data[s.off : s.off+n]
		s.off += n
		return p
	}

	switch tag {
	case rawKSUID:
		if cnt != 0 {
			return fail(fmt.Errorf("invalid raw KSUID record header 0x%02X", b))
		}
		p := read(byteLength)
		if p == nil {
			return fail(errTruncatedSet)
		}
		var id KSUID
		copy(id[:], p)
		ts = id.Timestamp()
		start = uint128Payload(id)
		end = start

	case timeDelta:
		if cnt < 1 || cnt > timestampLengthInBytes {
			return fail(fmt.Errorf("invalid timestamp delta length %d", cnt))
		}
		p := read(cnt + payloadLengthInBytes)
		if p == nil {
			return fail(errTruncatedSet)
		}
		d := varint32(p[:cnt])
		if d == 0 {
			return fail(errZeroTimeDelta)
		}
		ts = s.timestamp + d
		if ts < s.timestamp {
			return fail(errPayloadOverflow)
		}
		start = makeUint128FromPayload(p[cnt:])
		end = start

	case payloadDelta:
		if cnt < 1 || cnt > payloadLengthInBytes {
			return fail(fmt.Errorf("invalid payload delta length %d", cnt))
		}
		p := read(cnt)
		if p == nil {
			return fail(errTruncatedSet)
		}
		d := varint128(p)
		if d == (uint128{}) {
			return fail(errZeroPayloadDelta)
		}
		start = add128(s.lastValue, d)
		if cmp128(start, s.lastValue) <= 0 {
			return fail(errPayloadOverflow)
		}
		ts = s.timestamp
		end = start

	case payloadRange:
		if cnt < 1 || cnt > 8 {
			return fail(fmt.Errorf("invalid payload range length %d", cnt))
		}
		p := read(cnt)
		if p == nil {
			return fail(errTruncatedSet)
		}
		m := varint64(p)
		if m == 0 {
			return fail(errEmptyPayloadRange)
		}
		if s.lastValue == maxUint128 {
			return fail(errPayloadOverflow)
		}
		start = incr128(s.lastValue)
		end = add128(s.lastValue, makeUint128(0, m))
		if cmp128(end, start) < 0 {
			return fail(errPayloadOverflow)
		}
		ts = s.timestamp

	default:
		return fail(fmt.Errorf("invalid record header 0x%02X", b))
	}

	if s.hasLast {
		if ts < s.timestamp || (ts == s.timestamp && cmp128(start, s.lastValue) <= 0) {
			return fail(errNotAscending)
		}
	}

	s.timestamp = ts
	s.lastValue = end
	s.hasLast = true
	return ts, start, end, true
}

// compressedSetWriter encodes a sequence of contiguous segments into the
// compressed set format. Segments must be appended in ascending order;
// overlapping or adjacent segments sharing a timestamp are coalesced, so the
// output is always strictly ascending and deduplicated.
type compressedSetWriter struct {
	out       []byte
	timestamp uint32
	lastValue uint128
	started   bool

	pendTs    uint32
	pendStart uint128
	pendEnd   uint128
	hasPend   bool
}

// append adds the segment of payloads [start, end] at the given timestamp to
// the set being built.
func (w *compressedSetWriter) append(ts uint32, start, end uint128) {
	if w.hasPend && w.pendTs == ts {
		if cmp128(start, w.pendEnd) <= 0 {
			if cmp128(end, w.pendEnd) > 0 {
				w.pendEnd = end
			}
			return
		}
		if w.pendEnd != maxUint128 && incr128(w.pendEnd) == start {
			w.pendEnd = end
			return
		}
	}
	w.flush()
	w.hasPend = true
	w.pendTs, w.pendStart, w.pendEnd = ts, start, end
}

// finish flushes the pending segment and returns the encoded set.
func (w *compressedSetWriter) finish() CompressedSet {
	w.flush()
	return CompressedSet(w.out)
}

func (w *compressedSetWriter) flush() {
	if !w.hasPend {
		return
	}
	w.hasPend = false

	switch {
	case !w.started:
		id := w.pendStart.ksuid(w.pendTs)
		w.out = append(w.out, byte(rawKSUID))
		w.out = append(w.out, id[:]...)
		w.started = true
		w.timestamp = w.pendTs

	case w.pendTs == w.timestamp:
		// Adjacent segments were coalesced by append, so the delta to the
		// last written value is always greater than one here.
		delta := sub128(w.pendStart, w.lastValue)
		n := varintLength128(delta)
		w.out = append(w.out, payloadDelta|byte(n))
		w.out = appendVarint128(w.out, delta, n)

	default:
		d := w.pendTs - w.timestamp
		n := varintLength32(d)
		w.out = append(w.out, timeDelta|byte(n))
		w.out = appendVarint32(w.out, d, n)
		id := w.pendStart.ksuid(w.pendTs)
		w.out = append(w.out, id[timestampLengthInBytes:]...)
		w.timestamp = w.pendTs
	}

	w.lastValue = w.pendStart
	w.emitRange(sub128(w.pendEnd, w.pendStart))
	w.lastValue = w.pendEnd
}

// emitRange encodes count consecutive payloads following the last value
// written to the set, splitting the run into multiple range records if it
// exceeds what a single record can hold.
func (w *compressedSetWriter) emitRange(count uint128) {
	for count != (uint128{}) {
		chunk := count[0]
		if count[1] != 0 {
			chunk = ^uint64(0)
		}
		n := varintLength64(chunk)
		w.out = append(w.out, payloadRange|byte(n))
		w.out = appendVarint64(w.out, chunk, n)
		count = sub128(count, makeUint128(0, chunk))
	}
}
