package sjson

import (
	"math/big"

	"github.com/tidwall/gjson"
)

// DefaultIncrementMaxLength is the default maximum length, in bytes, of
// the number produced by Increment and IncrementWithOptions.
const DefaultIncrementMaxLength = 512

// maxIncrementAlignSlack allows the exponent alignment of an increment
// operation to exceed the output length limit by a small amount so that
// operands with largely overlapping magnitudes can still cancel out.
const maxIncrementAlignSlack = 1024

// IncrementOptions represents additional options for the
// IncrementWithOptions function.
type IncrementOptions struct {
	// Initial is the initial value used as the base of the addition when
	// the target path does not exist. It must use JSON number syntax.
	// When empty, a missing target results in an error.
	Initial string
	// MaxLength is the maximum allowed length, in bytes, of the resulting
	// number. The operation fails, before the json is modified, when the
	// result exceeds this limit. When zero, DefaultIncrementMaxLength is
	// used.
	MaxLength int
}

// decimal is an exact decimal number: value = mant * 10^exp, negated when
// neg is true. mant is always positive.
type decimal struct {
	neg  bool
	mant *big.Int
	exp  int
}

var errInvalidJSONNumber = &errorType{"invalid JSON number"}

// parseJSONNumber parses a number that uses the JSON number syntax,
// such as "-12.345e-2". The result is exact, no float64 is involved.
func parseJSONNumber(s string) (decimal, error) {
	var d decimal
	i := 0
	if i < len(s) && s[i] == '-' {
		d.neg = true
		i++
	}
	istart := i
	if i < len(s) && s[i] == '0' {
		i++
	} else {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == istart {
			return d, errInvalidJSONNumber
		}
	}
	intpart := s[istart:i]
	var fracpart string
	if i < len(s) && s[i] == '.' {
		i++
		fstart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == fstart {
			return d, errInvalidJSONNumber
		}
		fracpart = s[fstart:i]
	}
	exp := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		var eneg bool
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			eneg = s[i] == '-'
			i++
		}
		estart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			if exp < 1<<60 {
				exp = exp*10 + int(s[i]-'0')
			}
			i++
		}
		if i == estart {
			return d, errInvalidJSONNumber
		}
		if eneg {
			exp = -exp
		}
	}
	if i != len(s) {
		return d, errInvalidJSONNumber
	}
	d.exp = exp - len(fracpart)
	d.mant = new(big.Int)
	d.mant.SetString(intpart+fracpart, 10)
	d.normalize()
	return d, nil
}

// normalize strips insignificant trailing zeros and resets a zero value
// to a canonical form.
func (d *decimal) normalize() {
	if d.mant.Sign() == 0 {
		d.neg = false
		d.exp = 0
		return
	}
	ten := big.NewInt(10)
	q := new(big.Int)
	r := new(big.Int)
	for d.exp < 0 {
		q.QuoRem(d.mant, ten, r)
		if r.Sign() != 0 {
			break
		}
		d.mant.Set(q)
		d.exp++
	}
}

// length returns the exact length, in bytes, of the rendered number.
func (d decimal) length() int {
	if d.mant.Sign() == 0 {
		return 1
	}
	digits := len(d.mant.String())
	n := digits
	if d.neg {
		n++
	}
	if d.exp >= 0 {
		return n + d.exp
	}
	if digits+d.exp > 0 {
		return n + 1
	}
	return n + 2 - d.exp - digits
}

