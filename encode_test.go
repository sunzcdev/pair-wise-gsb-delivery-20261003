package ber

import (
	"bytes"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"
)

// buildChain returns a constructed packet nested depth levels deep, with a
// single OCTET STRING leaf holding payload at the bottom.
func buildChain(depth int, payload []byte) *Packet {
	leaf := Encode(ClassUniversal, TypePrimitive, TagOctetString, nil, "leaf")
	leaf.Data.Write(payload)

	p := leaf
	for i := 0; i < depth; i++ {
		parent := Encode(ClassContext, TypeConstructed, Tag(i%30), nil, "level")
		parent.AppendChild(p)
		p = parent
	}
	return p
}

// buildWideTree returns a root with fanout constructed children, each holding
// leavesPerChild primitive children.
func buildWideTree(fanout, leavesPerChild int) *Packet {
	root := NewSequence("root")
	for i := 0; i < fanout; i++ {
		child := Encode(ClassContext, TypeConstructed, Tag(i%30), nil, "branch")
		for j := 0; j < leavesPerChild; j++ {
			leaf := NewInteger(ClassUniversal, TypePrimitive, TagInteger, int64(j), "leaf")
			child.AppendChild(leaf)
		}
		root.AppendChild(child)
	}
	return root
}

func checkWriteToMatchesBytes(t *testing.T, p *Packet) {
	t.Helper()

	want := p.Bytes()

	var buf bytes.Buffer
	n, err := p.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: unexpected error: %v", err)
	}
	if n != int64(len(want)) {
		t.Errorf("WriteTo: wrote %d bytes, want %d", n, len(want))
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("WriteTo: output differs from Bytes()\nwant: %#v\ngot:  %#v", want, buf.Bytes())
	}

	appended, err := p.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: unexpected error: %v", err)
	}
	if !bytes.Equal(appended, want) {
		t.Errorf("AppendTo: output differs from Bytes()\nwant: %#v\ngot:  %#v", want, appended)
	}
}

func TestWriteToDeepChain(t *testing.T) {
	checkWriteToMatchesBytes(t, buildChain(2000, []byte("payload at the bottom")))
}

func TestWriteToWideTree(t *testing.T) {
	checkWriteToMatchesBytes(t, buildWideTree(500, 10))
}

func TestWriteToFixtures(t *testing.T) {
	// Every successfully-decoded suite fixture must re-encode identically
	// through WriteTo/AppendTo and Bytes.
	for _, tc := range testCases {
		if tc.Error != "" {
			continue
		}
		dataIn, err := os.ReadFile(tc.File)
		if err != nil {
			t.Errorf("%s: %v", tc.File, err)
			continue
		}
		packet, err := DecodePacketErr(dataIn)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.File, err)
			continue
		}
		t.Run(tc.File, func(t *testing.T) {
			checkWriteToMatchesBytes(t, packet)
		})
	}
}

func TestAppendToPreservesPrefix(t *testing.T) {
	p := buildWideTree(3, 2)
	prefix := []byte("existing prefix")

	out, err := p.AppendTo(append([]byte(nil), prefix...))
	if err != nil {
		t.Fatalf("AppendTo: unexpected error: %v", err)
	}
	if !bytes.Equal(out[:len(prefix)], prefix) {
		t.Errorf("AppendTo: prefix modified: %q", out[:len(prefix)])
	}
	if !bytes.Equal(out[len(prefix):], p.Bytes()) {
		t.Errorf("AppendTo: appended bytes differ from Bytes()")
	}
}

func TestWriteToSharedSubtree(t *testing.T) {
	// A subtree shared between two parents is legal and must be encoded once
	// per occurrence.
	shared := NewSequence("shared")
	shared.AppendChild(NewInteger(ClassUniversal, TypePrimitive, TagInteger, int64(42), "n"))

	root := NewSequence("root")
	root.AppendChild(shared)
	root.AppendChild(shared)

	checkWriteToMatchesBytes(t, root)

	decoded, err := DecodePacketErr(root.Bytes())
	if err != nil {
		t.Fatalf("DecodePacketErr: unexpected error: %v", err)
	}
	if len(decoded.Children) != 2 {
		t.Fatalf("expected 2 children after decode, got %d", len(decoded.Children))
	}
}

func TestWriteToCycle(t *testing.T) {
	direct := NewSequence("direct")
	direct.AppendChild(direct)
	if _, err := direct.WriteTo(io.Discard); err == nil {
		t.Error("WriteTo: expected cycle error, got none")
	}
	if _, err := direct.AppendTo(nil); err == nil {
		t.Error("AppendTo: expected cycle error, got none")
	}

	a := NewSequence("a")
	b := NewSequence("b")
	a.AppendChild(b)
	b.AppendChild(a)
	if _, err := a.WriteTo(io.Discard); err == nil {
		t.Error("WriteTo: expected indirect cycle error, got none")
	}
}

