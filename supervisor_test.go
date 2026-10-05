// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	grpctest "github.com/hashicorp/go-plugin/test/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// supervisorClientFactory returns a factory that creates a fresh Client per
// generation and records every created client so tests can inspect or kill
// individual generations.
func supervisorClientFactory(processArgs []string, clients *[]*Client, mu *sync.Mutex) (func() (*Client, error), func() int) {
	factory := func() (*Client, error) {
		c := NewClient(&ClientConfig{
			Cmd:              helperProcess(processArgs...),
			HandshakeConfig:  testHandshake,
			Plugins:          testGRPCPluginMap,
			AllowedProtocols: []Protocol{ProtocolGRPC},
		})
		mu.Lock()
		*clients = append(*clients, c)
		mu.Unlock()
		return c, nil
	}
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(*clients)
	}
	return factory, calls
}

func waitForCondition(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

// crashGeneration forcefully kills the OS process of the given generation
// without any graceful shutdown, simulating a plugin crash.
func crashGeneration(t *testing.T, c *Client) {
	t.Helper()
	if c.config.Cmd == nil || c.config.Cmd.Process == nil {
		t.Fatal("generation has no running process to kill")
	}
	if err := c.config.Cmd.Process.Kill(); err != nil {
		t.Fatalf("failed to kill plugin process: %s", err)
	}
}

func TestSupervisor_consecutiveStartFailures(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, calls := supervisorClientFactory([]string{"invalid-rpc-address"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		MaxRestarts:    3,
	})
	s.Start()

	select {
	case <-s.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("supervisor did not stop after exhausting restarts")
	}

	if s.State() != SupervisorStateStopped {
		t.Fatalf("expected stopped state, got %s", s.State())
	}
	if !errors.Is(s.Err(), ErrSupervisorStopped) {
		t.Fatalf("expected ErrSupervisorStopped, got %v", s.Err())
	}
	// 1 initial attempt + 3 restarts.
	if got := calls(); got != 4 {
		t.Fatalf("expected 4 start attempts, got %d", got)
	}
	if got := s.Generation(); got != 0 {
		t.Fatalf("expected no successful generation, got %d", got)
	}

	if _, err := s.Client(context.Background()); !errors.Is(err, ErrSupervisorStopped) {
		t.Fatalf("expected Client to fail with ErrSupervisorStopped, got %v", err)
	}
}

func TestSupervisor_restartBudgetExhausted(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, calls := supervisorClientFactory([]string{"invalid-rpc-address"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		InitialBackoff: 40 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		// No MaxRestarts: only the time budget may stop the supervisor.
		Budget: 150 * time.Millisecond,
	})
	start := time.Now()
	s.Start()

	select {
	case <-s.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("supervisor did not stop after exhausting its budget")
	}

	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("supervisor gave up before its budget elapsed: %s", elapsed)
	}
	if !errors.Is(s.Err(), ErrSupervisorStopped) {
		t.Fatalf("expected ErrSupervisorStopped, got %v", s.Err())
	}
	if got := calls(); got < 2 {
		t.Fatalf("expected multiple start attempts, got %d", got)
	}
}

func TestSupervisor_crashAfterHandshake(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, _ := supervisorClientFactory([]string{"test-grpc"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	defer s.Kill()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gen1, err := s.Client(ctx)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := gen1.Ping(); err != nil {
		t.Fatalf("err: %s", err)
	}
	if got := s.Generation(); got != 1 {
		t.Fatalf("expected generation 1, got %d", got)
	}

	// Dispense an implementation from generation 1.
	raw, err := gen1.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	gen1Impl := raw.(*testGRPCClient)

	// Crash the first generation.
	mu.Lock()
	gen1Client := clients[0]
	mu.Unlock()
	crashGeneration(t, gen1Client)

	// The supervisor must bring up a new, independent generation.
	waitForCondition(t, 30*time.Second, "generation 2 to start", func() bool {
		return s.Generation() == 2
	})

	gen2, err := s.Client(ctx)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := gen2.Ping(); err != nil {
		t.Fatalf("generation 2 should be healthy: %s", err)
	}

	// The old generation's client must not silently talk to the new process.
	if err := gen1.Ping(); err == nil {
		t.Fatal("old generation client should not reach the new process")
	}

	// Neither should an implementation dispensed from the old generation.
	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	_, err = gen1Impl.Client.Double(callCtx, &grpctest.TestRequest{Input: 2})
	if err == nil {
		t.Fatal("call on old generation should fail")
	}
	// The failure must be an identifiable transport error: Unavailable
	// while the dead process's connection is being torn down, or Canceled
	// once the supervisor has reaped the generation and closed its client
	// connection.
	if code := status.Code(err); code != codes.Unavailable && code != codes.Canceled {
		t.Fatalf("expected Unavailable or Canceled, got %v", err)
	}
}

func TestSupervisor_stableRunResetsRestartPolicy(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, _ := supervisorClientFactory([]string{"test-grpc"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		// Only a single restart is allowed per run of failures...
		MaxRestarts: 1,
		// ...but a generation that stays healthy for a while resets the
		// policy, so a later crash may be restarted again.
		StableAfter: 300 * time.Millisecond,
	})
	defer s.Kill()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Generation 1 crashes immediately: the single allowed restart is spent.
	if _, err := s.Client(ctx); err != nil {
		t.Fatalf("err: %s", err)
	}
	waitForCondition(t, 30*time.Second, "generation 1 to exist", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(clients) >= 1
	})
	mu.Lock()
	gen1 := clients[0]
	mu.Unlock()
	crashGeneration(t, gen1)

	waitForCondition(t, 30*time.Second, "generation 2 to start", func() bool {
		return s.Generation() == 2
	})

	// Generation 2 stays healthy past StableAfter, then crashes. If the
	// restart policy were not reset, the supervisor would give up here
	// because MaxRestarts was already spent on generation 1.
	time.Sleep(400 * time.Millisecond)
	waitForCondition(t, 30*time.Second, "generation 2 client to exist", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(clients) >= 2
	})
	mu.Lock()
	gen2 := clients[1]
	mu.Unlock()
	crashGeneration(t, gen2)

	waitForCondition(t, 30*time.Second, "generation 3 to start", func() bool {
		return s.Generation() == 3
	})

	gen3, err := s.Client(ctx)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := gen3.Ping(); err != nil {
		t.Fatalf("generation 3 should be healthy: %s", err)
	}
}

