package chi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNamedURLBasic(t *testing.T) {
	r := NewRouter()
	r.NamedGet("user.show", "/users/{id}", func(w http.ResponseWriter, r *http.Request) {})
	r.NamedGet("home", "/", func(w http.ResponseWriter, r *http.Request) {})

	u, err := r.URL("user.show", map[string]string{"id": "42"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/users/42" {
		t.Errorf("expected path /users/42, got %s", u.Path)
	}
	if u.RawPath != "" {
		t.Errorf("expected empty RawPath, got %s", u.RawPath)
	}
	if u.String() != "/users/42" {
		t.Errorf("expected string /users/42, got %s", u.String())
	}

	u, err = r.URL("home", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/" {
		t.Errorf("expected path /, got %s", u.Path)
	}
}

func TestNamedURLRegexpParam(t *testing.T) {
	r := NewRouter()
	r.NamedGet("article", "/articles/{id:\\d+}", func(w http.ResponseWriter, r *http.Request) {})
	r.NamedGet("date", "/date/{yyyy:\\d\\d\\d\\d}/{mm:\\d\\d}", func(w http.ResponseWriter, r *http.Request) {})

	u, err := r.URL("article", map[string]string{"id": "123"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/articles/123" {
		t.Errorf("expected /articles/123, got %s", u.Path)
	}

	if _, err = r.URL("article", map[string]string{"id": "abc"}, nil); err == nil {
		t.Error("expected error for value not matching the route regexp")
	}

	u, err = r.URL("date", map[string]string{"yyyy": "2026", "mm": "09"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/date/2026/09" {
		t.Errorf("expected /date/2026/09, got %s", u.Path)
	}
}

func TestNamedURLWildcard(t *testing.T) {
	r := NewRouter()
	r.NamedGet("file", "/files/*", func(w http.ResponseWriter, r *http.Request) {})

	u, err := r.URL("file", map[string]string{"*": "a/b/c.txt"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/files/a/b/c.txt" {
		t.Errorf("expected /files/a/b/c.txt, got %s", u.Path)
	}
	if u.RawPath != "" {
		t.Errorf("expected empty RawPath, got %s", u.RawPath)
	}

	// Wildcard values keep their '/' separators but escape the rest.
	u, err = r.URL("file", map[string]string{"*": "a b/中%.txt"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/files/a b/中%.txt" {
		t.Errorf("unexpected path %s", u.Path)
	}
	if u.EscapedPath() != "/files/a%20b/%E4%B8%AD%25.txt" {
		t.Errorf("unexpected escaped path %s", u.EscapedPath())
	}

	if _, err = r.URL("file", nil, nil); err == nil {
		t.Error("expected error for missing wildcard value")
	}
}

func TestNamedURLEscaping(t *testing.T) {
	r := NewRouter()
	r.NamedGet("user.show", "/users/{name}", func(w http.ResponseWriter, r *http.Request) {})

	values := []string{
		"a/b",      // slash
		"100%",     // percent
		"a b",      // space
		"中文用户",     // unicode
		"a/b %中?#", // everything at once
	}
	for _, v := range values {
		u, err := r.URL("user.show", map[string]string{"name": v}, nil)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", v, err)
		}
		// Path holds the decoded value.
		if u.Path != "/users/"+v {
			t.Errorf("value %q: expected path %q, got %q", v, "/users/"+v, u.Path)
		}
		// RawPath is set and decodes to Path exactly once.
		if u.RawPath == "" {
			t.Errorf("value %q: expected RawPath to be set", v)
		}
		decoded, err := url.PathUnescape(u.RawPath)
		if err != nil {
			t.Fatalf("value %q: RawPath %q is not unescapable: %v", v, u.RawPath, err)
		}
		if decoded != u.Path {
			t.Errorf("value %q: RawPath %q decodes to %q, want %q", v, u.RawPath, decoded, u.Path)
		}
		// The serialized URL parses back to the same Path.
		reparsed, err := url.Parse(u.String())
		if err != nil {
			t.Fatalf("value %q: cannot reparse %q: %v", v, u.String(), err)
		}
		if reparsed.Path != u.Path {
			t.Errorf("value %q: reparsed path %q, want %q", v, reparsed.Path, u.Path)
		}
	}
}

func TestNamedURLErrors(t *testing.T) {
	r := NewRouter()
	r.NamedGet("user.show", "/users/{id}", func(w http.ResponseWriter, r *http.Request) {})

	if _, err := r.URL("nope", nil, nil); err == nil {
		t.Error("expected error for unknown route name")
	}
	if _, err := r.URL("user.show", nil, nil); err == nil {
		t.Error("expected error for missing parameter")
	}
	if _, err := r.URL("user.show", map[string]string{"id": ""}, nil); err == nil {
		t.Error("expected error for empty parameter value")
	}
	if _, err := r.URL("user.show", map[string]string{"id": "1", "extra": "x"}, nil); err == nil {
		t.Error("expected error for unexpected parameter")
	}
	// Query values are never consumed as path parameters.
	if _, err := r.URL("user.show", nil, url.Values{"id": {"1"}}); err == nil {
		t.Error("expected error: query values must not satisfy path parameters")
	}
}

func TestNamedURLDuplicateName(t *testing.T) {
	r := NewRouter()
	r.NamedGet("dup", "/a/{id}", func(w http.ResponseWriter, r *http.Request) {})

	// Same name and same pattern for another method is fine.
	r.NamedPost("dup", "/a/{id}", func(w http.ResponseWriter, r *http.Request) {})

	defer func() {
		if recover() == nil {
			t.Error("expected panic for duplicate route name with different pattern")
		}
	}()
	r.NamedGet("dup", "/b/{id}", func(w http.ResponseWriter, r *http.Request) {})
}

func TestNamedURLQuery(t *testing.T) {
	r := NewRouter()
	r.NamedGet("search", "/search/{term}", func(w http.ResponseWriter, r *http.Request) {})

	q := url.Values{}
	q.Set("q", "a b")
	q.Add("tag", "x")
	q.Add("tag", "y")
	q.Set("term", "from-query")

	u, err := r.URL("search", map[string]string{"term": "go"}, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/search/go" {
		t.Errorf("expected /search/go, got %s", u.Path)
	}
	if u.Query().Get("q") != "a b" || u.Query()["tag"][1] != "y" {
		t.Errorf("unexpected query %s", u.RawQuery)
	}
	// The query's own "term" key stays in the query string.
	if u.Query().Get("term") != "from-query" {
		t.Errorf("expected query term to survive, got %s", u.RawQuery)
	}
	if !strings.HasPrefix(u.String(), "/search/go?") {
		t.Errorf("unexpected URL %s", u.String())
	}
}

func TestNamedURLMountedRouters(t *testing.T) {
	root := NewRouter()
	api := NewRouter()
	org := NewRouter()

	org.NamedGet("member.show", "/members/{memberID}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "org=%s member=%s", URLParam(r, "orgID"), URLParam(r, "memberID"))
	})
	api.Mount("/orgs/{orgID}", org)
	root.Mount("/api", api)

	u, err := root.URL("member.show", map[string]string{"orgID": "7", "memberID": "3"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/api/orgs/7/members/3" {
		t.Fatalf("expected /api/orgs/7/members/3, got %s", u.Path)
	}

	// Round trip: the generated URL routes back to the named handler.
	ts := httptest.NewServer(root)
	defer ts.Close()

	resp, body := testNamedRequest(t, ts, u.String())
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "org=7 member=3" {
		t.Errorf("unexpected body %q", body)
	}
}

func TestNamedURLSameParamDifferentLevels(t *testing.T) {
	root := NewRouter()
	sub := NewRouter()

	sub.NamedGet("thing", "/things/{id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "id=%s pattern=%s", URLParam(r, "id"), RouteContext(r.Context()).RoutePattern())
	})
	root.Mount("/{id}", sub)

	// The same parameter name appears in the mount pattern and in the
	// route pattern; both levels are filled from the same params entry.
	u, err := root.URL("thing", map[string]string{"id": "5"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/5/things/5" {
		t.Fatalf("expected /5/things/5, got %s", u.Path)
	}

	ts := httptest.NewServer(root)
	defer ts.Close()

	resp, body := testNamedRequest(t, ts, u.String())
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != "id=5 pattern=/{id}/things/{id}" {
		t.Errorf("unexpected body %q", body)
	}
}

func TestNamedURLMountRegexpPrefix(t *testing.T) {
	root := NewRouter()
	sub := NewRouter()
	sub.NamedGet("doc", "/docs/{slug}", func(w http.ResponseWriter, r *http.Request) {})
	root.Mount("/tenants/{tenant:[a-z]+}", sub)

	u, err := root.URL("doc", map[string]string{"tenant": "acme", "slug": "intro"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/tenants/acme/docs/intro" {
		t.Errorf("expected /tenants/acme/docs/intro, got %s", u.Path)
	}

	if _, err = root.URL("doc", map[string]string{"tenant": "ACME", "slug": "intro"}, nil); err == nil {
		t.Error("expected error for mount prefix value not matching its regexp")
	}
}

func TestNamedURLAmbiguousName(t *testing.T) {
	root := NewRouter()
	sub := NewRouter()
	sub.NamedGet("item", "/items/{id}", func(w http.ResponseWriter, r *http.Request) {})
	root.Mount("/a", sub)
	root.Mount("/b", sub)

	if _, err := root.URL("item", map[string]string{"id": "1"}, nil); err == nil {
		t.Error("expected error for a name reachable through multiple mounts")
	}
}

func TestNamedURLGroupAndRoute(t *testing.T) {
	r := NewRouter()
	r.Group(func(r Router) {
		r.NamedGet("grouped", "/g/{id}", func(w http.ResponseWriter, r *http.Request) {})
	})
	r.Route("/sub", func(r Router) {
		r.NamedGet("routed", "/s/{id}", func(w http.ResponseWriter, r *http.Request) {})
	})

	u, err := r.URL("grouped", map[string]string{"id": "1"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/g/1" {
		t.Errorf("expected /g/1, got %s", u.Path)
	}

	u, err = r.URL("routed", map[string]string{"id": "2"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Path != "/sub/s/2" {
		t.Errorf("expected /sub/s/2, got %s", u.Path)
	}
}

func TestNamedURLEscapedRoundTrip(t *testing.T) {
	root := NewRouter()
	root.NamedGet("user.show", "/users/{name}", func(w http.ResponseWriter, r *http.Request) {
		// chi matches against the escaped path, so the handler observes the
		// escaped value; a single PathUnescape recovers the original.
		escaped := URLParam(r, "name")
		decoded, err := url.PathUnescape(escaped)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write([]byte(decoded))
	})

	original := "a/b %中"
	u, err := root.URL("user.show", map[string]string{"name": original}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ts := httptest.NewServer(root)
	defer ts.Close()

	resp, body := testNamedRequest(t, ts, u.String())
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body != original {
		t.Errorf("expected %q after exactly one decode, got %q", original, body)
	}
}

func TestNamedURLUnnamedRoutesUnaffected(t *testing.T) {
	r := NewRouter()
	r.Get("/plain/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(URLParam(r, "id")))
	})
	r.NamedGet("named", "/named/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(URLParam(r, "id")))
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	for _, path := range []string{"/plain/1", "/named/2"} {
		resp, body := testNamedRequest(t, ts, path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: expected 200, got %d", path, resp.StatusCode)
		}
		if body != path[len(path)-1:] {
			t.Errorf("%s: unexpected body %q", path, body)
		}
	}
}

func testNamedRequest(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 64)
	buf := make([]byte, 64)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp, string(body)
}
