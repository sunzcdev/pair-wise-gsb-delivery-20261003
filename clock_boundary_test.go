package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

// runRetry executes Retry in a goroutine and returns the result once it stops.
// Nothing here sleeps: all waits are driven by the fake clock.
func runRetry[T any](t *testing.T, clk *fakeClock, ctx context.Context, op Operation[T], opts ...RetryOption) <-chan retryResult[T] {
	t.Helper()
	ch := make(chan retryResult[T], 1)
	go func() {
		all := append([]RetryOption{WithClock(clk)}, opts...)
		v, err := Retry(ctx, op, all...)
		ch <- retryResult[T]{v, err}
	}()
	return ch
}

type retryResult[T any] struct {
	v   T
	err error
}

// TestRetrySingleAdvanceDoesNotBurst verifies the core requirement: one
// virtual-time jump that crosses several backoff deadlines must not fire a
// burst of attempts that the elapsed-time budget would not allow. With a budget
// of exactly 30ms and a constant 10ms backoff, exactly four attempts run
// (t=0,10,20,30) and Retry stops with ErrMaxElapsedTime at t=30 — the budget
// is exhausted exactly, not overrun.
func TestRetrySingleAdvanceDoesNotBurst(t *testing.T) {
	const interval = 10 * time.Millisecond

	for _, jump := range []time.Duration{
		30 * interval, // cross all deadlines in one jump
		time.Hour,     // jump far past every deadline and the budget
	} {
		t.Run(jump.String(), func(t *testing.T) {
			clk := newFakeClock()
			stop := make(chan struct{})
			defer close(stop)

			attempts := make(chan struct{}, 16)
			op := func() (struct{}, error) {
				attempts <- struct{}{}
				return struct{}{}, errors.New("boom")
			}

			done := runRetry(t, clk, context.Background(), op,
				WithBackOff(NewConstantBackOff(interval)),
				WithMaxElapsedTime(30*time.Millisecond))

			clk.Advance(jump, stop)
			res := <-done
			if !errors.Is(res.err, ErrMaxElapsedTime) {
				t.Fatalf("error = %v, want ErrMaxElapsedTime", res.err)
			}

			if n := len(attempts); n != 4 {
				t.Errorf("attempts = %d, want 4 (t=0,10,20,30), no burst past the budget", n)
			}
		})
	}
}

// TestRetrySucceedsExactlyAtBudgetBoundary pins the boundary semantics: an
// attempt at exactly MaxElapsedTime is allowed (the next wait must not exceed
// the remaining budget; equality is permitted).
func TestRetrySucceedsExactlyAtBudgetBoundary(t *testing.T) {
	clk := newFakeClock()
	stop := make(chan struct{})
	defer close(stop)

	attempts := 0
	op := func() (int, error) {
		attempts++
		if attempts == 3 {
			return 7, nil
		}
		return 0, errors.New("boom")
	}

	done := runRetry(t, clk, context.Background(), op,
		WithBackOff(NewConstantBackOff(10*time.Millisecond)),
		WithMaxElapsedTime(20*time.Millisecond))

	clk.Advance(time.Hour, stop)

	res := <-done
	if res.err != nil || res.v != 7 {
		t.Fatalf("Retry = (%d, %v), want (7, nil) at exactly t=20ms", res.v, res.err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (t=0,10,20)", attempts)
	}
}

// TestRetryAttemptJustPastBudgetDoesNotRun is the other side of the boundary:
// a wait ending one unit past the budget is never scheduled.
func TestRetryAttemptJustPastBudgetDoesNotRun(t *testing.T) {
	clk := newFakeClock()
	stop := make(chan struct{})
	defer close(stop)

	attempts := make(chan struct{}, 8)
	op := func() (struct{}, error) {
		attempts <- struct{}{}
		return struct{}{}, errors.New("boom")
	}

	done := runRetry(t, clk, context.Background(), op,
		WithBackOff(NewConstantBackOff(11*time.Millisecond)),
		WithMaxElapsedTime(20*time.Millisecond))

	clk.Advance(time.Hour, stop)

	res := <-done
	if !errors.Is(res.err, ErrMaxElapsedTime) {
		t.Fatalf("error = %v, want ErrMaxElapsedTime", res.err)
	}
	if n := len(attempts); n != 2 {
		t.Errorf("attempts = %d, want 2 (t=0,11); the t=22 attempt must not run", n)
	}
}

// TestFakeTimerExpiredButNotRead covers the "timer fired but value not yet
// read" boundary at the timer level: leaving the value unread and stopping
// removes it, and a later timer has its own channel that stays empty until its
// own deadline is reached — the old value cannot leak forward.
func TestFakeTimerExpiredButNotRead(t *testing.T) {
	clk := newFakeClock()

	first := clk.NewTimer(10 * time.Millisecond)
	clk.Step(10 * time.Millisecond)

	if n := len(first.C()); n != 1 {
		t.Fatalf("fired timer channel len = %d, want 1 (expired but not read)", n)
	}

	// A new wait past its deadline must not fire at t=10ms.
	second := clk.NewTimer(10 * time.Millisecond)
	select {
	case <-second.C():
		t.Fatal("the new timer must not inherit the old fired value")
	default:
	}

	// Discarding the fired-but-unread first timer drains its value.
	if stopped := first.Stop(); stopped {
		t.Error("Stop on a fired timer should return false")
	}

	// The second timer fires only at its own deadline.
	clk.Step(10 * time.Millisecond)
	select {
	case tick := <-second.C():
		if !tick.Equal(clk.Now()) {
			t.Errorf("tick = %v, want %v", tick, clk.Now())
		}
	default:
		t.Fatal("the second timer should have fired at t=20ms")
	}
	second.Stop()
}

// TestRetryCancelledWhileTimerExpiredUnread covers the cancellation race
// boundary: the timer has already fired (its value is buffered and unread)
// when the context is cancelled. Cancellation must win the select handover
// exactly once: one attempt, the context cause, no ghost retry, no block.
func TestRetryCancelledWhileTimerExpiredUnread(t *testing.T) {
	const interval = 10 * time.Millisecond

	clk := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())

	attempts := 0
	done := runRetry(t, clk, ctx, func() (struct{}, error) {
		attempts++
		if attempts == 2 {
			cancel() // cancel during the operation, before the wait fires
		}
		return struct{}{}, errors.New("boom")
	},
		WithBackOff(NewConstantBackOff(interval)),
		WithMaxElapsedTime(0))

	// A single advance fires the wait after attempt 2. The context was already
	// cancelled, so Retry must take the cancellation branch and return; the
	// fired value must not wake a third attempt.
	stop := make(chan struct{})
	clk.Advance(interval, stop)
	close(stop)

	res := <-done
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", res.err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want exactly 2; an expired value woke a new wait", attempts)
	}
	if clk.activeTimers() != 0 {
		t.Errorf("%d timers leaked", clk.activeTimers())
	}
}

