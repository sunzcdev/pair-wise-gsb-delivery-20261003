/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTenantRegisterValidation(t *testing.T) {
	p := newDefaultPolicy[int](1000, 100)
	defer p.Close()

	require.Error(t, p.registerTenant(0, 10))
	require.Error(t, p.registerTenant(1, -1))
	require.NoError(t, p.registerTenant(1, 60))
	require.NoError(t, p.registerTenant(2, 40))
	// The sum of reservations may not exceed MaxCost.
	require.Error(t, p.registerTenant(3, 1))
	// Re-registering updates the reservation in place.
	require.NoError(t, p.registerTenant(1, 50))
	require.NoError(t, p.registerTenant(3, 10))
	require.Error(t, p.registerTenant(3, 11))

	stats, ok := p.tenantStats(1)
	require.True(t, ok)
	require.Equal(t, int64(50), stats.Reserved)
}

func TestTenantReservedShareProtected(t *testing.T) {
	p := newDefaultPolicy[int](10000, 100)
	defer p.Close()

	require.NoError(t, p.registerTenant(1, 40)) // small tenant
	require.NoError(t, p.registerTenant(2, 0))  // big tenant, no reservation

	// The small tenant fills its reserved share.
	for i := uint64(1); i <= 4; i++ {
		victims, added := p.add(1, i, 10)
		require.True(t, added)
		require.Empty(t, victims)
	}
	// The big tenant fills all the idle capacity (its own 60).
	for i := uint64(11); i <= 16; i++ {
		_, added := p.add(2, i, 10)
		require.True(t, added)
	}
	// The big tenant's data is globally hot.
	for i := uint64(11); i <= 16; i++ {
		for r := 0; r < 10; r++ {
			p.tenants[2].freq.Increment(i)
		}
	}

	// A cold new item from the big tenant is rejected: it may only compete
	// against the big tenant's own (hotter) borrowed entries.
	_, added := p.add(2, 20, 10)
	require.False(t, added)

	// A hot new item from the big tenant is admitted, but it can only evict
	// the big tenant's own entries, never the small tenant's reserved ones.
	for r := 0; r < 20; r++ {
		p.tenants[2].freq.Increment(20)
	}
	victims, added := p.add(2, 20, 10)
	require.True(t, added)
	require.NotEmpty(t, victims)
	for _, victim := range victims {
		require.GreaterOrEqual(t, victim.Key, uint64(11))
		require.Equal(t, uint64(2), victim.tenant)
	}

	// The small tenant's reserved entries survived untouched.
	for i := uint64(1); i <= 4; i++ {
		require.True(t, p.Has(i))
	}
	stats, ok := p.tenantStats(1)
	require.True(t, ok)
	require.Equal(t, int64(40), stats.Used)
	require.Equal(t, int64(0), stats.Lent)
}

func TestTenantReclaim(t *testing.T) {
	p := newDefaultPolicy[int](10000, 100)
	defer p.Close()

	require.NoError(t, p.registerTenant(1, 40)) // owner, currently idle
	require.NoError(t, p.registerTenant(2, 30)) // borrower

	// The borrower uses the whole cache: its own 30, the 30 unreserved, and
	// the 40 lent by the idle owner.
	for i := uint64(11); i <= 20; i++ {
		_, added := p.add(2, i, 10)
		require.True(t, added)
	}
	// The borrowed data is extremely hot.
	for i := uint64(11); i <= 20; i++ {
		for r := 0; r < 20; r++ {
			p.tenants[2].freq.Increment(i)
		}
	}

	stats, ok := p.tenantStats(2)
	require.True(t, ok)
	require.Equal(t, TenantStats{Reserved: 30, Used: 100, Borrowed: 70}, stats)
	stats, ok = p.tenantStats(1)
	require.True(t, ok)
	require.Equal(t, TenantStats{Reserved: 40, Used: 0, Lent: 40}, stats)

	// The owner comes back and reclaims its share. Reclaiming wins over the
	// borrower's hotness, and every returned victim carries the cost of the
	// actual evicted entry.
	var reclaimed int64
	for i := uint64(1); i <= 4; i++ {
		victims, added := p.add(1, i, 10)
		require.True(t, added)
		require.Len(t, victims, 1)
		for _, victim := range victims {
			require.GreaterOrEqual(t, victim.Key, uint64(11))
			require.Equal(t, uint64(2), victim.tenant)
			require.Equal(t, int64(10), victim.Cost)
			reclaimed += victim.Cost
		}
	}
	require.Equal(t, int64(40), reclaimed)

	stats, _ = p.tenantStats(1)
	require.Equal(t, TenantStats{Reserved: 40, Used: 40}, stats)
	stats, _ = p.tenantStats(2)
	require.Equal(t, TenantStats{Reserved: 30, Used: 60, Borrowed: 30}, stats)
	require.Equal(t, int64(100), p.evict.used)
}

