package decimal

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func mustDec(s string) Decimal {
	d, err := NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func halfUp(p int32) FusedOptions {
	return FusedOptions{Precision: p, Rounding: RoundHalfUp}
}

// Table of explicit results for every rounding mode. The exact value is
// value * 10^exp and the result must be an exact multiple of 10^(-precision).
func TestFusedRoundingModes(t *testing.T) {
	tests := []struct {
		name      string
		value     int64
		exp       int32
		precision int32
		mode      RoundingMode
		want      string
	}{
		{"half up positive tie", 25, -2, 1, RoundHalfUp, "0.3"},
		{"half up negative tie", -25, -2, 1, RoundHalfUp, "-0.3"},
		{"half down positive tie", 25, -2, 1, RoundHalfDown, "0.2"},
		{"half down negative tie", -25, -2, 1, RoundHalfDown, "-0.2"},
		{"half down above half", 26, -2, 1, RoundHalfDown, "0.3"},
		{"half even rounds to even", 25, -2, 1, RoundHalfEven, "0.2"},
		{"half even rounds from odd", 35, -2, 1, RoundHalfEven, "0.4"},
		{"half even negative tie", -35, -2, 1, RoundHalfEven, "-0.4"},
		{"ceil positive", 21, -2, 1, RoundFusedCeil, "0.3"},
		{"ceil negative", -21, -2, 1, RoundFusedCeil, "-0.2"},
		{"floor positive", 21, -2, 1, RoundFusedFloor, "0.2"},
		{"floor negative", -21, -2, 1, RoundFusedFloor, "-0.3"},
		{"down positive", 29, -2, 1, RoundFusedDown, "0.2"},
		{"down negative", -29, -2, 1, RoundFusedDown, "-0.2"},

		// Magnitude below one result unit: only directed modes leave zero.
		{"subunit ceil", 1, -100, 2, RoundFusedCeil, "0.01"},
		{"subunit ceil negative", -1, -100, 2, RoundFusedCeil, "0"},
		{"subunit floor", 1, -100, 2, RoundFusedFloor, "0"},
		{"subunit floor negative", -1, -100, 2, RoundFusedFloor, "-0.01"},
		{"subunit half up", 49, -100, 2, RoundHalfUp, "0"},

		// Negative precision rounds the integer part.
		{"negative precision half up", 1251, 0, -2, RoundHalfUp, "1300"},
		{"negative precision half even", 1250, 0, -2, RoundHalfEven, "1200"},

		// Coarser-than-requested exponent is returned without zero padding.
		{"coarse exponent kept", 7, 3, 2, RoundHalfUp, "7000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fusedRound(big.NewInt(tt.value), tt.exp,
				FusedOptions{Precision: tt.precision, Rounding: tt.mode, MaxDigits: DefaultFusedMaxDigits})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("want %s, got %s", tt.want, got.String())
			}
		})
	}
}

