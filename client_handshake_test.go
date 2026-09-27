// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/internal/cmdrunner"
	"github.com/hashicorp/go-plugin/runner"
)

// delayedStartRunner delays starting the real plugin process until the
// start channel is closed, so tests can interact with the handshake channel
// before the plugin connects.
type delayedStartRunner struct {
	runner.Runner
	start <-chan struct{}
}

func (r *delayedStartRunner) Start(ctx context.Context) error {
	select {
	case <-r.start:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Runner.Start(ctx)
}

// clientHandshakeForTest waits for the client to publish its handshake
// channel and returns it.
func clientHandshakeForTest(t *testing.T, c *Client) *clientHandshake {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if hs := c.handshake.Load(); hs != nil {
			return hs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the client handshake channel")
	return nil
}

// sendHandshakeForTest connects to the handshake channel and presents the
// given token, expecting the client to reject and close the connection.
func sendHandshakeForTest(t *testing.T, addr net.Addr, token string) {
	t.Helper()
	conn, err := net.DialTimeout(addr.Network(), addr.String(), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to handshake channel: %s", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = fmt.Fprintf(conn, "%s|%d|%d|tcp|127.0.0.1:1234|netrpc\n",
		token, CoreProtocolVersion, testHandshake.ProtocolVersion)
	if err != nil {
		t.Fatalf("failed to write handshake line: %s", err)
	}

	// A rejected connection must be closed by the client.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the client to close the rejected handshake connection")
	}
}

// TestClient_handshakeSocket tests the dedicated handshake channel end to
// end: a fake client and an old plugin reconnecting with the previous
// launch's credentials are both rejected, and only the real plugin
// instance, which received this launch's token via its environment,
// completes the handshake.
func TestClient_handshakeSocket(t *testing.T) {
	// Launch a plugin normally to obtain a legitimate handshake token. By
	// the time it is used below, it is a stale credential from a previous
	// launch.
	c1 := NewClient(&ClientConfig{
		Cmd:             helperProcess("test-interface"),
		HandshakeConfig: testHandshake,
		Plugins:         testPluginMap,
	})
	if _, err := c1.Start(); err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}
	hs1 := c1.handshake.Load()
	if hs1 == nil {
		t.Fatal("expected the client to offer a handshake channel")
	}
	staleToken := hs1.token
	c1.Kill()

	// Set up the real plugin, but hold back its process start until the
	// impostors below have had their chance.
	startCh := make(chan struct{})
	process := helperProcess("test-interface")
	c2 := NewClient(&ClientConfig{
		HandshakeConfig: testHandshake,
		Plugins:         testPluginMap,
		RunnerFunc: func(l hclog.Logger, cmd *exec.Cmd, tmpDir string) (runner.Runner, error) {
			process.Env = append(process.Env, cmd.Env...)
			r, err := cmdrunner.NewCmdRunner(l, process)
			if err != nil {
				return nil, err
			}
			return &delayedStartRunner{Runner: r, start: startCh}, nil
		},
	})

	startResult := make(chan error, 1)
	go func() {
		_, err := c2.Start()
		startResult <- err
	}()

	hs2 := clientHandshakeForTest(t, c2)
	addr := hs2.listener.Addr()

	// A fake client that does not know the token must be rejected.
	sendHandshakeForTest(t, addr, "bogus-token")

	// An old plugin reconnecting with the previous launch's credentials
	// must be rejected too.
	sendHandshakeForTest(t, addr, staleToken)

	// Neither impostor may have completed the handshake.
	select {
	case err := <-startResult:
		t.Fatalf("Start returned before the real plugin connected: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Now start the real plugin; only it can complete the handshake.
	close(startCh)
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("err should be nil, got %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the plugin handshake")
	}

	// The handshake channel must be cleaned up once the handshake is done.
	if conn, err := net.Dial(addr.Network(), addr.String()); err == nil {
		_ = conn.Close()
		t.Fatal("handshake listener should be closed after the handshake")
	}
	if hs2.socketDir != "" {
		if _, err := os.Stat(hs2.socketDir); !os.IsNotExist(err) {
			t.Fatalf("handshake socket dir should be removed, stat err: %v", err)
		}
	}

	// The plugin should be fully usable.
	client, err := c2.Client()
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

	c2.Kill()
}

// TestClient_handshakeSocketJunkStdout tests that output printed to stdout
// by a plugin's dependencies before the handshake does not break startup
// when the dedicated handshake channel is used.
func TestClient_handshakeSocketJunkStdout(t *testing.T) {
	process := helperProcess("test-interface-junk-stdout")
	c := NewClient(&ClientConfig{
		Cmd:             process,
		HandshakeConfig: testHandshake,
		Plugins:         testPluginMap,
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}
}

// TestClient_handshakeSocketEnv tests that the handshake channel
// environment variables are only given to the plugin when the channel is
// enabled.
func TestClient_handshakeSocketEnv(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			process := helperProcess("test-interface")
			var envs []string
			c := NewClient(&ClientConfig{
				HandshakeConfig:        testHandshake,
				Plugins:                testPluginMap,
				DisableHandshakeSocket: disabled,
				RunnerFunc: func(l hclog.Logger, cmd *exec.Cmd, tmpDir string) (runner.Runner, error) {
					envs = cmd.Env
					process.Env = append(process.Env, cmd.Env...)
					return cmdrunner.NewCmdRunner(l, process)
				},
			})
			defer c.Kill()

			if _, err := c.Start(); err != nil {
				t.Fatalf("err should be nil, got %s", err)
			}

			var found bool
			for _, env := range envs {
				if strings.HasPrefix(env, EnvHandshakeToken+"=") {
					found = true
				}
			}
			if found == disabled {
				t.Fatalf("handshake env present = %v, want %v", found, !disabled)
			}
		})
	}
}

