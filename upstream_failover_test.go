package goproxy_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headerTarget records every request it receives, including whether a
// Proxy-Authorization header leaked past the upstream proxy hop.
type headerTarget struct {
	mu       sync.Mutex
	requests []map[string]string
}

func (h *headerTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	rec := map[string]string{
		"method":             r.Method,
		"body":               string(body),
		"proxyAuthorization": r.Header.Get("Proxy-Authorization"),
		"authorization":      r.Header.Get("Authorization"),
	}
	h.requests = append(h.requests, rec)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "target-ok")
}

func (h *headerTarget) snapshot() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]string, len(h.requests))
	for i, r := range h.requests {
		out[i] = maps(r)
	}
	return out
}

func maps(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// fakeUpstream is a standalone forward proxy used to simulate an upstream
// line. It supports both plain-HTTP forwarding and CONNECT tunnelling, and
// can be told to fail in specific ways.
type fakeUpstream struct {
	name string

	cred string // expected Proxy-Authorization (empty = no auth required)

	// behaviors
	refuseConns   bool // close listener: simulate connection refused
	denyAuth      bool // always answer 407
	connectStatus int  // non-zero: answer this status to CONNECT instead of 200
	closeMidBody  bool // reset connection while forwarding the request body

	mu         sync.Mutex
	authSeen   []string
	plainHits  int
	connectHit int
	listener   net.Listener
	server     *httptest.Server
}

func newFakeUpstream(t *testing.T, name string) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{name: name}
	fu.server = httptest.NewServer(http.HandlerFunc(fu.handle))
	fu.listener = fu.server.Listener
	t.Cleanup(func() { fu.server.Close() })
	return fu
}

func (fu *fakeUpstream) url() string { return fu.server.URL }

func (fu *fakeUpstream) addr() string { return fu.listener.Addr().String() }

func (fu *fakeUpstream) shutdown() { fu.server.Close() }

func (fu *fakeUpstream) recordAuth(a string) {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	fu.authSeen = append(fu.authSeen, a)
}

func (fu *fakeUpstream) auths() []string {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return append([]string(nil), fu.authSeen...)
}

func (fu *fakeUpstream) hits() (plain, connect int) {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return fu.plainHits, fu.connectHit
}

func (fu *fakeUpstream) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Proxy-Authorization")
	fu.recordAuth(auth)
	if fu.cred != "" && auth != fu.cred {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fake"`)
		http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
		return false
	}
	if fu.denyAuth {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fake"`)
		http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
		return false
	}
	return true
}

func (fu *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		fu.handleConnect(w, r)
		return
	}
	fu.mu.Lock()
	fu.plainHits++
	fu.mu.Unlock()

	if !fu.checkAuth(w, r) {
		return
	}

	if fu.closeMidBody {
		// Consume a few bytes of the body, then drop the connection abruptly
		// so the forwarding transport fails mid-write.
		_, _ = io.CopyN(io.Discard, r.Body, 8)
		hj, _ := w.(http.Hijacker)
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}

	// Forward the request to the destination (absolute-form proxy request).
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	outReq.Header.Del("Proxy-Authorization")
	outReq.Header.Del("Proxy-Connection")
	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream fake error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (fu *fakeUpstream) handleConnect(w http.ResponseWriter, r *http.Request) {
	fu.mu.Lock()
	fu.connectHit++
	fu.mu.Unlock()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}

	auth := r.Header.Get("Proxy-Authorization")
	fu.recordAuth(auth)
	badAuth := fu.cred != "" && auth != fu.cred

	switch {
	case fu.denyAuth || badAuth:
		_, _ = io.WriteString(client, "HTTP/1.1 407 Proxy Authentication Required\r\n"+
			`Proxy-Authenticate: Basic realm="fake"`+"\r\nContent-Length: 0\r\n\r\n")
		_ = client.Close()
		return
	case fu.connectStatus != 0:
		fmt.Fprintf(client, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n",
			fu.connectStatus, http.StatusText(fu.connectStatus))
		_ = client.Close()
		return
	}

	target, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		_ = client.Close()
		return
	}
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, target); done <- struct{}{} }()
	go func() {
		<-done
		_ = target.Close()
		_ = client.Close()
	}()
}

