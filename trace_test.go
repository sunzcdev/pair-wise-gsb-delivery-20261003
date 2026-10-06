package fasthttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

// phaseNames turns an event slice into a compact "attempt:phase" sequence for
// asserting ordering without depending on timestamps.
func phaseNames(events []TraceEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%d:%s", e.Attempt, e.Phase.String()))
	}
	return out
}

func findPhase(events []TraceEvent, attempt int, phase TracePhase) (TraceEvent, bool) {
	for _, e := range events {
		if e.Attempt == attempt && e.Phase == phase {
			return e, true
		}
	}
	return TraceEvent{}, false
}

// startDelayingHeaderServer accepts raw TCP connections and holds the response
// until releaseCh is closed (or never sends one when closeFirst is true), so
// the wait-for-first-byte phase can be measured deterministically.
func startDelayingHeaderServer(t *testing.T, headerDelay time.Duration, closeFirst bool) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					req := AcquireRequest()
					if err := req.Read(br); err != nil {
						ReleaseRequest(req)
						return
					}
					ReleaseRequest(req)
					if closeFirst {
						return
					}
					if headerDelay > 0 {
						time.Sleep(headerDelay)
					}
					if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}

func TestTraceFreshConnectionPhases(t *testing.T) {
	t.Parallel()

	addr, stop := startDelayingHeaderServer(t, 0, false)
	defer stop()

	tr := NewRequestTrace()
	hc := &HostClient{Addr: addr}

	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTraceHooks(tr)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do: %v", err)
	}

	events := tr.Events()
	for _, want := range []TracePhase{PhaseConnect, PhaseWriteRequest, PhaseWaitResponse} {
		e, ok := findPhase(events, 1, want)
		if !ok {
			t.Fatalf("missing phase %s in %v", want, phaseNames(events))
		}
		if e.Duration() < 0 {
			t.Fatalf("phase %s negative duration: %v", want, e.Duration())
		}
		if e.Err != nil {
			t.Fatalf("phase %s unexpected err: %v", want, e.Err)
		}
	}
	// Fresh connection on the first request: no pool wait and no TLS. A literal
	// IP still passes through the default resolver, so the DNS phase is present.
	for _, absent := range []TracePhase{PhaseConnPoolWait, PhaseTLS} {
		if _, ok := findPhase(events, 1, absent); ok {
			t.Fatalf("unexpected phase %s for fresh conn: %v", absent, phaseNames(events))
		}
	}

	conns := tr.Connections()
	if len(conns) != 1 {
		t.Fatalf("want 1 GotConn, got %d", len(conns))
	}
	if conns[0].Reused || conns[0].WasIdle {
		t.Fatalf("fresh connection reported reused: %+v", conns[0])
	}
	if conns[0].RemoteAddr != addr {
		t.Fatalf("remote addr %q want %q", conns[0].RemoteAddr, addr)
	}

	// Events must be ordered: connect < write < wait(first byte).
	connect, _ := findPhase(events, 1, PhaseConnect)
	write, _ := findPhase(events, 1, PhaseWriteRequest)
	wait, _ := findPhase(events, 1, PhaseWaitResponse)
	if connect.Start.After(write.Start) || write.Start.After(wait.Start) {
		t.Fatalf("events out of order: connect %v write %v wait %v", connect.Start, write.Start, wait.Start)
	}
	if wait.End.Before(write.End) {
		t.Fatalf("wait phase should end after write flush")
	}
}

