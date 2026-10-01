package ftpserver

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const tlsSessionCacheKey = "ftps-test"

type tlsSessionCacheRecorder struct {
	tls.ClientSessionCache
	mu   sync.Mutex
	puts int
}

func (cache *tlsSessionCacheRecorder) Put(key string, state *tls.ClientSessionState) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if state != nil {
		cache.puts++
	}

	cache.ClientSessionCache.Put(key, state)
}

func (cache *tlsSessionCacheRecorder) Puts() int {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	return cache.puts
}

type explicitFTPSClient struct {
	t            *testing.T
	conn         *tls.Conn
	reader       *textproto.Reader
	writer       *textproto.Writer
	sessionCache *tlsSessionCacheRecorder
}

func newTLSBindingServer(t *testing.T, requireBinding bool) *FtpServer {
	t.Helper()

	return NewTestServerWithTestDriver(t, &TestServerDriver{
		TLS: true,
		Settings: &Settings{
			ListenAddr:                      "127.0.0.1:0",
			RequireTLSDataSessionResumption: requireBinding,
		},
	})
}

func newExplicitFTPSClient(t *testing.T, addr string, version uint16) (*explicitFTPSClient, *tlsSessionCacheRecorder) {
	t.Helper()

	tcpConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)

	client := &explicitFTPSClient{t: t}
	t.Cleanup(func() { _ = tcpConn.Close() })

	reader := textproto.NewReader(bufio.NewReader(tcpConn))
	code, message, err := reader.ReadResponse(0)
	require.NoError(t, err)
	require.Equal(t, 220, code, message)

	_, err = tcpConn.Write([]byte("AUTH TLS\r\n"))
	require.NoError(t, err)
	code, message, err = reader.ReadResponse(0)
	require.NoError(t, err)
	require.Equal(t, StatusAuthAccepted, code, message)

	cache := &tlsSessionCacheRecorder{ClientSessionCache: tls.NewLRUClientSessionCache(8)}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         version,
		MaxVersion:         version,
		ServerName:         "example.com",
		ClientSessionCache: fixedKeySessionCache{cache: cache},
	}
	tlsConn := tls.Client(tcpConn, tlsConfig)
	require.NoError(t, tlsConn.HandshakeContext(t.Context()))

	client.conn = tlsConn
	client.reader = textproto.NewReader(bufio.NewReader(tlsConn))
	client.writer = textproto.NewWriter(bufio.NewWriter(tlsConn))
	client.sessionCache = cache
	require.Equal(t, version, tlsConn.ConnectionState().Version)

	client.command("USER %s", authUser)
	client.command("PASS %s", authPass)
	client.command("PBSZ 0")
	client.command("PROT P")

	return client, cache
}

type fixedKeySessionCache struct {
	cache *tlsSessionCacheRecorder
}

func (cache fixedKeySessionCache) Get(string) (*tls.ClientSessionState, bool) {
	return cache.cache.Get(tlsSessionCacheKey)
}

func (cache fixedKeySessionCache) Put(_ string, state *tls.ClientSessionState) {
	cache.cache.Put(tlsSessionCacheKey, state)
}

func (client *explicitFTPSClient) command(format string, args ...any) (int, string) {
	require.NoError(client.t, client.writer.PrintfLine(format, args...))
	code, message, err := client.reader.ReadResponse(0)
	require.NoError(client.t, err)

	return code, message
}

func sendCommand(t *testing.T, client *explicitFTPSClient, format string, args ...any) {
	t.Helper()
	require.NoError(t, client.writer.PrintfLine(format, args...))
}

func stealSession(destination, source *explicitFTPSClient) {
	state, ok := source.sessionCache.Get(tlsSessionCacheKey)
	if ok {
		destination.sessionCache.Put(tlsSessionCacheKey, state)
	}
}

func TestTLSDataSessionBindingRejectsCrossSession(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tlsVersionName(version)+"/passive", func(t *testing.T) {
			server := newTLSBindingServer(t, true)
			first, _ := newExplicitFTPSClient(t, server.Addr(), version)
			firstPort := passivePort(t, first)
			sendCommand(t, first, "STOR first.bin")

			second, _ := newExplicitFTPSClient(t, server.Addr(), version)
			attacker, _ := newExplicitFTPSClient(t, server.Addr(), version)
			stealSession(attacker, second)
			require.Equal(t, first.conn.RemoteAddr().(*net.TCPAddr).IP,
				second.conn.RemoteAddr().(*net.TCPAddr).IP)
			require.Equal(t, first.conn.RemoteAddr().(*net.TCPAddr).IP,
				attacker.conn.RemoteAddr().(*net.TCPAddr).IP)
			wrongConn := dialTLSDataConn(t, "127.0.0.1", firstPort, attacker)
			err := wrongConn.HandshakeContext(t.Context())
			require.Error(t, err)
			require.True(t,
				errors.Is(err, io.EOF) ||
					strings.Contains(err.Error(), "handshake failure") ||
					strings.Contains(err.Error(), "remote error"),
				err.Error())
			require.NoError(t, wrongConn.Close())
			code, response, err := first.reader.ReadResponse(0)
			require.NoError(t, err)
			require.Equal(t, StatusCannotOpenDataConnection, code, response)

			secondPort := passivePort(t, second)
			secondConn := openPassiveTLSDataConnToPort(t, second, secondPort)
			require.True(t, secondConn.ConnectionState().DidResume)
			require.NoError(t, secondConn.Close())
			readFinalResponse(t, second, StatusClosingDataConn)
		})

		t.Run(tlsVersionName(version)+"/active", func(t *testing.T) {
			server := newTLSBindingServer(t, true)
			target, _ := newExplicitFTPSClient(t, server.Addr(), version)
			owner, _ := newExplicitFTPSClient(t, server.Addr(), version)
			attacker, _ := newExplicitFTPSClient(t, server.Addr(), version)
			stealSession(attacker, owner)

			targetListener, targetPort := activeDataListener(t)
			ownerListener, ownerPort := activeDataListener(t)

			code, _ := target.command("EPRT |1|127.0.0.1|%d|", targetPort)
			require.Equal(t, StatusOK, code)

			sendCommand(t, target, "LIST")

			attackerConn := acceptActiveTLSDataConn(t, targetListener, attacker)
			require.Error(t, attackerConn.HandshakeContext(t.Context()))
			require.NoError(t, attackerConn.Close())
			readFinalResponse(t, target, StatusCannotOpenDataConnection)

			code, _ = owner.command("EPRT |1|127.0.0.1|%d|", ownerPort)
			require.Equal(t, StatusOK, code)
			sendCommand(t, owner, "LIST")

			ownerConn := acceptActiveTLSDataConn(t, ownerListener, owner)
			require.NoError(t, ownerConn.HandshakeContext(t.Context()))
			require.True(t, ownerConn.ConnectionState().DidResume)
			readFinalResponse(t, owner, StatusFileStatusOK)
			require.NoError(t, ownerConn.Close())
			readFinalResponse(t, owner, StatusClosingDataConn)
		})
	}
}

