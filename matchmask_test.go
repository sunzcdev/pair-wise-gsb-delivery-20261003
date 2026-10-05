// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"errors"
	"math/big"
	"net/netip"
	"testing"
)

// TestMatchMaskExhaustive8 enumerates every (value, mask) pair in a
// simulated 8-bit address space and checks that the ranges produced by
// matchMaskRanges are exact against the bit-match predicate:
//
//	x matches iff x&mask == value&mask.
//
// 8 bits is small enough to test all 65536 value/mask combinations and
// every possible address per combination.
func TestMatchMaskExhaustive8(t *testing.T) {
	const n = 8
	const space = 1 << n
	wantCount := func(value, mask uint64) *big.Int {
		if mask == 0 {
			return big.NewInt(1)
		}
		free := n - bitCount8(int(mask))
		// Bits below the lowest fixed bit do not split ranges.
		exp := free - trailingZeros8(int(mask))
		return new(big.Int).Lsh(big.NewInt(1), uint(exp))
	}

	for value := 0; value < space; value++ {
		for mask := 0; mask < space; mask++ {
			v := uint128{0, uint64(value)}
			m := uint128{0, uint64(mask)}
			ranges, count := matchMaskRanges(v, m, n, space)
			if len(ranges) != int(count.Int64()) {
				t.Fatalf("value=%08b mask=%08b: got %d ranges but count=%v", value, mask, len(ranges), count)
			}
			if count.Cmp(wantCount(uint64(value), uint64(mask))) != 0 {
				t.Fatalf("value=%08b mask=%08b: count=%v want %v", value, mask, count, wantCount(uint64(value), uint64(mask)))
			}

			// Membership must exactly equal the predicate.
			for x := 0; x < space; x++ {
				want := uint64(x)&uint64(mask) == uint64(value)&uint64(mask)
				got := rangesContain128(ranges, uint128{0, uint64(x)})
				if got != want {
					t.Fatalf("value=%08b mask=%08b x=%08b: contains=%v want %v", value, mask, x, got, want)
				}
			}

			// Ranges must be sorted, non-overlapping, non-adjacent,
			// and exact-width blocks; i.e. already minimal.
			for i, r := range ranges {
				if r.lo.hi != 0 || r.hi.hi != 0 || r.lo.lo > r.hi.lo {
					t.Fatalf("value=%08b mask=%08b: bad range %v", value, mask, r)
				}
				if i > 0 && r.lo.lo <= ranges[i-1].hi.lo {
					t.Fatalf("value=%08b mask=%08b: ranges not minimal/sorted: %v then %v", value, mask, ranges[i-1], r)
				}
			}

			// Over-limit reporting must be exact and must not build.
			if got := int(count.Int64()); got > 1 {
				none, n2 := matchMaskRanges(v, m, n, got-1)
				if none != nil || n2.Cmp(count) != 0 {
					t.Fatalf("value=%08b mask=%08b: over-limit got ranges=%v count=%v, want nil and %v", value, mask, none != nil, n2, count)
				}
			}
			if len(ranges) > 0 {
				exact, n3 := matchMaskRanges(v, m, n, len(ranges))
				if exact == nil || n3.Cmp(count) != 0 {
					t.Fatalf("value=%08b mask=%08b: at-limit call failed", value, mask)
				}
			}
		}
	}
}

func bitCount8(x int) int {
	c := 0
	for x != 0 {
		c += x & 1
		x >>= 1
	}
	return c
}

func trailingZeros8(x int) int {
	if x == 0 {
		return 8
	}
	c := 0
	for x&1 == 0 {
		c++
		x >>= 1
	}
	return c
}

func rangesContain128(rr []matchRange, x uint128) bool {
	for _, r := range rr {
		if cmp128(r.lo, x) <= 0 && cmp128(x, r.hi) <= 0 {
			return true
		}
	}
	return false
}

func cmp128(a, b uint128) int {
	switch {
	case a.hi < b.hi || (a.hi == b.hi && a.lo < b.lo):
		return -1
	case a == b:
		return 0
	default:
		return 1
	}
}

