package decimal

import (
	"math/big"
	"testing"
)

func intRootTestCoeffs(coeffs ...int64) []Decimal {
	out := make([]Decimal, len(coeffs))
	for i, c := range coeffs {
		out[i] = New(c, 0)
	}
	return out
}

func assertRootsDisjoint(t *testing.T, roots []RootInterval) {
	t.Helper()
	for i := 0; i+1 < len(roots); i++ {
		if roots[i].Upper.Cmp(roots[i+1].Lower) >= 0 {
			t.Fatalf("root intervals %v and %v overlap", roots[i], roots[i+1])
		}
	}
}

func TestIsolateRealRootsRepeatedFactors(t *testing.T) {
	// p(x) = (x-1)^2 (x-2)^3 = x^5 - 8x^4 + 25x^3 - 38x^2 + 28x - 8
	coeffs := intRootTestCoeffs(-8, 28, -38, 25, -8, 1)
	opts := NewRootIsolationOptions(New(0, 0), New(3, 0), New(1, -3))
	res := IsolateRealRoots(coeffs, opts)
	if !res.Complete() {
		t.Fatalf("expected complete result, got %v (pending=%v)", res.Reason, res.Pending)
	}
	if len(res.Roots) != 2 {
		t.Fatalf("expected 2 roots, got %d: %v", len(res.Roots), res.Roots)
	}
	assertRootsDisjoint(t, res.Roots)

	one, two := big.NewRat(1, 1), big.NewRat(2, 1)
	if !res.Roots[0].Contains(one) || res.Roots[0].Multiplicity != 2 {
		t.Fatalf("expected root near 1 with multiplicity 2, got %v", res.Roots[0])
	}
	if !res.Roots[1].Contains(two) || res.Roots[1].Multiplicity != 3 {
		t.Fatalf("expected root near 2 with multiplicity 3, got %v", res.Roots[1])
	}
	width := New(1, -3).Rat()
	for _, r := range res.Roots {
		if !r.IsExact() && r.Width().Cmp(width) > 0 {
			t.Fatalf("root interval %v wider than target %v", r, width)
		}
	}
}

func TestIsolateRealRootsVeryCloseRoots(t *testing.T) {
	// p(x) = (x - 1)(x - 1 - 1e-20): two distinct roots 1e-20 apart.
	eps := New(1, -20)
	r1 := New(1, 0)
	r2 := New(1, 0).Add(eps)
	// p = x^2 - (r1+r2)x + r1*r2
	coeffs := []Decimal{r1.Mul(r2), r1.Add(r2).Neg(), New(1, 0)}
	opts := NewRootIsolationOptions(New(0, 0), New(3, 0), New(1, -24))
	res := IsolateRealRoots(coeffs, opts)
	if !res.Complete() {
		t.Fatalf("expected complete result, got %v (pending=%v)", res.Reason, res.Pending)
	}
	if len(res.Roots) != 2 {
		t.Fatalf("expected 2 roots, got %d: %v", len(res.Roots), res.Roots)
	}
	assertRootsDisjoint(t, res.Roots)
	if !res.Roots[0].Contains(r1.Rat()) || res.Roots[0].Multiplicity != 1 {
		t.Fatalf("expected simple root near 1, got %v", res.Roots[0])
	}
	if !res.Roots[1].Contains(r2.Rat()) || res.Roots[1].Multiplicity != 1 {
		t.Fatalf("expected simple root near 1+1e-20, got %v", res.Roots[1])
	}
	width := New(1, -24).Rat()
	for _, r := range res.Roots {
		if !r.IsExact() && r.Width().Cmp(width) > 0 {
			t.Fatalf("root interval %v wider than target %v", r, width)
		}
	}
}

