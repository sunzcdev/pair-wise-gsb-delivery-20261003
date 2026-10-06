package goproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCtxWithStore(store CertStorage) *ProxyCtx {
	return &ProxyCtx{
		Proxy:     NewProxyHttpServer(),
		certStore: store,
	}
}

func genTestAuthority(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
}

func genNonCA(t *testing.T, ca tls.Certificate) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "not-a-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.Leaf, &key.PublicKey, ca.PrivateKey)
	require.NoError(t, err)

	return tls.Certificate{
		Certificate: [][]byte{der, ca.Certificate[0]},
		PrivateKey:  key,
	}
}

func genIssuedCA(t *testing.T, issuer tls.Certificate, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, issuer.Leaf, &key.PublicKey, issuer.PrivateKey)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
}

func TestValidateCA(t *testing.T) {
	t.Run("builtin CA is valid", func(t *testing.T) {
		assert.NoError(t, validateCA(&GoproxyCa))
	})

	t.Run("generated self-signed root", func(t *testing.T) {
		ca := genTestAuthority(t, "test-ca")
		assert.NoError(t, validateCA(&ca))
	})

	t.Run("full two-tier chain is valid", func(t *testing.T) {
		root := genTestAuthority(t, "root-ca")
		intermediate := genIssuedCA(t, root, "intermediate-ca")
		chain := tls.Certificate{
			Certificate: [][]byte{intermediate.Certificate[0], root.Certificate[0]},
			PrivateKey:  intermediate.PrivateKey,
		}
		assert.NoError(t, validateCA(&chain))
	})

	t.Run("nil and empty", func(t *testing.T) {
		assert.Error(t, validateCA(nil))
		assert.Error(t, validateCA(&tls.Certificate{}))
		assert.Error(t, validateCA(&tls.Certificate{Certificate: [][]byte{[]byte("x")}}))
	})

	t.Run("non-CA leaf is rejected", func(t *testing.T) {
		ca := genTestAuthority(t, "issuer-ca")
		leaf := genNonCA(t, ca)
		err := validateCA(&leaf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certificate authority")
	})

	t.Run("private key mismatch is rejected", func(t *testing.T) {
		ca := genTestAuthority(t, "mismatch-ca")
		other := genTestAuthority(t, "other-ca")
		ca.PrivateKey = other.PrivateKey
		err := validateCA(&ca)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match")
	})

	t.Run("truncated chain is rejected", func(t *testing.T) {
		root := genTestAuthority(t, "orphan-root")
		intermediate := genIssuedCA(t, root, "orphan-intermediate")
		err := validateCA(&intermediate)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "self-signed root")
	})

	t.Run("chain signed by the wrong parent is rejected", func(t *testing.T) {
		rootA := genTestAuthority(t, "root-a")
		rootB := genTestAuthority(t, "root-b")
		intermediateA := genIssuedCA(t, rootA, "intermediate-a")
		chain := tls.Certificate{
			// intermediateA signed by rootA, but rootB appended instead.
			Certificate: [][]byte{intermediateA.Certificate[0], rootB.Certificate[0]},
			PrivateKey:  intermediateA.PrivateKey,
		}
		err := validateCA(&chain)
		require.Error(t, err)
	})
}

func TestReplaceRejectsInvalidUpdateAtomically(t *testing.T) {
	ca := genTestAuthority(t, "initial-ca")
	rotating, err := NewRotatingCA(&ca)
	require.NoError(t, err)
	require.EqualValues(t, 1, rotating.Generation())

	bad := ca
	other := genTestAuthority(t, "wrong-key-ca")
	bad.PrivateKey = other.PrivateKey

	require.Error(t, rotating.ReplaceCertificate(bad))
	assert.EqualValues(t, 1, rotating.Generation(), "rejected update must not bump the generation")

	config, err := rotating.TLSConfig()("example.test", testCtxWithStore(nil))
	require.NoError(t, err)
	served := config.Certificates[0]
	assert.NoError(t, served.Leaf.CheckSignatureFrom(ca.Leaf),
		"after a rejected update new handshakes must still use the old CA")

	newCA := genTestAuthority(t, "rotated-ca")
	require.NoError(t, rotating.ReplaceCertificate(newCA))
	assert.EqualValues(t, 2, rotating.Generation())
}

