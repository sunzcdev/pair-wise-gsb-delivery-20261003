package nosurf

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recorderCookies returns the cookies recorded on the ResponseRecorder.
func recorderCookies(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	cookies := make(map[string]*http.Cookie)
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c
	}
	return cookies
}

// Path 1: a safe request that never obtains a token must not get a cookie
// nor a Vary: Cookie header.
func TestOnDemandSafeRequestWithoutTokenSetsNoCookie(t *testing.T) {
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	req := dummyGet()
	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if c := recorderCookies(rec)[CookieName]; c != nil {
		t.Errorf("expected no CSRF cookie, got %v", c)
	}
	if sContains(rec.Header().Values("Vary"), "Cookie") {
		t.Errorf("expected no Vary: Cookie header, got %v", rec.Header().Values("Vary"))
	}
}

// Obtaining the token issues a fresh cookie and marks the response as varying
// on cookies; repeated calls return the identical token.
func TestOnDemandObtainingTokenIssuesCookie(t *testing.T) {
	var first, second string
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		first, err = GetToken(r)
		if err != nil {
			t.Fatalf("unexpected error getting token: %v", err)
		}
		second = Token(r)
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	req := dummyGet()
	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if first == "" {
		t.Fatal("expected a non-empty token")
	}
	if first != second {
		t.Errorf("token changed between calls: %q != %q", first, second)
	}

	cookie := recorderCookies(rec)[CookieName]
	if cookie == nil {
		t.Fatal("expected a CSRF cookie to be set after obtaining the token")
	}
	if !VerifyToken(cookie.Value, first) {
		t.Errorf("issued token %q does not verify against cookie %q", first, cookie.Value)
	}
	if !sContains(rec.Header().Values("Vary"), "Cookie") {
		t.Errorf("expected Vary: Cookie header, got %v", rec.Header().Values("Vary"))
	}
}

// Path 2: an existing valid cookie is reused (same token value), and the
// cookie is still scheduled so the browser keeps it.
func TestOnDemandReusesExistingCookie(t *testing.T) {
	realToken := generateToken()
	existing := &http.Cookie{Name: CookieName, Value: b64encode(realToken)}

	var issued string
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		issued, err = GetToken(r)
		if err != nil {
			t.Fatalf("unexpected error getting token: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	hand.SetCookieOnDemand(true)

	req := dummyGet()
	req.AddCookie(existing)
	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	cookie := recorderCookies(rec)[CookieName]
	if cookie == nil {
		t.Fatal("expected the existing CSRF cookie to be scheduled again")
	}
	if cookie.Value != existing.Value {
		t.Errorf("expected existing cookie value to be reused: %q != %q",
			existing.Value, cookie.Value)
	}
	if !VerifyToken(cookie.Value, issued) {
		t.Errorf("issued token %q does not verify against reused cookie", issued)
	}
}

// An invalid-length cookie value results in a fresh token rather than reuse.
func TestOnDemandRegeneratesInvalidCookie(t *testing.T) {
	existing := &http.Cookie{Name: CookieName, Value: b64encode([]byte("too-short"))}

	var issued string
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		issued, err = GetToken(r)
		if err != nil {
			t.Fatalf("unexpected error getting token: %v", err)
		}
	}))
	hand.SetCookieOnDemand(true)

	req := dummyGet()
	req.AddCookie(existing)
	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	cookie := recorderCookies(rec)[CookieName]
	if cookie == nil {
		t.Fatal("expected a freshly generated CSRF cookie")
	}
	if cookie.Value == existing.Value {
		t.Error("expected the invalid cookie value to be replaced")
	}
	if !VerifyToken(cookie.Value, issued) {
		t.Errorf("issued token %q does not verify against new cookie", issued)
	}
}

// Path 3: obtaining the token after WriteHeader() must fail explicitly and
// must not set a cookie the browser would never receive.
func TestOnDemandTokenAfterWriteHeaderFails(t *testing.T) {
	var gotErr error
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, gotErr = GetToken(r)
	}))
	hand.SetCookieOnDemand(true)

	req := dummyGet()
	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if !errors.Is(gotErr, ErrResponseAlreadyCommitted) {
		t.Fatalf("expected ErrResponseAlreadyCommitted, got %v", gotErr)
	}
	if c := recorderCookies(rec)[CookieName]; c != nil {
		t.Errorf("expected no CSRF cookie after premature commit, got %v", c)
	}
	if sContains(rec.Header().Values("Vary"), "Cookie") {
		t.Errorf("expected no Vary: Cookie header after premature commit")
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("expected handler-written status %d, got %d", http.StatusAccepted, rec.Code)
	}
}

