package backoff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"sync"
	"testing"
	"time"
)

// immediateTimer fires as soon as it is read, without sleeping.
type immediateTimer struct{}

func (immediateTimer) C() <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func (immediateTimer) Stop() bool { return false }

// immediateClock schedules every wait to fire immediately.
type immediateClock struct{}

func (immediateClock) Now() time.Time               { return time.Now() }
func (immediateClock) NewTimer(time.Duration) Timer { return immediateTimer{} }

func TestRetry(t *testing.T) {
	const successOn = 3
	var i = 0

	// This function is successful on "successOn" calls.
	f := func() (bool, error) {
		i++
		log.Printf("function is called %d. time\n", i)

		if i == successOn {
			log.Println("OK")
			return true, nil
		}

		log.Println("error")
		return false, errors.New("error")
	}

	_, err := Retry(context.Background(), f, WithBackOff(NewExponentialBackOff()), WithClock(immediateClock{}))
	if err != nil {
		t.Errorf("unexpected error: %s", err.Error())
	}
	if i != successOn {
		t.Errorf("invalid number of retries: %d", i)
	}
}

func TestRetryWithData(t *testing.T) {
	const successOn = 3
	var i = 0

	// This function is successful on "successOn" calls.
	f := func() (int, error) {
		i++
		log.Printf("function is called %d. time\n", i)

		if i == successOn {
			log.Println("OK")
			return 42, nil
		}

		log.Println("error")
		return 1, errors.New("error")
	}

	res, err := Retry(context.Background(), f, WithBackOff(NewExponentialBackOff()), WithClock(immediateClock{}))
	if err != nil {
		t.Errorf("unexpected error: %s", err.Error())
	}
	if i != successOn {
		t.Errorf("invalid number of retries: %d", i)
	}
	if res != 42 {
		t.Errorf("invalid data in response: %d, expected 42", res)
	}
}

func TestRetryContext(t *testing.T) {
	var cancelOn = 3
	var i = 0

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)

	expectedErr := errors.New("custom error")

	// This function cancels context on "cancelOn" calls.
	f := func() (bool, error) {
		i++
		log.Printf("function is called %d. time\n", i)

		// cancelling the context in the operation function is not a typical
		// use-case, however it allows to get predictable test results.
		if i == cancelOn {
			cancel(expectedErr)
		}

		log.Println("error")
		return false, fmt.Errorf("error (%d)", i)
	}

	_, err := Retry(ctx, f, WithBackOff(NewConstantBackOff(time.Millisecond)), WithClock(immediateClock{}))
	if err == nil {
		t.Errorf("error is unexpectedly nil")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("unexpected error: %s", err.Error())
	}
	if i != cancelOn {
		t.Errorf("invalid number of retries: %d", i)
	}
}

