package decimal

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func ctxMustParse(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := NewFromString(s)
	if err != nil {
		t.Fatalf("NewFromString(%q): %v", s, err)
	}
	return d
}

func testCtx(precision int32) Context {
	return Context{
		Precision:    precision,
		MaxExponent:  math.MaxInt32,
		MinExponent:  math.MinInt32,
		RoundingMode: RoundHalfUp,
	}
}

func TestContextAddExactTrailingZerosDropped(t *testing.T) {
	// 1.2300 has five significant digits; rounding to three drops only
	// trailing zeros, so the value stays exact.
	res, cond, err := testCtx(3).Add(ctxMustParse(t, "1.2300"), Zero)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Equal(ctxMustParse(t, "1.23")) {
		t.Errorf("expected 1.23, got %s", res)
	}
	if !cond.Has(Rounded) {
		t.Errorf("expected Rounded to be set, got %q", cond)
	}
	if cond.Has(Inexact) {
		t.Errorf("expected Inexact to be unset, got %q", cond)
	}
}

func TestContextAddInexactNonZeroDigitsDropped(t *testing.T) {
	res, cond, err := testCtx(3).Add(ctxMustParse(t, "1.2345"), Zero)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Equal(ctxMustParse(t, "1.23")) {
		t.Errorf("expected 1.23, got %s", res)
	}
	if !cond.Has(Rounded) || !cond.Has(Inexact) {
		t.Errorf("expected Rounded|Inexact, got %q", cond)
	}
}

func TestContextExactResultsRaiseNoConditions(t *testing.T) {
	ctx := testCtx(10)
	ops := []struct {
		name string
		got  func() (Decimal, Condition, error)
		want string
	}{
		{"add", func() (Decimal, Condition, error) { return ctx.Add(ctxMustParse(t, "1.25"), ctxMustParse(t, "2.5")) }, "3.75"},
		{"sub", func() (Decimal, Condition, error) { return ctx.Sub(ctxMustParse(t, "1.25"), ctxMustParse(t, "2.5")) }, "-1.25"},
		{"mul", func() (Decimal, Condition, error) { return ctx.Mul(ctxMustParse(t, "1.25"), ctxMustParse(t, "4")) }, "5"},
		{"div", func() (Decimal, Condition, error) { return ctx.Div(ctxMustParse(t, "1"), ctxMustParse(t, "2")) }, "0.5"},
		{"div carry", func() (Decimal, Condition, error) { return ctx.Div(ctxMustParse(t, "1"), ctxMustParse(t, "1")) }, "1"},
	}
	for _, op := range ops {
		res, cond, err := op.got()
		if err != nil {
			t.Errorf("%s: unexpected error: %v", op.name, err)
		}
		if res.String() != op.want {
			t.Errorf("%s: expected %s, got %s", op.name, op.want, res)
		}
		if cond != 0 {
			t.Errorf("%s: expected no conditions, got %q", op.name, cond)
		}
	}
}

