package gojq_test

import (
	"errors"
	"testing"

	"github.com/itchyny/gojq"
)

// drain runs the iterator and collects the emitted values and the terminal
// error (the error emitted on the final Next). A try-caught business error is
// an ordinary value here.
func drain(t *testing.T, iter gojq.Iter) ([]any, error) {
	t.Helper()
	var values []any
	for {
		v, ok := iter.Next()
		if !ok {
			return values, nil
		}
		if err, ok := v.(error); ok {
			// Any following Next must report termination.
			if v, ok := iter.Next(); ok {
				t.Errorf("iterator should terminate after error but got: %v", v)
			}
			return values, err
		}
		values = append(values, v)
	}
}

func TestInstructionLimitInfiniteRecursion(t *testing.T) {
	query, err := gojq.Parse("def f: f; f")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(1000))
	values, err := drain(t, iter)
	if len(values) != 0 {
		t.Errorf("expected no values, got: %v", values)
	}
	var limitErr *gojq.InstructionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %T %v", err, err)
	}
	if limitErr.Limit != 1000 {
		t.Errorf("expected limit 1000, got: %d", limitErr.Limit)
	}
}

func TestInstructionLimitManyResults(t *testing.T) {
	query, err := gojq.Parse("range(1000000)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(500))
	values, err := drain(t, iter)
	if len(values) == 0 || len(values) > 500 {
		t.Fatalf("expected some values within budget, got %d", len(values))
	}
	var limitErr *gojq.InstructionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %T %v", err, err)
	}
	// The budget must not be reset between results: consecutive prefixes
	// consume strictly more instructions.
	if values[0] != 0 || values[len(values)-1] != len(values)-1 {
		t.Errorf("unexpected emitted range: %v", values)
	}
}

func TestInstructionLimitAcrossNextCalls(t *testing.T) {
	// The same iterator is resumed many times; the limit must cover all of
	// them instead of resetting per result.
	query, err := gojq.Parse("range(100)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(300))
	var n int
	var terminal error
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if e, ok := v.(error); ok {
			terminal = e
			break
		}
		n++
	}
	if n >= 100 {
		t.Fatalf("expected the limit to stop before 100 results, got %d", n)
	}
	var limitErr *gojq.InstructionLimitError
	if !errors.As(terminal, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %v", terminal)
	}
	if v, ok := iter.Next(); ok {
		t.Errorf("expected terminated iterator, got: %v", v)
	}
}

func TestInstructionLimitBacktracking(t *testing.T) {
	// repeat(.)/range rely on forks (branch backtracking); the budget must
	// cover the backtracked instructions too, not only the forward path.
	query, err := gojq.Parse("limit(1000000; repeat(0))")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(400))
	values, err := drain(t, iter)
	if len(values) == 0 {
		t.Fatal("expected some values before the limit")
	}
	var limitErr *gojq.InstructionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %v (n=%d)", err, len(values))
	}
}

func TestInstructionLimitNotCatchableByTry(t *testing.T) {
	for _, src := range []string{
		"def f: f; (try f catch \"caught\")",
		"def f: f; try f catch \"caught\"",
		"(try range(1000000) catch \"caught\")",
		"try (def f: f; f) catch .",
	} {
		query, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		code, err := gojq.Compile(query)
		if err != nil {
			t.Fatal(err)
		}
		iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(300))
		values, err := drain(t, iter)
		for _, v := range values {
			if s, ok := v.(string); ok && len(s) >= 6 && s[:6] == "caught" {
				t.Errorf("limit error was caught by try-catch in %q: %v", src, v)
			}
		}
		var limitErr *gojq.InstructionLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("%q: expected *InstructionLimitError, got values %v err %v", src, values, err)
		}
	}
}

func TestInstructionLimitBusinessErrorStillCatchable(t *testing.T) {
	// An ordinary business error raised inside the budget must be catchable.
	query, err := gojq.Parse(`try error("boom") catch "caught: \(.)"`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(100000))
	values, err := drain(t, iter)
	if err != nil {
		t.Fatalf("expected no terminal error, got: %v", err)
	}
	if len(values) != 1 || values[0] != "caught: boom" {
		t.Fatalf("expected caught business error, got: %v", values)
	}
}

