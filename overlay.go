package immutable

// OverlayEntry represents a single update within an overlay layer. An entry is
// either an assignment of a value to a key or an explicit deletion of the key.
//
// The Deleted flag acts as a tombstone so that deletions are never confused
// with the zero value of V: setting a key to its zero value and deleting the
// key are distinct, observable operations.
type OverlayEntry[V any] struct {
	Value   V    // value assigned to the key (ignored when Deleted is true)
	Deleted bool // tombstone: marks the key as removed from the merged view
}

// SetEntry returns an overlay entry that assigns value to a key.
func SetEntry[V any](value V) OverlayEntry[V] {
	return OverlayEntry[V]{Value: value}
}

// DeleteEntry returns an overlay entry that deletes a key.
func DeleteEntry[V any]() OverlayEntry[V] {
	return OverlayEntry[V]{Deleted: true}
}

// Overlay is an immutable read-optimized view over a large base map. Small
// batches of updates are stacked as layers on top of the base instead of
// rebuilding the base map on every change. Reads consult the layers from
// newest to oldest and fall through to the base map.
//
// The number of layers is bounded by a caller-specified maximum. When a
// applied batch would exceed that maximum, the overlay is compacted: all
// layers are folded into a new base map. Because the layer count never
// exceeds maxLayers, a Get() performs at most maxLayers+1 map lookups in the
// worst case (one per layer plus one for the base). Compaction happens
// synchronously inside Apply(); no background goroutines are involved.
//
// Overlay is immutable: Apply() and Compact() return new Overlay instances
// and never modify the receiver, its layers, or its base map. Existing
// instances remain valid snapshots and are safe for concurrent reads.
type Overlay[K, V any] struct {
	base      *Map[K, V]                 // underlying compacted map
	layers    []*Map[K, OverlayEntry[V]] // update layers, oldest to newest
	size      int                        // number of keys in the merged view
	maxLayers int                        // maximum number of layers before compaction
}

// NewOverlay returns a new Overlay backed by base. If base is nil, an empty
// map is used. The maxLayers argument bounds how many update layers may stack
// before the overlay is compacted; values below 1 are treated as 1.
func NewOverlay[K, V any](base *Map[K, V], maxLayers int) *Overlay[K, V] {
	if base == nil {
		base = NewMap[K, V](nil)
	}
	if maxLayers < 1 {
		maxLayers = 1
	}
	return &Overlay[K, V]{
		base:      base,
		size:      base.Len(),
		maxLayers: maxLayers,
	}
}

// Len returns the number of keys visible in the merged view.
func (o *Overlay[K, V]) Len() int {
	return o.size
}

// Layers returns the current number of uncompacted update layers.
func (o *Overlay[K, V]) Layers() int {
	return len(o.layers)
}

// MaxLayers returns the maximum number of layers allowed before compaction.
func (o *Overlay[K, V]) MaxLayers() int {
	return o.maxLayers
}

// Get returns the value for the given key from the merged view. Layers are
// consulted from newest to oldest, then the base map. A tombstone entry hides
// any older value, including values stored in the base map.
func (o *Overlay[K, V]) Get(key K) (value V, ok bool) {
	if entry, ok := o.lookupLayer(key); ok {
		if entry.Deleted {
			return value, false
		}
		return entry.Value, true
	}
	return o.base.Get(key)
}

// lookupLayer returns the newest layer entry mentioning key, if any.
func (o *Overlay[K, V]) lookupLayer(key K) (OverlayEntry[V], bool) {
	for i := len(o.layers) - 1; i >= 0; i-- {
		if entry, ok := o.layers[i].Get(key); ok {
			return entry, true
		}
	}
	return OverlayEntry[V]{}, false
}

// Set returns a new overlay with a single-entry layer assigning value to key.
func (o *Overlay[K, V]) Set(key K, value V) *Overlay[K, V] {
	b := NewMapBuilder[K, OverlayEntry[V]](o.base.hasher)
	b.Set(key, SetEntry(value))
	return o.Apply(b.Map())
}

// Delete returns a new overlay with a single-entry layer deleting key.
func (o *Overlay[K, V]) Delete(key K) *Overlay[K, V] {
	b := NewMapBuilder[K, OverlayEntry[V]](o.base.hasher)
	b.Set(key, DeleteEntry[V]())
	return o.Apply(b.Map())
}

