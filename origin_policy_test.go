package nosurf

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Pins down the strictDecisions table: every combination of
// Sec-Fetch-Site classification and Origin/Referer evidence,
// plus the handling of duplicate headers, Origin: null
// and explicitly allowed cross-origin sites.
func TestStrictOriginPolicy(t *testing.T) {
	const host = "example.com"
	allowedOrigins := []string{"https://api.example.com", "http://example.org"}

	testCases := []struct {
		name         string
		insecure     bool
		secFetchSite []string
		origin       []string
		referer      []string
		expectReason error
	}{
		// Sec-Fetch-Site: same-origin
		{
			name:         "same-origin fetch, no other evidence",
			secFetchSite: []string{"same-origin"},
			expectReason: nil,
		},
		{
			name:         "same-origin fetch, matching Origin",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"https://example.com"},
			expectReason: nil,
		},
		{
			name:         "same-origin fetch, matching Referer",
			secFetchSite: []string{"same-origin"},
			referer:      []string{"https://example.com/form"},
			expectReason: nil,
		},
		{
			name:         "same-origin fetch, cross-origin Origin",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"https://attacker.lol"},
			expectReason: ErrBadOrigin,
		},
		{
			name:         "same-origin fetch, allowed Origin contradicts",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"https://api.example.com"},
			expectReason: ErrConflictingOrigin,
		},
		{
			name:         "same-origin fetch, matching Origin but cross-origin Referer",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"https://example.com"},
			referer:      []string{"https://attacker.lol"},
			expectReason: ErrConflictingOrigin,
		},

		// Sec-Fetch-Site: same-site
		{
			name:         "same-site fetch, explicitly allowed Origin",
			secFetchSite: []string{"same-site"},
			origin:       []string{"https://api.example.com"},
			expectReason: nil,
		},
		{
			name:         "same-site fetch, no corroborating evidence",
			secFetchSite: []string{"same-site"},
			expectReason: ErrCrossSiteFetch,
		},
		{
			name:         "same-site fetch, same-origin Origin contradicts",
			secFetchSite: []string{"same-site"},
			origin:       []string{"https://example.com"},
			expectReason: ErrConflictingOrigin,
		},
		{
			name:         "same-site fetch, disallowed Origin",
			secFetchSite: []string{"same-site"},
			origin:       []string{"https://attacker.lol"},
			expectReason: ErrBadOrigin,
		},

		// Sec-Fetch-Site: cross-site
		{
			name:         "cross-site fetch, explicitly allowed Origin",
			secFetchSite: []string{"cross-site"},
			origin:       []string{"http://example.org"},
			expectReason: nil,
		},
		{
			name:         "cross-site fetch, explicitly allowed Referer",
			secFetchSite: []string{"cross-site"},
			referer:      []string{"https://api.example.com/page"},
			expectReason: nil,
		},
		{
			name:         "cross-site fetch, no corroborating evidence",
			secFetchSite: []string{"cross-site"},
			expectReason: ErrCrossSiteFetch,
		},
		{
			name:         "cross-site fetch, same-origin Origin contradicts",
			secFetchSite: []string{"cross-site"},
			origin:       []string{"https://example.com"},
			expectReason: ErrConflictingOrigin,
		},
		{
			name:         "cross-site fetch, disallowed Origin",
			secFetchSite: []string{"cross-site"},
			origin:       []string{"https://attacker.lol"},
			expectReason: ErrBadOrigin,
		},

		// Sec-Fetch-Site: none
		{
			name:         "none fetch, no other evidence",
			secFetchSite: []string{"none"},
			expectReason: nil,
		},
		{
			name:         "none fetch, matching Origin",
			secFetchSite: []string{"none"},
			origin:       []string{"https://example.com"},
			expectReason: nil,
		},
		{
			name:         "none fetch, allowed Origin contradicts",
			secFetchSite: []string{"none"},
			origin:       []string{"https://api.example.com"},
			expectReason: ErrConflictingOrigin,
		},

		// Sec-Fetch-Site missing
		{
			name:         "no fetch metadata, matching Origin",
			origin:       []string{"https://example.com"},
			expectReason: nil,
		},
		{
			name:         "no fetch metadata, explicitly allowed Origin",
			origin:       []string{"https://api.example.com"},
			expectReason: nil,
		},
		{
			name:         "no fetch metadata, explicitly allowed Origin on insecure request",
			insecure:     true,
			origin:       []string{"http://example.org"},
			expectReason: nil,
		},
		{
			name:         "no fetch metadata, matching Referer",
			referer:      []string{"https://example.com/form"},
			expectReason: nil,
		},
		{
			name:         "no fetch metadata, no evidence",
			expectReason: ErrNoReferer,
		},
		{
			name:         "no fetch metadata, cross-origin Referer",
			referer:      []string{"https://attacker.lol"},
			expectReason: ErrBadReferer,
		},
		{
			name:         "no fetch metadata, matching Origin but cross-origin Referer",
			origin:       []string{"https://example.com"},
			referer:      []string{"https://attacker.lol"},
			expectReason: ErrConflictingOrigin,
		},

		// Sec-Fetch-Site with an unknown value
		{
			name:         "unknown fetch metadata, matching Origin",
			secFetchSite: []string{"whatever"},
			origin:       []string{"https://example.com"},
			expectReason: ErrUnknownSecFetchSite,
		},
		{
			name:         "unknown fetch metadata, no evidence",
			secFetchSite: []string{"whatever"},
			expectReason: ErrUnknownSecFetchSite,
		},

		// Duplicate headers are ambiguous evidence
		{
			name:         "duplicate Sec-Fetch-Site headers",
			secFetchSite: []string{"same-origin", "same-origin"},
			expectReason: ErrAmbiguousOriginHeaders,
		},
		{
			name:         "duplicate Origin headers",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"https://example.com", "https://example.com"},
			expectReason: ErrAmbiguousOriginHeaders,
		},
		{
			name:         "duplicate Referer headers",
			secFetchSite: []string{"same-origin"},
			referer:      []string{"https://example.com", "https://example.com"},
			expectReason: ErrAmbiguousOriginHeaders,
		},

		// Origin: null is an opaque origin
		{
			name:         "null Origin, no fetch metadata",
			origin:       []string{"null"},
			expectReason: ErrNullOrigin,
		},
		{
			name:         "null Origin, same-origin fetch",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"null"},
			expectReason: ErrNullOrigin,
		},
		{
			name:         "null Origin, matching Referer does not rescue it",
			origin:       []string{"null"},
			referer:      []string{"https://example.com"},
			expectReason: ErrNullOrigin,
		},

		// Malformed headers
		{
			name:         "malformed Origin",
			origin:       []string{"://bad"},
			expectReason: ErrBadOrigin,
		},
		{
			name:         "malformed Referer",
			referer:      []string{"://bad"},
			expectReason: ErrNoReferer,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hand := New(http.HandlerFunc(succHand))
			hand.SetOriginPolicy(OriginPolicyStrict)
			hand.SetIsTLSFunc(func(_ *http.Request) bool { return !tc.insecure })
			origins, err := StaticOrigins(allowedOrigins...)
			if err != nil {
				t.Fatal(err)
			}
			hand.SetIsAllowedOriginFunc(origins)

			req, err := http.NewRequest("POST", "https://"+host+"/submit", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range tc.secFetchSite {
				req.Header.Add("Sec-Fetch-Site", v)
			}
			for _, v := range tc.origin {
				req.Header.Add("Origin", v)
			}
			for _, v := range tc.referer {
				req.Header.Add("Referer", v)
			}

			reason := hand.ensureSameOrigin(req)
			if reason != tc.expectReason {
				t.Errorf("Expected reason %#v, got %#v", tc.expectReason, reason)
			}
		})
	}
}

