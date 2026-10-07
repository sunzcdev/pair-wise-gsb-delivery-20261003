package decimal

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
)

// ratDot computes the exact dot product with big.Rat as a reference.
func ratDot(xs, ys []Decimal) *big.Rat {
	acc := new(big.Rat)
	for i := range xs {
		acc.Add(acc, new(big.Rat).Mul(xs[i].Rat(), ys[i].Rat()))
	}
	return acc
}

// ratRound rounds an exact reference value to places digits after the
// decimal point, mirroring the documented semantics of RoundingMode.
func ratRound(r *big.Rat, places int32, mode RoundingMode) Decimal {
	scale := new(big.Rat).SetInt(new(big.Int).Exp(tenInt, big.NewInt(int64(abs(places))), nil))
	if places < 0 {
		scale.Inv(scale)
	}
	scaled := new(big.Rat).Mul(r, scale)
	sign := scaled.Sign()
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(scaled.Num(), scaled.Denom(), rem)
	if rem.Sign() != 0 {
		den := scaled.Denom()
		switch mode {
		case RoundingUp:
			q.Add(q, big.NewInt(int64(sign)))
		case RoundingFloor:
			if sign < 0 {
				q.Sub(q, oneInt)
			}
		case RoundingCeil:
			if sign > 0 {
				q.Add(q, oneInt)
			}
		case RoundingHalfUp, RoundingHalfEven:
			twice := new(big.Int).Abs(rem)
			twice.Lsh(twice, 1)
			cmp := twice.Cmp(den)
			if cmp > 0 || cmp == 0 && (mode == RoundingHalfUp || q.Bit(0) == 1) {
				q.Add(q, big.NewInt(int64(sign)))
			}
		}
	}
	return NewFromBigInt(q, -places)
}

func strsToDecimals(t *testing.T, ss []string) []Decimal {
	t.Helper()
	ds := make([]Decimal, len(ss))
	for i, s := range ss {
		var err error
		ds[i], err = NewFromString(s)
		if err != nil {
			t.Fatalf("NewFromString(%q): %v", s, err)
		}
	}
	return ds
}

func TestFusedDotProductAgainstBigRat(t *testing.T) {
	huge := "123456789012345678901234567890.123456789"
	base := "98765432109876543210.987654321"
	tiny := New(1, -27)
	baseDec := RequireFromString(base)

	cases := []struct {
		name   string
		xs, ys []Decimal
		places int32
		mode   RoundingMode
	}{
		{
			// Two enormous products cancel almost completely, leaving a
			// small remainder: a*b - a*(b - 1e-27) = a * 1e-27.
			name:   "huge products cancel leaving small remainder",
			xs:     strsToDecimals(t, []string{huge, "-" + huge}),
			ys:     []Decimal{baseDec, baseDec.Sub(tiny)},
			places: 10,
			mode:   RoundingHalfUp,
		},
		{
			name:   "mixed signs and scales",
			xs:     strsToDecimals(t, []string{"0.1", "-22.5", "3000.007", "-0.00000001"}),
			ys:     strsToDecimals(t, []string{"0.2", "4", "-0.001", "123456789"}),
			places: 6,
			mode:   RoundingHalfUp,
		},
		{
			name:   "extreme positive and negative exponents",
			xs:     []Decimal{New(12345, 2000), New(-6789, -2000), New(1, 0)},
			ys:     []Decimal{New(1, 5), New(1, -5), New(-1, 0)},
			places: 3,
			mode:   RoundingHalfEven,
		},
		{
			name:   "zero terms interleaved",
			xs:     []Decimal{Zero, New(25, -2), New(0, -5000), New(-4, 0)},
			ys:     []Decimal{New(9, 9), New(4, 0), New(7, 0), New(3, -1)},
			places: 2,
			mode:   RoundingHalfUp,
		},
		{
			name:   "negative places",
			xs:     strsToDecimals(t, []string{"12345.678", "-9999.001"}),
			ys:     strsToDecimals(t, []string{"3", "2"}),
			places: -2,
			mode:   RoundingHalfUp,
		},
		{
			name:   "tie half up",
			xs:     strsToDecimals(t, []string{"0.125", "0.125"}),
			ys:     strsToDecimals(t, []string{"1", "1"}),
			places: 1,
			mode:   RoundingHalfUp,
		},
		{
			name:   "tie half even",
			xs:     strsToDecimals(t, []string{"0.125", "0.125"}),
			ys:     strsToDecimals(t, []string{"1", "1"}),
			places: 1,
			mode:   RoundingHalfEven,
		},
		{
			name:   "negative tie half even",
			xs:     strsToDecimals(t, []string{"-0.15"}),
			ys:     strsToDecimals(t, []string{"1"}),
			places: 1,
			mode:   RoundingHalfEven,
		},
		{
			name:   "towards zero",
			xs:     strsToDecimals(t, []string{"-1.999", "0.5"}),
			ys:     strsToDecimals(t, []string{"1", "1"}),
			places: 1,
			mode:   RoundingDown,
		},
		{
			name:   "away from zero",
			xs:     strsToDecimals(t, []string{"-1.901", "0.5"}),
			ys:     strsToDecimals(t, []string{"1", "1"}),
			places: 1,
			mode:   RoundingUp,
		},
		{
			name:   "towards negative infinity",
			xs:     strsToDecimals(t, []string{"-1.901", "0.5"}),
			ys:     strsToDecimals(t, []string{"1", "1"}),
			places: 1,
			mode:   RoundingFloor,
		},
		{
			name:   "towards positive infinity",
			xs:     strsToDecimals(t, []string{"-1.999", "0.5"}),
			ys:     strsToDecimals(t, []string{"1", "1"}),
			places: 1,
			mode:   RoundingCeil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := FusedContext{Places: tc.places, Rounding: tc.mode}
			got, err := ctx.DotProduct(tc.xs, tc.ys)
			if err != nil {
				t.Fatalf("DotProduct: %v", err)
			}
			want := ratRound(ratDot(tc.xs, tc.ys), tc.places, tc.mode)
			if !got.Equal(want) || got.Exponent() != want.Exponent() {
				t.Errorf("DotProduct = %s (exp %d), want %s (exp %d)",
					got, got.Exponent(), want, want.Exponent())
			}
		})
	}
}

