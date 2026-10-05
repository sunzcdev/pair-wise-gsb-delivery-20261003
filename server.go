package digest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// AuthErrorKind identifies a server-side digest authentication failure.
type AuthErrorKind int

const (
	AuthInvalidChallenge AuthErrorKind = iota + 1
	AuthInvalidRequest
	AuthInvalidCredentials
	AuthUnknownUser
	AuthInvalidDigest
	AuthNcReplayed
	AuthLookupFailed
)

// AuthError is a classified authentication error. It never contains a password or HA1.
type AuthError struct {
	kind AuthErrorKind
	msg  string
}

func (e *AuthError) Error() string {
	return e.msg
}

// Kind returns the authentication failure category.
func (e *AuthError) Kind() AuthErrorKind {
	return e.kind
}

func (e *AuthError) Is(target error) bool {
	other, ok := target.(*AuthError)
	return ok && other.kind == e.kind
}

var (
	// ErrInvalidChallenge indicates that the server challenge is missing or unsupported.
	ErrInvalidChallenge = &AuthError{kind: AuthInvalidChallenge, msg: "digest: invalid challenge"}
	// ErrInvalidRequest indicates that the caller supplied an invalid verification request.
	ErrInvalidRequest = &AuthError{kind: AuthInvalidRequest, msg: "digest: invalid verification request"}
	// ErrInvalidCredentials indicates that a required credential field is missing or malformed.
	ErrInvalidCredentials = &AuthError{kind: AuthInvalidCredentials, msg: "digest: invalid credentials"}
	// ErrUnknownUser indicates that the secret lookup did not find the user.
	ErrUnknownUser = &AuthError{kind: AuthUnknownUser, msg: "digest: unknown user"}
	// ErrInvalidDigest indicates that the response digest does not match.
	ErrInvalidDigest = &AuthError{kind: AuthInvalidDigest, msg: "digest: invalid response digest"}
	// ErrNcReplayed indicates that the nonce count has already been used.
	ErrNcReplayed = &AuthError{kind: AuthNcReplayed, msg: "digest: nonce count replayed"}
	// ErrLookupFailed indicates that the secret lookup failed for a reason other than an unknown user.
	ErrLookupFailed = &AuthError{kind: AuthLookupFailed, msg: "digest: secret lookup failed"}
)

func authError(kind AuthErrorKind, detail string) error {
	return fmt.Errorf("%w: %s", kindSentinel(kind), detail)
}

func kindSentinel(kind AuthErrorKind) *AuthError {
	switch kind {
	case AuthInvalidChallenge:
		return ErrInvalidChallenge
	case AuthInvalidRequest:
		return ErrInvalidRequest
	case AuthInvalidCredentials:
		return ErrInvalidCredentials
	case AuthUnknownUser:
		return ErrUnknownUser
	case AuthInvalidDigest:
		return ErrInvalidDigest
	case AuthNcReplayed:
		return ErrNcReplayed
	case AuthLookupFailed:
		return ErrLookupFailed
	default:
		return ErrInvalidRequest
	}
}

// Secret is the password or precomputed HA1 used to verify a user.
// If HA1 is set, it must be the lowercase hexadecimal SHA-256 value of
// username:realm:password. Otherwise Password is used to calculate HA1.
type Secret struct {
	Password string
	HA1      string
}

// Authentication is the result of a successful digest verification.
type Authentication struct {
	Username  string
	Realm     string
	Nonce     string
	URI       string
	Method    string
	Algorithm string
	QOP       string
	Nc        int
	Cnonce    string
}

// VerifyRequest contains a request to be verified.
//
// URI must be the actual request target (for example, http.Request.URL.RequestURI()),
// not the uri claim from the Authorization header. The caller is responsible for
// checking nonce validity. It must also provide CommitNc, which is called only
// after the response digest has been verified.
type VerifyRequest struct {
	Challenge   *Challenge
	Method      string
	URI         string
	Credentials string

	LookupSecret func(username string, realm string) (Secret, error)
	CommitNc     func(Authentication) error
}

