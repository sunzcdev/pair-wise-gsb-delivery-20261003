package decimal

import "math/big"

// ratPoly is a univariate polynomial with exact rational coefficients,
// stored in ascending order: p[i] is the coefficient of x^i.
// The zero polynomial is represented by an empty slice and normalized
// polynomials never have trailing zero coefficients.
type ratPoly []*big.Rat

// ratPolyFromDecimals converts decimal coefficients (ascending order) to
// an exact rational polynomial. The conversion is lossless.
func ratPolyFromDecimals(coeffs []Decimal) ratPoly {
	p := make(ratPoly, len(coeffs))
	for i, c := range coeffs {
		p[i] = c.Rat()
	}
	return ratPolyNorm(p)
}

// ratPolyNorm drops trailing zero coefficients.
func ratPolyNorm(p ratPoly) ratPoly {
	for len(p) > 0 && p[len(p)-1].Sign() == 0 {
		p = p[:len(p)-1]
	}
	return p
}

// ratPolyDegree returns -1 for the zero polynomial.
func ratPolyDegree(p ratPoly) int {
	return len(p) - 1
}

// ratPolyIsOne reports whether p is the constant polynomial 1.
func ratPolyIsOne(p ratPoly) bool {
	return len(p) == 1 && p[0].Cmp(big.NewRat(1, 1)) == 0
}

// ratPolyMonic returns p scaled to have leading coefficient 1.
func ratPolyMonic(p ratPoly) ratPoly {
	if len(p) == 0 {
		return p
	}
	lc := p[len(p)-1]
	q := make(ratPoly, len(p))
	for i, c := range p {
		q[i] = new(big.Rat).Quo(c, lc)
	}
	return q
}

// ratPolyEval evaluates p at x with Horner's scheme, exactly.
func ratPolyEval(p ratPoly, x *big.Rat) *big.Rat {
	acc := new(big.Rat)
	for i := len(p) - 1; i >= 0; i-- {
		acc.Mul(acc, x)
		acc.Add(acc, p[i])
	}
	return acc
}

// ratPolyDeriv returns the formal derivative of p.
func ratPolyDeriv(p ratPoly) ratPoly {
	if len(p) <= 1 {
		return nil
	}
	d := make(ratPoly, len(p)-1)
	for i := 1; i < len(p); i++ {
		d[i-1] = new(big.Rat).Mul(p[i], new(big.Rat).SetInt64(int64(i)))
	}
	return ratPolyNorm(d)
}

// ratPolyNeg returns -p.
func ratPolyNeg(p ratPoly) ratPoly {
	q := make(ratPoly, len(p))
	for i, c := range p {
		q[i] = new(big.Rat).Neg(c)
	}
	return q
}

// ratPolyDivRem computes quotient and remainder of num/den in Q[x].
// It panics on division by the zero polynomial.
func ratPolyDivRem(num, den ratPoly) (quo, rem ratPoly) {
	if len(den) == 0 {
		panic("decimal: polynomial division by zero polynomial")
	}
	rem = make(ratPoly, len(num))
	for i, c := range num {
		rem[i] = new(big.Rat).Set(c)
	}
	rem = ratPolyNorm(rem)
	quo = ratPoly{}
	for len(rem) >= len(den) && len(rem) > 0 {
		shift := len(rem) - len(den)
		coeff := new(big.Rat).Quo(rem[len(rem)-1], den[len(den)-1])
		for len(quo) <= shift {
			quo = append(quo, new(big.Rat))
		}
		quo[shift].Set(coeff)
		for i := 0; i < len(den); i++ {
			rem[shift+i].Sub(rem[shift+i], new(big.Rat).Mul(coeff, den[i]))
		}
		rem = ratPolyNorm(rem)
	}
	return ratPolyNorm(quo), rem
}

