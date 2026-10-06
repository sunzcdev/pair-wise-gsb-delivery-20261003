// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
)

// errCorruptStorage is a fictional unrecoverable FSM error that implements the
// optional FSMApplyError interface.
type errCorruptStorage struct {
	index uint64
}

func (e *errCorruptStorage) Error() string {
	return fmt.Sprintf("corrupt local storage while applying index %d", e.index)
}

// FSMApplyError marks this as an unrecoverable apply failure.
func (e *errCorruptStorage) FSMApplyError() {}

// errBusinessReject is an ordinary, recoverable business error. It must NOT be
// treated as an unrecoverable FSM failure.
type errBusinessReject struct{}

func (e *errBusinessReject) Error() string { return "business rule rejected the command" }

var _ FSMApplyError = (*errCorruptStorage)(nil)

// failingFSM is an FSM that can be programmed to return an unrecoverable
// FSMApplyError at a specific log index. It records every index it actually
// applied so tests can assert that no entry at or after the failure reached
// the state machine.
type failingFSM struct {
	mu sync.Mutex

	// failAt, when > 0, is the command index at which Apply returns an
	// unrecoverable error.
	failAt uint64

	// businessErrorAt, when > 0, returns a plain business error at the index.
	businessErrorAt uint64

	applied   [][]byte
	restores  int
	snapshots int
}

func (f *failingFSM) snapshotCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots
}

type failingSnapshot struct {
	logs [][]byte
}

func (f *failingFSM) Apply(log *Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failAt > 0 && log.Index == f.failAt {
		return &errCorruptStorage{index: log.Index}
	}
	if f.businessErrorAt > 0 && log.Index == f.businessErrorAt {
		return &errBusinessReject{}
	}

	f.applied = append(f.applied, log.Data)
	return len(f.applied)
}

func (f *failingFSM) Snapshot() (FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots++
	logs := make([][]byte, len(f.applied))
	copy(logs, f.applied)
	return &failingSnapshot{logs: logs}, nil
}

func (f *failingFSM) Restore(inp io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() { _ = inp.Close() }()
	hd := codec.MsgpackHandle{}
	dec := codec.NewDecoder(inp, &hd)
	if err := dec.Decode(&f.applied); err != nil {
		return err
	}
	f.restores++
	return nil
}

func (f *failingFSM) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

func (s *failingSnapshot) Persist(sink SnapshotSink) error {
	hd := codec.MsgpackHandle{}
	enc := codec.NewEncoder(sink, &hd)
	if err := enc.Encode(s.logs); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *failingSnapshot) Release() {}

func newFailingCluster(t *testing.T, fsm *failingFSM) *cluster {
	t.Helper()
	return MakeClusterCustom(t, &MakeClusterOpts{
		Peers:     1,
		Bootstrap: true,
		Conf:      inmemConfig(t),
		MakeFSMFunc: func() FSM {
			return fsm
		},
	})
}

// TestFSM_ApplyError_StopsAtN verifies the core contract: after an
// unrecoverable error at index N, entry N+1 is committed but never applied to
// the state machine, the node enters Failed, and the client can tell the
// committed-but-failed entry apart from later applies.
func TestFSM_ApplyError_StopsAtN(t *testing.T) {
	fsm := &failingFSM{}
	c := newFailingCluster(t, fsm)
	defer c.Close()

	leader := c.Leader()

	// Commit a few healthy entries. The first entry on a freshly bootstrapped
	// single node is the leader no-op; discover the current applied index.
	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("a"), time.Second) })
	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("b"), time.Second) })

	healthyCount := fsm.appliedCount()
	failIndex := leader.AppliedIndex() + 1
	fsm.failAt = failIndex

	// Entry N fails to apply even though it is committed.
	failFuture := leader.Apply([]byte("boom"), time.Second)
	err := failFuture.Error()
	if !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected ErrFSMFailed for failing entry, got: %v", err)
	}
	var fatalErr *errCorruptStorage
	if !errors.As(err, &fatalErr) {
		t.Fatalf("expected errors.As to recover the FSMApplyError, got: %v", err)
	}
	if failFuture.Index() != failIndex {
		t.Fatalf("expected failing index %d, got %d", failIndex, failFuture.Index())
	}

	// The node should be failed and expose the error.
	waitForState(leader, Failed)
	if leader.State() != Failed {
		t.Fatalf("expected Failed, got %v", leader.State())
	}
	if !leader.FSMFailed() || leader.FSMError() == nil {
		t.Fatalf("expected FSMFailed() true and a non-nil FSMError")
	}

	// Entry N+1 may be committed/replicated but must never touch the FSM.
	after := leader.Apply([]byte("after"), time.Second)
	if err := after.Error(); !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected N+1 future to fail with ErrFSMFailed, got: %v", err)
	}

	// Give the FSM goroutine a moment, then assert it applied exactly the
	// healthy entries and never "boom" or "after".
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fsm.appliedCount() == int(failIndex-1) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := fsm.appliedCount(); got != healthyCount {
		t.Fatalf("expected applied entries to stay at %d (no entry at/after N), got %d", healthyCount, got)
	}
}

