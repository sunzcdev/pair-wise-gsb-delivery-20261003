package chi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// explainTestRouter builds a mux with overlapping static, regexp-param,
// param and wildcard routes, plus mounted subrouters, and counters that
// record any handler or middleware execution.
func explainTestRouter(handlerCalls, mwCalls *int32) *Mux {
	countMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(mwCalls, 1)
			next.ServeHTTP(w, r)
		})
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(handlerCalls, 1)
		rctx := RouteContext(r.Context())
		params := make([]string, 0, len(rctx.URLParams.Keys))
		for i, k := range rctx.URLParams.Keys {
			params = append(params, fmt.Sprintf("%s=%s", k, rctx.URLParams.Values[i]))
		}
		w.Write([]byte(rctx.RoutePattern() + "|" + strings.Join(params, ",")))
	}

	r := NewRouter()
	r.Use(countMw)
	r.Get("/users/list", h)
	r.Get("/users/{id:[0-9]+}", h)
	r.Get("/users/{name}", h)
	r.Get("/files/*", h)

	sub := NewRouter()
	sub.Use(countMw)
	sub.Get("/items/{itemID:[0-9]+}", h)
	sub.Get("/health", h)
	r.Mount("/api", sub)

	r.Mount("/ext", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(handlerCalls, 1)
		w.Write([]byte(r.Pattern + "|"))
	}))
	return r
}

func explainRequest(t *testing.T, mx *Mux, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mx.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func explainHasStep(exp *RouteExplanation, node, outcome string) *RouteStep {
	for i := range exp.Steps {
		s := &exp.Steps[i]
		if s.Node == node && s.Outcome == outcome {
			return s
		}
	}
	return nil
}

func explainFindStep(exp *RouteExplanation, node, outcome, value string) *RouteStep {
	for i := range exp.Steps {
		s := &exp.Steps[i]
		if s.Node == node && s.Outcome == outcome && s.Value == value {
			return s
		}
	}
	return nil
}

func TestExplainMatchesRealRequests(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	tests := []struct {
		method, path     string
		status           int
		pattern          string
		params           map[string]string
		methodNotAllowed bool
		allowed          []string
	}{
		{"GET", "/users/list", 200, "/users/list", nil, false, nil},
		{"GET", "/users/123", 200, "/users/{id:[0-9]+}", map[string]string{"id": "123"}, false, nil},
		{"GET", "/users/abc", 200, "/users/{name}", map[string]string{"name": "abc"}, false, nil},
		{"GET", "/users/listall", 200, "/users/{name}", map[string]string{"name": "listall"}, false, nil},
		{"GET", "/files/a/b/c", 200, "/files/*", map[string]string{"*": "a/b/c"}, false, nil},
		{"GET", "/api/items/42", 200, "/api/items/{itemID:[0-9]+}", map[string]string{"*": "", "itemID": "42"}, false, nil},
		{"GET", "/api/health", 200, "/api/health", map[string]string{"*": ""}, false, nil},
		{"GET", "/ext/anything", 200, "/ext/*", map[string]string{"*": "anything"}, false, nil},
		{"POST", "/users/list", 405, "", nil, true, []string{"GET"}},
		{"GET", "/users", 404, "", nil, false, nil},
		{"GET", "/api/items/xyz", 404, "", nil, false, nil},
		{"GET", "/nope", 404, "", nil, false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			code, body := explainRequest(t, mx, tt.method, tt.path)
			if code != tt.status {
				t.Fatalf("real request: expected status %d, got %d", tt.status, code)
			}
			if tt.status == 200 && !strings.HasPrefix(body, tt.pattern+"|") {
				t.Fatalf("real request: expected pattern %q in body %q", tt.pattern, body)
			}

			exp := mx.Explain(tt.method, tt.path)
			if exp.Matched != (tt.status == 200) {
				t.Fatalf("explain: expected Matched=%v, got %v\n%s", tt.status == 200, exp.Matched, exp)
			}
			if exp.Pattern != tt.pattern {
				t.Fatalf("explain: expected pattern %q, got %q\n%s", tt.pattern, exp.Pattern, exp)
			}
			if exp.MethodNotAllowed != tt.methodNotAllowed {
				t.Fatalf("explain: expected MethodNotAllowed=%v, got %v\n%s", tt.methodNotAllowed, exp.MethodNotAllowed, exp)
			}
			if !slices.Equal(exp.AllowedMethods, tt.allowed) {
				t.Fatalf("explain: expected allowed methods %v, got %v", tt.allowed, exp.AllowedMethods)
			}
			if len(exp.Params.Keys) != len(tt.params) {
				t.Fatalf("explain: expected params %v, got %v", tt.params, exp.Params)
			}
			for k, v := range tt.params {
				if got := exp.URLParam(k); got != v {
					t.Fatalf("explain: expected param %s=%q, got %q", k, v, got)
				}
			}
		})
	}
}

