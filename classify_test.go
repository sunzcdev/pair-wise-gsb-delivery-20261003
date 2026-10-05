package backoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptedBackOff plays back a fixed sequence of delays, repeating the last
// one when exhausted. Reset restarts the sequence from the beginning.
type scriptedBackOff struct {
	seq   []time.Duration
	nexts int
}

func (b *scriptedBackOff) Reset() { b.nexts = 0 }

func (b *scriptedBackOff) NextBackOff() time.Duration {
	if b.nexts >= len(b.seq) {
		return b.seq[len(b.seq)-1]
	}
	d := b.seq[b.nexts]
	b.nexts++
	return d
}

var (
	errConn = errors.New("connection refused")
	errRate = errors.New("rate limited")
)

func testClassifier(err error) string {
	switch {
	case errors.Is(err, errConn):
		return "conn"
	case errors.Is(err, errRate):
		return "rate"
	}
	return "other"
}

// testFactory gives connection errors a 10/20/40ms sequence and rate-limit
// errors a 100ms sequence; any other category is not retried (nil policy).
func testFactory(category string) BackOff {
	switch category {
	case "conn":
		return &scriptedBackOff{seq: []time.Duration{10, 20, 40}}
	case "rate":
		return &scriptedBackOff{seq: []time.Duration{100}}
	}
	return nil
}

// failSequence returns an operation that fails with each of errs in turn,
// then succeeds.
func failSequence(errs ...error) func() (int, error) {
	i := 0
	return func() (int, error) {
		if i < len(errs) {
			err := errs[i]
			i++
			return 0, err
		}
		return 1, nil
	}
}

func equalDurations(got, want []time.Duration) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The error sequence A, A, B, A proves both category-switch recovery rules by
// the recorded wait durations, not just the attempt count: the two rules
// produce the same number of attempts but a different last wait.
func TestClassifiedBackOffSwitchRules(t *testing.T) {
	run := func(opts ...ClassifiedOption) ([]time.Duration, error) {
		tm := &spyTimer{}
		_, err := Retry(context.Background(),
			failSequence(errConn, errConn, errRate, errConn),
			WithClassifiedBackOff(testClassifier, testFactory, opts...),
			withTimer(tm),
		)
		return tm.starts, err
	}

	t.Run("keep: returning to a category continues its sequence", func(t *testing.T) {
		waits, err := run()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// conn: 10, 20 — rate: 100 — conn resumes at its third delay: 40.
		want := []time.Duration{10, 20, 100, 40}
		if !equalDurations(waits, want) {
			t.Errorf("waits = %v, want %v", waits, want)
		}
	})

	t.Run("reset on switch: returning to a category restarts its sequence", func(t *testing.T) {
		waits, err := run(WithResetOnSwitch(true))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// conn: 10, 20 — rate: 100 — conn was reset on switch, restarts: 10.
		want := []time.Duration{10, 20, 100, 10}
		if !equalDurations(waits, want) {
			t.Errorf("waits = %v, want %v", waits, want)
		}
	})
}

func TestClassifiedBackOffNilPolicyStops(t *testing.T) {
	// A nil policy marks the category non-retriable: Retry stops with
	// ErrExhausted and the original error chain is preserved.
	other := errors.New("mystery failure")
	tm := &spyTimer{}
	calls := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			return 0, other
		},
		WithClassifiedBackOff(testClassifier, testFactory),
		withTimer(tm),
	)
	if !errors.Is(err, ErrExhausted) {
		t.Errorf("Cause = %v, want ErrExhausted", err)
	}
	if !errors.Is(err, other) {
		t.Errorf("operation error not in result: %v", err)
	}
	if re := AsRetryError(err); re == nil || re.LastErr != other {
		t.Errorf("LastErr = %v, want %v", re, other)
	}
	if calls != 1 || len(tm.starts) != 0 {
		t.Errorf("calls = %d, waits = %v; want one call and no wait", calls, tm.starts)
	}
}

func TestClassifiedBackOffPermanentPrecedesClassification(t *testing.T) {
	// A permanent error stops Retry before the classifier is consulted.
	classifierCalls := 0
	classifier := func(err error) string {
		classifierCalls++
		return testClassifier(err)
	}

	_, err := Retry(context.Background(),
		func() (int, error) { return 0, Permanent(errConn) },
		WithClassifiedBackOff(classifier, testFactory),
		withTimer(&testTimer{}),
	)
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("Cause = %v, want ErrPermanent", err)
	}
	if !errors.Is(err, errConn) {
		t.Errorf("operation error not in result: %v", err)
	}
	if classifierCalls != 0 {
		t.Errorf("classifier called %d times on a permanent error, want 0", classifierCalls)
	}
}

func TestClassifiedBackOffRetryAfterOverridesAndResetsCurrent(t *testing.T) {
	// Priority: the RetryAfterError delay overrides the routed policy's delay
	// and resets only the current category's sequence.
	cause := errConn
	tm := &spyTimer{}
	i := 0

	_, err := Retry(context.Background(),
		func() (int, error) {
			i++
			switch i {
			case 1:
				return 0, errConn // conn policy: 10
			case 2:
				return 0, RetryAfter(5*time.Second, cause) // overrides with 5s, resets conn
			case 3:
				return 0, errConn // conn restarts: 10
			}
			return 1, nil
		},
		WithClassifiedBackOff(testClassifier, testFactory),
		withTimer(tm),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []time.Duration{10, 5 * time.Second, 10}
	if !equalDurations(tm.starts, want) {
		t.Errorf("waits = %v, want %v", tm.starts, want)
	}
}

func TestClassifiedBackOffConcurrentCallsShareNothing(t *testing.T) {
	// Two concurrent Retry calls share the classifier and factory but must
	// each get their own policy instances: both see the full conn sequence
	// 10, 20 instead of interleaving into one shared sequence.
	var mu sync.Mutex
	created := 0
	bothCreated := make(chan struct{})
	var once sync.Once

	factory := func(category string) BackOff {
		policy := testFactory(category)
		mu.Lock()
		created++
		if created == 2 {
			once.Do(func() { close(bothCreated) })
		}
		mu.Unlock()
		<-bothCreated // hold both calls in-flight together
		return policy
	}

	var wg sync.WaitGroup
	waits := make([][]time.Duration, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm := &spyTimer{}
			_, errs[i] = Retry(context.Background(),
				failSequence(errConn, errConn),
				WithClassifiedBackOff(testClassifier, factory),
				withTimer(tm),
			)
			waits[i] = tm.starts
		}()
	}
	wg.Wait()

	want := []time.Duration{10, 20}
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("call %d: unexpected error: %v", i, errs[i])
		}
		if !equalDurations(waits[i], want) {
			t.Errorf("call %d: waits = %v, want %v (each call owns its sequence)", i, waits[i], want)
		}
	}
}