func TestWriteToNilInputs(t *testing.T) {
	var nilPacket *Packet
	if _, err := nilPacket.WriteTo(io.Discard); err == nil {
		t.Error("WriteTo: expected error for nil packet, got none")
	}
	if _, err := nilPacket.AppendTo(nil); err == nil {
		t.Error("AppendTo: expected error for nil packet, got none")
	}

	nilChild := NewSequence("nil child")
	nilChild.Children = append(nilChild.Children, nil)
	if _, err := nilChild.WriteTo(io.Discard); err == nil {
		t.Error("WriteTo: expected error for nil child, got none")
	}

	nilData := &Packet{}
	if _, err := nilData.WriteTo(io.Discard); err == nil {
		t.Error("WriteTo: expected error for nil Data, got none")
	}
}

// shortWriter accepts at most limit bytes in total, then reports a short
// write. It records every byte it was asked to accept.
type shortWriter struct {
	limit   int
	written int
	asked   int
}

func (sw *shortWriter) Write(p []byte) (int, error) {
	sw.asked += len(p)
	remaining := sw.limit - sw.written
	if remaining >= len(p) {
		sw.written += len(p)
		return len(p), nil
	}
	if remaining < 0 {
		remaining = 0
	}
	sw.written += remaining
	return remaining, nil
}

func TestWriteToShortWrite(t *testing.T) {
	p := buildWideTree(10, 5)
	full := p.Bytes()

	const limit = 17
	sw := &shortWriter{limit: limit}
	n, err := p.WriteTo(sw)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteTo: expected io.ErrShortWrite, got %v", err)
	}
	if n != limit {
		t.Errorf("WriteTo: wrote %d bytes, want %d", n, limit)
	}
	if sw.written != limit {
		t.Errorf("WriteTo: underlying writer accepted %d bytes, want %d", sw.written, limit)
	}
	// Once the short write happened, encoding must stop instead of carrying
	// on with the remaining siblings.
	if sw.asked >= len(full) {
		t.Errorf("WriteTo: kept writing after short write (asked %d of %d bytes)", sw.asked, len(full))
	}
}

// failWriter accepts limit bytes and then fails with err.
type failWriter struct {
	limit   int
	written int
	err     error
}

func (fw *failWriter) Write(p []byte) (int, error) {
	remaining := fw.limit - fw.written
	if remaining >= len(p) {
		fw.written += len(p)
		return len(p), nil
	}
	if remaining < 0 {
		remaining = 0
	}
	fw.written += remaining
	return remaining, fw.err
}

func TestWriteToUnderlyingError(t *testing.T) {
	p := buildWideTree(10, 5)

	errBoom := errors.New("boom")
	fw := &failWriter{limit: 9, err: errBoom}
	n, err := p.WriteTo(fw)
	if !errors.Is(err, errBoom) {
		t.Fatalf("WriteTo: expected %v, got %v", errBoom, err)
	}
	if n != 9 {
		t.Errorf("WriteTo: wrote %d bytes, want 9", n)
	}
}

func TestWriteToErrorWritesNothing(t *testing.T) {
	p := buildChain(3, []byte("x"))
	p.AppendChild(nil)

	var buf bytes.Buffer
	n, err := p.WriteTo(&buf)
	if err == nil {
		t.Fatal("WriteTo: expected error for nil child, got none")
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("WriteTo: validation failure wrote %d bytes, want 0", n)
	}
}

// TestWriteToAllocationDepth checks that encoding a deep chain does not copy
// the leaf content per level of nesting: total bytes allocated by WriteTo and
// AppendTo must stay proportional to the encoded size, not to
// depth x subtree size (which is what Bytes costs).
func TestWriteToAllocationDepth(t *testing.T) {
	const depth = 500
	payload := make([]byte, 64<<10)
	chain := buildChain(depth, payload)
	encoded := chain.Bytes()

	var m1, m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)
	if _, err := chain.WriteTo(io.Discard); err != nil {
		t.Fatalf("WriteTo: unexpected error: %v", err)
	}
	runtime.ReadMemStats(&m2)
	writeToAlloc := m2.TotalAlloc - m1.TotalAlloc

	runtime.GC()
	runtime.ReadMemStats(&m1)
	if _, err := chain.AppendTo(nil); err != nil {
		t.Fatalf("AppendTo: unexpected error: %v", err)
	}
	runtime.ReadMemStats(&m2)
	appendToAlloc := m2.TotalAlloc - m1.TotalAlloc

	// Bytes on this tree allocates ~depth/2 copies of the payload (~16 MB).
	// A streaming encoder must stay within a small multiple of the output.
	bound := uint64(4 * len(encoded))
	if writeToAlloc > bound {
		t.Errorf("WriteTo allocated %d bytes for %d-byte encoding (bound %d); content copied per depth?",
			writeToAlloc, len(encoded), bound)
	}
	if appendToAlloc > bound {
		t.Errorf("AppendTo allocated %d bytes for %d-byte encoding (bound %d); content copied per depth?",
			appendToAlloc, len(encoded), bound)
	}
}
