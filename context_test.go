package decimal

import (
	"sync"
	"testing"
)

func TestContextExactTrailingZeros(t *testing.T) {
	ctx := Context{Precision: 3, MinExponent: -10, MaxExponent: 10, Rounding: HalfUp}
	x := New(1200, -3)
	y := New(1800, -3)

	result, err := ctx.Add(x, y)
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if got, want := result.Value.String(), "3"; got != want {
		t.Fatalf("Value = %q, want %q", got, want)
	}
	if !result.Value.Equal(New(3, 0)) {
		t.Fatalf("Value = %s, want 3", result.Value)
	}
	if !result.Status.Rounded || result.Status.Inexact {
		t.Fatalf("Status = %+v, want Rounded without Inexact", result.Status)
	}

	multiplied, err := ctx.Mul(New(120, -2), New(10, -1))
	if err != nil {
		t.Fatalf("Mul returned error: %v", err)
	}
	if got, want := multiplied.Value.String(), "1.2"; got != want {
		t.Fatalf("Mul value = %q, want %q", got, want)
	}
	if !multiplied.Status.Rounded || multiplied.Status.Inexact {
		t.Fatalf("Mul status = %+v, want Rounded without Inexact", multiplied.Status)
	}
}

func TestContextInexactRounding(t *testing.T) {
	ctx := Context{Precision: 2, MinExponent: -10, MaxExponent: 10, Rounding: HalfUp}

	result, err := ctx.Add(New(126, -2), New(0, 0))
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if got, want := result.Value.String(), "1.3"; got != want {
		t.Fatalf("Value = %q, want %q", got, want)
	}
	if !result.Status.Rounded || !result.Status.Inexact {
		t.Fatalf("Status = %+v, want Rounded and Inexact", result.Status)
	}

	divided, err := ctx.Div(New(1, 0), New(3, 0))
	if err != nil {
		t.Fatalf("Div returned error: %v", err)
	}
	if got, want := divided.Value.String(), "0.33"; got != want {
		t.Fatalf("Div value = %q, want %q", got, want)
	}
	if !divided.Status.Rounded || !divided.Status.Inexact {
		t.Fatalf("Div status = %+v, want Rounded and Inexact", divided.Status)
	}
}

func TestContextConcurrentPrecision(t *testing.T) {
	const iterations = 200

	lowPrecision := Context{Precision: 2, MinExponent: -100, MaxExponent: 100, Rounding: HalfUp}
	highPrecision := Context{Precision: 6, MinExponent: -100, MaxExponent: 100, Rounding: HalfUp}
	one := New(1, 0)
	three := New(3, 0)

	var waitGroup sync.WaitGroup
	lowResults := make([]string, iterations)
	highResults := make([]string, iterations)

	for i := 0; i < iterations; i++ {
		waitGroup.Add(2)
		go func(index int) {
			defer waitGroup.Done()
			result, err := lowPrecision.Div(one, three)
			if err != nil {
				t.Errorf("low precision Div returned error: %v", err)
				return
			}
			lowResults[index] = result.Value.String()
		}(i)

		go func(index int) {
			defer waitGroup.Done()
			result, err := highPrecision.Div(one, three)
			if err != nil {
				t.Errorf("high precision Div returned error: %v", err)
				return
			}
			highResults[index] = result.Value.String()
		}(i)
	}

	waitGroup.Wait()
	for i, result := range lowResults {
		if result != "0.33" {
			t.Fatalf("low result %d = %q, want 0.33", i, result)
		}
	}
	for i, result := range highResults {
		if result != "0.333333" {
			t.Fatalf("high result %d = %q, want 0.333333", i, result)
		}
	}
	if DivisionPrecision != 16 {
		t.Fatalf("DivisionPrecision = %d, want 16", DivisionPrecision)
	}
}

func TestContextRoundingModes(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   RoundingMode
		result string
	}{
		{"half up", HalfUp, "-0.13"},
		{"half even", HalfEven, "-0.12"},
		{"half down", HalfDown, "-0.12"},
		{"up", Up, "-0.13"},
		{"down", Down, "-0.12"},
		{"ceiling", Ceiling, "-0.12"},
		{"floor", Floor, "-0.13"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := Context{Precision: 2, MinExponent: -10, MaxExponent: 10, Rounding: test.mode}
			result, err := ctx.Div(New(-1, 0), New(8, 0))
			if err != nil {
				t.Fatalf("Div returned error: %v", err)
			}
			if result.Value.String() != test.result {
				t.Fatalf("Value = %s, want %s", result.Value, test.result)
			}
		})
	}
}

