package immutable

import (
	"math/rand"
	"testing"

	"golang.org/x/exp/constraints"
)

// listValues returns all values in a List in index order.
func listValues[T any](l *List[T]) []T {
	values := make([]T, 0, l.Len())
	itr := l.Iterator()
	for !itr.Done() {
		_, v := itr.Next()
		values = append(values, v)
	}
	return values
}

// assertListEq fails if the list's length or values differ from want.
func assertListEq[T comparable](t *testing.T, l *List[T], want []T) {
	t.Helper()
	if l.Len() != len(want) {
		t.Fatalf("len=%d, expected %d (values=%v)", l.Len(), len(want), listValues(l))
	}
	got := listValues(l)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got=%v, expected=%v", i, got, want)
		}
	}
}

// TestListBuilder_Snapshot ensures snapshots are immutable views unaffected by
// later builder writes, while the builder itself keeps accumulating data.
func TestListBuilder_Snapshot(t *testing.T) {
	b := NewListBuilder[int]()

	s1 := b.Snapshot()
	assertListEq(t, s1, []int{})

	b.Append(0)
	b.Append(1)
	s2 := b.Snapshot()
	assertListEq(t, s1, []int{})
	assertListEq(t, s2, []int{0, 1})

	b.Append(2)
	b.Set(0, 10)
	s3 := b.Snapshot()
	assertListEq(t, s1, []int{})
	assertListEq(t, s2, []int{0, 1})
	assertListEq(t, s3, []int{10, 1, 2})

	// Finalization preserves state and invalidates the builder, snapshots stay.
	l := b.List()
	assertListEq(t, l, []int{10, 1, 2})
	assertListEq(t, s1, []int{})
	assertListEq(t, s2, []int{0, 1})
	assertListEq(t, s3, []int{10, 1, 2})

	assertPanics(t, func() { b.Append(3) }, "ListBuilder should be invalid after List()")
}

// TestListBuilder_Snapshot_AfterFinalization ensures Snapshot follows the same
// invalidation rules as every other builder method once List() has been called.
func TestListBuilder_Snapshot_AfterFinalization(t *testing.T) {
	b := NewListBuilder[int]()
	b.Append(1)
	_ = b.List()
	assertPanics(t, func() { _ = b.Snapshot() }, "Snapshot should panic after List()")
}

// TestListBuilder_Snapshot_HeightGrowth exercises height growth toward both
// sides (appending and prepending past leaf/depth-1 boundaries) while earlier
// snapshots share the expanded branch nodes.
func TestListBuilder_Snapshot_Prepend(t *testing.T) {
	b := NewListBuilder[int]()

	const n = 1100
	var s1, s2 *List[int]
	for i := 0; i < n; i++ {
		if i == 32 {
			s1 = b.Snapshot() // snapshot after first leaf fills
		}
		b.Prepend(i)
	}
	s2 = b.Snapshot() // snapshot after height grows past depth 1

	// Snapshot at i==32 contains prepended values in reverse order: 31..0.
	want1 := make([]int, 32)
	for i := range want1 {
		want1[i] = 31 - i
	}
	assertListEq(t, s1, want1)

	// Now append beyond the right edge of the depth-1 tree, forcing another
	// height increase, then mutate the grown regions in place. The snapshots
	// must remain intact while the builder reflects every change.
	for j := 0; j < n; j++ {
		b.Append(n + j)
	}
	b.Set(0, -1)
	b.Prepend(-2)

	// s2: n-1, n-2, ..., 0.
	want2 := make([]int, n)
	for i := range want2 {
		want2[i] = n - 1 - i
	}
	assertListEq(t, s2, want2)
	assertListEq(t, s1, want1)

	final := b.List()
	if final.Len() != 2*n+1 {
		t.Fatalf("final len=%d, expected %d", final.Len(), 2*n+1)
	}
	if got := final.Get(0); got != -2 {
		t.Fatalf("final[0]=%d, expected -2", got)
	}
	if got := final.Get(1); got != -1 {
		t.Fatalf("final[1]=%d, expected -1", got)
	}
	// The prepended range occupies indices 1..n after the final prepend, so the
	// first appended value is at index n+1.
	if got := final.Get(n + 1); got != n {
		t.Fatalf("final[%d]=%d, expected %d", n+1, got, n)
	}
	// Last appended value (n+j with j=n-1) is at the final index.
	if got := final.Get(2 * n); got != 2*n-1 {
		t.Fatalf("final[%d]=%d, expected %d", 2*n, got, 2*n-1)
	}
}

