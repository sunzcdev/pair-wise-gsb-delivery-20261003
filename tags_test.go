/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTagTestCache(t *testing.T) *Cache[string, int] {
	t.Helper()
	c, err := NewCache(&Config[string, int]{
		NumCounters:        1000,
		MaxCost:            10000,
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func TestSetWithTagsAndInvalidateTags(t *testing.T) {
	c := newTagTestCache(t)

	require.True(t, c.SetWithTags("a1", 1, 1, "tagA"))
	require.True(t, c.SetWithTags("a2", 2, 1, "tagA"))
	require.True(t, c.SetWithTags("b1", 3, 1, "tagB"))
	require.True(t, c.SetWithTags("ab", 4, 1, "tagA", "tagB"))
	require.True(t, c.Set("plain", 5, 1))
	c.Wait()

	c.InvalidateTags("tagA")

	// All keys carrying tagA are gone, including the multi-tag key.
	for _, key := range []string{"a1", "a2", "ab"} {
		_, ok := c.Get(key)
		require.False(t, ok, "key %q should have been invalidated", key)
	}
	// Untagged keys and keys of other tags are untouched.
	value, ok := c.Get("b1")
	require.True(t, ok)
	require.Equal(t, 3, value)
	value, ok = c.Get("plain")
	require.True(t, ok)
	require.Equal(t, 5, value)

	// The index is cleaned up: tagA is gone and "ab" no longer appears
	// under tagB either.
	require.Empty(t, c.tags.keys("tagA"))
	require.Len(t, c.tags.keys("tagB"), 1)

	// Invalidating multiple tags at once works too.
	c.InvalidateTags("tagB", "no-such-tag")
	_, ok = c.Get("b1")
	require.False(t, ok)
	require.Empty(t, c.tags.keys("tagB"))
}

func TestInvalidateTagsWithPendingSets(t *testing.T) {
	c := newTagTestCache(t)

	// Set many tagged keys without waiting: some of them are likely still
	// sitting in the asynchronous set buffer when InvalidateTags runs.
	for i := 0; i < 100; i++ {
		require.True(t, c.SetWithTags(fmt.Sprintf("key-%d", i), i, 1, "stale"))
	}
	c.InvalidateTags("stale")
	c.Wait()

	// No old value may resurface, including ones that were queued.
	for i := 0; i < 100; i++ {
		_, ok := c.Get(fmt.Sprintf("key-%d", i))
		require.False(t, ok, "key-%d resurfaced after invalidation", i)
	}

	// Repeatedly overwrite the same key and invalidate without waiting.
	for i := 0; i < 1000; i++ {
		c.SetWithTags("hot", i, 1, "hot-tag")
	}
	c.InvalidateTags("hot-tag")
	c.Wait()
	_, ok := c.Get("hot")
	require.False(t, ok, "queued Set resurrected an invalidated value")

	// The write queue is healthy afterwards: new writes go through.
	require.True(t, c.SetWithTags("fresh", 1, 1, "hot-tag"))
	c.Wait()
	value, ok := c.Get("fresh")
	require.True(t, ok)
	require.Equal(t, 1, value)
}

func TestTagsReplacedOnUpdate(t *testing.T) {
	c := newTagTestCache(t)

	require.True(t, c.SetWithTags("k", 1, 1, "old"))
	c.Wait()
	require.Len(t, c.tags.keys("old"), 1)

	// Re-setting the key replaces its tag set.
	require.True(t, c.SetWithTags("k", 2, 1, "new"))
	c.Wait()
	require.Empty(t, c.tags.keys("old"))
	require.Len(t, c.tags.keys("new"), 1)

	// The old tag no longer reaches the key.
	c.InvalidateTags("old")
	value, ok := c.Get("k")
	require.True(t, ok)
	require.Equal(t, 2, value)

	c.InvalidateTags("new")
	_, ok = c.Get("k")
	require.False(t, ok)

	// A plain Set drops all previous tag associations.
	require.True(t, c.SetWithTags("k2", 1, 1, "tag"))
	c.Wait()
	require.True(t, c.Set("k2", 2, 1))
	c.Wait()
	require.Empty(t, c.tags.keys("tag"))
	c.InvalidateTags("tag")
	value, ok = c.Get("k2")
	require.True(t, ok)
	require.Equal(t, 2, value)
}

func TestTagsRemovedOnDel(t *testing.T) {
	c := newTagTestCache(t)

	require.True(t, c.SetWithTags("k", 1, 1, "tag"))
	c.Wait()
	c.Del("k")
	require.Empty(t, c.tags.keys("tag"))

	// Re-setting the key without tags must not resurrect the association.
	require.True(t, c.Set("k", 2, 1))
	c.Wait()
	c.InvalidateTags("tag")
	value, ok := c.Get("k")
	require.True(t, ok)
	require.Equal(t, 2, value)
}

func TestTagsRemovedOnExpiration(t *testing.T) {
	c := newTagTestCache(t)

	require.True(t, c.SetWithTTLAndTags("k", 1, 1, time.Hour, "tag"))
	c.Wait()
	require.Len(t, c.tags.keys("tag"), 1)

	// Force the item to be expired and run the same cleanup path that
	// processItems runs on the cleanup ticker.
	keyHash, conflictHash := c.keyToHash("k")
	c.storedItems.Set(&Item[int]{
		Key:        keyHash,
		Conflict:   conflictHash,
		Value:      1,
		Expiration: time.Now().Add(-time.Hour),
	})
	sharded, ok := c.storedItems.(*shardedMap[int])
	require.True(t, ok)
	sharded.expiryMap.lastCleanedBucketNum = cleanupBucket(time.Now().Add(-2 * time.Hour))
	c.storedItems.Cleanup(c.cachePolicy, c.evictItem)

	// Natural expiration cleaned up the tag index.
	require.Empty(t, c.tags.keys("tag"))
	_, ok = c.Get("k")
	require.False(t, ok)
}

func TestInvalidateTagsRunsCallbacks(t *testing.T) {
	var mu sync.Mutex
	evicted := make(map[uint64]int)
	c, err := NewCache(&Config[string, int]{
		NumCounters:        1000,
		MaxCost:            10000,
		BufferItems:        64,
		IgnoreInternalCost: true,
		OnEvict: func(item *Item[int]) {
			mu.Lock()
			defer mu.Unlock()
			evicted[item.Key] = item.Value
		},
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.True(t, c.SetWithTags("k1", 1, 1, "tag"))
	require.True(t, c.SetWithTags("k2", 2, 1, "tag"))
	c.Wait()

	c.InvalidateTags("tag")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, evicted, 2)
}

func TestInvalidateTagsNoop(t *testing.T) {
	c := newTagTestCache(t)

	// Unknown tags and empty calls are no-ops and leave the cache usable.
	c.InvalidateTags("no-such-tag")
	c.InvalidateTags()

	require.True(t, c.SetWithTags("k", 1, 1, "tag"))
	c.Wait()
	value, ok := c.Get("k")
	require.True(t, ok)
	require.Equal(t, 1, value)

	// Clear drops all tag associations.
	c.Clear()
	require.Empty(t, c.tags.keys("tag"))
}
