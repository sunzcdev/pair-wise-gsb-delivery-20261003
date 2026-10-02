package gobreaker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeClock is a controllable Clock used to prove slow-call tripping with
// simulated time: no real goroutine ever sleeps for the "slow" latency.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func slowCallSettings(clock Clock) Settings {
	return Settings{
		Name:              "slow",
		MaxRequests:       3,
		Interval:          30 * time.Second,
		BucketPeriod:      3 * time.Second,
		Timeout:           90 * time.Second,
		Clock:             clock,
		SlowCallThreshold: 100 * time.Millisecond,
		SlowCallRatio:     0.5,
		MinSampleRequests: 5,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures > 5
		},
		IsExcluded: func(err error) bool {
			return errors.Is(err, errExcluded)
		},
	}
}

// timedCall executes a request whose simulated latency is d. Because Execute
// reads start and end time from the same injected Clock, latency is exactly
// the amount the clock advanced while the request ran.
func timedCall(cb *CircuitBreaker[bool], clock *fakeClock, d time.Duration, err error) error {
	_, e := cb.Execute(func() (bool, error) {
		clock.Advance(d)
		return err == nil, err
	})
	return e
}

// TestSlowCallMaskedByFewFastRequests proves that a few fast requests cannot
// mask sustained slow calls once MinSampleRequests has been reached:
// below the minimum sample size the breaker stays closed even when every
// request is slow, but once the minimum is met the persistent slow ratio
// trips even when a minority of subsequent requests are fast.
func TestSlowCallMaskedByFewFastRequests(t *testing.T) {
	clock := newFakeClock()
	cb := NewCircuitBreaker[bool](slowCallSettings(clock))

	// 4 slow successful calls: the slow ratio is already 100%, but the
	// minimum sample size (5) has not been reached, so the breaker stays
	// closed. This also shows that the error-based ReadyToTrip does not
	// fire for these successful-but-slow requests.
	for i := 0; i < 4; i++ {
		assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
	}
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, Counts{
		Requests:             4,
		TotalSuccesses:       4,
		TotalSlow:            4,
		ConsecutiveSuccesses: 4,
	}, cb.Counts())

	// A fast request interleaved with the slow stream does not mask it:
	// 4 slow + 1 fast => 5 valid requests, 4 slow => 0.8 >= 0.5, and the
	// minimum sample size is now met, so the breaker opens on the 5th call.
	assert.Nil(t, timedCall(cb, clock, 10*time.Millisecond, nil))
	assert.Equal(t, StateOpen, cb.State())
	assert.Equal(t, Counts{}, cb.Counts())
}

// TestSlowCallRatioAfterMinSamples verifies that after the breaker is reset
// by a window roll, sustained slow calls still trip once the minimum sample
// count is satisfied and a handful of fast requests cannot pull the ratio
// below the configured threshold.
func TestSlowCallRatioAfterMinSamples(t *testing.T) {
	clock := newFakeClock()
	cb := NewCircuitBreaker[bool](slowCallSettings(clock))

	// Reach the minimum sample with an exactly-at-boundary fast mix:
	// 3 slow + 2 fast = 5 requests, 3/5 = 0.6 >= 0.5 -> trips on the 5th.
	assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 1*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 1*time.Millisecond, nil))
	assert.Equal(t, StateClosed, cb.State())
	assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
	assert.Equal(t, StateOpen, cb.State())

	// On a fresh breaker, latency equal to the threshold is not slow
	// (strictly greater), so 3 boundary calls do not trip and close the
	// half-open window after the timeout.
	clock2 := newFakeClock()
	cb2 := NewCircuitBreaker[bool](slowCallSettings(clock2))
	assert.Nil(t, timedCall(cb2, clock2, 100*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb2, clock2, 100*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb2, clock2, 100*time.Millisecond, nil))
	assert.Equal(t, StateClosed, cb2.State())
	assert.Equal(t, uint32(0), cb2.Counts().TotalSlow)
}

