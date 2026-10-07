package rpc2

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type MetaArgs struct {
	Tag string
}

type MetaReply struct {
	PeerKeys  []string
	LocalKeys []string
	PeerVals  Metadata
	LocalVals Metadata

	// What the reverse call observed at the client side.
	ReverseKeys []string

	// View after the handler mutated its own copy.
	LocalAfterMutate []string

	// Whether a plain reverse call (no context) carried metadata.
	PlainReverseHasMD bool
}

// startMetaPair starts a server using NewMetadataCodec and a client connected
// to it.
func startMetaPair(t *testing.T, srv *Server) (*Client, net.Listener) {
	t.Helper()
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go srv.ServeCodec(NewMetadataCodec(conn))
		}
	}()

	conn, err := net.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := NewClientWithCodec(NewMetadataCodec(conn))
	go clt.Run()
	t.Cleanup(func() {
		clt.Close()
		lis.Close()
	})
	return clt, lis
}

func keys(md Metadata) []string {
	out := make([]string, 0, len(md))
	for k := range md {
		out = append(out, k)
	}
	return out
}

// TestMetadataOwnership sends two interleaved requests with distinct
// metadata and performs a nested reverse call from each handler. It verifies
// peer vs local attribution, isolation between interleaved calls, mutation
// isolation and whitelist-only propagation across the reverse call.
func TestMetadataOwnership(t *testing.T) {
	srv := NewServer()
	srv.SetMetadataWhitelist("tenant")
	srv.SetMetadataFunc(func(ctx context.Context, client *Client, method string, peer Metadata) (Metadata, error) {
		// Validate peer value, drop an untrusted key, add a local property.
		if v, ok := peer.Get("tenant"); !ok || v == "" {
			return nil, errors.New("missing tenant")
		}
		local := peer.clone()
		delete(local, "x-internal")
		local["local-only"] = []string{"confirmed"}
		return local, nil
	})

	release := make(chan struct{})
	var inside int
	var insideMu sync.Mutex

	srv.Handle("work", func(client *Client, args *MetaArgs, reply *MetaReply, ctx context.Context) error {
		peer, _ := FromIncomingContext(ctx)
		local, _ := FromLocalContext(ctx)

		reply.PeerKeys = keys(peer)
		reply.LocalKeys = keys(local)
		reply.PeerVals = peer.clone()
		reply.LocalVals = local.clone()

		// Reverse call with the handler context: only the whitelisted key
		// may cross the nesting.
		var rev struct{ Keys []string }
		err := client.CallWithContext(ctx, "echo-meta", MetaArgs{args.Tag}, &rev)
		if err != nil {
			return err
		}
		reply.ReverseKeys = rev.Keys

		// A plain reverse call (client.Call, no per-call context) must not
		// copy any attribute automatically.
		var plain struct{ Has bool }
		if err := client.Call("echo-plain", MetaArgs{args.Tag}, &plain); err != nil {
			return err
		}
		reply.PlainReverseHasMD = plain.Has

		// Wait until both requests are inside the handler at once.
		insideMu.Lock()
		inside++
		if inside == 2 {
			close(release)
		}
		insideMu.Unlock()
		<-release

		// Mutating the map read from context must not affect other calls;
		// and the local hook values must remain stable.
		local["handler-"+args.Tag] = []string{"mine"}
		delete(local, "local-only")
		localStill, _ := FromLocalContext(ctx)
		reply.LocalAfterMutate = keys(localStill)

		if _, ok := localStill.Get("local-only"); !ok {
			return errors.New("mutation leaked into context view")
		}
		return nil
	})

	clt, _ := startMetaPair(t, srv)

	seen := make(chan Metadata, 2)
	clt.Handle("echo-meta", func(client *Client, args *MetaArgs, _ *struct{}, ctx context.Context) error {
		peer, has := FromIncomingContext(ctx)
		if !has {
			return errors.New("reverse call has no incoming metadata")
		}
		seen <- peer.clone()
		return nil
	})
	clt.Handle("echo-plain", func(client *Client, args *MetaArgs, plain *struct{ Has bool }, ctx context.Context) error {
		_, plain.Has = FromIncomingContext(ctx)
		return nil
	})

	call := func(tag, tenant string) *MetaReply {
		ctx := NewOutgoingContext(context.Background(), Metadata{
			"tenant":     []string{tenant},
			"x-internal": []string{"secret"},
		})
		var rep MetaReply
		if err := clt.CallWithContext(ctx, "work", &MetaArgs{Tag: tag}, &rep); err != nil {
			t.Fatal(err)
		}
		return &rep
	}

	done := make(chan *MetaReply, 2)
	go func() { done <- call("a", "tenant-a") }()

	// Give A time to enter the handler, then interleave B.
	time.Sleep(50 * time.Millisecond)
	go func() { done <- call("b", "tenant-b") }()

	var reps [2]*MetaReply
	for i := 0; i < 2; i++ {
		reps[i] = <-done
	}

	for _, rep := range reps {
		if _, ok := rep.PeerVals.Get("x-internal"); !ok {
			t.Fatal("peer view must retain the raw submitted key")
		}
		if _, ok := rep.PeerVals.Get("local-only"); ok {
			t.Fatal("local-only must never appear in peer view")
		}
		if v, _ := rep.LocalVals.Get("x-internal"); v != "" {
			t.Fatal("hook must drop x-internal from local view")
		}
		if v, _ := rep.LocalVals.Get("local-only"); v != "confirmed" {
			t.Fatal("hook must add local-only to local view")
		}
		var foundLocal bool
		for _, k := range rep.LocalAfterMutate {
			if len(k) > len("handler-") && k[:len("handler-")] == "handler-" {
				foundLocal = true
			}
		}
		if foundLocal {
			t.Fatal("mutation of returned map leaked back into context view")
		}
	}

	// Each interleaved request keeps its own tenant.
	tenants := map[string]bool{}
	for _, rep := range reps {
		v, _ := rep.PeerVals.Get("tenant")
		tenants[v] = true
	}
	if len(tenants) != 2 {
		t.Fatalf("expected two distinct tenants, got %v", tenants)
	}

	// Reverse calls only see the whitelisted key.
	for i := 0; i < 2; i++ {
		md := <-seen
		if len(md) != 1 {
			t.Fatalf("reverse call must carry exactly one key, got %v", keys(md))
		}
		if _, ok := md["tenant"]; !ok {
			t.Fatalf("reverse call must carry tenant, got %v", keys(md))
		}
	}

	for _, rep := range reps {
		if rep.PlainReverseHasMD {
			t.Fatal("plain reverse call must not carry metadata")
		}
	}
}

