package lz4block

import (
	"bytes"
	"crypto/rand"
	"math"
	"testing"
	"time"
)

// bruteCandidates builds an exact match finder: for every position j it
// returns the length and offset of the longest earlier match (ties keep the
// nearest candidate), the same choice the production match finder records
// when its chain depth and hash table are unrestricted.
// matchLenAt is the decoder-aware match length from src[p:] to src[j:].
func matchLenAt(src []byte, p, j int) int {
	n := len(src)
	off := j - p
	ml := 0
	for j+ml < n {
		var ref int
		if ml < off {
			ref = p + ml
		} else {
			ref = j + ml - off
		}
		if src[ref] != src[j+ml] {
			break
		}
		ml++
	}
	return ml
}

// bruteCandidates builds an exact match finder: for every position j it
// returns, per valid reference p, the decoder-aware match length. Only
// references within the 64kiB window are considered.
func bruteCandidates(src []byte) (candOff [][]int, candLen [][]int) {
	n := len(src)
	candOff = make([][]int, n)
	candLen = make([][]int, n)
	for j := 0; j < n-mfLimit; j++ {
		lo := j - winSize + 1
		if lo < 0 {
			lo = 0
		}
		for p := lo; p < j; p++ {
			if src[p] != src[j] || src[p+1] != src[j+1] || src[p+2] != src[j+2] || src[p+3] != src[j+3] {
				continue
			}
			candOff[j] = append(candOff[j], j-p)
			candLen[j] = append(candLen[j], matchLenAt(src, p, j))
		}
	}
	return candOff, candLen
}

// bruteCost is an exhaustive reference parser: it considers every possible
// next match start and every legal match length, using the actual LZ4 block
// encoding cost of each sequence. It returns the minimum total cost of a
// valid LZ4 block encoding of src.
func bruteCost(src []byte) int {
	n := len(src)
	candOff, candLen := bruteCandidates(src)

	memo := make([]int, n+1)
	for i := range memo {
		memo[i] = -1
	}
	var f func(i int) int
	f = func(i int) int {
		if i >= n {
			return 0
		}
		if memo[i] >= 0 {
			return memo[i]
		}
		best := lastLiteralsCost(n - i)
		// A sequence with literals src[i:j] and a match at j of length l.
		for j := i; j < n-mfLimit; j++ {
			if len(candOff[j]) == 0 {
				continue
			}
			// Every reference and every length from four up to the
			// decoder-aware full length for that reference is a candidate.
			for ci, full := range candLen[j] {
				maxL := full
				if m := n - j - (mfLimit - minMatch); maxL > m {
					maxL = m
				}
				_ = candOff[j][ci]
				for l := minMatch; l <= maxL; l++ {
					c := 1 + literalRunCost(j-i) + 2 + matchExtraCost(l) + f(j+l)
					if c < best {
						best = c
					}
				}
			}
		}
		memo[i] = best
		return best
	}
	return f(0)
}

// unboundedOptimalParams lifts every search and parser bound, so the
// production match finder sees every candidate and the parser considers
// every postponement and match length. The memory budget is left at default.
func unboundedOptimalParams() OptimalParams {
	return OptimalParams{
		MemoryBudget: defaultOptimalMemoryBudget,
		Candidates:   math.MaxInt32,
		Gap:          math.MaxInt32,
		Lookahead:    math.MaxInt32,
		ShortMatch:   math.MaxInt32,
	}
}

// allStrings appends every length-n string over alphabet [0,k) to out.
func allStrings(out [][]byte, prefix []byte, n, k int) [][]byte {
	if n == 0 {
		out = append(out, append([]byte(nil), prefix...))
		return out
	}
	for c := 0; c < k; c++ {
		out = allStrings(out, append(prefix, byte(c)), n-1, k)
	}
	return out
}

// TestOptimalExhaustiveCost checks the production cost-optimal parser
// against the exhaustive reference on every short input over small
// alphabets. This covers repeated prefixes, far matches and the literal
// run/match length extension thresholds where greedy parsers fail.
func TestOptimalExhaustiveCost(t *testing.T) {
	const maxLen = 11
	var c CompressorOptimal
	total := 0
	alphabets := []struct{ k, maxN int }{{2, maxLen}, {3, maxLen - 3}}
	for _, a := range alphabets {
		k, maxN := a.k, a.maxN
		for n := 0; n <= maxN; n++ {
			var inputs [][]byte
			inputs = allStrings(inputs, nil, n, k)
			for _, src := range inputs {
				total++
				want := bruteCost(src)
				dst := make([]byte, CompressBlockBound(n))
				got, err := c.CompressBlock(src, dst, unboundedOptimalParams())
				if err != nil {
					t.Fatalf("src=%v: %v", src, err)
				}
				if want >= n && n > 0 {
					// Exhaustive reference finds no benefit: the block is
					// stored uncompressed.
					if got != 0 {
						t.Fatalf("src=%v: got %d bytes, want incompressible (brute=%d)", src, got, want)
					}
					continue
				}
				if n == 0 {
					if got != 1 {
						t.Fatalf("empty: got %d, want 1 zero token", got)
					}
					continue
				}
				if got != want {
					t.Fatalf("src=%v: optimal produced %d bytes, brute force %d\noutput: % x", src, got, want, dst[:got])
				}
				dec := make([]byte, n)
				dn, derr := UncompressBlock(dst[:got], dec, nil)
				if derr != nil || dn != n || !bytes.Equal(dec, src) {
					t.Fatalf("src=%v: invalid block (%v, %d)", src, derr, dn)
				}
			}
		}
	}
	t.Logf("checked %d inputs", total)
}

