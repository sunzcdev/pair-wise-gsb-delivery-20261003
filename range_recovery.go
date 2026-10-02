package reedsolomon

import (
	"errors"
	"fmt"
	"io"
)

// RangeRecovery is an optional interface implemented by encoders that can
// reconstruct a byte range of one or more shards without touching the rest of
// the shards.
//
// This is useful when a small region of a single multi-gigabyte shard is
// corrupt: only the bytes needed to decode the requested range (plus any
// padding required by the coding alignment) are read from the source shards
// and written back to the destination shards.
//
// The classic Reed-Solomon encoder returned by [New] implements this
// interface. Leopard encoders do not; a type assertion should be used to
// detect support.
type RangeRecovery interface {
	// RecoverRange reconstructs length bytes starting at offset of one or
	// more shards.
	//
	// Shards to read from are passed as non-nil entries in valid; shards to
	// write the recovered bytes to are passed as non-nil entries in fill.
	// Each index must have exactly one of valid[i] or fill[i] set, otherwise
	// [ErrReconstructMismatch] is returned. Both slices must have length
	// TotalShards. At least DataShards valid shards are required.
	//
	// The coding kernel may require the read and written ranges to start and
	// end on an alignment boundary. The implementation transparently extends
	// the accessed range to the smallest aligned window that contains
	// [offset, offset+length); bytes outside the caller requested range are
	// decoded if needed, but are never written to fill.
	//
	// The reader may be backed by a shorter shard: reads running past its end
	// return [io.EOF]/[io.ErrUnexpectedEOF] and the missing tail is treated as
	// zero padding. A fill write that would run past the reconstructed shard
	// end is clipped, but offset must start inside the shard.
	RecoverRange(valid []io.ReaderAt, fill []io.WriterAt, offset, length int64) error
}

var _ RangeRecovery = (*reedSolomon)(nil)

// RecoverRange implements [RangeRecovery] for the classic Reed-Solomon
// encoder. The 8-bit field kernel operates on individual bytes, so the
// required alignment is 1 and no extra bytes beyond the requested range are
// accessed.
func (r *reedSolomon) RecoverRange(valid []io.ReaderAt, fill []io.WriterAt, offset, length int64) error {
	return r.recoverRange(valid, fill, offset, length, 1)
}

// ErrRangeInvalid is returned when a recovery range is invalid, for example a
// negative offset or length, or an offset/length that overflows.
var ErrRangeInvalid = errors.New("invalid recovery range")

