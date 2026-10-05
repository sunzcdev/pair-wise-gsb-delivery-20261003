package ws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// gateBuffer is an in-memory writer that short-writes at most max bytes per
// Write call and parks inside every Write until the test allows it to
// proceed.
type gateBuffer struct {
	max int

	mu  sync.Mutex
	buf bytes.Buffer

	req   chan struct{}
	allow chan struct{}
}

func newGateBuffer(max int) *gateBuffer {
	return &gateBuffer{
		max:   max,
		req:   make(chan struct{}),
		allow: make(chan struct{}),
	}
}

func (g *gateBuffer) Write(p []byte) (int, error) {
	g.req <- struct{}{}
	<-g.allow
	if len(p) > g.max {
		p = p[:g.max]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

// step waits until a Write call is pending and allows it to proceed.
func (g *gateBuffer) step(t *testing.T) {
	t.Helper()
	select {
	case <-g.req:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for pending write")
	}
	g.allow <- struct{}{}
}

// park waits until a Write call is pending, but does not allow it.
func (g *gateBuffer) park(t *testing.T) {
	t.Helper()
	select {
	case <-g.req:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for pending write")
	}
}

func (g *gateBuffer) bytes() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]byte(nil), g.buf.Bytes()...)
}

func decodeFrames(t *testing.T, bts []byte) []Frame {
	t.Helper()
	var frames []Frame
	r := bytes.NewReader(bts)
	for r.Len() > 0 {
		f, err := ReadFrame(r)
		if err != nil {
			t.Fatalf("decode frame #%d: %s", len(frames), err)
		}
		frames = append(frames, f)
	}
	return frames
}

func checkFrame(t *testing.T, f Frame, op OpCode, fin bool, payload string) {
	t.Helper()
	if f.Header.OpCode != op {
		t.Errorf("frame opcode = %v; want %v", f.Header.OpCode, op)
	}
	if f.Header.Fin != fin {
		t.Errorf("frame %v fin = %v; want %v", op, f.Header.Fin, fin)
	}
	if string(f.Payload) != payload {
		t.Errorf("frame %v payload = %q; want %q", op, f.Payload, payload)
	}
}

func TestConcurrentWriterInterleaving(t *testing.T) {
	// Short writes of at most 3 bytes per call.
	g := newGateBuffer(3)
	cw := NewConcurrentWriter(g, 4)
	ctx := context.Background()

	msgErr := make(chan error, 1)
	go func() {
		msgErr <- cw.WriteMessage(ctx, OpText, []byte("0123456789"), 4)
	}()

	// First fragment: header (2) + payload (4) = 6 bytes, two short
	// writes of 3 bytes.
	g.step(t)
	g.step(t)

	// The flusher is now parked inside the first write of the second
	// fragment. The first message is in flight, so a second data
	// message enqueued now is guaranteed to stay queued until the
	// first one completes.
	g.park(t)
	msg2Err := make(chan error, 1)
	go func() {
		msg2Err <- cw.WriteMessage(ctx, OpBinary, []byte("AB"), 0)
	}()

	// Inject a ping control frame; it must cut in right after this
	// fragment, before the remaining one.
	pingRes := make(chan error, 1)
	cw.ctrl <- &cwriterCtrlReq{
		cwriterReq: cwriterReq{ctx: ctx, res: pingRes},
		f:          NewPingFrame(nil),
	}
	g.allow <- struct{}{} // second fragment, chunk 1
	g.step(t)             // second fragment, chunk 2
	g.step(t)             // ping frame (2 bytes)
	g.step(t)             // last fragment, chunk 1 (3 bytes)
	g.step(t)             // last fragment, chunk 2 (1 byte)

	if err := <-msgErr; err != nil {
		t.Fatalf("WriteMessage: %s", err)
	}
	if err := <-pingRes; err != nil {
		t.Fatalf("ping: %s", err)
	}

	// Second message: header (2) + payload (2) = 4 bytes, two writes.
	g.step(t)
	g.step(t)
	if err := <-msg2Err; err != nil {
		t.Fatalf("WriteMessage #2: %s", err)
	}

	if err := cw.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	frames := decodeFrames(t, g.bytes())
	if len(frames) != 5 {
		t.Fatalf("decoded %d frames; want 5", len(frames))
	}
	checkFrame(t, frames[0], OpText, false, "0123")
	checkFrame(t, frames[1], OpContinuation, false, "4567")
	checkFrame(t, frames[2], OpPing, true, "")
	checkFrame(t, frames[3], OpContinuation, true, "89")
	checkFrame(t, frames[4], OpBinary, true, "AB")
}

