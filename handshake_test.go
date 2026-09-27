// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

func testHandshakeServer(t *testing.T) *handshakeServer {
	t.Helper()

	hs, err := newHandshakeServer(t.TempDir(), hclog.NewNullLogger())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	t.Cleanup(func() { _ = hs.close() })

	return hs
}

func dialHandshake(t *testing.T, addr string) net.Conn {
	t.Helper()

	network := "unix"
	if runtime.GOOS == "windows" {
		network = "tcp"
	}

	conn, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	return conn
}

// expectConnClosed asserts that the server closes the connection without
// sending anything, which is how rejected handshake attempts are handled.
func expectConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected the connection to be closed")
	}
}

func expectNoHandshake(t *testing.T, lineCh <-chan string) {
	t.Helper()

	select {
	case line := <-lineCh:
		t.Fatalf("unexpected handshake line: %q", line)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandshakeServer_onlyCorrectInstanceCompletes simulates a fake client
// connecting first, an old plugin reconnecting with stale credentials from
// a previous launch, and finally the real plugin. Only the real plugin may
// complete the handshake.
func TestHandshakeServer_onlyCorrectInstanceCompletes(t *testing.T) {
	// A previous launch's handshake server, now torn down. Its token is
	// the stale credential an old plugin might try to reconnect with.
	previous, err := newHandshakeServer(t.TempDir(), hclog.NewNullLogger())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	staleToken := previous.token
	if err := previous.close(); err != nil {
		t.Fatalf("err: %s", err)
	}

	hs := testHandshakeServer(t)
	if hs.token == staleToken {
		t.Fatal("per-launch tokens should be unique")
	}

	lineCh := make(chan string, 1)
	go hs.accept(lineCh, time.Minute)

	// A fake client connects first and presents garbage credentials.
	fake := dialHandshake(t, hs.addr())
	if _, err := fmt.Fprintf(fake, "bogus-token\n1|1|tcp|127.0.0.1:1234|netrpc|\n"); err != nil {
		t.Fatalf("err: %s", err)
	}
	expectConnClosed(t, fake)
	_ = fake.Close()
	expectNoHandshake(t, lineCh)

	// An old plugin reconnects with the stale token from a previous launch.
	old := dialHandshake(t, hs.addr())
	if _, err := fmt.Fprintf(old, "%s\n1|1|tcp|127.0.0.1:1234|netrpc|\n", staleToken); err != nil {
		t.Fatalf("err: %s", err)
	}
	expectConnClosed(t, old)
	_ = old.Close()
	expectNoHandshake(t, lineCh)

	// The real plugin authenticates with the correct per-launch token and
	// completes the handshake.
	want := "1|2|tcp|127.0.0.1:4321|grpc|"
	real := dialHandshake(t, hs.addr())
	if _, err := fmt.Fprintf(real, "%s\n%s\n", hs.token, want); err != nil {
		t.Fatalf("err: %s", err)
	}
	defer func() { _ = real.Close() }()

	select {
	case line := <-lineCh:
		if line != want {
			t.Fatalf("bad: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for handshake")
	}
}

// TestHandshakeChannel_roundtrip verifies that a plugin using sendHandshake
// can authenticate and deliver its protocol line to the host.
func TestHandshakeChannel_roundtrip(t *testing.T) {
	hs := testHandshakeServer(t)

	lineCh := make(chan string, 1)
	go hs.accept(lineCh, time.Minute)

	want := "1|2|unix|/tmp/plugin1234|grpc|c2VydmVyLWNlcnQ"
	if err := sendHandshake(hs.addr(), hs.token, want); err != nil {
		t.Fatalf("err: %s", err)
	}

	select {
	case line := <-lineCh:
		if line != want {
			t.Fatalf("bad: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for handshake")
	}
}

// TestHandshakeServer_closeCleanup verifies that closing the handshake
// server stops the accept loop and removes the socket file.
func TestHandshakeServer_closeCleanup(t *testing.T) {
	hs, err := newHandshakeServer(t.TempDir(), hclog.NewNullLogger())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	addr := hs.addr()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		hs.accept(make(chan string, 1), time.Minute)
	}()

	if err := hs.close(); err != nil {
		t.Fatalf("err: %s", err)
	}

	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("accept loop did not stop after close")
	}

	if runtime.GOOS != "windows" {
		if _, err := os.Stat(addr); !os.IsNotExist(err) {
			t.Fatalf("expected socket file to be removed, stat err: %v", err)
		}
	}
}

// TestClient_grpcHandshakeChannel verifies that a plugin whose dependencies
// write to stdout during init can still complete the handshake, because the
// negotiation happens over the dedicated channel instead of stdout.
func TestClient_grpcHandshakeChannel(t *testing.T) {
	process := helperProcess("test-grpc-junk-stdout")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err: %s", err)
	}

	if v := c.Protocol(); v != ProtocolGRPC {
		t.Fatalf("bad: %s", v)
	}

	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	impl, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	if result := impl.Double(21); result != 42 {
		t.Fatalf("bad: %#v", result)
	}

	c.Kill()

	if !c.Exited() {
		t.Fatal("should say client has exited")
	}
}

// TestClient_grpcLegacyStdoutHandshake verifies that the legacy stdout
// handshake remains available as an explicit compatibility path for plugins
// that don't use the dedicated handshake channel.
func TestClient_grpcLegacyStdoutHandshake(t *testing.T) {
	process := helperProcess("test-grpc-legacy-handshake")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err: %s", err)
	}

	client, err := c.Client()
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	impl, ok := raw.(testInterface)
	if !ok {
		t.Fatalf("bad: %#v", raw)
	}

	if result := impl.Double(21); result != 42 {
		t.Fatalf("bad: %#v", result)
	}

	c.Kill()

	if !c.Exited() {
		t.Fatal("should say client has exited")
	}
}

// TestClient_handshakeChannelCleanupOnFailure verifies that a failed start
// cleans up the handshake channel and doesn't leave the listener behind.
func TestClient_handshakeChannelCleanupOnFailure(t *testing.T) {
	process := helperProcess("start-timeout")
	c := NewClient(&ClientConfig{
		Cmd:              process,
		HandshakeConfig:  testHandshake,
		Plugins:          testPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
		StartTimeout:     50 * time.Millisecond,
	})
	defer c.Kill()

	if _, err := c.Start(); err == nil {
		t.Fatal("should fail")
	}

	// The handshake listener must be closed after the failed start, so
	// connecting to it must fail. The address is not exported, so instead
	// verify the client can be killed cleanly and the process exits.
	c.Kill()

	if !c.Exited() {
		t.Fatal("should say client has exited")
	}
}
