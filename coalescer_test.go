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

// recordedWrite records the exact byte slice boundaries of one conn.Write
// call, so tests can verify that frames were merged into a single write.
type recordedWrite struct {
	data []byte
	err  error
}

// recordConn is an io.ReadWriteCloser that records every write boundary.
// Read blocks until the connection is closed, emulating an idle peer.
type recordConn struct {
	mu       sync.Mutex
	writes   []recordedWrite
	die      chan struct{}
	shortNth int // 1-based: the n-th write is short (3 bytes, no error) once
	failNth  int // 1-based: the n-th effective write returns failErr
	calls    int
	failErr  error
}

func newRecordConn() *recordConn {
	return &recordConn{
		die:     make(chan struct{}),
		failErr: errors.New("recordConn: write failure"),
	}
}

func (c *recordConn) Read(b []byte) (int, error) {
	<-c.die
	return 0, io.EOF
}

func (c *recordConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.calls++
	fail := c.failNth == c.calls
	short := c.shortNth == c.calls
	n := len(b)
	var werr error
	if short && n > 3 {
		n = 3
	}
	if fail {
		werr = c.failErr
		if n == len(b) {
			n = 0
		}
	}
	c.writes = append(c.writes, recordedWrite{data: append([]byte(nil), b[:n]...), err: werr})
	c.mu.Unlock()
	return n, werr
}

func (c *recordConn) Close() error {
	select {
	case <-c.die:
	default:
		close(c.die)
	}
	return nil
}

func (c *recordConn) snapshot() []recordedWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]recordedWrite, len(c.writes))
	copy(out, c.writes)
	return out
}

// fakeTimer is a controllable timer driven by fakeClock.
type fakeTimer struct {
	c      *fakeClock
	ch     chan time.Time
	d      time.Duration
	active bool
}

func (t *fakeTimer) Ch() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(d time.Duration) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	// drain a stale value, if any
	select {
	case <-t.ch:
	default:
	}
	t.d = d
	t.active = true
}

func (t *fakeTimer) Stop() {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if !t.active {
		return
	}
	t.active = false
	select {
	case <-t.ch:
	default:
	}
}

// fakeClock hands out fakeTimers and fires them with Advance.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (c *fakeClock) NewTimer(d time.Duration) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	ft := &fakeTimer{c: c, ch: make(chan time.Time, 1), d: d, active: false}
	c.timers = append(c.timers, ft)
	return ft
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []*fakeTimer
	for _, t := range c.timers {
		if t.active {
			due = append(due, t)
		}
	}
	c.mu.Unlock()

	for _, t := range due {
		t.c.mu.Lock()
		if !t.active {
			t.c.mu.Unlock()
			continue
		}
		t.active = false
		select {
		case t.ch <- now:
		default:
		}
		t.c.mu.Unlock()
	}
}

func coalesceConfig(version int, bytes int, delay time.Duration) *Config {
	cfg := DefaultConfig()
	cfg.Version = version
	cfg.KeepAliveDisabled = true
	cfg.WriteCoalesceBytes = bytes
	cfg.WriteCoalesceDelay = delay
	return cfg
}

func newCoalesceSession(t *testing.T, conn io.ReadWriteCloser, version int, bytes int, delay time.Duration) (*Session, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	cfg := coalesceConfig(version, bytes, delay)
	if err := VerifyConfig(cfg); err != nil {
		t.Fatalf("bad config: %v", err)
	}
	s := newSessionWithClock(cfg, conn, true, clk)
	t.Cleanup(func() { s.Close() })
	return s, clk
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
	t.Fatalf("timed out waiting for %s", msg)
}

// parseFrames decodes complete frames from a byte stream, each 8-byte header
// followed by its payload.
func parseFrames(t *testing.T, b []byte) []Frame {
	t.Helper()
	var frames []Frame
	for len(b) >= headerSize {
		var hdr rawHeader
		copy(hdr[:], b[:headerSize])
		n := int(hdr.Length())
		if len(b) < headerSize+n {
			t.Fatalf("truncated frame: header says %d payload bytes, %d remain", n, len(b)-headerSize)
		}
		f := Frame{
			ver:  hdr.Version(),
			cmd:  hdr.Cmd(),
			sid:  hdr.StreamID(),
			data: append([]byte(nil), b[headerSize:headerSize+n]...),
		}
		frames = append(frames, f)
		b = b[headerSize+n:]
	}
	if len(b) != 0 {
		t.Fatalf("%d stray bytes remain after parsing frames", len(b))
	}
	return frames
}

