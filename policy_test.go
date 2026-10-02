package secure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func tenantSelector(r *http.Request) string {
	return r.Header.Get("X-Policy")
}

func TestPolicyMuxSelectsNamedPolicy(t *testing.T) {
	mux, err := NewPolicyMux(tenantSelector, map[string]Options{
		"strict": {
			AllowedHosts:       []string{exampleHost},
			SSLRedirect:        true,
			FrameDeny:          true,
			STSSeconds:         31536000,
			ForceSTSHeader:     true,
			ContentTypeNosniff: true,
		},
		"loose": {
			ContentSecurityPolicy: "default-src 'self'",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := mux.Handler(myHandler)

	// The "strict" policy applies its host check, HTTPS redirect and headers.
	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+exampleHost+"/foo", nil)
	req.Header.Set("X-Policy", "strict")
	handler.ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)
	expect(t, res.Header().Get(frameOptionsHeader), frameOptionsValue)
	expect(t, res.Header().Get(contentTypeHeader), contentTypeValue)
	expect(t, res.Header().Get(stsHeader), "max-age=31536000")

	// The "strict" policy rejects unknown hosts.
	res = httptest.NewRecorder()
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "https://evil.example.com/foo", nil)
	req.Header.Set("X-Policy", "strict")
	handler.ServeHTTP(res, req)

	expect(t, res.Code, http.StatusInternalServerError)

	// The "strict" policy redirects plain HTTP to HTTPS.
	res = httptest.NewRecorder()
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+exampleHost+"/foo", nil)
	req.Header.Set("X-Policy", "strict")
	handler.ServeHTTP(res, req)

	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://"+exampleHost+"/foo")

	// The "loose" policy applies only its own headers, no host check or redirect.
	res = httptest.NewRecorder()
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "http://anything.example.com/foo", nil)
	req.Header.Set("X-Policy", "loose")
	handler.ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)
	expect(t, res.Header().Get(cspHeader), "default-src 'self'")
	expect(t, res.Header().Get(frameOptionsHeader), "")
}