// TestTickerStopDropsPendingExpiredTimer verifies that after Stop, an old
// timer event that has not been consumed cannot wake a new tick cycle or be
// delivered afterwards.
func TestTickerStopDropsPendingExpiredTimer(t *testing.T) {
	const interval = time.Hour
	clk := newFakeClock()
	ticker := NewTickerWithClock(NewConstantBackOff(interval), clk)

	stop := make(chan struct{})
	defer close(stop)
	<-ticker.C // guaranteed first tick
	if !clk.waitTimers(1, stop) {
		t.Fatal("the wait after the first tick was never registered")
	}

	clk.Step(interval) // timer fires, value buffered and unread
	ticker.Stop()

	// Give the run goroutine no extra time: the buffered value belongs to the
	// stopped wait and must never be delivered as a new tick.
	select {
	case tm, ok := <-ticker.C:
		if ok {
			t.Errorf("unexpected tick delivered after Stop: %v", tm)
		}
	default:
		<-ticker.C // wait for run() to observe Stop and close
	}
	if clk.activeTimers() != 0 {
		t.Errorf("active timers = %d, want 0 after Stop", clk.activeTimers())
	}

	// Time passing afterwards must not deliver anything.
	clk.Step(10 * interval)
}

// TestTickerStopBeforeDeadlineKeepsOldEventDead verifies the complementary
// case: Stop before the deadline removes the timer; a later virtual jump
// produces no ticks.
func TestTickerStopBeforeDeadlineKeepsOldEventDead(t *testing.T) {
	clk := newFakeClock()
	ticker := NewTickerWithClock(NewConstantBackOff(time.Hour), clk)
	stop := make(chan struct{})
	defer close(stop)
	<-ticker.C
	if !clk.waitTimers(1, stop) {
		t.Fatal("the wait after the first tick was never registered")
	}

	ticker.Stop()
	clk.Step(10 * time.Hour)
	if _, ok := <-ticker.C; ok {
		t.Error("expected ticker channel to stay closed")
	}
	if clk.activeTimers() != 0 {
		t.Errorf("active timers = %d, want 0", clk.activeTimers())
	}
}

// TestTickerSingleAdvanceDoesNotBurst is the Ticker analogue of the Retry
// burst test: one jump crossing three deadlines delivers exactly three ticks,
// each driven by its own fresh timer — never duplicated old events.
func TestTickerSingleAdvanceDoesNotBurst(t *testing.T) {
	const interval = 10 * time.Millisecond
	clk := newFakeClock()
	ticker := NewTickerWithClock(NewConstantBackOff(interval), clk)

	stop := make(chan struct{})
	defer close(stop)
	advDone := make(chan struct{})
	go func() {
		// Do not advance until the wait after the guaranteed first tick is
		// registered, so the first tick cannot be missed.
		if !clk.waitTimers(1, stop) {
			close(advDone)
			return
		}
		clk.Advance(3*interval, stop)
		close(advDone)
	}()

	// Consume the four ticks (t=0,10,20,30) deterministically on this
	// goroutine while Advance drives the clock concurrently.
	var ticks []time.Time
	for range 4 {
		select {
		case tm, ok := <-ticker.C:
			if !ok {
				t.Fatalf("ticker closed after %d ticks, want 4", len(ticks))
			}
			ticks = append(ticks, tm)
		case <-advDone:
			t.Fatalf("advance finished after %d ticks, want 4", len(ticks))
		}
	}
	ticker.Stop()
	<-advDone

	if len(ticks) != 4 {
		t.Fatalf("ticks after advance = %d, want 4 (t=0,10,20,30)", len(ticks))
	}
	want := []time.Duration{0, interval, 2 * interval, 3 * interval}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, tm := range ticks {
		if !tm.Equal(base.Add(want[i])) {
			t.Errorf("tick %d = %v, want %v", i, tm, base.Add(want[i]))
		}
	}
}