// TestIPSetFromMatchMaskIPv4 verifies the real IPv4 path with the
// non-contiguous mask 255.0.255.0, which matches the addresses
// A.*.C.* (65536 addresses) in 256 ranges of 256 addresses each.
func TestIPSetFromMatchMaskIPv4(t *testing.T) {
	addr := mustIP("1.2.3.4")
	mask := mustIP("255.0.255.0")

	s, err := IPSetFromMatchMask(addr, mask, 1000)
	if err != nil {
		t.Fatalf("IPSetFromMatchMask: %v", err)
	}
	ranges := s.Ranges()
	if len(ranges) != 256 {
		t.Fatalf("got %d ranges, want 256", len(ranges))
	}

	// Exact membership for all 256*256 combinations of the fixed
	// octets, with the second and fourth (unconstrained) octets set to
	// representative values.
	for first := 0; first < 256; first++ {
		for third := 0; third < 256; third++ {
			want := first == 1 && third == 3
			for _, second := range []int{0, 42, 255} {
				for _, fourth := range []int{0, 128, 255} {
					ip := mustIP(itoa4(first, second, third, fourth))
					if got := s.Contains(ip); got != want {
						t.Fatalf("Contains(%v)=%v want %v", ip, got, want)
					}
				}
			}
		}
	}
	// Unconstrained fourth octet: a whole 256-wide block is present.
	for fourth := 0; fourth < 256; fourth++ {
		if !s.Contains(mustIP(itoa4(1, 17, 3, fourth))) {
			t.Fatalf("missing 1.17.3.%d", fourth)
		}
	}

	// Check the exact structure: 256 sorted, adjacent-free blocks of
	// width 256, at 1.b.3.0-1.b.3.255.
	prevTo := netip.Addr{}
	for i, r := range ranges {
		from, to := r.From(), r.To()
		if from.String() != itoa4(1, i, 3, 0) || to.String() != itoa4(1, i, 3, 255) {
			t.Fatalf("range %d = %v-%v, want 1.%d.3.0-1.%d.3.255", i, from, to, i, i)
		}
		if i > 0 && !prevTo.Less(from) {
			t.Fatalf("range %d not strictly after previous", i)
		}
		prevTo = to
	}

	// Unconstrained bits of the value must be ignored: 1.9.3.9 with
	// the same mask yields the same set.
	s2, err := IPSetFromMatchMask(mustIP("1.9.3.9"), mask, 1000)
	if err != nil {
		t.Fatalf("IPSetFromMatchMask: %v", err)
	}
	if !s.Equal(s2) {
		t.Fatal("unconstrained value bits were not ignored")
	}

	// Over the limit: exact count and no construction.
	var b IPSetBuilder
	count, err := b.AddMatchMask(addr, mask, 255)
	if !errors.Is(err, ErrMatchMaskTooManyRanges) {
		t.Fatalf("err=%v want ErrMatchMaskTooManyRanges", err)
	}
	if count.Cmp(big.NewInt(256)) != 0 {
		t.Fatalf("count=%v want 256", count)
	}
	if set, _ := b.IPSet(); !set.Equal(&IPSet{}) {
		t.Fatal("builder was modified despite over-limit error")
	}
}

func itoa4(a, b, c, d int) string {
	const digits = "0123456789"
	oct := func(n int) string {
		if n == 0 {
			return "0"
		}
		var buf [3]byte
		i := len(buf)
		for n > 0 {
			i--
			buf[i] = digits[n%10]
			n /= 10
		}
		return string(buf[i:])
	}
	return oct(a) + "." + oct(b) + "." + oct(c) + "." + oct(d)
}

// TestIPSetFromMatchMaskIPv6 covers the extreme scattered fixed-bit
// case and the over-limit low-allocation path.
func TestIPSetFromMatchMaskIPv6(t *testing.T) {
	// Bit 127 and bits 11..126 are fixed, with ten free bits (1..10)
	// clustered above the fixed LSB. The matching set is 1024 isolated
	// addresses 8000::(2k+1) spread across a huge block, each its own
	// range.
	addr := mustIP("8000::1")
	mask := mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:f801")
	s, err := IPSetFromMatchMask(addr, mask, 2000)
	if err != nil {
		t.Fatalf("IPSetFromMatchMask: %v", err)
	}
	ranges := s.Ranges()
	if len(ranges) != 1024 {
		t.Fatalf("got %d ranges, want 1024", len(ranges))
	}
	for i, r := range ranges {
		if r.From() != r.To() {
			t.Fatalf("range %d = %v is not a singleton", i, r)
		}
		if i > 0 && !ranges[i-1].To().Less(r.From()) {
			t.Fatalf("range %d out of order", i)
		}
	}
	first, last := ranges[0].From(), ranges[len(ranges)-1].From()
	if first != mustIP("8000::1") || last != mustIP("8000::7ff") {
		t.Fatalf("extremes = %v .. %v", first, last)
	}
	if !s.Contains(mustIP("8000::3")) || s.Contains(mustIP("8000::2")) || s.Contains(mustIP("8001::1")) {
		t.Fatal("membership incorrect for scattered mask")
	}

	// Over the limit returns the exact count and builds nothing.
	var b IPSetBuilder
	count, err := b.AddMatchMask(addr, mask, 1023)
	if !errors.Is(err, ErrMatchMaskTooManyRanges) {
		t.Fatalf("err=%v want ErrMatchMaskTooManyRanges", err)
	}
	if count.Cmp(big.NewInt(1024)) != 0 {
		t.Fatalf("count=%v want 1024", count)
	}

	// Extreme: only the lowest bit fixed. Half the address space is
	// matched as 2^127 singleton ranges. The over-limit path must stay
	// cheap: one big.Int and no result slices.
	lsbAddr := mustIP("::1")
	lsbMask := mustIP("::1")
	want127 := new(big.Int).Lsh(big.NewInt(1), 127)
	allocs := testing.AllocsPerRun(100, func() {
		c, e := b.AddMatchMask(lsbAddr, lsbMask, 1<<20)
		if !errors.Is(e, ErrMatchMaskTooManyRanges) {
			t.Fatalf("err=%v", e)
		}
		if c.Cmp(want127) != 0 {
			t.Fatalf("count=%v want 2^127", c)
		}
	})
	if allocs > 3 {
		t.Fatalf("over-limit path allocated %.1f times, want <= 3", allocs)
	}
	if set, _ := b.IPSet(); !set.Equal(&IPSet{}) {
		t.Fatal("builder modified despite over-limit errors")
	}

	// Highly scattered fixed bits with alternating mask: free bits are
	// everywhere, so the exact 2^64 count is reported.
	var altMask uint128
	for i := uint(0); i < 128; i += 2 {
		altMask = altMask.or(numericBit128(i))
	}
	_, n := matchMaskRanges(uint128{}, altMask, 128, 1<<20)
	want := new(big.Int).Lsh(big.NewInt(1), 64)
	if n.Cmp(want) != 0 {
		t.Fatalf("alternating mask count=%v want 2^64", n)
	}
}

