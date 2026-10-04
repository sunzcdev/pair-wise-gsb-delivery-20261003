package pacer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gammazero/workerpool"
)

// fakeClock is a manually driven timer source for deterministic tests.
type fakeClock struct {
	mu      sync.Mutex
	cond    sync.Cond
	pending []chan time.Time
}

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.cond.L = &c.mu
	return c
}

func (c *fakeClock) after(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.pending = append(c.pending, ch)
	c.cond.Signal()
	c.mu.Unlock()
	return ch
}

// waitTimer blocks until the pacer has registered a pending timer.
func (c *fakeClock) waitTimer() {
	c.mu.Lock()
	for len(c.pending) == 0 {
		c.cond.Wait()
	}
	c.mu.Unlock()
}

// advance fires all pending timers, simulating the delay elapsing.
func (c *fakeClock) advance() {
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- time.Now()
	}
}

func callNextContext(p *Pacer, ctx context.Context) <-chan bool {
	ch := make(chan bool, 1)
	go func() {
		ch <- p.NextContext(ctx)
	}()
	return ch
}

func callNext(p *Pacer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		p.Next()
		close(done)
	}()
	return done
}

func recvBool(t *testing.T, ch <-chan bool) bool {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for NextContext")
		return false
	}
}

func recvDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for pacer")
	}
}

func queuedLen(p *Pacer) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

func waitQueued(t *testing.T, p *Pacer, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for queuedLen(p) != n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d queued waiters, have %d", n, queuedLen(p))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPacedWorkers(t *testing.T) {
	t.Parallel()

	delay1 := 100 * time.Millisecond
	delay2 := 300 * time.Millisecond
	wp := workerpool.New(5)
	defer wp.Stop()

	pacer := NewPacer(delay1)
	defer pacer.Stop()

	slowPacer := NewPacer(delay2)
	defer slowPacer.Stop()

	tasksDone := new(sync.WaitGroup)
	tasksDone.Add(20)
	start := time.Now()

	pacedTask := pacer.Pace(func() {
		//fmt.Println("Task")
		tasksDone.Done()
	})

	slowPacedTask := slowPacer.Pace(func() {
		//fmt.Println("SlowTask")
		tasksDone.Done()
	})

	// Cause worker to be created, and available for reuse before next task.
	for range 10 {
		wp.Submit(pacedTask)
		wp.Submit(slowPacedTask)
	}

	time.Sleep(500 * time.Millisecond)
	pacer.Pause()
	time.Sleep(time.Second)
	if !pacer.IsPaused() {
		t.Fatal("should be paused")
	}
	pacer.Resume()
	if pacer.IsPaused() {
		t.Fatal("should not be paused")
	}

	tasksDone.Wait()
	elapsed := time.Since(start)
	// 9 times delay2 since no wait for first task, and pacer and slowPacer run
	// currently so only limiter is slowPacer.
	if elapsed < 9*delay2 {
		t.Fatal("Did not pace tasks correctly - finished too soon:", elapsed)
	}
}

func TestNextContextCancelDuringPause(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	p := newPacer(time.Second, clock.after)
	defer p.Stop()

	// The first waiter is released immediately.
	if !p.NextContext(context.Background()) {
		t.Fatal("first NextContext should acquire a beat immediately")
	}

	p.Pause()

	// A waiter queued while paused is retained, and cancelling it withdraws
	// it from the queue.
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := callNextContext(p, ctx)
	waitQueued(t, p, 1)
	cancel()
	if recvBool(t, cancelled) {
		t.Fatal("cancelled waiter should not acquire a beat")
	}
	waitQueued(t, p, 0)

	// A new waiter is still retained while paused.
	next := callNextContext(p, context.Background())
	waitQueued(t, p, 1)
	clock.waitTimer()

	// Resume continues releasing retained waiters on the next beat.
	p.Resume()
	clock.advance()
	if !recvBool(t, next) {
		t.Fatal("waiter should acquire a beat after resume")
	}
}

func TestNextContextCancelledWaiterDoesNotStealRelease(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	p := newPacer(time.Second, clock.after)
	defer p.Stop()

	if !p.NextContext(context.Background()) {
		t.Fatal("first NextContext should acquire a beat immediately")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := callNextContext(p, ctx)
	waitQueued(t, p, 1)
	survivor := callNextContext(p, context.Background())
	waitQueued(t, p, 2)
	clock.waitTimer()

	// Cancel the head waiter before the release tick fires.
	cancel()
	if recvBool(t, cancelled) {
		t.Fatal("cancelled waiter should not acquire a beat")
	}

	// A single tick releases the next waiter; the cancelled waiter did not
	// consume the release.
	clock.advance()
	if !recvBool(t, survivor) {
		t.Fatal("next waiter should acquire the release skipped by the cancelled waiter")
	}
}

func TestNextContextAlreadyCancelled(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	p := newPacer(time.Second, clock.after)
	defer p.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p.NextContext(ctx) {
		t.Fatal("NextContext with cancelled context should not acquire a beat")
	}
	// The cancelled call did not consume the first immediate release.
	if !p.NextContext(context.Background()) {
		t.Fatal("first waiter should acquire a beat immediately")
	}
}

func TestStopReleasesAllWaiters(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	p := newPacer(time.Second, clock.after)

	if !p.NextContext(context.Background()) {
		t.Fatal("first NextContext should acquire a beat immediately")
	}

	ctxResult := callNextContext(p, context.Background())
	nextDone := callNext(p)
	waitQueued(t, p, 2)

	p.Stop()

	// All waiters get a consistent stop result immediately.
	if recvBool(t, ctxResult) {
		t.Fatal("NextContext should report no beat after Stop")
	}
	recvDone(t, nextDone)

	// Calls after Stop get the stop result immediately.
	if p.NextContext(context.Background()) {
		t.Fatal("NextContext after Stop should not acquire a beat")
	}
	p.Next() // returns immediately

	// Stop is idempotent.
	p.Stop()
}

func TestNextAndNextContextMixed(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	p := newPacer(time.Second, clock.after)
	defer p.Stop()

	// Next and NextContext share a single FIFO queue; the first waiter is
	// released immediately.
	recvDone(t, callNext(p))

	first := callNext(p)
	waitQueued(t, p, 1)
	second := callNextContext(p, context.Background())
	waitQueued(t, p, 2)
	clock.waitTimer()

	// The older Next waiter is released on the next beat, while the
	// NextContext waiter remains queued.
	clock.advance()
	recvDone(t, first)
	waitQueued(t, p, 1)
	clock.waitTimer()

	clock.advance()
	if !recvBool(t, second) {
		t.Fatal("second waiter should acquire a beat")
	}
}
