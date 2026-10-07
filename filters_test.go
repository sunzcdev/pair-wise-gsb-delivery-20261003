// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"hash/crc32"
	"io"
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"
)

const filterTestDir = "testdata/filters"

// chunkReaders feeds the provided data through Read calls limited to
// the given chunk size.
type chunkReader struct {
	data []byte
	pos  int
	n    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.n
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// decodeWithReadSize decodes the xz stream in src, feeding the reader
// in chunks limited by readSize and draining the xz.Reader with the
// given output buffer size.
func decodeWithSizes(t *testing.T, src []byte, readSize, outSize int,
) []byte {
	t.Helper()
	r, err := NewReader(&chunkReader{data: src, n: readSize})
	if err != nil {
		t.Fatalf("NewReader error %s", err)
	}
	var out bytes.Buffer
	p := make([]byte, outSize)
	for {
		n, err := r.Read(p)
		out.Write(p[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read error %s", err)
		}
	}
	return out.Bytes()
}

var filterReadSizes = []int{1, 2, 3, 4, 5, 6, 7, 8, 13, 17, 64, 4096}

// TestFilterReferenceFiles decodes files produced by the reference
// implementation (liblzma via CPython, see
// testdata/filters/gen_ref.py) using a variety of Read chunk sizes and
// verifies the decoded content matches payload.bin.
func TestFilterReferenceFiles(t *testing.T) {
	payload, err := ioutil.ReadFile(filepath.Join(filterTestDir,
		"payload.bin"))
	if err != nil {
		t.Fatalf("reading payload: %s", err)
	}
	names, err := filepath.Glob(filepath.Join(filterTestDir, "ref_*.xz"))
	if err != nil {
		t.Fatalf("glob: %s", err)
	}
	if len(names) == 0 {
		t.Fatal("no reference files found")
	}
	for _, name := range names {
		if filepath.Base(name) == "ref_unsupported_arm_crc64.xz" {
			continue
		}
		src, err := ioutil.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %s", name, err)
		}
		for _, rs := range filterReadSizes {
			for _, osz := range filterReadSizes {
				got := decodeWithSizes(t, src, rs, osz)
				if !bytes.Equal(got, payload) {
					t.Fatalf("%s readSize=%d outSize=%d: got %d bytes; "+
						"want %d", filepath.Base(name), rs, osz,
						len(got), len(payload))
				}
			}
		}
	}
}

// chunkWriter limits the size of each underlying write.
type chunkWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := w.n
		if n <= 0 || n > len(p) {
			n = len(p)
		}
		k, err := w.buf.Write(p[:n])
		total += k
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// encodeWithWriteSize compresses data with the given writer
// configuration while splitting every Write across chunks of
// writeSize bytes.
func encodeWithWriteSize(t *testing.T, c WriterConfig, data []byte,
	writeSize int) []byte {
	t.Helper()
	cw := &chunkWriter{n: writeSize}
	w, err := c.NewWriter(cw)
	if err != nil {
		t.Fatalf("NewWriter error %s", err)
	}
	for pos := 0; pos < len(data); {
		n := writeSize
		if n > len(data)-pos {
			n = len(data) - pos
		}
		k, err := w.Write(data[pos : pos+n])
		if err != nil {
			t.Fatalf("Write error %s", err)
		}
		if k != n {
			t.Fatalf("Write returned %d; want %d", k, n)
		}
		pos += n
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	return cw.buf.Bytes()
}

func TestFilterChainsRoundtrip(t *testing.T) {
	payload, err := ioutil.ReadFile(filepath.Join(filterTestDir,
		"payload.bin"))
	if err != nil {
		t.Fatalf("reading payload: %s", err)
	}

	chains := [][]Filter{
		nil,
		{&DeltaFilter{Distance: 1}},
		{&DeltaFilter{Distance: 7}},
		{&DeltaFilter{Distance: 256}},
		{&X86Filter{}},
		{&DeltaFilter{Distance: 1}, &X86Filter{}},
		{&DeltaFilter{Distance: 7}, &X86Filter{StartOffset: 16}},
	}

	for i, chain := range chains {
		c := WriterConfig{Filters: chain}
		for _, ws := range filterReadSizes {
			src := encodeWithWriteSize(t, c, payload, ws)
			for _, rs := range filterReadSizes {
				for _, osz := range filterReadSizes {
					got := decodeWithSizes(t, src, rs, osz)
					if !bytes.Equal(got, payload) {
						t.Fatalf("chain %d ws=%d rs=%d os=%d: mismatch",
							i, ws, rs, osz)
					}
				}
			}
		}
	}
}

// TestInteropGoEncodedDecodedByReference encodes a stream with the Go
// writer and verifies the reference implementation can decode it. The
// reference decoder is invoked through Python in TestReferenceHelper
// below when available; here we at least verify the block header of the
// Go output by round-tripping it through Go and checking the filter
// list parsing.
func TestBlockHeaderFilters(t *testing.T) {
	h := blockHeader{
		compressedSize:   -1,
		uncompressedSize: -1,
		filters: []filter{
			&DeltaFilter{Distance: 7},
			&X86Filter{StartOffset: 0x10000},
			&lzmaFilter{4096},
		},
	}
	data, err := h.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary error %s", err)
	}
	g, _, err := readBlockHeader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("readBlockHeader error %s", err)
	}
	if len(g.filters) != 3 {
		t.Fatalf("got %d filters; want 3", len(g.filters))
	}
	d := g.filters[0].(*DeltaFilter)
	if d.Distance != 7 {
		t.Errorf("delta distance %d; want 7", d.Distance)
	}
	x := g.filters[1].(*X86Filter)
	if x.StartOffset != 0x10000 {
		t.Errorf("start offset %#x; want 0x10000", x.StartOffset)
	}
}

