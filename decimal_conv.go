package cbor

import (
	"errors"
	"math/big"
)

var (
	errNilRat   = errors.New("cbor: cannot convert nil *big.Rat")
	errNilFloat = errors.New("cbor: cannot convert nil *big.Float")
)

// RatToDecimal converts r to an exact Decimal m * 10**e. It returns
// ErrNotExact when r has no finite base-10 expansion (for example 1/3, whose
// reduced denominator contains a prime factor other than 2 or 5). The result
// is canonical: no factor of 10 remains in the coefficient.
//
// A nil limits argument applies DefaultLimits.
func RatToDecimal(r *big.Rat, l *NumericLimits) (Decimal, error) {
	e, m, err := ratToScaled(r, tenInt, fiveInt, l)
	if err != nil {
		return Decimal{}, err
	}
	return Decimal{Exponent: e, Mantissa: m}, nil
}

// RatToBigFloat converts r to an exact BigFloat m * 2**e. It returns
// ErrNotExact when r has no finite base-2 expansion (for example 1/3, whose
// reduced denominator is not a power of two). The result is canonical.
//
// A nil limits argument applies DefaultLimits.
func RatToBigFloat(r *big.Rat, l *NumericLimits) (BigFloat, error) {
	e, m, err := ratToScaled(r, twoInt, nil, l)
	if err != nil {
		return BigFloat{}, err
	}
	return BigFloat{Exponent: e, Mantissa: m}, nil
}

// ratToScaled reduces r to an integer coefficient and exponent for the given
// base. denomExtra is the other prime factor of base besides 2 (5 for base
// 10, nil for base 2). For denominator 2**a * p**b the exponent starts at
// -max(a, b) after the numerator is multiplied by the missing factors, then
// the result is canonicalized.
func ratToScaled(r *big.Rat, base, denomExtra *big.Int, l *NumericLimits) (int64, *big.Int, error) {
	if r == nil {
		return 0, nil, errNilRat
	}
	lim := DefaultLimits
	if l != nil {
		lim = l.orDefault()
	}

	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom()) // den > 0, coprime with num

	if num.Sign() == 0 {
		return 0, new(big.Int), nil
	}

	a := removeFactor(den, twoInt)
	b := int64(0)
	if denomExtra != nil {
		b = removeFactor(den, denomExtra)
	}
	if den.Cmp(big.NewInt(1)) != 0 {
		return 0, nil, ErrNotExact
	}

	k := a
	if b > k {
		k = b
	}
	if err := lim.checkExponent(-k); err != nil {
		return 0, nil, err
	}

	// Bound the intermediate coefficient before multiplying: each missing
	// factor contributes at most 3 bits (log2(5) < 3).
	multBits := (k - a) + 3*(k-b)
	if num.BitLen()+int(multBits) > lim.MaxExpandedBits {
		return 0, nil, ErrExpandedValueTooLarge
	}

	m := new(big.Int).Set(num)
	if denomExtra != nil {
		if k-a > 0 {
			m.Mul(m, new(big.Int).Exp(twoInt, big.NewInt(k-a), nil))
		}
		if k-b > 0 {
			m.Mul(m, new(big.Int).Exp(fiveInt, big.NewInt(k-b), nil))
		}
	}

	e := -k
	rem := new(big.Int)
	for rem.Mod(m, base); rem.Sign() == 0; rem.Mod(m, base) {
		m.Quo(m, base)
		e++
	}

	if err := lim.checkExponent(e); err != nil {
		return 0, nil, err
	}
	if m.BitLen() > lim.MaxMantissaBits {
		return 0, nil, ErrMantissaTooLarge
	}
	if expandedBitLen(e, m, base) > lim.MaxExpandedBits {
		return 0, nil, ErrExpandedValueTooLarge
	}
	return e, m, nil
}

// removeFactor divides n by f as many times as possible and returns the
// number of divisions. n must be positive.
func removeFactor(n, f *big.Int) int64 {
	zero := new(big.Int)
	rem := new(big.Int)
	var count int64
	for rem.Mod(n, f); rem.Cmp(zero) == 0; rem.Mod(n, f) {
		n.Quo(n, f)
		count++
	}
	return count
}

// ToBigFloat converts a decimal fraction to an exact BigFloat (m * 2**e). It
// returns ErrNotExact when the decimal cannot be written finitely in base 2,
// which happens exactly when its reduced coefficient carries a factor of 5
// (for example 0.1).
func (x Decimal) ToBigFloat(l *NumericLimits) (BigFloat, error) {
	r, err := x.Rat(l)
	if err != nil {
		return BigFloat{}, err
	}
	return RatToBigFloat(r, l)
}

// ToDecimal converts a bigfloat to an exact Decimal. Every finite base-2
// fraction is also a finite base-10 fraction (2 divides 10), so this
// conversion never returns ErrNotExact.
func (x BigFloat) ToDecimal(l *NumericLimits) (Decimal, error) {
	r, err := x.Rat(l)
	if err != nil {
		return Decimal{}, err
	}
	return RatToDecimal(r, l)
}

