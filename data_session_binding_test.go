package ftpserver

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recordingSessionCache wraps a TLS ClientSessionCache and records every raw
// session ticket the client receives. Tests use it to prove that a resumed
// connection still receives a brand-new ticket (ticket rotation).
type recordingSessionCache struct {
	inner   tls.ClientSessionCache
	mu      sync.Mutex
	tickets [][]byte
}

func newRecordingSessionCache() *recordingSessionCache {
	return &recordingSessionCache{inner: tls.NewLRUClientSessionCache(16)}
}

func (c *recordingSessionCache) Get(sessionKey string) (*tls.ClientSessionState, bool) {
	return c.inner.Get(sessionKey)
}

func (c *recordingSessionCache) Put(sessionKey string, cs *tls.ClientSessionState) {
	if cs != nil {
		if ticket, _, err := cs.ResumptionState(); err == nil && len(ticket) > 0 {
			c.mu.Lock()
			c.tickets = append(c.tickets, append([]byte(nil), ticket...))
			c.mu.Unlock()
		}
	}

	c.inner.Put(sessionKey, cs)
}

func (c *recordingSessionCache) uniqueTickets() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := make(map[string]struct{}, len(c.tickets))
	for _, ticket := range c.tickets {
		seen[string(ticket)] = struct{}{}
	}

	return len(seen)
}

// explicitFTPSClient is a minimal explicit FTPS (AUTH TLS) client that lets
// tests choose the session cache used for every control and data connection.
// Sharing/swapping caches between clients is how the same-IP data connection
// swap attack is reproduced.
type explicitFTPSClient struct {
	t       *testing.T
	conn    net.Conn
	reader  *bufio.Reader
	cache   tls.ClientSessionCache
	tlsConf *tls.Config
}

func newExplicitFTPSClient(t *testing.T, addr string, cache tls.ClientSessionCache, maxVersion uint16) *explicitFTPSClient {
	t.Helper()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	rawConn, err := dialer.Dial("tcp", addr)
	require.NoError(t, err)

	tlsConf := &tls.Config{
		//nolint:gosec // test server uses a self-signed certificate
		InsecureSkipVerify: true,
		// A fixed ServerName makes every control and data connection to this
		// server share one cache key, so tickets can be reused (and swapped)
		// across connections to different data ports.
		ServerName:         "127.0.0.1",
		ClientSessionCache: cache,
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         maxVersion,
	}

	client := &explicitFTPSClient{
		t:       t,
		conn:    rawConn,
		reader:  bufio.NewReader(rawConn),
		cache:   cache,
		tlsConf: tlsConf,
	}

	client.readReply(StatusServiceReady)
	client.cmd(StatusAuthAccepted, "AUTH TLS")

	tlsConn := tls.Client(rawConn, tlsConf)
	require.NoError(t, tlsConn.Handshake())

	client.conn = tlsConn
	client.reader = bufio.NewReader(tlsConn)

	client.cmd(StatusOK, "PBSZ 0")
	client.cmd(StatusOK, "PROT P")
	client.cmd(StatusUserOK, "USER "+authUser)
	client.cmd(StatusUserLoggedIn, "PASS "+authPass)

	return client
}

func (c *explicitFTPSClient) readReply(expected int) (int, string) {
	line, err := c.reader.ReadString('\n')
	require.NoError(c.t, err)

	code, err := strconv.Atoi(strings.TrimSpace(line)[:3])
	require.NoError(c.t, err, "bad reply: %q", line)

	if expected != 0 {
		require.Equal(c.t, expected, code, "unexpected reply: %q", line)
	}

	return code, strings.TrimSpace(line[4:])
}

func (c *explicitFTPSClient) cmd(expected int, format string, args ...any) (int, string) {
	_, err := fmt.Fprintf(c.conn, format+"\r\n", args...)
	require.NoError(c.t, err)

	return c.readReply(expected)
}

func (c *explicitFTPSClient) close() {
	_ = c.conn.Close()
}

// epsv asks for a passive data connection and returns the advertised port.
func (c *explicitFTPSClient) epsv() int {
	_, reply := c.cmd(StatusEnteringEPSV, "EPSV")

	start := strings.LastIndex(reply, "|||")
	require.NotEqual(c.t, -1, start, "bad EPSV reply: %q", reply)

	end := strings.Index(reply[start:], "|)")
	require.NotEqual(c.t, -1, end, "bad EPSV reply: %q", reply)

	port, err := strconv.Atoi(reply[start+3 : start+end])
	require.NoError(c.t, err, "bad EPSV reply: %q", reply)

	return port
}

type dataDialResult struct {
	conn  net.Conn
	state tls.ConnectionState
	err   error
}

// dialData opens a TLS connection to a passive data port using the given TLS
// configuration.
func dialData(port int, conf *tls.Config) (net.Conn, tls.ConnectionState, error) {
	tcpConn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		return nil, tls.ConnectionState{}, err
	}

	tlsConn := tls.Client(tcpConn, conf)
	if err := tlsConn.Handshake(); err != nil {
		_ = tcpConn.Close()

		return nil, tls.ConnectionState{}, err
	}

	return tlsConn, tlsConn.ConnectionState(), nil
}

