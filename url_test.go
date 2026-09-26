package chi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestMuxURLBasic(t *testing.T) {
	r := NewRouter()
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("home")
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("user.show")
	r.Get("/date/{yyyy:\\d\\d\\d\\d}/{mm:\\d\\d}", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("archive.byDate")
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("files")

	tests := []struct {
		name   string
		params map[string]string
		path   string
	}{
		{"home", nil, "/"},
		{"user.show", map[string]string{"id": "42"}, "/users/42"},
		{"archive.byDate", map[string]string{"yyyy": "2026", "mm": "09"}, "/date/2026/09"},
		{"files", map[string]string{"*": "a/b/c.txt"}, "/files/a/b/c.txt"},
	}
	for _, tt := range tests {
		u, err := r.URL(tt.name, tt.params, nil)
		if err != nil {
			t.Fatalf("URL(%q): unexpected error: %v", tt.name, err)
		}
		if u.Path != tt.path {
			t.Errorf("URL(%q).Path = %q, want %q", tt.name, u.Path, tt.path)
		}
		if u.RawPath != "" {
			t.Errorf("URL(%q).RawPath = %q, want empty", tt.name, u.RawPath)
		}
		if u.String() != tt.path {
			t.Errorf("URL(%q).String() = %q, want %q", tt.name, u.String(), tt.path)
		}
	}
}

func TestMuxURLEncoding(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("user.show")
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("files")

	tests := []struct {
		name    string
		params  map[string]string
		path    string
		rawPath string
	}{
		// slash in a plain param is encoded, so Path and RawPath diverge
		{"user.show", map[string]string{"id": "a/b"}, "/users/a/b", "/users/a%2Fb"},
		// percent is encoded as %25
		{"user.show", map[string]string{"id": "100%"}, "/users/100%", "/users/100%25"},
		// space is encoded as %20
		{"user.show", map[string]string{"id": "a b"}, "/users/a b", "/users/a%20b"},
		// unicode is percent-encoded as UTF-8
		{"user.show", map[string]string{"id": "中文"}, "/users/中文", "/users/%E4%B8%AD%E6%96%87"},
		// wildcard keeps '/' separators, encodes each segment
		{"files", map[string]string{"*": "a b/中文/x%y"}, "/files/a b/中文/x%y", "/files/a%20b/%E4%B8%AD%E6%96%87/x%25y"},
		// wildcard without special chars needs no RawPath
		{"files", map[string]string{"*": "a/b/c"}, "/files/a/b/c", ""},
	}
	for _, tt := range tests {
		u, err := r.URL(tt.name, tt.params, nil)
		if err != nil {
			t.Fatalf("URL(%q): unexpected error: %v", tt.name, err)
		}
		if u.Path != tt.path {
			t.Errorf("URL(%q) params %v: Path = %q, want %q", tt.name, tt.params, u.Path, tt.path)
		}
		if u.RawPath != tt.rawPath {
			t.Errorf("URL(%q) params %v: RawPath = %q, want %q", tt.name, tt.params, u.RawPath, tt.rawPath)
		}
		// RawPath must decode to Path in exactly one pass
		if u.RawPath != "" {
			decoded, err := url.PathUnescape(u.RawPath)
			if err != nil {
				t.Fatalf("URL(%q): RawPath %q does not decode: %v", tt.name, u.RawPath, err)
			}
			if decoded != u.Path {
				t.Errorf("URL(%q): decoding RawPath once yields %q, want Path %q", tt.name, decoded, u.Path)
			}
			if u.EscapedPath() != u.RawPath {
				t.Errorf("URL(%q): EscapedPath() = %q, want RawPath %q", tt.name, u.EscapedPath(), u.RawPath)
			}
		}
	}
}

func TestMuxURLErrors(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id:\\d+}", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("user.show")
	r.Get("/files/*", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("files")

	if _, err := r.URL("nope", nil, nil); err == nil {
		t.Error("expected error for unknown route name")
	}
	if _, err := r.URL("user.show", nil, nil); err == nil {
		t.Error("expected error for missing param")
	}
	if _, err := r.URL("files", map[string]string{}, nil); err == nil {
		t.Error("expected error for missing wildcard param")
	}
	if _, err := r.URL("user.show", map[string]string{"id": "1", "extra": "x"}, nil); err == nil {
		t.Error("expected error for unexpected param")
	}
	if _, err := r.URL("user.show", map[string]string{"id": "abc"}, nil); err == nil {
		t.Error("expected error for value not matching the route regexp")
	}
	if _, err := r.URL("user.show", map[string]string{"id": "42"}, nil); err != nil {
		t.Errorf("unexpected error for valid params: %v", err)
	}
}

func TestMuxURLDuplicateName(t *testing.T) {
	r := NewRouter()
	r.Get("/a", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("dup")
	r.Get("/b", func(w http.ResponseWriter, r *http.Request) {})
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate route name")
		}
	}()
	r.Name("dup")
}

func TestMuxURLNameWithoutRoute(t *testing.T) {
	r := NewRouter()
	defer func() {
		if recover() == nil {
			t.Error("expected panic when Name() is called before any route")
		}
	}()
	r.Name("orphan")
}

func TestMuxURLAmbiguousAcrossMounts(t *testing.T) {
	r := NewRouter()
	a := NewRouter()
	a.Get("/x", func(w http.ResponseWriter, r *http.Request) {})
	a.Name("same")
	b := NewRouter()
	b.Get("/y", func(w http.ResponseWriter, r *http.Request) {})
	b.Name("same")
	r.Mount("/a", a)
	r.Mount("/b", b)

	if _, err := r.URL("same", nil, nil); err == nil {
		t.Error("expected error for ambiguous route name across mounts")
	}
	// each sub-router can still resolve its own name
	if u, err := a.URL("same", nil, nil); err != nil || u.Path != "/x" {
		t.Errorf("sub-router URL() = %v, %v; want /x, nil", u, err)
	}
}

