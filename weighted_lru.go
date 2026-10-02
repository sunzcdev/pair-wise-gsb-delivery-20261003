// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"sync"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// WeightedCache is a thread-safe LRU cache bounded by the total weight of
// its entries rather than by their count. The caller supplies a
// non-negative weight for every value.
type WeightedCache[K comparable, V any] struct {
	lru         *simplelru.WeightedLRU[K, V]
	evictedKeys []K
	evictedVals []V
	onEvictedCB func(k K, v V)
	lock        sync.RWMutex
}

// NewWeighted creates a weighted LRU with the given total weight budget.
func NewWeighted[K comparable, V any](maxWeight int64) (*WeightedCache[K, V], error) {
	return NewWeightedWithEvict[K, V](maxWeight, nil)
}

// NewWeightedWithEvict constructs a weighted cache with the given weight
// budget and eviction callback.
//
// The callback is invoked for every evicted entry after the cache's lock has
// been released, in the order entries were actually evicted (oldest first),
// so it may safely re-enter the cache (for example to call Get, Add or Len).
func NewWeightedWithEvict[K comparable, V any](maxWeight int64, onEvicted func(key K, value V)) (c *WeightedCache[K, V], err error) {
	c = &WeightedCache[K, V]{
		onEvictedCB: onEvicted,
	}
	if onEvicted != nil {
		c.initEvictBuffers()
		onEvicted = c.onEvicted
	}
	c.lru, err = simplelru.NewWeightedLRU(maxWeight, onEvicted)
	return c, err
}

func (c *WeightedCache[K, V]) initEvictBuffers() {
	c.evictedKeys = make([]K, 0, DefaultEvictedBufferSize)
	c.evictedVals = make([]V, 0, DefaultEvictedBufferSize)
}

// onEvicted buffers evicted key/val pairs while the lock is held; the
// externally registered callback is invoked later outside of the critical
// section.
func (c *WeightedCache[K, V]) onEvicted(k K, v V) {
	c.evictedKeys = append(c.evictedKeys, k)
	c.evictedVals = append(c.evictedVals, v)
}

// drainEvicted snapshots and resets the eviction buffer while the lock is
// held. The returned slices are owned by the caller and are used to invoke
// the external callback after the lock has been released. Swapping in a
// fresh buffer before unlock lets callbacks re-enter the cache safely:
// entries evicted by a nested call land in that call's own buffer instead of
// mixing into or duplicating the outer batch.
func (c *WeightedCache[K, V]) drainEvicted() (ks []K, vs []V) {
	if c.onEvictedCB != nil && len(c.evictedKeys) > 0 {
		ks, vs = c.evictedKeys, c.evictedVals
		c.initEvictBuffers()
	}
	return ks, vs
}

// fire invokes the external callback for a locally owned snapshot. It must
// be called without holding the cache lock.
func (c *WeightedCache[K, V]) fire(ks []K, vs []V) {
	for i := 0; i < len(ks); i++ {
		c.onEvictedCB(ks[i], vs[i])
	}
}

// Add adds a value with the given weight to the cache. It returns whether
// the entry was stored (or updated) and the number of entries evicted to fit
// the weight budget.
//
// The entry is rejected (and no other entry is touched) when weight is
// negative, or when it alone exceeds the total weight budget. Replacing an
// existing key computes the new budget before changing anything, so a
// rejected update keeps the old value, weight and position and performs no
// evictions. See simplelru.WeightedLRU.Add for full semantics.
//
// Eviction callbacks run outside the cache lock, in the order entries were
// evicted.
func (c *WeightedCache[K, V]) Add(key K, value V, weight int64) (added bool, evicted int) {
	c.lock.Lock()
	added, evicted = c.lru.Add(key, value, weight)
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
	return added, evicted
}

// Get looks up a key's value from the cache.
func (c *WeightedCache[K, V]) Get(key K) (value V, ok bool) {
	c.lock.Lock()
	value, ok = c.lru.Get(key)
	c.lock.Unlock()
	return value, ok
}

// Contains checks if a key is in the cache, without updating the
// recent-ness or deleting it for being stale.
func (c *WeightedCache[K, V]) Contains(key K) bool {
	c.lock.RLock()
	containKey := c.lru.Contains(key)
	c.lock.RUnlock()
	return containKey
}

