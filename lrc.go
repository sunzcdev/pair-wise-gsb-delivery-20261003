/**
 * Locally Repairable Code (LRC) layout.
 *
 * This file adds an independent erasure-coding layout that trades some
 * storage overhead for cheap, *local* repair of a single failed shard:
 * the most common failure mode in object storage.
 *
 * The layout is NOT a Maximum Distance Separable (MDS) code and is
 * deliberately kept separate from the Reed-Solomon layout returned by New.
 * It does not promise that any fixed number of arbitrary shard losses can
 * always be recovered: a flat (k, g) MDS stripe survives any g losses, but
 * this LRC can be defeated by particular loss patterns even when fewer than
 * g + groups shards are gone (for example losing every member of one local
 * group). Recoverability is therefore decided per failure set by rank over
 * GF(2^8) in LRC.PlanFromPresent rather than assumed from a loss count. See
 * the README "Locally Repairable Code" section and the exhaustive checks in
 * lrc_test.go and cmd/lrccatalog for exact recoverable and non-recoverable
 * combinations.
 *
 * The payoff is repair I/O: rebuilding one data shard reads only the other
 * members of its group plus the group's local parity, instead of all k
 * shards. Repair is tiered - local XOR parity first, global Reed-Solomon
 * parity only when the local tier is insufficient.
 */

package reedsolomon

import (
	"encoding/json"
	"errors"
	"fmt"
)

// LRCParamsVersion is the version tag written into every serialized
// [LRCLayoutParams]. Bump it if the on-disk representation changes.
const LRCParamsVersion = 1

// LRC scheme identifiers. Only LRCXorLocal is implemented today.
const (
	// LRCSchemeXorLocal is the Azure-style layout: XOR local group parity
	// plus Vandermonde Reed-Solomon global parity.
	LRCSchemeXorLocal = "lrc-xor-local-v1"
)

var (
	// ErrInvalidLRCParams is returned when LRC layout parameters are
	// self-contradictory or exceed the GF(2^8) shard limit.
	ErrInvalidLRCParams = errors.New("invalid LRC layout parameters")

	// ErrUnrecoverableLRC is returned when the requested shards cannot be
	// rebuilt from the currently available shards under this layout.
	ErrUnrecoverableLRC = errors.New("LRC: shards are not recoverable from the available set")
)

// LRCLayoutParams is the persistable description of a locally repairable
// layout. It is safe to marshal with encoding/json and later restore with
// [UnmarshalLRCLayoutParams].
//
// The code lays out DataShards logical data shards partitioned into
// len(GroupSizes) groups. Each group gets one XOR local parity shard, and
// GlobalParityShards Reed-Solomon parity shards are computed across all
// data shards. The total number of physical shards is therefore
//
//	DataShards + len(GroupSizes) + GlobalParityShards
//
// This metadata fully determines the layout, so it can be stored next to
// the object and reused to build repair plans without any side channels.
type LRCLayoutParams struct {
	// Version identifies the serialized metadata revision.
	Version int `json:"version"`
	// Scheme selects the local/global coding construction.
	Scheme string `json:"scheme"`
	// DataShards is the number of logical data shards (k).
	DataShards int `json:"data_shards"`
	// GroupSizes gives the number of data shards per local group.
	// The sizes must sum to DataShards and every group must be non-empty.
	GroupSizes []int `json:"group_sizes"`
	// GlobalParityShards is the number of Reed-Solomon global parities (g).
	GlobalParityShards int `json:"global_parity_shards"`
}

// Validate checks that the parameters describe a usable LRC layout.
func (p LRCLayoutParams) Validate() error {
	if p.Scheme != LRCSchemeXorLocal {
		return fmt.Errorf("%w: unknown scheme %q", ErrInvalidLRCParams, p.Scheme)
	}
	if p.Version != LRCParamsVersion {
		return fmt.Errorf("%w: unsupported metadata version %d", ErrInvalidLRCParams, p.Version)
	}
	k := p.DataShards
	if k <= 0 {
		return fmt.Errorf("%w: data shards must be positive", ErrInvalidLRCParams)
	}
	if len(p.GroupSizes) == 0 {
		return fmt.Errorf("%w: at least one local group is required", ErrInvalidLRCParams)
	}
	sum := 0
	for _, size := range p.GroupSizes {
		if size <= 0 {
			return fmt.Errorf("%w: every group must be non-empty", ErrInvalidLRCParams)
		}
		sum += size
	}
	if sum != k {
		return fmt.Errorf("%w: group sizes (%d) must sum to data shards (%d)", ErrInvalidLRCParams, sum, k)
	}
	if p.GlobalParityShards <= 0 {
		return fmt.Errorf("%w: at least one global parity shard is required", ErrInvalidLRCParams)
	}
	if k+len(p.GroupSizes)+p.GlobalParityShards > 256 {
		return fmt.Errorf("%w: total shards (%d) exceed the GF(2^8) limit of 256",
			ErrInvalidLRCParams, k+len(p.GroupSizes)+p.GlobalParityShards)
	}
	return nil
}

// MarshalJSON verifies the parameters before serializing them so that an
// invalid layout can never be persisted by accident.
func (p LRCLayoutParams) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	type alias LRCLayoutParams
	return json.Marshal(alias(p))
}

// LocalGroups returns the number of local groups (and local parity shards).
func (p LRCLayoutParams) LocalGroups() int { return len(p.GroupSizes) }

// TotalShards returns the physical shard count k + l + g.
func (p LRCLayoutParams) TotalShards() int {
	return p.DataShards + len(p.GroupSizes) + p.GlobalParityShards
}