// TestMetadataRejectedBeforeHandler feeds raw, invalid frames straight into a
// metadata codec connection and asserts the handler never runs and the
// connection keeps serving a well-formed request afterwards.
func TestMetadataRejectedBeforeHandler(t *testing.T) {
	ran := make(chan struct{}, 1)
	srv := NewServer()
	srv.Handle("touch", func(client *Client, _ *struct{}, _ *struct{}) error {
		ran <- struct{}{}
		return nil
	})
	clt, _ := startMetaPair(t, srv)

	// 1. Excessive number of metadata keys.
	big := make(Metadata, MaxMetadataKeys+1)
	for i := 0; i <= MaxMetadataKeys; i++ {
		big["k"+itoa(i)] = []string{"v"}
	}
	var rep struct{}
	err := clt.CallWithContext(
		NewOutgoingContext(context.Background(), big),
		"touch", &struct{}{}, &rep)
	if !errors.Is(err, ErrTooManyMetadataKeys) && err.Error() != ErrTooManyMetadataKeys.Error() {
		t.Fatalf("expected too many keys error, got %v", err)
	}

	// 2. Hook rejection must likewise prevent handler execution.
	srv2 := NewServer()
	srv2.SetMetadataFunc(func(ctx context.Context, client *Client, method string, peer Metadata) (Metadata, error) {
		return nil, errors.New("denied by hook")
	})
	srv2.Handle("touch", func(client *Client, _ *struct{}, _ *struct{}) error {
		ran <- struct{}{}
		return nil
	})
	clt2, _ := startMetaPair(t, srv2)
	err = clt2.CallWithContext(
		NewOutgoingContext(context.Background(), Metadata{"tenant": []string{"x"}}),
		"touch", &struct{}{}, &rep)
	if err == nil || err.Error() != "denied by hook" {
		t.Fatalf("expected hook denial, got %v", err)
	}

	// 3. Hand-crafted frame whose declared metadata length exceeds the limit.
	lis2, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := lis2.Accept()
			if err != nil {
				return
			}
			go srv2.ServeCodec(NewMetadataCodec(c))
		}
	}()
	t.Cleanup(func() { lis2.Close() })
	raw, err := net.Dial("tcp4", lis2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	codec := NewMetadataCodec(raw)
	bad := &metaHeader{
		Seq:     1,
		Method:  "touch",
		HasMeta: true,
		MetaLen: 16,
		Meta:    bytes.Repeat([]byte("x"), 16), // valid length, invalid gob payload
	}
	writeRawFrame(raw, bad)
	writeRawFrame(raw, struct{}{})

	// The server rejects before running the handler and sends an error
	// response; read it back from the client side of the same connection.
	var req Request
	var resp Response
	if err := codec.ReadHeader(&req, &resp); err != nil {
		t.Fatalf("reading rejection response header: %v", err)
	}
	if resp.Error == "" {
		t.Fatal("expected non-empty rejection error for invalid metadata encoding")
	}
	if err := codec.ReadResponseBody(nil); err != nil {
		t.Fatal(err)
	}

	// A declared MetaLen beyond the limit maps to ErrMetadataTooLarge.
	bad2 := &metaHeader{
		Seq:     2,
		Method:  "touch",
		HasMeta: true,
		MetaLen: MaxMetadataLen + 1,
		Meta:    bytes.Repeat([]byte("x"), 16),
	}
	writeRawFrame(raw, bad2)
	writeRawFrame(raw, struct{}{})
	req = Request{}
	resp = Response{}
	if err := codec.ReadHeader(&req, &resp); err != nil {
		t.Fatalf("reading rejection response header: %v", err)
	}
	if resp.Error != ErrMetadataTooLarge.Error() {
		t.Fatalf("expected ErrMetadataTooLarge, got %q", resp.Error)
	}
	if err := codec.ReadResponseBody(nil); err != nil {
		t.Fatal(err)
	}

	// The server rejects before running the handler and sends an error
	// response; read it back from the client side of the same connection.
	select {
	case <-ran:
		t.Fatal("handler must not run for rejected metadata")
	default:
	}

	// Give the server a moment to reject and verify later well-formed calls
	// on the first connection still work. The oversized raw connection is
	// torn down by the server, so use clt which only sent a recoverable
	// too-many-keys frame.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := clt.CallWithContext(ctx, "touch", &struct{}{}, &rep); err != nil {
		t.Fatalf("connection must recover after a rejected request: %v", err)
	}
}

