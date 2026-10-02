package nosurf

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// countingBody wraps an io.Reader and counts Read() calls,
// to verify whether the CSRF check consumes the request body.
type countingBody struct {
	r     io.Reader
	reads int
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	return b.r.Read(p)
}

func (b *countingBody) Close() error { return nil }

// Issues a GET against the handler and returns the token cookie
// along with a valid masked token derived from it.
func tokenAndCookie(h http.Handler) (*http.Cookie, string) {
	writer := httptest.NewRecorder()
	h.ServeHTTP(writer, dummyGet())

	resp := writer.Result()
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			return c, b64encode(maskToken(b64decode(c.Value)))
		}
	}
	return nil, ""
}

// Builds a same-origin POST request with the given body and cookie.
func postRequest(t *testing.T, body io.Reader, cookie *http.Cookie) *http.Request {
	req, err := http.NewRequest("POST", "http://example.com/", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("Referer", "https://example.com")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func TestCustomExtractorChain(t *testing.T) {
	// A JSON API keeps its token in a business-defined header.
	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(HeaderTokenExtractor("X-API-Token"))

	cookie, masked := tokenAndCookie(hand)

	// A token in the business-defined location passes.
	req := postRequest(t, nil, cookie)
	req.Header.Set("X-API-Token", masked)
	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)
	if writer.Code != 200 {
		t.Errorf("A token in the custom header should have passed, got code %d", writer.Code)
	}

	// A token in the default header is not picked up anymore.
	req = postRequest(t, nil, cookie)
	req.Header.Set(HeaderName, masked)
	writer = httptest.NewRecorder()
	hand.ServeHTTP(writer, req)
	if writer.Code != FailureCode {
		t.Errorf("A token in the default header should not be consulted, got code %d", writer.Code)
	}
}

func TestHeaderOnlyExtractionDoesNotConsumeBody(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("file", "payload"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	contentType := mw.FormDataContentType()
	want := buf.Bytes()

	var gotBody []byte
	readsAtEntry := -1
	cb := &countingBody{r: bytes.NewReader(want)}

	succ := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readsAtEntry = cb.reads
		if r.Body != nil {
			gotBody, _ = ioutil.ReadAll(r.Body)
		}
		w.WriteHeader(200)
	})

	hand := New(succ)
	hand.SetTokenExtractors(HeaderTokenExtractor(HeaderName))

	cookie, masked := tokenAndCookie(hand)

	req := postRequest(t, cb, cookie)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(HeaderName, masked)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Fatalf("The request should have succeeded, got code %d", writer.Code)
	}
	if readsAtEntry != 0 {
		t.Errorf("The CSRF check read the body %d times before the handler", readsAtEntry)
	}
	if req.MultipartForm != nil {
		t.Error("The CSRF check should not have parsed the multipart form")
	}
	if !bytes.Equal(gotBody, want) {
		t.Error("The downstream handler did not receive the intact body")
	}
}

func TestHeaderOnlyFailureShortCircuitsBody(t *testing.T) {
	readsAtFailure := -1
	cb := &countingBody{r: bytes.NewReader([]byte("payload"))}

	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(HeaderTokenExtractor(HeaderName))
	hand.SetFailureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readsAtFailure = cb.reads
		if Reason(r) != ErrBadToken {
			t.Errorf("Expected the reason %#v, got %#v", ErrBadToken, Reason(r))
		}
		http.Error(w, "", FailureCode)
	}))

	cookie, _ := tokenAndCookie(hand)

	req := postRequest(t, cb, cookie)
	req.Header.Set(HeaderName, "a very wrong value")

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Fatalf("The request should have failed, got code %d", writer.Code)
	}
	if readsAtFailure != 0 {
		t.Errorf("The failed CSRF check read the body %d times", readsAtFailure)
	}
}

func TestInvalidExtractorShortCircuitsChain(t *testing.T) {
	errBogus := errors.New("bogus token input")

	hand := New(http.HandlerFunc(succHand))
	cookie, masked := tokenAndCookie(hand)

	// The first extractor reports invalid input,
	// the second one would provide a valid token.
	hand.SetTokenExtractors(
		func(r *http.Request) (string, bool, error) { return "", false, errBogus },
		func(r *http.Request) (string, bool, error) { return masked, true, nil },
	)
	hand.SetFailureHandler(correctReason(t, errBogus))

	req := postRequest(t, nil, cookie)
	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Invalid input must not fall back to a valid token, got code %d", writer.Code)
	}
}

func TestInvalidHeaderDoesNotFallBackToForm(t *testing.T) {
	// The default chain: a duplicated (invalid) token header
	// must not fall back to a valid form token.
	hand := New(http.HandlerFunc(succHand))
	cookie, masked := tokenAndCookie(hand)

	req := postRequest(t, formBodyR([][]string{{FormFieldName, masked}}), cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add(HeaderName, masked)
	req.Header.Add(HeaderName, masked)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("An invalid header must not fall back to the form token, got code %d", writer.Code)
	}
}

func TestConflictingTokensFail(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(
		HeaderTokenExtractor("X-Token-A"),
		HeaderTokenExtractor("X-Token-B"),
	)
	hand.SetTokenConflictPolicy(FailOnConflict)
	hand.SetFailureHandler(correctReason(t, ErrConflictingTokens))

	cookie, masked := tokenAndCookie(hand)

	req := postRequest(t, nil, cookie)
	req.Header.Set("X-Token-A", masked)
	req.Header.Set("X-Token-B", "a different token")

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != FailureCode {
		t.Errorf("Differing tokens should fail with FailOnConflict, got code %d", writer.Code)
	}
}

func TestIdenticalTokensPassWithFailOnConflict(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(
		HeaderTokenExtractor("X-Token-A"),
		HeaderTokenExtractor("X-Token-B"),
	)
	hand.SetTokenConflictPolicy(FailOnConflict)

	cookie, masked := tokenAndCookie(hand)

	req := postRequest(t, nil, cookie)
	req.Header.Set("X-Token-A", masked)
	req.Header.Set("X-Token-B", masked)

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)

	if writer.Code != 200 {
		t.Errorf("Identical tokens should pass with FailOnConflict, got code %d", writer.Code)
	}
}

func TestFirstExtractorWins(t *testing.T) {
	hand := New(http.HandlerFunc(succHand))
	hand.SetTokenExtractors(
		HeaderTokenExtractor("X-Token-A"),
		HeaderTokenExtractor("X-Token-B"),
	)
	// FirstExtractorWins is the default policy.

	cookie, masked := tokenAndCookie(hand)

	// The first extractor's token is used, the second one is ignored.
	req := postRequest(t, nil, cookie)
	req.Header.Set("X-Token-A", masked)
	req.Header.Set("X-Token-B", "a different token")

	writer := httptest.NewRecorder()
	hand.ServeHTTP(writer, req)
	if writer.Code != 200 {
		t.Errorf("The first extractor's token should win, got code %d", writer.Code)
	}

	// A wrong token from the first extractor fails,
	// even if the second one is valid.
	req = postRequest(t, nil, cookie)
	req.Header.Set("X-Token-A", "a very wrong value")
	req.Header.Set("X-Token-B", masked)

	writer = httptest.NewRecorder()
	hand.ServeHTTP(writer, req)
	if writer.Code != FailureCode {
		t.Errorf("A wrong token from the first extractor should fail, got code %d", writer.Code)
	}
}
