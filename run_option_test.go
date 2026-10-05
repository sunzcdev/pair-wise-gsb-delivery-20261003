package gojq_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/itchyny/gojq"
)

// drain runs the iterator to completion and collects emitted values and
// errors. It fails the test if the iteration does not finish within a second,
// so an unstopped query breaks the test instead of hanging.
func drain(t *testing.T, iter gojq.Iter) (values []any, errs []error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			v, ok := iter.Next()
			if !ok {
				return
			}
			if e, ok := v.(error); ok {
				errs = append(errs, e)
			} else {
				values = append(values, v)
			}
		}
	}()
	select {
	case <-done:
		return values, errs
	case <-time.After(time.Second):
		t.Fatalf("iteration did not stop")
	}
	return nil, nil
}

func limitError(errs []error) *gojq.InstructionLimitError {
	for _, e := range errs {
		var le *gojq.InstructionLimitError
		if errors.As(e, &le) {
			return le
		}
	}
	return nil
}

func TestInstructionLimit_InfiniteRecursion(t *testing.T) {
	query, err := gojq.Parse("def f: f; f")
	if err != nil {
		t.Fatal(err)
	}
	iter := query.Run(nil, gojq.WithInstructionLimit(1000))
	values, errs := drain(t, iter)
	le := limitError(errs)
	if le == nil {
		t.Fatalf("expected an instruction limit error, got values: %v, errs: %v", values, errs)
	}
	if len(values) != 0 {
		t.Errorf("expected no values, got: %v", values)
	}
	if le.Used() != le.Limit() || le.Limit() != 1000 {
		t.Errorf("unexpected used/limit: %d/%d", le.Used(), le.Limit())
	}
}

func TestInstructionLimit_InfiniteRecursionWithOutputs(t *testing.T) {
	// The limit is shared across all emitted results: an unbounded recursive
	// query that emits values terminates instead of running forever.
	query, err := gojq.Parse("def f: ., f; f")
	if err != nil {
		t.Fatal(err)
	}
	const limit = 5000
	iter := query.Run(nil, gojq.WithInstructionLimit(limit))
	values, errs := drain(t, iter)
	if limitError(errs) == nil {
		t.Fatalf("expected an instruction limit error, got %d values, errs: %v", len(values), errs)
	}
	if uint64(len(values)) > limit {
		t.Errorf("emitted %d values under a %d instruction limit", len(values), limit)
	}
	if values, errs := drain(t, iter); values != nil || errs != nil {
		t.Errorf("iterator should be terminated after the limit error")
	}
}

func TestInstructionLimit_ManyResults(t *testing.T) {
	// range(infinite) generates an unbounded number of results; the budget is
	// not reset for each result, so iteration terminates.
	query, err := gojq.Parse("range(infinite)")
	if err != nil {
		t.Fatal(err)
	}
	const limit = 10000
	iter := query.Run(nil, gojq.WithInstructionLimit(limit))
	values, errs := drain(t, iter)
	if limitError(errs) == nil {
		t.Fatalf("expected an instruction limit error, got %d values", len(values))
	}
	if uint64(len(values)) > limit {
		t.Errorf("emitted %d values under a %d instruction limit", len(values), limit)
	}
	for i, v := range values {
		if v != i {
			t.Errorf("value at %d: expected %d, got: %v", i, i, v)
			break
		}
	}
}

func TestInstructionLimit_Backtracking(t *testing.T) {
	// An empty-producing branch followed by a fork keeps the VM backtracking;
	// the backtracking opcodes are charged too.
	query, err := gojq.Parse("def f: empty, f; f")
	if err != nil {
		t.Fatal(err)
	}
	iter := query.Run(nil, gojq.WithInstructionLimit(2000))
	values, errs := drain(t, iter)
	if len(values) != 0 {
		t.Errorf("expected no values, got: %v", values)
	}
	if limitError(errs) == nil {
		t.Fatalf("expected an instruction limit error, got: %v", errs)
	}
}

func TestInstructionLimit_NotCaughtByTryCatch(t *testing.T) {
	for _, src := range []string{
		"def f: f; try f catch \"caught\"",
		"def f: f; (f) // \"caught\"",
		"def f: f; try (f, 1) catch \"caught\"",
	} {
		query, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		values, errs := drain(t, query.Run(nil, gojq.WithInstructionLimit(1000)))
		if le := limitError(errs); le == nil {
			t.Errorf("%q: expected an instruction limit error, got values: %v, errs: %v",
				src, values, errs)
		}
		for _, v := range values {
			if s, ok := v.(string); ok && s == "caught" {
				t.Errorf("%q: instruction limit error must not be caught, got %q", src, s)
			}
		}
	}
}

func TestInstructionLimit_OrdinaryErrorsCaught(t *testing.T) {
	query, err := gojq.Parse("try error(\"boom\") catch .")
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, query.Run(nil, gojq.WithInstructionLimit(1000)))
	if len(errs) != 0 {
		t.Fatalf("ordinary error should be caught, got: %v", errs)
	}
	if len(values) != 1 || values[0] != "boom" {
		t.Errorf("expected the caught error message, got: %v", values)
	}
}

func TestInstructionLimit_NotResetPerResult(t *testing.T) {
	// The limit applies to the whole run: a query with many successful
	// results followed by an infinite branch terminates on the shared budget.
	query, err := gojq.Parse("(range(100)), (def f: ., f; f)")
	if err != nil {
		t.Fatal(err)
	}
	iter := query.Run(nil, gojq.WithInstructionLimit(2000))
	values, errs := drain(t, iter)
	if limitError(errs) == nil {
		t.Fatalf("expected an instruction limit error, got %d values", len(values))
	}
	if uint64(len(values)) >= 2000 {
		t.Errorf("expected the budget to be shared across results, got %d values", len(values))
	}
}

