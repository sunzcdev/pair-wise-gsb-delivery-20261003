package goproxy

import (
	"crypto/tls"
	"crypto/x509"
	"sync"
)

// CertStorage is an optional cache for host certificates signed during MITM
// interception. It is keyed only by hostname and is unaware of which CA
// signed a cached certificate.
//
// Existing implementations keep working when the signing CA is rotated:
// before a cached certificate is used, goproxy verifies it was signed by the
// active CA and transparently re-signs it otherwise. The application never
// has to clear this store for rotation to be correct.
//
// New code should implement GenerationCertStorage instead so that
// certificates of different CA generations are cached independently and old
// generations are reclaimed.
type CertStorage interface {
	Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error)
}

// GenerationCertStorage caches host certificates per signing CA generation.
// signerID uniquely identifies the active CA (see CAConfig), caLeaf is its
// leaf certificate, and gen signs a fresh host certificate with that CA.
//
// A certificate stored under one signerID must never be returned while
// serving a different signerID: if signing for an old generation finishes
// after the CA has already been replaced, the late result must not populate
// the new generation's cache.
//
// Storage is also expected to reclaim old generations so repeatedly rotating
// the CA does not accumulate one abandoned cache per rotation. Use
// NewCertStorage for the built-in implementation.
type GenerationCertStorage interface {
	FetchForSigner(
		signerID string,
		caLeaf *x509.Certificate,
		hostname string,
		gen func() (*tls.Certificate, error),
	) (*tls.Certificate, error)
}

// NewCertStorage returns the built-in generation-aware certificate cache.
// Certificates are partitioned by signing CA (signerID); when the CA is
// replaced, certificates signed by older generations are reclaimed as soon as
// the new generation starts serving, instead of accumulating forever.
// It is safe for concurrent use.
func NewCertStorage() *InMemoryCertStorage {
	return &InMemoryCertStorage{}
}

// InMemoryCertStorage is a concurrency-safe in-memory implementation of
// GenerationCertStorage.
type InMemoryCertStorage struct {
	mu sync.Mutex

	// generations maps a signerID to its hostname -> certificate cache.
	generations map[string]map[string]*tls.Certificate
	// currentSigner is the signerID of the most recently used generation.
	currentSigner string
}

// Fetch implements the legacy CertStorage interface for convenience. It is
// equivalent to FetchForSigner with an empty signerID. Code that knows the
// active CA generation should call FetchForSigner directly.
func (store *InMemoryCertStorage) Fetch(
	hostname string,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	return store.FetchForSigner("", nil, hostname, gen)
}

// FetchForSigner returns a cached certificate for hostname from the signerID
// generation, or invokes gen to sign one.
//
// The call first becomes the current generation and evicts every older
// generation. A signing operation that started before a rotation only caches
// its result if its generation is still the current one when it finishes, so
// an old CA's late result can never pollute the new CA's cache.
func (store *InMemoryCertStorage) FetchForSigner(
	signerID string,
	_ *x509.Certificate,
	hostname string,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	store.mu.Lock()
	store.becomeCurrentLocked(signerID)
	if cached, ok := store.generations[signerID][hostname]; ok {
		store.mu.Unlock()
		return cached, nil
	}
	store.mu.Unlock()

	cert, err := gen()
	if err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	// Re-check under the lock: another handshake may have signed the same
	// hostname concurrently. Prefer whichever cached first.
	if current, ok := store.generations[signerID][hostname]; ok {
		return current, nil
	}
	// This signing was started by an older generation; drop its result
	// instead of resurrecting a retired generation or polluting the new one.
	if store.currentSigner != signerID {
		return cert, nil
	}
	store.generations[signerID][hostname] = cert
	return cert, nil
}

// becomeCurrentLocked makes signerID the active generation, reclaiming all
// older generations in the process.
func (store *InMemoryCertStorage) becomeCurrentLocked(signerID string) {
	if store.generations == nil {
		store.generations = make(map[string]map[string]*tls.Certificate)
	}
	if store.currentSigner == signerID {
		if _, ok := store.generations[signerID]; !ok {
			store.generations[signerID] = make(map[string]*tls.Certificate)
		}
		return
	}
	// Reclaim every previous generation at once. Rotating once does not
	// leave a permanent extra cache behind.
	store.generations = map[string]map[string]*tls.Certificate{
		signerID: {},
	}
	store.currentSigner = signerID
}

// generationCount returns how many CA generations are currently cached. It is
// intended for tests.
func (store *InMemoryCertStorage) generationCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.generations)
}
