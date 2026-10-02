package smtp_test

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// limiterBackend is a backend authenticating any username with the
// password "password". If authEntered is non-nil, each authentication
// attempt signals the channel and blocks until authRelease is closed,
// allowing tests to make concurrent AUTH commands complete at the same
// time.
type limiterBackend struct {
	authEntered chan struct{}
	authRelease chan struct{}
}

func (be *limiterBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &limiterSession{be: be}, nil
}

type limiterSession struct {
	be   *limiterBackend
	user string
}

func (s *limiterSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *limiterSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if s.be.authEntered != nil {
			s.be.authEntered <- struct{}{}
			<-s.be.authRelease
		}
		if password != "password" {
			return smtp.ErrAuthFailed
		}
		s.user = username
		return nil
	}), nil
}

func (s *limiterSession) AuthIdentity() string { return s.user }

func (s *limiterSession) Reset()        {}
func (s *limiterSession) Logout() error { return nil }

func (s *limiterSession) Mail(from string, opts *smtp.MailOptions) error { return nil }
func (s *limiterSession) Rcpt(to string, opts *smtp.RcptOptions) error   { return nil }
func (s *limiterSession) Data(r io.Reader) error                         { return nil }

type limiterTestClient struct {
	t  *testing.T
	c  net.Conn
	sc *bufio.Scanner
}

func dialLimiterServer(t *testing.T, addr string) *limiterTestClient {
	t.Helper()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal("Dial:", err)
	}
	return &limiterTestClient{t: t, c: c, sc: bufio.NewScanner(c)}
}

func (cl *limiterTestClient) readLine() string {
	cl.t.Helper()

	if !cl.sc.Scan() {
		cl.t.Fatal("Connection closed while waiting for a response:", cl.sc.Err())
	}
	return cl.sc.Text()
}

func (cl *limiterTestClient) cmd(cmd string) string {
	cl.t.Helper()

	if _, err := io.WriteString(cl.c, cmd+"\r\n"); err != nil {
		cl.t.Fatal("Write:", err)
	}
	return cl.readLine()
}

func (cl *limiterTestClient) ehlo() {
	cl.t.Helper()

	line := cl.cmd("EHLO localhost")
	for {
		if strings.HasPrefix(line, "250 ") {
			return
		}
		if !strings.HasPrefix(line, "250-") {
			cl.t.Fatal("Invalid EHLO response:", line)
		}
		line = cl.readLine()
	}
}

func (cl *limiterTestClient) auth(user, pass string) string {
	cl.t.Helper()

	ir := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
	return cl.cmd("AUTH PLAIN " + ir)
}

func (cl *limiterTestClient) startTLS() {
	cl.t.Helper()

	if line := cl.cmd("STARTTLS"); !strings.HasPrefix(line, "220 ") {
		cl.t.Fatal("Invalid STARTTLS response:", line)
	}

	cfg := &tls.Config{InsecureSkipVerify: true}
	tc := tls.Client(cl.c, cfg)
	if err := tc.Handshake(); err != nil {
		cl.t.Fatal("TLS handshake:", err)
	}
	cl.c = tc
	cl.sc = bufio.NewScanner(tc)
}

func (cl *limiterTestClient) close() {
	cl.c.Close()
}

func testLimiterServer(t *testing.T, be smtp.Backend, lim *smtp.MemoryLimiter, fn ...serverConfigureFunc) (*smtp.Server, string) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := smtp.NewServer(be)
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	s.ConnLimiter = lim
	for _, f := range fn {
		f(s)
	}

	go s.Serve(l)
	return s, l.Addr().String()
}

