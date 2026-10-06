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
	// It is normal and fully supported to return a plain error here to signal a
	// recoverable, business-level failure of this single command; that error is
	// delivered through ApplyFuture.Response and never through Future.Error, and
	// it does not affect the state machine.
	//
	// To report an unrecoverable failure (for example corrupt local storage
	// that makes it impossible to apply the entry), return a value that
	// implements the optional FSMApplyError interface. That is fundamentally
	// different from a business error: the node treats the entry (and every
	// later entry) as not applied, stops applying logs, transitions to the
	// Failed state, and can only be recovered by rebuilding the process after
	// repairing the FSM/storage. See FSMApplyError for the full contract.
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
	//
	// As with FSM.Apply, a value implementing FSMApplyError marks an
	// unrecoverable failure. Once such a value is returned for some entry, the
	// FSM must not have mutated its state for that entry or any later entry in
	// the batch; responses for those later entries should carry the same
	// FSMApplyError. Raft then stops applying logs and enters the Failed state.
	ApplyBatch([]*Log) []interface{}

	FSM
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

// FSMApplyError is an optional error type that can be returned from
// FSM.Apply or BatchingFSM.ApplyBatch to indicate that an unrecoverable
// failure occurred while applying a committed log entry, for example because
// the FSM's local storage is corrupt.
//
// Returning a plain error from Apply is fully supported and remains the way
// business logic reports an ordinary, recoverable failure of a single
// command: that value is handed back unchanged through ApplyFuture.Response
// and does not affect the state machine. The two must not be conflated. An
// error only changes node behavior when it implements FSMApplyError, which
// declares that the FSM can no longer be trusted to stay consistent with the
// committed log.
//
// When an FSMApplyError is returned:
//
//   - The offending entry is treated as not applied, along with every later
//     entry, so lastApplied is not advanced past it. No subsequent committed
//     log (including entry N+1) is handed to the FSM.
//   - The future for the offending entry fails with an error that wraps
//     ErrFSMFailed and carries the FSMApplyError; later in-flight entries
//     fail the same way instead of touching the state machine.
//   - The node transitions to the Failed terminal state: it stops applying,
//     does not campaign and can not become leader, and refuses Barrier,
//     Snapshot and Restore operations.
//
// There is intentionally no in-process recovery. The operator must repair or
// replace the FSM/storage and create a new Raft instance, normally after
// restoring the FSM from a known-good snapshot. Swallowing the error and
// advancing the applied index is explicitly unsupported: it would leave the
// state machine divergent from the committed log.
type FSMApplyError interface {
	error

	// FSMApplyError is a marker method. It is never invoked; its presence
	// distinguishes an unrecoverable apply failure from an ordinary business
	// error returned via ApplyFuture.Response.
	FSMApplyError()
}

// fsmFailureError wraps an FSMApplyError reported by the FSM. Callers see
// errors.Is(err, ErrFSMFailed), and errors.As into the underlying
// FSMApplyError, so clients can distinguish a committed-but-failed apply from
// an unknown outcome (ErrLeadershipLost / ErrRaftShutdown).
type fsmFailureError struct {
	err FSMApplyError
}

func (e *fsmFailureError) Error() string {
	return fmt.Sprintf("%s: %v", ErrFSMFailed, e.err)
}

// Unwrap supports both errors.Is(err, ErrFSMFailed) and errors.As into the
// underlying FSMApplyError reported by the FSM.
func (e *fsmFailureError) Unwrap() []error {
	return []error{ErrFSMFailed, e.err}
}

// asFSMApplyError reports whether the given value is an unrecoverable apply
// failure. A non-error value or a plain business error returns nil.
func asFSMApplyError(resp interface{}) FSMApplyError {
	if resp == nil {
		return nil
	}
	applyErr, ok := resp.(error)
	if !ok {
		return nil
	}
	fatal, _ := applyErr.(FSMApplyError)
	return fatal
}

// reportFSMError records the first unrecoverable FSM error and signals the
// rest of the node exactly once. It is safe to call from any goroutine and
// never blocks. The returned wrapped error is what client futures receive.
func (r *Raft) reportFSMError(fatal FSMApplyError) *fsmFailureError {
	wrapped := &fsmFailureError{err: fatal}
	r.fatalErrOnce.Do(func() {
		r.fsmErr = wrapped
		r.logger.Error("FSM failed with an unrecoverable error; stopping application of further logs",
			"error", fatal)
		close(r.fatalErrCh)
	})
	return wrapped
}

