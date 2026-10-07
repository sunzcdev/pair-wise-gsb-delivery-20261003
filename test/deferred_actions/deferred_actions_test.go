package deferredactions

import (
	"errors"
	"reflect"
	"testing"
)

func recordCommit(log *[]string) func(DeferredAction) error {
	return func(d DeferredAction) error {
		*log = append(*log, d.Data.(string))
		return nil
	}
}

// The committed log must correspond exactly to the accepted parse path:
// registrations from branches abandoned by backtracking are discarded and
// registrations inside predicates are never committed.
func TestDeferredActionsAcceptedPathOnly(t *testing.T) {
	var log []string
	if _, err := Parse("", []byte("aabc"), DeferredActions(recordCommit(&log))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a", "a1", "ab"}
	if !reflect.DeepEqual(log, want) {
		t.Errorf("want committed log %v, got %v", want, log)
	}
}

// Without the DeferredActions option, registrations are simply never
// committed and parsing behaves as before.
func TestDeferredActionsDisabled(t *testing.T) {
	got, err := Parse("", []byte("a"), Entrypoint("Tok"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a" {
		t.Errorf("want %q, got %v", "a", got)
	}
}

// On parse failure no deferred action is committed.
func TestDeferredActionsParseFailure(t *testing.T) {
	var log []string
	if _, err := Parse("", []byte("ab"), DeferredActions(recordCommit(&log))); err == nil {
		t.Fatal("expected parse error, got none")
	}
	if len(log) != 0 {
		t.Errorf("want no committed action on parse failure, got %v", log)
	}
}

// With or without memoization, the deferred actions of a rule reused via
// the memoization table are committed exactly once.
func TestDeferredActionsMemoize(t *testing.T) {
	for _, memoize := range []bool{false, true} {
		var log []string
		_, err := Parse("", []byte("ac"),
			Entrypoint("Memo"),
			Memoize(memoize),
			DeferredActions(recordCommit(&log)))
		if err != nil {
			t.Fatalf("memoize=%v: unexpected error: %v", memoize, err)
		}
		want := []string{"tok"}
		if !reflect.DeepEqual(log, want) {
			t.Errorf("memoize=%v: want committed log %v, got %v", memoize, want, log)
		}
	}
}

// When the commit function fails, the returned error reports how many
// actions were committed before the failure.
func TestDeferredActionsCommitFailureBoundary(t *testing.T) {
	errCommit := errors.New("commit failed")
	var log []string
	commit := func(d DeferredAction) error {
		if d.Data.(string) == "a1" {
			return errCommit
		}
		log = append(log, d.Data.(string))
		return nil
	}
	_, err := Parse("", []byte("aabc"), DeferredActions(commit))
	if err == nil {
		t.Fatal("expected commit error, got none")
	}
	var commitErr *DeferredCommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("want *DeferredCommitError, got %T: %v", err, err)
	}
	if commitErr.Committed != 1 {
		t.Errorf("want 1 committed action, got %d", commitErr.Committed)
	}
	if !errors.Is(err, errCommit) {
		t.Errorf("want unwrapped error %v, got %v", errCommit, err)
	}
	if !reflect.DeepEqual(log, []string{"a"}) {
		t.Errorf("want committed log %v, got %v", []string{"a"}, log)
	}
}
