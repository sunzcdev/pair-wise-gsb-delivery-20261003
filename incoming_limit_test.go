package smux

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// limitTestConfig builds a configuration suitable for the in-memory pipe
// tests: keepalive is disabled so the session lifecycle is fully driven by
// the test itself.
func limitTestConfig(version int) *Config {
	cfg := DefaultConfig()
	cfg.Version = version
	cfg.KeepAliveDisabled = true
	return cfg
}

// newLimitSessionPair creates a client/server session pair over net.Pipe.
func newLimitSessionPair(t *testing.T, serverCfg, clientCfg *Config) (server, client *Session, p1, p2 net.Conn) {
	t.Helper()
	p1, p2 = net.Pipe()
	server, err := Server(p1, serverCfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	client, err = Client(p2, clientCfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return server, client, p1, p2
}

// waitFor waits until cond is true or the deadline expires.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// expectRejected verifies the peer has explicitly refused the stream with
// cmdFIN, surfaced as io.EOF on the first read.
func expectRejected(t *testing.T, s *Stream) {
	t.Helper()
	if err := s.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := s.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expected io.EOF for rejected stream, got %v", err)
	}
}

// assertNoLeftovers verifies that after every stream has been drained, the
// server keeps neither stream entries nor borrowed receive tokens.
func assertNoLeftovers(t *testing.T, s *Session, initialBucket int32) {
	t.Helper()
	waitFor(t, func() bool { return s.NumStreams() == 0 }, "all streams removed")
	if got := atomic.LoadInt32(&s.bucket); got != initialBucket {
		t.Fatalf("receive tokens leaked: bucket=%d, want %d", got, initialBucket)
	}
	s.streamLock.Lock()
	left := len(s.streams)
	s.streamLock.Unlock()
	if left != 0 {
		t.Fatalf("%d stream(s) left in the session map", left)
	}
}

// TestAcceptBacklogRejectsOverflow fills the accept backlog and verifies that
// overflow SYNs are explicitly rejected without being registered, that
// rejecting never reuses a stream ID, and that acceptance resumes once a slot
// is released.
func TestAcceptBacklogRejectsOverflow(t *testing.T) {
	serverCfg := limitTestConfig(1)
	serverCfg.MaxAcceptBacklog = 2
	clientCfg := limitTestConfig(1)
	server, client, p1, p2 := newLimitSessionPair(t, serverCfg, clientCfg)
	initialBucket := atomic.LoadInt32(&server.bucket)
	defer server.Close()
	defer client.Close()
	defer p1.Close()
	defer p2.Close()

	s1, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	// Both streams now occupy the backlog.
	waitFor(t, func() bool { return server.NumStreams() == 2 }, "two pending streams")

	// The third SYN hits the full backlog and must be refused.
	s3, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	expectRejected(t, s3)

	// The rejected stream never enters the server bookkeeping.
	waitFor(t, func() bool { return server.NumStreams() == 2 }, "rejected stream not registered")

	// Accept both queued streams.
	a1, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	a2, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}

	// Once accepted the backlog slot is released; a new stream is admitted
	// using a fresh ID rather than reusing the rejected stream's ID 5.
	s4, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	a4, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if a4.ID() != s4.ID() || s4.ID() != 9 {
		t.Fatalf("expected new stream id 9, got client=%d server=%d", s4.ID(), a4.ID())
	}

	for _, st := range []*Stream{s1, s2, s3, s4} {
		st.Close()
	}
	a1.Close()
	a2.Close()
	a4.Close()

	assertNoLeftovers(t, server, initialBucket)
}

// TestIncomingStreamLimitRelease verifies the total inbound stream limit, that
// it counts streams not yet handed to the application, and that closing a
// stream frees one slot for a later SYN.
func TestIncomingStreamLimitRelease(t *testing.T) {
	serverCfg := limitTestConfig(1)
	serverCfg.MaxIncomingStreams = 2
	serverCfg.MaxAcceptBacklog = 2
	clientCfg := limitTestConfig(1)
	server, client, p1, p2 := newLimitSessionPair(t, serverCfg, clientCfg)
	initialBucket := atomic.LoadInt32(&server.bucket)
	defer server.Close()
	defer client.Close()
	defer p1.Close()
	defer p2.Close()

	s1, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return server.NumStreams() == 2 }, "two inbound streams")

	// Total inbound limit (2) reached; next SYN is rejected even though both
	// streams are still waiting in the accept backlog.
	s3, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	expectRejected(t, s3)

	a1, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}

	// Accepting alone does not release the total-limit slot.
	s4, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	expectRejected(t, s4)

	// Closing the accepted stream releases exactly one slot.
	s1.Close()
	a1.Close()
	waitFor(t, func() bool { return server.NumStreams() == 1 }, "one slot released")

	s5, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// s2 is still queued ahead of s5, so it is accepted first.
	a2, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID() != s2.ID() {
		t.Fatalf("expected queued stream id %d, got %d", s2.ID(), a2.ID())
	}
	a2.Close()

	a5, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if a5.ID() != s5.ID() || s5.ID() != 11 {
		t.Fatalf("expected new stream id 11 after release, got %d", s5.ID())
	}

	for _, st := range []*Stream{s2, s3, s4, s5} {
		st.Close()
	}
	a5.Close()

	assertNoLeftovers(t, server, initialBucket)
}