func TestTraceReusedConnectionHasNoDialPhases(t *testing.T) {
	t.Parallel()

	addr, stop := startDelayingHeaderServer(t, 0, false)
	defer stop()

	hc := &HostClient{Addr: addr}

	run := func() *RequestTrace {
		tr := NewRequestTrace()
		req := AcquireRequest()
		req.SetRequestURI("http://" + addr + "/")
		req.SetTraceHooks(tr)
		resp := AcquireResponse()
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do: %v", err)
		}
		ReleaseResponse(resp)
		ReleaseRequest(req)
		return tr
	}

	first := run()
	if e, ok := first.Phase(PhaseConnect); !ok || e.Attempt != 1 {
		t.Fatalf("first request should dial, got %v", phaseNames(first.Events()))
	}

	second := run()
	events := second.Events()
	for _, forbidden := range []TracePhase{PhaseConnect, PhaseDNS, PhaseTLS} {
		if _, ok := findPhase(events, 1, forbidden); ok {
			t.Fatalf("reused connection produced %s: %v", forbidden, phaseNames(events))
		}
	}
	// A connection taken immediately from the pool does not emit a wait phase.
	if _, ok := findPhase(events, 1, PhaseConnPoolWait); ok {
		t.Fatalf("immediate pool reuse must not emit conn pool wait: %v", phaseNames(events))
	}
	conns := second.Connections()
	if len(conns) != 1 || !conns[0].Reused || !conns[0].WasIdle {
		t.Fatalf("second request should reuse idle conn, got %+v", conns)
	}
	if _, ok := findPhase(events, 1, PhaseWriteRequest); !ok {
		t.Fatalf("reused conn still needs write phase: %v", phaseNames(events))
	}
}

func TestTraceWaitResponseDelayedHeader(t *testing.T) {
	t.Parallel()

	const delay = 60 * time.Millisecond
	addr, stop := startDelayingHeaderServer(t, delay, false)
	defer stop()

	tr := NewRequestTrace()
	hc := &HostClient{Addr: addr}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTraceHooks(tr)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do: %v", err)
	}
	wait, ok := tr.Phase(PhaseWaitResponse)
	if !ok {
		t.Fatalf("missing wait phase: %v", phaseNames(tr.Events()))
	}
	if wait.Duration() < delay*3/4 {
		t.Fatalf("TTFB wait %v too short, expected >= %v", wait.Duration(), delay)
	}
}

func TestTraceControllableDialerConnectDelay(t *testing.T) {
	t.Parallel()

	addr, stop := startDelayingHeaderServer(t, 0, false)
	defer stop()

	const dialDelay = 40 * time.Millisecond
	tr := NewRequestTrace()
	hc := &HostClient{
		Addr: addr,
		Dial: func(a string) (net.Conn, error) {
			time.Sleep(dialDelay)
			return net.Dial("tcp", a)
		},
	}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.SetRequestURI("http://" + addr + "/")
	req.SetTraceHooks(tr)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do: %v", err)
	}

	// A custom Dial is opaque: connect is timed, but no DNS phase is invented.
	if _, ok := tr.Phase(PhaseDNS); ok {
		t.Fatalf("custom dial must not synthesize a DNS phase")
	}
	connect, ok := tr.Phase(PhaseConnect)
	if !ok {
		t.Fatalf("missing connect phase: %v", phaseNames(tr.Events()))
	}
	if connect.Duration() < dialDelay*3/4 {
		t.Fatalf("connect duration %v expected >= %v", connect.Duration(), dialDelay)
	}
}

func TestTraceDefaultDialerSplitsDNSAndConnect(t *testing.T) {
	t.Parallel()

	addr, stop := startDelayingHeaderServer(t, 0, false)
	defer stop()

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	const dnsDelay = 35 * time.Millisecond
	td := &TCPDialer{
		Resolver: delayedResolver{
			delay: dnsDelay,
			ip:    net.IPv4(127, 0, 0, 1),
		},
	}

	tr := NewRequestTrace()
	// Drive the internal trace-aware default path directly, substituting the
	// controllable dialer. This is the same path HostClient uses when no custom
	// Dial function is configured, so DNS and connect stay separate.
	conn, err := td.dialTrace("trace.test:"+port, false, 5*time.Second,
		phaseTracker{hooks: tr, attempt: 1})
	if err != nil {
		t.Fatalf("dialTrace: %v", err)
	}
	defer conn.Close()

	dns, ok := tr.Phase(PhaseDNS)
	if !ok {
		t.Fatalf("missing DNS phase: %v", phaseNames(tr.Events()))
	}
	if dns.Duration() < dnsDelay*3/4 {
		t.Fatalf("dns %v expected >= %v", dns.Duration(), dnsDelay)
	}
	connect, ok := tr.Phase(PhaseConnect)
	if !ok {
		t.Fatalf("missing connect phase: %v", phaseNames(tr.Events()))
	}
	if !(dns.End.Before(connect.Start) || dns.End.Equal(connect.Start)) {
		t.Fatalf("dns must precede connect: dns %+v connect %+v", dns, connect)
	}
}

