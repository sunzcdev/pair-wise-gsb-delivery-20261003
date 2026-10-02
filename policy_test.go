package secure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newRequest(method, target, host string) *http.Request {
	req, _ := http.NewRequestWithContext(context.Background(), method, target, nil)

	if host != "" {
		req.Host = host
	}

	return req
}

// A selector returning an unknown name must fail closed; it must never fall
// back to an empty/default policy that allows everything.
func TestPolicyUnknownNameFailsClosed(t *testing.T) {
	set, err := NewPolicySet(map[string]Options{
		"known": {
			FrameDeny: true,
		},
	})
	expect(t, err, nil)

	s := New()
	s.SetPolicySelector(set, func(*http.Request) string { return "does-not-exist" })

	res := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "http://good.example.com/", "good.example.com")
	s.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusBadRequest)
	expect(t, res.Body.String(), "Bad Request\n")
	expect(t, res.Header().Get(frameOptionsHeader), "")

	// Empty names are unknown as well.
	s.SetPolicySelector(set, func(*http.Request) string { return "" })

	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusBadRequest)
	expect(t, res.Header().Get(frameOptionsHeader), "")

	// A set without a selector is a misconfiguration and also fails closed.
	s = New()
	s.SetPolicySelector(set, nil)

	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusBadRequest)
}

// Host checks, HTTPS redirects and headers must all follow the single policy
// whose name the selector returned.
func TestPolicySelectionAppliesToAllChecks(t *testing.T) {
	set, err := NewPolicySet(map[string]Options{
		"strict": {
			AllowedHosts:       []string{"strict.example.com"},
			SSLRedirect:        true,
			ContentTypeNosniff: true,
		},
		"relaxed": {
			AllowedHosts:   []string{"relaxed.example.com"},
			FrameDeny:      true,
			ReferrerPolicy: "no-referrer",
			IsDevelopment:  false,
		},
	})
	expect(t, err, nil)

	s := New()
	s.SetPolicySelector(set, func(r *http.Request) string {
		return r.Header.Get("X-Tenant")
	})

	// strict tenant on its allowed HTTPS host gets the strict header.
	req := newRequest(http.MethodGet, "https://strict.example.com/", "strict.example.com")
	req.Header.Set("X-Tenant", "strict")

	res := httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusOK)
	expect(t, res.Header().Get(contentTypeHeader), contentTypeValue)
	expect(t, res.Header().Get(frameOptionsHeader), "")

	// strict tenant on a foreign host is rejected by its pinned policy, even
	// though that host is allowed under another policy in the same set.
	req = newRequest(http.MethodGet, "https://relaxed.example.com/", "relaxed.example.com")
	req.Header.Set("X-Tenant", "strict")

	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusInternalServerError)

	// strict tenant over HTTP is redirected by its pinned policy.
	req = newRequest(http.MethodGet, "http://strict.example.com/", "strict.example.com")
	req.Header.Set("X-Tenant", "strict")

	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://strict.example.com/")

	// relaxed tenant on its own host is served (no redirect, HTTP is fine)
	// with the relaxed headers only.
	req = newRequest(http.MethodGet, "http://relaxed.example.com/", "relaxed.example.com")
	req.Header.Set("X-Tenant", "relaxed")

	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusOK)
	expect(t, res.Header().Get(frameOptionsHeader), frameOptionsValue)
	expect(t, res.Header().Get(referrerPolicyHeader), "no-referrer")
	expect(t, res.Header().Get(contentTypeHeader), "")
}

// The selector must be invoked exactly once per request, regardless of
// whether the request later passes or fails a check.
func TestPolicySelectorCalledOnce(t *testing.T) {
	set, err := NewPolicySet(map[string]Options{
		"a": {AllowedHosts: []string{"a.example.com"}},
		"b": {AllowedHosts: []string{"b.example.com"}},
	})
	expect(t, err, nil)

	var calls int32

	s := New()
	s.SetPolicySelector(set, func(r *http.Request) string {
		atomic.AddInt32(&calls, 1)

		return r.Header.Get("X-Tenant")
	})

	cases := []struct {
		tenant string
		host   string
		code   int
	}{
		{"a", "a.example.com", http.StatusOK},
		{"a", "b.example.com", http.StatusInternalServerError},
		{"b", "b.example.com", http.StatusOK},
		{"missing", "a.example.com", http.StatusBadRequest},
	}

	for index, tc := range cases {
		req := newRequest(http.MethodGet, "http://"+tc.host+"/", tc.host)
		req.Header.Set("X-Tenant", tc.tenant)

		res := httptest.NewRecorder()
		s.Handler(myHandler).ServeHTTP(res, req)

		expect(t, res.Code, tc.code)
		expect(t, atomic.LoadInt32(&calls), int32(index+1))
	}
}

