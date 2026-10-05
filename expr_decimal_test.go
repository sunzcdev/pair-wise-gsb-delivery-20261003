package expr_test

import (
	"fmt"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/decimal"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
)

// Money cases that are famously wrong in binary floating point.
func TestDecimal_money(t *testing.T) {
	cases := []struct {
		expr   string
		expect string
	}{
		{`0.1d + 0.2d`, "0.3"},          // float64: 0.30000000000000004
		{`0.1d + 0.2d == 0.3d`, "true"}, // float64: false
		{`0.1d + 0.7d == 0.8d`, "true"},
		{`19.99d * 3`, "59.97"},
		{`100d * 0.0825d`, "8.2500"}, // sales tax on $100 at 8.25%
		{`4.015d * 100d`, "401.500"},
		{`round(2.675d, 2)`, "2.68"}, // float64 round(2.675, 2) is 2.67
		{`round(4.015d, 2)`, "4.02"},
		{`0.1d * 10 == 1.0d`, "true"},
		{`10.5d - 0.05d`, "10.45"},
		{`1.03d + 0.07d`, "1.10"},
	}
	for _, c := range cases {
		out, err := expr.Eval(c.expr, nil)
		require.NoError(t, err, c.expr)
		assert.Equal(t, c.expect, fmt.Sprintf("%v", out), c.expr)
	}

	// The same expressions in float64 for contrast.
	out, err := expr.Eval(`0.1 + 0.2 == 0.3`, nil)
	require.NoError(t, err)
	assert.Equal(t, false, out)
}

// Negative rounding: division results and explicit rounding of negative
// halves must follow the configured rounding mode.
func TestDecimal_negative_rounding(t *testing.T) {
	cases := []struct {
		expr   string
		prec   int
		mode   decimal.RoundingMode
		expect string
	}{
		{`-1d / 3`, 2, decimal.RoundHalfUp, "-0.33"},
		{`-1d / 3`, 2, decimal.RoundHalfEven, "-0.33"},
		{`-1d / 3`, 2, decimal.RoundDown, "-0.33"},
		{`-1d / 3`, 2, decimal.RoundUp, "-0.34"},
		{`-1d / 3`, 2, decimal.RoundCeiling, "-0.33"},
		{`-1d / 3`, 2, decimal.RoundFloor, "-0.34"},
		{`-5d / 2`, 0, decimal.RoundHalfUp, "-3"},
		{`-5d / 2`, 0, decimal.RoundHalfEven, "-2"},
		{`-5d / 2`, 0, decimal.RoundDown, "-2"},
		{`-5d / 2`, 0, decimal.RoundUp, "-3"},
		{`-5d / 2`, 0, decimal.RoundCeiling, "-2"},
		{`-5d / 2`, 0, decimal.RoundFloor, "-3"},
		{`5d / 2`, 0, decimal.RoundHalfUp, "3"},
		{`5d / 2`, 0, decimal.RoundHalfEven, "2"},
	}
	for _, c := range cases {
		program, err := expr.Compile(c.expr, expr.DecimalContext(c.prec, c.mode))
		require.NoError(t, err, c.expr)
		out, err := expr.Run(program, nil)
		require.NoError(t, err, c.expr)
		assert.Equal(t, c.expect, fmt.Sprintf("%v", out), "%s (%v, %v)", c.expr, c.prec, c.mode)
	}

	// Explicit rounding builtins on negative values.
	builtins := []struct {
		expr   string
		expect string
	}{
		{`round(-2.5d)`, "-3"}, // half away from zero, like math.Round
		{`round(-2.4d)`, "-2"},
		{`round(-2.675d, 2)`, "-2.68"},
		{`ceil(-2.5d)`, "-2"},
		{`floor(-2.5d)`, "-3"},
		{`abs(-2.5d)`, "2.5"},
	}
	for _, c := range builtins {
		out, err := expr.Eval(c.expr, nil)
		require.NoError(t, err, c.expr)
		assert.Equal(t, c.expect, fmt.Sprintf("%v", out), c.expr)
	}
}