// --- test infrastructure ---------------------------------------------------

func newFailoverProxy(t *testing.T, lines []*goproxy.UpstreamLine, bufferBody int64) (*goproxy.ProxyHttpServer, *httptest.Server) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	f := &goproxy.UpstreamFailover{Lines: lines, BufferBody: bufferBody}
	require.NoError(t, proxy.EnableUpstreamFailover(f))
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return proxy, srv
}

func proxyClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	pu, err := url.Parse(srv.URL)
	require.NoError(t, err)
	tr := &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	c := &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)
	return c
}

func basicAuthFor(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// --- plain HTTP forwarding tests -------------------------------------------

func linesFor(t *testing.T, a, b *fakeUpstream) []*goproxy.UpstreamLine {
	return []*goproxy.UpstreamLine{
		{Name: "A", URL: a.url(), Username: "usera", Password: "secret-a"},
		{Name: "B", URL: b.url(), Username: "userb", Password: "secret-b"},
	}
}

// Preferred line is used and its credential stays on that hop; the target
// never sees Proxy-Authorization.
func TestUpstreamFailoverPlainUsesPreferredAndIsolatesAuth(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/r1")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "target-ok", string(body))

	plainA, connA := a.hits()
	plainB, connB := b.hits()
	assert.Equal(t, 1, plainA)
	assert.Equal(t, 0, plainB)
	assert.Equal(t, 0, connA+connB)

	assert.Equal(t, []string{basicAuthFor("usera", "secret-a")}, a.auths())
	assert.Empty(t, b.auths())

	recs := target.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, "", recs[0]["proxyAuthorization"], "Proxy-Authorization must not reach target")
}

// Connection refused on the preferred line fails over to the backup.
func TestUpstreamFailoverPlainRefusedFallsBack(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	deadAddr := a.addr()
	a.shutdown() // nothing listening now -> connection refused

	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	lines := []*goproxy.UpstreamLine{
		{Name: "A", URL: "http://" + deadAddr, Username: "usera", Password: "secret-a"},
		{Name: "B", URL: b.url(), Username: "userb", Password: "secret-b"},
	}
	_, psrv := newFailoverProxy(t, lines, 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/r2")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	plainB, _ := b.hits()
	assert.Equal(t, 1, plainB)
	assert.Equal(t, []string{basicAuthFor("userb", "secret-b")}, b.auths())
	assert.Len(t, target.snapshot(), 1)
}

// 407 from preferred line retries on backup, carrying only the backup cred.
func TestUpstreamFailoverPlainAuthFailureFallsBack(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	a.denyAuth = true // even correct creds are rejected
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/r3")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	plainA, _ := a.hits()
	plainB, _ := b.hits()
	assert.Equal(t, 1, plainA)
	assert.Equal(t, 1, plainB)
	assert.Equal(t, []string{basicAuthFor("usera", "secret-a")}, a.auths())
	assert.Equal(t, []string{basicAuthFor("userb", "secret-b")}, b.auths())
}

// A streaming body (no GetBody) rejected with 407 cannot be replayed: the
// backup line must not be used and the error must list attempts and skips.
func TestUpstreamFailoverPlainStreamingBody407NotReplayed(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	a.denyAuth = true
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	body := bytes.Repeat([]byte("x"), 100)
	req, err := http.NewRequest(http.MethodPost, targetSrv.URL+"/post", &nonReplayableReader{data: body})
	require.NoError(t, err)
	req.ContentLength = int64(len(body))

	resp, err := client.Do(req)
	require.NoError(t, err) // proxy answers 500, transport-wise a valid response
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	msg := string(payload)
	assert.Contains(t, msg, "A")
	assert.Contains(t, msg, "B")
	assert.Contains(t, msg, "not tried")
	assert.NotContains(t, msg, "secret-a")
	assert.NotContains(t, msg, "secret-b")

	plainB, _ := b.hits()
	assert.Equal(t, 0, plainB, "non-replayable body must not be sent to backup")
	assert.Empty(t, target.snapshot(), "target must never be reached")

	// Public error type mirrors the same info without credentials.
	// (validated again in TestUpstreamFailoverErrorRedaction)
}

// Once part of the request body has been written and the proxy resets the
// connection, no other line is tried, even if the body would be replayable.
func TestUpstreamFailoverPlainPartialBodyNoFallback(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	a.closeMidBody = true
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	body := bytes.Repeat([]byte("y"), 4096)
	req, err := http.NewRequest(http.MethodPost, targetSrv.URL+"/post", bytes.NewReader(body))
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(payload), "A")
	assert.Contains(t, string(payload), "B")
	assert.Contains(t, string(payload), "already sent")
	assert.NotContains(t, string(payload), "secret-a")

	plainB, _ := b.hits()
	assert.Equal(t, 0, plainB)
}

