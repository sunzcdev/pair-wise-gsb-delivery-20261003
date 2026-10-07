package rpc2

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type MWArgs struct{ A int }
type MWReply struct{ B int }

func pipeConns() (clientConn, serverConn net.Conn) {
	return net.Pipe()
}

func servePipe(srv *Server) *Client {
	clientConn, serverConn := pipeConns()
	go srv.ServeConn(serverConn)
	clt := NewClient(clientConn)
	go clt.Run()
	return clt
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMiddlewareChainOrder(t *testing.T) {
	srv := NewServer()

	var mu sync.Mutex
	var events []string
	record := func(s string) {
		mu.Lock()
		events = append(events, s)
		mu.Unlock()
	}

	var seenCtx Context
	mw := func(name string) Middleware {
		return func(next HandlerFunc) HandlerFunc {
			return func(ctx *Context) error {
				record(name + ":before")
				if name == "auth" {
					seenCtx = *ctx
				}
				err := next(ctx)
				record(fmt.Sprintf("%s:after:reply=%d", name, ctx.Reply.(*MWReply).B))
				return err
			}
		}
	}
	srv.Use(mw("auth"), mw("timing"))
	srv.Handle("add", func(client *Client, args *MWArgs, reply *MWReply) error {
		record("handler")
		reply.B = args.A + 1
		return nil
	})

	clt := servePipe(srv)
	defer clt.Close()

	var rep MWReply
	if err := clt.Call("add", &MWArgs{A: 1}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.B != 2 {
		t.Fatalf("unexpected reply: %d", rep.B)
	}

	want := []string{
		"auth:before",
		"timing:before",
		"handler",
		"timing:after:reply=2",
		"auth:after:reply=2",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}

	// Middleware must see method name, request number and connection.
	if seenCtx.Method != "add" {
		t.Fatalf("ctx.Method = %q", seenCtx.Method)
	}
	if seenCtx.Seq == 0 {
		t.Fatal("ctx.Seq must be non-zero for a call")
	}
	if seenCtx.Notification {
		t.Fatal("ctx.Notification must be false for a call")
	}
	if seenCtx.Client == nil || seenCtx.Client.State == nil {
		t.Fatal("ctx.Client with State must be set")
	}
	if arg, ok := seenCtx.Args.(*MWArgs); !ok || arg.A != 1 {
		t.Fatalf("ctx.Args = %#v", seenCtx.Args)
	}
}

func TestMiddlewareEarlyReject(t *testing.T) {
	srv := NewServer()
	srv.Use(func(next HandlerFunc) HandlerFunc {
		return func(ctx *Context) error {
			return errors.New("access denied")
		}
	})
	var called int32
	srv.Handle("add", func(client *Client, args *MWArgs, reply *MWReply) error {
		atomic.AddInt32(&called, 1)
		return nil
	})

	clt := servePipe(srv)
	defer clt.Close()

	err := clt.Call("add", &MWArgs{A: 1}, &MWReply{})
	if err == nil || err.Error() != "access denied" {
		t.Fatalf("expected rejection error, got %v", err)
	}
	if n := atomic.LoadInt32(&called); n != 0 {
		t.Fatalf("handler ran %d times despite rejection", n)
	}
}

func TestMiddlewareRecovery(t *testing.T) {
	srv := NewServer()
	srv.Use(Recovery())
	srv.Handle("boom", func(client *Client, args *MWArgs, reply *MWReply) error {
		panic("secret database password")
	})

	clt := servePipe(srv)
	defer clt.Close()

	err := clt.Call("boom", &MWArgs{}, &MWReply{})
	if err == nil {
		t.Fatal("expected error from panicking handler")
	}
	if err.Error() != ErrInternal.Error() {
		t.Fatalf("error = %q, want %q", err.Error(), ErrInternal.Error())
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("internal panic details leaked to peer: %q", err.Error())
	}
}

func TestMiddlewareNextCalledTwice(t *testing.T) {
	var runs int32
	bad := func(next HandlerFunc) HandlerFunc {
		return func(ctx *Context) error {
			if err := next(ctx); err != nil {
				return err
			}
			return next(ctx) // must be prevented
		}
	}
	final := func(ctx *Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second next call")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, errNextCalledTwice) {
			t.Fatalf("panic = %v, want %v", r, errNextCalledTwice)
		}
		if n := atomic.LoadInt32(&runs); n != 1 {
			t.Fatalf("handler ran %d times, want 1", n)
		}
	}()
	_ = runChain([]Middleware{bad}, final, &Context{})
}

func TestMiddlewareNotification(t *testing.T) {
	srv := NewServer()
	type info struct {
		method string
		seq    uint64
		notif  bool
	}
	seen := make(chan info, 1)
	srv.Use(func(next HandlerFunc) HandlerFunc {
		return func(ctx *Context) error {
			seen <- info{ctx.Method, ctx.Seq, ctx.Notification}
			return next(ctx)
		}
	})
	handled := make(chan struct{}, 1)
	srv.Handle("set", func(client *Client, args *MWArgs, reply *MWReply) error {
		handled <- struct{}{}
		return nil
	})

	clt := servePipe(srv)
	defer clt.Close()

	if err := clt.Notify("set", &MWArgs{A: 6}); err != nil {
		t.Fatal(err)
	}
	select {
	case i := <-seen:
		if i.method != "set" || i.seq != 0 || !i.notif {
			t.Fatalf("middleware saw %+v, want method=set seq=0 notification=true", i)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("middleware did not run for notification")
	}
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run for notification")
	}
}

func TestMiddlewareCleanupOnDisconnect(t *testing.T) {
	srv := NewServer()
	var cleanups int32
	srv.Use(func(next HandlerFunc) HandlerFunc {
		return func(ctx *Context) error {
			defer atomic.AddInt32(&cleanups, 1)
			return next(ctx)
		}
	})
	srv.Handle("add", func(client *Client, args *MWArgs, reply *MWReply) error {
		reply.B = args.A + 1
		return nil
	})
	disconnected := make(chan struct{})
	srv.OnDisconnect(func(*Client) { close(disconnected) })

	clt := servePipe(srv)

	const calls = 3
	for i := 0; i < calls; i++ {
		var rep MWReply
		if err := clt.Call("add", &MWArgs{A: i}, &rep); err != nil {
			t.Fatal(err)
		}
	}
	if n := atomic.LoadInt32(&cleanups); n != calls {
		t.Fatalf("cleanups = %d, want %d", n, calls)
	}

	// Notifications run through the same chain exactly once.
	if err := clt.Notify("add", &MWArgs{A: 99}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "notification cleanup", func() bool {
		return atomic.LoadInt32(&cleanups) == calls+1
	})

	// Closing the connection must not trigger additional cleanups.
	if err := clt.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe disconnect")
	}
	if n := atomic.LoadInt32(&cleanups); n != calls+1 {
		t.Fatalf("cleanups after disconnect = %d, want %d", n, calls+1)
	}
}

