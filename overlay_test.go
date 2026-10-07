package immutable

import (
	"math/rand"
	"strconv"
	"testing"
)

// intHasher hashes ints the same way the library's default int hasher does.
func intHash(v int) uint32 {
	hash := uint64(v)
	value := hash
	for value > 0xffffffff {
		value /= 0xffffffff
		hash ^= value
	}
	return uint32(hash)
}

// countingHasher counts Hash/Equal invocations so read costs can be verified by
// operation counting rather than by timing.
type countingHasher struct {
	hashCalls  int
	equalCalls int
}

func (h *countingHasher) Hash(key int) uint32 {
	h.hashCalls++
	return intHash(key)
}

func (h *countingHasher) Equal(a, b int) bool {
	h.equalCalls++
	return a == b
}

// collisionHasher forces every key into the same 32-bit hash bucket, exercising
// mapHashCollisionNode handling under the overlay.
type collisionHasher struct {
	equalCalls int
}

func (h *collisionHasher) Hash(key int) uint32 { return 0 }

func (h *collisionHasher) Equal(a, b int) bool {
	h.equalCalls++
	return a == b
}

func overlayToMap(o *Overlay[int, int]) map[int]int {
	got := make(map[int]int)
	itr := o.Iterator()
	for !itr.Done() {
		k, v, ok := itr.Next()
		if !ok {
			break
		}
		got[k] = v
	}
	return got
}

func assertOverlayEquals(t *testing.T, o *Overlay[int, int], want map[int]int, msg string) {
	t.Helper()
	if o.Len() != len(want) {
		t.Fatalf("%s: Len=%d, want %d (%v)", msg, o.Len(), len(want), want)
	}
	for k, v := range want {
		got, ok := o.Get(k)
		if !ok {
			t.Fatalf("%s: Get(%d) missing, want %d", msg, k, v)
		}
		if got != v {
			t.Fatalf("%s: Get(%d)=%d, want %d", msg, k, got, v)
		}
	}
	got := overlayToMap(o)
	if len(got) != len(want) {
		t.Fatalf("%s: iterator has %d entries, want %d", msg, len(got), len(want))
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Fatalf("%s: iterator entry %d=%v (ok=%v), want %d", msg, k, g, ok, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Fatalf("%s: iterator has unexpected key %d", msg, k)
		}
	}
}

func TestOverlayTombstoneVsZeroAndShadow(t *testing.T) {
	base := NewMap[int, int](nil)
	base = base.Set(1, 10)
	base = base.Set(2, 20)
	base = base.Set(3, 30)

	o := NewOverlay[int, int](nil, base, 2)

	// Layer 1: overwrite with zero value (must remain present), delete a key,
	// and add a new key.
	w := o.NewWriter()
	w.Set(2, 0)
	w.Delete(3)
	w.Set(4, 40)
	o = w.Commit()

	want := map[int]int{1: 10, 2: 0, 4: 40}
	assertOverlayEquals(t, o, want, "layer1")
	if o.Layers() != 1 {
		t.Fatalf("expected 1 stacked layer, got %d", o.Layers())
	}

	if v, ok := o.Get(3); ok {
		t.Fatalf("deleted key reported present with value %d", v)
	}

	// The second batch would reach 2 layers, so it is folded into a new base
	// (compaction) instead of being stacked. It re-adds the deleted key and
	// overwrites existing keys.
	preCompact := o
	w = o.NewWriter()
	w.Set(3, 300)
	w.Set(2, 200)
	w.Set(4, 400)
	o = w.Commit()

	if o.Layers() != 0 {
		t.Fatalf("expected compaction to reset layers, got %d", o.Layers())
	}
	want = map[int]int{1: 10, 2: 200, 3: 300, 4: 400}
	assertOverlayEquals(t, o, want, "compacted")

	// New batches stack again on top of the compacted base.
	w = o.NewWriter()
	w.Delete(1)
	w.Set(5, 50)
	o = w.Commit()
	if o.Layers() != 1 {
		t.Fatalf("expected 1 layer after compaction, got %d", o.Layers())
	}
	want = map[int]int{2: 200, 3: 300, 4: 400, 5: 50}
	assertOverlayEquals(t, o, want, "post-compaction layer")

	// Old snapshot and old base must be unchanged by compaction.
	assertOverlayEquals(t, preCompact, map[int]int{1: 10, 2: 0, 4: 40}, "old snapshot")
	if v, ok := base.Get(3); !ok || v != 30 {
		t.Fatalf("old base mutated: Get(3)=%d,%v", v, ok)
	}
	if base.Len() != 3 {
		t.Fatalf("old base size mutated: %d", base.Len())
	}
}

