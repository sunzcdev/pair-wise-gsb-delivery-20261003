package digest

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

// joinRspAuth builds an Authentication-Info header value from a qop and a
// proof. Fields other than rspauth are omitted so binding is exercised only
// when explicitly requested.
func joinRspAuth(qop, rspauth string) string {
	return `qop="` + qop + `", rspauth="` + rspauth + `"`
}

// verifyTestServer returns a digest-authenticated server. The proof mode
// selects what Authentication-Info proof accompanies successful responses:
// "ok" sends the correct rspauth, "wrong" sends an incorrect one, and
// "missing" omits the header.
func verifyTestServer(t *testing.T, qop, respBody, proof string) *httptest.Server {
	t.Helper()
	username := "Mufasa"
	password := "Circle of Life"
	chal := &Challenge{
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		Algorithm: "MD5",
		QOP:       []string{qop},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// isolate subtests: failed proofs leave bodies unread
		w.Header().Set("Connection", "close")
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Add("WWW-Authenticate", chal.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reqBody, err := io.ReadAll(r.Body)
		assert.NilError(t, err)
		cred, err := ParseCredentials(auth)
		assert.NilError(t, err)
		want, err := Digest(chal, Options{
			Method:   r.Method,
			URI:      r.URL.RequestURI(),
			Cnonce:   cred.Cnonce,
			Count:    cred.Nc,
			Username: username,
			Password: password,
			GetBody: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(reqBody)), nil
			},
		})
		assert.NilError(t, err)
		if cred.Response != want.Response {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if proof != "missing" {
			sum := md5.Sum([]byte(respBody))
			rsp, err := rspAuth(cred, username, password, hex.EncodeToString(sum[:]))
			assert.NilError(t, err)
			if proof == "wrong" {
				rsp = "5d50d08380ec629129af2c7eac652cbf" // valid for a different URI
			}
			w.Header().Set("Authentication-Info", joinRspAuth(qop, rsp))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, respBody)
	}))
	return ts
}

func TestTransportVerifyRspAuth(t *testing.T) {
	const body = "Hello, World!"

	tests := []struct {
		name    string
		qop     string
		mode    VerifyMode
		proof   string // "ok", "wrong" or "missing"
		wantErr error
	}{
		{name: "auth strict correct", qop: "auth", mode: VerifyStrict, proof: "ok"},
		{name: "auth compat correct", qop: "auth", mode: VerifyCompat, proof: "ok"},
		{name: "auth strict missing", qop: "auth", mode: VerifyStrict, proof: "missing", wantErr: ErrRspAuthMissing},
		{name: "auth compat missing", qop: "auth", mode: VerifyCompat, proof: "missing"},
		{name: "auth strict wrong", qop: "auth", mode: VerifyStrict, proof: "wrong", wantErr: ErrRspAuthMismatch},
		{name: "auth compat wrong", qop: "auth", mode: VerifyCompat, proof: "wrong", wantErr: ErrRspAuthMismatch},
		{name: "auth-int strict correct", qop: "auth-int", mode: VerifyStrict, proof: "ok"},
		{name: "auth-int compat missing", qop: "auth-int", mode: VerifyCompat, proof: "missing"},
		{name: "auth-int strict missing", qop: "auth-int", mode: VerifyStrict, proof: "missing", wantErr: ErrRspAuthMissing},
		{name: "auth-int strict wrong", qop: "auth-int", mode: VerifyStrict, proof: "wrong", wantErr: ErrRspAuthMismatch},
		{name: "auth-int compat wrong", qop: "auth-int", mode: VerifyCompat, proof: "wrong", wantErr: ErrRspAuthMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := verifyTestServer(t, tt.qop, body, tt.proof)
			defer ts.Close()
			client := http.Client{Transport: &Transport{
				Username:       "Mufasa",
				Password:       "Circle of Life",
				VerifyResponse: tt.mode,
			}}
			res, err := client.Post(ts.URL, "text/plain", strings.NewReader(body))
			// qop=auth proofs are checked before the response is returned;
			// qop=auth-int proofs fail while the body is streamed.
			if tt.wantErr != nil && (tt.qop == "auth" || tt.proof == "missing") {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			assert.NilError(t, err)
			got, err := io.ReadAll(res.Body)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				_ = res.Body.Close()
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, string(got), body)
			assert.NilError(t, res.Body.Close())
		})
	}
}

