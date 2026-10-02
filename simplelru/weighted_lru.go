// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package simplelru

import (
	"errors"

	"github.com/hashicorp/golang-lru/v2/internal"
)

// WeightedLRU implements a non-thread safe LRU cache bounded by the total
// weight of its entries instead of by their count. Each entry is charged a
// caller-provided, non-negative weight against a fixed weight budget.
type WeightedLRU[K comparable, V any] struct {
	maxWeight int64
	curWeight int64
	evictList *internal.LruList[K, V]
	items     map[K]*internal.Entry[K, V]
	onEvict   EvictCallback[K, V]
}

// NewWeightedLRU constructs a weighted LRU with the given total weight budget.
func NewWeightedLRU[K comparable, V any](maxWeight int64, onEvict EvictCallback[K, V]) (*WeightedLRU[K, V], error) {
	if maxWeight < 0 {
		return nil, errors.New("must provide a non-negative weight budget")
	}

	c := &WeightedLRU[K, V]{
		maxWeight: maxWeight,
		evictList: internal.NewList[K, V](),
		items:     make(map[K]*internal.Entry[K, V]),
		onEvict:   onEvict,
	}
	return c, nil
}

// Add adds a value with the given weight to the cache and updates the
// "recently used"-ness of the key. It returns whether the entry was stored
// (or updated) and the number of entries evicted to fit the budget.
//
// An entry is not stored when weight is negative, or when its weight alone
// exceeds the total weight budget. For an existing key the new budget is
// computed against the new weight before anything is modified: a rejected
// update leaves the previous value, weight and position untouched and never
// evicts another entry. Otherwise, if the total weight exceeds the budget,
// entries are evicted starting with the least recently used one. The entry
// being added or updated is never evicted.
//
// A zero weight is valid and contributes nothing to the budget, so adding
// one never triggers eviction on its own; zero-weight entries still take
// their normal LRU position and may be evicted as the oldest entry when a
// later addition overflows the budget.
func (c *WeightedLRU[K, V]) Add(key K, value V, weight int64) (added bool, evicted int) {
	if weight < 0 {
		return false, 0
	}

	var oldWeight int64
	if ent, ok := c.items[key]; ok {
		oldWeight = ent.Weight
	}

	// Compute the resulting budget before mutating anything. Use uint64 so
	// that weight sums larger than math.MaxInt64 remain representable while
	// evictions bring them back within the budget.
	total := uint64(c.curWeight) - uint64(oldWeight) + uint64(weight)

	// A single entry that does not fit the budget is rejected outright. For
	// an update the old entry (including its order) is left as-is.
	if weight > c.maxWeight {
		return false, 0
	}

	if ent, ok := c.items[key]; ok {
		c.evictList.MoveToFront(ent)
		ent.Value = value
		ent.Weight = weight
	} else {
		ent := c.evictList.PushFrontWeighted(key, value, weight)
		c.items[key] = ent
	}

	for total > uint64(c.maxWeight) {
		// The freshly added or updated entry has weight <= maxWeight, so it
		// cannot be the back entry that gets removed here.
		if _, _, backWeight := c.removeOldest(); backWeight >= 0 {
			total -= uint64(backWeight)
			evicted++
		} else {
			break
		}
	}

	c.curWeight = int64(total)
	return true, evicted
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
	var ent *internal.Entry[K, V]
	if ent, ok = c.items[key]; ok {
		return ent.Value, true
	}
	return
}

// PeekWeight returns the key's weight (or zero if not found) without
// updating the "recently used"-ness of the key.
func (c *WeightedLRU[K, V]) PeekWeight(key K) (weight int64, ok bool) {
	if ent, ok := c.items[key]; ok {
		return ent.Weight, true
	}
	return 0, false
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
	var weight int64
	key, value, weight = c.removeOldest()
	return key, value, weight >= 0
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

// Weight returns the total weight currently charged against the budget.
func (c *WeightedLRU[K, V]) Weight() int64 {
	return c.curWeight
}

// Cap returns the total weight budget of the cache.
func (c *WeightedLRU[K, V]) Cap() int64 {
	return c.maxWeight
}

// Purge is used to completely clear the cache. Entries are reported to the
// eviction callback, if any, from oldest to newest.
func (c *WeightedLRU[K, V]) Purge() {
	for c.evictList.Length() > 0 {
		c.removeOldest()
	}
}

// Resize changes the weight budget, evicting the oldest entries until the
// total weight fits. A non-positive budget evicts every weighted entry;
// zero-weight entries are retained. It returns the number of evicted entries.
func (c *WeightedLRU[K, V]) Resize(maxWeight int64) (evicted int) {
	if maxWeight < 0 {
		maxWeight = 0
	}
	c.maxWeight = maxWeight

	total := uint64(c.curWeight)
	for total > uint64(c.maxWeight) {
		if _, _, backWeight := c.removeOldest(); backWeight >= 0 {
			total -= uint64(backWeight)
			evicted++
		} else {
			break
		}
	}
	c.curWeight = int64(total)
	return evicted
}

// removeOldest removes the oldest item from the cache. The returned weight is
// -1 if there was no item.
func (c *WeightedLRU[K, V]) removeOldest() (key K, value V, weight int64) {
	if ent := c.evictList.Back(); ent != nil {
		key, value, weight = ent.Key, ent.Value, ent.Weight
		c.removeElement(ent)
		return key, value, weight
	}
	return key, value, -1
}

// removeElement is used to remove a given list element from the cache.
func (c *WeightedLRU[K, V]) removeElement(e *internal.Entry[K, V]) {
	c.evictList.Remove(e)
	delete(c.items, e.Key)
	c.curWeight -= e.Weight
	if c.onEvict != nil {
		c.onEvict(e.Key, e.Value)
	}
}
