// Copyright 2026 The Go4 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"math/big"
	"math/rand"
	"net/netip"
	"reflect"
	"testing"
)

// ---- small helpers -------------------------------------------------------

func mustPrefixSet(prefixes ...string) *IPSet {
	var b IPSetBuilder
	for _, p := range prefixes {
		b.AddPrefix(mustIPPrefix(p))
	}
	return buildIPSet(&b)
}

func mustMap(t *testing.T, b *PrefixMapBuilder, in *IPSet) *PrefixMapResult {
	t.Helper()
	res, err := b.Map(in)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	return res
}

func rangeStrings(s *IPSet) []string {
	var out []string
	for _, r := range s.Ranges() {
		out = append(out, r.String())
	}
	return out
}

func assertRanges(t *testing.T, s *IPSet, want []string) {
	t.Helper()
	got := rangeStrings(s)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranges:\n got %v\nwant %v", got, want)
	}
}

func assertCounts(t *testing.T, got []big.Int, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("count length: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		g := got[i]
		if n := g.Int64(); n != want[i] {
			t.Errorf("count[%d] = %d; want %d", i, n, want[i])
		}
	}
}

func assertUnmatched(t *testing.T, r *PrefixMapResult, want int64) {
	t.Helper()
	u := r.Unmatched()
	if n := u.Int64(); n != want {
		t.Errorf("Unmatched = %d; want %d", n, want)
	}
}

// ---- fixed scenarios -----------------------------------------------------

// TestPrefixMapParentChild covers nested source prefixes: the most
// specific matching prefix wins, addresses only a parent covers are
// still mapped by the parent, and unmatched addresses are dropped or
// kept. It also checks that the input set is not mutated.
func TestPrefixMapParentChild(t *testing.T) {
	var b PrefixMapBuilder
	// 24 host bits on both sides.
	b.AddRule(mustIPPrefix("10.0.0.0/8"), mustIPPrefix("2001:db8:a::/104"))
	// 16 host bits on both sides.
	b.AddRule(mustIPPrefix("10.1.0.0/16"), mustIPPrefix("2001:db8:b::/112"))

	in := mustPrefixSet(
		"10.0.128.0/25",  // 128 addrs, only rule 0
		"10.1.0.0/24",    // 256 addrs, rule 1 (more specific) wins
		"10.2.3.0/24",    // 256 addrs, only rule 0
		"192.168.0.0/24", // 256 addrs, no rule
	)
	before := rangeStrings(in)

	res := mustMap(t, &b, in)
	assertCounts(t, res.Mapped(), []int64{128 + 256, 256})
	assertUnmatched(t, res, 256)

	// IPv4 offsets live in the low 32 bits of the address space:
	// 10.0.128.0 offset 0x008000 -> 2001:db8:a::8000
	// 10.2.3.0   offset 0x020300 -> 2001:db8:a::2:0300
	// 10.1.0.0/24             -> 2001:db8:b::-2001:db8:b::ff
	assertRanges(t, res.Out(), []string{
		"2001:db8:a::8000-2001:db8:a::807f",
		"2001:db8:a::2:300-2001:db8:a::2:3ff",
		"2001:db8:b::-2001:db8:b::ff",
	})

	b.SetOnMiss(KeepOnMiss)
	resKeep := mustMap(t, &b, in)
	assertCounts(t, resKeep.Mapped(), []int64{128 + 256, 256})
	assertUnmatched(t, resKeep, 256)
	assertRanges(t, resKeep.Out(), []string{
		"192.168.0.0-192.168.0.255",
		"2001:db8:a::8000-2001:db8:a::807f",
		"2001:db8:a::2:300-2001:db8:a::2:3ff",
		"2001:db8:b::-2001:db8:b::ff",
	})

	if got := rangeStrings(in); !reflect.DeepEqual(got, before) {
		t.Errorf("input set changed:\n before %v\nafter  %v", before, got)
	}
}