// ratPolyGCD returns the monic greatest common divisor of a and b.
// The budget is charged one operation per Euclidean step; ok is false
// when the budget is exhausted mid-computation.
func ratPolyGCD(a, b ratPoly, budget *opBudget) (g ratPoly, ok bool) {
	a = ratPolyMonic(a)
	b = ratPolyMonic(b)
	for len(b) > 0 {
		if !budget.charge(1) {
			return nil, false
		}
		_, r := ratPolyDivRem(a, b)
		a, b = b, ratPolyMonic(r)
	}
	return a, true
}

// squareFreeFactor holds one square-free factor of a polynomial together
// with the multiplicity with which it divides the polynomial.
type squareFreeFactor struct {
	factor       ratPoly
	multiplicity int
}

// squareFreeFactorize decomposes the non-constant polynomial p into
// pairwise coprime square-free factors f_i such that
// p = prod f_i^i (Yun's algorithm). Roots of f_i are exactly the roots
// of p with multiplicity i. p is assumed monic; the returned factors are
// monic as well.
func squareFreeFactorize(p ratPoly, budget *opBudget) ([]squareFreeFactor, bool) {
	if !budget.charge(1) {
		return nil, false
	}
	deriv := ratPolyDeriv(p)
	c, ok := ratPolyGCD(p, deriv, budget)
	if !ok {
		return nil, false
	}
	if !budget.charge(1) {
		return nil, false
	}
	w, _ := ratPolyDivRem(p, c)
	w = ratPolyMonic(w)
	var out []squareFreeFactor
	for mult := 1; !ratPolyIsOne(w); mult++ {
		y, ok := ratPolyGCD(w, c, budget)
		if !ok {
			return nil, false
		}
		if !budget.charge(1) {
			return nil, false
		}
		z, _ := ratPolyDivRem(w, y)
		z = ratPolyMonic(z)
		if !ratPolyIsOne(z) {
			out = append(out, squareFreeFactor{factor: z, multiplicity: mult})
		}
		if !budget.charge(1) {
			return nil, false
		}
		c, _ = ratPolyDivRem(c, y)
		c = ratPolyMonic(c)
		w = y
	}
	return out, true
}

// sturmSequence builds the Sturm sequence of the square-free polynomial f:
// s0 = f, s1 = f', s_{k+1} = -rem(s_{k-1}, s_k). Because f is square-free
// the sequence ends with a non-zero constant.
func sturmSequence(f ratPoly, budget *opBudget) ([]ratPoly, bool) {
	if !budget.charge(1) {
		return nil, false
	}
	seq := []ratPoly{f, ratPolyDeriv(f)}
	for {
		if !budget.charge(1) {
			return nil, false
		}
		_, r := ratPolyDivRem(seq[len(seq)-2], seq[len(seq)-1])
		r = ratPolyNorm(ratPolyNeg(r))
		if len(r) == 0 {
			return seq, true
		}
		seq = append(seq, r)
	}
}

// sturmSignVariations counts sign variations of the Sturm sequence
// evaluated at x, skipping zeros. With this convention the number of
// distinct roots of f in (a, b] equals V(a) - V(b) for any a < b.
func sturmSignVariations(seq []ratPoly, x *big.Rat) int {
	variations := 0
	prev := 0
	for _, s := range seq {
		sign := ratPolyEval(s, x).Sign()
		if sign == 0 {
			continue
		}
		if prev != 0 && sign != prev {
			variations++
		}
		prev = sign
	}
	return variations
}

// sturmCountOpen counts the roots of seq[0] in the open interval (a, b).
// The budget is charged for the required polynomial evaluations; ok is
// false when it is exhausted.
func sturmCountOpen(seq []ratPoly, a, b *big.Rat, budget *opBudget) (n int, ok bool) {
	if !budget.charge(int64(2*len(seq) + 1)) {
		return 0, false
	}
	n = sturmSignVariations(seq, a) - sturmSignVariations(seq, b)
	if ratPolyEval(seq[0], b).Sign() == 0 {
		// V(a) - V(b) counts roots in (a, b]; exclude a root at b.
		n--
	}
	return n, true
}
