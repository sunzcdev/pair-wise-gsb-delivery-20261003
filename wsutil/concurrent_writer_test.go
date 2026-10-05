package wsutil

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
)

var errConcurrentTestWrite = errors.New("concurrent writer test: write failed")

type shortMemoryWriter struct {
	mu           sync.Mutex
	buf          bytes.Buffer
	max          int
	fail         bool
	frameEnd     int
	frameCount   int
	nextFrameLen int
	blockAt      int
	started      chan struct{}
	release      chan struct{}
	once         sync.Once
}

func newShortMemoryWriter(max int) *shortMemoryWriter {
	return &shortMemoryWriter{
		max:     max,
		blockAt: -1,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *shortMemoryWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.blockAt == 0 && w.frameCount == 0 {
		w.once.Do(func() { close(w.started) })
		<-w.release
	}

	if w.fail {
		return 0, errConcurrentTestWrite
	}

	n := len(p)
	if n > w.max {
		n = w.max
	}
	written, err := w.buf.Write(p[:n])
	if err != nil {
		return written, err
	}
	for {
		if w.nextFrameLen == 0 {
			if w.buf.Len()-w.frameEnd < 2 {
				break
			}
			header := w.buf.Bytes()[w.frameEnd:]
			switch {
			case header[1] <= 125:
				w.nextFrameLen = int(header[1]) + 2
			case header[1] == 126 && w.buf.Len()-w.frameEnd >= 4:
				w.nextFrameLen = (int(header[2])<<8 | int(header[3])) + 4
			case header[1] == 127 && w.buf.Len()-w.frameEnd >= 10:
				w.nextFrameLen = (int(header[8])<<8 | int(header[9])) + 10
			default:
				return written, nil
			}
		}
		if w.buf.Len() < w.frameEnd+w.nextFrameLen {
			break
		}
		w.frameEnd += w.nextFrameLen
		w.nextFrameLen = 0
		w.frameCount++
		if w.blockAt > 0 && w.frameCount == w.blockAt {
			w.once.Do(func() { close(w.started) })
			<-w.release
		}
	}
	return written, nil
}

func (w *shortMemoryWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Bytes()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if cond() {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func TestConcurrentWriterShortWrites(t *testing.T) {
	dest := newShortMemoryWriter(1)
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 4, 2)

	if err := cw.WriteMessage(context.Background(), ws.OpText, []byte("abcdefg")); err != nil {
		t.Fatalf("write message: %v", err)
	}

	got := frames(t, dest.bytes())
	want := []ws.Frame{
		{Header: ws.Header{OpCode: ws.OpText, Length: 2}, Payload: []byte("ab")},
		{Header: ws.Header{OpCode: ws.OpContinuation, Length: 2}, Payload: []byte("cd")},
		{Header: ws.Header{OpCode: ws.OpContinuation, Length: 2}, Payload: []byte("ef")},
		{Header: ws.Header{Fin: true, OpCode: ws.OpContinuation, Length: 1}, Payload: []byte("g")},
	}
	assertFrames(t, got, want)
}

func TestConcurrentWriterPingBetweenFragments(t *testing.T) {
	dest := newShortMemoryWriter(2)
	dest.blockAt = 1
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 4, 2)

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- cw.WriteMessage(context.Background(), ws.OpText, []byte("abcdef"))
	}()

	<-dest.started
	pingErr := make(chan error, 1)
	go func() {
		pingErr <- cw.WriteControl(context.Background(), ws.OpPing, []byte("hi"))
	}()
	waitFor(t, func() bool { return len(cw.controlJobs) == 1 })
	close(dest.release)
	if err := <-pingErr; err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write message: %v", err)
	}

	got := frames(t, dest.bytes())
	want := []ws.Frame{
		{Header: ws.Header{OpCode: ws.OpText, Length: 2}, Payload: []byte("ab")},
		{Header: ws.Header{Fin: true, OpCode: ws.OpPing, Length: 2}, Payload: []byte("hi")},
		{Header: ws.Header{OpCode: ws.OpContinuation, Length: 2}, Payload: []byte("cd")},
		{Header: ws.Header{Fin: true, OpCode: ws.OpContinuation, Length: 2}, Payload: []byte("ef")},
	}
	assertFrames(t, got, want)
}