// ExactFloat returns the value as a *big.Float with exactly enough precision
// to represent it. It returns ErrNotExact when the value has no finite binary
// representation (for a Decimal this happens when a factor of 5 survives
// reduction, such as 0.1). Rounding is never performed.
func (x Decimal) ExactFloat(l *NumericLimits) (*big.Float, error) {
	r, err := x.Rat(l)
	if err != nil {
		return nil, err
	}
	return ratToExactFloat(r, l)
}

// ExactFloat returns the value as a *big.Float with exactly enough precision.
// A BigFloat is always a finite binary floating-point number, so this only
// fails on the resource limits.
func (x BigFloat) ExactFloat(l *NumericLimits) (*big.Float, error) {
	r, err := x.Rat(l)
	if err != nil {
		return nil, err
	}
	return ratToExactFloat(r, l)
}

func ratToExactFloat(r *big.Rat, l *NumericLimits) (*big.Float, error) {
	lim := DefaultLimits
	if l != nil {
		lim = l.orDefault()
	}
	if r.Num().Sign() == 0 {
		return new(big.Float).SetPrec(64).SetInt64(0), nil
	}
	// The denominator must be a power of two; a is that exponent.
	denResidual := new(big.Int).Set(r.Denom())
	a := removeFactor(denResidual, twoInt)
	if denResidual.Cmp(big.NewInt(1)) != 0 {
		return nil, ErrNotExact
	}
	// The exponent magnitude is part of the materialized value.
	if err := lim.checkExponent(-a); err != nil {
		return nil, err
	}
	prec := uint(r.Num().BitLen())
	if int(prec) > lim.MaxExpandedBits {
		return nil, ErrExpandedValueTooLarge
	}
	// num / 2**a: strip trailing zero bits so num == K * 2**t with K odd, then
	// the value is K * 2**(t-a). SetMantExp with a fresh mantissa reproduces
	// it exactly at precision == bitlen(num).
	num := new(big.Int).Set(r.Num())
	t := int64(0)
	for num.Bit(0) == 0 {
		num.Rsh(num, 1)
		t++
	}
	f := new(big.Float).SetPrec(prec)
	mant := new(big.Float).SetPrec(prec).SetInt(num)
	f.SetMantExp(mant, int(t-a))
	return f, nil
}

// FloatToDecimal converts f to an exact Decimal. It returns ErrNotExact only
// for non-finite f (±Inf or NaN); every finite *big.Float is a finite base-2
// and hence base-10 fraction. The result is canonical.
func FloatToDecimal(f *big.Float, l *NumericLimits) (Decimal, error) {
	r, err := floatToRat(f)
	if err != nil {
		return Decimal{}, err
	}
	return RatToDecimal(r, l)
}

// FloatToBigFloat converts f to an exact BigFloat. It returns ErrNotExact for
// non-finite f (±Inf or NaN). The result is canonical.
func FloatToBigFloat(f *big.Float, l *NumericLimits) (BigFloat, error) {
	r, err := floatToRat(f)
	if err != nil {
		return BigFloat{}, err
	}
	return RatToBigFloat(r, l)
}

// floatToRat converts a finite *big.Float to an exact rational. A big.Float
// stores sign * M * 2**exp where M is an integer of at most prec bits, so the
// decomposition below is exact and avoids any decimal-text round trip.
func floatToRat(f *big.Float) (*big.Rat, error) {
	if f == nil {
		return nil, errNilFloat
	}
	if f.IsInf() {
		return nil, ErrNotExact
	}
	if f.Sign() == 0 {
		return new(big.Rat), nil
	}

	// f == frac * 2**exp with 0.5 <= frac < 1. Multiplying frac by 2**prec
	// yields the integer mantissa M, hence f == M * 2**(exp - prec).
	prec := f.Prec()
	frac := new(big.Float).SetPrec(prec)
	exp := f.MantExp(frac)
	scaled := new(big.Float).SetPrec(prec+1).SetMantExp(frac, int(prec))
	m, acc := scaled.Int(nil)
	if acc != big.Exact {
		return nil, ErrNotExact
	}

	// Build M * 2**(exp-prec) exactly using integer shifts.
	q := int64(exp) - int64(prec) // f == M * 2**q
	r := new(big.Rat)
	switch {
	case q >= 0:
		r.SetInt(new(big.Int).Lsh(m, uint(q)))
	case int64(prec)+q >= 0:
		// Multiply numerator up instead of using an over-wide denominator:
		// M * 2**q == (M * 2**(prec+q)) / 2**prec.
		num := new(big.Int).Lsh(m, uint(int64(prec)+q))
		r.SetFrac(num, new(big.Int).Lsh(big.NewInt(1), uint(prec)))
	default:
		r.SetFrac(m, new(big.Int).Lsh(big.NewInt(1), uint(-q)))
	}
	return r, nil
}