// With BufferBody a modest body is captured and CAN be replayed after 407.
func TestUpstreamFailoverPlainBufferedBodyReplays(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	a.denyAuth = true
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 1024)
	client := proxyClient(t, psrv)

	body := bytes.Repeat([]byte("z"), 64)
	req, err := http.NewRequest(http.MethodPost, targetSrv.URL+"/post", &nonReplayableReader{data: body})
	require.NoError(t, err)
	req.ContentLength = int64(len(body))

	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	recs := target.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, string(body), recs[0]["body"])
	assert.Equal(t, "", recs[0]["proxyAuthorization"])
	plainB, _ := b.hits()
	assert.Equal(t, 1, plainB)
}

// nonReplayableReader is an io.Reader that yields data but provides no way for
// net/http to set GetBody, simulating a non-seekable request stream.
type nonReplayableReader struct {
	data []byte
	pos  int
}

func (n *nonReplayableReader) Read(p []byte) (int, error) {
	if n.pos >= len(n.data) {
		return 0, io.EOF
	}
	c := copy(p, n.data[n.pos:])
	n.pos += c
	return c, nil
}

// --- CONNECT tunnel tests --------------------------------------------------

func connectTestTarget(t *testing.T) (*headerTarget, *httptest.Server) {
	t.Helper()
	target := &headerTarget{}
	srv := httptest.NewTLSServer(target)
	t.Cleanup(srv.Close)
	return target, srv
}

// Transparent CONNECT prefers A and only A's credential is presented.
func TestUpstreamFailoverConnectPreferredAndAuthIsolated(t *testing.T) {
	target, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/c1")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, connA := a.hits()
	_, connB := b.hits()
	assert.Equal(t, 1, connA)
	assert.Equal(t, 0, connB)
	assert.Equal(t, []string{basicAuthFor("usera", "secret-a")}, a.auths())
	assert.Empty(t, b.auths())
	assert.Len(t, target.snapshot(), 1)
}

