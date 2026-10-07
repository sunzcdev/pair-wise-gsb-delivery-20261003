// Copyright 2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"testing"
)

// countingRA is an io.ReaderAt counting all bytes handed out.
type countingRA struct {
	ra    io.ReaderAt
	calls int
	n     int64
}

func (c *countingRA) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	n, err := c.ra.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

// testData generates pseudo-random but compressible data by repeating
// and mutating a small pattern.
func testData(n int) []byte {
	rng := rand.New(rand.NewSource(42))
	data := make([]byte, n)
	pattern := []byte("The quick brown fox jumps over the lazy dog. ")
	for i := range data {
		if rng.Intn(20) == 0 {
			data[i] = byte(rng.Intn(26) + 'a')
		} else {
			data[i] = pattern[i%len(pattern)]
		}
	}
	return data
}

// compressXZ compresses data using small blocks so that random access
// exercises several blocks.
func compressXZ(t *testing.T, data []byte, blockSize int64,
	check byte, noCheck bool) []byte {

	t.Helper()
	var buf bytes.Buffer
	cfg := WriterConfig{
		DictCap:    4096,
		BlockSize:  blockSize,
		CheckSum:   check,
		NoCheckSum: noCheck,
	}
	w, err := cfg.NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter error %s", err)
	}
	if _, err = w.Write(data); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	return buf.Bytes()
}

func TestRandomReaderIntervals(t *testing.T) {
	const N = 20000
	data := testData(N)
	xzData := compressXZ(t, data, 500, CRC64, false)

	cra := &countingRA{ra: bytes.NewReader(xzData)}
	rr, err := NewRandomReader(cra, int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader error %s", err)
	}
	if rr.Size() != int64(N) {
		t.Fatalf("Size got %d; want %d", rr.Size(), N)
	}
	if len(rr.blocks) < 3 {
		t.Fatalf("expected several blocks, got %d", len(rr.blocks))
	}

	// Verify that the logical uncompressed offsets cover the data
	// without gaps or overlaps.
	var off int64
	for i, b := range rr.blocks {
		if b.unpOff != off {
			t.Fatalf("block %d unpOff %d; want %d", i, b.unpOff, off)
		}
		off += b.uncompressedSize
	}
	if off != int64(N) {
		t.Fatalf("block offsets cover %d bytes; want %d", off, N)
	}

	// Random intervals, including intervals that span multiple
	// blocks, compared against the sequential decompression result.
	rng := rand.New(rand.NewSource(7))

	for iter := 0; iter < 200; iter++ {
		start := rng.Int63n(int64(N))
		length := rng.Int63n(3000) + 1
		stop := start + length
		if stop > int64(N) {
			stop = int64(N)
		}
		p := make([]byte, stop-start)
		n, err := rr.ReadAt(p, start)
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(%d,%d) error %s", start, length, err)
		}
		if int64(n) != stop-start {
			t.Fatalf("ReadAt(%d,%d) n %d; want %d",
				start, length, n, stop-start)
		}
		if !bytes.Equal(p, data[start:stop]) {
			t.Fatalf("ReadAt(%d,%d) mismatch", start, length)
		}
	}
}

func TestRandomReaderReadAtEOF(t *testing.T) {
	data := testData(5000)
	xzData := compressXZ(t, data, 1000, CRC32, false)
	rr, err := NewRandomReader(bytes.NewReader(xzData), int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader error %s", err)
	}

	// Read beyond end with partial fill.
	p := make([]byte, 100)
	n, err := rr.ReadAt(p, rr.Size()-10)
	if err != io.EOF {
		t.Fatalf("err %v; want io.EOF", err)
	}
	if n != 10 {
		t.Fatalf("n %d; want 10", n)
	}
	if !bytes.Equal(p[:10], data[len(data)-10:]) {
		t.Fatalf("tail mismatch")
	}

	// Read completely beyond end.
	if _, err = rr.ReadAt(make([]byte, 1), rr.Size()); err != io.EOF {
		t.Fatalf("err %v; want io.EOF", err)
	}

	// Empty read at the end is legal.
	if _, err = rr.ReadAt(make([]byte, 0), rr.Size()); err != nil {
		t.Fatalf("empty ReadAt error %s", err)
	}
}