// TestIncomingLimitKeepsStreamsFlowing verifies that while the backlog is
// full and an overflow SYN is rejected, the receive loop keeps processing
// data (PSH) and window updates (UPD) for existing streams, including one
// still queued for AcceptStream().
func TestIncomingLimitKeepsStreamsFlowing(t *testing.T) {
	serverCfg := limitTestConfig(2)
	serverCfg.MaxAcceptBacklog = 1
	clientCfg := limitTestConfig(2)
	server, client, p1, p2 := newLimitSessionPair(t, serverCfg, clientCfg)
	initialBucket := atomic.LoadInt32(&server.bucket)
	defer server.Close()
	defer client.Close()
	defer p1.Close()
	defer p2.Close()

	// s1 is queued for acceptance. Data sent before Accept must still be
	// received by recvLoop and buffered against the pending stream.
	s1, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	head := []byte("head")
	if _, err := s1.Write(head); err != nil {
		t.Fatal(err)
	}

	// The backlog is full; the next stream is rejected.
	s2, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	expectRejected(t, s2)

	a1, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := a1.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(head))
	if _, err := io.ReadFull(a1, got); err != nil {
		t.Fatalf("read buffered data: %v", err)
	}

	// A large transfer exercises both PSH and UPD (window updates) while the
	// backlog remains full, proving recvLoop is never blocked by the limits.
	const size = 512 * 1024
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := a1.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, werr := s1.Write(payload)
		done <- werr
	}()

	buf := make([]byte, 1400)
	received := 0
	for received < size {
		n, rerr := a1.Read(buf)
		received += n
		for j := 0; j < n; j++ {
			if buf[j] != payload[received-n+j] {
				t.Fatalf("data mismatch at %d", received-n+j)
			}
		}
		if rerr != nil {
			t.Fatalf("read payload: %v", rerr)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// All read tokens are returned once the consumer drained the data.
	waitFor(t, func() bool { return atomic.LoadInt32(&server.bucket) == initialBucket }, "tokens returned")

	s1.Close()
	s2.Close()
	a1.Close()

	assertNoLeftovers(t, server, initialBucket)
}

// TestIncomingLimitCloseRace opens streams up to and beyond the limits while
// the session is being torn down, ensuring the rejection path and session
// close do not race into a deadlock or panic.
func TestIncomingLimitCloseRace(t *testing.T) {
	serverCfg := limitTestConfig(2)
	serverCfg.MaxIncomingStreams = 4
	serverCfg.MaxAcceptBacklog = 2
	clientCfg := limitTestConfig(2)
	server, client, p1, p2 := newLimitSessionPair(t, serverCfg, clientCfg)
	defer client.Close()
	defer p1.Close()
	defer p2.Close()

	stop := make(chan struct{})
	var opened []*Stream
	openDone := make(chan struct{})
	go func() {
		defer close(openDone)
		for i := 0; i < 32; i++ {
			st, err := client.OpenStream()
			if err != nil {
				return
			}
			opened = append(opened, st)
			if i%3 == 0 {
				if err := st.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
					_, _ = st.Read(make([]byte, 1))
				}
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	go func() {
		for {
			a, err := server.AcceptStream()
			if err != nil {
				return
			}
			a.Close()
		}
	}()

	time.Sleep(10 * time.Millisecond)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-openDone
	for _, st := range opened {
		st.Close()
	}
}

// TestIncomingLimitConfigValidation checks the new configuration constraints.
func TestIncomingLimitConfigValidation(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(*Config)
	}{
		{"negative incoming streams", func(c *Config) { c.MaxIncomingStreams = -1 }},
		{"negative accept backlog", func(c *Config) { c.MaxAcceptBacklog = -1 }},
		{"backlog larger than incoming streams", func(c *Config) {
			c.MaxIncomingStreams = 2
			c.MaxAcceptBacklog = 3
		}},
	}
	for _, tc := range cases {
		cfg := DefaultConfig()
		tc.tweak(cfg)
		if err := VerifyConfig(cfg); err == nil {
			t.Fatalf("%s: expected VerifyConfig error", tc.name)
		}
	}

	// Zero values keep the unlimited/default behaviour and remain valid.
	cfg := DefaultConfig()
	cfg.MaxIncomingStreams = 4
	cfg.MaxAcceptBacklog = 4
	if err := VerifyConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
