package scs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
)

type failCommitStore struct {
	Store
}

func (f failCommitStore) Commit(token string, b []byte, expiry time.Time) error {
	return errors.New("commit failed")
}

func TestBufferedWriteThenPut(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 1024

	mux := http.NewServeMux()
	mux.HandleFunc("/put", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body first"))
		sessionManager.Put(r.Context(), "foo", "bar")
	}))
	mux.HandleFunc("/get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sessionManager.GetString(r.Context(), "foo")))
	}))

	ts := newTestServer(t, sessionManager.LoadAndSave(mux))
	defer ts.Close()

	header, body := ts.execute(t, "/put")
	if body != "body first" {
		t.Errorf("want %q; got %q", "body first", body)
	}
	if header.Get("Set-Cookie") == "" {
		t.Error("want a session cookie; got none")
	}

	_, body = ts.execute(t, "/get")
	if body != "bar" {
		t.Errorf("want %q; got %q", "bar", body)
	}
}

func TestBufferedCommitFailure(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 1024
	sessionManager.Store = failCommitStore{memstore.New()}

	var errCaught error
	sessionManager.ErrorFunc = func(w http.ResponseWriter, r *http.Request, err error) {
		errCaught = err
	}

	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("body first"))
		sessionManager.Put(r.Context(), "foo", "bar")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if errCaught == nil || errCaught.Error() != "commit failed" {
		t.Fatalf("want commit failed error; got %v", errCaught)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("want no status code written to the underlying ResponseWriter; got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("want no body written to the underlying ResponseWriter; got %q", rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("want no session cookie; got %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestBufferedLimit(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 5

	var writeErr error
	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, writeErr = w.Write([]byte("hello world"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(writeErr, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge; got %v", writeErr)
	}
}

func TestBufferedFlushAndHijack(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 1024

	var hijackErr error
	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body"))

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("want buffered writer to implement http.Flusher")
			return
		}
		flusher.Flush()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("want buffered writer to implement http.Hijacker")
			return
		}
		_, _, hijackErr = hijacker.Hijack()
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if hijackErr == nil {
		t.Error("want hijack to return an error; got nil")
	}
	if rec.Body.String() != "body" {
		t.Errorf("want %q; got %q", "body", rec.Body.String())
	}
}

func TestBufferedPanic(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 1024

	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body"))
		sessionManager.Put(r.Context(), "foo", "bar")
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("want panic to propagate; got none")
			}
		}()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	if rec.Body.Len() != 0 {
		t.Errorf("want no body written to the underlying ResponseWriter; got %q", rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("want no session cookie; got %q", rec.Header().Get("Set-Cookie"))
	}
}
