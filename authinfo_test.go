package digest

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

const (
	verifyUsername = "foo"
	verifyPassword = "bar"
)

// goodProof computes the correct rspauth for the credentials, the same way a
// compliant server would
func goodProof(t *testing.T, algorithm string, cred *Credentials, entity string) string {
	t.Helper()
	h, err := hashFor(algorithm)
	assert.NilError(t, err)
	a1 := hashjoin(h, verifyUsername, cred.Realm, verifyPassword)
	var entityHash string
	if cred.QOP == "auth-int" {
		entityHash = hashjoin(h, entity)
	}
	return rspauth(h, a1, cred, entityHash)
}

// authInfoServer is a test server which challenges requests and proves itself
// with an Authentication-Info header
type authInfoServer struct {
	t         *testing.T
	algorithm string
	qop       []string
	body      string
	// proof derives the rspauth value to send back; nil omits the header
	proof func(cred *Credentials) string
}

func (s *authInfoServer) start() *httptest.Server {
	chal := &Challenge{
		Realm:     "test",
		Nonce:     "jgdfsijdfisd",
		Algorithm: s.algorithm,
		QOP:       s.qop,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Add("WWW-Authenticate", chal.String())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cred, err := ParseCredentials(auth)
		assert.NilError(s.t, err)
		// verify the client's proof
		want, err := Digest(chal, Options{
			Method:   r.Method,
			URI:      r.URL.RequestURI(),
			Cnonce:   cred.Cnonce,
			Count:    cred.Nc,
			Username: verifyUsername,
			Password: verifyPassword,
		})
		assert.NilError(s.t, err)
		if cred.Response != want.Response {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s.proof != nil {
			w.Header().Set("Authentication-Info", fmt.Sprintf(
				`rspauth="%s", qop=%s, cnonce="%s", nc=%08x`,
				s.proof(cred), cred.QOP, cred.Cnonce, cred.Nc,
			))
		}
		io.WriteString(w, s.body)
	}))
}

func verifyClient(mode VerifyMode) *http.Client {
	return &http.Client{
		Transport: &Transport{
			Username: verifyUsername,
			Password: verifyPassword,
			Verify:   mode,
		},
	}
}

func TestTransportVerifyAuth(t *testing.T) {
	proofs := map[string]func(s *authInfoServer) func(cred *Credentials) string{
		"correct": func(s *authInfoServer) func(cred *Credentials) string {
			return func(cred *Credentials) string {
				return goodProof(t, s.algorithm, cred, "")
			}
		},
		"wrong-password": func(s *authInfoServer) func(cred *Credentials) string {
			return func(cred *Credentials) string {
				h, err := hashFor(s.algorithm)
				assert.NilError(t, err)
				a1 := hashjoin(h, verifyUsername, cred.Realm, "wrong")
				return rspauth(h, a1, cred, "")
			}
		},
		"wrong-nc": func(s *authInfoServer) func(cred *Credentials) string {
			return func(cred *Credentials) string {
				tampered := *cred
				tampered.Nc++
				return goodProof(t, s.algorithm, &tampered, "")
			}
		},
		"wrong-uri": func(s *authInfoServer) func(cred *Credentials) string {
			return func(cred *Credentials) string {
				tampered := *cred
				tampered.URI = "/other"
				return goodProof(t, s.algorithm, &tampered, "")
			}
		},
		"missing": func(s *authInfoServer) func(cred *Credentials) string {
			return nil
		},
	}
	for _, algorithm := range []string{"MD5", "SHA-256"} {
		for _, mode := range []VerifyMode{VerifyCompat, VerifyStrict} {
			for name, proof := range proofs {
				t.Run(fmt.Sprintf("%s/%d/%s", algorithm, mode, name), func(t *testing.T) {
					s := &authInfoServer{
						t:         t,
						algorithm: algorithm,
						qop:       []string{"auth"},
						body:      "Hello World",
					}
					s.proof = proof(s)
					ts := s.start()
					defer ts.Close()
					res, err := verifyClient(mode).Get(ts.URL)
					switch {
					case name == "correct":
						assert.NilError(t, err)
						body, err := io.ReadAll(res.Body)
						assert.NilError(t, err)
						assert.NilError(t, res.Body.Close())
						assert.Equal(t, string(body), "Hello World")
					case name == "missing" && mode == VerifyCompat:
						assert.NilError(t, err)
						assert.NilError(t, res.Body.Close())
					case name == "missing":
						assert.Assert(t, errors.Is(err, ErrNoAuthenticationInfo), "got: %v", err)
					default:
						assert.Assert(t, errors.Is(err, ErrResponseAuth), "got: %v", err)
					}
				})
			}
		}
	}
}

