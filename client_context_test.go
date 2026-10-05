package fasthttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

// stallConn wraps a net.Conn so tests can observe when Close is called.
// closedCh is closed when the canceled request shuts down its connection.
type stallConn struct {
	net.Conn

	closedCh  chan struct{}
	closeOnce sync.Once
}

func newStallConn(raw net.Conn) *stallConn {
	return &stallConn{
		Conn:     raw,
		closedCh: make(chan struct{}),
	}
}

func (c *stallConn) Close() error {
	c.closeOnce.Do(func() { close(c.closedCh) })
	return c.Conn.Close()
}

// blockingDialer blocks every Dial until release is closed.
type blockingDialer struct {
	release <-chan struct{}
	dials   atomic.Int32
}

func (d *blockingDialer) dial(string) (net.Conn, error) {
	d.dials.Add(1)
	<-d.release
	serverConn, clientConn := net.Pipe()
	go func() {
		_, _ = io.Copy(io.Discard, serverConn)
		_ = serverConn.Close()
	}()
	return clientConn, nil
}

func startContextTestServer(t *testing.T, block chan struct{}) (*fasthttputil.InmemoryListener, chan struct{}) {
	t.Helper()

	ln := fasthttputil.NewInmemoryListener()
	requestServed := make(chan struct{}, 1)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if block != nil {
				<-block
			}
			ctx.SetStatusCode(StatusOK)
			ctx.WriteString("ok") //nolint:errcheck
			select {
			case requestServed <- struct{}{}:
			default:
			}
		},
		Logger: &testLogger{},
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { ln.Close() })
	return ln, requestServed
}

func newContextRequest(uri string) (*Request, *Response) {
	req := AcquireRequest()
	req.SetRequestURI(uri)
	resp := AcquireResponse()
	return req, resp
}

// assertFollowupSucceeds reuses c after a canceled call and verifies the next
// request on the same HostClient still completes a full cycle.
func assertFollowupSucceeds(t *testing.T, c *HostClient, ln *fasthttputil.InmemoryListener) {
	t.Helper()

	served := make(chan struct{})
	go func() {
		srvConn, err := ln.Accept()
		if err != nil {
			return
		}
		defer srvConn.Close()
		go func() {
			br := bufio.NewReader(srvConn)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if line == "\r\n" {
					break
				}
			}
			_, _ = srvConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
			close(served)
		}()
	}()

	req, resp := newContextRequest("http://foobar/")
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("follow-up request after cancellation failed: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "ok" {
		t.Fatalf("unexpected follow-up response: status=%d body=%q", resp.StatusCode(), resp.Body())
	}
}

