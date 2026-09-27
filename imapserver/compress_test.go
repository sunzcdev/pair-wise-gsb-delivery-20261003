package imapserver_test

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	compressTestUsername = "user"
	compressTestPassword = "pass"
)

func testTLSConfig(t *testing.T) *tls.Config {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey() = %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() = %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
}

// compressTestClient is a raw IMAP test client speaking to an imapserver
// over TCP, with optional client-side DEFLATE compression.
type compressTestClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	zw   *flate.Writer
}

// compressTestEnv is an IMAP server with a memserver backend listening on
// a local TCP port.
type compressTestEnv struct {
	t  *testing.T
	ln net.Listener
}

func newCompressTestEnv(t *testing.T) *compressTestEnv {
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(compressTestUsername, compressTestPassword)
	user.Create("INBOX", nil)
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		TLSConfig:    testTLSConfig(t),
		InsecureAuth: true,
	})
	t.Cleanup(func() { server.Close() })

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		server.Serve(ln)
	}()

	return &compressTestEnv{t: t, ln: ln}
}

func (e *compressTestEnv) dial() *compressTestClient {
	conn, err := net.Dial("tcp", e.ln.Addr().String())
	if err != nil {
		e.t.Fatalf("net.Dial() = %v", err)
	}
	e.t.Cleanup(func() { conn.Close() })

	c := &compressTestClient{t: e.t, conn: conn}
	c.br = bufio.NewReader(conn)
	c.readLine() // greeting
	return c
}

func (c *compressTestClient) readLine() string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatalf("failed to read line: %v", err)
	}
	return strings.TrimSuffix(line, "\r\n")
}

func (c *compressTestClient) sendLine(line string) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if c.zw != nil {
		if _, err := c.zw.Write([]byte(line + "\r\n")); err != nil {
			c.t.Fatalf("failed to write compressed line: %v", err)
		}
		if err := c.zw.Flush(); err != nil {
			c.t.Fatalf("failed to flush compressor: %v", err)
		}
		return
	}
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatalf("failed to write line: %v", err)
	}
}

// enableCompression switches the client to the compressed connection, after
// the server has accepted COMPRESS.
func (c *compressTestClient) enableCompression() {
	c.t.Helper()
	var err error
	c.zw, err = flate.NewWriter(c.conn, flate.DefaultCompression)
	if err != nil {
		c.t.Fatalf("flate.NewWriter() = %v", err)
	}
	// c.br may hold buffered compressed data: inflate directly from it
	c.br = bufio.NewReader(flate.NewReader(c.br))
}

func (c *compressTestClient) expectTagged(prefix string) string {
	c.t.Helper()
	line := c.readLine()
	if !strings.HasPrefix(line, prefix+" ") {
		c.t.Fatalf("got line %q, want prefix %q", line, prefix)
	}
	return line
}

func (c *compressTestClient) login() {
	c.t.Helper()
	c.sendLine("l LOGIN " + compressTestUsername + " " + compressTestPassword)
	c.expectTagged("l OK")
}

func (c *compressTestClient) selectInbox() {
	c.t.Helper()
	c.sendLine("s SELECT INBOX")
	for {
		line := c.readLine()
		if strings.HasPrefix(line, "s ") {
			if !strings.HasPrefix(line, "s OK") {
				c.t.Fatalf("SELECT failed: %q", line)
			}
			return
		}
	}
}

