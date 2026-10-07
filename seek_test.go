// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"sort"
	"sync"
	"testing"
)

// compress creates a single xz stream from src using the given block
// size, producing len(src)/blockSize (or one) blocks.
func compress(t *testing.T, src []byte, blockSize int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := WriterConfig{BlockSize: blockSize, CheckSum: CRC64}
	w, err := cfg.NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter error %s", err)
	}
	if _, err = w.Write(src); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	return buf.Bytes()
}

// countReaderAt wraps an io.ReaderAt and records which file offsets are
// ever read together with the total number of bytes requested.
type countReaderAt struct {
	r      io.ReaderAt
	total  int64
	calls  int
	ranges []rng
}

type rng struct{ start, end int64 }

func (c *countReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	c.total += int64(len(p))
	c.ranges = append(c.ranges, rng{off, off + int64(len(p))})
	return c.r.ReadAt(p, off)
}

// contained reports whether every recorded range lies inside [lo, hi).
func (c *countReaderAt) contained(lo, hi int64) bool {
	for _, r := range c.ranges {
		if r.start < lo || r.end > hi {
			return false
		}
	}
	return true
}

// touches reports whether any recorded range intersects [lo, hi).
func (c *countReaderAt) touches(lo, hi int64) bool {
	for _, r := range c.ranges {
		if r.start < hi && lo < r.end {
			return true
		}
	}
	return false
}

func buildMultiStream(t *testing.T) (plain, xzData []byte, streams int) {
	t.Helper()
	rnd := rand.New(rand.NewSource(42))
	plain = make([]byte, 60000)
	if _, err := rnd.Read(plain); err != nil {
		t.Fatalf("rand error %s", err)
	}

	// Three streams, each with several small blocks. Insert legal
	// padding between some of them.
	cuts := []int{0, 17000, 39000, len(plain)}
	var m bytes.Buffer
	for i := 0; i < len(cuts)-1; i++ {
		chunk := compress(t, plain[cuts[i]:cuts[i+1]], 2000)
		m.Write(chunk)
		if i == 0 {
			m.Write([]byte{0, 0, 0, 0})
		}
		if i == 1 {
			m.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0})
		}
	}
	return plain, m.Bytes(), 3
}

func TestSeekReaderMultiStream(t *testing.T) {
	plain, data, nstreams := buildMultiStream(t)
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	if got := sr.Size(); got != int64(len(plain)) {
		t.Fatalf("Size %d; want %d", got, len(plain))
	}
	// Every stream contributes blocks; make sure several blocks were
	// parsed.
	if len(sr.block) <= nstreams {
		t.Fatalf("expected more than %d blocks, got %d",
			nstreams, len(sr.block))
	}

	// Read the whole file through ReadAt and compare to sequential
	// decompression.
	got := make([]byte, len(plain))
	if _, err = sr.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt full error %s", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("full ReadAt mismatch")
	}
}

func TestSeekReaderRandomIntervals(t *testing.T) {
	plain, data, _ := buildMultiStream(t)
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}

	offsets := []int64{0, 1, 1234, 17000, 17003, 25000, 39000,
		int64(len(plain)) - 1}
	lengths := []int{1, 7, 500, 4096, 9000}
	for _, off := range offsets {
		for _, ln := range lengths {
			if off+int64(ln) > int64(len(plain)) {
				continue
			}
			p := make([]byte, ln)
			if _, err := sr.ReadAt(p, off); err != nil {
				t.Fatalf("ReadAt(%d,%d) error %s", off, ln, err)
			}
			if !bytes.Equal(p, plain[off:off+int64(ln)]) {
				t.Fatalf("ReadAt(%d,%d) mismatch", off, ln)
			}
		}
	}

	// Reading past the end must fail with ErrUnexpectedEOF for a
	// partial request.
	tail := make([]byte, 16)
	_, err = sr.ReadAt(tail, int64(len(plain))-4)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("tail read error %v; want ErrUnexpectedEOF", err)
	}
	// Reading exactly at EOF returns io.EOF.
	_, err = sr.ReadAt(tail[:1], int64(len(plain)))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("EOF read error %v; want io.EOF", err)
	}
}

