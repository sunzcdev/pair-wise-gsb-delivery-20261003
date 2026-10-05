// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"net/netip"
	"reflect"
	"testing"
)

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", s, err)
	}
	return p
}

func TestAnalyzeRulesBigNetFirst(t *testing.T) {
	rules := []Rule{
		{mustPrefix(t, "10.0.0.0/8"), Allow},
		{mustPrefix(t, "10.1.0.0/16"), Deny},
	}
	a, err := AnalyzeRules(rules, Deny)
	if err != nil {
		t.Fatal(err)
	}
	reps := a.Reports()
	if reps[0].Shadow != NotShadowed {
		t.Errorf("rule 0: got %v, want not-shadowed", reps[0].Shadow)
	}
	if reps[1].Shadow != FullyShadowed {
		t.Errorf("rule 1: got %v, want fully-shadowed", reps[1].Shadow)
	}
	if !reflect.DeepEqual(reps[1].ShadowedBy, []int{0}) {
		t.Errorf("rule 1 ShadowedBy = %v, want [0]", reps[1].ShadowedBy)
	}
	if len(reps[1].Effective.Ranges()) != 0 {
		t.Errorf("rule 1 Effective = %v, want empty", reps[1].Effective)
	}
	if got, want := reps[1].ShadowedWitness, netip.MustParseAddr("10.1.0.0"); got != want {
		t.Errorf("rule 1 ShadowedWitness = %v, want %v", got, want)
	}
	if reps[1].EffectiveWitness.IsValid() {
		t.Errorf("rule 1 EffectiveWitness = %v, want invalid", reps[1].EffectiveWitness)
	}
}

func TestAnalyzeRulesSmallNetFirst(t *testing.T) {
	rules := []Rule{
		{mustPrefix(t, "10.1.0.0/16"), Deny},
		{mustPrefix(t, "10.0.0.0/8"), Allow},
	}
	a, err := AnalyzeRules(rules, Deny)
	if err != nil {
		t.Fatal(err)
	}
	reps := a.Reports()
	if reps[1].Shadow != PartiallyShadowed {
		t.Fatalf("rule 1: got %v, want partially-shadowed", reps[1].Shadow)
	}
	if !reflect.DeepEqual(reps[1].ShadowedBy, []int{0}) {
		t.Errorf("rule 1 ShadowedBy = %v, want [0]", reps[1].ShadowedBy)
	}
	// The effective set is 10.0.0.0/8 minus 10.1.0.0/16.
	var want IPSetBuilder
	want.AddPrefix(mustPrefix(t, "10.0.0.0/8"))
	want.RemovePrefix(mustPrefix(t, "10.1.0.0/16"))
	wantSet, _ := want.IPSet()
	if !reps[1].Effective.Equal(wantSet) {
		t.Errorf("rule 1 Effective = %v, want %v", reps[1].Effective.Prefixes(), wantSet.Prefixes())
	}
	if got, want := reps[1].ShadowedWitness, netip.MustParseAddr("10.1.0.0"); got != want {
		t.Errorf("rule 1 ShadowedWitness = %v, want %v", got, want)
	}
	if got, want := reps[1].EffectiveWitness, netip.MustParseAddr("10.0.0.0"); got != want {
		t.Errorf("rule 1 EffectiveWitness = %v, want %v", got, want)
	}
	// The witnesses must behave as advertised under Lookup.
	if act, idx := a.Lookup(reps[1].ShadowedWitness); act != Deny || idx != 0 {
		t.Errorf("Lookup(shadowed witness) = %v, %d; want deny, 0", act, idx)
	}
	if act, idx := a.Lookup(reps[1].EffectiveWitness); act != Allow || idx != 1 {
		t.Errorf("Lookup(effective witness) = %v, %d; want allow, 1", act, idx)
	}
}

func TestAnalyzeRulesDuplicate(t *testing.T) {
	rules := []Rule{
		{mustPrefix(t, "192.0.2.0/24"), Deny},
		{mustPrefix(t, "192.0.2.0/24"), Allow},
	}
	a, err := AnalyzeRules(rules, Allow)
	if err != nil {
		t.Fatal(err)
	}
	reps := a.Reports()
	if reps[1].Shadow != FullyShadowed {
		t.Errorf("rule 1: got %v, want fully-shadowed", reps[1].Shadow)
	}
	if !reflect.DeepEqual(reps[1].ShadowedBy, []int{0}) {
		t.Errorf("rule 1 ShadowedBy = %v, want [0]", reps[1].ShadowedBy)
	}
	if act, idx := a.Lookup(netip.MustParseAddr("192.0.2.7")); act != Deny || idx != 0 {
		t.Errorf("Lookup = %v, %d; want deny, 0", act, idx)
	}
}

