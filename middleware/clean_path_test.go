package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestCleanPath(t *testing.T) {
	r := chi.NewRouter()
	r.Use(CleanPath)
	r.Get("/users/1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/users////1", nil)
	r.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("clean path: unexpected response code: %v", w.Result().StatusCode)
	}
}

func TestCleanPathEscapedStrategy(t *testing.T) {
	r := chi.NewRouter()
	r.Use(CleanPath)
	r.SetPathStrategy(chi.PathStrategyEscaped)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chi.URLParam(r, "key")))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	// Cleaning collapses the duplicate literal slash but keeps the %2F
	// inside the {key} segment.
	if _, resp := testRequest(t, ts, "GET", "/objects//a%2Fb", nil); resp != "a/b" {
		t.Fatal(resp)
	}
}