func TestTLSDataSessionBindingAllowsRotationAndDisabledPolicy(t *testing.T) {
	server := newTLSBindingServer(t, true)
	client, cache := newExplicitFTPSClient(t, server.Addr(), tls.VersionTLS12)

	firstData := openPassiveTLSDataConn(t, client)
	require.True(t, firstData.ConnectionState().DidResume)
	require.NoError(t, firstData.Close())
	readFinalResponse(t, client, StatusClosingDataConn)

	secondData := openPassiveTLSDataConn(t, client)
	require.True(t, secondData.ConnectionState().DidResume)
	require.NoError(t, secondData.Close())
	readFinalResponse(t, client, StatusClosingDataConn)
	require.GreaterOrEqual(t, cache.Puts(), 2)

	disabledServer := newTLSBindingServer(t, false)
	disabledClient, _ := newExplicitFTPSClient(t, disabledServer.Addr(), tls.VersionTLS12)
	dataConn := openPassiveTLSDataConn(t, disabledClient)
	require.False(t, dataConn.ConnectionState().DidResume)
	require.NoError(t, dataConn.Close())
	readFinalResponse(t, disabledClient, StatusClosingDataConn)

	protectedClient, _ := newExplicitFTPSClient(t, server.Addr(), tls.VersionTLS12)
	code, response := protectedClient.command("PROT C")
	require.Equal(t, StatusOK, code, response)

	port := passivePort(t, protectedClient)
	sendCommand(t, protectedClient, "STOR protected.bin")
	tcpConn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tcpConn.Close() })
	_ = tcpConn.Close()
	readFinalResponse(t, protectedClient, StatusServiceNotAvailable)
}

func tlsVersionName(version uint16) string {
	if version == tls.VersionTLS13 {
		return "TLS1.3"
	}

	return "TLS1.2"
}

func passivePort(t *testing.T, client *explicitFTPSClient) int {
	t.Helper()

	code, response := client.command("EPSV")
	require.Equal(t, StatusEnteringEPSV, code, response)
	start := strings.Index(response, "|||")
	end := strings.LastIndex(response, "|")
	port, err := strconv.Atoi(response[start+3 : end])
	require.NoError(t, err)

	return port
}

func dialTLSDataConn(t *testing.T, host string, port int, client *explicitFTPSClient) *tls.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	require.NoError(t, err)

	state := client.conn.ConnectionState()
	return tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         state.Version,
		MaxVersion:         state.Version,
		ServerName:         "example.com",
		ClientSessionCache: fixedKeySessionCache{cache: client.sessionCache},
	})
}

func activeDataListener(t *testing.T) (net.Listener, int) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	return listener, listener.Addr().(*net.TCPAddr).Port
}

func acceptActiveTLSDataConn(t *testing.T, listener net.Listener, client *explicitFTPSClient) *tls.Conn {
	t.Helper()

	require.NoError(t, listener.(*net.TCPListener).SetDeadline(time.Now().Add(5*time.Second)))
	conn, err := listener.Accept()
	require.NoError(t, err)

	state := client.conn.ConnectionState()
	return tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         state.Version,
		MaxVersion:         state.Version,
		ServerName:         "example.com",
		ClientSessionCache: fixedKeySessionCache{cache: client.sessionCache},
	})
}

func openPassiveTLSDataConn(t *testing.T, client *explicitFTPSClient) *tls.Conn {
	t.Helper()

	port := passivePort(t, client)

	return openPassiveTLSDataConnToPort(t, client, port)
}

func openPassiveTLSDataConnToPort(t *testing.T, client *explicitFTPSClient, port int) *tls.Conn {
	t.Helper()

	sendCommand(t, client, "STOR file.bin")
	conn := dialTLSDataConn(t, "127.0.0.1", port, client)
	require.NoError(t, conn.HandshakeContext(t.Context()))
	readResponseCode(t, client, StatusFileStatusOK)

	return conn
}

func readResponseCode(t *testing.T, client *explicitFTPSClient, expectedCode int) {
	t.Helper()

	code, response, err := client.reader.ReadResponse(0)
	require.NoError(t, err)
	require.Equal(t, expectedCode, code, response)
}

func readFinalResponse(t *testing.T, client *explicitFTPSClient, expectedCode int) {
	t.Helper()

	code, response, err := client.reader.ReadResponse(0)
	require.NoError(t, err)
	require.Equal(t, expectedCode, code, response)
}
