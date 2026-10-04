package pacer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gammazero/workerpool"
)

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

// fakeClock is a manually driven clock used to make the pacer's delay
// timer deterministic in tests. Each time the pacer starts waiting out a
// delay, the timer channel is made available via next().
type fakeClock struct {
	timers chan chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{timers: make(chan chan time.Time, 16)}
}

func (c *fakeClock) newTimer(time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	c.timers <- ch
	return ch, func() {}
}

// next waits until the pacer starts waiting out a new delay and returns
// that timer's channel. Receiving the channel synchronizes the test with
// the pacer: everything the pacer did before starting this wait (such as
// granting a release) has happened.
func (c *fakeClock) next(t *testing.T) chan<- time.Time {
	t.Helper()
	select {
	case ch := <-c.timers:
		return ch
	case <-time.After(10 * time.Second):
		t.Fatal("pacer did not start waiting out a new delay")
		return nil
	}
}

// advance fires the given timer, releasing the pacer from its delay wait.
func (c *fakeClock) advance(timer chan<- time.Time) {
	timer <- time.Now()
}

// awaitResult waits for a pacer waiter's result. The generous timeout is a
// deadlock guard, not a timing assumption.
func awaitResult(t *testing.T, ch <-chan bool) bool {
	t.Helper()
	select {
	case ok := <-ch:
		return ok
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for pacer waiter")
		return false
	}
}

func TestNextContextCancelWhilePaused(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	pacer := newPacer(time.Minute, clock.newTimer)
	defer pacer.Stop()

	// The first release is immediate.
	if !pacer.NextContext(context.Background()) {
		t.Fatal("first release should be granted immediately")
	}
	timer := clock.next(t)

	// Pause and let the pacer run into the pause barrier.
	pacer.Pause()
	clock.advance(timer)

	// A waiter is retained while the pacer is paused.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- pacer.NextContext(ctx) }()
	select {
	case <-result:
		t.Fatal("waiter should be retained while paused")
	default:
	}

	// Canceling the retained waiter releases it with a negative result.
	cancel()
	if awaitResult(t, result) {
		t.Fatal("canceled waiter should not acquire a release")
	}

	// Another waiter is still retained by the pause.
	result2 := make(chan bool, 1)
	go func() { result2 <- pacer.NextContext(context.Background()) }()
	select {
	case <-result2:
		t.Fatal("waiter should be retained while paused")
	default:
	}

	// Resume releases the queued waiter; the delay was already waited out
	// before the pause barrier, so no new delay is needed.
	pacer.Resume()
	if !awaitResult(t, result2) {
		t.Fatal("queued waiter should be released after resume")
	}
	clock.next(t)
}

func TestNextContextCanceledWaiterDoesNotStealRelease(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	pacer := newPacer(time.Minute, clock.newTimer)
	defer pacer.Stop()

	if !pacer.NextContext(context.Background()) {
		t.Fatal("first release should be granted immediately")
	}
	timer := clock.next(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := make(chan bool, 1)
	go func() { canceled <- pacer.NextContext(ctx) }()
	waiting := make(chan bool, 1)
	go func() { waiting <- pacer.NextContext(context.Background()) }()

	// Cancel the first waiter while the pacer is waiting out the delay.
	cancel()
	if awaitResult(t, canceled) {
		t.Fatal("canceled waiter should not acquire a release")
	}

	// The next release goes to the remaining waiter. If the canceled
	// waiter had consumed a release, the pacer would wait out another
	// delay first and the remaining waiter would not be released here.
	clock.advance(timer)
	if !awaitResult(t, waiting) {
		t.Fatal("queued waiter should acquire the release")
	}
	clock.next(t)
}

func TestNextContextReleaseWinsOverCancel(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	pacer := newPacer(time.Minute, clock.newTimer)
	defer pacer.Stop()

	if !pacer.NextContext(context.Background()) {
		t.Fatal("first release should be granted immediately")
	}
	timer := clock.next(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- pacer.NextContext(ctx) }()

	// Release the waiter, then cancel its context. The pacer starting a
	// new delay wait synchronizes the test with the grant, so when cancel
	// runs both the release and the cancellation are ready.
	clock.advance(timer)
	clock.next(t)
	cancel()

	if !awaitResult(t, result) {
		t.Fatal("a granted release must win over a concurrent cancellation")
	}
}

func TestStopUnblocksAllWaiters(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	pacer := newPacer(time.Minute, clock.newTimer)

	// Mix the old Next with NextContext; both share the release sequence.
	pacer.Next()
	clock.next(t)

	const waiters = 3
	results := make(chan bool, waiters)
	for range waiters {
		go func() { results <- pacer.NextContext(context.Background()) }()
	}
	nextDone := make(chan bool, 1)
	go func() {
		pacer.Next()
		nextDone <- true
	}()

	// Stop immediately releases every waiter with a consistent stop
	// result, without advancing the clock.
	pacer.Stop()
	for range waiters {
		if awaitResult(t, results) {
			t.Fatal("stopped pacer must report the stop result to all waiters")
		}
	}
	if !awaitResult(t, nextDone) {
		t.Fatal("Next should return after Stop")
	}

	// Stop is idempotent.
	pacer.Stop()

	// Calls after Stop return immediately with the stop result.
	if pacer.NextContext(context.Background()) {
		t.Fatal("NextContext after Stop must return false")
	}
	done := make(chan bool, 1)
	go func() {
		pacer.Next()
		done <- true
	}()
	if !awaitResult(t, done) {
		t.Fatal("Next after Stop must return immediately")
	}
}

func TestNextAndNextContextShareReleases(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	pacer := newPacer(time.Minute, clock.newTimer)
	defer pacer.Stop()

	// Next takes the immediate first release.
	pacer.Next()
	timer := clock.next(t)

	// NextContext waits out the same delay for the next release.
	result := make(chan bool, 1)
	go func() { result <- pacer.NextContext(context.Background()) }()
	select {
	case <-result:
		t.Fatal("NextContext should wait out the delay like Next")
	default:
	}
	clock.advance(timer)
	if !awaitResult(t, result) {
		t.Fatal("NextContext should acquire the next release")
	}
	clock.next(t)
}