// Apply returns a new overlay with batch stacked as the newest layer. The
// batch maps keys to their updates: use SetEntry() to assign a value and
// DeleteEntry() to remove a key.
//
// If stacking the layer would exceed the maximum layer count, the overlay is
// compacted synchronously and the returned overlay has no layers.
func (o *Overlay[K, V]) Apply(batch *Map[K, OverlayEntry[V]]) *Overlay[K, V] {
	if batch == nil || batch.Len() == 0 {
		return o
	}

	// Compute the merged size incrementally by resolving each updated key
	// against the current view.
	size := o.size
	itr := batch.Iterator()
	for !itr.Done() {
		key, entry, _ := itr.Next()
		_, exists := o.Get(key)
		if entry.Deleted {
			if exists {
				size--
			}
		} else if !exists {
			size++
		}
	}

	other := &Overlay[K, V]{
		base:      o.base,
		layers:    append(append(make([]*Map[K, OverlayEntry[V]], 0, len(o.layers)+1), o.layers...), batch),
		size:      size,
		maxLayers: o.maxLayers,
	}
	if len(other.layers) > other.maxLayers {
		return other.Compact()
	}
	return other
}

// Compact returns a new overlay with all layers folded into a new base map.
// The result is equivalent to applying every layer's updates, oldest to
// newest, to the base map. The receiver, its layers, and its base map are
// left unmodified.
func (o *Overlay[K, V]) Compact() *Overlay[K, V] {
	if len(o.layers) == 0 {
		return o
	}

	b := NewMapBuilder[K, V](o.base.hasher)
	itr := o.base.Iterator()
	for !itr.Done() {
		key, value, _ := itr.Next()
		b.Set(key, value)
	}
	for _, layer := range o.layers {
		layerItr := layer.Iterator()
		for !layerItr.Done() {
			key, entry, _ := layerItr.Next()
			if entry.Deleted {
				b.Delete(key)
			} else {
				b.Set(key, entry.Value)
			}
		}
	}

	return &Overlay[K, V]{
		base:      b.Map(),
		size:      o.size,
		maxLayers: o.maxLayers,
	}
}

// Iterator returns a new iterator over the merged view positioned at the
// first key. Keys shadowed or deleted by newer layers are skipped and each
// key is yielded exactly once with its newest visible value.
func (o *Overlay[K, V]) Iterator() *OverlayIterator[K, V] {
	itr := &OverlayIterator[K, V]{overlay: o}
	itr.First()
	return itr
}

// OverlayIterator represents an iterator over an Overlay's merged view.
// Iteration order is deterministic but not sorted.
type OverlayIterator[K, V any] struct {
	overlay *Overlay[K, V]

	baseItr  *MapIterator[K, V]               // iterator over the base map
	layerIdx int                              // current layer index, newest to oldest
	layerItr *MapIterator[K, OverlayEntry[V]] // iterator over the current layer
	seen     *Map[K, struct{}]                // keys already yielded or shadowed

	pendingKey   K    // next key to yield
	pendingValue V    // next value to yield
	hasPending   bool // true when a next key/value pair is available
}

// Done returns true if no more key/value pairs remain in the iterator.
func (itr *OverlayIterator[K, V]) Done() bool {
	return !itr.hasPending
}

// First positions the iterator on the first key/value pair.
func (itr *OverlayIterator[K, V]) First() {
	o := itr.overlay
	itr.baseItr = o.base.Iterator()
	itr.layerIdx = len(o.layers) - 1
	itr.layerItr = nil
	itr.seen = NewMap[K, struct{}](o.base.hasher)
	itr.advance()
}

// Next returns the current key/value pair and moves the iterator forward.
// Returns ok as false when no more pairs remain.
func (itr *OverlayIterator[K, V]) Next() (key K, value V, ok bool) {
	if !itr.hasPending {
		return key, value, false
	}
	key, value = itr.pendingKey, itr.pendingValue
	itr.advance()
	return key, value, true
}

// advance fetches the next visible key/value pair into the pending slot.
func (itr *OverlayIterator[K, V]) advance() {
	itr.hasPending = false

	// Yield base entries first, applying the newest layer entry for each key.
	for !itr.baseItr.Done() {
		k, baseValue, _ := itr.baseItr.Next()
		entry, found := itr.overlay.lookupLayer(k)
		if !found {
			itr.pendingKey, itr.pendingValue, itr.hasPending = k, baseValue, true
			return
		}
		itr.seen = itr.seen.Set(k, struct{}{})
		if !entry.Deleted {
			itr.pendingKey, itr.pendingValue, itr.hasPending = k, entry.Value, true
			return
		}
	}

	// Then yield layer entries not already seen, newest layers first. Every
	// mentioned key is marked as seen, including tombstones, so that older
	// layers cannot resurrect a deleted or overwritten key.
	for itr.layerIdx >= 0 {
		if itr.layerItr == nil {
			itr.layerItr = itr.overlay.layers[itr.layerIdx].Iterator()
		}
		for !itr.layerItr.Done() {
			k, entry, _ := itr.layerItr.Next()
			if _, exists := itr.seen.Get(k); exists {
				continue
			}
			itr.seen = itr.seen.Set(k, struct{}{})
			if entry.Deleted {
				continue
			}
			itr.pendingKey, itr.pendingValue, itr.hasPending = k, entry.Value, true
			return
		}
		itr.layerItr = nil
		itr.layerIdx--
	}
}
