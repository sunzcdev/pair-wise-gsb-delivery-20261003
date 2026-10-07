package decimal

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// RoundingMode specifies how a Context rounds a result whose infinitely
// precise value cannot be represented with Context.Precision significant
// digits.
type RoundingMode int

const (
	// RoundHalfUp rounds to the nearest representable value; ties are
	// rounded away from zero. It matches the behavior of DivRound.
	RoundHalfUp RoundingMode = iota
	// RoundHalfEven rounds to the nearest representable value; ties are
	// rounded to the value with an even least significant digit.
	RoundHalfEven
	// RoundDown rounds toward zero (truncation).
	RoundDown
	// RoundUp rounds away from zero.
	RoundUp
	// RoundFloor rounds toward negative infinity.
	RoundFloor
	// RoundCeil rounds toward positive infinity.
	RoundCeil
)

// Condition is a bitmask of exceptional conditions raised by Context
// operations, modeled after IEEE 754 decimal flags.
type Condition uint32

const (
	// Rounded is set whenever digits of the result coefficient are
	// discarded, even if the discarded digits are all zeros and the value
	// is unchanged.
	Rounded Condition = 1 << iota
	// Inexact is set whenever the returned result differs from the
	// infinitely precise result (discarded digits were not all zero).
	Inexact
	// Overflow is set when the adjusted exponent of the rounded result is
	// greater than Context.MaxExponent. The infinitely precise result
	// cannot be represented, so an error is always returned as well.
	Overflow
	// Underflow is set when the adjusted exponent of the rounded result is
	// less than Context.MinExponent. The (finite) result is still
	// returned; an error is returned only if Underflow is trapped.
	Underflow
	// DivisionByZero is set when a Context.Div divisor is zero. The
	// result cannot be represented, so an error is always returned.
	DivisionByZero
)

// Has reports whether all bits of flag are set in c.
func (c Condition) Has(flag Condition) bool {
	return c&flag == flag
}