// https://github.com/cenkalti/backoff/issues/181
func TestRetryContextErrorIncludesOperationError(t *testing.T) {
	opErr := errors.New("operation error")
	ctxErr := errors.New("context error")

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	i := 0
	f := func() (bool, error) {
		i++
		if i == 2 {
			cancel(ctxErr)
		}
		return false, opErr
	}

	_, err := Retry(ctx, f, WithBackOff(NewConstantBackOff(time.Millisecond)), WithClock(immediateClock{}))
	if !errors.Is(err, ctxErr) {
		t.Errorf("context error not in result: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}
}

// https://github.com/cenkalti/backoff/issues/181
func TestRetryMaxElapsedTimeErrorIncludesOperationError(t *testing.T) {
	opErr := errors.New("operation error")

	_, err := Retry(
		context.Background(),
		func() (bool, error) { return false, opErr },
		WithMaxElapsedTime(time.Millisecond),
		WithClock(immediateClock{}),
	)
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Errorf("ErrMaxElapsedTime not in result: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}
}

func TestRetryError(t *testing.T) {
	opErr := errors.New("operation error")

	// MaxTries reached: Cause is ErrExhausted, LastErr is the operation error.
	_, err := Retry(
		context.Background(),
		func() (bool, error) { return false, opErr },
		WithMaxTries(1),
		WithClock(immediateClock{}),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("ErrExhausted not in result: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}

	// errors.As exposes the structured fields.
	var re *RetryError
	if !errors.As(err, &re) {
		t.Fatalf("result is not a *RetryError: %v", err)
	}
	if re.LastErr != opErr {
		t.Errorf("LastErr = %v, want %v", re.LastErr, opErr)
	}
	if re.Cause != ErrExhausted {
		t.Errorf("Cause = %v, want %v", re.Cause, ErrExhausted)
	}

	// Backoff policy returning Stop also reports ErrExhausted.
	_, err = Retry(
		context.Background(),
		func() (bool, error) { return false, opErr },
		WithBackOff(&StopBackOff{}),
		WithClock(immediateClock{}),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("ErrExhausted not in result on Stop: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result on Stop: %v", err)
	}
}

func TestRetryPermanent(t *testing.T) {
	ensureRetries := func(test string, shouldRetry bool, f func() (int, error), expectRes int) {
		numRetries := -1
		maxRetries := 1

		res, _ := Retry(
			context.Background(),
			func() (int, error) {
				numRetries++
				if numRetries >= maxRetries {
					return -1, Permanent(errors.New("forced"))
				}
				return f()
			},
			WithBackOff(NewExponentialBackOff()),
			WithClock(immediateClock{}),
		)

		if shouldRetry && numRetries == 0 {
			t.Errorf("Test: '%s', backoff should have retried", test)
		}

		if !shouldRetry && numRetries > 0 {
			t.Errorf("Test: '%s', backoff should not have retried", test)
		}

		if res != expectRes {
			t.Errorf("Test: '%s', got res %d but expected %d", test, res, expectRes)
		}
	}

	for _, testCase := range []struct {
		name        string
		f           func() (int, error)
		shouldRetry bool
		res         int
	}{
		{
			"nil test",
			func() (int, error) {
				return 1, nil
			},
			false,
			1,
		},
		{
			"io.EOF",
			func() (int, error) {
				return 2, io.EOF
			},
			true,
			-1,
		},
		{
			"Permanent(io.EOF)",
			func() (int, error) {
				return 3, Permanent(io.EOF)
			},
			false,
			3,
		},
		{
			"Wrapped: Permanent(io.EOF)",
			func() (int, error) {
				return 4, fmt.Errorf("Wrapped error: %w", Permanent(io.EOF))
			},
			false,
			4,
		},
	} {
		ensureRetries(testCase.name, testCase.shouldRetry, testCase.f, testCase.res)
	}
}

func TestPermanent(t *testing.T) {
	want := errors.New("foo")
	other := errors.New("bar")
	err := Permanent(want)

	if got := errors.Unwrap(err); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if !errors.Is(err, want) {
		t.Errorf("err: %v is not %v", err, want)
	}
	if errors.Is(err, other) {
		t.Errorf("err: %v is %v", err, other)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("err: %v is not ErrPermanent", err)
	}

	// A Permanent error stays detectable through wrapping.
	wrapped := fmt.Errorf("wrapped: %w", err)
	if !errors.Is(wrapped, ErrPermanent) {
		t.Errorf("wrapped: %v is not ErrPermanent", wrapped)
	}
	if !errors.Is(wrapped, want) {
		t.Errorf("wrapped: %v is not %v", wrapped, want)
	}

	if Permanent(nil) != nil {
		t.Errorf("Permanent(nil) should be nil")
	}
}

func TestRetryPermanentError(t *testing.T) {
	opErr := errors.New("operation error")

	_, err := Retry(
		context.Background(),
		func() (bool, error) { return false, Permanent(opErr) },
		WithClock(immediateClock{}),
	)
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("ErrPermanent not in result: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}

	re := AsRetryError(err)
	if re == nil {
		t.Fatalf("result is not a *RetryError: %v", err)
	}
	if re.Cause != ErrPermanent {
		t.Errorf("Cause = %v, want ErrPermanent", re.Cause)
	}
	if re.LastErr != opErr {
		t.Errorf("LastErr = %v, want %v", re.LastErr, opErr)
	}
}

// Permanent error bubbles up when WithMaxTries(1)
// https://github.com/cenkalti/backoff/issues/177
func TestIssue177(t *testing.T) {
	dummyErr := errors.New("dummy")
	operation := func() (int, error) {
		return 0, Permanent(dummyErr)
	}
	for i := range uint(3) {
		_, err := Retry(context.TODO(), operation, WithMaxTries(i))
		if !errors.Is(err, dummyErr) {
			t.Errorf("unexpected error: %v", err)
		}
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("error is not ErrPermanent: %v", err)
		}
	}
}

// spyClock records the durations it is asked to wait, then fires immediately.
type spyClock struct {
	mu     sync.Mutex
	starts []time.Duration
}

func (c *spyClock) Now() time.Time { return time.Now() }

func (c *spyClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	c.starts = append(c.starts, d)
	c.mu.Unlock()
	return immediateTimer{}
}

func (c *spyClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.starts...)
}

// countingBackOff records how many times it is reset and queried.
type countingBackOff struct{ resets, nexts int }

func (b *countingBackOff) Reset() { b.resets++ }

func (b *countingBackOff) NextBackOff() time.Duration {
	b.nexts++
	return time.Second
}

func TestRetryContextDeadline(t *testing.T) {
	// A context deadline surfaces as Cause context.DeadlineExceeded — distinct
	// from WithMaxElapsedTime's ErrMaxElapsedTime — with LastErr preserved.
	opErr := errors.New("operation error")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()

	_, err := Retry(ctx, func() (int, error) { return 0, opErr }, WithClock(immediateClock{}))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Cause is not context.DeadlineExceeded: %v", err)
	}
	if !errors.Is(err, opErr) {
		t.Errorf("operation error not in result: %v", err)
	}
	if re := AsRetryError(err); re == nil || re.Cause != context.DeadlineExceeded {
		t.Errorf("RetryError.Cause = %v, want context.DeadlineExceeded", err)
	}
}

func TestRetryNotify(t *testing.T) {
	t.Run("fires between attempts with the failing error, not on success", func(t *testing.T) {
		const successOn = 3
		calls := 0
		var got []string
		_, err := Retry(context.Background(),
			func() (int, error) {
				calls++
				if calls == successOn {
					return 1, nil
				}
				return 0, fmt.Errorf("attempt %d", calls)
			},
			WithBackOff(&ZeroBackOff{}), WithClock(immediateClock{}),
			WithNotify(func(e error, _ time.Duration) { got = append(got, e.Error()) }),
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0] != "attempt 1" || got[1] != "attempt 2" {
			t.Errorf("Notify errors = %v, want [attempt 1 attempt 2]", got)
		}
	})

	t.Run("not called for the terminal, exhausting error", func(t *testing.T) {
		// Two attempts both fail: Notify fires once (the single retry), not for
		// the second attempt that hits the limit and stops Retry.
		n := 0
		_, err := Retry(context.Background(),
			func() (int, error) { return 0, errors.New("boom") },
			WithMaxTries(2), WithBackOff(&ZeroBackOff{}), WithClock(immediateClock{}),
			WithNotify(func(error, time.Duration) { n++ }),
		)
		if !errors.Is(err, ErrExhausted) {
			t.Fatalf("Cause = %v, want ErrExhausted", err)
		}
		if n != 1 {
			t.Errorf("Notify called %d times, want 1 (only the retry, not the terminal error)", n)
		}
	})

	t.Run("not called on a permanent error", func(t *testing.T) {
		n := 0
		_, _ = Retry(context.Background(),
			func() (int, error) { return 0, Permanent(errors.New("nope")) },
			WithClock(immediateClock{}),
			WithNotify(func(error, time.Duration) { n++ }),
		)
		if n != 0 {
			t.Errorf("Notify called %d times on a permanent error, want 0", n)
		}
	})

	t.Run("not called on context cancellation", func(t *testing.T) {
		n := 0
		ctx, cancel := context.WithCancel(context.Background())
		_, _ = Retry(ctx,
			func() (int, error) { cancel(); return 0, errors.New("boom") },
			WithBackOff(&ZeroBackOff{}), WithClock(immediateClock{}),
			WithNotify(func(error, time.Duration) { n++ }),
		)
		if n != 0 {
			t.Errorf("Notify called %d times on cancellation, want 0", n)
		}
	})
}

func TestRetryAfter(t *testing.T) {
	// A RetryAfterError overrides the next wait with its own duration and
	// resets the backoff policy.
	const retryAfter = 42 * time.Second
	bo := &countingBackOff{}
	tm := &spyClock{}
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			if calls == 1 {
				return 0, &RetryAfterError{Duration: retryAfter}
			}
			return 1, nil
		},
		WithBackOff(bo),
		WithMaxElapsedTime(0),
		WithClock(tm),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tm.waits()) != 1 || tm.waits()[0] != retryAfter {
		t.Errorf("timer waits = %v, want a single wait of %v", tm.waits(), retryAfter)
	}
	// Reset is called once at the start of Retry and again because of RetryAfter.
	if bo.resets != 2 {
		t.Errorf("BackOff.Reset called %d times, want 2 (initial + RetryAfter)", bo.resets)
	}
}

func TestRetryAfterPolicyStop(t *testing.T) {
	// The policy's Stop wins over a RetryAfterError: no wait, no retry.
	cause := errors.New("rate limited")
	tm := &spyClock{}
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, RetryAfter(time.Second, cause)
		},
		WithBackOff(&StopBackOff{}),
		WithMaxElapsedTime(0),
		WithClock(tm),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("error = %v, want ErrExhausted", err)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != cause {
		t.Errorf("LastErr = %v, want %v", re, cause)
	}
	if calls != 1 || len(tm.waits()) != 0 {
		t.Errorf("calls = %d, waits = %v; want one call and no wait", calls, tm.waits())
	}
}

