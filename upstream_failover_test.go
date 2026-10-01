package goproxy_test

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
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
	"github.com/stretchr/testify/require"
)

// fakeUpstreamMode controls how a fake upstream proxy behaves.
type fakeUpstreamMode int

const (
	modeOK            fakeUpstreamMode = iota
	modeRefuse                         // no listener / dial refused
	mode407                            // always reply 407
	modeCloseAfter100                  // send 100-continue then close while receiving body
	mode407BeforeBody                  // reply 407 to a body-bearing request without reading body
)

type fakeUpstream struct {
	name     string
	user     string
	pass     string
	mode     fakeUpstreamMode
	listener net.Listener

	mu       sync.Mutex
	hits     int
	authSeen []string
}

func newFakeUpstream(t *testing.T, name, user, pass string, mode fakeUpstreamMode) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{name: name, user: user, pass: pass, mode: mode}
	if mode == modeRefuse {
		// Reserve a port then close it, guaranteeing a refused dial.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		fu.listener = l
		require.NoError(t, l.Close())
		return fu
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fu.listener = l
	go fu.serve()
	t.Cleanup(func() { _ = l.Close() })
	return fu
}

func (fu *fakeUpstream) addr() string { return fu.listener.Addr().String() }

func (fu *fakeUpstream) hitCount() int {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return fu.hits
}

func (fu *fakeUpstream) auths() []string {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return append([]string(nil), fu.authSeen...)
}

func (fu *fakeUpstream) record(auth string) {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	fu.hits++
	fu.authSeen = append(fu.authSeen, auth)
}

func (fu *fakeUpstream) expectedAuth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(fu.user+":"+fu.pass))
}

func (fu *fakeUpstream) serve() {
	for {
		c, err := fu.listener.Accept()
		if err != nil {
			return
		}
		go fu.handle(c)
	}
}

func (fu *fakeUpstream) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	auth := req.Header.Get("Proxy-Authorization")
	fu.record(auth)

	if req.Method == http.MethodConnect {
		fu.handleConnect(c, req)
		return
	}
	fu.handleForward(c, br, req, auth)
}

func (fu *fakeUpstream) handleConnect(c net.Conn, req *http.Request) {
	switch fu.mode {
	case mode407, mode407BeforeBody:
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 0\r\n\r\n")
		return
	case modeCloseAfter100:
		// 200 then immediate close: tunnel established, then drops.
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		_ = c.Close()
		return
	}
	if req.Header.Get("Proxy-Authorization") != fu.expectedAuth() {
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 0\r\n\r\n")
		return
	}
	target, err := net.Dial("tcp", req.Host)
	if err != nil {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer target.Close()
	io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, c); done <- struct{}{} }()
	go func() { io.Copy(c, target); done <- struct{}{} }()
	<-done
}

func (fu *fakeUpstream) handleForward(c net.Conn, br *bufio.Reader, req *http.Request, auth string) {
	expect := strings.EqualFold(req.Header.Get("Expect"), "100-continue")

	challenge := func() {
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}

	switch fu.mode {
	case mode407:
		challenge()
		return
	case mode407BeforeBody:
		if expect {
			challenge() // before touching the body
			return
		}
	case modeCloseAfter100:
		if expect {
			io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n")
			// Drain a couple of bytes then abruptly close mid-body.
			buf := make([]byte, 4)
			_, _ = io.ReadFull(br, buf)
			_ = c.Close()
			return
		}
	}

	if auth != fu.expectedAuth() {
		challenge()
		return
	}

	// Read the (possibly chunked) request body so it can be forwarded.
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
	}

	// Forward to the origin over a fresh, direct connection in origin form.
	target, err := net.Dial("tcp", req.Host)
	if err != nil {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	defer target.Close()

	out := &http.Request{
		Method:        req.Method,
		URL:           req.URL,
		Host:          req.Host,
		Header:        req.Header.Clone(),
		Body:          io.NopCloser(strings.NewReader(string(bodyBytes))),
		ContentLength: int64(len(bodyBytes)),
	}
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Expect")
	out.Header.Del("Transfer-Encoding")
	out.Header.Set("Connection", "close")
	if len(bodyBytes) == 0 {
		out.Body = http.NoBody
	}
	if err := out.Write(target); err != nil {
		return
	}
	respBuf := bufio.NewReader(target)
	resp, err := http.ReadResponse(respBuf, out)
	if err != nil {
		return
	}
	resp.Header.Set("Connection", "close")
	_ = resp.Write(c)
}

