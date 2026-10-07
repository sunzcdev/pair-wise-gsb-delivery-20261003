package realroots

import (
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func opts(low, high, width string, includeHigh bool) Options {
	return Options{
		Low:         d(low),
		High:        d(high),
		IncludeHigh: includeHigh,
		TargetWidth: d(width),
	}
}

func TestZeroPolynomial(t *testing.T) {
	res, err := Isolate([]decimal.Decimal{d("0"), d("0")}, opts("-1", "1", "0.01", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Zero || !res.Complete || len(res.Roots) != 0 {
		t.Fatalf("zero polynomial misclassified: %+v", res)
	}
}

func TestConstantPolynomial(t *testing.T) {
	res, err := Isolate([]decimal.Decimal{d("7.5")}, opts("-1", "1", "0.01", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Constant || !res.Complete || len(res.Roots) != 0 {
		t.Fatalf("constant misclassified: %+v", res)
	}
}

func TestNoRootsInInterval(t *testing.T) {
	// x^2 + 1 has no real roots anywhere.
	p := []decimal.Decimal{d("1"), d("0"), d("1")}
	res, err := Isolate(p, opts("-10", "10", "0.01", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Roots) != 0 || len(res.Pending) != 0 {
		t.Fatalf("expected no roots, got %+v", res)
	}
}

func TestRepeatedFactorMultiplicity(t *testing.T) {
	// (x-1)^3 (x+2)^2 = x^5 + x^4 - 5x^3 - x^2 + 8x - 4
	p := []decimal.Decimal{
		d("-4"), d("8"), d("-1"), d("-5"), d("1"), d("1"),
	}
	res, err := Isolate(p, opts("-3", "2", "0.001", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete {
		t.Fatalf("expected complete, got pending %+v", res.Pending)
	}
	if len(res.Roots) != 2 {
		t.Fatalf("want 2 distinct roots, got %d: %+v", len(res.Roots), res.Roots)
	}
	// Each distinct root is isolated with its true multiplicity and the
	// interval is guaranteed to contain it.
	for _, r := range res.Roots {
		lo, _ := decimalToRat(r.Low)
		hi, _ := decimalToRat(r.High)
		contains := func(root string) bool {
			rf, _ := new(big.Rat).SetString(root)
			return lo.Cmp(rf) <= 0 && rf.Cmp(hi) <= 0
		}
		switch {
		case contains("-2"):
			if r.Multiplicity != 2 {
				t.Fatalf("root -2 multiplicity: %+v", r)
			}
		case contains("1"):
			if r.Multiplicity != 3 {
				t.Fatalf("root 1 multiplicity: %+v", r)
			}
		default:
			t.Fatalf("interval %s..%s contains no expected root", r.Low, r.High)
		}
	}
	if res.Roots[0].Width().Cmp(d("0.001")) > 0 || res.Roots[1].Width().Cmp(d("0.001")) > 0 {
		t.Fatalf("isolating widths too wide: %+v", res.Roots)
	}
}

func TestCloseAdjacentRoots(t *testing.T) {
	// Two roots separated by 1e-4: (x - 0.2500)(x - 0.2501).
	// x^2 - 0.5001 x + 0.062525
	p := []decimal.Decimal{d("0.062525"), d("-0.5001"), d("1")}
	res, err := Isolate(p, opts("0", "1", "0.00001", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete {
		t.Fatalf("expected complete, pending %+v", res.Pending)
	}
	if len(res.Roots) != 2 {
		t.Fatalf("want 2 separated roots, got %d: %+v", len(res.Roots), res.Roots)
	}
	// Intervals must not overlap, each must hold its own root, and widths
	// must meet the target.
	target := d("0.00001")
	for i, r := range res.Roots {
		if r.Width().Cmp(target) > 0 {
			t.Fatalf("width %s exceeds target", r.Width())
		}
		if i > 0 && r.Low.Cmp(res.Roots[i-1].High) < 0 {
			t.Fatalf("overlapping intervals: %+v %+v", res.Roots[i-1], r)
		}
		// Certified containment of the true roots.
		lo := r.Low.BigFloat()
		hi := r.High.BigFloat()
		contains := func(root string) bool {
			rf := d(root).BigFloat()
			return lo.Cmp(rf) <= 0 && rf.Cmp(hi) <= 0
		}
		if !contains("0.25") && !contains("0.2501") {
			t.Fatalf("interval %s..%s contains neither root", r.Low, r.High)
		}
	}
}

func TestEndpointsAndOpenInterval(t *testing.T) {
	// x (x - 1): roots 0 and 1, both on the boundaries of [0, 1).
	p := []decimal.Decimal{d("0"), d("-1"), d("1")}

	// Half open [0,1): root 0 included, root 1 excluded.
	res, err := Isolate(p, opts("0", "1", "0.01", false))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Roots) != 1 {
		t.Fatalf("half-open: %+v", res)
	}
	r := res.Roots[0]
	if r.Low.String() != "0" || r.Endpoint != EndpointLow || r.Multiplicity != 1 {
		t.Fatalf("half-open root: %+v", r)
	}

	// Closed [0,1]: both endpoints reported.
	res, err = Isolate(p, opts("0", "1", "0.01", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Roots) != 2 {
		t.Fatalf("closed: %+v", res)
	}
	if res.Roots[0].Endpoint != EndpointLow || res.Roots[1].Endpoint != EndpointHigh {
		t.Fatalf("endpoint flags: %+v", res.Roots)
	}
}

func TestWidthTargetIsHonored(t *testing.T) {
	// Irrational roots: x^2 - 2, root sqrt(2) ~ 1.41421356...
	p := []decimal.Decimal{d("-2"), d("0"), d("1")}
	res, err := Isolate(p, opts("1", "2", "1e-20", true))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || len(res.Roots) != 1 {
		t.Fatalf("%+v", res)
	}
	r := res.Roots[0]
	if r.Width().Cmp(d("1e-20")) > 0 {
		t.Fatalf("width %s", r.Width())
	}
	// The exact root must lie inside.
	sqrt2 := new(big.Rat)
	_, _ = sqrt2.SetString("14142135623730950488/10000000000000000000")
	lo, _ := decimalToRat(r.Low)
	hi, _ := decimalToRat(r.High)
	if lo.Cmp(sqrt2) > 0 || sqrt2.Cmp(hi) > 0 {
		t.Fatalf("sqrt2 not in %s..%s", r.Low, r.High)
	}
}

func TestBisectionBudgetLeavesPending(t *testing.T) {
	// Close roots force deep subdivision; a tiny bisection budget must leave
	// unsearched pending regions rather than declaring anything root free.
	p := []decimal.Decimal{d("0.062525"), d("-0.5001"), d("1")}
	o := opts("0", "1", "1e-12", true)
	o.Budget = Budget{MaxBisections: 3}
	res, err := Isolate(p, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatalf("expected incomplete result")
	}
	if len(res.Pending) == 0 {
		t.Fatalf("expected pending intervals")
	}
	for _, pn := range res.Pending {
		if pn.Reason != ReasonMaxBisections {
			t.Fatalf("reason = %q", pn.Reason)
		}
		if pn.Low.Cmp(pn.High) >= 0 {
			t.Fatalf("degenerate pending interval")
		}
	}
}

func TestOperationBudgetLeavesPending(t *testing.T) {
	// x^2 - 2 with a very small exact-operation budget: the root may be
	// reported, but the unsearched remainder must be flagged, never dropped.
	p := []decimal.Decimal{d("-2"), d("0"), d("1")}
	o := opts("1", "2", "1e-30", true)
	o.Budget = Budget{MaxOperations: 50}
	res, err := Isolate(p, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatalf("expected operation budget to interrupt")
	}
	if len(res.Pending) == 0 || res.Pending[0].Reason != ReasonMaxOperations {
		t.Fatalf("pending/reason: %+v", res.Pending)
	}
}

func TestDegreeBudget(t *testing.T) {
	// Degree 3 polynomial with MaxDegree 2: nothing interior is certified.
	p := []decimal.Decimal{d("0"), d("-1"), d("0"), d("1")} // x(x-1)(x+1)
	o := opts("-0.5", "0.5", "0.01", true)
	o.Budget = Budget{MaxDegree: 2}
	res, err := Isolate(p, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || len(res.Roots) != 0 || len(res.Pending) != 1 {
		t.Fatalf("%+v", res)
	}
	if res.Pending[0].Reason != ReasonMaxDegree {
		t.Fatalf("reason = %q", res.Pending[0].Reason)
	}
}

func TestInvalidInterval(t *testing.T) {
	if _, err := Isolate([]decimal.Decimal{d("0"), d("1")}, opts("1", "1", "0.01", true)); err == nil {
		t.Fatal("expected error for Low == High")
	}
	if _, err := Isolate([]decimal.Decimal{d("0"), d("1")}, opts("2", "1", "0.01", true)); err == nil {
		t.Fatal("expected error for Low > High")
	}
}