func TestIsolateRealRootsNoRootsInInterval(t *testing.T) {
	// p(x) = x^2 + 1 has no real roots.
	res := IsolateRealRoots(intRootTestCoeffs(1, 0, 1),
		NewRootIsolationOptions(New(-10, 0), New(10, 0), New(1, -2)))
	if !res.Complete() {
		t.Fatalf("expected complete result, got %v", res.Reason)
	}
	if len(res.Roots) != 0 {
		t.Fatalf("expected no roots, got %v", res.Roots)
	}
	if len(res.Pending) != 0 {
		t.Fatalf("expected no pending intervals, got %v", res.Pending)
	}

	// p(x) = x^2 - 1 has roots, but none in [2, 10].
	res = IsolateRealRoots(intRootTestCoeffs(-1, 0, 1),
		NewRootIsolationOptions(New(2, 0), New(10, 0), New(1, -2)))
	if !res.Complete() || len(res.Roots) != 0 || len(res.Pending) != 0 {
		t.Fatalf("expected empty complete result, got roots=%v pending=%v reason=%v",
			res.Roots, res.Pending, res.Reason)
	}
}

func TestIsolateRealRootsEndpointRoots(t *testing.T) {
	// p(x) = x(x - 2) = x^2 - 2x: roots exactly at the endpoints of [0, 2].
	res := IsolateRealRoots(intRootTestCoeffs(0, -2, 1),
		NewRootIsolationOptions(New(0, 0), New(2, 0), New(1, -1)))
	if !res.Complete() {
		t.Fatalf("expected complete result, got %v", res.Reason)
	}
	if len(res.Roots) != 2 {
		t.Fatalf("expected 2 roots, got %v", res.Roots)
	}
	assertRootsDisjoint(t, res.Roots)
	for i, want := range []int64{0, 2} {
		r := res.Roots[i]
		if !r.IsExact() || r.Lower.Cmp(big.NewRat(want, 1)) != 0 || r.Multiplicity != 1 {
			t.Fatalf("expected exact root at %d with multiplicity 1, got %v", want, r)
		}
	}

	// p(x) = x - 1 on [0, 2]: bisection hits the root exactly.
	res = IsolateRealRoots(intRootTestCoeffs(-1, 1),
		NewRootIsolationOptions(New(0, 0), New(2, 0), New(1, -3)))
	if !res.Complete() || len(res.Roots) != 1 {
		t.Fatalf("expected 1 root, got roots=%v reason=%v", res.Roots, res.Reason)
	}
	if !res.Roots[0].IsExact() || res.Roots[0].Lower.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("expected exact root at 1, got %v", res.Roots[0])
	}
}

func TestIsolateRealRootsZeroAndConstant(t *testing.T) {
	opts := NewRootIsolationOptions(New(0, 0), New(1, 0), New(1, -2))

	res := IsolateRealRoots(intRootTestCoeffs(0, 0, 0), opts)
	if res.Reason != IsolationZeroPolynomial {
		t.Fatalf("expected IsolationZeroPolynomial, got %v", res.Reason)
	}
	if res.Complete() {
		t.Fatal("zero polynomial must not report complete")
	}

	res = IsolateRealRoots(nil, opts)
	if res.Reason != IsolationZeroPolynomial {
		t.Fatalf("expected IsolationZeroPolynomial for empty coeffs, got %v", res.Reason)
	}

	res = IsolateRealRoots(intRootTestCoeffs(7), opts)
	if !res.Complete() || len(res.Roots) != 0 {
		t.Fatalf("constant polynomial: expected complete empty result, got roots=%v reason=%v",
			res.Roots, res.Reason)
	}
}

func TestIsolateRealRootsDegreeLimit(t *testing.T) {
	// Degree 5 polynomial with MaxDegree 3: nothing may be searched.
	coeffs := intRootTestCoeffs(-120, 274, -225, 85, -15, 1)
	opts := NewRootIsolationOptions(New(0, 0), New(10, 0), New(1, -2))
	opts.MaxDegree = 3
	res := IsolateRealRoots(coeffs, opts)
	if res.Reason != IsolationDegreeLimit {
		t.Fatalf("expected IsolationDegreeLimit, got %v", res.Reason)
	}
	if len(res.Roots) != 0 {
		t.Fatalf("expected no roots, got %v", res.Roots)
	}
	if len(res.Pending) != 1 ||
		res.Pending[0].Lower.Cmp(big.NewRat(0, 1)) != 0 ||
		res.Pending[0].Upper.Cmp(big.NewRat(10, 1)) != 0 {
		t.Fatalf("expected whole interval pending, got %v", res.Pending)
	}
}

