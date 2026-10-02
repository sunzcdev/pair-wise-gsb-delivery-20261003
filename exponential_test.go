package backoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func TestBackOff(t *testing.T) {
	var (
		testInitialInterval     = 500 * time.Millisecond
		testRandomizationFactor = 0.1
		testMultiplier          = 2.0
		testMaxInterval         = 5 * time.Second
	)

	exp := NewExponentialBackOff()
	exp.InitialInterval = testInitialInterval
	exp.RandomizationFactor = testRandomizationFactor
	exp.Multiplier = testMultiplier
	exp.MaxInterval = testMaxInterval
	exp.Reset()

	var expectedResults = []time.Duration{500, 1000, 2000, 4000, 5000, 5000, 5000, 5000, 5000, 5000}
	for i, d := range expectedResults {
		expectedResults[i] = d * time.Millisecond
	}

	for _, expected := range expectedResults {
		assertEquals(t, expected, exp.currentInterval)
		// Assert that the next backoff falls in the expected range.
		var minInterval = expected - time.Duration(testRandomizationFactor*float64(expected))
		var maxInterval = expected + time.Duration(testRandomizationFactor*float64(expected))
		var actualInterval = exp.NextBackOff()
		if !(minInterval <= actualInterval && actualInterval <= maxInterval) {
			t.Error("error")
		}
	}
}

func TestGetRandomizedInterval(t *testing.T) {
	// 33% chance of being 1.
	assertEquals(t, 1, getRandomValueFromInterval(0.5, 0, 2))
	assertEquals(t, 1, getRandomValueFromInterval(0.5, 0.33, 2))
	// 33% chance of being 2.
	assertEquals(t, 2, getRandomValueFromInterval(0.5, 0.34, 2))
	assertEquals(t, 2, getRandomValueFromInterval(0.5, 0.66, 2))
	// 33% chance of being 3.
	assertEquals(t, 3, getRandomValueFromInterval(0.5, 0.67, 2))
	assertEquals(t, 3, getRandomValueFromInterval(0.5, 0.99, 2))
}

func TestBackOffOverflow(t *testing.T) {
	var (
		testInitialInterval time.Duration = math.MaxInt64 / 2
		testMaxInterval     time.Duration = math.MaxInt64
		testMultiplier                    = 2.1
	)

	exp := NewExponentialBackOff()
	exp.InitialInterval = testInitialInterval
	exp.Multiplier = testMultiplier
	exp.MaxInterval = testMaxInterval
	exp.Reset()

	exp.NextBackOff()
	// Assert that when an overflow is possible, the current interval time.Duration is set to the max interval time.Duration.
	assertEquals(t, testMaxInterval, exp.currentInterval)
}

func TestBackOffRandomizedOverflow(t *testing.T) {
	exp := NewExponentialBackOff()
	exp.InitialInterval = math.MaxInt64
	exp.MaxInterval = math.MaxInt64
	exp.RandomizationFactor = 0.5
	exp.Reset()

	for range 100 {
		if next := exp.NextBackOff(); next < 0 {
			t.Fatalf("NextBackOff() = %d, want non-negative", next)
		}
	}
}

func assertEquals(t *testing.T, expected, value time.Duration) {
	if expected != value {
		t.Errorf("got: %d, expected: %d", value, expected)
	}
}

func TestSnapshotRestoreContinuesSequence(t *testing.T) {
	newBackOff := func() *ExponentialBackOff {
		b := NewExponentialBackOff()
		b.InitialInterval = 100 * time.Millisecond
		b.RandomizationFactor = 0.5
		b.Multiplier = 1.5
		b.MaxInterval = 2 * time.Second
		b.Reset()
		return b
	}

	// Pause after the third failure: snapshot mid-sequence.
	const failuresBeforeSnapshot = 3
	continuous := newBackOff()
	for range failuresBeforeSnapshot {
		continuous.NextBackOff()
	}
	snapshot := continuous.Snapshot()

	restored := NewExponentialBackOff()
	if err := restored.Restore(snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The restored policy must produce the same intervals as the uninterrupted one.
	for i := range 20 {
		want := continuous.NextBackOff()
		if got := restored.NextBackOff(); got != want {
			t.Fatalf("interval %d after restore = %v, want %v", i, got, want)
		}
	}
}

func TestSnapshotSurvivesJSONRoundTrip(t *testing.T) {
	// The state is plain data: it must survive serialization to storage.
	continuous := NewExponentialBackOff()
	continuous.Reset()
	continuous.NextBackOff()
	continuous.NextBackOff()

	data, err := json.Marshal(continuous.Snapshot())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var snapshot ExponentialBackOffState
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	restored := NewExponentialBackOff()
	if err := restored.Restore(snapshot); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for i := range 10 {
		want := continuous.NextBackOff()
		if got := restored.NextBackOff(); got != want {
			t.Fatalf("interval %d after restore = %v, want %v", i, got, want)
		}
	}
}

func TestRestoreRejectsInvalidState(t *testing.T) {
	valid := func() ExponentialBackOffState {
		b := NewExponentialBackOff()
		b.Reset()
		b.NextBackOff()
		return b.Snapshot()
	}

	cases := map[string]func(*ExponentialBackOffState){
		"unsupported version":       func(s *ExponentialBackOffState) { s.Version = ExponentialBackOffStateVersion + 1 },
		"zero initial interval":     func(s *ExponentialBackOffState) { s.InitialInterval = 0 },
		"negative initial interval": func(s *ExponentialBackOffState) { s.InitialInterval = -time.Second },
		"zero max interval":         func(s *ExponentialBackOffState) { s.MaxInterval = 0 },
		"negative current interval": func(s *ExponentialBackOffState) { s.CurrentInterval = -time.Second },
		"negative randomization":    func(s *ExponentialBackOffState) { s.RandomizationFactor = -0.1 },
		"randomization above one":   func(s *ExponentialBackOffState) { s.RandomizationFactor = 1.1 },
		"nan randomization":         func(s *ExponentialBackOffState) { s.RandomizationFactor = math.NaN() },
		"multiplier below one":      func(s *ExponentialBackOffState) { s.Multiplier = 0.5 },
		"nan multiplier":            func(s *ExponentialBackOffState) { s.Multiplier = math.NaN() },
		"infinite multiplier":       func(s *ExponentialBackOffState) { s.Multiplier = math.Inf(1) },
		"missing random state":      func(s *ExponentialBackOffState) { s.RandomState = nil },
		"corrupt random state":      func(s *ExponentialBackOffState) { s.RandomState = []byte{1, 2, 3} },
	}

	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := valid()
			corrupt(&snapshot)

			b := NewExponentialBackOff()
			before := b.Snapshot()
			err := b.Restore(snapshot)
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("Restore error = %v, want ErrInvalidState", err)
			}
			after := b.Snapshot()
			same := after.Version == before.Version &&
				after.InitialInterval == before.InitialInterval &&
				after.RandomizationFactor == before.RandomizationFactor &&
				after.Multiplier == before.Multiplier &&
				after.MaxInterval == before.MaxInterval &&
				after.CurrentInterval == before.CurrentInterval &&
				bytes.Equal(after.RandomState, before.RandomState)
			if !same {
				t.Errorf("failed Restore mutated the policy: %+v, want %+v", after, before)
			}
		})
	}
}
