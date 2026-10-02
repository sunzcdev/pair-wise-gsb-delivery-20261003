package reedsolomon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

// ioRange is a single recorded ReadAt/WriteAt interval.
type ioRange struct {
	off  int64
	data []byte
}

func (r ioRange) end() int64 { return r.off + int64(len(r.data)) }

// recReaderAt is a ReaderAt double that records every read interval. When
// chunk > 0 each ReadAt call hands back at most chunk bytes, exercising the
// retry path for short reads. Reads past the data end return io.EOF.
type recReaderAt struct {
	data  []byte
	chunk int
	calls []ioRange
}

func (r *recReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.calls = append(r.calls, ioRange{off: off, data: append([]byte(nil), p...)})

	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	avail := len(r.data) - int(off)
	n := len(p)
	if n > avail {
		n = avail
	}
	if r.chunk > 0 && n > r.chunk {
		n = r.chunk
	}
	copy(p[:n], r.data[int(off):int(off)+n])
	if n < len(p) && int(off)+n >= len(r.data) {
		return n, io.EOF
	}
	return n, nil
}

// recWriterAt is a WriterAt double recording every write interval. Its buffer
// is pre-filled with a sentinel byte so any out-of-range write is detectable.
type recWriterAt struct {
	data  []byte
	calls []ioRange
}

func newRecWriterAt(size int, sentinel byte) *recWriterAt {
	w := &recWriterAt{data: make([]byte, size)}
	for i := range w.data {
		w.data[i] = sentinel
	}
	return w
}

func (w *recWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(w.data)) {
		return 0, fmt.Errorf("write offset %d out of range [0,%d)", off, len(w.data))
	}
	n := copy(w.data[off:], p)
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	w.calls = append(w.calls, ioRange{off: off, data: append([]byte(nil), p...)})
	return n, nil
}

// checkSentinels fails if any byte outside [start, end) differs from sentinel.
func checkSentinels(t *testing.T, w *recWriterAt, start, end int64, sentinel byte) {
	t.Helper()
	for i := int64(0); i < start; i++ {
		if w.data[i] != sentinel {
			t.Fatalf("left sentinel overwritten at %d (got %#x)", i, w.data[i])
		}
	}
	for i := end; i < int64(len(w.data)); i++ {
		if w.data[i] != sentinel {
			t.Fatalf("right sentinel overwritten at %d (got %#x)", i, w.data[i])
		}
	}
}

func setupRangeShards(t *testing.T, dataShards, parityShards, shardSize int) (Encoder, [][]byte) {
	t.Helper()
	enc, err := New(dataShards, parityShards)
	if err != nil {
		t.Fatal(err)
	}
	shards := enc.(Extensions).AllocAligned(shardSize)
	for i := 0; i < dataShards; i++ {
		for j := range shards[i] {
			shards[i][j] = byte(rand.Intn(256))
		}
	}
	if err := enc.Encode(shards); err != nil {
		t.Fatal(err)
	}
	return enc, shards
}

// TestRecoverRangeBasic recovers a data and a parity shard range through the
// public RecoverRange entry point (alignment 1) and checks content, sentinels
// and write extent.
func TestRecoverRangeBasic(t *testing.T) {
	const dataShards, parityShards, size = 4, 2, 4096
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)

	orig := make([][]byte, len(shards))
	for i := range shards {
		orig[i] = append([]byte(nil), shards[i]...)
	}

	rr, ok := enc.(RangeRecovery)
	if !ok {
		t.Fatal("classic encoder must implement RangeRecovery")
	}

	const off, length, sentinel = int64(100), int64(37), byte(0xAB)
	readers := make([]io.ReaderAt, len(shards))
	writers := make([]io.WriterAt, len(shards))
	dsts := map[int]*recWriterAt{}

	for i := range shards {
		if i == 1 || i == dataShards {
			w := newRecWriterAt(size, sentinel)
			dsts[i] = w
			writers[i] = w
		} else {
			readers[i] = bytes.NewReader(shards[i])
		}
	}

	if err := rr.RecoverRange(readers, writers, off, length); err != nil {
		t.Fatal(err)
	}

	for idx, w := range dsts {
		if got, want := w.data[off:off+length], orig[idx][off:off+length]; !bytes.Equal(got, want) {
			t.Fatalf("shard %d recovered range mismatch", idx)
		}
		checkSentinels(t, w, off, off+length, sentinel)
		if len(w.calls) != 1 || w.calls[0].off != off || int64(len(w.calls[0].data)) != length {
			t.Fatalf("shard %d unexpected write calls: %+v", idx, w.calls)
		}
	}
}