// recoverRange contains the range reconstruction logic. align is the byte
// alignment of the coding kernel; the accessed window is expanded to the
// smallest aligned window covering [offset, offset+length). Only the classic
// Reed-Solomon kernel is exposed, which uses align == 1, but the alignment
// handling is kept general and verified directly in the tests.
func (r *reedSolomon) recoverRange(valid []io.ReaderAt, fill []io.WriterAt, offset, length int64, align int64) error {
	if len(valid) != r.totalShards || len(fill) != r.totalShards {
		return ErrTooFewShards
	}
	if offset < 0 || length <= 0 {
		return ErrRangeInvalid
	}
	if align <= 0 {
		return ErrRangeInvalid
	}
	reqEnd, ok := addOffset(offset, length)
	if !ok {
		return ErrRangeInvalid
	}

	// Collect the provided and the requested shard indices.
	validIndices := make([]int, 0, r.totalShards)
	fillIndices := make([]int, 0, r.totalShards)
	for i := 0; i < r.totalShards; i++ {
		hasValid := valid[i] != nil
		hasFill := fill[i] != nil
		if hasValid && hasFill {
			return ErrReconstructMismatch
		}
		if hasValid {
			validIndices = append(validIndices, i)
		} else if hasFill {
			fillIndices = append(fillIndices, i)
		}
	}
	if len(validIndices) < r.dataShards {
		return ErrTooFewShards
	}
	if len(fillIndices) == 0 {
		return nil
	}

	// Only the first DataShards valid shards participate in the decode, and
	// every requested shard (including parity) is reconstructed at once.
	useValid := validIndices[:r.dataShards]
	used := make(map[int]bool, r.dataShards)
	invalidIndices := make([]int, 0, r.totalShards-r.dataShards)
	for _, shardIdx := range useValid {
		used[shardIdx] = true
	}
	for i := 0; i < r.totalShards; i++ {
		if !used[i] {
			invalidIndices = append(invalidIndices, i)
		}
	}
	dataDecodeMatrix, err := r.getDecodeMatrix(useValid, invalidIndices)
	if err != nil {
		return err
	}

	// Build one decode matrix row per requested output shard.
	matrixRows := make([][]byte, len(fillIndices))
	for outIdx, shardIdx := range fillIndices {
		if shardIdx < r.dataShards {
			matrixRows[outIdx] = dataDecodeMatrix[shardIdx]
		} else {
			matrixRows[outIdx] = multiplyRowWithMatrix(r.parity[shardIdx-r.dataShards], dataDecodeMatrix)
		}
	}

	// Expand the requested range to the smallest aligned window.
	readStart := (offset / align) * align
	var readEnd int64
	if reqEnd%align == 0 {
		readEnd = reqEnd
	} else {
		aligned := (reqEnd/align + 1) * align
		if aligned < reqEnd {
			return ErrRangeInvalid
		}
		readEnd = aligned
	}
	windowLen := readEnd - readStart
	if windowLen > int64(maxInt) {
		return ErrRangeInvalid
	}

	inputs := make([][]byte, len(useValid))
	// shardEnd is the physical end of the equal-length shards. A source that
	// returns fewer bytes than requested reveals that the shard ends inside
	// the aligned window.
	shardEnd := readEnd
	for j, shardIdx := range useValid {
		buf := AllocAligned(1, int(windowLen))[0]
		n, err := readRangeAt(valid[shardIdx], buf, readStart)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return StreamReadError{Err: err, Stream: shardIdx}
		}
		// Bytes beyond the physical shard end act as zero padding.
		for k := n; k < len(buf); k++ {
			buf[k] = 0
		}
		inputs[j] = buf
		// Only a read that could not fill the window proves the shard ends
		// inside it; a full window may still be paired with io.EOF.
		if int64(n) < windowLen && readStart+int64(n) < shardEnd {
			shardEnd = readStart + int64(n)
		}
	}

	outputs := make([][]byte, len(fillIndices))
	for outIdx := range fillIndices {
		outputs[outIdx] = AllocAligned(1, int(windowLen))[0]
	}

	// The outputs start zeroed, so accumulate without clearing.
	r.codeSomeShards(matrixRows, inputs, outputs, int(windowLen), false)

	// Write back only the caller requested range, clipped to the shard end.
	for outIdx, shardIdx := range fillIndices {
		writeStart := offset
		writeEnd := reqEnd
		if writeStart >= shardEnd {
			return fmt.Errorf("%w: offset %d beyond shard end %d", ErrRangeInvalid, offset, shardEnd)
		}
		if writeEnd > shardEnd {
			writeEnd = shardEnd
		}
		seg := outputs[outIdx][writeStart-readStart : writeEnd-readStart]
		if err := writeRangeAt(fill[shardIdx], seg, writeStart); err != nil {
			return StreamWriteError{Err: err, Stream: shardIdx}
		}
	}
	return nil
}

// readRangeAt fills dst from r at off, retrying short reads until dst is full
// or the source reports an end/error. io.EOF/io.ErrUnexpectedEOF are
// returned unchanged when the source runs out of data, so a short result
// marks the physical shard end.
func readRangeAt(r io.ReaderAt, dst []byte, off int64) (int, error) {
	total := 0
	for total < len(dst) {
		next := off + int64(total)
		if next < off {
			return total, ErrRangeInvalid
		}
		n, rerr := r.ReadAt(dst[total:], next)
		if n > 0 {
			total += n
		}
		if rerr != nil {
			return total, rerr
		}
		if n == 0 {
			return total, io.ErrUnexpectedEOF
		}
	}
	return total, nil
}

// writeRangeAt writes all of p to w at off, retrying short writes.
func writeRangeAt(w io.WriterAt, p []byte, off int64) error {
	total := 0
	for total < len(p) {
		next := off + int64(total)
		if next < off {
			return ErrRangeInvalid
		}
		n, err := w.WriteAt(p[total:], next)
		if n > 0 {
			total += n
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// addOffset returns off+length and false on overflow.
func addOffset(off, length int64) (int64, bool) {
	end := off + length
	if end < off {
		return 0, false
	}
	return end, true
}
