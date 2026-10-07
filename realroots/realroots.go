package realroots

import (
	"errors"
	"math/big"
	"sort"

	"github.com/shopspring/decimal"
)

// Endpoint labels which boundary of the requested interval an exact root
// lies on. Interior isolated roots use EndpointNone.
type Endpoint int

const (
	EndpointNone Endpoint = iota
	EndpointLow
	EndpointHigh
)

// RootInterval is a certified isolating interval for one distinct real root.
//
// The root is guaranteed to lie in [Low, High], Low <= High, and no other
// distinct root lies there. When Low == High the root is known exactly (this
// happens for rational roots hit during subdivision and for interval
// endpoints); otherwise Width satisfies the requested target width.
// Multiplicity is the multiplicity of the root in the original polynomial.
// Isolating intervals for distinct roots never overlap.
type RootInterval struct {
	Low          decimal.Decimal
	High         decimal.Decimal
	Multiplicity int
	Endpoint     Endpoint
}

// Width returns High-Low exactly.
func (r RootInterval) Width() decimal.Decimal { return r.High.Sub(r.Low) }

// PendingInterval is a region the process could not certify. It contains no
// reported root, but the library does not claim it is root free because it
// was not fully searched. MinMultiplicity is a certified lower bound on the
// total multiplicity known to remain inside it.
type PendingInterval struct {
	Low             decimal.Decimal
	High            decimal.Decimal
	MinMultiplicity int
	Reason          Reason
}

// Options configures an isolation request.
//
// Low/High are the search interval, interpreted as half open [Low, High)
// unless IncludeHigh is set, in which case it is closed [Low, High].
//
// TargetWidth bounds High-Low of every non-exact isolating interval. When
// zero, one millionth of the interval width is used.
type Options struct {
	Low         decimal.Decimal
	High        decimal.Decimal
	IncludeHigh bool
	TargetWidth decimal.Decimal
	Budget      Budget
}

// Result is the outcome of an isolation request.
//
// Roots contains all fully certified roots. Pending contains maximal
// regions not searched to completion (each tagged with a Reason); the two
// collections never overlap.
type Result struct {
	// Zero is true when the input polynomial is the zero polynomial, for
	// which "all roots" is every real number and a finite interval list is
	// impossible.
	Zero bool

	// Constant is true for a non-zero constant polynomial, which has no
	// roots anywhere.
	Constant bool

	Roots   []RootInterval
	Pending []PendingInterval

	// Complete is true iff Pending is empty. Every root in the requested
	// interval is then present in Roots.
	Complete bool
}

