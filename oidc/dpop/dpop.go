package dpop

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Header is the HTTP request header that carries the DPoP proof JWT.
const Header = "DPoP"

// NonceHeader is the HTTP response header that carries a server-issued DPoP
// nonce, and the request header clients echo it back in.
const NonceHeader = "DPoP-Nonce"

// ProofType is the required value of the JWS "typ" header for DPoP proofs.
const ProofType = "dpop+jwt"

// maxJTI is a generous upper bound on the length of a proof identifier,
// preventing an unbounded attacker-controlled string from entering the replay
// store.
const maxJTI = 1024

// defaultSigningAlgs are the asymmetric JWS algorithms RFC 9449 section 4.2
// permits for DPoP proofs. Symmetric algorithms and the "none" algorithm are
// never accepted: the proof key must be an asymmetric pair so that the token
// binding is meaningful.
var defaultSigningAlgs = []jose.SignatureAlgorithm{
	jose.RS256,
	jose.RS384,
	jose.RS512,
	jose.PS256,
	jose.PS384,
	jose.PS512,
	jose.ES256,
	jose.ES384,
	jose.ES512,
	jose.EdDSA,
}

// Config configures a [Verifier].
type Config struct {
	// ReplayStore records proof identifiers. It is required; deploy a shared
	// implementation to protect more than one service instance.
	ReplayStore ReplayStore

	// Nonce, if set, is the issuer whose nonces must be present in a proof's
	// "nonce" claim. A missing or invalid nonce produces a use_dpop_nonce
	// challenge carrying the current nonce. Leave nil to disable the nonce
	// mechanism.
	Nonce *NonceIssuer

	// TimeWindow is the maximum acceptable absolute difference between the
	// proof's "iat" and the server's current time. Defaults to 5 minutes,
	// matching the window recommended by RFC 9449 section 4.3.
	TimeWindow time.Duration

	// SupportedSigningAlgs restricts the JWS algorithms accepted in the
	// proof's "alg" header. Defaults to the asymmetric algorithms allowed by
	// RFC 9449. Values are JWT "alg" identifiers such as "ES256" or "RS256".
	SupportedSigningAlgs []string

	// Now returns the current time. Defaults to time.Now. Overriding it is
	// mainly useful in tests.
	Now func() time.Time
}

// VerifyParams are the inputs to [Verifier.Verify].
type VerifyParams struct {
	// Proof is the raw DPoP proof JWT, as received in the DPoP header.
	Proof string

	// Method is the HTTP method of the request; the "htm" claim is compared
	// to it using the case-sensitive token form (e.g. "GET").
	Method string

	// TargetURI is the absolute target URI of the request; the "htu" claim is
	// compared to its scheme, host and path, without query or fragment.
	TargetURI string

	// AccessToken is the raw access token presented in the request's
	// Authorization header. When set, the proof must carry an "ath" claim
	// equal to the base64url SHA-256 hash of this value, and Binding must be
	// set. Leave both unset for proofs presented at the token endpoint, which
	// do not bind to an access token.
	AccessToken string

	// Binding is the confirmation key that AccessToken was independently
	// verified to carry. It must never be derived from an unverified token:
	// see [BindingFromClaims] and [BindingFromIDToken].
	Binding *TokenBinding

	// ExpectedNonce, if non-empty, requires the proof to carry this exact
	// server nonce in its "nonce" claim. Set it to the DPoP-Nonce request
	// header value. A mismatch is reported as a use_dpop_nonce challenge
	// rather than an invalid proof, per RFC 9449 section 8.
	ExpectedNonce string
}

// Proof is the result of a successful DPoP validation.
type Proof struct {
	// JTI is the proof's unique identifier ("jti" claim).
	JTI string
	// IssuedAt is the proof's "iat" claim.
	IssuedAt time.Time
	// Key is the public key embedded in the proof header.
	Key jose.JSONWebKey
	// KeyThumbprint is the base64url-encoded SHA-256 JWK Thumbprint of Key.
	KeyThumbprint string
	// Nonce is the server nonce the proof carried, if any.
	Nonce string
}

// Verifier validates DPoP proofs. It is safe for concurrent use.
type Verifier struct {
	store  ReplayStore
	nonce  *NonceIssuer
	window time.Duration
	algs   []jose.SignatureAlgorithm
	now    func() time.Time
}

