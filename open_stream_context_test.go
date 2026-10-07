package smux

// MIT License
//
// Copyright (c) 2016-2017 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// frameEvent records an observed smux frame header at the client boundary.
type frameEvent struct {
	cmd byte
	sid uint32
}

// gateConn wraps a net.Conn and lets tests block the control-frame writer at
// precise interleaving points. When gate is non-nil, Write blocks until a
// release. The gate either blocks every write, or arms for frames matching
// armPred. In armed mode onMatch is closed once the matching write is about to
// happen (and is parked), giving tests a deterministic interleave point.
// Observed SYN/FIN frames are recorded before the underlying write so a parked
// SYN is visible.
type gateConn struct {
	net.Conn
	mu      sync.Mutex
	gate    chan struct{}
	armed   bool
	blocked bool
	armPred func(byte, uint32) bool
	onMatch chan struct{}
	events  []frameEvent
}

func newGateConn(conn net.Conn) *gateConn {
	return &gateConn{Conn: conn}
}

// block makes every subsequent Write park on the returned gate until release.
func (c *gateConn) block() chan struct{} {
	ch := make(chan struct{})
	c.mu.Lock()
	c.gate = ch
	c.blocked = true
	c.mu.Unlock()
	return ch
}

// arm makes the next Write matching pred park on a gate until release. The
// returned gate is closed on release; the caller should wait on entered to
// know the matching frame is being written and parked.
func (c *gateConn) arm(pred func(byte, uint32) bool) (gate chan struct{}, entered chan struct{}) {
	ch := make(chan struct{})
	entered = make(chan struct{})
	c.mu.Lock()
	c.gate = ch
	c.armed = true
	c.blocked = false
	c.armPred = pred
	c.onMatch = entered
	c.mu.Unlock()
	return ch, entered
}

// release unblocks a currently blocked (and future) Write.
func (c *gateConn) release() {
	c.mu.Lock()
	ch := c.gate
	c.gate = nil
	c.armed = false
	c.blocked = false
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// snapshot returns a copy of the observed SYN/FIN events so far.
func (c *gateConn) snapshot() []frameEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]frameEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *gateConn) Write(b []byte) (int, error) {
	// Record every complete frame header present in b, skipping payloads.
	off := 0
	for off+headerSize <= len(b) {
		cmd := b[off+sizeOfVer]
		length := int(binary.LittleEndian.Uint16(b[off+sizeOfVer+sizeOfCmd:]))
		sid := binary.LittleEndian.Uint32(b[off+sizeOfVer+sizeOfCmd+sizeOfLength:])

		c.mu.Lock()
		if cmd == cmdSYN || cmd == cmdFIN {
			c.events = append(c.events, frameEvent{cmd: cmd, sid: sid})
		}
		var ch chan struct{}
		if c.blocked {
			ch = c.gate
			c.mu.Unlock()
			<-ch
		} else if c.armed && c.armPred != nil && c.armPred(cmd, sid) {
			ch = c.gate
			c.armed = false
			onMatch := c.onMatch
			c.mu.Unlock()
			close(onMatch)
			<-ch
		} else {
			c.mu.Unlock()
		}
		off += headerSize + length
	}
	return c.Conn.Write(b)
}

// ctxPair builds a client/server session pair over a gate-controlled TCP
// connection. Keep-alives are disabled so they cannot perturb gate timing.
func ctxPair(t *testing.T, version int) (cli *Session, srv *Session, gate *gateConn, cleanup func()) {
	t.Helper()
	conn0, conn1, err := getTCPConnectionPair()
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Version = version
	cfg.KeepAliveDisabled = true

	gate = newGateConn(conn1)
	cli, err = Client(gate, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err = Server(conn0, cfg)
	if err != nil {
		t.Fatal(err)
	}

	cleanup = func() {
		gate.release()
		cli.Close()
		srv.Close()
		conn0.Close()
		conn1.Close()
	}
	return cli, srv, gate, cleanup
}

// startEchoServer accepts streams from srv and echoes data until EOF/error.
// Every fully accepted stream is reported on the returned channel.
func startEchoServer(t *testing.T, srv *Session) <-chan *Stream {
	t.Helper()
	ch := make(chan *Stream, 128)
	go func() {
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			ch <- st
			go func(s *Stream) {
				io.Copy(s, s)
				s.Close()
			}(st)
		}
	}()
	return ch
}

