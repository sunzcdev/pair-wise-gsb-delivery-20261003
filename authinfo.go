package digest

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/icholy/digest/internal/param"
)

// VerifyMode controls how the Transport validates the rspauth proof in the
// server's Authentication-Info response header.
type VerifyMode int

const (
	// VerifyOff does not validate the Authentication-Info header.
	// This is the zero value and preserves the legacy behavior.
	VerifyOff VerifyMode = iota
	// VerifyCompat accepts responses that omit the rspauth proof, but rejects
	// responses that carry an incorrect proof.
	VerifyCompat
	// VerifyStrict requires a correct rspauth proof on every authenticated
	// response. Responses without one are rejected.
	VerifyStrict
)

// AuthenticationInfo is the parsed Authentication-Info response header.
// See RFC 2617 Section 3.2.3 and RFC 7616 Section 3.5.
type AuthenticationInfo struct {
	RspAuth   string
	Cnonce    string
	Nonce     string
	QOP       string
	Nc        int
	NextNonce string
}

// ErrInvalidAuthInfo is returned when the Authentication-Info header is
// present but cannot be parsed.
var ErrInvalidAuthInfo = errors.New("digest: invalid Authentication-Info header")

// ErrRspAuthMissing is returned in strict verification mode when the
// authenticated response does not carry an rspauth proof, or when an auth-int
// response body is closed before the proof can be verified.
var ErrRspAuthMissing = errors.New("digest: missing rspauth in Authentication-Info")

// ErrRspAuthMismatch is returned when the rspauth proof does not match the
// value computed from this request's nonce, nc, cnonce, URI, qop and algorithm.
var ErrRspAuthMismatch = errors.New("digest: rspauth mismatch")

// ParseAuthenticationInfo parses an Authentication-Info response header value.
// The optional "Digest" auth-scheme prefix is accepted.
func ParseAuthenticationInfo(s string) (*AuthenticationInfo, error) {
	if body, ok := CutPrefix(s); ok {
		s = body
	}
	pp, err := param.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidAuthInfo, err)
	}
	if len(pp) == 0 {
		return nil, fmt.Errorf("%w: no parameters", ErrInvalidAuthInfo)
	}
	var info AuthenticationInfo
	for _, p := range pp {
		switch p.Key {
		case "rspauth":
			info.RspAuth = p.Value
		case "cnonce":
			info.Cnonce = p.Value
		case "nonce":
			info.Nonce = p.Value
		case "qop":
			info.QOP = p.Value
		case "nc":
			nc, err := parseHexNc(p.Value)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidAuthInfo, err)
			}
			info.Nc = nc
		case "nextnonce":
			info.NextNonce = p.Value
		}
	}
	return &info, nil
}

// findAuthenticationInfo returns the first Authentication-Info header that
// carries an rspauth parameter.
func findAuthenticationInfo(h http.Header) (*AuthenticationInfo, error) {
	var invalid error
	for _, value := range h.Values("Authentication-Info") {
		info, err := ParseAuthenticationInfo(value)
		if err != nil {
			if invalid == nil {
				invalid = err
			}
			continue
		}
		if info.RspAuth != "" {
			return info, nil
		}
	}
	if invalid != nil {
		return nil, invalid
	}
	return nil, nil
}

// rspAuth computes the response digest the server proves in the rspauth field.
// It binds this request's nonce, nc, cnonce and URI, the selected qop and
// algorithm, and the account credentials. For auth-int, entityHash is the hex
// hash of the response entity body; it is ignored for auth.
func rspAuth(cred *Credentials, username, password, entityHash string) (string, error) {
	h, err := newHash(cred.Algorithm)
	if err != nil {
		return "", err
	}
	a1 := hashjoin(h, username, cred.Realm, password)
	if !strings.EqualFold(cred.QOP, "auth-int") {
		return hashjoin(h,
			a1,
			cred.Nonce,
			fmt.Sprintf("%08x", cred.Nc),
			cred.Cnonce,
			cred.QOP,
			hashjoin(h, ":", cred.URI), // A2
		), nil
	}
	return hashjoin(h,
		a1,
		cred.Nonce,
		fmt.Sprintf("%08x", cred.Nc),
		cred.Cnonce,
		cred.QOP,
		hashjoin(h, ":", cred.URI, entityHash), // A2
	), nil
}

// verifyRspAuth compares the provided proof against the expected value.
// The comparison is case-insensitive because the digest is hex encoded.
func verifyRspAuth(want, got string) bool {
	if len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(strings.ToLower(want)),
		[]byte(strings.ToLower(got)),
	) == 1
}

// verifiedBody wraps an authenticated response body for auth-int rspauth
// verification. It hashes the entity while streaming it through a single read,
// and verifies the digest at EOF. The underlying body is closed at most once.
type verifiedBody struct {
	body     io.ReadCloser
	hash     hash.Hash
	expected string
	username string
	password string
	cred     *Credentials

	done    bool
	readErr error
	verify  error
	once    sync.Once
	closed  error
}

// newVerifiedBody wraps an authenticated response body for auth-int rspauth
// verification.
func newVerifiedBody(body io.ReadCloser, h hash.Hash, expected string, cred *Credentials, username, password string) *verifiedBody {
	return &verifiedBody{
		body:     body,
		hash:     h,
		expected: expected,
		username: username,
		password: password,
		cred:     cred,
	}
}

// Read passes bytes through the running entity hash. Underlying read errors are
// reported as-is; reaching EOF with a digest mismatch reports ErrRspAuthMismatch.
func (v *verifiedBody) Read(p []byte) (int, error) {
	if v.verify != nil {
		return 0, v.verify
	}
	n, err := v.body.Read(p)
	if n > 0 {
		_, _ = v.hash.Write(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) {
		v.readErr = err
	}
	if errors.Is(err, io.EOF) {
		v.check()
		if v.verify != nil {
			return n, v.verify
		}
	}
	return n, err
}

// check finalizes the entity digest and verifies it exactly once.
func (v *verifiedBody) check() {
	if v.done {
		return
	}
	v.done = true
	entityHash := fmt.Sprintf("%x", v.hash.Sum(nil))
	want, err := rspAuth(v.cred, v.username, v.password, entityHash)
	if err == nil && !verifyRspAuth(want, v.expected) {
		err = ErrRspAuthMismatch
	}
	v.verify = err
}

// Close closes the underlying body exactly once. Closing before EOF leaves the
// proof unverifiable and reports a verdict error joined with any close error;
// closing after an underlying read failure reports that failure unchanged.
func (v *verifiedBody) Close() error {
	first := false
	v.once.Do(func() {
		first = true
		if !v.done {
			v.done = true
			if v.readErr != nil {
				v.verify = v.readErr
			} else {
				v.verify = ErrRspAuthMissing
			}
		}
		v.closed = v.body.Close()
	})
	if first && v.verify != nil {
		return errors.Join(v.verify, v.closed)
	}
	return v.closed
}