func TestAnalyzeRulesSameActionShadows(t *testing.T) {
	// An earlier allow rule shadows a later allow rule too.
	rules := []Rule{
		{mustPrefix(t, "10.0.0.0/8"), Allow},
		{mustPrefix(t, "10.1.0.0/16"), Allow},
	}
	a, err := AnalyzeRules(rules, Deny)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Reports()[1].Shadow; got != FullyShadowed {
		t.Errorf("rule 1: got %v, want fully-shadowed", got)
	}
}

func TestAnalyzeRulesMixedFamilies(t *testing.T) {
	rules := []Rule{
		{mustPrefix(t, "10.0.0.0/8"), Allow},
		{mustPrefix(t, "2001:db8::/32"), Deny},
		{mustPrefix(t, "10.1.0.0/16"), Deny},
		{mustPrefix(t, "2001:db8:1::/48"), Allow},
	}
	a, err := AnalyzeRules(rules, Deny)
	if err != nil {
		t.Fatal(err)
	}
	reps := a.Reports()
	// IPv6 rule 1 is unaffected by the earlier IPv4 rule.
	if reps[1].Shadow != NotShadowed {
		t.Errorf("rule 1: got %v, want not-shadowed", reps[1].Shadow)
	}
	// IPv4 rule 2 is fully shadowed by IPv4 rule 0 only.
	if reps[2].Shadow != FullyShadowed {
		t.Errorf("rule 2: got %v, want fully-shadowed", reps[2].Shadow)
	}
	if !reflect.DeepEqual(reps[2].ShadowedBy, []int{0}) {
		t.Errorf("rule 2 ShadowedBy = %v, want [0]", reps[2].ShadowedBy)
	}
	// IPv6 rule 3 is fully shadowed by IPv6 rule 1 only.
	if reps[3].Shadow != FullyShadowed {
		t.Errorf("rule 3: got %v, want fully-shadowed", reps[3].Shadow)
	}
	if !reflect.DeepEqual(reps[3].ShadowedBy, []int{1}) {
		t.Errorf("rule 3 ShadowedBy = %v, want [1]", reps[3].ShadowedBy)
	}
	if act, idx := a.Lookup(netip.MustParseAddr("2001:db8:1::1")); act != Deny || idx != 1 {
		t.Errorf("Lookup(v6) = %v, %d; want deny, 1", act, idx)
	}
	if act, idx := a.Lookup(netip.MustParseAddr("10.1.0.1")); act != Allow || idx != 0 {
		t.Errorf("Lookup(v4) = %v, %d; want allow, 0", act, idx)
	}
}

func TestAnalyzeRulesDefaultHoles(t *testing.T) {
	rules := []Rule{
		{mustPrefix(t, "192.0.2.0/25"), Allow},
		{mustPrefix(t, "2001:db8::/48"), Deny},
	}
	a, err := AnalyzeRules(rules, Deny)
	if err != nil {
		t.Fatal(err)
	}
	ds := a.DefaultSet()
	// The unruled half of 192.0.2.0/24 falls through to the default.
	if !ds.ContainsPrefix(mustPrefix(t, "192.0.2.128/25")) {
		t.Errorf("DefaultSet does not contain 192.0.2.128/25")
	}
	if ds.Contains(netip.MustParseAddr("192.0.2.0")) {
		t.Errorf("DefaultSet contains ruled address 192.0.2.0")
	}
	if !ds.Contains(netip.MustParseAddr("203.0.113.9")) {
		t.Errorf("DefaultSet does not contain unruled 203.0.113.9")
	}
	if !ds.Contains(netip.MustParseAddr("2001:db8:1::1")) {
		t.Errorf("DefaultSet does not contain unruled 2001:db8:1::1")
	}
	if ds.Contains(netip.MustParseAddr("2001:db8::1")) {
		t.Errorf("DefaultSet contains ruled 2001:db8::1")
	}
	if act, idx := a.Lookup(netip.MustParseAddr("192.0.2.200")); act != Deny || idx != -1 {
		t.Errorf("Lookup(hole) = %v, %d; want deny, -1", act, idx)
	}
	if a.DefaultAction() != Deny {
		t.Errorf("DefaultAction = %v, want deny", a.DefaultAction())
	}
}