func TestTraceControllableNetDialHook(t *testing.T) {
	t.Parallel()

	addr, stop := startDelayingHeaderServer(t, 0, false)
	defer stop()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}

	var called atomic.Int32
	var gotAddress string
	var mu sync.Mutex

	td := &TCPDialer{
		Resolver: delayedResolver{ip: net.IPv4(127, 0, 0, 1)},
		NetDial: func(ctx context.Context, network, address string) (net.Conn, error) {
			called.Add(1)
			mu.Lock()
			gotAddress = address
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	}

	tr := NewRequestTrace()
	conn, err := td.dialTrace("netdial.test:"+port, false, 5*time.Second,
		phaseTracker{hooks: tr, attempt: 1})
	if err != nil {
		t.Fatalf("dialTrace: %v", err)
	}
	defer conn.Close()

	if called.Load() != 1 {
		t.Fatalf("NetDial should be invoked exactly once, got %d", called.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if host, _, _ := net.SplitHostPort(gotAddress); host != "127.0.0.1" {
		t.Fatalf("NetDial must receive the resolved address, got %q", gotAddress)
	}
	if _, ok := tr.Phase(PhaseDNS); !ok {
		t.Fatalf("DNS phase missing with NetDial: %v", phaseNames(tr.Events()))
	}
	if _, ok := tr.Phase(PhaseConnect); !ok {
		t.Fatalf("connect phase missing with NetDial: %v", phaseNames(tr.Events()))
	}
}

// delayedResolver implements Resolver, returning a fixed IP after a delay.
type delayedResolver struct {
	delay time.Duration
	ip    net.IP
}

func (r delayedResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	time.Sleep(r.delay)
	return []net.IPAddr{{IP: r.ip}}, nil
}

func startTLSTraceServer(t *testing.T) (string, func()) {
	t.Helper()
	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					req := AcquireRequest()
					if err := req.Read(br); err != nil {
						ReleaseRequest(req)
						return
					}
					ReleaseRequest(req)
					if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestTraceTLSHandshakePhase(t *testing.T) {
	t.Parallel()

	addr, stop := startTLSTraceServer(t)
	defer stop()

	tr := NewRequestTrace()
	hc := &HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.SetRequestURI("https://" + addr + "/")
	req.SetTraceHooks(tr)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do: %v", err)
	}

	tlsEvent, ok := tr.Phase(PhaseTLS)
	if !ok {
		t.Fatalf("missing TLS phase: %v", phaseNames(tr.Events()))
	}
	if tlsEvent.Err != nil || tlsEvent.Duration() < 0 {
		t.Fatalf("bad tls event: %+v", tlsEvent)
	}

	// Second request reuses the TLS connection: no new TLS or connect phases.
	tr2 := NewRequestTrace()
	req2 := AcquireRequest()
	defer ReleaseRequest(req2)
	req2.SetRequestURI("https://" + addr + "/")
	req2.SetTraceHooks(tr2)
	resp2 := AcquireResponse()
	defer ReleaseResponse(resp2)
	if err := hc.Do(req2, resp2); err != nil {
		t.Fatalf("Do2: %v", err)
	}
	for _, forbidden := range []TracePhase{PhaseTLS, PhaseConnect, PhaseDNS} {
		if _, ok := findPhase(tr2.Events(), 1, forbidden); ok {
			t.Fatalf("reused TLS conn produced %s: %v", forbidden, phaseNames(tr2.Events()))
		}
	}
}

// TestTraceRetryKeepsPerAttemptEvents makes one attempt fail (server closes
// before the first byte) and the next succeed, then asserts each attempt keeps
// its own ordered event sequence.
func TestTraceRetryKeepsPerAttemptEvents(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	var closeFirst atomic.Bool
	closeFirst.Store(true)
	srv := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetStatusCode(StatusOK)
		},
		Logger: &testLogger{},
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				if closeFirst.CompareAndSwap(true, false) {
					// Read and discard the request, then close without a
					// response so the first attempt fails while waiting for
					// the first response byte.
					go func() {
						br := bufio.NewReader(conn)
						req := AcquireRequest()
						_ = req.Read(br)
						ReleaseRequest(req)
						// Let the request finish flushing, then close with no
						// response so the failure lands in the wait phase.
						time.Sleep(60 * time.Millisecond)
						_ = conn.Close()
					}()
					return
				}
				_ = srv.ServeConn(conn)
			}(conn)
		}
	}()

	tr := NewRequestTrace()
	hc := &HostClient{
		Addr:                      "example.com",
		MaxConnDuration:           0,
		Dial:                      func(string) (net.Conn, error) { return ln.Dial() },
		MaxIdemponentCallAttempts: 3,
	}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	req.SetRequestURI("http://example.com/")
	req.Header.SetMethod(MethodGet)
	req.SetTraceHooks(tr)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do should succeed after retry, got %v", err)
	}

	if tr.Attempts() != 2 {
		t.Fatalf("want 2 attempts, got %d: %v", tr.Attempts(), phaseNames(tr.Events()))
	}
	a1 := tr.AttemptEvents(1)
	a2 := tr.AttemptEvents(2)
	if len(a1) == 0 || len(a2) == 0 {
		t.Fatalf("both attempts must record events: %v", phaseNames(tr.Events()))
	}

	// Attempt 1 writes the request and then fails waiting for the first byte.
	if _, ok := findPhase(a1, 1, PhaseWriteRequest); !ok {
		t.Fatalf("attempt 1 missing write: %v", phaseNames(a1))
	}
	write1, _ := findPhase(a1, 1, PhaseWriteRequest)
	if write1.Err != nil {
		t.Fatalf("attempt 1 write should succeed; failure is in wait phase: %+v", write1)
	}
	wait1, ok := findPhase(a1, 1, PhaseWaitResponse)
	if !ok || wait1.Err == nil {
		t.Fatalf("attempt 1 wait must carry error: %+v events %v", wait1, phaseNames(a1))
	}
	// Attempt 2 is an independent, successful sequence.
	wait2, ok := findPhase(a2, 2, PhaseWaitResponse)
	if !ok || wait2.Err != nil {
		t.Fatalf("attempt 2 wait must succeed: %+v events %v", wait2, phaseNames(a2))
	}

	// Global ordering: every attempt-1 event precedes every attempt-2 event.
	all := tr.Events()
	sawAttempt2 := false
	for _, e := range all {
		if e.Attempt == 2 {
			sawAttempt2 = true
		}
		if e.Attempt == 1 && sawAttempt2 {
			t.Fatalf("attempt 1 event after attempt 2: %v", phaseNames(all))
		}
	}

	// Failed attempt 1 must not poison attempt 2's error state.
	for _, e := range a2 {
		if e.Err != nil {
			t.Fatalf("attempt 2 event %s has unexpected err %v", e.Phase, e.Err)
		}
	}
}

