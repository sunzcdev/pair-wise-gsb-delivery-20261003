package rpc2

import (
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type MwArgs struct{ A, B int }
type MwReply int

// pipePair returns a server/client pair connected over net.Pipe with gob codec.
func pipePair(t *testing.T, srv *Server) *Client {
	t.Helper()
	sconn, cconn := net.Pipe()
	done := make(chan struct{})
	go func() {
		srv.ServeConn(sconn)
		close(done)
	}()
	t.Cleanup(func() {
		cconn.Close()
		<-done
	})
	clt := NewClient(cconn)
	go clt.Run()
	return clt
}

// countingCodec wraps a Codec and counts WriteResponse calls.
type countingCodec struct {
	Codec
	writeResponses int32
}

func (c *countingCodec) WriteResponse(r *Response, body interface{}) error {
	atomic.AddInt32(&c.writeResponses, 1)
	return c.Codec.WriteResponse(r, body)
}

// TestMiddlewareOrder verifies that middlewares run in registration order and
// that each middleware can observe the result after the downstream chain ends.
func TestMiddlewareOrder(t *testing.T) {
	srv := NewServer()
	srv.Handle("add", func(client *Client, args *MwArgs, reply *MwReply) error {
		*reply = MwReply(args.A + args.B)
		return nil
	})

	var mu sync.Mutex
	var log []string
	trace := func(name string) Middleware {
		return func(info *CallInfo, next HandlerFunc) error {
			mu.Lock()
			log = append(log, "before:"+name)
			mu.Unlock()
			err := next(info)
			mu.Lock()
			log = append(log, "after:"+name)
			mu.Unlock()
			return err
		}
	}
	srv.Use(trace("auth"))
	srv.Use(trace("timer"))

	clt := pipePair(t, srv)
	var rep MwReply
	if err := clt.Call("add", &MwArgs{2, 3}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep != 5 {
		t.Fatalf("reply = %d, want 5", rep)
	}

	want := []string{"before:auth", "before:timer", "after:timer", "after:auth"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", log, want)
	}
}

// TestMiddlewareReject verifies that a middleware may reject the call before
// invoking the handler; upstream middlewares still observe the error.
func TestMiddlewareReject(t *testing.T) {
	var denied = errors.New("rpc2: denied")

	srv := NewServer()
	var handlerCalls int32
	srv.Handle("secure", func(client *Client, args *MwArgs, reply *MwReply) error {
		atomic.AddInt32(&handlerCalls, 1)
		*reply = 42
		return nil
	})

	var observed error
	srv.Use(func(info *CallInfo, next HandlerFunc) error {
		err := next(info)
		observed = err
		return err
	})
	srv.Use(func(info *CallInfo, next HandlerFunc) error {
		return denied
	})

	clt := pipePair(t, srv)
	var rep MwReply
	err := clt.Call("secure", &MwArgs{1, 2}, &rep)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want denied", err)
	}
	if atomic.LoadInt32(&handlerCalls) != 0 {
		t.Fatalf("handler called %d times, want 0", handlerCalls)
	}
	if observed != denied {
		t.Fatalf("observed = %v, want %v", observed, denied)
	}
}

// TestCallInfo verifies the middleware sees method name, request number,
// notification flag, connection state, args and reply.
func TestCallInfo(t *testing.T) {
	srv := NewServer()
	srv.Handle("ping", func(client *Client, args *MwArgs, reply *MwReply) error {
		*reply = MwReply(args.A)
		return nil
	})

	got := make(chan *CallInfo, 2)
	srv.Use(func(info *CallInfo, next HandlerFunc) error {
		info.State.Set("seen", true)
		err := next(info)
		got <- info
		return err
	})

	clt := pipePair(t, srv)
	var rep MwReply
	if err := clt.Call("ping", &MwArgs{7, 0}, &rep); err != nil {
		t.Fatal(err)
	}
	if err := clt.Notify("ping", &MwArgs{9, 0}); err != nil {
		t.Fatal(err)
	}

	first := <-got
	if first.Method != "ping" || first.Seq == 0 || first.Notification {
		t.Fatalf("unexpected call info: %+v", first)
	}
	if first.Client == nil {
		t.Fatal("CallInfo.Client is nil")
	}
	if v, ok := first.State.Get("seen"); !ok || v != true {
		t.Fatalf("state not propagated: %v", v)
	}
	if a, ok := first.Args.(*MwArgs); !ok || a.A != 7 {
		t.Fatalf("args = %#v", first.Args)
	}

	second := <-got
	if !second.Notification || second.Seq != 0 {
		t.Fatalf("notification flag wrong: %+v", second)
	}
}

// TestNotificationNoResponse verifies that notifications never receive a
// response, not even when a middleware rejects them.
func TestNotificationNoResponse(t *testing.T) {
	srv := NewServer()
	var handlerCalls int32
	notifDone := make(chan struct{}, 1)
	srv.Handle("n", func(client *Client, args *MwArgs, reply *MwReply) error {
		atomic.AddInt32(&handlerCalls, 1)
		notifDone <- struct{}{}
		return nil
	})
	srv.Use(func(info *CallInfo, next HandlerFunc) error {
		err := next(info)
		if info.Notification {
			notifDone <- struct{}{}
		}
		return err
	})

	sconn, cconn := net.Pipe()
	cc := &countingCodec{Codec: NewGobCodec(sconn)}
	go srv.ServeCodec(cc)
	clt := NewClient(cconn)
	go clt.Run()
	defer clt.Close()

	if err := clt.Notify("n", &MwArgs{1, 1}); err != nil {
		t.Fatal(err)
	}
	<-notifDone // handler ran
	<-notifDone // post-next middleware ran

	if n := atomic.LoadInt32(&cc.writeResponses); n != 0 {
		t.Fatalf("server wrote %d responses for a notification, want 0", n)
	}
	if atomic.LoadInt32(&handlerCalls) != 1 {
		t.Fatalf("handlerCalls = %d, want 1", handlerCalls)
	}

	// A following ordinary call still gets exactly one response.
	var rep MwReply
	if err := clt.Call("n", &MwArgs{2, 2}, &rep); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&cc.writeResponses); n != 1 {
		t.Fatalf("writeResponses = %d, want 1", n)
	}
}

