// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/ulikunitz/xz/internal/randtxt"
)

// collectRecords inspects the stream and returns all chunk records.
func collectRecords(t *testing.T, data []byte) ([]ChunkRecord, error) {
	t.Helper()
	var recs []ChunkRecord
	err := Inspect2(bytes.NewReader(data), func(rec ChunkRecord) error {
		recs = append(recs, rec)
		return nil
	})
	return recs, err
}

// craftedStream returns a hand-built LZMA2 stream with the chunk
// sequence LRND, UD, LRN, EOS. The chunk payloads contain arbitrary
// bytes because the inspector doesn't decompress them.
func craftedStream(t *testing.T) []byte {
	t.Helper()
	props := Properties{LC: 3, LP: 0, PB: 2}
	headers := []chunkHeader{
		{ctype: cLRND, uncompressed: 3, compressed: 9, props: props},
		{ctype: cUD, uncompressed: 2},
		{ctype: cLRN, uncompressed: 0, compressed: 1, props: props},
		{ctype: cEOS},
	}
	var buf bytes.Buffer
	for _, h := range headers {
		data, err := h.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary for %v error %s", h, err)
		}
		buf.Write(data)
		payload := int64(h.uncompressed) + 1
		if !uncompressed(h.ctype) && h.ctype != cEOS {
			payload = int64(h.compressed) + 1
		}
		if h.ctype == cEOS {
			payload = 0
		}
		buf.Write(make([]byte, payload))
	}
	return buf.Bytes()
}

func TestInspector2Writer2SingleChunk(t *testing.T) {
	var buf bytes.Buffer
	w, err := Writer2Config{DictCap: 4096}.NewWriter2(&buf)
	if err != nil {
		t.Fatalf("NewWriter2 error %s", err)
	}
	if _, err = w.Write([]byte{'a'}); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}

	recs, err := collectRecords(t, buf.Bytes())
	if err != nil {
		t.Fatalf("Inspect2 error %s", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records; want %d", len(recs), 2)
	}
	r0 := recs[0]
	if r0.Offset != 0 || r0.Type != cUD || r0.HeaderLen != 3 ||
		r0.Uncompressed != 1 || r0.Compressed != 0 ||
		!r0.DictReset || r0.StateReset || r0.PropsReset {
		t.Errorf("unexpected first record %s", r0)
	}
	r1 := recs[1]
	if r1.Offset != 4 || r1.Type != cEOS || r1.HeaderLen != 1 ||
		r1.Uncompressed != 0 || r1.Compressed != 0 {
		t.Errorf("unexpected EOS record %s", r1)
	}
	for _, rec := range recs {
		if rec.Verified {
			t.Errorf("record %s marked verified", rec)
		}
	}
}

