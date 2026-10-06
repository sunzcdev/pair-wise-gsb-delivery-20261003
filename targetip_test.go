package goproxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/elazarl/goproxy"
	"github.com/elazarl/goproxy/internal/signer"
	"github.com/stretchr/testify/require"
)

// spoofRemoteConn reports a forged remote address, simulating a connection
// that was dialed to a freshly re-resolved IP (DNS rebinding).
type spoofRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c *spoofRemoteConn) RemoteAddr() net.Addr { return c.remote }

// resolvingDialer returns a dial function that "resolves" the host part of
// addr with resolve, dials realAddr (where the test server actually listens)
// and reports the resolved IP as the connection remote address, mimicking
// what the kernel would report after a real DNS lookup.
func resolvingDialer(resolve func(host string) string, realAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		resolved := resolve(host)
		var d net.Dialer
		conn, err := d.DialContext(ctx, network, realAddr)
		if err != nil {
			return nil, err
		}
		portNum, _ := strconv.Atoi(port)
		return &spoofRemoteConn{Conn: conn, remote: &net.TCPAddr{IP: net.ParseIP(resolved), Port: portNum}}, nil
	}
}

// sequenceResolver hands out the given IPs in order, repeating the last one,
// simulating a DNS server whose answers change between lookups.
type sequenceResolver struct {
	mu  sync.Mutex
	ips []string
	idx int
}

func (r *sequenceResolver) resolve(_ string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ip := r.ips[min(r.idx, len(r.ips)-1)]
	r.idx++
	return ip
}

type policyObservation struct {
	ip       net.IP
	verified bool
}

// recordingPolicy wraps a TargetIPPolicy and records every invocation.
type recordingPolicy struct {
	mu   sync.Mutex
	seen []policyObservation
	base goproxy.TargetIPPolicy
}

func (p *recordingPolicy) policy() goproxy.TargetIPPolicy {
	return func(ctx *goproxy.ProxyCtx, ip net.IP, verified bool) error {
		p.mu.Lock()
		p.seen = append(p.seen, policyObservation{ip: ip, verified: verified})
		p.mu.Unlock()
		return p.base(ctx, ip, verified)
	}
}

func (p *recordingPolicy) observations() []policyObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]policyObservation(nil), p.seen...)
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return network
}

// newIsolatedProxy returns a proxy detached from any proxy configuration
// coming from the environment (HTTP_PROXY/HTTPS_PROXY), so tests are
// hermetic regardless of the machine they run on.
func newIsolatedProxy() *goproxy.ProxyHttpServer {
	proxy := goproxy.NewProxyHttpServer()
	proxy.ConnectDial = nil
	proxy.Tr.Proxy = nil
	return proxy
}

func proxyClient(t *testing.T, proxyURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(proxyURL)
	require.NoError(t, err)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

func doGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(url)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp
}

// rawConnect sends a CONNECT request to the proxy over a raw TCP connection
// and returns the connection (the tunnel, on 2xx) together with the response.
func rawConnect(t *testing.T, proxyAddr, target string) (net.Conn, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	require.NoError(t, err)
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	return conn, resp
}

// startEchoServer starts a TCP server that echoes back everything it receives.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	return ln
}

// The resolver answers an allowed IP first and a forbidden IP on the next
// lookup. The proxy must check the IP of every connection it actually dials,
// so the second request must not slip into the forbidden network.
func TestTargetIPPolicyHTTPDNSRebinding(t *testing.T) {
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	proxy := newIsolatedProxy()
	resolver := &sequenceResolver{ips: []string{"127.0.0.1", "10.0.0.1"}}
	proxy.Tr.DialContext = resolvingDialer(resolver.resolve, origin.Listener.Addr().String())
	// Force a fresh dial (and a fresh DNS answer) for every request.
	proxy.Tr.DisableKeepAlives = true
	recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, "10.0.0.0/8"))}
	proxy.TargetIPPolicy = recorder.policy()
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := proxyClient(t, proxySrv.URL)

	resp := doGet(t, client, "http://spoof.test/")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = doGet(t, client, "http://spoof.test/")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	require.EqualValues(t, 1, originHits.Load(), "request to a forbidden IP must never reach the origin")
	seen := recorder.observations()
	require.Len(t, seen, 2)
	require.Equal(t, "127.0.0.1", seen[0].ip.String())
	require.True(t, seen[0].verified)
	require.Equal(t, "10.0.0.1", seen[1].ip.String())
	require.True(t, seen[1].verified)
}

