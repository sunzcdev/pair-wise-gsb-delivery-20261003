package cbor

import (
	"math/big"
	"strings"
)

// maxExpandedDigits caps the length of a fully expanded decimal/binary
// rendering. String is called in logging and fmt chains where it must stay
// cheap regardless of the numeric limits in force; beyond this size the
// compact coefficient form is used.
const maxExpandedDigits = 4096

// String renders the value using the stored exponent, without normalizing it.
// Thus NewDecimal(0, 1).String() is "1" and NewDecimal(-2, 100).String() is
// "1.00". If the expanded rendering would exceed maxExpandedDigits the
// compact coefficient form "m*10^e" is returned instead, so String never
// forces a huge allocation even for hostile exponents.
func (x Decimal) String() string {
	return formatScaled(x.Exponent, x.Mantissa, tenInt)
}

// String renders the value using the stored binary exponent, without
// normalizing it (for example "0.5" for {-1, 1}). See Decimal.String for the
// compact-form safety bound.
func (x BigFloat) String() string {
	return formatScaled(x.Exponent, x.Mantissa, twoInt)
}

func formatScaled(e int64, m *big.Int, base *big.Int) string {
	if m == nil {
		m = new(big.Int)
	}
	if m.Sign() == 0 {
		if e < 0 && -e > maxExpandedDigits {
			return compactForm(m, e, base)
		}
	}
	digitLen := int64(len(new(big.Int).Abs(m).String()))
	tail := e
	if tail < 0 {
		tail = -tail
	}
	if m.Sign() != 0 && digitLen+tail > maxExpandedDigits {
		return compactForm(m, e, base)
	}

	neg := m.Sign() < 0
	abs := new(big.Int).Abs(m)
	digits := abs.String()

	var body string
	switch {
	case m.Sign() == 0 && e >= 0:
		return "0"
	case m.Sign() == 0:
		// Zero with a stored fractional scale, e.g. {e:-2, 0} -> "0.00".
		body = "0." + strings.Repeat("0", int(-e))
	case e >= 0:
		body = digits + strings.Repeat("0", int(e))
	case int64(len(digits))+e >= 1:
		// Decimal point lies inside the digits.
		pos := int(int64(len(digits)) + e)
		body = digits[:pos] + "." + digits[pos:]
	default:
		// 0.00...digits
		zeros := int(-(int64(len(digits)) + e))
		body = "0." + strings.Repeat("0", zeros) + digits
	}
	if neg {
		return "-" + body
	}
	return body
}

func compactForm(m *big.Int, e int64, base *big.Int) string {
	b := "10"
	if base == twoInt {
		b = "2"
	}
	return m.String() + "*" + b + "^" + signedItoa(e)
}

func signedItoa(n int64) string {
	if n >= 0 {
		return itoa(uint64(n))
	}
	if n == -1<<63 {
		return "-9223372036854775808"
	}
	return "-" + itoa(uint64(-n))
}

// ParseDecimal parses a decimal literal such as "1", "-1.25", "1.10" or
// "0.001". It preserves the scale from the number of fractional digits, so
// "1.10" yields exponent -2 with coefficient 110; call Canonical on the
// result for the normalized representation. Scientific notation (for example
// "1.5e3") is also accepted, in which case the encoded exponent is folded
// into the scale.
func ParseDecimal(s string, l *NumericLimits) (Decimal, error) {
	lim := DefaultLimits
	if l != nil {
		lim = l.orDefault()
	}
	original := s

	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
	}

	var expAdj int64
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		if i == 0 {
			return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
		}
		v, ok := parseSigned(s[i+1:])
		if !ok {
			return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal exponent in " + original}
		}
		expAdj = v
		s = s[:i]
	}

	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		fracPart = s[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
		}
	}
	if intPart == "" {
		intPart = "0"
	}
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
		}
	}
	for _, c := range fracPart {
		if c < '0' || c > '9' {
			return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
		}
	}
	if intPart == "0" && fracPart == "" && strings.HasPrefix(s, ".") {
		return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
	}

	scale := int64(len(fracPart))
	digits := intPart + fracPart
	m := new(big.Int)
	if trimmed := strings.TrimLeft(digits, "0"); trimmed != "" {
		if _, ok := m.SetString(trimmed, 10); !ok {
			return Decimal{}, &SyntaxError{msg: "cbor: invalid decimal literal " + original}
		}
	}
	if neg {
		m.Neg(m)
	}

	e := expAdj - scale
	if err := lim.checkExponent(e); err != nil {
		return Decimal{}, err
	}
	if m.BitLen() > lim.MaxMantissaBits {
		return Decimal{}, ErrMantissaTooLarge
	}
	return Decimal{Exponent: e, Mantissa: m}, nil
}

func parseSigned(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
		if v < 0 {
			return 0, false
		}
	}
	if neg {
		v = -v
	}
	return v, true
}
