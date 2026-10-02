package reedsolomon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

// rangeOp records a single ReadAt/WriteAt call.
type rangeOp struct {
	shard int
	write bool
	off   int64
	n     int
}

// rangeOpLog collects I/O operations from the recording proxies.
type rangeOpLog struct {
	ops []rangeOp
}

func (l *rangeOpLog) reads() []rangeOp {
	var out []rangeOp
	for _, op := range l.ops {
		if !op.write {
			out = append(out, op)
		}
	}
	return out
}

func (l *rangeOpLog) writes() []rangeOp {
	var out []rangeOp
	for _, op := range l.ops {
		if op.write {
			out = append(out, op)
		}
	}
	return out
}

// recordingReaderAt wraps a shard and logs every ReadAt.
type recordingReaderAt struct {
	shard int
	data  []byte
	log   *rangeOpLog
}

func (r *recordingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.log.ops = append(r.log.ops, rangeOp{shard: r.shard, off: off, n: len(p)})
	return bytes.NewReader(r.data).ReadAt(p, off)
}

// recordingWriterAt writes into a shard and logs every WriteAt.
// Writes outside the backing slice are rejected.
type recordingWriterAt struct {
	shard int
	data  []byte
	log   *rangeOpLog
}

func (w *recordingWriterAt) WriteAt(p []byte, off int64) (int, error) {
	w.log.ops = append(w.log.ops, rangeOp{shard: w.shard, write: true, off: off, n: len(p)})
	if off < 0 || off+int64(len(p)) > int64(len(w.data)) {
		return 0, fmt.Errorf("write of %d bytes at offset %d exceeds shard size %d", len(p), off, len(w.data))
	}
	copy(w.data[off:], p)
	return len(p), nil
}

// makeRangeShards creates deterministic data shards and encodes them.
func makeRangeShards(t *testing.T, enc Encoder, shardSize int) [][]byte {
	t.Helper()
	ext := enc.(Extensions)
	rng := rand.New(rand.NewSource(0x5eed))
	shards := make([][]byte, ext.TotalShards())
	for i := range shards {
		shards[i] = make([]byte, shardSize)
		if i < ext.DataShards() {
			rng.Read(shards[i])
		}
	}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	return shards
}

// rangeRecoverSetup corrupts [off, off+length) of shard 'target' and returns
// the valid/fill slices, the I/O log and the (corrupted) target shard.
func rangeRecoverSetup(shards [][]byte, target int, off, length int64) ([]io.ReaderAt, []io.WriterAt, *rangeOpLog, []byte) {
	log := &rangeOpLog{}
	corrupt := make([]byte, len(shards[target]))
	copy(corrupt, shards[target])
	for i := off; i < off+length; i++ {
		corrupt[i] ^= 0xa5
	}
	valid := make([]io.ReaderAt, len(shards))
	fill := make([]io.WriterAt, len(shards))
	for i := range shards {
		if i == target {
			fill[i] = &recordingWriterAt{shard: i, data: corrupt, log: log}
		} else {
			valid[i] = &recordingReaderAt{shard: i, data: shards[i], log: log}
		}
	}
	return valid, fill, log, corrupt
}

// checkRangeIO verifies that all logged reads stay within [readStart, readEnd)
// and all writes stay within [writeStart, writeEnd).
func checkRangeIO(t *testing.T, log *rangeOpLog, readStart, readEnd, writeStart, writeEnd int64) {
	t.Helper()
	for _, op := range log.reads() {
		if op.off < readStart || op.off+int64(op.n) > readEnd {
			t.Errorf("read of shard %d at [%d, %d) outside allowed [%d, %d)",
				op.shard, op.off, op.off+int64(op.n), readStart, readEnd)
		}
	}
	for _, op := range log.writes() {
		if op.off < writeStart || op.off+int64(op.n) > writeEnd {
			t.Errorf("write of shard %d at [%d, %d) outside requested [%d, %d)",
				op.shard, op.off, op.off+int64(op.n), writeStart, writeEnd)
		}
	}
}

// checkSentinels verifies that the target outside [off, off+length) still
// holds the original data and that the range itself was recovered.
func checkSentinels(t *testing.T, got, want []byte, off, length int64) {
	t.Helper()
	if !bytes.Equal(got[:off], want[:off]) {
		t.Errorf("sentinel bytes before offset %d were modified", off)
	}
	if !bytes.Equal(got[off+length:], want[off+length:]) {
		t.Errorf("sentinel bytes after offset %d were modified", off+length)
	}
	if !bytes.Equal(got[off:off+length], want[off:off+length]) {
		t.Errorf("range [%d, %d) was not recovered correctly", off, off+length)
	}
}

