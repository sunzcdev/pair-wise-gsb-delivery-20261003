// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testWindowConfig() ReplicationWindowConfig {
	return ReplicationWindowConfig{
		InitialWindow:    8,
		MinWindow:        2,
		MaxWindow:        16,
		MaxInflightBytes: 1 << 20,
	}
}

func TestAdaptiveWindow_AcquireBlocksAtRequestLimit(t *testing.T) {
	w := newAdaptiveWindow("peer", testWindowConfig())
	gen := w.begin()
	if gen != 1 {
		t.Fatalf("expected first generation to be 1, got %d", gen)
	}

	// Shrink the window to the floor so the limit is small and deterministic.
	for i := 0; i < 10; i++ {
		w.complete(gen, 0, 0, true)
	}
	if d := w.diagnostics(); d.Window != 2 {
		t.Fatalf("expected window to shrink to floor 2, got %d", d.Window)
	}

	if !w.acquire(gen, 0, nil, nil) {
		t.Fatal("first acquire should succeed")
	}
	if !w.acquire(gen, 0, nil, nil) {
		t.Fatal("second acquire should succeed")
	}

	// The window is now full; a third acquire must block.
	abort := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- w.acquire(gen, 0, abort, nil) }()
	select {
	case <-done:
		t.Fatal("acquire should block while the window is full")
	case <-time.After(50 * time.Millisecond):
	}

	// A completion frees a slot and unblocks the waiter.
	w.complete(gen, 0, time.Millisecond, false)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("acquire should have succeeded after a slot freed")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not unblock after a completion")
	}

	// Aborting a waiter returns false.
	abort2 := make(chan struct{})
	go func() { done <- w.acquire(gen, 0, abort2, nil) }()
	time.Sleep(10 * time.Millisecond)
	close(abort2)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("aborted acquire should return false")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not abort")
	}
}

func TestAdaptiveWindow_AcquireBlocksAtByteLimit(t *testing.T) {
	cfg := testWindowConfig()
	cfg.MaxInflightBytes = 100
	w := newAdaptiveWindow("peer", cfg)
	gen := w.begin()

	if !w.acquire(gen, 60, nil, nil) {
		t.Fatal("first acquire should succeed")
	}

	// 60 + 50 exceeds the 100 byte budget, so this must block.
	done := make(chan bool, 1)
	go func() { done <- w.acquire(gen, 50, nil, nil) }()
	select {
	case <-done:
		t.Fatal("acquire should block while the byte budget is exhausted")
	case <-time.After(50 * time.Millisecond):
	}

	w.complete(gen, 60, time.Millisecond, false)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("acquire should have succeeded after bytes were released")
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not unblock after bytes were released")
	}

	// A single request larger than the whole budget is still admitted when
	// nothing else is in flight, so replication can never deadlock.
	w.complete(gen, 50, time.Millisecond, false)
	if !w.acquire(gen, 500, nil, nil) {
		t.Fatal("oversized request should be admitted while nothing is in flight")
	}
	if d := w.diagnostics(); d.InflightBytes != 500 {
		t.Fatalf("expected 500 inflight bytes, got %d", d.InflightBytes)
	}
}

func TestAdaptiveWindow_ShrinkOnSlowdownAndRecover(t *testing.T) {
	w := newAdaptiveWindow("peer", testWindowConfig())
	gen := w.begin()

	// Establish a fast baseline RTT.
	for i := 0; i < 4; i++ {
		w.complete(gen, 0, time.Millisecond, false)
	}
	if d := w.diagnostics(); d.ShrinkEvents != 0 {
		t.Fatalf("steady RTT should not shrink the window: %+v", d)
	}

	// The connection suddenly slows down.
	for i := 0; i < 10; i++ {
		w.complete(gen, 0, 50*time.Millisecond, false)
	}
	d := w.diagnostics()
	if d.Window != 2 {
		t.Fatalf("expected window to shrink to floor 2, got %d", d.Window)
	}
	if d.ShrinkEvents == 0 {
		t.Fatal("expected shrink events to be recorded")
	}
	if d.MinWindowObserved != 2 {
		t.Fatalf("expected min observed window 2, got %d", d.MinWindowObserved)
	}

	// The connection recovers; the window must grow back, capped by MaxWindow.
	for i := 0; i < 500; i++ {
		w.complete(gen, 0, time.Millisecond, false)
	}
	d = w.diagnostics()
	if d.Window != 16 {
		t.Fatalf("expected window to recover to max 16, got %d", d.Window)
	}
	if d.GrowEvents == 0 {
		t.Fatal("expected grow events to be recorded")
	}
	if d.MaxWindowObserved != 16 {
		t.Fatalf("expected max observed window 16, got %d", d.MaxWindowObserved)
	}
}