// Peek returns the key value (or undefined if not found) without updating
// the "recently used"-ness of the key.
func (c *WeightedCache[K, V]) Peek(key K) (value V, ok bool) {
	c.lock.RLock()
	value, ok = c.lru.Peek(key)
	c.lock.RUnlock()
	return value, ok
}

// PeekWeight returns the key's weight (or zero if not found) without
// updating the "recently used"-ness of the key.
func (c *WeightedCache[K, V]) PeekWeight(key K) (weight int64, ok bool) {
	c.lock.RLock()
	weight, ok = c.lru.PeekWeight(key)
	c.lock.RUnlock()
	return weight, ok
}

// ContainsOrAdd checks if a key is in the cache without updating the
// recent-ness or deleting it for being stale, and if not, adds the value
// with the given weight. Returns whether the key was found, whether the
// entry was stored and the number of evicted entries.
func (c *WeightedCache[K, V]) ContainsOrAdd(key K, value V, weight int64) (found, added bool, evicted int) {
	c.lock.Lock()
	if c.lru.Contains(key) {
		c.lock.Unlock()
		return true, false, 0
	}
	added, evicted = c.lru.Add(key, value, weight)
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
	return false, added, evicted
}

// PeekOrAdd checks if a key is in the cache without updating the
// recent-ness or deleting it for being stale, and if not, adds the value
// with the given weight. Returns the previous value (if found), whether it
// was found, whether the entry was stored and the number of evicted entries.
func (c *WeightedCache[K, V]) PeekOrAdd(key K, value V, weight int64) (previous V, found, added bool, evicted int) {
	c.lock.Lock()
	previous, found = c.lru.Peek(key)
	if found {
		c.lock.Unlock()
		return previous, true, false, 0
	}
	added, evicted = c.lru.Add(key, value, weight)
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
	return previous, false, added, evicted
}

// Remove removes the provided key from the cache.
func (c *WeightedCache[K, V]) Remove(key K) (present bool) {
	c.lock.Lock()
	present = c.lru.Remove(key)
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
	return present
}

// Resize changes the weight budget, evicting the oldest entries until the
// total weight fits, and returns the number of evicted entries. Eviction
// callbacks run outside the cache lock, in eviction order.
func (c *WeightedCache[K, V]) Resize(maxWeight int64) (evicted int) {
	c.lock.Lock()
	evicted = c.lru.Resize(maxWeight)
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
	return evicted
}

// RemoveOldest removes the oldest item from the cache.
func (c *WeightedCache[K, V]) RemoveOldest() (key K, value V, ok bool) {
	c.lock.Lock()
	key, value, ok = c.lru.RemoveOldest()
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
	return key, value, ok
}

// GetOldest returns the oldest entry.
func (c *WeightedCache[K, V]) GetOldest() (key K, value V, ok bool) {
	c.lock.RLock()
	key, value, ok = c.lru.GetOldest()
	c.lock.RUnlock()
	return
}

// Keys returns a slice of the keys in the cache, from oldest to newest.
func (c *WeightedCache[K, V]) Keys() []K {
	c.lock.RLock()
	keys := c.lru.Keys()
	c.lock.RUnlock()
	return keys
}

// Values returns a slice of the values in the cache, from oldest to newest.
func (c *WeightedCache[K, V]) Values() []V {
	c.lock.RLock()
	values := c.lru.Values()
	c.lock.RUnlock()
	return values
}

// Len returns the number of items in the cache.
func (c *WeightedCache[K, V]) Len() int {
	c.lock.RLock()
	length := c.lru.Len()
	c.lock.RUnlock()
	return length
}

// Weight returns the total weight currently charged against the budget.
func (c *WeightedCache[K, V]) Weight() int64 {
	c.lock.RLock()
	weight := c.lru.Weight()
	c.lock.RUnlock()
	return weight
}

// Cap returns the weight budget of the cache.
func (c *WeightedCache[K, V]) Cap() int64 {
	return c.lru.Cap()
}

// Purge is used to completely clear the cache. Eviction callbacks run outside
// the cache lock, in eviction order (oldest first).
func (c *WeightedCache[K, V]) Purge() {
	c.lock.Lock()
	c.lru.Purge()
	ks, vs := c.drainEvicted()
	c.lock.Unlock()
	c.fire(ks, vs)
}
