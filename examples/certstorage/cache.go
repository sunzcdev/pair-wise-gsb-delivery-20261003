package main

import (
	"crypto/tls"
	"crypto/x509"

	"github.com/elazarl/goproxy"
)

// CertStorage caches host certificates per signing-CA generation (signerID).
// Certificates signed by an older CA are kept in a separate map from the new
// CA's certificates, so rotating the CA never serves a stale leaf and never
// leaves one abandoned cache behind per rotation.
type CertStorage struct {
	// Implement goproxy.GenerationCertStorage by delegating to the built-in
	// generation-aware store. You could instead implement FetchForSigner
	// yourself (e.g. backed by Redis or disk).
	store *goproxy.InMemoryCertStorage
}

func NewCertStorage() *CertStorage {
	return &CertStorage{store: goproxy.NewCertStorage()}
}

// FetchForSigner implements goproxy.GenerationCertStorage.
func (cs *CertStorage) FetchForSigner(
	signerID string,
	caLeaf *x509.Certificate,
	hostname string,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	return cs.store.FetchForSigner(signerID, caLeaf, hostname, gen)
}

// Fetch keeps CertStorage compatible with code that only knows the legacy
// goproxy.CertStorage interface. goproxy prefers FetchForSigner when both are
// present, so this method is only used by older callers.
func (cs *CertStorage) Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	return cs.store.Fetch(hostname, gen)
}
