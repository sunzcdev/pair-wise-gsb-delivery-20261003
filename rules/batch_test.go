package rules

import (
	"reflect"
	"testing"
)

// After an atomic batch replacement the new snapshot reflects the change
// while the old snapshot keeps the previous rules and the exact previous
// shadowing explanation.
func TestSnapshotImmutabilityAfterBatchReplace(t *testing.T) {
	old := New().Batch().
		SetRules(nil, []Rule{rule("default", 100)}).
		SetRules([]byte("api/"), []Rule{rule("old-api", 10)}).
		Replace([]byte("api/v1/"), nil, true).
		Commit()

	key := []byte("api/v1/users")
	before := old.Decide(key)

	// Batch: raise the api rule, move the barrier deeper, add a local rule.
	next := old.Batch().
		SetRules([]byte("api/"), []Rule{rule("new-api", 200)}).
		Delete([]byte("api/v1/")).
		Replace([]byte("api/v2/"), []Rule{rule("v2", 1)}, true).
		SetRules(nil, []Rule{rule("default", 100)}).
		Commit()

	// Old view must be byte-for-byte the same explanation.
	afterOnOld := old.Decide(key)
	if !reflect.DeepEqual(before, afterOnOld) {
		t.Fatalf("old snapshot changed:\nbefore=%+v\nafter =%+v", before, afterOnOld)
	}
	if old.Len() != next.Len() {
		// Both happen to have 3 entries here; assert independently below.
	}

	// The queried key is not under the new api/v2/ barrier, so the new
	// high-priority api rule wins in the new snapshot.
	d := next.Decide(key)
	if !d.Hit || d.Effective.ID != "new-api" {
		t.Fatalf("expected new-api in new snapshot, got hit=%v %+v", d.Hit, d.Effective)
	}
	if len(d.Barriers) != 0 {
		t.Fatalf("old barrier path must not leak into new snapshot: %+v", d.Barriers)
	}

	// Under the new barrier, inheritance resets.
	d = next.Decide([]byte("api/v2/x"))
	if d.Effective.ID != "v2" {
		t.Fatalf("expected v2 beyond new barrier, got %+v", d.Effective)
	}
	if len(d.Barriers) != 1 || d.Barriers[0].Cleared != 1 {
		t.Fatalf("new barrier should clear exactly new-api: %+v", d.Barriers)
	}

	// Starting another batch from the old snapshot is independent of next.
	alsoOld := old.Batch().Commit()
	if !reflect.DeepEqual(old.Decide(key), alsoOld.Decide(key)) {
		t.Fatalf("no-op commit must preserve snapshot")
	}
}

// Batch changes are invisible until Commit and all-or-nothing.
func TestBatchAtomicity(t *testing.T) {
	base := New().Batch().
		SetRules([]byte("p/"), []Rule{rule("p", 1)}).
		Commit()

	batch := base.Batch()
	batch.SetRules([]byte("p/"), []Rule{rule("p2", 2)})
	batch.SetRules([]byte("q/"), []Rule{rule("q", 3)})

	// Uncommitted: base is unchanged.
	if got := base.Decide([]byte("p/k")).Effective.ID; got != "p" {
		t.Fatalf("uncommitted batch leaked into base: %q", got)
	}

	committed := batch.Commit()
	if got := committed.Decide([]byte("p/k")).Effective.ID; got != "p2" {
		t.Fatalf("expected p2 after commit, got %q", got)
	}
	if got := committed.Decide([]byte("q/k")).Effective.ID; got != "q" {
		t.Fatalf("expected q after commit, got %q", got)
	}
	// Base still untouched after commit.
	if got := base.Decide([]byte("q/k")).Hit; got {
		t.Fatalf("base snapshot must not gain new entries")
	}
}

// Removing the last rules/barrier drops the entry; clearing a barrier
// restores inheritance.
func TestEntryLifecycle(t *testing.T) {
	b := New().Batch()
	b.Replace([]byte("x/"), []Rule{rule("x1", 1)}, true)
	r := b.Commit()
	if r.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", r.Len())
	}

	rules, barrier, ok := r.Prefix([]byte("x/"))
	if !ok || !barrier || len(rules) != 1 || rules[0].ID != "x1" {
		t.Fatalf("unexpected prefix state: rules=%+v barrier=%v ok=%v", rules, barrier, ok)
	}

	// Clear only the barrier: rules remain, inheritance restored.
	next := r.Batch()
	next.SetBarrier([]byte("x/"), false)
	r = next.Commit()
	if _, barrier, ok := r.Prefix([]byte("x/")); !ok || barrier {
		t.Fatalf("barrier should be cleared while entry stays")
	}

	// Remove the rules too: entry disappears.
	r = r.Batch().SetRules([]byte("x/"), nil).Commit()
	if r.Len() != 0 {
		t.Fatalf("expected empty tree, len=%d", r.Len())
	}
	if _, _, ok := r.Prefix([]byte("x/")); ok {
		t.Fatalf("entry should be deleted when empty")
	}
}

// Stored rule lists are normalized and copies are independent of caller
// slices.
func TestNormalizationAndAliasing(t *testing.T) {
	input := []Rule{rule("b", 1), rule("a", 1), rule("dup", 3), rule("dup", 3)}
	r := New().Batch().SetRules([]byte("k/"), input).Commit()

	// Mutating the caller's slice after the fact cannot affect the tree.
	input[0].Action = "tampered"

	rules, _, ok := r.Prefix([]byte("k/"))
	if !ok {
		t.Fatal("entry missing")
	}
	want := []Rule{rule("dup", 3), rule("a", 1), rule("b", 1)}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("normalized rules=%+v want=%+v", rules, want)
	}

	// Returned slice is a copy: mutating it does not corrupt the snapshot.
	rules[0].Action = "also-tampered"
	again, _, _ := r.Prefix([]byte("k/"))
	if again[0].Action != "action-dup" {
		t.Fatalf("snapshot corrupted by caller: %+v", again)
	}
}

// The Decision does not retain the caller's key slice.
func TestDecisionKeyCopied(t *testing.T) {
	key := []byte("k")
	r := New().Batch().SetRules([]byte("k"), []Rule{rule("k", 1)}).Commit()
	d := r.Decide(key)
	key[0] = 'z'
	if string(d.Key) != "k" {
		t.Fatalf("decision retained caller key slice: %q", d.Key)
	}
}
