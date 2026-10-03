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

func TestCSPNonceReuseNestedMiddleware(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	outer := New(Options{ContentSecurityPolicy: csp})
	inner := New(Options{ContentSecurityPolicy: csp, ReuseRequestNonce: true})

	var outerCSP string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers, newR, err := outer.ProcessNoModifyRequest(w, r)
		expect(t, err, nil)

		outerCSP = headers.Get(cspHeader)

		inner.Handler(cspHandler).ServeHTTP(w, newR)
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	handler.ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	// The template (handler body) and both layers must share the same nonce.
	nonce := res.Body.String()
	expect(t, nonce != "", true)

	expected := fmt.Sprintf("default-src 'self' 'nonce-%s'", nonce)
	expect(t, res.Header().Get(cspHeader), expected)
	expect(t, outerCSP, expected)
}

func TestCSPNonceReuseDisabledByDefault(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	outer := New(Options{ContentSecurityPolicy: csp})
	inner := New(Options{ContentSecurityPolicy: csp})

	var outerCSP string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers, newR, err := outer.ProcessNoModifyRequest(w, r)
		expect(t, err, nil)

		outerCSP = headers.Get(cspHeader)

		inner.Handler(cspHandler).ServeHTTP(w, newR)
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	handler.ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	// Default behavior: each layer generates its own nonce.
	nonce := res.Body.String()
	expect(t, res.Header().Get(cspHeader), fmt.Sprintf("default-src 'self' 'nonce-%s'", nonce))
	expect(t, outerCSP != res.Header().Get(cspHeader), true)
}

func TestCSPNonceReuseReportOnlyShared(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	s := New(Options{
		ContentSecurityPolicy:           csp,
		ContentSecurityPolicyReportOnly: csp,
		ReuseRequestNonce:               true,
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	s.Handler(cspHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	// Enforce and report-only policies share the nonce with the template.
	nonce := res.Body.String()
	expected := fmt.Sprintf("default-src 'self' 'nonce-%s'", nonce)
	expect(t, res.Header().Get(cspHeader), expected)
	expect(t, res.Header().Get(cspReportOnlyHeader), expected)
}

func TestCSPNonceReuseInvalidInternalValue(t *testing.T) {
	s := New(Options{ContentSecurityPolicy: "default-src 'self' $NONCE", ReuseRequestNonce: true})

	// Invalid base64 and valid base64 with the wrong decoded length.
	invalidNonces := []string{"not-a-valid-nonce!!!", "AAAA"}

	for _, invalid := range invalidNonces {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)
		ctx := context.WithValue(req.Context(), cspNonceKey, cspNonceValue{nonce: invalid, internal: true})
		req = req.WithContext(ctx)

		res := httptest.NewRecorder()
		err := s.Process(res, req)
		expect(t, errors.Is(err, ErrInvalidCSPNonce), true)

		nonce, err := s.ProcessAndReturnNonce(httptest.NewRecorder(), req)
		expect(t, errors.Is(err, ErrInvalidCSPNonce), true)
		expect(t, nonce, "")
	}
}

func TestCSPNonceReuseIgnoresClientValues(t *testing.T) {
	s := New(Options{ContentSecurityPolicy: "default-src 'self' $NONCE", ReuseRequestNonce: true})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)
	req.Header.Set("X-CSP-Nonce", "client-supplied-nonce")
	// A nonce set through the public testing helper is not internal and must not be reused.
	req = req.WithContext(WithCSPNonce(req.Context(), "client-supplied-nonce"))

	s.Handler(cspHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	nonce := res.Body.String()
	expect(t, nonce != "client-supplied-nonce", true)
	expect(t, res.Header().Get(cspHeader), fmt.Sprintf("default-src 'self' 'nonce-%s'", nonce))
}

func TestCSPNonceReuseRequestsIndependent(t *testing.T) {
	s := New(Options{ContentSecurityPolicy: "default-src 'self' $NONCE", ReuseRequestNonce: true})

	nonces := make(map[string]struct{})

	for i := 0; i < 2; i++ {
		res := httptest.NewRecorder()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

		s.Handler(cspHandler).ServeHTTP(res, req)

		nonces[res.Body.String()] = struct{}{}
	}

	expect(t, len(nonces), 2)
}

func TestProcessAndReturnNonceReuse(t *testing.T) {
	csp := "default-src 'self' $NONCE"
	outer := New(Options{ContentSecurityPolicy: csp})
	inner := New(Options{ContentSecurityPolicy: csp, ReuseRequestNonce: true})

	var returnedNonce string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := inner.ProcessAndReturnNonce(w, r)
		expect(t, err, nil)

		returnedNonce = nonce
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	outer.Handler(handler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)
	expect(t, returnedNonce != "", true)

	// The returned nonce matches the nonce written to the final response.
	expect(t, res.Header().Get(cspHeader), fmt.Sprintf("default-src 'self' 'nonce-%s'", returnedNonce))

	decoded, err := base64.RawStdEncoding.DecodeString(returnedNonce)
	expect(t, err, nil)
	expect(t, len(decoded), cspNonceSize)
}
