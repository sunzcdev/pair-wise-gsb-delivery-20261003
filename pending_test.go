package smux

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedConn is an io.ReadWriteCloser whose writes are fully controlled by
// the test: every Write blocks until the test consumes it from writesC,
// simulating an arbitrarily slow backend connection. Reads block until the
// connection is closed.
type gatedConn struct {
	writesC chan []byte
	die     chan struct{}
	dieOnce sync.Once
}

func newGatedConn() *gatedConn {
	return &gatedConn{
		writesC: make(chan []byte),
		die:     make(chan struct{}),
	}
}

func (c *gatedConn) Read(b []byte) (int, error) {
	<-c.die
	return 0, io.EOF
}

func (c *gatedConn) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case c.writesC <- cp:
		return len(b), nil
	case <-c.die:
		return 0, io.ErrClosedPipe
	}
}

func (c *gatedConn) Close() error {
	c.dieOnce.Do(func() { close(c.die) })
	return nil
}

// capturedFrame is a parsed frame consumed from a gatedConn.
type capturedFrame struct {
	cmd  byte
	sid  uint32
	data []byte
}

func parseCapturedFrame(t *testing.T, b []byte) capturedFrame {
	t.Helper()
	if len(b) < headerSize {
		t.Fatalf("frame too short: %d bytes", len(b))
	}
	var hdr rawHeader
	copy(hdr[:], b[:headerSize])
	if hdr.Length() != uint16(len(b)-headerSize) {
		t.Fatalf("length mismatch: header %d, actual %d", hdr.Length(), len(b)-headerSize)
	}
	return capturedFrame{cmd: hdr.Cmd(), sid: hdr.StreamID(), data: b[headerSize:]}
}

// recvFrame consumes one frame from the gated connection.
func recvFrame(t *testing.T, conn *gatedConn) capturedFrame {
	t.Helper()
	select {
	case b := <-conn.writesC:
		return parseCapturedFrame(t, b)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return capturedFrame{}
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, msg)
}

func pendingTestConfig() *Config {
	config := DefaultConfig()
	config.KeepAliveDisabled = true
	config.MaxFrameSize = 1024
	config.MaxPendingBytes = 2048
	return config
}

// openStreamGated opens a stream while consuming its SYN frame from the
// gated connection, so that OpenStream can complete.
func openStreamGated(t *testing.T, session *Session, conn *gatedConn) *Stream {
	t.Helper()
	type result struct {
		stream *Stream
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		stream, err := session.OpenStream()
		ch <- result{stream, err}
	}()
	if f := recvFrame(t, conn); f.cmd != cmdSYN {
		t.Fatalf("expected SYN frame, got cmd %d", f.cmd)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.stream
}

// maxRecorder samples an int64 source and records its maximum.
func maxRecorder(src *int64, stop <-chan struct{}) *int64 {
	max := new(int64)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			cur := atomic.LoadInt64(src)
			for {
				old := atomic.LoadInt64(max)
				if cur <= old || atomic.CompareAndSwapInt64(max, old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return max
}

// TestPendingSessionLimitBlocks verifies that with a slow backend connection,
// queued-but-unsent payload across multiple streams is bounded by
// MaxPendingBytes, writers block when the budget is exhausted, and all
// counters return to zero once the connection drains.
func TestPendingSessionLimitBlocks(t *testing.T) {
	config := pendingTestConfig()
	conn := newGatedConn()
	session, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	const numStreams = 4
	const writeSize = 8192

	stopMon := make(chan struct{})
	defer close(stopMon)
	maxPending := maxRecorder(&session.pendingDataBytes, stopMon)

	streams := make([]*Stream, numStreams)
	for i := range streams {
		streams[i] = openStreamGated(t, session, conn)
	}

	var wg sync.WaitGroup
	var completed int32
	errs := make([]error, numStreams)
	for i, stream := range streams {
		wg.Add(1)
		go func(i int, stream *Stream) {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte('a' + i)}, writeSize)
			var n int
			n, errs[i] = stream.Write(data)
			if errs[i] == nil && n != writeSize {
				errs[i] = io.ErrShortWrite
			}
			atomic.AddInt32(&completed, 1)
		}(i, stream)
	}

	// the connection is frozen: exactly MaxPendingBytes of payload must pile
	// up, and no writer may complete.
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == int64(config.MaxPendingBytes)
	}, "pending bytes did not reach the session limit")
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(&session.pendingDataBytes); got != int64(config.MaxPendingBytes) {
		t.Fatalf("pending bytes %v exceeds the session limit %v", got, config.MaxPendingBytes)
	}
	if got := atomic.LoadInt32(&completed); got != 0 {
		t.Fatalf("%v writers completed while the connection was frozen", got)
	}

	// drain the connection and verify data integrity per stream
	received := make(map[uint32][]byte)
	for i := 0; i < numStreams*writeSize/config.MaxFrameSize; i++ {
		f := recvFrame(t, conn)
		if f.cmd != cmdPSH {
			t.Fatalf("expected PSH frame, got cmd %d", f.cmd)
		}
		received[f.sid] = append(received[f.sid], f.data...)
	}
	wg.Wait()
	for i, stream := range streams {
		if errs[i] != nil {
			t.Fatalf("stream %d write error: %v", i, errs[i])
		}
		want := bytes.Repeat([]byte{byte('a' + i)}, writeSize)
		if !bytes.Equal(received[stream.ID()], want) {
			t.Fatalf("stream %d data corrupted", i)
		}
	}

	// all budget must be released exactly once: counters return to zero
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == 0 &&
			atomic.LoadInt64(&session.pendingCtrlBytes) == 0
	}, "pending counters did not return to zero")

	// even transiently, the queued payload must stay bounded by the limit
	// plus at most one in-flight frame per concurrent writer
	if got := atomic.LoadInt64(maxPending); got > int64(config.MaxPendingBytes+numStreams*config.MaxFrameSize) {
		t.Fatalf("pending bytes peaked at %v, limit %v", got, config.MaxPendingBytes)
	}
}

