package decimal

import (
	"fmt"
	"math"
	"math/big"
)

// RoundingMode selects how Context rounds an inexact result.
type RoundingMode uint8

const (
	// HalfUp rounds halves away from zero.
	HalfUp RoundingMode = iota
	// HalfEven rounds halves to the nearest even digit.
	HalfEven
	// HalfDown rounds halves toward zero.
	HalfDown
	// Up rounds every discarded nonzero digit away from zero.
	Up
	// Down rounds toward zero.
	Down
	// Ceiling rounds toward positive infinity.
	Ceiling
	// Floor rounds toward negative infinity.
	Floor
)

// Traps selects conditions that return a typed error. Division by zero always
// returns a typed error because Decimal has no infinite representation.
type Traps struct {
	Rounded   bool
	Inexact   bool
	Overflow  bool
	Underflow bool
	// DivisionByZero is retained for configuration compatibility; division by
	// zero is always trapped because no finite Decimal can represent it.
	DivisionByZero bool
}

// Status contains the arithmetic conditions raised by an operation.
type Status struct {
	Rounded        bool
	Inexact        bool
	Overflow       bool
	Underflow      bool
	DivisionByZero bool
}

// Context carries independent significant-digit arithmetic settings.
type Context struct {
	// Precision is the maximum number of significant digits in the result.
	Precision uint32
	// MinExponent and MaxExponent bound the adjusted exponent. For example,
	// 1.23E-4 has precision 3 and adjusted exponent -4.
	MinExponent int32
	MaxExponent int32
	// Rounding selects the rounding direction used for discarded digits.
	Rounding RoundingMode
	// Traps selects conditions that return a typed error.
	Traps Traps
}

// Result is the finite result and status flags from a Context operation.
type Result struct {
	Value  Decimal
	Status Status
}

// InvalidContextError reports that a Context cannot be used.
type InvalidContextError struct {
	Reason string
}

func (e InvalidContextError) Error() string {
	return "decimal: invalid context: " + e.Reason
}

// ConditionError is the typed base for trapped arithmetic conditions.
type ConditionError struct {
	Operation string
	Result    Result
}

func (e ConditionError) Error() string {
	return "decimal: " + e.Operation + ": " + conditionName(e.Result.Status)
}

// Status returns the arithmetic status that triggered the trap.
func (e ConditionError) Status() Status {
	return e.Result.Status
}

// RoundedError reports that a trap fired for a rounded result.
type RoundedError struct{ ConditionError }

// InexactError reports that a trap fired for an inexact result.
type InexactError struct{ ConditionError }

// OverflowError reports that a trap fired for an overflow result.
type OverflowError struct{ ConditionError }

// UnderflowError reports that a trap fired for an underflow result.
type UnderflowError struct{ ConditionError }

// DivisionByZeroError reports a division by zero.
type DivisionByZeroError struct{ ConditionError }

type contextDecimal struct {
	value *big.Int
	exp   int64
}

func (c Context) validate() error {
	if c.Precision == 0 {
		return InvalidContextError{"precision must be greater than zero"}
	}
	if c.MinExponent > c.MaxExponent {
		return InvalidContextError{"minimum exponent is greater than maximum exponent"}
	}
	if c.Rounding > Floor {
		return InvalidContextError{"unsupported rounding mode"}
	}
	if int64(c.MaxExponent) < int64(c.MinExponent)+int64(c.Precision)-1 {
		return InvalidContextError{"exponent range cannot contain the requested precision"}
	}
	if int64(c.MinExponent)-int64(c.Precision)+1 < math.MinInt32 {
		return InvalidContextError{"minimum exponent and precision overflow Decimal exponent"}
	}
	if int64(c.MaxExponent)-int64(c.Precision)+1 > math.MaxInt32 {
		return InvalidContextError{"maximum exponent and precision overflow Decimal exponent"}
	}
	return nil
}

// Add returns x+y rounded according to c.
func (c Context) Add(x, y Decimal) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, err
	}

	rx, ry := rescalePairContext(toContextDecimal(x), toContextDecimal(y))
	exact := contextDecimal{value: new(big.Int).Add(rx.value, ry.value), exp: rx.exp}
	return c.finish("add", exact)
}