func TestLateOldGenerationTaskCannotPolluteNewCache(t *testing.T) {
	ca := genTestAuthority(t, "late-old-ca")
	rotating, err := NewRotatingCA(&ca)
	require.NoError(t, err)

	store := &blockingGenerationStore{
		certs:       make(map[uint64]map[string]*tls.Certificate),
		enteredGen1: make(chan struct{}),
		releaseGen1: make(chan struct{}),
	}
	store.blockGen.Store(1)

	oldDone := make(chan error, 1)
	go func() {
		_, err := rotating.TLSConfig()("same.example.test", testCtxWithStore(store))
		oldDone <- err
	}()
	<-store.enteredGen1

	newCA := genTestAuthority(t, "late-new-ca")
	require.NoError(t, rotating.ReplaceCertificate(newCA))

	// The new generation must not be able to observe a certificate for this host,
	// even though the old generation's signing task is still in flight.
	current := rotating.cur.Load()
	require.NotNil(t, current)

	newCtx := testCtxWithStore(store)
	newCert, err := current.fetchCert("same.example.test", newCtx, store)
	require.NoError(t, err)
	require.NoError(t, newCert.Leaf.CheckSignatureFrom(newCA.Leaf))

	// Now let the late old task finish; it must land only in generation 1.
	close(store.releaseGen1)
	require.NoError(t, <-oldDone)

	require.Contains(t, store.certs, uint64(1))
	require.Contains(t, store.certs, uint64(2))

	oldCert := store.certs[1]["same.example.test"]
	require.NotNil(t, oldCert)
	assert.NoError(t, oldCert.Leaf.CheckSignatureFrom(ca.Leaf))

	newStored := store.certs[2]["same.example.test"]
	require.NotNil(t, newStored)
	assert.Same(t, newCert, newStored)

	// The old generation's late certificate can never be reachable from the
	// current generation.
	again, err := current.fetchCert("same.example.test", testCtxWithStore(store), store)
	require.NoError(t, err)
	assert.Same(t, newCert, again)
}

func TestOldGenerationBuiltinCacheIsReclaimed(t *testing.T) {
	ca1 := genTestAuthority(t, "reclaim-1")
	ca2 := genTestAuthority(t, "reclaim-2")
	ca3 := genTestAuthority(t, "reclaim-3")

	rotating, err := NewRotatingCA(&ca1)
	require.NoError(t, err)

	ctx := testCtxWithStore(nil)
	g1 := rotating.cur.Load()
	_, err = g1.fetchCert("host1.example.test", ctx, nil)
	require.NoError(t, err)

	require.NoError(t, rotating.ReplaceCertificate(ca2))
	g2 := rotating.cur.Load()
	_, err = g2.fetchCert("host2.example.test", testCtxWithStore(nil), nil)
	require.NoError(t, err)

	require.NoError(t, rotating.ReplaceCertificate(ca3))

	assert.Eventually(t, func() bool {
		g1.builtinCacheMu.Lock()
		g1Empty := len(g1.builtinCache) == 0
		g1.builtinCacheMu.Unlock()

		g2.builtinCacheMu.Lock()
		g2Empty := len(g2.builtinCache) == 0
		g2.builtinCacheMu.Unlock()
		return g1Empty && g2Empty
	}, 2*time.Second, 10*time.Millisecond, "retired generations must not retain their caches")

	// Rotating repeatedly must never accumulate a permanent extra copy.
	current := rotating.cur.Load()
	current.builtinCacheMu.Lock()
	assert.Len(t, current.builtinCache, 0)
	current.builtinCacheMu.Unlock()
}

func TestLegacyCertStorageStaysIsolatedAcrossGenerations(t *testing.T) {
	ca := genTestAuthority(t, "legacy-old")
	newCA := genTestAuthority(t, "legacy-new")

	rotating, err := NewRotatingCA(&ca)
	require.NoError(t, err)

	store := newLegacyStore()
	ctx := testCtxWithStore(store)

	oldCert, err := rotating.TLSConfig()("same.legacy.test", ctx)
	require.NoError(t, err)
	require.NoError(t, oldCert.Certificates[0].Leaf.CheckSignatureFrom(ca.Leaf))

	require.NoError(t, rotating.ReplaceCertificate(newCA))

	newCert, err := rotating.TLSConfig()("same.legacy.test", testCtxWithStore(store))
	require.NoError(t, err)
	require.NoError(t, newCert.Certificates[0].Leaf.CheckSignatureFrom(newCA.Leaf))

	// The legacy adapter keeps the two generations in distinct key slots: it
	// never needed the user to clear their map, and the old slot still holds
	// the old-CA certificate.
	assert.Len(t, store.certs, 2)
	var oldSlot, newSlot *tls.Certificate
	for key, cert := range store.certs {
		switch {
		case len(key) > 0 && key[0] == '\x00':
			if cert.Leaf.CheckSignatureFrom(ca.Leaf) == nil {
				oldSlot = cert
			}
			if cert.Leaf.CheckSignatureFrom(newCA.Leaf) == nil {
				newSlot = cert
			}
		}
	}
	require.NotNil(t, oldSlot)
	require.NotNil(t, newSlot)
}

func TestGenerationAwareStorageDroppedOnRetire(t *testing.T) {
	ca := genTestAuthority(t, "drop-old")
	newCA := genTestAuthority(t, "drop-new")

	rotating, err := NewRotatingCA(&ca)
	require.NoError(t, err)

	store := newTrackingGenerationStore()

	_, err = rotating.TLSConfig()("drop.example.test", testCtxWithStore(store))
	require.NoError(t, err)

	require.NoError(t, rotating.ReplaceCertificate(newCA))

	assert.Eventually(t, func() bool {
		return store.dropped(1)
	}, 2*time.Second, 10*time.Millisecond)

	_, err = rotating.TLSConfig()("drop.example.test", testCtxWithStore(store))
	require.NoError(t, err)
	assert.False(t, store.dropped(2), "the active generation must not be dropped")
}