// TestPendingFailFast verifies that with PendingFailFast, writes fail
// immediately with ErrBackpressure instead of waiting for space.
func TestPendingFailFast(t *testing.T) {
	config := pendingTestConfig()
	config.PendingPolicy = PendingFailFast
	conn := newGatedConn()
	session, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	const numStreams = 4
	streams := make([]*Stream, numStreams)
	for i := range streams {
		streams[i] = openStreamGated(t, session, conn)
	}

	// the budget holds exactly two 1024-byte frames; with four concurrent
	// writers, exactly two must fail fast with ErrBackpressure.
	var wg sync.WaitGroup
	var failed int32
	errs := make([]error, numStreams)
	for i, stream := range streams {
		wg.Add(1)
		go func(i int, stream *Stream) {
			defer wg.Done()
			_, errs[i] = stream.Write(bytes.Repeat([]byte{byte('a' + i)}, 4096))
			if errs[i] == ErrBackpressure {
				atomic.AddInt32(&failed, 1)
			}
		}(i, stream)
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == int64(config.MaxPendingBytes)
	}, "pending bytes did not reach the session limit")

	// exactly two writers must have failed by now, while the connection is
	// still frozen.
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&failed) == numStreams-2
	}, "writers did not fail fast")
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&failed); got != numStreams-2 {
		t.Fatalf("expected %d fail-fast writers, got %d", numStreams-2, got)
	}

	// drain everything; the blocked writers complete and counters reset.
	for i := 0; i < 2*4096/config.MaxFrameSize; i++ {
		recvFrame(t, conn)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && err != ErrBackpressure {
			t.Fatalf("stream %d unexpected error: %v", i, err)
		}
	}
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == 0
	}, "pending counter did not return to zero")
}

// TestPendingStreamLimit verifies that a per-stream limit smaller than the
// session limit throttles one stream without affecting others.
func TestPendingStreamLimit(t *testing.T) {
	config := pendingTestConfig()
	config.MaxPendingBytes = 65536
	config.MaxStreamPendingBytes = 1024
	conn := newGatedConn()
	session, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	streamA := openStreamGated(t, session, conn)
	streamB := openStreamGated(t, session, conn)

	// three goroutines hammer stream A, one goroutine writes stream B
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamA.Write(bytes.Repeat([]byte{'a'}, 4096))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamB.Write(bytes.Repeat([]byte{'b'}, 4096))
	}()

	// stream A is capped by its own limit, stream B is unaffected and still
	// gets its frame queued under the session budget.
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&streamA.pendingBytes) == int64(config.MaxStreamPendingBytes) &&
			atomic.LoadInt64(&streamB.pendingBytes) == int64(config.MaxFrameSize)
	}, "per-stream pending limits not reached")
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt64(&streamA.pendingBytes); got != int64(config.MaxStreamPendingBytes) {
		t.Fatalf("stream A pending %v exceeds its limit %v", got, config.MaxStreamPendingBytes)
	}
	if got := atomic.LoadInt64(&session.pendingDataBytes); got != 2048 {
		t.Fatalf("session pending %v, want 2048", got)
	}

	// drain all frames: 3*4096 from A and 4096 from B
	total := 0
	for total < 4*4096 {
		f := recvFrame(t, conn)
		total += len(f.data)
	}
	wg.Wait()
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == 0 &&
			atomic.LoadInt64(&streamA.pendingBytes) == 0 &&
			atomic.LoadInt64(&streamB.pendingBytes) == 0
	}, "pending counters did not return to zero")
}

