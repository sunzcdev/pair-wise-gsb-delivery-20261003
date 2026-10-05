// Copyright 2023 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"testing"
)

func buildSet(t *testing.T, ranges ...string) *IPSet {
	t.Helper()
	return mustIPSet(ranges...)
}

type wantCoverage struct {
	r     string
	count int
}

func formatCoverage(got []CoveredRange) string {
	var b strings.Builder
	for _, c := range got {
		fmt.Fprintf(&b, "{%s x%d} ", c.Range(), c.Count())
	}
	return strings.TrimSpace(b.String())
}

func TestCoverage(t *testing.T) {
	v4max := "255.255.255.255"
	v6max := "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"

	tests := []struct {
		name       string
		minSources int
		// Each element of sets is one occurrence in the Coverage input;
		// the same *IPSet may be repeated via repeatFirst.
		sets [][]string
		// repeatFirst passes sets[0] a second time, modeling the same
		// *IPSet pointer appearing twice in the argument list.
		repeatFirst bool
		want        []wantCoverage
	}{
		{
			name:       "three_way_intersection",
			minSources: 3,
			sets: [][]string{
				{"+10.0.0.0-10.0.0.9"},
				{"+10.0.0.5-10.0.0.20"},
				{"+10.0.0.7-10.0.0.30"},
			},
			want: []wantCoverage{
				{"10.0.0.7-10.0.0.9", 3},
			},
		},
		{
			// Adjacent segments at depth 1 around the touching endpoints
			// must merge, even though three segments have distinct depths.
			name:       "common_endpoints_adjacent",
			minSources: 1,
			sets: [][]string{
				{"+10.0.0.0-10.0.0.4"},
				{"+10.0.0.5-10.0.0.9"},
				{"+10.0.0.5-10.0.0.5"},
			},
			want: []wantCoverage{
				{"10.0.0.0-10.0.0.4", 1},
				{"10.0.0.5-10.0.0.5", 2},
				{"10.0.0.6-10.0.0.9", 1},
			},
		},
		{
			name:       "single_address",
			minSources: 2,
			sets: [][]string{
				{"+10.0.0.1-10.0.0.1"},
				{"+10.0.0.1-10.0.0.1"},
			},
			want: []wantCoverage{
				{"10.0.0.1-10.0.0.1", 2},
			},
		},
		{
			name:       "identical_sets",
			minSources: 1,
			sets: [][]string{
				{"+10.0.0.0-10.0.0.10"},
				{"+10.0.0.0-10.0.0.10"},
				{"+10.0.0.0-10.0.0.10"},
			},
			want: []wantCoverage{
				{"10.0.0.0-10.0.0.10", 3},
			},
		},
		{
			name:       "v4_slash_zero",
			minSources: 2,
			sets: [][]string{
				// Two distinct sets, one of which is all of IPv4: the
				// result has depth 2 in the covered window and depth 1
				// elsewhere, and minSources=2 keeps only the window.
				{"+0.0.0.0-" + v4max},
				{"+192.0.2.0-192.0.2.255"},
			},
			want: []wantCoverage{
				{"192.0.2.0-192.0.2.255", 2},
			},
		},
		{
			name:       "both_slash_zero_depth_one",
			minSources: 1,
			sets: [][]string{
				{"+0.0.0.0-" + v4max},
				{"+::-" + v6max},
				{"+::-" + v6max},
			},
			want: []wantCoverage{
				{"0.0.0.0-" + v4max, 1},
				{"::" + "-" + v6max, 2},
			},
		},
		{
			name:       "v4_max_endpoint",
			minSources: 2,
			sets: [][]string{
				{"+255.255.255.250-" + v4max},
				{"+255.255.255.254-" + v4max},
			},
			want: []wantCoverage{
				{"255.255.255.254-" + v4max, 2},
			},
		},
		{
			name:       "v4_max_depth_one",
			minSources: 1,
			sets: [][]string{
				{"+255.255.255.250-" + v4max},
			},
			want: []wantCoverage{
				{"255.255.255.250-" + v4max, 1},
			},
		},
		{
			name:       "v6_max_endpoint",
			minSources: 2,
			sets: [][]string{
				{"+ffff:ffff:ffff:ffff:ffff:ffff:ffff:fff0-" + v6max},
				{"+ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe-" + v6max},
			},
			want: []wantCoverage{
				{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe-" + v6max, 2},
			},
		},
		{
			name:       "v6_max_depth_one",
			minSources: 1,
			sets: [][]string{
				{"+ffff:ffff:ffff:ffff:ffff:ffff:ffff:fff0-" + v6max},
			},
			want: []wantCoverage{
				{"ffff:ffff:ffff:ffff:ffff:ffff:ffff:fff0-" + v6max, 1},
			},
		},
		{
			// The middle window is covered by only one source and must
			// not merge the two depth-2 windows even though minSources=2
			// leaves no output for the hole.
			name:       "min_filter_keeps_hole",
			minSources: 2,
			sets: [][]string{
				{"+10.0.0.0-10.0.0.10"},
				{"+10.0.0.0-10.0.0.4", "+10.0.0.8-10.0.0.10"},
			},
			want: []wantCoverage{
				{"10.0.0.0-10.0.0.4", 2},
				{"10.0.0.8-10.0.0.10", 2},
			},
		},
		{
			// A zero-depth hole (covered by nobody) likewise separates
			// equally covered output ranges.
			name:       "zero_depth_hole",
			minSources: 1,
			sets: [][]string{
				{"+10.0.0.0-10.0.0.3", "+10.0.0.6-10.0.0.9"},
				{"+10.0.0.0-10.0.0.3", "+10.0.0.6-10.0.0.9"},
			},
			want: []wantCoverage{
				{"10.0.0.0-10.0.0.3", 2},
				{"10.0.0.6-10.0.0.9", 2},
			},
		},
		{
			// Overlapping ranges added to one builder are merged into a
			// normalized IPSet: a single occurrence is never double
			// counted, while a repeated *IPSet pointer is two sources.
			name:       "duplicate_pointer_is_two_sources",
			minSources: 2,
			sets: [][]string{
				{"+10.0.0.0-10.0.0.5", "+10.0.0.3-10.0.0.10"},
			},
			want: []wantCoverage{
				{"10.0.0.0-10.0.0.10", 2},
			},
			repeatFirst: true,
		},
		{
			name:       "mixed_family_order",
			minSources: 1,
			sets: [][]string{
				{"+2001:db8::1-2001:db8::2", "+192.0.2.0-192.0.2.1"},
			},
			want: []wantCoverage{
				{"192.0.2.0-192.0.2.1", 1},
				{"2001:db8::1-2001:db8::2", 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var built []*IPSet
			for _, ranges := range tt.sets {
				built = append(built, buildSet(t, ranges...))
			}
			if tt.repeatFirst {
				built = append(built, built[0])
			}
			// Exercise nil and empty inputs too.
			built = append(built, nil, buildSet(t))
			got := Coverage(tt.minSources, built...)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d ranges (%s), want %d (%v)",
					len(got), formatCoverage(got), len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if got[i].Count() != w.count {
					t.Errorf("range %d %s: count=%d want %d",
						i, got[i].Range(), got[i].Count(), w.count)
				}
				if got[i].Range().String() != w.r {
					t.Errorf("range %d: got %s want %s",
						i, got[i].Range(), w.r)
				}
				if got[i].Range().To().Less(got[i].Range().From()) {
					t.Errorf("range %d not ordered: %s", i, got[i].Range())
				}
			}
		})
	}
}

func TestCoverageNilAndDefaults(t *testing.T) {
	if got := Coverage(3, nil, nil); got != nil {
		t.Errorf("nil sets: got %v want nil", got)
	}
	empty := buildSet(t)
	if got := Coverage(1, empty); got != nil {
		t.Errorf("empty set: got %v want nil", got)
	}
	// minSources > len(sets) returns nothing.
	s := buildSet(t, "+10.0.0.0-10.0.0.1")
	if got := Coverage(5, s, s); got != nil {
		t.Errorf("minSources too high: got %v want nil", got)
	}
	// minSources <= 0 behaves like 1.
	got := Coverage(0, s)
	if len(got) != 1 || got[0].Count() != 1 {
		t.Errorf("minSources=0: got %s want single depth-1 range", formatCoverage(got))
	}
}

// naiveCoverage computes the expected result by counting sources at
// every address in the universe [base, base+size). Each input set is
// one source (its normalized ranges never overlap internally), and
// repeated *IPSet pointers are repeated sources. It walks addresses
// only because the universe is tiny; it never merges across gaps.
func naiveCoverage(t *testing.T, universe netip.Prefix, sets []*IPSet, minSources int) []wantCoverage {
	t.Helper()
	if minSources < 1 {
		minSources = 1
	}
	base := u128From16(universe.Addr().As16())
	size := uint64(1) << (uint(universe.Addr().BitLen()) - uint(universe.Bits()))
	is4 := universe.Addr().Is4()

	var want []wantCoverage
	var runFrom, runTo uint128
	runDepth := 0
	inRun := false

	flush := func(to uint128, depth int) {
		if !inRun {
			return
		}
		if depth >= minSources {
			want = append(want, wantCoverage{
				r:     IPRangeFrom(runFrom.addr(is4), to.addr(is4)).String(),
				count: depth,
			})
		}
		inRun = false
	}

	for off := uint64(0); off < size; off++ {
		addrU := base
		addrU.lo += off
		addr := addrU.addr(is4)
		depth := 0
		for _, s := range sets {
			if s != nil && s.Contains(addr) {
				depth++
			}
		}
		if !inRun || depth != runDepth {
			if inRun {
				flush(runTo, runDepth)
			}
			runFrom, runTo, runDepth, inRun = addrU, addrU, depth, true
		} else {
			runTo = addrU
		}
	}
	if inRun {
		flush(runTo, runDepth)
	}
	return want
}

// randomSet builds an IPSet from up to maxRanges random, possibly
// overlapping ranges inside the universe.
func randomSet(rng *rand.Rand, universe netip.Prefix, maxRanges int) *IPSet {
	base := u128From16(universe.Addr().As16())
	is4 := universe.Addr().Is4()
	bits := uint(universe.Addr().BitLen())
	size := uint64(1) << (bits - uint(universe.Bits()))

	var b IPSetBuilder
	nRanges := rng.Intn(maxRanges + 1)
	for i := 0; i < nRanges; i++ {
		lo := rng.Uint64() % size
		hi := lo + rng.Uint64()%(size-lo)
		loU, hiU := base, base
		loU.lo += lo
		hiU.lo += hi
		b.AddRange(IPRangeFrom(loU.addr(is4), hiU.addr(is4)))
	}
	s, err := b.IPSet()
	if err != nil {
		panic(err)
	}
	return s
}

func TestCoverageNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	// Universes are deliberately tiny so the address-by-address naive
	// oracle is cheap; they include the /0 endpoint cases indirectly via
	// the dedicated table tests above.
	cases := []struct {
		name     string
		universe netip.Prefix
	}{
		{"v4", netip.MustParsePrefix("198.51.100.0/24")},
		{"v6", netip.MustParsePrefix("2001:db8::/120")},
	}

	for iter := 0; iter < 600; iter++ {
		tc := cases[iter%len(cases)]

		nSets := rng.Intn(5)
		sets := make([]*IPSet, 0, nSets+2)
		// Occasionally reuse the same pointer to model duplicate sources.
		var dup *IPSet
		for i := 0; i < nSets; i++ {
			s := randomSet(rng, tc.universe, 4)
			sets = append(sets, s)
			if i == 0 && rng.Intn(3) == 0 {
				dup = s
			}
		}
		if dup != nil {
			sets = append(sets, dup)
		}
		if rng.Intn(4) == 0 {
			sets = append(sets, nil)
		}

		minSources := 1 + rng.Intn(4)
		want := naiveCoverage(t, tc.universe, sets, minSources)
		got := Coverage(minSources, sets...)

		if len(got) != len(want) {
			t.Fatalf("%s iter %d min=%d: got %d ranges (%s), want %d (%v)\nsets: %s",
				tc.name, iter, minSources,
				len(got), formatCoverage(got), len(want), want,
				describeSets(sets))
		}
		for i := range want {
			if got[i].Range().String() != want[i].r ||
				got[i].Count() != want[i].count {
				t.Fatalf("%s iter %d min=%d range %d: got {%s x%d}, want {%s x%d}\nsets: %s",
					tc.name, iter, minSources, i,
					got[i].Range(), got[i].Count(),
					want[i].r, want[i].count,
					describeSets(sets))
			}
		}
	}
}

