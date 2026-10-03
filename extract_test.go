package nosurf

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Returns a cookie carrying a fresh real token
// and a matching masked token, ready to be sent back.
func tokenPair() (*http.Cookie, string) {
	realToken := generateToken()
	cookie := &http.Cookie{Name: CookieName, Value: b64encode(realToken)}
	return cookie, b64encode(maskToken(realToken))
}

// A success handler that records whether it was called.
func trackingHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		succHand(w, r)
	})
}

// A POST request to a same-origin URL that passes the origin checks.
func newPost(t *testing.T, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "http://example.com/", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", "https://example.com")
	return req
}

// When the token is already in the header, the request body
// must not be read at all to look for a form token.
func TestHeaderTokenSkipsBody(t *testing.T) {
	cookie, sentToken := tokenPair()

	called := false
	hand := New(trackingHandler(&called))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Test failed. Reason: %v", Reason(r))
	}))

	// A body that errors out on Read(): if nosurf touches it
	// while the header token is present, the check fails.
	req := newPost(t, failReader{})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(HeaderName, sentToken)
	req.AddCookie(cookie)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("Expected the request to succeed, got code %d", writer.Code)
	}
	if !called {
		t.Error("The success handler was not called")
	}
}

// A form parsed before CSRFHandler runs must be reused
// instead of reading the body again.
func TestAlreadyParsedFormIsReused(t *testing.T) {
	cookie, sentToken := tokenPair()

	called := false
	hand := New(trackingHandler(&called))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Test failed. Reason: %v", Reason(r))
	}))

	req := newPost(t, formBodyR([][]string{{FormFieldName, sentToken}}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	// If nosurf tried to read the body again, this would blow up.
	req.Body = io.NopCloser(failReader{})

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("Expected the request to succeed, got code %d", writer.Code)
	}
	if !called {
		t.Error("The success handler was not called")
	}
}

// A non-form content type must not cause the body to be read;
// with no token sent elsewhere, the check fails with ErrBadToken.
func TestNonFormContentTypeSkipsBody(t *testing.T) {
	cookie, _ := tokenPair()

	called := false
	hand := New(trackingHandler(&called))
	hand.SetFailureHandler(correctReason(t, ErrBadToken))

	// failReader proves the body is never read:
	// reading it would surface ErrFormRead instead of ErrBadToken.
	req := newPost(t, failReader{})
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(cookie)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Expected the check to fail with code %d, got %d", FailureCode, writer.Code)
	}
	if called {
		t.Error("The success handler must not be called after a failed check")
	}
}

// A body that cannot be read fails with ErrFormRead,
// which is distinct from ErrBadToken.
func TestFormBodyReadError(t *testing.T) {
	cookie, _ := tokenPair()

	called := false
	hand := New(trackingHandler(&called))
	hand.SetFailureHandler(correctReason(t, ErrFormRead))

	req := newPost(t, failReader{})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Expected the check to fail with code %d, got %d", FailureCode, writer.Code)
	}
	if called {
		t.Error("The success handler must not be called after a failed check")
	}
}

// A URL-encoded body larger than MaxFormBytes fails with
// ErrFormTooLarge, whether the size is known upfront or not.
func TestFormBodyTooLarge(t *testing.T) {
	cookie, _ := tokenPair()

	testCases := []struct {
		name          string
		contentLength int64
	}{
		{"declared Content-Length", 0}, // set by NewRequest
		{"unknown Content-Length", -1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			hand := New(trackingHandler(&called))
			hand.SetFailureHandler(correctReason(t, ErrFormTooLarge))

			body := strings.NewReader(strings.Repeat("a", MaxFormBytes+1))
			req := newPost(t, body)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(cookie)
			if tc.contentLength != 0 {
				req.ContentLength = tc.contentLength
			}

			writer := httptest.NewRecorder()
			hand.ServeHTTP(writer, req)

			if writer.Code != FailureCode {
				t.Errorf("Expected the check to fail with code %d, got %d", FailureCode, writer.Code)
			}
			if called {
				t.Error("The success handler must not be called after a failed check")
			}
		})
	}
}

// A malformed URL-encoded body fails with ErrBadForm,
// which is distinct from ErrBadToken.
func TestBadFormSyntax(t *testing.T) {
	cookie, _ := tokenPair()

	called := false
	hand := New(trackingHandler(&called))
	hand.SetFailureHandler(correctReason(t, ErrBadForm))

	req := newPost(t, strings.NewReader("csrf_token=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Expected the check to fail with code %d, got %d", FailureCode, writer.Code)
	}
	if called {
		t.Error("The success handler must not be called after a failed check")
	}
}

// A multipart content type without a boundary fails with ErrBadForm.
func TestMultipartMissingBoundary(t *testing.T) {
	cookie, _ := tokenPair()

	called := false
	hand := New(trackingHandler(&called))
	hand.SetFailureHandler(correctReason(t, ErrBadForm))

	req := newPost(t, strings.NewReader("some data"))
	req.Header.Set("Content-Type", "multipart/form-data")
	req.AddCookie(cookie)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Expected the check to fail with code %d, got %d", FailureCode, writer.Code)
	}
	if called {
		t.Error("The success handler must not be called after a failed check")
	}
}

// The token is found in a multipart form and the uploaded file
// is still available to the wrapped handler: nosurf must not
// remove the parsed multipart form (and its temporary files)
// before downstream handlers had a chance to use it.
func TestMultipartFileAvailableDownstream(t *testing.T) {
	cookie, sentToken := tokenPair()

	var body bytes.Buffer
	wr := multipart.NewWriter(&body)
	if err := wr.WriteField(FormFieldName, sentToken); err != nil {
		t.Fatal(err)
	}
	fw, err := wr.CreateFormFile("upload", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("hello, multipart")); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}

	var fileContent []byte
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _, err := r.FormFile("upload")
		if err != nil {
			t.Errorf("The multipart file is not available in the wrapped handler: %v", err)
			return
		}
		defer f.Close()
		fileContent, err = io.ReadAll(f)
		if err != nil {
			t.Errorf("Could not read the multipart file in the wrapped handler: %v", err)
		}
	}))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Test failed. Reason: %v", Reason(r))
	}))

	req := newPost(t, &body)
	req.Header.Set("Content-Type", wr.FormDataContentType())
	req.AddCookie(cookie)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("Expected the request to succeed, got code %d", writer.Code)
	}
	if string(fileContent) != "hello, multipart" {
		t.Errorf("Wrong file content in the wrapped handler: %q", fileContent)
	}
}
