package smux

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

const allPhases = PhaseLocalWriteDone | PhasePeerWriteDone | PhaseClosed

// waitPhaseWithTimeout fails the test if WaitPhase does not return in time.
func waitPhaseWithTimeout(t *testing.T, s *Stream, p Phase) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.WaitPhase(p) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("WaitPhase(%v) hung", p)
		return nil
	}
}

// TestPhaseInterleavedHalfClose verifies phase notifications on both ends
// while the two sides half-close in interleaved order.
func TestPhaseInterleavedHalfClose(t *testing.T) {
	_, stop, cli, serverSessionCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clientSession, _ := Client(cli, nil)
	defer clientSession.Close()
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverStream, _ := serverSession.AcceptStream()

	// no phase reached initially
	if p := clientStream.Phase(); p != 0 {
		t.Fatalf("unexpected initial phase: %v", p)
	}

	// client sends data and half-closes first
	if _, err := clientStream.Write([]byte("c2s")); err != nil {
		t.Fatal(err)
	}
	if err := clientStream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := waitPhaseWithTimeout(t, clientStream, PhaseLocalWriteDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientStream.PhaseChan(PhasePeerWriteDone):
		t.Fatal("PhasePeerWriteDone fired before peer finished writing")
	default:
	}

	// server reads the data, then observes EOF and the peer-write-done phase
	buf := make([]byte, 3)
	if _, err := io.ReadFull(serverStream, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "c2s" {
		t.Fatalf("unexpected data: %q", buf)
	}
	if _, err := serverStream.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if err := waitPhaseWithTimeout(t, serverStream, PhasePeerWriteDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverStream.PhaseChan(PhaseLocalWriteDone):
		t.Fatal("PhaseLocalWriteDone fired before local CloseWrite")
	default:
	}

	// server writes back; the client must read the response before the
	// server half-closes, because once both sides have sent FIN the
	// stream is reclaimed and unread buffered data is recycled.
	if _, err := serverStream.Write([]byte("s2c")); err != nil {
		t.Fatal(err)
	}

	// client reads the response
	if _, err := io.ReadFull(clientStream, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "s2c" {
		t.Fatalf("unexpected data: %q", buf)
	}

	// server half-closes too; client observes EOF
	if err := serverStream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := clientStream.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}

	// both sides half-closed: the stream is fully reclaimed on both ends
	for _, s := range []*Stream{clientStream, serverStream} {
		if err := waitPhaseWithTimeout(t, s, allPhases); err != nil {
			t.Fatal(err)
		}
		if p := s.Phase(); p != allPhases {
			t.Fatalf("expected all phases, got %v", p)
		}
	}

	// notifications fire exactly once and stay fired: repeated waits
	// return immediately with success.
	if err := waitPhaseWithTimeout(t, clientStream, allPhases); err != nil {
		t.Fatal(err)
	}
}

// TestPhaseBufferedUnreadData verifies that buffered-but-unread data does
// not block phase notifications and remains readable after the peer's FIN.
func TestPhaseBufferedUnreadData(t *testing.T) {
	_, stop, cli, serverSessionCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clientSession, _ := Client(cli, nil)
	defer clientSession.Close()
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverStream, _ := serverSession.AcceptStream()

	// client sends data and fully closes before the server reads anything
	data := []byte("buffered payload")
	if _, err := clientStream.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := clientStream.Close(); err != nil {
		t.Fatal(err)
	}
	// the client fully closed before the peer's FIN: local-write-done and
	// closed are observable, peer-write-done is not part of this stream's
	// lifecycle anymore.
	if err := waitPhaseWithTimeout(t, clientStream, PhaseLocalWriteDone|PhaseClosed); err != nil {
		t.Fatal(err)
	}

	// server observes peer-write-done while data is still unread
	if err := waitPhaseWithTimeout(t, serverStream, PhasePeerWriteDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverStream.PhaseChan(PhaseClosed):
		t.Fatal("PhaseClosed fired while server side is still open")
	default:
	}

	// buffered data must still be delivered, followed by EOF
	buf := make([]byte, len(data))
	if _, err := io.ReadFull(serverStream, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data) {
		t.Fatalf("data mismatch: got %q, want %q", buf, data)
	}
	if _, err := serverStream.Read(buf); err != io.EOF {
		t.Fatalf("expected EOF after buffered data, got %v", err)
	}

	// server closes its side: all phases complete
	if err := serverStream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := waitPhaseWithTimeout(t, serverStream, allPhases); err != nil {
		t.Fatal(err)
	}
}

// TestPhaseAbruptSessionClose verifies that unfinished phases end with a
// reason instead of hanging when the underlying connection fails.
func TestPhaseAbruptSessionClose(t *testing.T) {
	_, stop, cli, serverSessionCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clientSession, _ := Client(cli, nil)
	defer clientSession.Close()
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverStream, _ := serverSession.AcceptStream()

	// abruptly kill the underlying connection
	cli.Close()

	// unfinished phases on both ends must end with a reason, not hang
	for _, s := range []*Stream{clientStream, serverStream} {
		err := waitPhaseWithTimeout(t, s, PhaseLocalWriteDone|PhasePeerWriteDone)
		if err == nil {
			t.Fatal("expected error for unfinished phases after connection failure")
		}
	}
}

// TestPhaseGracefulSessionClose verifies phases when the session is closed
// locally without an underlying connection error.
func TestPhaseGracefulSessionClose(t *testing.T) {
	_, stop, cli, serverSessionCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clientSession, _ := Client(cli, nil)
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverSession.AcceptStream()

	clientSession.Close()

	// PhaseClosed fires on session close
	if err := waitPhaseWithTimeout(t, clientStream, PhaseClosed); err != nil {
		t.Fatal(err)
	}
	// unfinished phases end with a reason: io.ErrClosedPipe, or the socket
	// error from closing the underlying connection, whichever is observed.
	if err := waitPhaseWithTimeout(t, clientStream, PhaseLocalWriteDone); err == nil {
		t.Fatal("expected error for unfinished phase after session close")
	}
}

// TestPhaseConcurrentClose verifies deterministic phase semantics when
// CloseWrite, Close and the peer's FIN race with each other.
func TestPhaseConcurrentClose(t *testing.T) {
	_, stop, cli, serverSessionCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clientSession, _ := Client(cli, nil)
	defer clientSession.Close()
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverStream, _ := serverSession.AcceptStream()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		clientStream.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		clientStream.Close()
	}()
	go func() {
		defer wg.Done()
		serverStream.Close() // sends the peer FIN
	}()
	wg.Wait()

	// Deterministic outcome regardless of the interleaving: local-write-done
	// and closed are always reached. Peer-write-done is reached iff the
	// peer's FIN arrived before the stream was reclaimed by the full close;
	// either way waiting on it must end promptly, never hang.
	for _, s := range []*Stream{clientStream, serverStream} {
		if err := waitPhaseWithTimeout(t, s, PhaseLocalWriteDone|PhaseClosed); err != nil {
			t.Fatal(err)
		}
		peerErr := waitPhaseWithTimeout(t, s, PhasePeerWriteDone)
		p := s.Phase()
		if p&PhaseLocalWriteDone == 0 || p&PhaseClosed == 0 {
			t.Fatalf("missing phases: %v", p)
		}
		if p&PhasePeerWriteDone != 0 {
			if peerErr != nil {
				t.Fatalf("peer-write-done reached but WaitPhase reported %v", peerErr)
			}
			select {
			case <-s.PhaseChan(PhasePeerWriteDone):
			default:
				t.Fatal("phase bit set but notification channel not closed")
			}
		}
	}

	// Read/Write conventions are preserved after the race
	if _, err := clientStream.Write([]byte("x")); err != io.ErrClosedPipe {
		t.Fatalf("write after close should return io.ErrClosedPipe, got %v", err)
	}
	// a read that starts before die is observed reports io.ErrClosedPipe,
	// a read that starts after reports io.EOF; both are valid here.
	if _, err := clientStream.Read(make([]byte, 1)); err != io.EOF && err != io.ErrClosedPipe {
		t.Fatalf("read after close should return io.EOF or io.ErrClosedPipe, got %v", err)
	}
}