// Sub returns x-y rounded according to c.
func (c Context) Sub(x, y Decimal) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, err
	}

	rx, ry := rescalePairContext(toContextDecimal(x), toContextDecimal(y))
	exact := contextDecimal{value: new(big.Int).Sub(rx.value, ry.value), exp: rx.exp}
	return c.finish("sub", exact)
}

// Mul returns x*y rounded according to c.
func (c Context) Mul(x, y Decimal) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, err
	}

	if x.getValue().Sign() == 0 || y.getValue().Sign() == 0 {
		return c.finish("mul", contextDecimal{value: new(big.Int), exp: 0})
	}

	exact := contextDecimal{
		value: new(big.Int).Mul(x.getValue(), y.getValue()),
		exp:   int64(x.exp) + int64(y.exp),
	}
	return c.finish("mul", exact)
}

// Div returns x/y rounded according to c.
func (c Context) Div(x, y Decimal) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, err
	}

	if y.getValue().Sign() == 0 {
		result := Result{Status: Status{DivisionByZero: true}}
		return result, &DivisionByZeroError{ConditionError{Operation: "div", Result: result}}
	}
	if x.getValue().Sign() == 0 {
		return c.finish("div", contextDecimal{value: new(big.Int), exp: 0})
	}

	rat := new(big.Rat).SetFrac(new(big.Int).Set(x.getValue()), new(big.Int).Set(y.getValue()))
	scaleRat(rat, int64(x.exp)-int64(y.exp))
	if value, exp, ok := finiteRatValue(rat); ok {
		return c.finish("div", contextDecimal{value: value, exp: exp})
	}

	adjusted := quotientAdjustedExponent(x, y)
	if adjusted > int64(c.MaxExponent) {
		return c.overflowResult("div", x.getValue().Sign()*y.getValue().Sign())
	}
	if adjusted < int64(c.MinExponent)-1 {
		return c.underflowZeroResult("div")
	}

	targetExp := adjusted - int64(c.Precision) + 1
	scaled := new(big.Rat).Set(rat)
	scaleRat(scaled, -targetExp)

	q, remainder := new(big.Int).QuoRem(
		new(big.Int).Set(scaled.Num()),
		new(big.Int).Set(scaled.Denom()),
		new(big.Int),
	)
	roundQuotient(q, new(big.Int).Abs(remainder), scaled.Denom(), c.Rounding, x.getValue().Sign()*y.getValue().Sign())

	rounded := contextDecimal{value: q, exp: targetExp}
	status := Status{Rounded: true, Inexact: true}
	roundedAdjusted := adjustedExponentContext(rounded)
	if roundedAdjusted > int64(c.MaxExponent) {
		return c.overflowResult("div", q.Sign())
	}
	if roundedAdjusted < int64(c.MinExponent) {
		return c.underflowZeroResult("div")
	}
	if adjusted < int64(c.MinExponent) {
		status.Underflow = true
	}

	value, err := toDecimalContext(rounded)
	if err != nil {
		return Result{}, err
	}
	return c.trapResult("div", Result{Value: value, Status: status})
}

func toContextDecimal(d Decimal) contextDecimal {
	return contextDecimal{value: new(big.Int).Set(d.getValue()), exp: int64(d.exp)}
}

func rescalePairContext(x, y contextDecimal) (contextDecimal, contextDecimal) {
	if x.exp < y.exp {
		return x, rescaleContextDecimal(y, x.exp)
	}
	if x.exp > y.exp {
		return rescaleContextDecimal(x, y.exp), y
	}
	return x, y
}

func rescaleContextDecimal(d contextDecimal, exp int64) contextDecimal {
	if d.exp == exp {
		return contextDecimal{value: new(big.Int).Set(d.value), exp: exp}
	}

	power := new(big.Int).Exp(tenInt, big.NewInt(absInt64(exp-d.exp)), nil)
	value := new(big.Int).Set(d.value)
	if exp > d.exp {
		value.Quo(value, power)
	} else {
		value.Mul(value, power)
	}
	return contextDecimal{value: value, exp: exp}
}

