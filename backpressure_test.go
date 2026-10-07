package smux

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// phaseConn is a net.Conn wrapper whose write behavior can be switched by the
// test. It starts open (writes pass through), can hold subsequent writes in
// place to simulate a slow writer, and can be failed to simulate a broken
// connection. Writes are serialized so the frame currently on the wire is
// well defined.
type phaseConn struct {
	net.Conn

	mu      sync.Mutex
	state   int32 // atomic: 0 = open, 1 = holding, 2 = failed
	blockCh chan struct{}
}

const (
	phaseOpen   int32 = 0
	phaseHold   int32 = 1
	phaseFailed int32 = 2
)

func newPhaseConn(c net.Conn) *phaseConn {
	return &phaseConn{Conn: c}
}

func (c *phaseConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch atomic.LoadInt32(&c.state) {
	case phaseFailed:
		return 0, errors.New("simulated write failure")
	case phaseHold:
		ch := c.blockCh
		c.mu.Unlock()
		<-ch
		c.mu.Lock()
		if atomic.LoadInt32(&c.state) == phaseFailed {
			return 0, errors.New("simulated write failure")
		}
	}
	return c.Conn.Write(b)
}

// hold blocks all future writes in place (writes already issued are not
// affected). It must be called when the connection is open.
func (c *phaseConn) hold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if atomic.LoadInt32(&c.state) == phaseOpen {
		c.blockCh = make(chan struct{})
		atomic.StoreInt32(&c.state, phaseHold)
	}
}

// resume releases any blocked writes and passes future writes through.
func (c *phaseConn) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if atomic.LoadInt32(&c.state) == phaseHold {
		close(c.blockCh)
		atomic.StoreInt32(&c.state, phaseOpen)
	}
}

// fail releases blocked writers and makes every future write fail.
func (c *phaseConn) fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := atomic.LoadInt32(&c.state)
	if state == phaseHold {
		close(c.blockCh)
	}
	atomic.StoreInt32(&c.state, phaseFailed)
}

// newBackpressureConfig returns a v1 config with small deterministic send
// limits suitable for backpressure tests.
func newBackpressureConfig() *Config {
	cfg := DefaultConfig()
	cfg.Version = 1
	cfg.KeepAliveDisabled = true
	cfg.MaxFrameSize = 1024
	cfg.MaxWriteBuffer = 4096
	cfg.MaxStreamWriteBuffer = 2048
	cfg.MaxReceiveBuffer = 1 << 20
	cfg.MaxStreamBuffer = 65536
	return cfg
}

// pipeSessions builds a connected client/server pair over net.Pipe and hands
// back the client's phaseConn for controlled writes.
func pipeSessions(t *testing.T, cfg *Config) (*Session, *Session, *phaseConn) {
	t.Helper()

	serverConn, clientConn := net.Pipe()
	phased := newPhaseConn(clientConn)

	server, err := Server(serverConn, cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	client, err := Client(phased, cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	t.Cleanup(func() {
		phased.fail()
		client.Close()
		server.Close()
	})

	return client, server, phased
}

// openStreamPair opens one client stream and accepts the peer on the server.
// drainTo, when non-nil, receives the exact bytes read on the server side.
func openStreamPair(t *testing.T, client, server *Session, drainTo *bytes.Buffer) *Stream {
	t.Helper()

	st, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	peer, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	go func() {
		if drainTo != nil {
			io.Copy(drainTo, peer)
			return
		}
		io.Copy(io.Discard, peer)
	}()
	return st
}

// waitFor polls get until it returns want.
func waitFor(t *testing.T, get func() int64, want int64) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("counter = %d, want %d", get(), want)
}

// waitForMin polls get until it returns at least want.
func waitForMin(t *testing.T, get func() int64, want int64) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if get() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("counter = %d, want at least %d", get(), want)
}

