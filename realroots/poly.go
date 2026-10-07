// Package realroots isolates every real root of a polynomial whose
// coefficients are finite decimal numbers (shopspring/decimal) inside a
// caller supplied interval.
//
// All internal arithmetic is performed exactly with math/big rationals.
// Coefficients are never converted to float64, so the answers carry a
// mathematical proof rather than a floating-point error estimate.
package realroots

import (
	"math/big"
)

// ratPoly is a polynomial over the rationals. Coefficient of x^i lives at
// index i. Slices returned by the constructors are trimmed of trailing zero
// coefficients; helpers never mutate their inputs unless noted.
type ratPoly []*big.Rat

func newRatPoly(degree int) ratPoly {
	p := make(ratPoly, degree+1)
	for i := range p {
		p[i] = new(big.Rat)
	}
	return p
}

// trim returns a copy without trailing zero coefficients.
func (p ratPoly) trim() ratPoly {
	d := len(p) - 1
	for d >= 0 && p[d].Sign() == 0 {
		d--
	}
	if d < 0 {
		return ratPoly{new(big.Rat)}
	}
	q := make(ratPoly, d+1)
	for i := 0; i <= d; i++ {
		q[i] = new(big.Rat).Set(p[i])
	}
	return q
}

func (p ratPoly) degree() int {
	for d := len(p) - 1; d >= 0; d-- {
		if p[d].Sign() != 0 {
			return d
		}
	}
	return -1
}

func (p ratPoly) isZero() bool { return p.degree() < 0 }

// eval evaluates p at x using Horner's rule. The result is newly allocated.
func (p ratPoly) eval(x *big.Rat) *big.Rat {
	r := new(big.Rat)
	for i := len(p) - 1; i >= 0; i-- {
		if p[i].Sign() != 0 || r.Sign() != 0 {
			r.Mul(r, x)
			r.Add(r, p[i])
		}
	}
	return r
}

func (p ratPoly) evalSign(x *big.Rat) int { return p.eval(x).Sign() }

// monic returns p divided by its leading coefficient.
func (p ratPoly) monic() ratPoly {
	d := p.degree()
	if d < 0 {
		return p.trim()
	}
	lc := p[d]
	q := newRatPoly(d)
	for i := 0; i <= d; i++ {
		q[i].Quo(p[i], lc)
	}
	return q
}

// neg returns -p.
func (p ratPoly) neg() ratPoly {
	q := newRatPoly(len(p) - 1)
	for i := range p {
		q[i].Neg(p[i])
	}
	return q.trim()
}

// add returns p+q.
func (p ratPoly) add(q ratPoly) ratPoly {
	n := len(p)
	if len(q) > n {
		n = len(q)
	}
	r := newRatPoly(n - 1)
	for i := 0; i < n; i++ {
		if i < len(p) {
			r[i].Add(r[i], p[i])
		}
		if i < len(q) {
			r[i].Add(r[i], q[i])
		}
	}
	return r.trim()
}

// sub returns p-q.
func (p ratPoly) sub(q ratPoly) ratPoly {
	n := len(p)
	if len(q) > n {
		n = len(q)
	}
	r := newRatPoly(n - 1)
	for i := 0; i < n; i++ {
		if i < len(p) {
			r[i].Add(r[i], p[i])
		}
		if i < len(q) {
			r[i].Sub(r[i], q[i])
		}
	}
	return r.trim()
}

// mul returns p*q.
func (p ratPoly) mul(q ratPoly) ratPoly {
	if p.isZero() || q.isZero() {
		return ratPoly{new(big.Rat)}
	}
	r := newRatPoly(p.degree() + q.degree())
	for i := range p {
		if p[i].Sign() == 0 {
			continue
		}
		for j := range q {
			if q[j].Sign() == 0 {
				continue
			}
			t := new(big.Rat).Mul(p[i], q[j])
			r[i+j].Add(r[i+j], t)
		}
	}
	return r.trim()
}

// scale returns p multiplied by the scalar c.
func (p ratPoly) scale(c *big.Rat) ratPoly {
	q := newRatPoly(len(p) - 1)
	for i := range p {
		q[i].Mul(p[i], c)
	}
	return q.trim()
}

// divmod returns the quotient and remainder of p divided by d using exact
// polynomial long division over the rationals. d must not be zero.
func (p ratPoly) divmod(d ratPoly) (q, r ratPoly) {
	dd := d.degree()
	r = p.trim()
	q = newRatPoly(p.degree()) // coefficients start at zero
	for rd := r.degree(); rd >= dd; rd = r.degree() {
		if rd < dd {
			break
		}
		c := new(big.Rat).Quo(r[rd], d[dd])
		k := rd - dd
		q[k].Add(q[k], c)
		// r <- r - c*x^k*d
		for j := 0; j <= dd; j++ {
			t := new(big.Rat).Mul(c, d[j])
			r[k+j].Sub(r[k+j], t)
		}
		r = r.trim()
	}
	return q.trim(), r
}

// mod returns the remainder of p divided by d.
func (p ratPoly) mod(d ratPoly) ratPoly {
	_, r := p.divmod(d)
	return r
}

// gcd returns the monic greatest common divisor of p and q.
func ratPolyGcd(p, q ratPoly) ratPoly {
	a := p.trim()
	b := q.trim()
	for !b.isZero() {
		a, b = b, a.mod(b)
	}
	if a.isZero() {
		return a
	}
	return a.monic()
}

// derivative returns the formal derivative of p.
func (p ratPoly) derivative() ratPoly {
	d := p.degree()
	if d <= 0 {
		return ratPoly{new(big.Rat)}
	}
	q := newRatPoly(d - 1)
	for i := 1; i <= d; i++ {
		q[i-1].SetInt64(int64(i))
		q[i-1].Mul(q[i-1], p[i])
	}
	return q.trim()
}

// divideByLinear returns p/(x-a). p(a) must be zero. Synthetic division.
func (p ratPoly) divideByLinear(a *big.Rat) ratPoly {
	d := p.degree()
	if d < 1 {
		return ratPoly{new(big.Rat)}
	}
	q := newRatPoly(d - 1)
	q[d-1].Set(p[d])
	for i := d - 1; i >= 1; i-- {
		t := new(big.Rat).Mul(a, q[i])
		q[i-1].Add(p[i], t)
	}
	return q.trim()
}

// squareFreeFactor is a factor g together with the multiplicity with which
// every root of g occurs in the original polynomial.
type squareFreeFactor struct {
	factor       ratPoly
	multiplicity int
}

// squareFreeYun applies Yun's algorithm over Q. The returned factors are
// monic, pairwise coprime and square free; p equals their product (up to a
// constant) with the stated multiplicities. p must be non-constant.
func squareFreeYun(p ratPoly) []squareFreeFactor {
	f := p.monic()
	fp := f.derivative()
	g := ratPolyGcd(f, fp)

	z, _ := f.divmod(g)  // f1
	w, _ := fp.divmod(g) // c1
	y := w.sub(z.derivative())

	var factors []squareFreeFactor
	mult := 1
	for {
		gg := ratPolyGcd(z, y)
		if gg.degree() > 0 {
			factors = append(factors, squareFreeFactor{factor: gg, multiplicity: mult})
		}
		z, _ = z.divmod(gg)
		if z.degree() == 0 {
			break
		}
		w, _ = y.divmod(gg)
		y = w.sub(z.derivative())
		mult++
	}
	return factors
}