// TestRangeRecovererDeclared checks that all encoders returned by New
// declare RangeRecoverer support with the documented alignment.
func TestRangeRecovererDeclared(t *testing.T) {
	tests := []struct {
		name  string
		opts  []Option
		align int64
	}{
		{"matrix", nil, 1},
		{"leopard-gf8", []Option{WithLeopardGF(true)}, 64},
		{"leopard-gf16", []Option{WithLeopardGF16(true)}, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := New(5, 3, tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			rr, ok := enc.(RangeRecoverer)
			if !ok {
				t.Fatalf("encoder does not implement RangeRecoverer")
			}
			if got := rr.RangeRecoveryAlignment(); got != tt.align {
				t.Errorf("RangeRecoveryAlignment() = %d, want %d", got, tt.align)
			}
			if got, want := rr.RangeRecoveryAlignment(), int64(enc.(Extensions).ShardSizeMultiple()); got != want {
				t.Errorf("RangeRecoveryAlignment() = %d, ShardSizeMultiple() = %d", got, want)
			}
		})
	}
}

// TestRangeRecoverMatrix verifies exact-range I/O for the matrix encoder,
// which has an alignment of 1 byte.
func TestRangeRecoverMatrix(t *testing.T) {
	enc, err := New(5, 3)
	if err != nil {
		t.Fatal(err)
	}
	const shardSize = 1000 // deliberately not a multiple of 64
	const off, length = int64(100), int64(61)
	const target = 2

	shards := makeRangeShards(t, enc, shardSize)
	valid, fill, log, corrupt := rangeRecoverSetup(shards, target, off, length)

	rr := enc.(RangeRecoverer)
	if err := rr.RecoverRange(valid, fill, off, length); err != nil {
		t.Fatal(err)
	}

	// Alignment 1: reads must cover exactly the requested range.
	reads := log.reads()
	if len(reads) != len(shards)-1 {
		t.Fatalf("expected %d reads, got %d", len(shards)-1, len(reads))
	}
	for _, op := range reads {
		if op.off != off || op.n != int(length) {
			t.Errorf("read of shard %d at [%d, %d), want exact [%d, %d)",
				op.shard, op.off, op.off+int64(op.n), off, off+length)
		}
	}
	writes := log.writes()
	if len(writes) != 1 || writes[0].shard != target || writes[0].off != off || writes[0].n != int(length) {
		t.Errorf("unexpected writes: %+v", writes)
	}
	checkRangeIO(t, log, off, off+length, off, off+length)
	checkSentinels(t, corrupt, shards[target], off, length)
}

// TestRangeRecoverAlignmentBoundary verifies that a range crossing a 64-byte
// alignment boundary expands reads minimally, while writes stay exact.
func TestRangeRecoverAlignmentBoundary(t *testing.T) {
	for _, opts := range [][]Option{{WithLeopardGF(true)}, {WithLeopardGF16(true)}} {
		enc, err := New(5, 3, opts...)
		if err != nil {
			t.Fatal(err)
		}
		const shardSize = 1024
		const off, length = int64(70), int64(60) // crosses the 128 boundary
		const target = 3
		// Minimal 64-byte expansion of [70, 130).
		const readStart, readEnd = int64(64), int64(192)

		shards := makeRangeShards(t, enc, shardSize)
		valid, fill, log, corrupt := rangeRecoverSetup(shards, target, off, length)

		rr := enc.(RangeRecoverer)
		if err := rr.RecoverRange(valid, fill, off, length); err != nil {
			t.Fatal(err)
		}

		for _, op := range log.reads() {
			if op.off != readStart || op.n != int(readEnd-readStart) {
				t.Errorf("read of shard %d at [%d, %d), want expanded [%d, %d)",
					op.shard, op.off, op.off+int64(op.n), readStart, readEnd)
			}
		}
		writes := log.writes()
		if len(writes) != 1 || writes[0].off != off || writes[0].n != int(length) {
			t.Errorf("writes must cover exactly the requested range, got %+v", writes)
		}
		checkRangeIO(t, log, readStart, readEnd, off, off+length)
		checkSentinels(t, corrupt, shards[target], off, length)
	}
}

