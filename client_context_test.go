package fasthttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

const contextTestResponse = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"

// contextTestReadRequest reads a single request head from conn.
func contextTestReadRequest(conn net.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := conn.Read(tmp)
		if err != nil {
			return err
		}
		buf = append(buf, tmp[:n]...)
		if bytes.Contains(buf, []byte("\r\n\r\n")) {
			return nil
		}
	}
}

// contextTestServeLoop answers every request on conn with a fixed response.
func contextTestServeLoop(conn net.Conn) {
	defer conn.Close()
	for {
		if err := contextTestReadRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write([]byte(contextTestResponse)); err != nil {
			return
		}
	}
}

// contextTestPipeDial returns a DialFunc whose n-th dialed connection is
// handled by the n-th handler; extra connections use the last handler.
func contextTestPipeDial(handlers ...func(net.Conn)) DialFunc {
	var dials int32
	return func(addr string) (net.Conn, error) {
		client, server := net.Pipe()
		n := atomic.AddInt32(&dials, 1)
		h := handlers[len(handlers)-1]
		if int(n) <= len(handlers) {
			h = handlers[n-1]
		}
		go h(server)
		return client, nil
	}
}

func contextTestDoRequest(t *testing.T, c *HostClient, ctx context.Context, uri string) error {
	t.Helper()
	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI(uri)
	return c.DoContext(ctx, req, resp)
}

func contextTestExpectSuccess(t *testing.T, c *HostClient, uri string) {
	t.Helper()
	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI(uri)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("subsequent request on the same HostClient failed: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "hello" {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode(), resp.Body())
	}
}

func TestHostClientDoContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	c := &HostClient{
		Addr: "example.com:80",
		Dial: func(addr string) (net.Conn, error) {
			t.Fatal("dial must not be called for a cancelled context")
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := contextTestDoRequest(t, c, ctx, "http://example.com/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestHostClientDoContextWaitConnCancel(t *testing.T) {
	t.Parallel()

	firstRequestReceived := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer close(releaseFirst)

	c := &HostClient{
		Addr:               "example.com:80",
		MaxConns:           1,
		MaxConnWaitTimeout: time.Second,
		Dial: contextTestPipeDial(
			func(conn net.Conn) {
				// First connection: read the request and then stall,
				// so the only pooled connection stays busy.
				if err := contextTestReadRequest(conn); err != nil {
					return
				}
				close(firstRequestReceived)
				<-releaseFirst
				conn.Close()
			},
			contextTestServeLoop,
		),
	}

	// Occupy the only connection with a request that never gets a response.
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- contextTestDoRequest(t, c, firstCtx, "http://example.com/")
	}()
	select {
	case <-firstRequestReceived:
	case <-time.After(10 * time.Second):
		t.Fatal("the first request was not received by the server")
	}

	// The second request must wait for a free connection; cancel it while waiting.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if err := contextTestDoRequest(t, c, ctx, "http://example.com/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancellation took too long: %v", elapsed)
	}

	// The cancelled request must not stay in the connection wait queue.
	c.connsLock.Lock()
	queued := 0
	if c.connsWait != nil {
		queued = c.connsWait.len()
	}
	c.connsLock.Unlock()
	if queued != 0 {
		t.Fatalf("cancelled request is still in the connection wait queue: %d", queued)
	}

	// Unblock the first request by cancelling it; its connection is closed.
	firstCancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled for the first request, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first request did not finish after cancellation")
	}

	// Cancelling requests must not break the HostClient for the next call.
	contextTestExpectSuccess(t, c, "http://example.com/")
}

