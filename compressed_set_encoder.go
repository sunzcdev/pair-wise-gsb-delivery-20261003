package ksuid

import (
	"encoding/binary"
	"errors"
	"io"
)

var (
	errCompressedSetNotSorted = errors.New("ksuid: KSUIDs added to a compressed set encoder must be sorted in ascending order")
	errCompressedSetFinished  = errors.New("ksuid: cannot add KSUIDs to a compressed set encoder after Finish has been called")
)

// CompressedSetEncoder incrementally encodes a sorted, deduplicated stream of
// KSUIDs in the compressed-set format produced by Compress and
// AppendCompressed and decoded by CompressedSetIter.
//
// Whereas AppendCompressed needs the complete slice of KSUIDs up front, a
// CompressedSetEncoder writes a record to its io.Writer every time an ID can
// be committed; the full set of IDs is never buffered. The only state held
// back is the length of an open run of KSUIDs sharing a timestamp whose
// 16-byte payloads are consecutive: the run is encoded as a single
// payloadRange record, which is emitted only when the run closes (a
// non-consecutive payload, a timestamp change, or Finish).
//
// IDs must be added in ascending order; duplicate IDs are silently ignored.
// Adding an ID smaller than the previous one fails when that ID is consumed.
//
// Writes follow the io.Writer contract: a Write that reports an error,
// including io.ErrShortWrite when it accepts no byte without an error, puts
// the encoder in a sticky failure state: every subsequent Add or Finish call
// returns the same failure and no further byte is ever produced, so a reader
// cannot mistake later output for a valid suffix. Bytes already written when
// a failure occurs must be discarded. The compressed format is a
// forward-only record stream with no end marker and no checksum, so a
// truncated stream either ends in the middle of a record (undecodable) or
// decodes as an arbitrary prefix subset of the IDs, with nothing telling the
// reader that the set is incomplete; it can therefore never be used as the
// complete set.
//
// Finish is idempotent: the trailing payloadRange record, if any, is written
// at most once, and the underlying io.Writer is never closed by the encoder.
//
// CompressedSetEncoder values are not safe for concurrent use.
type CompressedSetEncoder struct {
	w   io.Writer
	err error

	finished bool
	hasLast  bool

	timestamp uint32
	lastValue uint128

	// pendingRange is the number of consecutive KSUIDs, ending at lastValue,
	// whose payload equals the previous payload plus one. The IDs themselves
	// are not retained: only this counter is, until the run closes.
	pendingRange uint64
}

// NewCompressedSetEncoder returns a CompressedSetEncoder that writes the
// compressed representation of the KSUID stream to w.
func NewCompressedSetEncoder(w io.Writer) *CompressedSetEncoder {
	return &CompressedSetEncoder{w: w}
}

// Add feeds KSUIDs to the encoder. The IDs must be sorted in ascending order
// relative to the stream passed to previous Add calls; duplicates are
// ignored, and an out-of-order ID makes the encoder fail permanently.
//
// Add returns the first write error encountered, after which the encoder is
// in a sticky failure state and Add must not be expected to emit anything
// else.
func (e *CompressedSetEncoder) Add(ids ...KSUID) error {
	if e.err != nil {
		return e.err
	}
	if e.finished {
		// A programming error, not a write failure: nothing was emitted, so
		// the sticky failure state is not engaged and Finish keeps returning
		// nil.
		return errCompressedSetFinished
	}
	for i := range ids {
		if err := e.add(ids[i]); err != nil {
			return err
		}
	}
	return nil
}

// Finish flushes the record of the payload run still open, if any, and marks
// the encoder as finished. It does not close the underlying io.Writer.
//
// An encoder with no ID produces no output. Finish may be called more than
// once: the trailing record is written at most once and repeated calls
// return the first error, if any.
func (e *CompressedSetEncoder) Finish() error {
	if e.err != nil {
		return e.err
	}
	if e.finished {
		return nil
	}
	if err := e.closeRange(); err != nil {
		return err
	}
	e.finished = true
	return nil
}

func (e *CompressedSetEncoder) add(id KSUID) error {
	t := id.Timestamp()
	v := uint128Payload(id)

	if !e.hasLast {
		// The first KSUID is always stored verbatim, it is the origin of all
		// later deltas.
		var rec [1 + byteLength]byte
		rec[0] = rawKSUID
		copy(rec[1:], id[:])
		if err := e.write(rec[:]); err != nil {
			return err
		}
		e.hasLast = true
		e.timestamp = t
		e.lastValue = v
		return nil
	}

	if t < e.timestamp {
		return e.fail(errCompressedSetNotSorted)
	}

	if t == e.timestamp {
		switch cmp128(v, e.lastValue) {
		case -1:
			// Same timestamp but a smaller payload: the stream is not sorted.
			return e.fail(errCompressedSetNotSorted)
		case 0:
			// Duplicate of the most recently committed ID, including one that
			// repeats inside an open run: ignore it and keep the run alive.
			return nil
		}

		one := makeUint128(0, 1)
		if sub128(v, e.lastValue) == one {
			// Extend the open run; only the counter is kept.
			e.pendingRange++
			e.lastValue = v
			return nil
		}

		if err := e.closeRange(); err != nil {
			return err
		}

		d := sub128(v, e.lastValue)
		n := varintLength128(d)

		var rec [1 + payloadLengthInBytes]byte
		rec[0] = payloadDelta | byte(n)
		c := d.bytes()
		copy(rec[1:1+n], c[payloadLengthInBytes-n:])
		if err := e.write(rec[:1+n]); err != nil {
			return err
		}

		e.lastValue = v
		return nil
	}

	// The timestamp advanced: close the run of the previous timestamp first,
	// then emit the time delta followed by the full payload.
	if err := e.closeRange(); err != nil {
		return err
	}

	d := t - e.timestamp
	n := varintLength32(d)

	var rec [1 + timestampLengthInBytes + payloadLengthInBytes]byte
	rec[0] = timeDelta | byte(n)
	var tb [timestampLengthInBytes]byte
	binary.BigEndian.PutUint32(tb[:], d)
	copy(rec[1:1+n], tb[timestampLengthInBytes-n:])
	copy(rec[1+n:], id[timestampLengthInBytes:])
	if err := e.write(rec[:1+n+payloadLengthInBytes]); err != nil {
		return err
	}

	e.timestamp = t
	e.lastValue = v
	return nil
}

// closeRange emits the pending payloadRange record, if any.
func (e *CompressedSetEncoder) closeRange() error {
	if e.pendingRange == 0 {
		return nil
	}

	m := e.pendingRange
	n := varintLength64(m)

	var rec [1 + 8]byte
	rec[0] = payloadRange | byte(n)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], m)
	copy(rec[1:1+n], c[8-n:])
	if err := e.write(rec[:1+n]); err != nil {
		return err
	}

	e.pendingRange = 0
	return nil
}

// write sends p to the underlying writer, retrying from the unwritten suffix
// when the writer accepts a prefix without an error. Any error, a zero
// progress write, or an invalid byte count puts the encoder in its sticky
// failure state.
func (e *CompressedSetEncoder) write(p []byte) error {
	if e.err != nil {
		return e.err
	}
	for len(p) > 0 {
		n, err := e.w.Write(p)
		if err != nil {
			e.err = err
			return err
		}
		if n <= 0 || n > len(p) {
			e.err = io.ErrShortWrite
			return e.err
		}
		p = p[n:]
	}
	return nil
}

func (e *CompressedSetEncoder) fail(err error) error {
	e.err = err
	return err
}
