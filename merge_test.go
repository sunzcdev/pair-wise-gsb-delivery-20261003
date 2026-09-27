package scs

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
)

// controlledStore wraps a MemStore and lets tests hook into the versioned
// operations, so that the interleaving of concurrent requests can be fixed
// deterministically (e.g. forcing a delete to happen between another
// request's find and commit).
type controlledStore struct {
	*memstore.MemStore
	beforeFindVersioned   func(token string)
	beforeCommitVersioned func(token string)
}

func newControlledStore() *controlledStore {
	return &controlledStore{MemStore: memstore.NewWithCleanupInterval(0)}
}

func (c *controlledStore) FindVersioned(token string) ([]byte, uint64, bool, error) {
	if c.beforeFindVersioned != nil {
		c.beforeFindVersioned(token)
	}
	return c.MemStore.FindVersioned(token)
}

func (c *controlledStore) CommitVersioned(token string, b []byte, expiry time.Time, expectedVersion uint64) (bool, error) {
	if c.beforeCommitVersioned != nil {
		c.beforeCommitVersioned(token)
	}
	return c.MemStore.CommitVersioned(token, b, expiry, expectedVersion)
}

func newMergeTestManager() (*SessionManager, *controlledStore) {
	store := newControlledStore()
	s := New()
	s.Store = store
	return s, store
}

func loadForTest(t *testing.T, s *SessionManager, token string) context.Context {
	t.Helper()
	ctx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func commitForTest(t *testing.T, s *SessionManager, ctx context.Context) string {
	t.Helper()
	token, _, err := s.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Two requests for the same session change different keys; both changes must
// survive regardless of commit order.
func TestCommitMergesDisjointFields(t *testing.T) {
	s, _ := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "prefs", "light")
	s.Put(ctx0, "cart", "empty")
	token := commitForTest(t, s, ctx0)

	// Two "pages" load the same baseline.
	ctxPrefs := loadForTest(t, s, token)
	ctxCart := loadForTest(t, s, token)

	s.Put(ctxPrefs, "prefs", "dark")
	s.Put(ctxCart, "cart", "2 items")

	// The cart request commits last; it must not wipe out the prefs change.
	commitForTest(t, s, ctxPrefs)
	commitForTest(t, s, ctxCart)

	ctx := loadForTest(t, s, token)
	if got := s.GetString(ctx, "prefs"); got != "dark" {
		t.Errorf("prefs: got %q, want %q", got, "dark")
	}
	if got := s.GetString(ctx, "cart"); got != "2 items" {
		t.Errorf("cart: got %q, want %q", got, "2 items")
	}
}

// Two requests change the same key differently; the second commit must fail
// with a ConflictError instead of silently winning.
func TestCommitConflictIsVisible(t *testing.T) {
	s, _ := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "prefs", "light")
	token := commitForTest(t, s, ctx0)

	ctxA := loadForTest(t, s, token)
	ctxB := loadForTest(t, s, token)

	s.Put(ctxA, "prefs", "dark")
	s.Put(ctxB, "prefs", "blue")

	commitForTest(t, s, ctxA)

	_, _, err := s.Commit(ctxB)
	if err == nil {
		t.Fatal("expected a conflict error, got nil")
	}

	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected a *ConflictError, got %T (%v)", err, err)
	}
	if !reflect.DeepEqual(conflictErr.Keys, []string{"prefs"}) {
		t.Errorf("conflicting keys: got %v, want %v", conflictErr.Keys, []string{"prefs"})
	}

	// The first committer's value must still be in the store; the losing
	// request must not have silently overwritten it.
	ctx := loadForTest(t, s, token)
	if got := s.GetString(ctx, "prefs"); got != "dark" {
		t.Errorf("prefs: got %q, want %q", got, "dark")
	}
}

// Both requests making the same change to a key is not a conflict.
func TestCommitIdenticalChangesDoNotConflict(t *testing.T) {
	s, _ := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "prefs", "light")
	token := commitForTest(t, s, ctx0)

	ctxA := loadForTest(t, s, token)
	ctxB := loadForTest(t, s, token)

	s.Put(ctxA, "prefs", "dark")
	s.Put(ctxB, "prefs", "dark")

	commitForTest(t, s, ctxA)
	commitForTest(t, s, ctxB)

	ctx := loadForTest(t, s, token)
	if got := s.GetString(ctx, "prefs"); got != "dark" {
		t.Errorf("prefs: got %q, want %q", got, "dark")
	}
}

