// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pgzip

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
)

// checkBlocks walks every BGZF member with the independent reader and
// enforces the format invariants and the 64 KiB size limit.
func checkBlocks(t *testing.T, data []byte) {
	t.Helper()
	rr := newRefBGZFReader(bytes.NewReader(data))
	var off int64
	var blocks int
	for off < int64(len(data)) {
		blk, err := rr.readBlock(off)
		if err != nil {
			t.Fatalf("block at %d: %v", off, err)
		}
		if blk.size > bgzfMaxBlockSize {
			t.Fatalf("block at %d is %d bytes, exceeds %d", off, blk.size, bgzfMaxBlockSize)
		}
		if bs := int(binary.LittleEndian.Uint16(data[off+16:off+18])) + 1; bs != blk.size {
			t.Fatalf("block at %d: BSIZE %d != actual %d", off, bs, blk.size)
		}
		off += int64(blk.size)
		blocks++
	}
	if bytes.Compare(data[len(data)-len(bgzfEOF):], bgzfEOF[:]) != 0 {
		t.Fatal("missing canonical BGZF EOF marker")
	}
	if blocks < 1 {
		t.Fatal("expected at least the EOF member")
	}
}

func TestBGZFEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := NewBGZFWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), bgzfEOF[:]) {
		t.Fatalf("empty input should produce only the EOF marker, got %d bytes", buf.Len())
	}
	checkBlocks(t, buf.Bytes())

	rr := newRefBGZFReader(bytes.NewReader(buf.Bytes()))
	got, err := rr.readAll()
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(got))
	}
}

