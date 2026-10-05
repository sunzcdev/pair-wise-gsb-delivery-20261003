// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx_test

import (
	"errors"
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

// Device ACLs sometimes express matches as an address plus a
// per-bit mask rather than a CIDR prefix. AddrMask converts such a
// pattern into an exact IPSet.
func ExampleAddrMaskFrom() {
	addr := netip.MustParseAddr("10.0.20.0")
	mask := netip.MustParseAddr("255.0.255.0")

	am, err := netipx.AddrMaskFrom(addr, mask)
	if err != nil {
		panic(err)
	}

	fmt.Println(am.Contains(netip.MustParseAddr("10.9.20.3")))
	fmt.Println(am.Contains(netip.MustParseAddr("10.0.21.0")))

	set, count, err := am.IPSet(-1)
	if err != nil {
		panic(err)
	}
	fmt.Println("exact range count:", count)
	fmt.Println("first range:", set.Ranges()[0])

	// Refusing to build a result that is too large.
	_, _, err = am.IPSet(int(count.Int64() - 1))
	fmt.Println(errors.Is(err, netipx.ErrTooManyRanges))

	// Output:
	// true
	// false
	// exact range count: 256
	// first range: 10.0.20.0-10.0.20.255
	// true
}
