package backoff

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// started/finished channels let the test decide, from outside the operation,
// exactly when the in-flight attempt is allowed to observe cancellation and
// return. They prove cancellation reaches the running operation instead of the
// retry abandoning an attempt that keeps running.
func TestRetryCtxParentCancelInterruptsInFlightAttempt(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	started := make(chan struct{})
	finished := make(chan struct{})
	parentErr := errors.New("parent done")

	go func() {
		<-started
		cancel(parentErr)
		<-finished // only let the attempt return after the parent is cancelled
	}()

	var saw atomic.Int64
	done := make(chan struct{})
	var result int
	var err error
	go func() {
		result, err = RetryCtx(ctx, func(attemptCtx context.Context) (int, error) {
			saw.Add(1)
			close(started)
			<-attemptCtx.Done() // block until cancellation is delivered
			close(finished)
			return 0, attemptCtx.Err()
		}, WithBackOff(&ZeroBackOff{}), withTimer(&testTimer{}))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RetryCtx did not stop after cancelling the parent context")
	}

	if got := saw.Load(); got != 1 {
		t.Errorf("attempts started = %d, want exactly 1 (no retry after overall cancel)", got)
	}
	if result != 0 {
		t.Errorf("result = %d, want zero value", result)
	}
	if !errors.Is(err, parentErr) {
		t.Errorf("Cause = %v, want parent cancellation cause %v", err, parentErr)
	}
	if re := AsRetryError(err); re == nil || re.Cause != parentErr {
		t.Errorf("RetryError.Cause = %v, want %v", err, parentErr)
	}
}

// An attempt timeout cancels only the attempt context: the running operation
// receives the cancellation, returns, and RetryCtx retries it with a fresh
// context per policy.
func TestRetryCtxAttemptTimeoutCancelsAndRetries(t *testing.T) {
	var calls atomic.Int64
	var timedOut atomic.Int64

	_, err := RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
		n := calls.Add(1)
		if n >= 2 {
			return 42, nil
		}
		<-attemptCtx.Done()
		if context.Cause(attemptCtx) == ErrAttemptTimeout {
			timedOut.Add(1)
		}
		return 0, context.Cause(attemptCtx)
	},
		WithAttemptTimeout(20*time.Millisecond),
		WithBackOff(&ZeroBackOff{}),
		withTimer(&testTimer{}),
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := timedOut.Load(); got != 1 {
		t.Errorf("attempts ended by timeout = %d, want 1", got)
	}
}

// When the attempt timeout eventually stops the loop (here via WithMaxTries),
// the terminal cause is the exhausted limit; the last error preserves the
// attempt's ErrAttemptTimeout cause, which errors.Is sees.
func TestRetryCtxAttemptTimeoutExhausts(t *testing.T) {
	var calls atomic.Int64
	_, err := RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
		calls.Add(1)
		<-attemptCtx.Done()
		return 0, context.Cause(attemptCtx)
	},
		WithAttemptTimeout(20*time.Millisecond),
		WithMaxTries(2),
		WithBackOff(&ZeroBackOff{}),
		withTimer(&testTimer{}),
	)

	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, ErrAttemptTimeout) {
		t.Errorf("last error does not contain ErrAttemptTimeout: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// Attempts are strictly serial: a new attempt never starts while the previous
// one is still executing — there is no goroutine-wrapped old attempt left
// running after cancellation.
func TestRetryCtxNoOverlappingAttempts(t *testing.T) {
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64
	var calls atomic.Int64

	_, err := RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
		n := calls.Add(1)
		c := concurrent.Add(1)
		for {
			m := maxConcurrent.Load()
			if c <= m || maxConcurrent.CompareAndSwap(m, c) {
				break
			}
		}
		if n >= 3 {
			concurrent.Add(-1)
			return 7, nil
		}
		<-attemptCtx.Done()
		concurrent.Add(-1)
		return 0, context.Cause(attemptCtx)
	},
		WithAttemptTimeout(15*time.Millisecond),
		WithBackOff(&ZeroBackOff{}),
		withTimer(&testTimer{}),
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("max concurrent attempts = %d, want 1 (old attempt must exit before the next starts)", got)
	}
}

// Every attempt receives a freshly derived context. Earlier attempt contexts
// are already cancelled and never reused.
func TestRetryCtxFreshContextPerAttempt(t *testing.T) {
	var seen []context.Context
	_, err := RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
		seen = append(seen, attemptCtx)
		if len(seen) < 3 {
			return 0, errors.New("fail")
		}
		return 1, nil
	}, WithBackOff(&ZeroBackOff{}), withTimer(&testTimer{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("attempts = %d, want 3", len(seen))
	}
	for i, c := range seen {
		for j, d := range seen {
			if i != j && c == d {
				t.Errorf("attempt %d reused context from attempt %d", j, i)
			}
		}
	}
	// Every earlier attempt context is cancelled once its attempt ended.
	for i, c := range seen[:len(seen)-1] {
		select {
		case <-c.Done():
		default:
			t.Errorf("attempt %d context was not cancelled after the attempt ended", i+1)
		}
	}
}

