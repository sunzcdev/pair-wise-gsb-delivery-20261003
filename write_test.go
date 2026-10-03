package ber

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"runtime"
	"testing"
)

// writeMatchesBytes builds a representative packet and verifies WriteTo,
// AppendTo, and Bytes all produce identical output.
func writeMatchesBytes(t *testing.T, p *Packet) {
	t.Helper()

	want := p.Bytes()

	var buf bytes.Buffer
	n, err := p.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: unexpected error: %v", err)
	}
	if n != int64(len(want)) {
		t.Fatalf("WriteTo returned %d bytes, want %d", n, len(want))
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("WriteTo output mismatch:\n got: %x\nwant: %x", buf.Bytes(), want)
	}

	prefix := []byte("PRE")
	got, err := p.AppendTo(prefix)
	if err != nil {
		t.Fatalf("AppendTo: unexpected error: %v", err)
	}
	if !bytes.Equal(got[:len(prefix)], prefix) {
		t.Fatalf("AppendTo did not preserve the existing prefix")
	}
	if !bytes.Equal(got[len(prefix):], want) {
		t.Fatalf("AppendTo output mismatch:\n got: %x\nwant: %x", got[len(prefix):], want)
	}
}

func TestWriteToPrimitives(t *testing.T) {
	longValue := bytes.Repeat([]byte{0xab}, 200) // forces long-form length
	leaf := NewString(ClassUniversal, TypePrimitive, TagOctetString, string(longValue), "")
	writeMatchesBytes(t, leaf)
}

func TestWriteToHighTag(t *testing.T) {
	// Tag 0x1f4 -> high-tag identifier 1f 83 74.
	p := NewInteger(ClassContext, TypePrimitive, Tag(0x1f4), 1, "")
	writeMatchesBytes(t, p)

	seq := NewSequence("root")
	seq.AppendChild(p)
	writeMatchesBytes(t, seq)
}

func TestWriteToDeepChain(t *testing.T) {
	const depth = 500

	root := NewSequence("root")
	cur := root
	for i := 0; i < depth; i++ {
		child := NewSequence("nested")
		cur.AppendChild(child)
		cur = child
	}
	cur.AppendChild(NewString(ClassUniversal, TypePrimitive, TagOctetString, "leaf", ""))

	writeMatchesBytes(t, root)

	// The streamed output must itself decode and re-encode identically.
	var buf bytes.Buffer
	if _, err := root.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	decoded, err := DecodePacketErr(buf.Bytes())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !bytes.Equal(decoded.Bytes(), buf.Bytes()) {
		t.Fatalf("round-trip mismatch")
	}
}

func TestWriteToWideTree(t *testing.T) {
	root := NewSequence("root")
	for i := 0; i < 200; i++ {
		child := NewSequence("child")
		child.AppendChild(NewInteger(ClassUniversal, TypePrimitive, TagInteger, int64(i), ""))
		child.AppendChild(NewString(ClassUniversal, TypePrimitive, TagOctetString, "sibling", ""))
		root.AppendChild(child)
	}
	writeMatchesBytes(t, root)
}

func TestWriteToSharedSubtree(t *testing.T) {
	// The same child packet is shared by two parents. It must be encoded for
	// each occurrence, exactly like Bytes() does.
	shared := NewString(ClassUniversal, TypePrimitive, TagOctetString, "shared", "")

	left := NewSequence("left")
	left.AppendChild(shared)
	right := NewSequence("right")
	right.AppendChild(shared)

	root := NewSequence("root")
	root.AppendChild(left)
	root.AppendChild(right)

	writeMatchesBytes(t, root)

	var buf bytes.Buffer
	if _, err := root.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	want := root.Bytes()
	if bytes.Count(buf.Bytes(), []byte("shared")) != 2 {
		t.Fatalf("shared subtree encoded %d times, want 2", bytes.Count(buf.Bytes(), []byte("shared")))
	}
	if len(buf.Bytes()) != len(want) {
		t.Fatalf("encoded length %d, want %d", len(buf.Bytes()), len(want))
	}
}

func TestWriteToEmptyPacket(t *testing.T) {
	// A packet with no children and no Data.
	p := &Packet{}
	var buf bytes.Buffer
	n, err := p.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo on zero-value packet: %v", err)
	}
	if want := []byte{0x00, 0x00}; !bytes.Equal(buf.Bytes(), want) || n != 2 {
		t.Fatalf("zero-value packet encoded as %x (%d bytes), want %x", buf.Bytes(), n, want)
	}
	out, err := p.AppendTo(nil)
	if err != nil || !bytes.Equal(out, []byte{0x00, 0x00}) {
		t.Fatalf("AppendTo on zero-value packet = (%x, %v)", out, err)
	}

	// Constructed with no children: Bytes() treats it via the children path.
	empty := Encode(ClassUniversal, TypeConstructed, TagSequence, nil, "")
	writeMatchesBytes(t, empty)
}