// TestRecoverPanic verifies that the optional Recover layer converts handler
// panics into errors without leaking the internal stack trace to the peer.
func TestRecoverPanic(t *testing.T) {
	srv := NewServer()
	srv.Handle("boom", func(client *Client, args *MwArgs, reply *MwReply) error {
		panic("kaboom secret-internal-detail")
	})
	srv.Use(Recover)

	clt := pipePair(t, srv)
	var rep MwReply
	err := clt.Call("boom", &MwArgs{}, &rep)
	if err == nil {
		t.Fatal("expected error from recovered panic")
	}
	if strings.Contains(err.Error(), "kaboom") || strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("internal detail leaked to peer: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Fatalf("err = %q, want generic internal server error", err.Error())
	}
}

// TestNoRecoverPropagatesPanic is avoided: without Recover a panic crashes the
// process. Instead, verify double-next inside Recover returns a clean error and
// the terminal handler runs only once.
func TestDoubleNext(t *testing.T) {
	srv := NewServer()
	var handlerCalls int32
	srv.Handle("once", func(client *Client, args *MwArgs, reply *MwReply) error {
		atomic.AddInt32(&handlerCalls, 1)
		return nil
	})
	srv.Use(Recover)
	srv.Use(func(info *CallInfo, next HandlerFunc) error {
		if err := next(info); err != nil {
			return err
		}
		return next(info) // illegal second call
	})

	clt := pipePair(t, srv)
	var rep MwReply
	err := clt.Call("once", &MwArgs{}, &rep)
	if err == nil || !strings.Contains(err.Error(), ErrNextCalledTwice.Error()) {
		t.Fatalf("err = %v, want %v", err, ErrNextCalledTwice)
	}
	if atomic.LoadInt32(&handlerCalls) != 1 {
		t.Fatalf("handlerCalls = %d, want 1", handlerCalls)
	}
}

