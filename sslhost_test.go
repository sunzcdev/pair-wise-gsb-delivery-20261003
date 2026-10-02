package secure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseSSLHost(t *testing.T) {
	validCases := []struct {
		value string
		host  string
		port  string
	}{
		{"example.com", "example.com", ""},
		{"EXAMPLE.com:443", "EXAMPLE.com", "443"},
		{"[::1]", "[::1]", ""},
		{"[2001:db8::1]:8443", "[2001:db8::1]", "8443"},
		{"[fe80::1%25eth0]:8443", "[fe80::1%25eth0]", "8443"},
		{"[fe80::1%eth0]", "[fe80::1%eth0]", ""},
	}

	for _, tc := range validCases {
		host, port, err := parseSSLHost(tc.value)
		if err != nil {
			t.Errorf("parseSSLHost(%q): unexpected error: %v", tc.value, err)

			continue
		}

		if host != tc.host || port != tc.port {
			t.Errorf("parseSSLHost(%q): got (%q, %q), want (%q, %q)", tc.value, host, port, tc.host, tc.port)
		}
	}

	invalidCases := []string{
		"",
		"example.com/path",
		"example.com?q=1",
		"example.com#fragment",
		"user:pass@example.com",
		"example.com\\evil.com",
		"example.com:abc",
		"example.com:0",
		"example.com:65536",
		"example.com:",
		":443",
		"[::1",
		"[not-an-ip]:443",
		"[%25eth0]:443",
		"[::1]:",
		"[::1]:443:8443",
		"::1",
		"example.com\r",
		"example. com",
	}

	for _, value := range invalidCases {
		if _, _, err := parseSSLHost(value); err == nil {
			t.Errorf("parseSSLHost(%q): expected error, got nil", value)
		}
	}
}

func sslHostFuncRedirectRequest(t *testing.T, options Options, requestURL string) *httptest.ResponseRecorder {
	t.Helper()

	s := New(options)

	res := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatalf("unexpected error building request: %v", err)
	}

	req.Host = exampleHost
	if options.SSLForceHost {
		req.URL.Scheme = httpsSchema
	} else {
		req.URL.Scheme = httpSchema
	}

	s.Handler(myHandler).ServeHTTP(res, req)

	return res
}

func constSSLHostFunc(target string) *SSLHostFunc {
	f := SSLHostFunc(func(string) string { return target })

	return &f
}

func TestSSLHostFuncRedirectPreservesEscapedPathAndQuery(t *testing.T) {
	for _, forceHost := range []bool{false, true} {
		options := Options{
			SSLRedirect:  !forceHost,
			SSLForceHost: forceHost,
			SSLHostFunc:  constSSLHostFunc("secure.example.com:8443"),
		}

		res := sslHostFuncRedirectRequest(t, options, "http://www.example.com/a%2Fb/c%2Fd?x=1%2F2&y=2")

		expect(t, res.Code, http.StatusMovedPermanently)
		expect(t, res.Header().Get("Location"), "https://secure.example.com:8443/a%2Fb/c%2Fd?x=1%2F2&y=2")
	}
}

func TestSSLHostFuncRedirectIPv6AndEmptyQuery(t *testing.T) {
	options := Options{
		SSLRedirect: true,
		SSLHostFunc: constSSLHostFunc("[2001:db8::1]:8443"),
	}

	res := sslHostFuncRedirectRequest(t, options, "http://www.example.com/foo?")

	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://[2001:db8::1]:8443/foo?")
}

