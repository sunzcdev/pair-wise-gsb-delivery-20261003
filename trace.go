package fasthttp

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// TracePhase is a distinct stage of a client request attempt whose timing is
// reported through TraceHooks.
type TracePhase int

const (
	// PhaseConnPoolWait is the time spent waiting for an idle connection in the
	// connection pool. It is reported only when a request actually has to wait;
	// reusing an immediately available idle connection does not emit this phase.
	PhaseConnPoolWait TracePhase = iota

	// PhaseDNS is the time spent resolving the host name into IP addresses.
	// It is reported only for connections established through the fasthttp
	// default dialer (TCPDialer), which performs name resolution itself.
	// When a custom Dial/DialTimeout function is configured the dialer owns
	// resolution, so this phase is absent and the lookup is part of
	// PhaseConnect.
	PhaseDNS

	// PhaseConnect is the time spent establishing the underlying transport
	// connection (TCP dial). For a custom dial function this also includes any
	// work that function performs, such as its own name resolution.
	PhaseConnect

	// PhaseTLS is the time spent on the TLS handshake. It is reported only for
	// newly established HTTPS connections when fasthttp performs the handshake.
	// Reusing a keep-alive connection never reports it, and neither does a
	// dial function that returns a connection already carrying TLS.
	PhaseTLS

	// PhaseWriteRequest is the time spent writing and flushing the request.
	PhaseWriteRequest

	// PhaseWaitResponse is the time between the request being fully flushed and
	// receiving the first byte of the response headers (time to first byte).
	PhaseWaitResponse
)

// String returns the name of the phase.
func (p TracePhase) String() string {
	switch p {
	case PhaseConnPoolWait:
		return "conn_pool_wait"
	case PhaseDNS:
		return "dns"
	case PhaseConnect:
		return "connect"
	case PhaseTLS:
		return "tls"
	case PhaseWriteRequest:
		return "write_request"
	case PhaseWaitResponse:
		return "wait_response"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// TraceEvent records the timing of a single TracePhase within one attempt.
//
// Events are owned by the observer and contain only copied scalar values; they
// never reference the Request or Response (or their buffers), which fasthttp
// may reuse or reset as soon as the call returns. It is safe to retain a
// TraceEvent for as long as needed.
type TraceEvent struct {
	// Phase is the stage this event describes.
	Phase TracePhase

	// Attempt is the 1-based index of the attempt the event belongs to.
	// Each retry gets its own sequence of events.
	Attempt int

	// Start and End are timestamps captured with time.Now. When monotonic
	// clocks are available (the default), End.Sub(Start) measures the elapsed
	// phase time without being affected by wall-clock adjustments.
	Start time.Time
	End   time.Time

	// Err is the error that terminated the phase, if any. A completed phase
	// leaves Err nil.
	Err error
}

// Duration returns how long the phase took.
func (e TraceEvent) Duration() time.Duration {
	if e.End.IsZero() {
		return 0
	}
	return e.End.Sub(e.Start)
}

// GotConnInfo describes the connection an attempt ended up using.
//
// Like TraceEvent it only carries copied values and never references request
// or response buffers.
type GotConnInfo struct {
	// Attempt is the 1-based index of the attempt.
	Attempt int

	// Reused reports whether the connection was taken from the idle pool
	// instead of being freshly dialed.
	Reused bool

	// WasIdle reports whether the connection was previously used and pooled.
	WasIdle bool

	// RemoteAddr and LocalAddr are copies of the connection endpoint strings.
	// They are empty when unavailable.
	RemoteAddr string
	LocalAddr  string

	// Err is the error that prevented obtaining a connection, if any.
	Err error
}

// TraceHooks observes the internal timing of a single request. Install an
// implementation with Request.SetTraceHooks to enable tracing for that request
// only; requests without hooks pay no tracing cost.
//
// fasthttp may invoke the connection-establishment hooks from a different
// goroutine than the caller when the connection pool dials on behalf of a
// queued waiter, so implementations must be safe for concurrent use.
//
// Observers must not change request behavior: there is no error return, and a
// panic inside a hook is recovered by fasthttp. A misbehaving hook never
// changes connection pooling, the returned request error, or the retry result.
// Use RequestTrace when the built-in collector is sufficient.
type TraceHooks interface {
	// PhaseStart is called when a phase begins.
	PhaseStart(attempt int, phase TracePhase, at time.Time)

	// PhaseEnd is called when a phase ends. err is set when the phase failed.
	PhaseEnd(event TraceEvent)

	// GotConn is called once per attempt after a connection was obtained or
	// failed to be obtained.
	GotConn(info GotConnInfo)
}

// RequestTrace is the built-in TraceHooks implementation that records every
// event of a request, keeping one independent sequence per attempt so retries
// never overwrite each other.
//
// A RequestTrace must not be shared between concurrent requests. Create one
// per request with NewRequestTrace.
type RequestTrace struct {
	// OnHookError, when non-nil, receives any value recovered from a panicking
	// method of this RequestTrace. It is invoked while the panic is already
	// contained, so it cannot affect the request. It may be nil; in that case
	// the value is only stored and returned by HookPanic.
	OnHookError func(recovered any)

	mu         sync.Mutex
	events     []TraceEvent
	conns      []GotConnInfo
	hookPanic  any
	panicCount int
}

// NewRequestTrace returns a ready-to-use RequestTrace.
func NewRequestTrace() *RequestTrace {
	return &RequestTrace{}
}

// PhaseStart implements TraceHooks.
func (t *RequestTrace) PhaseStart(int, TracePhase, time.Time) {
	// The matching PhaseEnd carries the start timestamp, so nothing is stored.
}

// PhaseEnd implements TraceHooks by appending the event to the attempt log.
func (t *RequestTrace) PhaseEnd(event TraceEvent) {
	t.safe(func() {
		t.mu.Lock()
		t.events = append(t.events, event)
		t.mu.Unlock()
	})
}

// GotConn implements TraceHooks.
func (t *RequestTrace) GotConn(info GotConnInfo) {
	t.safe(func() {
		t.mu.Lock()
		t.conns = append(t.conns, info)
		t.mu.Unlock()
	})
}

func (t *RequestTrace) safe(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			t.mu.Lock()
			if t.panicCount == 0 {
				t.hookPanic = r
			}
			t.panicCount++
			cb := t.OnHookError
			t.mu.Unlock()
			if cb != nil {
				func() {
					defer func() {
						_ = recover()
					}()
					cb(r)
				}()
			}
		}
	}()
	fn()
}