func TestAdaptiveWindow_ShrinkOnFailure(t *testing.T) {
	w := newAdaptiveWindow("peer", testWindowConfig())
	gen := w.begin()

	w.complete(gen, 0, 0, true)
	if d := w.diagnostics(); d.Window != 4 {
		t.Fatalf("expected window to halve to 4, got %d", d.Window)
	}
	w.complete(gen, 0, 0, true)
	w.complete(gen, 0, 0, true)
	if d := w.diagnostics(); d.Window != 2 {
		t.Fatalf("expected window to bottom out at floor 2, got %d", d.Window)
	}
}

func TestAdaptiveWindow_StaleGenerationIgnored(t *testing.T) {
	w := newAdaptiveWindow("peer", testWindowConfig())
	gen1 := w.begin()

	// Have some in-flight state on the first connection.
	if !w.acquire(gen1, 100, nil, nil) {
		t.Fatal("acquire should succeed")
	}

	// Reconnect: a new generation starts and in-flight accounting resets.
	gen2 := w.begin()
	if gen2 != gen1+1 {
		t.Fatalf("expected generation to increment, got %d -> %d", gen1, gen2)
	}
	before := w.diagnostics()
	if before.Inflight != 0 || before.InflightBytes != 0 {
		t.Fatalf("in-flight state should reset on reconnect: %+v", before)
	}

	// Late completions from the old connection must not adjust the new
	// window, neither by shrinking it nor by releasing phantom slots.
	w.complete(gen1, 100, 0, true)
	w.complete(gen1, 100, time.Hour, false)
	after := w.diagnostics()
	if after.Window != before.Window {
		t.Fatalf("stale completions adjusted the window: before=%d after=%d", before.Window, after.Window)
	}
	if after.StaleCompletions != 2 {
		t.Fatalf("expected 2 stale completions, got %d", after.StaleCompletions)
	}
	if after.Inflight != 0 || after.InflightBytes != 0 {
		t.Fatalf("stale completions changed in-flight accounting: %+v", after)
	}

	// The old generation can no longer acquire slots either.
	if w.acquire(gen1, 0, nil, nil) {
		t.Fatal("acquire with a stale generation should fail")
	}
}

func TestAdaptiveWindow_Diagnostics(t *testing.T) {
	w := newAdaptiveWindow("peer", testWindowConfig())
	gen := w.begin()
	if !w.acquire(gen, 64, nil, nil) {
		t.Fatal("acquire should succeed")
	}
	w.complete(gen, 64, 3*time.Millisecond, false)

	d := w.diagnostics()
	if d.Generation != gen {
		t.Fatalf("expected generation %d, got %d", gen, d.Generation)
	}
	if d.LastRTT != 3*time.Millisecond {
		t.Fatalf("expected last RTT 3ms, got %s", d.LastRTT)
	}
	if d.MinRTT != 3*time.Millisecond {
		t.Fatalf("expected min RTT 3ms, got %s", d.MinRTT)
	}
	if d.Inflight != 0 || d.InflightBytes != 0 {
		t.Fatalf("expected no in-flight state, got %+v", d)
	}
}

func TestValidateConfig_ReplicationWindow(t *testing.T) {
	conf := inmemConfig(t)
	conf.LocalID = "server-1"
	conf.ReplicationWindow = &ReplicationWindowConfig{InitialWindow: 4, MinWindow: 8}
	if err := ValidateConfig(conf); err == nil {
		t.Fatal("expected error when MinWindow exceeds InitialWindow")
	}
	conf.ReplicationWindow = &ReplicationWindowConfig{InitialWindow: 8, MaxWindow: 4}
	if err := ValidateConfig(conf); err == nil {
		t.Fatal("expected error when MaxWindow is smaller than InitialWindow")
	}
	conf.ReplicationWindow = &ReplicationWindowConfig{}
	if err := ValidateConfig(conf); err != nil {
		t.Fatalf("defaulted window config should validate: %v", err)
	}
}

