package scs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2/memstore"
)

// failCommitStore delegates all operations to the embedded store, except
// Commit which always fails.
type failCommitStore struct {
	Store
}

func (f *failCommitStore) Commit(token string, b []byte, expiry time.Time) error {
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
		t.Error("want Set-Cookie header; got none")
	}

	_, body = ts.execute(t, "/get")
	if body != "bar" {
		t.Errorf("want %q; got %q", "bar", body)
	}
}

func TestBufferedCommitError(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 1024
	sessionManager.Store = &failCommitStore{Store: memstore.New()}

	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("original body"))
		sessionManager.Put(r.Context(), "foo", "bar")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want status %d; got %d", http.StatusInternalServerError, rec.Code)
	}
	if strings.Contains(rec.Body.String(), "original body") {
		t.Errorf("want buffered body to be discarded; got %q", rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("want no Set-Cookie header; got %q", rec.Header().Get("Set-Cookie"))
	}
	if rec.Header().Get("Content-Type") == "text/plain" {
		t.Errorf("want buffered headers to be discarded; got Content-Type %q", rec.Header().Get("Content-Type"))
	}
}

func TestBufferedLimitExceeded(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 4

	mux := http.NewServeMux()
	mux.HandleFunc("/put", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionManager.Put(r.Context(), "foo", "bar")
		w.Write([]byte("hello world"))
	}))
	mux.HandleFunc("/get", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sessionManager.GetString(r.Context(), "foo")))
	}))

	ts := newTestServer(t, sessionManager.LoadAndSave(mux))
	defer ts.Close()

	// The body exceeds the 4 byte buffer limit, so the session is committed
	// immediately and the response is streamed to the client.
	header, body := ts.execute(t, "/put")
	if body != "hello world" {
		t.Errorf("want %q; got %q", "hello world", body)
	}
	if header.Get("Set-Cookie") == "" {
		t.Error("want Set-Cookie header; got none")
	}

	_, body = ts.execute(t, "/get")
	if body != "bar" {
		t.Errorf("want %q; got %q", "bar", body)
	}
}

func TestBufferedFlushThenPutPanics(t *testing.T) {
	t.Parallel()

	sessionManager := New()
	sessionManager.BufferSize = 1024

	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
		w.(http.Flusher).Flush()
		sessionManager.Put(r.Context(), "foo", "bar")
	}))

	defer func() {
		if recover() != ErrUncommittedModification {
			t.Errorf("want panic with ErrUncommittedModification")
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestUnbufferedWriteThenPutPanics(t *testing.T) {
	t.Parallel()

	sessionManager := New()

	handler := sessionManager.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body first"))
		sessionManager.Put(r.Context(), "foo", "bar")
	}))

	defer func() {
		if recover() != ErrUncommittedModification {
			t.Errorf("want panic with ErrUncommittedModification")
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