func TestRandomReaderDoesNotReadWholeFile(t *testing.T) {
	const N = 100000
	data := testData(N)
	xzData := compressXZ(t, data, 1000, CRC64, false)

	cra := &countingRA{ra: bytes.NewReader(xzData)}
	rr, err := NewRandomReader(cra, int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader error %s", err)
	}

	// Opening reads footers, indexes and block headers only.
	openBytes := cra.n
	if openBytes >= int64(len(xzData)) {
		t.Fatalf("open read %d bytes; expected less than file size %d",
			openBytes, len(xzData))
	}

	// Read a small interval in the middle of the file.
	const start, length = 50000, 17
	cra.n, cra.calls = 0, 0
	p := make([]byte, length)
	if _, err = rr.ReadAt(p, start); err != nil {
		t.Fatalf("ReadAt error %s", err)
	}
	if !bytes.Equal(p, data[start:start+length]) {
		t.Fatalf("data mismatch")
	}

	// A small interval must touch only a small fraction of the file.
	if cra.n > int64(len(xzData))/4 {
		t.Fatalf("small read fetched %d bytes of %d; whole file decompressed?",
			cra.n, len(xzData))
	}
}

func TestRandomReaderMultipleStreams(t *testing.T) {
	const N = 6000
	data := testData(N)
	part := N / 3

	s1 := compressXZ(t, data[:part], 300, CRC64, false)
	s2 := compressXZ(t, data[part:2*part], 250, CRC64, false)
	s3 := compressXZ(t, data[2*part:], 200, CRC64, false)

	var xzData []byte
	xzData = append(xzData, s1...)
	xzData = append(xzData, 0, 0, 0, 0)
	xzData = append(xzData, s2...)
	xzData = append(xzData, 0, 0, 0, 0)
	xzData = append(xzData, 0, 0, 0, 0)
	xzData = append(xzData, s3...)
	xzData = append(xzData, 0, 0, 0, 0)

	// Cross-check against the sequential reader.
	seq, err := NewReader(bytes.NewReader(xzData))
	if err != nil {
		t.Fatalf("NewReader error %s", err)
	}
	want, err := io.ReadAll(seq)
	if err != nil {
		t.Fatalf("sequential ReadAll error %s", err)
	}
	if !bytes.Equal(want, data) {
		t.Fatalf("sequential data mismatch")
	}

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader error %s", err)
	}
	if rr.Size() != int64(N) {
		t.Fatalf("size %d; want %d", rr.Size(), N)
	}

	// Full comparison using random intervals, including stream and
	// block boundaries.
	rng := rand.New(rand.NewSource(99))
	for iter := 0; iter < 100; iter++ {
		start := rng.Int63n(int64(N))
		length := rng.Int63n(2000) + 1
		stop := start + length
		if stop > int64(N) {
			stop = int64(N)
		}
		p := make([]byte, stop-start)
		n, err := rr.ReadAt(p, start)
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt error %s", err)
		}
		if int64(n) != stop-start {
			t.Fatalf("n %d; want %d", n, stop-start)
		}
		if !bytes.Equal(p, want[start:stop]) {
			t.Fatalf("interval [%d,%d) mismatch", start, stop)
		}
	}

	// Also compare the entire file in one call.
	p := make([]byte, N)
	if _, err = io.ReadFull(io.NewSectionReader(rr, 0, rr.Size()), p); err != nil {
		t.Fatalf("ReadFull error %s", err)
	}
	if !bytes.Equal(p, want) {
		t.Fatalf("full data mismatch")
	}
}

func TestRandomReaderCheckMethods(t *testing.T) {
	data := testData(3000)
	for _, tc := range []struct {
		name    string
		check   byte
		noCheck bool
	}{
		{"CRC64", CRC64, false},
		{"CRC32", CRC32, false},
		{"SHA256", SHA256, false},
		{"None", None, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			xzData := compressXZ(t, data, 700, tc.check, tc.noCheck)
			rr, err := NewRandomReader(bytes.NewReader(xzData),
				int64(len(xzData)))
			if err != nil {
				t.Fatalf("NewRandomReader error %s", err)
			}
			p := make([]byte, len(data))
			if _, err = io.ReadFull(io.NewSectionReader(rr, 0, rr.Size()), p); err != nil {
				t.Fatalf("ReadFull error %s", err)
			}
			if !bytes.Equal(p, data) {
				t.Fatalf("data mismatch for %s", tc.name)
			}
		})
	}
}

