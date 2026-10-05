package wsutil

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/gobwas/ws"
)

// ErrConcurrentWriterClosed is returned by ConcurrentWriter after Close or
// after the writer has stopped because of an underlying write error.
var ErrConcurrentWriterClosed = errors.New("wsutil: concurrent writer is closed")

// ConcurrentWriter serializes WebSocket writes from multiple goroutines.
//
// Data messages are sent in the same order in which WriteMessage accepted
// them. A data message is not interleaved with another data message, but ping
// and pong frames are written at fragment boundaries instead of waiting for a
// whole large message to finish.
//
// Context cancellation before a queued job is received by the worker prevents
// that job from being written. After a data message has started, cancellation
// is ignored and the complete message is sent; this avoids leaving a
// fragmented WebSocket message unfinished. A close frame is written at the
// nearest fragment boundary. New data and ping/pong writes are rejected once
// Close has been accepted.
type ConcurrentWriter struct {
	dest         io.Writer
	state        ws.State
	fragmentSize int

	dataJobs    chan concurrentDataJob
	controlJobs chan concurrentControlJob
	closeJobs   chan concurrentCloseJob
	done        chan struct{}
	enqueued    sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	stopped bool
	err     error
}

type concurrentDataJob struct {
	*concurrentJob
	op     ws.OpCode
	p      []byte
	result chan error
}

type concurrentControlJob struct {
	*concurrentJob
	op     ws.OpCode
	p      []byte
	result chan error
}

type concurrentJob struct {
	ctx      context.Context
	mu       sync.Mutex
	started  bool
	canceled bool
	begin    chan struct{}
}

func newConcurrentJob(ctx context.Context) concurrentJob {
	return concurrentJob{
		ctx:   ctx,
		begin: make(chan struct{}),
	}
}

func newConcurrentJobPtr(ctx context.Context) *concurrentJob {
	j := newConcurrentJob(ctx)
	return &j
}

func (j *concurrentJob) markStarted() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.canceled {
		return false
	}
	if !j.started {
		j.started = true
		close(j.begin)
	}
	return true
}

func (j *concurrentJob) cancel() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started {
		return false
	}
	j.canceled = true
	return true
}

type concurrentCloseJob struct {
	p      []byte
	result chan error
}

// NewConcurrentWriter returns a writer that serializes frames written to dest.
//
// queueSize is the capacity of both the data queue and the ping/pong queue.
// fragmentSize is the maximum payload size of one data fragment; non-positive
// values select DefaultWriteBuffer.
func NewConcurrentWriter(dest io.Writer, state ws.State, queueSize, fragmentSize int) *ConcurrentWriter {
	if queueSize <= 0 {
		panic("wsutil: concurrent writer queue size must be positive")
	}
	if fragmentSize <= 0 {
		fragmentSize = DefaultWriteBuffer
	}

	w := &ConcurrentWriter{
		dest:         dest,
		state:        state,
		fragmentSize: fragmentSize,
		dataJobs:     make(chan concurrentDataJob, queueSize),
		controlJobs:  make(chan concurrentControlJob, queueSize),
		closeJobs:    make(chan concurrentCloseJob, 1),
		done:         make(chan struct{}),
	}
	go w.loop()

	return w
}

