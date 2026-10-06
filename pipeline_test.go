package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func newPipelineServer(t *testing.T, parallel int, handler RequestHandler, allow func(*RequestHeader) bool) *Server {
	t.Helper()
	return &Server{
		Handler:             handler,
		PipelineParallelism: parallel,
		AllowPipelineRequest: func(h *RequestHeader) bool {
			if allow != nil {
				return allow(h)
			}
			return true
		},
	}
}

func servePipelineServer(t *testing.T, s *Server) (net.Conn, func()) {
	t.Helper()
	ln := fasthttputil.NewInmemoryListener()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.Serve(ln)
	}()
	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c, func() {
		c.Close()
		ln.Close()
		<-serveErr
	}
}

func TestPipelineOrderingWhenFirstSlow(t *testing.T) {
	t.Parallel()

	var secondRanBeforeFirstResponded atomic.Bool

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		switch string(ctx.Path()) {
		case "/slow":
			time.Sleep(200 * time.Millisecond)
			ctx.SetStatusCode(StatusOK)
			ctx.SetBodyString("slow")
		case "/fast":
			secondRanBeforeFirstResponded.Store(true)
			ctx.SetStatusCode(StatusOK)
			ctx.SetBodyString("fast")
		}
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	if _, err := c.Write([]byte("GET /slow HTTP/1.1\r\nHost: a\r\n\r\nGET /fast HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("read first response: %v", err)
	}
	if string(resp.Body()) != "slow" {
		t.Fatalf("first response = %q, want slow", resp.Body())
	}

	start := time.Now()
	if err := resp.Read(br); err != nil {
		t.Fatalf("read second response: %v", err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("second response took %v; it should have been buffered already", d)
	}
	if string(resp.Body()) != "fast" {
		t.Fatalf("second response = %q, want fast", resp.Body())
	}
	if !secondRanBeforeFirstResponded.Load() {
		t.Fatal("second request did not run concurrently with the first slow one")
	}
}

func TestPipelineBounded(t *testing.T) {
	t.Parallel()

	const bound = 2
	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	release := make(chan struct{})

	s := newPipelineServer(t, bound, func(ctx *RequestCtx) {
		n := inFlight.Add(1)
		for {
			old := maxObserved.Load()
			if n <= old || maxObserved.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		ctx.SetStatusCode(StatusOK)
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	var reqs bytes.Buffer
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&reqs, "GET / HTTP/1.1\r\nHost: a\r\nX: %d\r\n\r\n", i)
	}
	if _, err := c.Write(reqs.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if got := maxObserved.Load(); got > bound {
		t.Fatalf("in-flight handlers reached %d, bound is %d", got, bound)
	}
	if got := maxObserved.Load(); got != bound {
		t.Fatalf("in-flight handlers peaked at %d, want %d", got, bound)
	}

	close(release)

	br := bufio.NewReader(c)
	var resp Response
	for i := 0; i < 6; i++ {
		if err := resp.Read(br); err != nil {
			t.Fatalf("read response %d: %v", i, err)
		}
		if resp.StatusCode() != StatusOK {
			t.Fatalf("response %d status = %d", i, resp.StatusCode())
		}
	}
}

func TestPipelineSerialBoundary(t *testing.T) {
	t.Parallel()

	var order []string
	var orderCh = make(chan string, 8)

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		if string(ctx.Method()) == "POST" {
			orderCh <- "post"
			ctx.SetStatusCode(StatusAccepted)
			return
		}
		if string(ctx.Path()) == "/g1" {
			time.Sleep(120 * time.Millisecond)
		}
		orderCh <- string(ctx.Path())
		ctx.SetStatusCode(StatusOK)
		ctx.SetBody(ctx.Path())
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	w := bufio.NewWriter(c)
	w.WriteString("GET /g1 HTTP/1.1\r\nHost: a\r\n\r\n")
	w.WriteString("POST /submit HTTP/1.1\r\nHost: a\r\nContent-Length: 4\r\n\r\nbody")
	w.WriteString("GET /g2 HTTP/1.1\r\nHost: a\r\n\r\n")
	w.Flush()

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("g1: %v", err)
	}
	if string(resp.Body()) != "/g1" {
		t.Fatalf("got %q want /g1", resp.Body())
	}
	if err := resp.Read(br); err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode() != StatusAccepted {
		t.Fatalf("post status = %d", resp.StatusCode())
	}
	if err := resp.Read(br); err != nil {
		t.Fatalf("g2: %v", err)
	}
	if string(resp.Body()) != "/g2" {
		t.Fatalf("got %q want /g2", resp.Body())
	}

	close(orderCh)
	for s := range orderCh {
		order = append(order, s)
	}
	if len(order) != 3 || order[0] != "/g1" || order[1] != "post" || order[2] != "/g2" {
		t.Fatalf("handler order = %v", order)
	}
}

func TestPipelineDisconnectCancels(t *testing.T) {
	t.Parallel()

	canceled := make(chan struct{})

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		<-ctx.Done()
		close(canceled)
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer func() {
		select {
		case <-canceled:
		default:
		}
		cleanup()
	}()

	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(80 * time.Millisecond)
	c.Close()

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight handler did not receive cancellation after disconnect")
	}
}

func TestPipelineDisabledByDefault(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			time.Sleep(50 * time.Millisecond)
			ctx.SetStatusCode(StatusTeapot)
		},
	}

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode() != StatusTeapot {
		t.Fatalf("status = %d want %d", resp.StatusCode(), StatusTeapot)
	}
}

