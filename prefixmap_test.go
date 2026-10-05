// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
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

func checkMapResult(t *testing.T, res *PrefixMapResult, err error, want *IPSet, wantMigrated []int64, wantUnmatched int64) {
	t.Helper()
	if err != nil {
		t.Fatalf("MapPrefixes: %v", err)
	}
	if !res.Set.Equal(want) {
		t.Fatalf("got ranges %v, want %v", res.Set.Ranges(), want.Ranges())
	}
	if len(res.Migrated) != len(wantMigrated) {
		t.Fatalf("got %d migration counts, want %d", len(res.Migrated), len(wantMigrated))
	}
	for i, want := range wantMigrated {
		if res.Migrated[i].Cmp(big.NewInt(want)) != 0 {
			t.Errorf("Migrated[%d] = %v, want %v", i, res.Migrated[i], want)
		}
	}
	if res.Unmatched.Cmp(big.NewInt(wantUnmatched)) != 0 {
		t.Errorf("Unmatched = %v, want %v", res.Unmatched, wantUnmatched)
	}
}

func TestMapPrefixesParentChild(t *testing.T) {
	in := mustIPSet("+10.0.0.0-10.0.0.255")
	rules := []PrefixMapping{
		{mustIPPrefix("10.0.0.0/24"), mustIPPrefix("192.0.2.0/24")},
		{mustIPPrefix("10.0.0.0/26"), mustIPPrefix("198.51.100.0/26")}, // most specific wins
	}
	res, err := in.MapPrefixes(rules, false)
	want := mustIPSet("+198.51.100.0-198.51.100.63", "+192.0.2.64-192.0.2.255")
	checkMapResult(t, res, err, want, []int64{192, 64}, 0)
}

func TestMapPrefixesIPv4ToIPv6(t *testing.T) {
	in := mustIPSet("+192.0.2.0-192.0.2.255")
	rules := []PrefixMapping{
		{mustIPPrefix("192.0.2.0/24"), mustIPPrefix("2001:db8::/120")},
	}
	res, err := in.MapPrefixes(rules, false)
	want := mustIPSet("+2001:db8::-2001:db8::ff")
	checkMapResult(t, res, err, want, []int64{256}, 0)
	if !res.Set.Contains(mustIP("2001:db8::ff")) {
		t.Error("mapped set does not contain 2001:db8::ff")
	}
}

func TestMapPrefixesIPv6ToIPv4(t *testing.T) {
	in := mustIPSet("+2001:db8::-2001:db8::ff")
	rules := []PrefixMapping{
		{mustIPPrefix("2001:db8::/120"), mustIPPrefix("192.0.2.0/24")},
	}
	res, err := in.MapPrefixes(rules, false)
	want := mustIPSet("+192.0.2.0-192.0.2.255")
	checkMapResult(t, res, err, want, []int64{256}, 0)
}

func TestMapPrefixesDstIntoAnotherSource(t *testing.T) {
	// Rule 0's destination is rule 1's source. Rules apply to the
	// original input only, so the mapped output must not be re-mapped.
	in := mustIPSet("+10.0.0.0-10.0.0.255")
	rules := []PrefixMapping{
		{mustIPPrefix("10.0.0.0/24"), mustIPPrefix("10.1.0.0/24")},
		{mustIPPrefix("10.1.0.0/24"), mustIPPrefix("192.0.2.0/24")},
	}
	res, err := in.MapPrefixes(rules, false)
	want := mustIPSet("+10.1.0.0-10.1.0.255")
	checkMapResult(t, res, err, want, []int64{256, 0}, 0)
}

func TestMapPrefixesDstCollision(t *testing.T) {
	// Adjacent destinations merge into a single range.
	in := mustIPSet("+10.0.0.0-10.0.0.255")
	rules := []PrefixMapping{
		{mustIPPrefix("10.0.0.0/25"), mustIPPrefix("192.0.2.0/25")},
		{mustIPPrefix("10.0.0.128/25"), mustIPPrefix("192.0.2.128/25")},
	}
	res, err := in.MapPrefixes(rules, false)
	want := mustIPSet("+192.0.2.0-192.0.2.255")
	checkMapResult(t, res, err, want, []int64{128, 128}, 0)
	if len(res.Set.Ranges()) != 1 {
		t.Fatalf("got %d ranges, want 1 merged range", len(res.Set.Ranges()))
	}

	// Overlapping destinations merge as a set union.
	in2 := mustIPSet("+10.0.0.0-10.0.1.127")
	rules2 := []PrefixMapping{
		{mustIPPrefix("10.0.0.0/24"), mustIPPrefix("192.0.2.0/24")},
		{mustIPPrefix("10.0.1.0/25"), mustIPPrefix("192.0.2.128/25")},
	}
	res2, err := in2.MapPrefixes(rules2, false)
	checkMapResult(t, res2, err, want, []int64{256, 128}, 0)
}

