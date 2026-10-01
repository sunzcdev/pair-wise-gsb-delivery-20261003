// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"sync"
	"testing"
	"time"
)

func TestVerifyConfig_SessionWindow(t *testing.T) {
	conf := DefaultConfig()
	conf.MaxSessionWindowSize = initialStreamWindow - 1
	if err := VerifyConfig(conf); err == nil {
		t.Fatalf("expected error for MaxSessionWindowSize below initial window")
	}

	conf = DefaultConfig()
	conf.MaxSessionWindowSize = 0
	if err := VerifyConfig(conf); err != nil {
		t.Fatalf("zero MaxSessionWindowSize should be allowed: %v", err)
	}

	conf = DefaultConfig()
	conf.MaxSessionWindowSize = initialStreamWindow
	if err := VerifyConfig(conf); err != nil {
		t.Fatalf("valid MaxSessionWindowSize rejected: %v", err)
	}
}

func TestRecvBudget_GrantsUpToLimit(t *testing.T) {
	b := newRecvBudget(100)

	if g := b.tryAcquireNonBlocking(60); g != 60 {
		t.Fatalf("grant: exp=60 got=%d", g)
	}
	// Only 40 remain; a request for 60 is capped.
	if g := b.tryAcquireNonBlocking(60); g != 40 {
		t.Fatalf("grant: exp=40 got=%d", g)
	}
	// Nothing left.
	if g := b.tryAcquireNonBlocking(10); g != 0 {
		t.Fatalf("grant: exp=0 got=%d", g)
	}

	b.release(40)
	if g := b.tryAcquireNonBlocking(10); g != 10 {
		t.Fatalf("grant after release: exp=10 got=%d", g)
	}
}

func TestRecvBudget_DisabledGrantsEverything(t *testing.T) {
	b := newRecvBudget(0)
	if !b.disabled() {
		t.Fatalf("expected disabled budget")
	}
	if g := b.tryAcquireNonBlocking(1 << 30); g != 1<<30 {
		t.Fatalf("grant: exp=%d got=%d", 1<<30, g)
	}
	if g := b.tryAcquire(1<<30, nil); g != 1<<30 {
		t.Fatalf("grant: exp=%d got=%d", 1<<30, g)
	}
	// Releases are no-ops and must not panic.
	b.release(1 << 30)
}

func TestRecvBudget_BlocksUntilReleased(t *testing.T) {
	b := newRecvBudget(10)
	if g := b.tryAcquireNonBlocking(10); g != 10 {
		t.Fatalf("grant: exp=10 got=%d", g)
	}

	done := make(chan uint32, 1)
	go func() {
		done <- b.tryAcquire(10, make(chan struct{}))
	}()

	select {
	case g := <-done:
		t.Fatalf("acquire returned early with %d", g)
	case <-time.After(50 * time.Millisecond):
	}

	b.release(10)

	select {
	case g := <-done:
		if g != 10 {
			t.Fatalf("grant: exp=10 got=%d", g)
		}
	case <-time.After(time.Second):
		t.Fatalf("acquire did not wake after release")
	}
}

func TestRecvBudget_ShutdownUnblocksWaiters(t *testing.T) {
	b := newRecvBudget(10)
	b.tryAcquireNonBlocking(10)

	shutdown := make(chan struct{})
	done := make(chan uint32, 1)
	go func() {
		done <- b.tryAcquire(10, shutdown)
	}()

	time.Sleep(50 * time.Millisecond)
	close(shutdown)

	select {
	case g := <-done:
		if g != 0 {
			t.Fatalf("grant: exp=0 got=%d", g)
		}
	case <-time.After(time.Second):
		t.Fatalf("shutdown did not unblock waiter")
	}

	// budget.shutdown also unblocks waiters that watch only the budget.
	b.tryAcquireNonBlocking(0) // noop to keep used at 10
	done2 := make(chan uint32, 1)
	go func() {
		done2 <- b.tryAcquire(10, make(chan struct{}))
	}()
	time.Sleep(50 * time.Millisecond)
	b.shutdown()
	select {
	case g := <-done2:
		if g != 0 {
			t.Fatalf("grant: exp=0 got=%d", g)
		}
	case <-time.After(time.Second):
		t.Fatalf("budget shutdown did not unblock waiter")
	}
}

func TestRecvBudget_ConcurrentNoLeak(t *testing.T) {
	const limit = uint32(1000)
	b := newRecvBudget(limit)

	var wg sync.WaitGroup
	acquired := make(chan uint32, 2000)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				g := b.tryAcquireNonBlocking(7)
				if g > 0 {
					acquired <- g
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(acquired)
		close(done)
	}()

	var total uint64
	outstanding := uint64(0)
	for g := range acquired {
		outstanding += uint64(g)
		if outstanding > uint64(limit) {
			t.Fatalf("committed %d exceeds limit %d", outstanding, limit)
		}
		total += uint64(g)
		outstanding -= uint64(g) // release immediately
		b.release(g)
	}
	<-done

	b.mu.Lock()
	used := b.used
	b.mu.Unlock()
	if used != 0 {
		t.Fatalf("leaked budget: used=%d total=%d", used, total)
	}
}