// Obtaining the token after a body Write (which implicitly commits headers)
// must fail as well.
func TestOnDemandTokenAfterWriteFails(t *testing.T) {
	var gotErr error
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
		_, gotErr = GetToken(r)
	}))
	hand.SetCookieOnDemand(true)

	req := dummyGet()
	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if !errors.Is(gotErr, ErrResponseAlreadyCommitted) {
		t.Fatalf("expected ErrResponseAlreadyCommitted, got %v", gotErr)
	}
	if c := recorderCookies(rec)[CookieName]; c != nil {
		t.Errorf("expected no CSRF cookie after implicit commit, got %v", c)
	}
}

// A failure handler can explicitly decide to issue a token: it must succeed
// when headers have not been written yet and a cookie is delivered.
func TestOnDemandFailureHandlerCanIssueToken(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Reason(r) != ErrBadToken {
			t.Errorf("expected failure reason %v, got %v", ErrBadToken, Reason(r))
		}
		token, err := GetToken(r)
		if err != nil {
			t.Fatalf("failure handler could not issue token: %v", err)
		}
		if token == "" {
			t.Fatal("failure handler got an empty token")
		}
		http.Error(w, "bad token", FailureCode)
	}))
	hand.SetCookieOnDemand(true)

	req, err := http.NewRequest("POST", "https://dum.my/", formBodyR([][]string{
		{FormFieldName, "wrong"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dum.my"
	req.Header.Set("Referer", "https://dum.my/")

	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if rec.Code != FailureCode {
		t.Errorf("expected failure status %d, got %d", FailureCode, rec.Code)
	}
	if c := recorderCookies(rec)[CookieName]; c == nil {
		t.Error("expected the failure handler to issue a CSRF cookie")
	}
}

// A failure handler that commits first gets an explicit error instead of a
// phantom token.
func TestOnDemandFailureHandlerAfterCommitCannotIssueToken(t *testing.T) {
	var gotErr error
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, gotErr = GetToken(r)
	}))
	hand.SetCookieOnDemand(true)

	req, err := http.NewRequest("POST", "https://dum.my/", formBodyR([][]string{
		{FormFieldName, "wrong"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dum.my"
	req.Header.Set("Referer", "https://dum.my/")

	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if !errors.Is(gotErr, ErrResponseAlreadyCommitted) {
		t.Fatalf("expected ErrResponseAlreadyCommitted, got %v", gotErr)
	}
	if c := recorderCookies(rec)[CookieName]; c != nil {
		t.Errorf("expected no CSRF cookie, got %v", c)
	}
}

// Unsafe methods remain fully validated in on-demand mode.
func TestOnDemandUnsafeMethodIsValidated(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", FailureCode)
	}))
	hand.SetCookieOnDemand(true)

	req, err := http.NewRequest("POST", "https://dum.my/", formBodyR([][]string{
		{"name", "Jolene"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dum.my"
	req.Header.Set("Referer", "https://dum.my/")

	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if rec.Code != FailureCode {
		t.Errorf("unsafe request without a token should fail with %d, got %d",
			FailureCode, rec.Code)
	}
}

// A valid unsafe request in on-demand mode passes end-to-end without the
// success handler needing a token.
func TestOnDemandValidUnsafeRequestPasses(t *testing.T) {
	realToken := generateToken()
	cookie := &http.Cookie{Name: CookieName, Value: b64encode(realToken)}
	finalToken := b64encode(maskToken(realToken))

	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected failure: %v", Reason(r))
		http.Error(w, "fail", FailureCode)
	}))
	hand.SetCookieOnDemand(true)

	req, err := http.NewRequest("POST", "https://dum.my/", formBodyR([][]string{
		{FormFieldName, finalToken},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "dum.my"
	req.Header.Set("Referer", "https://dum.my/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	rec := httptest.NewRecorder()
	hand.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected valid request to pass with %d, got %d",
			http.StatusOK, rec.Code)
	}
	// Token never obtained by the success handler: no cookie should be re-sent.
	if c := recorderCookies(rec)[CookieName]; c != nil {
		t.Errorf("expected no CSRF cookie when token is unused, got %v", c)
	}
}

func TestGetTokenWithoutHandlerContext(t *testing.T) {
	if _, err := GetToken(dummyGet()); !errors.Is(err, ErrContextNotAvailable) {
		t.Errorf("expected ErrContextNotAvailable, got %v", err)
	}
}
