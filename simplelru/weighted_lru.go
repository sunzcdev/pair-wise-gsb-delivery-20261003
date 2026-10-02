// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package simplelru

import (
	"errors"

	"github.com/hashicorp/golang-lru/v2/internal"
)

// WeightedEvictCallback is used to get a callback when a cache entry is evicted
// from a weight-budgeted cache.
type WeightedEvictCallback[K comparable, V any] func(key K, value V)

// WeightedLRU implements a non-thread safe LRU cache whose total entry
// weights must not exceed a fixed budget.
//
// Every entry carries a caller-provided, non-negative weight. Adding an
// entry evicts the least recently used entries until the aggregate weight
// fits the budget. A single entry whose weight is greater than the budget
// is never admitted.
type WeightedLRU[K comparable, V any] struct {
	maxWeight int64
	weight    int64
	evictList *internal.LruList[K, V]
	items     map[K]*internal.Entry[K, V]
	onEvict   WeightedEvictCallback[K, V]
}

// NewWeightedLRU constructs a weight-budgeted LRU with the given maximum
// total weight.
func NewWeightedLRU[K comparable, V any](maxWeight int64, onEvict WeightedEvictCallback[K, V]) (*WeightedLRU[K, V], error) {
	if maxWeight <= 0 {
		return nil, errors.New("must provide a positive max weight")
	}

	c := &WeightedLRU[K, V]{
		maxWeight: maxWeight,
		evictList: internal.NewList[K, V](),
		items:     make(map[K]*internal.Entry[K, V]),
		onEvict:   onEvict,
	}
	return c, nil
}

// Purge is used to completely clear the cache. Eviction callbacks, when set,
// run from the oldest to the newest entry.
func (c *WeightedLRU[K, V]) Purge() {
	// Walk the list from the back so the observed order matches actual
	// eviction order: oldest first.
	for ent := c.evictList.Back(); ent != nil; ent = ent.PrevEntry() {
		delete(c.items, ent.Key)
		if c.onEvict != nil {
			c.onEvict(ent.Key, ent.Value)
		}
	}
	c.evictList.Init()
	c.weight = 0
}

// Add adds a value to the cache with the given non-negative weight. It
// returns true when one or more entries were evicted to make room.
//
// When the key already exists, the new budget is computed from the current
// weight minus the old entry's weight plus the new weight before the cache
// is mutated. An entry whose weight alone exceeds the budget is rejected:
// for an existing key the previous value, weight and position are preserved
// and no eviction happens (and no eviction callback runs). Otherwise the
// entry is admitted, becoming the most recently used, and other entries are
// evicted starting from the oldest until the budget fits.
func (c *WeightedLRU[K, V]) Add(key K, value V, weight int64) (evicted bool) {
	if weight < 0 {
		panic("simplelru: weight must be non-negative")
	}

	// A single item heavier than the whole budget is rejected before any
	// state changes, preserving the old value, weight and order.
	if weight > c.maxWeight {
		return false
	}

	// Effective weight of all entries other than the replaced one. The
	// replaced entry is accounted for as removed, and later re-added at the
	// front like any Add. All arithmetic below is subtraction of weights
	// that are part of the running total, so sums near math.MaxInt64 cannot
	// overflow.
	ent, existed := c.items[key]
	projected := c.weight
	if existed {
		projected -= ent.Weight
	}
	room := c.maxWeight - weight

	// Evict other entries oldest-first until they occupy at most room. The
	// admitted entry is treated as the newest, so it is skipped when it is
	// currently the oldest entry.
	for projected > room {
		oldest := c.evictList.Back()
		if oldest == ent {
			oldest = ent.PrevEntry()
		}
		if oldest == nil {
			// Defensive: with weight <= maxWeight this cannot happen.
			break
		}
		c.removeElement(oldest)
		projected -= oldest.Weight
		evicted = true
	}

	if ent != nil {
		c.evictList.MoveToFront(ent)
		ent.Value = value
		ent.Weight = weight
	} else {
		ent = c.evictList.PushFront(key, value)
		ent.Weight = weight
		c.items[key] = ent
	}
	c.weight = projected + weight
	return evicted
}

