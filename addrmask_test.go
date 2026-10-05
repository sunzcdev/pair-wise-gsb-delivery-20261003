// Copyright 2023 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"errors"
	"math/big"
	"math/rand"
	"net/netip"
	"testing"
)

// u8Range is a minimal inclusive integer range used by the 8-bit
// exhaustive simulation.
type u8Range struct{ lo, hi uint8 }

// walkU8 runs the production expansion core over an 8-bit space and
// returns the ranges it produces.
func walkU8(base, mask uint8) []u8Range {
	var got []u8Range
	walkMaskRanges(uint128{0, uint64(base)}, uint128{0, uint64(mask)}, 8, func(lo, hi uint128) {
		got = append(got, u8Range{uint8(lo.lo), uint8(hi.lo)})
	})
	return got
}

// matches reports whether v satisfies the bit-match predicate
// (v & mask) == base.
func matches(v, base, mask uint8) bool { return v&mask == base }

// TestAddrMask8BitExhaustive enumerates every (base, mask) pair in an
// 8-bit simulated space and checks the range expansion against the
// direct bit-match predicate, plus the closed-form range count.
func TestAddrMask8BitExhaustive(t *testing.T) {
	for mask := 0; mask < 256; mask++ {
		// Only normalized bases are used by real AddrMasks, but
		// exercising all bases confirms wildcard bits in the
		// value are ignored.
		for baseRaw := 0; baseRaw < 256; baseRaw++ {
			m := uint8(mask)
			base := uint8(baseRaw) & m
			got := walkU8(base, m)
			wantCount := nBitRangeCount(uint128{0, uint64(m)}, 8)
			if len(got) != int(wantCount.Int64()) {
				t.Fatalf("base=%08b mask=%08b: got %d ranges, want %s", base, m, len(got), wantCount)
			}
			if len(got) == 0 {
				t.Fatalf("base=%08b mask=%08b: no ranges emitted", base, m)
			}
			var prev u8Range
			for i, r := range got {
				if r.lo > r.hi {
					t.Fatalf("base=%08b mask=%08b: inverted range [%d,%d]", base, m, r.lo, r.hi)
				}
				if i > 0 {
					if r.lo <= prev.hi {
						t.Fatalf("base=%08b mask=%08b: ranges overlap/unsorted: [%d,%d] after [%d,%d]", base, m, r.lo, r.hi, prev.lo, prev.hi)
					}
					if r.lo <= prev.hi+1 {
						t.Fatalf("base=%08b mask=%08b: adjacent ranges should have merged: [%d,%d],[%d,%d]", base, m, prev.lo, prev.hi, r.lo, r.hi)
					}
				}
				prev = r
			}
			// Every address must be classified correctly:
			// covered by exactly one range iff the predicate
			// matches, and ranges must contain no non-matching
			// addresses.
			ri := 0
			for v := 0; v < 256; v++ {
				vv := uint8(v)
				for ri < len(got) && got[ri].hi < vv {
					ri++
				}
				inRange := ri < len(got) && got[ri].lo <= vv && vv <= got[ri].hi
				if want := matches(vv, base, m); inRange != want {
					t.Fatalf("base=%08b mask=%08b v=%08b: range membership %v, predicate %v", base, m, vv, inRange, want)
				}
			}
		}
	}
}

func TestAddrMaskCountEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		addr string
		mask string
		want string // big.Int decimal
	}{
		{"v4 zero mask", "192.168.1.1", "0.0.0.0", "1"},
		{"v4 full mask", "1.2.3.4", "255.255.255.255", "1"},
		{"v4 /24", "10.0.0.5", "255.255.255.0", "1"},
		{"v4 discontiguous", "255.0.255.0", "255.0.255.0", "256"},
		{"v4 top and bottom fixed", "128.0.0.1", "128.0.0.1", "1073741824"},
		{"v6 zero mask", "2001:db8::1", "::", "1"},
		{"v6 full mask", "2001:db8::1", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "1"},
		{"v6 /64", "2001:db8::", "ffff:ffff:ffff:ffff::", "1"},
		{"v6 alternating", "::", alternatingMask(), new(big.Int).Lsh(big.NewInt(1), 64).String()},
		{"v6 bottom fixed", "::1", "::1", new(big.Int).Lsh(big.NewInt(1), 127).String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			am := MustAddrMaskFrom(mustIP(tt.addr), mustIP(tt.mask))
			got := am.RangeCount()
			want, ok := new(big.Int).SetString(tt.want, 10)
			if !ok {
				t.Fatalf("bad want %q", tt.want)
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("RangeCount = %s, want %s", got, want)
			}
		})
	}
}

