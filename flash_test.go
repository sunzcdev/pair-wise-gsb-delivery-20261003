package scs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
	"github.com/alexedwards/scs/v2/mockstore"
)

// decodeStore is a test helper which decodes the data stored for a token.
func decodeStore(t *testing.T, s *SessionManager, token string) map[string]interface{} {
	t.Helper()

	b, found, err := s.Store.Find(token)
	if err != nil {
		t.Fatalf("error finding token in store: %v", err)
	}
	if !found {
		return nil
	}

	_, values, err := s.Codec.Decode(b)
	if err != nil {
		t.Fatalf("error decoding stored data: %v", err)
	}

	return values
}

// seedSession commits a new session containing values and returns its token and
// a context with the loaded session data.
func seedSession(t *testing.T, s *SessionManager, values map[string]interface{}) (string, context.Context) {
	t.Helper()

	sd := newSessionData(time.Hour, 0)
	ctx := s.addSessionDataToContext(context.Background(), sd)
	for k, v := range values {
		s.Put(ctx, k, v)
	}

	token, _, err := s.Commit(ctx)
	if err != nil {
		t.Fatalf("error committing seed session: %v", err)
	}

	loadedCtx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("error loading seed session: %v", err)
	}

	return token, loadedCtx
}

func TestFlash_LocalOnly(t *testing.T) {
	t.Parallel()

	s := New()
	sd := newSessionData(time.Hour, 0)
	ctx := s.addSessionDataToContext(context.Background(), sd)

	s.Put(ctx, "notice", "welcome back")

	val, found, err := s.Flash(ctx, "notice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found to be true")
	}
	if val.(string) != "welcome back" {
		t.Fatalf("got %q, expected %q", val, "welcome back")
	}
	if s.Exists(ctx, "notice") {
		t.Error("expected key to be removed from local session data")
	}

	token, _, err := s.Commit(ctx)
	if err != nil {
		t.Fatalf("unexpected error committing: %v", err)
	}
	if values := decodeStore(t, s, token); values["notice"] != nil {
		t.Errorf("expected flashed key to be absent from store, got %v", values["notice"])
	}
}

func TestFlash_StoredHappyPath(t *testing.T) {
	t.Parallel()

	s := New()
	token, ctx := seedSession(t, s, map[string]interface{}{
		"notice": "welcome back",
		"other":  "kept",
	})

	val, found, err := s.Flash(ctx, "notice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found to be true")
	}
	if val.(string) != "welcome back" {
		t.Fatalf("got %q, expected %q", val, "welcome back")
	}

	values := decodeStore(t, s, token)
	if _, exists := values["notice"]; exists {
		t.Error("expected flashed key to be absent from store immediately")
	}
	if values["other"] != "kept" {
		t.Errorf("expected other value to be retained, got %v", values["other"])
	}
}

func TestFlash_NoValue(t *testing.T) {
	t.Parallel()

	s := New()
	token, ctx := seedSession(t, s, map[string]interface{}{
		"other": "kept",
	})

	val, found, err := s.Flash(ctx, "notice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("expected found to be false for missing key")
	}
	if val != nil {
		t.Errorf("expected nil value, got %v", val)
	}

	values := decodeStore(t, s, token)
	if values["other"] != "kept" {
		t.Errorf("expected unrelated data to be untouched, got %v", values)
	}
}

func TestFlash_OnlyOneConsumer(t *testing.T) {
	t.Parallel()

	s := New()
	token, ctx := seedSession(t, s, map[string]interface{}{
		"notice": "welcome back",
	})

	val, found, err := s.Flash(ctx, "notice")
	if err != nil || !found || val.(string) != "welcome back" {
		t.Fatalf("first flash got %v, %v, %v", val, found, err)
	}

	val, found, err = s.Flash(ctx, "notice")
	if err != nil {
		t.Fatalf("unexpected error on second flash: %v", err)
	}
	if found {
		t.Error("expected second flash to report no value")
	}
	if val != nil {
		t.Errorf("expected nil on second flash, got %v", val)
	}

	values := decodeStore(t, s, token)
	if _, exists := values["notice"]; exists {
		t.Error("expected flashed key to be absent from store")
	}
}

