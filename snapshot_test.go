package immutable

import (
	"fmt"
	"math/rand"
	"testing"
)

// assertList checks that a list has the expected length and contents.
func assertList[T comparable](t *testing.T, label string, l *List[T], want []T) {
	t.Helper()
	if got := l.Len(); got != len(want) {
		t.Fatalf("%s: len=%d, expected %d", label, got, len(want))
	}
	for i, v := range want {
		if got := l.Get(i); got != v {
			t.Fatalf("%s: get(%d)=%v, expected %v", label, i, got, v)
		}
	}
}

// publishList stores the current builder state alongside a copied expectation.
func publishList[T comparable](b *ListBuilder[T], want []T, snaps *[]*List[T], wants *[][]T) {
	*snaps = append(*snaps, b.Snapshot())
	dup := make([]T, len(want))
	copy(dup, want)
	*wants = append(*wants, dup)
}

// TestListBuilder_Snapshot publishes several snapshots while the builder
// keeps mutating and verifies every snapshot independently. Mutations cross
// 32-wide leaf boundaries and 1024/32768 element branch boundaries so the
// list grows in height and origin shifts via Prepend.
func TestListBuilder_Snapshot(t *testing.T) {
	b := NewListBuilder[int]()
	var snaps []*List[int]
	var wants [][]int
	var cur []int

	// Empty snapshot.
	publishList(b, cur, &snaps, &wants)

	// Within a single leaf node.
	for i := 0; i < 20; i++ {
		b.Append(i)
		cur = append(cur, i)
	}
	publishList(b, cur, &snaps, &wants)

	// Cross leaf boundary: depth 1 and beyond.
	for i := 20; i < 1100; i++ {
		b.Append(i)
		cur = append(cur, i)
	}
	publishList(b, cur, &snaps, &wants)

	// Overwrite values in shared leaf/branch nodes.
	for i := 0; i < 1100; i += 7 {
		b.Set(i, -i)
		cur[i] = -i
	}
	publishList(b, cur, &snaps, &wants)

	// Grow past 32768 elements: depth 3.
	for i := 1100; i < 40000; i++ {
		b.Append(i)
		cur = append(cur, i)
	}
	publishList(b, cur, &snaps, &wants)

	// Prepend shifts origin left; first call forces a taller root to the left.
	for i := 0; i < 2000; i++ {
		b.Prepend(-100000 - i)
		cur = append([]int{-100000 - i}, cur...)
	}
	publishList(b, cur, &snaps, &wants)

	// Slice contracts the tree while the earlier snapshots keep their nodes.
	b.Slice(1000, b.Len()-1000)
	cur = cur[1000 : len(cur)-1000]
	publishList(b, cur, &snaps, &wants)

	// Continue mutating after shrinking.
	for i := 0; i < 500; i++ {
		b.Append(700000 + i)
		cur = append(cur, 700000+i)
	}
	publishList(b, cur, &snaps, &wants)

	for i, snap := range snaps {
		assertList(t, fmt.Sprintf("snapshot #%d", i), snap, wants[i])
	}

	// Final terminating List() still matches, and the builder is invalidated.
	final := b.List()
	assertList(t, "final", final, cur)
	assertPanics(t, "ListBuilder.List() again", func() { b.List() })
	assertPanics(t, "ListBuilder.Len() after List()", func() { b.Len() })
	assertPanics(t, "ListBuilder.Append() after List()", func() { b.Append(1) })

	// All earlier snapshots remain valid after termination.
	for i, snap := range snaps {
		assertList(t, fmt.Sprintf("snapshot #%d (post-terminate)", i), snap, wants[i])
	}
}

// TestListBuilder_SnapshotShrink verifies node contraction after a Slice: the
// builder root drops back to leaf depth while the pre-shrink snapshot keeps
// its taller tree and full contents.
func TestListBuilder_SnapshotShrink(t *testing.T) {
	b := NewListBuilder[int]()
	var want []int
	for i := 0; i < 5000; i++ {
		b.Append(i)
		want = append(want, i)
	}
	tall := b.Snapshot()
	if d := b.list.root.depth(); d < 2 {
		t.Fatalf("expected depth >= 2 before shrink, got %d", d)
	}

	b.Slice(100, 120)
	// Contraction stops once a single child remains per level; the subrange is
	// small enough to collapse the depth-2 root into a depth-1 branch.
	if d := b.list.root.depth(); d != 1 {
		t.Fatalf("expected depth 1 after shrink, got %d", d)
	}
	if got := b.Len(); got != 20 {
		t.Fatalf("unexpected builder len after shrink: %d", got)
	}
	for i := 100; i < 120; i++ {
		if got := b.Get(i - 100); got != i {
			t.Fatalf("get(%d)=%d, expected %d", i-100, got, i)
		}
	}

	// The tall snapshot must be untouched by the contraction and later writes.
	b.Append(999999)
	assertList(t, "pre-shrink snapshot", tall, want)
}

