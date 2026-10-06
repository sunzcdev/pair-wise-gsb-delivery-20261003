// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"fmt"
	"math/big"
)

// tagNumDecimalFraction is the CBOR tag number for decimal fractions (RFC 8949 §3.4.4).
const tagNumDecimalFraction uint64 = 4

// tagNumBigFloat is the CBOR tag number for bigfloats (RFC 8949 §3.4.4).
const tagNumBigFloat uint64 = 5

// MaxExactExponent bounds the absolute exponent allowed when converting a
// DecimalFraction or BigFloat to math/big types. Computing base^exponent for
// an attacker-controlled exponent would otherwise allocate unbounded memory,
// so conversions reject exponents outside [-MaxExactExponent, MaxExactExponent]
// with ErrExponentOutOfRange instead of attempting the computation.
const MaxExactExponent = 100000

// ErrNotExactlyRepresentable is returned when a value cannot be represented
// exactly in the requested form, e.g. converting 1/3 to a DecimalFraction
// (denominator has prime factors other than 2 and 5) or to a BigFloat
// (denominator is not a power of 2), or rounding a value to a big.Float
// with insufficient precision. The conversion fails rather than silently
// rounding or looping forever on a non-terminating expansion.
var ErrNotExactlyRepresentable = errors.New("cbor: value cannot be represented exactly in the requested form")

// ErrExponentOutOfRange is returned when an exponent's absolute value exceeds
// MaxExactExponent during conversion to math/big types.
var ErrExponentOutOfRange = errors.New("cbor: exponent out of range for exact representation")

// DecimalFraction exactly represents a CBOR decimal fraction (tag 4) as
// Mantissa * 10^Exponent, using arbitrary-precision integers so no precision
// is lost through float64. The decoded exponent and mantissa are preserved
// as-is; use Normalized to obtain the canonical form with no trailing
// decimal zeros in the mantissa.
type DecimalFraction struct {
	Exponent *big.Int
	Mantissa *big.Int
}

// NewDecimalFraction returns a DecimalFraction representing mantissa * 10^exponent.
func NewDecimalFraction(mantissa *big.Int, exponent int64) DecimalFraction {
	return DecimalFraction{
		Exponent: big.NewInt(exponent),
		Mantissa: new(big.Int).Set(mantissa),
	}
}

// DecimalFractionFromBigRat converts r to a DecimalFraction. It returns an
// error wrapping ErrNotExactlyRepresentable if r's denominator has prime
// factors other than 2 and 5 (e.g. 1/3), since such values have no finite
// decimal representation.
func DecimalFractionFromBigRat(r *big.Rat) (DecimalFraction, error) {
	num := r.Num()
	den := new(big.Int).Set(r.Denom())

	// Factor den into 2^a * 5^b * rest; rest must be 1.
	a := removeSmallFactor(den, 2)
	b := removeSmallFactor(den, 5)
	if den.Cmp(bigOne) != 0 {
		return DecimalFraction{}, fmt.Errorf("%w: denominator has prime factors other than 2 and 5", ErrNotExactlyRepresentable)
	}

	// r = num / (2^a * 5^b) = num * 2^(e-a) * 5^(e-b) * 10^-e with e = max(a, b).
	e := max(a, b)
	m := new(big.Int).Set(num)
	if d := e - a; d > 0 {
		m.Mul(m, new(big.Int).Lsh(bigOne, d))
	}
	if d := e - b; d > 0 {
		m.Mul(m, new(big.Int).Exp(bigFive, big.NewInt(int64(d)), nil))
	}
	return DecimalFraction{
		Exponent: big.NewInt(-int64(e)),
		Mantissa: m,
	}, nil
}

// BigRat returns the exact value of df as a big.Rat. It returns
// ErrExponentOutOfRange if the exponent's absolute value exceeds
// MaxExactExponent, and an error if Exponent or Mantissa is nil.
func (df DecimalFraction) BigRat() (*big.Rat, error) {
	e, err := checkedExponent(df.Exponent, df.Mantissa)
	if err != nil {
		return nil, err
	}
	m := new(big.Int).Set(df.Mantissa)
	if e >= 0 {
		p := new(big.Int).Exp(bigTen, big.NewInt(e), nil)
		return new(big.Rat).SetInt(m.Mul(m, p)), nil
	}
	p := new(big.Int).Exp(bigTen, big.NewInt(-e), nil)
	return new(big.Rat).SetFrac(m, p), nil
}

// BigFloat returns the exact value of df as a big.Float with the given
// precision. It returns an error wrapping ErrNotExactlyRepresentable if prec
// bits are not enough to represent the value exactly, rather than silently
// rounding.
func (df DecimalFraction) BigFloat(prec uint) (*big.Float, error) {
	r, err := df.BigRat()
	if err != nil {
		return nil, err
	}
	return exactBigFloatFromRat(r, prec)
}

// Normalized returns the canonical form of df: the mantissa is divided by 10
// as often as possible and the exponent is adjusted accordingly, so the
// result has no trailing decimal zeros in the mantissa. df is not modified.
func (df DecimalFraction) Normalized() DecimalFraction {
	if df.Exponent == nil || df.Mantissa == nil || df.Mantissa.Sign() == 0 {
		return df
	}
	m := new(big.Int).Set(df.Mantissa)
	e := new(big.Int).Set(df.Exponent)
	q, r := new(big.Int), new(big.Int)
	for {
		q.QuoRem(m, bigTen, r)
		if r.Sign() != 0 {
			break
		}
		m.Set(q)
		e.Add(e, bigOne)
	}
	return DecimalFraction{Exponent: e, Mantissa: m}
}

