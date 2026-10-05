package netipx

import (
	"fmt"
	"net/netip"
	"sort"
)

// CoverageInterval is a maximal contiguous range of IP addresses that
// are all covered by the same number of input sets.
type CoverageInterval struct {
	// Range is the contiguous range of covered addresses.
	Range IPRange
	// Count is the number of input sets covering every address in
	// Range. It is always greater than zero.
	Count int
}

func (c CoverageInterval) String() string {
	return fmt.Sprintf("%v x%d", c.Range, c.Count)
}

// Coverage returns the coverage intervals of sets, as reported by
// CoverageMin with a minimum count of 1.
func Coverage(sets ...*IPSet) []CoverageInterval {
	return CoverageMin(1, sets...)
}

// CoverageMin returns the coverage intervals of sets: the maximal
// contiguous ranges of IP addresses covered by at least minCount of
// the input sets, along with the exact number of sets covering them.
//
// Each input set is one source: every address contributes at most one
// to the count per set, no matter how the set is built. Passing the
// same set pointer twice counts it as two sources. Nil sets are
// ignored.
//
// The result is sorted by address. Adjacent ranges with equal counts
// are merged, and addresses covered by fewer than minCount sets
// (including zero) are omitted; such gaps are never merged over. A
// minCount less than 1 is treated as 1.
//
// CoverageMin works on the canonical range boundaries of the input
// sets only. Its running time depends on the total number of ranges
// in the inputs, not on the number of addresses they cover.
func CoverageMin(minCount int, sets ...*IPSet) []CoverageInterval {
	if minCount < 1 {
		minCount = 1
	}
	// Turn every range into a sweep event pair: +1 at its first
	// address and -1 just past its last address. IPv4 and IPv6 events
	// are swept separately so that a range ending at the last address
	// of its family (which has no successor to place the -1 at) can
	// simply omit its end event without leaking depth into the other
	// family.
	var ev4, ev6 []coverageEvent
	for _, s := range sets {
		if s == nil {
			continue
		}
		for _, r := range s.rr {
			events := &ev4
			if !r.from.Is4() {
				events = &ev6
			}
			*events = append(*events, coverageEvent{r.from, 1})
			if next := r.to.Next(); next.IsValid() {
				*events = append(*events, coverageEvent{next, -1})
			}
		}
	}
	out := appendCoverage(nil, ev4, minCount)
	out = appendCoverage(out, ev6, minCount)
	return out
}

// coverageEvent is a change in coverage depth at the address at.
type coverageEvent struct {
	at    netip.Addr
	delta int
}

// appendCoverage sweeps events (all of one address family) in address
// order and appends the resulting coverage intervals to out.
func appendCoverage(out []CoverageInterval, events []coverageEvent, minCount int) []CoverageInterval {
	if len(events) == 0 {
		return out
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Less(events[j].at) })

	depth := 0
	start := events[0].at // first address of the open interval; meaningful when depth > 0
	close := func(to netip.Addr) {
		if depth > 0 && depth >= minCount {
			out = append(out, CoverageInterval{IPRangeFrom(start, to), depth})
		}
	}
	for i := 0; i < len(events); {
		at := events[i].at
		delta := 0
		for i < len(events) && events[i].at == at {
			delta += events[i].delta
			i++
		}
		if delta == 0 {
			// Events at a single address cancel out (e.g. one set's
			// range ends right where another's begins); the depth is
			// unchanged, so the open interval continues through.
			continue
		}
		close(at.Prev())
		depth += delta
		start = at
	}
	// Any depth still open extends to the last address of the family:
	// the ranges keeping it open had no end event.
	close(lastAddrOfFamily(start))
	return out
}

// lastAddrOfFamily returns the highest address of ip's address family.
func lastAddrOfFamily(ip netip.Addr) netip.Addr {
	if ip.Is4() {
		return netip.AddrFrom4([4]byte{0xff, 0xff, 0xff, 0xff})
	}
	return netip.AddrFrom16([16]byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})
}