// TestPhaseV2 verifies phase notifications with protocol version 2.
func TestPhaseV2(t *testing.T) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	config := DefaultConfig()
	config.Version = 2

	serverSessionCh := make(chan *Session, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sess, _ := Server(conn, config)
		serverSessionCh <- sess
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	clientSession, _ := Client(conn, config)
	defer clientSession.Close()
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverStream, _ := serverSession.AcceptStream()

	if _, err := clientStream.Write([]byte("v2 data")); err != nil {
		t.Fatal(err)
	}
	if err := clientStream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := serverStream.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	for _, s := range []*Stream{clientStream, serverStream} {
		if err := waitPhaseWithTimeout(t, s, allPhases); err != nil {
			t.Fatal(err)
		}
		if p := s.Phase(); p != allPhases {
			t.Fatalf("expected all phases, got %v", p)
		}
	}
}

// TestPhaseMonotonic verifies the phase bitmask never loses bits.
func TestPhaseMonotonic(t *testing.T) {
	_, stop, cli, serverSessionCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	clientSession, _ := Client(cli, nil)
	defer clientSession.Close()
	serverSession := <-serverSessionCh
	defer serverSession.Close()

	clientStream, _ := clientSession.OpenStream()
	serverStream, _ := serverSession.AcceptStream()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var prev Phase
		for i := 0; i < 1000; i++ {
			p := clientStream.Phase()
			if p&prev != prev {
				t.Errorf("phase regressed: %v -> %v", prev, p)
				return
			}
			prev = p
			if p == allPhases {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	clientStream.CloseWrite()
	serverStream.Close()
	clientStream.Close()

	<-done
	if p := clientStream.Phase(); p != allPhases {
		t.Fatalf("expected all phases, got %v", p)
	}
}
