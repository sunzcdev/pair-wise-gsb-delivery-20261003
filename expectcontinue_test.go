package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

// countingReader records the number of Read calls made against it. The
// request body is never consumed until the server allows the request to
// proceed with a '100 Continue'.
type countingReader struct {
	r     io.Reader
	reads atomic.Int32
}

func newCountingReader(body []byte) *countingReader {
	return &countingReader{r: bytes.NewReader(body)}
}

func (cr *countingReader) Read(p []byte) (int, error) {
	cr.reads.Add(1)
	return cr.r.Read(p)
}

func (cr *countingReader) ReadCount() int {
	return int(cr.reads.Load())
}

// expectContinueClient builds a HostClient backed by a net.Pipe and returns
// the server side of the connection.
func expectContinueClient(t *testing.T) (*HostClient, net.Conn) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	c := &HostClient{
		Addr:                  "example.com:80",
		Dial:                  func(string) (net.Conn, error) { return clientConn, nil },
		ExpectContinueTimeout: 100 * time.Millisecond,
	}
	t.Cleanup(c.CloseIdleConnections)
	return c, serverConn
}

// readRequestHeaders reads the request head (up to the blank line) on the
// server side and returns the raw bytes. It must not be called from the test
// goroutine, so it reports failures through the returned error instead of
// t.Fatalf (which is unsafe in other goroutines).
func readRequestHeaders(br *bufio.Reader) ([]byte, error) {
	var head bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			return nil, fmt.Errorf("reading request headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return head.Bytes(), nil
}

// mustReadRequestHeaders panics on error; server goroutines recover it into
// their serverDone channel.
func mustReadRequestHeaders(br *bufio.Reader) []byte {
	head, err := readRequestHeaders(br)
	if err != nil {
		panic(err)
	}
	return head
}

func setExpectContinueStreamRequest(req *Request, body []byte, cr *countingReader) {
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://example.com/upload")
	req.Header.Set(HeaderExpect, "100-continue")
	req.SetBodyStream(cr, len(body))
}

// closeTrackingReader wraps countingReader and records Close calls.
type closeTrackingReader struct {
	*countingReader
	closed atomic.Bool
}

func (r *closeTrackingReader) Close() error {
	r.closed.Store(true)
	return nil
}

func TestExpectContinueRejectedBufferedBodyNotReadFromSource(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := []byte("buffered-body-that-must-not-be-uploaded")

	// Materialize the body through a tracked reader first, then use the
	// resulting in-memory body. This mirrors callers that SetBody.
	cr := newCountingReader(body)
	buffered, err := io.ReadAll(cr)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("server goroutine panicked: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)
		_, _ = io.WriteString(serverConn, "HTTP/1.1 417 Expectation Failed\r\nContent-Length: 7\r\nConnection: close\r\n\r\nrefused")
	}()

	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://example.com/upload")
	req.Header.Set(HeaderExpect, "100-continue")
	req.SetBody(buffered)

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusExpectationFailed {
		t.Fatalf("unexpected status code: %d", resp.StatusCode())
	}
	if string(resp.Body()) != "refused" {
		t.Fatalf("unexpected body: %q", resp.Body())
	}
}

func TestExpectContinueRejectedClosesBodyStream(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := []byte("stream-that-should-be-closed-on-reject")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("server goroutine panicked: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)
		_, _ = io.WriteString(serverConn, "HTTP/1.1 413 Payload Too Large\r\nContent-Length: 2\r\nConnection: close\r\n\r\nno")
	}()

	tracker := &closeTrackingReader{countingReader: newCountingReader(body)}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://example.com/upload")
	req.Header.Set(HeaderExpect, "100-continue")
	req.SetBodyStream(tracker, len(body))

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tracker.ReadCount() != 0 {
		t.Fatalf("body stream read %d times after rejection; want 0", tracker.ReadCount())
	}
	if !tracker.closed.Load() {
		t.Fatalf("rejected body stream was not closed")
	}
}

func TestExpectContinueClientPropagation(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	c := &Client{
		Dial:                  func(string) (net.Conn, error) { return clientConn, nil },
		ExpectContinueTimeout: 100 * time.Millisecond,
	}

	body := []byte("client-level-config")
	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)
		if _, err := io.WriteString(serverConn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			serverDone <- err
			return
		}
		gotBody := make([]byte, len(body))
		if _, err := io.ReadFull(br, gotBody); err != nil {
			serverDone <- err
			return
		}
		if !bytes.Equal(gotBody, body) {
			serverDone <- errors.New("body mismatch")
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
		serverDone <- err
	}()

	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://example.com/upload")
	req.Header.Set(HeaderExpect, "100-continue")
	req.SetBodyStream(cr, len(body))

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode())
	}
	if cr.ReadCount() == 0 {
		t.Fatalf("body not uploaded after 100 Continue")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestExpectContinueEndToEndAcceptAndReject(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	var reject atomic.Bool
	s := &Server{
		ExpectHandler: func(ctx *RequestCtx) int {
			if reject.Load() || string(ctx.Request.Header.Peek(HeaderExpect)) != "100-continue" {
				return StatusRequestEntityTooLarge
			}
			return StatusContinue
		},
		Handler: func(ctx *RequestCtx) {
			ctx.SetStatusCode(StatusOK)
			_, _ = ctx.WriteString("got:" + string(ctx.Request.Body()))
		},
	}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Shutdown() })

	c := &Client{
		Dial:                  func(string) (net.Conn, error) { return ln.Dial() },
		ExpectContinueTimeout: time.Second,
	}

	// Accepted path: 100 Continue -> body uploaded -> 200 OK.
	body := []byte("end-to-end-body")
	cr := newCountingReader(body)
	req := AcquireRequest()
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://example.com/upload")
	req.Header.Set(HeaderExpect, "100-continue")
	req.SetBodyStream(cr, len(body))
	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("accept: unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "got:end-to-end-body" {
		t.Fatalf("accept: unexpected response: %d %q", resp.StatusCode(), resp.Body())
	}
	if cr.ReadCount() == 0 {
		t.Fatalf("accept: body was not uploaded")
	}
	ReleaseRequest(req)

	// Accepted path with an unknown body size forces chunked encoding.
	chunkBody := []byte("chunked-end-to-end")
	ccr := newCountingReader(chunkBody)
	creq := AcquireRequest()
	creq.Header.SetMethod(MethodPost)
	creq.SetRequestURI("http://example.com/upload")
	creq.Header.Set(HeaderExpect, "100-continue")
	creq.SetBodyStream(ccr, -1)
	var cresp Response
	if err := c.Do(creq, &cresp); err != nil {
		t.Fatalf("chunked accept: unexpected error: %v", err)
	}
	if cresp.StatusCode() != StatusOK || string(cresp.Body()) != "got:chunked-end-to-end" {
		t.Fatalf("chunked accept: unexpected response: %d %q", cresp.StatusCode(), cresp.Body())
	}
	if ccr.ReadCount() == 0 {
		t.Fatalf("chunked accept: body was not uploaded")
	}
	ReleaseRequest(creq)

	// Rejected path: 413 returned without reading the body.
	reject.Store(true)
	cr2 := newCountingReader(body)
	req2 := AcquireRequest()
	req2.Header.SetMethod(MethodPost)
	req2.SetRequestURI("http://example.com/upload")
	req2.Header.Set(HeaderExpect, "100-continue")
	req2.SetBodyStream(cr2, len(body))
	var resp2 Response
	if err := c.Do(req2, &resp2); err != nil {
		t.Fatalf("reject: unexpected error: %v", err)
	}
	if resp2.StatusCode() != StatusRequestEntityTooLarge {
		t.Fatalf("reject: unexpected status code: %d", resp2.StatusCode())
	}
	if cr2.ReadCount() != 0 {
		t.Fatalf("reject: body stream read %d times; want 0", cr2.ReadCount())
	}
	ReleaseRequest(req2)
}

