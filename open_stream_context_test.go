package smux

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateConn blocks the first Write matching a predicate, recording every frame
// that successfully reaches the wire. It builds deterministic interleavings
// for OpenStreamContext cancellation tests.
type gateConn struct {
	net.Conn

	mu         sync.Mutex
	gateOnce   sync.Once
	records    []rawHeader
	hit        chan struct{} // closed when the matching Write is parked
	release    chan struct{} // closing unblocks the parked Write
	shouldGate func(rawHeader) bool
}

func newGateConn(conn net.Conn, match func(rawHeader) bool) *gateConn {
	return &gateConn{
		Conn:       conn,
		hit:        make(chan struct{}),
		release:    make(chan struct{}),
		shouldGate: match,
	}
}

func (g *gateConn) Write(b []byte) (int, error) {
	var hdr rawHeader
	copy(hdr[:], b[:headerSize])

	g.mu.Lock()
	gate := false
	if g.shouldGate(hdr) {
		g.gateOnce.Do(func() {
			gate = true
		})
	}
	g.mu.Unlock()

	if gate {
		close(g.hit)
		<-g.release
	}

	n, err := g.Conn.Write(b)
	if err == nil {
		g.mu.Lock()
		g.records = append(g.records, hdr)
		g.mu.Unlock()
	}
	return n, err
}

// releaseGate unblocks the parked Write.
func (g *gateConn) releaseGate() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

// Close unblocks a writer that is still parked so its goroutine can exit.
func (g *gateConn) Close() error {
	g.releaseGate()
	return g.Conn.Close()
}

func (g *gateConn) recordedCmds(sid uint32) (cmds []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, h := range g.records {
		if h.StreamID() == sid {
			cmds = append(cmds, h.Cmd())
		}
	}
	return cmds
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}

func gateHit(g *gateConn) bool {
	select {
	case <-g.hit:
		return true
	default:
		return false
	}
}

// newGatedPair wires two smux sessions over net.Pipe. Only the client side
// uses a gateConn; keepalives are disabled for a deterministic wire.
func newGatedPair(t *testing.T, version int, match func(rawHeader) bool) (cli *Session, srv *Session, gate *gateConn) {
	t.Helper()
	p1, p2 := net.Pipe()
	gate = newGateConn(p1, match)

	cfg := DefaultConfig()
	cfg.Version = version
	cfg.KeepAliveDisabled = true

	cli, err := Client(gate, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err = Server(p2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cli.Close()
		srv.Close()
	})
	return cli, srv, gate
}

// injectControl enqueues a control frame directly through the shaper.
func injectControl(t *testing.T, s *Session, f Frame, seq uint32) chan writeResult {
	t.Helper()
	resultCh := make(chan writeResult, 1)
	req := writeRequest{
		class:  CLSCTRL,
		frame:  f,
		seq:    seq,
		result: resultCh,
	}
	select {
	case s.shaper <- req:
	case <-time.After(time.Second):
		t.Fatal("failed to inject control frame")
	}
	return resultCh
}

func containsCmd(cmds []byte, cmd byte) bool {
	for _, c := range cmds {
		if c == cmd {
			return true
		}
	}
	return false
}

// queuedForSID reports whether a frame for sid is currently waiting in the
// shaper queue.
func queuedForSID(s *Session, sid uint32) bool {
	s.sq.mu.Lock()
	defer s.sq.mu.Unlock()
	h, ok := s.sq.streams[sid]
	return ok && h.Len() > 0
}

func versionName(v int) string { return []string{"", "v1", "v2"}[v] }

// echoPeer accepts one stream and echoes its data until EOF.
func echoPeer(srv *Session, ready chan<- *Stream) {
	sst, err := srv.AcceptStream()
	if err != nil {
		ready <- nil
		return
	}
	ready <- sst
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := sst.Read(buf)
			if err != nil {
				return
			}
			if _, err := sst.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
}

// assertSingleResult verifies errCh delivers exactly one error and nothing
// more afterwards.
func assertSingleResult(t *testing.T, errCh <-chan error) error {
	t.Helper()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenStreamContext did not return")
	}
	select {
	case extra := <-errCh:
		t.Fatalf("unexpected second result: %v", extra)
	case <-time.After(100 * time.Millisecond):
	}
	return err
}

// TestOpenStreamContextCancelBeforeSYNCommitted cancels while the SYN is
// still queued behind a blocked control frame: no SYN reaches the wire, the
// peer never observes the stream, exactly one result is returned, and the
// allocated identifier is never reused.
func TestOpenStreamContextCancelBeforeSYNCommitted(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(versionName(version), func(t *testing.T) {
			testCancelBeforeCommitted(t, version)
		})
	}
}

