package smux

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// limitTestConfig returns a v2 config with tight incoming stream limits.
func limitTestConfig() *Config {
	config := DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	config.MaxIncomingStreams = 2
	config.MaxAcceptBacklog = 2
	return config
}

// clientLimitTestConfig returns a matching v2 config without incoming limits.
func clientLimitTestConfig() *Config {
	config := DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	return config
}

func pipeSessions(t *testing.T, clientConfig, serverConfig *Config) (client, server *Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	client, err := Client(c1, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	server, err = Server(c2, serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func waitForCondition(t *testing.T, msg string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

func numIncomingStreams(s *Session) int {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return s.numIncoming
}

func numRejectedStreams(s *Session) int {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.rejected)
}

func numSessionStreams(s *Session) int {
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// TestIncomingStreamLimit verifies that once the incoming stream limit is
// reached, further streams are explicitly rejected, existing streams still
// make progress, and accepting/closing a stream frees capacity again.
func TestIncomingStreamLimit(t *testing.T) {
	config := limitTestConfig()
	client, server := pipeSessions(t, clientLimitTestConfig(), config)
	defer client.Close()
	defer server.Close()

	// fill the limit, the server application does not accept yet
	s1, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "2 incoming streams", func() bool {
		return server.NumStreams() == 2 && numIncomingStreams(server) == 2
	})

	// the next stream exceeds the limit and must be rejected explicitly:
	// the peer observes EOF and the server never queues the stream.
	s3, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "rejected stream EOF", func() bool {
		s3.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, err := s3.Read(make([]byte, 1))
		return err == io.EOF
	})
	if n := server.NumStreams(); n != 2 {
		t.Fatalf("rejected stream was queued, streams: %d", n)
	}
	s3.Close()

	// existing streams still make progress while at the limit
	if _, err := s1.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	as1, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	as1.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(as1, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("unexpected data: %q", buf)
	}

	// the limit still holds with one stream pending accept
	s4, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "rejected stream EOF", func() bool {
		s4.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, err := s4.Read(make([]byte, 1))
		return err == io.EOF
	})
	s4.Close()

	// release one stream, a fresh stream can be accepted again
	as2, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := as1.Close(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "incoming stream released", func() bool {
		return numIncomingStreams(server) == 1
	})

	s5, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	as5, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}

	// the resumed stream must not reuse any previously allocated id
	seen := map[uint32]bool{s1.ID(): true, s2.ID(): true, s3.ID(): true, s4.ID(): true}
	if seen[as5.ID()] {
		t.Fatalf("stream id %d reused after resuming accept", as5.ID())
	}

	// close everything, no stream or receive token may be left over
	as2.Close() // recycles the unread 4096 bytes
	as5.Close()
	s1.Close()
	s2.Close()
	s5.Close()
	waitForCondition(t, "no leftover streams", func() bool {
		return numSessionStreams(server) == 0 && numIncomingStreams(server) == 0
	})
	waitForCondition(t, "no leftover tokens", func() bool {
		return atomic.LoadInt32(&server.bucket) == int32(config.MaxReceiveBuffer)
	})
}

