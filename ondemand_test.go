package nosurf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// issuedCookie returns the CSRF cookie recorded on the recorder, or nil.
func issuedCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Path 1: a public GET page that never obtains a token must not receive a
// cookie nor a Vary: Cookie header.
func TestOnDemandGetWithoutTokenIssuesNoCookie(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c != nil {
		t.Errorf("expected no CSRF cookie when no token is requested, got %v", c)
	}

	if sContains(rec.Header()["Vary"], "Cookie") {
		t.Errorf("expected no Vary: Cookie header when no token is requested, got %v",
			rec.Header()["Vary"])
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

// A GET page that actually renders a form and obtains a token gets a cookie.
func TestOnDemandGetWithTokenIssuesCookie(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := EnsureToken(w, r)
		if err != nil {
			t.Errorf("EnsureToken returned unexpected error: %v", err)
		}
		if token == "" {
			t.Error("expected a non-empty token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	c := issuedCookie(rec, CookieName)
	if c == nil {
		t.Fatal("expected a CSRF cookie once the token is requested")
	}
	if !sContains(rec.Header()["Vary"], "Cookie") {
		t.Errorf("expected Vary: Cookie header once a token is issued, got %v",
			rec.Header()["Vary"])
	}
}

// Path 2: when a valid cookie already exists, obtaining the token reuses it
// (the cookie value corresponds to the same real token) and does not mint a
// new one.
func TestOnDemandReusesExistingCookie(t *testing.T) {
	existing := b64encode(generateToken())

	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := EnsureToken(w, r)
		if err != nil {
			t.Errorf("EnsureToken returned unexpected error: %v", err)
		}
		// The masked token handed out must correspond to the existing cookie.
		if !VerifyToken(token, existing) {
			t.Error("the issued token does not correspond to the existing cookie")
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	req.AddCookie(&http.Cookie{Name: CookieName, Value: existing})
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c == nil {
		t.Error("expected a Set-Cookie response to (re)issue the reused token")
	} else if c.Value != existing {
		t.Errorf("expected cookie value to be reused (%s), got %s", existing, c.Value)
	}
}

// Obtaining the token several times within one request must always return the
// same masked token and schedule the cookie exactly once.
func TestOnDemandTokenIsConsistentAcrossCalls(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, err := EnsureToken(w, r)
		if err != nil {
			t.Fatalf("first EnsureToken: %v", err)
		}
		second := Token(r)
		third, err := EnsureToken(w, r)
		if err != nil {
			t.Fatalf("third EnsureToken: %v", err)
		}
		if first != second || second != third {
			t.Errorf("tokens differ across calls: %q, %q, %q", first, second, third)
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	count := 0
	for _, c := range cookies {
		if c.Name == CookieName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one Set-Cookie, got %d", count)
	}
}

// Path 3: if the downstream handler commits the response (WriteHeader or
// Write) before obtaining the token, EnsureToken must report the failure
// instead of claiming success for a cookie the browser will never receive.
func TestOnDemandTokenAfterWriteHeaderFails(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("committed"))

		token, err := EnsureToken(w, r)
		if !errors.Is(err, ErrResponseAlreadyCommitted) {
			t.Errorf("expected ErrResponseAlreadyCommitted, got %v", err)
		}
		if token != "" {
			t.Errorf("expected empty token after commit, got %q", token)
		}
		if Token(r) != "" {
			t.Error("Token() must return an empty string after the response is committed")
		}
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c != nil {
		t.Errorf("expected no cookie after an early commit, got %v", c)
	}
}

// A plain body Write (without an explicit WriteHeader) also commits the
// response and must prevent token issuance.
func TestOnDemandTokenAfterWriteFails(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))

		if _, err := EnsureToken(w, r); !errors.Is(err, ErrResponseAlreadyCommitted) {
			t.Errorf("expected ErrResponseAlreadyCommitted after Write, got %v", err)
		}
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c != nil {
		t.Errorf("expected no cookie after an early Write, got %v", c)
	}
}

// Flushing the response commits it too, even when the handler goes through
// http.ResponseController or a writer wrapper.
func TestOnDemandTokenAfterFlushFails(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).Flush()

		if _, err := EnsureToken(w, r); !errors.Is(err, ErrResponseAlreadyCommitted) {
			t.Errorf("expected ErrResponseAlreadyCommitted after Flush, got %v", err)
		}
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c != nil {
		t.Errorf("expected no cookie after an early Flush, got %v", c)
	}
}

// statusWriter is a downstream wrapper around the writer. nosurf tracks commit
// state on its own inner writer, so EnsureToken must still notice the commit.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func TestOnDemandTokenAfterWrappedWriteHeaderFails(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		sw.WriteHeader(http.StatusAccepted)

		if _, err := EnsureToken(sw, r); !errors.Is(err, ErrResponseAlreadyCommitted) {
			t.Errorf("expected ErrResponseAlreadyCommitted with a wrapped writer, got %v", err)
		}
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c != nil {
		t.Errorf("expected no cookie after a wrapped early commit, got %v", c)
	}
}

// Token() (the error-free helper) successfully issues a token in on-demand
// mode when called before the response is committed.
func TestOnDemandTokenHelperIssuesCookie(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Token(r) == "" {
			t.Error("expected Token() to issue a non-empty token")
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	rec := httptest.NewRecorder()
	req := dummyGet()
	hand.ServeHTTP(rec, req)

	if c := issuedCookie(rec, CookieName); c == nil {
		t.Error("expected a cookie when Token() is called before commit")
	}
}

// Exempt unsafe requests skip verification, but on-demand issuance still
// works and regenerates a token when the incoming cookie is malformed.
func TestOnDemandExemptPathIssuesOnRequest(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := EnsureToken(w, r)
		if err != nil {
			t.Errorf("EnsureToken: %v", err)
		}
		if token == "" {
			t.Error("expected a non-empty token on the exempt path")
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)
	hand.ExemptPath("/webhook")

	rec := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "http://dummy.us/webhook", strings.NewReader("a=b"))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "not-valid-base64!!"})
	hand.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected exempt POST to succeed, got %d", rec.Code)
	}
	c := issuedCookie(rec, CookieName)
	if c == nil {
		t.Fatal("expected a fresh cookie to replace the malformed one")
	}
	if len(b64decode(c.Value)) != tokenLength {
		t.Errorf("expected a %d-byte regenerated token, got %d bytes",
			tokenLength, len(b64decode(c.Value)))
	}
}

