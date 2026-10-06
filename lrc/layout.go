package lrc

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/klauspost/reedsolomon"
)

// LayoutType is the on-disk discriminator used by Metadata. It is always
// LayoutTypeLRC for this package and must never be set to a value that would
// make the layout be interpreted as the flat MDS layout.
const LayoutTypeLRC = "lrc/v1"

var (
	// ErrInvalidLayout is returned when layout parameters are inconsistent.
	ErrInvalidLayout = errors.New("lrc: invalid layout parameters")
	// ErrUnrecoverable is returned when the missing shards cannot be
	// reconstructed from the available shards under this LRC layout.
	ErrUnrecoverable = errors.New("lrc: shard losses are unrecoverable under this layout")
)

// Layout is a locally repairable coding layout: data shards are divided into
// equally sized groups, each group carries its own local parity, and a flat
// Reed-Solomon stripe provides global parity over all data shards.
//
// Layout values are safe for concurrent use after NewLayout returns.
type Layout struct {
	k int // data shards
	l int // groups
	p int // local parity per group
	r int // global parity

	dataPerGroup int
	localTotal   int

	global reedsolomon.Encoder // RS over all k data shards + r global parity
	groups []reedsolomon.Encoder
}

// Params is the persistable description of an LRC layout. The JSON tags make
// the layout parameters metadata that can be stored alongside an object.
type Params struct {
	// Type discriminates the coding family. It is always LayoutTypeLRC.
	// It exists so persisted metadata can never be mistaken for a flat MDS
	// Reed-Solomon layout, which has different fault-tolerance semantics.
	Type string `json:"type"`
	// DataShards is the total number k of data shards across all groups.
	DataShards int `json:"data_shards"`
	// Groups is the number l of local groups. DataShards must be divisible
	// by Groups.
	Groups int `json:"groups"`
	// LocalParity is the number p of local parity shards per group.
	LocalParity int `json:"local_parity"`
	// GlobalParity is the number r of global parity shards.
	GlobalParity int `json:"global_parity"`
}

// NewLayout validates the parameters and builds the encoders used for encoding
// and repair. DataShards must be evenly divisible by Groups and every parity
// count must be positive.
func NewLayout(k, groups, localParity, globalParity int) (*Layout, error) {
	if k <= 0 || groups <= 0 || localParity <= 0 || globalParity <= 0 {
		return nil, fmt.Errorf("%w: counts must be positive, got k=%d groups=%d local=%d global=%d",
			ErrInvalidLayout, k, groups, localParity, globalParity)
	}
	if groups > k || k%groups != 0 {
		return nil, fmt.Errorf("%w: data shards %d must be divisible by groups %d",
			ErrInvalidLayout, k, groups)
	}

	global, err := reedsolomon.New(k, globalParity)
	if err != nil {
		return nil, fmt.Errorf("%w: global encoder: %v", ErrInvalidLayout, err)
	}

	dataPerGroup := k / groups
	groupEnc := make([]reedsolomon.Encoder, groups)
	for i := range groupEnc {
		enc, err := reedsolomon.New(dataPerGroup, localParity)
		if err != nil {
			return nil, fmt.Errorf("%w: local encoder for group %d: %v", ErrInvalidLayout, i, err)
		}
		groupEnc[i] = enc
	}

	return &Layout{
		k:            k,
		l:            groups,
		p:            localParity,
		r:            globalParity,
		dataPerGroup: dataPerGroup,
		localTotal:   groups * localParity,
		global:       global,
		groups:       groupEnc,
	}, nil
}

// NewLayoutFromParams rebuilds a layout from persisted parameters.
func NewLayoutFromParams(p Params) (*Layout, error) {
	if p.Type != LayoutTypeLRC {
		return nil, fmt.Errorf("%w: metadata type %q is not %q; refusing to interpret non-LRC metadata as LRC",
			ErrInvalidLayout, p.Type, LayoutTypeLRC)
	}
	return NewLayout(p.DataShards, p.Groups, p.LocalParity, p.GlobalParity)
}

// Params returns the persistable parameters of the layout.
func (ly *Layout) Params() Params {
	return Params{
		Type:         LayoutTypeLRC,
		DataShards:   ly.k,
		Groups:       ly.l,
		LocalParity:  ly.p,
		GlobalParity: ly.r,
	}
}

// MarshalJSON serializes the layout parameters as metadata.
func (ly *Layout) MarshalJSON() ([]byte, error) {
	return json.Marshal(ly.Params())
}

// UnmarshalJSON restores a layout from persisted parameters.
func (ly *Layout) UnmarshalJSON(b []byte) error {
	var p Params
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	got, err := NewLayoutFromParams(p)
	if err != nil {
		return err
	}
	*ly = *got
	return nil
}

// DataShards returns k.
func (ly *Layout) DataShards() int { return ly.k }

// Groups returns l.
func (ly *Layout) Groups() int { return ly.l }

// LocalParity returns p.
func (ly *Layout) LocalParity() int { return ly.p }

// GlobalParity returns r.
func (ly *Layout) GlobalParity() int { return ly.r }

// Shards returns the total number of shards k + l*p + r.
func (ly *Layout) Shards() int { return ly.k + ly.localTotal + ly.r }

// DataPerGroup returns the number of data shards in each group, k/l.
func (ly *Layout) DataPerGroup() int { return ly.dataPerGroup }

// LocalShardIndex maps (group, slot) where slot is 0..DataPerGroup-1 for the
// group's data shards or DataPerGroup..DataPerGroup+p-1 for its local parity
// shards, to the flat shard index used by Encode/Reconstruct.
func (ly *Layout) LocalShardIndex(group, slot int) int {
	if slot < ly.dataPerGroup {
		return group*ly.dataPerGroup + slot
	}
	return ly.k + group*ly.p + (slot - ly.dataPerGroup)
}

// GlobalParityIndex returns the flat shard index of global parity shard j
// (0 <= j < r).
func (ly *Layout) GlobalParityIndex(j int) int {
	return ly.k + ly.localTotal + j
}

// DataIndexToGroup returns the group containing data shard index i.
func (ly *Layout) DataIndexToGroup(i int) int {
	return i / ly.dataPerGroup
}

// Encode fills every local parity shard (group XOR/RS parity over its own
// data shards) and every global parity shard (flat RS parity over all data
// shards). The slice must have Shards() entries; data shards must be present
// and equally sized.
func (ly *Layout) Encode(shards [][]byte) error {
	if len(shards) != ly.Shards() {
		return fmt.Errorf("%w: expected %d shards, got %d", ErrInvalidLayout, ly.Shards(), len(shards))
	}
	for g := 0; g < ly.l; g++ {
		view := make([][]byte, ly.dataPerGroup+ly.p)
		for s := 0; s < ly.dataPerGroup; s++ {
			view[s] = shards[ly.LocalShardIndex(g, s)]
		}
		for j := 0; j < ly.p; j++ {
			view[ly.dataPerGroup+j] = shards[ly.LocalShardIndex(g, ly.dataPerGroup+j)]
		}
		if err := ly.groups[g].Encode(view); err != nil {
			return fmt.Errorf("encode local parity for group %d: %w", g, err)
		}
	}

	globalView := make([][]byte, ly.k+ly.r)
	copy(globalView, shards[:ly.k])
	copy(globalView[ly.k:], shards[ly.k+ly.localTotal:])
	if err := ly.global.Encode(globalView); err != nil {
		return fmt.Errorf("encode global parity: %w", err)
	}
	return nil
}