// TestDoubleNextWithoutRecover verifies buildChain panics on double next.
func TestDoubleNextWithoutRecover(t *testing.T) {
	var calls int32
	terminal := func(*CallInfo) error {
		calls++
		return nil
	}
	bad := Middleware(func(info *CallInfo, next HandlerFunc) error {
		_ = next(info)
		return next(info)
	})
	h := buildChain([]Middleware{bad}, terminal)

	defer func() {
		r := recover()
		if r != ErrNextCalledTwice {
			t.Fatalf("panic = %v, want ErrNextCalledTwice", r)
		}
		if calls != 1 {
			t.Fatalf("terminal calls = %d, want 1", calls)
		}
	}()
	_ = h(&CallInfo{})
	t.Fatal("expected panic")
}

// TestImmutability verifies the chain cannot change after serving/running.
func TestServerUseAfterServingPanics(t *testing.T) {
	srv := NewServer()
	srv.Handle("add", func(client *Client, args *MwArgs, reply *MwReply) error { return nil })
	clt := pipePair(t, srv)
	defer clt.Close()

	// Wait until the server has definitely frozen its chain.
	var rep MwReply
	if err := clt.Call("add", &MwArgs{}, &rep); err != nil {
		t.Fatal(err)
	}
	assertPanic(t, func() { srv.Use(Recover) })
}

func TestClientUseAfterRunPanics(t *testing.T) {
	srv := NewServer()
	srv.Handle("add", func(client *Client, args *MwArgs, reply *MwReply) error { return nil })
	sconn, cconn := net.Pipe()
	go srv.ServeConn(sconn)
	clt := NewClient(cconn)
	go clt.Run()
	defer clt.Close()

	// A successful round trip guarantees Run entered its read loop, after
	// the middleware chain was frozen.
	var rep MwReply
	if err := clt.Call("add", &MwArgs{}, &rep); err != nil {
		t.Fatal(err)
	}
	assertPanic(t, func() { clt.Use(Recover) })
}

// TestMiddlewareRegistrationType verifies that reflection type failures appear
// at registration time rather than at request time.
func TestMiddlewareRegistrationType(t *testing.T) {
	cases := []interface{}{
		42,
		"not a func",
		func(info *CallInfo) error { return nil },
		func(info *CallInfo, next HandlerFunc) {},
		func(info CallInfo, next HandlerFunc) error { return nil },
		func(info *CallInfo, next func()) error { return nil },
		func(info *CallInfo, next HandlerFunc) (int, error) { return 0, nil },
	}
	for i, bad := range cases {
		assertPanic(t, func() { NewServer().Use(bad) }, i)
		assertPanic(t, func() { NewClient(nil).Use(bad) }, i)
	}

	// Valid shapes register without panic.
	srv := NewServer()
	srv.Use(func(info *CallInfo, next HandlerFunc) error { return next(info) })
	srv.Use(Middleware(Recover))
}

// TestConnectionCloseCleanup verifies that every request performs its
// post-chain cleanup exactly once, and after disconnect nothing runs again.
func TestConnectionCloseCleanup(t *testing.T) {
	srv := NewServer()
	srv.Handle("add", func(client *Client, args *MwArgs, reply *MwReply) error {
		*reply = MwReply(args.A + args.B)
		return nil
	})
	var cleanups int32
	srv.Use(func(info *CallInfo, next HandlerFunc) error {
		defer atomic.AddInt32(&cleanups, 1)
		return next(info)
	})

	sconn, cconn := net.Pipe()
	go srv.ServeConn(sconn)
	clt := NewClient(cconn)
	go clt.Run()

	const n = 5
	for i := 0; i < n; i++ {
		var rep MwReply
		if err := clt.Call("add", &MwArgs{i, 1}, &rep); err != nil {
			t.Fatal(err)
		}
	}

	disconnected := make(chan struct{})
	srv.OnDisconnect(func(*Client) { close(disconnected) })

	if err := clt.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe disconnect")
	}

	if got := atomic.LoadInt32(&cleanups); got != n {
		t.Fatalf("cleanups = %d, want %d", got, n)
	}
	// Settled after close: no further cleanups.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&cleanups); got != n {
		t.Fatalf("cleanups changed after close: %d", got)
	}
}

func assertPanic(t *testing.T, f func(), msg ...interface{}) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic %v", msg)
		}
	}()
	f()
}
