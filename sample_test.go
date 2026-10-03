// Copyright 2023 The Go4 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"sort"
	"testing"
)

// scriptReader serves its chunks as distinct Read calls. Once the
// chunks are exhausted it returns finalErr (typically io.EOF), so the
// number of Read calls and bytes consumed is fully observable.
type scriptReader struct {
	chunks   [][]byte
	reads    int
	finalErr error
}

func (s *scriptReader) Read(p []byte) (int, error) {
	s.reads++
	if len(s.chunks) == 0 {
		return 0, s.finalErr
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return copy(p, c), nil
}

// countReader counts bytes served before its underlying reader fails.
type countReader struct {
	r     io.Reader
	bytes int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes += n
	return n, err
}

func beBytes(width int, n uint64) []byte {
	b := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return b
}

// bigBytes returns the big-endian fixed-width encoding of v.
func bigBytes(width int, v *big.Int) []byte {
	b := make([]byte, width)
	raw := v.Bytes()
	copy(b[width-len(raw):], raw)
	return b
}

// small5Set is a set of 5 members across address families, split
// into two non-adjacent ranges:
//   - 10.0.0.1, 10.0.0.2
//   - 2001:db8::1, 2001:db8::2, 2001:db8::3
func small5Set(t *testing.T) *IPSet {
	t.Helper()
	var b IPSetBuilder
	b.AddRange(IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.2")))
	b.AddRange(IPRangeFrom(mustIP("2001:db8::1"), mustIP("2001:db8::3")))
	return buildIPSet(&b)
}

// floydRef is a reference implementation of Floyd's n-subset draw
// over a virtual space of N members. draws[t] is the accepted draw in
// [0, N-n+t). It returns the subset as sorted virtual indices.
func floydRef(N, n int, draws []int) []int {
	chosen := map[int]bool{}
	for t := 0; t < n; t++ {
		j := draws[t]
		bound := N - n + t + 1
		if j < 0 || j >= bound {
			panic(fmt.Sprintf("draw %d out of range [0,%d)", j, bound))
		}
		if !chosen[j] {
			chosen[j] = true
		} else {
			chosen[bound-1] = true
		}
	}
	out := make([]int, 0, n)
	for v := range chosen {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// enumerateDraws enumerates every accepted-draw stream for Floyd
// sampling n of N.
func enumerateDraws(N, n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	var rec func(t int, cur []int)
	rec = func(t int, cur []int) {
		if t == n {
			out = append(out, append([]int(nil), cur...))
			return
		}
		bound := N - n + t + 1
		for j := 0; j < bound; j++ {
			rec(t+1, append(cur, j))
		}
	}
	rec(0, nil)
	return out
}

// drawsToReader converts accepted draws into the byte stream randInt
// needs, with byteLen bytes per draw (no rejections). Draws against
// a bound of 1 consume no bytes and are skipped.
func drawsToReader(N, n int, draws []int, byteLen int) *scriptReader {
	r := &scriptReader{finalErr: io.EOF}
	for t, d := range draws {
		if N-n+t+1 == 1 {
			continue
		}
		r.chunks = append(r.chunks, beBytes(byteLen, uint64(d)))
	}
	return r
}

var small5Members = []string{
	"10.0.0.1",
	"10.0.0.2",
	"2001:db8::1",
	"2001:db8::2",
	"2001:db8::3",
}

// TestSampleExhaustiveSmall walks every possible accepted random
// stream in a tiny mixed-family space and checks deterministically
// that every n-subset is produced equally often, that results are
// sorted, contain no duplicates, and only contain real members. No
// statistical assertions are involved.
func TestSampleExhaustiveSmall(t *testing.T) {
	const N = 5
	for _, n := range []int{0, 1, 2, 3, 5} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			set := small5Set(t)
			streams := enumerateDraws(N, n)
			pairCount := map[string]int{}
			memberHits := map[string]int{}
			for _, draws := range streams {
				r := drawsToReader(N, n, draws, 1)
				got, err := set.Sample(n, r)
				if err != nil {
					t.Fatalf("Sample: %v", err)
				}
				if len(got) != n {
					t.Fatalf("got %d addrs, want %d", len(got), n)
				}
				if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Compare(got[j]) < 0 }) {
					t.Fatalf("not sorted: %v", got)
				}
				seen := map[string]bool{}
				var keyParts []string
				for _, a := range got {
					s := a.String()
					if seen[s] {
						t.Fatalf("duplicate %s in %v", s, got)
					}
					seen[s] = true
					memberHits[s]++
					keyParts = append(keyParts, s)
				}
				key := fmt.Sprint(keyParts)
				pairCount[key]++

				wantIdx := floydRef(N, n, draws)
				var wantAddrs []string
				for _, vi := range wantIdx {
					wantAddrs = append(wantAddrs, small5Members[vi])
				}
				if fmt.Sprint(wantAddrs) != key {
					t.Fatalf("draws %v -> %v, want %v", draws, keyParts, wantAddrs)
				}
			}

			// Every subset must appear the same number of times.
			wantSubsets := 1
			for k := 0; k < n; k++ {
				wantSubsets = wantSubsets * (N - k) / (k + 1)
			}
			if len(pairCount) != wantSubsets {
				t.Fatalf("observed %d distinct subsets, want %d: %v", len(pairCount), wantSubsets, pairCount)
			}
			wantFreq := len(streams) / wantSubsets
			for key, c := range pairCount {
				if c != wantFreq {
					t.Fatalf("subset %s frequency %d, want %d", key, c, wantFreq)
				}
			}
			// Every member must be reachable, and family weighting
			// follows member counts: expected hits = streams * n/N.
			if n > 0 {
				wantHits := len(streams) * n / N
				for _, m := range small5Members {
					if memberHits[m] != wantHits {
						t.Fatalf("member %s hits %d, want %d", m, memberHits[m], wantHits)
					}
				}
			}
		})
	}
}

