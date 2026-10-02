package smtp

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
)

// gatedAuthBackend releases all in-flight AUTH exchanges only after gate is
// closed, so a test can force many connections to complete authentication at
// the same instant.
type gatedAuthBackend struct {
	gate <-chan struct{}

	mu      sync.Mutex
	started int
}

func newGatedAuthBackend(gate <-chan struct{}) *gatedAuthBackend {
	return &gatedAuthBackend{gate: gate}
}

func (be *gatedAuthBackend) NewSession(_ *Conn) (Session, error) {
	return &gatedAuthSession{backend: be}, nil
}

type gatedAuthSession struct {
	backend *gatedAuthBackend
	user    string
}

func (s *gatedAuthSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

func (s *gatedAuthSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		s.backend.mu.Lock()
		s.backend.started++
		s.backend.mu.Unlock()
		<-s.backend.gate

		if username == "bad" || password != "password" {
			return errors.New("Invalid username or password")
		}
		s.user = username
		return nil
	}), nil
}

func (s *gatedAuthSession) Username() string { return s.user }
func (s *gatedAuthSession) Reset()           {}
func (s *gatedAuthSession) Logout() error    { return nil }
func (s *gatedAuthSession) Mail(from string, opts *MailOptions) error {
	return nil
}
func (s *gatedAuthSession) Rcpt(to string, opts *RcptOptions) error {
	return nil
}
func (s *gatedAuthSession) Data(r io.Reader) error { return nil }

// addrOverrideConn reports a fixed remote address, which lets tests forge the
// source IP seen by the server (e.g. an IPv4-mapped IPv6 address).
type addrOverrideConn struct {
	net.Conn
	remote net.Addr
}

func (c *addrOverrideConn) RemoteAddr() net.Addr { return c.remote }

type tcpAddrListener struct {
	net.Listener
	ip net.IP
}

func (l *tcpAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &addrOverrideConn{Conn: c, remote: &net.TCPAddr{IP: l.ip}}, nil
}

func newLimitedServer(t *testing.T, limiter Limiter, be Backend, l net.Listener, configure ...func(*Server)) (*Server, string) {
	t.Helper()
	s := NewServer(be)
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	s.Limiter = limiter
	for _, fn := range configure {
		fn(s)
	}
	if l == nil {
		var err error
		l, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { s.Close() })
	go s.Serve(l)
	return s, l.Addr().String()
}

func (be *gatedAuthBackend) startedCount() int {
	be.mu.Lock()
	defer be.mu.Unlock()
	return be.started
}

func dialEhlo(t *testing.T, addr string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		t.Fatal("missing greeting:", sc.Err())
	}
	if !strings.HasPrefix(sc.Text(), "220 ") {
		t.Fatal("unexpected greeting:", sc.Text())
	}
	io.WriteString(c, "EHLO localhost\r\n")
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "250 ") {
			break
		}
	}
	return c, sc
}

// authenticatePlain performs an AUTH PLAIN exchange and returns the final
// status line.
func authenticatePlain(c net.Conn, sc *bufio.Scanner, username, password string) string {
	ir := encodePlainAuth(username, password)
	io.WriteString(c, "AUTH PLAIN "+ir+"\r\n")
	sc.Scan()
	return sc.Text()
}

func encodePlainAuth(username, password string) string {
	buf := make([]byte, 0, len(username)+len(password)+2)
	buf = append(buf, 0)
	buf = append(buf, username...)
	buf = append(buf, 0)
	buf = append(buf, password...)
	return base64.StdEncoding.EncodeToString(buf)
}