func TestRandomReaderRejectsCorruptIndex(t *testing.T) {
	data := testData(4000)
	xzData := compressXZ(t, data, 500, CRC64, false)

	// Parse the footer to find the index offset.
	var f footer
	if err := f.UnmarshalBinary(
		xzData[len(xzData)-footerLen:]); err != nil {
		t.Fatalf("footer unmarshal %s", err)
	}
	idxOff := int64(len(xzData)) - footerLen - f.indexSize

	// Flip a byte inside the index and repair nothing; the index CRC
	// must reject the file at open time.
	bad := append([]byte(nil), xzData...)
	bad[idxOff+5] ^= 0xff
	if _, err := NewRandomReader(bytes.NewReader(bad),
		int64(len(bad))); err == nil {
		t.Fatalf("corrupt index accepted")
	}

	// A corrupt footer CRC must be rejected as well.
	bad = append([]byte(nil), xzData...)
	bad[len(bad)-5] ^= 0xff
	if _, err := NewRandomReader(bytes.NewReader(bad),
		int64(len(bad))); err == nil {
		t.Fatalf("corrupt footer accepted")
	}
}

func TestRandomReaderRejectsCorruptBlockHeader(t *testing.T) {
	data := testData(4000)
	xzData := compressXZ(t, data, 500, CRC64, false)

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	b1 := rr.blocks[1]

	bad := append([]byte(nil), xzData...)
	// Corrupt a filter byte inside the second block header and
	// recompute its CRC so that only the semantic content is wrong.
	bad[b1.compressedOff+2] ^= 0xff
	putUint32LE(bad[b1.compressedOff+int64(b1.headerLen)-4:],
		crc32.ChecksumIEEE(bad[b1.compressedOff:b1.compressedOff+int64(b1.headerLen)-4]))
	if _, err := NewRandomReader(bytes.NewReader(bad),
		int64(len(bad))); err == nil {
		t.Fatalf("corrupt block header accepted")
	}
}

func TestRandomReaderContentCheck(t *testing.T) {
	const N = 5000
	data := testData(N)
	xzData := compressXZ(t, data, 1000, CRC64, false)

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	if len(rr.blocks) < 4 {
		t.Fatalf("need at least 4 blocks, got %d", len(rr.blocks))
	}
	target := rr.blocks[2]

	bad := append([]byte(nil), xzData...)
	// Corrupt a compressed byte of the third block. The corruption is
	// inside the block and doesn't touch headers or indexes.
	bad[target.compressedOff+int64(target.headerLen)+1] ^= 0xff

	rr2, err := NewRandomReader(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatalf("NewRandomReader on corrupt data %s", err)
	}

	// Reading a healthy block must still succeed: unread blocks are
	// not content-verified.
	healthy := rr2.blocks[0]
	p := make([]byte, healthy.uncompressedSize)
	if _, err = rr2.ReadAt(p, healthy.unpOff); err != nil {
		t.Fatalf("healthy block read failed: %s", err)
	}
	if !bytes.Equal(p, data[:healthy.uncompressedSize]) {
		t.Fatalf("healthy block data mismatch")
	}

	// Reading the corrupted block must fail with a checksum or
	// decoding error.
	p = make([]byte, target.uncompressedSize)
	if _, err = rr2.ReadAt(p, target.unpOff); err == nil {
		t.Fatalf("corrupt block content accepted")
	}
}

func TestRandomReaderContentCheckNone(t *testing.T) {
	// With check type None there is no check value; a flipped byte
	// typically breaks LZMA decoding rather than a checksum.
	data := testData(3000)
	xzData := compressXZ(t, data, 500, None, true)

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	b := rr.blocks[len(rr.blocks)-1]

	bad := append([]byte(nil), xzData...)
	bad[b.compressedOff+int64(b.headerLen)] ^= 0x80
	rr2, err := NewRandomReader(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	p := make([]byte, b.uncompressedSize)
	// Decoding may fail or exact-size accounting must reject the
	// corrupted block; success would mean corruption went unnoticed.
	if _, err = rr2.ReadAt(p, b.unpOff); err == nil &&
		!bytes.Equal(p, data[b.unpOff:]) {
		t.Fatalf("corruption detected only via mismatch")
	}
}

func TestRandomReaderMemoryCaps(t *testing.T) {
	data := testData(5000)
	xzData := compressXZ(t, data, 500, CRC64, false)

	// Index cap smaller than the actual index.
	smallIndex := RandomReaderConfig{MaxIndexMemory: 4}
	if _, err := smallIndex.NewRandomReader(
		bytes.NewReader(xzData), int64(len(xzData))); err == nil {
		t.Fatalf("small index cap accepted")
	}

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}

	// Dictionary cap smaller than the block's requested dictionary.
	smallDict := RandomReaderConfig{MaxDictCap: rr.blocks[0].dictCap - 1}
	if _, err := smallDict.NewRandomReader(
		bytes.NewReader(xzData), int64(len(xzData))); err == nil {
		t.Fatalf("small dict cap accepted")
	}

	// Block cap smaller than the largest block uncompressed size.
	var maxBlock int64
	for _, b := range rr.blocks {
		if b.uncompressedSize > maxBlock {
			maxBlock = b.uncompressedSize
		}
	}
	smallBlock := RandomReaderConfig{MaxBlockMemory: maxBlock - 1}
	if _, err := smallBlock.NewRandomReader(
		bytes.NewReader(xzData), int64(len(xzData))); err == nil {
		t.Fatalf("small block cap accepted")
	}
}