// Same DNS-rebinding scenario, but through a CONNECT tunnel.
func TestTargetIPPolicyConnectDNSRebinding(t *testing.T) {
	echo := startEchoServer(t)

	proxy := newIsolatedProxy()
	resolver := &sequenceResolver{ips: []string{"127.0.0.1", "10.0.0.1"}}
	proxy.Tr.DialContext = resolvingDialer(resolver.resolve, echo.Addr().String())
	recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, "10.0.0.0/8"))}
	proxy.TargetIPPolicy = recorder.policy()
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	// First CONNECT: the hostname resolves to an allowed IP, the tunnel works.
	conn, resp := rawConnect(t, proxySrv.Listener.Addr().String(), "spoof.test:443")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, err := conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
	_ = conn.Close()

	// Second CONNECT: the hostname re-resolves to a forbidden IP. Even though
	// the first resolution was allowed, the proxy must refuse the tunnel.
	conn, resp = rawConnect(t, proxySrv.Listener.Addr().String(), "spoof.test:443")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	_ = conn.Close()

	seen := recorder.observations()
	require.Len(t, seen, 2)
	require.Equal(t, "127.0.0.1", seen[0].ip.String())
	require.True(t, seen[0].verified)
	require.Equal(t, "10.0.0.1", seen[1].ip.String())
	require.True(t, seen[1].verified)
}

func TestTargetIPPolicyLiteralAddresses(t *testing.T) {
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	newProxy := func(network string) (*recordingPolicy, *http.Client, func()) {
		proxy := newIsolatedProxy()
		recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, network))}
		proxy.TargetIPPolicy = recorder.policy()
		proxySrv := httptest.NewServer(proxy)
		return recorder, proxyClient(t, proxySrv.URL), proxySrv.Close
	}

	t.Run("AllowedLiteralIPv4", func(t *testing.T) {
		recorder, client, cleanup := newProxy("10.0.0.0/8")
		defer cleanup()

		resp := doGet(t, client, origin.URL+"/")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		seen := recorder.observations()
		require.Len(t, seen, 1)
		require.Equal(t, "127.0.0.1", seen[0].ip.String())
		require.True(t, seen[0].verified)
	})

	t.Run("DeniedLiteralIPv4", func(t *testing.T) {
		originHits.Store(0)
		recorder, client, cleanup := newProxy("127.0.0.0/8")
		defer cleanup()

		resp := doGet(t, client, origin.URL+"/")
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		require.EqualValues(t, 0, originHits.Load(), "request to a forbidden literal IP must never reach the origin")

		seen := recorder.observations()
		require.Len(t, seen, 1)
		require.Equal(t, "127.0.0.1", seen[0].ip.String())
		require.True(t, seen[0].verified)
	})

	t.Run("DeniedLiteralIPv4Connect", func(t *testing.T) {
		echo := startEchoServer(t)
		proxy := newIsolatedProxy()
		recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, "127.0.0.0/8"))}
		proxy.TargetIPPolicy = recorder.policy()
		proxySrv := httptest.NewServer(proxy)
		defer proxySrv.Close()

		conn, resp := rawConnect(t, proxySrv.Listener.Addr().String(), echo.Addr().String())
		require.Equal(t, http.StatusBadGateway, resp.StatusCode)
		_ = conn.Close()

		seen := recorder.observations()
		require.Len(t, seen, 1)
		require.Equal(t, "127.0.0.1", seen[0].ip.String())
		require.True(t, seen[0].verified)
	})
}

// A dialed IPv4-mapped IPv6 address ("::ffff:127.0.0.1") must be normalized
// to its 4-byte form before the policy sees it, so IPv4 blocklists apply.
func TestTargetIPPolicyIPv4MappedIPv6(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	proxy := newIsolatedProxy()
	proxy.Tr.DialContext = resolvingDialer(func(string) string { return "::ffff:127.0.0.1" }, origin.Listener.Addr().String())
	recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, "127.0.0.0/8"))}
	proxy.TargetIPPolicy = recorder.policy()
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp := doGet(t, proxyClient(t, proxySrv.URL), "http://spoof.test/")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	seen := recorder.observations()
	require.Len(t, seen, 1)
	require.Equal(t, "127.0.0.1", seen[0].ip.String())
	require.Equal(t, net.IPv4len, len(seen[0].ip), "IPv4-mapped IPv6 addresses must be normalized to 4-byte form")
}

// Connections that pass the policy stay in the transport pool and are reused
// without re-checks; a connection is only ever reused for the target it was
// verified for.
func TestTargetIPPolicyConnectionPoolReuse(t *testing.T) {
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	proxy := newIsolatedProxy()
	recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, "10.0.0.0/8"))}
	proxy.TargetIPPolicy = recorder.policy()
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := proxyClient(t, proxySrv.URL)
	for range 3 {
		resp := doGet(t, client, origin.URL+"/")
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	require.EqualValues(t, 3, originHits.Load())
	seen := recorder.observations()
	require.Len(t, seen, 1, "pooled connections are verified once, when dialed")
	require.Equal(t, "127.0.0.1", seen[0].ip.String())
	require.True(t, seen[0].verified)
}

