package decimal

import (
	"fmt"
	"math/big"
)

// SqrtRoundingMode selects the rounding rule used by Sqrt when the square
// root is not exactly representable at the requested precision.
type SqrtRoundingMode int

const (
	// SqrtRoundToZero rounds towards zero (truncation).
	SqrtRoundToZero SqrtRoundingMode = iota + 1
	// SqrtRoundAwayFromZero rounds away from zero.
	SqrtRoundAwayFromZero
	// SqrtRoundCeil rounds towards positive infinity.
	SqrtRoundCeil
	// SqrtRoundFloor rounds towards negative infinity.
	SqrtRoundFloor
	// SqrtRoundHalfUp rounds to the nearest neighbour; ties are resolved
	// away from zero.
	SqrtRoundHalfUp
	// SqrtRoundHalfEven rounds to the nearest neighbour; ties are resolved
	// towards the neighbour whose last digit is even ("banker's rounding").
	SqrtRoundHalfEven
)

// SqrtMaxPrecision is the largest number of decimal places Sqrt accepts in a
// single call. It bounds the size of the integer square-root extraction and
// therefore the memory and CPU a request can consume.
var SqrtMaxPrecision int32 = 10000

// SqrtMaxExponent bounds the magnitude of the decimal exponent of the
// radicand accepted by Sqrt.
var SqrtMaxExponent int32 = 1000000

// SqrtMaxCoeffDigits bounds the number of decimal digits of the radicand
// coefficient accepted by Sqrt.
var SqrtMaxCoeffDigits = 100000

// Sqrt returns the square root of d rounded to precision digits after the
// decimal point under the requested rounding mode. precision is the target
// number of digits after the decimal point; negative precision requests,
// precision above SqrtMaxPrecision, exponents beyond SqrtMaxExponent and
// coefficients longer than SqrtMaxCoeffDigits are rejected.
//
// The computation is performed entirely with arbitrary-precision integers and
// never converts through float64. The final digit is decided by an exact
// integer comparison of the squared midpoint against the radicand, not by the
// equality of two successive floating approximations, so the caller can audit
// which side of the rounding boundary the exact root lies on.
//
// Sqrt returns an error when d is negative (the result is not a real
// decimal), when precision is out of the allowed range, when the request
// exceeds the configured resource limits, or when mode is unknown.
func (d Decimal) Sqrt(precision int32, mode SqrtRoundingMode) (Decimal, error) {
	if precision < 0 {
		return Decimal{}, fmt.Errorf("Sqrt: precision must be non-negative, got %d", precision)
	}
	if precision > SqrtMaxPrecision {
		return Decimal{}, fmt.Errorf("Sqrt: precision %d exceeds SqrtMaxPrecision %d", precision, SqrtMaxPrecision)
	}
	switch mode {
	case SqrtRoundToZero, SqrtRoundAwayFromZero, SqrtRoundCeil,
		SqrtRoundFloor, SqrtRoundHalfUp, SqrtRoundHalfEven:
	default:
		return Decimal{}, fmt.Errorf("Sqrt: unsupported rounding mode %d", mode)
	}
	absExp := d.exp
	if absExp < 0 {
		absExp = -absExp
	}
	if absExp > SqrtMaxExponent {
		return Decimal{}, fmt.Errorf("Sqrt: exponent %d exceeds SqrtMaxExponent %d", d.exp, SqrtMaxExponent)
	}
	if d.NumDigits() > SqrtMaxCoeffDigits {
		return Decimal{}, fmt.Errorf("Sqrt: radicand has more than %d digits", SqrtMaxCoeffDigits)
	}

	if d.getValue().Sign() < 0 {
		return Decimal{}, fmt.Errorf("Sqrt: cannot compute square root of negative number %s", d.String())
	}
	if d.IsZero() {
		return Decimal{value: new(big.Int), exp: -precision}, nil
	}

	// The exact root coefficient c (in units of 10^-precision) satisfies
	//
	//	c = sqrt(v0 * 10^e),  e = d.exp + 2*precision.
	//
	// To keep everything integral let g = max(0, -e) and
	//
	//	N = v0 * 10^|e|,  so that  c = sqrt(N) / 10^g.
	//
	// Let Q = floor(sqrt(N)) (exact integer square root) and
	// r = floor(Q / 10^g) = floor(c).  Every decision below is an exact
	// integer comparison; no float64 value is ever produced.
	v0 := new(big.Int).Set(d.getValue())
	e := int64(d.exp) + 2*int64(precision)
	k := e
	if k < 0 {
		k = -k
	}
	g := int64(0)
	if e < 0 {
		g = -e
	}

	n := new(big.Int).Mul(v0, new(big.Int).Exp(tenInt, big.NewInt(k), nil))
	q := sqrtFloor(n)
	r := new(big.Int).Quo(q, new(big.Int).Exp(tenInt, big.NewInt(g), nil))

	scale := new(big.Int).Exp(tenInt, big.NewInt(2*g), nil) // 10^(2g)

	// exact means c == r, i.e. N == r^2 * 10^(2g).
	rSqScaled := new(big.Int).Mul(new(big.Int).Mul(r, r), scale)
	exact := rSqScaled.Cmp(n) == 0

	// cmp compares the exact root c with the rounding midpoint r + 1/2:
	//
	//	cmp = (2r+1)^2 * 10^(2g) - 4*N
	//
	//	cmp > 0: c < midpoint (round down); cmp == 0: exact tie;
	//	cmp < 0: c > midpoint (round up).
	midSq := new(big.Int).Mul(new(big.Int).Add(new(big.Int).Lsh(r, 1), oneInt),
		new(big.Int).Add(new(big.Int).Lsh(r, 1), oneInt))
	midSq.Mul(midSq, scale)
	cmp := midSq.Sub(midSq, new(big.Int).Lsh(n, 2))

	if exact {
		return Decimal{value: r, exp: -precision}, nil
	}

	switch mode {
	case SqrtRoundToZero, SqrtRoundFloor:
		// Non-negative root: truncation and floor coincide.
	case SqrtRoundCeil:
		// Non-negative root: ceil and away-from-zero coincide.
		fallthrough
	case SqrtRoundAwayFromZero:
		r.Add(r, oneInt)
	case SqrtRoundHalfUp:
		if cmp.Sign() <= 0 {
			r.Add(r, oneInt)
		}
	case SqrtRoundHalfEven:
		if cmp.Sign() < 0 || cmp.Sign() == 0 && r.Bit(0) != 0 {
			r.Add(r, oneInt)
		}
	}

	return Decimal{value: r, exp: -precision}, nil
}

// sqrtFloor returns floor(sqrt(n)) for n >= 0 using integer-only Newton
// iteration. Starting from an overestimate x0 >= sqrt(n), the sequence
// x_{i+1} = floor((x_i + floor(n/x_i))/2) strictly decreases while above the
// root; the first step with x_{i+1} >= x_i is the exact fixed point
// floor(sqrt(n)).
func sqrtFloor(n *big.Int) *big.Int {
	if n.Sign() <= 0 {
		return new(big.Int)
	}

	// Initial guess 2^ceil(BitLen(n)/2), guaranteed >= sqrt(n).
	x := new(big.Int).Lsh(oneInt, uint((n.BitLen()+1)/2))
	for {
		y := new(big.Int).Quo(new(big.Int).Add(x, new(big.Int).Quo(new(big.Int).Set(n), x)), twoInt)
		if y.Cmp(x) >= 0 {
			return x
		}
		x = y
	}
}
