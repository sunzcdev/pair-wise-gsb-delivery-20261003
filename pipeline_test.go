package fasthttp

import (
	"bufio"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func servePipelined(t *testing.T, s *Server) *fasthttputil.InmemoryListener {
	t.Helper()
	ln := fasthttputil.NewInmemoryListener()
	s.MaxPipelinedConcurrency = 4
	go s.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		s.Shutdown() //nolint:errcheck
		ln.Close()
	})
	return ln
}

func readResponseBody(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("cannot read response: %v", err)
	}
	return string(resp.Body())
}

// Responses must be written in request order even if later requests
// finish first.
func TestServerPipelinedConcurrencyResponseOrder(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if string(ctx.Path()) == "/one" {
				// Make sure the second request finishes first.
				time.Sleep(200 * time.Millisecond)
			}
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	ln := servePipelined(t, s)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "GET /one HTTP/1.1\r\nHost: a\r\n\r\n"+
		"GET /two HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	if body := readResponseBody(t, br); body != "one" {
		t.Fatalf("unexpected first response body %q, want %q", body, "one")
	}
	if body := readResponseBody(t, br); body != "two" {
		t.Fatalf("unexpected second response body %q, want %q", body, "two")
	}
}

// Pipelined requests must actually be processed concurrently: the test
// deadlocks unless all three handlers run at the same time.
func TestServerPipelinedConcurrencyParallel(t *testing.T) {
	t.Parallel()

	allStarted := make(chan struct{})
	var started atomic.Int32
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if started.Add(1) == 3 {
				close(allStarted)
			}
			select {
			case <-allStarted:
			case <-time.After(5 * time.Second):
			}
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	ln := servePipelined(t, s)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "GET /r1 HTTP/1.1\r\nHost: a\r\n\r\n"+
		"GET /r2 HTTP/1.1\r\nHost: a\r\n\r\n"+
		"GET /r3 HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	for _, want := range []string{"r1", "r2", "r3"} {
		if body := readResponseBody(t, br); body != want {
			t.Fatalf("unexpected response body %q, want %q", body, want)
		}
	}

	select {
	case <-allStarted:
	default:
		t.Fatal("pipelined requests were not processed concurrently")
	}
}

// The number of concurrently processed requests must be bounded by
// MaxPipelinedConcurrency.
func TestServerPipelinedConcurrencyBounded(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	started := make(chan struct{}, 8)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			started <- struct{}{}
			<-gate
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	ln := fasthttputil.NewInmemoryListener()
	s.MaxPipelinedConcurrency = 2
	go s.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		s.Shutdown() //nolint:errcheck
		ln.Close()
	})

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := 0; i < 4; i++ {
		if _, err := fmt.Fprintf(c, "GET /r%d HTTP/1.1\r\nHost: a\r\n\r\n", i); err != nil {
			t.Fatal(err)
		}
	}

	// Give the server some time to (not) start more handlers.
	time.Sleep(200 * time.Millisecond)
	if n := len(started); n != 2 {
		t.Fatalf("expected 2 in-flight requests, got %d", n)
	}

	close(gate)

	br := bufio.NewReader(c)
	for i := 0; i < 4; i++ {
		want := fmt.Sprintf("r%d", i)
		if body := readResponseBody(t, br); body != want {
			t.Fatalf("unexpected response body %q, want %q", body, want)
		}
	}
}

// Requests with a body form a serial boundary: earlier pipelined
// responses are flushed first and response order is preserved.
func TestServerPipelinedConcurrencySerialBoundary(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if ctx.IsPost() {
				ctx.SetBody(append([]byte("post:"), ctx.PostBody()...))
				return
			}
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	ln := servePipelined(t, s)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "GET /a HTTP/1.1\r\nHost: x\r\n\r\n"+
		"POST /b HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\n\r\nxyz"+
		"GET /c HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	for _, want := range []string{"a", "post:xyz", "c"} {
		if body := readResponseBody(t, br); body != want {
			t.Fatalf("unexpected response body %q, want %q", body, want)
		}
	}
}

// HEAD requests are pipelinable; their responses must not carry a body.
func TestServerPipelinedConcurrencyHead(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetBodyString("body")
		},
	}
	ln := servePipelined(t, s)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "HEAD /h HTTP/1.1\r\nHost: a\r\n\r\n"+
		"GET /g HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	var head Response
	head.SkipBody = true
	if err := head.Read(br); err != nil {
		t.Fatalf("cannot read HEAD response: %v", err)
	}
	if body := readResponseBody(t, br); body != "body" {
		t.Fatalf("unexpected GET response body %q", body)
	}
}

