// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errFatalFSM is a sentinel FSM error that also implements FatalError.
type errFatalFSM struct {
	index uint64
}

func (e *errFatalFSM) Error() string {
	return fmt.Sprintf("simulated unrecoverable storage corruption at index %d", e.index)
}

func (e *errFatalFSM) FatalError() bool { return true }

// fatalMockFSM wraps MockFSM and fails Apply at a configurable absolute raft
// index. failAt of 0 disables failure. It implements FatalFSM.
type fatalMockFSM struct {
	*MockFSM
	failAt atomic.Uint64
}

func newFatalMockFSM() *fatalMockFSM {
	return &fatalMockFSM{MockFSM: &MockFSM{}}
}

var (
	_ FatalFSM    = (*fatalMockFSM)(nil)
	_ BatchingFSM = (*fatalBatchingMockFSM)(nil)
)

func (m *fatalMockFSM) Apply(l *Log) interface{} {
	if at := m.failAt.Load(); at != 0 && l.Index >= at {
		return &errFatalFSM{index: l.Index}
	}
	return m.MockFSM.Apply(l)
}

func (m *fatalMockFSM) FatalError(resp interface{}) bool {
	return IsFatalError(resp)
}

func (m *fatalMockFSM) setFailAt(index uint64) { m.failAt.Store(index) }
func (m *fatalMockFSM) clearFailAt()           { m.failAt.Store(0) }

// fatalBatchingMockFSM is the batching variant.
type fatalBatchingMockFSM struct {
	*fatalMockFSM
}

func newFatalBatchingMockFSM() *fatalBatchingMockFSM {
	return &fatalBatchingMockFSM{fatalMockFSM: newFatalMockFSM()}
}

func (m *fatalBatchingMockFSM) ApplyBatch(logs []*Log) []interface{} {
	resps := make([]interface{}, len(logs))
	for i, l := range logs {
		resps[i] = m.fatalMockFSM.Apply(l)
	}
	return resps
}

// ordinaryError is a non-fatal error returned via Apply as business data.
type ordinaryError struct{}

func (ordinaryError) Error() string { return "ordinary business error" }

func makeFatalCluster(t *testing.T, peers int, batching bool) *cluster {
	t.Helper()
	return MakeClusterCustom(t, &MakeClusterOpts{
		Peers:     peers,
		Bootstrap: true,
		Conf:      inmemConfig(t),
		MakeFSMFunc: func() FSM {
			if batching {
				return newFatalBatchingMockFSM()
			}
			return newFatalMockFSM()
		},
	})
}

func fatalFSMOf(c *cluster, i int) *fatalMockFSM {
	switch f := c.fsms[i].(type) {
	case *fatalMockFSM:
		return f
	case *fatalBatchingMockFSM:
		return f.fatalMockFSM
	}
	return nil
}

func waitForLeaderState(r *Raft, conf *Config, t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return r.State() == Leader },
		3*conf.HeartbeatTimeout+time.Second, 5*time.Millisecond)
}

