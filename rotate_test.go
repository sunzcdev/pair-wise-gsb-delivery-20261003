package nosurf

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Counts the pending Set-Cookie headers for a cookie with the given name
func countSetCookies(header http.Header, name string) int {
	count := 0
	for _, line := range header["Set-Cookie"] {
		if strings.HasPrefix(line, name+"=") {
			count++
		}
	}
	return count
}

// A full round trip: after a rotation the old form token must fail
// and the new one must pass on the following requests.
func TestRegenerateTokenRotates(t *testing.T) {
	var hand *CSRFHandler
	var rotatedToken string
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			token, err := hand.RegenerateToken(w, r)
			if err != nil {
				t.Errorf("RegenerateToken failed: %v", err)
			}
			if token != Token(r) {
				t.Errorf("Token(r) should return the new token right after the rotation")
			}
			rotatedToken = token
		}
		w.WriteHeader(200)
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	// Issue the first request to get the initial token
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	cookie1 := getRespCookie(resp, CookieName)
	if cookie1 == nil {
		t.Fatal("Cookie was not found in the response.")
	}
	oldFormToken := b64encode(maskToken(b64decode(cookie1.Value)))

	// POST with the valid token, the handler rotates the token
	req, err := http.NewRequest("POST", server.URL, formBodyR([][]string{
		{FormFieldName, oldFormToken},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Add("Referer", "https://example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie1)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("The request should have succeeded, but it didn't. Instead, the code was %d",
			resp.StatusCode)
	}

	cookie2 := getRespCookie(resp, CookieName)
	if cookie2 == nil {
		t.Fatal("The rotated cookie was not found in the response.")
	}
	if cookie2.Value == cookie1.Value {
		t.Error("The cookie value should have changed after the rotation.")
	}
	if !VerifyToken(rotatedToken, cookie2.Value) {
		t.Error("The token returned by RegenerateToken should match the new cookie.")
	}

	// An old form (old token) submitted with the new cookie fails
	req, err = http.NewRequest("POST", server.URL, formBodyR([][]string{
		{FormFieldName, oldFormToken},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Add("Referer", "https://example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie2)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != FailureCode {
		t.Errorf("The old token should have failed after the rotation,"+
			" but the response code was %d", resp.StatusCode)
	}

	// A new form (new token) submitted with the new cookie passes
	newFormToken := b64encode(maskToken(b64decode(cookie2.Value)))
	req, err = http.NewRequest("POST", server.URL, formBodyR([][]string{
		{FormFieldName, newFormToken},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Add("Referer", "https://example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie2)

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("The new token should have passed after the rotation,"+
			" but the response code was %d", resp.StatusCode)
	}
}

// Multiple rotations within a single response
// must result in a single cookie carrying the last token.
func TestMultipleRotationsKeepLastCookie(t *testing.T) {
	var hand *CSRFHandler
	var tokens []string
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ {
			token, err := hand.RegenerateToken(w, r)
			if err != nil {
				t.Errorf("RegenerateToken failed: %v", err)
			}
			tokens = append(tokens, token)
		}
		if Token(r) != tokens[len(tokens)-1] {
			t.Errorf("Token(r) should return the last rotated token")
		}
		w.WriteHeader(200)
	}))

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, dummyGet())

	if count := countSetCookies(writer.Header(), CookieName); count != 1 {
		t.Fatalf("Expected exactly one %s cookie, got %d", CookieName, count)
	}

	resp := &http.Response{Header: writer.Header()}
	cookie := getRespCookie(resp, CookieName)
	if cookie == nil {
		t.Fatal("Cookie was not found in the response.")
	}

	if !VerifyToken(tokens[len(tokens)-1], cookie.Value) {
		t.Error("The cookie should carry the last rotated token.")
	}
	for _, token := range tokens[:len(tokens)-1] {
		if VerifyToken(token, cookie.Value) {
			t.Error("The cookie should not carry a superseded token.")
		}
	}
}

// A rotation in the failure handler must not change
// the check result of the request being served,
// but the new cookie must still be sent.
func TestRotationInFailureHandlerKeepsCheckResult(t *testing.T) {
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("The success handler should not have been called")
	}))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Reason(r) != ErrBadToken {
			t.Errorf("Expected the reason %v, got %v", ErrBadToken, Reason(r))
		}
		if _, err := hand.RegenerateToken(w, r); err != nil {
			t.Errorf("Rotation in the failure handler failed: %v", err)
		}
		if Reason(r) != ErrBadToken {
			t.Errorf("The rotation changed the check result: %v", Reason(r))
		}
		http.Error(w, "", FailureCode)
	}))

	req, err := http.NewRequest("POST", "/", formBodyR([][]string{
		{FormFieldName, "$#%^&"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Add("Referer", "https://example.com")
	writer := httptest.NewRecorder()

	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("The check should've failed with the code %d, but instead, it"+
			" returned code %d", FailureCode, writer.Code)
	}

	if count := countSetCookies(writer.Header(), CookieName); count != 1 {
		t.Fatalf("Expected exactly one %s cookie, got %d", CookieName, count)
	}
	resp := &http.Response{Header: writer.Header()}
	if cookie := getRespCookie(resp, CookieName); cookie == nil {
		t.Error("The rotated cookie was not found in the response.")
	}
}

// The rotated cookie must respect the custom name and path
// of the base cookie.
func TestRegenerateTokenCustomCookie(t *testing.T) {
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := hand.RegenerateToken(w, r); err != nil {
			t.Errorf("RegenerateToken failed: %v", err)
		}
		w.WriteHeader(200)
	}))
	hand.SetBaseCookie(http.Cookie{
		Name: "custom_csrf",
		Path: "/accounts",
	})

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, dummyGet())

	resp := &http.Response{Header: writer.Header()}
	cookie := getRespCookie(resp, "custom_csrf")
	if cookie == nil {
		t.Fatal("No cookie with the custom name was found in the response.")
	}
	if cookie.Path != "/accounts" {
		t.Errorf("Expected the cookie path %q, got %q", "/accounts", cookie.Path)
	}
	if c := getRespCookie(resp, CookieName); c != nil {
		t.Errorf("A cookie with the default name %s should not have been set.", CookieName)
	}
	if count := countSetCookies(writer.Header(), "custom_csrf"); count != 1 {
		t.Errorf("Expected exactly one custom_csrf cookie, got %d", count)
	}
}

