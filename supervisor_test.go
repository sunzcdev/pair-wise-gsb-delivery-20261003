// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	grpctest "github.com/hashicorp/go-plugin/test/grpc"
	"google.golang.org/grpc"
)

// testGRPCDoneCtxPlugin is a GRPCPlugin that captures the generation
// context handed to GRPCClient, so tests can inspect its cancellation
// cause after a crash.
type testGRPCDoneCtxPlugin struct {
	NetRPCUnsupportedPlugin

	mu  sync.Mutex
	ctx context.Context
}

func (p *testGRPCDoneCtxPlugin) GRPCServer(b *GRPCBroker, s *grpc.Server) error {
	grpctest.RegisterTestServer(s, &testGRPCServer{broker: b, Impl: &testInterfaceImpl{}})
	return nil
}

func (p *testGRPCDoneCtxPlugin) GRPCClient(ctx context.Context, b *GRPCBroker, conn *grpc.ClientConn) (interface{}, error) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	return &testGRPCClient{broker: b, Client: grpctest.NewTestClient(conn)}, nil
}

func (p *testGRPCDoneCtxPlugin) doneCtx() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctx
}

func waitForSupervisorEvent(t *testing.T, ch <-chan SupervisorEvent, kind SupervisorEventKind, timeout time.Duration) SupervisorEvent {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				return ev
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for supervisor event kind %d", kind)
		}
	}
}

