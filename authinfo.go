package digest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"

	"github.com/icholy/digest/internal/param"
)

// VerifyMode controls how the Transport validates the server's
// Authentication-Info rspauth proof.
type VerifyMode int

const (
	// VerifyNone disables server response verification. This is the default.
	VerifyNone VerifyMode = iota
	// VerifyCompat verifies the rspauth proof when the server provides one,
	// but accepts responses which don't contain an Authentication-Info header.
	VerifyCompat
	// VerifyStrict requires a valid rspauth proof on every authenticated
	// response. Responses missing the proof are rejected.
	VerifyStrict
)

// ErrNoAuthenticationInfo indicates that the response did not contain an
// Authentication-Info header with an rspauth proof.
var ErrNoAuthenticationInfo = errors.New("digest: no Authentication-Info rspauth in response")

// ErrResponseAuth indicates that the server's rspauth proof does not match
// the expected value for this request.
var ErrResponseAuth = errors.New("digest: response authentication failed")

// ErrResponseUnverified indicates that an auth-int response body was closed
// before it could be fully read and verified.
var ErrResponseUnverified = errors.New("digest: response body closed before authentication completed")

// AuthenticationInfo is a parsed Authentication-Info response header.
// Note: the NextNonce field is parsed but not currently used.
type AuthenticationInfo struct {
	NextNonce string
	QOP       string
	Response  string
	Cnonce    string
	Nc        int
}

// ParseAuthenticationInfo parses the Authentication-Info header value
func ParseAuthenticationInfo(s string) (*AuthenticationInfo, error) {
	pp, err := param.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("digest: invalid authentication info: %w", err)
	}
	var ai AuthenticationInfo
	for _, p := range pp {
		switch p.Key {
		case "nextnonce":
			ai.NextNonce = p.Value
		case "qop":
			ai.QOP = p.Value
		case "rspauth":
			ai.Response = p.Value
		case "cnonce":
			ai.Cnonce = p.Value
		case "nc":
			nc, err := strconv.ParseInt(p.Value, 16, 32)
			if err != nil {
				return nil, fmt.Errorf("digest: invalid nc: %w", err)
			}
			ai.Nc = int(nc)
		}
	}
	return &ai, nil
}

// FindAuthenticationInfo returns the first Authentication-Info header
// containing an rspauth proof
func FindAuthenticationInfo(h http.Header) (*AuthenticationInfo, error) {
	var last error
	for _, header := range h.Values("Authentication-Info") {
		ai, err := ParseAuthenticationInfo(header)
		if err != nil {
			last = err
			continue
		}
		if ai.Response != "" {
			return ai, nil
		}
	}
	if last != nil {
		return nil, last
	}
	return nil, ErrNoAuthenticationInfo
}

// rspauth computes the expected server response proof for the credentials.
// The entityHash is only used with qop=auth-int. The proof is bound to the
// request's nonce, nc, cnonce, URI and the selected digest algorithm.
func rspauth(h hash.Hash, a1 string, cred *Credentials, entityHash string) string {
	var a2 string
	switch cred.QOP {
	case "auth-int":
		a2 = hashjoin(h, "", cred.URI, entityHash) // A2 = ":" + uri + ":" + H(entity)
	default:
		a2 = hashjoin(h, "", cred.URI) // A2 = ":" + uri
	}
	return hashjoin(h,
		a1,
		cred.Nonce,
		fmt.Sprintf("%08x", cred.Nc),
		cred.Cnonce,
		cred.QOP,
		a2,
	)
}

// verify checks the server's Authentication-Info rspauth proof against the
// credentials used for the request. For qop=auth the proof is checked before
// the response is returned. For qop=auth-int the response body is wrapped and
// the proof is checked once the body has been read to EOF.
func (t *Transport) verify(res *http.Response, cred *Credentials) (*http.Response, error) {
	if t.Verify == VerifyNone || cred == nil {
		return res, nil
	}
	switch cred.QOP {
	case "auth", "auth-int":
	default:
		// rspauth is only defined when a qop is used
		return res, nil
	}
	ai, err := FindAuthenticationInfo(res.Header)
	if err != nil {
		if errors.Is(err, ErrNoAuthenticationInfo) && t.Verify == VerifyCompat {
			return res, nil
		}
		_ = res.Body.Close()
		return nil, err
	}
	h, err := hashFor(cred.Algorithm)
	if err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	a1 := hashjoin(h, t.Username, cred.Realm, t.Password)
	if cred.QOP == "auth" {
		if ai.Response != rspauth(h, a1, cred, "") {
			_ = res.Body.Close()
			return nil, ErrResponseAuth
		}
		return res, nil
	}
	// qop=auth-int: the proof covers the response entity, so it can only be
	// checked once the body has been read to EOF
	res.Body = &verifyBody{
		rc:   res.Body,
		h:    h,
		a1:   a1,
		cred: cred,
		want: ai.Response,
	}
	return res, nil
}

// verifyBody wraps an auth-int response body and verifies the server's
// rspauth proof against the entity digest once EOF is reached. The body is
// streamed through the hash exactly once and never buffered.
type verifyBody struct {
	rc   io.ReadCloser
	h    hash.Hash
	a1   string
	cred *Credentials
	want string

	started  bool
	verified bool
	readErr  error
	closed   bool
	closeErr error
}

func (b *verifyBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	if !b.started {
		// the hash was used to derive a1, reset it before streaming the entity
		b.h.Reset()
		b.started = true
	}
	n, err := b.rc.Read(p)
	if n > 0 {
		b.h.Write(p[:n])
	}
	if err == io.EOF {
		entity := hex.EncodeToString(b.h.Sum(nil))
		if rspauth(b.h, b.a1, b.cred, entity) != b.want {
			b.readErr = ErrResponseAuth
			return n, b.readErr
		}
		b.verified = true
	}
	if err != nil {
		b.readErr = err
	}
	return n, err
}

func (b *verifyBody) Close() error {
	if b.closed {
		return b.closeErr
	}
	b.closed = true
	err := b.rc.Close()
	if !b.verified && b.readErr == nil {
		err = errors.Join(err, ErrResponseUnverified)
	}
	b.closeErr = err
	return err
}