// StorageOverhead returns total_shards / data_shards as a floating point
// multiplier so callers can compare the LRC footprint against flat RS.
func (p LRCLayoutParams) StorageOverhead() float64 {
	return float64(p.TotalShards()) / float64(p.DataShards)
}

// GroupOf returns the group index that owns data shard d.
func (p LRCLayoutParams) GroupOf(d int) (int, error) {
	if d < 0 || d >= p.DataShards {
		return -1, fmt.Errorf("data shard index %d out of range [0,%d)", d, p.DataShards)
	}
	offset := 0
	for g, size := range p.GroupSizes {
		if d < offset+size {
			return g, nil
		}
		offset += size
	}
	return -1, fmt.Errorf("data shard index %d not assigned to any group", d)
}

// GroupMembers returns the data shard indices belonging to group g.
func (p LRCLayoutParams) GroupMembers(g int) ([]int, error) {
	if g < 0 || g >= len(p.GroupSizes) {
		return nil, fmt.Errorf("group index %d out of range [0,%d)", g, len(p.GroupSizes))
	}
	start := 0
	for i := 0; i < g; i++ {
		start += p.GroupSizes[i]
	}
	members := make([]int, p.GroupSizes[g])
	for i := range members {
		members[i] = start + i
	}
	return members, nil
}

// LocalParityIndex returns the physical shard index of the local parity for
// group g.
func (p LRCLayoutParams) LocalParityIndex(g int) (int, error) {
	if g < 0 || g >= len(p.GroupSizes) {
		return -1, fmt.Errorf("group index %d out of range [0,%d)", g, len(p.GroupSizes))
	}
	return p.DataShards + g, nil
}

// GlobalParityIndex returns the physical shard index of global parity j.
func (p LRCLayoutParams) GlobalParityIndex(j int) (int, error) {
	if j < 0 || j >= p.GlobalParityShards {
		return -1, fmt.Errorf("global parity index %d out of range [0,%d)", j, p.GlobalParityShards)
	}
	return p.DataShards + len(p.GroupSizes) + j, nil
}

// ShardKind classifies a physical shard index.
type ShardKind int

const (
	// ShardData is a logical data shard.
	ShardData ShardKind = iota
	// ShardLocalParity is an XOR parity for one local group.
	ShardLocalParity
	// ShardGlobalParity is a Reed-Solomon parity over all data shards.
	ShardGlobalParity
)

// String returns a short label for the shard kind.
func (k ShardKind) String() string {
	switch k {
	case ShardData:
		return "data"
	case ShardLocalParity:
		return "local"
	case ShardGlobalParity:
		return "global"
	default:
		return "unknown"
	}
}

// ClassifyShard maps a physical shard index to its kind and, for data or
// local parity shards, the owning group. The second result is -1 for
// global parity shards.
func (p LRCLayoutParams) ClassifyShard(idx int) (ShardKind, int, error) {
	k, l, g := p.DataShards, len(p.GroupSizes), p.GlobalParityShards
	switch {
	case idx >= 0 && idx < k:
		grp, err := p.GroupOf(idx)
		return ShardData, grp, err
	case idx >= k && idx < k+l:
		return ShardLocalParity, idx - k, nil
	case idx >= k+l && idx < k+l+g:
		return ShardGlobalParity, -1, nil
	default:
		return 0, -1, fmt.Errorf("shard index %d out of range [0,%d)", idx, k+l+g)
	}
}

// NewUniformLRC builds parameters for the common case of dataShards data
// shards split evenly into localGroups groups. dataShards must be divisible
// by localGroups.
func NewUniformLRC(dataShards, localGroups, globalParityShards int) (LRCLayoutParams, error) {
	if localGroups <= 0 || dataShards%localGroups != 0 {
		return LRCLayoutParams{}, fmt.Errorf("%w: %d data shards cannot be split evenly into %d groups",
			ErrInvalidLRCParams, dataShards, localGroups)
	}
	size := dataShards / localGroups
	groups := make([]int, localGroups)
	for i := range groups {
		groups[i] = size
	}
	return NewLRC(dataShards, groups, globalParityShards)
}

// NewLRC builds parameters for an explicit, possibly uneven group layout.
func NewLRC(dataShards int, groupSizes []int, globalParityShards int) (LRCLayoutParams, error) {
	groups := append([]int(nil), groupSizes...)
	p := LRCLayoutParams{
		Version:            LRCParamsVersion,
		Scheme:             LRCSchemeXorLocal,
		DataShards:         dataShards,
		GroupSizes:         groups,
		GlobalParityShards: globalParityShards,
	}
	if err := p.Validate(); err != nil {
		return LRCLayoutParams{}, err
	}
	return p, nil
}

// MarshalLRCLayoutParams serializes layout parameters to JSON for durable
// storage alongside the object.
func MarshalLRCLayoutParams(p LRCLayoutParams) ([]byte, error) {
	return p.MarshalJSON()
}

// UnmarshalLRCLayoutParams restores layout parameters previously produced by
// [MarshalLRCLayoutParams] and validates them.
func UnmarshalLRCLayoutParams(b []byte) (LRCLayoutParams, error) {
	var p LRCLayoutParams
	if err := json.Unmarshal(b, &p); err != nil {
		return LRCLayoutParams{}, err
	}
	if err := p.Validate(); err != nil {
		return LRCLayoutParams{}, err
	}
	return p, nil
}

// String renders the parameters as the compact notation LRC(k, l, g) with
// group sizes listed, e.g. LRC(12,[4,4,4],2).
func (p LRCLayoutParams) String() string {
	return fmt.Sprintf("LRC(%d,%v,%d)", p.DataShards, p.GroupSizes, p.GlobalParityShards)
}