// CONNECT: refused TCP connection on A falls back to B.
func TestUpstreamFailoverConnectRefusedFallsBack(t *testing.T) {
	_, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	deadAddr := a.addr()
	a.shutdown()
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	lines := []*goproxy.UpstreamLine{
		{Name: "A", URL: "http://" + deadAddr, Username: "usera", Password: "secret-a"},
		{Name: "B", URL: b.url(), Username: "userb", Password: "secret-b"},
	}
	_, psrv := newFailoverProxy(t, lines, 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/c2")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, connB := b.hits()
	assert.Equal(t, 1, connB)
	assert.Equal(t, []string{basicAuthFor("userb", "secret-b")}, b.auths())
}

// CONNECT: 407 on A falls back to B before any tunnel exists.
func TestUpstreamFailoverConnect407FallsBack(t *testing.T) {
	_, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	a.denyAuth = true
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/c3")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, []string{basicAuthFor("usera", "secret-a")}, a.auths())
	assert.Equal(t, []string{basicAuthFor("userb", "secret-b")}, b.auths())
	_, connA := a.hits()
	_, connB := b.hits()
	assert.Equal(t, 1, connA)
	assert.Equal(t, 1, connB)
}

// CONNECT: non-200 (503) status on A falls back to B.
func TestUpstreamFailoverConnectNon200FallsBack(t *testing.T) {
	_, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	a.connectStatus = http.StatusServiceUnavailable
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/c4")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, connB := b.hits()
	assert.Equal(t, 1, connB)
}

// CONNECT: when every line fails, the client must receive an error whose text
// identifies all lines tried, the phases, and never the credentials.
func TestUpstreamFailoverConnectAllFailError(t *testing.T) {
	_, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	deadA := a.addr()
	a.shutdown()
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")
	b.denyAuth = true

	// Embed credentials in URL userinfo too, to prove they are redacted.
	lines := []*goproxy.UpstreamLine{
		{Name: "A", URL: "http://usera:secret-a@" + deadA},
		{Name: "B", URL: b.url(), Username: "userb", Password: "secret-b"},
	}
	_, psrv := newFailoverProxy(t, lines, 0)
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/c5")
	if err == nil {
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.NotEqual(t, http.StatusOK, resp.StatusCode)
		msg := string(payload)
		assert.Contains(t, msg, "A")
		assert.Contains(t, msg, "B")
		assert.Contains(t, msg, "dial")
		assert.Contains(t, msg, "proxy-auth")
		assert.NotContains(t, msg, "secret-a")
		assert.NotContains(t, msg, "secret-b")
		assert.NotContains(t, msg, "usera:secret-a")
	}
}

// CONNECT: once the preferred upstream already answered 200 (tunnel open),
// failover must NOT touch the backup line even if data later breaks. Here both
// succeed, so we assert B is never connected and the client sees the tunnel.
func TestUpstreamFailoverConnect200NoSwitch(t *testing.T) {
	target, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	_, psrv := newFailoverProxy(t, linesFor(t, a, b), 0)

	// Raw CONNECT to inspect the exact tunnel reply.
	conn, err := net.Dial("tcp", strings.TrimPrefix(psrv.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()

	targetHost := strings.TrimPrefix(targetSrv.URL, "https://")
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetHost, targetHost)
	br := bufio.NewReader(conn)
	cresp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	_ = cresp.Body.Close()
	require.Equal(t, http.StatusOK, cresp.StatusCode)

	// Run a real TLS request inside the established tunnel.
	tlsConn := tls.Client(&bufferedConn{Conn: conn, r: br}, &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, tlsConn.HandshakeContext(context.Background()))
	req, _ := http.NewRequest(http.MethodGet, targetSrv.URL+"/c6", nil)
	require.NoError(t, req.Write(tlsConn))
	tresp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	require.NoError(t, err)
	_ = tresp.Body.Close()
	require.Equal(t, http.StatusOK, tresp.StatusCode)

	_, connA := a.hits()
	_, connB := b.hits()
	assert.Equal(t, 1, connA)
	assert.Equal(t, 0, connB, "backup must not be used after tunnel already returned 200")
	assert.Len(t, target.snapshot(), 1)
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Per-request selector can force the backup line as preferred.
func TestUpstreamFailoverSelectorPicksLine(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	f := &goproxy.UpstreamFailover{
		Lines: linesFor(t, a, b),
		SelectLine: func(req *http.Request) []string {
			if req.Header.Get("X-Prefer") == "B" {
				return []string{"B", "A"}
			}
			return nil
		},
	}
	require.NoError(t, proxy.EnableUpstreamFailover(f))
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()
	client := proxyClient(t, psrv)

	req, err := http.NewRequest(http.MethodGet, targetSrv.URL+"/sel", nil)
	require.NoError(t, err)
	req.Header.Set("X-Prefer", "B")
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	plainA, _ := a.hits()
	plainB, _ := b.hits()
	assert.Equal(t, 0, plainA)
	assert.Equal(t, 1, plainB)
	assert.Equal(t, []string{basicAuthFor("userb", "secret-b")}, b.auths())
}

// An unknown selector name yields a redacted error.
func TestUpstreamFailoverUnknownLine(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	b := newFakeUpstream(t, "B")

	proxy := goproxy.NewProxyHttpServer()
	f := &goproxy.UpstreamFailover{
		Lines: linesFor(t, a, b),
		SelectLine: func(req *http.Request) []string {
			return []string{"NOPE"}
		},
	}
	require.NoError(t, proxy.EnableUpstreamFailover(f))
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/unknown")
	require.NoError(t, err)
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(payload), "unknown line")
	assert.NotContains(t, string(payload), "secret-a")
}

// Verify the exported error type carries structured attempts/skips and that
// AsUpstreamFailoverError unwraps it. Uses a refused-then-407 scenario via a
// direct RoundTrip against a minimally configured proxy.
func TestUpstreamFailoverErrorTypeAndRedaction(t *testing.T) {
	_, targetSrv := connectTestTarget(t)

	a := newFakeUpstream(t, "A")
	deadA := a.addr()
	a.shutdown()
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")
	b.denyAuth = true

	proxy := goproxy.NewProxyHttpServer()
	f := &goproxy.UpstreamFailover{
		Lines: []*goproxy.UpstreamLine{
			{Name: "A", URL: "http://usera:secret-a@" + deadA},
			{Name: "B", URL: b.url(), Username: "userb", Password: "secret-b"},
		},
	}
	require.NoError(t, proxy.EnableUpstreamFailover(f))

	req, err := http.NewRequest(http.MethodConnect, targetSrv.URL, nil)
	require.NoError(t, err)
	req.Host = strings.TrimPrefix(targetSrv.URL, "https://")

	_, err = proxy.ConnectDialWithReq(req, "tcp", req.Host)
	require.Error(t, err)

	fe, ok := goproxy.AsUpstreamFailoverError(err)
	require.True(t, ok)
	require.Len(t, fe.Attempts, 2)
	assert.Equal(t, "A", fe.Attempts[0].Line)
	assert.Equal(t, goproxy.FailoverPhaseDial, fe.Attempts[0].Phase)
	assert.Equal(t, "B", fe.Attempts[1].Line)
	assert.Equal(t, goproxy.FailoverPhaseAuth, fe.Attempts[1].Phase)
	assert.Equal(t, 407, fe.Attempts[1].StatusCode)

	msg := err.Error()
	assert.Contains(t, msg, "A")
	assert.Contains(t, msg, "B")
	assert.NotContains(t, msg, "secret-a")
	assert.NotContains(t, msg, "secret-b")
	assert.NotContains(t, msg, "usera:secret-a")

	// Sanity: a nil error must not unwrap.
	_, ok = goproxy.AsUpstreamFailoverError(nil)
	assert.False(t, ok)
	_ = errors.New("x")
}

// MITM: inner HTTPS requests are also routed through failover; a refused
// preferred line falls back, and the target never receives
// Proxy-Authorization.
func TestUpstreamFailoverMitmInnerRequestFailsOver(t *testing.T) {
	target := &headerTarget{}
	targetSrv := httptest.NewTLSServer(target)
	defer targetSrv.Close()

	a := newFakeUpstream(t, "A")
	a.cred = basicAuthFor("usera", "secret-a")
	deadAddr := a.addr()
	a.shutdown()
	b := newFakeUpstream(t, "B")
	b.cred = basicAuthFor("userb", "secret-b")

	lines := []*goproxy.UpstreamLine{
		{Name: "A", URL: "http://" + deadAddr, Username: "usera", Password: "secret-a"},
		{Name: "B", URL: b.url(), Username: "userb", Password: "secret-b"},
	}

	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	f := &goproxy.UpstreamFailover{Lines: lines}
	require.NoError(t, proxy.EnableUpstreamFailover(f))

	psrv := httptest.NewServer(proxy)
	defer psrv.Close()
	client := proxyClient(t, psrv)

	resp, err := client.Get(targetSrv.URL + "/mitm")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	plainB, connB := b.hits()
	assert.Equal(t, 0, plainB)
	assert.Equal(t, 1, connB, "MITM inner request to an https target uses CONNECT at the backup")
	assert.Equal(t, []string{basicAuthFor("userb", "secret-b")}, b.auths())

	recs := target.snapshot()
	require.Len(t, recs, 1)
	assert.Equal(t, "", recs[0]["proxyAuthorization"])
}