// TestListBuilder_SnapshotShallow documents the value-sharing boundary:
// element values are not deep copied, so mutating a stored pointer-based value
// is visible everywhere it appears.
func TestListBuilder_SnapshotShallow(t *testing.T) {
	b := NewListBuilder[*int]()
	v := 1
	b.Append(&v)
	snap := b.Snapshot()

	if got := *snap.Get(0); got != 1 {
		t.Fatalf("snapshot value=%d, expected 1", got)
	}
	v = 2
	if got := *snap.Get(0); got != 2 {
		t.Fatalf("expected mutable value change to be visible, got %d", got)
	}
	if got := *b.Get(0); got != 2 {
		t.Fatalf("expected builder value change to be visible, got %d", got)
	}
}

// snapshotState records a builder snapshot and its expected contents.
type snapshotState[K comparable, V any] struct {
	m    *Map[K, V]
	want map[K]V
}

// TestMapBuilder_Snapshot publishes snapshots while mutating a map large
// enough to exercise array, bitmap-indexed, and hash-array root nodes, with
// interleaved deletes that shrink nodes back down.
func TestMapBuilder_Snapshot(t *testing.T) {
	// Hash into 64 possible values: collisions at the root slot force bitmap
	// nesting, and many keys force the root into a hash-array node.
	h := &mockHasher[int]{
		hash:  func(v int) uint32 { return hashUint64(uint64(v)) % 0x40 },
		equal: func(a, b int) bool { return a == b },
	}
	b := NewMapBuilder[int, int](h)
	var snaps []snapshotState[int, int]
	cur := make(map[int]int)
	publish := func() {
		want := make(map[int]int, len(cur))
		for k, v := range cur {
			want[k] = v
		}
		snaps = append(snaps, snapshotState[int, int]{m: b.Snapshot(), want: want})
	}

	publish() // empty

	rng := rand.New(rand.NewSource(42))
	keys := rng.Perm(4000)
	for _, k := range keys {
		b.Set(k, k)
		cur[k] = k
	}
	publish()

	if _, ok := b.m.root.(*mapHashArrayNode[int, int]); !ok {
		t.Fatalf("expected hash-array root, got %T", b.m.root)
	}

	// Overwrite every value through shared collision/branch/leaf nodes.
	for _, k := range keys {
		b.Set(k, k*2)
		cur[k] = k * 2
	}
	publish()

	// Delete most keys to force hash-array -> bitmap -> array contraction.
	// Delete most keys to force hash-array -> bitmap contraction. Note that
	// bitmap-indexed nodes never shrink back into array nodes.
	for _, k := range keys[:3990] {
		b.Delete(k)
		delete(cur, k)
	}
	publish()
	if _, ok := b.m.root.(*mapBitmapIndexedNode[int, int]); !ok {
		t.Fatalf("expected bitmap-indexed root after deletes, got %T", b.m.root)
	}

	// Regrow after shrinking.
	for i := 0; i < 2000; i++ {
		b.Set(i+5000, i)
		cur[i+5000] = i
	}
	publish()

	for i, s := range snaps {
		assertMap(t, fmt.Sprintf("snapshot #%d", i), s.m, s.want)
	}

	final := b.Map()
	assertMap(t, "final", final, cur)
	assertPanics(t, "MapBuilder.Map() again", func() { b.Map() })
	assertPanics(t, "MapBuilder.Set() after Map()", func() { b.Set(1, 1) })
	for i, s := range snaps {
		assertMap(t, fmt.Sprintf("snapshot #%d (post-terminate)", i), s.m, s.want)
	}
}

