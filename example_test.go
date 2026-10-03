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

func ExampleIPSet_CoveringPrefixes() {
	var b netipx.IPSetBuilder
	b.AddPrefix(netip.MustParsePrefix("2001:db8::1/128"))
	b.AddPrefix(netip.MustParsePrefix("2001:db8::2/128"))
	s, _ := b.IPSet()

	prefixes, extra, ok := s.CoveringPrefixes(1)
	fmt.Println("feasible:", ok)
	fmt.Println("prefixes:", prefixes)
	fmt.Println("extra addresses:", extra)
	// Output:
	// feasible: true
	// prefixes: [2001:db8::/126]
	// extra addresses: 2
}

func ExampleIPSet_CoveringPrefixes_dualStack() {
	var b netipx.IPSetBuilder
	b.AddPrefix(netip.MustParsePrefix("10.0.0.0/24"))
	b.AddPrefix(netip.MustParsePrefix("2001:db8::/32"))
	s, _ := b.IPSet()

	_, _, ok := s.CoveringPrefixes(1)
	fmt.Println("one prefix for both families:", ok)

	prefixes, extra, ok := s.CoveringPrefixes(2)
	fmt.Println("two prefixes:", ok, prefixes, "extra:", extra)
	// Output:
	// one prefix for both families: false
	// two prefixes: true [10.0.0.0/24 2001:db8::/32] extra: 0
}
