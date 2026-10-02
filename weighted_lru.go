// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"sync"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// WeightedCache is a thread-safe LRU cache bounded by a total weight budget
// rather than an entry count. Each value is stored together with a
// caller-provided, non-negative weight.
type WeightedCache[K comparable, V any] struct {
	lru         *simplelru.WeightedLRU[K, V]
	evictedKeys []K
	evictedVals []V
	onEvictedCB func(k K, v V)
	lock        sync.RWMutex
}

// NewWeighted creates a weight-budgeted LRU with the given maximum total
// weight.
func NewWeighted[K comparable, V any](maxWeight int64) (*WeightedCache[K, V], error) {
	return NewWeightedWithEvict[K, V](maxWeight, nil)
}

// NewWeightedWithEvict constructs a weight-budgeted cache with the given
// maximum total weight and eviction callback.
//
// onEvict is invoked for every removed entry outside the cache's internal
// lock, in the order entries were actually evicted (oldest first), so it
// may safely re-enter the cache (for example call Get, Add or Len).
func NewWeightedWithEvict[K comparable, V any](maxWeight int64, onEvicted func(key K, value V)) (c *WeightedCache[K, V], err error) {
	c = &WeightedCache[K, V]{
		onEvictedCB: onEvicted,
	}
	if onEvicted != nil {
		c.initEvictBuffers()
		onEvicted = c.onEvicted
	}
	c.lru, err = simplelru.NewWeightedLRU(maxWeight, onEvicted)
	return
}

func (c *WeightedCache[K, V]) initEvictBuffers() {
	c.evictedKeys = make([]K, 0, DefaultEvictedBufferSize)
	c.evictedVals = make([]V, 0, DefaultEvictedBufferSize)
}

// onEvicted saves an evicted key/value pair so it can be delivered to the
// externally registered callback outside the critical section.
func (c *WeightedCache[K, V]) onEvicted(k K, v V) {
	c.evictedKeys = append(c.evictedKeys, k)
	c.evictedVals = append(c.evictedVals, v)
}

// drainEvictedLocked detaches the pending eviction buffers. The caller must
// hold c.lock and invoke the returned pairs afterwards without the lock.
// Detaching first keeps re-entrant calls from callbacks independent.
func (c *WeightedCache[K, V]) drainEvictedLocked() (ks []K, vs []V) {
	ks, vs = c.evictedKeys, c.evictedVals
	c.evictedKeys, c.evictedVals = nil, nil
	return
}

// fireLocked replays pending evictions through the user callback outside of
// the lock. It must be called with c.lock held; it releases the lock while
// callbacks run.
func (c *WeightedCache[K, V]) fireLocked() {
	if c.onEvictedCB == nil || len(c.evictedKeys) == 0 {
		c.lock.Unlock()
		return
	}
	ks, vs := c.drainEvictedLocked()
	c.lock.Unlock()
	for i := 0; i < len(ks); i++ {
		c.onEvictedCB(ks[i], vs[i])
	}
}

// Purge is used to completely clear the cache.
func (c *WeightedCache[K, V]) Purge() {
	c.lock.Lock()
	c.lru.Purge()
	c.fireLocked()
}

// Add adds a value with the given non-negative weight to the cache. Returns
// true if one or more entries were evicted to make room.
//
// If the entry alone is heavier than the cache budget, the update is
// rejected: for an existing key the previous value, weight and position are
// kept, and no eviction occurs (and no eviction callback fires).
func (c *WeightedCache[K, V]) Add(key K, value V, weight int64) (evicted bool) {
	c.lock.Lock()
	evicted = c.lru.Add(key, value, weight)
	c.fireLocked()
	return
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

// Remove removes the provided key from the cache.
func (c *WeightedCache[K, V]) Remove(key K) (present bool) {
	c.lock.Lock()
	present = c.lru.Remove(key)
	c.fireLocked()
	return
}

// Resize changes the cache's total weight budget, evicting entries starting
// from the oldest until the remaining entries fit. It returns the number of
// evicted entries. A non-positive size is ignored.
func (c *WeightedCache[K, V]) Resize(maxWeight int64) (evicted int) {
	c.lock.Lock()
	evicted = c.lru.Resize(maxWeight)
	c.fireLocked()
	return evicted
}

// RemoveOldest removes the oldest item from the cache.
func (c *WeightedCache[K, V]) RemoveOldest() (key K, value V, ok bool) {
	c.lock.Lock()
	key, value, ok = c.lru.RemoveOldest()
	c.fireLocked()
	return
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

// Cap returns the maximum total weight of the cache.
func (c *WeightedCache[K, V]) Cap() int64 {
	return c.lru.Cap()
}

// Weight returns the aggregate weight currently stored in the cache.
func (c *WeightedCache[K, V]) Weight() int64 {
	c.lock.RLock()
	weight := c.lru.Weight()
	c.lock.RUnlock()
	return weight
}
