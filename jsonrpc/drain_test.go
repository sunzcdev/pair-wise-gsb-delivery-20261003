package jsonrpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cenkalti/rpc2"
)

// TestDrainOverJSON verifies the drain control notifications are encoded as
// JSON-RPC notifications (no id) and understood by the peer.
func TestDrainOverJSON(t *testing.T) {
	type Reply int
	entered := make(chan struct{})
	release := make(chan struct{})

	aConn, bConn := net.Pipe()
	a := rpc2.NewClientWithCodec(NewJSONCodec(aConn))
	b := rpc2.NewClientWithCodec(NewJSONCodec(bConn))
	a.Handle("slow", func(_ *rpc2.Client, _ int, reply *Reply) error {
		close(entered)
		<-release
		*reply = 1
		return nil
	})
	go a.Run()
	go b.Run()

	done := make(chan *rpc2.Call, 1)
	b.Go("slow", 0, new(Reply), done)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}

	drainErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drainErr <- a.Drain(ctx)
	}()

	close(release)

	select {
	case call := <-done:
		if call.Error != nil {
			t.Fatalf("in-flight call err = %v", call.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call did not finish")
	}

	select {
	case err := <-drainErr:
		if err != nil {
			t.Fatalf("drain err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return")
	}
}