// WriteMessage queues a complete text or binary message.
func (w *ConcurrentWriter) WriteMessage(ctx context.Context, op ws.OpCode, p []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if op != ws.OpText && op != ws.OpBinary {
		return ws.ProtocolError("wsutil: concurrent writer expects text or binary data opcode")
	}
	if err := w.checkTerminal(); err != nil {
		return err
	}

	job := concurrentDataJob{
		concurrentJob: newConcurrentJobPtr(ctx),
		op:            op,
		p:             p,
		result:        make(chan error, 1),
	}
	w.mu.Lock()
	if w.closed || w.stopped {
		err := w.terminalErrLocked()
		w.mu.Unlock()
		return err
	}
	w.enqueued.Add(1)
	w.mu.Unlock()

	var queued bool
	select {
	case w.dataJobs <- job:
		queued = true
	case <-ctx.Done():
	case <-w.done:
	}
	w.enqueued.Done()

	if queued {
		select {
		case err := <-job.result:
			return err
		case <-job.begin:
			return <-job.result
		case <-ctx.Done():
			if job.cancel() {
				return ctx.Err()
			}
			return <-job.result
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return w.checkTerminal()
	}
}

// WriteControl queues a ping or pong frame, or initiates a close handshake.
func (w *ConcurrentWriter) WriteControl(ctx context.Context, op ws.OpCode, p []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch op {
	case ws.OpPing, ws.OpPong:
		if len(p) > ws.MaxControlFramePayloadSize {
			return ErrControlOverflow
		}
		return w.writePingPong(ctx, op, p)
	case ws.OpClose:
		if len(p) > ws.MaxControlFramePayloadSize {
			return ErrControlOverflow
		}
		return w.writeClose(ctx, p)
	default:
		return ws.ProtocolError("wsutil: concurrent writer expects ping, pong or close opcode")
	}
}

// WriteClose writes a close frame at the next data-fragment boundary and marks
// the writer closed.
func (w *ConcurrentWriter) WriteClose(ctx context.Context, p []byte) error {
	return w.WriteControl(ctx, ws.OpClose, p)
}

func (w *ConcurrentWriter) writePingPong(ctx context.Context, op ws.OpCode, p []byte) error {
	if err := w.checkTerminal(); err != nil {
		return err
	}

	job := concurrentControlJob{
		concurrentJob: newConcurrentJobPtr(ctx),
		op:            op,
		p:             p,
		result:        make(chan error, 1),
	}
	w.mu.Lock()
	if w.closed || w.stopped {
		err := w.terminalErrLocked()
		w.mu.Unlock()
		return err
	}
	w.enqueued.Add(1)
	w.mu.Unlock()

	var queued bool
	select {
	case w.controlJobs <- job:
		queued = true
	case <-ctx.Done():
	case <-w.done:
	}
	w.enqueued.Done()

	if queued {
		select {
		case err := <-job.result:
			return err
		case <-job.begin:
			return <-job.result
		case <-ctx.Done():
			if job.cancel() {
				return ctx.Err()
			}
			return <-job.result
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return w.checkTerminal()
	}
}

func (w *ConcurrentWriter) writeClose(ctx context.Context, p []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	w.mu.Lock()
	if w.closed || w.stopped {
		err := w.terminalErrLocked()
		w.mu.Unlock()
		return err
	}
	w.closed = true
	w.mu.Unlock()

	job := concurrentCloseJob{
		p:      p,
		result: make(chan error, 1),
	}
	w.closeJobs <- job

	return <-job.result
}

func (w *ConcurrentWriter) checkTerminal() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed && !w.stopped {
		return nil
	}
	return w.terminalErrLocked()
}

func (w *ConcurrentWriter) terminalErrLocked() error {
	if w.err != nil {
		return w.err
	}
	return ErrConcurrentWriterClosed
}

func (w *ConcurrentWriter) loop() {
	for {
		if job, ok := w.nextDataJob(); ok {
			if w.writeData(job) {
				return
			}
			continue
		}
		return
	}
}

func (w *ConcurrentWriter) nextDataJob() (concurrentDataJob, bool) {
	for {
		if w.drainReadyControls() {
			return concurrentDataJob{}, false
		}
		if job, ok := w.readyClose(); ok {
			w.finishClose(job, nil)
			return concurrentDataJob{}, false
		}
		if len(w.dataJobs) > 0 {
			select {
			case job := <-w.dataJobs:
				return job, true
			default:
			}
		}

		select {
		case job := <-w.dataJobs:
			return job, true
		case job := <-w.controlJobs:
			if w.finishControl(job) {
				return concurrentDataJob{}, false
			}
		case job := <-w.closeJobs:
			w.finishClose(job, nil)
			return concurrentDataJob{}, false
		}
	}
}

func (w *ConcurrentWriter) writeData(job concurrentDataJob) (stopped bool) {
	if !job.markStarted() {
		job.result <- job.ctx.Err()
		return false
	}

	for pos := 0; pos < len(job.p); {
		end := pos + w.fragmentSize
		if end > len(job.p) {
			end = len(job.p)
		}

		op := ws.OpContinuation
		if pos == 0 {
			op = job.op
		}
		if err := w.writeFrame(op, pos+w.fragmentSize >= len(job.p), job.p[pos:end]); err != nil {
			job.result <- err
			w.shutdown(err)
			return true
		}
		pos = end

		if pos == len(job.p) {
			job.result <- nil
			return false
		}
		if w.drainBoundaryControls(&job) {
			return true
		}
	}

	// An empty message is still one final data frame.
	if err := w.writeFrame(job.op, true, nil); err != nil {
		job.result <- err
		w.shutdown(err)
		return true
	}
	job.result <- nil
	return false
}

func (w *ConcurrentWriter) drainReadyControls() bool {
	for {
		select {
		case job := <-w.controlJobs:
			if w.finishControl(job) {
				return true
			}
		default:
			return false
		}
	}
}

func (w *ConcurrentWriter) drainBoundaryControls(active *concurrentDataJob) bool {
	for {
		select {
		case job := <-w.controlJobs:
			if w.finishControl(job) {
				return true
			}
		case job := <-w.closeJobs:
			w.finishClose(job, active)
			return true
		default:
			return false
		}
	}
}

func (w *ConcurrentWriter) finishControl(job concurrentControlJob) bool {
	if !job.markStarted() {
		job.result <- job.ctx.Err()
		return false
	}
	err := w.writeFrame(job.op, true, job.p)
	job.result <- err
	if err != nil {
		w.shutdown(err)
		return true
	}
	return false
}

func (w *ConcurrentWriter) finishClose(job concurrentCloseJob, active *concurrentDataJob) {
	err := w.writeFrame(ws.OpClose, true, job.p)
	job.result <- err
	if active != nil {
		if err != nil {
			active.result <- err
		} else {
			active.result <- ErrConcurrentWriterClosed
		}
	}
	if err != nil {
		w.shutdown(err)
		return
	}
	w.shutdown(ErrConcurrentWriterClosed)
}

func (w *ConcurrentWriter) shutdown(err error) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.err = err
	w.mu.Unlock()

	close(w.done)
	w.drainQueuedJobs(err)
	w.enqueued.Wait()
	w.drainQueuedJobs(err)
}

func (w *ConcurrentWriter) readyClose() (concurrentCloseJob, bool) {
	select {
	case job := <-w.closeJobs:
		return job, true
	default:
		return concurrentCloseJob{}, false
	}
}

func (w *ConcurrentWriter) drainQueuedJobs(err error) {
	for {
		select {
		case job := <-w.dataJobs:
			job.result <- err
			continue
		case job := <-w.controlJobs:
			job.result <- err
			continue
		case job := <-w.closeJobs:
			job.result <- err
			continue
		default:
			return
		}
	}
}

func (w *ConcurrentWriter) writeFrame(op ws.OpCode, fin bool, p []byte) error {
	frame := ws.Frame{
		Header: ws.Header{
			Fin:    fin,
			OpCode: op,
			Length: int64(len(p)),
		},
		Payload: p,
	}
	if w.state.ClientSide() {
		frame = ws.MaskFrame(frame)
	}
	return writeFull(w.dest, ws.MustCompileFrame(frame))
}

func writeFull(dest io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := dest.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