func testCancelBeforeCommitted(t *testing.T, version int) {
	cli, srv, gate := newGatedPair(t, version, func(h rawHeader) bool {
		return h.Cmd() == cmdNOP && h.StreamID() == 3
	})

	// A NOP sharing sid 3 is enqueued before the SYN. Both requests live in
	// the same per-stream heap ordered by sequence, so the SYN cannot pass it.
	nop := newFrame(byte(version), cmdNOP, 3)
	seq := atomic.AddUint32(&cli.requestID, 1)
	nopResult := injectControl(t, cli, nop, seq)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := cli.OpenStreamContext(ctx)
		errCh <- err
	}()

	waitFor(t, time.Second, func() bool { return gateHit(gate) }, "blocking NOP was never written")
	waitFor(t, time.Second, func() bool { return queuedForSID(cli, 3) }, "SYN not queued behind blocked frame")

	cancel()
	if err := assertSingleResult(t, errCh); err == nil {
		t.Fatal("expected error after cancellation, got nil")
	}

	// Release the blocked NOP; the withdrawn SYN must never touch the wire.
	gate.releaseGate()
	select {
	case <-nopResult:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking NOP did not complete")
	}
	waitFor(t, time.Second, func() bool { return !queuedForSID(cli, 3) }, "SYN not withdrawn from queue")

	if cmds := gate.recordedCmds(3); containsCmd(cmds, cmdSYN) || containsCmd(cmds, cmdFIN) {
		t.Fatalf("withdrawn open left frames on the wire: %v", cmds)
	}

	// The peer must not see a stream for sid 3.
	srv.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := srv.AcceptStream(); err == nil {
		t.Fatal("peer accepted a stream from an uncommitted SYN")
	}

	// sid 3 was consumed: the next successful open must use sid 5.
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if st.ID() != 5 {
		t.Fatalf("stream id reused/skipped, got %d, want 5", st.ID())
	}
	st.Close()

	if cmds := gate.recordedCmds(3); len(cmds) != 1 || cmds[0] != cmdNOP {
		t.Fatalf("withdrawn sid 3 wire commands = %v, want only the leading NOP", cmds)
	}
}

// TestOpenStreamContextCancelAfterSYNCommitted cancels after the SYN Write
// has been performed but before the call returns: a FIN must be emitted for
// the now visible remote stream, with exactly one result returned.
func TestOpenStreamContextCancelAfterSYNCommitted(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(versionName(version), func(t *testing.T) {
			testCancelAfterCommitted(t, version)
		})
	}
}

func testCancelAfterCommitted(t *testing.T, version int) {
	cli, srv, gate := newGatedPair(t, version, func(h rawHeader) bool {
		return h.Cmd() == cmdSYN && h.StreamID() == 3
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := cli.OpenStreamContext(ctx)
		errCh <- err
	}()

	// Wait until the SYN write is parked inside the blocked conn.Write.
	waitFor(t, 2*time.Second, func() bool { return gateHit(gate) }, "SYN write was never attempted")

	// Cancel while the SYN is inside Write, then let it land on the wire.
	cancel()
	gate.releaseGate()

	if err := assertSingleResult(t, errCh); err == nil {
		t.Fatal("expected error after cancellation, got nil")
	}

	// Protocol-level cleanup must follow the committed SYN, in order.
	waitFor(t, 2*time.Second, func() bool {
		cmds := gate.recordedCmds(3)
		return len(cmds) >= 2 && cmds[0] == cmdSYN && cmds[1] == cmdFIN
	}, "SYN or ordered cleanup FIN missing from wire")

	// Peer sees the stream open, then FIN (EOF).
	sst, err := srv.AcceptStream()
	if err != nil {
		t.Fatalf("peer missed committed SYN: %v", err)
	}
	if sst.ID() != 3 {
		t.Fatalf("accepted sid = %d, want 3", sst.ID())
	}
	buf := make([]byte, 16)
	waitFor(t, 2*time.Second, func() bool {
		_, rerr := sst.Read(buf)
		return rerr == io.EOF
	}, "peer did not observe FIN for abandoned stream")

	// Identifier must not be reused.
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if st.ID() != 5 {
		t.Fatalf("stream id reused, got %d, want 5", st.ID())
	}
	st.Close()
}

// TestOpenStreamContextSuccess covers the normal open path and verifies that
// context cancellation after return has no effect on the returned stream.
func TestOpenStreamContextSuccess(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(versionName(version), func(t *testing.T) {
			testContextSuccess(t, version)
		})
	}
}