// TestClient_disableHandshakeSocket tests the explicit compatibility path:
// with the handshake channel disabled, the legacy stdout handshake is used.
func TestClient_disableHandshakeSocket(t *testing.T) {
	process := helperProcess("test-interface")
	c := NewClient(&ClientConfig{
		Cmd:                    process,
		HandshakeConfig:        testHandshake,
		Plugins:                testPluginMap,
		DisableHandshakeSocket: true,
	})
	defer c.Kill()

	if _, err := c.Start(); err != nil {
		t.Fatalf("err should be nil, got %s", err)
	}

	if c.handshake.Load() != nil {
		t.Fatal("expected no handshake channel when it is disabled")
	}
}

// TestClient_disableHandshakeSocketJunkStdout tests that the legacy stdout
// handshake keeps its strict behavior when the handshake channel is
// disabled: the first stdout line must be the handshake.
func TestClient_disableHandshakeSocketJunkStdout(t *testing.T) {
	process := helperProcess("test-interface-junk-stdout")
	c := NewClient(&ClientConfig{
		Cmd:                    process,
		HandshakeConfig:        testHandshake,
		Plugins:                testPluginMap,
		DisableHandshakeSocket: true,
		StartTimeout:           10 * time.Second,
	})
	defer c.Kill()

	_, err := c.Start()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Unrecognized remote plugin message") {
		t.Fatalf("unexpected error: %s", err)
	}
}

// TestClient_handshakeSocketCleanup tests that the handshake channel's
// resources are cleaned up when the plugin fails to start.
func TestClient_handshakeSocketCleanup(t *testing.T) {
	process := helperProcess("start-timeout")
	c := NewClient(&ClientConfig{
		Cmd:             process,
		HandshakeConfig: testHandshake,
		Plugins:         testPluginMap,
		StartTimeout:    100 * time.Millisecond,
	})

	startResult := make(chan error, 1)
	go func() {
		_, err := c.Start()
		startResult <- err
	}()

	hs := clientHandshakeForTest(t, c)
	addr := hs.listener.Addr()

	if err := <-startResult; err == nil {
		t.Fatal("expected a timeout error")
	}

	// The listener must be closed and the socket directory removed.
	if conn, err := net.Dial(addr.Network(), addr.String()); err == nil {
		_ = conn.Close()
		t.Fatal("handshake listener should be closed after a failed start")
	}
	if hs.socketDir != "" {
		if _, err := os.Stat(hs.socketDir); !os.IsNotExist(err) {
			t.Fatalf("handshake socket dir should be removed after a failed start, stat err: %v", err)
		}
	}

	c.Kill()
}
