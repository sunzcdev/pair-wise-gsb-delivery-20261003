package websocket

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// newPipeConnPair returns a client and server connection wired together by
// an in-memory pipe, so tests can drive both ends of a closing handshake
// with deterministic timing.
func newPipeConnPair(t *testing.T) (client, server *Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	client = newConn(c1, false, 1024, 1024, nil, nil, nil)
	server = newConn(c2, true, 1024, 1024, nil, nil, nil)
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

// readUntilError runs the application read loop until it fails and reports
// the terminal error.
func readUntilError(c *Conn, done chan<- error) {
	for {
		if _, _, err := c.NextReader(); err != nil {
			done <- err
			return
		}
	}
}

// waitResult waits for a goroutine to report. The generous timeout is a
// guard against deadlocks, not a timing assumption.
func waitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for goroutine")
		return nil
	}
}

func asCloseError(t *testing.T, err error, code int) {
	t.Helper()
	var cerr *CloseError
	if !errors.As(err, &cerr) || cerr.Code != code {
		t.Fatalf("error = %v, want CloseError with code %d", err, code)
	}
}

// The peer initiates the closing handshake. When the application later
// shuts the connection down, CloseHandshake returns immediately because the
// peer's close message was already processed by the read loop.
func TestCloseHandshakePeerClosesFirst(t *testing.T) {
	client, server := newPipeConnPair(t)

	handlerCalled := make(chan int, 1)
	defaultHandler := client.CloseHandler()
	client.SetCloseHandler(func(code int, text string) error {
		handlerCalled <- code
		return defaultHandler(code, text)
	})

	readerDone := make(chan error, 1)
	go readUntilError(client, readerDone)
	serverReader := make(chan error, 1)
	go readUntilError(server, serverReader)

	// The peer initiates the closing handshake.
	if err := server.WriteMessage(CloseMessage, FormatCloseMessage(CloseNormalClosure, "bye")); err != nil {
		t.Fatalf("server close: %v", err)
	}

	// The read loop processes the peer's close frame and invokes the
	// close handler, which echoes a close message to the peer.
	select {
	case code := <-handlerCalled:
		if code != CloseNormalClosure {
			t.Fatalf("close handler code = %d, want %d", code, CloseNormalClosure)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close handler not called")
	}

	// The handshake already completed, so CloseHandshake does not block
	// and does not depend on the context deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.CloseHandshake(ctx, CloseNormalClosure, ""); err != nil {
		t.Fatalf("CloseHandshake: %v", err)
	}

	// The client read loop terminated with the peer's close error, and
	// the server observed the echoed close message.
	asCloseError(t, waitResult(t, readerDone), CloseNormalClosure)
	asCloseError(t, waitResult(t, serverReader), CloseNormalClosure)

	// A repeated call resolves from the same handshake.
	if err := client.CloseHandshake(ctx, CloseNormalClosure, ""); err != nil {
		t.Fatalf("second CloseHandshake: %v", err)
	}
}

// Both endpoints start the closing handshake at the same time. Each side
// observes the peer's close message and completes.
func TestCloseHandshakeSimultaneous(t *testing.T) {
	client, server := newPipeConnPair(t)

	clientReader := make(chan error, 1)
	serverReader := make(chan error, 1)
	go readUntilError(client, clientReader)
	go readUntilError(server, serverReader)

	// Release both sides at the same time so each sends its own close
	// message before reading the peer's.
	start := make(chan struct{})
	clientDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() {
		<-start
		clientDone <- client.CloseHandshake(context.Background(), CloseNormalClosure, "client")
	}()
	go func() {
		<-start
		serverDone <- server.CloseHandshake(context.Background(), CloseGoingAway, "server")
	}()
	close(start)

	if err := waitResult(t, clientDone); err != nil {
		t.Fatalf("client CloseHandshake: %v", err)
	}
	if err := waitResult(t, serverDone); err != nil {
		t.Fatalf("server CloseHandshake: %v", err)
	}

	// Each read loop observed the peer's close code.
	asCloseError(t, waitResult(t, clientReader), CloseGoingAway)
	asCloseError(t, waitResult(t, serverReader), CloseNormalClosure)
}

// The peer never answers the close message. CloseHandshake unblocks when
// the context is done and releases the underlying connection.
func TestCloseHandshakeNoResponse(t *testing.T) {
	client, server := newPipeConnPair(t)

	// The peer reads the close frame but never sends one back.
	peerDone := make(chan error, 1)
	go func() {
		_, _, err := server.NextReader()
		peerDone <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := client.CloseHandshake(ctx, CloseNormalClosure, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseHandshake error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("CloseHandshake returned after %v, before the context deadline", elapsed)
	}

	// The peer did receive the close frame.
	asCloseError(t, waitResult(t, peerDone), CloseNormalClosure)

	// The underlying connection was released: further I/O fails.
	if err := client.NetConn().SetReadDeadline(time.Now()); err == nil {
		t.Fatal("expected error from released connection")
	}
}

// Concurrent calls resolve from a single handshake: exactly one close
// message is sent and the close handler runs exactly once.
func TestCloseHandshakeSingleResult(t *testing.T) {
	client, server := newPipeConnPair(t)

	var handlerCalls int32
	defaultHandler := client.CloseHandler()
	client.SetCloseHandler(func(code int, text string) error {
		atomic.AddInt32(&handlerCalls, 1)
		return defaultHandler(code, text)
	})

	readerDone := make(chan error, 1)
	go readUntilError(client, readerDone)
	serverReader := make(chan error, 1)
	go readUntilError(server, serverReader)

	const waiters = 3
	start := make(chan struct{})
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			<-start
			errs <- client.CloseHandshake(context.Background(), CloseNormalClosure, "")
		}()
	}
	close(start)

	for i := 0; i < waiters; i++ {
		if err := waitResult(t, errs); err != nil {
			t.Fatalf("CloseHandshake: %v", err)
		}
	}

	// The server's default close handler echoed exactly one close
	// message, and the client's close handler ran exactly once.
	asCloseError(t, waitResult(t, serverReader), CloseNormalClosure)
	asCloseError(t, waitResult(t, readerDone), CloseNormalClosure)
	if n := atomic.LoadInt32(&handlerCalls); n != 1 {
		t.Fatalf("close handler called %d times, want 1", n)
	}
}
