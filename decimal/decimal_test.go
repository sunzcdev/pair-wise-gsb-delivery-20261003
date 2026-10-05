package decimal

import (
	"testing"

	"github.com/expr-lang/expr/internal/testify/require"
)

func TestParseAndString(t *testing.T) {
	cases := map[string]string{
		"1":        "1",
		"1.20":     "1.20",
		".50":      "0.50",
		"1_234.56": "1234.56",
		"1.2e2":    "120",
		"1.2e-2":   "0.012",
		"-1.20":    "-1.20",
	}
	for input, want := range cases {
		value, err := Parse(input)
		require.NoError(t, err)
		require.Equal(t, want, value.String())
	}
}

func TestRoundingModes(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		rounding Rounding
		want     string
	}{
		{"up positive", "1.21", Up, "1.3"},
		{"up negative", "-1.21", Up, "-1.3"},
		{"down positive", "1.29", Down, "1.2"},
		{"down negative", "-1.29", Down, "-1.2"},
		{"ceiling positive", "1.21", Ceiling, "1.3"},
		{"ceiling negative", "-1.29", Ceiling, "-1.2"},
		{"floor positive", "1.29", Floor, "1.2"},
		{"floor negative", "-1.21", Floor, "-1.3"},
		{"half up negative tie", "-1.25", HalfUp, "-1.2"},
		{"half down positive tie", "1.25", HalfDown, "1.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := Parse(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.want, input.Rescale(1, tc.rounding).String())
		})
	}
}

func TestDivideByZero(t *testing.T) {
	_, err := Divide(MustParse("1.00"), 0, DefaultSettings())
	require.Error(t, err)
}

func MustParse(input string) Decimal {
	value, err := Parse(input)
	if err != nil {
		panic(err)
	}
	return value
}
