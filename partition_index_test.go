package bloom

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

// naivePartitionTest is the reference implementation: test the key against
// every partition's filter individually.
func naivePartitionTest(partitions []Partition, data []byte) []uint64 {
	var out []uint64
	for _, p := range partitions {
		if p.Filter.Test(data) {
			out = append(out, p.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]uint64(nil), a...)
	sort.Slice(as, func(i, j int) bool { return as[i] < as[j] })
	for i := range as {
		if as[i] != b[i] {
			return false
		}
	}
	return true
}

func makePartitions(specs ...struct {
	n       int
	m, k    uint
	idStart uint64
}) ([]Partition, [][]string) {
	var partitions []Partition
	var keys [][]string
	for _, s := range specs {
		for i := 0; i < s.n; i++ {
			f := New(s.m, s.k)
			var pkeys []string
			for j := 0; j < 20; j++ {
				key := fmt.Sprintf("key-m%d-k%d-p%d-%d", s.m, s.k, i, j)
				f.AddString(key)
				pkeys = append(pkeys, key)
			}
			partitions = append(partitions, Partition{
				ID:     s.idStart + uint64(i),
				Filter: f,
			})
			keys = append(keys, pkeys)
		}
	}
	return partitions, keys
}

// TestPartitionIndexMatchesNaive checks the index against naive per-filter
// Test calls over multiple (m, k) groups, including duplicate and absent keys.
func TestPartitionIndexMatchesNaive(t *testing.T) {
	partitions, keys := makePartitions(
		struct {
			n       int
			m, k    uint
			idStart uint64
		}{70, 1024, 4, 0}, // slots do not fill a whole number of words
		struct {
			n       int
			m, k    uint
			idStart uint64
		}{3, 1024, 4, 1000}, // same group parameters, disjoint IDs
		struct {
			n       int
			m, k    uint
			idStart uint64
		}{5, 4096, 7, 2000}, // a second (m, k) group
	)
	idx, err := NewPartitionIndex(partitions)
	if err != nil {
		t.Fatal(err)
	}

	var queries []string
	for _, pkeys := range keys {
		queries = append(queries, pkeys...)
	}
	// Duplicate keys must give the same result on every lookup.
	queries = append(queries, queries[0], queries[0], queries[len(queries)-1])
	// Keys never added to any filter.
	for i := 0; i < 50; i++ {
		queries = append(queries, fmt.Sprintf("absent-%d", i))
	}

	for _, q := range queries {
		got := idx.TestString(q)
		want := naivePartitionTest(partitions, []byte(q))
		if !equalIDs(got, want) {
			t.Errorf("query %q: index returned %v, naive returned %v", q, got, want)
		}
		// No duplicate IDs in a single result.
		seen := make(map[uint64]bool)
		for _, id := range got {
			if seen[id] {
				t.Errorf("query %q: duplicate partition ID %d in %v", q, id, got)
			}
			seen[id] = true
		}
	}
}

// TestPartitionIndexEmpty covers empty inputs: no partitions at all, and
// partitions whose filters contain no keys.
func TestPartitionIndexEmpty(t *testing.T) {
	idx, err := NewPartitionIndex(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.TestString("anything"); len(got) != 0 {
		t.Errorf("empty index returned %v", got)
	}
	var nilIdx *PartitionIndex
	if got := nilIdx.TestString("anything"); got != nil {
		t.Errorf("nil index returned %v", got)
	}

	// Empty partitions must never match, and word padding beyond the last
	// slot must not produce phantom hits.
	partitions := []Partition{
		{ID: 7, Filter: New(512, 3)},
		{ID: 8, Filter: New(512, 3)},
		{ID: 9, Filter: New(512, 3)},
	}
	idx, err = NewPartitionIndex(partitions)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		q := fmt.Sprintf("probe-%d", i)
		if got := idx.TestString(q); len(got) != 0 {
			t.Errorf("query %q against empty partitions returned %v", q, got)
		}
	}

	// A nil filter is rejected.
	if _, err := NewPartitionIndex([]Partition{{ID: 1}}); err == nil {
		t.Error("expected error for nil filter")
	}
}

// TestPartitionIndexSnapshot verifies that mutating source filters after the
// build does not change index results.
func TestPartitionIndexSnapshot(t *testing.T) {
	f1 := New(1024, 4)
	f1.AddString("alpha")
	f2 := New(1024, 4)
	f2.AddString("beta")
	partitions := []Partition{{ID: 1, Filter: f1}, {ID: 2, Filter: f2}}
	idx, err := NewPartitionIndex(partitions)
	if err != nil {
		t.Fatal(err)
	}

	before := idx.TestString("alpha")
	// Mutate the sources: add a new key and clear a filter entirely.
	f1.AddString("gamma")
	f2.ClearAll()

	if got := idx.TestString("alpha"); !equalIDs(got, before) {
		t.Errorf("result for alpha changed after source mutation: %v -> %v", before, got)
	}
	if got := idx.TestString("gamma"); len(got) != 0 {
		t.Errorf("key added after build must not appear, got %v", got)
	}
	if got := idx.TestString("beta"); !equalIDs(got, []uint64{2}) {
		t.Errorf("cleared source filter must still match in snapshot, got %v", got)
	}
}

// TestAtomicPartitionIndexReplace verifies queries never observe a
// half-updated index: updates happen only through Store/Rebuild.
func TestAtomicPartitionIndexReplace(t *testing.T) {
	var a AtomicPartitionIndex
	if got := a.TestString("x"); got != nil {
		t.Errorf("empty atomic index returned %v", got)
	}

	f1 := New(1024, 4)
	f1.AddString("one")
	if _, err := a.Rebuild([]Partition{{ID: 1, Filter: f1}}); err != nil {
		t.Fatal(err)
	}
	if got := a.TestString("one"); !equalIDs(got, []uint64{1}) {
		t.Errorf("got %v, want [1]", got)
	}

	f2 := New(1024, 4)
	f2.AddString("two")
	idx2, err := NewPartitionIndex([]Partition{{ID: 2, Filter: f2}})
	if err != nil {
		t.Fatal(err)
	}

	// Concurrent readers must always see a complete index, old or new.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := a.TestString("one")
				if len(got) > 1 || (len(got) == 1 && got[0] != 1) {
					t.Errorf("inconsistent read: %v", got)
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		a.Store(idx2)
		a.Store(a.Load()) // no-op replace
	}
	close(stop)
	wg.Wait()

	if got := a.TestString("two"); !equalIDs(got, []uint64{2}) {
		t.Errorf("got %v, want [2]", got)
	}
	if got := a.TestString("one"); len(got) != 0 {
		t.Errorf("stale key after replace: %v", got)
	}
}

// TestPartitionIndexHashCount proves that a batch query hashes the key once
// per (m, k) group, not once per partition, while naive per-filter Test
// hashes once per partition.
func TestPartitionIndexHashCount(t *testing.T) {
	var count int64
	baseHashesHook = func() { atomic.AddInt64(&count, 1) }
	defer func() { baseHashesHook = nil }()

	const groupSize = 50
	var partitions []Partition
	for i := 0; i < groupSize; i++ {
		f := New(2048, 5)
		f.AddString("shared-key")
		partitions = append(partitions, Partition{ID: uint64(i), Filter: f})
	}
	// A second group with different parameters.
	for i := 0; i < 10; i++ {
		f := New(8192, 3)
		partitions = append(partitions, Partition{ID: uint64(1000 + i), Filter: f})
	}
	idx, err := NewPartitionIndex(partitions)
	if err != nil {
		t.Fatal(err)
	}

	atomic.StoreInt64(&count, 0)
	got := idx.TestString("shared-key")
	if n := atomic.LoadInt64(&count); n != 1 {
		t.Errorf("index query computed %d hashes, want 1 (shared across groups)", n)
	}
	if len(got) != groupSize {
		t.Errorf("got %d partition hits, want %d", len(got), groupSize)
	}

	atomic.StoreInt64(&count, 0)
	naivePartitionTest(partitions, []byte("shared-key"))
	if n := atomic.LoadInt64(&count); n != int64(len(partitions)) {
		t.Errorf("naive query computed %d hashes, want %d (one per partition)", n, len(partitions))
	}
}