func TestIPSetFromMatchMaskSpecial(t *testing.T) {
	fullV4, err := IPSetFromMatchMask(mustIP("1.2.3.4"), mustIP("0.0.0.0"), 1)
	if err != nil {
		t.Fatal(err)
	}
	wantV4 := &IPSetBuilder{}
	wantV4.AddPrefix(mustIPPrefix("0.0.0.0/0"))
	if !fullV4.Equal(buildIPSet(wantV4)) {
		t.Fatal("all-zero IPv4 mask should match all IPv4")
	}

	fullV6, err := IPSetFromMatchMask(mustIP("2001:db8::1"), mustIP("::"), 1)
	if err != nil {
		t.Fatal(err)
	}
	wantV6 := &IPSetBuilder{}
	wantV6.AddPrefix(mustIPPrefix("::/0"))
	if !fullV6.Equal(buildIPSet(wantV6)) {
		t.Fatal("all-zero IPv6 mask should match all IPv6")
	}

	single, err := IPSetFromMatchMask(mustIP("2001:db8::abcd"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Ranges()) != 1 || !single.Contains(mustIP("2001:db8::abcd")) || single.Contains(mustIP("2001:db8::abce")) {
		t.Fatal("all-one mask should match exactly one address")
	}
}

func TestAddMatchMaskErrors(t *testing.T) {
	tests := []struct {
		name string
		addr netip.Addr
		mask netip.Addr
		max  int
	}{
		{"invalid_addr", netip.Addr{}, mustIP("0.0.0.0"), 1},
		{"invalid_mask", mustIP("1.2.3.4"), netip.Addr{}, 1},
		{"addr_zone", mustIP("fe80::1%eth0"), mustIP("::"), 1},
		{"mask_zone", mustIP("fe80::1"), mustIP("::%eth0"), 1},
		{"family_mismatch", mustIP("1.2.3.4"), mustIP("::"), 1},
		{"family_mismatch_reverse", mustIP("::1"), mustIP("0.0.0.0"), 1},
		{"zero_max", mustIP("1.2.3.4"), mustIP("255.0.0.0"), 0},
		{"negative_max", mustIP("1.2.3.4"), mustIP("255.0.0.0"), -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b IPSetBuilder
			count, err := b.AddMatchMask(tc.addr, tc.mask, tc.max)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if count != nil {
				t.Fatalf("count=%v, want nil on non-limit error", count)
			}
			if set, _ := b.IPSet(); !set.Equal(&IPSet{}) {
				t.Fatal("builder modified by failed call")
			}
			if _, err := IPSetFromMatchMask(tc.addr, tc.mask, tc.max); err == nil {
				t.Fatal("IPSetFromMatchMask: want error, got nil")
			}
		})
	}
}

func TestAddMatchMaskCombines(t *testing.T) {
	var b IPSetBuilder
	if _, err := b.AddMatchMask(mustIP("10.0.0.0"), mustIP("255.0.0.0"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddMatchMask(mustIP("192.168.0.0"), mustIP("255.255.0.0"), 100); err != nil {
		t.Fatal(err)
	}
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Contains(mustIP("10.9.9.9")) {
		t.Error("missing 10.9.9.9")
	}
	if s.Contains(mustIP("11.9.9.9")) {
		t.Error("unexpected 11.9.9.9")
	}
	if !s.Contains(mustIP("192.168.4.5")) {
		t.Error("missing 192.168.4.5")
	}
}
