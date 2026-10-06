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

// runChecker2 runs the checker over p and returns all chunk records.
func runChecker2(p []byte, allowTrailing bool) (infos []ChunkInfo, trailing int64, err error) {
	c := Checker2Config{AllowTrailingData: allowTrailing}.
		NewChecker2(bytes.NewReader(p))
	for {
		info, err := c.Next()
		if err != nil {
			if err == io.EOF {
				return infos, c.Trailing(), nil
			}
			return infos, c.Trailing(), err
		}
		infos = append(infos, *info)
	}
}

// makeMultiChunkStream creates an LZMA2 stream with multiple
// uncompressed chunks using Writer2.
func makeMultiChunkStream(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := Writer2Config{DictCap: 4096}.NewWriter2(&buf)
	if err != nil {
		t.Fatalf("NewWriter2 error %s", err)
	}
	for i := 0; i < 5; i++ {
		if _, err = w.Write([]byte("abcdefgh")); err != nil {
			t.Fatalf("Write error %s", err)
		}
		if err = w.Flush(); err != nil {
			t.Fatalf("Flush error %s", err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	return buf.Bytes()
}

func TestChecker2Writer2Small(t *testing.T) {
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
	p := buf.Bytes()
	infos, trailing, err := runChecker2(p, false)
	if err != nil {
		t.Fatalf("checker error %s", err)
	}
	if trailing != 0 {
		t.Errorf("trailing %d; want %d", trailing, 0)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d chunks; want %d", len(infos), 2)
	}
	ud := infos[0]
	if ud.Offset != 0 || ud.Control != 0x01 || ud.Type != cUD {
		t.Errorf("got %s; want UD chunk at offset 0", &ud)
	}
	if ud.Uncompressed != 1 || ud.Compressed != 0 {
		t.Errorf("uncompressed %d compressed %d; want 1 and 0",
			ud.Uncompressed, ud.Compressed)
	}
	if !ud.DictReset || ud.PropsReset || ud.StateReset {
		t.Errorf("unexpected reset flags for %s", &ud)
	}
	if !ud.DictInit || ud.PropsInit || ud.StateInit {
		t.Errorf("unexpected init flags for %s", &ud)
	}
	if ud.StateBefore != start || ud.StateAfter != 'R' {
		t.Errorf("state transition %s->%s; want start->reset-required",
			ud.StateBefore, ud.StateAfter)
	}
	eos := infos[1]
	if eos.Offset != 4 || eos.Type != cEOS || eos.StateAfter != stop {
		t.Errorf("got %s; want EOS chunk at offset 4", &eos)
	}
}

func TestChecker2Writer2Large(t *testing.T) {
	const txtlen = 2100000
	txt := make([]byte, txtlen)
	if _, err := io.ReadFull(randtxt.NewReader(rand.NewSource(42)),
		txt); err != nil {
		t.Fatalf("randtxt read error %s", err)
	}
	var buf bytes.Buffer
	w, err := Writer2Config{DictCap: 4096}.NewWriter2(&buf)
	if err != nil {
		t.Fatalf("NewWriter2 error %s", err)
	}
	if _, err = w.Write(txt); err != nil {
		t.Fatalf("Write error %s", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close error %s", err)
	}
	p := buf.Bytes()
	infos, trailing, err := runChecker2(p, false)
	if err != nil {
		t.Fatalf("checker error %s", err)
	}
	if trailing != 0 {
		t.Errorf("trailing %d; want %d", trailing, 0)
	}
	if len(infos) < 3 {
		t.Fatalf("got %d chunks; want at least 3", len(infos))
	}
	var off, uncompressed int64
	for i := range infos {
		info := &infos[i]
		if info.Offset != off {
			t.Errorf("chunk %d offset %d; want %d",
				i, info.Offset, off)
		}
		off += info.Total()
		uncompressed += info.Uncompressed
		if info.Type != cEOS {
			if info.Uncompressed > maxUncompressed {
				t.Errorf("chunk %d uncompressed %d exceeds %d",
					i, info.Uncompressed, maxUncompressed)
			}
			if info.Compressed > maxCompressed {
				t.Errorf("chunk %d compressed %d exceeds %d",
					i, info.Compressed, maxCompressed)
			}
		}
	}
	if off != int64(len(p)) {
		t.Errorf("chunks cover %d bytes; stream has %d", off, len(p))
	}
	if uncompressed != txtlen {
		t.Errorf("uncompressed total %d; want %d",
			uncompressed, txtlen)
	}
	first := infos[0]
	if !first.DictReset {
		t.Errorf("first chunk %s doesn't reset the dictionary", &first)
	}
	last := infos[len(infos)-1]
	if last.Type != cEOS || last.StateAfter != stop {
		t.Errorf("last chunk %s; want EOS", &last)
	}
}

// allTypesStream contains one chunk of every type. The payload bytes
// are arbitrary, because the checker doesn't decompress them.
var allTypesStream = []byte{
	// LRND: 4 uncompressed, 3 compressed, props {LC:3, LP:0, PB:2}
	0xe0, 0x00, 0x03, 0x00, 0x02, 0x5d, 0xaa, 0xbb, 0xcc,
	// U: 2 uncompressed
	0x02, 0x00, 0x01, 0x01, 0x02,
	// LR: 1 uncompressed, 1 compressed
	0xa0, 0x00, 0x00, 0x00, 0x00, 0xff,
	// UD: 1 uncompressed
	0x01, 0x00, 0x00, 0x7f,
	// LRN: 1 uncompressed, 1 compressed, props {LC:0, LP:0, PB:0}
	0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
	// EOS
	0x00,
}

func TestChecker2AllChunkTypes(t *testing.T) {
	infos, trailing, err := runChecker2(allTypesStream, false)
	if err != nil {
		t.Fatalf("checker error %s", err)
	}
	if trailing != 0 {
		t.Errorf("trailing %d; want %d", trailing, 0)
	}
	want := []ChunkInfo{
		{Offset: 0, Control: 0xe0, Type: cLRND,
			Uncompressed: 4, Compressed: 3,
			Props:     Properties{LC: 3, LP: 0, PB: 2},
			DictReset: true, PropsReset: true, StateReset: true,
			DictInit: true, PropsInit: true, StateInit: true,
			StateBefore: start, StateAfter: 'L'},
		{Offset: 9, Control: 0x02, Type: cU,
			Uncompressed: 2,
			DictInit:     true, PropsInit: true, StateInit: true,
			StateBefore: 'L', StateAfter: 'U'},
		{Offset: 14, Control: 0xa0, Type: cLR,
			Uncompressed: 1, Compressed: 1,
			StateReset: true,
			DictInit:   true, PropsInit: true, StateInit: true,
			StateBefore: 'U', StateAfter: 'L'},
		{Offset: 20, Control: 0x01, Type: cUD,
			Uncompressed: 1,
			DictReset:    true,
			DictInit:     true, PropsInit: true, StateInit: true,
			StateBefore: 'L', StateAfter: 'R'},
		{Offset: 24, Control: 0xc0, Type: cLRN,
			Uncompressed: 1, Compressed: 1,
			Props:      Properties{LC: 0, LP: 0, PB: 0},
			PropsReset: true, StateReset: true,
			DictInit: true, PropsInit: true, StateInit: true,
			StateBefore: 'R', StateAfter: 'L'},
		{Offset: 31, Control: 0x00, Type: cEOS,
			DictInit: true, PropsInit: true, StateInit: true,
			StateBefore: 'L', StateAfter: stop},
	}
	if len(infos) != len(want) {
		t.Fatalf("got %d chunks; want %d", len(infos), len(want))
	}
	for i := range want {
		if infos[i] != want[i] {
			t.Errorf("chunk %d got %s; want %s",
				i, &infos[i], &want[i])
		}
	}
}

func TestChecker2Truncation(t *testing.T) {
	p := makeMultiChunkStream(t)
	if _, _, err := runChecker2(p, false); err != nil {
		t.Fatalf("checker error for full stream %s", err)
	}
	for i := 0; i < len(p); i++ {
		_, _, err := runChecker2(p[:i], false)
		if err == nil {
			t.Errorf("no error for stream truncated to %d bytes", i)
			continue
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("error %s for stream truncated to %d bytes;"+
				" want %s", err, i, io.ErrUnexpectedEOF)
		}
	}
}

func TestChecker2ControlBytes(t *testing.T) {
	p := makeMultiChunkStream(t)
	infos, _, err := runChecker2(p, false)
	if err != nil {
		t.Fatalf("checker error %s", err)
	}
	// An unsupported control byte must be detected at every chunk
	// boundary.
	for i := range infos {
		q := bytes.Clone(p)
		q[infos[i].Offset] = 0x03
		if _, _, err = runChecker2(q, false); !errors.Is(err, errHeaderByte) {
			t.Errorf("chunk %d: error %v; want %v",
				i, err, errHeaderByte)
		}
	}
	// Illegal reset sequences at the start of the stream.
	for _, b := range []byte{0x02, 0x80, 0xa0, 0xc0} {
		q := bytes.Clone(p)
		q[infos[0].Offset] = b
		if _, _, err = runChecker2(q, false); !errors.Is(err, errChunkType) {
			t.Errorf("control 0x%02x: error %v; want %v",
				b, err, errChunkType)
		}
	}
	// A compressed chunk without state reset must not follow an
	// uncompressed chunk that reset the dictionary.
	q := bytes.Clone(p)
	q[infos[1].Offset] = 0x80
	if _, _, err = runChecker2(q, false); !errors.Is(err, errChunkType) {
		t.Errorf("L chunk after UD: error %v; want %v",
			err, errChunkType)
	}
	// An early end-of-stream chunk leaves trailing data.
	q = bytes.Clone(p)
	q[infos[2].Offset] = 0x00
	if _, _, err = runChecker2(q, false); !errors.Is(err, errTrailingData) {
		t.Errorf("early EOS: error %v; want %v", err, errTrailingData)
	}
	got, trailing, err := runChecker2(q, true)
	if err != nil {
		t.Fatalf("checker with trailing data allowed error %s", err)
	}
	wantTrailing := int64(len(p)) - infos[2].Offset - 1
	if trailing != wantTrailing {
		t.Errorf("trailing %d; want %d", trailing, wantTrailing)
	}
	if len(got) != 3 {
		t.Errorf("got %d chunks before EOS; want %d", len(got), 3)
	}
}

func TestChecker2TrailingData(t *testing.T) {
	p := append(makeMultiChunkStream(t), 0x55)
	if _, _, err := runChecker2(p, false); !errors.Is(err, errTrailingData) {
		t.Fatalf("error %v; want %v", err, errTrailingData)
	}
	_, trailing, err := runChecker2(p, true)
	if err != nil {
		t.Fatalf("checker with trailing data allowed error %s", err)
	}
	if trailing != 1 {
		t.Errorf("trailing %d; want %d", trailing, 1)
	}
}

func TestChecker2InvalidProps(t *testing.T) {
	// The properties byte of the LRN chunk sits at offset 29.
	q := bytes.Clone(allTypesStream)
	q[29] = maxPropertyCode + 1
	if _, _, err := runChecker2(q, false); err == nil {
		t.Fatal("no error for invalid properties code")
	}
	// Properties {LC:4, LP:1, PB:0} violate the LZMA2 condition
	// lc+lp <= 4.
	q = bytes.Clone(allTypesStream)
	q[29] = Properties{LC: 4, LP: 1, PB: 0}.Code()
	if _, _, err := runChecker2(q, false); err == nil {
		t.Fatal("no error for lc+lp > 4")
	}
}

func TestChecker2PayloadNotVerified(t *testing.T) {
	// The checker is a structural checker: corrupting payload bytes
	// must not change its result, because it doesn't decompress the
	// payload. Use Reader2 to verify the payload content.
	q := bytes.Clone(allTypesStream)
	q[6] ^= 0xff  // payload of the LRND chunk
	q[12] ^= 0xff // payload of the U chunk
	if _, _, err := runChecker2(q, false); err != nil {
		t.Fatalf("checker error for corrupted payload %s", err)
	}
}

// udGenerator generates a stream of uncompressed chunks on the fly.
type udGenerator struct {
	remaining int
	phase     int
	eos       bool
}

func (g *udGenerator) Read(p []byte) (n int, err error) {
	for n < len(p) {
		if g.remaining > 0 {
			var b byte
			switch g.phase {
			case 0:
				b = 0x01
			case 3:
				b = 'x'
			}
			p[n] = b
			n++
			g.phase++
			if g.phase == 4 {
				g.phase = 0
				g.remaining--
			}
			continue
		}
		if !g.eos {
			p[n] = 0x00
			n++
			g.eos = true
			continue
		}
		return n, io.EOF
	}
	return n, nil
}

func TestChecker2Streaming(t *testing.T) {
	const chunks = 1 << 20
	c := NewChecker2(&udGenerator{remaining: chunks})
	var count int
	var last *ChunkInfo
	for {
		info, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("checker error %s", err)
		}
		count++
		last = info
	}
	if count != chunks+1 {
		t.Errorf("got %d chunks; want %d", count, chunks+1)
	}
	if last == nil || last.Type != cEOS ||
		last.Offset != int64(4*chunks) {
		t.Errorf("last chunk %v; want EOS at offset %d",
			last, 4*chunks)
	}
	if c.Trailing() != 0 {
		t.Errorf("trailing %d; want %d", c.Trailing(), 0)
	}
}

func TestChecker2MultiChunkStream(t *testing.T) {
	p := makeMultiChunkStream(t)
	infos, _, err := runChecker2(p, false)
	if err != nil {
		t.Fatalf("checker error %s", err)
	}
	// The stream consists of one UD chunk, four U chunks and EOS.
	wantTypes := []chunkType{cUD, cU, cU, cU, cU, cEOS}
	if len(infos) != len(wantTypes) {
		t.Fatalf("got %d chunks; want %d", len(infos), len(wantTypes))
	}
	var off int64
	for i, info := range infos {
		if info.Type != wantTypes[i] {
			t.Errorf("chunk %d type %s; want %s",
				i, info.Type, wantTypes[i])
		}
		if info.Offset != off {
			t.Errorf("chunk %d offset %d; want %d", i, info.Offset, off)
		}
		off += info.Total()
	}
	if off != int64(len(p)) {
		t.Errorf("chunks cover %d bytes; stream has %d", off, len(p))
	}
}