// TestListBuilder_Snapshot_Slice exercises node contraction after Slice().
func TestListBuilder_Snapshot_Slice(t *testing.T) {
	b := NewListBuilder[int]()
	for i := 0; i < 1100; i++ {
		b.Append(i)
	}
	s1 := b.Snapshot()

	// Slice down to a range inside a single leaf so the tree contracts.
	b.Slice(10, 40)
	s2 := b.Snapshot()

	// Mutate through the contracted tree; the shared snapshot must be unaffected.
	b.Set(0, -1)
	b.Append(999)
	s3 := b.Snapshot()

	full := make([]int, 1100)
	for i := range full {
		full[i] = i
	}
	assertListEq(t, s1, full)

	want2 := make([]int, 30)
	for i := range want2 {
		want2[i] = 10 + i
	}
	assertListEq(t, s2, want2)

	want3 := make([]int, 0, len(want2)+2)
	want3 = append(want3, -1)
	want3 = append(want3, want2[1:]...) // Set(0,-1) overwrote the first sliced value (10)
	want3 = append(want3, 999)
	assertListEq(t, s3, want3)
}

// TestListBuilder_Snapshot_NoDeepCopy documents the value boundary: values are
// shared by reference; callers retain responsibility for mutable values.
func TestListBuilder_Snapshot_NoDeepCopy(t *testing.T) {
	type box struct{ n int }
	b := NewListBuilder[*box]()
	v := &box{n: 1}
	b.Append(v)
	snap := b.Snapshot()

	if got := snap.Get(0); got != v {
		t.Fatalf("snapshot value pointer=%p, expected same pointer=%p", got, v)
	}
	v.n = 2
	if got := snap.Get(0).n; got != 2 {
		t.Fatalf("snapshot saw deep-copied value=%d, expected shared value=2", got)
	}
}

// mapValues collects all key/value pairs from a Map into a Go map.
func mapValues[K comparable, V any](m *Map[K, V]) map[K]V {
	out := make(map[K]V, m.Len())
	itr := m.Iterator()
	for !itr.Done() {
		k, v, ok := itr.Next()
		if !ok {
			break
		}
		out[k] = v
	}
	return out
}

func assertMapEq[K comparable, V comparable](t *testing.T, m *Map[K, V], want map[K]V) {
	t.Helper()
	if m.Len() != len(want) {
		t.Fatalf("len=%d, expected %d", m.Len(), len(want))
	}
	got := mapValues(m)
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Fatalf("key %v: got=<%v,%v>, expected %v", k, gv, ok, v)
		}
	}
}

// TestMapBuilder_Snapshot covers basic snapshot isolation across interleaved
// sets, overwrites, and deletes.
func TestMapBuilder_Snapshot(t *testing.T) {
	b := NewMapBuilder[int, int](nil)

	s1 := b.Snapshot()
	assertMapEq(t, s1, map[int]int{})

	b.Set(1, 10)
	b.Set(2, 20)
	s2 := b.Snapshot()
	assertMapEq(t, s1, map[int]int{})
	assertMapEq(t, s2, map[int]int{1: 10, 2: 20})

	b.Set(2, 200) // overwrite
	b.Set(3, 30)
	b.Delete(1)
	s3 := b.Snapshot()
	assertMapEq(t, s1, map[int]int{})
	assertMapEq(t, s2, map[int]int{1: 10, 2: 20})
	assertMapEq(t, s3, map[int]int{2: 200, 3: 30})

	m := b.Map()
	assertMapEq(t, m, map[int]int{2: 200, 3: 30})
	assertMapEq(t, s1, map[int]int{})
	assertMapEq(t, s2, map[int]int{1: 10, 2: 20})
	assertMapEq(t, s3, map[int]int{2: 200, 3: 30})

	assertPanics(t, func() { b.Set(4, 40) }, "MapBuilder should be invalid after Map()")
}

// TestMapBuilder_Snapshot_AfterFinalization ensures Snapshot follows the same
// invalidation rules as every other builder method once Map() has been called.
func TestMapBuilder_Snapshot_AfterFinalization(t *testing.T) {
	b := NewMapBuilder[int, int](nil)
	b.Set(1, 10)
	_ = b.Map()
	assertPanics(t, func() { _ = b.Snapshot() }, "Snapshot should panic after Map()")
}

