package gojq_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/itchyny/gojq"
)

// countingIter emits 1, 2, 3, ... up to max values (infinitely when max is
// negative), and counts the number of reads. When errAt is positive, the
// errAt-th read returns err instead of a value.
type countingIter struct {
	count, max, errAt int
	err               error
}

func (iter *countingIter) Next() (any, bool) {
	iter.count++
	if iter.errAt > 0 && iter.count == iter.errAt {
		return iter.err, true
	}
	if iter.max >= 0 && iter.count > iter.max {
		return nil, false
	}
	return iter.count, true
}

func compileTopKBy(t *testing.T, iter gojq.Iter) *gojq.Code {
	t.Helper()
	query, err := gojq.Parse("top_k_by($k; gen; .)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithVariables([]string{"$k"}),
		gojq.WithIterFunction("gen", 0, 0, func(any, []any) gojq.Iter {
			return iter
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestTopKByReadCount(t *testing.T) {
	for _, tc := range []struct {
		k, max, wantReads int
		want              []any
	}{
		{0, 10, 0, []any{}},
		{1, 10, 11, []any{10}},
		{3, 10, 11, []any{10, 9, 8}},
		{10, 10, 11, []any{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}},
		{20, 10, 11, []any{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}},
	} {
		iter := &countingIter{max: tc.max}
		code := compileTopKBy(t, iter)
		got, ok := code.Run(nil, tc.k).Next()
		if !ok {
			t.Fatalf("k=%d: expected a value", tc.k)
		}
		if err, isErr := got.(error); isErr {
			t.Fatalf("k=%d: unexpected error: %v", tc.k, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("k=%d: got %v, want %v", tc.k, got, tc.want)
		}
		if iter.count != tc.wantReads {
			t.Errorf("k=%d: generator was read %d times, want %d", tc.k, iter.count, tc.wantReads)
		}
	}
}

func TestTopKByZeroCountDoesNotConsumeGenerator(t *testing.T) {
	iter := &countingIter{max: -1} // infinite generator
	code := compileTopKBy(t, iter)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, ok := code.RunWithContext(ctx, nil, 0).Next()
	if !ok {
		t.Fatal("expected a value")
	}
	if err, isErr := got.(error); isErr {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []any{}) {
		t.Errorf("got %v, want []", got)
	}
	if iter.count != 0 {
		t.Errorf("generator was read %d times, want 0", iter.count)
	}
}

func TestTopKByInvalidCount(t *testing.T) {
	for _, tc := range []struct {
		k    any
		want string
	}{
		{-1, "top_k_by doesn't support negative count"},
		{-2.5, "top_k_by count must be an integer"},
		{0.5, "top_k_by count must be an integer"},
		{"3", "top_k_by count must be an integer"},
		{nil, "top_k_by count must be an integer"},
		{true, "top_k_by count must be an integer"},
	} {
		iter := &countingIter{max: -1} // infinite generator
		code := compileTopKBy(t, iter)
		got, ok := code.Run(nil, tc.k).Next()
		if !ok {
			t.Fatalf("k=%v: expected an error", tc.k)
		}
		err, isErr := got.(error)
		if !isErr {
			t.Fatalf("k=%v: got %v, want an error", tc.k, got)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("k=%v: got error %q, want %q", tc.k, err, tc.want)
		}
		if iter.count != 0 {
			t.Errorf("k=%v: generator was read %d times, want 0", tc.k, iter.count)
		}
	}
}

func TestTopKByGeneratorError(t *testing.T) {
	iter := &countingIter{max: 10, errAt: 4, err: errors.New("boom")}
	code := compileTopKBy(t, iter)
	queryIter := code.Run(nil, 2)
	got, ok := queryIter.Next()
	if !ok {
		t.Fatal("expected an error")
	}
	err, isErr := got.(error)
	if !isErr || err.Error() != "boom" {
		t.Fatalf("got %v, want boom error", got)
	}
	if iter.count != 4 {
		t.Errorf("generator was read %d times, want 4", iter.count)
	}
}

func TestTopKByCancellation(t *testing.T) {
	iter := &countingIter{max: -1} // infinite generator
	code := compileTopKBy(t, iter)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, ok := code.RunWithContext(ctx, nil, 5).Next()
	if !ok {
		t.Fatal("expected the context error")
	}
	err, isErr := got.(error)
	if !isErr || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context deadline exceeded error", got)
	}
}