func TestTransportVerifyAuthInt(t *testing.T) {
	for _, body := range []string{"Hello World", ""} {
		for _, mode := range []VerifyMode{VerifyCompat, VerifyStrict} {
			for _, kind := range []string{"correct", "wrong", "missing"} {
				name := fmt.Sprintf("body=%q/%d/%s", body, mode, kind)
				t.Run(name, func(t *testing.T) {
					s := &authInfoServer{
						t:         t,
						algorithm: "MD5",
						qop:       []string{"auth-int"},
						body:      body,
					}
					switch kind {
					case "correct":
						s.proof = func(cred *Credentials) string {
							return goodProof(t, "MD5", cred, body)
						}
					case "wrong":
						s.proof = func(cred *Credentials) string {
							return goodProof(t, "MD5", cred, "tampered")
						}
					}
					ts := s.start()
					defer ts.Close()
					res, err := verifyClient(mode).Get(ts.URL)
					switch {
					case kind == "missing" && mode == VerifyStrict:
						assert.Assert(t, errors.Is(err, ErrNoAuthenticationInfo), "got: %v", err)
						return
					case kind == "missing":
						assert.NilError(t, err)
					default:
						assert.NilError(t, err)
					}
					got, err := io.ReadAll(res.Body)
					if kind == "wrong" {
						assert.Assert(t, errors.Is(err, ErrResponseAuth), "got: %v", err)
					} else {
						assert.NilError(t, err)
						assert.Equal(t, string(got), body)
					}
					assert.NilError(t, res.Body.Close())
				})
			}
		}
	}
}

func TestTransportVerifyDisabled(t *testing.T) {
	s := &authInfoServer{
		t:         t,
		algorithm: "MD5",
		qop:       []string{"auth"},
		body:      "Hello World",
		proof: func(cred *Credentials) string {
			return "bogus"
		},
	}
	ts := s.start()
	defer ts.Close()
	res, err := verifyClient(VerifyNone).Get(ts.URL)
	assert.NilError(t, err)
	body, err := io.ReadAll(res.Body)
	assert.NilError(t, err)
	assert.NilError(t, res.Body.Close())
	assert.Equal(t, string(body), "Hello World")
}

func TestParseAuthenticationInfo(t *testing.T) {
	ai, err := ParseAuthenticationInfo(`rspauth="6629fae49393a05397450978507c4ef1", qop=auth, cnonce="0a4f113b", nc=00000001, nextnonce="abc"`)
	assert.NilError(t, err)
	assert.DeepEqual(t, ai, &AuthenticationInfo{
		NextNonce: "abc",
		QOP:       "auth",
		Response:  "6629fae49393a05397450978507c4ef1",
		Cnonce:    "0a4f113b",
		Nc:        1,
	})
}

// dripReader returns at most n bytes per Read
type dripReader struct {
	data []byte
	n    int
}

func (r *dripReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(min(len(p), len(r.data)), r.n)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// eofDataReader returns the remaining data and io.EOF in the same Read
type eofDataReader struct {
	data []byte
	done bool
}

func (r *eofDataReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), io.EOF
}

// errReader fails every Read
type errReader struct {
	err error
}

func (r *errReader) Read([]byte) (int, error) {
	return 0, r.err
}

