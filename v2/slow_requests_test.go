package gobreaker

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeClock is a manually advanced clock. newBreaker seeds the breaker's start
// time and expiry at the current fake instant, so each breaker begins at a
// fixed, deterministic point in time.
type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock {
	// time.Now carries a monotonic reading, so Sub between advanced instants
	// measures elapsed time exactly like the production monotonic clock.
	return &fakeClock{t: time.Now()}
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func (c *fakeClock) newBreaker(st Settings) *CircuitBreaker[bool] {
	cb := NewCircuitBreaker[bool](st)
	cb.now = c.now
	// Re-initialise the current generation at the fixed fake instant so the
	// first Execute does not observe a "newer" clock and roll/clear counts.
	cb.toNewGeneration(c.t)
	return cb
}

// runWith admits a request at the current instant, advances the clock by
// delay (modelling the request's elapsed time), and then reports its result.
// This mirrors how Execute samples the clock at admission and again at
// completion, so latency is measured across the advanced interval.
func (c *fakeClock) runWith(cb *CircuitBreaker[bool], delay time.Duration, fn func() (bool, error)) error {
	// Admit at the current instant before advancing so the completion clock is
	// later by exactly delay and the request attributes to the current bucket.
	generation, age, startedAt, err := cb.beforeRequest()
	if err != nil {
		return err
	}
	c.advance(delay)
	_, reqErr := fn()
	cb.afterRequest(generation, age, startedAt, reqErr)
	return reqErr
}

func (c *fakeClock) fast(cb *CircuitBreaker[bool]) error {
	return c.runWith(cb, time.Millisecond, func() (bool, error) { return true, nil })
}

func (c *fakeClock) slow(cb *CircuitBreaker[bool]) error {
	return c.runWith(cb, 100*time.Millisecond, func() (bool, error) { return true, nil })
}

func slowSettings() Settings {
	return Settings{
		Name:        "slow",
		MaxRequests: 3,
		// A single wide fixed window so advancing the fake clock does not roll
		// generations while a scenario runs.
		Interval:             time.Hour,
		Timeout:              500 * time.Millisecond,
		SlowRequestThreshold: 50 * time.Millisecond,
		SlowRequestRatio:     0.5,
		MinSlowRequests:      5,
		ReadyToTrip: func(counts Counts) bool {
			numReqs := counts.validRequests()
			return numReqs >= 5 && float64(counts.TotalFailures)/float64(numReqs) >= 0.6
		},
		IsExcluded: func(err error) bool { return errors.Is(err, errExcluded) },
	}
}

// A few fast requests must not mask persistent slow calls once the minimum
// sample count is reached. The window starts with 6 fast + 1 slow requests
// (slow ratio ~0.14); as slow calls accumulate the ratio climbs until it trips
// at 8 slow / 14 total, with the 6 fast requests still in the window.
func TestSlowRequestsTripDespiteFastRequests(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	// Use 0.6 so the trip point is an exact integer ratio: 60% slow.
	st.SlowRequestRatio = 0.6
	cb := clock.newBreaker(st)

	// First 5 requests (at the sample floor): 4 fast + 1 slow = 20% slow.
	for i := 0; i < 4; i++ {
		assert.NoError(t, clock.fast(cb))
	}
	assert.NoError(t, clock.slow(cb))
	assert.Equal(t, StateClosed, cb.State())

	// Persistent slow calls accumulate: 4 slow / 8 total = 50% < 60% stays
	// closed even though slow calls now dominate.
	for i := 0; i < 3; i++ {
		assert.NoError(t, clock.slow(cb))
	}
	assert.Equal(t, StateClosed, cb.State())

	// The 9th request brings slow calls to 5/9, still under 60%...
	assert.NoError(t, clock.slow(cb))
	assert.Equal(t, StateClosed, cb.State())

	// Capture the just-below-threshold counts.
	beforeTrip := cb.Counts()
	assert.Equal(t, uint32(9), beforeTrip.Requests)
	assert.Equal(t, uint32(5), beforeTrip.TotalSlowRequests)

	// The 10th request reaches 6 slow / 10 total = 60% and trips, despite the
	// 4 fast requests in the same window.
	assert.NoError(t, clock.slow(cb))
	assert.Equal(t, StateOpen, cb.State())
}

// Below the minimum sample count the same slow ratio must not trip.
func TestSlowRequestsBelowMinSamplesStaysClosed(t *testing.T) {
	clock := newFakeClock()
	cb := clock.newBreaker(slowSettings())

	for i := 0; i < 4; i++ {
		assert.NoError(t, clock.slow(cb))
	}
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, uint32(4), cb.Counts().TotalSlowRequests)
}

// MinSlowRequests defaults to 5 when left unset.
func TestSlowRequestsDefaultMinSamples(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	st.MinSlowRequests = 0
	st.SlowRequestRatio = 1
	cb := clock.newBreaker(st)

	for i := 0; i < 4; i++ {
		assert.NoError(t, clock.slow(cb))
	}
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, uint32(5), cb.minSlowRequests)

	assert.NoError(t, clock.slow(cb))
	assert.Equal(t, StateOpen, cb.State())
}

// Failed requests count towards errors but are never double counted as slow.
func TestSlowRequestsOnlyCountSuccessfulCalls(t *testing.T) {
	clock := newFakeClock()
	cb := clock.newBreaker(slowSettings())

	assert.ErrorIs(t, clock.runWith(cb, 100*time.Millisecond, func() (bool, error) {
		return false, errFailed
	}), errFailed)
	assert.Equal(t, StateClosed, cb.State())
	counts := cb.Counts()
	assert.Equal(t, uint32(1), counts.TotalFailures)
	assert.Equal(t, uint32(0), counts.TotalSlowRequests)
}