// The per-attempt cancel must fire as soon as the attempt returns, not when
// RetryCtx finishes. A failed attempt's context is already cancelled while the
// backoff wait is in progress — waiting to cancel at the end of Retry would
// leave it live here.
func TestRetryCtxAttemptContextCancelledBeforeBackoffWait(t *testing.T) {
	attemptEnded := make(chan context.Context, 1)
	waitEntered := make(chan struct{})

	tm := &gateTimer{enter: waitEntered}

	go func() {
		_, _ = RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
			if len(attemptEnded) == 0 {
				attemptEnded <- attemptCtx
				return 0, errors.New("fail")
			}
			return 1, nil
		}, WithBackOff(NewConstantBackOff(time.Hour)), WithMaxElapsedTime(0), withTimer(tm))
	}()

	var ended context.Context
	select {
	case ended = <-attemptEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("first attempt never returned")
	}

	// The backoff timer is now started (the long wait is in progress); the
	// first attempt's context must already be released.
	select {
	case <-waitEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("never entered the backoff wait")
	}
	select {
	case <-ended.Done():
	case <-time.After(time.Second):
		t.Fatal("attempt context was still live during the backoff wait; cancel deferred to end of Retry")
	}
	tm.release()
}

// gateTimer starts a long wait and signals when Start is called; release lets
// the wait complete so the test's retry can proceed.
type gateTimer struct {
	enter   chan struct{}
	timer   *time.Timer
	entered bool
}

func (t *gateTimer) Start(time.Duration) {
	if !t.entered {
		t.entered = true
		t.timer = time.NewTimer(time.Hour)
		close(t.enter)
		return
	}
	t.timer = time.NewTimer(0)
}

func (t *gateTimer) Stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
}

func (t *gateTimer) C() <-chan time.Time { return t.timer.C }

func (t *gateTimer) release() {
	if t.timer != nil {
		if !t.timer.Stop() {
			select {
			case <-t.timer.C:
			default:
			}
		}
		t.timer.Reset(0)
	}
}

// Success adjudication: when the attempt deadline fires at about the same
// moment the operation returns successfully, the returned value wins. The
// operation returns nil error despite the already-cancelled context.
func TestRetryCtxSuccessBeatsFiredAttemptDeadline(t *testing.T) {
	var calls atomic.Int64
	res, err := RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
		calls.Add(1)
		<-attemptCtx.Done() // deadline fired
		return 99, nil      // but the operation produced a value
	},
		WithAttemptTimeout(15*time.Millisecond),
		WithMaxTries(1),
		WithBackOff(&ZeroBackOff{}),
		withTimer(&testTimer{}),
	)

	if err != nil {
		t.Errorf("nil error should win over the fired deadline, got %v", err)
	}
	if res != 99 {
		t.Errorf("result = %d, want 99", res)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (success must not be retried)", got)
	}
}

// Parent cancellation outranks the per-attempt timeout when both end an
// attempt: the parent cause is reported, and no retry occurs.
func TestRetryCtxParentCancelBeatsAttemptTimeout(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	parentErr := errors.New("parent wins")

	started := make(chan struct{})
	var calls atomic.Int64

	go func() {
		<-started
		cancel(parentErr)
	}()

	_, err := RetryCtx(ctx, func(attemptCtx context.Context) (int, error) {
		calls.Add(1)
		close(started)
		<-attemptCtx.Done()
		return 0, context.Cause(attemptCtx)
	},
		WithAttemptTimeout(time.Hour),
		WithBackOff(&ZeroBackOff{}),
		withTimer(&testTimer{}),
	)

	if !errors.Is(err, parentErr) {
		t.Errorf("Cause = %v, want parent cause %v, not ErrAttemptTimeout", err, parentErr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

// A success on a later attempt releases that attempt's context immediately
// too: the final context is cancelled after RetryCtx returns normally.
func TestRetryCtxContextReleasedOnSuccess(t *testing.T) {
	var last context.Context
	res, err := RetryCtx(context.Background(), func(attemptCtx context.Context) (int, error) {
		called := last != nil
		last = attemptCtx
		if called {
			return 5, nil
		}
		return 0, errors.New("fail once")
	}, WithBackOff(&ZeroBackOff{}), withTimer(&testTimer{}))

	if err != nil || res != 5 {
		t.Fatalf("RetryCtx = (%d, %v), want (5, nil)", res, err)
	}
	if last == nil {
		t.Fatal("operation never ran")
	}
	select {
	case <-last.Done():
	default:
		t.Fatal("successful attempt context was not cancelled after returning")
	}
}

// WithAttemptTimeout has no effect on the context-free Retry: it stays
// retriable as before and does not pass a context to the operation.
func TestRetryIgnoresAttemptTimeout(t *testing.T) {
	var calls atomic.Int64
	_, err := Retry(context.Background(), func() (int, error) {
		if calls.Add(1) >= 2 {
			return 1, nil
		}
		return 0, errors.New("fail")
	},
		WithAttemptTimeout(time.Nanosecond),
		WithBackOff(&ZeroBackOff{}),
		withTimer(&testTimer{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}
