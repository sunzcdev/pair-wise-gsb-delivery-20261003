package rpc2

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// newDrainTestPair starts srv on a local TCP listener and returns a
// connected client plus a channel that yields the server-side client.
func newDrainTestPair(t *testing.T, srv *Server) (*Client, <-chan *Client) {
	t.Helper()
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvCltCh := make(chan *Client, 1)
	srv.OnConnect(func(c *Client) { srvCltCh <- c })
	go srv.Accept(lis)

	conn, err := net.Dial("tcp4", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	clt := NewClient(conn)
	go clt.Run()
	t.Cleanup(func() {
		clt.Close()
		lis.Close()
	})
	return clt, srvCltCh
}

func waitDraining(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !c.Draining() {
		if time.Now().After(deadline) {
			t.Fatal("client did not enter draining state")
		}
		time.Sleep(time.Millisecond)
	}
}

func recvErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
		return nil
	}
}

// Draining client notifies the peer; the peer stops new requests while
// its accepted work finishes.
func TestDrainNotification(t *testing.T) {
	srv := NewServer()
	clt, srvCltCh := newDrainTestPair(t, srv)

	started := make(chan struct{})
	release := make(chan struct{})
	clt.Handle("slow", func(client *Client, _ int, _ *int) error {
		close(started)
		<-release
		return nil
	})

	srvClt := <-srvCltCh

	// Server originates a call that stays in flight during the drain.
	srvCallErr := make(chan error, 1)
	go func() {
		var rep int
		srvCallErr <- srvClt.Call("slow", 1, &rep)
	}()
	<-started

	drainErr := make(chan error, 1)
	go func() { drainErr <- clt.Drain(context.Background()) }()

	// The peer must be notified and reject new top-level requests.
	waitDraining(t, srvClt)
	var rep int
	if err := srvClt.Call("slow", 1, &rep); !errors.Is(err, ErrDraining) {
		t.Fatalf("expected ErrDraining, got %v", err)
	}

	// Let accepted work finish; both sides close and Drain succeeds.
	close(release)
	if err := recvErr(t, srvCallErr); err != nil {
		t.Fatalf("in-flight call failed: %v", err)
	}
	if err := recvErr(t, drainErr); err != nil {
		t.Fatalf("Drain failed: %v", err)
	}
}

// A handler accepted before the drain may still call back to the peer;
// both sides must let such nested calls complete instead of waiting for
// each other.
func TestDrainNestedCallback(t *testing.T) {
	srv := NewServer()
	outerStarted := make(chan struct{})
	proceed := make(chan struct{})
	srv.Handle("outer", func(client *Client, _ int, _ *int) error {
		close(outerStarted)
		<-proceed
		// Nested call back to the peer while both sides are draining.
		var rep int
		return client.Call("inner", 1, &rep)
	})
	clt, _ := newDrainTestPair(t, srv)

	innerCalled := make(chan struct{})
	clt.Handle("inner", func(client *Client, _ int, _ *int) error {
		close(innerCalled)
		return nil
	})

	callErr := make(chan error, 1)
	go func() {
		var rep int
		callErr <- clt.Call("outer", 1, &rep)
	}()
	<-outerStarted

	drainErr := make(chan error, 1)
	go func() { drainErr <- clt.Drain(context.Background()) }()
	close(proceed)

	select {
	case <-innerCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("nested callback was not served during drain")
	}
	if err := recvErr(t, callErr); err != nil {
		t.Fatalf("outer call failed: %v", err)
	}
	if err := recvErr(t, drainErr); err != nil {
		t.Fatalf("Drain failed: %v", err)
	}
}

// Multiple Drain calls (concurrent and after completion) all observe the
// same final outcome; new local calls are rejected while draining.
func TestDrainRepeated(t *testing.T) {
	srv := NewServer()
	started := make(chan struct{})
	release := make(chan struct{})
	srv.Handle("block", func(client *Client, _ int, _ *int) error {
		close(started)
		<-release
		return nil
	})
	clt, _ := newDrainTestPair(t, srv)

	callErr := make(chan error, 1)
	go func() {
		var rep int
		callErr <- clt.Call("block", 1, &rep)
	}()
	<-started

	const waiters = 3
	drainErrs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() { drainErrs <- clt.Drain(context.Background()) }()
	}
	waitDraining(t, clt)

	// New locally initiated calls are rejected while draining.
	var rep int
	if err := clt.Call("block", 1, &rep); !errors.Is(err, ErrDraining) {
		t.Fatalf("expected ErrDraining, got %v", err)
	}

	close(release)
	if err := recvErr(t, callErr); err != nil {
		t.Fatalf("in-flight call failed: %v", err)
	}
	for i := 0; i < waiters; i++ {
		if err := recvErr(t, drainErrs); err != nil {
			t.Fatalf("Drain waiter %d failed: %v", i, err)
		}
	}
	// A Drain call after completion reports the same outcome.
	if err := clt.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after completion failed: %v", err)
	}
}

// If the read loop fails before the drain completes, Drain returns the
// connection error and every waiter gets a deterministic final state.
func TestDrainReadLoopError(t *testing.T) {
	srv := NewServer()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv.Handle("block", func(client *Client, _ int, _ *int) error {
		close(started)
		<-release
		return nil
	})
	clt, srvCltCh := newDrainTestPair(t, srv)
	srvClt := <-srvCltCh

	callErr := make(chan error, 1)
	go func() {
		var rep int
		callErr <- clt.Call("block", 1, &rep)
	}()
	<-started

	drainErr1 := make(chan error, 1)
	drainErr2 := make(chan error, 1)
	go func() { drainErr1 <- clt.Drain(context.Background()) }()
	go func() { drainErr2 <- clt.Drain(context.Background()) }()
	waitDraining(t, clt)

	// The connection breaks before the drain can complete.
	srvClt.Close()

	if err := recvErr(t, drainErr1); err == nil {
		t.Fatal("expected Drain to return the connection error")
	}
	if err := recvErr(t, drainErr2); err == nil {
		t.Fatal("expected second Drain waiter to return the connection error")
	}
	if err := recvErr(t, callErr); err == nil {
		t.Fatal("expected pending call to be terminated with an error")
	}
}

// A timeout only ends the wait: the connection is closed as agreed and
// in-flight calls are terminated with an error, not a fabricated success.
func TestDrainTimeout(t *testing.T) {
	srv := NewServer()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv.Handle("block", func(client *Client, _ int, _ *int) error {
		close(started)
		<-release
		return nil
	})
	clt, _ := newDrainTestPair(t, srv)

	callErr := make(chan error, 1)
	go func() {
		var rep int
		callErr <- clt.Call("block", 1, &rep)
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := clt.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}

	select {
	case <-clt.DisconnectNotify():
	case <-time.After(5 * time.Second):
		t.Fatal("connection was not closed after drain timeout")
	}
	if err := recvErr(t, callErr); err == nil {
		t.Fatal("pending call must not report success after drain timeout")
	}
}