// NewVerifier returns a [Verifier] using the given configuration. It returns
// an error if the config lacks a [ReplayStore] or names a signing algorithm
// that is not one of the asymmetric algorithms DPoP permits.
func NewVerifier(config *Config) (*Verifier, error) {
	if config == nil || config.ReplayStore == nil {
		return nil, errors.New("dpop: NewVerifier requires a ReplayStore")
	}

	now := config.Now
	if now == nil {
		now = time.Now
	}

	window := config.TimeWindow
	if window <= 0 {
		window = 5 * time.Minute
	}

	algs := defaultSigningAlgs
	if len(config.SupportedSigningAlgs) != 0 {
		algs = make([]jose.SignatureAlgorithm, 0, len(config.SupportedSigningAlgs))
		for _, alg := range config.SupportedSigningAlgs {
			sa := jose.SignatureAlgorithm(alg)
			if !slices.Contains(defaultSigningAlgs, sa) {
				return nil, fmt.Errorf("dpop: unsupported DPoP signing algorithm %q", alg)
			}
			algs = append(algs, sa)
		}
	}

	return &Verifier{
		store:  config.ReplayStore,
		nonce:  config.Nonce,
		window: window,
		algs:   algs,
		now:    now,
	}, nil
}

// VerifyRequest validates the DPoP proof carried in an HTTP request.
//
// The target URI is reconstructed from the request per RFC 9449 section 4.3,
// so callers behind a reverse proxy must ensure the request scheme and host
// reflect what the client used (the method honors X-Forwarded-Proto and
// X-Forwarded-Host).
//
// accessToken is the raw token without its "DPoP " scheme prefix. When it is
// non-empty, binding must be the confirmation key established while verifying
// that token.
func (v *Verifier) VerifyRequest(ctx context.Context, r *http.Request, accessToken string, binding *TokenBinding) (*Proof, error) {
	values, ok := r.Header[http.CanonicalHeaderKey(Header)]
	if !ok || len(values) == 0 {
		return nil, proofError("missing DPoP header")
	}
	if len(values) > 1 {
		return nil, proofError("multiple DPoP headers")
	}

	return v.Verify(ctx, VerifyParams{
		Proof:         values[0],
		Method:        r.Method,
		TargetURI:     targetURI(r),
		AccessToken:   accessToken,
		Binding:       binding,
		ExpectedNonce: r.Header.Get(NonceHeader),
	})
}

// proofClaims is the DPoP proof payload, RFC 9449 section 4.2.
type proofClaims struct {
	JTI   string   `json:"jti"`
	HTM   string   `json:"htm"`
	HTU   string   `json:"htu"`
	IAT   *float64 `json:"iat"`
	ATH   string   `json:"ath,omitempty"`
	Nonce string   `json:"nonce,omitempty"`
}

