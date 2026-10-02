package gojq_test

import (
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"sync"
	"testing"

	"github.com/itchyny/gojq"
)

// countingIter is a lazily-read iterator that records how far it advanced.
type countingIter struct {
	values []any
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

// closeIter wraps an Iter and records whether Close was called.
type closeIter struct {
	gojq.Iter
	closed int
}

func (iter *closeIter) Close() error {
	iter.closed++
	return nil
}

func TestRunWithInput_InterleavedIsolation(t *testing.T) {
	query, err := gojq.Parse("[inputs]")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}

	a := &countingIter{values: []any{1, 2, 3}}
	b := &countingIter{values: []any{10, 20, 30, 40}}

	ia := code.RunWith(nil, a)
	ib := code.RunWith(nil, b)

	// The bound iterators must not be touched until results are pulled.
	if a.reads != 0 || b.reads != 0 {
		t.Fatalf("input must be read lazily, got reads a=%d b=%d", a.reads, b.reads)
	}

	got := func(iter gojq.Iter) []any {
		var xs []any
		for {
			v, ok := iter.Next()
			if !ok {
				return xs
			}
			if e, ok := v.(error); ok {
				t.Fatal(e)
			}
			xs = append(xs, v)
		}
	}

	// Interleave the two runs; each must consume only its own iterator.
	va, ok := ia.Next()
	if !ok {
		t.Fatal("expected a result")
	}
	vb, ok := ib.Next()
	if !ok {
		t.Fatal("expected a result")
	}
	if !reflect.DeepEqual(va, []any{1, 2, 3}) {
		t.Errorf("a = %#v, want [1 2 3]", va)
	}
	if !reflect.DeepEqual(vb, []any{10, 20, 30, 40}) {
		t.Errorf("b = %#v, want [10 20 30 40]", vb)
	}
	if a.reads != 3 || b.reads != 4 {
		t.Errorf("reads a=%d b=%d, want 3 and 4", a.reads, b.reads)
	}
	if _, ok := ia.Next(); ok {
		t.Errorf("a should be exhausted")
	}
	if _, ok := ib.Next(); ok {
		t.Errorf("b should be exhausted")
	}

	// A third run over the same code starts from a fresh iterator.
	c := &countingIter{values: []any{7}}
	if xs := got(code.RunWith(nil, c)); !reflect.DeepEqual(xs, []any{[]any{7}}) {
		t.Errorf("c = %#v, want [[7]]", xs)
	}
}

func TestRunWithInput_InputEOF(t *testing.T) {
	// A bare input at EOF surfaces jq's "break" once, then terminates.
	query, err := gojq.Parse("input")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWith(nil, gojq.NewIter[any]())
	v, ok := iter.Next()
	if !ok {
		t.Fatal("bare input at EOF must emit a break error")
	}
	if e, ok := v.(error); !ok || e.Error() != "break" {
		t.Fatalf("got %#v, want a break error", v)
	}
	if _, ok := iter.Next(); ok {
		t.Fatal("the iterator must terminate after the break error")
	}

	// inputs swallows the EOF break, so an empty source yields [] then ends.
	query, err = gojq.Parse("[inputs]")
	if err != nil {
		t.Fatal(err)
	}
	code, err = gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter = code.RunWith(nil, gojq.NewIter[any]())
	v, ok = iter.Next()
	if !ok || !reflect.DeepEqual(v, []any{}) {
		t.Fatalf("inputs at EOF = %#v (%v), want []", v, ok)
	}
	if _, ok := iter.Next(); ok {
		t.Fatal("inputs must terminate normally at EOF")
	}
}

// collect fully drains a result iterator, collecting emitted values.
func collect(iter gojq.Iter) []any {
	var xs []any
	for {
		v, ok := iter.Next()
		if !ok {
			return xs
		}
		xs = append(xs, v)
	}
}

func TestRunWithInput_ErrorPropagation(t *testing.T) {
	inputErr := errors.New("boom")
	query, err := gojq.Parse("[inputs]")
	if err != nil {
		t.Fatal(err)
	}

	// The run-time path must behave identically to the compile-time path:
	// the error surfaces as an error value (re-raised via error/1) and the
	// values collected before it are still produced.
	codeCT, err := gojq.Compile(query, gojq.WithInputIter(
		gojq.NewIter[any](1, inputErr, 3)))
	if err != nil {
		t.Fatal(err)
	}
	codeRT, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	want := collect(codeCT.Run(nil))
	got := collect(codeRT.RunWith(nil, gojq.NewIter[any](1, inputErr, 3)))
	if len(got) != 2 {
		t.Fatalf("got %d values: %#v", len(got), got)
	}
	if e, ok := got[0].(interface{ Value() any }); !ok || e.Value() != "boom" {
		t.Errorf("first value = %#v, want error value %q", got[0], "boom")
	}
	if !reflect.DeepEqual(got[1], []any{1}) {
		t.Errorf("second value = %#v, want [1]", got[1])
	}
	if !sameErrorSequence(got, want) {
		t.Errorf("runtime %#v != compile-time %#v", got, want)
	}
}