// A connection over the source IP quota is rejected during the connection
// phase with a 421 response, and the counter returns to zero after the
// rejected client disconnects.
func TestLimiterSourceQuotaRejectsAtConnect(t *testing.T) {
	limiter := NewMemoryLimiter(1, 0)
	_, addr := newLimitedServer(t, limiter, BackendFunc(func(*Conn) (Session, error) {
		return &noopSession{}, nil
	}), nil)

	holder, _ := dialEhlo(t, addr)
	defer holder.Close()

	ip := net.ParseIP("127.0.0.1")
	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count = %d, want 1", got)
	}

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "421 4.4.5 ") {
		t.Fatal("expected 421 4.4.5 rejection, got:", sc.Text())
	}

	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count after rejection = %d, want 1 (rejected slot never reserved)", got)
	}

	holder.Close()
	waitUntil(t, time.Second, func() bool { return limiter.SourceConns(ip) == 0 })

	// Quota is usable again once the holder leaves.
	retry, _ := dialEhlo(t, addr)
	defer retry.Close()
	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count after reconnect = %d, want 1", got)
	}
	retry.Close()
	waitUntil(t, time.Second, func() bool { return limiter.SourceConns(ip) == 0 })
}

// IPv4-mapped IPv6 addresses denote the same host as the embedded IPv4
// address and must share the source quota.
func TestLimiterIPv4MappedSharesQuota(t *testing.T) {
	limiter := NewMemoryLimiter(1, 0)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, addr := newLimitedServer(t, limiter, BackendFunc(func(*Conn) (Session, error) {
		return &noopSession{}, nil
	}), &tcpAddrListener{Listener: raw, ip: net.ParseIP("::ffff:198.51.100.7")})

	c1, _ := dialEhlo(t, addr)
	defer c1.Close()

	mapped := net.ParseIP("::ffff:198.51.100.7")
	plain := net.ParseIP("198.51.100.7")
	if got := limiter.SourceConns(mapped); got != 1 {
		t.Fatalf("mapped source count = %d, want 1", got)
	}
	if got := limiter.SourceConns(plain); got != 1 {
		t.Fatalf("plain-form lookup = %d, want 1 (mapped and plain must share a bucket)", got)
	}

	// A second connection presenting the plain IPv4 form hits the same quota.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	sc := bufio.NewScanner(c2)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "421 4.4.5 ") {
		t.Fatal("expected mapped-address connection to share quota, got:", sc.Text())
	}

	c1.Close()
	waitUntil(t, time.Second, func() bool {
		return limiter.SourceConns(mapped) == 0 && limiter.SourceConns(plain) == 0
	})
}

type noopSession struct{}

