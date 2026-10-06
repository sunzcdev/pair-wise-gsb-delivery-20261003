// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// BigFloat exactly represents a CBOR bigfloat (tag 5) as
// Mantissa * 2^Exponent, using arbitrary-precision integers so no precision
// is lost through float64. The decoded exponent and mantissa are preserved
// as-is; use Normalized to obtain the canonical form with no trailing
// binary zeros in the mantissa.
type BigFloat struct {
	Exponent *big.Int
	Mantissa *big.Int
}

// NewBigFloat returns a BigFloat representing mantissa * 2^exponent.
func NewBigFloat(mantissa *big.Int, exponent int64) BigFloat {
	return BigFloat{
		Exponent: big.NewInt(exponent),
		Mantissa: new(big.Int).Set(mantissa),
	}
}

// BigFloatFromBigRat converts r to a BigFloat. It returns an error wrapping
// ErrNotExactlyRepresentable if r's denominator is not a power of 2
// (e.g. 1/3 or 1/10), since such values have no finite binary representation.
func BigFloatFromBigRat(r *big.Rat) (BigFloat, error) {
	den := r.Denom()
	tz := den.TrailingZeroBits()
	if tz > math.MaxInt64 {
		return BigFloat{}, fmt.Errorf("%w: denominator 2^%d overflows int64 exponent", ErrExponentOutOfRange, tz)
	}
	if new(big.Int).Rsh(den, uint(tz)).Cmp(bigOne) != 0 {
		return BigFloat{}, fmt.Errorf("%w: denominator is not a power of 2", ErrNotExactlyRepresentable)
	}
	return BigFloat{
		Exponent: big.NewInt(-int64(tz)),
		Mantissa: new(big.Int).Set(r.Num()),
	}, nil
}

// BigFloatFromBigFloat converts f to a BigFloat, preserving f's exact value.
// f must be finite (not Inf).
func BigFloatFromBigFloat(f *big.Float) (BigFloat, error) {
	if f.IsInf() {
		return BigFloat{}, errors.New("cbor: cannot convert non-finite big.Float to BigFloat")
	}
	// A finite big.Float is always a dyadic rational, so the conversion
	// through big.Rat is exact.
	r, _ := f.Rat(nil)
	return BigFloatFromBigRat(r)
}

// BigRat returns the exact value of bf as a big.Rat. It returns
// ErrExponentOutOfRange if the exponent's absolute value exceeds
// MaxExactExponent, and an error if Exponent or Mantissa is nil.
func (bf BigFloat) BigRat() (*big.Rat, error) {
	e, err := checkedExponent(bf.Exponent, bf.Mantissa)
	if err != nil {
		return nil, err
	}
	m := new(big.Int).Set(bf.Mantissa)
	if e >= 0 {
		return new(big.Rat).SetInt(m.Lsh(m, uint(e))), nil
	}
	return new(big.Rat).SetFrac(m, new(big.Int).Lsh(bigOne, uint(-e))), nil
}

// BigFloat returns the exact value of bf as a big.Float with the given
// precision. It returns an error wrapping ErrNotExactlyRepresentable if prec
// bits are not enough to represent the value exactly, rather than silently
// rounding.
func (bf BigFloat) BigFloat(prec uint) (*big.Float, error) {
	r, err := bf.BigRat()
	if err != nil {
		return nil, err
	}
	return exactBigFloatFromRat(r, prec)
}

// Normalized returns the canonical form of bf: the mantissa is shifted right
// to remove trailing zero bits and the exponent is adjusted accordingly, so
// the resulting mantissa is odd (or zero). bf is not modified.
func (bf BigFloat) Normalized() BigFloat {
	if bf.Exponent == nil || bf.Mantissa == nil || bf.Mantissa.Sign() == 0 {
		return bf
	}
	tz := bf.Mantissa.TrailingZeroBits()
	if tz > math.MaxInt64 {
		return bf
	}
	return BigFloat{
		Exponent: new(big.Int).Add(bf.Exponent, big.NewInt(int64(tz))),
		Mantissa: new(big.Int).Rsh(bf.Mantissa, uint(tz)),
	}
}

// String returns bf in the form "mantissa p exponent" (e.g. "3p-1"),
// preserving the stored exponent rather than normalizing.
func (bf BigFloat) String() string {
	if bf.Exponent == nil || bf.Mantissa == nil {
		return "BigFloat(nil)"
	}
	if bf.Exponent.Sign() == 0 {
		return bf.Mantissa.String()
	}
	return bf.Mantissa.String() + "p" + bf.Exponent.String()
}

// MarshalCBOR encodes bf as CBOR tag 5 with a two-element array content
// [exponent, mantissa]. Exponent and mantissa encode as regular CBOR
// integers when they fit, or as bignums (tag 2 or 3) otherwise.
func (bf BigFloat) MarshalCBOR() ([]byte, error) {
	if bf.Exponent == nil || bf.Mantissa == nil {
		return nil, errors.New("cbor: cannot encode BigFloat with nil Exponent or Mantissa")
	}
	return Marshal(Tag{Number: tagNumBigFloat, Content: [2]*big.Int{bf.Exponent, bf.Mantissa}})
}

// UnmarshalCBOR decodes a CBOR tag 5 data item into bf. The content must be
// a two-element array [exponent, mantissa] of integers (regular integers or
// bignums). The exponent and mantissa are stored exactly as encoded, without
// normalization.
func (bf *BigFloat) UnmarshalCBOR(data []byte) error {
	e, m, err := unmarshalExponentMantissa(data, tagNumBigFloat)
	if err != nil {
		return err
	}
	bf.Exponent = e
	bf.Mantissa = m
	return nil
}