func TestMapPrefixesMaxLastAddr(t *testing.T) {
	// IPv4 range ending at 255.255.255.255.
	in := mustIPSet("+255.255.255.0-255.255.255.255")
	rules := []PrefixMapping{
		{mustIPPrefix("255.255.255.0/24"), mustIPPrefix("203.0.113.0/24")},
	}
	res, err := in.MapPrefixes(rules, false)
	want := mustIPSet("+203.0.113.0-203.0.113.255")
	checkMapResult(t, res, err, want, []int64{256}, 0)

	// IPv6 destination ending at the maximum address.
	in6 := mustIPSet("+2001:db8::-2001:db8::ff")
	rules6 := []PrefixMapping{
		{mustIPPrefix("2001:db8::/120"), mustIPPrefix("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00/120")},
	}
	res6, err := in6.MapPrefixes(rules6, false)
	want6 := mustIPSet("+ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	checkMapResult(t, res6, err, want6, []int64{256}, 0)
	if !res6.Set.Contains(mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")) {
		t.Error("mapped set does not contain the maximum IPv6 address")
	}
}

func TestMapPrefixesUnmatched(t *testing.T) {
	in := mustIPSet("+10.0.0.0-10.0.0.3", "+192.0.2.0-192.0.2.3")
	rules := []PrefixMapping{
		{mustIPPrefix("10.0.0.0/30"), mustIPPrefix("203.0.113.0/30")},
	}

	// Unmatched addresses are kept.
	res, err := in.MapPrefixes(rules, true)
	want := mustIPSet("+203.0.113.0-203.0.113.3", "+192.0.2.0-192.0.2.3")
	checkMapResult(t, res, err, want, []int64{4}, 4)

	// Unmatched addresses are dropped.
	res, err = in.MapPrefixes(rules, false)
	want = mustIPSet("+203.0.113.0-203.0.113.3")
	checkMapResult(t, res, err, want, []int64{4}, 4)
}

func TestMapPrefixesExactCounts(t *testing.T) {
	// An IPv6 /0 rule migrates 2^128 addresses; the count must be exact.
	in := mustIPSet("+::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	rules := []PrefixMapping{
		{mustIPPrefix("::/0"), mustIPPrefix("::/0")},
	}
	res, err := in.MapPrefixes(rules, false)
	if err != nil {
		t.Fatalf("MapPrefixes: %v", err)
	}
	want := new(big.Int).Lsh(big.NewInt(1), 128)
	if res.Migrated[0].Cmp(want) != 0 {
		t.Errorf("Migrated[0] = %v, want %v", res.Migrated[0], want)
	}
	if res.Unmatched.Sign() != 0 {
		t.Errorf("Unmatched = %v, want 0", res.Unmatched)
	}
}

func TestMapPrefixesErrors(t *testing.T) {
	in := mustIPSet("+10.0.0.0-10.0.0.255")
	tests := []struct {
		name  string
		rules []PrefixMapping
	}{
		{
			name: "host_bits_mismatch",
			rules: []PrefixMapping{
				{mustIPPrefix("10.0.0.0/24"), mustIPPrefix("192.0.2.0/25")},
			},
		},
		{
			name: "host_bits_mismatch_cross_family",
			rules: []PrefixMapping{
				{mustIPPrefix("10.0.0.0/24"), mustIPPrefix("2001:db8::/119")},
			},
		},
		{
			name: "conflicting_same_length_sources",
			rules: []PrefixMapping{
				{mustIPPrefix("10.0.0.0/24"), mustIPPrefix("192.0.2.0/24")},
				{mustIPPrefix("10.0.0.1/24"), mustIPPrefix("198.51.100.0/24")}, // masks to 10.0.0.0/24
			},
		},
		{
			name: "invalid_source",
			rules: []PrefixMapping{
				{netip.Prefix{}, mustIPPrefix("192.0.2.0/24")},
			},
		},
		{
			name: "invalid_destination",
			rules: []PrefixMapping{
				{mustIPPrefix("10.0.0.0/24"), netip.Prefix{}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := in.MapPrefixes(tt.rules, false); err == nil {
				t.Fatal("MapPrefixes succeeded, want error")
			}
		})
	}
}

// referenceMapPrefixes is a per-address reference implementation of
// MapPrefixes, only suitable for small sets.
func referenceMapPrefixes(s *IPSet, rules []PrefixMapping, keepUnmatched bool) *PrefixMapResult {
	migrated := make([]*big.Int, len(rules))
	for i := range migrated {
		migrated[i] = new(big.Int)
	}
	unmatched := new(big.Int)
	var b IPSetBuilder
	one := big.NewInt(1)
	for _, r := range s.Ranges() {
		for ip := r.From(); ; ip = ip.Next() {
			best := -1
			for i, m := range rules {
				if m.Src.Contains(ip) && (best < 0 || m.Src.Bits() > rules[best].Src.Bits()) {
					best = i
				}
			}
			if best < 0 {
				unmatched.Add(unmatched, one)
				if keepUnmatched {
					b.Add(ip)
				}
			} else {
				migrated[best].Add(migrated[best], one)
				m := rules[best]
				src, dst := m.Src.Masked(), m.Dst.Masked()
				off := u128From16(ip.As16()).sub(u128From16(src.Addr().As16()))
				mapped := u128From16(dst.Addr().As16()).add(off)
				if dst.Addr().Is4() {
					b.Add(mapped.IP4())
				} else {
					b.Add(mapped.IP6())
				}
			}
			if ip == r.To() {
				break
			}
		}
	}
	set, err := b.IPSet()
	if err != nil {
		panic(err)
	}
	return &PrefixMapResult{Set: set, Migrated: migrated, Unmatched: unmatched}
}

func randomMapPrefix(rnd *rand.Rand, v4 bool, hostBits int) netip.Prefix {
	if v4 {
		var a [4]byte
		rnd.Read(a[:])
		return netip.PrefixFrom(netip.AddrFrom4(a), 32-hostBits).Masked()
	}
	var a [16]byte
	rnd.Read(a[:])
	return netip.PrefixFrom(netip.AddrFrom16(a), 128-hostBits).Masked()
}

func TestMapPrefixesAgainstReference(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for iter := 0; iter < 500; iter++ {
		// Random rules with small prefixes so the reference
		// implementation can enumerate addresses.
		var rules []PrefixMapping
		seen := map[netip.Prefix]bool{}
		for len(rules) < rnd.Intn(6) {
			hostBits := rnd.Intn(7)
			src := randomMapPrefix(rnd, rnd.Intn(2) == 0, hostBits)
			dst := randomMapPrefix(rnd, rnd.Intn(2) == 0, hostBits)
			if seen[src] {
				continue
			}
			seen[src] = true
			rules = append(rules, PrefixMapping{src, dst})
		}

		// Random small input set.
		var b IPSetBuilder
		for i, n := 0, 1+rnd.Intn(8); i < n; i++ {
			b.AddPrefix(randomMapPrefix(rnd, rnd.Intn(2) == 0, rnd.Intn(7)))
		}
		in := buildIPSet(&b)
		keepUnmatched := rnd.Intn(2) == 0

		before := in.Ranges()
		got, err := in.MapPrefixes(rules, keepUnmatched)
		if err != nil {
			t.Fatalf("iter %d: MapPrefixes: %v", iter, err)
		}
		if after := in.Ranges(); !reflect.DeepEqual(before, after) {
			t.Fatalf("iter %d: input set modified: %v became %v", iter, before, after)
		}

		want := referenceMapPrefixes(in, rules, keepUnmatched)
		if !got.Set.Equal(want.Set) {
			t.Fatalf("iter %d: got ranges %v, want %v", iter, got.Set.Ranges(), want.Set.Ranges())
		}
		for i := range rules {
			if got.Migrated[i].Cmp(want.Migrated[i]) != 0 {
				t.Errorf("iter %d: Migrated[%d] = %v, want %v", iter, i, got.Migrated[i], want.Migrated[i])
			}
		}
		if got.Unmatched.Cmp(want.Unmatched) != 0 {
			t.Errorf("iter %d: Unmatched = %v, want %v", iter, got.Unmatched, want.Unmatched)
		}
	}
}