// TestRecoverRangeMatrices repeats the aligned data-shard recovery across the
// available coding matrices, including an unaligned request and a parity
// source.
func TestRecoverRangeMatrices(t *testing.T) {
	for _, opt := range [][]Option{
		{WithCauchyMatrix()},
		{WithPAR1Matrix()},
		{WithJerasureMatrix()},
		{WithFastOneParityMatrix()},
	} {
		t.Run(fmt.Sprintf("%T", opt), func(t *testing.T) {
			const dataShards, parityShards, size = 4, 2, 700
			enc, err := New(dataShards, parityShards, opt...)
			if err != nil {
				t.Fatal(err)
			}
			shards := enc.(Extensions).AllocAligned(size)
			for i := 0; i < dataShards; i++ {
				for j := range shards[i] {
					shards[i][j] = byte(rand.Intn(256))
				}
			}
			if err := enc.Encode(shards); err != nil {
				t.Fatal(err)
			}
			rs := enc.(*reedSolomon)

			target := 2
			orig := append([]byte(nil), shards[target]...)
			const off, length = int64(17), int64(123)
			readers := make([]io.ReaderAt, len(shards))
			writers := make([]io.WriterAt, len(shards))
			for i := range shards {
				if i == target {
					writers[i] = newRecWriterAt(size, 0x44)
				} else {
					readers[i] = &recReaderAt{data: shards[i]}
				}
			}
			if err := rs.recoverRange(readers, writers, off, length, 64); err != nil {
				t.Fatal(err)
			}
			w := writers[target].(*recWriterAt)
			if got, want := w.data[off:off+length], orig[off:off+length]; !bytes.Equal(got, want) {
				t.Fatal("matrix variant recovered mismatch")
			}
			checkSentinels(t, w, off, off+length, 0x44)
		})
	}
}

// TestRecoverRangeAligned forces a 64-byte alignment and verifies source
// reads extend only to the minimal aligned window covering the request, which
// crosses an alignment boundary, while writes remain strictly inside it.
func TestRecoverRangeAligned(t *testing.T) {
	const dataShards, parityShards, size = 4, 2, 8192
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)
	rs := enc.(*reedSolomon)

	target := 2 // corrupt a data shard; recover it from 3 data + 1 parity
	orig := append([]byte(nil), shards[target]...)

	const align, sentinel = int64(64), byte(0x5A)
	const off, length = int64(65), int64(100) // request: [65,165)
	const wantStart, wantEnd = int64(64), int64(192)

	readers := make([]io.ReaderAt, len(shards))
	writers := make([]io.WriterAt, len(shards))
	srcs := map[int]*recReaderAt{}
	for i := range shards {
		if i == target || i == dataShards+1 {
			// target is reconstructed; the second parity shard is unused.
			if i == target {
				writers[i] = newRecWriterAt(size, sentinel)
			}
		} else {
			r := &recReaderAt{data: shards[i]}
			readers[i] = r
			srcs[i] = r
		}
	}

	if err := rs.recoverRange(readers, writers, off, length, align); err != nil {
		t.Fatal(err)
	}

	for idx, r := range srcs {
		if len(r.calls) != 1 {
			t.Fatalf("source shard %d: expected 1 read, got %d", idx, len(r.calls))
		}
		c := r.calls[0]
		if c.off != wantStart || c.end() != wantEnd {
			t.Fatalf("source shard %d read [%d,%d), want [%d,%d)", idx, c.off, c.end(), wantStart, wantEnd)
		}
	}

	w := writers[target].(*recWriterAt)
	if got, want := w.data[off:off+length], orig[off:off+length]; !bytes.Equal(got, want) {
		t.Fatal("recovered data mismatch")
	}
	if len(w.calls) != 1 || w.calls[0].off != off || w.calls[0].end() != off+length {
		t.Fatalf("unexpected write calls: %+v", w.calls)
	}
	checkSentinels(t, w, off, off+length, sentinel)
}

