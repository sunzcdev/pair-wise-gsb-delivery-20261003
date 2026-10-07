package lz4block_test

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/pierrec/lz4/v4"
	"github.com/pierrec/lz4/v4/internal/lz4block"
)

func referenceLiteralCost(length int) int {
	cost := length
	if length >= 0xF {
		cost += 1 + (length-0xF)/0xFF
	}
	return cost
}

func referenceMatchExtra(length int) int {
	length -= 4
	if length < 0xF {
		return 0
	}
	return 1 + (length-0xF)/0xFF
}

func referenceCommonPrefix(a []byte, x, y int) int {
	n := 0
	for x+n < len(a) && y+n < len(a) && a[x+n] == a[y+n] {
		n++
	}
	return n
}

// optimalBlockCost enumerates every anchor, reference and match length. It is
// deliberately quadratic/cubic and only used for inputs short enough to make
// the search exhaustive.
func optimalBlockCost(src []byte) int {
	n := len(src)
	best := make([]int, n+1)
	for i := 1; i <= n; i++ {
		best[i] = math.MaxInt
	}

	answer := 1 + referenceLiteralCost(n)
	for pos := 0; pos+12 <= n; pos++ {
		before := math.MaxInt
		for anchor := 0; anchor <= pos; anchor++ {
			if best[anchor] == math.MaxInt {
				continue
			}
			cost := best[anchor] + referenceLiteralCost(pos-anchor)
			if cost < before {
				before = cost
			}
		}
		if before == math.MaxInt {
			continue
		}

		first := pos - int(lz4block.Block64Kb) + 1
		if first < 0 {
			first = 0
		}
		for ref := first; ref < pos; ref++ {
			limit := referenceCommonPrefix(src, ref, pos)
			if max := n - 5 - pos; limit > max {
				limit = max
			}
			if limit < 4 {
				continue
			}
			for matchLen := 4; matchLen <= limit; matchLen++ {
				cost := before + 3 + referenceMatchExtra(matchLen)
				if end := pos + matchLen; cost < best[end] {
					best[end] = cost
				}
			}
		}
	}

	for anchor := 0; anchor <= n; anchor++ {
		if best[anchor] == math.MaxInt {
			continue
		}
		cost := best[anchor] + 1 + referenceLiteralCost(n-anchor)
		if cost < answer {
			answer = cost
		}
	}
	return answer
}

func roundTripCost(t *testing.T, src []byte, opts lz4.CostOptions) int {
	t.Helper()
	dst := make([]byte, lz4block.CompressBlockBound(len(src)))
	n, err := lz4.CompressBlockCost(src, dst, opts)
	if err != nil {
		t.Fatalf("compression failed: %v", err)
	}
	if n == 0 {
		t.Fatal("expected compressed output with a full-size destination")
	}
	got := make([]byte, len(src))
	m, err := lz4block.UncompressBlock(dst[:n], got, nil)
	if err != nil {
		t.Fatalf("decompression failed: %v", err)
	}
	if m != len(src) || !bytes.Equal(got, src) {
		t.Fatal("round trip mismatch")
	}
	return n
}

func TestCostOptimalExhaustiveShortInputs(t *testing.T) {
	for n := 0; n <= 15; n++ {
		for pattern := 0; pattern < 1<<n; pattern++ {
			src := make([]byte, n)
			for i := range src {
				src[i] = byte(pattern >> i & 1)
			}
			got := roundTripCost(t, src, lz4.CostOptions{})
			if want := optimalBlockCost(src); got != want {
				t.Fatalf("pattern %0*b: cost %d, want %d", n, pattern, got, want)
			}
		}
	}

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		n := rng.Intn(65)
		src := make([]byte, n)
		for j := range src {
			src[j] = byte(rng.Intn(4))
		}
		got := roundTripCost(t, src, lz4.CostOptions{})
		if i == 6 {
			dst := make([]byte, lz4block.CompressBlockBound(len(src)))
			n, _ := lz4.CompressBlockCost(src, dst, lz4.CostOptions{})
			t.Logf("encoded %x", dst[:n])
		}
		if want := optimalBlockCost(src); got != want {
			t.Fatalf("random input %d: src=%v cost %d, want %d", i, src, got, want)
		}
	}
}

func TestCostOptimalLongInputs(t *testing.T) {
	prefix := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 2048)
	far := make([]byte, 140<<10)
	for i := range far {
		far[i] = byte(i * 7)
	}
	copy(far[65000:], "distant-reference-0123456789")
	copy(far, "distant-reference-0123456789")
	rng := rand.New(rand.NewSource(11))
	random := make([]byte, 128<<10)
	rng.Read(random)

	for name, src := range map[string][]byte{
		"repeated prefix": prefix,
		"far match":       far,
		"incompressible":  random,
	} {
		t.Run(name, func(t *testing.T) {
			roundTripCost(t, src, lz4.CostOptions{MaxCandidates: 64})
			roundTripCost(t, src, lz4.CostOptions{MaxCandidates: 1})
			roundTripCost(t, src, lz4.CostOptions{MemoryBudget: 1})
		})
	}
}

func TestCostOptimizedWriter(t *testing.T) {
	src := bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz"), 256)
	var compressed bytes.Buffer
	writer := lz4.NewWriter(&compressed)
	if err := writer.Apply(lz4.CostOptimizedOption(lz4.CostOptions{MaxCandidates: 8})); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(lz4.NewReader(&compressed))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Fatal("frame round trip mismatch")
	}
}
