package nosurf

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
)

// errReadCloser is a body that always fails on Read.
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errReadCloser) Close() error             { return nil }

// csrfCookie runs a GET through the handler and returns the CSRF cookie
// it sets, so that tests can build requests with a valid token.
func csrfCookie(t *testing.T, hand *CSRFHandler) *http.Cookie {
	t.Helper()

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, dummyGet())

	cookie := getRespCookie(writer.Result(), CookieName)
	if cookie == nil {
		t.Fatal("CSRF cookie was not set")
	}
	return cookie
}

// newFormPost builds a POST request that passes the origin checks
// and carries the given cookie.
func newFormPost(t *testing.T, cookie *http.Cookie, contentType string, body io.Reader) *http.Request {
	t.Helper()

	req, err := http.NewRequest("POST", "http://example.com/", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set("Referer", "https://example.com")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

// failRecorder returns a failure handler that records the failure reason
// and a function to retrieve it.
func failRecorder() (http.Handler, func() error) {
	var reason error
	hand := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reason = Reason(r)
		http.Error(w, "", FailureCode)
	})
	return hand, func() error { return reason }
}

func TestHeaderTokenSkipsBody(t *testing.T) {
	called := false
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		succHand(w, r)
	}))
	cookie := csrfCookie(t, hand)
	token := b64encode(maskToken(b64decode(cookie.Value)))

	// The body would fail on any read; the header token must win
	// without ever touching it.
	req := newFormPost(t, cookie, "application/x-www-form-urlencoded", errReadCloser{})
	req.Header.Set(HeaderName, token)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if !called {
		t.Error("Success handler was not called with a valid header token")
	}
	if writer.Code != 200 {
		t.Errorf("Expected status 200, got %d", writer.Code)
	}
}

func TestExtractTokenReusesParsedForm(t *testing.T) {
	called := false
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		succHand(w, r)
	}))
	cookie := csrfCookie(t, hand)
	token := b64encode(maskToken(b64decode(cookie.Value)))

	req := newFormPost(t, cookie, "application/x-www-form-urlencoded",
		formBodyR([][]string{{FormFieldName, token}}))
	// Parse the form upstream; the body is now drained.
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	// Any further read would fail, proving the body is not re-read.
	req.Body = errReadCloser{}

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if !called {
		t.Error("Success handler was not called with a pre-parsed form")
	}
	if writer.Code != 200 {
		t.Errorf("Expected status 200, got %d", writer.Code)
	}
}

func TestExtractTokenIgnoresNonFormContentType(t *testing.T) {
	called := false
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	fail, reason := failRecorder()
	hand.SetFailureHandler(fail)

	// A JSON body is not a form; it must be left unread
	// (any read would fail here) and yield ErrBadToken, not a read error.
	req := newFormPost(t, nil, "application/json", errReadCloser{})

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if called {
		t.Error("Success handler was called for a request without a token")
	}
	if got := reason(); got != ErrBadToken {
		t.Errorf("Expected reason %v, got %v", ErrBadToken, got)
	}
}

func TestExtractTokenBodyReadError(t *testing.T) {
	called := false
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	fail, reason := failRecorder()
	hand.SetFailureHandler(fail)

	req := newFormPost(t, nil, "application/x-www-form-urlencoded", errReadCloser{})

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if called {
		t.Error("Success handler was called despite a body read error")
	}
	if got := reason(); got != ErrFormRead {
		t.Errorf("Expected reason %v, got %v", ErrFormRead, got)
	}
	if writer.Code != FailureCode {
		t.Errorf("Expected status %d, got %d", FailureCode, writer.Code)
	}
}

