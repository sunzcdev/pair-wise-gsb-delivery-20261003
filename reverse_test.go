package netipx

import (
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
)

func mustReverseZone(t *testing.T, name string) ReverseZone {
	t.Helper()
	zone, err := ParseReverseZone(name)
	if err != nil {
		t.Fatalf("ParseReverseZone(%q): %v", name, err)
	}
	return zone
}

func mustPrefix(s string) netip.Prefix {
	return netip.MustParsePrefix(s)
}

func hostReverseZone(s string) ReverseZone {
	addr := netip.MustParseAddr(s)
	return newReverseZone(netip.PrefixFrom(addr, addr.BitLen()))
}

func addZones(t *testing.T, b *IPSetBuilder, zones []ReverseZone) {
	t.Helper()
	for _, zone := range zones {
		b.AddPrefix(zone.Prefix())
	}
}

func assertReverseZones(t *testing.T, s *IPSet, want []ReverseZone) {
	t.Helper()

	if got := s.ReverseZoneCount(); got != len(want) {
		t.Fatalf("ReverseZoneCount = %d, want %d", got, len(want))
	}

	got, err := s.ReverseZones(0)
	if err != nil {
		t.Fatalf("ReverseZones: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ReverseZones returned %d zones, want %d", len(got), len(want))
	}

	var union IPSetBuilder
	for i, zone := range got {
		parsed := mustReverseZone(t, zone.Name())
		if zone.Name() != want[i].Name() {
			t.Errorf("zone %d name = %q, want %q", i, zone.Name(), want[i].Name())
		}
		if zone.Prefix() != want[i].Prefix() {
			t.Errorf("zone %d prefix = %v, want %v", i, zone.Prefix(), want[i].Prefix())
		}
		if parsed.Name() != zone.Name() || parsed.Prefix() != zone.Prefix() {
			t.Errorf("zone %d round-trip mismatch: got %q, %v", i, parsed.Name(), parsed.Prefix())
		}
		union.AddPrefix(parsed.Prefix())
	}

	unionSet, err := union.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	if !unionSet.Equal(s) {
		t.Fatalf("zone prefix union = %v, want %v", unionSet.Prefixes(), s.Prefixes())
	}
}

func TestReverseZonesBoundaryExamples(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		want   []string
	}{
		{
			name:   "ipv4 /23",
			prefix: "10.0.0.0/23",
			want: []string{
				"0.0.10.in-addr.arpa.",
				"1.0.10.in-addr.arpa.",
			},
		},
		{
			name:   "ipv6 /63",
			prefix: "2001:db8::/63",
			want: []string{
				"0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",
				"1.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b IPSetBuilder
			b.AddPrefix(mustPrefix(tt.prefix))
			s, err := b.IPSet()
			if err != nil {
				t.Fatal(err)
			}

			want := make([]ReverseZone, len(tt.want))
			for i, name := range tt.want {
				zone := mustReverseZone(t, name)
				if zone.Prefix().Bits() != reverseZoneBoundaryBits(mustPrefix(tt.prefix)) {
					t.Fatalf("parsed zone %s has boundary %d", name, zone.Prefix().Bits())
				}
				want[i] = zone
			}
			assertReverseZones(t, s, want)
		})
	}
}

func TestReverseZonesIPv4Slash25(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(mustPrefix("192.0.2.0/25"))
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}

	want := make([]ReverseZone, 128)
	for i := 0; i < 128; i++ {
		addr := netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})
		prefix := mustPrefix("192.0.2." + strconv.Itoa(i) + "/32")
		zone := newReverseZone(netip.PrefixFrom(addr, 32))
		if zone.Prefix() != prefix {
			t.Fatalf("zone %s prefix = %v", zone.Name(), zone.Prefix())
		}
		want[i] = zone
	}

	assertReverseZones(t, s, want)
}

func TestReverseZonesEmptyAndRoots(t *testing.T) {
	var empty IPSet
	assertReverseZones(t, &empty, nil)
	if got := (*IPSet)(nil).ReverseZoneCount(); got != 0 {
		t.Fatalf("nil ReverseZoneCount = %d, want 0", got)
	}
	zones, err := (*IPSet)(nil).ReverseZones(0)
	if err != nil || zones == nil || len(zones) != 0 {
		t.Fatalf("nil ReverseZones = %v zones, %v; want empty non-nil slice", zones, err)
	}

	var b IPSetBuilder
	b.AddPrefix(mustPrefix("::/0"))
	b.AddPrefix(mustPrefix("0.0.0.0/0"))
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}

	assertReverseZones(t, s, []ReverseZone{
		mustReverseZone(t, "in-addr.arpa."),
		mustReverseZone(t, "ip6.arpa."),
	})
}

