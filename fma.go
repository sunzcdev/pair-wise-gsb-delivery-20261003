package decimal

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// DefaultFusedMaxDigits is the computation budget used by fused operations
// when FusedContext.MaxDigits is left at zero. It bounds the number of
// decimal digits of every intermediate big.Int produced while aligning
// exponents, so a hostile mix of exponents cannot silently allocate an
// unbounded integer.
const DefaultFusedMaxDigits int64 = 100000

// ErrFusedBudgetExceeded is returned when a fused operation would need an
// intermediate integer with more decimal digits than the configured budget
// (FusedContext.MaxDigits). The operation never falls back to float64; it
// fails instead so callers can raise the budget explicitly.
var ErrFusedBudgetExceeded = errors.New("decimal: fused operation exceeds the configured digit budget")

// ErrFusedExponentOverflow is returned when the exact exponent of an
// intermediate product does not fit in an int32.
var ErrFusedExponentOverflow = errors.New("decimal: fused operation exponent overflows int32")

// RoundingMode selects how the single final rounding of a fused operation
// is performed.
type RoundingMode int

const (
	// RoundingHalfUp rounds to the nearest representable value; ties are
	// rounded away from zero. It matches Decimal.Round.
	RoundingHalfUp RoundingMode = iota
	// RoundingHalfEven rounds to the nearest representable value; ties are
	// rounded to the even neighbor. It matches Decimal.RoundBank.
	RoundingHalfEven
	// RoundingDown truncates towards zero. It matches Decimal.RoundDown.
	RoundingDown
	// RoundingUp rounds away from zero. It matches Decimal.RoundUp.
	RoundingUp
	// RoundingFloor rounds towards negative infinity. It matches Decimal.RoundFloor.
	RoundingFloor
	// RoundingCeil rounds towards positive infinity. It matches Decimal.RoundCeil.
	RoundingCeil
)

// FusedContext configures fused multiply-add operations.
//
// The intermediate products and their sum are computed exactly; the result
// is rounded exactly once, to Places digits after the decimal point, using
// the Rounding mode. This avoids the double-rounding error of
// a.Mul(b).Round(p).Add(...) where rounding the product first can wipe out
// a small remainder left by cancellation of large terms.
//
// MaxDigits is the computation budget: the maximum number of decimal digits
// allowed in any intermediate aligned coefficient, in the exact total, and
// in the power of ten used for the final rescale. If any of these would
// exceed the budget, the operation returns ErrFusedBudgetExceeded instead of
// allocating a runaway big.Int or silently degrading to float64. A zero or
// negative MaxDigits selects DefaultFusedMaxDigits.
type FusedContext struct {
	// Places is the number of digits after the decimal point of the result.
	// It may be negative to round to tens, hundreds, etc.
	Places int32
	// Rounding is the rounding mode applied once, at the very end.
	Rounding RoundingMode
	// MaxDigits bounds the size of intermediate integers; see the type doc.
	MaxDigits int64
}

// fusedTerm is one exactly-representable summand: coeff * 10^exp.
type fusedTerm struct {
	coeff *big.Int
	exp   int64
}

// FMA returns a*b + c, computed exactly and rounded once according to ctx.
//
// A zero addend (including the zero-value Decimal) contributes nothing and
// its exponent does not count against the digit budget.
func (ctx FusedContext) FMA(a, b, c Decimal) (Decimal, error) {
	terms := make([]fusedTerm, 0, 2)
	if a.getValue().Sign() != 0 && b.getValue().Sign() != 0 {
		exp := int64(a.exp) + int64(b.exp)
		if exp > math.MaxInt32 || exp < math.MinInt32 {
			return Decimal{}, ErrFusedExponentOverflow
		}
		terms = append(terms, fusedTerm{new(big.Int).Mul(a.getValue(), b.getValue()), exp})
	}
	if c.getValue().Sign() != 0 {
		terms = append(terms, fusedTerm{new(big.Int).Set(c.getValue()), int64(c.exp)})
	}
	return ctx.roundExactSum(terms)
}