// TestPrefixMapV4ToV6 maps IPv4 /24 and /26 prefixes into IPv6
// /120 and /122 prefixes, preserving the host offset, including
// across family boundaries.
func TestPrefixMapV4ToV6(t *testing.T) {
	var b PrefixMapBuilder
	b.AddRule(mustIPPrefix("192.168.1.0/24"), mustIPPrefix("fd00::1:0/120")) // 8 host bits

	in := mustPrefixSet(
		"192.168.1.0/25",   // 128 addrs
		"192.168.1.128/26", // 64 addrs
		"192.168.2.0/24",   // 256 addrs, unmatched
	)

	res := mustMap(t, &b, in)
	assertCounts(t, res.Mapped(), []int64{192})
	assertUnmatched(t, res, 256)
	// The two mapped pieces are adjacent and merge into one range.
	assertRanges(t, res.Out(), []string{
		"fd00::1:0-fd00::1:bf",
	})

	b.SetOnMiss(KeepOnMiss)
	resKeep := mustMap(t, &b, in)
	assertRanges(t, resKeep.Out(), []string{
		"192.168.2.0-192.168.2.255",
		"fd00::1:0-fd00::1:bf",
	})
}

// TestPrefixMapDestinationIntoSource verifies that destinations that
// fall into another rule's source prefix are not remapped: rules
// apply to the original input only.
func TestPrefixMapDestinationIntoSource(t *testing.T) {
	var b PrefixMapBuilder
	// Rule 0 rewrites 10.0.0.0/8 onto 11.0.0.0/8 (24 host bits each).
	b.AddRule(mustIPPrefix("10.0.0.0/8"), mustIPPrefix("11.0.0.0/8"))
	// Rule 1's source prefix is rule 0's destination block, so a
	// chained implementation would remap rule 0's output a second
	// time. Both ends have 24 host bits.
	b.AddRule(mustIPPrefix("11.0.0.0/8"), mustIPPrefix("127.0.0.0/8"))

	in := mustPrefixSet("10.1.2.3/32", "11.5.5.5/32", "8.8.8.8/32")
	res := mustMap(t, &b, in)

	// 10.1.2.3 wins the more specific rule 0 and becomes 11.1.2.3.
	// It must not be fed back into rule 1 (which would have
	// produced 127.1.2.3). 11.5.5.5 is an original input address
	// covered by rule 1, while 8.8.8.8 matches nothing.
	assertCounts(t, res.Mapped(), []int64{1, 1})
	assertUnmatched(t, res, 1)
	assertRanges(t, res.Out(), []string{
		"11.1.2.3-11.1.2.3",
		"127.5.5.5-127.5.5.5",
	})
	if res.Out().Contains(mustIP("127.1.2.3")) {
		t.Errorf("output address was remapped a second time: %v", mustIP("127.1.2.3"))
	}
}

// TestPrefixMapDestinationCollision verifies that two rules mapping
// onto the same destination prefix are merged with set semantics,
// while per-rule counts report the pre-merge numbers.
func TestPrefixMapDestinationCollision(t *testing.T) {
	var b PrefixMapBuilder
	b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("172.16.0.0/24"))
	b.AddRule(mustIPPrefix("192.168.0.0/24"), mustIPPrefix("172.16.0.0/24"))

	in := mustPrefixSet("10.0.0.0/24", "192.168.0.0/24")
	res := mustMap(t, &b, in)

	assertCounts(t, res.Mapped(), []int64{256, 256})
	assertUnmatched(t, res, 0)
	assertRanges(t, res.Out(), []string{
		"172.16.0.0-172.16.0.255",
	})
}

