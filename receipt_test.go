/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newReceiptTestCache(t *testing.T, maxCost int64) *Cache[string, int] {
	t.Helper()
	c, err := NewCache(&Config[string, int]{
		NumCounters:        1000,
		MaxCost:            maxCost,
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func TestReceiptApplied(t *testing.T) {
	c := newReceiptTestCache(t, 100)

	r, ok := c.SetWithReceipt("k", 1, 1)
	require.True(t, ok)
	require.Equal(t, WriteApplied, r.Wait())

	val, ok := c.Get("k")
	require.True(t, ok)
	require.Equal(t, 1, val)
}

func TestReceiptUpdate(t *testing.T) {
	c := newReceiptTestCache(t, 100)

	require.True(t, c.Set("k", 1, 1))
	c.Wait()

	// The key already exists, so this goes through the update path.
	r, ok := c.SetWithReceipt("k", 2, 1)
	require.True(t, ok)
	require.Equal(t, WriteApplied, r.Wait())

	val, ok := c.Get("k")
	require.True(t, ok)
	require.Equal(t, 2, val)
}

func TestReceiptWithTTL(t *testing.T) {
	c := newReceiptTestCache(t, 100)

	r, ok := c.SetWithTTLReceipt("k", 1, 1, time.Minute)
	require.True(t, ok)
	require.Equal(t, WriteApplied, r.Wait())

	// A negative TTL is a no-op and never enters the queue.
	r, ok = c.SetWithTTLReceipt("k", 1, 1, -time.Minute)
	require.False(t, ok)
	require.Equal(t, WriteDropped, r.Wait())
}

func TestReceiptCostExceeded(t *testing.T) {
	c := newReceiptTestCache(t, 10)

	r, ok := c.SetWithReceipt("big", 1, 100)
	require.True(t, ok)
	require.Equal(t, WriteCostExceeded, r.Wait())

	_, ok = c.Get("big")
	require.False(t, ok)
}

func TestReceiptRejected(t *testing.T) {
	c := newReceiptTestCache(t, 10)

	// Fill the cache with a frequently accessed item.
	require.True(t, c.Set("hot", 1, 10))
	c.Wait()

	// A new item with no hits should lose against the hot item. Retry to
	// give the Get counters time to reach the policy.
	var r *Receipt
	require.Eventually(t, func() bool {
		for i := 0; i < 100; i++ {
			c.Get("hot")
		}
		time.Sleep(wait)
		var ok bool
		r, ok = c.SetWithReceipt("cold", 2, 5)
		return ok && r.Wait() == WriteRejected
	}, 10*time.Second, 10*time.Millisecond)

	_, ok := c.Get("cold")
	require.False(t, ok)
}

func TestReceiptDroppedWhenQueueFull(t *testing.T) {
	c := newReceiptTestCache(t, 1<<40)

	var dropped atomic.Pointer[Receipt]
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < setBufSize; i++ {
				if dropped.Load() != nil {
					return
				}
				r, ok := c.SetWithReceipt(strconv.Itoa(base+i), i, 1)
				if !ok {
					dropped.CompareAndSwap(nil, r)
					return
				}
			}
		}(g * setBufSize)
	}
	wg.Wait()

	require.NotNil(t, dropped.Load())
	require.Equal(t, WriteDropped, dropped.Load().Wait())
}

func TestReceiptClosed(t *testing.T) {
	c := newReceiptTestCache(t, 100)
	c.Close()

	r, ok := c.SetWithReceipt("k", 1, 1)
	require.False(t, ok)
	require.Equal(t, WriteClosed, r.Wait())

	rd := c.DelWithReceipt("k")
	require.Equal(t, WriteClosed, rd.Wait())
}

func TestReceiptDel(t *testing.T) {
	c := newReceiptTestCache(t, 100)

	require.True(t, c.Set("k", 1, 1))
	c.Wait()

	rd := c.DelWithReceipt("k")
	require.Equal(t, WriteApplied, rd.Wait())

	_, ok := c.Get("k")
	require.False(t, ok)
}

// TestReceiptGenerations verifies that receipts track their own write
// operation: two Sets on the same key with a Del in between must resolve
// independently, without inferring earlier outcomes from the current value.
func TestReceiptGenerations(t *testing.T) {
	c := newReceiptTestCache(t, 10)

	// First generation: rejected because the cost exceeds MaxCost.
	r1, ok := c.SetWithReceipt("k", 1, 100)
	require.True(t, ok)
	// Delete sandwiched between the two writes.
	rd := c.DelWithReceipt("k")
	// Second generation: fits and is admitted.
	r2, ok := c.SetWithReceipt("k", 2, 5)
	require.True(t, ok)

	require.Equal(t, WriteCostExceeded, r1.Wait())
	require.Equal(t, WriteApplied, rd.Wait())
	require.Equal(t, WriteApplied, r2.Wait())

	// The final value comes from the second write, but the first write's
	// receipt still reports its own outcome.
	val, ok := c.Get("k")
	require.True(t, ok)
	require.Equal(t, 2, val)
}

func TestReceiptDoneChannel(t *testing.T) {
	c := newReceiptTestCache(t, 100)

	r, ok := c.SetWithReceipt("k", 1, 1)
	require.True(t, ok)

	select {
	case <-r.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("receipt was not resolved")
	}
	require.Equal(t, WriteApplied, r.Outcome())
}