func TestReverseZonesNonPrefixRanges(t *testing.T) {
	var b IPSetBuilder
	b.AddRange(IPRangeFrom(
		netip.MustParseAddr("192.0.2.0"),
		netip.MustParseAddr("192.0.2.4"),
	))
	b.AddRange(IPRangeFrom(
		netip.MustParseAddr("2001:db8::"),
		netip.MustParseAddr("2001:db8::4"),
	))
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}

	want := make([]ReverseZone, 0, 10)
	for i := 0; i < 5; i++ {
		want = append(want, hostReverseZone("192.0.2."+strconv.Itoa(i)))
	}
	for i := 0; i < 5; i++ {
		want = append(want, hostReverseZone("2001:db8::"+strconv.Itoa(i)))
	}

	assertReverseZones(t, s, want)
}

func TestReverseZonesLimit(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(mustPrefix("192.0.2.0/25"))
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}

	if zones, err := s.ReverseZones(127); err == nil || zones != nil {
		t.Fatalf("ReverseZones(127) = %d zones, %v; want limit error", len(zones), err)
	} else {
		var limitErr *ReverseZoneLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("error is %T, want *ReverseZoneLimitError", err)
		}
		if limitErr.Count != 128 || limitErr.Max != 127 {
			t.Fatalf("limit error = Count:%d Max:%d", limitErr.Count, limitErr.Max)
		}
	}

	zones, err := s.ReverseZones(128)
	if err != nil || len(zones) != 128 {
		t.Fatalf("ReverseZones(128) = %d zones, %v", len(zones), err)
	}

	if _, err := s.ReverseZones(-1); err == nil {
		t.Fatal("ReverseZones(-1) succeeded; want error")
	}
}

func TestParseReverseZone(t *testing.T) {
	valid := []struct {
		name string
		cidr string
	}{
		{"IN-ADDR.ARPA", "0.0.0.0/0"},
		{"in-addr.arpa.", "0.0.0.0/0"},
		{"2.0.192.in-addr.arpa", "192.0.2.0/24"},
		{"IP6.ARPA", "::/0"},
		{"0.0.0.0.0.0.0.0.8.B.D.0.1.0.0.2.IP6.ARPA.", "2001:db8::/64"},
	}
	for _, tt := range valid {
		zone, err := ParseReverseZone(tt.name)
		if err != nil {
			t.Errorf("ParseReverseZone(%q): %v", tt.name, err)
			continue
		}
		if zone.Prefix() != mustPrefix(tt.cidr) {
			t.Errorf("ParseReverseZone(%q) = %v, want %s", tt.name, zone.Prefix(), tt.cidr)
		}
	}

	invalid := []string{
		"",
		"arpa.",
		"example.com.",
		"02.0.192.in-addr.arpa.",
		"1.2.3.4.5.in-addr.arpa.",
		"x.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa.",
	}
	for _, name := range invalid {
		if _, err := ParseReverseZone(name); err == nil {
			t.Errorf("ParseReverseZone(%q) succeeded; want error", name)
		}
	}
}

func TestReverseZonesAreExactPrefixes(t *testing.T) {
	var original IPSetBuilder
	original.AddPrefix(mustPrefix("192.0.2.0/25"))
	original.AddPrefix(mustPrefix("10.0.0.0/23"))
	original.AddPrefix(mustPrefix("2001:db8::/63"))
	original.RemovePrefix(mustPrefix("10.0.1.128/25"))
	s, err := original.IPSet()
	if err != nil {
		t.Fatal(err)
	}

	zones, err := s.ReverseZones(0)
	if err != nil {
		t.Fatal(err)
	}

	var rebuilt IPSetBuilder
	addZones(t, &rebuilt, zones)
	rebuiltSet, err := rebuilt.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	if !rebuiltSet.Equal(s) {
		t.Fatalf("zones cover %v, want %v", rebuiltSet.Prefixes(), s.Prefixes())
	}

	var widened IPSetBuilder
	for _, prefix := range s.Prefixes() {
		if prefix.Addr().Is4() && prefix.Bits() > 24 {
			widened.AddPrefix(netip.PrefixFrom(prefix.Addr(), 24))
		} else {
			widened.AddPrefix(prefix)
		}
	}
	widenedSet, _ := widened.IPSet()
	if widenedSet.Equal(rebuiltSet) {
		t.Fatal("reverse zones widened a non-octet-aligned IPv4 prefix to a /24")
	}
}

func TestReverseZonesOrder(t *testing.T) {
	var b IPSetBuilder
	b.AddPrefix(mustPrefix("2001:db8::/128"))
	b.AddPrefix(mustPrefix("192.0.2.0/32"))
	b.AddPrefix(mustPrefix("192.0.2.1/32"))
	b.AddPrefix(mustPrefix("2001:db8::1/128"))
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ReverseZones(0)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"0.2.0.192.in-addr.arpa.",
		"1.2.0.192.in-addr.arpa.",
	}
	wantNames = append(wantNames,
		hostReverseZone("2001:db8::").Name(),
		hostReverseZone("2001:db8::1").Name(),
	)
	var gotNames []string
	for _, zone := range got {
		gotNames = append(gotNames, zone.Name())
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("order = %v, want %v", gotNames, wantNames)
	}
}