func TestPipelineRejectedByHook(t *testing.T) {
	t.Parallel()

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		ctx.SetStatusCode(StatusOK)
		ctx.SetBody(ctx.Path())
	}, func(h *RequestHeader) bool {
		return string(h.RequestURI()) == "/parallel"
	})

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	if _, err := c.Write([]byte(
		"GET /serial HTTP/1.1\r\nHost: a\r\n\r\n" +
			"GET /parallel HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	for i, want := range []string{"/serial", "/parallel"} {
		if err := resp.Read(br); err != nil {
			t.Fatalf("resp %d: %v", i, err)
		}
		if string(resp.Body()) != want {
			t.Fatalf("resp %d = %q want %q", i, resp.Body(), want)
		}
	}
}

func TestPipelineUpgradeFormsSerialBoundary(t *testing.T) {
	t.Parallel()

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		if ctx.Request.Header.ConnectionUpgrade() {
			ctx.SetStatusCode(StatusSwitchingProtocols)
			return
		}
		ctx.SetStatusCode(StatusOK)
		ctx.SetBodyString("ok")
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	req := "GET /a HTTP/1.1\r\nHost: a\r\n\r\n" +
		"GET /up HTTP/1.1\r\nHost: a\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n" +
		"GET /b HTTP/1.1\r\nHost: a\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("a: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("a status = %d", resp.StatusCode())
	}
	if err := resp.Read(br); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if resp.StatusCode() != StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", resp.StatusCode())
	}
	if err := resp.Read(br); err != nil {
		t.Fatalf("b: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("b status = %d", resp.StatusCode())
	}
}

func TestPipelineHeadHasNoBody(t *testing.T) {
	t.Parallel()

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		ctx.SetBodyString("this-body-must-not-be-sent")
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	if _, err := c.Write([]byte("HEAD / HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	resp.SkipBody = true
	if err := resp.Read(br); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("status = %d", resp.StatusCode())
	}
	if len(resp.Body()) != 0 {
		t.Fatalf("HEAD response carried body %q", resp.Body())
	}
	if resp.Header.ContentLength() != len("this-body-must-not-be-sent") {
		t.Fatalf("HEAD content-length = %d", resp.Header.ContentLength())
	}
}

func TestPipelineConnectionCloseDrainsInOrder(t *testing.T) {
	t.Parallel()

	s := newPipelineServer(t, 2, func(ctx *RequestCtx) {
		switch string(ctx.Path()) {
		case "/slow":
			time.Sleep(120 * time.Millisecond)
		case "/close":
			ctx.SetConnectionClose()
		}
		ctx.SetStatusCode(StatusOK)
		ctx.SetBody(ctx.Path())
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	if _, err := c.Write([]byte(
		"GET /slow HTTP/1.1\r\nHost: a\r\n\r\n" +
			"GET /close HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("slow: %v", err)
	}
	if string(resp.Body()) != "/slow" {
		t.Fatalf("got %q want /slow", resp.Body())
	}
	if err := resp.Read(br); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(resp.Body()) != "/close" {
		t.Fatalf("got %q want /close", resp.Body())
	}
	if !resp.Header.ConnectionClose() {
		t.Fatal("expected Connection: close")
	}
}

func TestPipelineIndependentContexts(t *testing.T) {
	t.Parallel()

	s := newPipelineServer(t, 3, func(ctx *RequestCtx) {
		path := string(ctx.Path())
		ctx.SetUserValue("path", path)
		time.Sleep(20 * time.Millisecond)
		if got, _ := ctx.UserValue("path").(string); got != path {
			t.Errorf("user value leaked: got %q want %q", got, path)
		}
		ctx.SetBodyString(path)
	}, nil)

	c, cleanup := servePipelineServer(t, s)
	defer cleanup()

	var reqs bytes.Buffer
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&reqs, "GET /p%d HTTP/1.1\r\nHost: a\r\n\r\n", i)
	}
	if _, err := c.Write(reqs.Bytes()); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	for i := 0; i < 5; i++ {
		if err := resp.Read(br); err != nil {
			t.Fatalf("resp %d: %v", i, err)
		}
		want := fmt.Sprintf("/p%d", i)
		if string(resp.Body()) != want {
			t.Fatalf("resp %d = %q want %q", i, resp.Body(), want)
		}
	}
}

// TestPipelineIdleTimeoutEndsConn verifies that IdleTimeout closes an idle
// keep-alive connection even when parallel mode is enabled.
func TestPipelineIdleTimeoutEndsConn(t *testing.T) {
	t.Parallel()

	s := newPipelineServer(t, 4, func(ctx *RequestCtx) {
		ctx.SetStatusCode(StatusOK)
	}, nil)
	s.IdleTimeout = 50 * time.Millisecond

	ln := fasthttputil.NewInmemoryListener()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.Serve(ln)
	}()
	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		c.Close()
		ln.Close()
		<-serveErr
	}()

	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("read: %v", err)
	}

	// After the idle timeout the server should stop the connection; a further
	// read must fail rather than block forever.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := resp.Read(br); err == nil {
		t.Fatal("expected connection to be closed after idle timeout")
	}
}
