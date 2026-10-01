/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/dgraph-io/ristretto/v2/z"
)

const (
	// lfuSample is the number of items to sample when looking at eviction
	// candidates. 5 seems to be the most optimal number [citation needed].
	lfuSample = 5
)

func newPolicy[V any](numCounters, maxCost int64) *defaultPolicy[V] {
	return newDefaultPolicy[V](numCounters, maxCost)
}

type defaultPolicy[V any] struct {
	sync.Mutex
	admit    *tinyLFU
	evict    *sampledLFU
	itemsCh  chan []uint64
	stop     chan struct{}
	done     chan struct{}
	isClosed bool
	metrics  *Metrics
	// tenants holds the quota state of every registered tenant. Index 0 is
	// the implicit untenanted bucket and always exists.
	tenants map[uint64]*tenantInfo
	// keyTenant maps every key in the policy to its owning tenant.
	keyTenant map[uint64]uint64
	// totalReserved is the sum of all tenant reservations (<= maxCost).
	totalReserved int64
	// tenantItemsCh carries per-tenant access records.
	tenantItemsCh chan tenantPush
}

func newDefaultPolicy[V any](numCounters, maxCost int64) *defaultPolicy[V] {
	p := &defaultPolicy[V]{
		admit:         newTinyLFU(numCounters),
		evict:         newSampledLFU(maxCost),
		itemsCh:       make(chan []uint64, 3),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		tenants:       make(map[uint64]*tenantInfo),
		keyTenant:     make(map[uint64]uint64),
		tenantItemsCh: make(chan tenantPush, 64),
	}
	p.tenants[noTenant] = &tenantInfo{
		keys: make(map[uint64]int64),
		freq: p.admit,
	}
	go p.processItems()
	return p
}

func (p *defaultPolicy[V]) CollectMetrics(metrics *Metrics) {
	p.metrics = metrics
	p.evict.metrics = metrics
}

type policyPair struct {
	key  uint64
	cost int64
}

func (p *defaultPolicy[V]) processItems() {
	for {
		select {
		case items := <-p.itemsCh:
			p.Lock()
			p.admit.Push(items)
			// Attribute accesses to the owning tenant's sketch as well, so
			// tenant-level hotness reflects actual usage.
			for _, key := range items {
				if tenant, ok := p.keyTenant[key]; ok && tenant != noTenant {
					if t, found := p.tenants[tenant]; found {
						t.freq.Increment(key)
					}
				}
			}
			p.Unlock()
		case tp := <-p.tenantItemsCh:
			p.Lock()
			if t, ok := p.tenants[tp.tenant]; ok && tp.tenant != noTenant {
				t.freq.Increment(tp.key)
			} else {
				p.admit.Increment(tp.key)
			}
			p.Unlock()
		case <-p.stop:
			p.done <- struct{}{}
			return
		}
	}
}

func (p *defaultPolicy[V]) Push(keys []uint64) bool {
	if p.isClosed {
		return false
	}

	if len(keys) == 0 {
		return true
	}

	select {
	case p.itemsCh <- keys:
		p.metrics.add(keepGets, keys[0], uint64(len(keys)))
		return true
	default:
		p.metrics.add(dropGets, keys[0], uint64(len(keys)))
		return false
	}
}

// PushTenant records an access to a key on behalf of a tenant, feeding the
// tenant's own frequency sketch. It is lossy under contention, like Push.
func (p *defaultPolicy[V]) PushTenant(tenant uint64, key uint64) bool {
	if p.isClosed {
		return false
	}
	select {
	case p.tenantItemsCh <- tenantPush{tenant: tenant, key: key}:
		return true
	default:
		return false
	}
}

// Add decides whether the item with the given key and cost should be accepted by
// the policy. It returns the list of victims that have been evicted and a boolean
// indicating whether the incoming item should be accepted.
func (p *defaultPolicy[V]) Add(key uint64, cost int64) ([]*Item[V], bool) {
	return p.add(noTenant, key, cost)
}