func (c Context) finish(operation string, exact contextDecimal) (Result, error) {
	if exact.value == nil || exact.value.Sign() == 0 {
		return c.trapResult(operation, Result{Value: Decimal{value: new(big.Int), exp: 0}})
	}

	value, status := c.round(exact)
	return c.trapResult(operation, Result{Value: value, Status: status})
}

func (c Context) round(d contextDecimal) (Decimal, Status) {
	sign := d.value.Sign()
	adjusted := adjustedExponentContext(d)
	if adjusted > int64(c.MaxExponent) {
		return c.largestFinite(sign), Status{Rounded: true, Inexact: true, Overflow: true}
	}
	if adjusted < int64(c.MinExponent)-1 {
		return Decimal{}, Status{Rounded: true, Inexact: true, Underflow: true}
	}

	status := Status{}
	tiny := adjusted < int64(c.MinExponent)
	digits := int64(numDigitsBig(d.value))
	if digits > int64(c.Precision) {
		targetExp := d.exp + digits - int64(c.Precision)
		power := new(big.Int).Exp(tenInt, big.NewInt(digits-int64(c.Precision)), nil)
		q, remainder := new(big.Int).QuoRem(new(big.Int).Set(d.value), power, new(big.Int))

		if remainder.Sign() == 0 {
			status.Rounded = true
		} else {
			absoluteRemainder := new(big.Int).Abs(remainder)
			roundQuotient(q, absoluteRemainder, power, c.Rounding, sign)
			status = Status{Rounded: true, Inexact: true}
		}
		if q.BitLen() > 0 && int64(numDigitsBig(q)) > int64(c.Precision) {
			q.Quo(q, tenInt)
			targetExp++
		}
		d = contextDecimal{value: q, exp: targetExp}
	}

	adjusted = adjustedExponentContext(d)
	if adjusted > int64(c.MaxExponent) {
		return c.largestFinite(sign), Status{Rounded: true, Inexact: true, Overflow: true}
	}
	if adjusted < int64(c.MinExponent) {
		return Decimal{}, Status{Rounded: true, Inexact: true, Underflow: true}
	}
	if tiny {
		status.Underflow = true
	}

	value, err := toDecimalContext(d)
	if err != nil {
		return Decimal{}, Status{Rounded: true, Inexact: true, Underflow: true}
	}
	return value, status
}

func (c Context) largestFinite(sign int) Decimal {
	value := new(big.Int).Sub(
		new(big.Int).Exp(tenInt, big.NewInt(int64(c.Precision)), nil),
		oneInt,
	)
	value.Mul(value, big.NewInt(int64(sign)))
	exp := int64(c.MaxExponent) - int64(c.Precision) + 1
	return Decimal{value: value, exp: int32(exp)}
}

func (c Context) overflowResult(operation string, sign int) (Result, error) {
	return c.trapResult(operation, Result{
		Value:  c.largestFinite(sign),
		Status: Status{Rounded: true, Inexact: true, Overflow: true},
	})
}

func (c Context) underflowZeroResult(operation string) (Result, error) {
	return c.trapResult(operation, Result{
		Value:  Decimal{value: new(big.Int), exp: 0},
		Status: Status{Rounded: true, Inexact: true, Underflow: true},
	})
}

func (c Context) trapResult(operation string, result Result) (Result, error) {
	switch {
	case result.Status.DivisionByZero:
		return result, &DivisionByZeroError{ConditionError{Operation: operation, Result: result}}
	case result.Status.Overflow && c.Traps.Overflow:
		return result, &OverflowError{ConditionError{Operation: operation, Result: result}}
	case result.Status.Underflow && c.Traps.Underflow:
		return result, &UnderflowError{ConditionError{Operation: operation, Result: result}}
	case result.Status.Inexact && c.Traps.Inexact:
		return result, &InexactError{ConditionError{Operation: operation, Result: result}}
	case result.Status.Rounded && c.Traps.Rounded:
		return result, &RoundedError{ConditionError{Operation: operation, Result: result}}
	}
	return result, nil
}