func TestHostClientDoContextDialCancel(t *testing.T) {
	t.Parallel()

	dialStarted := make(chan struct{})
	unblockDial := make(chan struct{})
	defer close(unblockDial)

	var dials int32
	c := &HostClient{
		Addr: "example.com:80",
		Dial: func(addr string) (net.Conn, error) {
			if atomic.AddInt32(&dials, 1) == 1 {
				close(dialStarted)
				<-unblockDial
			}
			client, server := net.Pipe()
			go contextTestServeLoop(server)
			return client, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-dialStarted:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	if err := contextTestDoRequest(t, c, ctx, "http://example.com/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// The next call dials again and must succeed.
	contextTestExpectSuccess(t, c, "http://example.com/")
}

func TestHostClientDoContextTLSHandshakeCancel(t *testing.T) {
	t.Parallel()

	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatal(err)
	}

	unblock := make(chan struct{})
	defer close(unblock)

	c := &HostClient{
		Addr:      "localhost:443",
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		// A write timeout forces the TLS handshake to happen during dial.
		WriteTimeout: 30 * time.Second,
		Dial: contextTestPipeDial(
			func(conn net.Conn) {
				// First connection: never read, so the client TLS handshake stalls.
				<-unblock
				conn.Close()
			},
			func(conn net.Conn) {
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tlsConn.Handshake(); err != nil {
					conn.Close()
					return
				}
				contextTestServeLoop(tlsConn)
			},
		),
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := contextTestDoRequest(t, c, ctx, "https://localhost/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// The next call performs a full TLS handshake and must succeed.
	contextTestExpectSuccess(t, c, "https://localhost/")
}

func TestHostClientDoContextWriteCancel(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	defer close(unblock)

	c := &HostClient{
		Addr: "example.com:80",
		Dial: contextTestPipeDial(
			func(conn net.Conn) {
				// First connection: never read, so the request write stalls.
				<-unblock
				conn.Close()
			},
			contextTestServeLoop,
		),
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := contextTestDoRequest(t, c, ctx, "http://example.com/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// The next call must get a fresh connection and succeed.
	contextTestExpectSuccess(t, c, "http://example.com/")
}

func TestHostClientDoContextReadCancel(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	defer close(unblock)

	c := &HostClient{
		Addr: "example.com:80",
		Dial: contextTestPipeDial(
			func(conn net.Conn) {
				// First connection: read the request but never respond.
				if err := contextTestReadRequest(conn); err != nil {
					return
				}
				<-unblock
				conn.Close()
			},
			contextTestServeLoop,
		),
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := contextTestDoRequest(t, c, ctx, "http://example.com/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// The next call must not observe anything from the cancelled request.
	contextTestExpectSuccess(t, c, "http://example.com/")
}

func TestHostClientDoContextLateCancelAfterConnRelease(t *testing.T) {
	t.Parallel()

	var dials int32
	var served int32
	c := &HostClient{
		Addr: "example.com:80",
		Dial: func(addr string) (net.Conn, error) {
			atomic.AddInt32(&dials, 1)
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				for {
					if err := contextTestReadRequest(server); err != nil {
						return
					}
					if _, err := server.Write([]byte(contextTestResponse)); err != nil {
						return
					}
					atomic.AddInt32(&served, 1)
				}
			}()
			return client, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := contextTestDoRequest(t, c, ctx, "http://example.com/"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := c.IdleConnsCount(); n != 1 {
		t.Fatalf("expected 1 idle connection, got %d", n)
	}

	// Cancel the context after the connection has been returned to the pool.
	// This must not close the pooled connection.
	cancel()
	time.Sleep(200 * time.Millisecond)

	// The next request must reuse the pooled connection.
	contextTestExpectSuccess(t, c, "http://example.com/")
	if n := atomic.LoadInt32(&dials); n != 1 {
		t.Fatalf("expected the connection to be reused, got %d dials", n)
	}
	if n := atomic.LoadInt32(&served); n != 2 {
		t.Fatalf("expected 2 requests on the same connection, got %d", n)
	}
}

func TestHostClientDoContextDeadlineVsTimeout(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	defer close(unblock)

	newClient := func() *HostClient {
		return &HostClient{
			Addr: "example.com:80",
			Dial: contextTestPipeDial(func(conn net.Conn) {
				// Read the request but never respond.
				if err := contextTestReadRequest(conn); err != nil {
					return
				}
				<-unblock
				conn.Close()
			}),
		}
	}

	// The context deadline is earlier than the request timeout:
	// context.DeadlineExceeded must be returned.
	c := newClient()
	req := AcquireRequest()
	resp := AcquireResponse()
	req.SetRequestURI("http://example.com/")
	req.SetTimeout(30 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.DoContext(ctx, req, resp); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the context deadline was not respected: %v", elapsed)
	}
	ReleaseRequest(req)
	ReleaseResponse(resp)

	// The request timeout is earlier than the context deadline:
	// ErrTimeout must be returned.
	c = newClient()
	req = AcquireRequest()
	defer ReleaseRequest(req)
	resp = AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://example.com/")
	req.SetTimeout(100 * time.Millisecond)
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start = time.Now()
	if err := c.DoContext(ctx, req, resp); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the request timeout was not respected: %v", elapsed)
	}
}

func TestHostClientDoContextStreamBodyCancel(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	defer close(unblock)

	c := &HostClient{
		Addr:               "example.com:80",
		StreamResponseBody: true,
		Dial: contextTestPipeDial(
			func(conn net.Conn) {
				// First connection: send headers and a partial body, then stall.
				if err := contextTestReadRequest(conn); err != nil {
					return
				}
				if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nhello")); err != nil {
					return
				}
				<-unblock
				conn.Close()
			},
			contextTestServeLoop,
		),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://example.com/")
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stream := resp.BodyStream()
	if stream == nil {
		t.Fatal("expected a response body stream")
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("cannot read the partial body: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("unexpected body prefix: %q", buf)
	}

	// Cancelling the context aborts the in-flight stream.
	cancel()
	if _, err := stream.Read(make([]byte, 5)); err == nil {
		t.Fatal("expected an error reading the stream after cancellation")
	}
	if err := resp.CloseBodyStream(); err != nil {
		t.Fatalf("cannot close the body stream: %v", err)
	}

	// The next call must get a fresh connection and succeed.
	contextTestExpectSuccess(t, c, "http://example.com/")
}

func TestClientDoContext(t *testing.T) {
	t.Parallel()

	c := &Client{
		Dial: contextTestPipeDial(contextTestServeLoop),
	}

	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://example.com/")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.DoContext(ctx, req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "hello" {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode(), resp.Body())
	}

	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := c.DoContext(cancelledCtx, req, resp); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
