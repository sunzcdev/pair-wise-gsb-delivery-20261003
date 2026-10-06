package rpc2

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitDone(t *testing.T, call *Call) {
	t.Helper()
	select {
	case done := <-call.Done:
		if done.Error != nil {
			t.Fatalf("call %s failed: %v", call.Method, done.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("call %s did not complete", call.Method)
	}
}

// TestInboundPoolLimits verifies, with a gated handler, that the pool caps
// concurrency, rejects calls with ErrServerBusy when full, counts dropped
// notifications, and still lets reversed (server to client) calls complete
// while the inbound pool is full.
func TestInboundPoolLimits(t *testing.T) {
	type Args struct{ A int }
	type Reply int

	lis, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	pool := NewInboundPool(InboundConfig{MaxConcurrent: 1, QueueSize: 1})

	started := make(chan struct{})
	var startedOnce sync.Once
	gate := make(chan struct{})
	var cur, peak int32

	srv := NewServer()
	srv.HandleWithPool("block", func(client *Client, args *Args, reply *Reply) error {
		n := atomic.AddInt32(&cur, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		startedOnce.Do(func() { close(started) })
		<-gate
		atomic.AddInt32(&cur, -1)
		*reply = Reply(args.A)
		return nil
	}, pool)

	srvCltCh := make(chan *Client, 1)
	srv.OnConnect(func(c *Client) { srvCltCh <- c })
	go srv.Accept(lis)

	conn, err := net.Dial(network, lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := NewClient(conn)
	clt.Handle("ping", func(client *Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A + 1)
		return nil
	})
	go clt.Run()
	defer clt.Close()

	var srvClt *Client
	select {
	case srvClt = <-srvCltCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept connection")
	}

	// call1 occupies the only execution slot and blocks on the gate.
	call1 := clt.Go("block", &Args{1}, new(Reply), make(chan *Call, 1))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}

	// call2 fills the waiting queue. The read loop admits it before
	// reading any later message, so the rejections below are deterministic.
	call2 := clt.Go("block", &Args{2}, new(Reply), make(chan *Call, 1))

	// call3 finds the pool full (1 running + 1 queued) and is rejected
	// with an identifiable busy error.
	var rep3 Reply
	if err := clt.Call("block", &Args{3}, &rep3); !errors.Is(err, ErrServerBusy) {
		t.Fatalf("expected ErrServerBusy, got %v", err)
	}

	// A notification has no reply path; it is dropped and counted.
	if err := clt.Notify("block", &Args{4}); err != nil {
		t.Fatal(err)
	}
	// This rejected call is processed after the notification by the
	// server's read loop, so once it returns the drop has been counted.
	var rep5 Reply
	if err := clt.Call("block", &Args{5}, &rep5); !errors.Is(err, ErrServerBusy) {
		t.Fatalf("expected ErrServerBusy, got %v", err)
	}
	if got := srvClt.DroppedNotifications(); got != 1 {
		t.Fatalf("dropped notifications = %d, want 1", got)
	}

	// Reading replies does not go through the inbound pool: a reversed
	// call completes even though the pool is still full.
	var pong Reply
	if err := srvClt.Call("ping", &Args{41}, &pong); err != nil {
		t.Fatal(err)
	}
	if pong != 42 {
		t.Fatalf("pong = %d, want 42", pong)
	}

	// Release the gate; the running and the queued call complete.
	close(gate)
	waitDone(t, call1)
	waitDone(t, call2)

	if got := atomic.LoadInt32(&peak); got != 1 {
		t.Fatalf("peak concurrency = %d, want 1", got)
	}
}

// TestInboundPoolSharedAndRelease verifies that methods can share one pool
// while others keep their own limits, and that disconnecting releases
// queued requests without running them.
func TestInboundPoolSharedAndRelease(t *testing.T) {
	type Args struct{ A int }
	type Reply int

	lis, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	shared := NewInboundPool(InboundConfig{MaxConcurrent: 1})
	dedicated := NewInboundPool(InboundConfig{MaxConcurrent: 1, QueueSize: 1})

	gateA := make(chan struct{})
	startedA := make(chan struct{})
	gateD := make(chan struct{})
	startedD := make(chan struct{})
	var onceD sync.Once

	srv := NewServer()
	srv.HandleWithPool("a", func(_ *Client, _ *Args, _ *Reply) error {
		close(startedA)
		<-gateA
		return nil
	}, shared)
	srv.HandleWithPool("b", func(_ *Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A)
		return nil
	}, shared)
	srv.Handle("c", func(_ *Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A)
		return nil
	})
	srv.HandleWithPool("d", func(_ *Client, _ *Args, _ *Reply) error {
		onceD.Do(func() { close(startedD) })
		<-gateD
		return nil
	}, dedicated)
	go srv.Accept(lis)

	conn, err := net.Dial(network, lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := NewClient(conn)
	go clt.Run()

	// "a" occupies the only slot of the shared pool.
	callA := clt.Go("a", &Args{}, new(Reply), make(chan *Call, 1))
	select {
	case <-startedA:
	case <-time.After(5 * time.Second):
		t.Fatal("handler a did not start")
	}

	// "b" shares the pool with "a" and is rejected while "a" runs.
	var rep Reply
	if err := clt.Call("b", &Args{1}, &rep); !errors.Is(err, ErrServerBusy) {
		t.Fatalf("expected ErrServerBusy from shared pool, got %v", err)
	}
	// "c" has no pool and is unaffected.
	if err := clt.Call("c", &Args{2}, &rep); err != nil {
		t.Fatalf("unpooled method should not be limited: %v", err)
	}
	if rep != 2 {
		t.Fatalf("rep = %d, want 2", rep)
	}

	close(gateA)
	waitDone(t, callA)

	// Fill the dedicated pool: one running, one queued.
	clt.Go("d", &Args{}, new(Reply), make(chan *Call, 1))
	select {
	case <-startedD:
	case <-time.After(5 * time.Second):
		t.Fatal("handler d did not start")
	}
	clt.Go("d", &Args{}, new(Reply), make(chan *Call, 1))
	waitForCond(t, "queued request", func() bool { return dedicated.Waiting() == 1 })

	// Closing the connection releases the queued request without running
	// it, freeing its slot in the pool.
	clt.Close()
	waitForCond(t, "queued request to be released", func() bool { return dedicated.Waiting() == 0 })

	close(gateD)
	waitForCond(t, "running handler to finish", func() bool { return dedicated.Running() == 0 })
}