func TestFusedFMAAgainstBigRat(t *testing.T) {
	cases := []struct {
		a, b, c string
		places  int32
		mode    RoundingMode
	}{
		{"1000000.0049", "1", "-999999.9999", 2, RoundingHalfUp},
		{"123456789.987654321", "-987654321.123456789", "0.000000001", 12, RoundingHalfUp},
		{"-3.5", "2", "7.25", 1, RoundingHalfEven},
		{"0.1", "0.2", "-0.03", 4, RoundingDown},
	}
	for _, tc := range cases {
		a, b, c := RequireFromString(tc.a), RequireFromString(tc.b), RequireFromString(tc.c)
		ctx := FusedContext{Places: tc.places, Rounding: tc.mode}
		got, err := ctx.FMA(a, b, c)
		if err != nil {
			t.Fatalf("FMA(%s, %s, %s): %v", tc.a, tc.b, tc.c, err)
		}
		exact := new(big.Rat).Add(new(big.Rat).Mul(a.Rat(), b.Rat()), c.Rat())
		want := ratRound(exact, tc.places, tc.mode)
		if !got.Equal(want) || got.Exponent() != want.Exponent() {
			t.Errorf("FMA(%s, %s, %s) = %s (exp %d), want %s (exp %d)",
				tc.a, tc.b, tc.c, got, got.Exponent(), want, want.Exponent())
		}
	}
}

func TestFusedEmptyDotProduct(t *testing.T) {
	ctx := FusedContext{Places: 4, Rounding: RoundingHalfUp}
	got, err := ctx.DotProduct(nil, nil)
	if err != nil {
		t.Fatalf("DotProduct(nil, nil): %v", err)
	}
	if !got.IsZero() {
		t.Errorf("empty dot product = %s, want zero", got)
	}
	if got.Exponent() != -4 {
		t.Errorf("empty dot product exponent = %d, want -4", got.Exponent())
	}
}

func TestFusedZeroTermExtremeExponentSkipped(t *testing.T) {
	// A zero factor with an absurd exponent must not consume the budget.
	ctx := FusedContext{Places: 2, Rounding: RoundingHalfUp, MaxDigits: 10}
	got, err := ctx.DotProduct(
		[]Decimal{New(0, math.MinInt32), New(1, 0)},
		[]Decimal{New(9, math.MaxInt32-1), New(2, 0)},
	)
	if err != nil {
		t.Fatalf("DotProduct: %v", err)
	}
	if want := New(200, -2); !got.Equal(want) {
		t.Errorf("DotProduct = %s, want %s", got, want)
	}
}