func TestInspector2Writer2Stream(t *testing.T) {
	const txtlen = 1 << 20
	txt := new(bytes.Buffer)
	io.CopyN(txt, randtxt.NewReader(rand.NewSource(42)), txtlen)

	var buf bytes.Buffer
	w, err := Writer2Config{DictCap: 4096}.NewWriter2(&buf)
	if err != nil {
		t.Fatalf("NewWriter2 error %s", err)
	}
	if _, err = io.Copy(w, txt); err != nil {
		t.Fatalf("compressing copy error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}

	recs, err := collectRecords(t, buf.Bytes())
	if err != nil {
		t.Fatalf("Inspect2 error %s", err)
	}
	if len(recs) < 3 {
		t.Fatalf("got %d records; want at least 3", len(recs))
	}
	var off, totalUncompressed int64
	compressed := 0
	for i, rec := range recs {
		if rec.Offset != off {
			t.Fatalf("record %d offset %d; want %d",
				i, rec.Offset, off)
		}
		off += rec.Size()
		if rec.Verified {
			t.Errorf("record %d %s marked verified", i, rec)
		}
		totalUncompressed += rec.Uncompressed
		if !uncompressed(rec.Type) && rec.Type != cEOS {
			compressed++
		}
	}
	if off != int64(buf.Len()) {
		t.Errorf("records cover %d bytes; stream has %d",
			off, buf.Len())
	}
	if totalUncompressed != txtlen {
		t.Errorf("uncompressed length %d; want %d",
			totalUncompressed, txtlen)
	}
	if first := recs[0]; !first.DictReset {
		t.Errorf("first chunk %s doesn't reset the dictionary",
			first.Type)
	}
	if last := recs[len(recs)-1]; last.Type != cEOS {
		t.Errorf("last chunk %s; want EOS", last.Type)
	}
	if compressed == 0 {
		t.Error("no compressed chunks in Writer2 stream")
	}
}

func TestInspector2CraftedStream(t *testing.T) {
	data := craftedStream(t)
	s := NewInspector2(bytes.NewReader(data))

	want := []ChunkRecord{
		{Offset: 0, Type: cLRND, HeaderLen: 6,
			Uncompressed: 4, Compressed: 10,
			DictReset: true, StateReset: true, PropsReset: true,
			Props: Properties{LC: 3, LP: 0, PB: 2}},
		{Offset: 16, Type: cUD, HeaderLen: 3,
			Uncompressed: 3, DictReset: true},
		{Offset: 22, Type: cLRN, HeaderLen: 6,
			Uncompressed: 1, Compressed: 2,
			StateReset: true, PropsReset: true,
			Props: Properties{LC: 3, LP: 0, PB: 2}},
		{Offset: 30, Type: cEOS, HeaderLen: 1},
	}
	init := []struct{ dict, props, state bool }{
		{true, true, true},
		{true, false, false},
		{true, true, true},
		{true, true, true},
	}
	for i, w := range want {
		rec, err := s.Next()
		if err != nil {
			t.Fatalf("Next record %d error %s", i, err)
		}
		if rec != w {
			t.Errorf("record %d got %s; want %+v", i, rec, w)
		}
		if s.DictInit() != init[i].dict ||
			s.PropsInit() != init[i].props ||
			s.StateInit() != init[i].state {
			t.Errorf("record %d init state %t %t %t; want %v",
				i, s.DictInit(), s.PropsInit(),
				s.StateInit(), init[i])
		}
	}
	if !s.EOS() {
		t.Error("EOS not set after EOS chunk")
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("Next after EOS got %v; want io.EOF", err)
	}
}

func TestInspector2InvalidStreams(t *testing.T) {
	header := func(h chunkHeader) []byte {
		data, err := h.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary error %s", err)
		}
		return data
	}
	tests := []struct {
		name string
		data []byte
		err  error
	}{
		{"empty stream", nil, io.ErrUnexpectedEOF},
		{"first chunk U",
			[]byte{0x02, 0x00, 0x00, 'x', 0x00}, errChunkType},
		{"first chunk L",
			[]byte{0x80, 0, 0, 0, 0, 0x00}, errChunkType},
		{"first chunk LR",
			[]byte{0xa0, 0, 0, 0, 0, 0x00}, errChunkType},
		{"UD then L", append(
			header(chunkHeader{ctype: cUD, uncompressed: 0}),
			0x78, 0x80, 0, 0, 0, 0, 0x00), errChunkType},
		{"UD then LR", append(
			header(chunkHeader{ctype: cUD, uncompressed: 0}),
			0x78, 0xa0, 0, 0, 0, 0, 0x00), errChunkType},
		{"invalid control byte",
			[]byte{0x03}, errHeaderByte},
		{"lc+lp exceeds 4", append(
			header(chunkHeader{ctype: cLRND, uncompressed: 0,
				compressed: 0,
				props:      Properties{LC: 3, LP: 2, PB: 0}}),
			0x00, 0x00), errPropsSum},
		{"invalid properties code",
			[]byte{0xe0, 0, 0, 0, 0, 0xff, 0x00}, nil},
		{"trailing data after EOS",
			[]byte{0x00, 0x01}, errTrailingData},
		{"truncated header",
			[]byte{0xe0, 0x00}, io.ErrUnexpectedEOF},
		{"truncated payload",
			[]byte{0x01, 0x00, 0x01, 'x'}, io.ErrUnexpectedEOF},
	}
	for _, tc := range tests {
		err := Inspect2(bytes.NewReader(tc.data), nil)
		if tc.err == nil {
			if err == nil {
				t.Errorf("%s: no error", tc.name)
			}
			continue
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: got error %v; want %v",
				tc.name, err, tc.err)
		}
	}
}

