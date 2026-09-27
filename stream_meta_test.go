// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func testConfStreamMeta(admit func(ctx context.Context, meta []byte) bool) *Config {
	conf := testConf()
	conf.EnableStreamMeta = true
	conf.AdmitStream = admit
	return conf
}

// waitExtEnabled waits for the stream metadata negotiation to conclude.
func waitExtEnabled(t *testing.T, s *Session, want bool) {
	t.Helper()
	select {
	case <-s.extProbeDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("capability negotiation did not complete")
	}
	if got := s.streamMetaEnabled == 1; got != want {
		t.Fatalf("streamMetaEnabled=%v, want %v", got, want)
	}
}

func TestStreamMeta_Negotiation(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConfStreamMeta(nil))

	waitExtEnabled(t, client, true)
	waitExtEnabled(t, server, true)

	// Ordinary pings must still work alongside the reserved probe IDs.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("err: %v", err)
	}
}

// TestStreamMeta_NewClientOldServer verifies that a client with the
// extension enabled never sends metadata frames to a peer that does
// not support them, and that plain streams keep working.
func TestStreamMeta_NewClientOldServer(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConf())

	waitExtEnabled(t, client, false)

	// Metadata open must fail locally without sending unknown frames.
	_, err := client.OpenStreamWithMeta(context.Background(), []byte("svc"))
	if err != ErrStreamMetaUnsupported {
		t.Fatalf("err: %v", err)
	}

	// Plain streams are unaffected.
	go func() {
		stream, err := server.AcceptStream()
		if err != nil {
			return
		}
		_, _ = io.Copy(stream, stream)
	}()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("err: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad: %s", buf)
	}
}

// TestStreamMeta_OldClientNewServer verifies that a server with the
// extension enabled serves old clients that open plain streams.
func TestStreamMeta_OldClientNewServer(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn,
		testConf(), testConfStreamMeta(func(ctx context.Context, meta []byte) bool {
			t.Errorf("admission callback must not run for plain streams")
			return false
		}))

	waitExtEnabled(t, server, false)

	go func() {
		stream, err := server.AcceptStream()
		if err != nil {
			return
		}
		_, _ = io.Copy(stream, stream)
	}()

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("err: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad: %s", buf)
	}
}