// netemTransport is a transport double that wraps a LoopbackTransport and
// applies controllable latency, loss, and sudden slowdowns to pipelined
// AppendEntries traffic on a per-peer basis. Non-pipelined RPCs (heartbeats,
// standard AppendEntries, votes) are passed through untouched so the cluster
// itself remains stable while a single replication connection is impaired.
type netemTransport struct {
	LoopbackTransport

	mu    sync.Mutex
	peers map[ServerAddress]*netemSettings
}

type netemSettings struct {
	mu    sync.Mutex
	delay time.Duration
	drop  bool
}

func (s *netemSettings) get() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delay, s.drop
}

func newNetemTransport(inner LoopbackTransport) *netemTransport {
	return &netemTransport{
		LoopbackTransport: inner,
		peers:             make(map[ServerAddress]*netemSettings),
	}
}

// settingsFor returns the settings for a peer, creating them on first use so
// that pipelines created before a test impairs a peer still observe later
// changes.
func (t *netemTransport) settingsFor(addr ServerAddress) *netemSettings {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.peers[addr]
	if !ok {
		s = &netemSettings{}
		t.peers[addr] = s
	}
	return s
}

// impair sets the latency and loss behavior for pipelined traffic to a peer.
// It may be called at any time to simulate sudden changes in link quality.
func (t *netemTransport) impair(addr ServerAddress, delay time.Duration, drop bool) {
	s := t.settingsFor(addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = delay
	s.drop = drop
}

// Connect unwraps the double so the underlying in-memory transports can be
// wired together by the cluster harness.
func (t *netemTransport) Connect(peer ServerAddress, trans Transport) {
	t.LoopbackTransport.Connect(peer, trans.(*netemTransport).LoopbackTransport)
}

func (t *netemTransport) AppendEntriesPipeline(id ServerID, target ServerAddress) (AppendPipeline, error) {
	inner, err := t.LoopbackTransport.AppendEntriesPipeline(id, target)
	if err != nil {
		return nil, err
	}
	return newNetemPipeline(inner, t.settingsFor(target)), nil
}

// errNetemLoss is reported on futures whose response is dropped by the double.
var errNetemLoss = errors.New("netem: response lost")

// lostFuture wraps a completed future and reports it as lost, mimicking a
// response that never made it back to the leader.
type lostFuture struct {
	AppendFuture
}

func (lostFuture) Error() error { return errNetemLoss }

type netemPipeline struct {
	inner      AppendPipeline
	settings   *netemSettings
	doneCh     chan AppendFuture
	shutdownCh chan struct{}
	closeOnce  sync.Once
}

func newNetemPipeline(inner AppendPipeline, settings *netemSettings) *netemPipeline {
	p := &netemPipeline{
		inner:      inner,
		settings:   settings,
		doneCh:     make(chan AppendFuture, 64),
		shutdownCh: make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *netemPipeline) run() {
	innerCh := p.inner.Consumer()
	for {
		select {
		case f := <-innerCh:
			delay, drop := p.settings.get()
			if drop {
				f = lostFuture{AppendFuture: f}
			}
			p.forward(f, delay)
		case <-p.shutdownCh:
			return
		}
	}
}

func (p *netemPipeline) forward(f AppendFuture, delay time.Duration) {
	if delay <= 0 {
		select {
		case p.doneCh <- f:
		case <-p.shutdownCh:
		}
		return
	}
	time.AfterFunc(delay, func() {
		select {
		case p.doneCh <- f:
		case <-p.shutdownCh:
		}
	})
}

func (p *netemPipeline) AppendEntries(args *AppendEntriesRequest, resp *AppendEntriesResponse) (AppendFuture, error) {
	return p.inner.AppendEntries(args, resp)
}

func (p *netemPipeline) Consumer() <-chan AppendFuture {
	return p.doneCh
}

func (p *netemPipeline) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.shutdownCh)
		err = p.inner.Close()
	})
	return err
}

