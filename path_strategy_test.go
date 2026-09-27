package chi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func escapedPathRouter() *Mux {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyEscaped)
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q raw=%q pv=%q",
			URLParam(r, "key"), URLParamRaw(r, "key"), r.PathValue("key"))
	})
	return r
}

func getBody(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestEscapedPathStrategyParams(t *testing.T) {
	ts := httptest.NewServer(escapedPathRouter())
	defer ts.Close()

	tests := []struct {
		path string
		want string
	}{
		// %2F stays inside the {key} segment and is decoded once
		{"/objects/a%2Fb", `key="a/b" raw="a%2Fb" pv="a/b"`},
		// lowercase escape works the same
		{"/objects/a%2fb", `key="a/b" raw="a%2fb" pv="a/b"`},
		// double-encoded slash is decoded exactly once
		{"/objects/a%252Fb", `key="a%2Fb" raw="a%252Fb" pv="a%2Fb"`},
		// escaped unicode
		{"/objects/%E4%B8%AD%E6%96%87", `key="中文" raw="%E4%B8%AD%E6%96%87" pv="中文"`},
		// unreserved characters are unaffected
		{"/objects/plain", `key="plain" raw="plain" pv="plain"`},
	}
	for _, tt := range tests {
		code, body := getBody(t, ts, tt.path)
		if code != http.StatusOK {
			t.Fatalf("GET %s: unexpected status %d", tt.path, code)
		}
		if body != tt.want {
			t.Errorf("GET %s: got %s, want %s", tt.path, body, tt.want)
		}
	}
}

func TestEscapedPathStrategyDefaultUnchanged(t *testing.T) {
	// Without SetPathStrategy, behavior is unchanged: routing happens on
	// RawPath when set, and params are returned as captured (not decoded).
	r := NewRouter()
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q raw=%q", URLParam(r, "key"), URLParamRaw(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	code, body := getBody(t, ts, "/objects/a%2Fb")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d", code)
	}
	if want := `key="a%2Fb" raw="a%2Fb"`; body != want {
		t.Errorf("got %s, want %s", body, want)
	}
}

func TestEscapedPathStrategyInvalidRawPath(t *testing.T) {
	r := escapedPathRouter()
	r.Get("/objects/a/b", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("static"))
	})

	// A RawPath that does not unescape to URL.Path is invalid; the router
	// must fall back to the decoded path instead of trusting RawPath.
	req := httptest.NewRequest("GET", "http://example.com/objects/a/b", nil)
	req.URL.RawPath = "/objects/a%252Fb"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if body := w.Body.String(); body != "static" {
		t.Errorf("mismatched RawPath: got %q, want %q", body, "static")
	}

	// A RawPath with a malformed escape sequence is invalid as well.
	req = httptest.NewRequest("GET", "http://example.com/objects/a/b", nil)
	req.URL.RawPath = "/objects/a%zz"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if body := w.Body.String(); body != "static" {
		t.Errorf("malformed RawPath: got %q, want %q", body, "static")
	}

	// A valid RawPath is honored: %2F stays within the {key} segment.
	req = httptest.NewRequest("GET", "http://example.com/objects/a/b", nil)
	req.URL.RawPath = "/objects/a%2Fb"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if want := `key="a/b" raw="a%2Fb" pv="a/b"`; w.Body.String() != want {
		t.Errorf("valid RawPath: got %q, want %q", w.Body.String(), want)
	}
}

func TestEscapedPathStrategyRegexp(t *testing.T) {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyEscaped)
	r.Get("/files/{name:[a-z/]+}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "name=%q", URLParam(r, "name"))
	})
	r.Get("/nums/{id:\\d+}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "id=%q", URLParam(r, "id"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	// The regexp matches against the decoded segment: a/b matches [a-z/]+.
	if code, body := getBody(t, ts, "/files/a%2Fb"); code != http.StatusOK || body != `name="a/b"` {
		t.Errorf("got %d %s, want 200 name=\"a/b\"", code, body)
	}
	// Decoded "1/2" does not match \d+, and the %2F must not be split into
	// two segments either, so the request 404s.
	if code, _ := getBody(t, ts, "/nums/1%2F2"); code != http.StatusNotFound {
		t.Errorf("got %d, want 404", code)
	}
	// %32 decodes to "2", so the decoded segment "12" matches \d+.
	if code, body := getBody(t, ts, "/nums/1%32"); code != http.StatusOK || body != `id="12"` {
		t.Errorf("got %d %s, want 200 id=\"12\"", code, body)
	}
}

func TestEscapedPathStrategyWildcard(t *testing.T) {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyEscaped)
	r.Get("/dl/*", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "star=%q raw=%q", URLParam(r, "*"), URLParamRaw(r, "*"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	code, body := getBody(t, ts, "/dl/a%2Fb/c%20d")
	if code != http.StatusOK {
		t.Fatalf("unexpected status %d", code)
	}
	if want := `star="a/b/c d" raw="a%2Fb/c%20d"`; body != want {
		t.Errorf("got %s, want %s", body, want)
	}
}

func TestEscapedPathStrategyStaticAndTrailingSlash(t *testing.T) {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyEscaped)
	r.Get("/objects/list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("static"))
	})
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", URLParam(r, "key"))
	})
	r.Get("/objs/{key}/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	// Static nodes still match byte-wise on the escaped path.
	if code, body := getBody(t, ts, "/objects/list"); code != http.StatusOK || body != "static" {
		t.Errorf("got %d %s, want 200 static", code, body)
	}
	if code, body := getBody(t, ts, "/objects/a%2Fb"); code != http.StatusOK || body != `key="a/b"` {
		t.Errorf("got %d %s, want 200 key=\"a/b\"", code, body)
	}
	// A trailing-slash route keeps the %2F inside the param segment.
	if code, body := getBody(t, ts, "/objs/a%2Fb/"); code != http.StatusOK || body != `key="a/b"` {
		t.Errorf("got %d %s, want 200 key=\"a/b\"", code, body)
	}
}

func TestEscapedPathStrategyMount(t *testing.T) {
	r := NewRouter()

	sub := NewRouter()
	sub.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", URLParam(r, "key"))
	})

	nested := NewRouter()
	nested.Get("/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "nested=%q", URLParam(r, "key"))
	})
	sub.Mount("/v2", nested)

	r.Mount("/api", sub)

	// Setting the strategy after Mount must still propagate to the
	// already-mounted sub-routers.
	r.SetPathStrategy(PathStrategyEscaped)

	ts := httptest.NewServer(r)
	defer ts.Close()

	// The escaped path is forwarded across the Mount boundary without
	// splitting the %2F segment.
	if code, body := getBody(t, ts, "/api/objects/a%2Fb"); code != http.StatusOK || body != `key="a/b"` {
		t.Errorf("got %d %s, want 200 key=\"a/b\"", code, body)
	}
	// Nested mounts preserve the boundary as well.
	if code, body := getBody(t, ts, "/api/v2/x%2Fy"); code != http.StatusOK || body != `nested="x/y"` {
		t.Errorf("got %d %s, want 200 nested=\"x/y\"", code, body)
	}
}