// Events returns a copy of all recorded phase events, in the order they were
// observed and grouped by attempt (attempt 1's events, then attempt 2's, ...).
func (t *RequestTrace) Events() []TraceEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TraceEvent(nil), t.events...)
}

// Attempts returns the highest attempt index that recorded events.
func (t *RequestTrace) Attempts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, e := range t.events {
		if e.Attempt > n {
			n = e.Attempt
		}
	}
	return n
}

// AttemptEvents returns a copy of the events belonging to the given 1-based
// attempt. It returns nil for attempts that did not happen.
func (t *RequestTrace) AttemptEvents(attempt int) []TraceEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []TraceEvent
	for _, e := range t.events {
		if e.Attempt == attempt {
			out = append(out, e)
		}
	}
	return out
}

// Connections returns a copy of the per-attempt GotConn records.
func (t *RequestTrace) Connections() []GotConnInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]GotConnInfo(nil), t.conns...)
}

// Phase returns the last recorded event for the given phase across all
// attempts, and whether it was found.
func (t *RequestTrace) Phase(phase TracePhase) (TraceEvent, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.events) - 1; i >= 0; i-- {
		if t.events[i].Phase == phase {
			return t.events[i], true
		}
	}
	return TraceEvent{}, false
}

// HookPanic returns the first value recovered from a panicking hook and the
// number of panics that were contained.
func (t *RequestTrace) HookPanic() (any, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hookPanic, t.panicCount
}

// TraceHookFunc adapts callbacks into TraceHooks. Nil callbacks are skipped.
// Panics in the callbacks are recovered by fasthttp, so this adapter is safe
// to use with arbitrary observation code.
type TraceHookFunc struct {
	Start func(attempt int, phase TracePhase, at time.Time)
	End   func(event TraceEvent)
	Conn  func(info GotConnInfo)
}

// PhaseStart implements TraceHooks.
func (f TraceHookFunc) PhaseStart(attempt int, phase TracePhase, at time.Time) {
	if f.Start != nil {
		f.Start(attempt, phase, at)
	}
}

// PhaseEnd implements TraceHooks.
func (f TraceHookFunc) PhaseEnd(event TraceEvent) {
	if f.End != nil {
		f.End(event)
	}
}

// GotConn implements TraceHooks.
func (f TraceHookFunc) GotConn(info GotConnInfo) {
	if f.Conn != nil {
		f.Conn(info)
	}
}

// phaseTracker wraps TraceHooks with attempt-aware, panic-safe emission. The
// zero value is a disabled tracker, and every method is a no-op on it.
type phaseTracker struct {
	hooks   TraceHooks
	attempt int
}

func (pt phaseTracker) enabled() bool { return pt.hooks != nil }

func (pt phaseTracker) start(phase TracePhase) time.Time {
	at := time.Now()
	if pt.hooks != nil {
		safeTraceCall(func() { pt.hooks.PhaseStart(pt.attempt, phase, at) })
	}
	return at
}

func (pt phaseTracker) end(phase TracePhase, start time.Time, err error) {
	if pt.hooks != nil {
		safeTraceCall(func() {
			pt.hooks.PhaseEnd(TraceEvent{
				Phase:   phase,
				Attempt: pt.attempt,
				Start:   start,
				End:     time.Now(),
				Err:     err,
			})
		})
	}
}

func (pt phaseTracker) gotConn(reused, wasIdle bool, conn net.Conn, err error) {
	if pt.hooks == nil {
		return
	}
	info := GotConnInfo{Attempt: pt.attempt, Reused: reused, WasIdle: wasIdle, Err: err}
	if conn != nil {
		if ra := conn.RemoteAddr(); ra != nil {
			info.RemoteAddr = ra.String()
		}
		if la := conn.LocalAddr(); la != nil {
			info.LocalAddr = la.String()
		}
	}
	safeTraceCall(func() { pt.hooks.GotConn(info) })
}

// safeTraceCall runs an observer callback so a panic cannot escape into the
// request path. It is used for externally supplied hooks; the built-in
// RequestTrace additionally recovers internally.
func safeTraceCall(fn func()) {
	defer func() {
		_ = recover()
	}()
	fn()
}