func waitForCondition(t *testing.T, msg string, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("Timeout waiting for:", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServerConnLimiterReject(t *testing.T) {
	be := new(limiterBackend)
	s, addr := testLimiterServer(t, be, &smtp.MemoryLimiter{MaxPerIP: 1})
	defer s.Close()

	c1 := dialLimiterServer(t, addr)
	defer c1.close()
	if line := c1.readLine(); !strings.HasPrefix(line, "220 ") {
		t.Fatal("Invalid greeting:", line)
	}
	c1.ehlo()

	// A second unauthenticated connection from the same source IP is
	// rejected with a 421 response instead of the greeting.
	c2 := dialLimiterServer(t, addr)
	if line := c2.readLine(); !strings.HasPrefix(line, "421 ") {
		t.Fatal("Expected a 421 rejection, got:", line)
	}
	c2.close()

	// Once the first connection is gone, the source slot is released.
	c1.cmd("QUIT")
	c1.close()

	var c3 *limiterTestClient
	waitForCondition(t, "source slot to be released", func() bool {
		c3 = dialLimiterServer(t, addr)
		if strings.HasPrefix(c3.readLine(), "220 ") {
			return true
		}
		c3.close()
		return false
	})
	c3.close()
}

func TestServerConnLimiterAuthRace(t *testing.T) {
	be := &limiterBackend{
		authEntered: make(chan struct{}, 8),
		authRelease: make(chan struct{}),
	}
	s, addr := testLimiterServer(t, be, &smtp.MemoryLimiter{MaxPerIP: 8, MaxPerUser: 1})
	defer s.Close()

	c1 := dialLimiterServer(t, addr)
	defer c1.close()
	c1.readLine()
	c1.ehlo()
	c2 := dialLimiterServer(t, addr)
	defer c2.close()
	c2.readLine()
	c2.ehlo()

	// Both connections authenticate as the same user and complete the
	// credentials check at the same time.
	ir := base64.StdEncoding.EncodeToString([]byte("\x00user1\x00password"))
	io.WriteString(c1.c, "AUTH PLAIN "+ir+"\r\n")
	io.WriteString(c2.c, "AUTH PLAIN "+ir+"\r\n")
	<-be.authEntered
	<-be.authEntered
	close(be.authRelease)

	// Exactly one of them may enter the full user group.
	r1, r2 := c1.readLine(), c2.readLine()
	var winner, loser *limiterTestClient
	switch {
	case strings.HasPrefix(r1, "235 ") && strings.HasPrefix(r2, "454 "):
		winner, loser = c1, c2
	case strings.HasPrefix(r2, "235 ") && strings.HasPrefix(r1, "454 "):
		winner, loser = c2, c1
	default:
		t.Fatalf("Expected one 235 and one 454 response, got %q and %q", r1, r2)
	}

	// The loser stayed in the source group and the user group is still
	// full, so a retry fails again instead of overselling the quota.
	if line := loser.auth("user1", "password"); !strings.HasPrefix(line, "454 ") {
		t.Fatal("Expected a 454 response, got:", line)
	}

	// Once the winner disconnects, the user slot is released.
	winner.cmd("QUIT")
	winner.close()

	waitForCondition(t, "user slot to be released", func() bool {
		return strings.HasPrefix(loser.auth("user1", "password"), "235 ")
	})

	// After the last connection is gone, a fresh connection can
	// authenticate: both counts are back to zero.
	loser.close()
	waitForCondition(t, "all slots to be released", func() bool {
		c3 := dialLimiterServer(t, addr)
		defer c3.close()
		c3.readLine()
		c3.ehlo()
		return strings.HasPrefix(c3.auth("user1", "password"), "235 ")
	})
}

// limiterTestCert is a PEM-encoded TLS cert generated from
// src/crypto/tls:
//
//	go run generate_cert.go --rsa-bits 1024 --host 127.0.0.1,::1,example.com \
//		--ca --start-date "Jan 1 00:00:00 1970" --duration=1000000h
var limiterTestCert = []byte(`
-----BEGIN CERTIFICATE-----
MIICFDCCAX2gAwIBAgIRAK0xjnaPuNDSreeXb+z+0u4wDQYJKoZIhvcNAQELBQAw
EjEQMA4GA1UEChMHQWNtZSBDbzAgFw03MDAxMDEwMDAwMDBaGA8yMDg0MDEyOTE2
MDAwMFowEjEQMA4GA1UEChMHQWNtZSBDbzCBnzANBgkqhkiG9w0BAQEFAAOBjQAw
gYkCgYEA0nFbQQuOWsjbGtejcpWz153OlziZM4bVjJ9jYruNw5n2Ry6uYQAffhqa
JOInCmmcVe2siJglsyH9aRh6vKiobBbIUXXUU1ABd56ebAzlt0LobLlx7pZEMy30
LqIi9E6zmL3YvdGzpYlkFRnRrqwEtWYbGBf3znO250S56CCWH2UCAwEAAaNoMGYw
DgYDVR0PAQH/BAQDAgKkMBMGA1UdJQQMMAoGCCsGAQUFBwMBMA8GA1UdEwEB/wQF
MAMBAf8wLgYDVR0RBCcwJYILZXhhbXBsZS5jb22HBH8AAAGHEAAAAAAAAAAAAAAA
AAAAAAEwDQYJKoZIhvcNAQELBQADgYEAbZtDS2dVuBYvb+MnolWnCNqvw1w5Gtgi
NmvQQPOMgM3m+oQSCPRTNGSg25e1Qbo7bgQDv8ZTnq8FgOJ/rbkyERw2JckkHpD4
n4qcK27WkEDBtQFlPihIM8hLIuzWoi/9wygiElTy/tVL3y7fGCvY2/k1KBthtZGF
tN8URjVmyEo=
-----END CERTIFICATE-----`)

// limiterTestKey is the private key for limiterTestCert.
var limiterTestKey = []byte(`
-----BEGIN RSA PRIVATE KEY-----
MIICXgIBAAKBgQDScVtBC45ayNsa16NylbPXnc6XOJkzhtWMn2Niu43DmfZHLq5h
AB9+Gpok4icKaZxV7ayImCWzIf1pGHq8qKhsFshRddRTUAF3np5sDOW3QuhsuXHu
lkQzLfQuoiL0TrOYvdi90bOliWQVGdGurAS1ZhsYF/fOc7bnRLnoIJYfZQIDAQAB
AoGBAMst7OgpKyFV6c3JwyI/jWqxDySL3caU+RuTTBaodKAUx2ZEmNJIlx9eudLA
kucHvoxsM/eRxlxkhdFxdBcwU6J+zqooTnhu/FE3jhrT1lPrbhfGhyKnUrB0KKMM
VY3IQZyiehpxaeXAwoAou6TbWoTpl9t8ImAqAMY8hlULCUqlAkEA+9+Ry5FSYK/m
542LujIcCaIGoG1/Te6Sxr3hsPagKC2rH20rDLqXwEedSFOpSS0vpzlPAzy/6Rbb
PHTJUhNdwwJBANXkA+TkMdbJI5do9/mn//U0LfrCR9NkcoYohxfKz8JuhgRQxzF2
6jpo3q7CdTuuRixLWVfeJzcrAyNrVcBq87cCQFkTCtOMNC7fZnCTPUv+9q1tcJyB
vNjJu3yvoEZeIeuzouX9TJE21/33FaeDdsXbRhQEj23cqR38qFHsF1qAYNMCQQDP
QXLEiJoClkR2orAmqjPLVhR3t2oB3INcnEjLNSq8LHyQEfXyaFfu4U9l5+fRPL2i
jiC0k/9L5dHUsF0XZothAkEA23ddgRs+Id/HxtojqqUT27B8MT/IGNrYsp4DvS/c
qgkeluku4GjxRlDMBuXk94xOBEinUs+p/hwP1Alll80Tpg==
-----END RSA PRIVATE KEY-----`)

func TestServerConnLimiterAuthFailure(t *testing.T) {
	be := new(limiterBackend)
	s, addr := testLimiterServer(t, be, &smtp.MemoryLimiter{MaxPerIP: 2, MaxPerUser: 1})
	defer s.Close()

	c1 := dialLimiterServer(t, addr)
	defer c1.close()
	c1.readLine()
	c1.ehlo()

	// A failed authentication attempt consumes no user slot.
	if line := c1.auth("user1", "wrong"); !strings.HasPrefix(line, "535 ") {
		t.Fatal("Expected a 535 response, got:", line)
	}
	if line := c1.auth("user1", "password"); !strings.HasPrefix(line, "235 ") {
		t.Fatal("Invalid AUTH response:", line)
	}

	// The user group is now full for other connections.
	c2 := dialLimiterServer(t, addr)
	defer c2.close()
	c2.readLine()
	c2.ehlo()
	if line := c2.auth("user1", "password"); !strings.HasPrefix(line, "454 ") {
		t.Fatal("Expected a 454 response, got:", line)
	}

	// An abrupt disconnect (no QUIT) releases the user slot.
	c1.close()
	waitForCondition(t, "user slot to be released", func() bool {
		return strings.HasPrefix(c2.auth("user1", "password"), "235 ")
	})
}

func TestServerConnLimiterStartTLS(t *testing.T) {
	keypair, err := tls.X509KeyPair(limiterTestCert, limiterTestKey)
	if err != nil {
		t.Fatal(err)
	}

	be := new(limiterBackend)
	s, addr := testLimiterServer(t, be, &smtp.MemoryLimiter{MaxPerIP: 2, MaxPerUser: 1},
		func(s *smtp.Server) {
			s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{keypair}}
		})
	defer s.Close()

	c1 := dialLimiterServer(t, addr)
	defer c1.close()
	c1.readLine()
	c1.ehlo()
	if line := c1.auth("user1", "password"); !strings.HasPrefix(line, "235 ") {
		t.Fatal("Invalid AUTH response:", line)
	}

	// The user group is full.
	c2 := dialLimiterServer(t, addr)
	defer c2.close()
	c2.readLine()
	c2.ehlo()
	if line := c2.auth("user1", "password"); !strings.HasPrefix(line, "454 ") {
		t.Fatal("Expected a 454 response, got:", line)
	}

	// STARTTLS resets the connection state: c1 moves back to the source
	// group, releasing the user slot.
	c1.startTLS()
	c1.ehlo()

	if line := c2.auth("user1", "password"); !strings.HasPrefix(line, "235 ") {
		t.Fatal("Invalid AUTH response after STARTTLS reset:", line)
	}

	// c1 cannot take the user slot back while c2 holds it.
	if line := c1.auth("user1", "password"); !strings.HasPrefix(line, "454 ") {
		t.Fatal("Expected a 454 response, got:", line)
	}

	// Everything is released once both connections are gone.
	c1.close()
	c2.close()
	waitForCondition(t, "all slots to be released", func() bool {
		c3 := dialLimiterServer(t, addr)
		defer c3.close()
		c3.readLine()
		c3.ehlo()
		return strings.HasPrefix(c3.auth("user1", "password"), "235 ")
	})
}
