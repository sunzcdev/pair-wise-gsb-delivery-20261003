package scs

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
)

// controlledStore is an in-memory VersionedStore with test hooks which allow
// the ordering of concurrent commits to be fixed deterministically.
type controlledStore struct {
	inner *memstore.MemStore

	mu                sync.Mutex
	onFindVersioned   func(token string)
	onCommitVersioned func(token string)
}

func newControlledStore() *controlledStore {
	return &controlledStore{inner: memstore.NewWithCleanupInterval(0)}
}

func (s *controlledStore) setOnFindVersioned(hook func(token string)) {
	s.mu.Lock()
	s.onFindVersioned = hook
	s.mu.Unlock()
}

func (s *controlledStore) Find(token string) ([]byte, bool, error) {
	return s.inner.Find(token)
}

func (s *controlledStore) Commit(token string, b []byte, expiry time.Time) error {
	return s.inner.Commit(token, b, expiry)
}

func (s *controlledStore) Delete(token string) error {
	return s.inner.Delete(token)
}

func (s *controlledStore) FindVersioned(token string) ([]byte, uint64, bool, error) {
	s.mu.Lock()
	hook := s.onFindVersioned
	s.mu.Unlock()

	if hook != nil {
		hook(token)
	}

	return s.inner.FindVersioned(token)
}

func (s *controlledStore) CommitVersioned(token string, b []byte, expiry time.Time, expectedVersion uint64) (bool, error) {
	s.mu.Lock()
	hook := s.onCommitVersioned
	s.mu.Unlock()

	if hook != nil {
		hook(token)
	}

	return s.inner.CommitVersioned(token, b, expiry, expectedVersion)
}

func seedSession(t *testing.T, sm *SessionManager, values map[string]interface{}) string {
	t.Helper()

	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	for key, val := range values {
		sm.Put(ctx, key, val)
	}

	token, _, err := sm.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}

	return token
}

func loadSession(t *testing.T, sm *SessionManager, token string) context.Context {
	t.Helper()

	ctx, err := sm.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}

	return ctx
}

func storedValues(t *testing.T, sm *SessionManager, store *controlledStore, token string) map[string]interface{} {
	t.Helper()

	b, found, err := store.Find(token)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("expected token %q to exist in the store", token)
	}

	_, values, err := sm.Codec.Decode(b)
	if err != nil {
		t.Fatal(err)
	}

	return values
}

// Two tabs modify different fields of the same session. The controlled store
// fixes the commit order so that tab B reads-and-commits strictly after tab
// A's commit has completed. Both changes must be preserved.
func TestVersionedCommitMergesDifferentFields(t *testing.T) {
	store := newControlledStore()
	sm := New()
	sm.Store = store

	token := seedSession(t, sm, map[string]interface{}{"prefs": "light", "cart": "empty"})

	ctxA := loadSession(t, sm, token)
	ctxB := loadSession(t, sm, token)

	sm.Put(ctxA, "prefs", "dark")
	sm.Put(ctxB, "cart", "sku-1")

	// Pause tab B's commit inside the store until tab A has committed.
	bPaused := make(chan struct{})
	releaseB := make(chan struct{})
	var armed int32 = 1
	store.setOnFindVersioned(func(token string) {
		// Only the first FindVersioned call (tab B's, since tab A has not
		// started committing yet) is paused.
		if atomic.CompareAndSwapInt32(&armed, 1, 0) {
			close(bPaused)
			<-releaseB
		}
	})

	errBCh := make(chan error, 1)
	go func() {
		_, _, err := sm.Commit(ctxB)
		errBCh <- err
	}()

	<-bPaused

	if _, _, err := sm.Commit(ctxA); err != nil {
		t.Fatalf("tab A commit: %v", err)
	}

	close(releaseB)

	if err := <-errBCh; err != nil {
		t.Fatalf("tab B commit should merge cleanly, got: %v", err)
	}

	values := storedValues(t, sm, store, token)
	if values["prefs"] != "dark" {
		t.Errorf("prefs: expected %q, got %v", "dark", values["prefs"])
	}
	if values["cart"] != "sku-1" {
		t.Errorf("cart: expected %q, got %v", "sku-1", values["cart"])
	}
}