func testContextSuccess(t *testing.T, version int) {
	cli, srv, gate := newGatedPair(t, version, func(rawHeader) bool { return false })

	ready := make(chan *Stream, 1)
	go echoPeer(srv, ready)

	ctx, cancel := context.WithCancel(context.Background())
	st, err := cli.OpenStreamContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sst := <-ready
	if sst == nil {
		t.Fatal("peer did not accept stream")
	}
	if st.ID() != sst.ID() {
		t.Fatalf("sid mismatch: %d vs %d", st.ID(), sst.ID())
	}

	// Cancel after return must not disturb the stream or send a FIN.
	cancel()

	payload := []byte("hello-open-stream-context")
	if _, err := st.Write(payload); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(st, echo); err != nil {
		t.Fatalf("echo read failed: %v", err)
	}
	if string(echo) != string(payload) {
		t.Fatalf("echo mismatch: %q", echo)
	}

	if cmds := gate.recordedCmds(st.ID()); containsCmd(cmds, cmdFIN) {
		t.Fatalf("unexpected cleanup FIN for returned stream: %v", cmds)
	}
	st.Close()
}

// TestOpenStreamContextPreCanceled verifies an already canceled context
// fails immediately without allocating an identifier or sending a SYN.
func TestOpenStreamContextPreCanceled(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(versionName(version), func(t *testing.T) {
			cli, srv, gate := newGatedPair(t, version, func(rawHeader) bool { return false })

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := cli.OpenStreamContext(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if len(gate.records) != 0 {
				t.Fatalf("frames written before any open: %v", gate.records)
			}

			// The pre-canceled open must not be visible on the wire before
			// a real open consumes the identifier.
			srv.SetDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err := srv.AcceptStream(); err == nil {
				t.Fatal("peer observed stream from pre-canceled open")
			}

			st, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if st.ID() != 3 {
				t.Fatalf("sid = %d, want 3 (first client allocation)", st.ID())
			}
			st.Close()
		})
	}
}

// TestOpenStreamContextStreamIDNotReused interleaves successful,
// uncommitted-aborted and pre-canceled opens and verifies identifiers
// strictly advance by two and are never reused.
func TestOpenStreamContextStreamIDNotReused(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(versionName(version), func(t *testing.T) {
			testStreamIDNotReused(t, version)
		})
	}
}

func testStreamIDNotReused(t *testing.T, version int) {
	cli, srv, gate := newGatedPair(t, version, func(h rawHeader) bool {
		return h.Cmd() == cmdNOP && h.StreamID() == 5
	})

	// Drain accepted streams; echo + FIN release them on both sides.
	go func() {
		for {
			srv.SetDeadline(time.Now().Add(5 * time.Second))
			sst, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go func(s io.ReadWriteCloser) {
				io.Copy(s, s)
				s.Close()
			}(sst)
		}
	}()

	// First successful open consumes sid 3 (the allocator seeds at 1 and
	// advances by two before returning).
	st, err := cli.OpenStreamContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.ID() != 3 {
		t.Fatalf("sid = %d, want 3", st.ID())
	}

	// Close sid 3 and let its FIN land before arming the blocker, so the
	// blocker does not hold up the FIN through round-robin scheduling.
	st.Close()
	waitFor(t, time.Second, func() bool {
		cmds := gate.recordedCmds(3)
		return len(cmds) == 2 && cmds[0] == cmdSYN && cmds[1] == cmdFIN
	}, "sid 3 did not finish")

	nop := newFrame(byte(version), cmdNOP, 5)
	nopResult := injectControl(t, cli, nop, atomic.AddUint32(&cli.requestID, 1))

	// An uncommitted abort consumes sid 5: the NOP sharing sid 5 keeps the
	// SYN parked until cancellation withdraws it.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := cli.OpenStreamContext(ctx)
		errCh <- err
	}()

	waitFor(t, 2*time.Second, func() bool { return gateHit(gate) }, "NOP never parked")
	select {
	case e := <-errCh:
		t.Fatalf("open returned before SYN queued: %v", e)
	default:
	}
	waitFor(t, 2*time.Second, func() bool { return queuedForSID(cli, 5) }, "SYN not queued behind blocker")

	cancel()
	if err := assertSingleResult(t, errCh); err == nil {
		t.Fatal("aborted open returned nil")
	}

	gate.releaseGate()
	select {
	case <-nopResult:
	case <-time.After(2 * time.Second):
		t.Fatal("NOP did not drain")
	}
	waitFor(t, time.Second, func() bool { return !queuedForSID(cli, 5) }, "SYN not withdrawn from queue")
	if cmds := gate.recordedCmds(5); containsCmd(cmds, cmdSYN) {
		t.Fatalf("withdrawn SYN for sid 5 reached wire: %v", cmds)
	}

	// Successful opens continue at sid 7, 9.
	for _, wantSID := range []uint32{7, 9} {
		st, err := cli.OpenStreamContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.ID() != wantSID {
			t.Fatalf("sid = %d, want %d", st.ID(), wantSID)
		}
		st.Close()
	}

	// A pre-canceled open allocates nothing; next open uses sid 11.
	pctx, pcancel := context.WithCancel(context.Background())
	pcancel()
	if _, err := cli.OpenStreamContext(pctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled err = %v", err)
	}
	st11, err := cli.OpenStreamContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st11.ID() != 11 {
		t.Fatalf("sid = %d, want 11", st11.ID())
	}
	st11.Close()
}

