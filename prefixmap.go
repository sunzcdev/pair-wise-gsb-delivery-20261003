// Copyright 2026 The Go4 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"fmt"
	"math/big"
	"net/netip"
	"runtime"
	"sort"
)

// OnMissPolicy selects what PrefixMap does with input addresses that
// are not covered by any source prefix.
type OnMissPolicy int

const (
	// DropOnMiss drops input addresses that no rule matches.
	DropOnMiss OnMissPolicy = iota

	// KeepOnMiss leaves input addresses unmatched by any rule
	// unchanged in the output set.
	KeepOnMiss
)

// PrefixMapBuilder builds a batch prefix mapping for IP addresses.
//
// A mapping is a list of source→destination prefix rules, such as
// 10.0.0.0/8 → 2001:db8:1::/104. Each address in an input IPSet is
// classified against the source prefixes: the most specific
// (longest) matching prefix wins, and the address is rewritten into
// the destination prefix while preserving its host offset. Two
// source prefixes of the same length must never overlap; adding such
// a conflicting rule accumulates an error reported by Map.
//
// The two ends of a rule must have the same number of host bits, so
// an IPv4 /24 may map to an IPv6 /120, but an IPv4 /24 cannot map to
// an IPv6 /112.
//
// All rules apply to the original input simultaneously: an address
// produced as output of one rule is never fed back into another
// source prefix. Overlapping destinations are merged with set
// semantics, so output addresses are never duplicated.
//
// The zero value is a valid builder that drops unmatched addresses.
type PrefixMapBuilder struct {
	rules []prefixMapRule

	onMiss OnMissPolicy

	errs multiErr
}

type prefixMapRule struct {
	src netip.Prefix
	dst netip.Prefix

	// idx is the rule's insertion order, used to report
	// per-rule statistics in the order rules were added.
	idx int
}

// AddRule adds a rule rewriting src into dst.
//
// src and dst must be valid prefixes whose ends have the same number
// of host bits. Two rules whose source prefixes have the same length
// and overlap are conflicting and produce an error in Map. Source
// prefixes of different lengths may nest freely, with the most
// specific prefix winning.
//
// Host bits in src and dst are masked away.
func (b *PrefixMapBuilder) AddRule(src, dst netip.Prefix) {
	if !src.IsValid() {
		b.addError("AddRule: invalid source prefix %v", src)
		return
	}
	if !dst.IsValid() {
		b.addError("AddRule: invalid destination prefix %v", dst)
		return
	}
	src = src.Masked()
	dst = dst.Masked()
	srcHostBits := uint8(src.Addr().BitLen() - src.Bits())
	dstHostBits := uint8(dst.Addr().BitLen() - dst.Bits())
	if srcHostBits != dstHostBits {
		b.addError(
			"AddRule: source %v (%d host bits) and destination %v (%d host bits) must have the same number of host bits",
			src, srcHostBits, dst, dstHostBits,
		)
		return
	}
	for _, ex := range b.rules {
		// Prefixes of the same family and prefix length are
		// either identical or disjoint, so the only same-length
		// conflict is a duplicate source prefix.
		if ex.src.Addr().BitLen() == src.Addr().BitLen() && ex.src == src {
			b.addError("AddRule: duplicate source prefix %v", src)
			return
		}
	}
	b.rules = append(b.rules, prefixMapRule{
		src: src,
		dst: dst,
		idx: len(b.rules),
	})
}

// SetOnMiss sets what Map does with input addresses not covered by
// any source prefix. The default is DropOnMiss.
func (b *PrefixMapBuilder) SetOnMiss(policy OnMissPolicy) {
	switch policy {
	case DropOnMiss, KeepOnMiss:
		b.onMiss = policy
	default:
		b.addError("SetOnMiss: invalid policy %d", policy)
	}
}

func (b *PrefixMapBuilder) addError(msg string, args ...interface{}) {
	se := new(stacktraceErr)
	// Skip three frames: runtime.Callers, addError, and the
	// PrefixMapBuilder method that called addError.
	n := runtime.Callers(3, se.pcs[:])
	se.at = se.pcs[:n]
	se.err = fmt.Errorf(msg, args...)
	b.errs = append(b.errs, se)
}

// PrefixMapResult is the result of PrefixMapBuilder.Map.
type PrefixMapResult struct {
	// out is the mapped IPSet. When unmatched addresses are kept,
	// it also contains the unchanged input addresses.
	out *IPSet

	// mapped is the number of input addresses rewritten by each
	// rule, in the order the rules were added.
	mapped []big.Int

	// unmatched is the number of input addresses not covered by
	// any source prefix.
	unmatched big.Int
}

// Out returns the resulting IPSet.
func (r *PrefixMapResult) Out() *IPSet { return r.out }

// Mapped returns the number of input addresses rewritten by each
// rule, in the order the rules were added. The returned slice must
// not be modified.
func (r *PrefixMapResult) Mapped() []big.Int { return r.mapped }

// Unmatched returns the number of input addresses not covered by any
// source prefix.
func (r *PrefixMapResult) Unmatched() big.Int { return r.unmatched }

