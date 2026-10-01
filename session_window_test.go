// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// windowTestConf returns a keep-alive-free config with a session receive
// window of sessionWindow bytes.
func windowTestConf(streamWindow, sessionWindow uint32) *Config {
	conf := testConfNoKeepAlive()
	conf.ConnectionWriteTimeout = 2 * time.Second
	if streamWindow > 0 {
		conf.MaxStreamWindowSize = streamWindow
	}
	conf.MaxSessionWindowSize = sessionWindow
	conf.StreamCloseTimeout = 2 * time.Second
	return conf
}

// assertCommittedBounded ensures buffered receive data plus advertised
// receive credit never exceeds the configured session receive window.
func assertCommittedBounded(t testing.TB, s *Session) {
	t.Helper()
	if got, max := s.committedRecvTotal(), int64(s.config.MaxSessionWindowSize); got > max {
		t.Fatalf("session committed receive memory %d exceeds window %d", got, max)
	}
}

// TestSessionWindow_BoundedCommit proves that with many streams advertising
// small per-stream windows, the total memory the server commits stays within
// the session window while the client keeps streams open and data in flight.
func TestSessionWindow_BoundedCommit(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	clientConf := windowTestConf(initialStreamWindow, 0) // client is unlimited
	const accepted = 12
	serverConf := windowTestConf(initialStreamWindow, accepted*initialStreamWindow)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	const streams = accepted + 8

	var stopCheck atomic.Bool
	var wg sync.WaitGroup

	// Continuously assert the bound while data is flowing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stopCheck.Load() {
			assertCommittedBounded(t, server)
			time.Sleep(time.Millisecond)
		}
	}()

	// Server accepts everything but never reads, so each stream buffers up
	// to its advertised window and then stalls.
	acceptedCh := make(chan struct{}, streams)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < accepted; i++ {
			if _, err := server.AcceptStream(); err != nil {
				return
			}
			acceptedCh <- struct{}{}
		}
	}()

	// Clients open streams and write one byte. The first `accepted` block
	// forever (server never reads), the rest are rejected at open time. Each
	// goroutine reports whether its stream was reset.
	resultCh := make(chan bool, streams) // true == connection reset
	for i := 0; i < streams; i++ {
		go func() {
			stream, err := client.OpenStream()
			if err != nil {
				return
			}
			reset := false
			if _, err := stream.Write([]byte("x")); err != nil {
				reset = errors.Is(err, ErrConnectionReset)
				_ = stream.Close()
				if reset {
					resultCh <- true
				}
				return
			}
			buf := make([]byte, 1)
			if _, err := stream.Read(buf); err == nil || !errors.Is(err, ErrConnectionReset) {
				return // accepted stream: server never reads, this blocks
			}
			_ = stream.Close()
			resultCh <- true
		}()
	}

	// Wait until all streams were accepted and give data time to pile up.
	for i := 0; i < accepted; i++ {
		<-acceptedCh
	}

	// Exactly the streams beyond the budget report a reset at open time,
	// rather than having legal data dropped later.
	var resets int
	for i := 0; i < streams-accepted; i++ {
		select {
		case <-resultCh:
			resets++
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for over-budget stream resets (got %d)", resets)
		}
	}
	if resets != streams-accepted {
		t.Fatalf("expected %d RSTs for streams over budget, got %d", streams-accepted, resets)
	}

	assertCommittedBounded(t, server)

	// All credit must still be accounted for (avail + committed == limit).
	server.recvWindowLock.Lock()
	avail := server.recvWindowAvail
	server.recvWindowLock.Unlock()
	if got := avail + server.committedRecvTotal(); got != int64(server.config.MaxSessionWindowSize) {
		t.Fatalf("session receive budget leaked: avail+committed=%d, want=%d",
			got, server.config.MaxSessionWindowSize)
	}

	stopCheck.Store(true)
	wg.Wait()
}

