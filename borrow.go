/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"reflect"
	"sync"
)

// Borrowed is a handle to a cache value obtained through Cache.Borrow. The
// value is guaranteed to remain usable until Release is called: if the key is
// evicted, deleted, expired, replaced or the cache is closed while the value
// is borrowed, the logical eviction still takes effect immediately, but the
// configured OnEvict/OnExit resource-release callbacks for that value are
// deferred until the last outstanding borrow is released.
//
// A Borrowed handle must be released exactly once per successful Borrow and
// must not be shared across goroutines without external synchronization.
type Borrowed[V any] struct {
	// Value is the borrowed cache value.
	Value V

	reg   *borrowRegistry[V]
	entry *borrowEntry[V]
}

// Release ends the borrow. When the last borrow of a logically evicted value
// is released, the deferred OnEvict/OnExit callbacks for that value run
// exactly once. Release is idempotent: calling it more than once on the same
// handle has no effect.
func (b *Borrowed[V]) Release() {
	if b == nil || b.entry == nil {
		return
	}
	b.reg.release(b.entry)
	b.entry = nil
}

// borrowEntry tracks the outstanding borrows of a single cached value.
type borrowEntry[V any] struct {
	key   uint64
	value V
	cost  int64
	refs  int
	// evicted is set once the value has logically left the cache; release
	// then holds the resource-release callback to run when refs reaches 0.
	evicted bool
	release func()
}

// borrowRegistry tracks borrowed values so that resource release can be
// deferred until the last borrow of a value ends. The zero value is ready to
// use.
type borrowRegistry[V any] struct {
	mu sync.Mutex
	// entries maps key hashes to the borrow entries of values seen for that
	// key. Multiple entries per key exist when a value is replaced while an
	// older value is still borrowed.
	entries map[uint64][]*borrowEntry[V]
	// borrowedCost is the total cost of all currently borrowed values,
	// including values that were logically evicted but are still borrowed.
	borrowedCost int64
}

// borrowLocked registers a borrow of value under key. Callers must hold
// r.mu and must have read value from the store while holding it, so that a
// concurrent eviction cannot release the value before the borrow is
// registered. If an entry for the same value already exists it is shared.
// Borrowing is rejected when the borrowed cost would exceed limit.
func (r *borrowRegistry[V]) borrowLocked(key uint64, value V, cost, limit int64) (*borrowEntry[V], bool) {
	for _, e := range r.entries[key] {
		if !e.evicted && sameValue(e.value, value) {
			e.refs++
			return e, true
		}
	}
	if limit > 0 && r.borrowedCost+cost > limit {
		return nil, false
	}
	if r.entries == nil {
		r.entries = make(map[uint64][]*borrowEntry[V])
	}
	e := &borrowEntry[V]{key: key, value: value, cost: cost, refs: 1}
	r.entries[key] = append(r.entries[key], e)
	r.borrowedCost += cost
	return e, true
}

// release drops one borrow of e. When the last borrow of an evicted value
// ends, its deferred resource-release callback runs exactly once.
func (r *borrowRegistry[V]) release(e *borrowEntry[V]) {
	r.mu.Lock()
	e.refs--
	var fn func()
	if e.refs <= 0 {
		r.removeLocked(e)
		if e.evicted {
			fn = e.release
			e.release = nil
		}
	}
	r.mu.Unlock()
	// Run the user callback without holding the registry lock so it may
	// safely call back into the cache.
	if fn != nil {
		fn()
	}
}

func (r *borrowRegistry[V]) removeLocked(e *borrowEntry[V]) {
	list := r.entries[e.key]
	for i, other := range list {
		if other == e {
			r.entries[e.key] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(r.entries[e.key]) == 0 {
		delete(r.entries, e.key)
	}
	r.borrowedCost -= e.cost
}

// releaseValue runs fn to release value, unless value is currently borrowed.
// In that case fn runs when the last borrow ends. Duplicate eviction notices
// for the same value are dropped so fn runs at most once per value.
func (r *borrowRegistry[V]) releaseValue(key uint64, value V, fn func()) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	duplicate := false
	for _, e := range r.entries[key] {
		if !sameValue(e.value, value) {
			continue
		}
		if e.evicted {
			// This value was already logically evicted; ignore the
			// duplicate notice.
			duplicate = true
			continue
		}
		e.evicted = true
		e.release = fn
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if !duplicate {
		fn()
	}
}

// cost returns the total cost of all currently borrowed values.
func (r *borrowRegistry[V]) cost() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.borrowedCost
}

// sameValue reports whether a and b identify the same value. Comparable
// types are compared directly; reference kinds fall back to pointer
// identity. Values of other types (e.g. structs containing slices) cannot
// be identified and always compare unequal.
func sameValue[V any](a, b V) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() {
		return !va.IsValid() && !vb.IsValid()
	}
	if va.Type() != vb.Type() {
		return false
	}
	if va.Type().Comparable() {
		return va.Interface() == vb.Interface()
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return va.Pointer() == vb.Pointer()
	}
	return false
}

// Borrow returns a handle to the value stored for key, or false if the key
// is not in the cache or the borrow was rejected because too much cost is
// currently borrowed (see Cache.BorrowedCost). Unlike Get, Borrow guarantees
// that the returned value is not released (via OnEvict/OnExit) while it is
// borrowed, even if the key is evicted, deleted, expired, replaced or the
// cache is closed in the meantime. The caller must call Release on the
// returned handle once the value is no longer needed.
func (c *Cache[K, V]) Borrow(key K) (*Borrowed[V], bool) {
	if c == nil || c.isClosed.Load() {
		return nil, false
	}
	keyHash, conflictHash := c.keyToHash(key)
	c.getBuf.Push(keyHash)

	// The store lookup and the borrow registration must be atomic with
	// respect to eviction, otherwise the value could be released between
	// the two. Eviction paths take r.mu without holding any store lock, so
	// taking r.mu here before the store lock cannot deadlock.
	r := &c.borrows
	r.mu.Lock()
	value, ok := c.storedItems.Get(keyHash, conflictHash)
	if !ok {
		r.mu.Unlock()
		c.Metrics.add(miss, keyHash, 1)
		return nil, false
	}
	entry, admitted := r.borrowLocked(keyHash, value, c.cachePolicy.Cost(keyHash), c.cachePolicy.MaxCost())
	r.mu.Unlock()
	if !admitted {
		c.Metrics.add(rejectBorrow, keyHash, 1)
		return nil, false
	}
	c.Metrics.add(hit, keyHash, 1)
	c.Metrics.add(borrow, keyHash, 1)
	return &Borrowed[V]{Value: value, reg: r, entry: entry}, true
}

// BorrowedCost returns the total cost of all currently borrowed values,
// including values that have been logically evicted but are still borrowed.
// Borrowed cost counts against the cache's MaxCost: new borrows and new Sets
// are rejected when they would push the borrowed cost over MaxCost.
func (c *Cache[K, V]) BorrowedCost() int64 {
	if c == nil {
		return 0
	}
	return c.borrows.cost()
}

// releaseValue releases value (previously stored under key) by running fn,
// unless the value is currently borrowed, in which case fn is deferred until
// the last borrow ends. fn runs at most once per value.
func (c *Cache[K, V]) releaseValue(key uint64, value V, fn func()) {
	c.borrows.releaseValue(key, value, fn)
}