func TestExplainStaticFallback(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	// "/users/listall" partially matches the static "list" branch, which
	// dead-ends, so the search must fall back to the {name} param branch.
	exp := mx.Explain("GET", "/users/listall")
	if !exp.Matched || exp.Pattern != "/users/{name}" {
		t.Fatalf("expected fallback to /users/{name}, got\n%s", exp)
	}

	static := explainFindStep(exp, "static", "match", "list")
	if static == nil {
		t.Fatalf("expected static branch to consume %q, got\n%s", "list", exp)
	}
	if explainHasStep(exp, "static", "backtrack") == nil {
		t.Fatalf("expected a static backtrack step, got\n%s", exp)
	}
	param := explainFindStep(exp, "param", "match", "listall")
	if param == nil {
		t.Fatalf("expected param fallback with value %q, got\n%s", "listall", exp)
	}

	// "/users/li" fails the static prefix outright.
	exp = mx.Explain("GET", "/users/li")
	miss := explainHasStep(exp, "static", "miss")
	if miss == nil || !strings.Contains(miss.Note, "list") {
		t.Fatalf("expected static prefix-mismatch step, got\n%s", exp)
	}
	if !exp.Matched || exp.URLParam("name") != "li" {
		t.Fatalf("expected match on /users/{name}, got\n%s", exp)
	}
}

func TestExplainRegexpFailurePoint(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	exp := mx.Explain("GET", "/users/abc")
	miss := explainHasStep(exp, "regexp", "miss")
	if miss == nil {
		t.Fatalf("expected a regexp miss step, got\n%s", exp)
	}
	if miss.Value != "abc" || !strings.Contains(miss.Note, "[0-9]+") {
		t.Fatalf("expected regexp failure on candidate %q, got step %+v", "abc", *miss)
	}
	if !exp.Matched || exp.URLParam("name") != "abc" {
		t.Fatalf("expected match on /users/{name}, got\n%s", exp)
	}
}

func TestExplainWildcardConsumption(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	exp := mx.Explain("GET", "/files/a/b/c")
	ca := explainHasStep(exp, "catchall", "match")
	if ca == nil || ca.Value != "a/b/c" {
		t.Fatalf("expected catchall to consume %q, got\n%s", "a/b/c", exp)
	}
	if exp.URLParam("*") != "a/b/c" {
		t.Fatalf("expected wildcard param, got\n%s", exp)
	}
}

