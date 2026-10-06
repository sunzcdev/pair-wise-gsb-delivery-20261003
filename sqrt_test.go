package decimal

import (
	"math/big"
	"strings"
	"testing"
)

// scaledRadicand returns d * 10^(2*places) as an exact big.Rat.
func scaledRadicand(d Decimal, places int32) *big.Rat {
	rat := new(big.Rat).SetInt(d.getValue())
	scale := int64(d.exp) + 2*int64(places)
	p := new(big.Rat).SetInt(pow10BigInt(abs64(scale)))
	if scale >= 0 {
		rat.Mul(rat, p)
	} else {
		rat.Quo(rat, p)
	}
	return rat
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func squareRat(n *big.Int) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Mul(n, n))
}

// assertSqrtEvidence checks that got equals expected, carries exactly
// -places exponent, and then proves — with exact rational arithmetic on
// D = d * 10^(2*places) — that got lies on the correct side of the
// rounding midpoint for the given mode.
func assertSqrtEvidence(t *testing.T, d Decimal, places int32, mode RoundingMode, expected string) {
	t.Helper()

	got, err := d.Sqrt(places, mode)
	if err != nil {
		t.Fatalf("Sqrt(%d, %d) of %s returned error: %v", places, mode, d, err)
	}
	if got.String() != expected {
		t.Errorf("Sqrt(%d, %d) of %s = %s, expected %s", places, mode, d, got, expected)
	}
	if got.exp != -places {
		t.Errorf("Sqrt(%d, %d) of %s has exponent %d, expected %d", places, mode, d, got.exp, -places)
	}

	r := got.Coefficient()
	D := scaledRadicand(d, places)

	// Universal invariant: the result is within one ulp of the exact
	// root, i.e. (r-1)^2 < D < (r+1)^2.
	rMinus := new(big.Int).Sub(r, oneInt)
	if rMinus.Sign() >= 0 && squareRat(rMinus).Cmp(D) >= 0 {
		t.Errorf("Sqrt of %s: result %s is more than one ulp above the exact root", d, got)
	}
	if squareRat(new(big.Int).Add(r, oneInt)).Cmp(D) <= 0 {
		t.Errorf("Sqrt of %s: result %s is more than one ulp below the exact root", d, got)
	}

	// Mode-specific evidence against the exact midpoint (r - 1/2)^2 and
	// (r + 1/2)^2, compared as integers: (2r +/- 1)^2 vs 4D.
	fourD := new(big.Rat).Mul(D, big.NewRat(4, 1))
	below := new(big.Int).Sub(new(big.Int).Lsh(r, 1), oneInt) // 2r - 1
	above := new(big.Int).Add(new(big.Int).Lsh(r, 1), oneInt) // 2r + 1
	cmpBelow := squareRat(below).Cmp(fourD)                   // sign of (r-1/2)^2 - D
	cmpAbove := fourD.Cmp(squareRat(above))                   // sign of D - (r+1/2)^2

	switch mode {
	case RoundDown, RoundFloor:
		if squareRat(r).Cmp(D) > 0 || squareRat(new(big.Int).Add(r, oneInt)).Cmp(D) <= 0 {
			t.Errorf("Sqrt of %s: %s is not the truncation of the exact root", d, got)
		}
	case RoundCeiling:
		belowR := new(big.Int).Sub(r, oneInt)
		if squareRat(r).Cmp(D) < 0 || (belowR.Sign() >= 0 && squareRat(belowR).Cmp(D) >= 0) {
			t.Errorf("Sqrt of %s: %s is not the ceiling of the exact root", d, got)
		}
	case RoundHalfUp:
		// Result r means (r-1/2)^2 <= D < (r+1/2)^2: exact midpoints
		// round away from zero. The lower bound
		// only applies when r > 0, since the exact root is non-negative.
		if (r.Sign() > 0 && cmpBelow > 0) || cmpAbove >= 0 {
			t.Errorf("Sqrt of %s: %s violates half-up midpoint bounds", d, got)
		}
	case RoundHalfEven:
		// Result r means (r-1/2)^2 <= D <= (r+1/2)^2, and any exact
		// midpoint must have been rounded to an even neighbor.
		if (r.Sign() > 0 && cmpBelow > 0) || cmpAbove > 0 {
			t.Errorf("Sqrt of %s: %s violates half-even midpoint bounds", d, got)
		}
		if (cmpBelow == 0 || cmpAbove == 0) && r.Bit(0) != 0 {
			t.Errorf("Sqrt of %s: %s is an exact midpoint rounded to an odd neighbor", d, got)
		}
	}
}