func TestExtractTokenBodyTooLarge(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "urlencoded",
			contentType: "application/x-www-form-urlencoded",
			body:        "name=Jolene&csrf_token=toolong",
		},
		{
			name:        "multipart",
			contentType: "multipart/form-data; boundary=boundary",
			body: "--boundary\r\n" +
				"Content-Disposition: form-data; name=\"name\"\r\n\r\n" +
				"Jolene\r\n--boundary--\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))
			fail, reason := failRecorder()
			hand.SetFailureHandler(fail)
			hand.SetMaxFormBytes(8)

			req := newFormPost(t, nil, tc.contentType, strings.NewReader(tc.body))

			writer := httptest.NewRecorder()
			hand.ServeHTTP(writer, req)

			if called {
				t.Error("Success handler was called for an oversized body")
			}
			if got := reason(); got != ErrFormTooLarge {
				t.Errorf("Expected reason %v, got %v", ErrFormTooLarge, got)
			}
			if writer.Code != FailureCode {
				t.Errorf("Expected status %d, got %d", FailureCode, writer.Code)
			}
		})
	}
}

func TestExtractTokenFormParseError(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "bad percent-encoding",
			contentType: "application/x-www-form-urlencoded",
			body:        "csrf_token=%zz",
		},
		{
			name:        "multipart without boundary",
			contentType: "multipart/form-data",
			body:        "not a multipart body",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))
			fail, reason := failRecorder()
			hand.SetFailureHandler(fail)

			req := newFormPost(t, nil, tc.contentType, strings.NewReader(tc.body))

			writer := httptest.NewRecorder()
			hand.ServeHTTP(writer, req)

			if called {
				t.Error("Success handler was called for a malformed form")
			}
			if got := reason(); got != ErrFormParse {
				t.Errorf("Expected reason %v, got %v", ErrFormParse, got)
			}
			if writer.Code != FailureCode {
				t.Errorf("Expected status %d, got %d", FailureCode, writer.Code)
			}
		})
	}
}

// TestMultipartTempFilesSurvive checks that temporary files created while
// parsing a multipart form are not removed by the middleware: the success
// handler must still be able to read them, and they must still exist after
// ServeHTTP returns. Cleanup is the caller's job (RemoveAll), or net/http's
// for real server requests.
func TestMultipartTempFilesSurvive(t *testing.T) {
	fileContent := strings.Repeat("upload me ", 128)

	var tmpPath string
	var handlerReq *http.Request
	hand := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			succHand(w, r)
			return
		}
		handlerReq = r
		fh := r.MultipartForm.File["upload"][0]
		f, err := fh.Open()
		if err != nil {
			t.Errorf("Success handler could not open the uploaded file: %v", err)
			return
		}
		defer f.Close()
		// A part this large must be backed by a temporary file on disk.
		osFile, ok := f.(*os.File)
		if !ok {
			t.Error("Uploaded file was not spooled to a temporary file")
			return
		}
		tmpPath = osFile.Name()
		got, err := io.ReadAll(f)
		if err != nil {
			t.Errorf("Success handler could not read the uploaded file: %v", err)
			return
		}
		if string(got) != fileContent {
			t.Errorf("Uploaded file content mismatch: got %d bytes", len(got))
		}
		succHand(w, r)
	}))
	// Force the file part to be spooled to a temporary file on disk.
	hand.SetMaxFormMemory(16)
	cookie := csrfCookie(t, hand)
	token := b64encode(maskToken(b64decode(cookie.Value)))

	var buf bytes.Buffer
	wr := multipart.NewWriter(&buf)
	if err := wr.WriteField(FormFieldName, token); err != nil {
		t.Fatal(err)
	}
	part, err := wr.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="upload"; filename="up.txt"`},
		"Content-Type":        {"text/plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(fileContent)); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}

	req := newFormPost(t, cookie, wr.FormDataContentType(), &buf)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Fatalf("Expected status 200, got %d", writer.Code)
	}
	if handlerReq == nil || handlerReq.MultipartForm == nil {
		t.Fatal("r.MultipartForm was not populated for downstream handlers")
	}
	if tmpPath == "" {
		t.Fatal("The uploaded file was not spooled to a temporary file")
	}
	// The middleware must not remove temporary files that downstream
	// handlers may still be using.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("Temporary file was removed too early: %v", err)
	}

	// The request owns the files; clean them up now that we're done.
	handlerReq.MultipartForm.RemoveAll()
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("Temporary file was not removed by RemoveAll: %v", err)
	}
}