// Verify validates a proof from its constituent parameters. It is the
// transport-independent core used by [Verifier.VerifyRequest].
func (v *Verifier) Verify(ctx context.Context, params VerifyParams) (*Proof, error) {
	if params.Proof == "" {
		return nil, proofError("missing DPoP proof")
	}
	if params.Method == "" {
		return nil, proofError("missing HTTP method")
	}
	target, err := canonicalTarget(params.TargetURI)
	if err != nil {
		return nil, proofError("invalid target URI: %v", err)
	}
	if (params.AccessToken == "") != (params.Binding == nil) {
		return nil, errors.New("dpop: AccessToken and Binding must be provided together")
	}

	jws, err := jose.ParseSignedCompact(params.Proof, v.algs)
	if err != nil {
		return nil, proofError("malformed proof or unsupported signature algorithm: %v", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, proofError("proof must have exactly one signature")
	}
	signature := jws.Signatures[0]

	// RFC 9449 section 4.2: typ and jwk are protected-header parameters.
	typ, _ := signature.Protected.ExtraHeaders[jose.HeaderType].(string)
	if typ != ProofType {
		return nil, proofError("proof typ header must be %q, got %q", ProofType, typ)
	}
	headerKey := signature.Protected.JSONWebKey
	if headerKey == nil || !headerKey.Valid() || !headerKey.IsPublic() {
		return nil, proofError("proof header must carry a valid asymmetric public jwk")
	}

	// Verify the signature against the embedded key before trusting the
	// payload: a valid signature is what makes that key the holder of the key
	// the proof represents.
	payloadBytes, err := jws.Verify(headerKey)
	if err != nil {
		return nil, proofError("invalid proof signature: %v", err)
	}

	var claims proofClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, proofError("malformed proof claims: %v", err)
	}
	if claims.JTI == "" {
		return nil, proofError("proof missing jti claim")
	}
	if len(claims.JTI) > maxJTI {
		return nil, proofError("proof jti claim exceeds %d bytes", maxJTI)
	}

	// The nonce check runs before method/URI/time so that a stale nonce is
	// answered with a fresh challenge instead of a generic failure.
	if v.nonce != nil {
		if challenge := v.checkNonce(claims.Nonce, params.ExpectedNonce); challenge != nil {
			return nil, challenge
		}
	}

	if claims.HTM != params.Method {
		return nil, proofError("htm mismatch: proof claims %q, request is %q", claims.HTM, params.Method)
	}
	htu, err := canonicalTarget(claims.HTU)
	if err != nil {
		return nil, proofError("invalid htu claim: %v", err)
	}
	if htu != target {
		return nil, proofError("htu mismatch: proof claims %q, request targets %q", claims.HTU, params.TargetURI)
	}
	if claims.IAT == nil {
		return nil, proofError("proof missing iat claim")
	}
	issuedAt := time.Unix(0, int64(*claims.IAT*float64(time.Second)))
	if delta := v.now().Sub(issuedAt); delta > v.window || delta < -v.window {
		return nil, proofError("proof iat %s is outside the %s time window",
			issuedAt.UTC().Format(time.RFC3339), v.window)
	}

	thumbprint, err := keyThumbprint(*headerKey)
	if err != nil {
		return nil, proofError("computing jwk thumbprint: %v", err)
	}

	if params.AccessToken != "" {
		// ath binds the proof to this particular access token.
		if subtle.ConstantTimeCompare([]byte(claims.ATH), []byte(AccessTokenHash(params.AccessToken))) != 1 {
			return nil, proofError("ath does not match the presented access token")
		}
		// The thumbprint must equal the cnf.jkt the token was *verified* to
		// carry; an unverified cnf must never reach this comparison.
		if subtle.ConstantTimeCompare([]byte(thumbprint), []byte(params.Binding.JWKThumbprint)) != 1 {
			return nil, proofError("proof key thumbprint does not match the access token binding")
		}
	}

	// Record the jti only after every other check passed, and make
	// check-and-set the last step so a duplicate proof cannot also pass any
	// other validation path. The store operation is atomic across instances.
	ttl := 2 * v.window
	if ttl < v.window {
		ttl = v.window
	}
	added, err := v.store.Add(ctx, claims.JTI, ttl)
	if err != nil {
		if errors.Is(err, ErrReplay) {
			return nil, proofError("proof jti %q has already been used", claims.JTI)
		}
		return nil, fmt.Errorf("dpop: recording proof identifier: %w", err)
	}
	if !added {
		return nil, proofError("proof jti %q has already been used", claims.JTI)
	}

	return &Proof{
		JTI:           claims.JTI,
		IssuedAt:      issuedAt,
		Key:           *headerKey,
		KeyThumbprint: thumbprint,
		Nonce:         claims.Nonce,
	}, nil
}

func (v *Verifier) checkNonce(proofNonce, expectedNonce string) *Error {
	switch {
	case proofNonce == "":
		return nonceError(v.nonce.Issue(), nil, "proof missing server nonce")
	case expectedNonce != "" &&
		subtle.ConstantTimeCompare([]byte(proofNonce), []byte(expectedNonce)) != 1:
		return nonceError(v.nonce.Issue(), nil,
			"proof nonce does not match the nonce required for this request")
	}
	if err := v.nonce.Check(proofNonce); err != nil {
		return nonceError(v.nonce.Issue(), err, "proof carries an invalid server nonce")
	}
	return nil
}

func canonicalTarget(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}

	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" &&
		!((u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443")) {
		host = net.JoinHostPort(host, port)
	}

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}

	// RFC 9449 section 4.3 compares scheme, host, and path only; query and
	// fragment are not part of the htu comparison.
	return u.Scheme + "://" + host + path, nil
}

func targetURI(r *http.Request) string {
	if r.URL.IsAbs() {
		return r.URL.String()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0]))
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

// AccessTokenHash returns the base64url-encoded SHA-256 hash of an access
// token: the value required in the DPoP proof's "ath" claim.
func AccessTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func keyThumbprint(key jose.JSONWebKey) (string, error) {
	thumbprint, err := key.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(thumbprint), nil
}
