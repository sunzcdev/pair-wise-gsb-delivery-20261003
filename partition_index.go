/*
This file implements a read-only, transposed index over a collection of
partitioned Bloom filters.

When a key has to be looked up in many partitions, calling Test on every
partition recomputes the same hash values once per partition. The
PartitionIndex instead groups partitions that share the same parameters
(_m_ bits and _k_ hash functions) and stores the filters of a group in a
bit-position-transposed layout: for every bit position there is one packed
bitmap, in which each bit represents one partition. A key is then hashed
only once per parameter group, and all partitions that may contain it are
obtained with a handful of AND operations over machine words, independently
of the number of partitions.

The index is a point-in-time snapshot: it is built from the source filters
and never references them afterwards, so later changes to the source filters
do not affect an existing index. Updates are explicit: build a new index and
replace the current one atomically through a PartitionIndexStore. Queries
always observe either the old or the newly built index in full, never a
half-updated state.
*/
package bloom

import (
	"math/bits"
	"sort"
	"sync"
)

// hashFn computes the four base hashes of a key. It is a package-level
// variable so that the number of hash evaluations performed by the index can
// be observed in tests.
var hashFn = baseHashes

// PartitionID identifies a partitioned Bloom filter within a PartitionIndex.
// IDs are opaque: they need neither be dense nor start at zero, and they are
// independent of the physical bit slots used internally by the index.
type PartitionID uint64

// Partition pairs a partition identifier with its Bloom filter. Each
// PartitionID must be provided at most once when building an index; duplicate
// occurrences of the same ID (within the same parameter group) are ignored
// after the first.
type Partition struct {
	ID     PartitionID
	Filter *BloomFilter
}

// filterGroup holds all partitions sharing the same (m, k) parameters in a
// transposed layout.
type filterGroup struct {
	m uint
	k uint
	// ids maps the physical slot number to the user-provided partition ID.
	// Slot s corresponds to bit s in the column bitmaps.
	ids []PartitionID
	// slots is the number of valid partition slots, i.e. len(ids).
	slots int
	// wordsPerColumn is the number of machine words needed for slots bits.
	wordsPerColumn int
	// flat stores m columns of wordsPerColumn words each, concatenated:
	// column for bit position p starts at flat[p*wordsPerColumn]. Bit s of a
	// column is set iff the filter of the partition in slot s has bit p set.
	flat []uint64
}

// PartitionIndex is a read-only index over a collection of partitioned Bloom
// filters. A PartitionIndex is safe for concurrent use by multiple goroutines
// once built; it is never mutated in place. The zero value (and a nil pointer)
// is a valid empty index that matches no key.
type PartitionIndex struct {
	groups []*filterGroup
}

// BuildPartitionIndex builds a read-only index from the given partitions.
// Partitions are grouped by their (m, k) parameters; partitions with
// different parameters are never compared against each other.
//
// Only bits strictly below the filter capacity m are transposed, so padding
// bits of the last backing machine word cannot cause false hits, and empty
// partitions (partitions whose filter has no set bits) never match any key.
// The returned index is a snapshot: mutating or clearing the source filters
// after this call has no effect on it.
func BuildPartitionIndex(partitions ...Partition) *PartitionIndex {
	type grouped struct {
		m, k    uint
		ids     []PartitionID
		filters []*BloomFilter
		seen    map[PartitionID]struct{}
	}

	byParams := make(map[[2]uint]*grouped)
	var order [][2]uint
	for _, p := range partitions {
		if p.Filter == nil {
			continue
		}
		key := [2]uint{p.Filter.m, p.Filter.k}
		g := byParams[key]
		if g == nil {
			g = &grouped{m: p.Filter.m, k: p.Filter.k, seen: make(map[PartitionID]struct{})}
			byParams[key] = g
			order = append(order, key)
		}
		if _, dup := g.seen[p.ID]; dup {
			continue
		}
		g.seen[p.ID] = struct{}{}
		g.ids = append(g.ids, p.ID)
		g.filters = append(g.filters, p.Filter)
	}

	idx := &PartitionIndex{groups: make([]*filterGroup, 0, len(order))}
	for _, key := range order {
		g := byParams[key]
		slots := len(g.ids)
		wordsPerColumn := (slots + 63) / 64
		flat := make([]uint64, int(g.m)*wordsPerColumn)

		// Transpose each filter: for every set bit position, mark the
		// partition's slot in that bit position's column. Bits at or beyond m
		// are padding and must never be transposed.
		for slot, f := range g.filters {
			slotBit := uint64(1) << (uint(slot) & 63)
			wordOffset := slot >> 6
			for pos, ok := f.b.NextSet(0); ok; pos, ok = f.b.NextSet(pos + 1) {
				if pos >= g.m {
					continue
				}
				flat[int(pos)*wordsPerColumn+wordOffset] |= slotBit
			}
		}

		idx.groups = append(idx.groups, &filterGroup{
			m:              g.m,
			k:              g.k,
			ids:            g.ids,
			slots:          slots,
			wordsPerColumn: wordsPerColumn,
			flat:           flat,
		})
	}
	return idx
}