// TestRecoverRangeTail requests a range that runs past the physical shard
// end. Source reads reach into the aligned padding (EOF becomes zero
// padding), and the write is clipped to the shard end.
func TestRecoverRangeTail(t *testing.T) {
	const dataShards, parityShards = 4, 2
	const size, align, sentinel = 300, int64(64), byte(0x77)
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)
	rs := enc.(*reedSolomon)

	target := 1 // corrupt data shard; sources are 3 data + 1 parity
	orig := append([]byte(nil), shards[target]...)

	const off, length = int64(260), int64(100) // request [260,360), shard ends at 300
	readers := make([]io.ReaderAt, len(shards))
	writers := make([]io.WriterAt, len(shards))
	srcs := map[int]*recReaderAt{}
	for i := range shards {
		if i == target || i == dataShards+1 {
			if i == target {
				writers[i] = newRecWriterAt(size, sentinel)
			}
		} else {
			r := &recReaderAt{data: shards[i]}
			readers[i] = r
			srcs[i] = r
		}
	}

	if err := rs.recoverRange(readers, writers, off, length, align); err != nil {
		t.Fatal(err)
	}

	// The aligned read window is [256,384); sources only hold up to 300.
	for idx, r := range srcs {
		var lo, hi int64 = 1 << 62, -1
		for _, c := range r.calls {
			if c.off < lo {
				lo = c.off
			}
			if c.end() > hi {
				hi = c.end()
			}
		}
		if lo != 256 || hi != 384 {
			t.Fatalf("source shard %d read extent [%d,%d), want [256,384)", idx, lo, hi)
		}
	}

	w := writers[target].(*recWriterAt)
	if len(w.calls) != 1 || w.calls[0].off != off || w.calls[0].end() != int64(size) {
		t.Fatalf("write not clipped to shard end: %+v", w.calls)
	}
	if got, want := w.data[off:size], orig[off:size]; !bytes.Equal(got, want) {
		t.Fatal("tail recovered data mismatch")
	}
	checkSentinels(t, w, off, int64(size), sentinel)
}

// TestRecoverRangeShortRead uses sources that return only a few bytes per
// ReadAt call, verifying the retry loop reconstructs the whole range and
// that every read stays inside the aligned window.
func TestRecoverRangeShortRead(t *testing.T) {
	const dataShards, parityShards, size = 4, 2, 512
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)
	rs := enc.(*reedSolomon)

	target := 0 // corrupt data shard; sources are 3 data + 1 parity
	orig := append([]byte(nil), shards[target]...)

	const align, sentinel = int64(64), byte(0x33)
	const off, length = int64(70), int64(40) // window [64,128), one boundary

	readers := make([]io.ReaderAt, len(shards))
	writers := make([]io.WriterAt, len(shards))
	srcs := map[int]*recReaderAt{}
	for i := range shards {
		if i == target || i == dataShards+1 {
			if i == target {
				writers[i] = newRecWriterAt(size, sentinel)
			}
		} else {
			r := &recReaderAt{data: shards[i], chunk: 7}
			readers[i] = r
			srcs[i] = r
		}
	}

	if err := rs.recoverRange(readers, writers, off, length, align); err != nil {
		t.Fatal(err)
	}

	for idx, r := range srcs {
		var lo, hi int64 = 1 << 62, -1
		for _, c := range r.calls {
			if c.off < lo {
				lo = c.off
			}
			if c.end() > hi {
				hi = c.end()
			}
			if c.off < 64 || c.end() > 128 {
				t.Fatalf("source shard %d read [%d,%d) outside aligned window", idx, c.off, c.end())
			}
		}
		if lo != 64 || hi != 128 {
			t.Fatalf("source shard %d extent [%d,%d), want [64,128)", idx, lo, hi)
		}
	}

	w := writers[target].(*recWriterAt)
	if got, want := w.data[off:off+length], orig[off:off+length]; !bytes.Equal(got, want) {
		t.Fatal("short-read recovered data mismatch")
	}
	checkSentinels(t, w, off, off+length, sentinel)
}

// TestRecoverRangeReadError propagates a source error wrapped in
// StreamReadError.
func TestRecoverRangeReadError(t *testing.T) {
	const dataShards, parityShards, size = 4, 2, 256
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)

	myErr := errors.New("disk failure")
	readers := make([]io.ReaderAt, len(shards))
	writers := make([]io.WriterAt, len(shards))
	for i := range shards {
		if i == 0 {
			writers[i] = newRecWriterAt(size, 0)
		} else {
			readers[i] = errReaderAt{err: myErr}
		}
	}

	err := enc.(RangeRecovery).RecoverRange(readers, writers, 0, 16)
	if err == nil {
		t.Fatal("expected read error")
	}
	sre, ok := err.(StreamReadError)
	if !ok || !errors.Is(sre.Err, myErr) {
		t.Fatalf("expected StreamReadError wrapping %v, got %v", myErr, err)
	}
}