// refundControl releases control budget reserved by a test.
func refundControl(s *Session, n int) {
	s.budgetLock.Lock()
	s.controlBudget -= int64(n)
	s.budgetLock.Unlock()
	s.notifyBudgetSpace()
}

// setControlBudget overwrites the control budget for test purposes.
func setControlBudget(s *Session, v int64) {
	s.budgetLock.Lock()
	s.controlBudget = v
	s.budgetLock.Unlock()
}

// TestSendBufferBlocking verifies that against a controlled slow writer the
// queued payload is bounded by both the session-wide limit and the per-stream
// limit: blocking writers wait instead of growing memory, and after the writer
// drains every reservation is released exactly once.
func TestSendBufferBlocking(t *testing.T) {
	cfg := newBackpressureConfig()
	client, server, phased := pipeSessions(t, cfg)

	const nStreams = 4
	errs := make([]<-chan error, nStreams)

	// Open all streams while the connection still drains SYN frames.
	streams := make([]*Stream, nStreams)
	for i := range streams {
		streams[i] = openStreamPair(t, client, server, nil)
	}

	// Freeze the underlying writer, then have every stream push data.
	phased.hold()

	for i := range streams {
		st := streams[i]
		errs[i] = func() <-chan error {
			errc := make(chan error, 1)
			go func() {
				payload := make([]byte, 1024)
				for j := 0; j < 8; j++ {
					if _, err := st.Write(payload); err != nil {
						errc <- err
						return
					}
				}
				errc <- nil
			}()
			return errc
		}()
	}

	// Budgets saturate: 4096 session-wide, at most 2048 per stream.
	waitFor(t, client.dataBudget, int64(cfg.MaxWriteBuffer))

	if got := client.dataBudget(); got > int64(cfg.MaxWriteBuffer) {
		t.Fatalf("session pending = %d exceeds limit %d", got, cfg.MaxWriteBuffer)
	}
	client.streamLock.Lock()
	for _, st := range client.streams {
		if got := st.pending(); got > int64(cfg.MaxStreamWriteBuffer) {
			t.Fatalf("stream %d pending = %d exceeds per-stream limit %d",
				st.id, got, cfg.MaxStreamWriteBuffer)
		}
	}
	client.streamLock.Unlock()

	// Pending bytes must be stable while the writer stays frozen: no hidden
	// growth, no premature release.
	snapshot := client.dataBudget()
	time.Sleep(100 * time.Millisecond)
	if got := client.dataBudget(); got != snapshot {
		t.Fatalf("pending changed while writer held: %d -> %d", snapshot, got)
	}

	// Drain and wait for all writers to finish.
	phased.resume()
	for _, errc := range errs {
		if err := <-errc; err != nil {
			t.Fatalf("write after resume: %v", err)
		}
	}

	// Every reservation released exactly once: counters back to zero and
	// never negative.
	waitFor(t, client.dataBudget, 0)
	if got := client.dataBudget(); got < 0 {
		t.Fatalf("negative session pending = %d (double release)", got)
	}
	waitFor(t, client.ctrlBudget, 0)
	client.streamLock.Lock()
	for _, st := range client.streams {
		if got := st.pending(); got != 0 {
			t.Fatalf("stream %d pending = %d, want 0", st.id, got)
		}
	}
	client.streamLock.Unlock()
}