// TestPrefixMapPartialDestinationOverlap verifies merging of
// partially overlapping destination ranges.
func TestPrefixMapDestinationPartialOverlap(t *testing.T) {
	var b PrefixMapBuilder
	// /25 -> /25 (7 host bits), /26 -> /26 (6 host bits).
	b.AddRule(mustIPPrefix("10.0.0.0/25"), mustIPPrefix("172.16.0.128/25"))
	b.AddRule(mustIPPrefix("192.168.0.0/26"), mustIPPrefix("172.16.0.192/26"))

	in := mustPrefixSet("10.0.0.0/25", "192.168.0.0/26", "8.8.8.8/32")
	res := mustMap(t, &b, in)

	assertCounts(t, res.Mapped(), []int64{128, 64})
	assertUnmatched(t, res, 1)
	// Destinations are .128-.255 and .192-.255, union .128-.255.
	assertRanges(t, res.Out(), []string{
		"172.16.0.128-172.16.0.255",
	})

	b.SetOnMiss(KeepOnMiss)
	resKeep := mustMap(t, &b, in)
	assertRanges(t, resKeep.Out(), []string{
		"8.8.8.8-8.8.8.8",
		"172.16.0.128-172.16.0.255",
	})
}

// TestPrefixMapMaxLastAddress exercises mapping all the way to the
// last IPv6 address and confirms the arithmetic does not overflow.
func TestPrefixMapMaxLastAddress(t *testing.T) {
	var b PrefixMapBuilder
	// 255.255.255.0/24 -> the last /120 of IPv6 space.
	b.AddRule(
		mustIPPrefix("255.255.255.0/24"),
		mustIPPrefix("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00/120"),
	)
	// An identity IPv6 rule also maps the last address onto itself,
	// colliding with the first rule's final output.
	b.AddRule(mustIPPrefix("::/0"), mustIPPrefix("::/0"))

	in := mustPrefixSet(
		"255.255.255.0/24",
		"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128",
	)
	res := mustMap(t, &b, in)

	assertCounts(t, res.Mapped(), []int64{256, 1})
	assertUnmatched(t, res, 0)
	assertRanges(t, res.Out(), []string{
		"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
	})
	if !res.Out().Contains(mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")) {
		t.Errorf("max IPv6 address missing from output")
	}
}

// TestPrefixMapErrors checks rule validation.
func TestPrefixMapErrors(t *testing.T) {
	t.Run("host bits mismatch", func(t *testing.T) {
		var b PrefixMapBuilder
		b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8::/128"))
		res, err := b.Map(mustPrefixSet("10.0.0.0/24"))
		if err == nil || res != nil {
			t.Fatalf("want error and nil result, got res=%v err=%v", res, err)
		}
	})

	t.Run("duplicate source", func(t *testing.T) {
		var b PrefixMapBuilder
		b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8:a::/120"))
		b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8:b::/120"))
		if _, err := b.Map(mustPrefixSet("10.0.0.0/24")); err == nil {
			t.Fatalf("want duplicate-source error")
		}
	})

	t.Run("invalid prefix", func(t *testing.T) {
		var b PrefixMapBuilder
		b.AddRule(netip.Prefix{}, mustIPPrefix("2001:db8::/120"))
		if _, err := b.Map(mustPrefixSet("10.0.0.0/24")); err == nil {
			t.Fatalf("want invalid-prefix error")
		}
	})

	t.Run("invalid on-miss policy", func(t *testing.T) {
		var b PrefixMapBuilder
		b.SetOnMiss(OnMissPolicy(42))
		if _, err := b.Map(mustPrefixSet("10.0.0.0/24")); err == nil {
			t.Fatalf("want invalid-policy error")
		}
	})

	t.Run("recovered builder", func(t *testing.T) {
		var b PrefixMapBuilder
		b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8::/128"))
		_, _ = b.Map(mustPrefixSet("10.0.0.0/24")) // drains the error
		b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8::/120"))
		res := mustMap(t, &b, mustPrefixSet("10.0.0.0/24"))
		assertCounts(t, res.Mapped(), []int64{256})
	})
}

