package secure

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

//nolint:gochecknoglobals
var cspHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(CSPNonce(r.Context())))
})

func TestCSPNonce(t *testing.T) {
	csp := "default-src 'self' $NONCE; script-src 'strict-dynamic' $NONCE"
	cases := []struct {
		options Options
		headers []string
	}{
		{Options{ContentSecurityPolicy: csp}, []string{"Content-Security-Policy"}},
		{Options{ContentSecurityPolicyReportOnly: csp}, []string{"Content-Security-Policy-Report-Only"}},
		{
			Options{ContentSecurityPolicy: csp, ContentSecurityPolicyReportOnly: csp},
			[]string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"},
		},
	}

	for _, c := range cases {
		s := New(c.options)

		res := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

		s.Handler(cspHandler).ServeHTTP(res, req)

		expect(t, res.Code, http.StatusOK)

		for _, header := range c.headers {
			csp := res.Header().Get(header)
			expect(t, strings.Count(csp, "'nonce-"), 2)

			nonce := strings.Split(strings.Split(csp, "'")[3], "-")[1]
			// Test that the context has the CSP nonce, but only during the request.
			expect(t, res.Body.String(), nonce)
			expect(t, CSPNonce(req.Context()), "")

			_, err := base64.RawStdEncoding.DecodeString(nonce)
			expect(t, err, nil)

			expect(t, csp, fmt.Sprintf("default-src 'self' 'nonce-%[1]s'; script-src 'strict-dynamic' 'nonce-%[1]s'", nonce))
		}
	}
}

func TestWithCSPNonce(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	nonce := "jdgKGHkbnd+/"

	expect(t, CSPNonce(withCSPNonce(req, nonce).Context()), nonce)
}

// cspNonceFromHeader extracts the nonce value from a CSP header of the form
// "default-src 'self' 'nonce-<nonce>'".
func cspNonceFromHeader(t *testing.T, header string) string {
	t.Helper()

	parts := strings.Split(header, "'nonce-")
	if len(parts) != 2 {
		t.Fatalf("expected exactly one nonce in header %q", header)
	}

	return strings.TrimSuffix(parts[1], "'")
}

func TestReusedCSPNonceNestedMiddleware(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	outer := New(Options{ContentSecurityPolicy: csp})
	inner := New(Options{ContentSecurityPolicy: csp, ReuseNonce: true})

	var outerHeader string

	bridge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the header set by the outer layer before the inner layer runs.
		outerHeader = w.Header().Get(cspHeader)
		inner.Handler(cspHandler).ServeHTTP(w, r)
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	outer.Handler(bridge).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	outerNonce := cspNonceFromHeader(t, outerHeader)
	innerNonce := cspNonceFromHeader(t, res.Header().Get(cspHeader))

	// Both layers and the template (handler body) must share the same nonce.
	expect(t, innerNonce, outerNonce)
	expect(t, res.Body.String(), outerNonce)
	expect(t, res.Header().Get(cspHeader), fmt.Sprintf("default-src 'self' 'nonce-%s'", outerNonce))
}

func TestNestedMiddlewareDefaultGeneratesFreshNonce(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	outer := New(Options{ContentSecurityPolicy: csp})
	inner := New(Options{ContentSecurityPolicy: csp})

	var outerHeader string

	bridge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outerHeader = w.Header().Get(cspHeader)
		inner.Handler(cspHandler).ServeHTTP(w, r)
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	outer.Handler(bridge).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	outerNonce := cspNonceFromHeader(t, outerHeader)
	innerNonce := cspNonceFromHeader(t, res.Header().Get(cspHeader))

	// Default behavior is preserved: each layer generates its own nonce.
	if outerNonce == innerNonce {
		t.Fatalf("expected distinct nonces without ReuseNonce, both were %q", outerNonce)
	}

	expect(t, res.Body.String(), innerNonce)
}

func TestReusedCSPNonceSharedWithReportOnly(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	outer := New(Options{ContentSecurityPolicy: csp})
	inner := New(Options{
		ContentSecurityPolicy:           csp,
		ContentSecurityPolicyReportOnly: csp,
		ReuseNonce:                      true,
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	outer.Handler(inner.Handler(cspHandler)).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	enforceNonce := cspNonceFromHeader(t, res.Header().Get(cspHeader))
	reportOnlyNonce := cspNonceFromHeader(t, res.Header().Get(cspReportOnlyHeader))

	// Enforce and report-only policies share the same value, which the template sees too.
	expect(t, reportOnlyNonce, enforceNonce)
	expect(t, res.Body.String(), enforceNonce)
}

func TestReuseNonceProcessAndReturnNonce(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	s := New(Options{ContentSecurityPolicy: csp, ReuseNonce: true})

	preset := cspRandNonce()

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(WithCSPNonce(context.Background(), preset), http.MethodGet, "/foo", nil)

	nonce, err := s.ProcessAndReturnNonce(res, req)
	expect(t, err, nil)
	expect(t, nonce, preset)
	expect(t, res.Header().Get(cspHeader), fmt.Sprintf("default-src 'self' 'nonce-%s'", preset))
}

func TestReuseNonceInvalidInternalValue(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	s := New(Options{ContentSecurityPolicy: csp, ReuseNonce: true})

	invalidNonces := []string{
		"not base64!!!", // invalid format
		base64.RawStdEncoding.EncodeToString([]byte("too-short")), // valid base64, wrong length
	}

	for _, invalid := range invalidNonces {
		res := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(WithCSPNonce(context.Background(), invalid), http.MethodGet, "/foo", nil)

		_, err := s.ProcessAndReturnNonce(res, req)
		if !errors.Is(err, ErrInvalidCSPNonce) {
			t.Fatalf("expected ErrInvalidCSPNonce for nonce %q, got %v", invalid, err)
		}

		expect(t, res.Header().Get(cspHeader), "")
	}
}

func TestReuseNonceIgnoresClientHeaders(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	s := New(Options{ContentSecurityPolicy: csp, ReuseNonce: true})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)
	req.Header.Set("CSP-Nonce", "client-supplied-value")
	req.Header.Set("X-CSP-Nonce", "client-supplied-value")

	nonce, err := s.ProcessAndReturnNonce(res, req)
	expect(t, err, nil)

	// The nonce must be freshly generated, never filled from client headers.
	if nonce == "" || nonce == "client-supplied-value" {
		t.Fatalf("expected a freshly generated nonce, got %q", nonce)
	}

	expect(t, cspNonceFromHeader(t, res.Header().Get(cspHeader)), nonce)
}

func TestReuseNonceDistinctPerRequest(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	s := New(Options{ContentSecurityPolicy: csp, ReuseNonce: true})

	nonces := make(map[string]struct{})

	for i := 0; i < 2; i++ {
		res := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

		s.Handler(cspHandler).ServeHTTP(res, req)

		expect(t, res.Code, http.StatusOK)
		nonces[cspNonceFromHeader(t, res.Header().Get(cspHeader))] = struct{}{}
	}

	// Different requests remain independent.
	expect(t, len(nonces), 2)
}