func waitForFSMApplied(fsm *fatalMockFSM, n int) {
	// Best-effort poll; assertions on the caller still enforce the result.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fsm.MockFSM.Logs()) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestIsFatalError covers the public predicate.
func TestIsFatalError(t *testing.T) {
	require.False(t, IsFatalError(nil))
	require.False(t, IsFatalError(ordinaryError{}))
	require.False(t, IsFatalError("not an error"))
	require.True(t, IsFatalError(&errFatalFSM{index: 7}))
}

// A non-fatal error returned by Apply stays business data and never trips the
// failure latch.
func TestFatalFSM_NonFatalErrorIsBusinessData(t *testing.T) {
	c := MakeClusterCustom(t, &MakeClusterOpts{
		Peers:     1,
		Bootstrap: true,
		Conf:      inmemConfig(t),
		MakeFSMFunc: func() FSM {
			return &businessErrorFSM{}
		},
	})
	defer c.Close()
	r := c.rafts[0]
	waitForLeaderState(r, c.conf, t)

	f := r.Apply([]byte("boom"), time.Second)
	require.NoError(t, f.Error())
	_, isOrdinary := f.Response().(ordinaryError)
	require.True(t, isOrdinary, "expected ordinary error in Response, got %#v", f.Response())
	require.False(t, r.hasFSMFailed())

	// The node keeps applying subsequent logs normally.
	f2 := r.Apply([]byte("ok"), time.Second)
	require.NoError(t, f2.Error())
	require.Equal(t, 2, f2.Response())
}

type businessErrorFSM struct {
	count int
}

func (b *businessErrorFSM) Apply(*Log) interface{} {
	b.count++
	if b.count == 1 {
		return ordinaryError{}
	}
	return b.count
}
func (b *businessErrorFSM) Snapshot() (FSMSnapshot, error) {
	return &emptySnapshot{}, nil
}
func (b *businessErrorFSM) Restore(io.ReadCloser) error { return nil }

type emptySnapshot struct{}

func (emptySnapshot) Persist(sink SnapshotSink) error { return sink.Close() }
func (emptySnapshot) Release()                        {}

// Single-node leader: after index N fails fatally, the node must not apply
// N+1, the applied index stays at N-1, the node steps down, and Barrier /
// Snapshot / further Apply all respect the latched failure.
func TestFatalFSM_LeaderFailureLatches(t *testing.T) {
	c := makeFatalCluster(t, 1, false)
	defer c.Close()
	r := c.rafts[0]
	waitForLeaderState(r, c.conf, t)
	fsm := fatalFSMOf(c, 0)

	// Two healthy entries.
	f1 := r.Apply([]byte("a"), time.Second)
	require.NoError(t, f1.Error())
	f2 := r.Apply([]byte("b"), time.Second)
	require.NoError(t, f2.Error())
	failIndex := f2.Index() + 1
	fence := r.AppliedIndex()
	require.Equal(t, f2.Index(), fence)

	// Arm failure at the next index and commit it on the leader.
	fsm.setFailAt(failIndex)
	failed := r.Apply([]byte("corrupt"), time.Second)
	require.Eventually(t, func() bool {
		return errors.Is(failed.Error(), ErrFSMApplyFailed)
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, failIndex, failed.Index(), "committed index must be reported even though apply failed")
	require.Nil(t, failed.Response())

	// The failed index is never applied.
	require.Eventually(t, func() bool { return r.hasFSMFailed() }, time.Second, 5*time.Millisecond)
	require.Equal(t, fence, r.AppliedIndex(), "applied index must not advance past the failure")
	require.Equal(t, 2, len(fsm.MockFSM.Logs()), "failed log must not touch the FSM")

	// A later command is not even applied.
	after := r.Apply([]byte("after"), time.Second)
	require.True(t, errors.Is(after.Error(), ErrFSMFailed) || errors.Is(after.Error(), ErrNotLeader),
		"got %v", after.Error())
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, 2, len(fsm.MockFSM.Logs()), "N+1 must not be applied to the FSM")

	// The leader steps down.
	require.Eventually(t, func() bool { return r.State() == Follower }, time.Second, 5*time.Millisecond)

	// Barrier is rejected rather than hanging.
	barrier := r.Barrier(time.Second)
	require.Error(t, barrier.Error())
	require.True(t, errors.Is(barrier.Error(), ErrFSMFailed) || errors.Is(barrier.Error(), ErrNotLeader))

	// Snapshot refuses to capture corrupt state.
	snap := r.Snapshot()
	require.ErrorIs(t, snap.Error(), ErrFSMApplyFailed)

	// Stats expose the latch.
	require.Equal(t, "true", r.Stats()["fsm_failed"])

	// Shutdown still completes cleanly.
	require.NoError(t, r.Shutdown().Error())
}

// On a follower, a fatal failure stops application while the healthy leader
// keeps committing; the follower's FSM must not see any later entry and the
// healthy cluster keeps serving.
func TestFatalFSM_FollowerFailureStopsApply(t *testing.T) {
	c := makeFatalCluster(t, 3, false)
	defer c.Close()
	leader := c.Leader()
	followers := c.Followers()
	require.Len(t, followers, 2)
	leaderIdx := c.IndexOf(leader)
	followerIdx := (leaderIdx + 1) % 3
	follower := c.rafts[followerIdx]
	followerFSM := fatalFSMOf(c, followerIdx)

	// Replicate two healthy entries.
	require.NoError(t, leader.Apply([]byte("a"), time.Second).Error())
	require.NoError(t, leader.Apply([]byte("b"), time.Second).Error())
	waitForFSMApplied(followerFSM, 2)
	fence := follower.AppliedIndex()
	require.Equal(t, 2, len(followerFSM.MockFSM.Logs()))

	// Corrupt only the follower starting at index 3; the leader FSM stays
	// healthy so the cluster continues to commit.
	followerFSM.setFailAt(fence + 1)
	for i := 0; i < 5; i++ {
		require.NoError(t, leader.Apply([]byte(fmt.Sprintf("cmd-%d", i)), time.Second).Error())
	}

	// The failed follower never applies index 3 or later.
	require.Eventually(t, func() bool { return follower.hasFSMFailed() }, time.Second, 10*time.Millisecond)
	require.Equal(t, fence, follower.AppliedIndex(), "failed follower must fence applied index")
	require.Len(t, followerFSM.MockFSM.Logs(), 2)

	// The healthy nodes applied all committed entries.
	require.Eventually(t, func() bool {
		return len(fatalFSMOf(c, leaderIdx).MockFSM.Logs()) == 7
	}, time.Second, 10*time.Millisecond)
	otherIdx := 3 - leaderIdx - followerIdx
	require.Eventually(t, func() bool {
		return len(fatalFSMOf(c, otherIdx).MockFSM.Logs()) == 7
	}, time.Second, 10*time.Millisecond)
}

// A BatchingFSM fatal response stops the rest of the batch and fences the
// applied index.
func TestFatalFSM_BatchingFatalResponse(t *testing.T) {
	c := makeFatalCluster(t, 1, true)
	defer c.Close()
	r := c.rafts[0]
	waitForLeaderState(r, c.conf, t)
	fsm := fatalFSMOf(c, 0)

	require.NoError(t, r.Apply([]byte("a"), time.Second).Error())
	failIndex := r.AppliedIndex() + 1
	healthyLen := len(fsm.MockFSM.Logs())
	fsm.setFailAt(failIndex)

	// Fire several commands; the FSM receives them as a batch. Regardless of
	// how the batch boundary falls, the entry at failIndex and every later
	// entry must not be applied.
	errs := make(chan error, 3)
	for range 3 {
		f := r.Apply([]byte("batch-cmd"), time.Second)
		go func() { errs <- f.Error() }()
	}

	require.Eventually(t, func() bool { return r.hasFSMFailed() }, time.Second, 10*time.Millisecond)
	gotFatal := 0
	for range 3 {
		if errors.Is(<-errs, ErrFSMApplyFailed) {
			gotFatal++
		}
	}
	require.GreaterOrEqual(t, gotFatal, 1, "at least the failing command returns ErrFSMApplyFailed")

	require.Equal(t, failIndex-1, r.AppliedIndex())
	require.Len(t, fsm.MockFSM.Logs(), healthyLen, "entries from the failing batch must not touch the FSM")
}

// Recovery contract: once the FSM is restored from a consistent snapshot,
// the failure latch clears, the applied index moves to the snapshot, and log
// application resumes.
func TestFatalFSM_RestoreClearsFailure(t *testing.T) {
	c := makeFatalCluster(t, 1, false)
	defer c.Close()
	r := c.rafts[0]
	waitForLeaderState(r, c.conf, t)
	fsm := fatalFSMOf(c, 0)

	// Commit an entry and capture a consistent snapshot.
	require.NoError(t, r.Apply([]byte("a"), time.Second).Error())
	snapFuture := r.Snapshot()
	require.NoError(t, snapFuture.Error())
	meta, source, err := snapFuture.Open()
	require.NoError(t, err)
	require.NoError(t, source.Close(), "open only to retrieve metadata")

	require.NoError(t, r.Apply([]byte("b"), time.Second).Error())
	healthyApplied := r.AppliedIndex()
	require.Equal(t, 2, len(fsm.MockFSM.Logs()))

	// Latch a failure at the next raft index.
	fsm.setFailAt(healthyApplied + 1)
	bad := r.Apply([]byte("corrupt"), time.Second)
	require.Eventually(t, func() bool { return errors.Is(bad.Error(), ErrFSMApplyFailed) },
		time.Second, 5*time.Millisecond)
	require.True(t, r.hasFSMFailed())

	// A failed restore attempt must NOT clear the latch.
	failedRestore := &restoreFuture{ID: "does-not-exist"}
	failedRestore.init()
	r.fsmMutateCh <- failedRestore
	require.Error(t, failedRestore.Error())
	require.True(t, r.hasFSMFailed(), "failed restore must keep failure latched")

	// Repair the FSM physically and restore from the known-good snapshot.
	fsm.clearFailAt()
	okRestore := &restoreFuture{ID: meta.ID}
	okRestore.init()
	r.fsmMutateCh <- okRestore
	require.NoError(t, okRestore.Error())

	require.False(t, r.hasFSMFailed(), "successful restore must clear failure latch")
	require.Equal(t, meta.Index, r.AppliedIndex())
}

// A node with a failed FSM does not win an election while corrupt
// (single-node regression guard for candidacy gating).
func TestFatalFSM_CorruptNodeDoesNotLead(t *testing.T) {
	c := makeFatalCluster(t, 1, false)
	defer c.Close()
	r := c.rafts[0]
	waitForLeaderState(r, c.conf, t)
	fsm := fatalFSMOf(c, 0)

	require.NoError(t, r.Apply([]byte("a"), time.Second).Error())
	fsm.setFailAt(r.AppliedIndex() + 1)
	bad := r.Apply([]byte("corrupt"), time.Second)
	require.Eventually(t, func() bool { return errors.Is(bad.Error(), ErrFSMApplyFailed) },
		time.Second, 5*time.Millisecond)

	require.Eventually(t, func() bool { return r.State() == Follower }, time.Second, 5*time.Millisecond)
	time.Sleep(5 * c.conf.HeartbeatTimeout)
	require.NotEqual(t, Leader, r.State(), "corrupt node must not become leader")
}
