package lz4block

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/pierrec/lz4/v4/internal/lz4errors"
)

func buildScatterTestBlock() (src, dict []byte, want []byte) {
	dict = []byte("ABCDEFGHIJKLMNOPQRST")
	lit := []byte("12345")

	// The first match begins in the dictionary and continues into the output.
	src = appendSeq(nil, lit, 25, 30)
	var ok bool
	want, ok = refDecodeSeq(nil, dict, lit, 25, 30)
	if !ok {
		panic("invalid scatter test block")
	}

	// A literal long enough to cross several page boundaries.
	longLiteral := bytes.Repeat([]byte("L"), 25)
	src = appendSeq(src, longLiteral, 1, 70)
	want = append(want, longLiteral...)
	want = refAppendMatch(want, 1, 70)

	// Another overlapping pattern crosses the remaining boundaries.
	src = appendSeq(src, nil, 2, 35)
	want = refAppendMatch(want, 2, 35)

	src = appendSeq(src, []byte("Z"), 0, 0)
	want = append(want, 'Z')
	return src, dict, want
}

func refAppendMatch(out []byte, off, length int) []byte {
	for i := 0; i < length; i++ {
		out = append(out, out[len(out)-off])
	}
	return out
}

func scatterPartitions(n int) [][]int {
	thirds := []int{0, 1}
	for _, cut := range []int{35, 60, n / 4, n / 2, 130} {
		if cut > 0 && cut < n {
			thirds = append(thirds, cut)
		}
	}
	sets := [][]int{
		{},
		{0},
		{1},
	}
	sets = append(sets, thirds)

	duplicates := append(append([]int{}, thirds...), n, n)
	sets = append(sets, duplicates)

	if n > 130 {
		sets = append(sets, []int{0, 65, 70, 75, 130, n})
	}
	for chunk := 1; chunk <= 17; chunk++ {
		var cuts []int
		for cut := chunk; cut < n; cut += chunk {
			cuts = append(cuts, cut)
		}
		sets = append(sets, cuts)
	}
	for seed := int64(0); seed < 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		var cuts []int
		for pos := 0; pos < n; pos += 1 + r.Intn(7) {
			cuts = append(cuts, pos)
			if r.Intn(3) == 0 {
				cuts = append(cuts, pos)
			}
		}
		sets = append(sets, cuts)
	}
	return sets
}

func splitForSlices(n int, cuts []int) [][]byte {
	dsts := make([][]byte, 0, len(cuts)+1)
	start := 0
	for _, cut := range cuts {
		if cut < start {
			dsts = append(dsts, []byte{})
			continue
		}
		if cut > start {
			dsts = append(dsts, make([]byte, cut-start))
		}
		dsts = append(dsts, []byte{})
		start = cut
	}
	dsts = append(dsts, make([]byte, n-start))
	return dsts
}

func joinSlices(dsts [][]byte) []byte {
	var out []byte
	for _, dst := range dsts {
		out = append(out, dst...)
	}
	return out
}

func TestUncompressBlockSlices(t *testing.T) {
	src, dict, want := buildScatterTestBlock()

	for i, cuts := range scatterPartitions(len(want)) {
		t.Run(fmt.Sprintf("partition%02d", i), func(t *testing.T) {
			dsts := splitForSlices(len(want), cuts)
			n, err := UncompressBlockSlices(src, dsts, dict)
			if err != nil || n != len(want) {
				t.Fatalf("n=%d err=%v, want %d", n, err, len(want))
			}
			if got := joinSlices(dsts); !bytes.Equal(got, want) {
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("first mismatch at %d: got %#x, want %#x", i, got[i], want[i])
					}
				}
			}
		})
	}
}

func TestUncompressBlockSlicesRandomCompressed(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	patterns := [][]byte{
		bytes.Repeat([]byte("ABCD"), 128),
		append(bytes.Repeat([]byte("x"), 20), append(bytes.Repeat([]byte("1234567"), 20), []byte("tail")...)...),
	}
	for iter := 0; iter < 20; iter++ {
		raw := make([]byte, 80+rng.Intn(200))
		rng.Read(raw[:20])
		for i := 20; i < len(raw); i++ {
			raw[i] = raw[i-1-rng.Intn(12)]
		}
		if iter < len(patterns) {
			raw = append(append([]byte(nil), patterns[iter]...), raw...)
		}
		bound := CompressBlockBound(len(raw))
		compressed := make([]byte, bound)
		n, err := CompressBlock(raw, compressed)
		if err != nil || n == 0 {
			t.Fatalf("iter %d: compression failed: n=%d err=%v", iter, n, err)
		}
		compressed = compressed[:n]

		for _, cuts := range scatterPartitions(len(raw)) {
			dsts := splitForSlices(len(raw), cuts)
			n, err := UncompressBlockSlices(compressed, dsts, nil)
			if err != nil || n != len(raw) {
				t.Fatalf("iter %d: n=%d err=%v, want %d", iter, n, err, len(raw))
			}
			if got := joinSlices(dsts); !bytes.Equal(got, raw) {
				t.Fatalf("iter %d: output mismatch", iter)
			}
		}
	}
}

func TestUncompressBlockSlicesErrors(t *testing.T) {
	src, dict, want := buildScatterTestBlock()

	t.Run("empty source", func(t *testing.T) {
		if n, err := UncompressBlockSlices(nil, splitForSlices(1, nil), nil); err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if n, err := UncompressBlockSlices([]byte{0}, nil, nil); err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if n, err := UncompressBlockSlices([]byte{0x10}, nil, nil); err != lz4errors.ErrInvalidSourceShortBuffer || n != 0 {
			t.Fatalf("non-empty block with no destination: n=%d err=%v", n, err)
		}
	})

	t.Run("short logical destination", func(t *testing.T) {
		dsts := splitForSlices(len(want)-1, []int{0, 1, 35, 60})
		n, err := UncompressBlockSlices(src, dsts, dict)
		if err != lz4errors.ErrInvalidSourceShortBuffer {
			t.Fatalf("err=%v", err)
		}
		if n != len(want)-1 {
			t.Fatalf("n=%d, want %d", n, len(want)-1)
		}
		if got := joinSlices(dsts); !bytes.Equal(got, want[:len(want)-1]) {
			t.Fatal("written prefix mismatch")
		}
	})

	t.Run("invalid offset after prefix", func(t *testing.T) {
		bad := appendSeq(nil, []byte("abcd"), 0, 4)
		dsts := splitForSlices(8, []int{1, 2, 4})
		for i := range dsts[len(dsts)-1] {
			dsts[len(dsts)-1][i] = guardByte
		}
		n, err := UncompressBlockSlices(bad, dsts, nil)
		if err != lz4errors.ErrInvalidSourceShortBuffer || n != 4 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		got := joinSlices(dsts)
		if !bytes.Equal(got[:4], []byte("abcd")) {
			t.Fatalf("prefix %q", got[:4])
		}
		for i, b := range got[4:] {
			if b != guardByte {
				t.Fatalf("guard byte at %d modified: %#x", i, b)
			}
		}
	})
}