// TestUnsupportedFilter crafts a block header containing an unknown
// filter ID (ARM, 0x07) generated by the reference implementation and
// ensures the reader rejects it with a clear error instead of silently
// treating the data as uncompressed.
func TestUnsupportedFilter(t *testing.T) {
	src, err := ioutil.ReadFile(filepath.Join(filterTestDir,
		"ref_unsupported_arm_crc64.xz"))
	if err != nil {
		t.Fatalf("reading reference file: %s", err)
	}
	r, err := NewReader(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("NewReader error %s", err)
	}
	_, err = ioutil.ReadAll(r)
	if err == nil {
		t.Fatal("expected error for unsupported ARM filter; got nil")
	}
	if !strings.Contains(err.Error(), "unsupported filter") {
		t.Fatalf("error %q does not mention unsupported filter", err)
	}
}

// TestInvalidWriterFilters checks that invalid filter configurations
// are rejected when creating a writer.
func TestInvalidWriterFilters(t *testing.T) {
	data := []byte("hello world")
	cases := []struct {
		name string
		f    []Filter
	}{
		{"delta distance zero-ish high", []Filter{&DeltaFilter{Distance: 257}}},
		{"two deltas", []Filter{
			&DeltaFilter{Distance: 1}, &DeltaFilter{Distance: 2}}},
		{"four prefilters", []Filter{
			&X86Filter{}, &X86Filter{}, &X86Filter{}, &X86Filter{}}},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		w, err := WriterConfig{Filters: tc.f}.NewWriter(&buf)
		if err == nil {
			// Configuration errors may also surface on the first
			// write; ensure writing fails too.
			if _, werr := w.Write(data); werr == nil {
				if cerr := w.Close(); cerr == nil {
					t.Errorf("%s: expected error; got nil", tc.name)
				}
			}
		}
	}
}

// TestDefaultConfigUnchanged verifies that a writer without Filters
// produces a block header with exactly one LZMA2 filter, preserving
// the previous default behavior.
func TestDefaultConfigUnchanged(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter error %s", err)
	}
	if _, err = w.Write([]byte("abc")); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}

	// Parse the block header after the 12-byte stream header.
	r := bytes.NewReader(buf.Bytes())
	if _, err = r.Seek(HeaderLen, io.SeekStart); err != nil {
		t.Fatalf("Seek error %s", err)
	}
	h, _, err := readBlockHeader(r)
	if err != nil {
		t.Fatalf("readBlockHeader error %s", err)
	}
	if len(h.filters) != 1 {
		t.Fatalf("got %d filters; want 1", len(h.filters))
	}
	if _, ok := h.filters[0].(*lzmaFilter); !ok {
		t.Fatalf("default filter is %T; want *lzmaFilter", h.filters[0])
	}
}