func TestInspector2Truncation(t *testing.T) {
	streams := map[string][]byte{
		"crafted": craftedStream(t),
	}
	var buf bytes.Buffer
	w, err := Writer2Config{DictCap: 4096}.NewWriter2(&buf)
	if err != nil {
		t.Fatalf("NewWriter2 error %s", err)
	}
	if _, err = w.Write([]byte("abcdef")); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	streams["writer2"] = buf.Bytes()

	for name, data := range streams {
		for i := 0; i < len(data); i++ {
			err := Inspect2(bytes.NewReader(data[:i]), nil)
			if err == nil {
				t.Fatalf("%s truncated to %d bytes: no error",
					name, i)
			}
		}
		if err := Inspect2(bytes.NewReader(data), nil); err != nil {
			t.Fatalf("%s complete stream: error %s", name, err)
		}
	}
}

func TestInspector2ControlByteMutations(t *testing.T) {
	data := craftedStream(t)
	recs, err := collectRecords(t, data)
	if err != nil {
		t.Fatalf("Inspect2 error %s", err)
	}

	mutate := func(off int64, b byte) []byte {
		m := make([]byte, len(data))
		copy(m, data)
		m[off] = b
		return m
	}

	// Sweep all control bytes with all possible values; the
	// inspector must never panic and always terminate.
	for _, rec := range recs {
		orig := data[rec.Offset]
		for b := 0; b < 256; b++ {
			if byte(b) == orig {
				continue
			}
			_ = Inspect2(bytes.NewReader(
				mutate(rec.Offset, byte(b))), nil)
		}
	}

	// Illegal reset sequences at the start of the stream.
	for _, b := range []byte{0x80, 0xa0, 0x02} {
		if err := Inspect2(bytes.NewReader(mutate(0, b)),
			nil); !errors.Is(err, errChunkType) {
			t.Errorf("control byte %#02x: got %v; want %v",
				b, err, errChunkType)
		}
	}
	if err := Inspect2(bytes.NewReader(mutate(0, 0x03)),
		nil); !errors.Is(err, errHeaderByte) {
		t.Errorf("control byte 0x03: got %v; want %v",
			err, errHeaderByte)
	}

	// Changing the uncompressed size bits of a compressed chunk
	// keeps the stream structurally valid; the inspector accepts it
	// but must not mark the chunk content as verified.
	m := mutate(0, data[0]|0x01)
	accepted, err := collectRecords(t, m)
	if err != nil {
		t.Fatalf("mutated uncompressed size bits: error %s", err)
	}
	for _, rec := range accepted {
		if rec.Verified {
			t.Errorf("record %s marked verified", rec)
		}
	}
}

func TestInspector2ConstantMemory(t *testing.T) {
	build := func(n int) []byte {
		var buf bytes.Buffer
		buf.Write([]byte{0x01, 0x00, 0x00, 'x'})
		for i := 1; i < n; i++ {
			buf.Write([]byte{0x02, 0x00, 0x00, 'x'})
		}
		buf.WriteByte(0x00)
		return buf.Bytes()
	}
	small, large := build(10), build(5000)
	// Warm up before measuring.
	if err := Inspect2(bytes.NewReader(small), nil); err != nil {
		t.Fatalf("Inspect2 error %s", err)
	}
	a1 := testing.AllocsPerRun(100, func() {
		if err := Inspect2(bytes.NewReader(small), nil); err != nil {
			t.Fatalf("Inspect2 error %s", err)
		}
	})
	a2 := testing.AllocsPerRun(100, func() {
		if err := Inspect2(bytes.NewReader(large), nil); err != nil {
			t.Fatalf("Inspect2 error %s", err)
		}
	})
	if a2 > a1 {
		t.Errorf("allocations grow with stream size: "+
			"10 chunks %.1f, 5000 chunks %.1f", a1, a2)
	}
}