// collisionHasher hashes every key to the same value, forcing all entries into
// hash collision nodes. Keys remain distinguishable via Equal.
type collisionHasher[K constraints.Ordered] struct{}

func (collisionHasher[K]) Hash(K) uint32 { return 0 }
func (collisionHasher[K]) Equal(a, b K) bool {
	return any(a) == any(b)
}

// TestMapBuilder_Snapshot_HashCollision exercises copy-on-write through shared
// hash collision nodes (inserts, overwrites, and deletes down to a value node).
func TestMapBuilder_Snapshot_HashCollision(t *testing.T) {
	b := NewMapBuilder[int, int](collisionHasher[int]{})

	for i := 0; i < 6; i++ {
		b.Set(i, i*10)
	}
	s1 := b.Snapshot()

	b.Set(6, 60)  // append into shared collision node
	b.Set(0, 100) // overwrite inside shared collision node
	s2 := b.Snapshot()

	b.Delete(2) // still a collision node afterwards
	b.Delete(3)
	b.Delete(4)
	b.Delete(5)
	b.Delete(6) // collapses the collision node back to a value node
	b.Set(1, 111)
	s3 := b.Snapshot()

	want1 := map[int]int{}
	for i := 0; i < 6; i++ {
		want1[i] = i * 10
	}
	assertMapEq(t, s1, want1)

	want2 := map[int]int{0: 100, 6: 60}
	for i := 1; i < 6; i++ {
		want2[i] = i * 10
	}
	assertMapEq(t, s2, want2)

	// Only keys 0 and 1 remain after the deletes, with 1 overwritten.
	assertMapEq(t, s3, map[int]int{0: 100, 1: 111})
}

// limitedHasher restricts hashes to a small space so bitmap and hash-array
// nodes are exercised densely with many collisions at every trie level.
type limitedHasher struct{}

func (limitedHasher) Hash(key int) uint32 { return hashUint64(uint64(key)) % 0x7F }
func (limitedHasher) Equal(a, b int) bool { return a == b }

// TestMapBuilder_Snapshot_Randomized interleaves many snapshots with random
// sets and deletes, then verifies every snapshot independently. This covers
// bitmap->hash-array promotion and hash-array->bitmap contraction while shared
// with previously published snapshots.
func TestMapBuilder_Snapshot_Randomized(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: short")
	}

	b := NewMapBuilder[int, int](limitedHasher{})
	rng := rand.New(rand.NewSource(42))

	type snapshot struct {
		m    *Map[int, int]
		want map[int]int
	}
	snapshots := make([]snapshot, 0, 64)

	std := make(map[int]int)
	const ops = 20000
	for i := 0; i < ops; i++ {
		key := rng.Intn(500)
		if rng.Intn(3) == 0 {
			delete(std, key)
			b.Delete(key)
		} else {
			v := rng.Intn(1000000)
			std[key] = v
			b.Set(key, v)
		}

		if i%250 == 0 {
			want := make(map[int]int, len(std))
			for k, v := range std {
				want[k] = v
			}
			snapshots = append(snapshots, snapshot{m: b.Snapshot(), want: want})
		}
	}

	for _, s := range snapshots {
		assertMapEq(t, s.m, s.want)
	}

	assertMapEq(t, b.Map(), std)
}

func assertPanics(t *testing.T, fn func(), msg string) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic", msg)
		}
	}()
	fn()
}

// TestMapBuilder_Snapshot_NoDeepCopy documents the value boundary for maps:
// values are shared by reference between the builder and snapshots.
func TestMapBuilder_Snapshot_NoDeepCopy(t *testing.T) {
	type box struct{ n int }
	b := NewMapBuilder[int, *box](nil)
	v := &box{n: 1}
	b.Set(1, v)
	snap := b.Snapshot()

	got, ok := snap.Get(1)
	if !ok || got != v {
		t.Fatalf("snapshot value pointer=%p, expected same pointer=%p", got, v)
	}
	v.n = 2
	if got, _ := snap.Get(1); got.n != 2 {
		t.Fatalf("snapshot saw deep-copied value=%d, expected shared value=2", got.n)
	}
}