// testCred are the RFC 7616 Section 3.9 parameters reused by the wrapper tests.
func testCred(qop string) *Credentials {
	return &Credentials{
		Username:  "Mufasa",
		Realm:     "http-auth@example.org",
		Nonce:     "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
		URI:       "/dir/index.html",
		Algorithm: "MD5",
		Cnonce:    "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
		QOP:       qop,
		Nc:        1,
	}
}

// closeCountingBody records how many times Close is invoked.
type closeCountingBody struct {
	io.Reader
	closes int
	err    error
}

func (c *closeCountingBody) Close() error {
	c.closes++
	return c.err
}

// eofWithData returns all of its data together with io.EOF on the first Read.
type eofWithData struct {
	data string
	read bool
}

func (e *eofWithData) Read(p []byte) (int, error) {
	if e.read {
		return 0, io.EOF
	}
	e.read = true
	n := copy(p, e.data)
	return n, io.EOF
}

// errAfterData returns data first, then a sentinel error.
type errAfterData struct {
	data string
	sent bool
}

func (e *errAfterData) Read(p []byte) (int, error) {
	if !e.sent {
		e.sent = true
		return copy(p, e.data), nil
	}
	return 0, errors.New("boom: read failed")
}

// newTestVerifiedBody constructs a wrapper with a proof computed for data.
func newTestVerifiedBody(t *testing.T, underlying io.ReadCloser, data string) *verifiedBody {
	t.Helper()
	cred := testCred("auth-int")
	sum := md5.Sum([]byte(data))
	want, err := rspAuth(cred, "Mufasa", "Circle of Life", hex.EncodeToString(sum[:]))
	assert.NilError(t, err)
	return newVerifiedBody(underlying, md5.New(), want, cred, "Mufasa", "Circle of Life")
}

