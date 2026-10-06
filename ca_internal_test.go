package goproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy/internal/signer"
)

func makeTestCA(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
}

func TestPrepareCARejectsInvalid(t *testing.T) {
	ca := makeTestCA(t, "valid CA")

	t.Run("nil certificate", func(t *testing.T) {
		if _, err := prepareCA(nil); err == nil {
			t.Fatal("expected error for nil CA")
		}
	})

	t.Run("missing private key", func(t *testing.T) {
		noKey := ca
		noKey.PrivateKey = nil
		if _, err := prepareCA(&noKey); err == nil {
			t.Fatal("expected error for missing private key")
		}
	})

	t.Run("private key mismatch", func(t *testing.T) {
		other := makeTestCA(t, "other CA")
		mismatched := ca
		mismatched.PrivateKey = other.PrivateKey
		if _, err := prepareCA(&mismatched); err == nil {
			t.Fatal("expected error when private key does not match leaf")
		}
	})

	t.Run("leaf is not a CA", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: "not a CA"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca.Leaf, &key.PublicKey, ca.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(der)
		leafCert := tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        leaf,
		}
		if _, err := prepareCA(&leafCert); err == nil {
			t.Fatal("expected error for a leaf that is not a CA")
		}
	})

	t.Run("incomplete chain", func(t *testing.T) {
		// A leaf CA cert followed by an unrelated certificate: not signed by it.
		other := makeTestCA(t, "unrelated CA")
		incomplete := ca
		incomplete.Certificate = [][]byte{ca.Certificate[0], other.Certificate[0]}
		if _, err := prepareCA(&incomplete); err == nil {
			t.Fatal("expected error for a broken chain")
		}
	})

	t.Run("valid CA accepted", func(t *testing.T) {
		prepared, err := prepareCA(&ca)
		if err != nil {
			t.Fatalf("expected valid CA to be accepted, got %v", err)
		}
		if prepared.signerID == "" {
			t.Fatal("expected non-empty signer id")
		}
	})
}

func TestReplaceCARejectsAndKeepsPrevious(t *testing.T) {
	first := makeTestCA(t, "first CA")
	caConfig, err := NewCAConfig(&first)
	if err != nil {
		t.Fatalf("NewCAConfig: %v", err)
	}

	original := caConfig.current.Load()

	bad := makeTestCA(t, "bad CA")
	bad.PrivateKey = first.PrivateKey // key/cert mismatch
	if err := caConfig.ReplaceCA(&bad); err == nil {
		t.Fatal("expected ReplaceCA to reject mismatched key")
	}

	if got := caConfig.current.Load(); got != original {
		t.Fatal("active CA changed despite a rejected update")
	}

	second := makeTestCA(t, "second CA")
	if err := caConfig.ReplaceCA(&second); err != nil {
		t.Fatalf("ReplaceCA with valid CA: %v", err)
	}
	if got := caConfig.current.Load(); got.signerID == original.signerID {
		t.Fatal("active CA did not change after successful update")
	}
}

func TestSignerIDDistinctPerCA(t *testing.T) {
	first := makeTestCA(t, "first CA")
	second := makeTestCA(t, "second CA")
	firstPrepared, err := prepareCA(&first)
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := prepareCA(&second)
	if err != nil {
		t.Fatal(err)
	}
	if firstPrepared.signerID == secondPrepared.signerID {
		t.Fatal("expected distinct signer ids for distinct CAs")
	}

	// Re-installing the same CA material keeps the same generation.
	again, err := prepareCA(&first)
	if err != nil {
		t.Fatal(err)
	}
	if again.signerID != firstPrepared.signerID {
		t.Fatal("expected stable signer id for the same CA")
	}
}

func TestInMemoryStorageReclaimsOldGenerations(t *testing.T) {
	store := NewCertStorage()
	const host = "example.com"

	calls := 0
	gen := func() (*tls.Certificate, error) {
		calls++
		return &tls.Certificate{Leaf: &x509.Certificate{Raw: []byte{byte(calls)}}}, nil
	}

	if _, err := store.FetchForSigner("gen1", nil, host, gen); err != nil {
		t.Fatal(err)
	}
	if store.generationCount() != 1 {
		t.Fatalf("expected 1 generation, got %d", store.generationCount())
	}
	if _, err := store.FetchForSigner("gen2", nil, host, gen); err != nil {
		t.Fatal(err)
	}
	if store.generationCount() != 1 {
		t.Fatalf("expected old generation reclaimed, got %d", store.generationCount())
	}

	// Many rotations must not accumulate generations.
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		if _, err := store.FetchForSigner(id, nil, host, gen); err != nil {
			t.Fatal(err)
		}
	}
	if store.generationCount() != 1 {
		t.Fatalf("expected exactly 1 generation after many rotations, got %d", store.generationCount())
	}
}