func (noopSession) Reset()                          {}
func (noopSession) Logout() error                   { return nil }
func (noopSession) Mail(string, *MailOptions) error { return nil }
func (noopSession) Rcpt(string, *RcptOptions) error { return nil }
func (noopSession) Data(io.Reader) error            { return nil }

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// Many connections release AUTH simultaneously with exactly one user slot
// free: only one may reach the user bucket, the rest are rejected during
// authentication and keep their source slots. No overselling is possible.
func TestLimiterConcurrentAuthNoOversell(t *testing.T) {
	const total = 12

	gate := make(chan struct{})
	be := newGatedAuthBackend(gate)
	limiter := NewMemoryLimiter(total+10, 1)
	_, addr := newLimitedServer(t, limiter, be, nil)

	conns := make([]net.Conn, total)
	scanners := make([]*bufio.Scanner, total)
	for i := range conns {
		conns[i], scanners[i] = dialEhlo(t, addr)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	results := make([]string, total)
	var wg sync.WaitGroup
	wg.Add(total)
	for i := range conns {
		go func(i int) {
			defer wg.Done()
			results[i] = authenticatePlain(conns[i], scanners[i], "username", "password")
		}(i)
	}

	// Wait until every exchange has parked inside the gated authenticator,
	// then release them all at once.
	waitUntil(t, time.Second, func() bool { return be.startedCount() == total })
	close(gate)
	wg.Wait()

	var ok, rejected int
	okIndex := -1
	for i, res := range results {
		switch {
		case strings.HasPrefix(res, "235 "):
			ok++
			okIndex = i
		case strings.HasPrefix(res, "454 4.7.0 "):
			rejected++
		default:
			t.Fatal("unexpected AUTH response:", res)
		}
	}
	if ok != 1 {
		t.Fatalf("successful AUTHs = %d, want exactly 1", ok)
	}
	if rejected != total-1 {
		t.Fatalf("rejected AUTHs = %d, want %d", rejected, total-1)
	}
	if got := limiter.UserConns("username"); got != 1 {
		t.Fatalf("user count = %d, want 1 (no oversell)", got)
	}
	if got := limiter.SourceConns(net.ParseIP("127.0.0.1")); got != total-1 {
		t.Fatalf("source count = %d, want %d rejected connections still in source bucket", got, total-1)
	}

	// Closing a rejected connection frees its source slot; closing the
	// authenticated one frees the user slot.
	conns[okIndex].Close()
	waitUntil(t, time.Second, func() bool { return limiter.UserConns("username") == 0 })
	for i := 0; i < total; i++ {
		if i != okIndex {
			conns[i].Close()
		}
	}
	waitUntil(t, time.Second, func() bool {
		return limiter.SourceConns(net.ParseIP("127.0.0.1")) == 0
	})
}

// A failed SASL exchange and a connection dropped mid-session never move
// counters into the user bucket, and disconnections return counts to zero.
func TestLimiterAuthFailureAndDisconnect(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	be := newGatedAuthBackend(gate)
	limiter := NewMemoryLimiter(10, 1)
	_, addr := newLimitedServer(t, limiter, be, nil)

	ip := net.ParseIP("127.0.0.1")

	// Failed credentials: 454, source slot retained, no user slot taken.
	c1, sc1 := dialEhlo(t, addr)
	defer c1.Close()
	if res := authenticatePlain(c1, sc1, "username", "wrong"); !strings.HasPrefix(res, "454 ") {
		t.Fatal("expected 454 auth failure, got:", res)
	}
	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count after failed auth = %d, want 1", got)
	}
	if got := limiter.UserConns("username"); got != 0 {
		t.Fatalf("user count after failed auth = %d, want 0", got)
	}

	// Cancelled SASL negotiation (RFC 4954 "*") likewise changes nothing.
	io.WriteString(c1, "AUTH PLAIN\r\n")
	sc1.Scan() // 334 challenge
	io.WriteString(c1, "*\r\n")
	sc1.Scan() // 501 cancelled
	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count after cancelled SASL = %d, want 1", got)
	}

	// Successful auth on the same connection moves it into the user bucket.
	if res := authenticatePlain(c1, sc1, "username", "password"); !strings.HasPrefix(res, "235 ") {
		t.Fatal("expected 235 auth success, got:", res)
	}
	if got := limiter.UserConns("username"); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
	if got := limiter.SourceConns(ip); got != 0 {
		t.Fatalf("source count after auth = %d, want 0", got)
	}

	// A second, unauthenticated connection dropped abruptly frees its slot.
	c2, _ := dialEhlo(t, addr)
	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count with second connection = %d, want 1", got)
	}
	c2.Close()
	waitUntil(t, time.Second, func() bool { return limiter.SourceConns(ip) == 0 })

	// User bucket frees when the authenticated connection closes.
	c1.Close()
	waitUntil(t, time.Second, func() bool { return limiter.UserConns("username") == 0 })
}

// A full user bucket rejects an otherwise valid AUTH; after an existing
// authenticated session ends, the waiting connection can authenticate.
func TestLimiterUserQuotaThenFreed(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	be := newGatedAuthBackend(gate)
	limiter := NewMemoryLimiter(10, 1)
	_, addr := newLimitedServer(t, limiter, be, nil)

	c1, sc1 := dialEhlo(t, addr)
	defer c1.Close()
	if res := authenticatePlain(c1, sc1, "username", "password"); !strings.HasPrefix(res, "235 ") {
		t.Fatal("first auth:", res)
	}

	c2, sc2 := dialEhlo(t, addr)
	defer c2.Close()
	if res := authenticatePlain(c2, sc2, "username", "password"); !strings.HasPrefix(res, "454 4.7.0 ") {
		t.Fatal("expected 454 4.7.0 user quota error, got:", res)
	}

	// Failed transfer must not have consumed either bucket.
	if got := limiter.UserConns("username"); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
	if got := limiter.SourceConns(net.ParseIP("127.0.0.1")); got != 1 {
		t.Fatalf("source count = %d, want 1 (rejected connection retains source slot)", got)
	}

	c1.Close()
	waitUntil(t, time.Second, func() bool { return limiter.UserConns("username") == 0 })
	if res := authenticatePlain(c2, sc2, "username", "password"); !strings.HasPrefix(res, "235 ") {
		t.Fatal("retry auth after slot freed:", res)
	}
	if got := limiter.UserConns("username"); got != 1 {
		t.Fatalf("user count after retry = %d, want 1", got)
	}
}