func TestContextDiv(t *testing.T) {
	ctx := testCtx(3)
	res, cond, err := ctx.Div(ctxMustParse(t, "2"), ctxMustParse(t, "3"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.String() != "0.667" {
		t.Errorf("expected 0.667, got %s", res)
	}
	if !cond.Has(Rounded) || !cond.Has(Inexact) {
		t.Errorf("expected Rounded|Inexact, got %q", cond)
	}

	// Rounding carry across the precision boundary: 999 at precision 2.
	res, _, err = testCtx(2).Div(ctxMustParse(t, "999"), ctxMustParse(t, "1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.String() != "1000" {
		t.Errorf("expected 1000, got %s", res)
	}
}

func TestContextDivisionByZero(t *testing.T) {
	_, cond, err := testCtx(3).Div(ctxMustParse(t, "1"), Zero)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrDivisionByZero) {
		t.Errorf("expected errors.Is(err, ErrDivisionByZero), got %v", err)
	}
	var condErr *ConditionError
	if !errors.As(err, &condErr) {
		t.Fatalf("expected *ConditionError, got %T", err)
	}
	if !cond.Has(DivisionByZero) {
		t.Errorf("expected DivisionByZero condition, got %q", cond)
	}
}

func TestContextOverflow(t *testing.T) {
	ctx := testCtx(3)
	ctx.MaxExponent = 2
	_, cond, err := ctx.Mul(ctxMustParse(t, "999"), ctxMustParse(t, "999"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrOverflow) {
		t.Errorf("expected errors.Is(err, ErrOverflow), got %v", err)
	}
	if !cond.Has(Overflow) {
		t.Errorf("expected Overflow condition, got %q", cond)
	}
}

func TestContextUnderflow(t *testing.T) {
	ctx := testCtx(3)
	ctx.MinExponent = -2
	res, cond, err := ctx.Mul(ctxMustParse(t, "1e-5"), ctxMustParse(t, "1e-5"))
	if err != nil {
		t.Fatalf("underflow is not trapped, expected no error, got %v", err)
	}
	if !res.Equal(ctxMustParse(t, "1e-10")) {
		t.Errorf("expected 1e-10, got %s", res)
	}
	if !cond.Has(Underflow) {
		t.Errorf("expected Underflow condition, got %q", cond)
	}

	ctx.Traps = Underflow
	_, cond, err = ctx.Mul(ctxMustParse(t, "1e-5"), ctxMustParse(t, "1e-5"))
	if err == nil {
		t.Fatal("expected an error for trapped Underflow")
	}
	if !errors.Is(err, ErrUnderflow) {
		t.Errorf("expected errors.Is(err, ErrUnderflow), got %v", err)
	}
	if !cond.Has(Underflow) {
		t.Errorf("expected Underflow condition, got %q", cond)
	}
}

func TestContextTraps(t *testing.T) {
	ctx := testCtx(3)
	ctx.Traps = Inexact | Rounded
	res, cond, err := ctx.Div(ctxMustParse(t, "2"), ctxMustParse(t, "3"))
	if err == nil {
		t.Fatal("expected an error for trapped Inexact")
	}
	if !errors.Is(err, ErrInexact) || !errors.Is(err, ErrRounded) {
		t.Errorf("expected ErrInexact and ErrRounded, got %v", err)
	}
	// The finite result is still returned alongside the error.
	if res.String() != "0.667" {
		t.Errorf("expected 0.667, got %s", res)
	}
	if !cond.Has(Inexact) {
		t.Errorf("expected Inexact condition, got %q", cond)
	}
}

func TestContextRoundingModes(t *testing.T) {
	for _, tc := range []struct {
		mode  RoundingMode
		input string
		want  string
	}{
		{RoundHalfUp, "0.125", "0.13"},
		{RoundHalfUp, "-0.125", "-0.13"},
		{RoundHalfEven, "0.125", "0.12"},
		{RoundHalfEven, "0.135", "0.14"},
		{RoundHalfEven, "-0.125", "-0.12"},
		{RoundDown, "0.129", "0.12"},
		{RoundDown, "-0.129", "-0.12"},
		{RoundUp, "0.121", "0.13"},
		{RoundUp, "-0.121", "-0.13"},
		{RoundFloor, "0.129", "0.12"},
		{RoundFloor, "-0.121", "-0.13"},
		{RoundCeil, "0.121", "0.13"},
		{RoundCeil, "-0.129", "-0.12"},
	} {
		ctx := testCtx(2)
		ctx.RoundingMode = tc.mode
		res, _, err := ctx.Add(ctxMustParse(t, tc.input), Zero)
		if err != nil {
			t.Errorf("mode %d input %s: unexpected error: %v", tc.mode, tc.input, err)
			continue
		}
		if res.String() != tc.want {
			t.Errorf("mode %d input %s: expected %s, got %s", tc.mode, tc.input, tc.want, res)
		}
	}
}

func TestContextConcurrentIndependentPrecisions(t *testing.T) {
	// Two contexts with different precisions compute concurrently while a
	// third goroutine mutates the global DivisionPrecision; results must
	// not be affected.
	ctxA := testCtx(5)
	ctxB := testCtx(20)

	var wg sync.WaitGroup
	errs := make(chan error, 3)

	run := func(iterations int, f func(i int) error) {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := f(i); err != nil {
				errs <- err
				return
			}
		}
	}

	wg.Add(3)
	go run(500, func(i int) error {
		res, _, err := ctxA.Div(ctxMustParse(t, "2"), ctxMustParse(t, "3"))
		if err != nil {
			return err
		}
		if res.String() != "0.66667" {
			return errors.New("ctxA got " + res.String())
		}
		return nil
	})
	go run(500, func(i int) error {
		res, _, err := ctxB.Div(ctxMustParse(t, "2"), ctxMustParse(t, "3"))
		if err != nil {
			return err
		}
		if res.String() != "0.66666666666666666667" {
			return errors.New("ctxB got " + res.String())
		}
		return nil
	})
	go run(500, func(i int) error {
		DivisionPrecision = i % 7
		return nil
	})
	wg.Wait()
	close(errs)
	DivisionPrecision = 16
	for err := range errs {
		t.Error(err)
	}
}

func TestContextLegacyDivUnaffected(t *testing.T) {
	// The package-level API keeps its historical behavior.
	defer func() { DivisionPrecision = 16 }()
	DivisionPrecision = 3
	if got := New(2, 0).Div(New(3, 0)).String(); got != "0.667" {
		t.Errorf("expected 0.667, got %s", got)
	}
}
