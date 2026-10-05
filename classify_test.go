package backoff

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Failure classes and sentinel errors used by the routing tests.
const (
	classConn = "connection"
	classRate = "rate-limit"
)

var (
	errConn = errors.New("connection refused")
	errRate = errors.New("too many requests")
)

// linearBackOff returns a jitter-free sequence (interval, 2*interval, ...) so a
// test can observe exactly where each class is in its backoff sequence.
type linearBackOff struct {
	interval time.Duration
	current  time.Duration
}

func (b *linearBackOff) Reset() { b.current = 0 }

func (b *linearBackOff) NextBackOff() time.Duration {
	if b.current == 0 {
		b.current = b.interval
	}
	d := b.current
	b.current += b.interval
	return d
}

func classified(err error) (ErrorClass, bool) {
	switch {
	case errors.Is(err, errConn):
		return classConn, true
	case errors.Is(err, errRate):
		return classRate, true
	default:
		return "other", false
	}
}

// runSequence feeds a Retry the error sequence classes (e.g. A, A, B, A) and
// records the wait requested before each retry, succeeding after the last one.
// It returns the waits and the per-class factory call counts.
func runSequence(t *testing.T, errs []error, routeOpts ...RouteOption) ([]time.Duration, map[ErrorClass]int) {
	t.Helper()

	attempt := 0
	tm := &spyTimer{}
	creates := make(map[ErrorClass]int)

	opt := WithClassifiedBackOff(classified,
		func(class ErrorClass) BackOff {
			creates[class]++
			switch class {
			case classConn:
				return &linearBackOff{interval: 1 * time.Second}
			case classRate:
				return &linearBackOff{interval: 1 * time.Minute}
			default:
				return &StopBackOff{}
			}
		},
		routeOpts...)

	_, err := Retry(context.Background(),
		func() (int, error) {
			i := attempt
			attempt++
			if i >= len(errs) {
				return 1, nil
			}
			return 0, errs[i]
		},
		WithMaxElapsedTime(0),
		withTimer(tm),
		opt,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return tm.starts, creates
}

// The A, A, B, A sequence distinguishes the two recovery rules by the fourth
// wait, not merely by the number of attempts.
func TestClassifiedBackOffSequenceRules(t *testing.T) {
	sequence := []error{errConn, errConn, errRate, errConn}

	t.Run("retain: each class resumes its own sequence after a switch", func(t *testing.T) {
		waits, creates := runSequence(t, sequence)

		// Attempt 1 A -> A's 1st wait; attempt 2 A -> A's 2nd wait.
		// Attempt 3 B -> B's 1st wait. Attempt 4 A -> A resumes at its 3rd wait,
		// because B did not reset A's progress.
		want := []time.Duration{
			1 * time.Second, // A (conn) step 1
			2 * time.Second, // A (conn) step 2
			1 * time.Minute, // B (rate) step 1
			3 * time.Second, // A (conn) step 3 — retained
		}
		if len(waits) != len(want) {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
		for i := range want {
			if waits[i] != want[i] {
				t.Errorf("wait %d = %v, want %v (full sequence %v)", i, waits[i], want[i], waits)
			}
		}
		// One BackOff is created per class, lazily, for this Retry alone.
		if creates[classConn] != 1 || creates[classRate] != 1 {
			t.Errorf("BackOff creates = %v, want one per class", creates)
		}
	})

	t.Run("reset: a switch back to A restarts A at its first interval", func(t *testing.T) {
		waits, _ := runSequence(t, sequence, WithResetOnSwitch())

		want := []time.Duration{
			1 * time.Second, // A step 1
			2 * time.Second, // A step 2
			1 * time.Minute, // B step 1
			1 * time.Second, // A restarts — reset on switch
		}
		if len(waits) != len(want) {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
		for i := range want {
			if waits[i] != want[i] {
				t.Errorf("wait %d = %v, want %v (full sequence %v)", i, waits[i], want[i], waits)
			}
		}
	})
}

func TestClassifiedBackOffNonRetriable(t *testing.T) {
	// A wrapped error in a class the classifier refuses to retry stops at once.
	tokenErr := errors.New("expired token")
	wrapped := fmt.Errorf("auth failed: %w", tokenErr)
	attempt := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			attempt++
			return 0, wrapped
		},
		WithMaxElapsedTime(0),
		withTimer(&spyTimer{}),
		WithClassifiedBackOff(classified, func(ErrorClass) BackOff {
			t.Fatal("BackOff must not be created for a non-retriable class")
			return nil
		}),
	)
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("Cause = %v, want ErrPermanent", err)
	}
	if attempt != 1 {
		t.Errorf("attempts = %d, want 1", attempt)
	}
	// Classification must not alter the original error chain.
	re := AsRetryError(err)
	if re == nil || re.LastErr != wrapped {
		t.Errorf("LastErr = %v, want the exact returned error %v", re, wrapped)
	}
	if !errors.Is(err, tokenErr) {
		t.Errorf("inner error not reachable through the chain: %v", err)
	}
}