func TestExplainMountBoundary(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	exp := mx.Explain("GET", "/api/items/42")
	mount := explainHasStep(exp, "mount", "mount")
	if mount == nil {
		t.Fatalf("expected a mount boundary step, got\n%s", exp)
	}
	if mount.Prefix != "/api/*" || mount.Path != "/items/42" {
		t.Fatalf("expected mount at /api/* with remaining path /items/42, got %+v", *mount)
	}
	if exp.Pattern != "/api/items/{itemID:[0-9]+}" || exp.URLParam("itemID") != "42" {
		t.Fatalf("unexpected mount result\n%s", exp)
	}

	// a miss inside the subrouter still shows the boundary crossing
	exp = mx.Explain("GET", "/api/items/xyz")
	if exp.Matched {
		t.Fatalf("expected no match, got\n%s", exp)
	}
	if explainHasStep(exp, "mount", "mount") == nil {
		t.Fatalf("expected mount boundary before subrouter miss, got\n%s", exp)
	}
	miss := explainHasStep(exp, "regexp", "miss")
	if miss == nil || miss.Value != "xyz" {
		t.Fatalf("expected regexp miss inside subrouter, got\n%s", exp)
	}

	// a non-chi mounted handler ends the trace at the wildcard that
	// delivers the request to it
	exp = mx.Explain("GET", "/ext/anything")
	ca := explainFindStep(exp, "catchall", "match", "anything")
	if ca == nil {
		t.Fatalf("expected catchall to consume %q, got\n%s", "anything", exp)
	}
	if !exp.Matched || exp.Pattern != "/ext/*" {
		t.Fatalf("expected match on /ext/*, got\n%s", exp)
	}
}

func TestExplainMethodNotAllowed(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	exp := mx.Explain("POST", "/users/list")
	if exp.Matched || !exp.MethodNotAllowed {
		t.Fatalf("expected method-not-allowed explanation, got\n%s", exp)
	}
	if !slices.Equal(exp.AllowedMethods, []string{"GET"}) {
		t.Fatalf("expected allowed methods [GET], got %v", exp.AllowedMethods)
	}
	if explainHasStep(exp, "static", "method-not-allowed") == nil {
		t.Fatalf("expected a method-not-allowed step, got\n%s", exp)
	}

	code, _ := explainRequest(t, mx, "POST", "/users/list")
	if code != 405 {
		t.Fatalf("expected real request to return 405, got %d", code)
	}
}

func TestExplainNoSideEffects(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	var routesBefore []string
	if err := Walk(mx, func(m, route string, h http.Handler, mws ...func(http.Handler) http.Handler) error {
		routesBefore = append(routesBefore, m+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(routesBefore)

	for _, path := range []string{"/users/list", "/users/abc", "/users", "/api/items/1", "/api/nope", "/files/x"} {
		mx.Explain("GET", path)
		mx.Explain("DELETE", path)
	}

	if n := atomic.LoadInt32(&handlerCalls); n != 0 {
		t.Fatalf("Explain executed %d handlers", n)
	}
	if n := atomic.LoadInt32(&mwCalls); n != 0 {
		t.Fatalf("Explain executed %d middlewares", n)
	}

	var routesAfter []string
	if err := Walk(mx, func(m, route string, h http.Handler, mws ...func(http.Handler) http.Handler) error {
		routesAfter = append(routesAfter, m+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(routesAfter)
	if !slices.Equal(routesBefore, routesAfter) {
		t.Fatalf("routing tree changed by Explain:\nbefore: %v\nafter:  %v", routesBefore, routesAfter)
	}

	// routing still works afterwards
	if code, _ := explainRequest(t, mx, "GET", "/users/list"); code != 200 {
		t.Fatalf("expected 200 after Explain calls, got %d", code)
	}
}

func TestExplainStableAfterContextReuse(t *testing.T) {
	var handlerCalls, mwCalls int32
	mx := explainTestRouter(&handlerCalls, &mwCalls)

	exp := mx.Explain("GET", "/api/items/42")
	before := exp.String()
	pattern := exp.Pattern
	param := exp.URLParam("itemID")
	steps := len(exp.Steps)

	// churn the mux's context pool with real requests
	for i := 0; i < 100; i++ {
		explainRequest(t, mx, "GET", fmt.Sprintf("/users/%d", i))
		explainRequest(t, mx, "GET", "/nope")
	}

	if exp.String() != before || exp.Pattern != pattern || exp.URLParam("itemID") != param || len(exp.Steps) != steps {
		t.Fatalf("explanation changed after request contexts were recycled")
	}
}
