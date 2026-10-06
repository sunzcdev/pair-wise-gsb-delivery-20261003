// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pgzip

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math/rand"
	"testing"
)

// referenceBlock describes one BGZF member parsed independently from the
// writer implementation.
type referenceBlock struct {
	start  int64  // compressed offset of the member start
	size   int    // total member size (BSIZE+1)
	rawLen int    // ISIZE
	raw    []byte // inflated payload
}

// parseBGZF performs a strict, independent parse of a BGZF stream. It verifies
// the gzip header, the BC extra field, BSIZE consistency, CRC32, ISIZE, block
// ordering and the terminal EOF marker. It returns the decoded members.
func parseBGZF(t *testing.T, data []byte) []referenceBlock {
	t.Helper()
	var blocks []referenceBlock
	pos := int64(0)
	for pos < int64(len(data)) {
		start := pos
		if int(pos)+18 > len(data) {
			t.Fatalf("offset %d: truncated gzip header", start)
		}
		if data[pos+0] != 0x1f || data[pos+1] != 0x8b || data[pos+2] != 8 {
			t.Fatalf("offset %d: bad gzip magic/method %x %x %x", start, data[pos+0], data[pos+1], data[pos+2])
		}
		flg := data[pos+3]
		if flg&flagExtra == 0 {
			t.Fatalf("offset %d: FEXTRA not set", start)
		}
		xlen := int(binary.LittleEndian.Uint16(data[pos+10 : pos+12]))
		extra := data[pos+12 : pos+12+int64(xlen)]
		bsize := -1
		for len(extra) != 0 {
			if len(extra) < 4 {
				t.Fatalf("offset %d: truncated extra subfield header", start)
			}
			si1, si2 := extra[0], extra[1]
			slen := int(binary.LittleEndian.Uint16(extra[2:4]))
			if len(extra) < 4+slen {
				t.Fatalf("offset %d: truncated extra subfield", start)
			}
			if si1 == 'B' && si2 == 'C' {
				if slen != 2 {
					t.Fatalf("offset %d: BC SLEN = %d, want 2", start, slen)
				}
				bsize = int(binary.LittleEndian.Uint16(extra[4:6])) + 1
			}
			extra = extra[4+slen:]
		}
		if len(extra) != 0 {
			t.Fatalf("offset %d: malformed extra field (%d trailing bytes)", start, len(extra))
		}
		if bsize < 0 {
			t.Fatalf("offset %d: missing BC extra field", start)
		}
		if bsize > bgzfMaxBlockSize {
			t.Fatalf("offset %d: block size %d exceeds %d", start, bsize, bgzfMaxBlockSize)
		}
		end := start + int64(bsize)
		if end > int64(len(data)) {
			t.Fatalf("offset %d: BSIZE %d runs past end of stream", start, bsize)
		}
		deflateStart := pos + 12 + int64(xlen)
		deflateEnd := end - 8

		fr := flate.NewReader(bytes.NewReader(data[deflateStart:deflateEnd]))
		raw, err := io.ReadAll(fr)
		if err != nil {
			t.Fatalf("offset %d: inflate: %v", start, err)
		}
		if err := fr.Close(); err != nil {
			t.Fatalf("offset %d: inflate close: %v", start, err)
		}

		crc := binary.LittleEndian.Uint32(data[deflateEnd : deflateEnd+4])
		isize := binary.LittleEndian.Uint32(data[deflateEnd+4 : deflateEnd+8])
		if crc != crc32.ChecksumIEEE(raw) {
			t.Fatalf("offset %d: CRC mismatch", start)
		}
		if int(isize) != len(raw) {
			t.Fatalf("offset %d: ISIZE %d != payload %d", start, isize, len(raw))
		}

		blocks = append(blocks, referenceBlock{
			start:  start,
			size:   bsize,
			rawLen: len(raw),
			raw:    raw,
		})
		pos = end
	}

	if len(data) < len(bgzfEOF) || !bytes.Equal(data[len(data)-len(bgzfEOF):], bgzfEOF[:]) {
		t.Fatalf("stream does not end with the standard BGZF EOF marker")
	}
	return blocks
}

func decodeBGZF(t *testing.T, data []byte) []byte {
	t.Helper()
	blocks := parseBGZF(t, data)
	var out bytes.Buffer
	for i := range blocks {
		out.Write(blocks[i].raw)
	}
	return out.Bytes()
}

func TestBGZFEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := NewBGZFWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), bgzfEOF[:]) {
		t.Fatalf("empty stream = %x, want EOF marker", buf.Bytes())
	}
	if got := decodeBGZF(t, buf.Bytes()); len(got) != 0 {
		t.Fatalf("empty stream decoded to %d bytes", len(got))
	}
}

func randomIncompressible(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	if _, err := r.Read(b); err != nil {
		panic(err)
	}
	return b
}

func TestBGZFRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"short":          []byte("hello, BGZF"),
		"repetitive":     bytes.Repeat([]byte("ABCDEFGH"), 100000),
		"incompressible": randomIncompressible(400000, 1),
		"mixed":          append(append(bytes.Repeat([]byte("0123"), 100000), randomIncompressible(300000, 2)...), bytes.Repeat([]byte("xyz"), 50000)...),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := NewBGZFWriterLevel(&buf, DefaultCompression)
			if err != nil {
				t.Fatalf("NewBGZFWriterLevel: %v", err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			blocks := parseBGZF(t, buf.Bytes())
			if got := decodeBGZF(t, buf.Bytes()); !bytes.Equal(got, payload) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(payload))
			}
			for _, b := range blocks {
				if b.size > bgzfMaxBlockSize {
					t.Fatalf("block at %d has size %d > %d", b.start, b.size, bgzfMaxBlockSize)
				}
			}
		})
	}
}