func TestTenantHotColdReversal(t *testing.T) {
	p := newDefaultPolicy[int](10000, 10)
	defer p.Close()

	require.NoError(t, p.registerTenant(1, 0))
	require.NoError(t, p.registerTenant(2, 0))

	// Tenant 1 fills the cache and stays hot.
	for i := uint64(1); i <= 10; i++ {
		_, added := p.add(1, i, 1)
		require.True(t, added)
	}
	for i := uint64(1); i <= 10; i++ {
		for r := 0; r < 10; r++ {
			p.tenants[1].freq.Increment(i)
		}
	}

	// A cold item from tenant 2 is rejected: tenant 2's own access
	// frequency is zero, and tenant 1's hotness belongs to tenant 1.
	_, added := p.add(2, 100, 1)
	require.False(t, added)

	// Reversal: tenant 2 becomes the hot one.
	for r := 0; r < 20; r++ {
		p.tenants[2].freq.Increment(100)
	}
	victims, added := p.add(2, 100, 1)
	require.True(t, added)
	require.Len(t, victims, 1)
	require.LessOrEqual(t, victims[0].Key, uint64(10))
	require.Equal(t, uint64(1), victims[0].tenant)

	stats, _ := p.tenantStats(1)
	require.Equal(t, int64(9), stats.Used)
	stats, _ = p.tenantStats(2)
	require.Equal(t, int64(1), stats.Used)
}

func TestTenantCostUpdate(t *testing.T) {
	p := newDefaultPolicy[int](10000, 100)
	defer p.Close()

	require.NoError(t, p.registerTenant(1, 50))
	_, added := p.add(1, 1, 10)
	require.True(t, added)

	stats, _ := p.tenantStats(1)
	require.Equal(t, TenantStats{Reserved: 50, Used: 10, Lent: 40}, stats)

	// Re-adding an existing key with a new cost updates the accounting.
	_, added = p.add(1, 1, 25)
	require.False(t, added)
	stats, _ = p.tenantStats(1)
	require.Equal(t, int64(25), stats.Used)

	// Cost changes may push the tenant into borrowed territory and back.
	p.update(1, 1, 60)
	stats, _ = p.tenantStats(1)
	require.Equal(t, TenantStats{Reserved: 50, Used: 60, Borrowed: 10}, stats)
	p.update(1, 1, 20)
	stats, _ = p.tenantStats(1)
	require.Equal(t, TenantStats{Reserved: 50, Used: 20, Lent: 30}, stats)
	require.Equal(t, int64(20), p.evict.used)

	// A tenant-scoped update transfers ownership of the key.
	require.NoError(t, p.registerTenant(2, 10))
	_, added = p.add(2, 1, 20)
	require.False(t, added)
	stats, _ = p.tenantStats(1)
	require.Equal(t, int64(0), stats.Used)
	stats, _ = p.tenantStats(2)
	require.Equal(t, TenantStats{Reserved: 10, Used: 20, Borrowed: 10}, stats)

	// An untenanted update keeps the current owner.
	p.update(0, 1, 30)
	stats, _ = p.tenantStats(2)
	require.Equal(t, int64(30), stats.Used)
	require.Equal(t, int64(0), p.tenants[noTenant].used)
	require.Equal(t, int64(30), p.evict.used)
}

func TestTenantUnregister(t *testing.T) {
	p := newDefaultPolicy[int](10000, 100)
	defer p.Close()

	require.NoError(t, p.registerTenant(1, 100))
	for i := uint64(1); i <= 10; i++ {
		_, added := p.add(1, i, 10)
		require.True(t, added)
	}

	p.unregisterTenant(1)
	_, ok := p.tenantStats(1)
	require.False(t, ok)
	// The reservation was released.
	require.Equal(t, int64(0), p.totalReserved)
	// Entries stay in the cache but are now untenanted.
	for i := uint64(1); i <= 10; i++ {
		require.True(t, p.Has(i))
		require.Equal(t, uint64(noTenant), p.keyTenant[i])
	}
	require.Equal(t, int64(100), p.tenants[noTenant].used)

	// A new tenant can reserve the freed capacity and reclaim the space
	// held by the previous tenant's entries.
	require.NoError(t, p.registerTenant(2, 100))
	victims, added := p.add(2, 100, 10)
	require.True(t, added)
	require.Len(t, victims, 1)
	require.Equal(t, int64(10), victims[0].Cost)
	require.Equal(t, uint64(noTenant), victims[0].tenant)
	stats, _ := p.tenantStats(2)
	require.Equal(t, int64(10), stats.Used)
}

