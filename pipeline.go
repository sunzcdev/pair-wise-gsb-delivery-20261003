package fasthttp

import (
	"bufio"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/valyala/bytebufferpool"
)

// serveConnWithPipelining is the entry point used when PipelineParallelism is
// enabled for the connection. Requests are read sequentially on a single
// connection, but the Handler of explicitly approved, bodyless GET/HEAD
// requests may run concurrently. Responses are always written back in the
// order the requests arrived.
//
// The method owns every pooled resource used on the connection (ctx, reader,
// writer) and releases them exactly once on return.
func (s *Server) serveConnWithPipelining(c net.Conn, connTime time.Time, countConcurrency bool) error {
	maxBody := s.MaxRequestBodySize
	if maxBody <= 0 {
		maxBody = DefaultMaxRequestBodySize
	}
	p := &pipeline{
		s:          s,
		c:          c,
		connTime:   connTime,
		serverName: s.getServerName(),
		connID:     nextConnID(),
		maxBody:    maxBody,
		parallel:   s.PipelineParallelism,
		done:       make(chan struct{}),
		results:    make(chan *pipelineResult, s.PipelineParallelism),
		pending:    make(map[uint64]*pipelineResult),
	}
	return p.serve()
}

// pipeline carries all per-connection state of the parallel pipelining loop.
// There is a single pipeline value per connection and only the coordinator
// goroutine touches its mutable fields.
type pipeline struct {
	s          *Server
	c          net.Conn
	connTime   time.Time
	serverName string
	connID     uint64
	maxBody    int
	parallel   int

	idleConnTime *atomic.Int64

	// br/bw are owned by the coordinator goroutine.
	br *bufio.Reader
	bw *bufio.Writer

	// done is closed when the connection is being torn down. Every in-flight
	// parallel handler receives it through RequestCtx.Done().
	done chan struct{}

	// results delivers fully serialized responses from workers. It is bounded
	// by the configured parallelism, so buffered responses cannot grow
	// without limit.
	results chan *pipelineResult

	// inflight counts dispatched-but-not-yet-written requests. It never
	// exceeds parallel.
	inflight int

	// readSeq is the sequence number of the next request to read; writeSeq is
	// the sequence number of the next response that must hit the wire.
	readSeq  uint64
	writeSeq uint64

	// pending buffers out-of-order results keyed by sequence number. Its
	// length is bounded by inflight (<= parallel).
	pending map[uint64]*pipelineResult

	// stop is set after a fatal condition (write error, read error,
	// Connection: close, server shutdown) so no new requests are dispatched.
	stop bool

	// writeTimeout is the per-request response write timeout, mirroring the
	// HeaderReceived driven value used in the serial loop.
	writeTimeout time.Duration
	// previousWriteTimeout remembers a previously set deadline so it can be
	// cleared when a later request does not ask for one.
	previousWriteTimeout time.Duration

	// Reader pump. Exactly one goroutine ever calls c.Read; the coordinator
	// obtains bytes through readRes so an idle wait stays interruptible by
	// finished in-flight responses, connection teardown and server shutdown.
	// The pump is created lazily when the coordinator first needs to wait for
	// data, and is stopped when the connection ends.
	readReq chan readerRequest
	readRes chan readerResult

	// pumpBuf/pumpStart/pumpLen hold bytes read ahead by the pump goroutine
	// but not yet consumed by the coordinator's reader. They are touched only
	// by the coordinator (in pumpReader.Read), so no extra synchronization is
	// needed.
	pumpBuf   []byte
	pumpStart int
	pumpLen   int
}

// readerRequest asks the pump to read into a buffer.
type readerRequest struct {
	p []byte
}

// readerResult is the pump's reply to a readerRequest.
type readerResult struct {
	n   int
	err error
}

// pipelineResult is a fully serialized response produced by a worker.
type pipelineResult struct {
	seq uint64

	// buf holds the wire bytes of the response and is returned to a pool
	// immediately after the response has been flushed.
	buf *bytebufferpool.ByteBuffer

	// closeConn requests a serial boundary: no further requests are
	// processed after this response.
	closeConn bool

	// writeTimeout is the per-request response write deadline.
	writeTimeout time.Duration

	// reqSecond is the request timestamp used for idle bookkeeping.
	reqSecond int64
}

var pipelineBufPool bytebufferpool.Pool