// add is the tenant-aware version of Add. Items of a tenant that is within
// its reserved share may reclaim borrowed space unconditionally; items beyond
// the share compete for borrowed space using the tenant's own access
// frequency and can never evict entries protected by another tenant's
// reservation.
func (p *defaultPolicy[V]) add(tenant uint64, key uint64, cost int64) ([]*Item[V], bool) {
	p.Lock()
	defer p.Unlock()

	// Cannot add an item bigger than entire cache.
	if cost > p.evict.getMaxCost() {
		return nil, false
	}

	// No need to go any further if the item is already in the cache.
	if _, has := p.evict.keyCosts[key]; has {
		p.updateExisting(tenant, key, cost)
		// An update does not count as an addition, so return false.
		return nil, false
	}

	tenant = p.canonicalTenant(tenant)
	t := p.tenants[tenant]
	// withinShare means the tenant is entitled to the space this item needs:
	// borrowed space may be reclaimed to satisfy the reservation.
	withinShare := tenant != noTenant && t.used+cost <= t.reserved

	// If the execution reaches this point, the key doesn't exist in the cache.
	// Calculate the remaining room in the cache (usually bytes).
	room := p.evict.roomLeft(cost)
	if room >= 0 {
		// There's enough room in the cache to store the new item without
		// overflowing. Do that now and stop here.
		p.evict.add(key, cost)
		p.tenantAdd(tenant, key, cost)
		p.metrics.add(costAdd, key, uint64(cost))
		return nil, true
	}

	// incHits is the hit count for the incoming item.
	incHits := p.estimateFor(tenant, key)
	// sample is the eviction candidate pool to be filled via random sampling.
	// TODO: perhaps we should use a min heap here. Right now our time
	// complexity is N for finding the min. Min heap should bring it down to
	// O(lg N).
	sample := make([]*policyPair, 0, lfuSample)
	// As items are evicted they will be appended to victims.
	victims := make([]*Item[V], 0)

	// Delete victims until there's enough space or a minKey is found that has
	// more hits than incoming item.
	for ; room < 0; room = p.evict.roomLeft(cost) {
		// Fill up empty slots in sample with reclaimable (borrowed or
		// untenanted) entries only. Entries inside an owner's reserved share
		// are never eviction candidates for other tenants.
		sample = p.fillReclaimableSample(sample)
		if len(sample) == 0 {
			// The cache is full of entries protected by reservations. A
			// borrower has nothing to compete for. (A tenant within its own
			// share cannot reach this state because reservations sum to at
			// most MaxCost, so there is always enough reclaimable space.)
			p.metrics.add(rejectSets, key, 1)
			return victims, false
		}

		// Find minimally used item in sample.
		minKey, minHits, minId, minCost := uint64(0), int64(math.MaxInt64), 0, int64(0)
		for i, pair := range sample {
			// Look up hit count for sample key in its owner's sketch.
			if hits := p.estimateFor(p.keyTenant[pair.key], pair.key); hits < minHits {
				minKey, minHits, minId, minCost = pair.key, hits, i, pair.cost
			}
		}

		// If the incoming item isn't worth keeping in the policy, reject.
		// A tenant reclaiming its own reserved share skips the frequency
		// comparison: reservation ownership beats borrower hotness.
		if !withinShare && incHits < minHits {
			p.metrics.add(rejectSets, key, 1)
			return victims, false
		}

		// Delete the victim from metadata.
		victimTenant := p.keyTenant[minKey]
		p.evict.del(minKey)
		p.tenantDel(minKey)

		// Delete the victim from sample.
		sample[minId] = sample[len(sample)-1]
		sample = sample[:len(sample)-1]
		// Store victim in evicted victims slice.
		victims = append(victims, &Item[V]{
			Key:      minKey,
			Conflict: 0,
			Cost:     minCost,
			tenant:   victimTenant,
		})
	}

	p.evict.add(key, cost)
	p.tenantAdd(tenant, key, cost)
	p.metrics.add(costAdd, key, uint64(cost))
	return victims, true
}

func (p *defaultPolicy[V]) Has(key uint64) bool {
	p.Lock()
	_, exists := p.evict.keyCosts[key]
	p.Unlock()
	return exists
}

func (p *defaultPolicy[V]) Del(key uint64) {
	p.Lock()
	p.evict.del(key)
	p.tenantDel(key)
	p.Unlock()
}

func (p *defaultPolicy[V]) Cap() int64 {
	p.Lock()
	capacity := p.evict.getMaxCost() - p.evict.used
	p.Unlock()
	return capacity
}

func (p *defaultPolicy[V]) Update(key uint64, cost int64) {
	p.update(noTenant, key, cost)
}

// update is the tenant-aware version of Update.
func (p *defaultPolicy[V]) update(tenant uint64, key uint64, cost int64) {
	p.Lock()
	if _, ok := p.evict.keyCosts[key]; ok {
		p.updateExisting(tenant, key, cost)
	}
	p.Unlock()
}

func (p *defaultPolicy[V]) Cost(key uint64) int64 {
	p.Lock()
	if cost, found := p.evict.keyCosts[key]; found {
		p.Unlock()
		return cost
	}
	p.Unlock()
	return -1
}

func (p *defaultPolicy[V]) Clear() {
	p.Lock()
	p.admit.clear()
	p.evict.clear()
	for id, t := range p.tenants {
		t.used = 0
		t.keys = make(map[uint64]int64)
		if id != noTenant {
			t.freq.clear()
		}
	}
	p.keyTenant = make(map[uint64]uint64)
	p.Unlock()
}

