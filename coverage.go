// Copyright 2023 The Inet.Af AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netipx

import (
	"math"
	"net/netip"
	"sort"
)

// A CoveredRange is an inclusive, contiguous range of IP addresses
// that are reported by the same number of source IPSets.
type CoveredRange struct {
	r     IPRange
	count int
}

// Range returns the inclusive [IPRange] covered by c.
func (c CoveredRange) Range() IPRange { return c.r }

// Count returns the number of source IPSets that cover every address
// in c. The same *IPSet appearing more than once in the Coverage input
// is counted once for each occurrence.
func (c CoveredRange) Count() int { return c.count }

// Coverage returns the minimum and sorted set of contiguous IP ranges
// that are covered by at least minSources of the input sets, together
// with the number of sets covering each range.
//
// Each *IPSet argument is one source: an IPSet's own ranges never
// overlap after construction, so addresses within one input set are
// counted once. The same *IPSet pointer may be passed multiple times;
// each occurrence is counted as a separate source. Nil sets contribute
// nothing.
//
// The returned ranges are ordered by address (IPv4 first, then IPv6).
// Adjacent ranges with equal coverage counts are merged, and address
// space with a coverage count below minSources (including count zero)
// is not emitted. In particular, a minSources filter never bridges a
// middle range whose count is too low: such a gap keeps the ranges on
// either side separate.
//
// If minSources is less than 1, it is treated as 1. If it exceeds the
// number of sources, Coverage returns nil.
//
// The algorithm is a sweep line over the canonical range boundaries:
// its work is proportional to the number of input ranges, not the
// number of addresses in them. The maximum IPv4 and IPv6 addresses
// are handled without computing an out-of-range successor.
func Coverage(minSources int, sets ...*IPSet) []CoveredRange {
	if minSources < 1 {
		minSources = 1
	}
	if minSources > len(sets) {
		return nil
	}

	// Each normalized input range [from, to] makes its source active on
	// the half-open boundary [from, to+1). Events for the two address
	// families are gathered in one pass and swept separately, which
	// keeps IPv4 results before IPv6 ones. The closing event at the
	// address after a family's maximum is never representable, so it is
	// omitted and the final segment is flushed after the sweep.
	var v4Events, v6Events []coverageEvent
	for _, set := range sets {
		if set == nil {
			continue
		}
		for _, r := range set.rr {
			if !r.IsValid() {
				continue
			}
			from := u128From16(r.from.As16())
			to := u128From16(r.to.As16())
			if r.from.Is4() {
				v4Events = appendOpenRange(v4Events, from, to, v4MaxUint128)
			} else {
				v6Events = appendOpenRange(v6Events, from, to, v6MaxUint128)
			}
		}
	}

	var out []CoveredRange
	out = appendCoverage(out, v4Events, v4MaxUint128, true, minSources)
	out = appendCoverage(out, v6Events, v6MaxUint128, false, minSources)
	return out
}

type coverageEvent struct {
	at    uint128
	delta int
}

// appendOpenRange records the +1 boundary at from and the -1 boundary
// just after to. A range ending at max has no representable closing
// boundary, so its -1 event is dropped instead of overflowing.
func appendOpenRange(events []coverageEvent, from, to, max uint128) []coverageEvent {
	events = append(events, coverageEvent{at: from, delta: +1})
	if to != max {
		events = append(events, coverageEvent{at: to.addOne(), delta: -1})
	}
	return events
}

// appendCoverage sweeps one address family's boundary events and
// appends the resulting CoveredRanges to out.
//
// Consecutive segments with equal depth are merged. Segments whose
// depth is below minSources are emitted nowhere and act as barriers,
// so qualifying ranges on either side of an insufficiently covered
// hole stay separate.
func appendCoverage(out []CoveredRange, events []coverageEvent, max uint128, is4 bool, minSources int) []CoveredRange {
	if len(events) == 0 {
		return out
	}
	sort.Slice(events, func(i, j int) bool {
		return u128Less(events[i].at, events[j].at)
	})

	var prevTo uint128
	havePrev := false

	// emit records the inclusive segment [segFrom, segTo] at depth.
	emit := func(segFrom, segTo uint128, depth int) {
		if depth < minSources {
			havePrev = false
			return
		}
		cur := IPRangeFrom(segFrom.addr(is4), segTo.addr(is4))
		if havePrev && out[len(out)-1].count == depth && prevTo.addOne() == segFrom {
			out[len(out)-1].r.to = cur.to
		} else {
			out = append(out, CoveredRange{r: cur, count: depth})
		}
		prevTo = segTo
		havePrev = true
	}

	segFrom := events[0].at
	depth := 0
	for i := 0; i < len(events); {
		at := events[i].at
		// Close [segFrom, at) with the depth in effect before this
		// boundary takes effect.
		if at != segFrom {
			emit(segFrom, at.subOne(), depth)
		}
		j := i
		delta := 0
		for j < len(events) && events[j].at == at {
			delta += events[j].delta
			j++
		}
		depth += delta
		segFrom = at
		i = j
	}
	// Flush the tail: ranges ending at max have no closing event, so a
	// positive depth extends to the family's final address.
	if depth > 0 {
		emit(segFrom, max, depth)
	}
	return out
}

var (
	maxIPv4 = netip.AddrFrom4([4]byte{math.MaxUint8, math.MaxUint8, math.MaxUint8, math.MaxUint8})
	maxIPv6 = netip.AddrFrom16([16]byte{
		math.MaxUint8, math.MaxUint8, math.MaxUint8, math.MaxUint8,
		math.MaxUint8, math.MaxUint8, math.MaxUint8, math.MaxUint8,
		math.MaxUint8, math.MaxUint8, math.MaxUint8, math.MaxUint8,
		math.MaxUint8, math.MaxUint8, math.MaxUint8, math.MaxUint8,
	})

	v4MaxUint128 = u128From16(maxIPv4.As16())
	v6MaxUint128 = u128From16(maxIPv6.As16())
)

// addr reconstructs the address of the given family from u.
// IPv4 addresses occupy the mapped-in range ::ffff:0:0/96.
func (u uint128) addr(is4 bool) netip.Addr {
	if is4 {
		return u.IP4()
	}
	return u.IP6()
}

func u128Less(a, b uint128) bool {
	if a.hi != b.hi {
		return a.hi < b.hi
	}
	return a.lo < b.lo
}
