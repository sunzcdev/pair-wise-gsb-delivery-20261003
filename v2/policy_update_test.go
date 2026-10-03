package gobreaker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newPolicyTestBreaker() *CircuitBreaker[bool] {
	return NewCircuitBreaker[bool](Settings{
		Name:         "policy",
		MaxRequests:  2,
		Interval:     10 * time.Second,
		BucketPeriod: 3 * time.Second,
		Timeout:      time.Second,
	})
}

func TestUpdatePolicyRejectsInvalidPolicyWithoutMutation(t *testing.T) {
	cb := newPolicyTestBreaker()
	assert.Nil(t, succeed(cb))
	assert.Nil(t, fail(cb))

	wantPolicy := Policy{
		MaxRequests:  2,
		Interval:     12 * time.Second,
		BucketPeriod: 3 * time.Second,
		Timeout:      time.Second,
	}
	wantCounts := Counts{
		Requests:            2,
		TotalSuccesses:      1,
		TotalFailures:       1,
		ConsecutiveFailures: 1,
	}
	wantGeneration := cb.generation

	invalidPolicies := []Policy{
		{MaxRequests: 2, Interval: 0, BucketPeriod: time.Second, Timeout: time.Second},
		{MaxRequests: 2, Interval: -time.Second, BucketPeriod: time.Second, Timeout: time.Second},
		{MaxRequests: 2, Interval: 10 * time.Second, BucketPeriod: -time.Second, Timeout: time.Second},
		{MaxRequests: 2, Interval: 10 * time.Second, BucketPeriod: 11 * time.Second, Timeout: time.Second},
		{MaxRequests: 2, Interval: 10 * time.Second, BucketPeriod: time.Second, Timeout: -time.Second},
	}
	for _, policy := range invalidPolicies {
		err := cb.UpdatePolicy(policy, ResetClosedCounts)
		assert.ErrorIs(t, err, ErrInvalidPolicy)
		assert.Equal(t, wantPolicy, cb.Policy())
		assert.Equal(t, wantCounts, cb.Counts())
		assert.Equal(t, StateClosed, cb.State())
		assert.Equal(t, 4, len(cb.counts.buckets))
		assert.Equal(t, wantGeneration, cb.generation)
	}

	err := cb.UpdatePolicy(wantPolicy, ClosedCountsPolicy(99))
	assert.ErrorIs(t, err, ErrInvalidPolicy)
	assert.Equal(t, wantPolicy, cb.Policy())
	assert.Equal(t, wantCounts, cb.Counts())
	assert.Equal(t, wantGeneration, cb.generation)
}

func TestUpdatePolicyNormalizesAndAppliesAtomically(t *testing.T) {
	cb := newPolicyTestBreaker()

	assert.Equal(t, Policy{
		MaxRequests:  2,
		Interval:     12 * time.Second,
		BucketPeriod: 3 * time.Second,
		Timeout:      time.Second,
	}, cb.Policy())

	err := cb.UpdatePolicy(Policy{Interval: 10 * time.Second, BucketPeriod: 3 * time.Second}, ResetClosedCounts)
	assert.Nil(t, err)
	assert.Equal(t, Policy{
		MaxRequests:  1,
		Interval:     12 * time.Second,
		BucketPeriod: 3 * time.Second,
		Timeout:      60 * time.Second,
	}, cb.Policy())
	assert.Equal(t, 4, cb.numBuckets)
	assert.Equal(t, 2, int(cb.generation))
	assert.Equal(t, Counts{}, cb.Counts())
}

func TestPolicyRepresentsCumulativeAndFixedWindows(t *testing.T) {
	cumulative := NewCircuitBreaker[bool](Settings{})
	assert.Equal(t, Policy{
		MaxRequests:  1,
		Interval:     0,
		BucketPeriod: 0,
		Timeout:      60 * time.Second,
	}, cumulative.Policy())
	assert.Equal(t, 0, cumulative.numBuckets)

	fixed := NewCircuitBreaker[bool](Settings{Interval: time.Minute})
	assert.Equal(t, Policy{
		MaxRequests:  1,
		Interval:     time.Minute,
		BucketPeriod: 0,
		Timeout:      60 * time.Second,
	}, fixed.Policy())
	assert.Equal(t, 1, fixed.numBuckets)
}

