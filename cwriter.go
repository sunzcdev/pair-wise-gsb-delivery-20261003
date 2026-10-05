package ws

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Errors used by ConcurrentWriter.
var (
	// ErrConcurrentWriterClosed is returned when a write is attempted on a
	// ConcurrentWriter that is closed. A writer becomes closed after
	// Close() is called or after a close control frame has been written.
	ErrConcurrentWriterClosed = fmt.Errorf("ws: concurrent writer is closed")

	// ErrConcurrentWriterNotControl is returned by
	// ConcurrentWriter.WriteControl when given frame is not a valid
	// control frame.
	ErrConcurrentWriterNotControl = fmt.Errorf("ws: not a control frame")

	// ErrConcurrentWriterNotData is returned by
	// ConcurrentWriter.WriteMessage when given operation code is not a
	// data operation code.
	ErrConcurrentWriterNotData = fmt.Errorf("ws: not a data operation code")
)

// ConcurrentWriter wraps an io.Writer (usually a connection write end) and
// makes it safe for use by multiple goroutines.
//
// Data messages are written exclusively: fragments of a single message are
// never interleaved with fragments of another message. Control frames
// (ping, pong, close) may be injected between fragments of a data message,
// so a big fragmented message does not block control frames until it is
// fully written.
//
// The writer has a queue with explicit capacity for both data and control
// requests. Enqueueing blocks when the queue is full; the provided context
// is the cancellation entry point for that wait.
//
// Cancellation semantics: cancelling the context while a request is still
// waiting for a queue slot aborts the enqueue and nothing is written. Once
// a data message is accepted into the queue, cancelling its context drops
// the message entirely only if its first fragment has not been written yet.
// If at least one fragment has already been sent, the message is always
// written to completion to keep the frame stream valid; cancellation can
// not retract bytes that are already on the wire. In every case the caller
// receives the final result of the request.
//
// The first underlying write error is sticky: it is broadcast to all queued
// and in-flight requests, the writer stops, and all subsequent calls return
// that same error.
//
// After Close() is called, or after a close control frame has been written,
// new data messages are rejected with ErrConcurrentWriterClosed.
type ConcurrentWriter struct {
	w io.Writer

	data chan *cwriterDataReq
	ctrl chan *cwriterCtrlReq
	quit chan struct{}
	done chan struct{}

	mu         sync.Mutex
	err        error
	dataClosed bool
	closed     bool

	closeOnce sync.Once
}

