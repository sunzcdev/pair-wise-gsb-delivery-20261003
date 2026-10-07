package rpc2_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/cenkalti/rpc2"
	"github.com/cenkalti/rpc2/jsonrpc"
)

type MdArgs struct{ Tag string }
type MdReply struct {
	Peer      map[string]string
	Confirmed map[string]string
}

func serveJSON(t *testing.T, srv *rpc2.Server) (*rpc2.Client, net.Listener) {
	t.Helper()
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		srv.ServeCodec(jsonrpc.NewJSONCodec(conn))
	}()
	conn, err := net.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := rpc2.NewClientWithCodec(jsonrpc.NewJSONCodec(conn))
	go clt.Run()
	t.Cleanup(func() {
		clt.Close()
		lis.Close()
	})
	return clt, lis
}

// Two interleaved requests plus one nested reverse call prove: peer values
// differ from locally confirmed values; the interceptor validates, deletes
// and supplements before handlers run; handler mutations are isolated; and
// a reverse RPC propagates only the whitelisted inbound key.
func TestMetadataInterleavedAndReverse(t *testing.T) {
	srv := rpc2.NewServer()

	// Require tenant, drop a peer-only hint, add a local routing value.
	srv.SetMetadataInterceptor(func(ctx context.Context, peer, confirmed rpc2.Metadata) error {
		if peer["tenant"] == "" {
			return rpc2.ServerError("missing tenant")
		}
		delete(confirmed, "peer-only")
		confirmed["local-route"] = "route-" + peer["tenant"]
		return nil
	})
	// Reverse calls from handlers auto-propagate only the trace id.
	srv.SetMetadataWhitelist("trace")

	release := make(chan struct{})
	entered := make(chan struct{}, 2)

	srv.Handle("work", func(client *rpc2.Client, ctx context.Context, args *MdArgs, reply *MdReply) error {
		reply.Peer = rpc2.PeerMetadata(ctx)
		reply.Confirmed = rpc2.ConfirmedMetadata(ctx)

		// Mutating the read maps must not leak to the other request or to
		// the reverse call.
		p := rpc2.PeerMetadata(ctx)
		p["tenant"] = "MUTATED"
		cf := rpc2.ConfirmedMetadata(ctx)
		cf["local-route"] = "MUTATED"

		entered <- struct{}{}
		<-release

		var rev MdReply
		err := client.CallContext(
			rpc2.WithMetadata(ctx, rpc2.Metadata{"extra": "x"}),
			"reverse", MdArgs{}, &rev)
		if err != nil {
			return err
		}
		if _, ok := rev.Confirmed["tenant"]; ok {
			t.Errorf("tenant must not be auto-propagated: %v", rev.Confirmed)
		}
		if args.Tag == "a" && rev.Confirmed["trace"] != "t1" {
			t.Errorf("whitelisted trace not propagated on tagged request: %v", rev.Confirmed)
		}
		if args.Tag == "b" {
			if _, ok := rev.Confirmed["trace"]; ok {
				t.Errorf("trace must not appear when not submitted: %v", rev.Confirmed)
			}
		}
		if rev.Confirmed["extra"] != "x" {
			t.Errorf("explicit metadata not sent: %v", rev.Confirmed)
		}
		return nil
	})

	clt, _ := serveJSON(t, srv)

	clt.SetMetadataInterceptor(func(ctx context.Context, peer, confirmed rpc2.Metadata) error {
		return nil
	})
	clt.Handle("reverse", func(client *rpc2.Client, ctx context.Context, args *MdArgs, reply *MdReply) error {
		reply.Peer = rpc2.PeerMetadata(ctx)
		reply.Confirmed = rpc2.ConfirmedMetadata(ctx)
		return nil
	})

	type callResult struct {
		reply MdReply
		err   error
	}
	results := make(chan callResult, 2)
	call := func(tenant, trace string) {
		go func() {
			var r MdReply
			md := rpc2.Metadata{"tenant": tenant, "peer-only": "secret"}
			if trace != "" {
				md["trace"] = trace
			}
			err := clt.CallContext(
				rpc2.WithMetadata(context.Background(), md),
				"work", MdArgs{Tag: tenant}, &r)
			results <- callResult{r, err}
		}()
	}
	call("a", "t1")
	call("b", "")

	<-entered
	<-entered
	close(release)

	seenTenants := map[string]bool{}
	seenRoutes := map[string]bool{}
	for i := 0; i < 2; i++ {
		res := <-results
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.reply.Peer["peer-only"] != "secret" {
			t.Errorf("peer view must retain peer-only: %v", res.reply.Peer)
		}
		if _, ok := res.reply.Confirmed["peer-only"]; ok {
			t.Errorf("interceptor deletion ineffective: %v", res.reply.Confirmed)
		}
		if res.reply.Confirmed["local-route"] != "route-"+res.reply.Peer["tenant"] {
			t.Errorf("local supplement wrong: %v", res.reply.Confirmed)
		}
		seenTenants[res.reply.Peer["tenant"]] = true
		seenRoutes[res.reply.Confirmed["local-route"]] = true
	}
	if !seenTenants["a"] || !seenTenants["b"] {
		t.Fatalf("interleaved requests shared peer metadata: %v", seenTenants)
	}
	if !seenRoutes["route-a"] || !seenRoutes["route-b"] {
		t.Fatalf("interleaved requests shared confirmed metadata: %v", seenRoutes)
	}
}