func (p *defaultPolicy[V]) Close() {
	if p.isClosed {
		return
	}

	// Block until the p.processItems goroutine returns.
	p.stop <- struct{}{}
	<-p.done
	close(p.stop)
	close(p.done)
	close(p.itemsCh)
	close(p.tenantItemsCh)
	p.isClosed = true
}

func (p *defaultPolicy[V]) MaxCost() int64 {
	if p == nil || p.evict == nil {
		return 0
	}
	return p.evict.getMaxCost()
}

func (p *defaultPolicy[V]) UpdateMaxCost(maxCost int64) {
	if p == nil || p.evict == nil {
		return
	}
	p.evict.updateMaxCost(maxCost)
}

// sampledLFU is an eviction helper storing key-cost pairs.
type sampledLFU struct {
	// NOTE: align maxCost to 64-bit boundary for use with atomic.
	// As per https://golang.org/pkg/sync/atomic/: "On ARM, x86-32,
	// and 32-bit MIPS, it is the caller’s responsibility to arrange
	// for 64-bit alignment of 64-bit words accessed atomically.
	// The first word in a variable or in an allocated struct, array,
	// or slice can be relied upon to be 64-bit aligned."
	maxCost  int64
	used     int64
	metrics  *Metrics
	keyCosts map[uint64]int64
}

func newSampledLFU(maxCost int64) *sampledLFU {
	return &sampledLFU{
		keyCosts: make(map[uint64]int64),
		maxCost:  maxCost,
	}
}

func (p *sampledLFU) getMaxCost() int64 {
	return atomic.LoadInt64(&p.maxCost)
}

func (p *sampledLFU) updateMaxCost(maxCost int64) {
	atomic.StoreInt64(&p.maxCost, maxCost)
}

func (p *sampledLFU) roomLeft(cost int64) int64 {
	return p.getMaxCost() - (p.used + cost)
}

func (p *sampledLFU) fillSample(in []*policyPair) []*policyPair {
	if len(in) >= lfuSample {
		return in
	}
	for key, cost := range p.keyCosts {
		in = append(in, &policyPair{key, cost})
		if len(in) >= lfuSample {
			return in
		}
	}
	return in
}

func (p *sampledLFU) del(key uint64) {
	cost, ok := p.keyCosts[key]
	if !ok {
		return
	}
	p.used -= cost
	delete(p.keyCosts, key)
	p.metrics.add(costEvict, key, uint64(cost))
	p.metrics.add(keyEvict, key, 1)
}

func (p *sampledLFU) add(key uint64, cost int64) {
	p.keyCosts[key] = cost
	p.used += cost
}

func (p *sampledLFU) updateIfHas(key uint64, cost int64) bool {
	if prev, found := p.keyCosts[key]; found {
		// Update the cost of an existing key, but don't worry about evicting.
		// Evictions will be handled the next time a new item is added.
		p.metrics.add(keyUpdate, key, 1)
		if prev > cost {
			diff := prev - cost
			p.metrics.add(costAdd, key, ^(uint64(diff) - 1))
		} else if cost > prev {
			diff := cost - prev
			p.metrics.add(costAdd, key, uint64(diff))
		}
		p.used += cost - prev
		p.keyCosts[key] = cost
		return true
	}
	return false
}

func (p *sampledLFU) clear() {
	p.used = 0
	p.keyCosts = make(map[uint64]int64)
}

// tinyLFU is an admission helper that keeps track of access frequency using
// tiny (4-bit) counters in the form of a count-min sketch.
// tinyLFU is NOT thread safe.
type tinyLFU struct {
	freq    *cmSketch
	door    *z.Bloom
	incrs   int64
	resetAt int64
}

func newTinyLFU(numCounters int64) *tinyLFU {
	return &tinyLFU{
		freq:    newCmSketch(numCounters),
		door:    z.NewBloomFilter(float64(numCounters), 0.01),
		resetAt: numCounters,
	}
}

func (p *tinyLFU) Push(keys []uint64) {
	for _, key := range keys {
		p.Increment(key)
	}
}

func (p *tinyLFU) Estimate(key uint64) int64 {
	hits := p.freq.Estimate(key)
	if p.door.Has(key) {
		hits++
	}
	return hits
}

func (p *tinyLFU) Increment(key uint64) {
	// Flip doorkeeper bit if not already done.
	if added := p.door.AddIfNotHas(key); !added {
		// Increment count-min counter if doorkeeper bit is already set.
		p.freq.Increment(key)
	}
	p.incrs++
	if p.incrs >= p.resetAt {
		p.reset()
	}
}

func (p *tinyLFU) reset() {
	// Zero out incrs.
	p.incrs = 0
	// clears doorkeeper bits
	p.door.Clear()
	// halves count-min counters
	p.freq.Reset()
}

func (p *tinyLFU) clear() {
	p.incrs = 0
	p.door.Clear()
	p.freq.Clear()
}
