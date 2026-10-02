package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryContextPassesFreshContextPerAttempt(t *testing.T) {
	var seen []context.Context
	calls := 0

	_, err := RetryContext(context.Background(),
		func(ctx context.Context) (int, error) {
			calls++
			seen = append(seen, ctx)
			if calls < 3 {
				return 0, errors.New("boom")
			}
			return 7, nil
		},
		WithBackOff(&ZeroBackOff{}), withTimer(&testTimer{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[i-1] {
			t.Fatalf("attempt %d reused the previous attempt's context", i+1)
		}
	}
	// The child context is released immediately after its attempt returns, not
	// held until RetryContext itself returns.
	for i, ctx := range seen {
		select {
		case <-ctx.Done():
		default:
			t.Fatalf("context for attempt %d is still active after the attempt", i+1)
		}
	}
}

func TestRetryContextAttemptTimeoutCancelsRunningOperationAndRetries(t *testing.T) {
	// The operation blocks on its context and signals, through a channel that
	// is closed on return, exactly when each invocation has finished. These are
	// controlled channels: the test proves the in-flight operation receives
	// cancellation and that no old attempt is still running when RetryContext
	// moves on.
	released := make(chan struct{}, 4)
	calls := 0

	_, err := RetryContext(context.Background(),
		func(ctx context.Context) (int, error) {
			calls++
			if calls == 1 {
				<-ctx.Done()
				released <- struct{}{}
				return 0, ctx.Err()
			}
			return 9, nil
		},
		WithAttemptTimeout(20*time.Millisecond),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (timed-out attempt retried)", calls)
	}
	// No leaked attempt: the timed-out invocation returned before RetryContext
	// did. It was cancelled in place, never wrapped in an abandoned goroutine.
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("timed-out attempt was still running after RetryContext returned")
	}
}

func TestRetryContextOverallCancellationStopsInFlightAttempt(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	ctx, cancel := context.WithCancelCause(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := RetryContext(ctx,
			func(ctx context.Context) (int, error) {
				close(started)
				<-ctx.Done()
				close(released)
				return 0, ctx.Err()
			},
			WithBackOff(&ZeroBackOff{}),
			WithMaxElapsedTime(0),
			withTimer(&testTimer{}),
		)
		done <- err
	}()

	<-started
	cause := errors.New("shutting down")
	cancel(cause)

	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("RetryContext error = %v, want cause %v", err, cause)
		}
	case <-time.After(time.Second):
		t.Fatal("RetryContext did not return after overall cancellation")
	}

	// The in-flight attempt must observe the overall cancellation and return.
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("in-flight attempt did not receive cancellation")
	}
}

func TestRetryContextSuccessWinsOverDeadlineRace(t *testing.T) {
	t.Run("attempt timeout", func(t *testing.T) {
		calls := 0
		res, err := RetryContext(context.Background(),
			func(ctx context.Context) (int, error) {
				calls++
				// Return a value at about the same instant the deadline fires.
				// A synchronous, successful return is the verdict even though
				// the context is already done.
				<-ctx.Done()
				return 11, nil
			},
			WithAttemptTimeout(10*time.Millisecond),
			WithBackOff(&ZeroBackOff{}),
			withTimer(&testTimer{}),
		)
		if err != nil || res != 11 {
			t.Fatalf("RetryContext = (%d, %v), want (11, nil); success must win", res, err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (no retry on a successful attempt)", calls)
		}
	})

	t.Run("overall cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		res, err := RetryContext(ctx,
			func(ctx context.Context) (int, error) {
				calls++
				cancel()
				<-ctx.Done()
				return 12, nil
			},
			WithBackOff(&ZeroBackOff{}),
			withTimer(&testTimer{}),
		)
		if err != nil || res != 12 {
			t.Fatalf("RetryContext = (%d, %v), want (12, nil); success must win", res, err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
	})
}

func TestRetryContextAttemptTimeoutExhaustedReportsLastError(t *testing.T) {
	_, err := RetryContext(context.Background(),
		func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		},
		WithAttemptTimeout(time.Millisecond),
		WithMaxTries(2),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LastErr = %v, want context.DeadlineExceeded", err)
	}
}

// releaseTimer records whether the just-finished attempt's context was already
// cancelled when each backoff wait began. Start runs before RetryContext
// waits, while the attempt function has returned but RetryContext has not, so
// it observes that the cancel is prompt rather than deferred to the end.
type releaseTimer struct {
	timer          *time.Timer
	attemptCtx     []context.Context
	activeAtWait   int
	attemptsAtWait []int
}

func (t *releaseTimer) Start(time.Duration) {
	if last := len(t.attemptCtx) - 1; last >= 0 {
		select {
		case <-t.attemptCtx[last].Done():
		default:
			t.activeAtWait++
		}
		t.attemptsAtWait = append(t.attemptsAtWait, last+1)
	}
	t.timer = time.NewTimer(0)
}
func (t *releaseTimer) Stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
}
func (t *releaseTimer) C() <-chan time.Time { return t.timer.C }

func TestRetryContextTimerReleasedBeforeBackoffWait(t *testing.T) {
	tm := &releaseTimer{}
	calls := 0
	_, err := RetryContext(context.Background(),
		func(ctx context.Context) (int, error) {
			calls++
			tm.attemptCtx = append(tm.attemptCtx, ctx)
			if calls < 3 {
				return 0, errors.New("boom")
			}
			return 5, nil
		},
		WithAttemptTimeout(time.Hour),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(tm),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tm.attemptsAtWait) != 2 {
		t.Fatalf("backoff waits = %d, want 2", len(tm.attemptsAtWait))
	}
	if tm.activeAtWait != 0 {
		t.Fatalf("attempt context was still active at %d backoff wait(s); cancel must run as soon as the attempt returns", tm.activeAtWait)
	}
	for i, ctx := range tm.attemptCtx[:2] {
		select {
		case <-ctx.Done():
		default:
			t.Fatalf("attempt %d context still active after RetryContext returned", i+1)
		}
	}
}

func TestRetryContextAttemptTimeoutDoesNotCancelParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	_, err := RetryContext(parent,
		func(ctx context.Context) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		},
		WithAttemptTimeout(time.Millisecond),
		WithMaxTries(1),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		withTimer(&testTimer{}),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("Cause = %v, want ErrExhausted", err)
	}
	select {
	case <-parent.Done():
		t.Fatal("per-attempt timeout cancelled the overall context")
	default:
	}
}
