/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// closable is a test value whose Close calls can be counted.
type closable struct {
	closes atomic.Int32
}

func (c *closable) Close() {
	if c == nil {
		return
	}
	c.closes.Add(1)
}

func (c *closable) CloseCount() int32 {
	return c.closes.Load()
}

func newBorrowCache(t *testing.T, maxCost int64) *Cache[string, *closable] {
	t.Helper()
	cache, err := NewCache(&Config[string, *closable]{
		NumCounters:        1000,
		MaxCost:            maxCost,
		BufferItems:        64,
		Metrics:            true,
		IgnoreInternalCost: true,
		OnExit: func(v *closable) {
			v.Close()
		},
	})
	require.NoError(t, err)
	return cache
}

// cleanupExpired runs the TTL cleanup the same way processItems does.
func cleanupExpired[K Key, V any](c *Cache[K, V]) {
	c.storedItems.Cleanup(c.cachePolicy, func(i *Item[V]) {
		c.releaseValue(i.Key, i.Value, func() { c.onEvict(i) })
	})
}

func TestBorrowReplaceDefersRelease(t *testing.T) {
	cache := newBorrowCache(t, 100)
	defer cache.Close()

	oldVal := &closable{}
	require.True(t, cache.Set("k", oldVal, 10))
	cache.Wait()

	b, ok := cache.Borrow("k")
	require.True(t, ok)
	require.Same(t, oldVal, b.Value)

	// Replace the value while the old one is still borrowed.
	newVal := &closable{}
	require.True(t, cache.Set("k", newVal, 10))
	cache.Wait()

	// Logical eviction is immediate: the new value is visible.
	got, ok := cache.Get("k")
	require.True(t, ok)
	require.Same(t, newVal, got)
	// Resource release of the old value waits for the last borrow.
	require.Equal(t, int32(0), oldVal.CloseCount())

	b.Release()
	require.Equal(t, int32(1), oldVal.CloseCount())

	// Borrows of the new value are tracked independently of the old one:
	// the old value's borrows must not count towards the new value.
	b2, ok := cache.Borrow("k")
	require.True(t, ok)
	require.Same(t, newVal, b2.Value)
	cache.Del("k")
	cache.Wait()
	require.Equal(t, int32(0), newVal.CloseCount())
	b2.Release()
	require.Equal(t, int32(1), newVal.CloseCount())
	require.Equal(t, int32(1), oldVal.CloseCount())
}

func TestBorrowTTLExpiryDefersRelease(t *testing.T) {
	// Shrink the expiration buckets so the expired item becomes eligible
	// for cleanup quickly.
	oldBucketDuration := bucketDurationSecs
	bucketDurationSecs = 1
	defer func() { bucketDurationSecs = oldBucketDuration }()

	cache := newBorrowCache(t, 100)
	defer cache.Close()

	v := &closable{}
	require.True(t, cache.SetWithTTL("k", v, 10, time.Second))
	cache.Wait()

	b, ok := cache.Borrow("k")
	require.True(t, ok)

	// Wait for the item to expire and for its expiration bucket to become
	// eligible for cleanup, then run the cleanup while it is borrowed.
	time.Sleep(2200 * time.Millisecond)
	cleanupExpired(cache)

	// Logical eviction is immediate...
	_, ok = cache.Get("k")
	require.False(t, ok)
	// ...but the value is not released until the borrow ends.
	require.Equal(t, int32(0), v.CloseCount())

	b.Release()
	require.Equal(t, int32(1), v.CloseCount())
}

func TestBorrowDelDefersRelease(t *testing.T) {
	cache := newBorrowCache(t, 100)
	defer cache.Close()

	v := &closable{}
	require.True(t, cache.Set("k", v, 10))
	cache.Wait()

	b, ok := cache.Borrow("k")
	require.True(t, ok)

	cache.Del("k")
	cache.Wait()

	_, ok = cache.Get("k")
	require.False(t, ok)
	require.Equal(t, int32(0), v.CloseCount())

	b.Release()
	require.Equal(t, int32(1), v.CloseCount())

	// Releasing the same handle again must not release the value twice.
	b.Release()
	require.Equal(t, int32(1), v.CloseCount())
}