func (p *pipeline) cleanup() {
	// Stop the reader pump before releasing the shared reader, otherwise a
	// blocked pump Read outlives the connection.
	if p.readReq != nil {
		close(p.readReq)
		p.readReq = nil
	}
	if p.br != nil {
		releaseReader(p.s, p.br)
	}
	if p.bw != nil {
		releaseWriter(p.s, p.bw)
	}
	p.s.unregisterIdleConn(p.c)
}

// closeOnce signals every in-flight worker that the connection is gone.
func (p *pipeline) closeOnce() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

func (p *pipeline) serve() error {
	p.idleConnTime = p.s.registerIdleConn(p.c, p.connTime)
	defer p.cleanup()

	// Requests and responses share a 1-based sequence: the first request read
	// gets seq 1, so the first response written must be seq 1 as well.
	p.writeSeq = 1

	for {
		if p.stop {
			return p.drainAndFinish()
		}

		p.readSeq++
		reqNum := p.readSeq

		ctx, err := p.readNextRequest(reqNum)
		if err != nil {
			return p.handleReadError(ctx, err, reqNum)
		}

		if p.canParallel(ctx) {
			// Enforce the bound on in-flight work (and hence on buffered
			// responses). When the limit is reached, wait for the in-order
			// result before dispatching, which also applies read backpressure.
			if p.inflight >= p.parallel {
				if werr := p.flushReady(true); werr != nil {
					p.s.releaseCtx(ctx)
					return werr
				}
			}
			p.dispatch(ctx, reqNum)
			if werr := p.flushReady(false); werr != nil {
				return werr
			}
			continue
		}

		// Serial boundary: every earlier response must be on the wire before
		// this request runs, so the response sequence cannot be reordered.
		if werr := p.flushReady(true); werr != nil {
			p.s.releaseCtx(ctx)
			return werr
		}

		hijack, serr := p.runSerial(ctx, reqNum)
		if hijack {
			// The connection is owned by the hijack handler now.
			p.br = nil
			p.bw = nil
			return errHijacked
		}
		if serr != nil {
			return serr
		}
	}
}