// TestOpenStreamContextOthersUnaffected verifies that while one open is
// blocked in the control-frame writer, existing streams remain readable
// (recvLoop is independent), and that after the blocker is released other
// streams continue to read and write normally.
func TestOpenStreamContextOthersUnaffected(t *testing.T) {
	for _, version := range []int{1, 2} {
		version := version
		t.Run(versionName(version), func(t *testing.T) {
			testOthersUnaffected(t, version)
		})
	}
}

func testOthersUnaffected(t *testing.T, version int) {
	cli, srv, gate := newGatedPair(t, version, func(h rawHeader) bool {
		return h.Cmd() == cmdSYN && h.StreamID() == 5
	})

	// Establish a working stream first. The first client allocation is sid 3.
	accepted := make(chan *Stream, 1)
	go func() {
		sst, aerr := srv.AcceptStream()
		if aerr != nil {
			accepted <- nil
			return
		}
		accepted <- sst
		go func() {
			buf := make([]byte, 64)
			for {
				n, rerr := sst.Read(buf)
				if rerr != nil {
					return
				}
				if _, werr := sst.Write(buf[:n]); werr != nil {
					return
				}
			}
		}()
	}()
	existing, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if existing.ID() != 3 {
		t.Fatalf("existing sid = %d, want 3", existing.ID())
	}
	sst := <-accepted
	if sst == nil {
		t.Fatal("peer did not accept the working stream")
	}

	// Baseline: the existing stream works in both directions.
	roundTrip := func(t *testing.T, payload string) {
		t.Helper()
		if _, werr := existing.Write([]byte(payload)); werr != nil {
			t.Fatalf("write failed: %v", werr)
		}
		buf := make([]byte, len(payload))
		if _, rerr := io.ReadFull(existing, buf); rerr != nil {
			t.Fatalf("echo failed: %v", rerr)
		}
	}
	roundTrip(t, "before-block")

	// Begin a second open that parks inside the SYN writer. The shaper
	// allocator hands the second open sid 5.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := cli.OpenStreamContext(ctx)
		errCh <- err
	}()
	waitFor(t, 2*time.Second, func() bool { return gateHit(gate) }, "blocked SYN never parked")

	// While the single outbound writer is parked, the read side still runs:
	// the peer can open a brand new stream and the client receives it.
	srvOpened := make(chan *Stream, 1)
	go func() {
		rs, oerr := srv.OpenStream()
		if oerr != nil {
			srvOpened <- nil
			return
		}
		srvOpened <- rs
	}()
	var inbound *Stream
	waitFor(t, 3*time.Second, func() bool {
		rs, aerr := cli.AcceptStream()
		if aerr != nil {
			return false
		}
		inbound = rs
		return true
	}, "client did not receive peer-initiated stream while writer parked")
	if inbound == nil {
		t.Fatal("nil inbound stream")
	}
	rs := <-srvOpened
	if rs == nil {
		t.Fatal("server failed to open inbound stream")
	}

	// Accepting the peer-initiated stream while the outbound writer stays
	// parked proves the recvLoop is independent.
	if inbound.ID() != rs.ID() {
		t.Fatalf("inbound sid = %d, want %d", inbound.ID(), rs.ID())
	}

	// Release the parked open; afterwards the other streams exchange data in
	// both directions, including the peer-initiated one (v2 window updates
	// require the outbound writer, so its data phase starts after release).
	cancel()
	gate.releaseGate()
	assertSingleResult(t, errCh)

	probeIn := []byte("after-release-read")
	done := make(chan struct{})
	go func() {
		rs.Write(probeIn)
		close(done)
	}()
	in := make([]byte, len(probeIn))
	if _, rerr := io.ReadFull(inbound, in); rerr != nil {
		t.Fatalf("read of peer data failed after release: %v", rerr)
	}
	if string(in) != string(probeIn) {
		t.Fatalf("mismatch: %q", in)
	}
	<-done
	rs.Close()
	inbound.Close()

	// The original stream keeps working in both directions.
	roundTrip(t, "after-release")

	// And opening new streams keeps working; identifiers still advance.
	var another *Stream
	another, err = cli.OpenStream()
	if err != nil {
		t.Fatalf("open after release failed: %v", err)
	}
	if another.ID() != 7 {
		t.Fatalf("sid = %d, want 7", another.ID())
	}
	another.Close()
}
