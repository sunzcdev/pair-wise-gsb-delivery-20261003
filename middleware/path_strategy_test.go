package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestCleanPathUnescaped verifies that CleanPath operates on the escaped
// form of the path under chi.PathStrategyUnescaped, so an escaped slash
// (%2F) is not treated as a segment boundary while cleaning.
func TestCleanPathUnescaped(t *testing.T) {
	r := chi.NewRouter()
	r.SetPathStrategy(chi.PathStrategyUnescaped)
	r.Use(CleanPath)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", chi.URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/objects//a%2Fb", nil)
	if body != `key="a/b"` {
		t.Fatalf("expecting key=\"a/b\", got %q", body)
	}
}

// TestStripSlashesUnescaped verifies that StripSlashes strips the
// trailing slash from the escaped form of the path under
// chi.PathStrategyUnescaped.
func TestStripSlashesUnescaped(t *testing.T) {
	r := chi.NewRouter()
	r.SetPathStrategy(chi.PathStrategyUnescaped)
	r.Use(StripSlashes)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", chi.URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/objects/a%2Fb/", nil)
	if body != `key="a/b"` {
		t.Fatalf("expecting key=\"a/b\", got %q", body)
	}
}

// TestRedirectSlashesUnescaped verifies that RedirectSlashes keeps the
// original escaping of the path in the redirect target under
// chi.PathStrategyUnescaped, instead of redirecting to the decoded path
// where an escaped slash would become a real segment boundary.
func TestRedirectSlashesUnescaped(t *testing.T) {
	r := chi.NewRouter()
	r.SetPathStrategy(chi.PathStrategyUnescaped)
	r.Use(RedirectSlashes)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", chi.URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, _ := testRequestNoRedirect(t, ts, "GET", "/objects/a%2Fb/", nil)
	if resp.StatusCode != 301 {
		t.Fatalf("expecting 301, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/objects/a%2Fb" {
		t.Fatalf("expecting redirect to /objects/a%%2Fb, got %q", loc)
	}

	// Following the redirect routes to the handler with the decoded param.
	_, body := testRequest(t, ts, "GET", "/objects/a%2Fb/", nil)
	if body != `key="a/b"` {
		t.Fatalf("expecting key=\"a/b\", got %q", body)
	}
}

// TestURLFormatUnescaped verifies that URLFormat trims the format suffix
// from the escaped form of the path under chi.PathStrategyUnescaped.
func TestURLFormatUnescaped(t *testing.T) {
	r := chi.NewRouter()
	r.SetPathStrategy(chi.PathStrategyUnescaped)
	r.Use(URLFormat)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		format, _ := r.Context().Value(URLFormatCtxKey).(string)
		fmt.Fprintf(w, "key=%q format=%q", chi.URLParam(r, "key"), format)
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/objects/a%2Fb.json", nil)
	if body != `key="a/b" format="json"` {
		t.Fatalf("unexpected body %q", body)
	}
}

// TestGetHeadUnescaped verifies that GetHead routes HEAD requests to GET
// handlers using the escaped form of the path under
// chi.PathStrategyUnescaped.
func TestGetHeadUnescaped(t *testing.T) {
	r := chi.NewRouter()
	r.SetPathStrategy(chi.PathStrategyUnescaped)
	r.Use(GetHead)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Key", chi.URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, _ := testRequest(t, ts, "HEAD", "/objects/a%2Fb", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expecting 200, got %d", resp.StatusCode)
	}
	if key := resp.Header.Get("X-Key"); key != "a/b" {
		t.Fatalf("expecting X-Key \"a/b\", got %q", key)
	}
}
