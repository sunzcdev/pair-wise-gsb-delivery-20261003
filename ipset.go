// Copyright 2020 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"fmt"
	"math/bits"
	"net/netip"
	"runtime"
	"sort"
	"strings"
)

const maxSplitBalancedParts = 1 << 20

// IPSetBuilder builds an immutable IPSet.
//
// The zero value is a valid value representing a set of no IPs.
//
// The Add and Remove methods add or remove IPs to/from the set.
// Removals only affect the current membership of the set, so in
// general Adds should be called first. Input ranges may overlap in
// any way.
//
// Most IPSetBuilder methods do not return errors.
// Instead, errors are accumulated and reported by IPSetBuilder.IPSet.
type IPSetBuilder struct {
	// in are the ranges in the set.
	in []IPRange

	// out are the ranges to be removed from 'in'.
	out []IPRange

	// errs are errors accumulated during construction.
	errs multiErr
}

// normalize normalizes s: s.in becomes the minimal sorted list of
// ranges required to describe s, and s.out becomes empty.
func (s *IPSetBuilder) normalize() {
	const debug = false
	if debug {
		debugf("ranges start in=%v out=%v", s.in, s.out)
	}
	in, ok := mergeIPRanges(s.in)
	if !ok {
		return
	}
	out, ok := mergeIPRanges(s.out)
	if !ok {
		return
	}
	if debug {
		debugf("ranges sort  in=%v out=%v", in, out)
	}

	// in and out are sorted in ascending range order, and have no
	// overlaps within each other. We can run a merge of the two lists
	// in one pass.

	min := make([]IPRange, 0, len(in))
	for len(in) > 0 && len(out) > 0 {
		rin, rout := in[0], out[0]
		if debug {
			debugf("step in=%v out=%v", rin, rout)
		}

		switch {
		case !rout.IsValid() || !rin.IsValid():
			// mergeIPRanges should have prevented invalid ranges from
			// sneaking in.
			panic("invalid IPRanges during Ranges merge")
		case rout.entirelyBefore(rin):
			// "out" is entirely before "in".
			//
			//    out         in
			// f-------t   f-------t
			out = out[1:]
			if debug {
				debugf("out before in; drop out")
			}
		case rin.entirelyBefore(rout):
			// "in" is entirely before "out".
			//
			//    in         out
			// f------t   f-------t
			min = append(min, rin)
			in = in[1:]
			if debug {
				debugf("in before out; append in")
				debugf("min=%v", min)
			}
		case rin.coveredBy(rout):
			// "out" entirely covers "in".
			//
			//       out
			// f-------------t
			//    f------t
			//       in
			in = in[1:]
			if debug {
				debugf("in inside out; drop in")
			}
		case rout.inMiddleOf(rin):
			// "in" entirely covers "out".
			//
			//       in
			// f-------------t
			//    f------t
			//       out
			min = append(min, IPRange{from: rin.from, to: AddrPrior(rout.from)})
			// Adjust in[0], not ir, because we want to consider the
			// mutated range on the next iteration.
			in[0].from = rout.to.Next()
			out = out[1:]
			if debug {
				debugf("out inside in; split in, append first in, drop out, adjust second in")
				debugf("min=%v", min)
			}
		case rout.overlapsStartOf(rin):
			// "out" overlaps start of "in".
			//
			//   out
			// f------t
			//    f------t
			//       in
			in[0].from = rout.to.Next()
			// Can't move ir onto min yet, another later out might
			// trim it further. Just discard or and continue.
			out = out[1:]
			if debug {
				debugf("out cuts start of in; adjust in, drop out")
			}
		case rout.overlapsEndOf(rin):
			// "out" overlaps end of "in".
			//
			//           out
			//        f------t
			//    f------t
			//       in
			min = append(min, IPRange{from: rin.from, to: AddrPrior(rout.from)})
			in = in[1:]
			if debug {
				debugf("merge out cuts end of in; append shortened in")
				debugf("min=%v", min)
			}
		default:
			// The above should account for all combinations of in and
			// out overlapping, but insert a panic to be sure.
			panic("unexpected additional overlap scenario")
		}
	}
	if len(in) > 0 {
		// Ran out of removals before the end of in.
		min = append(min, in...)
		if debug {
			debugf("min=%v", min)
		}
	}

	s.in = min
	s.out = nil
}