// Lookup returns the IDs of all partitions that may contain data, i.e. for
// which all k hashed bit positions of data are set. The result is sorted and
// contains no duplicates. A negative result for a partition is definitive; a
// positive result may be a false positive, exactly as for Test.
//
// The base hashes of data are computed at most once per parameter group,
// rather than once per partition.
func (idx *PartitionIndex) Lookup(data []byte) []PartitionID {
	if idx == nil || len(idx.groups) == 0 {
		return nil
	}

	var result []PartitionID
	for _, g := range idx.groups {
		if g.slots == 0 {
			continue
		}

		// Start from the all-ones partition bitmap and intersect the k columns
		// selected by the k hashed locations.
		hits := make([]uint64, g.wordsPerColumn)
		for w := range hits {
			hits[w] = ^uint64(0)
		}

		h := hashFn(data)
		for i := uint(0); i < g.k; i++ {
			loc := uint(location(h, i) % uint64(g.m))
			base := int(loc) * g.wordsPerColumn
			for w := 0; w < g.wordsPerColumn; w++ {
				hits[w] &= g.flat[base+w]
			}
		}

		// Mask out physical padding slots when the number of partitions does
		// not fill the last machine word, so a stray set bit can never name a
		// non-existent partition.
		if rem := g.slots & 63; rem != 0 {
			hits[g.wordsPerColumn-1] &= uint64(1)<<uint(rem) - 1
		}

		for w, word := range hits {
			for word != 0 {
				bit := bits.TrailingZeros64(word)
				result = append(result, g.ids[w<<6+bit])
				word &= word - 1
			}
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// LookupMany returns the union of Lookup results for all given keys. The
// result is sorted and contains no duplicates. Repeated keys are only hashed
// once per parameter group: the number of hash evaluations equals the number
// of distinct keys multiplied by the number of parameter groups.
func (idx *PartitionIndex) LookupMany(keys ...[]byte) []PartitionID {
	if idx == nil || len(keys) == 0 || len(idx.groups) == 0 {
		return nil
	}

	seenKeys := make(map[string]struct{}, len(keys))
	matched := make(map[PartitionID]struct{})
	var result []PartitionID

	for _, key := range keys {
		ks := string(key)
		if _, dup := seenKeys[ks]; dup {
			continue
		}
		seenKeys[ks] = struct{}{}
		for _, id := range idx.Lookup(key) {
			if _, already := matched[id]; !already {
				matched[id] = struct{}{}
				result = append(result, id)
			}
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// PartitionIndexStore atomically holds the current PartitionIndex. It allows a
// new snapshot to be built and swapped in without any query observing a
// partially built index. The zero value is ready to use and answers queries
// with an empty index until the first replacement.
type PartitionIndexStore struct {
	mu  sync.RWMutex
	idx *PartitionIndex
}

// NewPartitionIndexStore creates a store holding the given index.
func NewPartitionIndexStore(idx *PartitionIndex) *PartitionIndexStore {
	return &PartitionIndexStore{idx: idx}
}

// Current returns the currently published index. The returned pointer remains
// valid and immutable even after the store is replaced.
func (s *PartitionIndexStore) Current() *PartitionIndex {
	s.mu.RLock()
	idx := s.idx
	s.mu.RUnlock()
	return idx
}

// Replace atomically publishes idx as the current index and returns the
// previously published index (which may be nil). Queries concurrent with the
// replacement observe either the previous or the new index in full.
func (s *PartitionIndexStore) Replace(idx *PartitionIndex) *PartitionIndex {
	s.mu.Lock()
	old := s.idx
	s.idx = idx
	s.mu.Unlock()
	return old
}

// Rebuild builds a new index from partitions, publishes it atomically, and
// returns it. The old index keeps serving queries until the new one has been
// built completely, so no query can observe a half-updated index.
func (s *PartitionIndexStore) Rebuild(partitions ...Partition) *PartitionIndex {
	idx := BuildPartitionIndex(partitions...)
	s.Replace(idx)
	return idx
}

// Lookup performs Lookup on the currently published index.
func (s *PartitionIndexStore) Lookup(data []byte) []PartitionID {
	return s.Current().Lookup(data)
}

// LookupMany performs LookupMany on the currently published index.
func (s *PartitionIndexStore) LookupMany(keys ...[]byte) []PartitionID {
	return s.Current().LookupMany(keys...)
}
