package rpc2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// memCh is one direction of a buffered, full-duplex byte stream. Unlike
// net.Pipe, writes are buffered so a handler can send a nested request while
// its own response is still unread without deadlocking.
type memCh struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      bytes.Buffer
	writeEnd bool // writer finished (EOF after draining buffered data)
	aborted  bool // fully closed; reads fail immediately
}

func newMemCh() *memCh {
	ch := &memCh{}
	ch.cond = sync.NewCond(&ch.mu)
	return ch
}

func (ch *memCh) Write(p []byte) (int, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.writeEnd || ch.aborted {
		return 0, io.ErrClosedPipe
	}
	n, err := ch.buf.Write(p)
	ch.cond.Broadcast()
	return n, err
}

func (ch *memCh) Read(p []byte) (int, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	for ch.buf.Len() == 0 && !ch.writeEnd && !ch.aborted {
		ch.cond.Wait()
	}
	if ch.buf.Len() > 0 {
		return ch.buf.Read(p)
	}
	if ch.aborted {
		return 0, io.ErrUnexpectedEOF
	}
	return 0, io.EOF // write end closed: clean EOF
}

// closeWrite signals a clean EOF after buffered data is consumed.
func (ch *memCh) closeWrite() {
	ch.mu.Lock()
	ch.writeEnd = true
	ch.cond.Broadcast()
	ch.mu.Unlock()
}

// abort terminates both directions immediately, discarding future reads.
func (ch *memCh) abort() {
	ch.mu.Lock()
	ch.aborted = true
	ch.writeEnd = true
	ch.cond.Broadcast()
	ch.mu.Unlock()
}

// memPipe is one end of an in-memory full-duplex connection.
type memPipe struct {
	rd, wr    *memCh
	other     *memPipe // peer end, for write half close
	writeOnce sync.Once
	closeOnce sync.Once
}

func (c *memPipe) Read(b []byte) (int, error)  { return c.rd.Read(b) }
func (c *memPipe) Write(b []byte) (int, error) { return c.wr.Write(b) }
func (c *memPipe) Close() error {
	c.closeOnce.Do(func() {
		// Treat a full close like both directions shutting down gracefully:
		// signal EOF to the peer after buffered bytes, and stop our own
		// reads. The read loop may already have half-closed, which only
		// closed one direction.
		c.other.rd.closeWrite()
		c.wr.closeWrite()
		c.rd.closeWrite()
	})
	return nil
}

// CloseWrite shuts down the write direction only, signalling EOF to the
// peer's reads while still allowing this end to receive, mirroring TCP.
func (c *memPipe) CloseWrite() error {
	c.writeOnce.Do(func() { c.other.rd.closeWrite() })
	return nil
}

// Break aborts both directions immediately, simulating an unexpected
// connection loss rather than a graceful close.
func (c *memPipe) Break() {
	c.rd.abort()
	c.wr.abort()
	c.other.rd.abort()
	c.other.wr.abort()
}
func (c *memPipe) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *memPipe) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *memPipe) SetDeadline(time.Time) error      { return nil }
func (c *memPipe) SetReadDeadline(time.Time) error  { return nil }
func (c *memPipe) SetWriteDeadline(time.Time) error { return nil }

// memPipePair returns the two ends of an in-memory connection.
func memPipePair() (net.Conn, net.Conn) {
	ab, ba := newMemCh(), newMemCh()
	a := &memPipe{rd: ba, wr: ab}
	b := &memPipe{rd: ab, wr: ba}
	a.other = b
	b.other = a
	return a, b
}

// newPair builds two connected rpc2 clients over an in-memory connection and
// starts both read loops.
func newPair(t *testing.T) (*Client, *Client) {
	t.Helper()
	aConn, bConn := memPipePair()
	a := NewClient(aConn)
	b := NewClient(bConn)
	go a.Run()
	go b.Run()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

// rawConnPair returns two ends of a fresh in-memory connection.
func rawConnPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	return memPipePair()
}

