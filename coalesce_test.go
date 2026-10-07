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

package smux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// boundaryConn is a fake connection that records the boundaries of every
// Write call, so tests can verify how frames were coalesced.
type boundaryConn struct {
	mu        sync.Mutex
	writes    [][]byte
	die       chan struct{}
	closeOnce sync.Once
	writeErr  error // if set, Write fails with this error
	shortOnce bool  // if set, the next Write returns a short count
}

func newBoundaryConn() *boundaryConn {
	return &boundaryConn{die: make(chan struct{})}
}

func (c *boundaryConn) Read(b []byte) (int, error) {
	<-c.die
	return 0, io.EOF
}

func (c *boundaryConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	c.writes = append(c.writes, cp)
	if c.shortOnce {
		c.shortOnce = false
		return len(b) / 2, nil
	}
	return len(b), nil
}

func (c *boundaryConn) Close() error {
	c.closeOnce.Do(func() { close(c.die) })
	return nil
}

func (c *boundaryConn) numWrites() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

func (c *boundaryConn) getWrites() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	copy(out, c.writes)
	return out
}

func (c *boundaryConn) setWriteErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeErr = err
}

func (c *boundaryConn) setShortOnce() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shortOnce = true
}

// fakeClock is a controllable clock for the coalescer commit timer.
type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	ch     chan time.Time
	active bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{}
}

func (c *fakeClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	t := &fakeTimer{ch: make(chan time.Time, 1), active: true}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	return t.ch, func() {
		c.mu.Lock()
		t.active = false
		c.mu.Unlock()
	}
}

// fire expires all active timers.
func (c *fakeClock) fire() {
	c.mu.Lock()
	timers := c.timers
	c.timers = nil
	for _, t := range timers {
		if t.active {
			t.active = false
			t.ch <- time.Now()
		}
	}
	c.mu.Unlock()
}

// pending returns the number of active timers.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}

func waitForCondition(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for:", msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// decodeWireFrames parses a coalesced write back into frames.
func decodeWireFrames(t *testing.T, data []byte) []Frame {
	t.Helper()
	var frames []Frame
	for len(data) > 0 {
		if len(data) < headerSize {
			t.Fatalf("truncated frame header: %d bytes left", len(data))
		}
		var hdr rawHeader
		copy(hdr[:], data[:headerSize])
		length := int(hdr.Length())
		if len(data) < headerSize+length {
			t.Fatalf("truncated frame payload: need %d, have %d", length, len(data)-headerSize)
		}
		frames = append(frames, Frame{
			ver:  hdr.Version(),
			cmd:  hdr.Cmd(),
			sid:  hdr.StreamID(),
			data: data[headerSize : headerSize+length],
		})
		data = data[headerSize+length:]
	}
	return frames
}

func coalesceTestConfig(version int) *Config {
	config := DefaultConfig()
	config.Version = version
	config.KeepAliveDisabled = true
	config.WriteCoalesceBytes = 1 << 20
	config.WriteCoalesceDelay = time.Hour // commits are triggered manually
	return config
}

// TestCoalesceMergesSmallFrames verifies that ready frames below the byte
// threshold are combined into a single Write on the underlying connection.
func TestCoalesceMergesSmallFrames(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.WriteCoalesceBytes = 2 * (headerSize + 4) // exactly two 4-byte frames
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// SYN is a control frame and must be written immediately
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := stream.Write([]byte("aaaa")); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := stream.Write([]byte("bbbb")); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	// the byte threshold of the two frames triggered the commit,
	// the commit timer must have been stopped
	waitForCondition(t, "batched write", func() bool { return conn.numWrites() == 2 })
	if n := clock.pending(); n != 0 {
		t.Fatalf("commit timer still pending after commit: %d", n)
	}

	frames := decodeWireFrames(t, conn.getWrites()[1])
	if len(frames) != 2 {
		t.Fatalf("expected 2 coalesced frames in one write, got %d", len(frames))
	}
	var payloads [][]byte
	for _, f := range frames {
		if f.cmd != cmdPSH {
			t.Fatalf("expected PSH frame, got cmd %d", f.cmd)
		}
		if f.sid != stream.ID() {
			t.Fatalf("expected sid %d, got %d", stream.ID(), f.sid)
		}
		payloads = append(payloads, f.data)
	}
	if !bytes.Equal(payloads[0], []byte("aaaa")) && !bytes.Equal(payloads[1], []byte("aaaa")) {
		t.Fatal("payload aaaa missing")
	}
	if !bytes.Equal(payloads[0], []byte("bbbb")) && !bytes.Equal(payloads[1], []byte("bbbb")) {
		t.Fatal("payload bbbb missing")
	}
}

// TestCoalesceTimerFlush verifies that the commit timer flushes an
// uncommitted batch after the configured delay.
func TestCoalesceTimerFlush(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("hello"))
		done <- err
	}()

	// the batch is pending but must not be written before the timer fires
	waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })
	if n := conn.numWrites(); n != 1 {
		t.Fatalf("batch committed before the timer fired: %d writes", n)
	}

	clock.fire()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "flushed write", func() bool { return conn.numWrites() == 2 })

	frames := decodeWireFrames(t, conn.getWrites()[1])
	if len(frames) != 1 || frames[0].cmd != cmdPSH || !bytes.Equal(frames[0].data, []byte("hello")) {
		t.Fatalf("unexpected frames: %+v", frames)
	}
}

