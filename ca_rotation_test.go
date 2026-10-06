package goproxy_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func genRotationTestCA(t *testing.T, commonName string) tls.Certificate {
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

type readConnWrapper struct {
	net.Conn
	r io.Reader
}

func (c *readConnWrapper) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func openMitmTunnelErr(proxyAddr, host string, roots *x509.CertPool) (*tls.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}

	connectReq, err := http.NewRequestWithContext(context.Background(), http.MethodConnect, host, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	connectResp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if connectResp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected CONNECT status %s", connectResp.Status)
	}

	tlsConn := tls.Client(&readConnWrapper{Conn: conn, r: br}, &tls.Config{
		ServerName: host,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func openMitmTunnel(t *testing.T, proxyAddr, host string, roots *x509.CertPool) *tls.Conn {
	t.Helper()
	tlsConn, err := openMitmTunnelErr(proxyAddr, host, roots)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tlsConn.Close() })
	return tlsConn
}

func tunnelRoundTrip(t *testing.T, tlsConn *tls.Conn, host string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+host+"/", nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(tlsConn))

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return string(body)
}

// e2eGenerationStore is a generation-aware CertStorage whose misses can be
// blocked for a specific (host, generation). This lets the test start a signing
// task under the old CA, rotate while it is blocked, and release it after the
// switch: exactly the "old task finishes late" scenario.
type e2eGenerationStore struct {
	mu          sync.Mutex
	certs       map[uint64]map[string]*tls.Certificate
	signed      map[uint64]map[string]*tls.Certificate
	blockGen    uint64
	blockHost   string
	arrivedCh   chan struct{}
	arrivedOnce sync.Once
	releaseCh   chan struct{}
	releaseOnce sync.Once
}

func newE2eGenerationStore() *e2eGenerationStore {
	return &e2eGenerationStore{
		certs:     make(map[uint64]map[string]*tls.Certificate),
		signed:    make(map[uint64]map[string]*tls.Certificate),
		arrivedCh: make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
}

func (s *e2eGenerationStore) blockMisses(host string, generation uint64) {
	s.mu.Lock()
	s.blockHost = host
	s.blockGen = generation
	s.mu.Unlock()
}

func (s *e2eGenerationStore) waitArrival() {
	select {
	case <-s.arrivedCh:
	case <-time.After(5 * time.Second):
	}
}

func (s *e2eGenerationStore) release() {
	s.releaseOnce.Do(func() { close(s.releaseCh) })
}

func (s *e2eGenerationStore) Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	return s.FetchGeneration(hostname, 0, gen)
}

func (s *e2eGenerationStore) FetchGeneration(
	hostname string,
	generation uint64,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	s.mu.Lock()
	if byGen, ok := s.certs[generation]; ok {
		if cert, found := byGen[hostname]; found {
			s.mu.Unlock()
			return cert, nil
		}
	}
	shouldBlock := s.blockHost == hostname && s.blockGen == generation
	s.mu.Unlock()

	if shouldBlock {
		s.arrivedOnce.Do(func() { close(s.arrivedCh) })
		<-s.releaseCh
	}

	cert, err := gen()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.certs[generation] == nil {
		s.certs[generation] = make(map[string]*tls.Certificate)
	}
	s.certs[generation][hostname] = cert
	if s.signed[generation] == nil {
		s.signed[generation] = make(map[string]*tls.Certificate)
	}
	s.signed[generation][hostname] = cert
	s.mu.Unlock()
	return cert, nil
}

func (s *e2eGenerationStore) DropGeneration(generation uint64) {
	s.mu.Lock()
	delete(s.certs, generation)
	s.mu.Unlock()
}

func (s *e2eGenerationStore) generationCount(generation uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.certs[generation])
}

func (s *e2eGenerationStore) get(generation uint64, host string) *tls.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.certs[generation][host]
}

func (s *e2eGenerationStore) signedCert(generation uint64, host string) *tls.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signed[generation][host]
}