func TestServerUseAfterStart(t *testing.T) {
	srv := NewServer()
	srv.Handle("add", func(client *Client, args *MWArgs, reply *MWReply) error {
		return nil
	})
	clt := servePipe(srv)
	defer clt.Close()

	var rep MWReply
	if err := clt.Call("add", &MWArgs{}, &rep); err != nil {
		t.Fatal(err)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when registering middleware after serving started")
		}
	}()
	srv.Use(func(next HandlerFunc) HandlerFunc { return next })
}

func TestClientMiddleware(t *testing.T) {
	srv := NewServer()
	srv.Handle("trigger", func(client *Client, args *MWArgs, reply *MWReply) error {
		var rep MWReply
		if err := client.Call("double", &MWArgs{A: 21}, &rep); err != nil {
			return err
		}
		reply.B = rep.B
		return nil
	})

	clientConn, serverConn := pipeConns()
	go srv.ServeConn(serverConn)
	clt := NewClient(clientConn)

	var ran int32
	clt.Use(func(next HandlerFunc) HandlerFunc {
		return func(ctx *Context) error {
			if ctx.Method != "double" {
				t.Errorf("unexpected method in client middleware: %s", ctx.Method)
			}
			atomic.AddInt32(&ran, 1)
			return next(ctx)
		}
	})
	clt.Handle("double", func(client *Client, args *MWArgs, reply *MWReply) error {
		reply.B = args.A * 2
		return nil
	})
	go clt.Run()
	defer clt.Close()

	var rep MWReply
	if err := clt.Call("trigger", &MWArgs{}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.B != 42 {
		t.Fatalf("unexpected reply: %d", rep.B)
	}
	if n := atomic.LoadInt32(&ran); n != 1 {
		t.Fatalf("client middleware ran %d times, want 1", n)
	}
}

func TestHandleTypeChecks(t *testing.T) {
	// Built via reflect to pass a Client by value without tripping copylocks.
	firstNotPtr := reflect.MakeFunc(
		reflect.FuncOf(
			[]reflect.Type{reflect.TypeOf(Client{}), reflect.TypeOf(&MWArgs{}), reflect.TypeOf(&MWReply{})},
			[]reflect.Type{typeOfError}, false),
		func([]reflect.Value) []reflect.Value { return []reflect.Value{reflect.Zero(typeOfError)} },
	).Interface()

	cases := map[string]interface{}{
		"no args":          func() {},
		"two ins":          func(*Client, *MWArgs) {},
		"first not client": func(*MWArgs, *MWArgs, *MWReply) error { return nil },
		"first not ptr":    firstNotPtr,
		"reply not ptr":    func(*Client, *MWArgs, MWReply) error { return nil },
		"no return":        func(*Client, *MWArgs, *MWReply) {},
		"return not error": func(*Client, *MWArgs, *MWReply) int { return 0 },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic at registration time")
				}
			}()
			NewServer().Handle("m", fn)
		})
	}
}
