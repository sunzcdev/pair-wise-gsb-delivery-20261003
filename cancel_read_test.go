// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pacingReader serves data up to an allowance; after the allowance is spent
// it blocks until more is granted, letting a test pause a producing writer.
// newTCPConns returns a server/client pair of websocket Conns connected over
// a real TCP loopback, so writes are buffered by the operating system.
func newTCPConns(t *testing.T) (server, client *Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- c
	}()

	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var sc net.Conn
	select {
	case sc = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("accept timed out")
	}

	server = newConn(sc, true, 512, 512, nil, nil, nil)
	client = newConn(cc, false, 512, 512, nil, nil, nil)
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	return server, client
}

// writeFragmented writes an explicitly fragmented binary message on a server
// connection. A ping is interleaved immediately after the fragment ending at
// pingAfterOffset.
func writeFragmented(t *testing.T, c *Conn, payload []byte, frameSize, pingAfterOffset int, pingPayload string) {
	t.Helper()
	if err := writeFragmentedE(c, payload, frameSize, pingAfterOffset, pingPayload); err != nil {
		t.Fatalf("writeFragmented: %v", err)
	}
}

func writeFragmentedE(c *Conn, payload []byte, frameSize, pingAfterOffset int, pingPayload string) error {
	var mw messageWriter
	if err := c.beginMessage(&mw, BinaryMessage); err != nil {
		return err
	}
	for off := 0; off < len(payload); {
		end := off + frameSize
		if end > len(payload) {
			end = len(payload)
		}
		if err := mw.flushFrame(end == len(payload), payload[off:end]); err != nil {
			return err
		}
		off = end
		if pingAfterOffset > 0 && off == pingAfterOffset {
			if err := c.WriteControl(PingMessage, []byte(pingPayload), time.Now().Add(writeWait)); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestCancelDeliveryThenNotification is the acceptance scenario: the reader
// consumes half of a large fragmented message, cancels the delivery, and
// subsequently receives a complete notification that follows it. The ping
// interleaved between fragments must be answered with a pong while the
// library drains the rest of the big message, and every waiting goroutine
// must exit.
func TestCancelDeliveryThenNotification(t *testing.T) {
	const (
		bigSize   = 200000
		frameSize = 1000
		half      = bigSize / 2
		notify    = "notification-after-cancel"
	)

	server, client := newTCPConns(t)

	// A server read loop processes the pong frames the client emits while
	// draining the big message. The pong handler records the payload.
	pongCh := make(chan string, 8)
	server.SetPongHandler(func(appData string) error {
		select {
		case pongCh <- appData:
		default:
		}
		return nil
	})
	go func() {
		for {
			_, sr, serr := server.NextReader()
			if serr != nil {
				return
			}
			_, _ = io.Copy(io.Discard, sr)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Producer writes the first half of the fragmented big message, waits for
	// the test to release it, then writes the rest (with an interleaved ping)
	// followed by the complete notification.
	resumeProduce := make(chan struct{})
	produceDone := make(chan struct{})
	produceErr := make(chan error, 1)
	go func() {
		defer close(produceDone)
		big := make([]byte, bigSize)
		var mw messageWriter
		if err := server.beginMessage(&mw, BinaryMessage); err != nil {
			produceErr <- err
			return
		}
		writeFrame := func(off, end int, final bool) error {
			return mw.flushFrame(final, big[off:end])
		}
		for off := 0; off < half; off += frameSize {
			end := off + frameSize
			if end > half {
				end = half
			}
			if err := writeFrame(off, end, false); err != nil {
				produceErr <- err
				return
			}
		}
		<-resumeProduce
		for off := half; off < bigSize; off += frameSize {
			end := off + frameSize
			if end > bigSize {
				end = bigSize
			}
			final := end == bigSize
			if err := writeFrame(off, end, final); err != nil {
				produceErr <- err
				return
			}
			if off == frameSize*101-frameSize {
				if err := server.WriteControl(PingMessage, []byte("keepalive"), time.Now().Add(writeWait)); err != nil {
					produceErr <- err
					return
				}
			}
		}
		if err := server.WriteMessage(TextMessage, []byte(notify)); err != nil {
			produceErr <- err
		}
	}()

	// The single reader consumes half of the big message, then waits to be
	// canceled.
	type halfResult struct {
		n   int
		err error
	}
	halfCh := make(chan halfResult, 1)
	readDone := make(chan error, 1)
	go func() {
		op, r, err := client.NextReaderContext(ctx)
		if err != nil {
			readDone <- err
			return
		}
		if op != BinaryMessage {
			readDone <- errors.New("expected binary message")
			return
		}
		// Read half the message with one-byte calls so the last call is known
		// to block waiting for the unread remainder; cancellation unblocks it.
		one := make([]byte, 1)
		total := 0
		for total < half {
			n, err := r.Read(one)
			total += n
			if err != nil {
				if total == half {
					halfCh <- halfResult{n: total}
				} else {
					halfCh <- halfResult{n: total, err: err}
				}
				readDone <- err
				return
			}
		}
		halfCh <- halfResult{n: total}
		// Now block waiting for the remainder; this is the canceled read.
		_, err = r.Read(one)
		readDone <- err
	}()

	select {
	case hr := <-halfCh:
		if hr.err != nil {
			t.Fatalf("reading half: %v", hr.err)
		}
		if hr.n != half {
			t.Fatalf("read %d bytes before cancel, want %d", hr.n, half)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive half of the big message")
	}

	// The reader is now blocked with no more data on the wire (the producer
	// is paused). Cancel, let the interrupt land, then release the remainder.
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(resumeProduce)

	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader goroutine did not exit after cancel")
	}

	// The server must observe the pong produced while the library drains the
	// rest of the big message in the background.
	select {
	case payload := <-pongCh:
		if payload != "keepalive" {
			t.Fatalf("pong payload = %q, want keepalive", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not observe pong while draining")
	}

	// Next delivery must be the complete notification.
	nop, nr, nerr := client.NextReaderContext(context.Background())
	if nerr != nil {
		t.Fatalf("NextReaderContext after cancel = %v", nerr)
	}
	if nop != TextMessage {
		t.Fatalf("message type = %d, want TextMessage", nop)
	}
	body, err := io.ReadAll(nr)
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if string(body) != notify {
		t.Fatalf("notification = %q, want %q", body, notify)
	}

	select {
	case err := <-produceErr:
		t.Fatalf("producer error: %v", err)
	default:
	}
	select {
	case <-produceDone:
	case <-time.After(3 * time.Second):
		t.Fatal("producer did not finish")
	}

	// The read deadline used to interrupt the delivery must have been
	// restored to its original (zero) value.
	if !client.readDeadline.IsZero() {
		t.Fatalf("readDeadline = %v, want zero", client.readDeadline)
	}
}

// TestCancelWhileWaiting resumes exactly at the frame that was arriving when
// the context was canceled. No delivery had started, so the connection must
// remain usable and the next read returns the full message.
func TestCancelWhileWaiting(t *testing.T) {
	server, client := newTCPConns(t)

	ctx, cancel := context.WithCancel(context.Background())

	type waitResult struct {
		op  int
		r   io.Reader
		err error
	}
	resCh := make(chan waitResult, 1)
	go func() {
		op, r, err := client.NextReaderContext(ctx)
		resCh <- waitResult{op: op, r: r, err: err}
	}()

	// Cancel with nothing on the wire.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case res := <-resCh:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("canceled wait = %v, want context.Canceled", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked NextReaderContext did not return after cancel")
	}

	// The connection must still work at the exact next frame.
	if err := server.WriteMessage(TextMessage, []byte("after-wait-cancel")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	op, r, err := client.NextReaderContext(context.Background())
	if err != nil {
		t.Fatalf("NextReaderContext after wait cancel = %v", err)
	}
	if op != TextMessage {
		t.Fatalf("type = %d, want TextMessage", op)
	}
	body, err := io.ReadAll(r)
	if err != nil || string(body) != "after-wait-cancel" {
		t.Fatalf("body = %q, %v", body, err)
	}
}

// TestCancelDrainByteBudget verifies that a message too large to drain within
// the configured byte budget closes the connection and reports a *DrainError
// wrapping ErrDrainLimit, and sends a close frame to the peer.
func TestCancelDrainByteBudget(t *testing.T) {
	server, client := newTCPConns(t)
	client.SetReadCancelBudget(5*time.Second, 1024)

	closeCode := make(chan int, 1)
	server.SetCloseHandler(func(code int, text string) error {
		select {
		case closeCode <- code:
		default:
		}
		message := FormatCloseMessage(code, "")
		return server.WriteControl(CloseMessage, message, time.Now().Add(writeWait))
	})
	go func() {
		for {
			if _, _, err := server.NextReader(); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resumeProduce := make(chan struct{})
	started := make(chan struct{})
	readerReturned := make(chan error, 1)
	go func() {
		op, r, err := client.NextReaderContext(ctx)
		if err != nil {
			readerReturned <- err
			return
		}
		if op != BinaryMessage {
			readerReturned <- errors.New("unexpected message type")
			return
		}
		close(started)
		buf := make([]byte, 1)
		for {
			if _, err := r.Read(buf); err != nil {
				readerReturned <- err
				return
			}
		}
	}()

	// First fragment: 16 bytes only. The reader reads one byte and blocks
	// waiting for more while the producer is paused.
	var mw messageWriter
	if err := server.beginMessage(&mw, BinaryMessage); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := mw.flushFrame(false, make([]byte, 16)); err != nil {
		t.Fatalf("flush fragment: %v", err)
	}

	<-started
	time.Sleep(50 * time.Millisecond)
	cancel()
	// Only after cancellation does the remainder (larger than the byte
	// budget) arrive; the drain must reject it on the byte budget.
	go func() {
		<-resumeProduce
		_ = mw.flushFrame(false, make([]byte, 4096))
		_ = mw.flushFrame(true, make([]byte, 16))
	}()
	close(resumeProduce)

	// The reader must observe the cancellation promptly even though more data
	// than the byte budget remains.
	select {
	case err := <-readerReturned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reader after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not return after cancel")
	}

	_, _, err := client.NextReaderContext(context.Background())
	var de *DrainError
	if !errors.As(err, &de) {
		t.Fatalf("next read = %v, want *DrainError", err)
	}
	if !errors.Is(err, ErrDrainLimit) {
		t.Fatalf("drain cause = %v, want ErrDrainLimit", err)
	}

	select {
	case code := <-closeCode:
		if code != ClosePolicyViolation {
			t.Fatalf("close code = %d, want %d", code, ClosePolicyViolation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer did not observe close frame after budget failure")
	}

	// The connection is permanently dead.
	if _, _, err := client.NextReaderContext(context.Background()); !errors.As(err, &de) {
		t.Fatalf("read after fatal drain = %v, want persistent *DrainError", err)
	}
}

// TestCancelDrainTimeout verifies that a drain that cannot finish within the
// configured time budget closes the connection with ErrDrainTimeout.
func TestCancelDrainTimeout(t *testing.T) {
	server, client := newTCPConns(t)
	client.SetReadCancelBudget(100*time.Millisecond, 1<<30)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	readerReturned := make(chan error, 1)
	go func() {
		op, r, err := client.NextReaderContext(ctx)
		if err != nil {
			readerReturned <- err
			return
		}
		if op != BinaryMessage {
			readerReturned <- errors.New("unexpected message type")
			return
		}
		close(started)
		buf := make([]byte, 1)
		for {
			if _, err := r.Read(buf); err != nil {
				readerReturned <- err
				return
			}
		}
	}()

	// Send one small frame and never the rest, so the drain waits for the
	// remainder until the time budget expires.
	var mw messageWriter
	if err := server.beginMessage(&mw, BinaryMessage); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := mw.flushFrame(false, []byte("x")); err != nil {
		t.Fatalf("flush: %v", err)
	}

	<-started
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-readerReturned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reader after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not return after cancel")
	}

	_, _, err := client.NextReaderContext(context.Background())
	var de *DrainError
	if !errors.As(err, &de) || !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("next read = %v, want *DrainError wrapping ErrDrainTimeout", err)
	}
}

// TestCancelCompressedMessage verifies that canceling an in-flight
// compressed delivery closes and returns the pooled decompressor, so no
// residual decompression state corrupts the next independently compressed
// message.
func TestCancelCompressedMessage(t *testing.T) {
	server, client := newTCPConns(t)
	server.newCompressionWriter = compressNoContextTakeover
	client.SetReadCancelBudget(10*time.Second, 1<<30)

	var mu sync.Mutex
	created, closed := 0, 0
	client.newDecompressionReader = func(r io.Reader) io.ReadCloser {
		mu.Lock()
		created++
		mu.Unlock()
		inner := decompressNoContextTakeover(r)
		return &trackingReadCloser{
			ReadCloser: inner,
			onClose: func() {
				mu.Lock()
				closed++
				mu.Unlock()
			},
		}
	}

	go func() {
		for {
			if _, _, err := server.NextReader(); err != nil {
				return
			}
		}
	}()

	// Serialize every server write so the default control handlers can never
	// race with this test's WriteMessage calls.
	writeMu := &sync.Mutex{}
	serverWrite := func(msgType int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return server.WriteMessage(msgType, data)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	readerReturned := make(chan error, 1)
	go func() {
		op, r, err := client.NextReaderContext(ctx)
		if err != nil {
			readerReturned <- err
			return
		}
		if op != BinaryMessage {
			readerReturned <- errors.New("unexpected message type")
			return
		}
		close(started)
		buf := make([]byte, 16)
		for {
			if _, err := r.Read(buf); err != nil {
				readerReturned <- err
				return
			}
		}
	}()

	// One large, weakly compressible message. The server-side connection is
	// gated so only the head of the frame reaches the client before cancel,
	// guaranteeing the delivery is open when canceled.
	payload := make([]byte, 2<<20)
	var x uint32 = 2463534242
	for i := range payload {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		payload[i] = byte(x) ^ byte(i>>11)
	}
	resumeGate := make(chan struct{})
	server.conn = &gatedConn{Conn: server.conn, allow: 4096, resume: resumeGate}
	go func() {
		if err := serverWrite(BinaryMessage, payload); err != nil {
			t.Errorf("write big: %v", err)
		}
	}()

	<-started
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-readerReturned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reader after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not return after cancel")
	}
	close(resumeGate)

	select {
	case <-client.drainDone:
		if client.drainErr != nil {
			t.Fatalf("drain error: %v", client.drainErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("drain did not finish")
	}

	mu.Lock()
	gotCreated, gotClosed := created, closed
	mu.Unlock()
	if gotClosed < gotCreated {
		t.Fatalf("decompressor not closed after cancel: created=%d closed=%d", gotCreated, gotClosed)
	}
	if client.reader != nil || client.messageReader != nil {
		t.Fatal("compressed reader was not released after cancel")
	}

	for _, msg := range []string{"independent-next-message", "second-independent-message"} {
		if err := serverWrite(TextMessage, []byte(msg)); err != nil {
			t.Fatalf("write message: %v", err)
		}
		op, r, err := client.NextReaderContext(context.Background())
		if err != nil {
			t.Fatalf("next after compressed cancel = %v", err)
		}
		if op != TextMessage {
			t.Fatalf("type = %d, want TextMessage", op)
		}
		body, err := io.ReadAll(r)
		if err != nil || string(body) != msg {
			t.Fatalf("message = %q, %v, want %q", body, err, msg)
		}
	}
}

type trackingReadCloser struct {
	io.ReadCloser
	onClose func()
}

func (t *trackingReadCloser) Close() error {
	t.onClose()
	return t.ReadCloser.Close()
}

// gatedConn wraps a net.Conn and lets at most allow bytes of writes through
// before blocking until resume is closed. It gates the server side so a test
// can hold back the tail of a large frame.
type gatedConn struct {
	net.Conn
	allowed int
	allow   int
	resume  chan struct{}
}

func (g *gatedConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		if g.allowed < g.allow {
			n := len(p)
			if room := g.allow - g.allowed; n > room {
				n = room
			}
			m, err := g.Conn.Write(p[:n])
			total += m
			g.allowed += m
			if err != nil {
				return total, err
			}
			p = p[m:]
			if m == 0 {
				return total, io.ErrShortWrite
			}
			continue
		}
		<-g.resume
		m, err := g.Conn.Write(p)
		total += m
		if err != nil {
			return total, err
		}
		p = p[m:]
	}
	return total, nil
}

// TestCancelDrainCloseHandshake verifies that a close frame arriving while a
// canceled message is draining is honored: the drain stops and the close is
// surfaced instead of pretending the connection is still readable.
func TestCancelDrainCloseHandshake(t *testing.T) {
	server, client := newTCPConns(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	go func() {
		op, r, err := client.NextReaderContext(ctx)
		if err != nil {
			t.Errorf("NextReaderContext: %v", err)
			return
		}
		if op != BinaryMessage {
			t.Errorf("type = %d", op)
			return
		}
		close(started)
		buf := make([]byte, 1)
		_, _ = r.Read(buf)
		_, _ = r.Read(buf)
	}()

	var mw messageWriter
	if err := server.beginMessage(&mw, BinaryMessage); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := mw.flushFrame(false, []byte("partial")); err != nil {
		t.Fatalf("flush: %v", err)
	}

	<-started
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Peer abandons the message and closes while the client is draining.
	if err := server.WriteControl(CloseMessage,
		FormatCloseMessage(CloseGoingAway, "leaving"),
		time.Now().Add(writeWait)); err != nil {
		t.Fatalf("write close: %v", err)
	}

	_, _, err := client.NextReaderContext(context.Background())
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseGoingAway {
		t.Fatalf("next read = %v, want CloseError code %d", err, CloseGoingAway)
	}
}
