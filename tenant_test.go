/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// evictedCosts collects the costs of evicted items via the OnEvict callback.
type evictedCosts struct {
	mu    sync.Mutex
	costs []int64
}

func (e *evictedCosts) add(item *Item[int]) {
	e.mu.Lock()
	e.costs = append(e.costs, item.Cost)
	e.mu.Unlock()
}

func (e *evictedCosts) sum() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total int64
	for _, cost := range e.costs {
		total += cost
	}
	return total
}

func (e *evictedCosts) reset() {
	e.mu.Lock()
	e.costs = nil
	e.mu.Unlock()
}

func newTenantCache(t *testing.T, maxCost int64, evicted *evictedCosts) *Cache[int, int] {
	t.Helper()
	c, err := NewCache(&Config[int, int]{
		NumCounters:        1000,
		MaxCost:            maxCost,
		BufferItems:        64,
		Metrics:            true,
		IgnoreInternalCost: true,
		OnEvict:            evicted.add,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func requireTenantCost(t *testing.T, c *Cache[int, int], tenant string, expected int64) {
	t.Helper()
	used, ok := c.TenantCost(tenant)
	require.True(t, ok, "tenant %q should be registered", tenant)
	require.Equal(t, expected, used, "tenant %q used cost", tenant)
}

// getForTenant accesses a key several times so its frequency is recorded in
// the tenant's own sketch, then waits for the accesses to be processed.
func getForTenant(c *Cache[int, int], tenant string, key, times int) {
	for i := 0; i < times; i++ {
		c.GetFor(tenant, key)
	}
	time.Sleep(5 * wait)
}

func TestTenantRegisterValidation(t *testing.T) {
	c := newTenantCache(t, 100, &evictedCosts{})

	require.Error(t, c.RegisterTenant("", 10))
	require.Error(t, c.RegisterTenant("a", -1))
	require.NoError(t, c.RegisterTenant("a", 60))
	require.Error(t, c.RegisterTenant("a", 10))
	// The sum of reserved shares can't exceed MaxCost.
	require.Error(t, c.RegisterTenant("b", 41))
	require.NoError(t, c.RegisterTenant("b", 40))

	// Sets for unregistered tenants are rejected.
	require.False(t, c.SetFor("unknown", 1, 1, 1))
}

// TestTenantReclaim verifies that a tenant can lend out its idle reserved
// share and reclaim it later, and that the eviction cost statistics match
// the actual evicted items.
func TestTenantReclaim(t *testing.T) {
	evicted := &evictedCosts{}
	c := newTenantCache(t, 100, evicted)
	require.NoError(t, c.RegisterTenant("big", 60))
	require.NoError(t, c.RegisterTenant("small", 40))

	// The big tenant uses the whole cache while the small tenant is idle,
	// borrowing the small tenant's entire reserved share.
	for i := 0; i < 10; i++ {
		require.True(t, c.SetFor("big", i, i, 10))
	}
	c.Wait()
	requireTenantCost(t, c, "big", 100)
	requireTenantCost(t, c, "small", 0)

	// The small tenant comes back and reclaims its reserved share.
	evicted.reset()
	for i := 100; i < 104; i++ {
		require.True(t, c.SetFor("small", i, i, 10))
	}
	c.Wait()
	requireTenantCost(t, c, "big", 60)
	requireTenantCost(t, c, "small", 40)

	// The reclaimed cost statistics must match the actual evicted items:
	// exactly 4 items of cost 10 each were handed back.
	require.Equal(t, int64(40), evicted.sum())
	require.Equal(t, uint64(40), c.Metrics.CostEvicted())
	require.Equal(t, uint64(4), c.Metrics.KeysEvicted())

	// The small tenant's items are all present; the big tenant kept exactly
	// its reserved share.
	for i := 100; i < 104; i++ {
		_, ok := c.GetFor("small", i)
		require.True(t, ok)
	}
	present := 0
	for i := 0; i < 10; i++ {
		if _, ok := c.GetFor("big", i); ok {
			present++
		}
	}
	require.Equal(t, 6, present)
}

// TestTenantReservedProtection verifies that a large tenant's hot data can't
// squeeze a small tenant out of its reserved share, no matter how hot the
// large tenant's items are.
func TestTenantReservedProtection(t *testing.T) {
	evicted := &evictedCosts{}
	c := newTenantCache(t, 100, evicted)
	require.NoError(t, c.RegisterTenant("big", 60))
	require.NoError(t, c.RegisterTenant("small", 40))

	// The small tenant fills its reserved share with cold (never accessed)
	// items.
	for i := 0; i < 4; i++ {
		require.True(t, c.SetFor("small", i, i, 10))
	}
	// The big tenant fills its share with very hot items.
	for i := 100; i < 106; i++ {
		require.True(t, c.SetFor("big", i, i, 10))
	}
	c.Wait()
	for i := 100; i < 106; i++ {
		getForTenant(c, "big", i, 20)
	}

	// The big tenant tries to grab more than its share. The cache is full
	// and the small tenant's items are protected, so this must be rejected.
	getForTenant(c, "big", 200, 20)
	require.True(t, c.SetFor("big", 200, 200, 10))
	c.Wait()

	requireTenantCost(t, c, "big", 60)
	requireTenantCost(t, c, "small", 40)
	require.Equal(t, uint64(1), c.Metrics.SetsRejected())
	_, ok := c.Get(200)
	require.False(t, ok)
	// None of the small tenant's cold items were evicted.
	require.Equal(t, int64(0), evicted.sum())
	for i := 0; i < 4; i++ {
		_, ok := c.GetFor("small", i)
		require.True(t, ok)
	}
}

// TestTenantHotColdReversal verifies that tenants competing for unreserved
// capacity are judged by their own access frequency: the hot tenant keeps
// the borrowed space, and when the hot/cold roles reverse, so does the
// admission outcome.
func TestTenantHotColdReversal(t *testing.T) {
	evicted := &evictedCosts{}
	c := newTenantCache(t, 100, evicted)
	require.NoError(t, c.RegisterTenant("a", 45))
	require.NoError(t, c.RegisterTenant("b", 45))

	// A borrows 5 of the 10 unreserved cost units, B stays within its share.
	for i := 0; i < 5; i++ {
		require.True(t, c.SetFor("a", i, i, 10))
	}
	for i := 100; i < 104; i++ {
		require.True(t, c.SetFor("b", i, i, 10))
	}
	require.True(t, c.SetFor("b", 104, 104, 5))
	c.Wait()
	requireTenantCost(t, c, "a", 50)
	requireTenantCost(t, c, "b", 45)

	// Phase 1: A is hot, B's candidate item is cold. B's attempt to borrow
	// is rejected because A's borrowed items are hotter.
	for i := 0; i < 5; i++ {
		getForTenant(c, "a", i, 5)
	}
	getForTenant(c, "b", 200, 1)
	require.True(t, c.SetFor("b", 200, 200, 10))
	c.Wait()
	requireTenantCost(t, c, "a", 50)
	requireTenantCost(t, c, "b", 45)
	_, ok := c.Get(200)
	require.False(t, ok, "cold item shouldn't displace the hot tenant's items")

	// Phase 2: the roles reverse. B's candidate becomes the hottest item
	// while A goes idle, so B now wins the competition for borrowed space.
	evicted.reset()
	getForTenant(c, "b", 200, 20)
	require.True(t, c.SetFor("b", 200, 200, 10))
	c.Wait()
	requireTenantCost(t, c, "a", 40)
	requireTenantCost(t, c, "b", 55)
	_, ok = c.GetFor("b", 200)
	require.True(t, ok)
	// Exactly one of A's borrowed items (cost 10) was evicted to make room.
	require.Equal(t, int64(10), evicted.sum())
}

// TestTenantCostUpdate verifies that item cost changes are reflected in the
// tenant's usage and that reclaiming borrowed space reports the updated
// costs of the actual evicted items.
func TestTenantCostUpdate(t *testing.T) {
	evicted := &evictedCosts{}
	c := newTenantCache(t, 100, evicted)
	require.NoError(t, c.RegisterTenant("a", 30))
	require.NoError(t, c.RegisterTenant("b", 70))

	require.True(t, c.SetFor("a", 1, 1, 20))
	c.Wait()
	requireTenantCost(t, c, "a", 20)

	// Growing the item pushes the tenant into borrowed space.
	require.True(t, c.SetFor("a", 1, 1, 40))
	c.Wait()
	requireTenantCost(t, c, "a", 40)

	// Shrinking it releases the borrowed space again.
	require.True(t, c.SetFor("a", 1, 1, 10))
	c.Wait()
	requireTenantCost(t, c, "a", 10)

	// Grow it once more so the tenant is borrowing 10 units.
	require.True(t, c.SetFor("a", 1, 1, 40))
	c.Wait()
	requireTenantCost(t, c, "a", 40)

	// B fills its reserved share and reclaims the borrowed capacity. The
	// evicted item must be reported with its current cost (40), not the
	// cost it was originally admitted with (20).
	evicted.reset()
	for i := 100; i < 107; i++ {
		require.True(t, c.SetFor("b", i, i, 10))
	}
	c.Wait()
	requireTenantCost(t, c, "a", 0)
	requireTenantCost(t, c, "b", 70)
	require.Equal(t, int64(40), evicted.sum())
	require.Equal(t, uint64(40), c.Metrics.CostEvicted())
	_, ok := c.GetFor("a", 1)
	require.False(t, ok)
}

// TestTenantUnregister verifies that deregistering a tenant evicts its items
// with matching cost statistics and releases its reserved share.
func TestTenantUnregister(t *testing.T) {
	evicted := &evictedCosts{}
	c := newTenantCache(t, 100, evicted)
	require.NoError(t, c.RegisterTenant("a", 50))
	require.NoError(t, c.RegisterTenant("b", 50))

	for i := 0; i < 5; i++ {
		require.True(t, c.SetFor("a", i, i, 10))
		require.True(t, c.SetFor("b", 100+i, 100+i, 10))
	}
	c.Wait()
	requireTenantCost(t, c, "a", 50)
	requireTenantCost(t, c, "b", 50)

	// Deregistering A evicts all of its items with their actual costs.
	evicted.reset()
	c.UnregisterTenant("a")
	require.Equal(t, int64(50), evicted.sum())
	_, ok := c.TenantCost("a")
	require.False(t, ok)
	for i := 0; i < 5; i++ {
		_, ok := c.Get(i)
		require.False(t, ok)
	}
	// Sets for the deregistered tenant are rejected.
	require.False(t, c.SetFor("a", 1, 1, 10))

	// A's reserved share is released, so B can now use the whole cache.
	for i := 105; i < 110; i++ {
		require.True(t, c.SetFor("b", i, i, 10))
	}
	c.Wait()
	requireTenantCost(t, c, "b", 100)

	// Deregistering the last tenant tears down tenant bookkeeping.
	evicted.reset()
	c.UnregisterTenant("b")
	_, ok = c.TenantCost("b")
	require.False(t, ok)
	require.Equal(t, int64(100), evicted.sum())

	// Deregistering an unknown tenant is a no-op.
	c.UnregisterTenant("unknown")
}