// One request deletes a key while another modifies it: that is a conflict.
func TestCommitDeleteVersusModifyConflicts(t *testing.T) {
	s, _ := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "cart", "1 item")
	token := commitForTest(t, s, ctx0)

	ctxA := loadForTest(t, s, token)
	ctxB := loadForTest(t, s, token)

	s.Remove(ctxA, "cart")
	s.Put(ctxB, "cart", "2 items")

	commitForTest(t, s, ctxA)

	_, _, err := s.Commit(ctxB)
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected a *ConflictError, got %v", err)
	}
	if !reflect.DeepEqual(conflictErr.Keys, []string{"cart"}) {
		t.Errorf("conflicting keys: got %v, want %v", conflictErr.Keys, []string{"cart"})
	}
}

// A request that loaded the session before it was destroyed must not
// recreate the session when it commits.
func TestStaleCommitDoesNotRecreateDestroyedSession(t *testing.T) {
	s, store := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "user", "alice")
	token := commitForTest(t, s, ctx0)

	// A slow request loads the session...
	ctxStale := loadForTest(t, s, token)
	s.Put(ctxStale, "cart", "1 item")

	// ...meanwhile another request logs the user out (Destroy).
	ctxLogout := loadForTest(t, s, token)
	if err := s.Destroy(ctxLogout); err != nil {
		t.Fatal(err)
	}

	// The stale request commits afterwards. It must not error, and must not
	// recreate the destroyed session.
	if _, _, err := s.Commit(ctxStale); err != nil {
		t.Fatalf("expected stale commit to be discarded silently, got %v", err)
	}

	if _, found, err := store.Find(token); err != nil || found {
		t.Errorf("destroyed session was recreated (found=%v, err=%v)", found, err)
	}
}

// Same as above, but the destroy happens between the stale request's
// versioned find and its compare-and-swap commit.
func TestDestroyBetweenFindAndCommitDoesNotRecreate(t *testing.T) {
	s, store := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "user", "alice")
	token := commitForTest(t, s, ctx0)

	ctxStale := loadForTest(t, s, token)
	s.Put(ctxStale, "cart", "1 item")

	// Force the session to be deleted mid-commit, after the stale request
	// has found it in the store but before it commits.
	store.beforeCommitVersioned = func(string) {
		if err := store.Delete(token); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := s.Commit(ctxStale); err != nil {
		t.Fatalf("expected stale commit to be discarded silently, got %v", err)
	}

	if _, found, err := store.Find(token); err != nil || found {
		t.Errorf("destroyed session was recreated (found=%v, err=%v)", found, err)
	}
}

// A concurrent commit between find and commit must trigger a merge retry
// against the newer state rather than a lost update.
func TestCommitRetriesOnConcurrentModification(t *testing.T) {
	s, store := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "prefs", "light")
	token := commitForTest(t, s, ctx0)

	ctxA := loadForTest(t, s, token)
	ctxB := loadForTest(t, s, token)

	s.Put(ctxA, "prefs", "dark")
	s.Put(ctxB, "cart", "2 items")

	// Sneak request A's commit in between request B's find and commit, so
	// that B's first compare-and-swap attempt fails and has to retry.
	store.beforeCommitVersioned = func(string) {
		store.beforeCommitVersioned = nil
		commitForTest(t, s, ctxA)
	}

	commitForTest(t, s, ctxB)

	ctx := loadForTest(t, s, token)
	if got := s.GetString(ctx, "prefs"); got != "dark" {
		t.Errorf("prefs: got %q, want %q", got, "dark")
	}
	if got := s.GetString(ctx, "cart"); got != "2 items" {
		t.Errorf("cart: got %q, want %q", got, "2 items")
	}
}

