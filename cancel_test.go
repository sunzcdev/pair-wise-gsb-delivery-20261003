// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// writeRawFrame writes a single WebSocket frame directly to the network
// connection, bypassing the library's message writer. This lets tests
// interleave control frames with data fragments.
func writeRawFrame(t *testing.T, c *Conn, fin bool, rsv1 bool, opcode int, payload []byte) {
	t.Helper()
	b0 := byte(opcode)
	if fin {
		b0 |= finalBit
	}
	if rsv1 {
		b0 |= rsv1Bit
	}

	var hdr []byte
	switch n := len(payload); {
	case n <= 125:
		hdr = []byte{b0, byte(n)}
	case n <= 65535:
		hdr = make([]byte, 4)
		hdr[0], hdr[1] = b0, 126
		hdr[2], hdr[3] = byte(n>>8), byte(n)
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = b0, 127
		for i := 0; i < 8; i++ {
			hdr[2+i] = byte(uint64(n) >> uint64(56-8*i))
		}
	}

	payload = append([]byte(nil), payload...)
	if !c.isServer {
		hdr[1] |= maskBit
		key := newMaskKey()
		hdr = append(hdr, key[:]...)
		maskBytes(key, 0, payload)
	}

	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	if _, err := c.conn.Write(append(hdr, payload...)); err != nil {
		t.Fatalf("writeRawFrame: %v", err)
	}
}

func deflatePayload(t *testing.T, p []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := fw.Flush(); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if len(out) < 4 {
		t.Fatal("compressed payload too short")
	}
	// permessage-deflate strips the trailing 0x00 0x00 0xff 0xff.
	return out[:len(out)-4]
}

func dialCancelPair(t *testing.T, compress bool, handler func(*Conn)) (*Conn, func()) {
	t.Helper()
	upgrader := Upgrader{EnableCompression: compress}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		handler(c)
	}))

	dialer := Dialer{EnableCompression: compress, HandshakeTimeout: 5 * time.Second}
	cl, _, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return cl, func() {
		cl.Close()
		srv.Close()
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(msg)
	}
}

// Acceptance scenario, uncompressed: a large message is delivered in two
// fragments; the client reads part of it and cancels. A ping is interleaved
// between the fragments while draining; afterwards the client reads a
// complete notification on the same connection.
func TestCancelHalfMessageThenNotification(t *testing.T) {
	payload := make([]byte, 8192)
	rng := rand.New(rand.NewSource(42))
	rng.Read(payload)

	gotPong := make(chan struct{}, 1)

	client, cleanup := dialCancelPair(t, false, func(server *Conn) {
		writeRawFrame(t, server, false, false, BinaryMessage, payload[:4096])

		// Wait for the client to signal (it has read part of the fragment).
		server.SetReadLimit(64)
		if _, _, err := server.ReadMessage(); err != nil {
			t.Errorf("server read signal: %v", err)
			return
		}

		// Interleave a control frame inside the message while the client is
		// draining it.
		if err := server.WriteControl(PingMessage, []byte("drain-ping"), time.Now().Add(time.Second)); err != nil {
			t.Errorf("server write ping: %v", err)
			return
		}
		writeRawFrame(t, server, true, false, continuationFrame, payload[4096:])
		if err := server.WriteMessage(TextMessage, []byte("notification")); err != nil {
			t.Errorf("server write notification: %v", err)
			return
		}

		server.SetPongHandler(func(string) error {
			select {
			case gotPong <- struct{}{}:
			default:
			}
			return nil
		})
		if _, _, err := server.ReadMessage(); err != nil {
			t.Errorf("server read done: %v", err)
		}
	})
	defer cleanup()

	op, r, err := client.NextReaderCancellable()
	if err != nil || op != BinaryMessage {
		t.Fatalf("NextReaderCancellable: op=%d err=%v", op, err)
	}
	cr := AsCancellableReader(r)
	if cr == nil {
		t.Fatal("reader is not a CancellableReader")
	}

	head := make([]byte, 128)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if !bytes.Equal(head, payload[:128]) {
		t.Fatal("first bytes mismatch")
	}

	if err := client.WriteMessage(TextMessage, []byte("go")); err != nil {
		t.Fatalf("signal: %v", err)
	}

	if err := cr.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Reads from the canceled reader never deliver more data.
	if _, err := r.Read(make([]byte, 16)); !errors.Is(err, ErrMessageCanceled) {
		t.Fatalf("read after cancel = %v, want ErrMessageCanceled", err)
	}
	// Idempotent cancel.
	if err := cr.Cancel(); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}

	waitSignal(t, gotPong, "pong for drain-ping was not observed")

	if err := client.WriteMessage(TextMessage, []byte("done")); err != nil {
		t.Fatalf("done: %v", err)
	}

	op2, msg, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if op2 != TextMessage || string(msg) != "notification" {
		t.Fatalf("notification = (%d, %q)", op2, msg)
	}
}

