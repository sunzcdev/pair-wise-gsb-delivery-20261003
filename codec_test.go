package bitset

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"
	"testing"
)

func testBitSet() *BitSet {
	b := New(130)
	for _, i := range []uint{0, 1, 5, 63, 64, 65, 100, 129} {
		b.Set(i)
	}
	return b
}

func equalBitSets(a, b *BitSet) bool {
	if a.Len() != b.Len() || a.Count() != b.Count() {
		return false
	}
	for i, ok := a.NextSet(0); ok; i, ok = a.NextSet(i + 1) {
		if !b.Test(i) {
			return false
		}
	}
	return true
}

func TestCodecRoundTripStream(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		c := NewCodec(order, 0)
		src := testBitSet()
		var buf bytes.Buffer
		n, err := c.Encode(&buf, src)
		if err != nil {
			t.Fatalf("Encode failed: %v", err)
		}
		if n != int64(src.BinaryStorageSize()) {
			t.Fatalf("Encode returned %d bytes, want %d", n, src.BinaryStorageSize())
		}
		var dst BitSet
		m, err := c.Decode(&buf, &dst)
		if err != nil {
			t.Fatalf("Decode failed: %v", err)
		}
		if m != n {
			t.Fatalf("Decode read %d bytes, wrote %d", m, n)
		}
		if !equalBitSets(src, &dst) {
			t.Fatal("round trip mismatch")
		}
	}
}

func TestCodecRoundTripSlice(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		c := NewCodec(order, 0)
		src := testBitSet()
		data := c.Marshal(src)
		if len(data) != src.BinaryStorageSize() {
			t.Fatalf("Marshal produced %d bytes, want %d", len(data), src.BinaryStorageSize())
		}
		var dst BitSet
		n, err := c.Unmarshal(data, &dst)
		if err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if n != len(data) {
			t.Fatalf("Unmarshal consumed %d bytes, want %d", n, len(data))
		}
		if !equalBitSets(src, &dst) {
			t.Fatal("round trip mismatch")
		}
	}
}

func TestCodecCompatibleWithGlobalAPI(t *testing.T) {
	// The default global order is big-endian; a big-endian Codec must
	// produce and consume the exact same wire format.
	src := testBitSet()
	c := NewCodec(binary.BigEndian, 0)

	global, err := src.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary failed: %v", err)
	}
	if !bytes.Equal(c.Marshal(src), global) {
		t.Fatal("Codec.Marshal differs from MarshalBinary")
	}

	var dst BitSet
	if _, err := c.Unmarshal(global, &dst); err != nil {
		t.Fatalf("Codec.Unmarshal of MarshalBinary output failed: %v", err)
	}
	if !equalBitSets(src, &dst) {
		t.Fatal("Codec.Unmarshal of MarshalBinary output mismatch")
	}

	var buf bytes.Buffer
	if _, err := c.Encode(&buf, src); err != nil {
		t.Fatalf("Codec.Encode failed: %v", err)
	}
	var dst2 BitSet
	if _, err := dst2.ReadFrom(&buf); err != nil {
		t.Fatalf("BitSet.Decode of Codec.WriteTo output failed: %v", err)
	}
	if !equalBitSets(src, &dst2) {
		t.Fatal("BitSet.Decode of Codec.WriteTo output mismatch")
	}
}

func TestCodecTruncatedInputLeavesTargetUnchanged(t *testing.T) {
	c := NewCodec(binary.BigEndian, 0)
	src := testBitSet()
	full := c.Marshal(src)

	for _, cut := range []int{0, 3, wordBytes - 1, wordBytes, wordBytes + 5, len(full) - 1} {
		dst := New(20).Set(3).Set(7)
		before := dst.Clone()
		if _, err := c.Unmarshal(full[:cut], dst); err == nil {
			t.Fatalf("Unmarshal of %d truncated bytes succeeded", cut)
		}
		if !equalBitSets(before, dst) {
			t.Fatalf("Unmarshal of %d truncated bytes modified the target", cut)
		}

		dst2 := New(20).Set(3).Set(7)
		before2 := dst2.Clone()
		if _, err := c.Decode(bytes.NewReader(full[:cut]), dst2); err == nil {
			t.Fatalf("Decode of %d truncated bytes succeeded", cut)
		}
		if !equalBitSets(before2, dst2) {
			t.Fatalf("Decode of %d truncated bytes modified the target", cut)
		}
	}
}

func TestCodecMaxBitsLimit(t *testing.T) {
	c := NewCodec(binary.BigEndian, 64)
	src := testBitSet() // length 130
	full := c.Marshal(src)

	dst := New(10).Set(2)
	before := dst.Clone()
	if _, err := c.Unmarshal(full, dst); err == nil {
		t.Fatal("Unmarshal accepted length above the codec limit")
	}
	if !equalBitSets(before, dst) {
		t.Fatal("Unmarshal over the limit modified the target")
	}

	// The limit is enforced before the payload is read from a stream.
	r := bytes.NewReader(full)
	dst2 := New(10).Set(2)
	before2 := dst2.Clone()
	if _, err := c.Decode(r, dst2); err == nil {
		t.Fatal("Decode accepted length above the codec limit")
	}
	if !equalBitSets(before2, dst2) {
		t.Fatal("Decode over the limit modified the target")
	}
	if r.Len() != len(full)-wordBytes {
		t.Fatalf("Decode consumed payload bytes before rejecting: %d left, want %d",
			r.Len(), len(full)-wordBytes)
	}

	// A bitset within the limit still decodes.
	small := New(64).Set(0).Set(63)
	var ok BitSet
	if _, err := c.Unmarshal(c.Marshal(small), &ok); err != nil {
		t.Fatalf("Unmarshal within the limit failed: %v", err)
	}
	if !equalBitSets(small, &ok) {
		t.Fatal("round trip within the limit mismatch")
	}
}