// TestSampleZeroConsumesNothing checks that n=0 never touches the
// reader, even on an empty set.
func TestSampleZeroConsumesNothing(t *testing.T) {
	set := mustIPSet()
	r := &countReader{r: &scriptReader{finalErr: errors.New("random source broken")}}
	got, err := set.Sample(0, r)
	if err != nil {
		t.Fatalf("Sample(0): %v", err)
	}
	if len(got) != 0 || got == nil {
		t.Fatalf("Sample(0) = %v, want non-nil empty slice", got)
	}
	if r.bytes != 0 {
		t.Fatalf("consumed %d bytes for n=0", r.bytes)
	}

	// Also on a non-empty set, with a nil reader.
	got, err = small5Set(t).Sample(0, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Sample(0, nil) = %v, %v", got, err)
	}
}

// TestSampleErrors exercises every error branch. None of them may
// return a partial sample.
func TestSampleErrors(t *testing.T) {
	set := small5Set(t)

	if got, err := set.Sample(-1, &scriptReader{finalErr: io.EOF}); !errors.Is(err, ErrSampleTooLarge) || got != nil {
		t.Fatalf("negative n: got %v, %v", got, err)
	}

	if got, err := set.Sample(6, &scriptReader{finalErr: io.EOF}); !errors.Is(err, ErrSampleTooLarge) || got != nil {
		t.Fatalf("n > cardinality: got %v, %v", got, err)
	}

	if got, err := mustIPSet().Sample(1, &scriptReader{finalErr: io.EOF}); !errors.Is(err, ErrSampleTooLarge) || got != nil {
		t.Fatalf("empty set: got %v, %v", got, err)
	}

	// Cardinality checks must not consume randomness.
	cr := &countReader{r: &scriptReader{finalErr: io.EOF}}
	_, err := set.Sample(6, cr)
	if err == nil || cr.bytes != 0 {
		t.Fatalf("cardinality error consumed %d bytes, err=%v", cr.bytes, err)
	}

	if got, err := set.Sample(1, nil); err == nil || got != nil {
		t.Fatalf("nil reader: got %v, %v", got, err)
	}

	// Reader fails immediately.
	if got, err := set.Sample(1, &scriptReader{finalErr: io.EOF}); err == nil || got != nil {
		t.Fatalf("immediate EOF: got %v, %v", got, err)
	}

	// Reader surfaces a non-EOF error; it must be wrapped and visible.
	sentinel := errors.New("hw rng exploded")
	if got, err := set.Sample(1, &scriptReader{finalErr: sentinel}); !errors.Is(err, sentinel) || got != nil {
		t.Fatalf("reader error: got %v, %v", got, err)
	}

	// Short read: some bytes then EOF yields ErrUnexpectedEOF.
	short := &scriptReader{chunks: [][]byte{make([]byte, 15)}, finalErr: io.EOF}
	// Force a 16-byte draw: sample from a set with a > 2^120 member.
	var b IPSetBuilder
	b.AddRange(IPRangeFrom(mustIP("::"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")))
	huge := buildIPSet(&b)
	if got, err := huge.Sample(1, short); !errors.Is(err, io.ErrUnexpectedEOF) || got != nil {
		t.Fatalf("short read: got %v, %v", got, err)
	}

	// Failure on a post-rejection redraw: first draw rejected, second
	// read hits EOF. bound=3 (byteLen=1, bitLen=2): values 3 are
	// rejected (limit is 4? no: limit=floor(4/3)*3=3, so val>=3
	// rejects).
	rj := &scriptReader{chunks: [][]byte{{3}}, finalErr: io.EOF}
	set3 := mustIPSet("+10.0.0.0-10.0.0.2")
	if got, err := set3.Sample(1, rj); err == nil || got != nil {
		t.Fatalf("post-rejection failure: got %v, %v", got, err)
	}
	if rj.reads != 2 {
		t.Fatalf("reads = %d, want 2 (rejected then failed)", rj.reads)
	}
}

// TestRandIntExhaustive deterministically verifies that rejection
// sampling produces every residue exactly equally often for every
// possible input byte, across small and large (16-byte, non-power-of-2
// and power-of-2) bounds.
func TestRandIntExhaustive(t *testing.T) {
	t.Run("1byte", func(t *testing.T) {
		for _, bound := range []int64{2, 3, 5, 7, 128, 200, 255} {
			counts := make(map[int64]int)
			rejects := 0
			for b := 0; b < 256; b++ {
				r := &scriptReader{chunks: [][]byte{{byte(b)}}, finalErr: io.EOF}
				got, err := randInt(r, big.NewInt(bound), nil)
				if err != nil {
					rejects++
					continue
				}
				counts[got.val.Int64()]++
			}
			if len(counts) != int(bound) {
				t.Fatalf("bound %d: observed %d residues, want %d (counts=%v, rejects=%d)", bound, len(counts), bound, counts, rejects)
			}
			// Masking reduces inputs to an effective domain of
			// pow2 = 2^bitLen values, each with 256/pow2 raw
			// preimages. Accepted values are [0, limit).
			pow2 := int64(1) << (big.NewInt(bound).BitLen())
			limit := pow2 / bound * bound
			want := (limit / bound) * (256 / pow2)
			wantRej := (pow2 - limit) * (256 / pow2)
			for v := int64(0); v < bound; v++ {
				if int64(counts[v]) != want {
					t.Fatalf("bound %d: residue %d count %d, want %d (rejects=%d)", bound, v, counts[v], want, rejects)
				}
			}
			if int64(rejects) != wantRej {
				t.Fatalf("bound %d: rejects %d unexpected", bound, rejects)
			}
		}
	})

	t.Run("2byte", func(t *testing.T) {
		// Enumerating all 65536 two-byte inputs is fast.
		for _, tc := range []struct {
			bound     int64
			wantCount int
			wantRej   int
		}{
			{256, 256, 0}, // power of two: masking maps all inputs into 0..511, all accepted
			{257, 128, 32640},
		} {
			bound := tc.bound
			counts := make(map[int64]int)
			rejects := 0
			for b := 0; b < 65536; b++ {
				r := &scriptReader{chunks: [][]byte{beBytes(2, uint64(b))}, finalErr: io.EOF}
				got, err := randInt(r, big.NewInt(bound), nil)
				if err != nil {
					rejects++
					continue
				}
				counts[got.val.Int64()]++
			}
			if rejects != tc.wantRej {
				t.Fatalf("bound %d: rejects = %d, want %d", bound, rejects, tc.wantRej)
			}
			if len(counts) != int(bound) {
				t.Fatalf("bound %d: observed %d residues", bound, len(counts))
			}
			for v := int64(0); v < bound; v++ {
				if counts[v] != tc.wantCount {
					t.Fatalf("bound %d: residue %d count %d, want %d", bound, v, counts[v], tc.wantCount)
				}
			}
		}
	})

	t.Run("16byte", func(t *testing.T) {
		// bound = 2^124 + 1: bitLen 125, byteLen 16, excess 3.
		// Inputs 0..7 are masked from the high byte; 8 distinct high
		// bytes map to each. Check just the boundary values instead
		// of all 2^128 inputs:
		//   val = limit-1 -> residue limit-1
		//   val = limit   -> rejected
		//   val = pow2-1  -> rejected
		bound := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 124), big.NewInt(1))
		pow2 := new(big.Int).Lsh(big.NewInt(1), 125)
		limit := new(big.Int).Mul(new(big.Int).Quo(pow2, bound), bound)

		// Accepted boundary.
		acc := new(big.Int).Sub(limit, big.NewInt(1))
		r := &scriptReader{chunks: [][]byte{bigBytes(16, acc)}, finalErr: io.EOF}
		got, err := randInt(r, bound, nil)
		if err != nil {
			t.Fatalf("accepted boundary: %v", err)
		}
		if got.val.Cmp(new(big.Int).Mod(new(big.Int).Set(acc), bound)) != 0 {
			t.Fatalf("accepted residue = %x, want %x", got.val, new(big.Int).Mod(acc, bound))
		}

		// Rejected boundaries: the first non-accepted value, the top
		// of the masked range, and an input whose unmasked high byte
		// is masked down to that top value.
		allFF := make([]byte, 16)
		for i := range allFF {
			allFF[i] = 0xff
		}
		for _, rv := range []*big.Int{
			new(big.Int).Set(limit),
			new(big.Int).Sub(pow2, big.NewInt(1)),
			new(big.Int).SetBytes(allFF), // masked high byte -> pow2-1
		} {
			r := &scriptReader{chunks: [][]byte{bigBytes(16, rv)}, finalErr: io.EOF}
			if _, err := randInt(r, bound, nil); err == nil {
				t.Fatalf("value %x should have been rejected", rv)
			}
			if r.reads != 2 {
				t.Fatalf("rejection should redraw once, got %d reads", r.reads)
			}
		}
	})

	t.Run("bound1", func(t *testing.T) {
		r := &scriptReader{finalErr: io.EOF}
		got, err := randInt(r, big.NewInt(1), nil)
		if err != nil || got.val.Sign() != 0 {
			t.Fatalf("bound 1: got %v, %v", got.val, err)
		}
		if r.reads != 0 {
			t.Fatalf("bound 1 consumed %d reads", r.reads)
		}
	})

	t.Run("invalid-bound", func(t *testing.T) {
		for _, bound := range []*big.Int{big.NewInt(0), big.NewInt(-1)} {
			if _, err := randInt(&scriptReader{finalErr: io.EOF}, bound, nil); err == nil {
				t.Fatalf("bound %d: expected error", bound)
			}
		}
	})
}

// TestSampleMapping checks virtual-index to address translation
// across gaps, ranges, and address families, including offsets into
// non-first ranges and maximum addresses.
func TestSampleMapping(t *testing.T) {
	// Three ranges with a gap:
	//   10.0.0.0
	//   10.0.0.10 .. 10.0.0.11
	//   ::1 .. ::3
	var b IPSetBuilder
	b.Add(mustIP("10.0.0.0"))
	b.AddRange(IPRangeFrom(mustIP("10.0.0.10"), mustIP("10.0.0.11")))
	b.AddRange(IPRangeFrom(mustIP("::1"), mustIP("::3")))
	set := buildIPSet(&b)
	wantByIndex := []string{
		"10.0.0.0",
		"10.0.0.10",
		"10.0.0.11",
		"::1",
		"::2",
		"::3",
	}
	for idx, want := range wantByIndex {
		r := &scriptReader{chunks: [][]byte{{byte(idx)}}, finalErr: io.EOF}
		got, err := set.Sample(1, r)
		if err != nil {
			t.Fatalf("index %d: %v", idx, err)
		}
		if got[0].String() != want {
			t.Fatalf("index %d = %s, want %s", idx, got[0], want)
		}
	}
}

// TestSampleMaxAddresses forces draws at the maximum address of each
// family, including the full IPv6 space (offset 2^128-1, whose range
// size computation must not overflow).
func TestSampleMaxAddresses(t *testing.T) {
	t.Run("v4", func(t *testing.T) {
		var b IPSetBuilder
		b.AddRange(IPRangeFrom(mustIP("1.2.3.4"), mustIP("255.255.255.255")))
		set := buildIPSet(&b)
		size := rangeSize(set.rr[0])
		// bound == size, draw size-1 to force the top address.
		r := &scriptReader{chunks: [][]byte{bigBytes(4, new(big.Int).Sub(size, big.NewInt(1)))}, finalErr: io.EOF}
		got, err := set.Sample(1, r)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].String() != "255.255.255.255" {
			t.Fatalf("got %s", got[0])
		}
	})

	t.Run("v6-full", func(t *testing.T) {
		var b IPSetBuilder
		b.AddPrefix(mustIPPrefix("::/0"))
		set := buildIPSet(&b)
		size := rangeSize(set.rr[0])
		wantSize := new(big.Int).Lsh(big.NewInt(1), 128)
		if size.Cmp(wantSize) != 0 {
			t.Fatalf("full v6 size = %x, want 2^128", size)
		}
		max := new(big.Int).Sub(size, big.NewInt(1))
		// 17-byte draw: bound 2^128 has bitLen 129, excess 7, so the
		// high byte is masked to 0x01; no rejection occurs.
		r := &scriptReader{chunks: [][]byte{bigBytes(17, max)}, finalErr: io.EOF}
		got, err := set.Sample(1, r)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].String() != "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff" {
			t.Fatalf("got %s", got[0])
		}

		// Force :: as well, covering the zero offset branch.
		r = &scriptReader{chunks: [][]byte{make([]byte, 17)}, finalErr: io.EOF}
		got, err = set.Sample(1, r)
		if err != nil || got[0].String() != "::" {
			t.Fatalf("zero offset: got %v, %v", got, err)
		}
	})

	t.Run("mixed-universe", func(t *testing.T) {
		// All of IPv4 plus all of IPv6: cardinality 2^32 + 2^128.
		// Virtual index 2^32-1 is the last IPv4 address; index 2^32
		// is ::; the final index is the max IPv6 address.
		var b IPSetBuilder
		b.AddPrefix(mustIPPrefix("0.0.0.0/0"))
		b.AddPrefix(mustIPPrefix("::/0"))
		set := buildIPSet(&b)
		v4Size := new(big.Int).Lsh(big.NewInt(1), 32)

		tests := []struct {
			index *big.Int
			want  netip.Addr
		}{
			{new(big.Int).Sub(v4Size, big.NewInt(1)), mustIP("255.255.255.255")},
			{v4Size, mustIP("::")},
			{new(big.Int).Add(v4Size, big.NewInt(1)), mustIP("::1")},
			{new(big.Int).Add(v4Size, new(big.Int).Lsh(big.NewInt(1), 64)), mustIP("0:0:0:1::")},
			{
				new(big.Int).Add(v4Size, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))),
				mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
			},
		}
		for _, tt := range tests {
			r := &scriptReader{chunks: [][]byte{bigBytes(17, tt.index)}, finalErr: io.EOF}
			got, err := set.Sample(1, r)
			if err != nil {
				t.Fatalf("index %x: %v", tt.index, err)
			}
			if got[0].Compare(tt.want) != 0 {
				t.Fatalf("index %x -> %s, want %s", tt.index, got[0], tt.want)
			}
		}
	})
}

