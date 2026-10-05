package runtime

import (
	"fmt"

	"github.com/expr-lang/expr/decimal"
)

// ToDecimal converts decimal.Decimal and integer kinds to decimal.Decimal.
// Float kinds are intentionally not converted: mixing decimals and floats
// requires an explicit conversion via the decimal() builtin.
func ToDecimal(v any) (decimal.Decimal, bool) {
	switch x := v.(type) {
	case decimal.Decimal:
		return x, true
	case int:
		return decimal.FromInt64(int64(x)), true
	case int8:
		return decimal.FromInt64(int64(x)), true
	case int16:
		return decimal.FromInt64(int64(x)), true
	case int32:
		return decimal.FromInt64(int64(x)), true
	case int64:
		return decimal.FromInt64(x), true
	case uint:
		return decimal.FromUint64(uint64(x)), true
	case uint8:
		return decimal.FromUint64(uint64(x)), true
	case uint16:
		return decimal.FromUint64(uint64(x)), true
	case uint32:
		return decimal.FromUint64(uint64(x)), true
	case uint64:
		return decimal.FromUint64(x), true
	}
	return decimal.Decimal{}, false
}

func decimalOperands(op string, a, b any) (decimal.Decimal, decimal.Decimal) {
	da, aok := ToDecimal(a)
	db, bok := ToDecimal(b)
	if !aok || !bok {
		other := b
		if !aok {
			other = a
		}
		switch other.(type) {
		case float32, float64:
			panic(fmt.Sprintf(
				"invalid operation: decimal %s float (mixing decimal and float requires explicit conversion with decimal() or float())",
				op,
			))
		}
		panic(fmt.Sprintf("invalid operation: %T %s %T", a, op, b))
	}
	return da, db
}

// DecimalArith evaluates an arithmetic operator ("+", "-" or "*") on
// operands where at least one is a decimal. Integer operands are promoted
// to decimals. It returns ok=false if neither operand is a decimal, so
// the caller can fall back to the generic numeric implementation.
//
// This function is the single implementation used by both the VM and the
// compile-time constant folder.
func DecimalArith(op string, a, b any) (any, bool) {
	if _, ok := a.(decimal.Decimal); !ok {
		if _, ok := b.(decimal.Decimal); !ok {
			return nil, false
		}
	}
	da, db := decimalOperands(op, a, b)
	switch op {
	case "+":
		return da.Add(db), true
	case "-":
		return da.Sub(db), true
	case "*":
		return da.Mul(db), true
	}
	panic(fmt.Sprintf("invalid operation: %T %s %T", a, op, b))
}

// DecimalQuo evaluates a / b on operands where at least one is a decimal,
// rounding the result to the precision and rounding mode of ctx. Integer
// operands are promoted to decimals. It returns ok=false if neither
// operand is a decimal.
//
// This function is the single implementation used by both the VM and the
// compile-time constant folder.
func DecimalQuo(a, b any, ctx decimal.Context) (any, bool) {
	if _, ok := a.(decimal.Decimal); !ok {
		if _, ok := b.(decimal.Decimal); !ok {
			return nil, false
		}
	}
	da, db := decimalOperands("/", a, b)
	return da.Quo(db, ctx), true
}

// DecimalCompare evaluates a comparison operator ("==", "!=", "<", "<=",
// ">" or ">=") on operands where at least one is a decimal. Integer
// operands are promoted to decimals. It returns ok=false if neither
// operand is a decimal.
//
// This function is the single implementation used by both the VM and the
// compile-time constant folder.
func DecimalCompare(op string, a, b any) (bool, bool) {
	if _, ok := a.(decimal.Decimal); !ok {
		if _, ok := b.(decimal.Decimal); !ok {
			return false, false
		}
	}
	da, db := decimalOperands(op, a, b)
	cmp := da.Cmp(db)
	switch op {
	case "==":
		return cmp == 0, true
	case "!=":
		return cmp != 0, true
	case "<":
		return cmp < 0, true
	case "<=":
		return cmp <= 0, true
	case ">":
		return cmp > 0, true
	case ">=":
		return cmp >= 0, true
	}
	panic(fmt.Sprintf("invalid operation: %T %s %T", a, op, b))
}

// decimalEqual handles equality for decimals. Unlike DecimalCompare it
// does not panic for non-numeric types: a decimal is simply not equal to
// them. Mixing with floats still panics, as it requires an explicit
// conversion. It returns ok=false if neither operand is a decimal.
func decimalEqual(a, b any) (bool, bool) {
	if _, ok := a.(decimal.Decimal); !ok {
		if _, ok := b.(decimal.Decimal); !ok {
			return false, false
		}
	}
	da, aok := ToDecimal(a)
	db, bok := ToDecimal(b)
	if !aok || !bok {
		other := b
		if !aok {
			other = a
		}
		switch other.(type) {
		case float32, float64:
			panic(fmt.Sprintf(
				"invalid operation: decimal == float (mixing decimal and float requires explicit conversion with decimal() or float())",
			))
		}
		return false, true
	}
	return da.Cmp(db) == 0, true
}
