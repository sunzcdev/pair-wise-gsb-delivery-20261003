// Copyright 2013 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// newConnPair returns a pair of WebSocket connections connected to each other
// over a local TCP connection.
func newConnPair(t *testing.T) (server, client *Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type acceptResult struct {
		c   net.Conn
		err error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		acceptCh <- acceptResult{c, err}
	}()

	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ar := <-acceptCh
	if ar.err != nil {
		t.Fatal(ar.err)
	}

	server = newConn(ar.c, true, 1024, 1024, nil, nil, nil)
	client = newConn(cc, false, 1024, 1024, nil, nil, nil)
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	return server, client
}

// readLoopError runs a read loop on c and reports the first read error.
func readLoopError(c *Conn) <-chan error {
	ch := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.NextReader(); err != nil {
				ch <- err
				return
			}
		}
	}()
	return ch
}

func recvCloseError(t *testing.T, ch <-chan error, code int) {
	t.Helper()
	select {
	case err := <-ch:
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Code != code {
			t.Fatalf("read loop error = %v, want CloseError with code %d", err, code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("read loop did not observe close with code %d", code)
	}
}

// The peer closes first: the read loop processes the peer's close message,
// the close handler echoes a close message, and the handshake completes
// before WaitCloseHandshake is called.
func TestWaitCloseHandshakePeerClosedFirst(t *testing.T) {
	server, client := newConnPair(t)

	handlerCalled := make(chan struct{})
	defaultHandler := server.CloseHandler()
	server.SetCloseHandler(func(code int, text string) error {
		close(handlerCalled)
		return defaultHandler(code, text)
	})

	serverReadErr := readLoopError(server)
	clientReadErr := readLoopError(client)

	// The peer initiates the close handshake.
	if err := client.WriteMessage(CloseMessage, FormatCloseMessage(CloseNormalClosure, "bye")); err != nil {
		t.Fatal(err)
	}

	// The read loop invokes the application's close handler, which echoes a
	// close message back to the peer.
	select {
	case <-handlerCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("close handler not called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.WaitCloseHandshake(ctx); err != nil {
		t.Fatalf("WaitCloseHandshake() = %v, want nil", err)
	}

	// The handshake has a single final result: a repeated call returns the
	// same result.
	if err := server.WaitCloseHandshake(ctx); err != nil {
		t.Fatalf("second WaitCloseHandshake() = %v, want nil", err)
	}

	recvCloseError(t, serverReadErr, CloseNormalClosure)
	recvCloseError(t, clientReadErr, CloseNormalClosure)
}

// Both sides close at the same time: WaitCloseHandshake blocks until the read
// loop processes the peer's close message.
func TestWaitCloseHandshakeSimultaneous(t *testing.T) {
	server, client := newConnPair(t)

	serverReadErr := readLoopError(server)
	clientReadErr := readLoopError(client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- server.WaitCloseHandshake(ctx)
	}()

	// Both sides send a close message concurrently.
	start := make(chan struct{})
	clientWriteErr := make(chan error, 1)
	go func() {
		<-start
		clientWriteErr <- client.WriteMessage(CloseMessage, FormatCloseMessage(CloseGoingAway, ""))
	}()
	close(start)
	if err := server.WriteMessage(CloseMessage, FormatCloseMessage(CloseNormalClosure, "")); err != nil {
		t.Fatal(err)
	}
	if err := <-clientWriteErr; err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("WaitCloseHandshake() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitCloseHandshake did not return")
	}

	recvCloseError(t, serverReadErr, CloseGoingAway)
	recvCloseError(t, clientReadErr, CloseNormalClosure)
}

// The peer never responds: WaitCloseHandshake returns the context's error and
// releases the connection.
func TestWaitCloseHandshakeTimeout(t *testing.T) {
	server, client := newConnPair(t)

	// Prevent the peer's default close handler from echoing a close message.
	client.SetCloseHandler(func(code int, text string) error { return nil })
	clientReadErr := readLoopError(client)

	if err := server.WriteMessage(CloseMessage, FormatCloseMessage(CloseNormalClosure, "")); err != nil {
		t.Fatal(err)
	}

	// Wait until the peer received (and ignored) the close message, so the
	// timeout below cannot be explained by a message in flight.
	recvCloseError(t, clientReadErr, CloseNormalClosure)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := server.WaitCloseHandshake(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitCloseHandshake() = %v, want %v", err, context.DeadlineExceeded)
	}

	// The handshake has a single final result: a repeated call with a fresh
	// context returns the recorded timeout.
	if err := server.WaitCloseHandshake(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second WaitCloseHandshake() = %v, want %v", err, context.DeadlineExceeded)
	}

	// The connection was released: the peer observes EOF.
	_ = client.NetConn().SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.NetConn().Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read = %v, want %v", err, io.EOF)
	}
}