// TestInMemoryStorageLateOldGenerationCannotPollute simulates the same
// hostname being signed concurrently around a rotation: the old generation's
// signing completes late and its result must never end up in the new cache.
func TestInMemoryStorageLateOldGenerationCannotPollute(t *testing.T) {
	store := NewCertStorage()
	const host = "example.com"

	var oldSigningStarted atomic.Bool
	allowOldSigning := make(chan struct{})
	oldSigned := make(chan struct{})

	oldGen := func() (*tls.Certificate, error) {
		oldSigningStarted.Store(true)
		<-allowOldSigning
		close(oldSigned)
		return &tls.Certificate{Leaf: &x509.Certificate{Raw: []byte("old")}}, nil
	}
	newGen := func() (*tls.Certificate, error) {
		return &tls.Certificate{Leaf: &x509.Certificate{Raw: []byte("new")}}, nil
	}

	oldDone := make(chan struct{})
	go func() {
		// Becomes current generation "old", finds a miss, then blocks in gen.
		_, _ = store.FetchForSigner("old", nil, host, oldGen)
		close(oldDone)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !oldSigningStarted.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !oldSigningStarted.Load() {
		t.Fatal("old generation signing never started")
	}

	// Rotate while the old signing is in flight and populate the new cache.
	newCert, err := store.FetchForSigner("new", nil, host, newGen)
	if err != nil {
		t.Fatal(err)
	}
	if store.generationCount() != 1 {
		t.Fatalf("expected new generation to reclaim old, got %d", store.generationCount())
	}

	// Let the old signing finish late.
	close(allowOldSigning)
	<-oldDone

	// The new cache must still hold the new certificate, never the late old one.
	got, err := store.FetchForSigner("new", nil, host, func() (*tls.Certificate, error) {
		t.Fatal("expected cache hit for new generation; late old result polluted the cache")
		return nil, errors.New("unexpected miss")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != newCert {
		t.Fatal("new generation certificate was overwritten by the late old result")
	}
	if store.generationCount() != 1 {
		t.Fatalf("late old result resurrected a generation: %d", store.generationCount())
	}
}

// blockingLegacyStorage always calls gen and blocks until released, mimicking
// a user's CertStorage that is midway through a rotation.
type blockingLegacyStorage struct {
	mu      sync.Mutex
	certs   map[string]*tls.Certificate
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingLegacyStorage) Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	s.mu.Lock()
	if cert, ok := s.certs[hostname]; ok {
		s.mu.Unlock()
		return cert, nil
	}
	s.mu.Unlock()

	s.once.Do(func() { close(s.started) })
	<-s.release
	cert, err := gen()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.certs[hostname] = cert
	s.mu.Unlock()
	return cert, nil
}

func TestLegacyStorageStaleCertIsResigned(t *testing.T) {
	first := makeTestCA(t, "first CA")
	second := makeTestCA(t, "second CA")
	firstPrepared, err := prepareCA(&first)
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := prepareCA(&second)
	if err != nil {
		t.Fatal(err)
	}

	// A host cert signed by the first CA.
	oldHostCert, err := signer.SignHost(firstPrepared.cert, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}

	store := &blockingLegacyStorage{
		certs:   map[string]*tls.Certificate{"example.com": oldHostCert},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}

	// Old CA: cache hit, no signing through the store (release stays closed).
	got, err := fetchFromLegacyStorage(store, firstPrepared.leaf, "example.com", func() (*tls.Certificate, error) {
		t.Fatal("old cert signed by old CA should be a cache hit")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != oldHostCert {
		t.Fatal("expected old CA to reuse the cached cert")
	}

	// New CA: stale cert must be transparently re-signed via gen.
	fresh, err := fetchFromLegacyStorage(store, secondPrepared.leaf, "example.com", func() (*tls.Certificate, error) {
		return signer.SignHost(secondPrepared.cert, []string{"example.com"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Leaf.CheckSignatureFrom(secondPrepared.leaf); err != nil {
		t.Fatalf("re-signed cert not signed by new CA: %v", err)
	}
}