func payloads(frames []Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, string(f.data))
	}
	return out
}

// TestCoalesceSmallWritesMerged verifies that several small ready frames are
// combined into a single underlying write once the timer fires, and that
// waiting does not stall before the timer is advanced.
func TestCoalesceSmallWritesMerged(t *testing.T) {
	conn := newRecordConn()
	s, clk := newCoalesceSession(t, conn, 1, 4096, 10*time.Millisecond)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, msg := range []string{"a", "bb", "ccc"} {
		wg.Add(1)
		go func(msg string) {
			defer wg.Done()
			_, err := stream.Write([]byte(msg))
			errs <- err
		}(msg)
	}

	// two writes at least: SYN then the batch of three PSH frames
	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 3 }, "three PSH frames pending")
	if ws := conn.snapshot(); len(ws) != 1 {
		t.Fatalf("expected only SYN write before timer fires, got %d writes", len(ws))
	}

	clk.Advance(10 * time.Millisecond)

	wg.Wait()
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("write returned error: %v", err)
		}
	}

	writes := conn.snapshot()
	if len(writes) != 2 {
		t.Fatalf("expected 2 underlying writes (SYN + batch), got %d", len(writes))
	}
	frames := parseFrames(t, writes[1].data)
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames in batch write, got %d", len(frames))
	}
	// Concurrent writes to one stream may interleave; assert the merged
	// batch contains every complete payload, none split or duplicated.
	got := payloads(frames)
	want := map[string]int{"a": 1, "bb": 1, "ccc": 1}
	for _, p := range got {
		if want[p] == 0 {
			t.Fatalf("unexpected payload %q in batch %q", p, got)
		}
		want[p]--
	}
	for _, f := range frames {
		if f.ver != 1 || f.cmd != cmdPSH {
			t.Fatalf("unexpected frame in batch: %+v", f)
		}
	}
}

// TestCoalesceByteThreshold verifies synchronous commit once the byte
// threshold is reached, without waiting for the timer.
func TestCoalesceByteThreshold(t *testing.T) {
	conn := newRecordConn()
	// threshold small enough that one frame of 100 bytes commits immediately
	s, _ := newCoalesceSession(t, conn, 1, 50, time.Hour)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := stream.Write(bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}

	writes := conn.snapshot()
	if len(writes) != 2 {
		t.Fatalf("expected SYN + one threshold commit, got %d writes", len(writes))
	}
	frames := parseFrames(t, writes[1].data)
	if len(frames) != 1 || len(frames[0].data) != 100 {
		t.Fatalf("unexpected batch content: %+v", frames)
	}
}

// TestCoalesceSingleWriteBounded verifies a single low-traffic write does not
// wait forever: it is committed when the delay elapses and no more frames
// arrive.
func TestCoalesceSingleWriteBounded(t *testing.T) {
	conn := newRecordConn()
	s, clk := newCoalesceSession(t, conn, 1, 4096, 5*time.Millisecond)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("lonely"))
		done <- err
	}()

	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "single PSH pending")

	select {
	case err := <-done:
		t.Fatalf("write should still be waiting before timer, got %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	clk.Advance(5 * time.Millisecond)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write after timer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("single write was not committed after delay")
	}

	writes := conn.snapshot()
	if len(writes) != 2 {
		t.Fatalf("expected SYN + single frame write, got %d", len(writes))
	}
}