// Clone returns a copy of s that shares no memory with s.
func (s *IPSetBuilder) Clone() *IPSetBuilder {
	return &IPSetBuilder{
		in:  append([]IPRange(nil), s.in...),
		out: append([]IPRange(nil), s.out...),
	}
}

func (s *IPSetBuilder) addError(msg string, args ...interface{}) {
	se := new(stacktraceErr)
	// Skip three frames: runtime.Callers, addError, and the IPSetBuilder
	// method that called addError (such as IPSetBuilder.Add).
	// The resulting stack trace ends at the line in the user's
	// code where they called into netaddr.
	n := runtime.Callers(3, se.pcs[:])
	se.at = se.pcs[:n]
	se.err = fmt.Errorf(msg, args...)
	s.errs = append(s.errs, se)
}

// Add adds ip to s.
func (s *IPSetBuilder) Add(ip netip.Addr) {
	if !ip.IsValid() {
		s.addError("Add(IP{})")
		return
	}
	s.AddRange(IPRangeFrom(ip, ip))
}

// AddPrefix adds all IPs in p to s.
func (s *IPSetBuilder) AddPrefix(p netip.Prefix) {
	if r := RangeOfPrefix(p); r.IsValid() {
		s.AddRange(r)
	} else {
		s.addError("AddPrefix(%v/%v)", p.Addr(), p.Bits())
	}
}

// AddRange adds r to s.
// If r is not Valid, AddRange does nothing.
func (s *IPSetBuilder) AddRange(r IPRange) {
	if !r.IsValid() {
		s.addError("AddRange(%v-%v)", r.From(), r.To())
		return
	}
	// If there are any removals (s.out), then we need to compact the set
	// first to get the order right.
	if len(s.out) > 0 {
		s.normalize()
	}
	s.in = append(s.in, r)
}

// AddSet adds all IPs in b to s.
func (s *IPSetBuilder) AddSet(b *IPSet) {
	if b == nil {
		return
	}
	for _, r := range b.rr {
		s.AddRange(r)
	}
}

// Remove removes ip from s.
func (s *IPSetBuilder) Remove(ip netip.Addr) {
	if !ip.IsValid() {
		s.addError("Remove(IP{})")
	} else {
		s.RemoveRange(IPRangeFrom(ip, ip))
	}
}

// RemovePrefix removes all IPs in p from s.
func (s *IPSetBuilder) RemovePrefix(p netip.Prefix) {
	if r := RangeOfPrefix(p); r.IsValid() {
		s.RemoveRange(r)
	} else {
		s.addError("RemovePrefix(%v/%v)", p.Addr(), p.Bits())
	}
}

// RemoveRange removes all IPs in r from s.
func (s *IPSetBuilder) RemoveRange(r IPRange) {
	if r.IsValid() {
		s.out = append(s.out, r)
	} else {
		s.addError("RemoveRange(%v-%v)", r.From(), r.To())
	}
}

// RemoveSet removes all IPs in o from s.
func (s *IPSetBuilder) RemoveSet(b *IPSet) {
	if b == nil {
		return
	}
	for _, r := range b.rr {
		s.RemoveRange(r)
	}
}

// removeBuilder removes all IPs in b from s.
func (s *IPSetBuilder) removeBuilder(b *IPSetBuilder) {
	b.normalize()
	for _, r := range b.in {
		s.RemoveRange(r)
	}
}

// Complement updates s to contain the complement of its current
// contents.
func (s *IPSetBuilder) Complement() {
	s.normalize()
	s.out = s.in
	s.in = []IPRange{
		RangeOfPrefix(netip.PrefixFrom(netip.AddrFrom4([4]byte{}), 0)),
		RangeOfPrefix(netip.PrefixFrom(netip.IPv6Unspecified(), 0)),
	}
}

// Intersect updates s to the set intersection of s and b.
func (s *IPSetBuilder) Intersect(b *IPSet) {
	var o IPSetBuilder
	o.Complement()
	o.RemoveSet(b)
	s.removeBuilder(&o)
}

func discardf(format string, args ...interface{}) {}

// debugf is reassigned by tests.
var debugf = discardf

