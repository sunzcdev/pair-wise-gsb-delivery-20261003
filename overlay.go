package immutable

// Overlay is an immutable, read-optimized view of a base Map with one or more
// thin update layers stacked on top. It is intended for read-heavy caches
// where small batches of updates arrive frequently and rebuilding the large
// base Map on every batch would be too expensive.
//
// Reads consult the update layers first (newest first) and then the base Map.
// A layer can record an explicit deletion via a tombstone so that a deleted
// key is distinguishable from a key whose value is the zero value of V.
//
// Layers are immutable: Set, Delete, and Writer.Commit always return a new
// Overlay and never modify the receiver, any existing layer, or the base Map.
// Compaction folds the stacked layers (plus the pending batch) into a new base
// Map using path-copying updates. The old base Map is left untouched, so any
// Overlay that references it remains a valid snapshot.
//
// Compaction is synchronous and runs on the goroutine that calls
// Writer.Commit. There is no background compaction goroutine.
type Overlay[K, V any] struct {
	base      *Map[K, V]                 // compacted, read-mostly data
	layers    []*Map[K, overlayEntry[V]] // pending layers, oldest first
	size      int                        // effective number of live keys
	hasher    Hasher[K]
	maxLayers int // caller-specified cap on the number of stacked layers
}

// overlayEntry records either a live value or a tombstone (tomb == true).
// The tombstone flag is what separates a deleted key from a key set to the
// zero value of V.
type overlayEntry[V any] struct {
	value V
	tomb  bool
}

// NewOverlay returns an Overlay backed by base. base may be nil, in which
// case an empty base is created using hasher. hasher may be nil only when
// base already carries a hasher (i.e. base is non-nil and has had a key set);
// the base's hasher is then reused.
//
// maxLayers bounds the worst-case read cost: after a commit at most
// maxLayers-1 update layers are stacked, so a Get performs at most
// maxLayers-1 layer probes plus one base probe, i.e. maxLayers HAMT probes in
// total and never more. A batch whose stacking would reach maxLayers is
// instead folded into the base (compaction). maxLayers must be at least 1.
func NewOverlay[K, V any](hasher Hasher[K], base *Map[K, V], maxLayers int) *Overlay[K, V] {
	if maxLayers < 1 {
		panic("immutable.NewOverlay(): maxLayers must be at least 1")
	}
	if base == nil {
		if hasher == nil {
			panic("immutable.NewOverlay(): hasher required when base is nil")
		}
		base = NewMap[K, V](hasher)
	} else if hasher == nil {
		if base.hasher == nil {
			panic("immutable.NewOverlay(): hasher required when base has no hasher")
		}
		hasher = base.hasher
	}
	return &Overlay[K, V]{
		base:      base,
		size:      base.Len(),
		hasher:    hasher,
		maxLayers: maxLayers,
	}
}

// Get returns the effective value for key and whether the key exists. Layers
// are checked from newest to oldest; a tombstone reports a missing key even if
// the key exists in an older layer or the base. If no layer records the key,
// the base is consulted.
//
// The cost is at most maxLayers-1 layer lookups plus one base lookup
// (maxLayers HAMT probes in total). Each lookup is a HAMT probe and the cost
// never depends on the total number of updates ever applied, only on the
// number of stacked layers.
func (o *Overlay[K, V]) Get(key K) (value V, ok bool) {
	for i := len(o.layers) - 1; i >= 0; i-- {
		if entry, found := o.layers[i].Get(key); found {
			return entry.value, !entry.tomb
		}
	}
	return o.base.Get(key)
}

// Len returns the effective number of key/value pairs after all layers have
// been applied, i.e. the same result as applying every update to the base one
// item at a time. The result is maintained incrementally.
func (o *Overlay[K, V]) Len() int {
	return o.size
}

// Layers returns the number of update layers currently stacked on top of the
// base. It is always in the range [0, maxLayers-1] after a commit: a batch
// committed when Layers() == maxLayers-1 is folded into the base (compaction)
// and Layers() resets to 0.
func (o *Overlay[K, V]) Layers() int {
	return len(o.layers)
}