// alternatingMask returns an IPv6 mask with 64 fixed bits at every
// other bit position (0x55...), including the least significant bit,
// forcing 2**64 ranges.
func alternatingMask() string {
	var b [16]byte
	for i := 0; i < 16; i++ {
		b[i] = 0x55 // bits 01010101
	}
	return netip.AddrFrom16(b).String()
}

// TestAddrMaskIPv4Discontiguous verifies the 255.0.255.0 pattern with
// real netip addresses: wildcard bits of the value are normalized,
// the predicate is exact, and the set expands to the right number of
// ranges.
func TestAddrMaskIPv4Discontiguous(t *testing.T) {
	// Value carries junk in wildcard positions; it must be ignored.
	am, err := AddrMaskFrom(mustIP("10.250.30.99"), mustIP("255.0.255.0"))
	if err != nil {
		t.Fatal(err)
	}
	if got := am.Addr().String(); got != "10.0.30.0" {
		t.Fatalf("normalized addr = %q, want 10.0.30.0", got)
	}
	if !am.Contains(mustIP("10.9.30.200")) {
		t.Errorf("should contain 10.9.30.200")
	}
	if !am.Contains(mustIP("10.0.30.0")) {
		t.Errorf("should contain 10.0.30.0")
	}
	if !am.Contains(mustIP("10.255.30.255")) {
		t.Errorf("should contain 10.255.30.255")
	}
	if am.Contains(mustIP("11.0.30.0")) {
		t.Errorf("should not contain 11.0.30.0")
	}
	if am.Contains(mustIP("10.0.31.0")) {
		t.Errorf("should not contain 10.0.31.0")
	}
	if am.Contains(mustIP("9.255.30.255")) {
		t.Errorf("should not contain 9.255.30.255")
	}
	if am.Contains(mustIP("::ffff:10.0.30.1")) {
		t.Errorf("Contains must not treat IPv4-mapped v6 as v4")
	}

	// Unlimited construction: 256 ranges of 65536 addresses each.
	s, count, err := am.IPSet(-1)
	if err != nil {
		t.Fatal(err)
	}
	if count.Int64() != 256 {
		t.Fatalf("count = %s, want 256", count)
	}
	if got := len(s.Ranges()); got != 256 {
		t.Fatalf("len(Ranges) = %d, want 256", got)
	}
	first := s.Ranges()[0]
	if first.From() != mustIP("10.0.30.0") || first.To() != mustIP("10.0.30.255") {
		t.Fatalf("first range = %v", first)
	}
	// The fourth octet is entirely wildcard, so the first range
	// spans 10.0.30.0-10.0.30.255. The last spans 10.255.30.*.
	last := s.Ranges()[255]
	if last.From() != mustIP("10.255.30.0") || last.To() != mustIP("10.255.30.255") {
		t.Fatalf("last range = %v", last)
	}
	for b2 := 0; b2 < 256; b2++ {
		lo := netip.AddrFrom4([4]byte{10, byte(b2), 30, 0})
		hi := netip.AddrFrom4([4]byte{10, byte(b2), 30, 255})
		if !s.Contains(lo) || !s.Contains(hi) {
			t.Fatalf("missing range around %v", lo)
		}
	}
	if s.Contains(mustIP("10.0.31.0")) {
		t.Fatalf("unexpected addr in third octet 31")
	}

	// A limit exactly at the count succeeds; one below does not.
	if _, _, err := am.IPSet(256); err != nil {
		t.Fatalf("IPSet(256) errored: %v", err)
	}
	s2, count2, err2 := am.IPSet(255)
	if s2 != nil {
		t.Fatalf("IPSet(255) returned non-nil set")
	}
	if count2.Int64() != 256 || !errors.Is(err2, ErrTooManyRanges) {
		t.Fatalf("IPSet(255) = count %v, err %v", count2, err2)
	}
}