// String returns df in the form "mantissa e exponent" (e.g. "27315e-2"),
// preserving the stored exponent rather than normalizing.
func (df DecimalFraction) String() string {
	if df.Exponent == nil || df.Mantissa == nil {
		return "DecimalFraction(nil)"
	}
	if df.Exponent.Sign() == 0 {
		return df.Mantissa.String()
	}
	return df.Mantissa.String() + "e" + df.Exponent.String()
}

// MarshalCBOR encodes df as CBOR tag 4 with a two-element array content
// [exponent, mantissa]. Exponent and mantissa encode as regular CBOR
// integers when they fit, or as bignums (tag 2 or 3) otherwise.
func (df DecimalFraction) MarshalCBOR() ([]byte, error) {
	if df.Exponent == nil || df.Mantissa == nil {
		return nil, errors.New("cbor: cannot encode DecimalFraction with nil Exponent or Mantissa")
	}
	return Marshal(Tag{Number: tagNumDecimalFraction, Content: [2]*big.Int{df.Exponent, df.Mantissa}})
}

// UnmarshalCBOR decodes a CBOR tag 4 data item into df. The content must be
// a two-element array [exponent, mantissa] of integers (regular integers or
// bignums). The exponent and mantissa are stored exactly as encoded, without
// normalization.
func (df *DecimalFraction) UnmarshalCBOR(data []byte) error {
	e, m, err := unmarshalExponentMantissa(data, tagNumDecimalFraction)
	if err != nil {
		return err
	}
	df.Exponent = e
	df.Mantissa = m
	return nil
}

var (
	bigOne  = big.NewInt(1)
	bigFive = big.NewInt(5)
	bigTen  = big.NewInt(10)
)

// checkedExponent validates that exponent and mantissa are non-nil and that
// the exponent fits the range allowed for exact conversion, returning the
// exponent as an int64.
func checkedExponent(exponent, mantissa *big.Int) (int64, error) {
	if exponent == nil || mantissa == nil {
		return 0, errors.New("cbor: nil Exponent or Mantissa")
	}
	if !exponent.IsInt64() {
		return 0, fmt.Errorf("%w: exponent %s does not fit int64", ErrExponentOutOfRange, exponent)
	}
	e := exponent.Int64()
	if e > MaxExactExponent || e < -MaxExactExponent {
		return 0, fmt.Errorf("%w: exponent %d exceeds limit %d", ErrExponentOutOfRange, e, MaxExactExponent)
	}
	return e, nil
}

// exactBigFloatFromRat converts r to a big.Float with the given precision,
// failing with ErrNotExactlyRepresentable if the value would be rounded.
func exactBigFloatFromRat(r *big.Rat, prec uint) (*big.Float, error) {
	if prec == 0 {
		return nil, errors.New("cbor: big.Float precision must be at least 1")
	}
	f := new(big.Float).SetPrec(prec).SetMode(big.ToNearestEven).SetRat(r)
	if f.Acc() != big.Exact {
		return nil, fmt.Errorf("%w: %s needs more than %d bits of precision", ErrNotExactlyRepresentable, r.RatString(), prec)
	}
	return f, nil
}

// removeSmallFactor divides x by base as often as possible and returns the
// number of divisions. x is modified in place.
func removeSmallFactor(x *big.Int, base int64) uint {
	if x.Sign() == 0 {
		return 0
	}
	b := big.NewInt(base)
	q, r := new(big.Int), new(big.Int)
	var n uint
	for {
		q.QuoRem(x, b, r)
		if r.Sign() != 0 {
			return n
		}
		x.Set(q)
		n++
	}
}

// unmarshalExponentMantissa decodes a tagged two-element array
// [exponent, mantissa] of integers for the given tag number.
func unmarshalExponentMantissa(data []byte, tagNum uint64) (exponent, mantissa *big.Int, err error) {
	var t RawTag
	if err := t.UnmarshalCBOR(data); err != nil {
		return nil, nil, err
	}
	if t.Number != tagNum {
		return nil, nil, fmt.Errorf("cbor: cannot unmarshal CBOR tag %d, expected tag %d", t.Number, tagNum)
	}
	var raw []RawMessage
	if err := Unmarshal(t.Content, &raw); err != nil {
		return nil, nil, fmt.Errorf("cbor: invalid tag %d content: %w", tagNum, err)
	}
	if len(raw) != 2 {
		return nil, nil, fmt.Errorf("cbor: tag %d content must be an array of 2 elements, got %d", tagNum, len(raw))
	}
	exponent, err = decodeContentBigInt(raw[0])
	if err != nil {
		return nil, nil, fmt.Errorf("cbor: tag %d exponent: %w", tagNum, err)
	}
	mantissa, err = decodeContentBigInt(raw[1])
	if err != nil {
		return nil, nil, fmt.Errorf("cbor: tag %d mantissa: %w", tagNum, err)
	}
	return exponent, mantissa, nil
}

// decodeContentBigInt decodes a CBOR integer (major type 0 or 1, or bignum
// tag 2 or 3) to a big.Int.
func decodeContentBigInt(data []byte) (*big.Int, error) {
	var v any
	if err := Unmarshal(data, &v); err != nil {
		return nil, err
	}
	switch n := v.(type) {
	case uint64:
		return new(big.Int).SetUint64(n), nil
	case int64:
		return big.NewInt(n), nil
	case big.Int:
		return new(big.Int).Set(&n), nil
	default:
		return nil, fmt.Errorf("expected integer, got %T", v)
	}
}
