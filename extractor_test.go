package nosurf

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body that counts how many times it has been read from,
// used to prove that the CSRF check does not touch the request body.
type countingBody struct {
	io.Reader
	reads int
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads++
	return c.Reader.Read(p)
}

func (c *countingBody) Close() error { return nil }

// Builds an authenticated POST request carrying a valid token cookie
// for the given real (unmasked) token.
func authedRequest(t *testing.T, realToken []byte, body io.Reader) *http.Request {
	req, err := http.NewRequest("POST", "https://example.com/", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", "https://example.com")
	req.AddCookie(&http.Cookie{Name: CookieName, Value: b64encode(realToken)})
	return req
}

func multipartBody(t *testing.T, fields [][]string) (body, contentType string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), w.FormDataContentType()
}

// A JSON API can supply the token from an application-defined location
// by configuring a custom extractor chain.
func TestCustomExtractor(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(HeaderTokenExtractor("X-API-Token"))

	realToken := generateToken()
	sentToken := b64encode(maskToken(realToken))

	// Token in the custom location passes.
	req := authedRequest(t, realToken, nil)
	req.Header.Set("X-API-Token", sentToken)
	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("Request with a token in the custom header should have passed,"+
			" got code %d", writer.Code)
	}

	// The default header is no longer consulted.
	hand.SetFailureHandler(correctReason(t, ErrBadToken))
	req = authedRequest(t, realToken, nil)
	req.Header.Set(HeaderName, sentToken)
	writer = httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Request with a token only in the default header should have failed,"+
			" got code %d", writer.Code)
	}
}

// In header-only mode, an upload request passes the CSRF check
// without the body being parsed or read, and the downstream handler
// receives the body intact.
func TestHeaderOnlyExtractorLeavesBodyUntouched(t *testing.T) {
	// The multipart body carries a wrong token on purpose:
	// it must never be consulted.
	body, contentType := multipartBody(t, [][]string{
		{FormFieldName, "a-wrong-token-that-must-not-be-read"},
		{"file", "payload"},
	})
	counting := &countingBody{Reader: strings.NewReader(body)}

	var downstreamBody string
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if counting.reads != 0 {
			t.Errorf("CSRF check read the request body %d times", counting.reads)
		}
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		downstreamBody = string(b)
		w.WriteHeader(200)
	}))
	hand.SetTokenExtractors(HeaderTokenExtractor(HeaderName))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Test failed. Reason: %v", Reason(r))
	}))

	realToken := generateToken()
	req := authedRequest(t, realToken, counting)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(HeaderName, b64encode(maskToken(realToken)))

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("The request should have succeeded, got code %d", writer.Code)
	}
	if downstreamBody != body {
		t.Errorf("The downstream handler received a different body than sent.")
	}
}

// A failing check short-circuits before the body is read,
// and the failure handler still sees the untouched body.
func TestFailedCheckDoesNotTouchBody(t *testing.T) {
	body, contentType := multipartBody(t, [][]string{
		{"file", "payload"},
	})
	counting := &countingBody{Reader: strings.NewReader(body)}

	successCalled := false
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		successCalled = true
	}))
	hand.SetTokenExtractors(HeaderTokenExtractor(HeaderName))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Reason(r) != ErrBadToken {
			t.Errorf("Expected failure reason %#v, got %#v", ErrBadToken, Reason(r))
		}
		if counting.reads != 0 {
			t.Errorf("CSRF check read the request body %d times", counting.reads)
		}
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if string(b) != body {
			t.Errorf("The failure handler received a different body than sent.")
		}
		http.Error(w, "", FailureCode)
	}))

	realToken := generateToken()
	req := authedRequest(t, realToken, counting)
	req.Header.Set("Content-Type", contentType)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if successCalled {
		t.Error("The success handler was called for a failed CSRF check.")
	}
	if writer.Code != FailureCode {
		t.Errorf("The check should've failed with the code %d, got %d",
			FailureCode, writer.Code)
	}
}

// Once an extractor reports invalid input, the check fails closed:
// there is no fallback to another extractor with a valid token.
func TestInvalidExtractorValuePreventsFallback(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(correctReason(t, ErrInvalidToken))

	realToken := generateToken()
	sentToken := b64encode(maskToken(realToken))

	// The form carries a valid token, but the header value is malformed.
	req := authedRequest(t, realToken, formBodyR([][]string{
		{FormFieldName, sentToken},
	}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(HeaderName, "!!!not-base64!!!")

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("The check should've failed with the code %d, got %d",
			FailureCode, writer.Code)
	}
}

// A malformed form token is reported as invalid input,
// not as a mere token mismatch.
func TestMalformedFormTokenIsInvalid(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(correctReason(t, ErrInvalidToken))

	realToken := generateToken()
	req := authedRequest(t, realToken, formBodyR([][]string{
		{FormFieldName, "$#%^&"},
	}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("The check should've failed with the code %d, got %d",
			FailureCode, writer.Code)
	}
}

// Errors returned by custom extractors become the failure reason.
func TestCustomExtractorErrorPropagates(t *testing.T) {
	errCustom := errors.New("bad token format")

	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(func(r *http.Request) (string, bool, error) {
		return "", false, errCustom
	})
	hand.SetFailureHandler(correctReason(t, errCustom))

	realToken := generateToken()
	req := authedRequest(t, realToken, nil)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("The check should've failed with the code %d, got %d",
			FailureCode, writer.Code)
	}
}

// With ConflictError, a request providing tokens via multiple sources
// fails even if all of them are valid.
func TestConflictErrorPolicy(t *testing.T) {
	realToken := generateToken()
	sentToken := b64encode(maskToken(realToken))

	// Tokens in both the header and the form: rejected.
	hand := New(http.HandlerFunc(succHand))
	hand.SetConflictPolicy(ConflictError)
	hand.SetFailureHandler(correctReason(t, ErrConflictingTokens))

	req := authedRequest(t, realToken, formBodyR([][]string{
		{FormFieldName, sentToken},
	}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(HeaderName, sentToken)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Conflicting tokens should've failed with the code %d, got %d",
			FailureCode, writer.Code)
	}

	// A single source still passes under ConflictError.
	hand = New(http.HandlerFunc(succHand))
	hand.SetConflictPolicy(ConflictError)
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Test failed. Reason: %v", Reason(r))
	}))

	req = authedRequest(t, realToken, nil)
	req.Header.Set(HeaderName, sentToken)

	writer = httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("A single token source should have passed, got code %d", writer.Code)
	}
}

// The default policy keeps the historical behavior:
// the first extractor that provides a token wins.
func TestDefaultPolicyFirstWins(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Test failed. Reason: %v", Reason(r))
	}))

	realToken := generateToken()
	sentToken := b64encode(maskToken(realToken))

	// Both sources provide the same valid token.
	req := authedRequest(t, realToken, formBodyR([][]string{
		{FormFieldName, sentToken},
	}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(HeaderName, sentToken)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("The request should have succeeded, got code %d", writer.Code)
	}
}
