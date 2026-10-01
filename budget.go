// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import "sync"

// recvBudget bounds the total receive memory a session is willing to commit.
// A committed byte is either data already received and buffered by a stream,
// or receive credit advertised to the peer (via the initial stream window or a
// window update) that has not yet been consumed by application reads. Bounding
// the sum of the two prevents a peer that keeps opening well-behaved streams
// from forcing unbounded receive buffering on the connection.
//
// The zero value is not usable; use newRecvBudget. A limit of zero disables
// the session level limit entirely.
type recvBudget struct {
	limit uint64

	mu      sync.Mutex
	used    uint64
	waiters int
	wake    chan struct{}
	closed  bool
}

// newRecvBudget returns a receive budget with the given limit. A limit of zero
// disables the budget and makes every reservation succeed.
func newRecvBudget(limit uint32) *recvBudget {
	return &recvBudget{
		limit: uint64(limit),
		wake:  make(chan struct{}),
	}
}

// disabled reports whether a session level limit is configured.
func (b *recvBudget) disabled() bool {
	return b == nil || b.limit == 0
}

// tryAcquire attempts to reserve up to want bytes, blocking until at least one
// byte can be committed. It returns the granted amount, which may be smaller
// than want; a zero return means the budget has been shut down before credit
// became available. A disabled budget grants the full request.
func (b *recvBudget) tryAcquire(want uint32, shutdownCh <-chan struct{}) uint32 {
	if want == 0 {
		return 0
	}
	if b.disabled() {
		return want
	}

	b.mu.Lock()
	b.waiters++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.waiters--
		b.mu.Unlock()
	}()

	for {
		b.mu.Lock()
		avail := b.limit - b.used
		if avail > 0 {
			grant := uint64(want)
			if grant > avail {
				grant = avail
			}
			b.used += grant
			b.mu.Unlock()
			return uint32(grant)
		}
		wake := b.wake
		b.mu.Unlock()

		select {
		case <-wake:
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return 0
			}
		case <-shutdownCh:
			return 0
		}
	}
}

// tryAcquireNonBlocking attempts to reserve up to want bytes without blocking.
// It returns the granted amount, which may be zero when no credit is currently
// available. A disabled budget grants the full request.
func (b *recvBudget) tryAcquireNonBlocking(want uint32) uint32 {
	if want == 0 {
		return 0
	}
	if b.disabled() {
		return want
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	avail := b.limit - b.used
	if avail == 0 {
		return 0
	}
	grant := uint64(want)
	if grant > avail {
		grant = avail
	}
	b.used += grant
	return uint32(grant)
}

// release returns n previously committed bytes to the budget and wakes blocked
// openers. Releasing more than the committed total is a bug and clamps to zero.
func (b *recvBudget) release(n uint32) {
	if n == 0 || b.disabled() {
		return
	}

	b.mu.Lock()
	if uint64(n) > b.used {
		b.used = 0
	} else {
		b.used -= uint64(n)
	}
	if b.waiters > 0 {
		b.broadcastLocked()
	}
	b.mu.Unlock()
}

// broadcastLocked closes the current wake channel and installs a fresh one so
// waiters joining after the broadcast block instead of observing an
// already-closed channel. After shutdown no replacement channel is installed.
// The caller must hold b.mu.
func (b *recvBudget) broadcastLocked() {
	wake := b.wake
	if !b.closed {
		b.wake = make(chan struct{})
	}
	close(wake)
}

// shutdown unblocks every waiter so blocked stream opens can fail fast during
// session teardown.
func (b *recvBudget) shutdown() {
	if b.disabled() {
		return
	}
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		b.broadcastLocked()
	}
	b.mu.Unlock()
}