func TestMuxURLQueryParams(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("user.show")

	query := url.Values{"id": {"query-value"}, "tab": {"info"}}
	u, err := r.URL("user.show", map[string]string{"id": "42"}, query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// the query param "id" must not be consumed as the path param
	if u.Path != "/users/42" {
		t.Errorf("Path = %q, want /users/42", u.Path)
	}
	if u.RawQuery != "id=query-value&tab=info" {
		t.Errorf("RawQuery = %q, want %q", u.RawQuery, "id=query-value&tab=info")
	}
	if u.String() != "/users/42?id=query-value&tab=info" {
		t.Errorf("String() = %q", u.String())
	}

	// a query param does not satisfy a missing path param
	if _, err := r.URL("user.show", nil, query); err == nil {
		t.Error("expected error when path param is only present in query")
	}
}

func TestMuxURLMountRoundTrip(t *testing.T) {
	var gotParams map[string]string
	var gotIDCount int
	handler := func(w http.ResponseWriter, r *http.Request) {
		rctx := RouteContext(r.Context())
		gotParams = map[string]string{}
		gotIDCount = 0
		for i, k := range rctx.URLParams.Keys {
			gotParams[k] = rctx.URLParams.Values[i]
			if k == "id" {
				gotIDCount++
			}
		}
		w.WriteHeader(http.StatusOK)
	}

	// three levels of Mount, with the same param name {id} on two levels
	// and a regexp param on the mount prefix
	teams := NewRouter()
	teams.Get("/teams/{id}", handler)
	teams.Name("team.detail")
	teams.Get("/files/*", handler)
	teams.Name("team.files")

	orgs := NewRouter()
	orgs.Mount("/orgs/{id}", teams)

	root := NewRouter()
	root.Mount("/api/{version:v[0-9]+}", orgs)

	u, err := root.URL("team.detail", map[string]string{"version": "v2", "id": "7"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/api/v2/orgs/7/teams/7" {
		t.Fatalf("Path = %q, want /api/v2/orgs/7/teams/7", u.Path)
	}

	// round-trip: the generated URL must route back to the handler
	req := httptest.NewRequest(http.MethodGet, u.String(), nil)
	rec := httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("round-trip status = %d, want 200", rec.Code)
	}
	if gotParams["version"] != "v2" || gotParams["id"] != "7" {
		t.Errorf("round-trip params = %v", gotParams)
	}
	// {id} appears on two levels, both captured
	if gotIDCount != 2 {
		t.Errorf("round-trip captured param 'id' %d times, want 2 (mount level and route level)", gotIDCount)
	}

	// mount-prefix regexp is enforced during generation
	if _, err := root.URL("team.detail", map[string]string{"version": "beta", "id": "7"}, nil); err == nil {
		t.Error("expected error for mount-prefix param not matching regexp")
	}

	// wildcard route through mounts, with characters needing encoding
	u, err = root.URL("team.files", map[string]string{"version": "v1", "id": "9", "*": "docs/a b.txt"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/api/v1/orgs/9/files/docs/a b.txt" {
		t.Fatalf("Path = %q", u.Path)
	}
	if u.RawPath != "/api/v1/orgs/9/files/docs/a%20b.txt" {
		t.Fatalf("RawPath = %q", u.RawPath)
	}

	req = httptest.NewRequest(http.MethodGet, u.String(), nil)
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("round-trip status = %d, want 200", rec.Code)
	}
	if gotParams["*"] != "docs/a b.txt" {
		t.Errorf("round-trip wildcard param = %q, want %q", gotParams["*"], "docs/a b.txt")
	}
}

func TestMuxURLUnnamedRoutesUnaffected(t *testing.T) {
	r := NewRouter()
	r.Get("/plain/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(URLParam(r, "id")))
	})

	// unnamed routes cannot be reversed
	if _, err := r.URL("plain", map[string]string{"id": "1"}, nil); err == nil {
		t.Error("expected error for unnamed route")
	}

	// but still route as before
	req := httptest.NewRequest(http.MethodGet, "/plain/abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "abc" {
		t.Errorf("unnamed route: status = %d, body = %q", rec.Code, rec.Body.String())
	}

	// naming after other routes still refers to the last registered one
	r.Get("/second", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("second")
	u, err := r.URL("second", nil, nil)
	if err != nil || u.Path != "/second" {
		t.Errorf("URL(second) = %v, %v; want /second, nil", u, err)
	}
}

func TestMuxURLInlineGroup(t *testing.T) {
	r := NewRouter()
	r.Group(func(gr Router) {
		gm := gr.(*Mux)
		gm.Get("/grouped/{id}", func(w http.ResponseWriter, r *http.Request) {})
		gm.Name("grouped")
	})

	u, err := r.URL("grouped", map[string]string{"id": "x"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/grouped/x" {
		t.Errorf("Path = %q, want /grouped/x", u.Path)
	}
}

func TestMuxURLStringConsistency(t *testing.T) {
	r := NewRouter()
	r.Get("/search/{q}", func(w http.ResponseWriter, r *http.Request) {})
	r.Name("search")

	u, err := r.URL("search", map[string]string{"q": "a?b c"}, url.Values{"page": {"1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.String() != "/search/a%3Fb%20c?page=1" {
		t.Errorf("String() = %q, want escaped path and query", u.String())
	}
	if u.Path != "/search/a?b c" {
		t.Errorf("Path = %q, want /search/a?b c", u.Path)
	}
}
