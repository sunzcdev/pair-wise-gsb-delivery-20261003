// Copyright 2026 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"fmt"
	"math/big"
	"net/netip"
	"sort"
)

// PrefixMapping is a single rule for IPSet.MapPrefixes, mapping the
// source prefix Src onto the destination prefix Dst while preserving
// each address's host offset within the prefix.
//
// Src and Dst must have the same number of host bits, so an IPv4 /24
// may map onto an IPv6 /120 (both have 8 host bits).
type PrefixMapping struct {
	Src netip.Prefix
	Dst netip.Prefix
}

// PrefixMapResult is the result of IPSet.MapPrefixes.
type PrefixMapResult struct {
	// Set is the resulting IP set: every matched input address
	// replaced by its mapped destination address, plus the unmatched
	// input addresses if they were kept. Overlapping mapped
	// destinations are merged according to set semantics.
	Set *IPSet

	// Migrated[i] is the exact number of input addresses migrated by
	// the i-th rule passed to MapPrefixes.
	Migrated []*big.Int

	// Unmatched is the exact number of input addresses not matched by
	// any rule.
	Unmatched *big.Int
}

// MapPrefixes applies the given batch of prefix mapping rules to s and
// returns the resulting set along with exact per-rule migration counts.
//
// Each rule maps the addresses of its source prefix onto its
// destination prefix, preserving host offsets; the two prefixes of a
// rule must have the same number of host bits. Where source prefixes
// overlap, the most specific (longest) prefix wins; rules with
// identical source prefixes are rejected as conflicting.
//
// All rules are applied to the original input simultaneously: mapped
// output addresses are never re-matched against another rule's source,
// even if a destination falls inside one. Overlapping destinations are
// merged in the output set.
//
// Input addresses matched by no rule are kept in the output if
// keepUnmatched is true, and dropped otherwise; either way they are
// reported in the result's Unmatched count.
//
// The mapping is computed with interval slicing and bit arithmetic
// only; no addresses are enumerated. s is not modified.
func (s *IPSet) MapPrefixes(rules []PrefixMapping, keepUnmatched bool) (*PrefixMapResult, error) {
	type mapRule struct {
		idx      int // position in the caller's rules slice
		src      netip.Prefix
		srcRange IPRange
		srcBase  uint128
		dstBase  uint128
		dstIs4   bool
	}
	rs := make([]mapRule, len(rules))
	seen := make(map[netip.Prefix]bool, len(rules))
	for i, m := range rules {
		src, dst := m.Src.Masked(), m.Dst.Masked()
		if !src.IsValid() || !dst.IsValid() {
			return nil, fmt.Errorf("netipx: mapping rule %d has an invalid prefix", i)
		}
		if src.Addr().Zone() != "" || dst.Addr().Zone() != "" {
			return nil, fmt.Errorf("netipx: mapping rule %d has a zone", i)
		}
		srcHost := src.Addr().BitLen() - src.Bits()
		dstHost := dst.Addr().BitLen() - dst.Bits()
		if srcHost != dstHost {
			return nil, fmt.Errorf("netipx: mapping rule %d: source %v and destination %v have different host bits (%d vs %d)", i, src, dst, srcHost, dstHost)
		}
		if seen[src] {
			return nil, fmt.Errorf("netipx: mapping rule %d: conflicting rules for source prefix %v", i, src)
		}
		seen[src] = true
		rs[i] = mapRule{
			idx:      i,
			src:      src,
			srcRange: RangeOfPrefix(src),
			srcBase:  u128From16(src.Addr().As16()),
			dstBase:  u128From16(dst.Addr().As16()),
			dstIs4:   dst.Addr().Is4(),
		}
	}
	// Most specific source prefix first, so that when a rule claims a
	// slice of the input, less specific rules can no longer see it.
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].src.Bits() > rs[j].src.Bits() })

	var remaining []IPRange
	if s != nil {
		remaining = append(remaining, s.rr...)
	}
	migrated := make([]*big.Int, len(rules))
	for i := range migrated {
		migrated[i] = new(big.Int)
	}
	var out IPSetBuilder
	for _, r := range rs {
		var claimed []IPRange
		remaining, claimed = sliceIPRanges(remaining, r.srcRange)
		for _, c := range claimed {
			migrated[r.idx].Add(migrated[r.idx], rangeAddrCount(c))
			out.AddRange(mapIPRange(c, r.srcBase, r.dstBase, r.dstIs4))
		}
	}
	unmatched := new(big.Int)
	for _, r := range remaining {
		unmatched.Add(unmatched, rangeAddrCount(r))
		if keepUnmatched {
			out.AddRange(r)
		}
	}
	set, err := out.IPSet()
	if err != nil {
		// All ranges added above are valid by construction.
		return nil, err
	}
	return &PrefixMapResult{
		Set:       set,
		Migrated:  migrated,
		Unmatched: unmatched,
	}, nil
}

// sliceIPRanges splits the sorted, non-overlapping ranges in into the
// parts covered by cut and the parts outside it. Both results remain
// sorted and non-overlapping.
func sliceIPRanges(in []IPRange, cut IPRange) (rest, claimed []IPRange) {
	for _, r := range in {
		if r.entirelyBefore(cut) || cut.entirelyBefore(r) {
			rest = append(rest, r)
			continue
		}
		from, to := r.from, r.to
		if cut.from.Compare(from) > 0 {
			from = cut.from
		}
		if cut.to.Compare(to) < 0 {
			to = cut.to
		}
		claimed = append(claimed, IPRangeFrom(from, to))
		if r.from.Less(from) {
			rest = append(rest, IPRangeFrom(r.from, AddrPrior(from)))
		}
		if to.Less(r.to) {
			rest = append(rest, IPRangeFrom(AddrNext(to), r.to))
		}
	}
	return rest, claimed
}

// mapIPRange maps r, which must lie within the source prefix whose
// base address is srcBase, onto the destination prefix whose base
// address is dstBase, preserving host offsets.
func mapIPRange(r IPRange, srcBase, dstBase uint128, dstIs4 bool) IPRange {
	from := u128From16(r.from.As16())
	to := u128From16(r.to.As16())
	newFrom := dstBase.add(from.sub(srcBase))
	newTo := newFrom.add(to.sub(from))
	if dstIs4 {
		return IPRangeFrom(newFrom.IP4(), newTo.IP4())
	}
	return IPRangeFrom(newFrom.IP6(), newTo.IP6())
}

// rangeAddrCount returns the exact number of addresses in r.
func rangeAddrCount(r IPRange) *big.Int {
	from := u128From16(r.from.As16())
	to := u128From16(r.to.As16())
	diff := to.sub(from)
	n := new(big.Int).SetUint64(diff.hi)
	n.Lsh(n, 64)
	n.Add(n, new(big.Int).SetUint64(diff.lo))
	return n.Add(n, big.NewInt(1))
}
