// Copyright 2026 The go4 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"math/rand"
	"net/netip"
	"testing"
)

func rulesFrom(specs []string, actions []RuleAction) []PrefixRule {
	rules := make([]PrefixRule, len(specs))
	for i, s := range specs {
		rules[i] = PrefixRule{Prefix: mustIPPrefix(s), Action: actions[i]}
	}
	return rules
}

func prefixSet(t *testing.T, pfx ...string) *IPSet {
	t.Helper()
	var b IPSetBuilder
	for _, p := range pfx {
		b.AddPrefix(mustIPPrefix(p))
	}
	s, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAnalyzeRulesFixed(t *testing.T) {
	allow, deny := ActionAllow, ActionDeny

	t.Run("big_prefix_first", func(t *testing.T) {
		rules := rulesFrom(
			[]string{"10.0.0.0/8", "10.1.0.0/16"},
			[]RuleAction{deny, allow},
		)
		a, err := AnalyzeRules(rules, allow)
		if err != nil {
			t.Fatal(err)
		}
		effects := a.Effects()

		if got := effects[0].Shadow; got != ShadowNone {
			t.Errorf("rule 0 shadow = %s, want none", got)
		}
		if !effects[0].Hit.Equal(prefixSet(t, "10.0.0.0/8")) {
			t.Errorf("rule 0 hit = %v, want 10.0.0.0/8", effects[0].Hit.Prefixes())
		}
		if got := effects[1].Shadow; got != ShadowFull {
			t.Errorf("rule 1 shadow = %s, want full", got)
		}
		if len(effects[1].Hit.Ranges()) != 0 {
			t.Errorf("fully shadowed rule hit = %v, want empty", effects[1].Hit.Prefixes())
		}
		if w := effects[1].ShadowedAddr; !mustIPPrefix("10.1.0.0/16").Contains(w) {
			t.Errorf("full shadow witness %v not in rule prefix", w)
		}

		if act, idx, ok := a.Lookup(mustIP("10.1.2.3")); !(act == deny && idx == 0 && ok) {
			t.Errorf("lookup 10.1.2.3 = (%s,%d,%v), want (deny,0,true)", act, idx, ok)
		}
		if act, idx, ok := a.Lookup(mustIP("10.2.3.4")); !(act == deny && idx == 0 && ok) {
			t.Errorf("lookup 10.2.3.4 = (%s,%d,%v), want (deny,0,true)", act, idx, ok)
		}
		if act, idx, ok := a.Lookup(mustIP("192.0.2.1")); !(act == allow && idx == -1 && !ok) {
			t.Errorf("lookup 192.0.2.1 = (%s,%d,%v), want (allow,-1,false)", act, idx, ok)
		}
	})

	t.Run("small_prefix_first", func(t *testing.T) {
		rules := rulesFrom(
			[]string{"10.1.0.0/16", "10.0.0.0/8"},
			[]RuleAction{allow, deny},
		)
		a, err := AnalyzeRules(rules, deny)
		if err != nil {
			t.Fatal(err)
		}
		effects := a.Effects()

		if got := effects[0].Shadow; got != ShadowNone {
			t.Errorf("rule 0 shadow = %s, want none", got)
		}
		if got := effects[1].Shadow; got != ShadowPartial {
			t.Fatalf("rule 1 shadow = %s, want partial", got)
		}
		// The live part of the /8 is everything but 10.1.0.0/16.
		var want IPSetBuilder
		want.AddPrefix(mustIPPrefix("10.0.0.0/8"))
		want.RemovePrefix(mustIPPrefix("10.1.0.0/16"))
		wantSet, _ := want.IPSet()
		if !effects[1].Hit.Equal(wantSet) {
			t.Errorf("rule 1 hit = %v, want 10.0.0.0/8 minus 10.1.0.0/16", effects[1].Hit.Prefixes())
		}
		sw, lw := effects[1].ShadowedAddr, effects[1].LiveAddr
		if !rules[1].Prefix.Contains(sw) || effects[1].Hit.Contains(sw) {
			t.Errorf("shadowed witness %v must be in prefix but out of hit set", sw)
		}
		if !rules[1].Prefix.Contains(lw) || !effects[1].Hit.Contains(lw) {
			t.Errorf("live witness %v must be in prefix and hit set", lw)
		}

		if act, idx, ok := a.Lookup(mustIP("10.1.2.3")); !(act == allow && idx == 0 && ok) {
			t.Errorf("lookup 10.1.2.3 = (%s,%d,%v), want (allow,0,true)", act, idx, ok)
		}
		if act, idx, ok := a.Lookup(mustIP("10.2.3.4")); !(act == deny && idx == 1 && ok) {
			t.Errorf("lookup 10.2.3.4 = (%s,%d,%v), want (deny,1,true)", act, idx, ok)
		}
	})

	t.Run("duplicate_rules_same_action", func(t *testing.T) {
		// An earlier rule with the same action still shadows.
		rules := rulesFrom(
			[]string{"10.0.0.0/8", "10.0.0.0/8"},
			[]RuleAction{deny, deny},
		)
		a, err := AnalyzeRules(rules, allow)
		if err != nil {
			t.Fatal(err)
		}
		effects := a.Effects()
		if effects[0].Shadow != ShadowNone || effects[1].Shadow != ShadowFull {
			t.Errorf("shadows = %s, %s; want none, full",
				effects[0].Shadow, effects[1].Shadow)
		}
		if act, idx, ok := a.Lookup(mustIP("10.9.9.9")); !(act == deny && idx == 0 && ok) {
			t.Errorf("lookup = (%s,%d,%v), want (deny,0,true)", act, idx, ok)
		}
	})

	t.Run("mixed_families", func(t *testing.T) {
		rules := rulesFrom(
			[]string{"0.0.0.0/0", "::/0", "10.0.0.0/8", "2001:db8::/32"},
			[]RuleAction{deny, deny, allow, allow},
		)
		a, err := AnalyzeRules(rules, allow)
		if err != nil {
			t.Fatal(err)
		}
		effects := a.Effects()
		wantShadows := []ShadowKind{ShadowNone, ShadowNone, ShadowFull, ShadowFull}
		for i, want := range wantShadows {
			if effects[i].Shadow != want {
				t.Errorf("rule %d (%s) shadow = %s, want %s",
					i, rules[i].Prefix, effects[i].Shadow, want)
			}
			if effects[i].Index != i {
				t.Errorf("rule %d lost its original index: %d", i, effects[i].Index)
			}
		}
		// First-match, not longest-prefix: the v4/v6 catch-all wins.
		if act, idx, ok := a.Lookup(mustIP("10.1.2.3")); !(act == deny && idx == 0 && ok) {
			t.Errorf("lookup 10.1.2.3 = (%s,%d,%v), want (deny,0,true)", act, idx, ok)
		}
		if act, idx, ok := a.Lookup(mustIP("2001:db8::1")); !(act == deny && idx == 1 && ok) {
			t.Errorf("lookup 2001:db8::1 = (%s,%d,%v), want (deny,1,true)", act, idx, ok)
		}

		// Independently of each other, small prefixes of one family
		// are NOT shadowed by the other family's catch-all.
		rules2 := rulesFrom(
			[]string{"0.0.0.0/0", "2001:db8::/32"},
			[]RuleAction{deny, allow},
		)
		a2, err := AnalyzeRules(rules2, deny)
		if err != nil {
			t.Fatal(err)
		}
		if a2.Effects()[1].Shadow != ShadowNone {
			t.Errorf("v6 rule shadowed by v4 catch-all: %s", a2.Effects()[1].Shadow)
		}
		if act, idx, ok := a2.Lookup(mustIP("2001:db8::1")); !(act == allow && idx == 1 && ok) {
			t.Errorf("lookup 2001:db8::1 = (%s,%d,%v), want (allow,1,true)", act, idx, ok)
		}
	})

	t.Run("default_action_holes", func(t *testing.T) {
		rules := rulesFrom(
			[]string{"10.0.0.0/8"},
			[]RuleAction{allow},
		)
		a, err := AnalyzeRules(rules, deny)
		if err != nil {
			t.Fatal(err)
		}
		// Holes contain everything except 10.0.0.0/8.
		if a.Unmatched().Contains(mustIP("10.0.0.1")) {
			t.Errorf("10.0.0.1 reported unmatched")
		}
		if !a.Unmatched().Contains(mustIP("11.0.0.1")) {
			t.Errorf("11.0.0.1 should be in default-action hole")
		}
		if !a.Unmatched().Contains(mustIP("2001:db8::1")) {
			t.Errorf("2001:db8::1 should be in default-action hole")
		}
		if act, idx, ok := a.Lookup(mustIP("192.0.2.1")); !(act == deny && idx == -1 && !ok) {
			t.Errorf("hole lookup = (%s,%d,%v), want (deny,-1,false)", act, idx, ok)
		}
		if act, _, ok := a.Lookup(mustIP("10.5.5.5")); !(act == allow && ok) {
			t.Errorf("covered lookup = (%s,%v), want (allow,true)", act, ok)
		}
	})

	t.Run("same_action_partial", func(t *testing.T) {
		// Same action as the earlier rule still produces partial
		// shadowing with witnesses.
		rules := rulesFrom(
			[]string{"10.1.0.0/16", "10.0.0.0/8"},
			[]RuleAction{allow, allow},
		)
		a, err := AnalyzeRules(rules, deny)
		if err != nil {
			t.Fatal(err)
		}
		eff := a.Effects()[1]
		if eff.Shadow != ShadowPartial {
			t.Fatalf("shadow = %s, want partial", eff.Shadow)
		}
		if !rules[0].Prefix.Contains(eff.ShadowedAddr) {
			t.Errorf("shadowed witness %v not in earlier rule", eff.ShadowedAddr)
		}
		if !eff.Hit.Contains(eff.LiveAddr) {
			t.Errorf("live witness %v not in hit set", eff.LiveAddr)
		}
	})

	t.Run("partial_shadow_witnesses", func(t *testing.T) {
		// The first part of the /24 is blocked, a middle chunk stays
		// live, and witnesses must come from those distinct chunks.
		rules := rulesFrom(
			[]string{"192.0.2.0/26", "192.0.2.128/26", "192.0.2.0/24"},
			[]RuleAction{deny, deny, allow},
		)
		a, err := AnalyzeRules(rules, deny)
		if err != nil {
			t.Fatal(err)
		}
		eff := a.Effects()[2]
		if eff.Shadow != ShadowPartial {
			t.Fatalf("shadow = %s, want partial", eff.Shadow)
		}
		if eff.ShadowedAddr != mustIP("192.0.2.0") {
			t.Errorf("shadowed witness = %v, want 192.0.2.0", eff.ShadowedAddr)
		}
		if eff.LiveAddr != mustIP("192.0.2.64") {
			t.Errorf("live witness = %v, want 192.0.2.64", eff.LiveAddr)
		}
	})
}

func TestAnalyzeRulesInvalid(t *testing.T) {
	// Invalid rules are reported but do not invalidate the analysis.
	rules := []PrefixRule{
		{Prefix: mustIPPrefix("10.0.0.0/8"), Action: ActionAllow},
		{Prefix: netip.Prefix{}, Action: ActionAllow},
		{Prefix: mustIPPrefix("::/0"), Action: RuleAction(42)},
	}
	a, err := AnalyzeRules(rules, ActionDeny)
	if err == nil {
		t.Fatal("want error for invalid rules, got nil")
	}
	if len(a.Effects()) != len(rules) {
		t.Fatalf("effects = %d, want %d", len(a.Effects()), len(rules))
	}
	if a.Effects()[0].Shadow != ShadowNone {
		t.Errorf("valid rule marked shadowed: %s", a.Effects()[0].Shadow)
	}
	if a.Effects()[1].Hit == nil || a.Effects()[2].Hit == nil {
		t.Errorf("invalid rule effects must carry a non-nil empty Hit")
	}
	if act, idx, ok := a.Lookup(mustIP("10.0.0.1")); !(act == ActionAllow && idx == 0 && ok) {
		t.Errorf("lookup = (%s,%d,%v), want (allow,0,true)", act, idx, ok)
	}

	if _, err := AnalyzeRules(nil, RuleAction(7)); err == nil {
		t.Fatal("want error for invalid default action")
	}

	// Zoned and invalid addresses never match a rule.
	a2, err := AnalyzeRules([]PrefixRule{
		{Prefix: mustIPPrefix("::/0"), Action: ActionAllow},
	}, ActionDeny)
	if err != nil {
		t.Fatal(err)
	}
	if _, idx, ok := a2.Lookup(netip.Addr{}); ok || idx != -1 {
		t.Errorf("invalid addr lookup = (%d,%v), want (-1,false)", idx, ok)
	}
	if _, idx, ok := a2.Lookup(mustIP("fe80::1").WithZone("eth0")); ok || idx != -1 {
		t.Errorf("zoned addr lookup = (%d,%v), want (-1,false)", idx, ok)
	}
}

// randomPrefixes builds a random list of rules whose prefixes all sit
// inside a small, fully enumerable universe.
func randomRules(rng *rand.Rand, base netip.Addr, bits, size int) []PrefixRule {
	n := 4 + rng.Intn(9) // 4..12 rules
	rules := make([]PrefixRule, n)
	for i := range rules {
		pLen := bits + rng.Intn(size+1)
		off := rng.Intn(1 << size)
		// Round the random offset down to the prefix granularity.
		hostBits := pLen - bits
		if hostBits < size {
			off &^= (1 << (size - hostBits)) - 1
		}
		addr := offsetAddr(base, uint64(off))
		rules[i] = PrefixRule{
			Prefix: netip.PrefixFrom(addr, pLen),
			Action: RuleAction(rng.Intn(2)),
		}
	}
	return rules
}

// offsetAddr returns base + off using the package's uint128 helpers, so
// it works for both address families.
func offsetAddr(base netip.Addr, off uint64) netip.Addr {
	u := u128From16(base.As16())
	u = uint128{hi: u.hi, lo: u.lo + off}
	if base.Is4() {
		return u.IP4()
	}
	return u.IP6()
}

// interpret is the reference per-address first-match interpreter over
// a small universe. It enumerates every address exactly once.
func interpret(t *testing.T, rules []PrefixRule, base netip.Addr, size int, def RuleAction) (
	action []RuleAction, winner []int, hits []map[uint64]bool,
) {
	total := uint64(1) << size
	action = make([]RuleAction, total)
	winner = make([]int, total)
	hits = make([]map[uint64]bool, len(rules))
	for i := range hits {
		hits[i] = map[uint64]bool{}
	}
	for x := uint64(0); x < total; x++ {
		addr := offsetAddr(base, x)
		action[x], winner[x] = def, -1
		for i, r := range rules {
			if r.Prefix.Contains(addr) {
				action[x] = r.Action
				winner[x] = i
				hits[i][x] = true
				break
			}
		}
	}
	return
}

func checkAnalysis(t *testing.T, rules []PrefixRule, def RuleAction, base netip.Addr, size int) [3]int {
	t.Helper()
	var kinds [3]int
	a, err := AnalyzeRules(rules, def)
	if err != nil {
		t.Fatalf("AnalyzeRules: %v", err)
	}
	total := uint64(1) << size
	refAction, refWinner, refHits := interpret(t, rules, base, size, def)
	effects := a.Effects()
	baseBits := 0
	if base.Is4() {
		baseBits = 24
	} else {
		baseBits = 120
	}

	for x := uint64(0); x < total; x++ {
		addr := offsetAddr(base, x)
		gotAct, gotIdx, gotOK := a.Lookup(addr)
		wantOK := refWinner[x] != -1
		if gotOK != wantOK || gotIdx != refWinner[x] || (wantOK && gotAct != refAction[x]) ||
			(!wantOK && gotAct != def) {
			t.Fatalf("Lookup(%v) = (%s,%d,%v), want (%s,%d,%v)",
				addr, gotAct, gotIdx, gotOK, refAction[x], refWinner[x], wantOK)
		}

		for i := range rules {
			if got := effects[i].Hit.Contains(addr); got != refHits[i][x] {
				t.Fatalf("rule %d Hit.Contains(%v) = %v, want %v",
					i, addr, got, refHits[i][x])
			}
		}

		// The unmatched set is exactly the default-action hole.
		if got := a.Unmatched().Contains(addr); got != !wantOK {
			t.Fatalf("Unmatched.Contains(%v) = %v, want %v", addr, got, !wantOK)
		}
	}

	for i := range rules {
		kind := ShadowNone
		if len(refHits[i]) == 0 {
			kind = ShadowFull
		} else if uint64(len(refHits[i])) < uint64(1)<<(size-(rules[i].Prefix.Bits()-baseBits)) {
			kind = ShadowPartial
		}
		if effects[i].Shadow != kind {
			t.Fatalf("rule %d %s shadow = %s, want %s",
				i, rules[i].Prefix, effects[i].Shadow, kind)
		}
		kinds[kind]++
		if effects[i].Index != i {
			t.Fatalf("rule %d original index lost: %d", i, effects[i].Index)
		}
		switch kind {
		case ShadowFull:
			if !rules[i].Prefix.Contains(effects[i].ShadowedAddr) {
				t.Fatalf("rule %d full shadow witness %v out of prefix",
					i, effects[i].ShadowedAddr)
			}
		case ShadowPartial:
			sw, lw := effects[i].ShadowedAddr, effects[i].LiveAddr
			if !rules[i].Prefix.Contains(sw) || effects[i].Hit.Contains(sw) {
				t.Fatalf("rule %d shadowed witness %v invalid", i, sw)
			}
			if !rules[i].Prefix.Contains(lw) || !effects[i].Hit.Contains(lw) {
				t.Fatalf("rule %d live witness %v invalid", i, lw)
			}
			if sw == lw {
				t.Fatalf("rule %d witnesses coincide: %v", i, sw)
			}
		case ShadowNone:
			if effects[i].ShadowedAddr.IsValid() || effects[i].LiveAddr.IsValid() {
				t.Fatalf("rule %d unshadowed but carries witnesses", i)
			}
		}
	}
	return kinds
}

func TestAnalyzeRulesCrossCheck(t *testing.T) {
	// Deterministic fuzzing: cross-check the analyzer against a
	// per-address interpreter in a small v4 and v6 universe.
	rng := rand.New(rand.NewSource(1))
	const iter = 300
	const size = 8 // 256 addresses per family
	v4Base := mustIP("192.0.2.0")
	v6Base := mustIP("2001:db8::")
	var v4Kinds, v6Kinds [3]int

	for n := 0; n < iter; n++ {
		v4Rules := randomRules(rng, v4Base, 24, size)
		v6Rules := randomRules(rng, v6Base, 120, size)
		def := RuleAction(rng.Intn(2))

		// Per-family analyses: full audit vs the interpreter.
		t.Run("v4", func(t *testing.T) {
			v4Kinds = addKinds(v4Kinds, checkAnalysis(t, v4Rules, def, v4Base, size))
		})
		t.Run("v6", func(t *testing.T) {
			v6Kinds = addKinds(v6Kinds, checkAnalysis(t, v6Rules, def, v6Base, size))
		})

		// Interleave the families in one analysis and confirm IPv4 and
		// IPv6 are audited independently (first-match never crosses
		// families), via Lookup.
		merged := make([]PrefixRule, 0, len(v4Rules)+len(v6Rules))
		i4, i6 := 0, 0
		for i4 < len(v4Rules) || i6 < len(v6Rules) {
			if i6 == len(v6Rules) || (i4 < len(v4Rules) && rng.Intn(2) == 0) {
				merged = append(merged, v4Rules[i4])
				i4++
			} else {
				merged = append(merged, v6Rules[i6])
				i6++
			}
		}
		ma, err := AnalyzeRules(merged, def)
		if err != nil {
			t.Fatalf("iter %d merged: %v", n, err)
		}
		checkLookup := func(base netip.Addr, familyRules []PrefixRule) {
			t.Helper()
			refAction, refWinner, _ := interpret(t, familyRules, base, size, def)
			local := make([]int, len(familyRules))
			k := 0
			for oi, r := range merged {
				if r.Prefix.Addr().Is4() == base.Is4() {
					local[k] = oi
					k++
				}
			}
			for x := uint64(0); x < 1<<size; x++ {
				addr := offsetAddr(base, x)
				act, idx, ok := ma.Lookup(addr)
				wantIdx, wantAct := -1, def
				if refWinner[x] != -1 {
					wantIdx = local[refWinner[x]]
					wantAct = refAction[x]
				}
				if ok != (wantIdx != -1) || idx != wantIdx || act != wantAct {
					t.Fatalf("iter %d merged Lookup(%v) = (%s,%d,%v), want (%s,%d)",
						n, addr, act, idx, ok, wantAct, wantIdx)
				}
			}
		}
		checkLookup(v4Base, v4Rules)
		checkLookup(v6Base, v6Rules)
	}
	for fam, kinds := range map[string][3]int{"v4": v4Kinds, "v6": v6Kinds} {
		for k, n := range kinds {
			if n == 0 {
				t.Fatalf("%s fuzz never exercised shadow kind %d", fam, k)
			}
		}
		t.Logf("%s shadow-kind counts: none=%d partial=%d full=%d",
			fam, kinds[ShadowNone], kinds[ShadowPartial], kinds[ShadowFull])
	}
}

func addKinds(a, b [3]int) [3]int {
	for i := range a {
		a[i] += b[i]
	}
	return a
}