// TestIncomingStreamRejectedIDNotReused verifies at the wire level that a
// rejected stream id is never reused, even after capacity frees up, and that
// the rejected entry is garbage collected once the peer closes the stream.
func TestIncomingStreamRejectedIDNotReused(t *testing.T) {
	config := limitTestConfig()
	c1, c2 := net.Pipe()
	server, err := Server(c2, config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	raw := c1
	defer raw.Close()

	// continuously drain frames from the server; the pipe is synchronous,
	// so server-side writes (e.g. FINs from stream closes) would otherwise
	// block its sendLoop.
	frames := make(chan rawHeader, 16)
	go func() {
		defer close(frames)
		for {
			var hdr rawHeader
			if _, err := io.ReadFull(raw, hdr[:]); err != nil {
				return
			}
			frames <- hdr
		}
	}()

	writeFrame := func(cmd byte, sid uint32) {
		t.Helper()
		var hdr rawHeader
		hdr[0] = byte(config.Version)
		hdr[1] = cmd
		binary.LittleEndian.PutUint32(hdr[4:], sid)
		raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := raw.Write(hdr[:]); err != nil {
			t.Fatal(err)
		}
	}
	// expectFIN reads frames until the FIN for sid arrives, skipping any
	// other frames (e.g. FINs from server-side stream closes).
	expectFIN := func(sid uint32) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case hdr, ok := <-frames:
				if !ok {
					t.Fatal("connection closed while waiting for FIN")
				}
				if hdr.Cmd() == cmdFIN && hdr.StreamID() == sid {
					return
				}
			case <-deadline:
				t.Fatalf("timeout waiting for FIN of stream %d", sid)
				return
			}
		}
	}

	// fill the limit with two pending streams
	writeFrame(cmdSYN, 3)
	writeFrame(cmdSYN, 5)
	waitForCondition(t, "2 incoming streams", func() bool {
		return server.NumStreams() == 2
	})

	// an over-limit SYN is explicitly rejected with a FIN
	writeFrame(cmdSYN, 7)
	expectFIN(7)

	// release one slot
	st, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "incoming stream released", func() bool {
		return numIncomingStreams(server) == 1
	})

	// the rejected id must not be recycled even though a slot is free
	writeFrame(cmdSYN, 7)
	expectFIN(7)

	// a fresh id is accepted again
	writeFrame(cmdSYN, 9)
	waitForCondition(t, "fresh stream accepted", func() bool {
		return server.NumStreams() == 2
	})

	// once the peer closes the rejected stream, its entry is collected
	writeFrame(cmdFIN, 7)
	waitForCondition(t, "rejected entry collected", func() bool {
		return numRejectedStreams(server) == 0
	})

	// drain and close everything, nothing may be left over
	for _, sid := range []uint32{5, 9} {
		st, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		if st.ID() != sid {
			t.Fatalf("expected pending stream %d, got %d", sid, st.ID())
		}
		st.Close()
		expectFIN(sid)
	}
	waitForCondition(t, "no leftover streams", func() bool {
		return numSessionStreams(server) == 0 && numIncomingStreams(server) == 0
	})
	if got := atomic.LoadInt32(&server.bucket); got != int32(config.MaxReceiveBuffer) {
		t.Fatalf("receive tokens leaked: %d of %d left", got, config.MaxReceiveBuffer)
	}
}

// TestIncomingStreamLimitCloseRace races stream opening, accepting and
// closing against the incoming limits, then verifies that no stream or
// receive token is left over.
func TestIncomingStreamLimitCloseRace(t *testing.T) {
	config := limitTestConfig()
	client, server := pipeSessions(t, clientLimitTestConfig(), config)

	stop := make(chan struct{})
	var openerWg, acceptorWg sync.WaitGroup

	// opener: keep opening streams, push some data and close them
	openerWg.Add(1)
	go func() {
		defer openerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			st, err := client.OpenStream()
			if err != nil {
				return
			}
			st.Write([]byte("x"))
			st.Close()
		}
	}()

	// acceptor: accept streams, read until EOF and close them
	acceptorWg.Add(1)
	go func() {
		defer acceptorWg.Done()
		for {
			st, err := server.AcceptStream()
			if err != nil {
				return
			}
			st.SetReadDeadline(time.Now().Add(time.Second))
			io.Copy(io.Discard, st)
			st.Close()
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)

	// wait for the opener to finish, so no new SYN is in flight anymore
	openerWg.Wait()
	time.Sleep(50 * time.Millisecond) // let recvLoop process in-flight frames

	// no new streams from now on, let the acceptor drain the backlog
	waitForCondition(t, "all streams drained", func() bool {
		return server.NumStreams() == 0
	})

	client.Close()
	server.Close()
	acceptorWg.Wait()

	// no leftover stream or receive token
	if n := numSessionStreams(server); n != 0 {
		t.Fatalf("%d leftover streams after close", n)
	}
	if n := numIncomingStreams(server); n != 0 {
		t.Fatalf("%d leftover incoming streams after close", n)
	}
	if n := numRejectedStreams(server); n != 0 {
		t.Fatalf("%d leftover rejected ids after close", n)
	}
	if got := atomic.LoadInt32(&server.bucket); got != int32(config.MaxReceiveBuffer) {
		t.Fatalf("receive tokens leaked: %d of %d left", got, config.MaxReceiveBuffer)
	}
}
