// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"testing"
)

// TestSeekReaderEnwik7 is an integration test on a large, highly
// compressible input. It builds a multi-block, multi-stream file, then
// cross-checks many random intervals against sequential decompression
// and asserts that a small read touches only the relevant block.
func TestSeekReaderEnwik7(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	raw, err := os.ReadFile("testdata/enwik7")
	if err != nil {
		t.Skipf("testdata/enwik7 unavailable: %s", err)
	}

	// Two streams with 1 MiB blocks.
	mid := len(raw) / 2
	var m bytes.Buffer
	m.Write(compress(t, raw[:mid], 1<<20))
	m.Write([]byte{0, 0, 0, 0})
	m.Write(compress(t, raw[mid:], 1<<20))
	data := m.Bytes()

	sr, err := NewReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReaderAt: %s", err)
	}
	if sr.Size() != int64(len(raw)) {
		t.Fatalf("Size %d; want %d", sr.Size(), len(raw))
	}

	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 30; i++ {
		off := rnd.Int63n(int64(len(raw)))
		ln := 1 + rnd.Intn(200000)
		if off+int64(ln) > int64(len(raw)) {
			ln = int(int64(len(raw)) - off)
		}
		p := make([]byte, ln)
		if _, err := sr.ReadAt(p, off); err != nil {
			t.Fatalf("iter %d ReadAt(%d,%d): %s", i, off, ln, err)
		}
		if !bytes.Equal(p, raw[off:off+int64(ln)]) {
			t.Fatalf("iter %d mismatch", i)
		}
	}

	// Locality: a single byte in a middle block must not pull a large
	// fraction of the file.
	b := sr.block[len(sr.block)/2]
	cr := &countReaderAt{r: bytes.NewReader(data)}
	csr, err := NewReaderAt(cr, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	cr.total, cr.calls, cr.ranges = 0, 0, nil
	one := make([]byte, 1)
	if _, err := csr.ReadAt(one, b.unpaddedOff+3); err != nil {
		t.Fatal(err)
	}
	if one[0] != raw[b.unpaddedOff+3] {
		t.Fatal("single-byte content mismatch")
	}
	if cr.total > int64(len(data))/4 {
		t.Fatalf("single-byte read consumed %d of %d bytes",
			cr.total, len(data))
	}

	// Sequential sanity.
	seq, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, seq)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(raw)) {
		t.Fatalf("sequential length %d; want %d", n, len(raw))
	}
}