// TestFSM_BusinessErrorNotFatal verifies that ordinary errors returned via
// ApplyFuture.Response do not stop the state machine.
func TestFSM_BusinessErrorNotFatal(t *testing.T) {
	fsm := &failingFSM{}
	c := newFailingCluster(t, fsm)
	defer c.Close()
	leader := c.Leader()

	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("ok"), time.Second) })
	badIndex := leader.AppliedIndex() + 1
	fsm.businessErrorAt = badIndex

	fut := leader.Apply([]byte("reject"), time.Second)
	if err := fut.Error(); err != nil {
		t.Fatalf("business error must not be returned by Error(): %v", err)
	}
	resp, ok := fut.Response().(error)
	if !ok || !errors.Is(resp, &errBusinessReject{}) {
		t.Fatalf("expected business error in Response, got: %#v", fut.Response())
	}

	// The node keeps applying subsequent logs normally.
	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("still-ok"), time.Second) })
	if leader.FSMFailed() {
		t.Fatalf("a plain business error must not fail the FSM")
	}
}

// TestFSM_ApplyError_BarrierSnapshotAndShutdown verifies that after failure a
// Barrier never completes successfully, snapshots are refused (and the FSM
// Snapshot() is not invoked), and Shutdown still tears the node down cleanly.
func TestFSM_ApplyError_BarrierSnapshotAndShutdown(t *testing.T) {
	fsm := &failingFSM{}
	c := newFailingCluster(t, fsm)
	defer c.Close()
	leader := c.Leader()

	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("a"), time.Second) })
	fsm.failAt = leader.AppliedIndex() + 1

	if err := leader.Apply([]byte("boom"), time.Second).Error(); !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected failure, got %v", err)
	}
	waitForState(leader, Failed)
	if leader.State() != Failed {
		t.Fatalf("expected Failed, got %v", leader.State())
	}

	// A barrier must not succeed and must not be swallowed by advancing the
	// applied index.
	if err := leader.Barrier(time.Second).Error(); !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected barrier to fail with ErrFSMFailed, got %v", err)
	}

	// A user snapshot request is refused; the failed FSM is never snapshotted.
	snapsBefore := fsm.snapshotCalls()
	if err := leader.Snapshot().Error(); !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected snapshot to fail with ErrFSMFailed, got %v", err)
	}
	if fsm.snapshotCalls() != snapsBefore {
		t.Fatalf("Snapshot() must not be invoked on a failed FSM")
	}

	// Restore is refused as well: no implicit in-process recovery.
	if err := leader.Restore(nil, nil, time.Second); !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected restore to fail with ErrFSMFailed, got %v", err)
	}

	// AppliedIndex must remain at the last healthy entry, not advance.
	if got := leader.AppliedIndex(); got != fsm.failAt-1 {
		t.Fatalf("expected applied index to stay at %d, got %d", fsm.failAt-1, got)
	}

	// Shutdown must still terminate all goroutines cleanly.
	shutdown := leader.Shutdown()
	done := make(chan error, 1)
	go func() { done <- shutdown.Error() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for shutdown of failed node")
	}
	if state := leader.State(); state != Shutdown {
		t.Fatalf("expected Shutdown state, got %v", state)
	}
}

