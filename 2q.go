// Copyright IBM Corp. 2014, 2025
// SPDX-License-Identifier: MPL-2.0

package lru

import (
	"errors"
	"sync"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

const (
	// Default2QRecentRatio is the ratio of the 2Q cache dedicated
	// to recently added entries that have only been accessed once.
	Default2QRecentRatio = 0.25

	// Default2QGhostEntries is the default ratio of ghost
	// entries kept to track entries recently evicted
	Default2QGhostEntries = 0.50

	// Default2QPromoteThreshold is the default number of hits an
	// entry in the recent list needs before it is promoted to the
	// frequent list. A threshold of 1 matches the classic 2Q behavior
	// of promoting on the first hit.
	Default2QPromoteThreshold = 1
)

// TwoQueueCache is a thread-safe fixed size 2Q cache.
// 2Q is an enhancement over the standard LRU cache
// in that it tracks both frequently and recently used
// entries separately. This avoids a burst in access to new
// entries from evicting frequently used entries. It adds some
// additional tracking overhead to the standard LRU cache, and is
// computationally about 2x the cost, and adds some metadata over
// head. The ARCCache is similar, but does not require setting any
// parameters.
type TwoQueueCache[K comparable, V any] struct {
	size        int
	recentSize  int
	recentRatio float64
	ghostRatio  float64

	promoteThreshold int

	minRecentRatio  float64
	maxRecentRatio  float64
	adjustStepRatio float64

	minRecentSize int
	maxRecentSize int
	adjustStep    int
	recentHits    map[K]int

	recent      simplelru.LRUCache[K, V]
	frequent    simplelru.LRUCache[K, V]
	recentEvict simplelru.LRUCache[K, struct{}]
	lock        sync.RWMutex
}

// New2Q creates a new TwoQueueCache using the default
// values for the parameters.
func New2Q[K comparable, V any](size int) (*TwoQueueCache[K, V], error) {
	return New2QParams[K, V](size, Default2QRecentRatio, Default2QGhostEntries)
}

// New2QParams creates a new TwoQueueCache using the provided
// parameter values.
func New2QParams[K comparable, V any](size int, recentRatio, ghostRatio float64) (*TwoQueueCache[K, V], error) {
	return New2QAdaptiveParams[K, V](size, recentRatio, ghostRatio,
		Default2QPromoteThreshold, recentRatio, recentRatio, 0.0)
}

// New2QAdaptiveParams creates a new TwoQueueCache using the provided
// parameter values. In addition to the classic 2Q parameters it takes
// a promotion threshold: an entry in the recent list must be hit
// promoteThreshold times before it is promoted to the frequent list.
// The target size of the recent list is adaptive: every ghost hit
// (re-adding a key tracked in the recentEvict list) grows the recent
// target by adjustStep (expressed as a ratio of the total size),
// bounded below by minRecentRatio and above by maxRecentRatio.
func New2QAdaptiveParams[K comparable, V any](size int, recentRatio, ghostRatio float64, promoteThreshold int, minRecentRatio, maxRecentRatio, adjustStep float64) (*TwoQueueCache[K, V], error) {
	if size <= 0 {
		return nil, errors.New("invalid size")
	}
	if recentRatio < 0.0 || recentRatio > 1.0 {
		return nil, errors.New("invalid recent ratio")
	}
	if ghostRatio < 0.0 || ghostRatio > 1.0 {
		return nil, errors.New("invalid ghost ratio")
	}
	if promoteThreshold < 1 {
		return nil, errors.New("invalid promote threshold")
	}
	if minRecentRatio < 0.0 || minRecentRatio > 1.0 {
		return nil, errors.New("invalid min recent ratio")
	}
	if maxRecentRatio < 0.0 || maxRecentRatio > 1.0 {
		return nil, errors.New("invalid max recent ratio")
	}
	if minRecentRatio > maxRecentRatio {
		return nil, errors.New("min recent ratio exceeds max recent ratio")
	}
	if adjustStep < 0.0 || adjustStep > 1.0 {
		return nil, errors.New("invalid adjust step")
	}

	// Determine the sub-sizes
	evictSize := int(float64(size) * ghostRatio)

	// Allocate the LRUs
	recent, err := simplelru.NewLRU[K, V](size, nil)
	if err != nil {
		return nil, err
	}
	frequent, err := simplelru.NewLRU[K, V](size, nil)
	if err != nil {
		return nil, err
	}
	recentEvict, err := simplelru.NewLRU[K, struct{}](evictSize, nil)
	if err != nil {
		return nil, err
	}

	// Initialize the cache
	c := &TwoQueueCache[K, V]{
		size:             size,
		recentRatio:      recentRatio,
		ghostRatio:       ghostRatio,
		promoteThreshold: promoteThreshold,
		minRecentRatio:   minRecentRatio,
		maxRecentRatio:   maxRecentRatio,
		adjustStepRatio:  adjustStep,
		recentHits:       make(map[K]int),
		recent:           recent,
		frequent:         frequent,
		recentEvict:      recentEvict,
	}
	c.computeTargets(size)
	return c, nil
}

// computeTargets derives the recent target size, its bounds and the
// adjustment step (in entries) for the given total size.
func (c *TwoQueueCache[K, V]) computeTargets(size int) {
	c.minRecentSize = int(float64(size) * c.minRecentRatio)
	c.maxRecentSize = int(float64(size) * c.maxRecentRatio)
	c.adjustStep = int(float64(size) * c.adjustStepRatio)
	c.recentSize = int(float64(size) * c.recentRatio)
	if c.recentSize < c.minRecentSize {
		c.recentSize = c.minRecentSize
	}
	if c.recentSize > c.maxRecentSize {
		c.recentSize = c.maxRecentSize
	}
}

// Get looks up a key's value from the cache.
func (c *TwoQueueCache[K, V]) Get(key K) (value V, ok bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Check if this is a frequent value
	if val, ok := c.frequent.Get(key); ok {
		return val, ok
	}

	// If the value is contained in recent, count the hit and
	// promote it to frequent once the threshold is reached
	if val, ok := c.recent.Peek(key); ok {
		hits := c.recentHits[key] + 1
		if hits < c.promoteThreshold {
			c.recentHits[key] = hits
			c.recent.Get(key)
			return val, ok
		}
		delete(c.recentHits, key)
		c.recent.Remove(key)
		c.frequent.Add(key, val)
		return val, ok
	}

	// No hit
	return
}

// Add adds a value to the cache.
func (c *TwoQueueCache[K, V]) Add(key K, value V) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Check if the value is frequently used already,
	// and just update the value
	if c.frequent.Contains(key) {
		c.frequent.Add(key, value)
		return
	}

	// Check if the value is recently used, and promote
	// the value into the frequent list
	if c.recent.Contains(key) {
		if c.promoteThreshold > 1 {
			// Replace the value and reset the heat of the key,
			// it must earn promotion with fresh hits
			c.recent.Add(key, value)
			c.recentHits[key] = 0
			return
		}
		c.recent.Remove(key)
		c.frequent.Add(key, value)
		return
	}

	// If the value was recently evicted, add it to the
	// frequently used list, and grow the recent target
	if c.recentEvict.Contains(key) {
		c.adjustRecentTarget()
		c.ensureSpace(true)
		c.recentEvict.Remove(key)
		c.frequent.Add(key, value)
		return
	}

	// Add to the recently seen list
	c.ensureSpace(false)
	c.recent.Add(key, value)
	if c.promoteThreshold > 1 {
		c.recentHits[key] = 0
	}
}