// TestCARotationEndToEnd sets up a proxy with two local test CAs and verifies
// the rotation contract over real TLS handshakes:
//
//   - a connection established before rotation keeps serving requests with the
//     old leaf and survives the switch;
//   - a signing task started with the old CA that completes after the switch
//     finishes with the old CA and can only land in the old generation cache;
//   - new handshakes immediately present leaves signed by the new CA;
//   - the retired generation's cache is reclaimed.
func TestCARotationEndToEnd(t *testing.T) {
	const (
		hostA     = "existing.example.test"
		hostLate  = "late.example.test"
		hostFresh = "fresh.example.test"
	)

	ca1 := genRotationTestCA(t, "e2e-test-ca-1")
	ca2 := genRotationTestCA(t, "e2e-test-ca-2")
	require.NotEqual(t, ca1.Leaf.Subject.CommonName, ca2.Leaf.Subject.CommonName)

	rotating, err := goproxy.NewRotatingCA(&ca1)
	require.NoError(t, err)

	store := newE2eGenerationStore()

	proxy := goproxy.NewProxyHttpServer()
	proxy.CertStore = store
	proxy.OnRequest().HandleConnectFunc(func(h string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		return &goproxy.ConnectAction{
			Action:    goproxy.ConnectMitm,
			TLSConfig: rotating.TLSConfig(),
		}, h
	})
	proxy.OnRequest().DoFunc(
		func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			return nil, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusOK, "rotated-ok")
		})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	roots := x509.NewCertPool()
	roots.AddCert(ca1.Leaf)
	roots.AddCert(ca2.Leaf)
	addr := proxyServer.Listener.Addr().String()

	// Phase 1: establish a connection with CA1 and serve a request.
	connA := openMitmTunnel(t, addr, hostA, roots)
	require.Equal(t, ca1.Leaf.Subject.CommonName,
		connA.ConnectionState().PeerCertificates[0].Issuer.CommonName,
		"the pre-rotation leaf must be issued by CA1")
	require.Equal(t, "rotated-ok", tunnelRoundTrip(t, connA, hostA))

	// Phase 2: start a signing task under generation 1 that blocks in the
	// cache miss with the generation already pinned, then rotate the CA.
	store.blockMisses(hostLate, 1)

	lateErr := make(chan error, 1)
	go func() {
		c, err := openMitmTunnelErr(addr, hostLate, roots)
		if err == nil {
			issuer := c.ConnectionState().PeerCertificates[0].Issuer.CommonName
			if issuer != ca1.Leaf.Subject.CommonName {
				err = fmt.Errorf("late task leaf issued by %q, want %q", issuer, ca1.Leaf.Subject.CommonName)
			}
			_ = c.Close()
		}
		lateErr <- err
	}()
	store.waitArrival()

	require.NoError(t, rotating.ReplaceCertificate(ca2))
	require.EqualValues(t, 2, rotating.Generation())

	// New handshakes (with a different host to avoid the gate) must use CA2 now.
	connB := openMitmTunnel(t, addr, "fresh.example.test", roots)
	require.Equal(t, ca2.Leaf.Subject.CommonName,
		connB.ConnectionState().PeerCertificates[0].Issuer.CommonName,
		"new handshakes must use the new CA immediately")

	// Release the late old-generation task and assert it used CA1.
	store.release()
	require.NoError(t, <-lateErr)

	// The late task populated only generation 1; generation 2 for this host is
	// still empty and must never be polluted by the old-CA certificate.
	lateCert := store.signedCert(1, hostLate)
	require.NotNil(t, lateCert)
	require.NoError(t, lateCert.Leaf.CheckSignatureFrom(ca1.Leaf))
	assert.Nil(t, store.signedCert(2, hostLate), "the old task must not pollute the new generation cache")

	// A handshake for the same host after the switch signs fresh with CA2.
	connC := openMitmTunnel(t, addr, hostLate, roots)
	require.Equal(t, ca2.Leaf.Subject.CommonName,
		connC.ConnectionState().PeerCertificates[0].Issuer.CommonName)
	require.Equal(t, "rotated-ok", tunnelRoundTrip(t, connC, hostLate))
	newCert := store.get(2, hostLate)
	require.NotNil(t, newCert)
	require.NoError(t, newCert.Leaf.CheckSignatureFrom(ca2.Leaf))

	// The original connection continues to complete requests with its old leaf.
	require.Equal(t, "rotated-ok", tunnelRoundTrip(t, connA, hostA),
		"the existing connection must continue to completion")

	// The retired generation is reclaimed once its pinned handshake is gone.
	assert.Eventually(t, func() bool {
		return store.generationCount(1) == 0
	}, 2*time.Second, 10*time.Millisecond, "the retired generation cache must be reclaimed")
	// Generation 2 holds the fresh host and the late host entries.
	assert.Equal(t, 2, store.generationCount(2))
}
