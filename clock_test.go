package backoff

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

type virtualClock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers []*virtualTimer
}

type virtualTimer struct {
	c        chan time.Time
	deadline time.Time
}

func newVirtualClock() *virtualClock {
	c := &virtualClock{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := &virtualTimer{
		c:        make(chan time.Time, 1),
		deadline: c.now.Add(d),
	}
	if d <= 0 {
		t.c <- c.now
		return &stoppableVirtualTimer{virtualTimer: t, clock: c}
	}

	c.timers = append(c.timers, t)
	c.cond.Broadcast()
	return &stoppableVirtualTimer{virtualTimer: t, clock: c}
}

func (t *virtualTimer) C() <-chan time.Time { return t.c }

func (t *virtualTimer) Stop() bool { return false }

func (t *virtualTimer) fire() {
	t.c <- t.deadline
}

func (c *virtualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)

	due := make([]*virtualTimer, 0)
	for i := 0; i < len(c.timers); {
		t := c.timers[i]
		if !t.deadline.After(c.now) {
			due = append(due, t)
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			continue
		}
		i++
	}
	c.mu.Unlock()

	sort.Slice(due, func(i, j int) bool {
		return due[i].deadline.Before(due[j].deadline)
	})
	for _, t := range due {
		t.fire()
	}
}

func (c *virtualClock) waitForTimers(t *testing.T, n int) {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.timers) != n {
		c.cond.Wait()
	}
}

func (c *virtualClock) pendingTimer() *virtualTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.timers) != 1 {
		return nil
	}
	return c.timers[0]
}

func (c *virtualClock) removeTimer(t *virtualTimer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, candidate := range c.timers {
		if candidate == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			c.cond.Broadcast()
			return true
		}
	}
	return false
}

type stoppableVirtualTimer struct {
	*virtualTimer
	clock *virtualClock
}

func (t *stoppableVirtualTimer) Stop() bool {
	return t.clock.removeTimer(t.virtualTimer)
}

type sequenceBackOff struct {
	durations []time.Duration
	next      int
}

func (b *sequenceBackOff) Reset() {}

func (b *sequenceBackOff) NextBackOff() time.Duration {
	if b.next >= len(b.durations) {
		return Stop
	}
	d := b.durations[b.next]
	b.next++
	return d
}

