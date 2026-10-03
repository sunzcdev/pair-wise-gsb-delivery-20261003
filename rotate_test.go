package nosurf

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issues a POST request with the given token in the form body
// and the given cookie attached.
func postWithToken(t *testing.T, url string, cookie *http.Cookie, token string) *http.Response {
	t.Helper()

	req, err := http.NewRequest("POST", url, formBodyR([][]string{
		{FormFieldName, token},
	}))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if cookie != nil {
		req.AddCookie(cookie)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Returns all cookies with the given name set on the response, in order.
func getRespCookies(resp *http.Response, name string) []*http.Cookie {
	var cookies []*http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == name {
			cookies = append(cookies, c)
		}
	}
	return cookies
}

func TestRotateTokenInvalidatesOldToken(t *testing.T) {
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rotate" {
			newToken, err := hand.RotateToken(w, r)
			if err != nil {
				t.Errorf("RotateToken failed: %v", err)
			}
			if got := Token(r); got != newToken {
				t.Errorf("Token(r) should return the new token immediately"+
					" after rotation: got %q, want %q", got, newToken)
			}
		}
		fmt.Fprint(w, Token(r))
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	// Get the initial token and cookie.
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldToken := bodyString(t, resp)
	oldCookie := getRespCookie(resp, CookieName)
	if oldCookie == nil {
		t.Fatal("Cookie was not found in the response.")
	}

	// Rotate the token.
	resp = postWithToken(t, server.URL+"/rotate", oldCookie, oldToken)
	if resp.StatusCode != 200 {
		t.Fatalf("The rotation request should have succeeded, got %d", resp.StatusCode)
	}
	newToken := bodyString(t, resp)
	newCookie := getRespCookie(resp, CookieName)
	if newCookie == nil {
		t.Fatal("Cookie was not found in the rotation response.")
	}
	if newCookie.Value == oldCookie.Value {
		t.Error("The cookie value should have changed after rotation.")
	}
	if newToken == oldToken {
		t.Error("The token should have changed after rotation.")
	}

	// The old form (old token) fails on the next request,
	// since the client's cookie has been replaced by the new one.
	resp = postWithToken(t, server.URL, newCookie, oldToken)
	if resp.StatusCode != FailureCode {
		t.Errorf("The old token should fail after rotation, got status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The new form (new token and cookie) passes.
	resp = postWithToken(t, server.URL, newCookie, newToken)
	if resp.StatusCode != 200 {
		t.Errorf("The new token should pass after rotation, got status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRotateTokenMultipleTimesKeepsLastCookie(t *testing.T) {
	var tokens []string
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ {
			token, err := hand.RotateToken(w, r)
			if err != nil {
				t.Errorf("RotateToken failed: %v", err)
			}
			tokens = append(tokens, token)
		}
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	cookies := getRespCookies(resp, CookieName)
	// The middleware sets the initial cookie, then each of the 3 rotations
	// expires the previous cookie and sets a new one: 1 + 3*2 = 7 headers.
	if len(cookies) != 7 {
		t.Fatalf("Expected 7 %s cookies in the response, got %d", CookieName, len(cookies))
	}

	expired := 0
	for _, c := range cookies {
		if c.MaxAge < 0 {
			expired++
		}
	}
	if expired != 3 {
		t.Errorf("Expected 3 expired cookies in the response, got %d", expired)
	}

	// Only the last cookie remains valid and matches the last rotated token.
	last := cookies[len(cookies)-1]
	if !VerifyToken(last.Value, tokens[2]) {
		t.Error("The last cookie should match the last rotated token.")
	}
	if VerifyToken(last.Value, tokens[1]) {
		t.Error("The last cookie should not match an earlier rotated token.")
	}

	// A form with the last token passes...
	resp = postWithToken(t, server.URL, last, tokens[2])
	if resp.StatusCode != 200 {
		t.Errorf("The last rotated token should pass, got status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// ...while a form with an earlier token fails.
	resp = postWithToken(t, server.URL, last, tokens[1])
	if resp.StatusCode != FailureCode {
		t.Errorf("An earlier rotated token should fail, got status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRotateTokenCustomCookieNameAndPath(t *testing.T) {
	const (
		cookieName = "custom_csrf"
		cookiePath = "/app"
	)

	var token string
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		token, err = hand.RotateToken(w, r)
		if err != nil {
			t.Errorf("RotateToken failed: %v", err)
		}
	}))
	hand.SetBaseCookie(http.Cookie{
		Name: cookieName,
		Path: cookiePath,
	})

	server := httptest.NewServer(hand)
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	cookies := getRespCookies(resp, cookieName)
	if len(cookies) == 0 {
		t.Fatalf("No %s cookie was set on the response.", cookieName)
	}

	// Both the live and the expired cookies honor the custom name and path.
	var live *http.Cookie
	expiredFound := false
	for _, c := range cookies {
		if c.Path != cookiePath {
			t.Errorf("Expected cookie path %q, got %q", cookiePath, c.Path)
		}
		if c.MaxAge < 0 {
			expiredFound = true
		} else {
			live = c
		}
	}
	if !expiredFound {
		t.Error("Expected an expired cookie for the token replaced by rotation.")
	}
	if live == nil {
		t.Fatal("No live cookie was set on the response.")
	}

	// The rotated token works with the custom-named cookie.
	resp = postWithToken(t, server.URL, live, token)
	if resp.StatusCode != 200 {
		t.Errorf("The rotated token should pass with the custom cookie, got status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRotateTokenInFailureHandler(t *testing.T) {
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Reason(r) != ErrBadToken {
			t.Errorf("Expected the failure reason %v, got %v", ErrBadToken, Reason(r))
		}

		newToken, err := hand.RotateToken(w, r)
		if err != nil {
			t.Errorf("RotateToken failed: %v", err)
		}

		// The rotation must not rewrite the original check result.
		if Reason(r) != ErrBadToken {
			t.Errorf("Rotation changed the failure reason to %v", Reason(r))
		}

		w.WriteHeader(FailureCode)
		fmt.Fprint(w, newToken)
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	// A request with a bad token fails, but the failure handler
	// rotates the token so the client can retry.
	resp := postWithToken(t, server.URL, nil, "a very wrong value")
	if resp.StatusCode != FailureCode {
		t.Errorf("The request should have failed with %d, got %d", FailureCode, resp.StatusCode)
	}
	newToken := bodyString(t, resp)
	// The middleware sets an initial cookie for this cookiless request,
	// so the rotated cookie is the last one with the name.
	cookies := getRespCookies(resp, CookieName)
	var newCookie *http.Cookie
	if len(cookies) > 0 {
		newCookie = cookies[len(cookies)-1]
	}
	if newCookie == nil {
		t.Fatal("The failure handler should have set a new cookie.")
	}
	if newToken == "" {
		t.Fatal("The failure handler should have returned a new token.")
	}

	// The token rotated in the failure handler works on the next request.
	resp = postWithToken(t, server.URL, newCookie, newToken)
	if resp.StatusCode != 200 {
		t.Errorf("The token rotated in the failure handler should pass, got status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRotateTokenAfterResponseCommitted(t *testing.T) {
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "committed")

		token, err := hand.RotateToken(w, r)
		if !errors.Is(err, ErrResponseCommitted) {
			t.Errorf("Expected ErrResponseCommitted, got %v", err)
		}
		if token != "" {
			t.Errorf("Expected an empty token on a committed response, got %q", token)
		}
	}))

	server := httptest.NewServer(hand)
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestRotateTokenWithoutMiddleware(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))

	req := dummyGet()
	writer := httptest.NewRecorder()

	token, err := hand.RotateToken(writer, req)
	if !errors.Is(err, ErrNoContext) {
		t.Errorf("Expected ErrNoContext, got %v", err)
	}
	if token != "" {
		t.Errorf("Expected an empty token without the middleware, got %q", token)
	}

	// The legacy entry point keeps working and reports failure
	// by returning an empty string.
	if got := hand.RegenerateToken(writer, req); got != "" {
		t.Errorf("Expected an empty token from RegenerateToken without the middleware, got %q", got)
	}
}

func TestRegenerateTokenCompatibility(t *testing.T) {
	var hand *CSRFHandler
	hand = New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := hand.RegenerateToken(w, r)
		if token == "" {
			t.Error("RegenerateToken should return the new token inside the middleware.")
		}
		if token != Token(r) {
			t.Error("RegenerateToken should return the same token as Token(r).")
		}
	}))

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, dummyGet())

	if writer.Code != 200 {
		t.Errorf("Expected status 200, got %d", writer.Code)
	}
	if getRespCookie(writer.Result(), CookieName) == nil {
		t.Error("RegenerateToken should set the token cookie.")
	}
}