// drainAndFinish waits for every in-flight worker, writes their responses in
// order and flushes. It is used on Connection: close / shutdown / EOF
// boundaries so an already announced response sequence is not truncated
// mid-stream.
func (p *pipeline) drainAndFinish() error {
	p.closeOnce()
	for p.inflight > 0 || len(p.pending) > 0 {
		// Emit any buffered contiguous results first.
		if res := p.pending[p.writeSeq]; res != nil {
			delete(p.pending, p.writeSeq)
			if werr := p.writeResult(res); werr != nil {
				p.releaseResult(res)
				return werr
			}
			p.releaseResult(res)
			p.writeSeq++
			continue
		}
		res := <-p.results
		{
			p.inflight--
			if res.seq == p.writeSeq {
				if werr := p.writeResult(res); werr != nil {
					p.releaseResult(res)
					return werr
				}
				p.releaseResult(res)
				p.writeSeq++
			} else {
				p.storeResult(res)
			}
		}
	}
	if p.bw != nil {
		if err := p.bw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// storeResult buffers an out-of-order result keyed by its sequence number.
func (p *pipeline) storeResult(res *pipelineResult) {
	p.pending[res.seq] = res
}

// absorbResult accounts for a worker result that was not immediately written
// and buffers it for ordered emission.
func (p *pipeline) absorbResult(res *pipelineResult) {
	p.inflight--
	p.storeResult(res)
}

// acquireWriter obtains a buffered writer bound to the connection from the
// server's writer pool without needing a ctx.
func (p *pipeline) acquireWriter() *bufio.Writer {
	v := p.s.writerPool.Get()
	if v == nil {
		n := p.s.WriteBufferSize
		if n <= 0 {
			n = defaultWriteBufferSize
		}
		return bufio.NewWriterSize(p.c, n)
	}
	w := v.(*bufio.Writer) //nolint:forcetypeassert
	w.Reset(p.c)
	return w
}

// releaseResult returns a result's wire buffer to the pool.
func (p *pipeline) releaseResult(res *pipelineResult) {
	if res.buf != nil {
		pipelineBufPool.Put(res.buf)
		res.buf = nil
	}
}

// readNextRequest reads and fully parses the next request, returning a fresh
// ctx. While blocked on the first byte of an idle keep-alive connection it
// keeps flushing in-flight responses, so a finished earlier response is never
// delayed by a later request's read.
//
// On a header/body parse error the returned ctx is still valid (it owns the
// partially parsed request) so the caller can render an error response. On
// errors occurring before a ctx existed it is nil.
func (p *pipeline) readNextRequest(reqNum uint64) (*RequestCtx, error) {
	ctx := p.s.acquireCtx(p.c)
	ctx.connTime = p.connTime
	ctx.Request.isTLS = ctx.IsTLS()
	ctx.pipelineDone = p.done

	if reqNum == 1 {
		if p.s.ReadTimeout > 0 {
			if err := p.c.SetReadDeadline(time.Now().Add(p.s.ReadTimeout)); err != nil {
				p.s.releaseCtx(ctx)
				return nil, err
			}
		}
	} else if d := p.s.idleTimeout(); d > 0 {
		if err := p.c.SetReadDeadline(time.Now().Add(d)); err != nil {
			p.s.releaseCtx(ctx)
			return nil, err
		}
	}

	p.configureCtx(ctx)
	p.idleConnTime.Store(0)
	p.s.setState(p.c, StateActive)

	if err := p.readHeaders(ctx, reqNum); err != nil {
		return ctx, err
	}

	if p.s.HeaderReceived != nil {
		if err := p.applyHeaderReceived(ctx); err != nil {
			p.s.releaseCtx(ctx)
			return nil, err
		}
	}

	if err := ctx.Request.parseURI(); err != nil {
		return ctx, err
	}

	if err := ctx.Request.readLimitBody(p.br, p.maxBody, p.s.GetOnly, !p.s.DisablePreParseMultipartForm); err != nil {
		return ctx, err
	}

	return ctx, nil
}

// applyHeaderReceived invokes the HeaderReceived hook and applies its
// per-request read timeout, body size limit and write timeout.
func (p *pipeline) applyHeaderReceived(ctx *RequestCtx) error {
	reqConf := p.s.HeaderReceived(&ctx.Request.Header)
	if reqConf.ReadTimeout > 0 {
		deadline := time.Now().Add(reqConf.ReadTimeout)
		if err := p.c.SetReadDeadline(deadline); err != nil {
			return err
		}
	}
	switch {
	case reqConf.MaxRequestBodySize > 0:
		p.maxBody = reqConf.MaxRequestBodySize
	case p.s.MaxRequestBodySize > 0:
		p.maxBody = p.s.MaxRequestBodySize
	default:
		p.maxBody = DefaultMaxRequestBodySize
	}
	if reqConf.WriteTimeout > 0 {
		p.writeTimeout = reqConf.WriteTimeout
	} else {
		p.writeTimeout = p.s.WriteTimeout
	}
	return nil
}

// configureCtx applies server-wide per-request header/response settings.
func (p *pipeline) configureCtx(ctx *RequestCtx) {
	ctx.Response.Header.noDefaultContentType = p.s.NoDefaultContentType
	ctx.Response.Header.noDefaultDate = p.s.NoDefaultDate
	ctx.Request.Header.secureErrorLogMessage = p.s.SecureErrorLogMessage
	ctx.Response.Header.secureErrorLogMessage = p.s.SecureErrorLogMessage
	ctx.Request.secureErrorLogMessage = p.s.SecureErrorLogMessage
	ctx.Response.secureErrorLogMessage = p.s.SecureErrorLogMessage
	if p.s.DisableHeaderNamesNormalizing {
		ctx.Request.Header.DisableNormalizing()
		ctx.Response.Header.DisableNormalizing()
	}
}

// readHeaders reads just the request headers. It performs the same
// read/flush dance as the serial loop but the underlying reader is backed by
// the interruptible pump, so an idle wait keeps in-flight responses flowing.
func (p *pipeline) readHeaders(ctx *RequestCtx, reqNum uint64) error {
	if p.br == nil {
		p.ensurePump()
		p.br = acquireReader(ctx)
		p.br.Reset(&pumpReader{p: p})
	}

	// First request: block until headers arrive under the read deadline.
	// Keep-alive: one non-blocking attempt while responses are buffered, then
	// flush and block — the pump keeps in-flight responses flowing meanwhile.
	if reqNum > 1 && p.bw != nil && p.bw.Buffered() > 0 {
		if err := ctx.Request.Header.readLoop(p.br, false); err != nil {
			if err != ErrNeedMore {
				return err
			}
			if ferr := p.bw.Flush(); ferr != nil {
				return ferr
			}
			if err := ctx.Request.Header.Read(p.br); err != nil {
				return err
			}
		}
	} else if err := ctx.Request.Header.Read(p.br); err != nil {
		return err
	}
	return nil
}

// ensurePump starts the single reader goroutine for this connection.
func (p *pipeline) ensurePump() {
	if p.readReq != nil {
		return
	}
	p.readReq = make(chan readerRequest)
	p.readRes = make(chan readerResult)
	go func() {
		for req := range p.readReq {
			n, err := p.c.Read(req.p)
			select {
			case p.readRes <- readerResult{n: n, err: err}:
			case <-p.done:
				return
			}
		}
	}()
}

// pumpReader routes Read calls through the pump, but while waiting it also
// services finished in-flight responses and watches teardown / shutdown, so an
// idle read never blocks an earlier response from being written.
type pumpReader struct {
	p *pipeline
}

// prependReader serves already-buffered bytes before falling through to the
// raw connection. It is used only when handing a hijacked connection over.
type prependReader struct {
	prefix []byte
	next   io.Reader
}

func (r *prependReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.next.Read(p)
}

func (r *pumpReader) Read(buf []byte) (int, error) {
	pp := r.p
	// Serve bytes the pump already read ahead first.
	if pp.pumpStart < pp.pumpLen {
		n := copy(buf, pp.pumpBuf[pp.pumpStart:pp.pumpLen])
		pp.pumpStart += n
		return n, nil
	}

	// Nothing buffered. While waiting for new socket bytes, keep writing any
	// responses that finish in order; a response must never be held up behind
	// the next request's read.
	if cap(pp.pumpBuf) == 0 {
		size := pp.s.ReadBufferSize
		if size <= 0 {
			size = defaultReadBufferSize
		}
		pp.pumpBuf = make([]byte, size)
	}
	pp.pumpBuf = pp.pumpBuf[:cap(pp.pumpBuf)]
	pp.readReq <- readerRequest{p: pp.pumpBuf}

	for {
		// A close response already written in order means no further request
		// must be read; surface EOF so the coordinator drains and tears down.
		if pp.stop {
			return 0, io.EOF
		}
		// Emit anything that is already in order (non-blocking).
		if err := pp.flushReady(false); err != nil {
			return 0, err
		}

		var sd <-chan struct{}
		if ptr := pp.s.done.Load(); ptr != nil {
			sd = *ptr
		}
		select {
		case res := <-pp.readRes:
			if res.err != nil {
				return res.n, res.err
			}
			if res.n == 0 {
				return 0, io.ErrUnexpectedEOF
			}
			pp.pumpStart = 0
			pp.pumpLen = res.n
			n := copy(buf, pp.pumpBuf[:res.n])
			pp.pumpStart = n
			return n, nil
		case pr := <-pp.results:
			pp.absorbResult(pr)
			if pr.closeConn {
				pp.stop = true
			}
			// The result may now be contiguous; flush in-order responses
			// before continuing to wait for socket bytes.
			if err := pp.flushReady(false); err != nil {
				return 0, err
			}
			if pp.stop {
				return 0, io.EOF
			}
		case <-pp.done:
			return 0, io.EOF
		case <-sd:
			pp.stop = true
			return 0, io.EOF
		}
	}
}

// canParallel reports whether the request is safe to process concurrently
// with other pipelined requests. The framework rejects any request that could
// change framing or touch the shared connection outside of a normal response:
// non-GET/HEAD methods, request bodies, chunked/identity framing, upgrades and
// 100-continue. The application's AllowPipelineRequest hook is the final,
// explicit opt-in.
func (p *pipeline) canParallel(ctx *RequestCtx) bool {
	h := &ctx.Request.Header

	if !h.IsHTTP11() {
		return false
	}
	if !h.IsGet() && !h.IsHead() {
		return false
	}
	// A framed or chunked body makes concurrent handling unsafe. For GET/HEAD
	// a missing Content-Length with no Transfer-Encoding (-2, identity) is the
	// normal bodyless framing; readLimitBody normalizes it to 0. For any other
	// method -2 means a close-delimited body and is unsafe.
	cl := h.ContentLength()
	if cl == -1 || (cl != 0 && !(cl == -2 && (h.IsGet() || h.IsHead()))) {
		return false
	}
	if h.ConnectionUpgrade() || ctx.Request.MayContinue() {
		return false
	}
	if p.s.AllowPipelineRequest == nil || !p.s.AllowPipelineRequest(h) {
		return false
	}
	return true
}

// dispatch starts a worker goroutine that runs the Handler and serializes the
// response. inflight is guaranteed to be below the bound by the caller: when
// the bound is reached the coordinator blocks in flush(true) before reading a
// further request, so the number of buffered responses is bounded too.
func (p *pipeline) dispatch(ctx *RequestCtx, seq uint64) {
	p.inflight++

	if p.serverName != "" {
		ctx.Response.Header.SetServer(p.serverName)
	}
	ctx.connID = p.connID
	ctx.connRequestNum = seq

	var reqTime time.Time
	var reqSecond int64
	if p.s.LazyRequestTime {
		reqSecond = coarseSecond()
	} else {
		reqTime = time.Now()
		reqSecond = reqTime.Unix()
	}
	ctx.time = reqTime

	writeTimeout := p.writeTimeout

	go func() {
		res := p.runParallel(ctx, seq, reqSecond, writeTimeout)
		// ctx ownership is consumed inside runParallel (either released back
		// to the pool or abandoned as a timed-out ctx).
		// Always deliver the result: the coordinator drains every in-flight
		// response in order, even while the connection is being torn down. The
		// results channel is buffered by the parallelism bound, so this cannot
		// deadlock.
		p.results <- res
	}()
}

// runParallel executes the handler for a single approved request, performs
// the same response framing decisions as the serial loop and serializes the
// response into an independent byte buffer. It must never touch the shared
// connection reader/writer.
func (p *pipeline) runParallel(ctx *RequestCtx, seq uint64, reqSecond int64, writeTimeout time.Duration) *pipelineResult {
	res := &pipelineResult{seq: seq, reqSecond: reqSecond}

	p.s.Handler(ctx)

	// TimeoutHandler-style timeout: the original ctx remains owned by the
	// (possibly still running) handler and must not be reused. Copy the frozen
	// response onto a fresh ctx, mirroring the serial loop.
	if tr := ctx.timeoutResponse; tr != nil {
		old := ctx
		ctx = p.s.acquireCtx(p.c)
		ctx.connTime = old.connTime
		ctx.time = old.time
		ctx.pipelineDone = p.done
		tr.CopyTo(&ctx.Response)
		// Deliberately do not release old: it is still referenced by the
		// timed-out handler, exactly like the serial code path.
	}

	if ctx.IsHead() {
		ctx.Response.SkipBody = true
	}

	// A parallel handler must not hijack or stream a body: both bypass the
	// ordered, buffered response path. Treat it as a fatal serial-boundary
	// error rather than silently corrupting the response order.
	unsafe := ctx.hijackHandler != nil || ctx.Response.bodyStream != nil
	ctx.hijackHandler = nil
	ctx.hijackNoResponse = false

	closeConn := p.s.DisableKeepalive ||
		ctx.Request.Header.ConnectionClose() ||
		(p.s.MaxRequestsPerConn > 0 && seq >= uint64(p.s.MaxRequestsPerConn)) || // #nosec G115
		ctx.Response.Header.ConnectionClose() ||
		(p.s.CloseOnShutdown && p.s.stop.Load() == 1) ||
		unsafe

	isHTTP11 := ctx.Request.Header.IsHTTP11()
	if closeConn {
		ctx.Response.Header.SetConnectionClose()
	} else if !isHTTP11 {
		ctx.Response.Header.setNonSpecial(strConnection, strKeepAlive)
	}
	if !isHTTP11 {
		ctx.Response.Header.noHTTP11 = true
	}
	if p.serverName != "" && len(ctx.Response.Header.Server()) == 0 {
		ctx.Response.Header.SetServer(p.serverName)
	}

	buf := pipelineBufPool.Get()
	buf.B = buf.B[:0]
	bufSize := p.s.WriteBufferSize
	if bufSize <= 0 {
		bufSize = defaultWriteBufferSize
	}
	bw := bufio.NewWriterSize(buf, bufSize)

	if unsafe {
		// Render a deterministic 500 for an unsupported parallel response.
		ctx.Response.Reset()
		ctx.Response.SetStatusCode(StatusInternalServerError)
		ctx.Response.SetBodyString("fasthttp: hijack and body streaming are not supported for parallel pipelined requests")
		ctx.Response.Header.SetConnectionClose()
	}

	if writeTimeout > 0 {
		res.writeTimeout = writeTimeout
	}

	if err := writeResponse(ctx, bw); err != nil {
		// Serialization into a memory buffer should not fail; on failure emit
		// a minimal error response.
		buf.B = buf.B[:0]
		bw.Reset(buf)
		p.s.writeFastError(bw, StatusInternalServerError, "failed to serialize response")
		closeConn = true
	}
	if err := bw.Flush(); err != nil {
		closeConn = true
	}

	res.buf = buf
	res.closeConn = closeConn

	// The request body is fully buffered for parallel requests and there is no
	// bodyStream to release. Reset and recycle the ctx.
	p.s.releaseCtx(ctx)

	return res
}

// flushReady consumes results and writes any that are contiguous starting at
// writeSeq. When wait is true it blocks until the result for writeSeq is
// available (forming a serial boundary); otherwise it returns once the next
// expected result is not yet ready.
func (p *pipeline) flushReady(wait bool) error {
	for {
		res := p.pending[p.writeSeq]
		if res == nil {
			// No buffered result for the next position. Obtain one either
			// non-blockingly (opportunistic flush) or blocking until the
			// in-order result arrives (serial boundary).
			r, got := p.receiveResult(wait)
			if !got {
				return nil
			}
			if r.seq != p.writeSeq {
				p.storeResult(r)
				if r.closeConn {
					p.stop = true
				}
				// On a non-blocking flush an out-of-order result means the
				// in-order response is not ready yet.
				if !wait {
					return nil
				}
				// Blocking boundary: keep draining until it shows up.
				continue
			}
			res = r
		} else {
			delete(p.pending, p.writeSeq)
		}

		if werr := p.writeResult(res); werr != nil {
			p.releaseResult(res)
			return werr
		}
		closeConn := res.closeConn
		p.releaseResult(res)
		p.writeSeq++

		if closeConn {
			p.stop = true
			return nil
		}

		// Opportunistic flushes keep draining while contiguous results are
		// already buffered or immediately available.
	}
}

// receiveResult fetches one worker result. When wait is false it is
// non-blocking and returns got=false if no result is ready. When wait is true
// it blocks, returning got=false only if there is no in-flight work to wait
// for.
func (p *pipeline) receiveResult(wait bool) (res *pipelineResult, got bool) {
	if !wait {
		select {
		case r := <-p.results:
			p.inflight--
			return r, true
		default:
			return nil, false
		}
	}
	if p.inflight == 0 {
		return nil, false
	}
	r := <-p.results
	p.inflight--
	return r, true
}

// writeResult writes one serialized response to the connection in order and
// applies the per-request write deadline.
func (p *pipeline) writeResult(res *pipelineResult) error {
	if p.bw == nil {
		p.bw = p.acquireWriter()
	}

	if res.writeTimeout > 0 {
		if err := p.c.SetWriteDeadline(time.Now().Add(res.writeTimeout)); err != nil {
			return err
		}
		p.previousWriteTimeout = res.writeTimeout
	} else if p.previousWriteTimeout > 0 {
		if err := p.c.SetWriteDeadline(zeroTime); err != nil {
			return err
		}
		p.previousWriteTimeout = 0
	}

	if _, err := p.bw.Write(res.buf.B); err != nil {
		return err
	}

	// Flush promptly: this is the in-order response the client is waiting for.
	if err := p.bw.Flush(); err != nil {
		return err
	}
	delete(p.pending, res.seq)

	if p.idleConnTime != nil {
		p.idleConnTime.Store(res.reqSecond)
	}
	p.s.setState(p.c, StateIdle)
	return nil
}

// runSerial processes one request on the coordinator goroutine. It is used for
// every request that is not safe to run in parallel (a body, an upgrade,
// Expect: 100-continue, a rejected opt-in, ...). All earlier parallel results
// have already been flushed in order by the caller, so this request runs
// exactly at its position in the response sequence.
//
// It mirrors the serial loop's behavior for Expect handling, the Handler
// invocation, response framing, writing, hijacking and per-request timeout.
func (p *pipeline) runSerial(ctx *RequestCtx, seq uint64) (hijack bool, err error) {
	var (
		continueReadingRequest = true
		connectionClose        bool
	)

	// 'Expect: 100-continue' request handling.
	if ctx.Request.MayContinue() {
		if p.s.ExpectHandler != nil {
			if expectStatus := p.s.ExpectHandler(ctx); expectStatus != StatusContinue {
				continueReadingRequest = false
				if p.br != nil {
					p.br.Reset(&pumpReader{p: p})
				}
				ctx.SetStatusCode(expectStatus)
				connectionClose = true
			}
		} else if p.s.ContinueHandler != nil {
			if continueReadingRequest = p.s.ContinueHandler(&ctx.Request.Header); !continueReadingRequest {
				if p.br != nil {
					p.br.Reset(&pumpReader{p: p})
				}
				ctx.SetStatusCode(StatusExpectationFailed)
			}
		}

		if continueReadingRequest {
			if p.bw == nil {
				p.bw = acquireWriter(ctx)
			}
			if _, err = p.bw.Write(strResponseContinue); err != nil {
				return false, err
			}
			if err = p.bw.Flush(); err != nil {
				return false, err
			}
			if err = ctx.Request.ContinueReadBody(p.br, p.maxBody, !p.s.DisablePreParseMultipartForm); err != nil {
				p.bw = p.s.writeErrorResponse(p.bw, ctx, p.serverName, err)
				if p.bw != nil {
					err = p.bw.Flush()
				}
				p.releaseSerialCtx(ctx)
				return false, err
			}
		}
	}

	connectionClose = connectionClose || p.s.DisableKeepalive || ctx.Request.Header.ConnectionClose()
	isHTTP11 := ctx.Request.Header.IsHTTP11()

	if p.serverName != "" {
		ctx.Response.Header.SetServer(p.serverName)
	}
	ctx.connID = p.connID
	ctx.connRequestNum = seq
	var reqTime time.Time
	var reqSecond int64
	if p.s.LazyRequestTime {
		reqSecond = coarseSecond()
	} else {
		reqTime = time.Now()
		reqSecond = reqTime.Unix()
	}
	ctx.time = reqTime

	if continueReadingRequest {
		p.s.Handler(ctx)
	}

	timeoutResponse := ctx.timeoutResponse
	if timeoutResponse != nil {
		ctx = p.acquireSerialCtx(ctx)
		ctx.time = reqTime
		timeoutResponse.CopyTo(&ctx.Response)
	}

	if ctx.IsHead() {
		ctx.Response.SkipBody = true
	}

	hijackHandler := ctx.hijackHandler
	ctx.hijackHandler = nil
	hijackNoResponse := ctx.hijackNoResponse && hijackHandler != nil
	ctx.hijackNoResponse = false

	if p.writeTimeout > 0 {
		if err = p.c.SetWriteDeadline(time.Now().Add(p.writeTimeout)); err != nil {
			p.releaseSerialCtx(ctx)
			return false, err
		}
		p.previousWriteTimeout = p.writeTimeout
	} else if p.previousWriteTimeout > 0 {
		if err = p.c.SetWriteDeadline(zeroTime); err != nil {
			p.releaseSerialCtx(ctx)
			return false, err
		}
		p.previousWriteTimeout = 0
	}

	connectionClose = connectionClose ||
		(p.s.MaxRequestsPerConn > 0 && seq >= uint64(p.s.MaxRequestsPerConn)) || // #nosec G115
		ctx.Response.Header.ConnectionClose() ||
		(p.s.CloseOnShutdown && p.s.stop.Load() == 1)
	if connectionClose {
		ctx.Response.Header.SetConnectionClose()
	} else if !isHTTP11 {
		ctx.Response.Header.setNonSpecial(strConnection, strKeepAlive)
	}
	if !isHTTP11 {
		ctx.Response.Header.noHTTP11 = true
	}
	if p.serverName != "" && len(ctx.Response.Header.Server()) == 0 {
		ctx.Response.Header.SetServer(p.serverName)
	}

	if !hijackNoResponse {
		if p.bw == nil {
			p.bw = acquireWriter(ctx)
		}
		if err = writeResponse(ctx, p.bw); err != nil {
			p.releaseSerialCtx(ctx)
			return false, err
		}
		// In-order serial response: flush so the client does not wait behind a
		// later request's read.
		if err = p.bw.Flush(); err != nil {
			p.releaseSerialCtx(ctx)
			return false, err
		}
		// The serial response occupies this sequence position in order.
		p.writeSeq++
		if connectionClose {
			p.releaseSerialCtx(ctx)
			p.stop = true
			return false, nil
		}
	}

	if hijackHandler != nil {
		// Hijack hands the raw connection to the handler. Stop the reader pump
		// and reset the reader to the direct connection so no goroutine keeps
		// reading behind the hijacker's back.
		var hjr io.Reader = p.c
		if p.br != nil {
			// Buffered bytes already read for this request belong to the
			// hijacked stream; prepend them before the raw connection.
			if n := p.br.Buffered(); n > 0 {
				buf, _ := p.br.Peek(n)
				hjr = &prependReader{prefix: append([]byte(nil), buf...), next: p.c}
			}
			releaseReader(p.s, p.br)
			p.br = nil
		}
		if p.readReq != nil {
			close(p.readReq)
			p.readReq = nil
		}
		if p.bw != nil {
			if err = p.bw.Flush(); err != nil {
				p.releaseSerialCtx(ctx)
				return false, err
			}
			releaseWriter(p.s, p.bw)
			p.bw = nil
		}
		if err = p.c.SetDeadline(zeroTime); err != nil {
			p.releaseSerialCtx(ctx)
			return false, err
		}
		go hijackConnHandler(ctx, hjr, p.c, p.s, hijackHandler)
		return true, errHijacked
	}

	if ctx.Request.bodyStream != nil {
		if rs, ok := ctx.Request.bodyStream.(*requestStream); ok {
			releaseRequestStream(rs)
		}
		ctx.Request.bodyStream = nil
	}

	if p.idleConnTime != nil {
		p.idleConnTime.Store(reqSecond)
	}
	p.s.setState(p.c, StateIdle)
	ctx.Request.Reset()
	ctx.Response.Reset()
	p.releaseSerialCtx(ctx)

	// p.br is the long-lived pump-backed reader; keep it (and any prefetched
	// pipelined bytes) for the next request.

	if p.s.stop.Load() == 1 {
		p.stop = true
	}
	return false, nil
}

// releaseSerialCtx releases a ctx used on the serial path.
func (p *pipeline) releaseSerialCtx(ctx *RequestCtx) {
	if ctx != nil {
		p.s.releaseCtx(ctx)
	}
}

// acquireSerialCtx obtains a fresh ctx carrying over connection metadata,
// used when a serial request timed out (its old ctx stays with the handler).
func (p *pipeline) acquireSerialCtx(old *RequestCtx) *RequestCtx {
	ctx := p.s.acquireCtx(p.c)
	ctx.connTime = old.connTime
	ctx.Request.isTLS = old.Request.isTLS
	ctx.pipelineDone = p.done
	return ctx
}

// handleReadError maps a read error to terminal behavior, mirroring the serial
// loop: clean EOF / nothing-read on keep-alive ends the connection, while a
// protocol error renders an error response on the partially parsed ctx.
func (p *pipeline) handleReadError(ctx *RequestCtx, err error, reqNum uint64) error {
	if err == io.EOF {
		if ctx != nil {
			p.s.releaseCtx(ctx)
		}
		return p.drainAndFinish()
	}
	if nr, ok := err.(ErrNothingRead); ok {
		if ctx != nil {
			p.s.releaseCtx(ctx)
		}
		if reqNum > 1 {
			return p.drainAndFinish()
		}
		err = nr.error
		// A first-request failure before any header: fatal connection error.
		return err
	}

	// A real protocol/body error with a parsed request: send an error response.
	if ctx == nil {
		return err
	}
	// Make sure earlier in-flight responses are written first; ordering must be
	// preserved even on error.
	if werr := p.flushReady(true); werr != nil {
		p.s.releaseCtx(ctx)
		return werr
	}
	p.bw = p.s.writeErrorResponse(p.bw, ctx, p.serverName, err)
	p.s.releaseCtx(ctx)
	p.stop = true
	if p.bw != nil {
		if ferr := p.bw.Flush(); ferr != nil {
			return ferr
		}
	}
	return err
}
