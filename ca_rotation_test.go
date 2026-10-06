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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/require"
)

const mitmHost = "example.test"

// makeLocalTestCA creates a fresh self-signed test CA. Two calls produce two
// independent CAs with distinguishable common names.
func makeLocalTestCA(t *testing.T, commonName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dialMitmTunnel opens a CONNECT tunnel via proxyAddr and performs the MITM
// TLS handshake trusting only caPool. It returns the established *tls.Conn.
func dialMitmTunnel(t *testing.T, proxyAddr string, caPool *x509.CertPool) (*tls.Conn, error) {
	t.Helper()
	target := mitmHost + ":443"

	raw, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if _, err := raw.Write([]byte(fmt.Sprintf(
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target))); err != nil {
		raw.Close()
		return nil, err
	}

	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		raw.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw.Close()
		return nil, fmt.Errorf("CONNECT failed: %s", resp.Status)
	}

	tlsConn := tls.Client(&bufferedConn{Conn: raw, r: br}, &tls.Config{
		RootCAs:    caPool,
		ServerName: mitmHost,
	})
	if err := tlsConn.HandshakeContext(t.Context()); err != nil {
		raw.Close()
		return nil, err
	}
	return tlsConn, nil
}

func poolFor(ca tls.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Leaf)
	return pool
}

func getBoboRaw(t *testing.T, conn net.Conn) {
	t.Helper()
	reqStr := "GET /bobo HTTP/1.1\r\nHost: " + mitmHost + "\r\nConnection: keep-alive\r\n\r\n"
	_, err := conn.Write([]byte(reqStr))
	require.NoError(t, err)
	br := bufio.NewReaderSize(conn, 1)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.GreaterOrEqual(t, resp.ContentLength, int64(0))
	body := make([]byte, resp.ContentLength)
	_, err = io.ReadFull(br, body)
	require.NoError(t, err)
	require.Equal(t, "bobo", string(body))
	require.Zero(t, br.Buffered(), "reader must not over-consume bytes")
}

// startRotationProxy starts a MITM proxy with a rotatable CA. Upstream
// requests for the MITM host are dialed to a local plaintext test server.
func startRotationProxy(t *testing.T, store goproxy.CertStorage) (string, *goproxy.CAConfig) {
	t.Helper()

	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "bobo")
	}))
	t.Cleanup(target.Close)
	targetAddr := strings.TrimPrefix(target.URL, "https://")

	ca := makeLocalTestCA(t, "test-ca-1")
	caConfig, err := goproxy.NewCAConfig(&ca)
	require.NoError(t, err)

	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	proxy.Tr.Proxy = nil
	proxy.ConnectDial = nil
	// Every MITM upstream dial reaches the local test server.
	proxy.Tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, targetAddr)
	}
	if store != nil {
		proxy.CertStore = store
	}
	mitm := &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: caConfig.TLSConfig()}
	proxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(
		func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
			return mitm, host
		}))

	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	proxyURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	return proxyURL.Host, caConfig
}

// TestCARotationNewHandshakesUseNewCAAndOldConnectionSurvives verifies that
// after ReplaceCA new handshakes are signed by the new CA immediately, a
// client trusting only the old CA can no longer connect, and a connection
// established before rotation keeps serving on the very same *tls.Conn.
func TestCARotationNewHandshakesUseNewCAAndOldConnectionSurvives(t *testing.T) {
	proxyAddr, caConfig := startRotationProxy(t, goproxy.NewCertStorage())

	ca1 := makeLocalTestCA(t, "ca-one")
	require.NoError(t, caConfig.ReplaceCA(&ca1))
	ca2 := makeLocalTestCA(t, "ca-two")

	// Pre-rotation connection, reused for two requests.
	oldConn, err := dialMitmTunnel(t, proxyAddr, poolFor(ca1))
	require.NoError(t, err)
	t.Cleanup(func() { oldConn.Close() })
	require.Equal(t, "ca-one", oldConn.ConnectionState().PeerCertificates[0].Issuer.CommonName)
	getBoboRaw(t, oldConn)

	require.NoError(t, caConfig.ReplaceCA(&ca2))

	// New handshake uses CA2.
	newConn, err := dialMitmTunnel(t, proxyAddr, poolFor(ca2))
	require.NoError(t, err)
	t.Cleanup(func() { newConn.Close() })
	require.Equal(t, "ca-two", newConn.ConnectionState().PeerCertificates[0].Issuer.CommonName)
	getBoboRaw(t, newConn)

	// Trusting only CA1 must fail now.
	_, err = dialMitmTunnel(t, proxyAddr, poolFor(ca1))
	require.Error(t, err)

	// A connection established pre-rotation still serves CA1 after rotation.
	require.Equal(t, "ca-one", oldConn.ConnectionState().PeerCertificates[0].Issuer.CommonName)
	getBoboRaw(t, oldConn)
}

// TestConcurrentMitmSameCA opens many connections concurrently without any
// rotation, ruling in/out the signing/caching path under parallelism.
func TestConcurrentMitmSameCA(t *testing.T) {
	runConcurrentClients(t, false)
}

