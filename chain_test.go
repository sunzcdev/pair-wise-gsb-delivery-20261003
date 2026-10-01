package socks5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"

	"github.com/things-go/go-socks5/statute"
)

// trackedConn signals when Close is called.
type trackedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

// trackedListener records every accepted connection.
type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*trackedConn
}

func newTrackedListener(t *testing.T) *trackedListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	return &trackedListener{Listener: l}
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, tc)
	l.mu.Unlock()
	return tc, nil
}

func (l *trackedListener) accepted() []*trackedConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*trackedConn(nil), l.conns...)
}

// recordingResolver records every name it is asked to resolve and answers
// from the given table.
type recordingResolver struct {
	mu    sync.Mutex
	names []string
	table map[string]net.IP
}

func (r *recordingResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
	if ip, ok := r.table[name]; ok {
		return ctx, ip, nil
	}
	return ctx, nil, fmt.Errorf("no such host: %s", name)
}

func (r *recordingResolver) resolved() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

// logRecorder is a thread-safe Logger capturing output for assertions.
type logRecorder struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logRecorder) Errorf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logRecorder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// dialRecorder records the addr of every outbound dial a server makes.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialRecorder) record(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, addr)
}

func (d *dialRecorder) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// serveEcho starts a ping/pong echo server and returns its address.
func serveEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() // nolint: errcheck
				buf := make([]byte, 4)
				if _, err := io.ReadFull(conn, buf); err != nil {
					return
				}
				if string(buf) == "ping" {
					conn.Write([]byte("pong")) // nolint: errcheck
				}
			}()
		}
	}()
	return l.Addr().String()
}

// serveHop starts a SOCKS5 server on a tracked listener.
func serveHop(t *testing.T, opts ...Option) (*trackedListener, *dialRecorder) {
	t.Helper()
	l := newTrackedListener(t)
	d := &dialRecorder{}
	opts = append(opts, WithDialAndRequest(
		func(ctx context.Context, network, addr string, _ *Request) (net.Conn, error) {
			d.record(addr)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}))
	srv := NewServer(opts...)
	go func() { srv.Serve(l) }() // nolint: errcheck
	return l, d
}

func waitClosed(t *testing.T, c *trackedConn, msg string) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for connection to close: %s", msg)
	}
}

// TestConnectChain_ThreeHops proves a CONNECT chain is established hop by
// hop through three local proxies, with the target name resolved only by
// the last hop.
func TestConnectChain_ThreeHops(t *testing.T) {
	echoAddr := serveEcho(t)
	_, echoPort, _ := net.SplitHostPort(echoAddr)

	// The last hop is the only node allowed to resolve the isolated name.
	hop3Resolver := &recordingResolver{table: map[string]net.IP{
		"echo.internal": net.ParseIP("127.0.0.1"),
	}}
	hop1Ln, hop1Dial := serveHop(t)
	hop2Ln, hop2Dial := serveHop(t, WithCredential(StaticCredentials{"hop2user": "hop2-secret"}))
	hop3Ln, hop3Dial := serveHop(t,
		WithCredential(StaticCredentials{"hop3user": "hop3-secret"}),
		WithResolver(hop3Resolver),
	)

	// The client-facing server must never resolve the isolated name locally.
	frontResolver := &recordingResolver{table: map[string]net.IP{}}
	frontLn := newTrackedListener(t)
	front := NewServer(
		WithResolver(frontResolver),
		WithConnectChain(Chain{
			Hops: []UpstreamHop{
				{Addr: hop1Ln.Addr().String()},
				{Addr: hop2Ln.Addr().String(), Username: "hop2user", Password: "hop2-secret"},
				{Addr: hop3Ln.Addr().String(), Username: "hop3user", Password: "hop3-secret"},
			},
			TargetResolve: ResolveRemote,
		}),
	)
	go func() { front.Serve(frontLn) }() // nolint: errcheck

	dialer, err := proxy.SOCKS5("tcp", frontLn.Addr().String(), nil, proxy.Direct)
	require.NoError(t, err)
	conn, err := dialer.Dial("tcp", net.JoinHostPort("echo.internal", echoPort))
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck

	conn.SetDeadline(time.Now().Add(3 * time.Second)) // nolint: errcheck
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(buf))

	// Every hop accepted exactly one connection.
	require.Len(t, hop1Ln.accepted(), 1)
	require.Len(t, hop2Ln.accepted(), 1)
	require.Len(t, hop3Ln.accepted(), 1)

	// Each hop dialed exactly the next hop; the last hop dialed the target.
	assert.Equal(t, []string{hop2Ln.Addr().String()}, hop1Dial.dialed())
	assert.Equal(t, []string{hop3Ln.Addr().String()}, hop2Dial.dialed())
	assert.Equal(t, []string{echoAddr}, hop3Dial.dialed())

	// Resolution happened only where configured: the front server never
	// resolved the isolated name, the last hop did.
	assert.NotContains(t, frontResolver.resolved(), "echo.internal")
	assert.Equal(t, []string{"echo.internal"}, hop3Resolver.resolved())
}