func TestSupervisor_killDuringBackoff(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, calls := supervisorClientFactory([]string{"test-grpc"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		// A backoff long enough that the test would time out if Kill did
		// not interrupt it.
		InitialBackoff: 1 * time.Minute,
		MaxBackoff:     1 * time.Minute,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.Client(ctx); err != nil {
		t.Fatalf("err: %s", err)
	}
	mu.Lock()
	gen1 := clients[0]
	mu.Unlock()
	crashGeneration(t, gen1)

	waitForCondition(t, 30*time.Second, "supervisor to enter backoff", func() bool {
		return s.State() == SupervisorStateBackoff
	})

	// Shutting down the host while the supervisor is waiting out the
	// backoff must stop it promptly and prevent the restart.
	killed := make(chan struct{})
	go func() {
		s.Kill()
		close(killed)
	}()
	select {
	case <-killed:
	case <-time.After(10 * time.Second):
		t.Fatal("Kill did not return promptly during backoff")
	}

	select {
	case <-s.Done():
	default:
		t.Fatal("supervisor should be stopped")
	}
	if s.State() != SupervisorStateStopped {
		t.Fatalf("expected stopped state, got %s", s.State())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("user-initiated Kill should not record an error, got %v", err)
	}
	// Give the (never to happen) restart a chance to prove it doesn't.
	time.Sleep(100 * time.Millisecond)
	if got := calls(); got != 1 {
		t.Fatalf("expected no restart after Kill, got %d start attempts", got)
	}
}

func TestSupervisor_killRunningGeneration(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, calls := supervisorClientFactory([]string{"test-grpc"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.Client(ctx); err != nil {
		t.Fatalf("err: %s", err)
	}
	mu.Lock()
	gen1 := clients[0]
	mu.Unlock()

	// A user-initiated Kill stops the plugin and must not trigger a
	// restart.
	s.Kill()

	if !gen1.Exited() {
		t.Fatal("plugin process should have exited after Kill")
	}
	if err := s.Err(); err != nil {
		t.Fatalf("user-initiated Kill should not record an error, got %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := calls(); got != 1 {
		t.Fatalf("expected no restart after Kill, got %d start attempts", got)
	}
}

func TestSupervisor_inflightCallFailsIdentifiably(t *testing.T) {
	var mu sync.Mutex
	var clients []*Client
	factory, _ := supervisorClientFactory([]string{"test-grpc"}, &clients, &mu)

	s := NewSupervisor(factory, SupervisorConfig{
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	defer s.Kill()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gen1, err := s.Client(ctx)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	raw, err := gen1.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	impl := raw.(*testGRPCClient)

	// Open a bidi stream and confirm it works.
	stream, err := impl.Client.Stream(context.Background())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := stream.Send(&grpctest.TestRequest{Input: 1}); err != nil {
		t.Fatalf("err: %s", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Crash the plugin while the stream is open.
	generation := s.Generation()
	mu.Lock()
	gen1Client := clients[0]
	mu.Unlock()
	crashGeneration(t, gen1Client)

	// The in-flight call must fail with an identifiable transport error,
	// not hang and not be silently served by the next generation.
	if _, err := stream.Recv(); err == nil {
		t.Fatal("in-flight call should fail after the crash")
	} else if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}

	// The business can detect the restart via the generation and decide to
	// retry against a freshly dispensed implementation.
	waitForCondition(t, 30*time.Second, "generation to change", func() bool {
		return s.Generation() != generation
	})
	gen2, err := s.Client(ctx)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	raw, err = gen2.Dispense("test")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	impl = raw.(*testGRPCClient)
	resp, err := impl.Client.Double(ctx, &grpctest.TestRequest{Input: 21})
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if resp.Output != 42 {
		t.Fatalf("bad: %d", resp.Output)
	}
}
