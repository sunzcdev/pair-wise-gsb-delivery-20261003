package cbor

import (
	"errors"
	"math/big"
)

// CBOR tags for exact base-10 and base-2 scaled numbers, defined by RFC 8949
// section 3.4.4. Their tag content is a two-element array [e, m] where e is an
// integer exponent and m is an integer or bignum coefficient. The represented
// value is m * 10**e for Decimal and m * 2**e for BigFloat.
//
// Unlike decoding through float64, these types preserve the exact coefficient
// and exponent, so financial and scientific values never acquire rounding
// errors from an intermediate binary float.
const (
	// TagDecimalFraction is CBOR tag number 4.
	TagDecimalFraction = tagNumDecimalFraction
	// TagBigFloat is CBOR tag number 5.
	TagBigFloat = tagNumBigFloat
)

var (
	// ErrNotExact is returned when a conversion cannot be performed without
	// losing precision, for example when converting 1/3 into a Decimal (which
	// requires a finite base-10 expansion) or into a BigFloat (which requires
	// a finite base-2 expansion). Exact conversion functions never round
	// silently or loop forever: the caller decides whether rounding is
	// acceptable.
	ErrNotExact = errors.New("cbor: value cannot be represented exactly")

	// ErrExponentTooLarge is returned when a CBOR tag 4 or 5 data item carries
	// an exponent whose magnitude exceeds the configured limit. Enforcing the
	// limit before the exponent is expanded prevents a malicious message such
	// as [2**63, 1] from forcing gigabytes of allocation.
	ErrExponentTooLarge = errors.New("cbor: decimal exponent exceeds configured limit")

	// ErrMantissaTooLarge is returned when the coefficient of a tag 4 or 5
	// data item uses more bits than the configured limit.
	ErrMantissaTooLarge = errors.New("cbor: decimal mantissa exceeds configured limit")

	// ErrExpandedValueTooLarge is returned when materializing a scaled number
	// (for example as a *big.Rat, *big.Int or *big.Float) would require more
	// bits than the configured limit. This guards against values like
	// 1 * 10**100000000, which are cheap to express but expensive to expand.
	ErrExpandedValueTooLarge = errors.New("cbor: expanded decimal value exceeds configured limit")
)

// DefaultLimits bounds resources used when decoding and materializing tag 4
// and 5 values. The numbers are generous for financial and scientific data
// while keeping a single hostile message well under a megabyte of mandatory
// allocation. Methods use these limits when a nil *NumericLimits is supplied.
var DefaultLimits = NumericLimits{
	MaxStoredExponent: 1 << 40,
	MaxExponent:       100_000,
	MaxMantissaBits:   1_000_000,
	MaxExpandedBits:   4_000_000,
}

// NumericLimits caps the resources that exact number conversions may consume.
// The zero value of every field means "use DefaultLimits", so constructors and
// conversion methods stay safe without extra configuration.
type NumericLimits struct {
	// MaxStoredExponent bounds |exponent| of values accepted directly from
	// CBOR. Its job is to reject absurd exponents (such as 2**63) up front;
	// the value is not expanded here, so it may be larger than MaxExponent.
	MaxStoredExponent int64

	// MaxExponent limits |exponent| of Decimal and BigFloat values.
	MaxExponent int64

	// MaxMantissaBits limits the bit length of the integer coefficient m.
	MaxMantissaBits int

	// MaxExpandedBits limits the bit length of values that must be written out
	// without an implicit scale: the numerator and denominator produced by
	// Rat, the integer produced by Int, and the mantissa precision produced
	// by ExactFloat.
	MaxExpandedBits int
}

func (l NumericLimits) orDefault() NumericLimits {
	d := DefaultLimits
	if l.MaxStoredExponent > 0 {
		d.MaxStoredExponent = l.MaxStoredExponent
	}
	if l.MaxExponent > 0 {
		d.MaxExponent = l.MaxExponent
	}
	if l.MaxMantissaBits > 0 {
		d.MaxMantissaBits = l.MaxMantissaBits
	}
	if l.MaxExpandedBits > 0 {
		d.MaxExpandedBits = l.MaxExpandedBits
	}
	return d
}

func (l NumericLimits) checkStoredExponent(e int64) error {
	if e < 0 {
		if e == -1<<63 {
			return ErrExponentTooLarge
		}
		e = -e
	}
	if e > l.MaxStoredExponent {
		return ErrExponentTooLarge
	}
	return nil
}

func (l NumericLimits) checkExponent(e int64) error {
	if e < 0 {
		if e == -1<<63 {
			return ErrExponentTooLarge
		}
		e = -e
	}
	if e > l.MaxExponent {
		return ErrExponentTooLarge
	}
	return nil
}

// Decimal is the exact Go representation of a CBOR decimal fraction (tag 4):
// the value Mantissa * 10**Exponent. The exponent is stored verbatim, so the
// distinction between encodings such as [0, 1] ("1") and [-2, 100] ("1.00")
// is preserved. Use Canonical to normalize such equivalent representations.
//
// The zero Decimal is 0 but is not produced by decoding; constructors and the
// decoder always leave Mantissa non-nil.
type Decimal struct {
	Exponent int64
	Mantissa *big.Int
}

// NewDecimal returns m * 10**e without changing the exponent. The coefficient
// is copied so later mutation of m cannot affect the result. A nil m is
// treated as 0.
func NewDecimal(e int64, m *big.Int) Decimal {
	return Decimal{Exponent: e, Mantissa: copyInt(m)}
}

// BigFloat is the exact Go representation of a CBOR bigfloat (tag 5):
// the value Mantissa * 2**Exponent. As with Decimal the original exponent is
// retained; use Canonical for the normalized form.
type BigFloat struct {
	Exponent int64
	Mantissa *big.Int
}