func TestSqrtPerfectSquares(t *testing.T) {
	cases := []struct {
		input    string
		places   int32
		expected string
	}{
		{"0", 0, "0"},
		{"0", 4, "0"},
		{"1", 0, "1"},
		{"144", 0, "12"},
		{"144", 2, "12"},
		{"2.25", 1, "1.5"},
		{"2.25", 5, "1.5"},
		{"0.0144", 3, "0.12"},
		{"0.0144", 6, "0.12"},
		{"152399025", 0, "12345"},
		{"1.44", 1, "1.2"},
	}
	for _, c := range cases {
		d := RequireFromString(c.input)
		for _, mode := range []RoundingMode{RoundDown, RoundCeiling, RoundFloor, RoundHalfUp, RoundHalfEven} {
			assertSqrtEvidence(t, d, c.places, mode, c.expected)
		}
	}
}

func TestSqrtRoundingModes(t *testing.T) {
	// sqrt(2) = 1.4142135623...
	two := RequireFromString("2")
	assertSqrtEvidence(t, two, 10, RoundDown, "1.4142135623")
	assertSqrtEvidence(t, two, 10, RoundFloor, "1.4142135623")
	assertSqrtEvidence(t, two, 10, RoundCeiling, "1.4142135624")
	assertSqrtEvidence(t, two, 10, RoundHalfUp, "1.4142135624")
	assertSqrtEvidence(t, two, 10, RoundHalfEven, "1.4142135624")
	assertSqrtEvidence(t, two, 1, RoundHalfUp, "1.4")
	assertSqrtEvidence(t, two, 0, RoundHalfUp, "1")

	// sqrt(6.25) = 2.5 exactly; ceiling must not increment exact roots.
	d := RequireFromString("6.25")
	assertSqrtEvidence(t, d, 1, RoundCeiling, "2.5")
	assertSqrtEvidence(t, d, 0, RoundCeiling, "3")
	assertSqrtEvidence(t, d, 0, RoundFloor, "2")
	assertSqrtEvidence(t, d, 0, RoundDown, "2")
}

func TestSqrtExactMidpoint(t *testing.T) {
	// 1.235^2 = 1.525225: sqrt sits exactly on the rounding midpoint
	// between 1.23 and 1.24 (neighbor 123 is odd).
	d := RequireFromString("1.525225")
	assertSqrtEvidence(t, d, 2, RoundHalfUp, "1.24")
	assertSqrtEvidence(t, d, 2, RoundHalfEven, "1.24")
	assertSqrtEvidence(t, d, 2, RoundDown, "1.23")
	assertSqrtEvidence(t, d, 2, RoundCeiling, "1.24")

	// 1.245^2 = 1.550025: exact midpoint with even lower neighbor 124.
	d = RequireFromString("1.550025")
	assertSqrtEvidence(t, d, 2, RoundHalfUp, "1.25")
	assertSqrtEvidence(t, d, 2, RoundHalfEven, "1.24")

	// 0.5^2 = 0.25: midpoint between 0 and 1 at 0 places.
	d = RequireFromString("0.25")
	assertSqrtEvidence(t, d, 0, RoundHalfUp, "1")
	assertSqrtEvidence(t, d, 0, RoundHalfEven, "0")

	// sqrt(0.16) = 0.4 rounds to zero at 0 places.
	d = RequireFromString("0.16")
	assertSqrtEvidence(t, d, 0, RoundHalfUp, "0")
	assertSqrtEvidence(t, d, 0, RoundHalfEven, "0")
}

func TestSqrtNearMidpoint(t *testing.T) {
	// Values one micro-unit below and above the exact midpoint square
	// 1.235^2 = 1.525225. An approximate (float64) implementation cannot
	// reliably tell these apart from the midpoint; exact arithmetic can.
	below := RequireFromString("1.525224") // sqrt = 1.23499959...
	above := RequireFromString("1.525226") // sqrt = 1.23500040...

	assertSqrtEvidence(t, below, 2, RoundHalfUp, "1.23")
	assertSqrtEvidence(t, below, 2, RoundHalfEven, "1.23")
	assertSqrtEvidence(t, below, 2, RoundCeiling, "1.24")
	assertSqrtEvidence(t, below, 2, RoundDown, "1.23")

	assertSqrtEvidence(t, above, 2, RoundHalfUp, "1.24")
	assertSqrtEvidence(t, above, 2, RoundHalfEven, "1.24")
	assertSqrtEvidence(t, above, 2, RoundFloor, "1.23")

	// Same probe at 30 places. With k = 10^30 + 4 (even), the exact
	// midpoint (k + 1/2) * 10^-30 has the finite decimal square
	// (2k+1)^2 * 25 * 10^-62, so the inputs below carry 62 places.
	k := new(big.Int).Add(new(big.Int).Exp(tenInt, big.NewInt(30), nil), big.NewInt(4))
	odd := new(big.Int).Add(new(big.Int).Lsh(k, 1), oneInt) // 2k + 1
	coeff := new(big.Int).Mul(odd, odd)
	coeff.Mul(coeff, big.NewInt(25)) // (2k+1)^2 * 25
	toDecimal := func(c *big.Int) Decimal {
		return NewFromBigInt(c, -62)
	}
	belowSq := new(big.Int).Sub(coeff, oneInt)
	aboveSq := new(big.Int).Add(coeff, oneInt)

	assertSqrtEvidence(t, toDecimal(coeff), 30, RoundHalfUp, "1.000000000000000000000000000005")
	assertSqrtEvidence(t, toDecimal(coeff), 30, RoundHalfEven, "1.000000000000000000000000000004")
	assertSqrtEvidence(t, toDecimal(belowSq), 30, RoundHalfUp, "1.000000000000000000000000000004")
	assertSqrtEvidence(t, toDecimal(aboveSq), 30, RoundHalfUp, "1.000000000000000000000000000005")
}