func TestContextOverflowAndUnderflow(t *testing.T) {
	withoutTraps := Context{Precision: 2, MinExponent: -5, MaxExponent: 1, Rounding: HalfUp}

	overflow, err := withoutTraps.Mul(New(99, 0), New(10, 0))
	if err != nil {
		t.Fatalf("Mul returned error: %v", err)
	}
	if overflow.Value.String() != "99" {
		t.Fatalf("Overflow value = %s, want 99", overflow.Value)
	}
	if !overflow.Status.Overflow || !overflow.Status.Rounded || !overflow.Status.Inexact {
		t.Fatalf("Overflow status = %+v", overflow.Status)
	}

	underflow, err := withoutTraps.Add(New(1, -6), New(0, 0))
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if !underflow.Value.IsZero() {
		t.Fatalf("Underflow value = %s, want zero", underflow.Value)
	}
	if !underflow.Status.Underflow || !underflow.Status.Rounded || !underflow.Status.Inexact {
		t.Fatalf("Underflow status = %+v", underflow.Status)
	}

	underflowBoundary, err := withoutTraps.Add(New(999, -8), New(0, 0))
	if err != nil {
		t.Fatalf("underflow boundary Add returned error: %v", err)
	}
	if underflowBoundary.Value.String() != "0.00001" {
		t.Fatalf("Underflow boundary value = %s, want 0.00001", underflowBoundary.Value)
	}
	if !underflowBoundary.Status.Underflow {
		t.Fatalf("Underflow boundary status = %+v", underflowBoundary.Status)
	}

	withTraps := withoutTraps
	withTraps.Traps = Traps{Overflow: true, Underflow: true}

	_, err = withTraps.Mul(New(99, 0), New(10, 0))
	overflowErr, ok := err.(*OverflowError)
	if !ok {
		t.Fatalf("Mul error = %T, want *OverflowError", err)
	}
	if overflowErr.Operation != "mul" || !overflowErr.Status().Overflow {
		t.Fatalf("OverflowError = %+v", overflowErr)
	}

	_, err = withTraps.Add(New(1, -6), New(0, 0))
	underflowErr, ok := err.(*UnderflowError)
	if !ok {
		t.Fatalf("Add error = %T, want *UnderflowError", err)
	}
	if underflowErr.Operation != "add" || !underflowErr.Status().Underflow {
		t.Fatalf("UnderflowError = %+v", underflowErr)
	}
}

func TestContextTraps(t *testing.T) {
	ctx := Context{
		Precision:   2,
		MinExponent: -10,
		MaxExponent: 10,
		Rounding:    HalfUp,
		Traps:       Traps{Rounded: true},
	}

	_, err := ctx.Add(New(1200, -3), New(1800, -3))
	roundedErr, ok := err.(*RoundedError)
	if !ok {
		t.Fatalf("exact rounding error = %T, want *RoundedError", err)
	}
	if roundedErr.Operation != "add" || !roundedErr.Status().Rounded {
		t.Fatalf("RoundedError = %+v", roundedErr)
	}

	ctx.Traps = Traps{Inexact: true}
	_, err = ctx.Add(New(126, -2), New(0, 0))
	inexactErr, ok := err.(*InexactError)
	if !ok {
		t.Fatalf("inexact error = %T, want *InexactError", err)
	}
	if inexactErr.Operation != "add" || !inexactErr.Status().Inexact {
		t.Fatalf("InexactError = %+v", inexactErr)
	}
}

func TestContextDivisionByZero(t *testing.T) {
	ctx := Context{Precision: 2, MinExponent: -10, MaxExponent: 10, Rounding: HalfUp}
	result, err := ctx.Div(New(1, 0), New(0, 0))

	divisionErr, ok := err.(*DivisionByZeroError)
	if !ok {
		t.Fatalf("error = %T, want *DivisionByZeroError", err)
	}
	if !result.Status.DivisionByZero {
		t.Fatalf("result status = %+v, want DivisionByZero", result.Status)
	}
	if divisionErr.Operation != "div" {
		t.Fatalf("Operation = %q, want div", divisionErr.Operation)
	}
}

func TestContextInvalidContext(t *testing.T) {
	for _, ctx := range []Context{
		{Precision: 0, MinExponent: -1, MaxExponent: 1},
		{Precision: 2, MinExponent: 2, MaxExponent: 1},
		{Precision: 2, MinExponent: -1, MaxExponent: 1, Rounding: RoundingMode(99)},
	} {
		_, err := ctx.Add(New(1, 0), New(1, 0))
		_, ok := err.(InvalidContextError)
		if !ok {
			t.Fatalf("error = %T, want InvalidContextError for %+v", err, ctx)
		}
	}
}