func TestCompress(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	c.sendLine("a CAPABILITY")
	if line := c.readLine(); !strings.Contains(line, string(imap.CapCompressDeflate)) {
		t.Fatalf("CAPABILITY response %q doesn't contain COMPRESS=DEFLATE", line)
	}
	c.expectTagged("a OK")

	// Pipeline the COMPRESS command and an already-compressed command in a
	// single network write: bytes pre-read by the server's bufio.Reader must
	// not be lost.
	var zbuf bytes.Buffer
	zw, err := flate.NewWriter(&zbuf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate.NewWriter() = %v", err)
	}
	if _, err := zw.Write([]byte("c NOOP\r\n")); err != nil {
		t.Fatalf("zw.Write() = %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("zw.Flush() = %v", err)
	}
	if _, err := c.conn.Write(append([]byte("b COMPRESS DEFLATE\r\n"), zbuf.Bytes()...)); err != nil {
		t.Fatalf("conn.Write() = %v", err)
	}

	// The tagged OK for COMPRESS itself must be sent uncompressed
	c.expectTagged("b OK")

	c.enableCompression()

	// The pipelined compressed command must have been processed
	c.expectTagged("c OK")

	// The connection keeps working compressed
	c.sendLine("d NOOP")
	c.expectTagged("d OK")

	// COMPRESS=DEFLATE is no longer advertised once active
	c.sendLine("e CAPABILITY")
	if line := c.readLine(); strings.Contains(line, string(imap.CapCompressDeflate)) {
		t.Fatalf("CAPABILITY response %q still contains COMPRESS=DEFLATE", line)
	}
	c.expectTagged("e OK")

	c.login()
	c.selectInbox()

	c.sendLine("f LOGOUT")
	c.expectTagged("* BYE")
	c.expectTagged("f OK")
}

func TestCompressUnknownAlgorithm(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	c.sendLine("a COMPRESS GZIP")
	c.expectTagged("a NO")

	// The connection is still usable uncompressed
	c.sendLine("b NOOP")
	c.expectTagged("b OK")
}

func TestCompressTwice(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	c.sendLine("a COMPRESS DEFLATE")
	c.expectTagged("a OK")
	c.enableCompression()

	c.sendLine("b COMPRESS DEFLATE")
	c.expectTagged("b BAD")
}

func TestCompressStartTLS(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	c.sendLine("a STARTTLS")
	c.expectTagged("a OK")

	tlsConn := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("tls.Handshake() = %v", err)
	}
	c.conn = tlsConn
	c.br = bufio.NewReader(tlsConn)

	// COMPRESS is allowed after STARTTLS
	c.sendLine("b COMPRESS DEFLATE")
	c.expectTagged("b OK")
	c.enableCompression()

	c.sendLine("c NOOP")
	c.expectTagged("c OK")

	// STARTTLS is not allowed once compression is active
	c.sendLine("d STARTTLS")
	c.expectTagged("d BAD")
}

func TestCompressCorruptData(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	c.sendLine("a COMPRESS DEFLATE")
	c.expectTagged("a OK")
	c.enableCompression()

	// Send garbage instead of a valid DEFLATE stream: the server must close
	// the connection
	if _, err := c.conn.Write([]byte("this is not a DEFLATE stream\r\n")); err != nil {
		t.Fatalf("conn.Write() = %v", err)
	}
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.br.ReadByte(); err == nil {
		t.Fatal("expected connection to be closed after corrupt compressed data")
	}
}

func TestCompressIdle(t *testing.T) {
	env := newCompressTestEnv(t)
	c := env.dial()

	c.sendLine("a COMPRESS DEFLATE")
	c.expectTagged("a OK")
	c.enableCompression()

	c.login()
	c.selectInbox()

	c.sendLine("i IDLE")
	if line := c.readLine(); !strings.HasPrefix(line, "+") {
		t.Fatalf("got line %q, want continuation request", line)
	}

	// Trigger a unilateral EXISTS update from a second connection
	c2 := env.dial()
	c2.login()
	c2.sendLine("a APPEND INBOX {5+}")
	if _, err := c2.conn.Write([]byte("hello\r\n")); err != nil {
		t.Fatalf("conn.Write() = %v", err)
	}
	c2.expectTagged("a OK")

	// The update must be flushed through the compressor in a timely manner:
	// readLine fails the test if nothing arrives within the deadline
	if line := c.readLine(); !strings.Contains(line, "EXISTS") {
		t.Fatalf("got line %q, want EXISTS update", line)
	}

	c.sendLine("DONE")
	c.expectTagged("i OK")
}

func TestCompressLogout(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	c.sendLine("a COMPRESS DEFLATE")
	c.expectTagged("a OK")
	c.enableCompression()

	c.sendLine("b LOGOUT")
	c.expectTagged("* BYE")
	c.expectTagged("b OK")

	// The server closes the connection after LOGOUT
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.br.ReadByte(); err != io.EOF {
		t.Fatalf("expected EOF after LOGOUT, got %v", err)
	}
}

func TestCompressNotNegotiated(t *testing.T) {
	c := newCompressTestEnv(t).dial()

	// A client which doesn't negotiate the extension keeps using the
	// original cleartext read/write path
	c.login()
	c.selectInbox()
	c.sendLine("n NOOP")
	c.expectTagged("n OK")
	c.sendLine("x LOGOUT")
	c.expectTagged("* BYE")
	c.expectTagged("x OK")
}
