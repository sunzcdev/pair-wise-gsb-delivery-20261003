// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import "errors"

// ErrIncrementalFallback is returned by an IncrementalDelegate to signal
// that the requested changes are no longer available (for example because
// they were pruned while the remote node was offline) and that the sync
// must fall back to a full LocalState/MergeRemoteState exchange. Returning
// this error never notifies either application that the incremental round
// completed.
var ErrIncrementalFallback = errors.New("incremental state unavailable, full sync required")

// IncrementalDelegate is an optional extension of Delegate. When both sides
// of a TCP push/pull implement it, memberlist first exchanges compact
// summaries and only requests the changes each side is missing instead of
// transferring the full LocalState. When either side does not implement it,
// the regular full-state push/pull is used unchanged.
//
// memberlist does not interpret the contents of summaries, requests or
// changes: all diffing and merging decisions are application defined. It
// only drives the negotiation, enforces ordering of delivery and bounds
// the number of negotiation rounds and bytes exchanged.
//
// All methods must be thread-safe.
type IncrementalDelegate interface {
	// IncrementalSummary returns a compact, opaque description of the
	// local state, such as watermarks, checksums or version vectors. It is
	// the first thing exchanged with the remote side.
	IncrementalSummary(join bool) []byte

	// MissingChanges inspects the remote summary and returns an opaque
	// request describing the changes the remote side should send back.
	// Returning a nil or empty request means this node believes it needs
	// nothing from the remote side.
	//
	// Return ErrIncrementalFallback if the remote summary is too old for
	// the locally retained change history and a full state exchange is
	// required.
	MissingChanges(remoteSummary []byte, join bool) ([]byte, error)

	// ProduceChanges returns the changes requested by the remote side via
	// its MissingChanges request, in the order in which the remote side
	// must apply them. The order is entirely application defined; multiple
	// returned blobs are delivered and applied in slice order.
	//
	// Return ErrIncrementalFallback if the request refers to changes that
	// have already been pruned and a full state exchange is required.
	ProduceChanges(request []byte, join bool) ([][]byte, error)

	// ApplyChange applies a single blob previously produced by
	// ProduceChanges on the remote side. Blobs are delivered in the order
	// returned by ProduceChanges.
	//
	// Return ErrIncrementalFallback to abort the incremental sync and
	// request a full state exchange. Any other error aborts the sync
	// without a fallback and without calling IncrementalSyncComplete.
	ApplyChange(change []byte, join bool) error

	// IncrementalSyncComplete is invoked exactly once after every change
	// requested from the remote side has been applied successfully. It is
	// never invoked when any exchange, ordering or application step fails,
	// including when the round falls back to a full state exchange; in
	// that case MergeRemoteState carries the completion notification as
	// usual.
	IncrementalSyncComplete(join bool)
}

// asIncrementalDelegate returns the IncrementalDelegate extension of d when
// d implements it, or nil otherwise.
func asIncrementalDelegate(d Delegate) IncrementalDelegate {
	if inc, ok := d.(IncrementalDelegate); ok {
		return inc
	}
	return nil
}
