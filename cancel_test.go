package rpc2

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memConn is an in-memory io.ReadWriteCloser backed by a buffered channel.
// Both ends share a single closer so shutting one side down unblocks the other.
type memConn struct {
	in   chan []byte
	buf  []byte
	peer *memConn
	once *sync.Once
	done chan struct{}
}

func memPipe() (io.ReadWriteCloser, io.ReadWriteCloser) {
	once := &sync.Once{}
	done := make(chan struct{})
	a := &memConn{in: make(chan []byte, 512), once: once, done: done}
	b := &memConn{in: make(chan []byte, 512), once: once, done: done}
	a.peer = b
	b.peer = a
	return a, b
}

func (m *memConn) Read(p []byte) (int, error) {
	if len(m.buf) == 0 {
		select {
		case b, ok := <-m.in:
			if !ok {
				return 0, io.EOF
			}
			m.buf = b
		case <-m.done:
			return 0, io.EOF
		}
	}
	n := copy(p, m.buf)
	m.buf = m.buf[n:]
	return n, nil
}

func (m *memConn) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case m.peer.in <- b:
		return len(p), nil
	case <-m.done:
		return 0, io.ErrClosedPipe
	}
}

func (m *memConn) Close() error {
	m.once.Do(func() { close(m.done) })
	return nil
}

type CancelArgs struct{ A, B int }
type CancelReply int

// newCancelPair connects a server and a client over in-memory connections.
func newCancelPair(t *testing.T) (*Server, *Client) {
	t.Helper()
	a, b := memPipe()
	srv := NewServer()
	go srv.ServeConn(a)
	clt := NewClient(b)
	go clt.Run()
	t.Cleanup(func() {
		clt.Close()
		a.Close()
		b.Close()
	})
	return srv, clt
}

// TestCancelExecuting verifies that a cancellation is delivered to the
// context-aware handler while it is executing, and that the caller returns
// promptly with context.Canceled and an untouched reply.
func TestCancelExecuting(t *testing.T) {
	srv, clt := newCancelPair(t)

	started := make(chan struct{})
	handlerSawCancel := make(chan struct{})
	srv.Handle("slow", func(c *Client, ctx context.Context, args *CancelArgs, reply *CancelReply) error {
		close(started)
		<-ctx.Done()
		close(handlerSawCancel)
		return ctx.Err()
	})
	srv.Handle("add", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		*reply = CancelReply(args.A + args.B)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		waitPeerCancel(t, clt)
		cancel()
	}()

	var rep CancelReply
	err := clt.CallWithContext(ctx, "slow", &CancelArgs{}, &rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if rep != 0 {
		t.Fatalf("reply must not be modified after cancellation, got %d", rep)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}
	select {
	case <-handlerSawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe cancellation")
	}

	// The late error response must have been drained without losing alignment.
	var ok2 CancelReply
	if err := clt.Call("add", &CancelArgs{2, 3}, &ok2); err != nil {
		t.Fatalf("stream not aligned after cancellation: %v", err)
	}
	if ok2 != 5 {
		t.Fatalf("unexpected reply: %d", ok2)
	}
}

// TestCancelQueued deterministically cancels a request whose handler goroutine
// has been spawned but has not started running yet. The handler block at a
// gate; an initial request is forced to be the first in line, the cancel frame
// is read ahead of it, and the handler must observe an already-canceled
// context.
func TestCancelQueued(t *testing.T) {
	srv, clt := newCancelPair(t)

	entered := make(chan uint64, 16)
	gate := make(chan struct{})
	srv.Handle("work", func(c *Client, ctx context.Context, seq int, reply *CancelReply) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entered <- uint64(seq)
		<-gate
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		*reply = 1
		return nil
	})

	// First handler blocks at the gate and occupies the only worker slot.
	var first CancelReply
	firstDone := make(chan error, 1)
	go func() { firstDone <- clt.Call("work", 1, &first) }()
	if uint64(<-entered) != 1 {
		t.Fatal("first request did not reach the handler first")
	}

	// Second request is dispatched (handler goroutine created) but cannot run
	// because the first handler holds the gate.
	ctx, cancel := context.WithCancel(context.Background())
	var second CancelReply
	secondDone := make(chan error, 1)
	go func() { secondDone <- clt.CallWithContext(ctx, "work", 2, &second) }()

	// Wait for the capability hello round-trip before canceling.
	waitPeerCancel(t, clt)

	// Give the read loop time to dispatch the second request and park its
	// goroutine before the cancel frame arrives.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued cancellation did not return")
	}
	if second != 0 {
		t.Fatalf("queued cancel reply was modified: %d", second)
	}

	close(gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// Stream must still be aligned.
	var rep CancelReply
	if err := clt.Call("work", 3, &rep); err != nil {
		t.Fatalf("stream not aligned: %v", err)
	}
}