// TestFSM_ApplyError_Batching verifies that a failure inside ApplyBatch stops
// at the failing entry and does not apply later entries in that batch.
func TestFSM_ApplyError_Batching(t *testing.T) {
	fsm := &batchingFailingFSM{failingFSM: &failingFSM{}}
	c := MakeClusterCustom(t, &MakeClusterOpts{
		Peers:       1,
		Bootstrap:   true,
		Conf:        inmemConfig(t),
		MakeFSMFunc: func() FSM { return fsm },
	})
	defer c.Close()
	leader := c.Leader()

	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("a"), time.Second) })
	healthyCount := fsm.appliedCount()
	failAt := leader.AppliedIndex() + 1
	fsm.failAt = failAt

	// Fire several entries that should land in one batch.
	n := 5
	futs := make([]ApplyFuture, n)
	for i := 0; i < n; i++ {
		futs[i] = leader.Apply([]byte(fmt.Sprintf("cmd-%d", i)), 0)
	}

	sawFailure := false
	for _, f := range futs {
		if errors.Is(f.Error(), ErrFSMFailed) {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatalf("expected at least one batched future to report ErrFSMFailed")
	}
	waitForState(leader, Failed)
	if leader.State() != Failed {
		t.Fatalf("expected Failed, got %v", leader.State())
	}

	if got := fsm.appliedCount(); got != healthyCount {
		t.Fatalf("expected applied entries to stay at %d, got %d", healthyCount, got)
	}
}

type batchingFailingFSM struct {
	*failingFSM
}

// ApplyBatch honors the apply contract: once an entry fails unrecoverably, no
// later entry in the batch mutates the state machine; those later responses
// carry the same fatal error.
func (b *batchingFailingFSM) ApplyBatch(logs []*Log) []interface{} {
	resps := make([]interface{}, len(logs))
	var fatal FSMApplyError
	for i, l := range logs {
		if fatal != nil {
			resps[i] = fatal
			continue
		}
		resp := b.Apply(l)
		if f, ok := resp.(FSMApplyError); ok {
			fatal = f
		}
		resps[i] = resp
	}
	return resps
}

func waitForApplied(t *testing.T, r *Raft, apply func() ApplyFuture) {
	t.Helper()
	f := apply()
	if err := f.Error(); err != nil {
		t.Fatalf("unexpected apply error: %v", err)
	}
}

// TestFSM_ApplyError_Cluster verifies behavior in a multi-node cluster: when
// the leader's FSM fails, the leader steps down into Failed, never campaigns
// again, and one of the healthy followers takes over leadership.
func TestFSM_ApplyError_Cluster(t *testing.T) {
	fsms := []*failingFSM{{}, {}, {}}
	idx := 0
	c := MakeClusterCustom(t, &MakeClusterOpts{
		Peers:     3,
		Bootstrap: true,
		Conf:      inmemConfig(t),
		MakeFSMFunc: func() FSM {
			f := fsms[idx]
			idx++
			return f
		},
	})
	defer c.Close()

	leader := c.Leader()
	leaderIdx := -1
	for i, rr := range c.rafts {
		if rr == leader {
			leaderIdx = i
		}
	}
	if leaderIdx < 0 {
		t.Fatalf("could not locate leader index")
	}

	// Commit something healthy on all nodes.
	waitForApplied(t, leader, func() ApplyFuture { return leader.Apply([]byte("warmup"), time.Second) })

	// Make the leader's FSM fail at the next committed entry.
	fsms[leaderIdx].failAt = leader.AppliedIndex() + 1
	if err := leader.Apply([]byte("leader-boom"), time.Second).Error(); !errors.Is(err, ErrFSMFailed) {
		t.Fatalf("expected leader apply to fail with ErrFSMFailed, got %v", err)
	}

	// The failed leader must reach Failed and stay there.
	waitForState(leader, Failed)
	if leader.State() != Failed {
		t.Fatalf("expected failed leader to be in Failed state, got %v", leader.State())
	}

	// A healthy follower should elect a new leader that is not the failed
	// node. Wait for a distinct leader to appear.
	deadline := time.Now().Add(5 * time.Second)
	var newLeader *Raft
	for time.Now().Before(deadline) {
		leaders := c.GetInState(Leader)
		for _, l := range leaders {
			if l != leader {
				newLeader = l
			}
		}
		if newLeader != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if newLeader == nil {
		t.Fatalf("expected a healthy node to become leader after leader FSM failure")
	}

	// The new leader keeps serving writes; the failed node never re-enters
	// leadership and never applies the offending entry.
	if err := newLeader.Apply([]byte("after-failover"), time.Second).Error(); err != nil {
		t.Fatalf("writes on the new leader should succeed, got %v", err)
	}
	if leader.State() != Failed {
		t.Fatalf("failed node must not campaign again, state=%v", leader.State())
	}
}
