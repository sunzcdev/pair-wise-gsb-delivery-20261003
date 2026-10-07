package bloom

import (
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/bits-and-blooms/bitset"
)

// Partition couples a stable partition ID with the Bloom filter that holds
// the keys of that partition.
type Partition struct {
	ID     uint64
	Filter *BloomFilter
}

// mk identifies a group of partitions whose filters share the same m and k
// parameters and can therefore be queried with a single set of locations.
type mk struct {
	m, k uint
}

// partitionGroup is the transposed index for one (m, k) group: for every bit
// position p of the filters, cols[p] records which partition slots have that
// bit set. Physical slots are dense indexes local to the group and are mapped
// back to user-supplied partition IDs through ids.
type partitionGroup struct {
	m    uint
	k    uint
	size uint
	ids  []uint64
	cols []*bitset.BitSet
}

// PartitionIndex is a read-only batch query index over many Bloom filters
// ("partitions"). It transposes the compatible filters of each (m, k) group
// by bit position, so a single key lookup computes the hash locations once
// per group and returns the IDs of all partitions that may contain the key,
// instead of re-hashing the key for every partition.
//
// The index snapshots the filters at build time: later modifications of the
// source filters do not affect it. To reflect changes, build a new index and
// replace the old one (see AtomicPartitionIndex); a PartitionIndex is never
// mutated in place, so concurrent queries always see a consistent snapshot.
type PartitionIndex struct {
	groups map[mk]*partitionGroup
	order  []mk // group keys, sorted for deterministic results
}

// NewPartitionIndex builds a read-only transposed index over the given
// partitions. Filters are snapshotted (copied) during the build; the caller
// must not mutate a filter concurrently with the build, but may freely
// mutate it afterwards. Partitions are grouped by their (m, k) parameters.
func NewPartitionIndex(partitions []Partition) (*PartitionIndex, error) {
	idx := &PartitionIndex{groups: make(map[mk]*partitionGroup)}
	for _, p := range partitions {
		if p.Filter == nil {
			return nil, fmt.Errorf("bloom: partition %d has a nil filter", p.ID)
		}
		key := mk{m: p.Filter.m, k: p.Filter.k}
		g := idx.groups[key]
		if g == nil {
			g = &partitionGroup{
				m:    key.m,
				k:    key.k,
				cols: make([]*bitset.BitSet, key.m),
			}
			for i := range g.cols {
				g.cols[i] = bitset.New(0)
			}
			idx.groups[key] = g
			idx.order = append(idx.order, key)
		}
		slot := g.size
		g.size++
		g.ids = append(g.ids, p.ID)
		// Snapshot the filter's bits so later source mutations cannot
		// leak into the index.
		snap := p.Filter.b.Clone()
		for pos, ok := snap.NextSet(0); ok; pos, ok = snap.NextSet(pos + 1) {
			g.cols[pos].Set(slot)
		}
	}
	sort.Slice(idx.order, func(i, j int) bool {
		if idx.order[i].m != idx.order[j].m {
			return idx.order[i].m < idx.order[j].m
		}
		return idx.order[i].k < idx.order[j].k
	})
	return idx, nil
}

// Test returns the IDs of all partitions whose filter may contain data,
// matching what an individual Test call on each source filter would have
// returned at build time. The key is hashed once per (m, k) group, not once
// per partition. The result is deterministic and contains no duplicates.
func (idx *PartitionIndex) Test(data []byte) []uint64 {
	if idx == nil {
		return nil
	}
	h := baseHashes(data)
	var out []uint64
	for _, key := range idx.order {
		g := idx.groups[key]
		if g.k == 0 {
			// A filter with zero hash functions matches every key,
			// mirroring BloomFilter.Test.
			out = append(out, g.ids...)
			continue
		}
		var acc *bitset.BitSet
		for i := uint(0); i < g.k; i++ {
			loc := uint(location(h, i) % uint64(g.m))
			if acc == nil {
				acc = g.cols[loc].Clone()
			} else {
				acc.InPlaceIntersection(g.cols[loc])
			}
			if acc.None() {
				break
			}
		}
		if acc == nil {
			continue
		}
		// Only slots below size are valid partition slots; bits beyond
		// it are word padding and must never produce a hit.
		for slot, ok := acc.NextSet(0); ok && uint(slot) < g.size; slot, ok = acc.NextSet(slot + 1) {
			out = append(out, g.ids[slot])
		}
	}
	return out
}

// TestString is like Test but takes a string key.
func (idx *PartitionIndex) TestString(data string) []uint64 {
	return idx.Test([]byte(data))
}

// AtomicPartitionIndex holds a *PartitionIndex that can be swapped
// atomically. Updates happen by building a fresh index and storing it, so
// concurrent Test calls always observe either the complete old index or the
// complete new one, never a half-updated state.
type AtomicPartitionIndex struct {
	v atomic.Value // stores *PartitionIndex
}

// Load returns the current index, or nil if none was stored yet.
func (a *AtomicPartitionIndex) Load() *PartitionIndex {
	if v := a.v.Load(); v != nil {
		return v.(*PartitionIndex)
	}
	return nil
}

// Store replaces the current index with idx. It is safe for concurrent use
// with Test.
func (a *AtomicPartitionIndex) Store(idx *PartitionIndex) {
	a.v.Store(idx)
}

// Rebuild builds a new index from partitions and atomically replaces the
// current one.
func (a *AtomicPartitionIndex) Rebuild(partitions []Partition) (*PartitionIndex, error) {
	idx, err := NewPartitionIndex(partitions)
	if err != nil {
		return nil, err
	}
	a.Store(idx)
	return idx, nil
}

// Test queries the currently stored index. It returns nil if no index has
// been stored yet.
func (a *AtomicPartitionIndex) Test(data []byte) []uint64 {
	return a.Load().Test(data)
}

// TestString is like Test but takes a string key.
func (a *AtomicPartitionIndex) TestString(data string) []uint64 {
	return a.Test([]byte(data))
}
