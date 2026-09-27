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

// borrowTestResource models a cache value that must be closed exactly once.
type borrowTestResource struct {
	id     int
	closed atomic.Int32
}

func newBorrowCache(t *testing.T, maxCost int64) *Cache[int, *borrowTestResource] {
	t.Helper()
	c, err := NewCache(&Config[int, *borrowTestResource]{
		NumCounters:        100,
		MaxCost:            maxCost,
		BufferItems:        64,
		IgnoreInternalCost: true,
		Metrics:            true,
		OnExit: func(v *borrowTestResource) {
			if v != nil {
				v.closed.Add(1)
			}
		},
	})
	require.NoError(t, err)
	return c
}

// TestBorrowReplaceDefersRelease verifies that replacing a borrowed value
// logically evicts it immediately but defers its release until the last
// borrower releases it, and that the new value's references are tracked
// separately.
func TestBorrowReplaceDefersRelease(t *testing.T) {
	c := newBorrowCache(t, 100)
	defer c.Close()

	oldVal := &borrowTestResource{id: 1}
	require.True(t, c.Set(1, oldVal, 1))
	c.Wait()

	b, ok := c.Borrow(1)
	require.True(t, ok)
	require.Same(t, oldVal, b.Value)
	require.Equal(t, int64(1), c.BorrowedCost())
	require.Equal(t, int64(1), c.BorrowedItems())

	// Replace the value while the old one is borrowed.
	newVal := &borrowTestResource{id: 2}
	require.True(t, c.Set(1, newVal, 1))
	c.Wait()

	// Logical eviction is immediate: Get returns the new value.
	got, ok := c.Get(1)
	require.True(t, ok)
	require.Same(t, newVal, got)

	// The old value is pinned: logically evicted but not yet released.
	require.Equal(t, int32(0), oldVal.closed.Load())
	require.Equal(t, int64(1), c.PinnedItems())
	require.Equal(t, int64(1), c.PinnedCost())

	// Borrowing the new value tracks a separate reference.
	nb, ok := c.Borrow(1)
	require.True(t, ok)
	require.Same(t, newVal, nb.Value)

	// Releasing the old borrow releases the old value exactly once and
	// does not count against the new value's references.
	b.Release()
	require.Equal(t, int32(1), oldVal.closed.Load())
	require.Equal(t, int32(0), newVal.closed.Load())
	require.Equal(t, int64(0), c.PinnedItems())
	require.Equal(t, int64(0), c.PinnedCost())

	// Releasing the new borrow does not release the new value, it is
	// still cached.
	nb.Release()
	require.Equal(t, int32(0), newVal.closed.Load())

	// A double release is a no-op.
	b.Release()
	require.Equal(t, int32(1), oldVal.closed.Load())

	// Deleting the new value releases it immediately (no outstanding
	// borrows).
	c.Del(1)
	c.Wait()
	require.Equal(t, int32(1), newVal.closed.Load())
	require.Equal(t, int64(0), c.BorrowedCost())
	require.Equal(t, int64(0), c.BorrowedItems())
}

// TestBorrowTTLExpiryDefersRelease verifies that a borrowed value whose TTL
// expires is logically evicted but only released after the last borrow is
// released.
func TestBorrowTTLExpiryDefersRelease(t *testing.T) {
	c, err := NewCache(&Config[int, *borrowTestResource]{
		NumCounters:            100,
		MaxCost:                100,
		BufferItems:            64,
		IgnoreInternalCost:     true,
		Metrics:                true,
		TtlTickerDurationInSec: 1,
		OnExit: func(v *borrowTestResource) {
			if v != nil {
				v.closed.Add(1)
			}
		},
	})
	require.NoError(t, err)
	defer c.Close()

	oldVal := &borrowTestResource{id: 1}
	require.True(t, c.SetWithTTL(1, oldVal, 1, time.Second))
	c.Wait()

	b, ok := c.Borrow(1)
	require.True(t, ok)

	// Wait for the TTL cleanup to logically evict the expired key.
	require.Eventually(t, func() bool {
		if _, ok := c.Get(1); ok {
			return false
		}
		return c.PinnedItems() == 1
	}, 20*time.Second, 100*time.Millisecond)

	// Expired and evicted, but still borrowed: not released yet.
	require.Equal(t, int32(0), oldVal.closed.Load())
	require.Equal(t, int64(1), c.PinnedCost())

	b.Release()
	require.Equal(t, int32(1), oldVal.closed.Load())
	require.Equal(t, int64(0), c.PinnedItems())
	require.Equal(t, int64(0), c.PinnedCost())
}

