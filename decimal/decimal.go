// Package decimal provides a fixed-point decimal type for exact monetary
// and base-10 arithmetic in expr expressions.
//
// A Decimal value is represented as coef * 10^exp, where coef is an
// arbitrary precision integer. Addition, subtraction and multiplication
// are always exact. Division is rounded to the precision and rounding
// mode of a Context.
package decimal

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// RoundingMode determines how inexact results are rounded.
type RoundingMode int

const (
	// RoundDown rounds towards zero (truncation).
	RoundDown RoundingMode = iota
	// RoundUp rounds away from zero.
	RoundUp
	// RoundHalfUp rounds to nearest, ties away from zero.
	RoundHalfUp
	// RoundHalfEven rounds to nearest, ties to even (banker's rounding).
	RoundHalfEven
	// RoundCeiling rounds towards positive infinity.
	RoundCeiling
	// RoundFloor rounds towards negative infinity.
	RoundFloor
)

func (m RoundingMode) String() string {
	switch m {
	case RoundDown:
		return "RoundDown"
	case RoundUp:
		return "RoundUp"
	case RoundHalfUp:
		return "RoundHalfUp"
	case RoundHalfEven:
		return "RoundHalfEven"
	case RoundCeiling:
		return "RoundCeiling"
	case RoundFloor:
		return "RoundFloor"
	}
	return fmt.Sprintf("RoundingMode(%d)", int(m))
}

// Context configures the precision (number of fractional decimal places)
// and the rounding mode used by operations that can be inexact, i.e.
// division. Addition, subtraction and multiplication are always exact and
// do not depend on a Context.
type Context struct {
	Precision int32
	Rounding  RoundingMode
}

// DefaultContext is used when no explicit context is configured.
var DefaultContext = Context{Precision: 16, Rounding: RoundHalfUp}

func (c Context) precision() int32 {
	if c.Precision < 0 {
		return 0
	}
	return c.Precision
}

// Decimal is a fixed-point decimal number: coef * 10^exp. The zero value
// is 0. Decimal values are immutable; all operations return new values.
type Decimal struct {
	coef *big.Int
	exp  int32
}

// New returns coef * 10^exp.
func New(coef int64, exp int32) Decimal {
	return Decimal{coef: big.NewInt(coef), exp: exp}
}

// FromInt64 returns x as a Decimal.
func FromInt64(x int64) Decimal {
	return New(x, 0)
}

// FromUint64 returns x as a Decimal.
func FromUint64(x uint64) Decimal {
	return Decimal{coef: new(big.Int).SetUint64(x), exp: 0}
}

// FromFloat64 converts f to a Decimal using the shortest decimal
// representation that round-trips to f (e.g. 0.1 becomes 1/10, not
// 0.1000000000000000055511151231257827). It returns an error for
// NaN and infinities.
func FromFloat64(f float64) (Decimal, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Decimal{}, fmt.Errorf("cannot convert %v to decimal", f)
	}
	return Parse(strconv.FormatFloat(f, 'g', -1, 64))
}

// Convert converts v to a Decimal. Integers and strings are converted
// exactly; floats are converted via FromFloat64. Any other type returns
// an error.
func Convert(v any) (Decimal, error) {
	switch x := v.(type) {
	case Decimal:
		return x, nil
	case int:
		return FromInt64(int64(x)), nil
	case int8:
		return FromInt64(int64(x)), nil
	case int16:
		return FromInt64(int64(x)), nil
	case int32:
		return FromInt64(int64(x)), nil
	case int64:
		return FromInt64(x), nil
	case uint:
		return FromUint64(uint64(x)), nil
	case uint8:
		return FromUint64(uint64(x)), nil
	case uint16:
		return FromUint64(uint64(x)), nil
	case uint32:
		return FromUint64(uint64(x)), nil
	case uint64:
		return FromUint64(x), nil
	case float32:
		return FromFloat64(float64(x))
	case float64:
		return FromFloat64(x)
	case string:
		return Parse(x)
	}
	return Decimal{}, fmt.Errorf("cannot convert %T to decimal", v)
}

