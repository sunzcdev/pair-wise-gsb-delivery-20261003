package deferred

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func values(acts []DeferredAction) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Value.(string))
	}
	return out
}

func parseWithLog(t *testing.T, input string) ([]string, error) {
	t.Helper()
	var committedLog []DeferredAction
	_, err := Parse("test", []byte(input), SetDeferredCommitter(
		func(acts []DeferredAction) (int, error) {
			committedLog = append(committedLog, acts...)
			return len(acts), nil
		}))
	if err != nil {
		return nil, err
	}
	return values(committedLog), nil
}

func TestDelayedAcceptPath(t *testing.T) {
	got, err := parseWithLog(t, "ab")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"win:a", "win:b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v", got, want)
	}
}

func TestDelayedFailedBranchRollback(t *testing.T) {
	// FailLate matches a letter and registers "late", but the missing
	// trailing "z" fails the branch; it must roll back so only Win commits.
	got, err := parseWithLog(t, "ab")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"win:a", "win:b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v (failed branch leaked)", got, want)
	}
}

func TestDelayedFailedBranchKeptWhenSucceeds(t *testing.T) {
	// With a trailing "z" the FailLate branch genuinely wins, so its
	// registered action is committed (and Win is not tried).
	got, err := parseWithLog(t, "azb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"late", "win:b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v", got, want)
	}
}

func TestDelayedMultipleOrder(t *testing.T) {
	got, err := parseWithLog(t, "abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"win:a", "win:b", "win:c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("committed log = %v, want %v", got, want)
	}
}

func TestDelayedNoCommitOnParseFailure(t *testing.T) {
	var called bool
	_, err := Parse("test", []byte("a!"),
		SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
			called = true
			return len(acts), nil
		}))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if called {
		t.Fatal("committer must not be called on a failed parse")
	}
}

func TestDelayedLookaheadNotCommitted(t *testing.T) {
	check := func(t *testing.T, rule string) {
		t.Helper()
		var got []DeferredAction
		_, err := Parse("test", []byte("a"),
			Entrypoint(rule),
			SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
				got = append(got, acts...)
				return len(acts), nil
			}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("%s lookahead committed actions: %v", rule, values(got))
		}
	}
	check(t, "Peek")
	check(t, "PeekNot")
}

var errBoomCommit = errors.New("boom")

func TestDelayedCommitBoundary(t *testing.T) {
	// Committer fails after the first two actions; the error must report
	// the committed boundary (2 of 3).
	_, err := Parse("test", []byte("abc"),
		SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
			if len(acts) != 3 {
				t.Errorf("committer received %d actions, want 3", len(acts))
			}
			if got := values(acts); !reflect.DeepEqual(got,
				[]string{"win:a", "win:b", "win:c"}) {
				t.Errorf("committer order = %v", got)
			}
			return 2, errBoomCommit
		}))
	if err == nil {
		t.Fatal("expected commit error, got nil")
	}
	var ce *DeferredCommitError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *DeferredCommitError in chain, got %T: %v", err, err)
	}
	if ce.Committed != 2 || ce.Total != 3 {
		t.Fatalf("boundary = committed %d total %d, want 2/3", ce.Committed, ce.Total)
	}
	if !errors.Is(err, errBoomCommit) {
		t.Fatalf("error does not wrap errBoomCommit: %v", err)
	}
	if !strings.Contains(err.Error(), "2 of 3") {
		t.Fatalf("error message missing boundary: %v", err)
	}
}

func TestDelayedMetadata(t *testing.T) {
	var got []DeferredAction
	_, err := Parse("test", []byte("a"),
		SetDeferredCommitter(func(acts []DeferredAction) (int, error) {
			got = append(got, acts...)
			return len(acts), nil
		}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 action, got %d", len(got))
	}
	if got[0].Rule != "Win" {
		t.Errorf("Rule = %q, want %q", got[0].Rule, "Win")
	}
	if got[0].PosOffset < 0 || got[0].PosLine == 0 {
		t.Errorf("action position not captured: line=%d offset=%d",
			got[0].PosLine, got[0].PosOffset)
	}
}