// recordingTarget is an HTTP and HTTPS origin that records headers it received.
type recordingTarget struct {
	server    *httptest.Server
	tlsServer *httptest.Server
	mu        sync.Mutex
	records   []map[string]string
}

func newRecordingTarget(t *testing.T) *recordingTarget {
	t.Helper()
	rt := &recordingTarget{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record := map[string]string{}
		for k := range r.Header {
			record[k] = r.Header.Get(k)
		}
		rt.mu.Lock()
		rt.records = append(rt.records, record)
		rt.mu.Unlock()
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "echo:%s", string(body))
	})
	rt.server = httptest.NewServer(h)
	rt.tlsServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record := map[string]string{}
		for k := range r.Header {
			record[k] = r.Header.Get(k)
		}
		rt.mu.Lock()
		rt.records = append(rt.records, record)
		rt.mu.Unlock()
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "echo:%s", string(body))
	}))
	t.Cleanup(func() {
		rt.server.Close()
		rt.tlsServer.Close()
	})
	return rt
}

func (rt *recordingTarget) snapshot() []map[string]string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]map[string]string, len(rt.records))
	copy(out, rt.records)
	return out
}

type failoverHarness struct {
	proxy   *goproxy.ProxyHttpServer
	server  *httptest.Server
	primary *fakeUpstream
	backup  *fakeUpstream
	target  *recordingTarget
}

func newFailoverHarness(t *testing.T, primaryMode, backupMode fakeUpstreamMode, opts ...goproxy.FailoverOption) *failoverHarness {
	t.Helper()
	target := newRecordingTarget(t)
	primary := newFakeUpstream(t, "primary", "puser", "ppass", primaryMode)
	backup := newFakeUpstream(t, "backup", "buser", "bpass", backupMode)

	f := goproxy.NewUpstreamFailover([]goproxy.UpstreamLine{
		{Name: "primary", Addr: primary.addr(), Username: "puser", Password: "ppass"},
		{Name: "backup", Addr: backup.addr(), Username: "buser", Password: "bpass"},
	}, opts...)

	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false
	f.Attach(proxy)
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	return &failoverHarness{
		proxy: proxy, server: server,
		primary: primary, backup: backup, target: target,
	}
}