// A commit after RenewToken must behave like a new session commit, even
// though the data was originally loaded from the store.
func TestCommitAfterRenewToken(t *testing.T) {
	s, store := newMergeTestManager()

	ctx0 := loadForTest(t, s, "")
	s.Put(ctx0, "user", "alice")
	oldToken := commitForTest(t, s, ctx0)

	ctx := loadForTest(t, s, oldToken)
	if err := s.RenewToken(ctx); err != nil {
		t.Fatal(err)
	}
	s.Put(ctx, "prefs", "dark")
	newToken := commitForTest(t, s, ctx)

	if newToken == oldToken {
		t.Fatal("expected a new token after RenewToken")
	}
	if _, found, _ := store.Find(oldToken); found {
		t.Error("old token should have been deleted from the store")
	}

	ctxReload := loadForTest(t, s, newToken)
	if got := s.GetString(ctxReload, "user"); got != "alice" {
		t.Errorf("user: got %q, want %q", got, "alice")
	}
	if got := s.GetString(ctxReload, "prefs"); got != "dark" {
		t.Errorf("prefs: got %q, want %q", got, "dark")
	}
}

func TestThreeWayMerge(t *testing.T) {
	tests := []struct {
		name      string
		baseline  map[string]interface{}
		current   map[string]interface{}
		local     map[string]interface{}
		want      map[string]interface{}
		conflicts []string
	}{
		{
			name:     "disjoint changes merge",
			baseline: map[string]interface{}{"a": 1, "b": 2},
			current:  map[string]interface{}{"a": 10, "b": 2},
			local:    map[string]interface{}{"a": 1, "b": 20},
			want:     map[string]interface{}{"a": 10, "b": 20},
		},
		{
			name:      "same key changed differently conflicts",
			baseline:  map[string]interface{}{"a": 1},
			current:   map[string]interface{}{"a": 10},
			local:     map[string]interface{}{"a": 20},
			want:      map[string]interface{}{},
			conflicts: []string{"a"},
		},
		{
			name:     "same change on both sides is not a conflict",
			baseline: map[string]interface{}{"a": 1},
			current:  map[string]interface{}{"a": 10},
			local:    map[string]interface{}{"a": 10},
			want:     map[string]interface{}{"a": 10},
		},
		{
			name:     "local delete merges with remote change to other key",
			baseline: map[string]interface{}{"a": 1, "b": 2},
			current:  map[string]interface{}{"a": 1, "b": 20},
			local:    map[string]interface{}{"b": 2},
			want:     map[string]interface{}{"b": 20},
		},
		{
			name:      "local delete conflicts with remote modify",
			baseline:  map[string]interface{}{"a": 1},
			current:   map[string]interface{}{"a": 10},
			local:     map[string]interface{}{},
			want:      map[string]interface{}{},
			conflicts: []string{"a"},
		},
		{
			name:     "remote delete merges with local change to other key",
			baseline: map[string]interface{}{"a": 1, "b": 2},
			current:  map[string]interface{}{"b": 2},
			local:    map[string]interface{}{"a": 1, "b": 20},
			want:     map[string]interface{}{"b": 20},
		},
		{
			name:     "new keys added on both sides merge",
			baseline: map[string]interface{}{},
			current:  map[string]interface{}{"a": 1},
			local:    map[string]interface{}{"b": 2},
			want:     map[string]interface{}{"a": 1, "b": 2},
		},
		{
			name:      "same new key added differently conflicts",
			baseline:  map[string]interface{}{},
			current:   map[string]interface{}{"a": 1},
			local:     map[string]interface{}{"a": 2},
			want:      map[string]interface{}{},
			conflicts: []string{"a"},
		},
		{
			name:     "no changes anywhere",
			baseline: map[string]interface{}{"a": 1},
			current:  map[string]interface{}{"a": 1},
			local:    map[string]interface{}{"a": 1},
			want:     map[string]interface{}{"a": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged, conflicts := threeWayMerge(tt.baseline, tt.current, tt.local)
			if !reflect.DeepEqual(merged, tt.want) {
				t.Errorf("merged: got %v, want %v", merged, tt.want)
			}
			if !reflect.DeepEqual(conflicts, tt.conflicts) {
				t.Errorf("conflicts: got %v, want %v", conflicts, tt.conflicts)
			}
		})
	}
}
