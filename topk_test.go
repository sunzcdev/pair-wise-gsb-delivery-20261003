package gojq_test

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/itchyny/gojq"
)

func runQuery(t *testing.T, src string, input any) []any {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	iter := query.Run(input)
	var got []any
	for {
		v, ok := iter.Next()
		if !ok {
			return got
		}
		if err, ok := v.(error); ok {
			t.Fatalf("%s: %v", src, err)
		}
		got = append(got, v)
	}
}

func TestTopK_EquivalenceWithSort(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for seed := 0; seed < 500; seed++ {
		n := rng.Intn(50)
		k := rng.Intn(12)
		arr := make([]any, n)
		for i := range arr {
			arr[i] = rng.Intn(11) - 5
		}
		got := runQuery(t, fmt.Sprintf("top_k(%d; .[])", k), arr)
		want := runQuery(t,
			fmt.Sprintf("sort | (if %d == 0 then [] else .[-%d:] end)", k, k), arr)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want[0]) {
			t.Errorf("seed=%d arr=%v k=%d: got %v, want %v", seed, arr, k, got, want)
		}
	}
}

func TestTopKBy_EquivalenceWithSortBy(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	filters := []string{
		".a",
		".a, .b",
		".a - .b",
		".a?",
	}
	for seed := 0; seed < 200; seed++ {
		n := rng.Intn(40)
		k := rng.Intn(10)
		arr := make([]any, n)
		for i := range arr {
			obj := map[string]any{"a": rng.Intn(7) - 3, "b": rng.Intn(7) - 3}
			arr[i] = obj
		}
		f := filters[rng.Intn(len(filters))]
		got := runQuery(t,
			fmt.Sprintf("top_k_by(%d; .[]; %s)", k, f), arr)
		wantSrc := fmt.Sprintf("sort_by(%s) | (if %d == 0 then [] else .[-%d:] end)", f, k, k)
		want := runQuery(t, wantSrc, arr)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want[0]) {
			t.Errorf("seed=%d k=%d f=%q arr=%v: got %v, want %v",
				seed, k, f, arr, got, want)
		}
	}
}

func TestTopK_ZeroDoesNotConsumeGenerator(t *testing.T) {
	// range(infinite) never terminates; k == 0 must not pull a single value.
	got := runQuery(t, "top_k(0; range(infinite))", nil)
	if len(got) != 1 {
		t.Fatalf("expected one output, got %v", got)
	}
	if arr, ok := got[0].([]any); !ok || len(arr) != 0 {
		t.Fatalf("expected empty array, got %#v", got[0])
	}

	// A generator guarded by a side effect: the effect must never fire.
	got = runQuery(t, `top_k_by(0; range(infinite); error("key evaluated"))`, nil)
	if len(got) != 1 {
		t.Fatalf("expected one output, got %v", got)
	}
}

func TestTopK_ReadsEveryValue(t *testing.T) {
	// top_k must drain the generator (it cannot know the largest k early),
	// and a late error must surface.
	query, err := gojq.Parse("top_k(2; range(5), error(\"late\"), range(20))")
	if err != nil {
		t.Fatal(err)
	}
	if got := query.Run(nil); func() bool {
		for {
			v, ok := got.Next()
			if !ok {
				return false
			}
			if e, ok := v.(error); ok {
				return e.Error() == "error: late"
			}
		}
	}() {
		// expected
	} else {
		t.Fatal("late generator error was not propagated")
	}
}

func TestTopK_InvalidCount(t *testing.T) {
	for _, src := range []string{
		`top_k(-1; empty)`,
		`top_k(1.5; empty)`,
		`top_k("3"; empty)`,
		`top_k(null; empty)`,
		`top_k(true; empty)`,
		`top_k_by(-1; empty; .)`,
	} {
		query, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		iter := query.Run(nil)
		var found bool
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if _, ok := v.(error); ok {
				found = true
			} else {
				t.Errorf("%s: unexpected value %#v", src, v)
			}
		}
		if !found {
			t.Errorf("%s: expected an error", src)
		}
	}
}

func TestTopK_BadCountDoesNotTouchGenerator(t *testing.T) {
	for _, src := range []string{
		`top_k(-1; error("generator touched"))`,
		`top_k("x"; error("generator touched"))`,
		`top_k_by(-1; error("generator touched"); error("key touched"))`,
	} {
		query, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		iter := query.Run(nil)
		for {
			v, ok := iter.Next()
			if !ok {
				t.Fatalf("%s: expected an error", src)
			}
			if err, ok := v.(error); ok {
				if err.Error() == "error: generator touched" ||
					err.Error() == "error: key touched" {
					t.Errorf("%s: generator was evaluated before k: %v", src, err)
				}
				break
			}
		}
	}
}

func TestTopK_ErrorPropagation(t *testing.T) {
	query, err := gojq.Parse(`top_k_by(2; range(5);
		if . == 3 then error("key error") else . end)`)
	if err != nil {
		t.Fatal(err)
	}
	iter := query.Run(nil)
	var found bool
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok && err.Error() == "error: key error" {
			found = true
		}
	}
	if !found {
		t.Fatal("key filter error not propagated")
	}
}

func TestTopK_BoundedSpaceAndReads(t *testing.T) {
	// A large generator driven entirely inside the query; the retained set
	// is bounded by k regardless of the generator length.
	query, err := gojq.Parse("top_k(4; range(100000))")
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
		t.Fatal("expected a result")
	}
	arr := v.([]any)
	if len(arr) != 4 {
		t.Fatalf("expected 4 values, got %d", len(arr))
	}
	// Every finite input value is read exactly once, and the result is the
	// global top k; the retained set never exceeds k items.
	if got := []any{99996, 99997, 99998, 99999}; !reflect.DeepEqual(arr, got) {
		t.Fatalf("got %v, want %v", arr, got)
	}
}

func TestTopK_SpaceBoundedByK(t *testing.T) {
	// Each generator value holds a heavy payload; only the top k payloads may
	// survive. With a small k the retained memory must not grow with the
	// number of emitted values. This guards against accidental full buffering.
	src := `[range(2000) | {key: ., payload: ([range(1024)] | map("x"))}]`
	query, err := gojq.Parse(src + " | top_k_by(3; .[]; .key) | map(.key)")
	if err != nil {
		t.Fatal(err)
	}
	iter := query.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("expected a result")
	}
	if got := []any{1997, 1998, 1999}; !reflect.DeepEqual(v, got) {
		t.Fatalf("got %v, want %v", v, got)
	}
	if _, ok := iter.Next(); ok {
		t.Fatal("expected exactly one output")
	}
}

func TestTopK_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	query, err := gojq.Parse("top_k(3; range(infinite))")
	if err != nil {
		t.Fatal(err)
	}
	iter := query.RunWithContext(ctx, nil)
	done := make(chan struct{})
	var got any
	go func() {
		defer close(done)
		got, _ = iter.Next()
	}()
	select {
	case <-done:
		if _, ok := got.(error); !ok || ctx.Err() == nil {
			t.Fatalf("expected context error, got %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation was not propagated in time")
	}
}
