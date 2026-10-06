package decimal

import (
	"fmt"
	"math/big"
)

// SqrtMaxScale is an explicit resource limit for the Sqrt method.
//
// Sqrt scales the radicand by 10^(exp + 2*places) before computing the
// integer square root, so the magnitude of exp + 2*places bounds the size
// of the integers (and thus the memory and CPU time) involved. Requests
// whose scale exceeds SqrtMaxScale in absolute value are rejected with an
// error instead of attempting an unbounded computation.
var SqrtMaxScale int64 = 100000

// RoundingMode selects how Sqrt rounds its result to the requested number
// of decimal places.
type RoundingMode int

const (
	// RoundDown rounds towards zero (truncation).
	RoundDown RoundingMode = iota
	// RoundCeiling rounds towards positive infinity.
	RoundCeiling
	// RoundFloor rounds towards negative infinity.
	RoundFloor
	// RoundHalfUp rounds to the nearest neighbor; exact midpoints are
	// rounded away from zero.
	RoundHalfUp
	// RoundHalfEven rounds to the nearest neighbor; exact midpoints are
	// rounded to the neighbor with an even least significant digit
	// (banker's rounding).
	RoundHalfEven
)

// Sqrt returns the square root of d rounded to the given number of decimal
// places using the given rounding mode.
//
// The computation is performed entirely with exact integer arithmetic: the
// radicand is scaled to an integer, its floor square root is computed and
// verified by exact squaring, and the final rounding decision is made by
// exactly comparing the radicand against the rounding midpoint. No float64
// approximation is involved at any step, so the last digit of the result is
// provably correct, including on exact rounding boundaries.
//
// An error is returned if d is negative, if the rounding mode is invalid,
// or if the requested scale exp + 2*places exceeds SqrtMaxScale.
//
// Example:
//
//	d, _ := decimal.NewFromString("2")
//	r, _ := d.Sqrt(10, decimal.RoundHalfEven)
//	r.String() // output: "1.4142135624"
func (d Decimal) Sqrt(places int32, mode RoundingMode) (Decimal, error) {
	if mode < RoundDown || mode > RoundHalfEven {
		return Decimal{}, fmt.Errorf("decimal: invalid sqrt rounding mode %d", int(mode))
	}
	if d.Sign() < 0 {
		return Decimal{}, fmt.Errorf("decimal: cannot compute square root of negative number %s", d.String())
	}

	scale := int64(d.exp) + 2*int64(places)
	if scale > SqrtMaxScale || scale < -SqrtMaxScale {
		return Decimal{}, fmt.Errorf("decimal: sqrt scale %d (exp %d + 2*places %d) exceeds the limit of %d", scale, d.exp, places, SqrtMaxScale)
	}

	value := d.getValue()

	// scaled = floor(d * 10^(2*places)); its integer square root is the
	// result truncated to the requested number of places.
	scaled := new(big.Int)
	if scale >= 0 {
		scaled.Mul(value, pow10BigInt(scale))
	} else {
		scaled.Quo(value, pow10BigInt(-scale))
	}
	root := sqrtFloorBigInt(scaled)

	// Adjust the truncated root according to the rounding mode. Every
	// comparison below is exact integer arithmetic on
	// d * 10^(2*places) = value * 10^scale.
	increment := false
	switch mode {
	case RoundDown, RoundFloor:
		// d >= 0, so both modes truncate; root is already correct.
	case RoundCeiling:
		// Increment unless root is the exact square root.
		increment = cmpScaledSquare(value, scale, root) != 0
	case RoundHalfUp:
		// Round up iff d * 10^(2*places) >= (root + 1/2)^2,
		// i.e. 4 * value * 10^scale >= (2*root + 1)^2.
		increment = cmpScaledSquareMidpoint(value, scale, root) >= 0
	case RoundHalfEven:
		cmp := cmpScaledSquareMidpoint(value, scale, root)
		increment = cmp > 0 || (cmp == 0 && root.Bit(0) == 1)
	}

	if increment {
		root.Add(root, oneInt)
	}
	return NewFromBigInt(root, -places), nil
}

// cmpScaledSquare compares value * 10^scale with m^2 using exact integer
// arithmetic and returns -1, 0 or 1.
func cmpScaledSquare(value *big.Int, scale int64, m *big.Int) int {
	lhs := new(big.Int).Set(value)
	rhs := new(big.Int).Mul(m, m)
	if scale >= 0 {
		lhs.Mul(lhs, pow10BigInt(scale))
	} else {
		rhs.Mul(rhs, pow10BigInt(-scale))
	}
	return lhs.Cmp(rhs)
}

// cmpScaledSquareMidpoint compares 4 * value * 10^scale with (2*m + 1)^2
// using exact integer arithmetic and returns -1, 0 or 1. This locates
// value * 10^scale relative to the rounding midpoint (m + 1/2)^2.
func cmpScaledSquareMidpoint(value *big.Int, scale int64, m *big.Int) int {
	lhs := new(big.Int).Lsh(new(big.Int).Set(value), 2)
	rhs := new(big.Int).Lsh(new(big.Int).Set(m), 1)
	rhs.Add(rhs, oneInt)
	rhs.Mul(rhs, rhs)
	if scale >= 0 {
		lhs.Mul(lhs, pow10BigInt(scale))
	} else {
		rhs.Mul(rhs, pow10BigInt(-scale))
	}
	return lhs.Cmp(rhs)
}

// sqrtFloorBigInt returns floor(sqrt(x)) for x >= 0.
//
// Newton's method is used only to get close to the answer efficiently; the
// returned value is then proven correct by exact integer comparisons
// enforcing r^2 <= x < (r+1)^2. Correctness never rests on two successive
// approximations merely being equal.
func sqrtFloorBigInt(x *big.Int) *big.Int {
	r := new(big.Int)
	if x.Sign() == 0 {
		return r
	}

	// Initial guess 2^ceil(bitlen/2) >= sqrt(x), so the Newton iteration
	// below converges monotonically from above.
	r.Lsh(oneInt, uint(x.BitLen()+1)/2)
	t := new(big.Int)
	for {
		t.Quo(x, r).Add(t, r).Rsh(t, 1)
		if t.Cmp(r) >= 0 {
			break
		}
		r.Set(t)
	}

	// Exact fix-up: enforce r^2 <= x < (r+1)^2 by direct comparison.
	sq := new(big.Int).Mul(r, r)
	for sq.Cmp(x) > 0 {
		r.Sub(r, oneInt)
		sq.Mul(r, r)
	}
	next := new(big.Int)
	for {
		next.Add(r, oneInt)
		sq.Mul(next, next)
		if sq.Cmp(x) > 0 {
			break
		}
		r.Set(next)
	}
	return r
}

// pow10BigInt returns 10^n as a big.Int. n must be non-negative.
func pow10BigInt(n int64) *big.Int {
	return new(big.Int).Exp(tenInt, big.NewInt(n), nil)
}