func TestBGZFRoundTrip(t *testing.T) {
	payload := make([]byte, 3*bgzfDefaultBlockSize+12345)
	// Pseudo-random but deterministic so incompressible content is covered.
	seed := uint32(0x9e3779b9)
	for i := range payload {
		seed = seed*1664525 + 1013904223
		payload[i] = byte(seed >> 16)
	}

	for _, level := range []int{BestSpeed, DefaultCompression, BestCompression, NoCompression} {
		var buf bytes.Buffer
		w, err := NewBGZFWriterLevel(&buf, level)
		if err != nil {
			t.Fatalf("NewBGZFWriterLevel: %v", err)
		}
		// Write in odd-sized chunks to exercise buffer splitting.
		p := payload
		for len(p) > 0 {
			n := 4099
			if n > len(p) {
				n = len(p)
			}
			if _, err := w.Write(p[:n]); err != nil {
				t.Fatalf("level %d Write: %v", level, err)
			}
			p = p[n:]
		}
		if err := w.Close(); err != nil {
			t.Fatalf("level %d Close: %v", level, err)
		}

		checkBlocks(t, buf.Bytes())

		rr := newRefBGZFReader(bytes.NewReader(buf.Bytes()))
		got, err := rr.readAll()
		if err != nil {
			t.Fatalf("level %d readAll: %v", level, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("level %d: payload mismatch", level)
		}

		// stdlib gzip must also accept the concatenated members.
		gz, err := gzip.NewReader(&buf)
		if err != nil {
			t.Fatalf("level %d gzip.NewReader: %v", level, err)
		}
		got2, err := io.ReadAll(gz)
		if err != nil {
			t.Fatalf("level %d stdlib read: %v", level, err)
		}
		if !bytes.Equal(got2, payload) {
			t.Fatalf("level %d: stdlib payload mismatch", level)
		}
	}
}

func TestBGZFIncompressibleNoOversizedBlock(t *testing.T) {
	// Pure cryptographic randomness: cannot compress, so stored blocks must
	// still stay within 64 KiB.
	payload := make([]byte, 5*bgzfDefaultBlockSize+777)
	if _, err := io.ReadFull(rand.Reader, payload); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	// Force the largest allowed input chunk so any expansion is tested.
	if err := w.SetConcurrency(bgzfMaxUncompressed, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checkBlocks(t, buf.Bytes())
	rr := newRefBGZFReader(bytes.NewReader(buf.Bytes()))
	got, err := rr.readAll()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("incompressible payload mismatch")
	}
}

func TestBGZFExactlyAtLimit(t *testing.T) {
	// A chunk exactly bgzfMaxUncompressed bytes long must fit, even under
	// NoCompression where output is a stored deflate block.
	payload := make([]byte, bgzfMaxUncompressed)
	if _, err := io.ReadFull(rand.Reader, payload); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(bgzfMaxUncompressed, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Exactly one data block plus the EOF block.
	if bytes.Compare(buf.Bytes()[len(buf.Bytes())-28:], bgzfEOF[:]) != 0 {
		t.Fatal("missing EOF marker")
	}
	checkBlocks(t, buf.Bytes())

	rr := newRefBGZFReader(bytes.NewReader(buf.Bytes()))
	first, err := rr.readBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	if first.size > bgzfMaxBlockSize {
		t.Fatalf("limit-sized block expanded to %d", first.size)
	}
	if !bytes.Equal(first.data, payload) {
		t.Fatal("limit-sized payload mismatch")
	}
}

func TestBGZFVirtualOffsets(t *testing.T) {
	// Build records of varying sizes spanning multiple blocks.
	var records [][]byte
	var total int
	for total < 4*bgzfDefaultBlockSize {
		rec := make([]byte, 1000+(total*7919)%50000)
		for i := range rec {
			rec[i] = byte(i*31 + total)
		}
		records = append(records, rec)
		total += len(rec)
	}

	var buf bytes.Buffer
	w := NewBGZFWriter(&buf)

	// Record the virtual offset at the start of each record, then write it.
	type marker struct {
		vo    int64
		data  []byte
		inoff uint16
	}
	var markers []marker
	var off int64
	for ri, rec := range records {
		// A virtual offset is only defined for completed blocks, so flush
		// every record before capturing its offset.
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
		off += int64(len(rec))
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		// Every third marker points a few bytes inside the record to cover
		// non-zero within-block offsets too.
		var query int64
		if ri%3 == 0 {
			query = 7
		}
		vo, err := w.VOffset(off - int64(len(rec)) + query)
		if err != nil {
			t.Fatalf("VOffset record %d: %v", ri, err)
		}
		markers = append(markers, marker{vo: vo, data: rec[query:], inoff: uint16(query)})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	checkBlocks(t, buf.Bytes())

	// EOF virtual offset points to the empty EOF member.
	eofVO, err := w.EOFVOffset()
	if err != nil {
		t.Fatal(err)
	}

	rr := newRefBGZFReader(bytes.NewReader(buf.Bytes()))

	for i, mk := range markers {
		if got := uint16(mk.vo & 0xffff); got != mk.inoff {
			t.Fatalf("record %d: within-block offset %d want %d", i, got, mk.inoff)
		}
		got, err := rr.readAtVirt(mk.vo)
		if err != nil {
			t.Fatalf("record %d: seek to %#x: %v", i, mk.vo, err)
		}
		if len(got) < len(mk.data) || !bytes.Equal(got[:len(mk.data)], mk.data) {
			t.Fatalf("record %d: data at virtual offset %#x does not match", i, mk.vo)
		}
	}

	// Reading at the EOF offset yields an empty (EOF) block.
	eofData, err := rr.readAtVirt(eofVO)
	if err != nil {
		t.Fatalf("EOF seek: %v", err)
	}
	if len(eofData) != 0 {
		t.Fatalf("expected empty EOF block, got %d bytes", len(eofData))
	}

	// The full stream still round-trips.
	all, err := rr.readAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != total {
		t.Fatalf("total length %d want %d", len(all), total)
	}
}

func TestBGZFSetConcurrencyRejected(t *testing.T) {
	var buf bytes.Buffer
	w := NewBGZFWriter(&buf)
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(1024, 2); err == nil {
		t.Fatal("expected SetConcurrency after Write to fail")
	}
	if err := w.SetConcurrency(bgzfMaxUncompressed+1, 2); err == nil {
		t.Fatal("expected oversized block to be rejected")
	}
	_ = w.Close()
}

var _ = binary.LittleEndian

func TestBGZFSingleWorker(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefghij"), 20000)
	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(4096, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	checkBlocks(t, buf.Bytes())
	got, err := newRefBGZFReader(bytes.NewReader(buf.Bytes())).readAll()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("single-worker payload mismatch")
	}
}

func TestBGZFParallelOrdering(t *testing.T) {
	// Many small blocks at high concurrency with level jitter can finish out
	// of order; the output and virtual offsets must still follow input.
	payload := make([]byte, 512*200)
	seed := uint32(12345)
	for i := range payload {
		seed = seed*1103515245 + 12345
		payload[i] = byte(seed >> 24)
	}
	want := append([]byte(nil), payload...)

	var buf bytes.Buffer
	w, err := NewBGZFWriterLevel(&buf, BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetConcurrency(512, 16); err != nil {
		t.Fatal(err)
	}

	var offsets []int64
	for off := 0; off < len(want); off += 512 {
		if _, err := w.Write(want[off : off+512]); err != nil {
			t.Fatal(err)
		}
	}
	// Record the virtual offset of every 512-byte record start.
	for pos := 0; pos < len(want); pos += 512 {
		vo, err := w.VOffset(int64(pos))
		if err != nil {
			t.Fatalf("VOffset(%d): %v", pos, err)
		}
		offsets = append(offsets, vo)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	checkBlocks(t, buf.Bytes())

	rr := newRefBGZFReader(bytes.NewReader(buf.Bytes()))
	for i, vo := range offsets {
		got, err := rr.readAtVirt(vo)
		if err != nil {
			t.Fatalf("seek %d: %v", i, err)
		}
		if !bytes.Equal(got[:512], want[i*512:(i+1)*512]) {
			t.Fatalf("record %d at virtual offset %#x mismatch", i, vo)
		}
	}

	all, err := rr.readAll()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(all, want) {
		t.Fatal("parallel output payload mismatch")
	}
}