// TestMapBuilder_SnapshotHashCollision forces mapHashCollisionNode coverage:
// identical hashes with distinct keys are updated and deleted after a
// snapshot, and the published collision node must remain intact.
func TestMapBuilder_SnapshotHashCollision(t *testing.T) {
	h := &mockHasher[int]{
		hash:  func(int) uint32 { return 7 },
		equal: func(a, b int) bool { return a == b },
	}
	b := NewMapBuilder[int, int](h)
	// Nine colliding keys exceed the array-node threshold so a collision node
	// is promoted out of the root array node.
	for i := 1; i <= 10; i++ {
		b.Set(i, i*10)
	}
	first := b.Snapshot()
	if _, ok := first.root.(*mapHashCollisionNode[int, int]); !ok {
		t.Fatalf("expected collision root, got %T", first.root)
	}

	// Overwrite inside the shared collision node.
	b.Set(2, 2222)
	second := b.Snapshot()

	// Delete down through collision -> value node and nil.
	b.Delete(1)
	b.Delete(2)
	third := b.Snapshot()
	for i := 3; i <= 10; i++ {
		b.Delete(i)
	}

	wantFirst := map[int]int{}
	for i := 1; i <= 10; i++ {
		wantFirst[i] = i * 10
	}
	wantSecond := map[int]int{}
	for i := 1; i <= 10; i++ {
		wantSecond[i] = i * 10
	}
	wantSecond[2] = 2222
	wantThird := map[int]int{3: 30, 4: 40, 5: 50, 6: 60, 7: 70, 8: 80, 9: 90, 10: 100}
	assertMap(t, "first collision snapshot", first, wantFirst)
	assertMap(t, "second collision snapshot", second, wantSecond)
	assertMap(t, "third collision snapshot", third, wantThird)
	if got := b.Len(); got != 0 {
		t.Fatalf("expected empty builder, got %d", got)
	}
	if b.m.root != nil {
		t.Fatalf("expected nil root after all deletes, got %T", b.m.root)
	}
}

// TestMapBuilder_SnapshotCollidingUnderBranch builds a hash-array root where a
// hash-collision node is nested under a branch slot, then mutates and deletes
// entries after snapshotting to exercise the full copied path.
func TestMapBuilder_SnapshotCollidingUnderBranch(t *testing.T) {
	// Keys 1..8 share one hash; other keys spread across slots to grow the
	// root beyond the bitmap-indexed threshold.
	h := &mockHasher[int]{
		hash: func(v int) uint32 {
			if v >= 1 && v <= 8 {
				return 0
			}
			return hashUint64(uint64(v)) % 0x40
		},
		equal: func(a, b int) bool { return a == b },
	}
	b := NewMapBuilder[int, int](h)
	for i := 1; i <= 8; i++ {
		b.Set(i, i)
	}
	for k := 1000; k < 1300; k++ {
		b.Set(k, k)
	}
	before := b.Snapshot()

	b.Set(3, 3333)
	b.Delete(4)
	for k := 1000; k < 1300; k++ {
		b.Delete(k)
	}

	want := make(map[int]int)
	for i := 1; i <= 8; i++ {
		want[i] = i
	}
	for k := 1000; k < 1300; k++ {
		want[k] = k
	}
	assertMap(t, "before", before, want)

	cur := make(map[int]int, 8)
	for i := 1; i <= 8; i++ {
		if i != 4 {
			v := i
			if i == 3 {
				v = 3333
			}
			cur[i] = v
		}
	}
	assertMap(t, "builder after", b.Snapshot(), cur)
}

// TestMapBuilder_SnapshotShallow documents the value-sharing boundary: keys
// and values are not deep copied.
func TestMapBuilder_SnapshotShallow(t *testing.T) {
	b := NewMapBuilder[string, *int](nil)
	v := 1
	b.Set("a", &v)
	snap := b.Snapshot()

	got, ok := snap.Get("a")
	if !ok {
		t.Fatal("snapshot key missing")
	}
	if *got != 1 {
		t.Fatalf("snapshot value=%d, expected 1", *got)
	}
	v = 2
	if *got != 2 {
		t.Fatalf("expected mutable value change to be visible, got %d", *got)
	}
}

func assertMap[K comparable, V comparable](t *testing.T, label string, m *Map[K, V], want map[K]V) {
	t.Helper()
	if got := m.Len(); got != len(want) {
		t.Fatalf("%s: len=%d, expected %d", label, got, len(want))
	}
	for k, v := range want {
		got, ok := m.Get(k)
		if !ok {
			t.Fatalf("%s: key %v missing", label, k)
		} else if got != v {
			t.Fatalf("%s: get(%v)=%v, expected %v", label, k, got, v)
		}
	}
	count := 0
	itr := m.Iterator()
	for !itr.Done() {
		k, v, ok := itr.Next()
		if !ok {
			t.Fatalf("%s: iterator returned not-ok", label)
		}
		if w, ok := want[k]; !ok || w != v {
			t.Fatalf("%s: unexpected iterated entry %v=%v", label, k, v)
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("%s: iterated %d entries, expected %d", label, count, len(want))
	}
}

func assertPanics(t *testing.T, label string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected panic", label)
		}
	}()
	fn()
}
