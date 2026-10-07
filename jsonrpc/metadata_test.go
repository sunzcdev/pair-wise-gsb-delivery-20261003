package jsonrpc

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/cenkalti/rpc2"
)

func pipeJSON(t *testing.T) (*rpc2.Server, *rpc2.Client) {
	t.Helper()
	sc, cc := net.Pipe()
	srv := rpc2.NewServer()
	go srv.ServeCodec(NewJSONCodec(sc))
	clt := rpc2.NewClientWithCodec(NewJSONCodec(cc))
	go clt.Run()
	t.Cleanup(func() { clt.Close() })
	return srv, clt
}

// A malformed "meta" area is rejected with an error response before the
// handler executes, and the connection stays in sync for the next request.
func TestInvalidMetadataEncodingRejected(t *testing.T) {
	srv, clt := pipeJSON(t)

	ran := make(chan struct{}, 2)
	srv.Handle("work", func(c *rpc2.Client, args *int, reply *int) error {
		ran <- struct{}{}
		*reply = 42
		return nil
	})

	// Grab the client's underlying connection by dialing a fresh pair is
	// not possible, so instead drive the existing client's codec through a
	// dedicated raw connection to another server instance.
	sc, cc := net.Pipe()
	defer cc.Close()
	rawSrv := rpc2.NewServer()
	rawSrv.Handle("work", func(c *rpc2.Client, args *int, reply *int) error {
		ran <- struct{}{}
		*reply = 7
		return nil
	})
	go rawSrv.ServeCodec(NewJSONCodec(sc))

	// Bad request: meta is an array rather than an object.
	if _, err := cc.Write([]byte(`{"method":"work","id":1,"params":[1],"meta":[1,2,3]}` + "\n")); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(cc)
	var badResp struct {
		ID    json.RawMessage `json:"id"`
		Error string          `json:"error"`
	}
	if err := dec.Decode(&badResp); err != nil {
		t.Fatal(err)
	}
	if string(badResp.ID) != "1" {
		t.Fatalf("expected id 1, got %s", badResp.ID)
	}
	if !strings.Contains(strings.ToLower(badResp.Error), "metadata") {
		t.Fatalf("expected metadata error, got: %q", badResp.Error)
	}

	// Next, valid request on the same connection is handled normally.
	enc := json.NewEncoder(cc)
	if err := enc.Encode(map[string]any{
		"method": "work", "id": 2, "params": [1]int{1},
	}); err != nil {
		t.Fatal(err)
	}
	var okResp struct {
		ID     json.RawMessage `json:"id"`
		Result int             `json:"result"`
	}
	if err := dec.Decode(&okResp); err != nil {
		t.Fatal(err)
	}
	if okResp.Result != 7 {
		t.Fatalf("unexpected result: %d", okResp.Result)
	}

	// The high-level client still works with valid metadata.
	var reply int
	ctx := rpc2.WithMetadata(context.Background(), rpc2.Metadata{"tenant": "a"})
	if err := clt.CallContext(ctx, "work", 1, &reply); err != nil {
		t.Fatal(err)
	}
	if reply != 42 {
		t.Fatalf("unexpected reply: %d", reply)
	}
}

// Metadata exceeding the outbound limits is reported back to the caller
// without hitting the wire, while ordinary calls keep working.
func TestMetadataLimits(t *testing.T) {
	srv, clt := pipeJSON(t)
	srv.Handle("work", func(c *rpc2.Client, args *int, reply *int) error {
		*reply = 1
		return nil
	})

	var reply int
	big := strings.Repeat("x", rpc2.MaxMetadataLen+1)
	err := clt.CallContext(
		rpc2.WithMetadata(context.Background(), rpc2.Metadata{"big": big}),
		"work", 1, &reply)
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("expected metadata length error, got: %v", err)
	}

	tooMany := rpc2.Metadata{}
	for i := 0; i < rpc2.MaxMetadataKeys+1; i++ {
		tooMany[string(rune('a'+i))] = "v"
	}
	err = clt.CallContext(
		rpc2.WithMetadata(context.Background(), tooMany),
		"work", 1, &reply)
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("expected metadata key-count error, got: %v", err)
	}

	if err := clt.Call("work", 1, &reply); err != nil {
		t.Fatal(err)
	}
	if reply != 1 {
		t.Fatalf("unexpected reply: %d", reply)
	}
}
