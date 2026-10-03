package nosurf

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Header values used to exercise the origin decision table.
const (
	selfOriginURL    = "https://example.com"
	allowedOriginURL = "https://api.example.com"
	foreignOriginURL = "https://attacker.lol"
)

var testAllowedOrigins = []string{allowedOriginURL, "http://example.org"}

// originTableCase pins a single cell of strictOriginRules: the exact
// allow/deny reason for a Sec-Fetch-Site / Origin / Referer combination.
type originTableCase struct {
	name    string
	fetch   string // raw Sec-Fetch-Site header value, "" means absent
	origin  string // raw Origin header value, "" means absent
	referer string // raw Referer header value, "" means absent
	dupHdr  string // if set, add a second value line for this header
	want    error
}

func strictOriginTableCases() []originTableCase {
	return []originTableCase{
		// ---- Sec-Fetch-Site: same-origin ----
		{"samesite SFS passes on its own", "same-origin", "", "", "", nil},
		{"samesite SFS with self Origin passes", "same-origin", selfOriginURL, "", "", nil},
		{"samesite SFS with self Referer passes", "same-origin", "", selfOriginURL, "", nil},
		{"samesite SFS with self Origin and self Referer passes", "same-origin", selfOriginURL, selfOriginURL, "", nil},
		{"samesite SFS with foreign Origin contradicts", "same-origin", foreignOriginURL, "", "", ErrContradictoryOrigin},
		{"samesite SFS with foreign Referer contradicts", "same-origin", "", foreignOriginURL, "", ErrContradictoryOrigin},
		{"samesite SFS with foreign Origin and self Referer contradicts", "same-origin", foreignOriginURL, selfOriginURL, "", ErrContradictoryOrigin},
		{"samesite SFS with self Origin and foreign Referer contradicts", "same-origin", selfOriginURL, foreignOriginURL, "", ErrContradictoryOrigin},
		{"samesite SFS with allow-listed foreign Origin contradicts", "same-origin", allowedOriginURL, "", "", ErrContradictoryOrigin},
		{"samesite SFS with allow-listed foreign Referer contradicts", "same-origin", "", allowedOriginURL, "", ErrContradictoryOrigin},
		{"samesite SFS with Origin null contradicts", "same-origin", "null", "", "", ErrNullOrigin},
		{"samesite SFS with malformed Origin contradicts", "same-origin", "://bad", "", "", ErrMalformedOrigin},
		{"samesite SFS with duplicate Origin contradicts", "same-origin", selfOriginURL, "", "Origin", ErrDuplicateOrigin},
		{"samesite SFS with duplicate Referer contradicts", "same-origin", "", selfOriginURL, "Referer", ErrDuplicateReferer},
		{"samesite SFS with malformed Referer contradicts", "same-origin", "", "://bad", "", ErrMalformedReferer},

		// ---- Sec-Fetch-Site: same-site ----
		{"same-site SFS with self Origin passes", "same-site", selfOriginURL, "", "", nil},
		{"same-site SFS with self Referer passes", "same-site", "", selfOriginURL, "", nil},
		{"same-site SFS with allow-listed Origin passes", "same-site", allowedOriginURL, "", "", nil},
		{"same-site SFS with allow-listed Referer passes", "same-site", "", allowedOriginURL, "", nil},
		{"same-site SFS alone lacks any evidence", "same-site", "", "", "", ErrNoReferer},
		{"same-site SFS with foreign Origin fails", "same-site", foreignOriginURL, "", "", ErrBadOrigin},
		{"same-site SFS with foreign Referer fails", "same-site", "", foreignOriginURL, "", ErrBadReferer},
		{"same-site SFS with self Origin and foreign Referer fails on Referer", "same-site", selfOriginURL, foreignOriginURL, "", ErrBadReferer},
		{"same-site SFS with foreign Origin and self Referer fails on Origin", "same-site", foreignOriginURL, selfOriginURL, "", ErrBadOrigin},
		{"same-site SFS with Origin null fails", "same-site", "null", "", "", ErrNullOrigin},
		{"same-site SFS with malformed Origin fails", "same-site", "://bad", "", "", ErrMalformedOrigin},
		{"same-site SFS with duplicate Origin fails", "same-site", allowedOriginURL, "", "Origin", ErrDuplicateOrigin},
		{"same-site SFS with duplicate Referer fails", "same-site", "", allowedOriginURL, "Referer", ErrDuplicateReferer},
		{"same-site SFS with malformed Referer fails", "same-site", "", "://bad", "", ErrMalformedReferer},

		// ---- Sec-Fetch-Site: cross-site ----
		{"cross-site SFS with allow-listed Origin passes", "cross-site", allowedOriginURL, "", "", nil},
		{"cross-site SFS with allow-listed Referer passes", "cross-site", "", allowedOriginURL, "", nil},
		{"cross-site SFS alone is rejected", "cross-site", "", "", "", ErrCrossSite},
		{"cross-site SFS with self Origin contradicts", "cross-site", selfOriginURL, "", "", ErrContradictoryOrigin},
		{"cross-site SFS with self Referer contradicts", "cross-site", "", selfOriginURL, "", ErrContradictoryOrigin},
		{"cross-site SFS with foreign Origin fails", "cross-site", foreignOriginURL, "", "", ErrBadOrigin},
		{"cross-site SFS with foreign Referer fails", "cross-site", "", foreignOriginURL, "", ErrBadReferer},
		{"cross-site SFS with allow-listed Origin and self Referer contradicts", "cross-site", allowedOriginURL, selfOriginURL, "", ErrContradictoryOrigin},
		{"cross-site SFS with self Origin and allow-listed Referer contradicts", "cross-site", selfOriginURL, allowedOriginURL, "", ErrContradictoryOrigin},
		{"cross-site SFS with Origin null fails", "cross-site", "null", "", "", ErrNullOrigin},
		{"cross-site SFS with malformed Origin fails", "cross-site", "://bad", "", "", ErrMalformedOrigin},
		{"cross-site SFS with duplicate Origin fails", "cross-site", allowedOriginURL, "", "Origin", ErrDuplicateOrigin},
		{"cross-site SFS with duplicate Referer fails", "cross-site", "", allowedOriginURL, "Referer", ErrDuplicateReferer},
		{"cross-site SFS with malformed Referer fails", "cross-site", "", "://bad", "", ErrMalformedReferer},

		// ---- Sec-Fetch-Site: none ----
		{"none SFS alone is rejected", "none", "", "", "", ErrCrossSite},
		{"none SFS with self Origin is rejected", "none", selfOriginURL, "", "", ErrCrossSite},
		{"none SFS with allow-listed Origin is rejected", "none", allowedOriginURL, "", "", ErrCrossSite},
		{"none SFS with foreign Origin is rejected", "none", foreignOriginURL, "", "", ErrCrossSite},
		{"none SFS with self Referer is rejected", "none", "", selfOriginURL, "", ErrCrossSite},
		{"none SFS with Origin null is rejected as null", "none", "null", "", "", ErrNullOrigin},
		{"none SFS with duplicate Referer is rejected", "none", "", selfOriginURL, "Referer", ErrDuplicateReferer},

		// ---- Sec-Fetch-Site absent ----
		{"missing SFS with self Origin passes", "", selfOriginURL, "", "", nil},
		{"missing SFS with self Referer passes", "", "", selfOriginURL, "", nil},
		{"missing SFS with self Origin and self Referer passes", "", selfOriginURL, selfOriginURL, "", nil},
		{"missing SFS with allow-listed Origin passes", "", allowedOriginURL, "", "", nil},
		{"missing SFS with allow-listed Referer passes", "", "", allowedOriginURL, "", nil},
		{"missing SFS with foreign Origin fails", "", foreignOriginURL, "", "", ErrBadOrigin},
		{"missing SFS with foreign Referer fails", "", "", foreignOriginURL, "", ErrBadReferer},
		{"missing SFS with foreign Origin and self Referer fails on Origin", "", foreignOriginURL, selfOriginURL, "", ErrBadOrigin},
		{"missing SFS with self Origin and foreign Referer fails on Referer", "", selfOriginURL, foreignOriginURL, "", ErrBadReferer},
		{"missing SFS with no evidence fails", "", "", "", "", ErrNoReferer},
		{"missing SFS with Origin null fails", "", "null", "", "", ErrNullOrigin},
		{"missing SFS with malformed Origin fails", "", "://bad", "", "", ErrMalformedOrigin},
		{"missing SFS with duplicate Origin fails", "", selfOriginURL, "", "Origin", ErrDuplicateOrigin},
		{"missing SFS with duplicate Referer fails", "", "", selfOriginURL, "Referer", ErrDuplicateReferer},
		{"missing SFS with malformed Referer fails", "", "", "://bad", "", ErrMalformedReferer},

		// ---- unknown / repeated Sec-Fetch-Site ----
		{"unknown SFS value is rejected", "bogus", selfOriginURL, selfOriginURL, "", ErrUnknownFetchSite},
		{"unknown SFS value alone is rejected", "bogus", "", "", "", ErrUnknownFetchSite},
		{"duplicate SFS header is rejected", "same-origin", "", "", "Sec-Fetch-Site", ErrDuplicateFetchSite},
		{"comma-folded SFS header is rejected", "same-origin, same-origin", "", "", "", ErrDuplicateFetchSite},
	}
}