// runFSM is a long running goroutine responsible for applying logs
// to the FSM. This is done async of other logs since we don't want
// the FSM to block our internal operations.
func (r *Raft) runFSM() {
	var lastIndex, lastTerm uint64

	// fatal is set when the FSM reports an unrecoverable error. Once set, the
	// FSM goroutine stops touching the state machine and only drains queued
	// requests by failing them.
	var fatal error

	batchingFSM, batchingEnabled := r.fsm.(BatchingFSM)
	configStore, configStoreEnabled := r.fsm.(ConfigurationStore)

	applySingle := func(req *commitTuple) {
		// Apply the log if a command or config change
		var resp interface{}
		// respondErr is the error delivered through the future's Error method.
		// It stays nil for normal applies (including ordinary business errors,
		// which are surfaced through Response) and becomes the fatal error only
		// for an unrecoverable apply failure.
		var respondErr error
		// Make sure we send a response
		defer func() {
			// Invoke the future if given
			if req.future != nil {
				req.future.response = resp
				req.future.respond(respondErr)
			}
		}()

		switch {
		case fatal != nil:
			// The state machine has already failed: never touch it again.
			resp = fatal
			respondErr = fatal

		default:
			handled := false
			switch req.log.Type {
			case LogCommand:
				start := time.Now()
				resp = r.fsm.Apply(req.log)
				metrics.MeasureSince([]string{"raft", "fsm", "apply"}, start)
				handled = true

			case LogConfiguration:
				if !configStoreEnabled {
					// Return early to avoid incrementing the index and term
					// for an unimplemented operation.
					return
				}

				start := time.Now()
				configStore.StoreConfiguration(req.log.Index, DecodeConfiguration(req.log.Data))
				metrics.MeasureSince([]string{"raft", "fsm", "store_config"}, start)
				handled = true

			case LogBarrier, LogNoop:
				// Not handed to the FSM; fall through to the index update.
			}

			if handled {
				if applyErr := asFSMApplyError(resp); applyErr != nil {
					// The entry failed to apply: record the failure, rewind
					// the applied index back to the last entry that succeeded,
					// and do not advance lastIndex/lastTerm past this entry.
					fatal = r.reportFSMError(applyErr)
					respondErr = fatal
					r.setLastApplied(lastIndex)
					return
				}
			}
		}

		// Update the indexes only for an entry that applied cleanly. A
		// LogBarrier/LogNoop reaches here without invoking the FSM, but must
		// not advance the position once the FSM has failed.
		if fatal != nil {
			return
		}
		lastIndex = req.log.Index
		lastTerm = req.log.Term
	}

	applyBatch := func(reqs []*commitTuple) {
		if fatal != nil {
			for _, ct := range reqs {
				applySingle(ct)
			}
			return
		}

		if !batchingEnabled {
			for _, ct := range reqs {
				applySingle(ct)
			}
			return
		}

		// Only LogCommand and LogConfiguration entries are handed to the FSM.
		// LogBarrier/LogNoop entries are not part of the ApplyBatch response
		// slice.
		sendLogs := make([]*Log, 0, len(reqs))
		sendReq := make([]*commitTuple, 0, len(reqs))
		for _, req := range reqs {
			switch req.log.Type {
			case LogCommand, LogConfiguration:
				sendLogs = append(sendLogs, req.log)
				sendReq = append(sendReq, req)
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

		// fatalAt is the index into sendLogs/sendReq/responses of the first
		// entry that failed unrecoverably, or -1 if the whole batch succeeded.
		fatalAt := -1
		for i, resp := range responses {
			if asFSMApplyError(resp) != nil {
				fatalAt = i
				break
			}
		}

		if fatalAt >= 0 {
			fatal = r.reportFSMError(asFSMApplyError(responses[fatalAt]))
		}
		// Index of the first entry that failed (or 0 when there was no
		// failure). Log indices in a batch are strictly increasing, so this is
		// a safe boundary for both sent and non-sent (LogBarrier/LogNoop)
		// entries.
		var fatalIndex uint64
		if fatalAt >= 0 {
			fatalIndex = sendReq[fatalAt].log.Index
		}

		// Walk every request in the batch in index order. Entries up to the
		// fatal one succeed; the fatal entry and everything after it fail
		// without advancing the applied position. Non-sent entries
		// (LogBarrier) at or after the failure fail too rather than skipping
		// over the hole.
		sentIdx := 0
		for _, req := range reqs {
			var resp interface{}
			var respondErr error
			sent := req.log.Type == LogCommand || req.log.Type == LogConfiguration
			if sent {
				resp = responses[sentIdx]
			}

			switch {
			case fatal != nil && req.log.Index >= fatalIndex:
				// Failed entry or a later entry: never advance the index.
				resp = fatal
				respondErr = fatal

			default:
				lastIndex = req.log.Index
				lastTerm = req.log.Term
			}

			if sent {
				sentIdx++
			}

			if req.future != nil {
				req.future.response = resp
				req.future.respond(respondErr)
			}
		}

		if fatalAt >= 0 {
			// Rewind the reported applied position to the last entry that
			// applied cleanly; the running lastIndex already stopped before
			// the failed entry (including across non-sent barrier entries).
			r.setLastApplied(lastIndex)
		}
	}

	restore := func(req *restoreFuture) {
		// Once the FSM has failed it is in an unknown, potentially corrupt
		// state. A snapshot restore is the only operation that could make it
		// consistent again, but doing it implicitly (e.g. via an
		// InstallSnapshot) would silently move the applied position and is not
		// a supported recovery path; the node must be rebuilt explicitly.
		if fatal != nil {
			req.respond(fatal)
			return
		}

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
		req.respond(nil)
	}

	snapshot := func(req *reqSnapshotFuture) {
		// A failed FSM must not be snapshotted: its state is not known to
		// match the committed log, so persisting it could spread corruption.
		if fatal != nil {
			req.respond(fatal)
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
				applyBatch(req)

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
