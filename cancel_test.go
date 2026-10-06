package rpc2

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

// pipeClients returns a connected pair of clients speaking the gob codec
// over an in-memory connection. Both read loops are started.
func pipeClients(t *testing.T) (a, b *Client) {
	t.Helper()
	c1, c2 := net.Pipe()
	a = NewClient(c1)
	b = NewClient(c2)
	go a.Run()
	go b.Run()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

func waitForCancelSupport(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !c.peerSupportsCancel() {
		if time.Now().After(deadline) {
			t.Fatal("peer did not announce cancellation support")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForServing(t *testing.T, c *Client, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.servingMu.Lock()
		l := len(c.serving)
		c.servingMu.Unlock()
		if l == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("serving map size did not become %d", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestContextHandler(t *testing.T) {
	a, b := pipeClients(t)

	b.Handle("echo", func(ctx context.Context, client *Client, in *int, out *int) error {
		if ctx == nil {
			t.Error("handler got nil context")
		}
		*out = *in * 2
		return nil
	})

	var out int
	if err := a.Call("echo", 21, &out); err != nil {
		t.Fatal(err)
	}
	if out != 42 {
		t.Fatalf("unexpected reply: %d", out)
	}
}

func TestCancelExecuting(t *testing.T) {
	a, b := pipeClients(t)

	started := make(chan struct{})
	cancelled := make(chan struct{})
	b.Handle("wait", func(ctx context.Context, client *Client, _ *struct{}, reply *int) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	b.Handle("add", func(client *Client, in *int, out *int) error {
		*out = *in + 1
		return nil
	})
	waitForCancelSupport(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	var reply int
	errCh := make(chan error, 1)
	go func() {
		errCh <- a.CallWithContext(ctx, "wait", &struct{}{}, &reply)
	}()

	<-started
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not observe cancellation")
	}
	waitForServing(t, b, 0)
	if reply != 0 {
		t.Fatalf("reply modified after cancel: %d", reply)
	}

	// The handler returned an error after the caller left; the late
	// response must be read and discarded without breaking the stream.
	var out int
	if err := a.Call("add", 1, &out); err != nil {
		t.Fatal(err)
	}
	if out != 2 {
		t.Fatalf("unexpected reply: %d", out)
	}
}

func TestCancelQueued(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	clt := NewClient(c1)
	handlers := make(map[string]*handler)
	ran := false
	addHandler(handlers, "work", func(client *Client, _ *struct{}, reply *int) error {
		ran = true
		return nil
	})

	// The request is cancelled before the handler gets a chance to run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		clt.handleRequest(
			Request{Seq: 9, Method: "work"},
			handlers["work"],
			reflect.ValueOf(&struct{}{}),
			ctx,
			cancel,
		)
	}()

	// The peer must receive an error response without the handler running.
	codec := NewGobCodec(c2)
	var req Request
	var resp Response
	if err := codec.ReadHeader(&req, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Seq != 9 || resp.Error == "" {
		t.Fatalf("expected error response for seq 9, got: %+v", resp)
	}
	if err := codec.ReadResponseBody(nil); err != nil {
		t.Fatal(err)
	}
	<-done
	if ran {
		t.Fatal("handler ran despite cancellation")
	}
}

func TestCancelAfterReplySent(t *testing.T) {
	a, b := pipeClients(t)

	b.Handle("fast", func(client *Client, in *int, out *int) error {
		*out = *in
		return nil
	})
	waitForCancelSupport(t, a)

	var out int
	if err := a.Call("fast", 5, &out); err != nil {
		t.Fatal(err)
	}
	if out != 5 {
		t.Fatalf("unexpected reply: %d", out)
	}
	waitForServing(t, b, 0)

	// A cancel frame for a request whose reply was already sent must be
	// ignored. Sequence 1 was the first (completed) call.
	if err := a.sendControl(controlMessage{Type: controlCancel, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	// A cancel frame for a never-seen sequence number must be ignored too.
	if err := a.sendControl(controlMessage{Type: controlCancel, Seq: 999}); err != nil {
		t.Fatal(err)
	}

	// The connection must still be usable.
	if err := a.Call("fast", 6, &out); err != nil {
		t.Fatal(err)
	}
	if out != 6 {
		t.Fatalf("unexpected reply: %d", out)
	}
}

func TestCancelCompletionRace(t *testing.T) {
	a, b := pipeClients(t)

	b.Handle("work", func(ctx context.Context, client *Client, ms int, reply *int) error {
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			*reply = ms * 2
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	waitForCancelSupport(t, a)

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100; i++ {
		ms := rng.Intn(4)
		timeout := time.Duration(rng.Intn(4)) * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		var reply int
		err := a.CallWithContext(ctx, "work", ms, &reply)
		cancel()
		switch {
		case err == nil:
			if reply != ms*2 {
				t.Fatalf("call succeeded but reply is %d, want %d", reply, ms*2)
			}
		case errors.Is(err, context.DeadlineExceeded):
			// We won the race against the response; the late reply
			// must not touch the reply value.
			if reply != 0 {
				t.Fatalf("reply modified after timeout: %d", reply)
			}
		case errors.Is(err, ServerError(context.Canceled.Error())):
			// The handler observed the cancellation and its error
			// response won the race.
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// The stream must still be aligned after the race.
	var reply int
	if err := a.Call("work", 1, &reply); err != nil {
		t.Fatal(err)
	}
	if reply != 2 {
		t.Fatalf("unexpected reply: %d", reply)
	}
	waitForServing(t, b, 0)
}

func TestBidirectionalSameSeq(t *testing.T) {
	a, b := pipeClients(t)

	// Both directions use the same sequence number for their first call.
	// Cancelling one direction must not affect the other.
	aStarted := make(chan struct{})
	aRelease := make(chan struct{})
	aCancelled := make(chan struct{})
	a.Handle("slow", func(ctx context.Context, client *Client, _ *struct{}, reply *int) error {
		close(aStarted)
		select {
		case <-aRelease:
			*reply = 42
			return nil
		case <-ctx.Done():
			close(aCancelled)
			return ctx.Err()
		}
	})

	bStarted := make(chan struct{})
	bCancelled := make(chan struct{})
	b.Handle("slow", func(ctx context.Context, client *Client, _ *struct{}, reply *int) error {
		close(bStarted)
		<-ctx.Done()
		close(bCancelled)
		return ctx.Err()
	})

	waitForCancelSupport(t, a)
	waitForCancelSupport(t, b)

	// b calls a (seq 1 in b->a direction); left to complete.
	bErr := make(chan error, 1)
	var bReply int
	go func() {
		bErr <- b.Call("slow", &struct{}{}, &bReply)
	}()

	// a calls b (seq 1 in a->b direction); cancelled below.
	ctxA, cancelA := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	var aReply int
	go func() {
		aErr <- a.CallWithContext(ctxA, "slow", &struct{}{}, &aReply)
	}()

	<-aStarted
	<-bStarted
	cancelA()

	if err := <-aErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	select {
	case <-bCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler on b did not observe cancellation")
	}

	// The opposite direction with the same sequence number must be
	// unaffected by the cancel frame.
	select {
	case <-aCancelled:
		t.Fatal("cancel leaked into the opposite direction")
	case <-time.After(200 * time.Millisecond):
	}
	close(aRelease)
	if err := <-bErr; err != nil {
		t.Fatal(err)
	}
	if bReply != 42 {
		t.Fatalf("unexpected reply: %d", bReply)
	}
	if aReply != 0 {
		t.Fatalf("reply modified after cancel: %d", aReply)
	}
}

func TestLegacyPeer(t *testing.T) {
	c1, c2 := net.Pipe()
	a := NewClient(c1)
	b := NewClient(c2)
	// Simulate an endpoint that does not implement the cancellation
	// extension: it neither announces support nor understands control
	// frames.
	b.disableCancel = true
	go a.Run()
	go b.Run()
	defer a.Close()
	defer b.Close()

	b.Handle("add", func(client *Client, in *int, out *int) error {
		*out = *in + 1
		return nil
	})
	started := make(chan struct{})
	release := make(chan struct{})
	b.Handle("wait", func(client *Client, _ *struct{}, reply *int) error {
		close(started)
		<-release
		return nil
	})

	// a's hello control frame is treated by the legacy peer as an unknown
	// notification; the stream must stay aligned.
	var out int
	if err := a.Call("add", 1, &out); err != nil {
		t.Fatal(err)
	}
	if out != 2 {
		t.Fatalf("unexpected reply: %d", out)
	}
	if a.peerSupportsCancel() {
		t.Fatal("legacy peer must not be flagged as supporting cancel")
	}

	// Cancelling still works locally: the caller returns, the pending
	// entry is removed, and no cancel frame is sent to the legacy peer.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	var reply int
	go func() {
		errCh <- a.CallWithContext(ctx, "wait", &struct{}{}, &reply)
	}()
	<-started
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// The legacy handler finishes later; its reply must be discarded.
	close(release)
	time.Sleep(100 * time.Millisecond)
	if reply != 0 {
		t.Fatalf("reply modified after cancel: %d", reply)
	}

	// The connection must still be usable.
	if err := a.Call("add", 2, &out); err != nil {
		t.Fatal(err)
	}
	if out != 3 {
		t.Fatalf("unexpected reply: %d", out)
	}
}

func TestReservedControlMethod(t *testing.T) {
	a, b := pipeClients(t)

	var out int
	if err := a.Call(controlMethod, 1, &out); !errors.Is(err, errReservedMethod) {
		t.Fatalf("expected errReservedMethod, got: %v", err)
	}
	if err := a.Notify(controlMethod, 1); !errors.Is(err, errReservedMethod) {
		t.Fatalf("expected errReservedMethod, got: %v", err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when registering the reserved method name")
		}
	}()
	b.Handle(controlMethod, func(client *Client, in *int, out *int) error { return nil })
}

// TestConcurrentCancelAndCall ensures control frames and regular calls
// can be in flight concurrently without corrupting the stream.
func TestConcurrentCancelAndCall(t *testing.T) {
	a, b := pipeClients(t)

	b.Handle("wait", func(ctx context.Context, client *Client, _ *struct{}, reply *int) error {
		<-ctx.Done()
		return ctx.Err()
	})
	b.Handle("add", func(client *Client, in *int, out *int) error {
		*out = *in + 1
		return nil
	})
	waitForCancelSupport(t, a)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i)*time.Millisecond)
			defer cancel()
			var reply int
			a.CallWithContext(ctx, "wait", &struct{}{}, &reply)
		}(i)
		go func(i int) {
			defer wg.Done()
			var out int
			if err := a.Call("add", i, &out); err != nil {
				t.Error(err)
				return
			}
			if out != i+1 {
				t.Errorf("unexpected reply: %d", out)
			}
		}(i)
	}
	wg.Wait()
}
