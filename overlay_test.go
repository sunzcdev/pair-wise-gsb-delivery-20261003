package immutable

import (
	"fmt"
	"reflect"
	"testing"
)

// applyOps applies a sequence of batches to a plain map, one entry at a time.
// It serves as the reference model for the merged view.
func applyOps(base *Map[string, int], batches ...*Map[string, OverlayEntry[int]]) *Map[string, int] {
	b := NewMapBuilder[string, int](nil)
	itr := base.Iterator()
	for !itr.Done() {
		k, v, _ := itr.Next()
		b.Set(k, v)
	}
	for _, batch := range batches {
		bitr := batch.Iterator()
		for !bitr.Done() {
			k, e, _ := bitr.Next()
			if e.Deleted {
				b.Delete(k)
			} else {
				b.Set(k, e.Value)
			}
		}
	}
	return b.Map()
}

func batchOf(entries map[string]OverlayEntry[int]) *Map[string, OverlayEntry[int]] {
	b := NewMapBuilder[string, OverlayEntry[int]](nil)
	for k, e := range entries {
		b.Set(k, e)
	}
	return b.Map()
}

func baseOf(entries map[string]int) *Map[string, int] {
	b := NewMapBuilder[string, int](nil)
	for k, v := range entries {
		b.Set(k, v)
	}
	return b.Map()
}

// overlayItems collects the iterated key/value pairs into a Go map.
func overlayItems(o *Overlay[string, int]) map[string]int {
	items := make(map[string]int)
	itr := o.Iterator()
	for !itr.Done() {
		k, v, _ := itr.Next()
		if _, dup := items[k]; dup {
			panic(fmt.Sprintf("duplicate key yielded: %q", k))
		}
		items[k] = v
	}
	return items
}

func mapItems(m *Map[string, int]) map[string]int {
	items := make(map[string]int)
	itr := m.Iterator()
	for !itr.Done() {
		k, v, _ := itr.Next()
		items[k] = v
	}
	return items
}

// assertEquivalent verifies Get, Len, and iteration against a reference map.
func assertEquivalent(t *testing.T, o *Overlay[string, int], ref *Map[string, int], probeKeys []string) {
	t.Helper()
	if o.Len() != ref.Len() {
		t.Fatalf("Len: got %d, want %d", o.Len(), ref.Len())
	}
	for _, k := range probeKeys {
		gotV, gotOK := o.Get(k)
		wantV, wantOK := ref.Get(k)
		if gotV != wantV || gotOK != wantOK {
			t.Fatalf("Get(%q): got (%v,%v), want (%v,%v)", k, gotV, gotOK, wantV, wantOK)
		}
	}
	if got, want := overlayItems(o), mapItems(ref); !reflect.DeepEqual(got, want) {
		t.Fatalf("iteration: got %v, want %v", got, want)
	}
}

func TestOverlay_GetPrefersLayers(t *testing.T) {
	base := baseOf(map[string]int{"a": 1, "b": 2})
	o := NewOverlay[string, int](base, 4)
	o = o.Apply(batchOf(map[string]OverlayEntry[int]{"a": SetEntry(10)}))
	o = o.Apply(batchOf(map[string]OverlayEntry[int]{"a": SetEntry(100), "c": SetEntry(3)}))

	if v, _ := o.Get("a"); v != 100 {
		t.Fatalf("Get(a): got %v, want 100", v)
	}
	if v, _ := o.Get("b"); v != 2 {
		t.Fatalf("Get(b): got %v, want 2", v)
	}
	if v, _ := o.Get("c"); v != 3 {
		t.Fatalf("Get(c): got %v, want 3", v)
	}
}

func TestOverlay_TombstoneDistinctFromZeroValue(t *testing.T) {
	base := baseOf(map[string]int{"a": 1})
	o := NewOverlay[string, int](base, 4)

	// Setting a key to the zero value must remain observable.
	o = o.Set("z", 0)
	if v, ok := o.Get("z"); !ok || v != 0 {
		t.Fatalf("Get(z): got (%v,%v), want (0,true)", v, ok)
	}
	if o.Len() != 2 {
		t.Fatalf("Len: got %d, want 2", o.Len())
	}

	// Deleting an existing key must hide the base value.
	o = o.Delete("a")
	if _, ok := o.Get("a"); ok {
		t.Fatal("Get(a): got ok=true, want false after delete")
	}
	if o.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", o.Len())
	}

	// Deleting a non-existent key must not change the size.
	o = o.Delete("missing")
	if o.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", o.Len())
	}
}