// TestTracePoolWaitEmittedOnlyWhenWaiting forces a second request to wait for
// an idle connection held by a first, in-flight request.
func TestTracePoolWaitEmittedOnlyWhenWaiting(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	release := make(chan struct{})
	inside := make(chan struct{}, 2)
	srv := &Server{
		Handler: func(ctx *RequestCtx) {
			inside <- struct{}{}
			<-release
			ctx.SetStatusCode(StatusOK)
		},
		Logger: &testLogger{},
	}
	go func() { _ = srv.Serve(ln) }()

	hc := &HostClient{
		Addr:               "example.com",
		Dial:               func(string) (net.Conn, error) { return ln.Dial() },
		MaxConns:           1,
		MaxConnWaitTimeout: 5 * time.Second,
	}

	doReq := func(tr *RequestTrace) {
		req := AcquireRequest()
		defer ReleaseRequest(req)
		req.SetRequestURI("http://example.com/")
		req.Header.SetMethod(MethodGet)
		req.SetTraceHooks(tr)
		resp := AcquireResponse()
		defer ReleaseResponse(resp)
		if err := hc.Do(req, resp); err != nil {
			t.Errorf("Do: %v", err)
		}
	}

	tr1 := NewRequestTrace()
	go doReq(tr1)
	<-inside

	tr2 := NewRequestTrace()
	done2 := make(chan struct{})
	go func() { defer close(done2); doReq(tr2) }()

	// Give the second request time to block in the pool wait queue.
	time.Sleep(80 * time.Millisecond)
	close(release)
	<-done2

	if tr1.Attempts() != 1 {
		t.Fatalf("first request should have one attempt")
	}
	if _, ok := tr1.Phase(PhaseConnPoolWait); ok {
		t.Fatalf("first (only) request should not wait on the pool")
	}
	wait, ok := tr2.Phase(PhaseConnPoolWait)
	if !ok {
		t.Fatalf("second request should record pool wait: %v", phaseNames(tr2.Events()))
	}
	if wait.Duration() < 50*time.Millisecond {
		t.Fatalf("pool wait %v too short", wait.Duration())
	}
	if wait.Attempt != 1 {
		t.Fatalf("pool wait attempt %d want 1", wait.Attempt)
	}
	conns := tr2.Connections()
	if len(conns) != 1 || !conns[0].Reused {
		t.Fatalf("second request must reuse released conn: %+v", conns)
	}
}

