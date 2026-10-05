// Copyright 2023 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"errors"
	"math/rand"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

func zonesFor(t *testing.T, add func(*IPSetBuilder)) []ReverseZone {
	t.Helper()
	var b IPSetBuilder
	add(&b)
	s, err := b.IPSet()
	if err != nil {
		t.Fatalf("IPSet: %v", err)
	}
	zones, err := s.ReverseZones(0)
	if err != nil {
		t.Fatalf("ReverseZones: %v", err)
	}
	return zones
}

func rz(zone, cidr string) ReverseZone {
	return ReverseZone{Zone: zone, Prefix: mustIPPrefix(cidr)}
}

// wantZones is the test oracle: for one prefix it independently
// enumerates every boundary-aligned sub-prefix and names it, without
// sharing production helpers.
func wantZones(t *testing.T, cidrs ...string) []ReverseZone {
	t.Helper()
	var out []ReverseZone
	for _, cidr := range cidrs {
		p := mustIPPrefix(cidr).Masked()
		is4 := p.Addr().Is4()
		step := 4
		maxBits := 128
		if is4 {
			step, maxBits = 8, 32
		}
		boundary := (p.Bits() + step - 1) / step * step
		if boundary > maxBits {
			boundary = maxBits
		}
		n := 1 << uint(boundary-p.Bits())
		cur := p.Addr()
		for i := 0; i < n; i++ {
			sub := netip.PrefixFrom(cur, boundary)
			out = append(out, ReverseZone{Zone: oracleName(sub), Prefix: sub})
			cur = addAddrPow2(cur, uint(maxBits-boundary))
		}
	}
	return out
}