// TestAddrMaskIPv6Extreme verifies an extremely scattered fixed-bit
// mask in IPv6: the exact count, rejection without constructing the
// huge result and with low allocation, and that a constructible
// sibling of similar shape produces an exact set.
func TestAddrMaskIPv6Extreme(t *testing.T) {
	am := MustAddrMaskFrom(mustIP("::"), mustIP(alternatingMask()))
	want := new(big.Int).Lsh(big.NewInt(1), 64)
	if got := am.RangeCount(); got.Cmp(want) != 0 {
		t.Fatalf("RangeCount = %s, want %s", got, want)
	}

	// Any int bound is far below 2**64: the call must fail, return
	// the exact count, and allocate essentially nothing.
	allocs := testing.AllocsPerRun(100, func() {
		s, count, err := am.IPSet(1 << 30)
		if s != nil || !errors.Is(err, ErrTooManyRanges) || count.Cmp(want) != 0 {
			t.Fatalf("IPSet = %v, %s, %v", s, count, err)
		}
	})
	if allocs > 10 {
		t.Fatalf("rejected IPSet allocated %v times, want <= 10", allocs)
	}

	// Constructible sibling: fixed bits at trie indices 1 and 3 in
	// the top byte (mask 0x50), giving 2**2 = 4 ranges.
	sib := MustAddrMaskFrom(mustIP("4000::"), mustIP("5000::")) // 50 = 0101 0000
	if got := sib.RangeCount().Int64(); got != 4 {
		t.Fatalf("sibling RangeCount = %d, want 4", got)
	}
	s, _, err := sib.IPSet(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Ranges()) != 4 {
		t.Fatalf("sibling ranges = %d, want 4", len(s.Ranges()))
	}
	ranges := s.Ranges()
	// Derive expected ranges explicitly: fixed bits at trie
	// indices 1 and 3 equal the value bits (1 and 0): patterns
	// ?1?0 in the top nibble, remainder wildcard.
	for i, wantStart := range []string{"4000::", "6000::", "c000::", "e000::"} {
		if ranges[i].From() != mustIP(wantStart) {
			t.Errorf("range %d from = %v, want %v", i, ranges[i].From(), wantStart)
		}
		if !ranges[i].To().IsValid() || ranges[i].To().Compare(ranges[i].From()) < 0 {
			t.Errorf("range %d invalid: %v", i, ranges[i])
		}
	}
	for _, c := range []string{"::", "1::", "4000::", "4000::1", "5fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "6000::", "8000::", "a000::1", "c000::1", "e000::", "ffff::"} {
		ip := mustIP(c)
		if s.Contains(ip) != sib.Contains(ip) {
			t.Errorf("Contains(%s): set %v mask %v", c, s.Contains(ip), sib.Contains(ip))
		}
	}
}

