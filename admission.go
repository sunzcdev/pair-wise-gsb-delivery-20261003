/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"errors"
	"fmt"

	"github.com/dgraph-io/ristretto/v2/z"
)

// admissionStateVersion is the version of the serialized admission policy
// format produced and accepted by this build.
const admissionStateVersion = 1

var (
	// ErrAdmissionStateVersion is returned when restoring an AdmissionPolicyState
	// whose Version is not supported by this build.
	ErrAdmissionStateVersion = errors.New("ristretto: unsupported admission policy state version")
	// ErrAdmissionStateSize is returned when restoring an AdmissionPolicyState
	// whose counter or bloom-filter dimensions do not match the receiving
	// policy. Ristretto never resamples or resizes the frequency sketch, so a
	// snapshot must be restored into a cache configured with the same
	// NumCounters as the one that produced it.
	ErrAdmissionStateSize = errors.New("ristretto: incompatible admission policy state size")
	// ErrAdmissionStateHash is returned when restoring an AdmissionPolicyState
	// whose KeyHashID does not match the receiving cache's KeyHashID. The
	// counts are keyed by hashed keys, so restoring counts produced under a
	// different hash function would attribute frequency to the wrong keys.
	ErrAdmissionStateHash = errors.New("ristretto: admission policy state hash identity mismatch")
	// ErrAdmissionStateInvalid is returned when an AdmissionPolicyState is
	// malformed, for example because a counter row or bloom bitset has the
	// wrong length for its declared dimensions.
	ErrAdmissionStateInvalid = errors.New("ristretto: invalid admission policy state")
)

// AdmissionPolicyState is a self-contained, serializable snapshot of the
// TinyLFU admission policy. It captures only the admission/frequency memory:
// the count-min sketch counters, the count-min row seeds (the internal hash
// identity), the bloom doorkeeper, and the reset bookkeeping. It deliberately
// does not contain any cache entries, key costs, or capacity usage, so
// restoring it can never create cache items or change the cache's cost.
//
// All fields are plain values, so an AdmissionPolicyState can be encoded with
// encoding/gob, encoding/json (see the json tags), or any other serializer.
//
// The fields required to interpret the counters are:
//
//   - Version: format version, used to reject snapshots from other builds.
//   - NumCounters: power-of-two width of the count-min sketch (one row holds
//     NumCounters 4-bit counters packed two per byte).
//   - ResetAt: number of increments at which the counters are halved.
//   - Seeds: per-row hash seeds; together with NumCounters they define how a
//     hashed key maps to counter positions.
//   - BloomSizeExp and BloomSetLocs: geometry of the doorkeeper bloom filter.
//   - KeyHashID: identity of the user-facing key-hash function (see
//     Config.KeyHashID). The admission counters are indexed by the hashed
//     keys, so a snapshot is only meaningful when restored under the same
//     hash identity.
type AdmissionPolicyState struct {
	// Version is the serialization format version.
	Version int `json:"version"`

	// NumCounters is the power-of-two number of counters in each sketch row.
	NumCounters int64 `json:"num_counters"`
	// ResetAt is the increment threshold at which the sketch is halved.
	ResetAt int64 `json:"reset_at"`
	// Incrs is the number of key increments observed since the last reset.
	Incrs int64 `json:"incrs"`

	// Seeds holds the count-min sketch per-row hash seeds. Its length is
	// always cmDepth (4); restoring a snapshot with a different number of
	// rows is rejected.
	Seeds [cmDepth]uint64 `json:"seeds"`
	// Rows holds the packed 4-bit counters for each sketch row. Each row has
	// NumCounters/2 bytes and there must be cmDepth (4) rows.
	Rows [cmDepth][]byte `json:"rows"`

	// BloomBits is the packed doorkeeper bloom bit set (8 bits per byte).
	BloomBits []byte `json:"bloom_bits"`
	// BloomSizeExp is the base-2 exponent of the bloom bit set length.
	BloomSizeExp uint64 `json:"bloom_size_exp"`
	// BloomSetLocs is the number of hash locations probed by the doorkeeper.
	BloomSetLocs uint64 `json:"bloom_set_locs"`

	// KeyHashID identifies the user-facing key-hash function that produced
	// the hashed keys. It must match Config.KeyHashID on restore.
	KeyHashID uint64 `json:"key_hash_id"`
}

// exportLocked builds a snapshot of the tinyLFU admission policy. The caller
// must hold the enclosing defaultPolicy lock so the sketch, doorkeeper and
// counters are captured as one coherent point-in-time copy with no torn
// internal structures.
func (t *tinyLFU) exportLocked(keyHashID uint64) *AdmissionPolicyState {
	state := &AdmissionPolicyState{
		Version:     admissionStateVersion,
		NumCounters: int64(t.freq.mask + 1),
		ResetAt:     t.resetAt,
		Incrs:       t.incrs,
		Seeds:       t.freq.seed,
		KeyHashID:   keyHashID,
	}
	bloom := t.door.Snapshot() // already a deep, independent copy
	state.BloomSizeExp = bloom.SizeExp
	state.BloomSetLocs = bloom.SetLocs
	state.BloomBits = bloom.Bits
	for i := range t.freq.rows {
		state.Rows[i] = append([]byte(nil), t.freq.rows[i]...)
	}
	return state
}