// Compressed acceptance scenario: a single large compressed message is
// canceled while partly read. The drain skips the wire remainder and the
// decompressor (with any buffered input) is dropped; subsequent compressed
// messages must decompress correctly with no residual state.
func TestCancelCompressedMessageResetsState(t *testing.T) {
	payload := bytes.Repeat([]byte("gorilla-websocket-"), 500)
	follow1 := []byte("first-after-cancel:" + strings.Repeat("a", 4000))
	follow2 := []byte("second-after-cancel:" + strings.Repeat("b", 4000))

	client, cleanup := dialCancelPair(t, true, func(server *Conn) {
		// Single-frame compressed messages (the library's flate reader does
		// not support deflate streams split across fragments).
		if err := server.WriteMessage(BinaryMessage, payload); err != nil {
			t.Errorf("write big: %v", err)
			return
		}
		if err := server.WriteMessage(TextMessage, follow1); err != nil {
			t.Errorf("write follow1: %v", err)
			return
		}
		if err := server.WriteMessage(TextMessage, follow2); err != nil {
			t.Errorf("write follow2: %v", err)
		}
		server.SetReadLimit(64)
		if _, _, err := server.ReadMessage(); err != nil {
			t.Errorf("server read done: %v", err)
		}
	})
	defer cleanup()

	op, r, err := client.NextReaderCancellable()
	if err != nil || op != BinaryMessage {
		t.Fatalf("NextReaderCancellable: op=%d err=%v", op, err)
	}
	cr := AsCancellableReader(r)

	head := make([]byte, 64)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if !bytes.Equal(head, payload[:64]) {
		t.Fatal("compressed head mismatch")
	}

	if err := cr.Cancel(); err != nil {
		t.Fatalf("Cancel compressed: %v", err)
	}

	for i, want := range [][]byte{follow1, follow2} {
		op, msg, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("follow %d: %v", i, err)
		}
		if op != TextMessage || !bytes.Equal(msg, want) {
			t.Fatalf("follow %d mismatch: len=%d want=%d", i, len(msg), len(want))
		}
	}

	if err := client.WriteMessage(TextMessage, []byte("done")); err != nil {
		t.Fatalf("done: %v", err)
	}
}

