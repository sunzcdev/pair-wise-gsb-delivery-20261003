package chi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPathStrategyComparison sends real HTTP requests with escaped
// characters in the path to two routers, one per path strategy, and
// compares the parameter values each one exposes.
func TestPathStrategyComparison(t *testing.T) {
	testCases := []struct {
		name string
		path string
		// PathStrategyEscaped (default)
		wantParamEscaped string
		wantRawEscaped   string
		// PathStrategyUnescaped
		wantParamUnescaped string
		wantRawUnescaped   string
	}{
		{
			name:               "escaped slash",
			path:               "/objects/a%2Fb",
			wantParamEscaped:   "a%2Fb",
			wantRawEscaped:     "a%2Fb",
			wantParamUnescaped: "a/b",
			wantRawUnescaped:   "a%2Fb",
		},
		{
			name:               "double-escaped slash",
			path:               "/objects/a%252Fb",
			wantParamEscaped:   "a%2Fb",
			wantRawEscaped:     "a%2Fb",
			wantParamUnescaped: "a%2Fb",
			wantRawUnescaped:   "a%252Fb",
		},
		{
			name:               "escaped unicode",
			path:               "/objects/%E4%B8%AD%E6%96%87",
			wantParamEscaped:   "中文",
			wantRawEscaped:     "中文",
			wantParamUnescaped: "中文",
			wantRawUnescaped:   "%E4%B8%AD%E6%96%87",
		},
		{
			name:               "escaped unreserved char",
			path:               "/objects/%41",
			wantParamEscaped:   "%41",
			wantRawEscaped:     "%41",
			wantParamUnescaped: "A",
			wantRawUnescaped:   "%41",
		},
		{
			name:               "plus is not a space",
			path:               "/objects/a+b",
			wantParamEscaped:   "a+b",
			wantRawEscaped:     "a+b",
			wantParamUnescaped: "a+b",
			wantRawUnescaped:   "a+b",
		},
	}

	newMux := func(strategy PathStrategy) *Mux {
		r := NewRouter()
		r.SetPathStrategy(strategy)
		r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "param=%q raw=%q pathvalue=%q",
				URLParam(r, "key"), URLParamRaw(r, "key"), r.PathValue("key"))
		})
		return r
	}

	servers := map[PathStrategy]*httptest.Server{}
	for _, strategy := range []PathStrategy{PathStrategyEscaped, PathStrategyUnescaped} {
		ts := httptest.NewServer(newMux(strategy))
		defer ts.Close()
		servers[strategy] = ts
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, body := testRequest(t, servers[PathStrategyEscaped], "GET", tc.path, nil)
			want := fmt.Sprintf("param=%q raw=%q pathvalue=%q",
				tc.wantParamEscaped, tc.wantRawEscaped, tc.wantParamEscaped)
			if body != want {
				t.Errorf("PathStrategyEscaped: expecting %s, got %s", want, body)
			}

			_, body = testRequest(t, servers[PathStrategyUnescaped], "GET", tc.path, nil)
			want = fmt.Sprintf("param=%q raw=%q pathvalue=%q",
				tc.wantParamUnescaped, tc.wantRawUnescaped, tc.wantParamUnescaped)
			if body != want {
				t.Errorf("PathStrategyUnescaped: expecting %s, got %s", want, body)
			}
		})
	}
}

// TestPathStrategyUnescapedInvalidRawPath verifies that an invalid
// RawPath hint is ignored under PathStrategyUnescaped, falling back to
// the escaped form of URL.Path. A real server never delivers such
// requests (it rejects them with a 400 before routing), so the requests
// are crafted by hand and passed through Mux.ServeHTTP.
func TestPathStrategyUnescapedInvalidRawPath(t *testing.T) {
	newMux := func(strategy PathStrategy) *Mux {
		r := NewRouter()
		r.SetPathStrategy(strategy)
		r.Get("/objects/{a}/{b}", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "a=%q b=%q", URLParam(r, "a"), URLParam(r, "b"))
		})
		return r
	}

	testCases := []struct {
		name    string
		rawPath string
	}{
		{"invalid escape sequence", "/objects/a%2Gb"},
		{"inconsistent with Path", "/objects/x/y"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			newReq := func() *http.Request {
				r := httptest.NewRequest("GET", "/objects/a/b", nil)
				r.URL.RawPath = tc.rawPath
				return r
			}

			// PathStrategyUnescaped ignores the invalid hint and routes
			// on the escaped form of URL.Path.
			w := httptest.NewRecorder()
			newMux(PathStrategyUnescaped).ServeHTTP(w, newReq())
			if w.Code != 200 {
				t.Fatalf("PathStrategyUnescaped: expecting 200, got %d", w.Code)
			}
			if body := w.Body.String(); body != `a="a" b="b"` {
				t.Fatalf("PathStrategyUnescaped: unexpected body %q", body)
			}

			// The default strategy keeps its historical behavior and
			// routes on RawPath verbatim.
			w = httptest.NewRecorder()
			newMux(PathStrategyEscaped).ServeHTTP(w, newReq())
			if tc.rawPath == "/objects/x/y" {
				if body := w.Body.String(); body != `a="x" b="y"` {
					t.Fatalf("PathStrategyEscaped: unexpected body %q", body)
				}
			} else if w.Code != 404 {
				t.Fatalf("PathStrategyEscaped: expecting 404, got %d", w.Code)
			}
		})
	}
}