func TestWriteToAppendToReusesCapacity(t *testing.T) {
	p := NewString(ClassUniversal, TypePrimitive, TagOctetString, "abc", "")

	buf := make([]byte, 1, 64)
	buf[0] = 'X'

	out, err := p.AppendTo(buf)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if !bytes.HasPrefix(out, []byte{'X'}) {
		t.Fatalf("prefix lost: %x", out)
	}

	// A second append onto a buffer that already has room must not grow it.
	before := cap(out)
	out2, err := p.AppendTo(out)
	if err != nil {
		t.Fatalf("second AppendTo: %v", err)
	}
	if cap(out2) != before {
		t.Fatalf("AppendTo grew the slice from cap %d to %d despite free capacity", before, cap(out2))
	}
	if !bytes.Equal(out2[len(out):], p.Bytes()) {
		t.Fatalf("second append mismatch")
	}
}

func TestWriteToNilPacket(t *testing.T) {
	var p *Packet
	if _, err := p.WriteTo(io.Discard); !errors.Is(err, ErrNilPacket) {
		t.Fatalf("nil receiver WriteTo error = %v, want ErrNilPacket", err)
	}
	if out, err := p.AppendTo(nil); !errors.Is(err, ErrNilPacket) || out != nil {
		t.Fatalf("nil receiver AppendTo = (%v, %v), want (nil, ErrNilPacket)", out, err)
	}
}

func TestWriteToNilChild(t *testing.T) {
	root := NewSequence("root")
	root.AppendChild(NewString(ClassUniversal, TypePrimitive, TagOctetString, "ok", ""))
	root.AppendChild(nil)

	if _, err := root.WriteTo(io.Discard); !errors.Is(err, ErrNilPacket) {
		t.Fatalf("WriteTo error = %v, want ErrNilPacket", err)
	}
	if _, err := root.AppendTo(nil); !errors.Is(err, ErrNilPacket) {
		t.Fatalf("AppendTo error = %v, want ErrNilPacket", err)
	}

	// A nil child nested deeper than a valid sibling is also rejected.
	inner := NewSequence("inner")
	inner.AppendChild(nil)
	outer := NewSequence("outer")
	outer.AppendChild(NewString(ClassUniversal, TypePrimitive, TagOctetString, "ok", ""))
	outer.AppendChild(inner)
	if _, err := outer.WriteTo(io.Discard); !errors.Is(err, ErrNilPacket) {
		t.Fatalf("nested nil child error = %v, want ErrNilPacket", err)
	}
}

func TestWriteToCycle(t *testing.T) {
	// Direct self-reference.
	self := NewSequence("self")
	self.AppendChild(self)
	if _, err := self.WriteTo(io.Discard); !errors.Is(err, ErrPacketCycle) {
		t.Fatalf("self cycle error = %v, want ErrPacketCycle", err)
	}

	// Indirect cycle through a sibling; sharing alone (same node reached via
	// two paths) must still be allowed.
	root := NewSequence("root")
	a := NewSequence("a")
	b := NewSequence("b")
	root.AppendChild(a)
	shared := NewString(ClassUniversal, TypePrimitive, TagOctetString, "shared", "")
	a.AppendChild(shared)
	a.AppendChild(b)
	b.AppendChild(a) // a -> ... -> b -> a
	if _, err := root.WriteTo(io.Discard); !errors.Is(err, ErrPacketCycle) {
		t.Fatalf("indirect cycle error = %v, want ErrPacketCycle", err)
	}

	// Pure sharing (no ancestor edge) is legal: two parents, one child.
	dag := NewSequence("dag")
	left := NewSequence("left")
	right := NewSequence("right")
	leaf := NewString(ClassUniversal, TypePrimitive, TagOctetString, "shared", "")
	left.AppendChild(leaf)
	right.AppendChild(leaf)
	dag.AppendChild(left)
	dag.AppendChild(right)
	writeMatchesBytes(t, dag)
}

// limitedWriter accepts at most max bytes total; every Write call is a
// full-or-short write with no error of its own.
type limitedWriter struct {
	buf bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.max - w.buf.Len()
	if remaining <= 0 {
		return 0, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}

func TestWriteToShortWrite(t *testing.T) {
	root := NewSequence("root")
	root.AppendChild(NewString(ClassUniversal, TypePrimitive, TagOctetString, "first", ""))
	root.AppendChild(NewString(ClassUniversal, TypePrimitive, TagOctetString, "second", ""))

	// Budget only enough for the identifier, length, and first child's bytes.
	firstChild := root.Children[0].Bytes()
	budget := 1 + 1 + len(firstChild)

	w := &limitedWriter{max: budget}
	n, err := root.WriteTo(w)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want io.ErrShortWrite", err)
	}
	if n != int64(budget) {
		t.Fatalf("wrote %d bytes, want %d", n, budget)
	}

	// The second sibling must not have been partially written.
	if bytes.Contains(w.buf.Bytes(), []byte("second")) {
		t.Fatalf("writer continued into a sibling after a short write")
	}
	if !bytes.HasSuffix(w.buf.Bytes(), firstChild) {
		t.Fatalf("output did not end with the fully-written first child")
	}
}