// closeCounter counts Close calls
type closeCounter struct {
	rc     io.Reader
	closes int
}

func (c *closeCounter) Read(p []byte) (int, error) {
	return c.rc.Read(p)
}

func (c *closeCounter) Close() error {
	c.closes++
	return nil
}

func newVerifyBody(t *testing.T, entity string, rc io.ReadCloser, want string) *verifyBody {
	t.Helper()
	cred := &Credentials{
		Realm:     "test",
		Nonce:     "jgdfsijdfisd",
		URI:       "/dir/index.html",
		Algorithm: "MD5",
		Cnonce:    "0a4f113b",
		QOP:       "auth-int",
		Nc:        1,
	}
	h, err := hashFor(cred.Algorithm)
	assert.NilError(t, err)
	a1 := hashjoin(h, verifyUsername, cred.Realm, verifyPassword)
	if want == "" {
		want = rspauth(h, a1, cred, hashjoin(h, entity))
	}
	return &verifyBody{
		rc:   rc,
		h:    h,
		a1:   a1,
		cred: cred,
		want: want,
	}
}

func TestVerifyBodyShortReads(t *testing.T) {
	entity := "Hello World"
	body := newVerifyBody(t, entity, io.NopCloser(&dripReader{data: []byte(entity), n: 1}), "")
	got, err := io.ReadAll(body)
	assert.NilError(t, err)
	assert.Equal(t, string(got), entity)
	assert.NilError(t, body.Close())
}

func TestVerifyBodyEmpty(t *testing.T) {
	body := newVerifyBody(t, "", io.NopCloser(strings.NewReader("")), "")
	got, err := io.ReadAll(body)
	assert.NilError(t, err)
	assert.Equal(t, string(got), "")
	assert.NilError(t, body.Close())
}

func TestVerifyBodyDataWithEOF(t *testing.T) {
	entity := "Hello World"
	body := newVerifyBody(t, entity, io.NopCloser(&eofDataReader{data: []byte(entity)}), "")
	got, err := io.ReadAll(body)
	assert.NilError(t, err)
	assert.Equal(t, string(got), entity)
	assert.NilError(t, body.Close())
}

func TestVerifyBodyMismatch(t *testing.T) {
	entity := "Hello World"
	body := newVerifyBody(t, entity, io.NopCloser(strings.NewReader(entity)), "bogus")
	_, err := io.ReadAll(body)
	assert.Assert(t, errors.Is(err, ErrResponseAuth), "got: %v", err)
}

func TestVerifyBodyEarlyClose(t *testing.T) {
	entity := "Hello World"
	rc := &closeCounter{rc: strings.NewReader(entity)}
	body := newVerifyBody(t, entity, rc, "")
	p := make([]byte, 5)
	n, err := body.Read(p)
	assert.NilError(t, err)
	assert.Equal(t, string(p[:n]), "Hello")
	// closing before EOF reports the missing verification
	err = body.Close()
	assert.Assert(t, errors.Is(err, ErrResponseUnverified), "got: %v", err)
	assert.Equal(t, rc.closes, 1)
	// closing again must not close the underlying body twice
	err = body.Close()
	assert.Assert(t, errors.Is(err, ErrResponseUnverified), "got: %v", err)
	assert.Equal(t, rc.closes, 1)
}

func TestVerifyBodyReadError(t *testing.T) {
	errBoom := errors.New("boom")
	rc := &closeCounter{rc: &errReader{err: errBoom}}
	body := newVerifyBody(t, "", rc, "")
	_, err := body.Read(make([]byte, 1))
	assert.Assert(t, errors.Is(err, errBoom), "got: %v", err)
	assert.Assert(t, !errors.Is(err, ErrResponseAuth))
	assert.Assert(t, !errors.Is(err, ErrResponseUnverified))
	// the read error already surfaced, Close only reports the close result
	assert.NilError(t, body.Close())
	assert.Equal(t, rc.closes, 1)
}