// TestSessionWindow_InboundRST proves that when an inbound SYN cannot be
// backed by the session receive window, the stream is rejected at open time
// with a RST rather than accepting the stream and dropping legal data later.
func TestSessionWindow_InboundRST(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	clientConf := windowTestConf(initialStreamWindow, 0)
	serverConf := windowTestConf(initialStreamWindow, initialStreamWindow)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	// Open and accept one stream without reading: this consumes the whole
	// session window on the server.
	prime, err := client.OpenStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	held, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("accept prime: %v", err)
	}

	// Every further inbound stream must be RST during the SYN, and the data
	// riding along with the SYN must not inflate the server's commitment.
	for i := 0; i < 8; i++ {
		stream, err := client.OpenStream()
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		// The RST may arrive before or after the first write completes;
		// either way the stream must surface connection reset.
		_, werr := stream.Write([]byte("flood"))
		if werr != nil && !errors.Is(werr, ErrConnectionReset) {
			t.Fatalf("write %d: %v", i, werr)
		}
		if !errors.Is(werr, ErrConnectionReset) {
			buf := make([]byte, 4)
			_, rerr := stream.Read(buf)
			if !errors.Is(rerr, ErrConnectionReset) {
				_ = stream.Close()
				t.Fatalf("expected connection reset for stream %d, got: %v", i, rerr)
			}
		}
		assertCommittedBounded(t, server)
	}

	server.recvWindowLock.Lock()
	avail := server.recvWindowAvail
	server.recvWindowLock.Unlock()
	if avail+server.committedRecvTotal() != int64(server.config.MaxSessionWindowSize) {
		t.Fatalf("session receive budget leaked after RSTs")
	}

	// Both sides closing refunds the prime stream's full commitment, so a
	// fresh stream is accepted normally.
	if err := held.Close(); err != nil {
		t.Fatalf("close held: %v", err)
	}
	if err := prime.Close(); err != nil {
		t.Fatalf("close prime: %v", err)
	}
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open after refund: %v", err)
	}
	defer func() { _ = stream.Close() }()
	deadline := time.After(2 * time.Second)
	for {
		if server.NumStreams() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("fresh stream was not accepted after budget refund")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// TestSessionWindow_OpenBlocksControlProceeds proves an outbound OpenStream
// blocks while the session cannot afford another initial window, but control
// frames (Ping) keep being processed, and freeing credit unblocks the open
// instead of deadlocking.
func TestSessionWindow_OpenBlocksControlProceeds(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	clientConf := windowTestConf(initialStreamWindow, initialStreamWindow)
	serverConf := windowTestConf(initialStreamWindow, 0)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	// Open and accept one stream: it consumes the client's single initial
	// window of outbound reservation.
	heldClient, err := client.OpenStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	heldServer, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("accept held: %v", err)
	}

	type openResult struct {
		stream *Stream
		err    error
	}
	openCh := make(chan openResult, 1)
	go func() {
		st, err := client.OpenStream()
		openCh <- openResult{st, err}
	}()

	// With only one initial window of client budget, the second open waits.
	select {
	case res := <-openCh:
		t.Fatalf("OpenStream should block for credit, got: %v", res.err)
	case <-time.After(150 * time.Millisecond):
	}

	// Control frames must still be serviced while budget is exhausted.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("ping while open blocked: %v", err)
	}
	if _, err := server.Ping(); err != nil {
		t.Fatalf("ping while open blocked: %v", err)
	}

	// Both sides closing releases the client's full reservation.
	if err := heldClient.Close(); err != nil {
		t.Fatalf("close held client: %v", err)
	}
	if err := heldServer.Close(); err != nil {
		t.Fatalf("close held server: %v", err)
	}

	select {
	case res := <-openCh:
		if res.err != nil {
			t.Fatalf("open after credit release: %v", res.err)
		}
		defer func() { _ = res.stream.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatal("OpenStream deadlocked after credit was released")
	}
}