func roundQuotient(q, remainder, denominator *big.Int, mode RoundingMode, sign int) {
	increment := false
	twiceRemainder := new(big.Int).Lsh(new(big.Int).Set(remainder), 1)
	comparison := twiceRemainder.Cmp(denominator)

	switch mode {
	case Up:
		increment = true
	case Down:
		increment = false
	case Ceiling:
		increment = sign > 0
	case Floor:
		increment = sign < 0
	case HalfUp:
		increment = comparison >= 0
	case HalfDown:
		increment = comparison > 0
	case HalfEven:
		increment = comparison > 0 || (comparison == 0 && q.Bit(0) == 1)
	}

	if increment {
		if sign < 0 {
			q.Sub(q, oneInt)
		} else {
			q.Add(q, oneInt)
		}
	}
}

func adjustedExponentContext(d contextDecimal) int64 {
	return d.exp + int64(numDigitsBig(d.value)) - 1
}

func numDigitsBig(value *big.Int) int {
	return Decimal{value: new(big.Int).Set(value)}.NumDigits()
}

func toDecimalContext(d contextDecimal) (Decimal, error) {
	if d.exp > math.MaxInt32 || d.exp < math.MinInt32 {
		return Decimal{}, InvalidContextError{"result exponent is outside the Decimal range"}
	}
	return Decimal{value: new(big.Int).Set(d.value), exp: int32(d.exp)}, nil
}

func quotientAdjustedExponent(x, y Decimal) int64 {
	absoluteX := new(big.Int).Abs(x.getValue())
	absoluteY := new(big.Int).Abs(y.getValue())
	digitsX := int64(Decimal{value: absoluteX}.NumDigits())
	digitsY := int64(Decimal{value: absoluteY}.NumDigits())
	digitDifference := digitsX - digitsY
	adjusted := digitDifference + int64(x.exp) - int64(y.exp)

	left := new(big.Int).Set(absoluteX)
	right := new(big.Int).Set(absoluteY)
	if digitDifference >= 0 {
		right.Mul(right, new(big.Int).Exp(tenInt, big.NewInt(digitDifference), nil))
	} else {
		left.Mul(left, new(big.Int).Exp(tenInt, big.NewInt(-digitDifference), nil))
	}
	if left.Cmp(right) < 0 {
		adjusted--
	}
	return adjusted
}

func finiteRatValue(rat *big.Rat) (*big.Int, int64, bool) {
	denominator := new(big.Int).Set(rat.Denom())
	twos := countFactors(denominator, twoInt)
	fives := countFactors(denominator, fiveInt)
	if denominator.Cmp(oneInt) != 0 {
		return nil, 0, false
	}

	required := twos
	if fives > required {
		required = fives
	}
	value := new(big.Int).Set(rat.Num())
	if twos > 0 {
		value.Mul(value, new(big.Int).Exp(fiveInt, big.NewInt(twos), nil))
	}
	if fives > 0 {
		value.Mul(value, new(big.Int).Exp(twoInt, big.NewInt(fives), nil))
	}
	return value, -required, true
}

func countFactors(value *big.Int, factor *big.Int) int64 {
	count := int64(0)
	for value.Cmp(oneInt) != 0 {
		quotient, mod := new(big.Int).QuoRem(value, factor, new(big.Int))
		if mod.Sign() != 0 {
			break
		}
		value.Set(quotient)
		count++
	}
	return count
}

func scaleRat(value *big.Rat, exponent int64) {
	if exponent == 0 {
		return
	}
	power := new(big.Int).Exp(tenInt, big.NewInt(absInt64(exponent)), nil)
	if exponent > 0 {
		value.Mul(value, new(big.Rat).SetInt(power))
	} else {
		value.Mul(value, new(big.Rat).SetFrac(oneInt, power))
	}
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func conditionName(status Status) string {
	switch {
	case status.DivisionByZero:
		return "division by zero"
	case status.Overflow:
		return "overflow"
	case status.Underflow:
		return "underflow"
	case status.Inexact:
		return "inexact result"
	case status.Rounded:
		return "rounded result"
	default:
		return fmt.Sprintf("status %+v", status)
	}
}