// Two tabs modify the same field with different values. The second committer
// must see a *ConflictError naming the clashing field; the first committer's
// data must remain untouched (no silent last-write-wins).
func TestVersionedCommitConflictingField(t *testing.T) {
	store := newControlledStore()
	sm := New()
	sm.Store = store

	token := seedSession(t, sm, map[string]interface{}{"prefs": "light"})

	ctxA := loadSession(t, sm, token)
	ctxB := loadSession(t, sm, token)

	sm.Put(ctxA, "prefs", "dark")
	sm.Put(ctxB, "prefs", "solarized")

	bPaused := make(chan struct{})
	releaseB := make(chan struct{})
	var armed int32 = 1
	store.setOnFindVersioned(func(token string) {
		if atomic.CompareAndSwapInt32(&armed, 1, 0) {
			close(bPaused)
			<-releaseB
		}
	})

	errBCh := make(chan error, 1)
	go func() {
		_, _, err := sm.Commit(ctxB)
		errBCh <- err
	}()

	<-bPaused

	if _, _, err := sm.Commit(ctxA); err != nil {
		t.Fatalf("tab A commit: %v", err)
	}

	close(releaseB)

	err := <-errBCh
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("tab B commit: expected *ConflictError, got %v", err)
	}
	if !reflect.DeepEqual(conflictErr.Keys, []string{"prefs"}) {
		t.Errorf("conflict keys: expected %v, got %v", []string{"prefs"}, conflictErr.Keys)
	}

	values := storedValues(t, sm, store, token)
	if values["prefs"] != "dark" {
		t.Errorf("prefs: expected tab A value %q to be preserved, got %v", "dark", values["prefs"])
	}
}

// A session is destroyed by one request while an older request (which loaded
// the session before the destroy) is still in flight. The stale request's
// commit must fail and must not resurrect the destroyed session.
func TestVersionedCommitAfterDestroy(t *testing.T) {
	store := newControlledStore()
	sm := New()
	sm.Store = store

	token := seedSession(t, sm, map[string]interface{}{"cart": "sku-1"})

	ctxOld := loadSession(t, sm, token)
	ctxDestroy := loadSession(t, sm, token)

	sm.Put(ctxOld, "cart", "sku-2")

	if err := sm.Destroy(ctxDestroy); err != nil {
		t.Fatal(err)
	}

	_, _, err := sm.Commit(ctxOld)
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("stale commit: expected *ConflictError, got %v", err)
	}
	if !conflictErr.Deleted {
		t.Errorf("expected Deleted to be true, got: %v", err)
	}

	if _, found, _ := store.Find(token); found {
		t.Error("destroyed session was resurrected by the stale commit")
	}
}

// Concurrent requests through the LoadAndSave middleware: a field conflict
// must surface to the application via ErrorFunc as a *ConflictError.
func TestVersionedConflictSurfacedViaMiddleware(t *testing.T) {
	store := newControlledStore()
	sm := New()
	sm.Store = store

	errCh := make(chan error, 1)
	sm.ErrorFunc = func(w http.ResponseWriter, r *http.Request, err error) {
		select {
		case errCh <- err:
		default:
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}

	inHandler := make(chan struct{}, 2)
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/put", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm.Put(r.Context(), "prefs", r.URL.Query().Get("v"))
		if r.URL.Query().Get("block") == "1" {
			inHandler <- struct{}{}
			<-release
		}
	}))

	ts := httptest.NewServer(sm.LoadAndSave(mux))
	defer ts.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	// Establish a session cookie shared by both tabs.
	resp, err := client.Get(ts.URL + "/put?v=seed")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	doReq := func(v string) {
		resp, err := client.Get(ts.URL + "/put?v=" + v + "&block=1")
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}

	go doReq("one")
	<-inHandler
	go doReq("two")
	<-inHandler

	// Both tabs loaded the same baseline and blocked before committing;
	// releasing them means exactly one commit must lose the race and report
	// a conflict.
	close(release)

	select {
	case err := <-errCh:
		var conflictErr *ConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("expected *ConflictError via ErrorFunc, got %v", err)
		}
		if !reflect.DeepEqual(conflictErr.Keys, []string{"prefs"}) {
			t.Errorf("conflict keys: expected %v, got %v", []string{"prefs"}, conflictErr.Keys)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected a conflict error to be surfaced via ErrorFunc")
	}
}