// Verify checks a SHA-256 qop=auth digest credential. It supports only that
// algorithm and qop combination; it does not generate nonces.
func Verify(req VerifyRequest) (*Authentication, error) {
	if req.Method == "" {
		return nil, authError(AuthInvalidRequest, "missing method")
	}
	if req.URI == "" {
		return nil, authError(AuthInvalidRequest, "missing request target")
	}
	if req.LookupSecret == nil {
		return nil, authError(AuthInvalidRequest, "missing secret lookup callback")
	}
	if req.CommitNc == nil {
		return nil, authError(AuthInvalidRequest, "missing nonce count callback")
	}

	chal := req.Challenge
	if chal == nil {
		return nil, authError(AuthInvalidChallenge, "missing challenge")
	}
	if chal.Realm == "" {
		return nil, authError(AuthInvalidChallenge, "missing realm")
	}
	if chal.Nonce == "" {
		return nil, authError(AuthInvalidChallenge, "missing nonce")
	}
	if !strings.EqualFold(chal.Algorithm, "SHA-256") {
		return nil, authError(AuthInvalidChallenge, "unsupported algorithm")
	}
	if !chal.SupportsQOP("auth") {
		return nil, authError(AuthInvalidChallenge, "unsupported qop")
	}
	if chal.Userhash {
		return nil, authError(AuthInvalidChallenge, "userhash is not supported")
	}

	cred, err := ParseCredentials(req.Credentials)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed authorization header", ErrInvalidCredentials)
	}
	if cred.Username == "" {
		return nil, authError(AuthInvalidCredentials, "missing username")
	}
	if cred.Realm != chal.Realm {
		return nil, authError(AuthInvalidCredentials, "realm mismatch")
	}
	if cred.Nonce != chal.Nonce {
		return nil, authError(AuthInvalidCredentials, "nonce mismatch")
	}
	if !strings.EqualFold(cred.Algorithm, "SHA-256") {
		return nil, authError(AuthInvalidCredentials, "algorithm mismatch")
	}
	if !strings.EqualFold(strings.TrimSpace(cred.QOP), "auth") {
		return nil, authError(AuthInvalidCredentials, "qop mismatch")
	}
	if cred.URI == "" {
		return nil, authError(AuthInvalidCredentials, "missing uri")
	}
	if cred.URI != req.URI {
		return nil, authError(AuthInvalidCredentials, "uri does not match request target")
	}
	if cred.Cnonce == "" {
		return nil, authError(AuthInvalidCredentials, "missing cnonce")
	}
	if cred.Nc <= 0 {
		return nil, authError(AuthInvalidCredentials, "missing or invalid nc")
	}
	responseBytes, err := hex.DecodeString(cred.Response)
	if err != nil || len(responseBytes) != sha256.Size {
		return nil, authError(AuthInvalidCredentials, "malformed response digest")
	}
	if chal.Opaque != "" || cred.Opaque != "" {
		if cred.Opaque != chal.Opaque {
			return nil, authError(AuthInvalidCredentials, "opaque mismatch")
		}
	}
	if cred.Userhash {
		return nil, authError(AuthInvalidCredentials, "userhash is not supported")
	}

	secret, err := req.LookupSecret(cred.Username, cred.Realm)
	if err != nil {
		if errors.Is(err, ErrUnknownUser) {
			return nil, ErrUnknownUser
		}
		return nil, ErrLookupFailed
	}

	a1 := secret.HA1
	if a1 == "" {
		a1 = sha256Join(cred.Username, cred.Realm, secret.Password)
	} else {
		a1Bytes, err := hex.DecodeString(a1)
		if err != nil || len(a1Bytes) != sha256.Size {
			return nil, ErrLookupFailed
		}
	}
	a2 := sha256Join(req.Method, req.URI)
	expected := sha256Join(
		a1,
		cred.Nonce,
		fmt.Sprintf("%08x", cred.Nc),
		cred.Cnonce,
		"auth",
		a2,
	)
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(cred.Response)), []byte(expected)) != 1 {
		return nil, ErrInvalidDigest
	}

	auth := Authentication{
		Username:  cred.Username,
		Realm:     cred.Realm,
		Nonce:     cred.Nonce,
		URI:       req.URI,
		Method:    req.Method,
		Algorithm: "SHA-256",
		QOP:       "auth",
		Nc:        cred.Nc,
		Cnonce:    cred.Cnonce,
	}
	if err := req.CommitNc(auth); err != nil {
		if errors.Is(err, ErrNcReplayed) {
			return nil, ErrNcReplayed
		}
		return nil, fmt.Errorf("digest: nonce count commit failed: %w", err)
	}
	return &auth, nil
}

func sha256Join(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			h.Write([]byte{':'})
		}
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type ncKey struct {
	nonce string
	nc    int
}

// NcStore is an in-memory nonce count tracker.
type NcStore struct {
	mu   sync.Mutex
	used map[ncKey]struct{}
}

// NewNcStore creates an in-memory nonce count tracker.
func NewNcStore() *NcStore {
	return &NcStore{used: make(map[ncKey]struct{})}
}

// CommitNc atomically records a successful nonce count. The cnonce is not part
// of the replay key; reusing a nonce count with a different cnonce is still replay.
func (s *NcStore) CommitNc(auth Authentication) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == nil {
		s.used = make(map[ncKey]struct{})
	}
	key := ncKey{nonce: auth.Nonce, nc: auth.Nc}
	if _, ok := s.used[key]; ok {
		return ErrNcReplayed
	}
	s.used[key] = struct{}{}
	return nil
}

// DeleteNonce removes all counts associated with a nonce.
func (s *NcStore) DeleteNonce(nonce string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.used {
		if key.nonce == nonce {
			delete(s.used, key)
		}
	}
}