// TestCARotationConcurrentTwoCAs opens many connections while the CA rotates
// and checks every leaf certificate's issuer matches the CA trusted by the
// client, plus continuity after rotation.
func TestCARotationConcurrentTwoCAs(t *testing.T) {
	runConcurrentClients(t, true)
}

func runConcurrentClients(t *testing.T, rotate bool) {
	store := goproxy.NewCertStorage()
	proxyAddr, caConfig := startRotationProxy(t, store)

	ca1 := makeLocalTestCA(t, "ca-concurrent-one")
	ca2 := makeLocalTestCA(t, "ca-concurrent-two")
	require.NoError(t, caConfig.ReplaceCA(&ca1))

	const clientsPerCA = 12
	var (
		wg      sync.WaitGroup
		ok      atomic.Int64
		mu      sync.Mutex
		errs    []string
		issuers sync.Map
	)

	noRotate := !rotate
	run := func(pool *x509.CertPool, expectIssuer string) {
		defer wg.Done()
		conn, err := dialMitmTunnel(t, proxyAddr, pool)
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s: %v", expectIssuer, err))
			mu.Unlock()
			return
		}
		issuer := conn.ConnectionState().PeerCertificates[0].Issuer.CommonName
		issuers.Store(issuer, struct{}{})
		if issuer != expectIssuer {
			mu.Lock()
			errs = append(errs, fmt.Sprintf("expected issuer %q got %q", expectIssuer, issuer))
			mu.Unlock()
			conn.Close()
			return
		}
		getBoboRaw(t, conn)
		t.Cleanup(func() { conn.Close() })
		ok.Add(1)
	}

	for i := 0; i < clientsPerCA; i++ {
		wg.Add(1)
		go run(poolFor(ca1), "ca-concurrent-one")
	}
	if !noRotate {
		// Rotate while first-wave handshakes are in progress: the store
		// serializes the first miss, so 50ms lands in that signing window.
		time.Sleep(50 * time.Millisecond)
		require.NoError(t, caConfig.ReplaceCA(&ca2))
	}
	for i := 0; i < clientsPerCA; i++ {
		wg.Add(1)
		if noRotate {
			go run(poolFor(ca1), "ca-concurrent-one")
		} else {
			go run(poolFor(ca2), "ca-concurrent-two")
		}
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent clients did not finish")
	}
	require.Empty(t, errs)
	require.Equal(t, int64(2*clientsPerCA), ok.Load())

	var got []string
	issuers.Range(func(k, _ any) bool {
		got = append(got, k.(string))
		return true
	})
	if rotate {
		require.ElementsMatch(t, []string{"ca-concurrent-one", "ca-concurrent-two"}, got)
	} else {
		require.ElementsMatch(t, []string{"ca-concurrent-one"}, got)
	}
}

// externalLegacyStore is a generation-unaware CertStorage, exactly like user
// code written before CA rotation support existed.
type externalLegacyStore struct {
	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

func newExternalLegacyStore() *externalLegacyStore {
	return &externalLegacyStore{certs: map[string]*tls.Certificate{}}
}

func (s *externalLegacyStore) Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	s.mu.Lock()
	cert, ok := s.certs[hostname]
	s.mu.Unlock()
	if ok {
		return cert, nil
	}
	cert, err := gen()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.certs[hostname] = cert
	s.mu.Unlock()
	return cert, nil
}

func (s *externalLegacyStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.certs)
}

// TestCARotationWithLegacyCertStorage verifies a plain CertStorage keeps
// working across rotation without anyone clearing its map.
func TestCARotationWithLegacyCertStorage(t *testing.T) {
	store := newExternalLegacyStore()
	proxyAddr, caConfig := startRotationProxy(t, store)

	ca1 := makeLocalTestCA(t, "ca-legacy-one")
	ca2 := makeLocalTestCA(t, "ca-legacy-two")
	require.NoError(t, caConfig.ReplaceCA(&ca1))

	conn1, err := dialMitmTunnel(t, proxyAddr, poolFor(ca1))
	require.NoError(t, err)
	require.Equal(t, "ca-legacy-one", conn1.ConnectionState().PeerCertificates[0].Issuer.CommonName)
	conn1.Close()
	require.Equal(t, 1, store.size())

	// Rotate without clearing the store: stale entry is detected, re-signed.
	require.NoError(t, caConfig.ReplaceCA(&ca2))
	conn2, err := dialMitmTunnel(t, proxyAddr, poolFor(ca2))
	require.NoError(t, err)
	t.Cleanup(func() { conn2.Close() })
	require.Equal(t, "ca-legacy-two", conn2.ConnectionState().PeerCertificates[0].Issuer.CommonName)

	conn3, err := dialMitmTunnel(t, proxyAddr, poolFor(ca2))
	require.NoError(t, err)
	t.Cleanup(func() { conn3.Close() })
	require.Equal(t, "ca-legacy-two", conn3.ConnectionState().PeerCertificates[0].Issuer.CommonName)
}
