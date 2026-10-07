package realroots

import "math/big"

// sturm holds a Sturm sequence for a square-free polynomial. Each remainder
// is negated, exactly as Sturm's theorem prescribes.
type sturm struct {
	seq []ratPoly
	ops *budget
}

// newSturm builds the Sturm sequence of the square-free polynomial p.
func newSturm(p ratPoly, ops *budget) *sturm {
	s := &sturm{ops: ops}
	s.seq = append(s.seq, p.trim())
	if p.degree() <= 0 {
		return s
	}
	s.seq = append(s.seq, p.derivative())
	for {
		a := s.seq[len(s.seq)-2]
		bp := s.seq[len(s.seq)-1]
		s.ops.remainderStep(a.degree())
		r := a.mod(bp)
		if r.isZero() {
			break
		}
		s.seq = append(s.seq, r.neg())
	}
	return s
}

// variations counts sign changes in the Sturm sequence evaluated at x.
func (s *sturm) variations(x *big.Rat) int {
	prev := 0
	count := 0
	for _, q := range s.seq {
		s.ops.polyEval(q)
		sgn := q.evalSign(x)
		if sgn == 0 {
			// A zero of one sequence member is skipped when counting
			// sign variations (standard Sturm convention).
			continue
		}
		if prev != 0 && sgn != prev {
			count++
		}
		prev = sgn
	}
	return count
}

// rootsOpen counts distinct roots in the open interval (lo, hi).
func (s *sturm) rootsOpen(lo, hi *big.Rat) int {
	n := s.variations(lo) - s.variations(hi)
	if n < 0 {
		return 0
	}
	return n
}

// rootsAtRight counts roots exactly at the point x. For a square-free
// polynomial there is at most one root at any point, so the answer is 0 or 1.
func (s *sturm) rootsAtRight(x *big.Rat) int {
	s.ops.polyEval(s.seq[0])
	if s.seq[0].evalSign(x) == 0 {
		return 1
	}
	return 0
}