// TestConnectChain_LocalTargetResolve proves ResolveLocal resolves the
// destination on the client side and sends an IP to the last hop.
func TestConnectChain_LocalTargetResolve(t *testing.T) {
	echoAddr := serveEcho(t)
	_, echoPort, _ := net.SplitHostPort(echoAddr)

	hopLn, hopDial := serveHop(t)

	frontResolver := &recordingResolver{table: map[string]net.IP{
		"echo.local": net.ParseIP("127.0.0.1"),
	}}
	frontLn := newTrackedListener(t)
	front := NewServer(
		WithResolver(frontResolver),
		WithConnectChain(Chain{
			Hops:          []UpstreamHop{{Addr: hopLn.Addr().String()}},
			TargetResolve: ResolveLocal,
		}),
	)
	go func() { front.Serve(frontLn) }() // nolint: errcheck

	dialer, err := proxy.SOCKS5("tcp", frontLn.Addr().String(), nil, proxy.Direct)
	require.NoError(t, err)
	conn, err := dialer.Dial("tcp", net.JoinHostPort("echo.local", echoPort))
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck

	conn.SetDeadline(time.Now().Add(3 * time.Second)) // nolint: errcheck
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(buf))

	// The front server resolved the name locally and the hop received an IP.
	assert.Equal(t, []string{"echo.local"}, frontResolver.resolved())
	assert.Equal(t, []string{echoAddr}, hopDial.dialed())
}

// TestConnectChain_AuthFailureClosesConnections makes a middle hop reject
// authentication and checks the failure carries hop/phase, leaks no
// password, and closes every connection already opened.
func TestConnectChain_AuthFailureClosesConnections(t *testing.T) {
	_, echoPort, _ := net.SplitHostPort(serveEcho(t))

	hop1Ln, _ := serveHop(t)
	hop2Ln, _ := serveHop(t, WithCredential(StaticCredentials{"hop2user": "hop2-secret-pass"}))

	logs := &logRecorder{}
	chain := Chain{
		Hops: []UpstreamHop{
			{Addr: hop1Ln.Addr().String()},
			{Addr: hop2Ln.Addr().String(), Username: "hop2user", Password: "wrong-secret-pass"},
			{Addr: "127.0.0.1:1"}, // never reached
		},
		TargetResolve: ResolveRemote,
	}
	frontLn := newTrackedListener(t)
	front := NewServer(WithLogger(logs), WithConnectChain(chain))
	go func() { front.Serve(frontLn) }() // nolint: errcheck

	// Direct typed-error check: hop 1 (the second hop), auth phase.
	dest, err := statute.ParseAddrSpec(net.JoinHostPort("echo.internal", echoPort))
	require.NoError(t, err)
	_, err = chain.dial(context.Background(), nil, &dest)
	require.Error(t, err)
	var chainErr *ChainError
	require.True(t, errors.As(err, &chainErr))
	assert.Equal(t, 1, chainErr.Hop)
	assert.Equal(t, ChainPhaseAuth, chainErr.Phase)
	assert.NotContains(t, err.Error(), "wrong-secret-pass")
	assert.NotContains(t, err.Error(), "hop2-secret-pass")

	// End to end: the client request fails and the server logs hop/phase.
	dialer, err := proxy.SOCKS5("tcp", frontLn.Addr().String(), nil, proxy.Direct)
	require.NoError(t, err)
	conn, err := dialer.Dial("tcp", net.JoinHostPort("echo.internal", echoPort))
	if err == nil {
		conn.Close() // nolint: errcheck
		t.Fatal("expected dial through failing chain to fail")
	}

	require.Eventually(t, func() bool {
		out := logs.String()
		return strings.Contains(out, "hop 1") && strings.Contains(out, "auth")
	}, 3*time.Second, 10*time.Millisecond)
	assert.NotContains(t, logs.String(), "wrong-secret-pass")
	assert.NotContains(t, logs.String(), "hop2-secret-pass")

	// Every connection opened before the failure is closed: the front
	// server's tunnel to hop1, and hop1's tunnel to hop2 (two runs each:
	// one from the direct dial, one from the proxied request).
	hop1Conns := hop1Ln.accepted()
	hop2Conns := hop2Ln.accepted()
	require.Len(t, hop1Conns, 2)
	require.Len(t, hop2Conns, 2)
	for i, c := range hop1Conns {
		waitClosed(t, c, fmt.Sprintf("hop1 conn %d", i))
	}
	for i, c := range hop2Conns {
		waitClosed(t, c, fmt.Sprintf("hop2 conn %d", i))
	}
}