// TestRangeRecoverFileTail verifies recovery of a range ending exactly at the
// end of the shards.
func TestRangeRecoverFileTail(t *testing.T) {
	tests := []struct {
		name      string
		opts      []Option
		shardSize int64
		length    int64
	}{
		{"matrix", nil, 1000, 100},
		{"leopard-gf8", []Option{WithLeopardGF(true)}, 1024, 64},
		{"leopard-gf16", []Option{WithLeopardGF16(true)}, 1024, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := New(5, 3, tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			const target = 1
			off := tt.shardSize - tt.length

			shards := makeRangeShards(t, enc, int(tt.shardSize))
			valid, fill, log, corrupt := rangeRecoverSetup(shards, target, off, tt.length)

			rr := enc.(RangeRecoverer)
			if err := rr.RecoverRange(valid, fill, off, tt.length); err != nil {
				t.Fatal(err)
			}
			checkRangeIO(t, log, 0, tt.shardSize, off, tt.shardSize)
			writes := log.writes()
			if len(writes) != 1 || writes[0].off != off || writes[0].n != int(tt.length) {
				t.Errorf("writes must cover exactly [%d, %d), got %+v", off, tt.shardSize, writes)
			}
			checkSentinels(t, corrupt, shards[target], off, tt.length)
		})
	}
}

// TestRangeRecoverShortSource verifies that a short read on a source shard
// recovers as much as possible, writes nothing beyond the available data and
// reports io.ErrUnexpectedEOF.
func TestRangeRecoverShortSource(t *testing.T) {
	enc, err := New(5, 3)
	if err != nil {
		t.Fatal(err)
	}
	const shardSize = 1000
	const shortSize = 950 // shard 1 is truncated
	const off, length = int64(900), int64(100)
	const target = 2

	shards := makeRangeShards(t, enc, shardSize)
	valid, fill, log, corrupt := rangeRecoverSetup(shards, target, off, length)
	// Truncate one of the source shards.
	valid[1] = &recordingReaderAt{shard: 1, data: shards[1][:shortSize], log: log}

	rr := enc.(RangeRecoverer)
	err = rr.RecoverRange(valid, fill, off, length)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}

	// Only the recoverable part [900, 950) may have been written.
	checkRangeIO(t, log, off, off+length, off, shortSize)
	writes := log.writes()
	if len(writes) != 1 || writes[0].off != off || writes[0].n != shortSize-int(off) {
		t.Fatalf("expected one partial write of %d bytes at %d, got %+v", shortSize-int(off), off, writes)
	}
	// The recoverable prefix must match the original...
	if !bytes.Equal(corrupt[off:shortSize], shards[target][off:shortSize]) {
		t.Errorf("range [%d, %d) was not recovered correctly", off, shortSize)
	}
	// ...and everything else, including the unrecoverable tail, untouched.
	if !bytes.Equal(corrupt[:off], shards[target][:off]) {
		t.Errorf("sentinel bytes before offset %d were modified", off)
	}
	for i := shortSize; i < shardSize; i++ {
		if corrupt[i] != shards[target][i]^0xa5 {
			t.Fatalf("unrecoverable byte %d was modified", i)
		}
	}
}

// TestRangeRecoverParityShard verifies recovery of a range of a parity shard.
func TestRangeRecoverParityShard(t *testing.T) {
	enc, err := New(5, 3)
	if err != nil {
		t.Fatal(err)
	}
	const shardSize = 1000
	const off, length = int64(333), int64(77)
	target := 6 // first parity shard

	shards := makeRangeShards(t, enc, shardSize)
	valid, fill, log, corrupt := rangeRecoverSetup(shards, target, off, length)

	rr := enc.(RangeRecoverer)
	if err := rr.RecoverRange(valid, fill, off, length); err != nil {
		t.Fatal(err)
	}
	checkRangeIO(t, log, off, off+length, off, off+length)
	checkSentinels(t, corrupt, shards[target], off, length)
}

// TestRangeRecoverErrors verifies input validation.
func TestRangeRecoverErrors(t *testing.T) {
	enc, err := New(5, 3)
	if err != nil {
		t.Fatal(err)
	}
	rr := enc.(RangeRecoverer)
	valid := make([]io.ReaderAt, 8)
	fill := make([]io.WriterAt, 8)

	if err := rr.RecoverRange(valid[:7], fill, 0, 10); !errors.Is(err, ErrTooFewShards) {
		t.Errorf("expected ErrTooFewShards, got %v", err)
	}
	if err := rr.RecoverRange(valid, fill, -1, 10); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
	both := &recordingReaderAt{}
	valid[0] = both
	fill[0] = &recordingWriterAt{data: make([]byte, 100)}
	if err := rr.RecoverRange(valid, fill, 0, 10); !errors.Is(err, ErrReconstructMismatch) {
		t.Errorf("expected ErrReconstructMismatch, got %v", err)
	}
}