// makeWindowedCluster builds a 3 node cluster with the adaptive replication
// window enabled and every transport wrapped in a netem double, returning the
// cluster and the doubles indexed like c.rafts.
func makeWindowedCluster(t *testing.T, cfg ReplicationWindowConfig) (*cluster, []*netemTransport) {
	t.Helper()
	conf := inmemConfig(t)
	conf.ReplicationWindow = &cfg
	var transports []*netemTransport
	c := MakeClusterCustom(t, &MakeClusterOpts{
		Peers:     3,
		Bootstrap: true,
		Conf:      conf,
		WrapTransport: func(lt LoopbackTransport) LoopbackTransport {
			nt := newNetemTransport(lt)
			transports = append(transports, nt)
			return nt
		},
	})
	return c, transports
}

// waitForWindowDiag polls the leader's replication window diagnostics for a
// follower until the condition holds or the timeout expires.
func waitForWindowDiag(t *testing.T, leader *Raft, id ServerID, timeout time.Duration, ok func(WindowDiagnostics) bool) WindowDiagnostics {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, exists := leader.ReplicationWindowDiagnostics()[id]
		if exists && ok(d) {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	d := leader.ReplicationWindowDiagnostics()[id]
	t.Fatalf("timed out waiting for window condition on %s; last diagnostics: %+v", id, d)
	return d
}

func applyLogs(t *testing.T, leader *Raft, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := leader.Apply([]byte(fmt.Sprintf("%s-%d", prefix, i)), 0).Error(); err != nil {
			t.Fatalf("apply failed: %v", err)
		}
	}
}

// TestRaft_ReplicationWindow_SlowdownAndRecovery verifies that when one
// follower's replication connection suddenly slows down, only that follower's
// window shrinks, and that it grows back once the connection recovers.
func TestRaft_ReplicationWindow_SlowdownAndRecovery(t *testing.T) {
	c, transports := makeWindowedCluster(t, testWindowConfig())
	defer c.Close()

	leader := c.Leader()
	followers := c.Followers()
	if len(followers) != 2 {
		t.Fatalf("expected 2 followers, got %d", len(followers))
	}
	slow, fast := followers[0], followers[1]
	leaderTrans := transports[c.IndexOf(leader)]

	// Warm up so pipelines are established and windows are in use.
	applyLogs(t, leader, "warm", 8)
	c.WaitForReplication(8)

	// Wait for the pipeline to be established and a fast RTT baseline to be
	// recorded, so the slowdown below is measured against it.
	waitForWindowDiag(t, leader, slow.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Generation >= 1 && d.MinRTT > 0 && d.MinRTT < 5*time.Millisecond
	})

	// Suddenly slow down one follower's connection.
	leaderTrans.impair(slow.localAddr, 30*time.Millisecond, false)

	applyLogs(t, leader, "slow", 32)

	// The slow follower's window must shrink in response to the latency.
	slowDiag := waitForWindowDiag(t, leader, slow.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.ShrinkEvents > 0 && d.Window < 8
	})

	// The fast follower must not be dragged down by the slow one: its window
	// stays at or above the initial size and it keeps up with the leader.
	fastDiag := waitForWindowDiag(t, leader, fast.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Window >= 8
	})
	if fastDiag.Window <= slowDiag.Window {
		t.Fatalf("fast follower window (%d) should exceed slow follower window (%d)",
			fastDiag.Window, slowDiag.Window)
	}
	if fastDiag.ShrinkEvents > slowDiag.ShrinkEvents {
		t.Fatalf("fast follower shrank more (%d) than the slow follower (%d)",
			fastDiag.ShrinkEvents, slowDiag.ShrinkEvents)
	}
	fastFSM := getMockFSM(c.fsms[c.IndexOf(fast)])
	deadline := time.Now().Add(5 * time.Second)
	for len(fastFSM.Logs()) < 40 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(fastFSM.Logs()); got != 40 {
		t.Fatalf("fast follower was dragged down by slow follower: applied %d of 40 logs", got)
	}

	// The connection recovers; the window must grow back.
	leaderTrans.impair(slow.localAddr, 0, false)
	applyLogs(t, leader, "recover", 32)
	recovered := waitForWindowDiag(t, leader, slow.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Window >= 8 && d.GrowEvents > slowDiag.GrowEvents
	})
	t.Logf("slow follower window recovered: before=%+v after=%+v", slowDiag, recovered)

	// Commit semantics are preserved: every node ends up with every log.
	c.WaitForReplication(72)
	c.EnsureSame(t)
}

