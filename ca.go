package goproxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/elazarl/goproxy/internal/signer"
)

// caGeneration is an immutable snapshot of the signing CA together with every
// cache entry created while this CA is active.
//
// Handshakes pin the generation they read from RotatingCA for their whole
// duration, so a CA replacement can never make an in-flight handshake sign
// with the wrong CA or write a freshly signed certificate into a newer
// generation's cache ("an old task finishing late cannot pollute the new cache").
type caGeneration struct {
	// generation is the monotonically increasing issuer generation number.
	// The first generation starts at 1.
	generation uint64
	ca         tls.Certificate

	// builtinCache holds signed leaf certificates for this generation when no
	// external CertStorage is configured. It is owned exclusively by this
	// generation: rotating the CA drops the whole map, so old generations do
	// not permanently leave a copy behind.
	builtinCacheMu sync.Mutex
	builtinCache   map[string]*tls.Certificate

	// store is the external CertStorage first used by this generation. It is
	// remembered so DropGeneration can be invoked once the generation retires.
	storeMu sync.Mutex
	store   CertStorage

	// inflight tracks handshakes that pinned this generation. The generation
	// is only retired after every one of them finishes, which guarantees that
	// an issuer key never disappears underneath an existing connection and
	// that caches are not reclaimed while a late signing task is still writing.
	inflight sync.WaitGroup
}

func (g *caGeneration) setStore(store CertStorage) {
	g.storeMu.Lock()
	if g.store == nil {
		g.store = store
	}
	g.storeMu.Unlock()
}

func (g *caGeneration) getStore() CertStorage {
	g.storeMu.Lock()
	defer g.storeMu.Unlock()
	return g.store
}

// legacyGenerationKey namespaces a hostname inside a CertStorage that only
// implements the legacy Fetch method, so certificates signed by different CA
// generations never share a cache slot. The NUL delimiter cannot appear in a
// real hostname.
func legacyGenerationKey(hostname string, generation uint64) string {
	return fmt.Sprintf("\x00goproxy-gen-%d\x00%s", generation, hostname)
}

// fetchCert returns a leaf certificate for hostname, signed by this
// generation's CA. When store is nil the generation's own built-in cache is
// used; otherwise the external store is called with generation-aware
// namespacing (FetchGeneration when available, a prefixed key otherwise).
func (g *caGeneration) fetchCert(hostname string, ctx *ProxyCtx, store CertStorage) (*tls.Certificate, error) {
	genCert := func() (*tls.Certificate, error) {
		return signer.SignHost(g.ca, []string{hostname})
	}

	if store == nil {
		g.builtinCacheMu.Lock()
		defer g.builtinCacheMu.Unlock()
		if cert, ok := g.builtinCache[hostname]; ok {
			return cert, nil
		}
		cert, err := genCert()
		if err != nil {
			ctx.Warnf("Cannot sign host certificate with provided CA: %s", err)
			return nil, err
		}
		g.builtinCache[hostname] = cert
		return cert, nil
	}

	g.setStore(store)
	if genStore, ok := store.(GenerationCertStorage); ok {
		return genStore.FetchGeneration(hostname, g.generation, genCert)
	}
	return store.Fetch(legacyGenerationKey(hostname, g.generation), genCert)
}

// retire waits for every handshake that pinned this generation, releases the
// built-in cache, and asks a generation-aware external store to drop the
// generation. It must only run after the generation has been swapped out of
// the RotatingCA, otherwise the WaitGroup could observe concurrent Add calls.
func (g *caGeneration) retire() {
	g.inflight.Wait()

	g.builtinCacheMu.Lock()
	clear(g.builtinCache)
	g.builtinCacheMu.Unlock()

	if store := g.getStore(); store != nil {
		if recycler, ok := store.(GenerationCertRecycler); ok {
			recycler.DropGeneration(g.generation)
		}
	}
}

// RotatingCA is an atomically replaceable signing CA for MITM interception.
//
// Assign its TLSConfig callback to a ConnectAction and call ReplaceCertificate
// (or ReplaceFromPEM) to rotate the issuer: every new TLS handshake immediately
// uses the new CA, while already established TLS connections keep using the CA
// they handshook with and continue to completion.
//
// Each successful replacement starts a new issuer generation. Certificate
// caches are partitioned per generation, so a signing task started with the
// old CA that finishes after the switch can never populate the new
// generation's cache. Replaced generations are reclaimed (including their
// cache) once the handshakes using them finish.
//
// A RotatingCA is intended to be used by a single proxy. It is safe for
// concurrent use.
type RotatingCA struct {
	mu  sync.Mutex
	cur atomic.Pointer[caGeneration]
}

// NewRotatingCA creates a RotatingCA from an already loaded CA. The CA is
// validated before use (key match and a complete, well-formed certificate
// chain); it returns an error and leaves no RotatingCA behind on failure.
func NewRotatingCA(ca *tls.Certificate) (*RotatingCA, error) {
	if ca == nil {
		return nil, errors.New("goproxy: cannot create RotatingCA from a nil CA")
	}
	rotating := &RotatingCA{}
	if err := rotating.ReplaceCertificate(*ca); err != nil {
		return nil, err
	}
	return rotating, nil
}

// NewRotatingCAFromPEM loads a CA certificate and its private key from PEM
// blocks and creates a RotatingCA. It fails if the key pair cannot be loaded
// or the resulting CA is invalid.
func NewRotatingCAFromPEM(caCertPEM, caKeyPEM []byte) (*RotatingCA, error) {
	cert, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, err
	}
	return NewRotatingCA(&cert)
}