func describeSets(sets []*IPSet) string {
	var b strings.Builder
	for i, s := range sets {
		if s == nil {
			fmt.Fprintf(&b, "[%d]=nil ", i)
			continue
		}
		fmt.Fprintf(&b, "[%d]={", i)
		for _, r := range s.Ranges() {
			fmt.Fprintf(&b, "%s ", r)
		}
		b.WriteString("} ")
	}
	return strings.TrimSpace(b.String())
}

// benchmarkSets builds 3 IPv6 sets, each containing rangesPerSet
// disjoint /32 windows that cover ~2^32 addresses apiece. The address
// space covered is astronomically large, so stable timings as
// rangesPerSet grows demonstrate the work depends on the number of
// input ranges, not on the number of addresses.
func benchmarkSets(b *testing.B, rangesPerSet int) []*IPSet {
	base := u128From16(netip.MustParseAddr("2001:db8::").As16())
	is4 := false
	makeSet := func(offset uint64) *IPSet {
		var bd IPSetBuilder
		for i := 0; i < rangesPerSet; i++ {
			lo := base
			lo.lo += (uint64(i)*8 + offset) << 32
			hi := lo
			hi.lo += 4 << 32
			hi.lo -= 1
			bd.AddRange(IPRangeFrom(lo.addr(is4), hi.addr(is4)))
		}
		s, err := bd.IPSet()
		if err != nil {
			b.Fatal(err)
		}
		return s
	}
	return []*IPSet{makeSet(0), makeSet(2), makeSet(3)}
}

func BenchmarkCoverage(b *testing.B) {
	for _, rangesPerSet := range []int{10, 100, 1000} {
		sets := benchmarkSets(b, rangesPerSet)
		b.Run(fmt.Sprintf("rangesPerSet=%d", rangesPerSet), func(b *testing.B) {
			b.ReportAllocs()
			var sink []CoveredRange
			for i := 0; i < b.N; i++ {
				sink = Coverage(2, sets...)
			}
			_ = sink
		})
	}
}