func TestOverlayBatchCollapse(t *testing.T) {
	base := NewMap[int, int](nil).Set(1, 10)
	o := NewOverlay[int, int](nil, base, 4)

	// Set then delete within a batch: only tombstone survives, key disappears.
	w := o.NewWriter()
	w.Set(2, 20)
	w.Delete(2)
	o = w.Commit()
	if _, ok := o.Get(2); ok {
		t.Fatal("set-then-delete in batch should hide key")
	}

	// Delete then re-add within a batch: key reappears with final value.
	w = o.NewWriter()
	w.Delete(1)
	w.Set(1, 11)
	o = w.Commit()
	if v, ok := o.Get(1); !ok || v != 11 {
		t.Fatalf("delete-then-set failed: %d,%v", v, ok)
	}

	// Repeated overwrite collapses to the final value and counts as one key.
	w = o.NewWriter()
	for i := 0; i < 10; i++ {
		w.Set(7, i)
	}
	o = w.Commit()
	assertOverlayEquals(t, o, map[int]int{1: 11, 7: 9}, "batch collapse")

	// An empty commit is a no-op returning the same overlay.
	if o.NewWriter().Commit() != o {
		t.Fatal("empty commit should return receiver")
	}
}

func TestOverlayRandomized(t *testing.T) {
	const (
		seed      = 42
		rounds    = 500
		keyRange  = 25
		maxLayers = 3
		baseKeys  = 10
	)

	run := func(t *testing.T, hasher Hasher[int], label string) {
		t.Helper()
		rng := rand.New(rand.NewSource(seed))

		builder := NewMapBuilder[int, int](hasher)
		want := make(map[int]int)
		for i := 0; i < baseKeys; i++ {
			builder.Set(i, i*100)
			want[i] = i * 100
		}
		o := NewOverlay[int, int](hasher, builder.Map(), maxLayers)

		type snapshot struct {
			o    *Overlay[int, int]
			want map[int]int
		}
		var snapshots []snapshot

		for r := 0; r < rounds; r++ {
			w := o.NewWriter()
			batch := 1 + rng.Intn(4)
			for b := 0; b < batch; b++ {
				key := rng.Intn(keyRange)
				if rng.Intn(3) == 0 {
					w.Delete(key)
					delete(want, key)
				} else {
					val := rng.Intn(1000)
					w.Set(key, val)
					want[key] = val
				}
			}
			o = w.Commit()
			if o.Layers() > maxLayers-1 {
				t.Fatalf("%s round %d: layers=%d exceeds maxLayers-1=%d",
					label, r, o.Layers(), maxLayers-1)
			}
			assertOverlayEquals(t, o, want, label+" live")

			cp := make(map[int]int, len(want))
			for k, v := range want {
				cp[k] = v
			}
			snapshots = append(snapshots, snapshot{o: o, want: cp})
		}

		// All historical snapshots must remain readable and unchanged even
		// though later commits compacted layers away.
		for i, s := range snapshots {
			assertOverlayEquals(t, s.o, s.want, label+" snapshot "+strconv.Itoa(i))
		}
	}

	run(t, nil, "default-hasher")
	run(t, &collisionHasher{}, "all-hash-collisions")
}

func TestOverlayReadOpCountBoundedByLayers(t *testing.T) {
	h := &countingHasher{}

	builder := NewMapBuilder[int, int](h)
	builder.Set(1, 10)
	builder.Set(2, 20)
	o := NewOverlay[int, int](h, builder.Map(), 3)

	// Stack two layers without triggering compaction (threshold is 3).
	w := o.NewWriter()
	w.Set(3, 30)
	o = w.Commit()
	w = o.NewWriter()
	w.Set(4, 40)
	o = w.Commit()

	if o.Layers() != 2 {
		t.Fatalf("expected 2 stacked layers, got %d", o.Layers())
	}

	// Key missing everywhere: probe both layers then the base == 3 hashes,
	// i.e. at most maxLayers layer probes + 1 base probe.
	h.hashCalls = 0
	if _, ok := o.Get(99); ok {
		t.Fatal("unexpected hit")
	}
	if h.hashCalls != 3 {
		t.Fatalf("miss: hashCalls=%d, want 3 (2 layers + base)", h.hashCalls)
	}

	// Key only in the base: same worst-case count.
	h.hashCalls = 0
	if v, ok := o.Get(1); !ok || v != 10 {
		t.Fatalf("base lookup wrong: %d,%v", v, ok)
	}
	if h.hashCalls != 3 {
		t.Fatalf("base hit: hashCalls=%d, want 3", h.hashCalls)
	}

	// Key in the newest layer: a single probe short-circuits.
	h.hashCalls = 0
	if v, ok := o.Get(4); !ok || v != 40 {
		t.Fatalf("top-layer lookup wrong: %d,%v", v, ok)
	}
	if h.hashCalls != 1 {
		t.Fatalf("top-layer hit: hashCalls=%d, want 1", h.hashCalls)
	}

	// Commit a third batch to force compaction, then the read collapses to a
	// single base probe regardless of how many updates were applied.
	w = o.NewWriter()
	w.Set(5, 50)
	o = w.Commit()
	if o.Layers() != 0 {
		t.Fatalf("expected compaction, layers=%d", o.Layers())
	}
	h.hashCalls = 0
	if _, ok := o.Get(99); ok {
		t.Fatal("unexpected hit")
	}
	if h.hashCalls != 1 {
		t.Fatalf("post-compaction miss: hashCalls=%d, want 1", h.hashCalls)
	}
}