func TestCodecDirtyTailBitsFail(t *testing.T) {
	c := NewCodec(binary.BigEndian, 0)
	// length 65, two words; set a bit beyond the declared length.
	data := make([]byte, wordBytes+2*wordBytes)
	binary.BigEndian.PutUint64(data, 65)
	data[wordBytes] = 1        // bit 0 set, fine
	data[len(data)-1] = 1 << 7 // bit 127 set, beyond length 65

	dst := New(8).Set(1)
	before := dst.Clone()
	if _, err := c.Unmarshal(data, dst); err == nil {
		t.Fatal("Unmarshal accepted dirty tail bits")
	}
	if !equalBitSets(before, dst) {
		t.Fatal("Unmarshal with dirty tail bits modified the target")
	}

	dst2 := New(8).Set(1)
	before2 := dst2.Clone()
	if _, err := c.Decode(bytes.NewReader(data), dst2); err == nil {
		t.Fatal("Decode accepted dirty tail bits")
	}
	if !equalBitSets(before2, dst2) {
		t.Fatal("Decode with dirty tail bits modified the target")
	}
}

func TestCodecConcatenatedStream(t *testing.T) {
	c := NewCodec(binary.LittleEndian, 0)
	first := New(70).Set(1).Set(69)
	second := New(10).Set(9)

	var buf bytes.Buffer
	if _, err := c.Encode(&buf, first); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if _, err := c.Encode(&buf, second); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	var gotFirst BitSet
	if _, err := c.Decode(&buf, &gotFirst); err != nil {
		t.Fatalf("Decode of first bitset failed: %v", err)
	}
	if !equalBitSets(first, &gotFirst) {
		t.Fatal("first bitset mismatch")
	}
	// The second bitset must still be intact in the stream.
	var gotSecond BitSet
	if _, err := c.Decode(&buf, &gotSecond); err != nil {
		t.Fatalf("Decode of second bitset failed: %v", err)
	}
	if !equalBitSets(second, &gotSecond) {
		t.Fatal("second bitset mismatch")
	}
	if buf.Len() != 0 {
		t.Fatalf("%d unexpected bytes left in stream", buf.Len())
	}
}

func TestCodecUnmarshalLeavesTrailingBytes(t *testing.T) {
	c := NewCodec(binary.BigEndian, 0)
	first := New(70).Set(1).Set(69)
	second := New(10).Set(9)
	data := append(c.Marshal(first), c.Marshal(second)...)

	var gotFirst BitSet
	n, err := c.Unmarshal(data, &gotFirst)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !equalBitSets(first, &gotFirst) {
		t.Fatal("first bitset mismatch")
	}
	var gotSecond BitSet
	m, err := c.Unmarshal(data[n:], &gotSecond)
	if err != nil {
		t.Fatalf("Unmarshal of trailing data failed: %v", err)
	}
	if !equalBitSets(second, &gotSecond) {
		t.Fatal("second bitset mismatch")
	}
	if n+m != len(data) {
		t.Fatalf("consumed %d+%d bytes, want %d", n, m, len(data))
	}
}

func TestCodecConcurrentEndianness(t *testing.T) {
	if BinaryOrder() != binary.BigEndian {
		t.Fatal("test expects the default global order to be big-endian")
	}
	big := NewCodec(binary.BigEndian, 0)
	little := NewCodec(binary.LittleEndian, 0)

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		for _, c := range []*Codec{big, little} {
			wg.Add(1)
			go func(c *Codec, seed uint) {
				defer wg.Done()
				src := New(200)
				for i := uint(0); i < 200; i += 3 {
					src.Set(i + seed%3)
				}
				data := c.Marshal(src)
				var dst BitSet
				if _, err := c.Unmarshal(data, &dst); err != nil {
					t.Errorf("Unmarshal failed: %v", err)
					return
				}
				if !equalBitSets(src, &dst) {
					t.Error("concurrent round trip mismatch")
				}
			}(c, uint(g))
		}
	}
	wg.Wait()

	// The two codecs must disagree on the wire format while the
	// global configuration stays untouched.
	src := testBitSet()
	if bytes.Equal(big.Marshal(src), little.Marshal(src)) {
		t.Fatal("big-endian and little-endian codecs produced identical output")
	}
	if BinaryOrder() != binary.BigEndian {
		t.Fatal("global binary order changed by codec usage")
	}
}

func TestCodecEmptyBitSet(t *testing.T) {
	c := NewCodec(binary.BigEndian, 0)
	var src BitSet
	data := c.Marshal(&src)
	if len(data) != wordBytes {
		t.Fatalf("empty bitset encoded to %d bytes, want %d", len(data), wordBytes)
	}
	var dst BitSet
	n, err := c.Unmarshal(data, &dst)
	if err != nil {
		t.Fatalf("Unmarshal of empty bitset failed: %v", err)
	}
	if n != wordBytes || dst.Len() != 0 {
		t.Fatalf("empty bitset decoded to length %d after %d bytes", dst.Len(), n)
	}
	if _, err := c.Unmarshal(nil, &dst); err != io.ErrUnexpectedEOF {
		t.Fatalf("Unmarshal of nil data: got %v, want io.ErrUnexpectedEOF", err)
	}
}
