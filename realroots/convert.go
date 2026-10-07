package realroots

import (
	"errors"
	"math/big"

	"github.com/shopspring/decimal"
)

// decimalToRat converts a finite decimal to an exact rational. A decimal
// stores coefficient*10^exponent, so this requires no rounding and never
// touches float64.
func decimalToRat(d decimal.Decimal) (*big.Rat, error) {
	coef := d.Coefficient()
	if coef == nil {
		return nil, errors.New("realroots: nil decimal coefficient")
	}
	num := new(big.Int).Set(coef)
	exp := d.Exponent()
	if exp >= 0 {
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
		num.Mul(num, pow)
		return new(big.Rat).SetInt(num), nil
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-exp)), nil)
	return new(big.Rat).SetFrac(num, pow), nil
}

// ratToDecimal renders an exact rational as a finite decimal. Every endpoint
// the engine emits is either the original decimal endpoint or a dyadic
// number (a repeated halving of one), hence always finite in base 10.
func ratToDecimal(r *big.Rat) decimal.Decimal {
	den := r.Denom()
	num := new(big.Int).Set(r.Num())
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}

	// Reduce den = 2^a * 5^b with b trailing factors of five; the rest must
	// be a power of two (possibly one). Scale the numerator so the
	// denominator becomes 10^k for k = max(a, b).
	rest := new(big.Int).Set(den)
	b5 := 0
	five := big.NewInt(5)
	zero := new(big.Int)
	for new(big.Int).Mod(rest, five).Cmp(zero) == 0 {
		rest.Quo(rest, five)
		b5++
	}
	a2 := 0
	two := big.NewInt(2)
	for new(big.Int).Mod(rest, two).Cmp(zero) == 0 {
		rest.Quo(rest, two)
		a2++
	}
	if rest.Cmp(big.NewInt(1)) != 0 {
		// Should be unreachable for endpoints this package emits; keep an
		// exact fallback via the decimal constructor's precision argument.
		return decimal.NewFromBigRat(r, 50)
	}
	k := a2
	if b5 > k {
		k = b5
	}
	// num / den == (num * 2^(k-a2) * 5^(k-b5)) / 10^k when k >= a2, b5.
	if k-a2 > 0 {
		num.Mul(num, new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(k-a2)), nil))
	}
	if k-b5 > 0 {
		num.Mul(num, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(k-b5)), nil))
	}
	if neg {
		num.Neg(num)
	}
	return decimal.NewFromBigInt(num, int32(-k))
}

func polyFromDecimals(coeffs []decimal.Decimal) (ratPoly, error) {
	p := make(ratPoly, len(coeffs))
	for i, d := range coeffs {
		r, err := decimalToRat(d)
		if err != nil {
			return nil, err
		}
		p[i] = r
	}
	if len(p) == 0 {
		return ratPoly{new(big.Rat)}, nil
	}
	return p.trim(), nil
}

func resolveTargetWidth(w decimal.Decimal, lo, hi *big.Rat) (*big.Rat, error) {
	if !w.IsZero() {
		if w.Sign() <= 0 {
			return nil, errors.New("realroots: TargetWidth must be positive")
		}
		return decimalToRat(w)
	}
	// Default: one millionth of the requested interval width.
	t := new(big.Rat).Sub(hi, lo)
	return t.Quo(t, big.NewRat(1000000, 1)), nil
}