// Pins down the compat policy: OriginPolicyCompat (the default)
// preserves the historical behavior of ensureSameOrigin,
// even for inputs that the strict policy rejects.
func TestCompatOriginPolicyPreservesBehavior(t *testing.T) {
	const host = "example.com"

	testCases := []struct {
		name         string
		secFetchSite []string
		origin       []string
		referer      []string
		expectReason error
	}{
		{
			name:         "same-origin fetch short-circuits a contradictory Origin",
			secFetchSite: []string{"same-origin"},
			origin:       []string{"https://attacker.lol"},
			expectReason: nil,
		},
		{
			name:         "unknown Sec-Fetch-Site value falls back to Origin",
			secFetchSite: []string{"whatever"},
			origin:       []string{"https://example.com"},
			expectReason: nil,
		},
		{
			name:         "null Origin falls back to a matching Referer",
			origin:       []string{"null"},
			referer:      []string{"https://example.com"},
			expectReason: nil,
		},
		{
			name:         "null Origin without a Referer fails",
			origin:       []string{"null"},
			expectReason: ErrNoReferer,
		},
		{
			name:         "duplicate Origin headers use the first value",
			origin:       []string{"https://example.com", "https://attacker.lol"},
			expectReason: nil,
		},
		{
			name:         "matching Origin ignores a cross-origin Referer",
			origin:       []string{"https://example.com"},
			referer:      []string{"https://attacker.lol"},
			expectReason: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Note: no SetOriginPolicy call, the default must be compat.
			hand := New(http.HandlerFunc(succHand))
			hand.SetIsTLSFunc(func(_ *http.Request) bool { return true })

			req, err := http.NewRequest("POST", "https://"+host+"/submit", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range tc.secFetchSite {
				req.Header.Add("Sec-Fetch-Site", v)
			}
			for _, v := range tc.origin {
				req.Header.Add("Origin", v)
			}
			for _, v := range tc.referer {
				req.Header.Add("Referer", v)
			}

			reason := hand.ensureSameOrigin(req)
			if reason != tc.expectReason {
				t.Errorf("Expected reason %#v, got %#v", tc.expectReason, reason)
			}
		})
	}
}

