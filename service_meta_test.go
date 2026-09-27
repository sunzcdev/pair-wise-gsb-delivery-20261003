// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func testServiceConfig() *Config {
	cfg := DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.Logger = nil
	return cfg
}

func testServicePair(t *testing.T, handler AdmissionHandler) (client, server *Session) {
	t.Helper()
	conn1, conn2 := testConnPipe(t)

	serverCfg := testServiceConfig()
	serverCfg.AdmissionHandler = handler
	server, err := Server(conn2, serverCfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	clientCfg := testServiceConfig()
	clientCfg.EnableServiceMetadata = true
	client, err = Client(conn1, clientCfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// echoAccepted accepts streams in a loop, records their service names on
// accepted, and echoes any data back.
func echoAccepted(t *testing.T, server *Session, accepted chan<- string) {
	t.Helper()
	go func() {
		for {
			stream, err := server.AcceptStream()
			if err != nil {
				return
			}
			if meta := stream.ServiceMetadata(); meta != nil {
				accepted <- meta.Service
			} else {
				accepted <- ""
			}
			go io.Copy(stream, stream) //nolint:errcheck
		}
	}()
}

func TestServiceMeta_AdmitRejectAndConcurrentStreams(t *testing.T) {
	release := make(chan struct{})
	handler := func(ctx context.Context, meta *StreamMetadata) error {
		switch meta.Service {
		case "blocked":
			return errors.New("service not allowed")
		case "slow":
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	client, server := testServicePair(t, handler)
	accepted := make(chan string, 8)
	echoAccepted(t, server, accepted)

	// Open a stream whose admission handler blocks until released.
	slowResult := make(chan error, 1)
	go func() {
		stream, err := client.OpenService(context.Background(), "slow", map[string]string{"version": "1"})
		if err == nil {
			_ = stream.Close()
		}
		slowResult <- err
	}()

	// While the slow admission is pending, a rejected stream must fail
	// fast with ErrStreamRejected.
	deadline := time.After(5 * time.Second)
	_, err := client.OpenService(context.Background(), "blocked", nil)
	if !errors.Is(err, ErrStreamRejected) {
		t.Fatalf("expected ErrStreamRejected, got %v", err)
	}

	// And an unrelated stream must still flow data in both directions,
	// proving the pending admission does not stall the session.
	stream, err := client.OpenService(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("open echo: %v", err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad echo: %q", buf)
	}

	// The rejected stream must never have reached Accept.
	got := <-accepted
	if got != "echo" {
		t.Fatalf("expected echo service, got %q", got)
	}

	// Release the slow admission; it must complete successfully.
	close(release)
	select {
	case err := <-slowResult:
		if err != nil {
			t.Fatalf("slow open: %v", err)
		}
	case <-deadline:
		t.Fatal("slow admission never completed")
	}
	if got := <-accepted; got != "slow" {
		t.Fatalf("expected slow service, got %q", got)
	}
}

func TestServiceMeta_MetadataDelivered(t *testing.T) {
	handler := func(ctx context.Context, meta *StreamMetadata) error {
		return nil
	}
	client, server := testServicePair(t, handler)

	type result struct {
		meta *StreamMetadata
	}
	gotCh := make(chan result, 1)
	go func() {
		stream, err := server.AcceptStream()
		if err != nil {
			close(gotCh)
			return
		}
		gotCh <- result{meta: stream.ServiceMetadata()}
	}()

	attrs := map[string]string{"version": "2", "tenant": "acme"}
	stream, err := client.OpenService(context.Background(), "kvstore", attrs)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()

	got := <-gotCh
	if got.meta == nil {
		t.Fatal("no metadata on accepted stream")
	}
	if got.meta.Service != "kvstore" {
		t.Fatalf("bad service: %q", got.meta.Service)
	}
	if got.meta.Attributes["version"] != "2" || got.meta.Attributes["tenant"] != "acme" {
		t.Fatalf("bad attributes: %v", got.meta.Attributes)
	}
}

func TestServiceMeta_AdmissionCancelOnClientCancel(t *testing.T) {
	entered := make(chan struct{})
	handlerErr := make(chan error, 1)
	handler := func(ctx context.Context, meta *StreamMetadata) error {
		close(entered)
		<-ctx.Done()
		handlerErr <- ctx.Err()
		return ctx.Err()
	}
	client, _ := testServicePair(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	openErr := make(chan error, 1)
	go func() {
		_, err := client.OpenService(ctx, "slow", nil)
		openErr <- err
	}()

	<-entered
	cancel()

	if err := <-openErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	select {
	case err := <-handlerErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission handler was not canceled")
	}
}

func TestServiceMeta_MetadataTooLarge(t *testing.T) {
	client, _ := testServicePair(t, func(context.Context, *StreamMetadata) error { return nil })

	big := map[string]string{"blob": strings.Repeat("x", 5000)}
	_, err := client.OpenService(context.Background(), "svc", big)
	if !errors.Is(err, ErrMetadataTooLarge) {
		t.Fatalf("expected ErrMetadataTooLarge, got %v", err)
	}
}

// TestServiceMeta_LegacyPeer drives a raw frame-level peer that only
// understands the base protocol (like an old yamux): it echoes pings and
// ACKs streams. The extension must negotiate down cleanly and never emit
// a frame the legacy peer would reject.
func TestServiceMeta_LegacyPeer(t *testing.T) {
	conn1, conn2 := testConnPipe(t)

	legacyDone := make(chan struct{})
	go func() {
		defer close(legacyDone)
		hdr := make([]byte, headerSize)
		for {
			if _, err := io.ReadFull(conn2, hdr); err != nil {
				return
			}
			msgType := hdr[1]
			flags := binary.BigEndian.Uint16(hdr[2:4])
			id := binary.BigEndian.Uint32(hdr[4:8])
			length := binary.BigEndian.Uint32(hdr[8:12])
			switch msgType {
			case typePing:
				if flags&flagSYN == 0 {
					continue
				}
				// Old peers echo the opaque ping value verbatim.
				resp := header(make([]byte, headerSize))
				resp.encode(typePing, flagACK, 0, length)
				if _, err := conn2.Write(resp); err != nil {
					return
				}
			case typeWindowUpdate:
				if flags&flagSYN == 0 {
					continue
				}
				resp := header(make([]byte, headerSize))
				resp.encode(typeWindowUpdate, flagACK, id, 0)
				if _, err := conn2.Write(resp); err != nil {
					return
				}
			case typeData:
				if _, err := io.CopyN(io.Discard, conn2, int64(length)); err != nil {
					return
				}
			default:
				// An old peer would reset the session on unknown frames.
				t.Errorf("legacy peer received unknown frame type %d", msgType)
				return
			}
		}
	}()

	cfg := testServiceConfig()
	cfg.EnableServiceMetadata = true
	client, err := Client(conn1, cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()

	// Extension use must fail cleanly against the legacy peer.
	_, err = client.OpenService(context.Background(), "svc", nil)
	if !errors.Is(err, ErrServiceMetaUnsupported) {
		t.Fatalf("expected ErrServiceMetaUnsupported, got %v", err)
	}

	// Plain streams and pings keep working on the same connection.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = stream.Close()

	if client.IsClosed() {
		t.Fatal("session must stay healthy with a legacy peer")
	}
}

// TestServiceMeta_DisabledServerUsesNewCode checks interop between a
// metadata-enabled client and a server running the new code with the
// extension turned off: plain streams work, OpenService fails cleanly.
func TestServiceMeta_DisabledServerUsesNewCode(t *testing.T) {
	conn1, conn2 := testConnPipe(t)

	server, err := Server(conn2, testServiceConfig())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer server.Close()

	clientCfg := testServiceConfig()
	clientCfg.EnableServiceMetadata = true
	client, err := Client(conn1, clientCfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer client.Close()

	go func() {
		stream, err := server.AcceptStream()
		if err != nil {
			return
		}
		io.Copy(stream, stream) //nolint:errcheck
	}()

	if _, err := client.OpenService(context.Background(), "svc", nil); !errors.Is(err, ErrServiceMetaUnsupported) {
		t.Fatalf("expected ErrServiceMetaUnsupported, got %v", err)
	}

	stream, err := client.OpenStream()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("bad echo: %q", buf)
	}
}

func TestServiceMeta_CodecRoundTrip(t *testing.T) {
	meta := &StreamMetadata{
		Service:    "cache",
		Attributes: map[string]string{"version": "3", "tenant": "acme", "empty": ""},
	}
	buf, err := encodeMetadata(meta)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeMetadata(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Service != meta.Service {
		t.Fatalf("service mismatch: %q", back.Service)
	}
	for k, v := range meta.Attributes {
		if back.Attributes[k] != v {
			t.Fatalf("attribute %q mismatch: %q", k, back.Attributes[k])
		}
	}

	// Truncated and trailing payloads must be rejected.
	for _, bad := range [][]byte{
		{},
		{0},
		buf[:len(buf)-1],
		append(append([]byte{}, buf...), 0),
	} {
		if _, err := decodeMetadata(bad); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("expected ErrInvalidMetadata for %v, got %v", bad, err)
		}
	}
}

func ExampleSession_OpenService() {
	clientConn, serverConn := testConnPipe(nil)

	serverCfg := DefaultConfig()
	serverCfg.EnableKeepAlive = false
	serverCfg.LogOutput = io.Discard
	serverCfg.Logger = nil
	serverCfg.AdmissionHandler = func(ctx context.Context, meta *StreamMetadata) error {
		if meta.Service != "echo" {
			return fmt.Errorf("unknown service %q", meta.Service)
		}
		return nil
	}
	server, _ := Server(serverConn, serverCfg)
	defer server.Close()

	clientCfg := DefaultConfig()
	clientCfg.EnableKeepAlive = false
	clientCfg.LogOutput = io.Discard
	clientCfg.Logger = nil
	clientCfg.EnableServiceMetadata = true
	client, _ := Client(clientConn, clientCfg)
	defer client.Close()

	go func() {
		for {
			stream, err := server.Accept()
			if err != nil {
				return
			}
			go io.Copy(stream, stream) //nolint:errcheck
		}
	}()

	// A rejected stream fails before any data is exchanged.
	if _, err := client.OpenService(context.Background(), "admin", nil); err != nil {
		fmt.Println("admin:", err)
	}

	// An admitted stream is a regular yamux stream.
	stream, err := client.OpenService(context.Background(), "echo", nil)
	if err != nil {
		fmt.Println("echo open:", err)
		return
	}
	if _, err := stream.Write([]byte("hello")); err != nil {
		fmt.Println("echo write:", err)
		return
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil {
		fmt.Println("echo read:", err)
		return
	}
	fmt.Println("echo:", string(buf))

	// Output:
	// admin: stream rejected by peer
	// echo: hello
}