func TestBorrowCloseDefersRelease(t *testing.T) {
	cache := newBorrowCache(t, 100)

	v := &closable{}
	require.True(t, cache.Set("k", v, 10))
	cache.Wait()

	b, ok := cache.Borrow("k")
	require.True(t, ok)

	// Delete and then close the cache while the value is borrowed. Both
	// are logical evictions of the same value, so it must be released
	// exactly once, and only after the borrow ends.
	cache.Del("k")
	cache.Wait()
	cache.Close()
	require.Equal(t, int32(0), v.CloseCount())

	b.Release()
	require.Equal(t, int32(1), v.CloseCount())
}

func TestBorrowMiss(t *testing.T) {
	cache := newBorrowCache(t, 100)
	defer cache.Close()

	b, ok := cache.Borrow("missing")
	require.False(t, ok)
	require.Nil(t, b)
}

func TestBorrowCostAccounting(t *testing.T) {
	cache := newBorrowCache(t, 100)
	defer cache.Close()

	require.True(t, cache.Set("a", &closable{}, 40))
	require.True(t, cache.Set("b", &closable{}, 30))
	require.True(t, cache.Set("c", &closable{}, 20))
	require.True(t, cache.Set("d", &closable{}, 10))
	cache.Wait()

	// Borrowed values occupy cost, observable via BorrowedCost.
	ba, ok := cache.Borrow("a")
	require.True(t, ok)
	require.Equal(t, int64(40), cache.BorrowedCost())
	bb, ok := cache.Borrow("b")
	require.True(t, ok)
	bc, ok := cache.Borrow("c")
	require.True(t, ok)
	require.Equal(t, int64(90), cache.BorrowedCost())

	// A logically deleted value that is still borrowed keeps occupying cost.
	cache.Del("a")
	cache.Wait()
	require.Equal(t, int64(90), cache.BorrowedCost())

	// Writes that would push the borrowed cost over MaxCost are rejected...
	require.False(t, cache.Set("e", &closable{}, 40))
	require.Equal(t, uint64(1), cache.Metrics.SetsRejectedBorrow())
	// ...while writes within the limit are accepted.
	require.True(t, cache.Set("e", &closable{}, 10))
	cache.Wait()

	be, ok := cache.Borrow("e")
	require.True(t, ok)
	require.Equal(t, int64(100), cache.BorrowedCost())

	// With the borrowed cost at MaxCost, new borrows are rejected, even
	// though the value is present in the cache.
	bd, ok := cache.Borrow("d")
	require.False(t, ok)
	require.Nil(t, bd)
	require.Equal(t, uint64(1), cache.Metrics.BorrowsRejected())
	// And so are new writes.
	require.False(t, cache.Set("f", &closable{}, 1))
	require.Equal(t, uint64(2), cache.Metrics.SetsRejectedBorrow())

	// Releasing borrows frees the borrowed cost.
	ba.Release()
	bb.Release()
	bc.Release()
	be.Release()
	require.Equal(t, int64(0), cache.BorrowedCost())

	// Borrows and writes succeed again once the cost is freed.
	bd, ok = cache.Borrow("d")
	require.True(t, ok)
	bd.Release()
	require.True(t, cache.Set("f", &closable{}, 1))
	require.Equal(t, uint64(5), cache.Metrics.Borrows())
}

func TestGetDoesNotBorrow(t *testing.T) {
	cache := newBorrowCache(t, 100)
	defer cache.Close()

	v := &closable{}
	require.True(t, cache.Set("k", v, 10))
	cache.Wait()

	got, ok := cache.Get("k")
	require.True(t, ok)
	require.Same(t, v, got)

	// Get does not pin the value: deletion releases it immediately, even
	// though the caller still holds the returned value.
	cache.Del("k")
	cache.Wait()
	require.Equal(t, int32(1), v.CloseCount())
	require.Equal(t, int64(0), cache.BorrowedCost())
}
