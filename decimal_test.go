package expr_test

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"
)

type decimalCase struct {
	name     string
	input    string
	scale    int
	rounding expr.Rounding
	want     string
}

func TestDecimalLiteralsAndComparisons(t *testing.T) {
	literalCases := map[string]string{
		".5d":       "0.5",
		"1d":        "1",
		"1_234.56d": "1234.56",
	}
	for input, want := range literalCases {
		tree, err := parser.Parse(input)
		require.NoError(t, err)
		literal, ok := tree.Node.(*ast.DecimalNode)
		require.True(t, ok)
		require.Equal(t, want, literal.Value.String())
	}

	tree, err := parser.Parse(`0x1d`)
	require.NoError(t, err)
	integer, ok := tree.Node.(*ast.IntegerNode)
	require.True(t, ok)
	require.Equal(t, 29, integer.Value)

	comparisons := []struct {
		input string
		want  bool
	}{
		{`1.00d == 1`, true},
		{`1.01d != 1`, true},
		{`-1.00d < 0`, true},
		{`2.5d >= 2`, true},
		{`1.004d == 1.00d`, true},
	}
	for _, tc := range comparisons {
		optimized, err := expr.Compile(tc.input)
		require.NoError(t, err)
		unoptimized, err := expr.Compile(tc.input, expr.Optimize(false))
		require.NoError(t, err)

		gotOptimized, err := expr.Run(optimized, nil)
		require.NoError(t, err)
		gotUnoptimized, err := expr.Run(unoptimized, nil)
		require.NoError(t, err)
		require.Equal(t, tc.want, gotOptimized)
		require.Equal(t, tc.want, gotUnoptimized)
	}
}

func TestDecimalDivisionByZero(t *testing.T) {
	for _, optimize := range []bool{true, false} {
		program, err := expr.Compile(`1.00d / 0`, expr.Optimize(optimize))
		if optimize {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			_, err = expr.Run(program, nil)
			require.Error(t, err)
		}

		program, err = expr.Compile(`amount / zero`, expr.Env(map[string]any{
			"amount": mustParseDecimal(t, "1.00"),
			"zero":   0,
		}), expr.Optimize(optimize))
		require.NoError(t, err)
		_, err = expr.Run(program, map[string]any{"amount": mustParseDecimal(t, "1.00"), "zero": 0})
		require.Error(t, err)
	}
}

func TestDecimalArithmetic(t *testing.T) {
	cases := []decimalCase{
		{name: "decimal addition", input: "0.10d + 0.20d", want: "0.30"},
		{name: "money multiplication", input: "19.99d * 3", want: "59.97"},
		{name: "integer decimal multiplication", input: "3 * 19.99d", want: "59.97"},
		{name: "money tax", input: "100.000d * 1.0005d", want: "100.05"},
		{name: "positive tie away from zero", input: "2.675d + 0d", want: "2.68"},
		{name: "negative tie away from zero", input: "-2.675d + 0d", want: "-2.68"},
		{name: "division recurring", input: "10.00d / 3", want: "3.33"},
		{name: "negative division recurring", input: "-1.00d / 3", want: "-0.33"},
		{name: "division tie", input: "0.05d / 2", want: "0.03"},
		{name: "negative division tie", input: "-0.05d / 2", want: "-0.03"},
		{name: "half up negative tie", input: "-2.675d + 0d", rounding: expr.RoundingHalfUp, want: "-2.67"},
		{name: "half down positive tie", input: "2.675d + 0d", rounding: expr.RoundingHalfDown, want: "2.67"},
		{name: "half even positive tie", input: "2.675d + 0d", rounding: expr.RoundingHalfEven, want: "2.68"},
		{name: "half even negative tie", input: "-2.675d + 0d", rounding: expr.RoundingHalfEven, want: "-2.68"},
		{name: "scale one near tie", input: "0.15d / 2", scale: 1, rounding: expr.RoundingHalfAwayFromZero, want: "0.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			optimized := compileDecimal(t, tc, true)
			unoptimized := compileDecimal(t, tc, false)

			_, folded := optimized.Node().(*ast.DecimalNode)
			require.True(t, folded)
			_, executed := unoptimized.Node().(*ast.BinaryNode)
			require.True(t, executed)

			gotOptimized, err := expr.Run(optimized, nil)
			require.NoError(t, err)
			gotUnoptimized, err := expr.Run(unoptimized, nil)
			require.NoError(t, err)

			require.Equal(t, tc.want, gotOptimized.(expr.Decimal).String())
			require.Equal(t, tc.want, gotUnoptimized.(expr.Decimal).String())
		})
	}
}

func TestDecimalRejectsFloatMixing(t *testing.T) {
	_, err := expr.Compile(`1.5d + 1.2`, expr.DecimalPrecision(2, expr.RoundingHalfAwayFromZero))
	require.Error(t, err)

	_, err = expr.Compile(`1.5d == 1.2`, expr.DecimalPrecision(2, expr.RoundingHalfAwayFromZero))
	require.Error(t, err)

	env := map[string]any{
		"amount": mustParseDecimal(t, "1.50"),
		"rate":   1.2,
	}
	_, err = expr.Compile(`amount * rate`, expr.Env(env))
	require.Error(t, err)

	program, err := expr.Compile(`amount * decimal(3)`, expr.Env(env))
	require.NoError(t, err)
	out, err := expr.Run(program, env)
	require.NoError(t, err)
	require.Equal(t, "4.50", out.(expr.Decimal).String())

	typeProgram, err := expr.Compile(`type(amount)`, expr.Env(env))
	require.NoError(t, err)
	typeName, err := expr.Run(typeProgram, env)
	require.NoError(t, err)
	require.Equal(t, "decimal", typeName)
}

func compileDecimal(t *testing.T, tc decimalCase, optimize bool) *vm.Program {
	t.Helper()
	rounding := tc.rounding
	if rounding == 0 {
		rounding = expr.RoundingHalfAwayFromZero
	}
	scale := tc.scale
	if scale == 0 {
		scale = 2
	}
	program, err := expr.Compile(tc.input, expr.Optimize(optimize), expr.DecimalPrecision(scale, rounding))
	require.NoError(t, err)
	return program
}

func mustParseDecimal(t *testing.T, input string) expr.Decimal {
	t.Helper()
	value, err := expr.ParseDecimal(input)
	require.NoError(t, err)
	return value
}