// TestTraceHookPanicAndErrorIsolation ensures a panicking/erroring observer
// cannot change the request result, retries, or connection pooling.
func TestTraceHookPanicAndErrorIsolation(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	srv := &Server{Handler: func(ctx *RequestCtx) { ctx.SetStatusCode(StatusOK) }, Logger: &testLogger{}}
	go func() { _ = srv.Serve(ln) }()

	hc := &HostClient{Addr: "example.com", Dial: func(string) (net.Conn, error) { return ln.Dial() }}

	var panicCount atomic.Int32
	hooks := TraceHookFunc{
		Start: func(int, TracePhase, time.Time) { panic("boom-start") },
		End: func(TraceEvent) {
			panicCount.Add(1)
			panic("boom-end")
		},
		Conn: func(GotConnInfo) { panic("boom-conn") },
	}

	for i := 0; i < 3; i++ {
		req := AcquireRequest()
		req.SetRequestURI("http://example.com/")
		req.Header.SetMethod(MethodGet)
		req.SetTraceHooks(hooks)
		resp := AcquireResponse()
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("request %d failed despite panicking hooks: %v", i, err)
		}
		if resp.StatusCode() != StatusOK {
			t.Fatalf("request %d bad status %d", i, resp.StatusCode())
		}
		ReleaseResponse(resp)
		ReleaseRequest(req)
	}

	if panicCount.Load() == 0 {
		t.Fatalf("expected end hooks to have been invoked (and recovered)")
	}
	// Reused connections must still be pooled normally after panicking hooks.
	if n := hc.IdleConnsCount(); n != 1 {
		t.Fatalf("want exactly 1 pooled idle connection, got %d", n)
	}
}

