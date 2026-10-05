// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"fmt"
	"net/netip"
)

// RuleAction is the decision applied to addresses matched by a Rule.
type RuleAction int

const (
	// Deny blocks matching addresses.
	Deny RuleAction = iota
	// Allow permits matching addresses.
	Allow
)

func (a RuleAction) String() string {
	switch a {
	case Deny:
		return "deny"
	case Allow:
		return "allow"
	}
	return fmt.Sprintf("RuleAction(%d)", int(a))
}

// Rule is a single ordered firewall-style rule: addresses in Prefix
// are subject to Action, unless an earlier rule matched them first.
type Rule struct {
	Prefix netip.Prefix
	Action RuleAction
}

// ShadowKind describes how much of a rule is shadowed by earlier rules.
type ShadowKind int

const (
	// NotShadowed means no earlier rule overlaps the rule at all.
	NotShadowed ShadowKind = iota
	// PartiallyShadowed means earlier rules claim some, but not all,
	// of the rule's addresses.
	PartiallyShadowed
	// FullyShadowed means earlier rules claim every address in the
	// rule, so the rule can never match.
	FullyShadowed
)

func (k ShadowKind) String() string {
	switch k {
	case NotShadowed:
		return "not-shadowed"
	case PartiallyShadowed:
		return "partially-shadowed"
	case FullyShadowed:
		return "fully-shadowed"
	}
	return fmt.Sprintf("ShadowKind(%d)", int(k))
}

// RuleReport describes the effect of one rule under first-match
// semantics, as computed by AnalyzeRules.
type RuleReport struct {
	// Index is the rule's original position in the input slice.
	Index int
	// Rule is a copy of the analyzed rule.
	Rule Rule
	// Effective is the set of addresses for which this rule is the
	// first matching rule, and therefore the addresses it actually
	// decides. It is empty iff Shadow is FullyShadowed.
	Effective *IPSet
	// Shadow reports whether earlier rules (of any action, including
	// the same action) claim part or all of this rule's prefix.
	Shadow ShadowKind
	// ShadowedBy lists the original indices of earlier rules whose
	// prefixes overlap this rule's prefix, in ascending order.
	ShadowedBy []int
	// ShadowedWitness is an address inside the rule's prefix that is
	// claimed by an earlier rule. It is valid iff Shadow is not
	// NotShadowed.
	ShadowedWitness netip.Addr
	// EffectiveWitness is an address inside the rule's prefix that
	// this rule still decides itself. It is valid iff Shadow is not
	// FullyShadowed.
	EffectiveWitness netip.Addr
}

// RuleAnalysis is the result of analyzing an ordered rule list with
// AnalyzeRules. It is safe for concurrent use.
type RuleAnalysis struct {
	rules   []Rule
	def     RuleAction
	reports []RuleReport
	// default4 and default6 are the IPv4 and IPv6 addresses not
	// matched by any rule, which fall through to the default action.
	default4 *IPSet
	default6 *IPSet
}

// AnalyzeRules analyzes an ordered list of allow/deny prefix rules
// under first-match semantics: the first rule whose prefix contains an
// address decides it, and remaining addresses get def. It is not a
// longest-prefix match.
//
// The analysis is purely in-memory and works on whole address ranges;
// it never enumerates individual member addresses. IPv4 and IPv6 rules
// are tracked independently, so rules of one family never shadow rules
// of the other.
func AnalyzeRules(rules []Rule, def RuleAction) (*RuleAnalysis, error) {
	a := &RuleAnalysis{
		rules: append([]Rule(nil), rules...),
		def:   def,
	}
	var covered4, covered6 IPSetBuilder
	for i, r := range rules {
		if !r.Prefix.IsValid() {
			return nil, fmt.Errorf("netipx: rule %d has invalid prefix %v", i, r.Prefix)
		}
		rep := RuleReport{Index: i, Rule: r}
		covered := &covered6
		if r.Prefix.Addr().Is4() {
			covered = &covered4
		}
		prior, err := covered.IPSet()
		if err != nil {
			return nil, err
		}

		// Effective = this prefix minus everything earlier rules claim.
		var eb IPSetBuilder
		eb.AddPrefix(r.Prefix)
		eb.RemoveSet(prior)
		effective, err := eb.IPSet()
		if err != nil {
			return nil, err
		}
		rep.Effective = effective

		// Shadowed = this prefix minus what remains effective.
		var sb IPSetBuilder
		sb.AddPrefix(r.Prefix)
		sb.RemoveSet(effective)
		shadowed, err := sb.IPSet()
		if err != nil {
			return nil, err
		}

		switch {
		case len(shadowed.Ranges()) == 0:
			rep.Shadow = NotShadowed
		case len(effective.Ranges()) == 0:
			rep.Shadow = FullyShadowed
		default:
			rep.Shadow = PartiallyShadowed
		}
		if rr := shadowed.Ranges(); len(rr) > 0 {
			rep.ShadowedWitness = rr[0].From()
		}
		if rr := effective.Ranges(); len(rr) > 0 {
			rep.EffectiveWitness = rr[0].From()
		}

		for j := 0; j < i; j++ {
			earlier := rules[j]
			if earlier.Prefix.Addr().Is4() != r.Prefix.Addr().Is4() {
				continue
			}
			if earlier.Prefix.Overlaps(r.Prefix) {
				rep.ShadowedBy = append(rep.ShadowedBy, j)
			}
		}

		covered.AddPrefix(r.Prefix)
		a.reports = append(a.reports, rep)
	}

	// Whatever no rule claims falls through to the default action.
	var d4, d6 IPSetBuilder
	d4.AddPrefix(netip.PrefixFrom(netip.AddrFrom4([4]byte{}), 0))
	d6.AddPrefix(netip.PrefixFrom(netip.IPv6Unspecified(), 0))
	c4, err := covered4.IPSet()
	if err != nil {
		return nil, err
	}
	c6, err := covered6.IPSet()
	if err != nil {
		return nil, err
	}
	d4.RemoveSet(c4)
	d6.RemoveSet(c6)
	if a.default4, err = d4.IPSet(); err != nil {
		return nil, err
	}
	if a.default6, err = d6.IPSet(); err != nil {
		return nil, err
	}
	return a, nil
}

// DefaultAction returns the action applied to addresses no rule matches.
func (a *RuleAnalysis) DefaultAction() RuleAction { return a.def }

// Reports returns the per-rule analysis reports, in the original rule
// order.
func (a *RuleAnalysis) Reports() []RuleReport {
	return append([]RuleReport(nil), a.reports...)
}

// DefaultSet returns the set of addresses, of both IP families, that
// no rule matches and that therefore get the default action.
func (a *RuleAnalysis) DefaultSet() *IPSet {
	var b IPSetBuilder
	b.AddSet(a.default4)
	b.AddSet(a.default6)
	s, _ := b.IPSet()
	return s
}

// Lookup reports the action applied to ip and the original index of
// the rule that decides it. If no rule matches, Lookup returns the
// default action and -1.
func (a *RuleAnalysis) Lookup(ip netip.Addr) (RuleAction, int) {
	for i, r := range a.rules {
		if r.Prefix.Contains(ip) {
			return r.Action, i
		}
	}
	return a.def, -1
}