func TestOverlayHashCollisionEquality(t *testing.T) {
	h := &collisionHasher{}
	builder := NewMapBuilder[int, int](h)
	for i := 0; i < 20; i++ {
		builder.Set(i, i)
	}
	o := NewOverlay[int, int](h, builder.Map(), 2)

	// Overwrite and delete keys that all share one hash bucket, then compact.
	w := o.NewWriter()
	w.Set(1, 101)
	w.Set(2, 102)
	w.Delete(3)
	o = w.Commit()
	w = o.NewWriter()
	w.Set(3, 303) // re-add after tombstone
	w.Delete(10)
	o = w.Commit()

	if o.Layers() != 0 {
		t.Fatalf("expected compaction, got layers=%d", o.Layers())
	}
	want := make(map[int]int)
	for i := 0; i < 20; i++ {
		want[i] = i
	}
	want[1] = 101
	want[2] = 102
	want[3] = 303
	delete(want, 10)
	assertOverlayEquals(t, o, want, "collision")

	if h.equalCalls == 0 {
		t.Fatal("expected collision-node equality probes to occur")
	}
}

func TestOverlayNewPanics(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for maxLayers < 1")
			}
		}()
		NewOverlay[int, int](nil, nil, 0)
	}()

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for nil hasher and nil base")
			}
		}()
		NewOverlay[int, int](nil, nil, 2)
	}()
}

func TestOverlayNilBaseHasherInferred(t *testing.T) {
	base := NewMap[int, int](nil).Set(1, 10)
	o := NewOverlay[int, int](nil, base, 2) // hasher inferred from base
	o = o.Set(1, 11).Delete(1)
	if _, ok := o.Get(1); ok {
		t.Fatal("expected deleted key to be absent")
	}
}

func TestOverlayEmptyBase(t *testing.T) {
	o := NewOverlay[int, string](&defaultHasher[int]{}, nil, 2)
	if o.Len() != 0 {
		t.Fatalf("empty overlay Len=%d", o.Len())
	}
	if _, ok := o.Get(1); ok {
		t.Fatal("empty overlay returned a value")
	}

	// Zero-value string is a legitimate value and must not be treated as
	// missing; only a tombstone means missing.
	o = o.Set(1, "")
	if v, ok := o.Get(1); !ok || v != "" {
		t.Fatalf("zero-value key missing or wrong: %q,%v", v, ok)
	}
	if o.Len() != 1 {
		t.Fatalf("Len=%d, want 1", o.Len())
	}

	o = o.Set(2, "two").Delete(1)
	if o.Len() != 1 {
		t.Fatalf("Len=%d, want 1", o.Len())
	}
	if v, ok := o.Get(2); !ok || v != "two" {
		t.Fatalf("Get(2)=%q,%v", v, ok)
	}
	if _, ok := o.Get(1); ok {
		t.Fatal("tombstoned key still visible")
	}

	// Iterating a compacted overlay built from a nil base must not panic.
	w := o.NewWriter()
	w.Set(3, "three")
	o = w.Commit() // triggers compaction (layers would reach 2)
	if o.Layers() != 0 {
		t.Fatalf("layers=%d, want 0", o.Layers())
	}
	if o.Len() != 2 {
		t.Fatalf("compacted Len=%d, want 2", o.Len())
	}
	count := 0
	itr := o.Iterator()
	for !itr.Done() {
		itr.Next()
		count++
	}
	if count != 2 {
		t.Fatalf("compacted iterator yielded %d entries, want 2", count)
	}
}
