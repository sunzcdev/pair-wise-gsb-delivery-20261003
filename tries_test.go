package backoff

import (
	"context"
	"errors"
	"testing"
)

func TestRetryMaxTriesCount(t *testing.T) {
	// WithMaxTries(n) runs the operation exactly n times (total attempts, not
	// retries) and then stops with Cause ErrExhausted.
	for _, n := range []uint{1, 2, 5} {
		calls := 0
		_, err := Retry(
			context.Background(),
			func() (int, error) {
				calls++
				return 0, errors.New("boom")
			},
			WithMaxTries(n),
			WithBackOff(&ZeroBackOff{}),
			WithMaxElapsedTime(0),
			WithClock(immediateClock{}),
		)
		if calls != int(n) {
			t.Errorf("WithMaxTries(%d): operation called %d times, want %d", n, calls, n)
		}
		if !errors.Is(err, ErrExhausted) {
			t.Errorf("WithMaxTries(%d): Cause = %v, want ErrExhausted", n, err)
		}
	}
}

func TestRetryMaxTriesUnlimited(t *testing.T) {
	// The default, WithMaxTries(0), imposes no attempt limit: Retry keeps
	// trying until the operation succeeds.
	const successOn = 6
	calls := 0
	res, err := Retry(
		context.Background(),
		func() (int, error) {
			calls++
			if calls == successOn {
				return 42, nil
			}
			return 0, errors.New("boom")
		},
		WithMaxTries(0),
		WithBackOff(&ZeroBackOff{}),
		WithMaxElapsedTime(0),
		WithClock(immediateClock{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != successOn {
		t.Errorf("operation called %d times, want %d", calls, successOn)
	}
	if res != 42 {
		t.Errorf("res = %d, want 42", res)
	}
}