// TestCoalesceFINOrder verifies a FIN never overtakes earlier data of the
// same stream: pending data is committed first, FIN second.
func TestCoalesceFINOrder(t *testing.T) {
	conn := newRecordConn()
	s, _ := newCoalesceSession(t, conn, 1, 4096, time.Hour)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("data"))
		writeDone <- err
	}()

	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "PSH pending before FIN")

	// Close enqueues a FIN for the same stream. The shaper keeps FIN behind
	// the PSH, and sendLoop must flush the batch before writing the FIN.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("pending data write failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending data was not flushed by FIN")
	}

	writes := conn.snapshot()
	if len(writes) != 3 {
		t.Fatalf("expected SYN, data batch and FIN writes, got %d", len(writes))
	}

	frames := parseFrames(t, writes[1].data)
	if len(frames) != 1 || frames[0].cmd != cmdPSH || string(frames[0].data) != "data" {
		t.Fatalf("expected data committed before FIN, got %+v", frames)
	}

	fin := parseFrames(t, writes[2].data)
	if len(fin) != 1 || fin[0].cmd != cmdFIN || fin[0].sid != frames[0].sid {
		t.Fatalf("expected standalone FIN for same stream, got %+v", fin)
	}
}

// TestCoalesceControlFlush verifies a control frame forces an immediate
// commit of the pending batch and goes out in its own write.
func TestCoalesceControlFlush(t *testing.T) {
	conn := newRecordConn()
	s, _ := newCoalesceSession(t, conn, 1, 4096, time.Hour)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		stream.Write([]byte("x"))
	}()

	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "PSH pending")

	nop := newFrame(1, cmdNOP, 0)
	if _, err := s.writeControlFrame(nop); err != nil {
		t.Fatal(err)
	}

	waitFor(t, time.Second, func() bool { return len(conn.snapshot()) >= 3 }, "batch + NOP written")
	writes := conn.snapshot()
	if len(writes) != 3 {
		t.Fatalf("expected SYN, flushed batch and NOP writes, got %d", len(writes))
	}
	if f := parseFrames(t, writes[1].data); len(f) != 1 || f[0].cmd != cmdPSH {
		t.Fatalf("write 2 should be the flushed PSH, got %+v", f)
	}
	if f := parseFrames(t, writes[2].data); len(f) != 1 || f[0].cmd != cmdNOP {
		t.Fatalf("write 3 should be the NOP, got %+v", f)
	}
}

// TestCoalesceShortWrite verifies a short write without error is retried
// until the whole batch is delivered; write boundaries still decode into the
// same complete frames.
func TestCoalesceShortWrite(t *testing.T) {
	conn := newRecordConn()
	conn.shortNth = 2 // SYN is call #1, the batch write is short once
	s, clk := newCoalesceSession(t, conn, 1, 4096, 5*time.Millisecond)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("abcdef"))
		done <- err
	}()

	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "PSH pending")
	clk.Advance(5 * time.Millisecond)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("short write should have been retried to completion, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not complete after short-write retries")
	}

	writes := conn.snapshot()
	if len(writes) < 3 {
		t.Fatalf("expected SYN plus at least two short-write fragments, got %d writes", len(writes))
	}

	var all bytes.Buffer
	for _, w := range writes[1:] {
		all.Write(w.data)
	}
	frames := parseFrames(t, all.Bytes())
	if len(frames) != 1 || string(frames[0].data) != "abcdef" {
		t.Fatalf("reassembled payload mismatch: %+v", frames)
	}
}

// TestCoalesceWriteError verifies a write failure is delivered to the writer
// and tears the session down so later writes fail immediately.
func TestCoalesceWriteError(t *testing.T) {
	conn := newRecordConn()
	// SYN is call #1; the batch commit is call #2 and must fail.
	conn.failNth = 2
	s, clk := newCoalesceSession(t, conn, 1, 4096, 5*time.Millisecond)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	writeErr := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("boom"))
		writeErr <- err
	}()

	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "PSH pending")
	clk.Advance(5 * time.Millisecond)

	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("expected write error to be delivered, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("write error was not delivered")
	}

	waitFor(t, time.Second, func() bool {
		select {
		case <-s.chSocketWriteError:
			return true
		default:
			return false
		}
	}, "socket write error signaled")

	if _, err := stream.Write([]byte("more")); err == nil {
		t.Fatal("write after socket failure must return an error")
	}
}