func TestRandomReaderRejectsContradictoryLengths(t *testing.T) {
	data := testData(3000)
	xzData := compressXZ(t, data, 500, CRC64, false)

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	b := rr.blocks[1]

	// Rewrite the block header claiming a different uncompressed size
	// while keeping the same header length (and rebuilding the
	// CRC-32). The index still records the original size, so the
	// contradiction must be detected at open time.
	bad := append([]byte(nil), xzData...)
	off := b.compressedOff

	hdr := make([]byte, b.headerLen)
	copy(hdr, bad[off:off+int64(b.headerLen)])

	// Build the replacement header manually: flags, claimed
	// uncompressed size, filter, padding, crc.
	nb := blockHeader{
		compressedSize:   -1,
		uncompressedSize: b.uncompressedSize + 1,
		filters:          b.filters,
	}
	nbData, err := nb.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary %s", err)
	}
	if len(nbData) != b.headerLen {
		t.Skipf("replacement header length %d != %d",
			len(nbData), b.headerLen)
	}
	copy(hdr, nbData)
	copy(bad[off:], hdr)

	if _, err := NewRandomReader(bytes.NewReader(bad),
		int64(len(bad))); err == nil {
		t.Fatalf("contradictory block header accepted")
	}
}

func TestRandomReaderRejectsBadPadding(t *testing.T) {
	data := testData(3000)
	xzData := compressXZ(t, data, 500, CRC64, false)

	// Inter-stream padding must be a multiple of four zero bytes.
	bad := append([]byte(nil), xzData...)
	bad = append(bad, xzData...)
	bad[len(xzData)+2] = 1
	if _, err := NewRandomReader(bytes.NewReader(bad),
		int64(len(bad))); err == nil {
		t.Fatalf("illegal inter-stream padding accepted")
	}
}

func TestRandomReaderIncompressibleAndSequentialCrossCheck(t *testing.T) {
	// Incompressible data forces the writer to emit uncompressed
	// LZMA2 chunks, exercising a different code path in the exact
	// compressed-size accounting.
	const N = 7000
	rng := rand.New(rand.NewSource(123))
	data := make([]byte, N)
	if _, err := rng.Read(data); err != nil {
		t.Fatalf("rng.Read %s", err)
	}
	xzData := compressXZ(t, data, 900, CRC64, false)

	// Ground truth from the sequential reader.
	seq, err := NewReader(bytes.NewReader(xzData))
	if err != nil {
		t.Fatalf("NewReader %s", err)
	}
	want, err := io.ReadAll(seq)
	if err != nil {
		t.Fatalf("sequential ReadAll %s", err)
	}
	if !bytes.Equal(want, data) {
		t.Fatalf("sequential decompression mismatch")
	}

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}

	for _, length := range []int64{1, 17, 899, 900, 901, 5000} {
		for start := int64(0); start+length <= N; start += 333 {
			p := make([]byte, length)
			n, rerr := rr.ReadAt(p, start)
			if rerr != nil && rerr != io.EOF {
				t.Fatalf("ReadAt(%d,%d) %s", start, length, rerr)
			}
			if int64(n) != length {
				t.Fatalf("ReadAt(%d,%d) n %d", start, length, n)
			}
			if !bytes.Equal(p, want[start:start+length]) {
				t.Fatalf("ReadAt(%d,%d) mismatch vs sequential",
					start, length)
			}
		}
	}
}

func TestRandomReaderEmptyP(t *testing.T) {
	data := testData(1000)
	xzData := compressXZ(t, data, 300, CRC32, false)
	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	if n, err := rr.ReadAt(nil, 50); err != nil || n != 0 {
		t.Fatalf("ReadAt(nil,50) n=%d err=%v", n, err)
	}
	if n, err := rr.ReadAt(nil, -1); err == nil {
		t.Fatalf("negative offset accepted n=%d", n)
	}
}

