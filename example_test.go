// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx_test

import (
	"fmt"
	"net/netip"

	"go4.org/netipx"
)

func ExampleIPSet() {
	var b netipx.IPSetBuilder

	b.AddPrefix(netip.MustParsePrefix("10.0.0.0/8"))
	b.RemovePrefix(netip.MustParsePrefix("10.0.0.0/16"))

	b.AddRange(netipx.IPRangeFrom(
		netip.MustParseAddr("fed0::0400"),
		netip.MustParseAddr("fed0::04ff"),
	))

	s, _ := b.IPSet()

	fmt.Println("Ranges:")
	for _, r := range s.Ranges() {
		fmt.Printf("  %s - %s\n", r.From(), r.To())
	}

	fmt.Println("Prefixes:")
	for _, p := range s.Prefixes() {
		fmt.Printf("  %s\n", p)
	}
	// Output:
	// Ranges:
	//   10.1.0.0 - 10.255.255.255
	//   fed0::400 - fed0::4ff
	// Prefixes:
	//   10.1.0.0/16
	//   10.2.0.0/15
	//   10.4.0.0/14
	//   10.8.0.0/13
	//   10.16.0.0/12
	//   10.32.0.0/11
	//   10.64.0.0/10
	//   10.128.0.0/9
	//   fed0::400/120
}

func ExampleIPSet_ReverseZones() {
	show := func(cidr string, max int, summary bool) {
		var b netipx.IPSetBuilder
		b.AddPrefix(netip.MustParsePrefix(cidr))
		s, _ := b.IPSet()

		zones, err := s.ReverseZones(max)
		if err != nil {
			fmt.Printf("%s -> %v\n", cidr, err)
			return
		}

		fmt.Printf("%s -> %d zone(s)\n", cidr, len(zones))
		if summary {
			for _, z := range zones[:2] {
				fmt.Printf("  %s (%s)\n", z.Zone, z.Prefix)
			}
			fmt.Println("  ...")
			z := zones[len(zones)-1]
			fmt.Printf("  %s (%s)\n", z.Zone, z.Prefix)
			return
		}
		for _, z := range zones {
			fmt.Printf("  %s (%s)\n", z.Zone, z.Prefix)
		}
	}

	// /23 ends on an octet boundary: two /24 zones.
	show("10.10.0.0/23", 0, false)
	// /25 does not end on an octet boundary: expand to 128 host
	// zones instead of enlarging to one /24. No RFC 2317 alias is used.
	show("192.168.1.128/25", 0, true)
	// /63 does not end on a nibble boundary: two /64 zones.
	show("2001:db8::/63", 0, false)
	// The maxZones guard reports the predicted count up front.
	show("192.168.1.128/25", 100, false)
	// Output:
	// 10.10.0.0/23 -> 2 zone(s)
	//   0.10.10.in-addr.arpa. (10.10.0.0/24)
	//   1.10.10.in-addr.arpa. (10.10.1.0/24)
	// 192.168.1.128/25 -> 128 zone(s)
	//   128.1.168.192.in-addr.arpa. (192.168.1.128/32)
	//   129.1.168.192.in-addr.arpa. (192.168.1.129/32)
	//   ...
	//   255.1.168.192.in-addr.arpa. (192.168.1.255/32)
	// 2001:db8::/63 -> 2 zone(s)
	//   0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa. (2001:db8::/64)
	//   1.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa. (2001:db8:0:1::/64)
	// 192.168.1.128/25 -> netipx: reverse zone count 128 exceeds limit 100
}
