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

func ExampleAnalyzeRules() {
	rules := []netipx.PrefixRule{
		{Prefix: netip.MustParsePrefix("10.1.0.0/16"), Action: netipx.ActionAllow},
		{Prefix: netip.MustParsePrefix("10.2.0.0/16"), Action: netipx.ActionDeny},
		{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Action: netipx.ActionDeny},
		{Prefix: netip.MustParsePrefix("10.1.5.0/24"), Action: netipx.ActionAllow},
	}
	a, _ := netipx.AnalyzeRules(rules, netipx.ActionAllow)

	for _, eff := range a.Effects() {
		switch eff.Shadow {
		case netipx.ShadowFull:
			fmt.Printf("rule %d (%s): fully shadowed\n", eff.Index, eff.Rule.Prefix)
		case netipx.ShadowPartial:
			fmt.Printf("rule %d (%s): partially shadowed (%s blocked, %s live)\n",
				eff.Index, eff.Rule.Prefix, eff.ShadowedAddr, eff.LiveAddr)
		default:
			fmt.Printf("rule %d (%s): live\n", eff.Index, eff.Rule.Prefix)
		}
	}

	act, idx, matched := a.Lookup(netip.MustParseAddr("10.1.2.3"))
	if matched {
		fmt.Printf("10.1.2.3: rule %d says %s\n", idx, act)
	} else {
		fmt.Printf("10.1.2.3: default %s\n", act)
	}
	// Output:
	// rule 0 (10.1.0.0/16): live
	// rule 1 (10.2.0.0/16): live
	// rule 2 (10.0.0.0/8): partially shadowed (10.1.0.0 blocked, 10.0.0.0 live)
	// rule 3 (10.1.5.0/24): fully shadowed
	// 10.1.2.3: rule 0 says allow
}