func TestSqrtHundredsOfDigits(t *testing.T) {
	// A 300-digit root; its square has 600 digits.
	rootStr := "1" + strings.Repeat("3", 299)
	rootInt, ok := new(big.Int).SetString(rootStr, 10)
	if !ok {
		t.Fatal("failed to build test integer")
	}
	sq := new(big.Int).Mul(rootInt, rootInt)
	d := NewFromBigInt(sq, 0)
	for _, mode := range []RoundingMode{RoundDown, RoundCeiling, RoundFloor, RoundHalfUp, RoundHalfEven} {
		assertSqrtEvidence(t, d, 0, mode, rootStr)
	}

	// 600-digit non-square: sq + 1 truncates to the same 300-digit root
	// for RoundDown, and rounds up only for RoundCeiling.
	d = NewFromBigInt(new(big.Int).Add(sq, oneInt), 0)
	assertSqrtEvidence(t, d, 0, RoundDown, rootStr)
	assertSqrtEvidence(t, d, 0, RoundFloor, rootStr)
	assertSqrtEvidence(t, d, 0, RoundHalfUp, rootStr)
	assertSqrtEvidence(t, d, 0, RoundHalfEven, rootStr)
	up := new(big.Int).Add(rootInt, oneInt)
	assertSqrtEvidence(t, d, 0, RoundCeiling, up.String())

	// 400-digit fractional input with 200 fractional digits requested.
	frac := "9" + strings.Repeat("8", 399)
	fracInt, _ := new(big.Int).SetString(frac, 10)
	d = NewFromBigInt(fracInt, -200)
	got, err := d.Sqrt(200, RoundHalfEven)
	if err != nil {
		t.Fatalf("Sqrt of 400-digit input returned error: %v", err)
	}
	// Exact evidence: r^2 <= d * 10^400 < (r+1)^2.
	r := got.Coefficient()
	D := scaledRadicand(d, 200)
	if squareRat(r).Cmp(D) > 0 || squareRat(new(big.Int).Add(r, oneInt)).Cmp(D) <= 0 {
		t.Errorf("Sqrt of 400-digit input: %s is not the verified floor root", got)
	}
}

func TestSqrtNegative(t *testing.T) {
	for _, input := range []string{"-1", "-0.0001", "-152399025"} {
		d := RequireFromString(input)
		if _, err := d.Sqrt(2, RoundHalfUp); err == nil {
			t.Errorf("Sqrt of %s: expected error, got nil", input)
		}
	}
}

func TestSqrtInvalidMode(t *testing.T) {
	d := RequireFromString("2")
	for _, mode := range []RoundingMode{-1, 5, 100} {
		if _, err := d.Sqrt(2, mode); err == nil {
			t.Errorf("Sqrt with mode %d: expected error, got nil", int(mode))
		}
	}
}

func TestSqrtResourceLimit(t *testing.T) {
	old := SqrtMaxScale
	defer func() { SqrtMaxScale = old }()
	SqrtMaxScale = 100

	d := RequireFromString("2")
	// scale = 0 + 2*51 = 102 > 100
	if _, err := d.Sqrt(51, RoundHalfUp); err == nil {
		t.Error("Sqrt with places=51: expected resource limit error, got nil")
	}
	// scale = 0 + 2*50 = 100, exactly at the limit: allowed.
	if _, err := d.Sqrt(50, RoundHalfUp); err != nil {
		t.Errorf("Sqrt with places=50: unexpected error: %v", err)
	}
	// Huge positive exponent: scale = 10^6.
	d = NewFromBigInt(oneInt, 1000000)
	if _, err := d.Sqrt(0, RoundHalfUp); err == nil {
		t.Error("Sqrt of 1e1000000: expected resource limit error, got nil")
	}
	// Huge negative exponent: scale = -10^6.
	d = NewFromBigInt(oneInt, -1000000)
	if _, err := d.Sqrt(0, RoundHalfUp); err == nil {
		t.Error("Sqrt of 1e-1000000: expected resource limit error, got nil")
	}
}