func TestRetryVirtualClockAllowsAttemptAtExactElapsedLimit(t *testing.T) {
	clock := newVirtualClock()
	const budget = 3 * time.Second

	var attempts int
	result := make(chan error, 1)
	go func() {
		_, err := Retry(
			context.Background(),
			func() (int, error) {
				attempts++
				if attempts == 3 {
					return 42, nil
				}
				return 0, errors.New("retry")
			},
			WithClock(clock),
			WithMaxElapsedTime(budget),
			WithBackOff(&sequenceBackOff{durations: []time.Duration{
				budget,
				0,
			}}),
		)
		result <- err
	}()

	clock.waitForTimers(t, 1)
	clock.Advance(budget)

	if err := <-result; err != nil {
		t.Fatalf("Retry returned error at exact elapsed limit: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestRetryVirtualClockAdvanceAcrossDeadlinesDoesNotBurst(t *testing.T) {
	clock := newVirtualClock()

	var attempts int
	result := make(chan error, 1)
	go func() {
		_, err := Retry(
			context.Background(),
			func() (int, error) {
				attempts++
				if attempts > 2 {
					return 0, Permanent(errors.New("unexpected attempt"))
				}
				return 0, errors.New("retry")
			},
			WithClock(clock),
			WithBackOff(NewConstantBackOff(time.Second)),
			WithMaxElapsedTime(3*time.Second),
		)
		result <- err
	}()

	clock.waitForTimers(t, 1)
	clock.Advance(3 * time.Second)

	if err := <-result; !errors.Is(err, ErrMaxElapsedTime) {
		t.Fatalf("error = %v, want ErrMaxElapsedTime", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2; crossing later deadlines must not burst", attempts)
	}
}

type gatedClock struct {
	Clock
	mu    sync.Mutex
	timer *gatedTimer
}

type gatedTimer struct {
	clock Clock
	inner Timer
	c     chan time.Time
}

func (c *gatedClock) NewTimer(d time.Duration) Timer {
	t := &gatedTimer{
		clock: c.Clock,
		inner: c.Clock.NewTimer(d),
		c:     make(chan time.Time, 1),
	}
	c.mu.Lock()
	c.timer = t
	c.mu.Unlock()
	return t
}

func (t *gatedTimer) C() <-chan time.Time { return t.c }

func (t *gatedTimer) Stop() bool { return t.inner.Stop() }

func (t *gatedTimer) release() {
	<-t.inner.C()
	t.c <- t.clock.Now()
}

func (c *gatedClock) firedTimer() *gatedTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timer
}

func TestRetryCancelsAfterTimerExpiredButBeforeItIsRead(t *testing.T) {
	virtual := newVirtualClock()
	clock := &gatedClock{Clock: virtual}
	ctx, cancel := context.WithCancelCause(context.Background())

	var attempts int
	result := make(chan error, 1)
	go func() {
		_, err := Retry(
			ctx,
			func() (int, error) {
				attempts++
				return 0, errors.New("retry")
			},
			WithClock(clock),
			WithBackOff(NewConstantBackOff(time.Second)),
			WithMaxElapsedTime(0),
		)
		result <- err
	}()

	virtual.waitForTimers(t, 1)
	virtual.Advance(time.Second)

	timer := clock.firedTimer()
	if len(timer.inner.C()) != 1 {
		t.Fatal("inner timer expired event was not delivered")
	}

	cancelCause := errors.New("cancel while timer is already expired")
	cancel(cancelCause)
	timer.release()

	if err := <-result; !errors.Is(err, cancelCause) {
		t.Fatalf("error = %v, want cancellation cause %v", err, cancelCause)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1; expired timer must not start another attempt", attempts)
	}
}

func TestTickerAdvanceAcrossDeadlinesDoesNotBurst(t *testing.T) {
	clock := newVirtualClock()
	ticker := NewTicker(NewConstantBackOff(time.Second), WithClock(clock))
	defer ticker.Stop()

	if tick := <-ticker.C; !tick.IsZero() {
		t.Fatalf("initial tick = %v, want zero time", tick)
	}

	clock.waitForTimers(t, 1)
	clock.Advance(3 * time.Second)

	if tick := <-ticker.C; tick != clock.now.Add(-2*time.Second) {
		t.Fatalf("tick = %v, want first deadline only", tick)
	}
	clock.waitForTimers(t, 1)

	select {
	case tick := <-ticker.C:
		t.Fatalf("unexpected tick from crossed deadline: %v", tick)
	default:
	}
}

func TestTickerStoppedTimerEventDoesNotWakeNewTicker(t *testing.T) {
	clock := newVirtualClock()
	first := NewTicker(NewConstantBackOff(time.Second), WithClock(clock))

	if tick := <-first.C; !tick.IsZero() {
		t.Fatalf("initial tick = %v, want zero time", tick)
	}
	clock.waitForTimers(t, 1)
	oldTimer := clock.pendingTimer()
	if oldTimer == nil {
		t.Fatal("timer not scheduled")
	}

	first.Stop()
	if _, ok := <-first.C; ok {
		t.Fatal("first ticker channel still open after Stop")
	}

	oldTimer.fire()

	second := NewTicker(&StopBackOff{}, WithClock(clock))
	defer second.Stop()

	if tick := <-second.C; !tick.IsZero() {
		t.Fatalf("second initial tick = %v, want zero time", tick)
	}
	if _, ok := <-second.C; ok {
		t.Fatal("old timer event woke the new ticker")
	}
}