// ensureSpace is used to ensure we have space in the cache
func (c *TwoQueueCache[K, V]) ensureSpace(recentEvict bool) {
	// If we have space, nothing to do
	recentLen := c.recent.Len()
	freqLen := c.frequent.Len()
	if recentLen+freqLen < c.size {
		return
	}

	// If the recent buffer is larger than
	// the target, evict from there
	if recentLen > 0 && (recentLen > c.recentSize || (recentLen == c.recentSize && !recentEvict)) {
		k, _, _ := c.recent.RemoveOldest()
		delete(c.recentHits, k)
		c.recentEvict.Add(k, struct{}{})
		return
	}

	// Remove from the frequent list otherwise
	c.frequent.RemoveOldest()
}

// adjustRecentTarget grows the target size of the recent list by the
// configured step, bounded by the configured min and max sizes.
func (c *TwoQueueCache[K, V]) adjustRecentTarget() {
	if c.adjustStep <= 0 {
		return
	}
	c.recentSize += c.adjustStep
	if c.recentSize > c.maxRecentSize {
		c.recentSize = c.maxRecentSize
	}
	if c.recentSize < c.minRecentSize {
		c.recentSize = c.minRecentSize
	}
}

// Len returns the number of items in the cache.
func (c *TwoQueueCache[K, V]) Len() int {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.recent.Len() + c.frequent.Len()
}

// Cap returns the capacity of the cache
func (c *TwoQueueCache[K, V]) Cap() int {
	return c.size
}

// Resize changes the cache size.
func (c *TwoQueueCache[K, V]) Resize(size int) (evicted int) {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Recalculate the sub-sizes
	evictSize := int(float64(size) * c.ghostRatio)
	c.size = size
	c.computeTargets(size)

	// ensureSpace
	diff := c.recent.Len() + c.frequent.Len() - size
	if diff < 0 {
		diff = 0
	}
	for i := 0; i < diff; i++ {
		c.ensureSpace(true)
	}

	// Reallocate the LRUs
	c.recent.Resize(size)
	c.frequent.Resize(size)
	c.recentEvict.Resize(evictSize)

	return diff
}

// Keys returns a slice of the keys in the cache.
// The frequently used keys are first in the returned slice.
func (c *TwoQueueCache[K, V]) Keys() []K {
	c.lock.RLock()
	defer c.lock.RUnlock()
	k1 := c.frequent.Keys()
	k2 := c.recent.Keys()
	return append(k1, k2...)
}

// Values returns a slice of the values in the cache.
// The frequently used values are first in the returned slice.
func (c *TwoQueueCache[K, V]) Values() []V {
	c.lock.RLock()
	defer c.lock.RUnlock()
	v1 := c.frequent.Values()
	v2 := c.recent.Values()
	return append(v1, v2...)
}

// Remove removes the provided key from the cache.
func (c *TwoQueueCache[K, V]) Remove(key K) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.recentHits, key)
	if c.frequent.Remove(key) {
		return
	}
	if c.recent.Remove(key) {
		return
	}
	if c.recentEvict.Remove(key) {
		return
	}
}

// Purge is used to completely clear the cache.
func (c *TwoQueueCache[K, V]) Purge() {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.recent.Purge()
	c.frequent.Purge()
	c.recentEvict.Purge()
	c.recentHits = make(map[K]int)
}

// Contains is used to check if the cache contains a key
// without updating recency or frequency.
func (c *TwoQueueCache[K, V]) Contains(key K) bool {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.frequent.Contains(key) || c.recent.Contains(key)
}

// Peek is used to inspect the cache value of a key
// without updating recency or frequency.
func (c *TwoQueueCache[K, V]) Peek(key K) (value V, ok bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if val, ok := c.frequent.Peek(key); ok {
		return val, ok
	}
	return c.recent.Peek(key)
}