func (c Condition) String() string {
	var names []string
	for _, e := range []struct {
		flag Condition
		name string
	}{
		{Rounded, "Rounded"},
		{Inexact, "Inexact"},
		{Overflow, "Overflow"},
		{Underflow, "Underflow"},
		{DivisionByZero, "DivisionByZero"},
	} {
		if c&e.flag != 0 {
			names = append(names, e.name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, " | ")
}

// Sentinel errors matched by errors.Is against the *ConditionError returned
// from Context operations.
var (
	ErrRounded        = errors.New("decimal: rounded")
	ErrInexact        = errors.New("decimal: inexact")
	ErrOverflow       = errors.New("decimal: overflow")
	ErrUnderflow      = errors.New("decimal: underflow")
	ErrDivisionByZero = errors.New("decimal: division by zero")
)

// ConditionError is the typed error returned by Context operations when a
// non-finite result occurs (Overflow, DivisionByZero) or when a raised
// condition is trapped by Context.Traps.
type ConditionError struct {
	Condition Condition
}

func (e *ConditionError) Error() string {
	return "decimal: " + e.Condition.String()
}

// Is reports whether target is the sentinel error of any condition bit set
// in e.Condition.
func (e *ConditionError) Is(target error) bool {
	switch target {
	case ErrRounded:
		return e.Condition&Rounded != 0
	case ErrInexact:
		return e.Condition&Inexact != 0
	case ErrOverflow:
		return e.Condition&Overflow != 0
	case ErrUnderflow:
		return e.Condition&Underflow != 0
	case ErrDivisionByZero:
		return e.Condition&DivisionByZero != 0
	}
	return false
}

// Context carries the parameters of a decimal arithmetic operation:
// significant-digit precision, exponent range, rounding mode and trapped
// conditions. Unlike the package-level DivisionPrecision, a Context is
// passed explicitly to each operation, so independent computations in the
// same process (and goroutine) never interfere with each other.
//
// Context is immutable and safe for concurrent use.
type Context struct {
	// Precision is the maximum number of significant digits of a result.
	// It must be >= 1.
	Precision int32
	// MaxExponent is the maximum adjusted exponent (exponent of the most
	// significant digit) of a result; larger results raise Overflow.
	MaxExponent int32
	// MinExponent is the minimum adjusted exponent of a result; smaller
	// results raise Underflow.
	MinExponent int32
	// RoundingMode selects how inexact results are rounded.
	RoundingMode RoundingMode
	// Traps selects the conditions that make an operation return a
	// *ConditionError in addition to the condition flags. Overflow and
	// DivisionByZero always return an error, regardless of Traps, because
	// their results cannot be represented as a finite Decimal.
	Traps Condition
}

// DefaultContext is a Context with 16 significant digits, an unrestricted
// exponent range, RoundHalfUp rounding and no traps.
var DefaultContext = Context{
	Precision:    16,
	MaxExponent:  math.MaxInt32,
	MinExponent:  math.MinInt32,
	RoundingMode: RoundHalfUp,
}

func (c Context) check() {
	if c.Precision < 1 {
		panic(fmt.Sprintf("decimal: Context.Precision must be >= 1, got %d", c.Precision))
	}
}

// Add returns d1 + d2 rounded to the context precision, along with the
// raised conditions and a *ConditionError if a trapped (or non-representable)
// condition occurred.
func (c Context) Add(d1, d2 Decimal) (Decimal, Condition, error) {
	c.check()
	return c.apply(d1.Add(d2), false)
}

// Sub returns d1 - d2 rounded to the context precision, along with the
// raised conditions and a *ConditionError if a trapped (or non-representable)
// condition occurred.
func (c Context) Sub(d1, d2 Decimal) (Decimal, Condition, error) {
	c.check()
	return c.apply(d1.Sub(d2), false)
}

// Mul returns d1 * d2 rounded to the context precision, along with the
// raised conditions and a *ConditionError if a trapped (or non-representable)
// condition occurred.
func (c Context) Mul(d1, d2 Decimal) (Decimal, Condition, error) {
	c.check()
	return c.apply(d1.Mul(d2), false)
}

// Div returns d1 / d2 rounded to Precision significant digits, along with
// the raised conditions and a *ConditionError if a trapped (or
// non-representable) condition occurred. A zero divisor always raises
// DivisionByZero and returns a *ConditionError matching ErrDivisionByZero.
func (c Context) Div(d1, d2 Decimal) (Decimal, Condition, error) {
	c.check()
	if d2.IsZero() {
		cond := DivisionByZero
		return Decimal{}, cond, &ConditionError{Condition: cond}
	}
	if d1.IsZero() {
		return Decimal{}, 0, nil
	}

	// Estimate the adjusted exponent of the quotient (off by at most one)
	// and compute the quotient with Precision+2 significant digits, which
	// leaves one guard digit for the rounding decision.
	adj := int64(d1.NumDigits()) + int64(d1.exp) - int64(d2.NumDigits()) - int64(d2.exp)
	qexp := adj - int64(c.Precision) - 1
	if qexp > math.MaxInt32 {
		// The adjusted exponent of the quotient is far above any
		// representable scale: the result overflows.
		cond := Overflow | Inexact | Rounded
		return Decimal{}, cond, &ConditionError{Condition: cond}
	}
	sticky := false
	if qexp < math.MinInt32 {
		// +1 so that -qexp still fits into an int32 for QuoRem.
		qexp = math.MinInt32 + 1
		sticky = true
	}
	q, r := d1.QuoRem(d2, int32(-qexp))
	if r.Sign() != 0 {
		sticky = true
	} else {
		// The division is exact: strip the trailing zeros of the guard
		// digits so that an exactly representable quotient raises no
		// conditions.
		q = stripTrailingZeros(q)
	}
	return c.apply(q, sticky)
}

// stripTrailingZeros removes trailing zero digits from the coefficient of d,
// increasing its exponent accordingly. The value is unchanged.
func stripTrailingZeros(d Decimal) Decimal {
	value := new(big.Int).Set(d.getValue())
	exp := d.exp
	var q, r big.Int
	for exp < math.MaxInt32 {
		q.QuoRem(value, tenInt, &r)
		if r.Sign() != 0 {
			break
		}
		value.Set(&q)
		exp++
	}
	return Decimal{value: value, exp: exp}
}

// apply rounds d to the context precision and checks the exponent range.
// sticky reports whether nonzero digits exist beyond the least significant
// digit of d (used by Div for the discarded remainder).
func (c Context) apply(d Decimal, sticky bool) (Decimal, Condition, error) {
	if d.IsZero() {
		return Decimal{}, 0, nil
	}

	var cond Condition
	sign := int64(d.getValue().Sign())
	digits := int64(d.NumDigits())
	value := new(big.Int).Abs(d.getValue())
	exp := int64(d.exp)

	if sticky && digits <= int64(c.Precision) {
		// Nonzero digits beyond the least significant digit of d exist
		// but d itself needs no rounding (only reachable when Div had to
		// clamp its working scale).
		cond |= Rounded | Inexact
	}

	if digits > int64(c.Precision) {
		cond |= Rounded
		shift := digits - int64(c.Precision)
		divisor := new(big.Int).Exp(tenInt, big.NewInt(shift), nil)
		quo, rem := new(big.Int).QuoRem(value, divisor, new(big.Int))
		if rem.Sign() != 0 || sticky {
			cond |= Inexact
			if c.roundsUp(quo, rem, divisor, sign < 0, sticky) {
				quo.Add(quo, oneInt)
				if quo.Cmp(new(big.Int).Exp(tenInt, big.NewInt(int64(c.Precision)), nil)) == 0 {
					// Rounding carried into an extra digit
					// (e.g. 999 -> 1000 at precision 2): renormalize
					// to 10 * 10^(exp+shift+1).
					quo.Quo(quo, tenInt)
					shift++
				}
			}
		}
		value = quo
		exp += shift
		if exp > math.MaxInt32 || exp < math.MinInt32 {
			cond |= Overflow | Inexact
			return Decimal{}, cond, &ConditionError{Condition: cond}
		}
	}

	// Adjusted exponent: exponent of the most significant digit.
	adjusted := exp + int64(coefficientDigits(value)) - 1
	if adjusted > int64(c.MaxExponent) {
		cond |= Overflow | Inexact
		return Decimal{}, cond, &ConditionError{Condition: cond}
	}
	if adjusted < int64(c.MinExponent) {
		cond |= Underflow
	}

	value.Mul(value, big.NewInt(sign))
	result := Decimal{value: value, exp: int32(exp)}
	if cond&c.Traps != 0 {
		return result, cond, &ConditionError{Condition: cond}
	}
	return result, cond, nil
}

// roundsUp decides whether the truncated quotient quo (of |d|) must be
// incremented by one unit, given the dropped remainder rem over divisor.
// neg is the sign of d; sticky reports nonzero digits beyond rem.
func (c Context) roundsUp(quo, rem, divisor *big.Int, neg bool, sticky bool) bool {
	switch c.RoundingMode {
	case RoundDown:
		return false
	case RoundUp:
		return true
	case RoundFloor:
		return neg
	case RoundCeil:
		return !neg
	case RoundHalfEven:
		half := compareTwice(rem, divisor)
		if half > 0 || half == 0 && sticky {
			return true
		}
		return half == 0 && quo.Bit(0) == 1
	default: // RoundHalfUp
		return compareTwice(rem, divisor) >= 0
	}
}

// compareTwice compares 2*rem with divisor.
func compareTwice(rem, divisor *big.Int) int {
	twice := new(big.Int).Lsh(rem, 1)
	return twice.Cmp(divisor)
}

// coefficientDigits returns the number of decimal digits of a positive big.Int.
func coefficientDigits(v *big.Int) int {
	return (Decimal{value: v}).NumDigits()
}