// NewBigFloat returns m * 2**e without changing the exponent. The coefficient
// is copied so later mutation of m cannot affect the result. A nil m is
// treated as 0.
func NewBigFloat(e int64, m *big.Int) BigFloat {
	return BigFloat{Exponent: e, Mantissa: copyInt(m)}
}

func copyInt(m *big.Int) *big.Int {
	if m == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(m)
}

var (
	twoInt  = big.NewInt(2)
	fiveInt = big.NewInt(5)
	tenInt  = big.NewInt(10)
)

// IsCanonical reports whether the representation is normalized: zero has
// exponent 0, and a non-zero coefficient is not divisible by 10.
func (x Decimal) IsCanonical() bool {
	return isCanonical(x.Mantissa, x.Exponent, tenInt)
}

// IsCanonical reports whether the representation is normalized: zero has
// exponent 0, and a non-zero coefficient is odd.
func (x BigFloat) IsCanonical() bool {
	return isCanonical(x.Mantissa, x.Exponent, twoInt)
}

func isCanonical(m *big.Int, e int64, base *big.Int) bool {
	if m == nil || m.Sign() == 0 {
		return e == 0
	}
	return new(big.Int).Mod(m, base).Sign() != 0
}

// Canonical returns a copy with trailing factors of 10 removed from the
// coefficient and folded into the exponent. The numeric value is unchanged;
// only the representation is normalized. A zero value becomes {0, 0}.
func (x Decimal) Canonical() Decimal {
	e, m := canonicalCopy(x.Exponent, x.Mantissa, tenInt)
	return Decimal{Exponent: e, Mantissa: m}
}

// Canonical returns a copy with trailing factors of 2 removed from the
// coefficient and folded into the exponent. A zero value becomes {0, 0}.
func (x BigFloat) Canonical() BigFloat {
	e, m := canonicalCopy(x.Exponent, x.Mantissa, twoInt)
	return BigFloat{Exponent: e, Mantissa: m}
}

func canonicalCopy(e int64, m *big.Int, base *big.Int) (int64, *big.Int) {
	c := copyInt(m)
	if c.Sign() == 0 {
		return 0, c
	}
	zero := new(big.Int)
	rem := new(big.Int)
	for rem.Mod(c, base); rem.Cmp(zero) == 0; rem.Mod(c, base) {
		c.Quo(c, base)
		e++
	}
	return e, c
}

// Rat returns the exact rational value m * 10**e. ErrExpandedValueTooLarge is
// returned when building the fraction would exceed l.MaxExpandedBits; a nil
// limits argument applies DefaultLimits. The returned *big.Rat is owned by
// the caller.
func (x Decimal) Rat(l *NumericLimits) (*big.Rat, error) {
	return scaledRat(x.Exponent, x.Mantissa, tenInt, l)
}

// Rat returns the exact rational value m * 2**e. See Decimal.Rat for limits.
func (x BigFloat) Rat(l *NumericLimits) (*big.Rat, error) {
	return scaledRat(x.Exponent, x.Mantissa, twoInt, l)
}

func scaledRat(e int64, m *big.Int, base *big.Int, l *NumericLimits) (*big.Rat, error) {
	lim := DefaultLimits
	if l != nil {
		lim = l.orDefault()
	}
	c := copyInt(m)
	if err := lim.checkExponent(e); err != nil {
		return nil, err
	}
	if c.BitLen() > lim.MaxMantissaBits {
		return nil, ErrMantissaTooLarge
	}
	if expandedBitLen(e, c, base) > lim.MaxExpandedBits {
		return nil, ErrExpandedValueTooLarge
	}

	r := new(big.Rat).SetInt(c)
	if e == 0 || c.Sign() == 0 {
		return r, nil
	}
	abs := e
	if abs < 0 {
		abs = -abs
	}
	scale := new(big.Int).Exp(base, big.NewInt(abs), nil)
	if e > 0 {
		r.Mul(r, new(big.Rat).SetInt(scale))
	} else {
		r.Quo(r, new(big.Rat).SetInt(scale))
	}
	return r, nil
}

// expandedBitLen is an allocation-free upper bound on the bit length of the
// numerator and denominator after applying base**e. log2(base) is added per
// exponent step, rounded up so the limit is never underestimated.
func expandedBitLen(e int64, m *big.Int, base *big.Int) int {
	if m.Sign() == 0 {
		return 0
	}
	if e < 0 {
		e = -e
	}
	// bitsPerStep = ceil(log2(base)): 1 for base 2, 4 for base 10.
	bitsPerStep := 1
	if base == tenInt {
		bitsPerStep = 4
	}
	return m.BitLen() + int(e)*bitsPerStep
}

// Int returns the value as a *big.Int when it is an integral multiple of
// 10**0, i.e. when e >= 0. It returns ErrNotExact for fractional values and
// ErrExpandedValueTooLarge when the integer would exceed the limit.
func (x Decimal) Int(l *NumericLimits) (*big.Int, error) {
	return scaledInt(x.Exponent, x.Mantissa, tenInt, l)
}

// Int returns the value as a *big.Int when it is an integral multiple of
// 2**0, i.e. when e >= 0. See Decimal.Int for errors.
func (x BigFloat) Int(l *NumericLimits) (*big.Int, error) {
	return scaledInt(x.Exponent, x.Mantissa, twoInt, l)
}

func scaledInt(e int64, m *big.Int, base *big.Int, l *NumericLimits) (*big.Int, error) {
	r, err := scaledRat(e, m, base, l)
	if err != nil {
		return nil, err
	}
	if !r.IsInt() {
		return nil, ErrNotExact
	}
	return r.Num(), nil
}
