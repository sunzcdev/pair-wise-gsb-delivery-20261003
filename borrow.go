/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"sync/atomic"
)

// borrowTracker holds the cache-level accounting for the Borrow/Release
// lifecycle. All counters are observable through the corresponding Cache
// methods.
type borrowTracker struct {
	// borrowedCost is the sum of costs of all outstanding borrows. Each
	// Borrow adds the borrowed value's cost and each Release removes it.
	borrowedCost atomic.Int64
	// borrowedItems is the number of outstanding borrows.
	borrowedItems atomic.Int64
	// pinnedCost is the sum of costs of values that have been logically
	// evicted (replaced, deleted, expired, cleared) but whose resource
	// release is deferred because they are still borrowed. It counts
	// against the cache's MaxCost budget.
	pinnedCost atomic.Int64
	// pinnedItems is the number of logically evicted values still pinned
	// by outstanding borrows.
	pinnedItems atomic.Int64
}

func newBorrowTracker() *borrowTracker {
	return &borrowTracker{}
}

// refEntry tracks the references held on a single cached value. The cache
// itself holds one reference while the value is stored; each outstanding
// borrow holds one more. When the last reference is dropped, the deferred
// eviction callback (if any) runs exactly once.
type refEntry[V any] struct {
	refs atomic.Int64
	cost int64
	// item and done are set by the eviction path before it drops the
	// cache's reference. They are only read by the goroutine that observes
	// refs reaching zero, which is synchronized through refs.
	item *Item[V]
	done func(*Item[V])
}

// Borrowed is a handle to a value obtained through Cache.Borrow. The value
// remains valid (it will not be released via OnEvict/OnExit) until Release
// is called, even if the key is replaced, deleted, expired or the cache is
// closed in the meantime. Release must be called once per Borrow; it is
// safe to call it multiple times (subsequent calls are no-ops).
type Borrowed[V any] struct {
	// Value is the borrowed cache value.
	Value V

	cost     int64
	entry    *refEntry[V]
	track    *borrowTracker
	released atomic.Bool
}

// Release returns the borrowed value to the cache. If the value was
// logically evicted while borrowed and this was the last outstanding
// borrow, the deferred eviction callback runs now.
func (b *Borrowed[V]) Release() {
	if b == nil || !b.released.CompareAndSwap(false, true) {
		return
	}
	b.track.borrowedCost.Add(-b.cost)
	b.track.borrowedItems.Add(-1)
	if b.entry.refs.Add(-1) == 0 {
		// The cache already dropped its reference, so the value was
		// logically evicted while borrowed. This was the last borrow, run
		// the deferred eviction callback.
		b.track.pinnedCost.Add(-b.entry.cost)
		b.track.pinnedItems.Add(-1)
		b.entry.done(b.entry.item)
	}
}

// Borrow returns a handle to the value stored for key, guaranteeing that
// the value's resource is not released (via OnEvict/OnExit) until
// Borrowed.Release is called. Logical eviction still happens immediately:
// the key may be replaced, deleted or expired while a borrow is active, but
// the old value's release is deferred until its last borrower releases it.
//
// Unlike Get, Borrow pins the value's lifecycle; plain Get does NOT get
// this guarantee. Each outstanding borrow accounts the value's cost against
// MaxCost (see Cache.BorrowedCost), and Borrow fails (returns false) when
// the borrowed cost budget is already exhausted. Logically evicted but
// still borrowed values also keep counting against MaxCost (see
// Cache.PinnedCost), which can cause new writes to be rejected by the
// admission policy.
//
// Every successful Borrow must be paired with exactly one Release.
func (c *Cache[K, V]) Borrow(key K) (*Borrowed[V], bool) {
	if c == nil || c.isClosed.Load() {
		return nil, false
	}
	keyHash, conflictHash := c.keyToHash(key)

	c.getBuf.Push(keyHash)
	si, ok := c.storedItems.peek(keyHash, conflictHash)
	if !ok {
		c.Metrics.add(miss, keyHash, 1)
		return nil, false
	}
	// Best-effort budget check: reject new borrows once the outstanding
	// borrowed cost reaches MaxCost.
	if c.borrows.borrowedCost.Load()+si.cost > c.cachePolicy.MaxCost() {
		c.Metrics.add(rejectBorrows, keyHash, 1)
		return nil, false
	}
	si, ok = c.storedItems.borrow(keyHash, conflictHash)
	if !ok {
		c.Metrics.add(miss, keyHash, 1)
		return nil, false
	}
	c.borrows.borrowedCost.Add(si.cost)
	c.borrows.borrowedItems.Add(1)
	c.Metrics.add(hit, keyHash, 1)
	return &Borrowed[V]{
		Value: si.value,
		cost:  si.cost,
		entry: si.entry,
		track: c.borrows,
	}, true
}

// evictItem runs done for an item whose value is going out of scope. If the
// value has outstanding borrows, the call is deferred until the last
// borrower releases it; the value's cost stays accounted as pinned until
// then. Values that were never borrowed are released immediately, exactly
// as before.
func (c *Cache[K, V]) evictItem(i *Item[V], done func(*Item[V])) {
	entry := i.entry
	if entry == nil {
		done(i)
		return
	}
	entry.item = i
	entry.done = done
	if entry.refs.Add(-1) > 0 {
		// Still borrowed: logically evicted but pinned until the last
		// Release.
		c.borrows.pinnedCost.Add(entry.cost)
		c.borrows.pinnedItems.Add(1)
		return
	}
	// No outstanding borrows, release immediately.
	done(i)
}

// exitValue adapts the onExit callback to the eviction helper.
func (c *Cache[K, V]) exitValue(i *Item[V]) {
	c.onExit(i.Value)
}

// BorrowedCost returns the sum of costs of all outstanding borrows. Each
// Borrow adds the borrowed value's cost and each Release removes it.
func (c *Cache[K, V]) BorrowedCost() int64 {
	if c == nil {
		return 0
	}
	return c.borrows.borrowedCost.Load()
}

// BorrowedItems returns the number of outstanding borrows.
func (c *Cache[K, V]) BorrowedItems() int64 {
	if c == nil {
		return 0
	}
	return c.borrows.borrowedItems.Load()
}

// PinnedCost returns the sum of costs of values that have been logically
// evicted but whose release is deferred because they are still borrowed.
// This cost counts against the cache's MaxCost budget (see RemainingCost)
// and can cause new writes to be rejected once the budget is exhausted.
func (c *Cache[K, V]) PinnedCost() int64 {
	if c == nil {
		return 0
	}
	return c.borrows.pinnedCost.Load()
}

// PinnedItems returns the number of logically evicted values still pinned
// by outstanding borrows.
func (c *Cache[K, V]) PinnedItems() int64 {
	if c == nil {
		return 0
	}
	return c.borrows.pinnedItems.Load()
}