// TestPrefixMapEmpty verifies nil and empty inputs and a builder
// with no rules.
func TestPrefixMapEmpty(t *testing.T) {
	var b PrefixMapBuilder
	b.AddRule(mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8::/120"))

	res := mustMap(t, &b, nil)
	assertCounts(t, res.Mapped(), []int64{0})
	assertUnmatched(t, res, 0)
	if len(res.Out().Ranges()) != 0 {
		t.Errorf("want empty output, got %v", res.Out().Ranges())
	}

	empty := buildIPSet(&IPSetBuilder{})
	res = mustMap(t, &b, empty)
	assertCounts(t, res.Mapped(), []int64{0})

	var noRules PrefixMapBuilder
	in := mustPrefixSet("10.0.0.0/24")

	res = mustMap(t, &noRules, in)
	assertUnmatched(t, res, 256)
	if len(res.Out().Ranges()) != 0 {
		t.Errorf("drop: want empty output, got %v", res.Out().Ranges())
	}

	noRules.SetOnMiss(KeepOnMiss)
	res = mustMap(t, &noRules, in)
	assertUnmatched(t, res, 256)
	assertRanges(t, res.Out(), []string{"10.0.0.0-10.0.0.255"})
}

// TestPrefixMapHugeCount verifies exact counts that exceed 64 bits,
// including the entire 128-bit IPv6 space (2^128 addresses).
func TestPrefixMapHugeCount(t *testing.T) {
	var b PrefixMapBuilder
	b.AddRule(mustIPPrefix("::/0"), mustIPPrefix("::/0"))

	in := mustPrefixSet("::/0")
	res := mustMap(t, &b, in)

	wantMapped := new(big.Int).Lsh(big.NewInt(1), 128)
	if res.Mapped()[0].Cmp(wantMapped) != 0 {
		t.Errorf("Mapped[0] = %s; want %s", &res.Mapped()[0], wantMapped)
	}
	u := res.Unmatched()
	if u.Sign() != 0 {
		t.Errorf("Unmatched = %s; want 0", &u)
	}
	assertRanges(t, res.Out(), []string{
		"::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
	})
}

// ---- randomized cross-check against a per-address reference --------------

func addrInt(a netip.Addr) *big.Int {
	var b []byte
	if a.Is4() {
		x := a.As4()
		b = x[:]
	} else {
		x := a.As16()
		b = x[:]
	}
	return new(big.Int).SetBytes(b)
}

func intAddr(n *big.Int, is4 bool) netip.Addr {
	raw := n.Bytes()
	if is4 {
		var x [4]byte
		copy(x[4-len(raw):], raw)
		return netip.AddrFrom4(x)
	}
	var x [16]byte
	copy(x[16-len(raw):], raw)
	return netip.AddrFrom16(x)
}

// refMap is an independent, per-address reference implementation of
// the prefix mapping semantics.
type refRule struct {
	src netip.Prefix
	dst netip.Prefix
}

func refMapSet(t *testing.T, rules []refRule, in *IPSet, keep bool) (*IPSet, []int, int) {
	t.Helper()
	counts := make([]int, len(rules))
	unmatched := 0
	var out IPSetBuilder

	for _, r := range in.Ranges() {
		for addr := r.From(); ; addr = addr.Next() {
			best := -1
			for i, rule := range rules {
				ruleSrc := rule.src
				if ruleSrc.Addr().BitLen() != addr.BitLen() {
					continue
				}
				if ruleSrc.Contains(addr) &&
					(best == -1 || ruleSrc.Bits() > rules[best].src.Bits()) {
					best = i
				}
			}
			switch {
			case best >= 0:
				counts[best]++
				rule := rules[best]
				offset := new(big.Int).Sub(addrInt(addr), addrInt(rule.src.Addr()))
				mapped := new(big.Int).Add(addrInt(rule.dst.Addr()), offset)
				out.Add(intAddr(mapped, rule.dst.Addr().Is4()))
			default:
				unmatched++
				if keep {
					out.Add(addr)
				}
			}
			if addr == r.To() {
				break
			}
		}
	}

	s, err := out.IPSet()
	if err != nil {
		t.Fatalf("reference set: %v", err)
	}
	return s, counts, unmatched
}

func TestPrefixMapReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	const iterations = 100
	for iter := 0; iter < iterations; iter++ {
		var b PrefixMapBuilder
		var rules []refRule

		nRules := 1 + rng.Intn(5)
		for k := 0; k < nRules; k++ {
			// Source prefixes live inside 10.11.0.0/16 and have
			// 4..8 host bits, keeping every generated block small.
			hostBits := 4 + rng.Intn(5)
			srcHost := rng.Intn(1<<(16-hostBits)) << uint(hostBits)
			src := netip.PrefixFrom(
				netip.AddrFrom4([4]byte{10, 11, byte(srcHost >> 8), byte(srcHost)}),
				32-hostBits,
			).Masked()

			dup := false
			for _, ex := range rules {
				if ex.src == src {
					dup = true
					break
				}
			}
			if dup {
				continue
			}

			var dst netip.Prefix
			if rng.Intn(2) == 0 {
				dstHost := rng.Intn(1<<(8-hostBits)) << uint(hostBits)
				dst = netip.PrefixFrom(
					netip.AddrFrom4([4]byte{192, 168, 77, byte(dstHost)}),
					32-hostBits,
				).Masked()
			} else {
				dstHost := rng.Intn(1<<(8-hostBits)) << uint(hostBits)
				dst = netip.PrefixFrom(
					netip.AddrFrom16([16]byte{
						0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0x77,
						0, 0, 0, 0, 0, 0, 0, byte(dstHost),
					}),
					128-hostBits,
				).Masked()
			}

			b.AddRule(src, dst)
			rules = append(rules, refRule{src: src, dst: dst})
		}

		var inb IPSetBuilder
		nRanges := 1 + rng.Intn(3)
		for k := 0; k < nRanges; k++ {
			length := 1 + rng.Intn(64)
			var start int
			if rng.Intn(3) == 0 {
				// Outside every source prefix.
				start = 0x08080800 + rng.Intn(256-length)
			} else {
				start = 0x0a0b0000 + rng.Intn(65536-length)
			}
			fromN := big.NewInt(int64(start))
			toN := new(big.Int).Add(fromN, big.NewInt(int64(length-1)))
			from := intAddr(fromN, true)
			to := intAddr(toN, true)
			inb.AddRange(IPRangeFrom(from, to))
		}
		in := buildIPSet(&inb)
		before := rangeStrings(in)

		keep := rng.Intn(2) == 0
		if keep {
			b.SetOnMiss(KeepOnMiss)
		}

		res := mustMap(t, &b, in)
		refOut, refCounts, refUnmatched := refMapSet(t, rules, in, keep)

		if got := rangeStrings(res.Out()); !reflect.DeepEqual(got, rangeStrings(refOut)) {
			t.Fatalf("iter %d keep=%v\n  rules=%v\n  in=%v\n got %v\nwant %v",
				iter, keep, rules, before, got, rangeStrings(refOut))
		}
		if len(res.Mapped()) != len(refCounts) {
			t.Fatalf("iter %d count len got %d want %d", iter, len(res.Mapped()), len(refCounts))
		}
		for i, want := range refCounts {
			g := res.Mapped()[i]
			if n := g.Int64(); n != int64(want) {
				t.Fatalf("iter %d count[%d] got %d want %d", iter, i, n, want)
			}
		}
		u := res.Unmatched()
		if n := u.Int64(); n != int64(refUnmatched) {
			t.Fatalf("iter %d unmatched got %d want %d", iter, n, refUnmatched)
		}
		if got := rangeStrings(in); !reflect.DeepEqual(got, before) {
			t.Fatalf("iter %d input set changed:\n before %v\nafter  %v", iter, before, got)
		}
	}
}