func TestConcurrentWriterPongBetweenFragments(t *testing.T) {
	dest := newShortMemoryWriter(2)
	dest.blockAt = 1
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 4, 2)

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- cw.WriteMessage(context.Background(), ws.OpBinary, []byte("abcd"))
	}()

	<-dest.started
	pongErr := make(chan error, 1)
	go func() {
		pongErr <- cw.WriteControl(context.Background(), ws.OpPong, []byte("ok"))
	}()
	waitFor(t, func() bool { return len(cw.controlJobs) == 1 })
	close(dest.release)
	if err := <-pongErr; err != nil {
		t.Fatalf("write pong: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write message: %v", err)
	}

	got := frames(t, dest.bytes())
	want := []ws.Frame{
		{Header: ws.Header{OpCode: ws.OpBinary, Length: 2}, Payload: []byte("ab")},
		{Header: ws.Header{Fin: true, OpCode: ws.OpPong, Length: 2}, Payload: []byte("ok")},
		{Header: ws.Header{Fin: true, OpCode: ws.OpContinuation, Length: 2}, Payload: []byte("cd")},
	}
	assertFrames(t, got, want)
}

func TestConcurrentWriterQueueCapacityAndCancel(t *testing.T) {
	dest := newShortMemoryWriter(16)
	dest.blockAt = 0
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 2, 16)

	activeErr := make(chan error, 1)
	go func() {
		activeErr <- cw.WriteMessage(context.Background(), ws.OpText, []byte("active"))
	}()
	<-dest.started

	queuedErrs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			queuedErrs <- cw.WriteMessage(context.Background(), ws.OpText, []byte("queued"))
		}()
	}
	waitFor(t, func() bool { return len(cw.dataJobs) == 2 })

	ctx, cancel := context.WithCancel(context.Background())
	blockedErr := make(chan error, 1)
	go func() {
		blockedErr <- cw.WriteMessage(ctx, ws.OpText, []byte("overflow"))
	}()
	runtime.Gosched()
	if len(cw.dataJobs) != 2 {
		t.Fatalf("queue length after overflow attempt: %d; want 2", len(cw.dataJobs))
	}
	cancel()
	if err := <-blockedErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("overflow cancellation: %v; want %v", err, context.Canceled)
	}

	close(dest.release)
	if err := <-activeErr; err != nil {
		t.Fatalf("active write: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-queuedErrs; err != nil {
			t.Fatalf("queued write: %v", err)
		}
	}
}

func TestConcurrentWriterCancelQueuedMessage(t *testing.T) {
	dest := newShortMemoryWriter(16)
	dest.blockAt = 0
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 1, 16)

	activeErr := make(chan error, 1)
	go func() {
		activeErr <- cw.WriteMessage(context.Background(), ws.OpText, []byte("first"))
	}()
	<-dest.started

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := cw.WriteMessage(canceled, ws.OpBinary, []byte("canceled"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error: %v; want %v", err, context.Canceled)
	}

	close(dest.release)
	if err := <-activeErr; err != nil {
		t.Fatalf("active write: %v", err)
	}

	got := frames(t, dest.bytes())
	want := []ws.Frame{
		{Header: ws.Header{Fin: true, OpCode: ws.OpText, Length: 5}, Payload: []byte("first")},
	}
	assertFrames(t, got, want)
}