func waitSignal(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// peerSeenDrain reports whether c has received a go-away from its peer.
func peerSeenDrain(c *Client) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.peerDrain
}

// waitPeerDrain polls until c has observed the peer draining.
func waitPeerDrain(t *testing.T, c *Client, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if peerSeenDrain(c) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}

// TestDrainNotification verifies that draining notifies the peer, which then
// refuses new locally initiated calls, while admitted inbound handlers and
// outstanding outbound requests finish.
func TestDrainNotification(t *testing.T) {
	type Reply int
	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})

	a, b := newPair(t)
	b.Handle("slow", func(_ *Client, _ int, reply *Reply) error {
		close(handlerEntered)
		<-release
		*reply = 1
		close(handlerDone)
		return nil
	})

	// An outstanding outbound request that must be allowed to complete
	// after draining starts.
	callDone := make(chan *Call, 1)
	a.Go("slow", 0, new(Reply), callDone)
	waitSignal(t, handlerEntered, "handler did not start")

	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drainDone <- b.Drain(ctx)
	}()

	// The caller observes the go-away and refuses new calls it initiates.
	waitPeerDrain(t, a, "caller did not observe drain")

	// a was notified that b is draining and runs no admitted handler of its
	// own, so a new top-level call it initiates is refused even though b's
	// slow handler is still running (only that handler's nested callbacks
	// are allowed).
	if err := a.Go("x", 0, new(Reply), make(chan *Call, 1)).Error; !errors.Is(err, ErrDraining) {
		t.Fatalf("notified peer new call err = %v, want ErrDraining", err)
	}
	// Same on the draining side once its admitted handler finishes later;
	// here b still runs the handler, so check the state rather than send.
	b.mutex.Lock()
	if !b.draining {
		t.Fatal("b should be draining")
	}
	b.mutex.Unlock()

	// The already admitted handler finishes; drain then completes cleanly.
	close(release)
	waitSignal(t, handlerDone, "handler did not finish")

	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatalf("drain err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return")
	}

	// The outstanding call got its real reply, not a synthetic error.
	select {
	case call := <-callDone:
		if call.Error != nil {
			t.Fatalf("in-flight call err = %v", call.Error)
		}
		if *(call.Reply.(*Reply)) != 1 {
			t.Fatalf("in-flight reply = %d, want 1", *call.Reply.(*Reply))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call did not finish")
	}

	// Disconnect channel is closed on both sides: everyone reaches a
	// definite terminal state.
	waitSignal(t, a.DisconnectNotify(), "a not disconnected")
	waitSignal(t, b.DisconnectNotify(), "b not disconnected")
}

// TestDrainNestedCallback verifies that a handler may call back into the
// peer after both sides have started draining, so neither waits forever for
// the other to close.
func TestDrainNestedCallback(t *testing.T) {
	type Reply int
	outerEntered := make(chan struct{})
	release := make(chan struct{})
	innerCalled := make(chan struct{})

	a, b := newPair(t)

	// b serves "inner", which a's "outer" handler calls back while draining.
	b.Handle("inner", func(_ *Client, _ int, reply *Reply) error {
		close(innerCalled)
		*reply = 7
		return nil
	})

	// a's admitted handler waits until draining is underway and then makes
	// the nested callback into b.
	a.Handle("outer", func(c *Client, _ int, reply *Reply) error {
		close(outerEntered)
		<-release
		var nested Reply
		if err := c.Call("inner", 0, &nested); err != nil {
			return err
		}
		if nested != 7 {
			t.Fatalf("nested reply = %d, want 7", nested)
		}
		*reply = nested
		return nil
	})

	callDone := make(chan *Call, 1)
	b.Go("outer", 0, new(Reply), callDone)
	waitSignal(t, outerEntered, "outer handler did not start")

	// Both peers drain at the same time.
	errs := make(chan error, 2)
	startB := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errs <- a.Drain(ctx)
	}()
	go func() {
		<-startB
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errs <- b.Drain(ctx)
	}()

	// Wait until a has announced the drain to b, then let b drain too and
	// finally release the handler so it needs its nested callback while
	// both peers are draining.
	waitPeerDrain(t, b, "b did not observe a draining")
	close(startB)
	close(release)

	waitSignal(t, innerCalled, "nested callback did not run")

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("drain %d err = %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("drain %d did not return", i)
		}
	}

	select {
	case call := <-callDone:
		if call.Error != nil {
			t.Fatalf("outer call err = %v", call.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("outer call did not finish")
	}
}

// TestDrainRepeated verifies that concurrent and later Drain calls join the
// same drain and all reach a terminal state.
func TestDrainRepeated(t *testing.T) {
	a, b := newPair(t)

	const n = 4
	errs := make(chan error, n+1)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- a.Drain(ctx)
		}()
	}
	close(start)

	// The peer observes the go-away even though it does not drain itself.
	waitPeerDrain(t, b, "peer did not observe drain")

	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("joining drain %d err = %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("joining drain %d stuck", i)
		}
	}

	// A later Drain is idempotent: the graceful close already completed.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Drain(ctx); err != nil {
		t.Fatalf("drain after completed drain err = %v, want nil", err)
	}
}

