/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"sync"
	"time"
)

type updateFn[V any] func(cur, prev V) bool

// TODO: Do we need this to be a separate struct from Item?
type storeItem[V any] struct {
	key        uint64
	conflict   uint64
	value      V
	cost       int64
	expiration time.Time
	entry      *refEntry[V]
}

// store is the interface fulfilled by all hash map implementations in this
// file. Some hash map implementations are better suited for certain data
// distributions than others, so this allows us to abstract that out for use
// in Ristretto.
//
// Every store is safe for concurrent usage.
type store[V any] interface {
	// Get returns the value associated with the key parameter.
	Get(uint64, uint64) (V, bool)
	// Expiration returns the expiration time for this key.
	Expiration(uint64) time.Time
	// Set adds the key-value pair to the Map or updates the value if it's
	// already present. The key-value pair is passed as a pointer to an
	// item object. It returns the value and borrow entry being replaced, if
	// any, so the caller can account for the old value going out of scope.
	Set(*Item[V]) (V, *refEntry[V], bool)
	// Del deletes the key-value pair from the Map.
	Del(uint64, uint64) (uint64, V, *refEntry[V])
	// Update attempts to update the key with a new value and returns true if
	// successful. It also returns the borrow entry of the replaced value, if
	// any.
	Update(*Item[V]) (V, *refEntry[V], bool)
	// peek returns the storeItem associated with the key without mutating
	// any borrow state.
	peek(uint64, uint64) (storeItem[V], bool)
	// borrow returns the storeItem associated with the key and registers a
	// new borrow on its borrow entry, creating the entry if needed.
	borrow(uint64, uint64) (storeItem[V], bool)
	// Cleanup removes items that have an expired TTL.
	Cleanup(policy *defaultPolicy[V], onEvict func(item *Item[V]))
	// Clear clears all contents of the store.
	Clear(onEvict func(item *Item[V]))
	SetShouldUpdateFn(f updateFn[V])
	// IterValues iterates the values of the Map, passing them to the callback.
	// It guarantees that any value in the Map will be visited only once.
	// The set of values visited by IterValues is non-deterministic.
	IterValues(cb func(v V) (stop bool))
}

// newStore returns the default store implementation.
func newStore[V any]() store[V] {
	return newShardedMap[V]()
}

const numShards uint64 = 256

type shardedMap[V any] struct {
	shards    []*lockedMap[V]
	expiryMap *expirationMap[V]
}

func newShardedMap[V any]() *shardedMap[V] {
	sm := &shardedMap[V]{
		shards:    make([]*lockedMap[V], int(numShards)),
		expiryMap: newExpirationMap[V](),
	}
	for i := range sm.shards {
		sm.shards[i] = newLockedMap[V](sm.expiryMap)
	}
	return sm
}

func (m *shardedMap[V]) SetShouldUpdateFn(f updateFn[V]) {
	for i := range m.shards {
		m.shards[i].setShouldUpdateFn(f)
	}
}

// IterValues iterates the values of the Map, passing them to the callback.
// It guarantees that any value in the Map will be visited only once.
// The set of values visited by IterValues is non-deterministic.
func (sm *shardedMap[V]) IterValues(cb func(v V) (stop bool)) {
	for _, shard := range sm.shards {
		stopped := func() bool {
			shard.RLock()
			defer shard.RUnlock()

			for _, item := range shard.data {
				if !item.expiration.IsZero() && time.Now().After(item.expiration) {
					continue
				}
				if stop := cb(item.value); stop {
					return true
				}
			}
			return false
		}()

		if stopped {
			break
		}
	}
}

func (sm *shardedMap[V]) Get(key, conflict uint64) (V, bool) {
	return sm.shards[key%numShards].get(key, conflict)
}

func (sm *shardedMap[V]) Expiration(key uint64) time.Time {
	return sm.shards[key%numShards].Expiration(key)
}

func (sm *shardedMap[V]) Set(i *Item[V]) (V, *refEntry[V], bool) {
	if i == nil {
		// If item is nil make this Set a no-op.
		return zeroValue[V](), nil, false
	}

	return sm.shards[i.Key%numShards].Set(i)
}

func (sm *shardedMap[V]) Del(key, conflict uint64) (uint64, V, *refEntry[V]) {
	return sm.shards[key%numShards].Del(key, conflict)
}

func (sm *shardedMap[V]) Update(newItem *Item[V]) (V, *refEntry[V], bool) {
	return sm.shards[newItem.Key%numShards].Update(newItem)
}

func (sm *shardedMap[V]) peek(key, conflict uint64) (storeItem[V], bool) {
	return sm.shards[key%numShards].peek(key, conflict)
}

func (sm *shardedMap[V]) borrow(key, conflict uint64) (storeItem[V], bool) {
	return sm.shards[key%numShards].borrow(key, conflict)
}

func (sm *shardedMap[V]) Cleanup(policy *defaultPolicy[V], onEvict func(item *Item[V])) {
	sm.expiryMap.cleanup(sm, policy, onEvict)
}

func (sm *shardedMap[V]) Clear(onEvict func(item *Item[V])) {
	for i := uint64(0); i < numShards; i++ {
		sm.shards[i].Clear(onEvict)
	}
	sm.expiryMap.clear()
}

type lockedMap[V any] struct {
	sync.RWMutex
	data         map[uint64]storeItem[V]
	em           *expirationMap[V]
	shouldUpdate updateFn[V]
}