// TestSessionWindow_ManySmallStreamsConcurrently is the end-to-end proof:
// many streams with windows far smaller than the session cap exchange data in
// both directions concurrently; total commitment stays bounded, no credit
// leaks, and resuming reads drains everything without deadlock.
func TestSessionWindow_ManySmallStreamsConcurrently(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	const (
		streamWindow  = uint32(initialStreamWindow)
		sessionWindow = uint32(8 * initialStreamWindow)
		streams       = 40
		totalPerSide  = int(initialStreamWindow)
	)
	clientConf := windowTestConf(streamWindow, sessionWindow)
	serverConf := windowTestConf(streamWindow, sessionWindow)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	var bounded atomic.Bool
	bounded.Store(true)
	var stopCheck atomic.Bool
	var checker sync.WaitGroup
	checker.Add(2)
	check := func(s *Session) {
		defer checker.Done()
		for !stopCheck.Load() {
			if s.committedRecvTotal() > int64(sessionWindow) {
				bounded.Store(false)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	go check(client)
	go check(server)

	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	errCh := make(chan error, streams)
	var clients sync.WaitGroup
	for i := 0; i < streams; i++ {
		go func() {
			// Each worker handles exactly one stream. The steady read flow
			// constantly releases session credit and drives expansion.
			stream, err := server.AcceptStream()
			if err != nil {
				return
			}
			go io.CopyN(io.Discard, stream, int64(totalPerSide))
			_, _ = io.Copy(stream, io.LimitReader(&constReader{data: payload}, int64(totalPerSide)))
			_ = stream.Close()
		}()
	}
	for i := 0; i < streams; i++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			deadline := time.Now().Add(60 * time.Second)
			for {
				stream, err := client.OpenStream()
				if err != nil {
					errCh <- err
					return
				}

				reset := exchangeFullDuplex(t, stream, payload, totalPerSide)
				_ = stream.Close()
				if reset {
					if time.Now().After(deadline) {
						errCh <- errors.New("stream repeatedly reset, giving up")
						return
					}
					// SYN beyond the session window is RST at open time, or
					// the stream was reset before it could drain. Back off
					// and retry: this is "block at open, not at data".
					time.Sleep(2 * time.Millisecond)
					continue
				}
				break
			}
			errCh <- nil
		}()
	}

	drainErrorsUntil(t, errCh, streams, 60*time.Second, "concurrent stream exchange stalled")
	clients.Wait()

	// Wait for FIN/RST teardown to fully return every stream's credit on
	// both sides instead of relying on a fixed sleep.
	deadline := time.Now().Add(6 * time.Second)
	for _, side := range []struct {
		name string
		sess *Session
	}{{"client", client}, {"server", server}} {
		sess := side.sess
		for {
			sess.recvWindowLock.Lock()
			avail := sess.recvWindowAvail
			sess.recvWindowLock.Unlock()
			if avail == int64(sessionWindow) && sess.committedRecvTotal() == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s credit not refunded: avail=%d committed=%d n=%d",
					side.name, avail, sess.committedRecvTotal(), sess.NumStreams())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	stopCheck.Store(true)
	checker.Wait()

	if !bounded.Load() {
		t.Fatal("committed receive memory exceeded the session window during the run")
	}

	for _, sess := range []*Session{client, server} {
		sess.recvWindowLock.Lock()
		avail := sess.recvWindowAvail
		sess.recvWindowLock.Unlock()
		if avail != int64(sessionWindow) {
			t.Fatalf("credit leaked on session: avail=%d, want=%d", avail, sessionWindow)
		}
		if got := sess.committedRecvTotal(); got != 0 {
			t.Fatalf("residual committed receive memory: %d", got)
		}
	}

	// Fresh streams still flow after the burst: reads resumed and no
	// deadlock remains.
	roundTrip(t, client, server, payload)
}

// constReader returns the same payload forever.
type constReader struct{ data []byte }

func (r *constReader) Read(p []byte) (int, error) { return copy(p, r.data), nil }

// roundTrip opens one stream, sends payload client->server, echoes it back,
// and verifies it arrives.
func roundTrip(t *testing.T, client, server *Session, payload []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		stream, err := server.AcceptStream()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = stream.Close() }()
		buf := make([]byte, len(payload))
		n, err := io.ReadFull(stream, buf)
		if err != nil {
			done <- err
			return
		}
		_, err = stream.Write(buf[:n])
		done <- err
	}()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("post-burst open: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("post-burst write: %v", err)
	}
	// Half-close our send side so the server's read reaches EOF.
	if err := stream.Close(); err != nil {
		t.Fatalf("post-burst half-close: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("post-burst read: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("post-burst server: %v", err)
	}
}

// exchangeFullDuplex sends total bytes and reads total bytes on stream
// concurrently. It returns true (without failing) when the stream is reset
// mid-exchange, allowing callers to retry; other errors fail the test.
func exchangeFullDuplex(t testing.TB, stream *Stream, payload []byte, total int) (reset bool) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	var resetRead, resetWrite atomic.Bool
	var otherErr atomic.Value

	go func() {
		defer wg.Done()
		buf := make([]byte, len(payload))
		got := 0
		for got < total {
			n, err := io.ReadFull(stream, buf)
			got += n
			if err != nil {
				if errors.Is(err, ErrConnectionReset) || errors.Is(err, io.EOF) {
					resetRead.Store(true)
				} else {
					otherErr.Store(err)
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		sent := 0
		for sent < total {
			n, err := stream.Write(payload)
			if err != nil {
				if errors.Is(err, ErrConnectionReset) {
					resetWrite.Store(true)
				} else {
					otherErr.Store(err)
				}
				return
			}
			sent += n
		}
	}()

	wg.Wait()
	if v := otherErr.Load(); v != nil {
		t.Fatalf("unexpected stream error: %v", v)
	}
	return resetRead.Load() || resetWrite.Load()
}
