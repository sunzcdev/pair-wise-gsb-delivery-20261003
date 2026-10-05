// Copyright 2026 The go4 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx // import "go4.org/netipx"

import (
	"fmt"
	"net/netip"
	"sort"
)

// RuleAction is the allow/deny decision applied to the addresses a
// PrefixRule matches.
type RuleAction uint8

const (
	// ActionDeny denies matched addresses.
	ActionDeny RuleAction = iota
	// ActionAllow permits matched addresses.
	ActionAllow
)

// String returns "allow" or "deny".
func (a RuleAction) String() string {
	switch a {
	case ActionAllow:
		return "allow"
	case ActionDeny:
		return "deny"
	default:
		return fmt.Sprintf("invalid-action(%d)", uint8(a))
	}
}

// PrefixRule is one ordered allow/deny rule.
//
// A list of PrefixRules is evaluated in order using first-match
// semantics: the first rule whose Prefix contains an address decides
// that address, regardless of prefix length. Addresses matching no
// rule take the configured default action.
type PrefixRule struct {
	// Prefix is the CIDR this rule matches.
	Prefix netip.Prefix
	// Action is ActionAllow or ActionDeny.
	Action RuleAction
}

// ShadowKind describes how much of a rule is blocked by earlier rules.
type ShadowKind uint8

const (
	// ShadowNone means every address in the rule is still decided by it.
	ShadowNone ShadowKind = iota
	// ShadowPartial means some, but not all, addresses in the rule are
	// decided by earlier rules.
	ShadowPartial
	// ShadowFull means every address in the rule is decided by earlier
	// rules, so the rule can never fire.
	ShadowFull
)

// String returns "none", "partial" or "full".
func (k ShadowKind) String() string {
	switch k {
	case ShadowNone:
		return "none"
	case ShadowPartial:
		return "partial"
	case ShadowFull:
		return "full"
	default:
		return fmt.Sprintf("invalid-shadow(%d)", uint8(k))
	}
}

// RuleEffect reports what a single input rule ends up deciding after
// earlier rules have been applied.
type RuleEffect struct {
	// Index is the position of the rule in the input slice.
	Index int
	// Rule is the input rule.
	Rule PrefixRule
	// Hit is the set of addresses for which this rule is the first
	// match. It is empty when the rule is fully shadowed.
	Hit *IPSet
	// Shadow reports whether the rule is unshadowed, partially
	// shadowed, or fully shadowed by earlier rules.
	Shadow ShadowKind
	// ShadowedAddr witnesses the shadowing. When Shadow is
	// ShadowPartial it is an address in the rule's prefix that an
	// earlier rule decides first; when Shadow is ShadowFull it is any
	// address in the prefix. It is otherwise the zero Addr.
	ShadowedAddr netip.Addr
	// LiveAddr is set only when Shadow is ShadowPartial. It is an
	// address in the rule's prefix that this rule still decides.
	LiveAddr netip.Addr
}

// RuleAnalysis is the result of analyzing an ordered rule list.
//
// It is safe for concurrent use.
type RuleAnalysis struct {
	defaultAction RuleAction
	effects       []RuleEffect

	// holes[fam] is the family universe minus the union of all input
	// prefixes of that family: addresses that fall through to the
	// default action.
	holes [2]*IPSet
	// rules[fam] holds, for every range of addresses that has a
	// winning rule, the winning rule index. The ranges are sorted,
	// disjoint and non-adjacent where they carry different indices.
	rules [2][]taggedRange
}

// taggedRange is a range of addresses all decided by the rule with
// the given index.
type taggedRange struct {
	r     IPRange
	index int
}

// Address family indices used by RuleAnalysis's fixed-size arrays.
const (
	famV4 = 0
	famV6 = 1
)

var (
	v4Universe    = netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	v6Universe    = netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	emptyIPSet, _ = new(IPSetBuilder).IPSet()
)

// DefaultAction returns the action applied to addresses that no input
// rule matches.
func (a *RuleAnalysis) DefaultAction() RuleAction { return a.defaultAction }

// Effects returns one RuleEffect per input rule, indexed by the rules'
// original positions.
func (a *RuleAnalysis) Effects() []RuleEffect {
	out := make([]RuleEffect, len(a.effects))
	copy(out, a.effects)
	return out
}

// Unmatched returns the addresses that no input rule covers, i.e. the
// addresses for which Lookup reports the default action and no rule
// index.
func (a *RuleAnalysis) Unmatched() *IPSet {
	var b IPSetBuilder
	b.AddSet(a.holes[famV4])
	b.AddSet(a.holes[famV6])
	s, _ := b.IPSet()
	return s
}

