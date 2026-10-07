package decimal

import (
	"math/big"
	"sort"
)

// opBudget is an abstract counter of exact-arithmetic operations.
type opBudget struct {
	limit int64
	used  int64
}

// charge consumes n operations and reports whether the budget still holds.
func (b *opBudget) charge(n int64) bool {
	b.used += n
	return b.limit <= 0 || b.used <= b.limit
}

// rootIsolator accumulates isolated roots and pending intervals for one run.
type rootIsolator struct {
	width   *big.Rat
	budget  *opBudget
	lo, hi  *big.Rat
	cands   []rootCandidate
	pending []PendingInterval
	failed  bool
}

// rootCandidate is an isolating interval still carrying the Sturm
// sequence of its square-free factor, needed for the separation pass.
type rootCandidate struct {
	lo, hi *big.Rat
	mult   int
	seq    []ratPoly
}

func (c rootCandidate) isPoint() bool { return c.lo.Cmp(c.hi) == 0 }

func (c rootCandidate) width() *big.Rat { return new(big.Rat).Sub(c.hi, c.lo) }

// fail records the whole search interval as unsearched.
func (iso *rootIsolator) fail() {
	iso.failed = true
	iso.pending = append(iso.pending, PendingInterval{cloneRat(iso.lo), cloneRat(iso.hi)})
}

// failQueue records one unsearched open interval plus everything still
// queued, and marks the run incomplete.
func (iso *rootIsolator) failQueue(current PendingInterval, queue []PendingInterval) {
	iso.failed = true
	iso.pending = append(iso.pending, current)
	iso.pending = append(iso.pending, clonePending(queue)...)
}

// isolateFactor isolates the roots of one square-free factor f of the
// polynomial; every root of f is a root of the polynomial with the given
// multiplicity. It reports false when the budget ran out.
func (iso *rootIsolator) isolateFactor(f ratPoly, mult int) bool {
	seq, ok := sturmSequence(f, iso.budget)
	if !ok {
		iso.fail()
		return false
	}
	b := iso.budget

	// Roots exactly at the endpoints of the search interval.
	if !b.charge(1) {
		iso.fail()
		return false
	}
	if ratPolyEval(f, iso.lo).Sign() == 0 {
		iso.cands = append(iso.cands, rootCandidate{cloneRat(iso.lo), cloneRat(iso.lo), mult, seq})
	}
	if iso.hi.Cmp(iso.lo) != 0 {
		if !b.charge(1) {
			iso.fail()
			return false
		}
		if ratPolyEval(f, iso.hi).Sign() == 0 {
			iso.cands = append(iso.cands, rootCandidate{cloneRat(iso.hi), cloneRat(iso.hi), mult, seq})
		}
	}
	if iso.lo.Cmp(iso.hi) == 0 {
		return true
	}

	// Bisect the open interval (lo, hi) with exact Sturm root counts.
	queue := []PendingInterval{{iso.lo, iso.hi}}
	for len(queue) > 0 {
		iv := queue[len(queue)-1]
		queue = queue[:len(queue)-1]

		n, ok := sturmCountOpen(seq, iv.Lower, iv.Upper, b)
		if !ok {
			iso.failQueue(PendingInterval{cloneRat(iv.Lower), cloneRat(iv.Upper)}, queue)
			return false
		}
		if n == 0 {
			continue
		}
		if n == 1 && new(big.Rat).Sub(iv.Upper, iv.Lower).Cmp(iso.width) <= 0 {
			iso.cands = append(iso.cands, rootCandidate{iv.Lower, iv.Upper, mult, seq})
			continue
		}
		mid := midpoint(iv.Lower, iv.Upper)
		if !b.charge(1) {
			iso.failQueue(PendingInterval{cloneRat(iv.Lower), cloneRat(iv.Upper)}, queue)
			return false
		}
		if ratPolyEval(f, mid).Sign() == 0 {
			iso.cands = append(iso.cands, rootCandidate{mid, mid, mult, seq})
		}
		queue = append(queue, PendingInterval{iv.Lower, mid}, PendingInterval{mid, iv.Upper})
	}
	return true
}

// finishRoots separates overlapping candidates (possible only across
// distinct square-free factors) and returns the final sorted root list.
func (iso *rootIsolator) finishRoots() []RootInterval {
	cands := iso.cands
	for {
		sort.Slice(cands, func(i, j int) bool { return cands[i].lo.Cmp(cands[j].lo) < 0 })
		overlap := -1
		for i := 0; i+1 < len(cands); i++ {
			if cands[i].hi.Cmp(cands[i+1].lo) >= 0 {
				overlap = i
				break
			}
		}
		if overlap < 0 {
			break
		}
		a, b := cands[overlap], cands[overlap+1]
		// Refine the wider candidate; a point candidate cannot be refined.
		target := overlap
		if a.isPoint() || b.width().Cmp(a.width()) > 0 {
			target = overlap + 1
		}
		refined, ok := refineCandidate(cands[target], iso.budget)
		if !ok {
			// Budget exhausted during separation: the two conflicting
			// candidates are no longer certified disjoint, so demote
			// them to pending instead of reporting overlapping roots.
			iso.failed = true
			iso.pending = append(iso.pending,
				PendingInterval{cloneRat(a.lo), cloneRat(a.hi)},
				PendingInterval{cloneRat(b.lo), cloneRat(b.hi)})
			cands = append(cands[:overlap], cands[overlap+2:]...)
			continue
		}
		cands[target] = refined
	}

	roots := make([]RootInterval, len(cands))
	for i, c := range cands {
		roots[i] = RootInterval{Lower: c.lo, Upper: c.hi, Multiplicity: c.mult}
	}
	return roots
}

// refineCandidate halves the isolating interval of a candidate that holds
// exactly one root, keeping the half that contains it.
func refineCandidate(c rootCandidate, budget *opBudget) (rootCandidate, bool) {
	mid := midpoint(c.lo, c.hi)
	if !budget.charge(1) {
		return rootCandidate{}, false
	}
	if ratPolyEval(c.seq[0], mid).Sign() == 0 {
		return rootCandidate{mid, mid, c.mult, c.seq}, true
	}
	n, ok := sturmCountOpen(c.seq, c.lo, mid, budget)
	if !ok {
		return rootCandidate{}, false
	}
	if n == 1 {
		return rootCandidate{c.lo, mid, c.mult, c.seq}, true
	}
	return rootCandidate{mid, c.hi, c.mult, c.seq}, true
}

func cloneRat(x *big.Rat) *big.Rat {
	if x == nil {
		return nil
	}
	return new(big.Rat).Set(x)
}

func clonePending(ivs []PendingInterval) []PendingInterval {
	out := make([]PendingInterval, len(ivs))
	for i, iv := range ivs {
		out[i] = PendingInterval{cloneRat(iv.Lower), cloneRat(iv.Upper)}
	}
	return out
}

func midpoint(a, b *big.Rat) *big.Rat {
	return new(big.Rat).Quo(new(big.Rat).Add(a, b), big.NewRat(2, 1))
}

func mergePending(ivs []PendingInterval) []PendingInterval {
	if len(ivs) == 0 {
		return nil
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].Lower.Cmp(ivs[j].Lower) < 0 })
	out := []PendingInterval{ivs[0]}
	for _, iv := range ivs[1:] {
		last := &out[len(out)-1]
		if iv.Lower.Cmp(last.Upper) <= 0 {
			if iv.Upper.Cmp(last.Upper) > 0 {
				last.Upper = iv.Upper
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}
