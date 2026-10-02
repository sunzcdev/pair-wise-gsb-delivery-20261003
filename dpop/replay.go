package dpop

import (
	"context"
	"sync"
	"time"
)

// ReplayStore atomically records single-use DPoP proof identifiers.
//
// CheckAndStore must be atomic: it reports whether jti was already present
// and, when it was not, records it with the given time-to-live in a single
// operation. Concurrent calls for the same jti (including from different
// service instances) must result in exactly one caller observing seen=false.
//
// A correct shared implementation is typically backed by a Redis command
// such as:
//
//	SET key <nonce-ish-value> NX PX ttl_ms
//
// or an equivalent compare-and-set on a relational/NoSQL store. A naive
// "check then insert" sequence is NOT sufficient because it admits a race
// window in which two replicas both accept the same proof.
//
// Keys may be namespaced (e.g. "dpop:jti:<jti>"); the store owns expiry and
// garbage collection.
type ReplayStore interface {
	// CheckAndStore atomically records jti for ttl. It returns seen=true if
	// the jti was already recorded and has not expired. A non-nil error means
	// no decision could be made; the verifier then rejects the request.
	CheckAndStore(ctx context.Context, jti string, ttl time.Duration) (seen bool, err error)
}

// MemoryReplayStore is a single-process [ReplayStore] guarded by a mutex. It
// is safe for concurrent use but does NOT share state across service
// instances; use a shared backend in multi-instance deployments.
type MemoryReplayStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
	now     func() time.Time
}

// NewMemoryReplayStore returns an empty in-process replay store.
func NewMemoryReplayStore() *MemoryReplayStore {
	return &MemoryReplayStore{
		entries: map[string]time.Time{},
		now:     time.Now,
	}
}

// CheckAndStore implements [ReplayStore].
func (s *MemoryReplayStore) CheckAndStore(_ context.Context, jti string, ttl time.Duration) (bool, error) {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Opportunistically sweep expired entries so the map cannot grow without
	// bound. Sweeping while holding the lock on every request is O(n); the
	// map only ever holds jtis within the replay window (seconds to minutes),
	// which keeps n small.
	for id, expiresAt := range s.entries {
		if !expiresAt.After(now) {
			delete(s.entries, id)
		}
	}

	if expiresAt, ok := s.entries[jti]; ok && expiresAt.After(now) {
		return true, nil
	}
	s.entries[jti] = now.Add(ttl)
	return false, nil
}

// Len reports the number of currently retained (unexpired) jtis. It is
// intended for tests and operational inspection.
func (s *MemoryReplayStore) Len() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, expiresAt := range s.entries {
		if expiresAt.After(now) {
			n++
		}
	}
	return n
}