// TestDrainReadErrorFirst verifies that a read-loop failure unblocks a
// waiting Drain and hands every waiter the real error instead of hanging.
func TestDrainReadErrorFirst(t *testing.T) {
	type Reply int
	handlerEntered := make(chan struct{})

	aConn, bConn := rawConnPair(t)
	a := NewClient(aConn)
	b := NewClient(bConn)
	go a.Run()
	go b.Run()

	// An admitted handler never completes on its own, so a cannot drain
	// naturally; the only way its Drain can finish is the read-loop error.
	neverRelease := make(chan struct{})
	a.Handle("hung", func(_ *Client, _ int, reply *Reply) error {
		close(handlerEntered)
		<-neverRelease
		*reply = 1
		return nil
	})
	inflight := b.Go("hung", 0, new(Reply), make(chan *Call, 1))
	waitSignal(t, handlerEntered, "handler did not start")

	drainReturned := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drainReturned <- a.Drain(ctx)
	}()

	// Let a's goAway reach b before breaking the pipe.
	waitPeerDrain(t, b, "b did not observe drain")

	// Break the in-memory pipe while a is still waiting on the handler.
	aConn.(*memPipe).Break()

	select {
	case err := <-drainReturned:
		if err == nil {
			t.Fatal("drain returned nil after read error, want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain stuck after read error")
	}

	// The pending call also gets a definite (error) terminal state.
	select {
	case call := <-inflight.Done:
		if call.Error == nil {
			t.Fatal("pending call got nil error after read error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call stuck after read error")
	}

	waitSignal(t, a.DisconnectNotify(), "a not disconnected")
	waitSignal(t, b.DisconnectNotify(), "b not disconnected")
}

// TestDrainTimeout verifies that an expired context ends the wait and closes
// the connection; still-pending calls get a real error, never a fake reply.
func TestDrainTimeout(t *testing.T) {
	type Reply int
	handlerEntered := make(chan struct{})
	release := make(chan struct{})

	a, b := newPair(t)
	a.Handle("hung", func(_ *Client, _ int, reply *Reply) error {
		close(handlerEntered)
		<-release
		*reply = 1
		return nil
	})

	// Start an outbound call on b into a's hung handler; it stays pending
	// while a drains.
	inflight := b.Go("hung", 0, new(Reply), make(chan *Call, 1))
	waitSignal(t, handlerEntered, "handler did not start")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := a.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain err = %v, want DeadlineExceeded", err)
	}

	// Let the handler return after the connection is already closing. Its
	// response write may fail, but nothing must report a fake success.
	close(release)

	select {
	case call := <-inflight.Done:
		if call.Error == nil {
			t.Fatal("pending call got nil error after drain timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call never reached a terminal state")
	}

	waitSignal(t, a.DisconnectNotify(), "a not disconnected")
}