func (h *failoverHarness) client() *http.Client {
	pu, _ := url.Parse(h.server.URL)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(pu),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func assertNoProxyHeaders(t *testing.T, rec map[string]string) {
	t.Helper()
	for k := range rec {
		if strings.EqualFold(k, "Proxy-Authorization") || strings.EqualFold(k, "Proxy-Connection") {
			t.Fatalf("proxy hop-by-hop header %q leaked to origin: %q", k, rec[k])
		}
	}
}

// TestForwardDialRefusedFailsOver: a refused connection on the preferred line
// is a pre-body failure, so the identical request is retried on the backup.
func TestForwardDialRefusedFailsOver(t *testing.T) {
	h := newFailoverHarness(t, modeRefuse, modeOK,
		goproxy.WithDialTimeout(2*time.Second),
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	resp, err := h.client().Get(h.target.server.URL + "/p")
	require.NoError(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))

	require.Equal(t, 0, h.primary.hitCount(), "primary refused the dial, it was never reached")
	require.Equal(t, 1, h.backup.hitCount())
	recs := h.target.snapshot()
	require.Len(t, recs, 1)
	assertNoProxyHeaders(t, recs[0])
}

// TestForward407FailsOver: an upstream auth challenge that arrives before the
// body is sent triggers a retry on the backup line with the backup's own
// credentials.
func TestForward407BeforeBodyFailsOver(t *testing.T) {
	h := newFailoverHarness(t, mode407, modeOK,
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	resp, err := h.client().Post(
		h.target.server.URL+"/p", "text/plain", strings.NewReader("payload"))
	require.NoError(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))

	require.Equal(t, 1, h.primary.hitCount())
	require.Equal(t, 1, h.backup.hitCount())
	// Each line only ever saw its own credential; primary's credential was not
	// replayed against the backup and vice versa.
	require.Equal(t, []string{h.primary.expectedAuth()}, h.primary.auths())
	require.Equal(t, []string{h.backup.expectedAuth()}, h.backup.auths())
}

// TestForwardPreferredHeaderPicksBackup: the request-level preference header
// selects the backup first; the primary is never contacted. The control header
// itself must not reach the origin.
func TestForwardPreferredHeaderPicksBackup(t *testing.T) {
	h := newFailoverHarness(t, modeOK, modeOK,
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	req, _ := http.NewRequest(http.MethodGet, h.target.server.URL+"/p", nil)
	req.Header.Set(goproxy.UpstreamPreferenceHeader, "backup")
	resp, err := h.client().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, 0, h.primary.hitCount())
	require.Equal(t, 1, h.backup.hitCount())
	recs := h.target.snapshot()
	require.Len(t, recs, 1)
	_, present := recs[0][goproxy.UpstreamPreferenceHeader]
	require.False(t, present, "preference control header leaked to origin")
	assertNoProxyHeaders(t, recs[0])
}

// TestForwardBodyPartialWriteNoRetry: once part of the request body has been
// committed to the preferred line, the request must not be replayed on the
// backup even though the connection breaks.
func TestForwardBodyPartialWriteNoRetry(t *testing.T) {
	h := newFailoverHarness(t, modeCloseAfter100, modeOK,
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	resp, err := h.client().Post(
		h.target.server.URL+"/p", "text/plain", strings.NewReader("abcdefghij"))
	require.NoError(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "expected 502 after mid-body break, got %s", string(data))
	require.Equal(t, 1, h.primary.hitCount())
	require.Equal(t, 0, h.backup.hitCount(), "backup must never receive a partially-sent request")
	require.Contains(t, string(data), "primary")
	require.NotContains(t, string(data), "ppass")
	require.NotContains(t, string(data), "puser")
}

// TestForwardNonReplayableLargeBodyNotRetried: a body larger than the buffer
// cap is streamed and not replayable; a refused preferred line must not cause a
// second send, and the error states that clearly.
func TestForwardNonReplayableLargeBodyNotRetried(t *testing.T) {
	h := newFailoverHarness(t, modeRefuse, modeOK,
		goproxy.WithMaxBufferedBody(64),
		goproxy.WithDialTimeout(2*time.Second),
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	big := strings.Repeat("z", 128)
	resp, err := h.client().Post(
		h.target.server.URL+"/big", "application/octet-stream", strings.NewReader(big))
	require.NoError(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	require.Equal(t, 0, h.primary.hitCount(), "dial refused: the non-replayable body was never sent")
	require.Equal(t, 0, h.backup.hitCount())
	require.Contains(t, string(data), "primary")
	require.NotContains(t, string(data), "ppass")
}

// TestForwardBothLinesDownReportsTriedLines: the 502 body lists every line
// actually tried and never contains credentials.
func TestForwardBothLinesDownReportsTriedLines(t *testing.T) {
	h := newFailoverHarness(t, modeRefuse, modeRefuse,
		goproxy.WithDialTimeout(2*time.Second),
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	resp, err := h.client().Get(h.target.server.URL + "/p")
	require.NoError(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	body := string(data)
	require.Contains(t, body, "primary")
	require.Contains(t, body, "backup")
	require.NotContains(t, body, "ppass")
	require.NotContains(t, body, "bpass")
	require.NotContains(t, body, "Basic")
}

// TestConnectDialRefusedFailsOver: a refused CONNECT dial retries the backup and
// the TLS request reaches the origin through the tunnel.
func TestConnectDialRefusedFailsOver(t *testing.T) {
	h := newFailoverHarness(t, modeRefuse, modeOK,
		goproxy.WithDialTimeout(2*time.Second),
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	resp, err := h.client().Get(h.target.tlsServer.URL + "/c")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, 0, h.primary.hitCount())
	require.Equal(t, 1, h.backup.hitCount())
	require.Equal(t, []string{h.backup.expectedAuth()}, h.backup.auths())
	recs := h.target.snapshot()
	require.NotEmpty(t, recs)
	assertNoProxyHeaders(t, recs[len(recs)-1])
}

// TestConnect407FailsOver: an upstream 407 during the CONNECT handshake is a
// pre-tunnel failure and is retried on the backup line.
func TestConnect407FailsOver(t *testing.T) {
	h := newFailoverHarness(t, mode407, modeOK,
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	resp, err := h.client().Get(h.target.tlsServer.URL + "/c")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Equal(t, 1, h.primary.hitCount())
	require.Equal(t, 1, h.backup.hitCount())
	require.Equal(t, []string{h.primary.expectedAuth()}, h.primary.auths())
	require.Equal(t, []string{h.backup.expectedAuth()}, h.backup.auths())
}

// TestConnect200ThenCloseHasNoFailover: once a tunnel replies 200 there is no
// failover, even if that upstream drops the connection right away.
func TestConnect200ThenCloseHasNoFailover(t *testing.T) {
	h := newFailoverHarness(t, modeCloseAfter100, modeOK,
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	_, err := h.client().Get(h.target.tlsServer.URL + "/c")
	require.Error(t, err, "TLS handshake through a closed tunnel must fail without failing over")

	require.Equal(t, 1, h.primary.hitCount())
	require.Equal(t, 0, h.backup.hitCount(), "no failover allowed after CONNECT 200")
}

// TestForward407AfterBodyIsTerminal: if an upstream reads the entire body and
// only then answers 407, the request has been consumed and must not be replayed
// on the backup. mode407BeforeBody honors Expect and challenges pre-body; here
// we simulate a proxy that ignores Expect and replies 407 only after the body.
func TestForward407AfterBodyIsTerminal(t *testing.T) {
	// modeCloseAfter100 sends 100 then closes mid-body; to test a full-body 407
	// we use a dedicated in-test fake upstream.
	primary := newLate407Upstream(t)
	defer primary.listener.Close()
	backup := newFakeUpstream(t, "backup", "buser", "bpass", modeOK)

	f := goproxy.NewUpstreamFailover([]goproxy.UpstreamLine{
		{Name: "primary", Addr: primary.addr, Username: "puser", Password: "ppass"},
		{Name: "backup", Addr: backup.addr(), Username: "buser", Password: "bpass"},
	},
		goproxy.WithAuthProbeTimeout(300*time.Millisecond))

	proxy := goproxy.NewProxyHttpServer()
	f.Attach(proxy)
	srv := httptest.NewServer(proxy)
	defer srv.Close()

	pu, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := client.Post("http://"+primary.originHost+"/p", "text/plain", strings.NewReader("full-body"))
	// The late-407 fake closes after responding; tolerate transport errors.
	if err == nil {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusBadGateway, resp.StatusCode, string(data))
		require.Contains(t, string(data), "primary")
	}
	require.Equal(t, 0, backup.hitCount(), "backup must not receive a body already consumed upstream")
}

// late407Upstream accepts the whole body and replies 407 only afterward.
type late407Upstream struct {
	listener   net.Listener
	addr       string
	originHost string
}

func newLate407Upstream(t *testing.T) *late407Upstream {
	t.Helper()
	target := newRecordingTarget(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	u := &late407Upstream{listener: l, addr: l.Addr().String(), originHost: strings.TrimPrefix(target.server.URL, "http://")}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				// Do not honor Expect: read the body fully first.
				if req.Body != nil {
					_, _ = io.Copy(io.Discard, req.Body)
				}
				io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}(c)
		}
	}()
	return u
}

// TestLineSelectionOrder is a package-level check via the exported selector
// behavior: preference header reorders, unknown names are ignored, defaults
// apply when absent.
func TestLineSelectionOrder(t *testing.T) {
	// Indirectly verified through header-driven selection, plus unknown-name
	// robustness: an unknown preferred name must not remove real lines.
	h := newFailoverHarness(t, modeOK, modeOK,
		goproxy.WithAuthProbeTimeout(500*time.Millisecond))

	// Unknown name first: real ordering is unaffected, primary still serves.
	req, _ := http.NewRequest(http.MethodGet, h.target.server.URL+"/p", nil)
	req.Header.Set(goproxy.UpstreamPreferenceHeader, "nonexistent")
	resp, err := h.client().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, h.primary.hitCount())
}