// DotProduct returns xs[0]*ys[0] + ... + xs[n-1]*ys[n-1], computed exactly
// and rounded once according to ctx.
//
// Conventions:
//   - xs and ys must have the same length, otherwise an error is returned.
//   - An empty dot product is defined as exact zero; the result is zero at
//     the requested precision, and no budget is consumed.
//   - Terms where either factor is zero contribute nothing and are skipped
//     entirely, so their exponents never count against the digit budget and
//     never cause exponent overflow.
//   - Extreme exponents are fine as long as the aligned coefficients stay
//     within the digit budget; otherwise ErrFusedBudgetExceeded is returned.
func (ctx FusedContext) DotProduct(xs, ys []Decimal) (Decimal, error) {
	if len(xs) != len(ys) {
		return Decimal{}, fmt.Errorf("decimal: dot product length mismatch: %d != %d", len(xs), len(ys))
	}
	terms := make([]fusedTerm, 0, len(xs))
	for i := range xs {
		x, y := xs[i].getValue(), ys[i].getValue()
		if x.Sign() == 0 || y.Sign() == 0 {
			continue
		}
		exp := int64(xs[i].exp) + int64(ys[i].exp)
		if exp > math.MaxInt32 || exp < math.MinInt32 {
			return Decimal{}, ErrFusedExponentOverflow
		}
		terms = append(terms, fusedTerm{new(big.Int).Mul(x, y), exp})
	}
	return ctx.roundExactSum(terms)
}

// roundExactSum sums the terms exactly and rounds the total once, to
// ctx.Places digits after the decimal point, using ctx.Rounding.
func (ctx FusedContext) roundExactSum(terms []fusedTerm) (Decimal, error) {
	if ctx.Places == math.MinInt32 {
		return Decimal{}, fmt.Errorf("decimal: fused places %d is out of range", ctx.Places)
	}
	if ctx.Rounding < RoundingHalfUp || ctx.Rounding > RoundingCeil {
		return Decimal{}, fmt.Errorf("decimal: unknown fused rounding mode %d", ctx.Rounding)
	}
	budget := ctx.MaxDigits
	if budget <= 0 {
		budget = DefaultFusedMaxDigits
	}

	if len(terms) == 0 {
		return Decimal{value: new(big.Int), exp: -ctx.Places}, nil
	}

	minExp := terms[0].exp
	for _, t := range terms[1:] {
		if t.exp < minExp {
			minExp = t.exp
		}
	}

	sum := new(big.Int)
	aligned := new(big.Int)
	for _, t := range terms {
		shift := t.exp - minExp
		if int64(fusedNumDigits(t.coeff))+shift > budget {
			return Decimal{}, ErrFusedBudgetExceeded
		}
		aligned.Mul(t.coeff, fusedPow10(shift))
		sum.Add(sum, aligned)
	}
	if int64(fusedNumDigits(sum)) > budget {
		return Decimal{}, ErrFusedBudgetExceeded
	}

	targetExp := int64(-ctx.Places)
	coeff := new(big.Int)
	if minExp >= targetExp {
		shift := minExp - targetExp
		if int64(fusedNumDigits(sum))+shift > budget {
			return Decimal{}, ErrFusedBudgetExceeded
		}
		coeff.Mul(sum, fusedPow10(shift))
	} else {
		drop := targetExp - minExp
		if drop > budget {
			return Decimal{}, ErrFusedBudgetExceeded
		}
		divisor := fusedPow10(drop)
		rem := new(big.Int)
		coeff.QuoRem(sum, divisor, rem)
		applyFusedRounding(coeff, rem, sum.Sign(), divisor, ctx.Rounding)
	}

	return Decimal{value: coeff, exp: -ctx.Places}, nil
}

// applyFusedRounding adjusts q (the truncated quotient of sum/divisor) in
// place according to the rounding mode. r is the remainder, sign the sign of
// the exact total, and divisor the power of ten that was divided out.
func applyFusedRounding(q, r *big.Int, sign int, divisor *big.Int, mode RoundingMode) {
	if r.Sign() == 0 {
		return
	}
	switch mode {
	case RoundingUp:
		q.Add(q, big.NewInt(int64(sign)))
	case RoundingFloor:
		if sign < 0 {
			q.Sub(q, oneInt)
		}
	case RoundingCeil:
		if sign > 0 {
			q.Add(q, oneInt)
		}
	case RoundingHalfUp, RoundingHalfEven:
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		cmp := twice.Cmp(divisor)
		if cmp > 0 || cmp == 0 && (mode == RoundingHalfUp || q.Bit(0) == 1) {
			q.Add(q, big.NewInt(int64(sign)))
		}
	}
}

// fusedNumDigits returns the number of decimal digits of |x|; zero has none.
func fusedNumDigits(x *big.Int) int {
	if x.Sign() == 0 {
		return 0
	}
	n := len(x.Text(10))
	if x.Sign() < 0 {
		n--
	}
	return n
}

func fusedPow10(n int64) *big.Int {
	return new(big.Int).Exp(tenInt, big.NewInt(n), nil)
}
