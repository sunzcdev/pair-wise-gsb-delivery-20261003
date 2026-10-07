package smux

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// pipeSessions creates a client/server session pair backed by net.Pipe. The
// underlying connections are returned so a test can sever them abruptly.
func pipeSessions(t *testing.T, cfg *Config) (*Session, *Session, net.Conn, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	cli, err := Client(c1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Server(c2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cli, srv, c1, c2
}

// tcpSessions creates a client/server session pair over a real TCP loopback
// connection so that queued FINs survive a local Stream.Close (net.Pipe would
// otherwise tear down synchronously).
func tcpSessions(t *testing.T, cfg *Config) (*Session, *Session, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srvCh := make(chan *Session, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		sess, _ := Server(conn, cfg)
		srvCh <- sess
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cli, err := Client(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := <-srvCh

	cleanup := func() {
		cli.Close()
		srv.Close()
		ln.Close()
	}
	return cli, srv, cleanup
}

// waitPhaseReady waits until a phase fires (for any reason) and fails only if
// it does not fire within 5s. The phase's completion reason is returned.
func waitPhaseReady(t *testing.T, s *Stream, p Phase) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitErr := s.WaitPhase(ctx, p)
	if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
		t.Fatalf("phase %s never fired: %v", p, waitErr)
	}
	return s.PhaseError(p)
}

// waitPhaseClean waits for a phase and asserts it completed without a reason.
func waitPhaseClean(t *testing.T, s *Stream, p Phase) {
	t.Helper()
	if err := waitPhaseReady(t, s, p); err != nil {
		t.Fatalf("phase %s should be clean, got %v", p, err)
	}
}

// assertNotReached asserts that a phase has not fired yet.
func assertNotReached(t *testing.T, s *Stream, p Phase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := s.WaitPhase(ctx, p)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("phase %s unexpectedly reached (err=%v)", p, err)
	}
}

// TestPhaseInterleavedHalfClose drives both ends through an interleaved
// half-close and verifies every phase fires exactly once, in order, cleanly.
func TestPhaseInterleavedHalfClose(t *testing.T) {
	for _, name := range []string{"v1", "v2"} {
		cfg := DefaultConfig()
		if name == "v2" {
			cfg.Version = 2
		}
		t.Run(name, func(t *testing.T) { testPhaseInterleavedHalfClose(t, cfg) })
	}
}

func testPhaseInterleavedHalfClose(t *testing.T, cfg *Config) {
	cli, srv, cleanup := tcpSessions(t, cfg)
	defer cleanup()
	defer cli.Close()
	defer srv.Close()

	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}

	// nothing has happened yet
	assertNotReached(t, cs, PhaseLocalDataDrained)
	assertNotReached(t, ss, PhasePeerDone)

	// client finishes sending
	if _, err := cs.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := cs.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	waitPhaseClean(t, cs, PhaseLocalDataDrained)
	// peer not done yet on the client side
	assertNotReached(t, cs, PhasePeerDone)

	// server drains client data and observes EOF (client FIN)
	buf := make([]byte, 4)
	if _, err := io.ReadFull(ss, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Read(buf); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	// server now knows the *client's* peer (itself receiving client FIN):
	// on the server this is its PeerDone phase; the server has not closed its
	// own write side yet so its LocalDataDrained is still pending.
	waitPhaseClean(t, ss, PhasePeerDone)
	assertNotReached(t, ss, PhaseLocalDataDrained)

	// server replies and half-closes
	if _, err := ss.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}

	// client drains the reply while it is still readable
	rb := make([]byte, 4)
	if _, err := io.ReadFull(cs, rb); err != nil {
		t.Fatal(err)
	}

	if err := ss.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	waitPhaseClean(t, ss, PhaseLocalDataDrained)

	// client now sees the server FIN (EOF)
	if _, err := cs.Read(rb); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}

	// both halves have now exchanged FINs: the streams recycle immediately
	waitPhaseClean(t, cs, PhasePeerDone)
	waitPhaseClean(t, cs, PhaseReclaimed)
	waitPhaseClean(t, ss, PhaseReclaimed)

	// monotonic + one-shot: re-reading errors is stable, current phase is final
	if perr := cs.PhaseError(PhaseReclaimed); perr != nil {
		t.Fatalf("reclaimed error: %v", perr)
	}
	if p, reached, perr := cs.CurrentPhase(); !reached || p != PhaseReclaimed || perr != nil {
		t.Fatalf("client final phase wrong: reached=%v phase=%s err=%v", reached, p, perr)
	}

	deadline := time.Now().Add(3 * time.Second)
	for cli.NumStreams() != 0 || srv.NumStreams() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("streams leaked: cli=%d srv=%d", cli.NumStreams(), srv.NumStreams())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPhaseUnreadBufferedData ensures the peer-done phase fires when FIN
