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

	"github.com/stretchr/testify/require"
)

// staticResolver maps fixed names to IPs and fails for anything else.
type staticResolver map[string]net.IP

func (r staticResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	if ip, ok := r[name]; ok {
		return ctx, ip, nil
	}
	return ctx, nil, fmt.Errorf("no such host: %s", name)
}

// failResolver fails the test if any local resolution is attempted.
type failResolver struct{ t *testing.T }

func (r failResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	r.t.Errorf("unexpected local resolution of %q", name)
	return ctx, nil, fmt.Errorf("local resolution disabled: %s", name)
}

// dialRecorder records every outbound dial address of a hop server.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (r *dialRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, addr)
	r.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func (r *dialRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addrs...)
}

// trackedConn notifies closed when the connection is closed.
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

// trackedListener wraps accepted connections with trackedConn.
type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*trackedConn
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: conn, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, tc)
	l.mu.Unlock()
	return tc, nil
}

func (l *trackedListener) tracked() []*trackedConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*trackedConn(nil), l.conns...)
}

func startEchoServer(t *testing.T) net.Listener {
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
				defer conn.Close()  // nolint: errcheck
				io.Copy(conn, conn) // nolint: errcheck
			}()
		}
	}()
	return l
}

func startHopServer(t *testing.T, l net.Listener, opts ...Option) {
	t.Helper()
	srv := NewServer(opts...)
	go func() { srv.Serve(l) }()    // nolint: errcheck
	t.Cleanup(func() { l.Close() }) // nolint: errcheck
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	return l
}

// TestUpstreamChain_ThreeHops proves the chain is established hop by hop:
// hop1 dials hop2, hop2 dials hop3, and only hop3 dials the target. The
// isolated-network destination name is resolved by the exit hop, never
// locally.
func TestUpstreamChain_ThreeHops(t *testing.T) {
	target := startEchoServer(t)
	targetAddr := target.Addr().String()

	// Exit hop resolves the isolated-network name on its side.
	rec3 := &dialRecorder{}
	l3 := listenTCP(t)
	startHopServer(t, l3,
		WithDial(rec3.dial),
		WithResolver(staticResolver{"isolated.internal": net.ParseIP("127.0.0.1")}),
	)

	// Middle hop requires its own auth material.
	rec2 := &dialRecorder{}
	l2 := listenTCP(t)
	startHopServer(t, l2,
		WithDial(rec2.dial),
		WithCredential(StaticCredentials{"hop2user": "hop2pass"}),
	)

	rec1 := &dialRecorder{}
	l1 := listenTCP(t)
	startHopServer(t, l1, WithDial(rec1.dial))

	chain := &UpstreamChain{
		Hops: []UpstreamHop{
			{Addr: l1.Addr().String()},
			{Addr: l2.Addr().String(), Username: "hop2user", Password: "hop2pass"},
			{Addr: l3.Addr().String()},
		},
		// Remote resolution (default): the isolated name must never be
		// resolved on this host.
		Resolver: failResolver{t},
	}

	conn, err := chain.DialContext(context.Background(), "tcp",
		net.JoinHostPort("isolated.internal", fmt.Sprintf("%d", target.Addr().(*net.TCPAddr).Port)))
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))

	// Each hop dialed exactly the next hop; only the exit hop dialed the target.
	require.Equal(t, []string{l2.Addr().String()}, rec1.recorded())
	require.Equal(t, []string{l3.Addr().String()}, rec2.recorded())
	require.Equal(t, []string{targetAddr}, rec3.recorded())
}

// TestUpstreamChain_MiddleHopRejectsAuth makes the middle hop reject the
// credentials, then checks the failure carries hop index and phase, leaks
// no password, and every connection already opened is closed.
func TestUpstreamChain_MiddleHopRejectsAuth(t *testing.T) {
	target := startEchoServer(t)

	l3 := listenTCP(t)
	tl3 := &trackedListener{Listener: l3}
	startHopServer(t, tl3)

	l2 := listenTCP(t)
	tl2 := &trackedListener{Listener: l2}
	startHopServer(t, tl2, WithCredential(StaticCredentials{"hop2user": "hop2pass"}))

	l1 := listenTCP(t)
	tl1 := &trackedListener{Listener: l1}
	startHopServer(t, tl1)

	chain := &UpstreamChain{
		Hops: []UpstreamHop{
			{Addr: l1.Addr().String()},
			{Addr: l2.Addr().String(), Username: "hop2user", Password: "wr0ng-pw"},
			{Addr: l3.Addr().String()},
		},
	}

	conn, err := chain.DialContext(context.Background(), "tcp", target.Addr().String())
	require.Error(t, err)
	require.Nil(t, conn)

	// The failure carries the hop index and the phase.
	var chainErr *ChainError
	require.True(t, errors.As(err, &chainErr))
	require.Equal(t, 1, chainErr.Hop)
	require.Equal(t, PhaseAuth, chainErr.Phase)

	// The error must not contain any password.
	require.NotContains(t, err.Error(), "wr0ng-pw")
	require.NotContains(t, err.Error(), "hop2pass")

	// The chain never reached hop3.
	require.Empty(t, tl3.tracked())

	// The connection already opened to hop1 is closed.
	var tracked1 []*trackedConn
	require.Eventually(t, func() bool {
		tracked1 = tl1.tracked()
		return len(tracked1) == 1
	}, 2*time.Second, 10*time.Millisecond)
	select {
	case <-tracked1[0].closed:
	case <-time.After(2 * time.Second):
		t.Fatal("connection to hop1 was not closed after the chain failed")
	}

	// Hop2 saw the rejected connection and closed it as well.
	require.Eventually(t, func() bool {
		tracked2 := tl2.tracked()
		if len(tracked2) != 1 {
			return false
		}
		select {
		case <-tracked2[0].closed:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
}

// TestUpstreamChain_LocalDestResolution checks the explicit local
// resolution mode: the destination name is resolved on this host and the
// CONNECT request carries the resolved IP.
func TestUpstreamChain_LocalDestResolution(t *testing.T) {
	target := startEchoServer(t)

	rec := &dialRecorder{}
	l1 := listenTCP(t)
	startHopServer(t, l1, WithDial(rec.dial))

	chain := &UpstreamChain{
		Hops:               []UpstreamHop{{Addr: l1.Addr().String()}},
		ResolveDestLocally: true,
		Resolver:           staticResolver{"isolated.internal": net.ParseIP("127.0.0.1")},
	}

	conn, err := chain.DialContext(context.Background(), "tcp",
		net.JoinHostPort("isolated.internal", fmt.Sprintf("%d", target.Addr().(*net.TCPAddr).Port)))
	require.NoError(t, err)
	defer conn.Close() // nolint: errcheck

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))

	// The hop received the locally resolved IP, not the FQDN.
	require.Equal(t, []string{target.Addr().String()}, rec.recorded())
}

// TestChainError_ScrubsPassword checks the password never leaks through
// the error message, even if the underlying error contains it.
func TestChainError_ScrubsPassword(t *testing.T) {
	hop := &UpstreamHop{Addr: "127.0.0.1:1080", Username: "u", Password: "s3cret"}
	err := hop.chainError(0, PhaseAuth, errors.New("auth failed for s3cret"))
	require.Equal(t, 0, err.Hop)
	require.Equal(t, PhaseAuth, err.Phase)
	require.NotContains(t, err.Error(), "s3cret")
	require.True(t, strings.Contains(err.Error(), "***"))
}