// Lookup reports the first-match outcome for addr: the winning rule's
// action and its original index.
//
// If addr matches no rule, matched is false, index is -1 and action
// is the configured default action.
//
// If addr is invalid or carries an IPv6 zone (which prefixes do not
// track), matched is false and index is -1.
func (a *RuleAnalysis) Lookup(addr netip.Addr) (action RuleAction, index int, matched bool) {
	if !addr.IsValid() || addr.Zone() != "" {
		return a.defaultAction, -1, false
	}
	fam := familyOf(addr)
	ranges := a.rules[fam]
	i := sort.Search(len(ranges), func(i int) bool {
		return addr.Less(ranges[i].r.from)
	})
	if i == 0 || !ranges[i-1].r.contains(addr) {
		return a.defaultAction, -1, false
	}
	index = ranges[i-1].index
	return a.effects[index].Rule.Action, index, true
}

// AnalyzeRules runs a first-match audit of the ordered rules.
//
// defaultAction decides addresses not covered by any rule. IPv4 and
// IPv6 rules are analyzed independently; a rule never shadows a rule
// of the other address family.
//
// Every valid input rule produces one RuleEffect preserving the
// rule's original index. Rules with an invalid prefix or an invalid
// action are skipped, and the returned error describes them; the
// returned analysis still covers all valid rules.
//
// The analysis operates on address ranges; it never enumerates member
// addresses.
func AnalyzeRules(rules []PrefixRule, defaultAction RuleAction) (*RuleAnalysis, error) {
	a := &RuleAnalysis{
		defaultAction: defaultAction,
		effects:       make([]RuleEffect, len(rules)),
	}
	var errs multiErr
	switch defaultAction {
	case ActionDeny, ActionAllow:
	default:
		errs = append(errs, fmt.Errorf("invalid default action %d", uint8(defaultAction)))
	}

	var (
		covered [2]IPSetBuilder // union of input prefixes seen so far
		built   [2]bool
		tagged  [2][]taggedRange
	)

	for i, rule := range rules {
		eff := RuleEffect{Index: i, Rule: rule, Hit: emptyIPSet}
		p := rule.Prefix.Masked()
		switch {
		case rule.Action != ActionDeny && rule.Action != ActionAllow:
			errs = append(errs, fmt.Errorf("rule %d: invalid action %d", i, uint8(rule.Action)))
			a.effects[i] = eff
			continue
		case !p.IsValid():
			errs = append(errs, fmt.Errorf("rule %d: invalid prefix", i))
			a.effects[i] = eff
			continue
		}
		fam := familyOf(p.Addr())
		var coverSet *IPSet
		if built[fam] {
			coverSet, _ = covered[fam].IPSet()
		}

		var hb IPSetBuilder
		hb.AddPrefix(p)
		if coverSet != nil {
			hb.RemoveSet(coverSet)
		}
		hit, _ := hb.IPSet()
		eff.Hit = hit

		switch {
		case len(hit.rr) == 0:
			eff.Shadow = ShadowFull
			// One earlier rule owns the first address of the prefix.
			eff.ShadowedAddr = p.Addr()
		case coverSet != nil && !hit.ContainsPrefix(p):
			eff.Shadow = ShadowPartial
			eff.ShadowedAddr, eff.LiveAddr = witnesses(p, hit)
		default:
			eff.Shadow = ShadowNone
		}
		a.effects[i] = eff

		covered[fam].AddPrefix(p)
		built[fam] = true
		for _, r := range hit.rr {
			tagged[fam] = append(tagged[fam], taggedRange{r: r, index: i})
		}
	}

	// Finalize per-family state: the default-action holes and the
	// sorted winning-rule ranges.
	for fam, universe := range []netip.Prefix{v4Universe, v6Universe} {
		var cov *IPSet
		if built[fam] {
			cov, _ = covered[fam].IPSet()
		} else {
			cov = emptyIPSet
		}
		var hb IPSetBuilder
		hb.AddPrefix(universe)
		hb.RemoveSet(cov)
		a.holes[fam], _ = hb.IPSet()
		sort.Slice(tagged[fam], func(i, j int) bool {
			return tagged[fam][i].r.from.Less(tagged[fam][j].r.from)
		})
		a.rules[fam] = tagged[fam]
	}

	if len(errs) > 0 {
		return a, errs
	}
	return a, nil
}

func familyOf(addr netip.Addr) int {
	if addr.Is4() {
		return famV4
	}
	return famV6
}

// witnesses returns two addresses in p: one that live does not contain
// (an earlier rule shadows it) and one that live contains (this rule
// still decides it). It is used only when live partially covers p;
// live.rr is sorted and non-empty.
func witnesses(p netip.Prefix, live *IPSet) (shadowed, liveAddr netip.Addr) {
	liveAddr = live.rr[0].from
	start := p.Addr()
	for _, r := range live.rr {
		if start.Less(r.from) {
			return start, liveAddr // gap before this live range
		}
		start = r.to.Next()
	}
	// No leading/middle gap: the shadowed part is the trailing tail.
	return start, liveAddr
}