// flashErrorStore wraps a flash-capable store but always fails the atomic
// flash operation.
type flashErrorStore struct {
	*memstore.MemStore
	err error
}

func (f flashErrorStore) Flash(token string, key string, fn FlashFunc) (bool, error) {
	return false, f.err
}

func TestFlash_StoreError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("boom")
	s := New()
	s.Store = flashErrorStore{MemStore: memstore.NewWithCleanupInterval(0), err: expectedErr}

	token, ctx := seedSession(t, s, map[string]interface{}{
		"notice": "welcome back",
	})

	val, found, err := s.Flash(ctx, "notice")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
	if found {
		t.Error("expected found to be false on store error")
	}
	if val != nil {
		t.Errorf("expected nil value on store error, got %v", val)
	}

	// The local session data must remain untouched when the store fails.
	if s.GetString(ctx, "notice") != "welcome back" {
		t.Error("expected local session data to be unchanged after store error")
	}

	values := decodeStore(t, s, token)
	if values["notice"] != "welcome back" {
		t.Error("expected stored data to be unchanged after store error")
	}
}

func TestFlash_UnsupportedStore(t *testing.T) {
	t.Parallel()

	s := New()
	s.Store = &mockstore.MockStore{}

	sd := newSessionData(time.Hour, 0)
	ctx := s.addSessionDataToContext(context.Background(), sd)
	s.Put(ctx, "notice", "welcome back")

	val, found, err := s.Flash(ctx, "notice")
	if !errors.Is(err, ErrFlashNotSupported) {
		t.Fatalf("expected ErrFlashNotSupported, got %v", err)
	}
	if found {
		t.Error("expected found to be false when unsupported")
	}
	if val != nil {
		t.Errorf("expected nil value when unsupported, got %v", val)
	}
	if s.GetString(ctx, "notice") != "welcome back" {
		t.Error("expected local session data to be unchanged when unsupported")
	}
}

type flashResult struct {
	val   interface{}
	found bool
	err   error
}

// TestFlash_ConcurrentContention is the acceptance test: two parallel requests
// race to consume the same flash value. Exactly one must receive it, and the
// value must be gone from the store.
func TestFlash_ConcurrentContention(t *testing.T) {
	t.Parallel()

	s := New()
	token, _ := seedSession(t, s, map[string]interface{}{
		"notice": "welcome back",
	})

	const n = 2
	ctxs := make([]context.Context, n)
	for i := range ctxs {
		ctx, err := s.Load(context.Background(), token)
		if err != nil {
			t.Fatalf("error loading session for request %d: %v", i, err)
		}
		ctxs[i] = ctx
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]flashResult, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			val, found, err := s.Flash(ctxs[i], "notice")
			results[i] = flashResult{val: val, found: found, err: err}
		}(i)
	}

	close(start)
	wg.Wait()

	var winners, losers int
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d returned unexpected error: %v", i, r.err)
		}
		switch r.found {
		case true:
			winners++
			if r.val.(string) != "welcome back" {
				t.Fatalf("winner got %q", r.val)
			}
		case false:
			losers++
			if r.val != nil {
				t.Fatalf("loser expected nil value, got %v", r.val)
			}
		}
	}

	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}
	if losers != n-1 {
		t.Errorf("expected %d losers, got %d", n-1, losers)
	}

	values := decodeStore(t, s, token)
	if _, exists := values["notice"]; exists {
		t.Error("expected flashed key to be absent from store after contention")
	}
}