// An interceptor error rejects the request before the handler runs, and the
// connection stays usable for a following valid request.
func TestMetadataRejectedBeforeHandler(t *testing.T) {
	srv := rpc2.NewServer()
	srv.SetMetadataInterceptor(func(ctx context.Context, peer, confirmed rpc2.Metadata) error {
		if peer["tenant"] == "" {
			return rpc2.ServerError("missing tenant")
		}
		return nil
	})
	ran := make(chan struct{}, 1)
	srv.Handle("work", func(client *rpc2.Client, ctx context.Context, args *MdArgs, reply *struct{}) error {
		ran <- struct{}{}
		return nil
	})
	clt, _ := serveJSON(t, srv)

	err := clt.CallContext(context.Background(), "work", &MdArgs{}, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "missing tenant") {
		t.Fatalf("expected missing tenant rejection, got: %v", err)
	}
	select {
	case <-ran:
		t.Fatal("handler must not run for rejected request")
	default:
	}

	err = clt.CallContext(
		rpc2.WithMetadata(context.Background(), rpc2.Metadata{"tenant": "a"}),
		"work", &MdArgs{}, &struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
	default:
		t.Fatal("handler should run for valid request")
	}
}

// Plain gob calls keep working and never expose metadata even when the
// caller attaches some, because the gob codec has no metadata area.
func TestGobPlainCallUnaffected(t *testing.T) {
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	type Args struct{ A, B int }
	type Reply int

	srv := rpc2.NewServer()
	srv.Handle("add", func(client *rpc2.Client, ctx context.Context, args *Args, reply *Reply) error {
		if len(rpc2.PeerMetadata(ctx)) != 0 || len(rpc2.ConfirmedMetadata(ctx)) != 0 {
			t.Fatal("gob handler must see no metadata")
		}
		*reply = Reply(args.A + args.B)
		return nil
	})
	go srv.Accept(lis)

	conn, err := net.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := rpc2.NewClient(conn)
	go clt.Run()
	defer clt.Close()

	var rep Reply
	err = clt.CallContext(
		rpc2.WithMetadata(context.Background(), rpc2.Metadata{"tenant": "a"}),
		"add", &Args{1, 2}, &rep)
	if err != nil {
		t.Fatal(err)
	}
	if rep != 3 {
		t.Fatalf("unexpected reply: %d", rep)
	}
}
