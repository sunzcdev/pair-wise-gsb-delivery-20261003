package smtp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// contextBackend is a mock backend implementing smtp.ContextSession. It
// records the transaction contexts it observes and can block in DataContext
// until the transaction context is cancelled.
type contextBackend struct {
	mu       sync.Mutex
	mailCtxs []context.Context
	rcptCtxs []context.Context

	// blockDataAfter makes DataContext consume that many bytes of the
	// message and then block until the transaction context is cancelled,
	// simulating a backend stuck on a downstream service.
	blockDataAfter int64
	// dataResults receives the error returned by each DataContext call.
	dataResults chan error
	// dataCtxErrs receives ctx.Err() as observed at DataContext entry.
	dataCtxErrs chan error

	mailErr error
	dataErr error
}

func newContextBackend() *contextBackend {
	return &contextBackend{
		dataResults: make(chan error, 4),
		dataCtxErrs: make(chan error, 4),
	}
}

func (be *contextBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &contextSession{be: be}, nil
}

func (be *contextBackend) contexts() []context.Context {
	be.mu.Lock()
	defer be.mu.Unlock()
	return append([]context.Context(nil), be.mailCtxs...)
}

type contextSession struct {
	be *contextBackend
}

var _ smtp.ContextSession = (*contextSession)(nil)

// The server must always prefer the context-aware methods; the plain
// Session methods delegate to them in case they are called anyway.

func (s *contextSession) Mail(from string, opts *smtp.MailOptions) error {
	return s.MailContext(context.Background(), from, opts)
}

func (s *contextSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	return s.RcptContext(context.Background(), to, opts)
}

func (s *contextSession) Data(r io.Reader) error {
	return s.DataContext(context.Background(), r)
}

func (s *contextSession) MailContext(ctx context.Context, from string, opts *smtp.MailOptions) error {
	s.be.mu.Lock()
	s.be.mailCtxs = append(s.be.mailCtxs, ctx)
	s.be.mu.Unlock()
	return s.be.mailErr
}

func (s *contextSession) RcptContext(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	s.be.mu.Lock()
	s.be.rcptCtxs = append(s.be.rcptCtxs, ctx)
	s.be.mu.Unlock()
	return nil
}

func (s *contextSession) DataContext(ctx context.Context, r io.Reader) error {
	s.be.dataCtxErrs <- ctx.Err()
	if s.be.blockDataAfter > 0 {
		// Consume part of the message, then get stuck until the
		// transaction is cancelled.
		io.CopyN(io.Discard, r, s.be.blockDataAfter)
		<-ctx.Done()
		err := ctx.Err()
		s.be.dataResults <- err
		return err
	}
	if s.be.dataErr != nil {
		io.Copy(io.Discard, r)
		s.be.dataResults <- s.be.dataErr
		return s.be.dataErr
	}
	_, err := io.ReadAll(r)
	s.be.dataResults <- err
	return err
}

func (s *contextSession) Reset() {}

func (s *contextSession) Logout() error { return nil }

func testContextServer(t *testing.T, be *contextBackend) (s *smtp.Server, c net.Conn, scanner *bufio.Scanner) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s = smtp.NewServer(be)
	s.Domain = "localhost"
	s.AllowInsecureAuth = true

	go s.Serve(l)
	t.Cleanup(func() { s.Close() })

	c, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	scanner = bufio.NewScanner(c)
	scanner.Scan()
	if scanner.Text() != "220 localhost ESMTP Service Ready" {
		t.Fatal("Invalid greeting:", scanner.Text())
	}

	io.WriteString(c, "EHLO localhost\r\n")
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 || line[:4] == "250 " {
			break
		}
	}

	return
}

func expectResp(t *testing.T, scanner *bufio.Scanner, prefix string) {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("Expected %v response, got EOF", prefix)
	}
	if line := scanner.Text(); len(line) < len(prefix) || line[:len(prefix)] != prefix {
		t.Fatalf("Expected %v response, got: %v", prefix, line)
	}
}

func waitCtxCancelled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Transaction context was not cancelled")
	}
}

// TestTransactionContextClientDisconnect checks that a backend blocked in
// DataContext is released when the client disconnects in the middle of a
// BDAT transaction.
func TestTransactionContextClientDisconnect(t *testing.T) {
	be := newContextBackend()
	be.blockDataAfter = 5

	_, c, scanner := testContextServer(t, be)

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "RCPT TO:<snowden@letsgo.ua>\r\n")
	expectResp(t, scanner, "250 ")

	// Send one complete chunk so that the backend gets stuck inside
	// DataContext.
	io.WriteString(c, "BDAT 5\r\n")
	io.WriteString(c, "hello")
	expectResp(t, scanner, "250 ")

	// Announce another chunk but disconnect before sending it.
	io.WriteString(c, "BDAT 5\r\n")
	c.Close()

	select {
	case err := <-be.dataResults:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("DataContext returned unexpected error:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DataContext did not return after client disconnect")
	}

	ctxs := be.contexts()
	if len(ctxs) != 1 {
		t.Fatal("Expected 1 transaction, got:", len(ctxs))
	}
	waitCtxCancelled(t, ctxs[0])
}

