// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"fmt"
	"io"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-metrics"
)

// FSM is implemented by clients to make use of the replicated log.
type FSM interface {
	// Apply is called once a log entry is committed by a majority of the cluster.
	//
	// Apply should apply the log to the FSM. Apply must be deterministic and
	// produce the same result on all peers in the cluster.
	//
	// The returned value is returned to the client as the ApplyFuture.Response.
	Apply(*Log) interface{}

	// Snapshot returns an FSMSnapshot used to: support log compaction, to
	// restore the FSM to a previous state, or to bring out-of-date followers up
	// to a recent log index.
	//
	// The Snapshot implementation should return quickly, because Apply can not
	// be called while Snapshot is running. Generally this means Snapshot should
	// only capture a pointer to the state, and any expensive IO should happen
	// as part of FSMSnapshot.Persist.
	//
	// Apply and Snapshot are always called from the same thread, but Apply will
	// be called concurrently with FSMSnapshot.Persist. This means the FSM should
	// be implemented to allow for concurrent updates while a snapshot is happening.
	//
	// Clients of this library should make no assumptions about whether a returned
	// Snapshot() will actually be stored by Raft. In fact it's quite possible that
	// any Snapshot returned by this call will be discarded, and that
	// FSMSnapshot.Persist will never be called. Raft will always call
	// FSMSnapshot.Release however.
	Snapshot() (FSMSnapshot, error)

	// Restore is used to restore an FSM from a snapshot. It is not called
	// concurrently with any other command. The FSM must discard all previous
	// state before restoring the snapshot.
	Restore(snapshot io.ReadCloser) error
}

// BatchingFSM extends the FSM interface to add an ApplyBatch function. This can
// optionally be implemented by clients to enable multiple logs to be applied to
// the FSM in batches. Up to MaxAppendEntries could be sent in a batch.
type BatchingFSM interface {
	// ApplyBatch is invoked once a batch of log entries has been committed and
	// are ready to be applied to the FSM. ApplyBatch will take in an array of
	// log entries. These log entries will be in the order they were committed,
	// will not have gaps, and could be of a few log types. Clients should check
	// the log type prior to attempting to decode the data attached. Presently
	// the LogCommand and LogConfiguration types will be sent.
	//
	// The returned slice must be the same length as the input and each response
	// should correlate to the log at the same index of the input. The returned
	// values will be made available in the ApplyFuture returned by Raft.Apply
	// method if that method was called on the same Raft node as the FSM.
	ApplyBatch([]*Log) []interface{}

	FSM
}

// FatalError marks an error returned from FSM.Apply or
// BatchingFSM.ApplyBatch as unrecoverable. An FSM may return a value
// implementing this interface to signal that the state machine has reached
// an inconsistent or unusable state (for example because its local durable
// storage is corrupted) and that Raft must stop applying committed logs to
// it.
//
// A non-fatal error returned through FSM.Apply is business data: it is
// delivered verbatim to the client as ApplyFuture.Response and application
// of subsequent logs continues. FatalError is how an FSM distinguishes the
// two cases without changing the existing Apply return contract.
type FatalError interface {
	error

	// FatalError returns true if the error is unrecoverable and the node
	// must stop applying logs to the FSM.
	FatalError() bool
}

