package goproxy

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/elazarl/goproxy/internal/signer"
)

// CAConfig holds a MITM signing CA that can be replaced atomically while the
// proxy keeps serving.
//
// ReplaceCA installs a new CA instantly: the next TLS handshake signs with the
// new CA, while connections established with an older CA keep their negotiated
// session and are not touched. Each distinct CA gets a unique signer id (a
// generation), which keeps certificates signed for the old CA apart from
// certificates signed for the new one in caches.
//
// A zero CAConfig is not usable; create one with NewCAConfig.
type CAConfig struct {
	current atomic.Pointer[preparedCA]
}

// preparedCA is an immutable, validated snapshot of a signing CA.
type preparedCA struct {
	cert     tls.Certificate
	leaf     *x509.Certificate
	signerID string
}

// NewCAConfig validates ca and returns an atomic, rotatable CA configuration.
// The certificate must be usable as a CA, its private key must match the leaf
// certificate's public key, and its certificate chain must be complete;
// otherwise an error is returned and no configuration is created.
func NewCAConfig(ca *tls.Certificate) (*CAConfig, error) {
	prepared, err := prepareCA(ca)
	if err != nil {
		return nil, err
	}
	caConfig := &CAConfig{}
	caConfig.current.Store(prepared)
	return caConfig, nil
}

// ReplaceCA atomically replaces the active CA. The same validation rules as
// NewCAConfig apply; on any error the previously active CA is kept and the
// error is returned. Handshakes in progress finish with the CA they already
// picked up, and established connections continue untouched.
func (caConfig *CAConfig) ReplaceCA(ca *tls.Certificate) error {
	prepared, err := prepareCA(ca)
	if err != nil {
		return err
	}
	caConfig.current.Store(prepared)
	return nil
}

// TLSConfig returns a per-host TLSConfig function suitable for
// ConnectAction.TLSConfig. It always signs host certificates with the CA most
// recently installed via ReplaceCA, keying any certStore lookup by that CA's
// signer id so a certificate signed by an older CA can never be served for
// the newer one.
func (caConfig *CAConfig) TLSConfig() func(host string, ctx *ProxyCtx) (*tls.Config, error) {
	return func(host string, ctx *ProxyCtx) (*tls.Config, error) {
		prepared := caConfig.current.Load()
		if prepared == nil {
			return nil, errors.New("goproxy: CAConfig is not initialized; use NewCAConfig")
		}
		return prepared.tlsConfig(host, ctx)
	}
}

// prepareCA validates the certificate before it can sign anything:
//   - at least one certificate and a private key are present;
//   - the leaf certificate is parsed;
//   - the leaf is a CA certificate allowed to sign other certificates;
//   - the private key matches the leaf's public key and can sign;
//   - every certificate in the chain is signed by the certificate following it,
//     and the final certificate is self-signed (complete chain).
//
// Any failure rejects the whole update and leaves the active CA untouched.
func prepareCA(ca *tls.Certificate) (*preparedCA, error) {
	if ca == nil {
		return nil, errors.New("goproxy: CA certificate is nil")
	}
	if len(ca.Certificate) == 0 {
		return nil, errors.New("goproxy: CA has no certificate")
	}
	if ca.PrivateKey == nil {
		return nil, errors.New("goproxy: CA has no private key")
	}
	signingKey, ok := ca.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("goproxy: CA private key %T cannot sign", ca.PrivateKey)
	}

	leaf := ca.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(ca.Certificate[0]); err != nil {
			return nil, fmt.Errorf("goproxy: cannot parse CA leaf certificate: %w", err)
		}
	}

	if !leaf.BasicConstraintsValid || !leaf.IsCA {
		return nil, errors.New("goproxy: CA leaf certificate is not a certificate authority")
	}
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("goproxy: CA leaf certificate is not allowed to sign certificates")
	}

	if err := validateKeyMatches(signingKey, leaf); err != nil {
		return nil, err
	}

	chain, err := parseChain(ca.Certificate)
	if err != nil {
		return nil, err
	}
	if err := validateChain(chain); err != nil {
		return nil, err
	}

	// tls.X509KeyPair populates Leaf, but never rely on callers to.
	certCopy := *ca
	certCopy.Leaf = leaf

	return &preparedCA{
		cert:     certCopy,
		leaf:     leaf,
		signerID: signerID(leaf),
	}, nil
}