// TestSampleFloydCollision forces the collision branch of Floyd's
// algorithm and checks the substituted top-of-range value and the
// ascending sort.
func TestSampleFloydCollision(t *testing.T) {
	// N=4, n=2: first bound 3 then 4. Draw j=0 both times: the
	// second collides and substitutes t-1 of the second iteration,
	// namely 3.
	set := mustIPSet("+10.0.0.0-10.0.0.3")
	r := &scriptReader{chunks: [][]byte{{0}, {0}}, finalErr: io.EOF}
	got, err := set.Sample(2, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "10.0.0.0" || got[1].String() != "10.0.0.3" {
		t.Fatalf("got %v, want [10.0.0.0 10.0.0.3]", got)
	}

	// Same collision across the family boundary: 2 v4 + 3 v6, first
	// draw 0, second draw 0 (bound 5) collides and inserts 4, the
	// final v6 address; output must sort v4 before v6.
	set = small5Set(t)
	r = &scriptReader{chunks: [][]byte{{0}, {0}, {0}, {0}, {0}}, finalErr: io.EOF}
	got, err = set.Sample(5, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %v", got)
	}
	for i, want := range small5Members {
		if got[i].String() != want {
			t.Fatalf("position %d = %s, want %s; full %v", i, got[i], want, got)
		}
	}
}

// TestSampleChunkedReader confirms io.ReadFull semantics: a single
// draw assembled from several smaller reads works, including a
// 17-byte draw from one-byte chunks for the full IPv6 space.
func TestSampleChunkedReader(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(mustIPPrefix("::/0"))
	set := buildIPSet(&b)

	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	raw := bigBytes(17, max)
	var chunks [][]byte
	for _, c := range raw {
		chunks = append(chunks, []byte{c})
	}
	r := &scriptReader{chunks: chunks, finalErr: io.EOF}
	got, err := set.Sample(1, r)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].String() != "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff" {
		t.Fatalf("got %s", got[0])
	}
	if r.reads != 17 {
		t.Fatalf("reads = %d, want 17", r.reads)
	}
}