// TestDoContextCancelWhileWaitingForConn cancels a request while all
// connections are busy and verifies the wantConn leaves the queue, returns
// context.Canceled, and the HostClient still serves the next request.
func TestDoContextCancelWhileWaitingForConn(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	ln, _ := startContextTestServer(t, block)

	c := &HostClient{
		Addr:               "foobar",
		Dial:               func(string) (net.Conn, error) { return ln.Dial() },
		MaxConns:           1,
		MaxConnWaitTimeout: time.Minute,
	}

	// Occupy the only connection with an in-flight request blocked in the
	// server handler.
	holdStarted := make(chan error, 1)
	go func() {
		req, resp := newContextRequest("http://foobar/hold")
		holdStarted <- c.Do(req, resp)
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()

	// Wait until the held request is actually using the single connection:
	// the second request must queue. Do this by trying a short queued request
	// repeatedly is racy; instead poll the waiter count.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.connsLock.Lock()
		n := c.connsCount
		c.connsLock.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		req, resp := newContextRequest("http://foobar/waiting")
		waitErr <- c.DoContext(ctx, req, resp)
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()

	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		c.connsLock.Lock()
		queued := c.connsWait != nil && c.connsWait.len() > 0
		c.connsLock.Unlock()
		if queued {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.connsLock.Lock()
	if c.connsWait == nil || c.connsWait.len() == 0 {
		c.connsLock.Unlock()
		t.Fatal("request did not enter the connection wait queue")
	}
	c.connsLock.Unlock()

	cancel()

	select {
	case err := <-waitErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting request returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request stayed blocked in the wait queue")
	}

	// The canceled request must no longer be queued.
	c.connsLock.Lock()
	remaining := 0
	if c.connsWait != nil {
		remaining = c.connsWait.len()
	}
	c.connsLock.Unlock()
	if remaining != 0 {
		t.Fatalf("canceled request remained in the wait queue: %d", remaining)
	}

	// Release the held request so its connection returns to the pool.
	close(block)
	select {
	case <-holdStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not finish")
	}

	// The next call on the same HostClient must be unaffected.
	assertFollowupSucceeds(t, c, ln)
}

// TestDoContextCancelWhileDialing cancels while the dialer is stuck.
func TestDoContextCancelWhileDialing(t *testing.T) {
	t.Parallel()

	releaseDial := make(chan struct{})
	d := &blockingDialer{release: releaseDial}

	c := &HostClient{
		Addr: "foobar",
		Dial: d.dial,
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		req, resp := newContextRequest("http://foobar/")
		errCh <- c.DoContext(ctx, req, resp)
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()

	if d.dials.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dialing request returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request stayed blocked while dialing")
	}

	// Let the stuck dial return; its socket must be cleaned up, not pooled.
	close(releaseDial)

	ln, _ := startContextTestServer(t, nil)
	c.Dial = func(string) (net.Conn, error) { return ln.Dial() }
	assertFollowupSucceeds(t, c, ln)
}

// TestDoContextCancelDuringTLSHandshake pins the TLS handshake by accepting
// the TCP connection but never sending a ServerHello.
func TestDoContextCancelDuringTLSHandshake(t *testing.T) {
	t.Parallel()

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpLn.Close() })

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := tcpLn.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
		// Never write a ServerHello; keep the conn open until test cleanup.
		buf := make([]byte, 1024)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	c := &HostClient{
		Addr:  tcpLn.Addr().String(),
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		req, resp := newContextRequest("https://" + tcpLn.Addr().String() + "/")
		errCh <- c.DoContext(ctx, req, resp)
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()

	var serverConn net.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the TCP connection")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handshake request returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request stayed blocked during the TLS handshake")
	}

	// The raw server side must observe the close caused by cancellation.
	serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := serverConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("server connection was not closed after handshake cancellation")
	}

	// Next call uses a fresh host client; pool must have no leaked conn.
	if got := c.ConnsCount(); got != 0 {
		t.Fatalf("expected 0 pooled conns after canceled handshake, got %d", got)
	}
}

// TestDoContextCancelWhileSending blocks the request write.
func TestDoContextCancelWhileSending(t *testing.T) {
	t.Parallel()

	// A server that accepts but never reads: the client's large request body
	// eventually blocks in Write, pinning the send phase.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpLn.Close() })
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := tcpLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-make(chan struct{})
	}()

	var (
		mu    sync.Mutex
		stall *stallConn
	)
	d := func(string) (net.Conn, error) {
		clientConn, err := net.Dial("tcp", tcpLn.Addr().String())
		if err != nil {
			return nil, err
		}
		mu.Lock()
		stall = newStallConn(clientConn)
		mu.Unlock()
		return stall, nil
	}

	c := &HostClient{Addr: "foobar", Dial: d}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		req, resp := newContextRequest("http://foobar/")
		req.SetBodyString(strings.Repeat("x", 1<<20))
		errCh <- c.DoContext(ctx, req, resp)
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()

	// Give the request time to enter the blocked write.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sending request returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request stayed blocked while sending")
	}

	mu.Lock()
	select {
	case <-stall.closedCh:
	default:
		t.Fatal("connection used by the canceled send was not closed")
	}
	mu.Unlock()

	ln, _ := startContextTestServer(t, nil)
	c.Dial = func(string) (net.Conn, error) { return ln.Dial() }
	assertFollowupSucceeds(t, c, ln)
}