// NewConcurrentWriter returns a ConcurrentWriter that writes frames to w.
// The queue argument sets the capacity of both the data message queue and
// the control frame queue. It must be greater than zero.
func NewConcurrentWriter(w io.Writer, queue int) *ConcurrentWriter {
	if queue <= 0 {
		panic(fmt.Sprintf("ws: NewConcurrentWriter with non-positive queue capacity %d", queue))
	}
	cw := &ConcurrentWriter{
		w:    w,
		data: make(chan *cwriterDataReq, queue),
		ctrl: make(chan *cwriterCtrlReq, queue),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go cw.run()
	return cw
}

type cwriterReq struct {
	ctx context.Context
	res chan error
}

type cwriterDataReq struct {
	cwriterReq
	op   OpCode
	p    []byte
	frag int
}

type cwriterCtrlReq struct {
	cwriterReq
	f Frame
}

// WriteMessage enqueues a data message with the given operation code (OpText
// or OpBinary) and payload, and blocks until the message is fully written
// or an error occurs.
//
// If fragment is positive and less than len(p), the message is split into
// fragments of at most fragment bytes, each sent as a separate frame.
// Control frames from concurrent WriteControl calls may be injected between
// those fragments. Fragments of other data messages are never interleaved.
//
// The ctx may be used to cancel the wait for a queue slot or to drop the
// message while it is still queued. Once the first fragment is written the
// message is always completed; see the ConcurrentWriter documentation for
// the exact cancellation semantics.
func (cw *ConcurrentWriter) WriteMessage(ctx context.Context, op OpCode, p []byte, fragment int) error {
	if !op.IsData() || op == OpContinuation {
		return ErrConcurrentWriterNotData
	}
	if err := cw.dataStateErr(); err != nil {
		return err
	}
	req := &cwriterDataReq{
		cwriterReq: cwriterReq{ctx: ctx, res: make(chan error, 1)},
		op:         op,
		p:          p,
		frag:       fragment,
	}
	select {
	case cw.data <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-cw.done:
		return cw.Err()
	}
	return cw.wait(&req.cwriterReq)
}

// WriteControl enqueues a control frame (ping, pong or close) and blocks
// until it is written or an error occurs. Control frames have priority over
// queued data messages and may be injected between fragments of a data
// message that is currently being written.
//
// Writing a close frame closes the writer for subsequent data messages:
// they are rejected with ErrConcurrentWriterClosed.
func (cw *ConcurrentWriter) WriteControl(ctx context.Context, f Frame) error {
	if !f.Header.OpCode.IsControl() || !f.Header.Fin ||
		f.Header.Length > MaxControlFramePayloadSize {
		return ErrConcurrentWriterNotControl
	}
	if err := cw.ctrlStateErr(); err != nil {
		return err
	}
	req := &cwriterCtrlReq{
		cwriterReq: cwriterReq{ctx: ctx, res: make(chan error, 1)},
		f:          f,
	}
	select {
	case cw.ctrl <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-cw.done:
		return cw.Err()
	}
	return cw.wait(&req.cwriterReq)
}

// Close closes the writer. New data messages are rejected with
// ErrConcurrentWriterClosed. A data message whose fragments are currently
// being written is completed; requests still sitting in the queues are
// failed with ErrConcurrentWriterClosed. Close blocks until the writer
// goroutine has stopped and returns the sticky write error, if any.
func (cw *ConcurrentWriter) Close() error {
	cw.closeOnce.Do(func() {
		cw.mu.Lock()
		cw.dataClosed = true
		cw.closed = true
		cw.mu.Unlock()
		close(cw.quit)
	})
	<-cw.done
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.err
}

// Err returns the sticky write error of the writer, if any. Once Err
// returns a non-nil error, all subsequent write calls return the same
// error.
func (cw *ConcurrentWriter) Err() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.err != nil {
		return cw.err
	}
	select {
	case <-cw.done:
		return ErrConcurrentWriterClosed
	default:
		return nil
	}
}

// dataStateErr reports whether the writer still accepts new data messages.
func (cw *ConcurrentWriter) dataStateErr() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.err != nil {
		return cw.err
	}
	if cw.dataClosed {
		return ErrConcurrentWriterClosed
	}
	return nil
}

// ctrlStateErr reports whether the writer still accepts new control frames.
func (cw *ConcurrentWriter) ctrlStateErr() error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if cw.err != nil {
		return cw.err
	}
	if cw.closed {
		return ErrConcurrentWriterClosed
	}
	return nil
}

// wait blocks until the request is served by the writer goroutine.
func (cw *ConcurrentWriter) wait(req *cwriterReq) error {
	select {
	case err := <-req.res:
		return err
	case <-cw.done:
		// The writer goroutine has stopped. If it served the request
		// before stopping, the result is already buffered in res.
		select {
		case err := <-req.res:
			return err
		default:
			return cw.Err()
		}
	}
}