// TestRaft_ReplicationWindow_LossReconnect verifies that when pipelined
// responses are lost, the leader falls back to standard replication (keeping
// commit semantics), reconnects with a fresh window generation, and that
// feedback from the dead connection does not corrupt the new window.
func TestRaft_ReplicationWindow_LossReconnect(t *testing.T) {
	c, transports := makeWindowedCluster(t, testWindowConfig())
	defer c.Close()

	leader := c.Leader()
	followers := c.Followers()
	if len(followers) != 2 {
		t.Fatalf("expected 2 followers, got %d", len(followers))
	}
	lossy := followers[0]
	leaderTrans := transports[c.IndexOf(leader)]

	applyLogs(t, leader, "warm", 4)
	c.WaitForReplication(4)

	gen0 := waitForWindowDiag(t, leader, lossy.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Generation >= 1
	}).Generation

	// Start dropping all pipelined responses to the follower.
	leaderTrans.impair(lossy.localAddr, 0, true)

	// Logs must still commit: the leader falls back to standard replication
	// and the healthy follower forms a quorum.
	applyLogs(t, leader, "loss", 16)
	c.WaitForReplication(20)

	// Each loss aborts the pipeline and forces a reconnect with a fresh
	// generation; failures shrink the window.
	lossyDiag := waitForWindowDiag(t, leader, lossy.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Generation > gen0 && d.ShrinkEvents > 0
	})
	t.Logf("lossy follower window after loss: %+v", lossyDiag)

	// The link recovers; the window must grow again on the new connection.
	leaderTrans.impair(lossy.localAddr, 0, false)
	applyLogs(t, leader, "heal", 16)
	waitForWindowDiag(t, leader, lossy.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Window >= 8 && d.GrowEvents > lossyDiag.GrowEvents
	})

	c.WaitForReplication(36)
	c.EnsureSame(t)
}

// TestRaft_ReplicationWindow_BehindFollower verifies that the conflict
// fallback semantics are preserved with the window enabled: a follower that
// fell behind rejects AppendEntries until nextIndex is backed up, then
// catches up and resumes pipelining.
func TestRaft_ReplicationWindow_BehindFollower(t *testing.T) {
	c, _ := makeWindowedCluster(t, testWindowConfig())
	defer c.Close()

	leader := c.Leader()
	followers := c.Followers()
	behind := followers[0]
	c.Disconnect(behind.localAddr)

	// Commit entries the partitioned follower never sees.
	applyLogs(t, leader, "behind", 32)

	// Reconnect; the follower's log conflicts with the leader's nextIndex,
	// so AppendEntries is rejected until the leader backs up far enough.
	c.FullyConnect()

	c.WaitForReplication(32)
	c.EnsureSame(t)

	leader = c.Leader()
	c.EnsureLeader(t, leader.localAddr)
	waitForWindowDiag(t, leader, behind.localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Generation >= 1
	})
}

// TestRaft_ReplicationWindow_ByteLimit verifies that the in-flight byte
// budget is enforced without deadlocking replication, even when a single log
// batch is larger than the budget.
func TestRaft_ReplicationWindow_ByteLimit(t *testing.T) {
	cfg := testWindowConfig()
	cfg.MaxInflightBytes = 1500
	c, _ := makeWindowedCluster(t, cfg)
	defer c.Close()

	leader := c.Leader()

	// Each 1KB log nearly exhausts the 1.5KB budget, so requests are
	// effectively serialized by the byte limit.
	for i := 0; i < 8; i++ {
		if err := leader.Apply(make([]byte, 1000), 0).Error(); err != nil {
			t.Fatalf("apply failed: %v", err)
		}
	}
	// A single oversized batch must still go through on an empty window.
	if err := leader.Apply(make([]byte, 3000), 0).Error(); err != nil {
		t.Fatalf("oversized apply failed: %v", err)
	}

	c.WaitForReplication(9)
	c.EnsureSame(t)

	followers := c.Followers()
	diag := waitForWindowDiag(t, leader, followers[0].localID, 5*time.Second, func(d WindowDiagnostics) bool {
		return d.Generation >= 1
	})
	if diag.InflightBytes > 1500 && diag.Inflight > 1 {
		t.Fatalf("byte budget exceeded with multiple requests in flight: %+v", diag)
	}
}
