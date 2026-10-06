// Copyright IBM Corp. 2013, 2025
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// RaftState captures the state of a Raft node: Follower, Candidate, Leader,
// or Shutdown.
type RaftState uint32

const (
	// Follower is the initial state of a Raft node.
	Follower RaftState = iota

	// Candidate is one of the valid states of a Raft node.
	Candidate

	// Leader is one of the valid states of a Raft node.
	Leader

	// Shutdown is the terminal state of a Raft node.
	Shutdown
)

func (s RaftState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	case Shutdown:
		return "Shutdown"
	default:
		return "Unknown"
	}
}

// raftState is used to maintain various state variables
// and provides an interface to set/get the variables in a
// thread safe manner.
type raftState struct {
	// currentTerm commitIndex, lastApplied,  must be kept at the top of
	// the struct so they're 64 bit aligned which is a requirement for
	// atomic ops on 32 bit platforms.

	// The current term, cache of StableStore
	currentTerm uint64

	// Highest committed log entry
	commitIndex uint64

	// Last applied log to the FSM
	lastApplied uint64

	// fsmFailed is non-zero once the FSM has reported an unrecoverable apply
	// error. When set, fsmFailedIndex holds the last index that was applied
	// successfully (the fence); the failed entry itself is never marked
	// applied. Access atomically.
	fsmFailedIdx uint64
	fsmFailed    uint32

	// protects 4 next fields
	lastLock sync.Mutex

	// Cache the latest snapshot index/term
	lastSnapshotIndex uint64
	lastSnapshotTerm  uint64

	// Cache the latest log from LogStore
	lastLogIndex uint64
	lastLogTerm  uint64

	// Tracks running goroutines
	routinesGroup sync.WaitGroup

	// The current state
	state RaftState
}

func (r *raftState) getState() RaftState {
	stateAddr := (*uint32)(&r.state)
	return RaftState(atomic.LoadUint32(stateAddr))
}

func (r *raftState) setState(s RaftState) {
	stateAddr := (*uint32)(&r.state)
	atomic.StoreUint32(stateAddr, uint32(s))
}

func (r *raftState) getCurrentTerm() uint64 {
	return atomic.LoadUint64(&r.currentTerm)
}

func (r *raftState) setCurrentTerm(term uint64) {
	atomic.StoreUint64(&r.currentTerm, term)
}

func (r *raftState) getLastLog() (index, term uint64) {
	r.lastLock.Lock()
	index = r.lastLogIndex
	term = r.lastLogTerm
	r.lastLock.Unlock()
	return
}

func (r *raftState) setLastLog(index, term uint64) {
	r.lastLock.Lock()
	r.lastLogIndex = index
	r.lastLogTerm = term
	r.lastLock.Unlock()
}

func (r *raftState) getLastSnapshot() (index, term uint64) {
	r.lastLock.Lock()
	index = r.lastSnapshotIndex
	term = r.lastSnapshotTerm
	r.lastLock.Unlock()
	return
}

func (r *raftState) setLastSnapshot(index, term uint64) {
	r.lastLock.Lock()
	r.lastSnapshotIndex = index
	r.lastSnapshotTerm = term
	r.lastLock.Unlock()
}

func (r *raftState) getCommitIndex() uint64 {
	return atomic.LoadUint64(&r.commitIndex)
}

func (r *raftState) setCommitIndex(index uint64) {
	atomic.StoreUint64(&r.commitIndex, index)
}

func (r *raftState) getLastApplied() uint64 {
	return atomic.LoadUint64(&r.lastApplied)
}

func (r *raftState) setLastApplied(index uint64) {
	// Never advance lastApplied past the FSM failure fence. The FSM
	// goroutine stores the fence before flipping the failure flag, so any
	// observer that sees the failure reads a consistent fence. This is a
	// plain store rather than a monotonic CAS because the FSM goroutine
	// must be able to pull lastApplied back to the fence when the main
	// thread optimistically advanced it before the failure was observed.
	if r.hasFSMFailed() {
		if fence := atomic.LoadUint64(&r.fsmFailedIdx); index > fence {
			index = fence
		}
	}
	atomic.StoreUint64(&r.lastApplied, index)
}

// hasFSMFailed reports whether the FSM is latched in an unrecoverable
// failure state. Safe to call from any goroutine.
func (r *raftState) hasFSMFailed() bool {
	return atomic.LoadUint32(&r.fsmFailed) != 0
}

// getFSMFailure returns the fence index (last index applied successfully)
// and whether the FSM has failed unrecoverably.
func (r *raftState) getFSMFailure() (lastAppliedIndex uint64, failed bool) {
	return atomic.LoadUint64(&r.fsmFailedIdx), atomic.LoadUint32(&r.fsmFailed) != 0
}

// latchFSMFailure marks the FSM as failed at the given fence index. The
// failed entry is never marked applied; lastSuccessfulApplied is the last
// index known to be applied to the FSM. This is set by the FSM goroutine
// and read by the main thread.
func (r *raftState) latchFSMFailure(lastSuccessfulApplied uint64) {
	// Store the fence before flipping the flag so that any observer that
	// sees the failure always reads a consistent fence.
	atomic.StoreUint64(&r.fsmFailedIdx, lastSuccessfulApplied)
	atomic.StoreUint32(&r.fsmFailed, 1)
}

// clearFSMFailure resets the failure latch. It must only be called once the
// FSM has been rebuilt from consistent state, i.e. after a successful
// snapshot restore on the FSM goroutine.
func (r *raftState) clearFSMFailure() {
	atomic.StoreUint32(&r.fsmFailed, 0)
}

// failCommitTuples responds to every tuple carrying a client future with an
// FSM-apply failure, without delivering any FSM response. It is used to drain
// committed entries that must not be applied while the FSM has failed.
func (r *Raft) failCommitTuples(reqs []*commitTuple, base error) {
	for _, req := range reqs {
		if req.future != nil {
			req.future.response = nil
			req.future.respond(fmt.Errorf("%w: index %d", base, req.log.Index))
		}
	}
}

// Start a goroutine and properly handle the race between a routine
// starting and incrementing, and exiting and decrementing.
func (r *raftState) goFunc(f func()) {
	r.routinesGroup.Add(1)
	go func() {
		defer r.routinesGroup.Done()
		f()
	}()
}

func (r *raftState) waitShutdown() {
	r.routinesGroup.Wait()
}

// getLastIndex returns the last index in stable storage.
// Either from the last log or from the last snapshot.
func (r *raftState) getLastIndex() uint64 {
	r.lastLock.Lock()
	defer r.lastLock.Unlock()
	return max(r.lastLogIndex, r.lastSnapshotIndex)
}

// getLastEntry returns the last index and term in stable storage.
// Either from the last log or from the last snapshot.
func (r *raftState) getLastEntry() (uint64, uint64) {
	r.lastLock.Lock()
	defer r.lastLock.Unlock()
	if r.lastLogIndex >= r.lastSnapshotIndex {
		return r.lastLogIndex, r.lastLogTerm
	}
	return r.lastSnapshotIndex, r.lastSnapshotTerm
}