func newLockedMap[V any](em *expirationMap[V]) *lockedMap[V] {
	return &lockedMap[V]{
		data: make(map[uint64]storeItem[V]),
		em:   em,
		shouldUpdate: func(cur, prev V) bool {
			return true
		},
	}
}

func (m *lockedMap[V]) setShouldUpdateFn(f updateFn[V]) {
	m.shouldUpdate = f
}

func (m *lockedMap[V]) get(key, conflict uint64) (V, bool) {
	m.RLock()
	item, ok := m.data[key]
	m.RUnlock()
	if !ok {
		return zeroValue[V](), false
	}
	if conflict != 0 && (conflict != item.conflict) {
		return zeroValue[V](), false
	}

	// Handle expired items.
	if !item.expiration.IsZero() && time.Now().After(item.expiration) {
		return zeroValue[V](), false
	}
	return item.value, true
}

func (m *lockedMap[V]) Expiration(key uint64) time.Time {
	m.RLock()
	defer m.RUnlock()
	return m.data[key].expiration
}

func (m *lockedMap[V]) Set(i *Item[V]) (V, *refEntry[V], bool) {
	if i == nil {
		// If the item is nil make this Set a no-op.
		return zeroValue[V](), nil, false
	}

	m.Lock()
	defer m.Unlock()
	item, ok := m.data[i.Key]

	if ok {
		// The item existed already. We need to check the conflict key and reject the
		// update if they do not match. Only after that the expiration map is updated.
		if i.Conflict != 0 && (i.Conflict != item.conflict) {
			return zeroValue[V](), nil, false
		}
		if m.shouldUpdate != nil && !m.shouldUpdate(i.Value, item.value) {
			return zeroValue[V](), nil, false
		}
		m.em.update(i.Key, i.Conflict, item.expiration, i.Expiration)
	} else {
		// The value is not in the map already. There's no need to return anything.
		// Simply add the expiration map.
		m.em.add(i.Key, i.Conflict, i.Expiration)
	}

	m.data[i.Key] = storeItem[V]{
		key:        i.Key,
		conflict:   i.Conflict,
		value:      i.Value,
		cost:       i.Cost,
		expiration: i.Expiration,
	}
	if ok {
		// Return the replaced value and its borrow entry so the caller can
		// account for the old value going out of scope.
		return item.value, item.entry, true
	}
	return zeroValue[V](), nil, false
}

func (m *lockedMap[V]) Del(key, conflict uint64) (uint64, V, *refEntry[V]) {
	m.Lock()
	defer m.Unlock()
	item, ok := m.data[key]
	if !ok {
		return 0, zeroValue[V](), nil
	}
	if conflict != 0 && (conflict != item.conflict) {
		return 0, zeroValue[V](), nil
	}

	if !item.expiration.IsZero() {
		m.em.del(key, item.expiration)
	}

	delete(m.data, key)
	return item.conflict, item.value, item.entry
}

func (m *lockedMap[V]) Update(newItem *Item[V]) (V, *refEntry[V], bool) {
	m.Lock()
	defer m.Unlock()
	item, ok := m.data[newItem.Key]
	if !ok {
		return zeroValue[V](), nil, false
	}
	if newItem.Conflict != 0 && (newItem.Conflict != item.conflict) {
		return zeroValue[V](), nil, false
	}
	if m.shouldUpdate != nil && !m.shouldUpdate(newItem.Value, item.value) {
		return item.value, nil, false
	}

	m.em.update(newItem.Key, newItem.Conflict, item.expiration, newItem.Expiration)
	m.data[newItem.Key] = storeItem[V]{
		key:        newItem.Key,
		conflict:   newItem.Conflict,
		value:      newItem.Value,
		cost:       newItem.Cost,
		expiration: newItem.Expiration,
	}

	return item.value, item.entry, true
}

func (m *lockedMap[V]) peek(key, conflict uint64) (storeItem[V], bool) {
	m.RLock()
	item, ok := m.data[key]
	m.RUnlock()
	if !ok {
		return storeItem[V]{}, false
	}
	if conflict != 0 && (conflict != item.conflict) {
		return storeItem[V]{}, false
	}

	// Handle expired items.
	if !item.expiration.IsZero() && time.Now().After(item.expiration) {
		return storeItem[V]{}, false
	}
	return item, true
}

func (m *lockedMap[V]) borrow(key, conflict uint64) (storeItem[V], bool) {
	m.Lock()
	defer m.Unlock()
	item, ok := m.data[key]
	if !ok {
		return storeItem[V]{}, false
	}
	if conflict != 0 && (conflict != item.conflict) {
		return storeItem[V]{}, false
	}

	// Handle expired items.
	if !item.expiration.IsZero() && time.Now().After(item.expiration) {
		return storeItem[V]{}, false
	}

	if item.entry == nil {
		// Lazily create the borrow entry. The cache itself holds one
		// reference which is dropped when the value leaves the store.
		entry := &refEntry[V]{cost: item.cost}
		entry.refs.Store(1)
		item.entry = entry
		m.data[key] = item
	}
	item.entry.refs.Add(1)
	return item, true
}

func (m *lockedMap[V]) Clear(onEvict func(item *Item[V])) {
	m.Lock()
	defer m.Unlock()
	if onEvict != nil {
		for _, si := range m.data {
			// Allocate a new Item per entry because the eviction may be
			// deferred (borrowed values) and the Item is retained until the
			// last borrower releases it.
			onEvict(&Item[V]{
				Key:      si.key,
				Conflict: si.conflict,
				Value:    si.value,
				Cost:     si.cost,
				entry:    si.entry,
			})
		}
	}
	m.data = make(map[uint64]storeItem[V])
}