// Pre-compiled regex policies work, and every invalid policy blocks
// publication.
func TestPolicySetValidation(t *testing.T) {
	valid := map[string]Options{
		"regex": {
			AllowedHosts:         []string{`.*\.example\.com`},
			AllowedHostsAreRegex: true,
		},
	}

	set, err := NewPolicySet(valid)
	expect(t, err, nil)
	expect(t, set.Version(), uint64(1))

	s := New()
	s.SetPolicySelector(set, func(*http.Request) string { return "regex" })

	req := newRequest(http.MethodGet, "http://api.example.com/", "api.example.com")
	res := httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusOK)

	req = newRequest(http.MethodGet, "http://evil.example.org/", "evil.example.org")
	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, req)
	expect(t, res.Code, http.StatusInternalServerError)

	cases := map[string]Options{
		"bad-regex": {
			AllowedHosts:         []string{"[unterminated"},
			AllowedHostsAreRegex: true,
		},
		"bad-header-value": {
			ReferrerPolicy: "no-referrer\r\nX-Injected: yes",
		},
		"bad-dns-keyword": {
			XDNSPrefetchControl: "sometimes",
		},
		"negative-sts": {
			STSSeconds: -1,
		},
		"bad-ssl-header-name": {
			SSLProxyHeaders: map[string]string{"X Forwarded Proto": "https"},
		},
		"bad-ssl-header-value": {
			SSLProxyHeaders: map[string]string{"X-Forwarded-Proto": "ht\r\ntps"},
		},
		"bad-literal-host": {
			AllowedHosts: []string{"good.example.com\r\nEvil: yes"},
		},
	}

	for name, options := range cases {
		_, err := NewPolicySet(map[string]Options{name: options})
		if err == nil {
			t.Errorf("policy %q should have been rejected", name)
		}
	}

	if _, err := NewPolicySet(nil); err == nil {
		t.Error("an empty policy set should be rejected")
	}

	if _, err := NewPolicySet(map[string]Options{"": {}}); err == nil {
		t.Error("a policy with an empty name should be rejected")
	}
}

// An invalid Replace must not be published: the previous version keeps
// serving requests.
func TestPolicyReplaceInvalidKeepsOld(t *testing.T) {
	set, err := NewPolicySet(map[string]Options{
		"v1": {ContentTypeNosniff: true},
	})
	expect(t, err, nil)
	expect(t, set.Version(), uint64(1))

	// Valid update bumps the version.
	err = set.Replace(map[string]Options{
		"v2": {FrameDeny: true},
	})
	expect(t, err, nil)
	expect(t, set.Version(), uint64(2))

	// Invalid update leaves version 2 in place.
	err = set.Replace(map[string]Options{
		"bad": {AllowedHostsAreRegex: true, AllowedHosts: []string{"[oops"}},
	})
	if err == nil {
		t.Fatal("invalid Replace should return an error")
	}

	expect(t, set.Version(), uint64(2))

	if _, ok := set.lookup("bad"); ok {
		t.Error("invalid policy should not have been published")
	}

	if _, ok := set.lookup("v2"); !ok {
		t.Error("previous version policy v2 must remain available")
	}

	s := New()
	s.SetPolicySelector(set, func(*http.Request) string { return "v2" })

	res := httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, newRequest(http.MethodGet, "http://example.com/", "example.com"))
	expect(t, res.Header().Get(frameOptionsHeader), frameOptionsValue)
	expect(t, res.Header().Get(contentTypeHeader), "")
}