// MustParse parses s and panics on error. It is useful for tests and
// package-level variables.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Parse parses a decimal literal: an optional sign, digits with an
// optional fractional part, and an optional exponent (e.g. "12.34e-2").
func Parse(s string) (Decimal, error) {
	orig := s
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}

	exp := int64(0)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return Decimal{}, fmt.Errorf("invalid decimal literal %q", orig)
		}
		exp = e
		s = s[:i]
	}

	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		fracPart = s[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return Decimal{}, fmt.Errorf("invalid decimal literal %q", orig)
	}
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return Decimal{}, fmt.Errorf("invalid decimal literal %q", orig)
		}
	}
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return Decimal{}, fmt.Errorf("invalid decimal literal %q", orig)
		}
	}

	digits := intPart + fracPart
	if digits == "" {
		digits = "0"
	}
	coef, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal literal %q", orig)
	}
	if neg {
		coef.Neg(coef)
	}
	e := exp - int64(len(fracPart))
	if e > math.MaxInt32 || e < math.MinInt32 {
		return Decimal{}, fmt.Errorf("decimal literal %q exponent out of range", orig)
	}
	return Decimal{coef: coef, exp: int32(e)}, nil
}

func (d Decimal) coefficient() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return d.coef
}

var pow10cache = [20]*big.Int{}

func init() {
	for i := range pow10cache {
		pow10cache[i] = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(i)), nil)
	}
}

func pow10(n int64) *big.Int {
	if n >= 0 && n < int64(len(pow10cache)) {
		return pow10cache[n]
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil)
}

// Sign returns -1, 0 or +1 depending on the sign of d.
func (d Decimal) Sign() int {
	return d.coefficient().Sign()
}

// IsZero reports whether d == 0.
func (d Decimal) IsZero() bool {
	return d.coefficient().Sign() == 0
}

// Scale returns the number of fractional decimal places of d.
func (d Decimal) Scale() int32 {
	return -d.exp
}

// Neg returns -d.
func (d Decimal) Neg() Decimal {
	return Decimal{coef: new(big.Int).Neg(d.coefficient()), exp: d.exp}
}

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	return Decimal{coef: new(big.Int).Abs(d.coefficient()), exp: d.exp}
}

// Add returns d + o. The result is exact.
func (d Decimal) Add(o Decimal) Decimal {
	dc, oc := d.coefficient(), o.coefficient()
	switch {
	case d.exp == o.exp:
		return Decimal{coef: new(big.Int).Add(dc, oc), exp: d.exp}
	case d.exp > o.exp:
		scaled := new(big.Int).Mul(dc, pow10(int64(d.exp-o.exp)))
		return Decimal{coef: scaled.Add(scaled, oc), exp: o.exp}
	default:
		scaled := new(big.Int).Mul(oc, pow10(int64(o.exp-d.exp)))
		return Decimal{coef: scaled.Add(scaled, dc), exp: d.exp}
	}
}

// Sub returns d - o. The result is exact.
func (d Decimal) Sub(o Decimal) Decimal {
	return d.Add(o.Neg())
}

// Mul returns d * o. The result is exact.
func (d Decimal) Mul(o Decimal) Decimal {
	return Decimal{
		coef: new(big.Int).Mul(d.coefficient(), o.coefficient()),
		exp:  d.exp + o.exp,
	}
}