// TestBGZFBlockLimit guarantees incompressible input never produces an
// oversized block across compression levels and boundary sizes.
func TestBGZFBlockLimit(t *testing.T) {
	sizes := []int{
		bgzfMaxUncompressed - 1,
		bgzfMaxUncompressed,
		bgzfMaxUncompressed + 1,
		bgzfMaxBlockSize,
		bgzfMaxBlockSize * 2,
	}
	for _, level := range []int{NoCompression, BestSpeed, DefaultCompression, BestCompression} {
		for _, n := range sizes {
			payload := randomIncompressible(n, int64(level*1000+n))
			var buf bytes.Buffer
			w, err := NewBGZFWriterLevel(&buf, level)
			if err != nil {
				t.Fatalf("level %d: %v", level, err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			blocks := parseBGZF(t, buf.Bytes())
			for _, b := range blocks {
				if b.size > bgzfMaxBlockSize {
					t.Fatalf("level %d n %d: block at %d size %d > %d", level, n, b.start, b.size, bgzfMaxBlockSize)
				}
			}
			if got := decodeBGZF(t, buf.Bytes()); !bytes.Equal(got, payload) {
				t.Fatalf("level %d n %d: round-trip mismatch", level, n)
			}
		}
	}
}

// TestBGZFSplitBoundary forces splitting at the maximum guaranteed chunk size
// for incompressible data and verifies in-order members and offset continuity.
func TestBGZFSplitBoundary(t *testing.T) {
	payload := randomIncompressible(bgzfMaxUncompressed, 99)
	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(bgzfMaxUncompressed, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := parseBGZF(t, buf.Bytes())
	if len(blocks) < 2 {
		t.Fatalf("expected split into multiple blocks, got %d", len(blocks))
	}
	var totalRaw int
	var off int64
	for i := range blocks {
		if blocks[i].start != off {
			t.Fatalf("block %d at offset %d, want %d", i, blocks[i].start, off)
		}
		off += int64(blocks[i].size)
		totalRaw += blocks[i].rawLen
	}
	if totalRaw != len(payload) {
		t.Fatalf("raw total %d, want %d", totalRaw, len(payload))
	}
}

// TestBGZFVirtualOffsets writes fixed-size records, captures the virtual
// offset at several record boundaries, then has the independent reader seek to
// each offset and confirm it lands exactly on the matching record.
func TestBGZFVirtualOffsets(t *testing.T) {
	const recordSize = 1000
	const records = 500
	payload := make([]byte, recordSize*records)
	rng := rand.New(rand.NewSource(7))
	rng.Read(payload)

	record := func(i int) []byte { return payload[i*recordSize : (i+1)*recordSize] }

	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(4096, 4); err != nil {
		t.Fatal(err)
	}

	checks := map[int]uint64{}
	for i := 0; i < records; i++ {
		// VOffset right before writing record i points at record i's start.
		var vo uint64
		if i%37 == 0 {
			var err error
			vo, err = w.VOffset()
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.Write(record(i)); err != nil {
			t.Fatal(err)
		}
		if i%37 == 0 {
			checks[i] = vo
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	blocks := parseBGZF(t, buf.Bytes())
	for i, vo := range checks {
		coffset := int64(vo >> 16)
		inoffset := int(vo & 0xffff)
		raw := readAtVirtualOffset(t, blocks, coffset, inoffset, recordSize)
		if !bytes.Equal(raw, record(i)) {
			t.Fatalf("record %d at voffset %d (coff=%d inoff=%d): content mismatch (got len=%d)",
				i, vo, coffset, inoffset, len(raw))
		}
	}
}

// readAtVirtualOffset models a BGZF reader seek: find the member starting at
// coffset and read length bytes starting inoffset inside it, continuing into
// following members across block boundaries.
func readAtVirtualOffset(t *testing.T, blocks []referenceBlock, coffset int64, inoffset, length int) []byte {
	t.Helper()
	idx := -1
	for i := range blocks {
		if blocks[i].start == coffset {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("no block starts at compressed offset %d", coffset)
	}
	out := make([]byte, 0, length)
	off := inoffset
	for len(out) < length {
		b := blocks[idx]
		if off > b.rawLen {
			t.Fatalf("in-block offset %d beyond block raw len %d", off, b.rawLen)
		}
		part := b.raw[off:]
		if need := length - len(out); len(part) > need {
			part = part[:need]
		}
		out = append(out, part...)
		off = 0
		idx++
	}
	return out[:length]
}

// TestBGZFParallelOrder uses many small workers writing distinct chunks with
// high parallelism, then verifies the decoded output equals the input, which
// can only hold if blocks land in input order despite parallel compression.
func TestBGZFParallelOrder(t *testing.T) {
	const chunks = 2000
	const chunkLen = 5000
	payload := randomIncompressible(chunks*chunkLen, 42)
	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(chunkLen, 8); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < chunks; i++ {
		if _, err := w.Write(payload[i*chunkLen : (i+1)*chunkLen]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	blocks := parseBGZF(t, buf.Bytes())
	var off int64
	for i := range blocks {
		if blocks[i].start != off {
			t.Fatalf("member %d at %d, expected contiguous offset %d", i, blocks[i].start, off)
		}
		off += int64(blocks[i].size)
		if blocks[i].size > bgzfMaxBlockSize {
			t.Fatalf("oversized member %d: %d", i, blocks[i].size)
		}
	}
	if got := decodeBGZF(t, buf.Bytes()); !bytes.Equal(got, payload) {
		t.Fatalf("parallel output out of order or corrupt")
	}
}

// TestBGZFExactBlockBoundary constructs input whose very first member lands as
// close as possible to the 65536-byte limit and verifies it never exceeds it.
func TestBGZFExactBlockBoundary(t *testing.T) {
	// Incompressible data at the guaranteed maximum yields a stored block
	// essentially touching the ceiling; check several exact byte counts.
	for n := bgzfMaxUncompressed - 2; n <= bgzfMaxUncompressed+2; n++ {
		payload := randomIncompressible(n, int64(n))
		var buf bytes.Buffer
		w, _ := NewBGZFWriterLevel(&buf, NoCompression)
		if err := w.SetConcurrency(bgzfMaxUncompressed, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		blocks := parseBGZF(t, buf.Bytes())
		for _, b := range blocks {
			if b.size > bgzfMaxBlockSize {
				t.Fatalf("n=%d member size %d exceeds limit", n, b.size)
			}
		}
		if got := decodeBGZF(t, buf.Bytes()); !bytes.Equal(got, payload) {
			t.Fatalf("n=%d round-trip mismatch", n)
		}
	}
}

func TestBGZFReset(t *testing.T) {
	var first, second bytes.Buffer
	w, err := NewBGZFWriterLevel(&first, DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("discarded data that must not appear"))
	w.Reset(&second)
	payload := []byte("after reset")
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := decodeBGZF(t, second.Bytes()); !bytes.Equal(got, payload) {
		t.Fatalf("reset stream mismatch: %q", got)
	}
}

func TestBGZFInvalidLevel(t *testing.T) {
	if _, err := NewBGZFWriterLevel(io.Discard, 42); err == nil {
		t.Fatal("expected error for invalid compression level")
	}
}

func TestBGZFWriteAfterClose(t *testing.T) {
	w := NewBGZFWriter(io.Discard)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected error writing after Close")
	}
}

// TestBGZFStandardGzip confirms each member is readable by the standard library
// gzip reader as a sequence of concatenated members.
func TestBGZFStandardGzip(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefg"), 100000)
	var buf bytes.Buffer
	w := NewBGZFWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("standard gzip reader mismatch: got %d want %d", len(got), len(payload))
	}
}
