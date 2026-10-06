package rpc2

import (
	"net"
	"sync"
	"testing"
	"time"
)

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// handlerGate blocks every handler invocation until released and records
// the peak number of concurrently running invocations.
type handlerGate struct {
	mu      sync.Mutex
	cur     int
	peak    int
	started chan struct{}
	release chan struct{}
}

func newHandlerGate() *handlerGate {
	return &handlerGate{
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (g *handlerGate) enter() {
	g.mu.Lock()
	g.cur++
	if g.cur > g.peak {
		g.peak = g.cur
	}
	g.mu.Unlock()
	g.started <- struct{}{}
	<-g.release
	g.mu.Lock()
	g.cur--
	g.mu.Unlock()
}

func (g *handlerGate) peakConcurrency() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func serveWithClient(t *testing.T, srv *Server) (clt *Client, srvClient *Client) {
	t.Helper()
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	srvClientCh := make(chan *Client, 1)
	srv.OnConnect(func(c *Client) { srvClientCh <- c })
	go srv.Accept(lis)

	conn, err := net.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt = NewClient(conn)
	go clt.Run()
	t.Cleanup(func() { clt.Close() })

	select {
	case srvClient = <-srvClientCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept connection")
	}
	return clt, srvClient
}

func TestInboundLimit(t *testing.T) {
	type Args struct{ A, B int }
	type Reply int

	gate := newHandlerGate()
	srv := NewServer()
	srv.Handle("block", func(client *Client, args *Args, reply *Reply) error {
		gate.enter()
		*reply = Reply(args.A + args.B)
		return nil
	})

	clt, srvClient := serveWithClient(t, srv)
	clt.Handle("mult", func(client *Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A * args.B)
		return nil
	})
	srvClient.SetInboundLimit(InboundLimit{MaxConcurrent: 2, MaxQueue: 1})

	// Fill both execution slots.
	done1 := make(chan *Call, 1)
	done2 := make(chan *Call, 1)
	clt.Go("block", Args{1, 2}, new(Reply), done1)
	clt.Go("block", Args{3, 4}, new(Reply), done2)
	<-gate.started
	<-gate.started

	// Fill the waiting queue.
	done3 := make(chan *Call, 1)
	clt.Go("block", Args{5, 6}, new(Reply), done3)
	waitForCond(t, "request to be queued", func() bool {
		return srvClient.InboundStats().Queued == 1
	})

	// Calls beyond the queue are rejected with a recognizable busy error.
	var rep Reply
	for i := 0; i < 2; i++ {
		if err := clt.Call("block", Args{7, 8}, &rep); err != ServerError(ErrBusy) {
			t.Fatalf("expected busy error, got: %v", err)
		}
	}
	if stats := srvClient.InboundStats(); stats.Rejected != 2 {
		t.Fatalf("expected 2 rejections, got: %d", stats.Rejected)
	}

	// Notifications beyond the queue are dropped and counted.
	for i := 0; i < 3; i++ {
		if err := clt.Notify("block", Args{i, i}); err != nil {
			t.Fatal(err)
		}
	}
	waitForCond(t, "notifications to be dropped", func() bool {
		return srvClient.InboundStats().Dropped == 3
	})

	// A callback from server to client must still complete while the
	// inbound queue is full; the reply path is not subject to the limit.
	var pong Reply
	if err := srvClient.Call("mult", Args{2, 3}, &pong); err != nil {
		t.Fatal(err)
	}
	if pong != 6 {
		t.Fatalf("unexpected callback reply: %d", pong)
	}

	// Release the handlers; the queued call must run and complete.
	close(gate.release)
	for i, done := range []chan *Call{done1, done2, done3} {
		select {
		case call := <-done:
			if call.Error != nil {
				t.Fatalf("call %d failed: %v", i, call.Error)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("call %d did not complete", i)
		}
	}

	if peak := gate.peakConcurrency(); peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
	stats := srvClient.InboundStats()
	if stats.Rejected != 2 || stats.Dropped != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestInboundPerMethodLimit(t *testing.T) {
	type Args struct{ A, B int }
	type Reply int

	gate := newHandlerGate()
	srv := NewServer()
	srv.Handle("slow", func(client *Client, args *Args, reply *Reply) error {
		gate.enter()
		*reply = Reply(args.A + args.B)
		return nil
	})
	srv.Handle("fast", func(client *Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A + args.B)
		return nil
	})

	clt, srvClient := serveWithClient(t, srv)
	srvClient.SetInboundLimit(InboundLimit{MaxConcurrent: 1, MaxQueue: 0})
	srvClient.SetMethodInboundLimit("slow", InboundLimit{MaxConcurrent: 1, MaxQueue: 0})

	// Occupy the only slot of the dedicated "slow" gate.
	done := make(chan *Call, 1)
	clt.Go("slow", Args{1, 2}, new(Reply), done)
	<-gate.started

	// A second "slow" call overflows the dedicated gate.
	var rep Reply
	if err := clt.Call("slow", Args{3, 4}, &rep); err != ServerError(ErrBusy) {
		t.Fatalf("expected busy error, got: %v", err)
	}

	// "fast" uses the shared pool and is unaffected by the full "slow" gate.
	if err := clt.Call("fast", Args{3, 4}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep != 7 {
		t.Fatalf("unexpected reply: %d", rep)
	}

	close(gate.release)
	select {
	case call := <-done:
		if call.Error != nil {
			t.Fatal(call.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow call did not complete")
	}

	if stats := srvClient.InboundStats(); stats.Rejected != 1 {
		t.Fatalf("expected 1 rejection, got: %d", stats.Rejected)
	}
}

func TestInboundCloseReleasesQueue(t *testing.T) {
	type Args struct{ A, B int }
	type Reply int

	gate := newHandlerGate()
	srv := NewServer()
	srv.Handle("block", func(client *Client, args *Args, reply *Reply) error {
		gate.enter()
		*reply = Reply(args.A + args.B)
		return nil
	})

	clt, srvClient := serveWithClient(t, srv)
	srvClient.SetInboundLimit(InboundLimit{MaxConcurrent: 1, MaxQueue: 4})

	// One running, then two calls and two notifications queued behind it.
	clt.Go("block", Args{1, 2}, new(Reply), make(chan *Call, 1))
	<-gate.started
	clt.Go("block", Args{3, 4}, new(Reply), make(chan *Call, 1))
	clt.Go("block", Args{5, 6}, new(Reply), make(chan *Call, 1))
	if err := clt.Notify("block", Args{7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := clt.Notify("block", Args{9, 10}); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "queue to fill", func() bool {
		return srvClient.InboundStats().Queued == 4
	})

	// Closing the connection must release every queued request.
	clt.Close()
	waitForCond(t, "queue to be released", func() bool {
		stats := srvClient.InboundStats()
		return stats.Queued == 0 && stats.Dropped == 2
	})

	close(gate.release)
}
