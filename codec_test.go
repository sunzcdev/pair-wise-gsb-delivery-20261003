package bitset

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"
	"testing"
)

func TestCodecRoundTripSlice(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		codec := NewCodec(order, 0)
		src := New(1010).Set(10).Set(1001)

		data, err := codec.Marshal(src)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if len(data) != src.BinaryStorageSize() {
			t.Fatalf("encoded size = %d, want %d", len(data), src.BinaryStorageSize())
		}

		dst := new(BitSet)
		if err := codec.Unmarshal(data, dst); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !src.Equal(dst) {
			t.Fatalf("round trip mismatch:\n\t%s\n\t%s", src.DumpAsBits(), dst.DumpAsBits())
		}
	}
}

func TestCodecRoundTripStream(t *testing.T) {
	const length = 9585
	const oneEvery = 97
	trailing := []byte("remaining-bytes")

	for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		codec := NewCodec(order, 0)
		src := New(length)
		for i := uint(0); i < length; i += oneEvery {
			src.Set(i)
		}

		var buf bytes.Buffer
		n, err := codec.Write(&buf, src)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != int64(src.BinaryStorageSize()) {
			t.Fatalf("written = %d, want %d", n, src.BinaryStorageSize())
		}
		buf.Write(trailing)

		dst := new(BitSet)
		n, err = codec.Read(&buf, dst)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n != int64(src.BinaryStorageSize()) {
			t.Fatalf("read count = %d, want %d", n, src.BinaryStorageSize())
		}
		if !src.Equal(dst) {
			t.Fatal("bitset mismatch after stream round trip")
		}

		// Trailing bytes must be left for the caller.
		rest, err := io.ReadAll(&buf)
		if err != nil {
			t.Fatalf("read trailing: %v", err)
		}
		if !bytes.Equal(rest, trailing) {
			t.Fatalf("trailing bytes = %q, want %q", rest, trailing)
		}
	}
}

func TestCodecEmpty(t *testing.T) {
	codec := NewCodec(binary.BigEndian, 0)
	data, err := codec.Marshal(New(0))
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if len(data) != wordBytes {
		t.Fatalf("empty encoding size = %d, want %d", len(data), wordBytes)
	}
	dst := New(100)
	if err := codec.Unmarshal(data, dst); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if dst.Len() != 0 {
		t.Fatalf("empty decode length = %d, want 0", dst.Len())
	}
}

func TestCodecMaxBits(t *testing.T) {
	const maxBits uint = 64
	codec := NewCodec(binary.BigEndian, maxBits)
	if codec.MaxBits() != maxBits {
		t.Fatalf("MaxBits = %d, want %d", codec.MaxBits(), maxBits)
	}

	t.Run("write too large", func(t *testing.T) {
		var buf bytes.Buffer
		if _, err := codec.Write(&buf, New(maxBits+1)); err == nil {
			t.Fatal("expected error writing bitset larger than max")
		}
		if buf.Len() != 0 {
			t.Fatalf("expected nothing written, got %d bytes", buf.Len())
		}
	})

	t.Run("read declared length too large", func(t *testing.T) {
		var buf bytes.Buffer
		binary.Write(&buf, binary.BigEndian, uint64(maxBits+1))
		buf.Write(make([]byte, wordBytes))

		dst := New(10)
		if err := codec.Unmarshal(buf.Bytes(), dst); err == nil {
			t.Fatal("expected error for declared length over max")
		}
		if dst.Len() != 10 {
			t.Fatalf("target changed on error, length = %d", dst.Len())
		}

		dst = New(10)
		if _, err := codec.Read(bytes.NewReader(buf.Bytes()), dst); err == nil {
			t.Fatal("expected stream error for declared length over max")
		}
		if dst.Len() != 10 {
			t.Fatalf("target changed on stream error, length = %d", dst.Len())
		}
	})

	t.Run("at limit accepted", func(t *testing.T) {
		src := New(maxBits).Set(0).Set(maxBits - 1)
		data, err := codec.Marshal(src)
		if err != nil {
			t.Fatalf("marshal at limit: %v", err)
		}
		dst := new(BitSet)
		if err := codec.Unmarshal(data, dst); err != nil {
			t.Fatalf("unmarshal at limit: %v", err)
		}
		if !src.Equal(dst) {
			t.Fatal("mismatch at limit")
		}
	})
}

func TestCodecTruncated(t *testing.T) {
	codec := NewCodec(binary.BigEndian, 0)
	src := New(130).Set(0).Set(129)
	data, err := codec.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, cut := range []int{0, 1, wordBytes - 1, wordBytes, len(data) - 1} {
		truncated := data[:cut]
		dst := New(10)
		if err := codec.Unmarshal(truncated, dst); err == nil {
			t.Fatalf("expected error for %d/%d bytes", cut, len(data))
		}
		if dst.Len() != 10 {
			t.Fatalf("target changed on truncated slice, length = %d", dst.Len())
		}

		dst = New(10)
		if _, err := codec.Read(bytes.NewReader(truncated), dst); err == nil {
			t.Fatalf("expected stream error for %d/%d bytes", cut, len(data))
		}
		if dst.Len() != 10 {
			t.Fatalf("target changed on truncated stream, length = %d", dst.Len())
		}
	}
}