// TestTransactionContextResetThenSuccess runs a cancelled transaction
// followed by a successful one on the same connection and checks that each
// transaction gets its own context.
func TestTransactionContextResetThenSuccess(t *testing.T) {
	be := newContextBackend()

	_, c, scanner := testContextServer(t, be)

	// First transaction, aborted with RSET.
	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "RCPT TO:<snowden@letsgo.ua>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "RSET\r\n")
	expectResp(t, scanner, "250 ")

	ctxs := be.contexts()
	if len(ctxs) != 1 {
		t.Fatal("Expected 1 transaction, got:", len(ctxs))
	}
	// The RSET response is sent after the reset, so the context must
	// already be cancelled.
	if !errors.Is(ctxs[0].Err(), context.Canceled) {
		t.Fatal("Transaction context is not cancelled after RSET")
	}

	// Second transaction, completed successfully.
	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "RCPT TO:<snowden@letsgo.ua>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "DATA\r\n")
	expectResp(t, scanner, "354 ")
	io.WriteString(c, "Hi\r\n.\r\n")
	expectResp(t, scanner, "250 ")

	if err := <-be.dataResults; err != nil {
		t.Fatal("DataContext returned unexpected error:", err)
	}
	// The context must be live while the transaction is in progress.
	if err := <-be.dataCtxErrs; err != nil {
		t.Fatal("Transaction context was already cancelled in DataContext:", err)
	}

	ctxs = be.contexts()
	if len(ctxs) != 2 {
		t.Fatal("Expected 2 transactions, got:", len(ctxs))
	}
	if ctxs[0] == ctxs[1] {
		t.Fatal("Cancelled transaction context was reused for the next transaction")
	}
	// RCPT must observe the same context as the enclosing MAIL transaction.
	be.mu.Lock()
	rcptCtxs := append([]context.Context(nil), be.rcptCtxs...)
	be.mu.Unlock()
	if len(rcptCtxs) != 2 || rcptCtxs[0] != ctxs[0] || rcptCtxs[1] != ctxs[1] {
		t.Fatal("RCPT did not receive the context of the enclosing transaction")
	}
	// A completed transaction is cancelled once DATA finishes.
	waitCtxCancelled(t, ctxs[1])
}

// TestTransactionContextBackendError checks that backend errors terminate
// the transaction context and that the connection stays usable.
func TestTransactionContextBackendError(t *testing.T) {
	be := newContextBackend()
	be.mailErr = errors.New("sender rejected")

	_, c, scanner := testContextServer(t, be)

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectResp(t, scanner, "451 ")

	ctxs := be.contexts()
	if len(ctxs) != 1 {
		t.Fatal("Expected 1 transaction, got:", len(ctxs))
	}
	if !errors.Is(ctxs[0].Err(), context.Canceled) {
		t.Fatal("Transaction context is not cancelled after MAIL rejection")
	}

	// A DATA failure must also end the transaction, and a fresh
	// transaction on the same connection must get a new, live context.
	be.mailErr = nil
	be.dataErr = errors.New("spam detected")

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "RCPT TO:<snowden@letsgo.ua>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "DATA\r\n")
	expectResp(t, scanner, "354 ")
	io.WriteString(c, "Buy my spam\r\n.\r\n")
	expectResp(t, scanner, "554 ")

	if err := <-be.dataResults; err == nil {
		t.Fatal("DataContext did not return the backend error")
	}

	ctxs = be.contexts()
	if len(ctxs) != 2 {
		t.Fatal("Expected 2 transactions, got:", len(ctxs))
	}
	if ctxs[0] == ctxs[1] {
		t.Fatal("Cancelled transaction context was reused for the next transaction")
	}
	waitCtxCancelled(t, ctxs[1])

	// The connection is still usable for a successful transaction.
	be.dataErr = nil
	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "RCPT TO:<snowden@letsgo.ua>\r\n")
	expectResp(t, scanner, "250 ")
	io.WriteString(c, "DATA\r\n")
	expectResp(t, scanner, "354 ")
	io.WriteString(c, "Hi\r\n.\r\n")
	expectResp(t, scanner, "250 ")

	if err := <-be.dataResults; err != nil {
		t.Fatal("DataContext returned unexpected error:", err)
	}
}