// Mutating the maps and slices passed to NewPolicySet/Replace must not change
// the published, in-use policies.
func TestPolicySetDefensiveCopy(t *testing.T) {
	named := map[string]Options{
		"tenant": {
			AllowedHosts:   []string{"tenant.example.com"},
			ReferrerPolicy: "no-referrer",
			SSLProxyHeaders: map[string]string{
				"X-Forwarded-Proto": "https",
			},
		},
	}

	set, err := NewPolicySet(named)
	expect(t, err, nil)

	// Mutate caller-owned input after construction.
	named["tenant"] = Options{AllowedHosts: []string{"attacker.example.com"}}

	s := New()
	s.SetPolicySelector(set, func(*http.Request) string { return "tenant" })

	res := httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, newRequest(http.MethodGet, "http://tenant.example.com/", "tenant.example.com"))
	expect(t, res.Code, http.StatusOK)

	res = httptest.NewRecorder()
	s.Handler(myHandler).ServeHTTP(res, newRequest(http.MethodGet, "http://attacker.example.com/", "attacker.example.com"))
	expect(t, res.Code, http.StatusInternalServerError)

	// Mutate the slice and map captured inside the Options value as well.
	named = map[string]Options{
		"tenant": {
			AllowedHosts:    []string{"tenant.example.com"},
			ReferrerPolicy:  "no-referrer",
			SSLProxyHeaders: map[string]string{"X-Forwarded-Proto": "https"},
		},
	}

	options := named["tenant"]
	options.AllowedHosts[0] = "attacker.example.com"
	options.SSLProxyHeaders["X-Forwarded-Proto"] = "http"

	policy, ok := set.lookup("tenant")
	expect(t, ok, true)
	expect(t, policy.opt.AllowedHosts[0], "tenant.example.com")
	expect(t, policy.opt.SSLProxyHeaders["X-Forwarded-Proto"], "https")
	expect(t, policy.opt.ReferrerPolicy, "no-referrer")

	// Replace also copies its input.
	replaceInput := map[string]Options{
		"tenant": {
			AllowedHosts:   []string{"new.example.com"},
			ReferrerPolicy: "same-origin",
		},
	}
	expect(t, set.Replace(replaceInput), nil)

	replaceInput["tenant"] = Options{AllowedHosts: []string{"attacker.example.com"}}

	policy, ok = set.lookup("tenant")
	expect(t, ok, true)
	expect(t, policy.opt.AllowedHosts[0], "new.example.com")
	expect(t, policy.opt.ReferrerPolicy, "same-origin")
}