// Isolate finds every real root of p in the requested interval.
//
// p holds decimal coefficients in ascending order of degree (p[i] is the
// coefficient of x^i). All computation is exact; coefficients are never
// converted through float64.
func Isolate(p []decimal.Decimal, opts Options) (*Result, error) {
	lo, err := decimalToRat(opts.Low)
	if err != nil {
		return nil, err
	}
	hi, err := decimalToRat(opts.High)
	if err != nil {
		return nil, err
	}
	if lo.Cmp(hi) >= 0 {
		return nil, errors.New("realroots: require Low < High")
	}

	rp, err := polyFromDecimals(p)
	if err != nil {
		return nil, err
	}

	res := &Result{Roots: []RootInterval{}}
	deg := rp.degree()
	switch {
	case deg < 0:
		res.Zero = true
		res.Complete = true
		return res, nil
	case deg == 0:
		res.Constant = true
		res.Complete = true
		return res, nil
	}

	budget := newBudget(opts.Budget)
	target, err := resolveTargetWidth(opts.TargetWidth, lo, hi)
	if err != nil {
		return nil, err
	}
	if deg > budget.degreeLimit {
		res.Pending = append(res.Pending, PendingInterval{
			Low:    opts.Low,
			High:   opts.High,
			Reason: ReasonMaxDegree,
		})
		return res, nil
	}

	factors := squareFreeYun(rp)

	states := make([]factorState, len(factors))
	for i, f := range factors {
		states[i].mult = f.multiplicity
		g := f.factor
		// Remove exact roots at the boundaries so the Sturm engine only has
		// to reason about the open interior (lo, hi).
		if g.eval(lo).Sign() == 0 {
			res.Roots = append(res.Roots, RootInterval{
				Low:          ratToDecimal(lo),
				High:         ratToDecimal(lo),
				Multiplicity: f.multiplicity,
				Endpoint:     EndpointLow,
			})
			g = g.divideByLinear(lo)
		}
		// Sturm's V(lo)-V(hi) counts roots in (lo,hi]; always strip a root
		// at hi so the interior engine counts the open interval (lo,hi), and
		// report it only when the caller asked for the closed interval.
		if g.eval(hi).Sign() == 0 {
			if opts.IncludeHigh {
				res.Roots = append(res.Roots, RootInterval{
					Low:          ratToDecimal(hi),
					High:         ratToDecimal(hi),
					Multiplicity: f.multiplicity,
					Endpoint:     EndpointHigh,
				})
			}
			g = g.divideByLinear(hi)
		}
		states[i].poly = g
		if g.degree() > 0 {
			states[i].sturm = newSturm(g, budget)
		}
	}

	root := &node{lo: lo, hi: hi, counts: make([]int, len(factors))}
	for i, st := range states {
		if st.sturm != nil {
			root.counts[i] = st.sturm.rootsOpen(lo, hi)
			root.total += root.counts[i]
		}
	}

	var pendings []*node
	var queue []*node
	if budget.exhausted != "" {
		// Setup itself (Sturm construction / initial counts) hit the
		// operation budget; counts may be missing, so nothing is certified.
		pendings = append(pendings, root)
	} else {
		queue = []*node{root}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]

		if n.total == 0 {
			continue // certified empty
		}
		width := new(big.Rat).Sub(n.hi, n.lo)
		if n.total == 1 && width.Cmp(target) <= 0 {
			res.Roots = emitRoot(res.Roots, n, states)
			continue
		}
		if !budget.chargeBisection() {
			pendings = append(pendings, n)
			continue
		}

		mid := new(big.Rat).Add(n.lo, n.hi)
		mid.Quo(mid, big.NewRat(2, 1))

		left := &node{lo: n.lo, hi: mid, counts: make([]int, len(states))}
		right := &node{lo: mid, hi: n.hi, counts: make([]int, len(states))}
		for i, st := range states {
			if st.sturm == nil || n.counts[i] == 0 {
				continue
			}
			at := st.sturm.rootsAtRight(mid)
			if at == 1 {
				// Exact rational root at the split point; remove it from the
				// factor before further counting and record it with
				// multiplicity as a zero-width interval.
				res.Roots = append(res.Roots, RootInterval{
					Low:          ratToDecimal(mid),
					High:         ratToDecimal(mid),
					Multiplicity: st.mult,
					Endpoint:     EndpointNone,
				})
				g := st.poly.divideByLinear(mid)
				st.poly = g
				st.sturm = newSturm(g, budget)
				// Rebuild counts for this factor in both children; the
				// already-separated midpoint root is excluded.
				left.counts[i] = st.sturm.rootsOpen(left.lo, left.hi)
				right.counts[i] = st.sturm.rootsOpen(right.lo, right.hi)
				states[i] = st
				n.counts[i]--
				continue
			}
			left.counts[i] = st.sturm.rootsOpen(left.lo, left.hi)
			right.counts[i] = n.counts[i] - left.counts[i]
		}
		for i := range states {
			left.total += left.counts[i]
			right.total += right.counts[i]
		}
		queue = append(queue, left, right)
	}

	if len(pendings) > 0 {
		reason := budget.exhausted
		sort.Slice(pendings, func(i, j int) bool {
			return pendings[i].lo.Cmp(pendings[j].lo) < 0
		})
		curLo, curHi := pendings[0].lo, pendings[0].hi
		curMin := pendingMinMult(pendings[0], states)
		flush := func() {
			res.Pending = append(res.Pending, PendingInterval{
				Low:             ratToDecimal(curLo),
				High:            ratToDecimal(curHi),
				MinMultiplicity: curMin,
				Reason:          reason,
			})
		}
		for _, n := range pendings[1:] {
			if n.lo.Cmp(curHi) == 0 {
				curHi = n.hi
				if m := pendingMinMult(n, states); m < curMin {
					curMin = m
				}
			} else {
				flush()
				curLo, curHi, curMin = n.lo, n.hi, pendingMinMult(n, states)
			}
		}
		flush()
	}

	sort.Slice(res.Roots, func(i, j int) bool {
		return res.Roots[i].Low.Cmp(res.Roots[j].Low) < 0
	})
	res.Complete = len(res.Pending) == 0
	return res, nil
}

// factorState tracks one square-free factor during subdivision.
type factorState struct {
	mult  int
	sturm *sturm
	poly  ratPoly // in-interior polynomial (sturm.seq[0])
}

// node is one dyadic cell of the shared subdivision tree.
type node struct {
	lo, hi *big.Rat
	counts []int // distinct roots per factor in (lo, hi)
	total  int
}

// emitRoot finalizes a leaf containing exactly one distinct root.
func emitRoot(out []RootInterval, n *node, states []factorState) []RootInterval {
	mult := 0
	for i, c := range n.counts {
		if c > 0 {
			mult += states[i].mult
		}
	}
	return append(out, RootInterval{
		Low:          ratToDecimal(n.lo),
		High:         ratToDecimal(n.hi),
		Multiplicity: mult,
		Endpoint:     EndpointNone,
	})
}

func pendingMinMult(n *node, states []factorState) int {
	m := 0
	for i, c := range n.counts {
		if c > 0 {
			m += states[i].mult
		}
	}
	return m
}