func waitPeerCancel(t *testing.T, clt *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		clt.mutex.Lock()
		ok := clt.peerCancel
		clt.mutex.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("peer did not advertise cancellation support")
}

// TestCancelAfterReplied verifies that a cancel frame arriving after the
// response has already been produced is a harmless no-op and cannot be
// mistaken for a regular method call.
func TestCancelAfterReplied(t *testing.T) {
	srv, clt := newCancelPair(t)

	srv.Handle("fast", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		*reply = 7
		return nil
	})

	var rep CancelReply
	if err := clt.Call("fast", &CancelArgs{}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep != 7 {
		t.Fatalf("unexpected reply: %d", rep)
	}

	// Cancel frames for finished/unknown requests are ignored, not dispatched.
	if err := clt.Notify(CancelMethod, &CancelRequest{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := clt.Notify(CancelMethod, &CancelRequest{Seq: 99999}); err != nil {
		t.Fatal(err)
	}

	// Server still responds normally.
	rep = 0
	if err := clt.Call("fast", &CancelArgs{}, &rep); err != nil {
		t.Fatalf("stream not aligned: %v", err)
	}
	if rep != 7 {
		t.Fatalf("unexpected reply: %d", rep)
	}
}

// TestCancelCompletionRace hammers calls whose completion races their context
// deadline. A successful response fully populates the reply; a canceled call
// leaves the reply zero even though the late body is still consumed.
func TestCancelCompletionRace(t *testing.T) {
	srv, clt := newCancelPair(t)

	srv.Handle("race", func(c *Client, ctx context.Context, i int, reply *int) error {
		*reply = 0x600DF00D
		return nil
	})

	const n = 200
	wg := sync.WaitGroup{}
	wg.Add(n)
	var failures int64
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration((i%3))*time.Millisecond)
			defer cancel()
			rep := new(int)
			err := clt.CallWithContext(ctx, "race", 1, rep)
			switch {
			case err == nil:
				if *rep != 0x600DF00D {
					t.Errorf("completed call has wrong reply: %d", *rep)
					atomic.AddInt64(&failures, 1)
				}
			case errors.Is(err, context.DeadlineExceeded):
				if *rep != 0 {
					t.Errorf("canceled call reply was modified: %d", *rep)
					atomic.AddInt64(&failures, 1)
				}
			default:
				t.Errorf("unexpected error: %v", err)
				atomic.AddInt64(&failures, 1)
			}
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt64(&failures) != 0 {
		t.Fatalf("had %d failing calls", failures)
	}

	// Final synchronization proves every late body was read.
	srv.Handle("ping", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		*reply = 1
		return nil
	})
	var rep CancelReply
	if err := clt.Call("ping", &CancelArgs{}, &rep); err != nil {
		t.Fatalf("stream not aligned after race: %v", err)
	}
}

// TestSameSeqBothDirections verifies that request numbers are independent in
// each direction: a client call and the server's reverse call can both use
// sequence number 1 simultaneously without colliding.
func TestSameSeqBothDirections(t *testing.T) {
	srv, clt := newCancelPair(t)

	srv.Handle("cb", func(c *Client, ctx context.Context, args *CancelArgs, reply *CancelReply) error {
		var inner CancelReply
		// This call uses seq 1 on the server side, while the enclosing
		// request uses seq 1 on the client side.
		if err := c.Call("echo", &CancelArgs{10, 20}, &inner); err != nil {
			return err
		}
		*reply = inner
		return nil
	})
	clt.Handle("echo", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		*reply = CancelReply(args.A + args.B)
		return nil
	})

	var rep CancelReply
	if err := clt.Call("cb", &CancelArgs{}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep != 30 {
		t.Fatalf("unexpected reply: %d", rep)
	}
}

