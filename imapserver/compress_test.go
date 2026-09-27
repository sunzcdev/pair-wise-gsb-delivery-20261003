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
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	compressTestUsername = "username"
	compressTestPassword = "password"
)

func newCompressTestListener(t *testing.T, options *imapserver.Options) net.Listener {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(compressTestUsername, compressTestPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	memServer.AddUser(user)

	if options == nil {
		options = &imapserver.Options{}
	}
	options.NewSession = func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return memServer.NewSession(), nil, nil
	}
	options.InsecureAuth = true

	server := imapserver.New(options)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = server.Serve(ln)
	}()
	t.Cleanup(func() {
		server.Close()
	})
	return ln
}

// compressTestClient is a raw IMAP test client speaking to a server over a
// TCP connection, with optional DEFLATE compression.
type compressTestClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader // cleartext reader

	zw  *flate.Writer // compressor for commands
	zbr *bufio.Reader // decompressor for responses
}

func newCompressTestClient(t *testing.T, ln net.Listener) *compressTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
	})
	c := &compressTestClient{t: t, conn: conn, br: bufio.NewReader(conn)}
	c.expectLine("* OK") // greeting
	return c
}

func (c *compressTestClient) readLineFrom(r *bufio.Reader) string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("failed to read line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func (c *compressTestClient) expectLine(prefix string) string {
	c.t.Helper()
	line := c.readLineFrom(c.br)
	if !strings.HasPrefix(line, prefix) {
		c.t.Fatalf("expected line with prefix %q, got %q", prefix, line)
	}
	return line
}

func (c *compressTestClient) expectCompressedLine(prefix string) string {
	c.t.Helper()
	line := c.readLineFrom(c.zbr)
	if !strings.HasPrefix(line, prefix) {
		c.t.Fatalf("expected compressed line with prefix %q, got %q", prefix, line)
	}
	return line
}