func TestConcurrentWriterCancelQueued(t *testing.T) {
	g := newGateBuffer(64)
	cw := NewConcurrentWriter(g, 4)
	ctx := context.Background()

	msgErr := make(chan error, 1)
	go func() {
		msgErr <- cw.WriteMessage(ctx, OpText, []byte("HELLOWORLD"), 5)
	}()

	g.step(t) // first fragment
	g.park(t) // flusher is parked inside the second fragment write

	msg2ctx, cancel := context.WithCancel(ctx)
	msg2Err := make(chan error, 1)
	go func() {
		msg2Err <- cw.WriteMessage(msg2ctx, OpText, []byte("bye"), 0)
	}()
	cancel()

	g.allow <- struct{}{} // second fragment

	if err := <-msgErr; err != nil {
		t.Fatalf("WriteMessage: %s", err)
	}
	if err := <-msg2Err; err != context.Canceled {
		t.Fatalf("cancelled WriteMessage = %v; want %v", err, context.Canceled)
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// The cancelled message must not appear on the wire at all.
	frames := decodeFrames(t, g.bytes())
	if len(frames) != 2 {
		t.Fatalf("decoded %d frames; want 2", len(frames))
	}
	checkFrame(t, frames[0], OpText, false, "HELLO")
	checkFrame(t, frames[1], OpContinuation, true, "WORLD")
}

func TestConcurrentWriterCancelInFlight(t *testing.T) {
	g := newGateBuffer(64)
	cw := NewConcurrentWriter(g, 4)

	ctx, cancel := context.WithCancel(context.Background())
	msgErr := make(chan error, 1)
	go func() {
		msgErr <- cw.WriteMessage(ctx, OpText, []byte("0123456789"), 4)
	}()

	g.step(t) // first fragment is on the wire

	// Cancellation after the first fragment has been sent must not
	// retract already written bytes: the message is completed to keep
	// the frame stream valid.
	cancel()

	g.step(t) // second fragment
	g.step(t) // last fragment

	if err := <-msgErr; err != nil {
		t.Fatalf("WriteMessage: %s", err)
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	frames := decodeFrames(t, g.bytes())
	if len(frames) != 3 {
		t.Fatalf("decoded %d frames; want 3", len(frames))
	}
	checkFrame(t, frames[0], OpText, false, "0123")
	checkFrame(t, frames[1], OpContinuation, false, "4567")
	checkFrame(t, frames[2], OpContinuation, true, "89")
}

func TestConcurrentWriterWriteErrorBroadcast(t *testing.T) {
	errWrite := errors.New("test write error")
	// Fail on the very first underlying write, so that no request can
	// complete before the error and all of them must observe it.
	w := &failAfterWriter{max: 0, err: errWrite}
	cw := NewConcurrentWriter(w, 2)
	ctx := context.Background()

	const waiters = 4
	errs := make(chan error, waiters)
	go func() { errs <- cw.WriteMessage(ctx, OpText, []byte("AAAA"), 2) }()
	go func() { errs <- cw.WriteMessage(ctx, OpBinary, []byte("BB"), 0) }()
	go func() { errs <- cw.WriteMessage(ctx, OpText, []byte("CC"), 0) }()
	go func() { errs <- cw.WriteControl(ctx, NewPingFrame(nil)) }()

	// The first write error must be broadcast to the in-flight message
	// and to all queued waiters.
	for i := 0; i < waiters; i++ {
		select {
		case err := <-errs:
			if err != errWrite {
				t.Errorf("waiter error = %v; want %v", err, errWrite)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for broadcast")
		}
	}

	// The error is sticky for subsequent calls.
	if err := cw.WriteMessage(ctx, OpText, []byte("x"), 0); err != errWrite {
		t.Errorf("WriteMessage after error = %v; want %v", err, errWrite)
	}
	if err := cw.WriteControl(ctx, NewPingFrame(nil)); err != errWrite {
		t.Errorf("WriteControl after error = %v; want %v", err, errWrite)
	}
	if err := cw.Err(); err != errWrite {
		t.Errorf("Err() = %v; want %v", err, errWrite)
	}
	if err := cw.Close(); err != errWrite {
		t.Errorf("Close() = %v; want %v", err, errWrite)
	}
}

type failAfterWriter struct {
	max   int
	err   error
	calls int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.max {
		return 0, w.err
	}
	return len(p), nil
}

func TestConcurrentWriterClose(t *testing.T) {
	g := newGateBuffer(64)
	cw := NewConcurrentWriter(g, 4)
	ctx := context.Background()

	msgErr := make(chan error, 1)
	go func() {
		msgErr <- cw.WriteMessage(ctx, OpText, []byte("HELLOWORLD"), 5)
	}()

	g.step(t) // first fragment of the in-flight message
	g.park(t) // flusher is parked inside the second fragment write

	// The first message is in flight, so this one stays queued.
	msg2Err := make(chan error, 1)
	go func() {
		msg2Err <- cw.WriteMessage(ctx, OpText, []byte("queued"), 0)
	}()

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- cw.Close()
	}()

	// Make sure Close has initiated the shutdown before the in-flight
	// message completes, so that the queued message is rejected instead
	// of being picked up.
	select {
	case <-cw.quit:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Close to initiate")
	}
	g.allow <- struct{}{} // in-flight message completes

	if err := <-msgErr; err != nil {
		t.Fatalf("in-flight WriteMessage: %s", err)
	}
	if err := <-msg2Err; err != ErrConcurrentWriterClosed {
		t.Fatalf("queued WriteMessage = %v; want %v", err, ErrConcurrentWriterClosed)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %s", err)
	}

	// After Close new data messages and control frames are rejected.
	if err := cw.WriteMessage(ctx, OpText, []byte("x"), 0); err != ErrConcurrentWriterClosed {
		t.Errorf("WriteMessage after Close = %v; want %v", err, ErrConcurrentWriterClosed)
	}
	if err := cw.WriteControl(ctx, NewPingFrame(nil)); err != ErrConcurrentWriterClosed {
		t.Errorf("WriteControl after Close = %v; want %v", err, ErrConcurrentWriterClosed)
	}

	frames := decodeFrames(t, g.bytes())
	if len(frames) != 2 {
		t.Fatalf("decoded %d frames; want 2", len(frames))
	}
	checkFrame(t, frames[0], OpText, false, "HELLO")
	checkFrame(t, frames[1], OpContinuation, true, "WORLD")
}

func TestConcurrentWriterCloseFrameRejectsData(t *testing.T) {
	g := newGateBuffer(64)
	cw := NewConcurrentWriter(g, 4)
	ctx := context.Background()

	msgErr := make(chan error, 1)
	go func() {
		msgErr <- cw.WriteMessage(ctx, OpText, []byte("0123456789"), 4)
	}()

	g.step(t) // first fragment
	g.park(t) // flusher is parked inside the second fragment write

	// Inject a close frame between the fragments.
	closeRes := make(chan error, 1)
	cw.ctrl <- &cwriterCtrlReq{
		cwriterReq: cwriterReq{ctx: ctx, res: closeRes},
		f:          NewCloseFrame(NewCloseFrameBody(StatusNormalClosure, "")),
	}
	g.allow <- struct{}{} // second fragment
	g.step(t)             // close frame

	// The remaining fragments of the in-flight message are abandoned;
	// already sent fragments stay on the wire.
	if err := <-msgErr; err != ErrConcurrentWriterClosed {
		t.Fatalf("WriteMessage = %v; want %v", err, ErrConcurrentWriterClosed)
	}
	if err := <-closeRes; err != nil {
		t.Fatalf("close frame: %s", err)
	}

	// New data messages are rejected, but control frames still work.
	if err := cw.WriteMessage(ctx, OpText, []byte("x"), 0); err != ErrConcurrentWriterClosed {
		t.Fatalf("WriteMessage after close frame = %v; want %v", err, ErrConcurrentWriterClosed)
	}
	pongErr := make(chan error, 1)
	go func() {
		pongErr <- cw.WriteControl(ctx, NewPongFrame(nil))
	}()
	g.step(t) // pong frame
	if err := <-pongErr; err != nil {
		t.Fatalf("pong after close frame: %s", err)
	}

	if err := cw.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	frames := decodeFrames(t, g.bytes())
	if len(frames) != 4 {
		t.Fatalf("decoded %d frames; want 4", len(frames))
	}
	checkFrame(t, frames[0], OpText, false, "0123")
	checkFrame(t, frames[1], OpContinuation, false, "4567")
	checkFrame(t, frames[2], OpClose, true, string(NewCloseFrameBody(StatusNormalClosure, "")))
	checkFrame(t, frames[3], OpPong, true, "")
}

func TestConcurrentWriterShortWriteZero(t *testing.T) {
	cw := NewConcurrentWriter(&zeroWriter{}, 1)
	err := cw.WriteMessage(context.Background(), OpText, []byte("x"), 0)
	if err != io.ErrShortWrite {
		t.Fatalf("WriteMessage = %v; want %v", err, io.ErrShortWrite)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) { return 0, nil }