// TestCoalesceSingleWriteNotStuck verifies that a single low-traffic write
// is committed by the (real) commit timer and does not wait forever.
func TestCoalesceSingleWriteNotStuck(t *testing.T) {
	conn := newBoundaryConn()
	config := coalesceTestConfig(1)
	config.WriteCoalesceDelay = 20 * time.Millisecond

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("ping"))
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("single low-traffic write was not committed in time")
	}
}

// TestCoalesceFINOrdering verifies that a FIN triggers a commit and never
// overtakes earlier data of the same stream.
func TestCoalesceFINOrdering(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	writeDone := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("last words"))
		writeDone <- err
	}()
	waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })

	// closing the stream sends FIN, which must commit the pending data first
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, "data and FIN writes", func() bool { return conn.numWrites() == 3 })
	writes := conn.getWrites()

	dataFrames := decodeWireFrames(t, writes[1])
	if len(dataFrames) != 1 || dataFrames[0].cmd != cmdPSH ||
		!bytes.Equal(dataFrames[0].data, []byte("last words")) {
		t.Fatalf("expected pending data to be committed first, got %+v", dataFrames)
	}
	finFrames := decodeWireFrames(t, writes[2])
	if len(finFrames) != 1 || finFrames[0].cmd != cmdFIN || finFrames[0].sid != stream.ID() {
		t.Fatalf("expected FIN after the data, got %+v", finFrames)
	}
}

// TestCoalesceControlFrameCommits verifies that a control frame requiring
// immediate handling commits the pending batch first.
func TestCoalesceControlFrameCommits(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream1, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	writeDone := make(chan error, 1)
	go func() {
		_, err := stream1.Write([]byte("pending"))
		writeDone <- err
	}()
	waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })

	// opening another stream sends a SYN, which must commit the pending batch
	stream2, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = stream2
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, "batch and SYN writes", func() bool { return conn.numWrites() == 3 })
	writes := conn.getWrites()
	dataFrames := decodeWireFrames(t, writes[1])
	if len(dataFrames) != 1 || dataFrames[0].cmd != cmdPSH {
		t.Fatalf("expected pending data to be committed before SYN, got %+v", dataFrames)
	}
	synFrames := decodeWireFrames(t, writes[2])
	if len(synFrames) != 1 || synFrames[0].cmd != cmdSYN {
		t.Fatalf("expected SYN after the batch, got %+v", synFrames)
	}
}

// TestCoalesceShortWrite verifies that a short write is reported as
// io.ErrShortWrite to the pending writers.
func TestCoalesceShortWrite(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	conn.setShortOnce()

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("hello"))
		done <- err
	}()
	waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })
	clock.fire()

	if err := <-done; !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
}

// TestCoalesceWriteError verifies that a connection write failure is
// delivered to the writers of the pending batch.
func TestCoalesceWriteError(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	errBoom := errors.New("boom")
	conn.setWriteErr(errBoom)

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("hello"))
		done <- err
	}()
	waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })
	clock.fire()

	if err := <-done; !errors.Is(err, errBoom) {
		t.Fatalf("expected boom, got %v", err)
	}
}

// TestCoalesceCloseWithPendingBatch verifies that requests still waiting in
// an uncommitted batch are failed with io.ErrClosedPipe when the session
// is closed.
func TestCoalesceCloseWithPendingBatch(t *testing.T) {
	conn := newBoundaryConn()
	clock := newFakeClock()
	config := coalesceTestConfig(1)
	config.newBatchTimer = clock.newTimer

	sess, err := Client(conn, config)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("never sent"))
		done <- err
	}()
	waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })

	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe, got %v", err)
	}
}