// TestSampleResultsAreMembers verifies every returned address is
// actually contained in the sampled set, and that repeated draws stay
// distinct, for a multi-range set with holes.
func TestSampleResultsAreMembers(t *testing.T) {
	set := mustIPSet(
		"+10.0.0.0-10.0.0.1",
		"+192.168.0.0-192.168.0.2",
	)
	// n = 3 of 5: enumerate accepted streams, assert containment and
	// distinctness.
	for _, draws := range enumerateDraws(5, 3) {
		r := drawsToReader(5, 3, draws, 1)
		got, err := set.Sample(3, r)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, a := range got {
			if !set.Contains(a) {
				t.Fatalf("%s not in set (draws %v)", a, draws)
			}
			if seen[a.String()] {
				t.Fatalf("duplicate %s", a)
			}
			seen[a.String()] = true
		}
	}
}

// TestSampleWeightedByMembership explicitly checks that draw
// probability follows member counts, not range counts. The set has
// two ranges, one holding four IPv4 members and one holding a single
// IPv6 member: a per-range weighting would give each family an even
// 1/2 chance, whereas member weighting gives v6 only 1/5.
func TestSampleWeightedByMembership(t *testing.T) {
	var b IPSetBuilder
	b.AddRange(IPRangeFrom(mustIP("10.0.0.0"), mustIP("10.0.0.3")))
	b.Add(mustIP("2001:db8::1"))
	set := buildIPSet(&b)

	familyHits := map[bool]int{}
	for d := 0; d < 5; d++ {
		r := drawsToReader(5, 1, []int{d}, 1)
		got, err := set.Sample(1, r)
		if err != nil {
			t.Fatal(err)
		}
		familyHits[got[0].Is6()]++
	}
	if familyHits[false] != 4 || familyHits[true] != 1 {
		t.Fatalf("v4 hits %d, v6 hits %d; want 4 and 1 (member weighting)",
			familyHits[false], familyHits[true])
	}
}