func (cw *ConcurrentWriter) run() {
	defer close(cw.done)

	var cur *cwriterDataReq
	var off int

	for {
		if cur == nil {
			// Idle: pick up the next request. Quit has priority so
			// that Close() does not serve requests that are still
			// queued.
			select {
			case <-cw.quit:
				cw.shutdown()
				return
			default:
			}
			select {
			case c := <-cw.ctrl:
				cw.serveCtrl(c)
				if cw.isDataClosed() {
					// A close frame has been written: reject
					// data messages that are still queued.
					cw.failData(ErrConcurrentWriterClosed)
				}
			case d := <-cw.data:
				switch err := d.ctx.Err(); {
				case err != nil:
					// Cancelled while queued: drop the whole
					// message, nothing is written.
					d.res <- err
				case cw.isDataClosed():
					d.res <- ErrConcurrentWriterClosed
				default:
					cur, off = d, 0
				}
			case <-cw.quit:
				cw.shutdown()
				return
			}
			if err := cw.Err(); err != nil {
				cw.stop(err)
				return
			}
			continue
		}

		// A data message is in flight. Control frames may cut in
		// between its fragments.
		select {
		case c := <-cw.ctrl:
			cw.serveCtrl(c)
			if err := cw.Err(); err != nil {
				cur.res <- err
				cw.stop(err)
				return
			}
			if cw.isDataClosed() {
				// A close frame has been written in the middle
				// of the message. Already sent fragments stay
				// on the wire; the rest is abandoned.
				cur.res <- ErrConcurrentWriterClosed
				cur = nil
				cw.failData(ErrConcurrentWriterClosed)
			}
			continue
		default:
		}

		fin, err := cw.writeFragment(cur, off)
		if err != nil {
			cw.setErr(err)
			cur.res <- err
			cw.stop(err)
			return
		}
		if fin {
			cur.res <- nil
			cur = nil
		} else {
			off = nextFragmentOff(cur, off)
		}
	}
}

// serveCtrl writes a single control frame and reports the result.
func (cw *ConcurrentWriter) serveCtrl(c *cwriterCtrlReq) {
	bts, err := CompileFrame(c.f)
	if err == nil {
		err = writeAll(cw.w, bts)
	}
	if err != nil {
		cw.setErr(err)
	}
	if c.f.Header.OpCode == OpClose {
		cw.mu.Lock()
		cw.dataClosed = true
		cw.mu.Unlock()
	}
	c.res <- err
}

// writeFragment writes the fragment of req starting at offset off. It
// reports whether the written fragment was the last one of the message.
func (cw *ConcurrentWriter) writeFragment(req *cwriterDataReq, off int) (fin bool, err error) {
	end := len(req.p)
	if req.frag > 0 && off+req.frag < end {
		end = off + req.frag
	}
	fin = end == len(req.p)

	op := req.op
	if off > 0 {
		op = OpContinuation
	}
	bts, err := CompileFrame(NewFrame(op, fin, req.p[off:end]))
	if err != nil {
		return false, err
	}
	return fin, writeAll(cw.w, bts)
}

func nextFragmentOff(req *cwriterDataReq, off int) int {
	if req.frag <= 0 {
		return len(req.p)
	}
	return off + req.frag
}

// setErr records the first write error.
func (cw *ConcurrentWriter) setErr(err error) {
	cw.mu.Lock()
	if cw.err == nil {
		cw.err = err
	}
	cw.mu.Unlock()
}

func (cw *ConcurrentWriter) isDataClosed() bool {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.dataClosed
}

// stop broadcasts the sticky error to all queued requests and returns,
// stopping the writer goroutine.
func (cw *ConcurrentWriter) stop(err error) {
	cw.setErr(err)
	cw.failData(err)
	cw.failCtrl(err)
}

// shutdown rejects all queued requests after Close.
func (cw *ConcurrentWriter) shutdown() {
	cw.mu.Lock()
	cw.dataClosed = true
	cw.closed = true
	cw.mu.Unlock()
	cw.failData(ErrConcurrentWriterClosed)
	cw.failCtrl(ErrConcurrentWriterClosed)
}

func (cw *ConcurrentWriter) failData(err error) {
	for {
		select {
		case d := <-cw.data:
			d.res <- err
		default:
			return
		}
	}
}

func (cw *ConcurrentWriter) failCtrl(err error) {
	for {
		select {
		case c := <-cw.ctrl:
			c.res <- err
		default:
			return
		}
	}
}

// writeAll writes bts to w, looping over short writes. A short write that
// reports no error and no progress is turned into io.ErrShortWrite.
func writeAll(w io.Writer, bts []byte) error {
	for len(bts) > 0 {
		n, err := w.Write(bts)
		bts = bts[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
