package dpop

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NonceIssuer mints and validates the opaque DPoP nonces used by the
// use_dpop_nonce challenge of RFC 9449 section 8.
//
// A nonce binds a challenge to an expiry instant. It is an HMAC-signed,
// self-contained token, so no server-side state is required: every instance
// of a service that shares the secret accepts each other's nonces. The secret
// MUST be generated with a cryptographically secure random source and kept
// private; use at least 32 bytes.
type NonceIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewNonceIssuer returns a [NonceIssuer] that accepts nonces for ttl after
// they are issued. All cooperating service instances must be configured with
// the same secret and equivalent ttl.
func NewNonceIssuer(secret []byte, ttl time.Duration) (*NonceIssuer, error) {
	if len(secret) < 32 {
		return nil, errors.New("dpop: nonce issuer secret must be at least 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("dpop: nonce ttl must be positive")
	}
	secretCopy := append([]byte(nil), secret...)
	return &NonceIssuer{
		secret: secretCopy,
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

// Issue returns a fresh nonce for use in a DPoP-Nonce response header.
func (n *NonceIssuer) Issue() string {
	return n.issueAt(n.now())
}

func (n *NonceIssuer) issueAt(now time.Time) string {
	expiry := now.Add(n.ttl).Unix()

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		// rand.Read failing is not recoverable; fail closed rather than
		// issuing a predictable nonce.
		panic(fmt.Sprintf("dpop: generating nonce randomness: %v", err))
	}

	payload := strconv.FormatInt(expiry, 10) + "." +
		base64.RawURLEncoding.EncodeToString(random)
	mac := n.sign(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac)
}

// Check validates a nonce previously produced by [NonceIssuer.Issue] on an
// issuer configured with the same secret. It rejects malformed, tampered, or
// expired nonces.
func (n *NonceIssuer) Check(nonce string) error {
	parts := strings.Split(nonce, ".")
	if len(parts) != 2 {
		return errors.New("dpop: malformed server nonce")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("dpop: malformed server nonce")
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("dpop: malformed server nonce")
	}

	expected := n.sign(string(payload))
	if subtle.ConstantTimeCompare(mac, expected) != 1 {
		return errors.New("dpop: invalid server nonce signature")
	}

	fields := strings.SplitN(string(payload), ".", 2)
	if len(fields) != 2 {
		return errors.New("dpop: malformed server nonce")
	}
	expiry, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return errors.New("dpop: malformed server nonce")
	}

	if time.Unix(expiry, 0).Before(n.now()) {
		return errors.New("dpop: server nonce expired")
	}
	return nil
}

func (n *NonceIssuer) sign(payload string) []byte {
	mac := hmac.New(sha256.New, n.secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