// TestTraceDoesNotReferenceBuffers mutates the request/response after the call
// and verifies recorded events are unaffected (they hold no buffer aliases).
func TestTraceDoesNotReferenceBuffers(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	srv := &Server{
		Handler: func(ctx *RequestCtx) { ctx.SetBodyString("original-body") },
		Logger:  &testLogger{},
	}
	go func() { _ = srv.Serve(ln) }()

	tr := NewRequestTrace()
	hc := &HostClient{Addr: "example.com", Dial: func(string) (net.Conn, error) { return ln.Dial() }}
	req := AcquireRequest()
	req.SetRequestURI("http://example.com/")
	req.SetTraceHooks(tr)
	resp := AcquireResponse()
	if err := hc.Do(req, resp); err != nil {
		t.Fatal(err)
	}

	snapshot := tr.Events()
	connSnapshot := tr.Connections()

	// Rewrite the request and response buffers the way reuse does.
	req.Reset()
	resp.Reset()
	req.SetRequestURI("http://example.com/changed")
	resp.SetBodyString("changed-body")

	if got := phaseNames(tr.Events()); len(got) != len(snapshot) {
		t.Fatalf("events changed after buffer rewrite: %v", got)
	}
	if len(tr.Connections()) != len(connSnapshot) {
		t.Fatalf("connection records changed after buffer rewrite")
	}
	for i, e := range tr.Events() {
		if e != snapshot[i] {
			t.Fatalf("event %d mutated after reset: %+v vs %+v", i, e, snapshot[i])
		}
	}
	ReleaseResponse(resp)
	ReleaseRequest(req)
}

// TestTraceDisabledByDefault asserts no hooks are present unless enabled.
func TestTraceDisabledByDefault(t *testing.T) {
	t.Parallel()

	req := AcquireRequest()
	defer ReleaseRequest(req)
	if req.TraceHooks() != nil {
		t.Fatalf("fresh request should have no trace hooks")
	}
	req.SetTraceHooks(NewRequestTrace())
	if req.TraceHooks() == nil {
		t.Fatalf("SetTraceHooks did not install hooks")
	}
	req.SetTraceHooks(nil)
	if req.TraceHooks() != nil {
		t.Fatalf("nil SetTraceHooks should disable tracing")
	}
	req.SetTraceHooks(NewRequestTrace())
	req.Reset()
	if req.TraceHooks() != nil {
		t.Fatalf("Request.Reset must clear trace hooks")
	}
}

// panickingTrace embeds RequestTrace and panics on PhaseEnd, letting us verify
// that the built-in collector contains hook panics and reports them.
type panickingTrace struct {
	*RequestTrace
}

func (p panickingTrace) PhaseEnd(TraceEvent) {
	// Panic inside the collector's own safe-wrapped path so its containment
	// and reporting are exercised.
	p.safe(func() { panic("collector-boom") })
}

func TestRequestTraceHookPanicContained(t *testing.T) {
	t.Parallel()

	var reported atomic.Int32
	tr := NewRequestTrace()
	tr.OnHookError = func(any) { reported.Add(1) }

	pt := phaseTracker{hooks: panickingTrace{RequestTrace: tr}, attempt: 1}
	pt.end(PhaseConnect, time.Now(), nil)
	pt.end(PhaseTLS, time.Now(), nil) // phaseTracker wraps each call safely

	if got := reported.Load(); got != 2 {
		t.Fatalf("OnHookError should receive 2 recovered panics, got %d", got)
	}
	val, n := tr.HookPanic()
	if n != 2 || val != "collector-boom" {
		t.Fatalf("HookPanic = %v/%d, want collector-boom/2", val, n)
	}
}

func benchmarkTraceClient(b *testing.B, traced bool) {
	ln := fasthttputil.NewInmemoryListener()
	srv := &Server{Handler: func(ctx *RequestCtx) { ctx.SetStatusCode(StatusOK) }, Logger: &testLogger{}}
	go func() { _ = srv.Serve(ln) }()
	b.Cleanup(func() { _ = ln.Close() })

	hc := &HostClient{Addr: "example.com", Dial: func(string) (net.Conn, error) { return ln.Dial() }}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := AcquireRequest()
		req.SetRequestURI("http://example.com/")
		req.Header.SetMethod(MethodGet)
		resp := AcquireResponse()
		if traced {
			req.SetTraceHooks(NewRequestTrace())
		}
		if err := hc.Do(req, resp); err != nil {
			b.Fatal(err)
		}
		ReleaseResponse(resp)
		ReleaseRequest(req)
	}
}

func BenchmarkHostClientDoTraceDisabled(b *testing.B) {
	benchmarkTraceClient(b, false)
}

func BenchmarkHostClientDoTraceEnabled(b *testing.B) {
	benchmarkTraceClient(b, true)
}
