package socks5

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/things-go/go-socks5/statute"
)

// bindClient dials the SOCKS server, performs the no-auth negotiation and
// sends a BIND request for dst, returning the control connection and the
// first reply (the server's listener address).
func bindClient(t *testing.T, proxyAddr string, dst *net.TCPAddr) (net.Conn, statute.Reply) {
	t.Helper()

	conn, err := net.Dial("tcp", proxyAddr) // nolint: noctx
	require.NoError(t, err)

	// method negotiation: no-auth
	_, err = conn.Write([]byte{statute.VersionSocks5, 1, statute.MethodNoAuth})
	require.NoError(t, err)
	methodResp := make([]byte, 2)
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	_, err = io.ReadFull(conn, methodResp)
	require.NoError(t, err)
	require.Equal(t, []byte{statute.VersionSocks5, statute.MethodNoAuth}, methodResp)

	// BIND request
	req := statute.Request{
		Version: statute.VersionSocks5,
		Command: statute.CommandBind,
		DstAddr: statute.AddrSpec{
			AddrType: statute.ATYPIPv4,
			IP:       dst.IP,
			Port:     dst.Port,
		},
	}
	_, err = conn.Write(req.Bytes())
	require.NoError(t, err)

	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	reply, err := statute.ParseReply(conn)
	require.NoError(t, err)
	return conn, reply
}

func socksBindAddr(reply statute.Reply) *net.TCPAddr {
	return &net.TCPAddr{IP: reply.BndAddr.IP, Port: reply.BndAddr.Port}
}

func mustResolveTCP(t *testing.T, addr string) *net.TCPAddr {
	t.Helper()
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	require.NoError(t, err)
	return tcpAddr
}

// reserveTCPPort closes a temporary loopback listener and returns its port
// so the caller can immediately reuse it as a fixed source port.
func reserveTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func TestSOCKS5_Bind(t *testing.T) {
	srv := NewServer(WithBindTimeout(5 * time.Second))
	listener, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	// Client sends BIND; any local peer may connect back (DST.PORT == 0).
	control, reply := bindClient(t, listener.Addr().String(),
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	defer control.Close() // nolint: errcheck
	require.Equal(t, statute.RepSuccess, reply.Response)
	relayAddr := socksBindAddr(reply)
	require.NotZero(t, relayAddr.Port)

	// The external service (third party) connects back to the relay.
	peer, err := net.Dial("tcp", relayAddr.String()) // nolint: noctx
	require.NoError(t, err)
	defer peer.Close() // nolint: errcheck

	// Second reply reports the connected peer's address.
	require.NoError(t, control.SetDeadline(time.Now().Add(time.Second)))
	reply2, err := statute.ParseReply(control)
	require.NoError(t, err)
	require.Equal(t, statute.RepSuccess, reply2.Response)
	peerAddr := peer.LocalAddr().(*net.TCPAddr)
	require.True(t, reply2.BndAddr.IP.Equal(peerAddr.IP))
	require.Equal(t, peerAddr.Port, reply2.BndAddr.Port)

	// Bidirectional data after both replies.
	require.NoError(t, control.SetDeadline(time.Now().Add(time.Second)))
	_, err = control.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(peer, buf)
	require.NoError(t, err)
	require.Equal(t, []byte("ping"), buf)

	require.NoError(t, peer.SetDeadline(time.Now().Add(time.Second)))
	_, err = peer.Write([]byte("pong"))
	require.NoError(t, err)
	_, err = io.ReadFull(control, buf)
	require.NoError(t, err)
	require.Equal(t, []byte("pong"), buf)

	require.NoError(t, control.SetDeadline(time.Time{}))
	require.NoError(t, peer.SetDeadline(time.Time{}))
}

func TestSOCKS5_Bind_UnexpectedSource(t *testing.T) {
	srv := NewServer(WithBindTimeout(5 * time.Second))
	listener, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	// Only an inbound connection sourced from expectedPort is accepted.
	expectedPort := reserveTCPPort(t)
	control, reply := bindClient(t, listener.Addr().String(),
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: expectedPort})
	defer control.Close() // nolint: errcheck
	require.Equal(t, statute.RepSuccess, reply.Response)
	relayAddr := socksBindAddr(reply)

	// A connection from an unexpected source port is rejected and closed;
	// no second reply is delivered.
	unexpected, err := net.Dial("tcp", relayAddr.String()) // nolint: noctx
	require.NoError(t, err)
	require.NoError(t, unexpected.SetReadDeadline(time.Now().Add(time.Second)))
	n, err := unexpected.Read(make([]byte, 1))
	require.Equal(t, 0, n)
	require.Error(t, err)
	require.NoError(t, unexpected.Close())

	require.NoError(t, control.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, err = control.Read(make([]byte, 1))
	require.Error(t, err)

	// The connection from the exact expected source is accepted and the
	// second (success) reply is sent.
	matched, err := net.DialTCP("tcp",
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: expectedPort},
		mustResolveTCP(t, relayAddr.String()))
	require.NoError(t, err)
	defer matched.Close() // nolint: errcheck
	require.NoError(t, control.SetDeadline(time.Now().Add(time.Second)))
	reply2, err := statute.ParseReply(control)
	require.NoError(t, err)
	require.Equal(t, statute.RepSuccess, reply2.Response)
	matchedLocal := matched.LocalAddr().(*net.TCPAddr)
	require.Equal(t, matchedLocal.Port, reply2.BndAddr.Port)
}

func TestSOCKS5_Bind_Timeout(t *testing.T) {
	srv := NewServer(WithBindTimeout(300 * time.Millisecond))
	listener, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	control, reply := bindClient(t, listener.Addr().String(),
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	defer control.Close() // nolint: errcheck
	require.Equal(t, statute.RepSuccess, reply.Response)

	require.NoError(t, control.SetDeadline(time.Now().Add(2*time.Second)))
	reply2, err := statute.ParseReply(control)
	require.NoError(t, err)
	require.Equal(t, statute.RepTTLExpired, reply2.Response)
}

func TestSOCKS5_Bind_ControlClosed(t *testing.T) {
	srv := NewServer(WithBindTimeout(5 * time.Second))
	listener, err := net.Listen("tcp", "127.0.0.1:0") // nolint: noctx
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	control, reply := bindClient(t, listener.Addr().String(),
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.Equal(t, statute.RepSuccess, reply.Response)
	relayAddr := socksBindAddr(reply)

	// Client abandons the BIND before any inbound connection arrives.
	require.NoError(t, control.Close())

	// The relay listener must be torn down; dialing it fails.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, dialErr := net.Dial("tcp", relayAddr.String()) // nolint: noctx
		if dialErr != nil {
			return
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("bind listener %s still accepting after control close", relayAddr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