// TestFlash_LateStaleCommitCannotResurrect is the acceptance test for the
// second requirement. A request that loaded the session before the flash was
// consumed later makes a plain commit: the consumed flash must not be written
// back.
func TestFlash_LateStaleCommitCannotResurrect(t *testing.T) {
	t.Parallel()

	s := New()
	token, _ := seedSession(t, s, map[string]interface{}{
		"notice": "welcome back",
		"other":  "original",
	})

	// Both requests load their own copy before anyone flashes.
	winnerCtx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("error loading winner session: %v", err)
	}
	lateCtx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("error loading late session: %v", err)
	}

	// The winner atomically consumes the flash.
	if val, found, err := s.Flash(winnerCtx, "notice"); err != nil || !found {
		t.Fatalf("expected winner to consume flash, got %v, %v, %v", val, found, err)
	}
	if _, _, err := s.Commit(winnerCtx); err != nil {
		t.Fatalf("error committing winner: %v", err)
	}

	// Meanwhile the late request makes an unrelated change and performs a
	// plain (non-flash) commit. Its stale copy still contains "notice".
	if got := s.GetString(lateCtx, "notice"); got != "welcome back" {
		t.Fatalf("expected late request's stale local copy to still contain the flash, got %q", got)
	}
	s.Put(lateCtx, "other", "updated")
	if _, _, err := s.Commit(lateCtx); err != nil {
		t.Fatalf("error committing late request: %v", err)
	}

	values := decodeStore(t, s, token)
	if _, exists := values["notice"]; exists {
		t.Errorf("late stale commit resurrected the flashed value: %#v", values)
	}
	if values["other"] != "updated" {
		t.Errorf("expected the late request's change to be persisted, got %v", values["other"])
	}

	// And a fresh load must not see the flash either.
	freshCtx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("error loading fresh session: %v", err)
	}
	if s.Exists(freshCtx, "notice") {
		t.Error("expected a fresh load to never see the consumed flash")
	}
}

// TestMergeCommit_ConcurrentChangesPreserved verifies that commits from
// independently loaded sessions overlay only their own modifications instead
// of overwriting each other.
func TestMergeCommit_ConcurrentChangesPreserved(t *testing.T) {
	t.Parallel()

	s := New()
	token, _ := seedSession(t, s, map[string]interface{}{
		"a": "orig",
		"b": "orig",
	})

	reqA, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("load reqA: %v", err)
	}
	reqB, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("load reqB: %v", err)
	}

	s.Put(reqA, "a", "from-a")
	if _, _, err := s.Commit(reqA); err != nil {
		t.Fatalf("commit reqA: %v", err)
	}

	// reqB's copy is now stale; its commit must not undo reqA's write.
	s.Put(reqB, "b", "from-b")
	if _, _, err := s.Commit(reqB); err != nil {
		t.Fatalf("commit reqB: %v", err)
	}

	values := decodeStore(t, s, token)
	if values["a"] != "from-a" || values["b"] != "from-b" {
		t.Fatalf("expected both changes persisted, got %#v", values)
	}
}

// TestMergeCommit_StaleRemoveDoesNotResurrect verifies that a key removed by
// one request is not brought back by another request's stale commit.
func TestMergeCommit_StaleRemoveDoesNotResurrect(t *testing.T) {
	t.Parallel()

	s := New()
	token, _ := seedSession(t, s, map[string]interface{}{
		"k": "v",
		"x": "y",
	})

	remover, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("load remover: %v", err)
	}
	other, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("load other: %v", err)
	}

	s.Remove(remover, "k")
	if _, _, err := s.Commit(remover); err != nil {
		t.Fatalf("commit remover: %v", err)
	}

	s.Put(other, "x", "z")
	if _, _, err := s.Commit(other); err != nil {
		t.Fatalf("commit other: %v", err)
	}

	values := decodeStore(t, s, token)
	if _, exists := values["k"]; exists {
		t.Fatalf("stale commit resurrected removed key: %#v", values)
	}
	if values["x"] != "z" {
		t.Fatalf("expected unrelated change persisted, got %#v", values)
	}
}