func TestStrictOriginDecisionTable(t *testing.T) {
	for _, tc := range strictOriginTableCases() {
		t.Run(tc.name, func(t *testing.T) {
			hand := New(http.HandlerFunc(succHand))
			hand.SetOriginPolicy(OriginPolicyStrict)
			hand.SetIsTLSFunc(func(*http.Request) bool { return true })
			origins, err := StaticOrigins(testAllowedOrigins...)
			if err != nil {
				t.Fatal(err)
			}
			hand.SetIsAllowedOriginFunc(origins)

			req := originTableRequest(tc)
			selfOrigin := hand.selfOriginFor(req)
			got := hand.evaluateStrictOrigin(selfOrigin, req)
			if got != tc.want {
				t.Fatalf("reason = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStrictOriginTableIsExhaustive feeds every possible
// (Sec-Fetch-Site, Origin, Referer) classification through the table and
// asserts the table pins an explicit reason for all of them, i.e. the
// fail-closed default in evaluateStrictOrigin is never reached.
func TestStrictOriginTableIsExhaustive(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	origins, err := StaticOrigins(testAllowedOrigins...)
	if err != nil {
		t.Fatal(err)
	}
	hand.SetIsAllowedOriginFunc(origins)

	req := httptest.NewRequest("POST", selfOriginURL+"/", nil)
	req.Host = "example.com"

	fetchClasses := []fetchSiteClass{fsSameOrigin, fsSameSite, fsCrossSite, fsNone, fsMissing, fsUnknown, fsDuplicate}
	evidenceClasses := []evidenceClass{evMissing, evDuplicate, evNull, evMalformed, evSelf, evAllowed, evForeign}

	for _, fs := range fetchClasses {
		for _, oe := range evidenceClasses {
			for _, re := range evidenceClasses {
				got := hand.lookupStrictOrigin(fs, oe, re)
				if got == errTableMiss {
					t.Errorf("no decision-table row for fs=%d origin=%d referer=%d", fs, oe, re)
				}
			}
		}
	}
}

// TestStrictModeRunsIndependentlyOfTokenCheck verifies that origin checks
// and token checks stay separate steps in strict mode.
func TestStrictModeRunsIndependentlyOfTokenCheck(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetOriginPolicy(OriginPolicyStrict)
	hand.SetFailureHandler(correctReason(t, ErrContradictoryOrigin))

	req, err := http.NewRequest("POST", "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", foreignOriginURL)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Fatalf("status = %d, want %d", writer.Code, FailureCode)
	}
}

// TestCompatModeKeepsLegacyBehavior verifies that the default policy
// keeps trusting Sec-Fetch-Site: same-origin even alongside a foreign
// Origin header (the pre-strict shortcut).
func TestCompatModeKeepsLegacyBehavior(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(correctReason(t, ErrBadToken))

	req, err := http.NewRequest("POST", "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", foreignOriginURL)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Fatalf("status = %d, want %d", writer.Code, FailureCode)
	}
}

// TestStrictModeEndToEnd drives ServeHTTP with a real token and verifies
// that strict origin checks let a fully consistent request through,
// while a same-origin claim paired with a foreign Origin is rejected
// before the token even matters.
func TestStrictModeEndToEnd(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetOriginPolicy(OriginPolicyStrict)
	hand.SetIsTLSFunc(func(*http.Request) bool { return false })
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected failure: %v", Reason(r))
		http.Error(w, "", FailureCode)
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cookie := getRespCookie(resp, CookieName)
	if cookie == nil {
		t.Fatal("missing CSRF cookie")
	}
	finalToken := b64encode(maskToken(b64decode(cookie.Value)))

	req, err := http.NewRequest("POST", server.URL, formBodyR([][]string{
		{FormFieldName, finalToken},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Referer", "http://example.com/")
	req.AddCookie(cookie)

	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func originTableRequest(tc originTableCase) *http.Request {
	req := httptest.NewRequest("POST", selfOriginURL+"/", nil)
	req.Host = "example.com"
	if tc.fetch != "" {
		req.Header.Set("Sec-Fetch-Site", tc.fetch)
	}
	if tc.origin != "" {
		req.Header.Set("Origin", tc.origin)
	}
	if tc.referer != "" {
		req.Header.Set("Referer", tc.referer)
	}
	switch tc.dupHdr {
	case "Sec-Fetch-Site":
		req.Header.Add("Sec-Fetch-Site", "cross-site")
	case "Origin":
		req.Header.Add("Origin", allowedOriginURL)
	case "Referer":
		req.Header.Add("Referer", allowedOriginURL)
	}
	return req
}

func (h *CSRFHandler) selfOriginFor(r *http.Request) *url.URL {
	return h.buildSelfOrigin(r)
}
