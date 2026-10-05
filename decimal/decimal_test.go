package decimal_test

import (
	"testing"

	"github.com/expr-lang/expr/decimal"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
)

func TestParse_String_roundtrip(t *testing.T) {
	cases := []string{
		"0", "1", "-1", "1.5", "-1.5", "0.1", "0.10", "19.99",
		"12345678901234567890.123456789", "0.000001", "1000",
	}
	for _, s := range cases {
		d, err := decimal.Parse(s)
		require.NoError(t, err, s)
		assert.Equal(t, s, d.String(), s)
	}
}

func TestParse_exponent(t *testing.T) {
	assert.Equal(t, "1500", decimal.MustParse("1.5e3").String())
	assert.Equal(t, "0.015", decimal.MustParse("1.5e-2").String())
	assert.Equal(t, "0.5", decimal.MustParse(".5").String())
	assert.Equal(t, "5", decimal.MustParse("5.").String())
	assert.Equal(t, "-0.5", decimal.MustParse("-.5").String())
}

func TestParse_invalid(t *testing.T) {
	for _, s := range []string{"", "abc", "1.2.3", "1e", "e3", "--1", "1..2"} {
		_, err := decimal.Parse(s)
		assert.Error(t, err, s)
	}
}

func TestDecimal_arithmetic_exact(t *testing.T) {
	// The classic float64 failure: 0.1 + 0.2 != 0.3.
	sum := decimal.MustParse("0.1").Add(decimal.MustParse("0.2"))
	assert.Equal(t, "0.3", sum.String())
	assert.Equal(t, 0, sum.Cmp(decimal.MustParse("0.3")))

	assert.Equal(t, "59.97", decimal.MustParse("19.99").Mul(decimal.MustParse("3")).String())
	assert.Equal(t, "8.2500", decimal.MustParse("100").Mul(decimal.MustParse("0.0825")).String())
	assert.Equal(t, "0.30000000000000000000", decimal.MustParse("0.1").Add(decimal.MustParse("0.2")).Add(decimal.MustParse("0.00000000000000000000")).String())
	assert.Equal(t, "-1.0", decimal.MustParse("0.1").Sub(decimal.MustParse("1.1")).String())
}

func TestDecimal_Quo(t *testing.T) {
	ctx := decimal.Context{Precision: 2, Rounding: decimal.RoundHalfUp}
	assert.Equal(t, "0.33", decimal.MustParse("1").Quo(decimal.MustParse("3"), ctx).String())
	assert.Equal(t, "2.50", decimal.MustParse("10").Quo(decimal.MustParse("4"), ctx).String())
	assert.Equal(t, "3.33", decimal.MustParse("10").Quo(decimal.MustParse("3"), ctx).String())

	// Exact division is not rounded away.
	assert.Equal(t, "2.5000", decimal.MustParse("10").Quo(decimal.MustParse("4"),
		decimal.Context{Precision: 4, Rounding: decimal.RoundHalfUp}).String())

	// Division by zero panics.
	assert.Panics(t, func() {
		decimal.MustParse("1").Quo(decimal.MustParse("0"), ctx)
	})
}

func TestDecimal_Quo_negative_rounding(t *testing.T) {
	one := decimal.MustParse("-1")
	three := decimal.MustParse("3")
	// -1/3 = -0.3333...
	assert.Equal(t, "-0.33", one.Quo(three, decimal.Context{Precision: 2, Rounding: decimal.RoundHalfUp}).String())
	assert.Equal(t, "-0.33", one.Quo(three, decimal.Context{Precision: 2, Rounding: decimal.RoundHalfEven}).String())
	assert.Equal(t, "-0.33", one.Quo(three, decimal.Context{Precision: 2, Rounding: decimal.RoundDown}).String())
	assert.Equal(t, "-0.34", one.Quo(three, decimal.Context{Precision: 2, Rounding: decimal.RoundUp}).String())
	assert.Equal(t, "-0.33", one.Quo(three, decimal.Context{Precision: 2, Rounding: decimal.RoundCeiling}).String())
	assert.Equal(t, "-0.34", one.Quo(three, decimal.Context{Precision: 2, Rounding: decimal.RoundFloor}).String())
}