// FatalFSM is an optional extension of the FSM interface. An FSM that can
// suffer an unrecoverable apply failure (for example corruption of its own
// local storage) should implement this interface.
//
// When Apply returns a value for which IsFatalError reports true, Raft
// latches an FSM failure and stops applying committed logs to the FSM until
// the FSM is repaired. The index of the failed entry is NOT marked applied,
// so no later entry is ever applied on top of a corrupt state. All futures
// for entries at or after the failed index fail with ErrFSMApplyFailed.
//
// FSMs that do not implement this interface keep their historical
// behaviour: every Apply return value is treated as an ordinary client
// response, including error values.
//
// # Failure semantics
//
// Once the failure is latched the node:
//
//   - never calls Apply, ApplyBatch or Snapshot on the FSM again, so a
//     corrupt state can not be compacted into a snapshot or replicated;
//   - keeps lastApplied at the index of the last entry applied before the
//     failure (the "fence"); it never advances the applied index by simply
//     swallowing the error;
//   - steps down if it was leader and will not campaign or win an election
//     while failed, so a healthy peer can take over;
//   - fails new Apply and Barrier requests with ErrFSMFailed (outcome
//     unknown), while futures for already committed entries fail with
//     ErrFSMApplyFailed (committed, not applied).
//
// # Recovery contract
//
// The failure is sticky for the lifetime of the Raft instance. Raft will
// never silently resume applying logs. Operators must repair the FSM out of
// band using one of the supported, durable mechanisms:
//
//   - In a cluster, remove/replace the corrupt node and let the healthy
//     leader catch it up with an InstallSnapshot. A successful snapshot
//     restore clears the latch and moves lastApplied to the snapshot index.
//   - On a standalone node, shut it down, repair or rebuild the FSM's
//     storage from a known-good snapshot or backup, and restart. On startup
//     Raft restores the latest snapshot before replaying logs.
//
// A failed Restore (for example because the snapshot is unreadable) leaves
// the failure latched; only a successful Restore clears it.
type FatalFSM interface {
	FSM

	// FatalError reports whether the given value returned by Apply
	// represents an unrecoverable failure of the FSM. resp is the exact
	// value returned by Apply. Returning true latches the FSM failure
	// described on the FatalError interface.
	FatalError(resp interface{}) bool
}

// IsFatalError reports whether v is a fatal FSM error. It returns true when
// v is a non-nil value implementing FatalError whose FatalError method
// returns true.
func IsFatalError(v interface{}) bool {
	if v == nil {
		return false
	}
	fatal, ok := v.(FatalError)
	return ok && fatal.FatalError()
}

// FSMSnapshot is returned by an FSM in response to a Snapshot
// It must be safe to invoke FSMSnapshot methods with concurrent
// calls to Apply.
type FSMSnapshot interface {
	// Persist should dump all necessary state to the WriteCloser 'sink',
	// and call sink.Close() when finished or call sink.Cancel() on error.
	Persist(sink SnapshotSink) error

	// Release is invoked when we are finished with the snapshot.
	Release()
}

