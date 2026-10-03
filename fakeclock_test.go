package backoff

import (
	"sort"
	"sync"
	"time"
)

// fakeClock is a controllable Clock for deterministic, sleep-free tests. Time
// only moves when Step or Advance is called.
//
// Each timer owns a capacity-one channel. Step fires non-blockingly: the value
// stays in the channel when nobody is reading ("expired but not yet read").
// Advance fires and then waits until every fired value is consumed (read or
// Stopped) and the consumer reacts (registers a next wait or settles), so one
// advance chains through deadlines without bursting. Every wait is a fresh
// timer with a fresh channel, so an old value can never reach a later wait.
type fakeClock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	timers  map[*fakeTimer]struct{}
	version uint64
}

func newFakeClock() *fakeClock {
	c := &fakeClock{
		timers: make(map[*fakeTimer]struct{}),
		now:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{
		clock: c,
		ch:    make(chan time.Time, 1),
		read:  make(chan struct{}),
		next:  c.now.Add(d),
	}
	c.timers[t] = struct{}{}
	c.version++
	c.cond.Broadcast()
	return t
}

// Step moves time to now+d and fires, in one non-blocking batch, every timer
// whose deadline is at or before the new time. With no receiver the value
// stays buffered ("expired but not yet read") until read or drained by Stop.
// Timers created afterwards are not fired by this call; use Advance to chain.
func (c *fakeClock) Step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.dueLocked(c.now) {
		t.fireLocked()
	}
}

// Advance moves time to now+d and chains through follow-up waits: after firing
// the timers due at a deadline it waits for each fired value to be consumed and
// for the consumer to register a next wait or settle, then continues. One call
// crosses many deadlines deterministically without bursting. Close stop once
// the driven phase is over.
func (c *fakeClock) Advance(d time.Duration, stop <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	stopDone := make(chan struct{})
	defer close(stopDone)
	go c.broadcastOnStop(stop, stopDone)

	target := c.now.Add(d)
	everFired := false
	var batchPrev []*fakeTimer
	for {
		if closed(stop) {
			return
		}

		next, found := c.earliestLocked()
		switch {
		case found && next.After(target):
			c.now = target
			return
		case !found:
			if everFired {
				// No follow-up yet after a batch: wait for the consumer to
				// register one or settle (Stop). Never assume "no timer"
				// means settled while the consumer is mid-handover.
				c.waitLocked(stop, func() bool {
					_, ok := c.earliestLocked()
					return ok || c.settledLocked(batchPrev) || closed(stop)
				})
				if closed(stop) {
					return
				}
				if _, ok := c.earliestLocked(); !ok && c.settledLocked(batchPrev) {
					c.now = target
					return
				}
				continue
			}
			// First wait not registered yet.
			c.waitLocked(stop, func() bool {
				_, ok := c.earliestLocked()
				return ok || closed(stop)
			})
			continue
		}

		batch := c.dueLocked(next)
		version := c.version
		for _, t := range batch {
			t.fireLocked()
		}
		c.now = next
		everFired = true
		batchPrev = batch

		// With the lock released, wait for every fired value to be consumed and
		// for the timer set to change past the snapshot (a follow-up registered
		// or the fired timers removed on settling).
		c.mu.Unlock()
		for _, t := range batch {
			select {
			case <-t.read:
			case <-stop:
				c.mu.Lock()
				return
			}
		}
		c.mu.Lock()
		c.waitLocked(stop, func() bool {
			return c.version != version || closed(stop)
		})
		if closed(stop) {
			return
		}
		// If the consumer settled (removed the fired timers without
		// registering a follow-up) the set is now empty (or holds only timers
		// from other consumers); detect settled state at the top of the loop.
	}
}

// settledLocked reports whether every timer in the last fired batch has been
// removed from the clock (i.e. the consumer Stopped it rather than chaining).
func (c *fakeClock) settledLocked(batch []*fakeTimer) bool {
	for _, t := range batch {
		if _, ok := c.timers[t]; ok {
			return false
		}
	}
	return true
}

func (c *fakeClock) broadcastOnStop(stop, done <-chan struct{}) {
	select {
	case <-stop:
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	case <-done:
	}
}

func (c *fakeClock) waitLocked(stop <-chan struct{}, ok func() bool) {
	for !ok() {
		if closed(stop) {
			return
		}
		c.cond.Wait()
	}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (c *fakeClock) earliestLocked() (time.Time, bool) {
	var next time.Time
	found := false
	for t := range c.timers {
		if t.fired {
			continue
		}
		if !found || t.next.Before(next) {
			next, found = t.next, true
		}
	}
	return next, found
}

func (c *fakeClock) dueLocked(deadline time.Time) []*fakeTimer {
	ready := make([]*fakeTimer, 0)
	for t := range c.timers {
		if !t.fired && !t.next.After(deadline) {
			ready = append(ready, t)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].next.Before(ready[j].next) })
	return ready
}

// activeTimers reports the number of timers the clock still tracks.
func (c *fakeClock) activeTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// waitTimers blocks until the clock tracks at least n timers, or stop closes.
func (c *fakeClock) waitTimers(n int, stop <-chan struct{}) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	done := make(chan struct{})
	defer close(done)
	go c.broadcastOnStop(stop, done)

	for len(c.timers) < n {
		if closed(stop) {
			return false
		}
		c.cond.Wait()
	}
	return true
}

// fakeTimer fires exactly once at a fixed virtual deadline.
type fakeTimer struct {
	clock *fakeClock
	ch    chan time.Time // capacity 1
	read  chan struct{}  // closed when the fired value is taken or drained
	next  time.Time
	fired bool
	once  sync.Once
	wg    sync.WaitGroup
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

// fireLocked buffers the firing value (non-blocking) and starts a watcher that
// closes read once the buffered value is received by a consumer. Caller holds
// the clock lock.
func (t *fakeTimer) fireLocked() {
	if t.fired {
		return
	}
	t.fired = true
	select {
	case t.ch <- t.next:
	default:
	}

	// Watch for the buffered value being received: attempt to send the value
	// a second time. While the first value is buffered this send cannot
	// proceed; the instant a consumer receives the first value, it succeeds and
	// read is closed. A sentinel is then drained by Stop.
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		select {
		case t.ch <- t.next:
			t.once.Do(func() { close(t.read) })
		case <-t.read:
		}
	}()
}

// Stop removes the timer and drains the buffered/duplicated value so the
// watcher goroutine exits; read is closed either way. Mirrors *time.Timer.Stop.
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	fired := t.fired
	delete(t.clock.timers, t)
	t.clock.version++
	t.clock.cond.Broadcast()
	t.clock.mu.Unlock()

	t.once.Do(func() { close(t.read) })
	if fired {
		// Drain the original buffered value and/or the watcher's duplicate.
		for range 2 {
			select {
			case <-t.ch:
			default:
			}
		}
		t.wg.Wait()
	}
	return !fired
}