func TestOverlay_RepeatedOverwriteAndReadd(t *testing.T) {
	base := baseOf(map[string]int{"k": 1})
	o := NewOverlay[string, int](base, 8)
	for i := 2; i <= 5; i++ {
		o = o.Set("k", i)
	}
	if v, _ := o.Get("k"); v != 5 {
		t.Fatalf("Get(k): got %v, want 5", v)
	}
	if o.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", o.Len())
	}

	// Delete then re-add across layers.
	o = o.Delete("k")
	if _, ok := o.Get("k"); ok {
		t.Fatal("Get(k): got ok=true after delete")
	}
	if o.Len() != 0 {
		t.Fatalf("Len: got %d, want 0", o.Len())
	}
	o = o.Set("k", 42)
	if v, ok := o.Get("k"); !ok || v != 42 {
		t.Fatalf("Get(k): got (%v,%v), want (42,true)", v, ok)
	}
	if o.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", o.Len())
	}
}

// collisionHasher forces every key into the same hash bucket.
type collisionHasher struct{}

func (h collisionHasher) Hash(key string) uint32 { return 0 }
func (h collisionHasher) Equal(a, b string) bool { return a == b }

func TestOverlay_HashCollision(t *testing.T) {
	base := NewMap[string, int](collisionHasher{})
	base = base.Set("a", 1).Set("b", 2).Set("c", 3)

	o := NewOverlay[string, int](base, 4)
	b := NewMapBuilder[string, OverlayEntry[int]](collisionHasher{})
	b.Set("a", SetEntry(10))
	b.Set("b", DeleteEntry[int]())
	b.Set("d", SetEntry(4))
	o = o.Apply(b.Map())
	o = o.Compact()

	ref := NewMap[string, int](collisionHasher{})
	ref = ref.Set("a", 10).Set("c", 3).Set("d", 4)

	if o.Len() != ref.Len() {
		t.Fatalf("Len: got %d, want %d", o.Len(), ref.Len())
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		gotV, gotOK := o.Get(k)
		wantV, wantOK := ref.Get(k)
		if gotV != wantV || gotOK != wantOK {
			t.Fatalf("Get(%q): got (%v,%v), want (%v,%v)", k, gotV, gotOK, wantV, wantOK)
		}
	}
}

func TestOverlay_CompactEqualsSequentialApplication(t *testing.T) {
	base := baseOf(map[string]int{"a": 1, "b": 2, "c": 3, "d": 4})
	batches := []*Map[string, OverlayEntry[int]]{
		batchOf(map[string]OverlayEntry[int]{"a": SetEntry(10), "e": SetEntry(5)}),
		batchOf(map[string]OverlayEntry[int]{"b": DeleteEntry[int](), "a": SetEntry(100)}),
		batchOf(map[string]OverlayEntry[int]{"e": DeleteEntry[int](), "f": SetEntry(6)}),
		batchOf(map[string]OverlayEntry[int]{"b": SetEntry(20), "c": SetEntry(0)}),
	}
	probes := []string{"a", "b", "c", "d", "e", "f", "g"}

	// Large enough maxLayers to keep all batches as layers.
	o := NewOverlay[string, int](base, len(batches))
	for _, batch := range batches {
		o = o.Apply(batch)
	}
	if o.Layers() != len(batches) {
		t.Fatalf("Layers: got %d, want %d", o.Layers(), len(batches))
	}

	ref := applyOps(base, batches...)
	assertEquivalent(t, o, ref, probes)

	// Compaction must not change the observable view.
	compacted := o.Compact()
	if compacted.Layers() != 0 {
		t.Fatalf("Layers after compact: got %d, want 0", compacted.Layers())
	}
	assertEquivalent(t, compacted, ref, probes)
}

