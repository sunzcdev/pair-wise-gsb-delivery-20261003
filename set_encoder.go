package ksuid

import (
	"errors"
	"io"
)

var (
	// ErrOutOfOrder is returned by (*CompressedSetEncoder).Add when the id
	// passed to the method does not sort after the most recently added id.
	ErrOutOfOrder = errors.New("ksuid: cannot add out-of-order id to compressed set encoder")

	// ErrEncoderClosed is returned by (*CompressedSetEncoder).Add when the
	// method is called after Finish.
	ErrEncoderClosed = errors.New("ksuid: cannot add id to compressed set encoder after Finish")
)

// CompressedSetEncoder incrementally encodes a compressed set of KSUIDs to
// an io.Writer, producing the same format as Compress and AppendCompressed.
//
// Unlike AppendCompressed, the encoder does not need to buffer the whole
// set: ids are streamed in one at a time with Add, and Finish flushes the
// pending state. The encoder only tracks the last id and the length of a
// pending run of consecutive payloads, so its memory usage is constant
// regardless of the size of the set.
//
// Ids must be added in ascending order. Duplicates of the most recently
// added id are silently ignored; an id that sorts before the most recently
// added one causes Add to return ErrOutOfOrder. The encoder state is left
// unchanged in this case, so the caller may keep adding valid ids.
//
// If a write to the underlying writer fails, the error is returned and the
// encoder enters a sticky failure state: all subsequent calls to Add and
// Finish return the same error and no further bytes are written. Short
// writes (n < len(p) with a nil error) are treated as io.ErrShortWrite, as
// mandated by the io.Writer contract.
//
// The compressed format has no length prefix, checksum, or end-of-stream
// marker: the decoder reads records until the input is exhausted, so any
// prefix of a valid stream is itself a valid stream which decodes to a
// subset of the ids, and a record truncated by a failed write may be
// misread. Bytes written before a failure must therefore be discarded;
// they cannot be completed, nor consumed as if they represented the full
// set.
//
// Finish flushes any pending run of consecutive ids. It is safe to call
// Finish multiple times: only the first call may write to the underlying
// writer. The encoder does not close the writer; that remains the
// responsibility of the caller.
//
// CompressedSetEncoder values are not safe to use concurrently from
// multiple goroutines.
type CompressedSetEncoder struct {
	w io.Writer

	err      error
	started  bool
	finished bool

	timestamp uint32
	lastID    KSUID
	lastValue uint128
	runLength uint64
}

// NewCompressedSetEncoder creates and returns an encoder writing a
// compressed set of KSUIDs to w.
func NewCompressedSetEncoder(w io.Writer) *CompressedSetEncoder {
	return &CompressedSetEncoder{w: w}
}

// Add encodes id into the compressed set. The id must sort after the most
// recently added one; duplicates of it are ignored.
func (enc *CompressedSetEncoder) Add(id KSUID) error {
	if enc.err != nil {
		return enc.err
	}
	if enc.finished {
		return ErrEncoderClosed
	}

	if !enc.started {
		// The first KSUID is always written in full, this is the starting
		// point for all deltas.
		b := [1 + byteLength]byte{}
		b[0] = byte(rawKSUID)
		copy(b[1:], id[:])

		if err := enc.write(b[:]); err != nil {
			return err
		}

		enc.started = true
		enc.timestamp = id.Timestamp()
		enc.lastValue = uint128Payload(id)
		enc.lastID = id
		return nil
	}

	if id == enc.lastID {
		return nil
	}

	one := makeUint128(0, 1)
	t := id.Timestamp()
	v := uint128Payload(id)

	if t != enc.timestamp {
		if t < enc.timestamp {
			return ErrOutOfOrder
		}

		if err := enc.flushRun(); err != nil {
			return err
		}

		d := t - enc.timestamp
		n := varintLength32(d)

		b := [1 + 4 + payloadLengthInBytes]byte{}
		b[0] = timeDelta | byte(n)
		c := appendVarint32(b[:1], d, n)
		c = append(c, id[timestampLengthInBytes:]...)

		if err := enc.write(c); err != nil {
			return err
		}

		enc.timestamp = t
	} else {
		if cmp128(v, enc.lastValue) < 0 {
			return ErrOutOfOrder
		}

		d := sub128(v, enc.lastValue)

		if d != one {
			if err := enc.flushRun(); err != nil {
				return err
			}

			n := varintLength128(d)

			b := [1 + 16]byte{}
			b[0] = payloadDelta | byte(n)
			c := appendVarint128(b[:1], d, n)

			if err := enc.write(c); err != nil {
				return err
			}
		} else {
			// The id extends a run of consecutive payloads: only the run
			// length is recorded, the run is encoded when it is broken by
			// a non-consecutive id or when Finish is called.
			enc.runLength++
		}
	}

	enc.lastValue = v
	enc.lastID = id
	return nil
}

// Finish flushes any pending encoder state to the underlying writer. It may
// be called multiple times; only the first call may write to the writer.
// The writer is not closed.
func (enc *CompressedSetEncoder) Finish() error {
	if enc.err != nil {
		return enc.err
	}
	if enc.finished {
		return nil
	}
	enc.finished = true
	return enc.flushRun()
}

// flushRun encodes a pending run of consecutive payloads, if any.
func (enc *CompressedSetEncoder) flushRun() error {
	if enc.runLength != 0 {
		n := varintLength64(enc.runLength)

		b := [1 + 8]byte{}
		b[0] = payloadRange | byte(n)
		c := appendVarint64(b[:1], enc.runLength, n)

		if err := enc.write(c); err != nil {
			return err
		}

		enc.runLength = 0
	}
	return nil
}

func (enc *CompressedSetEncoder) write(b []byte) error {
	n, err := enc.w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		enc.err = err
	}
	return err
}
