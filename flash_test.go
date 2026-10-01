package scs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
)

// newSessionWithFlash commits a session containing a flash value and returns
// the manager and token.
func newSessionWithFlash(t *testing.T, key, val string) (*SessionManager, string) {
	t.Helper()

	s := New()
	s.Store = memstore.NewWithCleanupInterval(0)

	origin, err := s.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	s.Put(origin, key, val)
	s.Put(origin, "other", "kept")
	token, _, err := s.Commit(origin)
	if err != nil {
		t.Fatal(err)
	}

	return s, token
}

func TestFlash_ConcurrentOnlyOneWins(t *testing.T) {
	t.Parallel()

	s, token := newSessionWithFlash(t, "notice", "welcome")

	const n = 32

	// Every request loads its own independent copy of the session, as would
	// happen when parallel requests arrive after the value was already stored.
	ctxs := make([]context.Context, n)
	for i := range ctxs {
		ctx, err := s.Load(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		ctxs[i] = ctx
	}

	type result struct {
		val   interface{}
		found bool
		err   error
	}

	results := make([]result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			val, found, err := s.Flash(ctxs[i], "notice")
			results[i] = result{val: val, found: found, err: err}
		}(i)
	}

	close(start)
	wg.Wait()

	winners := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d got unexpected error: %v", i, r.err)
		}
		if r.found {
			winners++
			if r.val != "welcome" {
				t.Fatalf("request %d got %q, expected %q", i, r.val, "welcome")
			}
		}
	}

	if winners != 1 {
		t.Fatalf("expected exactly 1 request to receive the flash, got %d", winners)
	}

	// A late, ordinary commit from one of the requests that loaded the session
	// before the flash must not resurrect the value.
	late := ctxs[0]
	if _, _, err := s.Commit(late); err != nil {
		t.Fatal(err)
	}

	// Reload fresh from the store: the flash is gone, the rest survives.
	fresh, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exists(fresh, "notice") {
		t.Fatal("flash value was written back to the store by the late commit")
	}
	if got := s.GetString(fresh, "other"); got != "kept" {
		t.Fatalf("expected unrelated value %q to survive, got %q", "kept", got)
	}

	// A subsequent flash finds nothing.
	val, found, err := s.Flash(fresh, "notice")
	if err != nil {
		t.Fatal(err)
	}
	if found || val != nil {
		t.Fatalf("expected no value on second flash, got %v (%v)", val, found)
	}
}

func TestFlash_LateCommitAfterDelay(t *testing.T) {
	t.Parallel()

	s, token := newSessionWithFlash(t, "notice", "welcome")

	stale, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}

	winner, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}

	val, found, err := s.Flash(winner, "notice")
	if err != nil {
		t.Fatal(err)
	}
	if !found || val != "welcome" {
		t.Fatalf("expected winner to get %q, got %v (%v)", "welcome", val, found)
	}

	// The stale request is unaware of the flash and still holds the value in
	// its local copy; committing it late must be stripped.
	time.Sleep(10 * time.Millisecond)
	if _, _, err := s.Commit(stale); err != nil {
		t.Fatal(err)
	}

	b, ok, err := s.doStoreFind(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("session missing from store")
	}

	_, values, err := s.Codec.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["notice"]; ok {
		t.Fatal("flash value resurrected by the late commit")
	}
}

func TestFlash_NoValue(t *testing.T) {
	t.Parallel()

	s, token := newSessionWithFlash(t, "notice", "welcome")

	ctx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}

	val, found, err := s.Flash(ctx, "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found to be false for a missing key")
	}
	if val != nil {
		t.Fatalf("expected nil value, got %v", val)
	}
}