// TestSampleDoesNotMutateSet ensures sampling leaves the set intact.
func TestSampleDoesNotMutateSet(t *testing.T) {
	set := small5Set(t)
	before := set.Ranges()
	for i := 0; i < 10; i++ {
		r := &scriptReader{chunks: [][]byte{{0}, {0}}, finalErr: io.EOF}
		if _, err := set.Sample(2, r); err != nil {
			t.Fatal(err)
		}
	}
	after := set.Ranges()
	if len(before) != len(after) {
		t.Fatalf("ranges changed: %v vs %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("range %d changed", i)
		}
	}
}

// TestSampleRealReader is a smoke test with the standard secure
// reader; statistical properties are covered by the exhaustive tests.
func TestSampleRealReader(t *testing.T) {
	set := small5Set(t)
	got, err := set.Sample(3, bytes.NewReader(make([]byte, 64)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, a := range got {
		if !set.Contains(a) {
			t.Fatalf("%s not in set", a)
		}
	}

	// All-zero stream still draws valid (if heavily colliding)
	// members: N=5, n=3 draws 0,0,0 -> {0, 3, 4}.
	want := []string{small5Members[0], small5Members[3], small5Members[4]}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("zero stream: got %v, want %v", got, want)
		}
	}
}

var _ netip.Addr