// Quo returns d / o rounded to ctx.Precision fractional decimal places
// using ctx.Rounding. It panics if o is zero.
func (d Decimal) Quo(o Decimal, ctx Context) Decimal {
	oc := o.coefficient()
	if oc.Sign() == 0 {
		panic("decimal: division by zero")
	}
	p := ctx.precision()
	// d/o = (dc/oc) * 10^(d.exp-o.exp); the result has exp = -p.
	e := int64(d.exp) - int64(o.exp) + int64(p)
	num := d.coefficient()
	den := oc
	if e >= 0 {
		num = new(big.Int).Mul(num, pow10(e))
	} else {
		den = new(big.Int).Mul(den, pow10(-e))
	}
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	return Decimal{coef: roundQuoRem(quo, rem, den, ctx.Rounding), exp: -p}
}

// Cmp compares d and o and returns -1, 0 or +1. Decimals with different
// scales compare by value: 1.0 == 1.00.
func (d Decimal) Cmp(o Decimal) int {
	dc, oc := d.coefficient(), o.coefficient()
	ds, os := dc.Sign(), oc.Sign()
	if ds != os {
		switch {
		case ds < os:
			return -1
		default:
			return 1
		}
	}
	switch e := int64(d.exp) - int64(o.exp); {
	case e == 0:
		return dc.Cmp(oc)
	case e > 0:
		return new(big.Int).Mul(dc, pow10(e)).Cmp(oc)
	default:
		return dc.Cmp(new(big.Int).Mul(oc, pow10(-e)))
	}
}

// Round returns d rounded to the given number of fractional decimal
// places using the given rounding mode.
func (d Decimal) Round(places int32, mode RoundingMode) Decimal {
	if places < 0 {
		places = 0
	}
	if d.exp >= -places {
		return d
	}
	shift := int64(-d.exp) - int64(places)
	divisor := pow10(shift)
	quo, rem := new(big.Int).QuoRem(d.coefficient(), divisor, new(big.Int))
	return Decimal{coef: roundQuoRem(quo, rem, divisor, mode), exp: -places}
}

// roundQuoRem adjusts a truncated quotient quo (with remainder rem over
// divisor den) according to the rounding mode. rem must be the remainder
// of a truncated (towards zero) division.
func roundQuoRem(quo, rem, den *big.Int, mode RoundingMode) *big.Int {
	if rem.Sign() == 0 {
		return quo
	}
	negative := rem.Sign()*den.Sign() < 0
	absRem2 := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	absDen := new(big.Int).Abs(den)
	cmp := absRem2.Cmp(absDen)

	increment := false
	switch mode {
	case RoundDown:
		increment = false
	case RoundUp:
		increment = true
	case RoundHalfUp:
		increment = cmp >= 0
	case RoundHalfEven:
		increment = cmp > 0 || cmp == 0 && quo.Bit(0) == 1
	case RoundCeiling:
		increment = !negative
	case RoundFloor:
		increment = negative
	}
	if !increment {
		return quo
	}
	if negative {
		return quo.Sub(quo, big.NewInt(1))
	}
	return quo.Add(quo, big.NewInt(1))
}

// Int64 returns d truncated towards zero as an int64.
func (d Decimal) Int64() int64 {
	coef := d.coefficient()
	if d.exp >= 0 {
		return new(big.Int).Mul(coef, pow10(int64(d.exp))).Int64()
	}
	return new(big.Int).Quo(coef, pow10(int64(-d.exp))).Int64()
}

// Float64 returns the closest float64 to d.
func (d Decimal) Float64() float64 {
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// String returns the decimal representation of d without exponent
// notation. The scale is preserved: 1.50 formats as "1.50".
func (d Decimal) String() string {
	coef := d.coefficient()
	neg := coef.Sign() < 0
	digits := new(big.Int).Abs(coef).String()

	var s string
	switch {
	case d.exp >= 0:
		s = digits + strings.Repeat("0", int(d.exp))
	default:
		point := len(digits) + int(d.exp)
		switch {
		case point > 0:
			s = digits[:point] + "." + digits[point:]
		default:
			s = "0." + strings.Repeat("0", -point) + digits
		}
	}
	if neg {
		return "-" + s
	}
	return s
}