// TestAddrMaskZeroAndFull verifies the all-zero mask (whole address
// space) and all-one mask (single address) for both families.
func TestAddrMaskZeroAndFull(t *testing.T) {
	// IPv4 whole space.
	all4 := MustAddrMaskFrom(mustIP("1.2.3.4"), mustIP("0.0.0.0"))
	if all4.RangeCount().Int64() != 1 {
		t.Fatalf("v4 zero mask count = %s", all4.RangeCount())
	}
	s4, _, err := all4.IPSet(-1)
	if err != nil {
		t.Fatal(err)
	}
	if got := s4.Ranges(); len(got) != 1 ||
		got[0].From() != mustIP("0.0.0.0") || got[0].To() != mustIP("255.255.255.255") {
		t.Fatalf("v4 whole space = %v", got)
	}
	if !s4.Contains(mustIP("0.0.0.0")) || !s4.Contains(mustIP("255.255.255.255")) {
		t.Fatal("v4 whole space misses an edge")
	}
	if s4.Contains(mustIP("::1")) {
		t.Fatal("v4 whole space must not contain v6")
	}

	// IPv6 whole space.
	all6 := MustAddrMaskFrom(mustIP("2001:db8::1"), mustIP("::"))
	s6, _, err := all6.IPSet(-1)
	if err != nil {
		t.Fatal(err)
	}
	if got := s6.Ranges(); len(got) != 1 ||
		got[0].From() != mustIP("::") || got[0].To() != mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff") {
		t.Fatalf("v6 whole space = %v", got)
	}
	if !s6.Contains(mustIP("::1")) {
		t.Fatal("v6 whole space misses ::1")
	}

	// Single address.
	one4 := MustAddrMaskFrom(mustIP("192.0.2.10"), mustIP("255.255.255.255"))
	os4, _, err := one4.IPSet(-1)
	if err != nil {
		t.Fatal(err)
	}
	if got := os4.Ranges(); len(got) != 1 || got[0].From() != got[0].To() || got[0].From() != mustIP("192.0.2.10") {
		t.Fatalf("v4 single = %v", got)
	}

	one6 := MustAddrMaskFrom(mustIP("2001:db8::abcd"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"))
	os6, _, err := one6.IPSet(-1)
	if err != nil {
		t.Fatal(err)
	}
	if got := os6.Ranges(); len(got) != 1 || got[0].From() != got[0].To() || got[0].From() != mustIP("2001:db8::abcd") {
		t.Fatalf("v6 single = %v", got)
	}
}

// TestAddrMaskValidation covers invalid addresses, family mismatch,
// and zone rejection for both the address and mask.
func TestAddrMaskValidation(t *testing.T) {
	tests := []struct {
		name string
		addr netip.Addr
		mask netip.Addr
	}{
		{"invalid addr", netip.Addr{}, mustIP("255.255.255.255")},
		{"invalid mask", mustIP("1.2.3.4"), netip.Addr{}},
		{"family mismatch", mustIP("1.2.3.4"), mustIP("ffff::")},
		{"family mismatch reverse", mustIP("::1"), mustIP("255.0.0.0")},
		{"addr zone", mustIP("fe80::1%eth0"), mustIP("ffff:ffff::")},
		{"mask zone", mustIP("fe80::1"), mustIP("ffff:ffff::%eth0")},
		{"4-in-6 addr rejected", mustIP("::ffff:1.2.3.4"), mustIP("::")},
		{"4-in-6 mask rejected", mustIP("::"), mustIP("::ffff:255.0.0.0")},
		{"unmapped v4 vs v6", mustIP("::ffff:1.2.3.4").Unmap(), mustIP("::")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if am, err := AddrMaskFrom(tt.addr, tt.mask); err == nil {
				t.Fatalf("AddrMaskFrom(%v, %v) = %+v, want error", tt.addr, tt.mask, am)
			}
		})
	}

	// An explicit Unmap on a 4-in-6 address yields a normal v4
	// AddrMask.
	am, err := AddrMaskFrom(mustIP("::ffff:1.2.3.4").Unmap(), mustIP("255.0.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if !am.Is4() || am.Addr() != mustIP("1.0.0.0") {
		t.Fatalf("mapped handling: %v %v", am.Is4(), am.Addr())
	}

	// The zero value is invalid.
	var zero AddrMask
	if zero.Valid() || zero.RangeCount() != nil || zero.Contains(mustIP("1.2.3.4")) {
		t.Fatal("zero AddrMask must be inert")
	}
	if _, _, err := zero.IPSet(-1); err == nil {
		t.Fatal("zero AddrMask IPSet must error")
	}
}

// TestAddrMaskRandomIPv4 builds random masks and checks every emitted
// range against the direct predicate, along with count and set
// agreement, using masks that keep the count constructible.
func TestAddrMaskRandomIPv4(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 500; iter++ {
		// Fixed bits only in the top 12 positions, so the count
		// never exceeds 2**12 = 4096 and the set stays small.
		m := rng.Uint32() & 0xFFF0_0000
		a := rng.Uint32() & m
		am := MustAddrMaskFrom(uint32IP(a), uint32IP(m))

		count := am.RangeCount()
		if !count.IsInt64() || count.Int64() > 4096 {
			t.Fatalf("iter %d: count %s unexpectedly large", iter, count)
		}
		s, _, err := am.IPSet(-1)
		if err != nil {
			t.Fatal(err)
		}
		ranges := s.Ranges()
		if int64(len(ranges)) != count.Int64() {
			t.Fatalf("iter %d: %d ranges vs count %s", iter, len(ranges), count)
		}
		for i, r := range ranges {
			if !r.IsValid() || r.From().Compare(r.To()) > 0 {
				t.Fatalf("iter %d: bad range %v", iter, r)
			}
			if i > 0 && !ranges[i-1].To().Next().Less(r.From()) {
				t.Fatalf("iter %d: ranges not minimal at %d", iter, i)
			}
			// Endpoints satisfy the predicate.
			if !am.Contains(r.From()) || !am.Contains(r.To()) {
				t.Fatalf("iter %d: range endpoint not in mask: %v", iter, r)
			}
			// Just outside each endpoint must not match, except at
			// the absolute edges of the address space.
			if prev := AddrPrior(r.From()); prev.IsValid() && am.Contains(prev) {
				t.Fatalf("iter %d: predecessor %v of %v unexpectedly matches", iter, prev, r.From())
			}
			if next := r.To().Next(); next.IsValid() && am.Contains(next) {
				t.Fatalf("iter %d: successor %v of %v unexpectedly matches", iter, next, r.To())
			}
		}
		// Random membership probes.
		for k := 0; k < 64; k++ {
			ip := uint32IP(rng.Uint32())
			if s.Contains(ip) != am.Contains(ip) {
				t.Fatalf("iter %d: mismatch at %v", iter, ip)
			}
		}
	}
}
