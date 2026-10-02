package dpop

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/subtle"
	"net"
	"net/url"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// defaultProofMaxAge mirrors the access-token lifetime guidance of
// RFC 9449, section 11.1: a proof should be used almost immediately.
const (
	defaultProofMaxAge = 5 * time.Minute
	defaultLeeway      = 1 * time.Minute
)

// supportedProofAlgs are the asymmetric algorithms permitted for DPoP
// proofs. Symmetric ("MAC") and "none" algorithms are never allowed because a
// proof must demonstrate possession of a private key (RFC 9449, section 4.2).
var supportedProofAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// Config configures a [Verifier]. The zero value is not usable; construct one
// with [NewVerifier].
type Config struct {
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time

	// MaxAge is the maximum acceptable age of a proof's "iat" claim.
	// Defaults to 5 minutes. RFC 9449 recommends a very short window.
	MaxAge time.Duration

	// Leeway tolerates clock skew when checking "iat". Defaults to 1 minute.
	Leeway time.Duration

	// ReplayStore atomically records proof identifiers. Defaults to an
	// in-process [MemoryReplayStore], which only protects a single instance.
	// Multi-instance deployments must supply a shared backend (see the
	// [ReplayStore] contract).
	ReplayStore ReplayStore

	// ReplayTTL is how long a jti is retained. It should cover the full
	// acceptance window (MaxAge + Leeway, including clock skew). Defaults to
	// MaxAge + 2*Leeway.
	ReplayTTL time.Duration

	// Nonces, when non-nil, enforces RFC 9449, section 8 server-issued
	// nonces: proofs must present a currently valid nonce, and failures
	// return a [*NonceError] carrying a fresh challenge. When nil, nonce
	// claims are accepted but not required.
	Nonces NonceProvider
}

// Verifier checks DPoP proofs per RFC 9449, sections 4.2 and 4.3. It is safe
// for concurrent use.
type Verifier struct {
	now        func() time.Time
	maxAge     time.Duration
	leeway     time.Duration
	replays    ReplayStore
	replayTTL  time.Duration
	nonces     NonceProvider
	allowedAlg []jose.SignatureAlgorithm
}

// NewVerifier constructs a [Verifier].
func NewVerifier(config *Config) *Verifier {
	if config == nil {
		config = &Config{}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	maxAge := config.MaxAge
	if maxAge <= 0 {
		maxAge = defaultProofMaxAge
	}
	leeway := config.Leeway
	if leeway <= 0 {
		leeway = defaultLeeway
	}
	replays := config.ReplayStore
	if replays == nil {
		replays = NewMemoryReplayStore()
	}
	replayTTL := config.ReplayTTL
	if replayTTL <= 0 {
		replayTTL = maxAge + 2*leeway
	}
	return &Verifier{
		now:        now,
		maxAge:     maxAge,
		leeway:     leeway,
		replays:    replays,
		replayTTL:  replayTTL,
		nonces:     config.Nonces,
		allowedAlg: supportedProofAlgs,
	}
}

// Verify validates a single DPoP proof.
//
//   - rawProof is the exact value of the DPoP request header.
//   - req binds the proof to the current HTTP method and target URI.
//   - accessToken is the raw access token presented in the Authorization
//     header. Its SHA-256 hash must match the proof's "ath" claim.
//   - binding is the confirmation claim the caller extracted from the access
//     token AFTER independently verifying that token. The proof's key
//     thumbprint must equal binding.JKT. It is never taken from the proof.
//
// On success the proof's jti has been atomically recorded: presenting the
// same proof again fails with [ErrReplayed].
func (v *Verifier) Verify(ctx context.Context, rawProof string, req Request, accessToken string, binding Binding) (*Proof, error) {
	if !binding.IsDPoP() {
		return nil, &bindingError{message: "dpop: access token has no verified cnf.jkt binding"}
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, &bindingError{message: "dpop: DPoP-bound request must present an access token"}
	}

	token, claims, err := parseProof(rawProof, v.allowedAlg)
	if err != nil {
		return nil, err
	}
	jwk, err := verifyHeader(token.Headers[0])
	if err != nil {
		return nil, err
	}

	// The signature is checked against the key embedded in the proof header.
	// Possession of the matching private key is therefore proven; trust in
	// that key for THIS request comes only from the access token binding
	// checked below.
	if err := token.Claims(jwk.Key, claims); err != nil {
		return nil, &invalidSignatureError{err: err}
	}

	now := v.now()
	if err := v.validateClaims(claims, req, accessToken, now); err != nil {
		return nil, err
	}

	thumbprint, err := jwkThumbprint(jwk)
	if err != nil {
		return nil, err
	}
	if !secureEqual(thumbprint, binding.JKT) {
		return nil, ErrKeyBindingMismatch
	}

	if v.nonces != nil {
		if claims.Nonce == "" {
			return nil, &NonceError{Err: ErrNonceRequired, Nonce: v.nonces.NewNonce()}
		}
		if !v.nonces.Verify(claims.Nonce) {
			return nil, &NonceError{Err: ErrNonceInvalid, Nonce: v.nonces.NewNonce()}
		}
	}

	// Replay protection is last: a challenge for a fresh nonce must not burn
	// the proof's single-use jti, since the client legitimately retries with
	// the same jti.
	seen, err := v.replays.CheckAndStore(ctx, claims.JTI, v.replayTTL)
	if err != nil {
		return nil, err
	}
	if seen {
		return nil, ErrReplayed
	}

	return &Proof{
		JTI:        claims.JTI,
		Thumbprint: thumbprint,
		Nonce:      claims.Nonce,
		IssuedAt:   claims.IAT.Time(),
	}, nil
}

// canonicalURI normalizes an htu/target URI per RFC 9449, section 4.3: an
// absolute http(s) URL compared without query or fragment, with a
// case-normalized scheme/host and implicit default ports removed. An empty
// path is treated as "/".
func canonicalURI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", &uriError{uri: raw}
	}
	hostname, port := splitHostPort(strings.ToLower(u.Host))
	host := hostname
	if port != "" && !((scheme == "http" && port == "80") || (scheme == "https" && port == "443")) {
		host = net.JoinHostPort(hostname, port)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path, nil
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func isPublicKey(key any) bool {
	switch key.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		return true
	default:
		return false
	}
}

// splitHostPort separates a lowercased url.URL.Host into host and port.
// url.Parse always brackets IPv6 literals, so an unbracketed colon marks the
// port. Port is empty when absent.
func splitHostPort(hostport string) (host, port string) {
	if hostport == "" {
		return "", ""
	}
	if strings.HasPrefix(hostport, "[") {
		if h, p, err := net.SplitHostPort(hostport); err == nil {
			return h, p
		}
		return strings.TrimPrefix(hostport[:strings.Index(hostport, "]")], "["), ""
	}
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		return hostport[:i], hostport[i+1:]
	}
	return hostport, ""
}

type uriError struct{ uri string }

func (e *uriError) Error() string { return "invalid absolute http(s) URI: " + e.uri }

// bindingError covers configuration-level misuse of the verifier.
type bindingError struct{ message string }

func (e *bindingError) Error() string { return e.message }

type invalidSignatureError struct{ err error }

func (e *invalidSignatureError) Error() string {
	return ErrInvalidSignature.Error() + ": " + e.err.Error()
}
func (e *invalidSignatureError) Unwrap() error { return ErrInvalidSignature }
