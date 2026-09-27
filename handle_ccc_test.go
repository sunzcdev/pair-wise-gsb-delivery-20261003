package ftpserver

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func cccReadLine(t *testing.T, reader *bufio.Reader) string {
	t.Helper()

	line, err := reader.ReadString('\n')
	require.NoError(t, err)

	return strings.TrimRight(line, "\r\n")
}

func cccSendCmd(t *testing.T, conn net.Conn, reader *bufio.Reader, cmd string) string {
	t.Helper()

	_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
	require.NoError(t, err)

	return cccReadLine(t, reader)
}

// cccDialTLS connects, upgrades to explicit TLS and logs in
func cccDialTLS(t *testing.T, addr string) (*tls.Conn, *bufio.Reader) {
	t.Helper()

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	reader := bufio.NewReader(conn)
	require.True(t, strings.HasPrefix(cccReadLine(t, reader), "220"))

	_, err = fmt.Fprintf(conn, "AUTH TLS\r\n")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(cccReadLine(t, reader), "234"))

	tlsConn := tls.Client(conn, &tls.Config{
		//nolint:gosec
		InsecureSkipVerify: true,
	})
	require.NoError(t, tlsConn.Handshake())

	tlsReader := bufio.NewReader(tlsConn)
	require.True(t, strings.HasPrefix(cccSendCmd(t, tlsConn, tlsReader, "USER "+authUser), "331"))
	require.True(t, strings.HasPrefix(cccSendCmd(t, tlsConn, tlsReader, "PASS "+authPass), "230"))

	return tlsConn, tlsReader
}

func TestCCCDisabled(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false, TLS: true})

	tlsConn, reader := cccDialTLS(t, server.Addr())

	defer func() { _ = tlsConn.Close() }()

	resp := cccSendCmd(t, tlsConn, reader, "CCC")
	require.True(t, strings.HasPrefix(resp, "502"), resp)
}

func TestCCCRequiresLogin(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false, TLS: true})
	server.settings.EnableCCC = true

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	conn, err := dialer.DialContext(t.Context(), "tcp", server.Addr())
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	require.True(t, strings.HasPrefix(cccReadLine(t, reader), "220"))

	resp := cccSendCmd(t, conn, reader, "CCC")
	require.True(t, strings.HasPrefix(resp, "530"), resp)
}

func TestCCCRequiresTLS(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false})
	server.settings.EnableCCC = true

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	conn, err := dialer.DialContext(t.Context(), "tcp", server.Addr())
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	require.True(t, strings.HasPrefix(cccReadLine(t, reader), "220"))
	require.True(t, strings.HasPrefix(cccSendCmd(t, conn, reader, "USER "+authUser), "331"))
	require.True(t, strings.HasPrefix(cccSendCmd(t, conn, reader, "PASS "+authPass), "230"))

	resp := cccSendCmd(t, conn, reader, "CCC")
	require.True(t, strings.HasPrefix(resp, "503"), resp)
}

func TestCCC(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false, TLS: true})
	server.settings.EnableCCC = true

	tlsConn, reader := cccDialTLS(t, server.Addr())

	defer func() { _ = tlsConn.Close() }()

	resp := cccSendCmd(t, tlsConn, reader, "CCC")
	require.True(t, strings.HasPrefix(resp, "200"), resp)

	rawConn := tlsConn.NetConn()

	// send our close_notify and expect the server's one
	require.NoError(t, tlsConn.CloseWrite())

	_, err := tlsConn.Read(make([]byte, 1024))
	require.ErrorIs(t, err, io.EOF)

	// CloseWrite sets the write deadline of the underlying connection to
	// "now", we have to reset it before writing in clear text
	require.NoError(t, rawConn.SetDeadline(time.Now().Add(10*time.Second)))

	// the control channel is now in clear text
	plainReader := bufio.NewReader(rawConn)
	require.Equal(t, "200 OK", cccSendCmd(t, rawConn, plainReader, "NOOP"))
	require.Equal(t, "215 UNIX Type: L8", cccSendCmd(t, rawConn, plainReader, "SYST"))
}

func TestCCCPipelinedCommand(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false, TLS: true})
	server.settings.EnableCCC = true

	tlsConn, reader := cccDialTLS(t, server.Addr())

	defer func() { _ = tlsConn.Close() }()

	// pipeline NOOP right after CCC inside the TLS session: it must not be lost
	_, err := fmt.Fprintf(tlsConn, "CCC\r\nNOOP\r\n")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(cccReadLine(t, reader), "200"))

	rawConn := tlsConn.NetConn()

	require.NoError(t, tlsConn.CloseWrite())

	_, err = tlsConn.Read(make([]byte, 1024))
	require.ErrorIs(t, err, io.EOF)

	// CloseWrite sets the write deadline of the underlying connection to
	// "now", we have to reset it before writing in clear text
	require.NoError(t, rawConn.SetDeadline(time.Now().Add(10*time.Second)))

	// the pipelined NOOP answer is sent in clear text
	plainReader := bufio.NewReader(rawConn)
	require.Equal(t, "200 OK", cccReadLine(t, plainReader))
	require.Equal(t, "200 OK", cccSendCmd(t, rawConn, plainReader, "NOOP"))
}

func TestCCCClientNeverSendsCloseNotify(t *testing.T) {
	oldTimeout := cccShutdownTimeout
	cccShutdownTimeout = 200 * time.Millisecond

	defer func() { cccShutdownTimeout = oldTimeout }()

	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false, TLS: true})
	server.settings.EnableCCC = true

	tlsConn, reader := cccDialTLS(t, server.Addr())

	defer func() { _ = tlsConn.Close() }()

	resp := cccSendCmd(t, tlsConn, reader, "CCC")
	require.True(t, strings.HasPrefix(resp, "200"), resp)

	rawConn := tlsConn.NetConn()

	// the client reads the server's close_notify but never sends its own
	_, err := tlsConn.Read(make([]byte, 1024))
	require.ErrorIs(t, err, io.EOF)

	// wait for the server to give up waiting for our close_notify
	time.Sleep(500 * time.Millisecond)

	// the control channel is in clear text anyway
	plainReader := bufio.NewReader(rawConn)
	require.Equal(t, "200 OK", cccSendCmd(t, rawConn, plainReader, "NOOP"))
}
