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

// Failure classifies why a digest authentication attempt was rejected.
type Failure int

const (
	FailureUnknown Failure = iota
	FailureMalformed
	FailureUnsupported
	FailureInvalidNonce
	FailureUnknownUser
	FailureBadDigest
	FailureReplay
)

// AuthError is returned for failed authentication attempts.
//
// It never embeds the password, the HA1 value or any other user secret, so it
// is safe to log or surface to callers. Match the failure class with Failure
// or use errors.Is with the package sentinel errors.
type AuthError struct {
	Reason  Failure
	message string
}

func (e *AuthError) Error() string {
	return "digest: " + e.message
}

// Is treats two AuthErrors as equal when they share the same Reason, which
// makes every classified failure match its package-level sentinel.
func (e *AuthError) Is(target error) bool {
	te, ok := target.(*AuthError)
	return ok && e.Reason == te.Reason
}

func authError(reason Failure, msg string) *AuthError {
	return &AuthError{Reason: reason, message: msg}
}

// Sentinel errors for the controllable failure classes. Every *AuthError
// matches the sentinel corresponding to its Failure value.
var (
	ErrMalformed    = authError(FailureMalformed, "malformed credentials")
	ErrUnsupported  = authError(FailureUnsupported, "unsupported digest parameters")
	ErrInvalidNonce = authError(FailureInvalidNonce, "invalid nonce")
	ErrUnknownUser  = authError(FailureUnknownUser, "unknown user")
	ErrBadDigest    = authError(FailureBadDigest, "digest mismatch")
	ErrReplay       = authError(FailureReplay, "nonce count replay")
)

// Secret is the caller's side of the credential lookup.
//
// Set either Password or HA1, not both. If HA1 is set, it must be the
// hex-encoded SHA-256 hash of "username:realm:password".
type Secret struct {
	Password string
	HA1      string
}

// LookupFunc resolves a user to their password or precomputed HA1.
//
// Return ErrUnknownUser for unknown users. Any other error is reported
// verbatim without being treated as an authentication failure.
type LookupFunc func(username, realm string) (Secret, error)

// CounterStore tracks used nonce counts. A successful CheckAndClaim atomically
// records nc for (nonce, username), guaranteeing that concurrent attempts using
// the same nc can succeed at most once.
type CounterStore interface {
	CheckAndClaim(nonce, username string, nc int) (bool, error)
}

// MemoryCounterStore is an in-process CounterStore safe for concurrent use.
// It keeps no background state and needs no external storage.
type MemoryCounterStore struct {
	mu      sync.Mutex
	claimed map[string]int
}

// NewMemoryCounterStore creates an empty MemoryCounterStore.
func NewMemoryCounterStore() *MemoryCounterStore {
	return &MemoryCounterStore{claimed: make(map[string]int)}
}

// CheckAndClaim claims nc for the (nonce, username) pair if it has not been
// used before.
func (s *MemoryCounterStore) CheckAndClaim(nonce, username string, nc int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := nonce + "\x00" + username
	last, ok := s.claimed[key]
	if ok && nc <= last {
		return false, nil
	}
	s.claimed[key] = nc
	return true, nil
}

// Reset clears all recorded nonce counts.
func (s *MemoryCounterStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.claimed)
}

// NonceChecker reports whether a nonce is currently valid (issued, unexpired
// and not otherwise revoked). The validator does not generate nonces and does
// not implement expiry itself.
type NonceChecker func(nonce string) error

// ServerValidator verifies digest authentication responses.
//
// It is restricted to qop=auth and SHA-256. It never generates nonces:
// nonce validity is delegated to CheckNonce, and nonce-count replay protection
// is delegated to Counts. The nonce count is committed atomically only after
// the response digest has been verified.
type ServerValidator struct {
	// Lookup resolves usernames to their password or HA1.
	Lookup LookupFunc
	// CheckNonce validates the nonce. If nil, any non-empty nonce is accepted.
	CheckNonce NonceChecker
	// Counts records used nonce counts. Required.
	Counts CounterStore
}

// Result is a successfully authenticated response.
type Result struct {
	Username string
	Realm    string
	Nonce    string
	URI      string
	Nc       int
	Cnonce   string
}

