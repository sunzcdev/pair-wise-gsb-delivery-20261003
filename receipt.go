/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import "sync/atomic"

// WriteOutcome describes the final result of a single write operation (Set or
// Del) once the cache has finished processing it.
type WriteOutcome int32

const (
	// WritePending means the write has entered the processing queue but the
	// policy has not finished processing it yet.
	WritePending WriteOutcome = iota
	// WriteApplied means the write was processed and applied by the policy.
	WriteApplied
	// WriteRejected means the write was rejected by the admission policy
	// (TinyLFU) because the item was not worth keeping.
	WriteRejected
	// WriteCostExceeded means the write was rejected because the item's cost
	// is larger than the cache's MaxCost, so it can never be admitted.
	WriteCostExceeded
	// WriteDropped means the write never entered the processing queue, e.g.
	// because the queue was full or the write was discarded while clearing
	// the cache.
	WriteDropped
	// WriteClosed means the cache was closed before the write could be
	// processed.
	WriteClosed
)

// String returns a human-readable representation of the WriteOutcome.
func (o WriteOutcome) String() string {
	switch o {
	case WritePending:
		return "pending"
	case WriteApplied:
		return "applied"
	case WriteRejected:
		return "rejected"
	case WriteCostExceeded:
		return "cost-exceeded"
	case WriteDropped:
		return "dropped"
	case WriteClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Receipt tracks the fate of a single write operation. A write that only
// entered the processing queue (e.g. Set returning true) may still be
// rejected by the policy later; the Receipt lets callers wait for the final
// outcome of their own write.
//
// Each write operation gets its own Receipt, so consecutive writes to the
// same key are tracked independently: the Receipt of an earlier write always
// reports the outcome of that specific write, regardless of later writes.
type Receipt struct {
	done    chan struct{}
	outcome atomic.Int32
}

func newReceipt() *Receipt {
	return &Receipt{done: make(chan struct{})}
}

// Done returns a channel that is closed once the write has been fully
// processed. It is useful for waiting inside a select statement.
func (r *Receipt) Done() <-chan struct{} {
	return r.done
}

// Wait blocks until the write has been fully processed and returns its
// final outcome.
func (r *Receipt) Wait() WriteOutcome {
	<-r.done
	return r.Outcome()
}

// Outcome returns the current outcome of the write without blocking. It
// returns WritePending if the write has not been fully processed yet.
func (r *Receipt) Outcome() WriteOutcome {
	return WriteOutcome(r.outcome.Load())
}

// resolve marks the receipt with the given outcome. Only the first call
// takes effect. It is a no-op on a nil receipt, so write paths that don't
// use receipts don't need nil checks.
func (r *Receipt) resolve(o WriteOutcome) {
	if r == nil {
		return
	}
	if r.outcome.CompareAndSwap(int32(WritePending), int32(o)) {
		close(r.done)
	}
}