// Excluded requests are excluded from the slow-ratio denominator just as they
// are from the error ratio, so they neither dilute nor inflate the ratio.
func TestSlowRequestsExcludedFromDenominator(t *testing.T) {
	clock := newFakeClock()
	cb := clock.newBreaker(slowSettings())

	for i := 0; i < 4; i++ {
		assert.NoError(t, clock.slow(cb))
	}
	for i := 0; i < 10; i++ {
		assert.ErrorIs(t, clock.runWith(cb, 100*time.Millisecond, func() (bool, error) {
			return false, errExcluded
		}), errExcluded)
	}
	assert.Equal(t, StateClosed, cb.State())
	counts := cb.Counts()
	assert.Equal(t, uint32(14), counts.Requests)
	assert.Equal(t, uint32(10), counts.TotalExclusions)
	assert.Equal(t, uint32(4), counts.validRequests())
	assert.Equal(t, uint32(4), counts.TotalSlowRequests)

	// One more valid slow request reaches 5 valid requests and trips.
	assert.NoError(t, clock.slow(cb))
	assert.Equal(t, StateOpen, cb.State())
}

// A request that starts in one bucket and completes in a later bucket is
// attributed to its start bucket (not the completion bucket) and still counted
// while that bucket remains inside the rolling window.
func TestSlowRequestsAttributedToStartBucketAcrossBuckets(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	st.Interval = 10 * time.Second
	st.BucketPeriod = time.Second
	st.MinSlowRequests = 1
	st.SlowRequestRatio = 0 // keep slow detection but disable ratio tripping
	cb := clock.newBreaker(st)

	clock.advance(1500 * time.Millisecond)
	generation, age, startedAt, err := cb.beforeRequest()
	assert.NoError(t, err)
	startAge := age
	clock.advance(1500 * time.Millisecond)
	cb.afterRequest(generation, age, startedAt, nil)

	counts := cb.Counts()
	assert.Equal(t, uint32(1), counts.Requests)
	assert.Equal(t, uint32(1), counts.TotalSuccesses)
	assert.Equal(t, uint32(1), counts.TotalSlowRequests)

	// The slow result landed in the start bucket, not the completion bucket.
	startBucket := cb.counts.buckets[cb.counts.index(startAge)]
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		TotalSlowRequests:    1,
		ConsecutiveSuccesses: 1,
	}, startBucket)

	// The completion bucket (one past the start bucket) stays empty, proving
	// attribution is by start time rather than completion time.
	completionBucket := cb.counts.buckets[cb.counts.index(startAge+1)]
	assert.Equal(t, Counts{}, completionBucket)
}

// Requests completing after their start bucket left the rolling window are
// discarded from the aggregated counts.
func TestSlowRequestsCompletingOutsideWindowAreDiscarded(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	st.Interval = 2 * time.Second
	st.BucketPeriod = time.Second
	st.MinSlowRequests = 1
	cb := clock.newBreaker(st)

	clock.advance(1500 * time.Millisecond)
	generation, age, startedAt, err := cb.beforeRequest()
	assert.NoError(t, err)
	// Complete after the start bucket (bucket 0) has scrolled out of the
	// 2-bucket window.
	clock.advance(3 * time.Second)
	cb.afterRequest(generation, age, startedAt, nil)

	assert.Equal(t, uint32(0), cb.Counts().TotalSuccesses)
	assert.Equal(t, uint32(0), cb.Counts().TotalSlowRequests)
	assert.Equal(t, StateClosed, cb.State())
}

// By default a slow success in half-open still counts towards closing.
func TestSlowSuccessHalfOpenCountedAsSuccessByDefault(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	cb := clock.newBreaker(st)

	cb.state = StateOpen
	cb.setState(StateHalfOpen, clock.now())
	assert.Equal(t, StateHalfOpen, cb.State())

	// maxRequests is 3: three slow successes close the breaker by default.
	for i := 0; i < 3; i++ {
		assert.NoError(t, clock.runWith(cb, 100*time.Millisecond, func() (bool, error) {
			return true, nil
		}))
	}
	assert.Equal(t, StateClosed, cb.State())
}

// With TreatSlowCallsAsFailuresWhenHalfOpen a slow success reopens at once.
func TestSlowSuccessHalfOpenReopensWhenConfigured(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	st.TreatSlowCallsAsFailuresWhenHalfOpen = true
	cb := clock.newBreaker(st)

	cb.state = StateOpen
	cb.setState(StateHalfOpen, clock.now())
	assert.Equal(t, StateHalfOpen, cb.State())

	assert.NoError(t, clock.runWith(cb, 100*time.Millisecond, func() (bool, error) {
		return true, nil
	}))
	assert.Equal(t, StateOpen, cb.State())
}

// A request whose duration equals the threshold counts as slow.
func TestSlowRequestThresholdBoundary(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	st.MinSlowRequests = 1
	st.SlowRequestRatio = 1
	cb := clock.newBreaker(st)

	assert.NoError(t, clock.runWith(cb, st.SlowRequestThreshold, func() (bool, error) {
		return true, nil
	}))
	assert.Equal(t, StateOpen, cb.State())
}

// The two-step Allow/done API measures latency from Allow to the done callback.
func TestSlowRequestsTwoStep(t *testing.T) {
	clock := newFakeClock()
	st := slowSettings()
	st.MinSlowRequests = 1
	st.SlowRequestRatio = 1
	tscb := NewTwoStepCircuitBreaker[bool](st)
	tscb.cb.now = clock.now
	tscb.cb.toNewGeneration(clock.now())

	done, err := tscb.Allow()
	assert.NoError(t, err)
	clock.advance(st.SlowRequestThreshold)
	done(nil)

	assert.Equal(t, StateOpen, tscb.State())
}