// arrives even though application data is still buffered and unread.
func TestPhaseUnreadBufferedData(t *testing.T) {
	cli, srv, cleanup := tcpSessions(t, DefaultConfig())
	defer cleanup()

	cs, _ := cli.OpenStream()
	ss, _ := srv.AcceptStream()

	if _, err := cs.Write([]byte("data that is never read")); err != nil {
		t.Fatal(err)
	}
	if err := cs.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// server side: FIN received while bytes remain unread
	waitPhaseClean(t, ss, PhasePeerDone)

	// Read convention intact: buffered data is still delivered despite FIN.
	// (Read it before the second FIN tears the stream down.)
	got, err := io.ReadAll(ss)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data that is never read" {
		t.Fatalf("data mismatch: %q", got)
	}

	// server fully closes; the client had already half-closed so both FINs
	// meet and the stream recycles cleanly
	if err := ss.Close(); err != nil {
		t.Fatal(err)
	}
	waitPhaseClean(t, ss, PhaseReclaimed)
	// client observes the server FIN; with its own write already half-closed
	// the two FINs meet and the client-side stream recycles cleanly
	waitPhaseClean(t, cs, PhasePeerDone)
	waitPhaseClean(t, cs, PhaseReclaimed)
}

// TestPhaseAbruptSessionClose severs the underlying connection while phases
// are incomplete; every pending phase must end with a reason instead of
// hanging.
func TestPhaseAbruptSessionClose(t *testing.T) {
	for _, name := range []string{"transport-close", "session-close"} {
		t.Run(name, func(t *testing.T) {
			cli, srv, cc, sc := pipeSessions(t, DefaultConfig())
			cs, err := cli.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			ss, err := srv.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			wg.Add(2)
			var cerr, serr error
			go func() {
				defer wg.Done()
				cerr = cs.WaitPhase(context.Background(), PhaseReclaimed)
			}()
			go func() {
				defer wg.Done()
				serr = ss.WaitPhase(context.Background(), PhaseReclaimed)
			}()

			// let the waiters park, then end the sessions without any FIN
			time.Sleep(50 * time.Millisecond)
			if name == "transport-close" {
				cc.Close()
				sc.Close()
			} else {
				cli.Close()
				srv.Close()
			}

			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("phase observers blocked forever after close")
			}

			if cerr == nil {
				t.Fatal("client phase should end with a reason")
			}
			if serr == nil {
				t.Fatal("server phase should end with a reason")
			}

			// every phase carries a non-nil reason on both streams
			for _, st := range []*Stream{cs, ss} {
				for _, p := range []Phase{
					PhaseLocalDataDrained,
					PhasePeerDone,
					PhaseReclaimed,
				} {
					if e := st.PhaseError(p); e == nil {
						t.Fatalf("%s: expected non-nil error on %s", name, p)
					}
				}
			}

			// one-shot: repeated waits return immediately, same reason
			if again := waitPhaseReady(t, cs, PhaseReclaimed); !errors.Is(again, cerr) {
				t.Fatalf("repeat wait reason changed: %v vs %v", again, cerr)
			}
		})
	}
}

