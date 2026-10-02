package dpop

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"sync"
	"time"
)

// NonceProvider issues and validates server nonces for the DPoP nonce
// challenge flow (RFC 9449, section 8).
//
// NewNonce returns a fresh, unpredictable nonce the server will accept until
// it expires. Verify reports whether nonce was issued and has not expired.
// A nonce may be presented on multiple requests during its lifetime; proof
// single-use rules are enforced through the "jti" claim instead.
//
// Like [ReplayStore], production deployments with multiple service instances
// should back this with shared, atomic storage (e.g. a short-lived Redis key),
// since a nonce issued by one instance may arrive at another.
type NonceProvider interface {
	NewNonce() string
	Verify(nonce string) bool
}

// NonceManager is an in-process [NonceProvider]. New nonces are valid for TTL
// and consumed on first successful use. It is safe for concurrent use but
// state is not shared across instances.
type NonceManager struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time
}

// NewNonceManager returns a [NonceManager] whose nonces live for ttl.
func NewNonceManager(ttl time.Duration) *NonceManager {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &NonceManager{
		ttl:     ttl,
		now:     time.Now,
		entries: map[string]time.Time{},
	}
}

// NewNonce implements [NonceProvider].
func (m *NonceManager) NewNonce() string {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		// rand.Reader failures are not recoverable; there is no secure way
		// to proceed without entropy.
		panic("dpop: cannot read random bytes for nonce: " + err.Error())
	}
	nonce := base64.RawURLEncoding.EncodeToString(buf)

	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(now)
	m.entries[nonce] = now.Add(m.ttl)
	return nonce
}

// Verify implements [NonceProvider]. A nonce remains valid until it expires;
// it is intentionally not consumed on use. Single-use semantics for an
// individual proof are enforced separately through its "jti" and the
// [ReplayStore] (RFC 9449, sections 4.3 and 8). The server can rotate the
// active nonce at any time by returning a [*NonceError] with a new value.
func (m *NonceManager) Verify(nonce string) bool {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(now)

	for existing := range m.entries {
		if subtle.ConstantTimeCompare([]byte(existing), []byte(nonce)) == 1 {
			return true
		}
	}
	return false
}

func (m *NonceManager) sweepLocked(now time.Time) {
	for nonce, expiresAt := range m.entries {
		if !expiresAt.After(now) {
			delete(m.entries, nonce)
		}
	}
}