// TestBorrowDelDefersRelease verifies that deleting a borrowed value
// removes it from the cache immediately but defers its release.
func TestBorrowDelDefersRelease(t *testing.T) {
	c := newBorrowCache(t, 100)
	defer c.Close()

	oldVal := &borrowTestResource{id: 1}
	require.True(t, c.Set(1, oldVal, 1))
	c.Wait()

	b, ok := c.Borrow(1)
	require.True(t, ok)

	c.Del(1)

	// Logical deletion is immediate.
	_, ok = c.Get(1)
	require.False(t, ok)

	// But the borrowed value is not released yet.
	require.Equal(t, int32(0), oldVal.closed.Load())
	require.Equal(t, int64(1), c.PinnedItems())
	require.Equal(t, int64(1), c.PinnedCost())

	b.Release()
	require.Equal(t, int32(1), oldVal.closed.Load())
	require.Equal(t, int64(0), c.PinnedItems())
	require.Equal(t, int64(0), c.PinnedCost())

	// The deferred release runs exactly once.
	c.Wait()
	require.Equal(t, int32(1), oldVal.closed.Load())
}

// TestBorrowCloseDefersRelease verifies that closing the cache with an
// outstanding borrow defers the value's release until the borrow is
// released.
func TestBorrowCloseDefersRelease(t *testing.T) {
	c := newBorrowCache(t, 100)

	oldVal := &borrowTestResource{id: 1}
	require.True(t, c.Set(1, oldVal, 1))
	c.Wait()

	b, ok := c.Borrow(1)
	require.True(t, ok)

	c.Close()

	// The cache is closed and cleared, but the borrowed value is still
	// pinned and not released.
	require.Equal(t, int32(0), oldVal.closed.Load())
	require.Equal(t, int64(1), c.PinnedItems())
	require.Equal(t, int64(1), c.PinnedCost())

	b.Release()
	require.Equal(t, int32(1), oldVal.closed.Load())
	require.Equal(t, int64(0), c.PinnedItems())
	require.Equal(t, int64(0), c.PinnedCost())
}

// TestBorrowMultipleBorrowsReleaseOnce verifies that with multiple
// outstanding borrows the value is released exactly once, after the last
// release.
func TestBorrowMultipleBorrowsReleaseOnce(t *testing.T) {
	c := newBorrowCache(t, 100)
	defer c.Close()

	v := &borrowTestResource{id: 1}
	require.True(t, c.Set(1, v, 1))
	c.Wait()

	const n = 5
	handles := make([]*Borrowed[*borrowTestResource], n)
	for i := range handles {
		h, ok := c.Borrow(1)
		require.True(t, ok)
		handles[i] = h
	}
	require.Equal(t, int64(n), c.BorrowedItems())
	require.Equal(t, int64(n), c.BorrowedCost())

	c.Del(1)
	require.Equal(t, int64(1), c.PinnedItems())
	require.Equal(t, int64(1), c.PinnedCost())

	for i, h := range handles {
		h.Release()
		if i < n-1 {
			require.Equal(t, int32(0), v.closed.Load())
		}
	}
	require.Equal(t, int32(1), v.closed.Load())
	require.Equal(t, int64(0), c.PinnedItems())
	require.Equal(t, int64(0), c.PinnedCost())
	require.Equal(t, int64(0), c.BorrowedItems())
	require.Equal(t, int64(0), c.BorrowedCost())
}