func TestFusedMultiplyAddBasic(t *testing.T) {
	got, err := FusedMultiplyAdd(mustDec("2.5"), mustDec("4"), mustDec("1.25"), halfUp(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "11.25" {
		t.Fatalf("want 11.25, got %s", got.String())
	}

	// A zero addend is simply ignored; the product stands alone.
	got, err = FusedMultiplyAdd(mustDec("3"), mustDec("3.5"), Zero, halfUp(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "10.5" {
		t.Fatalf("want 10.5, got %s", got.String())
	}
}

// Huge positive and negative products cancel to a tiny residual. Rounding once
// keeps the residual; per-term rounding erases it.
func TestFusedMultiplyAddCancellation(t *testing.T) {
	x := NewFromInt(2)
	y := mustDec("500000000000000000.002")
	add := mustDec("-999999999999999999.998")

	got, err := FusedMultiplyAdd(x, y, add, halfUp(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "0.01" {
		t.Fatalf("fused want 0.01, got %s", got.String())
	}

	naive := x.Mul(y).Round(2).Add(add.Round(2)).Round(2)
	if naive.String() != "0" {
		t.Fatalf("naive baseline want 0, got %s", naive.String())
	}
	if got.Cmp(naive) == 0 {
		t.Fatal("fused result unexpectedly agrees with per-term rounding")
	}
}

func TestDotProductBasic(t *testing.T) {
	a := []Decimal{mustDec("1.1"), mustDec("2.2"), mustDec("3.3")}
	b := []Decimal{mustDec("1"), mustDec("2"), mustDec("3")}

	got, err := DotProduct(a, b, halfUp(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1.1*1 + 2.2*2 + 3.3*3 = 15.4
	if got.String() != "15.4" {
		t.Fatalf("want 15.4, got %s", got.String())
	}
}

// Large opposite products cancel; two small products decide the surviving
// cent. Demonstrates the difference versus rounding every product first.
func TestDotProductCancellation(t *testing.T) {
	a := []Decimal{
		mustDec("1000000.004"),
		mustDec("999999.996"),
	}
	b := []Decimal{
		NewFromInt(1),
		NewFromInt(-1),
	}

	got, err := DotProduct(a, b, halfUp(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Each difference is below half a cent, so per-term rounding erases both,
	// yet the exact total is 0.008, which rounds up to a surviving 0.01.
	if got.String() != "0.01" {
		t.Fatalf("fused want 0.01, got %s", got.String())
	}

	naive := Zero
	for i := range a {
		naive = naive.Add(a[i].Mul(b[i]).Round(2))
	}
	naive = naive.Round(2)
	if !naive.IsZero() {
		t.Fatalf("naive baseline want 0, got %s", naive.String())
	}
	if got.Cmp(naive) == 0 {
		t.Fatal("fused result unexpectedly agrees with per-term rounding")
	}
}

// A two-term giant cancellation that leaves a small positive residual.
func TestDotProductHugeCancellation(t *testing.T) {
	// Build the exact products directly to avoid hand-counting huge strings:
	//   term1 = 2 * 10^30
	//   term2 = -2 * 10^30 + 10^-12
	// so their exact sum is the tiny residual 10^-12.
	term1 := New(1, 30).Mul(NewFromInt(2))
	term2 := New(1, -12).Sub(term1)
	a := []Decimal{term1, term2}
	b := []Decimal{NewFromInt(1), NewFromInt(1)}

	got, err := DotProduct(a, b, halfUp(6))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Exact residual is 1e-12, which rounds to 0 at 6 dp.
	if got.Sign() != 0 {
		t.Fatalf("want 0 residual at 6dp, got %s", got.String())
	}

	got, err = DotProduct(a, b, halfUp(12))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.String() != "0.000000000001" {
		t.Fatalf("want residual 1e-12, got %s", got.String())
	}

	// Rounding each product to 6 dp before summing wipes the 1e-12 residual.
	naive := Zero
	for _, term := range a {
		naive = naive.Add(term.Round(6))
	}
	naive = naive.Round(6)
	if naive.Sign() != 0 || got.Cmp(naive) == 0 {
		t.Fatalf("per-term baseline should erase residual, got %s", naive.String())
	}
}

func TestDotProductEmptyAndZeros(t *testing.T) {
	got, err := DotProduct(nil, nil, halfUp(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("empty dot product want zero, got %s", got.String())
	}

	// Zero terms carry extreme exponents but must never force alignment.
	a := []Decimal{Zero, mustDec("1000000000000000000000000"), New(0, -100000)}
	b := []Decimal{mustDec("0.000000001"), Zero, New(1, 100000)}
	got, err = DotProduct(a, b, halfUp(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("all-zero dot product want zero, got %s", got.String())
	}

	_, err = DotProduct([]Decimal{NewFromInt(1)}, []Decimal{NewFromInt(1), NewFromInt(2)}, halfUp(2))
	if err == nil {
		t.Fatal("expected length mismatch error")
	}
}

// Cross-check both fused operations against exact big.Rat arithmetic.
func TestFusedAgainstBigRat(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	ratMulAdd := func(x, y, add Decimal, precision int32) Decimal {
		r := new(big.Rat).Add(
			new(big.Rat).Mul(x.Rat(), y.Rat()),
			add.Rat())
		return NewFromBigRat(r, precision)
	}
	ratDot := func(a, b []Decimal, precision int32) Decimal {
		r := new(big.Rat)
		for i := range a {
			r.Add(r, new(big.Rat).Mul(a[i].Rat(), b[i].Rat()))
		}
		return NewFromBigRat(r, precision)
	}

	// Generic random inputs.
	for iter := 0; iter < 200; iter++ {
		precision := int32(rng.Intn(5))
		x := New(int64(rng.Intn(20000)-10000), int32(rng.Intn(5))-2)
		y := New(int64(rng.Intn(20000)-10000), int32(rng.Intn(5))-2)
		add := New(int64(rng.Intn(20000)-10000), int32(rng.Intn(5))-2)

		got, err := FusedMultiplyAdd(x, y, add, halfUp(precision))
		if err != nil {
			t.Fatalf("iter %d unexpected error: %v", iter, err)
		}
		want := ratMulAdd(x, y, add, precision)
		if got.Cmp(want) != 0 {
			t.Fatalf("iter %d fma want %s, got %s", iter, want.String(), got.String())
		}
	}

	// Cancellation-focused random inputs: large opposite products plus small
	// terms, constructed to keep every big.Rat a finite decimal.
	for iter := 0; iter < 200; iter++ {
		precision := int32(2)
		n := 2 + rng.Intn(3)
		a := make([]Decimal, n)
		b := make([]Decimal, n)

		base := New(int64(1+rng.Intn(1000000)), int32(rng.Intn(6)))
		// First term: base * scale.
		a[0] = base
		b[0] = NewFromInt(2)
		// Second term nearly cancels: (-base + tail) * (2 + unit).
		tail := New(int64(rng.Intn(2000)-1000), -int32(1+rng.Intn(4)))
		a[1] = tail.Sub(base)
		b[1] = mustDec("2.0001")
		for j := 2; j < n; j++ {
			a[j] = New(int64(rng.Intn(2000)-1000), -int32(rng.Intn(3)))
			b[j] = New(int64(rng.Intn(2000)-1000), -int32(rng.Intn(3)))
		}

		got, err := DotProduct(a, b, halfUp(precision))
		if err != nil {
			t.Fatalf("iter %d unexpected error: %v", iter, err)
		}
		want := ratDot(a, b, precision)
		if got.Cmp(want) != 0 {
			t.Fatalf("iter %d dot want %s, got %s", iter, want.String(), got.String())
		}
	}
}

func TestFusedBudgetErrors(t *testing.T) {
	// Product itself exceeds the budget.
	_, err := FusedMultiplyAdd(New(1, 0), New(1, 0), Zero, FusedOptions{
		Precision: 0, Rounding: RoundHalfUp, MaxDigits: 0,
	})
	if err != nil {
		t.Fatalf("1x1 within default budget, got %v", err)
	}

	big1 := New(1, 50) // 10^50; the product is 10^100 (101 digits)
	_, err = FusedMultiplyAdd(big1, big1, Zero, FusedOptions{
		Precision: 2, Rounding: RoundHalfUp, MaxDigits: 50,
	})
	if !errors.Is(err, ErrFusedBudgetExceeded) {
		t.Fatalf("product budget: want ErrFusedBudgetExceeded, got %v", err)
	}

	// Exponent alignment exceeds the budget (10^1000 + 10^-1000 needs 2001 digits).
	_, err = FusedMultiplyAdd(New(1, 1000), NewFromInt(1), New(1, -1000), FusedOptions{
		Precision: 2, Rounding: RoundHalfUp, MaxDigits: 1000,
	})
	if !errors.Is(err, ErrFusedBudgetExceeded) {
		t.Fatalf("alignment budget: want ErrFusedBudgetExceeded, got %v", err)
	}

	// The same computation succeeds with a generous budget.
	got, err := FusedMultiplyAdd(New(1, 1000), NewFromInt(1), New(1, -1000), FusedOptions{
		Precision: 2, Rounding: RoundHalfUp, MaxDigits: 5000,
	})
	if err != nil {
		t.Fatalf("expected success with larger budget, got %v", err)
	}
	if got.Cmp(New(1, 1000)) != 0 {
		t.Fatalf("want 10^1000, got %s", got.String())
	}
}

func TestFusedExponentOverflow(t *testing.T) {
	// Product exponent overflows int32.
	_, err := FusedMultiplyAdd(New(1, math.MaxInt32), New(1, 100), Zero, halfUp(2))
	if !errors.Is(err, ErrFusedExponentOverflow) {
		t.Fatalf("want ErrFusedExponentOverflow, got %v", err)
	}

	// A zero product never overflows, even with extreme operand exponents.
	got, err := FusedMultiplyAdd(New(0, math.MaxInt32), New(1, math.MaxInt32), Zero, halfUp(2))
	if err != nil {
		t.Fatalf("zero product should not overflow, got %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("want zero, got %s", got.String())
	}

	_, err = DotProduct(
		[]Decimal{New(1, math.MinInt32)},
		[]Decimal{New(1, -100)},
		halfUp(2),
	)
	if !errors.Is(err, ErrFusedExponentOverflow) {
		t.Fatalf("dot want ErrFusedExponentOverflow, got %v", err)
	}
}

func TestFusedInvalidOptions(t *testing.T) {
	if _, err := FusedMultiplyAdd(NewFromInt(1), NewFromInt(1), Zero, FusedOptions{
		Precision: 2, Rounding: RoundingMode(99),
	}); !errors.Is(err, ErrFusedInvalidRounding) {
		t.Fatalf("want ErrFusedInvalidRounding, got %v", err)
	}

	if _, err := DotProduct(nil, nil, FusedOptions{
		Precision: math.MinInt32, Rounding: RoundHalfUp,
	}); !errors.Is(err, ErrFusedInvalidPrecision) {
		t.Fatalf("want ErrFusedInvalidPrecision, got %v", err)
	}
}