func TestFlash_UncommittedSession(t *testing.T) {
	t.Parallel()

	s := New()
	ctx, err := s.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	s.Put(ctx, "notice", "welcome")

	val, found, err := s.Flash(ctx, "notice")
	if err != nil {
		t.Fatal(err)
	}
	if !found || val != "welcome" {
		t.Fatalf("got %v (%v), expected %q", val, found, "welcome")
	}
	if s.Exists(ctx, "notice") {
		t.Fatal("expected value to be removed locally")
	}
}

var flashBoom = errors.New("flash boom")

func TestFlash_StoreFailure(t *testing.T) {
	t.Parallel()

	s := New()
	store := &failingFlashStore{}
	s.Store = store

	token := "persisted-token"
	expiry := time.Now().Add(time.Hour)
	b, err := s.Codec.Encode(expiry, map[string]interface{}{"notice": "welcome"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Commit(token, b, expiry); err != nil {
		t.Fatal(err)
	}

	ctx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}

	val, found, err := s.Flash(ctx, "notice")
	if !errors.Is(err, flashBoom) {
		t.Fatalf("expected flashBoom, got %v", err)
	}
	if found {
		t.Fatal("expected found false on store failure")
	}
	if val != nil {
		t.Fatalf("expected nil value on failure, got %v", val)
	}

	// Nothing should have been consumed locally, so the value remains visible.
	if got := s.GetString(ctx, "notice"); got != "welcome" {
		t.Fatalf("expected local value untouched, got %q", got)
	}

	// A retry against a healthy store succeeds.
	healthy := memstore.NewWithCleanupInterval(0)
	s.Store = healthy
	if err := s.Store.Commit(token, b, expiry); err != nil {
		t.Fatal(err)
	}
	ctx2, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	val, found, err = s.Flash(ctx2, "notice")
	if err != nil || !found || val != "welcome" {
		t.Fatalf("retry got %v (%v, %v)", val, found, err)
	}
}

func TestFlash_UnsupportedStore(t *testing.T) {
	t.Parallel()

	s := New()
	s.Store = &nonFlashStore{}

	// Persist a session so that Flash must consult the store (rather than
	// short-circuiting on an uncommitted, local-only session).
	token := "persisted-token"
	expiry := time.Now().Add(time.Hour)
	b, err := s.Codec.Encode(expiry, map[string]interface{}{"notice": "welcome"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Commit(token, b, expiry); err != nil {
		t.Fatal(err)
	}

	ctx, err := s.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}

	val, found, err := s.Flash(ctx, "notice")
	if !errors.Is(err, ErrFlashNotSupported) {
		t.Fatalf("expected ErrFlashNotSupported, got %v", err)
	}
	if found {
		t.Fatal("expected found false when unsupported")
	}
	if val != nil {
		t.Fatalf("expected nil value when unsupported, got %v", val)
	}
}

// nonFlashStore is a Store which deliberately does NOT implement FlashStore.
type nonFlashStore struct {
	data map[string][]byte
}

func (n *nonFlashStore) Delete(string) error { return nil }
func (n *nonFlashStore) Find(token string) ([]byte, bool, error) {
	b, ok := n.data[token]
	return b, ok, nil
}
func (n *nonFlashStore) Commit(token string, b []byte, _ time.Time) error {
	if n.data == nil {
		n.data = make(map[string][]byte)
	}
	n.data[token] = b
	return nil
}

// failingFlashStore implements FlashCtxStore but always reports a storage
// failure, without modifying any data.
type failingFlashStore struct {
	b []byte
}

func (f *failingFlashStore) Delete(string) error { return nil }
func (f *failingFlashStore) Find(string) ([]byte, bool, error) {
	return f.b, f.b != nil, nil
}
func (f *failingFlashStore) Commit(_ string, b []byte, _ time.Time) error {
	f.b = b
	return nil
}
func (f *failingFlashStore) Flash(string, func([]byte, bool) ([]byte, error)) error {
	return flashBoom
}
func (f *failingFlashStore) FlashCtx(context.Context, string, func([]byte, bool) ([]byte, error)) error {
	return flashBoom
}