func TestRetryAfterTypedNil(t *testing.T) {
	// A typed-nil *RetryAfterError is retried like a plain error, not a panic.
	calls := 0
	tm := &spyClock{}

	res, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			if calls > 1 {
				return 1, nil
			}
			var rae *RetryAfterError
			return 0, rae
		},
		WithBackOff(NewConstantBackOff(time.Second)),
		WithClock(tm),
	)
	if err != nil || res != 1 {
		t.Fatalf("Retry = (%d, %v), want (1, nil)", res, err)
	}
	if len(tm.waits()) != 1 || tm.waits()[0] != time.Second {
		t.Errorf("timer waits = %v, want the policy's 1s", tm.waits())
	}
}

func TestRetryAfterError(t *testing.T) {
	// nil cause: behaves like a bare retry-after, Unwrap is nil.
	err := RetryAfter(3*time.Second, nil)
	var ra *RetryAfterError
	if !errors.As(err, &ra) {
		t.Fatalf("RetryAfter did not return a *RetryAfterError: %v", err)
	}
	if ra.Duration != 3*time.Second {
		t.Errorf("Duration = %v, want 3s", ra.Duration)
	}
	if got, want := ra.Error(), "retry after 3s"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if errors.Unwrap(err) != nil {
		t.Errorf("Unwrap() = %v, want nil", errors.Unwrap(err))
	}

	// With a cause: it is wrapped (Unwrap/Is see it) and shown in the message.
	cause := errors.New("rate limited")
	werr := RetryAfter(3*time.Second, cause)
	if !errors.Is(werr, cause) {
		t.Errorf("RetryAfter(_, cause) does not wrap cause: %v", werr)
	}
	if got, want := werr.Error(), "rate limited (retry after 3s)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// https://github.com/cenkalti/backoff/issues/184
func TestRetryAfterCarriesError(t *testing.T) {
	// When an operation returns RetryAfter with a cause and retrying then stops
	// (here via WithMaxTries), the cause — not the RetryAfterError — is reported
	// as LastErr, so the error context is not discarded.
	cause := errors.New("rate limited")
	_, err := Retry(
		context.Background(),
		func() (int, error) { return 0, RetryAfter(time.Second, cause) },
		WithMaxTries(1),
		WithClock(immediateClock{}),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("cause not in result: %v", err)
	}
	re := AsRetryError(err)
	if re == nil || re.LastErr != cause {
		t.Errorf("LastErr = %v, want %v", re, cause)
	}

	// A nil cause keeps the previous behavior: LastErr is the RetryAfterError
	// itself.
	_, err = Retry(
		context.Background(),
		func() (int, error) { return 0, RetryAfter(time.Second, nil) },
		WithMaxTries(1),
		WithClock(immediateClock{}),
	)
	if re := AsRetryError(err); re == nil {
		t.Fatalf("result is not a *RetryError: %v", err)
	} else if _, ok := re.LastErr.(*RetryAfterError); !ok {
		t.Errorf("LastErr = %T, want *RetryAfterError", re.LastErr)
	}
}

func TestRetryErrorString(t *testing.T) {
	re := &RetryError{LastErr: errors.New("last"), Cause: ErrExhausted}
	if got, want := re.Error(), "backoff: retries exhausted (last error: last)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// RetryError implements Unwrap() []error, so the single-value errors.Unwrap
	// returns nil; callers must use errors.Is/As. This pins the documented gotcha.
	if got := errors.Unwrap(error(re)); got != nil {
		t.Errorf("errors.Unwrap(RetryError) = %v, want nil", got)
	}
	if AsRetryError(nil) != nil {
		t.Error("AsRetryError(nil) should be nil")
	}
}

func TestRetryElapsedTimeOverflow(t *testing.T) {
	const maxDuration time.Duration = math.MaxInt64
	operationErr := errors.New("retry me")
	tests := []struct {
		name    string
		backOff BackOff
		err     error
	}{
		{"backoff", NewConstantBackOff(maxDuration), operationErr},
		{"retry-after", &ZeroBackOff{}, RetryAfter(maxDuration, operationErr)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			tm := &spyClock{}
			_, err := Retry(context.Background(), func() (int, error) {
				calls++
				if calls > 1 {
					return 1, nil
				}
				time.Sleep(time.Millisecond) // Ensure elapsed time is non-zero.
				return 0, tt.err
			}, WithBackOff(tt.backOff), WithMaxElapsedTime(time.Second), WithClock(tm))
			if !errors.Is(err, ErrMaxElapsedTime) {
				t.Fatalf("error = %v, want ErrMaxElapsedTime", err)
			}
			if re := AsRetryError(err); re == nil || re.LastErr != operationErr {
				t.Errorf("LastErr = %v, want %v", re, operationErr)
			}
			if calls != 1 || len(tm.waits()) != 0 {
				t.Errorf("calls = %d, waits = %v; want one call and no wait", calls, tm.waits())
			}
		})
	}
}