// TestBorrowCostAccountingAndLimits verifies that borrowed and pinned costs
// are observable, that new borrows are rejected once the borrowed cost
// budget is exhausted, and that new writes are rejected while pinned cost
// exhausts the cache budget.
func TestBorrowCostAccountingAndLimits(t *testing.T) {
	var rejected atomic.Int32
	c, err := NewCache(&Config[int, *borrowTestResource]{
		NumCounters:        100,
		MaxCost:            10,
		BufferItems:        64,
		IgnoreInternalCost: true,
		Metrics:            true,
		OnExit: func(v *borrowTestResource) {
			if v != nil {
				v.closed.Add(1)
			}
		},
		OnReject: func(item *Item[*borrowTestResource]) {
			rejected.Add(1)
		},
	})
	require.NoError(t, err)
	defer c.Close()

	a := &borrowTestResource{id: 1}
	b := &borrowTestResource{id: 2}
	require.True(t, c.Set(1, a, 6))
	require.True(t, c.Set(2, b, 4))
	c.Wait()

	// Outstanding borrows account their cost against MaxCost.
	ba, ok := c.Borrow(1)
	require.True(t, ok)
	require.Equal(t, int64(6), c.BorrowedCost())
	bb, ok := c.Borrow(2)
	require.True(t, ok)
	require.Equal(t, int64(10), c.BorrowedCost())
	require.Equal(t, int64(2), c.BorrowedItems())

	// The borrowed cost budget (MaxCost) is exhausted: new borrows are
	// rejected and the rejections are observable.
	_, ok = c.Borrow(1)
	require.False(t, ok)
	_, ok = c.Borrow(2)
	require.False(t, ok)
	require.Equal(t, uint64(2), c.Metrics.BorrowsRejected())

	// Releasing a borrow makes room for new borrows again.
	ba.Release()
	require.Equal(t, int64(4), c.BorrowedCost())
	require.Equal(t, int32(0), a.closed.Load())
	ba2, ok := c.Borrow(1)
	require.True(t, ok)
	require.Equal(t, int64(10), c.BorrowedCost())

	// Logically evicting borrowed values keeps their cost pinned against
	// the budget.
	c.Del(1)
	c.Del(2)
	c.Wait()
	require.Equal(t, int32(0), a.closed.Load())
	require.Equal(t, int32(0), b.closed.Load())
	require.Equal(t, int64(10), c.PinnedCost())
	require.Equal(t, int64(2), c.PinnedItems())
	require.Equal(t, int64(0), c.RemainingCost())

	// New writes are rejected while pinned cost exhausts the budget, and
	// the rejection is observable via OnReject.
	d := &borrowTestResource{id: 3}
	require.True(t, c.Set(3, d, 1))
	c.Wait()
	_, ok = c.Get(3)
	require.False(t, ok)
	require.Equal(t, int32(1), rejected.Load())
	require.Equal(t, int32(1), d.closed.Load())

	// Releasing the last borrows releases the pinned values and frees the
	// budget for new writes.
	ba2.Release()
	require.Equal(t, int32(1), a.closed.Load())
	require.Equal(t, int64(4), c.PinnedCost())
	bb.Release()
	require.Equal(t, int32(1), b.closed.Load())
	require.Equal(t, int64(0), c.PinnedCost())
	require.Equal(t, int64(0), c.BorrowedCost())
	require.Equal(t, int64(0), c.BorrowedItems())

	d2 := &borrowTestResource{id: 4}
	require.True(t, c.Set(3, d2, 1))
	c.Wait()
	got, ok := c.Get(3)
	require.True(t, ok)
	require.Same(t, d2, got)
}

// TestGetDoesNotBorrow verifies that a plain Get does not get the
// Borrow/Release lifecycle guarantee: the value is released as soon as it
// leaves the cache.
func TestGetDoesNotBorrow(t *testing.T) {
	c := newBorrowCache(t, 100)
	defer c.Close()

	v := &borrowTestResource{id: 1}
	require.True(t, c.Set(1, v, 1))
	c.Wait()

	got, ok := c.Get(1)
	require.True(t, ok)
	require.Same(t, v, got)

	// Get does not register any borrow.
	require.Equal(t, int64(0), c.BorrowedItems())
	require.Equal(t, int64(0), c.BorrowedCost())

	// The value is released as soon as it is deleted, even though the
	// caller still holds the reference returned by Get.
	c.Del(1)
	require.Equal(t, int32(1), v.closed.Load())
	require.Equal(t, int64(0), c.PinnedItems())
}
