// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"testing"
)

// sliceIter is an IPRangeIterator over an in-memory slice. It counts
// Next calls so tests can verify when consumption stops.
type sliceIter struct {
	rr      []IPRange
	pos     int
	started bool
	nexts   int
}

func (s *sliceIter) Next() bool {
	s.nexts++
	if !s.started {
		s.started = true
		s.pos = 0
	} else {
		s.pos++
	}
	return s.pos < len(s.rr)
}

func (s *sliceIter) Range() IPRange { return s.rr[s.pos] }

func (s *sliceIter) Err() error { return nil }

// errIter returns up to failAt ranges from rr, then a read error.
type errIter struct {
	rr     []IPRange
	failAt int
	err    error
	pos    int
	nexts  int
}

func (e *errIter) Next() bool {
	e.nexts++
	if e.pos >= e.failAt || e.pos >= len(e.rr) {
		return false
	}
	e.pos++
	return true
}

func (e *errIter) Range() IPRange { return e.rr[e.pos-1] }

func (e *errIter) Err() error {
	if e.pos >= e.failAt && e.failAt <= len(e.rr) {
		return e.err
	}
	return nil
}

func tr(from, to string) IPRange {
	return IPRangeFrom(netip.MustParseAddr(from), netip.MustParseAddr(to))
}

func rangesEq(a, b []IPRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIPSetFromSortedRangesValid(t *testing.T) {
	tests := []struct {
		name string
		in   []IPRange
		want []IPRange
	}{
		{
			name: "empty",
		},
		{
			name: "single",
			in:   []IPRange{tr("10.0.0.0", "10.0.0.5")},
			want: []IPRange{tr("10.0.0.0", "10.0.0.5")},
		},
		{
			name: "disjoint",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.5"),
				tr("10.0.0.10", "10.0.0.20"),
			},
			want: []IPRange{
				tr("10.0.0.0", "10.0.0.5"),
				tr("10.0.0.10", "10.0.0.20"),
			},
		},
		{
			name: "adjacent",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.5"),
				tr("10.0.0.6", "10.0.0.20"),
			},
			want: []IPRange{tr("10.0.0.0", "10.0.0.20")},
		},
		{
			name: "overlap",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.10"),
				tr("10.0.0.5", "10.0.0.20"),
			},
			want: []IPRange{tr("10.0.0.0", "10.0.0.20")},
		},
		{
			name: "duplicate",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.10"),
				tr("10.0.0.0", "10.0.0.10"),
			},
			want: []IPRange{tr("10.0.0.0", "10.0.0.10")},
		},
		{
			name: "contained",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.20"),
				tr("10.0.0.5", "10.0.0.10"),
			},
			want: []IPRange{tr("10.0.0.0", "10.0.0.20")},
		},
		{
			name: "same_from_extends",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.5"),
				tr("10.0.0.0", "10.0.0.20"),
			},
			want: []IPRange{tr("10.0.0.0", "10.0.0.20")},
		},
		{
			name: "adjacent_at_v4_max",
			in: []IPRange{
				tr("255.255.255.254", "255.255.255.255"),
			},
			want: []IPRange{tr("255.255.255.254", "255.255.255.255")},
		},
		{
			name: "merge_v6",
			in: []IPRange{
				tr("::", "::1"),
				tr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
			},
			want: []IPRange{
				tr("::", "::1"),
				tr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
			},
		},
		{
			name: "adjacent_v6",
			in: []IPRange{
				tr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffc", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffd"),
				tr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
			},
			want: []IPRange{
				tr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffc", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"),
			},
		},
		{
			name: "family_boundary",
			in: []IPRange{
				tr("10.0.0.0", "10.255.255.255"),
				tr("::", "::ffff"),
				tr("2000::", "2000::ffff"),
			},
			want: []IPRange{
				tr("10.0.0.0", "10.255.255.255"),
				tr("::", "::ffff"),
				tr("2000::", "2000::ffff"),
			},
		},
		{
			name: "family_boundary_adjacent_each_side",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.5"),
				tr("10.0.0.6", "10.0.0.7"),
				tr("::", "::5"),
				tr("::6", "::7"),
			},
			want: []IPRange{
				tr("10.0.0.0", "10.0.0.7"),
				tr("::", "::7"),
			},
		},
		{
			name: "merge_chain",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.3"),
				tr("10.0.0.3", "10.0.0.6"),
				tr("10.0.0.10", "10.0.0.10"),
				tr("10.0.0.11", "10.0.0.12"),
			},
			want: []IPRange{
				tr("10.0.0.0", "10.0.0.6"),
				tr("10.0.0.10", "10.0.0.12"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := &sliceIter{rr: tt.in}
			s, err := IPSetFromSortedRanges(it)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := s.Ranges(); !rangesEq(got, tt.want) {
				t.Errorf("ranges = %v, want %v", got, tt.want)
			}
			// Successful iteration consumes every range plus one
			// final Next that reports exhaustion.
			if wantNexts := len(tt.in) + 1; it.nexts != wantNexts {
				t.Errorf("Next calls = %d, want %d", it.nexts, wantNexts)
			}
		})
	}
}