func TestInstructionLimit_NoLimitRunsFully(t *testing.T) {
	query, err := gojq.Parse("range(100) | . * 2")
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, query.Run(nil))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(values) != 100 || values[99] != 198 {
		t.Errorf("unexpected results: %v...%v", values[:2], values[len(values)-2:])
	}
}

func TestInstructionLimit_Zero(t *testing.T) {
	query, err := gojq.Parse(". + 1")
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, query.Run(1, gojq.WithInstructionLimit(0)))
	if limitError(errs) == nil {
		t.Fatalf("zero limit should abort on the first instruction, values: %v, errs: %v",
			values, errs)
	}
	if len(values) != 0 {
		t.Errorf("expected no values, got: %v", values)
	}
}

func TestInstructionLimit_BuiltinInternalsNotCharged(t *testing.T) {
	// Building the million-character string happens entirely inside built-in
	// Go functions, which are not metered automatically; a small limit still
	// allows the query to complete.
	query, err := gojq.Parse("\"x\" * 1000000 | length")
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, query.Run(nil, gojq.WithInstructionLimit(100)))
	if le := limitError(errs); le != nil {
		t.Fatalf("builtin internals must not be charged, got: %v", le)
	}
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(values) != 1 || values[0] != 1000000 {
		t.Errorf("unexpected values: %v", values)
	}
}

func TestInstructionLimit_SharedBudgetAcrossRuns(t *testing.T) {
	budget := gojq.NewInstructionBudget(500)
	query, err := gojq.Parse("range(100)")
	if err != nil {
		t.Fatal(err)
	}
	_, errs := drain(t, query.Run(nil, gojq.WithInstructionBudget(budget)))
	if limitError(errs) != nil {
		t.Fatalf("first run should fit in the budget, errs: %v", errs)
	}
	usedAfterFirst := budget.Used()
	if usedAfterFirst == 0 || usedAfterFirst > 500 {
		t.Fatalf("unexpected used count after first run: %d", usedAfterFirst)
	}
	var terminated bool
	for range 50 {
		_, errs := drain(t, query.Run(nil, gojq.WithInstructionBudget(budget)))
		if limitError(errs) != nil {
			terminated = true
			break
		}
	}
	if !terminated {
		t.Errorf("shared budget should eventually stop a run; used: %d", budget.Used())
	}
	if budget.Used() != budget.Limit() {
		t.Errorf("used (%d) must not exceed limit (%d)", budget.Used(), budget.Limit())
	}
}

func TestInstructionLimit_ExtensionCharge(t *testing.T) {
	budget := gojq.NewInstructionBudget(100)
	query, err := gojq.Parse("expensive")
	if err != nil {
		t.Fatal(err)
	}
	var charged bool
	code, err := gojq.Compile(query,
		gojq.WithFunction("expensive", 0, 0, func(v any, args []any) any {
			// The extension proactively charges for its own work.
			if e := budget.Charge(1000); e != nil {
				return e
			}
			charged = true
			return "unexpected"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, code.RunWithOptions(nil, nil,
		gojq.WithInstructionBudget(budget)))
	if limitError(errs) == nil {
		t.Fatalf("expected extension charge to terminate the run, values: %v, errs: %v",
			values, errs)
	}
	if len(values) != 0 {
		t.Errorf("expected no values, got: %v", values)
	}
	if charged {
		t.Errorf("control flow must not continue after the limit error")
	}
}

func TestInstructionLimit_ExtensionChargeUncatchable(t *testing.T) {
	budget := gojq.NewInstructionBudget(10)
	query, err := gojq.Parse("try heavy catch \"caught\"")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("heavy", 0, 0, func(v any, args []any) gojq.Iter {
			return gojq.NewIter(budget.Charge(100))
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, code.RunWithOptions(nil, nil,
		gojq.WithInstructionBudget(budget)))
	if limitError(errs) == nil {
		t.Fatalf("expected an instruction limit error, values: %v, errs: %v", values, errs)
	}
	for _, v := range values {
		if s, ok := v.(string); ok && s == "caught" {
			t.Errorf("extension limit error must not be caught")
		}
	}
}

func TestInstructionLimit_ExtensionWithoutBudgetNoop(t *testing.T) {
	query, err := gojq.Parse("try heavy catch \"caught\"")
	if err != nil {
		t.Fatal(err)
	}
	var budget *gojq.InstructionBudget
	code, err := gojq.Compile(query,
		gojq.WithFunction("heavy", 0, 0, func(v any, args []any) any {
			if e := budget.Charge(100); e != nil {
				return e
			}
			return "ok"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	values, errs := drain(t, code.Run(nil))
	if len(errs) != 0 {
		t.Fatalf("charge without a budget must be a no-op, got: %v", errs)
	}
	if len(values) != 1 || values[0] != "ok" {
		t.Errorf("unexpected values: %v", values)
	}
}

func TestInstructionLimit_ErrorString(t *testing.T) {
	query, err := gojq.Parse("def f: f; f")
	if err != nil {
		t.Fatal(err)
	}
	_, errs := drain(t, query.Run(nil, gojq.WithInstructionLimit(42)))
	le := limitError(errs)
	if le == nil {
		t.Fatal("expected an instruction limit error")
	}
	if !strings.Contains(le.Error(), "instruction limit exceeded") {
		t.Errorf("unexpected error message: %s", le)
	}
}
