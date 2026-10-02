package gojq_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/itchyny/gojq"
)

type contextNameKey struct{}

func ExampleWithContextFunction() {
	query, err := gojq.Parse("greeting")
	if err != nil {
		log.Fatalln(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithContextFunction("greeting", 0, 0,
			func(ctx context.Context, _ any, _ []any) any {
				if name, ok := ctx.Value(contextNameKey{}).(string); ok {
					return "Hello, " + name + "!"
				}
				return "Hello, world!"
			},
		),
	)
	if err != nil {
		log.Fatalln(err)
	}
	ctx := context.WithValue(context.Background(), contextNameKey{}, "gojq")
	iter := code.RunWithContext(ctx, nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			log.Fatalln(err)
		}
		fmt.Printf("%#v\n", v)
	}

	// Output:
	// "Hello, gojq!"
}

// Implementation of range/2 using WithContextIterFunction option, which
// releases its resource when the iterator is closed.
type countIter struct {
	value, max int
}

func (iter *countIter) Next() (any, bool) {
	if iter.value >= iter.max {
		return nil, false
	}
	v := iter.value
	iter.value++
	return v, true
}

func (iter *countIter) Close() error {
	fmt.Println("close")
	return nil
}

func ExampleWithContextIterFunction() {
	query, err := gojq.Parse("first(f(3; 7))")
	if err != nil {
		log.Fatalln(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithContextIterFunction("f", 2, 2,
			func(_ context.Context, _ any, xs []any) gojq.Iter {
				if x, ok := xs[0].(int); ok {
					if y, ok := xs[1].(int); ok {
						return &countIter{x, y}
					}
				}
				return gojq.NewIter(fmt.Errorf("f cannot be applied to: %v", xs))
			},
		),
	)
	if err != nil {
		log.Fatalln(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			log.Fatalln(err)
		}
		fmt.Printf("%#v\n", v)
	}

	// Output:
	// 3
	// close
}

func TestWithContextFunction(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithContextFunction("f", 0, 0,
			func(ctx context.Context, _ any, _ []any) any {
				return ctx.Value(contextNameKey{})
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		ctx := context.WithValue(context.Background(), contextNameKey{}, name)
		iter := code.RunWithContext(ctx, nil)
		v, ok := iter.Next()
		if !ok {
			t.Fatal("should emit a value but got no output")
		}
		if v != name {
			t.Errorf("expected: %v, got: %v", name, v)
		}
		if v, ok := iter.Next(); ok {
			t.Errorf("should not emit a value but got: %v", v)
		}
	}
}

func TestWithContextFunctionCancellation(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	code, err := gojq.Compile(
		query,
		gojq.WithContextFunction("f", 0, 0,
			func(ctx context.Context, _ any, _ []any) any {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	iter := code.RunWithContext(ctx, nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, context.Canceled) {
		t.Errorf("expected: %v, got: %v", context.Canceled, v)
	}
}

func TestWithContextFunctionMultipleArities(t *testing.T) {
	query, err := gojq.Parse("f, f(1)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithFunction("f", 0, 0, func(_ any, _ []any) any {
			return "default"
		}),
		gojq.WithContextFunction("f", 1, 1,
			func(ctx context.Context, _ any, _ []any) any {
				return ctx.Value(contextNameKey{})
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), contextNameKey{}, "contextual")
	iter := code.RunWithContext(ctx, nil)
	for _, expected := range []string{"default", "contextual"} {
		v, ok := iter.Next()
		if !ok {
			t.Fatal("should emit a value but got no output")
		}
		if v != expected {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
	}
	if v, ok := iter.Next(); ok {
		t.Errorf("should not emit a value but got: %v", v)
	}
}

type closeCounterIter struct {
	values []any
	index  int
	closed int32
}

func (iter *closeCounterIter) Next() (any, bool) {
	if iter.index >= len(iter.values) {
		return nil, false
	}
	v := iter.values[iter.index]
	iter.index++
	return v, true
}

func (iter *closeCounterIter) Close() error {
	atomic.AddInt32(&iter.closed, 1)
	return nil
}

func (iter *closeCounterIter) closeCount() int {
	return int(atomic.LoadInt32(&iter.closed))
}

type infiniteIter struct {
	value  int
	closed int32
}

func (iter *infiniteIter) Next() (any, bool) {
	v := iter.value
	iter.value++
	return v, true
}

func (iter *infiniteIter) Close() error {
	atomic.AddInt32(&iter.closed, 1)
	return nil
}

func runToCompletion(t *testing.T, iter gojq.Iter) []any {
	t.Helper()
	var vs []any
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		vs = append(vs, v)
	}
	return vs
}

func TestWithContextIterFunctionCloseOnExhaust(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	it := &closeCounterIter{values: []any{1, 2}}
	code, err := gojq.Compile(
		query,
		gojq.WithContextIterFunction("f", 0, 0,
			func(context.Context, any, []any) gojq.Iter {
				return it
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	vs := runToCompletion(t, iter)
	if expected := []any{1, 2}; !reflect.DeepEqual(vs, expected) {
		t.Errorf("expected: %v, got: %v", expected, vs)
	}
	if n := it.closeCount(); n != 1 {
		t.Errorf("expected Close to be called once, got: %d", n)
	}
}

func TestWithContextIterFunctionCloseOnError(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	it := &closeCounterIter{values: []any{1, errors.New("boom")}}
	code, err := gojq.Compile(
		query,
		gojq.WithContextIterFunction("f", 0, 0,
			func(context.Context, any, []any) gojq.Iter {
				return it
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	v, ok := iter.Next()
	if !ok || v != 1 {
		t.Errorf("expected: %v, got: %v", 1, v)
	}
	v, ok = iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); !ok || err.Error() != "boom" {
		t.Errorf("expected: %v, got: %v", "boom", v)
	}
	if v, ok := iter.Next(); ok {
		t.Errorf("should not emit a value but got: %v", v)
	}
	if n := it.closeCount(); n != 1 {
		t.Errorf("expected Close to be called once, got: %d", n)
	}
}

func TestWithContextIterFunctionCloseOnCancel(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	it := &infiniteIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithContextIterFunction("f", 0, 0,
			func(context.Context, any, []any) gojq.Iter {
				return it
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	iter := code.RunWithContext(ctx, nil)
	v, ok := iter.Next()
	if !ok || v != 0 {
		t.Errorf("expected: %v, got: %v", 0, v)
	}
	cancel()
	v, ok = iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, context.Canceled) {
		t.Errorf("expected: %v, got: %v", context.Canceled, v)
	}
	if n := int(atomic.LoadInt32(&it.closed)); n != 1 {
		t.Errorf("expected Close to be called once, got: %d", n)
	}
}

func TestWithContextIterFunctionCloseOnAbandon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		query    string
		expected []any
	}{
		{name: "first", query: "first(f)", expected: []any{0}},
		{name: "limit", query: "limit(2; f)", expected: []any{0, 1}},
		{name: "error", query: `try (f | error("e")) catch .`, expected: []any{"e"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, err := gojq.Parse(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			it := &infiniteIter{}
			code, err := gojq.Compile(
				query,
				gojq.WithContextIterFunction("f", 0, 0,
					func(context.Context, any, []any) gojq.Iter {
						return it
					},
				),
			)
			if err != nil {
				t.Fatal(err)
			}
			iter := code.RunWithContext(context.Background(), nil)
			vs := runToCompletion(t, iter)
			if !reflect.DeepEqual(vs, tc.expected) {
				t.Errorf("expected: %v, got: %v", tc.expected, vs)
			}
			if n := int(atomic.LoadInt32(&it.closed)); n != 1 {
				t.Errorf("expected Close to be called once, got: %d", n)
			}
		})
	}
}

func TestWithIterFunctionCloseOnAbandon(t *testing.T) {
	query, err := gojq.Parse("first(f)")
	if err != nil {
		t.Fatal(err)
	}
	it := &infiniteIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return it
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	vs := runToCompletion(t, iter)
	if expected := []any{0}; !reflect.DeepEqual(vs, expected) {
		t.Errorf("expected: %v, got: %v", expected, vs)
	}
	if n := int(atomic.LoadInt32(&it.closed)); n != 1 {
		t.Errorf("expected Close to be called once, got: %d", n)
	}
}

func TestWithContextIterFunctionSharedCode(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	var iters []*closeCounterIter
	code, err := gojq.Compile(
		query,
		gojq.WithContextIterFunction("f", 0, 0,
			func(ctx context.Context, _ any, _ []any) gojq.Iter {
				it := &closeCounterIter{values: []any{ctx.Value(contextNameKey{})}}
				iters = append(iters, it)
				return it
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "bob"} {
		ctx := context.WithValue(context.Background(), contextNameKey{}, name)
		iter := code.RunWithContext(ctx, nil)
		vs := runToCompletion(t, iter)
		if expected := []any{name}; !reflect.DeepEqual(vs, expected) {
			t.Errorf("expected: %v, got: %v", expected, vs)
		}
	}
	if len(iters) != 2 {
		t.Fatalf("expected 2 iterators, got: %d", len(iters))
	}
	for i, it := range iters {
		if n := it.closeCount(); n != 1 {
			t.Errorf("expected Close of iterator %d to be called once, got: %d", i, n)
		}
	}
}