// MaxLayers returns the caller-specified compaction threshold.
func (o *Overlay[K, V]) MaxLayers() int {
	return o.maxLayers
}

// Base returns the current compacted base Map. It is the same Map passed to
// NewOverlay until the first compaction; compaction replaces it with a new Map
// without mutating the previous one.
func (o *Overlay[K, V]) Base() *Map[K, V] {
	return o.base
}

// Set returns a new Overlay with a one-key batch setting key to value. For
// batching several keys use NewWriter.
func (o *Overlay[K, V]) Set(key K, value V) *Overlay[K, V] {
	w := o.NewWriter()
	w.Set(key, value)
	return w.Commit()
}

// Delete returns a new Overlay with a one-key batch recording a tombstone for
// key. For batching several keys use NewWriter.
func (o *Overlay[K, V]) Delete(key K) *Overlay[K, V] {
	w := o.NewWriter()
	w.Delete(key)
	return w.Commit()
}

// NewWriter returns a Writer for accumulating one update batch. Repeated
// writes to the same key within a batch collapse to the final state (last
// write wins), so a Set followed by a Delete produces only a tombstone and a
// Delete followed by a Set re-adds the key.
func (o *Overlay[K, V]) NewWriter() *OverlayWriter[K, V] {
	return &OverlayWriter[K, V]{
		o:       o,
		pending: NewMapBuilder[K, overlayEntry[V]](o.hasher),
	}
}

// commit applies pendingLayer (the finalized batch) to o. If the layer stack
// is already full the batch is folded into a new base together with all
// existing layers (compaction); otherwise it is pushed as one new layer.
func (o *Overlay[K, V]) commit(pendingLayer *Map[K, overlayEntry[V]]) *Overlay[K, V] {
	if pendingLayer == nil || pendingLayer.Len() == 0 {
		return o
	}

	if len(o.layers) >= o.maxLayers-1 {
		return o.compact(pendingLayer)
	}

	// Fast path: stack the batch as a new layer and adjust the cached size by
	// the net effect each pending key has on its previous effective state.
	other := &Overlay[K, V]{
		base:      o.base,
		layers:    make([]*Map[K, overlayEntry[V]], len(o.layers)+1),
		size:      o.size,
		hasher:    o.hasher,
		maxLayers: o.maxLayers,
	}
	copy(other.layers, o.layers)
	other.layers[len(o.layers)] = pendingLayer

	itr := pendingLayer.Iterator()
	for !itr.Done() {
		key, entry, _ := itr.Next()
		_, existed := o.lookup(key)
		if !entry.tomb && !existed {
			other.size++
		} else if entry.tomb && existed {
			other.size--
		}
	}
	return other
}

// lookup returns the effective state of key before any new batch: layers
// oldest to newest (so the newest match wins) and then the base.
func (o *Overlay[K, V]) lookup(key K) (value V, ok bool) {
	for i := len(o.layers) - 1; i >= 0; i-- {
		if entry, found := o.layers[i].Get(key); found {
			return entry.value, !entry.tomb
		}
	}
	return o.base.Get(key)
}

// compact folds the stacked layers and pendingLayer into a copy of the base.
// The previous base and layers are never mutated: base.Set/Delete return new
// Maps via path copying. Only keys touched by the layers are rewritten, so the
// cost is proportional to the number of distinct touched keys times a single
// HAMT update, never a full rebuild of the base.
func (o *Overlay[K, V]) compact(pendingLayer *Map[K, overlayEntry[V]]) *Overlay[K, V] {
	// Collapse the layers oldest-to-newest, then the pending batch, into the
	// final entry per key.
	final := NewMapBuilder[K, overlayEntry[V]](o.hasher)
	collect := func(layer *Map[K, overlayEntry[V]]) {
		itr := layer.Iterator()
		for !itr.Done() {
			key, entry, _ := itr.Next()
			final.Set(key, entry)
		}
	}
	for _, layer := range o.layers {
		collect(layer)
	}
	collect(pendingLayer)

	base := o.base
	finalItr := final.Iterator()
	for !finalItr.Done() {
		key, entry, _ := finalItr.Next()
		if entry.tomb {
			base = base.Delete(key)
		} else {
			base = base.Set(key, entry.value)
		}
	}

	return &Overlay[K, V]{
		base:      base,
		layers:    nil,
		size:      base.Len(),
		hasher:    o.hasher,
		maxLayers: o.maxLayers,
	}
}