func TestInstructionLimitDisabledByDefault(t *testing.T) {
	query, err := gojq.Parse("range(5)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	// Default Run has no limit.
	iter := code.Run(nil)
	values, err := drain(t, iter)
	if err != nil || len(values) != 5 {
		t.Fatalf("expected 5 values, got %v err %v", values, err)
	}
	// Negative limit disables the limiter.
	iter = code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(-10))
	values, err = drain(t, iter)
	if err != nil || len(values) != 5 {
		t.Fatalf("expected 5 values, got %v err %v", values, err)
	}
}

func TestInstructionLimitZero(t *testing.T) {
	query, err := gojq.Parse("1, 2, 3")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(0))
	_, err = drain(t, iter)
	var limitErr *gojq.InstructionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %v", err)
	}
}

// A metered custom function is not charged automatically; it must call Charge.
// Once Charge returns false the query terminates with the uncatchable error.
func TestMeteredFunctionCharge(t *testing.T) {
	query, err := gojq.Parse("range(1000000) | expensive")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithMeteredFunction("expensive", 0, 0,
			func(m gojq.InstructionMeter, x any, _ []any) any {
				// Simulate 100 instructions of Go-side work per call.
				if !m.Charge(100) {
					return errors.New("should never be observed; limit is terminal")
				}
				return x
			}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(350))
	values, err := drain(t, iter)
	if len(values) == 0 {
		t.Fatalf("expected at least one value")
	}
	var limitErr *gojq.InstructionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %v (values=%v)", err, values)
	}
}

func TestMeteredFunctionNoLimit(t *testing.T) {
	query, err := gojq.Parse("range(3) | expensive")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithMeteredFunction("expensive", 0, 0,
			func(m gojq.InstructionMeter, x any, _ []any) any {
				if !m.Charge(1 << 30) {
					t.Fatal("Charge should succeed without a limit")
				}
				return x
			}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	values, err := drain(t, iter)
	if err != nil || len(values) != 3 {
		t.Fatalf("expected 3 values, got %v err %v", values, err)
	}
}

func TestMeteredFunctionBusinessErrorCatchable(t *testing.T) {
	query, err := gojq.Parse("try boom catch \"ok\"")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithMeteredFunction("boom", 0, 0,
			func(m gojq.InstructionMeter, _ any, _ []any) any {
				m.Charge(5)
				return errors.New("boom")
			}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(1000))
	values, err := drain(t, iter)
	if err != nil {
		t.Fatalf("expected no terminal error, got: %v", err)
	}
	if len(values) != 1 || values[0] != "ok" {
		t.Fatalf("expected caught business error, got: %v", values)
	}
}

func TestMeteredIterFunctionCharge(t *testing.T) {
	query, err := gojq.Parse("takebig")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithMeteredIterFunction("takebig", 0, 0,
			func(m gojq.InstructionMeter, _ any, _ []any) gojq.Iter {
				return &bigIter{meter: m}
			}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithOptions(nil, nil, gojq.WithInstructionLimit(250))
	values, err := drain(t, iter)
	if len(values) == 0 {
		t.Fatal("expected some values before the limit")
	}
	var limitErr *gojq.InstructionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *InstructionLimitError, got: %v (values=%v)", err, values)
	}
}

type bigIter struct {
	meter gojq.InstructionMeter
	n     int
}

func (iter *bigIter) Next() (any, bool) {
	// Each Go-side result accounts for 10 VM instructions.
	if !iter.meter.Charge(10) {
		return nil, false
	}
	iter.n++
	return iter.n, true
}

func TestInstructionLimitErrorMessage(t *testing.T) {
	err := &gojq.InstructionLimitError{Limit: 42}
	if got, expected := err.Error(), "instruction limit exceeded"; got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
	var _ error = err
}