// STARTTLS resets the authentication state: the connection moves from the
// user bucket back into its source IP bucket, and both counters end up
// consistent.
func TestLimiterSTARTTLSMovesBackToSource(t *testing.T) {
	keypair, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	close(gate)
	be := newGatedAuthBackend(gate)
	limiter := NewMemoryLimiter(5, 5)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, addr := newLimitedServer(t, limiter, be, raw, func(s *Server) {
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{keypair}}
	})

	c, sc := dialEhlo(t, addr)
	defer c.Close()

	if res := authenticatePlain(c, sc, "username", "password"); !strings.HasPrefix(res, "235 ") {
		t.Fatal("auth:", res)
	}
	if got := limiter.UserConns("username"); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
	ip := net.ParseIP("127.0.0.1")
	if got := limiter.SourceConns(ip); got != 0 {
		t.Fatalf("source count = %d, want 0", got)
	}

	io.WriteString(c, "STARTTLS\r\n")
	sc.Scan() // 220 ready

	tlsConn := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal("TLS handshake:", err)
	}
	defer tlsConn.Close()

	waitUntil(t, time.Second, func() bool {
		return limiter.UserConns("username") == 0 && limiter.SourceConns(ip) == 1
	})

	tlsConn.Close()
	waitUntil(t, time.Second, func() bool {
		return limiter.UserConns("username") == 0 && limiter.SourceConns(ip) == 0
	})
}

// If the source IP bucket is full when STARTTLS would move an authenticated
// connection back into it, the transfer is refused with a 421 and the
// connection is closed; the user slot is released by the close and no count
// leaks.
func TestLimiterSTARTTLSFailsWhenSourceFull(t *testing.T) {
	keypair, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	close(gate)
	be := newGatedAuthBackend(gate)
	limiter := NewMemoryLimiter(1, 5) // a single source slot

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, addr := newLimitedServer(t, limiter, be, raw, func(s *Server) {
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{keypair}}
	})

	// Authenticated connection owns the one source slot's successor: after
	// AUTH the source bucket is empty and the user bucket holds it.
	authConn, authSc := dialEhlo(t, addr)
	defer authConn.Close()
	if res := authenticatePlain(authConn, authSc, "username", "password"); !strings.HasPrefix(res, "235 ") {
		t.Fatal("auth:", res)
	}

	// A second, unauthenticated connection now occupies the single source slot.
	holder, _ := dialEhlo(t, addr)
	defer holder.Close()
	ip := net.ParseIP("127.0.0.1")
	if got := limiter.SourceConns(ip); got != 1 {
		t.Fatalf("source count = %d, want 1", got)
	}

	io.WriteString(authConn, "STARTTLS\r\n")
	authSc.Scan() // 220 ready
	tlsConn := tls.Client(authConn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal("TLS handshake:", err)
	}

	// Server responds 421 over TLS and closes. The scanner should observe EOF
	// after the status line.
	tlsSc := bufio.NewScanner(tlsConn)
	if !tlsSc.Scan() {
		t.Fatal("expected 421 response, got:", tlsSc.Err())
	}
	if !strings.HasPrefix(tlsSc.Text(), "421 4.4.5 ") {
		t.Fatal("expected 421 4.4.5 on refused STARTTLS transition, got:", tlsSc.Text())
	}

	waitUntil(t, time.Second, func() bool {
		return limiter.UserConns("username") == 0 && limiter.SourceConns(ip) == 1
	})

	holder.Close()
	waitUntil(t, time.Second, func() bool { return limiter.SourceConns(ip) == 0 })
}
