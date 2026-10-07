package lz4_test

import (
	"bytes"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// TestUncompressBlockScatterAPI exercises the public variadic entry points and
// checks that a block decodes identically across different fragmentations.
func TestUncompressBlockScatterAPI(t *testing.T) {
	data := bytes.Repeat([]byte("public-scatter-API!"), 128)

	compressed := make([]byte, lz4.CompressBlockBound(len(data)))
	n, err := lz4.CompressBlock(data, compressed, nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed = compressed[:n]

	split := func(sizes ...int) [][]byte {
		parts := make([][]byte, len(sizes))
		for i, s := range sizes {
			parts[i] = make([]byte, s)
		}
		return parts
	}

	// A handful of fixed partitions: first fragment 1 byte, then a 1-byte
	// fragment, an empty one, and the remainder.
	total := len(data)
	cases := [][][]byte{
		split(total),
		split(1, total-1),
		split(1, 1, 0, total-2),
		split(func() []int {
			sizes := make([]int, 0, total)
			sizes = append(sizes, 0)
			for range data {
				sizes = append(sizes, 1)
			}
			sizes = append(sizes, 0)
			return sizes
		}()...),
	}

	for i, parts := range cases {
		got := n
		var err error
		if len(parts) > 0 {
			got, err = lz4.UncompressBlockScatter(compressed, parts...)
		}
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got != total {
			t.Fatalf("case %d: got %d want %d", i, got, total)
		}
		var assembled bytes.Buffer
		for _, p := range parts {
			assembled.Write(p)
		}
		if !bytes.Equal(assembled.Bytes(), data) {
			t.Fatalf("case %d: mismatch", i)
		}
	}

	// Dictionary entry point: a match sourced entirely from the dictionary.
	dict := []byte("barbazquux")
	src := []byte("\x11b\x0a\x00\x401234")
	want := []byte("barbaz1234")
	dparts := [][]byte{make([]byte, 1), make([]byte, 0), make([]byte, len(want)-1)}
	got, err := lz4.UncompressBlockScatterWithDict(src, dict, dparts...)
	if err != nil {
		t.Fatal(err)
	}
	if got != len(want) {
		t.Fatalf("got %d want %d", got, len(want))
	}
	var assembled bytes.Buffer
	for _, p := range dparts {
		assembled.Write(p)
	}
	if !bytes.Equal(assembled.Bytes(), want) {
		t.Fatalf("dict scatter = %q want %q", assembled.Bytes(), want)
	}
}