// The token check is an independent step: it runs after the origin
// check regardless of the origin policy, and its failure reason
// is not masked by the policy in effect.
func TestStrictOriginPolicyTokenCheckStaysIndependent(t *testing.T) {
	const host = "example.com"

	testCases := []struct {
		name         string
		secFetchSite string
		origin       string
		sendToken    bool
		expectReason error
	}{
		{
			name:         "consistent origin evidence, bad token fails with ErrBadToken",
			secFetchSite: "same-origin",
			sendToken:    false,
			expectReason: ErrBadToken,
		},
		{
			name:         "contradictory origin evidence, valid token still fails the origin check",
			secFetchSite: "same-origin",
			origin:       "https://attacker.lol",
			sendToken:    true,
			expectReason: ErrBadOrigin,
		},
		{
			name:         "consistent origin evidence and a valid token passes",
			secFetchSite: "same-origin",
			sendToken:    true,
			expectReason: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hand := New(http.HandlerFunc(succHand))
			hand.SetOriginPolicy(OriginPolicyStrict)
			hand.SetIsTLSFunc(func(_ *http.Request) bool { return true })
			hand.SetFailureHandler(correctReason(t, tc.expectReason))

			server := httptest.NewServer(hand)
			t.Cleanup(func() { server.Close() })

			// Issue a GET to fetch the token
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			cookie := getRespCookie(resp, CookieName)
			if cookie == nil {
				t.Fatal("Cookie was not found in the response.")
			}

			form := [][]string{{"name", "Jolene"}}
			if tc.sendToken {
				finalToken := b64encode(maskToken(b64decode(cookie.Value)))
				form = append(form, []string{FormFieldName, finalToken})
			}

			req, err = http.NewRequest("POST", server.URL, formBodyR(form))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			req.AddCookie(cookie)

			resp, err = server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}

			if tc.expectReason == nil {
				if resp.StatusCode != http.StatusOK {
					t.Errorf("Expected request to succeed, but it failed with code %d", resp.StatusCode)
				}
			} else if resp.StatusCode != FailureCode {
				t.Errorf("Expected request to fail with status code %d, but the status code was %d",
					FailureCode, resp.StatusCode)
			}
		})
	}
}