// runFSM is a long running goroutine responsible for applying logs
// to the FSM. This is done async of other logs since we don't want
// the FSM to block our internal operations.
func (r *Raft) runFSM() {
	var lastIndex, lastTerm uint64

	batchingFSM, batchingEnabled := r.fsm.(BatchingFSM)
	configStore, configStoreEnabled := r.fsm.(ConfigurationStore)
	fatalFSM, fatalEnabled := r.fsm.(FatalFSM)

	fsmApplyErr := func(index uint64) error {
		return fmt.Errorf("%w: index %d", ErrFSMApplyFailed, index)
	}

	// isFatal reports whether an Apply/ApplyBatch response represents an
	// unrecoverable FSM failure.
	isFatal := func(resp interface{}) bool {
		if !fatalEnabled {
			return false
		}
		return fatalFSM.FatalError(resp) || IsFatalError(resp)
	}

	// latchFailure records an unrecoverable FSM failure at the given index.
	// The failed index is deliberately not marked applied: lastIndex/lastTerm
	// are left pointing at the last entry that was applied successfully, so
	// no later entry is ever applied on top of the corrupt state. The main
	// thread fences its applied index against this value.
	latchFailure := func(req *commitTuple, resp interface{}) {
		failedIndex := req.log.Index
		r.logger.Error("fatal FSM apply error, stopping application to FSM",
			"index", failedIndex,
			"last-applied", lastIndex,
			"error", resp)
		r.latchFSMFailure(lastIndex)
		// The main thread advances lastApplied optimistically when it
		// enqueues a batch, so it may already have moved past the fence.
		// Pull it back to the last index applied successfully here, where
		// the true FSM position is known.
		r.setLastApplied(lastIndex)
		asyncNotifyCh(r.fsmFailureCh)
	}

	applySingle := func(req *commitTuple) {
		// Apply the log if a command or config change
		var resp interface{}

		switch req.log.Type {
		case LogCommand:
			start := time.Now()
			resp = r.fsm.Apply(req.log)
			metrics.MeasureSince([]string{"raft", "fsm", "apply"}, start)

			// An unrecoverable FSM error must not be delivered as a normal
			// response and must stop any further application.
			if isFatal(resp) {
				latchFailure(req, resp)
				if req.future != nil {
					req.future.respond(fmt.Errorf("%w: %v", fsmApplyErr(req.log.Index), resp))
				}
				return
			}

		case LogConfiguration:
			if !configStoreEnabled {
				// Most FSMs do not store configurations directly. The entry
				// does not advance the FSM-visible index (so a snapshot is
				// still "nothing new" before the first command), but the
				// client future must still be completed.
				if req.future != nil {
					req.future.respond(nil)
				}
				return
			}

			start := time.Now()
			configStore.StoreConfiguration(req.log.Index, DecodeConfiguration(req.log.Data))
			metrics.MeasureSince([]string{"raft", "fsm", "store_config"}, start)
		}

		// Update the indexes
		lastIndex = req.log.Index
		lastTerm = req.log.Term

		// Invoke the future if given, delivering the ordinary FSM response.
		if req.future != nil {
			req.future.response = resp
			req.future.respond(nil)
		}
	}

	applyBatch := func(reqs []*commitTuple) {
		if !batchingEnabled {
			for index, ct := range reqs {
				applySingle(ct)
				// After a fatal failure stop draining the rest of the batch;
				// the caller fails all remaining futures.
				if r.hasFSMFailed() {
					r.failCommitTuples(reqs[index+1:], ErrFSMApplyFailed)
					return
				}
			}
			return
		}

		// Only send LogCommand and LogConfiguration log types. LogBarrier types
		// will not be sent to the FSM.
		shouldSend := func(l *Log) bool {
			switch l.Type {
			case LogCommand, LogConfiguration:
				return true
			}
			return false
		}

		sendLogs := make([]*Log, 0, len(reqs))
		for _, req := range reqs {
			if shouldSend(req.log) {
				sendLogs = append(sendLogs, req.log)
			}
		}

		var responses []interface{}
		if len(sendLogs) > 0 {
			start := time.Now()
			responses = batchingFSM.ApplyBatch(sendLogs)
			metrics.MeasureSince([]string{"raft", "fsm", "applyBatch"}, start)
			metrics.AddSample([]string{"raft", "fsm", "applyBatchNum"}, float32(len(reqs)))

			// Ensure we get the expected responses
			if len(sendLogs) != len(responses) {
				panic("invalid number of responses")
			}
		}

		// Walk the batch in order. A fatal response at position k means the
		// FSM is corrupt from that index: it and every later entry fail, and
		// the indexes only advance up to the last entry applied before the
		// failure. Entries before the failure are responded to normally.
		var i int
		var fatalAt int = -1
		var fatalResp interface{}
		for reqIndex, req := range reqs {
			var resp interface{}
			if shouldSend(req.log) {
				resp = responses[i]
				if fatalAt < 0 && isFatal(resp) {
					fatalAt = reqIndex
					fatalResp = resp
				}
				i++
			}

			if fatalAt < 0 {
				if req.future != nil {
					req.future.response = resp
					req.future.respond(nil)
				}
				lastIndex = req.log.Index
				lastTerm = req.log.Term
			}
		}

		if fatalAt >= 0 {
			failedReq := reqs[fatalAt]
			latchFailure(failedReq, fatalResp)
			// The fatal tuple itself carries the underlying FSM error; the
			// entries after it are failed without an underlying cause.
			if future := failedReq.future; future != nil {
				future.response = nil
				future.respond(fmt.Errorf("%w: %v", fsmApplyErr(failedReq.log.Index), fatalResp))
			}
			r.failCommitTuples(reqs[fatalAt+1:], ErrFSMApplyFailed)
		}
	}

	restore := func(req *restoreFuture) {
		// Open the snapshot
		meta, source, err := r.snapshots.Open(req.ID)
		if err != nil {
			req.respond(fmt.Errorf("failed to open snapshot %v: %v", req.ID, err))
			return
		}
		defer func() { _ = source.Close() }()

		snapLogger := r.logger.With(
			"id", req.ID,
			"last-index", meta.Index,
			"last-term", meta.Term,
			"size-in-bytes", meta.Size,
		)

		// Attempt to restore
		if err := fsmRestoreAndMeasure(snapLogger, r.fsm, source, meta.Size); err != nil {
			req.respond(fmt.Errorf("failed to restore snapshot %v: %v", req.ID, err))
			return
		}

		// Update the last index and term
		lastIndex = meta.Index
		lastTerm = meta.Term
		// A successful restore rebuilds the FSM from consistent state, which
		// is the supported recovery path after an unrecoverable apply error.
		// Keep lastApplied aligned with the snapshot position so application
		// resumes from the right place and the failure fence is dropped.
		r.clearFSMFailure()
		r.setLastApplied(meta.Index)
		req.respond(nil)
	}

	snapshot := func(req *reqSnapshotFuture) {
		// Never snapshot a corrupt FSM: the captured state is not known to
		// represent any committed prefix, so persisting it would poison
		// compaction and followers.
		if failedAt, failed := r.getFSMFailure(); failed {
			req.respond(fmt.Errorf("%w: FSM apply failed at index %d, repair it via snapshot restore", ErrFSMApplyFailed, failedAt))
			return
		}

		// Is there something to snapshot?
		if lastIndex == 0 {
			req.respond(ErrNothingNewToSnapshot)
			return
		}

		// Start a snapshot
		start := time.Now()
		snap, err := r.fsm.Snapshot()
		metrics.MeasureSince([]string{"raft", "fsm", "snapshot"}, start)

		// Respond to the request
		req.index = lastIndex
		req.term = lastTerm
		req.snapshot = snap
		req.respond(err)
	}

	saturation := newSaturationMetric([]string{"raft", "thread", "fsm", "saturation"}, 1*time.Second)

	for {
		saturation.sleeping()

		select {
		case ptr := <-r.fsmMutateCh:
			saturation.working()

			switch req := ptr.(type) {
			case []*commitTuple:
				// If the FSM has already failed, never touch it again and
				// fail every queued entry until a restore repairs it.
				if r.hasFSMFailed() {
					r.failCommitTuples(req, ErrFSMApplyFailed)
				} else {
					applyBatch(req)
				}

			case *restoreFuture:
				restore(req)

			default:
				panic(fmt.Errorf("bad type passed to fsmMutateCh: %#v", ptr))
			}

		case req := <-r.fsmSnapshotCh:
			saturation.working()

			snapshot(req)

		case <-r.shutdownCh:
			return
		}
	}
}

// fsmRestoreAndMeasure wraps the Restore call on an FSM to consistently measure
// and report timing metrics. The caller is still responsible for calling Close
// on the source in all cases.
func fsmRestoreAndMeasure(logger hclog.Logger, fsm FSM, source io.ReadCloser, snapshotSize int64) error {
	start := time.Now()

	crc := newCountingReadCloser(source)

	monitor := startSnapshotRestoreMonitor(logger, crc, snapshotSize, false)
	defer monitor.StopAndWait()

	if err := fsm.Restore(crc); err != nil {
		return err
	}
	metrics.MeasureSince([]string{"raft", "fsm", "restore"}, start)
	metrics.SetGauge([]string{"raft", "fsm", "lastRestoreDuration"},
		float32(time.Since(start).Milliseconds()))

	return nil
}
