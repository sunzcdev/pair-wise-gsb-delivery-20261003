/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"errors"
	"fmt"
)

// tenantNumCounters is the number of frequency counters kept per registered
// tenant. Each tenant gets its own tinyLFU sketch so that admission decisions
// for a tenant's items are based on that tenant's own access frequency rather
// than the global (possibly dominant-tenant) frequencies.
const tenantNumCounters = 4096

// noTenant is the implicit tenant id for items added without a tenant. The
// zero tenant has no reservation, uses the global frequency sketch, and its
// items are always reclaimable by tenants that hold a reservation.
const noTenant = 0

// tenantInfo tracks the quota state of a single tenant inside the policy.
type tenantInfo struct {
	// reserved is the cost share reserved for this tenant. Other tenants may
	// borrow the unused portion, but must give it back (be evicted) when the
	// owner needs it.
	reserved int64
	// used is the sum of costs of all live entries owned by this tenant.
	used int64
	// keys maps key hash -> cost for every entry owned by this tenant. It
	// mirrors the entries in sampledLFU.keyCosts that belong to the tenant,
	// so reclaiming and accounting always agree with the actual entries.
	keys map[uint64]int64
	// freq is the tenant's own access-frequency sketch. For the zero tenant
	// this aliases the policy's global tinyLFU.
	freq *tinyLFU
}

// TenantStats is a snapshot of a tenant's quota usage.
type TenantStats struct {
	// Reserved is the cost share reserved for the tenant.
	Reserved int64
	// Used is the total cost of the tenant's entries currently in the cache.
	Used int64
	// Lent is the portion of the reservation currently unused by the owner
	// and therefore available for other tenants to borrow.
	Lent int64
	// Borrowed is the portion of Used that exceeds the reservation, i.e. the
	// cost the tenant is currently borrowing from others' idle shares.
	Borrowed int64
}

// tenantPush carries a single access record for a tenant into the policy
// goroutine so the tenant's own frequency sketch can be incremented.
type tenantPush struct {
	tenant uint64
	key    uint64
}

// canonicalTenant maps an incoming tenant id to the id actually used for
// accounting: unregistered tenants are treated as untenanted.
func (p *defaultPolicy[V]) canonicalTenant(tenant uint64) uint64 {
	if tenant == noTenant {
		return noTenant
	}
	if _, ok := p.tenants[tenant]; ok {
		return tenant
	}
	return noTenant
}

// registerTenant reserves a cost share for the given tenant. The sum of all
// reservations may not exceed MaxCost. Registering an already registered
// tenant updates its reservation.
func (p *defaultPolicy[V]) registerTenant(tenant uint64, reservedCost int64) error {
	if tenant == noTenant {
		return errors.New("tenant id 0 is reserved for untenanted items")
	}
	if reservedCost < 0 {
		return errors.New("reserved cost can't be negative")
	}
	p.Lock()
	defer p.Unlock()
	maxCost := p.evict.getMaxCost()
	var current int64
	if t, ok := p.tenants[tenant]; ok {
		current = t.reserved
	}
	if p.totalReserved-current+reservedCost > maxCost {
		return fmt.Errorf("total reserved cost would exceed MaxCost (%d)", maxCost)
	}
	p.totalReserved += reservedCost - current
	if t, ok := p.tenants[tenant]; ok {
		t.reserved = reservedCost
		return nil
	}
	p.tenants[tenant] = &tenantInfo{
		reserved: reservedCost,
		keys:     make(map[uint64]int64),
		freq:     newTinyLFU(tenantNumCounters),
	}
	return nil
}

// unregisterTenant removes the tenant's reservation. The tenant's entries are
// kept in the cache but become untenanted: they lose their reserved-share
// protection and can be reclaimed or evicted like any other untenanted item.
func (p *defaultPolicy[V]) unregisterTenant(tenant uint64) {
	if tenant == noTenant {
		return
	}
	p.Lock()
	defer p.Unlock()
	t, ok := p.tenants[tenant]
	if !ok {
		return
	}
	p.totalReserved -= t.reserved
	zero := p.tenants[noTenant]
	for key, cost := range t.keys {
		zero.keys[key] = cost
		zero.used += cost
		p.keyTenant[key] = noTenant
	}
	delete(p.tenants, tenant)
}

