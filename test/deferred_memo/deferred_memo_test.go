package deferredmemo

import (
	"reflect"
	"testing"
)

func values(acts []DeferredAction) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Value.(string))
	}
	return out
}

// With Memoize enabled, reusing a memoized expression must replay the
// actions it registered on its first, successful evaluation, exactly once,
// in grammar order.
func TestDelayedMemoReplay(t *testing.T) {
	// Run twice through the same cached rules to exercise memo hits.
	inputs := map[string][]string{
		"ab":  {"win:a", "win:b"},
		"azb": {"late", "win:b"},
		"abc": {"win:a", "win:b", "win:c"},
	}
	for in, want := range inputs {
		var got []DeferredAction
		_, err := Parse("test", []byte(in),
			Memoize(true),
			SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
				got = append(got, acts...)
				return len(acts), nil
			}))
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		if !reflect.DeepEqual(values(got), want) {
			t.Fatalf("input %q committed log = %v, want %v", in, values(got), want)
		}
	}
}

// A failed parse followed by a successful parse must not leak memoized
// actions from the failed attempt.
func TestDelayedMemoFailureRollback(t *testing.T) {
	var called bool
	_, err := Parse("test", []byte("a!"),
		Memoize(true),
		SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
			called = true
			return len(acts), nil
		}))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if called {
		t.Fatal("committer must not be called on failed parse")
	}

	var got []DeferredAction
	_, err = Parse("test", []byte("ab"),
		Memoize(true),
		SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
			got = append(got, acts...)
			return len(acts), nil
		}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"win:a", "win:b"}; !reflect.DeepEqual(values(got), want) {
		t.Fatalf("after failure, log = %v, want %v", values(got), want)
	}
}
