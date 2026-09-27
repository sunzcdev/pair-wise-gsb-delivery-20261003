/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestCacheForTags(t *testing.T) *Cache[string, string] {
	t.Helper()
	c, err := NewCache(&Config[string, string]{
		NumCounters:        1000,
		MaxCost:            1000,
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func TestTagIndex(t *testing.T) {
	idx := newTagIndex()

	gen1 := idx.set(1, []string{"a", "b"})
	genA := idx.set(2, []string{"a"})
	require.Contains(t, idx.byTag["a"], uint64(1))
	require.Contains(t, idx.byTag["a"], uint64(2))
	require.Contains(t, idx.byTag["b"], uint64(1))

	// Replacing the tags of a key removes it from its previous tags.
	gen2 := idx.set(1, []string{"c"})
	require.NotEqual(t, gen1, gen2)
	require.NotContains(t, idx.byTag, "b")
	require.NotContains(t, idx.byTag["a"], uint64(1))
	require.Contains(t, idx.byTag["c"], uint64(1))

	// delIfGen only removes the entry if the generation matches.
	idx.delIfGen(2, gen1)
	require.Contains(t, idx.byKey, uint64(2))
	idx.delIfGen(2, gen2)
	require.Contains(t, idx.byKey, uint64(2))

	// delIfApplied only removes applied entries.
	idx.delIfApplied(2)
	require.Contains(t, idx.byKey, uint64(2))
	idx.markApplied(2, genA)
	idx.delIfApplied(2)
	require.NotContains(t, idx.byKey, uint64(2))
	require.NotContains(t, idx.byTag, "a")

	// take removes the tags and all keys registered under them.
	idx.set(3, []string{"x", "y"})
	idx.set(4, []string{"y"})
	keys := idx.take("x")
	require.Equal(t, map[uint64]struct{}{3: {}}, keys)
	require.NotContains(t, idx.byKey, uint64(3))
	require.NotContains(t, idx.byTag["y"], uint64(3))
	require.Contains(t, idx.byTag["y"], uint64(4))

	idx.clear()
	require.Empty(t, idx.byTag)
	require.Empty(t, idx.byKey)
}

func TestSetWithTagsAndInvalidateTags(t *testing.T) {
	c := newTestCacheForTags(t)

	require.True(t, c.SetWithTags("k1", "v1", 1, "user:1", "group:a"))
	require.True(t, c.SetWithTags("k2", "v2", 1, "user:1"))
	require.True(t, c.SetWithTags("k3", "v3", 1, "group:a"))
	require.True(t, c.Set("k4", "v4", 1))
	c.Wait()

	c.InvalidateTags("user:1")

	_, ok := c.Get("k1")
	require.False(t, ok, "k1 must be invalidated via user:1")
	_, ok = c.Get("k2")
	require.False(t, ok, "k2 must be invalidated via user:1")
	v, ok := c.Get("k3")
	require.True(t, ok, "k3 only shares group:a with k1 and must survive")
	require.Equal(t, "v3", v)
	v, ok = c.Get("k4")
	require.True(t, ok, "untagged k4 must survive")
	require.Equal(t, "v4", v)

	// The tag index entries of invalidated keys must be gone.
	require.NotContains(t, c.tags.byTag, "user:1")
	c.InvalidateTags("group:a")
	_, ok = c.Get("k3")
	require.False(t, ok)
	require.Empty(t, c.tags.byTag)
	require.Empty(t, c.tags.byKey)
}

// TestInvalidateTagsQueuedSet verifies that a Set still sitting in the
// asynchronous write queue cannot reappear after its tag is invalidated.
func TestInvalidateTagsQueuedSet(t *testing.T) {
	c := newTestCacheForTags(t)

	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key-%d", i)
		// Don't Wait: the Set may still be queued when invalidation runs.
		require.True(t, c.SetWithTags(key, "old", 1, "stale"))
		c.InvalidateTags("stale")
	}
	c.Wait()

	for i := 0; i < 1000; i++ {
		_, ok := c.Get(fmt.Sprintf("key-%d", i))
		require.False(t, ok, "old value of key-%d must not reappear", i)
	}
	require.Empty(t, c.tags.byTag)
	require.Empty(t, c.tags.byKey)

	// The queue must be operational again after the invalidations.
	require.True(t, c.SetWithTags("new", "value", 1, "stale"))
	c.Wait()
	v, ok := c.Get("new")
	require.True(t, ok)
	require.Equal(t, "value", v)
}

// TestInvalidateTagsKeepsUntouchedQueuedSets verifies that queued writes not
// affected by the invalidation survive the queue drain.
func TestInvalidateTagsKeepsUntouchedQueuedSets(t *testing.T) {
	c := newTestCacheForTags(t)

	for i := 0; i < 500; i++ {
		require.True(t, c.SetWithTags(fmt.Sprintf("stale-%d", i), "old", 1, "stale"))
		require.True(t, c.SetWithTags(fmt.Sprintf("fresh-%d", i), "new", 1, "fresh"))
		c.InvalidateTags("stale")
	}
	c.Wait()

	for i := 0; i < 500; i++ {
		_, ok := c.Get(fmt.Sprintf("stale-%d", i))
		require.False(t, ok)
		v, ok := c.Get(fmt.Sprintf("fresh-%d", i))
		require.True(t, ok, "unaffected queued write fresh-%d must survive", i)
		require.Equal(t, "new", v)
	}
}

func TestTagsReplaceCleansIndex(t *testing.T) {
	c := newTestCacheForTags(t)

	require.True(t, c.SetWithTags("k", "v1", 1, "old-tag"))
	c.Wait()
	// Replacing the value also replaces the tag set.
	require.True(t, c.SetWithTags("k", "v2", 1, "new-tag"))
	c.Wait()

	c.InvalidateTags("old-tag")
	v, ok := c.Get("k")
	require.True(t, ok, "k must survive the invalidation of its replaced tag")
	require.Equal(t, "v2", v)

	// A plain Set replaces the value and clears the tags.
	require.True(t, c.Set("k", "v3", 1))
	c.Wait()
	c.InvalidateTags("new-tag")
	v, ok = c.Get("k")
	require.True(t, ok, "k must survive the invalidation of a tag cleared by Set")
	require.Equal(t, "v3", v)
	require.Empty(t, c.tags.byTag)
	require.Empty(t, c.tags.byKey)
}

func TestTagsDelCleansIndex(t *testing.T) {
	c := newTestCacheForTags(t)

	require.True(t, c.SetWithTags("k", "v", 1, "tag"))
	c.Wait()
	c.Del("k")
	require.Empty(t, c.tags.byTag)
	require.Empty(t, c.tags.byKey)

	// Invalidating afterwards must be a no-op and must not panic.
	c.InvalidateTags("tag")
}

func TestTagsExpiryCleansIndex(t *testing.T) {
	// Shrink the expiration buckets so the periodic cleanup runs quickly.
	defer func(prev int64) { bucketDurationSecs = prev }(bucketDurationSecs)
	bucketDurationSecs = 1

	c, err := NewCache(&Config[string, string]{
		NumCounters:            1000,
		MaxCost:                1000,
		BufferItems:            64,
		IgnoreInternalCost:     true,
		TtlTickerDurationInSec: 1,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.True(t, c.SetWithTTLAndTags("k", "v", 1, 2*time.Second, "tag"))
	c.Wait()

	require.Eventually(t, func() bool {
		c.tags.mu.Lock()
		defer c.tags.mu.Unlock()
		return len(c.tags.byKey) == 0 && len(c.tags.byTag) == 0
	}, 15*time.Second, 100*time.Millisecond, "natural expiration must clean the tag index")
}

func TestTagsEvictionCleansIndex(t *testing.T) {
	c, err := NewCache(&Config[int, int]{
		NumCounters:        1000,
		MaxCost:            10,
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.True(t, c.SetWithTags(0, 0, 1, "tag"))
	c.Wait()
	// Raise the frequency of other keys so the policy evicts key 0.
	for i := 1; i <= 100; i++ {
		for j := 0; j < 3; j++ {
			c.Get(i)
		}
		c.Set(i, i, 1)
	}
	c.Wait()

	require.Eventually(t, func() bool {
		c.tags.mu.Lock()
		defer c.tags.mu.Unlock()
		return len(c.tags.byKey) == 0
	}, 5*time.Second, 10*time.Millisecond, "eviction must clean the tag index")
}

func TestInvalidateTagsCallbacks(t *testing.T) {
	var evicted, exited atomic.Int32
	c, err := NewCache(&Config[string, string]{
		NumCounters:        1000,
		MaxCost:            1000,
		BufferItems:        64,
		IgnoreInternalCost: true,
		OnEvict:            func(item *Item[string]) { evicted.Add(1) },
		OnExit:             func(val string) { exited.Add(1) },
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)

	require.True(t, c.SetWithTags("k1", "v1", 1, "tag"))
	require.True(t, c.SetWithTags("k2", "v2", 1, "tag"))
	c.Wait()

	c.InvalidateTags("tag")

	// Callbacks are invoked asynchronously; they must eventually observe
	// both invalidated values.
	require.Eventually(t, func() bool {
		return evicted.Load() == 2 && exited.Load() == 2
	}, 5*time.Second, time.Millisecond)
}

func TestInvalidateTagsEdgeCases(t *testing.T) {
	c := newTestCacheForTags(t)

	// No tags and unknown tags are no-ops.
	c.InvalidateTags()
	c.InvalidateTags("unknown")

	// Invalidating on a closed cache must not panic or block.
	c.Close()
	c.InvalidateTags("tag")

	var nilCache *Cache[string, string]
	nilCache.InvalidateTags("tag")
}

// TestInvalidateTagsConcurrent hammers the cache with tagged Sets,
// invalidations, Dels and Gets from multiple goroutines. It is meant to be
// run with -race and must not deadlock or corrupt the tag index.
func TestInvalidateTagsConcurrent(t *testing.T) {
	c := newTestCacheForTags(t)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("key-%d-%d", g, i%50)
				tag := fmt.Sprintf("tag-%d", i%10)
				c.SetWithTags(key, "value", 1, tag, "all")
				if i%7 == 0 {
					c.InvalidateTags(tag)
				}
				if i%5 == 0 {
					c.Del(key)
				}
				c.Get(key)
			}
		}(g)
	}
	wg.Wait()
	c.InvalidateTags("all")
	c.Wait()
}