func TestClassifiedBackOffPermanentPrecedence(t *testing.T) {
	// A Permanent error wins even though its underlying error belongs to a
	// retriable class, and the classifier is never consulted.
	attempt := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			attempt++
			return 0, Permanent(errConn)
		},
		WithMaxElapsedTime(0),
		withTimer(&spyTimer{}),
		WithClassifiedBackOff(
			func(error) (ErrorClass, bool) {
				t.Error("classifier must not run for a Permanent error")
				return "", true
			},
			func(ErrorClass) BackOff { return &ZeroBackOff{} },
		),
	)
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("Cause = %v, want ErrPermanent", err)
	}
	if attempt != 1 {
		t.Errorf("attempts = %d, want 1", attempt)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != errConn {
		t.Errorf("LastErr = %v, want %v", re, errConn)
	}
}

func TestClassifiedBackOffStop(t *testing.T) {
	// A retriable class whose BackOff returns Stop stops with ErrExhausted.
	_, err := Retry(context.Background(),
		func() (int, error) { return 0, errConn },
		WithMaxElapsedTime(0),
		withTimer(&spyTimer{}),
		WithClassifiedBackOff(classified, func(ErrorClass) BackOff { return &StopBackOff{} }),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, errConn) {
		t.Errorf("original error lost: %v", err)
	}
}

func TestClassifiedBackOffRetryAfter(t *testing.T) {
	// On a retriable error a RetryAfterError sets the wait and resets the active
	// class: A, A-with-RetryAfter, A yields 1s, requested, then 1s again.
	const requested = 5 * time.Second
	attempt := 0
	tm := &spyTimer{}
	sequence := []error{
		errConn,
		RetryAfter(requested, errRate), // B class carries an explicit delay
		errRate,
		errConn,
	}

	_, err := Retry(context.Background(),
		func() (int, error) {
			i := attempt
			attempt++
			if i >= len(sequence) {
				return 1, nil
			}
			return 0, sequence[i]
		},
		WithMaxElapsedTime(0),
		withTimer(tm),
		WithClassifiedBackOff(classified, func(class ErrorClass) BackOff {
			return &linearBackOff{interval: time.Second}
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A step 1, then B asks for 5s and resets B, B restarts at 1s, and A was not
	// reset so it resumes at step 2.
	want := []time.Duration{time.Second, requested, time.Second, 2 * time.Second}
	if len(tm.starts) != len(want) {
		t.Fatalf("waits = %v, want %v", tm.starts, want)
	}
	for i := range want {
		if tm.starts[i] != want[i] {
			t.Errorf("wait %d = %v, want %v (full %v)", i, tm.starts[i], want[i], tm.starts)
		}
	}
}

func TestClassifiedBackOffRetryAfterDoesNotGrantRetry(t *testing.T) {
	// The classifier's retry=false wins over an embedded RetryAfterError:
	// RetryAfter requests a delay, it does not override the stop verdict.
	attempt := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			attempt++
			return 0, RetryAfter(time.Hour, errors.New("rate limited but fatal"))
		},
		WithMaxElapsedTime(0),
		withTimer(&spyTimer{}),
		WithClassifiedBackOff(
			func(error) (ErrorClass, bool) { return "other", false },
			func(ErrorClass) BackOff { return &ZeroBackOff{} },
		),
	)
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("Cause = %v, want ErrPermanent", err)
	}
	if attempt != 1 {
		t.Errorf("attempts = %d, want 1", attempt)
	}
}

func TestClassifiedBackOffIndependentPerRetry(t *testing.T) {
	// Two concurrent Retry calls sharing the same options get independent class
	// BackOffs: each walks its own A sequence (1s, 2s) even when interleaved,
	// and the factory is invoked once per class per call.
	opt := WithClassifiedBackOff(classified, func(class ErrorClass) BackOff {
		switch class {
		case classConn:
			return &linearBackOff{interval: time.Second}
		default:
			return &linearBackOff{interval: time.Minute}
		}
	})

	const goroutines = 2
	var wg sync.WaitGroup
	results := make([][]time.Duration, goroutines)
	start := make(chan struct{})

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			attempt := 0
			tm := &spyTimer{}
			<-start
			_, err := Retry(context.Background(),
				func() (int, error) {
					attempt++
					if attempt > 2 {
						return 1, nil
					}
					return 0, errConn
				},
				WithMaxElapsedTime(0),
				withTimer(tm),
				opt,
			)
			if err != nil {
				t.Errorf("goroutine %d: %v", g, err)
			}
			results[g] = tm.starts
		}(g)
	}
	close(start)
	wg.Wait()

	for g, waits := range results {
		want := []time.Duration{time.Second, 2 * time.Second}
		if len(waits) != len(want) || waits[0] != want[0] || waits[1] != want[1] {
			t.Errorf("goroutine %d waits = %v, want %v (shared mutable state?)", g, waits, want)
		}
	}
}