// Allowed TLS connections must keep the original hostname: the proxy checks
// the dialed IP but never rewrites the dial address, so SNI and certificate
// verification still use the requested hostname.
func TestTargetIPPolicyTLSPreservesServerName(t *testing.T) {
	cert, err := signer.SignHost(goproxy.GoproxyCa, []string{"allowed.test"})
	require.NoError(t, err)

	sni := make(chan string, 1)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni <- r.TLS.ServerName
		_, _ = io.WriteString(w, "ok")
	}))
	origin.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	origin.StartTLS()
	defer origin.Close()

	proxy := newIsolatedProxy()
	proxy.Tr.DialContext = resolvingDialer(func(string) string { return "127.0.0.1" }, origin.Listener.Addr().String())
	recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy(mustCIDR(t, "10.0.0.0/8"))}
	proxy.TargetIPPolicy = recorder.policy()
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(goproxy.CA_CERT))

	t.Run("ConnectTunnel", func(t *testing.T) {
		// The client CONNECTs through the proxy and performs the TLS
		// handshake itself, verifying the certificate against the hostname.
		client := &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyURL(mustParseURL(t, proxySrv.URL)),
			TLSClientConfig: &tls.Config{RootCAs: roots},
		}}
		resp := doGet(t, client, "https://allowed.test/")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "allowed.test", <-sni)
	})

	t.Run("AbsoluteForm", func(t *testing.T) {
		// The proxy terminates TLS itself (absolute-form https request
		// forwarded by Tr): the handshake must still use the hostname.
		proxy.Tr.TLSClientConfig = &tls.Config{RootCAs: roots}
		conn, err := net.Dial("tcp", proxySrv.Listener.Addr().String())
		require.NoError(t, err)
		defer conn.Close()
		req, err := http.NewRequest(http.MethodGet, "https://allowed.test/", nil)
		require.NoError(t, err)
		req.RequestURI = ""
		require.NoError(t, req.WriteProxy(conn))
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "allowed.test", <-sni)
	})

	for _, obs := range recorder.observations() {
		require.Equal(t, "127.0.0.1", obs.ip.String())
		require.True(t, obs.verified)
	}
}

// When the proxy cannot observe the real target IP (custom dial hooks,
// upstream proxy), the policy is told the connection is unverified, and an
// unverified connection must not be treated as allowed.
func TestTargetIPPolicyUnverifiedIsNotAllowed(t *testing.T) {
	t.Run("CustomConnectDial", func(t *testing.T) {
		echo := startEchoServer(t)
		proxy := newIsolatedProxy()
		// An opaque custom dialer: it happens to reach the real target, but
		// the proxy cannot know that (it could be an upstream proxy).
		proxy.ConnectDial = func(network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.Dial(network, echo.Addr().String())
		}
		// The blocklist is empty: only the unverified status can reject.
		recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy()}
		proxy.TargetIPPolicy = recorder.policy()
		proxySrv := httptest.NewServer(proxy)
		defer proxySrv.Close()

		conn, resp := rawConnect(t, proxySrv.Listener.Addr().String(), "spoof.test:443")
		require.Equal(t, http.StatusBadGateway, resp.StatusCode)
		_ = conn.Close()

		seen := recorder.observations()
		require.Len(t, seen, 1)
		require.False(t, seen[0].verified, "custom ConnectDial connections must be reported unverified")
	})

	t.Run("PerRequestDialer", func(t *testing.T) {
		echo := startEchoServer(t)
		proxy := newIsolatedProxy()
		proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
			ctx.Dialer = func(_ context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.Dial(network, echo.Addr().String())
			}
			return goproxy.OkConnect, host
		})
		recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy()}
		proxy.TargetIPPolicy = recorder.policy()
		proxySrv := httptest.NewServer(proxy)
		defer proxySrv.Close()

		conn, resp := rawConnect(t, proxySrv.Listener.Addr().String(), "spoof.test:443")
		require.Equal(t, http.StatusBadGateway, resp.StatusCode)
		_ = conn.Close()

		seen := recorder.observations()
		require.Len(t, seen, 1)
		require.False(t, seen[0].verified, "ProxyCtx.Dialer connections must be reported unverified")
	})

	t.Run("UpstreamProxy", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}))
		defer origin.Close()

		proxy := newIsolatedProxy()
		proxy.Tr.DialContext = resolvingDialer(func(string) string { return "127.0.0.1" }, origin.Listener.Addr().String())
		// Requests are forwarded to an upstream proxy: the dialed peer is the
		// upstream, and the real target IP is chosen by it.
		proxy.Tr.Proxy = http.ProxyURL(mustParseURL(t, "http://upstream.test:8888"))
		recorder := &recordingPolicy{base: goproxy.NewCIDRBlocklistPolicy()}
		proxy.TargetIPPolicy = recorder.policy()
		proxySrv := httptest.NewServer(proxy)
		defer proxySrv.Close()

		resp := doGet(t, proxyClient(t, proxySrv.URL), "http://spoof.test/")
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

		seen := recorder.observations()
		require.Len(t, seen, 1)
		require.False(t, seen[0].verified, "upstream proxy connections must be reported unverified")
	})
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u
}