func TestCacheTenantQuota(t *testing.T) {
	var mu sync.Mutex
	evictedCost := make(map[uint64]int64)
	c, err := NewCache(&Config[string, string]{
		NumCounters:        10000,
		MaxCost:            100,
		BufferItems:        64,
		Metrics:            true,
		IgnoreInternalCost: true,
		OnEvict: func(item *Item[string]) {
			mu.Lock()
			evictedCost[item.tenant] += item.Cost
			mu.Unlock()
		},
	})
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.RegisterTenant(1, 40))
	require.NoError(t, c.RegisterTenant(2, 30))
	// 40 + 30 + 40 would exceed MaxCost.
	require.Error(t, c.RegisterTenant(3, 40))

	// While tenant 1 is idle, tenant 2 borrows the whole cache.
	for i := 0; i < 10; i++ {
		require.True(t, c.SetForTenant(2, fmt.Sprintf("b%d", i), "v", 10))
	}
	c.Wait()
	stats, ok := c.TenantStats(2)
	require.True(t, ok)
	require.Equal(t, TenantStats{Reserved: 30, Used: 100, Borrowed: 70}, stats)

	// Tenant 1 reclaims its reserved share.
	for i := 0; i < 4; i++ {
		require.True(t, c.SetForTenant(1, fmt.Sprintf("a%d", i), "v", 10))
	}
	c.Wait()

	stats, _ = c.TenantStats(1)
	require.Equal(t, TenantStats{Reserved: 40, Used: 40}, stats)
	stats, _ = c.TenantStats(2)
	require.Equal(t, TenantStats{Reserved: 30, Used: 60, Borrowed: 30}, stats)

	// The evicted cost reported through OnEvict matches the reclaimed share.
	mu.Lock()
	require.Equal(t, int64(40), evictedCost[2])
	mu.Unlock()

	// All of tenant 1's entries are retrievable.
	for i := 0; i < 4; i++ {
		_, ok := c.GetForTenant(1, fmt.Sprintf("a%d", i))
		require.True(t, ok)
	}
	// Global accounting stays consistent.
	require.Equal(t, int64(0), c.RemainingCost())
}

func TestCacheTenantHotColdReversal(t *testing.T) {
	c, err := NewCache(&Config[string, string]{
		NumCounters:        10000,
		MaxCost:            10,
		BufferItems:        64,
		Metrics:            true,
		IgnoreInternalCost: true,
	})
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.RegisterTenant(1, 0))
	require.NoError(t, c.RegisterTenant(2, 0))

	waitForFreq := func(tenant uint64, key string, want int64) {
		keyHash, _ := c.keyToHash(key)
		require.Eventually(t, func() bool {
			c.cachePolicy.Lock()
			defer c.cachePolicy.Unlock()
			ti, ok := c.cachePolicy.tenants[tenant]
			return ok && ti.freq.Estimate(keyHash) >= want
		}, 5*time.Second, time.Millisecond)
	}

	// Tenant 1 fills the cache and stays hot.
	for i := 0; i < 10; i++ {
		require.True(t, c.SetForTenant(1, fmt.Sprintf("a%d", i), "v", 1))
	}
	c.Wait()
	for r := 0; r < 5; r++ {
		for i := 0; i < 10; i++ {
			c.GetForTenant(1, fmt.Sprintf("a%d", i))
		}
	}
	waitForFreq(1, "a0", 5)

	// A cold item from tenant 2 cannot get in.
	require.True(t, c.SetForTenant(2, "b0", "v", 1))
	c.Wait()
	_, ok := c.Get("b0")
	require.False(t, ok)

	// Reversal: tenant 2 becomes hot, tenant 1 goes idle.
	for r := 0; r < 30; r++ {
		c.GetForTenant(2, "b0")
	}
	waitForFreq(2, "b0", 15)
	require.True(t, c.SetForTenant(2, "b0", "v", 1))
	c.Wait()
	_, ok = c.GetForTenant(2, "b0")
	require.True(t, ok)

	stats, _ := c.TenantStats(2)
	require.Equal(t, int64(1), stats.Used)
	stats, _ = c.TenantStats(1)
	require.Equal(t, int64(9), stats.Used)
}