// noCapCodec simulates a peer that does not implement the cancellation
// extension: it strips capability advertisement from every frame and counts
// incoming cancel frames.
type noCapCodec struct {
	Codec
	cancelsReceived int64
}

func (n *noCapCodec) ReadHeader(req *Request, resp *Response) error {
	if err := n.Codec.ReadHeader(req, resp); err != nil {
		return err
	}
	if req.Method == CancelMethod {
		atomic.AddInt64(&n.cancelsReceived, 1)
	}
	req.Capabilities = Capabilities{}
	resp.Capabilities = Capabilities{}
	return nil
}

func (n *noCapCodec) WriteRequest(r *Request, body interface{}) error {
	cp := *r
	cp.Capabilities = Capabilities{}
	return n.Codec.WriteRequest(&cp, body)
}

func (n *noCapCodec) WriteResponse(r *Response, body interface{}) error {
	cp := *r
	cp.Capabilities = Capabilities{}
	return n.Codec.WriteResponse(&cp, body)
}

// TestCancelUnsupportedPeer verifies that when the remote endpoint does not
// advertise cancellation support, no cancel frame is sent and the local
// caller still returns early; the connection remains usable.
func TestCancelUnsupportedPeer(t *testing.T) {
	a, b := memPipe()

	serverCodec := &noCapCodec{Codec: NewGobCodec(a)}
	srv := NewServer()
	srv.Handle("slow", func(c *Client, ctx context.Context, args *CancelArgs, reply *CancelReply) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		*reply = 42
		return nil
	})
	go srv.ServeCodec(serverCodec)

	clientCodec := &noCapCodec{Codec: NewGobCodec(b)}
	clt := NewClientWithCodec(clientCodec)
	go clt.Run()
	defer clt.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	var rep CancelReply
	err := clt.CallWithContext(ctx, "slow", &CancelArgs{}, &rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Wait long enough for a (wrongly sent) cancel frame to arrive.
	time.Sleep(600 * time.Millisecond)
	if n := atomic.LoadInt64(&serverCodec.cancelsReceived); n != 0 {
		t.Fatalf("cancel frame sent to unsupported peer: %d", n)
	}

	// The handler has finished and its late reply was drained; stream is usable.
	srv.Handle("add", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		*reply = CancelReply(args.A + args.B)
		return nil
	})
	var rep2 CancelReply
	if err := clt.Call("add", &CancelArgs{1, 2}, &rep2); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
	if rep2 != 3 {
		t.Fatalf("unexpected reply: %d", rep2)
	}
}

// TestCancelLegacyHandler verifies interoperability with an endpoint that
// supports the cancel frame but runs an old-style, non-context handler.
// The caller returns early on cancellation; the still-running handler later
// produces a normal reply that is drained, and the stream stays usable.
func TestCancelLegacyHandler(t *testing.T) {
	srv, clt := newCancelPair(t)

	proceed := make(chan struct{})
	srv.Handle("oldslow", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		<-proceed
		*reply = 99
		return nil
	})
	srv.Handle("add", func(c *Client, args *CancelArgs, reply *CancelReply) error {
		*reply = CancelReply(args.A + args.B)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	var rep CancelReply
	done := make(chan error, 1)
	go func() { done <- clt.CallWithContext(ctx, "oldslow", &CancelArgs{}, &rep) }()
	waitPeerCancel(t, clt)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not return")
	}

	close(proceed)

	// Wait for the late success reply to be drained, then verify alignment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var r CancelReply
		if err := clt.Call("add", &CancelArgs{4, 5}, &r); err == nil {
			if r != 9 {
				t.Fatalf("unexpected reply: %d", r)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stream not usable after late reply to legacy handler")
}

// TestRegisterCancelMethodPanics ensures the cancel frame can never be
// shadowed by a user handler.
func TestRegisterCancelMethodPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when registering reserved method")
		}
	}()
	NewServer().Handle(CancelMethod, func(c *Client, args *CancelArgs, reply *CancelReply) error {
		return nil
	})
}