// TestStreamMeta_RejectAndServe opens two streams over one session:
// the "blocked" stream is rejected by the admission callback while the
// "echo" stream is admitted and serves traffic.
func TestStreamMeta_RejectAndServe(t *testing.T) {
	var mu sync.Mutex
	admitted := map[string]bool{}
	admit := func(ctx context.Context, meta []byte) bool {
		mu.Lock()
		defer mu.Unlock()
		admitted[string(meta)] = true
		return string(meta) == "echo"
	}

	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConfStreamMeta(admit))

	// Server accepts and echoes admitted streams.
	go func() {
		for {
			stream, err := server.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(stream, stream)
				_ = stream.Close()
			}()
		}
	}()

	// Open the stream that will be rejected.
	blocked, err := client.OpenStreamWithMeta(context.Background(), []byte("blocked"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Open the stream that will be admitted.
	echo, err := client.OpenStreamWithMeta(context.Background(), []byte("echo"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// The rejected stream must surface a reset.
	_, werr := blocked.Write([]byte("hello"))
	if werr == nil {
		buf := make([]byte, 5)
		_, werr = blocked.Read(buf)
	}
	if werr != ErrConnectionReset {
		t.Fatalf("err: %v", werr)
	}

	// The admitted stream must read and write normally.
	if _, err := echo.Write([]byte("ping")); err != nil {
		t.Fatalf("err: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(echo, buf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad: %s", buf)
	}

	// The admission callback saw both streams.
	mu.Lock()
	defer mu.Unlock()
	if !admitted["blocked"] || !admitted["echo"] {
		t.Fatalf("bad: %v", admitted)
	}
}

// TestStreamMeta_MetaDelivered verifies the accepted stream exposes
// the metadata to the application.
func TestStreamMeta_MetaDelivered(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConfStreamMeta(nil))

	stream, err := client.OpenStreamWithMeta(context.Background(), []byte("kv-store"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer func() { _ = stream.Close() }()

	accepted, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(accepted.Meta()) != "kv-store" {
		t.Fatalf("bad: %q", accepted.Meta())
	}
}

// TestStreamMeta_SlowAdmissionDoesNotBlock verifies that a slow
// admission callback does not stall data or control frames of other
// streams.
func TestStreamMeta_SlowAdmissionDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	admitStarted := make(chan struct{})
	admit := func(ctx context.Context, meta []byte) bool {
		close(admitStarted)
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}

	clientConn, serverConn := testConnPipe(t)
	client, server := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConfStreamMeta(admit))

	// Open a stream whose admission blocks.
	slow, err := client.OpenStreamWithMeta(context.Background(), []byte("slow"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer func() { _ = slow.Close() }()

	select {
	case <-admitStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("admission callback did not start")
	}

	// While admission is blocked, a plain stream must flow end to end.
	go func() {
		stream, err := server.AcceptStream()
		if err != nil {
			return
		}
		_, _ = io.Copy(stream, stream)
	}()

	plain, err := client.OpenStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, err := plain.Write([]byte("ping")); err != nil {
		t.Fatalf("err: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(plain, buf); err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad: %s", buf)
	}

	// Control frames must flow as well.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Unblock admission; the slow stream is now accepted.
	close(release)
	accepted, err := server.AcceptStream()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(accepted.Meta()) != "slow" {
		t.Fatalf("bad: %q", accepted.Meta())
	}
}

// TestStreamMeta_AdmissionCancelled verifies the admission callback
// context is cancelled when the session shuts down mid-decision.
func TestStreamMeta_AdmissionCancelled(t *testing.T) {
	cancelled := make(chan struct{})
	admit := func(ctx context.Context, meta []byte) bool {
		<-ctx.Done()
		close(cancelled)
		return false
	}

	clientConn, serverConn := testConnPipe(t)
	client, _ := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConfStreamMeta(admit))

	stream, err := client.OpenStreamWithMeta(context.Background(), []byte("slow"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_ = stream.Close()

	// Shutting down the client session must cancel the pending
	// admission on the server side.
	_ = client.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("admission callback was not cancelled")
	}
}

func TestStreamMeta_TooLarge(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	client, _ := testClientServerConfig(t, clientConn, serverConn,
		testConfStreamMeta(nil), testConfStreamMeta(nil))

	meta := make([]byte, maxStreamMetaSize+1)
	if _, err := client.OpenStreamWithMeta(context.Background(), meta); err != ErrStreamMetaTooLarge {
		t.Fatalf("err: %v", err)
	}

	// Exactly the limit is accepted.
	stream, err := client.OpenStreamWithMeta(context.Background(), make([]byte, maxStreamMetaSize))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_ = stream.Close()
}

func TestStreamMeta_DisabledLocally(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	client, _ := testClientServerConfig(t, clientConn, serverConn,
		testConf(), testConf())

	_, err := client.OpenStreamWithMeta(context.Background(), []byte("svc"))
	if err != ErrStreamMetaUnsupported {
		t.Fatalf("err: %v", err)
	}
}

// TestStreamMeta_OpenContextCancel verifies the open can be cancelled
// while waiting for capability negotiation.
func TestStreamMeta_OpenContextCancel(t *testing.T) {
	clientConn, serverConn := testConnPipe(t)
	clientConf := testConfStreamMeta(nil)
	// Make the negotiation hang by never answering the capability
	// probe within the test window.
	clientConf.ConnectionWriteTimeout = 5 * time.Second
	client, err := Client(clientConn, clientConf)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	go func() { _, _ = io.Copy(io.Discard, serverConn) }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = client.OpenStreamWithMeta(ctx, []byte("svc"))
	if err != context.DeadlineExceeded {
		t.Fatalf("err: %v", err)
	}
}