// TestPhaseConcurrentCloseWriteCloseFin hammers CloseWrite, Close and the
// incoming FIN concurrently and asserts deterministic phase end states.
func TestPhaseConcurrentCloseWriteCloseFin(t *testing.T) {
	for i := 0; i < 50; i++ {
		cli, srv, cleanup := tcpSessions(t, DefaultConfig())
		cs, err := cli.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		ss, err := srv.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); cs.CloseWrite() }()
		go func() { defer wg.Done(); cs.Close() }()
		go func() { defer wg.Done(); ss.CloseWrite() }()
		go func() {
			defer wg.Done()
			// wait until client FIN (or close) surfaces, then full-close
			ss.Close()
		}()

		// Every stream must reach a final phase without hanging. The reason is
		// deterministic per ordering: clean when both FINs exchanged before the
		// resources were dropped, io.ErrClosedPipe when Close wins the race.
		for _, want := range []struct {
			st *Stream
			p  Phase
		}{
			{cs, PhaseReclaimed},
			{ss, PhaseReclaimed},
			{cs, PhasePeerDone},
			{ss, PhaseLocalDataDrained},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := want.st.WaitPhase(ctx, want.p)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("iter %d %s hung", i, want.p)
			}
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("iter %d %s unexpected reason: %v", i, want.p, err)
			}
		}
		wg.Wait()
		cleanup()
	}
}

// TestPhaseReadWriteConventions verifies the existing Read/Write return
// agreements are unchanged when phase observation is in use.
func TestPhaseReadWriteConventions(t *testing.T) {
	_, stop, conn, srvCh, err := setupHalfCloseServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	cli, err := Client(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv := <-srvCh
	defer srv.Close()

	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}

	// observing must not block teardown
	ctx, cancel := context.WithCancel(context.Background())
	observed := make(chan error, 1)
	go func() { observed <- cs.WaitPhase(ctx, PhaseReclaimed) }()

	if _, err := cs.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	cs.CloseWrite()

	buf := make([]byte, 1)
	if _, err := io.ReadFull(ss, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write([]byte("y")); err != io.ErrClosedPipe {
		t.Fatalf("write after CloseWrite = %v, want ErrClosedPipe", err)
	}

	ss.CloseWrite()

	// drain the client side until it terminates (EOF or ErrClosedPipe are
	// both valid pre-existing outcomes depending on FIN/CloseWrite ordering)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, rerr := cs.Read(buf)
		if rerr != nil {
			if rerr != io.EOF && rerr != io.ErrClosedPipe {
				t.Fatalf("terminal read = %v, want EOF/ErrClosedPipe", rerr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client read never terminated after server FIN")
		}
	}

	// observation never blocks or delays teardown: the parked observer is
	// released promptly with whatever deterministic reason applied
	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("observer not released after both halves closed")
	}

	cancel()
	// the client stream has reached its final phase one way or another
	waitPhaseReady(t, cs, PhaseReclaimed)

	// double close agreement on a fresh stream
	cs2, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := cs2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cs2.Close(); err != io.ErrClosedPipe {
		t.Fatalf("double close = %v, want ErrClosedPipe", err)
	}
}

// TestPhaseNoFinalizerDependency drives a stream through teardown without
// anyone observing it, then subscribes afterward. Notification channels are
// created lazily and must reflect phases that already fired; closing the
// stream never depends on a finalizer.
func TestPhaseNoFinalizerDependency(t *testing.T) {
	cli, srv, cleanup := tcpSessions(t, DefaultConfig())
	defer cleanup()

	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cs.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	if err := cs.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1)
	if _, err := io.ReadFull(ss, buf); err != nil {
		t.Fatal(err)
	}
	// server fully closes; both FINs meet so the streams recycle immediately
	if err := ss.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for cli.NumStreams() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("client stream not recycled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// subscribe only after everything happened: the lazily-created channels
	// must already be closed and report stored reasons, without a finalizer.
	for _, p := range []Phase{PhaseLocalDataDrained, PhasePeerDone, PhaseReclaimed} {
		ch := cs.PhaseChan(p)
		select {
		case <-ch:
		default:
			t.Fatalf("phase %s channel should already be closed", p)
		}
	}

	runtime.GC()

	if _, reached, _ := cs.CurrentPhase(); !reached {
		t.Fatal("expected at least one phase reached")
	}
}