// A request pins the policy snapshot when its name is selected. Blocking
// selectors make the switch boundary explicit:
//
//   - requests that pinned v1 before Replace finish with v1 headers even
//     though Replace happened before their (blocked) AllowRequestFunc ran;
//   - a request whose selector is held open across Replace resolves v2 once
//     the selector is released.
func TestPolicySetConcurrentSwitchBoundary(t *testing.T) {
	v1Gate := make(chan struct{})

	var enteredAllow int32

	blockingAllow := func(*http.Request) bool {
		atomic.AddInt32(&enteredAllow, 1)
		<-v1Gate

		return true
	}

	v1 := Options{
		FrameDeny:        true,
		ReferrerPolicy:   "no-referrer",
		AllowRequestFunc: blockingAllow,
	}
	v2 := Options{
		ContentTypeNosniff: true,
		ReferrerPolicy:     "same-origin",
	}

	set, err := NewPolicySet(map[string]Options{"p": v1})
	expect(t, err, nil)
	expect(t, set.Version(), uint64(1))

	// Instant selector: requests pin v1 and then block inside the pinned
	// policy's AllowRequestFunc.
	sBefore := New()
	sBefore.SetPolicySelector(set, func(*http.Request) string { return "p" })

	// Blocking selector: the name lookup cannot happen until the gate opens.
	selectorGate := make(chan struct{})

	var enteredSelector int32

	sAfter := New()
	sAfter.SetPolicySelector(set, func(*http.Request) string {
		atomic.AddInt32(&enteredSelector, 1)
		<-selectorGate

		return "p"
	})

	const requests = 8

	beforeResults := make([]*httptest.ResponseRecorder, requests)

	var wg sync.WaitGroup

	wg.Add(requests + 1)

	for i := 0; i < requests; i++ {
		i := i

		go func() {
			defer wg.Done()

			res := httptest.NewRecorder()
			req := newRequest(http.MethodGet, "http://example.com/", "example.com")
			sBefore.Handler(myHandler).ServeHTTP(res, req)
			beforeResults[i] = res
		}()
	}

	afterDone := make(chan struct{})
	afterResult := httptest.NewRecorder()

	go func() {
		defer wg.Done()
		defer close(afterDone)

		req := newRequest(http.MethodGet, "http://example.com/", "example.com")
		sAfter.Handler(myHandler).ServeHTTP(afterResult, req)
	}()

	// All v1 requests must have pinned v1 (and reached their post-selection
	// gate), and the late request must be blocked inside its selector.
	waitForCondition(t, func() bool {
		return atomic.LoadInt32(&enteredAllow) == requests
	}, "v1 requests never reached their blocking AllowRequestFunc")

	waitForCondition(t, func() bool {
		return atomic.LoadInt32(&enteredSelector) == 1
	}, "late request never entered its blocking selector")

	// Switch versions while requests are parked on both sides of the pin.
	expect(t, set.Replace(map[string]Options{"p": v2}), nil)
	expect(t, set.Version(), uint64(2))

	// Release the selector first: the name is looked up now, so the request
	// must observe v2.
	close(selectorGate)
	select {
	case <-afterDone:
	case <-time.After(5 * time.Second):
		t.Fatal("late request did not finish after selector release")
	}

	expect(t, afterResult.Code, http.StatusOK)
	expect(t, afterResult.Header().Get(contentTypeHeader), contentTypeValue)
	expect(t, afterResult.Header().Get(referrerPolicyHeader), "same-origin")
	expect(t, afterResult.Header().Get(frameOptionsHeader), "")

	// Now release the already-pinned v1 requests; they must finish with v1.
	close(v1Gate)
	wg.Wait()

	for i, res := range beforeResults {
		expect(t, res.Code, http.StatusOK)
		expect(t, res.Header().Get(frameOptionsHeader), frameOptionsValue)
		expect(t, res.Header().Get(referrerPolicyHeader), "no-referrer")

		if got := res.Header().Get(contentTypeHeader); got != "" {
			t.Errorf("request %d: expected no %s header, got %q", i, contentTypeHeader, got)
		}
	}
}

func waitForCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal(message)
}

// A named policy keeps supporting the $NONCE CSP template, producing a fresh
// nonce per request while using the pre-compiled template.
func TestPolicyCSPNonceTemplate(t *testing.T) {
	set, err := NewPolicySet(map[string]Options{
		"with-nonce": {
			ContentSecurityPolicy: "default-src 'self' $NONCE",
		},
	})
	expect(t, err, nil)

	s := New()
	s.SetPolicySelector(set, func(*http.Request) string { return "with-nonce" })

	res := httptest.NewRecorder()
	req := newRequest(http.MethodGet, "http://example.com/", "example.com")
	s.Handler(cspHandler).ServeHTTP(res, req)

	csp := res.Header().Get(cspHeader)
	if len(csp) == 0 || !strings.Contains(csp, "'nonce-") {
		t.Fatalf("expected a nonce in %q", csp)
	}

	nonce := res.Body.String()
	expect(t, csp, "default-src 'self' 'nonce-"+nonce+"'")
	expect(t, CSPNonce(req.Context()), "")

	// Two requests must get independent nonces (the compiled policy is reused).
	res2 := httptest.NewRecorder()
	s.Handler(cspHandler).ServeHTTP(res2, newRequest(http.MethodGet, "http://example.com/", "example.com"))

	if res2.Body.String() == nonce {
		t.Fatal("expected a fresh nonce for the second request")
	}
}

// A CSP template with an unsupported formatting verb must be rejected at
// publication time instead of corrupting a per-request header.
func TestPolicyCSPBadTemplateRejected(t *testing.T) {
	_, err := NewPolicySet(map[string]Options{
		"bad": {ContentSecurityPolicy: "default-src 'self' %s"},
	})
	if err == nil {
		t.Fatal("expected an invalid CSP template to be rejected")
	}
}