// Get looks up a key's value from the cache.
func (c *WeightedLRU[K, V]) Get(key K) (value V, ok bool) {
	if ent, ok := c.items[key]; ok {
		c.evictList.MoveToFront(ent)
		return ent.Value, true
	}
	return
}

// Contains checks if a key is in the cache, without updating the recent-ness
// or deleting it for being stale.
func (c *WeightedLRU[K, V]) Contains(key K) (ok bool) {
	_, ok = c.items[key]
	return ok
}

// Peek returns the key value (or undefined if not found) without updating
// the "recently used"-ness of the key.
func (c *WeightedLRU[K, V]) Peek(key K) (value V, ok bool) {
	if ent, ok := c.items[key]; ok {
		return ent.Value, true
	}
	return
}

// Remove removes the provided key from the cache, returning if the key was
// contained.
func (c *WeightedLRU[K, V]) Remove(key K) (present bool) {
	if ent, ok := c.items[key]; ok {
		c.removeElement(ent)
		return true
	}
	return false
}

// RemoveOldest removes the oldest item from the cache.
func (c *WeightedLRU[K, V]) RemoveOldest() (key K, value V, ok bool) {
	if ent := c.evictList.Back(); ent != nil {
		c.removeElement(ent)
		return ent.Key, ent.Value, true
	}
	return
}

// GetOldest returns the oldest entry.
func (c *WeightedLRU[K, V]) GetOldest() (key K, value V, ok bool) {
	if ent := c.evictList.Back(); ent != nil {
		return ent.Key, ent.Value, true
	}
	return
}

// Keys returns a slice of the keys in the cache, from oldest to newest.
func (c *WeightedLRU[K, V]) Keys() []K {
	keys := make([]K, c.evictList.Length())
	i := 0
	for ent := c.evictList.Back(); ent != nil; ent = ent.PrevEntry() {
		keys[i] = ent.Key
		i++
	}
	return keys
}

// Values returns a slice of the values in the cache, from oldest to newest.
func (c *WeightedLRU[K, V]) Values() []V {
	values := make([]V, c.evictList.Length())
	i := 0
	for ent := c.evictList.Back(); ent != nil; ent = ent.PrevEntry() {
		values[i] = ent.Value
		i++
	}
	return values
}

// Len returns the number of items in the cache.
func (c *WeightedLRU[K, V]) Len() int {
	return c.evictList.Length()
}

// Cap returns the maximum total weight of the cache.
func (c *WeightedLRU[K, V]) Cap() int64 {
	return c.maxWeight
}

// Weight returns the aggregate weight currently stored in the cache.
func (c *WeightedLRU[K, V]) Weight() int64 {
	return c.weight
}

// Resize changes the cache's maximum total weight, evicting entries starting
// from the oldest until the remaining entries fit. It returns the number of
// evicted entries. A non-positive size is ignored.
func (c *WeightedLRU[K, V]) Resize(maxWeight int64) (evicted int) {
	if maxWeight <= 0 {
		return 0
	}
	c.maxWeight = maxWeight
	for c.weight-c.maxWeight > 0 {
		if c.evictList.Back() == nil {
			break
		}
		c.removeOldest()
		evicted++
	}
	return evicted
}

// removeOldest removes the oldest item from the cache.
func (c *WeightedLRU[K, V]) removeOldest() {
	if ent := c.evictList.Back(); ent != nil {
		c.removeElement(ent)
	}
}

// removeElement is used to remove a given list element from the cache
func (c *WeightedLRU[K, V]) removeElement(e *internal.Entry[K, V]) {
	c.evictList.Remove(e)
	delete(c.items, e.Key)
	c.weight -= e.Weight
	if c.onEvict != nil {
		c.onEvict(e.Key, e.Value)
	}
}