func TestIPSetFromSortedRangesErrors(t *testing.T) {
	invalidFromZero := IPRangeFrom(netip.Addr{}, netip.MustParseAddr("10.0.0.1"))
	tests := []struct {
		name    string
		in      []IPRange
		wantIdx int
		wantTag string
	}{
		{
			name:    "invalid_zero",
			in:      []IPRange{invalidFromZero},
			wantIdx: 0,
			wantTag: "invalid IPRange",
		},
		{
			name: "invalid_mixed_family",
			in: []IPRange{
				tr("10.0.0.0", "::1"),
			},
			wantIdx: 0,
			wantTag: "invalid IPRange",
		},
		{
			name: "invalid_from_after_to",
			in: []IPRange{
				tr("10.0.0.10", "10.0.0.9"),
			},
			wantIdx: 0,
			wantTag: "invalid IPRange",
		},
		{
			name: "out_of_order_simple",
			in: []IPRange{
				tr("10.0.0.10", "10.0.0.20"),
				tr("10.0.0.0", "10.0.0.5"),
			},
			wantIdx: 1,
			wantTag: "out of order",
		},
		{
			name: "out_of_order_after_merge",
			in: []IPRange{
				tr("10.0.0.0", "10.0.0.10"),
				tr("10.0.0.20", "10.0.0.30"),
				tr("10.0.0.5", "10.0.0.6"),
			},
			wantIdx: 2,
			wantTag: "out of order",
		},
		{
			name: "family_order_v6_then_v4",
			in: []IPRange{
				tr("::", "::1"),
				tr("10.0.0.0", "10.0.0.1"),
			},
			wantIdx: 1,
			wantTag: "address-family order",
		},
		{
			name: "family_twice",
			in: []IPRange{
				tr("1.0.0.0", "1.0.0.1"),
				tr("::", "::1"),
				tr("2.0.0.0", "2.0.0.1"),
			},
			wantIdx: 2,
			wantTag: "address-family order",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := &sliceIter{rr: tt.in}
			s, err := IPSetFromSortedRanges(it)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if s != nil {
				t.Fatalf("expected nil IPSet on error, got %v", s.Ranges())
			}
			wantSubstr := fmt.Sprintf("input index %d", tt.wantIdx)
			if !strings.Contains(err.Error(), wantSubstr) {
				t.Errorf("error %q does not contain %q", err, wantSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantTag) {
				t.Errorf("error %q does not contain %q", err, tt.wantTag)
			}
			// Consumption stops at the offending range; no further
			// Next call is made.
			if wantNexts := tt.wantIdx + 1; it.nexts != wantNexts {
				t.Errorf("Next calls = %d, want %d", it.nexts, wantNexts)
			}
		})
	}
}

var errSimulatedRead = errors.New("simulated read failure")