func TestUpdatePolicyPreservesCountsWhenLayoutUnchanged(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{
		MaxRequests:  2,
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	})
	tscb := NewTwoStepCircuitBreaker[bool](Settings{
		MaxRequests:  2,
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	})

	assert.Nil(t, succeed(cb))
	assert.Nil(t, succeed(tscb.cb))

	policy := Policy{
		MaxRequests:  5,
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      3 * time.Second,
	}
	err := cb.UpdatePolicy(policy, PreserveClosedCounts)
	assert.Nil(t, err)

	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		ConsecutiveSuccesses: 1,
	}, cb.Counts())
	assert.Equal(t, 2, len(cb.counts.buckets))

	err = tscb.UpdatePolicy(policy, PreserveClosedCounts)
	assert.Nil(t, err)
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		ConsecutiveSuccesses: 1,
	}, tscb.Counts())

	assert.Nil(t, succeed(cb))
	assert.Equal(t, Counts{
		Requests:             2,
		TotalSuccesses:       2,
		ConsecutiveSuccesses: 2,
	}, cb.Counts())

	assert.Nil(t, succeed(tscb.cb))
	assert.Equal(t, Counts{
		Requests:             2,
		TotalSuccesses:       2,
		ConsecutiveSuccesses: 2,
	}, tscb.Counts())
}

func TestUpdatePolicyRemovesInFlightRequestBeforePreservingRollingCounts(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	})
	tscb := NewTwoStepCircuitBreaker[bool](Settings{
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	})

	assert.Nil(t, succeed(cb))
	done, err := tscb.Allow()
	assert.Nil(t, err)

	policy := Policy{
		MaxRequests:  3,
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      2 * time.Second,
	}
	assert.Nil(t, cb.UpdatePolicy(policy, PreserveClosedCounts))
	assert.Nil(t, tscb.UpdatePolicy(policy, PreserveClosedCounts))

	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		ConsecutiveSuccesses: 1,
	}, cb.Counts())
	assert.Equal(t, Counts{}, tscb.Counts())

	done(nil)
	assert.Equal(t, Counts{}, tscb.Counts())
	assert.Nil(t, succeed(tscb.cb))
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		ConsecutiveSuccesses: 1,
	}, tscb.Counts())
}

func TestUpdatePolicyDoesNotSpliceOldBucketsAcrossLayouts(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	})

	assert.Nil(t, succeed(cb))
	assert.Nil(t, fail(cb))
	oldCounts := cb.Counts()

	err := cb.UpdatePolicy(Policy{
		Interval:     3 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	}, PreserveClosedCounts)
	assert.Nil(t, err)

	assert.Equal(t, 3, len(cb.counts.buckets))
	assert.Equal(t, oldCounts, cb.Counts())
	for _, bucket := range cb.counts.buckets {
		assert.Equal(t, Counts{}, bucket)
	}
	assert.True(t, cb.counts.hasLegacy)
	assert.Equal(t, oldCounts, cb.counts.legacy)

	assert.Nil(t, succeed(cb))
	assert.Equal(t, Counts{
		Requests:             3,
		TotalSuccesses:       2,
		TotalFailures:        1,
		ConsecutiveSuccesses: 1,
	}, cb.Counts())

	pseudoSleep(cb, 3*time.Second)
	assert.Nil(t, succeed(cb))
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		ConsecutiveSuccesses: 1,
	}, cb.Counts())
	assert.False(t, cb.counts.hasLegacy)

	err = cb.UpdatePolicy(Policy{
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	}, ResetClosedCounts)
	assert.Nil(t, err)
	assert.Equal(t, Counts{}, cb.Counts())
	assert.False(t, cb.counts.hasLegacy)
}