// Iterator returns a new iterator over the effective key/value pairs, i.e. the
// pairs that would exist after applying every layer to the base one item at a
// time. Order is unspecified, matching Map iteration.
func (o *Overlay[K, V]) Iterator() *OverlayIterator[K, V] {
	itr := &OverlayIterator[K, V]{}
	itr.First(o)
	return itr
}

// OverlayWriter accumulates one immutable update batch against an Overlay.
type OverlayWriter[K, V any] struct {
	o       *Overlay[K, V]
	pending *MapBuilder[K, overlayEntry[V]]
	written bool
}

// Set stages key = value in the batch.
func (w *OverlayWriter[K, V]) Set(key K, value V) {
	w.pending.Set(key, overlayEntry[V]{value: value})
	w.written = true
}

// Delete stages an explicit deletion (tombstone) of key in the batch.
func (w *OverlayWriter[K, V]) Delete(key K) {
	w.pending.Set(key, overlayEntry[V]{tomb: true})
	w.written = true
}

// Commit finalizes the batch and returns the resulting Overlay. The receiver
// Overlay, its layers, and its base are not modified. An empty batch returns
// the receiver unchanged. The writer must not be reused after Commit.
func (w *OverlayWriter[K, V]) Commit() *Overlay[K, V] {
	if !w.written {
		return w.o
	}
	layer := w.pending.Map()
	w.written = false
	return w.o.commit(layer)
}

// OverlayIterator represents an iterator over the effective contents of an
// Overlay.
type OverlayIterator[K, V any] struct {
	entries []mapEntry[K, V]
	index   int
}

// First repositions the iterator at the first effective entry of o.
func (itr *OverlayIterator[K, V]) First(o *Overlay[K, V]) {
	// Final entry per key across all layers, newest layer winning. Iterating
	// newest-to-oldest and keeping the first match yields the newest state.
	latest := NewMapBuilder[K, overlayEntry[V]](o.hasher)
	for i := len(o.layers) - 1; i >= 0; i-- {
		layerItr := o.layers[i].Iterator()
		for !layerItr.Done() {
			key, entry, _ := layerItr.Next()
			if _, seen := latest.Get(key); !seen {
				latest.Set(key, entry)
			}
		}
	}

	itr.entries = itr.entries[:0]

	// Emit base entries unless a layer shadows them; a shadowing tombstone
	// hides the base entry.
	baseItr := o.base.Iterator()
	for !baseItr.Done() {
		key, value, _ := baseItr.Next()
		if entry, shadowed := latest.Get(key); shadowed {
			if !entry.tomb {
				itr.entries = append(itr.entries, mapEntry[K, V]{key: key, value: entry.value})
			}
			continue
		}
		itr.entries = append(itr.entries, mapEntry[K, V]{key: key, value: value})
	}

	// Emit live layer-only keys (keys absent from the base).
	latestItr := latest.Iterator()
	for !latestItr.Done() {
		key, entry, _ := latestItr.Next()
		if entry.tomb {
			continue
		}
		if _, inBase := o.base.Get(key); inBase {
			continue
		}
		itr.entries = append(itr.entries, mapEntry[K, V]{key: key, value: entry.value})
	}

	itr.index = 0
}

// Done returns true when no more entries remain.
func (itr *OverlayIterator[K, V]) Done() bool {
	return itr.index >= len(itr.entries)
}

// Next returns the current key/value pair and advances the iterator.
func (itr *OverlayIterator[K, V]) Next() (key K, value V, ok bool) {
	if itr.Done() {
		return key, value, false
	}
	entry := itr.entries[itr.index]
	itr.index++
	return entry.key, entry.value, true
}
