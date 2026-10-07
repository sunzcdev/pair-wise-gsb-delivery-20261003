package bloom

import (
	"fmt"
	"sort"
	"testing"
)

// naiveLookup is the reference implementation: it calls Test independently on
// every partition, recomputing the hashes once per partition.
func naiveLookup(parts []Partition, data []byte) []PartitionID {
	var result []PartitionID
	for _, p := range parts {
		if p.Filter != nil && p.Filter.Test(data) {
			result = append(result, p.ID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func idsEqual(a, b []PartitionID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buildScenario constructs several parameter groups (different m, k),
// including groups that span a machine-word boundary in partition count, an
// empty-filter partition, non-dense/non-ordered partition IDs and capacities
// that are not multiples of 64 (so positional padding bits exist).
func buildScenario() ([]Partition, [][]byte) {
	// (m, k, number of partitions, keys added to each filter)
	specs := []struct {
		m, k, n, keys uint
	}{
		{100, 3, 3, 5},    // small group, m not multiple of 64
		{200, 5, 65, 8},   // spans two partition words, one valid slot in last
		{300, 4, 130, 12}, // spans three partition words, partially filled last
	}

	var parts []Partition
	id := PartitionID(5000) // non-dense, arbitrary starting ID
	// Emit groups in a different order than the spec on purpose is unneeded;
	// IDs themselves are sparse.
	var present [][]byte
	for gi, s := range specs {
		for pi := uint(0); pi < s.n; pi++ {
			f := New(s.m, s.k)
			if pi == 1 {
				// Deliberately empty filter: must never match any key.
			} else {
				for ki := uint(0); ki < s.keys; ki++ {
					key := []byte(fmt.Sprintf("g%d-p%d-k%d", gi, pi, ki))
					f.Add(key)
					present = append(present, key)
				}
			}
			parts = append(parts, Partition{ID: id, Filter: f})
			id += 7 // sparse, non-consecutive IDs
		}
	}

	// Candidate keys: all present keys plus absent ones and a few keys that
	// may occasionally be false positives (comparison is still exact).
	candidates := append([][]byte{}, present...)
	for i := 0; i < 200; i++ {
		candidates = append(candidates, []byte(fmt.Sprintf("absent-%d", i)))
	}
	return parts, candidates
}

func TestPartitionIndexLookupMatchesNaive(t *testing.T) {
	parts, candidates := buildScenario()
	idx := BuildPartitionIndex(parts...)

	for _, key := range candidates {
		want := naiveLookup(parts, key)
		got := idx.Lookup(key)
		if !idsEqual(got, want) {
			t.Fatalf("Lookup(%s) = %v, want %v", key, got, want)
		}
		// Result must be sorted and deduplicated.
		if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
			t.Fatalf("Lookup(%s) result not sorted: %v", key, got)
		}
	}
}

func TestPartitionIndexMultipleParameterGroups(t *testing.T) {
	parts, _ := buildScenario()
	idx := BuildPartitionIndex(parts...)
	if len(idx.groups) != 3 {
		t.Fatalf("expected 3 parameter groups, got %d", len(idx.groups))
	}

	// Each group keeps its own (m, k); a key hitting one group's filters must
	// never be answered using another group's layout.
	seen := make(map[[2]uint]bool)
	for _, g := range idx.groups {
		seen[[2]uint{g.m, g.k}] = true
	}
	for _, mk := range [][2]uint{{100, 3}, {200, 5}, {300, 4}} {
		if !seen[mk] {
			t.Fatalf("missing parameter group %v", mk)
		}
	}
}

func TestPartitionIndexLookupManyDuplicateKeys(t *testing.T) {
	parts, candidates := buildScenario()
	idx := BuildPartitionIndex(parts...)

	// Build the expected union naively.
	wantSet := make(map[PartitionID]struct{})
	for _, key := range candidates {
		for _, id := range naiveLookup(parts, key) {
			wantSet[id] = struct{}{}
		}
	}

	// Repeat every key several times: results and hash work must be unchanged.
	var repeated [][]byte
	for rep := 0; rep < 4; rep++ {
		repeated = append(repeated, candidates...)
	}
	got := idx.LookupMany(repeated...)

	if len(got) != len(wantSet) {
		t.Fatalf("LookupMany returned %d IDs, want %d", len(got), len(wantSet))
	}
	for _, id := range got {
		if _, ok := wantSet[id]; !ok {
			t.Fatalf("unexpected partition %d in union", id)
		}
	}
}

func TestPartitionIndexEmptySets(t *testing.T) {
	// No partitions at all.
	idx := BuildPartitionIndex()
	if got := idx.Lookup([]byte("anything")); got != nil {
		t.Fatalf("empty index matched %v", got)
	}
	if got := idx.LookupMany([]byte("a"), []byte("b")); got != nil {
		t.Fatalf("empty index LookupMany matched %v", got)
	}

	// Nil pointer index is usable.
	var nilIdx *PartitionIndex
	if got := nilIdx.Lookup([]byte("x")); got != nil {
		t.Fatalf("nil index matched %v", got)
	}

	// Partitions with empty filters never match, even present-looking keys.
	f := New(128, 3)
	idx2 := BuildPartitionIndex(
		Partition{ID: 1, Filter: f},
		Partition{ID: 2, Filter: nil}, // nil filters are skipped
	)
	for i := 0; i < 50; i++ {
		if got := idx2.Lookup([]byte(fmt.Sprintf("key-%d", i))); len(got) != 0 {
			t.Fatalf("empty-filter partition matched: %v", got)
		}
	}
}

func TestPartitionIndexHashesOncePerGroup(t *testing.T) {
	parts, candidates := buildScenario()
	idx := BuildPartitionIndex(parts...)
	groups := len(idx.groups)
	totalPartitions := len(parts)

	// Instrument the base-hash function. A naive per-partition Test computes
	// these hashes once for every partition; the index must compute them only
	// once per parameter group.
	calls := 0
	original := hashFn
	hashFn = func(data []byte) [4]uint64 {
		calls++
		return original(data)
	}
	defer func() { hashFn = original }()

	// Single key: exactly one hash evaluation per parameter group, regardless
	// of how many partitions the group contains.
	_ = idx.Lookup(candidates[0])
	if calls != groups {
		t.Fatalf("single-key lookup evaluated hashes %d times, want %d (one per group)", calls, groups)
	}
	if calls >= totalPartitions {
		t.Fatalf("hashes evaluated %d times across %d partitions: expected no per-partition repetition",
			calls, totalPartitions)
	}
	t.Logf("single key over %d partitions in %d groups: index hashes %d vs naive %d",
		totalPartitions, groups, calls, totalPartitions)

	// LookupMany with duplicated keys: distinct keys times groups only.
	keyA, keyB := candidates[0], candidates[1]
	calls = 0
	_ = idx.LookupMany(keyA, keyA, keyA, keyB, keyA)
	want := 2 * groups
	if calls != want {
		t.Fatalf("LookupMany evaluated hashes %d times, want %d (distinct keys * groups)", calls, want)
	}
	t.Logf("two distinct keys (one repeated): index hashes %d vs naive %d",
		want, 2*totalPartitions)
}

func TestPartitionIndexSnapshotIsImmutable(t *testing.T) {
	f := New(128, 4)
	key := []byte("snapshot-key")
	f.Add([]byte("other"))
	idx := BuildPartitionIndex(Partition{ID: 7, Filter: f})

	if got := idx.Lookup(key); len(got) != 0 {
		t.Fatalf("key unexpectedly present before mutation: %v", got)
	}

	// Mutate the source filter after the index is built: the snapshot must not
	// see the new bit.
	f.Add(key)
	if got := idx.Lookup(key); len(got) != 0 {
		t.Fatalf("index observed post-build source mutation: %v", got)
	}

	// Clearing the source must not affect the snapshot either.
	f.ClearAll()
	if got := idx.Lookup([]byte("other")); len(got) != 1 || got[0] != 7 {
		t.Fatalf("snapshot changed after source clear: %v", got)
	}
}

func TestPartitionIndexDuplicateIDsDedup(t *testing.T) {
	f1 := New(128, 3)
	f2 := New(128, 3)
	key := []byte("dup")
	f1.Add(key)
	f2.Add(key)

	idx := BuildPartitionIndex(
		Partition{ID: 42, Filter: f1},
		Partition{ID: 42, Filter: f2}, // same ID, same group: ignored
	)
	got := idx.Lookup(key)
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("duplicate partition ID not deduplicated: %v", got)
	}
}

func TestPartitionIndexStoreRebuildReplace(t *testing.T) {
	f1 := New(128, 3)
	f1.Add([]byte("old"))
	store := NewPartitionIndexStore(BuildPartitionIndex(Partition{ID: 1, Filter: f1}))

	if got := store.Lookup([]byte("old")); len(got) != 1 {
		t.Fatalf("store lookup failed: %v", got)
	}

	// Rebuild from a completely different set; old key must be gone, new key
	// present. The old index object remains usable (no half-updated queries).
	old := store.Current()
	f2 := New(128, 3)
	f2.Add([]byte("new"))
	store.Rebuild(Partition{ID: 2, Filter: f2})

	if got := store.Lookup([]byte("old")); len(got) != 0 {
		t.Fatalf("rebuilt index still matches old key: %v", got)
	}
	if got := store.Lookup([]byte("new")); len(got) != 1 || got[0] != 2 {
		t.Fatalf("rebuilt index misses new key: %v", got)
	}
	if got := old.Lookup([]byte("old")); len(got) != 1 || got[0] != 1 {
		t.Fatalf("previous index mutated by rebuild: %v", got)
	}

	// Zero-value store behaves like an empty index.
	var zeroStore PartitionIndexStore
	if got := zeroStore.Lookup([]byte("old")); got != nil {
		t.Fatalf("zero store should match nothing, got %v", got)
	}
	zeroStore.Replace(BuildPartitionIndex(Partition{ID: 9, Filter: f1}))
	if got := zeroStore.Lookup([]byte("old")); len(got) != 1 || got[0] != 9 {
		t.Fatalf("replace on zero store failed: %v", got)
	}
}

func TestPartitionIndexStoreConcurrentQueriesDuringReplace(t *testing.T) {
	parts, candidates := buildScenario()
	store := NewPartitionIndexStore(BuildPartitionIndex(parts...))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = store.Lookup(candidates[i%len(candidates)])
		}
	}()
	for i := 0; i < 20; i++ {
		store.Rebuild(parts...)
	}
	<-done

	// Final state must be a fully built, consistent index.
	for _, key := range candidates[:20] {
		want := naiveLookup(parts, key)
		got := store.Lookup(key)
		if !idsEqual(got, want) {
			t.Fatalf("post-rebuild Lookup(%s) = %v, want %v", key, got, want)
		}
	}
}