// errorWriter always fails; WriteTo must report the error and never claim
// bytes it did not write.
type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("boom")
}

func TestWriteToPropagatesWriterError(t *testing.T) {
	p := NewString(ClassUniversal, TypePrimitive, TagOctetString, "abc", "")
	n, err := p.WriteTo(errorWriter{})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("error = %v, want boom", err)
	}
	if n != 0 {
		t.Fatalf("wrote %d bytes after a hard error, want 0", n)
	}
}

func TestWriteToAllocationsDoNotScaleWithDepth(t *testing.T) {
	const depth = 500
	const leafSize = 256 * 1024

	build := func() *Packet {
		root := NewSequence("root")
		cur := root
		for i := 0; i < depth; i++ {
			child := NewSequence("nested")
			cur.AppendChild(child)
			cur = child
		}
		cur.AppendChild(NewString(
			ClassUniversal, TypePrimitive, TagOctetString,
			string(bytes.Repeat([]byte{'x'}, leafSize)), "",
		))
		return root
	}

	root := build()
	encoded := root.Bytes()
	total := len(encoded)

	var ms0, ms1, ms2 runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&ms0)
	root.Bytes()
	runtime.ReadMemStats(&ms1)

	if _, err := root.WriteTo(io.Discard); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	runtime.ReadMemStats(&ms2)

	bytesAllocs := ms1.TotalAlloc - ms0.TotalAlloc
	streamAllocs := ms2.TotalAlloc - ms1.TotalAlloc

	t.Logf("depth=%d leaf=%dB total=%dB Bytes-allocs=%d WriteTo-allocs=%d",
		depth, leafSize, total, bytesAllocs, streamAllocs)

	// Bytes() concatenates the subtree at each level: leaf body copied once
	// per depth, i.e. well over a single body's worth. WriteTo streams it once.
	if bytesAllocs < uint64(depth/4)*uint64(leafSize) {
		t.Fatalf("sanity check failed: Bytes() only allocated %d bytes", bytesAllocs)
	}
	if streamAllocs >= 2*uint64(total) {
		t.Fatalf("WriteTo allocated %d bytes for a %d-byte deep tree; want well below depth x leaf",
			streamAllocs, total)
	}
	if streamAllocs >= bytesAllocs/2 {
		t.Fatalf("WriteTo allocated %d bytes vs Bytes() %d; expected no per-depth body copies",
			streamAllocs, bytesAllocs)
	}
}

func TestAddIntOverflow(t *testing.T) {
	maxInt := int(math.MaxInt)
	if _, err := addInt(maxInt-2, 3); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("addInt overflow error = %v, want ErrPacketTooLarge", err)
	}
	if _, err := addPacketLengths(1, maxInt-1, 1); !errors.Is(err, ErrPacketTooLarge) {
		t.Fatalf("addPacketLengths overflow error = %v, want ErrPacketTooLarge", err)
	}
	if got, err := addInt(1<<20, 1<<20); err != nil || got != 2<<20 {
		t.Fatalf("addInt valid sum = (%d, %v)", got, err)
	}
}

func TestLenOctets(t *testing.T) {
	cases := []struct {
		length int
		want   int
	}{
		{0, 1},
		{127, 1},
		{128, 2},
		{255, 2},
		{256, 3},
		{1 << 30, 5},
	}
	for _, c := range cases {
		if got := lenOctets(c.length); got != c.want {
			t.Errorf("lenOctets(%d) = %d, want %d", c.length, got, c.want)
		}
		if got := len(encodeLength(c.length)); got != c.want {
			t.Errorf("len(encodeLength(%d)) = %d, want %d", c.length, got, c.want)
		}
	}
}

// TestWriteToSuiteFiles re-encodes every valid corpus file and confirms
// WriteTo and AppendTo match Bytes on real-world decoded trees.
func TestWriteToSuiteFiles(t *testing.T) {
	for _, tc := range testCases {
		if tc.Error != "" {
			continue
		}

		data, err := os.ReadFile(tc.File)
		if err != nil {
			t.Errorf("%s: %v", tc.File, err)
			continue
		}
		p, err := DecodePacketErr(data)
		if err != nil {
			t.Errorf("%s: unexpected decode error: %v", tc.File, err)
			continue
		}

		want := p.Bytes()

		var buf bytes.Buffer
		if _, err := p.WriteTo(&buf); err != nil {
			t.Errorf("%s: WriteTo: %v", tc.File, err)
			continue
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("%s: WriteTo differs from Bytes", tc.File)
		}

		out, err := p.AppendTo(make([]byte, 0, len(want)))
		if err != nil {
			t.Errorf("%s: AppendTo: %v", tc.File, err)
			continue
		}
		if !bytes.Equal(out, want) {
			t.Errorf("%s: AppendTo differs from Bytes", tc.File)
		}
	}
}