// validate checks that the snapshot is internally well formed.
func (s *AdmissionPolicyState) validate() error {
	if s.Version != admissionStateVersion {
		return fmt.Errorf("%w: got %d, want %d",
			ErrAdmissionStateVersion, s.Version, admissionStateVersion)
	}
	if s.NumCounters < 2 || s.NumCounters&(s.NumCounters-1) != 0 {
		return fmt.Errorf("%w: num counters %d is not a power of two >= 2",
			ErrAdmissionStateInvalid, s.NumCounters)
	}
	if s.ResetAt <= 0 {
		return fmt.Errorf("%w: reset threshold %d must be positive",
			ErrAdmissionStateInvalid, s.ResetAt)
	}
	if s.Incrs < 0 || s.Incrs > s.ResetAt {
		return fmt.Errorf("%w: increments %d out of range [0,%d]",
			ErrAdmissionStateInvalid, s.Incrs, s.ResetAt)
	}
	wantRowLen := s.NumCounters / 2
	for i, row := range s.Rows {
		if int64(len(row)) != wantRowLen {
			return fmt.Errorf("%w: sketch row %d has %d bytes, want %d",
				ErrAdmissionStateInvalid, i, len(row), wantRowLen)
		}
	}
	// Validate the packed 4-bit counters are each <= 15 implicitly (they are
	// nibbles, so every byte is legal) but the bloom geometry must match its
	// bit set. The z package performs the authoritative geometry check.
	if _, err := z.RestoreBloom(z.BloomSnapshot{
		Bits:    s.BloomBits,
		SizeExp: s.BloomSizeExp,
		SetLocs: s.BloomSetLocs,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrAdmissionStateInvalid, err)
	}
	return nil
}

// restoreLocked validates the snapshot and, only when it is fully compatible
// with this policy, atomically replaces the admission state. A rejected
// snapshot leaves the current admission policy untouched. The caller must
// hold the enclosing defaultPolicy lock.
func (t *tinyLFU) restoreLocked(state *AdmissionPolicyState) error {
	if state == nil {
		return fmt.Errorf("%w: nil state", ErrAdmissionStateInvalid)
	}
	if err := state.validate(); err != nil {
		return err
	}

	counters := int64(t.freq.mask + 1)
	if state.NumCounters != counters {
		return fmt.Errorf("%w: snapshot num counters %d, policy num counters %d",
			ErrAdmissionStateSize, state.NumCounters, counters)
	}
	if state.ResetAt != t.resetAt {
		return fmt.Errorf("%w: snapshot reset threshold %d, policy reset threshold %d",
			ErrAdmissionStateSize, state.ResetAt, t.resetAt)
	}
	if state.BloomSizeExp != t.door.SizeExp() || state.BloomSetLocs != t.door.SetLocs() {
		return fmt.Errorf("%w: snapshot doorkeeper geometry (2^%d, %d locs), "+
			"policy doorkeeper geometry (2^%d, %d locs)",
			ErrAdmissionStateSize,
			state.BloomSizeExp, state.BloomSetLocs,
			t.door.SizeExp(), t.door.SetLocs())
	}

	door, err := z.RestoreBloom(z.BloomSnapshot{
		Bits:    state.BloomBits,
		SizeExp: state.BloomSizeExp,
		SetLocs: state.BloomSetLocs,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAdmissionStateInvalid, err)
	}

	freq := &cmSketch{mask: uint64(state.NumCounters - 1)}
	freq.seed = state.Seeds
	for i := range freq.rows {
		freq.rows[i] = append(cmRow(nil), state.Rows[i]...)
	}

	// Commit only after every component has been built and validated.
	t.freq = freq
	t.door = door
	t.incrs = state.Incrs
	t.resetAt = state.ResetAt
	return nil
}

// exportAdmissionLocked returns a deep-copied snapshot of just the admission
// policy. The caller must hold p's lock.
func (p *defaultPolicy[V]) exportAdmissionLocked() *AdmissionPolicyState {
	// exportLocked already builds independent copies of every slice.
	return p.admit.exportLocked(p.keyHashID)
}

// restoreAdmissionLocked replaces only the admission policy state. It never
// touches the sampled-LFU key costs, used cost or capacity, so restoring a
// snapshot neither inserts entries nor alters the cache's cost accounting.
// The caller must hold p's lock.
func (p *defaultPolicy[V]) restoreAdmissionLocked(state *AdmissionPolicyState) error {
	if state == nil {
		return fmt.Errorf("%w: nil state", ErrAdmissionStateInvalid)
	}
	if state.KeyHashID != p.keyHashID {
		return fmt.Errorf("%w: snapshot KeyHashID %d, cache KeyHashID %d",
			ErrAdmissionStateHash, state.KeyHashID, p.keyHashID)
	}
	return p.admit.restoreLocked(state)
}