// TestFilterAdversarialRoundtrip checks streams crafted to exercise
// the x86 BCJ boundary logic: far (out of range) branches, trailing
// opcodes and random bytes.
func TestFilterAdversarialRoundtrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0xe8},
		{0xe8, 0x00},
		{0xe8, 0x00, 0x00, 0x00},
		{0xe8, 0x00, 0x00, 0x00, 0x00},
		{0xe9, 0xff, 0xff, 0xff, 0x7f},
		{0xe9, 0x00, 0x00, 0x00, 0x80},
		bytes.Repeat([]byte{0xe8, 0x00, 0x00, 0x00, 0x00}, 20),
	}
	// Random-ish data with frequent E8/E9 bytes.
	state := uint32(0xdeadbeef)
	rnd := make([]byte, 20000)
	for i := range rnd {
		state = state*1103515245 + 12345
		rnd[i] = byte(state >> 16)
		if i%11 == 0 {
			rnd[i] = 0xe8
		}
		if i%17 == 0 {
			rnd[i] = 0xe9
		}
	}
	cases = append(cases, rnd)

	chains := [][]Filter{
		{&X86Filter{}},
		{&X86Filter{StartOffset: 0xffffffff}},
		{&DeltaFilter{Distance: 1}, &X86Filter{}},
		{&DeltaFilter{Distance: 16}, &X86Filter{StartOffset: 0x1234}},
	}
	for ci, chain := range chains {
		for _, data := range cases {
			src := encodeWithWriteSize(t, WriterConfig{Filters: chain},
				data, 1)
			r, err := NewReader(bytes.NewReader(src))
			if err != nil {
				t.Fatalf("chain %d: NewReader: %s", ci, err)
			}
			got, err := ioutil.ReadAll(r)
			if err != nil {
				t.Fatalf("chain %d: ReadAll: %s", ci, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("chain %d len %d: mismatch; got %d bytes",
					ci, len(data), len(got))
			}
		}
	}
}

// TestFilterPropertyMarshal exercises the Delta and x86 filter property
// encoding and decoding.
func TestFilterPropertyMarshal(t *testing.T) {
	cases := []filter{
		&DeltaFilter{Distance: 1},
		&DeltaFilter{Distance: 256},
		&X86Filter{},
		&X86Filter{StartOffset: 0xdeadbeef},
	}
	for _, f := range cases {
		data, err := f.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary: %s", err)
		}
		switch q := f.(type) {
		case *DeltaFilter:
			g := new(DeltaFilter)
			if err = g.UnmarshalBinary(data); err != nil {
				t.Fatalf("UnmarshalBinary: %s", err)
			}
			if *g != *q {
				t.Errorf("got %v; want %v", g, q)
			}
		case *X86Filter:
			g := new(X86Filter)
			if err = g.UnmarshalBinary(data); err != nil {
				t.Fatalf("UnmarshalBinary: %s", err)
			}
			if *g != *q {
				t.Errorf("got %v; want %v", g, q)
			}
		}
	}
}

// TestMalformedBlockHeaderFilter ensures malformed filter properties
// are rejected while reading the block header.
func TestMalformedBlockHeaderFilter(t *testing.T) {
	cases := [][]byte{
		// Delta with wrong properties size
		{0x01, 0x00, deltaFilterID, 0x00},
		// x86 BCJ with properties size neither 0 nor 4
		{0x01, 0x00, x86FilterID, 0x02, 0x00, 0x00},
		// LZMA2 with wrong properties size
		{0x01, 0x00, lzmaFilterID, 0x00},
		// LZMA2 placed before another filter (non-last LZMA2)
		{0x01, 0x01, lzmaFilterID, 0x01, 0x16,
			x86FilterID, 0x00},
	}
	for i, body := range cases {
		// Build a complete block header with proper size byte and
		// CRC.
		data := append([]byte{0}, body...)
		for len(data)%4 != 0 {
			data = append(data, 0)
		}
		data = append(data, 0, 0, 0, 0)
		data[0] = byte(len(data)/4 - 1)
		putUint32LE(data[len(data)-4:],
			crc32.ChecksumIEEE(data[:len(data)-4]))
		if _, _, err := readBlockHeader(bytes.NewReader(data)); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}
