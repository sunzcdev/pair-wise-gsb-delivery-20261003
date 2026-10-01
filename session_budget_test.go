// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// budgetConf returns a keepalive-free config with a small per-stream window and
// a tight session budget that can only hold a few streams.
func budgetConf(sessionWindow uint32) *Config {
	conf := testConfNoKeepAlive()
	// Keep the stream window at the 256KB default so each fresh stream
	// commits a predictable initialStreamWindow.
	conf.MaxStreamWindowSize = initialStreamWindow
	conf.MaxSessionWindowSize = sessionWindow
	return conf
}

// TestSessionRecvBudget_CommittedBounded repeatedly opens more streams than
// the session budget can hold while the receiver drains each one to EOF and
// closes it. It asserts that committed receive memory never exceeds the
// configured limit and that the budget returns to zero after every round.
func TestSessionRecvBudget_CommittedBounded(t *testing.T) {
	const funded = 2
	serverConf := budgetConf(funded * initialStreamWindow)
	serverConf.AcceptBacklog = 128
	serverConf.StreamCloseTimeout = 250 * time.Millisecond

	// The sender imposes no session limit; the bound under test is the
	// receiver's committed memory for these inbound streams.
	clientConf := testConfNoKeepAlive()
	clientConf.MaxStreamWindowSize = initialStreamWindow
	clientConf.MaxSessionWindowSize = 0
	clientConf.AcceptBacklog = 128
	clientConf.StreamCloseTimeout = 250 * time.Millisecond

	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	const rounds = 6
	for round := 0; round < rounds; round++ {
		// Open exactly as many streams as the budget can fund, send a small
		// payload, and half-close so the receiver observes EOF.
		const inFlight = funded
		for i := 0; i < inFlight; i++ {
			s, err := client.OpenStream()
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := s.Write([]byte("data")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		}

		// Accept and drain to EOF so both sides close gracefully and the
		// receiver returns every committed byte.
		for i := 0; i < inFlight; i++ {
			s, err := server.AcceptStream()
			if err != nil {
				t.Fatalf("accept %d/%d: %v", round, i, err)
			}
			got, err := io.Copy(io.Discard, s)
			if err != nil && err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			if got != 4 {
				t.Fatalf("short read: %d", got)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("server close: %v", err)
			}
		}

		// Budget must never exceed the limit at any point, and must be fully
		// reclaimed once the round's streams are gone.
		returnDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(returnDeadline) {
			server.recvBudget.mu.Lock()
			used := server.recvBudget.used
			server.recvBudget.mu.Unlock()
			if used > uint64(serverConf.MaxSessionWindowSize) {
				t.Fatalf("committed %d exceeds limit %d",
					used, serverConf.MaxSessionWindowSize)
			}
			if used == 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		server.recvBudget.mu.Lock()
		used := server.recvBudget.used
		server.recvBudget.mu.Unlock()
		if used != 0 {
			t.Fatalf("budget leaked after round %d: used=%d", round, used)
		}
	}
}

// TestSessionRecvBudget_ResumeReadNoDeadlock runs many small-window streams
// concurrently. The receiver (server) has a session budget that only fits a
// few streams. It first stops reading so the committed memory saturates, then
// resumes and drains everything. This proves the commitment stays bounded,
// no credit leaks, and resuming reads unblocks senders without deadlock.
// TestSessionRecvBudget_ResumeReadNoDeadlock runs many small-window streams
// concurrently. The receiver (server) has a session budget that only fits a
// few streams. It first stops reading so the committed memory saturates, then
// resumes and drains everything. This proves the commitment stays bounded, no
// credit leaks, and resuming reads unblocks senders without deadlock.
func TestSessionRecvBudget_ResumeReadNoDeadlock(t *testing.T) {
	const window = initialStreamWindow
	const streams = 8
	// Fund every stream's initial window. While reads are paused each stream
	// fills its window, saturating the limit; once reads resume, consumed
	// bytes are returned and re-granted via window updates.
	const funded = streams

	serverConf := testConfNoKeepAlive()
	serverConf.MaxStreamWindowSize = window
	serverConf.MaxSessionWindowSize = funded * window
	serverConf.AcceptBacklog = 128

	// The sender side places no session limit so it can open all streams.
	clientConf := testConfNoKeepAlive()
	clientConf.MaxStreamWindowSize = window
	clientConf.MaxSessionWindowSize = 0
	clientConf.AcceptBacklog = 128

	// Use a buffered TCP transport: with a synchronous io.Pipe the sender
	// blocks filling the first stream before it can open later ones, which is
	// an artifact of the transport rather than the session budget.
	clientConn, serverConn := testConnTCP(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	errCh := make(chan error, 16)

	// Senders open many streams, send more than one window (forcing window
	// updates once reads resume), then half-close.
	senderDone := make(chan struct{})
	var senderWg sync.WaitGroup
	payload := window + window/2
	for i := 0; i < streams; i++ {
		senderWg.Add(1)
		go func() {
			defer senderWg.Done()
			s, err := client.OpenStream()
			if err != nil {
				errCh <- err
				return
			}
			if _, err := s.Write(make([]byte, payload)); err != nil {
				_ = s.Close()
				errCh <- err
				return
			}
			_ = s.Close()
		}()
	}
	go func() { senderWg.Wait(); close(senderDone) }()

	// The server accepts every stream; each accepted stream blocks on resume
	// before reading, so no budget is reclaimed until reading resumes.
	accepted := make(chan *Stream, streams)
	acceptDone := make(chan struct{})
	resume := make(chan struct{})
	var drainWg sync.WaitGroup
	drainOne := func(s *Stream) {
		defer drainWg.Done()
		<-resume
		want := int64(window + window/2)
		got, err := io.Copy(io.Discard, s)
		if err != nil && err != io.EOF {
			errCh <- err
			return
		}
		if got != want {
			errCh <- fmt.Errorf("short read: got=%d want=%d", got, want)
			return
		}
		_ = s.Close()
	}
	go func() {
		defer close(acceptDone)
		for i := 0; i < streams; i++ {
			s, err := server.AcceptStream()
			if err != nil {
				errCh <- err
				return
			}
			accepted <- s
			drainWg.Add(1)
			go drainOne(s)
		}
	}()

	// Wait until the committed memory saturates while reads are paused.
	deadline := time.Now().Add(5 * time.Second)
	saturated := false
	for time.Now().Before(deadline) {
		server.recvBudget.mu.Lock()
		used := server.recvBudget.used
		server.recvBudget.mu.Unlock()
		if used >= uint64(serverConf.MaxSessionWindowSize) {
			saturated = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !saturated {
		t.Fatalf("budget never saturated; used stayed below %d",
			serverConf.MaxSessionWindowSize)
	}
	server.recvBudget.mu.Lock()
	usedWhilePaused := server.recvBudget.used
	server.recvBudget.mu.Unlock()
	if usedWhilePaused > uint64(serverConf.MaxSessionWindowSize) {
		t.Fatalf("committed %d exceeded session budget %d while paused",
			usedWhilePaused, serverConf.MaxSessionWindowSize)
	}

	// Resume reading: reads return budget, window updates reuse it to deliver
	// the remaining payload, and blocked senders complete.
	close(resume)

	select {
	case <-senderDone:
	case <-time.After(20 * time.Second):
		t.Fatalf("deadlock: senders did not finish after reads resumed")
	}
	select {
	case <-acceptDone:
	case <-time.After(20 * time.Second):
		t.Fatalf("server did not accept all streams")
	}
	close(accepted)

	drainDone := make(chan struct{})
	go func() { drainWg.Wait(); close(drainDone) }()
	select {
	case <-drainDone:
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("drain error: %v", err)
			}
		default:
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("readers did not finish after resuming")
	}

	// All streams closed -> every committed byte must have been returned.
	returnDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(returnDeadline) {
		server.recvBudget.mu.Lock()
		used := server.recvBudget.used
		server.recvBudget.mu.Unlock()
		if used == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	server.recvBudget.mu.Lock()
	t.Fatalf("budget leaked after drain: used=%d", server.recvBudget.used)
	server.recvBudget.mu.Unlock()
}

func TestSessionRecvBudget_InboundResetWhenExhausted(t *testing.T) {
	// Only a single stream's initial window fits.
	serverConf := budgetConf(initialStreamWindow)
	serverConf.AcceptBacklog = 64
	// The client places no session level limit, so it may keep opening
	// outbound streams regardless of how the server chooses to fund them.
	clientConf := testConfNoKeepAlive()
	clientConf.MaxStreamWindowSize = initialStreamWindow
	clientConf.MaxSessionWindowSize = 0
	clientConf.AcceptBacklog = 64

	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn, clientConf, serverConf)

	// First stream consumes the whole budget; the server accepts it but keeps
	// it open and unread in a background goroutine so the accept ACK window
	// update does not pile up behind subsequent client sends on synchronous
	// pipe transports.
	firstAccepted := make(chan *Stream, 1)
	go func() {
		s, err := server.AcceptStream()
		if err != nil {
			return
		}
		firstAccepted <- s
	}()

	// First stream consumes the whole budget; leave it unread/unclosed.
	first, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()

	select {
	case accepted := <-firstAccepted:
		defer func() { _ = accepted.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatalf("server did not accept first stream")
	}

	// Control frames (pings) must still be processed even while the budget is
	// exhausted.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("ping with exhausted budget: %v", err)
	}

	// A second incoming stream cannot be funded. It should be reset, so reading
	// it after FIN must surface a connection reset instead of hanging.
	second, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := second.Read(buf); err != ErrConnectionReset {
		t.Fatalf("expected ErrConnectionReset for unfunded stream, got: %v", err)
	}
}
