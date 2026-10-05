package decimal

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type Rounding int

const (
	HalfAwayFromZero Rounding = iota
	HalfUp
	HalfDown
	HalfEven
	Up
	Down
	Ceiling
	Floor
)

type Decimal struct {
	value *big.Int
	scale int
}

type Settings struct {
	Scale    int
	Rounding Rounding
}

func DefaultSettings() Settings {
	return Settings{Scale: 2, Rounding: HalfAwayFromZero}
}

func newDecimal(value *big.Int, scale int) Decimal {
	if value == nil {
		value = new(big.Int)
	}
	return Decimal{value: new(big.Int).Set(value), scale: scale}
}

func Zero(scale int) Decimal {
	if scale < 0 {
		scale = 0
	}
	return Decimal{value: new(big.Int), scale: scale}
}

func FromInt(value int) Decimal {
	return FromInt64(int64(value))
}

func FromInt64(value int64) Decimal {
	return newDecimal(big.NewInt(value), 0)
}

func FromUint64(value uint64) Decimal {
	return newDecimal(new(big.Int).SetUint64(value), 0)
}

func FromValue(value any) (Decimal, bool) {
	switch v := value.(type) {
	case Decimal:
		return v.Clone(), true
	case int:
		return FromInt(v), true
	case int8:
		return FromInt64(int64(v)), true
	case int16:
		return FromInt64(int64(v)), true
	case int32:
		return FromInt64(int64(v)), true
	case int64:
		return FromInt64(v), true
	case uint:
		return FromUint64(uint64(v)), true
	case uint8:
		return FromUint64(uint64(v)), true
	case uint16:
		return FromUint64(uint64(v)), true
	case uint32:
		return FromUint64(uint64(v)), true
	case uintptr:
		return FromUint64(uint64(v)), true
	case uint64:
		return FromUint64(v), true
	default:
		return Decimal{}, false
	}
}

func Parse(input string) (Decimal, error) {
	s := strings.ReplaceAll(input, "_", "")
	if s == "" {
		return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
	}

	negative := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		negative = true
		s = s[1:]
	}
	if s == "" {
		return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
	}

	exponent := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		n, err := strconv.Atoi(s[i+1:])
		if err != nil {
			return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
		}
		exponent = n
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
	}
	for _, part := range parts {
		if part != "" && !isDigits(part) {
			return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
		}
	}
	if len(parts) == 2 && parts[0] == "" && parts[1] == "" {
		return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
	}

	fractionLength := 0
	if len(parts) == 2 {
		fractionLength = len(parts[1])
		s = parts[0] + parts[1]
	}

	value, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal literal: %q", input)
	}
	if negative {
		value.Neg(value)
	}
	scale := fractionLength - exponent
	if scale < 0 {
		value.Mul(value, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-scale)), nil))
		scale = 0
	}
	return newDecimal(value, scale), nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (d Decimal) Clone() Decimal {
	return newDecimal(d.value, d.scale)
}

func (d Decimal) Scale() int {
	return d.scale
}

func (d Decimal) IsZero() bool {
	return d.value.Sign() == 0
}

func (d Decimal) String() string {
	if d.value == nil {
		return "0"
	}
	negative := d.value.Sign() < 0
	raw := d.value.String()
	if negative {
		raw = raw[1:]
	}
	if d.scale <= 0 {
		raw += strings.Repeat("0", -d.scale)
		if negative {
			return "-" + raw
		}
		return raw
	}
	if len(raw) <= d.scale {
		raw = strings.Repeat("0", d.scale-len(raw)+1) + raw
	}
	whole, fraction := raw[:len(raw)-d.scale], raw[len(raw)-d.scale:]
	out := whole + "." + fraction
	if negative {
		return "-" + out
	}
	return out
}