func TestRandomReaderEmptyAndFixtures(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		xzData := compressXZ(t, nil, 100, CRC64, false)
		rr, err := NewRandomReader(bytes.NewReader(xzData),
			int64(len(xzData)))
		if err != nil {
			t.Fatalf("NewRandomReader %s", err)
		}
		if rr.Size() != 0 {
			t.Fatalf("size %d; want 0", rr.Size())
		}
		if _, err = rr.ReadAt(make([]byte, 1), 0); err != io.EOF {
			t.Fatalf("ReadAt on empty err %v; want io.EOF", err)
		}
	})

	for _, file := range []string{"fox.xz", "fox-check-none.xz"} {
		t.Run(file, func(t *testing.T) {
			xzData, err := os.ReadFile(file)
			if err != nil {
				t.Skipf("fixture %s missing: %s", file, err)
			}

			// Sequential ground truth.
			seq, err := NewReader(bytes.NewReader(xzData))
			if err != nil {
				t.Fatalf("NewReader %s", err)
			}
			want, err := io.ReadAll(seq)
			if err != nil {
				t.Fatalf("ReadAll %s", err)
			}

			rr, err := NewRandomReader(bytes.NewReader(xzData),
				int64(len(xzData)))
			if err != nil {
				t.Fatalf("NewRandomReader %s", err)
			}
			if rr.Size() != int64(len(want)) {
				t.Fatalf("size %d; want %d", rr.Size(), len(want))
			}
			p := make([]byte, len(want))
			if _, err = io.ReadFull(
				io.NewSectionReader(rr, 0, rr.Size()), p); err != nil {
				t.Fatalf("ReadFull %s", err)
			}
			if !bytes.Equal(p, want) {
				t.Fatalf("%s mismatch", file)
			}
		})
	}
}

func TestRandomReaderManySmallStreams(t *testing.T) {
	rng := rand.New(rand.NewSource(2024))
	const streams = 12
	var xzData []byte
	var data []byte
	for i := 0; i < streams; i++ {
		n := 1 + rng.Intn(2500)
		chunk := testData(n)
		data = append(data, chunk...)
		xzData = append(xzData,
			compressXZ(t, chunk, int64(1+rng.Intn(300)),
				CRC32, false)...)
		// A random number of legal padding groups between streams,
		// but not after the final stream.
		if i+1 < streams {
			pads := 1 + rng.Intn(3)
			for j := 0; j < pads; j++ {
				xzData = append(xzData, 0, 0, 0, 0)
			}
		}
	}

	rr, err := NewRandomReader(bytes.NewReader(xzData),
		int64(len(xzData)))
	if err != nil {
		t.Fatalf("NewRandomReader %s", err)
	}
	if rr.Size() != int64(len(data)) {
		t.Fatalf("size %d; want %d", rr.Size(), len(data))
	}

	// Check individual bytes via ReadAt, sampling densely enough to
	// cover block and stream boundaries. (ReadAt intentionally does
	// not cache decompressed blocks.)
	for i := 0; i < len(data); i += 7 {
		var p [1]byte
		if _, err = rr.ReadAt(p[:], int64(i)); err != nil {
			t.Fatalf("ReadAt(%d) %s", i, err)
		}
		if p[0] != data[i] {
			t.Fatalf("byte %d mismatch", i)
		}
	}
	// Explicitly check the first and last byte of every block.
	for _, b := range rr.blocks {
		for _, o := range []int64{
			b.unpOff,
			b.unpOff + b.uncompressedSize - 1,
		} {
			var p [1]byte
			if _, err = rr.ReadAt(p[:], o); err != nil {
				t.Fatalf("boundary ReadAt(%d) %s", o, err)
			}
			if p[0] != data[o] {
				t.Fatalf("boundary byte %d mismatch", o)
			}
		}
	}

	// The number of parsed blocks must be positive and the covered
	// streams must all be reachable.
	if len(rr.blocks) < streams {
		t.Fatalf("blocks %d; want at least %d",
			len(rr.blocks), streams)
	}
}

func TestNewRandomReaderErrors(t *testing.T) {
	if _, err := NewRandomReader(nil, 10); err == nil {
		t.Fatalf("nil ReaderAt accepted")
	}
	if _, err := NewRandomReader(bytes.NewReader(nil), -1); err == nil {
		t.Fatalf("negative size accepted")
	}
	if _, err := NewRandomReader(bytes.NewReader(nil), 0); err == nil {
		t.Fatalf("empty file accepted")
	}
	// Random bytes must not parse as a valid stream.
	rng := rand.New(rand.NewSource(1))
	garbage := make([]byte, 256)
	if _, err := rng.Read(garbage); err != nil {
		t.Fatalf("rng.Read %s", err)
	}
	if _, err := NewRandomReader(bytes.NewReader(garbage),
		int64(len(garbage))); err == nil {
		t.Fatalf("garbage accepted as xz")
	}
}