func TestOverlay_CompactDoesNotMutateOldState(t *testing.T) {
	base := baseOf(map[string]int{"a": 1, "b": 2})
	b1 := batchOf(map[string]OverlayEntry[int]{"a": SetEntry(10)})
	b2 := batchOf(map[string]OverlayEntry[int]{"b": DeleteEntry[int]()})

	o0 := NewOverlay[string, int](base, 8)
	o1 := o0.Apply(b1)
	o2 := o1.Apply(b2)

	before := overlayItems(o2)
	_ = o2.Compact()

	// The pre-compaction overlay must be unchanged.
	if got := overlayItems(o2); !reflect.DeepEqual(got, before) {
		t.Fatalf("old overlay changed after compact: got %v, want %v", got, before)
	}
	if o2.Layers() != 2 {
		t.Fatalf("old overlay layers: got %d, want 2", o2.Layers())
	}

	// Intermediate snapshots must remain intact.
	if v, _ := o1.Get("a"); v != 10 {
		t.Fatalf("o1.Get(a): got %v, want 10", v)
	}
	if v, _ := o1.Get("b"); v != 2 {
		t.Fatalf("o1.Get(b): got %v, want 2", v)
	}

	// The base map must be untouched.
	if base.Len() != 2 {
		t.Fatalf("base.Len: got %d, want 2", base.Len())
	}
	if v, _ := base.Get("a"); v != 1 {
		t.Fatalf("base.Get(a): got %v, want 1", v)
	}
	if v, _ := base.Get("b"); v != 2 {
		t.Fatalf("base.Get(b): got %v, want 2", v)
	}
}

func TestOverlay_AutoCompactAtMaxLayers(t *testing.T) {
	base := baseOf(map[string]int{"k": 0})
	o := NewOverlay[string, int](base, 3)
	for i := 1; i <= 3; i++ {
		o = o.Set("k", i)
	}
	if o.Layers() != 3 {
		t.Fatalf("Layers: got %d, want 3", o.Layers())
	}

	// The next apply exceeds the limit and triggers compaction.
	o = o.Set("k", 4)
	if o.Layers() != 0 {
		t.Fatalf("Layers after auto-compact: got %d, want 0", o.Layers())
	}
	if v, _ := o.Get("k"); v != 4 {
		t.Fatalf("Get(k): got %v, want 4", v)
	}
}

// countingHasher counts Hash invocations to measure read cost.
type countingHasher struct {
	inner Hasher[string]
	n     int
}

func (h *countingHasher) Hash(key string) uint32 { h.n++; return h.inner.Hash(key) }
func (h *countingHasher) Equal(a, b string) bool { return h.inner.Equal(a, b) }

func TestOverlay_ReadCostBoundedByLayerCount(t *testing.T) {
	hasher := &countingHasher{inner: NewHasher("")}
	base := NewMap[string, int](hasher).Set("base", 1)

	const maxLayers = 3
	o := NewOverlay[string, int](base, maxLayers)
	for i := 0; i < maxLayers; i++ {
		b := NewMapBuilder[string, OverlayEntry[int]](hasher)
		b.Set(fmt.Sprintf("key%d", i), SetEntry(i))
		o = o.Apply(b.Map())
	}

	// Worst case: a key present in no layer and not in the base must probe
	// every layer plus the base exactly once.
	hasher.n = 0
	if _, ok := o.Get("missing"); ok {
		t.Fatal("Get(missing): got ok=true")
	}
	if got, want := hasher.n, maxLayers+1; got != want {
		t.Fatalf("hashes for missing key: got %d, want %d (layers+base)", got, want)
	}

	// A hit in the newest layer costs a single probe.
	hasher.n = 0
	if _, ok := o.Get("key2"); !ok {
		t.Fatal("Get(key2): got ok=false")
	}
	if got := hasher.n; got != 1 {
		t.Fatalf("hashes for newest-layer hit: got %d, want 1", got)
	}

	// Exceeding maxLayers compacts, restoring single-probe worst case.
	b := NewMapBuilder[string, OverlayEntry[int]](hasher)
	b.Set("extra", SetEntry(99))
	o = o.Apply(b.Map())
	if o.Layers() != 0 {
		t.Fatalf("Layers after auto-compact: got %d, want 0", o.Layers())
	}
	hasher.n = 0
	if _, ok := o.Get("missing"); ok {
		t.Fatal("Get(missing): got ok=true after compact")
	}
	if got := hasher.n; got != 1 {
		t.Fatalf("hashes after compact: got %d, want 1", got)
	}
}

func TestOverlay_IteratorEmptyAndTombstoneOnly(t *testing.T) {
	base := baseOf(map[string]int{"a": 1})
	o := NewOverlay[string, int](base, 4).Delete("a")
	if o.Len() != 0 {
		t.Fatalf("Len: got %d, want 0", o.Len())
	}
	itr := o.Iterator()
	if !itr.Done() {
		t.Fatal("iterator over fully deleted view should be done")
	}
	if _, _, ok := itr.Next(); ok {
		t.Fatal("Next on exhausted iterator should return ok=false")
	}
}