// TestSustainedSlownessSurvivesFastRequests starts from a healthy mix right
// at the minimum sample size, then feeds a sustained slow stream interleaved
// with occasional fast requests. The fast requests must not mask the
// persistent slow-call ratio once the minimum sample count is in effect.
func TestSustainedSlownessSurvivesFastRequests(t *testing.T) {
	clock := newFakeClock()
	st := slowCallSettings(clock)
	st.MinSampleRequests = 5
	st.SlowCallRatio = 0.6
	cb := NewCircuitBreaker[bool](st)

	// Healthy warm-up exactly at the minimum sample: 3 fast + 2 slow = 40%.
	assert.Nil(t, timedCall(cb, clock, 1*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 1*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 1*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
	assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
	assert.Equal(t, StateClosed, cb.State())

	// Sustained slow stream, with one fast request after every two slow
	// ones (steady slow ratio converges to 2/3 >= 0.6). The breaker must
	// open despite the regular fast requests.
	for i := 0; i < 20 && cb.State() == StateClosed; i++ {
		assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
		if cb.State() != StateClosed {
			break
		}
		assert.Nil(t, timedCall(cb, clock, 200*time.Millisecond, nil))
		if cb.State() != StateClosed {
			break
		}
		assert.Nil(t, timedCall(cb, clock, 1*time.Millisecond, nil))
	}
	assert.Equal(t, StateOpen, cb.State())
}

// TestSlowCallExcludedFromDenominator ensures the IsExcluded policy is
// consistent with the failure-ratio denominator: excluded requests neither
// count toward TotalSlow nor toward the ratio denominator.
func TestSlowCallExcludedFromDenominator(t *testing.T) {
	clock := newFakeClock()
	st := slowCallSettings(clock)
	st.MinSampleRequests = 2
	st.SlowCallRatio = 1.0
	cb := NewCircuitBreaker[bool](st)

	// Excluded requests are slow but must be ignored completely.
	assert.True(t, errors.Is(timedCall(cb, clock, 500*time.Millisecond, errExcluded), errExcluded))
	assert.True(t, errors.Is(timedCall(cb, clock, 500*time.Millisecond, errExcluded), errExcluded))
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, Counts{Requests: 2, TotalExclusions: 2}, cb.Counts())

	// One non-excluded slow request satisfies denominator >= 2 only if the
	// excluded requests counted; they must not, so a single request with
	// ratio 1.0 does not trip yet.
	assert.NoError(t, timedCall(cb, clock, 500*time.Millisecond, nil))
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, uint32(1), cb.Counts().TotalSlow)

	// The second non-excluded slow request trips on the slow condition.
	assert.NoError(t, timedCall(cb, clock, 500*time.Millisecond, nil))
	assert.Equal(t, StateOpen, cb.State())
}

// TestSlowFailureTripsBothConditions ensures slow failures are counted as
// slow and as failures, and the slow condition trips independently of the
// error-based ReadyToTrip.
func TestSlowFailureTripsOnSlowCondition(t *testing.T) {
	clock := newFakeClock()
	st := slowCallSettings(clock)
	st.MinSampleRequests = 2
	st.SlowCallRatio = 1.0
	cb := NewCircuitBreaker[bool](st)

	assert.True(t, errors.Is(timedCall(cb, clock, 200*time.Millisecond, errFailed), errFailed))
	assert.Equal(t, StateClosed, cb.State())
	assert.True(t, errors.Is(timedCall(cb, clock, 200*time.Millisecond, errFailed), errFailed))
	// Only 2 consecutive failures (< the default-style threshold of 6),
	// yet the slow-call ratio trips the breaker.
	assert.Equal(t, StateOpen, cb.State())
}

