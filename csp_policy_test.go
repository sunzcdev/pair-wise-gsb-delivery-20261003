package secure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unrolled/secure/cspbuilder"
)

func TestCSPPolicyBuilderNonce(t *testing.T) {
	enforcePolicy := cspbuilder.NewPolicyBuilder().
		Add(cspbuilder.DefaultSrc, "'self'").
		Add(cspbuilder.ScriptSrc, "'strict-dynamic'", cspbuilder.Nonce).
		Add(cspbuilder.StyleSrc, cspbuilder.Nonce).
		MustBuild()

	reportOnlyPolicy := cspbuilder.NewPolicyBuilder().
		Add(cspbuilder.ScriptSrc, cspbuilder.Nonce).
		MustBuild()

	s := New(Options{
		ContentSecurityPolicy:           enforcePolicy,
		ContentSecurityPolicyReportOnly: reportOnlyPolicy,
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)

	s.Handler(cspHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusOK)

	// The nonce generated for the request is exposed to the handler.
	nonce := res.Body.String()
	if nonce == "" {
		t.Fatal("expected a CSP nonce in the request context")
	}

	enforceHeader := res.Header().Get(cspHeader)
	reportOnlyHeader := res.Header().Get(cspReportOnlyHeader)

	// The nonce only expands in the slots that declared it.
	expectedEnforce := "default-src 'self'; script-src 'strict-dynamic' 'nonce-" + nonce + "'; style-src 'nonce-" + nonce + "'"
	expect(t, enforceHeader, expectedEnforce)
	expect(t, strings.Count(enforceHeader, "'nonce-"), 2)

	expectedReportOnly := "script-src 'nonce-" + nonce + "'"
	expect(t, reportOnlyHeader, expectedReportOnly)
	expect(t, strings.Count(reportOnlyHeader, "'nonce-"), 1)

	// Enforce and report-only policies share the same request nonce.
	if !strings.Contains(reportOnlyHeader, nonce) {
		t.Errorf("report-only policy %q does not share the enforce policy nonce %q", reportOnlyHeader, nonce)
	}

	// Directives that did not declare the nonce must not contain it.
	for _, directive := range strings.Split(enforceHeader, "; ") {
		if strings.HasPrefix(directive, "default-src") && strings.Contains(directive, "nonce-") {
			t.Errorf("nonce expanded in default-src, which did not declare it: %q", enforceHeader)
		}
	}
}
