package fasthttp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clientTraceRecorder struct {
	mu     sync.Mutex
	events []TraceEvent
}

func (r *clientTraceRecorder) trace() *ClientTrace {
	return &ClientTrace{OnPhase: func(ev TraceEvent) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	}}
}

func (r *clientTraceRecorder) forAttempt(attempt int) []TraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []TraceEvent
	for _, ev := range r.events {
		if ev.Attempt == attempt {
			out = append(out, ev)
		}
	}
	return out
}

func (r *clientTraceRecorder) attempts() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[int]struct{})
	var out []int
	for _, ev := range r.events {
		if _, ok := seen[ev.Attempt]; !ok {
			seen[ev.Attempt] = struct{}{}
			out = append(out, ev.Attempt)
		}
	}
	return out
}

func tracePhases(events []TraceEvent) []TracePhase {
	phases := make([]TracePhase, len(events))
	for i, ev := range events {
		phases[i] = ev.Phase
	}
	return phases
}

func requirePhaseOrder(t *testing.T, events []TraceEvent, want ...TracePhase) {
	t.Helper()
	got := tracePhases(events)
	if len(got) != len(want) {
		t.Fatalf("unexpected phases: got %v, want %v (events: %+v)", got, want, events)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected phases: got %v, want %v (events: %+v)", got, want, events)
		}
	}
	for _, ev := range events {
		if ev.Start.IsZero() || ev.End.Before(ev.Start) {
			t.Fatalf("event %v has invalid timestamps: %+v", ev.Phase, ev)
		}
	}
}

// startDelayedHeaderServer returns a keep-alive aware server that reads
// each request and sleeps before writing the response headers.
func startDelayedHeaderServer(t *testing.T, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					// Read request headers.
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						if line == "\r\n" {
							break
						}
					}
					time.Sleep(delay)
					if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// startFlakyServer closes the first accepted connection after reading the
// request and serves subsequent connections normally.
func startFlakyServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var connNum int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := atomic.AddInt32(&connNum, 1)
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				if n > 1 {
					conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")) //nolint:errcheck
				}
				// n == 1: close without responding, forcing a retry.
			}()
		}
	}()
	return ln.Addr().String()
}

func TestClientTracePhasesAndReuse(t *testing.T) {
	t.Parallel()

	const delay = 100 * time.Millisecond
	addr := startDelayedHeaderServer(t, delay)

	var dials int32
	hc := &HostClient{
		Addr: addr,
		Dial: func(addr string) (net.Conn, error) {
			atomic.AddInt32(&dials, 1)
			return net.Dial("tcp4", addr)
		},
	}
	uri := "http://" + addr + "/"

	// First request: fresh connection, full phase sequence.
	rec := &clientTraceRecorder{}
	req := AcquireRequest()
	resp := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)
	req.SetRequestURI(uri)
	req.SetTrace(rec.trace())

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := rec.forAttempt(1)
	// A custom Dial func cannot split out DNS, so no DNS phase is expected.
	requirePhaseOrder(t, events,
		TracePhaseConnWait, TracePhaseDial, TracePhaseWrite, TracePhaseFirstByte)
	if events[0].Reused {
		t.Fatalf("first connection must not be reported as reused: %+v", events[0])
	}
	for _, ev := range events {
		if ev.Err != nil {
			t.Fatalf("unexpected phase error: %+v", ev)
		}
	}
	if got := events[3].Duration(); got < delay-20*time.Millisecond {
		t.Fatalf("FirstByte wait %v shorter than server header delay %v", got, delay)
	}

	// Second request over the pooled connection: no dial phases may appear.
	rec2 := &clientTraceRecorder{}
	req.SetTrace(rec2.trace())
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events2 := rec2.forAttempt(1)
	requirePhaseOrder(t, events2, TracePhaseConnWait, TracePhaseWrite, TracePhaseFirstByte)
	if !events2[0].Reused {
		t.Fatalf("pooled connection must be reported as reused: %+v", events2[0])
	}
	if n := atomic.LoadInt32(&dials); n != 1 {
		t.Fatalf("expected exactly 1 dial, got %d", n)
	}
}

func TestClientTraceDNSPhase(t *testing.T) {
	t.Parallel()

	addr := startDelayedHeaderServer(t, 0)

	// No custom Dial: the built-in dialer reports DNS and connect separately.
	hc := &HostClient{Addr: addr}

	rec := &clientTraceRecorder{}
	req := AcquireRequest()
	resp := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTrace(rec.trace())

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requirePhaseOrder(t, rec.forAttempt(1),
		TracePhaseConnWait, TracePhaseDNS, TracePhaseDial, TracePhaseWrite, TracePhaseFirstByte)
}