func TestSeekReaderLocality(t *testing.T) {
	plain, data, _ := buildMultiStream(t)

	// Pick a small interval located fully inside a middle block.
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	off := int64(21000)
	bidx := sort.Search(len(sr.block), func(i int) bool {
		b := sr.block[i]
		return off < b.unpaddedOff+b.uncompressedSize
	})
	if bidx == 0 || bidx >= len(sr.block)-1 {
		t.Fatalf("target block index %d not in middle", bidx)
	}
	b := sr.block[bidx]
	padded, err := pad4(b.unpaddedSize)
	if err != nil {
		t.Fatal(err)
	}
	blockLo := b.off
	blockHi := b.off + padded

	cr := &countReaderAt{r: bytes.NewReader(data)}
	csr, err := NewReaderAt(cr, int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	// Construction parses indexes and block headers across the whole
	// file; reset the counters so the measurement covers only the small
	// ReadAt, which must not decompress the rest of the file.
	cr.total, cr.calls, cr.ranges = 0, 0, nil

	const readLen = 32
	p := make([]byte, readLen)
	if _, err = csr.ReadAt(p, off); err != nil {
		t.Fatalf("ReadAt error %s", err)
	}
	if !bytes.Equal(p, plain[off:off+readLen]) {
		t.Fatal("small read content mismatch")
	}

	// The tiny read must touch only the one target block and must not
	// read the whole compressed file.
	if !cr.contained(blockLo, blockHi) {
		t.Fatalf("read ranged outside target block [%d,%d): %v",
			blockLo, blockHi, cr.ranges)
	}
	if cr.total >= int64(len(data))/2 {
		t.Fatalf("small read consumed %d bytes of %d total",
			cr.total, len(data))
	}
	first := sr.block[0]
	last := sr.block[len(sr.block)-1]
	if cr.touches(0, first.off+int64(first.headerLen)) {
		t.Fatal("small read touched the first block")
	}
	if cr.touches(last.off, int64(len(data))) {
		t.Fatal("small read touched the last block")
	}
}

// TestSeekReaderCrossSequential compares random access against the
// output of the streaming reader for a range of intervals.
func TestSeekReaderCrossSequential(t *testing.T) {
	_, data, _ := buildMultiStream(t)

	seq, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader error %s", err)
	}
	sequential, err := io.ReadAll(seq)
	if err != nil {
		t.Fatalf("sequential read error %s", err)
	}

	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	if sr.Size() != int64(len(sequential)) {
		t.Fatalf("size %d; want %d", sr.Size(), len(sequential))
	}

	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		off := rnd.Int63n(int64(len(sequential)))
		ln := 1 + rnd.Intn(6000)
		if off+int64(ln) > int64(len(sequential)) {
			ln = int(int64(len(sequential)) - off)
		}
		p := make([]byte, ln)
		if _, err := sr.ReadAt(p, off); err != nil {
			t.Fatalf("iter %d ReadAt(%d,%d): %s", i, off, ln, err)
		}
		if !bytes.Equal(p, sequential[off:off+int64(ln)]) {
			t.Fatalf("iter %d mismatch off=%d len=%d", i, off, ln)
		}
	}
}

func TestSeekReaderContentChecksum(t *testing.T) {
	plain, data, _ := buildMultiStream(t)
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}

	// Corrupt a payload byte of a specific middle block and ensure that
	// reading that block fails while untouched blocks still work.
	target := 2
	b := sr.block[target]
	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	pos := b.compressedStart + b.compressedSize/2
	corrupt[pos] ^= 0xff

	csr, err := NewReaderAt(bytes.NewReader(corrupt), int64(len(corrupt)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}

	// A read fully inside the corrupted block must be detected.
	p := make([]byte, 16)
	inOff := b.unpaddedOff + b.uncompressedSize/2
	_, err = csr.ReadAt(p, inOff)
	if err == nil {
		t.Fatal("expected checksum/decode error for corrupted block")
	}

	// A different, untouched block must still decode correctly.
	good := sr.block[target+1]
	gp := make([]byte, 16)
	gOff := good.unpaddedOff + 4
	if _, err = csr.ReadAt(gp, gOff); err != nil {
		t.Fatalf("unexpected error reading untouched block: %s", err)
	}
	if !bytes.Equal(gp, plain[gOff:gOff+16]) {
		t.Fatal("untouched block content mismatch")
	}
}