// TestDoContextCancelWhileReadingResponse sends headers but then stalls the
// response body read.
func TestDoContextCancelWhileReadingResponse(t *testing.T) {
	t.Parallel()

	// A raw TCP server that sends complete response headers but never the
	// declared body, so the client read blocks on the network.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpLn.Close() })
	headersSent := make(chan struct{})
	go func() {
		conn, err := tcpLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\n"); err != nil {
			return
		}
		close(headersSent)
		// Park; the client cancels and closes its side.
		buf := make([]byte, 16)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	c := &HostClient{Addr: tcpLn.Addr().String()}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		req, resp := newContextRequest("http://" + tcpLn.Addr().String() + "/")
		errCh <- c.DoContext(ctx, req, resp)
		ReleaseRequest(req)
		ReleaseResponse(resp)
	}()

	select {
	case <-headersSent:
	case <-time.After(5 * time.Second):
		t.Fatal("server never sent response headers")
	}

	// Allow the client to consume headers and block reading the missing body.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reading request returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request stayed blocked while reading the response")
	}

	ln, _ := startContextTestServer(t, nil)
	c.Addr = "foobar"
	c.Dial = func(string) (net.Conn, error) { return ln.Dial() }
	assertFollowupSucceeds(t, c, ln)
}

// TestDoContextPreCanceledReturnsImmediately verifies a context that is
// already canceled before the call fails without dialing or queuing.
func TestDoContextPreCanceledReturnsImmediately(t *testing.T) {
	t.Parallel()

	var dials atomic.Int32
	c := &HostClient{
		Addr: "foobar",
		Dial: func(string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("should never dial")
		},
		MaxConns:           1,
		MaxConnWaitTimeout: time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req, _ := newContextRequest("http://foobar/")
	defer ReleaseRequest(req)
	if err := c.DoContext(ctx, req, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("pre-canceled request triggered %d dials", dials.Load())
	}
}

// TestClientDoContextBasic exercises the Client-level DoContext entry point
// through an in-memory listener via a custom Dial.
func TestClientDoContextBasic(t *testing.T) {
	t.Parallel()

	ln, _ := startContextTestServer(t, nil)
	c := &Client{
		Dial: func(string) (net.Conn, error) { return ln.Dial() },
	}

	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.SetRequestURI("http://foobar/")
	resp := AcquireResponse()
	defer ReleaseResponse(resp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("Client.DoContext failed: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode())
	}

	// A nil response must also be accepted and drained.
	req2 := AcquireRequest()
	defer ReleaseRequest(req2)
	req2.SetRequestURI("http://foobar/")
	if err := c.DoContext(context.Background(), req2, nil); err != nil {
		t.Fatalf("Client.DoContext with nil resp failed: %v", err)
	}
}

// TestWantConnQueueRemovePreservesOrder ensures removing a canceled waiter
// keeps FIFO order of the remaining entries across the two-stage queue.
func TestWantConnQueueRemovePreservesOrder(t *testing.T) {
	t.Parallel()

	var q wantConnQueue
	ws := make([]*wantConn, 6)
	for i := range ws {
		ws[i] = &wantConn{ready: make(chan struct{}, 1)}
		q.pushBack(ws[i])
	}

	// Consume two entries to create a non-zero headPos.
	if got := q.popFront(); got != ws[0] {
		t.Fatalf("popFront = %v, want ws[0]", got)
	}
	if got := q.popFront(); got != ws[1] {
		t.Fatalf("popFront = %v, want ws[1]", got)
	}

	// Remove an entry from the middle and an entry near the end.
	q.remove(ws[3])
	q.remove(ws[5])

	want := []*wantConn{ws[2], ws[4]}
	for i, expected := range want {
		if got := q.popFront(); got != expected {
			t.Fatalf("after remove, popFront[%d] mismatch", i)
		}
	}
	if q.len() != 0 {
		t.Fatalf("expected empty queue, len=%d", q.len())
	}
	if got := q.popFront(); got != nil {
		t.Fatalf("expected nil after draining, got %v", got)
	}

	// Removing a missing entry is a no-op.
	q.remove(ws[0])
	if q.len() != 0 {
		t.Fatalf("remove on missing entry changed len: %d", q.len())
	}
}

// TestDoContextLateCancelDoesNotClosePooledConn verifies that canceling after
// a buffered response has returned does not close the connection, which may
// already be in use by the next request on the same HostClient.
func TestDoContextLateCancelDoesNotClosePooledConn(t *testing.T) {
	t.Parallel()

	ln, _ := startContextTestServer(t, nil)
	var dialCount atomic.Int32
	c := &HostClient{
		Addr: "foobar",
		Dial: func(string) (net.Conn, error) {
			dialCount.Add(1)
			return ln.Dial()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, resp := newContextRequest("http://foobar/first")
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if got := c.IdleConnsCount(); got != 1 {
		t.Fatalf("expected 1 pooled idle conn, got %d", got)
	}

	// Late cancellation after the buffered response returned: the pooled
	// connection must survive.
	cancel()
	time.Sleep(50 * time.Millisecond)
	if got := c.IdleConnsCount(); got != 1 {
		t.Fatalf("late cancellation removed the pooled connection: idle=%d", got)
	}

	// The next request must reuse the same connection (no second dial) and
	// succeed.
	req2, resp2 := newContextRequest("http://foobar/second")
	if err := c.DoContext(context.Background(), req2, resp2); err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("expected connection reuse (1 dial), got %d dials", got)
	}
}

// TestDoContextEarlierDeadlineWins verifies the earlier of the request
// timeout and the context deadline is honored.
func TestDoContextEarlierDeadlineWins(t *testing.T) {
	t.Parallel()

	// Context deadline is 100ms; server blocks far longer.
	block := make(chan struct{})
	ln, _ := startContextTestServer(t, block)
	c := &HostClient{
		Addr: "foobar",
		Dial: func(string) (net.Conn, error) { return ln.Dial() },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, resp := newContextRequest("http://foobar/")
	start := time.Now()
	err := c.DoContext(ctx, req, resp)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("earlier context deadline was not honored, elapsed=%v", elapsed)
	}
	close(block)
}

// TestDoContextRequestTimeoutEarlierThanContext verifies that an explicit
// request timeout earlier than the context deadline still fires.
func TestDoContextRequestTimeoutEarlierThanContext(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	ln, _ := startContextTestServer(t, block)
	c := &HostClient{
		Addr: "foobar",
		Dial: func(string) (net.Conn, error) { return ln.Dial() },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, resp := newContextRequest("http://foobar/")
	req.SetTimeout(100 * time.Millisecond)
	start := time.Now()
	err := c.DoContext(ctx, req, resp)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected ErrTimeout or context.DeadlineExceeded via request timeout, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("earlier request timeout was not honored, elapsed=%v", elapsed)
	}
	close(block)
}

// TestDoContextStreamingCancelInterruptsRead verifies context cancellation
// interrupts an in-flight streamed body read, and that closing the body
// stream afterward is required and does not poison the next request.
func TestDoContextStreamingCancelInterruptsRead(t *testing.T) {
	t.Parallel()

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpLn.Close() })
	headersSent := make(chan struct{})
	go func() {
		conn, err := tcpLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n"); err != nil {
			return
		}
		close(headersSent)
		// Never send the next chunk.
		buf := make([]byte, 16)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	c := &HostClient{
		Addr:               tcpLn.Addr().String(),
		StreamResponseBody: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, resp := newContextRequest("http://" + tcpLn.Addr().String() + "/")
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("streaming request failed: %v", err)
	}
	stream, ok := resp.BodyStream().(ReadCloserWithError)
	if !ok {
		t.Fatalf("expected ReadCloserWithError, got %T", resp.BodyStream())
	}

	buf := make([]byte, 16)
	if n, rerr := stream.Read(buf); rerr != nil || n != 1 || buf[0] != 'a' {
		t.Fatalf("unexpected first chunk: n=%d err=%v", n, rerr)
	}

	<-headersSent

	// The next read blocks waiting for a chunk.
	readErr := make(chan error, 1)
	go func() {
		_, rerr := stream.Read(buf)
		readErr <- rerr
	}()
	time.Sleep(100 * time.Millisecond)

	// Cancellation must interrupt the in-flight stream read.
	cancel()
	select {
	case rerr := <-readErr:
		if rerr == nil {
			t.Fatal("stream read returned nil after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream read was not interrupted by context cancellation")
	}

	// Caller must still close the body stream.
	if err := stream.CloseWithError(context.Canceled); err != nil {
		t.Fatalf("CloseWithError returned %v", err)
	}

	// The canceled streaming connection must be gone; the next request on the
	// same HostClient dials a fresh (working) connection.
	ln, _ := startContextTestServer(t, nil)
	c.Addr = "foobar"
	c.Dial = func(string) (net.Conn, error) { return ln.Dial() }
	assertFollowupSucceeds(t, c, ln)
}