// TestPathStrategyUnescapedStaticSegments verifies that an escaped
// slash does not match a static multi-segment route, and that static
// matching keeps working for plain requests.
func TestPathStrategyUnescapedStaticSegments(t *testing.T) {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyUnescaped)
	r.Get("/objects/a/b", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("static"))
	})
	r.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "param=%q", URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	// The escaped slash stays inside a single segment, so the request
	// matches the {key} route, not the static "/objects/a/b" route.
	_, body := testRequest(t, ts, "GET", "/objects/a%2Fb", nil)
	if body != `param="a/b"` {
		t.Fatalf("expecting param route, got %q", body)
	}

	// A real slash still matches the static route.
	_, body = testRequest(t, ts, "GET", "/objects/a/b", nil)
	if body != "static" {
		t.Fatalf("expecting static route, got %q", body)
	}
}

// TestPathStrategyUnescapedRegexpParams verifies that regexp parameters
// match against the escaped segment text, while the captured value is
// decoded before being exposed.
func TestPathStrategyUnescapedRegexpParams(t *testing.T) {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyUnescaped)
	r.Get("/num/{n:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "n=%q", URLParam(r, "n"))
	})
	r.Get("/re/{key:[A-Za-z0-9%]+}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", URLParam(r, "key"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/num/123", nil)
	if body != `n="123"` {
		t.Fatalf("expecting n=\"123\", got %q", body)
	}

	// The regexp matches the escaped segment text "%31%32" ("12"), which
	// is not made of digits, so the route does not match.
	resp, _ := testRequest(t, ts, "GET", "/num/%31%32", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expecting 404, got %d", resp.StatusCode)
	}

	// A regexp that allows the escaped text matches, and the captured
	// value is decoded once.
	_, body = testRequest(t, ts, "GET", "/re/a%2Fb", nil)
	if body != `key="a/b"` {
		t.Fatalf("expecting key=\"a/b\", got %q", body)
	}
}

// TestPathStrategyUnescapedWildcard verifies that wildcard parameters
// follow the same decoding rule.
func TestPathStrategyUnescapedWildcard(t *testing.T) {
	r := NewRouter()
	r.SetPathStrategy(PathStrategyUnescaped)
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "param=%q raw=%q", URLParam(r, "*"), URLParamRaw(r, "*"))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/files/a%2Fb/c", nil)
	if body != `param="a/b/c" raw="a%2Fb/c"` {
		t.Fatalf("unexpected body %q", body)
	}
}

// TestPathStrategyUnescapedMount verifies that mounted sub-routers
// follow the root mux's path strategy, and that the remainder of the
// path handed to the sub-router keeps its raw segment boundaries.
func TestPathStrategyUnescapedMount(t *testing.T) {
	sub := NewRouter()
	sub.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "key=%q", URLParam(r, "key"))
	})

	r := NewRouter()
	r.SetPathStrategy(PathStrategyUnescaped)
	r.Mount("/api", sub)

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/api/objects/a%2Fb", nil)
	if body != `key="a/b"` {
		t.Fatalf("unexpected body %q", body)
	}
}

// TestPathStrategyUnescapedMountWithParams verifies that parameters
// captured before a Mount boundary are decoded, while the remainder of
// the path routed into the sub-router is not re-split.
func TestPathStrategyUnescapedMountWithParams(t *testing.T) {
	sub := NewRouter()
	sub.Get("/objects/{key}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tid=%q key=%q pattern=%q",
			URLParam(r, "tid"), URLParam(r, "key"), RouteContext(r.Context()).RoutePattern())
	})

	r := NewRouter()
	r.SetPathStrategy(PathStrategyUnescaped)
	r.Mount("/tenants/{tid}", sub)

	ts := httptest.NewServer(r)
	defer ts.Close()

	_, body := testRequest(t, ts, "GET", "/tenants/t%2F1/objects/k%2F2", nil)
	want := `tid="t/1" key="k/2" pattern="/tenants/{tid}/objects/{key}"`
	if body != want {
		t.Fatalf("expecting %s, got %s", want, body)
	}
}
