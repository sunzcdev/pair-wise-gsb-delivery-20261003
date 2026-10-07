package decimal

import "testing"

func TestContextExactDivision(t *testing.T) {
	ctx := Context{Precision: 3, MinExponent: -10, MaxExponent: 10, Rounding: HalfUp}

	for _, test := range []struct {
		x, y int64
		want string
	}{
		{1, 2, "0.5"},
		{1, 4, "0.25"},
		{-1, 8, "-0.125"},
		{100, 25, "4"},
		{1, 25, "0.04"},
	} {
		result, err := ctx.Div(New(test.x, 0), New(test.y, 0))
		if err != nil {
			t.Fatalf("%d/%d returned error: %v", test.x, test.y, err)
		}
		if result.Value.String() != test.want {
			t.Fatalf("%d/%d = %s, want %s", test.x, test.y, result.Value, test.want)
		}
		if result.Status.Rounded || result.Status.Inexact {
			t.Fatalf("%d/%d status = %+v, want no rounding status", test.x, test.y, result.Status)
		}
	}
}

func TestContextZeroOperandStatus(t *testing.T) {
	ctx := Context{Precision: 2, MinExponent: -10, MaxExponent: 10, Rounding: HalfUp}

	for operation, result := range map[string]Result{
		"add": mustResult(ctx.Add(New(1, 0), New(0, 0))),
		"sub": mustResult(ctx.Sub(New(1, 0), New(0, 0))),
		"mul": mustResult(ctx.Mul(New(1, 0), New(0, 0))),
		"div": mustResult(ctx.Div(New(0, 0), New(1, 0))),
	} {
		if result.Status != (Status{}) {
			t.Fatalf("%s status = %+v, want no status", operation, result.Status)
		}
	}
}

func TestContextSubtraction(t *testing.T) {
	ctx := Context{Precision: 3, MinExponent: -10, MaxExponent: 10, Rounding: HalfUp}

	exact, err := ctx.Sub(New(4500, -3), New(3300, -3))
	if err != nil {
		t.Fatalf("Sub returned error: %v", err)
	}
	if exact.Value.String() != "1.2" || !exact.Status.Rounded || exact.Status.Inexact {
		t.Fatalf("exact result = %s status %+v", exact.Value, exact.Status)
	}

	inexact, err := ctx.Sub(New(4567, -3), New(0, 0))
	if err != nil {
		t.Fatalf("inexact Sub returned error: %v", err)
	}
	if inexact.Value.String() != "4.57" || !inexact.Status.Rounded || !inexact.Status.Inexact {
		t.Fatalf("inexact result = %s status %+v", inexact.Value, inexact.Status)
	}
}

func mustResult(result Result, err error) Result {
	if err != nil {
		panic(err)
	}
	return result
}
