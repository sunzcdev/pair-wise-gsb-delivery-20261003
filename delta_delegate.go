// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

// DeltaDelegate is an optional extension of Delegate that enables
// incremental push/pull synchronization.
//
// When both peers in a push/pull implement DeltaDelegate, instead of
// exchanging their complete LocalState on every run they first exchange
// compact digests of what they have, then request and transfer only the
// changes that the other side is missing. memberlist stays agnostic about
// the application data, its ordering and its merge algorithm: every byte
// slice handed back by these methods is treated as opaque payload.
//
// A node that has fallen too far behind (for example because its peer has
// discarded old changes) is recovered transparently by transferring a full
// snapshot, which reuses LocalState and MergeRemoteState.
//
// The number of negotiation rounds and the number of bytes exchanged in a
// single sync are bounded by DeltaSyncMaxRounds and DeltaSyncMaxBytes.
type DeltaDelegate interface {
	Delegate

	// DeltaStateDigest returns a compact, application defined summary of
	// the local state. It is the first thing exchanged in an incremental
	// push/pull and lets the other side figure out which changes it is
	// missing.
	DeltaStateDigest(join bool) []byte

	// MissingDeltaChanges examines the peer's digest and returns the
	// request to send to it. If the local node cannot catch the peer up
	// from the changes it still retains, wantFull is set to true and the
	// peer replies with its full LocalState snapshot instead.
	MissingDeltaChanges(peerDigest []byte, join bool) (request []byte, wantFull bool)

	// ProvideDeltaChanges answers a request previously produced by the
	// peer's MissingDeltaChanges. It returns the changes in the exact order
	// the application expects them to be applied. If the requested changes
	// are no longer available, wantFull is set to true and the requester
	// receives the full LocalState snapshot instead.
	ProvideDeltaChanges(request []byte, join bool) (changes []byte, wantFull bool)

	// MergeDeltaChanges applies a batch of changes previously produced by
	// the peer's ProvideDeltaChanges. The payload is delivered in the same
	// order the producer assigned to it. Returning an error aborts the
	// sync round, in which case NotifyDeltaSyncComplete is not invoked.
	MergeDeltaChanges(changes []byte, join bool) error

	// NotifyDeltaSyncComplete is invoked once after an incremental
	// push/pull round has finished successfully and every change (or the
	// full snapshot used to recover an out-of-range node) requested from
	// the peer has been merged. It is never invoked for a round that fails
	// partway through, and it is not invoked by a classic full-state
	// push/pull.
	NotifyDeltaSyncComplete(join bool)
}