func TestUpdatePolicyLegacyCountsExpireAfterNewWindow(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{
		Interval:     2 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	})

	for i := 0; i < 3; i++ {
		assert.Nil(t, succeed(cb))
	}

	err := cb.UpdatePolicy(Policy{
		Interval:     3 * time.Second,
		BucketPeriod: time.Second,
		Timeout:      time.Second,
	}, PreserveClosedCounts)
	assert.Nil(t, err)

	pseudoSleep(cb, 2*time.Second)
	assert.Nil(t, succeed(cb))
	assert.Equal(t, Counts{
		Requests:             4,
		TotalSuccesses:       4,
		ConsecutiveSuccesses: 4,
	}, cb.Counts())

	pseudoSleep(cb, time.Second)
	assert.Nil(t, succeed(cb))
	assert.Equal(t, Counts{
		Requests:             2,
		TotalSuccesses:       2,
		ConsecutiveSuccesses: 2,
	}, cb.Counts())
	assert.False(t, cb.counts.hasLegacy)
}

func TestUpdatePolicyGenerationIgnoresOldRequests(t *testing.T) {
	tscb := NewTwoStepCircuitBreaker[bool](Settings{Interval: 2 * time.Second})
	done, err := tscb.Allow()
	assert.Nil(t, err)
	assert.Equal(t, Counts{Requests: 1}, tscb.Counts())

	err = tscb.UpdatePolicy(Policy{
		Interval: 3 * time.Second,
		Timeout:  time.Second,
	}, ResetClosedCounts)
	assert.Nil(t, err)
	assert.Equal(t, Counts{}, tscb.Counts())

	done(nil)
	assert.Equal(t, Counts{}, tscb.Counts())

	assert.Nil(t, succeed(tscb.cb))
	assert.Equal(t, Counts{
		Requests:             1,
		TotalSuccesses:       1,
		ConsecutiveSuccesses: 1,
	}, tscb.Counts())
}

func TestUpdatePolicyResetsNonClosedCounts(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{Timeout: time.Second})
	for i := 0; i < 6; i++ {
		assert.Nil(t, fail(cb))
	}
	assert.Equal(t, StateOpen, cb.State())

	err := cb.UpdatePolicy(Policy{Timeout: 2 * time.Second}, PreserveClosedCounts)
	assert.Nil(t, err)
	assert.Equal(t, StateOpen, cb.State())
	assert.Equal(t, Counts{}, cb.Counts())

	pseudoSleep(cb, 2*time.Second+time.Nanosecond)
	assert.Equal(t, StateHalfOpen, cb.State())
	tscb := &TwoStepCircuitBreaker[bool]{cb: cb}
	done, err := tscb.Allow()
	assert.Nil(t, err)

	err = cb.UpdatePolicy(Policy{Timeout: 3 * time.Second}, PreserveClosedCounts)
	assert.Nil(t, err)
	assert.Equal(t, StateHalfOpen, cb.State())
	assert.Equal(t, Counts{}, cb.Counts())
	done(nil)
	assert.Equal(t, Counts{}, cb.Counts())
}

func TestUpdatePolicyConcurrentWithExecutesAndCounts(t *testing.T) {
	cb := NewCircuitBreaker[bool](Settings{
		MaxRequests:  4,
		Interval:     50 * time.Millisecond,
		BucketPeriod: 10 * time.Millisecond,
		Timeout:      time.Second,
	})

	policies := []Policy{
		{MaxRequests: 4, Interval: 50 * time.Millisecond, BucketPeriod: 10 * time.Millisecond, Timeout: time.Second},
		{MaxRequests: 8, Interval: 80 * time.Millisecond, BucketPeriod: 20 * time.Millisecond, Timeout: 2 * time.Second},
	}
	invalidPolicy := Policy{MaxRequests: 4, Interval: time.Second, BucketPeriod: 2 * time.Second, Timeout: -time.Second}

	errCh := make(chan error, 1024)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, err := cb.Execute(func() (bool, error) { return true, nil })
				if err != nil {
					errCh <- err
				}
			}
		}()
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = cb.Counts()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			policy := policies[i%2]
			closedCounts := ResetClosedCounts
			if i%4 == 0 {
				closedCounts = PreserveClosedCounts
			}
			if err := cb.UpdatePolicy(policy, closedCounts); err != nil {
				errCh <- err
			}
			if err := cb.UpdatePolicy(invalidPolicy, ResetClosedCounts); err != nil &&
				!errors.Is(err, ErrInvalidPolicy) {
				errCh <- err
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		assert.NoError(t, err)
	}
}
