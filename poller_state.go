// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"context"
	"sync"
	"time"
)

// epState is the per-normalized-endpoint state guarded by its own mutex.
//
// The permit channel has capacity one and always starts with one token in
// it. A caller drains the token to claim the endpoint and must return it
// when finished, serializing all concurrent queries to the endpoint.
type epState struct {
	mu sync.Mutex

	// permit is the single polling permit for the endpoint.
	permit chan struct{}

	// denied is true after a deny-class kiss of death, until reset.
	denied bool

	// denyCode records the kiss code that caused the lockout.
	denyCode string

	// retryAfter is the end of the current RATE cooldown.
	retryAfter time.Time

	// wake is closed and replaced whenever the state is reset or enters
	// deny, interrupting cooldown waits so they can re-evaluate.
	wake chan struct{}
}

func newEPState() *epState {
	permit := make(chan struct{}, 1)
	permit <- struct{}{}
	return &epState{
		permit: permit,
		wake:   make(chan struct{}),
	}
}

// acquire claims the endpoint's polling permit, then waits until any
// pending cooldown has elapsed. A deny lockout is reported immediately.
// The ctx may cancel the wait; in that case the caller must release the
// permit it claimed.
func (s *epState) acquire(ctx context.Context, clock Clock) error {
	// Atomically claim the single polling permit before touching state.
	select {
	case <-s.permit:
	case <-ctx.Done():
		return ctx.Err()
	}

	for {
		wait, wake := s.waitPlan(clock.Now())
		switch {
		case wait < 0:
			// The endpoint is denied until an explicit reset.
			return ErrEndpointDenied
		case wait == 0:
			return nil
		}

		timer := clock.NewTimer(wait)
		select {
		case <-timer.C():
			// Re-check state after the timer fires.
		case <-wake:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// waitPlan reports how much longer the caller must wait under the state as
// observed at time now, along with the current wake channel. A negative
// duration means the endpoint is denied. A zero duration means proceed.
func (s *epState) waitPlan(now time.Time) (time.Duration, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denied {
		return -1, s.wake
	}
	if !s.retryAfter.IsZero() && now.Before(s.retryAfter) {
		return s.retryAfter.Sub(now), s.wake
	}
	return 0, s.wake
}

// release returns the polling permit claimed by acquire.
func (s *epState) release() {
	select {
	case s.permit <- struct{}{}:
	default:
	}
}

// enterCooldown arms a RATE cooldown ending at retryAfter and clears any
// deny lockout.
func (s *epState) enterCooldown(retryAfter time.Time) {
	s.mu.Lock()
	s.denied = false
	s.denyCode = ""
	s.retryAfter = retryAfter
	s.mu.Unlock()
}

// deny locks the endpoint out following a deny-class kiss of death and
// interrupts any cooldown wait.
func (s *epState) deny(code string) {
	s.mu.Lock()
	s.denied = true
	s.denyCode = code
	s.retryAfter = time.Time{}
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// clear removes a pending cooldown following a successful query. It does
// not clear a deny lockout, which requires an explicit reset.
func (s *epState) clear() {
	s.mu.Lock()
	if !s.denied {
		s.retryAfter = time.Time{}
	}
	s.mu.Unlock()
}

// reset clears all state and interrupts any wait so the endpoint can be
// queried immediately.
func (s *epState) reset() {
	s.mu.Lock()
	s.denied = false
	s.denyCode = ""
	s.retryAfter = time.Time{}
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// snapshot returns a point-in-time copy of the state, suppressing an
// already-elapsed cooldown.
func (s *epState) snapshot(now time.Time) EndpointState {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := EndpointState{Denied: s.denied, DenyCode: s.denyCode}
	if s.denied {
		snap.RetryAfter = time.Time{}
	} else if !s.retryAfter.IsZero() && now.Before(s.retryAfter) {
		snap.RetryAfter = s.retryAfter
	}
	return snap
}
