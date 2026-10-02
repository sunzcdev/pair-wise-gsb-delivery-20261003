// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"bytes"
	"fmt"
	"log"
	"math/rand"
	"net/netip"
	"reflect"
	"testing"
)

func buildIPSet(b *IPSetBuilder) *IPSet {
	ret, err := b.IPSet()
	if err != nil {
		panic(err)
	}
	return ret
}

func TestIPSet(t *testing.T) {
	tests := []struct {
		name         string
		f            func(s *IPSetBuilder)
		wantRanges   []IPRange
		wantPrefixes []IPPrefix      // non-nil to test
		wantContains map[string]bool // optional non-exhaustive IPs to test for in resulting set
	}{
		{
			name: "mix_family",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.AddPrefix(mustIPPrefix("::/0"))
				s.RemovePrefix(mustIPPrefix("10.2.0.0/16"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.1.255.255")},
				{mustIP("10.3.0.0"), mustIP("10.255.255.255")},
				{mustIP("::"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")},
			},
		},
		{
			name: "merge_adjacent",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.AddPrefix(mustIPPrefix("11.0.0.0/8"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("11.255.255.255")},
			},
			wantPrefixes: pxv("10.0.0.0/7"),
		},
		{
			name: "remove_32",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.RemovePrefix(mustIPPrefix("10.1.2.3/32"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.1.2.2")},
				{mustIP("10.1.2.4"), mustIP("10.255.255.255")},
			},
			wantPrefixes: pxv(
				"10.0.0.0/16",
				"10.1.0.0/23",
				"10.1.2.0/31",
				"10.1.2.2/32",
				"10.1.2.4/30",
				"10.1.2.8/29",
				"10.1.2.16/28",
				"10.1.2.32/27",
				"10.1.2.64/26",
				"10.1.2.128/25",
				"10.1.3.0/24",
				"10.1.4.0/22",
				"10.1.8.0/21",
				"10.1.16.0/20",
				"10.1.32.0/19",
				"10.1.64.0/18",
				"10.1.128.0/17",
				"10.2.0.0/15",
				"10.4.0.0/14",
				"10.8.0.0/13",
				"10.16.0.0/12",
				"10.32.0.0/11",
				"10.64.0.0/10",
				"10.128.0.0/9",
			),
		},
		{
			name: "remove_32_and_first_16",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.RemovePrefix(mustIPPrefix("10.1.2.3/32"))
				s.RemovePrefix(mustIPPrefix("10.0.0.0/16"))
			},
			wantRanges: []IPRange{
				{mustIP("10.1.0.0"), mustIP("10.1.2.2")},
				{mustIP("10.1.2.4"), mustIP("10.255.255.255")},
			},
			wantPrefixes: pxv(
				"10.1.0.0/23",
				"10.1.2.0/31",
				"10.1.2.2/32",
				"10.1.2.4/30",
				"10.1.2.8/29",
				"10.1.2.16/28",
				"10.1.2.32/27",
				"10.1.2.64/26",
				"10.1.2.128/25",
				"10.1.3.0/24",
				"10.1.4.0/22",
				"10.1.8.0/21",
				"10.1.16.0/20",
				"10.1.32.0/19",
				"10.1.64.0/18",
				"10.1.128.0/17",
				"10.2.0.0/15",
				"10.4.0.0/14",
				"10.8.0.0/13",
				"10.16.0.0/12",
				"10.32.0.0/11",
				"10.64.0.0/10",
				"10.128.0.0/9",
			),
		},
		{
			name: "add_dup",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.255.255.255")},
			},
		},
		{
			name: "add_dup_subet",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.AddPrefix(mustIPPrefix("10.0.0.0/16"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.255.255.255")},
			},
		},
		{
			name: "add_remove_add",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				s.RemovePrefix(mustIPPrefix("10.1.2.3/32"))
				s.AddPrefix(mustIPPrefix("10.1.0.0/16")) // undoes prior line
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.255.255.255")},
			},
		},
		{
			name: "remove_then_add",
			f: func(s *IPSetBuilder) {
				s.RemovePrefix(mustIPPrefix("1.2.3.4/32")) // no-op
				s.AddPrefix(mustIPPrefix("1.2.3.4/32"))
			},
			wantRanges: []IPRange{
				{mustIP("1.2.3.4"), mustIP("1.2.3.4")},
			},
		},
		{
			name: "remove_end_on_add_start",
			f: func(s *IPSetBuilder) {
				s.AddRange(IPRange{mustIP("0.0.0.38"), mustIP("0.0.0.177")})
				s.RemoveRange(IPRange{mustIP("0.0.0.18"), mustIP("0.0.0.38")})
			},
			wantRanges: []IPRange{
				{mustIP("0.0.0.39"), mustIP("0.0.0.177")},
			},
		},
		{
			name: "fuzz_fail_2",
			f: func(s *IPSetBuilder) {
				s.AddRange(IPRange{mustIP("0.0.0.143"), mustIP("0.0.0.185")})
				s.AddRange(IPRange{mustIP("0.0.0.84"), mustIP("0.0.0.174")})
				s.AddRange(IPRange{mustIP("0.0.0.51"), mustIP("0.0.0.61")})
				s.RemoveRange(IPRange{mustIP("0.0.0.66"), mustIP("0.0.0.146")})
				s.AddRange(IPRange{mustIP("0.0.0.22"), mustIP("0.0.0.207")})
				s.RemoveRange(IPRange{mustIP("0.0.0.198"), mustIP("0.0.0.203")})
				s.RemoveRange(IPRange{mustIP("0.0.0.23"), mustIP("0.0.0.69")})
				s.AddRange(IPRange{mustIP("0.0.0.64"), mustIP("0.0.0.105")})
				s.AddRange(IPRange{mustIP("0.0.0.151"), mustIP("0.0.0.203")})
				s.AddRange(IPRange{mustIP("0.0.0.138"), mustIP("0.0.0.160")})
				s.RemoveRange(IPRange{mustIP("0.0.0.64"), mustIP("0.0.0.161")})
			},
			wantRanges: []IPRange{
				{mustIP("0.0.0.22"), mustIP("0.0.0.22")},
				{mustIP("0.0.0.162"), mustIP("0.0.0.207")},
			},
			wantContains: map[string]bool{
				"0.0.0.22": true,
			},
		},
		{
			name: "single_ips",
			f: func(s *IPSetBuilder) {
				s.Add(mustIP("10.0.0.0"))
				s.Add(mustIP("10.0.0.1"))
				s.Add(mustIP("10.0.0.2"))
				s.Add(mustIP("10.0.0.3"))
				s.Add(mustIP("10.0.0.4"))
				s.Remove(mustIP("10.0.0.4"))
				s.Add(mustIP("10.0.0.255"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.0.0.3")},
				{mustIP("10.0.0.255"), mustIP("10.0.0.255")},
			},
			wantPrefixes: pxv("10.0.0.0/30", "10.0.0.255/32"),
		},
		{
			// regression test for a bug where Ranges returned invalid IPRanges.
			name: "single_ip_removal",
			f: func(s *IPSetBuilder) {
				s.Add(mustIP("10.0.0.0"))
				s.Add(mustIP("10.0.0.1"))
				s.Add(mustIP("10.0.0.2"))
				s.Add(mustIP("10.0.0.3"))
				s.Add(mustIP("10.0.0.4"))
				s.Remove(mustIP("10.0.0.4"))
			},
			wantRanges: []IPRange{
				{mustIP("10.0.0.0"), mustIP("10.0.0.3")},
			},
			wantPrefixes: pxv("10.0.0.0/30"),
		},
		{
			name: "invert_empty",
			f: func(s *IPSetBuilder) {
				s.Complement()
			},
			wantRanges: []IPRange{
				{mustIP("0.0.0.0"), mustIP("255.255.255.255")},
				{mustIP("::"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")},
			},
			wantPrefixes: pxv("0.0.0.0/0", "::/0"),
		},
		{
			name: "invert_full",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(mustIPPrefix("0.0.0.0/0"))
				s.AddPrefix(mustIPPrefix("::/0"))
				s.Complement()
			},
			wantRanges:   []IPRange{},
			wantPrefixes: pxv(),
		},
		{
			name: "invert_partial",
			f: func(s *IPSetBuilder) {
				s.AddRange(IPRange{mustIP("1.1.1.1"), mustIP("2.2.2.2")})
				s.Add(mustIP("3.3.3.3"))
				s.AddPrefix(mustIPPrefix("4.4.4.0/24"))
				s.Add(mustIP("1::1"))
				s.Complement()
			},
			wantRanges: []IPRange{
				{mustIP("0.0.0.0"), mustIP("1.1.1.0")},
				{mustIP("2.2.2.3"), mustIP("3.3.3.2")},
				{mustIP("3.3.3.4"), mustIP("4.4.3.255")},
				{mustIP("4.4.5.0"), mustIP("255.255.255.255")},
				{mustIP("::"), mustIP("1::")},
				{mustIP("1::2"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")},
			},
		},
		{
			name: "intersect",
			f: func(s *IPSetBuilder) {
				var t IPSetBuilder
				t.AddRange(IPRange{mustIP("2.2.2.2"), mustIP("3.3.3.3")})

				s.AddRange(IPRange{mustIP("1.1.1.1"), mustIP("4.4.4.4")})
				s.Intersect(buildIPSet(&t))
			},
			wantRanges: []IPRange{
				{mustIP("2.2.2.2"), mustIP("3.3.3.3")},
			},
		},
		{
			name: "intersect_disjoint",
			f: func(s *IPSetBuilder) {
				var t IPSetBuilder
				t.AddRange(IPRange{mustIP("1.1.1.1"), mustIP("2.2.2.2")})

				s.AddRange(IPRange{mustIP("3.3.3.3"), mustIP("4.4.4.4")})
				s.Intersect(buildIPSet(&t))
			},
			wantRanges: []IPRange{},
		},
		{
			name: "intersect_partial",
			f: func(s *IPSetBuilder) {
				var t IPSetBuilder
				t.AddRange(IPRange{mustIP("1.1.1.1"), mustIP("3.3.3.3")})

				s.AddRange(IPRange{mustIP("2.2.2.2"), mustIP("4.4.4.4")})
				s.Intersect(buildIPSet(&t))
			},
			wantRanges: []IPRange{
				{mustIP("2.2.2.2"), mustIP("3.3.3.3")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debugf = t.Logf
			defer func() { debugf = discardf }()
			var build IPSetBuilder
			tt.f(&build)
			s := buildIPSet(&build)
			got := s.Ranges()
			t.Run("ranges", func(t *testing.T) {
				for _, v := range got {
					if !v.IsValid() {
						t.Errorf("invalid IPRange in result: %s -> %s", v.From(), v.To())
					}
				}
				if reflect.DeepEqual(got, tt.wantRanges) {
					return
				}
				t.Error("failed. got:\n")
				for _, v := range got {
					t.Errorf("  %s -> %s", v.From(), v.To())
				}
				t.Error("want:\n")
				for _, v := range tt.wantRanges {
					t.Errorf("  %s -> %s", v.From(), v.To())
				}
			})
			if tt.wantPrefixes != nil {
				t.Run("prefixes", func(t *testing.T) {
					got := s.Prefixes()
					if got == nil {
						got = []IPPrefix{}
					}
					if reflect.DeepEqual(got, tt.wantPrefixes) {
						return
					}
					t.Error("failed. got:\n")
					for _, v := range got {
						t.Errorf("  %v", v)
					}
					t.Error("want:\n")
					for _, v := range tt.wantPrefixes {
						t.Errorf("  %v", v)
					}
				})
			}
			if len(tt.wantContains) > 0 {
				for ipStr, want := range tt.wantContains {
					got := s.Contains(mustIP(ipStr))
					if got != want {
						t.Errorf("Contains(%q) = %v; want %v", s, got, want)
					}
				}
			}
		})
	}
}

func TestIPSetRemoveFreePrefix(t *testing.T) {
	pfx := mustIPPrefix
	tests := []struct {
		name         string
		f            func(s *IPSetBuilder)
		b            uint8
		wantPrefix   IPPrefix
		wantPrefixes []IPPrefix
		wantOK       bool
	}{
		{
			name: "cut in half",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(pfx("10.0.0.0/8"))
			},
			b:            9,
			wantPrefix:   pfx("10.0.0.0/9"),
			wantPrefixes: pxv("10.128.0.0/9"),
			wantOK:       true,
		},
		{
			name: "on prefix left",
			f: func(s *IPSetBuilder) {
				s.AddPrefix(pfx("10.0.0.0/8"))
				s.RemovePrefix(pfx("10.0.0.0/9"))
			},
			b:            9,
			wantPrefix:   pfx("10.128.0.0/9"),
			wantPrefixes: []IPPrefix{},
			wantOK:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debugf = t.Logf
			var build IPSetBuilder
			tt.f(&build)
			s := buildIPSet(&build)
			gotPrefix, gotSet, ok := s.RemoveFreePrefix(tt.b)
			if ok != tt.wantOK {
				t.Errorf("extractPrefix() ok = %t, wantOK %t", ok, tt.wantOK)
				return
			}
			if !reflect.DeepEqual(gotPrefix, tt.wantPrefix) {
				t.Errorf("extractPrefix() = %v, want %v", gotPrefix, tt.wantPrefix)
			}
			if !reflect.DeepEqual(gotSet.Prefixes(), tt.wantPrefixes) {
				t.Errorf("extractPrefix() = %v, want %v", gotSet.Prefixes(), tt.wantPrefixes)
			}
		})
	}
}

func TestIPSetSplitBalancedFiveAddresses(t *testing.T) {
	var b IPSetBuilder
	for _, addr := range []string{
		"192.0.2.1",
		"198.51.100.2",
		"203.0.113.3",
		"2001:db8::1",
		"2001:db8::2",
	} {
		b.Add(mustIP(addr))
	}
	s := buildIPSet(&b)
	before := s.Ranges()

	shards := s.SplitBalanced(3)
	wantRanges := [][]IPRange{
		{
			IPRangeFrom(mustIP("192.0.2.1"), mustIP("192.0.2.1")),
			IPRangeFrom(mustIP("198.51.100.2"), mustIP("198.51.100.2")),
		},
		{
			IPRangeFrom(mustIP("203.0.113.3"), mustIP("203.0.113.3")),
			IPRangeFrom(mustIP("2001:db8::1"), mustIP("2001:db8::1")),
		},
		{
			IPRangeFrom(mustIP("2001:db8::2"), mustIP("2001:db8::2")),
		},
	}
	for i, want := range wantRanges {
		if got := shards[i].Ranges(); !reflect.DeepEqual(got, want) {
			t.Errorf("shard %d ranges = %v; want %v", i, got, want)
		}
	}

	if got := s.Ranges(); !reflect.DeepEqual(got, before) {
		t.Errorf("input changed: got %v; want %v", got, before)
	}
	assertSplitInvariants(t, s, shards)
}

func TestIPSetSplitBalancedBoundaries(t *testing.T) {
	fullV4 := mustIPPrefix("0.0.0.0/0")
	fullV6 := mustIPPrefix("::/0")
	tests := []struct {
		name  string
		build func(*IPSetBuilder)
		parts int
		check func(*testing.T, []*IPSet)
	}{
		{
			name: "single address with empty shards",
			build: func(b *IPSetBuilder) {
				b.Add(mustIP("192.0.2.1"))
			},
			parts: 3,
			check: func(t *testing.T, shards []*IPSet) {
				assertShardSizes(t, shards, 1, 0, 0)
				if got := len(shards[0].Ranges()); got != 1 {
					t.Fatalf("first shard has %d ranges; want 1", got)
				}
			},
		},
		{
			name: "ipv4 zero prefix",
			build: func(b *IPSetBuilder) {
				b.AddPrefix(fullV4)
			},
			parts: 3,
			check: func(t *testing.T, shards []*IPSet) {
				assertShardSizes(t, shards, 1431655766, 1431655765, 1431655765)
				want := []IPRange{
					IPRangeFrom(mustIP("0.0.0.0"), mustIP("85.85.85.85")),
					IPRangeFrom(mustIP("85.85.85.86"), mustIP("170.170.170.170")),
					IPRangeFrom(mustIP("170.170.170.171"), mustIP("255.255.255.255")),
				}
				for i, r := range want {
					got := shards[i].Ranges()
					if len(got) != 1 || got[0] != r {
						t.Fatalf("shard %d = %v; want %v", i, got, []IPRange{r})
					}
				}
			},
		},
		{
			name: "ipv6 zero prefix",
			build: func(b *IPSetBuilder) {
				b.AddPrefix(fullV6)
			},
			parts: 2,
			check: func(t *testing.T, shards []*IPSet) {
				want := []IPRange{
					IPRangeFrom(mustIP("::"), mustIP("7fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")),
					IPRangeFrom(mustIP("8000::"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")),
				}
				for i, r := range want {
					got := shards[i].Ranges()
					if len(got) != 1 || got[0] != r {
						t.Fatalf("shard %d = %v; want %v", i, got, []IPRange{r})
					}
				}
			},
		},
		{
			name: "ipv6 zero prefix one part",
			build: func(b *IPSetBuilder) {
				b.AddPrefix(fullV6)
			},
			parts: 1,
			check: func(t *testing.T, shards []*IPSet) {
				got := shards[0].Ranges()
				want := []IPRange{
					IPRangeFrom(mustIP("::"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")),
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("shard 0 = %v; want %v", got, want)
				}
			},
		},
		{
			name: "dual zero prefix one part",
			build: func(b *IPSetBuilder) {
				b.AddPrefix(fullV4)
				b.AddPrefix(fullV6)
			},
			parts: 1,
			check: func(t *testing.T, shards []*IPSet) {
				got := shards[0].Ranges()
				want := []IPRange{
					IPRangeFrom(mustIP("0.0.0.0"), mustIP("255.255.255.255")),
					IPRangeFrom(mustIP("::"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")),
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("shard 0 = %v; want %v", got, want)
				}
			},
		},
		{
			name: "dual zero prefix crosses family boundary",
			build: func(b *IPSetBuilder) {
				b.AddPrefix(fullV4)
				b.AddPrefix(fullV6)
			},
			parts: 3,
			check: func(t *testing.T, shards []*IPSet) {
				if !shards[0].ContainsPrefix(fullV4) {
					t.Fatal("first shard does not contain all IPv4 addresses")
				}
				if !shards[0].Contains(mustIP("::")) {
					t.Fatal("first shard does not contain the first IPv6 address")
				}
				if shards[0].Contains(mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")) {
					t.Fatal("first shard unexpectedly contains the last IPv6 address")
				}
			},
		},
		{
			name: "maximum last addresses",
			build: func(b *IPSetBuilder) {
				b.AddRange(IPRangeFrom(mustIP("255.255.255.254"), mustIP("255.255.255.255")))
				b.AddRange(IPRangeFrom(mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe"), mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")))
			},
			parts: 3,
			check: func(t *testing.T, shards []*IPSet) {
				assertShardSizes(t, shards, 2, 1, 1)
				if !shards[2].Contains(mustIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")) {
					t.Fatal("last shard does not contain the last IPv6 address")
				}
			},
		},
		{
			name: "empty set",
			build: func(b *IPSetBuilder) {
			},
			parts: 3,
			check: func(t *testing.T, shards []*IPSet) {
				assertShardSizes(t, shards, 0, 0, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b IPSetBuilder
			tt.build(&b)
			s := buildIPSet(&b)
			before := s.Ranges()
			shards := s.SplitBalanced(tt.parts)
			assertSplitInvariants(t, s, shards)
			tt.check(t, shards)
			if got := s.Ranges(); !reflect.DeepEqual(got, before) {
				t.Errorf("input changed: got %v; want %v", got, before)
			}
		})
	}
}

func TestIPSetSplitBalancedInvalidParts(t *testing.T) {
	s := buildIPSet(&IPSetBuilder{})
	for _, parts := range []int{0, -1, maxSplitBalancedParts + 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SplitBalanced(%d) did not panic", parts)
				}
			}()
			s.SplitBalanced(parts)
		}()
	}
}

func assertSplitInvariants(t *testing.T, original *IPSet, shards []*IPSet) {
	t.Helper()

	var total uint129
	sizes := make([]uint129, len(shards))
	var allRanges []IPRange
	var union IPSetBuilder

	for i, shard := range shards {
		if shard == nil {
			t.Fatalf("shard %d is nil", i)
		}
		ranges := shard.Ranges()
		var normalized IPSetBuilder
		for j, r := range ranges {
			if !r.IsValid() {
				t.Fatalf("shard %d contains invalid range %v", i, r)
			}
			normalized.AddRange(r)
			if j > 0 {
				prev := ranges[j-1]
				if !prev.entirelyBefore(r) {
					t.Fatalf("shard %d ranges are not ordered: %v then %v", i, prev, r)
				}
				if prev.To().Compare(r.From()) >= 0 {
					t.Fatalf("shard %d contains overlapping ranges %v and %v", i, prev, r)
				}
			}
			size := rangeSize129(r)
			sizes[i] = sizes[i].add(size)
			total = total.add(size)
		}
		if got := buildIPSet(&normalized).Ranges(); !reflect.DeepEqual(got, ranges) {
			t.Fatalf("shard %d ranges are not normalized: got %v; want %v", i, got, ranges)
		}
		allRanges = append(allRanges, ranges...)
		union.AddSet(shard)
	}

	if want := ipSetSize129(original); total.cmp(want) != 0 {
		t.Fatalf("split address count = %v; want %v", total, want)
	}
	if got := buildIPSet(&union); !got.Equal(original) {
		t.Fatalf("union of shards = %v; want %v", got.Ranges(), original.Ranges())
	}
	for i := range shards {
		for j := i + 1; j < len(shards); j++ {
			if shards[i].Overlaps(shards[j]) {
				t.Fatalf("shards %d and %d overlap", i, j)
			}
		}
	}
	for i := 1; i < len(allRanges); i++ {
		if !allRanges[i-1].entirelyBefore(allRanges[i]) {
			t.Fatalf("shards are not ordered: %v then %v", allRanges[i-1], allRanges[i])
		}
	}

	if len(sizes) == 0 {
		return
	}
	base, remainder := ipSetSize129(original).divmod(uint64(len(shards)))
	for i, size := range sizes {
		want := base
		if i < int(remainder) {
			want = want.addOne()
		}
		if size.cmp(want) != 0 {
			t.Errorf("shard %d size = %v; want %v", i, size, want)
		}
	}
}

func assertShardSizes(t *testing.T, shards []*IPSet, want ...uint64) {
	t.Helper()
	if len(shards) != len(want) {
		t.Fatalf("got %d shards; want %d", len(shards), len(want))
	}
	for i, n := range want {
		if got := ipSetSize129(shards[i]); got != (uint129{lo: uint128{lo: n}}) {
			t.Errorf("shard %d size = %v; want %d", i, got, n)
		}
	}
}

func ipSetSize129(s *IPSet) uint129 {
	var total uint129
	for _, r := range s.Ranges() {
		total = total.add(rangeSize129(r))
	}
	return total
}

func mustIPSet(ranges ...string) *IPSet {
	var ret IPSetBuilder
	for _, r := range ranges {
		ipr, err := ParseIPRange(r[1:])
		if err != nil {
			panic(err)
		}
		switch r[0] {
		case '+':
			ret.AddRange(ipr)
		case '-':
			ret.RemoveRange(ipr)
		default:
			panic(fmt.Sprintf("unknown command %q", r[0]))
		}
	}
	return buildIPSet(&ret)
}

func TestIPSetOverlaps(t *testing.T) {
	tests := []struct {
		a, b *IPSet
		want bool
	}{
		{
			mustIPSet(),
			mustIPSet(),
			false,
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.5"),
			mustIPSet("+10.0.0.0-10.0.0.5"),
			true, // exact match
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.5"),
			mustIPSet("+10.0.0.5-10.0.0.10"),
			true, // overlap on edge
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.5"),
			mustIPSet("+10.0.0.3-10.0.0.7"),
			true, // overlap in middle
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.5"),
			mustIPSet("+10.0.0.2-10.0.0.3"),
			true, // one inside other
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.5", "+10.1.0.0-10.1.0.5"),
			mustIPSet("+10.1.0.1-10.1.0.2"),
			true, // overlap in non-first
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.10", "-10.0.0.5-10.0.0.10"),
			mustIPSet("+10.0.0.7-10.0.0.8"),
			false, // removal cancels overlap
		},
		{
			mustIPSet("+10.0.0.0-10.0.0.10", "-10.0.0.5-10.0.0.10", "+10.0.0.5-10.0.0.10"),
			mustIPSet("+10.0.0.7-10.0.0.8"),
			true, // removal+readd restores overlap
		},
	}

	for _, test := range tests {
		got := test.a.Overlaps(test.b)
		if got != test.want {
			t.Errorf("(%s).Overlaps(%s) = %v, want %v", test.a, test.b, got, test.want)
		}
		got = test.b.Overlaps(test.a)
		if got != test.want {
			t.Errorf("(%s).Overlaps(%s) = %v, want %v", test.b, test.a, got, test.want)
		}
	}
}

func TestIPSetContains(t *testing.T) {
	var build IPSetBuilder
	build.AddPrefix(mustIPPrefix("10.0.0.0/8"))
	build.AddPrefix(mustIPPrefix("1.2.3.4/32"))
	build.AddPrefix(mustIPPrefix("fc00::/7"))
	s := buildIPSet(&build)

	tests := []struct {
		ip   string
		want bool
	}{
		{"0.0.0.0", false},
		{"::", false},

		{"1.2.3.3", false},
		{"1.2.3.4", true},
		{"1.2.3.5", false},

		{"9.255.255.255", false},
		{"10.0.0.0", true},
		{"10.1.2.3", true},
		{"10.255.255.255", true},
		{"11.0.0.0", false},

		{"::", false},
		{"fc00::", true},
		{"fc00::1", true},
		{"fd00::1", true},
		{"ff00::1", false},

		{"fd00::%a", false},
		{"fd00::1%a", false},
	}
	for _, tt := range tests {
		got := s.Contains(mustIP(tt.ip))
		if got != tt.want {
			t.Errorf("contains(%q) = %v; want %v", tt.ip, got, tt.want)
		}
	}
}

func TestIPSetFuzz(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		doIPSetFuzz(t, 100)
	} else {
		doIPSetFuzz(t, 5000)
	}
}

func BenchmarkIPSetFuzz(b *testing.B) {
	b.ReportAllocs()
	doIPSetFuzz(b, b.N)
}

func doIPSetFuzz(t testing.TB, iters int) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	debugf = logger.Printf
	defer func() { debugf = discardf }()
	for i := 0; i < iters; i++ {
		buf.Reset()
		steps, set, wantContains := newRandomIPSet()
		for b, want := range wantContains {
			ip := IPv4(0, 0, 0, uint8(b))
			got := set.Contains(ip)
			if got != want {
				t.Fatalf("for steps %q, contains(%v) = %v; want %v\n%s", steps, ip, got, want, buf.Bytes())
			}
		}
	}
}

func newRandomIPSet() (steps []string, s *IPSet, wantContains [256]bool) {
	b := new(IPSetBuilder)
	nstep := 2 + rand.Intn(10)
	for i := 0; i < nstep; i++ {
		op := rand.Intn(2)
		ip1 := uint8(rand.Intn(256))
		ip2 := uint8(rand.Intn(256))
		if ip2 < ip1 {
			ip1, ip2 = ip2, ip1
		}
		var v bool
		switch op {
		case 0:
			steps = append(steps, fmt.Sprintf("add 0.0.0.%d-0.0.0.%d", ip1, ip2))
			b.AddRange(IPRangeFrom(IPv4(0, 0, 0, ip1), IPv4(0, 0, 0, ip2)))
			v = true
		case 1:
			steps = append(steps, fmt.Sprintf("remove 0.0.0.%d-0.0.0.%d", ip1, ip2))
			b.RemoveRange(IPRangeFrom(IPv4(0, 0, 0, ip1), IPv4(0, 0, 0, ip2)))
		}
		for i := ip1; i <= ip2; i++ {
			wantContains[i] = v
			if i == ip2 {
				break
			}
		}
	}
	s = buildIPSet(b)
	return
}

// TestIPSetRanges tests IPSet.Ranges against 64k
// patterns of sets of ranges, checking the real implementation
// against the test's separate implementation.
//
// For each of uint16 pattern, each set bit is treated as an IP that
// should be in the set's resultant Ranges.
func TestIPSetRanges(t *testing.T) {
	t.Parallel()
	upper := 0x0fff
	if *long {
		upper = 0xffff
	}
	for pat := 0; pat <= upper; pat++ {
		var build IPSetBuilder
		var from, to IP
		ranges := make([]IPRange, 0)
		flush := func() {
			r := IPRangeFrom(from, to)
			build.AddRange(r)
			ranges = append(ranges, r)
			from, to = IP{}, IP{}
		}
		for b := uint16(0); b < 16; b++ {
			if uint16(pat)&(1<<b) != 0 {
				ip := IPv4(1, 0, 0, uint8(b))
				to = ip
				if !from.IsValid() {
					from = ip
				}
				continue
			}
			if from.IsValid() {
				flush()
			}
		}
		if from.IsValid() {
			flush()
		}
		got := buildIPSet(&build).Ranges()
		if !reflect.DeepEqual(got, ranges) {
			t.Errorf("for %016b: got %v; want %v", pat, got, ranges)
		}
	}
}

func TestIPSetRangesStress(t *testing.T) {
	t.Parallel()
	n := 50
	if testing.Short() {
		n /= 10
	} else if *long {
		n = 500
	}
	const numIPs = 1 << 16 // we test lower 16 bits only
	randRange := func() (a, b int, r IPRange) {
		a, b = rand.Intn(numIPs), rand.Intn(numIPs)
		if a > b {
			a, b = b, a
		}
		from := IPv4(0, 0, uint8(a>>8), uint8(a))
		to := IPv4(0, 0, uint8(b>>8), uint8(b))
		return a, b, IPRangeFrom(from, to)
	}
	for i := 0; i < n; i++ {
		var build IPSetBuilder
		var want [numIPs]bool
		// Add some ranges
		for i := 0; i < 1+rand.Intn(2); i++ {
			a, b, r := randRange()
			for i := a; i <= b; i++ {
				want[i] = true
			}
			build.AddRange(r)
		}
		// Remove some ranges
		for i := 0; i < rand.Intn(3); i++ {
			a, b, r := randRange()
			for i := a; i <= b; i++ {
				want[i] = false
			}
			build.RemoveRange(r)
		}
		ranges := buildIPSet(&build).Ranges()

		// Make sure no ranges are adjacent or overlapping
		for i, r := range ranges {
			if i == 0 {
				continue
			}
			if ranges[i-1].To().Compare(r.From()) != -1 {
				t.Fatalf("overlapping ranges: %v", ranges)
			}
		}

		// Copy the ranges back to a new set before using
		// ContainsFunc, in case the ContainsFunc implementation
		// changes in the future to not use Ranges itself:
		var build2 IPSetBuilder
		for _, r := range ranges {
			build2.AddRange(r)
		}
		s2 := buildIPSet(&build2)
		for i, want := range want {
			if got := s2.Contains(IPv4(0, 0, uint8(i>>8), uint8(i))); got != want {
				t.Fatal("failed")
			}
		}
	}
}

func TestIPSetEqual(t *testing.T) {
	a := new(IPSetBuilder)
	b := new(IPSetBuilder)
	MustParseIP := netip.MustParseAddr
	MustParseIPPrefix := netip.MustParsePrefix

	assertEqual := func(want bool) {
		t.Helper()
		if got := buildIPSet(a).Equal(buildIPSet(b)); got != want {
			t.Errorf("%v.Equal(%v) = %v want %v", a, b, got, want)
		}
	}

	a.Add(MustParseIP("1.1.1.0"))
	a.Add(MustParseIP("1.1.1.1"))
	a.Add(MustParseIP("1.1.1.2"))
	b.AddPrefix(MustParseIPPrefix("1.1.1.0/31"))
	b.Add(MustParseIP("1.1.1.2"))
	assertEqual(true)

	a.RemoveSet(buildIPSet(a))
	assertEqual(false)
	b.RemoveSet(buildIPSet(b))
	assertEqual(true)

	a.Add(MustParseIP("1.1.1.0"))
	a.Add(MustParseIP("1.1.1.1"))
	a.Add(MustParseIP("1.1.1.2"))

	b.AddPrefix(MustParseIPPrefix("1.1.1.0/30"))
	b.Remove(MustParseIP("1.1.1.3"))
	assertEqual(true)
}