// TestCoalesceCloseWithPending verifies uncommitted data at session close is
// delivered an error instead of hanging or being silently dropped.
func TestCoalesceCloseWithPending(t *testing.T) {
	conn := newRecordConn()
	s, _ := newCoalesceSession(t, conn, 1, 4096, time.Hour)

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	writeErr := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("stuck"))
		writeErr <- err
	}()

	waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "PSH pending at close")

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-writeErr:
		if err != io.ErrClosedPipe {
			t.Fatalf("expected io.ErrClosedPipe for pending data at close, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending write was not failed at session close")
	}
}

// TestCoalesceDecodableV1V2 verifies the coalesced byte stream decodes under
// both protocol versions without any wire-format change.
func TestCoalesceDecodableV1V2(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			conn := newRecordConn()
			s, clk := newCoalesceSession(t, conn, version, 4096, 5*time.Millisecond)

			stream, err := s.OpenStream()
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				_, err := stream.Write([]byte("hello"))
				done <- err
			}()

			waitFor(t, time.Second, func() bool { return s.coalescer.pending() == 1 }, "PSH pending")
			clk.Advance(5 * time.Millisecond)
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			writes := conn.snapshot()
			if len(writes) != 2 {
				t.Fatalf("expected SYN + batch, got %d writes", len(writes))
			}

			syn := parseFrames(t, writes[0].data)
			if len(syn) != 1 || syn[0].ver != byte(version) || syn[0].cmd != cmdSYN {
				t.Fatalf("bad SYN: %+v", syn)
			}

			psh := parseFrames(t, writes[1].data)
			if len(psh) != 1 || psh[0].ver != byte(version) || psh[0].cmd != cmdPSH ||
				string(psh[0].data) != "hello" {
				t.Fatalf("bad coalesced PSH for v%d: %+v", version, psh)
			}
		})
	}
}

// TestCoalesceEchoV1V2 runs data through a real pipe for both versions to
// confirm the merged stream interoperates with the standard recvLoop.
func TestCoalesceEchoV1V2(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			p1, p2 := net.Pipe()

			serverCfg := coalesceConfig(version, 64, 50*time.Millisecond)
			ss, err := Server(p1, serverCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()

			cs, err := Client(p2, coalesceConfig(version, 64, 50*time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()

			go func() {
				st, err := ss.AcceptStream()
				if err != nil {
					return
				}
				defer st.Close()
				io.Copy(st, st)
			}()

			st, err := cs.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			msg := []byte("coalesced echo payload")
			if _, err := st.Write(msg); err != nil {
				t.Fatal(err)
			}

			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(st, buf); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf, msg) {
				t.Fatalf("echo mismatch: got %q want %q", buf, msg)
			}
		})
	}
}

func TestCoalesceConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WriteCoalesceBytes = 1024
	cfg.WriteCoalesceDelay = 0
	if err := VerifyConfig(cfg); err == nil {
		t.Fatal("expected error when only one coalesce field is set")
	}

	cfg = DefaultConfig()
	cfg.WriteCoalesceBytes = 0
	cfg.WriteCoalesceDelay = time.Millisecond
	if err := VerifyConfig(cfg); err == nil {
		t.Fatal("expected error when only the delay is set")
	}

	cfg = DefaultConfig()
	cfg.WriteCoalesceBytes = headerSize - 1
	cfg.WriteCoalesceDelay = time.Millisecond
	if err := VerifyConfig(cfg); err == nil {
		t.Fatal("expected error when threshold is smaller than a frame header")
	}

	cfg = DefaultConfig()
	cfg.WriteCoalesceBytes = -1
	cfg.WriteCoalesceDelay = time.Millisecond
	if err := VerifyConfig(cfg); err == nil {
		t.Fatal("expected error for negative byte threshold")
	}

	cfg = DefaultConfig()
	cfg.WriteCoalesceDelay = -1
	if err := VerifyConfig(cfg); err == nil {
		t.Fatal("expected error for negative delay")
	}

	var bts buffer
	if _, err := Server(&bts, coalesceConfig(1, 4096, time.Millisecond)); err != nil {
		t.Fatalf("valid coalescing config rejected: %v", err)
	}
}
