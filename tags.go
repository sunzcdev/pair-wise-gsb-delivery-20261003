/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import "sync"

// tagIndex is a bidirectional index between tag names and the key hashes
// that carry them. It allows groups of items sharing a tag to be invalidated
// together, and tag associations to be cleaned up when a key is deleted,
// replaced, evicted, or expires.
//
// tagIndex is safe for concurrent usage.
type tagIndex struct {
	mu sync.RWMutex
	// tagToKeys maps a tag name to the set of key hashes carrying it.
	tagToKeys map[string]map[uint64]struct{}
	// keyToTags maps a key hash to the set of tag names it carries.
	keyToTags map[uint64]map[string]struct{}
}

func newTagIndex() *tagIndex {
	return &tagIndex{
		tagToKeys: make(map[string]map[uint64]struct{}),
		keyToTags: make(map[uint64]map[string]struct{}),
	}
}

// track replaces the key's tag associations with the given tags. Any tags
// previously attached to the key are dropped, so re-setting a key without
// tags cleans up its old associations.
func (t *tagIndex) track(key uint64, tags []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeKeyLocked(key)
	for _, tag := range tags {
		keys, ok := t.tagToKeys[tag]
		if !ok {
			keys = make(map[uint64]struct{})
			t.tagToKeys[tag] = keys
		}
		keys[key] = struct{}{}

		keyTags, ok := t.keyToTags[key]
		if !ok {
			keyTags = make(map[string]struct{})
			t.keyToTags[key] = keyTags
		}
		keyTags[tag] = struct{}{}
	}
}

// untrack removes the key from every tag it belongs to. It is called whenever
// a key leaves the cache (delete, replace, eviction, or expiration).
func (t *tagIndex) untrack(key uint64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeKeyLocked(key)
}

func (t *tagIndex) removeKeyLocked(key uint64) {
	tags, ok := t.keyToTags[key]
	if !ok {
		return
	}
	for tag := range tags {
		keys := t.tagToKeys[tag]
		delete(keys, key)
		if len(keys) == 0 {
			delete(t.tagToKeys, tag)
		}
	}
	delete(t.keyToTags, key)
}

// keys returns a snapshot of the key hashes currently associated with the tag.
func (t *tagIndex) keys(tag string) []uint64 {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	set := t.tagToKeys[tag]
	keys := make([]uint64, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}

// clear drops all tag associations.
func (t *tagIndex) clear() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tagToKeys = make(map[string]map[uint64]struct{})
	t.keyToTags = make(map[uint64]map[string]struct{})
}

// SetWithTags works like Set but also associates the key with the given tags.
// A key may carry multiple tags, and the same tag may be shared by any number
// of keys, so a group of related entries (e.g. all items derived from one
// source record) can be invalidated together via InvalidateTags.
//
// Tags are attached to the key, not the value: setting the key again replaces
// its tag set, and setting it without tags drops all previous associations.
//
// See Set for more information.
func (c *Cache[K, V]) SetWithTags(key K, value V, cost int64, tags ...string) bool {
	return c.SetWithTTLAndTags(key, value, cost, 0, tags...)
}

// InvalidateTags removes every key associated with at least one of the given
// tags from the cache and drops the tag associations themselves. It is a
// no-op for tags that no key currently carries.
//
// Completion boundary: when InvalidateTags returns, the affected keys have
// been removed from the cache and subsequent Get calls for them will miss.
// To guarantee this, InvalidateTags briefly pauses the background write
// queue, applies every Set still waiting in the asynchronous buffer, and
// only then removes the tagged keys, so stale values that were Set but not
// yet processed cannot resurface afterwards. Writes issued concurrently with
// or after InvalidateTags are not affected and may re-populate the keys.
//
// The removal triggers the usual OnEvict/OnExit callbacks for the old
// values, but the completion contract of InvalidateTags only covers the
// cache state described above: it does not wait for per-key callbacks of
// concurrently in-flight operations, and callers that depend on callback
// side effects must synchronize with them separately.
//
// Like Clear, InvalidateTags must not be called from OnEvict, OnReject, or
// OnExit callbacks, as that would deadlock the write queue.
func (c *Cache[K, V]) InvalidateTags(tags ...string) {
	if c == nil || c.isClosed.Load() || len(tags) == 0 {
		return
	}

	// Pause the goroutine processing the asynchronous write queue so that
	// Sets still sitting in setBuf cannot resurrect old values after the
	// invalidation.
	c.stop <- struct{}{}
	<-c.done

	// Drain the write queue, applying every pending write. Old values that
	// were Set but not yet processed are now visible in the store and the
	// tag index, and will be removed by the invalidation below.
loop:
	for {
		select {
		case i := <-c.setBuf:
			if i.wait != nil {
				close(i.wait)
				continue
			}
			c.applySetItem(i, c.evictItem)
		default:
			break loop
		}
	}

	// Remove every key associated with the tags from the policy, the store,
	// and the tag index.
	for _, tag := range tags {
		for _, key := range c.tags.keys(tag) {
			value, ok := c.storedItems.Get(key, 0)
			cost := c.cachePolicy.Cost(key)
			c.cachePolicy.Del(key)
			conflict, _ := c.storedItems.Del(key, 0)
			c.tags.untrack(key)
			if ok && c.onEvict != nil {
				c.onEvict(&Item[V]{Key: key, Conflict: conflict, Value: value, Cost: cost})
			}
		}
	}

	// Resume processing the write queue.
	go c.processItems()
}
