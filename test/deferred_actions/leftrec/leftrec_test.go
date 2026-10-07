package leftrec

import (
	"reflect"
	"testing"
)

// Left-recursion seed growth re-parses the rule on each iteration: the
// committed log must hold each action of the final accepted path exactly
// once, in syntactic order.
func TestLeftRecursionDeferredActions(t *testing.T) {
	var log []string
	commit := func(d DeferredAction) error {
		log = append(log, d.Data.(string))
		return nil
	}
	if _, err := Parse("", []byte("1+2+3"), DeferredActions(commit)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"num:1", "num:2", "add:1+2", "num:3", "add:1+2+3"}
	if !reflect.DeepEqual(log, want) {
		t.Errorf("want committed log %v, got %v", want, log)
	}
}

// A failed left-recursive parse commits nothing.
func TestLeftRecursionDeferredActionsFailure(t *testing.T) {
	var log []string
	commit := func(d DeferredAction) error {
		log = append(log, d.Data.(string))
		return nil
	}
	if _, err := Parse("", []byte("1+"), DeferredActions(commit)); err == nil {
		t.Fatal("expected parse error, got none")
	}
	if len(log) != 0 {
		t.Errorf("want no committed action on parse failure, got %v", log)
	}
}