func TestConcurrentWriterBroadcastFirstWriteError(t *testing.T) {
	dest := newShortMemoryWriter(16)
	dest.blockAt = 0
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 2, 16)

	errs := make(chan error, 3)
	go func() { errs <- cw.WriteMessage(context.Background(), ws.OpText, []byte("one")) }()
	<-dest.started
	go func() { errs <- cw.WriteMessage(context.Background(), ws.OpText, []byte("two")) }()
	go func() { errs <- cw.WriteMessage(context.Background(), ws.OpBinary, []byte("three")) }()

	waitFor(t, func() bool { return len(cw.dataJobs) == 2 })
	dest.fail = true
	close(dest.release)

	for i := 0; i < 3; i++ {
		if err := <-errs; !errors.Is(err, errConcurrentTestWrite) {
			t.Fatalf("queued writer got %v; want %v", err, errConcurrentTestWrite)
		}
	}
	if err := cw.WriteMessage(context.Background(), ws.OpText, []byte("after")); !errors.Is(err, errConcurrentTestWrite) {
		t.Fatalf("write after failure: %v; want %v", err, errConcurrentTestWrite)
	}
}

func TestConcurrentWriterCloseAtFragmentBoundary(t *testing.T) {
	dest := newShortMemoryWriter(2)
	dest.blockAt = 1
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 4, 2)
	closeBody := ws.NewCloseFrameBody(ws.StatusNormalClosure, "")

	dataErr := make(chan error, 1)
	go func() {
		dataErr <- cw.WriteMessage(context.Background(), ws.OpText, []byte("abcdef"))
	}()
	<-dest.started

	closeErr := make(chan error, 1)
	go func() { closeErr <- cw.WriteClose(context.Background(), closeBody) }()
	waitFor(t, func() bool { return len(cw.closeJobs) == 1 })

	if err := cw.WriteMessage(context.Background(), ws.OpBinary, []byte("new")); !errors.Is(err, ErrConcurrentWriterClosed) {
		t.Fatalf("new data after close: %v; want %v", err, ErrConcurrentWriterClosed)
	}
	if err := cw.WriteControl(context.Background(), ws.OpPing, nil); !errors.Is(err, ErrConcurrentWriterClosed) {
		t.Fatalf("ping after close: %v; want %v", err, ErrConcurrentWriterClosed)
	}

	close(dest.release)
	if err := <-closeErr; err != nil {
		t.Fatalf("write close: %v", err)
	}
	if err := <-dataErr; !errors.Is(err, ErrConcurrentWriterClosed) {
		t.Fatalf("interrupted data: %v; want %v", err, ErrConcurrentWriterClosed)
	}

	got := frames(t, dest.bytes())
	want := []ws.Frame{
		{Header: ws.Header{OpCode: ws.OpText, Length: 2}, Payload: []byte("ab")},
		{Header: ws.Header{Fin: true, OpCode: ws.OpClose, Length: 2}, Payload: closeBody},
	}
	assertFrames(t, got, want)
}

func TestConcurrentWriterCloseRejectsDuplicateControl(t *testing.T) {
	dest := newShortMemoryWriter(16)
	cw := NewConcurrentWriter(dest, ws.StateServerSide, 4, 16)

	if err := cw.WriteClose(context.Background(), nil); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := cw.WriteClose(context.Background(), nil); !errors.Is(err, ErrConcurrentWriterClosed) {
		t.Fatalf("second close: %v; want %v", err, ErrConcurrentWriterClosed)
	}
	if err := cw.WriteControl(context.Background(), ws.OpPong, nil); !errors.Is(err, ErrConcurrentWriterClosed) {
		t.Fatalf("pong after close: %v; want %v", err, ErrConcurrentWriterClosed)
	}
}

func assertFrames(t *testing.T, got, want []ws.Frame) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("decoded %d frames; want %d\n%s", len(got), len(want), pretty(got...))
	}
	for i := range want {
		if got[i].Header != want[i].Header || !bytes.Equal(got[i].Payload, want[i].Payload) {
			t.Fatalf("frame %d mismatch:\ngot:\t%s\nwant:\t%s", i, pretty(got[i]), pretty(want[i]))
		}
	}
}
