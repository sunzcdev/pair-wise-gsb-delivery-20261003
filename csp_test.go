package secure

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unrolled/secure/cspbuilder"
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

func TestCSPPolicyBuilderSharedNonce(t *testing.T) {
	enforcedPolicy, err := (&cspbuilder.PolicyBuilder{}).
		Add(cspbuilder.DefaultSrc, "'self'").
		Add(cspbuilder.ScriptSrc, "'self'", cspbuilder.Nonce).
		Add(cspbuilder.StyleSrc, cspbuilder.Nonce).
		Build()
	expect(t, err, nil)

	reportOnlyPolicy, err := (&cspbuilder.PolicyBuilder{}).
		Add(cspbuilder.ScriptSrc, "'self'", cspbuilder.Nonce).
		Add(cspbuilder.StyleSrc, "'self'").
		Build()
	expect(t, err, nil)

	s := New(Options{
		ContentSecurityPolicy:           enforcedPolicy,
		ContentSecurityPolicyReportOnly: reportOnlyPolicy,
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	s.Handler(cspHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	// The nonce generated for the request is shared by both policies.
	nonce := res.Body.String()
	_, decodeErr := base64.RawStdEncoding.DecodeString(nonce)
	expect(t, decodeErr, nil)

	enforcedHeader := res.Header().Get(cspHeader)
	expect(t, enforcedHeader, fmt.Sprintf("default-src 'self'; script-src 'self' 'nonce-%[1]s'; style-src 'nonce-%[1]s'", nonce))

	reportOnlyHeader := res.Header().Get(cspReportOnlyHeader)
	expect(t, reportOnlyHeader, fmt.Sprintf("script-src 'self' 'nonce-%[1]s'; style-src 'self'", nonce))

	// The nonce only expands in the slots that declared it.
	expect(t, strings.Count(enforcedHeader, "'nonce-"), 2)
	expect(t, strings.Count(reportOnlyHeader, "'nonce-"), 1)
	expect(t, strings.Contains(enforcedHeader, "default-src 'nonce-"), false)
	expect(t, strings.Contains(reportOnlyHeader, "style-src 'nonce-"), false)
}