// A 'Connection: close' response from a pipelined handler terminates
// the connection; later pipelined requests are discarded.
func TestServerPipelinedConcurrencyConnectionClose(t *testing.T) {
	t.Parallel()

	s := &Server{
		MaxPipelinedConcurrency: 4,
		Handler: func(ctx *RequestCtx) {
			if string(ctx.Path()) == "/first" {
				ctx.SetConnectionClose()
			}
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	// Use a real TCP listener: fasthttputil.pipeConn doesn't wake an
	// in-progress read on SetReadDeadline, which the server relies on
	// to stop reading after a 'Connection: close' response.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		s.Shutdown() //nolint:errcheck
		ln.Close()
	})

	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "GET /first HTTP/1.1\r\nHost: a\r\n\r\n"+
		"GET /second HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("cannot read response: %v", err)
	}
	if string(resp.Body()) != "first" {
		t.Fatalf("unexpected response body %q, want %q", resp.Body(), "first")
	}
	if !resp.Header.ConnectionClose() {
		t.Fatal("expected 'Connection: close' response header")
	}

	// The connection must be closed after the first response.
	c.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected connection to be closed")
	}
}

// When the connection is closed while pipelined handlers are still
// running, they must observe it via RequestCtx.Done.
func TestServerPipelinedConcurrencyConnCloseSignal(t *testing.T) {
	t.Parallel()

	doneObserved := make(chan struct{})
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if string(ctx.Path()) == "/block" {
				select {
				case <-ctx.Done():
					close(doneObserved)
				case <-time.After(5 * time.Second):
				}
				return
			}
			ctx.SetBodyString("ok")
		},
	}
	ln := servePipelined(t, s)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprintf(c, "GET /block HTTP/1.1\r\nHost: a\r\n\r\n"+
		"GET /fast HTTP/1.1\r\nHost: a\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Give the server a moment to read both requests, then go away
	// without reading any response.
	time.Sleep(100 * time.Millisecond)
	c.Close()

	select {
	case <-doneObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("running handler did not observe connection teardown via ctx.Done()")
	}
}

// AllowPipelinedRequest can opt requests out; they are then handled
// serially while response order is preserved.
func TestServerPipelinedConcurrencyAllowFilter(t *testing.T) {
	t.Parallel()

	s := &Server{
		AllowPipelinedRequest: func(ctx *RequestCtx) bool {
			return false
		},
		Handler: func(ctx *RequestCtx) {
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	ln := servePipelined(t, s)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "GET /a HTTP/1.1\r\nHost: x\r\n\r\n"+
		"GET /b HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	for _, want := range []string{"a", "b"} {
		if body := readResponseBody(t, br); body != want {
			t.Fatalf("unexpected response body %q, want %q", body, want)
		}
	}
}

// A pipelined handler that hijacks the connection takes it over after
// all earlier responses have been written; later pipelined requests are
// discarded.
func TestServerPipelinedConcurrencyHijack(t *testing.T) {
	t.Parallel()

	hijacked := make(chan struct{})
	s := &Server{
		MaxPipelinedConcurrency: 4,
		Handler: func(ctx *RequestCtx) {
			if string(ctx.Path()) == "/hij" {
				ctx.Hijack(func(c net.Conn) {
					defer c.Close()
					c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nhij")) //nolint:errcheck
					close(hijacked)
				})
				ctx.HijackSetNoResponse(true)
				return
			}
			ctx.SetBody(ctx.Path()[1:])
		},
	}
	// Use a real TCP listener: fasthttputil.pipeConn doesn't wake an
	// in-progress read on SetReadDeadline, which the server relies on
	// to hand a hijacked connection back to the read loop.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln) //nolint:errcheck
	t.Cleanup(func() {
		s.Shutdown() //nolint:errcheck
		ln.Close()
	})

	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := fmt.Fprintf(c, "GET /a HTTP/1.1\r\nHost: x\r\n\r\n"+
		"GET /hij HTTP/1.1\r\nHost: x\r\n\r\n"+
		"GET /b HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	if body := readResponseBody(t, br); body != "a" {
		t.Fatalf("unexpected first response body %q, want %q", body, "a")
	}

	select {
	case <-hijacked:
	case <-time.After(2 * time.Second):
		t.Fatal("connection was not hijacked")
	}

	if body := readResponseBody(t, br); body != "hij" {
		t.Fatalf("unexpected hijacked response body %q, want %q", body, "hij")
	}
}