func TestPolicyMuxUnknownPolicyFailsClosed(t *testing.T) {
	mux, err := NewPolicyMux(tenantSelector, map[string]Options{
		"known": {FrameDeny: true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/foo", nil)
	req.Header.Set("X-Policy", "does-not-exist")
	mux.Handler(inner).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusInternalServerError)
	expect(t, called, false)
	// Fail closed: no permissive default headers are written either.
	expect(t, res.Header().Get(frameOptionsHeader), "")
}

func TestPolicyMuxNilSelector(t *testing.T) {
	//nolint:staticcheck
	if _, err := NewPolicyMux(nil, map[string]Options{"a": {}}); err == nil {
		t.Fatal("expected an error for a nil selector")
	}
}

func TestPolicyMuxInvalidPoliciesRejected(t *testing.T) {
	// Invalid regular expression.
	_, err := NewPolicyMux(tenantSelector, map[string]Options{
		"bad": {
			AllowedHosts:         []string{"[invalid"},
			AllowedHostsAreRegex: true,
		},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid AllowedHosts regex")
	}

	// Invalid header value (CRLF injection).
	_, err = NewPolicyMux(tenantSelector, map[string]Options{
		"bad": {
			ContentSecurityPolicy: "default-src 'self'\r\nX-Injected: true",
		},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid header value")
	}
}

func TestPolicyMuxUpdateInvalidKeepsOldVersion(t *testing.T) {
	mux, err := NewPolicyMux(tenantSelector, map[string]Options{
		"tenant": {ContentSecurityPolicy: "default-src 'v1'"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = mux.Update(map[string]Options{
		"tenant": {
			AllowedHosts:         []string{"(broken"},
			AllowedHostsAreRegex: true,
		},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid update")
	}

	err = mux.Update(map[string]Options{
		"tenant": {CustomFrameOptionsValue: "DENY\nInjected: yes"},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid header value update")
	}

	// The old version is still active.
	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/foo", nil)
	req.Header.Set("X-Policy", "tenant")
	mux.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)
	expect(t, res.Header().Get(cspHeader), "default-src 'v1'")
	expect(t, res.Header().Get(frameOptionsHeader), "")
}

func TestPolicyMuxUpdateAppliesNewVersion(t *testing.T) {
	mux, err := NewPolicyMux(tenantSelector, map[string]Options{
		"tenant": {ContentSecurityPolicy: "default-src 'v1'"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = mux.Update(map[string]Options{
		"tenant": {ContentSecurityPolicy: "default-src 'v2'"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/foo", nil)
	req.Header.Set("X-Policy", "tenant")
	mux.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Header().Get(cspHeader), "default-src 'v2'")
}

func TestPolicyMuxExternalMutationDoesNotAffectSet(t *testing.T) {
	allowedHosts := []string{exampleHost}
	proxyHeaders := map[string]string{"X-Forwarded-Proto": "https"}
	policies := map[string]Options{
		"tenant": {
			AllowedHosts:    allowedHosts,
			SSLProxyHeaders: proxyHeaders,
			FrameDeny:       true,
		},
	}

	mux, err := NewPolicyMux(tenantSelector, policies)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Mutate everything the caller still holds a reference to.
	allowedHosts[0] = "attacker.example.com"
	proxyHeaders["X-Forwarded-Proto"] = "http"
	policies["tenant"] = Options{}
	delete(policies, "tenant")

	// The compiled set is unchanged: the original host is still allowed...
	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+exampleHost+"/foo", nil)
	req.Header.Set("X-Policy", "tenant")
	mux.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)
	expect(t, res.Header().Get(frameOptionsHeader), frameOptionsValue)

	// ...and the attacker host is still rejected.
	res = httptest.NewRecorder()
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "http://attacker.example.com/foo", nil)
	req.Header.Set("X-Policy", "tenant")
	mux.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusInternalServerError)
}

// TestPolicyMuxBlockingSelectorVersionBoundary uses a selector that blocks
// mid-request to prove that a request which captured the policy set before
// an Update keeps using the old version, while requests arriving after the
// Update use the new version.
func TestPolicyMuxBlockingSelectorVersionBoundary(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once
	selector := func(r *http.Request) string {
		name := r.Header.Get("X-Policy")
		if name == "blocked" {
			once.Do(func() { close(entered) })
			<-release

			return "tenant"
		}

		return name
	}

	mux, err := NewPolicyMux(selector, map[string]Options{
		"tenant": {ContentSecurityPolicy: "default-src 'v1'"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := mux.Handler(myHandler)

	// Request A starts and blocks inside the selector, having already
	// captured version 1 of the policy set.
	resA := httptest.NewRecorder()
	reqA, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/foo", nil)
	reqA.Header.Set("X-Policy", "blocked")

	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		handler.ServeHTTP(resA, reqA)
	}()

	<-entered

	// Publish version 2 while request A is still in flight.
	err = mux.Update(map[string]Options{
		"tenant": {ContentSecurityPolicy: "default-src 'v2'"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	close(release)
	<-doneA

	// Request A resolved its policy against the version it captured.
	expect(t, resA.Code, http.StatusOK)
	expect(t, resA.Header().Get(cspHeader), "default-src 'v1'")

	// Request B arrives after the update and sees version 2.
	resB := httptest.NewRecorder()
	reqB, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/foo", nil)
	reqB.Header.Set("X-Policy", "tenant")
	handler.ServeHTTP(resB, reqB)

	expect(t, resB.Code, http.StatusOK)
	expect(t, resB.Header().Get(cspHeader), "default-src 'v2'")
}

// TestPolicyMuxConcurrentUpdateAndRequests hammers the mux with requests
// while the policy set is swapped, to surface data races under -race.
func TestPolicyMuxConcurrentUpdateAndRequests(t *testing.T) {
	mux, err := NewPolicyMux(tenantSelector, map[string]Options{
		"tenant": {ContentSecurityPolicy: "default-src 'v1'"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handler := mux.Handler(myHandler)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				res := httptest.NewRecorder()
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/foo", nil)
				req.Header.Set("X-Policy", "tenant")
				handler.ServeHTTP(res, req)

				if got := res.Header().Get(cspHeader); got != "default-src 'v1'" && got != "default-src 'v2'" {
					t.Errorf("unexpected CSP header: %q", got)
				}
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			version := fmt.Sprintf("default-src 'v%d'", j%2+1)
			if err := mux.Update(map[string]Options{
				"tenant": {ContentSecurityPolicy: version},
			}); err != nil {
				t.Errorf("unexpected update error: %v", err)
			}
		}
	}()

	wg.Wait()
}