// startDataDial dials and handshakes a passive data connection in the
// background. The server only accepts the data connection while handling the
// transfer command, so both must be driven concurrently.
func startDataDial(port int, conf *tls.Config) <-chan dataDialResult {
	resultCh := make(chan dataDialResult, 1)

	go func() {
		conn, state, err := dialData(port, conf)
		resultCh <- dataDialResult{conn: conn, state: state, err: err}
	}()

	return resultCh
}

// listOverPassive performs one successful LIST over a passive data connection
// that resumes the control session.
func (c *explicitFTPSClient) listOverPassive(t *testing.T) tls.ConnectionState {
	t.Helper()

	port := c.epsv()
	resultCh := startDataDial(port, c.tlsConf)

	c.cmd(StatusFileStatusOK, "LIST")

	result := <-resultCh
	require.NoError(t, result.err)

	_, _ = io.Copy(io.Discard, result.conn)
	require.NoError(t, result.conn.Close())

	c.readReply(StatusClosingDataConn)

	return result.state
}

func newSessionBindingTestServer(t *testing.T, requirement TLSDataSessionRequirement) *FtpServer {
	t.Helper()

	return NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
		Settings: &Settings{
			TLSDataSessionRequirement: requirement,
			// Bind from a non-privileged source port so active-mode tests can
			// run without special privileges.
			ActiveTransferPortNon20: true,
		},
	})
}

func TestTLSDataSessionRequirementNormalResume(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxVersion uint16
	}{
		{"TLS1.2", tls.VersionTLS12},
		{"TLS1.3", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSessionBindingTestServer(t, TLSDataSessionRequired)
			cache := newRecordingSessionCache()
			client := newExplicitFTPSClient(t, server.Addr(), cache, tc.maxVersion)
			defer client.close()

			// Two consecutive data connections: both must resume the control
			// session and both must receive a freshly minted ticket.
			state := client.listOverPassive(t)
			require.True(t, state.DidResume, "first data connection must resume the control session")

			state = client.listOverPassive(t)
			require.True(t, state.DidResume, "second data connection must resume the rotated ticket session")

			require.GreaterOrEqual(t, cache.uniqueTickets(), 3,
				"the control ticket plus one new ticket per resumed connection should be distinct")
		})
	}
}

func TestTLSDataSessionRequirementSwappedDataConnectionRejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxVersion uint16
	}{
		{"TLS1.2", tls.VersionTLS12},
		{"TLS1.3", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSessionBindingTestServer(t, TLSDataSessionRequired)

			// Two distinct control sessions coming from the same local
			// address (same host / same NAT scenario). Each has its own
			// ticket cache.
			cacheA := newRecordingSessionCache()
			cacheB := newRecordingSessionCache()
			clientA := newExplicitFTPSClient(t, server.Addr(), cacheA, tc.maxVersion)
			defer clientA.close()
			clientB := newExplicitFTPSClient(t, server.Addr(), cacheB, tc.maxVersion)
			defer clientB.close()

			// Client A opens a passive listener on its control connection.
			portA := clientA.epsv()

			// Client B tries to hijack A's data connection using B's own
			// resumed session, from the same source IP.
			resultCh := startDataDial(portA, clientB.tlsConf)

			// The server-side transfer open fails and the LIST is refused.
			clientA.cmd(StatusCannotOpenDataConnection, "LIST")

			result := <-resultCh
			require.Error(t, result.err, "data connection resuming another control session must be rejected")

			// Sanity check: A itself can still use a fresh passive connection
			// normally.
			state := clientA.listOverPassive(t)
			require.True(t, state.DidResume, "the legitimate client must still be able to resume")
		})
	}
}

func TestTLSDataSessionRequirementFullHandshakeRejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxVersion uint16
	}{
		{"TLS1.2", tls.VersionTLS12},
		{"TLS1.3", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSessionBindingTestServer(t, TLSDataSessionRequired)
			client := newExplicitFTPSClient(t, server.Addr(), newRecordingSessionCache(), tc.maxVersion)
			defer client.close()

			port := client.epsv()

			// A client connecting with an empty session cache performs a full
			// handshake on the data connection, which the policy forbids.
			emptyCacheConf := client.tlsConf.Clone()
			emptyCacheConf.ClientSessionCache = tls.NewLRUClientSessionCache(4)

			resultCh := startDataDial(port, emptyCacheConf)
			client.cmd(StatusCannotOpenDataConnection, "LIST")

			result := <-resultCh
			if result.err == nil {
				// TLS 1.3 only invokes UnwrapSession when a PSK identity is
				// offered, so a ticketless connection completes a full
				// handshake that the server then rejects via DidResume.
				require.False(t, result.state.DidResume, "this must be a full handshake")
				_ = result.conn.Close()
			}
		})
	}
}