// String renders the number using plain JSON number syntax.
func (d decimal) String() string {
	if d.mant.Sign() == 0 {
		return "0"
	}
	s := d.mant.String()
	var out []byte
	if d.neg {
		out = append(out, '-')
	}
	if d.exp >= 0 {
		out = append(out, s...)
		out = appendRepeat(out, "0", d.exp)
	} else if point := len(s) + d.exp; point > 0 {
		out = append(out, s[:point]...)
		out = append(out, '.')
		out = append(out, s[point:]...)
	} else {
		out = append(out, "0."...)
		out = appendRepeat(out, "0", -point)
		out = append(out, s...)
	}
	return string(out)
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// addDecimals returns the exact sum of a and b.
func addDecimals(a, b decimal) decimal {
	if a.mant.Sign() == 0 {
		return b
	}
	if b.mant.Sign() == 0 {
		return a
	}
	exp := a.exp
	if b.exp < exp {
		exp = b.exp
	}
	am := new(big.Int).Set(a.mant)
	if diff := a.exp - exp; diff > 0 {
		am.Mul(am, pow10(diff))
	}
	if a.neg {
		am.Neg(am)
	}
	bm := new(big.Int).Set(b.mant)
	if diff := b.exp - exp; diff > 0 {
		bm.Mul(bm, pow10(diff))
	}
	if b.neg {
		bm.Neg(bm)
	}
	am.Add(am, bm)
	sum := decimal{mant: am, exp: exp}
	if am.Sign() < 0 {
		sum.neg = true
		sum.mant.Neg(sum.mant)
	}
	sum.normalize()
	return sum
}

var errIncrementTooLong = &errorType{
	"increment result exceeds the maximum number length"}

// addAndCheckLength returns the exact sum of a and b, or an error when the
// rendered result would exceed maxLen bytes.
func addAndCheckLength(a, b decimal, maxLen int) (decimal, error) {
	if a.mant.Sign() != 0 && b.mant.Sign() != 0 {
		// Guard the exponent alignment work against absurd exponents
		// before doing any arithmetic.
		diff := a.exp - b.exp
		if diff < 0 {
			diff = -diff
		}
		if diff > maxLen+maxIncrementAlignSlack {
			return decimal{}, errIncrementTooLong
		}
	}
	sum := addDecimals(a, b)
	if sum.length() > maxLen {
		return decimal{}, errIncrementTooLong
	}
	return sum, nil
}

// Increment increments the json number at the specified path by delta.
// The delta must use JSON number syntax, such as "2", "-0.5", or "1.25e3".
// The calculation is exact and does not convert through a float64, so
// integers larger than 2^53 and high precision decimals are safe.
// The target must exist and be a json number; null, strings, and other
// types are not converted automatically. Use IncrementWithOptions to
// provide an initial value for a missing target or to change the maximum
// length of the resulting number.
func Increment(json, path, delta string) (string, error) {
	return IncrementWithOptions(json, path, delta, nil)
}

// IncrementWithOptions increments the json number at the specified path by
// delta, with options. This function works the same as Increment except
// that a missing target may be initialized from opts.Initial and the
// maximum length of the resulting number may be changed with
// opts.MaxLength.
func IncrementWithOptions(json, path, delta string,
	opts *IncrementOptions) (string, error) {
	maxLen := DefaultIncrementMaxLength
	var initial string
	if opts != nil {
		if opts.MaxLength > 0 {
			maxLen = opts.MaxLength
		}
		initial = opts.Initial
	}
	dd, err := parseJSONNumber(delta)
	if err != nil {
		return json, &errorType{"invalid increment delta '" + delta + "'"}
	}
	var base decimal
	res := gjson.Get(json, path)
	if res.Exists() {
		if res.Type != gjson.Number {
			return json, &errorType{
				"cannot increment a value of type " + res.Type.String()}
		}
		base, err = parseJSONNumber(res.Raw)
		if err != nil {
			return json, &errorType{
				"cannot increment invalid number '" + res.Raw + "'"}
		}
	} else {
		if initial == "" {
			return json, &errorType{
				"cannot increment a missing path without an initial value"}
		}
		base, err = parseJSONNumber(initial)
		if err != nil {
			return json, &errorType{
				"invalid increment initial value '" + initial + "'"}
		}
	}
	sum, err := addAndCheckLength(base, dd, maxLen)
	if err != nil {
		return json, err
	}
	njson, err := set(json, path, sum.String(), false, false, false, false)
	if err == errNoChange {
		return json, nil
	}
	if err != nil {
		return json, err
	}
	return string(njson), nil
}