// Validate verifies a digest Authorization header against the challenge that
// was issued for this request.
//
// method is the actual HTTP method and targetURI is the actual request target
// (e.g. req.URL.RequestURI()). They are authoritative: the uri claimed inside
// the credentials is checked against them and never used in their place.
func (v *ServerValidator) Validate(chal *Challenge, method, targetURI, authorization string) (*Result, error) {
	if v == nil || v.Lookup == nil || v.Counts == nil {
		return nil, errors.New("digest: validator not configured")
	}
	if chal == nil {
		return nil, errors.New("digest: missing challenge")
	}
	// Only qop=auth with SHA-256 is supported by this validator.
	if !strings.EqualFold(chal.Algorithm, "SHA-256") {
		return nil, authError(FailureUnsupported, fmt.Sprintf("unsupported algorithm: %q", chal.Algorithm))
	}
	if !chal.SupportsQOP("auth") {
		return nil, authError(FailureUnsupported, "challenge does not offer qop=auth")
	}
	if chal.Realm == "" {
		return nil, authError(FailureMalformed, "challenge missing realm")
	}
	if chal.Nonce == "" {
		return nil, authError(FailureMalformed, "challenge missing nonce")
	}
	// The nonce is owned by the caller: validate (issued/unexpired) up front.
	if v.CheckNonce != nil {
		if err := v.CheckNonce(chal.Nonce); err != nil {
			var ae *AuthError
			if errors.As(err, &ae) {
				return nil, err
			}
			return nil, authError(FailureInvalidNonce, "rejected nonce")
		}
	}
	// Parse and structurally validate the credentials header.
	cred, err := ParseCredentials(authorization)
	if err != nil {
		return nil, authError(FailureMalformed, err.Error())
	}
	if cred.Username == "" {
		return nil, authError(FailureMalformed, "credentials missing username")
	}
	if cred.Nonce == "" {
		return nil, authError(FailureMalformed, "credentials missing nonce")
	}
	if cred.URI == "" {
		return nil, authError(FailureMalformed, "credentials missing uri")
	}
	if cred.Response == "" {
		return nil, authError(FailureMalformed, "credentials missing response")
	}
	if cred.Cnonce == "" {
		return nil, authError(FailureMalformed, "credentials missing cnonce")
	}
	if cred.Nc <= 0 {
		return nil, authError(FailureMalformed, "credentials missing or invalid nc")
	}
	if cred.QOP != "auth" {
		return nil, authError(FailureUnsupported, fmt.Sprintf("unsupported qop: %q", cred.QOP))
	}
	if !strings.EqualFold(cred.Algorithm, "SHA-256") {
		return nil, authError(FailureUnsupported, fmt.Sprintf("unsupported algorithm: %q", cred.Algorithm))
	}
	if cred.Userhash {
		return nil, authError(FailureUnsupported, "userhash is not supported")
	}
	// The credentials must answer the exact challenge that was issued.
	if cred.Nonce != chal.Nonce {
		return nil, authError(FailureInvalidNonce, "credentials nonce does not match challenge")
	}
	if cred.Realm != chal.Realm {
		return nil, authError(FailureMalformed, "credentials realm does not match challenge")
	}
	if chal.Opaque != "" && cred.Opaque != chal.Opaque {
		return nil, authError(FailureMalformed, "credentials opaque does not match challenge")
	}
	// The claimed uri must match the actual request target. It is never used
	// as a substitute for the real target, which prevents uri-confusion.
	if targetURI == "" || cred.URI != targetURI {
		return nil, authError(FailureMalformed, "credentials uri does not match request target")
	}
	// Resolve the account. Unknown users are a distinct failure class.
	secret, err := v.Lookup(cred.Username, cred.Realm)
	if err != nil {
		var ae *AuthError
		if errors.As(err, &ae) && ae.Reason == FailureUnknownUser {
			return nil, ErrUnknownUser
		}
		return nil, err
	}
	// Build HA1 from the password when the caller did not precompute it.
	ha1 := secret.HA1
	if ha1 == "" {
		h := sha256.New()
		fmt.Fprintf(h, "%s:%s:%s", cred.Username, cred.Realm, secret.Password)
		ha1 = hex.EncodeToString(h.Sum(nil))
	}
	// Recompute the expected response from the actual method and target.
	h := sha256.New()
	ha2 := hashjoin(h, method, targetURI)
	expected := hashjoin(h,
		ha1,
		chal.Nonce,
		fmt.Sprintf("%08x", cred.Nc),
		cred.Cnonce,
		"auth",
		ha2,
	)
	if subtle.ConstantTimeCompare([]byte(cred.Response), []byte(expected)) != 1 {
		return nil, ErrBadDigest
	}
	// Digest verified: claim the nonce count atomically. The first concurrent
	// attempt for a given nc wins; losers and any smaller reused nc are
	// reported as replays.
	ok, err := v.Counts.CheckAndClaim(chal.Nonce, cred.Username, cred.Nc)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrReplay
	}
	return &Result{
		Username: cred.Username,
		Realm:    cred.Realm,
		Nonce:    cred.Nonce,
		URI:      cred.URI,
		Nc:       cred.Nc,
		Cnonce:   cred.Cnonce,
	}, nil
}