// TestExpectContinueLate100Race verifies that a 100 Continue arriving just
// after the client timed out and started sending the body is ignored rather
// than corrupting the response.
func TestExpectContinueLate100Race(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	c := &HostClient{
		Addr:                  "example.com:80",
		Dial:                  func(string) (net.Conn, error) { return clientConn, nil },
		ExpectContinueTimeout: 40 * time.Millisecond,
	}
	t.Cleanup(c.CloseIdleConnections)

	body := []byte("late-hundred-race")
	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)
		// Wait until the client gives up and starts streaming the body, then
		// emit a late (stale) 100 Continue followed by the final response.
		gotBody := make([]byte, len(body))
		if _, err := io.ReadFull(br, gotBody); err != nil {
			serverDone <- err
			return
		}
		if _, err := io.WriteString(serverConn, "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK"); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://example.com/upload")
	req.Header.Set(HeaderExpect, "100-continue")
	req.SetBodyStream(cr, len(body))

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "OK" {
		t.Fatalf("unexpected response: %d %q", resp.StatusCode(), resp.Body())
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestExpectContinueRejectedBodyNotRead(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := []byte("large-payload-that-must-not-be-sent")
	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		head := mustReadRequestHeaders(br)
		if !bytes.Contains(bytes.ToLower(head), []byte("expect: 100-continue")) {
			serverDone <- fmt.Errorf("missing Expect header in %q", head)
			return
		}
		// Reject immediately without reading the body.
		_, err := io.WriteString(serverConn, "HTTP/1.1 413 Payload Too Large\r\nContent-Length: 9\r\nConnection: close\r\n\r\ntoo large")
		serverDone <- err
	}()

	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	setExpectContinueStreamRequest(req, body, cr)

	var resp Response
	err := c.Do(req, &resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusRequestEntityTooLarge {
		t.Fatalf("unexpected status code: %d", resp.StatusCode())
	}
	if string(resp.Body()) != "too large" {
		t.Fatalf("unexpected body: %q", resp.Body())
	}
	if n := cr.ReadCount(); n != 0 {
		t.Fatalf("request body reader was touched %d times before rejection; want 0", n)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestExpectContinueSendsBodyAfter100(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := bytes.Repeat([]byte("abcdefgh"), 1000)
	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	setExpectContinueStreamRequest(req, body, cr)

	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)

		// An unrelated interim response must not trigger the body upload.
		if _, err := io.WriteString(serverConn, "HTTP/1.1 103 Early Hints\r\nLink: </x>; rel=preload\r\n\r\n"); err != nil {
			serverDone <- err
			return
		}
		// Give the client time to process 103 and, incorrectly, start sending.
		time.Sleep(30 * time.Millisecond)
		if br.Buffered() > 0 {
			serverDone <- errors.New("client sent body after 103 instead of waiting for 100")
			return
		}

		if _, err := io.WriteString(serverConn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			serverDone <- err
			return
		}
		gotBody := make([]byte, len(body))
		if _, err := io.ReadFull(br, gotBody); err != nil {
			serverDone <- fmt.Errorf("reading request body: %w", err)
			return
		}
		if !bytes.Equal(gotBody, body) {
			serverDone <- errors.New("request body mismatch")
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
		serverDone <- err
	}()

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "OK" {
		t.Fatalf("unexpected response: %d %q", resp.StatusCode(), resp.Body())
	}
	if cr.ReadCount() == 0 {
		t.Fatalf("request body was never read after 100 Continue")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestExpectContinueTimeoutSendsBodyAnyway(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := []byte("payload-after-timeout")
	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	setExpectContinueStreamRequest(req, body, cr)

	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)
		// Do not send 100 Continue; the client must time out and send anyway.
		gotBody := make([]byte, len(body))
		if _, err := io.ReadFull(br, gotBody); err != nil {
			serverDone <- fmt.Errorf("reading request body after timeout: %w", err)
			return
		}
		if !bytes.Equal(gotBody, body) {
			serverDone <- errors.New("request body mismatch")
			return
		}
		// A late 100 Continue must be tolerated (ignored) once the body has
		// been sent; the final response follows it.
		_, err := io.WriteString(serverConn,
			"HTTP/1.1 100 Continue\r\n\r\n"+
				"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
		serverDone <- err
	}()

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK || string(resp.Body()) != "OK" {
		t.Fatalf("unexpected response: %d %q", resp.StatusCode(), resp.Body())
	}
	if cr.ReadCount() == 0 {
		t.Fatalf("request body was not sent after the continue timeout")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestExpectContinueServerClosesWhileWaiting(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := []byte("payload-that-is-never-sent")
	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	setExpectContinueStreamRequest(req, body, cr)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("server goroutine panicked: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		mustReadRequestHeaders(br)
		_ = serverConn.Close()
	}()

	var resp Response
	err := c.Do(req, &resp)
	if err == nil {
		t.Fatalf("expected an error after server closed the connection")
	}
	if n := cr.ReadCount(); n != 0 {
		t.Fatalf("request body reader was touched %d times after connection close; want 0", n)
	}
}

func TestExpectContinueConnectionPoolReuse(t *testing.T) {
	t.Parallel()

	c, serverConn := expectContinueClient(t)

	body := []byte("reuse-payload")

	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		for i := range 2 {
			mustReadRequestHeaders(br)
			if _, err := io.WriteString(serverConn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
				serverDone <- err
				return
			}
			gotBody := make([]byte, len(body))
			if _, err := io.ReadFull(br, gotBody); err != nil {
				serverDone <- fmt.Errorf("reading request %d body: %w", i, err)
				return
			}
			if !bytes.Equal(gotBody, body) {
				serverDone <- fmt.Errorf("request %d body mismatch", i)
				return
			}
			if _, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	for range 2 {
		cr := newCountingReader(body)
		req := AcquireRequest()
		setExpectContinueStreamRequest(req, body, cr)

		var resp Response
		if err := c.Do(req, &resp); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Fatalf("unexpected status code: %d", resp.StatusCode())
		}
		ReleaseRequest(req)
	}

	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
	if idle := c.IdleConnsCount(); idle != 1 {
		t.Fatalf("expected the keep-alive connection to be pooled, idle conns: %d", idle)
	}
}

func TestExpectContinueDisabledSendsImmediately(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	// ExpectContinueTimeout left at zero disables the two-phase handshake.
	c := &HostClient{
		Addr: "example.com:80",
		Dial: func(string) (net.Conn, error) { return clientConn, nil },
	}
	t.Cleanup(c.CloseIdleConnections)

	body := []byte("immediate-body")
	serverDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				serverDone <- fmt.Errorf("server panic: %v", r)
			}
		}()
		br := bufio.NewReader(serverConn)
		// Read the full request (headers + body) without sending 100 first.
		// Request.Read honors Expect and withholds the body, so read it
		// directly using the declared Content-Length.
		var req Request
		if err := req.Read(br); err != nil {
			serverDone <- err
			return
		}
		gotBody := make([]byte, req.Header.ContentLength())
		if _, err := io.ReadFull(br, gotBody); err != nil {
			serverDone <- fmt.Errorf("reading immediate body: %w", err)
			return
		}
		if !bytes.Equal(gotBody, body) {
			serverDone <- fmt.Errorf("unexpected body %q", gotBody)
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
		serverDone <- err
	}()

	cr := newCountingReader(body)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	setExpectContinueStreamRequest(req, body, cr)

	var resp Response
	if err := c.Do(req, &resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode())
	}
	if cr.ReadCount() == 0 {
		t.Fatalf("body should be sent immediately when the handshake is disabled")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
}