func TestFusedBudgetExceeded(t *testing.T) {
	ctx := FusedContext{Places: 0, Rounding: RoundingHalfUp, MaxDigits: 100}

	// Aligning 1e10000 with 1e-10000 needs a 20001-digit coefficient.
	_, err := ctx.DotProduct(
		[]Decimal{New(1, 10000), New(-1, -10000)},
		[]Decimal{New(1, 0), New(1, 0)},
	)
	if !errors.Is(err, ErrFusedBudgetExceeded) {
		t.Errorf("alignment: err = %v, want ErrFusedBudgetExceeded", err)
	}

	// Rounding 1e-10000 to 0 places needs a 10000-digit divisor.
	_, err = ctx.FMA(New(1, -10000), New(1, 0), Zero)
	if !errors.Is(err, ErrFusedBudgetExceeded) {
		t.Errorf("final rescale: err = %v, want ErrFusedBudgetExceeded", err)
	}

	// Raising the budget makes the same computations succeed.
	bigCtx := FusedContext{Places: 0, Rounding: RoundingHalfUp, MaxDigits: 100000}
	got, err := bigCtx.DotProduct(
		[]Decimal{New(1, 10000), New(-1, -10000)},
		[]Decimal{New(1, 0), New(1, 0)},
	)
	if err != nil {
		t.Fatalf("with larger budget: %v", err)
	}
	want := ratRound(ratDot(
		[]Decimal{New(1, 10000), New(-1, -10000)},
		[]Decimal{New(1, 0), New(1, 0)},
	), 0, RoundingHalfUp)
	if !got.Equal(want) {
		t.Errorf("with larger budget = %s, want %s", got, want)
	}
}

func TestFusedExponentOverflow(t *testing.T) {
	ctx := FusedContext{Places: 0, Rounding: RoundingHalfUp}
	_, err := ctx.FMA(New(1, math.MaxInt32), New(1, 1), Zero)
	if !errors.Is(err, ErrFusedExponentOverflow) {
		t.Errorf("FMA: err = %v, want ErrFusedExponentOverflow", err)
	}
	_, err = ctx.DotProduct([]Decimal{New(1, math.MinInt32)}, []Decimal{New(1, -1)})
	if !errors.Is(err, ErrFusedExponentOverflow) {
		t.Errorf("DotProduct: err = %v, want ErrFusedExponentOverflow", err)
	}
}

func TestFusedDotProductLengthMismatch(t *testing.T) {
	ctx := FusedContext{Places: 2, Rounding: RoundingHalfUp}
	_, err := ctx.DotProduct([]Decimal{New(1, 0)}, []Decimal{New(1, 0), New(2, 0)})
	if err == nil {
		t.Error("expected length mismatch error, got nil")
	}
}

func TestFusedInvalidRoundingMode(t *testing.T) {
	ctx := FusedContext{Places: 2, Rounding: RoundingMode(42)}
	_, err := ctx.FMA(New(1, 0), New(1, 0), Zero)
	if err == nil {
		t.Error("expected invalid rounding mode error, got nil")
	}
}

// ExampleFusedContext_DotProduct shows that rounding each product before
// summing accumulates error, while the fused dot product rounds only once.
func ExampleFusedContext_DotProduct() {
	price := RequireFromString("0.125")
	one := NewFromInt(1)
	xs := []Decimal{price, price, price}
	ys := []Decimal{one, one, one}

	ctx := FusedContext{Places: 2, Rounding: RoundingHalfUp}
	fused, err := ctx.DotProduct(xs, ys)
	if err != nil {
		panic(err)
	}

	naive := Zero
	for i := range xs {
		naive = naive.Add(xs[i].Mul(ys[i]).Round(2))
	}

	fmt.Println("round each term:", naive)
	fmt.Println("fused:", fused)
	// Output:
	// round each term: 0.39
	// fused: 0.38
}

// ExampleFusedContext_FMA shows a small balance that survives cancellation
// of two large amounts only when the final rounding happens once.
func ExampleFusedContext_FMA() {
	a := RequireFromString("1000000.0049")
	b := NewFromInt(1)
	c := RequireFromString("-999999.9999")

	ctx := FusedContext{Places: 2, Rounding: RoundingHalfUp}
	fused, err := ctx.FMA(a, b, c)
	if err != nil {
		panic(err)
	}

	// Rounding the product and the addend first wipes out the balance.
	naive := a.Mul(b).Round(2).Add(c.Round(2))

	fmt.Println("round then add:", naive)
	fmt.Println("fused:", fused)
	// Output:
	// round then add: 0
	// fused: 0.01
}
