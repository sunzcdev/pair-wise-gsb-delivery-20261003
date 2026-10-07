package deferredleftrecopt

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

func parseWithLog(t *testing.T, input string) []string {
	t.Helper()
	var got []DeferredAction
	v, err := Parse("test", []byte(input), SetDeferredCommitter(
		func(acts []DeferredAction) (int, error) {
			got = append(got, acts...)
			return len(acts), nil
		}))
	if err != nil {
		t.Fatalf("input %q: unexpected error: %v", input, err)
	}
	if v == nil {
		t.Fatalf("input %q: nil result", input)
	}
	return values(got)
}

func TestLeftRecursionSeedGrowthDedup(t *testing.T) {
	// "1+2+3": the base atom "1" is revisited for every seed-growth
	// attempt while growing the left-recursive expr. Its delayed action
	// must survive exactly once per atom, and the add actions once per
	// accepted '+', all in grammar order.
	got := parseWithLog(t, "1+2+3")
	want := []string{"term:1", "term:2", "add", "term:3", "add"} // nested terms register before their enclosing add completes
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v", got, want)
	}
}

func TestLeftRecursionSingle(t *testing.T) {
	got := parseWithLog(t, "7")
	if want := []string{"term:7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v", got, want)
	}
}

func TestLeftRecursionOneAdd(t *testing.T) {
	got := parseWithLog(t, "1+2")
	if want := []string{"term:1", "term:2", "add"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v", got, want)
	}
}