func TestCodecTrailingBytesSlice(t *testing.T) {
	codec := NewCodec(binary.BigEndian, 0)
	data, err := codec.Marshal(New(65).Set(64))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dst := new(BitSet)
	if err := codec.Unmarshal(append(data, 0x01), dst); err == nil {
		t.Fatal("expected error for extra trailing byte")
	}
	if dst.Len() != 0 {
		t.Fatalf("target changed on trailing bytes, length = %d", dst.Len())
	}
}

func TestCodecDirtyPaddingBits(t *testing.T) {
	codec := NewCodec(binary.BigEndian, 0)

	makeDirty := func(length uint) []byte {
		var buf bytes.Buffer
		binary.Write(&buf, binary.BigEndian, uint64(length))
		nWords := wordsNeeded(length)
		words := make([]uint64, nWords)
		// Set a bit beyond the logical length in the final word.
		words[nWords-1] = uint64(1) << (length & wordMask)
		for _, w := range words {
			binary.Write(&buf, binary.BigEndian, w)
		}
		return buf.Bytes()
	}

	for _, length := range []uint{1, 63, 65, 100, 127} {
		data := makeDirty(length)
		dst := New(10)
		if err := codec.Unmarshal(data, dst); err == nil {
			t.Fatalf("length %d: expected dirty padding error", length)
		}
		if dst.Len() != 10 {
			t.Fatalf("length %d: target changed on dirty bits", length)
		}

		dst = New(10)
		if _, err := codec.Read(bytes.NewReader(data), dst); err == nil {
			t.Fatalf("length %d: expected stream dirty padding error", length)
		}
		if dst.Len() != 10 {
			t.Fatalf("length %d: target changed on stream dirty bits", length)
		}
	}
}

func TestCodecEncodingMatchesGlobal(t *testing.T) {
	// A Codec's output must be byte-identical to the global marshaller when
	// the global order matches, and must differ across endianness.
	src := New(200).Set(3).Set(199)

	global, err := src.MarshalBinary()
	if err != nil {
		t.Fatalf("global marshal: %v", err)
	}
	be, err := NewCodec(binary.BigEndian, 0).Marshal(src)
	if err != nil {
		t.Fatalf("be marshal: %v", err)
	}
	le, err := NewCodec(binary.LittleEndian, 0).Marshal(src)
	if err != nil {
		t.Fatalf("le marshal: %v", err)
	}
	if !bytes.Equal(global, be) {
		t.Fatal("big-endian codec output differs from global output")
	}
	if bytes.Equal(be, le) {
		t.Fatal("big and little endian encodings unexpectedly equal")
	}

	// Cross-endian decoding must fail (header lengths disagree).
	if err := NewCodec(binary.LittleEndian, 0).Unmarshal(be, new(BitSet)); err == nil {
		t.Fatal("expected error decoding big-endian data with little-endian codec")
	}
}

func TestCodecConcurrentEndiansDoNotInterfere(t *testing.T) {
	// Pin the global configuration and ensure codec activity changes nothing.
	prev := BinaryOrder()
	defer func() { binaryOrder = prev }()
	binaryOrder = binary.BigEndian

	be := NewCodec(binary.BigEndian, 0)
	le := NewCodec(binary.LittleEndian, 0)

	const iterations = 200
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			length := uint(1 + (i % 500))
			src := New(length).Set(uint(i) % length)
			data, err := be.Marshal(src)
			if err != nil {
				t.Errorf("be marshal: %v", err)
				return
			}
			dst := new(BitSet)
			if err := be.Unmarshal(data, dst); err != nil || !src.Equal(dst) {
				t.Errorf("be round trip failed: %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			length := uint(1 + (i % 500))
			src := New(length).Set(uint(i) % length)
			data, err := le.Marshal(src)
			if err != nil {
				t.Errorf("le marshal: %v", err)
				return
			}
			dst := new(BitSet)
			if err := le.Unmarshal(data, dst); err != nil || !src.Equal(dst) {
				t.Errorf("le round trip failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if BinaryOrder() != binary.BigEndian {
		t.Fatal("codec use modified the global binary order")
	}
}

func TestCodecNilOrderDefaultsToBigEndian(t *testing.T) {
	codec := NewCodec(nil, 0)
	if codec.Order() != binary.BigEndian {
		t.Fatal("nil order should default to big endian")
	}
}