// A blocked Read must exit when Cancel interrupts it, and every waiter tied
// to the canceled message (the blocked Read goroutine, the Cancel caller and
// a concurrent next-message reader) comes back.
func TestCancelUnblocksAllWaiters(t *testing.T) {
	var (
		client     *Conn
		serverConn *Conn
		serverDone = make(chan struct{})
	)

	upgrader := Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConn = c
		writeRawFrame(t, c, false, false, BinaryMessage, make([]byte, 64))
		// Hold the FIN fragment open: the client will block mid-message.
		<-serverDone
	}))
	defer srv.Close()

	dialer := Dialer{HandshakeTimeout: 5 * time.Second}
	var err error
	client, _, err = dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	op, r, err := client.NextReaderCancellable()
	if err != nil || op != BinaryMessage {
		t.Fatalf("NextReaderCancellable: %d %v", op, err)
	}
	cr := AsCancellableReader(r)

	// Start a Read that must block: it asks for more bytes than the first
	// (already buffered) fragment contains, so it waits for the FIN fragment.
	readErr := make(chan error, 1)
	blocking := make(chan struct{})
	go func() {
		buf := make([]byte, 256)
		n1, err := r.Read(buf)
		if n1 != 64 || err != nil {
			readErr <- err
			return
		}
		// Second call blocks on the network (FIN fragment withheld).
		close(blocking)
		_, err = r.Read(buf[n1:])
		readErr <- err
	}()

	waitSignal(t, blocking, "read goroutine never reached the blocking Read")
	// The goroutine is scheduled but may not yet have entered the network
	// read; allow it to park before interrupting.
	time.Sleep(50 * time.Millisecond)

	// Concurrent next-message waiter: must return once the drain settles.
	type nextResult struct {
		op  int
		msg []byte
		err error
	}
	nextCh := make(chan nextResult, 1)
	go func() {
		op, rr, e := client.NextReaderCancellable()
		if e != nil {
			nextCh <- nextResult{err: e}
			return
		}
		data, _ := io.ReadAll(rr)
		nextCh <- nextResult{op: op, msg: data, err: e}
	}()

	// Complete the message so the drain reaches the FIN, followed by the next
	// notification message.
	go func() {
		writeRawFrame(t, serverConn, true, false, continuationFrame, []byte("rest"))
		writeRawFrame(t, serverConn, true, false, TextMessage, []byte("after"))
	}()

	if err := cr.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case err := <-readErr:
		if !errors.Is(err, ErrMessageCanceled) {
			t.Fatalf("blocked read err = %v, want ErrMessageCanceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Read did not exit after Cancel")
	}

	// The next-message waiter should now receive "after".
	select {
	case res := <-nextCh:
		if res.err != nil || res.op != TextMessage || string(res.msg) != "after" {
			t.Fatalf("concurrent next reader = (%d,%q,%v)", res.op, res.msg, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent next reader did not unblock")
	}

	close(serverDone)
}

// If the message remainder exceeds the configured byte budget, Cancel closes
// the connection with CloseMessageTooBig instead of pretending recovery is
// possible.
func TestCancelDrainLimitCloses(t *testing.T) {
	var (
		srvConn  *Conn
		gotClose = make(chan *CloseError, 1)
	)
	upgrader := Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		srvConn = c
		// One frame bigger than the client's drain budget.
		writeRawFrame(t, c, true, false, BinaryMessage, make([]byte, 1024))
		op, _, err := c.ReadMessage()
		_ = op
		var ce *CloseError
		if errors.As(err, &ce) {
			gotClose <- ce
		}
	}))
	defer srv.Close()

	d := Dialer{HandshakeTimeout: 5 * time.Second}
	cl, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	cl.SetCancelDrainLimit(128)
	_, r, err := cl.NextReaderCancellable()
	if err != nil {
		t.Fatal(err)
	}
	cr := AsCancellableReader(r)
	if _, err := r.Read(make([]byte, 16)); err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := cr.Cancel(); !errors.Is(err, ErrDrainLimit) {
		t.Fatalf("Cancel err = %v, want ErrDrainLimit", err)
	}

	// Reads are permanently failed.
	if _, _, err := cl.NextReader(); !errors.Is(err, ErrDrainLimit) {
		t.Fatalf("NextReader after drain limit = %v", err)
	}

	// Server received a close frame with CloseMessageTooBig.
	select {
	case ce := <-gotClose:
		if ce.Code != CloseMessageTooBig {
			t.Fatalf("close code = %d, want %d", ce.Code, CloseMessageTooBig)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not observe close")
	}
	_ = srvConn
}