// addAddrPow2 returns a + 2^shift without overflowing at shift 64.
func addAddrPow2(a netip.Addr, shift uint) netip.Addr {
	b := a.As16()
	byteIdx := 15 - int(shift/8)
	add := byte(1) << (shift % 8)
	for i := byteIdx; i >= 0; i-- {
		sum := uint16(b[i]) + uint16(add)
		b[i] = byte(sum)
		if sum <= 0xff {
			break
		}
		add = 1
	}
	if a.Is4() {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return netip.AddrFrom16(b)
}

func oracleName(p netip.Prefix) string {
	if p.Addr().Is4() {
		b := p.Addr().As4()
		n := p.Bits() / 8
		s := ""
		for i := 0; i < n; i++ {
			s += strconv.Itoa(int(b[n-1-i])) + "."
		}
		return s + "in-addr.arpa."
	}
	b := p.Addr().As16()
	s := ""
	n := p.Bits() / 4
	for i := 0; i < n; i++ {
		v := msbNibble(b, n-1-i)
		s += string([]byte{"0123456789abcdef"[v]}) + "."
	}
	return s + "ip6.arpa."
}

func TestReverseZonesCases(t *testing.T) {
	tests := []struct {
		name string
		add  func(*IPSetBuilder)
		want []ReverseZone
	}{
		{
			name: "empty",
			add:  func(b *IPSetBuilder) {},
			want: nil,
		},
		{
			name: "v4_default",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("0.0.0.0/0"))
			},
			want: wantZones(t, "0.0.0.0/0"),
		},
		{
			name: "v6_default",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("::/0"))
			},
			want: wantZones(t, "::/0"),
		},
		{
			name: "v4_slash8_16_24_32",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("10.0.0.0/8"))
				b.AddPrefix(mustIPPrefix("192.168.1.0/24"))
				b.AddPrefix(mustIPPrefix("1.2.3.4/32"))
			},
			want: wantZones(t, "1.2.3.4/32", "10.0.0.0/8", "192.168.1.0/24"),
		},
		{
			name: "v4_slash23",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("10.10.0.0/23"))
			},
			want: wantZones(t, "10.10.0.0/23"),
		},
		{
			name: "v4_slash22",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("10.10.4.0/22"))
			},
			want: wantZones(t, "10.10.4.0/22"),
		},
		{
			name: "v4_slash25_host_level",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("192.168.1.128/25"))
			},
			want: wantZones(t, "192.168.1.128/25"),
		},
		{
			name: "v4_slash17",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("172.16.0.0/17"))
			},
			want: wantZones(t, "172.16.0.0/17"),
		},
		{
			name: "v4_slash7",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("10.0.0.0/7"))
			},
			want: wantZones(t, "10.0.0.0/7"),
		},
		{
			name: "v4_slash1",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("128.0.0.0/1"))
			},
			want: wantZones(t, "128.0.0.0/1"),
		},
		{
			name: "v6_slash63",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2001:db8::/63"))
			},
			want: wantZones(t, "2001:db8::/63"),
		},
		{
			name: "v6_slash60",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2001:db8::/60"))
			},
			want: wantZones(t, "2001:db8::/60"),
		},
		{
			name: "v6_slash65",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2001:db8::/65"))
			},
			want: wantZones(t, "2001:db8::/65"),
		},
		{
			name: "v6_slash127",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2001:db8::1/127"))
			},
			want: wantZones(t, "2001:db8::1/127"),
		},
		{
			name: "v6_slash4",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2000::/4"))
			},
			want: wantZones(t, "2000::/4"),
		},
		{
			name: "v6_host",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2001:db8::1/128"))
			},
			want: wantZones(t, "2001:db8::1/128"),
		},
		{
			name: "mixed_family_order",
			add: func(b *IPSetBuilder) {
				b.AddPrefix(mustIPPrefix("2001:db8::/64"))
				b.AddPrefix(mustIPPrefix("10.0.0.0/8"))
			},
			want: append(
				wantZones(t, "10.0.0.0/8"),
				wantZones(t, "2001:db8::/64")...,
			),
		},
		{
			name: "non_prefix_range",
			add: func(b *IPSetBuilder) {
				b.AddRange(IPRangeFrom(mustIP("10.0.0.1"), mustIP("10.0.0.3")))
			},
			want: append(
				wantZones(t, "10.0.0.1/32"),
				wantZones(t, "10.0.0.2/31")...,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := zonesFor(t, tt.add)
			if len(got) != len(tt.want) {
				t.Fatalf("zone count = %d, want %d\n got %v\nwant %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("zone %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestReverseZonesProperties builds random IPSets (prefixes plus a few
// non-prefix ranges, both families) and asserts that:
//   - every zone name round-trips through ParseReverseZone,
//   - each zone prefix is boundary-aligned and lies within the set,
//   - the union of zone prefixes equals the source IPSet exactly,
//   - zones are emitted in IPSet prefix order with no duplicates.
func TestReverseZonesProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for iter := 0; iter < 300; iter++ {
		var b IPSetBuilder
		nPrefixes := rng.Intn(4)
		for i := 0; i < nPrefixes; i++ {
			if rng.Intn(2) == 0 {
				b.AddPrefix(randomV4Prefix(rng))
			} else {
				b.AddPrefix(randomV6Prefix(rng))
			}
		}
		if rng.Intn(2) == 0 {
			b.AddRange(randomV4Range(rng))
		}
		s, err := b.IPSet()
		if err != nil {
			t.Fatalf("IPSet: %v", err)
		}

		zones, err := s.ReverseZones(0)
		if err != nil {
			t.Fatalf("ReverseZones: %v", err)
		}

		src := s.Prefixes()
		if int64(len(zones)) != s.CountReverseZones() {
			t.Fatalf("len(zones)=%d, CountReverseZones=%d", len(zones), s.CountReverseZones())
		}

		var rebuilt IPSetBuilder
		var prev netip.Addr
		prevFamily := -1
		seen := map[string]bool{}
		for i, z := range zones {
			// Round-trip the name.
			parsed, err := ParseReverseZone(z.Zone)
			if err != nil {
				t.Fatalf("zone %q does not parse: %v", z.Zone, err)
			}
			if parsed != z {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", parsed, z)
			}
			// Boundary alignment.
			step := 8
			if !z.Prefix.Addr().Is4() {
				step = 4
			}
			if z.Prefix.Bits()%step != 0 {
				t.Fatalf("zone prefix %s not boundary aligned", z.Prefix)
			}
			if z.Prefix.Masked() != z.Prefix {
				t.Fatalf("zone prefix %s has host bits set", z.Prefix)
			}
			// Containment and ordering.
			if !s.ContainsPrefix(z.Prefix) {
				t.Fatalf("zone prefix %s not contained in source set", z.Prefix)
			}
			if i > 0 {
				curAddr := z.Prefix.Addr()
				family := 4
				if !curAddr.Is4() {
					family = 6
				}
				if family < prevFamily || (family == prevFamily && prev.Compare(curAddr) >= 0) {
					t.Fatalf("zones not strictly ordered at %d: %s then %s", i, prev, z.Prefix)
				}
				prev = curAddr
				prevFamily = family
			} else {
				prev = z.Prefix.Addr()
				prevFamily = 4
				if !prev.Is4() {
					prevFamily = 6
				}
			}
			if seen[z.Prefix.String()] {
				t.Fatalf("duplicate zone prefix %s", z.Prefix)
			}
			seen[z.Prefix.String()] = true
			rebuilt.AddPrefix(z.Prefix)
		}

		union, err := rebuilt.IPSet()
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if !union.Equal(s) {
			t.Fatalf("zone union differs from source\n source prefixes: %v\n zone prefixes:   %v", src, union.Prefixes())
		}
	}
}

func randomV4Prefix(rng *rand.Rand) netip.Prefix {
	var b [4]byte
	rng.Read(b[:])
	bits := rng.Intn(33)
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked()
}

func randomV6Prefix(rng *rand.Rand) netip.Prefix {
	var b [16]byte
	rng.Read(b[:])
	bits := rng.Intn(129)
	return netip.PrefixFrom(netip.AddrFrom16(b), bits).Masked()
}

func randomV4Range(rng *rand.Rand) IPRange {
	start := randomV4Prefix(rng).Addr()
	endBytes := start.As4()
	delta := rng.Intn(512)
	for i := 3; i >= 0 && delta != 0; i-- {
		sum := int(endBytes[i]) + delta&0xff
		endBytes[i] = byte(sum)
		delta = delta>>8 + sum>>8
	}
	end := netip.AddrFrom4(endBytes)
	if end.Less(start) {
		start, end = end, start
	}
	return IPRangeFrom(start, end)
}

func TestReverseZonesLimit(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(mustIPPrefix("192.168.1.128/25"))
	s, _ := b.IPSet()

	if got := s.CountReverseZones(); got != 128 {
		t.Fatalf("CountReverseZones = %d, want 128", got)
	}

	var limitErr ReverseZoneLimitError
	if _, err := s.ReverseZones(127); !errors.As(err, &limitErr) {
		t.Fatalf("err = %v, want ReverseZoneLimitError", err)
	} else {
		if limitErr.Got != 128 || limitErr.Limit != 127 {
			t.Fatalf("limit error = %+v, want Got=128 Limit=127", limitErr)
		}
	}

	zones, err := s.ReverseZones(128)
	if err != nil {
		t.Fatalf("ReverseZones(128): %v", err)
	}
	if len(zones) != 128 {
		t.Fatalf("len(zones) = %d, want 128", len(zones))
	}

	// A limit miss must leave the append destination untouched.
	dst := []ReverseZone{{Zone: "sentinel", Prefix: mustIPPrefix("1.2.3.4/32")}}
	out, err := s.AppendReverseZones(dst, 1)
	if err == nil {
		t.Fatal("expected limit error")
	}
	if len(out) != 1 || out[0].Zone != "sentinel" {
		t.Fatalf("dst modified on limit error: %v", out)
	}

	out, err = s.AppendReverseZones(dst, 256)
	if err != nil {
		t.Fatalf("AppendReverseZones: %v", err)
	}
	if len(out) != 129 {
		t.Fatalf("len(out) = %d, want 129", len(out))
	}
}

func TestReverseZonesNilAndEmpty(t *testing.T) {
	var nilSet *IPSet
	if got := nilSet.CountReverseZones(); got != 0 {
		t.Fatalf("nil CountReverseZones = %d, want 0", got)
	}
	zones, err := nilSet.ReverseZones(0)
	if err != nil || zones != nil {
		t.Fatalf("nil ReverseZones = %v, %v", zones, err)
	}
	dst := []ReverseZone{rz("0.in-addr.arpa.", "0.0.0.0/8")}
	out, err := nilSet.AppendReverseZones(dst, 0)
	if err != nil || len(out) != 1 {
		t.Fatalf("nil AppendReverseZones = %v, %v", out, err)
	}

	var b IPSetBuilder
	empty, _ := b.IPSet()
	zones, err = empty.ReverseZones(1)
	if err != nil {
		t.Fatalf("empty set with limit: %v", err)
	}
	if len(zones) != 0 {
		t.Fatalf("empty set produced %d zones", len(zones))
	}
}

func TestParseReverseZone(t *testing.T) {
	fullV6Name := "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa."
	if _, err := ParseReverseZone(fullV6Name); err != nil {
		t.Fatalf("generated full name invalid: %v", err)
	}
	good := []struct {
		in   string
		want ReverseZone
	}{
		{"in-addr.arpa.", rz("in-addr.arpa.", "0.0.0.0/0")},
		{"IN-ADDR.ARPA", rz("in-addr.arpa.", "0.0.0.0/0")},
		{"10.in-addr.arpa.", rz("10.in-addr.arpa.", "10.0.0.0/8")},
		{"1.168.192.in-addr.arpa", rz("1.168.192.in-addr.arpa.", "192.168.1.0/24")},
		{"4.3.2.1.in-addr.arpa.", rz("4.3.2.1.in-addr.arpa.", "1.2.3.4/32")},
		{"ip6.arpa.", rz("ip6.arpa.", "::/0")},
		{"0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.IP6.ARPA.", rz("0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8::/64")},
		{"1.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", rz("1.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8:0:1::/64")},
		{
			fullV6Name,
			rz(fullV6Name, "2001:db8::1/128"),
		},
	}
	for _, tt := range good {
		got, err := ParseReverseZone(tt.in)
		if err != nil {
			t.Errorf("ParseReverseZone(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseReverseZone(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}

	bad := []string{
		"",
		"arpa.",
		"example.com.",
		"256.0.0.10.in-addr.arpa.",
		"01.0.0.10.in-addr.arpa.",
		"x.0.0.10.in-addr.arpa.",
		"1.2.3.4.5.in-addr.arpa.",
		"g.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",
		"10.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", // multi-char nibble
		strings.Repeat("0.", 33) + "ip6.arpa.",
	}
	for _, in := range bad {
		if _, err := ParseReverseZone(in); err == nil {
			t.Errorf("ParseReverseZone(%q) succeeded, want error", in)
		}
	}
}

// TestReverseZonesBoundaryExpansion demonstrates the requested
// boundary-decomposition contrast: aligned prefixes stay coarse, while
// prefixes straddling a boundary expand exactly to the next boundary.
func TestReverseZonesBoundaryExpansion(t *testing.T) {
	cases := []struct {
		cidr string
		n    int64
		bits int
	}{
		{"10.10.0.0/23", 2, 24},
		{"192.168.1.128/25", 128, 32},
		{"2001:db8::/63", 2, 64},
		{"2001:db8::/65", 8, 68},
	}
	for _, tt := range cases {
		var b IPSetBuilder
		b.AddPrefix(mustIPPrefix(tt.cidr))
		s, _ := b.IPSet()
		if got := s.CountReverseZones(); got != tt.n {
			t.Errorf("%s: count = %d, want %d", tt.cidr, got, tt.n)
		}
		zones, _ := s.ReverseZones(0)
		for _, z := range zones {
			if z.Prefix.Bits() != tt.bits {
				t.Fatalf("%s: zone %s has bits %d, want %d", tt.cidr, z.Prefix, z.Prefix.Bits(), tt.bits)
			}
		}
	}
}
