package gobreaker

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeClock struct {
	mutex sync.Mutex
	now   time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (fc *fakeClock) Now() time.Time {
	fc.mutex.Lock()
	defer fc.mutex.Unlock()
	return fc.now
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mutex.Lock()
	defer fc.mutex.Unlock()
	fc.now = fc.now.Add(d)
}

func newLeasedBreaker(clock *fakeClock, st Settings) *TwoStepCircuitBreaker[bool] {
	tscb := NewTwoStepCircuitBreaker[bool](st)
	tscb.cb.clock = clock.Now
	return tscb
}

func openLeasedBreaker(t *testing.T, tscb *TwoStepCircuitBreaker[bool], clock *fakeClock) {
	t.Helper()
	for i := 0; i < 6 && tscb.State() != StateOpen; i++ {
		assert.Nil(t, fail2Step(tscb))
	}
	assert.Equal(t, StateOpen, tscb.State())
	clock.Advance(tscb.cb.timeout + time.Second)
	assert.Equal(t, StateHalfOpen, tscb.State())
}

func TestLeaseExpirationAbandonsByDefault(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{Name: "lease", MaxRequests: 1})

	openLeasedBreaker(t, tscb, clock)

	lease, err := tscb.AllowWithLease(10 * time.Second)
	assert.Nil(t, err)

	// the probe slot is occupied while the lease is outstanding
	_, err = tscb.Allow()
	assert.Equal(t, ErrTooManyRequests, err)

	// let the lease expire; the next access settles it as an abandonment
	clock.Advance(11 * time.Second)
	assert.Equal(t, StateHalfOpen, tscb.State())
	assert.Equal(t, Counts{Requests: 1, TotalExclusions: 1}, tscb.Counts())

	// the probe slot has been released
	done, err := tscb.Allow()
	assert.Nil(t, err)

	// late callbacks on the expired lease are ignored
	lease.Done(nil)
	lease.Abandon()
	assert.Equal(t, Counts{Requests: 2, TotalExclusions: 1}, tscb.Counts())
	assert.Equal(t, StateHalfOpen, tscb.State())

	// the plain Allow path is unaffected
	done(nil)
	assert.Equal(t, StateClosed, tscb.State())
}

func TestLeaseExpirationCountsAsFailure(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{
		Name:                     "lease-failure",
		MaxRequests:              1,
		LeaseExpirationIsFailure: true,
	})

	// in the closed state, an expired lease is recorded as a failure
	lease, err := tscb.AllowWithLease(10 * time.Second)
	assert.Nil(t, err)
	clock.Advance(11 * time.Second)
	assert.Equal(t, Counts{Requests: 1, TotalFailures: 1, ConsecutiveFailures: 1}, tscb.Counts())
	lease.Done(nil) // too late: already settled by the expiration
	assert.Equal(t, Counts{Requests: 1, TotalFailures: 1, ConsecutiveFailures: 1}, tscb.Counts())

	openLeasedBreaker(t, tscb, clock)

	// in the half-open state, an expired lease fails the probe and reopens the breaker
	lease, err = tscb.AllowWithLease(10 * time.Second)
	assert.Nil(t, err)
	clock.Advance(11 * time.Second)
	assert.Equal(t, StateOpen, tscb.State())
	assert.Equal(t, Counts{}, tscb.Counts())

	// the expired lease cannot affect the new generation
	lease.Done(nil)
	assert.Equal(t, StateOpen, tscb.State())
	assert.Equal(t, Counts{}, tscb.Counts())
}

func TestLeaseSettlesOnlyOnce(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{Name: "lease-once"})

	lease, err := tscb.AllowWithLease(time.Minute)
	assert.Nil(t, err)

	lease.Done(nil)
	lease.Done(errFailed)
	lease.Abandon()

	assert.Equal(t, Counts{Requests: 1, TotalSuccesses: 1, ConsecutiveSuccesses: 1}, tscb.Counts())
	assert.Equal(t, StateClosed, tscb.State())
}

func TestLeaseAbandonReleasesSlot(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{Name: "lease-abandon", MaxRequests: 1})

	openLeasedBreaker(t, tscb, clock)

	lease, err := tscb.AllowWithLease(time.Minute)
	assert.Nil(t, err)
	_, err = tscb.Allow()
	assert.Equal(t, ErrTooManyRequests, err)

	lease.Abandon()
	assert.Equal(t, Counts{Requests: 1, TotalExclusions: 1}, tscb.Counts())

	// abandoning released the probe slot without closing the breaker
	assert.Equal(t, StateHalfOpen, tscb.State())
	done, err := tscb.Allow()
	assert.Nil(t, err)
	done(nil)
	assert.Equal(t, StateClosed, tscb.State())
}

func TestStaleLeaseCannotCloseNewGeneration(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{Name: "lease-stale", MaxRequests: 2})

	openLeasedBreaker(t, tscb, clock)

	lease, err := tscb.AllowWithLease(time.Minute)
	assert.Nil(t, err)

	// another probe fails, moving the breaker to a new generation
	assert.Nil(t, fail2Step(tscb))
	assert.Equal(t, StateOpen, tscb.State())

	clock.Advance(tscb.cb.timeout + time.Second)
	assert.Equal(t, StateHalfOpen, tscb.State())

	// the stale lease must not close the new generation of the breaker
	lease.Done(nil)
	assert.Equal(t, StateHalfOpen, tscb.State())
	assert.Equal(t, Counts{}, tscb.Counts())

	// the new generation still needs its own successes to close
	assert.Nil(t, succeed2Step(tscb))
	assert.Equal(t, StateHalfOpen, tscb.State())
	assert.Nil(t, succeed2Step(tscb))
	assert.Equal(t, StateClosed, tscb.State())
}

func TestLeaseConcurrentSettlement(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{Name: "lease-concurrent"})

	lease, err := tscb.AllowWithLease(10 * time.Second)
	assert.Nil(t, err)
	clock.Advance(11 * time.Second)

	// concurrent completions, abandonments and expiration sweeps:
	// the lease is settled exactly once
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			lease.Done(nil)
		}()
		go func() {
			defer wg.Done()
			lease.Abandon()
		}()
		go func() {
			defer wg.Done()
			tscb.State()
		}()
	}
	wg.Wait()

	counts := tscb.Counts()
	assert.Equal(t, uint32(1), counts.Requests)
	settled := counts.TotalSuccesses + counts.TotalFailures + counts.TotalExclusions
	assert.Equal(t, uint32(1), settled)
}

func TestLeaseConcurrentProbesCloseBreaker(t *testing.T) {
	clock := newFakeClock()
	tscb := newLeasedBreaker(clock, Settings{Name: "lease-probes", MaxRequests: 2})

	openLeasedBreaker(t, tscb, clock)

	lease1, err := tscb.AllowWithLease(time.Minute)
	assert.Nil(t, err)
	lease2, err := tscb.AllowWithLease(time.Minute)
	assert.Nil(t, err)
	_, err = tscb.AllowWithLease(time.Minute)
	assert.Equal(t, ErrTooManyRequests, err)

	var wg sync.WaitGroup
	for _, lease := range []*Lease[bool]{lease1, lease2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease.Done(nil)
		}()
	}
	wg.Wait()

	assert.Equal(t, StateClosed, tscb.State())
	assert.Equal(t, Counts{}, tscb.Counts())
}