// If the peer withholds the rest of the message beyond the drain timeout,
// Cancel gives up and closes the connection.
func TestCancelDrainTimeoutCloses(t *testing.T) {
	release := make(chan struct{})
	var (
		gotClose = make(chan *CloseError, 1)
	)
	upgrader := Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		writeRawFrame(t, c, false, false, BinaryMessage, make([]byte, 16))
		<-release // never send the FIN fragment until the test ends.
		_ = c
	}))
	defer srv.Close()

	d := Dialer{HandshakeTimeout: 5 * time.Second}
	cl, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Server-side observation of the client close: read on a second handler
	// path is not possible, so simply verify the client fails its reads.
	cl.SetCancelDrainTimeout(200 * time.Millisecond)
	_, r, err := cl.NextReaderCancellable()
	if err != nil {
		t.Fatal(err)
	}
	cr := AsCancellableReader(r)

	if _, err := r.Read(make([]byte, 256)); err != nil {
		t.Fatalf("read: %v", err)
	}

	start := time.Now()
	if err := cr.Cancel(); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("Cancel err = %v, want ErrDrainTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Cancel took too long: %v", d)
	}

	if _, _, err := cl.NextReader(); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("NextReader after drain timeout = %v", err)
	}

	close(release)
	cl.Close()
	_ = gotClose
}

// A close frame interleaved while draining must complete the close handshake:
// the client surfaces the *CloseError and does not claim it can keep reading.
func TestCancelCloseDuringDrain(t *testing.T) {
	var serverConn *Conn
	upgrader := Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConn = c
		writeRawFrame(t, c, false, false, BinaryMessage, make([]byte, 64))
		// Wait for the client to start draining, then close the connection.
		time.Sleep(150 * time.Millisecond)
		_ = c.WriteControl(CloseMessage, FormatCloseMessage(CloseGoingAway, "bye"), time.Now().Add(time.Second))
		for {
			if _, _, err := c.NextReader(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	d := Dialer{HandshakeTimeout: 5 * time.Second}
	cl, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	_, r, err := cl.NextReaderCancellable()
	if err != nil {
		t.Fatal(err)
	}
	cr := AsCancellableReader(r)
	if _, err := r.Read(make([]byte, 256)); err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := cr.Cancel(); !IsCloseError(err, CloseGoingAway) {
		t.Fatalf("Cancel err = %v, want CloseGoingAway", err)
	}

	if _, _, err := cl.NextReader(); !IsCloseError(err, CloseGoingAway) {
		t.Fatalf("NextReader = %v, want CloseGoingAway", err)
	}
	_ = serverConn
}

// In-memory check for both server and client sides: draining a masked frame
// skips to the exact boundary and the following message is intact.
func TestCancelMaskedDrain(t *testing.T) {
	for _, isServer := range []bool{true, false} {
		t.Run(map[bool]string{true: "server", false: "client"}[isServer], func(t *testing.T) {
			var wire bytes.Buffer
			wc := newTestConn(nil, &wire, isServer)
			big := make([]byte, 300)
			for i := range big {
				big[i] = byte(i)
			}
			if err := wc.WriteMessage(BinaryMessage, big); err != nil {
				t.Fatal(err)
			}
			if err := wc.WriteMessage(TextMessage, []byte("next")); err != nil {
				t.Fatal(err)
			}

			rc := newTestConn(&wire, nil, !isServer)
			rc.SetCancelDrainLimit(4096)
			_, r, err := rc.NextReaderCancellable()
			if err != nil {
				t.Fatal(err)
			}
			cr := AsCancellableReader(r)
			if _, err := r.Read(make([]byte, 10)); err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := cr.Cancel(); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			op, msg, err := rc.ReadMessage()
			if err != nil || op != TextMessage || string(msg) != "next" {
				t.Fatalf("next = (%d,%q,%v)", op, msg, err)
			}
		})
	}
}