func TestTLSDataSessionRequirementDisabledCompatibility(t *testing.T) {
	server := newSessionBindingTestServer(t, TLSDataSessionDisabled)

	cacheA := newRecordingSessionCache()
	cacheB := newRecordingSessionCache()
	clientA := newExplicitFTPSClient(t, server.Addr(), cacheA, 0)
	defer clientA.close()
	clientB := newExplicitFTPSClient(t, server.Addr(), cacheB, 0)
	defer clientB.close()

	// With the policy off, swapping data connections between two same-IP
	// sessions is accepted (only the historical IP check applies).
	portA := clientA.epsv()
	resultCh := startDataDial(portA, clientB.tlsConf)

	clientA.cmd(StatusFileStatusOK, "LIST")

	result := <-resultCh
	require.NoError(t, result.err)
	require.True(t, result.state.DidResume, "B resumes its own session")
	_, _ = io.Copy(io.Discard, result.conn)
	require.NoError(t, result.conn.Close())

	clientA.readReply(StatusClosingDataConn)

	// A full handshake (no ticket) on the data channel is accepted too.
	port := clientA.epsv()

	fullConf := clientA.tlsConf.Clone()
	fullConf.ClientSessionCache = tls.NewLRUClientSessionCache(4)

	resultCh = startDataDial(port, fullConf)
	clientA.cmd(StatusFileStatusOK, "LIST")

	result = <-resultCh
	require.NoError(t, result.err)
	require.False(t, result.state.DidResume, "this must be a full handshake")
	_, _ = io.Copy(io.Discard, result.conn)
	require.NoError(t, result.conn.Close())

	clientA.readReply(StatusClosingDataConn)
}

// activeListener is the client side of an active-mode (EPRT) data
// connection. The FTP server dials in but remains the TLS server, so the
// endpoint accepts TCP and answers with a TLS ClientHello using the chosen
// client's session cache.
type activeListener struct {
	t        *testing.T
	listener net.Listener
	conf     *tls.Config
}

func newActiveListener(t *testing.T, conf *tls.Config) *activeListener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	return &activeListener{t: t, listener: listener, conf: conf}
}

func (a *activeListener) eprt(client *explicitFTPSClient) {
	addr := a.listener.Addr().(*net.TCPAddr)
	client.cmd(StatusOK, "EPRT |1|127.0.0.1|%d|", addr.Port)
}

// accept runs the TLS client side of the connection the server dials in.
func (a *activeListener) accept() (net.Conn, tls.ConnectionState, error) {
	tcpConn, err := a.listener.Accept()
	if err != nil {
		return nil, tls.ConnectionState{}, err
	}

	tlsConn := tls.Client(tcpConn, a.conf)
	if err := tlsConn.Handshake(); err != nil {
		_ = tcpConn.Close()

		return nil, tls.ConnectionState{}, err
	}

	return tlsConn, tlsConn.ConnectionState(), nil
}

func TestTLSDataSessionRequirementActiveMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxVersion uint16
	}{
		{"TLS1.2", tls.VersionTLS12},
		{"TLS1.3", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSessionBindingTestServer(t, TLSDataSessionRequired)

			cacheA := newRecordingSessionCache()
			cacheB := newRecordingSessionCache()
			clientA := newExplicitFTPSClient(t, server.Addr(), cacheA, tc.maxVersion)
			defer clientA.close()
			clientB := newExplicitFTPSClient(t, server.Addr(), cacheB, tc.maxVersion)
			defer clientB.close()

			// Active-mode swap: A advertises an address the server will dial
			// back, but B (same host, same source IP semantics) answers the
			// incoming TLS connection presenting B's own resumed session.
			bEndpoint := newActiveListener(t, clientB.tlsConf)
			bEndpoint.eprt(clientA)

			acceptCh := make(chan dataDialResult, 1)

			go func() {
				conn, state, err := bEndpoint.accept()
				acceptCh <- dataDialResult{conn: conn, state: state, err: err}
			}()

			// A triggers the active connection; the server dials B's endpoint
			// and the session binding check fails.
			clientA.cmd(StatusCannotOpenDataConnection, "LIST")

			result := <-acceptCh
			require.Error(t, result.err, "active data connection resuming another session must be rejected")

			// Legitimate active transfer: A advertises its own endpoint and
			// answers with a TLS server that the FTP server (the dialing
			// party) resumes A's control session into.
			aEndpoint := newActiveListener(t, clientA.tlsConf)
			aEndpoint.eprt(clientA)

			// In active mode the server is the TLS client: it connects out and
			// must present A's ticket. We verify the legitimate path succeeds.
			listCh := make(chan error, 1)

			go func() {
				conn, _, acceptErr := aEndpoint.accept()
				if acceptErr != nil {
					listCh <- acceptErr

					return
				}
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
				listCh <- nil
			}()

			clientA.cmd(StatusFileStatusOK, "LIST")
			require.NoError(t, <-listCh)
			clientA.readReply(StatusClosingDataConn)
		})
	}
}
