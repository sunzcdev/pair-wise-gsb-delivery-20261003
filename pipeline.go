package fasthttp

import (
	"bufio"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// pipelineState coordinates bounded concurrent processing of pipelined
// requests on a single connection.
//
// The serve loop reads requests and hands eligible ones to the pipeline:
// every request gets its own RequestCtx and is processed by its own
// goroutine, while a single writer goroutine writes the responses back
// in request order. The number of in-flight requests is bounded by
// slots, so responses completed out of order are buffered in bounded
// quantity only.
type pipelineState struct {
	// queue hands tasks from the serve loop to the writer goroutine in
	// request order. It is closed by the serve loop once no more
	// requests follow.
	queue chan *pipelineTask

	// slots bounds the number of in-flight (received but not yet
	// written) requests.
	slots chan struct{}

	// done is closed when the connection is torn down. It signals
	// still running handlers that they should abort; see
	// RequestCtx.Done.
	done     chan struct{}
	doneOnce sync.Once

	// wg tracks the writer goroutine.
	wg sync.WaitGroup

	// hijack is set by the writer when a pipelined handler hijacked
	// the connection. The serve loop performs the actual handoff.
	hijack atomic.Pointer[pipelineTask]

	// writeErr is set by the writer goroutine before done is closed,
	// so readers synchronized via done or wg don't need further
	// synchronization.
	writeErr error
}

func newPipelineState(maxConcurrency int) *pipelineState {
	return &pipelineState{
		queue: make(chan *pipelineTask, maxConcurrency),
		slots: make(chan struct{}, maxConcurrency),
		done:  make(chan struct{}),
	}
}

// signalDone signals connection teardown to all still running pipelined
// handlers. It is safe to call multiple times.
func (pl *pipelineState) signalDone() {
	pl.doneOnce.Do(func() {
		close(pl.done)
	})
}

// terminated reports whether the writer goroutine stopped serving the
// connection (write error, 'Connection: close' response or hijack).
func (pl *pipelineState) terminated() bool {
	select {
	case <-pl.done:
		return true
	default:
		return false
	}
}

// barrier blocks until all pipelined responses queued so far have been
// written and flushed to the connection. It is used to form a serial
// boundary for requests that can't be processed concurrently.
func (pl *pipelineState) barrier() {
	barrier := make(chan struct{})
	pl.queue <- &pipelineTask{barrierCh: barrier}
	<-barrier
}

// pipelineTask is a single request processed concurrently under
// Server.MaxPipelinedConcurrency.
type pipelineTask struct {
	ctx      *RequestCtx
	finished chan struct{}

	// Inputs captured by the serve loop before the task is started.
	writeTimeout time.Duration
	serverName   string
	isHTTP11     bool
	reqSecond    int64

	// Results produced by the handler goroutine before finished is
	// closed.
	hijackHandler    HijackHandler
	hijackNoResponse bool
	connectionClose  bool

	// barrierCh is non-nil for barrier tasks only: the writer closes
	// it once all previously queued responses are written and flushed.
	barrierCh chan struct{}
}

// isPipelinableRequest reports whether the fully received request in ctx
// may be processed concurrently with other pipelined requests on the
// same connection.
func (s *Server) isPipelinableRequest(ctx *RequestCtx) bool {
	req := &ctx.Request
	if req.bodyStream != nil {
		// Streamed request bodies are read from the connection on
		// demand and therefore can't be concurrent with other requests.
		return false
	}
	h := &req.Header
	if cl := h.ContentLength(); cl != 0 && cl != -2 {
		// Requests with a body form a serial boundary.
		// ContentLength is 0 or -2 (identity) for requests without a
		// body, -1 for chunked transfer encoding.
		return false
	}
	if h.ConnectionUpgrade() || len(h.Peek(HeaderUpgrade)) > 0 {
		// Connection upgrades (e.g. WebSocket) take over the
		// connection and can't be concurrent with other requests.
		return false
	}
	if req.MayContinue() {
		// 'Expect: 100-continue' interleaves an interim response.
		return false
	}
	if allow := s.AllowPipelinedRequest; allow != nil {
		return allow(ctx)
	}
	return ctx.IsGet() || ctx.IsHead()
}

// runPipelinedTask runs the server handler for a pipelined request and
// prepares its response for the writer goroutine.
func (s *Server) runPipelinedTask(t *pipelineTask) {
	ctx := t.ctx
	s.Handler(ctx)

	if timeoutResponse := ctx.timeoutResponse; timeoutResponse != nil {
		// Acquire a new ctx because the old one is still in use by the
		// timed out handler, mirroring the serial serve loop. The old
		// ctx is intentionally not recycled.
		nctx := s.acquireCtx(ctx.c)
		nctx.connTime = ctx.connTime
		nctx.time = ctx.time
		nctx.connID = ctx.connID
		nctx.connRequestNum = ctx.connRequestNum
		timeoutResponse.CopyTo(&nctx.Response)
		t.ctx = nctx
		ctx = nctx
	}

	if ctx.IsHead() {
		ctx.Response.SkipBody = true
	}

	t.hijackHandler = ctx.hijackHandler
	ctx.hijackHandler = nil
	t.hijackNoResponse = ctx.hijackNoResponse && t.hijackHandler != nil
	ctx.hijackNoResponse = false

	t.connectionClose = s.finalizeResponse(ctx, t.serverName, t.isHTTP11, false)
	close(t.finished)
}

// pipelineWriter writes the responses of pipelined requests to the
// connection in request order. It runs in its own goroutine and is the
// only writer to the connection while pipelined responses are pending;
// the serve loop regains write access via pipelineState.barrier.
func (s *Server) pipelineWriter(c net.Conn, pl *pipelineState, idleConnTime *atomic.Int64) {
	defer pl.wg.Done()

	var (
		bw                   *bufio.Writer
		previousWriteTimeout time.Duration
		terminated           bool
	)
	terminate := func(err error) {
		if terminated {
			return
		}
		terminated = true
		if err != nil {
			pl.writeErr = err
		}
		// Tell still running handlers that the connection is going away.
		pl.signalDone()
		// Wake the serve loop in case it is blocked reading the next
		// request, so it notices the termination.
		c.SetReadDeadline(time.Now()) //nolint:errcheck
	}

	for t := range pl.queue {
		if t.barrierCh != nil {
			// Serial boundary: make sure everything written so far is
			// flushed before the serve loop continues.
			if !terminated && bw != nil && bw.Buffered() > 0 {
				if err := bw.Flush(); err != nil {
					terminate(err)
				}
			}
			close(t.barrierCh)
			continue
		}

		<-t.finished

		if terminated {
			// The connection is gone; discard the response but still
			// wait for the handler so its ctx isn't recycled early.
			s.releaseCtx(t.ctx)
			<-pl.slots
			continue
		}

		ctx := t.ctx
		if t.writeTimeout > 0 {
			if err := c.SetWriteDeadline(time.Now().Add(t.writeTimeout)); err != nil {
				terminate(err)
				s.releaseCtx(ctx)
				<-pl.slots
				continue
			}
			previousWriteTimeout = t.writeTimeout
		} else if previousWriteTimeout > 0 {
			// We don't want a write timeout but we previously set
			// one, remove it.
			if err := c.SetWriteDeadline(zeroTime); err != nil {
				terminate(err)
				s.releaseCtx(ctx)
				<-pl.slots
				continue
			}
			previousWriteTimeout = 0
		}

		if !t.hijackNoResponse {
			if bw == nil {
				bw = acquireWriter(ctx)
			}
			if err := writeResponse(ctx, bw); err != nil {
				terminate(err)
				s.releaseCtx(ctx)
				<-pl.slots
				continue
			}
			// Flush when the client is waiting for this response or
			// the connection is about to close; otherwise batch
			// pipelined responses into fewer packets.
			if t.connectionClose || len(pl.queue) == 0 {
				if err := bw.Flush(); err != nil {
					terminate(err)
					s.releaseCtx(ctx)
					<-pl.slots
					continue
				}
			}
		}

		idleConnTime.Store(t.reqSecond)
		s.setState(c, StateIdle)

		if t.connectionClose {
			terminate(nil)
			s.releaseCtx(ctx)
			<-pl.slots
			continue
		}

		if t.hijackHandler != nil {
			if bw != nil && bw.Buffered() > 0 {
				if err := bw.Flush(); err != nil {
					terminate(err)
					s.releaseCtx(ctx)
					<-pl.slots
					continue
				}
			}
			// The serve loop performs the actual hijack handoff; wake
			// it up in case it is blocked reading the next request.
			pl.hijack.Store(t)
			terminate(nil)
			<-pl.slots
			continue
		}

		s.releaseCtx(ctx)
		<-pl.slots

		if s.ReduceMemoryUsage && len(pl.queue) == 0 && bw != nil {
			releaseWriter(s, bw)
			bw = nil
		}
	}

	if bw != nil {
		if !terminated && bw.Buffered() > 0 {
			bw.Flush() //nolint:errcheck
		}
		releaseWriter(s, bw)
	}
}

// finalizeResponse applies the connection-level response headers
// ('Connection: close' / keep-alive and Server) and reports whether the
// connection must be closed after this response.
func (s *Server) finalizeResponse(ctx *RequestCtx, serverName string, isHTTP11, connectionClose bool) bool {
	connectionClose = connectionClose ||
		ctx.Response.Header.ConnectionClose() ||
		(s.CloseOnShutdown && s.stop.Load() == 1)
	if connectionClose {
		ctx.Response.Header.SetConnectionClose()
	} else if !isHTTP11 {
		// Set 'Connection: keep-alive' response header for HTTP/1.0 request.
		// There is no need in setting this header for http/1.1, since in http/1.1
		// connections are keep-alive by default.
		ctx.Response.Header.setNonSpecial(strConnection, strKeepAlive)
	}
	if !isHTTP11 {
		// HTTP/1.0 clients can't read chunked encoding, so let
		// Response.Write keep Content-Length framing for them.
		ctx.Response.Header.noHTTP11 = true
	}

	if serverName != "" && len(ctx.Response.Header.Server()) == 0 {
		ctx.Response.Header.SetServer(serverName)
	}
	return connectionClose
}

// shutdownPipeline tears down pipelined processing at the end of
// serveConn: it signals connection teardown to all still running
// handlers and waits for the writer to flush all remaining responses in
// request order. A hijack requested by a pipelined handler is performed
// here, after all earlier responses have been written.
func (s *Server) shutdownPipeline(c net.Conn, pl *pipelineState, br *bufio.Reader, err error) (*bufio.Reader, error) {
	pl.signalDone()
	close(pl.queue)
	pl.wg.Wait()
	if err == nil && pl.writeErr != nil {
		err = pl.writeErr
	}
	if t := pl.hijack.Load(); t != nil {
		// All responses before the hijacked request have been written
		// and all requests after it have been discarded by the writer.
		if derr := c.SetDeadline(zeroTime); derr == nil {
			var hjr io.Reader = c
			if br != nil {
				hjr = br
				br = nil
			}
			go hijackConnHandler(t.ctx, hjr, c, s, t.hijackHandler)
			err = errHijacked
		} else {
			s.releaseCtx(t.ctx)
		}
	}
	return br, err
}