// readCompressedLineUntil reads compressed lines until one matches the
// given prefix, skipping untagged responses.
func (c *compressTestClient) readCompressedLineUntil(prefix string) string {
	c.t.Helper()
	for {
		line := c.readLineFrom(c.zbr)
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
}

func (c *compressTestClient) writeCleartext(s string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *compressTestClient) writeCompressed(s string) {
	c.t.Helper()
	if _, err := c.zw.Write([]byte(s)); err != nil {
		c.t.Fatal(err)
	}
	if err := c.zw.Flush(); err != nil {
		c.t.Fatal(err)
	}
}

// enableCompress negotiates compression with the given command tag and
// switches the client to the compressed stream.
func (c *compressTestClient) enableCompress(tag string) {
	c.t.Helper()
	c.writeCleartext(tag + " COMPRESS DEFLATE\r\n")
	c.expectLine(tag + " OK")
	zw, err := flate.NewWriter(c.conn, flate.DefaultCompression)
	if err != nil {
		c.t.Fatal(err)
	}
	c.zw = zw
	// Read compressed data via c.br so that any bytes it has buffered are
	// not lost.
	c.zbr = bufio.NewReader(flate.NewReader(c.br))
}

// expectClosed asserts that the server closes the connection.
func (c *compressTestClient) expectClosed() {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := c.conn.Read(make([]byte, 1))
	if err == nil {
		c.t.Fatalf("expected connection to be closed, read %v bytes", n)
	}
}

func TestCompressDeflate(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	c.enableCompress("a")

	c.writeCompressed("b NOOP\r\n")
	c.expectCompressedLine("b OK")

	// COMPRESS=DEFLATE must not be advertised once compression is active
	c.writeCompressed("c CAPABILITY\r\n")
	if line := c.expectCompressedLine("* CAPABILITY"); strings.Contains(line, "COMPRESS") {
		t.Fatalf("COMPRESS=DEFLATE still advertised after negotiation: %q", line)
	}
	c.expectCompressedLine("c OK")

	// Connection close: LOGOUT over the compressed stream, then the server
	// must close the connection
	c.writeCompressed("d LOGOUT\r\n")
	c.expectCompressedLine("* BYE")
	c.expectCompressedLine("d OK")
	c.expectClosed()
}

// TestCompressDeflatePipelined sends the COMPRESS command and the first
// compressed command in a single write. Bytes already buffered by the
// server's bufio.Reader must be decompressed rather than lost.
func TestCompressDeflatePipelined(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	var zbuf bytes.Buffer
	zw, err := flate.NewWriter(&zbuf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write([]byte("b NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatal(err)
	}

	payload := append([]byte("a COMPRESS DEFLATE\r\n"), zbuf.Bytes()...)
	if _, err := c.conn.Write(payload); err != nil {
		t.Fatal(err)
	}

	// The OK response to COMPRESS itself must be sent uncompressed
	c.expectLine("a OK")

	zw2, err := flate.NewWriter(c.conn, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	c.zw = zw2
	c.zbr = bufio.NewReader(flate.NewReader(c.br))
	c.expectCompressedLine("b OK")
}

func TestCompressDeflateUnsupportedMechanism(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	c.writeCleartext("a COMPRESS GZIP\r\n")
	c.expectLine("a NO")

	// The connection must remain usable and uncompressed
	c.writeCleartext("b NOOP\r\n")
	c.expectLine("b OK")
}

func TestCompressDeflateAlreadyActive(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	c.enableCompress("a")

	c.writeCompressed("b COMPRESS DEFLATE\r\n")
	c.expectCompressedLine("b BAD")
}

func TestCompressDeflateCorrupt(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	c.enableCompress("a")

	// Send data which is not a valid DEFLATE stream
	c.writeCleartext("this is not deflate data\r\n")
	c.expectClosed()
}

func TestCompressDeflateDecompressLimit(t *testing.T) {
	options := &imapserver.Options{
		CompressMaxDecompressedBytes: 32,
	}
	ln := newCompressTestListener(t, options)
	c := newCompressTestClient(t, ln)

	c.enableCompress("a")

	// This command decompresses to more than 32 bytes: the server must
	// terminate the connection
	c.writeCompressed("b " + strings.Repeat("X", 64) + "\r\n")
	c.expectClosed()
}

func TestCompressDeflateIdle(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	c.writeCleartext("a LOGIN " + compressTestUsername + " " + compressTestPassword + "\r\n")
	c.expectLine("a OK")

	c.enableCompress("b")

	c.writeCompressed("c SELECT INBOX\r\n")
	c.readCompressedLineUntil("c OK")

	c.writeCompressed("d IDLE\r\n")
	c.readCompressedLineUntil("+")

	// Append a message from a second connection: the update must be
	// flushed to the idling client immediately
	c2 := newCompressTestClient(t, ln)
	c2.writeCleartext("a LOGIN " + compressTestUsername + " " + compressTestPassword + "\r\n")
	c2.expectLine("a OK")
	c2.writeCleartext("b APPEND INBOX {5+}\r\nhello\r\n")
	c2.expectLine("b OK")

	c.expectCompressedLine("* 1 EXISTS")

	c.writeCompressed("DONE\r\n")
	c.readCompressedLineUntil("d OK")
}

func TestCompressDeflateStartTLSRejected(t *testing.T) {
	options := &imapserver.Options{
		TLSConfig: compressTestTLSConfig(t),
	}
	ln := newCompressTestListener(t, options)
	c := newCompressTestClient(t, ln)

	c.enableCompress("a")

	// RFC 4978 section 3: TLS must be negotiated before compression
	c.writeCompressed("b STARTTLS\r\n")
	c.expectCompressedLine("b BAD")
}

// TestCompressNotNegotiated ensures clients which don't negotiate the
// extension keep using the original cleartext read/write path.
func TestCompressNotNegotiated(t *testing.T) {
	ln := newCompressTestListener(t, nil)
	c := newCompressTestClient(t, ln)

	c.writeCleartext("a CAPABILITY\r\n")
	if line := c.expectLine("* CAPABILITY"); !strings.Contains(line, "COMPRESS=DEFLATE") {
		t.Fatalf("COMPRESS=DEFLATE not advertised: %q", line)
	}
	c.expectLine("a OK")

	c.writeCleartext("b LOGIN " + compressTestUsername + " " + compressTestPassword + "\r\n")
	c.expectLine("b OK")

	c.writeCleartext("c NOOP\r\n")
	c.expectLine("c OK")

	c.writeCleartext("d LOGOUT\r\n")
	c.expectLine("* BYE")
	c.expectLine("d OK")
	c.expectClosed()
}

func compressTestTLSConfig(t *testing.T) *tls.Config {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        cert,
		}},
	}
}