// Unsafe methods remain fully validated even in on-demand mode: a POST with no
// token must hit the failure handler.
func TestOnDemandUnsafeMethodStillValidated(t *testing.T) {
	failed := false
	hand := New(http.HandlerFunc(succHand))
	hand.SetCookieOnDemand(true)
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failed = true
		if Reason(r) != ErrBadToken {
			t.Errorf("expected ErrBadToken, got %v", Reason(r))
		}
		w.WriteHeader(FailureCode)
	}))

	rec := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "http://dummy.us/", strings.NewReader("a=b"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Referer", "https://example.com")
	hand.ServeHTTP(rec, req)

	if !failed {
		t.Error("expected the failure handler to run for a POST without a token")
	}
	if rec.Code != FailureCode {
		t.Errorf("expected status %d, got %d", FailureCode, rec.Code)
	}
	// The failure handler chose not to request a token, so no cookie is sent.
	if c := issuedCookie(rec, CookieName); c != nil {
		t.Errorf("expected no cookie when the failure handler does not request one, got %v", c)
	}
}

// The failure handler can explicitly decide to issue a token, in which case
// the cookie must actually be delivered (the failure response is still
// uncommitted at that point).
func TestOnDemandFailureHandlerCanIssueToken(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetCookieOnDemand(true)
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := EnsureToken(w, r)
		if err != nil {
			t.Errorf("failure handler EnsureToken: %v", err)
		}
		if token == "" {
			t.Error("expected the failure handler to obtain a token")
		}
		w.WriteHeader(FailureCode)
	}))

	rec := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "http://dummy.us/", strings.NewReader("a=b"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Referer", "https://example.com")
	hand.ServeHTTP(rec, req)

	if rec.Code != FailureCode {
		t.Errorf("expected status %d, got %d", FailureCode, rec.Code)
	}
	if c := issuedCookie(rec, CookieName); c == nil {
		t.Error("expected a cookie when the failure handler requests a token")
	}
}

// A full round trip in on-demand mode: GET obtains a token and cookie, then a
// POST presenting that token must pass.
func TestOnDemandRoundTrip(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if _, err := EnsureToken(w, r); err != nil {
				t.Errorf("EnsureToken: %v", err)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected failure: %v", Reason(r))
		w.WriteHeader(FailureCode)
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cookie := getRespCookie(resp, CookieName)
	if cookie == nil {
		t.Fatal("expected a cookie from the token-issuing GET")
	}

	finalToken := b64encode(maskToken(b64decode(cookie.Value)))
	req, err := http.NewRequest("POST", server.URL, formBodyR([][]string{
		{FormFieldName, finalToken},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Referer", "https://example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	postResp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if postResp.StatusCode != http.StatusOK {
		t.Errorf("expected the POST to succeed, got %d", postResp.StatusCode)
	}
}

// EnsureToken outside of the middleware returns an error rather than panicking.
func TestEnsureTokenWithoutContext(t *testing.T) {
	rec := httptest.NewRecorder()
	req := dummyGet()

	token, err := EnsureToken(rec, req)
	if err == nil {
		t.Error("expected an error when the request never went through the handler")
	}
	if token != "" {
		t.Errorf("expected empty token, got %q", token)
	}
}