func TestSeekReaderIndexCRC(t *testing.T) {
	_, data, _ := buildMultiStream(t)
	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	// Corrupt a byte inside the last stream's index (just before its
	// footer magic).
	corrupt[len(corrupt)-footerLen-1] ^= 0xff
	if _, err := NewReaderAt(bytes.NewReader(corrupt),
		int64(len(corrupt))); err == nil {
		t.Fatal("expected index CRC error")
	}
}

func TestSeekReaderDictCapLimit(t *testing.T) {
	plain := bytes.Repeat([]byte("abcdefghij"), 5000)
	data := compress(t, plain, 4096)
	// The blocks use the writer default dictionary (8 MiB encoded); a
	// tiny limit must be rejected.
	cfg := ReaderConfig{BlockDictCap: 1024}
	if _, err := cfg.NewReaderAt(bytes.NewReader(data),
		int64(len(data))); err == nil {
		t.Fatal("expected dictionary capacity limit error")
	}
}

func TestSeekReaderIndexMemLimit(t *testing.T) {
	_, data, _ := buildMultiStream(t)
	cfg := ReaderConfig{IndexMemCap: 1}
	if _, err := cfg.NewReaderAt(bytes.NewReader(data),
		int64(len(data))); err == nil {
		t.Fatal("expected index memory limit error")
	}
}

func TestSeekReaderCheckNone(t *testing.T) {
	data, err := os.ReadFile("fox-check-none.xz")
	if err != nil {
		t.Fatalf("ReadFile error %s", err)
	}
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	got := make([]byte, sr.Size())
	if _, err = sr.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt error %s", err)
	}
}

func TestSeekReaderPadding(t *testing.T) {
	plain, one, _ := buildMultiStream(t)

	// Leading and trailing legal padding in addition to the internal
	// padding already present.
	padded := append([]byte{0, 0, 0, 0}, one...)
	padded = append(padded, 0, 0, 0, 0, 0, 0, 0, 0)
	sr, err := NewReaderAt(bytes.NewReader(padded), int64(len(padded)))
	if err != nil {
		t.Fatalf("NewReaderAt padded error %s", err)
	}
	got := make([]byte, len(plain))
	if _, err = sr.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt padded error %s", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("padded content mismatch")
	}

	// A non-zero byte where padding is expected must be rejected.
	bad := make([]byte, len(one)+4)
	copy(bad, one)
	bad[len(one)] = 1
	if _, err = NewReaderAt(bytes.NewReader(bad), int64(len(bad))); err == nil {
		t.Fatal("expected error for non-zero padding")
	}
}

func TestSeekReaderBlockHeaderCRC(t *testing.T) {
	plain, data, _ := buildMultiStream(t)
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	// Flip a byte inside a middle block header (after the size byte);
	// construction validates every block header and must fail.
	b := sr.block[2]
	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	corrupt[b.off+int64(b.headerLen)-5] ^= 0xff
	if _, err = NewReaderAt(bytes.NewReader(corrupt),
		int64(len(corrupt))); err == nil {
		t.Fatal("expected block header checksum error at open")
	}
	_ = plain
}

