package lz4_test

import (
	"bytes"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestUncompressBlockSlices(t *testing.T) {
	raw := bytes.Repeat([]byte("ABCDEFGH"), 128)
	compressed := make([]byte, lz4.CompressBlockBound(len(raw)))
	n, err := lz4.CompressBlock(raw, compressed, nil)
	if err != nil || n == 0 {
		t.Fatalf("CompressBlock: n=%d err=%v", n, err)
	}
	compressed = compressed[:n]

	pages := make([][]byte, 0, len(raw)+1)
	pages = append(pages, []byte{})
	for range raw {
		pages = append(pages, make([]byte, 1))
	}
	pages = append(pages, []byte{})

	n, err = lz4.UncompressBlockSlices(compressed, pages)
	if err != nil || n != len(raw) {
		t.Fatalf("UncompressBlockSlices: n=%d err=%v", n, err)
	}

	for i := range pages {
		for j := range pages[i] {
			pages[i][j] = 0
		}
	}
	n, err = lz4.UncompressBlockSlicesWithDict(compressed, pages, nil)
	if err != nil || n != len(raw) {
		t.Fatalf("UncompressBlockSlicesWithDict: n=%d err=%v", n, err)
	}

	var got bytes.Buffer
	for _, page := range pages {
		got.Write(page)
	}
	if !bytes.Equal(got.Bytes(), raw) {
		t.Fatal("scattered output mismatch")
	}
}