// Map applies the configured rules to the addresses in in.
//
// Every address in in is classified exactly once, against the
// original input: most specific source prefix wins, and the
// resulting destination addresses are never re-examined by another
// rule. Overlapping destinations are merged. The input set is not
// modified.
//
// Any errors accumulated while configuring the builder (such as
// conflicting source prefixes) are reported here, and the result is
// nil. Calling Map clears accumulated errors.
func (b *PrefixMapBuilder) Map(in *IPSet) (*PrefixMapResult, error) {
	if len(b.errs) > 0 {
		errs := b.errs
		b.errs = nil
		return nil, errs
	}

	// remaining is the portion of the input not yet claimed by a
	// more specific rule. It stays sorted and non-overlapping as
	// rules are applied.
	var remaining []IPRange
	if in != nil {
		remaining = in.Ranges()
	}

	// rules are applied most specific first, so that a
	// longer-prefix rule claims its addresses before any parent
	// rule. Within a prefix length, source prefixes are disjoint
	// and the order does not matter; the insertion index keeps
	// the sort deterministic.
	rules := append([]prefixMapRule(nil), b.rules...)
	sort.SliceStable(rules, func(i, j int) bool {
		ri, rj := rules[i].src.Addr(), rules[j].src.Addr()
		if ri.BitLen() != rj.BitLen() {
			// The address families cannot overlap; keep the
			// deterministic IPv4-before-IPv6 order.
			return ri.BitLen() < rj.BitLen()
		}
		if rules[i].src.Bits() != rules[j].src.Bits() {
			return rules[i].src.Bits() > rules[j].src.Bits()
		}
		return rules[i].idx < rules[j].idx
	})

	mapped := make([]big.Int, len(rules))
	var mappedRanges []IPRange

	for _, rule := range rules {
		srcRange := RangeOfPrefix(rule.src)
		srcBase := u128From16(rule.src.Addr().As16())
		dstBase := u128From16(rule.dst.Addr().As16())

		var nextRemaining []IPRange
		for _, rem := range remaining {
			hit, rest := intersectRange(rem, srcRange)
			if hit.IsValid() {
				mapped[rule.idx].Add(&mapped[rule.idx], rangeSize(hit))
				mappedRanges = append(mappedRanges, IPRange{
					from: mapPrefixAddr(hit.from, srcBase, dstBase, rule.dst.Addr().Is4()),
					to:   mapPrefixAddr(hit.to, srcBase, dstBase, rule.dst.Addr().Is4()),
				})
			}
			nextRemaining = append(nextRemaining, rest...)
		}
		remaining = nextRemaining
	}

	var unmatched big.Int
	if b.onMiss == KeepOnMiss {
		mappedRanges = append(mappedRanges, remaining...)
	}
	for _, rem := range remaining {
		unmatched.Add(&unmatched, rangeSize(rem))
	}

	out, ok := mergeIPRanges(mappedRanges)
	if !ok {
		panic("netipx: invalid range produced by PrefixMap")
	}

	return &PrefixMapResult{
		out:       &IPSet{rr: out},
		mapped:    mapped,
		unmatched: unmatched,
	}, nil
}

// intersectRange splits the input range r against the rule range
// cut. hit is the portion of r covered by cut (the zero IPRange if
// they do not overlap), and rest is the sorted list of pieces of r
// not covered by cut: zero, one, or two ranges.
func intersectRange(r, cut IPRange) (hit IPRange, rest []IPRange) {
	if !r.IsValid() || !cut.IsValid() ||
		r.from.BitLen() != cut.from.BitLen() ||
		r.entirelyBefore(cut) || cut.entirelyBefore(r) {
		return IPRange{}, []IPRange{r}
	}
	// Clip both bounds of r to cut.
	lo := r.from
	if lo.Less(cut.from) {
		lo = cut.from
	}
	hi := r.to
	if cut.to.Less(hi) {
		hi = cut.to
	}
	hit = IPRangeFrom(lo, hi)
	if r.from.Less(hit.from) {
		rest = append(rest, IPRangeFrom(r.from, AddrPrior(hit.from)))
	}
	if hit.to.Less(r.to) {
		rest = append(rest, IPRangeFrom(hit.to.Next(), r.to))
	}
	return hit, rest
}

// mapPrefixAddr rewrites addr, which lies in a source prefix whose
// network number is srcBase, into the destination prefix with
// network number dstBase, preserving the host offset. The source and
// destination prefixes have equal host-bit widths, so the host
// portion can be transferred without shifting.
func mapPrefixAddr(addr netip.Addr, srcBase, dstBase uint128, dstIs4 bool) netip.Addr {
	a := u128From16(addr.As16())
	offset := a.sub(srcBase)
	out := dstBase.add(offset)
	if dstIs4 {
		return out.IP4()
	}
	return out.IP6()
}

// rangeSize returns the number of addresses in the inclusive range r
// (to-from+1) as a big.Int.
func rangeSize(r IPRange) *big.Int {
	from := u128From16(r.from.As16())
	to := u128From16(r.to.As16())
	width := to.sub(from)
	n := new(big.Int).SetUint64(width.hi)
	n.Lsh(n, 64)
	n.Or(n, new(big.Int).SetUint64(width.lo))
	return n.Add(n, big.NewInt(1))
}