// A rotation attempted after the response has been committed
// must report ErrResponseCommitted and change nothing.
func TestRegenerateTokenAfterResponseCommitted(t *testing.T) {
	var hand *CSRFHandler
	var tokenBefore string
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenBefore = Token(r)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("body"))

		token, err := hand.RegenerateToken(w, r)
		if err != ErrResponseCommitted {
			t.Errorf("Expected ErrResponseCommitted, got %v", err)
		}
		if token != "" {
			t.Errorf("Expected an empty token on failure, got %q", token)
		}
		if Token(r) != tokenBefore {
			t.Errorf("The token should not have changed after a failed rotation")
		}
	}))

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, dummyGet())

	// The cookie queued before the response was committed
	// is still the only one and still carries the old token.
	if count := countSetCookies(writer.Header(), CookieName); count != 1 {
		t.Fatalf("Expected exactly one %s cookie, got %d", CookieName, count)
	}
	resp := &http.Response{Header: writer.Header()}
	cookie := getRespCookie(resp, CookieName)
	if cookie == nil {
		t.Fatal("Cookie was not found in the response.")
	}
	if !VerifyToken(tokenBefore, cookie.Value) {
		t.Error("The cookie should still carry the token from before the failed rotation.")
	}
}

// A rotation attempted on a request that has not passed
// through CSRFHandler must report ErrNoContext.
func TestRegenerateTokenWithoutMiddleware(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))

	req := dummyGet()
	writer := httptest.NewRecorder()

	token, err := hand.RegenerateToken(writer, req)
	if err != ErrNoContext {
		t.Errorf("Expected ErrNoContext, got %v", err)
	}
	if token != "" {
		t.Errorf("Expected an empty token on failure, got %q", token)
	}
	if Token(req) != "" {
		t.Error("No token should be available for a request outside the middleware.")
	}
}