func validateKeyMatches(signingKey crypto.Signer, leaf *x509.Certificate) error {
	signerPub, err := x509.MarshalPKIXPublicKey(signingKey.Public())
	if err != nil {
		return fmt.Errorf("goproxy: cannot marshal CA private key's public key: %w", err)
	}
	leafPub, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return fmt.Errorf("goproxy: cannot marshal CA leaf public key: %w", err)
	}
	if !bytes.Equal(signerPub, leafPub) {
		return errors.New("goproxy: CA private key does not match the leaf certificate")
	}
	return nil
}

func parseChain(derCerts [][]byte) ([]*x509.Certificate, error) {
	chain := make([]*x509.Certificate, len(derCerts))
	for i, der := range derCerts {
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("goproxy: cannot parse CA chain certificate %d: %w", i, err)
		}
		chain[i] = parsed
	}
	return chain, nil
}

// validateChain verifies each certificate is signed by the next one and that
// the last certificate is self-signed. A single certificate must therefore be
// a self-signed root.
func validateChain(chain []*x509.Certificate) error {
	for i := 0; i < len(chain)-1; i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return fmt.Errorf("goproxy: CA chain broken between certificate %d and %d: %w", i, i+1, err)
		}
	}
	root := chain[len(chain)-1]
	if err := root.CheckSignatureFrom(root); err != nil {
		return fmt.Errorf("goproxy: CA chain does not end in a self-signed root certificate: %w", err)
	}
	return nil
}

// signerID is a stable identifier of a signing CA generation. Two distinct
// CAs have distinct ids; installing the same CA material again keeps the same
// id, so its cached certificates stay reusable.
func signerID(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:])
}

func (prepared *preparedCA) tlsConfig(host string, ctx *ProxyCtx) (*tls.Config, error) {
	hostname := stripPort(host)
	ctx.Logf("signing for %s", hostname)

	genCert := func() (*tls.Certificate, error) {
		return signer.SignHost(prepared.cert, []string{hostname})
	}

	var cert *tls.Certificate
	var err error
	switch store := ctx.certStore.(type) {
	case GenerationCertStorage:
		cert, err = store.FetchForSigner(prepared.signerID, prepared.leaf, hostname, genCert)
	case CertStorage:
		cert, err = fetchFromLegacyStorage(store, prepared.leaf, hostname, genCert)
	default:
		cert, err = genCert()
	}
	if err != nil {
		ctx.Warnf("Cannot sign host certificate with provided CA: %s", err)
		return nil, err
	}

	config := defaultTLSConfig.Clone()
	config.Certificates = append(config.Certificates, *cert)
	return config, nil
}

// fetchFromLegacyStorage adapts a generation-unaware CertStorage to CA
// rotation. Certificates returned by the user's store are only used when they
// were signed by the active CA; a stale or foreign certificate is transparently
// re-signed. Correctness therefore never depends on clearing the user's map.
func fetchFromLegacyStorage(
	store CertStorage,
	caLeaf *x509.Certificate,
	hostname string,
	genCert func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	cert, err := store.Fetch(hostname, genCert)
	if err != nil {
		return nil, err
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("goproxy: cert storage returned no certificate for %q", hostname)
	}
	leaf := cert.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, fmt.Errorf("goproxy: cannot parse cached certificate for %q: %w", hostname, err)
		}
	}
	if err := leaf.CheckSignatureFrom(caLeaf); err != nil {
		return genCert()
	}
	return cert, nil
}

// lazyPreparedCA caches the validated snapshot of a CA pointer that is used
// through the legacy TLSConfigFromCA API.
type lazyPreparedCA struct {
	once sync.Once
	ca   *tls.Certificate
	got  *preparedCA
	err  error
}

func (lazy *lazyPreparedCA) get() (*preparedCA, error) {
	lazy.once.Do(func() {
		lazy.got, lazy.err = prepareCA(lazy.ca)
	})
	return lazy.got, lazy.err
}

// TLSConfigFromCA returns a TLSConfig function that generates dynamic TLS
// certificates for each target host, signed by the given CA certificate.
// The generated certificates are used during MITM interception (ConnectMitm).
// If a CertStorage is set on the ProxyCtx, certificates are cached and reused
// to save CPU.
//
// For rotating the CA at runtime without restarting the proxy, use
// NewCAConfig and its TLSConfig method instead.
func TLSConfigFromCA(ca *tls.Certificate) func(host string, ctx *ProxyCtx) (*tls.Config, error) {
	lazy := &lazyPreparedCA{ca: ca}
	return func(host string, ctx *ProxyCtx) (*tls.Config, error) {
		prepared, err := lazy.get()
		if err != nil {
			ctx.Warnf("Cannot sign host certificate with provided CA: %s", err)
			return nil, err
		}
		return prepared.tlsConfig(host, ctx)
	}
}