type errReaderAt struct{ err error }

func (e errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, e.err }

// TestRecoverRangeArgs covers the argument validation paths.
func TestRecoverRangeArgs(t *testing.T) {
	const dataShards, parityShards, size = 4, 2, 256
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)
	rr := enc.(RangeRecovery)

	newSets := func() ([]io.ReaderAt, []io.WriterAt) {
		readers := make([]io.ReaderAt, len(shards))
		writers := make([]io.WriterAt, len(shards))
		for i := range shards {
			if i == 0 {
				writers[i] = newRecWriterAt(size, 0)
			} else {
				readers[i] = &recReaderAt{data: shards[i]}
			}
		}
		return readers, writers
	}

	readers, writers := newSets()
	if err := rr.RecoverRange(readers, writers, -1, 10); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("offset: want ErrRangeInvalid, got %v", err)
	}
	readers, writers = newSets()
	if err := rr.RecoverRange(readers, writers, 0, 0); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("length: want ErrRangeInvalid, got %v", err)
	}

	if err := rr.RecoverRange(readers[:2], writers, 0, 10); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("short valid: want ErrTooFewShards, got %v", err)
	}

	readers, writers = newSets()
	readers[0] = bytes.NewReader(shards[0])
	if err := rr.RecoverRange(readers, writers, 0, 10); !errors.Is(err, ErrReconstructMismatch) {
		t.Fatalf("mismatch: want ErrReconstructMismatch, got %v", err)
	}

	readers, writers = newSets()
	readers[1] = nil
	readers[dataShards] = nil // leave fewer than DataShards sources
	if err := rr.RecoverRange(readers, writers, 0, 10); !errors.Is(err, ErrTooFewShards) {
		t.Fatalf("few shards: want ErrTooFewShards, got %v", err)
	}

	readers, writers = newSets()
	if err := rr.RecoverRange(readers, writers, int64(size), 10); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("past end: want ErrRangeInvalid, got %v", err)
	}
}

// TestRecoverRangeLeopardUnsupported verifies leopard encoders do not
// implement RangeRecovery.
// TestRecoverRangeMultiTarget recovers a data and a parity shard in one call
// from the remaining dataShards sources, crossing an alignment boundary, and
// checks that each destination receives only its own requested bytes.
func TestRecoverRangeMultiTarget(t *testing.T) {
	const dataShards, parityShards, size = 4, 2, 1024
	enc, shards := setupRangeShards(t, dataShards, parityShards, size)
	rs := enc.(*reedSolomon)

	const align = int64(64)
	const off, length = int64(33), int64(200) // window [0,256), crosses 64/128/192

	targets := map[int]byte{0: 0x11, dataShards: 0x22}
	readers := make([]io.ReaderAt, len(shards))
	writers := make([]io.WriterAt, len(shards))
	dsts := map[int]*recWriterAt{}
	for i := range shards {
		if sentinel, ok := targets[i]; ok {
			w := newRecWriterAt(size, sentinel)
			writers[i] = w
			dsts[i] = w
		}
	}
	// Four sources: the other three data shards plus the spare parity shard.
	for i := range shards {
		if writers[i] == nil {
			readers[i] = &recReaderAt{data: shards[i]}
		}
	}

	if err := rs.recoverRange(readers, writers, off, length, align); err != nil {
		t.Fatal(err)
	}

	for idx, w := range dsts {
		if got, want := w.data[off:off+length], shards[idx][off:off+length]; !bytes.Equal(got, want) {
			t.Fatalf("shard %d recovered mismatch", idx)
		}
		checkSentinels(t, w, off, off+length, w.data[0])
		if len(w.calls) != 1 || w.calls[0].off != off || w.calls[0].end() != off+length {
			t.Fatalf("shard %d bad write extent: %+v", idx, w.calls)
		}
	}
}

func TestRecoverRangeLeopardUnsupported(t *testing.T) {
	for _, opt := range []Option{WithLeopardGF(true), WithLeopardGF16(true)} {
		enc, err := New(4, 2, opt)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := enc.(RangeRecovery); ok {
			t.Fatalf("leopard encoder (%T) must not implement RangeRecovery", enc)
		}
	}
}