func TestSSLHostFuncEmptyResultKeepsLegacyFallback(t *testing.T) {
	f := SSLHostFunc(func(string) string { return "" })

	s := New(Options{
		SSLRedirect: true,
		SSLHost:     "secure.example.com",
		SSLHostFunc: &f,
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo?a=b", nil)
	req.Host = exampleHost
	req.URL.Scheme = httpSchema

	s.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://secure.example.com/foo?a=b")
}

func TestSSLHostFuncEmptyResultFallsBackToRequestHost(t *testing.T) {
	f := SSLHostFunc(func(string) string { return "" })

	s := New(Options{
		SSLRedirect: true,
		SSLHostFunc: &f,
	})

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)
	req.Host = exampleHost
	req.URL.Scheme = httpSchema

	s.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://www.example.com/foo")
}

func TestSSLHostFuncAllowedHosts(t *testing.T) {
	allowed := Options{
		SSLRedirect:     true,
		AllowedSSLHosts: []string{"secure.example.com"},
		AllowedSSLPorts: []int{8443},
		SSLHostFunc:     constSSLHostFunc("SECURE.example.com:8443"),
	}

	res := sslHostFuncRedirectRequest(t, allowed, "http://www.example.com/foo")
	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://SECURE.example.com:8443/foo")

	deniedHost := Options{
		SSLRedirect:     true,
		AllowedSSLHosts: []string{"secure.example.com"},
		SSLHostFunc:     constSSLHostFunc("evil.example.com"),
	}

	res = sslHostFuncRedirectRequest(t, deniedHost, "http://www.example.com/foo")
	expect(t, res.Code, http.StatusInternalServerError)
	expect(t, res.Header().Get("Location"), "")

	deniedPort := Options{
		SSLRedirect:     true,
		AllowedSSLPorts: []int{443},
		SSLHostFunc:     constSSLHostFunc("secure.example.com:8443"),
	}

	res = sslHostFuncRedirectRequest(t, deniedPort, "http://www.example.com/foo")
	expect(t, res.Code, http.StatusInternalServerError)
	expect(t, res.Header().Get("Location"), "")

	noPortIsAllowed := Options{
		SSLRedirect:     true,
		AllowedSSLPorts: []int{443},
		SSLHostFunc:     constSSLHostFunc("secure.example.com"),
	}

	res = sslHostFuncRedirectRequest(t, noPortIsAllowed, "http://www.example.com/foo")
	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://secure.example.com/foo")
}

func TestSSLHostFuncAllowedHostsIPv6(t *testing.T) {
	withBrackets := Options{
		SSLRedirect:     true,
		AllowedSSLHosts: []string{"[2001:db8::1]"},
		SSLHostFunc:     constSSLHostFunc("[2001:DB8:0:0::1]:8443"),
	}

	res := sslHostFuncRedirectRequest(t, withBrackets, "http://www.example.com/foo")
	expect(t, res.Code, http.StatusMovedPermanently)
	expect(t, res.Header().Get("Location"), "https://[2001:DB8:0:0::1]:8443/foo")

	denied := Options{
		SSLRedirect:     true,
		AllowedSSLHosts: []string{"[2001:db8::2]"},
		SSLHostFunc:     constSSLHostFunc("[2001:db8::1]:8443"),
	}

	res = sslHostFuncRedirectRequest(t, denied, "http://www.example.com/foo")
	expect(t, res.Code, http.StatusInternalServerError)
}

func TestSSLHostFuncInvalidTargetsBothPaths(t *testing.T) {
	invalidTargets := []string{
		"secure.example.com/evil",
		"user:pass@secure.example.com",
		"[::1",
		"secure.example.com:99999",
		"secure.example.com\r\nSet-Cookie: a=b",
	}

	for _, target := range invalidTargets {
		for _, forceHost := range []bool{false, true} {
			options := Options{
				SSLRedirect:  !forceHost,
				SSLForceHost: forceHost,
				SSLHostFunc:  constSSLHostFunc(target),
			}

			res := sslHostFuncRedirectRequest(t, options, "http://www.example.com/foo")

			if res.Code != http.StatusInternalServerError {
				t.Errorf("target=%q forceHost=%v: expected 500, got %d (Location=%q)",
					target, forceHost, res.Code, res.Header().Get("Location"))
			}

			if location := res.Header().Get("Location"); location != "" {
				t.Errorf("target=%q forceHost=%v: expected no Location, got %q", target, forceHost, location)
			}
		}
	}
}

func TestSSLHostFuncCustomBadSSLHostHandler(t *testing.T) {
	s := New(Options{
		SSLRedirect: true,
		SSLHostFunc: constSSLHostFunc("secure.example.com/evil"),
	})

	badHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "BadSSLHost", http.StatusForbidden)
	})
	s.SetBadSSLHostHandler(badHandler)

	res := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/foo", nil)
	req.Host = exampleHost
	req.URL.Scheme = httpSchema

	s.Handler(myHandler).ServeHTTP(res, req)

	expect(t, res.Code, http.StatusForbidden)
	expect(t, res.Body.String(), "BadSSLHost\n")
}

func TestSSLRedirectDoesNotMutateRequest(t *testing.T) {
	for _, forceHost := range []bool{false, true} {
		f := SSLHostFunc(func(string) string { return "secure.example.com:8443" })

		s := New(Options{
			SSLRedirect:  !forceHost,
			SSLForceHost: forceHost,
			SSLHostFunc:  &f,
		})

		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://www.example.com/a%2Fb?x=1%2F2", nil)
		req.URL.Scheme = httpSchema
		if forceHost {
			req.URL.Scheme = httpsSchema
		}

		originalURL := req.URL.String()
		originalHost := req.Host

		s.Handler(myHandler).ServeHTTP(httptest.NewRecorder(), req)

		expect(t, req.URL.String(), originalURL)
		expect(t, req.Host, originalHost)
	}
}