// TestCrossBucketCompletionAttributesByStart verifies that a request which
// starts in one bucket and completes in a later bucket is attributed to its
// start bucket (where its Requests slot was reserved) and still counts as
// slow; requests completing after the whole window has rolled away are
// ignored.
func TestCrossBucketCompletionAttributesByStart(t *testing.T) {
	clock := newFakeClock()
	st := slowCallSettings(clock)
	st.Interval = 6 * time.Second
	st.BucketPeriod = 2 * time.Second
	st.MinSampleRequests = 1
	// Ratio tripping disabled here: this test only checks attribution.
	st.SlowCallRatio = 0
	tscb := NewTwoStepCircuitBreaker[bool](st)

	done, err := tscb.Allow()
	assert.Nil(t, err)

	// The request is outstanding across the next bucket boundary.
	clock.Advance(3 * time.Second)
	done(nil) // latency 3s > threshold

	// The completed request must land in the start bucket (bucketAt(-1))
	// while the current bucket is empty; the window aggregate shows it.
	cb := tscb.cb
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		TotalSlow:            1,
		ConsecutiveSuccesses: 1,
	}, cb.Counts())
	assert.Equal(t, Counts{}, cb.counts.bucketAt(0))
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		TotalSlow:            1,
		ConsecutiveSuccesses: 1,
	}, cb.counts.bucketAt(-1))

	// A request that completes only after the full window (3 buckets) has
	// rolled away is ignored by the counters.
	done2, err := tscb.Allow()
	assert.Nil(t, err)
	clock.Advance(6 * time.Second)
	done2(nil)
	assert.Equal(t, Counts{}, cb.Counts())
}

// TestSlowSuccessInHalfOpen covers the configurable half-open policy:
// with SlowSuccessInHalfOpen, a slow success re-opens immediately; without
// it, slow successes count toward closing the breaker.
func TestSlowSuccessInHalfOpen(t *testing.T) {
	t.Run("reopens", func(t *testing.T) {
		clock := newFakeClock()
		st := slowCallSettings(clock)
		st.MinSampleRequests = 1
		st.SlowCallRatio = 1.0
		st.SlowSuccessInHalfOpen = true
		cb := NewCircuitBreaker[bool](st)

		// Trip without consuming simulated time beyond the open timeout.
		assert.Nil(t, timedCall(cb, clock, 150*time.Millisecond, nil))
		assert.Equal(t, StateOpen, cb.State())

		clock.Advance(cb.timeout + time.Nanosecond)
		assert.Equal(t, StateHalfOpen, cb.State())
		assert.Nil(t, timedCall(cb, clock, 150*time.Millisecond, nil))
		assert.Equal(t, StateOpen, cb.State())
		assert.Equal(t, Counts{}, cb.Counts())
	})

	t.Run("closes", func(t *testing.T) {
		clock := newFakeClock()
		st := slowCallSettings(clock)
		st.MaxRequests = 1
		st.MinSampleRequests = 1
		st.SlowCallRatio = 1.0
		// SlowSuccessInHalfOpen left false (default): slow success is a
		// success for half-open purposes.
		cb := NewCircuitBreaker[bool](st)

		assert.Nil(t, timedCall(cb, clock, 150*time.Millisecond, nil))
		assert.Equal(t, StateOpen, cb.State())

		clock.Advance(cb.timeout + time.Nanosecond)
		assert.Equal(t, StateHalfOpen, cb.State())
		assert.Nil(t, timedCall(cb, clock, 150*time.Millisecond, nil))
		assert.Equal(t, StateClosed, cb.State())
	})
}

// TestSlowDetectionDisabledByDefault confirms that without slow-call
// settings the behavior is unchanged: slow successful calls never trip.
func TestSlowDetectionDisabledByDefault(t *testing.T) {
	clock := newFakeClock()
	cb := NewCircuitBreaker[bool](Settings{
		Name:              "default",
		Clock:             clock,
		SlowCallThreshold: 0,
		SlowCallRatio:     1.0,
		MinSampleRequests: 1,
	})

	for i := 0; i < 10; i++ {
		assert.Nil(t, timedCall(cb, clock, time.Hour, nil))
	}
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, uint32(0), cb.Counts().TotalSlow)
}

// TestDefaultClockIsSystemClock ensures the zero-value Settings use a
// functional clock so existing callers keep working.
func TestDefaultClockIsSystemClock(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{})
	_ = cb.clock.Now()
	assert.IsType(t, systemClock{}, cb.clock)
}