// waitForAccepted returns the accepted server stream whose ID matches sid.
func waitForAccepted(t *testing.T, ch <-chan *Stream, sid uint32) *Stream {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case st := <-ch:
			if st.ID() == sid {
				return st
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("server never accepted stream %d", sid)
		}
	}
}

// waitForEvent waits until the gate has observed an event matching pred.
func waitForEvent(t *testing.T, gate *gateConn, pred func(frameEvent) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, ev := range gate.snapshot() {
			if pred(ev) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; observed=%v", what, gate.snapshot())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// finAfterSyn asserts that for sid a FIN was observed after its SYN on the wire.
func finAfterSyn(t *testing.T, gate *gateConn, sid uint32) {
	t.Helper()
	events := gate.snapshot()
	synIdx, finIdx := -1, -1
	for i, ev := range events {
		if ev.sid != sid {
			continue
		}
		if ev.cmd == cmdSYN && synIdx < 0 {
			synIdx = i
		}
		if ev.cmd == cmdFIN {
			finIdx = i
		}
	}
	if synIdx < 0 {
		t.Fatalf("expected SYN for stream %d, observed=%v", sid, events)
	}
	if finIdx < 0 {
		t.Fatalf("expected FIN for stream %d, observed=%v", sid, events)
	}
	if finIdx <= synIdx {
		t.Fatalf("FIN(%d) must follow SYN(%d), observed=%v", finIdx, synIdx, events)
	}
}

// noEventFor asserts that no frame for sid has been observed on the wire.
func noEventFor(t *testing.T, gate *gateConn, sid uint32) {
	t.Helper()
	for _, ev := range gate.snapshot() {
		if ev.sid == sid {
			t.Fatalf("did not expect any frame for stream %d, observed=%v", sid, gate.snapshot())
		}
	}
}

// echoRoundTrip verifies a stream can write data and read the echo back.
func echoRoundTrip(t *testing.T, st *Stream, tag string) {
	t.Helper()
	payload := []byte("ping-" + tag)
	if _, err := st.Write(payload); err != nil {
		t.Fatalf("%s: write: %v", tag, err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(st, buf); err != nil {
		t.Fatalf("%s: read echo: %v", tag, err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("%s: echo mismatch %q", tag, buf)
	}
}

// nextOpenID reports the stream id that the next open attempt will allocate.
func nextOpenID(s *Session) uint32 {
	s.nextStreamIDLock.Lock()
	defer s.nextStreamIDLock.Unlock()
	return s.nextStreamID + 2
}

// openResult is one outcome of an OpenStreamContext invocation.
type openResult struct {
	st  *Stream
	err error
}

// openContextAsync invokes OpenStreamContext and reports on a buffered
// channel, allowing tests to assert that exactly one result is ever returned.
func openContextAsync(s *Session, ctx context.Context) chan openResult {
	ch := make(chan openResult, 2)
	go func() {
		st, err := s.OpenStreamContext(ctx)
		ch <- openResult{st: st, err: err}
	}()
	return ch
}

// TestOpenStreamContextCancelBeforeSYN covers the case where the SYN control
// frame has not yet been committed to the writer: the blocking control-frame
// writer saturates the shaper so the submit blocks, then the context is
// cancelled. Nothing must be sent for that id, the id must not be reused, and
// later streams must keep working.
func TestOpenStreamContextCancelBeforeSYN(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(versionName(version), func(t *testing.T) {
			cli, srv, gate, cleanup := ctxPair(t, version)
			defer cleanup()
			accepted := startEchoServer(t, srv)

			// Block the writer and fill both the shaper queue and the shaper
			// input channel, so the next submit cannot commit.
			gate.block()
			fillDone := saturateShaper(t, cli, version)

			skippedSid := nextOpenID(cli)
			ctx, cancel := context.WithCancel(context.Background())
			resCh := openContextAsync(cli, ctx)
			cancel()

			select {
			case res := <-resCh:
				if !errors.Is(res.err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", res.err)
				}
				if res.st != nil {
					t.Fatal("no stream should be returned")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("OpenStreamContext did not return after cancel")
			}

			// Drain the filler and the backlog, then prove a later open still
			// works and picked a fresh id.
			gate.release()
			select {
			case <-fillDone:
			case <-time.After(3 * time.Second):
				t.Fatal("filler never finished")
			}

			good, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			defer good.Close()
			if good.ID() == skippedSid {
				t.Fatalf("cancelled stream id %d was reused", skippedSid)
			}
			if good.ID() != skippedSid+2 {
				t.Fatalf("expected next id %d, got %d", skippedSid+2, good.ID())
			}
			waitForEvent(t, gate, func(ev frameEvent) bool {
				return ev.cmd == cmdSYN && ev.sid == good.ID()
			}, "SYN for later stream")
			noEventFor(t, gate, skippedSid)

			srvSt := waitForAccepted(t, accepted, good.ID())
			defer srvSt.Close()
			echoRoundTrip(t, good, "post-cancel")
		})
	}
}

// TestOpenStreamContextCancelAfterSYN covers the case where SYN has been
// committed (it is held in the blocked writer) but the local stream has not
// been returned: cancellation must still queue a FIN behind the SYN so the
// protocol-visible stream is closed on the peer, return exactly one result,
// never register the stream locally, leave the id unreused, and leave other
// streams fully usable.
func TestOpenStreamContextCancelAfterSYN(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(versionName(version), func(t *testing.T) {
			cli, srv, gate, cleanup := ctxPair(t, version)
			defer cleanup()
			accepted := startEchoServer(t, srv)

			// A normal stream opened before the writer blocks must stay alive.
			other, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			otherSrv := waitForAccepted(t, accepted, other.ID())
			defer otherSrv.Close()
			echoRoundTrip(t, other, "before")

			cancelSid := nextOpenID(cli)
			ctx, cancel := context.WithCancel(context.Background())
			_, entered := gate.arm(func(cmd byte, sid uint32) bool {
				return cmd == cmdSYN && sid == cancelSid
			})
			resCh := openContextAsync(cli, ctx)

			// The SYN is committed to the writer and parked, but the open has
			// not returned yet: this is the "committed, not returned" point.
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("writer never parked on the SYN")
			}
			// Recorded before the underlying write while parked.
			waitForEvent(t, gate, func(ev frameEvent) bool {
				return ev.cmd == cmdSYN && ev.sid == cancelSid
			}, "SYN recorded for cancelled stream")

			cancel()
			select {
			case res := <-resCh:
				if !errors.Is(res.err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", res.err)
				}
				if res.st != nil {
					t.Fatal("cancelled open must not return a stream")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("OpenStreamContext did not return after cancel")
			}

			// Exactly one result: the channel is buffered with room for two.
			select {
			case extra := <-resCh:
				t.Fatalf("OpenStreamContext returned more than once: %+v", extra)
			default:
			}

			// The cancelled stream must never have been registered locally.
			cli.streamLock.Lock()
			_, registered := cli.streams[cancelSid]
			cli.streamLock.Unlock()
			if registered {
				t.Fatal("cancelled stream was registered in the local map")
			}

			// Releasing the writer flushes SYN then FIN, in that order.
			gate.release()
			waitForEvent(t, gate, func(ev frameEvent) bool {
				return ev.cmd == cmdFIN && ev.sid == cancelSid
			}, "FIN for cancelled stream")
			finAfterSyn(t, gate, cancelSid)

			// The peer accepts the stream and observes EOF from our FIN.
			srvSt := waitForAccepted(t, accepted, cancelSid)
			buf := make([]byte, 8)
			if n, err := srvSt.Read(buf); err != io.EOF || n != 0 {
				t.Fatalf("peer expected EOF, got n=%d err=%v", n, err)
			}

			// The other stream is still fully usable.
			echoRoundTrip(t, other, "after")

			// The id is not reused: a fresh open advances past cancelSid.
			fresh, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			if fresh.ID() <= cancelSid {
				t.Fatalf("expected id larger than %d, got %d", cancelSid, fresh.ID())
			}
			freshSrv := waitForAccepted(t, accepted, fresh.ID())
			defer freshSrv.Close()
			echoRoundTrip(t, fresh, "fresh")

			// Once the echo server closes the aborted peer stream, neither
			// session should track it anymore.
			waitUntil(t, func() bool {
				srv.streamLock.Lock()
				_, ok := srv.streams[cancelSid]
				srv.streamLock.Unlock()
				return !ok
			}, "server cleanup of aborted stream")
			cli.streamLock.Lock()
			_, ok := cli.streams[cancelSid]
			cli.streamLock.Unlock()
			if ok {
				t.Fatal("client still tracks aborted stream")
			}
		})
	}
}

// TestOpenStreamContextCancelAfterReturn covers cancellation after success:
// the stream is returned exactly once and remains the caller's responsibility;
// cancelling the context must not tear it down or emit a FIN.
func TestOpenStreamContextCancelAfterReturn(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(versionName(version), func(t *testing.T) {
			cli, srv, gate, cleanup := ctxPair(t, version)
			defer cleanup()
			accepted := startEchoServer(t, srv)

			ctx, cancel := context.WithCancel(context.Background())
			st, err := cli.OpenStreamContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			srvSt := waitForAccepted(t, accepted, st.ID())
			defer srvSt.Close()

			cancel()

			// The returned stream keeps working after cancellation.
			echoRoundTrip(t, st, "after-cancel")

			// Cancellation alone must not produce a FIN for this stream.
			time.Sleep(50 * time.Millisecond)
			for _, ev := range gate.snapshot() {
				if ev.sid == st.ID() && ev.cmd == cmdFIN {
					t.Fatal("cancellation after return must not send FIN")
				}
			}

			st.Close()
			waitForEvent(t, gate, func(ev frameEvent) bool {
				return ev.cmd == cmdFIN && ev.sid == st.ID()
			}, "FIN from explicit close")
		})
	}
}

// TestOpenStreamContextSessionDying verifies that closing the session while a
// SYN is held in the blocked writer unblocks the open with an error instead
// of hanging forever.
func TestOpenStreamContextSessionDying(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(versionName(version), func(t *testing.T) {
			cli, _, gate, cleanup := ctxPair(t, version)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, entered := gate.arm(func(cmd byte, _ uint32) bool {
				return cmd == cmdSYN
			})
			resCh := openContextAsync(cli, ctx)

			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("writer never parked on the SYN")
			}

			cli.Close()
			select {
			case res := <-resCh:
				if res.err == nil {
					t.Fatal("expected error after session close")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("OpenStreamContext did not unblock on session close")
			}
		})
	}
}

// saturateShaper fills the shaper queue and input channel while the writer is
// blocked, using harmless NOP control frames. The returned channel closes once
// both buffers are full.
func saturateShaper(t *testing.T, s *Session, version int) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(3 * time.Second)
		for {
			if saturated(s) {
				return
			}
			if time.Now().After(deadline) {
				return
			}
			req := writeRequest{
				class:  CLSCTRL,
				frame:  newFrame(byte(version), cmdNOP, 0),
				seq:    atomic.AddUint32(&s.requestID, 1),
				result: make(chan writeResult, 1),
			}
			select {
			case s.shaper <- req:
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	// Wait until the shaper is fully saturated before returning.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if saturated(s) {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatalf("shaper never saturated: chan=%d queue=%d", len(s.shaper), s.sq.Len())
		}
		time.Sleep(time.Millisecond)
	}
}

// saturated reports whether the shaper cannot accept another submit: its input
// channel is full and a deep queue backs it up. The queue reaches at most
// maxShaperSize-1 because one request is held in flight inside shaperLoop, and
// the blocked writer drains nothing, so a full input channel cannot free up.
func saturated(s *Session) bool {
	return len(s.shaper) >= cap(s.shaper) && s.sq.Len() >= maxShaperSize-2
}

// waitUntil polls cond until it becomes true or the deadline elapses.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func versionName(version int) string {
	if version == 2 {
		return "v2"
	}
	return "v1"
}

// TestOpenStreamDeadlinePreserved ensures the legacy OpenStream deadline path
// still returns ErrTimeout when the SYN cannot complete in time. openStream is
// exercised directly with a short deadline channel that mirrors how
// OpenStream wires openCloseTimeout.
func TestOpenStreamDeadlinePreserved(t *testing.T) {
	cli, _, gate, cleanup := ctxPair(t, 1)
	defer cleanup()

	gate.block()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	done := make(chan openResult, 1)
	go func() {
		st, err := cli.openStream(context.Background(), timer.C)
		done <- openResult{st: st, err: err}
	}()
	select {
	case res := <-done:
		if !errors.Is(res.err, ErrTimeout) {
			t.Fatalf("expected ErrTimeout, got %v", res.err)
		}
		if res.st != nil {
			t.Fatal("no stream should be returned")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("openStream did not honor deadline")
	}
}