func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Decimal) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := Parse(value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func (d Decimal) Rat() *big.Rat {
	if d.value == nil {
		return new(big.Rat)
	}
	if d.scale >= 0 {
		return new(big.Rat).SetFrac(new(big.Int).Set(d.value), power10(d.scale))
	}
	return new(big.Rat).SetFrac(new(big.Int).Mul(new(big.Int).Set(d.value), power10(-d.scale)), big.NewInt(1))
}

func (d Decimal) Rescale(scale int, rounding Rounding) Decimal {
	return roundRat(d.Rat(), scale, rounding)
}

func (d Decimal) Neg() Decimal {
	value := new(big.Int)
	if d.value != nil {
		value.Neg(d.value)
	}
	return newDecimal(value, d.scale)
}

func Add(a, b any, settings Settings) (Decimal, error) {
	x, y, err := operands(a, b)
	if err != nil {
		return Decimal{}, err
	}
	return binary(x, y, settings, true), nil
}

func Subtract(a, b any, settings Settings) (Decimal, error) {
	x, y, err := operands(a, b)
	if err != nil {
		return Decimal{}, err
	}
	return binary(x, y, settings, false), nil
}

func Multiply(a, b any, settings Settings) (Decimal, error) {
	x, y, err := operands(a, b)
	if err != nil {
		return Decimal{}, err
	}
	value := new(big.Int).Mul(x.value, y.value)
	resultScale := x.scale + y.scale
	return roundRat(new(big.Rat).SetFrac(value, power10(resultScale)), settings.Scale, settings.Rounding), nil
}

func Divide(a, b any, settings Settings) (Decimal, error) {
	x, y, err := operands(a, b)
	if err != nil {
		return Decimal{}, err
	}
	if y.value.Sign() == 0 {
		return Decimal{}, fmt.Errorf("decimal divide by zero")
	}
	return roundRat(new(big.Rat).Quo(x.Rat(), y.Rat()), settings.Scale, settings.Rounding), nil
}

func operands(a, b any) (Decimal, Decimal, error) {
	x, ok := FromValue(a)
	if !ok {
		return Decimal{}, Decimal{}, mixedFloatError(a)
	}
	y, ok := FromValue(b)
	if !ok {
		return Decimal{}, Decimal{}, mixedFloatError(b)
	}
	return x, y, nil
}

func mixedFloatError(value any) error {
	if isFloat(value) {
		return fmt.Errorf("cannot mix decimal with float %T; convert explicitly with decimal(string(...))", value)
	}
	return fmt.Errorf("invalid decimal operand %T", value)
}

func isFloat(value any) bool {
	switch value.(type) {
	case float32, float64:
		return true
	default:
		return false
	}
}

func binary(x, y Decimal, settings Settings, add bool) Decimal {
	scale := x.scale
	if y.scale > scale {
		scale = y.scale
	}
	left := rescaleValue(x, scale)
	right := rescaleValue(y, scale)
	value := new(big.Int)
	if add {
		value.Add(left, right)
	} else {
		value.Sub(left, right)
	}
	return roundRat(new(big.Rat).SetFrac(value, power10(scale)), settings.Scale, settings.Rounding)
}

func rescaleValue(d Decimal, scale int) *big.Int {
	value := new(big.Int).Set(d.value)
	if scale > d.scale {
		value.Mul(value, power10(scale-d.scale))
	}
	return value
}

func Compare(a, b any) (int, error) {
	return CompareWithSettings(a, b, DefaultSettings())
}

func CompareWithSettings(a, b any, settings Settings) (int, error) {
	x, y, err := operands(a, b)
	if err != nil {
		return 0, err
	}
	left := x.Rescale(settings.Scale, settings.Rounding).value
	right := y.Rescale(settings.Scale, settings.Rounding).value
	return left.Cmp(right), nil
}

func Equal(a, b any) (bool, error) {
	cmp, err := Compare(a, b)
	return cmp == 0, err
}

func EqualWithSettings(a, b any, settings Settings) (bool, error) {
	cmp, err := CompareWithSettings(a, b, settings)
	return cmp == 0, err
}

func power10(scale int) *big.Int {
	if scale < 0 {
		scale = 0
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
}

func roundRat(rat *big.Rat, scale int, rounding Rounding) Decimal {
	scaled := new(big.Rat).Mul(rat, new(big.Rat).SetInt(power10(scale)))
	numerator := new(big.Int).Set(scaled.Num())
	denominator := new(big.Int).Set(scaled.Denom())

	zero := numerator.Sign() == 0
	exact := zero
	if zero {
		return Zero(scale)
	}

	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	remainder.Abs(remainder)
	if remainder.Sign() == 0 {
		exact = true
	}

	if !exact {
		negative := numerator.Sign() < 0
		doubleRemainder := new(big.Int).Lsh(remainder, 1)
		cmp := doubleRemainder.Cmp(denominator)

		switch rounding {
		case Up:
			advance(quotient, negative)
		case Down:
		case Ceiling:
			if !negative {
				quotient.Add(quotient, big.NewInt(1))
			}
		case Floor:
			if negative {
				quotient.Sub(quotient, big.NewInt(1))
			}
		case HalfUp:
			if cmp > 0 || cmp == 0 && !negative {
				quotient.Add(quotient, big.NewInt(1))
			}
		case HalfDown:
			if cmp > 0 {
				quotient.Add(quotient, big.NewInt(1))
			}
		case HalfEven:
			if cmp > 0 || cmp == 0 && quotient.Bit(0) == 1 {
				advance(quotient, negative)
			}
		case HalfAwayFromZero:
			fallthrough
		default:
			if cmp > 0 || cmp == 0 {
				advance(quotient, negative)
			}
		}
	}

	return newDecimal(quotient, scale)
}

func advance(q *big.Int, negative bool) {
	if negative {
		q.Sub(q, big.NewInt(1))
	} else {
		q.Add(q, big.NewInt(1))
	}
}