func sameErrorSequence(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ea, oka := a[i].(error)
		eb, okb := b[i].(error)
		if oka != okb {
			return false
		}
		if oka {
			if ea.Error() != eb.Error() {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestRunWithInput_NotAllowed(t *testing.T) {
	query, err := gojq.Parse("input")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("expected an error")
	}
	if e, ok := v.(error); !ok || e.Error() != "input(s)/0 is not allowed" {
		t.Fatalf("got %#v, want input(s)/0 is not allowed", v)
	}
}

func TestRunWithInput_Precedence(t *testing.T) {
	query, err := gojq.Parse("[inputs]")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithInputIter(gojq.NewIter(100, 200)))
	if err != nil {
		t.Fatal(err)
	}

	// nil falls back to the compile-time iterator.
	iter := code.RunWith(nil, nil)
	v, _ := iter.Next()
	if !reflect.DeepEqual(v, []any{100, 200}) {
		t.Errorf("compile-time fallback = %#v", v)
	}

	// A run-time iterator takes precedence.
	iter = code.RunWith(nil, gojq.NewIter("a", "b"))
	v, _ = iter.Next()
	if !reflect.DeepEqual(v, []any{"a", "b"}) {
		t.Errorf("run-time override = %#v", v)
	}
}

func TestRunWithInput_CloseContract(t *testing.T) {
	query, err := gojq.Parse("range(10) | input")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}

	// Fully draining closes the owned input exactly once.
	ci := &closeIter{Iter: gojq.NewIter(1, 2, 3)}
	iter := code.RunWith(nil, ci)
	for {
		if _, ok := iter.Next(); !ok {
			break
		}
	}
	if ci.closed != 1 {
		t.Errorf("drained run: closed %d times, want 1", ci.closed)
	}
	if err := iter.(io.Closer).Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
	if ci.closed != 1 {
		t.Errorf("Close must be idempotent, closed %d times", ci.closed)
	}

	// Stopping early without draining does not close; the caller must.
	ci = &closeIter{Iter: gojq.NewIter(1, 2, 3)}
	iter = code.RunWith(nil, ci)
	if _, ok := iter.Next(); !ok {
		t.Fatal("expected a value")
	}
	if ci.closed != 0 {
		t.Fatalf("early stop must not auto-close, closed %d times", ci.closed)
	}
	if err := iter.(io.Closer).Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
	if ci.closed != 1 {
		t.Errorf("explicit Close: closed %d times, want 1", ci.closed)
	}

	// The compile-time iterator is never closed by the engine.
	shared := &closeIter{Iter: gojq.NewIter(1, 2, 3)}
	codeShared, err := gojq.Compile(query, gojq.WithInputIter(shared))
	if err != nil {
		t.Fatal(err)
	}
	si := codeShared.Run(nil)
	for range 2 {
		si.Next()
	}
	if closer, ok := si.(io.Closer); ok {
		closer.Close()
	}
	if shared.closed != 0 {
		t.Errorf("compile-time iterator must not be closed, got %d", shared.closed)
	}
}

func TestRunWithInput_ConcurrentReuse(t *testing.T) {
	query, err := gojq.Parse("[inputs]")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			iter := code.RunWith(nil, gojq.NewIter(i, i+1))
			v, ok := iter.Next()
			if !ok {
				t.Errorf("goroutine %d: no value", i)
				return
			}
			want := []any{i, i + 1}
			if !reflect.DeepEqual(v, want) {
				t.Errorf("goroutine %d: %#v, want %#v", i, v, want)
			}
		}(i)
	}
	wg.Wait()
}

func ExampleCode_RunWith() {
	query, err := gojq.Parse("[inputs]")
	if err != nil {
		log.Fatalln(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		log.Fatalln(err)
	}
	for _, values := range [][]any{{1, 2, 3}, {4, 5}} {
		iter := code.RunWith(nil, gojq.NewIter(values...))
		v, ok := iter.Next()
		if !ok {
			return
		}
		if err, ok := v.(error); ok {
			log.Fatalln(err)
		}
		fmt.Printf("%#v\n", v)
	}

	// Output:
	// []interface {}{1, 2, 3}
	// []interface {}{4, 5}
}
