package expr

import (
	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/decimal"
)

type Decimal = decimal.Decimal
type Rounding = decimal.Rounding

func ParseDecimal(input string) (Decimal, error) {
	return decimal.Parse(input)
}

const (
	RoundingHalfUp           Rounding = decimal.HalfUp
	RoundingHalfDown         Rounding = decimal.HalfDown
	RoundingHalfEven         Rounding = decimal.HalfEven
	RoundingHalfAwayFromZero Rounding = decimal.HalfAwayFromZero
	RoundingUp               Rounding = decimal.Up
	RoundingDown             Rounding = decimal.Down
	RoundingCeiling          Rounding = decimal.Ceiling
	RoundingFloor            Rounding = decimal.Floor
)

// DecimalPrecision sets fixed-point decimal scale and rounding mode.
func DecimalPrecision(scale int, rounding Rounding) Option {
	return func(c *conf.Config) {
		if scale < 0 {
			panic("expr: decimal scale must not be negative")
		}
		if rounding < 0 || rounding > Rounding(decimal.Floor) {
			panic("expr: invalid decimal rounding")
		}
		c.DecimalScale = scale
		c.DecimalRounding = rounding
	}
}