// TestPendingWriteTimeoutIsolation verifies that a write timing out on one
// stream neither corrupts nor discards queued data of other streams, and
// that the timed-out frame's budget is still released exactly once.
func TestPendingWriteTimeoutIsolation(t *testing.T) {
	config := pendingTestConfig()
	config.MaxPendingBytes = 65536
	conn := newGatedConn()
	session, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	streamA := openStreamGated(t, session, conn)
	streamB := openStreamGated(t, session, conn)

	dataA := bytes.Repeat([]byte{'a'}, 4096)
	dataB := bytes.Repeat([]byte{'b'}, 2048)

	// stream A writes with a short deadline; the connection is frozen, so
	// the write times out while its first frame stays queued.
	streamA.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := streamA.Write(dataA)
	if err != ErrTimeout {
		t.Fatalf("expected ErrTimeout, got n=%d err=%v", n, err)
	}

	// stream B queues its first frame (blocked on the write result)
	bDone := make(chan error, 1)
	go func() {
		n, err := streamB.Write(dataB)
		if err == nil && n != len(dataB) {
			err = io.ErrShortWrite
		}
		bDone <- err
	}()

	// A's timed-out frame and B's first frame are both still queued
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == 2048
	}, "queued bytes after timeout mismatch")

	// drain: A's queued frame and B's data must arrive intact
	received := make(map[uint32][]byte)
	for i := 0; i < 3; i++ {
		f := recvFrame(t, conn)
		received[f.sid] = append(received[f.sid], f.data...)
	}
	if !bytes.Equal(received[streamA.ID()], dataA[:config.MaxFrameSize]) {
		t.Fatal("timed-out stream's queued frame was corrupted or lost")
	}
	if !bytes.Equal(received[streamB.ID()], dataB) {
		t.Fatal("other stream's queued data was corrupted by the timeout")
	}
	if err := <-bDone; err != nil {
		t.Fatal("stream B write failed:", err)
	}

	// A gave up after its first frame, no further frames may arrive
	select {
	case b := <-conn.writesC:
		f := parseCapturedFrame(t, b)
		t.Fatalf("unexpected extra frame: cmd %d sid %d", f.cmd, f.sid)
	case <-time.After(200 * time.Millisecond):
	}

	// the timed-out frame's budget is released exactly once
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == 0 &&
			atomic.LoadInt64(&streamA.pendingBytes) == 0 &&
			atomic.LoadInt64(&streamB.pendingBytes) == 0
	}, "pending counters did not return to zero")
}

// TestPendingControlBudgetIndependent verifies that control frames have a
// dedicated budget and cannot be starved by data frames, and that a blocked
// waiter is woken when budget is released.
func TestPendingControlBudgetIndependent(t *testing.T) {
	config := DefaultConfig()
	config.MaxPendingBytes = 1024
	s := &Session{
		config:             config,
		die:                make(chan struct{}),
		chSocketWriteError: make(chan struct{}),
	}

	// fill the data budget completely
	if err := s.acquirePending(nil, 1024, false, nil); err != nil {
		t.Fatal(err)
	}

	// a further data acquire must block
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- s.acquirePending(nil, 1, false, nil)
	}()
	select {
	case err := <-waitErr:
		t.Fatal("data acquire should block on a full budget, got", err)
	case <-time.After(100 * time.Millisecond):
	}

	// control frames (e.g. FIN close notifications) still go through
	if err := s.acquirePending(nil, headerSize, true, nil); err != nil {
		t.Fatal("control acquire starved by data budget:", err)
	}

	// releasing data budget wakes the blocked waiter
	s.releasePending(&writeRequest{pendBytes: 1024})
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatal("waiter returned error:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken after budget release")
	}

	// fail-fast policy returns ErrBackpressure instead of waiting
	s.config.PendingPolicy = PendingFailFast
	if err := s.acquirePending(nil, 1<<20, false, nil); err != ErrBackpressure {
		t.Fatal("expected ErrBackpressure, got", err)
	}
}

// TestPendingReleasedOnSessionClose verifies that closing the session
// releases the budget of all queued frames exactly once.
func TestPendingReleasedOnSessionClose(t *testing.T) {
	config := pendingTestConfig()
	config.MaxPendingBytes = 65536
	conn := newGatedConn()
	session, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}

	const numStreams = 2
	for i := 0; i < numStreams; i++ {
		stream := openStreamGated(t, session, conn)
		go stream.Write(bytes.Repeat([]byte{byte('a' + i)}, 8192))
	}

	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) > 0
	}, "no queued data before close")

	// let the writers settle into waiting for write results
	time.Sleep(100 * time.Millisecond)

	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		return atomic.LoadInt64(&session.pendingDataBytes) == 0 &&
			atomic.LoadInt64(&session.pendingCtrlBytes) == 0
	}, "pending counters did not return to zero after close")
}