// TLSConfig returns the ConnectAction TLSConfig callback. Every call observes
// the most recently installed generation.
func (r *RotatingCA) TLSConfig() func(host string, ctx *ProxyCtx) (*tls.Config, error) {
	return func(host string, ctx *ProxyCtx) (*tls.Config, error) {
		// Pin the generation for the whole handshake. The RLock cannot be held
		// concurrently with ReplaceCertificate's write lock, so this Add is
		// always observed by the retiring generation's WaitGroup.
		r.mu.Lock()
		generation := r.cur.Load()
		r.mu.Unlock()
		if generation == nil {
			return nil, errors.New("goproxy: RotatingCA has no active CA")
		}
		generation.inflight.Add(1)
		defer generation.inflight.Done()

		hostname := stripPort(host)
		ctx.Logf("signing for %s with CA generation %d", hostname, generation.generation)

		cert, err := generation.fetchCert(hostname, ctx, ctx.certStore)
		if err != nil {
			return nil, err
		}

		config := defaultTLSConfig.Clone()
		config.Certificates = append(config.Certificates, *cert)
		return config, nil
	}
}

// Generation returns the generation number of the currently active CA. It
// returns 0 before the first successful replacement.
func (r *RotatingCA) Generation() uint64 {
	if generation := r.cur.Load(); generation != nil {
		return generation.generation
	}
	return 0
}

// ReplaceFromPEM atomically installs a new CA loaded from PEM blocks. The new
// CA is fully validated before the swap; on any error the currently active CA
// and generation are left untouched.
func (r *RotatingCA) ReplaceFromPEM(caCertPEM, caKeyPEM []byte) error {
	cert, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		return err
	}
	return r.ReplaceCertificate(cert)
}

// ReplaceCertificate atomically installs ca as the new signing CA. The
// certificate and private key are validated (the key must match the CA
// certificate and the certificate chain must be complete and properly signed)
// before anything is swapped in, so an invalid update is rejected as a whole
// and never leaves the proxy with a half-loaded CA.
//
// New handshakes observe the new CA immediately; connections that already
// completed their handshake are unaffected.
func (r *RotatingCA) ReplaceCertificate(ca tls.Certificate) error {
	ca.Certificate = append([][]byte(nil), ca.Certificate...)
	if err := validateCA(&ca); err != nil {
		return err
	}

	r.mu.Lock()
	old := r.cur.Load()
	generationNumber := uint64(1)
	if old != nil {
		generationNumber = old.generation + 1
	}
	generation := &caGeneration{
		generation:   generationNumber,
		ca:           ca,
		builtinCache: make(map[string]*tls.Certificate),
	}
	r.cur.Store(generation)
	r.mu.Unlock()

	if old != nil {
		go old.retire()
	}
	return nil
}

// validateCA verifies that cert is a usable CA: it carries a certificate and
// a private key, it is allowed to sign certificates, the private key matches
// the leaf certificate's public key, and every certificate in the chain is
// signed by the next one, ending in a self-signed root. Any failure rejects
// the whole update.
func validateCA(cert *tls.Certificate) error {
	if cert == nil {
		return errors.New("goproxy: CA certificate is nil")
	}
	if len(cert.Certificate) == 0 {
		return errors.New("goproxy: CA has no certificate")
	}
	if cert.PrivateKey == nil {
		return errors.New("goproxy: CA has no private key")
	}

	parsed := make([]*x509.Certificate, len(cert.Certificate))
	for i, der := range cert.Certificate {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("goproxy: cannot parse CA certificate #%d: %w", i, err)
		}
		parsed[i] = c
	}

	leaf := parsed[0]
	if !leaf.IsCA {
		return errors.New("goproxy: leaf certificate is not a certificate authority allowed to sign certificates")
	}

	if err := validateKeyMatch(leaf, cert.PrivateKey); err != nil {
		return err
	}

	for i := 1; i < len(parsed); i++ {
		if err := parsed[i-1].CheckSignatureFrom(parsed[i]); err != nil {
			return fmt.Errorf("goproxy: incomplete or invalid CA chain: certificate #%d is not signed by certificate #%d: %w", i-1, i, err)
		}
	}
	root := parsed[len(parsed)-1]
	if err := root.CheckSignatureFrom(root); err != nil {
		return errors.New("goproxy: incomplete CA chain: last certificate is not a self-signed root")
	}

	cert.Leaf = leaf
	return nil
}

func validateKeyMatch(cert *x509.Certificate, key crypto.PrivateKey) error {
	mismatch := errors.New("goproxy: CA private key does not match the CA certificate")
	switch k := key.(type) {
	case *rsa.PrivateKey:
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok || pub.N.Cmp(k.N) != 0 || pub.E != k.E {
			return mismatch
		}
	case *ecdsa.PrivateKey:
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || !pub.Equal(k.Public()) {
			return mismatch
		}
	case ed25519.PrivateKey:
		pub, ok := cert.PublicKey.(ed25519.PublicKey)
		if !ok || !pub.Equal(k.Public()) {
			return mismatch
		}
	default:
		signerKey, ok := key.(crypto.Signer)
		if !ok {
			return fmt.Errorf("goproxy: unsupported CA private key type %T", key)
		}
		expected, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
		if err != nil {
			return fmt.Errorf("goproxy: cannot marshal CA public key: %w", err)
		}
		actual, err := x509.MarshalPKIXPublicKey(signerKey.Public())
		if err != nil {
			return fmt.Errorf("goproxy: cannot marshal CA private key's public key: %w", err)
		}
		if !equalBytes(expected, actual) {
			return mismatch
		}
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
