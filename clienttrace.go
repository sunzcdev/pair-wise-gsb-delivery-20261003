package fasthttp

import (
	"net"
	"time"
)

// TracePhase identifies a single client-side phase of an HTTP request.
type TracePhase uint8

const (
	// TracePhaseConnWait measures the time spent acquiring a connection
	// from the HostClient connection pool, including waiting for a free
	// connection when the pool is exhausted.
	TracePhaseConnWait TracePhase = iota + 1

	// TracePhaseDNS measures host name resolution. It is only reported
	// when the built-in dialer performs an actual lookup; results served
	// from the DNS cache and custom Dial functions do not produce
	// a separate DNS phase.
	TracePhaseDNS

	// TracePhaseDial measures establishing the TCP connection.
	// It is never reported for connections reused from the pool.
	TracePhaseDial

	// TracePhaseTLS measures the TLS handshake. It is only reported when
	// the handshake is performed eagerly while establishing the connection
	// (i.e. HostClient.WriteTimeout is set). With a lazy handshake the TLS
	// time is included in TracePhaseWrite instead.
	TracePhaseTLS

	// TracePhaseWrite measures writing the request to the connection,
	// including flushing the buffered writer.
	TracePhaseWrite

	// TracePhaseFirstByte measures the time from the request being fully
	// written until the first response byte is read from the connection.
	TracePhaseFirstByte
)

// String returns a human-readable name of the phase.
func (p TracePhase) String() string {
	switch p {
	case TracePhaseConnWait:
		return "ConnWait"
	case TracePhaseDNS:
		return "DNS"
	case TracePhaseDial:
		return "Dial"
	case TracePhaseTLS:
		return "TLS"
	case TracePhaseWrite:
		return "Write"
	case TracePhaseFirstByte:
		return "FirstByte"
	}
	return "Unknown"
}

// TraceEvent describes a single completed phase of one request attempt.
//
// The event deliberately contains only values copied out of the request
// path. It never references Request or Response buffers, so it stays
// valid after they are released back to their pools and reused.
type TraceEvent struct {
	// Phase is the request phase this event describes.
	Phase TracePhase

	// Attempt is the 1-based retry attempt the phase belongs to.
	// Every retry produces its own set of events with a distinct Attempt.
	Attempt int

	// Start and End bound the phase. Duration reports End-Start.
	Start time.Time
	End   time.Time

	// Err is set if the phase failed. The request may still be retried
	// and eventually succeed; inspect Attempt to distinguish retries.
	Err error

	// Reused reports whether the connection came straight from the idle
	// pool. It is only meaningful for TracePhaseConnWait events.
	Reused bool
}

// Duration returns how long the phase took.
func (e TraceEvent) Duration() time.Duration {
	return e.End.Sub(e.Start)
}

// ClientTrace collects per-phase timing events for a single request.
//
// Attach it to a request with Request.SetTrace. Tracing is disabled by
// default and costs nothing (a couple of nil checks) when not attached.
//
// Events are emitted in chronological order from the goroutine running
// the request. Retries emit a full set of events per attempt, tagged with
// TraceEvent.Attempt.
//
// Only the default transport reports these events. Custom RoundTripper
// implementations and the pipelining client ignore the trace.
//
// Note: a fresh connection dialed in the background for a request waiting
// on an exhausted pool is attributed to that request's ConnWait phase
// only; Dial/TLS events are emitted only when the dial happens on the
// requesting goroutine.
type ClientTrace struct {
	// OnPhase is called once per completed phase, in chronological order.
	//
	// The callback runs on the goroutine executing the request, so it
	// must be fast and must not block.
	//
	// Observability must never break requests: a panic inside OnPhase is
	// recovered and ignored. It cannot change the request outcome, its
	// retry behavior, or leak the connection out of the pool.
	OnPhase func(TraceEvent)
}

// emit invokes OnPhase while shielding the request path from panics in
// user-supplied observability code.
func (t *ClientTrace) emit(ev TraceEvent) {
	defer func() {
		// A misbehaving observer must not affect connection management
		// or retry logic. Swallow the panic on purpose.
		_ = recover()
	}()
	t.OnPhase(ev)
}

// SetTrace attaches a ClientTrace to the request, enabling per-phase
// timing events for the next client call. Pass nil to disable tracing.
//
// The trace is not copied by Request.CopyTo and is cleared by
// Request.Reset, so requests obtained from AcquireRequest always start
// with tracing disabled.
func (req *Request) SetTrace(t *ClientTrace) {
	req.trace = t
}

// Trace returns the ClientTrace attached to the request, or nil.
func (req *Request) Trace() *ClientTrace {
	return req.trace
}

// clientTracer carries the trace and the current retry attempt through
// the request path. A nil *clientTracer is valid and all its methods
// are no-ops, keeping the untraced fast path free of branching logic.
type clientTracer struct {
	trace   *ClientTrace
	attempt int
}

// newClientTracer returns nil when tracing is disabled for the request.
func newClientTracer(req *Request) *clientTracer {
	t := req.trace
	if t == nil || t.OnPhase == nil {
		return nil
	}
	attempt := req.traceAttempt
	if attempt <= 0 {
		attempt = 1
	}
	return &clientTracer{trace: t, attempt: attempt}
}

// start returns the phase start time, or the zero time when tracing is
// disabled, so callers don't need to guard time.Now themselves.
func (t *clientTracer) start() time.Time {
	if t == nil {
		return time.Time{}
	}
	return time.Now()
}

// emit reports a completed phase. It is a no-op when tracing is disabled.
func (t *clientTracer) emit(phase TracePhase, start time.Time, err error) {
	if t == nil {
		return
	}
	t.trace.emit(TraceEvent{
		Phase:   phase,
		Attempt: t.attempt,
		Start:   start,
		End:     time.Now(),
		Err:     err,
	})
}

// emitConnWait reports the connection acquisition phase.
func (t *clientTracer) emitConnWait(start time.Time, reused bool, err error) {
	if t == nil {
		return
	}
	t.trace.emit(TraceEvent{
		Phase:   TracePhaseConnWait,
		Attempt: t.attempt,
		Start:   start,
		End:     time.Now(),
		Err:     err,
		Reused:  reused,
	})
}

// emitFirstByte reports the wait for the first response byte. end is the
// moment the first byte was read; a zero end means the read failed before
// any byte arrived, in which case err is reported with the event.
func (t *clientTracer) emitFirstByte(start, end time.Time, err error) {
	if t == nil {
		return
	}
	if end.IsZero() {
		end = time.Now()
	} else {
		err = nil
	}
	t.trace.emit(TraceEvent{
		Phase:   TracePhaseFirstByte,
		Attempt: t.attempt,
		Start:   start,
		End:     end,
		Err:     err,
	})
}

// firstByteConn records the moment the first byte is read from the
// connection. It is only used on the single goroutine running the
// request, so no synchronization is needed.
type firstByteConn struct {
	net.Conn
	firstByte time.Time
}

func (c *firstByteConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.firstByte.IsZero() {
		c.firstByte = time.Now()
	}
	return n, err
}