func waitForCondition(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Consecutive start failures must consume the restart budget and
// eventually make the supervisor give up with an identifiable error.
func TestClientSupervised_startFailuresGiveUp(t *testing.T) {
	events := make(chan SupervisorEvent, 32)
	c := NewClient(&ClientConfig{
		Cmd:             helperProcess("invalid-rpc-address"),
		HandshakeConfig: testHandshake,
		Plugins:         testPluginMap,
		Supervisor: &SupervisorConfig{
			MaxRestarts:    2,
			InitialBackoff: 10 * time.Millisecond,
			MaxBackoff:     50 * time.Millisecond,
			Notify:         func(ev SupervisorEvent) { events <- ev },
		},
	})
	defer c.Kill()

	// The initial start fails and hands over to the supervisor.
	if _, err := c.Client(); err == nil {
		t.Fatal("expected initial start to fail")
	}

	ev := waitForSupervisorEvent(t, events, SupervisorEventGaveUp, 30*time.Second)
	if ev.Restarts != 2 {
		t.Fatalf("expected 2 restart attempts, got %d", ev.Restarts)
	}

	if _, err := c.Client(); !errors.Is(err, ErrPluginSupervisorGaveUp) {
		t.Fatalf("expected ErrPluginSupervisorGaveUp, got: %v", err)
	}

	waitForCondition(t, "client to report exited", 5*time.Second, c.Exited)
}

// A plugin that crashes right after the handshake must be restarted as a
// new, independent generation. Clients from the old generation must fail
// with an identifiable error instead of silently reaching the new process.
func TestClientSupervised_crashAfterHandshake(t *testing.T) {
	events := make(chan SupervisorEvent, 32)
	stateFile := filepath.Join(t.TempDir(), "launches")
	c := NewClient(&ClientConfig{
		Cmd:              helperProcess("supervised-grpc", "500", "1", stateFile),
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
		Supervisor: &SupervisorConfig{
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
			Notify:         func(ev SupervisorEvent) { events <- ev },
		},
	})
	defer c.Kill()

	client, err := c.Client()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	impl := raw.(testInterface)
	if got := impl.Double(21); got != 42 {
		t.Fatalf("bad: %d", got)
	}

	gen1 := c.Generation()
	id1 := c.ID()

	// The plugin crashes 500ms after launch; the supervisor restarts it
	// and the second launch (maxCrashes=1) keeps running.
	waitForSupervisorEvent(t, events, SupervisorEventRestarted, 30*time.Second)

	if gen := c.Generation(); gen != gen1+1 {
		t.Fatalf("expected generation %d, got %d", gen1+1, gen)
	}
	if id2 := c.ID(); id2 == id1 {
		t.Fatalf("expected a new process, got the same id %q", id2)
	}

	// The old client belongs to a stale generation.
	if _, err := client.Dispense("test"); !errors.Is(err, ErrPluginStaleGeneration) {
		t.Fatalf("expected ErrPluginStaleGeneration, got: %v", err)
	}
	if err := client.Ping(); !errors.Is(err, ErrPluginStaleGeneration) {
		t.Fatalf("expected ErrPluginStaleGeneration, got: %v", err)
	}

	// A fresh client talks to the new generation.
	client2, err := c.Client()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	raw2, err := client2.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if got := raw2.(testInterface).Double(21); got != 42 {
		t.Fatalf("bad: %d", got)
	}
}

// A plugin that crashes after running healthily for a while must be
// restarted. In-flight calls against the crashed generation fail
// identifiably; old dispensed implementations and broker connections must
// not silently reach the new process.
func TestClientSupervised_healthyThenCrash(t *testing.T) {
	events := make(chan SupervisorEvent, 32)
	stateFile := filepath.Join(t.TempDir(), "launches")
	ctxPlugin := &testGRPCDoneCtxPlugin{}
	c := NewClient(&ClientConfig{
		Cmd:              helperProcess("supervised-grpc", "1500", "1", stateFile),
		HandshakeConfig:  testHandshake,
		Plugins:          map[string]Plugin{"test": ctxPlugin},
		AllowedProtocols: []Protocol{ProtocolGRPC},
		Supervisor: &SupervisorConfig{
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
			Notify:         func(ev SupervisorEvent) { events <- ev },
		},
	})
	defer c.Kill()

	client, err := c.Client()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	raw, err := client.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	impl := raw.(testInterface)

	// The plugin is healthy for a while: business calls, including
	// brokered connections, succeed.
	for i := 1; i <= 3; i++ {
		if got := impl.Double(i); got != 2*i {
			t.Fatalf("bad: %d", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := impl.Bidirectional(); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Wait for the plugin to crash.
	waitForSupervisorEvent(t, events, SupervisorEventRestarting, 30*time.Second)

	// The crashed generation's context is canceled with an identifiable
	// cause, so in-flight calls can tell the plugin crashed and decide
	// whether to retry.
	doneCtx := ctxPlugin.doneCtx()
	if doneCtx == nil {
		t.Fatal("expected a generation context")
	}
	<-doneCtx.Done()
	if !errors.Is(context.Cause(doneCtx), ErrPluginCrashed) {
		t.Fatalf("expected context cause ErrPluginCrashed, got: %v", context.Cause(doneCtx))
	}

	// Calls on the old dispensed implementation, including its broker
	// connections, fail rather than silently reaching the new process.
	if err := impl.Bidirectional(); err == nil {
		t.Fatal("expected error from stale broker connection")
	}

	// The supervisor restarts the plugin; the new generation works,
	// including newly brokered connections.
	waitForSupervisorEvent(t, events, SupervisorEventRestarted, 30*time.Second)

	client2, err := c.Client()
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	raw2, err := client2.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	impl2 := raw2.(testInterface)
	if got := impl2.Double(21); got != 42 {
		t.Fatalf("bad: %d", got)
	}
	if err := impl2.Bidirectional(); err != nil {
		t.Fatalf("err: %s", err)
	}
}

// Killing the host client while the supervisor is waiting out its backoff
// must return promptly and must not trigger a restart.
func TestClientSupervised_killDuringBackoff(t *testing.T) {
	events := make(chan SupervisorEvent, 32)
	stateFile := filepath.Join(t.TempDir(), "launches")
	c := NewClient(&ClientConfig{
		Cmd:              helperProcess("supervised-grpc", "300", "1", stateFile),
		HandshakeConfig:  testHandshake,
		Plugins:          testGRPCPluginMap,
		AllowedProtocols: []Protocol{ProtocolGRPC},
		Supervisor: &SupervisorConfig{
			InitialBackoff: 30 * time.Second,
			MaxRestarts:    5,
			Notify:         func(ev SupervisorEvent) { events <- ev },
		},
	})
	defer c.Kill()

	if _, err := c.Client(); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Wait for the crash and for the supervisor to enter its backoff.
	waitForSupervisorEvent(t, events, SupervisorEventRestarting, 30*time.Second)

	start := time.Now()
	c.Kill()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Kill took too long while waiting out a backoff: %s", elapsed)
	}

	if !c.Exited() {
		t.Fatal("should say client has exited")
	}
	if gen := c.Generation(); gen != 1 {
		t.Fatalf("unexpected restart after Kill: generation %d", gen)
	}

	// A user-initiated Kill never triggers a restart.
	if _, err := c.Client(); !errors.Is(err, ErrPluginKilled) {
		t.Fatalf("expected ErrPluginKilled, got: %v", err)
	}
	select {
	case ev := <-events:
		if ev.Kind == SupervisorEventRestarted {
			t.Fatal("plugin was restarted after Kill")
		}
	case <-time.After(500 * time.Millisecond):
	}
}

// A supervised client cannot reattach to an existing process.
func TestClientSupervised_reattachError(t *testing.T) {
	c := NewClient(&ClientConfig{
		Cmd:             helperProcess("mock"),
		HandshakeConfig: testHandshake,
		Plugins:         testPluginMap,
		Reattach:        &ReattachConfig{},
		Supervisor:      &SupervisorConfig{},
	})
	defer c.Kill()

	if _, err := c.Start(); !errors.Is(err, ErrSupervisorAndReattach) {
		t.Fatalf("expected ErrSupervisorAndReattach, got: %v", err)
	}
}
