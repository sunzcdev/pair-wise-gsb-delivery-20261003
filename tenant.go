/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"errors"
	"math"
)

// tenantState tracks a single tenant's reserved cost share, its current cost
// usage and its own access-frequency sketch. Because each tenant has its own
// sketch, admission decisions compare items against the access stream of
// their own tenant: a large tenant's global hotness can never directly
// inflate the frequency counts used to judge a small tenant's items.
type tenantState struct {
	// reserved is the cost share guaranteed to the tenant. Other tenants may
	// borrow this share while it is idle, but must give it back on demand.
	reserved int64
	// used is the cost currently held by the tenant. used > reserved means
	// the tenant is borrowing idle capacity from other tenants.
	used int64
	// admit is the tenant's private access-frequency sketch.
	admit *tinyLFU
	// keyCosts mirrors the costs of the keys owned by this tenant.
	keyCosts map[uint64]int64
}

// tenantKey is a single Get access attributed to a tenant.
type tenantKey struct {
	tenant string
	key    uint64
}

// defaultTenant is the implicit tenant for items added without a tenant. It
// has no reserved share, so its items are the first to be reclaimed.
const defaultTenant = ""

// RegisterTenant reserves a cost share for the given tenant. The sum of all
// reserved shares may not exceed the cache's MaxCost. Reserved capacity can
// be borrowed by other tenants while idle and is reclaimed when the owner
// needs it back.
func (p *defaultPolicy[V]) RegisterTenant(tenant string, reservedCost int64) error {
	p.Lock()
	defer p.Unlock()

	if tenant == defaultTenant {
		return errors.New("tenant id can't be empty")
	}
	if reservedCost < 0 {
		return errors.New("reserved cost can't be negative")
	}
	if p.tenants == nil {
		p.tenants = make(map[string]*tenantState)
		p.keyTenant = make(map[uint64]string)
		// Attribute all pre-existing items to the default tenant.
		ts := p.defaultTenantState()
		for key, cost := range p.evict.keyCosts {
			ts.used += cost
			ts.keyCosts[key] = cost
			p.keyTenant[key] = defaultTenant
		}
	}
	if _, exists := p.tenants[tenant]; exists {
		return errors.New("tenant is already registered")
	}
	var reservedTotal int64
	for _, ts := range p.tenants {
		reservedTotal += ts.reserved
	}
	if reservedTotal+reservedCost > p.evict.getMaxCost() {
		return errors.New("reserved cost exceeds the cache's remaining capacity")
	}
	p.tenants[tenant] = &tenantState{
		reserved: reservedCost,
		admit:    newTinyLFU(p.numCounters),
		keyCosts: make(map[uint64]int64),
	}
	return nil
}

// UnregisterTenant removes the tenant, evicting all of its items and
// releasing its reserved share. The evicted items are returned with their
// actual costs so the caller can keep its own statistics in sync.
func (p *defaultPolicy[V]) UnregisterTenant(tenant string) []*Item[V] {
	p.Lock()
	defer p.Unlock()

	if p.tenants == nil {
		return nil
	}
	ts, ok := p.tenants[tenant]
	if !ok {
		return nil
	}
	victims := make([]*Item[V], 0, len(ts.keyCosts))
	for key, cost := range ts.keyCosts {
		p.evict.del(key)
		delete(p.keyTenant, key)
		victims = append(victims, &Item[V]{Key: key, Cost: cost})
	}
	delete(p.tenants, tenant)
	// When only the implicit default tenant remains, tear down tenant
	// bookkeeping entirely and fall back to the tenant-unaware fast path.
	if len(p.tenants) == 1 {
		p.tenants = nil
		p.keyTenant = nil
	}
	return victims
}

// HasTenant reports whether the tenant is currently registered.
func (p *defaultPolicy[V]) HasTenant(tenant string) bool {
	p.Lock()
	defer p.Unlock()
	if p.tenants == nil {
		return false
	}
	_, ok := p.tenants[tenant]
	return ok
}

// TenantCost returns the cost currently used by the tenant and whether the
// tenant is registered.
func (p *defaultPolicy[V]) TenantCost(tenant string) (int64, bool) {
	p.Lock()
	defer p.Unlock()
	if p.tenants == nil {
		return 0, false
	}
	ts, ok := p.tenants[tenant]
	if !ok {
		return 0, false
	}
	return ts.used, true
}

// PushTenantKey records a single Get access in the tenant's own frequency
// sketch. Accesses of unknown tenants are attributed to the global sketch.
func (p *defaultPolicy[V]) PushTenantKey(tenant string, key uint64) {
	if p.isClosed {
		return
	}
	select {
	case p.tenantCh <- tenantKey{tenant: tenant, key: key}:
		p.metrics.add(keepGets, key, 1)
	default:
		p.metrics.add(dropGets, key, 1)
	}
}

// defaultTenantState returns the state of the implicit default tenant. The
// default tenant shares the policy's global frequency sketch. The caller
// must hold the policy lock.
func (p *defaultPolicy[V]) defaultTenantState() *tenantState {
	ts, ok := p.tenants[defaultTenant]
	if !ok {
		ts = &tenantState{
			reserved: 0,
			admit:    p.admit,
			keyCosts: make(map[uint64]int64),
		}
		p.tenants[defaultTenant] = ts
	}
	return ts
}

