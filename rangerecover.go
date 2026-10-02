/**
 * Reed-Solomon Coding over 8-bit values.
 *
 * Copyright 2026, Klaus Post
 */

package reedsolomon

import (
	"errors"
	"fmt"
	"io"
)

// RangeRecoverer is an optional interface for encoders that can reconstruct
// a byte range of missing shards without reading the complete shards.
//
// This is useful when a small section of a single shard is damaged and
// reading every other shard in full would be wasteful.
//
// All Encoder implementations returned by New support this interface:
//
//   - The default matrix encoder (reedSolomon) has an alignment of 1 byte,
//     so exactly the requested range is read.
//   - The Leopard GF(2^16) encoder (leopardFF16) has a 64 byte alignment.
//   - The Leopard GF(2^8) encoder (leopardFF8) has a 64 byte alignment.
//
// Use a type assertion to check for support:
//
//	if rr, ok := enc.(reedsolomon.RangeRecoverer); ok { ... }
type RangeRecoverer interface {
	// RangeRecoveryAlignment returns the alignment in bytes that
	// RecoverRange requires. Reads may be expanded to a multiple of this
	// value on both sides of the requested range. Writes are never
	// expanded; only the requested range is written.
	RangeRecoveryAlignment() int64

	// RecoverRange recreates the byte range [off, off+length) of the
	// shards indicated by non-nil entries in 'fill'.
	//
	// 'valid' and 'fill' must both have TotalShards entries.
	// You indicate that a shard is missing by setting it to nil in 'valid'
	// and at the same time setting a non-nil writer in 'fill'.
	// An index cannot contain both a non-nil 'valid' and 'fill' entry;
	// ErrReconstructMismatch is returned in that case.
	//
	// Only the minimal range required by RangeRecoveryAlignment is read
	// from the valid shards: the requested range rounded outwards to the
	// alignment. No data outside [off, off+length) is ever written to the
	// fill writers.
	//
	// If a source shard is too short to cover the requested range, the
	// range is recovered as far as the sources allow and
	// io.ErrUnexpectedEOF is returned.
	//
	// If there are too few shards to reconstruct the missing ones,
	// ErrTooFewShards will be returned.
	//
	// The reconstructed range is complete, but integrity is not verified.
	// Use the Verify function to check if a data set is ok.
	RecoverRange(valid []io.ReaderAt, fill []io.WriterAt, off, length int64) error
}

var (
	_ RangeRecoverer = &reedSolomon{}
	_ RangeRecoverer = &leopardFF16{}
	_ RangeRecoverer = &leopardFF8{}
)

func (r *reedSolomon) RangeRecoveryAlignment() int64 {
	return 1
}

func (r *reedSolomon) RecoverRange(valid []io.ReaderAt, fill []io.WriterAt, off, length int64) error {
	return recoverRange(r, 1, valid, fill, off, length)
}

func (r *leopardFF16) RangeRecoveryAlignment() int64 {
	return 64
}

func (r *leopardFF16) RecoverRange(valid []io.ReaderAt, fill []io.WriterAt, off, length int64) error {
	return recoverRange(r, 64, valid, fill, off, length)
}

func (r *leopardFF8) RangeRecoveryAlignment() int64 {
	return 64
}

func (r *leopardFF8) RecoverRange(valid []io.ReaderAt, fill []io.WriterAt, off, length int64) error {
	return recoverRange(r, 64, valid, fill, off, length)
}

// recoverRange implements RangeRecoverer for an encoder by reading the
// alignment-expanded window of every valid shard, reconstructing the window
// in memory and writing back only the requested range.
func recoverRange(enc Encoder, align int64, valid []io.ReaderAt, fill []io.WriterAt, off, length int64) error {
	total := enc.(Extensions).TotalShards()
	if len(valid) != total || len(fill) != total {
		return ErrTooFewShards
	}
	if off < 0 || length < 0 {
		return errors.Join(ErrInvalidInput, fmt.Errorf("negative offset %d or length %d", off, length))
	}
	required := make([]bool, total)
	fills := 0
	for i := range valid {
		if valid[i] != nil && fill[i] != nil {
			return ErrReconstructMismatch
		}
		if fill[i] != nil {
			required[i] = true
			fills++
		}
	}
	if length == 0 || fills == 0 {
		return nil
	}

	// Expand the read window to the alignment, never beyond what is needed.
	start := off - off%align
	end := off + length
	if rem := end % align; rem != 0 {
		end += align - rem
	}
	window := int(end - start)

	// Read the window from all valid shards.
	// A short read (io.EOF) shrinks the recoverable window.
	shards := make([][]byte, total)
	avail := end
	for i, ra := range valid {
		if ra == nil {
			continue
		}
		buf := AllocAligned(1, window)[0]
		n, err := ra.ReadAt(buf, start)
		if err != nil && err != io.EOF {
			return StreamReadError{Err: err, Stream: i}
		}
		avail = min(avail, start+int64(n))
		shards[i] = buf
	}

	// Keep the reconstructed window aligned for the encoder.
	size := int(avail - start)
	size -= size % int(align)
	if size <= 0 {
		return io.ErrUnexpectedEOF
	}
	for i := range shards {
		switch {
		case shards[i] != nil:
			shards[i] = shards[i][:size]
		case fill[i] != nil:
			// Zero length marks the shard as missing; the capacity
			// will be used for the reconstructed data.
			shards[i] = AllocAligned(1, size)[0][:0]
		}
	}

	if err := enc.ReconstructSome(shards, required); err != nil {
		return err
	}

	// Write back only the requested range, clamped to what could be recovered.
	wstart := off - start
	wend := min(wstart+length, int64(size))
	if wstart >= wend {
		return io.ErrUnexpectedEOF
	}
	for i, wa := range fill {
		if wa == nil {
			continue
		}
		if _, err := wa.WriteAt(shards[i][wstart:wend], off); err != nil {
			return StreamWriteError{Err: err, Stream: i}
		}
	}
	if wend < wstart+length {
		return io.ErrUnexpectedEOF
	}
	return nil
}