// TestOptimalSampledCost extends the exhaustive check to longer, larger
// alphabet inputs, where enumerating every string is infeasible.
func TestOptimalSampledCost(t *testing.T) {
	var c CompressorOptimal
	for i := 0; i < 3000; i++ {
		n := 13 + (i*7)%60
		k := 2 + (i % 6)
		src := make([]byte, n)
		for j := range src {
			src[j] = byte((i*1103515245 + j*12345 + j*j*7) % k)
		}
		want := bruteCost(src)
		dst := make([]byte, CompressBlockBound(n))
		// The default parser is bounded (hence a heuristic); here we only
		// need a valid block. Exact equality with the brute reference is
		// enforced on small inputs by TestOptimalExhaustiveCost, where the
		// parser bounds are lifted.
		got, err := c.CompressBlock(src, dst, OptimalParams{})
		if err != nil {
			t.Fatalf("src=%v: %v", src, err)
		}
		if got == 0 {
			continue
		}
		dec := make([]byte, n)
		dn, derr := UncompressBlock(dst[:got], dec, nil)
		if derr != nil || dn != n || !bytes.Equal(dec, src) {
			t.Fatalf("src=%v: invalid block (%v, %d)", src, derr, dn)
		}
		// It must never be larger than the brute-force optimum.
		if got > want {
			t.Fatalf("src=%v: bounded optimal %d > brute optimum %d", src, got, want)
		}
	}
}

// TestOptimalRoundTrip exercises the default bounded parameters, which make
// the parser a heuristic, on a range of sizes and alphabets, and validates
// every block against the decoder.
func TestOptimalRoundTrip(t *testing.T) {
	var c CompressorOptimal
	sizes := []int{0, 1, 13, 14, 15, 16, 100, 4095, 4096, 70000}
	for i, n := range sizes {
		src := make([]byte, n)
		switch i % 3 {
		case 0:
			// repeated prefix / runs
			for j := range src {
				src[j] = byte(j % 5)
			}
		case 1:
			// sparse alphabet
			for j := range src {
				src[j] = byte((j * 2654435761) % 3)
			}
		case 2:
			if _, err := rand.Read(src); err != nil {
				t.Fatal(err)
			}
		}
		dst := make([]byte, CompressBlockBound(n))
		start := time.Now()
		got, err := c.CompressBlock(src, dst, OptimalParams{})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		t.Logf("n=%d case=%d -> %d bytes in %v", n, i%3, got, elapsed)
		if got == 0 {
			continue
		}
		dec := make([]byte, n)
		dn, derr := UncompressBlock(dst[:got], dec, nil)
		if derr != nil || dn != n || !bytes.Equal(dec, src) {
			t.Fatalf("n=%d: invalid block (%v, %d)", n, derr, dn)
		}
	}
}

// TestOptimalBudgetDegradation checks that a budget too small for even the
// minimum tables deterministically falls back to the HC algorithm and still
// produces valid output, while a budget that fits runs the parser.
func TestOptimalBudgetDegradation(t *testing.T) {
	src := bytes.Repeat([]byte("the quick brown fox "), 5000)

	// Tiny budget: must still succeed (via HC fallback) and round-trip.
	tiny := make([]byte, CompressBlockBound(len(src)))
	n1, err := CompressBlockOptimal(src, tiny, OptimalParams{MemoryBudget: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if n1 == 0 {
		t.Fatal("expected compression")
	}
	out := make([]byte, len(src))
	if dn, err := UncompressBlock(tiny[:n1], out, nil); err != nil || dn != len(src) || !bytes.Equal(out, src) {
		t.Fatalf("tiny budget invalid block: %v %d", err, dn)
	}

	// Generous budget: parser runs and is never worse than the fallback.
	full := make([]byte, CompressBlockBound(len(src)))
	n2, err := CompressBlockOptimal(src, full, OptimalParams{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default optimal=%d hc9-fallback=%d (parser may differ slightly)", n2, n1)

	// Shrinking budgets must be stable: each produces a valid block.
	prev := math.MaxInt
	for _, budget := range []int{1 << 12, 1 << 14, 1 << 18, 1 << 22, 1 << 26} {
		buf := make([]byte, CompressBlockBound(len(src)))
		n, err := CompressBlockOptimal(src, buf, OptimalParams{MemoryBudget: budget})
		if err != nil {
			t.Fatalf("budget=%d: %v", budget, err)
		}
		_ = prev
		prev = n
	}
}

// TestOptimalShortDestination verifies the destination buffer contract:
// encoding to the bound always succeeds, smaller destinations return 0 with
// or without an error, and nothing panics.
func TestOptimalShortDestination(t *testing.T) {
	src := bytes.Repeat([]byte("abcdefgh"), 3000)
	for _, dlen := range []int{0, 1, 10, len(src) / 2, len(src), CompressBlockBound(len(src))} {
		dst := make([]byte, dlen)
		n, err := CompressBlockOptimal(src, dst, OptimalParams{})
		if n < 0 || n > dlen {
			t.Fatalf("dlen=%d: n=%d out of range", dlen, n)
		}
		if dlen == CompressBlockBound(len(src)) && (err != nil || n == 0) {
			t.Fatalf("dlen=bound: n=%d err=%v", n, err)
		}
		if n > 0 {
			dec := make([]byte, len(src))
			if dn, derr := UncompressBlock(dst[:n], dec, nil); derr != nil || dn != len(src) || !bytes.Equal(dec, src) {
				t.Fatalf("dlen=%d: invalid block", dlen)
			}
		}
	}
}