func TestSeekReaderSingleBlockBoundary(t *testing.T) {
	// A read that crosses a block boundary must return bytes in order.
	plain := bytes.Repeat([]byte("0123456789ABCDEF"), 1000)
	data := compress(t, plain, 1000)
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt error %s", err)
	}
	if len(sr.block) < 2 {
		t.Fatalf("expected multiple blocks, got %d", len(sr.block))
	}
	b := sr.block[0]
	off := b.unpaddedOff + b.uncompressedSize - 10
	ln := 40
	p := make([]byte, ln)
	if _, err = sr.ReadAt(p, off); err != nil {
		t.Fatalf("ReadAt across boundary error %s", err)
	}
	if !bytes.Equal(p, plain[off:off+int64(ln)]) {
		t.Fatal("cross-boundary content mismatch")
	}
}

func TestSeekReaderConcurrent(t *testing.T) {
	plain, data, _ := buildMultiStream(t)
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt: %s", err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			for i := 0; i < 100; i++ {
				off := rnd.Int63n(int64(len(plain)))
				ln := 1 + rnd.Intn(2048)
				if off+int64(ln) > int64(len(plain)) {
					ln = int(int64(len(plain)) - off)
				}
				p := make([]byte, ln)
				if _, err := sr.ReadAt(p, off); err != nil {
					t.Errorf("ReadAt(%d,%d): %s", off, ln, err)
					return
				}
				if !bytes.Equal(p, plain[off:off+int64(ln)]) {
					t.Errorf("concurrent mismatch off=%d", off)
					return
				}
			}
		}(int64(g) + 1)
	}
	wg.Wait()
}

func TestSeekReaderCheckMethods(t *testing.T) {
	plain := bytes.Repeat([]byte("checksum-method-"), 800)
	for _, check := range []byte{None, CRC32, CRC64, SHA256} {
		var buf bytes.Buffer
		cfg := WriterConfig{BlockSize: 700, CheckSum: check}
		w, err := cfg.NewWriter(&buf)
		if err != nil {
			t.Fatalf("check %d NewWriter: %s", check, err)
		}
		if _, err = w.Write(plain); err != nil {
			t.Fatalf("check %d Write: %s", check, err)
		}
		if err = w.Close(); err != nil {
			t.Fatalf("check %d Close: %s", check, err)
		}
		data := buf.Bytes()
		sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("check %d NewReaderAt: %s", check, err)
		}
		got := make([]byte, 1234)
		if _, err = sr.ReadAt(got, 137); err != nil {
			t.Fatalf("check %d ReadAt: %s", check, err)
		}
		if !bytes.Equal(got, plain[137:137+1234]) {
			t.Fatalf("check %d content mismatch", check)
		}
	}
}

func TestSeekReaderEmptyStream(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt empty: %s", err)
	}
	if sr.Size() != 0 {
		t.Fatalf("empty Size %d; want 0", sr.Size())
	}
	p := make([]byte, 1)
	if _, err = sr.ReadAt(p, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("empty ReadAt err %v; want io.EOF", err)
	}
}

// TestSeekReaderSizeContradiction hand-assembles a stream whose block
// header declares explicit sizes that disagree with the index record;
// opening must reject the contradiction.
func TestSeekReaderSizeContradiction(t *testing.T) {
	plain := []byte("hello contradiction")

	var raw bytes.Buffer
	w, err := WriterConfig{BlockSize: 1 << 62, CheckSum: CRC32}.
		NewWriter(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	data := raw.Bytes()

	good, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("good NewReaderAt: %s", err)
	}
	b := good.block[0]

	// Rebuild the block header carrying an explicit (wrong) compressed
	// size. The on-disk length of the new header must stay identical so
	// all following offsets remain valid.
	wrong := &blockHeader{
		compressedSize:   b.compressedSize + 1,
		uncompressedSize: b.uncompressedSize,
		filters:          []filter{&lzmaFilter{b.dictCap}},
	}
	newHeader, err := wrong.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(newHeader) != b.headerLen {
		t.Skip("assembled header has different length")
	}

	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	copy(corrupt[b.off:], newHeader)
	if _, err = NewReaderAt(bytes.NewReader(corrupt),
		int64(len(corrupt))); err == nil {
		t.Fatal("expected header/index size contradiction error")
	}
}