func TestIPSetFromSortedRangesIteratorError(t *testing.T) {
	tests := []struct {
		name   string
		rr     []IPRange
		failAt int
	}{
		{name: "error_on_first", failAt: 0},
		{name: "error_midway", rr: []IPRange{
			tr("10.0.0.0", "10.0.0.5"),
			tr("10.0.0.10", "10.0.0.20"),
		}, failAt: 1},
		{name: "error_after_all", rr: []IPRange{
			tr("10.0.0.0", "10.0.0.5"),
		}, failAt: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := &errIter{rr: tt.rr, failAt: tt.failAt, err: errSimulatedRead}
			s, err := IPSetFromSortedRanges(it)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if s != nil {
				t.Fatalf("expected nil IPSet, got %v", s.Ranges())
			}
			if !errors.Is(err, errSimulatedRead) {
				t.Errorf("error does not wrap iterator error: %v", err)
			}
			wantSubstr := fmt.Sprintf("input index %d", tt.failAt)
			if !strings.Contains(err.Error(), wantSubstr) {
				t.Errorf("error %q does not contain %q", err, wantSubstr)
			}
			// The failing Next is the last one; no reads after it.
			if wantNexts := tt.failAt + 1; it.nexts != wantNexts {
				t.Errorf("Next calls = %d, want %d", it.nexts, wantNexts)
			}
		})
	}
}

// countingIter wraps another iterator and atomically records the total
// number of Next calls, including any made after an error.
type countingIter struct {
	inner IPRangeIterator
	calls int
}

func (c *countingIter) Next() bool {
	c.calls++
	return c.inner.Next()
}

func (c *countingIter) Range() IPRange { return c.inner.Range() }

func (c *countingIter) Err() error { return c.inner.Err() }

func TestIPSetFromSortedRangesStopsAfterError(t *testing.T) {
	// A valid prefix followed by an out-of-order range, then many
	// more ranges that must never be read.
	rr := []IPRange{
		tr("10.0.0.0", "10.0.0.10"),
		tr("9.0.0.0", "9.0.0.1"),
	}
	for i := 0; i < 100; i++ {
		rr = append(rr, tr("8.0.0.0", "8.0.0.1"))
	}
	inner := &sliceIter{rr: rr}
	it := &countingIter{inner: inner}
	s, err := IPSetFromSortedRanges(it)
	if err == nil || s != nil {
		t.Fatalf("expected error and nil set, got %v, %v", s, err)
	}
	if it.calls != 2 {
		t.Errorf("Next calls = %d, want 2 (must not read past error)", it.calls)
	}
}

// randomSortedRanges builds a random but legal ascending stream of
// ranges, mixing families and exercising duplicates, containment,
// overlap and adjacency.
func randomSortedRanges(rng *rand.Rand) []IPRange {
	var out []IPRange
	gen := func(addrs func(lo, hi uint64) IPRange) {
		const space = 1 << 20 // test within a 20-bit address window
		next := uint64(0)
		count := 1 + rng.Intn(40)
		for i := 0; i < count; i++ {
			if next >= space {
				break
			}
			from := next + uint64(rng.Intn(4))
			if from >= space {
				break
			}
			to := from + uint64(rng.Intn(8))
			if to >= space {
				to = space - 1
			}
			out = append(out, addrs(from, to))
			switch rng.Intn(4) {
			case 0:
				// Duplicate or smaller-range-contained duplicate.
				out = append(out, addrs(from, to))
			case 1:
				// A wholly-contained sub-range (possibly sharing
				// the lower bound).
				if to > from {
					lo := from + uint64(rng.Intn(int(to-from)))
					hi := lo + uint64(rng.Intn(int(to-lo)))
					out = append(out, addrs(lo, hi))
				}
			}
			// Advance; the next range may overlap, touch, or leave
			// a gap.
			next = to + uint64(rng.Intn(3))
		}
	}
	v4 := func(lo, hi uint64) IPRange {
		return IPRangeFrom(
			netip.AddrFrom4([4]byte{10, byte(lo >> 16), byte(lo >> 8), byte(lo)}),
			netip.AddrFrom4([4]byte{10, byte(hi >> 16), byte(hi >> 8), byte(hi)}),
		)
	}
	v6 := func(lo, hi uint64) IPRange {
		return IPRangeFrom(
			netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, byte(lo >> 16), byte(lo >> 8), byte(lo), 0}),
			netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, byte(hi >> 16), byte(hi >> 8), byte(hi), 0xff}),
		)
	}
	gen(v4)
	gen(v6)
	return out
}

