package backoff

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func newTestExponentialBackOff() *ExponentialBackOff {
	exp := NewExponentialBackOff()
	exp.InitialInterval = 500 * time.Millisecond
	exp.RandomizationFactor = 0.5
	exp.Multiplier = 1.5
	exp.MaxInterval = 5 * time.Second
	exp.Reset()
	return exp
}

// A snapshot taken after the Nth failure must let a restored backoff produce
// exactly the same subsequent intervals as the original, randomization included.
func TestSnapshotRestoreSameSequence(t *testing.T) {
	const failuresBeforeSnapshot = 3
	const intervalsAfter = 6

	exp := newTestExponentialBackOff()
	for range failuresBeforeSnapshot {
		exp.NextBackOff()
	}
	snapshot := exp.Snapshot()

	continued := make([]time.Duration, intervalsAfter)
	for i := range continued {
		continued[i] = exp.NextBackOff()
	}

	restored, err := RestoreExponentialBackOff(snapshot)
	if err != nil {
		t.Fatalf("RestoreExponentialBackOff() error: %v", err)
	}
	for i, want := range continued {
		if got := restored.NextBackOff(); got != want {
			t.Fatalf("interval %d after restore = %v, want %v", i, got, want)
		}
	}
}

// The snapshot must survive a serialization round trip.
func TestSnapshotRestoreJSONRoundTrip(t *testing.T) {
	exp := newTestExponentialBackOff()
	exp.NextBackOff()
	exp.NextBackOff()

	data, err := json.Marshal(exp.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var state ExponentialBackOffState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	restored, err := RestoreExponentialBackOff(state)
	if err != nil {
		t.Fatalf("RestoreExponentialBackOff() error: %v", err)
	}
	for range 5 {
		want := exp.NextBackOff()
		if got := restored.NextBackOff(); got != want {
			t.Fatalf("restored interval = %v, want %v", got, want)
		}
	}
}

// A snapshot taken mid-Retry (after the 3rd failure) must let a second,
// restored Retry continue with the same backoff intervals — and only when
// WithBackOffReset(false) keeps Retry from resetting the restored state.
func TestRetryResumeFromSnapshot(t *testing.T) {
	const snapshotAfterFailure = 3

	alwaysFail := func() (bool, error) { return false, errors.New("error") }

	// First run: snapshot after the 3rd failure, keep going, record all intervals.
	exp := newTestExponentialBackOff()
	var snapshot ExponentialBackOffState
	var firstRun []time.Duration
	failures := 0
	_, err := Retry(context.Background(), alwaysFail,
		WithBackOff(exp),
		withTimer(&testTimer{}),
		WithMaxTries(8),
		WithNotify(func(_ error, d time.Duration) {
			failures++
			firstRun = append(firstRun, d)
			if failures == snapshotAfterFailure {
				snapshot = exp.Snapshot()
			}
		}),
	)
	if err == nil {
		t.Fatal("first Retry() unexpectedly succeeded")
	}

	restored, err := RestoreExponentialBackOff(snapshot)
	if err != nil {
		t.Fatalf("RestoreExponentialBackOff() error: %v", err)
	}

	// Second run: resume from the restored state.
	var resumed []time.Duration
	_, err = Retry(context.Background(), alwaysFail,
		WithBackOff(restored),
		WithBackOffReset(false),
		withTimer(&testTimer{}),
		WithMaxTries(5),
		WithNotify(func(_ error, d time.Duration) { resumed = append(resumed, d) }),
	)
	if err == nil {
		t.Fatal("resumed Retry() unexpectedly succeeded")
	}

	want := firstRun[snapshotAfterFailure:]
	if len(resumed) != len(want) {
		t.Fatalf("resumed %d intervals, want %d", len(resumed), len(want))
	}
	for i := range want {
		if resumed[i] != want[i] {
			t.Fatalf("resumed interval %d = %v, want %v", i, resumed[i], want[i])
		}
	}
}

// Without WithBackOffReset(false), Retry resets a restored policy back to the
// initial interval, discarding the snapshot position.
func TestRetryResetsRestoredBackOffByDefault(t *testing.T) {
	exp := newTestExponentialBackOff()
	exp.RandomizationFactor = 0 // deterministic intervals
	for range 4 {
		exp.NextBackOff()
	}
	if exp.currentInterval == exp.InitialInterval {
		t.Fatal("test precondition failed: backoff did not advance")
	}

	restored, err := RestoreExponentialBackOff(exp.Snapshot())
	if err != nil {
		t.Fatalf("RestoreExponentialBackOff() error: %v", err)
	}

	var firstInterval time.Duration
	alwaysFail := func() (bool, error) { return false, errors.New("error") }
	_, err = Retry(context.Background(), alwaysFail,
		WithBackOff(restored),
		withTimer(&testTimer{}),
		WithMaxTries(2),
		WithNotify(func(_ error, d time.Duration) { firstInterval = d }),
	)
	if err == nil {
		t.Fatal("Retry() unexpectedly succeeded")
	}
	if firstInterval != exp.InitialInterval {
		t.Fatalf("first interval after default reset = %v, want initial %v", firstInterval, exp.InitialInterval)
	}
}

func TestRestoreExponentialBackOffInvalidState(t *testing.T) {
	valid := newTestExponentialBackOff().Snapshot()

	cases := map[string]func(ExponentialBackOffState) ExponentialBackOffState{
		"unsupported version": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.Version = ExponentialBackOffStateVersion + 1
			return s
		},
		"zero initial interval": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.InitialInterval = 0
			return s
		},
		"negative max interval": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.MaxInterval = -time.Second
			return s
		},
		"zero current interval": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.CurrentInterval = 0
			return s
		},
		"negative randomization factor": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.RandomizationFactor = -0.1
			return s
		},
		"randomization factor above one": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.RandomizationFactor = 1.1
			return s
		},
		"nan randomization factor": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.RandomizationFactor = math.NaN()
			return s
		},
		"multiplier below one": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.Multiplier = 0.5
			return s
		},
		"nan multiplier": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.Multiplier = math.NaN()
			return s
		},
		"infinite multiplier": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.Multiplier = math.Inf(1)
			return s
		},
		"corrupt random state": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.RandState = []byte{1, 2, 3}
			return s
		},
		"missing random state": func(s ExponentialBackOffState) ExponentialBackOffState {
			s.RandState = nil
			return s
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := RestoreExponentialBackOff(mutate(valid)); err == nil {
				t.Fatal("RestoreExponentialBackOff() succeeded, want error")
			}
		})
	}
}