func TestIsolateRealRootsBudgetExhausted(t *testing.T) {
	// p(x) = (x-1)(x-2)(x-3)(x-4)(x-5)
	coeffs := intRootTestCoeffs(-120, 274, -225, 85, -15, 1)

	// Unlimited budget: all five roots are isolated.
	res := IsolateRealRoots(coeffs, NewRootIsolationOptions(New(0, 0), New(10, 0), New(1, -2)))
	if !res.Complete() {
		t.Fatalf("expected complete result, got %v (pending=%v)", res.Reason, res.Pending)
	}
	if len(res.Roots) != 5 {
		t.Fatalf("expected 5 roots, got %v", res.Roots)
	}
	assertRootsDisjoint(t, res.Roots)
	for i, r := range res.Roots {
		if r.Multiplicity != 1 || !r.Contains(big.NewRat(int64(i+1), 1)) {
			t.Fatalf("expected simple root near %d, got %v", i+1, r)
		}
	}

	// Tiny budget: the run must report the unsearched remainder instead
	// of silently declaring intervals root-free.
	opts := NewRootIsolationOptions(New(0, 0), New(10, 0), New(1, -2))
	opts.MaxOps = 1
	res = IsolateRealRoots(coeffs, opts)
	if res.Reason != IsolationBudgetExhausted {
		t.Fatalf("expected IsolationBudgetExhausted, got %v", res.Reason)
	}
	if res.Complete() {
		t.Fatal("exhausted run must not report complete")
	}
	if len(res.Pending) == 0 {
		t.Fatal("expected pending intervals for the unsearched region")
	}
	// The pending intervals must cover the whole search interval [0, 10]:
	// nothing unsearched may be dropped.
	merged := mergePending(res.Pending)
	if len(merged) != 1 ||
		merged[0].Lower.Cmp(big.NewRat(0, 1)) != 0 ||
		merged[0].Upper.Cmp(big.NewRat(10, 1)) != 0 {
		t.Fatalf("pending intervals do not cover [0, 10]: %v", res.Pending)
	}
}

func TestIsolateRealRootsRationalEndpoints(t *testing.T) {
	// p(x) = 3x - 1: the root 1/3 is not a finite decimal.
	opts := NewRootIsolationOptionsRat(big.NewRat(0, 1), big.NewRat(1, 1), big.NewRat(1, 100))
	res := IsolateRealRoots(intRootTestCoeffs(-1, 3), opts)
	if !res.Complete() {
		t.Fatalf("expected complete result, got %v", res.Reason)
	}
	if len(res.Roots) != 1 {
		t.Fatalf("expected 1 root, got %v", res.Roots)
	}
	r := res.Roots[0]
	if !r.Contains(big.NewRat(1, 3)) {
		t.Fatalf("root interval %v does not contain 1/3", r)
	}
	if r.Multiplicity != 1 {
		t.Fatalf("expected multiplicity 1, got %v", r)
	}
	if !r.IsExact() && r.Width().Cmp(big.NewRat(1, 100)) > 0 {
		t.Fatalf("root interval %v wider than target 1/100", r)
	}
}

func TestIsolateRealRootsInvalidOptions(t *testing.T) {
	coeffs := intRootTestCoeffs(-1, 1)

	res := IsolateRealRoots(coeffs, RootIsolationOptions{})
	if res.Reason != IsolationInvalidInput {
		t.Fatalf("expected IsolationInvalidInput, got %v", res.Reason)
	}

	res = IsolateRealRoots(coeffs, NewRootIsolationOptions(New(0, 0), New(1, 0), New(0, 0)))
	if res.Reason != IsolationInvalidInput {
		t.Fatalf("expected IsolationInvalidInput for zero width, got %v", res.Reason)
	}

	res = IsolateRealRoots(coeffs, NewRootIsolationOptions(New(2, 0), New(1, 0), New(1, -1)))
	if res.Reason != IsolationInvalidInput {
		t.Fatalf("expected IsolationInvalidInput for swapped endpoints, got %v", res.Reason)
	}
}