// addTenant is the tenant-aware admission algorithm. The caller must hold
// the policy lock.
//
// Admission rules:
//   - Free capacity is usable by everyone, no questions asked.
//   - A tenant adding within its reserved share is reclaiming: borrowed-out
//     capacity is taken back from tenants that are over their own share,
//     coldest items first, without any frequency gate.
//   - A tenant adding beyond its reserved share is borrowing: it competes
//     with the other borrowers' items only, using each item's frequency in
//     its own tenant's sketch. Items of tenants within their reserved share
//     are never eligible victims.
func (p *defaultPolicy[V]) addTenant(tenant string, key uint64, cost int64) ([]*Item[V], bool) {
	// Cannot add an item bigger than entire cache.
	if cost > p.evict.getMaxCost() {
		return nil, false
	}

	ts, ok := p.tenants[tenant]
	if !ok {
		// Unknown tenants (e.g. recently unregistered) are demoted to the
		// default, unreserved tenant.
		tenant = defaultTenant
		ts = p.defaultTenantState()
	}

	// An update of an existing key is not an addition. Adjust the owning
	// tenant's usage by the cost delta.
	if prev, found := p.evict.keyCosts[key]; found {
		p.evict.updateIfHas(key, cost)
		owner := p.tenants[p.keyTenant[key]]
		owner.used += cost - prev
		owner.keyCosts[key] = cost
		return nil, false
	}

	// If there's enough room left, admit the item without evicting anything.
	// This is what keeps idle capacity fully usable.
	room := p.evict.roomLeft(cost)
	if room >= 0 {
		p.evict.add(key, cost)
		ts.used += cost
		ts.keyCosts[key] = cost
		p.keyTenant[key] = tenant
		p.metrics.add(costAdd, key, uint64(cost))
		return nil, true
	}

	// reclaiming means the tenant stays within its reserved share after this
	// addition, so admission is guaranteed: since the sum of all reserved
	// shares never exceeds MaxCost, some other tenant must be borrowing and
	// can be evicted.
	reclaiming := ts.used+cost <= ts.reserved
	// incHits is the hit count for the incoming item in its tenant's sketch.
	incHits := ts.admit.Estimate(key)
	sample := make([]*policyPair, 0, lfuSample)
	victims := make([]*Item[V], 0)

	for ; room < 0; room = p.evict.roomLeft(cost) {
		// Drop sampled pairs whose tenant is no longer borrowing, then
		// refill the sample with eligible (borrowing) tenants' items.
		kept := sample[:0]
		for _, pair := range sample {
			if vts := p.tenants[p.keyTenant[pair.key]]; vts != nil && vts.used > vts.reserved {
				kept = append(kept, pair)
			}
		}
		sample = p.fillTenantSample(kept)
		if len(sample) == 0 {
			// Everything left belongs to tenants within their reserved
			// share and is protected from eviction.
			p.metrics.add(rejectSets, key, 1)
			return victims, false
		}

		// Find the coldest item in the sample, comparing frequencies
		// within each victim's own tenant sketch.
		minKey, minHits, minId, minCost := uint64(0), int64(math.MaxInt64), 0, int64(0)
		for i, pair := range sample {
			vts := p.tenants[p.keyTenant[pair.key]]
			if hits := vts.admit.Estimate(pair.key); hits < minHits {
				minKey, minHits, minId, minCost = pair.key, hits, i, pair.cost
			}
		}

		// Reclaiming a reserved share always wins. Otherwise the incoming
		// item must be hotter than the coldest borrower.
		if !reclaiming && incHits < minHits {
			p.metrics.add(rejectSets, key, 1)
			return victims, false
		}

		p.delTenantKey(minKey)

		// Delete the victim from the sample.
		sample[minId] = sample[len(sample)-1]
		sample = sample[:len(sample)-1]
		// Store the victim with its actual cost.
		victims = append(victims, &Item[V]{
			Key:      minKey,
			Conflict: 0,
			Cost:     minCost,
		})
	}

	p.evict.add(key, cost)
	ts.used += cost
	ts.keyCosts[key] = cost
	p.keyTenant[key] = tenant
	p.metrics.add(costAdd, key, uint64(cost))
	return victims, true
}

// fillTenantSample fills the eviction sample with items owned by tenants
// that are currently borrowing (used > reserved). Items of tenants within
// their reserved share are protected and never sampled. The caller must
// hold the policy lock.
func (p *defaultPolicy[V]) fillTenantSample(in []*policyPair) []*policyPair {
	if len(in) >= lfuSample {
		return in
	}
	for _, ts := range p.tenants {
		if ts.used <= ts.reserved {
			continue
		}
		for key, cost := range ts.keyCosts {
			in = append(in, &policyPair{key, cost})
			if len(in) >= lfuSample {
				return in
			}
		}
	}
	return in
}

// delTenantKey removes a key from the policy and from its owning tenant's
// books. The caller must hold the policy lock.
func (p *defaultPolicy[V]) delTenantKey(key uint64) {
	if t, ok := p.keyTenant[key]; ok {
		if ts := p.tenants[t]; ts != nil {
			if cost, found := ts.keyCosts[key]; found {
				ts.used -= cost
				delete(ts.keyCosts, key)
			}
		}
		delete(p.keyTenant, key)
	}
	p.evict.del(key)
}