func TestIPSetFromSortedRangesMatchesBuilder(t *testing.T) {
	iters := 300
	if testing.Short() {
		iters = 30
	}
	for seed := int64(0); seed < int64(iters); seed++ {
		rng := rand.New(rand.NewSource(seed))
		rr := randomSortedRanges(rng)

		var b IPSetBuilder
		for _, r := range rr {
			b.AddRange(r)
		}
		want, err := b.IPSet()
		if err != nil {
			t.Fatalf("seed %d: builder error: %v", seed, err)
		}

		got, err := IPSetFromSortedRanges(&sliceIter{rr: rr})
		if err != nil {
			t.Fatalf("seed %d: stream error: %v (ranges %v)", seed, err, rr)
		}
		if !got.Equal(want) {
			t.Fatalf("seed %d: mismatch.\n input: %v\n got:  %v\n want: %v",
				seed, rr, got.Ranges(), want.Ranges())
		}
	}
}

func TestIPSetFromSortedRangesLarge(t *testing.T) {
	t.Run("all_adjacent_merge_to_one", func(t *testing.T) {
		const n = 4096
		rr := make([]IPRange, n)
		for i := 0; i < n; i++ {
			from := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
			to := netip.AddrFrom4([4]byte{10, byte((i + 1) >> 16), byte((i + 1) >> 8), byte(i + 1)})
			rr[i] = IPRangeFrom(from, to)
		}

		it := &sliceIter{rr: rr}
		s, err := IPSetFromSortedRanges(it)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Ranges(); len(got) != 1 {
			t.Fatalf("got %d ranges, want 1: %v", len(got), got)
		}

		var b IPSetBuilder
		for _, r := range rr {
			b.AddRange(r)
		}
		want := buildIPSet(&b)
		if !s.Equal(want) {
			t.Fatalf("merged set differs from builder: %v vs %v", s.Ranges(), want.Ranges())
		}

		streamAllocs := testing.AllocsPerRun(100, func() {
			_, err := IPSetFromSortedRanges(&sliceIter{rr: rr})
			if err != nil {
				t.Fatal(err)
			}
		})
		builderAllocs := testing.AllocsPerRun(100, func() {
			var b IPSetBuilder
			for _, r := range rr {
				b.AddRange(r)
			}
			if _, err := b.IPSet(); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("all_adjacent: stream=%v allocs builder=%v allocs", streamAllocs, builderAllocs)
		if streamAllocs >= builderAllocs {
			t.Errorf("streaming path allocated %v, want fewer than builder's %v",
				streamAllocs, builderAllocs)
		}
	})

	t.Run("all_disjoint", func(t *testing.T) {
		const n = 4096
		rr := make([]IPRange, n)
		for i := 0; i < n; i++ {
			// 256-address slots; each range covers the first 2, the
			// remaining 254 stay empty, so ranges never touch.
			base := uint32(i) << 8
			from := netip.AddrFrom4([4]byte{byte(base >> 24), byte(base >> 16), byte(base >> 8), byte(base)})
			to := netip.AddrFrom4([4]byte{byte(base >> 24), byte(base >> 16), byte(base >> 8), byte(base + 1)})
			rr[i] = IPRangeFrom(from, to)
		}

		s, err := IPSetFromSortedRanges(&sliceIter{rr: rr})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(s.Ranges()); got != n {
			t.Fatalf("got %d ranges, want %d", got, n)
		}

		var b IPSetBuilder
		for _, r := range rr {
			b.AddRange(r)
		}
		want := buildIPSet(&b)
		if !s.Equal(want) {
			t.Fatalf("disjoint set differs from builder")
		}

		streamAllocs := testing.AllocsPerRun(100, func() {
			_, err := IPSetFromSortedRanges(&sliceIter{rr: rr})
			if err != nil {
				t.Fatal(err)
			}
		})
		builderAllocs := testing.AllocsPerRun(100, func() {
			var b IPSetBuilder
			for _, r := range rr {
				b.AddRange(r)
			}
			if _, err := b.IPSet(); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("all_disjoint: stream=%v allocs builder=%v allocs", streamAllocs, builderAllocs)
		if streamAllocs > builderAllocs {
			t.Errorf("streaming path allocated %v, want no more than builder's %v",
				streamAllocs, builderAllocs)
		}
	})
}
