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

	// Default2QRecentRatioMin is the default lower bound of the
	// target recent-queue ratio when adaptive ratio adjustment is
	// enabled.
	Default2QRecentRatioMin = 0.10

	// Default2QRecentRatioMax is the default upper bound of the
	// target recent-queue ratio when adaptive ratio adjustment is
	// enabled.
	Default2QRecentRatioMax = 0.90

	// Default2QRecentRatioStep is the default fixed step used to
	// adjust the target recent-queue ratio on every ghost hit.
	Default2QRecentRatioStep = 0.05

	// Default2QPromotionHits is the default number of hits a key
	// must accumulate while resident in the recent queue before it
	// is promoted into the frequent queue. One hit preserves the
	// classic 2Q behavior.
	Default2QPromotionHits = 1
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

	// Bounds and fixed step for adapting the target recent ratio
	// based on ghost-queue hits.
	recentRatioMin  float64
	recentRatioMax  float64
	recentRatioStep float64
	adaptive        bool

	// Number of recent-queue hits required before a key is promoted
	// into the frequent queue.
	promotionHits int

	// promoteOnReadd keeps the classic 2Q semantics, where adding a
	// key that already exists in recent immediately promotes it.
	promoteOnReadd bool

	// recentHits records the number of hits accumulated per key
	// while resident in the recent queue. Frequent-queue keys carry
	// no hit state.
	recentHits map[K]int

	recent      simplelru.LRUCache[K, V]
	frequent    simplelru.LRUCache[K, V]
	recentEvict simplelru.LRUCache[K, struct{}]
	lock        sync.RWMutex
}

// TwoQueueConfig configures a TwoQueueCache built with New2QWithConfig.
//
// Size is the total number of entries kept in the recent and frequent
// queues combined.
//
// RecentRatio is the initial fraction of Size targeted for the recent
// queue; defaults to Default2QRecentRatio. GhostRatio is the capacity of
// the ghost queue relative to Size; defaults to Default2QGhostEntries.
//
// PromotionHits is the number of hits a key must receive while resident
// in the recent queue before it is promoted into the frequent queue.
// It defaults to Default2QPromotionHits, which preserves classic 2Q
// behavior. Only Get counts as a hit; re-adding an existing recent key
// updates its value without touching its heat.
//
// When AdaptRatio is true, a hit on a ghost entry raises the target
// recent ratio by RecentRatioStep, clamped to the inclusive
// [RecentRatioMin, RecentRatioMax] bounds. The ratio only moves upward
// in fixed increments, so adaptation is deterministic. All ratio fields
// fall back to their Default2Q* constants when zero.
type TwoQueueConfig struct {
	Size            int
	RecentRatio     float64
	GhostRatio      float64
	PromotionHits   int
	AdaptRatio      bool
	RecentRatioMin  float64
	RecentRatioMax  float64
	RecentRatioStep float64
}

// New2Q creates a new TwoQueueCache using the default
// values for the parameters.
func New2Q[K comparable, V any](size int) (*TwoQueueCache[K, V], error) {
	return New2QParams[K, V](size, Default2QRecentRatio, Default2QGhostEntries)
}

// New2QParams creates a new TwoQueueCache using the provided
// parameter values.
func New2QParams[K comparable, V any](size int, recentRatio, ghostRatio float64) (*TwoQueueCache[K, V], error) {
	if size <= 0 {
		return nil, errors.New("invalid size")
	}
	if recentRatio < 0.0 || recentRatio > 1.0 {
		return nil, errors.New("invalid recent ratio")
	}
	if ghostRatio < 0.0 || ghostRatio > 1.0 {
		return nil, errors.New("invalid ghost ratio")
	}

	return new2Q[K, V](&TwoQueueConfig{
		Size:          size,
		RecentRatio:   recentRatio,
		GhostRatio:    ghostRatio,
		PromotionHits: Default2QPromotionHits,
	})
}