func TestDecimal_Round_negative_halves(t *testing.T) {
	cases := []struct {
		input  string
		mode   decimal.RoundingMode
		expect string
	}{
		{"-2.5", decimal.RoundHalfUp, "-3"},
		{"-2.5", decimal.RoundHalfEven, "-2"},
		{"-3.5", decimal.RoundHalfEven, "-4"},
		{"-2.5", decimal.RoundDown, "-2"},
		{"-2.5", decimal.RoundUp, "-3"},
		{"-2.5", decimal.RoundCeiling, "-2"},
		{"-2.5", decimal.RoundFloor, "-3"},
		{"2.5", decimal.RoundHalfUp, "3"},
		{"2.5", decimal.RoundHalfEven, "2"},
		{"3.5", decimal.RoundHalfEven, "4"},
		{"2.5", decimal.RoundDown, "2"},
		{"2.5", decimal.RoundUp, "3"},
		{"2.5", decimal.RoundCeiling, "3"},
		{"2.5", decimal.RoundFloor, "2"},
	}
	for _, c := range cases {
		got := decimal.MustParse(c.input).Round(0, c.mode)
		assert.Equal(t, c.expect, got.String(), "%s %s", c.input, c.mode)
	}
}

func TestDecimal_Round_places(t *testing.T) {
	assert.Equal(t, "4.02", decimal.MustParse("4.015").Round(2, decimal.RoundHalfUp).String())
	assert.Equal(t, "4.01", decimal.MustParse("4.015").Round(2, decimal.RoundDown).String())
	assert.Equal(t, "4.02", decimal.MustParse("4.014").Round(2, decimal.RoundUp).String())
	assert.Equal(t, "4.01", decimal.MustParse("4.014").Round(2, decimal.RoundDown).String())
	assert.Equal(t, "-4.02", decimal.MustParse("-4.015").Round(2, decimal.RoundHalfUp).String())
	assert.Equal(t, "-4.01", decimal.MustParse("-4.015").Round(2, decimal.RoundDown).String())
	// Rounding to more places than the scale is a no-op.
	assert.Equal(t, "4.01", decimal.MustParse("4.01").Round(5, decimal.RoundHalfUp).String())
}

func TestDecimal_Cmp(t *testing.T) {
	assert.Equal(t, 0, decimal.MustParse("1.0").Cmp(decimal.MustParse("1.00")))
	assert.Equal(t, -1, decimal.MustParse("0.3").Cmp(decimal.MustParse("0.30000000000000004")))
	assert.Equal(t, 1, decimal.MustParse("2").Cmp(decimal.MustParse("-2")))
	assert.Equal(t, 0, decimal.MustParse("0").Cmp(decimal.MustParse("-0.00")))
	assert.Equal(t, -1, decimal.MustParse("-0.01").Cmp(decimal.MustParse("0")))
}

func TestDecimal_Convert(t *testing.T) {
	d, err := decimal.Convert(42)
	require.NoError(t, err)
	assert.Equal(t, "42", d.String())

	d, err = decimal.Convert(uint64(1<<63 + 1))
	require.NoError(t, err)
	assert.Equal(t, "9223372036854775809", d.String())

	d, err = decimal.Convert("19.99")
	require.NoError(t, err)
	assert.Equal(t, "19.99", d.String())

	// Float conversion uses the shortest round-trip representation.
	d, err = decimal.Convert(0.1)
	require.NoError(t, err)
	assert.Equal(t, "0.1", d.String())

	_, err = decimal.Convert(true)
	assert.Error(t, err)
}

func TestDecimal_Float64_Int64(t *testing.T) {
	assert.Equal(t, 0.3, decimal.MustParse("0.3").Float64())
	assert.Equal(t, int64(-2), decimal.MustParse("-2.5").Int64())
	assert.Equal(t, int64(2), decimal.MustParse("2.5").Int64())
}