// TestSendBufferNonBlocking verifies that with WriteBlock disabled, writes
// fail immediately with ErrSendBufferFull once the per-stream or session
// budget is full instead of blocking.
func TestSendBufferNonBlocking(t *testing.T) {
	cfg := newBackpressureConfig()
	cfg.WriteBlock = false
	// Per-stream cap equals the session cap so one saturated stream drives
	// the session-wide limit deterministically.
	cfg.MaxStreamWriteBuffer = cfg.MaxWriteBuffer
	client, server, phased := pipeSessions(t, cfg)

	payload := make([]byte, 1024)
	st := openStreamPair(t, client, server, nil)

	phased.hold()

	// Concurrent writers race for the 4096 budget. Exactly four reserve and
	// the rest must bounce immediately with ErrSendBufferFull.
	const nWriters = 12
	errc := make(chan error, nWriters)
	for i := 0; i < nWriters; i++ {
		go func() {
			_, err := st.Write(payload)
			errc <- err
		}()
	}

	// Rejected writers return immediately while the four that reserve stay
	// blocked until the slow writer drains. Collect the immediate rejections.
	waitFor(t, client.dataBudget, int64(cfg.MaxWriteBuffer))

	var full, succeeded int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && full < nWriters-4 {
		select {
		case err := <-errc:
			switch {
			case errors.Is(err, ErrSendBufferFull):
				full++
			case err != nil:
				t.Fatalf("concurrent write: %v", err)
			default:
				succeeded++
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	if full != nWriters-4 {
		t.Fatalf("session cap: %d immediate full errors, want %d", full, nWriters-4)
	}

	// A further write must bounce immediately while the writer is held.
	if _, err := st.Write(payload); !errors.Is(err, ErrSendBufferFull) {
		t.Fatalf("session full write returned %v, want ErrSendBufferFull", err)
	}

	if got := client.dataBudget(); got > int64(cfg.MaxWriteBuffer) {
		t.Fatalf("session pending = %d exceeds limit", got)
	}

	phased.resume()

	// The four reserved writers now drain successfully.
	drainDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(drainDeadline) && full+succeeded < nWriters {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("drained write: %v", err)
			}
			succeeded++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if full+succeeded != nWriters {
		t.Fatalf("writers finished: %d, want %d", full+succeeded, nWriters)
	}
	waitFor(t, client.dataBudget, 0)
}

// TestControlQuotaIndependent verifies that control frames keep an
// independent small quota: when the whole data budget is saturated, a close
// notification (FIN) is still accepted and the control budget can be
// reserved independently of the data backlog.
func TestControlQuotaIndependent(t *testing.T) {
	cfg := newBackpressureConfig()
	client, server, phased := pipeSessions(t, cfg)

	payload := make([]byte, 1024)
	streams := make([]*Stream, 0, 4)
	for i := 0; i < 4; i++ {
		streams = append(streams, openStreamPair(t, client, server, nil))
	}

	phased.hold()

	// One frame per stream saturates the data budget without touching any
	// per-stream cap: 4 x 1024 = 4096.
	saturated := make(chan error, len(streams))
	for _, st := range streams {
		st := st
		go func() {
			_, err := st.Write(payload)
			saturated <- err
		}()
	}
	waitFor(t, client.dataBudget, int64(cfg.MaxWriteBuffer))

	// The control quota reserves independently of the fully saturated data
	// budget (the size of a window-update payload is used here).
	if err := client.reserveControl(szCmdUPD, nil); err != nil {
		t.Fatalf("reserve control quota: %v", err)
	}
	if got := client.ctrlBudget(); got != int64(szCmdUPD) {
		t.Fatalf("control budget = %d, want %d", got, szCmdUPD)
	}
	if got := client.dataBudget(); got != int64(cfg.MaxWriteBuffer) {
		t.Fatalf("data budget changed after control reservation: %d", got)
	}
	refundControl(client, szCmdUPD)

	// A queued control frame (FIN here) is scheduled ahead of the data
	// backlog: once the writer drains it is delivered before any PSH, so a
	// saturated data queue cannot trap close notifications. Close blocks only
	// on the actual (held) write, not on data budget space.
	closeErr := make(chan error, 1)
	go func() { closeErr <- streams[0].Close() }()
	// Give FIN time to join the shaper queue while the writer is held.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-closeErr:
		t.Fatal("FIN should still be waiting on the held underlying writer")
	default:
	}

	// Filling the control quota itself blocks further control reservations
	// while data accounting stays untouched.
	setControlBudget(client, int64(maxControlWriteBuffer))
	errc := make(chan error, 1)
	go func() { errc <- client.reserveControl(1, nil) }()
	select {
	case err := <-errc:
		t.Fatalf("expected control reservation to block, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	setControlBudget(client, 0)
	client.notifyBudgetSpace()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("control reservation after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control reservation did not resume after release")
	}

	// Drain the held writer; the queued FIN then completes successfully.
	phased.resume()
	for i := 0; i < len(streams); i++ {
		if err := <-saturated; err != nil {
			t.Fatalf("saturating write: %v", err)
		}
	}
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("queued FIN after resume: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued FIN did not complete after writer resumed")
	}

	waitFor(t, client.dataBudget, 0)
}

// TestWriteTimeoutKeepsOtherStreamsIntact is the central isolation test: a
// write that times out while queued behind a slow writer is canceled and
// skipped, and the bytes the peer receives on every other stream are exactly
// what those streams wrote, in order, with nothing from the timed-out stream
// interleaved.
func TestWriteTimeoutKeepsOtherStreamsIntact(t *testing.T) {
	cfg := newBackpressureConfig()
	client, server, phased := pipeSessions(t, cfg)

	var got1, got2, got3 bytes.Buffer
	s1 := openStreamPair(t, client, server, &got1)
	s2 := openStreamPair(t, client, server, &got2)
	s3 := openStreamPair(t, client, server, &got3)

	phased.hold()

	frameA := bytes.Repeat([]byte("A"), 1024)
	frameB := bytes.Repeat([]byte("B"), 1024)

	// Queue frame A (becomes the in-flight write behind the held connection).
	errA := make(chan error, 1)
	go func() { _, err := s1.Write(frameA); errA <- err }()
	waitFor(t, client.dataBudget, 1024)

	// Frame B is queued behind A with a short deadline. It times out and is
	// canceled before reaching the wire.
	errB := make(chan error, 1)
	go func() {
		s1.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		_, err := s1.Write(frameB)
		errB <- err
		s1.SetWriteDeadline(time.Time{})
	}()

	select {
	case err := <-errB:
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("timed-out write returned %v, want ErrTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued write did not time out")
	}
	// B's reservation is released by the cancellation.
	waitFor(t, s1.stream.pending, 1024)

	// s2 and s3 each queue a full frame while s1's A is still in flight.
	frameX := bytes.Repeat([]byte("X"), 1024)
	frameY := bytes.Repeat([]byte("Y"), 1024)
	errX := make(chan error, 1)
	errY := make(chan error, 1)
	go func() { _, err := s2.Write(frameX); errX <- err }()
	go func() { _, err := s3.Write(frameY); errY <- err }()

	// Wait until the queued backlog is A + X + Y = 3072 (B was canceled and
	// its reservation refunded).
	waitFor(t, client.dataBudget, 3072)

	// Drain everything and give the peer time to process.
	phased.resume()
	if err := <-errA; err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := <-errX; err != nil {
		t.Fatalf("write X: %v", err)
	}
	if err := <-errY; err != nil {
		t.Fatalf("write Y: %v", err)
	}

	waitFor(t, client.dataBudget, 0)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) &&
		(got1.Len() != 1024 || got2.Len() != 1024 || got3.Len() != 1024) {
		time.Sleep(10 * time.Millisecond)
	}

	if !bytes.Equal(got1.Bytes(), frameA) {
		t.Fatalf("s1 peer got %d bytes: want exactly frame A (B must be skipped)", got1.Len())
	}
	if !bytes.Equal(got2.Bytes(), frameX) {
		t.Fatalf("s2 peer got %d bytes: want exactly frame X", got2.Len())
	}
	if !bytes.Equal(got3.Bytes(), frameY) {
		t.Fatalf("s3 peer got %d bytes: want exactly frame Y", got3.Len())
	}

	// Every reservation, including the canceled B frame, is released once.
	if got := s1.stream.pending(); got != 0 {
		t.Fatalf("s1 pending = %d, want 0", got)
	}
	if got := client.dataBudget(); got != 0 {
		t.Fatalf("session pending = %d, want 0", got)
	}
}

// TestWriteFailureReleasesBudget verifies that when the underlying write
// fails, queued reservations are refunded exactly once, blocked writers wake
// with an error, and no counter is left positive (or driven negative by a
// double release).
func TestWriteFailureReleasesBudget(t *testing.T) {
	cfg := newBackpressureConfig()
	client, server, phased := pipeSessions(t, cfg)

	const nStreams = 4
	streams := make([]*Stream, nStreams)
	for i := range streams {
		streams[i] = openStreamPair(t, client, server, nil)
	}

	phased.hold()

	errc := make(chan error, nStreams)
	for _, st := range streams {
		st := st
		go func() {
			_, err := st.Write(make([]byte, 8192))
			errc <- err
		}()
	}

	// Writers saturate the 4096 budget and then block for space.
	waitFor(t, client.dataBudget, int64(cfg.MaxWriteBuffer))

	// Break the connection: the in-flight write fails and the sendLoop drains
	// the queue, refunding each reservation once.
	phased.fail()

	for i := 0; i < nStreams; i++ {
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("expected write error after connection failure")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("blocked writer did not wake on write failure")
		}
	}

	waitFor(t, client.dataBudget, 0)
	waitFor(t, client.ctrlBudget, 0)
	if got := client.dataBudget(); got < 0 {
		t.Fatalf("negative session pending = %d (double release)", got)
	}
}