func TestRetryAfterNegativeDuration(t *testing.T) {
	// A negative delay must not extend the elapsed-time budget.
	calls := 0
	tm := &spyClock{}
	_, err := Retry(context.Background(), func() (int, error) {
		calls++
		if calls > 1 {
			return 1, nil
		}
		time.Sleep(2 * time.Millisecond)
		return 0, RetryAfter(-time.Hour, errors.New("rate limited"))
	}, WithMaxElapsedTime(time.Millisecond), WithClock(tm))
	if !errors.Is(err, ErrMaxElapsedTime) {
		t.Fatalf("error = %v, want ErrMaxElapsedTime", err)
	}
	if calls != 1 || len(tm.waits()) != 0 {
		t.Errorf("calls = %d, waits = %v; want one call and no wait", calls, tm.waits())
	}
}

func TestRetryAfterNegativeDurationWaitsZero(t *testing.T) {
	calls := 0
	tm := &spyClock{}
	_, err := Retry(context.Background(), func() (int, error) {
		calls++
		if calls > 1 {
			return 1, nil
		}
		return 0, RetryAfter(Stop, errors.New("rate limited"))
	}, WithClock(tm))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tm.waits()) != 1 || tm.waits()[0] != 0 {
		t.Errorf("waits = %v, want a single wait of 0", tm.waits())
	}
}