// IPSet returns an immutable IPSet representing the current state of s.
//
// Most IPSetBuilder methods do not return errors.
// Rather, the builder ignores any invalid inputs (such as an invalid IPPrefix),
// and accumulates a list of any such errors that it encountered.
//
// IPSet also reports any such accumulated errors.
// Even if the returned error is non-nil, the returned IPSet is usable
// and contains all modifications made with valid inputs.
//
// The builder remains usable after calling IPSet.
// Calling IPSet clears any accumulated errors.
func (s *IPSetBuilder) IPSet() (*IPSet, error) {
	s.normalize()
	ret := &IPSet{
		rr: append([]IPRange{}, s.in...),
	}
	if len(s.errs) == 0 {
		return ret, nil
	} else {
		errs := s.errs
		s.errs = nil
		return ret, errs
	}
}

// IPSet represents a set of IP addresses.
//
// IPSet is safe for concurrent use.
// The zero value is a valid value representing a set of no IPs.
// Use IPSetBuilder to construct IPSets.
type IPSet struct {
	// rr is the set of IPs that belong to this IPSet. The IPRanges
	// are normalized according to IPSetBuilder.normalize, meaning
	// they are a sorted, minimal representation (no overlapping
	// ranges, no contiguous ranges). The implementation of various
	// methods rely on this property.
	rr []IPRange
}

// Ranges returns the minimum and sorted set of IP
// ranges that covers s.
func (s *IPSet) Ranges() []IPRange {
	return append([]IPRange{}, s.rr...)
}

// Prefixes returns the minimum and sorted set of IP prefixes
// that covers s.
func (s *IPSet) Prefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s.rr))
	for _, r := range s.rr {
		out = append(out, r.Prefixes()...)
	}
	return out
}

// Equal reports whether s and o represent the same set of IP
// addresses.
func (s *IPSet) Equal(o *IPSet) bool {
	if len(s.rr) != len(o.rr) {
		return false
	}
	for i := range s.rr {
		if s.rr[i] != o.rr[i] {
			return false
		}
	}
	return true
}

// SplitBalanced divides s into parts ordered IPSets with counts that differ
// by at most one address. The first results receive any extra addresses.
//
// Addresses retain their existing ascending order, so IPv4 addresses come
// before IPv6 addresses and one result may contain addresses from both
// families. Empty results are returned when parts exceeds the number of
// addresses. SplitBalanced panics if parts is not positive or is greater than
// 1,048,576.
func (s *IPSet) SplitBalanced(parts int) []*IPSet {
	if parts <= 0 {
		panic("SplitBalanced: parts must be positive")
	}
	if parts > maxSplitBalancedParts {
		panic("SplitBalanced: too many parts")
	}
	if s == nil {
		s = &IPSet{}
	}

	results := make([]*IPSet, parts)
	if parts == 1 {
		results[0] = &IPSet{rr: append([]IPRange{}, s.rr...)}
		return results
	}

	totalTop, totalHi, totalLo := ipSetSize192(s)
	quota, extra := div192(totalTop, totalHi, totalLo, uint64(parts))

	rangeIndex := 0
	var cursor uint128
	for shardIndex := 0; shardIndex < parts; shardIndex++ {
		remaining := quota
		if uint64(shardIndex) < extra {
			remaining = remaining.addOne()
		}

		var ranges []IPRange
		for remaining.cmp(uint128{}) > 0 && rangeIndex < len(s.rr) {
			r := s.rr[rangeIndex]
			from := u128From16(r.from.As16())
			to := u128From16(r.to.As16())
			if cursor.isZero() {
				cursor = from
			}

			end := to
			available, availableOverflow := to.sub(cursor).addOneCarry()
			if availableOverflow != 0 || available.cmp(remaining) >= 0 {
				end = cursor.add(remaining.subOne())
			}

			endAddr := end.addrInFamily(r.from)
			ranges = append(ranges, IPRangeFrom(cursor.addrInFamily(r.from), endAddr))
			remaining = remaining.sub(end.sub(cursor).addOne())

			if end == to {
				rangeIndex++
				cursor = uint128{}
			} else {
				cursor = end.addOne()
			}
		}

		results[shardIndex] = &IPSet{rr: ranges}
	}

	return results
}

func (u uint128) addrInFamily(family netip.Addr) netip.Addr {
	if family.Is4() {
		return u.IP4()
	}
	return u.IP6()
}

func ipRangeSize(r IPRange) (uint128, bool) {
	from := u128From16(r.from.As16())
	to := u128From16(r.to.As16())
	size, carry := to.sub(from).addOneCarry()
	if r.from.Is4() {
		return uint128{0, size.lo}, false
	}
	return size, carry != 0
}

