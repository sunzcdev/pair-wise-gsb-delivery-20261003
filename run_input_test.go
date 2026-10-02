package gojq_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"testing"

	"github.com/itchyny/gojq"
)

// countingIter reads lazily from a channel-like source, counting how many
// values Next has consumed.
type countingIter struct {
	values []int
	reads  int
}

func (iter *countingIter) Next() (any, bool) {
	if iter.reads >= len(iter.values) {
		return nil, false
	}
	v := iter.values[iter.reads]
	iter.reads++
	return v, true
}

type closeCountingIter struct {
	countingIter
	closes int
	err    error
}

func (iter *closeCountingIter) Close() error {
	iter.closes++
	return iter.err
}

type errIter struct {
	err   error
	calls int
}

func (iter *errIter) Next() (any, bool) {
	iter.calls++
	return iter.err, true
}

func mustCompile(t *testing.T, src string, opts ...gojq.CompilerOption) *gojq.Code {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func drain(t *testing.T, iter gojq.Iter) []any {
	t.Helper()
	var vs []any
	for {
		v, ok := iter.Next()
		if !ok {
			return vs
		}
		if err, ok := v.(error); ok {
			t.Fatalf("unexpected error: %v", err)
		}
		vs = append(vs, v)
	}
}

// Two runs bound to distinct input sources must not consume each other's
// data, and the sources have to be read lazily while Next is interleaved.
func TestRunInputIsolationAndLaziness(t *testing.T) {
	code := mustCompile(t, "input, input")
	left := &countingIter{values: []int{1, 2, 3}}
	right := &countingIter{values: []int{10, 20, 30}}
	il := code.RunInput(left, nil)
	ir := code.RunInput(right, nil)

	if left.reads != 0 || right.reads != 0 {
		t.Fatalf("inputs must not be read before Next: %d, %d", left.reads, right.reads)
	}
	v, ok := il.Next()
	if !ok || v != 1 {
		t.Fatalf("expected 1, got: %v (%v)", v, ok)
	}
	if left.reads != 1 || right.reads != 0 {
		t.Fatalf("unexpected reads: left=%d right=%d", left.reads, right.reads)
	}
	v, ok = ir.Next()
	if !ok || v != 10 {
		t.Fatalf("expected 10, got: %v (%v)", v, ok)
	}
	if left.reads != 1 || right.reads != 1 {
		t.Fatalf("unexpected reads: left=%d right=%d", left.reads, right.reads)
	}
	v, ok = il.Next()
	if !ok || v != 2 {
		t.Fatalf("expected 2, got: %v (%v)", v, ok)
	}
	v, ok = ir.Next()
	if !ok || v != 20 {
		t.Fatalf("expected 20, got: %v (%v)", v, ok)
	}
	if v, ok := il.Next(); ok {
		t.Fatalf("expected end of left results, got: %v", v)
	}
	if v, ok := ir.Next(); ok {
		t.Fatalf("expected end of right results, got: %v", v)
	}
	if left.reads != 2 || right.reads != 2 {
		t.Fatalf("each run must consume only its own input: %d, %d", left.reads, right.reads)
	}
}

// inputs over the same source within a run sees all remaining values.
func TestRunInputInputs(t *testing.T) {
	code := mustCompile(t, "reduce inputs as $x (0; . + $x)")
	iter := code.RunInput(&countingIter{values: []int{1, 2, 3, 4, 5}}, nil)
	if vs := drain(t, iter); len(vs) != 1 || vs[0] != 15 {
		t.Fatalf("expected [15], got: %v", vs)
	}
}

// A non-nil run-time iterator wins over the compile-time one; nil falls back.
func TestRunInputPrecedence(t *testing.T) {
	code := mustCompile(t, "input",
		gojq.WithInputIter(gojq.NewIter(0, 1, 2, 3, 4)))

	if vs := drain(t, code.RunInput(gojq.NewIter(100), nil)); len(vs) != 1 || vs[0] != 100 {
		t.Fatalf("run-time iterator must take precedence, got: %v", vs)
	}
	if vs := drain(t, code.Run(nil)); len(vs) != 1 || vs[0] != 0 {
		t.Fatalf("Run must fall back to the compile-time iterator, got: %v", vs)
	}
	if vs := drain(t, code.RunInput(nil, nil)); len(vs) != 1 || vs[0] != 1 {
		t.Fatalf("nil run-time iterator must fall back, got: %v", vs)
	}

	// Each fresh compile-time source is independently resolvable per run.
	other := mustCompile(t, "input", gojq.WithInputIter(gojq.NewIter(7)))
	if vs := drain(t, other.RunInput(nil, nil)); len(vs) != 1 || vs[0] != 7 {
		t.Fatalf("expected the other compile-time iterator, got: %v", vs)
	}
}

// Compile-time configuration keeps working without run-time binding.
func TestRunInputCompileTimeOnly(t *testing.T) {
	code := mustCompile(t, "range(3) | input",
		gojq.WithInputIter(gojq.NewIter("a", "b", "c")))
	if vs := drain(t, code.Run(nil)); len(vs) != 3 ||
		vs[0] != "a" || vs[1] != "b" || vs[2] != "c" {
		t.Fatalf("expected [a b c], got: %v", vs)
	}
}

// input at EOF behaves like jq: it emits a "break" error, which inputs
// catches to terminate its stream normally.
func TestRunInputEOFSemantics(t *testing.T) {
	code := mustCompile(t, "input")
	iter := code.RunInput(gojq.NewIter[int](), nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("input at EOF must emit an error")
	}
	err, ok := v.(error)
	if !ok || err.Error() != "break" {
		t.Fatalf("expected break error, got: %v", v)
	}

	code = mustCompile(t, "[inputs]")
	if vs := drain(t, code.RunInput(gojq.NewIter[int](), nil)); len(vs) != 1 {
		t.Fatalf("inputs at EOF must emit empty array, got: %v", vs)
	}
}

// Backtracking over input(s) consumes exactly one value per evaluation.
func TestRunInputBacktracking(t *testing.T) {
	for _, tc := range []struct {
		src      string
		expected []any
	}{
		{"(input, input, .), (input, input)", []any{10, 20, "X", 30, 40}},
		{"[inputs]", []any{[]any{10, 20, 30, 40, 50}}},
		{"try input catch .", []any{10}},
		{"input?, input?", []any{10, 20}},
	} {
		code := mustCompile(t, tc.src)
		got := drain(t, code.RunInput(gojq.NewIter(10, 20, 30, 40, 50), "X"))
		if !reflect.DeepEqual(got, tc.expected) {
			t.Errorf("%s: expected %#v, got %#v", tc.src, tc.expected, got)
		}
	}
}

// An error from the input source propagates as an emitted result error.
func TestRunInputSourceError(t *testing.T) {
	want := errors.New("source failure")
	code := mustCompile(t, "input")
	iter := code.RunInput(&errIter{err: want}, nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("expected an error value")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, want) {
		t.Fatalf("expected source error, got: %v", v)
	}
}

// A run owns only the run-time source; stopping early then closing releases
// it exactly once. Fully draining does not close it automatically.
func TestRunInputCloseContract(t *testing.T) {
	code := mustCompile(t, "input",
		gojq.WithInputIter(gojq.NewIter(0)))

	src := &closeCountingIter{countingIter: countingIter{values: []int{1, 2, 3}}}
	iter := code.RunInput(src, nil)
	if v, ok := iter.Next(); !ok || v != 1 {
		t.Fatalf("expected 1, got: %v (%v)", v, ok)
	}
	// Stop the run early; the owned source must be closable once.
	closer, ok := iter.(interface{ Close() error })
	if !ok {
		t.Fatal("the result iterator must implement io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close must be idempotent: %v", err)
	}
	if src.closes != 1 {
		t.Fatalf("source must be closed once, got: %d", src.closes)
	}

	// A compile-time source is shared and never closed by runs.
	shared := &closeCountingIter{countingIter: countingIter{values: []int{9}}}
	sharedCode := mustCompile(t, "input", gojq.WithInputIter(shared))
	sharedIter := sharedCode.Run(nil)
	if _, ok := sharedIter.(interface{ Close() error }); !ok {
		t.Fatal("the result iterator must implement io.Closer")
	}
	if err := sharedIter.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if shared.closes != 0 {
		t.Fatalf("compile-time source must not be closed, got: %d", shared.closes)
	}

	// Draining a run does not close its source automatically.
	drained := &closeCountingIter{countingIter: countingIter{values: []int{7}}}
	drain(t, code.RunInput(drained, nil))
	if drained.closes != 0 {
		t.Fatalf("natural termination must not auto-close, got: %d", drained.closes)
	}
}

func TestQueryRunInput(t *testing.T) {
	query, err := gojq.Parse("input + 1")
	if err != nil {
		t.Fatal(err)
	}
	if vs := drain(t, query.RunInput(gojq.NewIter(41), nil)); len(vs) != 1 || vs[0] != 42 {
		t.Fatalf("expected 42, got: %v", vs)
	}
}

// After a run is stopped early by canceling the context, the owned input
// source is released through the returned iterator's Close.
func TestRunInputContextCancelClose(t *testing.T) {
	code := mustCompile(t, "repeat(input)")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &closeCountingIter{countingIter: countingIter{values: []int{1}}}
	iter := code.RunInputWithContext(ctx, src, nil)
	if v, ok := iter.Next(); !ok || v != 1 {
		t.Fatalf("expected 1, got: %v (%v)", v, ok)
	}
	cancel()
	v, ok := iter.Next()
	if !ok {
		t.Fatal("expected the context error")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", v)
	}
	if err := iter.(interface{ Close() error }).Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if src.closes != 1 {
		t.Fatalf("owned source must be closed once, got: %d", src.closes)
	}
}

func ExampleCode_RunInput() {
	query, err := gojq.Parse("input")
	if err != nil {
		log.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		log.Fatal(err)
	}
	for _, v := range []int{1, 2, 3} {
		iter := code.RunInput(gojq.NewIter(v), nil)
		x, ok := iter.Next()
		if !ok {
			return
		}
		fmt.Println(x)
	}
	// Output:
	// 1
	// 2
	// 3
}