// TestSendBufferConfigValidation verifies the new config fields are checked.
func TestSendBufferConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*Config)
	}{
		{"zero write buffer", func(c *Config) { c.MaxWriteBuffer = 0 }},
		{"stream write larger than session", func(c *Config) {
			c.MaxStreamWriteBuffer = c.MaxWriteBuffer + 1
		}},
		{"write buffer below frame size", func(c *Config) {
			c.MaxWriteBuffer = c.MaxFrameSize - 1
			c.MaxStreamWriteBuffer = c.MaxFrameSize - 1
		}},
		{"write buffer overflow", func(c *Config) {
			c.MaxWriteBuffer = 1<<31 + 1
			c.MaxStreamWriteBuffer = 1 << 20
		}},
		{"stream write buffer overflow", func(c *Config) {
			c.MaxStreamWriteBuffer = 1<<31 + 1
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newBackpressureConfig()
			tc.modify(cfg)
			if err := VerifyConfig(cfg); err == nil {
				t.Fatal("expected config verification to fail")
			}
		})
	}

	if err := VerifyConfig(newBackpressureConfig()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestSendBufferV2 verifies the session send cap also bounds queued data on a
// version 2 session, where writes are additionally gated by the peer window.
func TestSendBufferV2(t *testing.T) {
	cfg := newBackpressureConfig()
	cfg.Version = 2
	client, server, phased := pipeSessions(t, cfg)

	// Concurrent writers across several streams saturate the 4096 budget even
	// though each individual Write is synchronous.
	const nStreams = 6
	streams := make([]*Stream, nStreams)
	for i := range streams {
		streams[i] = openStreamPair(t, client, server, nil)
	}

	phased.hold()

	errs := make([]<-chan error, nStreams)
	for i := 0; i < nStreams; i++ {
		st := streams[i]
		errc := make(chan error, 1)
		errs[i] = errc
		go func() {
			_, err := st.Write(make([]byte, 1024))
			errc <- err
		}()
	}

	waitFor(t, client.dataBudget, int64(cfg.MaxWriteBuffer))
	if got := client.dataBudget(); got > int64(cfg.MaxWriteBuffer) {
		t.Fatalf("v2 session pending = %d exceeds limit %d", got, cfg.MaxWriteBuffer)
	}

	phased.resume()
	for _, errc := range errs {
		if err := <-errc; err != nil {
			t.Fatalf("v2 write after resume: %v", err)
		}
	}
	waitFor(t, client.dataBudget, 0)
}