func TestClientTraceTLSPhase(t *testing.T) {
	t.Parallel()

	s := startEchoServerTLS(t, "tcp4", "127.0.0.1:0")
	defer s.Stop()

	hc := &HostClient{
		Addr:         s.Addr(),
		IsTLS:        true,
		TLSConfig:    &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		WriteTimeout: 5 * time.Second,                       // eager TLS handshake during dial
	}

	rec := &clientTraceRecorder{}
	req := AcquireRequest()
	resp := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)
	req.SetRequestURI("https://" + s.Addr() + "/")
	req.SetTrace(rec.trace())

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	requirePhaseOrder(t, rec.forAttempt(1),
		TracePhaseConnWait, TracePhaseDNS, TracePhaseDial, TracePhaseTLS, TracePhaseWrite, TracePhaseFirstByte)
}

func TestClientTracePoolWait(t *testing.T) {
	t.Parallel()

	addr := startDelayedHeaderServer(t, 0)

	hc := &HostClient{
		Addr:               addr,
		MaxConns:           1,
		MaxConnWaitTimeout: 100 * time.Millisecond,
	}

	// Hold the only connection so the request below has to wait for it.
	cc, err := hc.AcquireConn(0, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer hc.ReleaseConn(cc)

	rec := &clientTraceRecorder{}
	req := AcquireRequest()
	resp := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTrace(rec.trace())

	err = hc.Do(req, resp)
	if !errors.Is(err, ErrNoFreeConns) {
		t.Fatalf("expected ErrNoFreeConns, got %v", err)
	}

	events := rec.forAttempt(1)
	requirePhaseOrder(t, events, TracePhaseConnWait)
	if events[0].Err == nil {
		t.Fatalf("ConnWait event must carry the pool exhaustion error: %+v", events[0])
	}
	if got := events[0].Duration(); got < 80*time.Millisecond {
		t.Fatalf("ConnWait duration %v shorter than MaxConnWaitTimeout", got)
	}
}

func TestClientTraceRetryAttempts(t *testing.T) {
	t.Parallel()

	addr := startFlakyServer(t)
	hc := &HostClient{Addr: addr}

	rec := &clientTraceRecorder{}
	req := AcquireRequest()
	resp := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTrace(rec.trace())

	// The first attempt fails (server closes the connection), the retry succeeds.
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body := string(resp.Body()); body != "ok" {
		t.Fatalf("unexpected body %q", body)
	}

	attempts := rec.attempts()
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("expected events for attempts 1 and 2, got %v", attempts)
	}

	// First attempt: the failure must be visible on its own events.
	first := rec.forAttempt(1)
	var failed bool
	for _, ev := range first {
		if ev.Err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("first attempt events must record the failure: %+v", first)
	}

	// Second attempt: a full, successful phase sequence of its own.
	// The DNS result is cached from the first attempt, so no DNS phase
	// is reported for the retry.
	second := rec.forAttempt(2)
	requirePhaseOrder(t, second,
		TracePhaseConnWait, TracePhaseDial, TracePhaseWrite, TracePhaseFirstByte)
	for _, ev := range second {
		if ev.Err != nil {
			t.Fatalf("unexpected error on retry event: %+v", ev)
		}
	}
}

func TestClientTraceCallbackPanic(t *testing.T) {
	t.Parallel()

	addr := startFlakyServer(t)
	hc := &HostClient{Addr: addr}

	panicTrace := &ClientTrace{OnPhase: func(TraceEvent) {
		panic("observer bug")
	}}

	req := AcquireRequest()
	resp := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTrace(panicTrace)

	// The panicking observer must not change the outcome: the first attempt
	// fails, the retry succeeds, and the connection returns to the pool.
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("panic in trace callback changed the request outcome: %v", err)
	}
	if body := string(resp.Body()); body != "ok" {
		t.Fatalf("unexpected body %q", body)
	}

	// The connection from the successful attempt must be back in the pool.
	rec := &clientTraceRecorder{}
	req.SetTrace(rec.trace())
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := rec.forAttempt(1)
	requirePhaseOrder(t, events, TracePhaseConnWait, TracePhaseWrite, TracePhaseFirstByte)
	if !events[0].Reused {
		t.Fatalf("connection was not returned to the pool after panicking callback: %+v", events[0])
	}
}

func TestClientTraceResetClears(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	req.SetTrace(&ClientTrace{OnPhase: func(TraceEvent) {}})
	if req.Trace() == nil {
		t.Fatal("trace not attached")
	}
	ReleaseRequest(req)

	req2 := AcquireRequest()
	defer ReleaseRequest(req2)
	if req2.Trace() != nil {
		t.Fatal("pooled request must not carry a trace")
	}
}

func BenchmarkClientTraceDisabled(b *testing.B) {
	benchmarkClientTrace(b, false)
}

func BenchmarkClientTraceEnabled(b *testing.B) {
	benchmarkClientTrace(b, true)
}

func benchmarkClientTrace(b *testing.B, enabled bool) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetBodyString("ok")
		},
		Logger: &testLogger{},
	}
	go s.Serve(ln) //nolint:errcheck
	b.Cleanup(func() { ln.Close() })

	c := &HostClient{Addr: ln.Addr().String()}

	req := AcquireRequest()
	resp := AcquireResponse()
	req.SetRequestURI(fmt.Sprintf("http://%s/", ln.Addr()))
	if enabled {
		req.SetTrace(&ClientTrace{OnPhase: func(TraceEvent) {}})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Do(req, resp); err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}