func TestDecimal_division(t *testing.T) {
	// Default context: 16 fractional places, half-up.
	out, err := expr.Eval(`1d / 3`, nil)
	require.NoError(t, err)
	assert.Equal(t, "0.3333333333333333", fmt.Sprintf("%v", out))

	program, err := expr.Compile(`10d / 4`, expr.DecimalContext(2, decimal.RoundHalfUp))
	require.NoError(t, err)
	out, err = expr.Run(program, nil)
	require.NoError(t, err)
	assert.Equal(t, "2.50", fmt.Sprintf("%v", out))

	// Division by zero is an error at compile time (folded) ...
	_, err = expr.Compile(`1d / 0`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "division by zero")

	// ... and the same error at runtime with optimization disabled.
	program, err = expr.Compile(`1d / 0`, expr.Optimize(false))
	require.NoError(t, err)
	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "division by zero")
}

// Integers combine with decimals by exact promotion; floats must be
// converted explicitly.
func TestDecimal_int_and_float_mixing(t *testing.T) {
	cases := []struct {
		expr   string
		expect string
	}{
		{`2 * 1.5d`, "3.0"},
		{`1.5d + 2`, "3.5"},
		{`2 - 0.5d`, "1.5"},
		{`10 / 4d`, "2.5000000000000000"},
		{`1.5d > 1`, "true"},
		{`2 >= 2.0d`, "true"},
		{`1.50d == 1.5d`, "true"},
		{`-1.5d + 1d`, "-0.5"},
		{`decimal(1.5) + 1.5d`, "3.0"}, // explicit float -> decimal
		{`float(1.5d) + 1.5`, "3"},     // explicit decimal -> float
		{`decimal("19.99") * 2`, "39.98"},
		{`int(1.9d)`, "1"},
		{`int(-1.9d)`, "-1"},
		{`string(1.50d)`, "1.50"},
	}
	for _, c := range cases {
		out, err := expr.Eval(c.expr, nil)
		require.NoError(t, err, c.expr)
		assert.Equal(t, c.expect, fmt.Sprintf("%v", out), c.expr)
	}

	// Mixing decimals and floats without conversion is a compile-time error.
	for _, code := range []string{
		`1.5d + 1.5`, `1.5 + 1.5d`, `1.5d - 0.5`, `1.5d * 2.0`,
		`1.5d / 0.5`, `1.5d == 1.5`, `1.5d != 1.5`, `1.5d < 1.6`,
		`1.5d <= 1.5`, `1.5d > 1.4`, `1.5d >= 1.5`,
	} {
		_, err := expr.Compile(code)
		require.Error(t, err, code)
		assert.Contains(t, err.Error(), "mixing decimal and float", code)
	}

	// The same mixing is also an error at runtime without type checking.
	_, err := expr.Eval(`1.5d + 1.5`, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mixing decimal and float")

	// Operators that are not defined for decimals.
	for _, code := range []string{`1.5d % 2`, `1.5d ** 2`, `1.5d ^ 2`} {
		_, err := expr.Compile(code)
		require.Error(t, err, code)
	}
}

func TestDecimal_env(t *testing.T) {
	env := map[string]any{
		"price": decimal.MustParse("19.99"),
		"qty":   3,
		"rate":  0.0825,
	}
	out, err := expr.Eval(`price * qty`, env)
	require.NoError(t, err)
	assert.Equal(t, "59.97", fmt.Sprintf("%v", out))

	// A float from the environment must be converted explicitly.
	_, err = expr.Eval(`price * rate`, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mixing decimal and float")

	out, err = expr.Eval(`price * decimal(rate)`, env)
	require.NoError(t, err)
	assert.Equal(t, "1.649175", fmt.Sprintf("%v", out))

	// Membership and comparisons use decimal equality: 1.50 == 1.5.
	out, err = expr.Eval(`1.50d in [1.5d, 2.5d]`, nil)
	require.NoError(t, err)
	assert.Equal(t, true, out)

	// A decimal is not equal to nil or other types, but never panics.
	out, err = expr.Eval(`1.5d == nil`, nil)
	require.NoError(t, err)
	assert.Equal(t, false, out)
	out, err = expr.Eval(`1.5d in ["a", "b"]`, nil)
	require.NoError(t, err)
	assert.Equal(t, false, out)
}

// Decimals in a typed environment are recognized by the type checker.
func TestDecimal_typed_env(t *testing.T) {
	type Env struct {
		Price decimal.Decimal
		Qty   int
	}
	program, err := expr.Compile(`Price * Qty + 0.01d`, expr.Env(Env{}))
	require.NoError(t, err)
	out, err := expr.Run(program, Env{Price: decimal.MustParse("19.99"), Qty: 2})
	require.NoError(t, err)
	assert.Equal(t, "39.99", fmt.Sprintf("%v", out))

	// Mixing a typed decimal with a float is rejected at compile time.
	_, err = expr.Compile(`Price + 0.5`, expr.Env(Env{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mixing decimal and float")
}

// Compile-time folding and runtime execution share one implementation:
// results must be identical with optimization on and off.
func TestDecimal_optimize_on_off_identical(t *testing.T) {
	expressions := []string{
		`0.1d + 0.2d`,
		`0.1d + 0.2d == 0.3d`,
		`19.99d * 3`,
		`100d * 0.0825d`,
		`1d / 3`,
		`10d / 4`,
		`-1d / 3`,
		`-5d / 2`,
		`2 * 1.5d + 10d / 4`,
		`(1.5d + 2) * 3 - 0.05d`,
		`1.5d > 1`,
		`1.50d == 1.5d`,
		`-1.5d + 1d`,
		`abs(-2.5d) + round(2.675d, 2)`,
	}
	contexts := []decimal.Context{
		decimal.DefaultContext,
		{Precision: 2, Rounding: decimal.RoundHalfUp},
		{Precision: 2, Rounding: decimal.RoundDown},
		{Precision: 2, Rounding: decimal.RoundFloor},
		{Precision: 0, Rounding: decimal.RoundHalfEven},
	}
	for _, code := range expressions {
		for _, ctx := range contexts {
			var results []string
			for _, optimize := range []bool{true, false} {
				program, err := expr.Compile(code,
					expr.Optimize(optimize),
					expr.DecimalContext(int(ctx.Precision), ctx.Rounding),
				)
				require.NoError(t, err, code)
				out, err := expr.Run(program, nil)
				require.NoError(t, err, code)
				results = append(results, fmt.Sprintf("%v", out))
			}
			assert.Equal(t, results[0], results[1],
				"optimize on/off mismatch for %s with context %+v", code, ctx)
		}
	}
}

// The constant folder must actually fold decimal literals at compile time.
func TestDecimal_folded_at_compile_time(t *testing.T) {
	program, err := expr.Compile(`0.1d + 0.2d`)
	require.NoError(t, err)
	node, ok := program.Node().(*ast.DecimalNode)
	require.True(t, ok, "expected *ast.DecimalNode, got %T", program.Node())
	assert.Equal(t, "0.3", node.Value.String())

	program, err = expr.Compile(`1d / 3`, expr.DecimalContext(4, decimal.RoundHalfUp))
	require.NoError(t, err)
	node, ok = program.Node().(*ast.DecimalNode)
	require.True(t, ok, "expected *ast.DecimalNode, got %T", program.Node())
	assert.Equal(t, "0.3333", node.Value.String())

	// With optimization disabled the expression stays a binary node ...
	program, err = expr.Compile(`1d / 3`,
		expr.Optimize(false),
		expr.DecimalContext(4, decimal.RoundHalfUp),
	)
	require.NoError(t, err)
	_, ok = program.Node().(*ast.BinaryNode)
	require.True(t, ok, "expected *ast.BinaryNode, got %T", program.Node())

	// ... but evaluates to the same value.
	out, err := expr.Run(program, nil)
	require.NoError(t, err)
	assert.Equal(t, "0.3333", fmt.Sprintf("%v", out))
}