// TestCoalesceWireFormat verifies that coalesced output decodes correctly
// for both protocol versions.
func TestCoalesceWireFormat(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			conn := newBoundaryConn()
			clock := newFakeClock()
			config := coalesceTestConfig(version)
			config.newBatchTimer = clock.newTimer

			sess, err := Client(conn, config)
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()

			stream, err := sess.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, "SYN write", func() bool { return conn.numWrites() == 1 })

			done := make(chan error, 1)
			go func() {
				_, err := stream.Write([]byte("smux"))
				done <- err
			}()
			waitForCondition(t, "commit timer armed", func() bool { return clock.pending() == 1 })
			clock.fire()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			waitForCondition(t, "batched write", func() bool { return conn.numWrites() == 2 })

			writes := conn.getWrites()
			syn := decodeWireFrames(t, writes[0])
			if len(syn) != 1 || syn[0].ver != byte(version) || syn[0].cmd != cmdSYN {
				t.Fatalf("bad SYN frame: %+v", syn)
			}
			psh := decodeWireFrames(t, writes[1])
			if len(psh) != 1 || psh[0].ver != byte(version) || psh[0].cmd != cmdPSH ||
				psh[0].sid != stream.ID() || !bytes.Equal(psh[0].data, []byte("smux")) {
				t.Fatalf("bad PSH frame: %+v", psh)
			}
		})
	}
}

// TestCoalesceEcho runs an end-to-end echo over net.Pipe with coalescing
// enabled on the client side, for both protocol versions.
func TestCoalesceEcho(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			c1, c2 := net.Pipe()

			serverConfig := DefaultConfig()
			serverConfig.Version = version
			serverConfig.KeepAliveDisabled = true
			srv, err := Server(c2, serverConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			go func() {
				for {
					stream, err := srv.AcceptStream()
					if err != nil {
						return
					}
					go func(s *Stream) {
						buf := make([]byte, 65536)
						for {
							n, err := s.Read(buf)
							if err != nil {
								return
							}
							if _, err := s.Write(buf[:n]); err != nil {
								return
							}
						}
					}(stream)
				}
			}()

			clientConfig := coalesceTestConfig(version)
			clientConfig.WriteCoalesceBytes = 1024
			clientConfig.WriteCoalesceDelay = 5 * time.Millisecond
			cli, err := Client(c1, clientConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()

			stream, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()

			for i := 0; i < 20; i++ {
				msg := []byte(fmt.Sprintf("coalesced-message-%d", i))
				if _, err := stream.Write(msg); err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, len(msg))
				if _, err := io.ReadFull(stream, buf); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf, msg) {
					t.Fatalf("echo mismatch: got %q, want %q", buf, msg)
				}
			}
		})
	}
}

// TestPopStreamBefore verifies that the shaper queue can extract earlier
// frames of a stream in sequence order.
func TestPopStreamBefore(t *testing.T) {
	sq := NewShaperQueue()
	mkreq := func(sid uint32, seq uint32) writeRequest {
		return writeRequest{
			class: CLSDATA,
			frame: newFrame(1, cmdPSH, sid),
			seq:   seq,
		}
	}
	sq.Push(mkreq(7, 1))
	sq.Push(mkreq(9, 2))
	sq.Push(mkreq(7, 3))
	sq.Push(mkreq(7, 5))
	sq.Push(mkreq(9, 4))

	// a control frame for stream 7 with seq 6 is about to be written;
	// all earlier frames of stream 7 must be extracted, in order
	extracted := sq.PopStreamBefore(7, 6)
	if len(extracted) != 3 {
		t.Fatalf("expected 3 extracted requests, got %d", len(extracted))
	}
	for i, seq := range []uint32{1, 3, 5} {
		if extracted[i].seq != seq || extracted[i].frame.sid != 7 {
			t.Fatalf("extracted[%d]: expected sid 7 seq %d, got sid %d seq %d",
				i, seq, extracted[i].frame.sid, extracted[i].seq)
		}
	}
	if sq.Len() != 2 {
		t.Fatalf("expected 2 requests left, got %d", sq.Len())
	}

	// nothing earlier remains for stream 7
	if got := sq.PopStreamBefore(7, 6); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}

	// remaining requests still pop in round-robin order
	first, ok := sq.Pop()
	if !ok || first.frame.sid != 9 {
		t.Fatalf("expected stream 9 request, got %+v", first)
	}
	second, ok := sq.Pop()
	if !ok || second.frame.sid != 9 {
		t.Fatalf("expected stream 9 request, got %+v", second)
	}
	if !sq.IsEmpty() {
		t.Fatal("queue should be empty")
	}

	// the stream can be reused after full extraction
	sq.Push(mkreq(7, 10))
	if got := sq.PopStreamBefore(7, 11); len(got) != 1 {
		t.Fatalf("expected 1 extracted request, got %d", len(got))
	}
}