func TestVerifiedBodyEmpty(t *testing.T) {
	underlying := &closeCountingBody{Reader: strings.NewReader("")}
	v := newTestVerifiedBody(t, underlying, "")
	got, err := io.ReadAll(v)
	assert.NilError(t, err)
	assert.Equal(t, string(got), "")
	assert.NilError(t, v.Close())
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyShortReads(t *testing.T) {
	const data = "Hello, World!"
	underlying := &closeCountingBody{Reader: strings.NewReader(data)}
	v := newTestVerifiedBody(t, underlying, data)
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := v.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		assert.NilError(t, err)
	}
	assert.Equal(t, string(got), data)
	assert.NilError(t, v.Close())
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyDataAndEOF(t *testing.T) {
	const data = "final bytes"
	underlying := &closeCountingBody{Reader: &eofWithData{data: data}}
	v := newTestVerifiedBody(t, underlying, data)
	buf := make([]byte, 64)
	n, err := v.Read(buf)
	assert.Equal(t, err, io.EOF)
	assert.Equal(t, string(buf[:n]), data)
	assert.NilError(t, v.Close())
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyEarlyClose(t *testing.T) {
	const data = "truncated response body"
	underlying := &closeCountingBody{Reader: strings.NewReader(data)}
	v := newTestVerifiedBody(t, underlying, data)
	buf := make([]byte, 4)
	n, err := v.Read(buf)
	assert.NilError(t, err)
	assert.Equal(t, n, 4)
	// close before EOF: the proof cannot be verified
	assert.ErrorIs(t, v.Close(), ErrRspAuthMissing)
	// underlying body is closed exactly once even on repeated Close
	assert.NilError(t, v.Close())
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyUnderlyingReadError(t *testing.T) {
	const data = "partial"
	underlying := &closeCountingBody{Reader: &errAfterData{data: data}}
	v := newTestVerifiedBody(t, underlying, data)
	buf := make([]byte, 64)
	n, err := v.Read(buf)
	assert.NilError(t, err)
	assert.Equal(t, string(buf[:n]), data)
	_, err = v.Read(buf)
	assert.Error(t, err, "boom: read failed")
	assert.Assert(t, !errors.Is(err, ErrRspAuthMismatch))
	assert.Assert(t, !errors.Is(err, ErrRspAuthMissing))
}

func TestVerifiedBodyReadErrorThenClose(t *testing.T) {
	const data = "partial"
	underlying := &closeCountingBody{Reader: &errAfterData{data: data}}
	v := newTestVerifiedBody(t, underlying, data)
	buf := make([]byte, 64)
	_, _ = v.Read(buf)
	_, readErr := v.Read(buf)
	assert.Assert(t, readErr != nil)
	// close only reports the underlying read failure, never a digest verdict
	err := v.Close()
	assert.Assert(t, !errors.Is(err, ErrRspAuthMismatch))
	assert.Assert(t, !errors.Is(err, ErrRspAuthMissing))
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyMismatch(t *testing.T) {
	const data = "the actual entity"
	cred := testCred("auth-int")
	// proof computed over a different entity
	sum := md5.Sum([]byte("something else"))
	wrong, err := rspAuth(cred, "Mufasa", "Circle of Life", hex.EncodeToString(sum[:]))
	assert.NilError(t, err)
	underlying := &closeCountingBody{Reader: strings.NewReader(data)}
	v := newVerifiedBody(underlying, md5.New(), wrong, cred, "Mufasa", "Circle of Life")
	_, err = io.ReadAll(v)
	assert.ErrorIs(t, err, ErrRspAuthMismatch)
	// the error sticks: the body is never read again
	_, err = v.Read(make([]byte, 1))
	assert.ErrorIs(t, err, ErrRspAuthMismatch)
	assert.ErrorIs(t, v.Close(), ErrRspAuthMismatch)
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyMismatchEarlyCloseJoinsErrors(t *testing.T) {
	const data = "x"
	cred := testCred("auth-int")
	wrong, err := rspAuth(cred, "Mufasa", "Circle of Life", hex.EncodeToString(make([]byte, 0)))
	assert.NilError(t, err)
	closeErr := errors.New("boom: close failed")
	underlying := &closeCountingBody{Reader: strings.NewReader(data), err: closeErr}
	v := newVerifiedBody(underlying, md5.New(), wrong, cred, "Mufasa", "Circle of Life")
	err = v.Close()
	assert.ErrorIs(t, err, ErrRspAuthMissing)
	assert.ErrorIs(t, err, closeErr)
	assert.Equal(t, underlying.closes, 1)
}

func TestVerifiedBodyStreamsLargeBody(t *testing.T) {
	// entity much larger than any single read buffer, must not be buffered whole
	data := strings.Repeat("abcdef0123", 100000)
	ts := verifyTestServer(t, "auth-int", data, "ok")
	defer ts.Close()
	client := http.Client{Transport: &Transport{
		Username:       "Mufasa",
		Password:       "Circle of Life",
		VerifyResponse: VerifyStrict,
	}}
	res, err := client.Post(ts.URL, "text/plain", strings.NewReader(data))
	assert.NilError(t, err)
	got, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, len(got), len(data))
	assert.Equal(t, string(got), data)
	assert.NilError(t, res.Body.Close())
}

func TestTransportVerifyCachedChallenge(t *testing.T) {
	const body = "second request"
	ts := verifyTestServer(t, "auth", body, "ok")
	defer ts.Close()
	tr := &Transport{
		Username:       "Mufasa",
		Password:       "Circle of Life",
		VerifyResponse: VerifyStrict,
	}
	client := http.Client{Transport: tr}
	// first request discovers and caches the challenge
	res1, err := client.Get(ts.URL)
	assert.NilError(t, err)
	assert.NilError(t, res1.Body.Close())
	// the second request is authenticated on the first try and still verified
	res2, err := client.Get(ts.URL)
	assert.NilError(t, err)
	got, err := io.ReadAll(res2.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(got), body)
	assert.NilError(t, res2.Body.Close())
}

func TestTransportVerifyOffIgnoresBadProof(t *testing.T) {
	const body = "whatever"
	ts := verifyTestServer(t, "auth", body, "wrong")
	defer ts.Close()
	client := http.Client{Transport: &Transport{
		Username: "Mufasa",
		Password: "Circle of Life",
	}}
	res, err := client.Get(ts.URL)
	assert.NilError(t, err)
	got, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, string(got), body)
	assert.NilError(t, res.Body.Close())
}

func TestRspAuthVectors(t *testing.T) {
	// https://tools.ietf.org/html/rfc7616#section-3.9.1 publishes the rspauth.
	cred := testCred("auth")
	rsp, err := rspAuth(cred, "Mufasa", "Circle of Life", "")
	assert.NilError(t, err)
	assert.Equal(t, rsp, "316170eece0438f017953fdabe84daec")

	// qop=auth-int with an empty entity body (RFC 7616 Section 3.9.2).
	credInt := testCred("auth-int")
	rsp, err = rspAuth(credInt, "Mufasa", "Circle of Life", "d41d8cd98f00b204e9800998ecf8427e")
	assert.NilError(t, err)
	assert.Equal(t, rsp, "1244e8877a48044622d767dfda501e8b")

	// SHA-256 binds the selected algorithm
	credSHA := testCred("auth")
	credSHA.Algorithm = "SHA-256"
	rsp, err = rspAuth(credSHA, "Mufasa", "Circle of Life", "")
	assert.NilError(t, err)
	assert.Equal(t, rsp, "0ef823dc6a40e632b75ddfdf950bfc010fb78b301becba5c9ddd2cea3ca9099d")
}

func TestParseAuthenticationInfo(t *testing.T) {
	tests := []struct {
		name  string
		input string
		rsp   string
		err   error
	}{
		{
			name:  "quoted",
			input: `qop="auth", rspauth="316170eece0438f017953fdabe84daec", cnonce="abc", nc=00000001`,
			rsp:   "316170eece0438f017953fdabe84daec",
		},
		{
			name:  "scheme prefix",
			input: `Digest rspauth="316170eece0438f017953fdabe84daec"`,
			rsp:   "316170eece0438f017953fdabe84daec",
		},
		{
			name:  "no rspauth",
			input: `nextnonce="5TsQWLVdgBdmrQ0XsxbDODV+57QdFR34I9HAbC/RVvkK"`,
			rsp:   "",
		},
		{
			name:  "invalid",
			input: `qop auth`,
			err:   ErrInvalidAuthInfo,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParseAuthenticationInfo(tt.input)
			if tt.err != nil {
				assert.ErrorIs(t, err, tt.err)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, info.RspAuth, tt.rsp)
		})
	}
}

func TestRspAuthBinding(t *testing.T) {
	cred := testCred("auth")
	want, err := rspAuth(cred, "Mufasa", "Circle of Life", "")
	assert.NilError(t, err)

	tests := []struct {
		name   string
		header string
		cred   *Credentials
	}{
		{name: "wrong nonce", cred: cred, header: `nonce="attacker-nonce", qop="auth", rspauth="` + want + `"`},
		{name: "wrong cnonce", cred: cred, header: `qop="auth", cnonce="attacker-cnonce", rspauth="` + want + `"`},
		{name: "wrong nc", cred: cred, header: `qop="auth", nc=00000002, rspauth="` + want + `"`},
		{name: "wrong qop", cred: cred, header: `qop="auth-int", rspauth="` + want + `"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := &http.Response{
				Header: http.Header{"Authentication-Info": []string{tt.header}},
				Body:   io.NopCloser(strings.NewReader("")),
			}
			tr := &Transport{
				Username:       "Mufasa",
				Password:       "Circle of Life",
				VerifyResponse: VerifyStrict,
			}
			assert.ErrorIs(t, tr.verify(res, tt.cred), ErrRspAuthMismatch)
		})
	}

	t.Run("wrong algorithm", func(t *testing.T) {
		credSHA := testCred("auth")
		credSHA.Algorithm = "SHA-256"
		res := &http.Response{
			Header: http.Header{"Authentication-Info": []string{
				`qop="auth", rspauth="` + want + `"`,
			}},
			Body: io.NopCloser(strings.NewReader("")),
		}
		tr := &Transport{
			Username:       "Mufasa",
			Password:       "Circle of Life",
			VerifyResponse: VerifyStrict,
		}
		assert.ErrorIs(t, tr.verify(res, credSHA), ErrRspAuthMismatch)
	})
}