// New2QWithConfig creates a new TwoQueueCache using the provided
// configuration. The promotion threshold and optional adaptive recent
// ratio are configured here; see TwoQueueConfig for details.
func New2QWithConfig[K comparable, V any](config TwoQueueConfig) (*TwoQueueCache[K, V], error) {
	if config.Size <= 0 {
		return nil, errors.New("invalid size")
	}
	if config.RecentRatio < 0.0 || config.RecentRatio > 1.0 {
		return nil, errors.New("invalid recent ratio")
	}
	if config.GhostRatio < 0.0 || config.GhostRatio > 1.0 {
		return nil, errors.New("invalid ghost ratio")
	}
	if config.PromotionHits < 0 {
		return nil, errors.New("invalid promotion hits")
	}

	if config.RecentRatio == 0.0 {
		config.RecentRatio = Default2QRecentRatio
	}
	if config.GhostRatio == 0.0 {
		config.GhostRatio = Default2QGhostEntries
	}
	if config.PromotionHits == 0 {
		config.PromotionHits = Default2QPromotionHits
	}

	if config.AdaptRatio {
		if config.RecentRatioMin == 0.0 {
			config.RecentRatioMin = Default2QRecentRatioMin
		}
		if config.RecentRatioMax == 0.0 {
			config.RecentRatioMax = Default2QRecentRatioMax
		}
		if config.RecentRatioStep == 0.0 {
			config.RecentRatioStep = Default2QRecentRatioStep
		}
		if config.RecentRatioMin < 0.0 || config.RecentRatioMin > 1.0 ||
			config.RecentRatioMax < 0.0 || config.RecentRatioMax > 1.0 ||
			config.RecentRatioMin > config.RecentRatioMax ||
			config.RecentRatio < config.RecentRatioMin ||
			config.RecentRatio > config.RecentRatioMax {
			return nil, errors.New("invalid recent ratio bounds")
		}
		if config.RecentRatioStep <= 0.0 || config.RecentRatioStep > 1.0 {
			return nil, errors.New("invalid recent ratio step")
		}
	} else {
		config.RecentRatioMin = config.RecentRatio
		config.RecentRatioMax = config.RecentRatio
		config.RecentRatioStep = 0.0
	}

	return new2Q[K, V](&config)
}

// new2Q constructs a cache from validated parameters.
func new2Q[K comparable, V any](config *TwoQueueConfig) (*TwoQueueCache[K, V], error) {
	recentSize := int(float64(config.Size) * config.RecentRatio)
	evictSize := int(float64(config.Size) * config.GhostRatio)

	// Allocate the LRUs
	recent, err := simplelru.NewLRU[K, V](config.Size, nil)
	if err != nil {
		return nil, err
	}
	frequent, err := simplelru.NewLRU[K, V](config.Size, nil)
	if err != nil {
		return nil, err
	}
	if evictSize <= 0 {
		evictSize = 1
	}
	recentEvict, err := simplelru.NewLRU[K, struct{}](evictSize, nil)
	if err != nil {
		return nil, err
	}

	// Initialize the cache
	c := &TwoQueueCache[K, V]{
		size:            config.Size,
		recentSize:      recentSize,
		recentRatio:     config.RecentRatio,
		ghostRatio:      config.GhostRatio,
		recentRatioMin:  config.RecentRatioMin,
		recentRatioMax:  config.RecentRatioMax,
		recentRatioStep: config.RecentRatioStep,
		adaptive:        config.AdaptRatio,
		promotionHits:   config.PromotionHits,
		promoteOnReadd:  config.PromotionHits == Default2QPromotionHits,
		recentHits:      make(map[K]int),
		recent:          recent,
		frequent:        frequent,
		recentEvict:     recentEvict,
	}
	return c, nil
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
	// promote it to frequent once the threshold is reached.
	if val, ok := c.recent.Get(key); ok {
		hits := c.recentHits[key] + 1
		c.recentHits[key] = hits
		if hits >= c.promotionHits {
			c.promote(key)
		}
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

	if c.recent.Contains(key) {
		// Check if the value is recently used. In the classic mode a
		// repeated add promotes the value into the frequent list; with
		// a configured promotion threshold it only replaces the value
		// and keeps the accumulated heat, so only Get hits drive a
		// promotion.
		if c.promoteOnReadd {
			c.recent.Remove(key)
			delete(c.recentHits, key)
			c.frequent.Add(key, value)
		} else {
			c.recent.Add(key, value)
		}
		return
	}

	// If the value was recently evicted, add it to the
	// frequently used list
	if c.recentEvict.Contains(key) {
		c.ensureSpace(true)
		c.recentEvict.Remove(key)
		c.adaptRecentRatio()
		c.frequent.Add(key, value)
		return
	}

	// Add to the recently seen list
	c.ensureSpace(false)
	c.recent.Add(key, value)
	c.recentHits[key] = 0
}

// promote moves a key out of recent into frequent, dropping its
// recent-queue heat. The caller must hold the lock.
func (c *TwoQueueCache[K, V]) promote(key K) {
	val, ok := c.recent.Peek(key)
	if !ok {
		return
	}
	c.recent.Remove(key)
	delete(c.recentHits, key)
	c.frequent.Add(key, val)
}

// adaptRecentRatio raises the target recent ratio by the fixed step,
// clamped to the configured upper bound. Adjustment only happens when
// explicitly enabled, so classic-mode caches are unaffected.
func (c *TwoQueueCache[K, V]) adaptRecentRatio() {
	if !c.adaptive {
		return
	}
	ratio := c.recentRatio + c.recentRatioStep
	if ratio > c.recentRatioMax {
		ratio = c.recentRatioMax
	}
	if ratio == c.recentRatio {
		return
	}
	c.recentRatio = ratio
	c.recentSize = int(float64(c.size) * ratio)
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
	recentSize := int(float64(size) * c.recentRatio)
	evictSize := int(float64(size) * c.ghostRatio)
	c.size = size
	c.recentSize = recentSize

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