// TestOldCodecUnaffected proves plain gob codec carries no metadata and
// ordinary 3-argument calls keep working.
func TestOldCodecUnaffected(t *testing.T) {
	if _, ok := NewGobCodec(nil).(MetadataCodec); ok {
		t.Fatal("gob codec must not implement MetadataCodec")
	}

	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer()
	type Args struct{ A, B int }
	type Reply int
	srv.Handle("add", func(client *Client, args *Args, reply *Reply) error {
		*reply = Reply(args.A + args.B)
		return nil
	})
	go srv.Accept(lis)
	t.Cleanup(func() { lis.Close() })

	conn, err := net.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := NewClient(conn)
	go clt.Run()
	var rep Reply
	if err := clt.CallWithContext(
		NewOutgoingContext(context.Background(), Metadata{"tenant": []string{"t"}}),
		"add", Args{1, 2}, &rep); err != nil {
		t.Fatal(err)
	}
	if rep != 3 {
		t.Fatalf("unexpected reply %d", rep)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func writeRawFrame(w io.Writer, v interface{}) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		panic(err)
	}
	var lenBuf [10]byte
	n := putUvarint(lenBuf[:], uint64(buf.Len()))
	if _, err := w.Write(lenBuf[:n]); err != nil {
		panic(err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		panic(err)
	}
}

func putUvarint(buf []byte, x uint64) int {
	i := 0
	for x >= 0x80 {
		buf[i] = byte(x) | 0x80
		x >>= 7
		i++
	}
	buf[i] = byte(x)
	return i + 1
}