// interpret is a naive per-address first-match interpreter used to
// cross-check the range-based analyzer.
func interpret(rules []Rule, def RuleAction, ip netip.Addr) (RuleAction, int) {
	for i, r := range rules {
		if r.Prefix.Contains(ip) {
			return r.Action, i
		}
	}
	return def, -1
}

func TestAnalyzeRulesCrossCheck(t *testing.T) {
	rules := []Rule{
		{mustPrefix(t, "192.0.2.0/26"), Allow},
		{mustPrefix(t, "192.0.2.0/25"), Deny},   // partially shadowed by rule 0
		{mustPrefix(t, "192.0.2.64/26"), Allow}, // fully shadowed by rule 1
		{mustPrefix(t, "192.0.2.128/26"), Allow},
		{mustPrefix(t, "192.0.2.0/24"), Deny},   // partially shadowed by rules 0-3
		{mustPrefix(t, "192.0.2.128/26"), Deny}, // duplicate of rule 3's range
		{mustPrefix(t, "2001:db8::ff00/120"), Deny},
		{mustPrefix(t, "2001:db8::/112"), Allow}, // partially shadowed by rule 6
	}
	const def = Allow
	a, err := AnalyzeRules(rules, def)
	if err != nil {
		t.Fatal(err)
	}
	reps := a.Reports()

	checkAddr := func(ip netip.Addr) {
		t.Helper()
		wantAct, wantIdx := interpret(rules, def, ip)
		gotAct, gotIdx := a.Lookup(ip)
		if gotAct != wantAct || gotIdx != wantIdx {
			t.Fatalf("Lookup(%v) = %v, %d; interpret = %v, %d", ip, gotAct, gotIdx, wantAct, wantIdx)
		}
		if wantIdx == -1 {
			if !a.DefaultSet().Contains(ip) {
				t.Fatalf("DefaultSet missing %v", ip)
			}
			return
		}
		if !reps[wantIdx].Effective.Contains(ip) {
			t.Fatalf("rule %d Effective missing %v", wantIdx, ip)
		}
		for i, rep := range reps {
			if i != wantIdx && rep.Effective.Contains(ip) {
				t.Fatalf("rule %d Effective unexpectedly contains %v", i, ip)
			}
		}
	}

	// Walk every address of the small 192.0.2.0/24 and
	// 2001:db8::/112 spaces without relying on the analyzer.
	ip := netip.MustParseAddr("192.0.2.0")
	for i := 0; i < 256; i++ {
		checkAddr(ip)
		ip = ip.Next()
	}
	ip = netip.MustParseAddr("2001:db8::")
	for i := 0; i < 65536; i++ {
		checkAddr(ip)
		ip = ip.Next()
	}

	// Every witness must behave as advertised.
	for _, rep := range reps {
		if rep.Shadow != NotShadowed {
			if _, idx := a.Lookup(rep.ShadowedWitness); idx >= rep.Index {
				t.Errorf("rule %d shadowed witness %v decided by rule %d", rep.Index, rep.ShadowedWitness, idx)
			}
			if !rep.Rule.Prefix.Contains(rep.ShadowedWitness) {
				t.Errorf("rule %d shadowed witness %v outside prefix", rep.Index, rep.ShadowedWitness)
			}
		}
		if rep.Shadow != FullyShadowed {
			if _, idx := a.Lookup(rep.EffectiveWitness); idx != rep.Index {
				t.Errorf("rule %d effective witness %v decided by rule %d", rep.Index, rep.EffectiveWitness, idx)
			}
		}
	}

	// Spot-check the expected shadow kinds.
	wantKinds := []ShadowKind{
		NotShadowed,
		PartiallyShadowed,
		FullyShadowed,
		NotShadowed,
		PartiallyShadowed,
		FullyShadowed,
		NotShadowed,
		PartiallyShadowed,
	}
	for i, want := range wantKinds {
		if reps[i].Shadow != want {
			t.Errorf("rule %d: got %v, want %v", i, reps[i].Shadow, want)
		}
	}
}

func TestAnalyzeRulesInvalidPrefix(t *testing.T) {
	if _, err := AnalyzeRules([]Rule{{netip.Prefix{}, Allow}}, Deny); err == nil {
		t.Fatal("expected error for invalid prefix")
	}
}