func (u uint128) addOneCarry() (uint128, uint64) {
	return u.addCarry(uint128{0, 1})
}

func ipSetSize192(s *IPSet) (uint64, uint64, uint64) {
	var top uint64
	var total uint128
	for _, r := range s.rr {
		size, overflow := ipRangeSize(r)
		if overflow {
			top++
			continue
		}

		var carry uint64
		total, carry = total.addCarry(size)
		top, _ = bits.Add64(top, 0, carry)
	}
	return top, total.hi, total.lo
}

// Contains reports whether ip is in s.
// If ip has an IPv6 zone, Contains returns false,
// because IPSets do not track zones.
func (s *IPSet) Contains(ip netip.Addr) bool {
	if ip.Zone() != "" {
		return false
	}
	// TODO: data structure permitting more efficient lookups:
	// https://github.com/inetaf/netaddr/issues/139
	i := sort.Search(len(s.rr), func(i int) bool {
		return ip.Less(s.rr[i].from)
	})
	if i == 0 {
		return false
	}
	i--
	return s.rr[i].contains(ip)
}

// ContainsRange reports whether all IPs in r are in s.
func (s *IPSet) ContainsRange(r IPRange) bool {
	for _, x := range s.rr {
		if r.coveredBy(x) {
			return true
		}
	}
	return false
}

// ContainsPrefix reports whether all IPs in p are in s.
func (s *IPSet) ContainsPrefix(p netip.Prefix) bool {
	return s.ContainsRange(RangeOfPrefix(p))
}

// Overlaps reports whether any IP in b is also in s.
func (s *IPSet) Overlaps(b *IPSet) bool {
	// TODO: sorted ranges lets us do this in O(n+m)
	for _, r := range s.rr {
		for _, or := range b.rr {
			if r.Overlaps(or) {
				return true
			}
		}
	}
	return false
}

// OverlapsRange reports whether any IP in r is also in s.
func (s *IPSet) OverlapsRange(r IPRange) bool {
	// TODO: sorted ranges lets us do this more efficiently.
	for _, x := range s.rr {
		if x.Overlaps(r) {
			return true
		}
	}
	return false
}

// OverlapsPrefix reports whether any IP in p is also in s.
func (s *IPSet) OverlapsPrefix(p netip.Prefix) bool {
	return s.OverlapsRange(RangeOfPrefix(p))
}

// RemoveFreePrefix splits s into a Prefix of length bitLen and a new
// IPSet with that prefix removed.
//
// If no contiguous prefix of length bitLen exists in s,
// RemoveFreePrefix returns ok=false.
func (s *IPSet) RemoveFreePrefix(bitLen uint8) (p netip.Prefix, newSet *IPSet, ok bool) {
	var bestFit netip.Prefix
	for _, r := range s.rr {
		for _, prefix := range r.Prefixes() {
			if uint8(prefix.Bits()) > bitLen {
				continue
			}
			if !bestFit.Addr().IsValid() || prefix.Bits() > bestFit.Bits() {
				bestFit = prefix
				if uint8(bestFit.Bits()) == bitLen {
					// exact match, done.
					break
				}
			}
		}
	}

	if !bestFit.Addr().IsValid() {
		return netip.Prefix{}, s, false
	}

	prefix := netip.PrefixFrom(bestFit.Addr(), int(bitLen))

	var b IPSetBuilder
	b.AddSet(s)
	b.RemovePrefix(prefix)
	newSet, _ = b.IPSet()
	return prefix, newSet, true
}

type multiErr []error

func (e multiErr) Error() string {
	var ret []string
	for _, err := range e {
		ret = append(ret, err.Error())
	}
	return strings.Join(ret, "; ")
}

// A stacktraceErr combines an error with a stack trace.
type stacktraceErr struct {
	pcs [16]uintptr // preallocated array of PCs
	at  []uintptr   // stack trace whence the error
	err error       // underlying error
}

func (e *stacktraceErr) Error() string {
	frames := runtime.CallersFrames(e.at)
	buf := new(strings.Builder)
	buf.WriteString(e.err.Error())
	buf.WriteString(" @ ")
	for {
		frame, more := frames.Next()
		if !more {
			break
		}
		fmt.Fprintf(buf, "%s:%d ", frame.File, frame.Line)
	}
	return strings.TrimSpace(buf.String())
}

func (e *stacktraceErr) Unwrap() error {
	return e.err
}