// tenantStats returns a snapshot of the tenant's quota usage.
func (p *defaultPolicy[V]) tenantStats(tenant uint64) (TenantStats, bool) {
	p.Lock()
	defer p.Unlock()
	t, ok := p.tenants[tenant]
	if !ok || tenant == noTenant {
		return TenantStats{}, false
	}
	stats := TenantStats{Reserved: t.reserved, Used: t.used}
	if t.used < t.reserved {
		stats.Lent = t.reserved - t.used
	}
	if t.used > t.reserved {
		stats.Borrowed = t.used - t.reserved
	}
	return stats, true
}

// tenantAdd records ownership of a newly admitted key.
func (p *defaultPolicy[V]) tenantAdd(tenant uint64, key uint64, cost int64) {
	t := p.tenants[tenant]
	t.keys[key] = cost
	t.used += cost
	p.keyTenant[key] = tenant
}

// tenantDel removes a key from its owner's accounting. It is called whenever
// a key leaves the policy (eviction, deletion, clear of a single key).
func (p *defaultPolicy[V]) tenantDel(key uint64) {
	tenant, ok := p.keyTenant[key]
	if !ok {
		return
	}
	if t, ok := p.tenants[tenant]; ok {
		if cost, found := t.keys[key]; found {
			t.used -= cost
			delete(t.keys, key)
		}
	}
	delete(p.keyTenant, key)
}

// updateExisting applies a cost change (and optional ownership transfer) for
// a key that is already in the policy. A non-zero tenant id takes ownership of
// the key; a zero tenant id (plain Set) leaves the ownership untouched.
func (p *defaultPolicy[V]) updateExisting(tenant uint64, key uint64, cost int64) {
	prev := p.evict.keyCosts[key]
	p.evict.updateIfHas(key, cost)
	owner := p.keyTenant[key]
	if tenant != noTenant {
		if next := p.canonicalTenant(tenant); next != owner {
			if t, ok := p.tenants[owner]; ok {
				t.used -= prev
				delete(t.keys, key)
			}
			owner = next
			p.keyTenant[key] = next
			p.tenants[next].keys[key] = cost
			p.tenants[next].used += cost
			return
		}
	}
	if t, ok := p.tenants[owner]; ok {
		t.used += cost - prev
		t.keys[key] = cost
	}
}

// estimateFor returns the access-frequency estimate for a key from the point
// of view of the given tenant: each tenant is judged by its own sketch.
func (p *defaultPolicy[V]) estimateFor(tenant uint64, key uint64) int64 {
	if t, ok := p.tenants[tenant]; ok && t.freq != nil {
		return t.freq.Estimate(key)
	}
	return p.admit.Estimate(key)
}

// isReclaimable reports whether the tenant's entries may be taken back by
// others: untenanted items and items of tenants that are over their reserved
// share are reclaimable. Entries of tenants within their share are protected.
func (p *defaultPolicy[V]) isReclaimable(tenant uint64) bool {
	t, ok := p.tenants[tenant]
	if !ok {
		return true
	}
	return t.used > t.reserved
}

// fillReclaimableSample fills the eviction sample with entries that may be
// evicted to make room: borrowed and untenanted entries only. Entries inside
// an owner's reserved share are never sampled.
func (p *defaultPolicy[V]) fillReclaimableSample(in []*policyPair) []*policyPair {
	if len(in) >= lfuSample {
		return in
	}
	for id, t := range p.tenants {
		if !p.isReclaimable(id) {
			continue
		}
		for key, cost := range t.keys {
			in = append(in, &policyPair{key, cost})
			if len(in) >= lfuSample {
				return in
			}
		}
	}
	return in
}
